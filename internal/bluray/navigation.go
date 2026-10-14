package bluray

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

type IndexEntry struct {
	ObjectType   uint8  `json:"object_type"`
	PlaybackType uint8  `json:"playback_type"`
	AccessType   uint8  `json:"access_type"`
	ObjectID     uint16 `json:"object_id"`
	BDJOName     string `json:"bdjo_name,omitempty"`
}

type DiscIndex struct {
	Version       string       `json:"version"`
	FirstPlay     IndexEntry   `json:"first_play"`
	TopMenu       IndexEntry   `json:"top_menu"`
	Titles        []IndexEntry `json:"titles"`
	ExtensionData []byte       `json:"extension_data,omitempty"`
}

type NavigationCommand struct {
	Operands, Group, Subgroup uint8
	Branch, Compare, Set      uint8
	Immediate1, Immediate2    bool
	Destination, Source       uint32
	Reserved                  uint32
	Opcode                    uint32
}

type MovieObject struct {
	ResumeIntention, MenuCallMask, TitleSearchMask bool
	Commands                                       []NavigationCommand
}

type MovieObjects struct {
	Version       string
	Objects       []MovieObject
	ExtensionData []byte
}

func navigationHeader(data []byte, signature string) (string, error) {
	if len(data) < 40 || string(data[:4]) != signature {
		return "", fmt.Errorf("invalid %s header", signature)
	}
	version := string(data[4:8])
	switch version {
	case "0100", "0200", "0240", "0300":
		return version, nil
	default:
		return "", fmt.Errorf("unsupported %s version %q", signature, version)
	}
}

func ParseIndex(data []byte) (*DiscIndex, error) {
	version, err := navigationHeader(data, "INDX")
	if err != nil {
		return nil, err
	}
	start, extension := binary.BigEndian.Uint32(data[8:12]), binary.BigEndian.Uint32(data[12:16])
	app, appEnd, err := section(data, 40, "index application info")
	if err != nil {
		return nil, err
	}
	if app.remaining() < 34 || uint64(start) < uint64(appEnd) {
		return nil, fmt.Errorf("index table overlaps application info or has short application info")
	}
	r, end, err := section(data, start, "index table")
	if err != nil {
		return nil, err
	}
	index := &DiscIndex{Version: version}
	index.FirstPlay = parseIndexEntry(r, false)
	index.TopMenu = parseIndexEntry(r, false)
	count := int(r.u16())
	if count > r.remaining()/12 {
		return nil, fmt.Errorf("index title count exceeds table length")
	}
	for i := 0; i < count && r.err == nil; i++ {
		index.Titles = append(index.Titles, parseIndexEntry(r, true))
	}
	if r.err != nil {
		return nil, r.err
	}
	if r.remaining() != 0 {
		return nil, fmt.Errorf("index table has %d unrecognized trailing bytes", r.remaining())
	}
	index.ExtensionData, err = navigationExtension(data, extension, end)
	return index, err
}

func parseIndexEntry(r *reader, title bool) IndexEntry {
	flags := r.u32()
	entry := IndexEntry{ObjectType: uint8(flags >> 30)}
	if title {
		entry.AccessType = uint8(flags >> 28 & 3)
	}
	entry.PlaybackType = uint8(r.u16() >> 14)
	switch entry.ObjectType {
	case 1:
		entry.ObjectID = r.u16()
		r.skip(4)
		if entry.PlaybackType > 1 && r.err == nil {
			r.err = fmt.Errorf("invalid HDMV index playback type %d", entry.PlaybackType)
		}
	case 2:
		entry.BDJOName = r.str(5)
		r.skip(1)
		if entry.PlaybackType < 2 && r.err == nil {
			r.err = fmt.Errorf("invalid BD-J index playback type %d", entry.PlaybackType)
		}
		if !fiveDigits(entry.BDJOName) && r.err == nil {
			r.err = fmt.Errorf("invalid BDJO name %q", entry.BDJOName)
		}
	default:
		if r.err == nil {
			r.err = fmt.Errorf("unknown index object type %d", entry.ObjectType)
		}
	}
	return entry
}

