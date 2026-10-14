package bitstream

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"testing"
)

func mediaTool(t *testing.T, name string) string {
	t.Helper()
	if testing.Short() {
		t.Skip("external media interoperability tests disabled by -short")
	}
	// Prefer packaged binaries over shell aliases and user wrappers.
	if info, err := os.Stat("/usr/bin/" + name); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
		return "/usr/bin/" + name
	}
	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s is needed for media interoperability tests", name)
	}
	return path
}

func runMediaTool(t *testing.T, tool string, source []byte, args ...string) []byte {
	t.Helper()
	cmd := exec.Command(tool, args...)
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

func TestDialnormFFmpegRoundTrip(t *testing.T) {
	ffmpeg, ffprobe := mediaTool(t, "ffmpeg"), mediaTool(t, "ffprobe")
	cases := []struct {
		codec    string
		rate     int
		channels int
		bitrate  string
	}{
		{"ac3", 48000, 1, "96k"},
		{"ac3", 44100, 2, "192k"},
		{"ac3", 32000, 6, "448k"},
		{"eac3", 48000, 1, "96k"},
		{"eac3", 44100, 2, "192k"},
		{"eac3", 48000, 6, "640k"},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s_%d_%dch", tc.codec, tc.rate, tc.channels), func(t *testing.T) {
			source := runMediaTool(t, ffmpeg, nil, "-v", "error", "-f", "lavfi", "-i",
				fmt.Sprintf("sine=frequency=997:sample_rate=%d:duration=0.25", tc.rate),
				"-ac", strconv.Itoa(tc.channels), "-c:a", tc.codec, "-b:a", tc.bitrate,
				"-dialnorm", "-27", "-threads", "1", "-f", tc.codec, "pipe:1")
			var output bytes.Buffer
			stats, err := RemoveDialnorm(bytes.NewReader(source), &output)
			if err != nil {
				t.Fatal(err)
			}
			if stats.Frames < 5 || stats.Frames != stats.ChangedFrames || stats.Frames != stats.ChangedFields {
				t.Fatalf("unexpected frame stats %+v", stats)
			}
			for offset := 0; offset < len(source); {
				size, enhanced, err := audioFrameSize(source[offset:])
				if err != nil {
					t.Fatal(err)
				}
				original := source[offset : offset+size]
				positions, err := dialnormPositions(original, enhanced)
				if err != nil {
					t.Fatal(err)
				}
				assertAudioIntegrity(t, original, output.Bytes()[offset:offset+size], enhanced, positions)
				offset += size
			}
			// Compare every decoded floating-point sample with metadata gain and
			// DRC disabled, while asking the decoder to fail on CRC errors.
			decodeArgs := []string{"-v", "error", "-xerror", "-err_detect", "crccheck+explode", "-drc_scale", "0",
				"-target_level", "0", "-f", tc.codec, "-i", "pipe:0", "-c:a", "pcm_f32le", "-f", "f32le", "pipe:1"}
			originalPCM := runMediaTool(t, ffmpeg, source, decodeArgs...)
			editedPCM := runMediaTool(t, ffmpeg, output.Bytes(), decodeArgs...)
			if len(originalPCM) == 0 || !bytes.Equal(originalPCM, editedPCM) {
				t.Fatal("decoded samples changed when metadata gain was disabled")
			}
			probe := runMediaTool(t, ffprobe, output.Bytes(), "-v", "error", "-err_detect", "crccheck+explode", "-f", tc.codec,
				"-i", "pipe:0", "-count_frames", "-show_entries", "stream=codec_name,sample_rate,channels,nb_read_frames", "-of", "json")
			var data struct {
				Streams []struct {
					CodecName  string `json:"codec_name"`
					SampleRate string `json:"sample_rate"`
					Channels   int    `json:"channels"`
					Frames     string `json:"nb_read_frames"`
				} `json:"streams"`
			}
			if err := json.Unmarshal(probe, &data); err != nil {
				t.Fatal(err)
			}
			if len(data.Streams) != 1 || data.Streams[0].CodecName != tc.codec || data.Streams[0].SampleRate != strconv.Itoa(tc.rate) || data.Streams[0].Channels != tc.channels || data.Streams[0].Frames != strconv.FormatInt(stats.Frames, 10) {
				t.Fatalf("unexpected ffprobe result: %s", probe)
			}
		})
	}
}

