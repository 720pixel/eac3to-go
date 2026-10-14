package bitstream

import (
	"encoding/binary"
	"fmt"
	"io"
)

const (
	pgsPalette      = 0x14
	pgsObject       = 0x15
	pgsPresentation = 0x16
	pgsWindow       = 0x17
	pgsEnd          = 0x80
	pgsHeaderSize   = 13
)

// PGSStats counts input segments and object references, including repeated
// references to the same bitmap. Empty presentations are clearing commands.
type PGSStats struct {
	Segments            int64
	Presentations       int64
	ForcedPresentations int64
	Objects             int64
	ForcedObjects       int64
	BytesRead           int64
	BytesWritten        int64
}

// FilterForcedPGS writes a SUP stream containing only forced presentation
// objects. It handles mixed and cropped objects without decoding their bitmaps.
// All palettes, object fragments, window definitions, timestamps, epoch changes
// and display-set ENDs are retained: dependencies can have been transmitted in
// earlier display sets. A presentation with no forced objects becomes an empty
// presentation so a previously visible forced subtitle is cleared on time.
// Consequently output size need not shrink much even if few subtitles are forced.
//
// The SUP packet layout and composition flag interpretation are documented by
// FFmpeg's libavformat/supdec.c and libavcodec/pgssubdec.c:
// https://github.com/FFmpeg/FFmpeg/blob/master/libavformat/supdec.c
// https://github.com/FFmpeg/FFmpeg/blob/master/libavcodec/pgssubdec.c
// Cropping is bit 7, forced display is bit 6; crop coordinates add eight bytes.
// This implementation is an independent parser, not a port of decoder code.
// It validates framing and object fragmentation but does not validate RLE pixels.
func FilterForcedPGS(r io.Reader, w io.Writer) (PGSStats, error) {
	var stats PGSStats
	packet := make([]byte, pgsHeaderSize+65535)
	fragments := make(map[uint16]pgsFragment)
	openDisplay := false
	for {
		offset := stats.BytesRead
		n, err := io.ReadFull(r, packet[:pgsHeaderSize])
		stats.BytesRead += int64(n)
		if err == io.EOF && n == 0 {
			if stats.Segments == 0 {
				return stats, ErrEmptyInput
			}
			if openDisplay || len(fragments) != 0 {
				return stats, fmt.Errorf("PGS at byte %d: incomplete display set: %w", offset, io.ErrUnexpectedEOF)
			}
			return stats, nil
		}
		if err != nil {
			return stats, fmt.Errorf("PGS header at byte %d: %w", offset, err)
		}
		if packet[0] != 'P' || packet[1] != 'G' {
			return stats, fmt.Errorf("PGS at byte %d: expected SUP magic PG", offset)
		}
		size := int(binary.BigEndian.Uint16(packet[11:13]))
		p := packet[pgsHeaderSize : pgsHeaderSize+size]
		n, err = io.ReadFull(r, p)
		stats.BytesRead += int64(n)
		if err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return stats, fmt.Errorf("PGS payload at byte %d: %w", offset, err)
		}
		if !openDisplay && (packet[10] == pgsPalette || packet[10] == pgsObject || packet[10] == pgsWindow) {
			return stats, fmt.Errorf("PGS segment 0x%02x at byte %d: dependency segment outside a display set", packet[10], offset)
		}
		switch packet[10] {
		case pgsPresentation:
			if openDisplay {
				err = fmt.Errorf("new presentation before previous display-set END")
				break
			}
			var objects, forced int
			p, objects, forced, err = filterPGSPresentation(p)
			if err == nil {
				openDisplay = true
				stats.Presentations++
				stats.Objects += int64(objects)
				stats.ForcedObjects += int64(forced)
				if forced != 0 {
					stats.ForcedPresentations++
				}
			}
		case pgsPalette:
			if len(p) < 2 || (len(p)-2)%5 != 0 {
				err = fmt.Errorf("invalid palette segment length %d", len(p))
			}
		case pgsObject:
			err = checkPGSObject(p, fragments)
		case pgsWindow:
			if len(p) < 1 || len(p) != 1+int(p[0])*9 {
				err = fmt.Errorf("invalid window segment length %d", len(p))
			}
		case pgsEnd:
			if len(p) != 0 {
				err = fmt.Errorf("non-empty display-set END")
			} else if !openDisplay {
				err = fmt.Errorf("display-set END without presentation")
			} else if len(fragments) != 0 {
				err = fmt.Errorf("display-set END with incomplete object fragments")
			} else {
				openDisplay = false
			}
		default:
			err = fmt.Errorf("unsupported PGS segment type 0x%02x", packet[10])
		}
		if err != nil {
			return stats, fmt.Errorf("PGS segment 0x%02x at byte %d: %w", packet[10], offset, err)
		}
		stats.Segments++
		binary.BigEndian.PutUint16(packet[11:13], uint16(len(p)))
		n, err = writePacket(w, packet[:pgsHeaderSize+len(p)])
		stats.BytesWritten += int64(n)
		if err != nil {
			return stats, fmt.Errorf("write PGS at byte %d: %w", offset, err)
		}
	}
}

