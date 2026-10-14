package app

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"eac3to/internal/bluray"
)

var ErrInteractiveCancelled = errors.New("interactive extraction cancelled")

// RunWithInput enables terminal selection for source-only launches. Batch
// writers and redirected streams never prompt unless --interactive is supplied.
func RunWithInput(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) error {
	o, err := ParseOptions(args)
	if err != nil {
		return err
	}
	inputFile, inputTTY := stdin.(*os.File)
	errorFile, errorTTY := stderr.(*os.File)
	terminal := inputTTY && errorTTY && interactiveTerminal(inputFile) && interactiveTerminal(errorFile)
	wizard, err := interactiveMode(o, len(args) == 0, terminal)
	if err != nil {
		return err
	}
	if !wizard {
		return Run(ctx, args, stdout, stderr)
	}
	if stdin == nil {
		return fmt.Errorf("interactive mode requires an input reader")
	}
	return runInteractive(ctx, o, &interactiveReader{reader: bufio.NewReader(stdin), input: stdin, writer: stderr}, stdout, stderr)
}

func interactiveMode(o Options, noArgs, terminal bool) (bool, error) {
	if o.NonInteractive {
		return false, nil
	}
	inspection := o.Help && !noArgs || o.Version || o.Test || o.JSON || o.Report != "" || o.DryRun || o.Check || o.AuditPL || o.Segments || o.Explain || len(o.ComparePlaylists) > 0
	if inspection {
		if o.Interactive {
			return false, fmt.Errorf("--interactive cannot be combined with machine-readable or inspection modes")
		}
		return false, nil
	}
	return o.Interactive || terminal && len(o.Outputs) == 0 && !o.Demux, nil
}

type interactiveReader struct {
	reader *bufio.Reader
	input  io.Reader
	writer io.Writer
}

func (ui *interactiveReader) ask(ctx context.Context, prompt string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if _, err := io.WriteString(ui.writer, prompt); err != nil {
		return "", err
	}
	type reply struct {
		line string
		err  error
	}
	answer := make(chan reply, 1)
	go func() { line, err := ui.reader.ReadString('\n'); answer <- reply{line, err} }()
	select {
	case <-ctx.Done():
		if closer, ok := ui.input.(io.Closer); ok {
			closer.Close()
		}
		return "", ctx.Err()
	case value := <-answer:
		if value.err == io.EOF {
			return "", ErrInteractiveCancelled
		}
		if value.err != nil {
			return "", value.err
		}
		line := strings.TrimSuffix(strings.TrimSuffix(value.line, "\n"), "\r")
		if strings.EqualFold(strings.TrimSpace(line), "q") || strings.EqualFold(strings.TrimSpace(line), "quit") {
			return "", ErrInteractiveCancelled
		}
		return line, nil
	}
}

