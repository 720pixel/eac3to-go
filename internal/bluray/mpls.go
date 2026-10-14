// Package bluray reads Blu-ray MPLS playlists without a platform library.
//
// File-format fields were checked against VideoLAN's libbluray bdnav parsers:
// https://code.videolan.org/videolan/libbluray/-/tree/master/src/libbluray/bdnav
// This parser describes the main presentation and the additional streams and
// subpaths; it does not decrypt discs, execute menus, or render PiP/3D content.
package bluray

import (
	"encoding/binary"
	"fmt"
	"sort"
)

const TicksPerSecond = 45000

// Playlist contains the presentation described by one MPLS file. Items and
// marks keep their original order. Chapters are sorted into presentation order.
type Playlist struct {
	Path          string      `json:"path,omitempty"`
	Name          string      `json:"name,omitempty"`
	Version       string      `json:"version"`
	PlaybackType  uint8       `json:"playback_type"`
	PlaybackCount uint16      `json:"playback_count,omitempty"`
	Items         []PlayItem  `json:"items"`
	Marks         []Mark      `json:"marks"`
	Chapters      []Chapter   `json:"chapters"`
	SubPaths      []SubPath   `json:"subpaths,omitempty"`
	ExtSubPaths   []SubPath   `json:"extension_subpaths,omitempty"`
	Extensions    []Extension `json:"extensions,omitempty"`
}

type PlayItem struct {
	ClipID              string `json:"clip_id"`
	CodecID             string `json:"codec_id"`
	STCID               uint8  `json:"stc_id"`
	InTime              uint32 `json:"in_time"`
	OutTime             uint32 `json:"out_time"`
	ConnectionCondition uint8  `json:"connection_condition"`
	RandomAccess        bool   `json:"random_access"`
	StillMode           uint8  `json:"still_mode,omitempty"`
	StillTime           uint16 `json:"still_time,omitempty"`
	MultiAngle          bool   `json:"multi_angle"`
	DifferentAudio      bool   `json:"different_audio,omitempty"`
	SeamlessAngle       bool   `json:"seamless_angle,omitempty"`
	// Angles always includes the primary angle at index zero.
	Angles  []Angle  `json:"angles"`
	Streams []Stream `json:"streams"`
}

type Angle struct {
	ClipID  string `json:"clip_id"`
	CodecID string `json:"codec_id"`
	STCID   uint8  `json:"stc_id"`
}

// Stream is one STN entry. Kind identifies its role: video, audio, subtitle,
// pip_subtitle, interactive_graphics, secondary_audio, secondary_video, or
// dolby_vision. EntryType 1 refers to the main clip; 2-4 refer to subpaths.
// Unknown entry/coding types retain their raw bytes for inspection.
type Stream struct {
	Kind               string  `json:"kind"`
	PID                uint16  `json:"pid"`
	EntryType          uint8   `json:"entry_type"`
	SubPathID          uint8   `json:"subpath_id,omitempty"`
	SubClipID          uint8   `json:"subclip_id,omitempty"`
	CodingType         uint8   `json:"coding_type"`
	Language           string  `json:"language,omitempty"`
	Format             uint8   `json:"format,omitempty"`
	Rate               uint8   `json:"rate,omitempty"`
	DynamicRange       uint8   `json:"dynamic_range,omitempty"`
	ColorSpace         uint8   `json:"color_space,omitempty"`
	CRFlag             bool    `json:"cr_flag,omitempty"`
	HDRPlus            bool    `json:"hdr_plus,omitempty"`
	CharacterCode      uint8   `json:"character_code,omitempty"`
	PrimaryAudioRefs   []uint8 `json:"primary_audio_refs,omitempty"`
	SecondaryAudioRefs []uint8 `json:"secondary_audio_refs,omitempty"`
	PIPSubtitleRefs    []uint8 `json:"pip_subtitle_refs,omitempty"`
	RawEntry           []byte  `json:"raw_entry,omitempty"`
	RawAttributes      []byte  `json:"raw_attributes,omitempty"`
}

// Mark timestamps refer to a source clip; Chapter timestamps refer to the
// concatenated, trimmed presentation. MPLS stores no human chapter titles.
type Mark struct {
	Type     uint8  `json:"type"`
	PlayItem int    `json:"play_item"`
	Time     uint32 `json:"time"`
	EntryPID uint16 `json:"entry_pid"`
	Duration uint32 `json:"duration"`
}

