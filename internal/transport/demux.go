package transport

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"sort"
)

type pidState struct {
	pid                  uint16
	handler              func(PES) error
	stats                PIDStats
	lastCC               int
	lastPayload          [188]byte
	havePayload          bool
	buffer               []byte
	active               bool
	length               int // -1 until the first six bytes arrive; zero is unbounded.
	segment              Segment
	offset               int64
	startPacket          int64
	arrival              int64
	discontinuity        bool
	pendingDiscontinuity bool
	outside              bool
}

// Demuxer is a single-stream state machine. Its callbacks must not reenter it,
// and it must not be used concurrently. Any read or callback failure stops it;
// callers should discard staged outputs rather than retry a partially read file.
type Demuxer struct {
	cfg         Config
	pids        [8192]*pidState
	selected    []*pidState
	current     Segment
	haveSegment bool
	stats       Stats
	failure     error
	finished    bool
}

func New(cfg Config) (*Demuxer, error) {
	if len(cfg.Handlers) == 0 {
		return nil, fmt.Errorf("at least one PES PID handler is required")
	}
	if cfg.MaxPESBytes == 0 {
		cfg.MaxPESBytes = 16 << 20
	}
	if cfg.MaxPESBytes < 6 {
		return nil, fmt.Errorf("PES buffer limit must be at least six bytes")
	}
	d := &Demuxer{cfg: cfg}
	for pid, handler := range cfg.Handlers {
		if pid >= 0x1fff || handler == nil {
			return nil, fmt.Errorf("invalid PES handler for PID 0x%04x", pid)
		}
		state := &pidState{pid: pid, handler: handler, lastCC: -1}
		d.pids[pid] = state
		d.selected = append(d.selected, state)
	}
	// Keep final flush order stable, including streams that have no data yet.
	sort.Slice(d.selected, func(i, j int) bool { return d.selected[i].pid < d.selected[j].pid })
	d.cfg.Handlers = nil
	return d, nil
}

func (d *Demuxer) Stats() Stats {
	result := d.stats
	result.ByPID = make(map[uint16]PIDStats, len(d.selected))
	for _, state := range d.selected {
		result.ByPID[state.pid] = state.stats
	}
	return result
}

// ReadSegment consumes whole transport packets, retaining any unfinished PES
// for the next file or Finish. It does not close source. Cancellation is checked
// between reads; interruption of a blocking reader remains the caller's job.
func (d *Demuxer) ReadSegment(ctx context.Context, source io.Reader, segment Segment) error {
	if d.failure != nil {
		return d.failure
	}
	if d.finished {
		return ErrFinished
	}
	if err := ctx.Err(); err != nil {
		return d.stop(err)
	}
	if segment.Index < 0 || d.haveSegment && segment.Index <= d.current.Index {
		return d.stop(fmt.Errorf("segment indices must be nonnegative and strictly increasing"))
	}
	if segment.PacketSize != 0 && segment.PacketSize != 188 && segment.PacketSize != 192 {
		return d.stop(fmt.Errorf("transport packet size must be 188, 192 or zero for detection"))
	}
	if segment.HasWindow && (segment.In90k < 0 || segment.Out90k <= segment.In90k || segment.Out90k > 1<<33) {
		return d.stop(fmt.Errorf("playlist window must be a nonempty half-open 33-bit timestamp interval"))
	}
	reader := bufio.NewReaderSize(source, 256<<10)
	if segment.PacketSize == 0 {
		packetSize, err := detectPacketSize(reader)
		if err != nil {
			return d.stop(&ParseError{Segment: segment.Index, PID: -1, Problem: err.Error()})
		}
		segment.PacketSize = packetSize
	}
	if d.haveSegment {
		reset := segment.BoundaryDelta90k != 0 || segment.Reset
		if reset {
			if err := d.flushAll(); err != nil {
				return d.stop(err)
			}
			for _, state := range d.selected {
				state.lastCC, state.havePayload = -1, false
				if segment.Reset {
					state.outside, state.pendingDiscontinuity = false, false
				}
			}
		}
		if d.cfg.OnBoundary != nil {
			event := Boundary{PreviousSegment: d.current.Index, CurrentSegment: segment.Index, Delta90k: segment.BoundaryDelta90k, Before: true, Reset: reset}
			if err := d.cfg.OnBoundary(event); err != nil {
				return d.stop(fmt.Errorf("before segment %d boundary: %w", segment.Index, err))
			}
			event.Before = false
			if err := d.cfg.OnBoundary(event); err != nil {
				return d.stop(fmt.Errorf("after segment %d boundary: %w", segment.Index, err))
			}
		}
	}
	d.current, d.haveSegment = segment, true
	d.stats.Segments++
	var packet [192]byte
	var offset int64
	for {
		if d.stats.TransportPackets&255 == 0 {
			if err := ctx.Err(); err != nil {
				return d.stop(err)
			}
		}
		n, err := io.ReadFull(reader, packet[:segment.PacketSize])
		d.stats.BytesRead += int64(n)
		if err == io.EOF && n == 0 {
			if offset == 0 {
				return d.stop(&ParseError{Segment: segment.Index, PID: -1, Problem: "empty transport segment"})
			}
			return nil
		}
		if err != nil {
			return d.stop(&ParseError{Segment: segment.Index, ByteOffset: offset, PID: -1, Problem: fmt.Sprintf("incomplete transport packet: %v", err)})
		}
		d.stats.TransportPackets++
		data := packet[:188]
		arrival := NoTimestamp
		if segment.PacketSize == 192 {
			arrival = int64(binary.BigEndian.Uint32(packet[:4]) & 0x3fffffff)
			data = packet[4:]
		}
		if err := d.packet(data, segment, offset, arrival); err != nil {
			return d.stop(err)
		}
		offset += int64(segment.PacketSize)
	}
}

