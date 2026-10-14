package flaccheck

import (
	"crypto/md5"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
	"testing"
)

func requireVerifier(t testing.TB, info Info) *Verifier {
	t.Helper()
	v, err := NewVerifier(info)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func normalized(samples []int32, bits int) []float64 {
	out := make([]float64, len(samples))
	for i, sample := range samples {
		out[i] = math.Ldexp(float64(sample), 1-bits)
	}
	return out
}

func leftAligned(samples []int32, bits int) []byte {
	out := make([]byte, len(samples)*4)
	for i, sample := range samples {
		binary.LittleEndian.PutUint32(out[4*i:], uint32(sample)<<uint(32-bits))
	}
	return out
}

func TestPrecisionAndChunkFraming(t *testing.T) {
	for _, tc := range []struct {
		bits int
		pcm  string
	}{
		{4, "f8ff000107f9"},
		{8, "80ff00017f81"},
		{12, "00f8ffff00000100ff0701f8"},
		{16, "0080ffff00000100ff7f0180"},
		{20, "0000f8ffffff000000010000ffff070100f8"},
		{24, "000080ffffff000000010000ffff7f010080"},
		{32, "00000080ffffffff0000000001000000ffffff7f01000080"},
	} {
		t.Run(fmt.Sprintf("%dbit", tc.bits), func(t *testing.T) {
			limit := int64(1) << (tc.bits - 1)
			samples := []int32{int32(-limit), -1, 0, 1, int32(limit - 1), int32(1 - limit)}
			info := Info{SampleRate: 48000, Channels: 2, BitsPerSample: tc.bits, TotalFrames: 3, MD5: md5.Sum(decodeHex(t, tc.pcm))}
			floats, pcm := normalized(samples, tc.bits), leftAligned(samples, tc.bits)
			for chunk := 1; chunk <= 11; chunk++ {
				v := requireVerifier(t, info)
				for offset := 0; offset < len(floats); offset += chunk {
					if err := v.WriteF64(floats[offset:min(offset+chunk, len(floats))]); err != nil {
						t.Fatal(err)
					}
				}
				checkResult(t, v, info)
				v = requireVerifier(t, info)
				for offset := 0; offset < len(pcm); offset += chunk {
					part := pcm[offset:min(offset+chunk, len(pcm))]
					if n, err := v.WriteS32LE(part); n != len(part) || err != nil {
						t.Fatalf("chunk %d: wrote %d/%d bytes: %v", chunk, n, len(part), err)
					}
				}
				checkResult(t, v, info)
			}
		})
	}
}

func checkResult(t testing.TB, v *Verifier, info Info) {
	t.Helper()
	got, err := v.Finish()
	if err != nil {
		t.Fatal(err)
	}
	want := Result{Frames: info.TotalFrames, MD5: info.MD5, CountChecked: true, MD5Checked: true}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	if again, err := v.Finish(); err != nil || again != got {
		t.Fatalf("repeated Finish changed result: %+v, %v", again, err)
	}
	if err := v.WriteF64(nil); !errors.Is(err, ErrFinished) {
		t.Fatalf("write after Finish: %v", err)
	}
	if _, err := v.WriteS32LE(nil); !errors.Is(err, ErrFinished) {
		t.Fatalf("write after Finish: %v", err)
	}
}

func TestRFCChecksumExamples(t *testing.T) {
	for _, tc := range []struct {
		name     string
		bits     int
		channels int
		samples  []int32
		checksum string
	}{
		{"D.1", 16, 2, []int32{25588, 10416}, "3e84b41807dc690307586a3dad1a2e0f"},
		{"D.3", 8, 1, []int32{0, 79, 111, 78, 8, -61, -90, -68, -13, 42, 67, 53, 13, -27, -46, -38, -12, 14, 24, 19, 6, -4, -5, 0}, "f8f9e396f5cbcfc6dc807f9977906b32"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := Info{Channels: tc.channels, BitsPerSample: tc.bits, TotalFrames: uint64(len(tc.samples) / tc.channels)}
			copy(info.MD5[:], decodeHex(t, tc.checksum))
			v := requireVerifier(t, info)
			if err := v.WriteF64(normalized(tc.samples, tc.bits)); err != nil {
				t.Fatal(err)
			}
			checkResult(t, v, info)
		})
	}
}

