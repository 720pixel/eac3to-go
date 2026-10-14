package flaccheck

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Complete examples from RFC 9639, Appendix D.
var encodedExamples = []struct {
	hex    string
	frames uint64
	blocks uint64
}{
	{"664c6143800000221000100000000f00000f0ac442f0000000013e84b41807dc690307586a3dad1a2e0ffff869180000bf0358fd03128baa9a", 1, 1},
	{"664c614300000022001000100000170000440ac442f000000013d5b0564975e98b8d8b930422757b8103030000120000000000000000000000000000000000100400003a200000007265666572656e6365206c6962464c414320312e332e33203230313930383034010000000e0000005449544c453dd7a9d79cd795d79d81000006000000000000fff86998000f9912086701623d1442998f5df70d6fe00c17caeb21000ee7a77a24a1590c1217b603097b784faa9a33d285e070ad5b1b4851b4010d99d2cd1a68f1e6b810fff869180102a402c382c40bc14a03ee48dd03b67c1330", 19, 2},
	{"664c6143800000221000100000001f00001f07d0007000000018f8f9e396f5cbcfc6dc807f9977906b32fff868020017e944004f6f313d1047d227cb6d090831452bdc2822228057a3", 24, 1},
}

func TestEncodedRFCExamples(t *testing.T) {
	for i, example := range encodedExamples {
		data := decodeHex(t, example.hex)
		for _, chunk := range []int{1, 3, 64, 8192} {
			result, err := Validate(&chunkReader{r: bytes.NewReader(data), chunk: chunk})
			if err != nil || result.Frames != example.frames || result.EncodedFrames != example.blocks {
				t.Fatalf("example %d, chunk %d: %+v, %v", i+1, chunk, result, err)
			}
		}
	}
}

type chunkReader struct {
	r     io.Reader
	chunk int
}

func (r *chunkReader) Read(p []byte) (int, error) { return r.r.Read(p[:min(len(p), r.chunk)]) }

type encodedTestBits struct {
	data []byte
	used int
}

func (b *encodedTestBits) put(value uint64, width int) {
	for i := width - 1; i >= 0; i-- {
		if b.used%8 == 0 {
			b.data = append(b.data, 0)
		}
		b.data[len(b.data)-1] |= byte(value>>i&1) << (7 - b.used%8)
		b.used++
	}
}

func (b *encodedTestBits) zero(width int) {
	for width > 0 {
		n := min(width, 64)
		b.put(0, n)
		width -= n
	}
}

func (b *encodedTestBits) unary(value int) { b.zero(value); b.put(1, 1) }

func testCRC(data []byte, width int, polynomial uint32) uint32 {
	var crc uint32
	for _, value := range data {
		for bit := 7; bit >= 0; bit-- {
			top := crc>>(width-1) ^ uint32(value>>bit)
			crc <<= 1
			if top&1 != 0 {
				crc ^= polynomial
			}
		}
	}
	return crc & (1<<width - 1)
}

func testCodedNumber(value uint64) []byte {
	if value < 128 {
		return []byte{byte(value)}
	}
	width := 2
	limits := [...]uint64{0, 0, 1 << 11, 1 << 16, 1 << 21, 1 << 26, 1 << 31, 1 << 36}
	for value >= limits[width] && width < 7 {
		width++
	}
	out := make([]byte, width)
	for i := width - 1; i > 0; i-- {
		out[i] = 128 | byte(value&63)
		value >>= 6
	}
	out[0] = byte(0xff<<(8-width)) | byte(value)
	return out
}

type encodedTestFrame struct {
	block, depth, channels int
	assignment             byte
	variable               bool
	number                 uint64
	blockCode, rateCode    byte
	depthCode              byte
	rate                   int
}

