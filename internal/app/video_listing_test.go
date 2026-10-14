package app

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestVideoListingUsesValidatedHeaderClock(t *testing.T) {
	tools := mediaTools(t)
	p, err := probe(context.Background(), tools, []string{"-i", videoContainerFixture()})
	if err != nil || len(p.Streams) != 1 {
		t.Fatalf("video fixture: %+v, %v", p, err)
	}
	st := &p.Streams[0]
	st.FrameRate = "50/1"
	s := &Source{Name: "short clip", Probe: p, Tracks: []Track{{Number: 1, Kind: "video", Stream: st}}}
	var text bytes.Buffer
	if err := printTracks(&text, s, false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text.String(), "25 fps (header clock)") || strings.Contains(text.String(), "50/1 fps") {
		t.Fatal(text.String())
	}
	text.Reset()
	if err := printTracks(&text, s, true); err != nil {
		t.Fatal(err)
	}
	var listing struct {
		Tracks []Track `json:"tracks"`
	}
	if err := json.Unmarshal(text.Bytes(), &listing); err != nil || len(listing.Tracks) != 1 || listing.Tracks[0].Stream.HeaderFPS != 25 || listing.Tracks[0].Stream.FrameRate != "50/1" {
		t.Fatalf("JSON discarded the original probe estimate or lacked the validated clock: %s, %v", text.String(), err)
	}
	if st.HeaderFPS != 0 || st.FrameRate != "50/1" {
		t.Fatal("listing changed source metadata")
	}
}
