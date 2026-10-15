package app

import (
	"eac3to/internal/bluray"
	"strings"
)

func applyAuthoredTrackMetadata(stream *Stream, pid uint16, metadata bluray.PlaylistMetadata) {
	for _, m := range metadata.Tracks {
		if m.PID != pid || m.Kind != stream.Type {
			continue
		}
		if stream.Tags == nil {
			stream.Tags = map[string]string{}
		}
		if stream.Disposition == nil {
			stream.Disposition = map[string]int{}
		}
		stream.Tags["source_label"], stream.Tags["metadata_source"] = m.Label, m.Source
		var title []string
		if m.Commentary {
			stream.Disposition["comment"] = 1
			title = append(title, "Commentary")
		}
		if m.Descriptive {
			stream.Disposition["visual_impaired"] = 1
			title = append(title, "Audio Description")
		}
		if m.SDH {
			stream.Disposition["hearing_impaired"] = 1
			title = append(title, "SDH")
		}
		if m.Forced {
			stream.Disposition["forced"] = 1
			title = append(title, "Forced")
		}
		if len(title) > 0 && stream.Tags["title"] == "" {
			stream.Tags["title"] = strings.Join(title, ", ")
		}
	}
}
