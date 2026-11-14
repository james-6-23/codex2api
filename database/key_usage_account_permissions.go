package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

// KeyUsageAccountPermissions represents the global policy (key 0) or a
// key-specific restriction. Missing key policies inherit the global policy.
type KeyUsageAccountPermissions struct {
	ViewEnabled  bool `json:"view_enabled"`
	ResetEnabled bool `json:"reset_enabled"`
}

// KeyUsageAccountAccess is a credential-free projection for portal authorization.
type KeyUsageAccountAccess struct {
	ID               int64
	Name             string
	Email            string
	UpstreamType     string
	PlanType         string
	AllowedAPIKeyIDs []int64
}

func (db *DB) ListKeyUsageAccountAccess(ctx context.Context) (map[int64]KeyUsageAccountAccess, error) {
	query := `SELECT id, name, COALESCE(credentials->>'email', ''), COALESCE(credentials->>'upstream_type', ''), COALESCE(credentials->>'plan_type', ''), COALESCE(credentials->'allowed_api_key_ids', '[]'::jsonb)::text FROM accounts WHERE status <> 'deleted' AND COALESCE(error_message, '') <> 'deleted'`
	if db.isSQLite() {
		query = `SELECT id, name, COALESCE(json_extract(credentials, '$.email'), ''), COALESCE(json_extract(credentials, '$.upstream_type'), ''), COALESCE(json_extract(credentials, '$.plan_type'), ''), COALESCE(json_extract(credentials, '$.allowed_api_key_ids'), '[]') FROM accounts WHERE status <> 'deleted' AND COALESCE(error_message, '') <> 'deleted'`
	}
	rows, err := db.conn.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[int64]KeyUsageAccountAccess)
	for rows.Next() {
		var item KeyUsageAccountAccess
		var allowed string
		if err := rows.Scan(&item.ID, &item.Name, &item.Email, &item.UpstreamType, &item.PlanType, &allowed); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(allowed), &item.AllowedAPIKeyIDs); err != nil {
			return nil, err
		}
		result[item.ID] = item
	}
	return result, rows.Err()
}

func (db *DB) GetKeyUsageAccountPermissions(ctx context.Context, keyID int64) (KeyUsageAccountPermissions, error) {
	var p KeyUsageAccountPermissions
	err := db.conn.QueryRowContext(ctx, `SELECT view_enabled, reset_enabled FROM key_usage_account_permissions WHERE key_id = $1`, keyID).Scan(&p.ViewEnabled, &p.ResetEnabled)
	if errors.Is(err, sql.ErrNoRows) {
		if keyID > 0 {
			return KeyUsageAccountPermissions{ViewEnabled: true, ResetEnabled: true}, nil
		}
		return p, nil
	}
	return p, err
}

func (db *DB) SetKeyUsageAccountPermissions(ctx context.Context, keyID int64, p KeyUsageAccountPermissions) error {
	_, err := db.conn.ExecContext(ctx, `INSERT INTO key_usage_account_permissions (key_id, view_enabled, reset_enabled) VALUES ($1, $2, $3) ON CONFLICT(key_id) DO UPDATE SET view_enabled = excluded.view_enabled, reset_enabled = excluded.reset_enabled`, keyID, p.ViewEnabled, p.ResetEnabled)
	return err
}

// ReserveKeyUsageReset prevents retries of the same irreversible request from
// reaching the upstream twice, including after a process restart.
func (db *DB) ReserveKeyUsageReset(ctx context.Context, requestID string, keyID, accountID int64) (bool, error) {
	result, err := db.conn.ExecContext(ctx, `INSERT INTO key_usage_reset_attempts (request_id, key_id, account_id) VALUES ($1, $2, $3) ON CONFLICT(request_id) DO NOTHING`, requestID, keyID, accountID)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

// AcquireKeyUsageResetLease coordinates all manual and automatic redemptions
// even when replicas use independent in-memory caches.
func (db *DB) AcquireKeyUsageResetLease(ctx context.Context, scope, owner string, now, expires int64) (bool, error) {
	r, err := db.conn.ExecContext(ctx, `INSERT INTO key_usage_reset_leases (scope, owner, expires_at) VALUES ($1, $2, $3) ON CONFLICT(scope) DO UPDATE SET owner=excluded.owner, expires_at=excluded.expires_at WHERE key_usage_reset_leases.expires_at <= $4`, scope, owner, expires, now)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n == 1, err
}

func (db *DB) ReleaseKeyUsageResetLease(ctx context.Context, scope, owner string) error {
	_, err := db.conn.ExecContext(ctx, `DELETE FROM key_usage_reset_leases WHERE scope=$1 AND owner=$2`, scope, owner)
	return err
}

// ReserveKeyUsageResetCooldown is recorded before sending the upstream request:
// a crash or lost response therefore cannot immediately trigger another debit.
func (db *DB) ReserveKeyUsageResetCooldown(ctx context.Context, scope string, now, expires int64) (bool, error) {
	r, err := db.conn.ExecContext(ctx, `INSERT INTO key_usage_reset_cooldowns (scope, expires_at) VALUES ($1, $2) ON CONFLICT(scope) DO UPDATE SET expires_at=excluded.expires_at WHERE key_usage_reset_cooldowns.expires_at <= $3`, scope, expires, now)
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n == 1, err
}

// RecordKeyUsageResetCooldown fences subsequent public/automatic resets after
// a manual redemption, even when instances use separate memory caches.
func (db *DB) RecordKeyUsageResetCooldown(ctx context.Context, scope string, expires int64) error {
	_, err := db.conn.ExecContext(ctx, `INSERT INTO key_usage_reset_cooldowns (scope, expires_at) VALUES ($1, $2) ON CONFLICT(scope) DO UPDATE SET expires_at = CASE WHEN key_usage_reset_cooldowns.expires_at > excluded.expires_at THEN key_usage_reset_cooldowns.expires_at ELSE excluded.expires_at END`, scope, expires)
	return err
}
