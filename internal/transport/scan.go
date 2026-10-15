package transport

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sort"
)

const timestampWrap int64 = 1 << 33

type ScanConfig struct {
	PacketSize int
	VideoPIDs  map[uint16]bool
	// PIDs restricts timestamp parsing to known elementary streams. Packet
	// boundaries and sync bytes remain checked for the complete input.
	PIDs map[uint16]bool
	// WindowBytes is the size of each aligned beginning/end sample. Zero uses
	// 4 MiB. FullScan reads the whole file regardless of WindowBytes.
	WindowBytes int64
	FullScan    bool
}

type ScanExtent struct {
	Offset int64
	Length int64
}

// TimestampRange reports the earliest timestamp in the beginning sample and
// the latest in the end sample, including presentation reordering. FirstPTS is
// normalized to one 33-bit cycle; LastPTS is unwrapped relative to it. Bounded
// scans assume fewer than one complete clock cycle between sampled extents.
// HasFirst/HasLast distinguish missing observations from timestamp zero.
// LastPTS is a presentation instant, not the end of that picture or its duration.
type TimestampRange struct {
	FirstPTS, LastPTS       int64
	FirstOffset, LastOffset int64
	HasFirst, HasLast       bool
	Samples                 int64
	Wrapped                 bool
}

type ScanResult struct {
	PacketSize int
	FileSize   int64
	// TrailingBytes reports a partial transport packet excluded from the scan.
	// FullScan means every complete packet was examined, including when this
	// diagnostic is nonzero.
	TrailingBytes                                       int64
	FullScan                                            bool
	Extents                                             []ScanExtent
	PacketsScanned, BytesScanned                        int64
	HasPTS, UsedVideo                                   bool
	FirstPTS, LastPTS                                   int64
	ByPID                                               map[uint16]TimestampRange
	TransportErrors, ScrambledPackets, ContinuityErrors int64
	Retransmissions, Discontinuities, IncompleteHeaders int64
}

type timestampRange struct {
	count                           int64
	min, max, previous, previousRaw int64
	minOffset, maxOffset            int64
}

func (r *timestampRange) add(raw, offset int64) error {
	value := raw
	if r.count > 0 {
		delta, err := TimestampDelta(raw, r.previousRaw)
		if err != nil {
			return err
		}
		value = r.previous + delta
	}
	if r.count == 0 || value < r.min {
		r.min, r.minOffset = value, offset
	}
	if r.count == 0 || value > r.max {
		r.max, r.maxOffset = value, offset
	}
	r.previous, r.previousRaw = value, raw
	r.count++
	return nil
}

// TimestampDelta returns the shortest signed displacement on a 33-bit clock.
// An exactly half-cycle displacement is ambiguous and returns an error. This
// helper does not establish whether two timestamps belong to adjacent segments.
func TimestampDelta(current, previous int64) (int64, error) {
	if current < 0 || previous < 0 {
		return 0, fmt.Errorf("timestamps must be nonnegative")
	}
	delta := current%timestampWrap - previous%timestampWrap
	if delta == timestampWrap/2 || delta == -timestampWrap/2 {
		return 0, fmt.Errorf("timestamp displacement is an ambiguous half clock cycle")
	}
	if delta > timestampWrap/2 {
		delta -= timestampWrap
	}
	if delta < -timestampWrap/2 {
		delta += timestampWrap
	}
	return delta, nil
}

