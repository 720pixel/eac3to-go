package bluray

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
)

type Selection struct {
	Candidates                   []Candidate        `json:"candidates"`
	Recommended                  *Playlist          `json:"recommended,omitempty"`
	Confidence                   string             `json:"confidence"`
	Ambiguous                    bool               `json:"ambiguous"`
	Reasons                      []string           `json:"reasons"`
	MetadataWarnings             []string           `json:"metadata_warnings,omitempty"`
	Navigation                   []NavigationReport `json:"navigation,omitempty"`
	Families                     []PlaylistFamily   `json:"families,omitempty"`
	LanguageRequested            string             `json:"language_requested,omitempty"`
	LanguageMatched              bool               `json:"language_matched"`
	SuspectedObfuscationFamilies int                `json:"suspected_obfuscation_families"`
}

type Candidate struct {
	Playlist         *Playlist            `json:"playlist"`
	OriginalIndex    int                  `json:"original_index"`
	DuplicateNames   []string             `json:"duplicate_names,omitempty"`
	DuplicateIndices []int                `json:"duplicate_indices,omitempty"`
	Score            float64              `json:"score"`
	Reasons          []string             `json:"reasons"`
	Valid            bool                 `json:"valid"`
	MissingClips     []string             `json:"missing_clips,omitempty"`
	Label            string               `json:"label,omitempty"`
	LabelSource      string               `json:"label_source,omitempty"`
	FamilyID         string               `json:"family_id,omitempty"`
	Variant          string               `json:"variant,omitempty"`
	PictureLanguage  string               `json:"picture_language,omitempty"`
	Navigation       []NavigationEvidence `json:"navigation,omitempty"`
	ClipValidation   ClipValidation       `json:"clip_validation"`
	video, audio     bool
	repeatFraction   float64
	suspect          bool
}

// SelectMain ranks structural evidence without decoding media. Exact duplicate
// manifests are grouped; differing track languages, marks, angles, subpaths, or
// extension data remain distinct. Similar episodes/cuts are left ambiguous.
// OriginalIndex refers to the caller's slice; duplicate names exclude the
// representative. A recommendation is a heuristic, not disc-navigation truth.
func selectMain(playlists []*Playlist) (*Selection, error) {
	if len(playlists) == 0 {
		return nil, fmt.Errorf("no playlists to rank")
	}
	selection := &Selection{Confidence: "low", Ambiguous: true}
	groups := make(map[[32]byte]int)
	inventories := make(map[string]*streamInventory)
	for index, p := range playlists {
		if p == nil {
			return nil, fmt.Errorf("playlist %d is nil", index)
		}
		key, err := presentationKey(p)
		if err != nil {
			return nil, err
		}
		// Identical manifests on different discs do not share clip availability.
		dir := playlistBDMV(p.Path)
		discKey := sha256.Sum256(append(key[:], []byte(dir)...))
		if existing, ok := groups[discKey]; ok {
			c := &selection.Candidates[existing]
			c.DuplicateNames = append(c.DuplicateNames, p.Name)
			c.DuplicateIndices = append(c.DuplicateIndices, index)
			continue
		}
		inventory := inventories[dir]
		if inventory == nil {
			inventory = readInventory(dir)
			inventories[dir] = inventory
		}
		candidate := rankCandidate(p, index, inventory)
		groups[discKey] = len(selection.Candidates)
		selection.Candidates = append(selection.Candidates, candidate)
	}
	for i := range selection.Candidates {
		c := &selection.Candidates[i]
		if len(c.DuplicateNames) > 0 {
			c.Reasons = append(c.Reasons, fmt.Sprintf("%d exact duplicate playlist(s)", len(c.DuplicateNames)))
		}
	}
	attachClipValidation(selection)
	attachMenuLabels(selection)
	attachNavigation(selection)
	sort.SliceStable(selection.Candidates, func(i, j int) bool {
		a, b := selection.Candidates[i], selection.Candidates[j]
		if a.Valid != b.Valid {
			return a.Valid
		}
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		return a.OriginalIndex < b.OriginalIndex
	})
	attachFamilies(selection)
	valid := 0
	for _, c := range selection.Candidates {
		if c.Valid {
			valid++
		}
	}
	if valid == 0 {
		selection.Reasons = []string{"No playlist has valid windows and all required clip references."}
		return selection, nil
	}
	if preferred := menuDefault(selection, valid); preferred != nil {
		selection.Recommended = preferred.Playlist
		selection.Confidence = "high"
		selection.Ambiguous = false
		selection.Reasons = []string{fmt.Sprintf("%s is labeled FPL_MainFeature in %s; localized picture branches remain separately listed. The label matches its MPLS timeline and all required clips are present.", preferred.Playlist.Name, preferred.LabelSource)}
		return selection, nil
	}
	top := &selection.Candidates[0]
	if top.suspect {
		selection.Reasons = []string{"The leading playlist has unusual repetition, duration, or playback order; select a title explicitly."}
		return selection, nil
	}
	if valid == 1 {
		selection.Recommended = top.Playlist
		selection.Confidence = "high"
		selection.Ambiguous = false
		selection.Reasons = []string{"One distinct playable presentation remains after validating clips and grouping exact duplicates."}
		return selection, nil
	}
	var comparable []string
	for i := 1; i < valid; i++ {
		other := &selection.Candidates[i]
		if other.suspect || (top.video && top.audio && (!other.video || !other.audio)) {
			continue
		}
		if trackSuperset(top.Playlist, other.Playlist) {
			top.Reasons = append(top.Reasons, fmt.Sprintf("same presentation as %s with every listed track plus additional tracks", other.Playlist.Name))
			continue
		}
		ratio := other.Playlist.Duration() / top.Playlist.Duration()
		shared := sharedClipFraction(top.Playlist, other.Playlist)
		if ratio >= .8 && ratio <= 1.25 || ratio >= .5 && ratio <= 2 && shared >= .5 {
			comparable = append(comparable, other.Playlist.Name)
		}
	}
	if len(comparable) > 0 {
		selection.Reasons = []string{fmt.Sprintf("%s has similarly long or substantially overlapping alternatives: %s.", top.Playlist.Name, strings.Join(comparable, ", ")), "Distinct cuts, episodes, languages, and playlist obfuscation require an explicit title choice."}
		return selection, nil
	}
	if looksLikeCompilation(top.Playlist, selection.Candidates[1:valid]) {
		selection.Reasons = []string{"The leading playlist combines clips from several similarly sized presentations; choose the compilation or an episode explicitly."}
		return selection, nil
	}
	if !top.video || !top.audio || top.Playlist.Duration() < 15*60 {
		selection.Reasons = []string{"Several presentations remain and structural evidence does not identify a clear main feature."}
		return selection, nil
	}
	selection.Recommended = top.Playlist
	selection.Confidence = "high"
	selection.Ambiguous = false
	selection.Reasons = []string{fmt.Sprintf("%s has the strongest duration and primary-stream evidence, valid clip references, and no comparable distinct cut or episode.", top.Playlist.Name)}
	return selection, nil
}

