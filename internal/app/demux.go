package app

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"

	"eac3to/internal/bitstream"
)

type filteredPipe struct {
	out    Output
	pipe   *demuxPipe
	file   *os.File
	verify *flacVerifier
}
type filterResult struct {
	track int
	text  string
	err   error
}

func copyStreams(ctx context.Context, t Tools, s *Source, outputs []Output, o Options, log io.Writer) error {
	var err error
	outputs, err = copyNativeStreams(ctx, s, outputs, o, log)
	if err != nil {
		return err
	}
	outputs, err = copyElementaryTrueHD(ctx, s, outputs, o, log)
	if err != nil {
		return err
	}
	outputs, err = copyElementaryDelays(ctx, s, outputs, o, log)
	if err != nil {
		return err
	}
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	args := []string{"-hide_banner", "-nostdin", "-y", "-v", "error", "-xerror"}
	args = append(args, inputArgs(s, o.Fast)...)
	var pipes []filteredPipe
	defer func() {
		for _, p := range pipes {
			p.pipe.Close()
			if p.file != nil {
				p.file.Close()
			}
		}
	}()
	count := 0
	checks := map[int]bool{}
	for _, out := range outputs {
		if !out.Copy {
			continue
		}
		count++
		path := out.Stage
		if out.Forced || out.RemoveDialnorm || out.Codec == "dts" && out.Format == "dts" || out.Request.Options.DelayMS != 0 || out.Codec == "truehd" && out.Format == "truehd" || videoTimingRequested(out) {
			pipe, e := newDemuxPipe(jobCtx, len(pipes))
			if e != nil {
				return e
			}
			f, e := os.Create(out.Stage)
			if e != nil {
				pipe.Close()
				return e
			}
			path = pipe.path
			pipes = append(pipes, filteredPipe{out: out, pipe: pipe, file: f})
		}
		codec := "copy"
		if out.CacheCodec != "" {
			codec = out.CacheCodec
		}
		args = append(args, "-map", fmt.Sprintf("0:%d", out.Track.Stream.Index), "-map_metadata", "-1", "-map_chapters", "-1", "-c", codec)
		if out.BSF != "" {
			args = append(args, "-bsf:a", out.BSF)
		}
		if out.Codec == "h264" {
			args = append(args, "-bsf:v", "h264_mp4toannexb")
		}
		if out.Codec == "hevc" {
			args = append(args, "-bsf:v", "hevc_mp4toannexb")
		}
		if s.Limit > 0 {
			args = append(args, "-t", fmt.Sprintf("%.9f", s.Limit))
		}
		args = append(args, "-f", out.Format)
		if ext(out.Path) == "rf64" {
			args = append(args, "-rf64", "always")
		} else if out.Format == "wav" {
			args = append(args, "-rf64", "auto")
		}
		args = append(args, path)
		if out.Track.Stream.Codec == "flac" && !checks[out.Track.Stream.Index] {
			verifier, err := newFLACVerifier(s, out.Track.Stream)
			if err != nil {
				return err
			}
			pipe, err := newDemuxPipe(jobCtx, len(pipes))
			if err != nil {
				return err
			}
			path := pipe.path
			pipes = append(pipes, filteredPipe{out: out, pipe: pipe, verify: verifier})
			args = append(args, "-map", fmt.Sprintf("0:%d", out.Track.Stream.Index), "-c:a", "pcm_s32le", "-f", "s32le", path)
			checks[out.Track.Stream.Index] = true
		}
	}
	if count == 0 {
		return nil
	}
	if !o.Quiet {
		fmt.Fprintf(log, "Extracting %d tracks in one pass.\n", count)
	}
	cmd := exec.CommandContext(jobCtx, t.FFmpeg, args...)
	cmd.Stdout = io.Discard
	errout := tailBuffer{Limit: 32 << 10}
	cmd.Stderr = &errout
	for _, p := range pipes {
		p.pipe.bind(cmd)
	}
	if e := cmd.Start(); e != nil {
		return e
	}
	results := make(chan filterResult, len(pipes))
	for _, p := range pipes {
		p.pipe.started()
		go func(p filteredPipe) {
			result := filterResult{track: p.out.Track.Number}
			if p.verify != nil {
				_, result.err = io.CopyBuffer(writeFunc(p.verify.WriteS32LE), p.pipe, make([]byte, 256<<10))
				if result.err == nil {
					_, result.err = p.verify.Finish()
				}
				p.pipe.Close()
				if result.err != nil {
					cancel()
				}
				results <- result
				return
			}
			w := bufio.NewWriterSize(p.file, 256<<10)
			if videoTimingRequested(p.out) {
				var videoLog io.Writer
				if !o.Quiet {
					videoLog = log
				}
				result.err = copyVideoTiming(jobCtx, p.pipe, w, p.out, videoLog)
			} else if p.out.Codec == "truehd" && p.out.Format == "truehd" {
				result.err = copyTrueHDPipe(jobCtx, p.pipe, w, p.out, log)
			} else if p.out.Request.Options.DelayMS != 0 {
				result.err = copyFrameDelay(jobCtx, p.pipe, w, p.out, o, log)
			} else if p.out.Forced {
				stats, e := bitstream.FilterForcedPGS(p.pipe, w)
				result.err = e
				result.text = fmt.Sprintf("PGS: %d forced objects in %d presentations.", stats.ForcedObjects, stats.Presentations)
			} else if p.out.Codec == "dts" {
				stats, e := bitstream.CopyDTSHeaders(p.pipe, w, p.out.RemoveDialnorm)
				result.err = e
				result.text = fmt.Sprintf("DTS: %d dialnorm edits, %d source-resolution header repairs.", stats.ChangedFrames, stats.CoreResolutionChanges)
			} else {
				stats, e := bitstream.RemoveDialnorm(p.pipe, w)
				result.err = e
				result.text = fmt.Sprintf("Dialnorm: changed %d of %d frames.", stats.ChangedFrames, stats.Frames)
			}
			if e := w.Flush(); result.err == nil {
				result.err = e
			}
			if e := p.file.Close(); result.err == nil {
				result.err = e
			}
			p.pipe.Close()
			if result.err != nil {
				cancel()
			}
			results <- result
		}(p)
	}
	cmdErr := cmd.Wait()
	for _, p := range pipes {
		p.pipe.processDone()
	}
	var firstErr error
	for range pipes {
		r := <-results
		if r.err != nil && firstErr == nil {
			firstErr = fmt.Errorf("track %d bitstream processing: %w", r.track, r.err)
		}
		if r.err == nil && r.text != "" && !o.Quiet {
			fmt.Fprintln(log, r.text)
		}
	}
	if firstErr != nil {
		return firstErr
	}
	return strictToolError("demux", cmdErr, &errout)
}
