package app

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func mediaTools(t *testing.T) Tools {
	t.Helper()
	tools, e := findTools(Options{})
	if e != nil {
		t.Skip(e)
	}
	return tools
}
func mediaCommand(t *testing.T, tool string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command(tool, args...)
	var errout bytes.Buffer
	cmd.Stderr = &errout
	b, e := cmd.Output()
	if e != nil {
		t.Fatalf("%s %q: %v\n%s", tool, args, e, errout.String())
	}
	return b
}
func runTest(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out, errout bytes.Buffer
	e := Run(context.Background(), args, &out, &errout)
	if e != nil {
		return out.String(), fmt.Errorf("%w\n%s", e, errout.String())
	}
	return out.String(), nil
}
func waveFixture(t *testing.T, dir string) (string, []byte) {
	t.Helper()
	data := make([]byte, 4800*2*2)
	for i := 0; i < len(data)/2; i++ {
		value := int16((i*7919)%60001 - 30000)
		binary.LittleEndian.PutUint16(data[i*2:], uint16(value))
	}
	var w bytes.Buffer
	w.WriteString("RIFF")
	binary.Write(&w, binary.LittleEndian, uint32(36+len(data)))
	w.WriteString("WAVEfmt ")
	binary.Write(&w, binary.LittleEndian, uint32(16))
	binary.Write(&w, binary.LittleEndian, uint16(1))
	binary.Write(&w, binary.LittleEndian, uint16(2))
	binary.Write(&w, binary.LittleEndian, uint32(48000))
	binary.Write(&w, binary.LittleEndian, uint32(192000))
	binary.Write(&w, binary.LittleEndian, uint16(4))
	binary.Write(&w, binary.LittleEndian, uint16(16))
	w.WriteString("data")
	binary.Write(&w, binary.LittleEndian, uint32(len(data)))
	w.Write(data)
	path := filepath.Join(dir, "input's stereo.wav")
	if e := os.WriteFile(path, w.Bytes(), 0600); e != nil {
		t.Fatal(e)
	}
	return path, data
}

func TestLosslessPCMToFLAC(t *testing.T) {
	tools := mediaTools(t)
	d := t.TempDir()
	source, want := waveFixture(t, d)
	out := filepath.Join(d, "lossless.flac")
	if _, e := runTest(t, source, out); e != nil {
		t.Fatal(e)
	}
	got := mediaCommand(t, tools.FFmpeg, "-v", "error", "-i", out, "-f", "s16le", "-c:a", "pcm_s16le", "-")
	if !bytes.Equal(got, want) {
		t.Fatalf("PCM changed: %d bytes vs %d", len(got), len(want))
	}
}

