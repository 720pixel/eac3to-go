package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"eac3to/internal/transport"
	"eac3to/internal/truehd"
)

func nativeTransportSource(s *Source) bool {
	return s.Playlist != nil || len(s.Paths) == 1 && strings.Contains(s.Probe.Format.Name, "mpegts")
}

// Source is immutable during one command. Preparing TrueHD's delay filenames
// and extracting its payload reuse the same physical scans and startup probe.
func sourceNativeScans(ctx context.Context, s *Source, o Options) ([]clipScan, error) {
	if s.nativeScans != nil {
		return s.nativeScans, nil
	}
	var scans []clipScan
	var err error
	pids := map[uint16]bool{}
	for _, st := range s.Probe.Streams {
		if st.Type == "video" || st.Type == "audio" || st.Type == "subtitle" {
			if pid, err := strconv.ParseUint(st.ID, 0, 13); err == nil && pid < 0x1fff {
				pids[uint16(pid)] = true
			}
		}
	}
	for _, track := range s.Tracks {
		if track.Stream != nil {
			if pid, err := strconv.ParseUint(track.Stream.ID, 0, 13); err == nil && pid < 0x1fff {
				pids[uint16(pid)] = true
			}
		}
	}
	if s.Playlist != nil {
		for _, item := range s.Playlist.Items {
			for pid := range item.ListedPIDs() {
				pids[pid] = true
			}
		}
		scans, err = scanPlaylistPIDs(ctx, s.Playlist, o, pids)
	} else {
		if !nativeTransportSource(s) {
			return nil, fmt.Errorf("source is not a supported native transport input")
		}
		video := map[uint16]bool{}
		for _, st := range s.Probe.Streams {
			if st.Type == "video" {
				if pid, e := strconv.ParseUint(st.ID, 0, 13); e == nil && pid < 0x1fff {
					video[uint16(pid)] = true
				}
			}
		}
		scan, e := transport.ScanFile(ctx, s.Paths[0], transport.ScanConfig{VideoPIDs: video, PIDs: pids})
		if e != nil {
			return nil, e
		}
		if scan.TrailingBytes != 0 {
			return nil, fmt.Errorf("input has %d bytes after its last complete transport packet", scan.TrailingBytes)
		}
		scans = []clipScan{{Item: 1, Path: s.Paths[0], Clip: filepath.Base(s.Paths[0]), Scan: scan}}
	}
	if err != nil {
		return nil, err
	}
	s.nativeScans = scans
	return scans, nil
}

func sourceNativeTiming(ctx context.Context, s *Source, scans []clipScan) (initialTransportTiming, error) {
	if s.nativeTiming != nil {
		return *s.nativeTiming, nil
	}
	timing, err := transportInitialTiming(ctx, s, scans)
	if err != nil {
		return timing, err
	}
	s.nativeTiming = &timing
	return timing, nil
}

// Resolve the final names before validating destinations or creating staging
// files. The dry-run entry point calls this same preparation function.
func prepareTrueHDOutputs(ctx context.Context, s *Source, outputs []Output, o Options) ([]Output, error) {
	prepared := append([]Output(nil), outputs...)
	var timing initialTransportTiming
	haveTiming := false
	for i := range prepared {
		out := &prepared[i]
		if !out.Copy || out.Codec != "truehd" || out.Format != "truehd" || out.TrueHDPrepared {
			continue
		}
		manual := out.Request.Options.DelayMS
		if math.IsNaN(manual) || math.IsInf(manual, 0) || manual != math.Trunc(manual) || manual >= float64(math.MaxInt64) || manual < float64(math.MinInt64) {
			return nil, fmt.Errorf("TrueHD delay needs a representable whole number of milliseconds")
		}
		var source int64
		if offset, ok := matroskaAudioOffset(s, out.Track.Stream); ok {
			source = offset
		}
		if nativeTransportSource(s) {
			if !haveTiming {
				scans, err := sourceNativeScans(ctx, s, o)
				if err != nil {
					return nil, err
				}
				timing, err = sourceNativeTiming(ctx, s, scans)
				if err != nil {
					return nil, err
				}
				haveTiming = true
			}
			pid, err := strconv.ParseUint(out.Track.Stream.ID, 0, 13)
			if err != nil || pid >= 0x1fff {
				return nil, fmt.Errorf("TrueHD track has no valid transport PID")
			}
			source = timing.Delays[uint16(pid)]
		}
		delay := int64(manual)
		if delay > 0 && source > math.MaxInt64-delay || delay < 0 && source < math.MinInt64-delay {
			return nil, fmt.Errorf("TrueHD source and manual delay overflow")
		}
		delay += source
		path := out.Path
		// .truehd is a CLI alias of the elementary .thd format.
		extension := filepath.Ext(path)
		canonical := strings.TrimSuffix(path, extension) + ".thd"
		path, err := truehd.DelayPath(canonical, delay)
		if err != nil {
			return nil, err
		}
		out.Path = strings.TrimSuffix(path, ".thd") + extension
		out.TrueHDDelayMS, out.TrueHDPrepared = delay, true
	}
	if err := validateOutputPaths(s, prepared, o); err != nil {
		return nil, err
	}
	return prepared, nil
}

