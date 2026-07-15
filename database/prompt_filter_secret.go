package database

import (
	"context"
	"strings"
)

func (db *DB) GetPromptFilterNewAPISecret(ctx context.Context) (string, error) {
	var secret string
	err := db.conn.QueryRowContext(ctx, `SELECT newapi_secret FROM prompt_filter_secrets WHERE id = 1`).Scan(&secret)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(secret), nil
}

func (db *DB) SetPromptFilterNewAPISecret(ctx context.Context, secret string) error {
	secret = strings.TrimSpace(secret)
	return db.withSQLiteWriteLock(ctx, func() error {
		if db.isSQLite() {
			_, err := db.conn.ExecContext(ctx, `INSERT INTO prompt_filter_secrets (id, newapi_secret, updated_at) VALUES (1, ?, CURRENT_TIMESTAMP) ON CONFLICT(id) DO UPDATE SET newapi_secret = excluded.newapi_secret, updated_at = CURRENT_TIMESTAMP`, secret)
			return err
		}
		_, err := db.conn.ExecContext(ctx, `INSERT INTO prompt_filter_secrets (id, newapi_secret, updated_at) VALUES (1, $1, NOW()) ON CONFLICT(id) DO UPDATE SET newapi_secret = EXCLUDED.newapi_secret, updated_at = NOW()`, secret)
		return err
	})
}
