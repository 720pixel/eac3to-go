package app

import (
	"context"
	"eac3to/internal/bitstream"
	"io"
	"os"
)

func standaloneSubtitle(s *Source, out Output) bool {
	if s.Playlist != nil || len(s.Paths) != 1 || out.Track.Stream == nil || !out.Copy || out.Track.Stream.Type != "subtitle" {
		return false
	}
	codec := map[string]string{"srt": "subrip", "ass": "ass", "sup": "hdmv_pgs_subtitle"}[s.Probe.Format.Name]
	return codec != "" && codec == out.Codec && out.Track.Stream.Codec == codec
}

// Subtitle files already carry their own cue timestamps. Passing a standalone
// text file through a demuxer can subtract the first cue's nonzero timestamp.
func copyStandaloneSubtitles(ctx context.Context, s *Source, outputs []Output) ([]Output, error) {
	var remaining []Output
	for _, out := range outputs {
		if !standaloneSubtitle(s, out) {
			remaining = append(remaining, out)
			continue
		}
		input, err := os.Open(s.Paths[0])
		if err != nil {
			return nil, err
		}
		output, err := os.Create(out.Stage)
		if err != nil {
			input.Close()
			return nil, err
		}
		reader := &checkedReader{ctx: ctx, reader: input}
		if out.Forced {
			_, err = bitstream.FilterForcedPGS(reader, output)
		} else {
			_, err = io.CopyBuffer(output, reader, make([]byte, 256<<10))
		}
		closeErr := output.Close()
		input.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, closeErr
		}
	}
	return remaining, nil
}

type checkedReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *checkedReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
