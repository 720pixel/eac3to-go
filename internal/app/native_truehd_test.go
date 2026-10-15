package app

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"eac3to/internal/aften"
	"eac3to/internal/bluray"
)

// Synthetic fixture provenance matches internal/truehd/parser_test.go:
// 4096 stereo signed24 frames at 48000 Hz, each sample
// ((frame*109 + channel*12345 + 567) % 8388608) - 4194304.
// ffmpeg -i synthetic.wav -c:a truehd -strict -2 -f truehd synthetic.thd
// These are the first three generated access units, not disc or executable data.
const truehdAppFixture = "e054ffd8f8726fba0050a001b752000000008c7f1014000000000000000034282043f1ea0000011000000017bdc0000000020c8204c0012f30004b80423600e4de010a8c0396e00431000e692010df4039db0043ea00e84601115c03a480044c400e9fa0114c403ab5244038c785b1f25b56be5b9c369bb72848ffdb6200001718f0b63e4b6ad7cb7386d376e5091ffb6c400002ffffffffffffffffffffffffffffffff8000990e70110000600e914080500bfffffffffffffffffffffffffffffffffffffffe00a251000f0028600c7fffffffffffffffffffffffffffffffffffffffc000e994"

func truehdAppBytes(t *testing.T) []byte {
	t.Helper()
	b, err := hex.DecodeString(truehdAppFixture)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Small payload packets force timestamps, the extension, and codec access units
// to cross transport-packet boundaries without inserting stuffing into audio.
func truehdPESPackets(pid uint16, sid byte, ext int, pts int64, payload []byte, cc *byte) []byte {
	pes := []byte{0, 0, 1, sid, 0, 0, 0x80, 0x80, 5}
	pes = append(pes, byte(pts>>29)&14|0x21, byte(pts>>22), byte(pts>>14)&0xfe|1, byte(pts>>7), byte(pts<<1)|1)
	if ext >= 0 {
		pes[7], pes[8] = 0x81, 8
		pes = append(pes, 1, 0x81, byte(ext))
	}
	pes = append(pes, payload...)
	binary.BigEndian.PutUint16(pes[4:6], uint16(len(pes)-6))
	var result []byte
	start := true
	for len(pes) > 0 {
		n := min(37, len(pes))
		p := bytes.Repeat([]byte{0xff}, 188)
		p[0], p[1], p[2], p[3] = 0x47, byte(pid>>8), byte(pid), 0x30|*cc
		if start {
			p[1] |= 0x40
		}
		p[4], p[5] = byte(183-n), 0
		copy(p[188-n:], pes[:n])
		result = append(result, p...)
		pes = pes[n:]
		*cc = (*cc + 1) & 15
		start = false
	}
	return result
}

func truehdAppSource(t *testing.T, clips int, delayMS int64) (*Source, []byte, []byte) {
	t.Helper()
	dir := t.TempDir()
	streams := filepath.Join(dir, "BDMV", "STREAM")
	if err := os.MkdirAll(streams, 0755); err != nil {
		t.Fatal(err)
	}
	thd := truehdAppBytes(t)
	core, err := os.ReadFile(filepath.Join("..", "aften", "testdata", "stereo48_192.ac3"))
	if err != nil {
		t.Fatal(err)
	}
	s := &Source{Probe: Probe{Streams: []Stream{
		{Index: 0, Codec: "h264", Type: "video", ID: "0x1011"},
		{Index: 1, Codec: "truehd", Type: "audio", ID: "0x1100", Channels: 2, SampleRate: "48000", Layout: "stereo"},
		{Index: 2, Codec: "ac3", Type: "audio", ID: "0x1100", Channels: 2, SampleRate: "48000", Layout: "stereo"},
	}}}
	for i := 0; i < clips; i++ {
		var cc, vc byte
		var packets []byte
		packets = append(packets, truehdPESPackets(0x1011, 0xe0, -1, 900000, []byte{0, 0, 1, 9, 0xf0}, &vc)...)
		packets = append(packets, truehdPESPackets(0x1100, 0xfd, 0x72, 900000+delayMS*90, thd, &cc)...)
		for j := 0; j < 3; j++ {
			packets = append(packets, truehdPESPackets(0x1100, 0xfd, 0x76, 900000+delayMS*90+int64(j)*2880, core, &cc)...)
		}
		packets = append(packets, truehdPESPackets(0x1011, 0xe0, -1, 990000, []byte{0, 0, 1, 9, 0xf0}, &vc)...)
		path := filepath.Join(streams, fmt.Sprintf("%05d.m2ts", i))
		if err := os.WriteFile(path, packets, 0600); err != nil {
			t.Fatal(err)
		}
		s.Paths = append(s.Paths, path)
	}
	if clips > 1 {
		s.Playlist = &bluray.Playlist{Path: filepath.Join(dir, "BDMV", "PLAYLIST", "00000.mpls")}
		for i := 0; i < clips; i++ {
			s.Playlist.Items = append(s.Playlist.Items, bluray.PlayItem{ClipID: fmt.Sprintf("%05d", i), CodecID: "M2TS", InTime: 450000, OutTime: 495000, Streams: []bluray.Stream{{EntryType: 1, Kind: "video", PID: 0x1011}, {EntryType: 1, Kind: "audio", PID: 0x1100, CodingType: 0x83}}})
		}
	} else {
		s.Probe.Format.Name = "mpegts"
	}
	for i := range s.Probe.Streams {
		st := &s.Probe.Streams[i]
		s.Tracks = append(s.Tracks, Track{Number: i + 1, Kind: st.Type, Stream: st})
	}
	return s, thd, core
}

func truehdOutputOptions(track int, path string, keep bool, delay float64, core bool) OutputRequest {
	return OutputRequest{Track: track, Path: path, Options: AudioOptions{Speed: 1, ExtractChannel: -1, KeepDialnorm: keep, DelayMS: delay, Core: core}}
}

func TestNativeTrueHDSharedPIDOutputAndDelaySum(t *testing.T) {
	s, thd, core := truehdAppSource(t, 1, 7)
	dir := t.TempDir()
	o := Options{Quiet: true, Outputs: []OutputRequest{
		truehdOutputOptions(2, filepath.Join(dir, "kept.thd"), true, -7, false),
		truehdOutputOptions(2, filepath.Join(dir, "neutral.thd"), false, 0, false),
		truehdOutputOptions(2, filepath.Join(dir, "companion.ac3"), true, -39, true),
	}}
	outputs, err := buildOutputs(s, o)
	if err != nil {
		t.Fatal(err)
	}
	outputs, err = prepareTrueHDOutputs(context.Background(), s, outputs, o)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(outputs[0].Path) != "kept.thd" || filepath.Base(outputs[1].Path) != "neutral DELAY 7ms.thd" {
		t.Fatal("source/manual metadata delay not summed before naming")
	}
	cache := s.nativeTiming
	if err = execute(context.Background(), Tools{}, s, outputs, o, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	if s.nativeTiming != cache {
		t.Fatal("startup probe was repeated")
	}
	got, err := os.ReadFile(outputs[0].Path)
	if err != nil || !bytes.Equal(got, thd) {
		t.Fatalf("TrueHD payload changed: %v", err)
	}
	want := bytes.Clone(thd)
	header, _ := hex.DecodeString("f8726fba0050a001b752000000008c7f101400004a07c007c0006b8b")
	copy(want[4:32], header)
	got, err = os.ReadFile(outputs[1].Path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("TrueHD normalization differs: %v", err)
	}
	got, err = os.ReadFile(outputs[2].Path)
	if err != nil || !bytes.Equal(got, bytes.Repeat(core, 2)) {
		t.Fatalf("companion source7 + manual-39 did not remove exactly one frame: %v", err)
	}
}

func TestNativeTrueHDCompanionDoesNotDeleteBoundaryDuplicate(t *testing.T) {
	s, _, core := truehdAppSource(t, 2, 0)
	path := filepath.Join(t.TempDir(), "companion.ac3")
	o := Options{AllowUnverified: true, Quiet: true, Outputs: []OutputRequest{truehdOutputOptions(2, path, true, 0, true)}}
	outputs, err := buildOutputs(s, o)
	if err != nil {
		t.Fatal(err)
	}
	if err = execute(context.Background(), Tools{}, s, outputs, o, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, bytes.Repeat(core, 6)) {
		t.Fatal("ordinary AC-3 boundary overlap was applied to the TrueHD companion")
	}
}

func TestNativeTrueHDAndEnhancedCompanionAreSeparatePayloads(t *testing.T) {
	s, thd, _ := truehdAppSource(t, 1, 0)
	s.Probe.Streams[2].Codec = "eac3"
	companion, err := os.ReadFile(filepath.Join("testdata", "core-independent-stereo48.eac3"))
	if err != nil {
		t.Fatal(err)
	}
	var vc, cc byte
	packets := truehdPESPackets(0x1011, 0xe0, -1, 900000, []byte{0, 0, 1, 9, 0xf0}, &vc)
	packets = append(packets, truehdPESPackets(0x1100, 0xfd, 0x72, 900000, thd, &cc)...)
	packets = append(packets, truehdPESPackets(0x1100, 0xfd, 0x76, 900000, companion, &cc)...)
	packets = append(packets, truehdPESPackets(0x1011, 0xe0, -1, 990000, []byte{0, 0, 1, 9, 0xf0}, &vc)...)
	if err := os.WriteFile(s.Paths[0], packets, 0600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	main, core := filepath.Join(dir, "main.thd"), filepath.Join(dir, "compatibility.eac3")
	o := Options{Quiet: true, Outputs: []OutputRequest{
		truehdOutputOptions(2, main, true, 0, false),
		truehdOutputOptions(2, core, true, 0, true),
	}}
	outputs, err := buildOutputs(s, o)
	if err != nil {
		t.Fatal(err)
	}
	if err := execute(context.Background(), Tools{}, s, outputs, o, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string][]byte{main: thd, core: companion} {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("separate output %s did not preserve its own payload: %v", filepath.Base(path), err)
		}
	}
	bad := truehdOutputOptions(2, filepath.Join(dir, "misnamed.ac3"), true, 0, true)
	if _, err := makeOutput(s, s.Tracks[1], bad, Options{}); err == nil {
		t.Fatal("E-AC-3 companion was mislabeled as AC-3")
	}
}

func TestPlaylistVideoWindowPreservesSeparateTrueHDAndAC3(t *testing.T) {
	s, thd, core := truehdAppSource(t, 1, 0)
	root := filepath.Dir(filepath.Dir(filepath.Dir(s.Paths[0])))
	s.Playlist = &bluray.Playlist{Path: filepath.Join(root, "BDMV", "PLAYLIST", "00000.mpls"), Items: []bluray.PlayItem{{ClipID: "00000", CodecID: "M2TS", InTime: 450000, OutTime: 450450, Streams: []bluray.Stream{{EntryType: 1, Kind: "video", PID: 0x1011}, {EntryType: 1, Kind: "audio", PID: 0x1100}}}}}
	dir := t.TempDir()
	video := Output{Track: s.Tracks[0], Copy: true, Format: "h264", Codec: "h264", Path: filepath.Join(dir, "video.h264"), Stage: filepath.Join(dir, "video.h264")}
	o := Options{Quiet: true, PlaylistVideoWindow: true, Outputs: []OutputRequest{
		truehdOutputOptions(2, filepath.Join(dir, "audio.thd"), true, 0, false),
		truehdOutputOptions(2, filepath.Join(dir, "audio.ac3"), true, 0, true),
	}}
	outputs, err := buildOutputs(s, o)
	if err != nil {
		t.Fatal(err)
	}
	for i := range outputs {
		outputs[i].Stage = outputs[i].Path
	}
	outputs = append(outputs, video)
	if _, err := copyNativeStreams(context.Background(), s, outputs, o, io.Discard); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string][]byte{video.Path: {0, 0, 1, 9, 0xf0}, outputs[0].Path: thd, outputs[1].Path: bytes.Repeat(core, 3)} {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("video-only window altered %s: %d bytes, want %d; %v", filepath.Base(path), len(got), len(want), err)
		}
	}
}

