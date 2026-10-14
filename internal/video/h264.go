package video

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
)

var ErrFinished = errors.New("video writer is finished")

// H264Stats counts NAL units, not pictures: a picture can contain many slices.
// FirstSPS and LastSPS describe the input before editing.
type H264Stats struct {
	BytesRead      int64
	BytesWritten   int64
	NALUnits       int64
	VCLNALUnits    int64
	SPS            int64
	ChangedSPS     int64
	AddedTimingSPS int64
	FirstSPS       H264SPS
	LastSPS        H264SPS
	Target         H264Clock
}

// H264Writer rewrites SPS timing in an Annex B elementary stream. It preserves
// all other NAL bytes, start codes, AUDs and trailing zero bytes. Only SPS NALs
// are buffered; other NALs can be arbitrarily large. Finish must succeed before
// the caller publishes the output. It does not close the underlying writer.
//
// Subset SPS and SVC/MVC/3D extensions are rejected, because their additional
// timing syntax needs a separate editor. A stream must supply an SPS before its
// first coded slice; out-of-band parameter sets should be emitted by its demuxer.
type H264Writer struct {
	destination *bufio.Writer
	clock       H264Clock
	stats       H264Stats
	zeroRun     uint64
	started     bool
	haveHeader  bool
	nalType     byte
	nalBytes    uint64
	sps         []byte
	zeroBytes   [256]byte
	finished    bool
	err         error
}

type h264Output struct {
	destination io.Writer
	stats       *H264Stats
}

func (w h264Output) Write(p []byte) (int, error) {
	n, err := w.destination.Write(p)
	if n < 0 || n > len(p) {
		return 0, fmt.Errorf("invalid video writer byte count %d", n)
	}
	w.stats.BytesWritten += int64(n)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	return n, err
}

func NewH264Writer(destination io.Writer, clock H264Clock) (*H264Writer, error) {
	if destination == nil {
		return nil, fmt.Errorf("nil H.264 destination")
	}
	if !clock.valid() {
		return nil, fmt.Errorf("H.264 timing clocks must be nonzero")
	}
	w := &H264Writer{clock: clock}
	w.stats.Target = clock
	w.destination = bufio.NewWriterSize(h264Output{destination, &w.stats}, 32<<10)
	return w, nil
}

func (w *H264Writer) Stats() H264Stats { return w.stats }

func (w *H264Writer) Write(p []byte) (n int, err error) {
	if w.err != nil {
		return 0, w.err
	}
	if w.finished {
		return 0, ErrFinished
	}
	defer func() {
		w.stats.BytesRead += int64(n)
		if err != nil {
			w.err = err
		}
	}()
	for n < len(p) {
		if p[n] == 0 {
			start := n
			for n < len(p) && p[n] == 0 {
				n++
			}
			w.zeroRun += uint64(n - start)
			continue
		}
		if w.zeroRun >= 2 && p[n] == 1 {
			if w.started {
				if err = w.finishNAL(); err != nil {
					return n, err
				}
			}
			if err = w.writeZeros(w.zeroRun); err != nil {
				return n, err
			}
			if err = w.destination.WriteByte(1); err != nil {
				return n, err
			}
			w.zeroRun, w.nalBytes = 0, 0
			w.started, w.haveHeader = true, false
			n++
			continue
		}
		if !w.started {
			return n, fmt.Errorf("expected Annex B H.264 start code at byte %d", w.stats.BytesRead+int64(n))
		}
		if w.zeroRun != 0 {
			if !w.haveHeader {
				return n, fmt.Errorf("invalid zero H.264 NAL header")
			}
			if w.nalType == 7 {
				if w.zeroRun > uint64(MaxH264SPSSize-len(w.sps)) {
					return n, fmt.Errorf("H.264 SPS exceeds %d bytes", MaxH264SPSSize)
				}
				w.sps = append(w.sps, make([]byte, int(w.zeroRun))...)
			} else if err = w.writeZeros(w.zeroRun); err != nil {
				return n, err
			}
			w.nalBytes += w.zeroRun
			w.zeroRun = 0
		}
		if !w.haveHeader {
			if err = w.beginNAL(p[n]); err != nil {
				return n, err
			}
			n++
			continue
		}
		length := bytes.IndexByte(p[n:], 0)
		if length < 0 {
			length = len(p) - n
		}
		part := p[n : n+length]
		if w.nalType == 7 {
			if len(part) > MaxH264SPSSize-len(w.sps) {
				return n, fmt.Errorf("H.264 SPS exceeds %d bytes", MaxH264SPSSize)
			}
			w.sps = append(w.sps, part...)
		} else if _, err = w.destination.Write(part); err != nil {
			return n, err
		}
		w.nalBytes += uint64(length)
		n += length
	}
	return n, nil
}