func TestUnknownMetadata(t *testing.T) {
	pcm := []byte{0, 0, 1, 0, 255, 255}
	checksum := md5.Sum(pcm)
	for _, countKnown := range []bool{false, true} {
		for _, md5Known := range []bool{false, true} {
			info := Info{SampleRate: 48000, Channels: 1, BitsPerSample: 16}
			if countKnown {
				info.TotalFrames = 3
			}
			if md5Known {
				info.MD5 = checksum
			}
			v := requireVerifier(t, info)
			if err := v.WriteF64(normalized([]int32{0, 1, -1}, 16)); err != nil {
				t.Fatal(err)
			}
			got, err := v.Finish()
			if err != nil || got.Frames != 3 || got.MD5 != checksum || got.CountChecked != countKnown || got.MD5Checked != md5Known {
				t.Fatalf("count known %v, hash known %v: %+v, %v", countKnown, md5Known, got, err)
			}
		}
	}
	info := Info{Channels: 1, BitsPerSample: 4, MD5: md5.Sum(nil)}
	got, err := requireVerifier(t, info).Finish()
	if err != nil || got.Frames != 0 || !got.MD5Checked || got.CountChecked {
		t.Fatalf("empty stream: %+v, %v", got, err)
	}
}

func TestCorruption(t *testing.T) {
	for _, tc := range []struct {
		name    string
		info    Info
		samples []float64
		message string
	}{
		{"checksum", Info{Channels: 1, BitsPerSample: 16, MD5: md5.Sum([]byte{1, 0})}, []float64{0}, "MD5 mismatch"},
		{"dropped frame", Info{Channels: 1, BitsPerSample: 16, TotalFrames: 2}, []float64{0}, "decoded 1 frames"},
		{"additional frame", Info{Channels: 1, BitsPerSample: 16, TotalFrames: 1}, []float64{0, 0}, "more than"},
		{"partial stereo frame", Info{Channels: 2, BitsPerSample: 16}, []float64{0}, "incomplete channel frame"},
		{"fractional", Info{Channels: 1, BitsPerSample: 16}, []float64{1.0 / 65536}, "integer grid"},
		{"positive overflow", Info{Channels: 1, BitsPerSample: 32}, []float64{1}, "exceeds"},
		{"negative overflow", Info{Channels: 1, BitsPerSample: 32}, []float64{math.Nextafter(-1, -2)}, "exceeds"},
		{"NaN", Info{Channels: 1, BitsPerSample: 24}, []float64{math.NaN()}, "non-finite"},
		{"positive infinity", Info{Channels: 1, BitsPerSample: 24}, []float64{math.Inf(1)}, "non-finite"},
		{"negative infinity", Info{Channels: 1, BitsPerSample: 24}, []float64{math.Inf(-1)}, "non-finite"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := requireVerifier(t, tc.info)
			writeErr := v.WriteF64(tc.samples)
			_, err := v.Finish()
			if !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("unexpected error: %v", err)
			}
			if writeErr != nil && writeErr != err {
				t.Fatalf("write error was lost: %v => %v", writeErr, err)
			}
			if _, again := v.Finish(); again != err {
				t.Fatalf("repeated Finish changed error: %v => %v", err, again)
			}
		})
	}
}

func TestPartialS32AndPadding(t *testing.T) {
	for length := 1; length < 4; length++ {
		v := requireVerifier(t, Info{Channels: 1, BitsPerSample: 16})
		if n, err := v.WriteS32LE(make([]byte, length)); n != length || err != nil {
			t.Fatalf("partial write: %d, %v", n, err)
		}
		if _, err := v.Finish(); !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), "trailing bytes") {
			t.Fatalf("accepted %d trailing bytes: %v", length, err)
		}
	}
	for bits := 4; bits < 32; bits++ {
		v := requireVerifier(t, Info{Channels: 1, BitsPerSample: bits})
		if n, err := v.WriteS32LE([]byte{1, 0, 0, 0}); n != 4 || !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), "padding") {
			t.Fatalf("%d-bit padding accepted: %d, %v", bits, n, err)
		}
	}
	v := requireVerifier(t, Info{Channels: 1, BitsPerSample: 16})
	if n, err := v.WriteS32LE([]byte{0, 0, 0, 0, 1}); n != 5 || err != nil {
		t.Fatalf("prefix write: %d, %v", n, err)
	}
	if n, err := v.WriteS32LE([]byte{0, 0, 0, 0, 0, 0, 0}); n != 3 || !errors.Is(err, ErrCorrupt) {
		t.Fatalf("bad split sample consumed incorrect length: %d, %v", n, err)
	}
}

func TestVerifierFormatValidation(t *testing.T) {
	for _, info := range []Info{
		{Channels: 0, BitsPerSample: 16},
		{Channels: 9, BitsPerSample: 16},
		{Channels: 1, BitsPerSample: 3},
		{Channels: 1, BitsPerSample: 33},
		{Channels: 1, BitsPerSample: 16, SampleRate: -1},
		{Channels: 1, BitsPerSample: 16, SampleRate: 1 << 20},
		{Channels: 1, BitsPerSample: 16, TotalFrames: 1 << 36},
	} {
		if _, err := NewVerifier(info); err == nil {
			t.Fatalf("accepted invalid format %+v", info)
		}
	}
	for _, firstFloat := range []bool{false, true} {
		v := requireVerifier(t, Info{Channels: 1, BitsPerSample: 16})
		if err := v.WriteF64(nil); err != nil {
			t.Fatal(err)
		}
		if _, err := v.WriteS32LE(nil); err != nil {
			t.Fatal(err)
		}
		var err error
		if firstFloat {
			if err := v.WriteF64([]float64{0}); err != nil {
				t.Fatal(err)
			}
			_, err = v.WriteS32LE([]byte{0})
		} else {
			if _, err := v.WriteS32LE([]byte{0}); err != nil {
				t.Fatal(err)
			}
			err = v.WriteF64([]float64{0})
		}
		if !errors.Is(err, ErrInputFormat) {
			t.Fatalf("mixed format accepted: %v", err)
		}
		if _, finishErr := v.Finish(); finishErr != err {
			t.Fatalf("format error lost: %v => %v", err, finishErr)
		}
	}
}

