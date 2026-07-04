//go:build !windows

package admin

import (
	"os"
	"syscall"
)

func defaultRestartProcess() error {
	exePath, err := os.Executable()
	if err != nil {
		return err
	}
	return syscall.Exec(exePath, os.Args, os.Environ())
}