func TestNativeTrueHDCompanionPaddingUsesAC3Policy(t *testing.T) {
	if !aften.Available() {
		t.Skip("native AC3 padding requires Cgo")
	}
	s, _, core := truehdAppSource(t, 1, 0)
	path := filepath.Join(t.TempDir(), "core.ac3")
	o := Options{Quiet: true, Outputs: []OutputRequest{truehdOutputOptions(2, path, true, 32, true)}}
	outputs, err := buildOutputs(s, o)
	if err != nil {
		t.Fatal(err)
	}
	if err = execute(context.Background(), Tools{}, s, outputs, o, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	want, err := aften.SilenceFromAC3Frame(core)
	if err != nil {
		t.Fatal(err)
	}
	want = append(want, bytes.Repeat(core, 3)...)
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatal("companion +32ms did not insert one AC3 silence frame")
	}
}

func TestTrueHDDelayPathsValidatedBeforeStaging(t *testing.T) {
	for _, kind := range []string{"existing", "duplicate", "input", "hardlink"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, "source.thd")
			if err := os.WriteFile(source, truehdAppBytes(t), 0600); err != nil {
				t.Fatal(err)
			}
			stream := Stream{Codec: "truehd", Type: "audio"}
			s := &Source{Paths: []string{source}, Tracks: []Track{{Number: 1, Kind: "audio", Stream: &stream}}}
			s.Probe.Streams = []Stream{stream}
			s.Probe.Format.Name = "truehd"
			path := filepath.Join(dir, "new", "out.thd")
			o := Options{Outputs: []OutputRequest{truehdOutputOptions(1, path, true, 32, false)}}
			target := filepath.Join(dir, "new", "out DELAY 32ms.thd")
			switch kind {
			case "existing":
				if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(target, []byte("existing"), 0600); err != nil {
					t.Fatal(err)
				}
			case "duplicate":
				o.Outputs = append(o.Outputs, truehdOutputOptions(1, target, true, 0, false))
			case "input":
				o.Outputs[0] = truehdOutputOptions(1, source, true, 0, false)
				o.Force = true
			case "hardlink":
				if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.Link(source, target); err != nil {
					t.Fatal(err)
				}
				o.Force = true
			}
			outputs, err := buildOutputs(s, o)
			if err != nil {
				t.Fatal(err)
			}
			if err = execute(context.Background(), Tools{}, s, outputs, o, io.Discard, io.Discard); err == nil {
				t.Fatal("unsafe final TrueHD destination accepted")
			}
			entries, err := os.ReadDir(filepath.Dir(source))
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".eac3to-") {
					t.Fatal("staging created before path validation")
				}
			}
			b, err := os.ReadFile(source)
			if err != nil || !bytes.Equal(b, truehdAppBytes(t)) {
				t.Fatal("source bytes changed")
			}
			if kind == "duplicate" {
				if _, err = os.Stat(filepath.Dir(path)); !os.IsNotExist(err) {
					t.Fatal("destination directory created before duplicate validation")
				}
			}
		})
	}
}