// ScanFile inventories timestamps without emitting or changing payloads.
// Video PIDs are preferred only when beginning and end observations both exist;
// otherwise complete ranges from all PES PIDs are used. HasPTS=false means no
// boundary timestamps can be established. A bounded sample cannot prove the
// absence of errors, clock resets or extra wraps in the unexamined middle.
// FullScan examines every packet but is not a codec integrity check.
func ScanFile(ctx context.Context, path string, cfg ScanConfig) (ScanResult, error) {
	result := ScanResult{FirstPTS: NoTimestamp, LastPTS: NoTimestamp, ByPID: make(map[uint16]TimestampRange)}
	if cfg.PacketSize != 0 && cfg.PacketSize != 188 && cfg.PacketSize != 192 {
		return result, fmt.Errorf("transport packet size must be 188, 192 or zero")
	}
	if cfg.WindowBytes < 0 {
		return result, fmt.Errorf("timestamp scan window must be nonnegative")
	}
	if cfg.WindowBytes == 0 {
		cfg.WindowBytes = 4 << 20
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	file, err := os.Open(path)
	if err != nil {
		return result, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return result, err
	}
	if !info.Mode().IsRegular() {
		return result, fmt.Errorf("timestamp scan requires a regular file")
	}
	result.FileSize = info.Size()
	result.PacketSize = cfg.PacketSize
	if result.PacketSize == 0 {
		// Inspect a complete packet prefix even when the file has a short tail.
		probe := make([]byte, 192*5)
		n, readErr := file.ReadAt(probe, 0)
		if readErr != nil && readErr != io.EOF {
			return result, readErr
		}
		probe = probe[:n]
		var candidates []int
		for _, size := range []int{188, 192} {
			valid := len(probe) >= size
			for offset := 0; valid && offset+size <= len(probe); offset += size {
				valid = probe[offset+size-188] == 0x47
			}
			if valid {
				candidates = append(candidates, size)
			}
		}
		if len(candidates) != 1 {
			return result, fmt.Errorf("cannot unambiguously detect transport packet size")
		}
		result.PacketSize = candidates[0]
	}
	size := int64(result.PacketSize)
	result.TrailingBytes = result.FileSize % size
	complete := result.FileSize - result.TrailingBytes
	if complete == 0 {
		return result, fmt.Errorf("file has no complete transport packet")
	}
	window := min(complete, max(size, cfg.WindowBytes/size*size))
	if cfg.FullScan || window >= (complete+1)/2 {
		result.Extents = []ScanExtent{{0, complete}}
		result.FullScan = true
	} else {
		result.Extents = []ScanExtent{{0, window}, {complete - window, window}}
	}
	ranges := make([]map[uint16]*timestampRange, len(result.Extents))
	for i, extent := range result.Extents {
		ranges[i] = make(map[uint16]*timestampRange)
		if err := scanExtent(ctx, file, extent, result.PacketSize, cfg.PIDs, ranges[i], &result); err != nil {
			return result, err
		}
	}
	first, last := ranges[0], ranges[len(ranges)-1]
	allPIDs := make(map[uint16]bool)
	for pid := range first {
		allPIDs[pid] = true
	}
	for pid := range last {
		allPIDs[pid] = true
	}
	for pid := range allPIDs {
		a, b := first[pid], last[pid]
		r := TimestampRange{FirstPTS: NoTimestamp, LastPTS: NoTimestamp, FirstOffset: -1, LastOffset: -1}
		if a != nil && a.count > 0 {
			r.HasFirst = true
			r.FirstPTS = positiveClock(a.min)
			r.FirstOffset = a.minOffset
			r.Samples = a.count
		}
		if b != nil && b.count > 0 {
			r.HasLast = true
			r.LastPTS = positiveClock(b.max)
			r.LastOffset = b.maxOffset
			if a != b {
				r.Samples += b.count
			}
		}
		if r.HasFirst && r.HasLast {
			if result.FullScan {
				r.LastPTS = r.FirstPTS + b.max - a.min
			} else {
				r.LastPTS = r.FirstPTS + positiveClock(b.max-r.FirstPTS)
			}
			r.Wrapped = r.LastPTS >= timestampWrap
		}
		result.ByPID[pid] = r
	}
	var preferred, fallback []uint16
	for pid, r := range result.ByPID {
		if r.HasFirst && r.HasLast {
			fallback = append(fallback, pid)
			if cfg.VideoPIDs[pid] {
				preferred = append(preferred, pid)
			}
		}
	}
	selected := preferred
	if len(selected) == 0 {
		selected = fallback
	} else {
		result.UsedVideo = true
	}
	if len(selected) > 0 {
		sort.Slice(selected, func(i, j int) bool { return selected[i] < selected[j] })
		anchor := result.ByPID[selected[0]].FirstPTS
		minimum, maximum := anchor, anchor
		for _, pid := range selected {
			r := result.ByPID[pid]
			delta, err := TimestampDelta(r.FirstPTS, anchor)
			if err != nil {
				return result, err
			}
			start := anchor + delta
			minimum = min(minimum, start)
			maximum = max(maximum, start+r.LastPTS-r.FirstPTS)
		}
		result.FirstPTS = positiveClock(minimum)
		result.LastPTS = result.FirstPTS + maximum - minimum
		result.HasPTS = true
	}
	return result, nil
}

type scanPID struct {
	cc                  int
	haveCC, havePayload bool
	lastPayload         [188]byte
	header              []byte
	active              bool
	start               int64
}

func scanExtent(ctx context.Context, file io.ReaderAt, extent ScanExtent, packetSize int, selected map[uint16]bool, ranges map[uint16]*timestampRange, result *ScanResult) error {
	reader := bufio.NewReaderSize(io.NewSectionReader(file, extent.Offset, extent.Length), 256<<10)
	states := make(map[uint16]*scanPID)
	var buffer [192]byte
	for offset := extent.Offset; offset < extent.Offset+extent.Length; offset += int64(packetSize) {
		if result.PacketsScanned&255 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if _, err := io.ReadFull(reader, buffer[:packetSize]); err != nil {
			return fmt.Errorf("timestamp scan at byte %d: %w", offset, err)
		}
		result.PacketsScanned++
		result.BytesScanned += int64(packetSize)
		packet := buffer[packetSize-188 : packetSize]
		if packet[0] != 0x47 {
			return &ParseError{ByteOffset: offset, PID: -1, Problem: "missing transport sync byte"}
		}
		pid := binary.BigEndian.Uint16(packet[1:3]) & 0x1fff
		if pid == 0x1fff || selected != nil && !selected[pid] {
			continue
		}
		state := states[pid]
		if state == nil {
			state = &scanPID{}
			states[pid] = state
		}
		if packet[1]&0x80 != 0 {
			result.TransportErrors++
			state.active = false
			continue
		}
		if packet[3]&0xc0 != 0 {
			result.ScrambledPackets++
			state.active = false
			continue
		}
		control := (packet[3] >> 4) & 3
		if control == 0 {
			return &ParseError{ByteOffset: offset, PID: int(pid), Problem: "reserved adaptation-field control"}
		}
		start, discontinuity, err := adaptation(packet, control)
		if err != nil {
			return &ParseError{ByteOffset: offset, PID: int(pid), Problem: err.Error()}
		}
		cc, payload := int(packet[3]&15), control&1 != 0
		if discontinuity {
			result.Discontinuities++
		}
		if state.haveCC && !discontinuity {
			expected := state.cc
			if payload {
				expected = (expected + 1) & 15
			}
			if cc != expected {
				if payload && cc == state.cc && state.havePayload && bytes.Equal(packet, state.lastPayload[:]) {
					result.Retransmissions++
					continue
				}
				result.ContinuityErrors++
				state.active = false
			}
		}
		state.cc, state.haveCC = cc, true
		if !payload {
			continue
		}
		copy(state.lastPayload[:], packet)
		state.havePayload = true
		if packet[1]&0x40 != 0 {
			if state.active {
				result.IncompleteHeaders++
			}
			state.active, state.start = true, offset
			state.header = state.header[:0]
		}
		if !state.active {
			continue
		}
		state.header = append(state.header, packet[start:start+min(len(packet)-start, 264-len(state.header))]...)
		header := state.header
		if len(header) < 3 {
			continue
		}
		if !bytes.Equal(header[:3], []byte{0, 0, 1}) {
			state.active = false
			continue
		}
		if len(header) < 6 {
			continue
		}
		switch header[3] {
		case 0xbc, 0xbe, 0xbf, 0xf0, 0xf1, 0xf2, 0xf8, 0xff:
			state.active = false
			continue
		}
		if len(header) < 9 {
			continue
		}
		length := 9 + int(header[8])
		if len(header) < length {
			continue
		}
		declared := int(binary.BigEndian.Uint16(header[4:6]))
		if declared != 0 && declared+6 < length {
			return &ParseError{ByteOffset: state.start, PID: int(pid), Problem: "PES header exceeds declared packet length"}
		}
		pes, err := parsePES(header[:length])
		if err != nil {
			return &ParseError{ByteOffset: state.start, PID: int(pid), Problem: err.Error()}
		}
		state.active = false
		if pes.PTS == NoTimestamp {
			continue
		}
		r := ranges[pid]
		if r == nil {
			r = &timestampRange{}
			ranges[pid] = r
		}
		if err := r.add(pes.PTS, state.start); err != nil {
			return fmt.Errorf("PID 0x%04x at byte %d: %w", pid, state.start, err)
		}
	}
	for _, state := range states {
		if state.active {
			result.IncompleteHeaders++
		}
	}
	return nil
}

func positiveClock(value int64) int64 {
	value %= timestampWrap
	if value < 0 {
		value += timestampWrap
	}
	return value
}
