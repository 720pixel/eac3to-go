package aften

import (
	"errors"
	"fmt"
	"io"
	"math"
	"math/bits"
	"runtime"
	"sync"
)

const samplesPerFrame = 1536

var (
	ErrConfig           = errors.New("unsupported Aften encoder configuration")
	ErrClosed           = errors.New("Aften encoder is finished or closed")
	ErrInvalidPCM       = errors.New("Aften requires finite, interleaved whole PCM sample frames")
	ErrCoefficientRange = errors.New("Aften cannot represent this PCM level; normalize the audio or reduce its gain")
	ErrNoSamples        = errors.New("Aften received no PCM samples")
)

// Config describes normalized PCM in ascending WAVE speaker-mask order.
// ChannelMask zero selects the conventional WAVE layout for Channels.
// BitrateKbps zero selects 448 for mono/stereo and 640 for larger layouts.
// No sample-rate conversion or downmix is performed.
type Config struct {
	SampleRate  int
	Channels    int
	ChannelMask uint32
	BitrateKbps int
	// FrameContexts preserves the historical per-worker frame-size and bit
	// allocation state. Zero uses the logical CPU count, capped at 32.
	// This controls bitstream decisions, not the number of operating-system
	// threads. Independent Encoder instances can encode concurrently.
	FrameContexts int
}

type encoderConfig struct {
	Config
	acmod int
	lfe   int
	order [6]int
}

// ValidateConfig checks the configuration without allocating an encoder or
// writing output. Use Available to check whether the C backend was built.
func ValidateConfig(c Config) error {
	_, err := resolveConfig(c)
	return err
}

func resolveConfig(c Config) (encoderConfig, error) {
	if c.SampleRate != 32000 && c.SampleRate != 44100 && c.SampleRate != 48000 {
		return encoderConfig{}, fmt.Errorf("%w: sample rate %d; use 32000, 44100 or 48000 Hz", ErrConfig, c.SampleRate)
	}
	if c.Channels < 1 || c.Channels > 6 {
		return encoderConfig{}, fmt.Errorf("%w: %d channels; AC-3 requires one to six channels", ErrConfig, c.Channels)
	}
	if c.ChannelMask == 0 {
		c.ChannelMask = [...]uint32{0, 4, 3, 7, 0x33, 0x37, 0x3f}[c.Channels]
	}
	if bits.OnesCount32(c.ChannelMask) != c.Channels {
		return encoderConfig{}, fmt.Errorf("%w: channel mask %#x does not describe %d channels", ErrConfig, c.ChannelMask, c.Channels)
	}
	if c.BitrateKbps == 0 {
		c.BitrateKbps = 448
		if c.Channels > 2 {
			c.BitrateKbps = 640
		}
	}
	if c.FrameContexts == 0 {
		c.FrameContexts = min(runtime.NumCPU(), 32)
	}
	if c.FrameContexts < 1 || c.FrameContexts > 32 {
		return encoderConfig{}, fmt.Errorf("%w: frame contexts must be between 1 and 32", ErrConfig)
	}
	switch c.BitrateKbps {
	case 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384, 448, 512, 576, 640:
	default:
		return encoderConfig{}, fmt.Errorf("%w: bitrate %d kbps", ErrConfig, c.BitrateKbps)
	}
	resolved := encoderConfig{Config: c}
	mask := c.ChannelMask &^ 8
	if c.ChannelMask&8 != 0 {
		resolved.lfe = 1
	}
	var speakers []uint32
	switch mask {
	case 4:
		resolved.acmod, speakers = 1, []uint32{4}
	case 3:
		resolved.acmod, speakers = 2, []uint32{1, 2}
	case 7:
		resolved.acmod, speakers = 3, []uint32{1, 4, 2}
	case 0x103:
		resolved.acmod, speakers = 4, []uint32{1, 2, 0x100}
	case 0x107:
		resolved.acmod, speakers = 5, []uint32{1, 4, 2, 0x100}
	case 0x33:
		resolved.acmod, speakers = 6, []uint32{1, 2, 0x10, 0x20}
	case 0x603:
		resolved.acmod, speakers = 6, []uint32{1, 2, 0x200, 0x400}
	case 0x37:
		resolved.acmod, speakers = 7, []uint32{1, 4, 2, 0x10, 0x20}
	case 0x607:
		resolved.acmod, speakers = 7, []uint32{1, 4, 2, 0x200, 0x400}
	default:
		return encoderConfig{}, fmt.Errorf("%w: channel mask %#x", ErrConfig, c.ChannelMask)
	}
	if resolved.lfe != 0 {
		speakers = append(speakers, 8)
	}
	for i, speaker := range speakers {
		resolved.order[i] = bits.OnesCount32(c.ChannelMask & (speaker - 1))
	}
	return resolved, nil
}

