package compare

import (
	"bytes"
	"context"
	"encoding/binary"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestCompareReadersAcrossBlocksAndTail(t *testing.T) {
	const channels = 2
	var a []byte
	for i := 0; i < 20000; i++ {
		a = binary.LittleEndian.AppendUint32(a, uint32(i-10000))
	}
	b := append([]byte(nil), a...)
	changed := 4321*channels + 1
	binary.LittleEndian.PutUint32(b[changed*4:], uint32(int32(-10000+changed+123)))
	b = append(b, make([]byte, channels*4)...)
	zero := uint64(0)
	r := Report{MaxIntegerError: &zero}
	if err := compareReaders(bytes.NewReader(a), bytes.NewReader(b), channels, 4, &r); err != nil {
		t.Fatal(err)
	}
	if r.Exact || r.Reference.Frames != 10000 || r.Candidate.Frames != 10001 || r.DifferingSamples != 3 || r.MissingSamples != 2 || r.ComparedSamples != 20000 || *r.MaxIntegerError != 123 {
		t.Fatalf("bad report: %+v", r)
	}
	if r.FirstDifference.Frame != 4321 || r.FirstDifference.Channel != 1 {
		t.Fatal(r.FirstDifference)
	}
	if err := compareReaders(bytes.NewReader(a[:3]), bytes.NewReader(nil), 2, 4, &Report{}); err == nil {
		t.Fatal("accepted partial PCM frame")
	}
}

func TestCompareExtremeIntegersAndFloatBits(t *testing.T) {
	a := binary.LittleEndian.AppendUint32(nil, 0x80000000)
	b := binary.LittleEndian.AppendUint32(nil, 0x7fffffff)
	zero := uint64(0)
	r := Report{MaxIntegerError: &zero}
	if err := compareReaders(bytes.NewReader(a), bytes.NewReader(b), 1, 4, &r); err != nil {
		t.Fatal(err)
	}
	if *r.MaxIntegerError != 4294967295 {
		t.Fatal(*r.MaxIntegerError)
	}
	a = binary.LittleEndian.AppendUint64(nil, math.Float64bits(0))
	b = binary.LittleEndian.AppendUint64(nil, math.Float64bits(math.Copysign(0, -1)))
	r = Report{}
	if err := compareReaders(bytes.NewReader(a), bytes.NewReader(b), 1, 8, &r); err != nil {
		t.Fatal(err)
	}
	if r.Exact || r.DifferingSamples != 1 || *r.FirstDifference.CandidateValue != "-0" {
		t.Fatal(r)
	}
}

func TestRejectMetadataDifferences(t *testing.T) {
	a := AudioInfo{SampleRate: 48000, Channels: 2, ChannelLayout: "stereo", SampleFormat: "s32", Bits: 24}
	for _, change := range []func(*AudioInfo){
		func(b *AudioInfo) { b.SampleRate = 44100 },
		func(b *AudioInfo) { b.Channels = 1 },
		func(b *AudioInfo) { b.ChannelLayout = "" },
		func(b *AudioInfo) { b.Bits = 64; b.SampleFormat = "s64" },
	} {
		b := a
		change(&b)
		if err := compatible(a, b); err == nil {
			t.Fatal("accepted mismatched/unsupported metadata")
		}
	}
}

func writeWave(t *testing.T, path string, rate, bits, format int, data []byte) {
	t.Helper()
	const channels = 2
	var b []byte
	b = append(b, []byte("RIFF")...)
	b = binary.LittleEndian.AppendUint32(b, uint32(60+len(data)))
	b = append(b, []byte("WAVEfmt ")...)
	b = binary.LittleEndian.AppendUint32(b, 40)
	b = binary.LittleEndian.AppendUint16(b, 0xfffe)
	b = binary.LittleEndian.AppendUint16(b, channels)
	b = binary.LittleEndian.AppendUint32(b, uint32(rate))
	b = binary.LittleEndian.AppendUint32(b, uint32(rate*channels*bits/8))
	b = binary.LittleEndian.AppendUint16(b, uint16(channels*bits/8))
	b = binary.LittleEndian.AppendUint16(b, uint16(bits))
	b = binary.LittleEndian.AppendUint16(b, 22)
	b = binary.LittleEndian.AppendUint16(b, uint16(bits))
	b = binary.LittleEndian.AppendUint32(b, 3)
	b = binary.LittleEndian.AppendUint32(b, uint32(format))
	b = append(b, 0, 0, 0x10, 0, 0x80, 0, 0, 0xaa, 0, 0x38, 0x9b, 0x71)
	b = append(b, []byte("data")...)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(data)))
	b = append(b, data...)
	if err := os.WriteFile(path, b, 0644); err != nil {
		t.Fatal(err)
	}
}

