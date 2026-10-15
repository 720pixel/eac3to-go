package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestStandaloneSubtitleCopyPreservesCueTimesAndReport(t *testing.T) {
	tools := mediaTools(t)
	dir := t.TempDir()
	input := filepath.Join(dir, "source.srt")
	output := filepath.Join(dir, "out.srt")
	report := filepath.Join(dir, "report.json")
	data := []byte("\xef\xbb\xbf1\r\n00:00:12,345 --> 00:00:14,567\r\nGenerated sign\r\n\r\n")
	if err := os.WriteFile(input, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := runTest(t, input, "1:", output, "--report="+report, "--ffmpeg="+tools.FFmpeg, "--ffprobe="+tools.FFprobe); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(output)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("cue bytes changed: %q %v", got, err)
	}
	r := readExtractionReport(t, report)
	if len(r.Outputs) != 1 || r.Outputs[0].MuxDelayMS != 0 {
		t.Fatal(r)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reader := checkedReader{ctx: ctx, reader: bytes.NewReader(data)}
	if _, err = reader.Read(make([]byte, 8)); err == nil {
		t.Fatal("ignored cancellation")
	}
}

func TestMatroskaContainerAudioOffsets(t *testing.T) {
	s := &Source{Paths: []string{"movie.mkv"}}
	s.Probe.Format.Name = "matroska,webm"
	s.Probe.Streams = []Stream{{Type: "video", Start: "0.040"}}
	st := &Stream{Type: "audio", Start: "0.163"}
	if got, ok := matroskaAudioOffset(s, st); !ok || got != 123 {
		t.Fatalf("%d %t", got, ok)
	}
	st.Start = "-0.083"
	if got, ok := matroskaAudioOffset(s, st); !ok || got != -123 {
		t.Fatalf("%d %t", got, ok)
	}
	for _, invalid := range []string{"N/A", "NaN", "+Inf", "1000000"} {
		st.Start = invalid
		if _, ok := matroskaAudioOffset(s, st); ok {
			t.Fatal(invalid)
		}
	}
}