func (t *nativeTrack) setupTrueHD(experimental bool) error {
	var err error
	t.truehdPost, err = truehd.NewPostprocessor(truehd.PostprocessConfig{WindowsSeamless: experimental, OnFrame: t.writeTrueHD})
	if err != nil {
		return err
	}
	t.truehdParser, err = truehd.NewParser(truehd.ParserConfig{OnFrame: t.truehdPost.Accept})
	return err
}

func (t *nativeTrack) writeTrueHD(frame truehd.Frame) error {
	if err := t.ctx.Err(); err != nil {
		return err
	}
	var normalized []byte
	for _, out := range t.outputs {
		data := frame.Bytes
		if out.output.RemoveDialnorm && frame.MajorSync {
			if normalized == nil {
				if cap(t.truehdBuffer) < len(data) {
					t.truehdBuffer = make([]byte, len(data))
				}
				t.truehdBuffer = t.truehdBuffer[:len(data)]
				copy(t.truehdBuffer, data)
				changedFrame := frame
				changedFrame.Bytes = t.truehdBuffer
				changed, err := truehd.RemoveDialogNormalization(changedFrame)
				if errors.Is(err, truehd.ErrAtmosDialogNormalization) {
					if !t.truehdAtmosNotice && t.log != nil {
						fmt.Fprintln(t.log, "Dialog normalization cannot be removed from Atmos audio.")
						t.truehdAtmosNotice = true
					}
				} else if err != nil {
					return err
				}
				if changed {
					t.truehdChanged++
				}
				normalized = t.truehdBuffer
			}
			data = normalized
		}
		if _, err := out.writer.Write(data); err != nil {
			return err
		}
	}
	t.frames++
	t.bytes += int64(len(frame.Bytes))
	return nil
}

func (t *nativeTrack) finishTrueHD() error {
	if err := t.truehdParser.Finish(); err != nil {
		return err
	}
	return t.truehdPost.Finish()
}

// Pure elementary TrueHD is read once and framed natively. Other containers
// still use their demuxer; their TrueHD pipe uses this same checked frame path.
func copyElementaryTrueHD(ctx context.Context, s *Source, outputs []Output, o Options, log io.Writer) ([]Output, error) {
	if s.Playlist != nil || len(s.Paths) != 1 || s.Probe.Format.Name != "truehd" || len(s.Probe.Streams) != 1 || s.Probe.Streams[0].Codec != "truehd" {
		return outputs, nil
	}
	var remaining []Output
	track := &nativeTrack{ctx: ctx, log: log, codec: "truehd"}
	var files []*os.File
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()
	for _, out := range outputs {
		if !nativeCopySupported(out) {
			remaining = append(remaining, out)
			continue
		}
		file, err := os.Create(out.Stage)
		if err != nil {
			return nil, err
		}
		files = append(files, file)
		track.outputs = append(track.outputs, &nativeOutput{output: out, file: file, writer: bufio.NewWriterSize(file, 1<<20)})
	}
	if len(track.outputs) == 0 {
		return outputs, nil
	}
	if err := track.setupTrueHD(false); err != nil {
		return nil, err
	}
	input, err := os.Open(s.Paths[0])
	if err != nil {
		return nil, err
	}
	defer input.Close()
	buffer := make([]byte, 256<<10)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, err := input.Read(buffer)
		if _, writeErr := track.truehdParser.Write(buffer[:n]); writeErr != nil {
			return nil, writeErr
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
	}
	if err := track.finishTrueHD(); err != nil {
		return nil, err
	}
	if track.bytes == 0 {
		return nil, fmt.Errorf("TrueHD input has no access units")
	}
	for _, out := range track.outputs {
		if err := out.writer.Flush(); err != nil {
			return nil, err
		}
		if err := out.file.Close(); err != nil {
			return nil, err
		}
	}
	return remaining, nil
}

func copyTrueHDPipe(ctx context.Context, r io.Reader, w *bufio.Writer, out Output, log io.Writer) error {
	track := &nativeTrack{ctx: ctx, log: log, codec: "truehd", outputs: []*nativeOutput{{output: out, writer: w}}}
	if err := track.setupTrueHD(false); err != nil {
		return err
	}
	if _, err := io.CopyBuffer(track.truehdParser, r, make([]byte, 256<<10)); err != nil {
		return err
	}
	if err := track.finishTrueHD(); err != nil {
		return err
	}
	if track.bytes == 0 {
		return fmt.Errorf("TrueHD output has no access units")
	}
	return nil
}
