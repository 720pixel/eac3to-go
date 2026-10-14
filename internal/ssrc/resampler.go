// Package ssrc wraps the double-precision SSRC algorithm shipped with eac3to.
// The input and output slices contain interleaved floating-point PCM frames.
package ssrc

import (
	"fmt"
	"math"
	"runtime"
	"sync"
)

type Config struct {
	Attenuation    float64
	TransitionBand float64
	FFTLength      int
}

// DefaultConfig uses the filter parameters of eac3to 4.00's default SSRC path.
func DefaultConfig() Config { return Config{Attenuation: 200, TransitionBand: 500, FFTLength: 8192} }

// SteepConfig uses the filter parameters selected by eac3to 4.00's -steep switch.
func SteepConfig() Config { return Config{Attenuation: 170, TransitionBand: 100, FFTLength: 65536} }

type backend interface {
	write([]float64) error
	read() ([]float64, error)
	finish() error
	drainInputFrames() uint64
	close()
}

type Resampler struct {
	mu           sync.Mutex
	core         backend
	inRate       int
	outRate      int
	effectiveOut float64
	channels     int
	inputFrames  uint64
	outputFrames uint64
	pending      []float64
	finished     bool
	closed       bool
}

func New(inRate, outRate, channels int) (*Resampler, error) {
	return NewWithConfig(inRate, outRate, channels, DefaultConfig())
}

func NewWithConfig(inRate, outRate, channels int, config Config) (*Resampler, error) {
	return NewWithOutputRate(inRate, float64(outRate), channels, config)
}

// NewWithOutputRate rounds the filter rate to a whole hertz, while retaining
// the requested rate for the final frame count, as the Windows SSRC wrapper does.
func NewWithOutputRate(inRate int, effectiveOut float64, channels int, config Config) (*Resampler, error) {
	if !finite(effectiveOut) || effectiveOut < 1 || effectiveOut > 768000 {
		return nil, fmt.Errorf("SSRC output rate must be between 1 and 768000 Hz")
	}
	outRate := int(math.RoundToEven(effectiveOut))
	if inRate < 1 || inRate > 768000 || outRate < 1 || outRate > 768000 {
		return nil, fmt.Errorf("SSRC sample rates must be between 1 and 768000 Hz")
	}
	if inRate == outRate && effectiveOut != float64(inRate) {
		return nil, fmt.Errorf("SSRC fractional output rate %.9f rounds to the input rate; no resampling filter can be selected", effectiveOut)
	}
	if channels < 1 || channels > 32 {
		return nil, fmt.Errorf("SSRC channel count must be between 1 and 32")
	}
	if !finite(config.Attenuation) || config.Attenuation < 21 || config.Attenuation > 300 || !finite(config.TransitionBand) || config.TransitionBand < 10 || config.TransitionBand > 100000 || config.FFTLength < 32 || config.FFTLength > 65536 || config.FFTLength&(config.FFTLength-1) != 0 {
		return nil, fmt.Errorf("invalid SSRC filter configuration")
	}
	// The original core uses signed 32-bit rates and phase-table indices.
	gcd := greatestCommonDivisor(inRate, outRate)
	if int64(inRate/gcd)*int64(outRate) > math.MaxInt32 {
		return nil, fmt.Errorf("SSRC rate ratio %d:%d exceeds the core's phase range", inRate, outRate)
	}
	r := &Resampler{inRate: inRate, outRate: outRate, effectiveOut: effectiveOut, channels: channels}
	if inRate != outRate {
		core, err := newBackend(inRate, outRate, channels, config)
		if err != nil {
			return nil, err
		}
		r.core = core
	}
	runtime.SetFinalizer(r, func(r *Resampler) { _ = r.Close() })
	return r, nil
}

// Write accepts complete frames. It copies input before returning and emits
// available output; a short write may produce no output until more arrives.
func (r *Resampler) Write(input []float64) ([]float64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || r.finished {
		return nil, fmt.Errorf("SSRC resampler is closed or finished")
	}
	if len(input)%r.channels != 0 {
		return nil, fmt.Errorf("SSRC input has %d samples, not a whole number of %d-channel frames", len(input), r.channels)
	}
	for _, sample := range input {
		if !finite(sample) {
			return nil, fmt.Errorf("SSRC input contains a non-finite sample")
		}
	}
	frames := uint64(len(input) / r.channels)
	if r.inputFrames+frames < r.inputFrames || r.inputFrames+frames > math.MaxUint64/uint64(math.Ceil(r.effectiveOut)) {
		return nil, fmt.Errorf("SSRC input exceeds the sample counter range")
	}
	r.inputFrames += frames
	if r.core == nil {
		r.outputFrames += frames
		return append([]float64(nil), input...), nil
	}
	const blockFrames = 32768
	for len(input) > 0 {
		count := blockFrames * r.channels
		if count > len(input) {
			count = len(input)
		}
		if err := r.core.write(input[:count]); err != nil {
			return nil, err
		}
		output, err := r.core.read()
		if err != nil {
			return nil, err
		}
		r.pending = append(r.pending, output...)
		input = input[count:]
	}
	return r.takeOutput(r.expectedFrames()), nil
}

