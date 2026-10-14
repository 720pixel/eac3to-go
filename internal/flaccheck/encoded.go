package flaccheck

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
)

// ErrFormatChange identifies a legal FLAC stream whose PCM format changes.
// A single PCM verifier cannot safely check such a stream after conversion.
var ErrFormatChange = errors.New("FLAC stream changes its PCM format")

// EncodedResult describes a structurally checked native FLAC stream. Frames is
// the number of interchannel PCM samples; EncodedFrames counts compressed frames.
// The stored PCM MD5 is retained in Info but is not checked without decoding.
type EncodedResult struct {
	Info          Info
	Frames        uint64
	EncodedFrames uint64
}

func ValidateFile(path string) (EncodedResult, error) {
	return ValidateFileContext(context.Background(), path)
}

func ValidateFileContext(ctx context.Context, path string) (EncodedResult, error) {
	f, err := os.Open(path)
	if err != nil {
		return EncodedResult{}, err
	}
	defer f.Close()
	return ValidateContext(ctx, f)
}

// Validate checks the native FLAC marker, metadata framing, and every encoded
// frame. It follows subframe and residual lengths rather than searching for sync
// bytes, checks both CRCs, and requires continuous frame or sample numbering.
// It uses bounded memory and does not reconstruct PCM or inspect artwork/tags.
// With no stored total or MD5, removal of complete final frames is undetectable.
func Validate(r io.Reader) (EncodedResult, error) {
	return ValidateContext(context.Background(), r)
}

// ValidateContext also checks cancellation when refilling its input buffer.
func ValidateContext(ctx context.Context, r io.Reader) (EncodedResult, error) {
	var result EncodedResult
	if err := ctx.Err(); err != nil {
		return result, err
	}
	input := bufio.NewReaderSize(encodedContextReader{ctx, r}, 64<<10)
	info, offset, err := encodedMetadata(input)
	if err != nil {
		return result, err
	}
	result.Info = info
	bits := frameBits{reader: input, offset: offset, maxBytes: uint64(info.MaxFrameSize)}
	var previous, firstBlock uint32
	var variable bool
	for {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if _, err := input.Peek(1); err != nil {
			if err == io.EOF {
				break
			}
			return result, err
		}
		if result.EncodedFrames > 0 {
			if previous < uint32(info.MinBlockSize) {
				return result, fmt.Errorf("%w: non-final block has %d samples, below STREAMINFO minimum %d", ErrCorrupt, previous, info.MinBlockSize)
			}
			if !variable && previous != firstBlock {
				return result, fmt.Errorf("%w: a shortened block precedes the last fixed-size frame", ErrCorrupt)
			}
		}
		bits.start, bits.crc8, bits.crc16, bits.header = bits.offset, 0, 0, true
		h := bits.frameHeader(info)
		if bits.err != nil {
			return result, encodedFrameError(result.EncodedFrames, bits.start, bits.err)
		}
		if result.EncodedFrames == 0 {
			variable, firstBlock = h.variable, h.block
		} else if h.variable != variable {
			return result, fmt.Errorf("%w: blocking strategy changes at encoded frame %d", ErrCorrupt, result.EncodedFrames)
		}
		wantNumber := result.EncodedFrames
		if variable {
			wantNumber = result.Frames
		}
		if h.number != wantNumber {
			return result, fmt.Errorf("%w: encoded frame %d has number %d, expected %d", ErrCorrupt, result.EncodedFrames, h.number, wantNumber)
		}
		if h.block > uint32(info.MaxBlockSize) || !variable && h.block > firstBlock {
			return result, fmt.Errorf("%w: invalid block size %d at encoded frame %d", ErrCorrupt, h.block, result.EncodedFrames)
		}
		for channel := 0; channel < h.channels && bits.err == nil; channel++ {
			depth := h.depth
			if h.assignment == 8 && channel == 1 || h.assignment == 9 && channel == 0 || h.assignment == 10 && channel == 1 {
				depth++
			}
			bits.subframe(h.block, depth)
		}
		if bits.remaining != 0 && bits.read(uint(bits.remaining)) != 0 {
			bits.bad("nonzero frame alignment padding")
		}
		crc := bits.crc16
		stored := uint16(bits.byte()) << 8
		stored |= uint16(bits.byte())
		if bits.err == nil && stored != crc {
			bits.bad("frame CRC16 mismatch: stored %04x, calculated %04x", stored, crc)
		}
		if bits.err != nil {
			return result, encodedFrameError(result.EncodedFrames, bits.start, bits.err)
		}
		if size := bits.offset - bits.start; info.MinFrameSize != 0 && size < uint64(info.MinFrameSize) {
			return result, fmt.Errorf("%w: encoded frame %d is smaller than STREAMINFO minimum", ErrCorrupt, result.EncodedFrames)
		}
		if ^uint64(0)-result.Frames < uint64(h.block) {
			return result, fmt.Errorf("%w: PCM sample count overflow", ErrCorrupt)
		}
		result.Frames += uint64(h.block)
		result.EncodedFrames++
		previous = h.block
		if info.TotalFrames != 0 && result.Frames > info.TotalFrames {
			return result, fmt.Errorf("%w: encoded audio exceeds the %d frames declared by STREAMINFO", ErrCorrupt, info.TotalFrames)
		}
	}
	if result.EncodedFrames == 0 {
		return result, fmt.Errorf("%w: no encoded audio frames", ErrCorrupt)
	}
	if info.TotalFrames != 0 && result.Frames != info.TotalFrames {
		return result, fmt.Errorf("%w: encoded audio contains %d frames, STREAMINFO declares %d", ErrCorrupt, result.Frames, info.TotalFrames)
	}
	return result, nil
}

