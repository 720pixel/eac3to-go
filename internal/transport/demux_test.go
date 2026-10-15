package transport

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestPESPayloadAndTimestamps(t *testing.T) {
	for _, size := range []int{188, 192} {
		t.Run(fmt.Sprint(size), func(t *testing.T) {
			first := makePES(0xbd, (1<<32)+90000, (1<<32)+89900, bytes.Repeat([]byte{0, 0, 1, 0xbd, 0xff, 0x47, 0x99}, 120), false)
			second := makePES(0xbd, NoTimestamp, NoTimestamp, []byte{1, 2, 3}, false)
			packets, cc := packPES(0x1100, first, 0, size, 137)
			tail, _ := packPES(0x1100, second, cc, size, 184)
			var seen []PES
			d, _ := New(Config{Handlers: map[uint16]func(PES) error{0x1100: retain(&seen)}})
			input := append(packets, tail...)
			if err := d.ReadSegment(context.Background(), slowReader{bytes.NewReader(input)}, Segment{Index: 4}); err != nil {
				t.Fatal(err)
			}
			if err := d.Finish(); err != nil {
				t.Fatal(err)
			}
			if len(seen) != 2 || !bytes.Equal(seen[0].Raw, first) || !bytes.Equal(seen[1].Raw, second) {
				t.Fatal("PES bytes changed or packets disappeared")
			}
			if seen[0].PTS != (1<<32)+90000 || seen[0].DTS != (1<<32)+89900 || seen[1].PTS != NoTimestamp || seen[1].DTS != NoTimestamp {
				t.Fatalf("timestamp values or absence changed: %+v", seen)
			}
			if seen[0].ByteOffset != 0 || seen[1].ByteOffset != int64(len(packets)) || seen[0].Segment != 4 {
				t.Fatal("PES source location changed")
			}
			if size == 192 && seen[0].ArrivalTimestamp != 12345 || size == 188 && seen[0].ArrivalTimestamp != NoTimestamp {
				t.Fatal("M2TS arrival timestamp changed")
			}
			stats := d.Stats()
			if stats.Segments != 1 || stats.BytesRead != int64(len(input)) || stats.TransportPackets != int64(len(input)/size) || stats.ByPID[0x1100].EmittedPES != 2 || stats.ByPID[0x1100].PayloadBytes != 843 {
				t.Fatalf("wrong counters: %+v", stats)
			}
			stats.ByPID[0x1100] = PIDStats{}
			if d.Stats().ByPID[0x1100].EmittedPES != 2 {
				t.Fatal("Stats exposed mutable internal state")
			}
			if d.Finish() != nil || !errors.Is(d.ReadSegment(context.Background(), bytes.NewReader(input), Segment{Index: 5}), ErrFinished) {
				t.Fatal("incorrect finished lifecycle")
			}
		})
	}
}

func TestWholePESWindowFiltering(t *testing.T) {
	for _, strict := range []bool{false, true} {
		var input []byte
		cc := byte(0)
		timestamps := []int64{999, NoTimestamp, 1000, NoTimestamp, 1999, 2000, NoTimestamp}
		for i, pts := range timestamps {
			packets, next := packPES(0x101, makePES(0xbd, pts, NoTimestamp, bytes.Repeat([]byte{byte(i)}, 400), false), cc, 188, 184)
			input, cc = append(input, packets...), next
		}
		var seen []PES
		var audits []Audit
		d, _ := New(Config{Strict: strict, Handlers: map[uint16]func(PES) error{0x101: retain(&seen)}, OnAudit: func(a Audit) error { audits = append(audits, a); return nil }})
		if err := d.ReadSegment(context.Background(), bytes.NewReader(input), Segment{Index: 0, HasWindow: true, In90k: 1000, Out90k: 2000}); err != nil {
			t.Fatal(err)
		}
		if err := d.Finish(); err != nil {
			t.Fatal(err)
		}
		want := []byte{0, 1, 2, 3, 4, 5, 6}
		if strict {
			want = []byte{2, 3, 4}
		}
		var got []byte
		for _, pes := range seen {
			if len(pes.Payload) != 400 {
				t.Fatal("strict mode trimmed a PES instead of filtering the whole packet")
			}
			got = append(got, pes.Payload[0])
		}
		if !bytes.Equal(got, want) || len(audits) != 4 {
			t.Fatalf("strict=%v: packets %v, audits %+v", strict, got, audits)
		}
		for _, audit := range audits {
			if audit.Kind != AuditOutsideWindow || audit.Dropped != strict || audit.Bytes != 400 {
				t.Fatalf("unexpected audit: %+v", audit)
			}
		}
		stats := d.Stats().ByPID[0x101]
		if stats.OutsideWindowPES != 4 || strict && (stats.DroppedPES != 4 || stats.DroppedBytes != 1600) || !strict && stats.DroppedPES != 0 {
			t.Fatalf("strict=%v: counters %+v", strict, stats)
		}
	}
}

