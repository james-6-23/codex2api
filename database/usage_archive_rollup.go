package database

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"
)

// usage_account_hourly_rollups keeps compact operational statistics after the
// request-detail log has been cleared. One row represents one UTC hour and one
// account/model/API-key/channel tuple; request bodies and error text are never
// retained.
func (db *DB) ensureUsageAccountHourlyRollupsTable(ctx context.Context) error {
	if db == nil || db.conn == nil {
		return fmt.Errorf("database is not initialized")
	}
	_, err := db.conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS usage_account_hourly_rollups (
		bucket_start BIGINT NOT NULL,
		account_id BIGINT NOT NULL DEFAULT 0,
		credential_generation BIGINT NOT NULL DEFAULT 0,
		channel VARCHAR(32) NOT NULL DEFAULT '',
		model VARCHAR(255) NOT NULL DEFAULT 'unknown',
		api_key_id BIGINT NOT NULL DEFAULT 0,
		api_key_name VARCHAR(255) NOT NULL DEFAULT '',
		api_key_masked VARCHAR(255) NOT NULL DEFAULT '',
		status_code INTEGER NOT NULL DEFAULT 0,
		requests BIGINT NOT NULL DEFAULT 0,
		success_requests BIGINT NOT NULL DEFAULT 0,
		nonretry_error_requests BIGINT NOT NULL DEFAULT 0,
		retry_error_requests BIGINT NOT NULL DEFAULT 0,
		rate_limit_attempts BIGINT NOT NULL DEFAULT 0,
		end_user_requests BIGINT NOT NULL DEFAULT 0,
		end_user_tokens BIGINT NOT NULL DEFAULT 0,
		end_user_account_billed DOUBLE PRECISION NOT NULL DEFAULT 0,
		end_user_user_billed DOUBLE PRECISION NOT NULL DEFAULT 0,
		total_tokens BIGINT NOT NULL DEFAULT 0,
		prompt_tokens BIGINT NOT NULL DEFAULT 0,
		completion_tokens BIGINT NOT NULL DEFAULT 0,
		input_tokens BIGINT NOT NULL DEFAULT 0,
		output_tokens BIGINT NOT NULL DEFAULT 0,
		reasoning_tokens BIGINT NOT NULL DEFAULT 0,
		cached_tokens BIGINT NOT NULL DEFAULT 0,
		cache_hit_requests BIGINT NOT NULL DEFAULT 0,
		error_requests BIGINT NOT NULL DEFAULT 0,
		retry_requests BIGINT NOT NULL DEFAULT 0,
		duration_ms_sum DOUBLE PRECISION NOT NULL DEFAULT 0,
		duration_samples BIGINT NOT NULL DEFAULT 0,
		first_token_ms_sum DOUBLE PRECISION NOT NULL DEFAULT 0,
		first_token_samples BIGINT NOT NULL DEFAULT 0,
		stream_requests BIGINT NOT NULL DEFAULT 0,
		compact_requests BIGINT NOT NULL DEFAULT 0,
		account_billed DOUBLE PRECISION NOT NULL DEFAULT 0,
		user_billed DOUBLE PRECISION NOT NULL DEFAULT 0,
		PRIMARY KEY (bucket_start, account_id, credential_generation, channel, model, api_key_id, api_key_name, api_key_masked, status_code)
	)`)
	if err != nil {
		return err
	}
	_, err = db.conn.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_usage_account_hourly_account_bucket ON usage_account_hourly_rollups(account_id, bucket_start)`)
	return err
}

type AccountBillingWindowKind string

const (
	AccountBillingWindow5h   AccountBillingWindowKind = "5h"
	AccountBillingWindowLong AccountBillingWindowKind = "long"
)

type AccountBillingWindow struct {
	AccountID int64
	Kind      AccountBillingWindowKind
	Start     time.Time
	Duration  time.Duration
}

type AccountBillingWindowKey struct {
	AccountID int64
	Kind      AccountBillingWindowKind
}

const maxAccountBillingWindowDrift = 24 * time.Hour

const maxLegacyAccountBillingWindowDrift = 5 * time.Minute

// A duration correction can move the derived start substantially while the
// upstream reset boundary stays the same. Preserve already archived cost when
// those two independently derived reset instants differ only by clock/header
// noise.
const accountBillingWindowResetTolerance = 5 * time.Minute

