package bitstream

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
)

func TestDTSDialnormCoreVersionsCRCAndByteOrder(t *testing.T) {
	for _, version := range []int{0, 5, 6, 7, 8, 15} {
		for code := 0; code < 16; code++ {
			for _, crc := range []bool{false, true} {
				for _, swapped := range []bool{false, true} {
					input := dtsCoreFixture(version, code, crc)
					expected := bytes.Clone(input)
					shift := 0
					if crc {
						shift = 16
					}
					changed := code != 0 || version == 6
					versionChanged := changed && version != 7
					if changed {
						writer := testBits{p: expected, pos: 89 + shift}
						writer.put(4, 7)
						writer.pos = 100 + shift
						writer.put(4, 0)
					}
					if swapped {
						swapWords(input)
						swapWords(expected)
					}
					var out bytes.Buffer
					stats, err := RemoveDTSDialnorm(iotest.OneByteReader(bytes.NewReader(input)), &out)
					if err != nil || !bytes.Equal(out.Bytes(), expected) {
						t.Fatalf("version%d code%d crc%v swapped%v: %v", version, code, crc, swapped, err)
					}
					if stats.Frames != 1 || stats.CoreFrames != 1 || stats.ExtensionFrames != 0 || stats.DialnormFields != 1 || stats.ChangedFields != boolCount(changed) || stats.ChangedFrames != boolCount(changed) || stats.EncoderRevisionChanges != boolCount(versionChanged) || stats.ByteSwappedFrames != boolCount(swapped) || stats.HeaderCRCUpdates != 0 {
						t.Fatalf("wrong counters: %+v", stats)
					}
					var second bytes.Buffer
					again, err := RemoveDTSDialnorm(bytes.NewReader(out.Bytes()), &second)
					if err != nil || again.ChangedFrames != 0 || !bytes.Equal(second.Bytes(), out.Bytes()) {
						t.Fatal("DTS core edit is not idempotent")
					}
				}
			}
		}
	}
}

func TestDTSDialnormExtensionAssetsAndHeaders(t *testing.T) {
	for _, tc := range []struct {
		wide, static, rich bool
		assets             int
	}{
		{false, false, false, 1}, {true, false, false, 1},
		{false, true, false, 2}, {false, true, true, 2}, {true, true, true, 8},
	} {
		fixture := dtsExtensionFixture(tc.wide, tc.static, tc.rich, tc.assets)
		expected := bytes.Clone(fixture.frame)
		changed := int64(0)
		for _, position := range fixture.dialnorm {
			if getBitsAt(expected, position, 5) != 0 {
				changed++
			}
			writer := testBits{p: expected, pos: position}
			writer.put(5, 0)
		}
		binary.BigEndian.PutUint16(expected[fixture.header-2:fixture.header], referenceDTSCRC(expected[5:fixture.header-2]))
		for _, swapped := range []bool{false, true} {
			input, want := bytes.Clone(fixture.frame), bytes.Clone(expected)
			if swapped {
				swapWords(input)
				swapWords(want)
			}
			var out bytes.Buffer
			stats, err := RemoveDTSDialnorm(bytes.NewReader(input), &out)
			if err != nil || !bytes.Equal(out.Bytes(), want) {
				t.Fatalf("%+v swapped%v: %v", tc, swapped, err)
			}
			if stats.Frames != 1 || stats.ExtensionFrames != 1 || stats.CoreFrames != 0 || stats.ExtensionAssets != int64(tc.assets) || stats.DialnormFields != int64(len(fixture.dialnorm)) || stats.ChangedFields != changed || stats.ChangedFrames != boolCount(changed > 0) || stats.HeaderCRCUpdates != boolCount(changed > 0) || stats.ByteSwappedFrames != boolCount(swapped) {
				t.Fatalf("wrong counters: %+v", stats)
			}
			var second bytes.Buffer
			again, err := RemoveDTSDialnorm(bytes.NewReader(out.Bytes()), &second)
			if err != nil || again.ChangedFrames != 0 || !bytes.Equal(second.Bytes(), out.Bytes()) {
				t.Fatal("DTS extension edit is not idempotent")
			}
		}
	}
}

