package app

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"eac3to/internal/aften"
	"eac3to/internal/audio"
)

func validateAudioOutput(out Output) error {
	a := out.Request.Options
	if out.Codec == "dts" || out.Codec == "aac" || out.Codec == "opus" || out.Codec == "vorbis" || out.Codec == "mp3" || out.Codec == "eac3" {
		return fmt.Errorf("%s encoding requires a verified native encoder path; refusing to substitute an encoder with different output quality", out.Codec)
	}
	if a.Bitrate != 0 && out.Codec != "ac3" {
		return fmt.Errorf("a lossy bitrate cannot be applied to %s output", out.Codec)
	}
	rate, err := strconv.Atoi(out.Track.Stream.SampleRate)
	if err != nil || rate <= 0 {
		return fmt.Errorf("track has no usable sample rate")
	}
	st := out.Track.Stream
	if _, err := audioRatePlan(rate, a); err != nil {
		return err
	}
	processor, err := audio.New(audio.Config{InputLayout: st.Layout, Channels: st.Channels, SampleRate: rate, GainDB: a.Gain, Correction: 1, DelayMS: a.DelayMS, Mix: a.Mix, MixLFE: a.MixLFE, ChannelMap: a.ChannelMap, ExtractChannel: a.ExtractChannel})
	if err != nil {
		return err
	}
	if out.Codec == "ac3" {
		if a.Rate != 0 {
			rate = a.Rate
		}
		_, err = ac3Config(out, processor, rate)
	}
	return err
}

func runAudio(ctx context.Context, t Tools, s *Source, out Output, o Options, log io.Writer) error {
	if s.PCMSourceDelay != nil {
		out.Request.Options.DelayMS += float64(*s.PCMSourceDelay)
	}
	if err := validateAudioOutput(out); err != nil {
		return err
	}
	a := out.Request.Options
	if canPackPCM(out) {
		return packPCM(ctx, t, s, out, o, log)
	}
	peak, e := audioPass(ctx, t, s, out, o, 1, log)
	if e != nil {
		return e
	}
	if peak > 0 && !a.NoSecondPass && (a.Normalize || peak > 1) {
		gain := 0.99 / peak
		if !o.Quiet {
			fmt.Fprintf(log, "Peak %.9f; repeating with %.6f dB correction.\n", peak, 20*math.Log10(gain))
		}
		_, e = audioPass(ctx, t, s, out, o, gain, log)
		return e
	}
	if a.ForceSecondPass && !a.Normalize && !a.NoSecondPass {
		_, e = audioPass(ctx, t, s, out, o, 1, log)
	}
	return e
}

