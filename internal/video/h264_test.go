package video

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"os"
	"testing"
)

const originalSPS = "6764000aacd9420db011000003000100000300320f122596"

func fromHex(t testing.TB, value string) []byte {
	t.Helper()
	p, err := hex.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func fixture(t testing.TB) []byte {
	t.Helper()
	p, err := os.ReadFile("testdata/colour25.h264")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestH264ReferenceClocksAndUnchangedPictures(t *testing.T) {
	input := fixture(t)
	old := fromHex(t, originalSPS)
	for _, tc := range []struct {
		name string
		fps  float64
		sps  string
	}{
		{"25", 25, "6764000aacd9420db01100000303e80000c3500f122596"},
		{"24", 24, "6764000aacd9420db01100000303e80000bb800f122596"},
		{"23976", 24000.0 / 1001, "6764000aacd9420db01100000303e90000bb800f122596"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			clock, err := H264ClockForFPS(tc.fps)
			if err != nil {
				t.Fatal(err)
			}
			expected := bytes.ReplaceAll(input, old, fromHex(t, tc.sps))
			for _, chunk := range []int{1, 2, 3, 7, 17, 257, 32768, len(input)} {
				var output bytes.Buffer
				w, err := NewH264Writer(&output, clock)
				if err != nil {
					t.Fatal(err)
				}
				for pos := 0; pos < len(input); pos += chunk {
					part := input[pos:min(pos+chunk, len(input))]
					if n, err := w.Write(part); err != nil || n != len(part) {
						t.Fatalf("chunk %d: Write = %d, %v", chunk, n, err)
					}
				}
				if err := w.Finish(); err != nil {
					t.Fatalf("chunk %d: %v", chunk, err)
				}
				if err := w.Finish(); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(output.Bytes(), expected) {
					t.Fatalf("chunk %d changed bytes outside the expected SPS clocks", chunk)
				}
				stats := w.Stats()
				if stats.SPS != 2 || stats.ChangedSPS != 2 || stats.AddedTimingSPS != 0 || stats.VCLNALUnits != 50 || stats.NALUnits != 105 {
					t.Fatalf("unexpected counts: %+v", stats)
				}
				if stats.BytesRead != int64(len(input)) || stats.BytesWritten != int64(len(expected)) || stats.FirstSPS.Clock != (H264Clock{1, 50}) || stats.FirstSPS.FixedFrameRate {
					t.Fatalf("unexpected original timing or byte counts: %+v", stats)
				}
				if _, err := w.Write([]byte{1}); !errors.Is(err, ErrFinished) {
					t.Fatalf("write after finish: %v", err)
				}
			}
		})
	}
}

func withoutAUD(input []byte) []byte {
	var starts []int
	for i := 0; i+3 <= len(input); i++ {
		if i+4 <= len(input) && bytes.Equal(input[i:i+4], []byte{0, 0, 0, 1}) {
			starts = append(starts, i)
			i += 3
		} else if bytes.Equal(input[i:i+3], []byte{0, 0, 1}) {
			starts = append(starts, i)
			i += 2
		}
	}
	var result []byte
	for i, start := range starts {
		end := len(input)
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		header := start + 3
		if input[start+2] == 0 {
			header++
		}
		if input[header]&31 != 9 {
			result = append(result, input[start:end]...)
		}
	}
	return result
}

func TestH264WithoutAUDAndAnnexBPadding(t *testing.T) {
	input := append([]byte{0, 0, 0, 0, 0}, withoutAUD(fixture(t))...)
	input = append(input, make([]byte, 1000)...)
	clock := H264Clock{1000, 48000}
	var output bytes.Buffer
	stats, err := RewriteH264(bytes.NewReader(input), &output, clock)
	if err != nil {
		t.Fatal(err)
	}
	expected := bytes.ReplaceAll(input, fromHex(t, originalSPS), fromHex(t, "6764000aacd9420db01100000303e80000bb800f122596"))
	if !bytes.Equal(output.Bytes(), expected) || stats.NALUnits != 55 || stats.VCLNALUnits != 50 {
		t.Fatalf("delimiter-free stream or padding changed: %+v", stats)
	}
}

func TestH264ClockRates(t *testing.T) {
	for _, tc := range []struct {
		fps   float64
		clock H264Clock
	}{
		{23.976, H264Clock{1001, 48000}}, {24, H264Clock{1000, 48000}},
		{24.975, H264Clock{1001, 50000}}, {25, H264Clock{1000, 50000}},
		{29.970, H264Clock{1001, 60000}}, {30, H264Clock{1000, 60000}},
		{47.952, H264Clock{1001, 96000}}, {48, H264Clock{1000, 96000}},
		{50, H264Clock{1000, 100000}}, {59.940, H264Clock{1001, 120000}}, {60, H264Clock{1000, 120000}},
	} {
		got, err := H264ClockForFPS(tc.fps)
		if err != nil || got != tc.clock || math.Abs(got.FPS()-tc.fps) > 0.0001 {
			t.Fatalf("%g: %+v, %v", tc.fps, got, err)
		}
	}
	for _, rate := range []float64{0, -24, 27, 120, math.Inf(1), math.NaN()} {
		if _, err := H264ClockForFPS(rate); err == nil {
			t.Fatalf("accepted rate %g", rate)
		}
	}
}

func TestH264StreamFailuresAreSticky(t *testing.T) {
	sps := append([]byte{0, 0, 1}, fromHex(t, originalSPS)...)
	cases := map[string][]byte{
		"empty":          nil,
		"AVCC":           {0, 0, 0, 24, 0x67, 1},
		"slice-first":    {0, 0, 1, 0x65, 0x80},
		"no-SPS":         {0, 0, 1, 9, 0xf0},
		"empty-NAL":      append(append([]byte{}, sps...), 0, 0, 1),
		"duplicate-sync": {0, 0, 1, 0, 0, 1, 9, 0xf0},
		"forbidden":      {0, 0, 1, 0xe7, 0x80},
		"subset-SPS":     {0, 0, 1, 0x6f, 0x80},
		"MVC-slice":      append(append([]byte{}, sps...), 0, 0, 1, 0x74, 0x80),
		"large-SPS":      append([]byte{0, 0, 1, 0x67}, bytes.Repeat([]byte{1}, MaxH264SPSSize)...),
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			w, _ := NewH264Writer(io.Discard, H264Clock{1000, 48000})
			_, err := w.Write(input)
			if err == nil {
				err = w.Finish()
			}
			if err == nil {
				t.Fatal("accepted invalid stream")
			}
			if w.Finish() != err {
				t.Fatal("Finish lost the original error")
			}
			if n, later := w.Write([]byte{1}); n != 0 || later != err {
				t.Fatal("Write lost the original error")
			}
		})
	}
}

