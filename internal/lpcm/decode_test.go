package lpcm

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"testing"
)

func fixture(assignment, rate, depth, frames int) ([]byte, []byte) {
	a := assignments[assignment]
	width := 2
	if depth > 1 {
		width = 3
	}
	sourceChannels := (a.channels + 1) &^ 1
	b := make([]byte, 4+frames*sourceChannels*width)
	binary.BigEndian.PutUint16(b, uint16(len(b)-4))
	b[2] = byte(assignment<<4 | rate)
	b[3] = byte(depth << 6)
	want := make([]byte, frames*a.channels*width)
	for frame := 0; frame < frames; frame++ {
		for out := 0; out < a.channels; out++ {
			// Distinct channels and alternating signs expose remapping and sign loss.
			value := int32((frame+1)*137 + (out+1)*1009)
			if frame%2 == 0 {
				value = -value
			}
			if depth == 2 {
				value <<= 4
			}
			for i := 0; i < width; i++ {
				byteValue := byte(value >> uint(8*i))
				want[(frame*a.channels+out)*width+i] = byteValue
				b[4+(frame*sourceChannels+a.order[out])*width+width-1-i] = byteValue
			}
		}
	}
	return b, want
}

func TestAllAssignmentsRatesAndDepths(t *testing.T) {
	for _, assignment := range []int{1, 3, 4, 5, 6, 7, 8, 9, 10, 11} {
		for _, rate := range []int{1, 4, 5} {
			for _, depth := range []int{1, 2, 3} {
				t.Run(fmt.Sprintf("assignment%d_rate%d_depth%d", assignment, rate, depth), func(t *testing.T) {
					payload, want := fixture(assignment, rate, depth, 17)
					d, _ := New(Config{})
					result, err := d.Decode(payload)
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(result.PCM, want) || result.Frames != 17 || result.Format.Channels != assignments[assignment].channels {
						t.Fatalf("unexpected decode %+v", result)
					}
					if result.Format.Bits != [4]int{0, 16, 20, 24}[depth] {
						t.Fatal(result.Format)
					}
					width := result.Format.StorageBits / 8
					if result.PaddingBytes != 17*(result.Format.SourceChannels-result.Format.Channels)*width {
						t.Fatal(result.PaddingBytes)
					}
				})
			}
		}
	}
}

func TestMalformedPayloads(t *testing.T) {
	valid, _ := fixture(3, 1, 3, 3)
	cases := map[string][]byte{
		"short header": valid[:3], "truncated": valid[:len(valid)-1], "extra bytes": append(bytes.Clone(valid), 0), "empty": {0, 0, 0x31, 0xc0},
	}
	for name, edit := range map[string]func([]byte){
		"unknown assignment": func(b []byte) { b[2] = 0x21 }, "unknown rate": func(b []byte) { b[2] = 0x32 }, "unknown depth": func(b []byte) { b[3] = 0 },
		"partial frame": func(b []byte) { binary.BigEndian.PutUint16(b, uint16(len(b)-5)) },
	} {
		b := bytes.Clone(valid)
		edit(b)
		cases[name] = b
	}
	padding, _ := fixture(1, 1, 1, 2)
	padding[6] = 1
	cases["nonzero padding"] = padding
	partial := bytes.Clone(valid[:len(valid)-1])
	binary.BigEndian.PutUint16(partial, uint16(len(partial)-4))
	cases["declared partial sample frame"] = partial
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			d, _ := New(Config{})
			result, err := d.Decode(payload)
			if err == nil || len(result.PCM) != 0 {
				t.Fatalf("published malformed data: %v", err)
			}
		})
	}
	d, _ := New(Config{})
	_, err := d.Decode(valid[:len(valid)-1])
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal(err)
	}
}

func TestLargestPacketAndBoundedReuse(t *testing.T) {
	payload, want := fixture(11, 5, 3, 2730)
	d, _ := New(Config{})
	for i := 0; i < 100; i++ {
		result, err := d.Decode(payload)
		if err != nil || !bytes.Equal(result.PCM, want) {
			t.Fatal(err)
		}
		if len(d.buffer) != 65520 || cap(d.buffer) != 65520 {
			t.Fatal("packet memory did not stay bounded")
		}
	}
	old := bytes.Clone(d.buffer)
	tooLarge := make([]byte, MaxPayloadBytes+1)
	if _, err := d.Decode(tooLarge); err == nil {
		t.Fatal("accepted oversized input")
	}
	if !bytes.Equal(old, d.buffer) {
		t.Fatal("invalid packet modified published PCM")
	}
}

func TestFormatChangesAndBorrowedStorage(t *testing.T) {
	first, want := fixture(3, 1, 3, 20)
	d, _ := New(Config{})
	result, err := d.Decode(first)
	if err != nil {
		t.Fatal(err)
	}
	saved := bytes.Clone(result.PCM)
	for _, change := range [][3]int{{3, 4, 3}, {3, 1, 2}, {4, 1, 3}} {
		payload, _ := fixture(change[0], change[1], change[2], 20)
		_, err := d.Decode(payload)
		if !errors.Is(err, ErrFormatChange) {
			t.Fatal(err)
		}
		if !bytes.Equal(result.PCM, saved) {
			t.Fatal("failed decode modified borrowed output")
		}
	}
	if !bytes.Equal(result.PCM, want) {
		t.Fatal("changed source")
	}
	first[4] ^= 0xff
	result, err = d.Decode(first)
	if err != nil || bytes.Equal(result.PCM, saved) {
		t.Fatal("buffer was not reused")
	}
	if cap(d.buffer) > MaxPayloadBytes {
		t.Fatal("unbounded buffer")
	}
	d, _ = New(Config{MaxPayloadBytes: 10})
	if _, err = d.Decode(first); err == nil {
		t.Fatal("ignored payload bound")
	}
}

func TestTwentyBitStorageDoesNotDropLowBits(t *testing.T) {
	payload, _ := fixture(3, 1, 2, 1)
	payload[6] |= 15
	d, _ := New(Config{})
	result, err := d.Decode(payload)
	if err != nil || result.PCM[0] != payload[6] {
		t.Fatal("lost original 24-bit storage")
	}
}

func FuzzDecodeBounded(f *testing.F) {
	for _, assignment := range []int{1, 3, 9, 10, 11} {
		b, _ := fixture(assignment, 1, 3, 10)
		f.Add(b)
	}
	f.Fuzz(func(t *testing.T, payload []byte) {
		d, _ := New(Config{})
		result, err := d.Decode(payload)
		if err == nil && len(result.PCM) > MaxPayloadBytes {
			t.Fatal("unbounded PCM")
		}
		if cap(d.buffer) > MaxPayloadBytes {
			t.Fatal("unbounded buffer")
		}
	})
}
