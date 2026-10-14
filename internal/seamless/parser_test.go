package seamless

import (
	"bytes"
	"errors"
	"io"
	"testing"
)

func setBits(b []byte, offset, width int, value uint32) {
	for i := 0; i < width; i++ {
		n := offset + i
		mask := byte(1 << uint(7-n%8))
		b[n/8] &= ^mask
		if value&(1<<uint(width-1-i)) != 0 {
			b[n/8] |= mask
		}
	}
}

func ac3Fixture(tag byte) []byte {
	b := make([]byte, 1280)
	copy(b, []byte{0x0b, 0x77, 0, 0, 26, 8 << 3, 2 << 5})
	b[len(b)-3] = tag
	return b
}

func coreFixture(size int, tag byte) []byte {
	b := make([]byte, size)
	copy(b, []byte{0x7f, 0xfe, 0x80, 0x01})
	setBits(b, 32, 1, 1)
	setBits(b, 33, 5, 31)
	setBits(b, 39, 7, 15)
	setBits(b, 46, 14, uint32(size-1))
	setBits(b, 60, 6, 9)
	setBits(b, 66, 4, 13)
	b[len(b)-1] = tag
	return b
}

func extensionFixture(size int, wide bool, tag byte) []byte {
	b := make([]byte, size)
	copy(b, []byte{0x64, 0x58, 0x20, 0x25})
	width := 8
	if wide {
		setBits(b, 42, 1, 1)
		width = 12
	}
	setBits(b, 43, width, 9)
	setBits(b, 43+width, width+8, uint32(size-1))
	b[len(b)-1] = tag
	return b
}

func collect(t *testing.T, codec Codec, source []byte, chunk int) []Frame {
	t.Helper()
	var frames []Frame
	p, err := NewParser(Config{Codec: codec, OnFrame: func(f Frame) error { f.Bytes = bytes.Clone(f.Bytes); frames = append(frames, f); return nil }})
	if err != nil {
		t.Fatal(err)
	}
	for len(source) > 0 {
		n := min(chunk, len(source))
		written, err := p.Write(source[:n])
		if err != nil || written != n {
			t.Fatalf("write %d: %d %v", n, written, err)
		}
		source = source[n:]
	}
	if err := p.Flush(); err != nil {
		t.Fatal(err)
	}
	return frames
}

func TestSplitHeadersAndExactBytes(t *testing.T) {
	for _, codec := range []Codec{AC3, DTS} {
		var a, b []byte
		if codec == AC3 {
			a, b = ac3Fixture(1), ac3Fixture(2)
		} else {
			a, b = coreFixture(96, 1), coreFixture(100, 2)
		}
		source := append(bytes.Clone(a), b...)
		for _, chunk := range []int{1, 2, 3, 7, 13, 127, 4096} {
			frames := collect(t, codec, source, chunk)
			if len(frames) != 2 || !bytes.Equal(frames[0].Bytes, a) || !bytes.Equal(frames[1].Bytes, b) {
				t.Fatalf("codec %d chunk %d changed frames", codec, chunk)
			}
			for _, f := range frames {
				if f.SampleRate != 48000 {
					t.Fatal(f)
				}
				if codec == AC3 && f.Samples != 1536 {
					t.Fatal(f)
				}
				if codec == DTS && f.Samples != 512 {
					t.Fatal(f)
				}
			}
		}
	}
}

func TestCoreAndExSSComposite(t *testing.T) {
	for _, wide := range []bool{false, true} {
		core := coreFixture(97, 9)
		extension := extensionFixture(200, wide, 3)
		composite := append(bytes.Clone(core), 0, 0, 0)
		composite = append(composite, extension...)
		last := coreFixture(100, 1)
		source := append(bytes.Clone(composite), last...)
		for _, chunk := range []int{1, 10, 97, 100, 107, 4096} {
			frames := collect(t, DTS, source, chunk)
			if len(frames) != 2 || !bytes.Equal(frames[0].Bytes, composite) || frames[0].CoreBytes != 97 || frames[0].PaddingBytes != 3 || frames[0].ExtensionBytes != 200 {
				t.Fatalf("wide %v chunk %d: %+v", wide, chunk, frames)
			}
		}
	}
}