type Chapter struct {
	Time      float64 `json:"time"`
	TimeTicks uint64  `json:"time_ticks"`
	Name      string  `json:"name"`
	PlayItem  int     `json:"play_item"`
	MarkIndex int     `json:"mark_index"`
}

type SubPath struct {
	Type   uint8         `json:"type"`
	Repeat bool          `json:"repeat"`
	Items  []SubPlayItem `json:"items"`
}

type SubPlayItem struct {
	Clips               []Angle `json:"clips"`
	ConnectionCondition uint8   `json:"connection_condition"`
	InTime              uint32  `json:"in_time"`
	OutTime             uint32  `json:"out_time"`
	// SyncPlayItem retains the raw reference; asynchronous subpaths do not use
	// it in the same way as synchronized subpaths.
	SyncPlayItem int    `json:"sync_play_item"`
	SyncTime     uint32 `json:"sync_time"`
}

// Extension preserves its identity and payload. Subpath extension 2.2 is also
// decoded into ExtSubPaths. Other extension semantics are not interpreted.
type Extension struct {
	ID1    uint16 `json:"id1"`
	ID2    uint16 `json:"id2"`
	Offset uint32 `json:"offset"`
	Length uint32 `json:"length"`
	Data   []byte `json:"-"`
}

func (p PlayItem) DurationTicks() uint64 {
	if p.OutTime < p.InTime {
		return 0
	}
	return uint64(p.OutTime - p.InTime)
}

func (p PlayItem) Duration() float64 { return float64(p.DurationTicks()) / TicksPerSecond }

func (p *Playlist) DurationTicks() uint64 {
	var result uint64
	for _, item := range p.Items {
		result += item.DurationTicks()
	}
	return result
}

func (p *Playlist) Duration() float64 { return float64(p.DurationTicks()) / TicksPerSecond }

// ListedPIDs returns the main-clip PIDs listed by this item's STN. Streams in
// the actual transport stream but absent from this set are hidden by the MPLS.
// Subpath PIDs must be compared with their own source clip, not the main clip.
func (p PlayItem) ListedPIDs() map[uint16]bool {
	out := make(map[uint16]bool)
	for _, stream := range p.Streams {
		if stream.EntryType == 1 {
			out[stream.PID] = true
		}
	}
	return out
}

// Slice keeps the half-open item interval [first,last), drops marks referring
// to removed items, and rebuilds chapter timestamps from the retained windows.
// Subpaths are refused because their sync references need separate editing.
func (p *Playlist) Slice(first, last int) (*Playlist, error) {
	if first < 0 || last > len(p.Items) || first >= last {
		return nil, fmt.Errorf("invalid play-item interval [%d,%d) for %d items", first, last, len(p.Items))
	}
	if len(p.SubPaths) != 0 || len(p.ExtSubPaths) != 0 {
		return nil, fmt.Errorf("trimming playlists with subpaths is not supported")
	}
	for _, ext := range p.Extensions {
		if ext.ID1 != 3 || ext.ID2 != 5 {
			return nil, fmt.Errorf("trimming playlist extension %d.%d is not supported", ext.ID1, ext.ID2)
		}
	}
	out := *p
	out.Items = append([]PlayItem(nil), p.Items[first:last]...)
	out.Marks = nil
	for _, mark := range p.Marks {
		if mark.PlayItem >= first && mark.PlayItem < last {
			mark.PlayItem -= first
			out.Marks = append(out.Marks, mark)
		}
	}
	if err := out.buildChapters(); err != nil {
		return nil, err
	}
	return &out, nil
}

