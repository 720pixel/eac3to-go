// Package compare compares decoded PCM without changing its sample rate,
// channel count, or layout. Decoder output is consumed in bounded blocks.
package compare

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

type Options struct {
	FFmpeg          string
	FFprobe         string
	Float           bool
	ReferenceStream *int // audio-stream ordinal; nil requires exactly one audio stream
	CandidateStream *int
}

type Tool struct {
	Path    string `json:"path"`
	Version string `json:"version"`
}

type AudioInfo struct {
	Index         int    `json:"index"`
	Codec         string `json:"codec"`
	SampleFormat  string `json:"sample_format"`
	SampleRate    int    `json:"sample_rate"`
	Channels      int    `json:"channels"`
	ChannelLayout string `json:"channel_layout"`
	Bits          int    `json:"bits"`
}

type Source struct {
	Path    string    `json:"path"`
	Audio   AudioInfo `json:"audio"`
	Samples uint64    `json:"samples"`
	Frames  uint64    `json:"frames"`
}

// Difference positions are zero-based. An absent value means that stream ended.
type Difference struct {
	Frame          uint64  `json:"frame"`
	Channel        int     `json:"channel"`
	ReferenceValue *string `json:"reference_value"`
	CandidateValue *string `json:"candidate_value"`
}

type Report struct {
	Exact            bool        `json:"exact"`
	Format           string      `json:"format"`
	Reference        Source      `json:"reference"`
	Candidate        Source      `json:"candidate"`
	ComparedSamples  uint64      `json:"compared_samples"`
	DifferingSamples uint64      `json:"differing_samples"`
	MissingSamples   uint64      `json:"missing_samples"`
	MaxIntegerError  *uint64     `json:"max_integer_error,omitempty"`
	FirstDifference  *Difference `json:"first_difference,omitempty"`
	FFmpeg           Tool        `json:"ffmpeg"`
	FFprobe          Tool        `json:"ffprobe"`
}

func Files(ctx context.Context, reference, candidate string, options Options) (*Report, error) {
	ffmpeg, err := findTool(ctx, options.FFmpeg, "ffmpeg")
	if err != nil {
		return nil, err
	}
	ffprobe, err := findTool(ctx, options.FFprobe, "ffprobe")
	if err != nil {
		return nil, err
	}
	refPath, err := filepath.Abs(reference)
	if err != nil {
		return nil, err
	}
	canPath, err := filepath.Abs(candidate)
	if err != nil {
		return nil, err
	}
	refInfo, err := probe(ctx, ffprobe.Path, refPath, options.ReferenceStream)
	if err != nil {
		return nil, fmt.Errorf("reference: %w", err)
	}
	canInfo, err := probe(ctx, ffprobe.Path, canPath, options.CandidateStream)
	if err != nil {
		return nil, fmt.Errorf("candidate: %w", err)
	}
	if err := compatible(refInfo, canInfo); err != nil {
		return nil, err
	}
	floatMode := options.Float || floating(refInfo.SampleFormat) || floating(canInfo.SampleFormat)
	format, codec, width := "s32le", "pcm_s32le", 4
	if floatMode {
		format, codec, width = "f64le", "pcm_f64le", 8
	}
	report := &Report{Format: format, Reference: Source{Path: refPath, Audio: refInfo}, Candidate: Source{Path: canPath, Audio: canInfo}, FFmpeg: ffmpeg, FFprobe: ffprobe}
	if !floatMode {
		zero := uint64(0)
		report.MaxIntegerError = &zero
	}
	decodeCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	ref, err := startDecoder(decodeCtx, ffmpeg.Path, refPath, refInfo.Index, format, codec)
	if err != nil {
		return nil, fmt.Errorf("reference decoder: %w", err)
	}
	can, err := startDecoder(decodeCtx, ffmpeg.Path, canPath, canInfo.Index, format, codec)
	if err != nil {
		cancel()
		ref.stdout.Close()
		_ = ref.cmd.Wait()
		return nil, fmt.Errorf("candidate decoder: %w", err)
	}
	compareErr := compareReaders(ref.stdout, can.stdout, refInfo.Channels, width, report)
	if compareErr != nil {
		cancel()
	}
	ref.stdout.Close()
	can.stdout.Close()
	refErr, canErr := ref.cmd.Wait(), can.cmd.Wait()
	if compareErr != nil {
		return nil, compareErr
	}
	if refErr != nil {
		return nil, fmt.Errorf("reference decode: %w: %s", refErr, ref.stderr.String())
	}
	if canErr != nil {
		return nil, fmt.Errorf("candidate decode: %w: %s", canErr, can.stderr.String())
	}
	return report, nil
}

