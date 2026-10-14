package transport

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestFFmpegElementaryPayloads(t *testing.T) {
	ffmpeg := transportMediaTool(t)
	dir := t.TempDir()
	audio := runTransportMedia(t, ffmpeg, "-v", "error", "-f", "lavfi", "-i", "sine=frequency=997:sample_rate=48000:duration=0.24", "-c:a", "ac3", "-b:a", "192k", "-threads", "1", "-f", "ac3", "pipe:1")
	video := runTransportMedia(t, ffmpeg, "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=64x48:rate=25:duration=0.24", "-c:v", "mpeg2video", "-bf", "2", "-threads", "1", "-f", "mpeg2video", "pipe:1")
	audioPath, videoPath := filepath.Join(dir, "input.ac3"), filepath.Join(dir, "input.m2v")
	if err := os.WriteFile(audioPath, audio, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(videoPath, video, 0600); err != nil {
		t.Fatal(err)
	}
	for _, m2ts := range []bool{false, true} {
		var multiplexed []byte
		videoPID, audioPID := uint16(256), uint16(257)
		if m2ts {
			videoPID, audioPID = 0x1011, 0x1100
			multiplexed = runTransportMedia(t, ffmpeg, "-v", "error", "-fflags", "+genpts", "-r", "25", "-i", videoPath, "-i", audioPath, "-map", "0:v:0", "-map", "1:a:0", "-c", "copy", "-mpegts_m2ts_mode", "1", "-f", "mpegts", "pipe:1")
		} else {
			multiplexed = runTransportMedia(t, ffmpeg, "-v", "error", "-fflags", "+genpts", "-r", "25", "-i", videoPath, "-i", audioPath, "-map", "0:v:0", "-map", "1:a:0", "-c", "copy", "-mpegts_start_pid", "256", "-f", "mpegts", "pipe:1")
		}
		var gotVideo, gotAudio bytes.Buffer
		var ptsCount, dtsCount int
		videoMin, videoMax := int64(1<<33), int64(-1)
		writer := func(dest *bytes.Buffer) func(PES) error {
			return func(p PES) error {
				if p.PTS != NoTimestamp {
					ptsCount++
					if dest == &gotVideo {
						videoMin, videoMax = min(videoMin, p.PTS), max(videoMax, p.PTS)
					}
				}
				if p.DTS != NoTimestamp {
					dtsCount++
				}
				_, err := dest.Write(p.Payload)
				return err
			}
		}
		d, err := New(Config{Handlers: map[uint16]func(PES) error{videoPID: writer(&gotVideo), audioPID: writer(&gotAudio)}})
		if err != nil {
			t.Fatal(err)
		}
		if err := d.ReadSegment(context.Background(), bytes.NewReader(multiplexed), Segment{Index: 0}); err != nil {
			t.Fatalf("m2ts=%v: %v", m2ts, err)
		}
		if err := d.Finish(); err != nil {
			t.Fatalf("m2ts=%v: %v", m2ts, err)
		}
		if !bytes.Equal(gotVideo.Bytes(), video) || !bytes.Equal(gotAudio.Bytes(), audio) {
			t.Fatalf("m2ts=%v changed elementary payloads: video %d/%d bytes, audio %d/%d bytes", m2ts, gotVideo.Len(), len(video), gotAudio.Len(), len(audio))
		}
		if ptsCount < 5 || dtsCount == 0 {
			t.Fatalf("m2ts=%v: lost PTS/DTS: %d/%d", m2ts, ptsCount, dtsCount)
		}
		muxPath := filepath.Join(dir, "multiplexed.ts")
		if err := os.WriteFile(muxPath, multiplexed, 0600); err != nil {
			t.Fatal(err)
		}
		scan, err := ScanFile(context.Background(), muxPath, ScanConfig{VideoPIDs: map[uint16]bool{videoPID: true}, FullScan: true})
		if err != nil {
			t.Fatalf("m2ts=%v scanner: %v", m2ts, err)
		}
		if !scan.HasPTS || !scan.UsedVideo || scan.FirstPTS != videoMin || scan.LastPTS != videoMax || scan.ContinuityErrors != 0 || scan.TransportErrors != 0 || scan.ScrambledPackets != 0 || scan.TrailingBytes != 0 {
			t.Fatalf("m2ts=%v scanner range differs from full PES extraction %d..%d: %+v", m2ts, videoMin, videoMax, scan)
		}
	}
}

func transportMediaTool(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("external media interoperability tests disabled by -short")
	}
	if info, err := os.Stat("/usr/bin/ffmpeg"); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
		return "/usr/bin/ffmpeg"
	}
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg is needed for media interoperability tests")
	}
	return path
}

func runTransportMedia(t *testing.T, tool string, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, tool, args...)
	var output, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s %v: %v\n%s", tool, args, err, stderr.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("%s reported an error: %s", tool, stderr.String())
	}
	return output.Bytes()
}