// SelectAngle selects a zero-based angle for all multi-angle items. Single-angle
// items are retained. Failure is explicit if a multi-angle item lacks the angle.
func (p *Playlist) SelectAngle(index int) (*Playlist, error) {
	if index < 0 {
		return nil, fmt.Errorf("invalid angle %d", index)
	}
	maxAngles := 0
	for _, item := range p.Items {
		if len(item.Angles) > maxAngles {
			maxAngles = len(item.Angles)
		}
	}
	if index >= maxAngles {
		return nil, fmt.Errorf("playlist has %d angles, cannot select angle %d", maxAngles, index+1)
	}
	out := *p
	out.Items = append([]PlayItem(nil), p.Items...)
	for i := range out.Items {
		item := &out.Items[i]
		if !item.MultiAngle && index > 0 {
			continue
		}
		if index >= len(item.Angles) {
			return nil, fmt.Errorf("play item %d has %d angles, cannot select angle %d", i+1, len(item.Angles), index+1)
		}
		angle := item.Angles[index]
		item.ClipID, item.CodecID, item.STCID = angle.ClipID, angle.CodecID, angle.STCID
	}
	return &out, nil
}

// Parse validates MPLS section boundaries, item windows, and mark references.
// Known unsupported features remain visible in the returned inventory.
func Parse(data []byte) (*Playlist, error) {
	if len(data) < 40 || string(data[:4]) != "MPLS" {
		return nil, fmt.Errorf("invalid or truncated MPLS header")
	}
	p := &Playlist{Version: string(data[4:8])}
	switch p.Version {
	case "0100", "0200", "0240", "0300":
	default:
		return nil, fmt.Errorf("unsupported MPLS version %q", p.Version)
	}
	header := reader{data: data[8:20], base: 8}
	listOffset, markOffset, extensionOffset := header.u32(), header.u32(), header.u32()
	app, appEnd, err := section(data, 40, "application info")
	if err != nil {
		return nil, err
	}
	app.skip(1)
	p.PlaybackType = app.u8()
	p.PlaybackCount = app.u16()
	app.skip(10) // user-operation mask and flags
	if app.err != nil {
		return nil, fmt.Errorf("application info: %w", app.err)
	}
	if p.PlaybackType != 2 && p.PlaybackType != 3 {
		p.PlaybackCount = 0
	}
	if uint64(listOffset) < uint64(appEnd) {
		return nil, fmt.Errorf("playlist section overlaps header or application info")
	}
	list, listEnd, err := section(data, listOffset, "playlist")
	if err != nil {
		return nil, err
	}
	list.skip(2)
	itemCount, subCount := int(list.u16()), int(list.u16())
	if list.err != nil || itemCount > list.remaining()/2 || subCount > list.remaining()/4 {
		return nil, fmt.Errorf("invalid playlist item counts")
	}
	for i := 0; i < itemCount; i++ {
		item, err := parseItem(list)
		if err != nil {
			return nil, fmt.Errorf("play item %d: %w", i+1, err)
		}
		p.Items = append(p.Items, item)
	}
	for i := 0; i < subCount; i++ {
		sub, err := parseSubPath(list)
		if err != nil {
			return nil, fmt.Errorf("subpath %d: %w", i, err)
		}
		p.SubPaths = append(p.SubPaths, sub)
	}
	if uint64(markOffset) < uint64(listEnd) {
		return nil, fmt.Errorf("mark section overlaps playlist or header")
	}
	marks, markEnd, err := section(data, markOffset, "marks")
	if err != nil {
		return nil, err
	}
	markCount := int(marks.u16())
	if marks.err != nil || markCount > marks.remaining()/14 {
		return nil, fmt.Errorf("truncated MPLS mark table")
	}
	for i := 0; i < markCount; i++ {
		marks.skip(1)
		p.Marks = append(p.Marks, Mark{Type: marks.u8(), PlayItem: int(marks.u16()), Time: marks.u32(), EntryPID: marks.u16(), Duration: marks.u32()})
	}
	if err := p.buildChapters(); err != nil {
		return nil, err
	}
	if extensionOffset != 0 {
		if uint64(extensionOffset) < uint64(markEnd) {
			return nil, fmt.Errorf("extension section overlaps marks or playlist")
		}
		if err := parseExtensions(data, extensionOffset, p); err != nil {
			return nil, err
		}
	}
	return p, nil
}

