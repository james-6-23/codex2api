package proxy

import (
	"testing"

	"github.com/codex2api/database"
)

func TestApplyUsageCacheWritesToLog(t *testing.T) {
	usage := &UsageInfo{CacheWriteTokens: 4081, CacheWrite5mTokens: 0, CacheWrite1hTokens: 4081}
	var log database.UsageLogInput
	applyUsageCacheWritesToLog(&log, usage)
	if log.CacheWrite5mTokens != 0 || log.CacheWrite1hTokens != 4081 {
		t.Fatalf("log cache writes = 5m %d / 1h %d", log.CacheWrite5mTokens, log.CacheWrite1hTokens)
	}
	applyUsageCacheWritesToLog(&log, &UsageInfo{CacheWriteTokens: 500})
	if log.CacheWrite5mTokens != 500 || log.CacheWrite1hTokens != 0 {
		t.Fatalf("total-only write must map to 5m: %d / %d", log.CacheWrite5mTokens, log.CacheWrite1hTokens)
	}
	applyUsageCacheWritesToLog(&log, nil)
	applyUsageCacheWritesToLog(nil, usage)
}
