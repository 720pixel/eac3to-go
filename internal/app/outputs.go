package app

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
)

type Output struct {
	Request                OutputRequest
	Track                  Track
	Format                 string
	Codec                  string
	Copy                   bool
	BSF                    string
	Path, Stage            string
	RemoveDialnorm, Forced bool
	Depth                  int
	CacheCodec             string
	NativePCM              bool
	DecodeCache            bool
	PCMSourceDelay         *int64
	TrueHDPrepared         bool
	TrueHDDelayMS          int64
	TrueHDCompanion        bool
	muxTiming              *outputMuxTiming
}

func defaultExtension(s *Stream) string {
	switch s.Codec {
	case "h264":
		return "h264"
	case "hevc":
		return "h265"
	case "mpeg2video":
		return "m2v"
	case "vc1":
		return "vc1"
	case "av1":
		return "ivf"
	case "truehd":
		return "thd"
	case "dts":
		return "dts"
	case "hdmv_pgs_subtitle":
		return "sup"
	case "subrip":
		return "srt"
	case "ass", "ssa":
		return "ass"
	case "pcm_s16le", "pcm_s16be", "pcm_s24le", "pcm_s24be", "pcm_s32le", "pcm_bluray", "pcm_dvd":
		return "wav"
	case "vorbis":
		return "ogg"
	case "aac":
		return "aac"
	default:
		return s.Codec
	}
}

func buildOutputs(s *Source, o Options) ([]Output, error) {
	requests := o.Outputs
	if o.Demux || len(requests) == 1 && strings.Contains(requests[0].Path, "*") {
		prefix := s.Name
		defaults := o.Defaults
		if !o.Demux {
			if requests[0].Track != 0 {
				return nil, fmt.Errorf("wildcard output cannot have a track selector")
			}
			prefix = strings.TrimSuffix(requests[0].Path, ".*")
			if strings.Contains(prefix, "*") {
				return nil, fmt.Errorf("wildcard output must end in .* ")
			}
			defaults = requests[0].Options
		}
		requests = nil
		for _, t := range s.Tracks {
			if t.Hidden && !o.IncludeHidden {
				continue
			}
			suffix := "txt"
			description := "Chapters"
			if t.Stream != nil {
				suffix = defaultExtension(t.Stream)
				description = t.Stream.Codec
				if l := t.Stream.Tags["language"]; l != "" {
					description += " - " + l
				}
			}
			requests = append(requests, OutputRequest{Track: t.Number, Path: fmt.Sprintf("%s - %d - %s.%s", prefix, t.Number, safeName(description), suffix), Options: defaults})
		}
	}
	var outputs []Output
	for _, r := range requests {
		if r.Track == 0 {
			var candidates []Track
			for _, t := range s.Tracks {
				if t.Stream != nil {
					candidates = append(candidates, t)
				}
			}
			if len(candidates) != 1 {
				return nil, fmt.Errorf("input has %d streams; select a listed track with N: output", len(candidates))
			}
			r.Track = candidates[0].Number
		}
		var track *Track
		for i := range s.Tracks {
			if s.Tracks[i].Number == r.Track {
				track = &s.Tracks[i]
				break
			}
		}
		if track == nil {
			return nil, fmt.Errorf("track %d does not exist", r.Track)
		}
		if r.Path == "-" || ext(r.Path) == "stdout" {
			return nil, fmt.Errorf("stdout output still needs the reference pass and format rules")
		}
		if ext(r.Path) == "wavs" {
			if track.Stream == nil || track.Stream.Type != "audio" {
				return nil, fmt.Errorf(".wavs needs an audio track")
			}
			if r.Options.Mix != "" || len(r.Options.ChannelMap) > 0 {
				return nil, fmt.Errorf("channel splitting combined with channel remapping is not implemented")
			}
			labels := channelNames(track.Stream.Layout, track.Stream.Channels)
			if len(labels) != track.Stream.Channels {
				return nil, fmt.Errorf("cannot split an unknown channel layout")
			}
			for i, label := range labels {
				part := r
				part.Path = strings.TrimSuffix(r.Path, filepath.Ext(r.Path)) + "." + label + ".wav"
				part.Options.ExtractChannel = i
				out, e := makeOutput(s, *track, part, o)
				if e != nil {
					return nil, e
				}
				outputs = append(outputs, out)
			}
		} else {
			out, e := makeOutput(s, *track, r, o)
			if e != nil {
				return nil, e
			}
			outputs = append(outputs, out)
		}
	}
	if len(outputs) == 0 {
		return nil, fmt.Errorf("no outputs selected")
	}
	if err := validateOutputPaths(s, outputs, o); err != nil {
		return nil, err
	}
	return outputs, nil
}