func (d *Demuxer) packet(packet []byte, segment Segment, offset, arrival int64) error {
	bad := func(pid int, message string) error { return &ParseError{segment.Index, offset, pid, message} }
	if packet[0] != 0x47 {
		return bad(-1, "missing transport sync byte")
	}
	pid := int(binary.BigEndian.Uint16(packet[1:3]) & 0x1fff)
	state := d.pids[pid]
	if state == nil {
		return nil
	}
	state.stats.TransportPackets++
	if packet[1]&0x80 != 0 {
		return bad(pid, "transport error indicator is set")
	}
	if packet[3]&0xc0 != 0 {
		return bad(pid, "scrambled transport payload cannot be extracted")
	}
	control := (packet[3] >> 4) & 3
	if control == 0 {
		return bad(pid, "reserved adaptation-field control")
	}
	payloadOffset, discontinuity, err := adaptation(packet, control)
	if err != nil {
		return bad(pid, err.Error())
	}
	cc, hasPayload := int(packet[3]&15), control&1 != 0
	if !discontinuity && state.lastCC >= 0 {
		expected := state.lastCC
		if hasPayload {
			expected = (expected + 1) & 15
		}
		if cc != expected {
			if hasPayload && cc == state.lastCC && state.havePayload && bytes.Equal(packet, state.lastPayload[:]) && d.cfg.AllowRetransmissions {
				state.stats.Retransmissions++
				return d.audit(Audit{Kind: AuditRetransmission, PID: uint16(pid), Segment: segment.Index, ByteOffset: offset, PTS: NoTimestamp, Bytes: len(packet) - payloadOffset, Dropped: true})
			}
			return bad(pid, fmt.Sprintf("continuity counter %d, expected %d; packet loss or an unapproved retransmission", cc, expected))
		}
	}
	state.lastCC = cc
	if discontinuity {
		state.stats.Discontinuities++
		if state.active {
			state.discontinuity = true
		} else {
			state.pendingDiscontinuity = true
		}
		if err := d.audit(Audit{Kind: AuditDiscontinuity, PID: uint16(pid), Segment: segment.Index, ByteOffset: offset, PTS: NoTimestamp}); err != nil {
			return err
		}
	}
	if !hasPayload {
		return nil
	}
	copy(state.lastPayload[:], packet)
	state.havePayload = true
	if packet[1]&0x40 != 0 {
		if state.active {
			if err := d.emit(state); err != nil {
				return err
			}
		}
		state.active, state.length = true, -1
		state.buffer = state.buffer[:0]
		state.segment, state.offset, state.arrival = segment, offset, arrival
		state.startPacket = d.stats.TransportPackets
		state.discontinuity = state.pendingDiscontinuity || discontinuity
		state.pendingDiscontinuity = false
	} else if !state.active {
		return bad(pid, "PES continuation without a preceding payload-unit start")
	}
	payload := packet[payloadOffset:]
	if len(payload) > d.cfg.MaxPESBytes-len(state.buffer) {
		return bad(pid, fmt.Sprintf("PES exceeds the %d-byte assembly limit", d.cfg.MaxPESBytes))
	}
	state.buffer = append(state.buffer, payload...)
	if state.length < 0 && len(state.buffer) >= 6 {
		if !bytes.Equal(state.buffer[:3], []byte{0, 0, 1}) {
			return bad(pid, "payload does not begin with a PES start code")
		}
		state.length = int(binary.BigEndian.Uint16(state.buffer[4:6]))
		if state.length != 0 {
			state.length += 6
			if state.length > d.cfg.MaxPESBytes {
				return bad(pid, fmt.Sprintf("declared PES size %d exceeds the assembly limit", state.length))
			}
		}
	}
	if state.length > 0 && len(state.buffer) >= state.length {
		if len(state.buffer) != state.length {
			return bad(pid, "transport payload extends beyond the declared PES length")
		}
		return d.emit(state)
	}
	return nil
}