// Finish flushes the native filter and limits output to the nearest whole frame
// count, rounding half upward. The original filter's terminal-block behavior
// is retained: a short native tail is not padded. Repeated calls return no
// further output. Call Close after Finish to release the filter buffers.
func (r *Resampler) Finish() ([]float64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, fmt.Errorf("SSRC resampler is closed")
	}
	if r.finished {
		return nil, nil
	}
	r.finished = true
	if err := r.flush(); err != nil {
		return nil, err
	}
	result := r.takeOutput(r.expectedFrames())
	r.pending = nil
	return result, nil
}

// FinishComplete advances the original filter with zero input until it has
// emitted the nearest whole output-frame count for the supplied audio. This
// preserves the final samples that Finish can leave in the filter's overlap
// buffers. The zero extension is filtered; output samples are never fabricated
// or appended as silence. InputFrames continues to count caller input only.
// Choose one finishing method; a finished resampler cannot be drained again.
func (r *Resampler) FinishComplete() ([]float64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, fmt.Errorf("SSRC resampler is closed")
	}
	if r.finished {
		return nil, nil
	}
	r.finished = true
	target := r.expectedFrames()
	if r.core != nil {
		// Bound both the source write and its approximate output allocation.
		block := min(4096, max(1, 32768*r.inRate/r.outRate))
		zero := make([]float64, block*r.channels)
		remaining := r.core.drainInputFrames()
		for r.outputFrames+uint64(len(r.pending)/r.channels) < target {
			if remaining == 0 {
				return nil, fmt.Errorf("SSRC filter did not drain within its filter-length bound")
			}
			frames := min(uint64(block), remaining)
			if err := r.core.write(zero[:int(frames)*r.channels]); err != nil {
				return nil, err
			}
			output, err := r.core.read()
			if err != nil {
				return nil, err
			}
			r.pending = append(r.pending, output...)
			remaining -= frames
		}
	}
	result := r.takeOutput(target)
	r.pending = nil
	return result, nil
}

// FinishRaw flushes the filter without limiting the final frame count. This is
// useful when a caller must cap by an effective rate different from the integer
// filter rate, as in a combined speed and sample-rate conversion. The returned
// tail may include zero-padded filter output. Use either Finish or FinishRaw.
func (r *Resampler) FinishRaw() ([]float64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, fmt.Errorf("SSRC resampler is closed")
	}
	if r.finished {
		return nil, nil
	}
	r.finished = true
	if err := r.flush(); err != nil {
		return nil, err
	}
	result := r.pending
	r.pending = nil
	r.outputFrames += uint64(len(result) / r.channels)
	return result, nil
}

func (r *Resampler) flush() error {
	if r.core == nil {
		return nil
	}
	if err := r.core.finish(); err != nil {
		return err
	}
	output, err := r.core.read()
	if err != nil {
		return err
	}
	r.pending = append(r.pending, output...)
	return nil
}

// Close releases memory. It discards any unconsumed tail; use Finish to obtain it.
func (r *Resampler) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	if r.core != nil {
		r.core.close()
		r.core = nil
	}
	r.pending = nil
	runtime.SetFinalizer(r, nil)
	return nil
}

func (r *Resampler) InputFrames() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inputFrames
}

func (r *Resampler) OutputFrames() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.outputFrames
}

// ExpectedFrames is the requested presentation length before the original
// filter's terminal-block limitation. Callers can reject an incomplete tail.
func (r *Resampler) ExpectedFrames() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.expectedFrames()
}

func (r *Resampler) expectedFrames() uint64 {
	if r.effectiveOut != float64(r.outRate) {
		return uint64(math.Floor(float64(r.inputFrames)*r.effectiveOut/float64(r.inRate) + 0.5))
	}
	product := r.inputFrames * uint64(r.outRate)
	quotient, remainder := product/uint64(r.inRate), product%uint64(r.inRate)
	if remainder*2 >= uint64(r.inRate) {
		quotient++
	}
	return quotient
}

func (r *Resampler) takeOutput(limit uint64) []float64 {
	if r.outputFrames >= limit {
		return nil
	}
	frames := uint64(len(r.pending) / r.channels)
	if frames > limit-r.outputFrames {
		frames = limit - r.outputFrames
	}
	count := int(frames) * r.channels
	if count == 0 {
		return nil
	}
	result := append([]float64(nil), r.pending[:count]...)
	r.pending = r.pending[count:]
	if len(r.pending) == 0 {
		r.pending = nil
	}
	r.outputFrames += frames
	return result
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func greatestCommonDivisor(a, b int) int {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}