func testEncodedFrame(spec encodedTestFrame, subframe func(*encodedTestBits, int, int)) []byte {
	if spec.blockCode == 0 {
		spec.blockCode = 7
	}
	header := []byte{0xff, 0xf8, spec.blockCode<<4 | spec.rateCode, spec.assignment<<4 | spec.depthCode<<1}
	if spec.variable {
		header[1]++
	}
	header = append(header, testCodedNumber(spec.number)...)
	if spec.blockCode == 6 {
		header = append(header, byte(spec.block-1))
	} else if spec.blockCode == 7 {
		header = append(header, byte((spec.block-1)>>8), byte(spec.block-1))
	}
	switch spec.rateCode {
	case 12:
		header = append(header, byte(spec.rate/1000))
	case 13:
		header = append(header, byte(spec.rate>>8), byte(spec.rate))
	case 14:
		header = append(header, byte((spec.rate/10)>>8), byte(spec.rate/10))
	}
	header = append(header, byte(testCRC(header, 8, 7)))
	bits := encodedTestBits{data: header, used: len(header) * 8}
	for channel := 0; channel < spec.channels; channel++ {
		depth := spec.depth
		if spec.assignment == 8 && channel == 1 || spec.assignment == 9 && channel == 0 || spec.assignment == 10 && channel == 1 {
			depth++
		}
		if subframe == nil {
			bits.put(0, 8)
			bits.put(0, depth)
		} else {
			subframe(&bits, depth, spec.block)
		}
	}
	crc := testCRC(bits.data, 16, 0x8005)
	return append(bits.data, byte(crc>>8), byte(crc))
}

func testEncodedFile(info Info, frames ...[]byte) []byte {
	data := append([]byte{'f', 'L', 'a', 'C', 128, 0, 0, 34}, streamInfoBytes(info)...)
	for _, frame := range frames {
		data = append(data, frame...)
	}
	return data
}

func TestEncodedDepthChannelsAndStereo(t *testing.T) {
	for depth := 4; depth <= 32; depth++ {
		for assignment := byte(0); assignment <= 10; assignment++ {
			channels := int(assignment) + 1
			if assignment >= 8 {
				channels = 2
			}
			info := Info{MinBlockSize: 16, MaxBlockSize: 65535, SampleRate: 48000, BitsPerSample: depth, Channels: channels}
			spec := encodedTestFrame{block: 65535, depth: depth, channels: channels, assignment: assignment}
			data := testEncodedFile(info, testEncodedFrame(spec, nil))
			got, err := Validate(bytes.NewReader(data))
			if err != nil || got.Frames != 65535 {
				t.Fatalf("%d-bit assignment %d: %+v, %v", depth, assignment, got, err)
			}
		}
	}
}

func TestEncodedSubframeMethods(t *testing.T) {
	info := Info{MinBlockSize: 16, MaxBlockSize: 64, SampleRate: 48000, BitsPerSample: 24, Channels: 1}
	spec := encodedTestFrame{block: 64, depth: 24, channels: 1}
	for kind := 0; kind < 64; kind++ {
		if kind >= 2 && kind <= 7 || kind >= 13 && kind <= 31 {
			continue
		}
		for _, escaped := range []bool{false, true} {
			frame := testEncodedFrame(spec, func(b *encodedTestBits, depth, block int) {
				b.put(uint64(kind)<<1, 8)
				if kind == 0 {
					b.put(1, depth)
					return
				}
				if kind == 1 {
					for i := 0; i < block; i++ {
						b.put(0xfff8, depth)
					}
					return
				}
				order := kind - 8
				if kind >= 32 {
					order = kind - 31
				}
				b.zero(order * depth)
				if kind >= 32 {
					b.put(0, 4)
					b.put(0, 5)
					b.zero(order)
				}
				b.put(0, 2)
				b.put(0, 4)
				if escaped {
					b.put(15, 4)
					b.put(0, 5)
				} else {
					b.put(0, 4)
					for i := order; i < block; i++ {
						b.unary(i % 7)
					}
				}
			})
			if got, err := Validate(bytes.NewReader(testEncodedFile(info, frame))); err != nil || got.Frames != 64 {
				t.Fatalf("kind %d, escape %t: %+v, %v", kind, escaped, got, err)
			}
		}
	}
}

func TestEncodedWastedBits(t *testing.T) {
	for depth := 4; depth <= 32; depth++ {
		for wasted := 1; wasted < depth; wasted++ {
			info := Info{MinBlockSize: 16, MaxBlockSize: 16, SampleRate: 1, BitsPerSample: depth, Channels: 1}
			spec := encodedTestFrame{block: 16, depth: depth, channels: 1}
			frame := testEncodedFrame(spec, func(b *encodedTestBits, depth, block int) {
				b.put(3, 8)
				b.unary(wasted - 1)
				b.zero((depth - wasted) * block)
			})
			if _, err := Validate(bytes.NewReader(testEncodedFile(info, frame))); err != nil {
				t.Fatalf("depth %d, wasted %d: %v", depth, wasted, err)
			}
		}
	}
}

