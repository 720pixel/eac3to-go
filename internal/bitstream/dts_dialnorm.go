package bitstream

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"math/bits"
)

// DTSStats counts individual core and extension-substream frames. One DTS-HD
// access unit can contain both kinds and therefore contributes multiple Frames.
type DTSStats struct {
	Frames, ChangedFrames               int64
	CoreFrames, ExtensionFrames         int64
	ExtensionAssets                     int64
	DialnormFields, ChangedFields       int64
	HeaderCRCUpdates, ByteSwappedFrames int64
	EncoderRevisionChanges              int64
	CoreResolutionChanges               int64
	BytesRead, BytesWritten             int64
}

// RemoveDTSDialnorm clears dialog-normalization metadata in DTS core and DTS-HD
// extension-substream headers. It accepts 16-bit big-endian or word-swapped
// elementary streams, including standalone extension substreams. Fourteen-bit
// packing, containers, padding and lost synchronization are rejected.
// Core revisions are changed to 7 when needed to make the cleared gain code
// unambiguous, including revision 6's additional 16 dB attenuation.
//
// All asset descriptors are bounded by their declared lengths. Existing ExSS
// header checksums must be valid; changed headers receive a new checksum. Core
// HCRC words are preserved: ETSI explicitly excludes them from validation. This
// operation does not decode or verify the compressed audio payload.
//
// Header syntax and CRC definitions: ETSI TS 102 114 V1.6.1, clauses 5.4.2,
// 7.5.2, 7.5.3 and Annex B:
// https://www.etsi.org/deliver/etsi_ts/102100_102199/102114/01.06.01_60/ts_102114v010601p.pdf
func RemoveDTSDialnorm(source io.Reader, destination io.Writer) (DTSStats, error) {
	return processDTSHeaders(source, destination, true, false)
}

// CopyDTSHeaders preserves the compressed payload and applies the legacy core
// source-resolution repair. Dialog normalization is optional and independent.
func CopyDTSHeaders(source io.Reader, destination io.Writer, removeDialnorm bool) (DTSStats, error) {
	return processDTSHeaders(source, destination, removeDialnorm, true)
}

func processDTSHeaders(source io.Reader, destination io.Writer, removeDialnorm, repairResolution bool) (DTSStats, error) {
	var stats DTSStats
	reader := bufio.NewReaderSize(source, 64<<10)
	storage := make([]byte, 4096)
	for {
		offset := stats.BytesRead
		n, err := io.ReadFull(reader, storage[:10])
		stats.BytesRead += int64(n)
		if err == io.EOF && n == 0 {
			if stats.Frames == 0 {
				return stats, ErrEmptyInput
			}
			return stats, nil
		}
		if err != nil {
			return stats, fmt.Errorf("DTS header at byte %d: %w", offset, err)
		}
		swapped, err := dtsByteSwapped(storage[:10])
		if err != nil {
			return stats, fmt.Errorf("DTS frame at byte %d: %w", offset, err)
		}
		if swapped {
			swapWords(storage[:10])
		}
		shape, err := dtsFrameBounds(storage[:10])
		if err != nil {
			return stats, fmt.Errorf("DTS frame at byte %d: %w", offset, err)
		}
		if swapped && shape.size&1 != 0 {
			return stats, fmt.Errorf("DTS frame at byte %d: odd-length word-swapped frames are unsupported", offset)
		}
		if shape.size > len(storage) {
			larger := make([]byte, shape.size)
			copy(larger, storage[:10])
			storage = larger
		}
		frame := storage[:shape.size]
		n, err = io.ReadFull(reader, frame[10:])
		stats.BytesRead += int64(n)
		if err != nil {
			if err == io.EOF {
				err = io.ErrUnexpectedEOF
			}
			return stats, fmt.Errorf("DTS frame at byte %d: %w", offset, err)
		}
		if swapped {
			swapWords(frame[10:])
		}
		var edits dtsEdits
		if shape.extension {
			edits, err = dtsExtensionEdits(frame, shape)
		} else {
			edits, err = dtsCoreEdits(frame)
		}
		if err != nil {
			return stats, fmt.Errorf("DTS frame at byte %d: %w", offset, err)
		}
		if removeDialnorm {
			applyDTSEdits(frame, shape, edits, &stats)
		} else {
			stats.Frames++
			if shape.extension {
				stats.ExtensionFrames++
			} else {
				stats.CoreFrames++
			}
		}
		if repairResolution && !shape.extension && repairDTSCoreResolution(frame) {
			stats.CoreResolutionChanges++
		}
		if swapped {
			stats.ByteSwappedFrames++
			swapWords(frame)
		}
		n, err = writePacket(destination, frame)
		stats.BytesWritten += int64(n)
		if err != nil {
			return stats, fmt.Errorf("write DTS frame at byte %d: %w", offset, err)
		}
	}
}

