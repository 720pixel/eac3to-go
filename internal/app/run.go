package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"eac3to/internal/aften"
	"eac3to/internal/bluray"
	"eac3to/internal/ssrc"
)

const help = `eac3to-go — native media demuxer and audio processor

Usage:
  eac3to INPUT                             Select tracks in a terminal
  eac3to INPUT 2: audio.flac               Process a selected track
  eac3to INPUT 1: video.h264 2: audio.dts   Extract several tracks
  eac3to INPUT -demux -destpath=OUTPUT     Extract tracks and chapters
  eac3to INPUT output.*                    Extract tracks with a filename prefix
  eac3to DISC                              Select playlist and tracks in a terminal
  eac3to DISC --main                       Inspect the recommended feature
  eac3to DISC -demux                       Select and extract the main feature
  eac3to DISC --playlist=00001 -demux       Extract a specific playlist
  eac3to DISC --compare-playlists=1,125     Compare alternate segment orders
  eac3to DISC '1)'                         List tracks in a playlist
  eac3to PLAYLIST.mpls 1: chapters.txt      Export chapters
  eac3to input.wav output.flac             Convert one audio stream

Audio: -core, -keepDialnorm, -removeDialnorm, -normalize, -2pass,
       -no2ndpass, -down16, -down24, -full, -mono, -downStereo, -downDpl,
       -down6, -double7, -mixlfe, -0,1,2,3,4,5, +3dB, -3dB,
       +100ms, -100ms, -silence, -loop, -resampleTo48000, --reference-tail, -slowdown, -speedup,
       -decodeHdcd, -448, -640
PCM:   -override -24 -48000 -6 -little (or -big)
Video: -changeTo24.000, -slowdown, -speedup (H.264 elementary output)
Disc:  --main, --playlist=NNNNN, --segments, --compare-playlists=ID,ID,
       --explain, --language=eng (authored picture-language preference),
       -showall, -sf, -sl, -strictpl,
       -include_hidden, -minDuration=SECONDS, -collapse=N, -auditpl
PGS:   -forced
Other: -test, -check, --json, --dry-run, --force, --quiet,
       --interactive, --non-interactive,
       --report=PATH (atomic output manifest with residual mux delays),
       --jobs=N, --threads=N, --ffmpeg=PATH, --ffprobe=PATH, --decoder=PATH, --version
       --allow-unverified (enable reference-comparison paths)

Track options follow their output filename. Options before the first output
are defaults for every output. Quote title selectors and wildcard filenames
so the shell passes them unchanged. Existing files require --force.

With terminal input and diagnostics, no arguments or a source-only launch
starts interactive selection. --non-interactive keeps source listing behavior;
--interactive forces prompts with redirected input. Fully specified outputs,
JSON, reports and inspection modes remain unattended.

The version-1 extraction report links final output paths to source tracks.
Its mux_delay_ms is the remaining whole-millisecond compressed-frame delay;
PCM delay is already applied and reports zero. TrueHD includes its filename
delay with delay_in_filename=true: apply that report offset only once.
Unverified container-copy timing fails explicitly when a report is requested.

This is a development build. Full Windows 4.00 parity is still being tested;
unsupported operations fail explicitly.
`

