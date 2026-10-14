package seamless

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Parser accepts arbitrarily split elementary payloads. It retains at most one
// bounded composite frame and ten header/lookahead bytes, independent of input size.
// It rejects malformed input instead of discarding bytes to find another sync.
type Parser struct {
	config           Config
	buffer           []byte
	frame            Frame
	coreSize, target int
	finished         bool
	failure          error
}

func NewParser(config Config) (*Parser, error) {
	if config.Codec != AC3 && config.Codec != DTS {
		return nil, fmt.Errorf("%w: codec %d", ErrUnsupported, config.Codec)
	}
	if config.OnFrame == nil {
		return nil, fmt.Errorf("audio frame callback is required")
	}
	if config.MaxFrameBytes == 0 {
		config.MaxFrameBytes = DefaultMaxFrameBytes
	}
	if config.MaxFrameBytes < 16 {
		return nil, fmt.Errorf("audio frame bound must be at least 16 bytes")
	}
	return &Parser{config: config}, nil
}

// PendingBytes is the number of bytes queued at a transport boundary. It can be
// used as the signed comparison budget for the reference overlap policy.
func (p *Parser) PendingBytes() int { return len(p.buffer) }

func (p *Parser) Write(input []byte) (int, error) {
	if p.failure != nil {
		return 0, p.failure
	}
	if p.finished {
		return 0, ErrFinished
	}
	consumed := 0
	for {
		need, ready, err := p.next(false)
		if err != nil {
			p.failure = err
			return consumed, err
		}
		if ready {
			if err = p.emit(); err != nil {
				p.failure = err
				return consumed, err
			}
			continue
		}
		if len(input) == 0 {
			return consumed, nil
		}
		count := need - len(p.buffer)
		if count <= 0 {
			err = fmt.Errorf("invalid audio parser state")
			p.failure = err
			return consumed, err
		}
		if count > len(input) {
			count = len(input)
		}
		p.buffer = append(p.buffer, input[:count]...)
		input = input[count:]
		consumed += count
	}
}

// Flush emits a final complete core or composite and reports any truncated tail.
// Flush is terminal; input cannot be appended afterward.
func (p *Parser) Flush() error {
	if p.failure != nil {
		return p.failure
	}
	if p.finished {
		return ErrFinished
	}
	p.finished = true
	for len(p.buffer) > 0 {
		_, ready, err := p.next(true)
		if err == nil && !ready {
			err = io.ErrUnexpectedEOF
		}
		if err != nil {
			p.failure = err
			return err
		}
		if err = p.emit(); err != nil {
			p.failure = err
			return err
		}
	}
	return nil
}

func (p *Parser) next(eof bool) (int, bool, error) {
	headerSize := 8
	if p.config.Codec == DTS {
		headerSize = 14
	}
	if p.coreSize == 0 {
		if len(p.buffer) < headerSize {
			return headerSize, false, nil
		}
		var err error
		if p.config.Codec == DTS {
			p.frame, p.coreSize, err = dtsCoreHeader(p.buffer)
		} else {
			p.frame, p.coreSize, err = ac3Header(p.buffer)
		}
		if err != nil {
			return 0, false, err
		}
		if p.coreSize > p.config.MaxFrameBytes {
			return 0, false, fmt.Errorf("audio frame exceeds %d-byte bound", p.config.MaxFrameBytes)
		}
	}
	if len(p.buffer) < p.coreSize {
		return p.coreSize, false, nil
	}
	if p.config.Codec == AC3 {
		p.target = p.coreSize
		return 0, true, nil
	}
	if p.target > 0 {
		if len(p.buffer) < p.target {
			return p.target, false, nil
		}
		return 0, true, nil
	}
	// The core's declared length can be followed by zero padding to a four-byte
	// boundary. Preserve that padding when it precedes the extension.
	end := p.coreSize
	if len(p.buffer) < end+4 {
		if eof && len(p.buffer) == end {
			p.target = end
			return 0, true, nil
		}
		return end + 4, false, nil
	}
	marker := binary.BigEndian.Uint32(p.buffer[end:])
	if marker != 0x64582025 {
		if isCore(marker) {
			p.target = end
			return 0, true, nil
		}
		aligned := (end + 3) &^ 3
		if aligned == end {
			return 0, false, fmt.Errorf("invalid data after DTS core")
		}
		if aligned > p.config.MaxFrameBytes {
			return 0, false, fmt.Errorf("padded DTS core exceeds %d-byte bound", p.config.MaxFrameBytes)
		}
		for _, b := range p.buffer[end:min(aligned, len(p.buffer))] {
			if b != 0 {
				return 0, false, fmt.Errorf("invalid DTS core padding")
			}
		}
		if len(p.buffer) < aligned+4 {
			return aligned + 4, false, nil
		}
		if binary.BigEndian.Uint32(p.buffer[aligned:]) != 0x64582025 {
			return 0, false, fmt.Errorf("invalid data after DTS core padding")
		}
		p.frame.PaddingBytes = aligned - end
		end = aligned
	}
	if len(p.buffer) < end+10 {
		return end + 10, false, nil
	}
	extSize, err := exssHeader(p.buffer[end:])
	if err != nil {
		return 0, false, err
	}
	if extSize > p.config.MaxFrameBytes-end {
		return 0, false, fmt.Errorf("DTS composite exceeds %d-byte bound", p.config.MaxFrameBytes)
	}
	p.frame.ExtensionBytes = extSize
	p.target = end + extSize
	if len(p.buffer) < p.target {
		return p.target, false, nil
	}
	return 0, true, nil
}

func (p *Parser) emit() error {
	p.frame.Bytes = p.buffer[:p.target]
	if err := p.config.OnFrame(p.frame); err != nil {
		return err
	}
	remaining := copy(p.buffer, p.buffer[p.target:])
	p.buffer = p.buffer[:remaining]
	p.frame = Frame{}
	p.coreSize = 0
	p.target = 0
	return nil
}

func isCore(marker uint32) bool {
	return marker == 0x7ffe8001 || marker == 0xfe7f0180 || marker == 0x1fffe800 || marker == 0xff1f00e8
}