// RemoveDTSDialnormFrame edits one complete DTS access unit in place. It accepts
// a core frame, up to four distinct extension substreams, or a core followed by
// those extensions. All headers, lengths and extension CRCs are validated before
// any byte is changed. Failure leaves the whole input untouched. The caller must
// own data, copying it first if another output needs its original metadata.
// Ordinary big-endian units require no heap allocation.
func RemoveDTSDialnormFrame(data []byte) (DTSStats, error) {
	var stats DTSStats
	if len(data) == 0 {
		return stats, ErrEmptyInput
	}
	type plan struct {
		offset  int
		shape   dtsFrameShape
		edits   dtsEdits
		swapped bool
	}
	var plans [5]plan
	var normalized [4096]byte
	count := 0
	corePresent := false
	var extensions uint8
	for offset := 0; offset < len(data); {
		if count == len(plans) {
			return stats, fmt.Errorf("DTS access unit has too many frames")
		}
		if len(data)-offset < 10 {
			return stats, fmt.Errorf("DTS header at byte %d: %w", offset, io.ErrUnexpectedEOF)
		}
		frame := data[offset:]
		swapped, err := dtsByteSwapped(frame[:10])
		if err != nil {
			return stats, fmt.Errorf("DTS frame at byte %d: %w", offset, err)
		}
		prefix := frame[:10]
		if swapped {
			copy(normalized[:10], prefix)
			swapWords(normalized[:10])
			prefix = normalized[:10]
		}
		shape, err := dtsFrameBounds(prefix)
		if err != nil {
			return stats, fmt.Errorf("DTS frame at byte %d: %w", offset, err)
		}
		if shape.size > len(frame) {
			return stats, fmt.Errorf("DTS frame at byte %d: %w", offset, io.ErrUnexpectedEOF)
		}
		if swapped && shape.size&1 != 0 {
			return stats, fmt.Errorf("DTS frame at byte %d: odd-length word-swapped frames are unsupported", offset)
		}
		if shape.extension {
			index := getBitsAt(prefix, 40, 2)
			if extensions&(1<<uint(index)) != 0 {
				return stats, fmt.Errorf("duplicate DTS extension substream index %d in access unit", index)
			}
			extensions |= 1 << uint(index)
		} else {
			if corePresent || count != 0 {
				return stats, fmt.Errorf("DTS access unit has multiple or misplaced core frames")
			}
			corePresent = true
		}
		frame = frame[:shape.size]
		if swapped {
			length := 96
			if shape.extension {
				length = (shape.header + 1) &^ 1
			}
			copy(normalized[:length], frame[:length])
			swapWords(normalized[:length])
			frame = normalized[:length]
		}
		var edits dtsEdits
		if shape.extension {
			edits, err = dtsExtensionEdits(frame, shape)
		} else {
			edits, err = dtsCoreEdits(frame)
		}
		if err != nil {
			return stats, fmt.Errorf("DTS frame at byte %d: %w", offset, err)
		}
		plans[count] = plan{offset, shape, edits, swapped}
		count++
		offset += shape.size
	}
	for _, p := range plans[:count] {
		frame := data[p.offset : p.offset+p.shape.size]
		if p.swapped {
			swapWords(frame)
		}
		applyDTSEdits(frame, p.shape, p.edits, &stats)
		if p.swapped {
			swapWords(frame)
			stats.ByteSwappedFrames++
		}
	}
	stats.BytesRead, stats.BytesWritten = int64(len(data)), int64(len(data))
	return stats, nil
}