func compatible(a, b AudioInfo) error {
	if a.SampleRate != b.SampleRate {
		return fmt.Errorf("sample-rate mismatch: reference %d Hz, candidate %d Hz", a.SampleRate, b.SampleRate)
	}
	if a.Channels != b.Channels {
		return fmt.Errorf("channel-count mismatch: reference %d, candidate %d", a.Channels, b.Channels)
	}
	if a.ChannelLayout != b.ChannelLayout {
		return fmt.Errorf("channel-layout mismatch: reference %q, candidate %q", a.ChannelLayout, b.ChannelLayout)
	}
	for _, source := range []AudioInfo{a, b} {
		if !floating(source.SampleFormat) && (source.Bits > 32 || strings.HasPrefix(source.SampleFormat, "s64")) {
			return fmt.Errorf("integer source precision above 32 bits is unsupported")
		}
	}
	return nil
}

func probe(ctx context.Context, tool, path string, ordinal *int) (AudioInfo, error) {
	var raw struct {
		Streams []struct {
			Index    int    `json:"index"`
			Codec    string `json:"codec_name"`
			Format   string `json:"sample_fmt"`
			Rate     string `json:"sample_rate"`
			Channels int    `json:"channels"`
			Layout   string `json:"channel_layout"`
			Bits     int    `json:"bits_per_sample"`
			RawBits  string `json:"bits_per_raw_sample"`
		} `json:"streams"`
	}
	out, err := smallCommand(ctx, tool, "-v", "error", "-select_streams", "a", "-show_entries", "stream=index,codec_name,sample_fmt,sample_rate,channels,channel_layout,bits_per_sample,bits_per_raw_sample", "-of", "json", path)
	if err != nil {
		return AudioInfo{}, err
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return AudioInfo{}, fmt.Errorf("invalid ffprobe result: %w", err)
	}
	index := 0
	if ordinal == nil {
		if len(raw.Streams) != 1 {
			return AudioInfo{}, fmt.Errorf("found %d audio streams; specify an audio-stream ordinal", len(raw.Streams))
		}
	} else {
		index = *ordinal
		if index < 0 || index >= len(raw.Streams) {
			return AudioInfo{}, fmt.Errorf("audio-stream ordinal %d does not exist", index)
		}
	}
	s := raw.Streams[index]
	rate, err := strconv.Atoi(s.Rate)
	if err != nil || rate <= 0 || s.Channels <= 0 || s.Channels > 64 {
		return AudioInfo{}, fmt.Errorf("invalid audio format: sample rate %q, channels %d", s.Rate, s.Channels)
	}
	bits := s.Bits
	if rawBits, err := strconv.Atoi(s.RawBits); err == nil && rawBits > 0 {
		bits = rawBits
	}
	return AudioInfo{Index: s.Index, Codec: s.Codec, SampleFormat: s.Format, SampleRate: rate, Channels: s.Channels, ChannelLayout: s.Layout, Bits: bits}, nil
}

func floating(format string) bool {
	return format == "flt" || format == "fltp" || format == "dbl" || format == "dblp"
}

func findTool(ctx context.Context, path, fallback string) (Tool, error) {
	if path == "" {
		path = fallback
	}
	resolved, err := exec.LookPath(path)
	if err != nil {
		return Tool{}, err
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return Tool{}, err
	}
	out, err := smallCommand(ctx, resolved, "-version")
	if err != nil {
		return Tool{}, err
	}
	version := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
	return Tool{Path: resolved, Version: version}, nil
}

type limitedBuffer struct {
	data     []byte
	limit    int
	overflow bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	count := len(p)
	space := b.limit - len(b.data)
	if len(p) > space {
		p = p[:space]
		b.overflow = true
	}
	b.data = append(b.data, p...)
	return count, nil
}
func (b *limitedBuffer) String() string { return strings.TrimSpace(string(b.data)) }

func smallCommand(ctx context.Context, tool string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, tool, args...)
	output, stderr := &limitedBuffer{limit: 1 << 20}, &limitedBuffer{limit: 64 << 10}
	cmd.Stdout, cmd.Stderr = output, stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s: %w: %s", filepath.Base(tool), err, stderr.String())
	}
	if output.overflow {
		return nil, fmt.Errorf("%s output exceeded metadata limit", filepath.Base(tool))
	}
	return output.data, nil
}

