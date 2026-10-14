package seamless

import (
	"encoding/binary"
	"fmt"
	"io"

	"eac3to/internal/bitstream"
)

func ac3Header(h []byte) (Frame, int, error) {
	info, err := bitstream.ParseAudioHeader(h)
	if err != nil {
		return Frame{}, 0, err
	}
	return Frame{Samples: info.Samples, SampleRate: info.SampleRate, CoreBytes: info.Size, Enhanced: info.Enhanced}, info.Size, nil
}

// Header field positions are checked against FFmpeg's dca.c and dca_exss.c:
// https://github.com/FFmpeg/FFmpeg/blob/n6.1/libavcodec/dca.c
// https://github.com/FFmpeg/FFmpeg/blob/n6.1/libavcodec/dca_exss.c
func dtsCoreHeader(h []byte) (Frame, int, error) {
	if len(h) < 14 {
		return Frame{}, 0, io.ErrUnexpectedEOF
	}
	var normalized [14]byte
	switch binary.BigEndian.Uint32(h) {
	case 0x7ffe8001:
		copy(normalized[:], h)
	case 0xfe7f0180:
		for i := 0; i < 14; i += 2 {
			normalized[i], normalized[i+1] = h[i+1], h[i]
		}
	case 0x1fffe800, 0xff1f00e8:
		return Frame{}, 0, fmt.Errorf("%w: 14-bit DTS core", ErrUnsupported)
	case 0x64582025:
		return Frame{}, 0, fmt.Errorf("%w: DTS extension without a core", ErrUnsupported)
	default:
		return Frame{}, 0, fmt.Errorf("invalid DTS core sync word")
	}
	b := normalized[:]
	blocks := int(bits(b, 39, 7)) + 1
	size := int(bits(b, 46, 14)) + 1
	rates := [...]int{0, 8000, 16000, 32000, 0, 0, 11025, 22050, 44100, 0, 0, 12000, 24000, 48000, 96000, 192000}
	rate := rates[bits(b, 66, 4)]
	if bits(b, 33, 5) != 31 || blocks%8 != 0 || size < 96 || rate == 0 || bits(b, 60, 6) > 15 {
		return Frame{}, 0, fmt.Errorf("invalid DTS core size, block count, or sample rate")
	}
	return Frame{Samples: blocks * 32, SampleRate: rate, CoreBytes: size}, size, nil
}

func exssHeader(h []byte) (int, error) {
	if len(h) < 10 {
		return 0, io.ErrUnexpectedEOF
	}
	if binary.BigEndian.Uint32(h) != 0x64582025 {
		return 0, fmt.Errorf("invalid DTS ExSS sync word")
	}
	wide := int(bits(h, 42, 1))
	headerBits := 8 + 4*wide
	headerSize := int(bits(h, 43, headerBits)) + 1
	size := int(bits(h, 43+headerBits, 16+4*wide)) + 1
	if headerSize < 10 || size < headerSize {
		return 0, fmt.Errorf("invalid DTS ExSS header/frame size")
	}
	return size, nil
}

func bits(b []byte, offset, width int) uint32 {
	var value uint32
	for i := 0; i < width; i++ {
		n := offset + i
		value = value<<1 | uint32((b[n/8]>>uint(7-n%8))&1)
	}
	return value
}
