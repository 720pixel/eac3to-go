package app

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
)

// A requested output owns this state across value copies. Decoder caches are
// separate Outputs and must not inherit a compressed-copy residual.
type outputMuxTiming struct {
	delay          atomic.Int64
	known          atomic.Bool
	acceptResidual bool
}

type ExtractionReport struct {
	Version int                      `json:"version"`
	Outputs []ExtractionReportOutput `json:"outputs"`
}

type ExtractionReportOutput struct {
	Track           int    `json:"track"`
	Path            string `json:"path"`
	Kind            string `json:"kind"`
	Codec           string `json:"codec"`
	MuxDelayMS      int64  `json:"mux_delay_ms"`
	DelayInFilename bool   `json:"delay_in_filename,omitempty"`
}

func setOutputMuxDelay(out Output, delay int64) {
	if out.muxTiming != nil && !out.DecodeCache {
		out.muxTiming.delay.Store(delay)
		out.muxTiming.known.Store(true)
	}
}

func setOutputResidual(out Output, delay int64) {
	if out.muxTiming != nil && out.muxTiming.acceptResidual {
		setOutputMuxDelay(out, delay)
	}
}

func extractionReportPath(s *Source, outputs []Output, o Options) (string, error) {
	if o.Report == "" {
		return "", nil
	}
	if strings.ContainsRune(o.Report, 0) {
		return "", fmt.Errorf("report path contains NUL")
	}
	p, err := filepath.Abs(o.Report)
	if err != nil {
		return "", err
	}
	protected := *s
	protected.Paths = append(append([]string(nil), s.Paths...), o.Inputs...)
	all := append(append([]Output(nil), outputs...), Output{Path: p})
	if err := validateOutputPaths(&protected, all, o); err != nil {
		return "", fmt.Errorf("report destination: %w", err)
	}
	reportKey, err := canonicalOutput(p)
	if err != nil {
		return "", err
	}
	for _, out := range outputs {
		if out.Request.Path == "" {
			continue
		}
		requested := out.Request.Path
		if o.Dest != "" && !filepath.IsAbs(requested) {
			requested = filepath.Join(o.Dest, requested)
		}
		key, err := canonicalOutput(requested)
		if err != nil {
			return "", err
		}
		if key == reportKey {
			return "", fmt.Errorf("report would replace a requested output path")
		}
	}
	for _, input := range o.Inputs {
		fi, err := os.Stat(input)
		if err != nil || !fi.IsDir() {
			continue
		}
		root, err := filepath.EvalSymlinks(input)
		if err != nil {
			return "", err
		}
		rel, err := filepath.Rel(root, reportKey)
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("report must be outside the source disc directory")
		}
	}
	return p, nil
}

func prepareOutputReportTiming(s *Source, outputs []Output) {
	for i := range outputs {
		out := &outputs[i]
		out.muxTiming = new(outputMuxTiming)
		if offset, ok := matroskaAudioOffset(s, out.Track.Stream); ok && out.Request.Options.DelayMS == 0 && !(out.Codec == "truehd" && out.TrueHDPrepared) {
			setOutputMuxDelay(*out, offset)
			continue
		}
		switch {
		case out.Track.Kind == "chapters", !out.Copy, out.NativePCM, strings.HasPrefix(out.Codec, "pcm_"):
			// PCM processing applies source+manual delay in sample space.
			setOutputMuxDelay(*out, 0)
		case out.Codec == "truehd" && out.Format == "truehd" && out.TrueHDPrepared:
			setOutputMuxDelay(*out, out.TrueHDDelayMS)
		case out.Track.Stream != nil && out.Track.Stream.Type == "video":
			// Elementary video starts at its first retained coded picture.
			setOutputMuxDelay(*out, 0)
		case s.Playlist != nil && out.Codec == "hdmv_pgs_subtitle" && out.Format == "sup":
			// Native playlist PGS is already rebased against the video origin.
			setOutputMuxDelay(*out, 0)
		case standaloneSubtitle(s, *out):
			// Cue timestamps survive a direct standalone subtitle copy.
			setOutputMuxDelay(*out, 0)
		case s.Playlist == nil && out.Track.Stream != nil && len(s.Paths) == 1 && s.Probe.Format.Name == out.Track.Stream.Codec && out.Request.Options.DelayMS == 0:
			// Plain elementary audio has no source container offset.
			setOutputMuxDelay(*out, 0)
		}
		if out.Copy && out.Track.Stream != nil && nativeCopySupported(*out) && (out.Codec == "ac3" || out.Codec == "eac3" || out.Codec == "dts") {
			out.muxTiming.acceptResidual = s.Playlist != nil || len(s.Paths) == 1 && s.Probe.Format.Name == out.Track.Stream.Codec || nativeTransportSource(s) && (out.TrueHDCompanion || sharedTrueHDPID(s, out.Track.Stream))
		}
	}
}

func makeExtractionReport(outputs []Output) (ExtractionReport, error) {
	r := ExtractionReport{Version: 1, Outputs: make([]ExtractionReportOutput, 0, len(outputs))}
	for _, out := range outputs {
		if out.muxTiming == nil || !out.muxTiming.known.Load() {
			return r, fmt.Errorf("track %d mux timing is not verified for this extraction path", out.Track.Number)
		}
		r.Outputs = append(r.Outputs, ExtractionReportOutput{
			Track: out.Track.Number, Path: out.Path, Kind: out.Track.Kind, Codec: out.Codec,
			MuxDelayMS: out.muxTiming.delay.Load(), DelayInFilename: out.Copy && out.Codec == "truehd" && out.Format == "truehd" && out.TrueHDPrepared,
		})
	}
	return r, nil
}

func stageExtractionReport(path string, r ExtractionReport) (string, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return "", err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".eac3to-report-")
	if err != nil {
		return "", err
	}
	name := f.Name()
	ok := false
	defer func() {
		f.Close()
		if !ok {
			os.Remove(name)
		}
	}()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	if err := enc.Encode(r); err != nil {
		return "", err
	}
	if err := f.Sync(); err != nil {
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	ok = true
	return name, nil
}

func publishExtractionReport(stage, path string, force bool) error {
	if force {
		return os.Rename(stage, path)
	}
	if err := os.Link(stage, path); err != nil {
		return fmt.Errorf("publish report: %w", err)
	}
	// Publication has succeeded; temporary-name cleanup must not turn it
	// into a reported failure after the manifest is already visible.
	return nil
}