func filterPGSPresentation(p []byte) ([]byte, int, int, error) {
	if len(p) < 11 {
		return nil, 0, 0, fmt.Errorf("presentation needs 11 header bytes, got %d", len(p))
	}
	if binary.BigEndian.Uint16(p[:2]) == 0 || binary.BigEndian.Uint16(p[2:4]) == 0 {
		return nil, 0, 0, fmt.Errorf("zero presentation dimensions")
	}
	count := int(p[10])
	src, dst, kept := 11, 11, 0
	for i := 0; i < count; i++ {
		if len(p)-src < 8 {
			return nil, 0, 0, fmt.Errorf("truncated composition object %d", i)
		}
		flags := p[src+3]
		size := 8
		if flags&0x80 != 0 {
			size += 8
		}
		if len(p)-src < size {
			return nil, 0, 0, fmt.Errorf("truncated crop fields for composition object %d", i)
		}
		if flags&0x40 != 0 {
			copy(p[dst:dst+size], p[src:src+size])
			dst += size
			kept++
		}
		src += size
	}
	if src != len(p) {
		return nil, 0, 0, fmt.Errorf("%d trailing presentation bytes", len(p)-src)
	}
	p[10] = byte(kept)
	return p[:dst], count, kept, nil
}

type pgsFragment struct {
	version   byte
	remaining int
}

func checkPGSObject(p []byte, pending map[uint16]pgsFragment) error {
	if len(p) < 4 {
		return fmt.Errorf("truncated object header")
	}
	id := binary.BigEndian.Uint16(p[:2])
	state, exists := pending[id]
	if p[3]&0x80 != 0 {
		if exists {
			return fmt.Errorf("object %d restarted before its final fragment", id)
		}
		if len(p) < 11 {
			return fmt.Errorf("truncated first fragment of object %d", id)
		}
		length := int(p[4])<<16 | int(p[5])<<8 | int(p[6])
		if length < 4 || binary.BigEndian.Uint16(p[7:9]) == 0 || binary.BigEndian.Uint16(p[9:11]) == 0 {
			return fmt.Errorf("invalid dimensions/length for object %d", id)
		}
		state = pgsFragment{version: p[2], remaining: length - 4 - (len(p) - 11)}
	} else {
		if !exists || state.version != p[2] {
			return fmt.Errorf("object %d continuation has no matching first fragment", id)
		}
		state.remaining -= len(p) - 4
	}
	if state.remaining < 0 {
		return fmt.Errorf("object %d fragments exceed declared length", id)
	}
	if p[3]&0x40 != 0 {
		if state.remaining != 0 {
			return fmt.Errorf("object %d final fragment is missing %d bytes", id, state.remaining)
		}
		delete(pending, id)
	} else {
		pending[id] = state
	}
	return nil
}
