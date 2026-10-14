package bluray

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

func TestFamilyPermutationsRemainAmbiguous(t *testing.T) {
	makePlaylist := selectionFixture(t)
	items := []PlayItem{selectionItem("00011", 30), selectionItem("00012", 30), selectionItem("00013", 30), selectionItem("00014", 30)}
	var playlists []*Playlist
	for i := 0; i < 4; i++ {
		order := append(append([]PlayItem(nil), items[i:]...), items[:i]...)
		playlists = append(playlists, makePlaylist(fmt.Sprintf("%05d.mpls", i+1), 120, order...))
	}
	selection, err := SelectMain(playlists)
	if err != nil || !selection.Ambiguous || selection.Recommended != nil || len(selection.Families) != 1 {
		t.Fatalf("selection = %+v, %v", selection, err)
	}
	family := selection.Families[0]
	if family.Kind != "clip_permutations" || family.DistinctOrders != 4 || !family.SuspectedObfuscation || selection.SuspectedObfuscationFamilies != 1 || len(family.Differences) == 0 {
		t.Fatal(family)
	}
	if family.Differences[0].LeftClip != "00011" || family.Differences[0].RightClip != "00012" || family.Differences[0].StartTicks != 0 || family.Differences[0].EndTicks != 30*60*45000 {
		t.Fatal(family.Differences[0])
	}
}

func TestPictureLanguageRequiresAuthoredEvidence(t *testing.T) {
	makePlaylist := selectionFixture(t)
	main := makePlaylist("00001.mpls", 100)
	spanish := makePlaylist("00125.mpls", 100)
	list := []*Playlist{main, spanish}
	writeFixtureMenu(t, list, []string{"FPL_MainFeature", "FPL_MainFeature_spa"})
	selection, err := SelectMainWithOptions(list, SelectionOptions{Language: "SPA"})
	if err != nil || selection.Recommended != spanish || !selection.LanguageMatched || selection.LanguageRequested != "spa" || len(selection.Families) != 1 || selection.Families[0].Kind != "localized_picture" {
		t.Fatalf("language selection = %+v, %v", selection, err)
	}
	selection, err = SelectMainWithOptions(list, SelectionOptions{Language: "eng"})
	if err != nil || selection.Recommended != main || selection.LanguageMatched {
		t.Fatalf("audio language was mistaken for picture language: %+v, %v", selection, err)
	}
	if !strings.Contains(strings.Join(selection.Reasons, " "), "No unambiguous authored eng") {
		t.Fatal("unmatched preference was silently ignored")
	}
	if _, err := SelectMainWithOptions(list, SelectionOptions{Language: "../"}); err == nil {
		t.Fatal("accepted malformed language preference")
	}
}

func TestWindowDifferencesAlignSegmentBoundariesAndTrims(t *testing.T) {
	a := &Playlist{Name: "a", Items: []PlayItem{{ClipID: "00001", CodecID: "M2TS", InTime: 100, OutTime: 200}, {ClipID: "00002", CodecID: "M2TS", InTime: 1000, OutTime: 1100}}}
	b := &Playlist{Name: "b", Items: []PlayItem{{ClipID: "00001", CodecID: "M2TS", InTime: 100, OutTime: 150}, {ClipID: "00001", CodecID: "M2TS", InTime: 150, OutTime: 200}, {ClipID: "00002", CodecID: "M2TS", InTime: 1010, OutTime: 1060}}}
	diffs, count := ComparePlaylistWindows(a, b, 10)
	if count != 2 || len(diffs) != 2 {
		t.Fatalf("differences = %+v/%d", diffs, count)
	}
	first, tail := diffs[0], diffs[1]
	if first.StartTicks != 100 || first.EndTicks != 150 || first.LeftInTicks != 1000 || first.RightInTicks != 1010 || first.LeftOutTicks != 1050 || first.RightOutTicks != 1060 {
		t.Fatal(first)
	}
	if tail.StartTicks != 150 || tail.EndTicks != 200 || tail.RightItem != -1 || tail.Kind != "duration" || tail.LeftInTicks != 1050 || tail.LeftOutTicks != 1100 {
		t.Fatal(tail)
	}
	if limited, total := ComparePlaylistWindows(a, b, 1); len(limited) != 1 || total != 2 {
		t.Fatalf("bounded difference report = %+v/%d", limited, total)
	}
}

func TestFamilyGroupingDoesNotUseSharedLogoAlone(t *testing.T) {
	makePlaylist := selectionFixture(t)
	logo := selectionItem("00099", 1)
	var playlists []*Playlist
	for i := 1; i <= 30; i++ {
		playlists = append(playlists, makePlaylist(fmt.Sprintf("%05d.mpls", i), 45, logo, selectionItem(fmt.Sprintf("%05d", i), 44)))
	}
	selection, err := SelectMain(playlists)
	if err != nil || len(selection.Families) != 0 || !selection.Ambiguous {
		t.Fatalf("shared logos grouped unrelated episodes: %+v, %v", selection, err)
	}
}

func BenchmarkPlaylistFamilies1000(b *testing.B) {
	var base []PlayItem
	for i := 0; i < 20; i++ {
		base = append(base, selectionItem(fmt.Sprintf("%05d", i), 6))
	}
	candidates := make([]Candidate, 1000)
	for i := range candidates {
		items := append([]PlayItem(nil), base...)
		random := rand.New(rand.NewSource(int64(i)))
		random.Shuffle(len(items), func(a, b int) { items[a], items[b] = items[b], items[a] })
		name := fmt.Sprintf("%05d.mpls", i)
		candidates[i] = Candidate{Valid: true, Playlist: &Playlist{Name: name, Path: "/disc/BDMV/PLAYLIST/" + name, Items: items}}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		selection := Selection{Candidates: append([]Candidate(nil), candidates...)}
		attachFamilies(&selection)
		if len(selection.Families) != 1 || selection.Families[0].DistinctOrders != 1000 {
			b.Fatal("permutation family was not identified")
		}
	}
}
