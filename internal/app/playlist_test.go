package app

import (
	"path/filepath"
	"testing"

	"eac3to/internal/bluray"
)

func TestChapterProcessingOptionsAreNotIgnored(t *testing.T) {
	s := &Source{Tracks: []Track{{Number: 1, Kind: "chapters"}}}
	for _, flag := range []string{"+100ms", "-100ms", "-slowdown", "-changeTo24.000", "-down16", "+3dB", "-core", "-forced"} {
		for _, defaults := range []bool{false, true} {
			path := filepath.Join(t.TempDir(), "chapters.txt")
			args := []string{"source.mkv", "1:", path, flag}
			if defaults {
				args = []string{"source.mkv", flag, "1:", path}
			}
			o, err := ParseOptions(args)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := buildOutputs(s, o); err == nil {
				t.Fatalf("ignored chapter flag %s (defaults=%t)", flag, defaults)
			}
		}
	}
}

func TestSkippingLogoAndEndCardRebasesChapterMarks(t *testing.T) {
	p := &bluray.Playlist{Items: []bluray.PlayItem{
		{ClipID: "00001", InTime: 27000000, OutTime: 27450000},
		{ClipID: "00002", InTime: 27000000, OutTime: 31500000},
		{ClipID: "00003", InTime: 27000000, OutTime: 27225000},
	}, Marks: []bluray.Mark{
		{Type: 1, PlayItem: 0, Time: 27000000},
		{Type: 1, PlayItem: 1, Time: 27000000},
		{Type: 1, PlayItem: 1, Time: 29250000},
		{Type: 1, PlayItem: 2, Time: 27000000},
	}}
	selected, seek, err := selectPlaylistItems(p, Options{SkipFirst: true, SkipLast: true})
	if err != nil {
		t.Fatal(err)
	}
	if seek != 10 || selected.Duration() != 100 || len(selected.Items) != 1 || selected.Items[0].ClipID != "00002" {
		t.Fatalf("selection=%+v seek=%g", selected, seek)
	}
	if len(selected.Chapters) != 2 || selected.Chapters[0].Time != 0 || selected.Chapters[1].Time != 50 {
		t.Fatalf("chapters=%+v", selected.Chapters)
	}
	if len(p.Items) != 3 || len(p.Marks) != 4 {
		t.Fatal("original playlist was changed")
	}
}

func TestChapterExportAddsNewBeginningWithoutMovingRetainedMarks(t *testing.T) {
	p := &bluray.Playlist{Chapters: []bluray.Chapter{{Time: 171.547, TimeTicks: 7719615}, {Time: 420, TimeTicks: 18900000}}}
	chapters := playlistChapters(p)
	if len(chapters) != 3 || chapters[0].Start != "0.000000000" || chapters[1].Start != "171.547000000" || chapters[2].Start != "420.000000000" {
		t.Fatalf("chapters=%+v", chapters)
	}
	if chapters[1].Tags["title"] != "Chapter 02" || len(p.Chapters) != 2 {
		t.Fatal("chapter names or source marks changed")
	}
}

func TestBackupPlaylistUsesDiscRoot(t *testing.T) {
	for _, path := range []string{"/disc/BDMV/PLAYLIST/00000.mpls", "/disc/BDMV/BACKUP/PLAYLIST/00000.mpls", "/disc/bdmv/backup/playlist/00000.mpls"} {
		if root := playlistDiscRoot(path); root != filepath.FromSlash("/disc") {
			t.Fatalf("%s resolved to %s", path, root)
		}
	}
}