func integrationTools(t *testing.T) (string, string) {
	t.Helper()
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("FFmpeg is unavailable")
	}
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe is unavailable")
	}
	return ffmpeg, ffprobe
}

func TestFilesLosslessContainersAndOneSample(t *testing.T) {
	ffmpeg, ffprobe := integrationTools(t)
	dir := t.TempDir()
	wav, flac, changed, wrongRate := filepath.Join(dir, "source.wav"), filepath.Join(dir, "candidate.flac"), filepath.Join(dir, "changed.wav"), filepath.Join(dir, "wrong-rate.wav")
	pcm := make([]byte, 5000*2*3)
	for i := 0; i < 10000; i++ {
		v := int32(i*31337%16777216 - 8388608)
		pcm[i*3], pcm[i*3+1], pcm[i*3+2] = byte(v), byte(v>>8), byte(v>>16)
	}
	writeWave(t, wav, 48000, 24, 1, pcm)
	command := exec.Command(ffmpeg, "-v", "error", "-nostdin", "-i", wav, "-c:a", "flac", flac)
	if out, err := command.CombinedOutput(); err != nil {
		t.Fatalf("fixture encode: %v %s", err, out)
	}
	options := Options{FFmpeg: ffmpeg, FFprobe: ffprobe}
	r, err := Files(context.Background(), wav, flac, options)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Exact || r.Reference.Frames != 5000 || r.Format != "s32le" || r.FFmpeg.Path == "" || r.FFprobe.Version == "" {
		t.Fatalf("lossless containers: %+v", r)
	}
	bad := append([]byte(nil), pcm...)
	pos := (4321*2 + 1) * 3
	bad[pos]++
	writeWave(t, changed, 48000, 24, 1, bad)
	r, err = Files(context.Background(), wav, changed, options)
	if err != nil {
		t.Fatal(err)
	}
	if r.Exact || r.DifferingSamples != 1 || r.FirstDifference.Frame != 4321 || r.FirstDifference.Channel != 1 || *r.MaxIntegerError != 256 {
		t.Fatalf("one 24-bit sample: %+v", r)
	}
	writeWave(t, wrongRate, 44100, 24, 1, pcm)
	if _, err = Files(context.Background(), wav, wrongRate, options); err == nil || !strings.Contains(err.Error(), "sample-rate mismatch") {
		t.Fatalf("rate mismatch: %v", err)
	}
}

func TestFilesPreserveFloatPrecision(t *testing.T) {
	ffmpeg, ffprobe := integrationTools(t)
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.wav"), filepath.Join(dir, "b.wav")
	var pcm []byte
	for i := 0; i < 256; i++ {
		pcm = binary.LittleEndian.AppendUint64(pcm, math.Float64bits(.125))
	}
	writeWave(t, a, 48000, 64, 3, pcm)
	changed := append([]byte(nil), pcm...)
	binary.LittleEndian.PutUint64(changed[10*8:], math.Float64bits(math.Nextafter(.125, 1)))
	writeWave(t, b, 48000, 64, 3, changed)
	r, err := Files(context.Background(), a, b, Options{FFmpeg: ffmpeg, FFprobe: ffprobe})
	if err != nil {
		t.Fatal(err)
	}
	if r.Format != "f64le" || r.DifferingSamples != 1 || r.FirstDifference.Frame != 5 || r.FirstDifference.Channel != 0 {
		t.Fatalf("float precision lost: %+v", r)
	}
}
