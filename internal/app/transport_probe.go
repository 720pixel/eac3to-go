package app

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"

	"eac3to/internal/transport"
)

const referenceProbeBytes int64 = 300 << 20

// The reference analyzer examines the first 300 MiB of the physical input.
// Its minima span every clip reached by that probe, even with -strictpl.
func probeTransportTimestamps(ctx context.Context, clips []clipScan, pids map[uint16]bool, limit int64) (map[uint16]int64, error) {
	minimum := map[uint16]int64{}
	seen := map[uint16]bool{}
	pending := map[uint16]int64{}
	handlers := map[uint16]func(transport.PES) error{}
	for pid := range pids {
		handlers[pid] = func(pes transport.PES) error {
			if pes.PTS != transport.NoTimestamp {
				stamp := pes.PTS
				if !seen[pes.PID] {
					// Boundaries before a track's first timestamp accumulate in
					// the PES reader. It subtracts that amount from this first
					// timestamp even when reporting raw timestamp minima.
					delta := pending[pes.PID]
					if delta > 0 && stamp < math.MinInt64+delta || delta < 0 && stamp > math.MaxInt64+delta {
						return fmt.Errorf("PID 0x%04x startup timestamp overflow", pes.PID)
					}
					stamp -= delta
					seen[pes.PID] = true
					delete(pending, pes.PID)
				}
				value, exists := minimum[pes.PID]
				if !exists || stamp < value {
					minimum[pes.PID] = stamp
				}
			}
			return nil
		}
	}
	demux, err := transport.New(transport.Config{Handlers: handlers, OnBoundary: func(boundary transport.Boundary) error {
		if !boundary.Before || boundary.Delta90k == 0 {
			return nil
		}
		for pid := range pids {
			if seen[pid] {
				continue
			}
			value, delta := pending[pid], boundary.Delta90k
			if delta > 0 && value > math.MaxInt64-delta || delta < 0 && value < math.MinInt64-delta {
				return fmt.Errorf("PID 0x%04x startup boundary overflow", pid)
			}
			pending[pid] = value + delta
		}
		return nil
	}})
	if err != nil {
		return nil, err
	}
	partial := false
	for i, clip := range clips {
		count := min(limit, clip.Scan.FileSize)
		count -= count % int64(clip.Scan.PacketSize)
		if count == 0 {
			partial = true
			break
		}
		file, err := os.Open(clip.Path)
		if err != nil {
			return nil, err
		}
		err = demux.ReadSegment(ctx, io.LimitReader(file, count), transport.Segment{Index: i, PacketSize: clip.Scan.PacketSize, BoundaryDelta90k: clip.Boundary})
		closeErr := file.Close()
		if err != nil {
			return nil, fmt.Errorf("timing probe in clip %s: %w", clip.Clip, err)
		}
		if closeErr != nil {
			return nil, closeErr
		}
		limit -= count
		if count < clip.Scan.FileSize {
			partial = true
			break
		}
	}
	// A prefix may end in the middle of a PES. Only complete observations count;
	// that artificial end is not evidence of a damaged source.
	if !partial {
		if err := demux.Finish(); err != nil {
			return nil, err
		}
	}
	return minimum, nil
}

type initialTransportTiming struct {
	Anchor    int64
	HasAnchor bool
	Delays    map[uint16]int64
}

func transportInitialTiming(ctx context.Context, s *Source, clips []clipScan) (initialTransportTiming, error) {
	result := initialTransportTiming{Delays: map[uint16]int64{}}
	pids := map[uint16]bool{}
	for _, stream := range s.Probe.Streams {
		pid, err := strconv.ParseUint(stream.ID, 0, 13)
		if err == nil && pid < 0x1fff && (stream.Type == "audio" || stream.Type == "video") {
			pids[uint16(pid)] = true
		}
	}
	if len(pids) == 0 {
		return result, nil
	}
	minimum, err := probeTransportTimestamps(ctx, clips, pids, referenceProbeBytes)
	if err != nil {
		return result, err
	}
	anchor, haveAnchor := int64(0), false
	for _, stream := range s.Probe.Streams {
		pid, err := strconv.ParseUint(stream.ID, 0, 13)
		if err != nil || stream.Type != "video" {
			continue
		}
		if value, exists := minimum[uint16(pid)]; exists {
			anchor, haveAnchor = value, true
			break
		}
	}
	if !haveAnchor {
		for _, value := range minimum {
			if !haveAnchor || value < anchor {
				anchor, haveAnchor = value, true
			}
		}
	}
	result.Anchor, result.HasAnchor = anchor, haveAnchor
	for pid, value := range minimum {
		// Truncate each absolute timestamp before subtraction. Truncating the
		// difference instead changes the result by a millisecond on some discs.
		result.Delays[pid] = sourceDelayMilliseconds(value/90 - anchor/90)
	}
	return result, nil
}

// Track records reserve -1 for an unknown offset and expose it as zero.
// Explicit user delays do not pass through this source-metadata conversion.
func sourceDelayMilliseconds(delay int64) int64 {
	if delay == -1 {
		return 0
	}
	return delay
}
