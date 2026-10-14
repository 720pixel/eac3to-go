package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"eac3to/internal/aften"
)

func TestNativeAC3PipelineReference(t *testing.T) {
	if !aften.Available() {
		t.Skip("Aften backend requires Cgo")
	}
	tools := mediaTools(t)
	dir := t.TempDir()
	raw := make([]byte, 5120*2*2)
	for frame := 0; frame < 5120; frame++ {
		for channel := 0; channel < 2; channel++ {
			frequency := float64(997 + 127*channel)
			value := int(math.RoundToEven(3072 * math.Sin(2*math.Pi*frequency*float64(frame)/48000)))
			if frame%(97+channel*13) == 0 {
				value += 1024 + channel*128
			}
			binary.LittleEndian.PutUint16(raw[(frame*2+channel)*2:], uint16(int16(value)))
		}
	}
	pcm, wav := filepath.Join(dir, "source.pcm"), filepath.Join(dir, "source.wav")
	if err := os.WriteFile(pcm, raw, 0600); err != nil {
		t.Fatal(err)
	}
	mediaCommand(t, tools.FFmpeg, "-v", "error", "-f", "s16le", "-ar", "48000", "-ac", "2", "-i", pcm, "-c:a", "pcm_s16le", wav)
	first, second := filepath.Join(dir, "first.ac3"), filepath.Join(dir, "second.ac3")
	if _, err := runTest(t, wav, "1:", first, "-192", "1:", second, "-192", "--jobs=2"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{first, second} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		const expected = "f6ff04e54dab56b3950f777b9608b7869a6db7c8ba7675c86a191cbee6af110e"
		if len(data) != 3072 || fmt.Sprintf("%x", sha256.Sum256(data)) != expected {
			t.Fatalf("AC-3 differs from the 4.00 reference: %d bytes, %x", len(data), sha256.Sum256(data))
		}
	}
}

func TestAC3CannotUsePCMPackingOrBypassValidation(t *testing.T) {
	stream := &Stream{Codec: "pcm_s16le", SampleFormat: "s16", SampleRate: "48000", Channels: 2, Bits: 16, Layout: "stereo"}
	options := AudioOptions{Speed: 1, ExtractChannel: -1, Bitrate: 192}
	out := Output{Codec: "ac3", Format: "ac3", Depth: 16, Track: Track{Kind: "audio", Stream: stream}, Request: OutputRequest{Options: options}}
	if canPackPCM(out) {
		t.Fatal("native AC-3 output entered the FFmpeg PCM packing path")
	}
	if err := validateParityPaths(&Source{}, []Output{out}, Options{}); err != nil {
		t.Fatal(err)
	}
	if aften.Available() {
		if err := validateAudioOutput(out); err != nil {
			t.Fatal(err)
		}
		out.Request.Options.Bitrate = 123
		if err := validateAudioOutput(out); err == nil {
			t.Fatal("invalid AC-3 bitrate accepted")
		}
	}
	out.Codec, out.Format = "flac", "flac"
	if err := validateAudioOutput(out); err == nil || !strings.Contains(err.Error(), "bitrate") {
		t.Fatalf("lossy bitrate was ignored for FLAC output: %v", err)
	}
}

func TestNativeAC3FinishesPeakScanAfterEncoderRangeFailure(t *testing.T) {
	if !aften.Available() {
		t.Skip("Aften backend requires Cgo")
	}
	tools := mediaTools(t)
	dir := t.TempDir()
	wav, _ := waveFixture(t, dir)
	out := filepath.Join(dir, "corrected.ac3")
	var log bytes.Buffer
	if err := Run(context.Background(), []string{wav, out, "+60dB", "-192"}, &bytes.Buffer{}, &log); err != nil {
		t.Fatalf("correction failed: %v\n%s", err, &log)
	}
	if !strings.Contains(log.String(), "repeating with") {
		t.Fatal("overrange PCM did not cause a correction pass")
	}
	decoded := mediaCommand(t, tools.FFmpeg, "-v", "error", "-xerror", "-err_detect", "crccheck+explode", "-i", out, "-f", "s16le", "-")
	if len(decoded) != 4*1536*2*2 {
		t.Fatalf("AC-3 correction lost encoded frames: %d decoded bytes", len(decoded))
	}
	failed := filepath.Join(dir, "unrepresentable.ac3")
	if _, err := runTest(t, wav, failed, "+60dB", "-192", "-no2ndpass"); err == nil {
		t.Fatal("unrepresentable PCM bypassed the encoder guard")
	}
	if _, err := os.Stat(failed); !os.IsNotExist(err) {
		t.Fatalf("failed encoding published output: %v", err)
	}
}
