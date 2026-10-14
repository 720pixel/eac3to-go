package bluray

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type fixtureBytes []byte

func (b *fixtureBytes) u8(n uint8)   { *b = append(*b, n) }
func (b *fixtureBytes) u16(n uint16) { *b = binary.BigEndian.AppendUint16(*b, n) }
func (b *fixtureBytes) u32(n uint32) { *b = binary.BigEndian.AppendUint32(*b, n) }
func (b *fixtureBytes) zero(n int)   { *b = append(*b, make([]byte, n)...) }
func (b *fixtureBytes) str(s string) { *b = append(*b, s...) }
func block16(b []byte) []byte {
	return append(binary.BigEndian.AppendUint16(nil, uint16(len(b))), b...)
}
func block32(b []byte) []byte {
	return append(binary.BigEndian.AppendUint32(nil, uint32(len(b))), b...)
}

func fixtureSTN(rich bool) []byte {
	var b fixtureBytes
	b.zero(2)
	if rich {
		b = append(b, 1, 1, 1, 1, 1, 1, 1, 1)
	} else {
		b.zero(8)
	}
	b.zero(4)
	if rich {
		entry := func(streamType uint8, pid uint16, attr []byte) {
			var e fixtureBytes
			e.u8(streamType)
			if streamType != 1 {
				e.u8(0)
			}
			if streamType == 2 {
				e.u8(1)
			}
			e.u16(pid)
			b.u8(uint8(len(e)))
			b = append(b, e...)
			b.u8(uint8(len(attr)))
			b = append(b, attr...)
		}
		entry(1, 0x1011, []byte{0x1b, 0x63})
		entry(1, 0x1100, []byte{0x83, 0x31, 'e', 'n', 'g'})
		entry(1, 0x1200, []byte{0x90, 'j', 'p', 'n'})
		entry(2, 0x1201, []byte{0x90, 'e', 'n', 'g'})
		entry(3, 0x1400, []byte{0x91, 'e', 'n', 'g'})
		entry(2, 0x1a00, []byte{0xa1, 0x31, 'f', 'r', 'a'})
		b = append(b, 1, 0, 0, 0) // one primary audio ref and odd-count padding
		entry(4, 0x1b00, []byte{0x1b, 0x63})
		b = append(b, 2, 0, 0, 1, 1, 0, 0, 0) // two audio refs, one PiP PG ref
		entry(1, 0x1015, []byte{0x24, 0x83, 0x21, 0xc0})
	}
	return block16(b)
}

func fixtureItem(id string, in, out uint32, rich, multi bool) []byte {
	var b fixtureBytes
	b.str(id)
	b.str("M2TS")
	flags := uint16(5)
	if multi {
		flags |= 0x10
	}
	b.u16(flags)
	b.u8(3)
	b.u32(in)
	b.u32(out)
	b.zero(8)
	b.u8(0x80)
	b.u8(0)
	b.zero(2)
	if multi {
		b.u8(2)
		b.u8(3)
		b.str("00099")
		b.str("M2TS")
		b.u8(7)
	}
	b = append(b, fixtureSTN(rich)...)
	return block16(b)
}

func fixtureSubPath() []byte {
	var item fixtureBytes
	item.str("00100")
	item.str("M2TS")
	item.u32(11) // connection condition 5, multi-clip
	item.u8(2)
	item.u32(100)
	item.u32(200)
	item.u16(1)
	item.u32(9000)
	item.u8(2)
	item.str("00101")
	item.str("M2TS")
	item.u8(4)
	var b fixtureBytes
	b.u8(0)
	b.u8(8)
	b.u16(1)
	b.u8(0)
	b.u8(1)
	b = append(b, block16(item)...)
	return block32(b)
}

