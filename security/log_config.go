package security

import (
	"os"
	"strings"
)

// FileLogsDisabled reports whether file-backed logs should be skipped.
func FileLogsDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("LOG_DISABLED"))) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}
