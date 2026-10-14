//go:build cgo

package aften

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"testing"
)

func referenceSamples(rate, channels, frames int) []float64 {
	samples := make([]float64, frames*channels)
	for frame := 0; frame < frames; frame++ {
		for channel := 0; channel < channels; channel++ {
			value := int16(math.RoundToEven(3072 * math.Sin(2*math.Pi*float64(997+127*channel)*float64(frame)/float64(rate))))
			if frame%(97+channel*13) == 0 {
				value += int16(1024 + channel*128)
			}
			samples[frame*channels+channel] = float64(value) / 32768
		}
	}
	return samples
}

func encodeTestSamples(t *testing.T, c Config, samples []float64, chunkFrames int) []byte {
	t.Helper()
	var output bytes.Buffer
	e, err := NewEncoder(&output, c)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	for len(samples) > 0 {
		n := min(len(samples), chunkFrames*c.Channels)
		if err := e.Write(samples[:n]); err != nil {
			t.Fatal(err)
		}
		samples = samples[n:]
	}
	if err := e.Finish(); err != nil {
		t.Fatal(err)
	}
	if err := e.Finish(); err != nil {
		t.Fatalf("repeated Finish: %v", err)
	}
	if err := e.Write(nil); !errors.Is(err, ErrClosed) {
		t.Fatalf("Write after Finish: %v", err)
	}
	return output.Bytes()
}

func TestEncoderReferenceMono(t *testing.T) {
	// Captured output from the historical encoder for exact signed16 PCM.
	const expected = "a5212ed98f818171e88b35eaf43ebf1d97f321215dc709c79b2341778510fea3"
	for _, chunk := range []int{1, 97, 1536, 4096} {
		t.Run(fmt.Sprint(chunk), func(t *testing.T) {
			t.Parallel()
			samples := referenceSamples(48000, 1, 5120)
			original := slices.Clone(samples)
			output := encodeTestSamples(t, Config{SampleRate: 48000, Channels: 1, ChannelMask: 4, BitrateKbps: 96}, samples, chunk)
			if got := fmt.Sprintf("%x", sha256.Sum256(output)); got != expected {
				t.Fatalf("output SHA256 = %s, want %s", got, expected)
			}
			if !slices.Equal(samples, original) {
				t.Fatal("modified caller's PCM")
			}
		})
	}
}

func TestEncoderReferenceMatrix(t *testing.T) {
	cases := []struct {
		name                            string
		rate, channels, bitrate, frames int
		mask                            uint32
		hash                            string
	}{
		{"mono_default", 48000, 1, 0, 5120, 4, "a27cee5f03012938c0befa58777caa44b38c9ddc62b4f674fe9dfc119e3a502c"},
		{"stereo_192", 48000, 2, 192, 5120, 3, "f6ff04e54dab56b3950f777b9608b7869a6db7c8ba7675c86a191cbee6af110e"},
		{"stereo_default", 48000, 2, 0, 5120, 3, "427788cc6d3cd7e8f5b510a09e530fbd889d8fc49d1c7477f5030c518a33ab91"},
		{"surround_default", 48000, 6, 0, 5120, 0x3f, "eee88bb63f87128dabe4fb1cee92d5da3a79aec049a36f247bd7b30b5be31667"},
		{"surround_sides", 48000, 6, 640, 5120, 0x60f, "eee88bb63f87128dabe4fb1cee92d5da3a79aec049a36f247bd7b30b5be31667"},
		{"stereo_44100", 44100, 2, 192, 5120, 3, "58297d19043fdefc624a545811f21bc003ad13f1a2af03c2398c07fbada68f05"},
		{"stereo_32000", 32000, 2, 192, 5120, 3, "a76dbd32f03c226b2225bbca09d368b0bc2c56353abed27f57b6c4cb8baad40e"},
		{"mono_1280", 48000, 1, 96, 1280, 4, "abbc1a67e9dda271a5364325149414eb86209dc3c4d090dfecf67aae00c4df05"},
		{"mono_1281", 48000, 1, 96, 1281, 4, "85c7db02f39931d6fe6647baa3fca74a88973459a6debd78c008054e217379d4"},
		{"stereo_1280", 48000, 2, 192, 1280, 3, "6eeb2f786f7225b5b462a3460becbe6c3ed2517df2cfb63f9fc98eec7de5750e"},
		{"stereo_1281", 48000, 2, 192, 1281, 3, "6bffe8dfc5860690edd2a74357c2988e29a9563f510781dda646f077162f2f98"},
		{"long_mono_default", 48000, 1, 448, 100000, 4, "c74306ab329add16f383e797058548aac44fc8f07ab1b6c4c2e94c81f0d7b617"},
		{"long_stereo_default", 48000, 2, 448, 100000, 3, "a50ce8c19bbcc67916a452296b3a6cb22fc0c9b264e16d6cedac682b47db9b37"},
		{"long_stereo_44100", 44100, 2, 192, 100000, 3, "4481766dbf4f0f5653b12296422e3f090abf115465c154955a13f3469e129c54"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			samples := referenceSamples(c.rate, c.channels, c.frames)
			output := encodeTestSamples(t, Config{SampleRate: c.rate, Channels: c.channels, BitrateKbps: c.bitrate, ChannelMask: c.mask, FrameContexts: 32}, samples, 97)
			if got := fmt.Sprintf("%x", sha256.Sum256(output)); got != c.hash {
				h := sha256.New()
				for _, sample := range samples {
					var b [8]byte
					binary.LittleEndian.PutUint64(b[:], math.Float64bits(sample))
					_, _ = h.Write(b[:])
				}
				t.Fatalf("output SHA256 = %s, want %s; PCM SHA256 %x", got, c.hash, h.Sum(nil))
			}
		})
	}
}

