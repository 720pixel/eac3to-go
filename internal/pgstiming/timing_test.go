package pgstiming

import (
	"math"
	"testing"
)

func TestLinkedMappingDefaultAndStrict(t *testing.T) {
	timing := ClipTiming{FirstActualStart90k: 1000, CurrentActualStart90k: 3000, PriorPlaylistDuration90k: 9000, In90k: 3500}
	value, err := MapClipTimestamp(4000, timing)
	if err != nil || value != 11000 {
		t.Fatal(value, err)
	}
	timing.Strict = true
	value, err = MapClipTimestamp(4000, timing)
	if err != nil || value != 10500 {
		t.Fatal(value, err)
	}
}

func TestFullSourceClockAndExplicitSUPWrap(t *testing.T) {
	value, err := MapClipTimestamp((1<<32)+100, ClipTiming{})
	if err != nil || value != (1<<32)+100 {
		t.Fatal("lost33rd bit", value, err)
	}
	ticks, err := SUPClock(value, ClockConfig{})
	if err != nil || ticks != 100 {
		t.Fatal(ticks, err)
	}
	value, err = MapClipTimestamp((1<<32)+100, ClipTiming{Clock: WindowsClock32})
	if err != nil || value != 100 {
		t.Fatal(value, err)
	}
	value, err = MapClipTimestamp(100, ClipTiming{SourceCycles: 1, CurrentActualStart90k: (1 << 33) - 100})
	if err != nil || value != 200 {
		t.Fatal(value, err)
	}
	ticks, err = SUPClock(-2, ClockConfig{})
	if err != nil || ticks != math.MaxUint32-1 {
		t.Fatal(ticks, err)
	}
}

func TestWindowsObservedTimestampOrder(t *testing.T) {
	delay, err := WindowsDelayMilliseconds(1048560)
	if err != nil || delay != -11650 {
		t.Fatal(delay, err)
	}
	config := ClockConfig{Mode: Windows100ns, DelayMilliseconds: delay}
	for _, tc := range [][2]int64{{1131142, 82641}, {1131141, 82640}, {1125310, 76809}} {
		value, err := SUPClock(tc[0], config)
		if err != nil || int64(value) != tc[1] {
			t.Fatal(value, err)
		}
	}
	// Delay is added after both integer conversions. Moving it between the
	// conversions changes this negative result by one tick.
	value, err := SUPClock(1, ClockConfig{Mode: Windows100ns, DelayMilliseconds: -1})
	if err != nil || value != uint32(math.MaxUint32-89) {
		t.Fatal(value, err)
	}
}

func TestTimestampInputAndOverflowErrors(t *testing.T) {
	if _, err := MapClipTimestamp(math.MaxUint32, ClipTiming{Clock: WindowsClock32}); err == nil {
		t.Fatal("accepted reference unavailable-timestamp sentinel")
	}
	for _, raw := range []int64{-1, 1 << 33} {
		if _, err := MapClipTimestamp(raw, ClipTiming{}); err == nil {
			t.Fatal(raw)
		}
	}
	if _, err := MapClipTimestamp(0, ClipTiming{SourceCycles: math.MaxInt64}); err == nil {
		t.Fatal("accepted overflow")
	}
	if _, err := MapClipTimestamp(1, ClipTiming{PriorPlaylistDuration90k: math.MaxInt64}); err == nil {
		t.Fatal("accepted addition overflow")
	}
	if _, err := SUPClock(math.MaxInt64, ClockConfig{Mode: Windows100ns}); err == nil {
		t.Fatal("accepted clock overflow")
	}
}
