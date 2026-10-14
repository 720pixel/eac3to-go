package flaccheck

import (
	"crypto/md5"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"math"
)

var (
	ErrCorrupt     = errors.New("FLAC integrity check failed")
	ErrFinished    = errors.New("FLAC verifier is finished")
	ErrInputFormat = errors.New("FLAC verifier input format changed")
)

// Result describes the original decoded samples, before any audio processing.
// Checked fields report which metadata comparisons were performed. Success also
// requires a nil error from Finish. Both are false when the metadata is unknown.
type Result struct {
	Frames       uint64
	MD5          [16]byte
	CountChecked bool
	MD5Checked   bool
}

// Verifier checks a single decoded stream using fixed-size working storage.
// Feed either normalized F64 samples or left-aligned S32LE bytes, in original
// channel order and without resampling, dithering, or other transformations.
// Use NewVerifier to initialize it. A Verifier must not be used concurrently.
type Verifier struct {
	info       Info
	hash       hash.Hash
	scale      float64
	width      int
	shift      uint
	padding    uint32
	samples    uint64
	mode       byte
	buffer     [8192]byte
	used       int
	partial    [4]byte
	partialLen int
	finished   bool
	err        error
	result     Result
}

func NewVerifier(info Info) (*Verifier, error) {
	if err := validateFormat(info); err != nil {
		return nil, err
	}
	shift := uint(32 - info.BitsPerSample)
	return &Verifier{
		info:    info,
		hash:    md5.New(),
		scale:   math.Ldexp(1, info.BitsPerSample-1),
		width:   (info.BitsPerSample + 7) / 8,
		shift:   shift,
		padding: uint32(1)<<shift - 1,
	}, nil
}

// WriteF64 accepts normalized, interleaved PCM. Each value must lie exactly on
// the source's integer sample grid. Calls may split an interchannel frame.
func (v *Verifier) WriteF64(samples []float64) error {
	if err := v.begin('f', len(samples)); err != nil {
		return err
	}
	for _, sample := range samples {
		value := sample * v.scale
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return v.sampleError("non-finite decoded sample")
		}
		if value < -v.scale || value >= v.scale {
			return v.sampleError("decoded sample exceeds the source precision")
		}
		if math.Trunc(value) != value {
			return v.sampleError("decoded sample is not on the source integer grid")
		}
		if err := v.add(int32(value)); err != nil {
			return err
		}
	}
	return nil
}

// WriteS32LE accepts signed 32-bit little-endian PCM with the source sample in
// the most significant bits. Unused low bits must be zero. Byte chunks need not
// align with samples or channel frames. On error, n includes the invalid word.
func (v *Verifier) WriteS32LE(data []byte) (n int, err error) {
	if err := v.begin('s', len(data)); err != nil {
		return 0, err
	}
	if v.partialLen != 0 {
		n = copy(v.partial[v.partialLen:], data)
		v.partialLen += n
		if v.partialLen < 4 {
			return n, nil
		}
		v.partialLen = 0
		if err := v.addS32(binary.LittleEndian.Uint32(v.partial[:])); err != nil {
			return n, err
		}
	}
	for len(data)-n >= 4 {
		word := binary.LittleEndian.Uint32(data[n : n+4])
		n += 4
		if err := v.addS32(word); err != nil {
			return n, err
		}
	}
	if n < len(data) {
		v.partialLen = copy(v.partial[:], data[n:])
		n += v.partialLen
	}
	return n, nil
}

// Finish checks channel framing, any known total count, and any stored MD5.
// It is idempotent. A write failure remains an error even if Finish is called.
func (v *Verifier) Finish() (Result, error) {
	if v.finished {
		return v.result, v.err
	}
	v.finished = true
	v.flush()
	v.result.Frames = v.samples / uint64(v.info.Channels)
	copy(v.result.MD5[:], v.hash.Sum(nil))
	if v.err != nil {
		return v.result, v.err
	}
	if v.partialLen != 0 {
		return v.result, v.fail(fmt.Errorf("%w: incomplete S32LE sample (%d trailing bytes)", ErrCorrupt, v.partialLen))
	}
	if v.samples%uint64(v.info.Channels) != 0 {
		return v.result, v.fail(fmt.Errorf("%w: incomplete channel frame (%d samples for %d channels)", ErrCorrupt, v.samples, v.info.Channels))
	}
	var failures []error
	if v.info.TotalFrames != 0 {
		v.result.CountChecked = true
		if v.result.Frames != v.info.TotalFrames {
			failures = append(failures, fmt.Errorf("%w: decoded %d frames, STREAMINFO declares %d", ErrCorrupt, v.result.Frames, v.info.TotalFrames))
		}
	}
	if v.info.MD5 != ([16]byte{}) {
		v.result.MD5Checked = true
		if v.result.MD5 != v.info.MD5 {
			failures = append(failures, fmt.Errorf("%w: PCM MD5 mismatch (decoded %x, STREAMINFO %x)", ErrCorrupt, v.result.MD5, v.info.MD5))
		}
	}
	v.err = errors.Join(failures...)
	return v.result, v.err
}

func (v *Verifier) begin(mode byte, size int) error {
	if v.err != nil {
		return v.err
	}
	if v.finished {
		return ErrFinished
	}
	if size == 0 {
		return nil
	}
	if v.mode != 0 && v.mode != mode {
		return v.fail(ErrInputFormat)
	}
	v.mode = mode
	return nil
}

func (v *Verifier) addS32(word uint32) error {
	if word&v.padding != 0 {
		return v.sampleError("nonzero padding bits in left-aligned S32LE sample")
	}
	return v.add(int32(word) >> v.shift)
}

func (v *Verifier) add(sample int32) error {
	if v.samples == math.MaxUint64 {
		return v.fail(fmt.Errorf("%w: decoded sample count overflow", ErrCorrupt))
	}
	if v.info.TotalFrames != 0 && v.samples >= v.info.TotalFrames*uint64(v.info.Channels) {
		return v.fail(fmt.Errorf("%w: decoded more than the %d frames declared by STREAMINFO", ErrCorrupt, v.info.TotalFrames))
	}
	if v.used+v.width > len(v.buffer) {
		v.flush()
	}
	// The FLAC checksum stores each signed sample in the smallest whole number
	// of bytes. Casting preserves sign extension for non-byte-aligned depths.
	word := uint32(sample)
	for i := 0; i < v.width; i++ {
		v.buffer[v.used+i] = byte(word >> (8 * i))
	}
	v.used += v.width
	v.samples++
	return nil
}

func (v *Verifier) flush() {
	if v.used != 0 {
		_, _ = v.hash.Write(v.buffer[:v.used])
		v.used = 0
	}
}

func (v *Verifier) sampleError(message string) error {
	return v.fail(fmt.Errorf("%w: %s at frame %d, channel %d", ErrCorrupt, message, v.samples/uint64(v.info.Channels)+1, v.samples%uint64(v.info.Channels)+1))
}

func (v *Verifier) fail(err error) error {
	if v.err == nil {
		v.err = err
	}
	return v.err
}
