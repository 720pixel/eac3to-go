package audio

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"math"
	"reflect"
	"testing"
)

func TestQuantizerFloatNearestEvenAndLegacyOverflow(t *testing.T) {
	q, err := NewQuantizer(32, false, 0)
	if err != nil {
		t.Fatal(err)
	}
	scale := float64(uint64(1) << 31)
	input := []float64{0.5 / scale, 1.5 / scale, 2.5 / scale, -0.5 / scale, -1.5 / scale,
		-3, -2.5, -2, -1.5, -1, -0.5, math.Copysign(0, -1), 0, 0.5, 1, 1.5, 2, 2.5, 3, 3.5, 4}
	want := []int32{0, 2, 2, 0, -2,
		math.MinInt32, -1073741824, math.MinInt32, math.MinInt32, math.MinInt32, -1073741824,
		0, 0, 1073741824, math.MaxInt32, math.MaxInt32, 0, 1073741824, math.MaxInt32, math.MaxInt32, 0}
	got, err := q.Process(input)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, %v; want %v", got, err, want)
	}
}

func TestQuantizerSeparateRoundingSteps(t *testing.T) {
	q, _ := NewQuantizer(16, false, 0)
	scale := float64(uint64(1) << 31)
	input := []float64{(32768 - 0.5) / scale, (32768 - 0.5001) / scale,
		(-32768 - 0.5) / scale, (-32768 - 0.5001) / scale, 32768 / scale,
		-32768 / scale, -32769 / scale, 98304 / scale, -98304 / scale}
	want := []int32{65536, 0, 0, -65536, 65536, 0, -65536, 131072, -65536}
	got, err := q.Process(input)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, %v; want %v", got, err, want)
	}
}

func TestQuantizerIntegerOverflowPolicies(t *testing.T) {
	cases := []struct {
		name          string
		source, noise int64
		bits, discard int
		want          int64
	}{
		{"16-bit upper clamp", 32760, 127, 16, 8, 127},
		{"16-bit lower clamp", -32760, -256, 16, 8, -128},
		{"24-bit upper clamp", 8388607, 255, 24, 8, 32767},
		{"24-bit lower clamp", -8388607, -256, 24, 8, -32768},
		{"32-bit noise overflow restores source", math.MinInt32 + 15, -16, 32, 4, -134217727},
		{"32-bit rounding overflow restores original", math.MaxInt32 - 15, 15, 32, 4, 134217727},
		{"32-bit rounding overflow upper endpoint", math.MaxInt32, 0, 32, 16, 32767},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := quantizeInteger(tc.source, tc.noise, tc.bits, tc.discard); got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}
}

func TestQuantizerDitherSilenceVector(t *testing.T) {
	q, _ := NewQuantizerWithSeedKey(16, true, []uint32{0x123, 0x234, 0x345, 0x456})
	want := []int32{0, 0, 1, 0, 0, 0, -1, 0, 0, 0, 0, -1, 0, 1, -1, 1,
		0, 0, 0, 0, 0, 0, 0, 0, 0, -1, 1, 0, 0, 0, 0, 0}
	got, err := q.Process(make([]float64, len(want)))
	if err != nil {
		t.Fatal(err)
	}
	for i, value := range want {
		if got[i] != value<<16 {
			t.Fatalf("sample %d = %d, want %d", i, got[i], value<<16)
		}
	}
}

func TestQuantizerDitherReferenceDigests(t *testing.T) {
	// Independent integer arithmetic and CPython MT19937 produce these hashes.
	// The 65539-sample input crosses the 65536-sample random batch boundary.
	cases := []struct {
		width, depth int
		hash         string
	}{
		{16, 8, "f70eff7a5e73cf3f9f6f19cc5a356e700374a3040c051155706bf6505ef7027a"},
		{24, 9, "3f4ae4f688b4df7acefb6c94b855f9a03755024e6e365966aef3d9299a660814"},
		{24, 16, "bb35f62e86e40a2a92effc153b30aad386c8c88559c102055c5346673f6dc912"},
		{32, 8, "aa9a7b4a8243e67716465a82b1f8f8e97dae8f0c6b1b327d27c0949d4a839549"},
		{32, 16, "3e9bafde78a1874529a4f5915544c8a0d99eefdf2c5065d06a6aab35027ed95e"},
		{32, 20, "d213da937d2d4f7febf72143bae2ef782529dcc75f208782216b7912af13ff90"},
		{32, 24, "a091aee410de61b95d9d383ffd516b315f23f5ec0fc24b9d7c7d41d00549f1f8"},
		{32, 31, "034475e1e200f85d47a26470f8289f4cf6bafa0596e89b9235fc79b6ac3a09b8"},
		{32, 32, "d03d0fc939708a18e6e6e851f8b3579d6cf7f35497759a6909e2d776cb88439f"},
	}
	for _, tc := range cases {
		q, err := NewPCMQuantizerWithSeedKey(tc.width, tc.depth, true, []uint32{0x123, 0x234, 0x345, 0x456})
		if err != nil {
			t.Fatal(err)
		}
		samples := quantizerFixture(tc.width, 65539)
		got, err := q.Process(samples)
		if err != nil {
			t.Fatal(err)
		}
		if digest := pcmDigest(got); digest != tc.hash {
			t.Errorf("%d to %d: got %s, want %s", tc.width, tc.depth, digest, tc.hash)
		}
		if tc.width == 32 {
			floating, _ := NewQuantizerWithSeedKey(tc.depth, true, []uint32{0x123, 0x234, 0x345, 0x456})
			floatResult, err := floating.Process(samples)
			if err != nil || !reflect.DeepEqual(floatResult, got) {
				t.Errorf("float to %d differs from exact integer input: %v", tc.depth, err)
			}
		}
	}
}

