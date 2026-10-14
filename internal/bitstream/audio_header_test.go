package bitstream

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func TestParseAudioHeaderRatesAndBlocks(t *testing.T) {
	rates := []int{48000, 44100, 32000}
	for _, bsid := range []int{6, 8, 9, 10} {
		for code, rate := range rates {
			for sizeCode := 0; sizeCode <= 37; sizeCode++ {
				input, _ := metadataFrame(false, bsid, 2, 0, code, sizeCode)
				wantRate := rate
				if bsid > 8 {
					wantRate >>= bsid - 8
				}
				for _, swapped := range []bool{false, true} {
					prefix := append([]byte(nil), input[:8]...)
					if swapped {
						swapWords(prefix)
					}
					before := append([]byte(nil), prefix...)
					got, err := ParseAudioHeader(prefix)
					if err != nil || got.Size != len(input) || got.SampleRate != wantRate || got.Samples != 1536 || got.Enhanced || got.ByteSwapped != swapped {
						t.Fatalf("AC-3 bsid %d rate %d size %d: %+v, %v", bsid, code, sizeCode, got, err)
					}
					if !bytes.Equal(prefix, before) {
						t.Fatal("header parser changed input")
					}
				}
			}
		}
	}
	for code := 0; code < 4; code++ {
		for blocks := 0; blocks < 4; blocks++ {
			if code == 3 && blocks == 3 {
				continue
			}
			prefix := []byte{0x0b, 0x77, 7, 0xff, byte(code<<6 | blocks<<4), 16 << 3, 0, 0}
			wantRate, wantSamples := 0, 1536
			if code == 3 {
				wantRate = rates[blocks] / 2
			} else {
				wantRate, wantSamples = rates[code], 256*[]int{1, 2, 3, 6}[blocks]
			}
			got, err := ParseAudioHeader(prefix)
			if err != nil || !got.Enhanced || got.Size != 4096 || got.SampleRate != wantRate || got.Samples != wantSamples {
				t.Fatalf("E-AC-3 rate %d blocks %d: %+v, %v", code, blocks, got, err)
			}
		}
	}
}

func TestParseAudioHeaderRejectsShortAndReserved(t *testing.T) {
	for n := 0; n < 8; n++ {
		if _, err := ParseAudioHeader(make([]byte, n)); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("length %d: %v", n, err)
		}
	}
	for _, input := range [][]byte{
		{0x0b, 0x77, 0, 0, 0, 11 << 3, 0, 0},
		{0x0b, 0x77, 0xc0, 0xff, 0, 16 << 3, 0, 0},
		{0x0b, 0x77, 0, 0xff, 0xf0, 16 << 3, 0, 0},
	} {
		if _, err := ParseAudioHeader(input); err == nil {
			t.Fatalf("reserved header accepted: %x", input)
		}
	}
}

func TestValidateAudioFrame(t *testing.T) {
	for _, enhanced := range []bool{false, true} {
		frame, _ := metadataFrame(enhanced, 8, 2, 0, 0, 0)
		for _, swapped := range []bool{false, true} {
			input := bytes.Clone(frame)
			if swapped {
				swapWords(input)
			}
			before := bytes.Clone(input)
			if err := ValidateAudioFrame(input); err != nil {
				t.Fatalf("enhanced=%v swapped=%v: %v", enhanced, swapped, err)
			}
			if !bytes.Equal(input, before) {
				t.Fatal("validation mutated input")
			}
			bad := bytes.Clone(input)
			bad[len(bad)-1] ^= 1
			for _, invalid := range [][]byte{input[:7], input[:len(input)-1], append(bytes.Clone(input), 0), bad} {
				if err := ValidateAudioFrame(invalid); err == nil {
					t.Fatalf("enhanced=%v swapped=%v accepted damaged frame", enhanced, swapped)
				}
			}
		}
	}
	frame, _ := metadataFrame(false, 8, 2, 0, 0, 0)
	if allocations := testing.AllocsPerRun(100, func() {
		if err := ValidateAudioFrame(frame); err != nil {
			panic(err)
		}
	}); allocations != 0 {
		t.Fatalf("ordinary frame validation allocated %v times", allocations)
	}
}
