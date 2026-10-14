//go:build windows

package app

import (
	"os"
	"syscall"
	"unsafe"
)

func interactiveTerminal(f *os.File) bool {
	var mode uint32
	result, _, _ := syscall.NewLazyDLL("kernel32.dll").NewProc("GetConsoleMode").Call(f.Fd(), uintptr(unsafe.Pointer(&mode)))
	return result != 0
}
