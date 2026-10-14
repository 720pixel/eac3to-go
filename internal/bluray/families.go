package bluray

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sort"
)

type PlaylistFamily struct {
	ID                   string               `json:"id"`
	Kind                 string               `json:"kind"`
	Members              []string             `json:"members"`
	DistinctOrders       int                  `json:"distinct_orders"`
	SuspectedObfuscation bool                 `json:"suspected_obfuscation"`
	Reasons              []string             `json:"reasons"`
	Differences          []PlaylistDifference `json:"differences,omitempty"`
	ComparedMembers      int                  `json:"compared_members"`
	DifferenceCount      int                  `json:"difference_count"`
	DifferencesTruncated bool                 `json:"differences_truncated"`
}

type PlaylistDifference struct {
	LeftPlaylist  string `json:"left_playlist"`
	RightPlaylist string `json:"right_playlist"`
	StartTicks    uint64 `json:"start_ticks"`
	EndTicks      uint64 `json:"end_ticks"`
	LeftItem      int    `json:"left_item"`
	RightItem     int    `json:"right_item"`
	LeftClip      string `json:"left_clip,omitempty"`
	RightClip     string `json:"right_clip,omitempty"`
	LeftInTicks   uint32 `json:"left_in_ticks"`
	LeftOutTicks  uint32 `json:"left_out_ticks"`
	RightInTicks  uint32 `json:"right_in_ticks"`
	RightOutTicks uint32 `json:"right_out_ticks"`
	Kind          string `json:"kind"`
}

type familyProfile struct {
	order, bag, shape [32]byte
	windows           map[string][]timeWindow
	duration          uint64
	disc              string
}

func familyFingerprints(p *Playlist) familyProfile {
	var order, shape []byte
	var pieces [][32]byte
	for _, item := range p.Items {
		piece := []byte(item.ClipID + item.CodecID)
		piece = append(piece, item.STCID, item.StillMode)
		piece = binary.BigEndian.AppendUint16(piece, item.StillTime)
		piece = binary.BigEndian.AppendUint32(piece, item.InTime)
		piece = binary.BigEndian.AppendUint32(piece, item.OutTime)
		piece = append(piece, pictureStreams(item)...)
		hash := sha256.Sum256(piece)
		pieces = append(pieces, hash)
		order = append(order, hash[:]...)
		shape = binary.BigEndian.AppendUint64(shape, item.DurationTicks())
	}
	sort.Slice(pieces, func(i, j int) bool { return bytes.Compare(pieces[i][:], pieces[j][:]) < 0 })
	var bag []byte
	for _, piece := range pieces {
		bag = append(bag, piece[:]...)
	}
	windows, duration := mergedWindows(p)
	return familyProfile{order: sha256.Sum256(order), bag: sha256.Sum256(bag), shape: sha256.Sum256(shape), windows: windows, duration: duration, disc: playlistBDMV(p.Path)}
}

func pictureStreams(item PlayItem) []byte {
	var result []byte
	for _, stream := range item.Streams {
		if stream.Kind == "video" {
			result = append(result, stream.EntryType, stream.CodingType)
			result = binary.BigEndian.AppendUint16(result, stream.PID)
		}
	}
	return result
}