func applyDTSEdits(frame []byte, shape dtsFrameShape, edits dtsEdits, stats *DTSStats) {
	changed := int64(0)
	for _, position := range edits.positions[:edits.count] {
		if getBitsAt(frame, position, edits.width) != 0 {
			clearDTSBits(frame, position, edits.width)
			changed++
		}
	}
	if edits.revisionPosition != 0 {
		clearDTSBits(frame, edits.revisionPosition, 4)
		for bit := 0; bit < 3; bit++ {
			position := edits.revisionPosition + 1 + bit
			frame[position/8] |= 1 << uint(7-position%8)
		}
		stats.EncoderRevisionChanges++
		if changed == 0 {
			changed = 1
		}
	}
	if changed > 0 {
		stats.ChangedFrames++
		if shape.extension {
			binary.BigEndian.PutUint16(frame[shape.header-2:shape.header], dtsCRC16(frame[5:shape.header-2]))
			stats.HeaderCRCUpdates++
		}
	}
	stats.Frames++
	stats.DialnormFields += int64(edits.count)
	stats.ChangedFields += changed
	if shape.extension {
		stats.ExtensionFrames++
		stats.ExtensionAssets += int64(edits.assets)
	} else {
		stats.CoreFrames++
	}
}

func dtsByteSwapped(prefix []byte) (bool, error) {
	switch binary.BigEndian.Uint32(prefix[:4]) {
	case 0x7ffe8001, 0x64582025:
		return false, nil
	case 0xfe7f0180, 0x58642520:
		return true, nil
	case 0x1fffe800, 0xff1f00e8:
		return false, fmt.Errorf("14-bit packing is not supported")
	default:
		return false, fmt.Errorf("expected a core or extension-substream sync word")
	}
}

type dtsFrameShape struct {
	size, header, sizeBits int
	extension              bool
}

func dtsFrameBounds(prefix []byte) (dtsFrameShape, error) {
	var result dtsFrameShape
	if len(prefix) < 10 {
		return result, io.ErrUnexpectedEOF
	}
	switch binary.BigEndian.Uint32(prefix[:4]) {
	case 0x7ffe8001:
		result.size = getBitsAt(prefix, 46, 14) + 1
		if result.size < 96 {
			return result, fmt.Errorf("invalid DTS core frame length %d", result.size)
		}
	case 0x64582025:
		result.extension = true
		wide := getBitsAt(prefix, 42, 1)
		result.sizeBits = 16 + 4*wide
		result.header = getBitsAt(prefix, 43, 8+4*wide) + 1
		result.size = getBitsAt(prefix, 51+4*wide, result.sizeBits) + 1
		minimum := (67+8*wide+7)/8 + 2
		if result.header < minimum || result.header >= result.size {
			return result, fmt.Errorf("invalid DTS extension header/frame lengths %d/%d", result.header, result.size)
		}
	default:
		return result, fmt.Errorf("invalid DTS frame sync word")
	}
	return result, nil
}

type dtsEdits struct {
	positions            [8]int
	count, width, assets int
	revisionPosition     int
}

