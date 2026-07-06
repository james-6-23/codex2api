//go:build windows

package admin

import "fmt"

func defaultRestartProcess() error {
	return fmt.Errorf("online restart is not supported on Windows")
}
