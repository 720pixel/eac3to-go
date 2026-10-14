//go:build !cgo

package aften

func Available() bool                { return false }
func silence([]byte) ([]byte, error) { return nil, ErrUnavailable }

type nativeEncoder struct{}

func newNativeEncoder(encoderConfig) (*nativeEncoder, error) { return nil, ErrUnavailable }
func (*nativeEncoder) encode([]float64, []byte) (int, error) { return 0, ErrUnavailable }
func (*nativeEncoder) close()                                {}