func profileOverlap(a, b familyProfile) float64 {
	if a.duration == 0 || b.duration == 0 {
		return 0
	}
	var shared uint64
	for key, left := range a.windows {
		right := b.windows[key]
		for i, j := 0, 0; i < len(left) && j < len(right); {
			start, end := max(left[i].start, right[j].start), min(left[i].end, right[j].end)
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
	return float64(shared) / float64(min(a.duration, b.duration))
}

func attachFamilies(selection *Selection) {
	n := len(selection.Candidates)
	parents := make([]int, n)
	profiles := make([]familyProfile, n)
	var root func(int) int
	root = func(i int) int {
		if parents[i] != i {
			parents[i] = root(parents[i])
		}
		return parents[i]
	}
	join := func(a, b int) { parents[root(a)] = root(b) }
	type groupKey struct {
		disc string
		hash [32]byte
	}
	orders, bags, shapes := make(map[groupKey]int), make(map[groupKey]int), make(map[groupKey]int)
	postings := make(map[string][]int)
	for i, candidate := range selection.Candidates {
		parents[i] = i
		if !candidate.Valid {
			continue
		}
		p := familyFingerprints(candidate.Playlist)
		profiles[i] = p
		for kind, table := range []map[groupKey]int{orders, bags, shapes} {
			hash := [][32]byte{p.order, p.bag, p.shape}[kind]
			key := groupKey{p.disc, hash}
			if previous, exists := table[key]; exists {
				if kind < 2 || len(candidate.Playlist.Items) > 1 && profileOverlap(p, profiles[previous]) >= .25 || sameAuthoredFeature(candidate, selection.Candidates[previous]) {
					join(i, previous)
				}
			} else {
				table[key] = i
			}
		}
		// Popular shared logos cannot produce an all-pairs comparison. Retain a
		// small representative set per clip and test at most eight neighbours.
		votes := make(map[int]int)
		for clip := range p.windows {
			key := p.disc + "/" + clip
			for _, previous := range postings[key] {
				votes[previous]++
			}
			if len(postings[key]) < 16 {
				postings[key] = append(postings[key], i)
			}
		}
		var nearby []int
		for previous := range votes {
			nearby = append(nearby, previous)
		}
		sort.Slice(nearby, func(a, b int) bool {
			if votes[nearby[a]] != votes[nearby[b]] {
				return votes[nearby[a]] > votes[nearby[b]]
			}
			return nearby[a] < nearby[b]
		})
		for _, previous := range nearby[:min(8, len(nearby))] {
			other := profiles[previous]
			if p.duration*2 >= other.duration && other.duration*2 >= p.duration && profileOverlap(p, other) >= .65 {
				join(i, previous)
			}
		}
	}
	groups := make(map[int][]int)
	var groupOrder []int
	for i := range selection.Candidates {
		if !selection.Candidates[i].Valid {
			continue
		}
		key := root(i)
		if _, found := groups[key]; !found {
			groupOrder = append(groupOrder, key)
		}
		groups[key] = append(groups[key], i)
	}
	for _, key := range groupOrder {
		members := groups[key]
		if len(members) < 2 {
			continue
		}
		family := PlaylistFamily{ID: fmt.Sprintf("family-%03d", len(selection.Families)+1), Kind: "overlapping_cuts"}
		distinct := make(map[[32]byte]bool)
		bagOrders := make(map[[32]byte]map[[32]byte]bool)
		localized := false
		for _, member := range members {
			candidate := &selection.Candidates[member]
			candidate.FamilyID = family.ID
			family.Members = append(family.Members, candidate.Playlist.Name)
			family.Members = append(family.Members, candidate.DuplicateNames...)
			profile := profiles[member]
			distinct[profile.order] = true
			if bagOrders[profile.bag] == nil {
				bagOrders[profile.bag] = make(map[[32]byte]bool)
			}
			bagOrders[profile.bag][profile.order] = true
			localized = localized || candidate.PictureLanguage != ""
		}
		family.DistinctOrders = len(distinct)
		permutations := 0
		for _, orderSet := range bagOrders {
			permutations = max(permutations, len(orderSet))
		}
		switch {
		case permutations > 1:
			family.Kind = "clip_permutations"
			family.SuspectedObfuscation = permutations >= 3
			family.Reasons = append(family.Reasons, fmt.Sprintf("The same clip windows appear in %d different orders; metadata similarity cannot establish narrative order.", permutations))
		case len(distinct) == 1:
			family.Kind = "same_picture"
			family.Reasons = append(family.Reasons, "Picture clip order and windows match; track listings, marks, or other presentation metadata differ.")
		case localized:
			family.Kind = "localized_picture"
			family.Reasons = append(family.Reasons, "Authored menu metadata identifies a picture-language branch; differing clip windows remain visible.")
		default:
			family.Reasons = append(family.Reasons, "Presentations share substantial source windows; this alone does not identify an edition or a preferred cut.")
		}
		if family.SuspectedObfuscation {
			selection.SuspectedObfuscationFamilies++
		}
		base := selection.Candidates[members[0]].Playlist
		for _, member := range members[1:min(len(members), 9)] {
			other := selection.Candidates[member].Playlist
			diffs, count := ComparePlaylistWindows(base, other, 32-len(family.Differences))
			family.ComparedMembers++
			family.DifferenceCount += count
			family.Differences = append(family.Differences, diffs...)
		}
		family.DifferencesTruncated = len(family.Differences) < family.DifferenceCount || family.ComparedMembers < len(members)-1
		selection.Families = append(selection.Families, family)
	}
}

func sameAuthoredFeature(a, b Candidate) bool {
	aFeature := a.Label == "FPL_MainFeature" || a.PictureLanguage != "" || a.Label == "FPL_MainFeature_Narrative"
	bFeature := b.Label == "FPL_MainFeature" || b.PictureLanguage != "" || b.Label == "FPL_MainFeature_Narrative"
	return aFeature && bFeature && a.LabelSource != "" && a.LabelSource == b.LabelSource && playlistBDMV(a.Playlist.Path) == playlistBDMV(b.Playlist.Path)
}

// ComparePlaylistWindows aligns the two presentation timelines in 45 kHz ticks.
// It returns at most limit differences plus the total number of differing
// intervals. A missing side has item -1. Track-language changes are separate
// from these picture/source-window differences.
func ComparePlaylistWindows(a, b *Playlist, limit int) ([]PlaylistDifference, int) {
	if a == nil || b == nil {
		return nil, 0
	}
	limit = min(max(limit, 0), 256)
	var differences []PlaylistDifference
	count, i, j := 0, 0, 0
	aTotal, bTotal := a.DurationTicks(), b.DurationTicks()
	var aStart, bStart, position uint64
	for i < len(a.Items) || j < len(b.Items) {
		for i < len(a.Items) && a.Items[i].DurationTicks() == 0 {
			i++
		}
		for j < len(b.Items) && b.Items[j].DurationTicks() == 0 {
			j++
		}
		if i == len(a.Items) && j == len(b.Items) {
			break
		}
		aEnd, bEnd := aTotal, bTotal
		if i < len(a.Items) {
			aEnd = aStart + a.Items[i].DurationTicks()
		}
		if j < len(b.Items) {
			bEnd = bStart + b.Items[j].DurationTicks()
		}
		end := max(aEnd, bEnd)
		if i < len(a.Items) && j < len(b.Items) {
			end = min(aEnd, bEnd)
		}
		diff := PlaylistDifference{LeftPlaylist: a.Name, RightPlaylist: b.Name, StartTicks: position, EndTicks: end, LeftItem: -1, RightItem: -1, Kind: "source_window"}
		if i < len(a.Items) {
			diff.LeftItem, diff.LeftClip = i, a.Items[i].ClipID
			diff.LeftInTicks = a.Items[i].InTime + uint32(position-aStart)
			diff.LeftOutTicks = diff.LeftInTicks + uint32(end-position)
		}
		if j < len(b.Items) {
			diff.RightItem, diff.RightClip = j, b.Items[j].ClipID
			diff.RightInTicks = b.Items[j].InTime + uint32(position-bStart)
			diff.RightOutTicks = diff.RightInTicks + uint32(end-position)
		}
		different := diff.LeftItem < 0 || diff.RightItem < 0
		if !different {
			left, right := a.Items[i], b.Items[j]
			different = left.ClipID != right.ClipID || left.CodecID != right.CodecID || left.STCID != right.STCID || diff.LeftInTicks != diff.RightInTicks
			if !different && (!bytes.Equal(pictureStreams(left), pictureStreams(right)) || left.StillMode != right.StillMode || left.StillTime != right.StillTime) {
				different, diff.Kind = true, "picture_presentation"
			}
		} else {
			diff.Kind = "duration"
		}
		if different {
			count++
			if len(differences) < limit {
				differences = append(differences, diff)
			}
		}
		position = end
		if i < len(a.Items) && end == aEnd {
			i++
			aStart = end
		}
		if j < len(b.Items) && end == bEnd {
			j++
			bStart = end
		}
	}
	return differences, count
}
