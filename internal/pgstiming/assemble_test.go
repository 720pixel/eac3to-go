package pgstiming

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"eac3to/internal/bitstream"
)

func rawSegment(kind byte, payload []byte) []byte {
	b := make([]byte, 3)
	b[0] = kind
	binary.BigEndian.PutUint16(b[1:], uint16(len(payload)))
	return append(b, payload...)
}

func supSegment(kind byte, pts, dts uint32, payload []byte) []byte {
	b := make([]byte, 13)
	copy(b, "PG")
	binary.BigEndian.PutUint32(b[2:], pts)
	binary.BigEndian.PutUint32(b[6:], dts)
	b[10] = kind
	binary.BigEndian.PutUint16(b[11:], uint16(len(payload)))
	return append(b, payload...)
}

func TestSplitHeadersBodiesAndMultipleSegments(t *testing.T) {
	body := []byte{1, 2, 3, 4, 5, 6, 7}
	source := append(rawSegment(0x14, body), rawSegment(0x80, nil)...)
	for _, chunk := range []int{1, 2, 3, 7, 10, 65536} {
		var output bytes.Buffer
		a, err := New(Config{Output: &output, PreserveDTS: true})
		if err != nil {
			t.Fatal(err)
		}
		for offset := 0; offset < len(source); {
			n := min(chunk, len(source)-offset)
			if err = a.WritePES(source[offset:offset+n], Stamp{PTS90k: 1000, DTS90k: 900}); err != nil {
				t.Fatal(err)
			}
			offset += n
		}
		if err = a.Finish(); err != nil {
			t.Fatal(err)
		}
		want := append(supSegment(0x14, 1000, 900, body), supSegment(0x80, 1000, 900, nil)...)
		if !bytes.Equal(output.Bytes(), want) {
			t.Fatalf("chunk %d changed bytes", chunk)
		}
		if a.Stats().Segments != 2 || a.Stats().PayloadBytes != 7 || a.Stats().OutputBytes != int64(len(want)) {
			t.Fatal(a.Stats())
		}
	}
}

func TestSegmentRetainsStartingTimestamp(t *testing.T) {
	var out bytes.Buffer
	a, _ := New(Config{Output: &out})
	first := rawSegment(0x14, []byte{9, 8, 7})
	second := rawSegment(0x80, nil)
	if err := a.WritePES(first[:1], Stamp{PTS90k: 100, DTS90k: 50}); err != nil {
		t.Fatal(err)
	}
	if err := a.WritePES(append(bytes.Clone(first[1:]), second...), Stamp{PTS90k: 200, DTS90k: 150}); err != nil {
		t.Fatal(err)
	}
	if err := a.Finish(); err != nil {
		t.Fatal(err)
	}
	want := append(supSegment(0x14, 100, 0, []byte{9, 8, 7}), supSegment(0x80, 200, 0, nil)...)
	if !bytes.Equal(out.Bytes(), want) {
		t.Fatalf("timestamps changed: %x", out.Bytes())
	}
}

func TestMalformedTruncatedAndMissingTime(t *testing.T) {
	cases := []struct {
		data  []byte
		stamp Stamp
	}{
		{[]byte{0x14}, Stamp{PTS90k: 1}},
		{[]byte{0x14, 0, 3, 1}, Stamp{PTS90k: 1}},
		{[]byte{0x19, 0, 0}, Stamp{PTS90k: 1}},
		{[]byte{0x80, 0, 1, 0}, Stamp{PTS90k: 1}},
		{rawSegment(0x80, nil), Stamp{PTS90k: NoTimestamp}},
	}
	for _, tc := range cases {
		var out bytes.Buffer
		a, _ := New(Config{Output: &out})
		err := a.WritePES(tc.data, tc.stamp)
		if err == nil {
			err = a.Finish()
		}
		if err == nil || out.Len() != 0 {
			t.Fatalf("published invalid segment %x: %v", tc.data, err)
		}
	}
}

type shortWriter struct{}

func (shortWriter) Write(b []byte) (int, error) { return len(b) - 1, nil }

func TestBoundedStorageAndTerminalFailure(t *testing.T) {
	a, _ := New(Config{Output: io.Discard, MaxSegmentBytes: 10})
	if err := a.WritePES([]byte{0x15, 0xff, 0xff}, Stamp{PTS90k: 0}); err == nil {
		t.Fatal("accepted oversized object")
	}
	if len(a.packet) != 23 || a.PendingBytes() != 3 {
		t.Fatal("read oversized body")
	}
	a, _ = New(Config{Output: io.Discard})
	large := bytes.Repeat(rawSegment(0x14, bytes.Repeat([]byte{1}, 65535)), 100)
	if err := a.WritePES(large, Stamp{PTS90k: 0}); err != nil {
		t.Fatal(err)
	}
	if a.PendingBytes() != 0 || len(a.packet) != 65548 || a.Stats().Segments != 100 {
		t.Fatal("unbounded input memory")
	}
	a, _ = New(Config{Output: shortWriter{}})
	err := a.WritePES(rawSegment(0x80, nil), Stamp{PTS90k: 0})
	if !errors.Is(err, io.ErrShortWrite) {
		t.Fatal(err)
	}
	if err = a.WritePES(nil, Stamp{}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal(err)
	}
	a, _ = New(Config{Output: io.Discard})
	if err = a.Finish(); err != nil {
		t.Fatal(err)
	}
	if err = a.WritePES(nil, Stamp{}); !errors.Is(err, ErrFinished) {
		t.Fatal(err)
	}
}

func TestForcedFilterThroughBoundedPipe(t *testing.T) {
	reader, writer := io.Pipe()
	var output bytes.Buffer
	done := make(chan error, 1)
	go func() { _, err := bitstream.FilterForcedPGS(reader, &output); reader.CloseWithError(err); done <- err }()
	a, _ := New(Config{Output: writer})
	for i, forced := range []bool{false, true} {
		pcs := []byte{7, 128, 4, 56, 0x10, 0, 1, 0x80, 0, 0, 1, 0, 1, 0, 0, 0, 50, 0, 60}
		if forced {
			pcs[14] = 0x40
		}
		raw := append(rawSegment(0x16, pcs), rawSegment(0x80, nil)...)
		if err := a.WritePES(raw, Stamp{PTS90k: int64(100 + i*100)}); err != nil {
			writer.CloseWithError(err)
			t.Fatal(err)
		}
	}
	if err := a.Finish(); err != nil {
		writer.CloseWithError(err)
		t.Fatal(err)
	}
	writer.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	data := output.Bytes()
	if binary.BigEndian.Uint32(data[2:6]) != 100 || data[23] != 0 {
		t.Fatal("unforced presentation did not clear")
	}
	second := len(supSegment(0x16, 100, 0, make([]byte, 11))) + 13
	if binary.BigEndian.Uint32(data[second+2:second+6]) != 200 || data[second+23] != 1 {
		t.Fatal("forced presentation or timing changed")
	}
}

func FuzzPGSAssemblyBounded(f *testing.F) {
	f.Add(rawSegment(0x14, []byte{1, 2}))
	f.Add(rawSegment(0x80, nil))
	f.Fuzz(func(t *testing.T, b []byte) {
		a, _ := New(Config{Output: io.Discard, MaxSegmentBytes: 4096})
		for len(b) > 0 {
			n := min(7, len(b))
			err := a.WritePES(b[:n], Stamp{PTS90k: 0})
			b = b[n:]
			if err != nil {
				break
			}
		}
		_ = a.Finish()
		if a.PendingBytes() > 4099 {
			t.Fatal("unbounded assembly")
		}
	})
}
