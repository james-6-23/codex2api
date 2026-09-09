package database

import (
	"strings"

	"github.com/codex2api/internal/timezone"
)

func normalizeProxyTimezoneOverride(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", nil
	}
	location, err := timezone.Load(name)
	if err != nil {
		return "", err
	}
	return location.String(), nil
}

func normalizeProxyTestTimezone(name string) string {
	return timezone.Normalize(name)
}