func TestDTSLittleEndianPreservesOrder(t *testing.T) {
	b := coreFixture(100, 7)
	for i := 0; i < len(b); i += 2 {
		b[i], b[i+1] = b[i+1], b[i]
	}
	f := collect(t, DTS, b, 1)
	if len(f) != 1 || !bytes.Equal(f[0].Bytes, b) || f[0].Samples != 512 {
		t.Fatal(f)
	}
}

func TestEnhancedAndSwappedAudioMetadata(t *testing.T) {
	for code, samples := range []int{256, 512, 768, 1536} {
		frame := make([]byte, 128)
		copy(frame, []byte{0x0b, 0x77, 0, 63, byte(code<<4 | 2<<1), 16 << 3})
		for _, swapped := range []bool{false, true} {
			input := bytes.Clone(frame)
			if swapped {
				for i := 0; i < len(input); i += 2 {
					input[i], input[i+1] = input[i+1], input[i]
				}
			}
			frames := collect(t, AC3, input, 1)
			if len(frames) != 1 || !frames[0].Enhanced || frames[0].Samples != samples || frames[0].SampleRate != 48000 || !bytes.Equal(frames[0].Bytes, input) {
				t.Fatalf("code %d swapped %v: %+v", code, swapped, frames)
			}
		}
	}
}

func TestUnsupportedDTSDoesNotDiscardPayload(t *testing.T) {
	for _, header := range [][]byte{{0x1f, 0xff, 0xe8, 0}, {0xff, 0x1f, 0, 0xe8}, {0x64, 0x58, 0x20, 0x25}} {
		input := make([]byte, 100)
		copy(input, header)
		emitted := 0
		p, _ := NewParser(Config{Codec: DTS, OnFrame: func(Frame) error { emitted++; return nil }})
		_, err := p.Write(input)
		if !errors.Is(err, ErrUnsupported) || emitted != 0 {
			t.Fatalf("unsupported header %x: emitted %d, %v", header, emitted, err)
		}
	}
}

func TestPendingBoundaryBytes(t *testing.T) {
	p, _ := NewParser(Config{Codec: AC3, OnFrame: func(Frame) error { return nil }})
	frame := ac3Fixture(1)
	_, _ = p.Write(frame[:4])
	if p.PendingBytes() != 4 {
		t.Fatal(p.PendingBytes())
	}
	_, _ = p.Write(frame[4:])
	if p.PendingBytes() != 0 {
		t.Fatal(p.PendingBytes())
	}
	p, _ = NewParser(Config{Codec: DTS, OnFrame: func(Frame) error { return nil }})
	_, _ = p.Write(coreFixture(96, 1))
	if p.PendingBytes() != 96 {
		t.Fatal(p.PendingBytes())
	}
}

func TestRateAndLayoutChangesPreserveFrames(t *testing.T) {
	stereo := ac3Fixture(1)
	surround := make([]byte, 1920)
	copy(surround, stereo)
	surround[4] = 0x80 | 26
	surround[6] = 7 << 5
	frames := collect(t, AC3, append(bytes.Clone(stereo), surround...), 17)
	if len(frames) != 2 || frames[0].SampleRate != 48000 || frames[1].SampleRate != 32000 || !bytes.Equal(frames[1].Bytes, surround) {
		t.Fatal("AC3 parameter change was lost")
	}
	first := coreFixture(96, 1)
	second := coreFixture(100, 2)
	setBits(second, 60, 6, 2)
	setBits(second, 66, 4, 12)
	setBits(second, 39, 7, 31)
	frames = collect(t, DTS, append(bytes.Clone(first), second...), 17)
	if len(frames) != 2 || frames[0].SampleRate != 48000 || frames[1].SampleRate != 24000 || frames[1].Samples != 1024 || !bytes.Equal(frames[1].Bytes, second) {
		t.Fatal("DTS parameter change was lost")
	}
}

func TestEnhancedDependentFramesRemainSeparate(t *testing.T) {
	independent := make([]byte, 128)
	copy(independent, []byte{0x0b, 0x77, 0, 63, 3<<4 | 2<<1, 16 << 3})
	dependent := bytes.Clone(independent)
	dependent[2] = 1<<6 | 1<<3
	group := append(bytes.Clone(independent), dependent...)
	frames := collect(t, AC3, append(bytes.Clone(group), group...), 5)
	if len(frames) != 4 {
		t.Fatalf("coalesced independent/dependent frames: %d", len(frames))
	}
	filter, _ := NewOverlap(128)
	for i, frame := range frames {
		if i == 2 {
			_ = filter.Boundary(WindowsBoundary(0))
		}
		keep, err := filter.Accept(frame)
		if !keep || err != nil {
			t.Fatalf("individual-frame policy deleted an EAC3 group member: %d %v", i, err)
		}
	}
	// Group overlap and presentation-duration accounting require an access-unit
	// assembler; individual sync-frame equality is insufficient evidence.
}

