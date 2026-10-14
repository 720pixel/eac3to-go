package flaccheck

import (
	"encoding/binary"
	"encoding/hex"
	"testing"
)

func streamInfoBytes(info Info) []byte {
	data := make([]byte, 34)
	binary.BigEndian.PutUint16(data[:2], info.MinBlockSize)
	binary.BigEndian.PutUint16(data[2:4], info.MaxBlockSize)
	data[4], data[5], data[6] = byte(info.MinFrameSize>>16), byte(info.MinFrameSize>>8), byte(info.MinFrameSize)
	data[7], data[8], data[9] = byte(info.MaxFrameSize>>16), byte(info.MaxFrameSize>>8), byte(info.MaxFrameSize)
	packed := uint64(info.SampleRate)<<44 | uint64(info.Channels-1)<<41 | uint64(info.BitsPerSample-1)<<36 | info.TotalFrames
	binary.BigEndian.PutUint64(data[10:18], packed)
	copy(data[18:], info.MD5[:])
	return data
}

func decodeHex(t testing.TB, value string) []byte {
	t.Helper()
	data, err := hex.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestParseStreamInfoRFCExample(t *testing.T) {
	// RFC 9639, Appendix D.1: one stereo frame, with 4096-frame block sizes.
	data := decodeHex(t, "1000100000000f00000f0ac442f0000000013e84b41807dc690307586a3dad1a2e0f")
	info, err := ParseStreamInfo(data)
	if err != nil {
		t.Fatal(err)
	}
	if info.SampleRate != 44100 || info.Channels != 2 || info.BitsPerSample != 16 || info.TotalFrames != 1 {
		t.Fatalf("incorrect stream format: %+v", info)
	}
	if info.MinBlockSize != 4096 || info.MaxBlockSize != 4096 || info.MinFrameSize != 15 || info.MaxFrameSize != 15 {
		t.Fatalf("incorrect block/frame sizes: %+v", info)
	}
	if got := hex.EncodeToString(info.MD5[:]); got != "3e84b41807dc690307586a3dad1a2e0f" {
		t.Fatalf("MD5 = %s", got)
	}
}

func TestParseStreamInfoLimits(t *testing.T) {
	for _, want := range []Info{
		{MinBlockSize: 16, MaxBlockSize: 16, Channels: 1, BitsPerSample: 4},
		{MinBlockSize: 16, MaxBlockSize: 65535, MinFrameSize: 1, MaxFrameSize: 1<<24 - 1, SampleRate: 1<<20 - 1, Channels: 8, BitsPerSample: 32, TotalFrames: maxTotalFrames},
		{MinBlockSize: 16, MaxBlockSize: 16, MinFrameSize: 4096, SampleRate: 48000, Channels: 1, BitsPerSample: 24},
	} {
		got, err := ParseStreamInfo(streamInfoBytes(want))
		if err != nil || got != want {
			t.Fatalf("got %+v, %v; want %+v", got, err, want)
		}
	}
}

func TestParseStreamInfoInvalid(t *testing.T) {
	base := Info{MinBlockSize: 16, MaxBlockSize: 4096, SampleRate: 48000, Channels: 2, BitsPerSample: 24}
	valid := streamInfoBytes(base)
	for length := 0; length < 34; length++ {
		if _, err := ParseStreamInfo(valid[:length]); err == nil {
			t.Fatalf("accepted %d-byte block", length)
		}
	}
	if _, err := ParseStreamInfo(append(valid, 0)); err == nil {
		t.Fatal("accepted oversized block")
	}
	for _, tc := range []struct {
		name   string
		change func(*Info)
	}{
		{"small minimum block", func(i *Info) { i.MinBlockSize = 15 }},
		{"small maximum block", func(i *Info) { i.MaxBlockSize = 15 }},
		{"reversed block range", func(i *Info) { i.MinBlockSize = 8192 }},
		{"reversed frame range", func(i *Info) { i.MinFrameSize, i.MaxFrameSize = 100, 99 }},
		{"one-bit precision", func(i *Info) { i.BitsPerSample = 1 }},
		{"three-bit precision", func(i *Info) { i.BitsPerSample = 3 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := base
			tc.change(&info)
			if _, err := ParseStreamInfo(streamInfoBytes(info)); err == nil {
				t.Fatal("accepted invalid metadata")
			}
		})
	}
}

func FuzzParseStreamInfo(f *testing.F) {
	f.Add(streamInfoBytes(Info{MinBlockSize: 16, MaxBlockSize: 4096, SampleRate: 44100, Channels: 2, BitsPerSample: 16}))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		info, err := ParseStreamInfo(data)
		if err != nil {
			return
		}
		if len(data) != 34 || info.MinBlockSize < 16 || info.MinBlockSize > info.MaxBlockSize {
			t.Fatal("accepted invalid metadata")
		}
		if _, err := NewVerifier(info); err != nil {
			t.Fatalf("parsed format cannot be verified: %v", err)
		}
	})
}
