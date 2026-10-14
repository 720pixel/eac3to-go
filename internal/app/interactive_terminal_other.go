//go:build !linux && !windows

package app

import "os"

func interactiveTerminal(*os.File) bool { return false }