func audioPass(ctx context.Context, t Tools, s *Source, out Output, o Options, correction float64, log io.Writer) (float64, error) {
	st := out.Track.Stream
	a := out.Request.Options
	verifier, e := newFLACVerifier(s, st)
	if e != nil {
		return 0, e
	}
	if verifier != nil && a.HDCD {
		if e := checkFLACSource(ctx, t, s, st, io.Discard); e != nil {
			return 0, e
		}
		verifier = nil
	}
	rate, e := strconv.Atoi(st.SampleRate)
	if e != nil || rate <= 0 {
		return 0, fmt.Errorf("track has no usable sample rate")
	}
	processor, e := audio.New(audio.Config{InputLayout: st.Layout, Channels: st.Channels, SampleRate: rate, GainDB: a.Gain, Correction: correction, DelayMS: a.DelayMS, Mix: a.Mix, MixLFE: a.MixLFE, ChannelMap: a.ChannelMap, ExtractChannel: a.ExtractChannel})
	if e != nil {
		return 0, e
	}
	outRate := rate
	if a.Rate != 0 {
		outRate = a.Rate
	}
	var quantizer *audio.Quantizer
	pcmInputFormat := "f64le"
	nativeAC3 := out.Codec == "ac3"
	if !strings.HasPrefix(out.Codec, "pcm_f") && (!nativeAC3 || a.Depth != 0) {
		var seedBytes [16]byte
		if _, e := io.ReadFull(rand.Reader, seedBytes[:]); e != nil {
			return 0, e
		}
		seed := make([]uint32, 4)
		for i := range seed {
			seed[i] = binary.LittleEndian.Uint32(seedBytes[i*4:])
		}
		integerSource := strings.HasPrefix(st.SampleFormat, "s") || strings.HasPrefix(st.SampleFormat, "u")
		preservesGrid := a.Gain == 0 && correction == 1 && a.Mix == "" && a.Rate == 0 && a.Speed == 1 && !a.HDCD
		if integerSource && preservesGrid {
			quantizer, e = audio.NewPCMQuantizerWithSeedKey(storageDepth(sampleDepth(st)), out.Depth, false, seed)
		} else {
			quantizer, e = audio.NewQuantizerWithSeedKey(out.Depth, a.Depth == 0, seed)
		}
		if e != nil {
			return 0, e
		}
		pcmInputFormat = "s32le"
	}
	resamplers, e := newAudioResamplers(rate, processor.OutputChannels(), a)
	if e != nil {
		return 0, e
	}
	defer resamplers.Close()
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	decodeArgs := []string{"-hide_banner", "-nostdin", "-v", "error", "-xerror"}
	if o.CodecThreads > 0 {
		decodeArgs = append(decodeArgs, "-threads", strconv.Itoa(o.CodecThreads))
	}
	decodeArgs = append(decodeArgs, inputArgs(s, o.Fast)...)
	decodeArgs = append(decodeArgs, "-map", fmt.Sprintf("0:%d", st.Index), "-vn", "-sn", "-dn")
	if s.Limit > 0 {
		decodeArgs = append(decodeArgs, "-t", fmt.Sprintf("%.9f", s.Limit))
	}
	if a.HDCD {
		decodeArgs = append(decodeArgs, "-af", "hdcd=disable_autoconvert=0")
	}
	decodeArgs = append(decodeArgs, "-c:a", "pcm_f64le", "-f", "f64le", "pipe:1")
	decoder := exec.CommandContext(jobCtx, t.audioDecoder(st.Codec, st.Profile), decodeArgs...)
	decErr := tailBuffer{Limit: 32 << 10}
	decoder.Stderr = &decErr
	encodeArgs := []string{"-hide_banner", "-nostdin", "-y", "-v", "error", "-xerror", "-f", pcmInputFormat, "-ar", strconv.Itoa(outRate), "-ac", strconv.Itoa(processor.OutputChannels())}
	if layout := processor.OutputLayout(); layout != "" {
		encodeArgs = append(encodeArgs, "-channel_layout", layout)
	}
	encodeArgs = append(encodeArgs, "-i", "pipe:0", "-map_metadata", "-1", "-c:a", out.Codec)
	if o.CodecThreads > 0 {
		encodeArgs = append(encodeArgs, "-threads", strconv.Itoa(o.CodecThreads))
	}
	if out.Codec == "flac" {
		if out.Depth <= 16 {
			encodeArgs = append(encodeArgs, "-sample_fmt", "s16")
		} else {
			encodeArgs = append(encodeArgs, "-sample_fmt", "s32")
		}
		encodeArgs = append(encodeArgs, "-bits_per_raw_sample", strconv.Itoa(out.Depth), "-compression_level", "8")
	}
	if out.Codec == "truehd" || out.Codec == "mlp" {
		return 0, fmt.Errorf("experimental lossless encoders have not been validated for this output")
	}
	encodeArgs = append(encodeArgs, "-f", out.Format)
	if out.Format == "wav" {
		mode := "auto"
		if ext(out.Path) == "rf64" {
			mode = "always"
		}
		encodeArgs = append(encodeArgs, "-rf64", mode)
	}
	encodeArgs = append(encodeArgs, out.Stage)
	var encoder *exec.Cmd
	var nativeEncoder *aften.Encoder
	var encoded io.WriteCloser
	var writer *bufio.Writer
	encErr := tailBuffer{Limit: 32 << 10}
	if nativeAC3 {
		cfg, e := ac3Config(out, processor, outRate)
		if e != nil {
			return 0, e
		}
		file, e := os.Create(out.Stage)
		if e != nil {
			return 0, e
		}
		encoded = file
		writer = bufio.NewWriterSize(file, 256<<10)
		nativeEncoder, e = aften.NewEncoder(writer, cfg)
		if e != nil {
			file.Close()
			return 0, e
		}
		defer nativeEncoder.Close()
	} else {
		encoder = exec.CommandContext(jobCtx, t.FFmpeg, encodeArgs...)
		encoder.Stderr = &encErr
		encoded, e = encoder.StdinPipe()
		if e != nil {
			return 0, e
		}
		if e = encoder.Start(); e != nil {
			encoded.Close()
			return 0, e
		}
		writer = bufio.NewWriterSize(encoded, 256<<10)
	}
	decoded, e := decoder.StdoutPipe()
	if e != nil {
		encoded.Close()
		cancel()
		if encoder != nil {
			encoder.Wait()
		}
		return 0, e
	}
	if e = decoder.Start(); e != nil {
		encoded.Close()
		cancel()
		if encoder != nil {
			encoder.Wait()
		}
		return 0, e
	}
	peak := 0.0
	frames := int64(0)
	var encoderRangeErr error
	writeSamples := func(samples []float64) error {
		if len(samples) == 0 {
			return nil
		}
		for _, v := range samples {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return fmt.Errorf("non-finite audio sample")
			}
			if math.Abs(v) > peak {
				peak = math.Abs(v)
			}
		}
		frames += int64(len(samples) / processor.OutputChannels())
		if nativeEncoder != nil {
			if encoderRangeErr != nil {
				return nil
			}
			if quantizer != nil {
				pcm, err := quantizer.Process(samples)
				if err != nil {
					return err
				}
				samples = make([]float64, len(pcm))
				for i, value := range pcm {
					samples[i] = float64(value) / 0x1p31
				}
			}
			err := nativeEncoder.Write(samples)
			if errors.Is(err, aften.ErrCoefficientRange) && correction == 1 && !a.NoSecondPass {
				// Finish the peak scan; the corrected pass will replace this stage.
				encoderRangeErr = err
				return nil
			}
			return err
		}
		var b []byte
		if quantizer != nil {
			pcm, e := quantizer.Process(samples)
			if e != nil {
				return e
			}
			b = make([]byte, len(pcm)*4)
			for i, v := range pcm {
				binary.LittleEndian.PutUint32(b[i*4:], uint32(v))
			}
		} else {
			b = make([]byte, len(samples)*8)
			for i, v := range samples {
				binary.LittleEndian.PutUint64(b[i*8:], math.Float64bits(v))
			}
		}
		_, e := writer.Write(b)
		return e
	}
	feed := func(samples []float64) error {
		samples, e := resamplers.Write(samples)
		if e != nil {
			return e
		}
		return writeSamples(samples)
	}
	processErr := func() error {
		for {
			silence := processor.DrainDelay()
			if len(silence) == 0 {
				break
			}
			if e := feed(silence); e != nil {
				return e
			}
		}
		frameBytes := st.Channels * 8
		if frameBytes <= 0 {
			return fmt.Errorf("invalid channel count")
		}
		raw := make([]byte, 4096*frameBytes)
		for {
			n, readErr := io.ReadFull(decoded, raw)
			if n%frameBytes != 0 {
				return fmt.Errorf("decoder returned an incomplete PCM frame")
			}
			if n > 0 {
				samples := make([]float64, n/8)
				for i := range samples {
					samples[i] = math.Float64frombits(binary.LittleEndian.Uint64(raw[i*8:]))
				}
				if verifier != nil {
					if e := verifier.WriteF64(samples); e != nil {
						return e
					}
				}
				mixed, e := processor.Process(samples)
				if e != nil {
					return e
				}
				if e = feed(mixed); e != nil {
					return e
				}
			}
			if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
				break
			}
			if readErr != nil {
				return readErr
			}
		}
		if verifier != nil {
			if _, e := verifier.Finish(); e != nil {
				return e
			}
		}
		tail, e := processor.Finish()
		if e != nil {
			return e
		}
		if e = feed(tail); e != nil {
			return e
		}
		if err := resamplers.Finish(a.ReferenceTail, log, writeSamples); err != nil {
			return err
		}
		if nativeEncoder != nil && encoderRangeErr == nil {
			if err := nativeEncoder.Finish(); err != nil {
				if errors.Is(err, aften.ErrCoefficientRange) && correction == 1 && !a.NoSecondPass && peak > 1 {
					encoderRangeErr = err
					return nil
				}
				return err
			}
		}
		return writer.Flush()
	}()
	if err := encoded.Close(); err != nil && processErr == nil {
		processErr = err
	}
	decoded.Close()
	if processErr != nil {
		cancel()
	}
	de := decoder.Wait()
	if de != nil {
		cancel()
	}
	var ee error
	if encoder != nil {
		ee = encoder.Wait()
	}
	if processErr != nil {
		return peak, fmt.Errorf("audio processing: %w\n%s\n%s", processErr, decErr.String(), encErr.String())
	}
	if err := strictToolError("audio decoder", de, &decErr); err != nil {
		return peak, err
	}
	if err := strictToolError("audio encoder", ee, &encErr); err != nil {
		return peak, err
	}
	if frames == 0 {
		return peak, fmt.Errorf("track produced no PCM frames")
	}
	if encoderRangeErr != nil && peak <= 1 {
		return peak, encoderRangeErr
	}
	if out.Format == "flac" || out.Format == "wav" || out.Format == "w64" {
		written, e := probe(ctx, t, []string{"-i", out.Stage})
		if e != nil {
			return peak, e
		}
		if len(written.Streams) != 1 {
			return peak, fmt.Errorf("encoder produced an unexpected stream count")
		}
		actual := written.Streams[0]
		if actual.SampleRate != strconv.Itoa(outRate) || actual.Channels != processor.OutputChannels() {
			return peak, fmt.Errorf("encoder changed the sample rate or channel count")
		}
		if sampleDepth(&actual) < out.Depth {
			return peak, fmt.Errorf("encoder stored only %d of %d requested bits", sampleDepth(&actual), out.Depth)
		}
	}
	return peak, nil
}
