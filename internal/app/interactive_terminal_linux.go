//go:build linux

package app

import (
	"os"
	"syscall"
	"unsafe"
)

func interactiveTerminal(f *os.File) bool {
	var mode syscall.Termios
	_, _, err := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(syscall.TCGETS), uintptr(unsafe.Pointer(&mode)))
	return err == 0
}