func TestDTSDialnormMixedFramesAndMissingFields(t *testing.T) {
	fixture := dtsExtensionFixture(false, true, false, 1)
	// Replace the presence bit and preserve the now-opaque descriptor suffix.
	writer := testBits{p: fixture.frame, pos: fixture.dialnorm[0] - 1}
	writer.put(1, 0)
	binary.BigEndian.PutUint16(fixture.frame[fixture.header-2:fixture.header], referenceDTSCRC(fixture.frame[5:fixture.header-2]))
	first, last := dtsCoreFixture(7, 3, false), dtsCoreFixture(5, 0, true)
	input := append(append(bytes.Clone(first), fixture.frame...), last...)
	var out bytes.Buffer
	stats, err := RemoveDTSDialnorm(bytes.NewReader(input), &out)
	if err != nil || stats.Frames != 3 || stats.CoreFrames != 2 || stats.ExtensionFrames != 1 || stats.ChangedFrames != 1 || stats.DialnormFields != 2 || stats.BytesRead != int64(len(input)) || stats.BytesWritten != int64(len(input)) {
		t.Fatalf("mixed stream: %+v, %v", stats, err)
	}
	if !bytes.Equal(input[len(first):], out.Bytes()[len(first):]) {
		t.Fatal("absent dialog metadata or later frame was changed")
	}
}

func TestDTSDialnormChannelMappingFlags(t *testing.T) {
	for _, channels := range []int{1, 2, 3, 6, 7, 256} {
		for _, maskWidth := range []int{0, 4, 8, 12, 16} {
			for _, drc := range []bool{false, true} {
				fixture := dtsMappingFixture(channels, maskWidth, drc)
				expected := bytes.Clone(fixture.frame)
				writer := testBits{p: expected, pos: fixture.dialnorm[0]}
				writer.put(5, 0)
				binary.BigEndian.PutUint16(expected[fixture.header-2:fixture.header], referenceDTSCRC(expected[5:fixture.header-2]))
				stats, err := RemoveDTSDialnormFrame(fixture.frame)
				if err != nil || stats.ChangedFields != 1 || !bytes.Equal(fixture.frame, expected) {
					t.Fatalf("channels%d mask%d drc%v: %+v, %v", channels, maskWidth, drc, stats, err)
				}
			}
		}
	}
}

func TestDTSDialnormAccessUnitAtomicityAndAllocations(t *testing.T) {
	unit := dtsCoreFixture(6, 3, true)
	for index := 0; index < 4; index++ {
		fixture := dtsExtensionFixture(true, true, true, 2)
		writer := testBits{p: fixture.frame, pos: 40}
		writer.put(2, index)
		// Static presentation masks use index+1 bits. Keep the original index
		// in this fixture and use non-static headers for the smaller indices.
		if index != 3 {
			fixture = dtsExtensionFixture(true, false, false, 1)
			writer = testBits{p: fixture.frame, pos: 40}
			writer.put(2, index)
		}
		binary.BigEndian.PutUint16(fixture.frame[fixture.header-2:fixture.header], referenceDTSCRC(fixture.frame[5:fixture.header-2]))
		unit = append(unit, fixture.frame...)
	}
	var expected bytes.Buffer
	wantStats, err := RemoveDTSDialnorm(bytes.NewReader(unit), &expected)
	if err != nil {
		t.Fatal(err)
	}
	for _, swapped := range []bool{false, true} {
		input, want := bytes.Clone(unit), bytes.Clone(expected.Bytes())
		if swapped {
			swapWords(input)
			swapWords(want)
		}
		stats, err := RemoveDTSDialnormFrame(input)
		if err != nil || !bytes.Equal(input, want) || stats.Frames != 5 || stats.ChangedFields != wantStats.ChangedFields || stats.ByteSwappedFrames != 5*boolCount(swapped) {
			t.Fatalf("access unit swapped%v: %+v, %v", swapped, stats, err)
		}
	}
	input := bytes.Clone(unit)
	if allocations := testing.AllocsPerRun(100, func() {
		copy(input, unit)
		if _, err := RemoveDTSDialnormFrame(input); err != nil {
			panic(err)
		}
	}); allocations != 0 {
		t.Fatalf("DTS access-unit processing allocated %v times", allocations)
	}
	broken := bytes.Clone(unit)
	broken[len(dtsCoreFixture(7, 0, false))+300-1] ^= 1
	for _, invalid := range [][]byte{nil, unit[:len(unit)-1], append(bytes.Clone(unit), 0), broken, append(bytes.Clone(unit), dtsCoreFixture(7, 0, false)...), append(dtsCoreFixture(7, 1, false), dtsCoreFixture(7, 2, false)...)} {
		before := bytes.Clone(invalid)
		if _, err := RemoveDTSDialnormFrame(invalid); err == nil {
			t.Fatal("invalid access unit accepted")
		}
		if !bytes.Equal(invalid, before) {
			t.Fatal("failed access-unit operation changed earlier valid frame")
		}
	}
	fixture := dtsExtensionFixture(false, false, false, 1)
	duplicate := append(bytes.Clone(fixture.frame), fixture.frame...)
	before := bytes.Clone(duplicate)
	if _, err := RemoveDTSDialnormFrame(duplicate); err == nil || !bytes.Equal(duplicate, before) {
		t.Fatal("duplicate extension index did not fail atomically")
	}
}

