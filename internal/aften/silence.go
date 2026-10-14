// Package aften encodes AC-3 audio using the historical Aften encoder.
package aften

import "errors"

var (
	ErrUnavailable  = errors.New("Aften is unavailable in this build; rebuild with CGO_ENABLED=1 and a C compiler")
	ErrInvalidFrame = errors.New("Aften silence requires one complete, valid big-endian AC-3 frame")
	ErrChecksum     = errors.New("AC-3 frame checksum mismatch")
	ErrFrameSize    = errors.New("Aften did not produce the required silent frame size")
	ErrUnsupported  = errors.New("Aften silent-frame generation does not support E-AC-3 or reduced-rate AC-3")
)

// SilenceFromAC3Frame generates one frame of zero PCM using the source frame's
// rate, bitrate, channel mode and supported mixing metadata. The input is not
// modified. Both source CRC partitions must be valid. E-AC-3 and reduced-rate
// AC-3 (bitstream identifiers 9 and 10) are unsupported.
//
// The expected size follows the historical integer bitrate/rate calculation.
// Some 44.1 kHz configurations therefore return ErrFrameSize. Callers that need
// historical delay behavior can explicitly fall back to repeating the source
// frame when generation fails; this function never hides that fallback.
func SilenceFromAC3Frame(frame []byte) ([]byte, error) { return silence(frame) }
