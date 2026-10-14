package transport

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestScanBoundedVideoPreferenceAndPresentationOrder(t *testing.T) {
	for _, size := range []int{188, 192} {
		video := []int64{1000, 1300, 2000, 3000, 4000, 5000, 9000, 8000}
		audio := []int64{0, 100, 500, 600, 1000, 2000, 19900, 20000}
		var input []byte
		for i := range video {
			for _, stream := range []struct {
				pid uint16
				pts int64
			}{{0x1011, video[i]}, {0x1100, audio[i]}} {
				packet, _ := packPES(stream.pid, makePES(0xe0, stream.pts, NoTimestamp, []byte{1}, false), byte(i), size, 184)
				input = append(input, packet...)
			}
		}
		path := scanFixture(t, input)
		cfg := ScanConfig{VideoPIDs: map[uint16]bool{0x1011: true}, WindowBytes: int64(4 * size)}
		got, err := ScanFile(context.Background(), path, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if !got.HasPTS || !got.UsedVideo || got.FirstPTS != 1000 || got.LastPTS != 9000 || got.FullScan || got.PacketSize != size || got.FileSize != int64(len(input)) || got.PacketsScanned != 8 || got.BytesScanned != int64(8*size) || got.TrailingBytes != 0 {
			t.Fatalf("size %d: %+v", size, got)
		}
		if !reflect.DeepEqual(got.Extents, []ScanExtent{{0, int64(4 * size)}, {int64(12 * size), int64(4 * size)}}) {
			t.Fatalf("wrong sampled extents: %+v", got.Extents)
		}
		v := got.ByPID[0x1011]
		if v.Samples != 4 || v.FirstOffset != 0 || v.LastOffset != int64(12*size) || !v.HasFirst || !v.HasLast || v.Wrapped {
			t.Fatalf("tail maximum lost to later B-frame: %+v", v)
		}
		cfg.VideoPIDs = map[uint16]bool{0x1012: true}
		fallback, err := ScanFile(context.Background(), path, cfg)
		if err != nil || !fallback.HasPTS || fallback.UsedVideo || fallback.FirstPTS != 0 || fallback.LastPTS != 20000 {
			t.Fatalf("fallback: %+v, %v", fallback, err)
		}
		cfg.FullScan = true
		full, err := ScanFile(context.Background(), path, cfg)
		if err != nil || !full.FullScan || full.PacketsScanned != 16 || len(full.Extents) != 1 || full.ByPID[0x1011].Samples != 8 {
			t.Fatalf("full scan: %+v, %v", full, err)
		}
	}
}

func TestScanClockWrapAndReorderedTimestamps(t *testing.T) {
	for _, timestamps := range [][]int64{
		{timestampWrap - 30, timestampWrap - 20, timestampWrap - 10, 0, 20, 10},
		{10, timestampWrap - 10, 30},
	} {
		var input []byte
		for i, pts := range timestamps {
			packet, _ := packPES(0x100, makePES(0xe0, pts, NoTimestamp, []byte{1}, false), byte(i), 188, 184)
			input = append(input, packet...)
		}
		path := scanFixture(t, input)
		full, err := ScanFile(context.Background(), path, ScanConfig{FullScan: true})
		if err != nil {
			t.Fatal(err)
		}
		wantFirst, wantLast := timestampWrap-30, timestampWrap+20
		if len(timestamps) == 3 {
			wantFirst, wantLast = timestampWrap-10, timestampWrap+30
		}
		if full.FirstPTS != wantFirst || full.LastPTS != wantLast || !full.ByPID[0x100].Wrapped {
			t.Fatalf("wrap/reordering: %+v", full)
		}
		if len(timestamps) == 6 {
			bounded, err := ScanFile(context.Background(), path, ScanConfig{WindowBytes: 2 * 188})
			if err != nil || bounded.FullScan || bounded.FirstPTS != wantFirst || bounded.LastPTS != wantLast || !bounded.ByPID[0x100].Wrapped {
				t.Fatalf("bounded wrap: %+v, %v", bounded, err)
			}
		}
	}
	var input []byte
	const step = timestampWrap/2 - 1
	for i := 0; i < 6; i++ {
		packet, _ := packPES(0x100, makePES(0xe0, int64(i)*step%timestampWrap, NoTimestamp, []byte{1}, false), byte(i), 188, 184)
		input = append(input, packet...)
	}
	got, err := ScanFile(context.Background(), scanFixture(t, input), ScanConfig{FullScan: true})
	if err != nil || got.FirstPTS != 0 || got.LastPTS != 5*step {
		t.Fatalf("multiple observed wraps: %+v, %v", got, err)
	}
}

func TestScanMissingBoundaryObservationAndSplitHeader(t *testing.T) {
	var input []byte
	for i := 0; i < 6; i++ {
		pid := uint16(0x100)
		if i >= 3 {
			pid = 0x101
		}
		packet, _ := packPES(pid, makePES(0xe0, int64(i)*1000, NoTimestamp, []byte{1}, false), byte(i%3), 188, 184)
		input = append(input, packet...)
	}
	got, err := ScanFile(context.Background(), scanFixture(t, input), ScanConfig{WindowBytes: 188})
	if err != nil || got.HasPTS || !got.ByPID[0x100].HasFirst || got.ByPID[0x100].HasLast || got.ByPID[0x101].HasFirst || !got.ByPID[0x101].HasLast {
		t.Fatalf("missing end observation: %+v, %v", got, err)
	}
	packets, _ := packPES(0x100, makePES(0xe0, 1<<32|123, NoTimestamp, []byte{1}, false), 0, 188, 5)
	path := scanFixture(t, packets)
	full, err := ScanFile(context.Background(), path, ScanConfig{FullScan: true})
	if err != nil || !full.HasPTS || full.FirstPTS != 1<<32|123 || full.ByPID[0x100].Samples != 1 || full.IncompleteHeaders != 0 {
		t.Fatalf("split header: %+v, %v", full, err)
	}
	bounded, err := ScanFile(context.Background(), path, ScanConfig{WindowBytes: 188})
	if err != nil || bounded.HasPTS || bounded.IncompleteHeaders != 1 {
		t.Fatalf("partial sampled header: %+v, %v", bounded, err)
	}
	noPTS, _ := packPES(0x100, makePES(0xe0, NoTimestamp, NoTimestamp, []byte{1}, false), 0, 188, 184)
	got, err = ScanFile(context.Background(), scanFixture(t, noPTS), ScanConfig{})
	if err != nil || got.HasPTS || got.FirstPTS != NoTimestamp || got.LastPTS != NoTimestamp {
		t.Fatalf("invented timestamp: %+v, %v", got, err)
	}
}

func TestScanDiagnosticsAndTrailingBytes(t *testing.T) {
	packet := func(cc byte, pts int64) []byte {
		p, _ := packPES(0x100, makePES(0xe0, pts, NoTimestamp, []byte{1}, false), cc, 188, 184)
		return p
	}
	first := packet(0, 1000)
	input := append(append([]byte{}, first...), first...)
	input = append(input, packet(2, 2000)...)
	bad := packet(3, 3000)
	bad[1] |= 0x80
	input = append(input, bad...)
	bad = packet(4, 4000)
	bad[3] |= 0x80
	input = append(input, bad...)
	marked := packet(5, 5000)
	marked[5] |= 0x80
	input = append(input, marked...)
	input = append(input, []byte{1, 2, 3, 4, 5, 6, 7}...)
	got, err := ScanFile(context.Background(), scanFixture(t, input), ScanConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if !got.FullScan || got.TrailingBytes != 7 || got.PacketsScanned != 6 || got.TransportErrors != 1 || got.ScrambledPackets != 1 || got.ContinuityErrors != 1 || got.Retransmissions != 1 || got.Discontinuities != 1 || got.ByPID[0x100].Samples != 3 || got.FirstPTS != 1000 || got.LastPTS != 5000 {
		t.Fatalf("diagnostics: %+v", got)
	}
	if got.BytesScanned+got.TrailingBytes != got.FileSize {
		t.Fatal("trailing input was hidden")
	}
}

func TestScanRejectsMalformedInputAndCancellation(t *testing.T) {
	packet, _ := packPES(0x100, makePES(0xe0, 1000, NoTimestamp, []byte{1}, false), 0, 188, 184)
	for name, mutate := range map[string]func([]byte){
		"sync":          func(p []byte) { p[0] = 0 },
		"adaptation":    func(p []byte) { p[4] = 184 },
		"PES timestamp": func(p []byte) { p[len(p)-6] &= 0xfe },
	} {
		t.Run(name, func(t *testing.T) {
			bad := append([]byte(nil), packet...)
			mutate(bad)
			if _, err := ScanFile(context.Background(), scanFixture(t, bad), ScanConfig{PacketSize: 188}); err == nil {
				t.Fatal("malformed input accepted")
			}
		})
	}
	for _, cfg := range []ScanConfig{{PacketSize: 204}, {WindowBytes: -1}} {
		if _, err := ScanFile(context.Background(), scanFixture(t, packet), cfg); err == nil {
			t.Fatal("invalid configuration accepted")
		}
	}
	if _, err := ScanFile(context.Background(), scanFixture(t, nil), ScanConfig{}); err == nil {
		t.Fatal("empty input accepted")
	}
	if _, err := ScanFile(context.Background(), t.TempDir(), ScanConfig{}); err == nil {
		t.Fatal("directory accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ScanFile(ctx, scanFixture(t, packet), ScanConfig{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	second, _ := packPES(0x100, makePES(0xe0, 1000+timestampWrap/2, NoTimestamp, []byte{1}, false), 1, 188, 184)
	if _, err := ScanFile(context.Background(), scanFixture(t, append(packet, second...)), ScanConfig{}); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous unwrap: %v", err)
	}
}

func TestTimestampDelta(t *testing.T) {
	for _, tc := range []struct{ current, previous, want int64 }{{10, 0, 10}, {0, 10, -10}, {10, timestampWrap - 10, 20}, {timestampWrap - 10, 10, -20}, {2*timestampWrap + 10, timestampWrap + 20, -10}} {
		got, err := TimestampDelta(tc.current, tc.previous)
		if err != nil || got != tc.want {
			t.Fatalf("%+v: %d, %v", tc, got, err)
		}
	}
	for _, tc := range [][2]int64{{-1, 0}, {0, -1}, {timestampWrap / 2, 0}, {0, timestampWrap / 2}} {
		if _, err := TimestampDelta(tc[0], tc[1]); err == nil {
			t.Fatalf("invalid displacement accepted: %v", tc)
		}
	}
}

func scanFixture(t *testing.T, input []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "input.ts")
	if err := os.WriteFile(path, bytes.Clone(input), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
