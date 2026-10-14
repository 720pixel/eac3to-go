package truehd

import (
	"bytes"
	"errors"
	"testing"
)

func TestDialogNormalizationNeutralCodesAndChecksum(t *testing.T) {
	b := units(t)
	var result []byte
	majorCount := 0
	p, _ := NewParser(ParserConfig{OnFrame: func(f Frame) error {
		before := append([]byte(nil), f.Bytes...)
		changed, err := RemoveDialogNormalization(f)
		if err != nil {
			t.Fatal(err)
		}
		if f.MajorSync {
			majorCount++
			if !changed {
				t.Fatal("zero metadata codes were treated as neutral")
			}
			h := f.Bytes[4:32]
			if (h[20]&0x7e)>>1 != 37 || ((h[21]&7)<<2|h[22]>>6) != 31 || ((h[23]&7)<<2|h[24]>>6) != 31 {
				t.Fatal("wrong neutral dialogue codes")
			}
			if _, err := parseMajor(h); err != nil {
				t.Fatal("metadata checksum was not repaired")
			}
			for i := range before {
				if before[i] != f.Bytes[i] && i != 24 && i != 25 && i != 26 && i != 27 && i != 28 && i != 30 && i != 31 {
					t.Fatalf("non-dialogue byte changed at %d", i)
				}
			}
			if changed, err = RemoveDialogNormalization(f); changed || err != nil {
				t.Fatal("normalization was not idempotent")
			}
		} else if changed || !bytes.Equal(before, f.Bytes) {
			t.Fatal("nonmajor bytes changed")
		}
		result = append(result, f.Bytes...)
		return nil
	}})
	if _, err := p.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := p.Finish(); err != nil {
		t.Fatal(err)
	}
	if majorCount != 1 || len(result) != len(b) {
		t.Fatal("metadata edit changed access unit boundaries")
	}
}

func TestAtmosNormalizationNoticePreservesEveryByte(t *testing.T) {
	b := append([]byte(nil), units(t)[:168]...)
	b[21] |= 0x80
	updateMajorChecksum(b[4:32])
	before := append([]byte(nil), b...)
	changed, err := RemoveDialogNormalization(Frame{Bytes: b, MajorSync: true})
	if changed || !errors.Is(err, ErrAtmosDialogNormalization) || !bytes.Equal(before, b) {
		t.Fatal("Atmos metadata limitation changed bytes or was not reported")
	}
}

func TestDialogNormalizationRejectsUncheckedMajor(t *testing.T) {
	b := units(t)[:168]
	for _, mutate := range []func([]byte){
		func(b []byte) { b[10] ^= 1 },
		func(b []byte) { b[0] = 0; b[1] = 0 },
		func(b []byte) { b[21] |= 0x80; updateMajorChecksum(b[4:32]) },
	} {
		copyFrame := append([]byte(nil), b...)
		mutate(copyFrame)
		before := append([]byte(nil), copyFrame...)
		if changed, err := RemoveDialogNormalization(Frame{Bytes: copyFrame, MajorSync: true}); err == nil || changed {
			t.Fatal("accepted invalid or unvalidated metadata")
		}
		if !bytes.Equal(before, copyFrame) {
			t.Fatal("failed transformation partially changed bytes")
		}
	}
}
