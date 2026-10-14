// Package seamless frames compressed audio without changing its bytes and
// applies explicitly bounded comparisons at source-file boundaries.
package seamless

import "errors"

type Codec uint8

const (
	// AC3 also accepts individual E-AC-3 sync frames. Independent/dependent
	// E-AC-3 substreams are not assembled into presentation access units.
	AC3 Codec = iota + 1
	DTS
)

const DefaultMaxFrameBytes = (1 << 20) + 16384 + 3

var (
	ErrFinished    = errors.New("audio parser is finished")
	ErrUnsupported = errors.New("unsupported compressed audio framing")
)

// Frame.Bytes is borrowed until the callback returns. Samples and SampleRate
// describe the time interval, using the compatible core clock for DTS-HD.
// CoreBytes excludes padding and ExtensionBytes includes the complete ExSS.
// E-AC-3 dependent sync frames describe concurrent substreams; their Samples
// must not be added to the independent substream's presentation duration.
type Frame struct {
	Bytes                                   []byte
	Samples, SampleRate                     int
	CoreBytes, PaddingBytes, ExtensionBytes int
	Enhanced                                bool
}

// Duration90k truncates to the transport clock. Callers that accumulate durations
// should retain a rational remainder rather than repeatedly truncating frames.
func (f Frame) Duration90k() int64 {
	if f.SampleRate <= 0 {
		return 0
	}
	return int64(f.Samples) * 90000 / int64(f.SampleRate)
}

type Config struct {
	Codec Codec
	// MaxFrameBytes bounds a complete core+extension and all pending input.
	// Zero uses the largest ExSS size plus the largest 16-bit DTS core.
	MaxFrameBytes int
	OnFrame       func(Frame) error
}
