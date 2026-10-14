package app

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestEAC3CoreFilenamePreservesInternalFormat(t *testing.T) {
	stream := &Stream{Codec: "eac3", Type: "audio", SampleRate: "48000", Channels: 2}
	track := Track{Number: 1, Kind: "audio", Stream: stream}
	for _, suffix := range []string{"ac3", "eac3"} {
		r := OutputRequest{Path: "core." + suffix, Options: AudioOptions{Core: true, KeepDialnorm: true, Speed: 1, ExtractChannel: -1}}
		out, err := makeOutput(&Source{}, track, r, Options{})
		if err != nil {
			t.Fatal(err)
		}
		if !out.Copy || out.Codec != "eac3" || out.Format != "eac3" || out.BSF != "eac3_core" {
			t.Fatalf(".%s core output would convert or mislabel its retained frames: %+v", suffix, out)
		}
	}
}

func TestEAC3CoreFilenameReferenceBytes(t *testing.T) {
	tools := mediaTools(t)
	source := filepath.Join("testdata", "core-independent-stereo48.eac3")
	want, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	// The generated input is also the complete .ac3 -core -keepDialnorm
	// output from eac3to 4.00. The generation recipe is in testdata/NOTICE.
	const referenceSHA = "4211435953c3e6ec024c99fce3251bada344b0a00d2a0245eeb1b1bbbb3322c2"
	if len(want) != 2304 || fmt.Sprintf("%x", sha256.Sum256(want)) != referenceSHA {
		t.Fatal("generated E-AC-3 reference fixture changed")
	}
	dir := t.TempDir()
	ac3, eac3 := filepath.Join(dir, "core.ac3"), filepath.Join(dir, "core.eac3")
	if _, err := runTest(t, source, "1:", ac3, "-core", "-keepDialnorm", "1:", eac3, "-core", "-keepDialnorm", "--ffmpeg="+tools.FFmpeg, "--ffprobe="+tools.FFprobe); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{ac3, eac3} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%s differs from the complete reference output: %d bytes versus %d", path, len(got), len(want))
		}
		if got[5]>>3 != 16 || got[2]>>6 != 0 {
			t.Fatal("independent E-AC-3 was converted to another bitstream")
		}
	}
}
