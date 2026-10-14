package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"

	"eac3to/internal/ssrc"
)

func TestFrameRateOptionsAndOrder(t *testing.T) {
	for _, tc := range []struct {
		flags           []string
		from, to, speed float64
	}{
		{[]string{"-slowdown"}, 25, 24000.0 / 1001, 24000.0 / 25025},
		{[]string{"-24.000", "-slowdown"}, 24, 24000.0 / 1001, 1000.0 / 1001},
		{[]string{"-slowdown", "-24.000"}, 24, 24000.0 / 1001, 1000.0 / 1001},
		{[]string{"-speedup"}, 24000.0 / 1001, 25, 25025.0 / 24000},
		{[]string{"-24.000", "-speedup"}, 24, 25, 25.0 / 24},
		{[]string{"-25.000", "-changeTo24.000"}, 25, 24, 24.0 / 25},
		{[]string{"-25.000", "-slowdownTo24.000"}, 25, 24, 24.0 / 25},
		{[]string{"-24.975", "-speedupTo29.970"}, 25000.0 / 1001, 30000.0 / 1001, 30.0 / 25},
	} {
		o, err := ParseOptions(append([]string{"source.wav", "out.wav"}, tc.flags...))
		if err != nil {
			t.Fatal(err)
		}
		a := o.Outputs[0].Options
		if a.SourceFPS != tc.from || a.TargetFPS != tc.to || math.Abs(a.Speed-tc.speed) > 1e-15 {
			t.Fatalf("%v: %+v", tc.flags, a)
		}
	}
	for _, rate := range []string{"23.977", "0.000", "240.001", "24.0001"} {
		if _, err := ParseOptions([]string{"source.wav", "out.wav", "-changeTo" + rate}); err == nil {
			t.Fatalf("accepted unsupported rate %s", rate)
		}
	}
}

func TestSpeedAndDelayOutputLengths(t *testing.T) {
	if !ssrc.Available() {
		t.Skip("native resampling requires Cgo")
	}
	tools := mediaTools(t)
	dir := t.TempDir()
	source, original := waveFixture(t, dir)
	for i, tc := range []struct {
		flags        []string
		frames, rate int
	}{
		{[]string{"-slowdown"}, 5005, 48000},
		{[]string{"-24.000", "-slowdown"}, 4805, 48000},
		{[]string{"-speedup"}, 4603, 48000},
		{[]string{"-24.000", "-speedup"}, 4608, 48000},
		{[]string{"-25.000", "-changeTo24.000"}, 5000, 48000},
		{[]string{"-slowdown", "+100ms"}, 10010, 48000},
		{[]string{"-slowdown", "-6ms"}, 4705, 48000},
		{[]string{"-slowdown", "-resampleTo44100"}, 4598, 44100},
	} {
		out := filepath.Join(dir, fmt.Sprintf("rate%d.wav", i))
		args := append([]string{source, out, "-full", "-no2ndpass"}, tc.flags...)
		if _, err := runTest(t, args...); err != nil {
			t.Fatalf("%v: %v", tc.flags, err)
		}
		got := mediaCommand(t, tools.FFmpeg, "-v", "error", "-i", out, "-c:a", "pcm_f64le", "-f", "f64le", "-")
		if len(got) != tc.frames*2*8 {
			t.Fatalf("%v: %d frames, want %d", tc.flags, len(got)/16, tc.frames)
		}
		metadata, err := probe(context.Background(), tools, []string{"-i", out})
		if err != nil || len(metadata.Streams) != 1 || metadata.Streams[0].SampleRate != fmt.Sprint(tc.rate) {
			t.Fatalf("%v: output rate changed: %+v, %v", tc.flags, metadata, err)
		}
	}
	for _, flags := range [][]string{{"-25.000"}, {"-24.000", "-changeTo24.000"}} {
		out := filepath.Join(t.TempDir(), "identity.wav")
		if _, err := runTest(t, append([]string{source, out}, flags...)...); err != nil {
			t.Fatal(err)
		}
		got := mediaCommand(t, tools.FFmpeg, "-v", "error", "-i", out, "-f", "s16le", "-")
		if !bytes.Equal(got, original) {
			t.Fatalf("identity rate options changed PCM: %v", flags)
		}
	}
	if _, err := runTest(t, source, filepath.Join(dir, "unknown.wav"), "-changeTo24.000"); err == nil {
		t.Fatal("target-only conversion accepted an unknown source frame rate")
	}
}

