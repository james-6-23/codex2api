package database

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"strings"
	"time"
)

type PromptManualWindowLock struct {
	Platform     string     `json:"platform"`
	NewAPIUserID string     `json:"newapi_user_id"`
	SessionHash  string     `json:"session_hash"`
	LockedAt     time.Time  `json:"locked_at"`
	ExpiresAt    time.Time  `json:"expires_at"`
	UnlockedAt   *time.Time `json:"unlocked_at,omitempty"`
}

type PromptSessionAccountError struct {
	SessionHash     string
	AccountID       int64
	Last500At       time.Time
	WindowExpiresAt int64
}

func (db *DB) ensurePromptWindowOperationsTables(ctx context.Context) error {
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS prompt_manual_window_locks (
			platform VARCHAR(100) NOT NULL, newapi_user_id VARCHAR(255) NOT NULL,
			session_hash VARCHAR(64) NOT NULL, locked_at TIMESTAMP NOT NULL,
			expires_at TIMESTAMP NOT NULL, unlocked_at TIMESTAMP,
			PRIMARY KEY(platform, newapi_user_id, session_hash)
		)`,
		`CREATE TABLE IF NOT EXISTS prompt_session_account_errors (
			platform VARCHAR(100) NOT NULL, newapi_user_id VARCHAR(255) NOT NULL,
			session_hash VARCHAR(64) NOT NULL, account_id BIGINT NOT NULL,
			window_expires_at BIGINT NOT NULL, last_500_at TIMESTAMP NOT NULL,
			PRIMARY KEY(platform, newapi_user_id, session_hash, account_id, window_expires_at)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_prompt_session_account_errors_expiry ON prompt_session_account_errors(window_expires_at)`,
	} {
		if _, err := db.conn.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func validPromptWindowScope(platform, userID, sessionHash string) bool {
	decoded, err := hex.DecodeString(sessionHash)
	return strings.TrimSpace(platform) != "" && strings.TrimSpace(userID) != "" && err == nil && len(decoded) == 12
}

func scanPromptManualWindowLock(scanner interface{ Scan(...any) error }) (*PromptManualWindowLock, error) {
	item := &PromptManualWindowLock{}
	var lockedRaw, expiresRaw, unlockedRaw any
	if err := scanner.Scan(&item.Platform, &item.NewAPIUserID, &item.SessionHash, &lockedRaw, &expiresRaw, &unlockedRaw); err != nil {
		return nil, err
	}
	var err error
	if item.LockedAt, err = parseDBTimeValue(lockedRaw); err != nil {
		return nil, err
	}
	if item.ExpiresAt, err = parseDBTimeValue(expiresRaw); err != nil {
		return nil, err
	}
	if unlockedRaw != nil {
		unlocked, err := parseDBTimeValue(unlockedRaw)
		if err != nil {
			return nil, err
		}
		item.UnlockedAt = &unlocked
	}
	return item, nil
}

func (db *DB) LockPromptUserWindow(ctx context.Context, platform, userID, sessionHash string, now time.Time, ttl time.Duration) (*PromptManualWindowLock, error) {
	platform, userID, sessionHash = strings.ToLower(strings.TrimSpace(platform)), strings.TrimSpace(userID), strings.ToLower(strings.TrimSpace(sessionHash))
	if !validPromptWindowScope(platform, userID, sessionHash) || ttl <= 0 {
		return nil, errors.New("invalid manual window lock")
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	_, err := db.conn.ExecContext(ctx, `INSERT INTO prompt_manual_window_locks
		(platform, newapi_user_id, session_hash, locked_at, expires_at, unlocked_at)
		VALUES ($1,$2,$3,$4,$5,NULL)
		ON CONFLICT(platform, newapi_user_id, session_hash) DO UPDATE SET
		locked_at=excluded.locked_at, expires_at=excluded.expires_at, unlocked_at=NULL
		WHERE prompt_manual_window_locks.unlocked_at IS NOT NULL OR prompt_manual_window_locks.expires_at<=excluded.locked_at`,
		platform, userID, sessionHash, db.timeArg(now.UTC()), db.timeArg(now.Add(ttl).UTC()))
	if err != nil {
		return nil, err
	}
	return db.GetActivePromptUserWindowLock(ctx, platform, userID, sessionHash, now)
}

func (db *DB) GetActivePromptUserWindowLock(ctx context.Context, platform, userID, sessionHash string, now time.Time) (*PromptManualWindowLock, error) {
	return scanPromptManualWindowLock(db.conn.QueryRowContext(ctx, `SELECT platform, newapi_user_id, session_hash, locked_at, expires_at, unlocked_at
		FROM prompt_manual_window_locks WHERE platform=$1 AND newapi_user_id=$2 AND session_hash=$3 AND unlocked_at IS NULL AND expires_at>$4`,
		strings.ToLower(strings.TrimSpace(platform)), strings.TrimSpace(userID), strings.ToLower(strings.TrimSpace(sessionHash)), db.timeArg(now.UTC())))
}

func (db *DB) ListPromptUserWindowLocks(ctx context.Context, platform, userID string, now time.Time) ([]*PromptManualWindowLock, error) {
	rows, err := db.conn.QueryContext(ctx, `SELECT platform, newapi_user_id, session_hash, locked_at, expires_at, unlocked_at
		FROM prompt_manual_window_locks WHERE platform=$1 AND newapi_user_id=$2 AND unlocked_at IS NULL AND expires_at>$3 ORDER BY locked_at DESC`,
		strings.ToLower(strings.TrimSpace(platform)), strings.TrimSpace(userID), db.timeArg(now.UTC()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]*PromptManualWindowLock, 0)
	for rows.Next() {
		item, err := scanPromptManualWindowLock(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func (db *DB) UnlockPromptUserWindow(ctx context.Context, platform, userID, sessionHash string, now time.Time) error {
	result, err := db.conn.ExecContext(ctx, `UPDATE prompt_manual_window_locks SET unlocked_at=$4
		WHERE platform=$1 AND newapi_user_id=$2 AND session_hash=$3 AND unlocked_at IS NULL`,
		strings.ToLower(strings.TrimSpace(platform)), strings.TrimSpace(userID), strings.ToLower(strings.TrimSpace(sessionHash)), db.timeArg(now.UTC()))
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (db *DB) applyPromptSessionAccountErrorsWithExec(ctx context.Context, execer sqlExecer, batch []usageLogEntry) error {
	type errorKey struct {
		platform, userID, sessionHash string
		accountID                     int64
		windowExpiresAt               int64
	}
	latest := make(map[errorKey]time.Time)
	for _, entry := range batch {
		if entry.StatusCode != 500 || entry.AccountID <= 0 || entry.SessionWindowExpiresAt.IsZero() || !validPromptWindowScope(entry.NewAPIPlatform, entry.NewAPIUserID, entry.SessionHash) {
			continue
		}
		at := entry.ObservedAt
		if at.IsZero() {
			at = time.Now().UTC()
		}
		key := errorKey{strings.ToLower(strings.TrimSpace(entry.NewAPIPlatform)), strings.TrimSpace(entry.NewAPIUserID), entry.SessionHash, entry.AccountID, entry.SessionWindowExpiresAt.UnixNano()}
		if at.After(latest[key]) {
			latest[key] = at
		}
	}
	for key, at := range latest {
		if _, err := execer.ExecContext(ctx, `INSERT INTO prompt_session_account_errors
			(platform, newapi_user_id, session_hash, account_id, window_expires_at, last_500_at) VALUES ($1,$2,$3,$4,$5,$6)
			ON CONFLICT(platform, newapi_user_id, session_hash, account_id, window_expires_at) DO UPDATE SET last_500_at=excluded.last_500_at
			WHERE excluded.last_500_at>prompt_session_account_errors.last_500_at`,
			key.platform, key.userID, key.sessionHash, key.accountID, key.windowExpiresAt, db.preciseTimeArg(at)); err != nil {
			return err
		}
	}
	if len(latest) > 0 {
		_, err := execer.ExecContext(ctx, `DELETE FROM prompt_session_account_errors WHERE window_expires_at<=$1`, time.Now().UTC().UnixNano())
		return err
	}
	return nil
}

func (db *DB) ListPromptSessionAccountErrors(ctx context.Context, platform, userID string, now time.Time) ([]PromptSessionAccountError, error) {
	rows, err := db.conn.QueryContext(ctx, `SELECT session_hash, account_id, window_expires_at, last_500_at FROM prompt_session_account_errors
		WHERE platform=$1 AND newapi_user_id=$2 AND window_expires_at>$3`, strings.ToLower(strings.TrimSpace(platform)), strings.TrimSpace(userID), now.UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]PromptSessionAccountError, 0)
	for rows.Next() {
		var item PromptSessionAccountError
		var raw any
		if err := rows.Scan(&item.SessionHash, &item.AccountID, &item.WindowExpiresAt, &raw); err != nil {
			return nil, err
		}
		item.Last500At, err = parseDBTimeValue(raw)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}
