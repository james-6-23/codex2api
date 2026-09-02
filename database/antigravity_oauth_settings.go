package database

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// Antigravity OAuth client 配置存放在 system_settings.antigravity_oauth_config,
// JSON 由 auth 层解析/校验。这里刻意用独立的小 UPDATE 读写,不进 SaveSettings 的
// 巨型 UPSERT:该列由专用管理入口维护,与整行设置保存互不覆盖。

// LoadAntigravityOAuthConfig 返回 antigravity_oauth_config 原始 JSON,未配置时为 "{}"。
func (db *DB) LoadAntigravityOAuthConfig(ctx context.Context) (string, error) {
	var raw string
	err := db.conn.QueryRowContext(ctx, `
		SELECT COALESCE(antigravity_oauth_config, '{}')
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

// SaveAntigravityOAuthConfig 持久化 antigravity_oauth_config JSON(调用方保证已归一化)。
func (db *DB) SaveAntigravityOAuthConfig(ctx context.Context, raw string) error {
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
			SET antigravity_oauth_config = $1
			WHERE id = 1
		`, raw)
		return err
	})
}
