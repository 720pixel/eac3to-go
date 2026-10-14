package audio

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"testing"
)

func TestDPLReferenceImpulses(t *testing.T) {
	// Windows eac3to 4.00, -downDpl -full -no2ndpass. Each input channel
	// has +0.25 at 1000*(channel+1), followed by -0.25 100 frames later.
	// These hashes cover every f64 sample, including the silent intervals.
	cases := []struct {
		layout   string
		channels int
		lfe      bool
		digest   string
	}{
		{"7.1", 8, false, "c93febbfeb148853be18be6b1e87881ee3ad7e699c17378d1a69b10dd51ddaa3"},
		{"7.1", 8, true, "6c703387dc4bc63d8a3d4427d96728662dd7aeb52c35ba423962bdd8c8b14d48"},
		{"6.1", 7, false, "318acb47291ad046512891403f3ed7129b56864d4cc85a3ff9df4e3b337af22e"},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s/lfe=%t", tc.layout, tc.lfe), func(t *testing.T) {
			cfg := config(tc.layout, tc.channels)
			cfg.Mix, cfg.MixLFE = "dplii", tc.lfe
			p, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			input := make([]float64, 48000*tc.channels)
			for ch := 0; ch < tc.channels; ch++ {
				input[(1000*(ch+1))*tc.channels+ch] = .25
				input[(1000*(ch+1)+100)*tc.channels+ch] = -.25
			}
			h := sha256.New()
			var word [8]byte
			for start := 0; start < 48000; start += 997 {
				end := min(start+997, 48000)
				out, err := p.Process(input[start*tc.channels : end*tc.channels])
				if err != nil {
					t.Fatal(err)
				}
				if len(out) != (end-start)*2 {
					t.Fatal("plain matrix changed the frame count")
				}
				for _, value := range out {
					binary.LittleEndian.PutUint64(word[:], math.Float64bits(value))
					h.Write(word[:])
				}
			}
			if got := fmt.Sprintf("%x", h.Sum(nil)); got != tc.digest {
				t.Fatalf("PCM SHA256 = %s, want %s", got, tc.digest)
			}
			if tail, err := p.Finish(); err != nil || len(tail) != 0 {
				t.Fatalf("unexpected matrix tail: %d, %v", len(tail), err)
			}
		})
	}
}

func TestDPLBackCenterRounding(t *testing.T) {
	cfg := config("6.1", 7)
	cfg.Mix = "dplii"
	p, out := process(t, cfg, []float64{0, 0, 0, 0, .25, 0, 0})
	assertSamples(t, out, []float64{.24147696577520597, -.24147696577520597})
	if p.Peak() != .24147696577520597 {
		t.Fatal("signed surround peak was not retained")
	}
}

func TestDPLDenseReference(t *testing.T) {
	// Deterministic six-channel signed-16 PCM, decoded by Windows eac3to
	// 4.00 using -downDpl -full -no2ndpass. Overlapping contributions test
	// the order of multiplication and summation as well as routing.
	const frames = 12013
	input := make([]float64, frames*6)
	state := uint32(1)
	for i := range input {
		state = 1664525*state + 1013904223
		value := (int32(state>>16) - 32768) >> 5
		input[i] = float64(value) / 32768
	}
	cfg := config("5.1", 6)
	cfg.Mix = "dplii"
	_, out := process(t, cfg, input)
	h := sha256.New()
	var word [8]byte
	for _, value := range out {
		binary.LittleEndian.PutUint64(word[:], math.Float64bits(value))
		h.Write(word[:])
	}
	const want = "9ecdd80557b117dfba2d7587d3bf7092a1a70638174054d0051639333cd459bc"
	if got := fmt.Sprintf("%x", h.Sum(nil)); got != want {
		t.Fatalf("PCM SHA256 = %s, want %s", got, want)
	}
}
