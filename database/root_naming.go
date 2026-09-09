package database

import (
	"context"
	"time"
)

func (db *DB) ClaimRootNaming(ctx context.Context, subject, root string) (bool, error) {
	result, err := db.conn.ExecContext(ctx, `INSERT INTO prompt_root_naming_claims(subject,root,created_at) VALUES ($1,$2,$3) ON CONFLICT(subject,root) DO NOTHING`, subject, root, time.Now().UnixMilli())
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}
