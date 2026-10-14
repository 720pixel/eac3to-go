package app

import (
	"context"
	"fmt"
	"io"
	"math"

	"eac3to/internal/video"
)

func videoOptions(st *Stream, a AudioOptions) (AudioOptions, error) {
	if a.DelayMS != 0 {
		return a, fmt.Errorf("video delay needs container timestamps; elementary video cannot carry a delay without adding or removing pictures")
	}
	if a.TargetFPS != 0 || a.SourceFPS != 0 {
		if st.Codec != "h264" {
			return a, fmt.Errorf("native frame-rate editing is not yet validated for %s", st.Codec)
		}
		if a.TargetFPS == 0 {
			if _, err := h264SourceFrameRate(st); err != nil {
				return a, fmt.Errorf("video has no validated source cadence; specify a target frame rate to write timing headers: %w", err)
			}
		}
		// Video speed changes rewrite timing headers; encoded pictures stay intact.
		a.Speed = 1
	}
	return a, nil
}

func sourceFrameRate(s *Source) (float64, error) {
	var known float64
	for i := range s.Probe.Streams {
		st := &s.Probe.Streams[i]
		if st.Type != "video" || st.Disposition["attached_pic"] != 0 {
			continue
		}
		if st.Codec != "h264" {
			return 0, fmt.Errorf("source frame-rate inference for %s video still needs validation; extract the audio first and specify its source FPS", st.Codec)
		}
		fps, err := h264SourceFrameRate(st)
		if err != nil {
			return 0, fmt.Errorf("cannot infer audio speed from video stream %d: %w; extract the audio first and specify its source FPS", st.Index, err)
		}
		if known != 0 && math.Abs(known-fps) > 0.0005 {
			return 0, fmt.Errorf("input contains different video frame rates; extract audio first to specify its source rate explicitly")
		}
		known = fps
	}
	return known, nil
}

func h264SourceFrameRate(st *Stream) (float64, error) {
	// r_frame_rate can be twice the frame clock for a short progressive stream.
	// Codec SPSs avoid deriving audio speed from that probe estimate.
	if len(st.Extradata) > 5<<20 {
		return 0, fmt.Errorf("H.264 codec-data dump exceeds its bound")
	}
	data, err := parseProbeHex(st.Extradata)
	if err != nil {
		return 0, fmt.Errorf("invalid H.264 codec data: %w", err)
	}
	sets, err := video.ParseH264Config(data)
	if err != nil {
		return 0, err
	}
	var clock video.H264Clock
	for _, sps := range sets {
		if !sps.TimingPresent {
			return 0, fmt.Errorf("H.264 SPS %d has no timing clock", sps.ID)
		}
		if !sps.FrameMbsOnly || sps.PicStructPresent {
			return 0, fmt.Errorf("H.264 SPS %d signals field or picture-structure cadence that needs separate validation", sps.ID)
		}
		if clock.NumUnitsInTick != 0 && uint64(clock.TimeScale)*uint64(sps.Clock.NumUnitsInTick) != uint64(sps.Clock.TimeScale)*uint64(clock.NumUnitsInTick) {
			return 0, fmt.Errorf("H.264 SPS clocks disagree")
		}
		clock = sps.Clock
	}
	return clock.FPS(), nil
}

func videoTimingRequested(out Output) bool {
	return out.Track.Stream != nil && out.Track.Stream.Type == "video" && out.Request.Options.TargetFPS != 0
}

func newVideoTimingWriter(out Output, destination io.Writer) (*video.H264Writer, error) {
	if out.Codec != "h264" || out.Format != "h264" {
		return nil, fmt.Errorf("frame-rate editing requires an elementary H.264 stream")
	}
	clock, err := video.H264ClockForFPS(out.Request.Options.TargetFPS)
	if err != nil {
		return nil, err
	}
	return video.NewH264Writer(destination, clock)
}

func copyVideoTiming(ctx context.Context, source io.Reader, destination io.Writer, out Output, log io.Writer) error {
	writer, err := newVideoTimingWriter(out, destination)
	if err != nil {
		return err
	}
	buffer := make([]byte, 256<<10)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := source.Read(buffer)
		if n > 0 {
			if _, err := writer.Write(buffer[:n]); err != nil {
				return err
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return readErr
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := writer.Finish(); err != nil {
		return err
	}
	logVideoTiming(log, out, writer.Stats())
	return nil
}

func logVideoTiming(log io.Writer, out Output, stats video.H264Stats) {
	if log != nil {
		fmt.Fprintf(log, "Track %d: H.264 header clock %.6f fps; changed %d of %d SPS headers.\n", out.Track.Number, stats.Target.FPS(), stats.ChangedSPS, stats.SPS)
	}
}
