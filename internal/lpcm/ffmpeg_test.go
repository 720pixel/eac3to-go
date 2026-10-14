package lpcm

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os/exec"
	"testing"
)

func sectionCRC(section []byte) []byte {
	crc := uint32(0xffffffff)
	for _, b := range section {
		crc ^= uint32(b) << 24
		for i := 0; i < 8; i++ {
			if crc&0x80000000 != 0 {
				crc = crc<<1 ^ 0x04c11db7
			} else {
				crc <<= 1
			}
		}
	}
	return binary.BigEndian.AppendUint32(section, crc)
}

func transportPacket(pid uint16, start bool, cc byte, payload []byte) []byte {
	b := bytes.Repeat([]byte{0xff}, 188)
	b[0] = 0x47
	b[1] = byte(pid >> 8)
	if start {
		b[1] |= 0x40
	}
	b[2] = byte(pid)
	b[3] = 0x10 | cc
	offset := 4
	if len(payload) < 184 {
		b[3] |= 0x20
		b[4] = byte(183 - len(payload))
		if b[4] > 0 {
			b[5] = 0
		}
		offset = 188 - len(payload)
	}
	copy(b[offset:], payload)
	return b
}

func transportFixture(payload []byte) []byte {
	pat := sectionCRC([]byte{0, 0xb0, 13, 0, 1, 0xc1, 0, 0, 0, 1, 0xf0, 0})
	pmt := []byte{2, 0xb0, 24, 0, 1, 0xc1, 0, 0, 0xf1, 0, 0xf0, 6, 5, 4, 'H', 'D', 'M', 'V', 0x80, 0xf1, 0, 0xf0, 0}
	pmt = sectionCRC(pmt)
	out := append(transportPacket(0, true, 0, append([]byte{0}, pat...)), transportPacket(0x1000, true, 0, append([]byte{0}, pmt...))...)
	pes := []byte{0, 0, 1, 0xbd, 0, 0, 0x80, 0x80, 5, 0x21, 0, 1, 0, 1}
	binary.BigEndian.PutUint16(pes[4:6], uint16(len(payload)+8))
	pes = append(pes, payload...)
	cc := byte(0)
	start := true
	for len(pes) > 0 {
		n := min(184, len(pes))
		out = append(out, transportPacket(0x1100, start, cc, pes[:n])...)
		start = false
		cc = (cc + 1) & 15
		pes = pes[n:]
	}
	return out
}

func TestFFmpegChannelAndSampleInterop(t *testing.T) {
	if testing.Short() {
		t.Skip("external media interoperability disabled")
	}
	ffmpeg, err := exec.LookPath("/usr/bin/ffmpeg")
	if err != nil {
		ffmpeg, err = exec.LookPath("ffmpeg")
	}
	if err != nil {
		t.Skip("FFmpeg unavailable")
	}
	for _, assignment := range []int{1, 3, 4, 5, 6, 7, 8, 9, 10, 11} {
		for _, rate := range []int{1, 4, 5} {
			for _, depth := range []int{1, 2, 3} {
				t.Run(fmt.Sprintf("assignment%d_rate%d_depth%d", assignment, rate, depth), func(t *testing.T) {
					payload, want := fixture(assignment, rate, depth, 300)
					d, _ := New(Config{})
					result, err := d.Decode(payload)
					if err != nil {
						t.Fatal(err)
					}
					// FFmpeg 6.1 rejects the 20-bit quantization code. The same storage
					// is independently decoded using its 24-bit code; coded depth stays
					// 20 in the native metadata and no sample bits are discarded.
					if depth == 2 {
						payload[3] = 3 << 6
					}
					format := "s24le"
					if depth == 1 {
						format = "s16le"
					}
					cmd := exec.Command(ffmpeg, "-v", "error", "-f", "mpegts", "-i", "pipe:0", "-map", "0:a:0", "-c:a", "pcm_"+format, "-f", format, "pipe:1")
					cmd.Stdin = bytes.NewReader(transportFixture(payload))
					var out, stderr bytes.Buffer
					cmd.Stdout = &out
					cmd.Stderr = &stderr
					if err = cmd.Run(); err != nil || stderr.Len() != 0 {
						t.Fatalf("FFmpeg: %v %s", err, stderr.String())
					}
					if !bytes.Equal(out.Bytes(), want) || !bytes.Equal(out.Bytes(), result.PCM) {
						t.Fatalf("sample/channel mismatch: native %d reference %d", len(result.PCM), out.Len())
					}
				})
			}
		}
	}
}
