package bluray

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
)

func fixtureIndex(index DiscIndex) []byte {
	var entries fixtureBytes
	entry := func(e IndexEntry) {
		entries.u32(uint32(e.ObjectType)<<30 | uint32(e.AccessType)<<28)
		entries.u16(uint16(e.PlaybackType) << 14)
		if e.ObjectType == 2 {
			entries.str(e.BDJOName)
			entries.zero(1)
		} else {
			entries.u16(e.ObjectID)
			entries.zero(4)
		}
	}
	entry(index.FirstPlay)
	entry(index.TopMenu)
	entries.u16(uint16(len(index.Titles)))
	for _, e := range index.Titles {
		entry(e)
	}
	data := make([]byte, 40)
	copy(data, "INDX0200")
	binary.BigEndian.PutUint32(data[8:12], 78)
	data = append(data, block32(make([]byte, 34))...)
	return append(data, block32(entries)...)
}

func fixtureObjects(objects ...[]NavigationCommand) []byte {
	var table fixtureBytes
	table.zero(4)
	table.u16(uint16(len(objects)))
	for _, object := range objects {
		table.u16(0x8000)
		table.u16(uint16(len(object)))
		for _, command := range object {
			op := uint32(command.Operands)<<29 | uint32(command.Group)<<27 | uint32(command.Subgroup)<<24 | uint32(command.Branch)<<16 | uint32(command.Compare)<<8 | uint32(command.Set)
			if command.Immediate1 {
				op |= 0x800000
			}
			if command.Immediate2 {
				op |= 0x400000
			}
			table.u32(op)
			table.u32(command.Destination)
			table.u32(command.Source)
		}
	}
	data := make([]byte, 40)
	copy(data, "MOBJ0200")
	return append(data, block32(table)...)
}

func playCommand(id uint32) NavigationCommand {
	return NavigationCommand{Operands: 1, Subgroup: 2, Immediate1: true, Destination: id}
}

func navFixture(t *testing.T, commands ...NavigationCommand) (*DiscIndex, *MovieObjects) {
	t.Helper()
	i, err := ParseIndex(fixtureIndex(DiscIndex{FirstPlay: IndexEntry{ObjectType: 1, ObjectID: 1}, TopMenu: IndexEntry{ObjectType: 1, ObjectID: 2, PlaybackType: 1}, Titles: []IndexEntry{{ObjectType: 1, ObjectID: 0}}}))
	if err != nil {
		t.Fatal(err)
	}
	o, err := ParseMovieObjects(fixtureObjects(commands, []NavigationCommand{playCommand(999)}, []NavigationCommand{playCommand(998)}))
	if err != nil {
		t.Fatal(err)
	}
	return i, o
}

func TestNavigationSeparatesStartupMenuAndFeature(t *testing.T) {
	i, o := navFixture(t, playCommand(1), playCommand(7))
	report := AnalyzeNavigation(i, o)
	if report.HDMVTitles != 1 || report.MovieObjectCount != 3 || len(report.Entries) != 3 {
		t.Fatal(report)
	}
	for entry, id := range []uint32{999, 998, 1} {
		r := report.Entries[entry]
		if !r.Complete || len(r.Playlists) != 1 || r.Playlists[0].PlaylistID != id || !r.Playlists[0].Proven {
			t.Fatalf("entry %d: %+v", entry, r)
		}
	}
}

func TestNavigationConstantPropagationAndUnknownBranchMerge(t *testing.T) {
	i, o := navFixture(t,
		NavigationCommand{Operands: 2, Group: 2, Set: 1, Immediate2: true, Destination: 5, Source: 123},
		NavigationCommand{Operands: 2, Group: 1, Compare: 2, Immediate2: true, Destination: 0x80000024, Source: 1},
		NavigationCommand{Operands: 1, Branch: 1, Immediate1: true, Destination: 4},
		NavigationCommand{Operands: 1, Branch: 1, Immediate1: true, Destination: 5},
		NavigationCommand{Group: 2, Subgroup: 1, Set: 1},
		NavigationCommand{Operands: 1, Subgroup: 2, Destination: 5},
	)
	r := AnalyzeNavigation(i, o).Entries[2]
	if !r.Complete || len(r.Dependencies) != 1 || len(r.Playlists) != 1 || !r.Playlists[0].Proven || r.Playlists[0].PlaylistID != 123 {
		t.Fatal(r)
	}
}

func TestNavigationUncertaintyNeverBecomesDefaultProof(t *testing.T) {
	for _, scenario := range []string{"branches", "computed", "cycle", "runtime", "dead_path", "bdj"} {
		t.Run(scenario, func(t *testing.T) {
			var commands []NavigationCommand
			switch scenario {
			case "branches":
				commands = []NavigationCommand{{Operands: 2, Group: 1, Compare: 2, Immediate2: true, Destination: 0, Source: 1}, playCommand(1), playCommand(2)}
			case "computed":
				commands = []NavigationCommand{{Operands: 1, Subgroup: 2, Destination: 0}}
			case "cycle":
				commands = []NavigationCommand{{Operands: 1, Branch: 1, Immediate1: true, Destination: 0}, playCommand(1)}
			case "runtime":
				commands = []NavigationCommand{{Group: 2, Subgroup: 1, Set: 2}, playCommand(1)}
			case "dead_path":
				commands = []NavigationCommand{{Operands: 2, Group: 1, Compare: 2, Destination: 0, Source: 1}, {Branch: 2}, playCommand(1)}
			case "bdj":
				commands = []NavigationCommand{{Operands: 1, Subgroup: 1, Branch: 1, Immediate1: true, Destination: 0}}
			}
			i, o := navFixture(t, commands...)
			if scenario == "bdj" {
				i.TopMenu = IndexEntry{ObjectType: 2, PlaybackType: 3, BDJOName: "00000"}
			}
			r := AnalyzeNavigation(i, o).Entries[2]
			for _, evidence := range r.Playlists {
				if evidence.Proven {
					t.Fatalf("uncertain path promoted to proof: %+v", r)
				}
			}
			if scenario == "branches" && len(r.Playlists) != 2 {
				t.Fatal("both possible playlist branches must be exposed")
			}
		})
	}
}

