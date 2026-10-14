package truehd

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"testing"

	"eac3to/internal/transport"
)

// Synthetic fixture provenance: create a 4096-frame, 48000 Hz stereo signed24
// WAV with sample(frame, channel) = ((frame*109 + channel*12345 + 567) %
// 8388608) - 4194304, packed little endian. Encode with FFmpeg 6.1.1:
// ffmpeg -i synthetic.wav -c:a truehd -strict -2 -f truehd synthetic.thd
// The literal is the first 232 bytes (three access units) of that output.
// Full output SHA256: 5cb30d014ec66cd898656e59fd9eab5d6d87a88aedcda0c501a132dbd932d4bd.
// No disc audio or executable bytes are used. The first codec clock is -40
// modulo 65536.
const encodedUnits = "e054ffd8f8726fba0050a001b752000000008c7f1014000000000000000034282043f1ea0000011000000017bdc0000000020c8204c0012f30004b80423600e4de010a8c0396e00431000e692010df4039db0043ea00e84601115c03a480044c400e9fa0114c403ab5244038c785b1f25b56be5b9c369bb72848ffdb6200001718f0b63e4b6ad7cb7386d376e5091ffb6c400002ffffffffffffffffffffffffffffffff8000990e70110000600e914080500bfffffffffffffffffffffffffffffffffffffffe00a251000f0028600c7fffffffffffffffffffffffffffffffffffffffc000e994"

