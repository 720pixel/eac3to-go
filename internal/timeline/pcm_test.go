package timeline

import (
	"reflect"
	"testing"
)

func TestFramesTruncatesSignedTimes(t *testing.T) {
	for _, tc := range []struct {
		ticks int64
		want  int64
	}{{100, 0}, {-100, 0}, {1000, 4}, {-1000, -4}, {TicksPerSecond, 48000}} {
		got, e := Frames(tc.ticks, 48000)
		if e != nil || got != tc.want {
			t.Fatalf("%d: %d %v", tc.ticks, got, e)
		}
	}
}

func TestPCMHistoryAndCumulativeEdits(t *testing.T) {
	// A 100 Hz fixture makes each sample ten milliseconds.
	p, e := NewPCMEditor(100, 1, []Correction{{Position: 11000000, Cumulative: 1000000}, {Position: 15000000, Cumulative: 0}}, false, false)
	if e != nil {
		t.Fatal(e)
	}
	var got []float64
	write := func(b []float64) error { got = append(got, b...); return nil }
	first := make([]float64, 100)
	for i := range first {
		first[i] = float64(i)
	}
	if e = p.Process(first, write); e != nil {
		t.Fatal(e)
	}
	second := make([]float64, 100)
	for i := range second {
		second[i] = float64(i + 100)
	}
	if e = p.Process(second, write); e != nil {
		t.Fatal(e)
	}
	if e = p.Finish(); e != nil {
		t.Fatal(e)
	}
	want := append([]float64(nil), first...)
	want = append(want, first[90:]...)
	want = append(want, second[:50]...)
	want = append(want, second[60:]...)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("bad correction: %v", got)
	}
}

func TestEarlyPositiveEditUsesSilence(t *testing.T) {
	p, e := NewPCMEditor(100, 1, []Correction{{Position: 200000, Cumulative: 200000}}, false, false)
	if e != nil {
		t.Fatal(e)
	}
	var got []float64
	if e = p.Process([]float64{1, 2, 3}, func(b []float64) error { got = append(got, b...); return nil }); e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(got, []float64{0, 0, 1, 2, 3}) {
		t.Fatal(got)
	}
	if e = p.Finish(); e != nil {
		t.Fatal(e)
	}
}

func TestDeletionCarriesAcrossBlocks(t *testing.T) {
	p, e := NewPCMEditor(100, 1, []Correction{{Position: 0, Cumulative: -500000}}, false, false)
	if e != nil {
		t.Fatal(e)
	}
	var got []float64
	write := func(b []float64) error { got = append(got, b...); return nil }
	for _, b := range [][]float64{{1, 2}, {3, 4}, {5, 6, 7}} {
		if e = p.Process(b, write); e != nil {
			t.Fatal(e)
		}
	}
	if !reflect.DeepEqual(got, []float64{6, 7}) {
		t.Fatal(got)
	}
	if e = p.Finish(); e != nil {
		t.Fatal(e)
	}
}

func TestIncompleteEditFails(t *testing.T) {
	p, e := NewPCMEditor(48000, 2, []Correction{{Position: 0, Cumulative: -TicksPerSecond}}, false, false)
	if e != nil {
		t.Fatal(e)
	}
	if e = p.Process([]float64{1, 2}, func([]float64) error { return nil }); e != nil {
		t.Fatal(e)
	}
	if e = p.Finish(); e == nil {
		t.Fatal("incomplete edit succeeded")
	}
}
