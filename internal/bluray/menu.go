package bluray

import (
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Some Blu-print menu projects retain authored playlist names. The binary
// records use Java DataInputStream strings and big-endian numeric fields.
// Read the complete structure before trusting a label, then match its timeline
// to the MPLS. Names found by scanning arbitrary bytes are not sufficient.
type menuLabel struct {
	name, source string
	id           uint32
	duration     float32
	items        []menuItem
	marks        int
}

type menuItem struct {
	id      uint32
	in, out float32
}

type menuReader struct{ reader }

func (r *menuReader) count(minBytes int) int {
	n := r.u32()
	if r.err != nil {
		return 0
	}
	if n > 100000 || uint64(n)*uint64(minBytes) > uint64(r.remaining()) {
		r.err = fmt.Errorf("menu count %d exceeds remaining data at byte %d", n, r.pos)
		return 0
	}
	return int(n)
}

func (r *menuReader) text() string {
	b := r.take(int(r.u16()))
	units := make([]uint16, 0, len(b))
	for i := 0; i < len(b); {
		a := b[i]
		switch {
		case a > 0 && a < 0x80:
			units = append(units, uint16(a))
			i++
		case a&0xe0 == 0xc0 && i+1 < len(b) && b[i+1]&0xc0 == 0x80:
			v := uint16(a&0x1f)<<6 | uint16(b[i+1]&0x3f)
			if v < 0x80 && v != 0 {
				r.err = fmt.Errorf("overlong menu string at byte %d", r.pos-len(b)+i)
				return ""
			}
			units = append(units, v)
			i += 2
		case a&0xf0 == 0xe0 && i+2 < len(b) && b[i+1]&0xc0 == 0x80 && b[i+2]&0xc0 == 0x80:
			v := uint16(a&0xf)<<12 | uint16(b[i+1]&0x3f)<<6 | uint16(b[i+2]&0x3f)
			if v < 0x800 {
				r.err = fmt.Errorf("overlong menu string at byte %d", r.pos-len(b)+i)
				return ""
			}
			units = append(units, v)
			i += 3
		default:
			r.err = fmt.Errorf("invalid menu string at byte %d", r.pos-len(b)+i)
			return ""
		}
	}
	return string(utf16.Decode(units))
}

func (r *menuReader) number() float32 {
	v := math.Float32frombits(r.u32())
	if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
		r.err = fmt.Errorf("non-finite menu time at byte %d", r.pos)
	}
	return v
}

func (r *menuReader) fields(layout string) {
	for _, field := range layout {
		if field == 's' {
			r.text()
		} else {
			r.u32()
		}
	}
}

func parseMenuProject(data []byte) ([]menuLabel, error) {
	r := menuReader{reader{data: data}}
	if r.text() == "" && r.err == nil {
		r.err = fmt.Errorf("menu project name is empty")
	}
	if discs := r.count(12); discs != 1 && r.err == nil {
		return nil, fmt.Errorf("menu project has %d discs; disc identity is not available", discs)
	}
	r.fields("ss") // disc name and identifier
	for count := r.count(6); count > 0 && r.err == nil; count-- {
		r.fields("is") // sound number and name
	}
	var labels []menuLabel
	seen := make(map[uint32]bool)
	for count := r.count(26); count > 0 && r.err == nil; count-- {
		label := menuLabel{name: r.text()}
		r.text() // tag
		label.id = r.u32()
		r.text() // 2D/3D presentation
		rate := r.number()
		label.duration = r.number()
		if label.id > 99999 || seen[label.id] || rate <= 0 || rate > 240 || label.duration < 0 {
			r.err = fmt.Errorf("invalid or duplicate menu playlist %d", label.id)
		}
		seen[label.id] = true
		for items := r.count(22); items > 0 && r.err == nil; items-- {
			r.text() // tag
			item := menuItem{id: r.u32(), in: r.number(), out: r.number()}
			r.u32() // still mode
			if item.id != uint32(len(label.items)) || item.in < 0 || item.out < item.in {
				r.err = fmt.Errorf("invalid menu play item in playlist %d", label.id)
			}
			label.items = append(label.items, item)
			for angles := r.count(38); angles > 0 && r.err == nil; angles-- {
				r.u32() // primary-video field
				r.fields("ssiiiiss")
				for audio := r.count(12); audio > 0 && r.err == nil; audio-- {
					r.fields("ssiss")
				}
				for subs := r.count(18); subs > 0 && r.err == nil; subs-- {
					r.fields("ssisssss")
				}
				for video := r.count(18); video > 0 && r.err == nil; video-- {
					r.fields("ssiiis")
				}
				for audio := r.count(12); audio > 0 && r.err == nil; audio-- {
					r.fields("ssiss")
				}
			}
		}
		label.marks = r.count(10)
		for i := 0; i < label.marks && r.err == nil; i++ {
			r.fields("is")
			r.number()
		}
		labels = append(labels, label)
	}
	if r.err == nil && r.remaining() == 12 {
		r.fields("iii") // authoring-version triplet
	}
	if r.err == nil && r.remaining() != 0 {
		r.err = fmt.Errorf("menu project has %d unrecognized trailing bytes", r.remaining())
	}
	if r.err != nil {
		return nil, r.err
	}
	return labels, nil
}

