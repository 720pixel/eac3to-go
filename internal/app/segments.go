package app

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"eac3to/internal/bluray"
)

type segmentCell struct {
	Clip string `json:"clip"`
	Item int    `json:"item"`
	In   uint64 `json:"in_ticks"`
	Out  uint64 `json:"out_ticks"`
	STC  uint8  `json:"stc_id"`
}

type segmentRow struct {
	Start     uint64        `json:"start_ticks"`
	End       uint64        `json:"end_ticks"`
	Cells     []segmentCell `json:"playlists"`
	Different bool          `json:"different"`
}

func segmentRows(playlists []*bluray.Playlist) []segmentRow {
	bounds := map[uint64]bool{0: true}
	for _, p := range playlists {
		var at uint64
		for _, item := range p.Items {
			at += uint64(item.OutTime - item.InTime)
			bounds[at] = true
		}
	}
	points := make([]uint64, 0, len(bounds))
	for point := range bounds {
		points = append(points, point)
	}
	sort.Slice(points, func(i, j int) bool { return points[i] < points[j] })
	indices := make([]int, len(playlists))
	starts := make([]uint64, len(playlists))
	var rows []segmentRow
	for i := 0; i+1 < len(points); i++ {
		start, end := points[i], points[i+1]
		row := segmentRow{Start: start, End: end, Cells: make([]segmentCell, len(playlists))}
		for j, p := range playlists {
			for indices[j] < len(p.Items) && starts[j]+uint64(p.Items[indices[j]].OutTime-p.Items[indices[j]].InTime) <= start {
				starts[j] += uint64(p.Items[indices[j]].OutTime - p.Items[indices[j]].InTime)
				indices[j]++
			}
			if indices[j] < len(p.Items) {
				item := p.Items[indices[j]]
				in := uint64(item.InTime) + start - starts[j]
				row.Cells[j] = segmentCell{Clip: item.ClipID, Item: indices[j] + 1, In: in, Out: in + end - start, STC: item.STCID}
			}
			if j > 0 {
				a, b := row.Cells[0], row.Cells[j]
				if a.Clip != b.Clip || a.In != b.In || a.Out != b.Out || a.STC != b.STC {
					row.Different = true
				}
			}
		}
		rows = append(rows, row)
	}
	return rows
}

func printSegments(w io.Writer, playlists []*bluray.Playlist, o Options) error {
	rows := segmentRows(playlists)
	names := make([]string, len(playlists))
	for i, p := range playlists {
		names[i] = p.Name
	}
	if o.JSON {
		return json.NewEncoder(w).Encode(struct {
			Timescale int          `json:"timescale"`
			Names     []string     `json:"playlists"`
			Rows      []segmentRow `json:"segments"`
		}{45000, names, rows})
	}
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintf(tw, "FROM\tTO\t%s\n", strings.Join(names, "\t"))
	differences := 0
	for _, row := range rows {
		if row.Different {
			differences++
		}
		if len(playlists) > 1 && !row.Different && !o.Segments {
			continue
		}
		fmt.Fprintf(tw, "%s\t%s", timestamp(float64(row.Start)/45000), timestamp(float64(row.End)/45000))
		for _, cell := range row.Cells {
			if cell.Clip == "" {
				fmt.Fprint(tw, "\t—")
				continue
			}
			fmt.Fprintf(tw, "\t%s.m2ts [%d] @ %s", cell.Clip, cell.Item, timestamp(float64(cell.In)/45000))
		}
		fmt.Fprintln(tw)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if len(playlists) > 1 {
		fmt.Fprintf(w, "\n%d differing windows. Use --segments to include matching windows.\n", differences)
		if differences == 0 {
			fmt.Fprintln(w, "The picture timeline matches; track lists, angles, and chapter metadata can still differ.")
		}
	}
	return nil
}
