package timeline

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"testing"
)

func TestPCMEditInsideBlockUsesWholeBlockHistory(t *testing.T) {
	p, err := NewPCMEditor(100, 1, []Correction{{11100000, 1000000}}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	var got []float64
	write := func(b []float64) error { got = append(got, b...); return nil }
	first, second := make([]float64, 100), make([]float64, 100)
	for i := range first {
		first[i], second[i] = float64(i), float64(100+i)
	}
	if err = p.Process(first, write); err != nil {
		t.Fatal(err)
	}
	if err = p.Process(second, write); err != nil {
		t.Fatal(err)
	}
	if err = p.Finish(); err != nil {
		t.Fatal(err)
	}
	want := append(append([]float64(nil), first...), second[0])
	want = append(want, second[90:]...)
	want = append(want, second[1:]...)
	if !reflect.DeepEqual(got, want) {
		t.Fatal("current block was not in source history")
	}
	if p.Stats() != (PCMStats{200, 210, 10, 0, 1}) {
		t.Fatal(p.Stats())
	}
}

func TestPCMSilenceClearsHistoryForFollowingEdit(t *testing.T) {
	// The first insertion clears the history. Emitting source samples does
	// not refill it: the next edit in this same block still sees silence.
	p, err := NewPCMEditor(100, 1, []Correction{{1000000, 1000000}, {4000000, 3000000}}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	first, second := make([]float64, 10), make([]float64, 10)
	for i := range first {
		first[i], second[i] = 1, 2
	}
	var got []float64
	write := func(b []float64) error { got = append(got, b...); return nil }
	if err = p.Process(first, write); err != nil {
		t.Fatal(err)
	}
	if err = p.Process(second, write); err != nil {
		t.Fatal(err)
	}
	if err = p.Finish(); err != nil {
		t.Fatal(err)
	}
	// The second edit is at the end of the first input block, so it executes
	// before the second block arrives and sees the already cleared history.
	want := make([]float64, 10)
	want = append(want, first...)
	want = append(want, make([]float64, 20)...)
	want = append(want, second...)
	if !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
}

func TestPCMForcedLoopAndSilenceAndNotifications(t *testing.T) {
	for _, mode := range []string{"default", "silence", "loop"} {
		t.Run(mode, func(t *testing.T) {
			p, err := NewPCMEditor(100, 1, []Correction{{25000000, 15000000}}, mode == "silence", mode == "loop")
			if err != nil {
				t.Fatal(err)
			}
			var got []float64
			var marks []int
			p.OnSplice = func() error { marks = append(marks, len(got)); return nil }
			input := make([]float64, 100)
			for i := range input {
				input[i] = float64(i + 1)
			}
			if err = p.Process(input, func(b []float64) error { got = append(got, b...); return nil }); err != nil {
				t.Fatal(err)
			}
			if err = p.Finish(); err != nil {
				t.Fatal(err)
			}
			want := append([]float64(nil), input...)
			if mode == "loop" {
				want = append(want, input...)
				want = append(want, input[50:]...)
			} else {
				want = append(want, make([]float64, 150)...)
			}
			if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(marks, []int{100, 200, 250}) {
				t.Fatalf("output%v marks%v", got, marks)
			}
		})
	}
}

func TestPCMNegativePositionsAndRegressionsClampToConsumed(t *testing.T) {
	p, err := NewPCMEditor(100, 1, []Correction{{-1000000, 100000}, {0, 200000}}, true, false)
	if err != nil {
		t.Fatal(err)
	}
	var got []float64
	if err = p.Process([]float64{1, 2, 3}, func(b []float64) error { got = append(got, b...); return nil }); err != nil {
		t.Fatal(err)
	}
	if err = p.Finish(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []float64{0, 0, 1, 2, 3}) {
		t.Fatal(got)
	}
}

func TestPCMCumulativeDeltasTruncateIndividually(t *testing.T) {
	p, err := NewPCMEditor(44100, 1, []Correction{{50000, 50000}, {100000, 100000}}, true, false)
	if err != nil {
		t.Fatal(err)
	}
	var frames int
	if err = p.Process([]float64{1}, func(b []float64) error { frames += len(b); return nil }); err != nil {
		t.Fatal(err)
	}
	if err = p.Finish(); err != nil {
		t.Fatal(err)
	}
	// Two 5 ms changes each truncate 220.5 to 220, rather than rounding the
	// aggregate 10 ms to 441. This distinction belongs to correction playback.
	if frames != 441 || p.Stats().InsertedFrames != 440 {
		t.Fatal(frames, p.Stats())
	}
}

func TestPCMErrorsAreTerminal(t *testing.T) {
	failure := errors.New("writer stopped")
	p, _ := NewPCMEditor(48000, 2, nil, false, false)
	if err := p.Process([]float64{1, 2}, func([]float64) error { return failure }); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if err := p.Finish(); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if err := p.Process(nil, func([]float64) error { t.Fatal("write after failure"); return nil }); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	for _, input := range [][]float64{{1}, {math.NaN(), 0}, {math.Inf(1), 0}} {
		p, _ := NewPCMEditor(48000, 2, nil, false, false)
		if err := p.Process(input, func([]float64) error { return nil }); err == nil {
			t.Fatal(input)
		}
		if err := p.Finish(); err == nil {
			t.Fatal("lost error")
		}
	}
	p, _ = NewPCMEditor(48000, 1, []Correction{{0, 1000}}, true, false)
	p.OnSplice = func() error { return failure }
	if err := p.Process([]float64{1}, func([]float64) error { return nil }); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if err := p.Finish(); !errors.Is(err, failure) {
		t.Fatal(err)
	}
	if _, err := NewPCMEditor(100, 1, []Correction{{0, math.MinInt64}, {0, 0}}, true, false); err == nil {
		t.Fatal("overflowing cumulative delta")
	}
	if _, err := NewPCMEditor(100, 1, []Correction{{math.MinInt64, 1}}, true, false); err == nil {
		t.Fatal("overflowing position")
	}
	p, _ = NewPCMEditor(48000, 1, nil, false, false)
	if err := p.Process(nil, nil); err == nil {
		t.Fatal("nil writer")
	}
}

func FuzzPCMEditorAccounting(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5})
	f.Add([]byte{255, 254, 253, 0, 17})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 64 {
			data = data[:64]
		}
		var records []Correction
		var cumulative int64
		for i, v := range data {
			cumulative += int64(int8(v)%5) * 100000
			records = append(records, Correction{int64(i) * 1000000, cumulative})
		}
		p, err := NewPCMEditor(100, 2, records, true, false)
		if err != nil {
			t.Fatal(err)
		}
		var output int64
		write := func(b []float64) error {
			if len(b)%2 != 0 {
				t.Fatal("partial frame")
			}
			output += int64(len(b) / 2)
			return nil
		}
		for i := 0; i < 150; i++ {
			if err = p.Process(make([]float64, 20), write); err != nil {
				t.Fatal(err)
			}
		}
		if err = p.Finish(); err != nil {
			t.Fatal(err)
		}
		s := p.Stats()
		if s.InputFrames != 1500 || s.OutputFrames != output || s.InputFrames+s.InsertedFrames-s.RemovedFrames != output || s.Corrections != len(records) {
			t.Fatal(s)
		}
		if len(p.history) > 200 {
			t.Fatal("unbounded history")
		}
	})
}

func BenchmarkTimestampPlanner(b *testing.B) {
	for _, rate := range []int{44100, 48000} {
		b.Run(fmt.Sprint(rate), func(b *testing.B) {
			p, _ := NewTimestampPlanner(PlannerConfig{SampleRate: rate})
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				stamp, _ := sampleTime(p.InputFrames(), rate)
				if err := p.Observe(stamp, int64(rate/200)); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
