package database

import (
	"context"
	"database/sql"
	"fmt"
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

// ensureUsageAccountBillingWindowRollupsTable creates the exact-window billing
// snapshots used by the account list's local 5h/7d cost badges. The general
// usage archive is intentionally hourly, which is too coarse for an upstream
// quota reset that can happen at an arbitrary second. Keying the compact cost
// snapshot by the exact window start preserves both behaviours: clearing raw
// logs keeps the current badge value, while a new upstream reset start no
// longer matches the previous window and therefore starts again from zero.
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
	_, err = db.conn.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS idx_usage_account_billing_window_start ON usage_account_billing_window_rollups(window_start)`)
	return err
}

type accountBillingWindow struct {
	AccountID int64
	Since     time.Time
}

func collectAccountBillingWindows(windowSets []map[int64]time.Time) []accountBillingWindow {
	seen := make(map[string]struct{})
	windows := make([]accountBillingWindow, 0)
	for _, windowSet := range windowSets {
		for accountID, since := range windowSet {
			if accountID <= 0 || since.IsZero() {
				continue
			}
			since = since.UTC()
			key := fmt.Sprintf("%d:%d", accountID, since.UnixNano())
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			windows = append(windows, accountBillingWindow{AccountID: accountID, Since: since})
		}
	}
	return windows
}

// archiveAccountBillingWindowsWithExec snapshots only the exact active quota
// windows supplied by the runtime account store. Repeated clears add just the
// newly-created detail rows to the same window. Old window rows are retained
// but become unreachable as soon as the upstream reset advances the start.
func (db *DB) archiveAccountBillingWindowsWithExec(ctx context.Context, tx *sql.Tx, windowSets []map[int64]time.Time) error {
	windows := collectAccountBillingWindows(windowSets)
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
		args := make([]interface{}, 0, (end-start)*3)
		argIdx := 1
		for _, window := range windows[start:end] {
			if db.isSQLite() {
				values = append(values, fmt.Sprintf("($%d, $%d, $%d)", argIdx, argIdx+1, argIdx+2))
			} else {
				values = append(values, fmt.Sprintf("($%d::BIGINT, $%d::BIGINT, $%d::TIMESTAMPTZ)", argIdx, argIdx+1, argIdx+2))
			}
			args = append(args, window.AccountID, window.Since.UnixNano(), db.timeArg(window.Since))
			argIdx += 3
		}
		query := fmt.Sprintf(`WITH billing_windows(account_id, window_start, since_at) AS (VALUES %s)
			INSERT INTO usage_account_billing_window_rollups(account_id, window_start, account_billed)
			SELECT billing_windows.account_id, billing_windows.window_start,
				COALESCE(SUM(usage_logs.account_billed), 0)
			FROM billing_windows
			JOIN usage_logs ON usage_logs.account_id = billing_windows.account_id
				AND usage_logs.created_at >= billing_windows.since_at
				AND usage_logs.status_code <> 499
				AND TRIM(COALESCE(usage_logs.internal_reason, '')) = ''
			GROUP BY billing_windows.account_id, billing_windows.window_start
			ON CONFLICT (account_id, window_start) DO UPDATE SET
				account_billed = usage_account_billing_window_rollups.account_billed + excluded.account_billed`, strings.Join(values, ","))
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
