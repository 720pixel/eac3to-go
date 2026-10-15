package app

import (
	"math"
	"strconv"
	"strings"
)

// Matroska elementary audio loses its container timestamps when demuxed. Keep
// the onset relative to the source video's onset as a separate mux offset.
// Audio-only Matroska retains its authored offset from timestamp zero.
func matroskaAudioOffset(s *Source, stream *Stream) (int64, bool) {
	if s.Playlist != nil || len(s.Paths) != 1 || !strings.Contains(s.Probe.Format.Name, "matroska") || stream == nil || stream.Type != "audio" {
		return 0, false
	}
	start, err := strconv.ParseFloat(stream.Start, 64)
	if err != nil || math.IsNaN(start) || math.IsInf(start, 0) {
		return 0, false
	}
	origin := 0.0
	for _, st := range s.Probe.Streams {
		if st.Type == "video" {
			origin, err = strconv.ParseFloat(st.Start, 64)
			if err != nil || math.IsNaN(origin) || math.IsInf(origin, 0) {
				return 0, false
			}
			break
		}
	}
	delay := math.Round((start - origin) * 1000)
	if delay < -86400000 || delay > 86400000 {
		return 0, false
	}
	return int64(delay), true
}