func TestQuantizerDepthsAndPrecisionIncrease(t *testing.T) {
	for _, width := range []int{8, 16, 24, 32} {
		samples := quantizerFixture(width, 1000)
		for depth := 8; depth <= 32; depth++ {
			q, _ := NewPCMQuantizer(width, depth, true, 1)
			got, err := q.Process(samples)
			if err != nil {
				t.Fatalf("%d to %d: %v", width, depth, err)
			}
			mask := uint32(1)<<(32-depth) - 1
			for i, value := range got {
				if uint32(value)&mask != 0 {
					t.Fatalf("%d to %d leaves nonzero padding at %d", width, depth, i)
				}
				if depth >= width && float64(value)/2147483648 != samples[i] {
					t.Fatalf("increasing %d to %d changes sample %d", width, depth, i)
				}
			}
			if depth >= width && q.mt != newMT19937([]uint32{1}) {
				t.Fatalf("increasing %d to %d consumed random numbers", width, depth)
			}
		}
	}
}

func TestQuantizerBatchCadenceAndChunking(t *testing.T) {
	samples := make([]float64, QuantizerBatchSamples+29)
	for _, depth := range []int{16, 32} {
		q, _ := NewQuantizer(depth, true, 123)
		if _, err := q.Process(samples); err != nil {
			t.Fatal(err)
		}
		mt := newMT19937([]uint32{123})
		for i := 0; i < len(samples)+2; i++ {
			mt.next()
		}
		if q.mt != mt {
			t.Fatalf("depth %d did not consume N+1 random draws per batch", depth)
		}
	}
	for _, dither := range []bool{false, true} {
		samples := quantizerFixture(32, 1000)
		whole, _ := NewQuantizer(16, dither, 123)
		chunks, _ := NewQuantizer(16, dither, 123)
		want, _ := whole.Process(samples)
		got, _ := chunks.Process(samples[:500])
		tail, _ := chunks.Process(samples[500:])
		got = append(got, tail...)
		if reflect.DeepEqual(got, want) == dither {
			t.Fatalf("dither=%v: unexpected chunk dependence", dither)
		}
		if !reflect.DeepEqual(got[:500], want[:500]) {
			t.Fatal("chunk boundary affected preceding samples")
		}
	}
}

func TestQuantizerValidationAndAtomicFailure(t *testing.T) {
	for _, depth := range []int{-1, 0, 7, 33, 64} {
		if _, err := NewQuantizer(depth, false, 0); err == nil {
			t.Errorf("depth %d accepted", depth)
		}
	}
	for _, width := range []int{0, 12, 20, 31, 64} {
		if _, err := NewPCMQuantizer(width, 16, false, 0); err == nil {
			t.Errorf("source width %d accepted", width)
		}
	}
	if _, err := NewQuantizerWithSeedKey(16, false, nil); err == nil {
		t.Error("empty seed accepted")
	}
	for _, floating := range []bool{false, true} {
		q, _ := NewPCMQuantizer(16, 8, true, 44)
		invalid := []float64{math.NaN(), math.Inf(-1), math.Inf(1), 1, -1.0001, 0.1}
		if floating {
			q, _ = NewQuantizer(16, true, 44)
			invalid = []float64{math.NaN(), math.Inf(-1), math.Inf(1), math.Ldexp(1, 32), -math.Ldexp(1, 33)}
		}
		state := q.mt
		for _, sample := range invalid {
			input := make([]float64, QuantizerBatchSamples+3)
			input[len(input)-1] = sample
			if output, err := q.Process(input); err == nil || output != nil {
				t.Errorf("floating=%v: invalid sample %g returned output=%v, error=%v", floating, sample, output, err)
			}
			if q.mt != state {
				t.Fatal("failed chunk advanced random sequence")
			}
		}
		if output, err := q.Process(nil); err != nil || len(output) != 0 || q.mt != state {
			t.Fatal("empty chunk changed state")
		}
	}
}

func quantizerFixture(width, count int) []float64 {
	result := make([]float64, count)
	mask := uint64(1)<<width - 1
	scale := int64(1) << (width - 1)
	for i := range result {
		value := int64((uint64(i)*1664525 + 1013904223) & mask)
		if value >= scale {
			value -= 2 * scale
		}
		result[i] = float64(value) / float64(scale)
	}
	for i, value := range []int64{-scale, -scale + 1, -1, 0, 1, scale - 2, scale - 1, 0} {
		if i < len(result) {
			result[i] = float64(value) / float64(scale)
		}
	}
	return result
}

func pcmDigest(values []int32) string {
	digest := sha256.New()
	var word [4]byte
	for _, value := range values {
		binary.LittleEndian.PutUint32(word[:], uint32(value))
		digest.Write(word[:])
	}
	return hex.EncodeToString(digest.Sum(nil))
}
