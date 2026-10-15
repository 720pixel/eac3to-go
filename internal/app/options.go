package app

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

const Version = "0.1.0-alpha.2"

type AudioOptions struct {
	Depth                                          int
	Full, Normalize, NoSecondPass, ForceSecondPass bool
	Gain, DelayMS                                  float64
	Rate                                           int
	Speed                                          float64
	SourceFPS, TargetFPS                           float64
	Mix                                            string
	MixLFE, HDCD, Core, Forced                     bool
	KeepDialnorm                                   bool
	ChannelMap                                     []int
	Bitrate                                        int
	Silence, Loop                                  bool
	Steep                                          bool
	ExtractChannel                                 int // -1 means no channel extraction
	ReferenceTail                                  bool
}

type OutputRequest struct {
	Track   int
	Path    string
	Options AudioOptions
}
type Options struct {
	Inputs                                                       []string
	Outputs                                                      []OutputRequest
	Defaults                                                     AudioOptions
	Help, Version, Test, Demux, JSON, DryRun, Force, Quiet       bool
	DownloadTools                                                bool
	ToolsDir                                                     string
	Check, SkipFirst, SkipLast, StrictPL, IncludeHidden, ShowAll bool
	MinDuration                                                  float64
	Title                                                        int
	Dest, FFmpeg, FFprobe                                        string
	Report                                                       string
	Interactive, NonInteractive                                  bool
	Decoder                                                      string
	referenceDecoder                                             bool
	RawDepth, RawRate, RawChannels                               int
	RawEndian                                                    string
	RawOverride                                                  bool
	Fast                                                         bool
	AllowUnverified                                              bool
	Main                                                         bool
	Playlist                                                     string
	Jobs, Threads                                                int
	CodecThreads                                                 int
	Segments                                                     bool
	ComparePlaylists                                             []string
	Collapse                                                     int
	AuditPL                                                      bool
	Explain                                                      bool
	Language                                                     string
}

var selectorRE = regexp.MustCompile(`^(\d+):(.*)$`)
var titleRE = regexp.MustCompile(`^(\d+)\)$`)
var numberRE = regexp.MustCompile(`^[+-]?(?:\d+(?:\.\d*)?|\.\d+)$`)

