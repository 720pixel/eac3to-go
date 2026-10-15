package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"eac3to/internal/bluray"
	"eac3to/internal/toolinstall"
)

type Tools struct {
	FFmpeg, FFprobe, Decoder string
	DecoderProfile           bool
}
type Stream struct {
	Index        int               `json:"index"`
	Codec        string            `json:"codec_name"`
	Type         string            `json:"codec_type"`
	Profile      string            `json:"profile"`
	SampleFormat string            `json:"sample_fmt"`
	SampleRate   string            `json:"sample_rate"`
	Channels     int               `json:"channels"`
	Layout       string            `json:"channel_layout"`
	Bits         int               `json:"bits_per_sample"`
	RawBits      string            `json:"bits_per_raw_sample"`
	Width        int               `json:"width"`
	Height       int               `json:"height"`
	FrameRate    string            `json:"r_frame_rate"`
	HeaderFPS    float64           `json:"header_fps,omitempty"`
	ID           string            `json:"id"`
	Start        string            `json:"start_time"`
	Duration     string            `json:"duration"`
	Extradata    string            `json:"-"`
	BitRate      string            `json:"bit_rate"`
	Tags         map[string]string `json:"tags"`
	Disposition  map[string]int    `json:"disposition"`
	flacFrames   *uint64
}

