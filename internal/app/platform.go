package app

import (
	"os"
	"runtime"
)

func executableName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

func executable(path string) bool {
	f, err := os.Stat(path)
	return err == nil && f.Mode().IsRegular() && (runtime.GOOS == "windows" || f.Mode().Perm()&0111 != 0)
}