func menuMatches(p *Playlist, label menuLabel) bool {
	id, err := strconv.ParseUint(strings.TrimSuffix(p.Name, filepath.Ext(p.Name)), 10, 32)
	if err != nil || uint32(id) != label.id || len(p.Items) != len(label.items) || len(p.Marks) != label.marks || !menuTimeMatches(p.Duration(), label.duration) {
		return false
	}
	localWindows, masterTimeline := true, true
	for i, item := range p.Items {
		v := label.items[i]
		if !menuTimeMatches(float64(item.InTime)/45000, v.in) || !menuTimeMatches(float64(item.OutTime)/45000, v.out) {
			localWindows = false
		}
		if i > 0 && !menuTimeMatches(float64(v.in), label.items[i-1].out) {
			masterTimeline = false
		}
		tolerance := 2*(menuULP(v.in)+menuULP(v.out)) + 2.0/45000
		if math.Abs(item.Duration()-(float64(v.out)-float64(v.in))) > tolerance {
			return false
		}
	}
	// Authoring projects may use a continuous master-tape timecode while each
	// authored transport clip starts its own clock. Both retain the same item
	// durations and order; a non-contiguous unrelated timeline is rejected.
	return localWindows || masterTimeline
}

func menuTimeMatches(seconds float64, value float32) bool {
	// Menu times have float32 precision; MPLS times are integer 45 kHz ticks.
	return math.Abs(seconds-float64(value)) <= 2*menuULP(value)+1.0/45000
}

func menuULP(value float32) float64 {
	next := math.Nextafter32(value, float32(math.Inf(1)))
	if math.IsInf(float64(next), 0) {
		next = math.Nextafter32(value, float32(math.Inf(-1)))
	}
	return math.Abs(float64(next - value))
}

func readMenuLabels(bdmv string) (map[uint32]menuLabel, []string) {
	labels := make(map[uint32]menuLabel)
	jarDir, err := childCaseInsensitive(bdmv, "JAR")
	if err != nil {
		return labels, nil
	}
	entries, err := os.ReadDir(jarDir)
	if err != nil {
		return labels, []string{fmt.Sprintf("Cannot read menu metadata: %v", err)}
	}
	var warnings []string
	conflicts := make(map[uint32]bool)
	remaining := int64(32 << 20)
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(jarDir, entry.Name(), "bluray_project.bin")
		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || !info.Mode().IsRegular() || info.Size() > 16<<20 || info.Size() > remaining {
			warnings = append(warnings, "Menu project is unreadable or exceeds the metadata limit: "+path)
			continue
		}
		remaining -= info.Size()
		file, err := os.Open(path)
		if err != nil {
			warnings = append(warnings, "Cannot open menu project: "+path)
			continue
		}
		data, readErr := io.ReadAll(io.LimitReader(file, (16<<20)+1))
		file.Close()
		if readErr != nil || len(data) > 16<<20 {
			warnings = append(warnings, "Cannot read bounded menu project: "+path)
			continue
		}
		parsed, err := parseMenuProject(data)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("Menu labels ignored in %s: %v", path, err))
			continue
		}
		for _, label := range parsed {
			label.source = filepath.Join("JAR", entry.Name(), "bluray_project.bin")
			if previous, exists := labels[label.id]; exists && previous.name != label.name {
				conflicts[label.id] = true
			}
			labels[label.id] = label
		}
	}
	for id := range conflicts {
		delete(labels, id)
	}
	if len(conflicts) != 0 {
		warnings = append(warnings, "Conflicting menu playlist labels were ignored.")
	}
	return labels, warnings
}

