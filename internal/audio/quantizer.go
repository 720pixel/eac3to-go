package audio

import (
	"fmt"
	"math"
)

// QuantizerBatchSamples is the maximum number of interleaved scalar samples in
// one dither batch. A batch consumes one more random draw than it has samples.
const QuantizerBatchSamples = 65536

// Quantizer converts normalized PCM to left-aligned signed 32-bit samples.
// Write the results as s32le; the lowest 32-depth bits are always zero. One
// Quantizer belongs to one stream and must not be called concurrently.
//
// Dither uses adjacent differences of 30-bit MT19937 values. Consequently its
// sequence depends on Process call boundaries, as well as the seed. Each call
// starts fresh batches of at most QuantizerBatchSamples scalar samples. There
// is no special suppression of dither on digital silence.
type Quantizer struct {
	inputBits int
	depth     int
	floating  bool
	dither    bool
	mt        mt19937
}

// NewQuantizer converts floating-point samples, first rounding sample*2^31 to
// the nearest even integer. Reduction then adds half an output step and uses
// arithmetic shifts. These are two separate rounding operations.
//
// The compatible float conversion retains the low 32 bits and clamps when the
// integer sign disagrees with a sample outside [-0.5,0.5]. This is not a general
// saturating conversion: for example, +2.0 becomes zero. Callers wanting bounded
// output should correct the gain before conversion. Magnitudes outside the
// signed 64-bit intermediate range, NaNs and infinities are rejected.
func NewQuantizer(depth int, dither bool, seed uint32) (*Quantizer, error) {
	return NewQuantizerWithSeedKey(depth, dither, []uint32{seed})
}

// NewQuantizerWithSeedKey selects the MT19937 init_by_array seed directly. The
// key must not be empty. A fixed key makes output reproducible for fixed input
// and Process call boundaries.
func NewQuantizerWithSeedKey(depth int, dither bool, key []uint32) (*Quantizer, error) {
	return newQuantizer(32, depth, true, dither, key)
}

// NewPCMQuantizer preserves the integer reduction rules for original 8-, 16-,
// 24- or 32-bit PCM. Its normalized input must lie exactly on the declared
// source grid, with full scale 2^(inputBits-1). Use NewQuantizer after processing
// that changes sample values. Increasing depth preserves all samples exactly
// and does not add or consume dither.
func NewPCMQuantizer(inputBits, depth int, dither bool, seed uint32) (*Quantizer, error) {
	return NewPCMQuantizerWithSeedKey(inputBits, depth, dither, []uint32{seed})
}

// NewPCMQuantizerWithSeedKey is NewPCMQuantizer with an explicit nonempty
// MT19937 init_by_array key.
func NewPCMQuantizerWithSeedKey(inputBits, depth int, dither bool, key []uint32) (*Quantizer, error) {
	return newQuantizer(inputBits, depth, false, dither, key)
}

func newQuantizer(inputBits, depth int, floating, dither bool, key []uint32) (*Quantizer, error) {
	if depth < 8 || depth > 32 {
		return nil, fmt.Errorf("output depth %d is outside 8..32 bits", depth)
	}
	if inputBits != 8 && inputBits != 16 && inputBits != 24 && inputBits != 32 {
		return nil, fmt.Errorf("integer input storage must be 8, 16, 24 or 32 bits")
	}
	if len(key) == 0 {
		return nil, fmt.Errorf("dither seed key must not be empty")
	}
	return &Quantizer{inputBits: inputBits, depth: depth, floating: floating,
		dither: dither, mt: newMT19937(key)}, nil
}

// Process preserves its input and returns owned output. Invalid samples fail
// the whole call without advancing the random sequence. No channel-specific
// random state is used: samples are processed in their supplied interleaved
// order. Float conversion to 32 bits consumes dither draws when enabled, but
// does not apply noise because it has no integer bits left to discard.
func (q *Quantizer) Process(samples []float64) ([]int32, error) {
	if len(samples) == 0 {
		return nil, nil
	}
	output := make([]int32, len(samples))
	scale := math.Ldexp(1, q.inputBits-1)
	for i, sample := range samples {
		if !finite(sample) {
			return nil, fmt.Errorf("non-finite input sample at position %d", i)
		}
		if q.floating {
			scaled := math.RoundToEven(sample * 2147483648)
			if scaled < -9223372036854775808 || scaled >= 9223372036854775808 {
				return nil, fmt.Errorf("sample at position %d exceeds the signed 64-bit conversion range", i)
			}
			value := int32(int64(scaled))
			if (sample < 0) != (value < 0) {
				if sample < -0.5 {
					value = math.MinInt32
				} else if sample > 0.5 {
					value = math.MaxInt32
				}
			}
			output[i] = value
		} else {
			scaled := sample * scale
			if scaled < -scale || scaled >= scale || scaled != math.Trunc(scaled) {
				return nil, fmt.Errorf("sample at position %d is not an exact signed %d-bit PCM value", i, q.inputBits)
			}
			output[i] = int32(scaled)
		}
	}
	if q.depth >= q.inputBits && !q.floating {
		for i, value := range output {
			output[i] = value << (32 - q.inputBits)
		}
		return output, nil
	}

	discard := q.inputBits - q.depth
	for start := 0; start < len(output); start += QuantizerBatchSamples {
		end := min(start+QuantizerBatchSamples, len(output))
		var previous int64
		if q.dither {
			previous = int64(q.mt.next() >> 2)
		}
		for i := start; i < end; i++ {
			var noise int64
			if q.dither {
				current := int64(q.mt.next() >> 2)
				noise = (current - previous) >> (30 - discard)
				previous = current
			}
			if discard == 0 {
				continue
			}
			value := quantizeInteger(int64(output[i]), noise, q.inputBits, discard)
			output[i] = int32(value << (32 - q.depth))
		}
	}
	return output, nil
}

// The 16- and 24-bit paths clamp after adding noise and half a step. The 32-bit
// path instead restores the original source on each overflowing addition,
// including when only the second addition overflows.
func quantizeInteger(source, noise int64, inputBits, discard int) int64 {
	value := source + noise
	half := int64(1) << (discard - 1)
	if inputBits == 32 {
		if value < math.MinInt32 || value > math.MaxInt32 {
			value = source
		}
		value += half
		if value < math.MinInt32 || value > math.MaxInt32 {
			value = source
		}
	} else {
		minimum := -int64(1) << (inputBits - 1)
		maximum := -minimum - 1
		value = min(max(value+half, minimum), maximum)
	}
	return value >> discard
}
