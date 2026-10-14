// Package audio transforms interleaved floating-point PCM without clipping.
// Samples use full scale 1.0, and channel positions follow the supplied layout.
package audio

import (
	"errors"
	"fmt"
	"math"
	"math/big"
)

// Config selects PCM operations. ExtractChannel must be -1 to disable explicit
// extraction. ChannelMap uses zero-based source positions for each named output
// speaker position; it must be a full permutation of the input channels.
// Correction is a separate linear gain stage: zero means unity, as does one.
// DelayMS explicitly inserts silence or, when negative, removes starting frames.
// Mix "dplii" selects the signed stereo matrix. Phase shifting is a separate
// operation and is not included in this matrix.
type Config struct {
	InputLayout    string
	Channels       int
	SampleRate     int
	GainDB         float64
	Correction     float64
	DelayMS        float64
	Mix            string
	MixLFE         bool
	ChannelMap     []int
	ExtractChannel int
}

var ErrFinished = errors.New("audio processor is finished")

type contribution struct {
	channel int
	gain    float64
}

// Processor maintains delay and peak state across chunks. A Processor belongs
// to one stream and must not be called concurrently.
type Processor struct {
	inputChannels int
	outputLayout  string
	rows          [][]contribution
	channelMap    []int
	gain          float64
	correction    float64
	silenceFrames int64
	trimFrames    int64
	peak          float64
	finished      bool
}

func New(cfg Config) (*Processor, error) {
	if cfg.Channels < 1 || cfg.Channels > 64 {
		return nil, fmt.Errorf("invalid input channel count %d (supported range 1..64)", cfg.Channels)
	}
	if cfg.SampleRate <= 0 {
		return nil, fmt.Errorf("invalid sample rate %d", cfg.SampleRate)
	}
	if !finite(cfg.GainDB) || !finite(cfg.Correction) || !finite(cfg.DelayMS) {
		return nil, fmt.Errorf("gain, correction and delay must be finite")
	}
	if cfg.Correction < 0 {
		return nil, fmt.Errorf("correction must be positive, or zero for unity")
	}
	if cfg.ExtractChannel < -1 || cfg.ExtractChannel >= cfg.Channels {
		return nil, fmt.Errorf("extract channel %d is outside the input", cfg.ExtractChannel)
	}
	if cfg.ExtractChannel >= 0 && cfg.Mix != "" {
		return nil, fmt.Errorf("explicit channel extraction and mixing cannot be combined")
	}
	if cfg.MixLFE && cfg.Mix != "stereo" && cfg.Mix != "dplii" {
		return nil, fmt.Errorf("LFE mixing requires stereo downmix")
	}
	gain := math.Pow(10, cfg.GainDB/20)
	if !finite(gain) || gain == 0 {
		return nil, fmt.Errorf("gain is outside the finite floating-point range")
	}
	correction := cfg.Correction
	if correction == 0 {
		correction = 1
	}
	delayFrames, err := pcmDelayFrames(cfg.DelayMS, cfg.SampleRate)
	if err != nil {
		return nil, err
	}
	roles, layout, err := parseLayout(cfg.InputLayout, cfg.Channels)
	if err != nil {
		return nil, err
	}
	rows, outLayout, err := mixMatrix(cfg, roles, layout)
	if err != nil {
		return nil, err
	}
	channelMap := make([]int, cfg.Channels)
	for i := range channelMap {
		channelMap[i] = i
	}
	if len(cfg.ChannelMap) != 0 {
		if len(cfg.ChannelMap) != cfg.Channels {
			return nil, fmt.Errorf("channel map needs exactly %d source positions", cfg.Channels)
		}
		seen := make([]bool, cfg.Channels)
		for i, source := range cfg.ChannelMap {
			if source < 0 || source >= cfg.Channels || seen[source] {
				return nil, fmt.Errorf("channel map must contain each source position exactly once")
			}
			seen[source] = true
			channelMap[i] = source
		}
	}
	p := &Processor{
		inputChannels: cfg.Channels, outputLayout: outLayout, rows: rows,
		channelMap: channelMap, gain: gain, correction: correction,
	}
	if delayFrames > 0 {
		p.silenceFrames = delayFrames
	} else {
		p.trimFrames = -delayFrames
	}
	return p, nil
}

func (p *Processor) OutputChannels() int  { return len(p.rows) }
func (p *Processor) OutputLayout() string { return p.outputLayout }

// Peak is the largest absolute output sample with manual gain and channel
// mixing, but without Correction. Correction is actually applied before mixing,
// as a separate multiplication after manual gain. This counter cannot account
// for a resampler or other processing performed after this Processor.
func (p *Processor) Peak() float64 { return p.peak }