func TestWindowKeepsFull33BitTimestamp(t *testing.T) {
	packets, _ := packPES(0x100, makePES(0xbd, 1<<32|99, NoTimestamp, []byte{1}, false), 0, 188, 184)
	var seen []PES
	d, _ := New(Config{Strict: true, Handlers: map[uint16]func(PES) error{0x100: retain(&seen)}})
	if err := d.ReadSegment(context.Background(), bytes.NewReader(packets), Segment{Index: 0, HasWindow: true, In90k: 1<<32 | 99, Out90k: 1<<32 | 100}); err != nil {
		t.Fatal(err)
	}
	if err := d.Finish(); err != nil || len(seen) != 1 {
		t.Fatalf("full 33-bit PTS was truncated: %v, %v", seen, err)
	}
}

func TestPlaylistWindowCanSelectVideoWithoutCuttingAudio(t *testing.T) {
	var input []byte
	for _, pid := range []uint16{0x1011, 0x1100} {
		var cc byte
		for _, pts := range []int64{1000, 2000} {
			packets, next := packPES(pid, makePES(0xbd, pts, NoTimestamp, []byte{1}, false), cc, 188, 184)
			cc = next
			input = append(input, packets...)
		}
	}
	var video, audio []PES
	demux, err := New(Config{Handlers: map[uint16]func(PES) error{0x1011: retain(&video), 0x1100: retain(&audio)}, StrictPIDs: map[uint16]bool{0x1011: true}})
	if err != nil {
		t.Fatal(err)
	}
	if err := demux.ReadSegment(context.Background(), bytes.NewReader(input), Segment{Index: 0, HasWindow: true, In90k: 1000, Out90k: 2000}); err != nil {
		t.Fatal(err)
	}
	if err := demux.Finish(); err != nil || len(video) != 1 || len(audio) != 2 || demux.Stats().ByPID[0x1011].DroppedPES != 1 || demux.Stats().ByPID[0x1100].DroppedPES != 0 {
		t.Fatalf("playlist filtering affected the wrong stream: video %d audio %d, %v", len(video), len(audio), err)
	}
}

