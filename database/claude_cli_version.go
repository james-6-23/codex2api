package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// GetClaudeSyncedCLIVersion 读取后台同步到的 Claude Code CLI 版本（空=尚未同步）。
func (db *DB) GetClaudeSyncedCLIVersion(ctx context.Context) (string, error) {
	if db == nil || db.conn == nil {
		return "", errors.New("database unavailable")
	}
	var version string
	err := db.conn.QueryRowContext(ctx, `SELECT COALESCE(claude_synced_cli_version, '') FROM system_settings WHERE id = 1`).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(version), nil
}

// UpdateClaudeSyncedCLIVersion 只更新同步版本单列，不回写整行设置。
func (db *DB) UpdateClaudeSyncedCLIVersion(ctx context.Context, version string) error {
	if db == nil || db.conn == nil {
		return errors.New("database unavailable")
	}
	return db.withSQLiteWriteLock(ctx, func() error {
		_, err := db.conn.ExecContext(ctx, `
			INSERT INTO system_settings (id, claude_synced_cli_version) VALUES (1, $1)
			ON CONFLICT (id) DO UPDATE SET claude_synced_cli_version = EXCLUDED.claude_synced_cli_version`,
			strings.TrimSpace(version))
		return err
	})
}

// UpdateAccountCustomHeaders 整体替换账号 credentials.custom_headers，其余凭据字段不动，
// 不递增 credential_generation（指纹版本变化不是身份变化）。
func (db *DB) UpdateAccountCustomHeaders(ctx context.Context, id int64, headers map[string]string) error {
	if db == nil || db.conn == nil {
		return errors.New("database unavailable")
	}
	if id <= 0 {
		return fmt.Errorf("invalid account id %d", id)
	}
	normalized := make(map[string]interface{}, len(headers))
	for key, value := range headers {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		normalized[key] = strings.TrimSpace(value)
	}
	return db.withSQLiteWriteLock(ctx, func() error {
		tx, err := db.conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		query := `SELECT credentials FROM accounts WHERE id = $1 AND status <> 'deleted' AND COALESCE(error_message, '') <> 'deleted'`
		if !db.isSQLite() {
			query += ` FOR UPDATE`
		}
		var raw interface{}
		if err := tx.QueryRowContext(ctx, query, id).Scan(&raw); err != nil {
			return err
		}
		merged := mergeCredentialMaps(cloneCredentialUpdates(decodeCredentials(raw)), map[string]interface{}{"custom_headers": normalized})
		credJSON, err := json.Marshal(encryptSensitiveCredentials(merged))
		if err != nil {
			return fmt.Errorf("序列化 credentials 失败: %w", err)
		}
		update := `UPDATE accounts SET credentials = $1, updated_at = CURRENT_TIMESTAMP WHERE id = $2`
		if !db.isSQLite() {
			update = `UPDATE accounts SET credentials = $1::jsonb, updated_at = CURRENT_TIMESTAMP WHERE id = $2`
		}
		if _, err := tx.ExecContext(ctx, update, credJSON, id); err != nil {
			return err
		}
		return tx.Commit()
	})
}
