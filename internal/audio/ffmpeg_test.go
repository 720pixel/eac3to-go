package audio

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestQuantizerFFmpegPackingRoundTrip(t *testing.T) {
	ffmpeg, ffprobe := audioMediaTool(t, "ffmpeg"), audioMediaTool(t, "ffprobe")
	cases := []struct {
		depth int
		codec string
		ext   string
	}{
		{8, "pcm_u8", ".wav"},
		{12, "pcm_s16le", ".wav"},
		{16, "flac", ".flac"},
		{20, "pcm_s24le", ".wav"},
		{24, "flac", ".flac"},
		{31, "pcm_s32le", ".wav"},
		{32, "pcm_s32le", ".wav"},
	}
	for _, tc := range cases {
		t.Run(strconv.Itoa(tc.depth)+tc.ext, func(t *testing.T) {
			q, _ := NewQuantizer(tc.depth, true, 123)
			samples, err := q.Process(quantizerFixture(32, 4096))
			if err != nil {
				t.Fatal(err)
			}
			packed := make([]byte, len(samples)*4)
			for i, value := range samples {
				binary.LittleEndian.PutUint32(packed[i*4:], uint32(value))
			}
			output := filepath.Join(t.TempDir(), "output"+tc.ext)
			args := []string{"-v", "error", "-f", "s32le", "-ar", "44100", "-ac", "2", "-i", "pipe:0", "-c:a", tc.codec}
			if tc.codec == "flac" {
				args = append(args, "-sample_fmt", "s32", "-bits_per_raw_sample", strconv.Itoa(tc.depth))
			}
			runAudioMediaTool(t, ffmpeg, packed, append(args, output)...)
			decoded := runAudioMediaTool(t, ffmpeg, nil, "-v", "error", "-xerror", "-err_detect", "crccheck+explode",
				"-i", output, "-c:a", "pcm_s32le", "-f", "s32le", "pipe:1")
			if !bytes.Equal(decoded, packed) {
				t.Fatalf("packing changed samples or frame count: got %d bytes, want %d", len(decoded), len(packed))
			}
			probe := runAudioMediaTool(t, ffprobe, nil, "-v", "error", "-show_entries",
				"stream=sample_rate,channels,duration_ts,time_base", "-of", "json", output)
			var metadata struct {
				Streams []struct {
					SampleRate string `json:"sample_rate"`
					Channels   int    `json:"channels"`
					DurationTS int64  `json:"duration_ts"`
					TimeBase   string `json:"time_base"`
				} `json:"streams"`
			}
			if err := json.Unmarshal(probe, &metadata); err != nil {
				t.Fatal(err)
			}
			if len(metadata.Streams) != 1 || metadata.Streams[0].Channels != 2 || metadata.Streams[0].SampleRate != "44100" ||
				metadata.Streams[0].DurationTS != 2048 || metadata.Streams[0].TimeBase != "1/44100" {
				t.Fatalf("output changed stream layout or timing: %s", probe)
			}
		})
	}
}

func audioMediaTool(t *testing.T, name string) string {
	t.Helper()
	if testing.Short() {
		t.Skip("external media interoperability tests disabled by -short")
	}
	if info, err := os.Stat("/usr/bin/" + name); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
		return "/usr/bin/" + name
	}
	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s is needed for media interoperability tests", name)
	}
	return path
}

func runAudioMediaTool(t *testing.T, tool string, source []byte, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, tool, args...)
	cmd.Stdin = bytes.NewReader(source)
	var output, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %v: %v\n%s", tool, args, err, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("%s reported an error: %s", tool, stderr.String())
	}
	return output.Bytes()
}