func (d *Demuxer) emit(state *pidState) error {
	bad := func(message string) error {
		return &ParseError{state.segment.Index, state.offset, int(state.pid), message}
	}
	if state.length < 0 || state.length > 0 && len(state.buffer) != state.length {
		return bad(fmt.Sprintf("truncated PES: buffered %d bytes, declared %d", len(state.buffer), state.length))
	}
	pes, err := parsePES(state.buffer)
	if err != nil {
		return bad(err.Error())
	}
	pes.PID, pes.Segment, pes.ByteOffset = state.pid, state.segment.Index, state.offset
	pes.ArrivalTimestamp, pes.Discontinuity = state.arrival, state.discontinuity
	state.stats.PESPackets++
	if !state.segment.HasWindow {
		state.outside = false
	} else if pes.PTS != NoTimestamp {
		state.outside = pes.PTS < state.segment.In90k || pes.PTS >= state.segment.Out90k
	}
	drop := d.cfg.Strict && state.outside
	if state.outside {
		state.stats.OutsideWindowPES++
		if err := d.audit(Audit{Kind: AuditOutsideWindow, PID: state.pid, Segment: pes.Segment, ByteOffset: pes.ByteOffset, PTS: pes.PTS, Bytes: len(pes.Payload), Dropped: drop}); err != nil {
			return err
		}
	}
	if drop {
		state.stats.DroppedPES++
		state.stats.DroppedBytes += int64(len(pes.Payload))
	} else {
		if err := state.handler(pes); err != nil {
			return fmt.Errorf("PID 0x%04x PES handler at segment %d byte %d: %w", state.pid, pes.Segment, pes.ByteOffset, err)
		}
		state.stats.EmittedPES++
		state.stats.PayloadBytes += int64(len(pes.Payload))
	}
	state.active = false
	state.buffer = state.buffer[:0]
	return nil
}

func (d *Demuxer) audit(event Audit) error {
	if d.cfg.OnAudit != nil {
		if err := d.cfg.OnAudit(event); err != nil {
			return fmt.Errorf("transport audit %s: %w", event.Kind, err)
		}
	}
	return nil
}

func (d *Demuxer) flushAll() error {
	states := append([]*pidState(nil), d.selected...)
	sort.SliceStable(states, func(i, j int) bool { return states[i].startPacket < states[j].startPacket })
	for _, state := range states {
		if state.active {
			if err := d.emit(state); err != nil {
				return err
			}
		}
	}
	return nil
}

// Finish delivers final unbounded PES and rejects incomplete bounded packets.
// It is idempotent after success. A selected PID absent from all files has zero
// counters in Stats; callers decide whether their requested output requires it.
func (d *Demuxer) Finish() error {
	if d.failure != nil {
		return d.failure
	}
	if d.finished {
		return nil
	}
	if !d.haveSegment {
		return d.stop(fmt.Errorf("no transport segments were read"))
	}
	if err := d.flushAll(); err != nil {
		return d.stop(err)
	}
	d.finished = true
	return nil
}

func (d *Demuxer) stop(err error) error { d.failure = err; return err }

func detectPacketSize(reader *bufio.Reader) (int, error) {
	probe, err := reader.Peek(192 * 5)
	if err != nil && err != io.EOF && err != bufio.ErrBufferFull {
		return 0, fmt.Errorf("cannot inspect transport packet size: %w", err)
	}
	var candidates []int
	for _, size := range []int{188, 192} {
		if len(probe) < size {
			continue
		}
		valid := true
		for start := 0; start+size <= len(probe); start += size {
			if probe[start+size-188] != 0x47 {
				valid = false
				break
			}
		}
		if valid && (err != io.EOF || len(probe)%size == 0) {
			candidates = append(candidates, size)
		}
	}
	if len(candidates) != 1 {
		return 0, fmt.Errorf("cannot unambiguously identify 188-byte TS or 192-byte M2TS packets")
	}
	return candidates[0], nil
}
