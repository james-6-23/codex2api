package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"
)

type UserWindowGrant struct {
	ID           string    `json:"id"`
	Root         string    `json:"root"`
	CreatedAt    time.Time `json:"created_at"`
	ExpiresAt    time.Time `json:"expires_at"`
	PendingUntil time.Time `json:"pending_until"`
	Confirmed    bool      `json:"confirmed"`
	NoWindow     bool      `json:"no_window,omitempty"`
	Expanded     bool      `json:"expanded"`
	Multiplier   float64   `json:"multiplier"`
	ExtraLimit   int       `json:"extra_limit"`
}

type UserWindowAdmissionState struct {
	Windows      map[string]*UserWindowGrant     `json:"windows"`
	Reservations map[string]map[string]time.Time `json:"reservations,omitempty"`
}

func (db *DB) UpdateUserWindowAdmissions(ctx context.Context, subject string, update func(*UserWindowAdmissionState) error) error {
	return db.withWriteTx(ctx, func(transaction *sql.Tx) error {
		if _, err := transaction.ExecContext(ctx, `INSERT INTO prompt_user_window_grants(subject,state) VALUES ($1,'{}') ON CONFLICT(subject) DO UPDATE SET state=prompt_user_window_grants.state`, subject); err != nil {
			return err
		}
		var raw string
		if err := transaction.QueryRowContext(ctx, `SELECT state FROM prompt_user_window_grants WHERE subject=$1`, subject).Scan(&raw); err != nil {
			return err
		}
		state := UserWindowAdmissionState{}
		if err := json.Unmarshal([]byte(raw), &state); err != nil {
			return err
		}
		if state.Windows == nil {
			state.Windows = make(map[string]*UserWindowGrant)
		}
		if err := update(&state); err != nil {
			return err
		}
		payload, err := json.Marshal(state)
		if err != nil {
			return err
		}
		_, err = transaction.ExecContext(ctx, `UPDATE prompt_user_window_grants SET state=$2 WHERE subject=$1`, subject, string(payload))
		return err
	})
}

func (db *DB) ReadUserWindowAdmissions(ctx context.Context, subject string) (UserWindowAdmissionState, error) {
	var raw string
	state := UserWindowAdmissionState{Windows: make(map[string]*UserWindowGrant)}
	err := db.conn.QueryRowContext(ctx, `SELECT state FROM prompt_user_window_grants WHERE subject=$1`, subject).Scan(&raw)
	if err == sql.ErrNoRows {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	err = json.Unmarshal([]byte(raw), &state)
	return state, err
}