func ParseOptions(args []string) (Options, error) {
	o := Options{Defaults: AudioOptions{Speed: 1, ExtractChannel: -1}, RawEndian: "little", Title: -1}
	current := &o.Defaults
	for i := 0; i < len(args); i++ {
		a := args[i]
		lower := strings.ToLower(a)
		if m := selectorRE.FindStringSubmatch(a); m != nil {
			n, err := strconv.Atoi(m[1])
			if err != nil || n < 1 {
				return o, fmt.Errorf("invalid track selector %q", a)
			}
			p := m[2]
			if p == "" {
				i++
				if i == len(args) {
					return o, fmt.Errorf("missing output after %s", a)
				}
				p = args[i]
			}
			if len(o.Inputs) == 0 {
				return o, fmt.Errorf("provide an input before a track selector")
			}
			o.Outputs = append(o.Outputs, OutputRequest{Track: n, Path: p, Options: o.Defaults})
			current = &o.Outputs[len(o.Outputs)-1].Options
			continue
		}
		if m := titleRE.FindStringSubmatch(a); m != nil {
			n, err := strconv.Atoi(m[1])
			if err != nil || n < 1 {
				return o, fmt.Errorf("invalid title selector %q", a)
			}
			o.Title = n - 1
			continue
		}
		key, val, hasVal := strings.Cut(lower, "=")
		_, originalVal, _ := strings.Cut(a, "=")
		needVal := func() (string, error) {
			if hasVal && originalVal != "" {
				return originalVal, nil
			}
			return "", fmt.Errorf("%s requires =VALUE", key)
		}
		switch key {
		case "--interactive":
			o.Interactive = true
		case "--non-interactive":
			o.NonInteractive = true
		case "-h", "--help", "-help", "-?":
			o.Help = true
		case "--version", "-version":
			o.Version = true
		case "-test", "--doctor":
			o.Test = true
		case "--download-tools":
			o.DownloadTools = true
		case "--tools-dir":
			v, e := needVal()
			if e != nil {
				return o, e
			}
			o.ToolsDir = v
		case "-demux":
			o.Demux = true
		case "--json":
			o.JSON = true
		case "--dry-run":
			o.DryRun = true
		case "--report":
			v, e := needVal()
			if e != nil {
				return o, e
			}
			o.Report = v
		case "--allow-unverified":
			o.AllowUnverified = true
		case "--main":
			o.Main = true
		case "--segments":
			o.Segments = true
		case "--explain":
			o.Explain = true
		case "--language":
			v, e := needVal()
			if e != nil {
				return o, e
			}
			v = strings.ToLower(v)
			if len(v) != 3 || strings.Trim(v, "abcdefghijklmnopqrstuvwxyz") != "" {
				return o, fmt.Errorf("--language needs a three-letter language code, such as eng or spa")
			}
			o.Language = v
		case "--compare-playlists":
			v, e := needVal()
			if e != nil {
				return o, e
			}
			seen := map[string]bool{}
			for _, id := range strings.Split(v, ",") {
				n, e := strconv.Atoi(strings.TrimSuffix(strings.ToLower(id), ".mpls"))
				if e != nil || n < 0 || n > 99999 {
					return o, fmt.Errorf("invalid playlist number %q", id)
				}
				name := fmt.Sprintf("%05d.mpls", n)
				if seen[name] {
					return o, fmt.Errorf("playlist %s is repeated", name)
				}
				seen[name] = true
				o.ComparePlaylists = append(o.ComparePlaylists, name)
			}
			if len(o.ComparePlaylists) < 2 || len(o.ComparePlaylists) > 8 {
				return o, fmt.Errorf("compare between 2 and 8 playlists")
			}
		case "--playlist":
			v, e := needVal()
			if e != nil {
				return o, e
			}
			v = strings.TrimSuffix(strings.ToLower(v), ".mpls")
			n, e := strconv.Atoi(v)
			if e != nil || n < 0 || n > 99999 {
				return o, fmt.Errorf("--playlist needs a number from 00000 to 99999")
			}
			o.Playlist = fmt.Sprintf("%05d.mpls", n)
		case "--jobs", "--threads":
			v, e := needVal()
			if e != nil {
				return o, e
			}
			n, e := strconv.Atoi(v)
			if e != nil || n < 1 || n > 1024 {
				return o, fmt.Errorf("%s needs a number from 1 to 1024", key)
			}
			if key == "--jobs" {
				o.Jobs = n
			} else {
				o.Threads = n
			}
		case "--force", "-y":
			o.Force = true
		case "--quiet", "-quiet":
			o.Quiet = true
		case "-destpath":
			v, e := needVal()
			if e != nil {
				return o, e
			}
			o.Dest = v
		case "--ffmpeg":
			v, e := needVal()
			if e != nil {
				return o, e
			}
			o.FFmpeg = v
		case "--ffprobe":
			v, e := needVal()
			if e != nil {
				return o, e
			}
			o.FFprobe = v
		case "--decoder":
			v, e := needVal()
			if e != nil {
				return o, e
			}
			o.Decoder = v
		case "-check":
			o.Check = true
		case "-sf":
			o.SkipFirst = true
		case "-sl":
			o.SkipLast = true
		case "-strictpl":
			o.StrictPL = true
		case "-auditpl":
			o.AuditPL = true
		case "-collapse":
			v, e := needVal()
			if e != nil {
				return o, e
			}
			n, e := strconv.Atoi(v)
			if e != nil || n < 1 || n > 65535 {
				return o, fmt.Errorf("-collapse needs a segment count from 1 to 65535")
			}
			o.Collapse = n
		case "-include_hidden":
			o.IncludeHidden = true
		case "-showall":
			o.ShowAll = true
		case "-minduration":
			v, e := positiveFloat(val, true)
			if e != nil {
				return o, fmt.Errorf("invalid -minDuration: %w", e)
			}
			o.MinDuration = v
		case "-fast":
			o.Fast = true
		case "-normal":
			o.Fast = false
		case "-libav": // This backend is always used for decoding.
		case "-progressnumbers": // Progress is line-oriented by default.
		case "-keepdialnorm":
			o.Defaults.KeepDialnorm = true
			for j := range o.Outputs {
				o.Outputs[j].Options.KeepDialnorm = true
			}
		case "-removedialnorm":
			o.Defaults.KeepDialnorm = false
			for j := range o.Outputs {
				o.Outputs[j].Options.KeepDialnorm = false
			}
		case "-core":
			current.Core = true
		case "-forced":
			current.Forced = true
		case "-normalize":
			current.Normalize = true
		case "-2pass":
			current.ForceSecondPass = true
		case "-no2ndpass":
			current.NoSecondPass = true
		case "-full":
			current.Full = true
		case "-decodehdcd":
			current.HDCD = true
		case "-mono":
			current.Mix = "mono"
		case "-downstereo":
			current.Mix = "stereo"
		case "-downdpl", "-down2":
			current.Mix = "dplii"
		case "-down6":
			current.Mix = "5.1"
		case "-double7":
			current.Mix = "double7"
		case "-mixlfe":
			current.MixLFE = true
		case "-silence":
			current.Silence = true
		case "-loop":
			current.Loop = true
		case "-steep":
			current.Steep = true
		case "--reference-tail":
			current.ReferenceTail = true
		case "-slowdown":
			if current.SourceFPS == 0 {
				current.SourceFPS = 25
			}
			current.TargetFPS = 24000.0 / 1001
			current.Speed = 24000.0 / 25025.0
		case "-speedup":
			if current.SourceFPS == 0 {
				current.SourceFPS = 24000.0 / 1001
			}
			current.TargetFPS = 25
			current.Speed = 25025.0 / 24000.0
		case "-override":
			o.RawOverride = true
		case "-little", "-big":
			o.RawEndian = key[1:]
		case "-keeppulldown": // Stream-copy currently preserves pulldown metadata.
		default:
			switch {
			case strings.HasPrefix(lower, "-down"):
				n, e := strconv.Atoi(lower[5:])
				if e != nil || n < 8 || n > 32 {
					return o, fmt.Errorf("unsupported bit depth option %s", a)
				}
				current.Depth = n
			case strings.HasPrefix(lower, "-resampleto"):
				n, e := strconv.Atoi(lower[11:])
				if e != nil || n < 8000 || n > 768000 {
					return o, fmt.Errorf("invalid sample rate %s", a)
				}
				current.Rate = n
			case strings.HasPrefix(lower, "-changeto") || strings.HasPrefix(lower, "-speedupto") || strings.HasPrefix(lower, "-slowdownto"):
				_, suffix, _ := strings.Cut(lower, "to")
				v, e := frameRate(suffix)
				if e != nil {
					return o, e
				}
				current.TargetFPS = v
			case strings.HasSuffix(lower, "ms") && numberRE.MatchString(lower[:len(lower)-2]):
				v, e := strconv.ParseFloat(lower[:len(lower)-2], 64)
				if e != nil || math.Abs(v) > 86400000 {
					return o, fmt.Errorf("invalid delay %s", a)
				}
				current.DelayMS = v
			case strings.HasSuffix(lower, "db") && numberRE.MatchString(lower[:len(lower)-2]):
				v, e := strconv.ParseFloat(lower[:len(lower)-2], 64)
				if e != nil || math.Abs(v) > 200 {
					return o, fmt.Errorf("invalid gain %s", a)
				}
				current.Gain = v
			case strings.HasPrefix(lower, "-") && strings.Contains(lower, ","):
				for _, s := range strings.Split(lower[1:], ",") {
					n, e := strconv.Atoi(s)
					if e != nil || n < 0 || n > 63 {
						return o, fmt.Errorf("invalid channel map %s", a)
					}
					current.ChannelMap = append(current.ChannelMap, n)
				}
			case strings.HasPrefix(lower, "-") && numberRE.MatchString(lower[1:]):
				s := lower[1:]
				if strings.Contains(s, ".") {
					v, e := frameRate(s)
					if e != nil {
						return o, e
					}
					current.SourceFPS = v
					break
				}
				n, e := strconv.Atoi(s)
				if e != nil {
					return o, fmt.Errorf("invalid numeric option %s", a)
				}
				switch {
				case n >= 1 && n <= 8:
					o.RawChannels = n
				case n == 16 || n == 24 || n == 32:
					o.RawDepth = n
				case n >= 8000 && n <= 768000:
					o.RawRate = n
				case n >= 32 && n <= 6144:
					current.Bitrate = n
				default:
					return o, fmt.Errorf("unsupported numeric option %s", a)
				}
			case strings.HasPrefix(a, "-") || strings.HasPrefix(a, "+") && a != "+":
				return o, fmt.Errorf("option %q has no verified native implementation yet; refusing to ignore it", a)
			case a == "+":
				i++
				if i == len(args) || len(o.Inputs) == 0 || len(o.Outputs) > 0 {
					return o, fmt.Errorf("+ must join input filenames")
				}
				o.Inputs = append(o.Inputs, args[i])
			default:
				if len(o.Inputs) == 0 {
					o.Inputs = splitInput(a)
				} else {
					o.Outputs = append(o.Outputs, OutputRequest{Path: a, Options: o.Defaults})
					current = &o.Outputs[len(o.Outputs)-1].Options
				}
			}
		}
	}
	if len(args) == 0 {
		o.Help = true
	}
	if o.Interactive && o.NonInteractive {
		return o, fmt.Errorf("--interactive and --non-interactive are mutually exclusive")
	}
	for _, option := range append([]*AudioOptions{&o.Defaults}, outputAudioOptions(o.Outputs)...) {
		if option.SourceFPS != 0 && option.TargetFPS != 0 {
			var err error
			option.Speed, err = frameSpeed(option.SourceFPS, option.TargetFPS)
			if err != nil {
				return o, err
			}
		}
	}
	selectors := 0
	if o.Title >= 0 {
		selectors++
	}
	if o.Main {
		selectors++
	}
	if o.Playlist != "" {
		selectors++
	}
	if selectors > 1 {
		return o, fmt.Errorf("use one title selector: N), --main, or --playlist=NNNNN")
	}
	if o.Language != "" && (o.Playlist != "" || o.Title >= 0 || len(o.ComparePlaylists) > 0) {
		return o, fmt.Errorf("--language applies to automatic playlist selection; an explicit playlist takes precedence")
	}
	if len(o.Inputs) == 1 && ext(o.Inputs[0]) == "mpls" && (o.Language != "" || o.Explain) {
		return o, fmt.Errorf("playlist ranking and language preferences need a disc folder, not a single MPLS file")
	}
	if o.Explain && (selectors > 0 || o.Demux || len(o.Outputs) > 0 || o.Check || o.Segments || o.AuditPL || len(o.ComparePlaylists) > 0) {
		return o, fmt.Errorf("--explain shows playlist ranking; use it with a disc folder and optional --language or -showall")
	}
	if o.Demux && len(o.Outputs) > 0 {
		return o, fmt.Errorf("use either -demux or explicit outputs")
	}
	if o.Demux {
		for i := range o.Outputs {
			o.Outputs[i].Options = o.Defaults
		}
	}
	return o, nil
}