func TestDTSDialnormMaximumWideFrame(t *testing.T) {
	fixture := dtsExtensionFixture(true, false, false, 1)
	frame := make([]byte, 1<<20)
	copy(frame, fixture.frame[:fixture.header])
	for i := 4096; i < len(frame); i++ {
		frame[i] = byte(i * 23)
	}
	writer := testBits{p: frame, pos: 43}
	writer.put(12, 4095)
	writer.put(20, len(frame)-1)
	writer.pos = fixture.assetLengths[0]
	writer.put(20, len(frame)-4096-1)
	binary.BigEndian.PutUint16(frame[4094:4096], referenceDTSCRC(frame[5:4094]))
	for _, swapped := range []bool{false, true} {
		input := bytes.Clone(frame)
		if swapped {
			swapWords(input)
		}
		before := bytes.Clone(input)
		stats, err := RemoveDTSDialnormFrame(input)
		if err != nil || stats.BytesWritten != 1<<20 || stats.ExtensionAssets != 1 || stats.ChangedFields != 1 {
			t.Fatalf("maximum wide frame swapped%v: %+v, %v", swapped, stats, err)
		}
		if !bytes.Equal(before[4096:], input[4096:]) {
			t.Fatal("large compressed payload changed")
		}
	}
}

func TestDTSDialnormRejectsDamagedMetadata(t *testing.T) {
	fixture := dtsExtensionFixture(false, true, true, 2)
	core := dtsCoreFixture(7, 3, false)
	for name, input := range map[string][]byte{
		"empty": nil, "short sync": core[:3], "short frame": core[:len(core)-1],
		"unknown sync":              bytes.Repeat([]byte{0}, 96),
		"14bit BE":                  append([]byte{0x1f, 0xff, 0xe8, 0x00}, make([]byte, 12)...),
		"14bit LE":                  append([]byte{0xff, 0x1f, 0x00, 0xe8}, make([]byte, 12)...),
		"bad extension CRC":         dtsMutate(fixture.frame, func(p []byte) { p[fixture.header-1] ^= 1 }),
		"core fixed bit":            dtsMutate(core, func(p []byte) { w := testBits{p: p, pos: 75}; w.put(1, 1) }),
		"core rate":                 dtsMutate(core, func(p []byte) { w := testBits{p: p, pos: 66}; w.put(4, 15) }),
		"core resolution":           dtsMutate(core, func(p []byte) { w := testBits{p: p, pos: 95}; w.put(3, 7) }),
		"tiny core":                 dtsMutate(core, func(p []byte) { w := testBits{p: p, pos: 46}; w.put(14, 0) }),
		"header exceeds frame":      dtsMutate(fixture.frame, func(p []byte) { w := testBits{p: p, pos: 51}; w.put(16, 10) }),
		"descriptor exceeds header": dtsMutateCRC(fixture, func(p []byte) { w := testBits{p: p, pos: fixture.descriptors[0]}; w.put(9, 511) }),
		"descriptor too short":      dtsMutateCRC(fixture, func(p []byte) { w := testBits{p: p, pos: fixture.descriptors[0]}; w.put(9, 1) }),
		"text exceeds descriptor":   dtsMutateCRC(fixture, func(p []byte) { w := testBits{p: p, pos: fixture.textLength}; w.put(10, 1023) }),
		"asset exceeds frame":       dtsMutateCRC(fixture, func(p []byte) { w := testBits{p: p, pos: fixture.assetLengths[0]}; w.put(16, 65535) }),
		"duplicate asset index":     dtsMutateCRC(fixture, func(p []byte) { w := testBits{p: p, pos: fixture.descriptors[1] + 9}; w.put(3, 0) }),
	} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			_, err := RemoveDTSDialnorm(bytes.NewReader(input), &out)
			if err == nil {
				t.Fatal("invalid input accepted")
			}
			if out.Len() != 0 {
				t.Fatal("partially changed invalid frame was emitted")
			}
		})
	}
	odd := dtsCoreFixture(7, 3, false)
	w := testBits{p: odd, pos: 46}
	w.put(14, len(odd)-2)
	swapWords(odd)
	if _, err := RemoveDTSDialnorm(bytes.NewReader(odd), io.Discard); err == nil || !strings.Contains(err.Error(), "odd-length") {
		t.Fatalf("odd word-swapped frame: %v", err)
	}
}

