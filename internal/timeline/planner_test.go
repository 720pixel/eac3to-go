package timeline

import (
	"errors"
	"math"
	"reflect"
	"testing"
)

func observeAt(t *testing.T, p *TimestampPlanner, timestamp, frames int64) {
	t.Helper()
	if err := p.Observe(timestamp, frames); err != nil {
		t.Fatal(err)
	}
}

func TestPlannerPersistenceAndMinimum(t *testing.T) {
	for _, gap := range []int64{-70000, -69999, 69999, 70000} {
		for _, extra := range []int64{0, 1} {
			p, _ := NewTimestampPlanner(PlannerConfig{SampleRate: 10000})
			observeAt(t, p, 900000000, 10000)
			observeAt(t, p, 910000000+gap, 0)
			// Set the decoded end to event time +30s, then one frame later.
			// Only the deliberately off-grid 6.9999ms cases round upward.
			rest := (30*TicksPerSecond+gap+999)/1000 + extra
			observeAt(t, p, NoTimestamp, rest)
			records, err := p.Finish(0)
			if err != nil {
				t.Fatal(err)
			}
			want := (gap == -70000 || gap == 70000) && extra != 0
			if (len(records) != 0) != want {
				t.Fatalf("gap=%d extra=%d records=%v", gap, extra, records)
			}
			if want && records[0] != (Correction{TicksPerSecond + gap, gap}) {
				t.Fatal(records)
			}
		}
	}
}

func TestPlannerJitterRetractionAndSignChange(t *testing.T) {
	for _, sign := range []int64{-1, 1} {
		p, _ := NewTimestampPlanner(PlannerConfig{SampleRate: 48000})
		observeAt(t, p, 0, 48000)
		for _, gap := range []int64{100000, 149999, 200000, 300000, 170000} {
			expected, _ := sampleTime(p.InputFrames(), 48000)
			observeAt(t, p, expected+sign*gap, 48000)
		}
		observeAt(t, p, NoTimestamp, 40*48000)
		records, err := p.Finish(17)
		if err != nil {
			t.Fatal(err)
		}
		want := []Correction{{TicksPerSecond + sign*100000 + 170000, sign * 100000}, {3*TicksPerSecond + sign*200000 + 170000, sign * 170000}}
		if !reflect.DeepEqual(records, want) {
			t.Fatalf("sign %d: got %v want %v", sign, records, want)
		}
	}
	for _, gap := range []int64{0, -100000} {
		p, _ := NewTimestampPlanner(PlannerConfig{SampleRate: 48000})
		observeAt(t, p, 0, 48000)
		observeAt(t, p, TicksPerSecond+200000, 48000)
		observeAt(t, p, 2*TicksPerSecond+gap, 40*48000)
		records, err := p.Finish(0)
		if err != nil {
			t.Fatal(err)
		}
		if gap == 0 && len(records) != 0 {
			t.Fatal(records)
		}
		if gap != 0 && !reflect.DeepEqual(records, []Correction{{2*TicksPerSecond + gap, gap}}) {
			t.Fatal(records)
		}
	}
}

func TestPlannerFiveMillisecondThreshold(t *testing.T) {
	p, _ := NewTimestampPlanner(PlannerConfig{SampleRate: 48000})
	observeAt(t, p, 0, 48000)
	observeAt(t, p, TicksPerSecond+49999, 48000)
	observeAt(t, p, 2*TicksPerSecond+50000, 48000)
	observeAt(t, p, 3*TicksPerSecond+100000, 40*48000)
	got, err := p.Finish(0)
	want := []Correction{{2*TicksPerSecond + 50000, 50000}, {3*TicksPerSecond + 100000, 100000}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("%v %v", got, err)
	}
}

