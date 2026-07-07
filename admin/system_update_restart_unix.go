//go:build unix

package admin

import (
	"fmt"
	"os"
	"syscall"
)

func defaultRestartProcess(exePath string) error {
	if exePath == "" {
		return fmt.Errorf("restart executable path is empty")
	}
	return syscall.Exec(exePath, os.Args, os.Environ())
}
