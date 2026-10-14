package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"

	"eac3to/internal/bluray"
	"eac3to/internal/ssrc"
	"eac3to/internal/video"
)

func videoFixture(t *testing.T) string {
	t.Helper()
	return filepath.Join("..", "video", "testdata", "colour25.h264")
}

func videoContainerFixture() string {
	return filepath.Join("..", "video", "testdata", "colour25.mkv")
}

func annexNALs(data []byte) [][]byte {
	var result [][]byte
	start := -1
	for i := 0; i+2 < len(data); i++ {
		if data[i] == 0 && data[i+1] == 0 && data[i+2] == 1 {
			if start >= 0 {
				result = append(result, bytes.TrimRight(data[start:i], "\x00"))
			}
			start = i + 3
			i += 2
		}
	}
	if start >= 0 {
		result = append(result, bytes.TrimRight(data[start:], "\x00"))
	}
	return result
}

func codedPictures(data []byte) ([][]byte, int) {
	var pictures [][]byte
	aud := 0
	for _, nal := range annexNALs(data) {
		if len(nal) == 0 {
			continue
		}
		kind := nal[0] & 31
		if kind >= 1 && kind <= 5 {
			pictures = append(pictures, nal)
		}
		if kind == 9 {
			aud++
		}
	}
	return pictures, aud
}

func verifyVideoPictures(t *testing.T, tools Tools, path string, wantBytes, wantDecoded []byte, target float64) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	wantNALs, wantAUD := codedPictures(wantBytes)
	gotNALs, gotAUD := codedPictures(data)
	if len(gotNALs) != len(wantNALs) || gotAUD != wantAUD {
		t.Fatalf("picture units/AUDs changed: %d/%d, want %d/%d", len(gotNALs), gotAUD, len(wantNALs), wantAUD)
	}
	for i := range wantNALs {
		if !bytes.Equal(gotNALs[i], wantNALs[i]) {
			t.Fatalf("encoded picture unit %d changed", i)
		}
	}
	spsCount := 0
	for _, nal := range annexNALs(data) {
		if len(nal) == 0 || nal[0]&31 != 7 {
			continue
		}
		sps, err := video.ParseH264SPS(nal)
		if err != nil || math.Abs(sps.Clock.FPS()-target) > 1e-9 {
			t.Fatalf("SPS clock: %+v, %v; want %g", sps, err, target)
		}
		spsCount++
	}
	if spsCount == 0 {
		t.Fatal("missing SPS")
	}
	decoded := mediaCommand(t, tools.FFmpeg, "-v", "error", "-i", path, "-fps_mode", "passthrough", "-pix_fmt", "yuv420p", "-f", "rawvideo", "-")
	if !bytes.Equal(decoded, wantDecoded) {
		t.Fatalf("decoded pictures changed: %d/%d bytes, %x/%x", len(decoded), len(wantDecoded), sha256.Sum256(decoded), sha256.Sum256(wantDecoded))
	}
}

func TestH264FrameRateKeepsEveryPicture(t *testing.T) {
	tools := mediaTools(t)
	input := videoFixture(t)
	original, err := os.ReadFile(input)
	if err != nil {
		t.Fatal(err)
	}
	decoded := mediaCommand(t, tools.FFmpeg, "-v", "error", "-i", input, "-fps_mode", "passthrough", "-pix_fmt", "yuv420p", "-f", "rawvideo", "-")
	if len(decoded) != 50*128*96*3/2 {
		t.Fatalf("fixture decoded to %d bytes", len(decoded))
	}
	dir := t.TempDir()
	container := videoContainerFixture()
	noAUD := filepath.Join(dir, "no-aud.h264")
	var without []byte
	for _, nal := range annexNALs(original) {
		if len(nal) != 0 && nal[0]&31 != 9 {
			without = append(without, 0, 0, 0, 1)
			without = append(without, nal...)
		}
	}
	if err := os.WriteFile(noAUD, without, 0600); err != nil {
		t.Fatal(err)
	}
	for _, source := range []string{input, container, noAUD} {
		want := original
		if source == noAUD {
			want = without
		}
		for _, tc := range []struct {
			flags []string
			fps   float64
		}{
			{[]string{"-changeTo24.000"}, 24},
			{[]string{"-25.000", "-changeTo24.000"}, 24},
			{[]string{"-slowdown"}, 24000.0 / 1001},
			{[]string{"-24.000"}, 25},
		} {
			out := filepath.Join(t.TempDir(), "output.h264")
			if _, err := runTest(t, append([]string{source, out}, tc.flags...)...); err != nil {
				t.Fatalf("%s %v: %v", source, tc.flags, err)
			}
			verifyVideoPictures(t, tools, out, want, decoded, tc.fps)
		}
	}
}