func dtsCoreEdits(frame []byte) (dtsEdits, error) {
	var edits dtsEdits
	if len(frame) < 96 {
		return edits, io.ErrUnexpectedEOF
	}
	if getBitsAt(frame, 32, 1) != 0 && getBitsAt(frame, 33, 5) != 31 {
		return edits, fmt.Errorf("invalid DTS normal-frame deficit sample count")
	}
	if getBitsAt(frame, 60, 6) > 15 {
		return edits, fmt.Errorf("unsupported DTS core channel arrangement")
	}
	switch getBitsAt(frame, 66, 4) {
	case 1, 2, 3, 6, 7, 8, 11, 12, 13:
	default:
		return edits, fmt.Errorf("reserved DTS core sample-rate code")
	}
	if getBitsAt(frame, 75, 1) != 0 || getBitsAt(frame, 85, 2) == 3 {
		return edits, fmt.Errorf("reserved DTS core header flag")
	}
	shift := 16 * getBitsAt(frame, 38, 1)
	switch getBitsAt(frame, 95+shift, 3) {
	case 0, 1, 2, 3, 5, 6:
	default:
		return edits, fmt.Errorf("reserved DTS source PCM resolution")
	}
	version := getBitsAt(frame, 89+shift, 4)
	edits.positions[0], edits.count, edits.width = 100+shift, 1, 4
	if version != 7 && (version == 6 || getBitsAt(frame, 100+shift, 4) != 0) {
		edits.revisionPosition = 89 + shift
	}
	// Revision 6 represents an additional -16 dB. A nonzero normalization
	// nibble also requires revision 7 to make the cleared code unambiguous.
	return edits, nil
}

func dtsExtensionEdits(frame []byte, shape dtsFrameShape) (dtsEdits, error) {
	edits := dtsEdits{width: 5}
	if shape.header > len(frame) || shape.header < 12 {
		return edits, fmt.Errorf("invalid DTS extension header extent")
	}
	if binary.BigEndian.Uint16(frame[shape.header-2:shape.header]) != dtsCRC16(frame[5:shape.header-2]) {
		return edits, fmt.Errorf("DTS extension header CRC mismatch")
	}
	metadataPosition := 67
	if shape.sizeBits == 20 {
		metadataPosition = 75
	}
	b := dtsCursor{data: frame, pos: metadataPosition, limit: (shape.header - 2) * 8}
	static := b.read(1) != 0
	assets := 1
	if static {
		if b.read(2) == 3 {
			return edits, fmt.Errorf("reserved DTS extension reference clock")
		}
		b.skip(3)
		if b.read(1) != 0 {
			b.skip(36)
		}
		presentations := b.read(3) + 1
		assets = b.read(3) + 1
		index := getBitsAt(frame, 40, 2)
		var activeMasks [8]int
		for i := range activeMasks[:presentations] {
			activeMasks[i] = b.read(index + 1)
		}
		for _, mask := range activeMasks[:presentations] {
			b.skip(bits.OnesCount(uint(mask)) * 8)
		}
		if b.read(1) != 0 {
			b.skip(2)
			maskBits := (b.read(2) + 1) * 4
			mixConfigurations := b.read(2) + 1
			b.skip(mixConfigurations * maskBits)
		}
	}
	remaining := shape.size - shape.header
	for i := 0; i < assets; i++ {
		size := b.read(shape.sizeBits) + 1
		if size > remaining {
			return edits, fmt.Errorf("DTS extension asset %d exceeds frame bounds", i)
		}
		remaining -= size
	}
	if b.err != 0 {
		return edits, fmt.Errorf("truncated DTS extension metadata: %w", dtsCursorError(b.err))
	}
	var seen uint8
	for i := 0; i < assets; i++ {
		start := b.pos
		length := b.read(9) + 1
		end := start + length*8
		if b.err != 0 || end > b.limit || end < b.pos+5 {
			return edits, fmt.Errorf("DTS asset %d descriptor exceeds header bounds", i)
		}
		limit := b.limit
		b.limit = end
		index := b.read(3)
		if seen&(1<<uint(index)) != 0 {
			return edits, fmt.Errorf("duplicate DTS asset index %d", index)
		}
		seen |= 1 << uint(index)
		if static {
			skipDTSAssetStatic(&b)
		}
		if b.read(1) != 0 {
			b.skip(8)
		}
		if b.read(1) != 0 {
			edits.positions[edits.count] = b.pos
			edits.count++
			b.skip(5)
		}
		if b.err != 0 {
			return edits, fmt.Errorf("DTS asset %d descriptor: %w", i, dtsCursorError(b.err))
		}
		// The rest of the descriptor is unrelated DRC, mixing, decoder
		// navigation and reserved metadata. Its declared extent is authoritative.
		b.pos, b.limit = end, limit
	}
	edits.assets = assets
	return edits, nil
}