func presentationKey(p *Playlist) ([32]byte, error) {
	h := sha256.New()
	encoder := json.NewEncoder(h)
	for _, value := range []any{p.PlaybackType, p.PlaybackCount, p.Items, p.Marks, p.SubPaths, p.ExtSubPaths} {
		if err := encoder.Encode(value); err != nil {
			return [32]byte{}, err
		}
	}
	for _, ext := range p.Extensions {
		if err := encoder.Encode([]uint16{ext.ID1, ext.ID2}); err != nil {
			return [32]byte{}, err
		}
		if err := encoder.Encode(ext.Data); err != nil {
			return [32]byte{}, err
		}
	}
	var result [32]byte
	copy(result[:], h.Sum(nil))
	return result, nil
}

func rankCandidate(p *Playlist, index int, inventory *streamInventory) Candidate {
	c := Candidate{Playlist: p, OriginalIndex: index, Valid: true}
	if len(p.Items) == 0 || p.DurationTicks() == 0 {
		c.Valid = false
		c.Reasons = append(c.Reasons, "no positive-duration main presentation")
	}
	if inventory.err != nil {
		c.Valid = false
		c.Reasons = append(c.Reasons, inventory.err.Error())
	}
	missing := make(map[string]bool)
	check := func(clip Angle) {
		if err := validateClip(clip); err != nil {
			c.Valid = false
			c.Reasons = append(c.Reasons, err.Error())
			return
		}
		if !inventory.has(clip) {
			missing[clip.ClipID] = true
		}
	}
	short := 0
	lossless := false
	maxTracks := 0
	for _, item := range p.Items {
		if item.OutTime < item.InTime {
			c.Valid = false
			c.Reasons = append(c.Reasons, "a play-item out time precedes its in time")
		}
		check(Angle{ClipID: item.ClipID, CodecID: item.CodecID, STCID: item.STCID})
		if item.Duration() < 1 {
			short++
		}
		tracks := 0
		for _, stream := range item.Streams {
			if stream.Kind == "video" && stream.EntryType == 1 {
				c.video = true
			}
			if stream.Kind == "audio" && stream.EntryType == 1 {
				tracks++
				c.audio = true
				if stream.CodingType == 0x80 || stream.CodingType == 0x83 || stream.CodingType == 0x86 {
					lossless = true
				}
			}
			if stream.Kind == "subtitle" && stream.EntryType == 1 {
				tracks++
			}
		}
		if tracks > maxTracks {
			maxTracks = tracks
		}
	}
	for _, paths := range [][]SubPath{p.SubPaths, p.ExtSubPaths} {
		for _, path := range paths {
			for _, item := range path.Items {
				for _, clip := range item.Clips {
					check(clip)
				}
			}
		}
	}
	for id := range missing {
		c.MissingClips = append(c.MissingClips, id)
	}
	sort.Strings(c.MissingClips)
	if len(c.MissingClips) > 0 {
		c.Valid = false
		c.Reasons = append(c.Reasons, fmt.Sprintf("missing or ambiguous clip references: %s", strings.Join(c.MissingClips, ", ")))
	}
	if !c.Valid {
		c.Score = -1000000
		return c
	}
	minutes := p.Duration() / 60
	c.Score = 12 * math.Log1p(minutes)
	c.Reasons = append(c.Reasons, fmt.Sprintf("%.1f-minute presentation; all referenced clips available", minutes))
	if minutes >= 40 {
		c.Score += 20
	} else if minutes >= 15 {
		c.Score += 8
	}
	if c.video {
		c.Score += 16
		c.Reasons = append(c.Reasons, "primary video listed")
	}
	if c.audio {
		c.Score += 14
		c.Reasons = append(c.Reasons, "primary audio listed")
	}
	if lossless {
		c.Score += 2
	}
	c.Score += 2 * math.Log1p(float64(maxTracks))
	if len(p.Chapters) > 1 {
		c.Score += math.Min(float64(len(p.Chapters)), 6)
		c.Reasons = append(c.Reasons, fmt.Sprintf("%d chapter entries", len(p.Chapters)))
	}
	c.repeatFraction = repeatedFraction(p)
	if c.repeatFraction > .01 {
		c.Score -= 100 * c.repeatFraction
		c.Reasons = append(c.Reasons, fmt.Sprintf("%.1f%% of duration repeats previously used clip windows", 100*c.repeatFraction))
		if c.repeatFraction > .2 {
			c.suspect = true
		}
	}
	if len(p.Items) >= 20 && short*2 > len(p.Items) {
		c.Score -= 8
		c.Reasons = append(c.Reasons, fmt.Sprintf("%d of %d play items are shorter than one second", short, len(p.Items)))
	}
	if minutes > 240 {
		c.Score -= math.Min((minutes-240)*.25, 80)
		c.Reasons = append(c.Reasons, "unusually long presentation; may be a compilation or loop")
		c.suspect = true
	}
	if p.PlaybackType == 2 || p.PlaybackType == 3 {
		c.Score -= 30
		c.suspect = true
		c.Reasons = append(c.Reasons, "playlist declares random or shuffle playback")
	}
	c.Score = math.Round(c.Score*10) / 10
	return c
}

