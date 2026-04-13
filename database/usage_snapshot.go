package database

import (
	"context"
	"time"
)

// UpdateUsageSnapshot5h 持久化 5h 用量快照（无 7d 数据时使用）
func (db *DB) UpdateUsageSnapshot5h(ctx context.Context, id int64, pct5h float64, reset5hAt time.Time, updatedAt time.Time) error {
	return db.UpdateCredentials(ctx, id, map[string]interface{}{
		"codex_5h_used_percent":  pct5h,
		"codex_5h_reset_at":      reset5hAt.Format(time.RFC3339),
		"codex_usage_updated_at": updatedAt.Format(time.RFC3339),
	})
}
