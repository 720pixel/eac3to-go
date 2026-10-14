package bluray

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type NavigationEvidence struct {
	Entry        string `json:"entry"`
	Title        int    `json:"title,omitempty"`
	PlaylistID   uint32 `json:"playlist_id"`
	ObjectID     int    `json:"object_id"`
	CommandIndex int    `json:"command_index"`
	Mode         string `json:"mode"`
	Position     uint32 `json:"position,omitempty"`
	Proven       bool   `json:"proven"`
	Explanation  string `json:"explanation"`
}

type NavigationEntryReport struct {
	Entry        string               `json:"entry"`
	Title        int                  `json:"title,omitempty"`
	Kind         string               `json:"kind"`
	AccessType   uint8                `json:"access_type"`
	ObjectID     uint16               `json:"object_id"`
	BDJOName     string               `json:"bdjo_name,omitempty"`
	Complete     bool                 `json:"complete"`
	Playlists    []NavigationEvidence `json:"playlists,omitempty"`
	Dependencies []string             `json:"dependencies,omitempty"`
	Unresolved   []string             `json:"unresolved,omitempty"`
}

type NavigationReport struct {
	Path             string                  `json:"path"`
	TitleCount       int                     `json:"title_count"`
	HDMVTitles       int                     `json:"hdmv_titles"`
	BDJTitles        int                     `json:"bdj_titles"`
	MovieObjectCount int                     `json:"movie_object_count"`
	Entries          []NavigationEntryReport `json:"entries"`
	Warnings         []string                `json:"warnings,omitempty"`
}

type navigationTrace struct {
	plays        []NavigationEvidence
	dependencies []string
	unresolved   []string
	withoutPlay  bool
}

// AnalyzeNavigation follows the first playlist reached from each index entry.
// Unknown comparisons explore both successors; unknown jumps stop that path.
// Playback ends a trace because interactive graphics and player events may
// change registers before the MovieObject resumes. No player state is guessed.
func AnalyzeNavigation(index *DiscIndex, objects *MovieObjects) NavigationReport {
	var report NavigationReport
	if index == nil {
		report.Warnings = []string{"No index metadata is available."}
		return report
	}
	report.TitleCount = len(index.Titles)
	if objects != nil {
		report.MovieObjectCount = len(objects.Objects)
	}
	if len(index.ExtensionData) > 0 || objects != nil && len(objects.ExtensionData) > 0 {
		report.Warnings = append(report.Warnings, "Navigation extension data is retained but is not interpreted.")
	}
	remaining := 50000
	evidenceRemaining := 4096
	cache := make(map[uint16]navigationTrace)
	entries := append([]IndexEntry{index.FirstPlay, index.TopMenu}, index.Titles...)
	for number, entry := range entries {
		name, title := "first_play", 0
		if number == 1 {
			name = "top_menu"
		} else if number > 1 {
			title = number - 1
			name = fmt.Sprintf("title:%d", title)
		}
		result := NavigationEntryReport{Entry: name, Title: title, AccessType: entry.AccessType, ObjectID: entry.ObjectID, BDJOName: entry.BDJOName}
		if entry.ObjectType == 2 {
			result.Kind = "bdj"
			result.Unresolved = []string{"BD-J application controls playback; static HDMV commands do not establish its playlist choice."}
			if title != 0 {
				report.BDJTitles++
			}
		} else if entry.ObjectType != 1 {
			result.Kind = "unknown"
			result.Unresolved = []string{"Unknown index object type."}
		} else if entry.ObjectID == 0xffff {
			result.Kind, result.Complete = "absent", true
		} else {
			result.Kind = "hdmv"
			if title != 0 {
				report.HDMVTitles++
			}
			trace, exists := cache[entry.ObjectID]
			if !exists {
				trace = traceNavigation(index, objects, int(entry.ObjectID), &remaining)
				cache[entry.ObjectID] = trace
			}
			result.Complete = len(trace.unresolved) == 0
			result.Dependencies = trace.dependencies
			result.Unresolved = append([]string(nil), trace.unresolved...)
			if len(index.ExtensionData) > 0 || objects != nil && len(objects.ExtensionData) > 0 {
				result.Complete = false
				result.Unresolved = append(result.Unresolved, "Uninterpreted navigation extensions prevent a complete first-playlist proof.")
			}
			if len(trace.plays) > evidenceRemaining {
				result.Complete = false
				result.Unresolved = append(result.Unresolved, "The bounded playlist-reference output limit was reached.")
				report.Warnings = appendUnique(report.Warnings, "Some playlist references were omitted after the 4096-reference output limit.", 16)
			}
			unique := make(map[string]bool)
			if result.Complete {
				for _, play := range trace.plays {
					unique[fmt.Sprintf("%d/%s/%d", play.PlaylistID, play.Mode, play.Position)] = true
				}
			}
			proven := result.Complete && !trace.withoutPlay && len(unique) == 1
			for _, play := range trace.plays[:min(len(trace.plays), evidenceRemaining)] {
				play.Entry, play.Title, play.Proven = name, title, proven
				if proven {
					play.Explanation = "Every resolved path from this index entry reaches this first playlist. This establishes entry playback, not the disc's main feature."
				} else {
					play.Explanation = "A statically possible path references this playlist; player state or unresolved navigation may choose another path."
				}
				result.Playlists = append(result.Playlists, play)
				evidenceRemaining--
			}
		}
		report.Entries = append(report.Entries, result)
	}
	return report
}

