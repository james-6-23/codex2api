package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

// 仪表盘与账号管理里显示哪些上游渠道，存放在 system_settings.visible_channels_config。
// 和 invite_guide_config 一样刻意用独立的小 UPDATE 读写，不进 SaveSettings 的
// 巨型 UPSERT——那条语句的占位符已经排到 $119，每加一列都要整体顺移。

// AllUpstreamChannels 是管理台可见性开关能控制的全部渠道，顺序即展示顺序。
var AllUpstreamChannels = []string{"codex", "claude", "antigravity", "grok"}

// FallbackVisibleChannel 是兜底渠道：无论怎么配置都保持显示，避免把管理台关成空白。
const FallbackVisibleChannel = "codex"

// VisibleChannelsConfig 记录用户勾选的可见渠道。
//
// Channels 为 nil 表示「从未配置」，按全部显示处理；显式空列表则只剩兜底渠道。
// 这里不能加 omitempty：否则保存空列表会被序列化成 '{}'，再读回来又变成全部显示。
type VisibleChannelsConfig struct {
	Channels []string `json:"channels"`
}

// Effective 解析出实际生效的可见渠道列表。
func (c VisibleChannelsConfig) Effective() []string {
	return NormalizeVisibleChannels(c.Channels)
}

// NormalizeVisibleChannels 把任意输入收敛成合法列表：nil 视为全部；去掉未知渠道与重复项；
// 兜底渠道始终在列；输出按 AllUpstreamChannels 的固定顺序排列。
func NormalizeVisibleChannels(channels []string) []string {
	if channels == nil {
		return append([]string(nil), AllUpstreamChannels...)
	}
	wanted := map[string]bool{FallbackVisibleChannel: true}
	for _, raw := range channels {
		name := strings.ToLower(strings.TrimSpace(raw))
		if name == "" {
			continue
		}
		wanted[name] = true
	}
	out := make([]string, 0, len(AllUpstreamChannels))
	for _, name := range AllUpstreamChannels {
		if wanted[name] {
			out = append(out, name)
		}
	}
	return out
}

// LoadVisibleChannelsConfig 读取配置。未配置、空串或 JSON 损坏都退回「全部显示」——
// 可见性只是展示偏好，不该因为一行坏数据让管理台缺渠道。
func (db *DB) LoadVisibleChannelsConfig(ctx context.Context) (VisibleChannelsConfig, error) {
	var cfg VisibleChannelsConfig
	var raw string
	err := db.conn.QueryRowContext(ctx, `
		SELECT COALESCE(visible_channels_config, '{}')
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
		return VisibleChannelsConfig{}, nil
	}
	return cfg, nil
}

// SaveVisibleChannelsConfig 持久化配置，落库前先规范化，保证兜底渠道永远在列。
func (db *DB) SaveVisibleChannelsConfig(ctx context.Context, cfg VisibleChannelsConfig) error {
	payload, err := json.Marshal(VisibleChannelsConfig{Channels: NormalizeVisibleChannels(cfg.Channels)})
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
			SET visible_channels_config = $1
			WHERE id = 1
		`, string(payload))
		return err
	})
}