func TestEncodedHeaderCodes(t *testing.T) {
	blocks := [...]int{0, 192, 576, 1152, 2304, 4608, 128, 65535, 256, 512, 1024, 2048, 4096, 8192, 16384, 32768}
	rates := [...]int{1048575, 88200, 176400, 192000, 8000, 16000, 22050, 24000, 32000, 44100, 48000, 96000, 255000, 65535, 655350}
	depths := [...]int{4, 8, 12, 0, 16, 20, 24, 32}
	for code := byte(1); code <= 15; code++ {
		for rateCode, rate := range rates {
			for depthCode, depth := range depths {
				if depth == 0 {
					continue
				}
				info := Info{MinBlockSize: 16, MaxBlockSize: 65535, SampleRate: rate, BitsPerSample: depth, Channels: 1}
				spec := encodedTestFrame{block: blocks[code], depth: depth, channels: 1, blockCode: code, rateCode: byte(rateCode), depthCode: byte(depthCode), rate: rate}
				got, err := Validate(bytes.NewReader(testEncodedFile(info, testEncodedFrame(spec, nil))))
				if err != nil || got.Frames != uint64(blocks[code]) {
					t.Fatalf("block/rate/depth codes %d/%d/%d: %+v, %v", code, rateCode, depthCode, got, err)
				}
			}
		}
	}
}

func TestEncodedVariableBlocksAndSequence(t *testing.T) {
	for _, variable := range []bool{false, true} {
		info := Info{MinBlockSize: 16, MaxBlockSize: 32, SampleRate: 48000, BitsPerSample: 16, Channels: 1}
		var frames [][]byte
		var total uint64
		for i := 0; i < 132; i++ {
			block := 32
			if variable {
				block = 16 + i%17
			}
			if i == 131 {
				block = 1
			}
			number := uint64(i)
			if variable {
				number = total
			}
			spec := encodedTestFrame{block: block, depth: 16, channels: 1, variable: variable, number: number}
			frames = append(frames, testEncodedFrame(spec, nil))
			total += uint64(block)
		}
		info.TotalFrames = total
		got, err := Validate(bytes.NewReader(testEncodedFile(info, frames...)))
		if err != nil || got.Frames != total || got.EncodedFrames != 132 {
			t.Fatalf("variable=%t: %+v, %v", variable, got, err)
		}
		for _, broken := range [][][]byte{frames[1:], append(append([][]byte(nil), frames[:1]...), frames[2:]...), append(append([][]byte(nil), frames[:1]...), frames...)} {
			if _, err := Validate(bytes.NewReader(testEncodedFile(info, broken...))); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("accepted missing/duplicate frame: %v", err)
			}
		}
	}
}

func TestEncodedCRCsAndTruncation(t *testing.T) {
	data := decodeHex(t, encodedExamples[0].hex)
	for length := 0; length < len(data); length++ {
		if _, err := Validate(bytes.NewReader(data[:length])); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("accepted %d-byte truncated file: %v", length, err)
		}
	}
	for offset := 42; offset < len(data); offset++ {
		for bit := byte(1); bit != 0; bit <<= 1 {
			damaged := append([]byte(nil), data...)
			damaged[offset] ^= bit
			if _, err := Validate(bytes.NewReader(damaged)); err == nil {
				t.Fatalf("accepted single-bit corruption at byte %d mask %02x", offset, bit)
			}
		}
	}
	for _, trailer := range [][]byte{{0}, {255}, {'f', 'L', 'a', 'C'}} {
		if _, err := Validate(bytes.NewReader(append(append([]byte(nil), data...), trailer...))); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("accepted trailing data: %v", err)
		}
	}
	// Both unknown fields are legal; frame CRCs must still detect damage.
	for i := 21; i < 42; i++ {
		data[i] = 0
	}
	data[21] = 0xf0
	if result, err := Validate(bytes.NewReader(data)); err != nil || result.Frames != 1 {
		t.Fatalf("unknown total/checksum: %+v, %v", result, err)
	}
	data[len(data)-3] ^= 4
	if _, err := Validate(bytes.NewReader(data)); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("accepted damaged stream with unknown metadata: %v", err)
	}
}

