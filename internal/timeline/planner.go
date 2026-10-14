package timeline

import (
	"errors"
	"fmt"
	"math"
	"sort"
)

const NoTimestamp int64 = -1

var (
	ErrFinished = errors.New("timeline is finished")
	ErrLimit    = errors.New("timeline correction limit exceeded")
)

type PlannerConfig struct {
	SampleRate int
	// MaxCorrections bounds retained candidates. Zero allows 65536 records.
	MaxCorrections int
}

// TimestampPlanner detects persistent PCM discontinuities from decoder blocks.
// Timestamps are already mapped to one continuous 100 ns clock; transport wraps,
// file joins and playlist windows must be handled before Observe. This is the
// legacy PCM gap detector, not the Blu-ray 4.00 extraction policy.
type TimestampPlanner struct {
	rate, limit int
	frames      int64
	offset      int64
	initialized bool
	candidates  []Correction
	finished    bool
	err         error
}

func NewTimestampPlanner(config PlannerConfig) (*TimestampPlanner, error) {
	if err := validateRate(config.SampleRate); err != nil {
		return nil, err
	}
	limit := config.MaxCorrections
	if limit == 0 {
		limit = 65536
	}
	if limit < 1 || limit > 1<<20 {
		return nil, fmt.Errorf("invalid timeline correction limit %d", limit)
	}
	return &TimestampPlanner{rate: config.SampleRate, limit: limit}, nil
}

// Observe associates a block's timestamp with its first complete sample frame.
// NoTimestamp advances the sample count without changing timestamp evidence.
// The first known timestamp establishes the origin, including when preceding
// blocks lack timestamps. Audio absent before that block cannot be inferred.
func (p *TimestampPlanner) Observe(timestamp100ns, frames int64) error {
	if p.err != nil {
		return p.err
	}
	if p.finished {
		return ErrFinished
	}
	if frames < 0 || frames > math.MaxInt64-p.frames {
		p.err = fmt.Errorf("PCM frame count overflow")
		return p.err
	}
	// Check the final duration before changing any state.
	if _, err := sampleTime(p.frames+frames, p.rate); err != nil {
		p.err = err
		return err
	}
	if timestamp100ns != NoTimestamp {
		if err := p.observe(timestamp100ns); err != nil {
			p.err = err
			return err
		}
	}
	p.frames += frames
	return nil
}

func (p *TimestampPlanner) observe(timestamp int64) error {
	expected, err := sampleTime(p.frames, p.rate)
	if err != nil {
		return err
	}
	if !p.initialized {
		p.offset, err = checkedSubtract(timestamp, expected)
		if err != nil {
			return err
		}
		p.initialized = true
	}
	position, err := checkedSubtract(timestamp, p.offset)
	if err != nil {
		return err
	}
	gap, err := checkedSubtract(position, expected)
	if err != nil {
		return err
	}
	previous := int64(0)
	if len(p.candidates) != 0 {
		previous = p.candidates[len(p.candidates)-1].Cumulative
	}
	if gap == 0 || (gap < 0) != (previous < 0) {
		p.candidates = p.candidates[:0]
		previous = 0
	}
	difference, err := checkedSubtract(gap, previous)
	if err != nil {
		return err
	}
	if difference > -50000 && difference < 50000 {
		return nil
	}
	if gap > 0 && gap > previous || gap < 0 && gap < previous {
		if len(p.candidates) == p.limit {
			return ErrLimit
		}
		p.candidates = append(p.candidates, Correction{Position: position, Cumulative: gap})
		return nil
	}
	// Within one sign, cumulative magnitudes increase. Preserve the earliest
	// position at which the smaller surviving discontinuity appeared.
	i := sort.Search(len(p.candidates), func(i int) bool {
		if gap > 0 {
			return p.candidates[i].Cumulative >= gap
		}
		return p.candidates[i].Cumulative <= gap
	})
	if i < len(p.candidates) {
		p.candidates[i].Cumulative = gap
		p.candidates = p.candidates[:i+1]
	}
	return nil
}

// InputFrames counts complete interleaved sample frames observed successfully.
func (p *TimestampPlanner) InputFrames() int64 { return p.frames }

// Finish keeps a prefix ending more than 30 seconds before the decoded end,
// provided that prefix's final cumulative discontinuity is at least 7 ms.
// The summed upstream delay shifts event positions, not correction amounts.
// Returned records keep observation order even when source timestamps regress.
func (p *TimestampPlanner) Finish(delayMilliseconds int64) ([]Correction, error) {
	if p.err != nil {
		return nil, p.err
	}
	if p.finished {
		return nil, ErrFinished
	}
	p.finished = true
	end, err := sampleTime(p.frames, p.rate)
	if err != nil {
		return nil, err
	}
	keep := 0
	for i := len(p.candidates) - 1; i >= 0; i-- {
		age, err := checkedSubtract(end, p.candidates[i].Position)
		if err != nil {
			return nil, err
		}
		if age <= 30*TicksPerSecond {
			continue
		}
		gap := p.candidates[i].Cumulative
		if gap >= 70000 || gap <= -70000 {
			keep = i + 1
		}
		break
	}
	if keep == 0 {
		return nil, nil
	}
	if delayMilliseconds > math.MaxInt32/10000 || delayMilliseconds < math.MinInt32/10000 {
		return nil, fmt.Errorf("upstream delay exceeds the PCM gap detector's signed 32-bit clock")
	}
	delay := delayMilliseconds * 10000
	result := append([]Correction(nil), p.candidates[:keep]...)
	for i := range result {
		result[i].Position, err = checkedAdd(result[i].Position, delay)
		if err != nil {
			return nil, err
		}
	}
	return result, nil
}

func validateRate(rate int) error {
	if rate < 100 || rate > 768000 {
		return fmt.Errorf("invalid PCM sample rate %d", rate)
	}
	if rate%100 != 0 {
		return fmt.Errorf("timeline conversion at %d Hz needs reference validation", rate)
	}
	return nil
}

func sampleTime(frames int64, rate int) (int64, error) {
	if frames < 0 || frames > math.MaxInt64/100000 {
		return 0, fmt.Errorf("PCM duration overflow")
	}
	return frames * 100000 / int64(rate/100), nil
}

func checkedAdd(a, b int64) (int64, error) {
	if b > 0 && a > math.MaxInt64-b || b < 0 && a < math.MinInt64-b {
		return 0, fmt.Errorf("timeline clock overflow")
	}
	return a + b, nil
}

func checkedSubtract(a, b int64) (int64, error) {
	if b > 0 && a < math.MinInt64+b || b < 0 && a > math.MaxInt64+b {
		return 0, fmt.Errorf("timeline clock overflow")
	}
	return a - b, nil
}