func TestDamagedAudioCannotPublishShortenedOutput(t *testing.T) {
	tools := mediaTools(t)
	dir := t.TempDir()
	source := filepath.Join(dir, "damaged.flac")
	mediaCommand(t, tools.FFmpeg, "-v", "error", "-f", "lavfi", "-i", "sine=frequency=997:sample_rate=48000", "-t", "30", "-ac", "2", "-c:a", "flac", source)
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)/2] ^= 1
	if err := os.WriteFile(source, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := runTest(t, source, "-check"); err == nil {
		t.Fatal("corrupt input passed the integrity check")
	}
	for _, gain := range []bool{false, true} {
		out := filepath.Join(dir, fmt.Sprintf("output-%t.wav", gain))
		args := []string{source, out}
		if gain {
			args = append(args, "+1dB")
		}
		if _, err := runTest(t, args...); err == nil {
			t.Fatal("corrupt input reported success")
		}
		if _, err := os.Stat(out); !os.IsNotExist(err) {
			t.Fatalf("failed conversion published %s: %v", out, err)
		}
	}
	copy := filepath.Join(dir, "copy.flac")
	if _, err := runTest(t, source, copy); err == nil {
		t.Fatal("copy of corrupt input reported success")
	}
	if _, err := os.Stat(copy); !os.IsNotExist(err) {
		t.Fatalf("failed copy published %s: %v", copy, err)
	}
	matches, err := filepath.Glob(filepath.Join(dir, ".eac3to-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("failed conversion leaked staging directories: %v, %v", matches, err)
	}
}

func TestFLACChecksumSurvivesContainersAndCaches(t *testing.T) {
	tools := mediaTools(t)
	dir := t.TempDir()
	wav, want := waveFixture(t, dir)
	flac := filepath.Join(dir, "source.flac")
	mediaCommand(t, tools.FFmpeg, "-v", "error", "-i", wav, "-c:a", "flac", flac)
	container := filepath.Join(dir, "source.mka")
	mediaCommand(t, tools.FFmpeg, "-v", "error", "-i", flac, "-c:a", "copy", container)
	for _, source := range []string{flac, container} {
		packed := source + ".packed.wav"
		processed := source + ".processed.wav"
		if _, err := runTest(t, source, "1:", packed, "1:", processed, "-2pass"); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{packed, processed} {
			got := mediaCommand(t, tools.FFmpeg, "-v", "error", "-i", path, "-f", "s16le", "-")
			if !bytes.Equal(got, want) {
				t.Fatalf("FLAC verification or caching changed PCM in %s", path)
			}
		}
	}
	data, err := os.ReadFile(flac)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) < 42 || string(data[:4]) != "fLaC" {
		t.Fatal("unexpected FLAC fixture")
	}
	data[26] ^= 1 // STREAMINFO MD5; every encoded frame remains intact.
	bad := filepath.Join(dir, "bad-checksum.flac")
	if err := os.WriteFile(bad, data, 0600); err != nil {
		t.Fatal(err)
	}
	badContainer := filepath.Join(dir, "bad-checksum.mka")
	mediaCommand(t, tools.FFmpeg, "-v", "error", "-i", bad, "-c:a", "copy", badContainer)
	for _, source := range []string{bad, badContainer} {
		for _, mode := range []string{"packed", "processed", "copied", "cached"} {
			out := source + "." + mode + ".wav"
			args := []string{source, out}
			switch mode {
			case "processed":
				args = append(args, "+1dB")
			case "copied":
				out = source + ".copy.flac"
				args[1] = out
			case "cached":
				args = []string{source, "1:", out, "1:", out + ".flac"}
			}
			if _, err := runTest(t, args...); err == nil || !strings.Contains(err.Error(), "MD5 mismatch") {
				t.Fatalf("%s did not reject the checksum mismatch for %s: %v", mode, source, err)
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Fatalf("bad checksum published %s: %v", out, err)
			}
		}
	}
}

func TestFLACUnknownMetadataStillValidatesFrames(t *testing.T) {
	tools := mediaTools(t)
	dir := t.TempDir()
	flac := filepath.Join(dir, "unknown.flac")
	mediaCommand(t, tools.FFmpeg, "-v", "error", "-f", "lavfi", "-i", "sine=frequency=997:sample_rate=48000", "-t", "1", "-ac", "2", "-c:a", "flac", flac)
	data, err := os.ReadFile(flac)
	if err != nil {
		t.Fatal(err)
	}
	data[21] &= 0xf0
	clear(data[22:42])
	if err := os.WriteFile(flac, data, 0600); err != nil {
		t.Fatal(err)
	}
	want := mediaCommand(t, tools.FFmpeg, "-v", "error", "-i", flac, "-f", "s16le", "-")
	container := filepath.Join(dir, "unknown.mka")
	mediaCommand(t, tools.FFmpeg, "-v", "error", "-i", flac, "-c:a", "copy", container)
	for _, source := range []string{flac, container} {
		out := source + ".wav"
		copy := source + ".copy.flac"
		if _, err := runTest(t, source, "1:", out, "1:", copy); err != nil {
			t.Fatalf("valid source without optional metadata failed: %v", err)
		}
		for _, path := range []string{out, copy} {
			pcm := mediaCommand(t, tools.FFmpeg, "-v", "error", "-i", path, "-f", "s16le", "-")
			if len(pcm) != 48000*2*2 || !bytes.Equal(pcm, want) {
				t.Fatalf("PCM changed in %s: %d bytes", path, len(pcm))
			}
		}
	}
	containerData, err := os.ReadFile(container)
	if err != nil {
		t.Fatal(err)
	}
	middle := len(data) / 2
	position := bytes.Index(containerData, data[middle-16:middle+16])
	if position < 0 {
		t.Fatal("could not locate the copied FLAC frame in Matroska")
	}
	data[middle] ^= 1
	containerData[position+16] ^= 1
	for _, tc := range []struct {
		name string
		data []byte
	}{{"damaged.flac", data}, {"damaged.mka", containerData}} {
		source := filepath.Join(dir, tc.name)
		if err := os.WriteFile(source, tc.data, 0600); err != nil {
			t.Fatal(err)
		}
		out := source + ".copy.flac"
		if _, err := runTest(t, source, out); err == nil {
			t.Fatalf("corrupt source without optional metadata succeeded: %s", source)
		}
		if _, err := os.Stat(out); !os.IsNotExist(err) {
			t.Fatalf("corrupt source published %s: %v", out, err)
		}
	}
}

