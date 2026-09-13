package database

import (
	"context"
	"encoding/hex"
	"errors"
)

func (db *DB) RecordSessionPolicyLockIdentity(ctx context.Context, sessionKey, kind, lockKey string) error {
	decoded, err := hex.DecodeString(lockKey)
	if !ValidSessionOperationKey(sessionKey) || err != nil || !(kind == "cyber" && len(decoded) == 32 || kind == "window" && len(decoded) == 12) {
		return errors.New("invalid session policy lock identity")
	}
	var found bool
	if err := db.conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM session_policy_lock_identities WHERE session_key=$1 AND lock_kind=$2 AND lock_key=$3)`, sessionKey, kind, lockKey).Scan(&found); err != nil || found {
		return err
	}
	_, err = db.conn.ExecContext(ctx, `INSERT INTO session_policy_lock_identities(session_key,lock_kind,lock_key) VALUES ($1,$2,$3) ON CONFLICT DO NOTHING`, sessionKey, kind, lockKey)
	return err
}

func (db *DB) SessionPolicyLockKeys(ctx context.Context, sessionKey, parentKey, kind string) ([]string, error) {
	if !ValidSessionOperationKey(sessionKey) || parentKey != "" && !ValidSessionOperationKey(parentKey) || kind != "cyber" && kind != "window" {
		return nil, ErrSessionLineageConflict
	}
	if parentKey != "" {
		if err := db.RecordSessionParent(ctx, sessionKey, parentKey); err != nil {
			return nil, err
		}
	}
	rows, err := db.conn.QueryContext(ctx, `WITH RECURSIVE ancestry(session_key,depth) AS (
		SELECT CAST($1 AS TEXT), 0 UNION ALL
		SELECT links.parent_key, ancestry.depth+1 FROM session_identity_links links
		JOIN ancestry ON links.session_key=ancestry.session_key WHERE ancestry.depth<64
	) SELECT ancestry.depth, COALESCE(identities.lock_key,'') FROM ancestry
	LEFT JOIN session_policy_lock_identities identities ON identities.session_key=ancestry.session_key AND identities.lock_kind=$2
	ORDER BY ancestry.depth LIMIT 4097`, sessionKey, kind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := make([]string, 0)
	seen := make(map[string]bool)
	count := 0
	for rows.Next() {
		var depth int
		var key string
		if err := rows.Scan(&depth, &key); err != nil {
			return nil, err
		}
		count++
		if depth >= 64 || count > 4096 {
			return nil, ErrSessionLineageConflict
		}
		if key != "" && !seen[key] {
			keys = append(keys, key)
			seen[key] = true
		}
	}
	return keys, rows.Err()
}
