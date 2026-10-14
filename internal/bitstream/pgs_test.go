package bitstream

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"testing"
	"testing/iotest"
)

func supPacket(kind byte, pts uint32, p []byte) []byte {
	h := make([]byte, 13)
	copy(h, "PG")
	binary.BigEndian.PutUint32(h[2:6], pts)
	if pts > 90 {
		binary.BigEndian.PutUint32(h[6:10], pts-90)
	}
	h[10] = kind
	binary.BigEndian.PutUint16(h[11:13], uint16(len(p)))
	return append(h, p...)
}

func presentation(objects ...[]byte) []byte {
	p := []byte{0x07, 0x80, 0x04, 0x38, 0x10, 0, 1, 0, 0, 0, byte(len(objects))}
	for _, obj := range objects {
		p = append(p, obj...)
	}
	return p
}

func compositionObject(id uint16, flags byte) []byte {
	p := []byte{byte(id >> 8), byte(id), 0, flags, 0, 50, 0, 60}
	if flags&0x80 != 0 {
		p = append(p, 0, 0, 0, 0, 0, 1, 0, 1)
	}
	return p
}

func tinyObject(id uint16) []byte {
	// A two-pixel white row followed by the RLE end-of-line marker.
	return []byte{byte(id >> 8), byte(id), 0, 0xc0, 0, 0, 8, 0, 2, 0, 1, 1, 1, 0, 0}
}

func endPacket(pts uint32) []byte { return supPacket(pgsEnd, pts, nil) }

func TestForcedPGSMixedObjectsAndCrops(t *testing.T) {
	for _, firstFlags := range []byte{0, 0x40, 0x80, 0xc0} {
		for _, secondFlags := range []byte{0, 0x40, 0x80, 0xc0} {
			t.Run(fmt.Sprintf("%02x_%02x", firstFlags, secondFlags), func(t *testing.T) {
				objects := [][]byte{compositionObject(10, firstFlags), compositionObject(11, secondFlags)}
				var keep [][]byte
				for _, p := range objects {
					if p[3]&0x40 != 0 {
						keep = append(keep, p)
					}
				}
				input := append(supPacket(pgsPresentation, 1000, presentation(objects...)), endPacket(1000)...)
				want := append(supPacket(pgsPresentation, 1000, presentation(keep...)), endPacket(1000)...)
				var out bytes.Buffer
				stats, err := FilterForcedPGS(iotest.OneByteReader(bytes.NewReader(input)), &out)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(out.Bytes(), want) {
					t.Fatalf("unexpected output\n got %x\nwant %x", out.Bytes(), want)
				}
				if stats.Segments != 2 || stats.Presentations != 1 || stats.Objects != 2 || stats.ForcedObjects != int64(len(keep)) {
					t.Fatalf("bad stats %+v", stats)
				}
				if stats.BytesRead != int64(len(input)) || stats.BytesWritten != int64(len(want)) {
					t.Fatalf("bad byte counts %+v", stats)
				}
			})
		}
	}
}

func TestForcedPGSPreservesDependenciesAndClears(t *testing.T) {
	var input, want []byte
	appendBoth := func(p []byte) {
		input = append(input, p...)
		want = append(want, p...)
	}
	// An unforced display set initially defines the object. A later forced
	// presentation references that object with no new palette or bitmap.
	input = append(input, supPacket(pgsPresentation, 100, presentation(compositionObject(1, 0)))...)
	want = append(want, supPacket(pgsPresentation, 100, presentation())...)
	appendBoth(supPacket(pgsWindow, 100, []byte{1, 0, 0, 0, 0, 0, 0x07, 0x80, 0x04, 0x38}))
	appendBoth(supPacket(pgsPalette, 100, []byte{0, 0, 1, 235, 128, 128, 255}))
	first := tinyObject(1)
	first[3] = 0x80
	appendBoth(supPacket(pgsObject, 100, first[:12]))
	appendBoth(supPacket(pgsObject, 100, []byte{0, 1, 0, 0x40, 1, 0, 0}))
	appendBoth(endPacket(100))
	appendBoth(supPacket(pgsPresentation, 200, presentation(compositionObject(1, 0xc0))))
	appendBoth(endPacket(200))
	// A non-forced presentation must clear the previously visible forced one.
	input = append(input, supPacket(pgsPresentation, 300, presentation(compositionObject(1, 0)))...)
	want = append(want, supPacket(pgsPresentation, 300, presentation())...)
	appendBoth(endPacket(300))
	// The original clearing command remains present too.
	appendBoth(supPacket(pgsPresentation, 400, presentation()))
	appendBoth(endPacket(400))
	var out bytes.Buffer
	stats, err := FilterForcedPGS(bytes.NewReader(input), &out)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), want) {
		t.Fatal("dependencies, timing, or clearing commands changed")
	}
	if stats.ForcedObjects != 1 || stats.ForcedPresentations != 1 || stats.Presentations != 4 {
		t.Fatalf("bad stats %+v", stats)
	}
	var again bytes.Buffer
	if _, err := FilterForcedPGS(&out, &again); err != nil || !bytes.Equal(again.Bytes(), want) {
		t.Fatalf("filter is not idempotent: %v", err)
	}
}

