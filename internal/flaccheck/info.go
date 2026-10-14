// Package flaccheck verifies decoded PCM against FLAC STREAMINFO metadata.
package flaccheck

import (
	"encoding/binary"
	"fmt"
)

const maxTotalFrames = uint64(1<<36 - 1)

// Info contains the fixed-size STREAMINFO block defined by RFC 9639, section 8.2.
// TotalFrames is an interchannel sample count. Zero means the count is unknown.
// An all-zero MD5 means the source does not provide a checksum.
type Info struct {
	MinBlockSize  uint16
	MaxBlockSize  uint16
	MinFrameSize  uint32
	MaxFrameSize  uint32
	SampleRate    int
	Channels      int
	BitsPerSample int
	TotalFrames   uint64
	MD5           [16]byte
}

// ParseStreamInfo parses the 34-byte block body, without a metadata header.
// A zero sample rate is preserved because FLAC permits it for non-audio data.
func ParseStreamInfo(data []byte) (Info, error) {
	if len(data) != 34 {
		return Info{}, fmt.Errorf("FLAC STREAMINFO must contain exactly 34 bytes, got %d", len(data))
	}
	packed := binary.BigEndian.Uint64(data[10:18])
	info := Info{
		MinBlockSize:  binary.BigEndian.Uint16(data[0:2]),
		MaxBlockSize:  binary.BigEndian.Uint16(data[2:4]),
		MinFrameSize:  uint32(data[4])<<16 | uint32(data[5])<<8 | uint32(data[6]),
		MaxFrameSize:  uint32(data[7])<<16 | uint32(data[8])<<8 | uint32(data[9]),
		SampleRate:    int(packed >> 44),
		Channels:      int(packed>>41&7) + 1,
		BitsPerSample: int(packed>>36&31) + 1,
		TotalFrames:   packed & maxTotalFrames,
	}
	copy(info.MD5[:], data[18:34])
	if info.MinBlockSize < 16 || info.MaxBlockSize < 16 || info.MinBlockSize > info.MaxBlockSize {
		return Info{}, fmt.Errorf("FLAC STREAMINFO has invalid block sizes %d..%d", info.MinBlockSize, info.MaxBlockSize)
	}
	if info.MinFrameSize != 0 && info.MaxFrameSize != 0 && info.MinFrameSize > info.MaxFrameSize {
		return Info{}, fmt.Errorf("FLAC STREAMINFO has invalid frame sizes %d..%d", info.MinFrameSize, info.MaxFrameSize)
	}
	if err := validateFormat(info); err != nil {
		return Info{}, err
	}
	return info, nil
}

func validateFormat(info Info) error {
	if info.SampleRate < 0 || info.SampleRate > 1<<20-1 {
		return fmt.Errorf("FLAC STREAMINFO has invalid sample rate %d", info.SampleRate)
	}
	if info.Channels < 1 || info.Channels > 8 {
		return fmt.Errorf("FLAC STREAMINFO has invalid channel count %d", info.Channels)
	}
	if info.BitsPerSample < 4 || info.BitsPerSample > 32 {
		return fmt.Errorf("FLAC STREAMINFO has invalid precision %d bits", info.BitsPerSample)
	}
	if info.TotalFrames > maxTotalFrames {
		return fmt.Errorf("FLAC STREAMINFO sample count %d exceeds its 36-bit field", info.TotalFrames)
	}
	return nil
}
