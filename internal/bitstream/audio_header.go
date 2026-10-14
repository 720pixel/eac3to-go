package bitstream

import "fmt"

// AudioHeader describes one AC-3 or E-AC-3 sync frame. Samples is the number of
// samples per channel, including for E-AC-3 dependent substreams.
type AudioHeader struct {
	Size        int
	SampleRate  int
	Samples     int
	BitRate     int
	Enhanced    bool
	ByteSwapped bool
}

// ParseAudioHeader reads at least eight bytes without changing the input. It
// accepts either byte order and validates the sync word, bitstream identifier,
// stream type, size and rate codes. It does not require the complete frame or
// validate its CRC. Size is bounded to 4096 bytes.
//
// Rate and block-count fields follow ATSC A/52 and ETSI TS 102 366; see also
// https://github.com/FFmpeg/FFmpeg/blob/master/libavcodec/ac3_parser.c.
func ParseAudioHeader(data []byte) (AudioHeader, error) {
	var header AudioHeader
	var prefix [8]byte
	copy(prefix[:], data)
	n := min(len(data), len(prefix))
	if n >= 2 && prefix[0] == 0x77 && prefix[1] == 0x0b {
		header.ByteSwapped = true
		// A short prefix is rejected by audioFrameSize below.
		swapWords(prefix[:])
	}
	size, enhanced, err := audioFrameSize(prefix[:n])
	if err != nil {
		return AudioHeader{}, err
	}
	header.Size, header.Enhanced = size, enhanced
	rates := [...]int{48000, 44100, 32000}
	rateCode := prefix[4] >> 6
	if !enhanced {
		header.SampleRate = rates[rateCode]
		if bsid := prefix[5] >> 3; bsid > 8 {
			header.SampleRate >>= bsid - 8
		}
		header.Samples = 1536
		header.BitRate = [...]int{32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384, 448, 512, 576, 640}[(prefix[4]&63)/2] * 1000
		if bsid := prefix[5] >> 3; bsid > 8 {
			header.BitRate >>= bsid - 8
		}
	} else if rateCode == 3 {
		header.SampleRate = rates[(prefix[4]>>4)&3] / 2
		header.Samples = 1536
	} else {
		header.SampleRate = rates[rateCode]
		header.Samples = 256 * [...]int{1, 2, 3, 6}[(prefix[4]>>4)&3]
	}
	if enhanced {
		header.BitRate = size * 8 * header.SampleRate / header.Samples
	}
	return header, nil
}

// ValidateAudioFrame checks the supported AC-3/E-AC-3 header, exact frame length
// and frame CRCs without changing data. Word-swapped frames are accepted. This
// is a framing and checksum check, not a complete compressed-audio decode.
func ValidateAudioFrame(data []byte) error {
	header, err := ParseAudioHeader(data)
	if err != nil {
		return err
	}
	if len(data) != header.Size {
		return fmt.Errorf("audio frame has %d bytes, header declares %d", len(data), header.Size)
	}
	if header.ByteSwapped {
		normalized := make([]byte, len(data))
		copy(normalized, data)
		swapWords(normalized)
		data = normalized
	}
	return checkAudioCRC(data, header.Enhanced)
}