func TestDTSDialnormIOFailures(t *testing.T) {
	if _, err := RemoveDTSDialnorm(bytes.NewReader(nil), io.Discard); !errors.Is(err, ErrEmptyInput) {
		t.Fatalf("empty: %v", err)
	}
	sentinel := errors.New("read failure")
	if _, err := RemoveDTSDialnorm(iotest.ErrReader(sentinel), io.Discard); !errors.Is(err, sentinel) {
		t.Fatalf("read error: %v", err)
	}
	frame := dtsCoreFixture(7, 3, false)
	stats, err := RemoveDTSDialnorm(bytes.NewReader(frame), dtsFailWriter{})
	if !errors.Is(err, io.ErrClosedPipe) || stats.BytesWritten != 3 {
		t.Fatalf("write error: %+v, %v", stats, err)
	}
}

type dtsFailWriter struct{}

func (dtsFailWriter) Write(p []byte) (int, error) { return min(3, len(p)), io.ErrClosedPipe }

func TestDTSCRCKnownVector(t *testing.T) {
	if got := dtsCRC16([]byte("123456789")); got != 0x29b1 {
		t.Fatalf("CRC-16/CCITT-FALSE check: %04x", got)
	}
	for n := 0; n <= 4096; n += 17 {
		p := make([]byte, n)
		for i := range p {
			p[i] = byte(i*41 + n)
		}
		if dtsCRC16(p) != referenceDTSCRC(p) {
			t.Fatalf("CRC mismatch at %d bytes", n)
		}
		p = append(p, byte(dtsCRC16(p)>>8), byte(dtsCRC16(p)))
		if dtsCRC16(p) != 0 {
			t.Fatalf("nonzero CRC residue at %d bytes", n)
		}
	}
}

// The optional reference files are independently produced elementary streams.
// They remain outside the source tree and can include complete feature tracks.
func TestDTSDialnormReference(t *testing.T) {
	sourcePath, expectedPath := os.Getenv("EAC3TO_DTS_REFERENCE_INPUT"), os.Getenv("EAC3TO_DTS_REFERENCE_OUTPUT")
	if sourcePath == "" || expectedPath == "" {
		t.Skip("set EAC3TO_DTS_REFERENCE_INPUT and EAC3TO_DTS_REFERENCE_OUTPUT for an external byte comparison")
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	output, err := os.Create(filepath.Join(t.TempDir(), "removed.dts"))
	if err != nil {
		t.Fatal(err)
	}
	defer output.Close()
	stats, err := RemoveDTSDialnorm(source, output)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := output.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	expected, err := os.Open(expectedPath)
	if err != nil {
		t.Fatal(err)
	}
	defer expected.Close()
	var a, b [64 << 10]byte
	var offset int64
	for {
		n, ea := io.ReadFull(output, a[:])
		m, eb := io.ReadFull(expected, b[:])
		if n != m || !bytes.Equal(a[:n], b[:m]) {
			t.Fatalf("reference differs in block at byte %d", offset)
		}
		offset += int64(n)
		if ea != nil || eb != nil {
			if ea != eb || ea != io.EOF && ea != io.ErrUnexpectedEOF {
				t.Fatalf("reference read: %v, %v", ea, eb)
			}
			break
		}
	}
	t.Logf("%d bytes match; %+v", offset, stats)
}

func FuzzDTSDialnorm(f *testing.F) {
	f.Add(dtsCoreFixture(7, 3, true))
	f.Add(dtsExtensionFixture(false, true, true, 2).frame)
	f.Add(dtsExtensionFixture(true, true, true, 8).frame)
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 128<<10 {
			t.Skip()
		}
		// Repair the outer checksum so mutations also exercise descriptor
		// lengths, optional metadata and asset boundaries beyond the CRC gate.
		if len(data) >= 10 && binary.BigEndian.Uint32(data[:4]) == 0x64582025 {
			if shape, err := dtsFrameBounds(data); err == nil && shape.size <= len(data) {
				data = bytes.Clone(data)
				binary.BigEndian.PutUint16(data[shape.header-2:shape.header], referenceDTSCRC(data[5:shape.header-2]))
			}
		}
		var out bytes.Buffer
		stats, err := RemoveDTSDialnorm(bytes.NewReader(data), &out)
		if out.Len() > len(data) || stats.BytesRead > int64(len(data)) {
			t.Fatal("DTS transform expanded or read beyond input")
		}
		if err == nil && out.Len() != len(data) {
			t.Fatal("successful transform lost input bytes")
		}
		unit := bytes.Clone(data)
		_, unitErr := RemoveDTSDialnormFrame(unit)
		if unitErr != nil && !bytes.Equal(unit, data) {
			t.Fatal("failed unit edit was not atomic")
		}
	})
}