type timeWindow struct{ start, end uint32 }

func repeatedFraction(p *Playlist) float64 {
	if p.DurationTicks() == 0 {
		return 0
	}
	windows := make(map[string][]timeWindow)
	for _, item := range p.Items {
		key := fmt.Sprintf("%s/%s/%d", item.ClipID, item.CodecID, item.STCID)
		windows[key] = append(windows[key], timeWindow{item.InTime, item.OutTime})
	}
	var unique uint64
	for _, list := range windows {
		sort.Slice(list, func(i, j int) bool { return list[i].start < list[j].start })
		start, end := list[0].start, list[0].end
		for _, window := range list[1:] {
			if window.start > end {
				unique += uint64(end - start)
				start, end = window.start, window.end
			} else if window.end > end {
				end = window.end
			}
		}
		unique += uint64(end - start)
	}
	return float64(p.DurationTicks()-unique) / float64(p.DurationTicks())
}

func sharedClipFraction(a, b *Playlist) float64 {
	aWindows, aDuration := mergedWindows(a)
	bWindows, bDuration := mergedWindows(b)
	if aDuration == 0 || bDuration == 0 {
		return 0
	}
	var shared uint64
	for key, left := range aWindows {
		right := bWindows[key]
		for i, j := 0, 0; i < len(left) && j < len(right); {
			start, end := left[i].start, left[i].end
			if right[j].start > start {
				start = right[j].start
			}
			if right[j].end < end {
				end = right[j].end
			}
			if end > start {
				shared += uint64(end - start)
			}
			if left[i].end < right[j].end {
				i++
			} else {
				j++
			}
		}
	}
	minimum := aDuration
	if bDuration < minimum {
		minimum = bDuration
	}
	return float64(shared) / float64(minimum)
}

