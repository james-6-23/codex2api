package database

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
	"math"
	"strings"
	"time"
)

const singlePromptRequestGroup = "single"

type PromptRequestConversation struct {
	PromptRequestRecord
	Rounds int `json:"rounds"`
}

func PromptRequestConversationGroup(body []byte, keyID int64) string {
	if keyID <= 0 {
		return singlePromptRequestGroup
	}
	// A cache key alone can be deliberately shared across conversations.
	thread := promptRequestMarker(body, "client_metadata.thread_id client_metadata.session_id metadata.conversation_id metadata.session_id conversation_id")
	if thread == "" {
		return singlePromptRequestGroup
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s", keyID, thread))))
}

func (db *DB) ListPromptRequestConversations(ctx context.Context, before int64, limit int) ([]PromptRequestConversation, error) {
	if before <= 0 {
		before = math.MaxInt64
	}
	result, err := db.conn.QueryContext(ctx, `WITH grouped AS (
		SELECT MAX(id) AS latest_id, COUNT(*) AS rounds
		FROM prompt_request_records
		GROUP BY CASE WHEN conversation_group IN ('', 'single') THEN 'request:' || CAST(id AS TEXT) ELSE conversation_group END
		HAVING MAX(id) < $1
		ORDER BY MAX(id) DESC LIMIT $2
	) SELECT r.`+strings.ReplaceAll(promptRequestSummaryColumns, ", ", ", r.")+`, g.rounds
	  FROM grouped g JOIN prompt_request_records r ON r.id = g.latest_id ORDER BY g.latest_id DESC`, before, limit)
	if err != nil {
		return nil, err
	}
	defer result.Close()
	groups := make([]PromptRequestConversation, 0, limit)
	for result.Next() {
		var group PromptRequestConversation
		if err := result.Scan(append(group.scanTargets(), &group.Rounds)...); err != nil {
			return nil, err
		}
		groups = append(groups, group)
	}
	return groups, result.Err()
}

func (db *DB) ListPromptRequestRounds(ctx context.Context, anchorID, before int64, limit int) ([]PromptRequestRecord, int, error) {
	var group string
	if err := db.conn.QueryRowContext(ctx, `SELECT conversation_group FROM prompt_request_records WHERE id = $1`, anchorID).Scan(&group); err != nil {
		return nil, 0, err
	}
	if group == "" || group == singlePromptRequestGroup {
		var record PromptRequestRecord
		err := db.conn.QueryRowContext(ctx, `SELECT `+promptRequestSummaryColumns+` FROM prompt_request_records WHERE id = $1`, anchorID).
			Scan(record.scanTargets()...)
		return []PromptRequestRecord{record}, 1, err
	}

	var total int
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM prompt_request_records WHERE conversation_group = $1 AND id <= $2`, group, anchorID).Scan(&total); err != nil {
		return nil, 0, err
	}
	query := `SELECT ` + promptRequestSummaryColumns + ` FROM prompt_request_records WHERE conversation_group = $1 AND id <= $2`
	args := []any{group, anchorID, limit}
	if before > 0 {
		query += ` AND (created_at, id) < (
			SELECT created_at, id FROM prompt_request_records WHERE id = $4 AND conversation_group = $1
		)`
		args = append(args, before)
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT $3`
	rows, err := db.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	rounds := make([]PromptRequestRecord, 0, limit)
	for rows.Next() {
		var record PromptRequestRecord
		if err := rows.Scan(record.scanTargets()...); err != nil {
			return nil, 0, err
		}
		rounds = append(rounds, record)
	}
	return rounds, total, rows.Err()
}

// PromptRequestNeighborIDs walks only a verified conversation, in the order
// shown on its timeline. Request bodies are fetched individually by the caller.
func (db *DB) PromptRequestNeighborIDs(ctx context.Context, current PromptRequestBody, following bool, limit int) ([]int64, error) {
	if current.ConversationGroup == "" || current.ConversationGroup == singlePromptRequestGroup {
		return nil, nil
	}
	query := `SELECT id FROM prompt_request_records
		WHERE conversation_group = $1 AND conversation_group <> '' AND conversation_group <> 'single'`
	if following {
		query += ` AND (created_at, id) > ($2, $3) ORDER BY created_at, id LIMIT $4`
	} else {
		query += ` AND (created_at, id) < ($2, $3) ORDER BY created_at DESC, id DESC LIMIT $4`
	}
	rows, err := db.conn.QueryContext(ctx, query, current.ConversationGroup, current.CreatedAt, current.ID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := make([]int64, 0, limit)
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// IndexPromptRequestConversations annotates archived rows without changing their
// bodies. Names were the only key identity stored by older builds, so index a
// legacy row only if its name maps to a unique key that existed at admission.
func (db *DB) IndexPromptRequestConversations(ctx context.Context) error {
	var pending bool
	if err := db.conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM prompt_request_records WHERE conversation_group = '')`).Scan(&pending); err != nil || !pending {
		return err
	}
	type keyIdentity struct {
		id      int64
		created time.Time
	}
	keys := make(map[string]keyIdentity)
	rows, err := db.conn.QueryContext(ctx, `SELECT name, id, created_at FROM api_keys WHERE created_at IS NOT NULL AND name IN
		(SELECT name FROM api_keys WHERE name <> '' GROUP BY name HAVING COUNT(*) = 1)`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var label string
		var key keyIdentity
		if err := rows.Scan(&label, &key.id, &key.created); err != nil {
			rows.Close()
			return err
		}
		keys[label] = key
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	// Only carry IDs and hashes to the write transaction, keeping large bodies
	// out of memory between batches and closing SQLite read rows before writing.
	for {
		type update struct {
			id    int64
			group string
		}
		const batchSize = 16
		batch := make([]update, 0, batchSize)
		rows, err := db.conn.QueryContext(ctx, `SELECT id, api_key_name, created_at, body FROM prompt_request_records
			WHERE conversation_group = '' ORDER BY id LIMIT $1`, batchSize)
		if err != nil {
			return err
		}
		for rows.Next() {
			var item update
			var label string
			var created time.Time
			var body []byte
			if err := rows.Scan(&item.id, &label, &created, &body); err != nil {
				rows.Close()
				return err
			}
			key := keys[label]
			if key.created.After(created) {
				key.id = 0
			}
			item.group = PromptRequestConversationGroup(body, key.id)
			batch = append(batch, item)
		}
		rows.Close()
		if err := rows.Err(); err != nil || len(batch) == 0 {
			return err
		}
		if err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
			for _, item := range batch {
				if _, err := tx.ExecContext(ctx, `UPDATE prompt_request_records SET conversation_group = $1
					WHERE id = $2 AND conversation_group = ''`, item.group, item.id); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
}
