// Package timeline applies cumulative audio corrections on a 100 ns clock.
package timeline

import (
	"fmt"
	"io"
	"math"
)

const TicksPerSecond int64 = 10000000

type Correction struct {
	// Position and Cumulative use 100 ns ticks. Records are applied in input
	// order; a regressing position is clamped to the frames already consumed.
	Position   int64
	Cumulative int64
}

// PCMEditor keeps one second of source history. Feed it the decoder's original
// blocks: the history includes the whole current block before an edit is made.
type PCMEditor struct {
	Rate, Channels int
	// OnSplice is called before each inserted block and after each correction.
	// A downstream splice smoother can use this to mark its buffered boundary.
	OnSplice                  func() error
	records                   []Correction
	index                     int
	consumed                  int64
	previous                  int64
	target                    int64
	delta                     int64
	loaded                    bool
	history                   []float64
	forceSilence, forceLoop   bool
	finished                  bool
	err                       error
	output, inserted, removed int64
}

type PCMStats struct {
	InputFrames, OutputFrames, InsertedFrames, RemovedFrames int64
	Corrections                                              int
}

func (p *PCMEditor) Stats() PCMStats {
	return PCMStats{p.consumed, p.output, p.inserted, p.removed, p.index}
}

func NewPCMEditor(rate, channels int, records []Correction, silence, loop bool) (*PCMEditor, error) {
	if channels < 1 || channels > 32 {
		return nil, fmt.Errorf("invalid PCM format")
	}
	if err := validateRate(rate); err != nil {
		return nil, err
	}
	if silence && loop {
		return nil, fmt.Errorf("silence and loop modes conflict")
	}
	if len(records) > 1<<20 {
		return nil, ErrLimit
	}
	previous := int64(0)
	for _, r := range records {
		if _, e := Frames(r.Position, rate); e != nil {
			return nil, e
		}
		if _, e := Frames(r.Cumulative, rate); e != nil {
			return nil, e
		}
		delta, e := checkedSubtract(r.Cumulative, previous)
		if e != nil {
			return nil, e
		}
		if _, e := Frames(delta, rate); e != nil {
			return nil, e
		}
		if r.Cumulative > 0 {
			position, e := checkedSubtract(r.Position, r.Cumulative)
			if e != nil {
				return nil, e
			}
			if _, e := Frames(position, rate); e != nil {
				return nil, e
			}
		}
		previous = r.Cumulative
	}
	return &PCMEditor{Rate: rate, Channels: channels, records: append([]Correction(nil), records...), forceSilence: silence, forceLoop: loop}, nil
}

// Frames truncates toward zero at each division, including negative edits.
func Frames(ticks int64, rate int) (int64, error) {
	if rate < 100 {
		return 0, fmt.Errorf("invalid sample rate")
	}
	factor := int64(rate / 100)
	if ticks > math.MaxInt64/factor || ticks < math.MinInt64/factor {
		return 0, fmt.Errorf("timeline position is too large")
	}
	return (ticks * factor / 10) / 10000, nil
}

func (p *PCMEditor) load() error {
	if p.loaded || p.index == len(p.records) {
		return nil
	}
	r := p.records[p.index]
	pos := r.Position
	if r.Cumulative > 0 {
		if pos < math.MinInt64+r.Cumulative {
			return fmt.Errorf("timeline position overflow")
		}
		pos -= r.Cumulative
	}
	target, e := Frames(pos, p.Rate)
	if e != nil {
		return e
	}
	p.target = max(target, p.consumed)
	difference, e := checkedSubtract(r.Cumulative, p.previous)
	if e != nil {
		return e
	}
	delta, e := Frames(difference, p.Rate)
	if e != nil {
		return e
	}
	p.delta = delta
	p.loaded = true
	return nil
}

func (p *PCMEditor) updateHistory(block []float64) {
	limit := p.Rate * p.Channels
	if len(block) >= limit {
		p.history = append(p.history[:0], block[len(block)-limit:]...)
		return
	}
	excess := len(p.history) + len(block) - limit
	if excess > 0 {
		copy(p.history, p.history[excess:])
		p.history = p.history[:len(p.history)-excess]
	}
	p.history = append(p.history, block...)
}

