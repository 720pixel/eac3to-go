package truehd

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"eac3to/internal/transport"
)

func stamp(v int64) []byte {
	return []byte{0x21 | byte((v>>29)&14), byte(v >> 22), byte((v>>14)&254) | 1, byte(v >> 7), byte(v<<1) | 1}
}

func pes(ext int, pts int64, payload []byte) []byte {
	h := append([]byte{0, 0, 1, 0xfd, 0, 0, 0x80, 0x80, 5}, stamp(pts)...)
	if ext >= 0 {
		h[7], h[8] = 0x81, 8
		h = append(h, 1, 0x81, byte(ext))
	}
	h = append(h, payload...)
	h[4], h[5] = byte((len(h)-6)>>8), byte(len(h)-6)
	return h
}

// Small payload chunks deliberately split the PES extension and timestamps
// across TS packets. Adaptation padding keeps padding out of the codec bytes.
func packets(raw []byte, cc *byte) []byte {
	var result []byte
	first := true
	for len(raw) > 0 {
		n := 7
		if len(raw) < n {
			n = len(raw)
		}
		p := bytes.Repeat([]byte{0xff}, 192)
		copy(p[:4], []byte{0, 0, 0, 0})
		p[4], p[5], p[6], p[7] = 0x47, 0x11, 0, 0x30|*cc
		if first {
			p[5] |= 0x40
		}
		p[8], p[9] = byte(183-n), 0
		copy(p[len(p)-n:], raw[:n])
		result = append(result, p...)
		raw = raw[n:]
		*cc = (*cc + 1) & 15
		first = false
	}
	return result
}

func TestMixedPIDPreservesPayloadMetadataAndWindows(t *testing.T) {
	// These fragments intentionally contain the opposite codec's sync pattern.
	// PES metadata, rather than a sync scan, determines their destination.
	thd := [][]byte{{0x0b, 0x77, 1, 2}, {0xf8, 0x72, 0x6f, 0xba, 9}, {4, 5}}
	ac3 := [][]byte{{0xf8, 0x72, 0x6f, 0xba, 3}, {0x0b, 0x77, 6, 7}}
	var gotTHD, gotAC3 []byte
	var timesTHD, timesAC3 []int64
	collect := func(dst *[]byte, times *[]int64) func(transport.PES) error {
		return func(p transport.PES) error {
			if p.PID != 0x1100 || p.StreamID != 0xfd || p.Segment != 3 || p.DTS != transport.NoTimestamp {
				t.Fatalf("changed PES metadata: %+v", p)
			}
			if len(p.Payload) > 0 && &p.Payload[0] != &p.Raw[len(p.Raw)-len(p.Payload)] {
				t.Fatal("payload was copied or detached from the original PES")
			}
			*dst = append(*dst, p.Payload...)
			*times = append(*times, p.PTS)
			return nil
		}
	}
	s, err := New(Config{PID: 0x1100, OnTrueHD: collect(&gotTHD, &timesTHD), OnAC3: collect(&gotAC3, &timesAC3)})
	if err != nil {
		t.Fatal(err)
	}
	var cc byte
	var input []byte
	for _, row := range []struct {
		ext     int
		pts     int64
		payload []byte
	}{
		{0x72, 900000, thd[0]}, {0x76, 900090, ac3[0]},
		{0x72, 900150, thd[1]}, {0x76, 900180, ac3[1]},
		{0x72, 900300, thd[2]},
	} {
		input = append(input, packets(pes(row.ext, row.pts, row.payload), &cc)...)
	}
	d, err := transport.New(transport.Config{Handlers: map[uint16]func(transport.PES) error{0x1100: s.WritePES}, Strict: true})
	if err != nil {
		t.Fatal(err)
	}
	if err = d.ReadSegment(context.Background(), bytes.NewReader(input), transport.Segment{Index: 3, PacketSize: 192, HasWindow: true, In90k: 900000, Out90k: 900300}); err != nil {
		t.Fatal(err)
	}
	if err = d.Finish(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotTHD, bytes.Join(thd[:2], nil)) || !bytes.Equal(gotAC3, bytes.Join(ac3, nil)) {
		t.Fatalf("payload changed: THD %x AC3 %x", gotTHD, gotAC3)
	}
	if len(timesTHD) != 2 || timesTHD[0] != 900000 || timesTHD[1] != 900150 || len(timesAC3) != 2 || timesAC3[0] != 900090 || timesAC3[1] != 900180 {
		t.Fatal("interleaved timestamps changed")
	}
	if d.Stats().ByPID[0x1100].OutsideWindowPES != 1 {
		t.Fatal("strict out-point was not applied by transport")
	}
	if s.Stats().TrueHD.PayloadBytes != 9 || s.Stats().AC3.PayloadBytes != 9 {
		t.Fatalf("incorrect routing stats: %+v", s.Stats())
	}
}

