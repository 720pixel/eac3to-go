package app

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"

	"eac3to/internal/seamless"
)

func newFrameDelayTrack(ctx context.Context, out Output, o Options, writer *bufio.Writer, log io.Writer) (*nativeTrack, error) {
	codec := seamless.AC3
	if out.Track.Stream.Codec == "dts" {
		codec = seamless.DTS
	} else if out.Track.Stream.Codec != "ac3" {
		return nil, fmt.Errorf("compressed delay is unsupported for %s", out.Track.Stream.Codec)
	}
	track := &nativeTrack{
		ctx: ctx, log: log, codec: out.Track.Stream.Codec,
		delayMS: int64(out.Request.Options.DelayMS), checkCRC: !o.Fast,
		outputs: []*nativeOutput{{output: out, writer: writer}},
	}
	var err error
	track.overlap, err = seamless.NewOverlap(seamless.DefaultMaxFrameBytes)
	if err != nil {
		return nil, err
	}
	track.parser, err = seamless.NewParser(seamless.Config{Codec: codec, OnFrame: track.frame})
	return track, err
}

func finishFrameDelay(track *nativeTrack, number int, o Options, log io.Writer) error {
	if err := track.parser.Flush(); err != nil {
		return err
	}
	if track.bytes == 0 {
		return fmt.Errorf("track %d has no frames left after its delay", number)
	}
	if !o.Quiet {
		fmt.Fprintf(log, "Track %d: delay %+d ms; removed %d / inserted %d frames; remaining delay %+d ms.\n", number, track.delayMS, track.initialCut, track.initialPad, track.residualMS)
	}
	return nil
}

func copyFrameDelay(ctx context.Context, reader io.Reader, writer *bufio.Writer, out Output, o Options, log io.Writer) error {
	track, err := newFrameDelayTrack(ctx, out, o, writer, log)
	if err != nil {
		return err
	}
	if _, err := io.CopyBuffer(track.parser, reader, make([]byte, 256<<10)); err != nil {
		return err
	}
	return finishFrameDelay(track, out.Track.Number, o, log)
}

// Read elementary input directly so a container parser cannot discard damaged
// frames before the delay stage checks them.
func copyElementaryDelays(ctx context.Context, s *Source, outputs []Output, o Options, log io.Writer) ([]Output, error) {
	if s.Playlist != nil || len(s.Paths) != 1 || s.Probe.Format.Name != "ac3" && s.Probe.Format.Name != "dts" {
		return outputs, nil
	}
	var remaining []Output
	var tracks []*nativeTrack
	var numbers []int
	var files []*os.File
	var writers []*bufio.Writer
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()
	for _, out := range outputs {
		if !out.Copy || out.Request.Options.DelayMS == 0 || !nativeCopySupported(out) {
			remaining = append(remaining, out)
			continue
		}
		file, err := os.Create(out.Stage)
		if err != nil {
			return nil, err
		}
		files = append(files, file)
		writer := bufio.NewWriterSize(file, 256<<10)
		track, err := newFrameDelayTrack(ctx, out, o, writer, log)
		if err != nil {
			return nil, err
		}
		writers = append(writers, writer)
		tracks = append(tracks, track)
		numbers = append(numbers, out.Track.Number)
	}
	if len(tracks) == 0 {
		return outputs, nil
	}
	input, err := os.Open(s.Paths[0])
	if err != nil {
		return nil, err
	}
	defer input.Close()
	buffer := make([]byte, 1<<20)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n, readErr := input.Read(buffer)
		for _, track := range tracks {
			if _, err := track.parser.Write(buffer[:n]); err != nil {
				return nil, err
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, readErr
		}
	}
	for i, track := range tracks {
		if err := finishFrameDelay(track, numbers[i], o, log); err != nil {
			return nil, err
		}
		if err := writers[i].Flush(); err != nil {
			return nil, err
		}
		if err := files[i].Close(); err != nil {
			return nil, err
		}
	}
	return remaining, nil
}
