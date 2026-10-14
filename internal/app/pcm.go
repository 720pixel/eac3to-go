package app

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

func canPackPCM(out Output) bool {
	st, a := out.Track.Stream, out.Request.Options
	integer := strings.HasPrefix(st.SampleFormat, "s") || strings.HasPrefix(st.SampleFormat, "u")
	lossless := strings.HasPrefix(st.Codec, "pcm_") || st.Codec == "flac"
	losslessOutput := strings.HasPrefix(out.Codec, "pcm_") || out.Codec == "flac"
	return integer && lossless && losslessOutput && out.Depth == storageDepth(sampleDepth(st)) &&
		!a.Full && !a.Normalize && !a.ForceSecondPass && !a.HDCD && a.Gain == 0 &&
		a.DelayMS == 0 && a.Mix == "" && a.Rate == 0 && a.Speed == 1 && len(a.ChannelMap) == 0 && a.ExtractChannel < 0
}

func packPCM(ctx context.Context, t Tools, s *Source, out Output, o Options, log io.Writer) error {
	st := out.Track.Stream
	verifier, err := newFLACVerifier(s, st)
	if err != nil {
		return err
	}
	args := []string{"-hide_banner", "-nostdin", "-y", "-v", "error", "-xerror"}
	if o.CodecThreads > 0 {
		args = append(args, "-threads", strconv.Itoa(o.CodecThreads))
	}
	args = append(args, inputArgs(s, o.Fast)...)
	args = append(args, "-map", fmt.Sprintf("0:%d", st.Index), "-map_metadata", "-1", "-map_chapters", "-1", "-c:a", out.Codec)
	if o.CodecThreads > 0 {
		args = append(args, "-threads", strconv.Itoa(o.CodecThreads))
	}
	if s.Limit > 0 {
		args = append(args, "-t", fmt.Sprintf("%.9f", s.Limit))
	}
	if out.Codec == "flac" {
		format := "s32"
		if out.Depth <= 16 {
			format = "s16"
		}
		args = append(args, "-sample_fmt", format, "-bits_per_raw_sample", strconv.Itoa(out.Depth), "-compression_level", "8")
	}
	args = append(args, "-f", out.Format)
	if out.Format == "wav" {
		mode := "auto"
		if ext(out.Path) == "rf64" {
			mode = "always"
		}
		args = append(args, "-rf64", mode)
	}
	args = append(args, out.Stage)
	if verifier != nil {
		args = append(args, "-map", fmt.Sprintf("0:%d", st.Index), "-c:a", "pcm_s32le", "-f", "s32le", "pipe:1")
	}
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(jobCtx, t.FFmpeg, args...)
	var errout tailBuffer
	errout.Limit = 32 << 10
	cmd.Stderr = &errout
	if verifier != nil {
		pcm, err := cmd.StdoutPipe()
		if err != nil {
			return err
		}
		if err := cmd.Start(); err != nil {
			return err
		}
		_, copyErr := io.CopyBuffer(writeFunc(verifier.WriteS32LE), pcm, make([]byte, 256<<10))
		pcm.Close()
		if copyErr != nil {
			cancel()
		}
		runErr := cmd.Wait()
		if copyErr != nil {
			return fmt.Errorf("FLAC PCM integrity: %w", copyErr)
		}
		if err := strictToolError("lossless PCM packing", runErr, &errout); err != nil {
			return err
		}
		if _, err := verifier.Finish(); err != nil {
			return err
		}
	} else if err := strictToolError("lossless PCM packing", cmd.Run(), &errout); err != nil {
		return err
	}
	if out.Codec == "flac" || out.Format == "wav" || out.Format == "w64" {
		p, err := probe(ctx, t, []string{"-i", out.Stage})
		if err != nil {
			return err
		}
		if len(p.Streams) != 1 || p.Streams[0].Channels != st.Channels || p.Streams[0].SampleRate != st.SampleRate || sampleDepth(&p.Streams[0]) < out.Depth {
			return fmt.Errorf("encoder changed the PCM format or discarded sample precision")
		}
	}
	return nil
}

type writeFunc func([]byte) (int, error)

func (f writeFunc) Write(p []byte) (int, error) { return f(p) }