func validateOutputPaths(s *Source, outputs []Output, o Options) error {
	seen := map[string]bool{}
	for _, out := range outputs {
		// TrueHD's final name carries its source+manual delay. It must be
		// resolved and validated before execution or a dry-run is returned.
		if out.Copy && out.Codec == "truehd" && out.Format == "truehd" && !out.TrueHDPrepared {
			continue
		}
		key, e := canonicalOutput(out.Path)
		if e != nil {
			return e
		}
		if seen[key] {
			return fmt.Errorf("duplicate output: %s", out.Path)
		}
		seen[key] = true
		fi, e := os.Lstat(out.Path)
		if e == nil {
			if !fi.Mode().IsRegular() {
				return fmt.Errorf("output is not a regular file: %s", out.Path)
			}
			if !o.Force {
				return fmt.Errorf("output exists: %s (use --force to replace it)", out.Path)
			}
		} else if !os.IsNotExist(e) {
			return e
		}
		for _, p := range s.Paths {
			source, e := filepath.EvalSymlinks(p)
			if e == nil && source == key {
				return fmt.Errorf("output would overwrite input: %s", out.Path)
			}
			if fi != nil {
				pi, e := os.Stat(p)
				if e == nil && os.SameFile(fi, pi) {
					return fmt.Errorf("output is a hard link to input: %s", out.Path)
				}
			}
		}
	}
	return nil
}

