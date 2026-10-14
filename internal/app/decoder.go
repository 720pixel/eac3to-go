package app

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const decoderProfile = "8.0.2-eac3to-decoder-profile1"

func findDecoder(o Options, t *Tools) error {
	path := o.Decoder
	if path == "" {
		path = os.Getenv("EAC3TO_DECODER")
	}
	if path == "" {
		if executablePath, err := os.Executable(); err == nil {
			if resolved, err := filepath.EvalSymlinks(executablePath); err == nil {
				executablePath = resolved
			}
			candidate := filepath.Join(filepath.Dir(executablePath), "libexec", executableName("eac3to-decoder"))
			if executable(candidate) {
				path = candidate
			}
		}
	}
	if path == "" {
		return nil
	}
	resolved, err := exec.LookPath(path)
	if err != nil {
		return fmt.Errorf("audio decoder: %w", err)
	}
	t.Decoder, err = filepath.Abs(resolved)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var version limitedBuffer
	version.Limit = 32 << 10
	cmd := exec.CommandContext(ctx, t.Decoder, "-version")
	cmd.Stdout = &version
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("audio decoder version: %w", err)
	}
	first := strings.Fields(strings.SplitN(version.String(), "\n", 2)[0])
	t.DecoderProfile = len(first) >= 3 && first[0] == "ffmpeg" && first[1] == "version" && first[2] == decoderProfile
	return nil
}

func (t Tools) audioDecoder(codec, profile string) string {
	if t.Decoder != "" && (codec == "ac3" || codec == "eac3" || codec == "dts" && profile != "DTS-HD MA") {
		return t.Decoder
	}
	return t.FFmpeg
}
