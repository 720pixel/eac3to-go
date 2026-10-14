//go:build !cgo

package ssrc

import "fmt"

func Available() bool { return false }

func newBackend(inRate, outRate, channels int, config Config) (backend, error) {
	return nil, fmt.Errorf("SSRC is unavailable in this build; rebuild with CGO_ENABLED=1 and a C++ compiler")
}
