package app

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"

	"eac3to/internal/transport"
)

func writeProbeClip(t *testing.T, name string, packets ...[]byte) clipScan {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	data := bytes.Join(packets, nil)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return clipScan{Clip: name, Path: path, Scan: transport.ScanResult{PacketSize: 188, FileSize: int64(len(data))}}
}

func TestInitialProbeSpansClipsAndTruncatesTimestampsSeparately(t *testing.T) {
	first := writeProbeClip(t, "first.ts",
		testPESPacket(0x1011, 0, 90001, []byte{1}),
		testPESPacket(0x1100, 0, 90100, []byte{2}),
		testPESPacket(0x1012, 0, 90000, []byte{3}),
	)
	second := writeProbeClip(t, "second.ts", testPESPacket(0x1100, 0, 89819, []byte{4}))
	second.Boundary = 1
	source := &Source{Probe: Probe{Streams: []Stream{
		{ID: "0x1011", Type: "video"}, {ID: "0x1100", Type: "audio"}, {ID: "0x1012", Type: "video"},
	}}}
	timing, err := transportInitialTiming(context.Background(), source, []clipScan{first, second})
	if err != nil {
		t.Fatal(err)
	}
	if !timing.HasAnchor || timing.Anchor != 90001 || timing.Delays[0x1100] != -3 {
		t.Fatalf("unexpected initial timing: %+v", timing)
	}
	limited, err := probeTransportTimestamps(context.Background(), []clipScan{first, second}, map[uint16]bool{0x1100: true}, first.Scan.FileSize)
	if err != nil || limited[0x1100] != 90100 {
		t.Fatalf("probe passed its byte limit: minima=%v err=%v", limited, err)
	}
}

func TestInitialSourceDelayUnknownSentinel(t *testing.T) {
	source := &Source{Probe: Probe{Streams: []Stream{{ID: "0x1011", Type: "video"}, {ID: "0x1100", Type: "audio"}}}}
	for _, delay := range []int64{-3, -2, -1, 0, 1, 7} {
		clip := writeProbeClip(t, "offset.ts", testPESPacket(0x1011, 0, 90000, []byte{1}), testPESPacket(0x1100, 0, 90000+90*delay, []byte{2}))
		timing, err := transportInitialTiming(context.Background(), source, []clipScan{clip})
		want := delay
		if delay == -1 {
			want = 0
		}
		if err != nil || timing.Delays[0x1100] != want || timing.Anchor != 90000 {
			t.Fatalf("source offset %d: %+v, %v", delay, timing, err)
		}
	}
}

func TestProbeIgnoresPESIncompleteAtArtificialByteLimit(t *testing.T) {
	partial := testPESPacket(0x1100, 1, 90000, []byte{2})
	start := bytes.Index(partial, []byte{0, 0, 1, 0xbd})
	binary.BigEndian.PutUint16(partial[start+4:start+6], 400)
	clip := writeProbeClip(t, "prefix.ts",
		testPESPacket(0x1100, 0, 91000, []byte{1}), partial,
		testPESPacket(0x1100, 2, 89000, []byte{3}),
	)
	minimum, err := probeTransportTimestamps(context.Background(), []clipScan{clip}, map[uint16]bool{0x1100: true}, 2*188)
	if err != nil || minimum[0x1100] != 91000 {
		t.Fatalf("partial PES counted or rejected: minima=%v err=%v", minimum, err)
	}
}

func TestInitialProbeTrackAppearsAfterBoundary(t *testing.T) {
	source := &Source{Probe: Probe{Streams: []Stream{{ID: "0x1011", Type: "video"}, {ID: "0x1100", Type: "audio"}}}}
	for _, tc := range []struct {
		name                          string
		boundary, first, second, want int64
	}{
		{"negative boundary", -86342, 900000, 900450, 5},
		{"different packet duration", -86342, 900000, 900900, 10},
		{"source offset", -86342, 907200, 907650, 85},
		{"positive boundary", 813658, 1800000, 1800450, 959},
		{"zero boundary", 0, 986342, 986792, 959},
	} {
		t.Run(tc.name, func(t *testing.T) {
			intro := writeProbeClip(t, "intro.ts", testPESPacket(0x1011, 0, 900000, []byte{1}))
			audio := writeProbeClip(t, "audio.ts", testPESPacket(0x1100, 0, tc.first, []byte{2}), testPESPacket(0x1100, 1, tc.second, []byte{3}))
			audio.Boundary = tc.boundary
			timing, err := transportInitialTiming(context.Background(), source, []clipScan{intro, audio})
			if err != nil || timing.Anchor != 900000 || timing.Delays[0x1100] != tc.want {
				t.Fatalf("timing=%+v err=%v", timing, err)
			}
		})
	}
}

func TestInitialProbeAccumulatesOnlyBeforeFirstKnownTimestamp(t *testing.T) {
	intro := writeProbeClip(t, "intro.ts", testPESPacket(0x1011, 0, 900000, []byte{1}))
	unknown := testPESPacket(0x1100, 0, 0, []byte{2})
	start := bytes.Index(unknown, []byte{0, 0, 1, 0xbd})
	unknown[start+7] = 0 // Retain five optional-header stuffing bytes, but no PTS.
	for i := start + 9; i < start+14; i++ {
		unknown[i] = 0xff
	}
	second := writeProbeClip(t, "second.ts", unknown)
	second.Boundary = 813658
	third := writeProbeClip(t, "third.ts", testPESPacket(0x1100, 0, 900000, []byte{3}), testPESPacket(0x1100, 1, 900450, []byte{4}))
	third.Boundary = -986342
	last := writeProbeClip(t, "last.ts", testPESPacket(0x1100, 0, 900900, []byte{5}))
	last.Boundary = 800000
	minimum, err := probeTransportTimestamps(context.Background(), []clipScan{intro, second, third, last}, map[uint16]bool{0x1011: true, 0x1100: true}, referenceProbeBytes)
	if err != nil || minimum[0x1100] != 900450 {
		t.Fatalf("minima=%v err=%v", minimum, err)
	}
}

func TestInitialProbeRejectsStartupClockOverflow(t *testing.T) {
	intro := writeProbeClip(t, "intro.ts", testPESPacket(0x1011, 0, 900000, []byte{1}))
	second := writeProbeClip(t, "second.ts", testPESPacket(0x1011, 0, 900000, []byte{2}))
	second.Boundary = math.MinInt64
	last := writeProbeClip(t, "last.ts", testPESPacket(0x1100, 0, 900000, []byte{3}))
	last.Boundary = -1
	for _, clips := range [][]clipScan{{intro, second, last}, {intro, {Path: last.Path, Clip: last.Clip, Scan: last.Scan, Boundary: math.MinInt64}}} {
		if _, err := probeTransportTimestamps(context.Background(), clips, map[uint16]bool{0x1011: true, 0x1100: true}, referenceProbeBytes); err == nil {
			t.Fatal("accepted overflowing startup timestamps")
		}
	}
}