func TestElementaryTrueHDCopyDelayAndDamage(t *testing.T) {
	for _, damaged := range []bool{false, true} {
		t.Run(fmt.Sprint(damaged), func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "source.thd")
			input := truehdAppBytes(t)
			if damaged {
				input = input[:len(input)-1]
			}
			if err := os.WriteFile(path, input, 0600); err != nil {
				t.Fatal(err)
			}
			st := Stream{Codec: "truehd", Type: "audio"}
			s := &Source{Paths: []string{path}, Probe: Probe{Streams: []Stream{st}}, Tracks: []Track{{Number: 1, Kind: "audio", Stream: &st}}}
			s.Probe.Format.Name = "truehd"
			out := filepath.Join(dir, "result.thd")
			o := Options{Quiet: true, Outputs: []OutputRequest{truehdOutputOptions(1, out, true, 32, false)}}
			outputs, err := buildOutputs(s, o)
			if err != nil {
				t.Fatal(err)
			}
			err = execute(context.Background(), Tools{}, s, outputs, o, io.Discard, io.Discard)
			final := filepath.Join(dir, "result DELAY 32ms.thd")
			if damaged {
				if err == nil {
					t.Fatal("truncated TrueHD published")
				}
				if _, e := os.Stat(final); !os.IsNotExist(e) {
					t.Fatal("failed output was published")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				got, e := os.ReadFile(final)
				if e != nil || !bytes.Equal(got, input) {
					t.Fatal("metadata delay changed elementary payload")
				}
			}
		})
	}
}

