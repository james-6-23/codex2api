package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

// 导入后的邀请引导配置存放在 system_settings.invite_guide_config。和
// antigravity_oauth_config 一样刻意用独立的小 UPDATE 读写，不进 SaveSettings 的
// 巨型 UPSERT——那条语句的占位符已经排到 $119，每加一列都要整体顺移。

// InviteGuideDefaultEnabled 是用户从未做过选择时的默认值：默认弹出引导。
const InviteGuideDefaultEnabled = true

// InviteGuideConfig 控制导入账号后是否弹出邀请积分引导。
//
// Enabled 用指针区分「还没做过选择」与「明确关掉了」。列默认值是 '{}'，
// 用裸 bool 会把「没配过」读成 false，导致默认关闭——与预期相反。
type InviteGuideConfig struct {
	Enabled *bool `json:"enabled,omitempty"`
}

// IsEnabled 解析出实际生效的开关值。
func (c InviteGuideConfig) IsEnabled() bool {
	if c.Enabled == nil {
		return InviteGuideDefaultEnabled
	}
	return *c.Enabled
}

// LoadInviteGuideConfig 读取配置。未配置、空串或 JSON 损坏都退回默认值——
// 引导弹窗是锦上添花的功能，不该因为一行坏数据让设置页打不开。
func (db *DB) LoadInviteGuideConfig(ctx context.Context) (InviteGuideConfig, error) {
	var cfg InviteGuideConfig
	var raw string
	err := db.conn.QueryRowContext(ctx, `
		SELECT COALESCE(invite_guide_config, '{}')
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
		return InviteGuideConfig{}, nil
	}
	return cfg, nil
}

// SaveInviteGuideConfig 持久化配置。
func (db *DB) SaveInviteGuideConfig(ctx context.Context, cfg InviteGuideConfig) error {
	payload, err := json.Marshal(cfg)
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
			SET invite_guide_config = $1
			WHERE id = 1
		`, string(payload))
		return err
	})
}
