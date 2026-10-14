package app

import (
	"fmt"
	"io"
	"math"

	"eac3to/internal/ssrc"
)

type audioRateStage struct {
	input  int
	output float64
}

func frameRateParts(fps float64) (int, bool, error) {
	code := int(math.Round(fps * 1000))
	switch code {
	case 23976, 24975, 29970, 47952, 59940:
		return (code*1001 + 500000) / 1000000, true, nil
	case 24000, 25000, 30000, 48000, 50000, 60000:
		return code / 1000, false, nil
	default:
		return 0, false, fmt.Errorf("unsupported frame rate %.6f", fps)
	}
}

func frameSpeed(source, target float64) (float64, error) {
	from, sourceFractional, err := frameRateParts(source)
	if err != nil {
		return 0, err
	}
	to, targetFractional, err := frameRateParts(target)
	if err != nil {
		return 0, err
	}
	if sourceFractional && !targetFractional {
		return float64(to*1001) / float64(from*1000), nil
	}
	if targetFractional && !sourceFractional {
		return float64(to*1000) / float64(from*1001), nil
	}
	return float64(to) / float64(from), nil
}

func audioRatePlan(rate int, a AudioOptions) ([]audioRateStage, error) {
	var stages []audioRateStage
	if a.Speed != 1 {
		from, sourceFractional, err := frameRateParts(a.SourceFPS)
		if err != nil {
			return nil, err
		}
		to, targetFractional, err := frameRateParts(a.TargetFPS)
		if err != nil {
			return nil, err
		}
		if sourceFractional && !targetFractional {
			// This branch changes the filter's input clock to avoid a fractional output clock.
			input := int(math.RoundToEven(float64(rate*to*1001) / float64(from*1000)))
			stages = append(stages, audioRateStage{input: input, output: float64(rate)})
		} else {
			output := float64(rate*from) / float64(to)
			if targetFractional && !sourceFractional {
				output *= 1.001
			}
			stages = append(stages, audioRateStage{input: rate, output: output})
		}
	}
	if a.Rate != 0 && a.Rate != rate {
		stages = append(stages, audioRateStage{input: rate, output: float64(a.Rate)})
	}
	return stages, nil
}

type audioResamplers struct {
	stages []*ssrc.Resampler
}

func newAudioResamplers(rate, channels int, a AudioOptions) (*audioResamplers, error) {
	plan, err := audioRatePlan(rate, a)
	if err != nil {
		return nil, err
	}
	config := ssrc.DefaultConfig()
	if a.Steep {
		config = ssrc.SteepConfig()
	}
	chain := &audioResamplers{}
	for _, stage := range plan {
		r, err := ssrc.NewWithOutputRate(stage.input, stage.output, channels, config)
		if err != nil {
			chain.Close()
			return nil, err
		}
		chain.stages = append(chain.stages, r)
	}
	return chain, nil
}

func (c *audioResamplers) Write(samples []float64) ([]float64, error) {
	var err error
	for _, stage := range c.stages {
		samples, err = stage.Write(samples)
		if err != nil {
			return nil, err
		}
	}
	return samples, nil
}

func (c *audioResamplers) Finish(referenceTail bool, log io.Writer, write func([]float64) error) error {
	for i, stage := range c.stages {
		var tail []float64
		var err error
		if referenceTail {
			tail, err = stage.Finish()
		} else {
			tail, err = stage.FinishComplete()
		}
		if err != nil {
			return err
		}
		if stage.OutputFrames() != stage.ExpectedFrames() {
			if !referenceTail {
				return fmt.Errorf("SSRC produced %d of %d expected frames; refusing to publish audio with a missing tail", stage.OutputFrames(), stage.ExpectedFrames())
			}
			fmt.Fprintf(log, "Reference SSRC tail: %d of %d frames; this reproduces the Windows truncation.\n", stage.OutputFrames(), stage.ExpectedFrames())
		}
		for _, next := range c.stages[i+1:] {
			tail, err = next.Write(tail)
			if err != nil {
				return err
			}
		}
		if err := write(tail); err != nil {
			return err
		}
	}
	return nil
}

func (c *audioResamplers) Close() {
	for _, stage := range c.stages {
		stage.Close()
	}
}
