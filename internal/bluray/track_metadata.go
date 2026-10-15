package bluray

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
)

// TrackMetadata is an authored stream label matched to an MPLS stream number,
// language, PID and complete playlist timeline. It never uses channel counts.
type TrackMetadata struct {
	PID         uint16 `json:"pid"`
	Kind        string `json:"kind"`
	Label       string `json:"label"`
	Language    string `json:"language"`
	Commentary  bool   `json:"commentary,omitempty"`
	Descriptive bool   `json:"descriptive,omitempty"`
	SDH         bool   `json:"sdh,omitempty"`
	Forced      bool   `json:"forced,omitempty"`
	Source      string `json:"source"`
}

type PlaylistMetadata struct {
	Label    string          `json:"label,omitempty"`
	Source   string          `json:"source,omitempty"`
	Tracks   []TrackMetadata `json:"tracks,omitempty"`
	Warnings []string        `json:"warnings,omitempty"`
}

// ReadPlaylistMetadata checks the untrimmed playlist before attaching labels.
func ReadPlaylistMetadata(p *Playlist) PlaylistMetadata {
	labels, warnings := readMenuLabels(playlistBDMV(p.Path))
	result := PlaylistMetadata{Warnings: warnings}
	id, err := strconv.ParseUint(strings.TrimSuffix(p.Name, filepath.Ext(p.Name)), 10, 32)
	label, ok := labels[uint32(id)]
	if err != nil || !ok || !menuMatches(p, label) {
		return result
	}
	result.Label, result.Source = label.name, label.source
	tracks, err := matchMenuStreams(p, label)
	if err != nil {
		result.Warnings = append(result.Warnings, err.Error())
		return result
	}
	result.Tracks = tracks
	return result
}

func matchMenuStreams(p *Playlist, label menuLabel) ([]TrackMetadata, error) {
	byPID := map[uint16]TrackMetadata{}
	var order []uint16
	for i, item := range p.Items {
		if len(label.items[i].angles) == 0 {
			return nil, fmt.Errorf("Menu stream labels ignored: play item %d has no primary angle", i+1)
		}
		angle := label.items[i].angles[0]
		for _, group := range []struct {
			kind string
			menu []menuStream
		}{{"audio", angle.audio}, {"subtitle", angle.subtitles}} {
			var streams []Stream
			for _, stream := range item.Streams {
				if stream.Kind == group.kind {
					streams = append(streams, stream)
				}
			}
			if len(streams) != len(group.menu) {
				return nil, fmt.Errorf("Menu %s labels ignored: play item %d stream count disagrees with MPLS", group.kind, i+1)
			}
			for n, authored := range group.menu {
				stream := streams[n]
				if stream.EntryType != 1 || authored.number != uint32(n+1) || authored.language != stream.Language || authored.language2 != stream.Language {
					return nil, fmt.Errorf("Menu %s labels ignored: play item %d stream %d identity disagrees with MPLS", group.kind, i+1, n+1)
				}
				m := TrackMetadata{PID: stream.PID, Kind: stream.Kind, Label: authored.name, Language: stream.Language, Source: label.source}
				for _, token := range strings.FieldsFunc(strings.ToUpper(authored.name), func(r rune) bool { return r == '_' || r == ' ' || r == '-' }) {
					switch token {
					case "ACOM":
						m.Commentary = stream.Kind == "audio"
					case "SCOM":
						m.Commentary = stream.Kind == "subtitle"
					case "ADES":
						m.Descriptive = stream.Kind == "audio"
					case "SDH":
						m.SDH = stream.Kind == "subtitle"
					}
				}
				m.Forced = stream.Kind == "subtitle" && strings.HasSuffix(strings.ToUpper(authored.name), "_TXT_FOR")
				if prev, exists := byPID[m.PID]; exists {
					if !reflect.DeepEqual(prev, m) {
						return nil, fmt.Errorf("Menu stream labels ignored: PID 0x%x has inconsistent labels across play items", m.PID)
					}
				} else {
					byPID[m.PID] = m
					order = append(order, m.PID)
				}
			}
		}
	}
	var result []TrackMetadata
	for _, pid := range order {
		result = append(result, byPID[pid])
	}
	return result, nil
}