func TestPlannerLateCandidateDoesNotDiscardOldPrefix(t *testing.T) {
	p, _ := NewTimestampPlanner(PlannerConfig{SampleRate: 48000})
	observeAt(t, p, 0, 48000)
	observeAt(t, p, TicksPerSecond+100000, 40*48000)
	observeAt(t, p, 41*TicksPerSecond+200000, 48000)
	got, err := p.Finish(-7)
	if err != nil || !reflect.DeepEqual(got, []Correction{{TicksPerSecond + 30000, 100000}}) {
		t.Fatalf("%v %v", got, err)
	}
}

func TestPlannerFirstTimestampDoesNotInventMissingAudio(t *testing.T) {
	p, _ := NewTimestampPlanner(PlannerConfig{SampleRate: 44100})
	observeAt(t, p, NoTimestamp, 147)
	// This timestamp is a source clock origin, not 20s of missing PCM.
	observeAt(t, p, 20*TicksPerSecond, 147)
	for i := 0; i < 12000; i++ {
		expected, _ := sampleTime(p.InputFrames(), 44100)
		firstTime, _ := sampleTime(147, 44100)
		observeAt(t, p, 20*TicksPerSecond-firstTime+expected, 147)
	}
	got, err := p.Finish(0)
	if err != nil || len(got) != 0 {
		t.Fatalf("%v %v", got, err)
	}
	if p.InputFrames() != 147*12002 {
		t.Fatal(p.InputFrames())
	}
}

func TestPlannerLimitsAndErrors(t *testing.T) {
	for _, c := range []PlannerConfig{{SampleRate: 0}, {SampleRate: 44101}, {SampleRate: 48000, MaxCorrections: -1}, {SampleRate: 48000, MaxCorrections: 1<<20 + 1}} {
		if _, err := NewTimestampPlanner(c); err == nil {
			t.Fatal(c)
		}
	}
	p, _ := NewTimestampPlanner(PlannerConfig{SampleRate: 48000, MaxCorrections: 1})
	observeAt(t, p, 0, 48000)
	observeAt(t, p, TicksPerSecond+100000, 48000)
	if err := p.Observe(2*TicksPerSecond+200000, 0); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
	if _, err := p.Finish(0); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
	if err := p.Observe(0, 0); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
	for _, frames := range []int64{-1, math.MaxInt64} {
		p, _ := NewTimestampPlanner(PlannerConfig{SampleRate: 48000})
		if err := p.Observe(0, frames); err == nil {
			t.Fatal(frames)
		}
	}
	p, _ = NewTimestampPlanner(PlannerConfig{SampleRate: 48000})
	observeAt(t, p, math.MinInt64, 48000)
	if err := p.Observe(math.MaxInt64, 0); err == nil {
		t.Fatal("accepted clock overflow")
	}
	p, _ = NewTimestampPlanner(PlannerConfig{SampleRate: 48000})
	if _, err := p.Finish(0); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Finish(0); !errors.Is(err, ErrFinished) {
		t.Fatal(err)
	}
	if err := p.Observe(0, 0); !errors.Is(err, ErrFinished) {
		t.Fatal(err)
	}
}

func FuzzTimestampPlanner(f *testing.F) {
	f.Add([]byte{0, 8, 8, 16, 32, 8, 0})
	f.Add([]byte{128, 255, 127, 0, 1})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 4096 {
			data = data[:4096]
		}
		p, _ := NewTimestampPlanner(PlannerConfig{SampleRate: 48000, MaxCorrections: 4096})
		observeAt(t, p, 0, 48000)
		for _, v := range data {
			expected, _ := sampleTime(p.InputFrames(), 48000)
			observeAt(t, p, expected+int64(int8(v))*10000, 4800)
		}
		observeAt(t, p, NoTimestamp, 40*48000)
		records, err := p.Finish(0)
		if err != nil {
			t.Fatal(err)
		}
		for i, r := range records {
			if r.Cumulative == 0 {
				t.Fatal("zero surviving correction")
			}
			if i > 0 {
				previous := records[i-1].Cumulative
				if r.Cumulative > 0 && r.Cumulative <= previous || r.Cumulative < 0 && r.Cumulative >= previous {
					t.Fatal(records)
				}
			}
		}
	})
}