func dtsCoreFixture(version, code int, crc bool) []byte {
	p := make([]byte, 128)
	for i := range p {
		p[i] = byte(i*17 + 11)
	}
	b := testBits{p: p}
	for _, field := range [][2]int{{32, 0x7ffe8001}, {1, 1}, {5, 31}, {1, int(boolCount(crc))}, {7, 15}, {14, len(p) - 1}, {6, 2}, {4, 13}, {5, 15}, {1, 0}, {1, 0}, {1, 0}, {1, 0}, {1, 0}, {3, 0}, {1, 0}, {1, 0}, {2, 0}, {1, 0}} {
		b.put(field[0], field[1])
	}
	if crc {
		b.put(16, 0xa12b)
	}
	for _, field := range [][2]int{{1, 1}, {4, version}, {2, 0}, {3, 6}, {1, 0}, {1, 0}, {4, code}} {
		b.put(field[0], field[1])
	}
	return p
}

type dtsFixture struct {
	frame                               []byte
	header                              int
	dialnorm, descriptors, assetLengths []int
	textLength                          int
}

func dtsExtensionFixture(wide, static, rich bool, assets int) dtsFixture {
	if !static {
		assets = 1
	}
	p := make([]byte, 8192)
	for i := range p {
		p[i] = byte(i*37 + 7)
	}
	b := testBits{p: p}
	var result dtsFixture
	width, sizeWidth := 8, 16
	if wide {
		width, sizeWidth = 12, 20
	}
	b.put(32, 0x64582025)
	b.put(8, 0xa5)
	b.put(2, 3)
	b.put(1, int(boolCount(wide)))
	headerPosition := b.pos
	b.put(width, 0)
	framePosition := b.pos
	b.put(sizeWidth, 0)
	b.put(1, int(boolCount(static)))
	if static {
		b.put(2, 2)
		b.put(3, 0)
		b.put(1, 1)
		b.put(36, 0x123456789)
		b.put(3, 1)
		b.put(3, assets-1)
		b.put(4, 11)
		b.put(4, 9)
		for i := 0; i < 5; i++ {
			b.put(8, 0x03)
		}
		b.put(1, 1)
		b.put(2, 2)
		b.put(2, 1)
		b.put(2, 1)
		b.put(8, 3)
		b.put(8, 15)
	}
	dataSize := 0
	for i := 0; i < assets; i++ {
		size := 32 + i*16
		result.assetLengths = append(result.assetLengths, b.pos)
		b.put(sizeWidth, size-1)
		dataSize += size
	}
	for i := 0; i < assets; i++ {
		start := b.pos
		result.descriptors = append(result.descriptors, start)
		b.put(9, 0)
		b.put(3, i)
		if static {
			if rich {
				b.put(1, 1)
				b.put(4, 3)
				b.put(1, 1)
				b.put(24, 0x656e67)
				b.put(1, 1)
				result.textLength = b.pos
				b.put(10, 3)
				b.put(32, 0x74657374)
			} else {
				b.put(3, 0)
			}
			b.put(5, 23)
			b.put(4, 12)
			b.put(8, 7)
			b.put(1, int(boolCount(rich)))
			if rich {
				b.put(1, 1)
				b.put(1, 1)
				b.put(1, 1)
				b.put(2, 3)
				b.put(16, 0x2f)
				b.put(3, 2)
				b.put(16, 2)
				b.put(16, 16)
				for _, speakers := range []int{2, 1} {
					b.put(5, 31)
					for ch := 0; ch < speakers; ch++ {
						b.put(32, 0x80000001)
						b.put(5, 11)
						b.put(5, 17)
					}
				}
			} else {
				b.put(3, 2)
			}
		}
		b.put(1, 1)
		b.put(8, 0x5a)
		if i%3 == 2 {
			b.put(1, 0)
		} else {
			b.put(1, 1)
			result.dialnorm = append(result.dialnorm, b.pos)
			b.put(5, (i*7+4)%32)
		}
		b.put(16, 0xa57e)
		length := (b.pos - start + 7) / 8
		b.pos = start + length*8
		patch := testBits{p: p, pos: start}
		patch.put(9, length-1)
	}
	header := (b.pos+7)/8 + 2
	if wide && header < 300 {
		header = 300
	}
	if header%2 != 0 {
		header++
	}
	if !wide && header > 256 {
		panic(fmt.Sprintf("test header too large: %d", header))
	}
	total := header + dataSize
	patch := testBits{p: p, pos: headerPosition}
	patch.put(width, header-1)
	patch.pos = framePosition
	patch.put(sizeWidth, total-1)
	result.frame, result.header = p[:total], header
	binary.BigEndian.PutUint16(p[header-2:header], referenceDTSCRC(p[5:header-2]))
	return result
}

