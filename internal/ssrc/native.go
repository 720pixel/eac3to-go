//go:build cgo

package ssrc

/*
#cgo CXXFLAGS: -std=c++11 -O2 -fno-fast-math
#cgo LDFLAGS: -lstdc++ -lm
#include "native.h"
*/
import "C"

import (
	"fmt"
	"runtime"
	"unsafe"
)

func Available() bool { return true }

type nativeBackend struct{ handle C.ssrc_handle }

func newBackend(inRate, outRate, channels int, config Config) (backend, error) {
	handle := C.native_ssrc_new(C.int(inRate), C.int(outRate), C.int(channels), C.double(config.Attenuation), C.double(config.TransitionBand), C.int(config.FFTLength))
	if handle == nil {
		return nil, fmt.Errorf("SSRC cannot convert %d Hz to %d Hz with this configuration", inRate, outRate)
	}
	return &nativeBackend{handle: handle}, nil
}

func (n *nativeBackend) write(input []float64) error {
	if len(input) == 0 {
		return nil
	}
	result := C.native_ssrc_write(n.handle, (*C.double)(unsafe.Pointer(&input[0])), C.uint(len(input)*8))
	runtime.KeepAlive(input)
	if result == 0 {
		return fmt.Errorf("SSRC failed to process input")
	}
	return nil
}

func (n *nativeBackend) read() ([]float64, error) {
	bytes := C.native_ssrc_size(n.handle)
	if bytes == 0 {
		return nil, nil
	}
	if bytes%8 != 0 {
		return nil, fmt.Errorf("SSRC returned a partial floating-point sample")
	}
	output := make([]float64, int(bytes)/8)
	if C.native_ssrc_read(n.handle, (*C.double)(unsafe.Pointer(&output[0])), bytes) == 0 {
		return nil, fmt.Errorf("SSRC failed to return output")
	}
	runtime.KeepAlive(output)
	return output, nil
}

func (n *nativeBackend) finish() error {
	if C.native_ssrc_finish(n.handle) == 0 {
		return fmt.Errorf("SSRC failed to flush its filter")
	}
	return nil
}

func (n *nativeBackend) drainInputFrames() uint64 {
	return uint64(C.native_ssrc_drain_input_frames(n.handle))
}

func (n *nativeBackend) close() {
	C.native_ssrc_close(n.handle)
	n.handle = nil
}