func traceNavigation(index *DiscIndex, objects *MovieObjects, initial int, remaining *int) navigationTrace {
	var trace navigationTrace
	if objects == nil {
		trace.unresolved = []string{"MovieObject metadata is unavailable."}
		return trace
	}
	colors := make(map[string]uint8)
	playSites := make(map[string]bool)
	states := 0
	addIssue := func(text string) { trace.unresolved = appendUnique(trace.unresolved, text, 16) }
	var visit func(int, int, map[uint32]uint32, int)
	visit = func(object, pc int, known map[uint32]uint32, depth int) {
		if object < 0 || object >= len(objects.Objects) {
			addIssue(fmt.Sprintf("MovieObject reference %d is outside the object table.", object))
			return
		}
		if pc == len(objects.Objects[object].Commands) {
			trace.withoutPlay = true
			return
		}
		if pc < 0 || pc > len(objects.Objects[object].Commands) {
			addIssue(fmt.Sprintf("Command target %d is outside MovieObject %d.", pc, object))
			return
		}
		key := navStateKey(object, pc, known)
		if colors[key] == 2 {
			return
		}
		if colors[key] == 1 {
			addIssue("A navigation cycle prevents a complete first-playlist proof.")
			return
		}
		if depth >= 1024 || states >= 8192 || *remaining <= 0 {
			addIssue("The bounded navigation analysis limit was reached.")
			return
		}
		states++
		*remaining--
		colors[key] = 1
		defer func() { colors[key] = 2 }()
		cmd := objects.Objects[object].Commands[pc]
		if cmd.Reserved != 0 || cmd.Operands > 2 {
			addIssue(fmt.Sprintf("Unsupported command encoding in MovieObject %d command %d.", object, pc))
			return
		}
		dst, dstKnown := navOperand(cmd.Destination, cmd.Immediate1, known)
		src, srcKnown := navOperand(cmd.Source, cmd.Immediate2, known)
		next := func(offset int, regs map[uint32]uint32) { visit(object, pc+offset, regs, depth+1) }
		switch cmd.Group {
		case 0:
			switch cmd.Subgroup {
			case 0:
				switch cmd.Branch {
				case 0:
					next(1, known)
				case 1:
					if cmd.Operands != 1 || !dstKnown {
						addIssue("A computed Goto target depends on unknown register state.")
					} else {
						visit(object, int(dst), known, depth+1)
					}
				case 2:
					trace.withoutPlay = true
				default:
					addIssue("Unknown HDMV branch operation.")
				}
			case 1:
				if cmd.Branch == 4 {
					addIssue("Resume depends on a saved player context.")
				} else if cmd.Operands != 1 || !dstKnown {
					addIssue("A title or object jump depends on unknown register state.")
				} else if cmd.Branch == 0 || cmd.Branch == 2 {
					visit(int(dst), 0, known, depth+1)
				} else if cmd.Branch == 1 || cmd.Branch == 3 {
					entry, ok := navTitle(index, dst)
					if !ok || entry.ObjectType != 1 || entry.ObjectID == 0xffff {
						addIssue("A title transfer reaches absent or BD-J navigation.")
					} else {
						visit(int(entry.ObjectID), 0, known, depth+1)
					}
				} else {
					addIssue("Unknown HDMV title-transfer operation.")
				}
			case 2:
				if cmd.Branch > 2 {
					addIssue("Playlist termination or interactive linking requires player state.")
					return
				}
				if !dstKnown || cmd.Operands < 1 || dst > 99999 {
					addIssue("A playlist number is invalid or depends on unknown register state.")
					return
				}
				mode := []string{"playlist", "item", "mark"}[cmd.Branch]
				if cmd.Branch != 0 && (cmd.Operands != 2 || !srcKnown) {
					addIssue("A playlist entry position depends on unknown register state.")
				}
				position := uint32(0)
				if cmd.Branch != 0 && srcKnown {
					position = src
				}
				site := fmt.Sprintf("%d/%d/%d/%s/%d", object, pc, dst, mode, position)
				if !playSites[site] && len(trace.plays) < 256 {
					playSites[site] = true
					trace.plays = append(trace.plays, NavigationEvidence{PlaylistID: dst, ObjectID: object, CommandIndex: pc, Mode: mode, Position: position})
				} else if len(trace.plays) == 256 {
					addIssue("The bounded playlist-reference limit was reached.")
				}
			default:
				addIssue("Unknown HDMV branch subgroup.")
			}
		case 1:
			if cmd.Operands != 2 || cmd.Subgroup != 0 || cmd.Compare < 1 || cmd.Compare > 7 {
				addIssue("Unsupported HDMV comparison.")
				return
			}
			if dstKnown && srcKnown {
				if navCompare(cmd.Compare, dst, src) {
					next(1, known)
				} else {
					next(2, known)
				}
			} else {
				trace.dependencies = appendUnique(trace.dependencies, fmt.Sprintf("MovieObject %d command %d compares player-dependent register values; both successors were considered.", object, pc), 16)
				next(1, known)
				next(2, known)
			}
		case 2:
			if cmd.Subgroup == 1 {
				// These operations only select streams or output presentation.
				// Timers, buttons, and still-frame commands can alter navigation.
				if cmd.Set == 1 || cmd.Set == 6 || cmd.Set == 10 || cmd.Set == 11 {
					next(1, known)
				} else {
					addIssue("A player timer, button, still-frame, or unsupported system command requires runtime navigation.")
				}
				return
			}
			if cmd.Subgroup != 0 || cmd.Operands != 2 || cmd.Immediate1 || cmd.Destination > 4095 || cmd.Set < 1 || cmd.Set > 15 {
				addIssue("Unsupported HDMV register assignment.")
				return
			}
			changed := make(map[uint32]uint32, len(known)+1)
			for register, value := range known {
				changed[register] = value
			}
			if cmd.Set == 2 {
				if cmd.Immediate2 || cmd.Source > 4095 {
					addIssue("Invalid register swap.")
					return
				}
				putNavValue(changed, cmd.Source, dst, dstKnown)
				putNavValue(changed, cmd.Destination, src, srcKnown)
			} else {
				value, valid := navCalculate(cmd.Set, dst, src, dstKnown, srcKnown)
				putNavValue(changed, cmd.Destination, value, valid)
			}
			next(1, changed)
		default:
			addIssue("Unknown HDMV command group.")
		}
	}
	visit(initial, 0, nil, 0)
	return trace
}