// Encoder writes AC-3 frames to a caller-owned writer. The writer is not closed.
// Each instance retains at most 1536 PCM sample frames. Different encoders can
// be used concurrently. Initialization and destruction exclude frame encoding
// while shared lookup tables are being initialized; ordinary frame encoding
// on independent contexts runs concurrently.
type Encoder struct {
	mu       sync.Mutex
	config   encoderConfig
	native   *nativeEncoder
	dst      io.Writer
	buffer   [samplesPerFrame * 6]float64
	output   [3840]byte
	pending  int
	wrote    bool
	finished bool
	closed   bool
	err      error
}

func NewEncoder(dst io.Writer, c Config) (*Encoder, error) {
	config, err := resolveConfig(c)
	if err != nil {
		return nil, err
	}
	if dst == nil {
		return nil, fmt.Errorf("Aften requires an output writer")
	}
	native, err := newNativeEncoder(config)
	if err != nil {
		return nil, err
	}
	e := &Encoder{config: config, native: native, dst: dst}
	runtime.SetFinalizer(e, func(e *Encoder) { _ = e.Close() })
	return e, nil
}

// Write accepts normalized float64 PCM without changing the input slice.
// Each call must contain complete interleaved sample frames. Finite samples
// outside [-1,1] remain unchanged. If the resulting transform coefficients
// exceed the encoder's representation, Write returns ErrCoefficientRange.
// NaN and infinity are rejected before encoding.
func (e *Encoder) Write(samples []float64) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.err != nil {
		return e.err
	}
	if e.closed || e.finished {
		return ErrClosed
	}
	channels := e.config.Channels
	if len(samples)%channels != 0 {
		return ErrInvalidPCM
	}
	for _, sample := range samples {
		if math.IsNaN(sample) || math.IsInf(sample, 0) {
			return ErrInvalidPCM
		}
	}
	for len(samples) != 0 {
		e.wrote = true
		frames := min(samplesPerFrame-e.pending/channels, len(samples)/channels)
		for i := 0; i < frames; i++ {
			for ch := 0; ch < channels; ch++ {
				e.buffer[e.pending+i*channels+ch] = samples[i*channels+e.config.order[ch]]
			}
		}
		e.pending += frames * channels
		samples = samples[frames*channels:]
		if e.pending == samplesPerFrame*channels {
			if _, err := e.encode(e.buffer[:e.pending]); err != nil {
				return err
			}
			e.pending = 0
		}
	}
	return nil
}

// Finish writes the final partial frame and drains the encoder's 256-sample
// transform delay. It frees the encoder and is idempotent. AC-3 frame padding
// is preserved; callers must not discard the final encoded frame.
func (e *Encoder) Finish() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.err != nil {
		e.release()
		return e.err
	}
	if e.finished {
		return nil
	}
	if e.closed {
		return ErrClosed
	}
	e.finished = true
	defer e.release()
	if !e.wrote {
		e.err = ErrNoSamples
		return e.err
	}
	if e.pending != 0 {
		if _, err := e.encode(e.buffer[:e.pending]); err != nil {
			return err
		}
		e.pending = 0
	}
	// The scalar encoder emits at most one additional frame. Bound this loop
	// as well so a broken backend cannot write an unending stream of silence.
	for attempts := 0; attempts < 3; attempts++ {
		n, err := e.encode(nil)
		if err != nil || n == 0 {
			return err
		}
	}
	e.err = errors.New("Aften did not finish draining")
	return e.err
}

// Close frees an encoder without flushing buffered PCM, for cancellation or
// errors. Call Finish to complete a successful stream. Close is idempotent.
func (e *Encoder) Close() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.closed = true
	e.release()
	return e.err
}

func (e *Encoder) release() {
	if e.native != nil {
		e.native.close()
		e.native = nil
		runtime.SetFinalizer(e, nil)
	}
}

func (e *Encoder) encode(samples []float64) (int, error) {
	n, err := e.native.encode(samples, e.output[:])
	if err == nil && n != 0 {
		var written int
		written, err = e.dst.Write(e.output[:n])
		if err == nil && written != n {
			err = io.ErrShortWrite
		}
	}
	if err != nil {
		e.err = err
	}
	return n, err
}
