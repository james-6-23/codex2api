package database

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// ModelRegistryRow stores one runtime model entry.
type ModelRegistryRow struct {
	ID                  string
	Enabled             bool
	Category            string
	Source              string
	ProOnly             bool
	APIKeyAuthAvailable bool
	LastSeenAt          sql.NullTime
	UpdatedAt           time.Time
}

// ModelRegistrySyncState stores metadata about the latest upstream sync.
type ModelRegistrySyncState struct {
	SourceURL    string
	LastSyncedAt sql.NullTime
}

// ListModelRegistry returns all persisted model entries.
func (db *DB) ListModelRegistry(ctx context.Context) ([]ModelRegistryRow, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT id, enabled, category, source, pro_only, api_key_auth_available, last_seen_at, updated_at
		FROM model_registry
		ORDER BY id ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]ModelRegistryRow, 0)
	for rows.Next() {
		var row ModelRegistryRow
		var lastSeenRaw, updatedRaw interface{}
		if err := rows.Scan(
			&row.ID,
			&row.Enabled,
			&row.Category,
			&row.Source,
			&row.ProOnly,
			&row.APIKeyAuthAvailable,
			&lastSeenRaw,
			&updatedRaw,
		); err != nil {
			return nil, err
		}
		lastSeen, err := parseDBNullTimeValue(lastSeenRaw)
		if err != nil {
			return nil, err
		}
		updatedAt, err := parseDBTimeValue(updatedRaw)
		if err != nil {
			return nil, err
		}
		row.LastSeenAt = lastSeen
		row.UpdatedAt = updatedAt
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

// UpsertModelRegistryRows merges model entries without deleting other rows.
func (db *DB) UpsertModelRegistryRows(ctx context.Context, models []ModelRegistryRow) error {
	if len(models) == 0 {
		return nil
	}

	query := `
		INSERT INTO model_registry (
			id, enabled, category, source, pro_only, api_key_auth_available, last_seen_at, updated_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, CURRENT_TIMESTAMP)
		ON CONFLICT (id) DO UPDATE SET
			enabled = EXCLUDED.enabled,
			category = EXCLUDED.category,
			source = EXCLUDED.source,
			pro_only = EXCLUDED.pro_only,
			api_key_auth_available = EXCLUDED.api_key_auth_available,
			last_seen_at = EXCLUDED.last_seen_at,
			updated_at = CURRENT_TIMESTAMP
	`
	if db.isSQLite() {
		query = `
			INSERT INTO model_registry (
				id, enabled, category, source, pro_only, api_key_auth_available, last_seen_at, updated_at
			)
			VALUES ($1, $2, $3, $4, $5, $6, $7, CURRENT_TIMESTAMP)
			ON CONFLICT(id) DO UPDATE SET
				enabled = excluded.enabled,
				category = excluded.category,
				source = excluded.source,
				pro_only = excluded.pro_only,
				api_key_auth_available = excluded.api_key_auth_available,
				last_seen_at = excluded.last_seen_at,
				updated_at = CURRENT_TIMESTAMP
		`
	}

	for _, model := range models {
		lastSeen := interface{}(nil)
		if model.LastSeenAt.Valid {
			lastSeen = db.timeArg(model.LastSeenAt.Time)
		}
		if _, err := db.conn.ExecContext(ctx, query,
			model.ID,
			model.Enabled,
			model.Category,
			model.Source,
			model.ProOnly,
			model.APIKeyAuthAvailable,
			lastSeen,
		); err != nil {
			return err
		}
	}
	return nil
}

// DeleteModelRegistryRows removes the given model entries by ID. Missing IDs are ignored.
func (db *DB) DeleteModelRegistryRows(ctx context.Context, ids []string) error {
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, err := db.conn.ExecContext(ctx, `DELETE FROM model_registry WHERE id = $1`, id); err != nil {
			return err
		}
	}
	return nil
}

// GetModelRegistrySyncState returns metadata about the latest upstream sync.
func (db *DB) GetModelRegistrySyncState(ctx context.Context) (*ModelRegistrySyncState, error) {
	var sourceURL string
	var syncedRaw interface{}
	err := db.conn.QueryRowContext(ctx, `
		SELECT source_url, last_synced_at FROM model_registry_sync WHERE id = 1
	`).Scan(&sourceURL, &syncedRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	lastSyncedAt, err := parseDBNullTimeValue(syncedRaw)
	if err != nil {
		return nil, err
	}
	return &ModelRegistrySyncState{
		SourceURL:    sourceURL,
		LastSyncedAt: lastSyncedAt,
	}, nil
}

// UpdateModelRegistrySyncState stores metadata about a successful upstream sync.
func (db *DB) UpdateModelRegistrySyncState(ctx context.Context, sourceURL string, syncedAt time.Time) error {
	query := `
		INSERT INTO model_registry_sync (id, source_url, last_synced_at)
		VALUES (1, $1, $2)
		ON CONFLICT (id) DO UPDATE SET
			source_url = EXCLUDED.source_url,
			last_synced_at = EXCLUDED.last_synced_at
	`
	if db.isSQLite() {
		query = `
			INSERT INTO model_registry_sync (id, source_url, last_synced_at)
			VALUES (1, $1, $2)
			ON CONFLICT(id) DO UPDATE SET
				source_url = excluded.source_url,
				last_synced_at = excluded.last_synced_at
		`
	}
	_, err := db.conn.ExecContext(ctx, query, sourceURL, db.timeArg(syncedAt))
	return err
}
