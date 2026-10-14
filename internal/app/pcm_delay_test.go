package app

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path/filepath"
	"testing"

	"eac3to/internal/bluray"
)

func TestFLACDelayPreservesSamples(t *testing.T) {
	tools := mediaTools(t)
	dir := t.TempDir()
	wav, input := waveFixture(t, dir)
	flac := filepath.Join(dir, "source.flac")
	mediaCommand(t, tools.FFmpeg, "-v", "error", "-i", wav, "-c:a", "flac", flac)
	for _, delay := range []int{-6, 6, 100} {
		for _, mode := range []string{"", "-silence", "-loop"} {
			out := filepath.Join(dir, fmt.Sprintf("delay%d%s.wav", delay, mode))
			args := []string{flac, out, fmt.Sprintf("%+dms", delay)}
			if mode != "" {
				args = append(args, mode)
			}
			if _, err := runTest(t, args...); err != nil {
				t.Fatal(err)
			}
			got := mediaCommand(t, tools.FFmpeg, "-v", "error", "-i", out, "-f", "s16le", "-")
			want := input
			if delay < 0 {
				want = input[-delay*48*4:]
			} else {
				want = append(make([]byte, delay*48*4), input...)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("delay %d %s: sample mismatch (%d vs %d bytes)", delay, mode, len(got), len(want))
			}
		}
	}
}

func TestPCMSourceAndManualDelaysCombineBeforeEditing(t *testing.T) {
	tools := mediaTools(t)
	dir := t.TempDir()
	wav, input := waveFixture(t, dir)
	stream := &Stream{Index: 0, Codec: "pcm_s16le", Type: "audio", SampleFormat: "s16", SampleRate: "48000", Channels: 2, Layout: "stereo", Bits: 16}
	for _, sourceDelay := range []int64{-20, 5} {
		source := &Source{Paths: []string{wav}, InputArgs: []string{"-i", wav}, PCMSourceDelay: &sourceDelay}
		for _, manual := range []int64{-sourceDelay, -7, 7} {
			path := filepath.Join(dir, fmt.Sprintf("source%d-manual%d.wav", sourceDelay, manual))
			out := Output{Track: Track{Number: 1, Kind: "audio", Stream: stream}, Format: "wav", Codec: "pcm_s16le", Path: path, Stage: path, Depth: 16, Request: OutputRequest{Options: AudioOptions{Speed: 1, ExtractChannel: -1, DelayMS: float64(manual)}}}
			if err := runAudio(context.Background(), tools, source, out, Options{Quiet: true}, io.Discard); err != nil {
				t.Fatal(err)
			}
			got := mediaCommand(t, tools.FFmpeg, "-v", "error", "-i", path, "-f", "s16le", "-")
			delay := sourceDelay + manual
			want := input
			if delay < 0 {
				want = input[-delay*48*4:]
			} else if delay > 0 {
				want = append(make([]byte, delay*48*4), input...)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("source %+d ms + manual %+d ms: sample mismatch", sourceDelay, manual)
			}
		}
	}
}

func TestNativePCMJobsShareUneditedCacheAndDelay(t *testing.T) {
	dir := t.TempDir()
	stream := &Stream{Codec: "pcm_bluray", Type: "audio", SampleFormat: "s16", SampleRate: "48000", Channels: 2, Layout: "stereo", Bits: 16}
	var outputs []Output
	for _, delay := range []float64{-5, 7} {
		path := filepath.Join(dir, fmt.Sprintf("output%g.wav", delay))
		outputs = append(outputs, Output{Track: Track{Number: 1, Kind: "audio", Stream: stream}, Format: "wav", Codec: "pcm_s16le", Path: path, Stage: path, Depth: 16, Request: OutputRequest{Options: AudioOptions{Speed: 1, ExtractChannel: -1, DelayMS: delay}}})
	}
	jobs, caches, err := prepareAudioJobs(&Source{Playlist: &bluray.Playlist{}}, outputs)
	if err != nil || len(jobs) != 2 || len(caches) != 1 {
		t.Fatalf("jobs %d, caches %d, error %v", len(jobs), len(caches), err)
	}
	if caches[0].PCMSourceDelay == nil || !caches[0].NativePCM || !caches[0].DecodeCache {
		t.Fatal("PCM cache lost its source delay metadata")
	}
	for _, job := range jobs {
		if job.source.PCMSourceDelay != caches[0].PCMSourceDelay {
			t.Fatal("outputs do not share the measured source delay")
		}
	}
}

func TestLossyDTSDecodeNeedsReferenceOverride(t *testing.T) {
	stream := &Stream{Type: "audio", Codec: "dts", Profile: "DTS"}
	out := Output{Track: Track{Kind: "audio", Stream: stream}}
	if err := validateParityPaths(&Source{}, []Output{out}, Options{}); err == nil {
		t.Fatal("unverified lossy DTS decode was accepted")
	}
	if err := validateParityPaths(&Source{}, []Output{out}, Options{AllowUnverified: true}); err != nil {
		t.Fatal(err)
	}
	stream.Profile = "DTS-HD MA"
	if err := validateParityPaths(&Source{}, []Output{out}, Options{}); err != nil {
		t.Fatal(err)
	}
}
