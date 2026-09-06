package auth

import (
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/internal/timezone"
)

func buildProxyTimezones(rows []*database.ProxyRow) map[string]*time.Location {
	locations := make(map[string]*time.Location)
	for _, row := range rows {
		if row == nil || strings.TrimSpace(row.URL) == "" {
			continue
		}
		name := strings.TrimSpace(row.TimezoneOverride)
		if name == "" {
			name = row.TestTimezone
		}
		if location, err := timezone.Load(name); err == nil {
			locations[strings.TrimSpace(row.URL)] = location
		}
	}
	return locations
}

func (store *Store) ProxyTimezone(proxyURL string) *time.Location {
	if store == nil || strings.TrimSpace(proxyURL) == "" {
		return nil
	}
	store.mu.RLock()
	defer store.mu.RUnlock()
	return store.proxyTimezones[strings.TrimSpace(proxyURL)]
}
