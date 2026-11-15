package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

// Kiro 渠道设置存放在 system_settings.kiro_config，和 visible_channels_config 一样用独立的小 UPDATE 读写。

// KiroSettings 是 Kiro 渠道的全局设置。
type KiroSettings struct {
	// APIDomain 选择上游域名："amazonaws"（q.{region}.amazonaws.com，默认）或 "kiro"（*.kiro.dev）。
	APIDomain string `json:"api_domain"`
	// CacheSimulation 开启后，上游没给精确用量时按 cache_control 断点本地估算 cache 读写拆分。
	CacheSimulation bool `json:"cache_simulation"`
}

// Normalized 把未知域名收敛为默认值。
func (s KiroSettings) Normalized() KiroSettings {
	if strings.ToLower(strings.TrimSpace(s.APIDomain)) == "kiro" {
		s.APIDomain = "kiro"
	} else {
		s.APIDomain = "amazonaws"
	}
	return s
}

// LoadKiroSettings 读取设置。未配置或 JSON 损坏都退回默认值。
func (db *DB) LoadKiroSettings(ctx context.Context) (KiroSettings, error) {
	var raw string
	err := db.conn.QueryRowContext(ctx, `
		SELECT COALESCE(kiro_config, '{}')
		FROM system_settings
		WHERE id = 1
	`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return KiroSettings{}.Normalized(), nil
	}
	if err != nil {
		return KiroSettings{}.Normalized(), err
	}
	var settings KiroSettings
	_ = json.Unmarshal([]byte(raw), &settings)
	return settings.Normalized(), nil
}

// SaveKiroSettings 规范化后持久化。
func (db *DB) SaveKiroSettings(ctx context.Context, settings KiroSettings) error {
	payload, err := json.Marshal(settings.Normalized())
	if err != nil {
		return err
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
			SET kiro_config = $1
			WHERE id = 1
		`, string(payload))
		return err
	})
}
