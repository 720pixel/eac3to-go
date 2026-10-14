package bluray

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func selectionFixture(t *testing.T) func(string, int, ...PlayItem) *Playlist {
	t.Helper()
	dir := t.TempDir()
	playlistDir, streamDir := filepath.Join(dir, "BDMV", "PLAYLIST"), filepath.Join(dir, "BDMV", "STREAM")
	for _, path := range []string{playlistDir, streamDir} {
		if err := os.MkdirAll(path, 0755); err != nil {
			t.Fatal(err)
		}
	}
	return func(name string, minutes int, items ...PlayItem) *Playlist {
		if len(items) == 0 {
			items = []PlayItem{selectionItem(strings.TrimSuffix(name, ".mpls"), minutes)}
		}
		for _, item := range items {
			if err := os.WriteFile(filepath.Join(streamDir, item.ClipID+".m2ts"), nil, 0644); err != nil {
				t.Fatal(err)
			}
		}
		return &Playlist{Name: name, Path: filepath.Join(playlistDir, name), Version: "0300", PlaybackType: 1, Items: append([]PlayItem(nil), items...)}
	}
}

func selectionItem(id string, minutes int) PlayItem {
	return PlayItem{ClipID: id, CodecID: "M2TS", InTime: 45000, OutTime: 45000 + uint32(minutes)*60*45000, Angles: []Angle{{ClipID: id, CodecID: "M2TS"}}, Streams: []Stream{{Kind: "video", EntryType: 1, PID: 0x1011, CodingType: 0x1b}, {Kind: "audio", EntryType: 1, PID: 0x1100, CodingType: 0x83, Language: "eng"}}}
}

func TestSelectMainGroupsDuplicatesAndKeepsOriginalIndex(t *testing.T) {
	makePlaylist := selectionFixture(t)
	bonus := makePlaylist("00001.mpls", 15)
	feature := makePlaylist("00002.mpls", 120)
	duplicate := makePlaylist("00003.mpls", 120, feature.Items...)
	s, err := SelectMain([]*Playlist{bonus, feature, duplicate})
	if err != nil {
		t.Fatal(err)
	}
	if s.Ambiguous || s.Recommended != feature || s.Confidence != "high" || len(s.Candidates) != 2 {
		t.Fatalf("selection: %+v", s)
	}
	c := s.Candidates[0]
	if c.OriginalIndex != 1 || len(c.DuplicateNames) != 1 || c.DuplicateNames[0] != "00003.mpls" || c.DuplicateIndices[0] != 2 {
		t.Fatal(c)
	}
}

func TestSelectMainRejectsMissingLongerAndBonusLoop(t *testing.T) {
	makePlaylist := selectionFixture(t)
	missing := makePlaylist("00001.mpls", 180)
	feature := makePlaylist("00002.mpls", 100)
	loopItem := selectionItem("00003", 30)
	loop := makePlaylist("00003.mpls", 240, loopItem, loopItem, loopItem, loopItem, loopItem, loopItem, loopItem, loopItem)
	if err := os.Remove(filepath.Join(filepath.Dir(filepath.Dir(missing.Path)), "STREAM", "00001.m2ts")); err != nil {
		t.Fatal(err)
	}
	s, err := SelectMain([]*Playlist{missing, loop, feature})
	if err != nil {
		t.Fatal(err)
	}
	if s.Ambiguous || s.Recommended != feature || s.Candidates[0].OriginalIndex != 2 {
		t.Fatalf("selection: %+v", s)
	}
	if s.Candidates[2].Valid || len(s.Candidates[2].MissingClips) != 1 {
		t.Fatalf("missing clip did not invalidate: %+v", s.Candidates[2])
	}
	if s.Candidates[1].repeatFraction < .8 {
		t.Fatal("loop not recognized")
	}
}

func TestSelectMainLeavesCutsEpisodesAndLanguagesAmbiguous(t *testing.T) {
	for _, scenario := range []string{"cuts", "episodes", "languages"} {
		t.Run(scenario, func(t *testing.T) {
			makePlaylist := selectionFixture(t)
			first := makePlaylist("00001.mpls", 120)
			second := makePlaylist("00002.mpls", 126)
			list := []*Playlist{first, second}
			if scenario == "episodes" {
				list = []*Playlist{makePlaylist("00011.mpls", 44), makePlaylist("00012.mpls", 45), makePlaylist("00013.mpls", 46)}
			}
			if scenario == "languages" {
				second.Items = append([]PlayItem(nil), first.Items...)
				second.Items[0].Streams = append([]Stream(nil), first.Items[0].Streams...)
				second.Items[0].Streams[1].Language = "jpn"
			}
			s, err := SelectMain(list)
			if err != nil {
				t.Fatal(err)
			}
			if !s.Ambiguous || s.Recommended != nil || len(s.Candidates) != len(list) {
				t.Fatalf("ambiguous %s: %+v", scenario, s)
			}
		})
	}
}

