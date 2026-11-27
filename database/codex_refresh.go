package database

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	ErrCodexRefreshUncertain   = errors.New("上一次 Token 刷新结果未确认，已停止重复使用旧 RT，请重新授权并导入最新凭据")
	ErrCodexCredentialsChanged = errors.New("Codex 凭据已变更")
)

// Codex rotations and administrative identity edits use the same generation
// fence. Metadata-only updates must not invalidate an in-flight refresh.
func codexIdentityCredentialChanged(before, after map[string]any) bool {
	upstream := strings.ToLower(strings.TrimSpace(credentialStringFromMap(after, "upstream_type")))
	if upstream != "" && upstream != "codex" {
		return false
	}
	if credentialStringFromMap(before, "refresh_token") == "" && credentialStringFromMap(after, "refresh_token") == "" {
		return false
	}
	for _, key := range []string{"refresh_token", "access_token", "session_token", "upstream_type", "account_id"} {
		if credentialStringFromMap(before, key) != credentialStringFromMap(after, key) {
			return true
		}
	}
	return false
}

// CodexRefreshAttempt is a durable consumption fence, not a renewable lock.
// A crashed/ambiguous exchange must never become retryable just because a lease
// expires. The journal stores only a hash and an attempt ID, never tokens.
type CodexRefreshAttempt struct {
	ID           string
	Fingerprint  string
	RefreshToken string
	Generations  map[int64]int64
}

