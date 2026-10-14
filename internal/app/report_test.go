package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func readExtractionReport(t *testing.T, path string) ExtractionReport {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var report ExtractionReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatal(err)
	}
	if report.Version != 1 {
		t.Fatalf("wrong report version %d", report.Version)
	}
	return report
}

func TestExtractionReportSourceManualResidualAndTrueHDName(t *testing.T) {
	s, _, _ := truehdAppSource(t, 1, 7)
	dir := t.TempDir()
	o := Options{Quiet: true, Report: filepath.Join(dir, "report.json"), Outputs: []OutputRequest{
		truehdOutputOptions(2, filepath.Join(dir, "main.thd"), true, 9, false),
		truehdOutputOptions(2, filepath.Join(dir, "core.ac3"), true, -35, true),
	}}
	outputs, err := buildOutputs(s, o)
	if err != nil {
		t.Fatal(err)
	}
	if err := execute(context.Background(), Tools{}, s, outputs, o, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	r := readExtractionReport(t, o.Report)
	if len(r.Outputs) != 2 || r.Outputs[0].Track != 2 || r.Outputs[1].Track != 2 {
		t.Fatalf("report lost requested source track linkage: %+v", r)
	}
	thd, core := r.Outputs[0], r.Outputs[1]
	if thd.Path != filepath.Join(dir, "main DELAY 16ms.thd") || thd.MuxDelayMS != 16 || !thd.DelayInFilename || thd.Codec != "truehd" || thd.Kind != "audio" {
		t.Fatalf("TrueHD delay applied incorrectly: %+v", thd)
	}
	if core.MuxDelayMS != 4 || core.DelayInFilename || core.Codec != "ac3" || core.Path != filepath.Join(dir, "core.ac3") {
		t.Fatalf("source7 + manual-35 = -28ms should drop32ms and report+4ms: %+v", core)
	}
	for _, out := range r.Outputs {
		if _, err := os.Stat(out.Path); err != nil {
			t.Fatalf("report path does not link to the published output: %v", err)
		}
	}
}

func TestCLIReportCompressedResidualAndPCMZero(t *testing.T) {
	tools := mediaTools(t)
	for _, codec := range []string{"ac3", "dts"} {
		dir := t.TempDir()
		input := filepath.Join("testdata", "delay-stereo48."+codec)
		out, manifest := filepath.Join(dir, "out."+codec), filepath.Join(dir, "report.json")
		delay := "-28ms"
		if codec == "dts" {
			delay = "-6ms"
		}
		if _, err := runTest(t, input, out, delay, "-keepDialnorm", "--report="+manifest, "--ffmpeg="+tools.FFmpeg, "--ffprobe="+tools.FFprobe); err != nil {
			t.Fatal(err)
		}
		r := readExtractionReport(t, manifest)
		if len(r.Outputs) != 1 || r.Outputs[0].Track != 1 || r.Outputs[0].Path != out || r.Outputs[0].MuxDelayMS != 4 {
			t.Fatalf("%s report residual: %+v", codec, r)
		}
	}
	dir := t.TempDir()
	wav, samples := waveFixture(t, dir)
	output, reportPath := filepath.Join(dir, "delayed.wav"), filepath.Join(dir, "pcm-report.json")
	if _, err := runTest(t, wav, output, "-7ms", "--report="+reportPath); err != nil {
		t.Fatal(err)
	}
	r := readExtractionReport(t, reportPath)
	if len(r.Outputs) != 1 || r.Outputs[0].MuxDelayMS != 0 || r.Outputs[0].DelayInFilename {
		t.Fatalf("PCM must not carry a second mux correction: %+v", r)
	}
	got := mediaCommand(t, tools.FFmpeg, "-v", "error", "-i", output, "-f", "s16le", "-")
	if !bytes.Equal(got, samples[7*48*4:]) {
		t.Fatal("PCM report describes an output without its sample correction")
	}
}

func TestReportCachesCannotLeakCompressedResidual(t *testing.T) {
	stream := &Stream{Index: 0, Codec: "ac3", Type: "audio", ID: "0x1100", Channels: 2, SampleRate: "48000"}
	dir := t.TempDir()
	outputs := []Output{{Track: Track{Number: 2, Kind: "audio", Stream: stream}, Codec: "pcm_s16le", Format: "wav", Stage: filepath.Join(dir, "decoded.wav")}, {Track: Track{Number: 2, Kind: "audio", Stream: stream}, Copy: true, Codec: "ac3", Format: "ac3", Stage: filepath.Join(dir, "raw.ac3")}}
	for i := range outputs {
		outputs[i].Request.Options = AudioOptions{Speed: 1, ExtractChannel: -1}
	}
	prepareOutputReportTiming(&Source{}, outputs)
	jobs, caches, err := prepareAudioJobs(&Source{}, outputs)
	if err != nil || len(jobs) != 1 || len(caches) != 1 {
		t.Fatalf("cache setup: %v", err)
	}
	if caches[0].muxTiming != nil {
		t.Fatal("decode cache inherited the requested output's residual state")
	}
	setOutputMuxDelay(caches[0], 4)
	setOutputMuxDelay(outputs[1], 4)
	if jobs[0].output.muxTiming != outputs[0].muxTiming || outputs[0].muxTiming.delay.Load() != 0 || outputs[1].muxTiming.delay.Load() != 4 {
		t.Fatal("copy/cache state overwrote a decoded output's already applied delay")
	}
}

func TestReportDestinationAndFailureSafety(t *testing.T) {
	mediaTools(t)
	for _, mode := range []string{"input", "output", "existing", "input-hardlink"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			input := filepath.Join(dir, "source.ac3")
			original, err := os.ReadFile(filepath.Join("testdata", "delay-stereo48.ac3"))
			if err != nil || os.WriteFile(input, original, 0600) != nil {
				t.Fatal("could not create input")
			}
			out, reportPath := filepath.Join(dir, "out.ac3"), filepath.Join(dir, "report.json")
			force := ""
			switch mode {
			case "input":
				reportPath, force = input, "--force"
			case "output":
				reportPath, force = out, "--force"
			case "existing":
				if err := os.WriteFile(reportPath, []byte("existing"), 0600); err != nil {
					t.Fatal(err)
				}
			case "input-hardlink":
				if err := os.Link(input, reportPath); err != nil {
					t.Fatal(err)
				}
				force = "--force"
			}
			args := []string{input, out, "-keepDialnorm", "--report=" + reportPath}
			if force != "" {
				args = append(args, force)
			}
			if _, err := runTest(t, args...); err == nil {
				t.Fatal("unsafe report destination accepted")
			}
			if got, err := os.ReadFile(input); err != nil || !bytes.Equal(got, original) {
				t.Fatal("report mutated source")
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Fatal("destination rejection published media")
			}
		})
	}
	dir := t.TempDir()
	for _, existing := range []bool{false, true} {
		out := filepath.Join(dir, fmt.Sprintf("failed%v.ac3", existing))
		reportPath := filepath.Join(dir, fmt.Sprintf("failed%v.json", existing))
		if existing {
			if err := os.WriteFile(reportPath, []byte("old report"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := runTest(t, filepath.Join("testdata", "delay-stereo48.ac3"), out, "-1000ms", "--report="+reportPath, "--force"); err == nil {
			t.Fatal("exhausted extraction succeeded")
		}
		data, err := os.ReadFile(reportPath)
		if existing {
			if err != nil || string(data) != "old report" {
				t.Fatal("failed operation replaced old report")
			}
		} else if !os.IsNotExist(err) {
			t.Fatal("failed operation published a report")
		}
		if _, err := os.Stat(out); !os.IsNotExist(err) {
			t.Fatal("failed extraction published output")
		}
	}
}

func TestReportForceAndDryRun(t *testing.T) {
	mediaTools(t)
	dir := t.TempDir()
	input := filepath.Join("testdata", "delay-stereo48.ac3")
	out, report := filepath.Join(dir, "out.ac3"), filepath.Join(dir, "report.json")
	if err := os.WriteFile(report, []byte("old report"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := runTest(t, input, out, "-keepDialnorm", "--report="+report, "--force"); err != nil {
		t.Fatal(err)
	}
	r := readExtractionReport(t, report)
	if len(r.Outputs) != 1 || r.Outputs[0].MuxDelayMS != 0 {
		t.Fatalf("force report: %+v", r)
	}
	newOut, newReport := filepath.Join(dir, "new", "out.ac3"), filepath.Join(dir, "new", "report.json")
	if _, err := runTest(t, input, newOut, "--report="+newReport, "--dry-run"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(newOut)); !os.IsNotExist(err) {
		t.Fatal("dry-run created output/report paths")
	}
	for _, args := range [][]string{{input, "--report=" + newReport}, {input, out, "--report="}} {
		if _, err := ParseOptions(args); err != nil {
			continue
		}
		if _, err := runTest(t, args...); err == nil {
			t.Fatal("listing or missing report path accepted")
		}
	}
}

func TestReportUnknownContainerOffsetIsNotAssumedZero(t *testing.T) {
	stream := &Stream{Codec: "ac3", Type: "audio"}
	source := &Source{Paths: []string{"source.mka"}}
	source.Probe.Format.Name = "matroska"
	outputs := []Output{{Track: Track{Number: 1, Kind: "audio", Stream: stream}, Copy: true, Codec: "ac3", Format: "ac3"}}
	prepareOutputReportTiming(source, outputs)
	setOutputResidual(outputs[0], 4)
	if _, err := makeExtractionReport(outputs); err == nil {
		t.Fatal("a manual frame correction falsely verified an unknown container source offset")
	}
}
