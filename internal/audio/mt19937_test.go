package audio

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"testing"
)

func TestMT19937ArraySeedVector(t *testing.T) {
	// Standard init_by_array demonstration seed and published first outputs.
	mt := newMT19937([]uint32{0x123, 0x234, 0x345, 0x456})
	want := []uint32{1067595299, 955945823, 477289528, 4107218783, 4228976476,
		3344332714, 3355579695, 227628506, 810200273, 2591290167,
		2560260675, 3242736208, 646746669, 1479517882, 4245472273,
		1143372638, 3863670494, 3221021970, 1773610557, 1138697238}
	for i, expected := range want {
		if got := mt.next(); got != expected {
			t.Fatalf("draw %d = %d, want %d", i, got, expected)
		}
	}
}

func TestMT19937MultipleTwists(t *testing.T) {
	// Cross-checked with CPython's independent MT implementation: its integer
	// seed is split into the same little-endian 32-bit init_by_array words.
	// https://github.com/python/cpython/blob/main/Modules/_randommodule.c
	cases := []struct {
		key  []uint32
		hash string
	}{
		{[]uint32{0x123, 0x234, 0x345, 0x456}, "fd86d45daa8f24485ef2f12718f75085b52c1096d3d7a4e9b023933991201f66"},
		{[]uint32{0}, "9e48e8e3f0c4f1a71e4c952fedac97345faf2c1c892512c7551cdd0803db0501"},
		{[]uint32{0xffffffff}, "5df6a659f2907d4dbcd8700849bae35419aafc3bab4050000e2e28523c923335"},
	}
	for _, tc := range cases {
		mt := newMT19937(tc.key)
		digest := sha256.New()
		var word [4]byte
		for i := 0; i < 10000; i++ {
			binary.LittleEndian.PutUint32(word[:], mt.next())
			digest.Write(word[:])
		}
		if got := hex.EncodeToString(digest.Sum(nil)); got != tc.hash {
			t.Fatalf("seed %v: got digest %s, want %s", tc.key, got, tc.hash)
		}
	}
}