func TestEncoderOverrangeReferenceAndLimit(t *testing.T) {
	samples := referenceSamples(48000, 2, 5120)
	for i := range samples {
		samples[i] *= math.Pow(10, 24.0/20)
	}
	output := encodeTestSamples(t, Config{SampleRate: 48000, Channels: 2, BitrateKbps: 192, FrameContexts: 32}, samples, 1536)
	const expected = "394c308665f49b1d8c484a0908327be2bb87ad7981b5a0c14d51b36acd310f72"
	if got := fmt.Sprintf("%x", sha256.Sum256(output)); got != expected {
		t.Fatalf("overrange SHA256 = %s, want %s", got, expected)
	}
	e, err := NewEncoder(io.Discard, Config{SampleRate: 48000, Channels: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	samples = referenceSamples(48000, 1, 1536)
	for i := range samples {
		samples[i] *= 1000
	}
	if err := e.Write(samples); !errors.Is(err, ErrCoefficientRange) {
		t.Fatalf("unsafe coefficient range: %v", err)
	}
}

func TestEncoderEmptyInput(t *testing.T) {
	var output bytes.Buffer
	e, err := NewEncoder(&output, Config{SampleRate: 48000, Channels: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if err := e.Finish(); !errors.Is(err, ErrNoSamples) {
		t.Fatalf("empty input: %v", err)
	}
	if output.Len() != 0 {
		t.Fatal("empty input produced an AC-3 frame")
	}
}

func TestEncoderFinalSampleCounts(t *testing.T) {
	for _, count := range []int{1, 256, 1280, 1281, 1536, 1537, 2816, 2817, 3072} {
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			output := encodeTestSamples(t, Config{SampleRate: 48000, Channels: 2, BitrateKbps: 192}, referenceSamples(48000, 2, count), 19)
			// Each AC-3 frame spans 1536 samples and carries a 256-sample
			// transform delay; the final frame must cover the whole input.
			want := ((count + 256 + 1535) / 1536) * 768
			if len(output) != want {
				t.Fatalf("%d output bytes, want %d", len(output), want)
			}
		})
	}
}

func TestEncoderRejectsInvalidPCMAndCanContinue(t *testing.T) {
	var output bytes.Buffer
	e, err := NewEncoder(&output, Config{SampleRate: 48000, Channels: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	for _, samples := range [][]float64{{0}, {0, math.NaN()}, {math.Inf(1), 0}, {0, math.Inf(-1)}} {
		if err := e.Write(samples); !errors.Is(err, ErrInvalidPCM) {
			t.Fatalf("got %v", err)
		}
	}
	if output.Len() != 0 {
		t.Fatal("invalid input produced output")
	}
	if err := e.Write([]float64{0, 0}); err != nil {
		t.Fatal(err)
	}
	if err := e.Finish(); err != nil {
		t.Fatal(err)
	}
}

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestEncoderWriterFailureAndAbort(t *testing.T) {
	e, err := NewEncoder(shortWriter{}, Config{SampleRate: 48000, Channels: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if err := e.Write(make([]float64, 1536)); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("got %v", err)
	}
	if err := e.Finish(); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("Finish lost writer error: %v", err)
	}
	if err := e.Close(); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("Close lost writer error: %v", err)
	}
	var output bytes.Buffer
	e, err = NewEncoder(&output, Config{SampleRate: 48000, Channels: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Write([]float64{0.1}); err != nil {
		t.Fatal(err)
	}
	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	if output.Len() != 0 {
		t.Fatal("Close flushed cancelled audio")
	}
	if err := e.Finish(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Finish after Close: %v", err)
	}
}
