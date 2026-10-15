package app

import (
	"eac3to/internal/bluray"
	"testing"
)

func TestTrackMetadataUsesPIDAndKind(t *testing.T) {
	metadata := bluray.PlaylistMetadata{Tracks: []bluray.TrackMetadata{{PID: 0x1100, Kind: "audio", Commentary: true, Label: "eng_ACOM", Source: "menu"}, {PID: 0x1200, Kind: "subtitle", Forced: true, Label: "eng_TXT_FOR", Source: "menu"}}}
	s := Stream{Type: "audio"}
	applyAuthoredTrackMetadata(&s, 0x1100, metadata)
	if s.Disposition["comment"] != 1 || s.Tags["title"] != "Commentary" || s.Tags["metadata_source"] != "menu" {
		t.Fatalf("%+v", s)
	}
	s = Stream{Type: "subtitle"}
	applyAuthoredTrackMetadata(&s, 0x1100, metadata)
	if len(s.Disposition) != 0 || len(s.Tags) != 0 {
		t.Fatal("trusted an audio label on a subtitle")
	}
	applyAuthoredTrackMetadata(&s, 0x1200, metadata)
	if s.Disposition["forced"] != 1 || s.Tags["title"] != "Forced" {
		t.Fatalf("%+v", s)
	}
}