func accountBillingWindowDriftTolerance(duration time.Duration) time.Duration {
	if duration <= 0 {
		return 0
	}
	// State stores whole window seconds. Keep the tolerance on the same unit so
	// the Go resolver and the portable SQLite/PostgreSQL UPSERT predicate agree.
	tolerance := time.Duration(int64(duration/time.Second)/4) * time.Second
	if tolerance > maxAccountBillingWindowDrift {
		return maxAccountBillingWindowDrift
	}
	return tolerance
}

func legacyAccountBillingWindowDriftTolerance(duration time.Duration) time.Duration {
	tolerance := accountBillingWindowDriftTolerance(duration)
	if tolerance > maxLegacyAccountBillingWindowDrift {
		return maxLegacyAccountBillingWindowDrift
	}
	return tolerance
}

func normalizeAccountBillingWindows(windows []AccountBillingWindow) ([]AccountBillingWindow, error) {
	byKey := make(map[AccountBillingWindowKey]AccountBillingWindow, len(windows))
	for _, window := range windows {
		if window.AccountID <= 0 || window.Start.IsZero() || window.Duration < time.Second {
			continue
		}
		switch window.Kind {
		case AccountBillingWindow5h, AccountBillingWindowLong:
		default:
			return nil, fmt.Errorf("unsupported account billing window kind %q", window.Kind)
		}
		window.Start = window.Start.UTC()
		window.Duration = time.Duration(int64(window.Duration/time.Second)) * time.Second
		key := AccountBillingWindowKey{AccountID: window.AccountID, Kind: window.Kind}
		if previous, ok := byKey[key]; ok {
			if previous.Start.Equal(window.Start) && previous.Duration == window.Duration {
				continue
			}
			return nil, fmt.Errorf("conflicting account billing windows for account %d kind %s", window.AccountID, window.Kind)
		}
		byKey[key] = window
	}

	normalized := make([]AccountBillingWindow, 0, len(byKey))
	for _, window := range byKey {
		normalized = append(normalized, window)
	}
	sort.Slice(normalized, func(i, j int) bool {
		if normalized[i].AccountID != normalized[j].AccountID {
			return normalized[i].AccountID < normalized[j].AccountID
		}
		return normalized[i].Kind < normalized[j].Kind
	})
	return normalized, nil
}

