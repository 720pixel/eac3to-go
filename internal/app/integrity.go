package app

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"eac3to/internal/flaccheck"
)

type flacVerifier struct {
	*flaccheck.Verifier
	expected *uint64
}

func (v *flacVerifier) Finish() (flaccheck.Result, error) {
	result, err := v.Verifier.Finish()
	if err != nil {
		return result, err
	}
	if v.expected != nil {
		result.CountChecked = true
		if result.Frames != *v.expected {
			return result, fmt.Errorf("FLAC integrity check failed: decoded %d frames, encoded source contains %d", result.Frames, *v.expected)
		}
	}
	return result, nil
}

func flacInfo(st *Stream) (flaccheck.Info, error) {
	data, err := parseProbeHex(st.Extradata)
	if err != nil {
		return flaccheck.Info{}, fmt.Errorf("FLAC STREAMINFO: %w", err)
	}
	info, err := flaccheck.ParseStreamInfo(data)
	if err != nil {
		return info, err
	}
	if strconv.Itoa(info.SampleRate) != st.SampleRate || info.Channels != st.Channels || info.BitsPerSample != sampleDepth(st) {
		return info, fmt.Errorf("FLAC STREAMINFO does not match the reported PCM format")
	}
	return info, nil
}

func newFLACVerifier(s *Source, st *Stream) (*flacVerifier, error) {
	if st.Codec != "flac" {
		return nil, nil
	}
	if s.Seek != 0 || s.Limit != 0 {
		return nil, fmt.Errorf("FLAC integrity checking requires the complete source stream")
	}
	info, err := flacInfo(st)
	if err != nil {
		return nil, err
	}
	if info.TotalFrames == 0 && info.MD5 == ([16]byte{}) && st.flacFrames == nil {
		return nil, fmt.Errorf("FLAC lacks a sample count and PCM checksum; encoded frame validation is required before processing")
	}
	verifier, err := flaccheck.NewVerifier(info)
	if err != nil {
		return nil, err
	}
	return &flacVerifier{Verifier: verifier, expected: st.flacFrames}, nil
}

func prepareFLACChecks(ctx context.Context, t Tools, s *Source, streams []*Stream) error {
	for _, st := range streams {
		if st == nil || st.Codec != "flac" || st.flacFrames != nil {
			continue
		}
		info, err := flacInfo(st)
		if err != nil {
			return err
		}
		if info.TotalFrames != 0 || info.MD5 != ([16]byte{}) {
			continue
		}
		result, err := checkEncodedFLAC(ctx, t, s, st)
		if err != nil {
			return fmt.Errorf("FLAC stream %d: %w", st.Index, err)
		}
		if result.Info.SampleRate != info.SampleRate || result.Info.Channels != info.Channels || result.Info.BitsPerSample != info.BitsPerSample {
			return fmt.Errorf("FLAC encoded frames do not match the source format")
		}
		st.flacFrames = &result.Frames
	}
	return nil
}

func checkEncodedFLAC(ctx context.Context, t Tools, s *Source, st *Stream) (flaccheck.EncodedResult, error) {
	if len(s.Paths) == 1 {
		f, err := os.Open(s.Paths[0])
		if err != nil {
			return flaccheck.EncodedResult{}, err
		}
		var magic [4]byte
		_, err = io.ReadFull(f, magic[:])
		f.Close()
		if err == nil && string(magic[:]) == "fLaC" {
			return flaccheck.ValidateFileContext(ctx, s.Paths[0])
		}
	}
	// Disable parser recovery so damaged frames reach the independent checker.
	args := []string{"-hide_banner", "-nostdin", "-v", "error", "-xerror", "-fflags", "+noparse+nofillin"}
	args = append(args, s.InputArgs...)
	args = append(args, "-map", fmt.Sprintf("0:%d", st.Index), "-c:a", "copy", "-f", "flac", "pipe:1")
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(jobCtx, t.FFmpeg, args...)
	errout := tailBuffer{Limit: 32 << 10}
	cmd.Stderr = &errout
	encoded, err := cmd.StdoutPipe()
	if err != nil {
		return flaccheck.EncodedResult{}, err
	}
	if err := cmd.Start(); err != nil {
		return flaccheck.EncodedResult{}, err
	}
	result, parseErr := flaccheck.ValidateContext(ctx, encoded)
	encoded.Close()
	if parseErr != nil {
		cancel()
	}
	runErr := cmd.Wait()
	if parseErr != nil {
		return result, parseErr
	}
	return result, strictToolError("FLAC extraction for integrity checking", runErr, &errout)
}

func parseProbeHex(s string) ([]byte, error) {
	var data []byte
	for _, line := range strings.Split(s, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		offset, rest, ok := strings.Cut(line, ": ")
		if !ok {
			return nil, fmt.Errorf("invalid codec data dump")
		}
		n, err := strconv.ParseUint(offset, 16, 64)
		if err != nil || n != uint64(len(data)) {
			return nil, fmt.Errorf("invalid codec data offset")
		}
		// Two spaces separate the hexadecimal bytes from the printable column.
		column, _, ok := strings.Cut(rest, "  ")
		if !ok {
			return nil, fmt.Errorf("codec data dump has no printable column")
		}
		b, err := hex.DecodeString(strings.ReplaceAll(column, " ", ""))
		if err != nil || len(b) == 0 || len(b) > 16 {
			return nil, fmt.Errorf("invalid codec data bytes")
		}
		data = append(data, b...)
	}
	return data, nil
}

func strictToolError(action string, err error, stderr *tailBuffer) error {
	if err != nil {
		return fmt.Errorf("%s: %w\n%s", action, err, stderr.String())
	}
	if text := strings.TrimSpace(stderr.String()); text != "" {
		return fmt.Errorf("%s reported an error:\n%s", action, text)
	}
	return nil
}

func checkFLACSource(ctx context.Context, t Tools, s *Source, st *Stream, log io.Writer) error {
	verifier, err := newFLACVerifier(s, st)
	if err != nil {
		return err
	}
	args := []string{"-hide_banner", "-nostdin", "-v", "error", "-xerror"}
	args = append(args, inputArgs(s, false)...)
	args = append(args, "-map", fmt.Sprintf("0:%d", st.Index), "-c:a", "pcm_s32le", "-f", "s32le", "pipe:1")
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(jobCtx, t.FFmpeg, args...)
	errout := tailBuffer{Limit: 32 << 10}
	cmd.Stderr = &errout
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
		return copyErr
	}
	if err := strictToolError("FLAC decoder", runErr, &errout); err != nil {
		return err
	}
	result, err := verifier.Finish()
	if err != nil {
		return err
	}
	if result.Frames == 0 {
		return fmt.Errorf("FLAC stream produced no PCM frames")
	}
	var checked []string
	if result.CountChecked {
		checked = append(checked, "sample count")
	}
	if result.MD5Checked {
		checked = append(checked, "PCM checksum")
	}
	if len(checked) == 0 {
		_, err = fmt.Fprintf(log, "FLAC stream %d decoded %d frames; no stored sample count or PCM checksum is available.\n", st.Index, result.Frames)
	} else {
		_, err = fmt.Fprintf(log, "FLAC stream %d: %d frames; %s verified.\n", st.Index, result.Frames, strings.Join(checked, " and "))
	}
	return err
}