func fixturePlaylist(rich, multi, subpaths, extension bool) []byte {
	var list fixtureBytes
	list.zero(2)
	list.u16(2)
	if subpaths {
		list.u16(1)
	} else {
		list.u16(0)
	}
	list = append(list, fixtureItem("00001", 45000, 135000, rich, multi)...)
	list = append(list, fixtureItem("00002", 9000, 144000, false, false)...)
	if subpaths {
		list = append(list, fixtureSubPath()...)
	}
	var marks fixtureBytes
	marks.u16(5)
	for _, m := range []Mark{
		{Type: 1, PlayItem: 0, Time: 45000, EntryPID: 0x1011},
		{Type: 2, PlayItem: 0, Time: 90000, EntryPID: 0x1011},
		{Type: 1, PlayItem: 1, Time: 27000, EntryPID: 0x1011},
		{Type: 1, PlayItem: 0, Time: 67500, EntryPID: 0x1011},
		{Type: 1, PlayItem: 1, Time: 144000, EntryPID: 0x1011},
	} {
		marks.u8(0)
		marks.u8(m.Type)
		marks.u16(uint16(m.PlayItem))
		marks.u32(m.Time)
		marks.u16(m.EntryPID)
		marks.u32(m.Duration)
	}
	var app fixtureBytes
	app.u8(0)
	app.u8(1)
	app.zero(12)
	b := make([]byte, 40)
	copy(b, "MPLS0300")
	b = append(b, block32(app)...)
	binary.BigEndian.PutUint32(b[8:12], uint32(len(b)))
	b = append(b, block32(list)...)
	binary.BigEndian.PutUint32(b[12:16], uint32(len(b)))
	b = append(b, block32(marks)...)
	if extension {
		var payload fixtureBytes
		payload.u16(1)
		payload = append(payload, fixtureSubPath()...)
		payload = block32(payload)
		var ext fixtureBytes
		ext.u32(24) // data offset, relative to section including length
		ext.zero(3)
		ext.u8(1)
		ext.u16(2)
		ext.u16(2)
		ext.u32(24)
		ext.u32(uint32(len(payload)))
		ext = append(ext, payload...)
		binary.BigEndian.PutUint32(b[16:20], uint32(len(b)))
		b = append(b, block32(ext)...)
	}
	return b
}

func TestParseTimelineAndSlice(t *testing.T) {
	p, err := Parse(fixturePlaylist(false, false, false, false))
	if err != nil {
		t.Fatal(err)
	}
	if p.Duration() != 5 || p.DurationTicks() != 225000 {
		t.Fatalf("incorrect duration: %v", p.Duration())
	}
	if len(p.Marks) != 5 || len(p.Chapters) != 3 {
		t.Fatalf("marks/chapters = %d/%d", len(p.Marks), len(p.Chapters))
	}
	for i, want := range []float64{0, .5, 2.4} {
		if p.Chapters[i].Time != want {
			t.Fatalf("chapter %d = %v, want %v", i, p.Chapters[i].Time, want)
		}
	}
	if p.Chapters[2].Name != "Chapter 03" {
		t.Fatal(p.Chapters)
	}
	first, err := p.Slice(0, 1)
	if err != nil || first.Duration() != 2 || len(first.Chapters) != 2 {
		t.Fatalf("first slice: %+v %v", first, err)
	}
	last, err := p.Slice(1, 2)
	if err != nil {
		t.Fatal(err)
	}
	if last.Duration() != 3 || len(last.Chapters) != 1 || last.Chapters[0].Time != .4 || last.Chapters[0].PlayItem != 0 {
		t.Fatalf("recomputed slice: %+v", last)
	}
	if p.Marks[2].PlayItem != 1 || len(p.Items) != 2 {
		t.Fatal("Slice mutated the source playlist")
	}
	if _, err = p.Slice(1, 1); err == nil {
		t.Fatal("accepted empty slice")
	}
	if _, err = p.SelectAngle(1); err == nil {
		t.Fatal("accepted a second angle on a single-angle playlist")
	}
}

