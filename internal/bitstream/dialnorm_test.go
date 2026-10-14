package bitstream

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"testing"
	"testing/iotest"
)

// This deliberately uses a bit-at-a-time implementation, independent of the
// lookup table and reverse updates used in production.
func referenceCRC(p []byte) uint16 {
	var crc uint16
	for _, v := range p {
		for mask := byte(0x80); mask != 0; mask >>= 1 {
			feedback := (crc & 0x8000) != 0
			if v&mask != 0 {
				feedback = !feedback
			}
			crc <<= 1
			if feedback {
				crc ^= 0x8005
			}
		}
	}
	return crc
}

func TestCRC16KnownVectorAndReversal(t *testing.T) {
	if got := crc16ANSI([]byte("123456789")); got != 0xfee8 {
		t.Fatalf("CRC-16/BUYPASS check vector = %04x, want fee8", got)
	}
	for size := 0; size < 500; size++ {
		p := make([]byte, size)
		for i := range p {
			p[i] = byte(i*37 + size*19)
		}
		got := crc16ANSI(p)
		if got != referenceCRC(p) || undoCRC16(got, p) != 0 {
			t.Fatalf("bad CRC or reverse at size %d", size)
		}
	}
}

type testBits struct {
	p   []byte
	pos int
}

func (b *testBits) put(n, v int) {
	for i := n - 1; i >= 0; i-- {
		mask := byte(1 << uint(7-b.pos%8))
		b.p[b.pos/8] &^= mask
		if v&(1<<uint(i)) != 0 {
			b.p[b.pos/8] |= mask
		}
		b.pos++
	}
}

func metadataFrame(enhanced bool, bsid, acmod, options, rateCode, sizeCode int) ([]byte, []int) {
	size := 256
	if !enhanced {
		h := []byte{0x0b, 0x77, 0, 0, byte(rateCode<<6 | sizeCode), byte(bsid << 3), 0, 0}
		size, _, _ = audioFrameSize(h)
	}
	p := make([]byte, size)
	for i := range p {
		p[i] = byte(i*37 + 13)
	}
	b := testBits{p: p}
	b.put(16, 0x0b77)
	if enhanced {
		b.put(2, 0) // independent stream
		b.put(3, 0) // substream zero
		b.put(11, size/2-1)
		b.put(2, rateCode)
		b.put(2, 0) // one block, or 24 kHz for fscod == 3
		b.put(3, acmod)
		b.put(1, 1)
		b.put(5, 16)
	} else {
		b.put(16, 0)
		b.put(2, rateCode)
		b.put(6, sizeCode)
		b.put(5, bsid)
		b.put(3, 0)
		b.put(3, acmod)
		if acmod&1 != 0 && acmod != 1 {
			b.put(2, 1)
		}
		if acmod&4 != 0 {
			b.put(2, 1)
		}
		if acmod == 2 {
			b.put(2, 2)
		}
		b.put(1, 1)
	}
	var positions []int
	programs := 1
	if acmod == 0 {
		programs = 2
	}
	for i := 0; i < programs; i++ {
		positions = append(positions, b.pos)
		b.put(5, 27-i)
		b.put(1, options&1)
		if options&1 != 0 {
			b.put(8, 0xa4)
		}
		if !enhanced {
			b.put(1, (options>>1)&1)
			if options&2 != 0 {
				b.put(8, 0x55)
			}
			b.put(1, (options>>2)&1)
			if options&4 != 0 {
				b.put(7, 0x4b)
			}
		}
	}
	updateAudioCRC(p, enhanced)
	return p, positions
}