func runInteractive(ctx context.Context, o Options, ui *interactiveReader, stdout, stderr io.Writer) error {
	o.Help, o.Interactive = false, false
	fmt.Fprintln(stderr, "Select a source, tracks and outputs. Enter q to cancel.")
	for len(o.Inputs) == 0 {
		value, err := ui.ask(ctx, "Source file or disc folder: ")
		if err != nil {
			return err
		}
		value = interactivePath(value)
		if value == "" {
			continue
		}
		if _, err := os.Stat(value); err != nil {
			fmt.Fprintln(stderr, "Source:", err)
			continue
		}
		o.Inputs = []string{value}
	}
	list, err := discover(o)
	if err != nil {
		return err
	}
	var play *bluray.Playlist
	if list != nil {
		play, err = chooseInteractivePlaylist(ctx, ui, list, o)
		if err != nil {
			return err
		}
	}
	if play == nil && (o.Main || o.Playlist != "" || o.Title >= 0 || o.SkipFirst || o.SkipLast || o.StrictPL || o.Collapse > 0 || o.Language != "") {
		return fmt.Errorf("playlist options require a disc or MPLS source")
	}
	if len(o.Inputs) == 1 && ext(o.Inputs[0]) == "mpls" && o.Language != "" {
		return fmt.Errorf("playlist language preferences require a disc folder")
	}
	tools, err := findTools(o)
	if err != nil {
		return err
	}
	o.referenceDecoder = tools.DecoderProfile
	source, err := openSource(ctx, tools, o, play)
	if err != nil {
		return err
	}
	if err := printTracks(stderr, source, false); err != nil {
		return err
	}
	if len(o.Outputs) == 0 && !o.Demux {
		var tracks []Track
		for {
			choice, err := ui.ask(ctx, "Tracks (numbers/ranges separated by commas, or all): ")
			if err != nil {
				return err
			}
			tracks, err = interactiveTracks(choice, source.Tracks, o.IncludeHidden)
			if err == nil {
				break
			}
			fmt.Fprintln(stderr, err)
		}
		defaultDir := o.Dest
		if defaultDir == "" {
			defaultDir = "."
		}
		directory, err := ui.ask(ctx, fmt.Sprintf("Output directory [%s]: ", defaultDir))
		if err != nil {
			return err
		}
		directory = interactivePath(directory)
		if directory == "" {
			directory = defaultDir
		}
		directory, err = filepath.Abs(directory)
		if err != nil {
			return err
		}
		o.Dest = ""
		for _, track := range tracks {
			request, err := chooseInteractiveOutput(ctx, ui, source, track, directory, o)
			if err != nil {
				return err
			}
			o.Outputs = append(o.Outputs, request)
		}
	}
	outputs, err := buildOutputs(source, o)
	if err != nil {
		return err
	}
	if err := validateParityPaths(source, outputs, o); err != nil {
		return err
	}
	outputs, err = prepareTrueHDOutputs(ctx, source, outputs, o)
	if err != nil {
		return err
	}
	for _, out := range outputs {
		if out.Track.Kind == "audio" && !out.Copy {
			if err := validateAudioOutput(out); err != nil {
				return err
			}
		}
	}
	fmt.Fprintln(stderr, "\nReview outputs:")
	if source.Playlist != nil {
		fmt.Fprintf(stderr, "Playlist %s; %d segments; %s\n", source.Playlist.Name, len(source.Playlist.Items), timestamp(source.Playlist.Duration()))
	}
	for _, out := range outputs {
		operation := "copy encoded stream"
		if out.Track.Kind == "chapters" {
			operation = "export chapters"
		} else if !out.Copy {
			operation = "decode/process to " + out.Codec
		}
		if out.RemoveDialnorm {
			operation += "; remove dialog normalization"
		}
		if out.Request.Options.Core {
			operation += "; extract core/companion"
		} else if out.BSF != "" {
			operation += "; bitstream filter " + out.BSF
		}
		if out.Forced {
			operation += "; keep forced subtitle presentations"
		}
		if out.Request.Options.TargetFPS != 0 {
			operation += fmt.Sprintf("; target frame rate %.6g fps", out.Request.Options.TargetFPS)
		}
		if out.Request.Options.Bitrate > 0 {
			operation += fmt.Sprintf("; %dkbps", out.Request.Options.Bitrate)
		}
		if out.Request.Options.DelayMS != 0 {
			operation += fmt.Sprintf("; manual delay %+.3gms", out.Request.Options.DelayMS)
		}
		if o.Force {
			operation += "; replace existing destination if present"
		}
		fmt.Fprintf(stderr, "%d: %s -> %s (%s)\n", out.Track.Number, out.Format, out.Path, operation)
	}
	for {
		answer, err := ui.ask(ctx, "Start extraction? [yes/no]: ")
		if err != nil {
			return err
		}
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "yes", "y":
			return execute(ctx, tools, source, outputs, o, stdout, stderr)
		case "no", "n", "":
			return ErrInteractiveCancelled
		default:
			fmt.Fprintln(stderr, "Enter yes to start or no to cancel.")
		}
	}
}

