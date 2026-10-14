package app

import (
	"fmt"

	"eac3to/internal/aften"
	"eac3to/internal/audio"
)

func ac3Config(out Output, processor *audio.Processor, sampleRate int) (aften.Config, error) {
	var cfg aften.Config
	if !aften.Available() {
		return cfg, aften.ErrUnavailable
	}
	if out.Format != "ac3" {
		return cfg, fmt.Errorf("native AC-3 encoding currently needs an .ac3 output")
	}
	if processor.OutputChannels() > 6 {
		return cfg, fmt.Errorf("AC-3 supports at most six channels; select -down6 or -downStereo")
	}
	mask, err := audio.WaveChannelMask(processor.OutputLayout(), processor.OutputChannels())
	if err != nil {
		return cfg, err
	}
	cfg = aften.Config{SampleRate: sampleRate, Channels: processor.OutputChannels(), ChannelMask: mask, BitrateKbps: out.Request.Options.Bitrate}
	return cfg, aften.ValidateConfig(cfg)
}
