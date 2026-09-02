package database

import "context"

// SetOwnedAccountError installs an error fence only when no unrelated error is
// active. The owner prefix makes repeated updates idempotent without allowing a
// provider-specific observer to overwrite an administrator quarantine. Existing
// cooldown state is independent and remains durable beneath the owned fence.
func (db *DB) SetOwnedAccountError(ctx context.Context, id int64, ownerPrefix, message string) (bool, error) {
	var applied bool
	err := db.withSQLiteWriteLock(ctx, func() error {
		result, err := db.conn.ExecContext(ctx, `
			UPDATE accounts
			SET status = 'error', error_message = $3, updated_at = CURRENT_TIMESTAMP
			WHERE id = $1 AND status <> 'deleted' AND deleted_at IS NULL AND (
				status <> 'error' OR error_message = '' OR
				substr(error_message, 1, length($2)) = $2
			)
		`, id, ownerPrefix, message)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		applied = affected > 0
		return nil
	})
	return applied, err
}

// ClearOwnedAccountError clears only the error still owned by ownerPrefix. A
// concurrent administrator or another subsystem can replace the message
// without having its newer fence erased by a delayed provider recovery. Any
// independently recorded cooldown is preserved and becomes the active runtime
// gate after the owned error is removed.
func (db *DB) ClearOwnedAccountError(ctx context.Context, id int64, ownerPrefix string) (bool, error) {
	var cleared bool
	err := db.withSQLiteWriteLock(ctx, func() error {
		result, err := db.conn.ExecContext(ctx, `
			UPDATE accounts
			SET status = 'active', error_message = '', updated_at = CURRENT_TIMESTAMP
			WHERE id = $1 AND status = 'error' AND deleted_at IS NULL AND substr(error_message, 1, length($2)) = $2
		`, id, ownerPrefix)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		cleared = affected > 0
		return nil
	})
	return cleared, err
}