func TestNavigationParserBounds(t *testing.T) {
	index := fixtureIndex(DiscIndex{FirstPlay: IndexEntry{ObjectType: 1, ObjectID: 0xffff}, TopMenu: IndexEntry{ObjectType: 2, PlaybackType: 3, BDJOName: "00001"}, Titles: []IndexEntry{{ObjectType: 1}}})
	objects := fixtureObjects([]NavigationCommand{playCommand(1)})
	for _, tc := range []struct {
		name string
		data []byte
		read func([]byte) error
	}{{"index", index, func(b []byte) error { _, err := ParseIndex(b); return err }}, {"objects", objects, func(b []byte) error { _, err := ParseMovieObjects(b); return err }}} {
		for length := 0; length < len(tc.data); length++ {
			if err := tc.read(tc.data[:length]); err == nil {
				t.Fatalf("%s accepted truncation at %d", tc.name, length)
			}
		}
		bad := append([]byte(nil), tc.data...)
		binary.BigEndian.PutUint32(bad[8:12], 40)
		if err := tc.read(bad); err == nil {
			t.Fatal("accepted overlapping navigation sections")
		}
	}
}

func TestNavigationArithmeticUsesUnsignedSaturation(t *testing.T) {
	for _, tc := range []struct {
		operation  uint8
		a, b, want uint32
	}{{3, math.MaxUint32, 1, math.MaxUint32}, {4, 1, 2, 0}, {5, 0xffff, 0xffff, 0xfffe0001}, {5, 0x10000, 0x10000, math.MaxUint32}, {6, 10, 0, math.MaxUint32}, {7, 10, 3, 1}} {
		got, known := navCalculate(tc.operation, tc.a, tc.b, true, true)
		if !known || got != tc.want {
			t.Fatalf("operation %d = %d/%t, want %d", tc.operation, got, known, tc.want)
		}
	}
	if _, known := navCalculate(14, 1, 32, true, true); known {
		t.Fatal("undefined shift was treated as a known value")
	}
}

func TestNavigationExtensionsAndReferenceLimitPreventIncompleteProofs(t *testing.T) {
	i, o := navFixture(t, playCommand(1))
	o.ExtensionData = []byte{1}
	r := AnalyzeNavigation(i, o)
	if r.Entries[2].Complete || r.Entries[2].Playlists[0].Proven || len(r.Entries[2].Unresolved) == 0 {
		t.Fatal("uninterpreted extensions were accepted as a complete proof")
	}
	o.ExtensionData = nil
	i.Titles = make([]IndexEntry, 5000)
	for j := range i.Titles {
		i.Titles[j] = IndexEntry{ObjectType: 1}
	}
	r = AnalyzeNavigation(i, o)
	count := 0
	for _, entry := range r.Entries {
		count += len(entry.Playlists)
	}
	if count != 4096 || r.Entries[len(r.Entries)-1].Complete || len(r.Warnings) == 0 {
		t.Fatalf("unbounded or silently incomplete references: %d, %+v", count, r.Warnings)
	}
}

func TestNavigationCandidateAliasesAndInvalidEntryPosition(t *testing.T) {
	bdmv := filepath.Join(t.TempDir(), "BDMV")
	if err := os.Mkdir(bdmv, 0755); err != nil {
		t.Fatal(err)
	}
	i, _ := navFixture(t, playCommand(1))
	if err := os.WriteFile(filepath.Join(bdmv, "index.bdmv"), fixtureIndex(*i), 0644); err != nil {
		t.Fatal(err)
	}
	command := playCommand(2)
	command.Operands, command.Branch, command.Immediate2, command.Source = 2, 1, true, 3
	if err := os.WriteFile(filepath.Join(bdmv, "MovieObject.bdmv"), fixtureObjects([]NavigationCommand{command}, []NavigationCommand{playCommand(999)}, []NavigationCommand{playCommand(998)}), 0644); err != nil {
		t.Fatal(err)
	}
	s := &Selection{Candidates: []Candidate{{Playlist: &Playlist{Name: "00001.MPLS", Path: filepath.Join(bdmv, "PLAYLIST", "00001.MPLS"), Items: []PlayItem{{}}}, DuplicateNames: []string{"00002.mpl", "00002.MPLS"}}}}
	attachNavigation(s)
	if len(s.Candidates[0].Navigation) != 1 || s.Candidates[0].Navigation[0].Proven {
		t.Fatalf("alias was omitted, duplicated, or invalid item index was proven: %+v", s.Candidates[0].Navigation)
	}
}

func FuzzNavigationMetadata(f *testing.F) {
	f.Add(fixtureIndex(DiscIndex{FirstPlay: IndexEntry{ObjectType: 1}, TopMenu: IndexEntry{ObjectType: 1, ObjectID: 0xffff}}))
	f.Add(fixtureObjects([]NavigationCommand{playCommand(1)}))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 1<<20 {
			t.Skip()
		}
		ParseIndex(data)
		if objects, err := ParseMovieObjects(data); err == nil {
			AnalyzeNavigation(&DiscIndex{FirstPlay: IndexEntry{ObjectType: 1}, TopMenu: IndexEntry{ObjectType: 1, ObjectID: 0xffff}}, objects)
		}
	})
}
