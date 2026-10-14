package app

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"eac3to/internal/aften"
	"eac3to/internal/bluray"
)

func TestCompressedDelayMatchesReference(t *testing.T) {
	tools := mediaTools(t)
	for _, tc := range []struct {
		codec, delay, hash string
		bytes              int
	}{
		{"ac3", "+100ms", "6516b5b55b2dbb8264d06d04bb416a529ec8c9db52f430f537a4deaeed9395fd", 5376},
		{"ac3", "-100ms", "43f5ba3a29ed42bbf56f262a9c198838c52da5a37c1c2a25f222d920b15ad163", 768},
		{"ac3", "+16ms", "6621bf4d3e1332442a557ff216485eff6ccb8757ba8f2eb693a2927e9f098a22", 3840},
		{"ac3", "-16ms", "d25f3b1fb2bc043f66db1cb13ac7d834a5a00a8781d75df2fccc1f2018d18046", 2304},
		{"dts", "+100ms", "5ce77b7931cc41ab74e84daac858b7749b7c2cc97a9dafc0947f01c846b494ea", 19456},
		{"dts", "-6ms", "9928587bbfb42d20394ef7c7cb11ccffa37e63729bd3c2b8612e34c0561efe52", 9216},
	} {
		t.Run(tc.codec+tc.delay, func(t *testing.T) {
			if tc.codec == "ac3" && strings.HasPrefix(tc.delay, "+") && !aften.Available() {
				t.Skip("AC-3 silence requires Cgo")
			}
			dir := t.TempDir()
			raw := filepath.Join("testdata", "delay-stereo48."+tc.codec)
			container := filepath.Join(dir, "source.mka")
			mediaCommand(t, tools.FFmpeg, "-v", "error", "-i", raw, "-c:a", "copy", container)
			for _, input := range []string{raw, container} {
				for _, mode := range []string{"", "-silence", "-loop"} {
					out := filepath.Join(dir, filepath.Base(input)+mode+"-delayed."+tc.codec)
					args := []string{input, out, tc.delay, "-keepDialnorm"}
					if mode != "" {
						args = append(args, mode)
					}
					if _, err := runTest(t, args...); err != nil {
						t.Fatal(err)
					}
					got, err := os.ReadFile(out)
					if err != nil {
						t.Fatal(err)
					}
					if len(got) != tc.bytes || fmt.Sprintf("%x", sha256.Sum256(got)) != tc.hash {
						t.Fatalf("%s %s: %d bytes, %x", input, mode, len(got), sha256.Sum256(got))
					}
				}
			}
		})
	}
}

func TestPlaylistOutputsHaveIndependentDelays(t *testing.T) {
	if !aften.Available() {
		t.Skip("AC-3 silence requires Cgo")
	}
	tools := mediaTools(t)
	dir := t.TempDir()
	streamDir := filepath.Join(dir, "BDMV", "STREAM")
	if err := os.MkdirAll(streamDir, 0755); err != nil {
		t.Fatal(err)
	}
	raw := filepath.Join("testdata", "delay-stereo48.ac3")
	mediaCommand(t, tools.FFmpeg, "-v", "error", "-i", raw, "-c:a", "copy", "-streamid", "0:4352", "-mpegts_m2ts_mode", "1", "-f", "mpegts", filepath.Join(streamDir, "00001.m2ts"))
	playlist := &bluray.Playlist{Path: filepath.Join(dir, "BDMV", "PLAYLIST", "00001.mpls"), Items: []bluray.PlayItem{{ClipID: "00001", CodecID: "M2TS", OutTime: 90000, Streams: []bluray.Stream{{EntryType: 1, Kind: "audio", PID: 4352, CodingType: 0x81}}}}}
	stream := &Stream{Codec: "ac3", Type: "audio", ID: "0x1100"}
	var outputs []Output
	for _, delay := range []float64{0, 16, -16} {
		path := filepath.Join(dir, fmt.Sprintf("delay-%g.ac3", delay))
		outputs = append(outputs, Output{Track: Track{Number: 1, Kind: "audio", Stream: stream}, Copy: true, Format: "ac3", Codec: "ac3", Path: path, Stage: path, Request: OutputRequest{Options: AudioOptions{DelayMS: delay}}})
	}
	remaining, err := copyNativeStreams(context.Background(), &Source{Playlist: playlist}, outputs, Options{}, io.Discard)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("playlist delay: %v, remaining %d", err, len(remaining))
	}
	for i, want := range []string{"f6ff04e54dab56b3950f777b9608b7869a6db7c8ba7675c86a191cbee6af110e", "6621bf4d3e1332442a557ff216485eff6ccb8757ba8f2eb693a2927e9f098a22", "d25f3b1fb2bc043f66db1cb13ac7d834a5a00a8781d75df2fccc1f2018d18046"} {
		got, err := os.ReadFile(outputs[i].Stage)
		if err != nil || fmt.Sprintf("%x", sha256.Sum256(got)) != want {
			t.Fatalf("output %d: %x, %v", i, sha256.Sum256(got), err)
		}
	}
}

func TestCompressedDelayRejectsDamagedOrExhaustedInput(t *testing.T) {
	mediaTools(t)
	dir := t.TempDir()
	source, err := os.ReadFile(filepath.Join("testdata", "delay-stereo48.ac3"))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, delay string
		input       []byte
	}{
		{"truncated", "-16ms", source[:len(source)-1]},
		{"exhausted", "-1000ms", source},
		{"fractional", "+15.5ms", source},
		{"crc", "-16ms", append(append(append([]byte{}, source[:100]...), source[100]^1), source[101:]...)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input, out := filepath.Join(dir, tc.name+"-input.ac3"), filepath.Join(dir, tc.name+"-output.ac3")
			if err := os.WriteFile(input, tc.input, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := runTest(t, input, out, tc.delay, "-keepDialnorm"); err == nil {
				t.Fatal("invalid delay operation succeeded")
			}
			if _, err := os.Stat(out); !os.IsNotExist(err) {
				t.Fatalf("failed operation published output: %v", err)
			}
			got, err := os.ReadFile(input)
			if err != nil || !bytes.Equal(got, tc.input) {
				t.Fatal("input changed")
			}
		})
	}
}