func (db *DB) ensureCodexRefreshSchema(ctx context.Context) error {
	_, err := db.conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS codex_oauth_refresh_attempts (
		rt_fingerprint TEXT PRIMARY KEY,
		attempt_id TEXT NOT NULL,
		started_at BIGINT NOT NULL
	)`)
	return err
}

func (db *DB) codexRefreshRows(ctx context.Context, tx *sql.Tx, rt string) ([]AccountRow, error) {
	field, upstream := "credentials->>'refresh_token'", "credentials->>'upstream_type'"
	if db.isSQLite() {
		field, upstream = "json_extract(credentials, '$.refresh_token')", "json_extract(credentials, '$.upstream_type')"
	}
	query := `SELECT id, credentials, credential_generation FROM accounts WHERE status <> 'deleted' AND
		COALESCE(` + upstream + `, '') IN ('', 'codex') AND ` + field + ` IN ($1, $2) ORDER BY id`
	if !db.isSQLite() {
		query += ` FOR UPDATE`
	}
	rows, err := tx.QueryContext(ctx, query, rt, encryptCredentialValue("refresh_token", rt))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []AccountRow
	for rows.Next() {
		var row AccountRow
		var raw any
		if err := rows.Scan(&row.ID, &raw, &row.CredentialGeneration); err != nil {
			return nil, err
		}
		row.Credentials = decodeCredentials(raw)
		result = append(result, row)
	}
	return result, rows.Err()
}

// BeginCodexRefresh records intent before the first OAuth request. All routes
// with the same RT, including disabled routes absent from runtime, participate.
func (db *DB) BeginCodexRefresh(ctx context.Context, accountID, generation int64, rt string) (*CodexRefreshAttempt, error) {
	rt = strings.TrimSpace(rt)
	if rt == "" {
		return nil, errors.New("refresh_token 为空")
	}
	hash := sha256.Sum256([]byte(rt))
	attempt := &CodexRefreshAttempt{ID: uuid.NewString(), Fingerprint: hex.EncodeToString(hash[:]), RefreshToken: rt, Generations: make(map[int64]int64)}
	err := db.withSQLiteWriteLock(ctx, func() error {
		tx, err := db.conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		res, err := tx.ExecContext(ctx, `INSERT INTO codex_oauth_refresh_attempts(rt_fingerprint, attempt_id, started_at)
			VALUES ($1,$2,$3) ON CONFLICT(rt_fingerprint) DO NOTHING`, attempt.Fingerprint, attempt.ID, time.Now().Unix())
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrCodexRefreshUncertain
		}
		rows, err := db.codexRefreshRows(ctx, tx, rt)
		if err != nil {
			return err
		}
		found := false
		for _, row := range rows {
			attempt.Generations[row.ID] = row.CredentialGeneration
			if row.ID == accountID && row.CredentialGeneration == generation {
				found = true
			}
		}
		if !found {
			return ErrCodexCredentialsChanged
		}
		return tx.Commit()
	})
	if err != nil {
		return nil, err
	}
	return attempt, nil
}

// FinishCodexRefresh atomically publishes all unchanged routes and removes the
// consumption fence. It is idempotent even if a commit acknowledgment was lost.
// Administrative replacements win: both the old RT and generation must match.
func (db *DB) FinishCodexRefresh(ctx context.Context, attempt *CodexRefreshAttempt, updates map[string]any) ([]int64, error) {
	var applied []int64
	err := db.withSQLiteWriteLock(ctx, func() error {
		tx, err := db.conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		query := `SELECT attempt_id FROM codex_oauth_refresh_attempts WHERE rt_fingerprint=$1`
		if !db.isSQLite() {
			query += ` FOR UPDATE`
		}
		var owner string
		err = tx.QueryRowContext(ctx, query, attempt.Fingerprint).Scan(&owner)
		if errors.Is(err, sql.ErrNoRows) {
			// A previous commit may have succeeded while its acknowledgment failed.
			for id := range attempt.Generations {
				var raw any
				if err := tx.QueryRowContext(ctx, `SELECT credentials FROM accounts WHERE id=$1 AND status <> 'deleted'`, id).Scan(&raw); err != nil {
					if errors.Is(err, sql.ErrNoRows) {
						continue
					}
					return err
				}
				if credentialString(raw, "codex_refresh_attempt_id") == attempt.ID {
					applied = append(applied, id)
				}
			}
			return nil
		}
		if err != nil {
			return err
		}
		if owner != attempt.ID {
			return ErrCodexRefreshUncertain
		}
		rows, err := db.codexRefreshRows(ctx, tx, attempt.RefreshToken)
		if err != nil {
			return err
		}
		unresolved := false
		for _, row := range rows {
			if expected, ok := attempt.Generations[row.ID]; !ok || expected != row.CredentialGeneration {
				unresolved = true
				continue
			}
			merged := mergeCredentialMaps(row.Credentials, updates)
			merged["codex_refresh_attempt_id"] = attempt.ID
			if err := db.writeCodexRefreshCredentials(ctx, tx, row, merged, true); err != nil {
				return err
			}
			applied = append(applied, row.ID)
		}
		if !unresolved {
			if _, err := tx.ExecContext(ctx, `DELETE FROM codex_oauth_refresh_attempts WHERE rt_fingerprint=$1 AND attempt_id=$2`, attempt.Fingerprint, attempt.ID); err != nil {
				return err
			}
		}
		return tx.Commit()
	})
	if err != nil {
		return nil, err
	}
	return applied, nil
}

// FailCodexRefresh keeps the fence for an uncertain result. Only an explicit
// rejection or a request known not to have reached the server may release it.
func (db *DB) FailCodexRefresh(ctx context.Context, attempt *CodexRefreshAttempt, message string, uncertain bool) error {
	return db.withSQLiteWriteLock(ctx, func() error {
		tx, err := db.conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		rows, err := db.codexRefreshRows(ctx, tx, attempt.RefreshToken)
		if err != nil {
			return err
		}
		for _, row := range rows {
			if expected, ok := attempt.Generations[row.ID]; !ok || expected != row.CredentialGeneration {
				continue
			}
			row.Credentials["codex_refresh_error"] = message
			if err := db.writeCodexRefreshCredentials(ctx, tx, row, row.Credentials, false); err != nil {
				return err
			}
		}
		if !uncertain {
			if _, err := tx.ExecContext(ctx, `DELETE FROM codex_oauth_refresh_attempts WHERE rt_fingerprint=$1 AND attempt_id=$2`, attempt.Fingerprint, attempt.ID); err != nil {
				return err
			}
		}
		return tx.Commit()
	})
}

func (db *DB) writeCodexRefreshCredentials(ctx context.Context, tx *sql.Tx, row AccountRow, credentials map[string]any, advance bool) error {
	encoded, err := json.Marshal(encryptSensitiveCredentials(credentials))
	if err != nil {
		return err
	}
	generation := row.CredentialGeneration
	if advance {
		generation++
	}
	value := "$1"
	if !db.isSQLite() {
		value += "::jsonb"
	}
	status := ""
	if advance {
		// Credential publication is not proof that the upstream 401 is gone.
		status = ", status=CASE WHEN cooldown_reason='unauthorized' THEN status ELSE 'active' END" +
			", error_message=CASE WHEN cooldown_reason='unauthorized' THEN error_message ELSE '' END"
	}
	// Cooldown columns are deliberately untouched, including concurrent 429s.
	result, err := tx.ExecContext(ctx, `UPDATE accounts SET credentials=`+value+`, credential_generation=$2`+status+`, updated_at=CURRENT_TIMESTAMP WHERE id=$3 AND credential_generation=$4`, string(encoded), generation, row.ID, row.CredentialGeneration)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return fmt.Errorf("%w: account %d", ErrCodexCredentialsChanged, row.ID)
	}
	return invalidateDaybreakIdentity(ctx, tx, row.ID)
}

// ClearCodexAuthorizationCooldown clears only the rejection observed by a
// successful probe for the still-current bearer (including AT-only accounts).
func (db *DB) ClearCodexAuthorizationCooldown(ctx context.Context, id, generation int64, accessToken string, until time.Time, detail string) (bool, error) {
	cleared := false
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		query := `SELECT credentials, cooldown_until FROM accounts WHERE id=$1 AND credential_generation=$2
			AND status='active' AND cooldown_reason='unauthorized' AND COALESCE(error_message,'')=$3`
		if !db.isSQLite() {
			query += ` FOR UPDATE`
		}
		var credentials, rawUntil any
		if err := tx.QueryRowContext(ctx, query, id, generation, detail).Scan(&credentials, &rawUntil); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return nil
			}
			return err
		}
		currentUntil, err := parseDBNullTimeValue(rawUntil)
		if err != nil {
			return err
		}
		if !db.isSQLite() {
			until = until.Truncate(time.Microsecond)
		}
		row := AccountRow{Credentials: decodeCredentials(credentials)}
		upstream := strings.ToLower(strings.TrimSpace(row.GetCredential("upstream_type")))
		if !currentUntil.Valid || !currentUntil.Time.Equal(until) || (upstream != "" && upstream != "codex") ||
			strings.EqualFold(strings.TrimSpace(row.GetCredential("auth_mode")), "agentIdentity") || row.GetCredential("access_token") != accessToken {
			return nil
		}
		if _, err = tx.ExecContext(ctx, `UPDATE accounts SET cooldown_reason='', cooldown_until=NULL, error_message='', updated_at=CURRENT_TIMESTAMP WHERE id=$1`, id); err != nil {
			return err
		}
		cleared = true
		return nil
	})
	return cleared && err == nil, err
}