func (p *Playlist) buildChapters() error {
	starts := make([]uint64, len(p.Items))
	var elapsed uint64
	for i, item := range p.Items {
		starts[i] = elapsed
		elapsed += item.DurationTicks()
	}
	p.Chapters = nil
	for i, mark := range p.Marks {
		if mark.PlayItem < 0 || mark.PlayItem >= len(p.Items) {
			return fmt.Errorf("mark %d references nonexistent play item %d", i+1, mark.PlayItem)
		}
		item := p.Items[mark.PlayItem]
		if mark.Time < item.InTime || mark.Time > item.OutTime {
			return fmt.Errorf("mark %d timestamp %d is outside play item %d window [%d,%d]", i+1, mark.Time, mark.PlayItem+1, item.InTime, item.OutTime)
		}
		if mark.Type != 1 { // Link marks are not chapters.
			continue
		}
		ticks := starts[mark.PlayItem] + uint64(mark.Time-item.InTime)
		// An entry mark at the very end is an end marker, not a playable chapter.
		if ticks == elapsed {
			continue
		}
		p.Chapters = append(p.Chapters, Chapter{Time: float64(ticks) / TicksPerSecond, TimeTicks: ticks, PlayItem: mark.PlayItem, MarkIndex: i})
	}
	sort.SliceStable(p.Chapters, func(i, j int) bool { return p.Chapters[i].TimeTicks < p.Chapters[j].TimeTicks })
	for i := range p.Chapters {
		p.Chapters[i].Name = fmt.Sprintf("Chapter %02d", i+1)
	}
	return nil
}

func parseItem(parent *reader) (PlayItem, error) {
	r := parent.block16()
	item := PlayItem{ClipID: r.str(5), CodecID: r.str(4)}
	flags := r.u16()
	item.MultiAngle = flags&0x10 != 0
	item.ConnectionCondition = uint8(flags & 0x0f)
	item.STCID, item.InTime, item.OutTime = r.u8(), r.u32(), r.u32()
	r.skip(8)
	item.RandomAccess = r.u8()&0x80 != 0
	item.StillMode, item.StillTime = r.u8(), r.u16()
	item.Angles = []Angle{{ClipID: item.ClipID, CodecID: item.CodecID, STCID: item.STCID}}
	if item.MultiAngle {
		count, flags := int(r.u8()), r.u8()
		if count == 0 {
			return item, fmt.Errorf("multi-angle item declares zero angles")
		}
		item.DifferentAudio, item.SeamlessAngle = flags&2 != 0, flags&1 != 0
		for i := 1; i < count; i++ {
			item.Angles = append(item.Angles, Angle{ClipID: r.str(5), CodecID: r.str(4), STCID: r.u8()})
		}
	}
	if r.err != nil {
		return item, r.err
	}
	for _, angle := range item.Angles {
		if err := validateClip(angle); err != nil {
			return item, err
		}
	}
	if item.OutTime < item.InTime {
		return item, fmt.Errorf("out time %d precedes in time %d", item.OutTime, item.InTime)
	}
	var err error
	item.Streams, err = parseStreams(r)
	return item, err
}

func parseStreams(parent *reader) ([]Stream, error) {
	r := parent.block16()
	r.skip(2)
	counts := make([]int, 8)
	for i := range counts {
		counts[i] = int(r.u8())
	}
	r.skip(4)
	if r.err != nil {
		return nil, fmt.Errorf("STN header: %w", r.err)
	}
	// PiP PG entries are adjacent to the ordinary PG entries, before IG.
	groups := []struct {
		kind  string
		count int
	}{
		{"video", counts[0]}, {"audio", counts[1]}, {"subtitle", counts[2]},
		{"pip_subtitle", counts[6]}, {"interactive_graphics", counts[3]},
		{"secondary_audio", counts[4]}, {"secondary_video", counts[5]}, {"dolby_vision", counts[7]},
	}
	var streams []Stream
	for _, group := range groups {
		for i := 0; i < group.count; i++ {
			stream, err := parseStream(r)
			if err != nil {
				return nil, fmt.Errorf("STN %s entry %d: %w", group.kind, i+1, err)
			}
			stream.Kind = group.kind
			switch group.kind {
			case "secondary_audio":
				stream.PrimaryAudioRefs = parseReferences(r)
			case "secondary_video":
				stream.SecondaryAudioRefs = parseReferences(r)
				stream.PIPSubtitleRefs = parseReferences(r)
			}
			if r.err != nil {
				return nil, fmt.Errorf("STN %s references: %w", group.kind, r.err)
			}
			streams = append(streams, stream)
		}
	}
	return streams, nil
}

