package truehd

import "fmt"

type PostprocessConfig struct {
	OnFrame func(Frame) error
	// WindowsSeamless selects the original eac3to multi-segment TrueHD
	// postprocess. It is not an assertion of decoded continuity or A/V sync.
	WindowsSeamless bool
	MaxFrameBytes   int
}

type PostprocessStats struct {
	InputFrames, EmittedFrames, SuppressedFrames int64
	InputBytes, EmittedBytes, SuppressedBytes    int64
}

// Postprocessor holds two access units in WindowsSeamless mode. When the
// incoming and immediately preceding units both have a major sync, it
// suppresses the unit two positions earlier, regardless of byte identity.
// It retains both major units. This reproduces eac3to's final THD pass and
// deliberately does not use the AC3/DTS identical-frame boundary policy.
type Postprocessor struct {
	cfg      PostprocessConfig
	history  [2]Frame
	storage  [2][]byte
	n        int
	stats    PostprocessStats
	failure  error
	finished bool
}

func NewPostprocessor(cfg PostprocessConfig) (*Postprocessor, error) {
	if cfg.OnFrame == nil {
		return nil, fmt.Errorf("a TrueHD postprocess handler is required")
	}
	if cfg.MaxFrameBytes == 0 {
		cfg.MaxFrameBytes = MaxAccessUnitBytes
	}
	if cfg.MaxFrameBytes < 6 || cfg.MaxFrameBytes > MaxAccessUnitBytes {
		return nil, fmt.Errorf("invalid TrueHD postprocess frame bound")
	}
	p := &Postprocessor{cfg: cfg}
	if cfg.WindowsSeamless {
		p.storage[0] = make([]byte, cfg.MaxFrameBytes)
		p.storage[1] = make([]byte, cfg.MaxFrameBytes)
	}
	return p, nil
}

func (p *Postprocessor) Stats() PostprocessStats { return p.stats }
func (p *Postprocessor) PendingFrames() int      { return p.n }

func (p *Postprocessor) Accept(frame Frame) error {
	if p.failure != nil {
		return p.failure
	}
	if p.finished {
		return fmt.Errorf("TrueHD postprocessor is finished")
	}
	if len(frame.Bytes) < 6 || len(frame.Bytes) > p.cfg.MaxFrameBytes {
		return p.stop(fmt.Errorf("invalid TrueHD postprocess frame size"))
	}
	p.stats.InputFrames++
	p.stats.InputBytes += int64(len(frame.Bytes))
	if !p.cfg.WindowsSeamless {
		return p.emit(frame)
	}
	if p.n == 2 {
		if frame.MajorSync && p.history[1].MajorSync {
			p.stats.SuppressedFrames++
			p.stats.SuppressedBytes += int64(len(p.history[0].Bytes))
		} else if err := p.emit(p.history[0]); err != nil {
			return err
		}
		p.history[0] = p.history[1]
		p.storage[0], p.storage[1] = p.storage[1], p.storage[0]
		p.n = 1
	}
	copy(p.storage[p.n], frame.Bytes)
	frame.Bytes = p.storage[p.n][:len(frame.Bytes)]
	p.history[p.n] = frame
	p.n++
	return nil
}

func (p *Postprocessor) Finish() error {
	if p.failure != nil {
		return p.failure
	}
	if p.finished {
		return nil
	}
	p.finished = true
	for i := 0; i < p.n; i++ {
		if err := p.emit(p.history[i]); err != nil {
			return err
		}
	}
	p.n = 0
	return nil
}

func (p *Postprocessor) emit(frame Frame) error {
	if err := p.cfg.OnFrame(frame); err != nil {
		return p.stop(err)
	}
	p.stats.EmittedFrames++
	p.stats.EmittedBytes += int64(len(frame.Bytes))
	return nil
}

func (p *Postprocessor) stop(err error) error {
	if p.failure == nil {
		p.failure = err
	}
	return p.failure
}
