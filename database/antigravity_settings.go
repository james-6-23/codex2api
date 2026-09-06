package database

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// Antigravity 渠道级设置（模型重定向等）存放在 system_settings.antigravity_config，
// JSON 由 auth 层解析/校验。和 antigravity_oauth_config 一样用独立的小 UPDATE 读写。

// LoadAntigravityConfig 返回 antigravity_config 原始 JSON，未配置时为 "{}"。
func (db *DB) LoadAntigravityConfig(ctx context.Context) (string, error) {
	var raw string
	err := db.conn.QueryRowContext(ctx, `
		SELECT COALESCE(antigravity_config, '{}')
		FROM system_settings
		WHERE id = 1
	`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return "{}", nil
	}
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(raw) == "" {
		return "{}", nil
	}
	return raw, nil
}

// SaveAntigravityConfig 持久化 antigravity_config JSON（调用方保证已归一化）。
func (db *DB) SaveAntigravityConfig(ctx context.Context, raw string) error {
	if strings.TrimSpace(raw) == "" {
		raw = "{}"
	}
	return db.withSQLiteWriteLock(ctx, func() error {
		if _, err := db.conn.ExecContext(ctx, `
			INSERT INTO system_settings (id) VALUES (1)
			ON CONFLICT (id) DO NOTHING
		`); err != nil {
			return err
		}
		_, err := db.conn.ExecContext(ctx, `
			UPDATE system_settings
			SET antigravity_config = $1
			WHERE id = 1
		`, raw)
		return err
	})
}
