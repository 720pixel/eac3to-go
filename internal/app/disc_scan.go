package app

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"sync"
	"text/tabwriter"

	"eac3to/internal/bluray"
	"eac3to/internal/transport"
)

type clipScan struct {
	Item     int                  `json:"item"`
	Clip     string               `json:"clip"`
	Path     string               `json:"-"`
	In90k    int64                `json:"in_90k"`
	Out90k   int64                `json:"out_90k"`
	Boundary int64                `json:"boundary_delta_90k"`
	Scan     transport.ScanResult `json:"scan"`
}

func scanPlaylist(ctx context.Context, p *bluray.Playlist, o Options) ([]clipScan, error) {
	return scanPlaylistPIDs(ctx, p, o, nil)
}

func scanPlaylistPIDs(ctx context.Context, p *bluray.Playlist, o Options, selected map[uint16]bool) ([]clipScan, error) {
	paths, err := bluray.ResolveClips(p)
	if err != nil {
		return nil, err
	}
	scans := make([]clipScan, len(paths))
	err = parallelItems(ctx, o, len(paths), func(ctx context.Context, i int) error {
		item := p.Items[i]
		video := map[uint16]bool{}
		for _, stream := range item.Streams {
			if stream.EntryType == 1 && stream.Kind == "video" {
				video[stream.PID] = true
			}
		}
		scan, err := transport.ScanFile(ctx, paths[i], transport.ScanConfig{VideoPIDs: video, PIDs: selected})
		if err != nil {
			return fmt.Errorf("clip %s: %w", item.ClipID, err)
		}
		if scan.TrailingBytes != 0 {
			return fmt.Errorf("clip %s has %d bytes after its last complete transport packet", item.ClipID, scan.TrailingBytes)
		}
		scans[i] = clipScan{Item: i + 1, Clip: item.ClipID, Path: paths[i], In90k: int64(item.InTime) * 2, Out90k: int64(item.OutTime) * 2, Scan: scan}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for i := 1; i < len(scans); i++ {
		previous, current := scans[i-1].Scan, scans[i].Scan
		if !previous.HasPTS || !current.HasPTS {
			return nil, fmt.Errorf("cannot establish timestamps at clips %s / %s", scans[i-1].Clip, scans[i].Clip)
		}
		scans[i].Boundary, err = transport.TimestampDelta(current.FirstPTS, previous.LastPTS)
		if err != nil {
			return nil, fmt.Errorf("clips %s / %s: %w", scans[i-1].Clip, scans[i].Clip, err)
		}
	}
	return scans, nil
}

func parallelItems(ctx context.Context, o Options, count int, fn func(context.Context, int) error) error {
	workers, _ := concurrency(o, count)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	queue := make(chan int)
	var wg sync.WaitGroup
	var once sync.Once
	var failure error
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range queue {
				if ctx.Err() != nil {
					return
				}
				if err := fn(ctx, i); err != nil {
					once.Do(func() { failure = err; cancel() })
					return
				}
			}
		}()
	}
loop:
	for i := range count {
		select {
		case queue <- i:
		case <-ctx.Done():
			break loop
		}
	}
	close(queue)
	wg.Wait()
	if failure != nil {
		return failure
	}
	return ctx.Err()
}

type pidAudit struct {
	PID        uint16 `json:"pid"`
	Packets    int64  `json:"pes_packets"`
	Outside    int64  `json:"outside_window_packets"`
	Bytes      int64  `json:"outside_window_bytes"`
	MissingPTS int64  `json:"missing_timestamp_packets"`
}

type clipAudit struct {
	clipScan
	PIDs []pidAudit `json:"pids"`
}

func auditPlaylist(ctx context.Context, p *bluray.Playlist, o Options, w, log io.Writer) error {
	scans, err := scanPlaylist(ctx, p, o)
	if err != nil {
		return err
	}
	if !o.Quiet {
		fmt.Fprintf(log, "Auditing all transport packets in %d retained clips.\n", len(scans))
	}
	results := make([]clipAudit, len(scans))
	err = parallelItems(ctx, o, len(scans), func(ctx context.Context, i int) error {
		clip := scans[i]
		counts := map[uint16]*pidAudit{}
		handlers := map[uint16]func(transport.PES) error{}
		for pid := range p.Items[i].ListedPIDs() {
			counts[pid] = &pidAudit{PID: pid}
			handlers[pid] = func(pes transport.PES) error {
				c := counts[pes.PID]
				c.Packets++
				if pes.PTS == transport.NoTimestamp {
					c.MissingPTS++
				}
				return nil
			}
		}
		if len(handlers) == 0 {
			return fmt.Errorf("clip %s has no main-clip PIDs in its playlist", clip.Clip)
		}
		demux, err := transport.New(transport.Config{
			Handlers: handlers,
			OnAudit: func(a transport.Audit) error {
				if a.Kind == transport.AuditOutsideWindow {
					counts[a.PID].Outside++
					counts[a.PID].Bytes += int64(a.Bytes)
				}
				return nil
			},
		})
		if err != nil {
			return err
		}
		f, err := os.Open(clip.Path)
		if err != nil {
			return err
		}
		defer f.Close()
		segment := transport.Segment{Index: i, PacketSize: clip.Scan.PacketSize, HasWindow: true, In90k: clip.In90k, Out90k: clip.Out90k}
		if err = demux.ReadSegment(ctx, f, segment); err == nil {
			err = demux.Finish()
		}
		if err != nil {
			return fmt.Errorf("clip %s: %w", clip.Clip, err)
		}
		results[i] = clipAudit{clipScan: clip}
		for _, count := range counts {
			results[i].PIDs = append(results[i].PIDs, *count)
		}
		sort.Slice(results[i].PIDs, func(a, b int) bool { return results[i].PIDs[a].PID < results[i].PIDs[b].PID })
		return nil
	})
	if err != nil {
		return err
	}
	if o.JSON {
		return json.NewEncoder(w).Encode(struct {
			Playlist string      `json:"playlist"`
			Clips    []clipAudit `json:"clips"`
		}{p.Name, results})
	}
	fmt.Fprintf(w, "%s: complete PES audit of listed main-clip streams.\n", p.Name)
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ITEM\tCLIP\tIN\tOUT\tOUTSIDE PES\tOUTSIDE BYTES")
	var packets, bytes int64
	for _, result := range results {
		var outside, size int64
		for _, pid := range result.PIDs {
			outside += pid.Outside
			size += pid.Bytes
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%d\t%d\n", result.Item, result.Clip, timestamp(float64(result.In90k)/90000), timestamp(float64(result.Out90k)/90000), outside, size)
		packets += outside
		bytes += size
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%d PES packets (%d payload bytes) lie outside playlist windows. No data was changed.\n", packets, bytes)
	return err
}
