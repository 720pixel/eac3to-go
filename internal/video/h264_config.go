package video

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// ParseH264Config reads SPSs from Annex B codec data or an AVC decoder
// configuration record. It validates record lengths and rejects extension SPS
// records whose extra timing syntax is not supported. All returned SPSs must
// be considered when deciding whether a source has one consistent clock.
func ParseH264Config(data []byte) ([]H264SPS, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("H.264 codec data has no SPS")
	}
	if len(data) > 1<<20 {
		return nil, fmt.Errorf("H.264 codec data exceeds 1 MiB")
	}
	var result []H264SPS
	add := func(nal []byte, expected byte) error {
		if len(nal) < 2 || nal[0]&0x80 != 0 {
			return fmt.Errorf("invalid H.264 codec-data NAL header")
		}
		kind := nal[0] & 31
		if expected != 0 && kind != expected {
			return fmt.Errorf("H.264 codec data contains NAL type %d, expected %d", kind, expected)
		}
		switch kind {
		case 7:
			if len(result) >= 32 {
				return fmt.Errorf("H.264 codec data contains more than 32 SPSs")
			}
			info, err := ParseH264SPS(nal)
			if err != nil {
				return err
			}
			result = append(result, info)
		case 13, 14, 15, 20, 21:
			return fmt.Errorf("H.264 codec data uses unsupported timing extension NAL type %d", kind)
		case 6, 8, 9:
		default:
			return fmt.Errorf("unexpected H.264 codec-data NAL type %d", kind)
		}
		return nil
	}
	if data[0] == 1 {
		if len(data) < 7 || data[4]&0xfc != 0xfc || data[4]&3 == 2 || data[5]&0xe0 != 0xe0 {
			return nil, fmt.Errorf("invalid AVC decoder configuration header")
		}
		position := 6
		read := func(count int, expected byte) error {
			for i := 0; i < count; i++ {
				if len(data)-position < 2 {
					return fmt.Errorf("truncated AVC parameter-set length")
				}
				size := int(binary.BigEndian.Uint16(data[position:]))
				position += 2
				if size == 0 || size > len(data)-position {
					return fmt.Errorf("truncated AVC parameter set")
				}
				if err := add(data[position:position+size], expected); err != nil {
					return err
				}
				position += size
			}
			return nil
		}
		if err := read(int(data[5]&31), 7); err != nil {
			return nil, err
		}
		if position == len(data) {
			return nil, fmt.Errorf("missing AVC picture-parameter-set count")
		}
		pps := int(data[position])
		position++
		if err := read(pps, 8); err != nil {
			return nil, err
		}
		if position < len(data) {
			if len(data)-position < 4 || data[position]&0xfc != 0xfc || data[position+1]&0xf8 != 0xf8 || data[position+2]&0xf8 != 0xf8 {
				return nil, fmt.Errorf("invalid AVC configuration extension")
			}
			extensions := int(data[position+3])
			position += 4
			if extensions != 0 {
				return nil, fmt.Errorf("AVC configuration SPS extensions need separate timing validation")
			}
		}
		if position != len(data) {
			return nil, fmt.Errorf("unexpected trailing AVC configuration bytes")
		}
	} else {
		prefix := []byte{0, 0, 1}
		start := bytes.Index(data, prefix)
		if start < 0 || len(bytes.Trim(data[:start], "\x00")) != 0 {
			return nil, fmt.Errorf("H.264 codec data is neither Annex B nor an AVC configuration record")
		}
		for {
			body := start + 3
			next := bytes.Index(data[body:], prefix)
			end := len(data)
			if next >= 0 {
				end = body + next
			}
			if err := add(bytes.TrimRight(data[body:end], "\x00"), 0); err != nil {
				return nil, err
			}
			if next < 0 {
				break
			}
			start = end
		}
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("H.264 codec data has no SPS")
	}
	return result, nil
}