func encodedFrameError(index, offset uint64, err error) error {
	return fmt.Errorf("FLAC encoded frame %d at byte %d: %w", index, offset, err)
}

type encodedContextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r encodedContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func encodedReadError(where string, err error) error {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return fmt.Errorf("%w: truncated %s: %w", ErrCorrupt, where, err)
	}
	return err
}

func encodedMetadata(r *bufio.Reader) (Info, uint64, error) {
	var marker [4]byte
	if _, err := io.ReadFull(r, marker[:]); err != nil {
		return Info{}, 0, encodedReadError("FLAC marker", err)
	}
	if string(marker[:]) != "fLaC" {
		return Info{}, 0, fmt.Errorf("%w: native FLAC marker is missing", ErrCorrupt)
	}
	var info Info
	var scratch [8192]byte
	offset := uint64(4)
	first := true
	for {
		var header [4]byte
		if _, err := io.ReadFull(r, header[:]); err != nil {
			return info, offset, encodedReadError("FLAC metadata header", err)
		}
		offset += 4
		kind := header[0] & 127
		size := uint32(header[1])<<16 | uint32(header[2])<<8 | uint32(header[3])
		if kind == 127 || first && kind != 0 || !first && kind == 0 {
			return info, offset, fmt.Errorf("%w: invalid FLAC metadata block type/order", ErrCorrupt)
		}
		if first {
			if size != 34 {
				return info, offset, fmt.Errorf("%w: STREAMINFO length is %d, expected 34", ErrCorrupt, size)
			}
			if _, err := io.ReadFull(r, scratch[:34]); err != nil {
				return info, offset, encodedReadError("FLAC STREAMINFO", err)
			}
			var err error
			info, err = ParseStreamInfo(scratch[:34])
			if err != nil {
				return info, offset, fmt.Errorf("%w: %v", ErrCorrupt, err)
			}
		} else {
			if kind == 2 && size < 4 || kind == 3 && size%18 != 0 {
				return info, offset, fmt.Errorf("%w: invalid length for FLAC metadata type %d", ErrCorrupt, kind)
			}
			for remaining := size; remaining > 0; {
				n := min(remaining, uint32(len(scratch)))
				if _, err := io.ReadFull(r, scratch[:n]); err != nil {
					return info, offset, encodedReadError("FLAC metadata body", err)
				}
				if kind == 1 {
					for _, value := range scratch[:n] {
						if value != 0 {
							return info, offset, fmt.Errorf("%w: nonzero FLAC metadata padding", ErrCorrupt)
						}
					}
				}
				remaining -= n
			}
		}
		offset += uint64(size)
		first = false
		if header[0]&128 != 0 {
			return info, offset, nil
		}
	}
}