func (s *Stream) UnmarshalJSON(data []byte) error {
	type streamFields Stream
	var raw struct {
		streamFields
		Extradata string `json:"extradata"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	*s = Stream(raw.streamFields)
	s.Extradata = raw.Extradata
	return nil
}

type Chapter struct {
	Start string            `json:"start_time"`
	End   string            `json:"end_time"`
	Tags  map[string]string `json:"tags"`
}
type Probe struct {
	Streams  []Stream  `json:"streams"`
	Chapters []Chapter `json:"chapters"`
	Format   struct {
		Name     string            `json:"format_name"`
		Duration string            `json:"duration"`
		Start    string            `json:"start_time"`
		Tags     map[string]string `json:"tags"`
	} `json:"format"`
}
type Track struct {
	Number int     `json:"number"`
	Kind   string  `json:"kind"`
	Stream *Stream `json:"stream,omitempty"`
	Hidden bool    `json:"hidden,omitempty"`
}
type Source struct {
	Paths          []string
	InputArgs      []string
	Probe          Probe
	Tracks         []Track
	Name           string
	Playlist       *bluray.Playlist
	Seek, Limit    float64
	TempDir        string
	PCMSourceDelay *int64
	Metadata       bluray.PlaylistMetadata
	nativeScans    []clipScan
	nativeTiming   *initialTransportTiming
}

func findTools(o Options) (Tools, error) {
	t := Tools{FFmpeg: o.FFmpeg, FFprobe: o.FFprobe}
	if o.ToolsDir != "" {
		if t.FFmpeg == "" {
			t.FFmpeg = filepath.Join(o.ToolsDir, executableName("ffmpeg"))
		}
		if t.FFprobe == "" {
			t.FFprobe = filepath.Join(o.ToolsDir, executableName("ffprobe"))
		}
	}
	if t.FFmpeg == "" {
		t.FFmpeg = os.Getenv("EAC3TO_FFMPEG")
	}
	if t.FFprobe == "" {
		t.FFprobe = os.Getenv("EAC3TO_FFPROBE")
	}
	if t.FFmpeg == "" && t.FFprobe == "" {
		// Keep both tools from the same installation when possible.
		dirs := filepath.SplitList(os.Getenv("PATH"))
		if self, err := os.Executable(); err == nil {
			if resolved, err := filepath.EvalSymlinks(self); err == nil {
				self = resolved
			}
			base := filepath.Dir(self)
			dirs = append([]string{base, filepath.Join(base, "tools")}, dirs...)
		}
		if installed, err := toolinstall.DefaultDir(); err == nil {
			dirs = append(dirs, installed)
		}
		t.FFmpeg, t.FFprobe = findToolPair(dirs)
	}
	if t.FFmpeg == "" {
		t.FFmpeg = "ffmpeg"
	}
	if t.FFprobe == "" {
		t.FFprobe = filepath.Join(filepath.Dir(t.FFmpeg), executableName("ffprobe"))
		if !executable(t.FFprobe) {
			t.FFprobe = "ffprobe"
		}
	}
	f, ferr := exec.LookPath(t.FFmpeg)
	p, perr := exec.LookPath(t.FFprobe)
	var missing []string
	if ferr != nil {
		missing = append(missing, "FFmpeg")
	}
	if perr != nil {
		missing = append(missing, "FFprobe")
	}
	if len(missing) > 0 {
		return t, fmt.Errorf("%s required but not found. Put a matching FFmpeg/FFprobe pair on PATH or beside eac3to, use --ffmpeg=PATH and --ffprobe=PATH, or run eac3to --download-tools. Blu-ray input requires libbluray support", strings.Join(missing, " and "))
	}
	t.FFmpeg, _ = filepath.Abs(f)
	t.FFprobe, _ = filepath.Abs(p)
	if err := findDecoder(o, &t); err != nil {
		return t, err
	}
	return t, nil
}

func findToolPair(dirs []string) (ffmpeg, ffprobe string) {
	for _, dir := range dirs {
		if dir == "" {
			continue
		}
		f := filepath.Join(dir, executableName("ffmpeg"))
		p := filepath.Join(dir, executableName("ffprobe"))
		if executable(f) && executable(p) {
			return f, p
		}
	}
	return "", ""
}

type limitedBuffer struct {
	bytes.Buffer
	Limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.Limit {
		return 0, fmt.Errorf("tool output exceeded %d bytes", b.Limit)
	}
	return b.Buffer.Write(p)
}

type tailBuffer struct {
	data  []byte
	Limit int
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) >= b.Limit {
		b.data = append(b.data[:0], p[len(p)-b.Limit:]...)
	} else {
		excess := len(b.data) + len(p) - b.Limit
		if excess > 0 {
			b.data = append(b.data[:0], b.data[excess:]...)
		}
		b.data = append(b.data, p...)
	}
	return n, nil
}
func (b *tailBuffer) String() string { return string(b.data) }

func probe(ctx context.Context, t Tools, args []string) (Probe, error) {
	p := Probe{}
	a := []string{"-v", "error", "-probesize", "100M", "-analyzeduration", "100M", "-show_streams", "-show_data", "-show_format", "-show_chapters", "-of", "json"}
	a = append(a, args...)
	cmd := exec.CommandContext(ctx, t.FFprobe, a...)
	var out limitedBuffer
	out.Limit = 16 << 20
	errout := tailBuffer{Limit: 32 << 10}
	cmd.Stdout = &out
	cmd.Stderr = &errout
	if e := cmd.Run(); e != nil {
		for _, arg := range args {
			if strings.HasPrefix(arg, "bluray:") && strings.Contains(errout.String(), "Protocol not found") {
				return p, fmt.Errorf("Blu-ray input requires FFprobe with libbluray support; use --ffprobe=PATH or --download-tools --tools-dir=NEW_DIRECTORY: %w\n%s", e, errout.String())
			}
		}
		return p, fmt.Errorf("ffprobe: %w\n%s", e, errout.String())
	}
	if e := json.Unmarshal(out.Bytes(), &p); e != nil {
		return p, fmt.Errorf("invalid ffprobe response: %w", e)
	}
	if len(p.Streams) == 0 {
		return p, fmt.Errorf("input has no readable streams")
	}
	return p, nil
}

func openSource(ctx context.Context, t Tools, o Options, play *bluray.Playlist) (*Source, error) {
	s := &Source{Paths: append([]string(nil), o.Inputs...)}
	var metadata bluray.PlaylistMetadata
	if len(s.Paths) == 0 {
		return nil, fmt.Errorf("an input file or disc folder is required")
	}
	for i, p := range s.Paths {
		abs, e := filepath.Abs(p)
		if e != nil {
			return nil, e
		}
		s.Paths[i] = abs
		fi, e := os.Stat(abs)
		if e != nil {
			return nil, e
		}
		if !fi.Mode().IsRegular() && play == nil {
			return nil, fmt.Errorf("input is not a regular file: %s", abs)
		}
	}
	if play != nil {
		metadata = bluray.ReadPlaylistMetadata(play)
		s.Playlist = play
		s.Name = strings.TrimSuffix(filepath.Base(play.Path), filepath.Ext(play.Path))
		clips, e := bluray.ResolveClips(play)
		if e != nil {
			return nil, e
		}
		s.Paths = append(s.Paths, clips...)
		s.Playlist, s.Seek, e = selectPlaylistItems(play, o)
		if e != nil {
			return nil, e
		}
		if s.Playlist != play {
			s.Limit = s.Playlist.Duration()
		}
		root := playlistDiscRoot(play.Path)
		n, e := strconv.Atoi(s.Name)
		if e != nil {
			return nil, fmt.Errorf("playlist filename must be a five-digit number")
		}
		s.InputArgs = []string{"-playlist", strconv.Itoa(n), "-angle", "0", "-i", "bluray:" + root}
	} else {
		s.Name = strings.TrimSuffix(filepath.Base(s.Paths[0]), filepath.Ext(s.Paths[0]))
		raw := ext(s.Paths[0])
		if raw == "raw" || raw == "pcm" || raw == "lpcm" {
			if !o.RawOverride || o.RawDepth == 0 || o.RawRate == 0 || o.RawChannels == 0 {
				return nil, fmt.Errorf("raw PCM needs -override, bit depth, sample rate, channels and endianness (for example -24 -48000 -6 -little)")
			}
			endian := "le"
			if o.RawEndian == "big" {
				endian = "be"
			}
			s.InputArgs = []string{"-f", fmt.Sprintf("s%d%s", o.RawDepth, endian), "-ar", strconv.Itoa(o.RawRate), "-ac", strconv.Itoa(o.RawChannels)}
		}
		if len(s.Paths) == 1 {
			s.InputArgs = append(s.InputArgs, "-i", s.Paths[0])
		} else {
			for _, p := range s.Paths {
				if strings.ContainsAny(p, "|\r\n") {
					return nil, fmt.Errorf("joined input filename contains an unsupported separator: %q", p)
				}
			}
			// Physical joins are only valid for byte-concatenable elementary and transport streams.
			switch ext(s.Paths[0]) {
			case "ts", "m2ts", "mts", "vob", "evo", "ac3", "eac3", "dts", "dtshd", "dtsma", "thd", "mlp", "h264", "264", "h265", "265", "hevc", "vc1", "m2v", "raw", "pcm", "lpcm":
			default:
				return nil, fmt.Errorf("joining %s containers needs a timestamp-aware implementation", ext(s.Paths[0]))
			}
			s.InputArgs = append(s.InputArgs, "-i", "concat:"+strings.Join(s.Paths, "|"))
		}
	}
	p, e := probe(ctx, t, s.InputArgs)
	if e != nil {
		return nil, e
	}
	s.Probe = p
	s.Metadata = metadata
	if s.Playlist != nil {
		s.Probe.Chapters = playlistChapters(s.Playlist)
		s.Probe.Format.Duration = strconv.FormatFloat(s.Playlist.Duration(), 'f', 9, 64)
	}
	if len(s.Probe.Chapters) > 0 {
		s.Tracks = append(s.Tracks, Track{Number: 1, Kind: "chapters"})
	}
	var listed map[uint16]bool
	languages := map[uint16]string{}
	if s.Playlist != nil && len(s.Playlist.Items) > 0 {
		listed = map[uint16]bool{}
		for _, item := range s.Playlist.Items {
			for pid := range item.ListedPIDs() {
				listed[pid] = true
			}
			for _, stream := range item.Streams {
				if stream.EntryType == 1 && stream.Language != "" && languages[stream.PID] == "" {
					languages[stream.PID] = stream.Language
				}
			}
		}
	}
	for i := range s.Probe.Streams {
		stream := &s.Probe.Streams[i]
		if stream.Type != "audio" && stream.Type != "video" && stream.Type != "subtitle" {
			continue
		}
		track := Track{Number: len(s.Tracks) + 1, Kind: stream.Type, Stream: stream}
		if listed != nil {
			pid, e := strconv.ParseUint(stream.ID, 0, 16)
			if e == nil {
				track.Hidden = !listed[uint16(pid)]
				if language := languages[uint16(pid)]; language != "" {
					if stream.Tags == nil {
						stream.Tags = map[string]string{}
					}
					stream.Tags["language"] = language
					stream.Tags["language_source"] = "MPLS STN"
				}
				applyAuthoredTrackMetadata(stream, uint16(pid), metadata)
			}
		}
		s.Tracks = append(s.Tracks, track)
	}
	return s, nil
}

func playlistDiscRoot(path string) string {
	bdmv := filepath.Dir(filepath.Dir(path))
	if strings.EqualFold(filepath.Base(bdmv), "BACKUP") {
		bdmv = filepath.Dir(bdmv)
	}
	return filepath.Dir(bdmv)
}

func discover(o Options) ([]*bluray.Playlist, error) {
	if len(o.Inputs) != 1 {
		return nil, nil
	}
	fi, e := os.Stat(o.Inputs[0])
	if e != nil {
		return nil, e
	}
	if !fi.IsDir() && ext(o.Inputs[0]) != "mpls" {
		return nil, nil
	}
	list, e := bluray.Discover(o.Inputs[0])
	if e != nil {
		return nil, e
	}
	if fi.IsDir() {
		sort.SliceStable(list, func(i, j int) bool {
			if list[i].DurationTicks() == list[j].DurationTicks() {
				return list[i].Name < list[j].Name
			}
			return list[i].DurationTicks() > list[j].DurationTicks()
		})
		if o.MinDuration > 0 {
			filtered := list[:0]
			for _, p := range list {
				if p.Duration() >= o.MinDuration {
					filtered = append(filtered, p)
				}
			}
			list = filtered
		}
	}
	return list, nil
}
func printTracks(w io.Writer, s *Source, jsonMode bool) error {
	tracks := append([]Track(nil), s.Tracks...)
	for i := range tracks {
		st := tracks[i].Stream
		if st == nil || st.Codec != "h264" {
			continue
		}
		if fps, err := h264SourceFrameRate(st); err == nil {
			listed := *st
			listed.HeaderFPS = fps
			tracks[i].Stream = &listed
		}
	}
	if jsonMode {
		return json.NewEncoder(w).Encode(struct {
			Name     string                  `json:"name"`
			Duration string                  `json:"duration"`
			Tracks   []Track                 `json:"tracks"`
			Chapters []Chapter               `json:"chapters,omitempty"`
			Metadata bluray.PlaylistMetadata `json:"authored_metadata,omitempty"`
		}{s.Name, s.Probe.Format.Duration, tracks, s.Probe.Chapters, s.Metadata})
	}
	duration, _ := strconv.ParseFloat(s.Probe.Format.Duration, 64)
	fmt.Fprintf(w, "%s — %s\n", s.Name, timestamp(duration))
	for _, t := range tracks {
		if t.Kind == "chapters" {
			fmt.Fprintf(w, "%d: Chapters, %d chapters\n", t.Number, len(s.Probe.Chapters))
			continue
		}
		st := t.Stream
		mark := ""
		if t.Hidden {
			mark = " *"
		}
		fmt.Fprintf(w, "%d:%s %s", t.Number, mark, st.Codec)
		if st.Profile != "" {
			fmt.Fprintf(w, " (%s)", st.Profile)
		}
		if st.Type == "audio" {
			layout := st.Layout
			if layout == "" {
				layout = fmt.Sprintf("%d channels", st.Channels)
			}
			fmt.Fprintf(w, ", %s, %s Hz", layout, st.SampleRate)
			if depth := sampleDepth(st); depth > 0 && (strings.HasPrefix(st.Codec, "pcm_") || st.Codec == "flac" || st.Codec == "truehd" || st.Codec == "dts") {
				fmt.Fprintf(w, ", %d-bit", depth)
			}
		}
		if st.Type == "video" {
			if st.HeaderFPS > 0 {
				fmt.Fprintf(w, ", %dx%d, %.6g fps (header clock)", st.Width, st.Height, st.HeaderFPS)
			} else {
				fmt.Fprintf(w, ", %dx%d, %s fps (reported)", st.Width, st.Height, st.FrameRate)
			}
		}
		if l := st.Tags["language"]; l != "" {
			fmt.Fprintf(w, ", %s", l)
		}
		if title := st.Tags["title"]; title != "" {
			fmt.Fprintf(w, ", %s", strings.ReplaceAll(title, "\n", " "))
		}
		fmt.Fprintln(w)
	}
	return nil
}
func timestamp(seconds float64) string {
	ms := int64(seconds*1000 + 0.5)
	if ms < 0 {
		ms = 0
	}
	return fmt.Sprintf("%02d:%02d:%02d.%03d", ms/3600000, (ms/60000)%60, (ms/1000)%60, ms%1000)
}
