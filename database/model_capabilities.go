package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// A snapshot belongs to one upstream credential generation, never to a global
// model name. Provider-specific capabilities must not leak across accounts.
type ModelCapabilitySnapshot struct {
	AccountID            int64
	CredentialGeneration int64
	ObservedAt           int64
	Models               map[string]map[string]json.RawMessage
}

const modelCapabilitiesSchema = `CREATE TABLE IF NOT EXISTS model_capability_snapshots (
 account_id BIGINT PRIMARY KEY REFERENCES accounts(id) ON DELETE CASCADE,
 credential_generation BIGINT NOT NULL,
 observed_at BIGINT NOT NULL,
 models_json TEXT NOT NULL
);`

func (db *DB) SaveModelCapabilities(ctx context.Context, snapshot ModelCapabilitySnapshot) error {
	if snapshot.AccountID <= 0 || len(snapshot.Models) == 0 {
		return nil
	}
	if len(snapshot.Models) > 512 {
		return fmt.Errorf("model capability snapshot exceeds model limit")
	}
	return db.withWriteTx(ctx, func(tx *sql.Tx) error {
		query := `SELECT credential_generation FROM accounts WHERE id=$1 AND deleted_at IS NULL`
		if !db.isSQLite() {
			query += ` FOR UPDATE`
		}
		var generation int64
		if err := tx.QueryRowContext(ctx, query, snapshot.AccountID).Scan(&generation); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		if generation != snapshot.CredentialGeneration {
			return nil
		}
		var oldGeneration, oldObserved int64
		var oldJSON string
		err := tx.QueryRowContext(ctx, `SELECT credential_generation, observed_at, models_json FROM model_capability_snapshots WHERE account_id=$1`, snapshot.AccountID).Scan(&oldGeneration, &oldObserved, &oldJSON)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		old := map[string]map[string]json.RawMessage{}
		if err == nil && oldGeneration == generation {
			if oldObserved > snapshot.ObservedAt {
				return nil
			}
			_ = json.Unmarshal([]byte(oldJSON), &old)
		}
		merged := make(map[string]map[string]json.RawMessage, len(snapshot.Models))
		for model, fields := range snapshot.Models {
			values := make(map[string]json.RawMessage)
			for name, value := range old[model] {
				values[name] = value
			}
			for name, value := range fields {
				values[name] = value
			}
			merged[model] = values
		}
		encoded, err := json.Marshal(merged)
		if err != nil {
			return err
		}
		if len(encoded) > 2<<20 {
			return fmt.Errorf("model capability snapshot exceeds byte limit")
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO model_capability_snapshots(account_id,credential_generation,observed_at,models_json)
 VALUES($1,$2,$3,$4) ON CONFLICT(account_id) DO UPDATE SET credential_generation=excluded.credential_generation,observed_at=excluded.observed_at,models_json=excluded.models_json`, snapshot.AccountID, generation, snapshot.ObservedAt, string(encoded))
		return err
	})
}

func (db *DB) ListModelCapabilities(ctx context.Context, accountIDs []int64) (map[int64]ModelCapabilitySnapshot, error) {
	result := make(map[int64]ModelCapabilitySnapshot)
	for len(accountIDs) > 0 {
		n := len(accountIDs)
		if n > 512 {
			n = 512
		}
		args := make([]any, n)
		placeholders := make([]string, n)
		for i, id := range accountIDs[:n] {
			args[i] = id
			placeholders[i] = fmt.Sprintf("$%d", i+1)
		}
		rows, err := db.conn.QueryContext(ctx, `SELECT s.account_id,s.credential_generation,s.observed_at,s.models_json FROM model_capability_snapshots s JOIN accounts a ON a.id=s.account_id AND a.credential_generation=s.credential_generation WHERE a.deleted_at IS NULL AND s.account_id IN (`+strings.Join(placeholders, ",")+")", args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var snapshot ModelCapabilitySnapshot
			var raw string
			if err := rows.Scan(&snapshot.AccountID, &snapshot.CredentialGeneration, &snapshot.ObservedAt, &raw); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if len(raw) <= 2<<20 && json.Unmarshal([]byte(raw), &snapshot.Models) == nil {
				result[snapshot.AccountID] = snapshot
			}
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, err
		}
		accountIDs = accountIDs[n:]
	}
	return result, nil
}
