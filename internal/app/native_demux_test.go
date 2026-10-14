package app

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"eac3to/internal/aften"
	"eac3to/internal/bitstream"
	"eac3to/internal/bluray"
	"eac3to/internal/lpcm"
	"eac3to/internal/seamless"
)

func TestNativePlaylistCopiesOnlyOneBoundaryDuplicate(t *testing.T) {
	tools := mediaTools(t)
	dir := t.TempDir()
	streamDir := filepath.Join(dir, "BDMV", "STREAM")
	playlistDir := filepath.Join(dir, "BDMV", "PLAYLIST")
	for _, path := range []string{streamDir, playlistDir} {
		if err := os.MkdirAll(path, 0755); err != nil {
			t.Fatal(err)
		}
	}
	audio := filepath.Join(dir, "silence.ac3")
	mediaCommand(t, tools.FFmpeg, "-v", "error", "-f", "lavfi", "-i", "anullsrc=r=48000:cl=stereo", "-t", "0.192", "-c:a", "ac3", "-b:a", "192k", "-f", "ac3", audio)
	payload, err := os.ReadFile(audio)
	if err != nil {
		t.Fatal(err)
	}
	header, err := bitstream.ParseAudioHeader(payload)
	if err != nil {
		t.Fatal(err)
	}
	if len(payload) < 3*header.Size || !bytes.Equal(payload[:header.Size], payload[len(payload)-header.Size:]) {
		t.Fatal("fixture needs identical AC-3 silence frames")
	}
	p := &bluray.Playlist{Path: filepath.Join(playlistDir, "00001.mpls"), Name: "00001.mpls"}
	for i := range 2 {
		clip := fmt.Sprintf("%05d", i+1)
		mediaCommand(t, tools.FFmpeg, "-v", "error", "-i", audio, "-c", "copy", "-streamid", "0:4352", "-mpegts_m2ts_mode", "1", "-f", "mpegts", filepath.Join(streamDir, clip+".m2ts"))
		p.Items = append(p.Items, bluray.PlayItem{ClipID: clip, CodecID: "M2TS", OutTime: 90000, Streams: []bluray.Stream{{EntryType: 1, Kind: "audio", PID: 4352, CodingType: 0x81}}})
	}
	stage := filepath.Join(dir, "output.ac3")
	track := Track{Number: 1, Kind: "audio", Stream: &Stream{Codec: "ac3", Type: "audio", ID: "0x1100"}}
	outputs := []Output{{Track: track, Copy: true, Format: "ac3", Codec: "ac3", Path: stage, Stage: stage}}
	remaining, err := copyNativeStreams(context.Background(), &Source{Playlist: p}, outputs, Options{}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 0 {
		t.Fatal("native output fell through to another demuxer")
	}
	got, err := os.ReadFile(stage)
	if err != nil {
		t.Fatal(err)
	}
	want := append(append([]byte(nil), payload...), payload[header.Size:]...)
	if !bytes.Equal(got, want) {
		t.Fatalf("native payload differs: %d bytes, want %d", len(got), len(want))
	}
	var audit bytes.Buffer
	if err := auditPlaylist(context.Background(), p, Options{JSON: true}, &audit, io.Discard); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(audit.Bytes(), []byte(`"outside_window_packets":0`)) {
		t.Fatal(audit.String())
	}
	firstClip := filepath.Join(streamDir, "00001.m2ts")
	damaged, err := os.ReadFile(firstClip)
	if err != nil {
		t.Fatal(err)
	}
	start := bytes.Index(damaged, payload[:16])
	if start < 0 {
		t.Fatal("fixture has no contiguous audio header")
	}
	damaged[start+16] ^= 1
	if err := os.WriteFile(firstClip, damaged, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := copyNativeStreams(context.Background(), &Source{Playlist: p}, outputs, Options{}, io.Discard); err == nil || !strings.Contains(err.Error(), "CRC") {
		t.Fatalf("normal mode accepted bad audio CRC: %v", err)
	}
	if _, err := copyNativeStreams(context.Background(), &Source{Playlist: p}, outputs, Options{Fast: true}, io.Discard); err != nil {
		t.Fatalf("fast mode failed framing after CRC-only damage: %v", err)
	}
}

func TestCompressedInitialDelayRounding(t *testing.T) {
	data := make([]byte, 1280)
	copy(data, []byte{0x0b, 0x77, 0, 0, 26, 8 << 3, 2 << 5, 0})
	frame := seamless.Frame{Bytes: data, SampleRate: 48000, Samples: 1536}
	for _, tc := range []struct{ delay, frames, residual int64 }{{-28, -1, 4}, {-16, -1, 16}, {-15, 0, -15}, {16, 1, -16}, {48, 2, -16}, {0, 0, 0}} {
		frames, residual, err := compressedDelayFrames("ac3", frame, tc.delay)
		if err != nil || frames != tc.frames || residual != tc.residual {
			t.Fatalf("delay %d: frames=%d residual=%d err=%v", tc.delay, frames, residual, err)
		}
	}
	frame.SampleRate, frame.Samples = 48000, 512
	for _, tc := range []struct{ delay, frames, residual int64 }{{-5, 0, -5}, {-6, -1, 4}, {5, 0, 5}, {6, 1, -4}, {16, 2, -5}, {-16, -2, 5}} {
		frames, residual, err := compressedDelayFrames("dts", frame, tc.delay)
		if err != nil || frames != tc.frames || residual != tc.residual {
			t.Fatalf("DTS delay %d: frames=%d residual=%d err=%v", tc.delay, frames, residual, err)
		}
	}
}

func TestNativePCMDelaySpansPESPackets(t *testing.T) {
	format := lpcm.Format{Rate: 48000, Bits: 16, StorageBits: 16, Channels: 2}
	input := make([]byte, 64*4)
	for i := range input {
		input[i] = byte(i + 1)
	}
	for _, delay := range []int64{-1, 0, 1} {
		var output bytes.Buffer
		writer := bufio.NewWriter(&output)
		track := nativeTrack{ctx: context.Background(), delayMS: delay, outputs: []*nativeOutput{{writer: writer}}}
		for _, block := range [][]byte{input[:128], input[128:]} {
			if err := track.writePCM(lpcm.Result{PCM: block, Frames: len(block) / 4, Format: format}); err != nil {
				t.Fatal(err)
			}
		}
		if err := writer.Flush(); err != nil {
			t.Fatal(err)
		}
		want := input
		if delay < 0 {
			want = input[48*4:]
		} else if delay > 0 {
			want = append(make([]byte, 48*4), input...)
		}
		if !bytes.Equal(output.Bytes(), want) || track.frames != int64(len(want)/4) {
			t.Fatalf("delay %d: got %d bytes / %d frames, want %d bytes", delay, output.Len(), track.frames, len(want))
		}
	}
}

func TestNativeAC3InitialPadding(t *testing.T) {
	if !aften.Available() {
		t.Skip("native silent-frame encoder requires Cgo")
	}
	for _, tc := range []struct {
		name, hash string
		delay      int64
		fallback   bool
	}{
		{"stereo48_192", "325c135d5999e011d0efad8f06857eca09aee1aecd11b14fdd47bb182b5ef837", 32, false},
		{"stereo44_192odd", "", 35, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source, err := os.ReadFile(filepath.Join("..", "aften", "testdata", tc.name+".ac3"))
			if err != nil {
				t.Fatal(err)
			}
			header, err := bitstream.ParseAudioHeader(source)
			if err != nil {
				t.Fatal(err)
			}
			overlap, err := seamless.NewOverlap(4096)
			if err != nil {
				t.Fatal(err)
			}
			var output, log bytes.Buffer
			writer := bufio.NewWriter(&output)
			track := nativeTrack{ctx: context.Background(), log: &log, codec: "ac3", delayMS: tc.delay, overlap: overlap, checkCRC: true, outputs: []*nativeOutput{{writer: writer}}}
			frame := seamless.Frame{Bytes: source, SampleRate: header.SampleRate, Samples: header.Samples, CoreBytes: header.Size}
			for range 2 {
				if err := track.frame(frame); err != nil {
					t.Fatal(err)
				}
			}
			if err := writer.Flush(); err != nil {
				t.Fatal(err)
			}
			data := output.Bytes()
			if track.initialPad != 1 || track.frames != 3 || len(data) != len(source)*3 || !bytes.Equal(data[len(source):], bytes.Repeat(source, 2)) {
				t.Fatalf("unexpected padding/frame count: %+v", track)
			}
			if tc.fallback {
				if !bytes.Equal(data[:len(source)], source) || !strings.Contains(log.String(), "repeating the first frame") {
					t.Fatal("missing source-frame fallback or diagnostic")
				}
			} else if got := fmt.Sprintf("%x", sha256.Sum256(data[:len(source)])); got != tc.hash {
				t.Fatalf("silent frame hash %s, want %s", got, tc.hash)
			}
		})
	}
}

func TestNativeDTSMetadataDoesNotChangeSharedDecodeCache(t *testing.T) {
	tools := mediaTools(t)
	path := filepath.Join(t.TempDir(), "source.dts")
	mediaCommand(t, tools.FFmpeg, "-v", "error", "-f", "lavfi", "-i", "sine=frequency=997:sample_rate=48000", "-t", "0.04", "-ac", "2", "-c:a", "dca", "-strict", "-2", "-b:a", "768k", path)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var frame seamless.Frame
	parser, err := seamless.NewParser(seamless.Config{Codec: seamless.DTS, OnFrame: func(f seamless.Frame) error {
		if frame.Bytes == nil {
			frame = f
			frame.Bytes = bytes.Clone(f.Bytes)
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parser.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := parser.Flush(); err != nil {
		t.Fatal(err)
	}
	if len(frame.Bytes) < 13 || frame.Bytes[4]&2 != 0 || frame.Bytes[11]>>3&15 != 7 {
		t.Fatal("fixture needs a revision-7 DTS core without header CRC")
	}
	frame.Bytes[12] = frame.Bytes[12]&0xf0 | 5
	source := bytes.Clone(frame.Bytes)
	want := bytes.Clone(source)
	want[12] &= 0xf0
	if _, err := bitstream.RepairDTSCoreResolution(want); err != nil {
		t.Fatal(err)
	}
	var kept, removed bytes.Buffer
	keepWriter, removeWriter := bufio.NewWriter(&kept), bufio.NewWriter(&removed)
	track := nativeTrack{ctx: context.Background(), codec: "dts", outputs: []*nativeOutput{
		{output: Output{RemoveDialnorm: true}, writer: removeWriter},
		{output: Output{DecodeCache: true}, writer: keepWriter},
	}}
	for range 2 {
		if err := track.writeFrame(frame); err != nil {
			t.Fatal(err)
		}
	}
	if err := keepWriter.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := removeWriter.Flush(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(frame.Bytes, source) || !bytes.Equal(kept.Bytes(), bytes.Repeat(source, 2)) || !bytes.Equal(removed.Bytes(), bytes.Repeat(want, 2)) {
		t.Fatal("metadata output changed the input or shared decode cache")
	}
}