func navTitle(index *DiscIndex, title uint32) (IndexEntry, bool) {
	if title == 0 {
		return index.TopMenu, true
	}
	if title == 0xffff {
		return index.FirstPlay, true
	}
	if title > uint32(len(index.Titles)) {
		return IndexEntry{}, false
	}
	return index.Titles[title-1], true
}

func navStateKey(object, pc int, known map[uint32]uint32) string {
	keys := make([]uint32, 0, len(known))
	for key := range known {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	b := binary.BigEndian.AppendUint32(nil, uint32(object))
	b = binary.BigEndian.AppendUint32(b, uint32(pc))
	for _, key := range keys {
		b = binary.BigEndian.AppendUint32(b, key)
		b = binary.BigEndian.AppendUint32(b, known[key])
	}
	return string(b)
}

func navOperand(value uint32, immediate bool, known map[uint32]uint32) (uint32, bool) {
	if immediate {
		return value, true
	}
	if value > 4095 { // PSRs and invalid register encodings have no assumed value.
		return 0, false
	}
	v, exists := known[value]
	return v, exists
}

func putNavValue(known map[uint32]uint32, register, value uint32, valid bool) {
	if valid {
		known[register] = value
	} else {
		delete(known, register)
	}
}

func navCompare(operation uint8, a, b uint32) bool {
	switch operation {
	case 1:
		return a & ^b == 0
	case 2:
		return a == b
	case 3:
		return a != b
	case 4:
		return a >= b
	case 5:
		return a > b
	case 6:
		return a <= b
	default:
		return a < b
	}
}

func navCalculate(operation uint8, a, b uint32, aKnown, bKnown bool) (uint32, bool) {
	if operation == 1 {
		return b, bKnown
	}
	if !aKnown || !bKnown || operation == 8 || operation >= 12 && b >= 32 {
		return 0, false
	}
	switch operation {
	case 3:
		return uint32(min(uint64(a)+uint64(b), math.MaxUint32)), true
	case 4:
		if a < b {
			return 0, true
		}
		return a - b, true
	case 5:
		return uint32(min(uint64(a)*uint64(b), math.MaxUint32)), true
	case 6, 7:
		if b == 0 {
			return math.MaxUint32, true
		}
		if operation == 6 {
			return a / b, true
		}
		return a % b, true
	case 9:
		return a & b, true
	case 10:
		return a | b, true
	case 11:
		return a ^ b, true
	case 12:
		return a | 1<<b, true
	case 13:
		return a & ^(1 << b), true
	case 14:
		return a << b, true
	case 15:
		return a >> b, true
	}
	return 0, false
}

func appendUnique(values []string, value string, limit int) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	if len(values) < limit {
		return append(values, value)
	}
	return values
}