func TestForcedPGSRejectsMalformed(t *testing.T) {
	pcs := supPacket(pgsPresentation, 100, presentation())
	invalidCrop := presentation(compositionObject(1, 0xc0))
	invalidCrop = invalidCrop[:len(invalidCrop)-1]
	badDimensions := presentation()
	badDimensions[0], badDimensions[1] = 0, 0
	shortObject := tinyObject(1)
	shortObject = shortObject[:len(shortObject)-1]
	cases := map[string][]byte{
		"empty":                 nil,
		"short header":          []byte("PG"),
		"bad magic":             bytes.Repeat([]byte{0}, 13),
		"truncated payload":     pcs[:len(pcs)-1],
		"missing END":           pcs,
		"END without PCS":       endPacket(10),
		"overlapping PCS":       append(append([]byte(nil), pcs...), pcs...),
		"short PCS":             supPacket(pgsPresentation, 10, []byte{1}),
		"truncated object ref":  supPacket(pgsPresentation, 10, append(presentation(), 1)),
		"truncated crop":        supPacket(pgsPresentation, 10, invalidCrop),
		"zero width":            supPacket(pgsPresentation, 10, badDimensions),
		"short palette":         supPacket(pgsPalette, 10, []byte{1}),
		"partial palette entry": supPacket(pgsPalette, 10, []byte{1, 0, 1}),
		"short window":          supPacket(pgsWindow, 10, []byte{1, 0}),
		"short ODS":             supPacket(pgsObject, 10, []byte{1}),
		"orphan ODS fragment":   supPacket(pgsObject, 10, []byte{0, 1, 0, 0x40, 0}),
		"short final fragment":  supPacket(pgsObject, 10, shortObject),
		"unknown segment":       supPacket(0xff, 10, nil),
		"nonempty END":          supPacket(pgsEnd, 10, []byte{0}),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := FilterForcedPGS(bytes.NewReader(input), io.Discard); err == nil {
				t.Fatal("accepted invalid SUP input")
			}
		})
	}
}

type shortWriter struct{}

func (shortWriter) Write(p []byte) (int, error) { return len(p) / 2, nil }

func TestForcedPGSIOErrors(t *testing.T) {
	input := append(supPacket(pgsPresentation, 10, presentation()), endPacket(10)...)
	if _, err := FilterForcedPGS(bytes.NewReader(input), shortWriter{}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("missing short-write error: %v", err)
	}
	want := errors.New("reader failed")
	if _, err := FilterForcedPGS(iotest.ErrReader(want), io.Discard); !errors.Is(err, want) {
		t.Fatalf("lost reader error: %v", err)
	}
}

func FuzzForcedPGS(f *testing.F) {
	f.Add(append(supPacket(pgsPresentation, 100, presentation(compositionObject(1, 0xc0))), endPacket(100)...))
	f.Add(supPacket(pgsObject, 100, tinyObject(1)))
	f.Fuzz(func(t *testing.T, input []byte) {
		// Besides panics, check successful streams always remain valid and
		// filtering a second time leaves precisely the same bytes.
		var once, twice bytes.Buffer
		if _, err := FilterForcedPGS(bytes.NewReader(input), &once); err == nil {
			if _, err := FilterForcedPGS(bytes.NewReader(once.Bytes()), &twice); err != nil {
				t.Fatalf("filtered output is invalid: %v", err)
			}
			if !bytes.Equal(once.Bytes(), twice.Bytes()) {
				t.Fatal("filter is not idempotent")
			}
		}
	})
}