func assertAudioIntegrity(t *testing.T, original, edited []byte, enhanced bool, positions []int) {
	t.Helper()
	if len(original) != len(edited) {
		t.Fatal("frame size changed")
	}
	if referenceCRC(edited[2:]) != 0 {
		t.Fatal("nonzero whole-frame CRC")
	}
	if !enhanced {
		// Calculate the split independently in sixteen-bit words.
		words := len(edited) / 2
		split := 2 * (words/2 + words/8)
		if referenceCRC(edited[2:split]) != 0 || referenceCRC(edited[split:]) != 0 {
			t.Fatal("nonzero AC-3 partition CRC")
		}
	}
	allowed := make([]byte, len(original))
	for _, pos := range positions {
		if getBitsAt(edited, pos, 5) != 31 {
			t.Fatalf("dialnorm at bit %d is not 31", pos)
		}
		for i := 0; i < 5; i++ {
			allowed[(pos+i)/8] |= 1 << uint(7-(pos+i)%8)
		}
	}
	if !enhanced {
		allowed[2], allowed[3] = 0xff, 0xff
	}
	allowed[len(allowed)-2], allowed[len(allowed)-1] = 0xff, 0xff
	allowed[len(allowed)-3] = 1 // reserved CRC syncword-avoidance bit
	for i := range edited {
		if (original[i]^edited[i]) & ^allowed[i] != 0 {
			t.Fatalf("audio/other metadata changed at byte %d: %02x -> %02x", i, original[i], edited[i])
		}
	}
}

func TestDialnormAC3ModesDualMonoAndReducedRates(t *testing.T) {
	for _, bsid := range []int{6, 8, 9, 10} {
		for mode := 0; mode < 8; mode++ {
			for options := 0; options < 8; options++ {
				t.Run(fmt.Sprintf("bsid%d_mode%d_options%d", bsid, mode, options), func(t *testing.T) {
					p, positions := metadataFrame(false, bsid, mode, options, 0, 0)
					var out bytes.Buffer
					stats, err := RemoveDialnorm(iotest.OneByteReader(bytes.NewReader(p)), &out)
					if err != nil {
						t.Fatal(err)
					}
					assertAudioIntegrity(t, p, out.Bytes(), false, positions)
					if stats.AC3Frames != 1 || stats.ChangedFrames != 1 || stats.ChangedFields != int64(len(positions)) {
						t.Fatalf("bad stats %+v", stats)
					}
				})
			}
		}
	}
}

func TestDialnormAC3FrameSizes(t *testing.T) {
	for rate := 0; rate < 3; rate++ {
		for size := 0; size < 38; size++ {
			p, positions := metadataFrame(false, 8, 2, 0, rate, size)
			var out bytes.Buffer
			if _, err := RemoveDialnorm(bytes.NewReader(p), &out); err != nil {
				t.Fatalf("rate %d size %d: %v", rate, size, err)
			}
			assertAudioIntegrity(t, p, out.Bytes(), false, positions)
		}
	}
}

func TestDialnormEAC3ModesAndSubstreams(t *testing.T) {
	for mode := 0; mode < 8; mode++ {
		for rate := 0; rate < 4; rate++ {
			for frameType := 0; frameType < 3; frameType++ {
				for substream := 0; substream < 8; substream++ {
					p, positions := metadataFrame(true, 16, mode, 1, rate, 0)
					p[2] = p[2]&7 | byte(frameType<<6|substream<<3)
					updateAudioCRC(p, true)
					var out bytes.Buffer
					stats, err := RemoveDialnorm(bytes.NewReader(p), &out)
					if err != nil {
						t.Fatal(err)
					}
					assertAudioIntegrity(t, p, out.Bytes(), true, positions)
					if stats.EAC3Frames != 1 || stats.ChangedFields != int64(len(positions)) {
						t.Fatalf("bad stats %+v", stats)
					}
				}
			}
		}
	}
}

func TestDialnormMultipleFramesWordSwapAndIdempotence(t *testing.T) {
	var source []byte
	for _, enhanced := range []bool{false, true} {
		p, _ := metadataFrame(enhanced, 8, 0, 7, 0, 0)
		source = append(source, p...)
		swapWords(p)
		source = append(source, p...)
	}
	var once, twice bytes.Buffer
	stats, err := RemoveDialnorm(bytes.NewReader(source), &once)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Frames != 4 || stats.AC3Frames != 2 || stats.EAC3Frames != 2 || stats.ByteSwappedFrames != 2 || stats.ChangedFields != 8 || stats.BytesRead != int64(len(source)) || stats.BytesWritten != int64(len(source)) {
		t.Fatalf("bad stats %+v", stats)
	}
	stats, err = RemoveDialnorm(bytes.NewReader(once.Bytes()), &twice)
	if err != nil || !bytes.Equal(once.Bytes(), twice.Bytes()) || stats.ChangedFrames != 0 {
		t.Fatalf("not idempotent: %+v, %v", stats, err)
	}
	if once.Bytes()[0] != 0x0b || once.Bytes()[128] != 0x77 || once.Bytes()[256] != 0x0b || once.Bytes()[512] != 0x77 {
		t.Fatal("byte order changed")
	}
}