// ensureUsageAccountBillingWindowRollupsTable keeps the original untyped table
// readable for upgrades, while all new clears write to the typed v2 state table.
// One v2 row is the stable anchor and archived cost for an account/window kind.
// A separate kind is required because a 5h and long window can legitimately
// have the same derived start while representing different billing ranges.
func (db *DB) ensureUsageAccountBillingWindowRollupsTable(ctx context.Context) error {
	if db == nil || db.conn == nil {
		return fmt.Errorf("database is not initialized")
	}
	_, err := db.conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS usage_account_billing_window_rollups (
		account_id BIGINT NOT NULL,
		window_start BIGINT NOT NULL,
		account_billed DOUBLE PRECISION NOT NULL DEFAULT 0,
		PRIMARY KEY (account_id, window_start)
	)`)
	if err != nil {
		return err
	}
	if _, err = db.conn.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_usage_account_billing_window_start ON usage_account_billing_window_rollups(window_start)`); err != nil {
		return err
	}
	if err = db.migrateUsageAccountBillingWindowRollupKeys(ctx); err != nil {
		return err
	}
	_, err = db.conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS usage_account_billing_window_states (
		account_id BIGINT NOT NULL,
		window_kind VARCHAR(16) NOT NULL,
		window_seconds BIGINT NOT NULL,
		anchor_start BIGINT NOT NULL,
		account_billed DOUBLE PRECISION NOT NULL DEFAULT 0,
		PRIMARY KEY (account_id, window_kind)
	)`)
	return err
}

const legacyBillingWindowNanosecondThreshold int64 = 1_000_000_000_000

const billingWindowMigrationLockID int64 = 0x6332613262776d31

// accountBillingWindowRollupKey deliberately affects only the archive lookup
// identity. Live usage_logs are still filtered by the exact since timestamp so
// normalization never widens the active billing range.
func accountBillingWindowRollupKey(since time.Time) int64 {
	return since.UTC().Unix() / 60 * 60
}

// migrateUsageAccountBillingWindowRollupKeys upgrades the original UnixNano
// keys to minute-stable Unix-second keys. Multiple old rows can represent
// successive log clears in the same upstream window, so they are summed. The
// insert and delete share one transaction, making the migration idempotent.
func (db *DB) migrateUsageAccountBillingWindowRollupKeys(ctx context.Context) error {
	var options *sql.TxOptions
	if !db.isSQLite() {
		options = &sql.TxOptions{Isolation: sql.LevelReadCommitted}
	}
	tx, err := db.conn.BeginTx(ctx, options)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if !db.isSQLite() {
		if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock($1)`, billingWindowMigrationLockID); err != nil {
			return err
		}
	}

	if _, err = tx.ExecContext(ctx, `
		INSERT INTO usage_account_billing_window_rollups(account_id, window_start, account_billed)
		SELECT account_id, ((window_start / 1000000000) / 60) * 60, SUM(account_billed)
		FROM usage_account_billing_window_rollups
		WHERE window_start > $1
		GROUP BY account_id, ((window_start / 1000000000) / 60) * 60
		ON CONFLICT (account_id, window_start) DO UPDATE SET
			account_billed = usage_account_billing_window_rollups.account_billed + excluded.account_billed
	`, legacyBillingWindowNanosecondThreshold); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM usage_account_billing_window_rollups WHERE window_start > $1`, legacyBillingWindowNanosecondThreshold); err != nil {
		return err
	}
	return tx.Commit()
}

type accountBillingWindowState struct {
	WindowSeconds int64
	AnchorStart   int64
	AccountBilled float64
}

type accountBillingWindowStateQueryer interface {
	QueryContext(context.Context, string, ...interface{}) (*sql.Rows, error)
}

type accountBillingWindowStateExecer interface {
	ExecContext(context.Context, string, ...interface{}) (sql.Result, error)
}

const keepAccountBillingWindowStateSQL = `usage_account_billing_window_states.window_seconds = excluded.window_seconds
	AND excluded.anchor_start <= usage_account_billing_window_states.anchor_start +
		(CASE
			WHEN excluded.window_seconds / 4 > 86400 THEN 86400
			ELSE excluded.window_seconds / 4
		END) * 1000000000`

func sameAccountBillingWindowResetSQL() string {
	return fmt.Sprintf(`
		usage_account_billing_window_states.window_seconds > 0
		AND excluded.window_seconds > 0
		AND ABS(
			(excluded.anchor_start - usage_account_billing_window_states.anchor_start) +
			(excluded.window_seconds - usage_account_billing_window_states.window_seconds) * 1000000000
		) <= %d
	`, accountBillingWindowResetTolerance.Nanoseconds())
}

func keepAccountBillingWindowStateUpdateSQL() string {
	// Once a reset generation has been expanded from a provisional weekly
	// duration to its authoritative monthly duration, a stale process must not
	// shrink it again and delete still-live early-month rows on the next clear.
	return fmt.Sprintf(`(%s) OR ((%s) AND
		excluded.window_seconds <= usage_account_billing_window_states.window_seconds)`,
		keepAccountBillingWindowStateSQL, sameAccountBillingWindowResetSQL())
}

func preserveAccountBillingWindowCostSQL() string {
	return fmt.Sprintf(`(%s) OR (%s)`, keepAccountBillingWindowStateSQL, sameAccountBillingWindowResetSQL())
}

// ensureAccountBillingWindowStates atomically establishes the first observed
// anchor even before any log clear. Later observations inside the bounded drift
// reuse it; a different reset generation replaces just that kind. A duration
// correction that still describes the same reset boundary preserves its cost.
func (db *DB) ensureAccountBillingWindowStates(ctx context.Context, execer accountBillingWindowStateExecer, windows []AccountBillingWindow) error {
	if len(windows) == 0 {
		return nil
	}
	const maxWindowsPerBatch = 500
	for start := 0; start < len(windows); start += maxWindowsPerBatch {
		end := start + maxWindowsPerBatch
		if end > len(windows) {
			end = len(windows)
		}
		values := make([]string, 0, end-start)
		args := make([]interface{}, 0, (end-start)*4)
		argIdx := 1
		for _, window := range windows[start:end] {
			if db.isSQLite() {
				values = append(values, fmt.Sprintf("($%d, $%d, $%d, $%d, 0)", argIdx, argIdx+1, argIdx+2, argIdx+3))
			} else {
				values = append(values, fmt.Sprintf("($%d::BIGINT, $%d::VARCHAR, $%d::BIGINT, $%d::BIGINT, 0)", argIdx, argIdx+1, argIdx+2, argIdx+3))
			}
			args = append(args, window.AccountID, string(window.Kind), int64(window.Duration/time.Second), window.Start.UnixNano())
			argIdx += 4
		}
		query := fmt.Sprintf(`INSERT INTO usage_account_billing_window_states(
				account_id, window_kind, window_seconds, anchor_start, account_billed)
			VALUES %s
			ON CONFLICT (account_id, window_kind) DO UPDATE SET
				window_seconds = excluded.window_seconds,
				anchor_start = excluded.anchor_start,
				account_billed = CASE
					WHEN %s THEN usage_account_billing_window_states.account_billed
					ELSE 0
				END
			WHERE NOT (%s)`, strings.Join(values, ","), preserveAccountBillingWindowCostSQL(), keepAccountBillingWindowStateUpdateSQL())
		if _, err := execer.ExecContext(ctx, query, args...); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) loadAccountBillingWindowStates(ctx context.Context, queryer accountBillingWindowStateQueryer, windows []AccountBillingWindow) (map[AccountBillingWindowKey]accountBillingWindowState, error) {
	states := make(map[AccountBillingWindowKey]accountBillingWindowState)
	if len(windows) == 0 {
		return states, nil
	}

	const maxWindowsPerBatch = 500
	for start := 0; start < len(windows); start += maxWindowsPerBatch {
		end := start + maxWindowsPerBatch
		if end > len(windows) {
			end = len(windows)
		}
		values := make([]string, 0, end-start)
		args := make([]interface{}, 0, (end-start)*2)
		argIdx := 1
		for _, window := range windows[start:end] {
			if db.isSQLite() {
				values = append(values, fmt.Sprintf("($%d, $%d)", argIdx, argIdx+1))
			} else {
				values = append(values, fmt.Sprintf("($%d::BIGINT, $%d::VARCHAR)", argIdx, argIdx+1))
			}
			args = append(args, window.AccountID, string(window.Kind))
			argIdx += 2
		}
		query := fmt.Sprintf(`WITH requested(account_id, window_kind) AS (VALUES %s)
			SELECT states.account_id, states.window_kind, states.window_seconds,
				states.anchor_start, states.account_billed
			FROM usage_account_billing_window_states states
			JOIN requested ON requested.account_id = states.account_id
				AND requested.window_kind = states.window_kind`, strings.Join(values, ","))
		rows, err := queryer.QueryContext(ctx, query, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var accountID int64
			var kind string
			var state accountBillingWindowState
			if err := rows.Scan(&accountID, &kind, &state.WindowSeconds, &state.AnchorStart, &state.AccountBilled); err != nil {
				rows.Close()
				return nil, err
			}
			states[AccountBillingWindowKey{AccountID: accountID, Kind: AccountBillingWindowKind(kind)}] = state
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return states, nil
}

type resolvedAccountBillingWindow struct {
	AccountBillingWindow
	AnchorStart   time.Time
	AccountBilled float64
	Matched       bool
}

func resolveAccountBillingWindows(windows []AccountBillingWindow, states map[AccountBillingWindowKey]accountBillingWindowState) []resolvedAccountBillingWindow {
	resolved := make([]resolvedAccountBillingWindow, 0, len(windows))
	for _, window := range windows {
		item := resolvedAccountBillingWindow{AccountBillingWindow: window, AnchorStart: window.Start}
		key := AccountBillingWindowKey{AccountID: window.AccountID, Kind: window.Kind}
		state, ok := states[key]
		if ok && state.WindowSeconds > 0 {
			anchor := time.Unix(0, state.AnchorStart).UTC()
			stateDuration := time.Duration(state.WindowSeconds) * time.Second
			// The anchor is monotonic for one duration. A far-backward value can
			// come from an old process or stale runtime snapshot and must not
			// rewind a newer billing generation. A far-forward value is a rollover.
			sameDuration := state.WindowSeconds == int64(window.Duration/time.Second) &&
				window.Start.Sub(anchor) <= accountBillingWindowDriftTolerance(window.Duration)
			resetDelta := window.Start.Add(window.Duration).Sub(anchor.Add(stateDuration))
			sameResetWithoutShrink := stateDuration >= window.Duration &&
				resetDelta >= -accountBillingWindowResetTolerance && resetDelta <= accountBillingWindowResetTolerance
			if sameDuration || sameResetWithoutShrink {
				item.Start = anchor
				item.Duration = stateDuration
				item.AnchorStart = anchor
				item.AccountBilled = state.AccountBilled
				item.Matched = true
			}
		}
		resolved = append(resolved, item)
	}
	return resolved
}

// archiveAccountBillingWindowsWithExec archives each explicitly typed active
// quota window. A matching v2 state keeps its first anchor, so relative reset
// headers may drift across minutes or hours without changing the live filter or
// archive identity. A different reset generation replaces only that kind;
// same-boundary duration corrections keep the already archived cost.
func (db *DB) archiveAccountBillingWindowsWithExec(ctx context.Context, tx *sql.Tx, input []AccountBillingWindow) error {
	windows, err := normalizeAccountBillingWindows(input)
	if err != nil {
		return err
	}
	if len(windows) == 0 {
		return nil
	}
	// Establish or advance every state while this clear transaction owns the
	// row lock. A concurrent typed read cannot move the state between resolve
	// and the cost UPSERT and make an older anchor win again.
	if err := db.ensureAccountBillingWindowStates(ctx, tx, windows); err != nil {
		return err
	}
	states, err := db.loadAccountBillingWindowStates(ctx, tx, windows)
	if err != nil {
		return err
	}
	resolved := resolveAccountBillingWindows(windows, states)

	const maxWindowsPerBatch = 500
	for start := 0; start < len(resolved); start += maxWindowsPerBatch {
		end := start + maxWindowsPerBatch
		if end > len(resolved) {
			end = len(resolved)
		}
		values := make([]string, 0, end-start)
		args := make([]interface{}, 0, (end-start)*5)
		argIdx := 1
		for _, window := range resolved[start:end] {
			if db.isSQLite() {
				values = append(values, fmt.Sprintf("($%d, $%d, $%d, $%d, $%d)", argIdx, argIdx+1, argIdx+2, argIdx+3, argIdx+4))
			} else {
				values = append(values, fmt.Sprintf("($%d::BIGINT, $%d::VARCHAR, $%d::BIGINT, $%d::BIGINT, $%d::TIMESTAMPTZ)", argIdx, argIdx+1, argIdx+2, argIdx+3, argIdx+4))
			}
			args = append(args, window.AccountID, string(window.Kind), int64(window.Duration/time.Second), window.AnchorStart.UnixNano(), db.timeArg(window.AnchorStart))
			argIdx += 5
		}
		query := fmt.Sprintf(`WITH billing_windows(account_id, window_kind, window_seconds, anchor_start, since_at) AS (VALUES %s)
			INSERT INTO usage_account_billing_window_states(account_id, window_kind, window_seconds, anchor_start, account_billed)
			SELECT billing_windows.account_id, billing_windows.window_kind,
				billing_windows.window_seconds, billing_windows.anchor_start,
				COALESCE(SUM(usage_logs.account_billed), 0)
			FROM billing_windows
			LEFT JOIN usage_logs ON usage_logs.account_id = billing_windows.account_id
				AND usage_logs.created_at >= billing_windows.since_at
				AND usage_logs.status_code <> 499
				AND TRIM(COALESCE(usage_logs.internal_reason, '')) = ''
			GROUP BY billing_windows.account_id, billing_windows.window_kind,
				billing_windows.window_seconds, billing_windows.anchor_start
			ON CONFLICT (account_id, window_kind) DO UPDATE SET
				window_seconds = excluded.window_seconds,
				anchor_start = excluded.anchor_start,
				account_billed = CASE
					WHEN usage_account_billing_window_states.window_seconds = excluded.window_seconds
						AND usage_account_billing_window_states.anchor_start = excluded.anchor_start
					THEN usage_account_billing_window_states.account_billed + excluded.account_billed
					ELSE excluded.account_billed
				END`, strings.Join(values, ","))
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) archiveUsageLogsWithExec(ctx context.Context, tx *sql.Tx) error {
	if db == nil || tx == nil {
		return fmt.Errorf("archive usage logs: database transaction is nil")
	}
	bucketExpr := `(CAST(strftime('%s', created_at) AS INTEGER) / 3600) * 3600`
	boolTrue := "1"
	retryFalse := "COALESCE(is_retry_attempt, 0) = 0"
	retryTrue := "COALESCE(is_retry_attempt, 0) = 1"
	if !db.isSQLite() {
		bucketExpr = `CAST(FLOOR(EXTRACT(EPOCH FROM created_at) / 3600) AS BIGINT) * 3600`
		boolTrue = "TRUE"
		retryFalse = "COALESCE(is_retry_attempt, false) = false"
		retryTrue = "COALESCE(is_retry_attempt, false) = true"
	}
	query := fmt.Sprintf(`INSERT INTO usage_account_hourly_rollups (
		bucket_start, account_id, credential_generation, channel, model,
		api_key_id, api_key_name, api_key_masked, status_code, requests,
		success_requests, nonretry_error_requests, retry_error_requests, rate_limit_attempts,
		end_user_requests, end_user_tokens, end_user_account_billed, end_user_user_billed,
		total_tokens, prompt_tokens, completion_tokens, input_tokens, output_tokens, reasoning_tokens, cached_tokens,
		cache_hit_requests, error_requests, retry_requests,
		duration_ms_sum, duration_samples, first_token_ms_sum, first_token_samples,
		stream_requests, compact_requests, account_billed, user_billed
	)
	SELECT %s, account_id, COALESCE(credential_generation, 0), COALESCE(channel, ''),
		COALESCE(NULLIF(effective_model, ''), NULLIF(model, ''), 'unknown'),
		COALESCE(api_key_id, 0), COALESCE(api_key_name, ''), COALESCE(api_key_masked, ''), status_code, COUNT(*),
		COALESCE(SUM(CASE WHEN status_code < 400 AND %s THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status_code >= 400 AND %s THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status_code >= 400 AND %s THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status_code = 429 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status_code <> 499 AND %s THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status_code <> 499 AND %s THEN total_tokens ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status_code <> 499 AND %s THEN account_billed ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status_code <> 499 AND %s THEN user_billed ELSE 0 END), 0),
		COALESCE(SUM(total_tokens), 0), COALESCE(SUM(prompt_tokens), 0), COALESCE(SUM(completion_tokens), 0),
		COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0),
		COALESCE(SUM(reasoning_tokens), 0), COALESCE(SUM(cached_tokens), 0),
		COALESCE(SUM(CASE WHEN cached_tokens > 0 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status_code >= 400 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN attempt_index > 1 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN duration_ms > 0 THEN duration_ms ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN duration_ms > 0 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN first_token_ms > 0 THEN first_token_ms ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN first_token_ms > 0 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN stream = %s THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN compact = %s THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(account_billed), 0), COALESCE(SUM(user_billed), 0)
	FROM usage_logs
	WHERE TRIM(COALESCE(internal_reason, '')) = ''
	GROUP BY %s, account_id, COALESCE(credential_generation, 0), COALESCE(channel, ''),
		COALESCE(NULLIF(effective_model, ''), NULLIF(model, ''), 'unknown'),
		COALESCE(api_key_id, 0), COALESCE(api_key_name, ''), COALESCE(api_key_masked, ''), status_code
	ON CONFLICT (bucket_start, account_id, credential_generation, channel, model, api_key_id, api_key_name, api_key_masked, status_code)
	DO UPDATE SET
		requests=usage_account_hourly_rollups.requests+excluded.requests,
		success_requests=usage_account_hourly_rollups.success_requests+excluded.success_requests,
		nonretry_error_requests=usage_account_hourly_rollups.nonretry_error_requests+excluded.nonretry_error_requests,
		retry_error_requests=usage_account_hourly_rollups.retry_error_requests+excluded.retry_error_requests,
		rate_limit_attempts=usage_account_hourly_rollups.rate_limit_attempts+excluded.rate_limit_attempts,
		end_user_requests=usage_account_hourly_rollups.end_user_requests+excluded.end_user_requests,
		end_user_tokens=usage_account_hourly_rollups.end_user_tokens+excluded.end_user_tokens,
		end_user_account_billed=usage_account_hourly_rollups.end_user_account_billed+excluded.end_user_account_billed,
		end_user_user_billed=usage_account_hourly_rollups.end_user_user_billed+excluded.end_user_user_billed,
		total_tokens=usage_account_hourly_rollups.total_tokens+excluded.total_tokens,
		prompt_tokens=usage_account_hourly_rollups.prompt_tokens+excluded.prompt_tokens,
		completion_tokens=usage_account_hourly_rollups.completion_tokens+excluded.completion_tokens,
		input_tokens=usage_account_hourly_rollups.input_tokens+excluded.input_tokens,
		output_tokens=usage_account_hourly_rollups.output_tokens+excluded.output_tokens,
		reasoning_tokens=usage_account_hourly_rollups.reasoning_tokens+excluded.reasoning_tokens,
		cached_tokens=usage_account_hourly_rollups.cached_tokens+excluded.cached_tokens,
		cache_hit_requests=usage_account_hourly_rollups.cache_hit_requests+excluded.cache_hit_requests,
		error_requests=usage_account_hourly_rollups.error_requests+excluded.error_requests,
		retry_requests=usage_account_hourly_rollups.retry_requests+excluded.retry_requests,
		duration_ms_sum=usage_account_hourly_rollups.duration_ms_sum+excluded.duration_ms_sum,
		duration_samples=usage_account_hourly_rollups.duration_samples+excluded.duration_samples,
		first_token_ms_sum=usage_account_hourly_rollups.first_token_ms_sum+excluded.first_token_ms_sum,
		first_token_samples=usage_account_hourly_rollups.first_token_samples+excluded.first_token_samples,
		stream_requests=usage_account_hourly_rollups.stream_requests+excluded.stream_requests,
		compact_requests=usage_account_hourly_rollups.compact_requests+excluded.compact_requests,
		account_billed=usage_account_hourly_rollups.account_billed+excluded.account_billed,
		user_billed=usage_account_hourly_rollups.user_billed+excluded.user_billed`, bucketExpr,
		retryFalse, retryFalse, retryTrue, retryFalse, retryFalse, retryFalse, retryFalse,
		boolTrue, boolTrue, bucketExpr)
	_, err := tx.ExecContext(ctx, query)
	return err
}

type archivedUsageSummary struct {
	Requests, Tokens, Prompt, Completion, Cached, CacheHits, Errors int64
	DurationSum                                                     float64
	DurationSamples                                                 int64
	FirstTokenSum                                                   float64
	FirstTokenSamples                                               int64
	AccountBilled, UserBilled                                       float64
}

func (db *DB) archivedUsageSummaryForRange(ctx context.Context, start, end time.Time, channel string) (archivedUsageSummary, error) {
	result := archivedUsageSummary{}
	startBucket := start.UTC().Unix() / 3600 * 3600
	args := []interface{}{startBucket}
	where := "bucket_start >= $1 AND status_code <> 499"
	if !end.IsZero() {
		args = append(args, end.UTC().Unix()/3600*3600)
		where += fmt.Sprintf(" AND bucket_start < $%d", len(args))
	}
	if channel = strings.TrimSpace(channel); channel != "" {
		args = append(args, channel)
		where += fmt.Sprintf(" AND channel = $%d", len(args))
	}
	err := db.conn.QueryRowContext(ctx, `SELECT COALESCE(SUM(requests),0), COALESCE(SUM(total_tokens),0),
		COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0), COALESCE(SUM(cached_tokens),0),
		COALESCE(SUM(cache_hit_requests),0), COALESCE(SUM(error_requests),0),
		COALESCE(SUM(duration_ms_sum),0), COALESCE(SUM(duration_samples),0),
		COALESCE(SUM(first_token_ms_sum),0), COALESCE(SUM(first_token_samples),0),
		COALESCE(SUM(account_billed),0), COALESCE(SUM(user_billed),0)
		FROM usage_account_hourly_rollups WHERE `+where, args...).Scan(
		&result.Requests, &result.Tokens, &result.Prompt, &result.Completion, &result.Cached,
		&result.CacheHits, &result.Errors, &result.DurationSum, &result.DurationSamples,
		&result.FirstTokenSum, &result.FirstTokenSamples, &result.AccountBilled, &result.UserBilled)
	return result, err
}

func mergeAccountRequestCount(dst map[int64]*AccountRequestCount, src *AccountRequestCount) {
	if src == nil {
		return
	}
	item := dst[src.AccountID]
	if item == nil {
		item = &AccountRequestCount{AccountID: src.AccountID}
		dst[src.AccountID] = item
	}
	item.SuccessCount += src.SuccessCount
	item.ErrorCount += src.ErrorCount
	item.RetryErrorCount += src.RetryErrorCount
	item.RateLimitAttemptCount += src.RateLimitAttemptCount
}

func (db *DB) archivedAccountRequestCounts(ctx context.Context, since time.Time, ids []int64) (map[int64]*AccountRequestCount, error) {
	result := make(map[int64]*AccountRequestCount)
	args := []interface{}{since.UTC().Unix() / 3600 * 3600}
	idFilter := ""
	if ids != nil {
		ids = positiveUniqueIDs(ids)
		if len(ids) == 0 {
			return result, nil
		}
		idFilter = " AND " + db.appendAccountIDFilter(&args, ids)
	}
	rows, err := db.conn.QueryContext(ctx, `SELECT account_id,
		COALESCE(SUM(success_requests),0), COALESCE(SUM(nonretry_error_requests),0),
		COALESCE(SUM(retry_error_requests),0), COALESCE(SUM(rate_limit_attempts),0)
		FROM usage_account_hourly_rollups WHERE bucket_start >= $1`+idFilter+` GROUP BY account_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		item := &AccountRequestCount{}
		if err := rows.Scan(&item.AccountID, &item.SuccessCount, &item.ErrorCount, &item.RetryErrorCount, &item.RateLimitAttemptCount); err != nil {
			return nil, err
		}
		result[item.AccountID] = item
	}
	return result, rows.Err()
}

