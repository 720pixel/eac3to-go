package ssrc

import (
	"fmt"
	"math"
	"reflect"
	"testing"
)

func convert(t *testing.T, in, out, channels int, config Config, input []float64, chunk int, raw bool) []float64 {
	t.Helper()
	r, err := NewWithConfig(in, out, channels, config)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var result []float64
	for offset := 0; offset < len(input); {
		end := offset + chunk*channels
		if end > len(input) {
			end = len(input)
		}
		block, err := r.Write(input[offset:end])
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, block...)
		offset = end
	}
	var tail []float64
	if raw {
		tail, err = r.FinishRaw()
	} else {
		tail, err = r.Finish()
	}
	if err != nil {
		t.Fatal(err)
	}
	return append(result, tail...)
}

func TestIdentityAndLifecycle(t *testing.T) {
	r, err := New(48000, 48000, 2)
	if err != nil {
		t.Fatal(err)
	}
	input := []float64{.25, -.25, .5, -.5}
	output, err := r.Write(input)
	if err != nil || !reflect.DeepEqual(input, output) {
		t.Fatalf("identity: %v %v", output, err)
	}
	input[0] = 1
	if output[0] != .25 {
		t.Fatal("output aliases the input")
	}
	if _, err := r.Write([]float64{1}); err == nil {
		t.Fatal("accepted partial frame")
	}
	if _, err := r.Write([]float64{math.NaN(), 0}); err == nil {
		t.Fatal("accepted NaN")
	}
	if _, err := r.Finish(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Write([]float64{0, 0}); err == nil {
		t.Fatal("accepted input after Finish")
	}
	if tail, err := r.Finish(); err != nil || len(tail) != 0 {
		t.Fatalf("repeated Finish: %v %v", tail, err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := r.FinishRaw(); err == nil {
		t.Fatal("accepted FinishRaw after Close")
	}
}

func TestFractionalOutputClockKeepsPresentationLength(t *testing.T) {
	if !Available() {
		t.Skip("native SSRC requires Cgo")
	}
	for _, rate := range []float64{45937.5, 44144.1} {
		r, err := NewWithOutputRate(44100, rate, 1, DefaultConfig())
		if err != nil {
			t.Fatal(err)
		}
		var count int
		for remaining := 96000; remaining > 0; {
			n := min(remaining, 1027)
			out, err := r.Write(make([]float64, n))
			if err != nil {
				t.Fatal(err)
			}
			count += len(out)
			remaining -= n
		}
		tail, err := r.FinishComplete()
		if err != nil {
			t.Fatal(err)
		}
		count += len(tail)
		want := int(math.Floor(96000*rate/44100 + .5))
		if count != want || r.OutputFrames() != uint64(want) || r.ExpectedFrames() != uint64(want) {
			t.Fatalf("effective rate %g: got %d frames, want %d", rate, count, want)
		}
		r.Close()
	}
}

func TestRejectInvalidConfigurations(t *testing.T) {
	for _, rate := range []float64{47999.6, 48000.4} {
		if r, err := NewWithOutputRate(48000, rate, 2, DefaultConfig()); err == nil {
			r.Close()
			t.Fatalf("accepted fractional output clock %.1f without a resampling filter", rate)
		}
	}
	for _, args := range [][3]int{{0, 48000, 2}, {48000, -1, 2}, {48000, 48000, 0}, {48000, 48000, 33}, {48001, 96000, 2}} {
		if r, err := New(args[0], args[1], args[2]); err == nil {
			r.Close()
			t.Fatalf("accepted invalid rates/channels %v", args)
		}
	}
	c := DefaultConfig()
	c.FFTLength = 1234
	if r, err := NewWithConfig(48000, 44100, 2, c); err == nil {
		r.Close()
		t.Fatal("accepted non-power-of-two FFT length")
	}
	if !Available() {
		if _, err := New(48000, 44100, 2); err == nil {
			t.Fatal("Cgo-disabled conversion must report unavailable")
		}
	}
}

func TestChunkingAndChannelIsolation(t *testing.T) {
	if !Available() {
		t.Skip("native SSRC requires Cgo")
	}
	for _, rates := range [][2]int{{44100, 48000}, {48000, 44100}, {24000, 48000}, {48000, 24000}} {
		t.Run(fmt.Sprintf("%d-%d", rates[0], rates[1]), func(t *testing.T) {
			const channels = 2
			input := make([]float64, rates[0]*channels)
			for frame := 0; frame < rates[0]; frame++ {
				input[frame*channels] = .25
			}
			whole := convert(t, rates[0], rates[1], channels, DefaultConfig(), input, len(input)/channels, false)
			chunks := convert(t, rates[0], rates[1], channels, DefaultConfig(), input, 137, false)
			if !reflect.DeepEqual(whole, chunks) {
				t.Fatalf("chunk-dependent results: %d / %d samples", len(whole), len(chunks))
			}
			frames := len(whole) / channels
			if frames > rates[1] || frames < rates[1]/2 {
				t.Fatalf("unexpected output length %d", frames)
			}
			margin := frames / 4
			for frame := margin; frame < frames-margin; frame++ {
				if math.Abs(whole[frame*channels]-.25) > 1e-9 {
					t.Fatalf("DC gain at frame %d: %.15g", frame, whole[frame*channels])
				}
				if whole[frame*channels+1] != 0 {
					t.Fatalf("cross-channel leakage at frame %d", frame)
				}
			}
			if frames != rates[1] {
				t.Logf("original terminal block emitted %d/%d frames", frames, rates[1])
			}
		})
	}
}

func TestDownsamplingRejectsAliasing(t *testing.T) {
	if !Available() {
		t.Skip("native SSRC requires Cgo")
	}
	for _, frequency := range []float64{1000, 18000} {
		input := make([]float64, 48000)
		for i := range input {
			input[i] = .1 * math.Sin(2*math.Pi*frequency*float64(i)/48000)
		}
		output := convert(t, 48000, 24000, 1, DefaultConfig(), input, 509, false)
		var square float64
		count := 0
		for i := 2000; i < len(output)-2000; i++ {
			square += output[i] * output[i]
			count++
		}
		if count == 0 {
			t.Fatal("no steady output")
		}
		rms := math.Sqrt(square / float64(count))
		if frequency == 1000 && (rms < .0706 || rms > .0709) {
			t.Fatalf("passband RMS %g", rms)
		}
		if frequency == 18000 && rms > 1e-8 {
			t.Fatalf("aliased stopband RMS %g", rms)
		}
	}
}

func TestFinishCapsRawTailAndSteepProfile(t *testing.T) {
	if !Available() {
		t.Skip("native SSRC requires Cgo")
	}
	for _, config := range []Config{DefaultConfig(), SteepConfig()} {
		for _, frames := range []int{0, 1, 17, 4096, 8192, 16000} {
			input := make([]float64, frames)
			if frames > 0 {
				input[0] = 1
			}
			capped := convert(t, 44100, 48000, 1, config, input, 512, false)
			raw := convert(t, 44100, 48000, 1, config, input, 512, true)
			expected := int(math.Floor(float64(frames)*48000/44100 + .5))
			if len(capped) > expected || len(capped) > len(raw) {
				t.Fatalf("cap failed: %d frames => %d/%d, target %d", frames, len(capped), len(raw), expected)
			}
			if !reflect.DeepEqual(capped, raw[:len(capped)]) {
				t.Fatal("capping changed samples")
			}
			for _, sample := range raw {
				if !finite(sample) {
					t.Fatal("non-finite filter output")
				}
			}
		}
	}
}

func TestOriginalFlushFrameAccounting(t *testing.T) {
	if !Available() {
		t.Skip("native SSRC requires Cgo")
	}
	for _, tc := range []struct{ in, out, emitted int }{
		{44100, 48000, 47787}, {48000, 44100, 43899},
		{24000, 48000, 45057}, {48000, 24000, 22529},
	} {
		r, err := New(tc.in, tc.out, 1)
		if err != nil {
			t.Fatal(err)
		}
		input := make([]float64, tc.in)
		output, err := r.Write(input)
		if err != nil {
			r.Close()
			t.Fatal(err)
		}
		tail, err := r.Finish()
		if err != nil {
			r.Close()
			t.Fatal(err)
		}
		if len(output)+len(tail) != tc.emitted || r.InputFrames() != uint64(tc.in) || r.OutputFrames() != uint64(tc.emitted) || r.ExpectedFrames() != uint64(tc.out) {
			t.Errorf("%d→%d: input %d, output %d, expected %d", tc.in, tc.out, r.InputFrames(), r.OutputFrames(), r.ExpectedFrames())
		}
		r.Close()
	}
}

func convertComplete(t *testing.T, in, out, channels int, config Config, input []float64, chunk int) []float64 {
	t.Helper()
	r, err := NewWithConfig(in, out, channels, config)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	var result []float64
	for offset := 0; offset < len(input); {
		end := min(offset+chunk*channels, len(input))
		output, err := r.Write(input[offset:end])
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, output...)
		offset = end
	}
	tail, err := r.FinishComplete()
	if err != nil {
		t.Fatal(err)
	}
	result = append(result, tail...)
	if r.InputFrames() != uint64(len(input)/channels) || r.OutputFrames() != r.ExpectedFrames() || uint64(len(result)/channels) != r.ExpectedFrames() {
		t.Fatalf("incomplete accounting: input %d, output %d, expected %d", r.InputFrames(), r.OutputFrames(), r.ExpectedFrames())
	}
	if tail, err := r.FinishComplete(); len(tail) != 0 || err != nil {
		t.Fatalf("repeated completion returned %d samples, %v", len(tail), err)
	}
	if _, err := r.Write(make([]float64, channels)); err == nil {
		t.Fatal("accepted input after completion")
	}
	return result
}

func TestFinishCompleteMatchesOriginalFilterWithExtendedInput(t *testing.T) {
	if !Available() {
		t.Skip("native SSRC requires Cgo")
	}
	for _, config := range []Config{DefaultConfig(), SteepConfig()} {
		for _, rates := range [][2]int{{44100, 48000}, {48000, 44100}, {24000, 48000}, {48000, 24000}, {96000, 44100}, {8000, 192000}} {
			for _, frames := range []int{0, 1, 17, 8192, rates[0]} {
				t.Run(fmt.Sprintf("fft%d_%d-%d_%d", config.FFTLength, rates[0], rates[1], frames), func(t *testing.T) {
					const channels = 2
					input := make([]float64, frames*channels)
					for i := 0; i < frames; i++ {
						input[i*channels] = .25
						input[i*channels+1] = .1 * math.Sin(float64(i)*.037)
					}
					if frames > 0 {
						input[len(input)-1] = .75 // an end impulse exercises the retained tail
					}
					complete := convertComplete(t, rates[0], rates[1], channels, config, input, 137)
					expected := int(math.Floor(float64(frames)*float64(rates[1])/float64(rates[0]) + .5))
					if len(complete) != expected*channels {
						t.Fatalf("output length %d, expected %d", len(complete), expected*channels)
					}
					// Supply a separate, longer input to the unmodified finishing
					// path. Its prefix is the zero-boundary reference waveform.
					padded := append(append([]float64(nil), input...), make([]float64, 131072*channels)...)
					original := convert(t, rates[0], rates[1], channels, config, padded, 4093, false)
					if len(original) < len(complete) {
						t.Fatal("reference padding was too short")
					}
					for i, sample := range complete {
						if math.Float64bits(sample) != math.Float64bits(original[i]) {
							t.Fatalf("frame %d channel %d: complete %.17g, original %.17g", i/channels, i%channels, sample, original[i])
						}
					}
				})
			}
		}
	}
}

func TestFinishCompleteGainAndAliasing(t *testing.T) {
	if !Available() {
		t.Skip("native SSRC requires Cgo")
	}
	const channels = 3
	input := make([]float64, 48000*channels)
	for i := 0; i < 48000; i++ {
		input[i*channels] = .25
		input[i*channels+1] = .1 * math.Sin(2*math.Pi*1000*float64(i)/48000)
		input[i*channels+2] = .1 * math.Sin(2*math.Pi*18000*float64(i)/48000)
	}
	output := convertComplete(t, 48000, 24000, channels, DefaultConfig(), input, 509)
	if len(output) != 24000*channels {
		t.Fatal("wrong output duration")
	}
	var squares [2]float64
	for i := 2000; i < 22000; i++ {
		if math.Abs(output[i*channels]-.25) > 1e-9 {
			t.Fatalf("DC gain at frame %d: %g", i, output[i*channels])
		}
		for channel := 1; channel < channels; channel++ {
			sample := output[i*channels+channel]
			squares[channel-1] += sample * sample
		}
	}
	pass, stop := math.Sqrt(squares[0]/20000), math.Sqrt(squares[1]/20000)
	if pass < .0706 || pass > .0709 || stop > 1e-8 {
		t.Fatalf("passband RMS %g, stopband RMS %g", pass, stop)
	}
}

func TestFinishCompleteIdentity(t *testing.T) {
	input := []float64{.25, -.5, .75, 1}
	output := convertComplete(t, 48000, 48000, 2, DefaultConfig(), input, 1)
	if !reflect.DeepEqual(input, output) {
		t.Fatal("completion changed identity samples")
	}
}
