package bitstream

import (
	"encoding/binary"
	"fmt"
	"io"
)

// AudioStats describes complete frames processed by RemoveDialnorm. Counts
// include E-AC-3 dependent substreams and both dialnorm fields in dual mono.
type AudioStats struct {
	Frames            int64
	ChangedFrames     int64
	AC3Frames         int64
	EAC3Frames        int64
	DialnormFields    int64
	ChangedFields     int64
	ByteSwappedFrames int64
	BytesRead         int64
	BytesWritten      int64
}

// RemoveDialnorm sets every AC-3/E-AC-3 dialnorm field to 31 (-31 dB, no
// attenuation), preserving compressed samples and all other metadata. It accepts
// big-endian elementary streams and 16-bit-word-swapped streams, preserving each
// frame's byte order. It covers AC-3 bsid <= 10 (including reduced-rate 9/10) and
// E-AC-3 bsid 16, all stream types except reserved type 3, and dual mono. It
// deliberately rejects reserved bsid 11..15, bad CRCs and lost synchronization;
// containers and padding should be removed by the caller's demuxer first.
//
// The bit fields and CRC polynomial x^16+x^15+x^2+1 follow ATSC A/52 and
// ETSI TS 102 366. Cross-checks for header syntax and CRC extents:
// https://github.com/FFmpeg/FFmpeg/blob/master/libavcodec/ac3_parser.c
// https://github.com/FFmpeg/FFmpeg/blob/master/libavcodec/ac3enc.c
// This implementation uses an independently derived reverse-CRC algorithm for
// AC-3's leading CRC. It neither decodes nor re-encodes samples. On rare frames
// CRC regeneration also toggles the reserved CRC bit to avoid a 0x0b77 CRC2.
func RemoveDialnorm(r io.Reader, w io.Writer) (AudioStats, error) {
	var stats AudioStats
	var storage [4096]byte // E-AC-3's eleven-bit word count is the largest frame.
	for {
		offset := stats.BytesRead
		n, err := io.ReadFull(r, storage[:8])
		stats.BytesRead += int64(n)
		if err == io.EOF && n == 0 {
			if stats.Frames == 0 {
				return stats, ErrEmptyInput
			}
			return stats, nil
		}
		if err != nil {
			return stats, fmt.Errorf("audio header at byte %d: %w", offset, err)
		}
		swapped := storage[0] == 0x77 && storage[1] == 0x0b
		if swapped {
			swapWords(storage[:8])
		}
		size, enhanced, err := audioFrameSize(storage[:8])
		if err != nil {
			return stats, fmt.Errorf("audio frame at byte %d: %w", offset, err)
		}
		frame := storage[:size]
		n, err = io.ReadFull(r, frame[8:])
		stats.BytesRead += int64(n)
		if err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return stats, fmt.Errorf("audio frame at byte %d: %w", offset, err)
		}
		if swapped {
			swapWords(frame[8:])
		}
		if err = checkAudioCRC(frame, enhanced); err != nil {
			return stats, fmt.Errorf("audio frame at byte %d: %w", offset, err)
		}
		positions, err := dialnormPositions(frame, enhanced)
		if err != nil {
			return stats, fmt.Errorf("audio frame at byte %d: %w", offset, err)
		}
		changed := int64(0)
		for _, pos := range positions {
			if getBitsAt(frame, pos, 5) != 31 {
				setFiveOnes(frame, pos)
				changed++
			}
		}
		if changed > 0 {
			updateAudioCRC(frame, enhanced)
			stats.ChangedFrames++
		}
		stats.Frames++
		stats.DialnormFields += int64(len(positions))
		stats.ChangedFields += changed
		if enhanced {
			stats.EAC3Frames++
		} else {
			stats.AC3Frames++
		}
		if swapped {
			stats.ByteSwappedFrames++
			swapWords(frame)
		}
		n, err = writePacket(w, frame)
		stats.BytesWritten += int64(n)
		if err != nil {
			return stats, fmt.Errorf("write audio frame at byte %d: %w", offset, err)
		}
	}
}

// audioFrameSize needs eight bytes and returns a bounded, even frame length.
func audioFrameSize(h []byte) (size int, enhanced bool, err error) {
	if len(h) < 8 {
		return 0, false, io.ErrUnexpectedEOF
	}
	if h[0] != 0x0b || h[1] != 0x77 {
		return 0, false, fmt.Errorf("expected AC-3/E-AC-3 sync word 0b77")
	}
	bsid := h[5] >> 3
	if bsid <= 10 {
		rateCode, sizeCode := h[4]>>6, h[4]&0x3f
		if rateCode == 3 || sizeCode > 37 {
			return 0, false, fmt.Errorf("invalid AC-3 sample-rate/frame-size code %d/%d", rateCode, sizeCode)
		}
		kbps := [...]int{32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384, 448, 512, 576, 640}[sizeCode/2]
		switch rateCode {
		case 0: // 48 kHz, six blocks of 256 samples.
			size = kbps * 4
		case 1: // 44.1 kHz; the size-code LSB selects the padded word.
			size = (kbps*320/147 + int(sizeCode&1)) * 2
		case 2: // 32 kHz.
			size = kbps * 6
		}
		return size, false, nil
	}
	if bsid != 16 {
		return 0, false, fmt.Errorf("unsupported/reserved bitstream id %d", bsid)
	}
	if h[2]>>6 == 3 {
		return 0, true, fmt.Errorf("reserved E-AC-3 stream type 3")
	}
	if h[4]>>6 == 3 && (h[4]>>4)&3 == 3 {
		return 0, true, fmt.Errorf("reserved E-AC-3 reduced sample-rate code 3")
	}
	size = (int(h[2]&7)<<8|int(h[3]))*2 + 2
	if size < 10 { // Eight-byte header plus a non-overlapping CRC word.
		return 0, true, fmt.Errorf("E-AC-3 frame too short (%d bytes)", size)
	}
	return size, true, nil
}