func parseReferences(r *reader) []uint8 {
	count := int(r.u8())
	r.skip(1)
	refs := append([]uint8(nil), r.take(count)...)
	if count&1 != 0 {
		r.skip(1)
	}
	return refs
}

func parseStream(parent *reader) (Stream, error) {
	entry := parent.block8()
	stream := Stream{EntryType: entry.u8()}
	switch stream.EntryType {
	case 1:
		stream.PID = entry.u16()
	case 2:
		stream.SubPathID, stream.SubClipID, stream.PID = entry.u8(), entry.u8(), entry.u16()
	case 3, 4:
		stream.SubPathID, stream.PID = entry.u8(), entry.u16()
	default:
		stream.RawEntry = append([]byte(nil), entry.data...)
	}
	if entry.err != nil {
		return stream, entry.err
	}
	if stream.PID > 0x1fff {
		return stream, fmt.Errorf("invalid transport PID 0x%x", stream.PID)
	}
	attr := parent.block8()
	stream.CodingType = attr.u8()
	switch stream.CodingType {
	case 0x01, 0x02, 0xea, 0x1b, 0x24:
		formatRate := attr.u8()
		stream.Format, stream.Rate = formatRate>>4, formatRate&15
		if stream.CodingType == 0x24 {
			color := attr.u8()
			stream.DynamicRange, stream.ColorSpace = color>>4, color&15
			flags := attr.u8()
			stream.CRFlag, stream.HDRPlus = flags&0x80 != 0, flags&0x40 != 0
		}
	case 0x03, 0x04, 0x80, 0x81, 0x82, 0x83, 0x84, 0x85, 0x86, 0xa1, 0xa2:
		formatRate := attr.u8()
		stream.Format, stream.Rate = formatRate>>4, formatRate&15
		stream.Language = attr.str(3)
	case 0x90, 0x91:
		stream.Language = attr.str(3)
	case 0x92:
		stream.CharacterCode, stream.Language = attr.u8(), attr.str(3)
	default:
		stream.RawAttributes = append([]byte(nil), attr.data...)
	}
	if attr.err != nil {
		return stream, attr.err
	}
	return stream, nil
}

func parseSubPath(parent *reader) (SubPath, error) {
	r := parent.block32()
	r.skip(1)
	path := SubPath{Type: r.u8(), Repeat: r.u16()&1 != 0}
	r.skip(1)
	count := int(r.u8())
	if r.err != nil {
		return path, r.err
	}
	for i := 0; i < count; i++ {
		item := r.block16()
		clip := Angle{ClipID: item.str(5), CodecID: item.str(4)}
		flags := item.u32()
		clip.STCID = item.u8()
		sub := SubPlayItem{Clips: []Angle{clip}, ConnectionCondition: uint8((flags >> 1) & 15), InTime: item.u32(), OutTime: item.u32(), SyncPlayItem: int(item.u16()), SyncTime: item.u32()}
		if flags&1 != 0 {
			clipCount := int(item.u8())
			if clipCount == 0 {
				return path, fmt.Errorf("subplay item %d declares zero clips", i)
			}
			for j := 1; j < clipCount; j++ {
				sub.Clips = append(sub.Clips, Angle{ClipID: item.str(5), CodecID: item.str(4), STCID: item.u8()})
			}
		}
		if item.err != nil {
			return path, fmt.Errorf("subplay item %d: %w", i, item.err)
		}
		if sub.OutTime < sub.InTime {
			return path, fmt.Errorf("subplay item %d out time precedes in time", i)
		}
		for _, clip := range sub.Clips {
			if err := validateClip(clip); err != nil {
				return path, err
			}
		}
		path.Items = append(path.Items, sub)
	}
	return path, nil
}