func TestSelectMainManyBranchesAndDisjointWindows(t *testing.T) {
	makePlaylist := selectionFixture(t)
	var items []PlayItem
	for i := 0; i < 120; i++ {
		items = append(items, selectionItem(fmt.Sprintf("%05d", i+100), 1))
	}
	feature := makePlaylist("00001.mpls", 120, items...)
	bonus := makePlaylist("00002.mpls", 10)
	s, err := SelectMain([]*Playlist{bonus, feature})
	if err != nil || s.Ambiguous || s.Recommended != feature {
		t.Fatalf("branching: %+v %v", s, err)
	}
	for _, reason := range s.Candidates[0].Reasons {
		if strings.Contains(reason, "repeat") || strings.Contains(reason, "shorter") {
			t.Fatal(reason)
		}
	}
	a := selectionItem("01000", 50)
	b := a
	b.InTime = a.OutTime
	b.OutTime = b.InTime + 50*60*45000
	split := makePlaylist("00003.mpls", 100, a, b)
	if fraction := repeatedFraction(split); fraction != 0 {
		t.Fatalf("disjoint windows counted as repetition: %v", fraction)
	}
}

func TestSelectMainEpisodeCompilationIsAmbiguous(t *testing.T) {
	makePlaylist := selectionFixture(t)
	var episodes []*Playlist
	var items []PlayItem
	for i := 0; i < 3; i++ {
		episode := makePlaylist(fmt.Sprintf("%05d.mpls", i+1), 45)
		episodes = append(episodes, episode)
		items = append(items, episode.Items...)
	}
	compilation := makePlaylist("00009.mpls", 135, items...)
	s, err := SelectMain(append([]*Playlist{compilation}, episodes...))
	if err != nil {
		t.Fatal(err)
	}
	if !s.Ambiguous || s.Recommended != nil || !strings.Contains(strings.Join(s.Reasons, " "), "combines") {
		t.Fatalf("compilation: %+v", s)
	}
}

func TestSelectMainPrefersAllTracksForSamePresentation(t *testing.T) {
	makePlaylist := selectionFixture(t)
	full := makePlaylist("00001.mpls", 100)
	restricted := makePlaylist("00002.mpls", 100, full.Items...)
	full.Items[0].Streams = append(append([]Stream(nil), full.Items[0].Streams...), Stream{Kind: "audio", EntryType: 1, PID: 0x1101, CodingType: 0x81, Language: "jpn"})
	s, err := SelectMain([]*Playlist{restricted, full})
	if err != nil {
		t.Fatal(err)
	}
	if s.Ambiguous || s.Recommended != full || len(s.Candidates) != 2 {
		t.Fatalf("track superset selection: %+v", s)
	}
	if !strings.Contains(strings.Join(s.Candidates[0].Reasons, " "), "additional tracks") {
		t.Fatal(s.Candidates[0].Reasons)
	}
}

func TestSharedWindowsDoesNotConfuseDisjointContent(t *testing.T) {
	a := selectionItem("00001", 40)
	b := a
	b.InTime = a.OutTime
	b.OutTime = b.InTime + 40*60*45000
	if fraction := sharedClipFraction(&Playlist{Items: []PlayItem{a}}, &Playlist{Items: []PlayItem{b}}); fraction != 0 {
		t.Fatalf("disjoint content overlap %v", fraction)
	}
}

func TestSelectMainInvalidInventoryAndWindows(t *testing.T) {
	makePlaylist := selectionFixture(t)
	p := makePlaylist("00001.mpls", 90)
	p.Items[0].OutTime = p.Items[0].InTime - 1
	s, err := SelectMain([]*Playlist{p})
	if err != nil {
		t.Fatal(err)
	}
	if !s.Ambiguous || s.Recommended != nil || s.Candidates[0].Valid {
		t.Fatal(s)
	}
	if _, err := SelectMain(nil); err == nil {
		t.Fatal("accepted empty inventory")
	}
}