func chooseInteractivePlaylist(ctx context.Context, ui *interactiveReader, list []*bluray.Playlist, o Options) (*bluray.Playlist, error) {
	if len(list) == 0 {
		return nil, fmt.Errorf("no playlists match the duration limit")
	}
	if o.Playlist != "" {
		for _, p := range list {
			if p.Name == o.Playlist {
				return p, nil
			}
		}
		return nil, fmt.Errorf("playlist %s not found", o.Playlist)
	}
	if o.Title >= 0 {
		if o.Title >= len(list) {
			return nil, fmt.Errorf("title does not exist")
		}
		return list[o.Title], nil
	}
	if len(o.Inputs) == 1 && ext(o.Inputs[0]) == "mpls" {
		return list[0], nil
	}
	selection, err := bluray.SelectMainWithOptions(list, bluray.SelectionOptions{Language: o.Language})
	if err != nil {
		return nil, err
	}
	if err := printTitles(ui.writer, list, selection, Options{ShowAll: o.ShowAll}); err != nil {
		return nil, err
	}
	defaultName := ""
	if selection.Recommended != nil {
		defaultName = selection.Recommended.Name
	}
	for {
		prompt := "Playlist (title number or NNNNN.mpls; more lists all): "
		if defaultName != "" {
			prompt = fmt.Sprintf("Playlist (title number or NNNNN.mpls; Enter=%s; more lists all): ", defaultName)
		}
		value, err := ui.ask(ctx, prompt)
		if err != nil {
			return nil, err
		}
		value = strings.TrimSpace(value)
		if value == "more" {
			if err := printTitles(ui.writer, list, selection, Options{ShowAll: true}); err != nil {
				return nil, err
			}
			continue
		}
		if value == "" {
			value = defaultName
		}
		if strings.HasSuffix(strings.ToLower(value), ".mpls") {
			for _, p := range list {
				if strings.EqualFold(p.Name, value) {
					return p, nil
				}
			}
		} else if n, err := strconv.Atoi(strings.TrimSuffix(value, ")")); err == nil && n >= 1 && n <= len(list) {
			return list[n-1], nil
		}
		fmt.Fprintln(ui.writer, "Choose a listed title number or playlist filename.")
	}
}

func interactiveTracks(value string, available []Track, includeHidden bool) ([]Track, error) {
	value = strings.TrimSpace(value)
	if strings.EqualFold(value, "all") {
		var tracks []Track
		for _, track := range available {
			if !track.Hidden || includeHidden {
				tracks = append(tracks, track)
			}
		}
		if len(tracks) == 0 {
			return nil, fmt.Errorf("no visible tracks are available")
		}
		return tracks, nil
	}
	var result []Track
	seen := map[int]bool{}
	for _, part := range strings.Split(value, ",") {
		bounds := strings.Split(strings.TrimSpace(part), "-")
		if len(bounds) > 2 {
			return nil, fmt.Errorf("invalid track range")
		}
		first, err := strconv.Atoi(strings.TrimSpace(bounds[0]))
		if err != nil || first < 1 {
			return nil, fmt.Errorf("choose one or more listed track numbers")
		}
		last := first
		if len(bounds) == 2 {
			last, err = strconv.Atoi(strings.TrimSpace(bounds[1]))
			if err != nil || last < first || last-first > len(available) {
				return nil, fmt.Errorf("invalid track range")
			}
		}
		for n := first; n <= last; n++ {
			var found *Track
			for i := range available {
				if available[i].Number == n {
					found = &available[i]
					break
				}
			}
			if found == nil || seen[n] {
				return nil, fmt.Errorf("track %d is absent or repeated", n)
			}
			result = append(result, *found)
			seen[n] = true
		}
	}
	return result, nil
}

