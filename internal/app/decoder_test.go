package app

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func matchedTestDecoder(t *testing.T) string {
	t.Helper()
	path := os.Getenv("EAC3TO_TEST_DECODER")
	if path == "" {
		path = filepath.Join("..", "..", "bin", "libexec", executableName("eac3to-decoder"))
	}
	if !executable(path) {
		t.Skip("build the matched decoder with make decoder, or set EAC3TO_TEST_DECODER")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatal(err)
	}
	return abs
}

func TestMatchedDecoderKeepsSelectedTools(t *testing.T) {
	decoder := matchedTestDecoder(t)
	base := mediaTools(t)
	selected, err := findTools(Options{FFmpeg: base.FFmpeg, FFprobe: base.FFprobe, Decoder: decoder})
	if err != nil {
		t.Fatal(err)
	}
	if selected.FFmpeg != base.FFmpeg || selected.FFprobe != base.FFprobe || selected.Decoder != decoder || !selected.DecoderProfile {
		t.Fatalf("unexpected tool selection: %+v", selected)
	}
	if selected.audioDecoder("dts", "DTS") != decoder || selected.audioDecoder("ac3", "") != decoder || selected.audioDecoder("flac", "") != base.FFmpeg || selected.audioDecoder("dts", "DTS-HD MA") != base.FFmpeg {
		t.Fatal("decoder profile changed an unrelated codec path")
	}
}

func TestMatchedDecodeAndDelayAgainstWindows(t *testing.T) {
	decoder := matchedTestDecoder(t)
	tools := mediaTools(t)
	dir := t.TempDir()
	for _, tc := range []struct {
		codec, delay, hash string
		frames             int
	}{
		{"ac3", "", "d5aba8cc0a2a6effb0796ab2c8020f4e07311480e4ca7fc2edea4a3b93239aa5", 6144},
		{"dts", "", "307c35bf452de39e72a9e1116aac12dfd1b1fd54531b920deb0cb23877dfd663", 5120},
		{"dts", "+6ms", "694a868e5deb6f588d7080c58aab72f41f98abd13159ea20ab159c10d0aab0db", 5408},
		{"dts", "-6ms", "16d87758e2d53ca41c609eaa2f77c80c161fcf09e605566ec6e57aa3939c7e5b", 4832},
		{"dts", "+100ms", "7ec7c17c31fe31c88b02dc66a9939c8b28671a90a072f612b44033af9ff02e30", 9920},
	} {
		input := filepath.Join("testdata", "delay-stereo48."+tc.codec)
		container := filepath.Join(dir, tc.codec+tc.delay+".mka")
		mediaCommand(t, tools.FFmpeg, "-v", "error", "-i", input, "-c:a", "copy", container)
		for _, source := range []string{input, container} {
			out := filepath.Join(t.TempDir(), "decoded.pcm")
			args := []string{source, out, "-full", "-no2ndpass", "-keepDialnorm", "--decoder=" + decoder}
			if tc.delay != "" {
				args = append(args, tc.delay)
			}
			if _, err := runTest(t, args...); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(out)
			if err != nil || len(got) != tc.frames*16 || fmt.Sprintf("%x", sha256.Sum256(got)) != tc.hash {
				t.Fatalf("%s %s: %d bytes, %x, %v", source, tc.delay, len(got), sha256.Sum256(got), err)
			}
		}
	}
}
