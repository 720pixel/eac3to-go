package app

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParallelAudioKeepsSamplesAndCleansCaches(t *testing.T) {
	tools := mediaTools(t)
	dir := t.TempDir()
	wav, want := waveFixture(t, dir)
	input := filepath.Join(dir, "two tracks.mkv")
	mediaCommand(t, tools.FFmpeg, "-v", "error", "-i", wav, "-map", "0:a", "-map", "0:a", "-c", "copy", input)
	a := filepath.Join(dir, "first.flac")
	b := filepath.Join(dir, "second.flac")
	c := filepath.Join(dir, "duplicate.wav")
	var log bytes.Buffer
	err := Run(context.Background(), []string{input, "1:", a, "2:", b, "1:", c, "--jobs=3", "--threads=6"}, &bytes.Buffer{}, &log)
	if err != nil {
		t.Fatalf("%v\n%s", err, &log)
	}
	for _, path := range []string{a, b, c} {
		got := mediaCommand(t, tools.FFmpeg, "-v", "error", "-i", path, "-c:a", "pcm_s16le", "-f", "s16le", "-")
		if !bytes.Equal(got, want) {
			t.Fatalf("samples changed in %s", path)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".eac3to-") {
			t.Fatalf("cache directory leaked: %s", entry.Name())
		}
	}
}

func TestConcurrencyBudget(t *testing.T) {
	for _, tc := range []struct {
		o                    Options
		count, jobs, threads int
	}{
		{Options{Threads: 12}, 9, 4, 1},
		{Options{Threads: 24, Jobs: 2}, 9, 2, 4},
		{Options{Threads: 1, Jobs: 8}, 3, 1, 1},
		{Options{Threads: 12}, 1, 1, 4},
	} {
		jobs, threads := concurrency(tc.o, tc.count)
		if jobs != tc.jobs || threads != tc.threads {
			t.Fatalf("%+v: jobs=%d threads=%d", tc, jobs, threads)
		}
	}
}

func TestTitleAndParallelOptions(t *testing.T) {
	for _, args := range [][]string{
		{"disc", "--main", "--playlist=00001"},
		{"disc", "1)", "--main"},
		{"disc", "--playlist=100000"},
		{"disc", "--jobs=0"},
		{"disc", "--threads=-1"},
	} {
		if _, err := ParseOptions(args); err == nil {
			t.Fatalf("accepted %q", args)
		}
	}
	o, err := ParseOptions([]string{"disc", "--playlist=1", "--jobs=2", "--threads=6"})
	if err != nil || o.Playlist != "00001.mpls" || o.Jobs != 2 || o.Threads != 6 {
		t.Fatalf("%+v, %v", o, err)
	}
}

func TestAudioOutputCannotAliasItsCache(t *testing.T) {
	tools := mediaTools(t)
	dir := t.TempDir()
	source := filepath.Join(dir, "tone.wav")
	mediaCommand(t, tools.FFmpeg, "-v", "error", "-f", "lavfi", "-i", "sine=frequency=997:sample_rate=48000", "-t", "30", "-ac", "2", "-c:a", "pcm_s16le", source)
	processed := filepath.Join(dir, "source-0.mka")
	unchanged := filepath.Join(dir, "other.wav")
	if _, err := runTest(t, source, "1:", processed, "+3dB", "1:", unchanged); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{processed, unchanged} {
		data := mediaCommand(t, tools.FFmpeg, "-v", "error", "-xerror", "-i", path, "-c:a", "pcm_s16le", "-f", "s16le", "-")
		if len(data) != 30*48000*2*2 {
			t.Fatalf("output %s was truncated to %d bytes", path, len(data))
		}
	}
	matches, err := filepath.Glob(filepath.Join(dir, ".eac3to-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("cache directories remain: %v, %v", matches, err)
	}
}
