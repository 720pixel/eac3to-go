// Package truehd separates Blu-ray TrueHD and its AC-3 companion on a shared
// transport PID. It does not frame, align, decode, or modify either payload.
package truehd

import (
	"fmt"

	"eac3to/internal/transport"
)

type Kind uint8

const (
	TrueHD Kind = iota + 1
	AC3
)

// Classify requires the explicit Blu-ray PES extension. Inferring a stream from
// a payload sync word could misroute fragments of a split codec frame.
// FFmpeg's HDMV transport routing uses extension 0x76 for AC-3; the TrueHD
// substream is 0x72 (also used by GStreamer's Blu-ray transport demuxer).
// https://github.com/FFmpeg/FFmpeg/blob/n6.1/libavformat/mpegts.c
// https://gitlab.freedesktop.org/gstreamer/gstreamer/-/blob/main/subprojects/gst-plugins-bad/gst/mpegtsdemux/tsdemux.c
func Classify(p transport.PES) (Kind, error) {
	if p.StreamID != 0xfd {
		return 0, fmt.Errorf("Blu-ray TrueHD requires PES stream ID 0xfd, got 0x%02x", p.StreamID)
	}
	switch p.StreamIDExtension {
	case 0x72:
		return TrueHD, nil
	case 0x76:
		return AC3, nil
	default:
		return 0, fmt.Errorf("unsupported Blu-ray TrueHD PES extension %d", p.StreamIDExtension)
	}
}

type Config struct {
	PID uint16
	// A nil handler deselects that companion. Selected callbacks receive the
	// original borrowed PES, including timestamps and segment information.
	OnTrueHD func(transport.PES) error
	OnAC3    func(transport.PES) error
}

type StreamStats struct {
	PESPackets, PayloadBytes, EmittedPES, EmittedBytes int64
}

type Stats struct {
	TrueHD, AC3 StreamStats
}

// Separator retains no payload bytes and must not be used concurrently.
// A callback failure is terminal, preventing accidental retries that duplicate
// bytes already consumed by an output writer.
type Separator struct {
	cfg     Config
	stats   Stats
	failure error
}

func New(cfg Config) (*Separator, error) {
	if cfg.PID >= 0x1fff {
		return nil, fmt.Errorf("invalid Blu-ray TrueHD PID 0x%04x", cfg.PID)
	}
	if cfg.OnTrueHD == nil && cfg.OnAC3 == nil {
		return nil, fmt.Errorf("at least one TrueHD or AC-3 handler is required")
	}
	return &Separator{cfg: cfg}, nil
}

func (s *Separator) Stats() Stats { return s.stats }

func (s *Separator) WritePES(p transport.PES) error {
	if s.failure != nil {
		return s.failure
	}
	if p.PID != s.cfg.PID {
		s.failure = fmt.Errorf("Blu-ray TrueHD PID changed from 0x%04x to 0x%04x", s.cfg.PID, p.PID)
		return s.failure
	}
	kind, err := Classify(p)
	if err != nil {
		s.failure = err
		return err
	}
	stats, handler := &s.stats.TrueHD, s.cfg.OnTrueHD
	if kind == AC3 {
		stats, handler = &s.stats.AC3, s.cfg.OnAC3
	}
	stats.PESPackets++
	stats.PayloadBytes += int64(len(p.Payload))
	if handler == nil {
		return nil
	}
	if err = handler(p); err != nil {
		s.failure = err
		return err
	}
	stats.EmittedPES++
	stats.EmittedBytes += int64(len(p.Payload))
	return nil
}