func mergedWindows(p *Playlist) (map[string][]timeWindow, uint64) {
	result := make(map[string][]timeWindow)
	for _, item := range p.Items {
		if item.OutTime <= item.InTime {
			continue
		}
		key := fmt.Sprintf("%s/%s/%d", item.ClipID, item.CodecID, item.STCID)
		result[key] = append(result[key], timeWindow{item.InTime, item.OutTime})
	}
	var duration uint64
	for key, windows := range result {
		sort.Slice(windows, func(i, j int) bool { return windows[i].start < windows[j].start })
		merged := windows[:1]
		for _, window := range windows[1:] {
			last := &merged[len(merged)-1]
			if window.start > last.end {
				merged = append(merged, window)
			} else if window.end > last.end {
				last.end = window.end
			}
		}
		result[key] = merged
		for _, window := range merged {
			duration += uint64(window.end - window.start)
		}
	}
	return result, duration
}

func trackSuperset(a, b *Playlist) bool {
	if len(a.Items) != len(b.Items) || a.PlaybackType != b.PlaybackType || !reflect.DeepEqual(a.Marks, b.Marks) || !reflect.DeepEqual(a.SubPaths, b.SubPaths) || !reflect.DeepEqual(a.ExtSubPaths, b.ExtSubPaths) || !reflect.DeepEqual(a.Extensions, b.Extensions) {
		return false
	}
	additional := false
	for i, left := range a.Items {
		right := b.Items[i]
		leftStreams, rightStreams := left.Streams, right.Streams
		left.Streams, right.Streams = nil, nil
		if !reflect.DeepEqual(left, right) {
			return false
		}
		if len(leftStreams) > len(rightStreams) {
			additional = true
		}
		for _, wanted := range rightStreams {
			found := false
			for _, available := range leftStreams {
				if reflect.DeepEqual(wanted, available) {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
	}
	return additional
}

func looksLikeCompilation(top *Playlist, others []Candidate) bool {
	var episodes []float64
	var duration float64
	for _, other := range others {
		minutes := other.Playlist.Duration() / 60
		if other.Valid && !other.suspect && minutes >= 15 && minutes <= 75 && sharedClipFraction(top, other.Playlist) >= .8 {
			episodes = append(episodes, other.Playlist.Duration())
			duration += other.Playlist.Duration()
		}
	}
	if len(episodes) < 3 {
		return false
	}
	sort.Float64s(episodes)
	return episodes[len(episodes)-1] <= episodes[0]*1.15 && duration >= top.Duration()*.8
}

type streamInventory struct {
	dir     string
	names   map[string]string
	checked map[string]bool
	err     error
}

func playlistBDMV(path string) string {
	dir := filepath.Dir(path)
	if !strings.EqualFold(filepath.Base(dir), "PLAYLIST") {
		return ""
	}
	dir = filepath.Dir(dir)
	if strings.EqualFold(filepath.Base(dir), "BACKUP") {
		dir = filepath.Dir(dir)
	}
	absolute, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	return absolute
}

func readInventory(bdmv string) *streamInventory {
	i := &streamInventory{names: make(map[string]string), checked: make(map[string]bool)}
	if bdmv == "" {
		i.err = fmt.Errorf("playlist path is not inside a PLAYLIST directory")
		return i
	}
	i.dir, i.err = childCaseInsensitive(bdmv, "STREAM")
	if i.err != nil {
		return i
	}
	entries, err := os.ReadDir(i.dir)
	if err != nil {
		i.err = err
		return i
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		key := strings.ToLower(entry.Name())
		if _, exists := i.names[key]; exists {
			i.names[key] = ""
			i.checked[key] = false
			continue
		}
		i.names[key] = entry.Name()
		if entry.Type().IsRegular() {
			i.checked[key] = true
		}
	}
	return i
}

func (i *streamInventory) has(clip Angle) bool {
	if i.err != nil {
		return false
	}
	ext := ".m2ts"
	if clip.CodecID == "FMTS" {
		ext = ".fmts"
	}
	key := clip.ClipID + ext
	if _, exists := i.names[key]; !exists && ext == ".m2ts" {
		key = clip.ClipID + ".mts"
	}
	name, exists := i.names[key]
	if !exists || name == "" {
		return false
	}
	if valid, checked := i.checked[key]; checked {
		return valid
	}
	// Only symlinks or unusual directory entries need a stat; each is checked
	// once per disc, regardless of how many playlists refer to the clip.
	info, err := os.Stat(filepath.Join(i.dir, name))
	valid := err == nil && info.Mode().IsRegular()
	i.checked[key] = valid
	return valid
}
