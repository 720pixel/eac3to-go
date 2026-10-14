package app

import (
	"testing"

	"eac3to/internal/bluray"
)

func TestSegmentsAlignTimeInsteadOfItemNumbers(t *testing.T) {
	a := &bluray.Playlist{Items: []bluray.PlayItem{
		{ClipID: "00001", InTime: 45000, OutTime: 225000},
	}}
	b := &bluray.Playlist{Items: []bluray.PlayItem{
		{ClipID: "00001", InTime: 45000, OutTime: 90000},
		{ClipID: "00001", InTime: 90000, OutTime: 180000},
		{ClipID: "00002", InTime: 180000, OutTime: 225000},
	}}
	rows := segmentRows([]*bluray.Playlist{a, b})
	if len(rows) != 3 {
		t.Fatalf("got %d rows", len(rows))
	}
	if rows[0].Different || rows[1].Different || !rows[2].Different {
		t.Fatalf("unexpected differences: %+v", rows)
	}
	if rows[1].Cells[0].In != 90000 || rows[1].Cells[1].In != 90000 {
		t.Fatal(rows[1])
	}
}

func TestSegmentsExposeMissingTailAndSTCChanges(t *testing.T) {
	a := &bluray.Playlist{Items: []bluray.PlayItem{{ClipID: "00001", OutTime: 90000, STCID: 1}}}
	b := &bluray.Playlist{Items: []bluray.PlayItem{{ClipID: "00001", OutTime: 45000, STCID: 2}}}
	rows := segmentRows([]*bluray.Playlist{a, b})
	if len(rows) != 2 || !rows[0].Different || !rows[1].Different || rows[1].Cells[1].Clip != "" {
		t.Fatalf("%+v", rows)
	}
}
