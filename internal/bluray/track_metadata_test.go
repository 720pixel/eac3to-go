package bluray

import (
	"os"
	"path/filepath"
	"testing"
)

func metadataFixture(t *testing.T) (*Playlist, menuLabel) {
	t.Helper()
	p := selectionFixture(t)("00001.mpls", 100)
	p.Items[0].Streams = []Stream{
		{Kind: "audio", EntryType: 1, PID: 0x1100, Language: "eng"},
		{Kind: "audio", EntryType: 1, PID: 0x1101, Language: "eng"},
		{Kind: "subtitle", EntryType: 1, PID: 0x1200, Language: "eng"},
		{Kind: "subtitle", EntryType: 1, PID: 0x1201, Language: "spa"},
	}
	angle := menuAngle{audio: []menuStream{{name: "eng_ACOM", number: 1, language: "eng", language2: "eng"}, {name: "eng_ADES", number: 2, language: "eng", language2: "eng"}}, subtitles: []menuStream{{name: "eng_SCOM", number: 1, language: "eng", language2: "eng"}, {name: "spa_TXT_FOR", number: 2, language: "spa", language2: "spa"}}}
	return p, menuLabel{source: "JAR/00001/bluray_project.bin", items: []menuItem{{angles: []menuAngle{angle}}}}
}

func TestAuthoredTrackRolesAndIdentityValidation(t *testing.T) {
	p, label := metadataFixture(t)
	tracks, err := matchMenuStreams(p, label)
	if err != nil || len(tracks) != 4 || !tracks[0].Commentary || !tracks[1].Descriptive || !tracks[2].Commentary || !tracks[3].Forced || tracks[0].PID != 0x1100 {
		t.Fatalf("%+v %v", tracks, err)
	}
	for _, change := range []func(*Playlist, *menuLabel){
		func(p *Playlist, l *menuLabel) { p.Items[0].Streams[0].Language = "spa" },
		func(p *Playlist, l *menuLabel) { l.items[0].angles[0].audio[0].number = 2 },
		func(p *Playlist, l *menuLabel) { l.items[0].angles[0].audio = l.items[0].angles[0].audio[:1] },
		func(p *Playlist, l *menuLabel) { l.items[0].angles[0].audio[0].language2 = "spa" },
		func(p *Playlist, l *menuLabel) { p.Items[0].Streams[0].EntryType = 2 },
	} {
		p, l := metadataFixture(t)
		change(p, &l)
		if _, err = matchMenuStreams(p, l); err == nil {
			t.Fatal("trusted a mismatched stream identity")
		}
	}
}

func TestUnknownMenuNamesDoNotInventRoles(t *testing.T) {
	p, l := metadataFixture(t)
	l.items[0].angles[0].audio[0].name = "English Stereo"
	l.items[0].angles[0].subtitles[0].name = "PG Stream 1"
	tracks, err := matchMenuStreams(p, l)
	if err != nil || tracks[0].Commentary || tracks[2].SDH || tracks[2].Forced || tracks[2].Commentary {
		t.Fatalf("%+v %v", tracks, err)
	}
}

func TestMenuStreamParsingAndCompleteTimeline(t *testing.T) {
	p, _ := metadataFixture(t)
	data := fixtureMenu([]*Playlist{p}, []string{"MainFeature"}, map[uint16]string{0x1100: "eng_ACOM", 0x1101: "eng_ADES", 0x1200: "eng_SCOM", 0x1201: "spa_TXT_FOR"})
	path := filepath.Join(playlistBDMV(p.Path), "JAR", "00001", "bluray_project.bin")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	metadata := ReadPlaylistMetadata(p)
	if len(metadata.Tracks) != 4 || metadata.Label != "MainFeature" || !metadata.Tracks[0].Commentary || !metadata.Tracks[3].Forced {
		t.Fatalf("%+v", metadata)
	}
	p.Items[0].OutTime += 45000
	if m := ReadPlaylistMetadata(p); len(m.Tracks) > 0 || m.Label != "" {
		t.Fatal("trusted stale menu timeline")
	}
}