func makeOutput(s *Source, t Track, r OutputRequest, o Options) (Output, error) {
	out := Output{Request: r, Track: t}
	p := r.Path
	if o.Dest != "" && !filepath.IsAbs(p) {
		p = filepath.Join(o.Dest, p)
	}
	var e error
	out.Path, e = filepath.Abs(p)
	if e != nil {
		return out, e
	}
	if t.Kind == "chapters" {
		a := r.Options
		if a.DelayMS != 0 || a.SourceFPS != 0 || a.TargetFPS != 0 {
			return out, fmt.Errorf("chapter delay and frame-rate changes are not implemented; scope timing options to the audio/video outputs")
		}
		if audioTransform(a) || a.Core || a.Forced || a.Bitrate != 0 {
			return out, fmt.Errorf("audio/video processing options cannot be applied to chapters")
		}
		if ext(p) != "txt" && ext(p) != "xml" && ext(p) != "json" {
			return out, fmt.Errorf("chapters need .txt, .xml or .json")
		}
		out.Format = ext(p)
		return out, nil
	}
	st := t.Stream
	a := r.Options
	if st.Type == "video" {
		a, e = videoOptions(st, a)
		if e != nil {
			return out, e
		}
		out.Request.Options = a
	}
	if st.Type == "audio" && a.TargetFPS != 0 {
		known, err := sourceFrameRate(s)
		if err != nil {
			return out, err
		}
		if known != 0 {
			a.SourceFPS = known
		}
		if a.SourceFPS == 0 {
			return out, fmt.Errorf("changing audio frame rate requires the source frame rate, such as -23.976 or -25.000")
		}
		a.Speed, e = frameSpeed(a.SourceFPS, a.TargetFPS)
		if e != nil {
			return out, e
		}
		out.Request.Options = a
	}
	if a.Forced && st.Codec != "hdmv_pgs_subtitle" {
		return out, fmt.Errorf("-forced requires a PGS subtitle track")
	}
	if st.Type != "audio" && audioTransform(a) {
		return out, fmt.Errorf("audio options cannot be applied to track %d (%s)", t.Number, st.Type)
	}
	if st.Type != "audio" && st.Type != "video" && (a.TargetFPS != 0 || a.SourceFPS != 0) {
		return out, fmt.Errorf("frame-rate options require an audio or video track")
	}
	if a.Core {
		switch st.Codec {
		case "dts":
			out.BSF = "dca_core"
			if ext(p) != "dts" {
				return out, fmt.Errorf("DTS core output must be .dts")
			}
		case "truehd":
			var core *Stream
			for i := range s.Probe.Streams {
				c := &s.Probe.Streams[i]
				if c.Codec == "ac3" && ((c.ID != "" && c.ID == st.ID) || len(s.Probe.Streams) == 2 && strings.Contains(s.Probe.Format.Name, "truehd")) {
					core = c
					break
				}
			}
			if core == nil {
				return out, fmt.Errorf("this TrueHD stream has no separately demuxable AC-3 companion; -core cannot create one")
			}
			out.Track.Stream = core
			out.TrueHDCompanion = true
			st = core
			if ext(p) != "ac3" {
				return out, fmt.Errorf("TrueHD AC-3 companion output must be .ac3")
			}
		case "eac3":
			out.BSF = "eac3_core"
			if ext(p) != "ac3" && ext(p) != "eac3" {
				return out, fmt.Errorf("E-AC-3 core output must be .ac3 or .eac3")
			}
		default:
			return out, fmt.Errorf("-core is not available for %s", st.Codec)
		}
		if audioTransform(a) && !frameDelayOnly(a) {
			return out, fmt.Errorf("core extraction and PCM processing need separate commands")
		}
	}
	out.Depth = sampleDepth(st)
	if a.Depth != 0 {
		out.Depth = a.Depth
	}
	if a.HDCD {
		out.Depth = 24
	}
	if a.Full {
		out.Depth = 64
	}
	x := ext(p)
	switch x {
	case "h264", "264":
		out.Format = "h264"
		out.Codec = "h264"
	case "h265", "265", "hevc":
		out.Format = "hevc"
		out.Codec = "hevc"
	case "m2v", "mpv":
		out.Format = "mpeg2video"
		out.Codec = "mpeg2video"
	case "vc1":
		out.Format = "vc1"
		out.Codec = "vc1"
	case "ivf":
		out.Format = "ivf"
		out.Codec = st.Codec
	case "sup":
		out.Format = "sup"
		out.Codec = "hdmv_pgs_subtitle"
		out.Forced = a.Forced
	case "srt":
		out.Format = "srt"
		out.Codec = "subrip"
	case "ass", "ssa":
		out.Format = "ass"
		out.Codec = "ass"
	case "ac3", "eac3", "aac", "flac", "mp3", "dts", "mlp":
		out.Format = x
		out.Codec = x
		if x == "aac" {
			out.Format = "adts"
		}
	case "thd", "truehd":
		out.Format = "truehd"
		out.Codec = "truehd"
	case "dtshd", "dtsma":
		out.Format = "dts"
		out.Codec = "dts"
		if st.Codec != "dts" || audioTransform(a) && !frameDelayOnly(a) {
			return out, fmt.Errorf("DTS-HD encoding is unavailable; these extensions require unchanged DTS input")
		}
	case "wav", "w64", "rf64":
		out.Format = x
		if x == "rf64" {
			out.Format = "wav"
		}
		out.Codec = pcmCodec(out.Depth, false)
	case "raw", "pcm", "lpcm":
		out.Format = fmt.Sprintf("s%dle", storageDepth(out.Depth))
		out.Codec = pcmCodec(out.Depth, false)
		if out.Depth <= 8 {
			out.Format = "s8"
			out.Codec = "pcm_s8"
		}
		if a.Full {
			out.Format = "f64le"
		}
	case "mka", "mkv":
		out.Format = "matroska"
		out.Codec = st.Codec
	case "m4a", "mp4":
		out.Format = "mp4"
		if st.Type == "audio" {
			out.Codec = "aac"
		} else {
			out.Codec = st.Codec
		}
	case "opus":
		out.Format = "opus"
		out.Codec = "opus"
	case "ogg":
		out.Format = "ogg"
		out.Codec = "vorbis"
	default:
		return out, fmt.Errorf("unsupported output extension .%s", x)
	}
	if a.Core && st.Codec == "eac3" {
		// The reference accepts .ac3 while retaining independent E-AC-3 frames.
		out.Format, out.Codec = "eac3", "eac3"
	}
	if st.Type == "video" && out.Codec != st.Codec {
		return out, fmt.Errorf("video transcoding is outside this demuxer; %s cannot be written as %s", st.Codec, out.Codec)
	}
	if st.Type == "video" && (a.TargetFPS != 0 || a.SourceFPS != 0) && out.Format != "h264" {
		return out, fmt.Errorf("video frame-rate changes currently require elementary .h264 output; container timestamps need separate validation")
	}
	if st.Type == "subtitle" && out.Codec != st.Codec {
		return out, fmt.Errorf("subtitle conversion from %s to %s is not implemented", st.Codec, out.Codec)
	}
	out.Copy = out.Codec == st.Codec && !audioTransform(a) && a.Bitrate == 0
	if out.Codec == st.Codec && !strings.HasPrefix(st.Codec, "pcm_") && st.Codec != "flac" && frameDelayOnly(a) {
		if st.Codec != "ac3" && st.Codec != "dts" && st.Codec != "truehd" {
			return out, fmt.Errorf("compressed delay without re-encoding is not implemented for %s", st.Codec)
		}
		if out.Format != "ac3" && out.Format != "dts" && out.Format != "truehd" {
			return out, fmt.Errorf("compressed delay needs an elementary .%s output", st.Codec)
		}
		if a.DelayMS != float64(int64(a.DelayMS)) {
			return out, fmt.Errorf("compressed delay needs a whole number of milliseconds")
		}
		out.Copy = true
	}
	if strings.HasPrefix(st.Codec, "pcm_") && strings.HasPrefix(out.Codec, "pcm_") {
		out.Copy = out.Codec == st.Codec && !audioTransform(a)
	}
	if a.Core {
		out.Copy = true
	}
	if (st.Type == "video" || st.Type == "subtitle") && !out.Copy {
		return out, fmt.Errorf("cannot process track %d as .%s", t.Number, x)
	}
	if out.Codec == "flac" && out.Depth > 32 {
		return out, fmt.Errorf("FLAC cannot store 64-bit float samples")
	}
	if strings.HasPrefix(st.Codec, "pcm_f") && a.Depth == 0 && !a.Full {
		if out.Codec == "flac" {
			return out, fmt.Errorf("floating PCM needs an explicit output bit depth before FLAC conversion")
		}
		if out.Format == "wav" || out.Format == "w64" {
			out.Codec = st.Codec
			out.Copy = !audioTransform(a)
		}
	}
	out.RemoveDialnorm = !a.KeepDialnorm && (out.Codec == "ac3" || out.Codec == "eac3" || out.Codec == "dts" || out.Codec == "truehd")
	if out.RemoveDialnorm && out.Format != "ac3" && out.Format != "eac3" && out.Format != "dts" && out.Format != "truehd" {
		return out, fmt.Errorf("dialog normalization in a container needs an intermediate elementary stream; use -keepDialnorm or extract .%s", out.Codec)
	}
	return out, nil
}