func TestFLACVerificationPrecedesHDCDFilter(t *testing.T) {
	tools := mediaTools(t)
	dir := t.TempDir()
	wav, _ := waveFixture(t, dir)
	flac := filepath.Join(dir, "source.flac")
	mediaCommand(t, tools.FFmpeg, "-v", "error", "-i", wav, "-c:a", "flac", flac)
	out := filepath.Join(dir, "decoded.wav")
	if _, err := runTest(t, flac, out, "-decodeHdcd", "--allow-unverified"); err != nil {
		t.Fatal(err)
	}
	data := mediaCommand(t, tools.FFmpeg, "-v", "error", "-i", out, "-c:a", "pcm_s24le", "-f", "s24le", "-")
	if len(data) != 4800*2*3 {
		t.Fatalf("HDCD processing changed frame count: %d bytes", len(data))
	}
}

func TestCheckHonorsSelectedDuration(t *testing.T) {
	tools := mediaTools(t)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	source := &Source{InputArgs: []string{"-f", "lavfi", "-i", "anullsrc=r=48000:cl=stereo"}, Seek: 0.25, Limit: 0.05}
	var log bytes.Buffer
	if err := checkSource(ctx, tools, source, &log); err != nil {
		t.Fatalf("bounded check: %v\n%s", err, &log)
	}
}

func TestExplicitPCMDelayPreservesRemainingSamples(t *testing.T) {
	tools := mediaTools(t)
	for _, rate := range []int{44100, 48000} {
		t.Run(strconv.Itoa(rate), func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, "source.wav")
			mediaCommand(t, tools.FFmpeg, "-v", "error", "-f", "lavfi", "-i", fmt.Sprintf("sine=frequency=997:sample_rate=%d", rate), "-t", "0.1", "-ac", "2", "-c:a", "pcm_s16le", source)
			original := mediaCommand(t, tools.FFmpeg, "-v", "error", "-i", source, "-f", "s16le", "-")
			plus, minus := filepath.Join(dir, "plus.wav"), filepath.Join(dir, "minus.wav")
			if _, err := runTest(t, source, "1:", plus, "+1ms", "1:", minus, "-1ms"); err != nil {
				t.Fatal(err)
			}
			count := rate / 100 / 10 * 4
			for _, tc := range []struct {
				path string
				want []byte
			}{{plus, append(make([]byte, count), original...)}, {minus, original[count:]}} {
				data := mediaCommand(t, tools.FFmpeg, "-v", "error", "-i", tc.path, "-f", "s16le", "-")
				if !bytes.Equal(data, tc.want) {
					t.Fatalf("delay changed samples in %s: got %d bytes, want %d", tc.path, len(data), len(tc.want))
				}
			}
		})
	}
}

func TestFailedOutputLeavesSourceAndNoPartials(t *testing.T) {
	mediaTools(t)
	d := t.TempDir()
	source, _ := waveFixture(t, d)
	original, e := os.ReadFile(source)
	if e != nil {
		t.Fatal(e)
	}
	for _, path := range []string{source, filepath.Join(d, "alias.wav")} {
		if path != source {
			if e := os.Link(source, path); e != nil {
				t.Fatal(e)
			}
		}
		if _, e := runTest(t, source, path, "--force"); e == nil {
			t.Fatalf("allowed overwrite through %s", path)
		}
	}
	after, _ := os.ReadFile(source)
	if !bytes.Equal(after, original) {
		t.Fatal("input was changed")
	}
	out := filepath.Join(d, "invalid-channels.wav")
	if _, e := runTest(t, source, out, "-0,1,2"); e == nil {
		t.Fatal("channel remapping read past the stereo input")
	}
	if _, e := os.Stat(out); !os.IsNotExist(e) {
		t.Fatal("failed conversion published output")
	}
	matches, _ := filepath.Glob(filepath.Join(d, ".eac3to-*"))
	if len(matches) > 0 {
		t.Fatal("temporary files leaked", matches)
	}
}

