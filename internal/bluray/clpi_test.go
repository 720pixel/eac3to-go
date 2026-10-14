package bluray

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func fixtureClipInfo() []byte {
	var info, sequence, program fixtureBytes
	info.zero(2)
	info.u8(1)
	info.u8(1)
	info.u32(0)
	info.u32(6000000)
	info.u32(1000)
	info.zero(128)
	info.u16(5)
	info.u8(1)
	info.str("HDMV")
	sequence.u8(0)
	sequence.u8(1)
	sequence.u32(0)
	sequence.u8(2)
	sequence.u8(3)
	for _, s := range [][3]uint32{{0, 45000, 90000}, {500, 90000, 180000}} {
		sequence.u16(0x1001)
		sequence.u32(s[0])
		sequence.u32(s[1])
		sequence.u32(s[2])
	}
	program.u8(0)
	program.u8(1)
	program.u32(0)
	program.u16(0x100)
	program.u8(3)
	program.u8(1)
	for _, stream := range []struct {
		pid  uint16
		attr []byte
	}{{0x1011, []byte{0x1b, 0x63, 0x30, 0, 0}}, {0x1100, []byte{0x80, 0x31, 'e', 'n', 'g'}}, {0x1200, []byte{0x90, 'e', 'n', 'g', 0}}} {
		program.u16(stream.pid)
		program.u8(uint8(len(stream.attr) + 12))
		program = append(program, stream.attr...)
		program.zero(12)
	}
	data := make([]byte, 40)
	copy(data, "HDMV0200")
	data = append(data, block32(info)...)
	for i, section := range [][]byte{sequence, program, nil, nil} {
		binary.BigEndian.PutUint32(data[8+i*4:12+i*4], uint32(len(data)))
		data = append(data, block32(section)...)
	}
	return data
}

func TestClipInfoInventoryClocksAndValidation(t *testing.T) {
	info, err := ParseClipInfo(fixtureClipInfo())
	if err != nil {
		t.Fatal(err)
	}
	if info.SourcePackets != 1000 || info.FormatID != "HDMV" || len(info.Sequences) != 2 || info.Sequences[1].STCID != 4 || len(info.Programs) != 1 || len(info.Programs[0].Streams) != 3 || info.Programs[0].Streams[1].Language != "eng" {
		t.Fatal(info)
	}
	item := PlayItem{ClipID: "00001", CodecID: "M2TS", STCID: 4, InTime: 100000, OutTime: 170000, Streams: []Stream{{EntryType: 1, PID: 0x1011, CodingType: 0x1b}, {EntryType: 1, PID: 0x1100, CodingType: 0x80}}}
	if issues := ValidatePlayItem(item, info); len(issues) != 0 {
		t.Fatal(issues)
	}
	item.STCID = 3
	if issues := ValidatePlayItem(item, info); len(issues) != 1 || issues[0].Code != "outside_presentation" || issues[0].Level != "error" {
		t.Fatal(issues)
	}
	item.STCID = 5
	if issues := ValidatePlayItem(item, info); len(issues) != 1 || issues[0].Code != "missing_stc" {
		t.Fatal(issues)
	}
	item.STCID = 4
	item.Streams = append(item.Streams, Stream{EntryType: 1, PID: 0x1101, CodingType: 0x81})
	if issues := ValidatePlayItem(item, info); len(issues) != 1 || issues[0].Code != "missing_pid" || issues[0].Level != "warning" {
		t.Fatal(issues)
	}
	info.Sequences[1].ClockWrap = true
	if issues := ValidatePlayItem(item, info); len(issues) != 2 || issues[0].Code != "clock_wrap" || issues[0].Level != "warning" {
		t.Fatal(issues)
	}
}

func TestClipInfoRejectsTruncationAndOverlappingSections(t *testing.T) {
	data := fixtureClipInfo()
	for length := 0; length < len(data); length++ {
		if _, err := ParseClipInfo(data[:length]); err == nil {
			t.Fatalf("accepted CLPI truncation at %d", length)
		}
	}
	for _, mutate := range []func([]byte){
		func(b []byte) { binary.BigEndian.PutUint32(b[8:12], 40) },
		func(b []byte) { binary.BigEndian.PutUint32(b[16:20], 0xffffffff) },
		func(b []byte) { offset := binary.BigEndian.Uint32(b[8:12]); b[offset+10] = 255 },
		func(b []byte) { offset := binary.BigEndian.Uint32(b[12:16]); b[offset+12] = 255 },
		func(b []byte) { offset := binary.BigEndian.Uint32(b[12:16]); b[offset+16] = 1 },
	} {
		copyData := append([]byte(nil), data...)
		mutate(copyData)
		if _, err := ParseClipInfo(copyData); err == nil {
			t.Fatal("accepted corrupt CLPI")
		}
	}
}

func TestSelectionRejectsWindowContradictedByClipInfo(t *testing.T) {
	makePlaylist := selectionFixture(t)
	p := makePlaylist("00001.mpls", 100)
	dir := filepath.Join(playlistBDMV(p.Path), "CLIPINF")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "00001.clpi"), fixtureClipInfo(), 0644); err != nil {
		t.Fatal(err)
	}
	s, err := SelectMain([]*Playlist{p})
	if err != nil || s.Recommended != nil || s.Candidates[0].Valid || s.Candidates[0].ClipValidation.Status != "invalid" {
		t.Fatalf("contradicted window accepted: %+v, %v", s, err)
	}
}

func FuzzClipInfo(f *testing.F) {
	f.Add(fixtureClipInfo())
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			t.Skip()
		}
		ParseClipInfo(data)
	})
}
