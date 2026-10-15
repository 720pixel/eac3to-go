package transport

import (
	"bytes"
	"context"
	"testing"
)

func FuzzTransport(f *testing.F) {
	for _, size := range []int{188, 192} {
		packet, _ := packPES(0x100, makePES(0xbd, 90000, NoTimestamp, []byte{1, 2, 3, 4}, false), 0, size, 184)
		f.Add(packet, size)
	}
	f.Add([]byte{}, 0)
	f.Fuzz(func(t *testing.T, data []byte, size int) {
		if len(data) > 64<<10 {
			t.Skip()
		}
		if size != 188 && size != 192 {
			size = 0
		}
		var payloadBytes int64
		d, err := New(Config{MaxPESBytes: 64 << 10, AllowRetransmissions: true, Handlers: map[uint16]func(PES) error{0x100: func(p PES) error { payloadBytes += int64(len(p.Payload)); return nil }}})
		if err != nil {
			t.Fatal(err)
		}
		if err = d.ReadSegment(context.Background(), bytes.NewReader(data), Segment{Index: 0, PacketSize: size}); err == nil {
			_ = d.Finish()
		}
		if payloadBytes > int64(len(data)) {
			t.Fatal("emitted more payload than source data")
		}
	})
}

func FuzzTimestampScan(f *testing.F) {
	for _, size := range []int{188, 192} {
		packet, _ := packPES(0x100, makePES(0xe0, (1<<33)-1, NoTimestamp, []byte{1, 2, 3}, false), 0, size, 5)
		f.Add(packet, size)
	}
	f.Fuzz(func(t *testing.T, data []byte, size int) {
		if len(data) > 64<<10 {
			t.Skip()
		}
		if size != 192 {
			size = 188
		}
		extent := ScanExtent{Length: int64(len(data) / size * size)}
		ranges := make(map[uint16]*timestampRange)
		var result ScanResult
		_ = scanExtent(context.Background(), bytes.NewReader(data), extent, size, nil, ranges, &result)
		if result.BytesScanned > int64(len(data)) {
			t.Fatal("scan exceeded input extent")
		}
		for _, r := range ranges {
			if r.count < 1 || r.min > r.max || r.minOffset < 0 || r.maxOffset < 0 || r.minOffset >= extent.Length || r.maxOffset >= extent.Length {
				t.Fatal("invalid observed timestamp range")
			}
		}
	})
}

func FuzzPESHeader(f *testing.F) {
	f.Add(makePES(0xbd, (1<<33)-1, (1<<33)-2, []byte{1, 2, 3}, false))
	f.Add([]byte{0, 0, 1, 0xbe, 0, 1, 0xff})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 64<<10 {
			t.Skip()
		}
		pes, err := parsePES(data)
		if err == nil && (len(pes.Payload) > len(data) || pes.PTS >= 1<<33 || pes.DTS >= 1<<33 || pes.PTS < NoTimestamp || pes.DTS < NoTimestamp) {
			t.Fatal("invalid successful PES parse")
		}
	})
}
