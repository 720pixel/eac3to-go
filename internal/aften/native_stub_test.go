//go:build !cgo

package aften

import (
	"errors"
	"io"
	"testing"
)

func TestUnavailable(t *testing.T) {
	if Available() {
		t.Fatal("unexpected Cgo backend")
	}
	if _, err := SilenceFromAC3Frame(nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
	if _, err := NewEncoder(io.Discard, Config{SampleRate: 48000, Channels: 2}); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("got %v", err)
	}
}