func units(t testing.TB) []byte {
	t.Helper()
	b, err := hex.DecodeString(encodedUnits)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestAccessUnitsSplitAtEveryByte(t *testing.T) {
	input := units(t)
	for split := 1; split <= len(input); split++ {
		var output []byte
		var previous *Frame
		count := 0
		p, err := NewParser(ParserConfig{OnFrame: func(f Frame) error {
			if f.Format.SampleRate != 48000 || f.Format.Samples != 40 || f.Format.Substreams != 1 || f.Format.ChannelMap1 != 1 || f.Format.ChannelMap2 != 1 {
				t.Fatalf("unexpected format: %+v", f.Format)
			}
			if f.MajorSync != (count == 0) || f.FormatChanged {
				t.Fatal("incorrect major sync classification")
			}
			if previous != nil && ClockDifference(*previous, f) != 0 {
				t.Fatal("codec clock discontinuity at wrap")
			}
			copyFrame := f
			previous = &copyFrame
			output = append(output, f.Bytes...)
			count++
			return nil
		}})
		if err != nil {
			t.Fatal(err)
		}
		for offset := 0; offset < len(input); offset += split {
			end := offset + split
			if end > len(input) {
				end = len(input)
			}
			if _, err = p.Write(input[offset:end]); err != nil {
				t.Fatalf("split %d: %v", split, err)
			}
		}
		if err = p.Finish(); err != nil {
			t.Fatal(err)
		}
		if count != 3 || !bytes.Equal(output, input) || p.PendingBytes() != 0 || p.Stats().Samples != 120 || p.Stats().MajorSyncs != 1 {
			t.Fatalf("incomplete or changed stream: %+v", p.Stats())
		}
		if cap(p.buffer) != MaxAccessUnitBytes {
			t.Fatal("unbounded parser buffer")
		}
	}
}

func TestFrameKeepsStartingPESMetadata(t *testing.T) {
	input := units(t)
	var seen []Frame
	p, _ := NewParser(ParserConfig{OnFrame: func(f Frame) error { seen = append(seen, f); return nil }})
	first := transport.PES{StreamID: 0xfd, StreamIDExtension: 0x72, PTS: 900000, DTS: 899000, ArrivalTimestamp: 123, Segment: 2, ByteOffset: 192, Discontinuity: true, Payload: input[:17]}
	second := transport.PES{StreamID: 0xfd, StreamIDExtension: 0x72, PTS: 900100, DTS: 899100, Segment: 3, ByteOffset: 384, Payload: input[17:]}
	if err := p.WritePES(first); err != nil {
		t.Fatal(err)
	}
	if err := p.WritePES(second); err != nil {
		t.Fatal(err)
	}
	if err := p.Finish(); err != nil {
		t.Fatal(err)
	}
	if len(seen) != 3 || seen[0].Source.PTS != 900000 || seen[0].Source.DTS != 899000 || seen[0].Source.Segment != 2 || seen[0].Source.ArrivalTimestamp != 123 || !seen[0].Source.Discontinuity {
		t.Fatal("starting PES metadata changed")
	}
	if seen[1].Source.PTS != 900100 || seen[1].Source.Segment != 3 || seen[1].Source.PayloadOffset != 151 || seen[2].Source.PayloadOffset != 185 {
		t.Fatalf("new PES metadata/offset changed: %+v", seen)
	}
}

func updateMajorChecksum(h []byte) {
	var v uint16
	for _, b := range h[:len(h)-4] {
		v ^= uint16(b) << 8
		for i := 0; i < 8; i++ {
			if v&0x8000 != 0 {
				v = v<<1 ^ 0x2d
			} else {
				v <<= 1
			}
		}
	}
	value := v>>8 | v<<8
	value ^= binary.LittleEndian.Uint16(h[len(h)-4 : len(h)-2])
	binary.LittleEndian.PutUint16(h[len(h)-2:], value)
}

func TestMajorExtensionsAndSampleRates(t *testing.T) {
	base := units(t)[:168]
	for _, row := range []struct {
		code          byte
		rate, samples int
	}{{0, 48000, 40}, {1, 96000, 80}, {2, 192000, 160}, {8, 44100, 40}, {9, 88200, 80}, {10, 176400, 160}} {
		b := append([]byte(nil), base...)
		b[8] = row.code << 4
		updateMajorChecksum(b[4:32])
		p, _ := NewParser(ParserConfig{OnFrame: func(f Frame) error {
			if f.Format.SampleRate != row.rate || f.Format.Samples != row.samples {
				t.Fatal(f.Format)
			}
			return nil
		}})
		if _, err := p.Write(b); err != nil {
			t.Fatal(err)
		}
		if err := p.Finish(); err != nil {
			t.Fatal(err)
		}
	}
	h := make([]byte, 34)
	copy(h, base[4:30])
	h[25] |= 1
	h[26] = 0x20
	updateMajorChecksum(h)
	b := append(append(append([]byte(nil), base[:4]...), h...), base[32:]...)
	b[0] = (b[0] & 0xf0) | byte((len(b)/2)>>8)
	b[1] = byte(len(b) / 2)
	p, _ := NewParser(ParserConfig{OnFrame: func(Frame) error { return nil }})
	if _, err := p.Write(b); err != nil {
		t.Fatalf("extended major: %v", err)
	}
	if err := p.Finish(); err != nil {
		t.Fatal(err)
	}
}

func TestInitialFormatAndFormatChanges(t *testing.T) {
	input := units(t)
	firstFormat := Format{SampleRate: 48000, Samples: 40, Substreams: 1, ChannelMap1: 1, ChannelMap2: 1, SubstreamInfo: 0x14}
	p, _ := NewParser(ParserConfig{OnFrame: func(Frame) error { return nil }})
	if _, err := p.Write(input[168:]); err == nil {
		t.Fatal("guessed a missing major format")
	}
	p, _ = NewParser(ParserConfig{InitialFormat: &firstFormat, OnFrame: func(Frame) error { return nil }})
	if _, err := p.Write(input[168:]); err != nil {
		t.Fatal(err)
	}
	if err := p.Finish(); err != nil {
		t.Fatal(err)
	}
	next := append([]byte(nil), input[:168]...)
	next[8] = 0x10
	updateMajorChecksum(next[4:32])
	for _, reject := range []bool{false, true} {
		changed := false
		p, _ = NewParser(ParserConfig{RejectFormatChanges: reject, OnFrame: func(f Frame) error { changed = changed || f.FormatChanged; return nil }})
		if _, err := p.Write(input[:168]); err != nil {
			t.Fatal(err)
		}
		_, err := p.Write(next)
		if reject {
			if err == nil {
				t.Fatal("accepted forbidden format change")
			}
		} else if err != nil || !changed || p.Stats().FormatChanges != 1 {
			t.Fatalf("change reporting: %v %+v", err, p.Stats())
		}
	}
	bad := firstFormat
	bad.SampleRate = 88200
	bad.Samples = 40
	if _, err := NewParser(ParserConfig{InitialFormat: &bad, OnFrame: func(Frame) error { return nil }}); err == nil {
		t.Fatal("accepted wrong sample count")
	}
}

func TestMalformedAccessUnitsAndTerminalErrors(t *testing.T) {
	input := units(t)
	for _, cut := range []int{1, 2, 3, 7, 27, 31, 167} {
		p, _ := NewParser(ParserConfig{OnFrame: func(Frame) error { return nil }})
		if _, err := p.Write(input[:cut]); err != nil {
			t.Fatal(err)
		}
		if err := p.Finish(); err == nil {
			t.Fatalf("accepted truncated unit at %d", cut)
		}
	}
	for _, mutate := range []func([]byte){
		func(b []byte) { b[0] = 0; b[1] = 0 },
		func(b []byte) { b[10] ^= 1 },
		func(b []byte) { b[8] = 0xf0; updateMajorChecksum(b[4:32]) },
		func(b []byte) { b[20] &= 15; updateMajorChecksum(b[4:32]) },
		func(b []byte) { b[168] ^= 0x10 },
		func(b []byte) { b[0] = 0; b[1] = 16 },
	} {
		b := append([]byte(nil), input...)
		mutate(b)
		p, _ := NewParser(ParserConfig{OnFrame: func(Frame) error { return nil }})
		_, err := p.Write(b)
		if err == nil {
			t.Fatal("accepted malformed unit")
		}
		if _, nextErr := p.Write(input); nextErr != err {
			t.Fatal("retried failed parser")
		}
	}
	want := errors.New("writer stopped")
	p, _ := NewParser(ParserConfig{OnFrame: func(Frame) error { return want }})
	if _, err := p.Write(input); err != want || p.Stats().Frames != 0 {
		t.Fatal("callback error ignored or counted")
	}
	if p.Finish() != want {
		t.Fatal("failure lost during finish")
	}
	p, _ = NewParser(ParserConfig{OnFrame: func(Frame) error { return nil }, MaxFrameBytes: 100})
	if _, err := p.Write(input); err == nil {
		t.Fatal("frame limit ignored")
	}
	p, _ = NewParser(ParserConfig{OnFrame: func(Frame) error { return nil }})
	if err := p.WritePES(transport.PES{StreamID: 0xfd, StreamIDExtension: 0x76, Payload: input}); err == nil {
		t.Fatal("AC3 companion reached TrueHD parser")
	}
}

func TestCodecClockDoesNotAuthorizeEdits(t *testing.T) {
	previous := Frame{InputTiming: 65500, Format: Format{Samples: 40}}
	next := Frame{InputTiming: 4}
	if ClockDifference(previous, next) != 0 {
		t.Fatal("clock wrap")
	}
	next.InputTiming = 44
	if ClockDifference(previous, next) != 40 {
		t.Fatal("positive clock difference")
	}
	next.InputTiming = previous.InputTiming
	if ClockDifference(previous, next) != -40 {
		t.Fatal("duplicate clock difference")
	}
}

func FuzzAccessUnitParser(f *testing.F) {
	f.Add(units(f))
	f.Add([]byte{0, 0})
	f.Add([]byte{0xff, 0xff})
	f.Fuzz(func(t *testing.T, data []byte) {
		p, err := NewParser(ParserConfig{OnFrame: func(frame Frame) error {
			if len(frame.Bytes) < 6 || len(frame.Bytes) > MaxAccessUnitBytes {
				t.Fatal("invalid emitted size")
			}
			return nil
		}})
		if err != nil {
			t.Fatal(err)
		}
		for start := 0; start < len(data); start += 17 {
			end := start + 17
			if end > len(data) {
				end = len(data)
			}
			if _, err = p.Write(data[start:end]); err != nil {
				break
			}
		}
		_ = p.Finish()
		if p.PendingBytes() > MaxAccessUnitBytes || cap(p.buffer) > MaxAccessUnitBytes {
			t.Fatal("unbounded frame buffering")
		}
	})
}
