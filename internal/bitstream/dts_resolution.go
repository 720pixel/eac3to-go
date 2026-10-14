package bitstream

import "fmt"

// RepairDTSCoreResolution applies the historical 24-bit source-resolution
// header fix while retaining the ES flag. CRC-bearing core headers are left
// unchanged. The compressed samples and any following ExSS data are untouched.
func RepairDTSCoreResolution(data []byte) (bool, error) {
	if len(data) < 14 {
		return false, fmt.Errorf("DTS core header needs at least 14 bytes")
	}
	var header [14]byte
	copy(header[:], data)
	swapped, err := dtsByteSwapped(header[:])
	if err != nil {
		return false, err
	}
	if swapped {
		swapWords(header[:])
	}
	shape, err := dtsFrameBounds(header[:])
	if err != nil {
		return false, err
	}
	if shape.extension {
		return false, nil
	}
	if len(data) < shape.size {
		return false, fmt.Errorf("DTS core frame is truncated")
	}
	if !repairDTSCoreResolution(header[:]) {
		return false, nil
	}
	if swapped {
		swapWords(header[:])
	}
	copy(data[:14], header[:])
	return true, nil
}

func repairDTSCoreResolution(header []byte) bool {
	if header[4]&2 != 0 {
		return false
	}
	old11, old12 := header[11], header[12]
	header[11] |= 1
	if header[12]&0x40 != 0 {
		header[12] &^= 0x80
	} else {
		header[12] |= 0x80
	}
	return header[11] != old11 || header[12] != old12
}