// DrainDelay returns up to 4096 frames of initial silence. Call it until it
// returns nil before calling Process, writing each returned block to the output.
// This keeps long positive delays independent of available memory. DelayMS is
// converted using 100 ns ticks and floor(sampleRate/100), with signed divisions
// truncating toward zero to whole sample frames.
func (p *Processor) DrainDelay() []float64 {
	if p.finished || p.silenceFrames == 0 {
		return nil
	}
	frames := min(p.silenceFrames, 4096)
	p.silenceFrames -= frames
	return make([]float64, int(frames)*len(p.rows))
}

// Process accepts whole interleaved input frames and returns owned output data.
// It preserves the input slice. Gain and correction multiply each input sample
// separately, in that order, before the channel matrix is summed. It never
// clamps samples to full scale. NaNs, infinities and arithmetic overflow fail
// the complete chunk without advancing delay or peak state.
func (p *Processor) Process(samples []float64) ([]float64, error) {
	if p.finished {
		return nil, ErrFinished
	}
	if p.silenceFrames > 0 {
		return nil, fmt.Errorf("initial silence remains: call DrainDelay until it returns nil")
	}
	if len(samples)%p.inputChannels != 0 {
		return nil, fmt.Errorf("%d samples do not form complete %d-channel frames", len(samples), p.inputChannels)
	}
	for i, sample := range samples {
		if !finite(sample) {
			return nil, fmt.Errorf("non-finite input sample at position %d", i)
		}
	}
	frames := len(samples) / p.inputChannels
	trim := min(p.trimFrames, int64(frames))
	frames -= int(trim)
	channels := len(p.rows)
	if frames > int(^uint(0)>>1)/channels {
		return nil, fmt.Errorf("output chunk size overflows memory addressing")
	}
	output := make([]float64, frames*channels)
	var gained, corrected [64]float64
	peak := p.peak
	for frame := 0; frame < frames; frame++ {
		base := (frame + int(trim)) * p.inputChannels
		for ch, source := range p.channelMap {
			gained[ch] = float64(samples[base+source] * p.gain)
			corrected[ch] = float64(gained[ch] * p.correction)
			if !finite(gained[ch]) || !finite(corrected[ch]) {
				return nil, fmt.Errorf("gain overflow at input frame %d, channel %d", frame+int(trim), source)
			}
		}
		for ch, row := range p.rows {
			uncorrected := sumRow(row, gained[:])
			value := uncorrected
			if p.correction != 1 {
				value = sumRow(row, corrected[:])
			}
			if !finite(uncorrected) || !finite(value) {
				return nil, fmt.Errorf("mix overflow at input frame %d, output channel %d", frame+int(trim), ch)
			}
			peak = math.Max(peak, math.Abs(uncorrected))
			output[frame*channels+ch] = value
		}
	}
	p.trimFrames -= trim
	p.peak = peak
	return output, nil
}

// Finish closes the processor. Whole-frame matrix operations have no buffered
// tail. It is safe to call Finish more than once.
func (p *Processor) Finish() ([]float64, error) {
	if p.silenceFrames > 0 {
		return nil, fmt.Errorf("initial silence remains: call DrainDelay until it returns nil")
	}
	p.finished = true
	return nil, nil
}

func sumRow(row []contribution, samples []float64) float64 {
	if len(row) == 0 {
		return 0
	}
	// Starting at the first contribution preserves signed zero in identity
	// and single-channel extraction routes.
	v := samples[row[0].channel] * row[0].gain
	for _, term := range row[1:] {
		product := float64(samples[term.channel] * term.gain)
		v += product
	}
	return v
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func pcmDelayFrames(milliseconds float64, sampleRate int) (int64, error) {
	if milliseconds == 0 {
		return 0, nil
	}
	if sampleRate < 100 {
		return 0, fmt.Errorf("PCM delay requires a sample rate of at least 100 Hz")
	}
	ticks := math.Trunc(milliseconds * 10000)
	if !finite(ticks) || math.Abs(ticks) >= 0x1p63 {
		return 0, fmt.Errorf("delay is outside the supported timestamp range")
	}
	// Wider multiplication prevents intermediate overflow for long offsets.
	// Two consecutive positive-divisor truncations equal one by their product.
	var value big.Int
	value.Mul(big.NewInt(int64(ticks)), big.NewInt(int64(sampleRate/100)))
	value.Quo(&value, big.NewInt(100000))
	if !value.IsInt64() || value.Int64() == math.MinInt64 {
		return 0, fmt.Errorf("delay is outside the supported frame-count range")
	}
	return value.Int64(), nil
}