func dtsMappingFixture(channels, maskWidth int, drc bool) dtsFixture {
	p := make([]byte, 256)
	b := testBits{p: p}
	for _, field := range [][2]int{{32, 0x64582025}, {8, 0}, {2, 0}, {1, 0}, {8, 0}, {16, 0}, {1, 1}, {2, 2}, {3, 0}, {1, 0}, {3, 0}, {3, 0}, {1, 1}, {8, 1}, {1, 0}, {16, 31}} {
		b.put(field[0], field[1])
	}
	start := b.pos
	for _, field := range [][2]int{{9, 0}, {3, 0}, {3, 0}, {5, 23}, {4, 12}, {8, channels - 1}, {1, 1}} {
		b.put(field[0], field[1])
	}
	if channels > 2 {
		b.put(1, 1)
	}
	if channels > 6 {
		b.put(1, 1)
	}
	b.put(1, int(boolCount(maskWidth != 0)))
	if maskWidth != 0 {
		b.put(2, maskWidth/4-1)
		b.put(maskWidth, 2)
	}
	b.put(3, 0)
	b.put(1, int(boolCount(drc)))
	if drc {
		b.put(8, 0xad)
	}
	b.put(1, 1)
	position := b.pos
	b.put(5, 31)
	b.put(16, 0)
	descriptorLength := (b.pos - start + 7) / 8
	b.pos = start + descriptorLength*8
	header := (b.pos+7)/8 + 2
	if header&1 != 0 {
		header++
	}
	patch := testBits{p: p, pos: start}
	patch.put(9, descriptorLength-1)
	patch.pos = 43
	patch.put(8, header-1)
	patch.put(16, header+31)
	binary.BigEndian.PutUint16(p[header-2:header], referenceDTSCRC(p[5:header-2]))
	return dtsFixture{frame: p[:header+32], header: header, dialnorm: []int{position}}
}

func dtsMutate(input []byte, mutate func([]byte)) []byte {
	p := bytes.Clone(input)
	mutate(p)
	return p
}
func dtsMutateCRC(fixture dtsFixture, mutate func([]byte)) []byte {
	p := dtsMutate(fixture.frame, mutate)
	binary.BigEndian.PutUint16(p[fixture.header-2:fixture.header], referenceDTSCRC(p[5:fixture.header-2]))
	return p
}
func boolCount(value bool) int64 {
	if value {
		return 1
	}
	return 0
}

func referenceDTSCRC(data []byte) uint16 {
	crc := uint16(0xffff)
	for _, value := range data {
		for bit := 7; bit >= 0; bit-- {
			feedback := crc>>15 ^ uint16(value>>uint(bit)&1)
			crc <<= 1
			if feedback != 0 {
				crc ^= 0x1021
			}
		}
	}
	return crc
}