func TestUnsupportedIDsAndTerminalFailure(t *testing.T) {
	for _, p := range []transport.PES{
		{StreamID: 0xbd, StreamIDExtension: 0x72},
		{StreamID: 0xfd, StreamIDExtension: -1},
		{StreamID: 0xfd, StreamIDExtension: 0x71},
		{StreamID: 0xfd, StreamIDExtension: 0x80},
	} {
		if _, err := Classify(p); err == nil {
			t.Fatalf("accepted unsupported PES %+v", p)
		}
	}
	s, err := New(Config{PID: 0x1100, OnAC3: func(transport.PES) error { return nil }})
	if err != nil {
		t.Fatal(err)
	}
	p := transport.PES{PID: 0x1101, StreamID: 0xfd, StreamIDExtension: 0x76}
	first := s.WritePES(p)
	if first == nil {
		t.Fatal("accepted wrong PID")
	}
	p.PID = 0x1100
	if s.WritePES(p) != first {
		t.Fatal("retried after failure")
	}
}

func TestSelectionAndCallbackFailure(t *testing.T) {
	want := errors.New("writer failed")
	s, err := New(Config{PID: 0x1100, OnAC3: func(transport.PES) error { return want }})
	if err != nil {
		t.Fatal(err)
	}
	p := transport.PES{PID: 0x1100, StreamID: 0xfd, StreamIDExtension: 0x72, Payload: []byte{1, 2, 3}}
	if err = s.WritePES(p); err != nil {
		t.Fatal(err)
	}
	if s.Stats().TrueHD.PESPackets != 1 || s.Stats().TrueHD.EmittedPES != 0 {
		t.Fatal("deselected stream stats")
	}
	p.StreamIDExtension = 0x76
	if s.WritePES(p) != want || s.WritePES(p) != want {
		t.Fatal("callback failure was not terminal")
	}
	if s.Stats().AC3.PESPackets != 1 || s.Stats().AC3.EmittedPES != 0 {
		t.Fatal("failed callback counted as successful")
	}
	if _, err = New(Config{PID: 0x1fff, OnAC3: func(transport.PES) error { return nil }}); err == nil {
		t.Fatal("accepted null PID")
	}
	if _, err = New(Config{PID: 0x1100}); err == nil {
		t.Fatal("accepted empty handlers")
	}
}

func TestTransportRejectsTruncatedPESBeforeRouting(t *testing.T) {
	var calls int
	s, _ := New(Config{PID: 0x1100, OnTrueHD: func(transport.PES) error { calls++; return nil }})
	d, _ := transport.New(transport.Config{Handlers: map[uint16]func(transport.PES) error{0x1100: s.WritePES}})
	raw := pes(0x72, 900000, []byte{1, 2, 3, 4})
	var cc byte
	input := packets(raw[:len(raw)-1], &cc)
	err := d.ReadSegment(context.Background(), bytes.NewReader(input), transport.Segment{Index: 0, PacketSize: 192})
	if err == nil {
		err = d.Finish()
	}
	if err == nil || calls != 0 {
		t.Fatalf("truncated PES delivered: %v, calls %d", err, calls)
	}
}

func TestBorrowedMetadataAndNoPayloadBuffer(t *testing.T) {
	p := transport.PES{PID: 0x1100, StreamID: 0xfd, StreamIDExtension: 0x72,
		PTS: (1 << 33) - 3, DTS: (1 << 33) - 9, Segment: 17,
		ByteOffset: 384, ArrivalTimestamp: 0x3fffffff, Discontinuity: true,
		Payload: make([]byte, 65536), Raw: []byte{9, 8, 7}}
	s, err := New(Config{PID: p.PID, OnTrueHD: func(got transport.PES) error {
		if got.PTS != p.PTS || got.DTS != p.DTS || got.Segment != p.Segment ||
			got.ByteOffset != p.ByteOffset || got.ArrivalTimestamp != p.ArrivalTimestamp ||
			got.Discontinuity != p.Discontinuity || &got.Payload[0] != &p.Payload[0] || &got.Raw[0] != &p.Raw[0] {
			t.Fatal("PES metadata or borrowed storage changed")
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if allocations := testing.AllocsPerRun(1000, func() {
		if err := s.WritePES(p); err != nil {
			t.Fatal(err)
		}
	}); allocations != 0 {
		t.Fatalf("separator allocated payload storage: %v allocations/PES", allocations)
	}
}