func Run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	o, e := ParseOptions(args)
	if e != nil {
		return e
	}
	if o.Interactive {
		return fmt.Errorf("--interactive needs input; use RunWithInput")
	}
	if o.Report != "" && (o.Help || o.Version || o.Test || o.Check || !o.Demux && len(o.Outputs) == 0) {
		return fmt.Errorf("--report requires media extraction")
	}
	if o.Help {
		_, e = io.WriteString(stdout, help)
		return e
	}
	if o.Version {
		_, e = fmt.Fprintln(stdout, "eac3to-go", Version)
		return e
	}
	if o.Test {
		t, e := findTools(o)
		if e != nil {
			return e
		}
		return doctor(ctx, t, stdout, o.JSON)
	}
	list, e := discover(o)
	if e != nil {
		return e
	}
	if len(o.ComparePlaylists) > 0 {
		if o.Demux || len(o.Outputs) > 0 || o.Check || o.AuditPL {
			return fmt.Errorf("playlist comparison cannot be combined with extraction")
		}
		var chosen []*bluray.Playlist
		for _, name := range o.ComparePlaylists {
			var found *bluray.Playlist
			for _, p := range list {
				if strings.EqualFold(p.Name, name) {
					found = p
					break
				}
			}
			if found == nil {
				return fmt.Errorf("playlist %s was not found", name)
			}
			selected, _, err := selectPlaylistItems(found, o)
			if err != nil {
				return err
			}
			chosen = append(chosen, selected)
		}
		return printSegments(stdout, chosen, o)
	}
	var play *bluray.Playlist
	if list != nil {
		if o.Playlist != "" {
			for _, p := range list {
				if strings.EqualFold(p.Name, o.Playlist) {
					play = p
					break
				}
			}
			if play == nil {
				return fmt.Errorf("playlist %s was not found in the disc listing", o.Playlist)
			}
		} else if o.Title >= 0 {
			if o.Title >= len(list) {
				return fmt.Errorf("title %d does not exist", o.Title+1)
			}
			play = list[o.Title]
		} else if len(o.Inputs) == 1 && ext(o.Inputs[0]) == "mpls" {
			if len(list) == 0 {
				return fmt.Errorf("playlist is empty")
			}
			play = list[0]
		} else {
			selection, err := bluray.SelectMainWithOptions(list, bluray.SelectionOptions{Language: o.Language})
			if err != nil {
				return err
			}
			if !o.Main && !o.Segments && !o.Demux && len(o.Outputs) == 0 && !o.Check && !o.AuditPL {
				return printTitles(stdout, list, selection, o)
			}
			if selection.Recommended == nil {
				return fmt.Errorf("no unambiguous main playlist: %s Use --playlist=NNNNN to select one", strings.Join(selection.Reasons, " "))
			}
			play = selection.Recommended
			if !o.Quiet {
				fmt.Fprintf(stderr, "Selected %s (%s confidence): %s\n", play.Name, selection.Confidence, strings.Join(selection.Reasons, " "))
			}
		}
	} else if o.Title >= 0 || o.Main || o.Playlist != "" {
		return fmt.Errorf("title selection requires a disc or playlist")
	}
	if play == nil && list == nil && (o.Explain || o.Language != "") {
		return fmt.Errorf("playlist ranking and language preferences need a disc folder")
	}
	if play == nil && (o.SkipFirst || o.SkipLast || o.Collapse > 0 || o.StrictPL || o.AuditPL) {
		return fmt.Errorf("playlist trimming and audit options need a disc or MPLS source")
	}
	if o.AuditPL {
		selected, _, err := selectPlaylistItems(play, o)
		if err != nil {
			return err
		}
		dest := stdout
		if o.Demux || len(o.Outputs) > 0 || o.Check {
			dest = stderr
		}
		if err := auditPlaylist(ctx, selected, o, dest, stderr); err != nil {
			return err
		}
		if !o.Demux && len(o.Outputs) == 0 && !o.Check {
			return nil
		}
	}
	if o.Segments && !o.Demux && len(o.Outputs) == 0 && !o.Check {
		if play == nil {
			return fmt.Errorf("--segments needs a playlist or disc")
		}
		selected, _, err := selectPlaylistItems(play, o)
		if err != nil {
			return err
		}
		return printSegments(stdout, []*bluray.Playlist{selected}, o)
	}
	t, e := findTools(o)
	if e != nil {
		return e
	}
	s, e := openSource(ctx, t, o, play)
	if e != nil {
		return e
	}
	if o.Check {
		return checkSource(ctx, t, s, stderr)
	}
	if !o.Demux && len(o.Outputs) == 0 {
		return printTracks(stdout, s, o.JSON)
	}
	outputs, e := buildOutputs(s, o)
	if e != nil {
		return e
	}
	o.referenceDecoder = t.DecoderProfile
	if e = validateParityPaths(s, outputs, o); e != nil {
		return e
	}
	outputs, e = prepareTrueHDOutputs(ctx, s, outputs, o)
	if e != nil {
		return e
	}
	if _, e = extractionReportPath(s, outputs, o); e != nil {
		return e
	}
	if o.DryRun {
		type step struct {
			Track           int          `json:"track"`
			Output          string       `json:"output"`
			Operation       string       `json:"operation"`
			Codec           string       `json:"codec"`
			BitstreamFilter string       `json:"bitstream_filter,omitempty"`
			RemoveDialnorm  bool         `json:"remove_dialnorm,omitempty"`
			Forced          bool         `json:"forced_only,omitempty"`
			Options         AudioOptions `json:"audio_options"`
		}
		var steps []step
		for _, out := range outputs {
			op := "decode/process/encode"
			if out.Copy {
				op = "stream copy"
				if out.Track.Stream != nil && out.Track.Stream.Type == "video" && out.Request.Options.TargetFPS != 0 {
					op = "stream copy with frame-rate header edits"
				}
			}
			if out.Track.Kind == "chapters" {
				op = "write chapters"
			}
			steps = append(steps, step{out.Track.Number, out.Path, op, out.Codec, out.BSF, out.RemoveDialnorm, out.Forced, out.Request.Options})
		}
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(steps)
	}
	return execute(ctx, t, s, outputs, o, stdout, stderr)
}

