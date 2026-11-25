package database

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
)

// PromptTaskClientSnapshot contains only the fields needed to replay a client
// turn. The original user message remains in the gateway request record.
type PromptTaskClientSnapshot struct {
	TurnID         string                 `json:"turn_id"`
	StartedAt      time.Time              `json:"started_at"`
	FinishedAt     *time.Time             `json:"finished_at,omitempty"`
	FinalText      string                 `json:"final_text,omitempty"`
	FinalRedacted  bool                   `json:"final_redacted,omitempty"`
	FinalTruncated bool                   `json:"final_truncated,omitempty"`
	Status         string                 `json:"status"`
	Steps          []PromptTaskClientStep `json:"steps,omitempty"`
	StepsTruncated bool                   `json:"steps_truncated,omitempty"`
}

type PromptTaskClientStep struct {
	Name   string     `json:"name"`
	Kind   string     `json:"kind"`
	Status string     `json:"status"`
	At     *time.Time `json:"at,omitempty"`
}

var taskClientNewlines = strings.NewReplacer("\r\n", "\n", "\r", "\n")

func PromptTaskClientUserDigest(text string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(taskClientNewlines.Replace(text))))
	return hex.EncodeToString(sum[:])
}

// PromptTaskClientGroups shares the exact authenticated group namespace used
// by the audit recorder. A hook's session identifier may occupy either field.
func PromptTaskClientGroups(sessionID string, keyID int64) []string {
	if keyID <= 0 || sessionID == "" {
		return nil
	}
	groups := make([]string, 0, 2)
	for _, field := range []string{"thread_id", "session_id"} {
		body, _ := json.Marshal(map[string]any{"client_metadata": map[string]string{field: sessionID}})
		if group := PromptRequestConversationGroup(body, keyID); group != singlePromptRequestGroup {
			groups = append(groups, group)
		}
	}
	return groups
}

func (db *DB) ensurePromptTaskClientTable(ctx context.Context) error {
	timestamp := "TIMESTAMPTZ"
	if db.isSQLite() {
		timestamp = "DATETIME"
	}
	if _, err := db.conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS prompt_task_client_snapshots (
		request_id BIGINT PRIMARY KEY REFERENCES prompt_request_records(id) ON DELETE CASCADE,
		conversation_group TEXT NOT NULL,
		turn_id TEXT NOT NULL,
		user_digest TEXT NOT NULL,
		started_at `+timestamp+` NOT NULL,
		finished_at `+timestamp+` NOT NULL,
		snapshot TEXT NOT NULL
	)`); err != nil {
		return err
	}
	if db.isSQLite() {
		if _, err := db.conn.ExecContext(ctx, `CREATE TRIGGER IF NOT EXISTS delete_prompt_task_client_snapshot
			AFTER DELETE ON prompt_request_records BEGIN
				DELETE FROM prompt_task_client_snapshots WHERE request_id = OLD.id;
			END`); err != nil {
			return err
		}
	}
	if _, err := db.conn.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_prompt_task_client_digest
		ON prompt_task_client_snapshots (conversation_group, user_digest, started_at)`); err != nil {
		return err
	}
	_, err := db.conn.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_prompt_task_client_expiry
		ON prompt_task_client_snapshots (started_at)`)
	return err
}

const promptTaskClientClockSkew = 3 * time.Second

// StorePromptTaskClientSnapshot keeps at most one report for a recorded task.
// A report must match the original user text and the scoped gateway turn.
func (db *DB) StorePromptTaskClientSnapshot(ctx context.Context, groups []string, userDigest string, snapshot PromptTaskClientSnapshot) (bool, error) {
	if snapshot.FinishedAt == nil || len(groups) == 0 {
		return false, nil
	}
	snapshot.StartedAt = snapshot.StartedAt.UTC()
	finished := snapshot.FinishedAt.UTC()
	snapshot.FinishedAt = &finished
	encoded, err := json.Marshal(snapshot)
	if err != nil {
		return false, err
	}
	admitted := false
	err = db.withWriteTx(ctx, func(tx *sql.Tx) error {
		for _, group := range groups {
			id, err := selectPromptTaskClientAnchor(ctx, tx, group, userDigest, snapshot)
			if err != nil {
				return err
			}
			if id <= 0 {
				continue
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO prompt_task_client_snapshots
				(request_id, conversation_group, turn_id, user_digest, started_at, finished_at, snapshot)
				VALUES ($1, $2, $3, $4, $5, $6, $7)
				ON CONFLICT (request_id) DO UPDATE
				SET finished_at = excluded.finished_at, snapshot = excluded.snapshot
				WHERE excluded.turn_id = prompt_task_client_snapshots.turn_id
				AND excluded.started_at = prompt_task_client_snapshots.started_at
				AND excluded.finished_at > prompt_task_client_snapshots.finished_at`,
				id, group, snapshot.TurnID, userDigest, snapshot.StartedAt, finished, string(encoded)); err != nil {
				return err
			}
			admitted = true
		}
		return nil
	})
	return admitted, err
}