func TestParseSTNAnglesAndSubPaths(t *testing.T) {
	p, err := Parse(fixturePlaylist(true, true, true, true))
	if err != nil {
		t.Fatal(err)
	}
	i := p.Items[0]
	if !i.MultiAngle || !i.DifferentAudio || !i.SeamlessAngle || !i.RandomAccess || i.ConnectionCondition != 5 || i.STCID != 3 {
		t.Fatalf("incorrect item flags: %+v", i)
	}
	if len(i.Angles) != 2 || i.Angles[1].ClipID != "00099" || i.Angles[1].STCID != 7 {
		t.Fatal(i.Angles)
	}
	if len(i.Streams) != 8 {
		t.Fatalf("%d streams", len(i.Streams))
	}
	if i.Streams[1].Language != "eng" || i.Streams[1].CodingType != 0x83 || i.Streams[1].PID != 0x1100 {
		t.Fatal(i.Streams[1])
	}
	if i.Streams[3].Kind != "pip_subtitle" || i.Streams[3].SubClipID != 1 {
		t.Fatal(i.Streams[3])
	}
	if !reflect.DeepEqual(i.Streams[5].PrimaryAudioRefs, []uint8{0}) || !reflect.DeepEqual(i.Streams[6].SecondaryAudioRefs, []uint8{0, 1}) || !reflect.DeepEqual(i.Streams[6].PIPSubtitleRefs, []uint8{0}) {
		t.Fatal("secondary stream refs lost")
	}
	dv := i.Streams[7]
	if dv.Kind != "dolby_vision" || dv.DynamicRange != 2 || dv.ColorSpace != 1 || !dv.CRFlag || !dv.HDRPlus {
		t.Fatal(dv)
	}
	if !reflect.DeepEqual(i.ListedPIDs(), map[uint16]bool{0x1011: true, 0x1100: true, 0x1200: true, 0x1015: true}) {
		t.Fatal(i.ListedPIDs())
	}
	if len(p.SubPaths) != 1 || len(p.ExtSubPaths) != 1 || !reflect.DeepEqual(p.SubPaths, p.ExtSubPaths) {
		t.Fatal("subpath extension differs")
	}
	sub := p.SubPaths[0]
	if !sub.Repeat || sub.Type != 8 || len(sub.Items[0].Clips) != 2 || sub.Items[0].SyncPlayItem != 1 || sub.Items[0].SyncTime != 9000 {
		t.Fatal(sub)
	}
	angle, err := p.SelectAngle(1)
	if err != nil || angle.Items[0].ClipID != "00099" || angle.Items[1].ClipID != "00002" {
		t.Fatalf("angle selection: %v, %v", angle, err)
	}
	if p.Items[0].ClipID != "00001" {
		t.Fatal("SelectAngle mutated the source playlist")
	}
	if _, err := p.SelectAngle(2); err == nil {
		t.Fatal("accepted missing angle")
	}
	if _, err := p.Slice(0, 1); err == nil {
		t.Fatal("subpath trimming needs sync-reference handling")
	}
}

func TestRejectCorruptSectionsAndIdentifiers(t *testing.T) {
	valid := fixturePlaylist(true, true, true, true)
	for n := 0; n < len(valid); n++ {
		if _, err := Parse(valid[:n]); err == nil {
			t.Fatalf("accepted truncated file at %d/%d bytes", n, len(valid))
		}
	}
	mutations := []struct {
		name   string
		mutate func([]byte)
	}{
		{"signature", func(b []byte) { b[0] = 'X' }},
		{"version", func(b []byte) { copy(b[4:8], "9999") }},
		{"list offset", func(b []byte) { binary.BigEndian.PutUint32(b[8:12], 0xffffffff) }},
		{"list overlap", func(b []byte) { binary.BigEndian.PutUint32(b[8:12], 40) }},
		{"list length", func(b []byte) {
			off := binary.BigEndian.Uint32(b[8:12])
			binary.BigEndian.PutUint32(b[off:off+4], 0xffffffff)
		}},
		{"clip traversal", func(b []byte) { pos := bytes.Index(b, []byte("00001M2TS")); copy(b[pos:pos+5], "../x/") }},
		{"angle traversal", func(b []byte) { pos := bytes.Index(b, []byte("00099M2TS")); copy(b[pos:pos+5], "../x/") }},
		{"reversed window", func(b []byte) {
			pos := bytes.Index(b, []byte("00001M2TS"))
			binary.BigEndian.PutUint32(b[pos+16:pos+20], 0)
		}},
		{"mark item", func(b []byte) {
			off := binary.BigEndian.Uint32(b[12:16])
			binary.BigEndian.PutUint16(b[off+8:off+10], 100)
		}},
		{"mark before in", func(b []byte) {
			off := binary.BigEndian.Uint32(b[12:16])
			binary.BigEndian.PutUint32(b[off+10:off+14], 0)
		}},
		{"mark after out", func(b []byte) {
			off := binary.BigEndian.Uint32(b[12:16])
			binary.BigEndian.PutUint32(b[off+10:off+14], 0xffffffff)
		}},
		{"mark table length", func(b []byte) { off := binary.BigEndian.Uint32(b[12:16]); binary.BigEndian.PutUint32(b[off:off+4], 2) }},
		{"extension payload", func(b []byte) {
			off := binary.BigEndian.Uint32(b[16:20])
			binary.BigEndian.PutUint32(b[off+16:off+20], 0xffffffff)
		}},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			b := append([]byte(nil), valid...)
			tc.mutate(b)
			if _, err := Parse(b); err == nil {
				t.Fatal("accepted damaged input")
			}
		})
	}
}

