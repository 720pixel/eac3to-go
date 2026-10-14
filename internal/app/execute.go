package app

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"eac3to/internal/bitstream"
)

func execute(ctx context.Context, t Tools, s *Source, outputs []Output, o Options, stdout, stderr io.Writer) error {
	stderr = &lockedWriter{w: stderr}
	var err error
	outputs, err = prepareTrueHDOutputs(ctx, s, outputs, o)
	if err != nil {
		return err
	}
	reportPath, err := extractionReportPath(s, outputs, o)
	if err != nil {
		return err
	}
	if reportPath != "" {
		prepareOutputReportTiming(s, outputs)
	}
	var audioStreams []*Stream
	for _, out := range outputs {
		audioStreams = append(audioStreams, out.Track.Stream)
	}
	if err := prepareFLACChecks(ctx, t, s, audioStreams); err != nil {
		return err
	}
	var dirs []string
	defer func() {
		for _, d := range dirs {
			os.RemoveAll(d)
		}
	}()
	for i := range outputs {
		out := &outputs[i]
		parent := filepath.Dir(out.Path)
		if e := os.MkdirAll(parent, 0755); e != nil {
			return e
		}
		d, e := os.MkdirTemp(parent, ".eac3to-")
		if e != nil {
			return e
		}
		dirs = append(dirs, d)
		out.Stage = filepath.Join(d, filepath.Base(out.Path))
	}
	for _, out := range outputs {
		if out.Track.Kind == "chapters" {
			if e := writeChapters(out, s.Probe.Chapters); e != nil {
				return e
			}
		}
	}
	jobs, caches, e := prepareAudioJobs(s, outputs)
	if e != nil {
		return e
	}
	if e := copyStreams(ctx, t, s, append(append([]Output(nil), outputs...), caches...), o, stderr); e != nil {
		return e
	}
	if e := processAudioJobs(ctx, t, jobs, o, stderr); e != nil {
		return e
	}
	for _, out := range outputs {
		if e := ensureRegularOutput(out.Stage); e != nil {
			return e
		}
		if out.Forced && !out.Copy {
			if e := rewrite(out.Stage, func(r io.Reader, w io.Writer) error {
				stats, e := bitstream.FilterForcedPGS(r, w)
				if e == nil && !o.Quiet {
					fmt.Fprintf(stderr, "PGS: %d forced objects in %d presentations.\n", stats.ForcedObjects, stats.Presentations)
				}
				return e
			}); e != nil {
				return e
			}
		}
		if out.RemoveDialnorm && !out.Copy {
			if e := rewrite(out.Stage, func(r io.Reader, w io.Writer) error {
				if out.Codec == "dts" {
					stats, e := bitstream.RemoveDTSDialnorm(r, w)
					if e == nil && !o.Quiet {
						fmt.Fprintf(stderr, "DTS dialnorm: changed %d of %d headers.\n", stats.ChangedFrames, stats.Frames)
					}
					return e
				}
				stats, e := bitstream.RemoveDialnorm(r, w)
				if e == nil && !o.Quiet {
					fmt.Fprintf(stderr, "Dialnorm: changed %d of %d frames.\n", stats.ChangedFrames, stats.Frames)
				}
				return e
			}); e != nil {
				return e
			}
		}
	}
	if e := ctx.Err(); e != nil {
		return e
	}
	var reportStage string
	if reportPath != "" {
		report, err := makeExtractionReport(outputs)
		if err != nil {
			return err
		}
		reportStage, err = stageExtractionReport(reportPath, report)
		if err != nil {
			return err
		}
		defer os.Remove(reportStage)
	}
	for _, out := range outputs {
		f, e := os.OpenFile(out.Stage, os.O_RDWR, 0)
		if e != nil {
			return e
		}
		e = f.Sync()
		closeErr := f.Close()
		if e != nil {
			return e
		}
		if closeErr != nil {
			return closeErr
		}
		if o.Force {
			if e := os.Rename(out.Stage, out.Path); e != nil {
				return e
			}
		} else {
			if e := os.Link(out.Stage, out.Path); e != nil {
				return fmt.Errorf("publish %s: %w", out.Path, e)
			}
			if e := os.Remove(out.Stage); e != nil {
				return e
			}
		}
		if !o.Quiet {
			fmt.Fprintln(stderr, "Created", out.Path)
		}
	}
	if reportStage != "" {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := publishExtractionReport(reportStage, reportPath, o.Force); err != nil {
			return err
		}
	}
	return nil
}

func rewrite(path string, fn func(io.Reader, io.Writer) error) error {
	in, e := os.Open(path)
	if e != nil {
		return e
	}
	defer in.Close()
	out, e := os.CreateTemp(filepath.Dir(path), "edit-")
	if e != nil {
		return e
	}
	name := out.Name()
	defer os.Remove(name)
	e = fn(in, out)
	ce := out.Close()
	if e != nil {
		return e
	}
	if ce != nil {
		return ce
	}
	if e := in.Close(); e != nil {
		return e
	}
	return os.Rename(name, path)
}

func writeChapters(out Output, chapters []Chapter) error {
	f, e := os.Create(out.Stage)
	if e != nil {
		return e
	}
	defer f.Close()
	switch out.Format {
	case "json":
		return json.NewEncoder(f).Encode(chapters)
	case "xml":
		type display struct {
			Title    string `xml:"ChapterString"`
			Language string `xml:"ChapterLanguage"`
		}
		type atom struct {
			Start   string  `xml:"ChapterTimeStart"`
			Display display `xml:"ChapterDisplay"`
		}
		type edition struct {
			Atoms []atom `xml:"ChapterAtom"`
		}
		type doc struct {
			XMLName xml.Name `xml:"Chapters"`
			Edition edition  `xml:"EditionEntry"`
		}
		d := doc{}
		for i, c := range chapters {
			start, e := strconv.ParseFloat(c.Start, 64)
			if e != nil {
				return e
			}
			title := c.Tags["title"]
			if title == "" {
				title = fmt.Sprintf("Chapter %02d", i+1)
			}
			d.Edition.Atoms = append(d.Edition.Atoms, atom{timestamp(start), display{title, "und"}})
		}
		if _, e := io.WriteString(f, xml.Header); e != nil {
			return e
		}
		enc := xml.NewEncoder(f)
		enc.Indent("", "  ")
		return enc.Encode(d)
	default:
		for i, c := range chapters {
			start, e := strconv.ParseFloat(c.Start, 64)
			if e != nil {
				return e
			}
			title := c.Tags["title"]
			if title == "" {
				title = fmt.Sprintf("Chapter %02d", i+1)
			}
			title = strings.NewReplacer("\r", " ", "\n", " ").Replace(title)
			if _, e := fmt.Fprintf(f, "CHAPTER%02d=%s\nCHAPTER%02dNAME=%s\n", i+1, timestamp(start), i+1, title); e != nil {
				return e
			}
		}
		return nil
	}
}
