package seamless

import (
	"bytes"
	"fmt"
)

// OverlapLimit is explicit evidence supplied by the caller. ByteBudget is the
// number of bytes queued when the boundary arrives, not the length of a silent
// run. A zero budget still compares the next complete frame.
type OverlapLimit struct {
	ByteBudget  int64
	MaxRemovals int
}

// WindowsBoundary restricts the recovered policy to one matching last frame.
func WindowsBoundary(queuedBytes int) OverlapLimit {
	return OverlapLimit{ByteBudget: int64(queuedBytes), MaxRemovals: 1}
}

// Overlap retains exactly one accepted frame. It never compares repeated frames
// until Boundary explicitly arms it. Independent timestamp policies should
// supply their own limits rather than inferring an unlimited overlap from bytes.
type Overlap struct {
	last          []byte
	budget        int64
	remaining     int
	maxFrameBytes int
}

func NewOverlap(maxFrameBytes int) (*Overlap, error) {
	if maxFrameBytes <= 0 {
		return nil, fmt.Errorf("overlap frame bound must be positive")
	}
	return &Overlap{maxFrameBytes: maxFrameBytes}, nil
}

func (o *Overlap) Boundary(limit OverlapLimit) error {
	if limit.ByteBudget < 0 || limit.MaxRemovals < 0 {
		return fmt.Errorf("negative overlap limit")
	}
	o.budget = limit.ByteBudget
	o.remaining = limit.MaxRemovals
	return nil
}

// Accept returns false only for a bounded boundary duplicate. Accepted input is
// copied into private history so the caller can reuse the frame buffer.
func (o *Overlap) Accept(frame Frame) (bool, error) {
	if len(frame.Bytes) == 0 || len(frame.Bytes) > o.maxFrameBytes {
		return false, fmt.Errorf("invalid overlap frame length %d", len(frame.Bytes))
	}
	if o.remaining > 0 {
		if bytes.Equal(o.last, frame.Bytes) {
			o.remaining--
			return false, nil
		}
		o.budget -= int64(len(frame.Bytes))
		if o.budget < 0 {
			o.remaining = 0
		}
	}
	o.last = append(o.last[:0], frame.Bytes...)
	return true, nil
}
