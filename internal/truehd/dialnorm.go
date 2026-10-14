package truehd

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// ErrAtmosDialogNormalization signals eac3to's explicit metadata limitation.
// Compatibility callers report this once and preserve the unchanged frame;
// the Windows tool completes the extraction successfully with that notice.
var ErrAtmosDialogNormalization = errors.New("dialog normalization cannot be removed from Atmos audio")

// RemoveDialogNormalization edits a checked major header in the borrowed frame
// storage and updates its checksum. Nonmajor units remain unchanged. eac3to's
// neutral codes are 37 for the two-channel presentation and 31 for the six- and
// eight-channel presentations; zeroing these fields would not match it.
// Apply this before buffering the frame for the optional final postprocess.
func RemoveDialogNormalization(frame Frame) (bool, error) {
	if !frame.MajorSync {
		return false, nil
	}
	if len(frame.Bytes) < 8 || binary.BigEndian.Uint32(frame.Bytes[4:8]) != 0xf8726fba || int(binary.BigEndian.Uint16(frame.Bytes[:2])&0xfff)*2 != len(frame.Bytes) {
		return false, fmt.Errorf("invalid TrueHD major access unit for dialogue normalization")
	}
	h := frame.Bytes[4:]
	format, err := parseMajor(h)
	if err != nil {
		return false, err
	}
	if format.SubstreamInfo&0x80 != 0 {
		return false, ErrAtmosDialogNormalization
	}
	size, err := majorHeaderSize(h)
	if err != nil {
		return false, err
	}
	changed := (h[20]&0x7e) != 0x4a || h[21]&7 != 7 || h[22]&0xc0 != 0xc0 || h[23]&7 != 7 || h[24]&0xc0 != 0xc0
	if !changed {
		return false, nil
	}
	h[20] = (h[20] & 0x81) | 0x4a
	h[21] |= 7
	h[22] |= 0xc0
	h[23] |= 7
	h[24] |= 0xc0
	binary.LittleEndian.PutUint16(h[size-2:size], majorChecksum(h[:size]))
	return true, nil
}
