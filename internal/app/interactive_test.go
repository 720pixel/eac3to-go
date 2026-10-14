package app

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"eac3to/internal/bluray"
)

func TestInteractiveModePreservesBatchAndMachineCalls(t *testing.T) {
	for _, tc := range []struct {
		args                   []string
		terminal, wizard, fail bool
	}{
		{nil, true, true, false}, {nil, false, false, false},
		{[]string{"source.wav"}, true, true, false},
		{[]string{"source.wav", "out.flac"}, true, false, false},
		{[]string{"source.wav", "-demux"}, true, false, false},
		{[]string{"source.wav", "--json"}, true, false, false},
		{[]string{"source.wav", "--dry-run"}, true, false, false},
		{[]string{"source.wav", "--non-interactive"}, true, false, false},
		{[]string{"source.wav", "--interactive"}, false, true, false},
		{[]string{"source.wav", "out.flac", "--interactive"}, false, true, false},
		{[]string{"source.wav", "--interactive", "--json"}, false, false, true},
		{[]string{"source.wav", "--interactive", "--report=out.json"}, false, false, true},
	} {
		o, err := ParseOptions(tc.args)
		if err != nil {
			t.Fatal(err)
		}
		got, err := interactiveMode(o, len(tc.args) == 0, tc.terminal)
		if got != tc.wizard || (err != nil) != tc.fail {
			t.Fatalf("%q: %v %v", tc.args, got, err)
		}
	}
	if _, err := ParseOptions([]string{"--interactive", "--non-interactive"}); err == nil {
		t.Fatal("conflicting modes accepted")
	}
}

func TestInteractiveScriptedTwoSecondWAVToFLAC(t *testing.T) {
	tools := mediaTools(t)
	dir := t.TempDir()
	source := filepath.Join(dir, "source with spaces.wav")
	mediaCommand(t, tools.FFmpeg, "-nostdin", "-v", "error", "-f", "lavfi", "-i", "sine=frequency=997:sample_rate=48000:duration=2", "-ac", "2", "-c:a", "pcm_s16le", source)
	destination := filepath.Join(dir, "output files")
	input := strings.Join([]string{"\"" + source + "\"", "99", "1", "\"" + destination + "\"", "flac", "wrong.ac3", "flac", "\"processed audio.flac\"", "yes", ""}, "\n")
	var stdout, stderr bytes.Buffer
	err := RunWithInput(context.Background(), []string{"--interactive", "--ffmpeg=" + tools.FFmpeg, "--ffprobe=" + tools.FFprobe}, strings.NewReader(input), &stdout, &stderr)
	if err != nil {
		t.Fatalf("wizard: %v\n%s", err, stderr.String())
	}
	out := filepath.Join(destination, "processed audio.flac")
	want := mediaCommand(t, tools.FFmpeg, "-v", "error", "-i", source, "-f", "s16le", "-")
	got := mediaCommand(t, tools.FFmpeg, "-v", "error", "-i", out, "-f", "s16le", "-")
	if len(want) != 96000*4 || !bytes.Equal(want, got) {
		t.Fatal("interactive FLAC changed source samples")
	}
	if _, err := os.Stat(filepath.Join(destination, "wrong.ac3")); !os.IsNotExist(err) {
		t.Fatal("format mismatch created an unselected lossy output")
	}
	if !strings.Contains(stderr.String(), "Destination extension must match") || !strings.Contains(stderr.String(), "Review outputs:") || !strings.Contains(stderr.String(), out) {
		t.Fatal("missing validation or concrete review")
	}
}

func TestInteractiveCancelAndEOFNeverCreateDestination(t *testing.T) {
	mediaTools(t)
	dir := t.TempDir()
	wav, _ := waveFixture(t, dir)
	for _, finish := range []string{"no\n", "q\n", "", "yes"} {
		dest := filepath.Join(t.TempDir(), "new directory")
		input := "1\n" + dest + "\nflac\n\n" + finish
		var log bytes.Buffer
		err := RunWithInput(context.Background(), []string{wav, "--interactive"}, strings.NewReader(input), io.Discard, &log)
		if !errors.Is(err, ErrInteractiveCancelled) {
			t.Fatalf("cancel %q: %v\n%s", finish, err, log.String())
		}
		if _, err := os.Stat(dest); !os.IsNotExist(err) {
			t.Fatal("cancelled review created destinations")
		}
	}
}

type interactivePromptSignal struct {
	once  sync.Once
	ready chan struct{}
}

func (w *interactivePromptSignal) Write(p []byte) (int, error) {
	if bytes.Contains(p, []byte("Source file or disc folder:")) {
		w.once.Do(func() { close(w.ready) })
	}
	return len(p), nil
}