type decoder struct {
	cmd    *exec.Cmd
	stdout io.ReadCloser
	stderr *limitedBuffer
}

func startDecoder(ctx context.Context, tool, path string, index int, format, codec string) (*decoder, error) {
	// Only the scalar representation changes. Rate/layout options are deliberately
	// absent; disabling layout guessing also preserves an unspecified layout.
	cmd := exec.CommandContext(ctx, tool, "-v", "error", "-nostdin", "-guess_layout_max", "0", "-i", path, "-map", fmt.Sprintf("0:%d", index), "-vn", "-sn", "-dn", "-c:a", codec, "-f", format, "pipe:1")
	d := &decoder{cmd: cmd, stderr: &limitedBuffer{limit: 64 << 10}}
	cmd.Stderr = d.stderr
	var err error
	d.stdout, err = cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		d.stdout.Close()
		return nil, err
	}
	return d, nil
}

func compareReaders(reference, candidate io.Reader, channels, width int, report *Report) error {
	if channels < 1 || (width != 4 && width != 8) {
		return fmt.Errorf("invalid PCM comparison format")
	}
	blockBytes := 4096 * channels * width
	a, b := make([]byte, blockBytes), make([]byte, blockBytes)
	doneA, doneB := false, false
	var position uint64
	for !doneA || !doneB {
		nA, nB := 0, 0
		var err error
		if !doneA {
			nA, doneA, err = readBlock(reference, a)
			if err != nil {
				return fmt.Errorf("reading reference PCM: %w", err)
			}
		}
		if !doneB {
			nB, doneB, err = readBlock(candidate, b)
			if err != nil {
				return fmt.Errorf("reading candidate PCM: %w", err)
			}
		}
		if nA%(channels*width) != 0 || nB%(channels*width) != 0 {
			return fmt.Errorf("decoder returned an incomplete PCM frame")
		}
		samplesA, samplesB := nA/width, nB/width
		report.Reference.Samples += uint64(samplesA)
		report.Candidate.Samples += uint64(samplesB)
		count := samplesA
		if samplesB > count {
			count = samplesB
		}
		for i := 0; i < count; i++ {
			presentA, presentB := i < samplesA, i < samplesB
			var valueA, valueB uint64
			if presentA {
				valueA = sampleBits(a[i*width:], width)
			}
			if presentB {
				valueB = sampleBits(b[i*width:], width)
			}
			if presentA && presentB {
				report.ComparedSamples++
			} else {
				report.MissingSamples++
			}
			if presentA && presentB && valueA == valueB {
				continue
			}
			report.DifferingSamples++
			if report.FirstDifference == nil {
				d := &Difference{Frame: (position + uint64(i)) / uint64(channels), Channel: int((position + uint64(i)) % uint64(channels))}
				if presentA {
					v := sampleString(valueA, width)
					d.ReferenceValue = &v
				}
				if presentB {
					v := sampleString(valueB, width)
					d.CandidateValue = &v
				}
				report.FirstDifference = d
			}
			if width == 4 && presentA && presentB && report.MaxIntegerError != nil {
				diff := int64(int32(valueA)) - int64(int32(valueB))
				if diff < 0 {
					diff = -diff
				}
				if uint64(diff) > *report.MaxIntegerError {
					*report.MaxIntegerError = uint64(diff)
				}
			}
		}
		position += uint64(count)
	}
	report.Reference.Frames = report.Reference.Samples / uint64(channels)
	report.Candidate.Frames = report.Candidate.Samples / uint64(channels)
	report.Exact = report.DifferingSamples == 0
	return nil
}

func readBlock(reader io.Reader, buffer []byte) (int, bool, error) {
	n, err := io.ReadFull(reader, buffer)
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return n, true, nil
	}
	return n, false, err
}
func sampleBits(data []byte, width int) uint64 {
	if width == 4 {
		return uint64(binary.LittleEndian.Uint32(data))
	}
	return binary.LittleEndian.Uint64(data)
}
func sampleString(bits uint64, width int) string {
	if width == 4 {
		return strconv.FormatInt(int64(int32(bits)), 10)
	}
	return strconv.FormatFloat(math.Float64frombits(bits), 'g', -1, 64)
}