type encodedHeader struct {
	block      uint32
	number     uint64
	channels   int
	depth      int
	assignment byte
	variable   bool
}

func (b *frameBits) frameHeader(info Info) encodedHeader {
	var h encodedHeader
	first, second := b.byte(), b.byte()
	if first != 0xff || second&0xfe != 0xf8 {
		b.bad("invalid frame sync at an expected frame boundary")
		return h
	}
	h.variable = second&1 != 0
	codes, format := b.byte(), b.byte()
	blockCode, rateCode := codes>>4, codes&15
	h.assignment = format >> 4
	depthCode := format >> 1 & 7
	if blockCode == 0 || rateCode == 15 || h.assignment > 10 || depthCode == 3 || format&1 != 0 {
		b.bad("reserved or forbidden frame header field")
		return h
	}
	h.number = b.codedNumber(h.variable)
	switch blockCode {
	case 1:
		h.block = 192
	case 2, 3, 4, 5:
		h.block = 144 << blockCode
	case 6:
		h.block = uint32(b.byte()) + 1
	case 7:
		h.block = uint32(b.byte()) << 8
		h.block += uint32(b.byte()) + 1
		if h.block == 65536 {
			b.bad("forbidden block size 65536")
		}
	default:
		h.block = 1 << blockCode
	}
	rates := [...]int{0, 88200, 176400, 192000, 8000, 16000, 22050, 24000, 32000, 44100, 48000, 96000}
	rate := info.SampleRate
	switch rateCode {
	case 0:
	case 12:
		rate = int(b.byte()) * 1000
	case 13, 14:
		rate = int(b.byte()) << 8
		rate |= int(b.byte())
		if rateCode == 14 {
			rate *= 10
		}
	default:
		rate = rates[rateCode]
	}
	depths := [...]int{0, 8, 12, 0, 16, 20, 24, 32}
	h.depth = depths[depthCode]
	if depthCode == 0 {
		h.depth = info.BitsPerSample
	}
	h.channels = int(h.assignment) + 1
	if h.assignment >= 8 {
		h.channels = 2
	}
	crc := b.crc8
	stored := b.byte()
	b.header = false
	if stored != crc {
		b.bad("header CRC8 mismatch: stored %02x, calculated %02x", stored, crc)
	}
	if b.err == nil && (rate != info.SampleRate || h.channels != info.Channels || h.depth != info.BitsPerSample) {
		b.err = fmt.Errorf("%w: frame is %d Hz/%d channels/%d bits, STREAMINFO is %d Hz/%d channels/%d bits", ErrFormatChange, rate, h.channels, h.depth, info.SampleRate, info.Channels, info.BitsPerSample)
	}
	return h
}

func (b *frameBits) codedNumber(variable bool) uint64 {
	first := b.byte()
	if first < 128 {
		return uint64(first)
	}
	n := 0
	for mask := byte(128); first&mask != 0 && mask != 0; mask >>= 1 {
		n++
	}
	if n < 2 || n > 7 || !variable && n == 7 {
		b.bad("invalid coded frame/sample number")
		return 0
	}
	value := uint64(first & (127 >> n))
	for i := 1; i < n; i++ {
		next := b.byte()
		if next&192 != 128 {
			b.bad("invalid continuation in coded frame/sample number")
			return 0
		}
		value = value<<6 | uint64(next&63)
	}
	minimum := [...]uint64{0, 0, 0x80, 0x800, 0x10000, 0x200000, 0x4000000, 0x80000000}
	if value < minimum[n] {
		b.bad("overlong coded frame/sample number")
	}
	return value
}
