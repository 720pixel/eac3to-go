package truehd

import (
	"encoding/binary"
	"fmt"

	"eac3to/internal/transport"
)

const MaxAccessUnitBytes = 0xfff * 2

// Format comes from a checked TrueHD major sync header. Samples is the number
// of samples per access unit, rather than a decoded block size or PES duration.
// Channel maps describe the coded presentations and do not imply PCM order.
type Format struct {
	SampleRate, Samples, Substreams      int
	ChannelMap1, ChannelMap2             uint16
	ExtendedSubstreamInfo, SubstreamInfo uint8
}

type Source struct {
	PTS, DTS, ArrivalTimestamp int64
	Segment                    int
	ByteOffset                 int64
	Discontinuity              bool
	// PayloadOffset identifies the access unit's first byte in its starting
	// PES. No timestamp is inferred for access units later in that PES.
	PayloadOffset int
}

// Frame bytes are borrowed until its callback returns. InputTiming is the
// codec's 16-bit sample clock; it can wrap or reset independently of PES PTS.
type Frame struct {
	Bytes                    []byte
	Format                   Format
	MajorSync, FormatChanged bool
	InputTiming              uint16
	Source                   Source
}

type ParserConfig struct {
	OnFrame       func(Frame) error
	MaxFrameBytes int
	// InitialFormat is only for callers that already validated an earlier
	// major header. It does not permit guessing the format of a partial start.
	InitialFormat       *Format
	RejectFormatChanges bool
}

type ParserStats struct {
	Frames, Bytes, MajorSyncs, FormatChanges int64
	Samples                                  int64
}

type Parser struct {
	cfg        ParserConfig
	buffer     []byte
	n, size    int
	source     Source
	format     Format
	haveFormat bool
	stats      ParserStats
	failure    error
	finished   bool
}

func NewParser(cfg ParserConfig) (*Parser, error) {
	if cfg.OnFrame == nil {
		return nil, fmt.Errorf("a TrueHD frame handler is required")
	}
	if cfg.MaxFrameBytes == 0 {
		cfg.MaxFrameBytes = MaxAccessUnitBytes
	}
	if cfg.MaxFrameBytes < 6 || cfg.MaxFrameBytes > MaxAccessUnitBytes {
		return nil, fmt.Errorf("TrueHD access unit limit must be between 6 and %d bytes", MaxAccessUnitBytes)
	}
	p := &Parser{cfg: cfg, buffer: make([]byte, cfg.MaxFrameBytes)}
	if cfg.InitialFormat != nil {
		if err := validateFormat(*cfg.InitialFormat); err != nil {
			return nil, err
		}
		p.format, p.haveFormat = *cfg.InitialFormat, true
		p.cfg.InitialFormat = nil
	}
	return p, nil
}

func (p *Parser) PendingBytes() int  { return p.n }
func (p *Parser) Stats() ParserStats { return p.stats }

// WritePES accepts only the TrueHD substream and preserves the first byte's
// source metadata when an access unit spans PES packets or physical clips.
func (p *Parser) WritePES(pes transport.PES) error {
	kind, err := Classify(pes)
	if err != nil {
		return p.stop(err)
	}
	if kind != TrueHD {
		return p.stop(fmt.Errorf("AC-3 companion sent to TrueHD frame parser"))
	}
	_, err = p.write(pes.Payload, Source{PTS: pes.PTS, DTS: pes.DTS, ArrivalTimestamp: pes.ArrivalTimestamp, Segment: pes.Segment, ByteOffset: pes.ByteOffset, Discontinuity: pes.Discontinuity})
	return err
}

// Write frames a raw TrueHD stream without assigning container timestamps.
func (p *Parser) Write(data []byte) (int, error) {
	return p.write(data, Source{PTS: transport.NoTimestamp, DTS: transport.NoTimestamp, ArrivalTimestamp: transport.NoTimestamp, Segment: -1, ByteOffset: -1})
}

func (p *Parser) write(data []byte, source Source) (int, error) {
	if p.failure != nil {
		return 0, p.failure
	}
	if p.finished {
		return 0, fmt.Errorf("TrueHD frame parser is finished")
	}
	consumed := 0
	for len(data) > 0 {
		if p.n == 0 {
			p.source = source
			p.source.PayloadOffset = consumed
		}
		want := 2
		if p.size != 0 {
			want = p.size
		}
		n := want - p.n
		if n > len(data) {
			n = len(data)
		}
		copy(p.buffer[p.n:p.n+n], data[:n])
		p.n += n
		consumed += n
		data = data[n:]
		if p.n < 2 {
			continue
		}
		if p.size == 0 {
			p.size = int(binary.BigEndian.Uint16(p.buffer[:2])&0xfff) * 2
			if p.size < 6 || p.size > len(p.buffer) {
				return consumed, p.stop(fmt.Errorf("invalid or oversized TrueHD access unit: %d bytes", p.size))
			}
		}
		if p.n < p.size {
			continue
		}
		frame, err := p.frame()
		if err != nil {
			return consumed, p.stop(err)
		}
		if err = p.cfg.OnFrame(frame); err != nil {
			return consumed, p.stop(err)
		}
		p.stats.Frames++
		p.stats.Bytes += int64(p.size)
		p.stats.Samples += int64(frame.Format.Samples)
		if frame.MajorSync {
			p.stats.MajorSyncs++
		}
		if frame.FormatChanged {
			p.stats.FormatChanges++
		}
		p.n, p.size = 0, 0
	}
	return consumed, nil
}

func (p *Parser) Finish() error {
	if p.failure != nil {
		return p.failure
	}
	if p.finished {
		return nil
	}
	p.finished = true
	if p.n != 0 {
		return p.stop(fmt.Errorf("truncated TrueHD access unit: %d of %d bytes", p.n, p.size))
	}
	return nil
}