func fiveDigits(s string) bool {
	if len(s) != 5 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func ParseMovieObjects(data []byte) (*MovieObjects, error) {
	version, err := navigationHeader(data, "MOBJ")
	if err != nil {
		return nil, err
	}
	r, end, err := section(data, 40, "movie objects")
	if err != nil {
		return nil, err
	}
	r.skip(4)
	count := int(r.u16())
	if count > r.remaining()/4 {
		return nil, fmt.Errorf("movie-object count exceeds section length")
	}
	objects := &MovieObjects{Version: version}
	for i := 0; i < count && r.err == nil; i++ {
		flags := r.u16()
		object := MovieObject{ResumeIntention: flags&0x8000 != 0, MenuCallMask: flags&0x4000 != 0, TitleSearchMask: flags&0x2000 != 0}
		commands := int(r.u16())
		if commands > r.remaining()/12 {
			return nil, fmt.Errorf("movie object %d command count exceeds section length", i)
		}
		for j := 0; j < commands; j++ {
			op := r.u32()
			object.Commands = append(object.Commands, NavigationCommand{
				Opcode: op, Operands: uint8(op >> 29), Group: uint8(op >> 27 & 3), Subgroup: uint8(op >> 24 & 7),
				Immediate1: op&0x800000 != 0, Immediate2: op&0x400000 != 0,
				Branch: uint8(op >> 16 & 15), Compare: uint8(op >> 8 & 15), Set: uint8(op & 31),
				Reserved: op & 0x0030f0e0, Destination: r.u32(), Source: r.u32(),
			})
		}
		objects.Objects = append(objects.Objects, object)
	}
	if r.err != nil {
		return nil, r.err
	}
	if r.remaining() != 0 {
		return nil, fmt.Errorf("movie objects have %d unrecognized trailing bytes", r.remaining())
	}
	objects.ExtensionData, err = navigationExtension(data, binary.BigEndian.Uint32(data[8:12]), end)
	return objects, err
}

func navigationExtension(data []byte, offset uint32, precedingEnd int) ([]byte, error) {
	if offset == 0 {
		return nil, nil
	}
	if uint64(offset) < uint64(precedingEnd) {
		return nil, fmt.Errorf("navigation extension overlaps preceding data")
	}
	r, _, err := section(data, offset, "navigation extension")
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), r.data...), nil
}

func readNavigationFile(path string) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 16<<20 {
		return nil, fmt.Errorf("navigation metadata is not a regular file within the 16 MiB limit: %s", path)
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, (16<<20)+1))
	if len(data) > 16<<20 {
		return nil, fmt.Errorf("navigation metadata exceeds 16 MiB: %s", path)
	}
	return data, err
}

// ReadNavigation accepts a disc root or BDMV directory. It reads metadata only;
// Java applications, interactive menus, and player registers are not executed.
func ReadNavigation(path string) (*NavigationReport, error) {
	bdmv := path
	if !strings.EqualFold(filepath.Base(filepath.Clean(path)), "BDMV") {
		var err error
		bdmv, err = childCaseInsensitive(path, "BDMV")
		if err != nil {
			return nil, err
		}
	}
	indexPath, err := childCaseInsensitive(bdmv, "index.bdmv")
	if err != nil {
		return nil, err
	}
	data, err := readNavigationFile(indexPath)
	if err != nil {
		return nil, err
	}
	index, err := ParseIndex(data)
	if err != nil {
		return nil, err
	}
	var objects *MovieObjects
	var problem error
	objectPath, err := childCaseInsensitive(bdmv, "MovieObject.bdmv")
	if err == nil {
		data, err = readNavigationFile(objectPath)
		if err == nil {
			objects, err = ParseMovieObjects(data)
		}
	}
	problem = err
	report := AnalyzeNavigation(index, objects)
	report.Path, _ = filepath.Abs(bdmv)
	if problem != nil {
		report.Warnings = append(report.Warnings, "MovieObject metadata unavailable: "+problem.Error())
	}
	return &report, nil
}
