// Package transport extracts complete PES packets from 188-byte MPEG transport
// streams and 192-byte M2TS streams. It preserves elementary payloads and raw
// 33-bit timestamps. Codec framing and timestamp remapping belong to callers.
//
// Packet syntax references: ITU-T H.222.0 and FFmpeg's transport parser:
// https://www.itu.int/rec/T-REC-H.222.0
// https://github.com/FFmpeg/FFmpeg/blob/master/libavformat/mpegts.c
package transport

import (
	"errors"
	"fmt"
)

const NoTimestamp int64 = -1

var ErrFinished = errors.New("transport demuxer is finished")

type Config struct {
	Handlers   map[uint16]func(PES) error
	OnBoundary func(Boundary) error
	OnAudit    func(Audit) error
	Strict     bool
	// StrictPIDs applies playlist windows to selected streams independently.
	StrictPIDs map[uint16]bool
	// MaxPESBytes bounds each selected PID's assembly buffer. Zero means 16 MiB.
	MaxPESBytes int
	// AllowRetransmissions permits skipping an identical TS packet repeated with
	// the same continuity counter. Each skip is counted and audited. No codec
	// frame or ordinary repeated PES is removed, regardless of this setting.
	AllowRetransmissions bool
}

// Segment describes one physical source file. PacketSize is 188, 192, or zero
// for detection. Playlist windows and boundary deltas use the 90 kHz clock.
// The caller supplies an observed boundary delta; it is not derived from the
// playlist duration. A zero delta carries unfinished PES and continuity state
// across files. A nonzero delta flushes complete PES and resets continuity.
// Reset requests the same flush for an explicitly independent stream even when
// a boundary delta is zero or has not been established.
type Segment struct {
	Index            int
	PacketSize       int
	HasWindow        bool
	In90k, Out90k    int64
	BoundaryDelta90k int64
	Reset            bool
}

// PES refers to bytes valid only during its handler call. Copy Raw or Payload
// when retaining them. Payload includes codec-private headers such as Blu-ray
// LPCM's four-byte prefix; no codec-specific bytes are removed here.
// Segment and ByteOffset identify the first transport packet of this PES.
// PTS and DTS are unmodified 33-bit values, or NoTimestamp when absent.
type PES struct {
	PID               uint16
	StreamID          uint8
	StreamIDExtension int
	Segment           int
	ByteOffset        int64
	PTS, DTS          int64
	ArrivalTimestamp  int64
	Discontinuity     bool
	Raw, Payload      []byte
}

// Boundary occurs twice per file transition, Before=true then Before=false.
// For a nonzero delta, buffered complete PES are delivered before these events.
// A codec framer can use the events without conflating frame overlap with an
// identical transport-packet retransmission.
type Boundary struct {
	PreviousSegment int
	CurrentSegment  int
	Delta90k        int64
	Before          bool
	Reset           bool
}

const (
	AuditOutsideWindow  = "outside_playlist_window"
	AuditRetransmission = "transport_retransmission"
	AuditDiscontinuity  = "transport_discontinuity"
)

type Audit struct {
	Kind       string
	PID        uint16
	Segment    int
	ByteOffset int64
	PTS        int64
	Bytes      int
	Dropped    bool
}

type PIDStats struct {
	TransportPackets int64
	PESPackets       int64
	EmittedPES       int64
	PayloadBytes     int64
	DroppedPES       int64
	DroppedBytes     int64
	OutsideWindowPES int64
	Retransmissions  int64
	Discontinuities  int64
}

type Stats struct {
	Segments         int64
	TransportPackets int64
	BytesRead        int64
	ByPID            map[uint16]PIDStats
}

// ParseError locates invalid data without attempting a lossy resynchronization.
// PID is -1 when the packet header is not available.
type ParseError struct {
	Segment    int
	ByteOffset int64
	PID        int
	Problem    string
}

func (e *ParseError) Error() string {
	if e.PID < 0 {
		return fmt.Sprintf("segment %d at byte %d: %s", e.Segment, e.ByteOffset, e.Problem)
	}
	return fmt.Sprintf("segment %d at byte %d, PID 0x%04x: %s", e.Segment, e.ByteOffset, e.PID, e.Problem)
}