func (p *Parser) stop(err error) error {
	if p.failure == nil {
		p.failure = err
	}
	return p.failure
}

func (p *Parser) frame() (Frame, error) {
	b := p.buffer[:p.size]
	frame := Frame{Bytes: b, InputTiming: binary.BigEndian.Uint16(b[2:4]), Source: p.source}
	headerEnd := 4
	if len(b) >= 8 && binary.BigEndian.Uint32(b[4:8]) == 0xf8726fba {
		format, err := parseMajor(b[4:])
		if err != nil {
			return frame, err
		}
		frame.MajorSync = true
		frame.FormatChanged = p.haveFormat && format != p.format
		if frame.FormatChanged && p.cfg.RejectFormatChanges {
			return frame, fmt.Errorf("TrueHD format changed at a major sync")
		}
		p.format, p.haveFormat = format, true
		headerEnd = 32
		if b[29]&1 != 0 {
			headerEnd += 2 + 2*int(b[30]>>4)
		}
	} else {
		if len(b) >= 8 && binary.BigEndian.Uint32(b[4:8]) == 0xf8726fbb {
			return frame, fmt.Errorf("MLP access units are not Blu-ray TrueHD")
		}
		if !p.haveFormat {
			return frame, fmt.Errorf("TrueHD stream starts without a validated major sync")
		}
		pos := 4
		parity := b[0] ^ b[1] ^ b[2] ^ b[3]
		for i := 0; i < p.format.Substreams; i++ {
			if pos+2 > len(b) {
				return frame, fmt.Errorf("truncated TrueHD substream header")
			}
			n := 2
			if b[pos]&0x80 != 0 {
				n = 4
			}
			if pos+n > len(b) {
				return frame, fmt.Errorf("truncated extended TrueHD substream header")
			}
			for _, v := range b[pos : pos+n] {
				parity ^= v
			}
			pos += n
		}
		if ((parity>>4)^parity)&15 != 15 {
			return frame, fmt.Errorf("TrueHD access unit header parity mismatch")
		}
	}
	// Check that each declared substream has its complete length header. The
	// entropy payload and its optional CRC belong to a decoder, not this framer.
	for i := 0; i < p.format.Substreams; i++ {
		if headerEnd+2 > len(b) {
			return frame, fmt.Errorf("truncated TrueHD substream header")
		}
		n := 2
		if b[headerEnd]&0x80 != 0 {
			n = 4
		}
		if headerEnd+n > len(b) {
			return frame, fmt.Errorf("truncated extended TrueHD substream header")
		}
		headerEnd += n
	}
	frame.Format = p.format
	return frame, nil
}

// The major header uses a non-reflected 0x002d CRC, byte swapped before XOR
// with the penultimate word. References: FFmpeg mlp_parse.c and mlp.c.
func parseMajor(b []byte) (Format, error) {
	var f Format
	size, err := majorHeaderSize(b)
	if err != nil {
		return f, err
	}
	if majorChecksum(b[:size]) != binary.LittleEndian.Uint16(b[size-2:size]) {
		return f, fmt.Errorf("TrueHD major sync checksum mismatch")
	}
	rateCode := b[4] >> 4
	if rateCode&7 > 2 || rateCode == 15 {
		return f, fmt.Errorf("unsupported TrueHD sample rate code %d", rateCode)
	}
	baseRate := 48000
	if rateCode&8 != 0 {
		baseRate = 44100
	}
	f.SampleRate = baseRate << uint(rateCode&7)
	f.Samples = 40 << uint(rateCode&7)
	f.ChannelMap1 = uint16(b[5]&15)<<1 | uint16(b[6]>>7)
	f.ChannelMap2 = uint16(b[6]&31)<<8 | uint16(b[7])
	f.Substreams = int(b[16] >> 4)
	f.ExtendedSubstreamInfo = b[16] & 3
	f.SubstreamInfo = b[17]
	if err := validateFormat(f); err != nil {
		return f, err
	}
	return f, nil
}

func majorHeaderSize(b []byte) (int, error) {
	if len(b) < 28 {
		return 0, fmt.Errorf("truncated TrueHD major sync header")
	}
	size := 28
	if b[25]&1 != 0 {
		size += 2 + 2*int(b[26]>>4)
	}
	if len(b) < size {
		return 0, fmt.Errorf("truncated TrueHD extended major sync header")
	}
	return size, nil
}

func majorChecksum(b []byte) uint16 {
	var crc uint16
	for _, v := range b[:len(b)-4] {
		crc ^= uint16(v) << 8
		for i := 0; i < 8; i++ {
			if crc&0x8000 != 0 {
				crc = (crc << 1) ^ 0x002d
			} else {
				crc <<= 1
			}
		}
	}
	checksum := (crc >> 8) | (crc << 8)
	return checksum ^ binary.LittleEndian.Uint16(b[len(b)-4:len(b)-2])
}

func validateFormat(f Format) error {
	wantSamples := 0
	switch f.SampleRate {
	case 44100, 48000:
		wantSamples = 40
	case 88200, 96000:
		wantSamples = 80
	case 176400, 192000:
		wantSamples = 160
	}
	if wantSamples == 0 || f.Samples != wantSamples || f.Substreams < 1 || f.Substreams > 15 || f.ChannelMap1 > 31 || f.ChannelMap2 > 8191 {
		return fmt.Errorf("invalid TrueHD frame format: %+v", f)
	}
	return nil
}

// ClockDifference compares codec clocks across one access-unit transition.
// The signed result uses the nearest 16-bit cycle and does not authorize edits.
func ClockDifference(previous Frame, next Frame) int {
	expected := previous.InputTiming + uint16(previous.Format.Samples)
	return int(int16(next.InputTiming - expected))
}