func (w *H264Writer) beginNAL(header byte) error {
	kind := header & 31
	if header&0x80 != 0 || kind == 0 || kind >= 22 || kind >= 16 && kind <= 18 {
		return fmt.Errorf("invalid H.264 NAL header 0x%02x", header)
	}
	switch kind {
	case 14, 15, 20, 21:
		return fmt.Errorf("H.264 NAL type %d needs unsupported SVC/MVC/3D timing edits", kind)
	case 1, 2, 3, 4, 5, 19:
		if w.stats.SPS == 0 {
			return fmt.Errorf("H.264 coded slice precedes the first SPS; supply Annex B parameter sets")
		}
	}
	w.nalType, w.nalBytes, w.haveHeader = kind, 1, true
	if kind == 7 {
		w.sps = append(w.sps[:0], header)
		return nil
	}
	return w.destination.WriteByte(header)
}

func (w *H264Writer) finishNAL() error {
	if !w.haveHeader || w.nalBytes == 0 {
		return fmt.Errorf("empty H.264 NAL after start code")
	}
	// End-of-sequence and end-of-stream units have no RBSP payload.
	if w.nalBytes == 1 && w.nalType != 10 && w.nalType != 11 {
		return fmt.Errorf("truncated H.264 NAL type %d", w.nalType)
	}
	if w.nalType == 7 {
		edited, info, err := RewriteH264SPS(w.sps, w.clock)
		if err != nil {
			return err
		}
		if _, err = w.destination.Write(edited); err != nil {
			return err
		}
		if w.stats.SPS == 0 {
			w.stats.FirstSPS = info
		}
		w.stats.LastSPS = info
		w.stats.SPS++
		if !bytes.Equal(edited, w.sps) {
			w.stats.ChangedSPS++
		}
		if !info.TimingPresent {
			w.stats.AddedTimingSPS++
		}
	}
	w.stats.NALUnits++
	if w.nalType >= 1 && w.nalType <= 5 || w.nalType == 19 {
		w.stats.VCLNALUnits++
	}
	return nil
}

func (w *H264Writer) writeZeros(n uint64) error {
	for n != 0 {
		size := uint64(len(w.zeroBytes))
		if n < size {
			size = n
		}
		if _, err := w.destination.Write(w.zeroBytes[:int(size)]); err != nil {
			return err
		}
		n -= size
	}
	return nil
}

// Finish validates and writes the final SPS/NAL and flushes buffered output.
// Repeated Finish calls return the same result.
func (w *H264Writer) Finish() error {
	if w.err != nil {
		return w.err
	}
	if w.finished {
		return nil
	}
	w.finished = true
	if !w.started {
		w.err = fmt.Errorf("no Annex B H.264 NAL units")
	} else {
		w.err = w.finishNAL()
	}
	if w.err == nil && w.stats.SPS == 0 {
		w.err = fmt.Errorf("no H.264 SPS; requested timing was not applied")
	}
	if w.err == nil {
		w.err = w.writeZeros(w.zeroRun)
	}
	if w.err == nil {
		w.err = w.destination.Flush()
	}
	return w.err
}

// RewriteH264 is the reader-to-writer convenience form of H264Writer. Reader
// and writer errors are returned; neither endpoint is closed.
func RewriteH264(source io.Reader, destination io.Writer, clock H264Clock) (H264Stats, error) {
	w, err := NewH264Writer(destination, clock)
	if err != nil {
		return H264Stats{}, err
	}
	if source == nil {
		return w.Stats(), fmt.Errorf("nil H.264 source")
	}
	if _, err = io.Copy(w, source); err != nil {
		return w.Stats(), err
	}
	err = w.Finish()
	return w.Stats(), err
}
