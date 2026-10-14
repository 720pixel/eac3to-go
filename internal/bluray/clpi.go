package bluray

import (
	"encoding/binary"
	"fmt"
)

type ClipInfo struct {
	Version         string         `json:"version"`
	StreamType      uint8          `json:"stream_type"`
	ApplicationType uint8          `json:"application_type"`
	RecordingRate   uint32         `json:"recording_rate"`
	SourcePackets   uint32         `json:"source_packets"`
	FormatID        string         `json:"format_id"`
	Sequences       []ClipSequence `json:"sequences"`
	Programs        []ClipProgram  `json:"programs"`
	ATCDeltas       []ClipATCDelta `json:"atc_deltas,omitempty"`
	CPI             []byte         `json:"cpi,omitempty"`
	Marks           []byte         `json:"marks,omitempty"`
	Extensions      []byte         `json:"extensions,omitempty"`
}

type ClipSequence struct {
	ATCIndex    int    `json:"atc_index"`
	STCID       uint8  `json:"stc_id"`
	PCRPID      uint16 `json:"pcr_pid"`
	StartPacket uint32 `json:"start_packet"`
	InTime      uint32 `json:"in_time"`
	OutTime     uint32 `json:"out_time"`
	ClockWrap   bool   `json:"clock_wrap"`
}

type ClipProgram struct {
	StartPacket uint32       `json:"start_packet"`
	MapPID      uint16       `json:"map_pid"`
	Groups      uint8        `json:"groups"`
	Streams     []ClipStream `json:"streams"`
}

type ClipStream struct {
	PID        uint16 `json:"pid"`
	CodingType uint8  `json:"coding_type"`
	Format     uint8  `json:"format"`
	Rate       uint8  `json:"rate"`
	Language   string `json:"language,omitempty"`
	Attributes []byte `json:"attributes"`
}

type ClipATCDelta struct {
	Delta   uint32 `json:"delta"`
	ClipID  string `json:"clip_id"`
	CodecID string `json:"codec_id"`
}