type failingWriter struct{ err error }

func (w failingWriter) Write(p []byte) (int, error) { return len(p) / 2, w.err }

type readError struct{}

func (readError) Read([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestH264EndpointErrors(t *testing.T) {
	input := fixture(t)
	for _, err := range []error{nil, io.ErrClosedPipe} {
		_, got := RewriteH264(bytes.NewReader(input), failingWriter{err}, H264Clock{1000, 48000})
		if err == nil {
			err = io.ErrShortWrite
		}
		if !errors.Is(got, err) {
			t.Fatalf("lost writer error: %v", got)
		}
	}
	if _, err := RewriteH264(readError{}, io.Discard, H264Clock{1000, 48000}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("lost reader error: %v", err)
	}
	if _, err := NewH264Writer(nil, H264Clock{1000, 48000}); err == nil {
		t.Fatal("accepted nil destination")
	}
	if _, err := NewH264Writer(io.Discard, H264Clock{}); err == nil {
		t.Fatal("accepted zero clock")
	}
}

func TestH264LargeSliceDoesNotAccumulate(t *testing.T) {
	w, _ := NewH264Writer(io.Discard, H264Clock{1000, 48000})
	header := append([]byte{0, 0, 1}, fromHex(t, originalSPS)...)
	header = append(header, 0, 0, 1, 0x65)
	if _, err := w.Write(header); err != nil {
		t.Fatal(err)
	}
	part := bytes.Repeat([]byte{0x55}, 32768)
	for i := 0; i < 1024; i++ {
		if _, err := w.Write(part); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Finish(); err != nil {
		t.Fatal(err)
	}
	if cap(w.sps) > MaxH264SPSSize || w.Stats().VCLNALUnits != 1 || w.Stats().BytesRead != int64(len(header)+len(part)*1024) {
		t.Fatal("large slice changed bounded buffering or accounting")
	}
}

func BenchmarkH264Rewrite(b *testing.B) {
	input := fixture(b)
	b.SetBytes(int64(len(input)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := RewriteH264(bytes.NewReader(input), io.Discard, H264Clock{1000, 48000}); err != nil {
			b.Fatal(err)
		}
	}
}

func FuzzH264Writer(f *testing.F) {
	seed := append([]byte{0, 0, 0, 1}, fromHex(f, originalSPS)...)
	seed = append(seed, 0, 0, 1, 9, 0xf0, 0, 0, 1, 0x65, 0x88, 0x84, 0, 0, 3, 0, 0x80, 0, 0)
	f.Add(seed, uint8(17))
	f.Add([]byte{0, 0, 1, 0x67, 0x80}, uint8(1))
	f.Fuzz(func(t *testing.T, input []byte, size uint8) {
		if len(input) > 1<<20 {
			t.Skip()
		}
		var a, b bytes.Buffer
		_, wholeErr := RewriteH264(bytes.NewReader(input), &a, H264Clock{1001, 48000})
		w, _ := NewH264Writer(&b, H264Clock{1001, 48000})
		var splitErr error
		chunk := int(size) + 1
		for pos := 0; pos < len(input); pos += chunk {
			if _, splitErr = w.Write(input[pos:min(pos+chunk, len(input))]); splitErr != nil {
				break
			}
		}
		if splitErr == nil {
			splitErr = w.Finish()
		}
		if (wholeErr == nil) != (splitErr == nil) || wholeErr == nil && !bytes.Equal(a.Bytes(), b.Bytes()) {
			t.Fatalf("chunk-dependent result: whole=%v split=%v", wholeErr, splitErr)
		}
		if cap(w.sps) > 2*MaxH264SPSSize {
			t.Fatal("SPS allocation escaped its bound")
		}
		if wholeErr == nil && w.Stats().SPS == 0 {
			t.Fatal("success without timing edit")
		}
	})
}
