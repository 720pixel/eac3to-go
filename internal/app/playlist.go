package app

import (
	"fmt"
	"strconv"

	"eac3to/internal/bluray"
)

func playlistChapters(p *bluray.Playlist) []Chapter {
	var chapters []Chapter
	// A skipped introduction may own the only zero mark. Keep the remaining
	// marks at their rebased times and add a chapter for the new beginning.
	if len(p.Chapters) > 0 && p.Chapters[0].TimeTicks > 0 {
		chapters = append(chapters, Chapter{Start: "0.000000000"})
	}
	for _, chapter := range p.Chapters {
		chapters = append(chapters, Chapter{Start: strconv.FormatFloat(chapter.Time, 'f', 9, 64)})
	}
	for i := range chapters {
		chapters[i].Tags = map[string]string{"title": fmt.Sprintf("Chapter %02d", i+1)}
	}
	return chapters
}

func selectPlaylistItems(p *bluray.Playlist, o Options) (*bluray.Playlist, float64, error) {
	first, last := 0, len(p.Items)
	if o.Collapse > 0 {
		last = min(last, o.Collapse)
	}
	if o.SkipFirst && last-first > 1 {
		first++
	}
	if o.SkipLast && last-first > 1 {
		last--
	}
	if first == 0 && last == len(p.Items) {
		return p, 0, nil
	}
	var seek float64
	for _, item := range p.Items[:first] {
		seek += item.Duration()
	}
	selected, err := p.Slice(first, last)
	return selected, seek, err
}