func validateParityPaths(s *Source, outputs []Output, o Options) error {
	if o.AllowUnverified {
		return nil
	}
	for _, out := range outputs {
		if out.Track.Kind == "chapters" {
			continue
		}
		st := out.Track.Stream
		multiple := s.Playlist != nil && len(s.Playlist.Items) > 1 || s.Playlist == nil && len(s.Paths) > 1
		truehdTrack := st.Codec == "truehd" || out.TrueHDCompanion || sharedTrueHDPID(s, st)
		if truehdTrack && multiple {
			return fmt.Errorf("TrueHD and companion alignment across segments still needs an authored-disc comparison; --allow-unverified permits comparison runs only")
		}
		if truehdTrack && o.StrictPL && s.Playlist != nil {
			return fmt.Errorf("strict TrueHD playlist starts still need major-sync validation; --allow-unverified permits comparison runs only")
		}
		if st.Type == "audio" && multiple {
			native := s.Playlist != nil && (out.Copy && nativeCopySupported(out) || !out.Copy && (st.Codec == "dts" || st.Codec == "pcm_bluray"))
			if !native {
				return fmt.Errorf("%s alignment across segments still needs validation; --allow-unverified permits comparison runs only", st.Codec)
			}
		}
		if !out.Copy && (st.Codec == "ac3" || st.Codec == "eac3") && !o.referenceDecoder {
			return fmt.Errorf("AC-3/E-AC-3 decoding requires the matched decoder profile; build it with make decoder or select it with --decoder=PATH; --allow-unverified permits comparison runs only")
		}
		if !out.Copy && st.Codec == "dts" && st.Profile != "DTS-HD MA" && !o.referenceDecoder {
			return fmt.Errorf("lossy DTS decoding requires the matched decoder profile; build it with make decoder or select it with --decoder=PATH; --allow-unverified permits comparison runs only")
		}
		if out.Request.Options.HDCD {
			return fmt.Errorf("HDCD output still needs comparison with the reference decoder; --allow-unverified permits comparison runs only")
		}
	}
	return nil
}

