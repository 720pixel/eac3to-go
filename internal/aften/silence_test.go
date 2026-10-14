//go:build cgo

package aften

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"testing"
)

// Input fixtures are first frames of generated 997Hz test tones. These hashes
// were checked against inserted silent frames from the historical encoder.
func TestSilentFrameReferenceMatrix(t *testing.T) {
	cases := []struct{ name, hash string }{
		{"mono48_96", "419752c6d2983849c5c0a7664829a31c6ed7203677f3e97805c8968f48971f97"},
		{"stereo48_192", "325c135d5999e011d0efad8f06857eca09aee1aecd11b14fdd47bb182b5ef837"},
		{"surround48_384", "742d597dda3a327a0d396f3e1d4f163f6b1c63c6bf25dbdc26d423adc79698fe"},
		{"surround48_640xbsi", "dd352e65ee9fc15a4fc1d0bee8560d4edddc95033c9d6a833de53b5bcfa49826"},
		{"mono32_64", "7b3107e4c575d44c665f3c35cadeb463b5638b5e7627c5a6ba6deedb73b0dbbb"},
		{"stereo32_192", "f423e26166e3bdfe86c910675ed6637bdd23e92896a8871bcd7b5b76a580e8a7"},
		{"surround32_384", "23eff783c952a178913cf4edd7bf7f367c0a02c91aa350503be557e83b99f862"},
		{"mono44_64", "53ea6543d4d9bb838677fb4a1243b4aa9341324cf4cc1f452cf367861f158bc0"},
		{"stereo44_160", "bbe446d63db6d3939913b844f615c4697a1dfb4fff3da559903a0ce9cc04a215"},
		{"surround44_640", "d10dcb1eb737279ba02839e3b1703056340aa6d943de7823386333a7de7a76d8"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			input, err := os.ReadFile("testdata/" + c.name + ".ac3")
			if err != nil {
				t.Fatal(err)
			}
			original := bytes.Clone(input)
			output, err := SilenceFromAC3Frame(input)
			if err != nil {
				t.Fatal(err)
			}
			if got := fmt.Sprintf("%x", sha256.Sum256(output)); got != c.hash {
				t.Fatalf("silent frame SHA256 = %s, want %s", got, c.hash)
			}
			if !bytes.Equal(input, original) {
				t.Fatal("modified source frame")
			}
		})
	}
}

func TestOddExpectedFrameSize(t *testing.T) {
	input, err := os.ReadFile("testdata/stereo44_192odd.ac3")
	if err != nil {
		t.Fatal(err)
	}
	output, err := SilenceFromAC3Frame(input)
	if !errors.Is(err, ErrFrameSize) || output != nil {
		t.Fatalf("output %d bytes, error %v", len(output), err)
	}
}

func TestRejectInvalidSourceFrame(t *testing.T) {
	source, err := os.ReadFile("testdata/surround48_640xbsi.ac3")
	if err != nil {
		t.Fatal(err)
	}
	for _, bsid := range []byte{9, 10, 16} {
		input := bytes.Clone(source)
		input[5] = bsid << 3
		if _, err := SilenceFromAC3Frame(input); !errors.Is(err, ErrUnsupported) {
			t.Fatalf("bsid %d: got %v", bsid, err)
		}
	}
	for _, n := range []int{0, 1, 7, 8, len(source) - 1} {
		t.Run(fmt.Sprintf("short_%d", n), func(t *testing.T) {
			if _, err := SilenceFromAC3Frame(source[:n]); !errors.Is(err, ErrInvalidFrame) {
				t.Fatalf("got %v", err)
			}
		})
	}
	for _, offset := range []int{2, 40, len(source) - 1} {
		t.Run(fmt.Sprintf("checksum_%d", offset), func(t *testing.T) {
			input := bytes.Clone(source)
			input[offset] ^= 1
			if _, err := SilenceFromAC3Frame(input); !errors.Is(err, ErrChecksum) {
				t.Fatalf("got %v", err)
			}
		})
	}
	for _, offset := range []int{0, 4, 5} {
		t.Run(fmt.Sprintf("header_%d", offset), func(t *testing.T) {
			input := bytes.Clone(source)
			input[offset] = 0xff
			if _, err := SilenceFromAC3Frame(input); !errors.Is(err, ErrInvalidFrame) {
				t.Fatalf("got %v", err)
			}
		})
	}
}