func TestMalformedAndTruncatedFrames(t *testing.T) {
	badAC3 := ac3Fixture(0)
	badAC3[4] = 0xc0
	badCore := coreFixture(96, 0)
	setBits(badCore, 66, 4, 0)
	badExtension := extensionFixture(30, false, 0)
	setBits(badExtension, 43, 8, 40)
	withBadExtension := append(coreFixture(96, 0), badExtension...)
	cases := []struct {
		name      string
		codec     Codec
		source    []byte
		truncated bool
	}{
		{"AC3 header", AC3, []byte{0x0b}, true},
		{"AC3 body", AC3, ac3Fixture(0)[:1000], true},
		{"AC3 rate", AC3, badAC3, false},
		{"DTS header", DTS, coreFixture(96, 0)[:10], true},
		{"DTS body", DTS, coreFixture(96, 0)[:90], true},
		{"DTS rate", DTS, badCore, false},
		{"ExSS body", DTS, append(coreFixture(96, 0), extensionFixture(200, false, 0)[:100]...), true},
		{"ExSS header size", DTS, withBadExtension, false},
		{"garbage", DTS, append(coreFixture(96, 0), 1, 2, 3, 4), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p, _ := NewParser(Config{Codec: tc.codec, OnFrame: func(Frame) error { return nil }})
			_, err := p.Write(tc.source)
			if err == nil {
				err = p.Flush()
			}
			if err == nil {
				t.Fatal("accepted damaged input")
			}
			if tc.truncated && !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("wanted truncation, got %v", err)
			}
		})
	}
}

func TestFrameBoundAndInputMemory(t *testing.T) {
	count := 0
	p, err := NewParser(Config{Codec: AC3, MaxFrameBytes: 1280, OnFrame: func(Frame) error { count++; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	large := bytes.Repeat(ac3Fixture(0), 10000)
	if _, err = p.Write(large); err != nil {
		t.Fatal(err)
	}
	if count != 10000 || len(p.buffer) != 0 || cap(p.buffer) > 2560 {
		t.Fatalf("unbounded pending memory: count %d len %d cap %d", count, len(p.buffer), cap(p.buffer))
	}
	p, _ = NewParser(Config{Codec: DTS, MaxFrameBytes: 200, OnFrame: func(Frame) error { return nil }})
	oversized := append(coreFixture(96, 0), extensionFixture(200, true, 0)...)
	if _, err = p.Write(oversized); err == nil {
		t.Fatal("accepted oversized composite")
	}
	if len(p.buffer) > 210 {
		t.Fatalf("read oversized body: %d", len(p.buffer))
	}
}

func TestTerminalErrorsAndBorrowedFrame(t *testing.T) {
	want := errors.New("output failed")
	p, _ := NewParser(Config{Codec: AC3, OnFrame: func(Frame) error { return want }})
	if _, err := p.Write(ac3Fixture(0)); !errors.Is(err, want) {
		t.Fatal(err)
	}
	if _, err := p.Write(nil); !errors.Is(err, want) {
		t.Fatal(err)
	}
	p, _ = NewParser(Config{Codec: AC3, OnFrame: func(Frame) error { return nil }})
	if err := p.Flush(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Write(nil); !errors.Is(err, ErrFinished) {
		t.Fatal(err)
	}
}

func FuzzFramesBounded(f *testing.F) {
	f.Add(uint8(AC3), ac3Fixture(1))
	f.Add(uint8(DTS), coreFixture(96, 1))
	f.Add(uint8(DTS), append(coreFixture(96, 1), extensionFixture(200, false, 2)...))
	f.Fuzz(func(t *testing.T, codec uint8, source []byte) {
		p, _ := NewParser(Config{Codec: Codec(codec%2 + 1), MaxFrameBytes: 4096, OnFrame: func(Frame) error { return nil }})
		for len(source) > 0 {
			n := min(17, len(source))
			_, err := p.Write(source[:n])
			if err != nil {
				break
			}
			source = source[n:]
			if len(p.buffer) > 4106 {
				t.Fatal("unbounded buffer")
			}
		}
		_ = p.Flush()
	})
}