func chooseInteractiveOutput(ctx context.Context, ui *interactiveReader, s *Source, track Track, directory string, o Options) (OutputRequest, error) {
	defaultFormat := "txt"
	formats := []string{"txt", "xml", "json"}
	if track.Stream != nil {
		defaultFormat = defaultExtension(track.Stream)
		formats = []string{defaultFormat}
		if track.Kind == "audio" {
			if strings.HasPrefix(track.Stream.Codec, "pcm_") {
				defaultFormat = "flac"
				if strings.HasPrefix(track.Stream.Codec, "pcm_f") {
					defaultFormat = "wav"
				}
			}
			if o.Defaults.Core && (track.Stream.Codec == "truehd" || track.Stream.Codec == "eac3") {
				defaultFormat = "ac3"
			}
			if o.Defaults.Core && track.Stream.Codec == "dts" {
				defaultFormat = "dts"
			}
			formats = []string{defaultFormat, "flac", "wav", "w64", "rf64", "raw", "ac3", "mka"}
			// These codecs can be copied but have no verified encoding path.
			switch track.Stream.Codec {
			case "aac", "mp3", "opus":
				formats = append(formats, track.Stream.Codec)
			}
		} else if track.Kind == "video" {
			formats = append(formats, "mkv")
		} else if track.Kind == "subtitle" && !o.Defaults.Forced {
			formats = append(formats, "mkv")
		}
	}
	seen := map[string]bool{}
	var choices []string
	for _, format := range formats {
		if seen[format] {
			continue
		}
		seen[format] = true
		choices = append(choices, format)
	}
	for {
		format, err := ui.ask(ctx, fmt.Sprintf("Track %d format (%s) [%s]: ", track.Number, strings.Join(choices, "/"), defaultFormat))
		if err != nil {
			return OutputRequest{}, err
		}
		format = strings.TrimPrefix(strings.ToLower(strings.TrimSpace(format)), ".")
		if format == "" {
			format = defaultFormat
		}
		request := OutputRequest{Track: track.Number, Options: o.Defaults}
		if format == "ac3" && track.Stream != nil && track.Stream.Codec != "ac3" && !request.Options.Core {
			bitrate := 448
			if track.Stream.Channels > 2 {
				bitrate = 640
			}
			value, err := ui.ask(ctx, fmt.Sprintf("AC-3 is lossy. Bitrate in kbps [%d]: ", bitrate))
			if err != nil {
				return request, err
			}
			if value != "" {
				bitrate, err = strconv.Atoi(strings.TrimSpace(value))
				if err != nil || bitrate <= 0 {
					fmt.Fprintln(ui.writer, "Choose a positive bitrate.")
					continue
				}
			}
			request.Options.Bitrate = bitrate
		}
		filename := filepath.Join(directory, fmt.Sprintf("%s - %d.%s", safeName(s.Name), track.Number, format))
		value, err := ui.ask(ctx, fmt.Sprintf("Track %d destination [%s]: ", track.Number, filename))
		if err != nil {
			return request, err
		}
		value = interactivePath(value)
		if value != "" {
			filename = value
			if !filepath.IsAbs(filename) {
				filename = filepath.Join(directory, filename)
			}
		}
		if ext(filename) == "" {
			filename += "." + format
		}
		if !interactiveFormatMatches(format, ext(filename)) {
			fmt.Fprintf(ui.writer, "Destination extension must match the selected .%s format. Choose the format again to change it.\n", format)
			continue
		}
		request.Path = filename
		out, err := makeOutput(s, track, request, o)
		if err == nil {
			err = validateParityPaths(s, []Output{out}, o)
		}
		if err == nil && track.Kind == "audio" && !out.Copy {
			err = validateAudioOutput(out)
		}
		if err != nil {
			fmt.Fprintln(ui.writer, "Output:", err)
			continue
		}
		return request, nil
	}
}

func interactivePath(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
		return value[1 : len(value)-1]
	}
	return value
}

func interactiveFormatMatches(chosen, actual string) bool {
	if chosen == actual {
		return true
	}
	for _, aliases := range [][]string{{"thd", "truehd"}, {"h264", "264"}, {"h265", "265", "hevc"}, {"dtshd", "dtsma"}, {"raw", "pcm", "lpcm"}, {"ass", "ssa"}} {
		foundChosen, foundActual := false, false
		for _, value := range aliases {
			foundChosen = foundChosen || value == chosen
			foundActual = foundActual || value == actual
		}
		if foundChosen && foundActual {
			return true
		}
	}
	return false
}
