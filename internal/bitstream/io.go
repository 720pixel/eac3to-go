// Package bitstream provides streaming, lossless metadata edits for elementary
// media streams. Callers should write to temporary files and publish them only
// after success: a late input or output error can leave a partial result.
package bitstream

import (
	"errors"
	"io"
)

// ErrEmptyInput means the input did not contain a single complete packet/frame.
var ErrEmptyInput = errors.New("empty bitstream")

func writePacket(w io.Writer, p []byte) (int, error) {
	n, err := w.Write(p)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	return n, err
}
