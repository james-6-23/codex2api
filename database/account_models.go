package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

const maxAccountModels = 200

// MergeAccountModels appends upstream-confirmed model IDs to an account's
// existing explicit model allowlist. An empty allowlist means "all models";
// it is intentionally left empty rather than being converted into a list of
// every model seen in an upstream manifest.
//
// The read/merge/write happens in one transaction. PostgreSQL locks the row
// with FOR UPDATE; SQLite is serialized through withWriteTx. This prevents an
// asynchronous manifest refresh from overwriting a concurrent administrator
// edit made through the models editor.
func (db *DB) MergeAccountModels(ctx context.Context, id int64, additions []string) (models []string, added []string, err error) {
	if db == nil || db.conn == nil {
		return nil, nil, fmt.Errorf("database is not initialized")
	}
	additions = normalizeAccountModelIDs(additions)
	if id <= 0 || len(additions) == 0 {
		return nil, nil, nil
	}

	err = db.withWriteTx(ctx, func(tx *sql.Tx) error {
		query := `SELECT credentials FROM accounts WHERE id = $1`
		if !db.isSQLite() {
			query += ` FOR UPDATE`
		}
		var raw interface{}
		if err := tx.QueryRowContext(ctx, query, id).Scan(&raw); err != nil {
			return err
		}

		credentials := decodeCredentials(raw)
		current := stringSliceFromValue(credentials["models"])
		// Empty is the documented unlimited sentinel. Do not silently turn an
		// unlimited account into a restrictive list during auto-completion.
		if len(current) == 0 {
			models = []string{}
			return nil
		}

		seen := make(map[string]struct{}, len(current)+len(additions))
		merged := make([]string, 0, len(current)+len(additions))
		for _, model := range current {
			key := strings.ToLower(strings.TrimSpace(model))
			if key == "" {
				continue
			}
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			merged = append(merged, strings.TrimSpace(model))
		}
		for _, model := range additions {
			if len(merged) >= maxAccountModels {
				break
			}
			key := strings.ToLower(model)
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			merged = append(merged, model)
			added = append(added, model)
		}
		models = append([]string(nil), merged...)
		if len(merged) == len(current) {
			return nil
		}

		credentials["models"] = merged
		encoded, err := json.Marshal(encryptSensitiveCredentials(credentials))
		if err != nil {
			return fmt.Errorf("serialize account credentials: %w", err)
		}
		update := `UPDATE accounts SET credentials = $1, updated_at = CURRENT_TIMESTAMP WHERE id = $2`
		if !db.isSQLite() {
			update = `UPDATE accounts SET credentials = $1::jsonb, updated_at = CURRENT_TIMESTAMP WHERE id = $2`
		}
		result, err := tx.ExecContext(ctx, update, encoded, id)
		if err != nil {
			return err
		}
		if affected, err := result.RowsAffected(); err != nil {
			return err
		} else if affected == 0 {
			return sql.ErrNoRows
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	if models == nil {
		models = []string{}
	}
	return models, added, nil
}

func normalizeAccountModelIDs(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		key := strings.ToLower(value)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	return out
}
