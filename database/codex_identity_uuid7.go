package database

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"time"

	"github.com/google/uuid"
)

func (db *DB) ResolveCodexIdentityUUIDv7(ctx context.Context, key, entropy string) (string, error) {
	random, err := hex.DecodeString(entropy)
	if !ValidSessionOperationKey(key) || err != nil || len(random) != 32 {
		return "", errors.New("invalid codex UUIDv7 mapping input")
	}
	var candidate uuid.UUID
	copy(candidate[:], random[:16])
	candidate[6] = candidate[6]&0x0f | 0x70
	candidate[8] = candidate[8]&0x3f | 0x80
	var stored string
	err = db.conn.QueryRowContext(ctx, `SELECT value FROM codex_identity_uuid7_values WHERE identity_key=$1`, key).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		milliseconds := time.Now().UTC().UnixMilli()
		if milliseconds <= 0 || milliseconds >= 1<<48 {
			return "", errors.New("invalid codex UUIDv7 mapping timestamp")
		}
		for index := 5; index >= 0; index-- {
			candidate[index] = byte(milliseconds)
			milliseconds >>= 8
		}
		err = db.withWriteTx(ctx, func(transaction *sql.Tx) error {
			if _, err := transaction.ExecContext(ctx, `INSERT INTO codex_identity_uuid7_values(identity_key,value) VALUES ($1,$2) ON CONFLICT DO NOTHING`, key, candidate.String()); err != nil {
				return err
			}
			if err := transaction.QueryRowContext(ctx, `SELECT value FROM codex_identity_uuid7_values WHERE identity_key=$1`, key).Scan(&stored); errors.Is(err, sql.ErrNoRows) {
				return ErrCodexIdentityAliasCollision
			} else {
				return err
			}
		})
	}
	if err != nil {
		return "", err
	}
	parsed, err := uuid.Parse(stored)
	if err != nil || parsed.Version() != 7 || parsed.Variant() != uuid.RFC4122 || parsed.String() != stored || !bytes.Equal(parsed[6:], candidate[6:]) {
		return "", errors.New("stored codex UUIDv7 mapping is invalid or belongs to a different seed")
	}
	return stored, nil
}
