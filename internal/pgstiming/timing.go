// Package pgstiming assembles PGS elementary segments into SUP packets and maps
// explicit source-clip timestamps onto a playlist clock. It does not infer a
// video frame rate, execute menus, or select playlist items.
package pgstiming

import (
	"fmt"
	"math"
)

const NoTimestamp int64 = -1

type SourceClock uint8

const (
	Clock33 SourceClock = iota
	// WindowsClock32 reproduces the source timestamp width of the reference.
	WindowsClock32
)

type ClipTiming struct {
	FirstActualStart90k      int64
	CurrentActualStart90k    int64
	PriorPlaylistDuration90k int64
	In90k                    int64
	Strict                   bool
	Clock                    SourceClock
	// SourceCycles explicitly extends a wrapped raw timestamp. Scanned starts
	// and playlist durations must use the same extended source clock.
	SourceCycles int64
}

// MapClipTimestamp implements the linked playlist mapping: the first physical
// start plus completed playlist durations plus the offset within this clip.
// Strict uses playlist in-time as the source reference; default uses its actual
// scanned start. Filtering the PES against [in,out) remains caller responsibility.
func MapClipTimestamp(raw90k int64, timing ClipTiming) (int64, error) {
	if raw90k < 0 || raw90k >= 1<<33 {
		return 0, fmt.Errorf("PGS source timestamp is outside the 33-bit clock")
	}
	period := int64(1 << 33)
	if timing.Clock == WindowsClock32 {
		raw90k = int64(uint32(raw90k))
		if raw90k == math.MaxUint32 {
			return 0, fmt.Errorf("unavailable reference 32-bit PGS timestamp")
		}
		period = 1 << 32
	} else if timing.Clock != Clock33 {
		return 0, fmt.Errorf("unknown PGS source clock")
	}
	if timing.SourceCycles > math.MaxInt64/period || timing.SourceCycles < math.MinInt64/period {
		return 0, fmt.Errorf("PGS source-cycle overflow")
	}
	value, err := checkedAdd(raw90k, timing.SourceCycles*period)
	if err != nil {
		return 0, err
	}
	reference := timing.CurrentActualStart90k
	if timing.Strict {
		reference = timing.In90k
	}
	if reference < 0 || timing.FirstActualStart90k < 0 || timing.PriorPlaylistDuration90k < 0 {
		return 0, fmt.Errorf("negative PGS clip timing input")
	}
	value, err = checkedAdd(value, -reference)
	if err != nil {
		return 0, err
	}
	value, err = checkedAdd(value, timing.FirstActualStart90k)
	if err != nil {
		return 0, err
	}
	return checkedAdd(value, timing.PriorPlaylistDuration90k)
}

type OutputClock uint8

const (
	// Direct90k retains integer transport ticks, subtracting Origin90k.
	Direct90k OutputClock = iota
	// Windows100ns uses the recovered integer round trip through 100ns units.
	Windows100ns
)

type ClockConfig struct {
	Mode              OutputClock
	Origin90k         int64
	DelayMilliseconds int64
}

// WindowsDelayMilliseconds truncates the initial video timestamp to milliseconds
// and negates it. Analyzer codec offsets and other requested delays must be
// included by the caller when constructing ClockConfig.DelayMilliseconds.
func WindowsDelayMilliseconds(video90k int64) (int64, error) {
	if video90k < 0 {
		return 0, fmt.Errorf("invalid PGS origin")
	}
	return -(video90k / 90), nil
}

// SUPClock narrows the result modulo 2^32, including negative clock values.
// Source timestamps remain full-width until this explicit SUP conversion.
func SUPClock(mapped90k int64, config ClockConfig) (uint32, error) {
	var value int64
	var err error
	switch config.Mode {
	case Direct90k:
		if config.Origin90k == math.MinInt64 {
			return 0, fmt.Errorf("PGS origin overflow")
		}
		value, err = checkedAdd(mapped90k, -config.Origin90k)
	case Windows100ns:
		if mapped90k > math.MaxInt64/1000 || mapped90k < math.MinInt64/1000 || config.DelayMilliseconds > math.MaxInt64/90 || config.DelayMilliseconds < math.MinInt64/90 {
			return 0, fmt.Errorf("PGS clock overflow")
		}
		value = mapped90k * 1000 / 9
		if value > math.MaxInt64/9 || value < math.MinInt64/9 {
			return 0, fmt.Errorf("PGS output clock overflow")
		}
		value, err = checkedAdd(value*9/1000, config.DelayMilliseconds*90)
	default:
		return 0, fmt.Errorf("unknown PGS output clock")
	}
	if err != nil {
		return 0, err
	}
	return uint32(value), nil
}

func checkedAdd(a, b int64) (int64, error) {
	if b > 0 && a > math.MaxInt64-b || b < 0 && a < math.MinInt64-b {
		return 0, fmt.Errorf("PGS clock overflow")
	}
	return a + b, nil
}