func attachMenuLabels(selection *Selection) {
	byDisc := make(map[string]map[uint32]menuLabel)
	for i := range selection.Candidates {
		candidate := &selection.Candidates[i]
		bdmv := playlistBDMV(candidate.Playlist.Path)
		labels, loaded := byDisc[bdmv]
		if !loaded {
			var warnings []string
			labels, warnings = readMenuLabels(bdmv)
			byDisc[bdmv] = labels
			selection.MetadataWarnings = append(selection.MetadataWarnings, warnings...)
		}
		var label menuLabel
		matched := false
		for member, name := range append([]string{candidate.Playlist.Name}, candidate.DuplicateNames...) {
			id, err := strconv.ParseUint(strings.TrimSuffix(name, filepath.Ext(name)), 10, 32)
			if err != nil {
				continue
			}
			possible, exists := labels[uint32(id)]
			playlist := *candidate.Playlist
			playlist.Name = name
			playlist.Path = filepath.Join(filepath.Dir(playlist.Path), name)
			if !exists || !menuMatches(&playlist, possible) || member != 0 && possible.name != "FPL_MainFeature" || matched && label.name == "FPL_MainFeature" {
				continue
			}
			label, matched = possible, true
			if member != 0 && possible.name == "FPL_MainFeature" {
				originalIndex := candidate.DuplicateIndices[member-1]
				candidate.DuplicateNames[member-1], candidate.DuplicateIndices[member-1] = candidate.Playlist.Name, candidate.OriginalIndex
				candidate.OriginalIndex = originalIndex
				candidate.Playlist = &playlist
			}
		}
		if !matched {
			continue
		}
		candidate.Label = label.name
		candidate.LabelSource = label.source
		candidate.PictureLanguage = authoredPictureLanguage(label.name)
		switch {
		case label.name == "FPL_MainFeature":
			candidate.Variant = "authored_default"
		case candidate.PictureLanguage != "":
			candidate.Variant = "localized_picture"
		case label.name == "FPL_MainFeature_Narrative":
			candidate.Variant = "narrative"
		default:
			candidate.Variant = "named_presentation"
		}
		candidate.Reasons = append(candidate.Reasons, fmt.Sprintf("menu label %q matches the playlist ID, duration, play-item durations, and mark count", label.name))
	}
}

func menuDefault(selection *Selection, valid int) *Candidate {
	var found *Candidate
	for i := 0; i < valid; i++ {
		candidate := &selection.Candidates[i]
		if candidate.suspect || !candidate.video || !candidate.audio || candidate.Playlist.Duration() < 15*60 || candidate.Label != "FPL_MainFeature" {
			continue
		}
		if found != nil {
			return nil
		}
		found = candidate
	}
	if found == nil {
		return nil
	}
	for i := 0; i < valid; i++ {
		other := &selection.Candidates[i]
		if other == found || other.suspect || !other.video || !other.audio || trackSuperset(found.Playlist, other.Playlist) {
			continue
		}
		ratio := other.Playlist.Duration() / found.Playlist.Duration()
		shared := sharedClipFraction(found.Playlist, other.Playlist)
		if !(ratio >= .8 && ratio <= 1.25 || ratio >= .5 && ratio <= 2 && shared >= .5) {
			continue
		}
		// Only known language suffixes resolve a picture-branch tie. Director's
		// cuts and other named editions still require an explicit choice.
		if authoredPictureLanguage(other.Label) == "" || other.LabelSource != found.LabelSource || playlistBDMV(other.Playlist.Path) != playlistBDMV(found.Playlist.Path) {
			return nil
		}
	}
	return found
}
