//go:build cgo

package aften

/*
#cgo CFLAGS: -std=gnu99 -O2 -fno-fast-math -DAFTEN_SAFE_COEFFICIENTS=1 -DAFTEN_LEGACY_TABLE_PRECISION=1 -DHAVE_INTTYPES_H=1 -DAFTEN_STATIC=1 -DMAX_NUM_THREADS=1 -I${SRCDIR}/../../third_party/aften -I${SRCDIR}/../../third_party/aften/libaften
#cgo !windows CFLAGS: -DHAVE_POSIX_MEMALIGN=1
#cgo amd64 CFLAGS: -DHAVE_CPU_CAPS_DETECTION=1 -DHAVE_MMX=1 -DHAVE_SSE=1 -DHAVE_SSE2=1 -DHAVE_SSE3=1
#cgo 386 CFLAGS: -DHAVE_CPU_CAPS_DETECTION=1 -DHAVE_MMX=1 -DHAVE_SSE=1 -DHAVE_SSE2=1 -DHAVE_SSE3=1
#cgo LDFLAGS: -lm
#include "native.h"
*/
import "C"

import (
	"fmt"
	"runtime"
	"sync"
	"unsafe"
)

// Initialization writes nexpgrptab, expstr_set_bits, a52_window, CRC and
// psychoacoustic lookup tables. The frame path only reads those tables;
// its mutable state belongs to the context. Allow simultaneous frame encodes
// while excluding initialization, silence generation and destruction.
var encoderMu sync.RWMutex

func Available() bool { return true }

type nativeEncoder struct {
	ptr      *C.NativeAftenEncoder
	channels int
}

func newNativeEncoder(c encoderConfig) (*nativeEncoder, error) {
	encoderMu.Lock()
	ptr := C.native_aften_create(C.int(c.SampleRate), C.int(c.Channels), C.int(c.acmod), C.int(c.lfe), C.int(c.BitrateKbps), C.int(c.FrameContexts))
	encoderMu.Unlock()
	if ptr == nil {
		return nil, fmt.Errorf("Aften failed to initialize the AC-3 encoder")
	}
	return &nativeEncoder{ptr: ptr, channels: c.Channels}, nil
}

func (e *nativeEncoder) encode(samples []float64, output []byte) (int, error) {
	var input *C.double
	if len(samples) != 0 {
		input = (*C.double)(unsafe.Pointer(&samples[0]))
	}
	encoderMu.RLock()
	n := int(C.native_aften_encode(e.ptr, input, C.int(len(samples)/e.channels), (*C.uint8_t)(unsafe.Pointer(&output[0])), C.size_t(len(output))))
	encoderMu.RUnlock()
	runtime.KeepAlive(samples)
	runtime.KeepAlive(output)
	if n == -2 {
		return 0, ErrCoefficientRange
	}
	if n < 0 || n > len(output) || (len(samples) != 0 && n == 0) {
		return 0, fmt.Errorf("Aften failed to encode an AC-3 frame: %d", n)
	}
	return n, nil
}

func (e *nativeEncoder) close() {
	encoderMu.Lock()
	C.native_aften_destroy(e.ptr)
	encoderMu.Unlock()
	e.ptr = nil
}

func silence(frame []byte) ([]byte, error) {
	if len(frame) < 8 || len(frame) > 3840 {
		return nil, ErrInvalidFrame
	}
	output := make([]byte, 3840)
	encoderMu.Lock()
	n := int(C.native_aften_silence((*C.uint8_t)(unsafe.Pointer(&frame[0])), C.size_t(len(frame)), (*C.uint8_t)(unsafe.Pointer(&output[0])), C.size_t(len(output))))
	encoderMu.Unlock()
	runtime.KeepAlive(frame)
	runtime.KeepAlive(output)
	switch n {
	case -1:
		return nil, ErrInvalidFrame
	case -2:
		return nil, ErrChecksum
	case -3:
		return nil, fmt.Errorf("Aften failed to initialize the silent-frame encoder")
	case -4:
		return nil, fmt.Errorf("Aften could not allocate silent PCM")
	case -5:
		return nil, ErrFrameSize
	case -6:
		return nil, ErrUnsupported
	}
	if n <= 0 || n > len(output) {
		return nil, fmt.Errorf("Aften returned an invalid frame length: %d", n)
	}
	return output[:n], nil
}
