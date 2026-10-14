package app

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"eac3to/internal/bluray"
	"eac3to/internal/ssrc"
	"eac3to/internal/video"
)

func TestNativeVideoLateSPSFailurePreservesDestinations(t *testing.T) {
	tools := mediaTools(t)
	for _, force := range []bool{false, true} {
		name := "new"
		if force {
			name = "force"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			streamDir := filepath.Join(dir, "BDMV", "STREAM")
			if err := os.MkdirAll(streamDir, 0755); err != nil {
				t.Fatal(err)
			}
			clip := filepath.Join(streamDir, "00001.m2ts")
			mediaCommand(t, tools.FFmpeg, "-v", "error", "-i", videoContainerFixture(), "-c", "copy", "-streamid", "0:4113", "-mpegts_m2ts_mode", "1", "-f", "mpegts", clip)
			data, err := os.ReadFile(clip)
			if err != nil {
				t.Fatal(err)
			}
			sps, err := hex.DecodeString("6764000aacd9420db011000003000100000300320f122596")
			if err != nil {
				t.Fatal(err)
			}
			first, last := bytes.Index(data, sps[:4]), bytes.LastIndex(data, sps)
			if first < 0 || last <= first || last < len(data)/3 {
				t.Fatal("fixture needs a complete SPS after earlier video payload")
			}
			data[last+2] |= 1 // Nonzero reserved SPS bit; TS/PES structure stays valid.
			if err := os.WriteFile(clip, data, 0600); err != nil {
				t.Fatal(err)
			}
			p := &bluray.Playlist{Path: filepath.Join(dir, "BDMV", "PLAYLIST", "00001.mpls"), Items: []bluray.PlayItem{{ClipID: "00001", CodecID: "M2TS", OutTime: 90000, Streams: []bluray.Stream{{EntryType: 1, Kind: "video", PID: 4113, CodingType: 0x1b}}}}}
			st := &Stream{Type: "video", Codec: "h264", ID: "0x1011", FrameRate: "25/1"}
			source := &Source{Playlist: p, Paths: []string{clip}, Tracks: []Track{{Number: 1, Kind: "video", Stream: st}}}
			plain, changed := filepath.Join(dir, "plain.h264"), filepath.Join(dir, "changed.h264")
			old := []byte("existing destination must survive a failed timing edit\n")
			args := []string{clip, "1:", plain, "1:", changed, "-changeTo24.000"}
			if force {
				args = append(args, "--force")
				for _, path := range []string{plain, changed} {
					if err := os.WriteFile(path, old, 0600); err != nil {
						t.Fatal(err)
					}
				}
			}
			options, err := ParseOptions(args)
			if err != nil {
				t.Fatal(err)
			}
			outputs, err := buildOutputs(source, options)
			if err != nil {
				t.Fatal(err)
			}
			var log bytes.Buffer
			err = execute(context.Background(), tools, source, outputs, options, io.Discard, &log)
			if err == nil || !strings.Contains(err.Error(), "SPS") || !strings.Contains(log.String(), "native transport reader") {
				t.Fatalf("expected native SPS failure, got %v; log=%s", err, log.String())
			}
			for _, path := range []string{plain, changed} {
				got, err := os.ReadFile(path)
				if force {
					if err != nil || !bytes.Equal(got, old) {
						t.Fatalf("failed edit replaced %s: %q, %v", path, got, err)
					}
				} else if !os.IsNotExist(err) {
					t.Fatalf("failed edit published %s: %v", path, err)
				}
			}
			stages, err := filepath.Glob(filepath.Join(dir, ".eac3to-*"))
			if err != nil || len(stages) != 0 {
				t.Fatalf("failed edit leaked stages: %v, %v", stages, err)
			}
			after, err := os.ReadFile(clip)
			if err != nil || !bytes.Equal(after, data) {
				t.Fatal("timing edit modified source transport bytes")
			}
		})
	}
}

func probeDataHex(data []byte) string {
	var dump strings.Builder
	for i := 0; i < len(data); i += 16 {
		fmt.Fprintf(&dump, "%08x: %x  data\n", i, data[i:min(i+16, len(data))])
	}
	return dump.String()
}

func TestVideoSourceCadenceRejectsUnknownAndConflictingClocks(t *testing.T) {
	sps, _ := hex.DecodeString("6764000aacd9420db011000003000100000300320f122596")
	clock24, _, err := video.RewriteH264SPS(sps, video.H264Clock{NumUnitsInTick: 1000, TimeScale: 48000})
	if err != nil {
		t.Fatal(err)
	}
	annex := func(sets ...[]byte) string {
		var data []byte
		for _, set := range sets {
			data = append(data, 0, 0, 1)
			data = append(data, set...)
		}
		return probeDataHex(data)
	}
	st := Stream{Index: 0, Type: "video", Codec: "h264", FrameRate: "50/1", Extradata: annex(sps)}
	if fps, err := sourceFrameRate(&Source{Probe: Probe{Streams: []Stream{st}}}); err != nil || fps != 25 {
		t.Fatalf("SPS clock did not replace doubled probe estimate: %g, %v", fps, err)
	}
	for name, data := range map[string]string{
		"missing": "", "broken": "not a hexadecimal dump",
		"conflicting":       annex(sps, clock24),
		"field":             func() string { p, _ := hex.DecodeString("6742001ef4106284000003000400000300c810"); return annex(p) }(),
		"picture-structure": func() string { p, _ := hex.DecodeString("6742001ef4106d0800000300080000030190a0"); return annex(p) }(),
	} {
		t.Run(name, func(t *testing.T) {
			bad := st
			bad.Extradata = data
			if _, err := sourceFrameRate(&Source{Probe: Probe{Streams: []Stream{bad}}}); err == nil || !strings.Contains(err.Error(), "extract the audio first") {
				t.Fatalf("accepted uncertain source cadence: %v", err)
			}
		})
	}
	for _, codec := range []string{"hevc", "mpeg2video", "vc1"} {
		bad := st
		bad.Codec = codec
		if _, err := sourceFrameRate(&Source{Probe: Probe{Streams: []Stream{bad}}}); err == nil {
			t.Fatalf("accepted unvalidated %s inference", codec)
		}
	}
	st.Disposition = map[string]int{"attached_pic": 1}
	st.Extradata = ""
	if fps, err := sourceFrameRate(&Source{Probe: Probe{Streams: []Stream{st}}}); err != nil || fps != 0 {
		t.Fatalf("attached picture affected audio speed: %g, %v", fps, err)
	}
}

