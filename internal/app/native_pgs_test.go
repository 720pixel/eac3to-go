package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"eac3to/internal/bluray"
	"eac3to/internal/transport"
)

func testPESPacket(pid uint16, cc byte, pts int64, payload []byte) []byte {
	pes := []byte{0, 0, 1, 0xbd, 0, 0, 0x80, 0x80, 5}
	pes = append(pes, byte(pts>>29)&0x0e|0x21, byte(pts>>22), byte(pts>>14)&0xfe|1, byte(pts>>7), byte(pts<<1)|1)
	pes = append(pes, payload...)
	binary.BigEndian.PutUint16(pes[4:6], uint16(len(pes)-6))
	if len(pes) > 182 {
		panic("fixture PES exceeds one packet")
	}
	packet := bytes.Repeat([]byte{0xff}, 188)
	packet[0], packet[1], packet[2], packet[3] = 0x47, 0x40|byte(pid>>8), byte(pid), 0x30|cc
	packet[4], packet[5] = byte(183-len(pes)), 0
	copy(packet[188-len(pes):], pes)
	return packet
}

func TestPGSContinuousBoundaryRetainsActiveMapping(t *testing.T) {
	for _, strict := range []bool{false, true} {
		var output bytes.Buffer
		writer := bufio.NewWriter(&output)
		track := nativeTrack{outputs: []*nativeOutput{{writer: writer}}}
		clips := []clipScan{
			{In90k: 90000, Out90k: 190000, Scan: transport.ScanResult{HasPTS: true, FirstPTS: 90000}},
			{In90k: 180000, Out90k: 280000, Boundary: 0, Scan: transport.ScanResult{HasPTS: true, FirstPTS: 180000}},
			{In90k: 90000, Out90k: 190000, Boundary: -180000, Scan: transport.ScanResult{HasPTS: true, FirstPTS: 90000}},
		}
		if err := track.setupPGS(clips, initialTransportTiming{HasAnchor: true, Anchor: 90000}, Options{StrictPL: strict}); err != nil {
			t.Fatal(err)
		}
		for i, pts := range []int64{101000, 191000, 101000} {
			if err := track.writePGS(transport.PES{Segment: i, PTS: pts, Payload: []byte{0x80, 0, 0}}); err != nil {
				t.Fatal(err)
			}
		}
		if err := track.pgs.Finish(); err != nil {
			t.Fatal(err)
		}
		if err := writer.Flush(); err != nil {
			t.Fatal(err)
		}
		for i, want := range []uint32{10999, 100999, 210999} {
			got := binary.BigEndian.Uint32(output.Bytes()[13*i+2:])
			if got != want {
				t.Fatalf("strict=%t clip=%d PTS=%d, want %d", strict, i, got, want)
			}
		}
	}
}

func TestPGSSourceClockWrapAndUnknownDelay(t *testing.T) {
	for _, tc := range []struct {
		name        string
		starts      []int64
		anchor, pts int64
		want        uint32
	}{
		{"continuous wrap", []int64{(1 << 33) - 90, 45}, (1 << 33) - 90, 135, 286},
		{"unknown delay sentinel", []int64{90}, 90, 180, 180},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			writer := bufio.NewWriter(&output)
			track := nativeTrack{outputs: []*nativeOutput{{writer: writer}}}
			var clips []clipScan
			for _, start := range tc.starts {
				clips = append(clips, clipScan{In90k: start, Out90k: start + 90, Scan: transport.ScanResult{HasPTS: true, FirstPTS: start}})
			}
			if err := track.setupPGS(clips, initialTransportTiming{HasAnchor: true, Anchor: tc.anchor}, Options{}); err != nil {
				t.Fatal(err)
			}
			if err := track.writePGS(transport.PES{Segment: len(clips) - 1, PTS: tc.pts, Payload: []byte{0x80, 0, 0}}); err != nil {
				t.Fatal(err)
			}
			if err := track.pgs.Finish(); err != nil {
				t.Fatal(err)
			}
			if err := writer.Flush(); err != nil {
				t.Fatal(err)
			}
			if got := binary.BigEndian.Uint32(output.Bytes()[2:]); got != tc.want {
				t.Fatalf("PTS=%d, want %d", got, tc.want)
			}
		})
	}
}

