package database

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	PromptSessionLimitModeCustom = "custom"
	PromptSessionLimitModeOff    = "off"
)

// PromptSessionLimitOverride is a persisted per-person override for the
// global Prompt session creation limit. Absence means inherit the global
// policy; off explicitly exempts this verified person; custom supplies an
// independent rolling-window limit.
type PromptSessionLimitOverride struct {
	Platform      string    `json:"platform"`
	NewAPIUserID  string    `json:"newapi_user_id"`
	Mode          string    `json:"mode"`
	Limit         int       `json:"limit"`
	WindowSeconds int       `json:"window_seconds"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

var promptSessionLimitOverrideSchemaMu sync.Mutex

var ErrInvalidPromptSessionLimitOverride = errors.New("invalid session limit override")

func normalizePromptSessionLimitOverride(item PromptSessionLimitOverride) (PromptSessionLimitOverride, error) {
	item.Platform = strings.ToLower(strings.TrimSpace(item.Platform))
	item.NewAPIUserID = strings.TrimSpace(item.NewAPIUserID)
	item.Mode = strings.ToLower(strings.TrimSpace(item.Mode))
	if item.Platform == "" || item.NewAPIUserID == "" {
		return item, fmt.Errorf("%w: NewAPI platform and user id are required", ErrInvalidPromptSessionLimitOverride)
	}
	switch item.Mode {
	case PromptSessionLimitModeOff:
		item.Limit = 0
		item.WindowSeconds = 0
	case PromptSessionLimitModeCustom:
		if item.Limit < 1 || item.Limit > 100000 {
			return item, fmt.Errorf("%w: session limit must be between 1 and 100000", ErrInvalidPromptSessionLimitOverride)
		}
		if item.WindowSeconds < 60 || item.WindowSeconds > 2592000 {
			return item, fmt.Errorf("%w: session limit window must be between 60 and 2592000 seconds", ErrInvalidPromptSessionLimitOverride)
		}
	default:
		return item, fmt.Errorf("%w: session limit override mode must be custom or off", ErrInvalidPromptSessionLimitOverride)
	}
	return item, nil
}

func (db *DB) ensurePromptSessionLimitOverridesTable(ctx context.Context) error {
	if db == nil {
		return errors.New("database unavailable")
	}
	promptSessionLimitOverrideSchemaMu.Lock()
	defer promptSessionLimitOverrideSchemaMu.Unlock()
	ddl := `CREATE TABLE IF NOT EXISTS prompt_session_limit_overrides (
		platform VARCHAR(64) NOT NULL,
		newapi_user_id VARCHAR(255) NOT NULL,
		mode VARCHAR(16) NOT NULL,
		limit_count INT NOT NULL DEFAULT 0,
		window_seconds INT NOT NULL DEFAULT 0,
		created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
		PRIMARY KEY(platform, newapi_user_id)
	)`
	if db.isSQLite() {
		ddl = `CREATE TABLE IF NOT EXISTS prompt_session_limit_overrides (
			platform TEXT NOT NULL,
			newapi_user_id TEXT NOT NULL,
			mode TEXT NOT NULL,
			limit_count INTEGER NOT NULL DEFAULT 0,
			window_seconds INTEGER NOT NULL DEFAULT 0,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY(platform, newapi_user_id)
		)`
	}
	_, err := db.conn.ExecContext(ctx, ddl)
	return err
}

func scanPromptSessionLimitOverride(scanner interface{ Scan(...any) error }) (*PromptSessionLimitOverride, error) {
	item := &PromptSessionLimitOverride{}
	var createdAt, updatedAt any
	if err := scanner.Scan(&item.Platform, &item.NewAPIUserID, &item.Mode, &item.Limit, &item.WindowSeconds, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	var err error
	if item.CreatedAt, err = parseDBTimeValue(createdAt); err != nil {
		return nil, err
	}
	if item.UpdatedAt, err = parseDBTimeValue(updatedAt); err != nil {
		return nil, err
	}
	return item, nil
}

func (db *DB) ListPromptSessionLimitOverrides(ctx context.Context) ([]PromptSessionLimitOverride, error) {
	if err := db.ensurePromptSessionLimitOverridesTable(ctx); err != nil {
		return nil, err
	}
	rows, err := db.conn.QueryContext(ctx, `SELECT platform, newapi_user_id, mode, limit_count, window_seconds, created_at, updated_at FROM prompt_session_limit_overrides`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]PromptSessionLimitOverride, 0)
	for rows.Next() {
		item, scanErr := scanPromptSessionLimitOverride(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		items = append(items, *item)
	}
	return items, rows.Err()
}

func (db *DB) GetPromptSessionLimitOverride(ctx context.Context, platform, userID string) (*PromptSessionLimitOverride, error) {
	if err := db.ensurePromptSessionLimitOverridesTable(ctx); err != nil {
		return nil, err
	}
	platform = strings.ToLower(strings.TrimSpace(platform))
	userID = strings.TrimSpace(userID)
	return scanPromptSessionLimitOverride(db.conn.QueryRowContext(ctx, `SELECT platform, newapi_user_id, mode, limit_count, window_seconds, created_at, updated_at FROM prompt_session_limit_overrides WHERE platform=$1 AND newapi_user_id=$2`, platform, userID))
}

func (db *DB) UpsertPromptSessionLimitOverride(ctx context.Context, raw PromptSessionLimitOverride) (*PromptSessionLimitOverride, error) {
	item, err := normalizePromptSessionLimitOverride(raw)
	if err != nil {
		return nil, err
	}
	if err := db.ensurePromptSessionLimitOverridesTable(ctx); err != nil {
		return nil, err
	}
	_, err = db.conn.ExecContext(ctx, `INSERT INTO prompt_session_limit_overrides (platform, newapi_user_id, mode, limit_count, window_seconds, updated_at)
		VALUES ($1,$2,$3,$4,$5,CURRENT_TIMESTAMP)
		ON CONFLICT(platform, newapi_user_id) DO UPDATE SET mode=EXCLUDED.mode, limit_count=EXCLUDED.limit_count, window_seconds=EXCLUDED.window_seconds, updated_at=CURRENT_TIMESTAMP`,
		item.Platform, item.NewAPIUserID, item.Mode, item.Limit, item.WindowSeconds)
	if err != nil {
		return nil, err
	}
	return db.GetPromptSessionLimitOverride(ctx, item.Platform, item.NewAPIUserID)
}

func (db *DB) DeletePromptSessionLimitOverride(ctx context.Context, platform, userID string) error {
	if err := db.ensurePromptSessionLimitOverridesTable(ctx); err != nil {
		return err
	}
	_, err := db.conn.ExecContext(ctx, `DELETE FROM prompt_session_limit_overrides WHERE platform=$1 AND newapi_user_id=$2`, strings.ToLower(strings.TrimSpace(platform)), strings.TrimSpace(userID))
	return err
}