func TestSampleCountOverflow(t *testing.T) {
	v := requireVerifier(t, Info{Channels: 1, BitsPerSample: 16})
	v.samples = math.MaxUint64
	if err := v.WriteF64([]float64{0}); !errors.Is(err, ErrCorrupt) || !strings.Contains(err.Error(), "overflow") {
		t.Fatalf("sample count wrapped: %v", err)
	}
	if v.samples != math.MaxUint64 {
		t.Fatal("sample count changed")
	}
}

func TestAllPrecisionsAndChannels(t *testing.T) {
	for bits := 4; bits <= 32; bits++ {
		for channels := 1; channels <= 8; channels++ {
			info := Info{Channels: channels, BitsPerSample: bits, TotalFrames: 3001}
			f64 := requireVerifier(t, info)
			s32 := requireVerifier(t, info)
			for i := 0; i < channels*int(info.TotalFrames); i++ {
				value := int32(i%13 - 6)
				if err := f64.WriteF64(normalized([]int32{value}, bits)); err != nil {
					t.Fatal(err)
				}
				if _, err := s32.WriteS32LE(leftAligned([]int32{value}, bits)); err != nil {
					t.Fatal(err)
				}
			}
			a, err := f64.Finish()
			if err != nil {
				t.Fatal(err)
			}
			b, err := s32.Finish()
			if err != nil || a != b || a.Frames != info.TotalFrames || a.MD5Checked || !a.CountChecked {
				t.Fatalf("%d-bit %d-channel mismatch: %+v / %+v; %v", bits, channels, a, b, err)
			}
		}
	}
}

func FuzzVerifierChunks(f *testing.F) {
	f.Add([]byte{0, 0, 0, 128, 255, 255, 255, 127}, uint8(28), uint8(1), uint8(2))
	f.Add([]byte{0, 0, 0, 0, 255, 255, 255, 255}, uint8(0), uint8(0), uint8(0))
	f.Add([]byte{}, uint8(20), uint8(7), uint8(255))
	f.Fuzz(func(t *testing.T, data []byte, depth, channelCount, split uint8) {
		if len(data) > 65536 {
			t.Skip()
		}
		bits, channels := 4+int(depth)%29, 1+int(channelCount)%8
		frames := len(data) / 4 / channels
		info := Info{Channels: channels, BitsPerSample: bits, TotalFrames: uint64(frames)}
		f64 := requireVerifier(t, info)
		s32 := requireVerifier(t, info)
		samples := make([]int32, frames*channels)
		for i := range samples {
			samples[i] = int32(binary.LittleEndian.Uint32(data[4*i:])) >> uint(32-bits)
		}
		if err := f64.WriteF64(normalized(samples, bits)); err != nil {
			t.Fatal(err)
		}
		pcm, chunk := leftAligned(samples, bits), 1+int(split)%64
		for offset := 0; offset < len(pcm); offset += chunk {
			part := pcm[offset:min(offset+chunk, len(pcm))]
			if n, err := s32.WriteS32LE(part); n != len(part) || err != nil {
				t.Fatalf("write %d/%d: %v", n, len(part), err)
			}
		}
		a, err := f64.Finish()
		if err != nil {
			t.Fatal(err)
		}
		b, err := s32.Finish()
		if err != nil || a != b || a.Frames != uint64(frames) {
			t.Fatalf("chunked output differs: %+v / %+v: %v", a, b, err)
		}
	})
}

func BenchmarkWriteF64(b *testing.B) {
	samples := make([]float64, 16384)
	for i := range samples {
		samples[i] = math.Ldexp(float64(i-8192), -23)
	}
	v := requireVerifier(b, Info{Channels: 2, BitsPerSample: 24})
	b.SetBytes(int64(len(samples) * 8))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := v.WriteF64(samples); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWriteS32LE(b *testing.B) {
	samples := make([]int32, 16384)
	for i := range samples {
		samples[i] = int32(i - 8192)
	}
	pcm := leftAligned(samples, 24)
	v := requireVerifier(b, Info{Channels: 2, BitsPerSample: 24})
	b.SetBytes(int64(len(pcm)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := v.WriteS32LE(pcm); err != nil {
			b.Fatal(err)
		}
	}
}