func (db *DB) archivedAccountTimeRangeUsage(ctx context.Context, since time.Time, ids []int64) (map[int64]*AccountTimeRangeUsage, error) {
	result := make(map[int64]*AccountTimeRangeUsage)
	args := []interface{}{since.UTC().Unix() / 3600 * 3600}
	idFilter := ""
	if ids != nil {
		ids = positiveUniqueIDs(ids)
		if len(ids) == 0 {
			return result, nil
		}
		idFilter = " AND " + db.appendAccountIDFilter(&args, ids)
	}
	rows, err := db.conn.QueryContext(ctx, `SELECT account_id,
		COALESCE(SUM(end_user_requests),0), COALESCE(SUM(end_user_tokens),0),
		COALESCE(SUM(end_user_account_billed),0), COALESCE(SUM(end_user_user_billed),0)
		FROM usage_account_hourly_rollups WHERE bucket_start >= $1`+idFilter+` GROUP BY account_id`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		item := &AccountTimeRangeUsage{}
		if err := rows.Scan(&item.AccountID, &item.Requests, &item.Tokens, &item.AccountBilled, &item.UserBilled); err != nil {
			return nil, err
		}
		result[item.AccountID] = item
	}
	return result, rows.Err()
}

func mergeAccountTimeRangeUsage(dst map[int64]*AccountTimeRangeUsage, src map[int64]*AccountTimeRangeUsage) {
	for accountID, item := range src {
		if item == nil {
			continue
		}
		current := dst[accountID]
		if current == nil {
			current = &AccountTimeRangeUsage{AccountID: accountID}
			dst[accountID] = current
		}
		current.Requests += item.Requests
		current.Tokens += item.Tokens
		current.AccountBilled += item.AccountBilled
		current.UserBilled += item.UserBilled
	}
}

