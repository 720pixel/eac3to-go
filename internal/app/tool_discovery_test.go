package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestToolPairKeepsInstallationAndPriority(t *testing.T) {
	root := t.TempDir()
	makeTool := func(dir, name string) string {
		t.Helper()
		dir = filepath.Join(root, dir)
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, executableName(name))
		if err := os.WriteFile(path, []byte("fixture"), 0755); err != nil {
			t.Fatal(err)
		}
		return path
	}
	makeTool("partial-one", "ffmpeg")
	makeTool("partial-two", "ffprobe")
	wantF, wantP := makeTool("bundle", "ffmpeg"), makeTool("bundle", "ffprobe")
	otherF, otherP := makeTool("system", "ffmpeg"), makeTool("system", "ffprobe")
	dirs := []string{"", filepath.Join(root, "partial-one"), filepath.Join(root, "partial-two"), filepath.Join(root, "bundle"), filepath.Join(root, "system")}
	if f, p := findToolPair(dirs); f != wantF || p != wantP {
		t.Fatalf("selected pair %q, %q", f, p)
	}
	if f, p := findToolPair(dirs[:3]); f != "" || p != "" {
		t.Fatalf("combined tools from separate incomplete installations: %q, %q", f, p)
	}
	t.Setenv("EAC3TO_FFMPEG", otherF)
	t.Setenv("EAC3TO_FFPROBE", otherP)
	t.Setenv("EAC3TO_DECODER", "")
	tools, err := findTools(Options{FFmpeg: wantF, FFprobe: wantP})
	if err != nil || tools.FFmpeg != wantF || tools.FFprobe != wantP {
		t.Fatalf("explicit paths did not override environment: %+v, %v", tools, err)
	}
	tools, err = findTools(Options{})
	if err != nil || tools.FFmpeg != otherF || tools.FFprobe != otherP {
		t.Fatalf("environment paths were not retained: %+v, %v", tools, err)
	}
}

func TestMissingToolDiagnosticsNameBothRequirements(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("EAC3TO_FFMPEG", "")
	t.Setenv("EAC3TO_FFPROBE", "")
	_, err := findTools(Options{FFmpeg: filepath.Join(t.TempDir(), "missing-ffmpeg"), FFprobe: filepath.Join(t.TempDir(), "missing-ffprobe")})
	if err == nil || !strings.Contains(err.Error(), "FFmpeg and FFprobe") || !strings.Contains(err.Error(), "--download-tools") || !strings.Contains(err.Error(), "PATH") {
		t.Fatalf("%v", err)
	}
}