func TestDryRunDoesNotCreateDirectories(t *testing.T) {
	mediaTools(t)
	d := t.TempDir()
	source, _ := waveFixture(t, d)
	dest := filepath.Join(d, "not-created")
	result, e := runTest(t, source, "out.flac", "-destpath="+dest, "--dry-run")
	if e != nil {
		t.Fatal(e)
	}
	if !strings.Contains(result, "decode/process/encode") {
		t.Fatal(result)
	}
	if _, e := os.Stat(dest); !os.IsNotExist(e) {
		t.Fatal("dry run created output directory")
	}
}

func TestAutomaticClippingRepair(t *testing.T) {
	tools := mediaTools(t)
	d := t.TempDir()
	source, raw := waveFixture(t, d)
	out := filepath.Join(d, "gain.wav")
	if _, e := runTest(t, source, out, "+9dB", "-full"); e != nil {
		t.Fatal(e)
	}
	got := mediaCommand(t, tools.FFmpeg, "-v", "error", "-i", out, "-f", "f64le", "-c:a", "pcm_f64le", "-")
	if len(got) != len(raw)*4 {
		t.Fatalf("sample count changed: %d", len(got))
	}
	peak := 0.0
	for i := 0; i < len(got); i += 8 {
		v := math.Abs(math.Float64frombits(binary.LittleEndian.Uint64(got[i:])))
		peak = math.Max(peak, v)
	}
	if math.Abs(peak-.99) > 1e-12 {
		t.Fatalf("peak %0.17f, want .99", peak)
	}
}

func TestResampleCompleteTailAndReferenceMode(t *testing.T) {
	tools := mediaTools(t)
	d := t.TempDir()
	input := filepath.Join(d, "one second.wav")
	mediaCommand(t, tools.FFmpeg, "-v", "error", "-f", "lavfi", "-i", "sine=frequency=997:sample_rate=48000:duration=1", "-c:a", "pcm_s24le", input)
	for _, tc := range []struct {
		name      string
		reference bool
		frames    int
	}{
		{"complete", false, 24000}, {"reference", true, 22529},
	} {
		path := filepath.Join(d, tc.name+".wav")
		args := []string{input, path, "-resampleTo24000", "-full"}
		if tc.reference {
			args = append(args, "--reference-tail")
		}
		if _, err := runTest(t, args...); err != nil {
			t.Fatal(err)
		}
		pcm := mediaCommand(t, tools.FFmpeg, "-v", "error", "-i", path, "-f", "f64le", "-c:a", "pcm_f64le", "-")
		if len(pcm) != tc.frames*8 {
			t.Fatalf("%s: got %d frames, want %d", tc.name, len(pcm)/8, tc.frames)
		}
	}
}

func TestNoSecondPassOverridesNormalize(t *testing.T) {
	tools := mediaTools(t)
	d := t.TempDir()
	input, _ := waveFixture(t, d)
	path := filepath.Join(d, "unadjusted.wav")
	if _, err := runTest(t, input, path, "+9dB", "-full", "-normalize", "-no2ndpass"); err != nil {
		t.Fatal(err)
	}
	pcm := mediaCommand(t, tools.FFmpeg, "-v", "error", "-i", path, "-c:a", "pcm_f64le", "-f", "f64le", "-")
	peak := 0.0
	for i := 0; i < len(pcm); i += 8 {
		peak = math.Max(peak, math.Abs(math.Float64frombits(binary.LittleEndian.Uint64(pcm[i:]))))
	}
	if peak <= 1 {
		t.Fatalf("-no2ndpass still applied correction: peak=%g", peak)
	}
}