func (db *DB) attachArchivedAccountRequestBreakdowns(ctx context.Context, result map[int64]*AccountRequestCount, ids []int64) error {
	if len(result) == 0 {
		return nil
	}
	since := time.Now().AddDate(0, 0, -7).UTC().Unix() / 3600 * 3600
	args := []interface{}{since}
	idFilter := ""
	if ids != nil {
		ids = positiveUniqueIDs(ids)
		if len(ids) == 0 {
			return nil
		}
		idFilter = " AND " + db.appendAccountIDFilter(&args, ids)
	}
	rows, err := db.conn.QueryContext(ctx, `SELECT account_id, status_code, COALESCE(SUM(nonretry_error_requests),0)
		FROM usage_account_hourly_rollups
		WHERE bucket_start >= $1 AND status_code >= 400`+idFilter+`
		GROUP BY account_id, status_code`, args...)
	if err != nil {
		return err
	}
	for rows.Next() {
		var accountID, count int64
		var statusCode int
		if err := rows.Scan(&accountID, &statusCode, &count); err != nil {
			rows.Close()
			return err
		}
		if item := result[accountID]; item != nil && count > 0 {
			if item.ErrorStatusCounts == nil {
				item.ErrorStatusCounts = make(map[int]int64)
			}
			item.ErrorStatusCounts[statusCode] += count
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	args = []interface{}{since}
	idFilter = ""
	if ids != nil {
		idFilter = " AND " + db.appendAccountIDFilter(&args, ids)
	}
	rows, err = db.conn.QueryContext(ctx, `SELECT account_id, model, COALESCE(SUM(success_requests),0)
		FROM usage_account_hourly_rollups
		WHERE bucket_start >= $1`+idFilter+`
		GROUP BY account_id, model`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var accountID, count int64
		var model string
		if err := rows.Scan(&accountID, &model, &count); err != nil {
			return err
		}
		if item := result[accountID]; item != nil && count > 0 {
			if item.SuccessModelCounts == nil {
				item.SuccessModelCounts = make(map[string]int64)
			}
			item.SuccessModelCounts[model] += count
		}
	}
	return rows.Err()
}