func selectPromptTaskClientAnchor(ctx context.Context, tx *sql.Tx, group, digest string, snapshot PromptTaskClientSnapshot) (int64, error) {
	turnHash := sha256.Sum256([]byte(snapshot.TurnID))
	// Inspect each distinct indexed user anchor at most once. Larger wire
	// bodies continue to be available in the original audit UI.
	const maxMatchBody = 16 << 20
	const maxCandidates = 8
	rows, err := tx.QueryContext(ctx, `WITH candidates AS (
		SELECT MIN(id) AS id
		FROM prompt_request_visible_records
		WHERE conversation_group = $1 AND conversation_group <> '' AND conversation_group <> 'single'
		AND created_at >= $2 AND created_at <= $3
		AND (task_turn = $4 OR task_turn = '')
		AND length(body) <= $5
		GROUP BY CASE WHEN task_anchor IS NULL OR task_anchor = '' THEN CAST(id AS TEXT) ELSE task_anchor END
		ORDER BY MIN(id) LIMIT $6
	) SELECT r.id, r.body FROM candidates c JOIN prompt_request_visible_records r ON r.id = c.id ORDER BY r.id`,
		group, snapshot.StartedAt.Add(-promptTaskClientClockSkew), snapshot.FinishedAt.Add(promptTaskClientClockSkew),
		hex.EncodeToString(turnHash[:]), maxMatchBody, maxCandidates+1)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var exact, fallback int64
	ambiguousFallback := false
	for count := 0; rows.Next(); count++ {
		if count >= maxCandidates {
			return 0, nil
		}
		var id int64
		var body []byte
		if err := rows.Scan(&id, &body); err != nil {
			return 0, err
		}
		task := DecodePromptRequestTaskInput(body)
		if task.Field == "" || PromptTaskClientUserDigest(task.Text) != digest {
			continue
		}
		if task.TurnID == snapshot.TurnID {
			if exact != 0 {
				return 0, nil
			}
			exact = id
		} else if task.TurnID == "" && task.Text != "" {
			if fallback != 0 {
				ambiguousFallback = true
			}
			fallback = id
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if exact != 0 || ambiguousFallback {
		return exact, nil
	}
	return fallback, nil
}

// MatchPromptTaskClient requires a single client event matching the scoped
// conversation, user text fingerprint and observed time. The explicit gateway
// turn id, when present, must agree. Older requests can use a short time window.
func (db *DB) MatchPromptTaskClient(ctx context.Context, group, turnID, userText string, observedStart time.Time) (*PromptTaskClientSnapshot, error) {
	if group == "" || group == singlePromptRequestGroup || observedStart.IsZero() {
		return nil, nil
	}
	if turnID == "" && strings.TrimSpace(userText) == "" {
		return nil, nil
	}
	start := observedStart.UTC()
	args := []any{group, PromptTaskClientUserDigest(userText), start.Add(promptTaskClientClockSkew), start.Add(-promptTaskClientClockSkew)}
	query := `SELECT snapshot FROM prompt_task_client_snapshots snapshot_row
		WHERE EXISTS(SELECT 1 FROM prompt_request_visible_records r WHERE r.id=snapshot_row.request_id)
		AND conversation_group = $1 AND user_digest = $2
		AND started_at <= $3 AND finished_at >= $4`
	if turnID != "" {
		query += " AND turn_id = $5"
		args = append(args, turnID)
	} else {
		query += " AND started_at >= $5"
		args = append(args, start.Add(-2*time.Minute))
	}
	rows, err := db.conn.QueryContext(ctx, query+" LIMIT 2", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	var encoded string
	if err := rows.Scan(&encoded); err != nil {
		return nil, err
	}
	if rows.Next() {
		// A duplicate prompt submitted twice can be ambiguous.
		return nil, nil
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var snapshot PromptTaskClientSnapshot
	if err := json.Unmarshal([]byte(encoded), &snapshot); err != nil {
		return nil, err
	}
	return &snapshot, nil
}

func (db *DB) purgePromptTaskClientSnapshots(ctx context.Context, cutoff time.Time, days int) error {
	return db.withSQLiteWriteLock(ctx, func() error {
		_, err := db.conn.ExecContext(ctx, `DELETE FROM prompt_task_client_snapshots
			WHERE started_at < $1 AND $2 = COALESCE(`+promptRequestStoredRetention+`, $3)`,
			cutoff, days, DefaultPromptRequestRetentionDays)
		return err
	})
}
