package truehd

import (
	"bytes"
	"errors"
	"testing"
)

func TestWindowsPostprocessKeepsBothMajorUnits(t *testing.T) {
	for _, row := range []struct {
		major []bool
		want  []byte
	}{
		{[]bool{false, true, true, false}, []byte{1, 2, 3}},
		{[]bool{false, true, true, true, false}, []byte{2, 3, 4}},
		{[]bool{true, true, false}, []byte{0, 1, 2}},
		{[]bool{true, false, false, true, false}, []byte{0, 1, 2, 3, 4}},
		{[]bool{true, true}, []byte{0, 1}},
		{[]bool{true}, []byte{0}},
		{nil, nil},
	} {
		for _, enabled := range []bool{false, true} {
			var got []byte
			p, err := NewPostprocessor(PostprocessConfig{WindowsSeamless: enabled, OnFrame: func(f Frame) error {
				if f.Source.Segment != int(f.Bytes[5]) || f.Source.PTS != int64(f.Bytes[5])*90 {
					t.Fatal("buffered metadata changed")
				}
				got = append(got, f.Bytes[5])
				return nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			storage := make([]byte, 6)
			for i, major := range row.major {
				storage[5] = byte(i)
				if err = p.Accept(Frame{Bytes: storage, MajorSync: major, Source: Source{Segment: i, PTS: int64(i) * 90}}); err != nil {
					t.Fatal(err)
				}
				storage[5] = 255
				if p.PendingFrames() > 2 {
					t.Fatal("unbounded frame history")
				}
			}
			if err = p.Finish(); err != nil {
				t.Fatal(err)
			}
			want := row.want
			if !enabled {
				want = make([]byte, len(row.major))
				for i := range want {
					want[i] = byte(i)
				}
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("major flags%v enabled%v got%v want%v", row.major, enabled, got, want)
			}
			stats := p.Stats()
			if stats.InputFrames != int64(len(row.major)) || stats.EmittedFrames != int64(len(want)) || stats.SuppressedFrames != int64(len(row.major)-len(want)) || stats.InputBytes != stats.EmittedBytes+stats.SuppressedBytes {
				t.Fatal(stats)
			}
			if p.PendingFrames() != 0 || p.Finish() != nil {
				t.Fatal("finish did not drain exactly once")
			}
		}
	}
}

func TestPostprocessDoesNotDeleteIdenticalMinorAudio(t *testing.T) {
	var frames int
	p, _ := NewPostprocessor(PostprocessConfig{WindowsSeamless: true, OnFrame: func(Frame) error { frames++; return nil }})
	f := Frame{Bytes: make([]byte, 1024)}
	allocations := testing.AllocsPerRun(1000, func() {
		if err := p.Accept(f); err != nil {
			t.Fatal(err)
		}
	})
	if allocations != 0 {
		t.Fatal("postprocess allocated per access unit")
	}
	if err := p.Finish(); err != nil {
		t.Fatal(err)
	}
	if frames != 1001 || p.Stats().SuppressedFrames != 0 {
		t.Fatal("identical nonmajor audio was deleted")
	}
	if cap(p.storage[0])+cap(p.storage[1]) != 2*MaxAccessUnitBytes {
		t.Fatal("unbounded history storage")
	}
}

func TestPostprocessFailuresAreTerminal(t *testing.T) {
	want := errors.New("output failed")
	for _, enabled := range []bool{false, true} {
		p, _ := NewPostprocessor(PostprocessConfig{WindowsSeamless: enabled, OnFrame: func(Frame) error { return want }})
		frame := Frame{Bytes: make([]byte, 6)}
		var err error
		for i := 0; i < 3; i++ {
			err = p.Accept(frame)
			if err != nil {
				break
			}
		}
		if err != want || p.Finish() != want || p.Accept(frame) != want {
			t.Fatal("failed output retried")
		}
	}
	p, _ := NewPostprocessor(PostprocessConfig{OnFrame: func(Frame) error { return nil }})
	if p.Accept(Frame{Bytes: make([]byte, 5)}) == nil {
		t.Fatal("invalid frame accepted")
	}
	if _, err := NewPostprocessor(PostprocessConfig{OnFrame: func(Frame) error { return nil }, MaxFrameBytes: MaxAccessUnitBytes + 1}); err == nil {
		t.Fatal("invalid bound accepted")
	}
}