func splitInput(s string) []string {
	if _, err := os.Stat(s); err == nil {
		return []string{s}
	}
	return strings.Split(s, "+")
}
func positiveFloat(s string, zero bool) (float64, error) {
	v, e := strconv.ParseFloat(s, 64)
	if e != nil || math.IsNaN(v) || math.IsInf(v, 0) || v < 0 || !zero && v == 0 {
		return 0, fmt.Errorf("expected a positive finite number")
	}
	return v, nil
}
func frameRate(s string) (float64, error) {
	v, e := positiveFloat(s, false)
	if e != nil || v > 240 || math.Abs(v*1000-math.Round(v*1000)) > 1e-7 {
		return 0, fmt.Errorf("invalid frame rate %q", s)
	}
	nominal, fractional, err := frameRateParts(v)
	if err != nil {
		return 0, err
	}
	if fractional {
		return float64(nominal*1000) / 1001, nil
	}
	return float64(nominal), nil
}

func outputAudioOptions(outputs []OutputRequest) []*AudioOptions {
	options := make([]*AudioOptions, len(outputs))
	for i := range outputs {
		options[i] = &outputs[i].Options
	}
	return options
}
func ext(path string) string { return strings.ToLower(strings.TrimPrefix(filepath.Ext(path), ".")) }
