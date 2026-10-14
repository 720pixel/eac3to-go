package app

import (
	"fmt"
	"io"

	"eac3to/internal/pgstiming"
	"eac3to/internal/transport"
)

func (t *nativeTrack) setupPGS(clips []clipScan, initial initialTransportTiming, o Options) error {
	if len(clips) == 0 || !initial.HasAnchor {
		return fmt.Errorf("cannot establish the subtitle timeline")
	}
	delay, err := pgstiming.WindowsDelayMilliseconds(initial.Anchor)
	if err != nil {
		return err
	}
	delay = sourceDelayMilliseconds(delay)
	var writers []io.Writer
	for _, out := range t.outputs {
		writers = append(writers, out.writer)
	}
	t.pgs, err = pgstiming.New(pgstiming.Config{
		Output: io.MultiWriter(writers...),
		Clock:  pgstiming.ClockConfig{Mode: pgstiming.Windows100ns, DelayMilliseconds: delay},
	})
	if err != nil {
		return err
	}
	t.pgsTiming = make([]pgstiming.ClipTiming, len(clips))
	t.pgsStarts = make([]int64, len(clips))
	var prior int64
	for i, clip := range clips {
		if !clip.Scan.HasPTS {
			return fmt.Errorf("clip %s has no subtitle timing reference", clip.Clip)
		}
		t.pgsStarts[i] = clip.Scan.FirstPTS
		if i > 0 && clip.Boundary == 0 {
			// A continuous boundary leaves the active mapping unchanged, even
			// when MPLS duration differs from the observed source displacement.
			t.pgsTiming[i] = t.pgsTiming[i-1]
			delta, err := transport.TimestampDelta(clip.Scan.FirstPTS, clips[i-1].Scan.FirstPTS)
			if err != nil {
				return err
			}
			t.pgsStarts[i] = t.pgsStarts[i-1] + delta
		} else {
			t.pgsTiming[i] = pgstiming.ClipTiming{
				FirstActualStart90k:      clips[0].Scan.FirstPTS,
				CurrentActualStart90k:    clip.Scan.FirstPTS,
				PriorPlaylistDuration90k: prior,
				In90k:                    clip.In90k,
				Strict:                   o.StrictPL,
			}
		}
		prior += clip.Out90k - clip.In90k
	}
	return nil
}

func (t *nativeTrack) writePGS(pes transport.PES) error {
	if pes.Segment < 0 || pes.Segment >= len(t.pgsTiming) {
		return fmt.Errorf("subtitle PES has an invalid clip index")
	}
	timing := t.pgsTiming[pes.Segment]
	start := t.pgsStarts[pes.Segment]
	delta, err := transport.TimestampDelta(pes.PTS, start)
	if err != nil {
		return fmt.Errorf("subtitle timestamp: %w", err)
	}
	// Choose the source-clock cycle containing the scanned clip start. The SUP
	// writer performs its own final 32-bit conversion after playlist rebasing.
	timing.SourceCycles = (start + delta - pes.PTS) / (1 << 33)
	mapped, err := pgstiming.MapClipTimestamp(pes.PTS, timing)
	if err != nil {
		return err
	}
	return t.pgs.WritePES(pes.Payload, pgstiming.Stamp{PTS90k: mapped, DTS90k: pgstiming.NoTimestamp})
}