// Process emits bounded blocks through write. A failed write stops the editor;
// callers should discard the staged output rather than retrying the same block.
func (p *PCMEditor) Process(block []float64, write func([]float64) error) (err error) {
	if p.err != nil {
		return p.err
	}
	if p.finished {
		return ErrFinished
	}
	defer func() {
		if err != nil {
			p.err = err
		}
	}()
	if write == nil {
		return fmt.Errorf("nil PCM writer")
	}
	if len(block)%p.Channels != 0 {
		return fmt.Errorf("incomplete PCM frame")
	}
	for _, v := range block {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("non-finite PCM sample")
		}
	}
	if int64(len(block)/p.Channels) > math.MaxInt64-p.consumed {
		return fmt.Errorf("PCM frame count overflow")
	}
	if p.index < len(p.records) {
		p.updateHistory(block)
	}
	for {
		if e := p.load(); e != nil {
			p.finished = true
			return e
		}
		frames := int64(len(block) / p.Channels)
		if !p.loaded {
			if e := p.emit(block, write); e != nil {
				p.finished = true
				return e
			}
			p.consumed += frames
			return nil
		}
		if p.consumed < p.target {
			if frames == 0 {
				return nil
			}
			n := min(frames, p.target-p.consumed)
			count := int(n) * p.Channels
			if e := p.emit(block[:count], write); e != nil {
				p.finished = true
				return e
			}
			p.consumed += n
			block = block[count:]
			continue
		}
		if p.delta > 0 {
			if e := p.insert(write); e != nil {
				p.finished = true
				return e
			}
		}
		if p.delta < 0 {
			if frames == 0 {
				return nil
			}
			n := min(frames, -p.delta)
			p.delta += n
			p.consumed += n
			p.removed += n
			block = block[int(n)*p.Channels:]
			if p.delta < 0 {
				continue
			}
		}
		if p.delta == 0 {
			if e := p.mark(); e != nil {
				return e
			}
			p.previous = p.records[p.index].Cumulative
			p.index++
			p.loaded = false
		}
	}
}

func (p *PCMEditor) insert(write func([]float64) error) error {
	deltaTime := p.records[p.index].Cumulative - p.previous
	silence := p.forceSilence || (!p.forceLoop && deltaTime >= TicksPerSecond) || len(p.history) < p.Rate*p.Channels
	if silence {
		// Silence replaces the whole loop history, including for later edits.
		limit := p.Rate * p.Channels
		if cap(p.history) < limit {
			p.history = make([]float64, limit)
		} else {
			p.history = p.history[:limit]
			clear(p.history)
		}
	}
	for p.delta >= int64(p.Rate) {
		if e := p.mark(); e != nil {
			return e
		}
		if e := p.emit(p.history, write); e != nil {
			return e
		}
		p.delta -= int64(p.Rate)
		p.inserted += int64(p.Rate)
	}
	if p.delta > 0 {
		count := int(p.delta) * p.Channels
		if e := p.mark(); e != nil {
			return e
		}
		if e := p.emit(p.history[len(p.history)-count:], write); e != nil {
			return e
		}
		p.inserted += p.delta
		p.delta = 0
	}
	return nil
}

func (p *PCMEditor) mark() error {
	if p.OnSplice != nil {
		return p.OnSplice()
	}
	return nil
}

func (p *PCMEditor) emit(block []float64, write func([]float64) error) error {
	if len(block) == 0 {
		return nil
	}
	n := int64(len(block) / p.Channels)
	if n > math.MaxInt64-p.output {
		return fmt.Errorf("PCM output frame count overflow")
	}
	if err := write(block); err != nil {
		return err
	}
	p.output += n
	return nil
}

// Finish reports edits beyond EOF instead of pretending they were applied.
func (p *PCMEditor) Finish() error {
	if p.err != nil {
		return p.err
	}
	p.finished = true
	if p.index != len(p.records) {
		p.err = fmt.Errorf("correction %d extends beyond the source: %w", p.index+1, io.ErrUnexpectedEOF)
		return p.err
	}
	return nil
}