func attachNavigation(selection *Selection) {
	byDisc := make(map[string]map[uint32][]NavigationEvidence)
	for i := range selection.Candidates {
		candidate := &selection.Candidates[i]
		bdmv := playlistBDMV(candidate.Playlist.Path)
		byPlaylist, loaded := byDisc[bdmv]
		if !loaded {
			report, err := ReadNavigation(bdmv)
			byPlaylist = make(map[uint32][]NavigationEvidence)
			byDisc[bdmv] = byPlaylist
			if err != nil {
				if !os.IsNotExist(err) {
					selection.MetadataWarnings = append(selection.MetadataWarnings, "Navigation metadata ignored: "+err.Error())
				}
			} else {
				selection.Navigation = append(selection.Navigation, *report)
				for _, entry := range report.Entries {
					for _, evidence := range entry.Playlists {
						byPlaylist[evidence.PlaylistID] = append(byPlaylist[evidence.PlaylistID], evidence)
					}
				}
			}
		}
		seen := make(map[uint32]bool)
		for _, name := range append([]string{candidate.Playlist.Name}, candidate.DuplicateNames...) {
			extension := strings.ToLower(filepath.Ext(name))
			stem := strings.TrimSuffix(name, filepath.Ext(name))
			if (extension != ".mpls" && extension != ".mpl") || !fiveDigits(stem) {
				continue
			}
			id, _ := strconv.ParseUint(stem, 10, 32)
			if seen[uint32(id)] {
				continue
			}
			seen[uint32(id)] = true
			for _, evidence := range byPlaylist[uint32(id)] {
				if evidence.Mode == "item" && uint64(evidence.Position) >= uint64(len(candidate.Playlist.Items)) || evidence.Mode == "mark" && uint64(evidence.Position) >= uint64(len(candidate.Playlist.Marks)) {
					evidence.Proven = false
					evidence.Explanation = "This command references a position outside the parsed playlist; it does not establish valid entry playback."
				}
				candidate.Navigation = append(candidate.Navigation, evidence)
				candidate.Reasons = appendUnique(candidate.Reasons, fmt.Sprintf("%s references this playlist in HDMV navigation (first-playlist proof: %t)", evidence.Entry, evidence.Proven), 64)
			}
		}
	}
}
