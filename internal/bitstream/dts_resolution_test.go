package bitstream

import (
	"bytes"
	"testing"
)

func TestDTSCoreResolutionPreservesESAndCRCHeaders(t *testing.T) {
	for _, code := range []int{0, 1, 2, 3, 5, 6} {
		for _, crc := range []bool{false, true} {
			for _, swapped := range []bool{false, true} {
				input := dtsCoreFixture(7, 4, crc)
				shift := 0
				if crc {
					shift = 16
				}
				writer := testBits{p: input, pos: 95 + shift}
				writer.put(3, code)
				want := bytes.Clone(input)
				if !crc {
					resolution := 6
					if code&1 != 0 {
						resolution = 5
					}
					writer = testBits{p: want, pos: 95}
					writer.put(3, resolution)
				}
				if swapped {
					swapWords(input)
					swapWords(want)
				}
				changed := !bytes.Equal(input, want)
				got, err := RepairDTSCoreResolution(input)
				if err != nil || got != changed || !bytes.Equal(input, want) {
					t.Fatalf("resolution%d crc%v swapped%v: changed%v, %v", code, crc, swapped, got, err)
				}
				got, err = RepairDTSCoreResolution(input)
				if err != nil || got {
					t.Fatal("resolution repair is not idempotent")
				}
			}
		}
	}
}

func TestDTSCopyRepairsResolutionWithoutChangingDialnorm(t *testing.T) {
	input := dtsCoreFixture(6, 12, false)
	writer := testBits{p: input, pos: 95}
	writer.put(3, 0)
	for _, remove := range []bool{false, true} {
		var output bytes.Buffer
		stats, err := CopyDTSHeaders(bytes.NewReader(input), &output, remove)
		if err != nil || stats.CoreFrames != 1 || stats.CoreResolutionChanges != 1 {
			t.Fatalf("remove%v: %+v %v", remove, stats, err)
		}
		got := output.Bytes()
		if getBitsAt(got, 95, 3) != 6 {
			t.Fatal("source resolution not repaired")
		}
		if remove {
			if getBitsAt(got, 100, 4) != 0 || getBitsAt(got, 89, 4) != 7 {
				t.Fatal("dialnorm not removed")
			}
		} else if getBitsAt(got, 100, 4) != 12 || getBitsAt(got, 89, 4) != 6 {
			t.Fatal("dialnorm changed while copying")
		}
		if !bytes.Equal(got[13:], input[13:]) {
			t.Fatal("compressed payload changed")
		}
	}
}