func doctor(ctx context.Context, t Tools, w io.Writer, jsonMode bool) error {
	type info struct {
		Path, Version string
		Capabilities  map[string]bool
	}
	result := map[string]info{}
	for name, path := range map[string]string{"ffmpeg": t.FFmpeg, "ffprobe": t.FFprobe} {
		cmd := exec.CommandContext(ctx, path, "-version")
		b, e := cmd.Output()
		if e != nil {
			return e
		}
		result[name] = info{Path: path, Version: strings.SplitN(string(b), "\n", 2)[0]}
	}
	features := map[string]bool{}
	for _, spec := range []struct {
		arg   string
		names []string
	}{{"-decoders", []string{"ac3", "eac3", "truehd", "mlp", "dca", "flac", "aac"}}, {"-encoders", []string{"flac", "pcm_s24le", "ac3", "eac3", "libfdk_aac"}}, {"-bsfs", []string{"dca_core", "eac3_core", "h264_mp4toannexb", "hevc_mp4toannexb"}}, {"-protocols", []string{"bluray"}}} {
		cmd := exec.CommandContext(ctx, t.FFmpeg, "-hide_banner", spec.arg)
		b, e := cmd.Output()
		if e != nil {
			return e
		}
		for _, n := range spec.names {
			found := false
			for _, line := range strings.Split(string(b), "\n") {
				f := strings.Fields(line)
				for j, word := range f {
					if word == n && (j == 0 || j == 1) {
						found = true
					}
				}
			}
			features[strings.TrimPrefix(spec.arg, "-")+":"+n] = found
		}
	}
	f := result["ffmpeg"]
	f.Capabilities = features
	result["ffmpeg"] = f
	result["native"] = info{Version: Version, Capabilities: map[string]bool{"ssrc": ssrc.Available(), "aften": aften.Available(), "flac_integrity": true}}
	if t.Decoder != "" {
		result["audio_decoder"] = info{Path: t.Decoder, Capabilities: map[string]bool{"matched_profile": t.DecoderProfile}}
	}
	if jsonMode {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(result)
	}
	fmt.Fprintf(w, "eac3to-go %s\nFFmpeg: %s\n%s\nFFprobe: %s\n%s\n", Version, t.FFmpeg, result["ffmpeg"].Version, t.FFprobe, result["ffprobe"].Version)
	for _, n := range []string{"decoders:dca", "decoders:truehd", "decoders:eac3", "encoders:flac", "encoders:libfdk_aac", "bsfs:dca_core", "protocols:bluray"} {
		fmt.Fprintf(w, "%-24s %t\n", n, features[n])
	}
	fmt.Fprintf(w, "%-24s %t\n%-24s %t\n", "native:ssrc", ssrc.Available(), "native:aften", aften.Available())
	if t.Decoder != "" {
		fmt.Fprintf(w, "Audio decoder: %s\n%-24s %t\n", t.Decoder, "decoder:matched_profile", t.DecoderProfile)
	} else {
		fmt.Fprintln(w, "Matched audio decoder: not selected (make decoder or --decoder=PATH).")
	}
	fmt.Fprintln(w, "Capability presence does not certify Windows decoder or DSP parity.")
	return nil
}

func checkSource(ctx context.Context, t Tools, s *Source, w io.Writer) error {
	var streams []*Stream
	for i := range s.Probe.Streams {
		streams = append(streams, &s.Probe.Streams[i])
	}
	if err := prepareFLACChecks(ctx, t, s, streams); err != nil {
		return err
	}
	var maps []string
	checkedFLAC := false
	for i := range s.Probe.Streams {
		st := &s.Probe.Streams[i]
		if st.Codec == "flac" {
			if err := checkFLACSource(ctx, t, s, st, w); err != nil {
				return fmt.Errorf("track %d: %w", st.Index, err)
			}
			checkedFLAC = true
		} else if st.Type == "audio" || st.Type == "video" {
			maps = append(maps, "-map", fmt.Sprintf("0:%d", st.Index))
		}
	}
	if checkedFLAC && len(maps) == 0 {
		return nil
	}
	if len(maps) == 0 {
		maps = []string{"-map", "0:v?", "-map", "0:a?"}
	}
	args := []string{"-hide_banner", "-nostdin", "-v", "warning", "-xerror", "-err_detect", "crccheck+explode"}
	if s.Seek > 0 {
		args = append(args, "-ss", fmt.Sprintf("%.9f", s.Seek))
	}
	args = append(args, s.InputArgs...)
	args = append(args, maps...)
	if s.Limit > 0 {
		args = append(args, "-t", fmt.Sprintf("%.9f", s.Limit))
	}
	args = append(args, "-f", "null", "-")
	cmd := exec.CommandContext(ctx, t.FFmpeg, args...)
	cmd.Stderr = w
	cmd.Stdout = io.Discard
	if e := cmd.Run(); e != nil {
		return fmt.Errorf("media check failed: %w", e)
	}
	_, e := fmt.Fprintln(w, "Selected audio and video streams decoded without a reported error.")
	return e
}

func inputArgs(s *Source, fast bool) []string {
	args := []string{"-drc_scale", "0"}
	checks := "explode"
	if !fast {
		checks = "crccheck+explode"
	}
	args = append(args, "-err_detect", checks)
	if s.Seek > 0 {
		args = append(args, "-ss", fmt.Sprintf("%.9f", s.Seek))
	}
	args = append(args, s.InputArgs...)
	return args
}

func ensureRegularOutput(p string) error {
	fi, e := os.Stat(p)
	if e != nil {
		return e
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("output is not regular: %s", p)
	}
	if fi.Size() == 0 {
		return fmt.Errorf("track produced no data: %s", p)
	}
	return nil
}
