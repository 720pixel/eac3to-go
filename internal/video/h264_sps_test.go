package video

import (
	"bytes"
	"math/bits"
	"testing"
)

type testBits struct{ bitBuilder }

func (b *testBits) ue(v uint32) {
	n := bits.Len32(v + 1)
	b.put(0, n-1)
	b.put(v+1, n)
}

func (b *testBits) se(v int32) {
	u := uint32(-2 * v)
	if v > 0 {
		u = uint32(2*v - 1)
	}
	b.ue(u)
}

func escapeSPS(rbsp []byte) []byte {
	result := []byte{0x67}
	zeros := 0
	for _, value := range rbsp {
		if zeros == 2 && value <= 3 {
			result = append(result, 3)
			zeros = 0
		}
		result = append(result, value)
		if value == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return result
}

func syntheticSPS(vui, timing, fixed, complex bool) []byte {
	b := testBits{}
	if complex {
		b.put(244, 8)
	} else {
		b.put(66, 8)
	}
	b.put(0, 8)
	b.put(40, 8)
	b.ue(3)
	if complex {
		b.ue(3)
		b.put(1, 1)
		b.ue(4)
		b.ue(4)
		b.put(1, 1)
		b.put(1, 1)
		for i := 0; i < 12; i++ {
			b.put(1, 1)
			if i == 0 {
				b.se(-8)
				continue
			}
			n := 16
			if i >= 6 {
				n = 64
			}
			for j := 0; j < n; j++ {
				b.se(0)
			}
		}
	}
	b.ue(4)
	if complex {
		b.ue(1)
		b.put(0, 1)
		b.se(-1)
		b.se(2)
		b.ue(3)
		b.se(-1)
		b.se(0)
		b.se(2)
	} else {
		b.ue(0)
		b.ue(4)
	}
	b.ue(4)
	b.put(0, 1)
	b.ue(119)
	b.ue(67)
	if complex {
		b.put(0, 1)
		b.put(1, 1)
	} else {
		b.put(1, 1)
	}
	b.put(1, 1)
	b.put(1, 1)
	b.ue(1)
	b.ue(2)
	b.ue(3)
	b.ue(4)
	if vui {
		b.put(1, 1)
		if complex {
			b.put(1, 1)
			b.put(255, 8)
			b.put(4, 16)
			b.put(3, 16)
			b.put(1, 1)
			b.put(0, 1)
			b.put(1, 1)
			b.put(5, 3)
			b.put(1, 1)
			b.put(1, 1)
			b.put(1, 8)
			b.put(2, 8)
			b.put(3, 8)
			b.put(1, 1)
			b.ue(4)
			b.ue(5)
		} else {
			b.put(0, 4)
		}
		if timing {
			b.put(1, 1)
			b.put(1, 32)
			b.put(50, 32)
			if fixed {
				b.put(1, 1)
			} else {
				b.put(0, 1)
			}
		} else {
			b.put(0, 1)
		}
		if complex {
			for j := 0; j < 2; j++ {
				b.put(1, 1)
				b.ue(1)
				b.put(3, 4)
				b.put(4, 4)
				for i := 0; i < 2; i++ {
					b.ue(0xfffffffd)
					b.ue(999999)
					b.put(1, 1)
				}
				b.put(0xfffff, 20)
			}
			b.put(1, 1)
			b.put(1, 1)
			b.put(1, 1)
			b.put(0, 1)
			b.ue(2)
			b.ue(1)
			b.ue(15)
			b.ue(16)
			b.ue(4)
			b.ue(5)
		} else {
			b.put(0, 4)
		}
	} else {
		b.put(0, 1)
	}
	b.put(1, 1)
	for b.bits%8 != 0 {
		b.put(0, 1)
	}
	return escapeSPS(b.data)
}

func bitString(data []byte, start, end int) []byte {
	result := make([]byte, end-start)
	for i := start; i < end; i++ {
		result[i-start] = data[i/8] >> uint(7-i%8) & 1
	}
	return result
}

func TestH264SPSPreservesAllOtherFields(t *testing.T) {
	for _, complex := range []bool{false, true} {
		for _, fixed := range []bool{false, true} {
			input := syntheticSPS(true, true, fixed, complex)
			before, err := parseSPS(input)
			if err != nil {
				t.Fatal(err)
			}
			output, original, err := RewriteH264SPS(input, H264Clock{1001, 48000})
			if err != nil {
				t.Fatal(err)
			}
			after, err := parseSPS(output)
			if err != nil {
				t.Fatal(err)
			}
			if original != before.info || after.info.Clock != (H264Clock{1001, 48000}) || after.info.FixedFrameRate != fixed || original.ID != 3 {
				t.Fatalf("wrong SPS fields: before=%+v after=%+v", original, after.info)
			}
			if complex && (original.ChromaFormatIDC != 3 || !original.SeparateColourPlane || original.BitDepthLuma != 12 || original.FrameMbsOnly) {
				t.Fatalf("complex SPS parsed incorrectly: %+v", original)
			}
			if !bytes.Equal(bitString(before.rbsp, 0, before.clockStart), bitString(after.rbsp, 0, after.clockStart)) ||
				!bytes.Equal(bitString(before.rbsp, before.clockStart+64, before.payloadEnd), bitString(after.rbsp, after.clockStart+64, after.payloadEnd)) {
				t.Fatal("changed an SPS field other than the clocks")
			}
			unchanged, _, err := RewriteH264SPS(output, H264Clock{1001, 48000})
			if err != nil || !bytes.Equal(unchanged, output) {
				t.Fatal("editing an identical clock changed its bytes")
			}
		}
	}
}

func TestH264SPSAddsMissingTiming(t *testing.T) {
	for _, vui := range []bool{false, true} {
		for _, complex := range []bool{false, true} {
			input := syntheticSPS(vui, false, false, complex)
			before, err := parseSPS(input)
			if err != nil {
				t.Fatal(err)
			}
			output, original, err := RewriteH264SPS(input, H264Clock{1000, 48000})
			if err != nil {
				t.Fatal(err)
			}
			after, err := parseSPS(output)
			if err != nil {
				t.Fatal(err)
			}
			if original.TimingPresent || !after.info.TimingPresent || !after.info.VUIPresent || after.info.FixedFrameRate || after.info.Clock != (H264Clock{1000, 48000}) {
				t.Fatalf("missing timing not inserted: %+v", after.info)
			}
			if !bytes.Equal(bitString(before.rbsp, 0, before.vuiFlag), bitString(after.rbsp, 0, after.vuiFlag)) {
				t.Fatal("inserting VUI changed SPS picture parameters")
			}
			if vui && (!bytes.Equal(bitString(before.rbsp, 0, before.timingFlag), bitString(after.rbsp, 0, after.timingFlag)) ||
				!bytes.Equal(bitString(before.rbsp, before.timingFlag+1, before.payloadEnd), bitString(after.rbsp, after.clockStart+65, after.payloadEnd))) {
				t.Fatal("inserting timing changed another VUI field")
			}
			var destination bytes.Buffer
			stats, err := RewriteH264(bytes.NewReader(append([]byte{0, 0, 1}, input...)), &destination, H264Clock{1000, 48000})
			if err != nil || stats.SPS != 1 || stats.AddedTimingSPS != 1 || stats.ChangedSPS != 1 {
				t.Fatalf("missing-timing accounting: %+v, %v", stats, err)
			}
		}
	}
}

func TestH264SPSRejectsDamage(t *testing.T) {
	valid := fromHex(t, originalSPS)
	for n := 0; n < len(valid); n++ {
		if _, err := ParseH264SPS(valid[:n]); err == nil {
			t.Fatalf("accepted truncated SPS at %d/%d", n, len(valid))
		}
	}
	mutate := func(pos int, value byte) []byte {
		p := append([]byte{}, valid...)
		p[pos] = value
		return p
	}
	invalid := [][]byte{
		mutate(0, 0xe7), mutate(0, 7), mutate(1, 255), mutate(2, 1),
		append(append([]byte{}, valid...), 0x80),
		append([]byte{0x67}, bytes.Repeat([]byte{0x55}, MaxH264SPSSize)...),
		{0x67, 0x42, 0, 0, 3}, {0x67, 0x42, 0, 0, 3, 4},
		{0x67, 0x42, 0, 0, 0, 0x80},
	}
	for _, input := range invalid {
		if _, err := ParseH264SPS(input); err == nil {
			t.Fatalf("accepted invalid SPS %x", input[:min(len(input), 40)])
		}
	}
	layout, _ := parseSPS(valid)
	for _, field := range []int{layout.clockStart, layout.clockStart + 32} {
		b := bitBuilder{}
		b.copy(layout.rbsp, 0, field)
		b.put(0, 32)
		b.copy(layout.rbsp, field+32, len(layout.rbsp)*8)
		if _, err := ParseH264SPS(escapeSPS(b.data)); err == nil {
			t.Fatal("accepted zero timing clock")
		}
	}
	// A corrupt loop count must fail before entering the declared iteration count.
	complex, _ := parseSPS(syntheticSPS(true, true, false, true))
	b := testBits{}
	count := complex.clockStart + 66
	b.copy(complex.rbsp, 0, count)
	b.ue(0xfffffffd)
	b.copy(complex.rbsp, count+3, len(complex.rbsp)*8)
	if _, err := ParseH264SPS(escapeSPS(b.data)); err == nil {
		t.Fatal("accepted oversized HRD loop count")
	}
}

func FuzzH264SPS(f *testing.F) {
	f.Add(fromHex(f, originalSPS))
	f.Add(syntheticSPS(false, false, false, false))
	f.Add(syntheticSPS(true, false, false, true))
	f.Fuzz(func(t *testing.T, input []byte) {
		if len(input) > 2*MaxH264SPSSize {
			t.Skip()
		}
		info, err := ParseH264SPS(input)
		edited, original, editErr := RewriteH264SPS(input, H264Clock{1001, 48000})
		if (err == nil) != (editErr == nil) {
			t.Fatalf("parse/edit disagree: %v, %v", err, editErr)
		}
		if err != nil {
			return
		}
		if original != info {
			t.Fatal("original fields changed")
		}
		result, err := ParseH264SPS(edited)
		if err != nil || result.Clock != (H264Clock{1001, 48000}) || result.FixedFrameRate != info.FixedFrameRate {
			t.Fatalf("edited SPS is invalid: %+v, %v", result, err)
		}
	})
}