func TestEncodedRiceParametersAndLimits(t *testing.T) {
	info := Info{MinBlockSize: 16, MaxBlockSize: 16, SampleRate: 48000, BitsPerSample: 32, Channels: 1}
	spec := encodedTestFrame{block: 16, depth: 32, channels: 1}
	for method := 0; method <= 1; method++ {
		for parameter := 0; parameter < 1<<(4+method); parameter++ {
			frame := testEncodedFrame(spec, func(b *encodedTestBits, depth, block int) {
				b.put(16, 8)
				b.put(uint64(method), 2)
				b.put(2, 4)
				for partition := 0; partition < 4; partition++ {
					b.put(uint64(parameter), 4+method)
					if parameter == (1<<(4+method))-1 {
						b.put(uint64(partition*10), 5)
						b.zero(4 * partition * 10)
					} else {
						for i := 0; i < 4; i++ {
							b.unary(0)
							b.put(uint64(i)%(1<<parameter), parameter)
						}
					}
				}
			})
			if _, err := Validate(bytes.NewReader(testEncodedFile(info, frame))); err != nil {
				t.Fatalf("method %d, parameter %d: %v", method, parameter, err)
			}
		}
	}
	for _, folded := range []uint64{0xfffffffe, 0xffffffff} {
		frame := testEncodedFrame(spec, func(b *encodedTestBits, depth, block int) {
			b.put(16, 8)
			b.put(1, 2)
			b.put(0, 4)
			b.put(30, 5)
			for i := 0; i < block; i++ {
				b.unary(int(folded >> 30))
				b.put(folded&((1<<30)-1), 30)
			}
		})
		_, err := Validate(bytes.NewReader(testEncodedFile(info, frame)))
		if folded == 0xfffffffe && err != nil || folded == 0xffffffff && !errors.Is(err, ErrCorrupt) {
			t.Fatalf("folded residual %x: %v", folded, err)
		}
	}
}

func TestEncodedInvalidSyntaxWithValidCRCs(t *testing.T) {
	info := Info{MinBlockSize: 16, MaxBlockSize: 16, SampleRate: 48000, BitsPerSample: 16, Channels: 1}
	spec := encodedTestFrame{block: 16, depth: 16, channels: 1}
	for _, tc := range []struct {
		name string
		body func(*encodedTestBits, int, int)
	}{
		{"reserved subframe", func(b *encodedTestBits, _, _ int) { b.put(4, 8) }},
		{"subframe reserved bit", func(b *encodedTestBits, _, _ int) { b.put(128, 8) }},
		{"too many wasted bits", func(b *encodedTestBits, depth, _ int) { b.put(1, 8); b.unary(depth - 1); b.put(0, 1) }},
		{"oversized predictor", func(b *encodedTestBits, _, _ int) { b.put(126, 8) }},
		{"reserved residual", func(b *encodedTestBits, _, _ int) { b.put(16, 8); b.put(2, 2); b.put(0, 4) }},
		{"too many partitions", func(b *encodedTestBits, _, _ int) { b.put(16, 8); b.put(0, 2); b.put(5, 4) }},
		{"no residual after warmup", func(b *encodedTestBits, depth, _ int) { b.put(18, 8); b.zero(depth); b.put(0, 2); b.put(4, 4) }},
		{"forbidden LPC precision", func(b *encodedTestBits, depth, _ int) { b.put(64, 8); b.zero(depth); b.put(15, 4); b.put(0, 5) }},
		{"negative LPC shift", func(b *encodedTestBits, depth, _ int) { b.put(64, 8); b.zero(depth); b.put(0, 4); b.put(31, 5) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := testEncodedFile(info, testEncodedFrame(spec, tc.body))
			if _, err := Validate(bytes.NewReader(data)); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("accepted malformed payload with valid CRC: %v", err)
			}
		})
	}
	for _, blocks := range [][]int{{8, 8}, {16, 8, 8}, {16, 32}} {
		var frames [][]byte
		for i, block := range blocks {
			spec.block, spec.number = block, uint64(i)
			frames = append(frames, testEncodedFrame(spec, nil))
		}
		if _, err := Validate(bytes.NewReader(testEncodedFile(info, frames...))); !errors.Is(err, ErrCorrupt) {
			t.Fatalf("accepted invalid block sequence %v: %v", blocks, err)
		}
	}
}