func skipDTSAssetStatic(b *dtsCursor) {
	if b.read(1) != 0 {
		b.skip(4)
	}
	if b.read(1) != 0 {
		b.skip(24)
	}
	if b.read(1) != 0 {
		b.skip((b.read(10) + 1) * 8)
	}
	b.skip(9) // Source resolution and maximum sample-rate code.
	channels := b.read(8) + 1
	if b.read(1) == 0 {
		b.skip(3)
		return
	}
	if channels > 2 {
		b.skip(1)
	}
	if channels > 6 {
		b.skip(1)
	}
	maskBits := 0
	if b.read(1) != 0 {
		maskBits = (b.read(2) + 1) * 4
		b.skip(maskBits)
	}
	sets := b.read(3)
	if sets != 0 && maskBits == 0 {
		b.err = 2
		return
	}
	var speakers [7]int
	for i := 0; i < sets; i++ {
		speakers[i] = dtsSpeakerCount(b.read(maskBits))
	}
	for i := 0; i < sets; i++ {
		decoded := b.read(5) + 1
		for j := 0; j < speakers[i]; j++ {
			b.skip(bits.OnesCount32(uint32(b.read(decoded))) * 5)
		}
	}
}

func dtsSpeakerCount(mask int) int {
	// Table 7-10 assigns some activity bits to a left/right speaker pair.
	weights := [16]int{1, 2, 2, 1, 1, 2, 2, 1, 1, 2, 2, 2, 1, 2, 1, 2}
	count := 0
	for i, weight := range weights {
		if mask&(1<<uint(i)) != 0 {
			count += weight
		}
	}
	return count
}

type dtsCursor struct {
	data       []byte
	pos, limit int
	err        uint8
}

func (b *dtsCursor) read(n int) int {
	start := b.pos
	b.skip(n)
	if b.err != 0 {
		return 0
	}
	return getBitsAt(b.data, start, n)
}

func (b *dtsCursor) skip(n int) {
	if b.err != 0 {
		return
	}
	if n < 0 || n > b.limit-b.pos || b.limit > len(b.data)*8 {
		b.err = 1
		return
	}
	b.pos += n
}

func dtsCursorError(code uint8) error {
	if code == 2 {
		return fmt.Errorf("speaker remapping has no declared speaker-mask width")
	}
	return io.ErrUnexpectedEOF
}

func clearDTSBits(frame []byte, position, width int) {
	for i := position; i < position+width; i++ {
		frame[i/8] &^= 1 << uint(7-i%8)
	}
}

var dtsCRCTable = func() [256]uint16 {
	var table [256]uint16
	for i := range table {
		crc := uint16(i) << 8
		for bit := 0; bit < 8; bit++ {
			if crc&0x8000 != 0 {
				crc = crc<<1 ^ 0x1021
			} else {
				crc <<= 1
			}
		}
		table[i] = crc
	}
	return table
}()

func dtsCRC16(data []byte) uint16 {
	crc := uint16(0xffff)
	for _, v := range data {
		crc = crc<<8 ^ dtsCRCTable[byte(crc>>8)^v]
	}
	return crc
}