func parseExtensions(data []byte, offset uint32, p *Playlist) error {
	r, end, err := section(data, offset, "extension")
	if err != nil {
		return err
	}
	r.skip(7) // data block start address and reserved bytes
	count := int(r.u8())
	if r.err != nil || count > r.remaining()/12 {
		return fmt.Errorf("truncated extension index")
	}
	dataStart := uint64(offset) + 12 + uint64(count)*12
	type payloadRange struct{ start, end uint64 }
	var ranges []payloadRange
	for i := 0; i < count; i++ {
		ext := Extension{ID1: r.u16(), ID2: r.u16(), Offset: r.u32(), Length: r.u32()}
		start := uint64(offset) + uint64(ext.Offset)
		finish := start + uint64(ext.Length)
		if start < dataStart || finish > uint64(end) {
			return fmt.Errorf("extension %d.%d points outside extension payload", ext.ID1, ext.ID2)
		}
		for _, previous := range ranges {
			if start < previous.end && previous.start < finish {
				return fmt.Errorf("extension %d.%d overlaps another extension payload", ext.ID1, ext.ID2)
			}
		}
		ranges = append(ranges, payloadRange{start, finish})
		ext.Data = append([]byte(nil), data[int(start):int(finish)]...)
		p.Extensions = append(p.Extensions, ext)
		if ext.ID1 == 2 && ext.ID2 == 2 {
			block := reader{data: ext.Data, base: int(start)}
			sub := block.block32()
			n := int(sub.u16())
			if sub.err != nil || n > sub.remaining()/4 {
				return fmt.Errorf("invalid subpath extension count")
			}
			for j := 0; j < n; j++ {
				path, err := parseSubPath(sub)
				if err != nil {
					return fmt.Errorf("extension subpath %d: %w", j, err)
				}
				p.ExtSubPaths = append(p.ExtSubPaths, path)
			}
		}
	}
	return nil
}

func validateClip(clip Angle) error {
	if len(clip.ClipID) != 5 {
		return fmt.Errorf("invalid clip identifier %q", clip.ClipID)
	}
	for _, ch := range clip.ClipID {
		if ch < '0' || ch > '9' {
			return fmt.Errorf("invalid clip identifier %q (expected five digits)", clip.ClipID)
		}
	}
	if clip.CodecID != "M2TS" && clip.CodecID != "FMTS" {
		return fmt.Errorf("unsupported clip codec identifier %q", clip.CodecID)
	}
	return nil
}

// reader uses sticky errors and bounded children, so a malformed nested length
// can never consume bytes from the following item or section.
type reader struct {
	data []byte
	pos  int
	base int
	err  error
}

func (r *reader) remaining() int { return len(r.data) - r.pos }

func (r *reader) take(n int) []byte {
	if r.err != nil {
		return nil
	}
	if n < 0 || n > r.remaining() {
		r.err = fmt.Errorf("truncated data at byte %d: need %d bytes, have %d", r.base+r.pos, n, r.remaining())
		return nil
	}
	out := r.data[r.pos : r.pos+n]
	r.pos += n
	return out
}

func (r *reader) skip(n int)       { r.take(n) }
func (r *reader) str(n int) string { return string(r.take(n)) }
func (r *reader) u8() uint8 {
	b := r.take(1)
	if len(b) == 0 {
		return 0
	}
	return b[0]
}
func (r *reader) u16() uint16 {
	b := r.take(2)
	if len(b) == 0 {
		return 0
	}
	return binary.BigEndian.Uint16(b)
}
func (r *reader) u32() uint32 {
	b := r.take(4)
	if len(b) == 0 {
		return 0
	}
	return binary.BigEndian.Uint32(b)
}
func (r *reader) child(n uint32) *reader {
	base := r.base + r.pos
	if uint64(n) > uint64(r.remaining()) && r.err == nil {
		r.err = fmt.Errorf("truncated block at byte %d: length %d exceeds %d available bytes", base, n, r.remaining())
	}
	if r.err != nil {
		return &reader{base: base, err: r.err}
	}
	return &reader{data: r.take(int(n)), base: base}
}
func (r *reader) block8() *reader  { return r.child(uint32(r.u8())) }
func (r *reader) block16() *reader { return r.child(uint32(r.u16())) }
func (r *reader) block32() *reader { return r.child(r.u32()) }

func section(data []byte, offset uint32, name string) (*reader, int, error) {
	if uint64(offset)+4 > uint64(len(data)) {
		return nil, 0, fmt.Errorf("%s section offset %d is outside file", name, offset)
	}
	r := reader{data: data[int(offset):], base: int(offset)}
	child := r.block32()
	if child.err != nil {
		return nil, 0, fmt.Errorf("%s section: %w", name, child.err)
	}
	return child, int(offset) + r.pos, nil
}
