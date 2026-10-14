package audio

import "testing"

func TestWaveChannelMaskKeepsSpeakerOrder(t *testing.T) {
	for _, tc := range []struct {
		layout   string
		channels int
		mask     uint32
	}{
		{"mono", 1, 0x4},
		{"", 2, 0x3},
		{"5.1", 6, 0x3f},
		{"5.1(side)", 6, 0x60f},
		{"FL+FR+BC", 3, 0x103},
		{"7.1", 8, 0x63f},
	} {
		mask, err := WaveChannelMask(tc.layout, tc.channels)
		if err != nil || mask != tc.mask {
			t.Fatalf("%s/%d: mask %x, error %v", tc.layout, tc.channels, mask, err)
		}
	}
	for _, tc := range []struct {
		layout   string
		channels int
	}{{"", 6}, {"FR+FL", 2}, {"FC+LFE+FL+FR", 4}, {"FL+FR", 3}, {"FL+unknown", 2}} {
		if _, err := WaveChannelMask(tc.layout, tc.channels); err == nil {
			t.Fatalf("accepted ambiguous layout %s/%d", tc.layout, tc.channels)
		}
	}
}