func TestAudioCRCDoesNotEmulateSyncWord(t *testing.T) {
	for _, enhanced := range []bool{false, true} {
		p, _ := metadataFrame(enhanced, 8, 2, 0, 0, 0)
		start := 2
		if !enhanced {
			start = ac3FirstCRCSize(len(p))
		}
		found := false
		for v := 0; v <= 65535; v++ {
			binary.BigEndian.PutUint16(p[len(p)-5:len(p)-3], uint16(v))
			if crc16ANSI(p[start:len(p)-2]) == 0x0b77 {
				found = true
				break
			}
		}
		if !found {
			t.Fatal("could not construct CRC syncword fixture")
		}
		reserved := p[len(p)-3]
		updateAudioCRC(p, enhanced)
		if p[len(p)-3] != reserved^1 || binary.BigEndian.Uint16(p[len(p)-2:]) != 0x8b72 || referenceCRC(p[2:]) != 0 {
			t.Fatalf("bad syncword escape (E-AC-3 %v)", enhanced)
		}
	}
}

func TestDialnormRejectsMalformed(t *testing.T) {
	ac3, _ := metadataFrame(false, 8, 2, 0, 0, 0)
	eac3, _ := metadataFrame(true, 16, 2, 0, 0, 0)
	corrupt := func(p []byte, at int, value byte) []byte {
		q := bytes.Clone(p)
		q[at] = value
		return q
	}
	cases := map[string][]byte{
		"empty":              nil,
		"truncated header":   ac3[:5],
		"truncated frame":    ac3[:len(ac3)-1],
		"bad sync":           corrupt(ac3, 0, 0x0a),
		"bad CRC1":           corrupt(ac3, 2, ac3[2]^1),
		"bad CRC2":           corrupt(ac3, len(ac3)-1, ac3[len(ac3)-1]^1),
		"bad EAC3 CRC":       corrupt(eac3, len(eac3)-1, eac3[len(eac3)-1]^1),
		"reserved AC3 rate":  corrupt(ac3, 4, 0xc0),
		"reserved AC3 size":  corrupt(ac3, 4, 38),
		"reserved bsid":      corrupt(ac3, 5, 11<<3),
		"reserved EAC3 type": corrupt(eac3, 2, eac3[2]|0xc0),
		"reserved EAC3 rate": corrupt(eac3, 4, 0xf0),
		"too short EAC3":     corrupt(eac3, 3, 0),
	}
	for name, source := range cases {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			if _, err := RemoveDialnorm(bytes.NewReader(source), &out); err == nil {
				t.Fatal("accepted malformed bitstream")
			}
			if out.Len() != 0 {
				t.Fatal("wrote a malformed frame")
			}
		})
	}
	if _, err := RemoveDialnorm(bytes.NewReader(ac3), shortWriter{}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("missing short-write error: %v", err)
	}
	want := errors.New("source failed")
	if _, err := RemoveDialnorm(iotest.ErrReader(want), io.Discard); !errors.Is(err, want) {
		t.Fatalf("lost read error: %v", err)
	}
}

func FuzzRemoveDialnorm(f *testing.F) {
	for _, enhanced := range []bool{false, true} {
		frame, _ := metadataFrame(enhanced, 8, 0, 7, 0, 0)
		f.Add(frame)
	}
	f.Fuzz(func(t *testing.T, source []byte) {
		var once, twice bytes.Buffer
		if _, err := RemoveDialnorm(bytes.NewReader(source), &once); err == nil {
			if len(source) != once.Len() {
				t.Fatal("stream size changed")
			}
			stats, err := RemoveDialnorm(bytes.NewReader(once.Bytes()), &twice)
			if err != nil || stats.ChangedFields != 0 || !bytes.Equal(once.Bytes(), twice.Bytes()) {
				t.Fatalf("filter is not idempotent: %v", err)
			}
		}
	})
}