func TestContinuousPESCrossesSegmentBoundary(t *testing.T) {
	raw := makePES(0xbd, 987654, NoTimestamp, bytes.Repeat([]byte{0x31}, 700), false)
	packets, _ := packPES(0x100, raw, 14, 192, 184)
	var order []string
	var seen []PES
	d, _ := New(Config{Handlers: map[uint16]func(PES) error{0x100: func(p PES) error { order = append(order, "pes"); return retain(&seen)(p) }},
		OnBoundary: func(b Boundary) error {
			if b.PreviousSegment != 0 || b.CurrentSegment != 2 || b.Delta90k != 0 {
				t.Fatalf("wrong boundary: %+v", b)
			}
			order = append(order, fmt.Sprint(b.Before))
			return nil
		}})
	if err := d.ReadSegment(context.Background(), bytes.NewReader(packets[:384]), Segment{Index: 0, PacketSize: 192}); err != nil {
		t.Fatal(err)
	}
	if err := d.ReadSegment(context.Background(), bytes.NewReader(packets[384:]), Segment{Index: 2, PacketSize: 192}); err != nil {
		t.Fatal(err)
	}
	if err := d.Finish(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(order, []string{"true", "false", "pes"}) || len(seen) != 1 || !bytes.Equal(seen[0].Raw, raw) || seen[0].Segment != 0 {
		t.Fatalf("boundary changed partial PES: %v, %+v", order, seen)
	}
}

func TestDiscontinuousBoundaryFlushesBeforeEvents(t *testing.T) {
	var order []string
	d, _ := New(Config{Handlers: map[uint16]func(PES) error{0x100: func(p PES) error { order = append(order, fmt.Sprintf("pes%d", p.Segment)); return nil }},
		OnBoundary: func(b Boundary) error { order = append(order, fmt.Sprint(b.Before)); return nil }})
	for index := 0; index < 2; index++ {
		packets, _ := packPES(0x100, makePES(0xe0, int64(index)*90000, NoTimestamp, []byte{1, 2, 3}, true), 0, 188, 184)
		if err := d.ReadSegment(context.Background(), bytes.NewReader(packets), Segment{Index: index, BoundaryDelta90k: -123}); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Finish(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(order, []string{"pes0", "true", "false", "pes1"}) {
		t.Fatalf("wrong boundary order %v", order)
	}
}

func TestExplicitResetDoesNotInventTimestampDelta(t *testing.T) {
	var seen []PES
	var boundaries []Boundary
	d, _ := New(Config{Handlers: map[uint16]func(PES) error{0x100: retain(&seen)}, OnBoundary: func(b Boundary) error { boundaries = append(boundaries, b); return nil }})
	for i := 0; i < 2; i++ {
		packets, _ := packPES(0x100, makePES(0xe0, 1000, NoTimestamp, []byte{byte(i)}, true), 0, 188, 184)
		if err := d.ReadSegment(context.Background(), bytes.NewReader(packets), Segment{Index: i, Reset: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.Finish(); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || seen[0].PTS != 1000 || seen[1].PTS != 1000 || len(boundaries) != 2 {
		t.Fatalf("reset lost data or changed timestamps: %+v, %+v", seen, boundaries)
	}
	for _, b := range boundaries {
		if b.Delta90k != 0 || !b.Reset {
			t.Fatalf("reset invented delta: %+v", b)
		}
	}
}

func TestTransportRetransmissionIsExplicit(t *testing.T) {
	raw := makePES(0xbd, 90000, NoTimestamp, bytes.Repeat([]byte{1}, 600), false)
	packets, _ := packPES(0x100, raw, 0, 188, 184)
	input := append([]byte{}, packets[:188]...)
	input = append(input, packets[:188]...)
	input = append(input, packets[188:]...)
	for _, allow := range []bool{false, true} {
		var seen []PES
		var audits []Audit
		d, _ := New(Config{AllowRetransmissions: allow, Handlers: map[uint16]func(PES) error{0x100: retain(&seen)}, OnAudit: func(a Audit) error { audits = append(audits, a); return nil }})
		err := d.ReadSegment(context.Background(), bytes.NewReader(input), Segment{Index: 0})
		if !allow {
			if err == nil || !strings.Contains(err.Error(), "continuity") {
				t.Fatalf("unapproved retransmission accepted: %v", err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := d.Finish(); err != nil {
			t.Fatal(err)
		}
		if len(seen) != 1 || !bytes.Equal(seen[0].Raw, raw) || len(audits) != 1 || audits[0].Kind != AuditRetransmission || d.Stats().ByPID[0x100].Retransmissions != 1 {
			t.Fatal("retransmission mishandled")
		}
	}
}

func TestIdenticalPESAndFramesArePreserved(t *testing.T) {
	raw := makePES(0xbd, 90000, NoTimestamp, bytes.Repeat([]byte{0x0b, 0x77, 0x31}, 100), false)
	first, cc := packPES(0x100, raw, 0, 188, 184)
	second, _ := packPES(0x100, raw, cc, 188, 184)
	var seen []PES
	d, _ := New(Config{AllowRetransmissions: true, Handlers: map[uint16]func(PES) error{0x100: retain(&seen)}})
	if err := d.ReadSegment(context.Background(), bytes.NewReader(append(first, second...)), Segment{Index: 0}); err != nil {
		t.Fatal(err)
	}
	if err := d.Finish(); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 2 || !bytes.Equal(seen[0].Raw, seen[1].Raw) || d.Stats().ByPID[0x100].Retransmissions != 0 {
		t.Fatal("ordinary identical PES was removed")
	}
}

func TestAdaptationContinuityAndDiscontinuity(t *testing.T) {
	raw := makePES(0xbd, 1, NoTimestamp, bytes.Repeat([]byte{9}, 400), false)
	packets, _ := packPES(0x100, raw, 0, 188, 184)
	adapt := bytes.Repeat([]byte{0xff}, 188)
	copy(adapt, []byte{0x47, 1, 0, 0x20, 183, 0})
	input := append([]byte{}, packets[:188]...)
	input = append(input, adapt...)
	input = append(input, packets[188:]...)
	var seen []PES
	d, _ := New(Config{Handlers: map[uint16]func(PES) error{0x100: retain(&seen)}})
	if err := d.ReadSegment(context.Background(), bytes.NewReader(input), Segment{Index: 0}); err != nil {
		t.Fatal(err)
	}
	if err := d.Finish(); err != nil || len(seen) != 1 {
		t.Fatalf("adaptation-only counter changed payload: %v", err)
	}

	first, _ := packPES(0x100, makePES(0xbd, 1000, NoTimestamp, []byte{1}, false), 0, 188, 184)
	second, _ := packPES(0x100, makePES(0xbd, 90000, NoTimestamp, []byte{2}, false), 9, 188, 184)
	second[5] |= 0x80
	seen = nil
	var audits []Audit
	d, _ = New(Config{Handlers: map[uint16]func(PES) error{0x100: retain(&seen)}, OnAudit: func(a Audit) error { audits = append(audits, a); return nil }})
	if err := d.ReadSegment(context.Background(), bytes.NewReader(append(first, second...)), Segment{Index: 0}); err != nil {
		t.Fatal(err)
	}
	if err := d.Finish(); err != nil || len(seen) != 2 || !seen[1].Discontinuity || len(audits) != 1 || audits[0].Kind != AuditDiscontinuity {
		t.Fatalf("explicit discontinuity lost: %v, %+v", err, seen)
	}
}

func TestMalformedTransportAndPES(t *testing.T) {
	base, _ := packPES(0x100, makePES(0xbd, 90000, NoTimestamp, []byte{1, 2, 3}, false), 0, 188, 184)
	cases := []struct {
		name   string
		mutate func([]byte) []byte
		text   string
	}{
		{"sync", func(b []byte) []byte { b[0] = 0; return b }, "sync"},
		{"TEI", func(b []byte) []byte { b[1] |= 0x80; return b }, "error indicator"},
		{"scrambled", func(b []byte) []byte { b[3] |= 0x80; return b }, "scrambled"},
		{"reserved adaptation", func(b []byte) []byte { b[3] &= 15; return b }, "reserved"},
		{"large adaptation", func(b []byte) []byte { b[4] = 183; return b }, "length"},
		{"bad stuffing", func(b []byte) []byte { b[6] = 0; return b }, "stuffing"},
		{"missing PES start", func(b []byte) []byte { b[1] &^= 0x40; return b }, "continuation"},
		{"truncated TS", func(b []byte) []byte { return b[:187] }, "incomplete transport"},
		{"bad PES code", func(b []byte) []byte { b[5+int(b[4])+2] = 2; return b }, "start code"},
		{"bad PTS marker", func(b []byte) []byte { b[5+int(b[4])+9] &^= 1; return b }, "timestamp"},
		{"short PES", func(b []byte) []byte { b[5+int(b[4])+5]++; return b }, "truncated PES"},
		{"long PES", func(b []byte) []byte { b[5+int(b[4])+5]--; return b }, "beyond"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, _ := New(Config{Handlers: map[uint16]func(PES) error{0x100: func(PES) error { return nil }}})
			input := tc.mutate(append([]byte(nil), base...))
			err := d.ReadSegment(context.Background(), bytes.NewReader(input), Segment{Index: 3, PacketSize: 188})
			if err == nil {
				err = d.Finish()
			}
			var located *ParseError
			if err == nil || !strings.Contains(err.Error(), tc.text) || !errors.As(err, &located) || located.Segment != 3 {
				t.Fatalf("got %v, want located error containing %q", err, tc.text)
			}
			if d.Finish() != err {
				t.Fatal("failed parser did not remain stopped")
			}
		})
	}
}

func TestPESLimitAndIncompleteBoundary(t *testing.T) {
	for _, unbounded := range []bool{false, true} {
		packets, _ := packPES(0x100, makePES(0xbd, 1, NoTimestamp, bytes.Repeat([]byte{1}, 800), unbounded), 0, 188, 184)
		d, _ := New(Config{MaxPESBytes: 512, Handlers: map[uint16]func(PES) error{0x100: func(PES) error { return nil }}})
		if err := d.ReadSegment(context.Background(), bytes.NewReader(packets), Segment{Index: 0}); err == nil || !strings.Contains(err.Error(), "limit") {
			t.Fatalf("unbounded=%v: ignored size limit: %v", unbounded, err)
		}
	}
	packets, _ := packPES(0x100, makePES(0xbd, 1, NoTimestamp, bytes.Repeat([]byte{1}, 800), false), 0, 188, 184)
	d, _ := New(Config{Handlers: map[uint16]func(PES) error{0x100: func(PES) error { t.Fatal("incomplete PES emitted"); return nil }}})
	if err := d.ReadSegment(context.Background(), bytes.NewReader(packets[:188]), Segment{Index: 0}); err != nil {
		t.Fatal(err)
	}
	if err := d.ReadSegment(context.Background(), bytes.NewReader(packets[188:]), Segment{Index: 1, BoundaryDelta90k: 100}); err == nil || !strings.Contains(err.Error(), "truncated PES") {
		t.Fatalf("incomplete boundary not rejected: %v", err)
	}
}

func TestCallbackFailureCancellationAndConfig(t *testing.T) {
	sentinel := errors.New("output failure")
	packets, _ := packPES(0x100, makePES(0xbd, 1, NoTimestamp, []byte{1}, false), 0, 188, 184)
	d, _ := New(Config{Handlers: map[uint16]func(PES) error{0x100: func(PES) error { return sentinel }}})
	err := d.ReadSegment(context.Background(), bytes.NewReader(packets), Segment{Index: 0})
	if !errors.Is(err, sentinel) || d.Stats().ByPID[0x100].EmittedPES != 0 {
		t.Fatalf("handler failure disappeared: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d, _ = New(Config{Handlers: map[uint16]func(PES) error{0x100: func(PES) error { return nil }}})
	if err := d.ReadSegment(ctx, bytes.NewReader(packets), Segment{Index: 0}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation ignored: %v", err)
	}
	for _, cfg := range []Config{{}, {Handlers: map[uint16]func(PES) error{8191: func(PES) error { return nil }}}, {Handlers: map[uint16]func(PES) error{1: nil}}, {Handlers: map[uint16]func(PES) error{1: func(PES) error { return nil }}, MaxPESBytes: 5}} {
		if _, err := New(cfg); err == nil {
			t.Fatalf("bad config accepted: %+v", cfg)
		}
	}
}

func retain(destination *[]PES) func(PES) error {
	return func(p PES) error {
		p.Raw = append([]byte(nil), p.Raw...)
		p.Payload = append([]byte(nil), p.Payload...)
		*destination = append(*destination, p)
		return nil
	}
}

func makePES(streamID byte, pts, dts int64, payload []byte, unbounded bool) []byte {
	header := []byte{0, 0, 1, streamID, 0, 0, 0x80, 0, 0}
	if pts != NoTimestamp {
		prefix := byte(2)
		if dts != NoTimestamp {
			prefix = 3
		}
		header[7] = prefix << 6
		header = append(header, encodeTimestamp(pts, prefix)...)
		if dts != NoTimestamp {
			header = append(header, encodeTimestamp(dts, 1)...)
		}
	}
	header[8] = byte(len(header) - 9)
	result := append(header, payload...)
	if !unbounded {
		binary.BigEndian.PutUint16(result[4:6], uint16(len(result)-6))
	}
	return result
}

func encodeTimestamp(value int64, prefix byte) []byte {
	return []byte{prefix<<4 | byte(value>>29)&14 | 1, byte(value >> 22), byte(value>>14)&0xfe | 1, byte(value >> 7), byte(value<<1) | 1}
}

func packPES(pid uint16, raw []byte, counter byte, size, maxPayload int) ([]byte, byte) {
	var result []byte
	first := true
	for len(raw) > 0 {
		count := min(len(raw), maxPayload, 184)
		packet := bytes.Repeat([]byte{0xff}, size)
		p := packet[size-188:]
		p[0], p[1], p[2], p[3] = 0x47, byte(pid>>8), byte(pid), 0x10|counter
		if first {
			p[1] |= 0x40
			first = false
		}
		counter = (counter + 1) & 15
		start := 4
		if count < 184 {
			p[3] |= 0x20
			p[4] = byte(183 - count)
			if p[4] != 0 {
				p[5] = 0
			}
			start = 5 + int(p[4])
		}
		copy(p[start:], raw[:count])
		if size == 192 {
			binary.BigEndian.PutUint32(packet[:4], 0xc0000000|12345)
		}
		result, raw = append(result, packet...), raw[count:]
	}
	return result, counter
}

type slowReader struct{ io.Reader }

func (r slowReader) Read(p []byte) (int, error) { return r.Reader.Read(p[:min(len(p), 3)]) }
