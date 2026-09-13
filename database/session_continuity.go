package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

var ErrSessionOwnerConflict = errors.New("session account ownership changed")

type SessionContinuityRecord struct {
	AccountID           int64                                  `json:"account_id"`
	ThreadID            string                                 `json:"thread_id"`
	Number              uint64                                 `json:"number"`
	NumberKnown         bool                                   `json:"number_known"`
	LastSeen            time.Time                              `json:"last_seen"`
	LastCompleted       time.Time                              `json:"last_completed,omitempty"`
	CompletedNumber     *uint64                                `json:"completed_number,omitempty"`
	LastStatus          int                                    `json:"last_status,omitempty"`
	PreviousAccountID   int64                                  `json:"previous_account_id,omitempty"`
	FailoverCount       uint64                                 `json:"failover_count,omitempty"`
	LastFailoverAt      time.Time                              `json:"last_failover_at,omitempty"`
	LastFailoverReason  string                                 `json:"last_failover_reason,omitempty"`
	OutboundWindowReset bool                                   `json:"outbound_window_reset,omitempty"`
	OutboundWindowBases map[string]uint64                      `json:"outbound_window_bases,omitempty"`
	OutboundWindowMode  string                                 `json:"outbound_window_mode,omitempty"`
	OutboundWindows     map[string]*SessionOutboundWindowState `json:"outbound_windows,omitempty"`
	LossyContextRestart bool                                   `json:"lossy_context_restart,omitempty"`
}

func (db *DB) ReadSessionContinuity(ctx context.Context, key string) (SessionContinuityRecord, bool, error) {
	var record SessionContinuityRecord
	var raw string
	err := db.conn.QueryRowContext(ctx, `SELECT state FROM codex_session_continuity WHERE root_key=$1`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return record, false, nil
	}
	if err != nil {
		return record, false, err
	}
	err = json.Unmarshal([]byte(raw), &record)
	return record, err == nil, err
}

func (db *DB) CommitSessionContinuity(ctx context.Context, key string, next SessionContinuityRecord) (SessionContinuityRecord, error) {
	var committed SessionContinuityRecord
	err := db.withWriteTx(ctx, func(transaction *sql.Tx) error {
		payload, err := json.Marshal(next)
		if err != nil {
			return err
		}
		if _, err = transaction.ExecContext(ctx, `INSERT INTO codex_session_continuity(root_key,state,updated_at) VALUES ($1,$2,$3) ON CONFLICT(root_key) DO UPDATE SET state=codex_session_continuity.state`, key, string(payload), next.LastSeen.Unix()); err != nil {
			return err
		}
		var raw string
		if err = transaction.QueryRowContext(ctx, `SELECT state FROM codex_session_continuity WHERE root_key=$1`, key).Scan(&raw); err != nil {
			return err
		}
		if err = json.Unmarshal([]byte(raw), &committed); err != nil {
			return err
		}
		if committed.AccountID != next.AccountID || committed.FailoverCount != next.FailoverCount || committed.ThreadID != "" && committed.ThreadID != next.ThreadID {
			return ErrSessionOwnerConflict
		}
		if committed.ThreadID == "" {
			committed.ThreadID = next.ThreadID
		}
		if next.NumberKnown && (!committed.NumberKnown || next.Number > committed.Number) {
			committed.Number, committed.NumberKnown = next.Number, true
		}
		if next.LastSeen.After(committed.LastSeen) {
			committed.LastSeen = next.LastSeen
		}
		if next.LastCompleted.After(committed.LastCompleted) {
			committed.LastCompleted, committed.CompletedNumber, committed.LastStatus = next.LastCompleted, next.CompletedNumber, next.LastStatus
		}
		payload, err = json.Marshal(committed)
		if err != nil {
			return err
		}
		_, err = transaction.ExecContext(ctx, `UPDATE codex_session_continuity SET state=$2, updated_at=$3 WHERE root_key=$1`, key, string(payload), committed.LastSeen.Unix())
		return err
	})
	return committed, err
}