func TestDemuxAndChapters(t *testing.T) {
	tools := mediaTools(t)
	d := t.TempDir()
	wav, _ := waveFixture(t, d)
	srt := filepath.Join(d, "captions.srt")
	os.WriteFile(srt, []byte("1\n00:00:00,000 --> 00:00:00,080\nTest caption\n"), 0600)
	meta := filepath.Join(d, "chapters.meta")
	os.WriteFile(meta, []byte(";FFMETADATA1\n[CHAPTER]\nTIMEBASE=1/1000\nSTART=0\nEND=100\ntitle=Opening\n"), 0600)
	input := filepath.Join(d, "movie.mkv")
	mediaCommand(t, tools.FFmpeg, "-v", "error", "-f", "lavfi", "-i", "color=c=blue:size=32x32:rate=25:duration=0.12", "-i", wav, "-i", srt, "-i", meta, "-map", "0:v", "-map", "1:a", "-map", "2:s", "-map_metadata", "3", "-map_chapters", "3", "-c:v", "ffv1", "-c:a", "flac", "-c:s", "srt", input)
	listing, e := runTest(t, input, "--json")
	if e != nil {
		t.Fatal(e)
	}
	var parsed struct {
		Tracks []Track `json:"tracks"`
	}
	if e = json.Unmarshal([]byte(listing), &parsed); e != nil {
		t.Fatal(e)
	}
	if len(parsed.Tracks) != 4 || parsed.Tracks[0].Kind != "chapters" {
		t.Fatal(listing)
	}
	chapter := filepath.Join(d, "chapters.txt")
	flac := filepath.Join(d, "extracted.flac")
	sub := filepath.Join(d, "extracted.srt")
	if _, e := runTest(t, input, "1:", chapter, "3:", flac, "4:", sub); e != nil {
		t.Fatal(e)
	}
	got, _ := os.ReadFile(chapter)
	if !strings.Contains(string(got), "CHAPTER01=00:00:00.000\nCHAPTER01NAME=Opening") {
		t.Fatal(string(got))
	}
	subdata, _ := os.ReadFile(sub)
	if !strings.Contains(string(subdata), "Test caption") {
		t.Fatal(string(subdata))
	}
	reference := mediaCommand(t, tools.FFmpeg, "-v", "error", "-i", input, "-map", "0:a:0", "-f", "s16le", "-")
	actual := mediaCommand(t, tools.FFmpeg, "-v", "error", "-i", flac, "-f", "s16le", "-")
	if !bytes.Equal(reference, actual) {
		t.Fatal("demux changed PCM")
	}
}

func TestStreamedDialnormOutputs(t *testing.T) {
	tools := mediaTools(t)
	d := t.TempDir()
	source := filepath.Join(d, "source.ac3")
	mediaCommand(t, tools.FFmpeg, "-v", "error", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000:duration=0.2", "-ac", "2", "-c:a", "ac3", "-dialnorm", "-27", source)
	a, b := filepath.Join(d, "a.ac3"), filepath.Join(d, "b.ac3")
	if _, e := runTest(t, source, "1:", a, "1:", b); e != nil {
		t.Fatal(e)
	}
	aBytes, _ := os.ReadFile(a)
	bBytes, _ := os.ReadFile(b)
	sourceBytes, _ := os.ReadFile(source)
	if !bytes.Equal(aBytes, bBytes) || bytes.Equal(aBytes, sourceBytes) {
		t.Fatal("dialnorm edit did not reach both outputs")
	}
	decode := func(p string) []byte {
		return mediaCommand(t, tools.FFmpeg, "-v", "error", "-drc_scale", "0", "-target_level", "0", "-i", p, "-c:a", "pcm_f64le", "-f", "f64le", "-")
	}
	if !bytes.Equal(decode(source), decode(a)) {
		t.Fatal("dialnorm edit changed decoded samples")
	}
}

func TestLossless24BitMultichannel(t *testing.T) {
	tools := mediaTools(t)
	d := t.TempDir()
	raw := make([]byte, 1001*8*3)
	for i := 0; i < len(raw)/3; i++ {
		v := uint32((i * 104729) % 16777216)
		raw[i*3] = byte(v)
		raw[i*3+1] = byte(v >> 8)
		raw[i*3+2] = byte(v >> 16)
	}
	rawPath := filepath.Join(d, "samples.raw")
	if e := os.WriteFile(rawPath, raw, 0600); e != nil {
		t.Fatal(e)
	}
	wav := filepath.Join(d, "channels.wav")
	mediaCommand(t, tools.FFmpeg, "-v", "error", "-f", "s24le", "-ar", "48000", "-ac", "8", "-channel_layout", "7.1", "-i", rawPath, "-c:a", "pcm_s24le", wav)
	flac := filepath.Join(d, "channels.flac")
	if _, e := runTest(t, wav, flac); e != nil {
		t.Fatal(e)
	}
	actual := mediaCommand(t, tools.FFmpeg, "-v", "error", "-i", flac, "-c:a", "pcm_s24le", "-f", "s24le", "-")
	if !bytes.Equal(actual, raw) {
		t.Fatal("24-bit channel samples changed")
	}
}