func dialnormPositions(frame []byte, enhanced bool) ([]int, error) {
	// Exclude CRC2; no syntax read may run into the checksum.
	b := bitReader{data: frame[:len(frame)-2]}
	var acmod int
	if enhanced {
		b.pos = 36 // fscod plus numblkscod/fscod2 have already passed.
		acmod = b.read(3)
		b.skip(6) // lfeon (1) and bsid (5).
	} else {
		b.pos = 48 // syncinfo, bsid and bsmod.
		acmod = b.read(3)
		if acmod&1 != 0 && acmod != 1 {
			b.skip(2) // cmixlev
		}
		if acmod&4 != 0 {
			b.skip(2) // surmixlev
		}
		if acmod == 2 {
			b.skip(2) // dsurmod
		}
		b.skip(1) // lfeon
	}
	positions := make([]int, 0, 2)
	programs := 1
	if acmod == 0 {
		programs = 2
	}
	for i := 0; i < programs; i++ {
		positions = append(positions, b.pos)
		b.skip(5) // dialnorm
		if b.read(1) != 0 {
			b.skip(8) // compression gain word
		}
		if !enhanced {
			if b.read(1) != 0 {
				b.skip(8) // langcod
			}
			if b.read(1) != 0 {
				b.skip(7) // mixlevel/roomtyp
			}
		}
	}
	if b.err != nil {
		return nil, fmt.Errorf("truncated dialnorm metadata: %w", b.err)
	}
	return positions, nil
}

type bitReader struct {
	data []byte
	pos  int
	err  error
}

func (b *bitReader) read(n int) int {
	if b.err != nil {
		return 0
	}
	if b.pos+n > len(b.data)*8 {
		b.err = io.ErrUnexpectedEOF
		return 0
	}
	v := getBitsAt(b.data, b.pos, n)
	b.pos += n
	return v
}

func (b *bitReader) skip(n int) { b.read(n) }

func getBitsAt(p []byte, pos, n int) int {
	v := 0
	for i := 0; i < n; i++ {
		v = v<<1 | int((p[(pos+i)/8]>>uint(7-(pos+i)%8))&1)
	}
	return v
}

func setFiveOnes(p []byte, pos int) {
	for i := 0; i < 5; i++ {
		p[(pos+i)/8] |= 1 << uint(7-(pos+i)%8)
	}
}

func swapWords(p []byte) {
	for i := 0; i < len(p); i += 2 {
		p[i], p[i+1] = p[i+1], p[i]
	}
}

// The first CRC extent is (floor(words/2)+floor(words/8)) 16-bit words,
// not simply floor(frameBytes*5/8), which differs for 44.1 kHz frames.
func ac3FirstCRCSize(size int) int { return (size/4 + size/16) * 2 }

func checkAudioCRC(p []byte, enhanced bool) error {
	if enhanced {
		if crc16ANSI(p[2:]) != 0 {
			return fmt.Errorf("E-AC-3 CRC mismatch")
		}
		return nil
	}
	split := ac3FirstCRCSize(len(p))
	if crc16ANSI(p[2:split]) != 0 {
		return fmt.Errorf("AC-3 CRC1 mismatch")
	}
	if crc16ANSI(p[split:]) != 0 {
		return fmt.Errorf("AC-3 CRC2 mismatch")
	}
	return nil
}

func updateAudioCRC(p []byte, enhanced bool) {
	start := 2
	if !enhanced {
		start = ac3FirstCRCSize(len(p))
		// Rewind a desired zero remainder through all bytes after CRC1,
		// then invert the mapping of the two initial CRC bytes themselves.
		required := undoCRC16(0, p[4:start])
		leading := undoCRC16(required, []byte{0, 0})
		binary.BigEndian.PutUint16(p[2:4], leading)
	}
	crc := crc16ANSI(p[start : len(p)-2])
	if crc == 0x0b77 {
		// crcrsv is the last bit before CRC2. Flipping it changes the
		// remainder by 0x8005, avoiding an emulated frame sync word.
		p[len(p)-3] ^= 1
		crc ^= 0x8005
	}
	binary.BigEndian.PutUint16(p[len(p)-2:], crc)
}
