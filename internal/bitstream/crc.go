package bitstream

// Non-reflected CRC-16/ANSI with polynomial 0x8005 and initial remainder 0.
var crc16Table = func() [256]uint16 {
	var table [256]uint16
	for i := range table {
		v := uint16(i) << 8
		for j := 0; j < 8; j++ {
			if v&0x8000 != 0 {
				v = v<<1 ^ 0x8005
			} else {
				v <<= 1
			}
		}
		table[i] = v
	}
	return table
}()

// Because the polynomial's constant term is 1, the table's low bytes are
// a permutation. That provides a constant-time inverse of each byte update.
var crc16Inverse = func() [256]byte {
	var inverse [256]byte
	for i, v := range crc16Table {
		inverse[byte(v)] = byte(i)
	}
	return inverse
}()

func crc16ANSI(p []byte) uint16 {
	var crc uint16
	for _, v := range p {
		crc = crc<<8 ^ crc16Table[byte(crc>>8)^v]
	}
	return crc
}

func undoCRC16(crc uint16, p []byte) uint16 {
	for i := len(p) - 1; i >= 0; i-- {
		index := crc16Inverse[byte(crc)]
		crc = uint16(index^p[i])<<8 | (crc^crc16Table[index])>>8
	}
	return crc
}
