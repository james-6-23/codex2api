package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

// Claude / Antigravity 渠道各自的连通性测试配置（测试模型、测活内容），存放在
// system_settings.channel_test_config。全局 test_model / test_content 是 Codex 语义
// 的默认值，其它渠道的模型目录完全不同，需要各自的默认探测模型与提示词。
// 和 visible_channels_config 一样刻意用独立的小 UPDATE 读写，不进 SaveSettings 的
// 巨型 UPSERT。

// ChannelTestSettings 是单个渠道的连通性测试配置。空字段表示沿用默认：
// 模型留空由服务端按账号目录自动选择，内容留空沿用全局测活内容。
type ChannelTestSettings struct {
	TestModel   string `json:"test_model"`
	TestContent string `json:"test_content"`
	// TestConcurrency 是该渠道批量测试的并发数；0 表示沿用全局 test_concurrency。
	TestConcurrency int `json:"test_concurrency"`
}

// MaxChannelTestConcurrency 与全局测试并发的上限保持一致。
const MaxChannelTestConcurrency = 200

// ChannelTestConfig 汇总有独立测试配置的渠道。
type ChannelTestConfig struct {
	Antigravity ChannelTestSettings `json:"antigravity"`
	Claude      ChannelTestSettings `json:"claude"`
}

// Normalized 去掉首尾空白，保证落库与回显的一致性。
func (c ChannelTestConfig) Normalized() ChannelTestConfig {
	return ChannelTestConfig{
		Antigravity: c.Antigravity.normalized(),
		Claude:      c.Claude.normalized(),
	}
}

func (s ChannelTestSettings) normalized() ChannelTestSettings {
	concurrency := s.TestConcurrency
	if concurrency < 0 {
		concurrency = 0
	}
	if concurrency > MaxChannelTestConcurrency {
		concurrency = MaxChannelTestConcurrency
	}
	return ChannelTestSettings{
		TestModel:       strings.TrimSpace(s.TestModel),
		TestContent:     strings.TrimSpace(s.TestContent),
		TestConcurrency: concurrency,
	}
}

// LoadChannelTestConfig 读取配置。未配置、空串或 JSON 损坏都退回零值——
// 测试配置只是探测偏好，坏数据不该让测连不可用。
func (db *DB) LoadChannelTestConfig(ctx context.Context) (ChannelTestConfig, error) {
	var cfg ChannelTestConfig
	var raw string
	err := db.conn.QueryRowContext(ctx, `
		SELECT COALESCE(channel_test_config, '{}')
		FROM system_settings
		WHERE id = 1
	`).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return cfg, nil
	}
	if err != nil {
		return cfg, err
	}
	if strings.TrimSpace(raw) == "" {
		return cfg, nil
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return ChannelTestConfig{}, nil
	}
	return cfg.Normalized(), nil
}

// SaveChannelTestConfig 持久化配置，落库前先规范化。
func (db *DB) SaveChannelTestConfig(ctx context.Context, cfg ChannelTestConfig) error {
	payload, err := json.Marshal(cfg.Normalized())
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
			SET channel_test_config = $1
			WHERE id = 1
		`, string(payload))
		return err
	})
}
