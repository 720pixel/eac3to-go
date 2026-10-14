package app

import (
	"fmt"
	"io"
	"time"
)

type extractionProgress struct {
	log            io.Writer
	total, read    int64
	clip, clips    int
	start, updated time.Time
}

func newExtractionProgress(scans []clipScan, log io.Writer, quiet bool) *extractionProgress {
	if quiet {
		log = io.Discard
	}
	now := time.Now()
	p := &extractionProgress{log: log, clips: len(scans), start: now, updated: now}
	for _, clip := range scans {
		p.total += clip.Scan.FileSize
	}
	return p
}

func (p *extractionProgress) reader(r io.Reader) io.Reader {
	if p.log == io.Discard {
		return r
	}
	return extractionReader{Reader: r, progress: p}
}

type extractionReader struct {
	io.Reader
	progress *extractionProgress
}

func (r extractionReader) Read(data []byte) (int, error) {
	n, err := r.Reader.Read(data)
	p := r.progress
	p.read += int64(n)
	now := time.Now()
	if now.Sub(p.updated) >= 5*time.Second {
		p.report(now)
	}
	return n, err
}

func (p *extractionProgress) report(now time.Time) {
	if p.total <= 0 || p.log == io.Discard {
		return
	}
	elapsed := now.Sub(p.start).Seconds()
	if elapsed <= 0 {
		return
	}
	fmt.Fprintf(p.log, "Extracting: %5.1f%% | clip %d/%d | %.1f MiB/s | %s elapsed\n", 100*float64(p.read)/float64(p.total), p.clip, p.clips, float64(p.read)/(1<<20)/elapsed, now.Sub(p.start).Round(time.Second))
	p.updated = now
}

func (p *extractionProgress) finish() {
	if time.Since(p.start) >= 5*time.Second {
		p.report(time.Now())
	}
}
