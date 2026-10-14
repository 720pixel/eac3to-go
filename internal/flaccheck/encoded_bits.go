package flaccheck

import (
	"bufio"
	"fmt"
	"io"
	"math/bits"
)

var frameCRC8 = func() [256]byte {
	var table [256]byte
	for i := range table {
		value := byte(i)
		for j := 0; j < 8; j++ {
			if value&128 != 0 {
				value = value<<1 ^ 7
			} else {
				value <<= 1
			}
		}
		table[i] = value
	}
	return table
}()

var frameCRC16 = func() [256]uint16 {
	var table [256]uint16
	for i := range table {
		value := uint16(i) << 8
		for j := 0; j < 8; j++ {
			if value&0x8000 != 0 {
				value = value<<1 ^ 0x8005
			} else {
				value <<= 1
			}
		}
		table[i] = value
	}
	return table
}()

type frameBits struct {
	reader        *bufio.Reader
	offset, start uint64
	maxBytes      uint64
	current       byte
	remaining     uint8
	crc8          byte
	crc16         uint16
	header        bool
	err           error
	scratch       [8192]byte
}

func (b *frameBits) bad(format string, args ...any) {
	if b.err == nil {
		b.err = fmt.Errorf("%w: %s", ErrCorrupt, fmt.Sprintf(format, args...))
	}
}

func (b *frameBits) byte() byte {
	if b.err != nil {
		return 0
	}
	if b.maxBytes != 0 && b.offset-b.start >= b.maxBytes {
		b.bad("encoded frame exceeds STREAMINFO maximum size")
		return 0
	}
	value, err := b.reader.ReadByte()
	if err != nil {
		b.err = encodedReadError("FLAC encoded frame", err)
		return 0
	}
	b.offset++
	b.crc16 = b.crc16<<8 ^ frameCRC16[byte(b.crc16>>8)^value]
	if b.header {
		b.crc8 = frameCRC8[b.crc8^value]
	}
	return value
}

func (b *frameBits) read(n uint) uint64 {
	var value uint64
	for n != 0 && b.err == nil {
		if b.remaining == 0 {
			b.current, b.remaining = b.byte(), 8
		}
		take := min(n, uint(b.remaining))
		b.remaining -= uint8(take)
		value = value<<take | uint64(b.current>>b.remaining)&(1<<take-1)
		n -= take
	}
	return value
}

func (b *frameBits) skip(n uint64) {
	if b.remaining != 0 {
		take := min(n, uint64(b.remaining))
		b.read(uint(take))
		n -= take
	}
	for n >= 8 && b.err == nil {
		count := min(n/8, uint64(len(b.scratch)))
		if b.maxBytes != 0 && count > b.maxBytes-(b.offset-b.start) {
			b.bad("encoded frame exceeds STREAMINFO maximum size")
			return
		}
		got, err := io.ReadFull(b.reader, b.scratch[:count])
		for _, value := range b.scratch[:got] {
			b.crc16 = b.crc16<<8 ^ frameCRC16[byte(b.crc16>>8)^value]
		}
		b.offset += uint64(got)
		if err != nil {
			b.err = encodedReadError("FLAC subframe", err)
			return
		}
		n -= count * 8
	}
	b.read(uint(n))
}

func (b *frameBits) unary(limit uint64) uint64 {
	var zeros uint64
	for b.err == nil {
		if b.remaining == 0 {
			b.current, b.remaining = b.byte(), 8
			if b.err != nil {
				break
			}
		}
		leading := uint8(bits.LeadingZeros8(b.current << (8 - b.remaining)))
		if leading < b.remaining {
			zeros += uint64(leading)
			b.remaining -= leading + 1
			if zeros > limit {
				b.bad("unary-coded value exceeds its legal range")
			}
			return zeros
		}
		zeros += uint64(b.remaining)
		b.remaining = 0
		if zeros > limit {
			b.bad("unary-coded value exceeds its legal range")
		}
	}
	return 0
}

func (b *frameBits) subframe(block uint32, depth int) {
	header := b.read(8)
	if header&128 != 0 {
		b.bad("nonzero reserved subframe bit")
		return
	}
	if header&1 != 0 {
		wasted := b.unary(uint64(depth-2)) + 1
		depth -= int(wasted)
	}
	if b.err != nil {
		return
	}
	kind := header >> 1
	var order uint32
	switch {
	case kind == 0:
		b.skip(uint64(depth))
		return
	case kind == 1:
		b.skip(uint64(depth) * uint64(block))
		return
	case kind >= 8 && kind <= 12:
		order = uint32(kind - 8)
	case kind >= 32:
		order = uint32(kind - 31)
	default:
		b.bad("reserved subframe type %d", kind)
		return
	}
	if order >= block {
		b.bad("predictor order %d is not smaller than block size %d", order, block)
		return
	}
	b.skip(uint64(order) * uint64(depth))
	if kind >= 32 {
		precision := b.read(4) + 1
		shift := b.read(5)
		if precision == 16 || shift >= 16 {
			b.bad("forbidden LPC precision or negative prediction shift")
			return
		}
		b.skip(precision * uint64(order))
	}
	b.residual(block, order)
}

func (b *frameBits) residual(block, order uint32) {
	method := b.read(2)
	partitionOrder := b.read(4)
	if method > 1 {
		b.bad("reserved residual coding method")
		return
	}
	partitions := uint32(1) << partitionOrder
	if block%partitions != 0 || block/partitions <= order {
		b.bad("invalid residual partition order for block/predictor size")
		return
	}
	parameterBits := uint(4 + method)
	escape := uint64(1)<<parameterBits - 1
	for partition := uint32(0); partition < partitions && b.err == nil; partition++ {
		count := block / partitions
		if partition == 0 {
			count -= order
		}
		parameter := b.read(parameterBits)
		if parameter == escape {
			width := b.read(5)
			b.skip(uint64(count) * width)
			continue
		}
		for i := uint32(0); i < count && b.err == nil; i++ {
			quotient := b.unary(uint64(0xfffffffe) >> parameter)
			remainder := b.read(uint(parameter))
			if quotient<<parameter|remainder > 0xfffffffe {
				b.bad("residual sample exceeds the signed 32-bit FLAC range")
			}
		}
	}
}