func TestInteractiveContextCancelsBlockedInput(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	log := &interactivePromptSignal{ready: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- RunWithInput(ctx, []string{"--interactive"}, reader, io.Discard, log) }()
	select {
	case <-log.ready:
	case <-time.After(time.Second):
		t.Fatal("no source prompt")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel remained blocked on terminal input")
	}
}

func TestInteractivePlaylistAndTrackSelection(t *testing.T) {
	dir := t.TempDir()
	list := []*bluray.Playlist{
		{Path: filepath.Join(dir, "BDMV", "PLAYLIST", "00001.mpls"), Name: "00001.mpls", Items: []bluray.PlayItem{{ClipID: "00001", CodecID: "M2TS", OutTime: 45000}}},
		{Path: filepath.Join(dir, "BDMV", "PLAYLIST", "00002.mpls"), Name: "00002.mpls", Items: []bluray.PlayItem{{ClipID: "00002", CodecID: "M2TS", OutTime: 90000}}},
	}
	for _, answer := range []string{"unknown\n2\n", "00002.mpls\n"} {
		var log bytes.Buffer
		r := strings.NewReader(answer)
		ui := &interactiveReader{reader: bufio.NewReader(r), input: r, writer: &log}
		p, err := chooseInteractivePlaylist(context.Background(), ui, list, Options{Title: -1})
		if err != nil || p != list[1] || !strings.Contains(log.String(), "00001.mpls") || !strings.Contains(log.String(), "CLIPS") {
			t.Fatalf("playlist selection %v\n%s", err, log.String())
		}
	}
	available := []Track{{Number: 1, Kind: "chapters"}, {Number: 2, Kind: "video"}, {Number: 3, Kind: "audio"}}
	if got, err := interactiveTracks("2-3,1", available, false); err != nil || len(got) != 3 || got[0].Number != 2 || got[2].Number != 1 {
		t.Fatal("specific track order lost")
	}
	for _, invalid := range []string{"", "0", "4", "3-1", "1,1", "1-9999"} {
		if _, err := interactiveTracks(invalid, available, false); err == nil {
			t.Fatalf("selection %q accepted", invalid)
		}
	}
	available[2].Hidden = true
	if got, err := interactiveTracks("all", available, false); err != nil || len(got) != 2 {
		t.Fatal("all included hidden tracks")
	}
	if got, err := interactiveTracks("all", available, true); err != nil || len(got) != 3 {
		t.Fatal("include_hidden did not affect all")
	}
	if got, err := interactiveTracks("3", available, false); err != nil || len(got) != 1 {
		t.Fatal("explicit hidden track unavailable")
	}
	if interactiveFormatMatches("flac", "ac3") || !interactiveFormatMatches("thd", "truehd") || interactivePath(`"a b.wav"`) != "a b.wav" {
		t.Fatal("format/path safety regression")
	}
}

type interactiveNeverRead struct{}

func (interactiveNeverRead) Read([]byte) (int, error) {
	panic("batch invocation read interactive stdin")
}

func TestRunWithInputJSONAndBatchDoNotPrompt(t *testing.T) {
	mediaTools(t)
	dir := t.TempDir()
	wav, _ := waveFixture(t, dir)
	for _, args := range [][]string{{wav, "--json"}, {wav, "--non-interactive"}, {"--help"}} {
		var out, log bytes.Buffer
		if err := RunWithInput(context.Background(), args, interactiveNeverRead{}, &out, &log); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(log.String(), "Start extraction?") {
			t.Fatal("batch invocation prompted")
		}
	}
}

func TestInteractiveFormatsAndNonDiscLanguageValidation(t *testing.T) {
	for _, codec := range []string{"pcm_s16le", "aac", "mp3", "opus"} {
		stream := &Stream{Codec: codec, Type: "audio"}
		var log bytes.Buffer
		r := strings.NewReader("q\n")
		ui := &interactiveReader{reader: bufio.NewReader(r), input: r, writer: &log}
		_, err := chooseInteractiveOutput(context.Background(), ui, &Source{Name: "generated"}, Track{Number: 1, Kind: "audio", Stream: stream}, t.TempDir(), Options{})
		if !errors.Is(err, ErrInteractiveCancelled) {
			t.Fatal(err)
		}
		if !strings.Contains(log.String(), "rf64") {
			t.Fatal("RF64 was not offered")
		}
		for _, copyCodec := range []string{"aac", "mp3", "opus"} {
			if strings.Contains(log.String(), "/"+copyCodec+"/") && codec != copyCodec {
				t.Fatalf("unsupported %s encoder offered for %s", copyCodec, codec)
			}
		}
	}
	mediaTools(t)
	wav, _ := waveFixture(t, t.TempDir())
	var log bytes.Buffer
	err := RunWithInput(context.Background(), []string{wav, "--interactive", "--language=eng"}, interactiveNeverRead{}, io.Discard, &log)
	if err == nil || !strings.Contains(err.Error(), "playlist options require") {
		t.Fatalf("non-disc language preference was ignored: %v", err)
	}
}