// ParseClipInfo reads CLPI presentation clocks and program inventories. CPI,
// clip marks, and extensions are retained with section bounds checked; this
// parser does not infer random-access packet positions from their raw bytes.
func ParseClipInfo(data []byte) (*ClipInfo, error) {
	version, err := navigationHeader(data, "HDMV")
	if err != nil {
		return nil, err
	}
	offsets := []uint32{40}
	for i := 0; i < 5; i++ {
		offsets = append(offsets, binary.BigEndian.Uint32(data[8+4*i:12+4*i]))
	}
	if offsets[1] == 0 || offsets[2] == 0 {
		return nil, fmt.Errorf("CLPI sequence and program sections are required")
	}
	names := []string{"clip information", "clip sequences", "clip programs", "clip CPI", "clip marks", "clip extensions"}
	sections := make([]*reader, len(offsets))
	previousEnd := 40
	for i, offset := range offsets {
		if offset == 0 {
			continue
		}
		if uint64(offset) < uint64(previousEnd) {
			return nil, fmt.Errorf("CLPI %s section overlaps earlier data", names[i])
		}
		sectionReader, end, err := section(data, offset, names[i])
		if err != nil {
			return nil, err
		}
		sections[i], previousEnd = sectionReader, end
	}
	info := &ClipInfo{Version: version}
	r := sections[0]
	r.skip(2)
	info.StreamType, info.ApplicationType = r.u8(), r.u8()
	hasDelta := r.u32()&1 != 0
	info.RecordingRate, info.SourcePackets = r.u32(), r.u32()
	r.skip(128)
	typeInfo := r.block16()
	if typeInfo.remaining() > 0 {
		typeInfo.u8() // format validity
		info.FormatID = typeInfo.str(4)
	}
	if r.err == nil {
		r.err = typeInfo.err
	}
	if hasDelta {
		r.skip(1)
		count := int(r.u8())
		if count > r.remaining()/14 {
			return nil, fmt.Errorf("CLPI ATC delta count exceeds section")
		}
		for i := 0; i < count && r.err == nil; i++ {
			delta := ClipATCDelta{Delta: r.u32(), ClipID: r.str(5), CodecID: r.str(4)}
			r.skip(1)
			if !fiveDigits(delta.ClipID) {
				return nil, fmt.Errorf("invalid CLPI ATC clip identifier %q", delta.ClipID)
			}
			info.ATCDeltas = append(info.ATCDeltas, delta)
		}
	}
	if info.ApplicationType == 6 {
		r.skip(1)
		fonts := int(r.u8())
		if fonts > r.remaining()/6 {
			return nil, fmt.Errorf("CLPI font count exceeds section")
		}
		for i := 0; i < fonts && r.err == nil; i++ {
			if !fiveDigits(r.str(5)) {
				return nil, fmt.Errorf("invalid CLPI font identifier")
			}
			r.skip(1)
		}
	}
	if r.err != nil {
		return nil, r.err
	}
	r = sections[1]
	r.skip(1)
	for atc, count := 0, int(r.u8()); atc < count && r.err == nil; atc++ {
		start := r.u32()
		stcs, firstID := int(r.u8()), int(r.u8())
		if stcs > r.remaining()/14 || firstID+stcs > 256 || start > info.SourcePackets {
			return nil, fmt.Errorf("invalid CLPI ATC sequence %d", atc)
		}
		for i := 0; i < stcs; i++ {
			sequence := ClipSequence{ATCIndex: atc, STCID: uint8(firstID + i), PCRPID: r.u16(), StartPacket: r.u32(), InTime: r.u32(), OutTime: r.u32()}
			sequence.ClockWrap = sequence.OutTime < sequence.InTime
			if sequence.PCRPID > 0x1fff || sequence.StartPacket < start || sequence.StartPacket > info.SourcePackets {
				return nil, fmt.Errorf("invalid CLPI STC packet or PID reference")
			}
			info.Sequences = append(info.Sequences, sequence)
		}
	}
	if r.err != nil {
		return nil, r.err
	}
	if r.remaining() != 0 {
		return nil, fmt.Errorf("CLPI sequence section has trailing data")
	}
	r = sections[2]
	r.skip(1)
	for i, count := 0, int(r.u8()); i < count && r.err == nil; i++ {
		program := ClipProgram{StartPacket: r.u32(), MapPID: r.u16()}
		streams := int(r.u8())
		program.Groups = r.u8()
		if streams > r.remaining()/4 || program.MapPID > 0x1fff || program.StartPacket > info.SourcePackets {
			return nil, fmt.Errorf("invalid CLPI program %d", i)
		}
		for j := 0; j < streams && r.err == nil; j++ {
			pid := r.u16()
			attributes := r.block8()
			stream := ClipStream{PID: pid, Attributes: append([]byte(nil), attributes.data...)}
			stream.CodingType = attributes.u8()
			switch stream.CodingType {
			case 0x01, 0x02, 0xea, 0x1b, 0x20, 0x24:
				format := attributes.u8()
				stream.Format, stream.Rate = format>>4, format&15
				attributes.skip(3 + 12)
			case 0x03, 0x04, 0x80, 0x81, 0x82, 0x83, 0x84, 0x85, 0x86, 0xa1, 0xa2:
				format := attributes.u8()
				stream.Format, stream.Rate = format>>4, format&15
				stream.Language = attributes.str(3)
				attributes.skip(12)
			case 0x90, 0x91, 0xa0:
				stream.Language = attributes.str(3)
				attributes.skip(1 + 12)
			case 0x92:
				attributes.u8()
				stream.Language = attributes.str(3)
				attributes.skip(12)
			}
			if attributes.err != nil {
				return nil, fmt.Errorf("CLPI PID 0x%x attributes: %w", pid, attributes.err)
			}
			if pid > 0x1fff {
				return nil, fmt.Errorf("invalid CLPI stream PID 0x%x", pid)
			}
			program.Streams = append(program.Streams, stream)
		}
		info.Programs = append(info.Programs, program)
	}
	if r.err != nil {
		return nil, r.err
	}
	if r.remaining() != 0 {
		return nil, fmt.Errorf("CLPI program section has trailing data")
	}
	for i, destination := range []*[]byte{&info.CPI, &info.Marks, &info.Extensions} {
		if sections[i+3] != nil {
			*destination = append([]byte(nil), sections[i+3].data...)
		}
	}
	return info, nil
}

func ReadClipInfo(path string) (*ClipInfo, error) {
	data, err := readNavigationFile(path)
	if err != nil {
		return nil, err
	}
	info, err := ParseClipInfo(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return info, nil
}