func TestNativeVideoRateOutputsStayIndependent(t *testing.T) {
	tools := mediaTools(t)
	dir := t.TempDir()
	streamDir := filepath.Join(dir, "BDMV", "STREAM")
	if err := os.MkdirAll(streamDir, 0755); err != nil {
		t.Fatal(err)
	}
	input := videoFixture(t)
	clip := filepath.Join(streamDir, "00001.m2ts")
	mediaCommand(t, tools.FFmpeg, "-v", "error", "-i", videoContainerFixture(), "-c", "copy", "-streamid", "0:4113", "-mpegts_m2ts_mode", "1", "-f", "mpegts", clip)
	p := &bluray.Playlist{Path: filepath.Join(dir, "BDMV", "PLAYLIST", "00001.mpls"), Items: []bluray.PlayItem{{ClipID: "00001", CodecID: "M2TS", OutTime: 90000, Streams: []bluray.Stream{{EntryType: 1, Kind: "video", PID: 4113, CodingType: 0x1b}}}}}
	p.Items = append(p.Items, p.Items[0])
	st := &Stream{Type: "video", Codec: "h264", ID: "0x1011", FrameRate: "25/1"}
	s := &Source{Playlist: p, Paths: []string{clip}, Tracks: []Track{{Number: 1, Kind: "video", Stream: st}}}
	first, second := filepath.Join(dir, "film24.h264"), filepath.Join(dir, "film23976.h264")
	o, err := ParseOptions([]string{clip, "1:", first, "-changeTo24.000", "1:", second, "-slowdown"})
	if err != nil {
		t.Fatal(err)
	}
	outputs, err := buildOutputs(s, o)
	if err != nil {
		t.Fatal(err)
	}
	if err := execute(context.Background(), tools, s, outputs, o, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(input)
	if err != nil {
		t.Fatal(err)
	}
	decoded := mediaCommand(t, tools.FFmpeg, "-v", "error", "-i", input, "-fps_mode", "passthrough", "-pix_fmt", "yuv420p", "-f", "rawvideo", "-")
	verifyVideoPictures(t, tools, first, bytes.Repeat(original, 2), bytes.Repeat(decoded, 2), 24)
	verifyVideoPictures(t, tools, second, bytes.Repeat(original, 2), bytes.Repeat(decoded, 2), 24000.0/1001)
}

func TestAudioSpeedUsesRecognizedVideoRate(t *testing.T) {
	if !ssrc.Available() {
		t.Skip("native resampling requires Cgo")
	}
	tools := mediaTools(t)
	dir := t.TempDir()
	input := filepath.Join(dir, "picture-and-sound.mkv")
	mediaCommand(t, tools.FFmpeg, "-v", "error", "-i", videoContainerFixture(), "-i", filepath.Join("testdata", "rate-stereo48.flac"), "-map", "0:v", "-map", "1:a", "-c:v", "copy", "-c:a", "pcm_s16le", input)
	for _, tc := range []struct {
		flags  []string
		frames int
		hash   string
	}{
		{[]string{"-changeTo24.000"}, 100000, "f399fe1423e0316ed34a6670d4dda5d415e611aaed74d1749d2edf11e09b6c90"},
		{[]string{"-24.000", "-changeTo25.000"}, 96000, "6a63dfbaa50bf1066cc42cb7d97c57f6d3dee26f5eb8d599d8542b3cb03fe5b8"},
		{[]string{"-24.000", "-slowdown"}, 100100, "7371f440f96d7bc8813867001b303c5ec579422030762969f1d8f7fe9119c924"},
	} {
		out := filepath.Join(t.TempDir(), "output.pcm")
		args := append([]string{input, "2:", out, "-down24", "-no2ndpass", "--reference-tail"}, tc.flags...)
		if _, err := runTest(t, args...); err != nil {
			t.Fatalf("%v: %v", tc.flags, err)
		}
		data, err := os.ReadFile(out)
		if err != nil || len(data) != tc.frames*6 || fmt.Sprintf("%x", sha256.Sum256(data)) != tc.hash {
			t.Fatalf("%v: %d bytes %x, %v", tc.flags, len(data), sha256.Sum256(data), err)
		}
	}
}

func TestVideoOptionsRejectUnsupportedTimingRoutes(t *testing.T) {
	s := &Source{Tracks: []Track{{Number: 1, Kind: "video", Stream: &Stream{Type: "video", Codec: "h264", FrameRate: "25/1"}}}}
	for _, tc := range []struct {
		output, flag string
	}{
		{"out.h264", "+40ms"}, {"out.h264", "-40ms"},
		{"out.h264", "+3dB"}, {"out.mkv", "-changeTo24.000"},
	} {
		o, err := ParseOptions([]string{"input.mkv", filepath.Join(t.TempDir(), tc.output), tc.flag})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := buildOutputs(s, o); err == nil {
			t.Fatalf("accepted unsupported operation %s %s", tc.output, tc.flag)
		}
	}
}