func TestDiscoverAndResolve(t *testing.T) {
	root := t.TempDir()
	plDir := filepath.Join(root, "bdmv", "playlist")
	streamDir := filepath.Join(root, "bdmv", "stream")
	for _, dir := range []string{plDir, streamDir} {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(plDir, "00001.MPLS")
	if err := os.WriteFile(path, fixturePlaylist(false, true, false, false), 0644); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"00001.M2TS", "00002.mts", "00099.m2ts"} {
		if err := os.WriteFile(filepath.Join(streamDir, name), nil, 0644); err != nil {
			t.Fatal(err)
		}
	}
	for _, dir := range []string{root, filepath.Join(root, "bdmv"), plDir, path} {
		playlists, err := Discover(dir)
		if err != nil || len(playlists) != 1 {
			t.Fatalf("discover %s: %v %v", dir, playlists, err)
		}
		clips, err := ResolveClips(playlists[0])
		if err != nil || len(clips) != 2 || filepath.Base(clips[0]) != "00001.M2TS" || filepath.Base(clips[1]) != "00002.mts" {
			t.Fatalf("resolved %v %v", clips, err)
		}
		angle, err := playlists[0].SelectAngle(1)
		if err != nil {
			t.Fatal(err)
		}
		clips, err = angle.ResolveClips()
		if err != nil || filepath.Base(clips[0]) != "00099.m2ts" {
			t.Fatalf("angle paths: %v %v", clips, err)
		}
	}
	if err := os.WriteFile(filepath.Join(plDir, "00002.mpls"), []byte("broken"), 0644); err != nil {
		t.Fatal(err)
	}
	partial, err := Discover(root)
	if len(partial) != 1 || err == nil || !strings.Contains(err.Error(), "00002.mpls") {
		t.Fatalf("partial discovery: %v %v", partial, err)
	}
	if err := os.Remove(filepath.Join(streamDir, "00002.mts")); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveClips(partial[0]); err == nil {
		t.Fatal("accepted missing clip")
	}
	partial[0].Items[0].ClipID = "../xx"
	if _, err := ResolveClips(partial[0]); err == nil {
		t.Fatal("accepted unsafe constructed playlist")
	}
}

func TestDiscoverKeepsFileAndErrorOrder(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "PLAYLIST")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	var want []string
	var broken []string
	for i := 31; i >= 0; i-- {
		name := fmt.Sprintf("%05d.mpls", i)
		data := fixturePlaylist(false, i%2 == 0, false, false)
		if i%7 == 0 {
			data = []byte("broken")
			broken = append([]string{name}, broken...)
		} else {
			want = append([]string{name}, want...)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0644); err != nil {
			t.Fatal(err)
		}
	}
	playlists, err := Discover(dir)
	if err == nil {
		t.Fatal("expected corrupt-playlist errors")
	}
	var names []string
	for _, playlist := range playlists {
		names = append(names, playlist.Name)
	}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("order = %v, want %v", names, want)
	}
	position := -1
	for _, name := range broken {
		next := strings.Index(err.Error(), name)
		if next <= position {
			t.Fatalf("errors not in filename order: %v", err)
		}
		position = next
	}
}

func TestExtensionPayloadsCannotAlias(t *testing.T) {
	b := fixturePlaylist(false, false, false, true)
	offset := int(binary.BigEndian.Uint32(b[16:20]))
	payload := append([]byte(nil), b[offset+24:]...)
	var ext fixtureBytes
	ext.u32(36)
	ext.zero(3)
	ext.u8(2)
	for i := 0; i < 2; i++ {
		ext.u16(2)
		ext.u16(2)
		ext.u32(36)
		ext.u32(uint32(len(payload)))
	}
	ext = append(ext, payload...)
	b = append(b[:offset], block32(ext)...)
	if _, err := Parse(b); err == nil || !strings.Contains(err.Error(), "overlaps") {
		t.Fatalf("overlapping payload: %v", err)
	}
}

func FuzzParse(f *testing.F) {
	f.Add(fixturePlaylist(false, false, false, false))
	f.Add(fixturePlaylist(true, true, true, true))
	f.Add([]byte("MPLS0300"))
	f.Fuzz(func(t *testing.T, data []byte) {
		p, err := Parse(data)
		if err != nil {
			return
		}
		for _, item := range p.Items {
			if item.OutTime < item.InTime {
				t.Fatal("invalid accepted item")
			}
		}
		for _, chapter := range p.Chapters {
			if chapter.Time < 0 || chapter.Time >= p.Duration() {
				t.Fatal("invalid accepted chapter")
			}
		}
	})
}