func TestEncodedCodedNumbers(t *testing.T) {
	for _, value := range []uint64{0, 127, 128, 2047, 2048, 65535, 65536, 0x1fffff, 0x200000, 0x3ffffff, 0x4000000, 0x7fffffff, 0x80000000, maxTotalFrames} {
		b := frameBits{reader: bufio.NewReader(bytes.NewReader(testCodedNumber(value)))}
		if got := b.codedNumber(true); got != value || b.err != nil {
			t.Fatalf("number %d: %d, %v", value, got, b.err)
		}
	}
	for _, data := range [][]byte{{128}, {255}, {192, 128}, {224, 128, 128}, {194, 64}, {254, 128, 128, 128, 128, 128, 128}} {
		b := frameBits{reader: bufio.NewReader(bytes.NewReader(data))}
		b.codedNumber(true)
		if !errors.Is(b.err, ErrCorrupt) {
			t.Fatalf("accepted malformed coded number %x: %v", data, b.err)
		}
	}
	b := frameBits{reader: bufio.NewReader(bytes.NewReader(testCodedNumber(1 << 31)))}
	b.codedNumber(false)
	if !errors.Is(b.err, ErrCorrupt) {
		t.Fatal("accepted fixed-block frame number above 31 bits")
	}
}

func TestEncodedFileAndCancellation(t *testing.T) {
	data := decodeHex(t, encodedExamples[0].hex)
	path := filepath.Join(t.TempDir(), "test.flac")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateFile(path); err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateFile(path + ".missing"); !os.IsNotExist(err) {
		t.Fatalf("missing file: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ValidateContext(ctx, bytes.NewReader(data)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read: %v", err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	r := &cancelEncodedReader{prefix: bytes.NewReader(data[:49]), cancel: cancel}
	if _, err := ValidateContext(ctx, r); !errors.Is(err, context.Canceled) {
		t.Fatalf("mid-frame cancellation: %v", err)
	}
	// A very long Rice quotient must remain cancellable without buffering it.
	info := Info{MinBlockSize: 16, MaxBlockSize: 16, SampleRate: 48000, BitsPerSample: 16, Channels: 1}
	frame := testEncodedFrame(encodedTestFrame{block: 16, depth: 16, channels: 1}, func(b *encodedTestBits, _, _ int) {
		b.put(16, 8)
		b.put(0, 2)
		b.put(0, 4)
		b.put(0, 4)
		b.zero(256 << 10)
	})
	ctx, cancel = context.WithCancel(context.Background())
	long := &cancelAfterEncodedReader{r: bytes.NewReader(testEncodedFile(info, frame)), cancel: cancel}
	if _, err := ValidateContext(ctx, long); !errors.Is(err, context.Canceled) {
		t.Fatalf("long unary cancellation: %v", err)
	}
	if long.reads > 3 {
		t.Fatalf("continued reading after cancellation: %d reads", long.reads)
	}
}

type cancelEncodedReader struct {
	prefix *bytes.Reader
	cancel context.CancelFunc
}

func (r *cancelEncodedReader) Read(p []byte) (int, error) {
	n, err := r.prefix.Read(p)
	if n > 0 {
		r.cancel()
	}
	return n, err
}

type cancelAfterEncodedReader struct {
	r      io.Reader
	cancel context.CancelFunc
	reads  int
}

func (r *cancelAfterEncodedReader) Read(p []byte) (int, error) {
	r.reads++
	n, err := r.r.Read(p[:min(len(p), 4096)])
	if r.reads == 3 {
		r.cancel()
	}
	return n, err
}

func TestEncodedFFmpegFiles(t *testing.T) {
	if _, err := os.Stat("/usr/bin/ffmpeg"); err != nil {
		t.Skip("system FFmpeg unavailable")
	}
	for _, tc := range []struct {
		bits, channels, level int
		source                string
	}{
		{16, 1, 0, "anullsrc=r=48000:cl=mono"},
		{16, 2, 5, "sine=frequency=997:sample_rate=48000"},
		{24, 6, 8, "sine=frequency=997:sample_rate=48000"},
		{32, 8, 12, "anoisesrc=sample_rate=48000:seed=7"},
	} {
		path := filepath.Join(t.TempDir(), "audio.flac")
		args := []string{"-v", "error", "-f", "lavfi", "-i", tc.source, "-t", "0.137", "-ac", fmt.Sprint(tc.channels), "-c:a", "flac", "-bits_per_raw_sample", fmt.Sprint(tc.bits), "-compression_level", fmt.Sprint(tc.level), path}
		if output, err := exec.Command("/usr/bin/ffmpeg", args...).CombinedOutput(); err != nil {
			t.Fatalf("encode: %v: %s", err, output)
		}
		got, err := ValidateFile(path)
		if err != nil || got.Frames != 6576 || got.Info.Channels != tc.channels {
			t.Fatalf("%d-bit/%d-channel/level%d: %+v, %v", tc.bits, tc.channels, tc.level, got, err)
		}
	}
}

func FuzzEncodedFrames(f *testing.F) {
	for _, example := range encodedExamples {
		f.Add(decodeHex(f, example.hex))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 65536 {
			t.Skip()
		}
		result, err := Validate(bytes.NewReader(data))
		if err == nil && (result.Frames == 0 || result.EncodedFrames == 0) {
			t.Fatalf("accepted empty stream: %+v", result)
		}
	})
}

func FuzzEncodedSubframes(f *testing.F) {
	f.Add([]byte{0, 0, 0})
	f.Add([]byte{16, 0, 255, 255})
	f.Add([]byte{64, 0, 0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, payload []byte) {
		if len(payload) > 8192 {
			t.Skip()
		}
		info := Info{MinBlockSize: 16, MaxBlockSize: 64, SampleRate: 48000, BitsPerSample: 16, Channels: 1}
		frame := testEncodedFrame(encodedTestFrame{block: 64, depth: 16, channels: 1}, nil)
		frame = append(frame[:8:8], payload...)
		crc := testCRC(frame, 16, 0x8005)
		frame = append(frame, byte(crc>>8), byte(crc))
		_, _ = Validate(bytes.NewReader(testEncodedFile(info, frame)))
	})
}

func TestEncodedMetadataAndFormatMismatch(t *testing.T) {
	for _, tc := range []struct {
		name   string
		modify func([]byte) []byte
	}{
		{"marker", func(p []byte) []byte { p[0] = 0; return p }},
		{"first metadata", func(p []byte) []byte { p[4] = 129; return p }},
		{"forbidden metadata", func(p []byte) []byte { p[4] = 255; return p }},
		{"wrong length", func(p []byte) []byte { p[7] = 33; return p }},
		{"missing last metadata", func(p []byte) []byte { p[4] = 0; return p }},
		{"duplicate STREAMINFO", func(p []byte) []byte { p[4] = 0; return append(append(p[:42:42], p[4:42]...), p[42:]...) }},
		{"too many samples", func(p []byte) []byte { p[25] = 2; return p }},
		{"minimum frame size", func(p []byte) []byte { p[14], p[17] = 16, 16; return p }},
		{"maximum frame size", func(p []byte) []byte { p[14], p[17] = 14, 14; return p }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := tc.modify(decodeHex(t, encodedExamples[0].hex))
			if _, err := Validate(bytes.NewReader(data)); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("accepted invalid metadata: %v", err)
			}
		})
	}
	data := decodeHex(t, encodedExamples[0].hex)
	data[18]++
	if _, err := Validate(bytes.NewReader(data)); !errors.Is(err, ErrFormatChange) || !strings.Contains(err.Error(), "STREAMINFO") {
		t.Fatalf("format mismatch not distinguished: %v", err)
	}
}

func BenchmarkValidateEncoded(b *testing.B) {
	for _, rice := range []bool{false, true} {
		b.Run(fmt.Sprintf("rice=%t", rice), func(b *testing.B) {
			info := Info{MinBlockSize: 4096, MaxBlockSize: 4096, SampleRate: 48000, BitsPerSample: 24, Channels: 2}
			var frames [][]byte
			for number := uint64(0); number < 16; number++ {
				frames = append(frames, testEncodedFrame(encodedTestFrame{block: 4096, depth: 24, channels: 2, assignment: 1, number: number}, func(w *encodedTestBits, depth, count int) {
					if !rice {
						w.put(2, 8)
						w.zero(depth * count)
						return
					}
					w.put(16, 8)
					w.put(0, 2)
					w.put(0, 4)
					w.put(4, 4)
					for i := 0; i < count; i++ {
						w.unary(0)
						w.put(uint64(i&15), 4)
					}
				}))
			}
			data := testEncodedFile(info, frames...)
			b.SetBytes(int64(len(data)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := Validate(bytes.NewReader(data)); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