func TestNativePGSPlaylistTimingAndForcedObjects(t *testing.T) {
	root := t.TempDir()
	streamDir := filepath.Join(root, "BDMV", "STREAM")
	if err := os.MkdirAll(streamDir, 0755); err != nil {
		t.Fatal(err)
	}
	pl := &bluray.Playlist{Path: filepath.Join(root, "BDMV", "PLAYLIST", "00001.mpls")}
	video := Stream{Index: 0, Codec: "h264", Type: "video", ID: "0x1011"}
	subtitle := Stream{Index: 1, Codec: "hdmv_pgs_subtitle", Type: "subtitle", ID: "0x1200"}
	for i := range 2 {
		start := int64(90000 + i*180000)
		pcs := []byte{0x16, 0, 19, 7, 128, 4, 56, 0x10, 0, 1, 0, 0, 0, 1, 0, 1, 0, byte(i) * 0x40, 0, 50, 0, 60, 0x80, 0, 0}
		var packets []byte
		packets = append(packets, testPESPacket(0x1011, 0, start, []byte{0, 0, 1, 9, 0xf0})...)
		packets = append(packets, testPESPacket(0x1200, 0, start+5000, pcs)...)
		packets = append(packets, testPESPacket(0x1200, 1, start+11000, pcs)...)
		packets = append(packets, testPESPacket(0x1011, 1, start+90000, []byte{0, 0, 1, 9, 0xf0})...)
		clip := fmt.Sprintf("%05d", i+1)
		if err := os.WriteFile(filepath.Join(streamDir, clip+".m2ts"), packets, 0600); err != nil {
			t.Fatal(err)
		}
		pl.Items = append(pl.Items, bluray.PlayItem{ClipID: clip, CodecID: "M2TS", InTime: uint32((start + 9000) / 2), OutTime: uint32((start + 99000) / 2), Streams: []bluray.Stream{{EntryType: 1, PID: 0x1011, Kind: "video"}, {EntryType: 1, PID: 0x1200, Kind: "subtitle"}}})
	}
	source := &Source{Playlist: pl, Probe: Probe{Streams: []Stream{video, subtitle}}}
	for _, strict := range []bool{false, true} {
		for _, forced := range []bool{false, true} {
			t.Run(fmt.Sprintf("strict_%t_forced_%t", strict, forced), func(t *testing.T) {
				stage := filepath.Join(t.TempDir(), "out.sup")
				out := Output{Track: Track{Number: 2, Kind: "subtitle", Stream: &subtitle}, Copy: true, Format: "sup", Codec: subtitle.Codec, Stage: stage, Path: stage, Forced: forced}
				remaining, err := copyNativeStreams(context.Background(), source, []Output{out}, Options{StrictPL: strict}, io.Discard)
				if err != nil || len(remaining) != 0 {
					t.Fatalf("native subtitle extraction: remaining=%d err=%v", len(remaining), err)
				}
				data, err := os.ReadFile(stage)
				if err != nil {
					t.Fatal(err)
				}
				wantPTS := []uint32{4999, 10999, 94999, 100999}
				if strict {
					wantPTS = []uint32{1999, 91999}
				}
				presentation := 0
				for len(data) > 0 {
					if len(data) < 13 || string(data[:2]) != "PG" {
						t.Fatal("invalid SUP header")
					}
					size := int(binary.BigEndian.Uint16(data[11:13]))
					if len(data) < 13+size || binary.BigEndian.Uint32(data[6:10]) != 0 {
						t.Fatal("invalid SUP size or DTS")
					}
					if data[10] == 0x16 {
						if presentation >= len(wantPTS) || binary.BigEndian.Uint32(data[2:6]) != wantPTS[presentation] {
							t.Fatalf("presentation %d has PTS %d", presentation, binary.BigEndian.Uint32(data[2:6]))
						}
						objects := byte(1)
						if forced && presentation < len(wantPTS)/2 {
							objects = 0
						}
						if data[23] != objects {
							t.Fatalf("presentation %d: object count %d, want %d", presentation, data[23], objects)
						}
						presentation++
					}
					data = data[13+size:]
				}
				if presentation != len(wantPTS) {
					t.Fatalf("got %d presentations, want %d", presentation, len(wantPTS))
				}
			})
		}
	}
}
