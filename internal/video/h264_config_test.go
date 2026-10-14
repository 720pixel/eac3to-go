package video

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func avcConfig(sps ...[]byte) []byte {
	data := []byte{1, 100, 0, 10, 0xff, 0xe0 | byte(len(sps))}
	for _, set := range sps {
		data = binary.BigEndian.AppendUint16(data, uint16(len(set)))
		data = append(data, set...)
	}
	data = append(data, 1, 0, 6, 0x68, 0xeb, 0xe0, 0x8c, 0xb2, 0x2c)
	return append(data, 0xfd, 0xf8, 0xf8, 0)
}

func TestH264ConfigFormats(t *testing.T) {
	original := fromHex(t, originalSPS)
	changed, _, err := RewriteH264SPS(original, H264Clock{1000, 48000})
	if err != nil {
		t.Fatal(err)
	}
	annex := append([]byte{0, 0, 0, 1}, original...)
	annex = append(annex, 0, 0, 1)
	annex = append(annex, changed...)
	annex = append(annex, 0, 0, 0, 1, 0x68, 0xeb, 0xe0, 0x8c, 0xb2, 0x2c)
	for _, config := range [][]byte{annex, avcConfig(original, changed)} {
		sets, err := ParseH264Config(config)
		if err != nil || len(sets) != 2 || sets[0].Clock != (H264Clock{1, 50}) || sets[1].Clock != (H264Clock{1000, 48000}) {
			t.Fatalf("unexpected parameter sets: %+v, %v", sets, err)
		}
	}
	config := avcConfig(original)
	if _, err := ParseH264Config(config[:len(config)-4]); err != nil {
		t.Fatalf("older AVC config without optional high-profile extension: %v", err)
	}
}

func TestH264ConfigRejectsBadRecords(t *testing.T) {
	valid := avcConfig(fromHex(t, originalSPS))
	for n := 0; n < len(valid); n++ {
		if n == len(valid)-4 {
			continue
		}
		if _, err := ParseH264Config(valid[:n]); err == nil {
			t.Fatalf("accepted truncated AVC configuration at %d", n)
		}
	}
	for _, at := range []int{4, 5, len(valid) - 4, len(valid) - 3, len(valid) - 2} {
		input := bytes.Clone(valid)
		input[at] = 0
		if _, err := ParseH264Config(input); err == nil {
			t.Fatalf("accepted corrupt reserved config bits at %d", at)
		}
	}
	for _, input := range [][]byte{
		append(bytes.Clone(valid), 1),
		append([]byte{0, 0, 1, 0x6f}, fromHex(t, originalSPS)[1:]...),
		{0, 0, 1}, {0, 0, 1, 0x68, 0x80},
		append([]byte{0x55, 0, 0, 1}, fromHex(t, originalSPS)...),
		bytes.Repeat([]byte{0}, (1<<20)+1),
	} {
		if _, err := ParseH264Config(input); err == nil {
			t.Fatal("accepted invalid or unsupported codec data")
		}
	}
}

func TestH264PicStructField(t *testing.T) {
	for _, complex := range []bool{false, true} {
		info, err := ParseH264SPS(syntheticSPS(true, true, false, complex))
		if err != nil || info.PicStructPresent != complex {
			t.Fatalf("picture structure flag: %+v, %v", info, err)
		}
	}
}

func FuzzH264Config(f *testing.F) {
	f.Add(avcConfig(fromHex(f, originalSPS)))
	f.Add(append([]byte{0, 0, 1}, fromHex(f, originalSPS)...))
	f.Fuzz(func(t *testing.T, input []byte) {
		sets, err := ParseH264Config(input)
		if err == nil && (len(sets) == 0 || len(sets) > 32) {
			t.Fatal("configuration escaped its SPS bound")
		}
	})
}
