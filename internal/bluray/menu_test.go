package bluray

import (
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func fixtureMenu(playlists []*Playlist, names []string) []byte {
	var b fixtureBytes
	text := func(s string) { b.u16(uint16(len(s))); b.str(s) }
	number := func(v float64) { b.u32(math.Float32bits(float32(v))) }
	text("Feature project")
	b.u32(1)
	text("Disc 1")
	text("88000000000000000000000000000001")
	b.u32(0) // sounds
	b.u32(uint32(len(playlists)))
	for i, p := range playlists {
		id, _ := strconv.Atoi(strings.TrimSuffix(p.Name, filepath.Ext(p.Name)))
		text(names[i])
		text("")
		b.u32(uint32(id))
		text("2D")
		number(24000.0 / 1001)
		number(p.Duration())
		b.u32(uint32(len(p.Items)))
		for j, item := range p.Items {
			text("")
			b.u32(uint32(j))
			number(float64(item.InTime) / 45000)
			number(float64(item.OutTime) / 45000)
			b.u32(0)
			b.u32(1) // angles
			b.u32(1)
			text("Video Stream 1")
			text("")
			for _, v := range []uint32{1, 0, 1920, 1080} {
				b.u32(v)
			}
			text("23.976")
			text("16:9")
			for k := 0; k < 4; k++ {
				b.u32(0)
			}
		}
		b.u32(uint32(len(p.Marks)))
		for j := range p.Marks {
			b.u32(uint32(j))
			text("entry-mark")
			number(0)
		}
	}
	for _, v := range []uint32{6, 10, 6} {
		b.u32(v)
	}
	return b
}

func writeFixtureMenu(t *testing.T, playlists []*Playlist, names []string) {
	t.Helper()
	path := filepath.Join(playlistBDMV(playlists[0].Path), "JAR", "03001", "bluray_project.bin")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, fixtureMenu(playlists, names), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestMenuProjectParsingAndBounds(t *testing.T) {
	makePlaylist := selectionFixture(t)
	playlists := []*Playlist{makePlaylist("00001.mpls", 100), makePlaylist("00125.mpls", 100)}
	data := fixtureMenu(playlists, []string{"FPL_MainFeature", "FPL_MainFeature_spa"})
	labels, err := parseMenuProject(data)
	if err != nil || len(labels) != 2 || labels[1].id != 125 || labels[1].name != "FPL_MainFeature_spa" || !menuMatches(playlists[1], labels[1]) {
		t.Fatalf("labels = %+v, %v", labels, err)
	}
	for length := 0; length < len(data); length++ {
		if length == len(data)-12 { // optional authoring-version triplet
			continue
		}
		if _, err := parseMenuProject(data[:length]); err == nil {
			t.Fatalf("accepted truncated project at %d", length)
		}
	}
	if _, err := parseMenuProject(append(data, 0)); err == nil {
		t.Fatal("accepted unrecognized trailing data")
	}
	labels[1].items[0].out++
	if menuMatches(playlists[1], labels[1]) {
		t.Fatal("trusted a stale menu timeline")
	}
	if menuTimeMatches(1000, math.MaxFloat32) {
		t.Fatal("overflowing precision tolerance accepted an unrelated duration")
	}
}

func TestMenuMasterTimelineMatchesLocalClipClocks(t *testing.T) {
	makePlaylist := selectionFixture(t)
	p := makePlaylist("00001.mpls", 100, selectionItem("00001", 40), selectionItem("00002", 60))
	label := menuLabel{name: "FPL_MainFeature", id: 1, duration: 6000, items: []menuItem{{id: 0, in: 3600, out: 6000}, {id: 1, in: 6000, out: 9600}}}
	if !menuMatches(p, label) {
		t.Fatal("rejected matching master-tape timeline")
	}
	label.items[1].in += 10
	label.items[1].out += 10
	if menuMatches(p, label) {
		t.Fatal("trusted unrelated non-contiguous timecodes")
	}
}

func TestMenuSelectionDistinguishesLocalizedPicture(t *testing.T) {
	makePlaylist := selectionFixture(t)
	feature := makePlaylist("00001.mpls", 100)
	localized := makePlaylist("00125.mpls", 100)
	list := []*Playlist{localized, feature}
	writeFixtureMenu(t, list, []string{"FPL_MainFeature_spa", "FPL_MainFeature"})
	s, err := SelectMain(list)
	if err != nil || s.Ambiguous || s.Recommended != feature || len(s.Candidates) != 2 || len(s.MetadataWarnings) != 0 {
		t.Fatalf("selection = %+v, %v", s, err)
	}
	if s.Candidates[0].Label != "FPL_MainFeature_spa" || s.Candidates[1].Label != "FPL_MainFeature" {
		t.Fatal(s.Candidates)
	}
	writeFixtureMenu(t, list, []string{"FPL_MainFeature_DirectorsCut", "FPL_MainFeature"})
	s, err = SelectMain(list)
	if err != nil || !s.Ambiguous || s.Recommended != nil {
		t.Fatalf("different editions should remain ambiguous: %+v, %v", s, err)
	}
}

func TestMenuDefaultChoosesNamedDuplicate(t *testing.T) {
	makePlaylist := selectionFixture(t)
	duplicate := makePlaylist("00001.mpls", 100)
	feature := makePlaylist("00009.mpls", 100, duplicate.Items...)
	localized := makePlaylist("00125.mpls", 100)
	list := []*Playlist{duplicate, feature, localized}
	writeFixtureMenu(t, list, []string{"Other duplicate", "FPL_MainFeature", "FPL_MainFeature_spa"})
	s, err := SelectMain(list)
	if err != nil || s.Ambiguous || s.Recommended == nil || s.Recommended.Path != feature.Path {
		t.Fatalf("selection = %+v, %v", s, err)
	}
	if s.Candidates[0].OriginalIndex != 1 || len(s.Candidates[0].DuplicateIndices) != 1 || s.Candidates[0].DuplicateIndices[0] != 0 {
		t.Fatal(s.Candidates[0])
	}
}

func TestMenuModifiedUTF(t *testing.T) {
	r := menuReader{reader{data: []byte{0, 9, 'A', 0xc0, 0x80, 0xed, 0xa0, 0xbd, 0xed, 0xb8, 0x80}}}
	if got := r.text(); got != "A\x00😀" || r.err != nil {
		t.Fatalf("string = %q, %v", got, r.err)
	}
	for _, data := range [][]byte{{0, 1, 0}, {0, 1, 0xc0}, {0, 2, 0xc1, 0xbf}, {0, 3, 0xe0, 0x80, 0x80}} {
		r := menuReader{reader{data: data}}
		r.text()
		if r.err == nil {
			t.Fatalf("accepted malformed modified UTF: %x", data)
		}
	}
}