func audioTransform(a AudioOptions) bool {
	return a.Depth != 0 || a.Full || a.Normalize || a.ForceSecondPass || a.Gain != 0 || a.DelayMS != 0 || a.Rate != 0 || a.Speed != 1 || a.Mix != "" || a.HDCD || len(a.ChannelMap) > 0 || a.ExtractChannel >= 0
}

func frameDelayOnly(a AudioOptions) bool {
	if a.DelayMS == 0 || a.Bitrate != 0 {
		return false
	}
	a.DelayMS = 0
	return !audioTransform(a)
}
func sampleDepth(s *Stream) int {
	if n, e := strconv.Atoi(s.RawBits); e == nil && n > 0 {
		return n
	}
	if s.Bits > 0 {
		return s.Bits
	}
	if s.SampleFormat == "s16" || s.SampleFormat == "s16p" {
		return 16
	}
	return 24
}
func storageDepth(n int) int {
	if n <= 8 {
		return 8
	}
	if n <= 16 {
		return 16
	}
	if n <= 24 {
		return 24
	}
	return 32
}
func pcmCodec(n int, big bool) string {
	if n == 64 {
		return "pcm_f64le"
	}
	end := "le"
	if big {
		end = "be"
	}
	if n <= 8 {
		return "pcm_u8"
	}
	return fmt.Sprintf("pcm_s%d%s", storageDepth(n), end)
}
func safeName(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) || strings.ContainsRune(`/\:*?"<>|`, r) {
			b.WriteByte('_')
		} else {
			b.WriteRune(r)
		}
		if b.Len() > 100 {
			break
		}
	}
	s = strings.Trim(b.String(), " .")
	if s == "" {
		return "track"
	}
	return s
}
func canonicalOutput(p string) (string, error) {
	parent := filepath.Dir(p)
	for {
		real, e := filepath.EvalSymlinks(parent)
		if e == nil {
			rel, _ := filepath.Rel(parent, p)
			return filepath.Join(real, rel), nil
		}
		if !os.IsNotExist(e) {
			return "", e
		}
		next := filepath.Dir(parent)
		if next == parent {
			return "", e
		}
		parent = next
	}
}

func channelNames(layout string, n int) []string {
	known := map[string][]string{"mono": {"FC"}, "stereo": {"FL", "FR"}, "2.1": {"FL", "FR", "LFE"}, "3.0": {"FL", "FR", "FC"}, "3.0(back)": {"FL", "FR", "BC"}, "quad": {"FL", "FR", "BL", "BR"}, "quad(side)": {"FL", "FR", "SL", "SR"}, "4.0": {"FL", "FR", "FC", "BC"}, "5.0": {"FL", "FR", "FC", "BL", "BR"}, "5.0(side)": {"FL", "FR", "FC", "SL", "SR"}, "5.1": {"FL", "FR", "FC", "LFE", "BL", "BR"}, "5.1(side)": {"FL", "FR", "FC", "LFE", "SL", "SR"}, "6.1": {"FL", "FR", "FC", "LFE", "BC", "SL", "SR"}, "6.1(back)": {"FL", "FR", "FC", "LFE", "BL", "BR", "BC"}, "7.1": {"FL", "FR", "FC", "LFE", "BL", "BR", "SL", "SR"}}
	if v := known[layout]; len(v) == n {
		return v
	}
	if n == 1 {
		return []string{"FC"}
	}
	if n == 2 {
		return []string{"FL", "FR"}
	}
	return nil
}
