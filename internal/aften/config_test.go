package aften

import (
	"errors"
	"testing"
)

func TestEncoderConfig(t *testing.T) {
	for _, c := range []Config{
		{SampleRate: 48000, Channels: 1},
		{SampleRate: 44100, Channels: 2, BitrateKbps: 192},
		{SampleRate: 32000, Channels: 6, ChannelMask: 0x60f, BitrateKbps: 640},
	} {
		if err := ValidateConfig(c); err != nil {
			t.Fatal(err)
		}
	}
	for _, c := range []Config{
		{SampleRate: 96000, Channels: 2},
		{SampleRate: 48000, Channels: 8},
		{SampleRate: 48000, Channels: 0},
		{SampleRate: 48000, Channels: 2, ChannelMask: 4},
		{SampleRate: 48000, Channels: 2, ChannelMask: 0x30000},
		{SampleRate: 48000, Channels: 2, BitrateKbps: 193},
	} {
		if err := ValidateConfig(c); !errors.Is(err, ErrConfig) {
			t.Fatalf("%+v: %v", c, err)
		}
	}
}