func TestForcedPGSFFprobeDecode(t *testing.T) {
	ffprobe := mediaTool(t, "ffprobe")
	// The first display set defines two bitmaps; the second reuses them,
	// swapping which one is forced. The next two presentations clear them.
	var input []byte
	input = append(input, supPacket(pgsPresentation, 90000,
		presentation(compositionObject(1, 0), compositionObject(2, 0x40)))...)
	input = append(input, supPacket(pgsPalette, 90000, []byte{0, 0, 1, 235, 128, 128, 255})...)
	input = append(input, supPacket(pgsWindow, 90000, []byte{1, 0, 0, 0, 0, 0, 0x07, 0x80, 0x04, 0x38})...)
	input = append(input, supPacket(pgsObject, 90000, tinyObject(1))...)
	input = append(input, supPacket(pgsObject, 90000, tinyObject(2))...)
	input = append(input, endPacket(90000)...)
	input = append(input, supPacket(pgsPresentation, 180000,
		presentation(compositionObject(1, 0x40), compositionObject(2, 0)))...)
	input = append(input, endPacket(180000)...)
	input = append(input, supPacket(pgsPresentation, 270000, presentation(compositionObject(1, 0)))...)
	input = append(input, endPacket(270000)...)
	input = append(input, supPacket(pgsPresentation, 360000, presentation())...)
	input = append(input, endPacket(360000)...)
	var out bytes.Buffer
	if _, err := FilterForcedPGS(bytes.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		label string
		input []byte
		rects []int
	}{
		{"original", input, []int{2, 2, 1, 0}},
		{"filtered", out.Bytes(), []int{1, 1, 0, 0}},
	} {
		probe := runMediaTool(t, ffprobe, tc.input, "-v", "error", "-f", "sup", "-i", "pipe:0", "-show_frames", "-of", "json")
		var data struct {
			Frames []struct {
				NumRects int   `json:"num_rects"`
				PTS      int64 `json:"pts"`
			} `json:"frames"`
		}
		if err := json.Unmarshal(probe, &data); err != nil {
			t.Fatal(err)
		}
		if len(data.Frames) != len(tc.rects) {
			t.Fatalf("%s: wrong subtitle count: %s", tc.label, probe)
		}
		for i, frame := range data.Frames {
			if frame.NumRects != tc.rects[i] || frame.PTS != int64(i+1)*1000000 {
				t.Fatalf("%s subtitle %d: unexpected rectangles/timing: %s", tc.label, i, probe)
			}
		}
	}
}

func TestDTSDialnormFFmpegRoundTrip(t *testing.T) {
	ffmpeg := mediaTool(t, "ffmpeg")
	input := runMediaTool(t, ffmpeg, nil, "-v", "error", "-f", "lavfi", "-i", "sine=frequency=997:sample_rate=48000:duration=0.25", "-ac", "2", "-c:a", "dca", "-strict", "-2", "-b:a", "768k", "-threads", "1", "-f", "dts", "pipe:1")
	var offsets []int
	for offset := 0; offset < len(input); {
		shape, err := dtsFrameBounds(input[offset:])
		if err != nil || shape.extension || shape.size > len(input)-offset {
			t.Fatalf("encoded core header at%d: %+v, %v", offset, shape, err)
		}
		shift := getBitsAt(input[offset:], 38, 1) * 16
		writer := testBits{p: input[offset:], pos: 89 + shift}
		writer.put(4, 7)
		writer.pos = 100 + shift
		writer.put(4, 3)
		offsets = append(offsets, offset)
		offset += shape.size
	}
	var output bytes.Buffer
	stats, err := RemoveDTSDialnorm(bytes.NewReader(input), &output)
	if err != nil {
		t.Fatal(err)
	}
	if stats.CoreFrames < 5 || stats.CoreFrames != int64(len(offsets)) || stats.ChangedFields != stats.CoreFrames {
		t.Fatalf("unexpected DTS counts: %+v", stats)
	}
	for _, offset := range offsets {
		shape, _ := dtsFrameBounds(input[offset:])
		expected := bytes.Clone(input[offset : offset+shape.size])
		shift := getBitsAt(expected, 38, 1) * 16
		writer := testBits{p: expected, pos: 100 + shift}
		writer.put(4, 0)
		if !bytes.Equal(expected, output.Bytes()[offset:offset+shape.size]) {
			t.Fatal("DTS operation changed unrelated frame bytes")
		}
	}
	decode := []string{"-v", "error", "-xerror", "-err_detect", "crccheck+explode", "-f", "dts", "-i", "pipe:0", "-c:a", "pcm_s32le", "-f", "s32le", "pipe:1"}
	before := runMediaTool(t, ffmpeg, input, decode...)
	after := runMediaTool(t, ffmpeg, output.Bytes(), decode...)
	if len(before) == 0 || !bytes.Equal(before, after) {
		t.Fatal("FFmpeg decoded DTS samples changed")
	}
}
