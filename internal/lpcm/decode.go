// Package lpcm unpacks complete Blu-ray LPCM PES payloads into little-endian
// integer PCM in WAVE/FFmpeg speaker order, without a floating-point conversion.
// Channel assignments are checked against FFmpeg's pcm-bluray.c:
// https://github.com/FFmpeg/FFmpeg/blob/n6.1/libavcodec/pcm-bluray.c
package lpcm

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const MaxPayloadBytes = 65535 + 4

var ErrFormatChange = errors.New("Blu-ray LPCM format changed")

type Format struct {
	Rate, Bits, StorageBits, Channels int
	Layout                            string
	Mask                              uint64
	// SourceChannels includes the unused slot in odd-channel assignments.
	SourceChannels int
}

type Result struct {
	// PCM is borrowed until the next Decode call. Packed 20-bit samples retain
	// their left-aligned 24-bit storage, including all original storage bits.
	PCM    []byte
	Format Format
	Frames int
	// PaddingBytes counts explicitly validated unused channel slots.
	PaddingBytes int
}

type Config struct{ MaxPayloadBytes int }

type Decoder struct {
	max        int
	buffer     []byte
	format     Format
	haveFormat bool
}

func New(config Config) (*Decoder, error) {
	if config.MaxPayloadBytes == 0 {
		config.MaxPayloadBytes = MaxPayloadBytes
	}
	if config.MaxPayloadBytes < 4 || config.MaxPayloadBytes > MaxPayloadBytes {
		return nil, fmt.Errorf("invalid LPCM payload bound %d", config.MaxPayloadBytes)
	}
	return &Decoder{max: config.MaxPayloadBytes}, nil
}

type assignment struct {
	channels int
	layout   string
	mask     uint64
	order    [8]int
}

var assignments = [16]assignment{
	1:  {1, "mono", 0x4, [8]int{0}},
	3:  {2, "stereo", 0x3, [8]int{0, 1}},
	4:  {3, "3.0", 0x7, [8]int{0, 1, 2}},
	5:  {3, "3.0(back)", 0x103, [8]int{0, 1, 2}},
	6:  {4, "4.0", 0x107, [8]int{0, 1, 2, 3}},
	7:  {4, "quad(side)", 0x603, [8]int{0, 1, 2, 3}},
	8:  {5, "5.0(side)", 0x607, [8]int{0, 1, 2, 3, 4}},
	9:  {6, "5.1(side)", 0x60f, [8]int{0, 1, 2, 5, 3, 4}},
	10: {7, "7.0", 0x637, [8]int{0, 1, 2, 4, 5, 3, 6}},
	11: {8, "7.1", 0x63f, [8]int{0, 1, 2, 7, 4, 5, 3, 6}},
}

func ParseHeader(header []byte) (Format, error) {
	if len(header) < 4 {
		return Format{}, io.ErrUnexpectedEOF
	}
	a := assignments[header[2]>>4]
	if a.channels == 0 {
		return Format{}, fmt.Errorf("reserved LPCM channel assignment %d", header[2]>>4)
	}
	rate := 0
	switch header[2] & 15 {
	case 1:
		rate = 48000
	case 4:
		rate = 96000
	case 5:
		rate = 192000
	default:
		return Format{}, fmt.Errorf("reserved LPCM sample-rate code %d", header[2]&15)
	}
	bits := [4]int{0, 16, 20, 24}[header[3]>>6]
	if bits == 0 {
		return Format{}, fmt.Errorf("reserved LPCM sample-depth code 0")
	}
	storage := 16
	if bits > 16 {
		storage = 24
	}
	return Format{Rate: rate, Bits: bits, StorageBits: storage, Channels: a.channels, Layout: a.layout, Mask: a.mask, SourceChannels: (a.channels + 1) &^ 1}, nil
}

// Decode requires the four-byte LPCM prefix and its entire declared payload.
// It validates all bytes before publishing PCM or accepting a new format.
// Unused padded channels must contain zero; nonzero data is rejected explicitly.
func (d *Decoder) Decode(payload []byte) (Result, error) {
	if len(payload) < 4 {
		return Result{}, io.ErrUnexpectedEOF
	}
	if len(payload) > d.max {
		return Result{}, fmt.Errorf("LPCM payload exceeds %d-byte bound", d.max)
	}
	format, err := ParseHeader(payload)
	if err != nil {
		return Result{}, err
	}
	size := int(binary.BigEndian.Uint16(payload[:2]))
	if len(payload)-4 < size {
		return Result{}, io.ErrUnexpectedEOF
	}
	if len(payload)-4 != size {
		return Result{}, fmt.Errorf("LPCM declared %d audio bytes, received %d", size, len(payload)-4)
	}
	if size == 0 {
		return Result{}, fmt.Errorf("empty LPCM audio payload")
	}
	width := format.StorageBits / 8
	stride := width * format.SourceChannels
	if size%stride != 0 {
		return Result{}, fmt.Errorf("LPCM payload ends within a %d-byte sample frame: %w", stride, io.ErrUnexpectedEOF)
	}
	if d.haveFormat && d.format != format {
		return Result{}, fmt.Errorf("%w: %+v to %+v", ErrFormatChange, d.format, format)
	}
	frames := size / stride
	data := payload[4:]
	if format.SourceChannels != format.Channels {
		for i := 0; i < frames; i++ {
			for _, b := range data[i*stride+format.Channels*width : (i+1)*stride] {
				if b != 0 {
					return Result{}, fmt.Errorf("nonzero LPCM padding channel at sample frame %d", i)
				}
			}
		}
	}
	outputSize := frames * format.Channels * width
	if cap(d.buffer) < outputSize {
		d.buffer = make([]byte, outputSize)
	} else {
		d.buffer = d.buffer[:outputSize]
	}
	order := assignments[payload[2]>>4].order
	for i := 0; i < frames; i++ {
		for channel := 0; channel < format.Channels; channel++ {
			src := data[i*stride+order[channel]*width:]
			dst := d.buffer[(i*format.Channels+channel)*width:]
			for j := 0; j < width; j++ {
				dst[j] = src[width-1-j]
			}
		}
	}
	d.format = format
	d.haveFormat = true
	return Result{PCM: d.buffer, Format: format, Frames: frames, PaddingBytes: frames * (format.SourceChannels - format.Channels) * width}, nil
}
