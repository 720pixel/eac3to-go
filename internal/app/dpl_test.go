package app

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func TestDPLCLIProducesReferenceSamples(t *testing.T) {
	tools := mediaTools(t)
	for _, tc := range []struct {
		channels int
		layout   string
		lfe      bool
		hash     string
	}{
		{8, "7.1", false, "c93febbfeb148853be18be6b1e87881ee3ad7e699c17378d1a69b10dd51ddaa3"},
		{8, "7.1", true, "6c703387dc4bc63d8a3d4427d96728662dd7aeb52c35ba423962bdd8c8b14d48"},
		{7, "6.1", false, "318acb47291ad046512891403f3ed7129b56864d4cc85a3ff9df4e3b337af22e"},
	} {
		dir := t.TempDir()
		pcm := make([]byte, 48000*tc.channels*8)
		for channel := 0; channel < tc.channels; channel++ {
			frame := 1000 * (channel + 1)
			binary.LittleEndian.PutUint64(pcm[(frame*tc.channels+channel)*8:], math.Float64bits(.25))
			binary.LittleEndian.PutUint64(pcm[((frame+100)*tc.channels+channel)*8:], math.Float64bits(-.25))
		}
		raw, source := filepath.Join(dir, "input.f64le"), filepath.Join(dir, "source.wav")
		if err := os.WriteFile(raw, pcm, 0600); err != nil {
			t.Fatal(err)
		}
		mediaCommand(t, tools.FFmpeg, "-v", "error", "-f", "f64le", "-ar", "48000", "-ac", fmt.Sprint(tc.channels), "-channel_layout", tc.layout, "-i", raw, "-c:a", "pcm_f64le", source)
		for _, flag := range []string{"-downDpl", "-down2"} {
			out := filepath.Join(dir, flag+".pcm")
			args := []string{source, out, flag, "-full", "-no2ndpass"}
			if tc.lfe {
				args = append(args, "-mixlfe")
			}
			if _, err := runTest(t, args...); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(out)
			if err != nil || len(data) != 48000*16 || fmt.Sprintf("%x", sha256.Sum256(data)) != tc.hash {
				t.Fatalf("%s %s LFE=%t: %d bytes %x, %v", tc.layout, flag, tc.lfe, len(data), sha256.Sum256(data), err)
			}
		}
	}
}
