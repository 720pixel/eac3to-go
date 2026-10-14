package pgstiming

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
)

const MaxSegmentBytes = 65535

var ErrFinished = errors.New("PGS assembler is finished")

type Stamp struct{ PTS90k, DTS90k int64 }

type Config struct {
	Output io.Writer
	Clock  ClockConfig
	// PreserveDTS maps a present decoding timestamp; default writes zero,
	// matching the reference SUP writer. Missing DTS is always encoded as zero.
	PreserveDTS     bool
	MaxSegmentBytes int
}

type Stats struct{ Segments, PayloadBytes, OutputBytes int64 }

// Assembler retains at most one SUP segment. A pipe writer lets the existing
// forced-PGS reader consume its output directly, without buffering a whole file.
type Assembler struct {
	config         Config
	packet         []byte
	queued, target int
	stamp          Stamp
	stats          Stats
	failure        error
	finished       bool
}

func New(config Config) (*Assembler, error) {
	if config.Output == nil {
		return nil, fmt.Errorf("PGS output writer is required")
	}
	if config.MaxSegmentBytes == 0 {
		config.MaxSegmentBytes = MaxSegmentBytes
	}
	if config.MaxSegmentBytes < 0 || config.MaxSegmentBytes > MaxSegmentBytes {
		return nil, fmt.Errorf("invalid PGS segment bound")
	}
	return &Assembler{config: config, packet: make([]byte, 13+config.MaxSegmentBytes)}, nil
}

func (a *Assembler) Stats() Stats      { return a.stats }
func (a *Assembler) PendingBytes() int { return a.queued }

// WritePES accepts arbitrarily split three-byte segment headers and bodies.
// Each segment retains the timestamp supplied with its first byte. Subsequent
// segments starting within this PES inherit this PES's stamp.
func (a *Assembler) WritePES(payload []byte, stamp Stamp) error {
	if a.failure != nil {
		return a.failure
	}
	if a.finished {
		return ErrFinished
	}
	for len(payload) > 0 {
		if a.queued == 0 {
			a.stamp = stamp
		}
		need := 3
		if a.target > 0 {
			need = a.target
		}
		count := min(len(payload), need-a.queued)
		copy(a.packet[10+a.queued:], payload[:count])
		a.queued += count
		payload = payload[count:]
		if a.queued < 3 {
			continue
		}
		if a.target == 0 {
			kind := a.packet[10]
			if kind != 0x14 && kind != 0x15 && kind != 0x16 && kind != 0x17 && kind != 0x80 {
				return a.fail(fmt.Errorf("unknown PGS segment type 0x%02x", kind))
			}
			size := int(binary.BigEndian.Uint16(a.packet[11:13]))
			if size > a.config.MaxSegmentBytes {
				return a.fail(fmt.Errorf("PGS segment exceeds %d-byte bound", a.config.MaxSegmentBytes))
			}
			if kind == 0x80 && size != 0 {
				return a.fail(fmt.Errorf("nonempty PGS END segment"))
			}
			a.target = 3 + size
		}
		if a.queued == a.target {
			if err := a.emit(); err != nil {
				return a.fail(err)
			}
		}
	}
	return nil
}

func (a *Assembler) emit() error {
	if a.stamp.PTS90k == NoTimestamp {
		return fmt.Errorf("PGS segment has no starting presentation timestamp")
	}
	pts, err := SUPClock(a.stamp.PTS90k, a.config.Clock)
	if err != nil {
		return err
	}
	dts := uint32(0)
	if a.config.PreserveDTS && a.stamp.DTS90k != NoTimestamp {
		dts, err = SUPClock(a.stamp.DTS90k, a.config.Clock)
		if err != nil {
			return err
		}
	}
	a.packet[0], a.packet[1] = 'P', 'G'
	binary.BigEndian.PutUint32(a.packet[2:6], pts)
	binary.BigEndian.PutUint32(a.packet[6:10], dts)
	packet := a.packet[:10+a.target]
	n, err := a.config.Output.Write(packet)
	if n < 0 || n > len(packet) {
		return fmt.Errorf("invalid PGS writer count %d", n)
	}
	a.stats.OutputBytes += int64(n)
	if err != nil {
		return err
	}
	if n != len(packet) {
		return io.ErrShortWrite
	}
	a.stats.Segments++
	a.stats.PayloadBytes += int64(a.target - 3)
	a.target, a.queued = 0, 0
	return nil
}

func (a *Assembler) Finish() error {
	if a.failure != nil {
		return a.failure
	}
	if a.finished {
		return ErrFinished
	}
	a.finished = true
	if a.queued != 0 {
		return a.fail(io.ErrUnexpectedEOF)
	}
	return nil
}

func (a *Assembler) fail(err error) error { a.failure = err; return err }