func TestTrueHDDryRunShowsFinalPathWithoutDirectories(t *testing.T) {
	tools := mediaTools(t)
	dir := t.TempDir()
	source := filepath.Join(dir, "source.thd")
	// The three-unit fixture is too short for FFprobe's raw-format detection.
	wav, _ := waveFixture(t, dir)
	mediaCommand(t, tools.FFmpeg, "-v", "error", "-i", wav, "-c:a", "truehd", "-strict", "-2", "-f", "truehd", source)
	out := filepath.Join(dir, "new", "out.thd")
	var stdout, stderr bytes.Buffer
	err := Run(context.Background(), []string{source, out, "+32ms", "-keepDialnorm", "--dry-run", "--ffmpeg=" + tools.FFmpeg, "--ffprobe=" + tools.FFprobe}, &stdout, &stderr)
	if err != nil {
		t.Fatalf("dry-run: %v\n%s", err, stderr.String())
	}
	var steps []struct {
		Output    string `json:"output"`
		Operation string `json:"operation"`
	}
	if err = json.Unmarshal(stdout.Bytes(), &steps); err != nil {
		t.Fatal(err)
	}
	if len(steps) != 1 || steps[0].Output != filepath.Join(dir, "new", "out DELAY 32ms.thd") || steps[0].Operation != "stream copy" {
		t.Fatalf("incorrect TrueHD dry-run: %s", stdout.String())
	}
	if _, err = os.Stat(filepath.Dir(out)); !os.IsNotExist(err) {
		t.Fatal("dry-run created destination directories")
	}
}

