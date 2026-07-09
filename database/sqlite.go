package database

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

const (
	defaultSQLiteMaxOpenConns = 8
	maxSQLiteOpenConns        = 16
	sqliteBusyTimeoutMillis   = 15000
)

func sqliteConnectDSN(dsn string) string {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" || dsn == ":memory:" {
		return dsn
	}

	q := url.Values{}
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", sqliteBusyTimeoutMillis))
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")

	if strings.HasPrefix(strings.ToLower(dsn), "file:") {
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		return dsn + sep + q.Encode()
	}

	return "file:" + dsn + "?" + q.Encode()
}

func applySQLiteConnLimits(conn *sql.DB, n int) {
	if conn == nil {
		return
	}
	if n <= 0 {
		n = defaultSQLiteMaxOpenConns
	}
	if n > maxSQLiteOpenConns {
		n = maxSQLiteOpenConns
	}
	if n < 2 {
		n = 2
	}
	conn.SetMaxOpenConns(n)
	conn.SetMaxIdleConns(n)
	conn.SetConnMaxLifetime(0)
}

func (db *DB) withSQLiteWriteLock(ctx context.Context, fn func() error) error {
	if !db.isSQLite() || db.sqliteWriteSem == nil {
		return fn()
	}
	select {
	case db.sqliteWriteSem <- struct{}{}:
		defer func() { <-db.sqliteWriteSem }()
		return fn()
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (db *DB) configureSQLite(ctx context.Context) error {
	pragmas := []string{
		fmt.Sprintf(`PRAGMA busy_timeout=%d;`, sqliteBusyTimeoutMillis),
		`PRAGMA journal_mode=WAL;`,
		`PRAGMA synchronous=NORMAL;`,
	}
	for _, pragma := range pragmas {
		if _, err := db.conn.ExecContext(ctx, pragma); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) migrateSQLite(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS accounts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT DEFAULT '',
			platform TEXT DEFAULT 'openai',
			type TEXT DEFAULT 'oauth',
			credentials TEXT NOT NULL DEFAULT '{}',
			proxy_url TEXT DEFAULT '',
			status TEXT DEFAULT 'active',
			cooldown_reason TEXT DEFAULT '',
			cooldown_until TIMESTAMP NULL,
			score_bias_override INTEGER NULL,
			base_concurrency_override INTEGER NULL,
			skip_warm_tier INTEGER DEFAULT 0,
			error_message TEXT DEFAULT '',
			deleted_at TIMESTAMP NULL,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		);`,
		`CREATE TABLE IF NOT EXISTS usage_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			account_id INTEGER DEFAULT 0,
			client_ip TEXT DEFAULT '',
			endpoint TEXT DEFAULT '',
			model TEXT DEFAULT '',
			prompt_tokens INTEGER DEFAULT 0,
			completion_tokens INTEGER DEFAULT 0,
			total_tokens INTEGER DEFAULT 0,
			status_code INTEGER DEFAULT 0,
			duration_ms INTEGER DEFAULT 0,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			input_tokens INTEGER DEFAULT 0,
			output_tokens INTEGER DEFAULT 0,
			reasoning_tokens INTEGER DEFAULT 0,
			first_token_ms INTEGER DEFAULT 0,
			reasoning_effort TEXT DEFAULT '',
			effective_model TEXT DEFAULT '',
			inbound_endpoint TEXT DEFAULT '',
			upstream_endpoint TEXT DEFAULT '',
				stream INTEGER DEFAULT 0,
				compact INTEGER DEFAULT 0,
				via_websocket INTEGER DEFAULT 0,
				cached_tokens INTEGER DEFAULT 0,
				service_tier TEXT DEFAULT '',
				requested_service_tier TEXT DEFAULT '',
				actual_service_tier TEXT DEFAULT '',
				billing_service_tier TEXT DEFAULT '',
				api_key_id INTEGER DEFAULT 0,
			api_key_name TEXT DEFAULT '',
			api_key_masked TEXT DEFAULT '',
			image_count INTEGER DEFAULT 0,
			image_width INTEGER DEFAULT 0,
			image_height INTEGER DEFAULT 0,
			image_bytes INTEGER DEFAULT 0,
			image_format TEXT DEFAULT '',
			image_size TEXT DEFAULT '',
			error_message TEXT DEFAULT ''
		);`,
		`CREATE TABLE IF NOT EXISTS api_keys (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT DEFAULT '',
			key TEXT NOT NULL UNIQUE,
			quota_limit REAL DEFAULT 0,
			quota_used REAL DEFAULT 0,
			total_used REAL DEFAULT 0,
			reset_count INTEGER DEFAULT 0,
			last_reset_at TIMESTAMP NULL,
			allowed_group_ids TEXT DEFAULT '[]',
			expires_at TIMESTAMP NULL,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		);`,
		`CREATE TABLE IF NOT EXISTS account_groups (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT UNIQUE NOT NULL,
			description TEXT DEFAULT '',
			color TEXT DEFAULT '',
			sort_order INTEGER DEFAULT 0,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		);`,
		`CREATE TABLE IF NOT EXISTS account_group_members (
			account_id INTEGER NOT NULL,
			group_id INTEGER NOT NULL,
			PRIMARY KEY (account_id, group_id)
		);`,
		`CREATE TABLE IF NOT EXISTS account_model_cooldowns (
			account_id INTEGER NOT NULL,
			model TEXT NOT NULL,
			reason TEXT DEFAULT '',
			reset_at TIMESTAMP NOT NULL,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (account_id, model)
		);`,
		`CREATE TABLE IF NOT EXISTS system_settings (
					id INTEGER PRIMARY KEY DEFAULT 1 CHECK (id = 1),
					site_name TEXT DEFAULT 'CodexProxy',
					site_logo TEXT DEFAULT '',
					background_config TEXT DEFAULT '{}',
					max_concurrency INTEGER DEFAULT 2,
				global_rpm INTEGER DEFAULT 0,
				test_model TEXT DEFAULT 'gpt-5.4',
				test_content TEXT DEFAULT 'hi',
				test_concurrency INTEGER DEFAULT 50,
				proxy_url TEXT DEFAULT '',
				pg_max_conns INTEGER DEFAULT 50,
				redis_pool_size INTEGER DEFAULT 30,
				auto_clean_unauthorized INTEGER DEFAULT 0,
				auto_clean_rate_limited INTEGER DEFAULT 0,
				background_refresh_interval_minutes INTEGER DEFAULT 2,
					usage_probe_max_age_minutes INTEGER DEFAULT 10,
					usage_probe_concurrency INTEGER DEFAULT 16,
					usage_probe_responses_fallback_enabled INTEGER DEFAULT 1,
					recovery_probe_interval_minutes INTEGER DEFAULT 30,
				admin_secret TEXT DEFAULT '',
				auto_clean_full_usage INTEGER DEFAULT 0,
				auto_clean_error INTEGER DEFAULT 0,
				auto_clean_expired INTEGER DEFAULT 0,
				lazy_mode INTEGER DEFAULT 0,
				proxy_pool_enabled INTEGER DEFAULT 0,
				fast_scheduler_enabled INTEGER DEFAULT 0,
				max_retries INTEGER DEFAULT 2,
				max_rate_limit_retries INTEGER DEFAULT 1,
				reasoning_effort_models TEXT DEFAULT '[]',
				allow_remote_migration INTEGER DEFAULT 0,
				client_compat_mode TEXT DEFAULT 'preserve',
				codex_min_cli_version TEXT DEFAULT '0.118.0',
				codex_user_agent_config TEXT DEFAULT '{}',
				usage_log_mode TEXT DEFAULT 'full',
				usage_log_batch_size INTEGER DEFAULT 200,
				usage_log_flush_interval_seconds INTEGER DEFAULT 5,
				stream_flush_policy TEXT DEFAULT 'immediate',
				stream_flush_interval_ms INTEGER DEFAULT 20,
				first_token_mode TEXT DEFAULT 'strict',
				first_token_timeout_seconds INTEGER DEFAULT 0,
				image_storage_config TEXT DEFAULT '{}',
				show_full_usage_numbers INTEGER DEFAULT 0,
				public_key_usage_page_enabled INTEGER DEFAULT 1,
				scheduler_mode TEXT DEFAULT 'round_robin',
					affinity_mode TEXT DEFAULT 'bounded',
					codex_force_websocket INTEGER DEFAULT 0,
					codex_ws_keepalive_enabled INTEGER DEFAULT 0,
					codex_ws_keepalive_interval_sec INTEGER DEFAULT 60,
					codex_ws_hide_upstream_errors INTEGER DEFAULT 1,
					codex_ws_silent_retry_enabled INTEGER DEFAULT 1,
					codex_ws_silent_max_retries INTEGER DEFAULT 2,
					codex_continue_thinking_enabled INTEGER DEFAULT 0,
					codex_continue_max_rounds INTEGER DEFAULT 8,
					retry_interval_ms INTEGER DEFAULT 0,
					transport_retry_policy TEXT DEFAULT 'rotate'
				);`,
		`CREATE TABLE IF NOT EXISTS model_registry (
			id TEXT PRIMARY KEY,
			enabled INTEGER DEFAULT 1,
			category TEXT DEFAULT 'codex',
			source TEXT DEFAULT 'manual',
			pro_only INTEGER DEFAULT 0,
			api_key_auth_available INTEGER DEFAULT 1,
			last_seen_at TIMESTAMP NULL,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		);`,
		`CREATE TABLE IF NOT EXISTS model_registry_sync (
			id INTEGER PRIMARY KEY DEFAULT 1 CHECK (id = 1),
			source_url TEXT DEFAULT '',
			last_synced_at TIMESTAMP NULL
		);`,
		`CREATE TABLE IF NOT EXISTS proxies (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			url TEXT NOT NULL UNIQUE,
			label TEXT DEFAULT '',
			enabled INTEGER DEFAULT 1,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			test_ip TEXT DEFAULT '',
			test_location TEXT DEFAULT '',
			test_latency_ms INTEGER DEFAULT 0
		);`,
		`CREATE TABLE IF NOT EXISTS account_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			account_id INTEGER NOT NULL DEFAULT 0,
			event_type TEXT NOT NULL,
			source TEXT DEFAULT '',
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		);`,
		`CREATE TABLE IF NOT EXISTS image_prompt_templates (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL DEFAULT '',
			prompt TEXT NOT NULL DEFAULT '',
			model TEXT DEFAULT '',
			size TEXT DEFAULT '',
			quality TEXT DEFAULT '',
			output_format TEXT DEFAULT '',
			background TEXT DEFAULT '',
			style TEXT DEFAULT '',
			tags TEXT NOT NULL DEFAULT '[]',
			favorite INTEGER DEFAULT 0,
			usage_count INTEGER DEFAULT 0,
			last_used_at TIMESTAMP NULL,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		);`,
		`CREATE TABLE IF NOT EXISTS image_generation_jobs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			status TEXT NOT NULL DEFAULT 'queued',
			prompt TEXT NOT NULL DEFAULT '',
			params_json TEXT NOT NULL DEFAULT '{}',
			api_key_id INTEGER DEFAULT 0,
			api_key_name TEXT DEFAULT '',
			api_key_masked TEXT DEFAULT '',
			error_message TEXT DEFAULT '',
			duration_ms INTEGER DEFAULT 0,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			started_at TIMESTAMP NULL,
			completed_at TIMESTAMP NULL
		);`,
		`CREATE TABLE IF NOT EXISTS image_assets (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			job_id INTEGER NOT NULL DEFAULT 0,
			template_id INTEGER DEFAULT 0,
			filename TEXT NOT NULL DEFAULT '',
			storage_path TEXT NOT NULL DEFAULT '',
			mime_type TEXT NOT NULL DEFAULT '',
			bytes INTEGER DEFAULT 0,
			width INTEGER DEFAULT 0,
			height INTEGER DEFAULT 0,
			model TEXT DEFAULT '',
			requested_size TEXT DEFAULT '',
			actual_size TEXT DEFAULT '',
			quality TEXT DEFAULT '',
			output_format TEXT DEFAULT '',
			revised_prompt TEXT DEFAULT '',
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		);`,
		`CREATE TABLE IF NOT EXISTS prompt_filter_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			source TEXT DEFAULT '',
			endpoint TEXT DEFAULT '',
			model TEXT DEFAULT '',
			action TEXT DEFAULT '',
			mode TEXT DEFAULT '',
			score INTEGER DEFAULT 0,
			threshold_value INTEGER DEFAULT 0,
			matched_patterns TEXT DEFAULT '[]',
			text_preview TEXT DEFAULT '',
			api_key_id INTEGER DEFAULT 0,
			api_key_name TEXT DEFAULT '',
			api_key_masked TEXT DEFAULT '',
			client_ip TEXT DEFAULT '',
			error_code TEXT DEFAULT '',
			review_model TEXT DEFAULT '',
			review_flagged INTEGER DEFAULT 0,
			review_error TEXT DEFAULT '',
			full_text TEXT DEFAULT ''
		);`,
	}
	for _, stmt := range statements {
		if _, err := db.conn.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}

	columns := []struct {
		table string
		name  string
		def   string
	}{
		{"accounts", "cooldown_reason", "TEXT DEFAULT ''"},
		{"accounts", "cooldown_until", "TIMESTAMP NULL"},
		{"accounts", "score_bias_override", "INTEGER NULL"},
		{"accounts", "base_concurrency_override", "INTEGER NULL"},
		{"accounts", "tags", "TEXT DEFAULT '[]'"},
		{"accounts", "deleted_at", "TIMESTAMP NULL"},
		{"usage_logs", "input_tokens", "INTEGER DEFAULT 0"},
		{"usage_logs", "output_tokens", "INTEGER DEFAULT 0"},
		{"usage_logs", "reasoning_tokens", "INTEGER DEFAULT 0"},
		{"usage_logs", "first_token_ms", "INTEGER DEFAULT 0"},
		{"usage_logs", "reasoning_effort", "TEXT DEFAULT ''"},
		{"usage_logs", "effective_model", "TEXT DEFAULT ''"},
		{"usage_logs", "inbound_endpoint", "TEXT DEFAULT ''"},
		{"usage_logs", "upstream_endpoint", "TEXT DEFAULT ''"},
		{"usage_logs", "stream", "INTEGER DEFAULT 0"},
		{"usage_logs", "via_websocket", "INTEGER DEFAULT 0"},
		{"usage_logs", "compact", "INTEGER DEFAULT 0"},
		{"usage_logs", "cached_tokens", "INTEGER DEFAULT 0"},
		{"usage_logs", "service_tier", "TEXT DEFAULT ''"},
		{"usage_logs", "requested_service_tier", "TEXT DEFAULT ''"},
		{"usage_logs", "actual_service_tier", "TEXT DEFAULT ''"},
		{"usage_logs", "billing_service_tier", "TEXT DEFAULT ''"},
		{"usage_logs", "api_key_id", "INTEGER DEFAULT 0"},
		{"usage_logs", "api_key_name", "TEXT DEFAULT ''"},
		{"usage_logs", "api_key_masked", "TEXT DEFAULT ''"},
		{"usage_logs", "client_ip", "TEXT DEFAULT ''"},
		{"usage_logs", "image_count", "INTEGER DEFAULT 0"},
		{"usage_logs", "image_width", "INTEGER DEFAULT 0"},
		{"usage_logs", "image_height", "INTEGER DEFAULT 0"},
		{"usage_logs", "image_bytes", "INTEGER DEFAULT 0"},
		{"usage_logs", "image_format", "TEXT DEFAULT ''"},
		{"usage_logs", "image_size", "TEXT DEFAULT ''"},
		{"usage_logs", "account_billed", "REAL DEFAULT 0"},
		{"usage_logs", "user_billed", "REAL DEFAULT 0"},
		{"usage_logs", "is_retry_attempt", "INTEGER DEFAULT 0"},
		{"usage_logs", "attempt_index", "INTEGER DEFAULT 0"},
		{"usage_logs", "upstream_error_kind", "TEXT DEFAULT ''"},
		{"usage_logs", "error_message", "TEXT DEFAULT ''"},
		{"api_keys", "quota_limit", "REAL DEFAULT 0"},
		{"api_keys", "quota_used", "REAL DEFAULT 0"},
		{"api_keys", "total_used", "REAL DEFAULT 0"},
		{"api_keys", "reset_count", "INTEGER DEFAULT 0"},
		{"api_keys", "last_reset_at", "TIMESTAMP NULL"},
		{"api_keys", "allowed_group_ids", "TEXT DEFAULT '[]'"},
		{"api_keys", "limits", "TEXT DEFAULT '{}'"},
		{"api_keys", "expires_at", "TIMESTAMP NULL"},
		{"account_groups", "description", "TEXT DEFAULT ''"},
		{"account_groups", "color", "TEXT DEFAULT ''"},
		{"account_groups", "sort_order", "INTEGER DEFAULT 0"},
		{"account_groups", "created_at", "TIMESTAMP DEFAULT CURRENT_TIMESTAMP"},
		{"account_groups", "updated_at", "TIMESTAMP DEFAULT CURRENT_TIMESTAMP"},
		{"system_settings", "site_name", "TEXT DEFAULT 'CodexProxy'"},
		{"system_settings", "site_logo", "TEXT DEFAULT ''"},
		{"system_settings", "background_config", "TEXT DEFAULT '{}'"},
		{"system_settings", "test_content", "TEXT DEFAULT 'hi'"},
		{"system_settings", "pg_max_conns", "INTEGER DEFAULT 50"},
		{"system_settings", "redis_pool_size", "INTEGER DEFAULT 30"},
		{"system_settings", "auto_clean_unauthorized", "INTEGER DEFAULT 0"},
		{"system_settings", "auto_clean_rate_limited", "INTEGER DEFAULT 0"},
		{"system_settings", "background_refresh_interval_minutes", "INTEGER DEFAULT 2"},
		{"system_settings", "usage_probe_max_age_minutes", "INTEGER DEFAULT 10"},
		{"system_settings", "usage_probe_concurrency", "INTEGER DEFAULT 16"},
		{"system_settings", "usage_probe_responses_fallback_enabled", "INTEGER DEFAULT 1"},
		{"system_settings", "recovery_probe_interval_minutes", "INTEGER DEFAULT 30"},
		{"system_settings", "admin_secret", "TEXT DEFAULT ''"},
		{"system_settings", "auto_clean_full_usage", "INTEGER DEFAULT 0"},
		{"system_settings", "auto_clean_error", "INTEGER DEFAULT 0"},
		{"system_settings", "auto_clean_expired", "INTEGER DEFAULT 0"},
		{"system_settings", "lazy_mode", "INTEGER DEFAULT 0"},
		{"system_settings", "proxy_pool_enabled", "INTEGER DEFAULT 0"},
		{"system_settings", "fast_scheduler_enabled", "INTEGER DEFAULT 0"},
		{"system_settings", "codex_force_websocket", "INTEGER DEFAULT 0"},
		{"system_settings", "codex_ws_keepalive_enabled", "INTEGER DEFAULT 0"},
		{"system_settings", "codex_ws_keepalive_interval_sec", "INTEGER DEFAULT 60"},
		{"system_settings", "codex_ws_hide_upstream_errors", "INTEGER DEFAULT 1"},
		{"system_settings", "codex_ws_silent_retry_enabled", "INTEGER DEFAULT 1"},
		{"system_settings", "codex_ws_silent_max_retries", "INTEGER DEFAULT 2"},
		{"system_settings", "codex_continue_thinking_enabled", "INTEGER DEFAULT 0"},
		{"system_settings", "codex_continue_max_rounds", "INTEGER DEFAULT 8"},
		{"system_settings", "retry_interval_ms", "INTEGER DEFAULT 0"},
		{"system_settings", "transport_retry_policy", "TEXT DEFAULT 'rotate'"},
		{"system_settings", "max_retries", "INTEGER DEFAULT 2"},
		{"system_settings", "max_rate_limit_retries", "INTEGER DEFAULT 1"},
		{"system_settings", "allow_remote_migration", "INTEGER DEFAULT 0"},
		{"system_settings", "model_mapping", "TEXT DEFAULT '{}'"},
		{"system_settings", "codex_model_mapping", "TEXT DEFAULT '{}'"},
		{"system_settings", "reasoning_effort_models", "TEXT DEFAULT '[]'"},
		{"system_settings", "resin_url", "TEXT DEFAULT ''"},
		{"system_settings", "resin_platform_name", "TEXT DEFAULT ''"},
		{"system_settings", "prompt_filter_enabled", "INTEGER DEFAULT 0"},
		{"system_settings", "prompt_filter_mode", "TEXT DEFAULT 'monitor'"},
		{"system_settings", "prompt_filter_threshold", "INTEGER DEFAULT 50"},
		{"system_settings", "prompt_filter_strict_threshold", "INTEGER DEFAULT 90"},
		{"system_settings", "prompt_filter_log_matches", "INTEGER DEFAULT 1"},
		{"system_settings", "prompt_filter_max_text_length", "INTEGER DEFAULT 81920"},
		{"system_settings", "prompt_filter_sensitive_words", "TEXT DEFAULT ''"},
		{"system_settings", "prompt_filter_custom_patterns", "TEXT DEFAULT '[]'"},
		{"system_settings", "prompt_filter_disabled_patterns", "TEXT DEFAULT '[]'"},
		{"system_settings", "prompt_filter_review_enabled", "INTEGER DEFAULT 0"},
		{"system_settings", "prompt_filter_review_api_key", "TEXT DEFAULT ''"},
		{"system_settings", "prompt_filter_review_base_url", "TEXT DEFAULT 'https://api.openai.com'"},
		{"system_settings", "prompt_filter_review_model", "TEXT DEFAULT 'omni-moderation-latest'"},
		{"system_settings", "prompt_filter_review_timeout_seconds", "INTEGER DEFAULT 10"},
		{"system_settings", "prompt_filter_review_fail_closed", "INTEGER DEFAULT 1"},
		{"prompt_filter_logs", "review_model", "TEXT DEFAULT ''"},
		{"prompt_filter_logs", "review_flagged", "INTEGER DEFAULT 0"},
		{"prompt_filter_logs", "review_error", "TEXT DEFAULT ''"},
		{"prompt_filter_logs", "full_text", "TEXT DEFAULT ''"},
		{"system_settings", "client_compat_mode", "TEXT DEFAULT 'preserve'"},
		{"system_settings", "codex_min_cli_version", "TEXT DEFAULT '0.118.0'"},
		{"system_settings", "codex_user_agent_config", "TEXT DEFAULT '{}'"},
		{"system_settings", "usage_log_mode", "TEXT DEFAULT 'full'"},
		{"system_settings", "usage_log_batch_size", "INTEGER DEFAULT 200"},
		{"system_settings", "usage_log_flush_interval_seconds", "INTEGER DEFAULT 5"},
		{"system_settings", "stream_flush_policy", "TEXT DEFAULT 'immediate'"},
		{"system_settings", "stream_flush_interval_ms", "INTEGER DEFAULT 20"},
		{"system_settings", "first_token_mode", "TEXT DEFAULT 'strict'"},
		{"system_settings", "first_token_timeout_seconds", "INTEGER DEFAULT 0"},
		{"system_settings", "billing_tier_policy", "TEXT DEFAULT 'actual'"},
		{"system_settings", "image_storage_config", "TEXT DEFAULT '{}'"},
		{"system_settings", "show_full_usage_numbers", "INTEGER DEFAULT 0"},
		{"system_settings", "public_key_usage_page_enabled", "INTEGER DEFAULT 1"},
		{"system_settings", "scheduler_mode", "TEXT DEFAULT 'round_robin'"},
		{"system_settings", "affinity_mode", "TEXT DEFAULT 'bounded'"},
		{"system_settings", "auto_pause_5h_threshold", "REAL DEFAULT 0"},
		{"system_settings", "auto_pause_7d_threshold", "REAL DEFAULT 0"},
		{"system_settings", "auto_pause_5h_guard_band_percent", "REAL DEFAULT 5"},
		{"system_settings", "auto_pause_5h_guard_concurrency", "INTEGER DEFAULT 1"},
		{"system_settings", "smart_pacing_enabled", "BOOLEAN DEFAULT 0"},
		{"system_settings", "smart_pacing_min_concurrency", "INTEGER DEFAULT 1"},
		{"system_settings", "smart_pacing_windows", "TEXT DEFAULT '5h,7d'"},
		{"account_groups", "auto_pause_5h_threshold", "REAL DEFAULT 0"},
		{"account_groups", "auto_pause_7d_threshold", "REAL DEFAULT 0"},
		{"accounts", "enabled", "INTEGER DEFAULT 1"},
		{"accounts", "locked", "INTEGER DEFAULT 0"},
		{"accounts", "credit_enabled", "INTEGER DEFAULT 0"},
		{"accounts", "credit_skip_usage_window", "INTEGER DEFAULT 0"},
		{"accounts", "skip_warm_tier", "INTEGER DEFAULT 0"},
		{"accounts", "image_quota_remaining", "INTEGER NULL"},
		{"accounts", "image_quota_total", "INTEGER NULL"},
		{"accounts", "today_used_count", "INTEGER DEFAULT 0"},
		{"accounts", "image_quota_reset_at", "TEXT NULL"},
		{"proxies", "test_ip", "TEXT DEFAULT ''"},
		{"proxies", "test_location", "TEXT DEFAULT ''"},
		{"proxies", "test_latency_ms", "INTEGER DEFAULT 0"},
	}
	for _, column := range columns {
		if err := db.ensureSQLiteColumn(ctx, column.table, column.name, column.def); err != nil {
			return err
		}
	}

	indexStatements := []string{
		`CREATE INDEX IF NOT EXISTS idx_accounts_status ON accounts(status);`,
		`CREATE INDEX IF NOT EXISTS idx_accounts_platform ON accounts(platform);`,
		`CREATE INDEX IF NOT EXISTS idx_accounts_cooldown_until ON accounts(cooldown_until);`,
		`CREATE INDEX IF NOT EXISTS idx_usage_logs_created_at ON usage_logs(created_at);`,
		`CREATE INDEX IF NOT EXISTS idx_usage_logs_account_id ON usage_logs(account_id);`,
		`CREATE INDEX IF NOT EXISTS idx_usage_logs_account_created_at ON usage_logs(account_id, created_at);`,
		`CREATE INDEX IF NOT EXISTS idx_usage_logs_created_status ON usage_logs(created_at, status_code);`,
		`CREATE INDEX IF NOT EXISTS idx_usage_logs_account_status ON usage_logs(account_id, status_code);`,
		`CREATE INDEX IF NOT EXISTS idx_usage_logs_api_key_created_at ON usage_logs(api_key_id, created_at);`,
		`CREATE INDEX IF NOT EXISTS idx_api_keys_expires_at ON api_keys(expires_at);`,
		`CREATE INDEX IF NOT EXISTS idx_account_group_members_group ON account_group_members(group_id);`,
		`CREATE INDEX IF NOT EXISTS idx_account_group_members_account ON account_group_members(account_id);`,
		`CREATE INDEX IF NOT EXISTS idx_account_model_cooldowns_reset_at ON account_model_cooldowns(reset_at);`,
		`CREATE INDEX IF NOT EXISTS idx_account_events_created ON account_events(created_at);`,
		`CREATE INDEX IF NOT EXISTS idx_account_events_type_created ON account_events(event_type, created_at);`,
		`CREATE INDEX IF NOT EXISTS idx_image_prompt_templates_updated ON image_prompt_templates(updated_at);`,
		`CREATE INDEX IF NOT EXISTS idx_image_prompt_templates_favorite ON image_prompt_templates(favorite, updated_at);`,
		`CREATE INDEX IF NOT EXISTS idx_image_generation_jobs_created ON image_generation_jobs(created_at);`,
		`CREATE INDEX IF NOT EXISTS idx_image_generation_jobs_status ON image_generation_jobs(status, created_at);`,
		`CREATE INDEX IF NOT EXISTS idx_image_assets_created ON image_assets(created_at);`,
		`CREATE INDEX IF NOT EXISTS idx_image_assets_job_id ON image_assets(job_id);`,
		`CREATE INDEX IF NOT EXISTS idx_prompt_filter_logs_created_at ON prompt_filter_logs(created_at);`,
		`CREATE INDEX IF NOT EXISTS idx_prompt_filter_logs_action_created_at ON prompt_filter_logs(action, created_at);`,
	}
	for _, stmt := range indexStatements {
		if _, err := db.conn.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}

	if _, err := db.conn.ExecContext(ctx, `
		UPDATE accounts
		SET status = 'deleted',
			error_message = '',
			cooldown_reason = '',
			cooldown_until = NULL,
			deleted_at = COALESCE(deleted_at, updated_at, CURRENT_TIMESTAMP),
			updated_at = CURRENT_TIMESTAMP
		WHERE status <> 'deleted' AND COALESCE(error_message, '') = 'deleted'
	`); err != nil {
		return err
	}

	return db.runDataMigrationsWithTimeout()
}

func (db *DB) ensureSQLiteColumn(ctx context.Context, table string, name string, columnDef string) error {
	columns, err := db.sqliteTableColumns(ctx, table)
	if err != nil {
		return err
	}
	if _, ok := columns[name]; ok {
		return nil
	}
	_, err = db.conn.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, name, columnDef))
	return err
}

func (db *DB) sqliteTableColumns(ctx context.Context, table string) (map[string]struct{}, error) {
	rows, err := db.conn.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]struct{})
	for rows.Next() {
		var (
			cid        int
			name       string
			columnType string
			notNull    int
			defaultVal sql.NullString
			pk         int
		)
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultVal, &pk); err != nil {
			return nil, err
		}
		result[name] = struct{}{}
	}
	return result, rows.Err()
}

func (db *DB) getTrafficSnapshotSQLite(ctx context.Context) (*TrafficSnapshot, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT created_at, total_tokens
		FROM usage_logs
		WHERE created_at >= $1
	`, db.timeArg(time.Now().Add(-5*time.Minute)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	perSecondRequests := make(map[int64]float64)
	perSecondTokens := make(map[int64]float64)
	now := time.Now()
	windowStart := now.Add(-10 * time.Second).Unix()

	for rows.Next() {
		var createdRaw interface{}
		var totalTokens int64
		if err := rows.Scan(&createdRaw, &totalTokens); err != nil {
			return nil, err
		}
		createdAt, err := parseDBTimeValue(createdRaw)
		if err != nil || createdAt.IsZero() {
			continue
		}
		sec := createdAt.Unix()
		perSecondRequests[sec]++
		perSecondTokens[sec] += float64(totalTokens)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	result := &TrafficSnapshot{}
	var qpsPeak float64
	var tpsPeak float64
	var qpsWindow float64
	var tpsWindow float64
	for sec, reqCount := range perSecondRequests {
		if reqCount > qpsPeak {
			qpsPeak = reqCount
		}
		tokenCount := perSecondTokens[sec]
		if tokenCount > tpsPeak {
			tpsPeak = tokenCount
		}
		if sec >= windowStart {
			qpsWindow += reqCount
			tpsWindow += tokenCount
		}
	}
	result.QPS = qpsWindow / 10.0
	result.TPS = tpsWindow / 10.0
	result.QPSPeak = qpsPeak
	result.TPSPeak = tpsPeak
	return result, nil
}

func (db *DB) getChartAggregationSQLite(ctx context.Context, start, end time.Time, bucketMinutes int) (*ChartAggregation, error) {
	startArg, endArg := db.timeRangeArgs(start, end)
	rows, err := db.conn.QueryContext(ctx, `
		SELECT created_at, duration_ms, input_tokens, output_tokens, reasoning_tokens, cached_tokens, model, status_code
		FROM usage_logs
		WHERE created_at >= $1 AND created_at <= $2
		  AND status_code <> 499
	`, startArg, endArg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type bucketAgg struct {
		requests        int64
		totalLatency    float64
		inputTokens     int64
		outputTokens    int64
		reasoningTokens int64
		cachedTokens    int64
		errors4xx       int64
		errors5xx       int64
	}

	result := &ChartAggregation{}
	timelineMap := make(map[string]*bucketAgg)
	modelMap := make(map[string]int64)

	for rows.Next() {
		var createdRaw interface{}
		var durationMs int
		var inputTokens int64
		var outputTokens int64
		var reasoningTokens int64
		var cachedTokens int64
		var model sql.NullString
		var statusCode int
		if err := rows.Scan(&createdRaw, &durationMs, &inputTokens, &outputTokens, &reasoningTokens, &cachedTokens, &model, &statusCode); err != nil {
			return nil, err
		}
		createdAt, err := parseDBTimeValue(createdRaw)
		if err != nil || createdAt.IsZero() {
			continue
		}

		bucket := createdAt.Truncate(time.Duration(bucketMinutes) * time.Minute).Format("2006-01-02T15:04:05")
		agg, ok := timelineMap[bucket]
		if !ok {
			agg = &bucketAgg{}
			timelineMap[bucket] = agg
		}
		agg.requests++
		agg.totalLatency += float64(durationMs)
		agg.inputTokens += inputTokens
		agg.outputTokens += outputTokens
		agg.reasoningTokens += reasoningTokens
		agg.cachedTokens += cachedTokens
		if statusCode >= 400 && statusCode < 500 {
			agg.errors4xx++
		}
		if statusCode >= 500 && statusCode < 600 {
			agg.errors5xx++
		}

		modelName := "unknown"
		if model.Valid && model.String != "" {
			modelName = model.String
		}
		modelMap[modelName]++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	keys := make([]string, 0, len(timelineMap))
	for key := range timelineMap {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		agg := timelineMap[key]
		avgLatency := 0.0
		if agg.requests > 0 {
			avgLatency = agg.totalLatency / float64(agg.requests)
		}
		result.Timeline = append(result.Timeline, ChartTimelinePoint{
			Bucket:          key,
			Requests:        agg.requests,
			AvgLatency:      avgLatency,
			InputTokens:     agg.inputTokens,
			OutputTokens:    agg.outputTokens,
			ReasoningTokens: agg.reasoningTokens,
			CachedTokens:    agg.cachedTokens,
			Errors4xx:       agg.errors4xx,
			Errors5xx:       agg.errors5xx,
		})
	}
	if result.Timeline == nil {
		result.Timeline = []ChartTimelinePoint{}
	}

	type modelAgg struct {
		model    string
		requests int64
	}
	models := make([]modelAgg, 0, len(modelMap))
	for model, requests := range modelMap {
		models = append(models, modelAgg{model: model, requests: requests})
	}
	sort.Slice(models, func(i, j int) bool {
		if models[i].requests == models[j].requests {
			return models[i].model < models[j].model
		}
		return models[i].requests > models[j].requests
	})
	if len(models) > 10 {
		models = models[:10]
	}
	for _, model := range models {
		result.Models = append(result.Models, ChartModelPoint{
			Model:    model.model,
			Requests: model.requests,
		})
	}
	if result.Models == nil {
		result.Models = []ChartModelPoint{}
	}

	return result, nil
}

// getAccountEventTrendSQLite SQLite 版账号事件趋势聚合（内存分桶）
func (db *DB) getAccountEventTrendSQLite(ctx context.Context, start, end time.Time, bucketMinutes int) ([]AccountEventPoint, error) {
	if bucketMinutes < 1 {
		bucketMinutes = 60
	}

	startArg, endArg := db.timeRangeArgs(start, end)
	rows, err := db.conn.QueryContext(ctx,
		`SELECT created_at, event_type, source FROM account_events WHERE created_at >= $1 AND created_at <= $2`,
		startArg, endArg,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type bucketAgg struct {
		added   int
		deleted int
	}
	bucketMap := make(map[string]*bucketAgg)

	for rows.Next() {
		var createdRaw interface{}
		var eventType, source string
		if err := rows.Scan(&createdRaw, &eventType, &source); err != nil {
			return nil, err
		}
		createdAt, err := parseDBTimeValue(createdRaw)
		if err != nil || createdAt.IsZero() {
			continue
		}

		// 只统计用户操作：added 全部计入，deleted 只计 manual
		if eventType == "deleted" && source != "manual" {
			continue
		}

		// 对齐到桶
		minute := createdAt.Minute()
		aligned := minute - (minute % bucketMinutes)
		bucketTime := time.Date(createdAt.Year(), createdAt.Month(), createdAt.Day(),
			createdAt.Hour(), aligned, 0, 0, createdAt.Location())
		key := bucketTime.Format("2006-01-02T15:04:05")

		agg, ok := bucketMap[key]
		if !ok {
			agg = &bucketAgg{}
			bucketMap[key] = agg
		}
		switch eventType {
		case "added":
			agg.added++
		case "deleted":
			agg.deleted++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 排序输出
	keys := make([]string, 0, len(bucketMap))
	for k := range bucketMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	result := make([]AccountEventPoint, 0, len(keys))
	for _, k := range keys {
		agg := bucketMap[k]
		result = append(result, AccountEventPoint{Bucket: k, Added: agg.added, Deleted: agg.deleted})
	}
	return result, nil
}

// getUsageStatsSQLite SQLite 版使用统计（内存聚合，避免 PG 特有语法）。
// rangeStart 为零值时回落到"今日"(本地 0 点起);rangeEnd 为零值表示至今。
func (db *DB) getUsageStatsSQLite(ctx context.Context, rangeStart, rangeEnd time.Time) (*UsageStats, error) {
	now := time.Now()
	if rangeStart.IsZero() {
		rangeStart = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	}
	minuteAgo := now.Add(-1 * time.Minute)

	query := `
			SELECT created_at, total_tokens, prompt_tokens, completion_tokens,
			       cached_tokens, first_token_ms, duration_ms, status_code, account_billed, user_billed
			FROM usage_logs
			WHERE created_at >= $1 AND status_code <> 499
		`
	args := []interface{}{db.timeArg(rangeStart)}
	if !rangeEnd.IsZero() {
		query += " AND created_at < $2"
		args = append(args, db.timeArg(rangeEnd))
	}

	rows, err := db.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	stats := &UsageStats{}
	var todayErrors int64
	var totalDuration float64
	var totalFirstTokenMs float64
	var totalFirstTokenSamples int64
	var todayCacheHitRequests int64

	for rows.Next() {
		var createdRaw interface{}
		var totalTokens, promptTokens, completionTokens, cachedTokens int64
		var firstTokenMs, durationMs int
		var statusCode int
		var accountBilled, userBilled float64
		if err := rows.Scan(&createdRaw, &totalTokens, &promptTokens, &completionTokens,
			&cachedTokens, &firstTokenMs, &durationMs, &statusCode, &accountBilled, &userBilled); err != nil {
			return nil, err
		}
		createdAt, err := parseDBTimeValue(createdRaw)
		if err != nil || createdAt.IsZero() {
			continue
		}

		stats.TodayRequests++
		stats.TodayTokens += totalTokens
		stats.TodayPrompt += promptTokens
		stats.TodayCompletion += completionTokens
		stats.TotalCachedTokens += cachedTokens
		stats.TodayCachedTokens += cachedTokens
		stats.TodayAccountBilled += accountBilled
		stats.TodayUserBilled += userBilled
		totalDuration += float64(durationMs)
		if firstTokenMs > 0 {
			totalFirstTokenMs += float64(firstTokenMs)
			totalFirstTokenSamples++
		}
		if cachedTokens > 0 {
			todayCacheHitRequests++
		}

		if statusCode >= 400 {
			todayErrors++
		}
		// 最近 1 分钟窗口：RPM / TPM
		if !createdAt.Before(minuteAgo) {
			stats.RPM++
			stats.TPM += float64(totalTokens)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if stats.TodayRequests > 0 {
		stats.AvgDurationMs = totalDuration / float64(stats.TodayRequests)
		stats.ErrorRate = float64(todayErrors) / float64(stats.TodayRequests) * 100
		stats.TodayCacheRate = float64(todayCacheHitRequests) / float64(stats.TodayRequests) * 100
	}
	if totalFirstTokenSamples > 0 {
		stats.AvgFirstTokenMs = totalFirstTokenMs / float64(totalFirstTokenSamples)
	}

	// 可见请求总数（排除 499）
	var visibleTotal, visibleCacheHitRequests, visibleFirstTokenSamples int64
	var currentTokens, currentPrompt, currentCompletion, currentCached int64
	var currentFirstTokenMsSum float64
	var currentAccountBilled, currentUserBilled float64
	_ = db.conn.QueryRowContext(ctx, `
		SELECT
			COUNT(*),
			COALESCE(SUM(total_tokens), 0),
			COALESCE(SUM(prompt_tokens), 0),
			COALESCE(SUM(completion_tokens), 0),
			COALESCE(SUM(cached_tokens), 0),
			COALESCE(SUM(CASE WHEN cached_tokens > 0 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN first_token_ms > 0 THEN first_token_ms ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN first_token_ms > 0 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(account_billed), 0),
			COALESCE(SUM(user_billed), 0)
		FROM usage_logs
		WHERE status_code <> 499
	`).Scan(&visibleTotal, &currentTokens, &currentPrompt, &currentCompletion, &currentCached, &visibleCacheHitRequests, &currentFirstTokenMsSum, &visibleFirstTokenSamples, &currentAccountBilled, &currentUserBilled)

	// 基线值
	var bReq, bTok, bPrompt, bComp, bCached, bCacheHitRequests, bFirstTokenSamples int64
	var bFirstTokenMsSum float64
	var bAccountBilled, bUserBilled float64
	_ = db.conn.QueryRowContext(ctx, `
		SELECT total_requests, total_tokens, prompt_tokens, completion_tokens, cached_tokens, cache_hit_requests, first_token_ms_sum, first_token_samples, account_billed, user_billed
		FROM usage_stats_baseline WHERE id = 1
	`).Scan(&bReq, &bTok, &bPrompt, &bComp, &bCached, &bCacheHitRequests, &bFirstTokenMsSum, &bFirstTokenSamples, &bAccountBilled, &bUserBilled)

	stats.TotalRequests = visibleTotal + bReq
	stats.TotalTokens = currentTokens + bTok
	stats.TotalPrompt = currentPrompt + bPrompt
	stats.TotalCompletion = currentCompletion + bComp
	stats.TotalCachedTokens = currentCached + bCached
	stats.TotalAccountBilled = currentAccountBilled + bAccountBilled
	stats.TotalUserBilled = currentUserBilled + bUserBilled
	if stats.TotalRequests > 0 {
		stats.TotalCacheRate = float64(visibleCacheHitRequests+bCacheHitRequests) / float64(stats.TotalRequests) * 100
	}
	if visibleFirstTokenSamples+bFirstTokenSamples > 0 {
		stats.AvgFirstTokenMs = (currentFirstTokenMsSum + bFirstTokenMsSum) / float64(visibleFirstTokenSamples+bFirstTokenSamples)
	}
	if stats.TotalRequests > 0 {
		stats.AvgAccountBilled = stats.TotalAccountBilled / float64(stats.TotalRequests)
		stats.AvgUserBilled = stats.TotalUserBilled / float64(stats.TotalRequests)
	}
	stats.ModelStats, err = db.getUsageModelStats(ctx, 10, rangeStart, rangeEnd)
	if err != nil {
		return nil, err
	}
	if err := db.populateUsageBreakdownStats(ctx, stats, rangeStart, rangeEnd); err != nil {
		return nil, err
	}

	return stats, nil
}