func TestSpeedPlanUsesSeparateClocksAndStages(t *testing.T) {
	for _, tc := range []struct {
		flags       []string
		rate, input int
		output      float64
	}{
		{[]string{"-slowdown"}, 48000, 48000, 50050},
		{[]string{"-speedup"}, 48000, 50050, 48000},
		{[]string{"-25.000", "-changeTo24.000"}, 44100, 44100, 45937.5},
		{[]string{"-24.000", "-slowdown"}, 44100, 44100, 44144.1},
	} {
		o, err := ParseOptions(append([]string{"in.wav", "out.wav"}, tc.flags...))
		if err != nil {
			t.Fatal(err)
		}
		a := o.Outputs[0].Options
		a.Rate = 96000
		plan, err := audioRatePlan(tc.rate, a)
		if err != nil || len(plan) != 2 || plan[0].input != tc.input || math.Abs(plan[0].output-tc.output) > 1e-9 || plan[1].input != tc.rate || plan[1].output != 96000 {
			t.Fatalf("%v: %+v, %v", tc.flags, plan, err)
		}
	}
}

func TestRateConversionMatchesWindowsPCM(t *testing.T) {
	if !ssrc.Available() {
		t.Skip("native resampling requires Cgo")
	}
	tools := mediaTools(t)
	dir := t.TempDir()
	input48 := filepath.Join("testdata", "rate-stereo48.flac")
	input44 := filepath.Join(dir, "same-samples-44100.wav")
	mediaCommand(t, tools.FFmpeg, "-v", "error", "-i", input48, "-af", "asetrate=44100", "-c:a", "pcm_s16le", input44)
	for i, tc := range []struct {
		input  string
		flags  []string
		frames int
		hash   string
	}{
		{input48, []string{"-slowdown"}, 100100, "7371f440f96d7bc8813867001b303c5ec579422030762969f1d8f7fe9119c924"},
		{input48, []string{"-speedup"}, 92068, "4c10babd5f191d8617e107fe97efe478980f245ad044835cc27598a24d2ba037"},
		{input48, []string{"-slowdown", "+100ms"}, 104449, "aca315592b943ad2e54402af569f2dc5591f7078821ecc6f8d789ce57f91c282"},
		{input48, []string{"-slowdown", "-100ms"}, 95095, "941498e765c5164a599ce044c38739621c5a3655fc093d4ff9d20339d7834a07"},
		{input48, []string{"-slowdown", "-resampleTo44100"}, 91566, "0d8184077d79535390e08f4a6ab57937287ba4bf691786b984ff7d861e7497fe"},
		{input44, []string{"-25.000", "-changeTo24.000"}, 100000, "76d8b19ae359d63fa36ef3707a98182c6f9eb9460be0f6cac8e23d0358559114"},
		{input44, []string{"-24.000", "-slowdown"}, 96096, "9e667407c871ad953ced75ddae4d7d37470eb771cb976fbc584adc3958016a7e"},
		{input44, []string{"-speedup"}, 92069, "c9527f21289f0780f6b7a8821ef4beb6f3f72f546d3390789028f6618bae18df"},
	} {
		out := filepath.Join(dir, fmt.Sprintf("reference%d.pcm", i))
		args := append([]string{tc.input, out, "-down24", "-no2ndpass", "--reference-tail"}, tc.flags...)
		if _, err := runTest(t, args...); err != nil {
			t.Fatalf("%v: %v", tc.flags, err)
		}
		got, err := os.ReadFile(out)
		if err != nil || len(got) != tc.frames*6 || fmt.Sprintf("%x", sha256.Sum256(got)) != tc.hash {
			t.Fatalf("%v: %d frames, SHA256 %x, error %v", tc.flags, len(got)/6, sha256.Sum256(got), err)
		}
	}
}
