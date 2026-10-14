package transport

import "fmt"

func adaptation(packet []byte, control byte) (int, bool, error) {
	if control&2 == 0 {
		return 4, false, nil
	}
	length := int(packet[4])
	if control == 2 && length != 183 || control == 3 && length > 182 {
		return 0, false, fmt.Errorf("invalid adaptation-field length %d for control %d", length, control)
	}
	end := 5 + length
	if length == 0 {
		return end, false, nil
	}
	flags, position := packet[5], 6
	consume := func(n int) bool {
		if n > end-position {
			return false
		}
		position += n
		return true
	}
	for _, field := range []struct {
		flag byte
		size int
	}{{0x10, 6}, {0x08, 6}, {0x04, 1}} {
		if flags&field.flag != 0 && !consume(field.size) {
			return 0, false, fmt.Errorf("adaptation field is shorter than its declared optional fields")
		}
	}
	for _, flag := range []byte{0x02, 0x01} {
		if flags&flag != 0 {
			if !consume(1) || !consume(int(packet[position-1])) {
				return 0, false, fmt.Errorf("truncated adaptation private-data or extension field")
			}
		}
	}
	for _, stuffing := range packet[position:end] {
		if stuffing != 0xff {
			return 0, false, fmt.Errorf("non-0xff adaptation stuffing byte")
		}
	}
	return end, flags&0x80 != 0, nil
}

func parsePES(raw []byte) (PES, error) {
	result := PES{PTS: NoTimestamp, DTS: NoTimestamp, ArrivalTimestamp: NoTimestamp, StreamIDExtension: -1, Raw: raw}
	if len(raw) < 6 || raw[0] != 0 || raw[1] != 0 || raw[2] != 1 {
		return result, fmt.Errorf("invalid or truncated PES start code")
	}
	result.StreamID = raw[3]
	switch result.StreamID {
	case 0xbc, 0xbe, 0xbf, 0xf0, 0xf1, 0xf2, 0xf8, 0xff:
		result.Payload = raw[6:]
		return result, nil
	}
	if len(raw) < 9 || raw[6]&0xc0 != 0x80 {
		return result, fmt.Errorf("missing MPEG-2 PES optional header")
	}
	if raw[6]&0x30 != 0 {
		return result, fmt.Errorf("scrambled PES payload cannot be extracted")
	}
	end := 9 + int(raw[8])
	if end > len(raw) {
		return result, fmt.Errorf("PES header data length exceeds packet size")
	}
	position := 9
	read := func(size int) ([]byte, error) {
		if size > end-position {
			return nil, fmt.Errorf("PES optional fields exceed the declared header length")
		}
		data := raw[position : position+size]
		position += size
		return data, nil
	}
	flags := raw[7]
	timestampFlags := flags >> 6
	if timestampFlags == 1 {
		return result, fmt.Errorf("reserved PES timestamp flags")
	}
	if timestampFlags >= 2 {
		data, err := read(5)
		if err != nil {
			return result, err
		}
		result.PTS, err = timestamp(data, timestampFlags)
		if err != nil {
			return result, err
		}
	}
	if timestampFlags == 3 {
		data, err := read(5)
		if err != nil {
			return result, err
		}
		result.DTS, err = timestamp(data, 1)
		if err != nil {
			return result, err
		}
	}
	for _, field := range []struct {
		flag byte
		size int
	}{{0x20, 6}, {0x10, 3}, {0x08, 1}, {0x04, 1}, {0x02, 2}} {
		if flags&field.flag != 0 {
			if _, err := read(field.size); err != nil {
				return result, err
			}
		}
	}
	if flags&1 != 0 {
		data, err := read(1)
		if err != nil {
			return result, err
		}
		extension := data[0]
		if extension&0x80 != 0 {
			if _, err = read(16); err != nil {
				return result, err
			}
		}
		if extension&0x40 != 0 {
			data, err = read(1)
			if err != nil {
				return result, err
			}
			if _, err = read(int(data[0])); err != nil {
				return result, err
			}
		}
		for _, flag := range []byte{0x20, 0x10} {
			if extension&flag != 0 {
				if _, err = read(2); err != nil {
					return result, err
				}
			}
		}
		if extension&1 != 0 {
			data, err = read(1)
			if err != nil {
				return result, err
			}
			if data[0]&0x80 == 0 {
				return result, fmt.Errorf("missing PES extension marker bit")
			}
			data, err = read(int(data[0] & 0x7f))
			if err != nil {
				return result, err
			}
			if len(data) > 0 && data[0]&0x80 == 0 {
				result.StreamIDExtension = int(data[0] & 0x7f)
			}
		}
	}
	for _, stuffing := range raw[position:end] {
		if stuffing != 0xff {
			return result, fmt.Errorf("non-0xff PES header stuffing byte")
		}
	}
	result.Payload = raw[end:]
	return result, nil
}

func timestamp(data []byte, prefix byte) (int64, error) {
	if len(data) != 5 || data[0]>>4 != prefix || data[0]&1 == 0 || data[2]&1 == 0 || data[4]&1 == 0 {
		return NoTimestamp, fmt.Errorf("invalid PES timestamp prefix or marker bit")
	}
	return int64(data[0]&0x0e)<<29 | int64(data[1])<<22 | int64(data[2]&0xfe)<<14 | int64(data[3])<<7 | int64(data[4]>>1), nil
}