func TestTrueHDPlaylistCopiesAndRemainingQualityGates(t *testing.T) {
	s, _, _ := truehdAppSource(t, 2, 0)
	for _, core := range []bool{false, true} {
		path := filepath.Join(t.TempDir(), "out.thd")
		if core {
			path = filepath.Join(t.TempDir(), "out.ac3")
		}
		o := Options{Outputs: []OutputRequest{truehdOutputOptions(2, path, true, 0, core)}}
		outputs, err := buildOutputs(s, o)
		if err != nil {
			t.Fatal(err)
		}
		if err = validateParityPaths(s, outputs, o); err != nil {
			t.Fatal("validated TrueHD playlist copying was blocked", err)
		}
		if err = validateParityPaths(s, outputs, Options{AllowUnverified: true}); err != nil {
			t.Fatal(err)
		}
	}
	s.Playlist = nil
	if err := validateParityPaths(s, []Output{{Track: s.Tracks[1], Copy: true, Codec: "truehd", Format: "truehd"}}, Options{}); err == nil {
		t.Fatal("unvalidated physical joins bypassed their quality gate")
	}
	s.Playlist = &bluray.Playlist{Items: []bluray.PlayItem{{}}}
	s.Playlist.Items = s.Playlist.Items[:1]
	out := Output{Track: s.Tracks[1], Copy: true, Codec: "truehd", Format: "truehd"}
	if err := validateParityPaths(s, []Output{out}, Options{}); err != nil {
		t.Fatal("proven single-clip path blocked")
	}
	if err := validateParityPaths(s, []Output{out}, Options{StrictPL: true}); err == nil {
		t.Fatal("unverified strict start accepted")
	}
}