func TestAudioSpeedIgnoresDoubledTransportProbeRate(t *testing.T) {
	if !ssrc.Available() {
		t.Skip("native resampling requires Cgo")
	}
	tools := mediaTools(t)
	dir := t.TempDir()
	mixed := filepath.Join(dir, "short.m2ts")
	mediaCommand(t, tools.FFmpeg, "-v", "error", "-i", videoContainerFixture(), "-i", filepath.Join("testdata", "delay-stereo48.ac3"), "-map", "0:v:0", "-map", "1:a:0", "-frames:v", "2", "-c", "copy", "-mpegts_m2ts_mode", "1", "-f", "mpegts", mixed)
	inspected, err := probe(context.Background(), tools, []string{"-i", mixed})
	if err != nil {
		t.Fatal(err)
	}
	if len(inspected.Streams) != 2 || inspected.Streams[0].Codec != "h264" || inspected.Streams[1].Codec != "ac3" {
		t.Fatalf("unexpected test streams: %+v", inspected.Streams)
	}
	if inspected.Streams[0].FrameRate != "50/1" {
		t.Logf("this FFprobe reports %s for the short transport fixture", inspected.Streams[0].FrameRate)
	}
	fps, err := sourceFrameRate(&Source{Probe: inspected})
	if err != nil || fps != 25 {
		t.Fatalf("short source cadence: %g, %v", fps, err)
	}
	compressed := filepath.Join(dir, "source.ac3")
	mediaCommand(t, tools.FFmpeg, "-v", "error", "-i", mixed, "-map", "0:a:0", "-c", "copy", compressed)
	decoded := mediaCommand(t, tools.FFmpeg, "-v", "error", "-i", compressed, "-f", "s32le", "-")
	wantFrames := int(math.Round(float64(len(decoded)/8) * 25 / 24))
	baseline := filepath.Join(dir, "baseline.pcm")
	// This compares two routes through the same decoder, independently of which
	// optional matched decoder build is installed.
	if _, err := runTest(t, compressed, baseline, "-25.000", "-changeTo24.000", "-down24", "-no2ndpass", "--allow-unverified"); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(baseline)
	if err != nil || len(want) != wantFrames*6 {
		t.Fatalf("standalone reference produced %d bytes, want %d: %v", len(want), wantFrames*6, err)
	}
	for _, flags := range [][]string{{"-changeTo24.000"}, {"-25.000", "-changeTo24.000"}, {"-50.000", "-changeTo24.000"}} {
		output := filepath.Join(t.TempDir(), "converted.pcm")
		args := append([]string{mixed, "2:", output, "-down24", "-no2ndpass", "--allow-unverified"}, flags...)
		if _, err := runTest(t, args...); err != nil {
			t.Fatalf("%v: %v", flags, err)
		}
		got, err := os.ReadFile(output)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%v: %d bytes, want %d identical PCM bytes: %v", flags, len(got), len(want), err)
		}
	}
}

type cancelAtVideoEOF struct {
	reader *bytes.Reader
	cancel context.CancelFunc
}

func (r cancelAtVideoEOF) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if err == io.EOF {
		r.cancel()
	}
	return n, err
}

func TestVideoCancellationAtEOFFailsBeforeFinalFlush(t *testing.T) {
	data, err := os.ReadFile(videoFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := Output{Codec: "h264", Format: "h264", Request: OutputRequest{Options: AudioOptions{TargetFPS: 24}}}
	err = copyVideoTiming(ctx, cancelAtVideoEOF{bytes.NewReader(data), cancel}, io.Discard, out, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled video edit returned %v", err)
	}
}

func TestFFmpegVideoRateOutputsStayIndependent(t *testing.T) {
	tools := mediaTools(t)
	dir := t.TempDir()
	first, second, plain := filepath.Join(dir, "film24.h264"), filepath.Join(dir, "film23976.h264"), filepath.Join(dir, "plain.h264")
	if _, err := runTest(t, videoContainerFixture(), "1:", first, "-changeTo24.000", "1:", second, "-slowdown", "1:", plain); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(videoFixture(t))
	if err != nil {
		t.Fatal(err)
	}
	decoded := mediaCommand(t, tools.FFmpeg, "-v", "error", "-i", videoFixture(t), "-fps_mode", "passthrough", "-pix_fmt", "yuv420p", "-f", "rawvideo", "-")
	verifyVideoPictures(t, tools, first, original, decoded, 24)
	verifyVideoPictures(t, tools, second, original, decoded, 24000.0/1001)
	verifyVideoPictures(t, tools, plain, original, decoded, 25)
}
