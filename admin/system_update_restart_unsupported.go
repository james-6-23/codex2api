//go:build !unix && !windows

package admin

import "fmt"

func defaultRestartProcess(string) error {
	return fmt.Errorf("online restart is not supported on this platform")
}
