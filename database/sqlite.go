package database

import (
	"context"
	"database/sql"
	"errors"
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
	// deferred 事务（BeginTx 默认）从读锁升级写锁遇忙会立刻 SQLITE_BUSY，
	// busy_timeout 拦不住；immediate 让写事务在 BEGIN 就拿写锁、正常走等待。
	q.Add("_txlock", "immediate")
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

// withWriteTx serializes top-level SQLite mutations while preserving the
// normal transaction behavior for PostgreSQL. Callers pass all nested writes
// through the same transaction to avoid writer-gate re-entry deadlocks.
func (db *DB) withWriteTx(ctx context.Context, fn func(*sql.Tx) error) error {
	if db == nil || db.conn == nil {
		return errors.New("database is not initialized")
	}
	run := func() error {
		tx, err := db.conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if err := fn(tx); err != nil {
			return err
		}
		return tx.Commit()
	}
	return db.withSQLiteWriteLock(ctx, run)
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
			note TEXT DEFAULT '',
			error_message TEXT DEFAULT '',
			deleted_at TIMESTAMP NULL,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		);`,
		`CREATE TABLE IF NOT EXISTS usage_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			account_id INTEGER DEFAULT 0,
			credential_generation INTEGER NOT NULL DEFAULT 0,
			client_ip TEXT DEFAULT '',
			client_user_agent TEXT DEFAULT '',
			upstream_user_agent TEXT DEFAULT '',
			user_agent_overridden INTEGER DEFAULT 0,
			internal_reason TEXT DEFAULT '',
			parent_request_id TEXT DEFAULT '',
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
			ws_acquire_ms INTEGER DEFAULT 0,
			reasoning_effort TEXT DEFAULT '',
			effective_model TEXT DEFAULT '',
			inbound_endpoint TEXT DEFAULT '',
			upstream_endpoint TEXT DEFAULT '',
				stream INTEGER DEFAULT 0,
				compact INTEGER DEFAULT 0,
				has_compaction_history INTEGER DEFAULT 0,
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
		`CREATE TABLE IF NOT EXISTS api_key_scope_counters (
			api_key_id INTEGER NOT NULL,
			scope_type TEXT NOT NULL,
			scope_id INTEGER NOT NULL,
			used_cost REAL DEFAULT 0,
			used_tokens INTEGER DEFAULT 0,
			used_requests INTEGER DEFAULT 0,
			reset_count INTEGER DEFAULT 0,
			last_reset_at TIMESTAMP NULL,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (api_key_id, scope_type, scope_id)
		);`,
		`CREATE TABLE IF NOT EXISTS account_groups (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT UNIQUE NOT NULL,
			description TEXT DEFAULT '',
			color TEXT DEFAULT '',
			sort_order INTEGER DEFAULT 0,
			base_concurrency_override INTEGER NULL,
			proxy_urls TEXT DEFAULT '[]',
			channel TEXT DEFAULT 'codex',
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
					grok_config TEXT DEFAULT '{}',
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
				codex_min_cli_version TEXT DEFAULT '0.144.1',
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
				public_image_studio_page_enabled INTEGER DEFAULT 1,
				public_account_portal_page_enabled INTEGER DEFAULT 0,
				scheduler_mode TEXT DEFAULT 'round_robin',
					affinity_mode TEXT DEFAULT 'bounded',
					session_affinity_spread INTEGER DEFAULT 0,
					codex_force_websocket INTEGER DEFAULT 0,
					codex_ws_weak_network_mode INTEGER DEFAULT 0,
					codex_ws_keepalive_enabled INTEGER DEFAULT 0,
					codex_ws_keepalive_interval_sec INTEGER DEFAULT 60,
					codex_ws_hide_upstream_errors INTEGER DEFAULT 1,
					codex_ws_silent_retry_enabled INTEGER DEFAULT 1,
					codex_ws_silent_max_retries INTEGER DEFAULT 2,
					codex_ws_size_router_enabled INTEGER DEFAULT 1,
					codex_ws_busy_acquire_max_wait_sec INTEGER DEFAULT 30,
					codex_ws_busy_overflow_enabled INTEGER DEFAULT 0,
					codex_ws_busy_patience_sec INTEGER DEFAULT 2,
					codex_ws_stateless_slots INTEGER DEFAULT 8,
					github_token TEXT DEFAULT '',
					github_proxy_url TEXT DEFAULT '',
					codex_overload_pause_enabled INTEGER DEFAULT 0,
					codex_overload_threshold_percent INTEGER DEFAULT 20,
					codex_overload_pause_minutes INTEGER DEFAULT 30,
					codex_overload_window_minutes INTEGER DEFAULT 5,
					overflow_auto_compact_enabled INTEGER DEFAULT 0,
					compact_via_responses_enabled INTEGER DEFAULT 0,
					codex_preflight_sse_passthrough_enabled INTEGER DEFAULT 0,
					first_token_excludes_ws_acquire INTEGER DEFAULT 0,
					codex_continue_thinking_enabled INTEGER DEFAULT 0,
					codex_continue_max_rounds INTEGER DEFAULT 8,
					retry_interval_ms INTEGER DEFAULT 0,
					transport_retry_policy TEXT DEFAULT 'rotate',
					continuous_retry_policy TEXT DEFAULT '{"enabled":false,"catch_all":false,"categories":["transport","http_429","http_5xx","stream_error"],"status_codes":[],"error_codes":[],"max_duration_seconds":600}',
					codex_synced_cli_version TEXT DEFAULT '',
					codex_cli_version_sync_enabled INTEGER DEFAULT 1,
					codex_cli_version_sync_interval_hours INTEGER DEFAULT 12,
					model_pricing_overrides TEXT DEFAULT '{}',
					model_pricing_sync_url TEXT DEFAULT '',
					ignore_usage_limit_status INTEGER DEFAULT 0,
					auto_reset_credits_enabled INTEGER DEFAULT 0,
					auto_reset_credits_before_expiry_min INTEGER DEFAULT 60,
					utls_shutdown_timeout_minutes INTEGER DEFAULT 30,
					codex_fingerprint_default_mode TEXT DEFAULT 'off',
					response_cache_local_max_bytes INTEGER NOT NULL DEFAULT 67108864,
					response_cache_local_max_entry_bytes INTEGER NOT NULL DEFAULT 8388608,
					response_cache_reconstruct_max_bytes INTEGER NOT NULL DEFAULT 67108864,
					response_cache_config_generation INTEGER NOT NULL DEFAULT 1,
					relay_model_cooldown_mode TEXT NOT NULL DEFAULT 'off',
					relay_model_cooldown_seconds INTEGER NOT NULL DEFAULT 2,
					relay_model_cooldown_backoff_enabled INTEGER NOT NULL DEFAULT 0,
					oauth_model_cooldown_mode TEXT NOT NULL DEFAULT 'adaptive',
					oauth_model_cooldown_seconds INTEGER NOT NULL DEFAULT 300,
					oauth_model_cooldown_backoff_enabled INTEGER NOT NULL DEFAULT 1
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
			test_latency_ms INTEGER DEFAULT 0,
			test_status TEXT NOT NULL DEFAULT 'untested'
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
			request_protocol TEXT DEFAULT '',
			request_provider TEXT DEFAULT '',
			model TEXT DEFAULT '',
			action TEXT DEFAULT '',
			mode TEXT DEFAULT '',
			score INTEGER DEFAULT 0,
			audit_score INTEGER DEFAULT 0,
			threshold_value INTEGER DEFAULT 0,
			policy_profile TEXT DEFAULT '',
			reason_code TEXT DEFAULT '',
			primary_origin TEXT DEFAULT '',
			strike_eligible INTEGER DEFAULT 0,
			matched_patterns TEXT DEFAULT '[]',
			text_preview TEXT DEFAULT '',
			match_context TEXT DEFAULT '',
			api_key_id INTEGER DEFAULT 0,
			api_key_name TEXT DEFAULT '',
			api_key_masked TEXT DEFAULT '',
			client_ip TEXT DEFAULT '',
			error_code TEXT DEFAULT '',
			review_model TEXT DEFAULT '',
			review_flagged INTEGER DEFAULT 0,
			review_error TEXT DEFAULT '',
			reviewed INTEGER DEFAULT 0,
			review_confidence REAL NULL,
			review_threshold REAL NULL,
			review_reason TEXT DEFAULT '',
			review_endpoint TEXT DEFAULT '',
			review_request_mode TEXT DEFAULT '',
			review_latency_ms INTEGER NULL,
			full_text TEXT DEFAULT ''
		);`,
		`CREATE TABLE IF NOT EXISTS prompt_review_profiles (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			base_url TEXT NOT NULL DEFAULT '',
			model TEXT NOT NULL DEFAULT '',
			request_mode TEXT NOT NULL DEFAULT 'moderations',
			adapter_json TEXT NOT NULL DEFAULT '{}',
			api_keys TEXT NOT NULL DEFAULT '',
			timeout_seconds INTEGER NOT NULL DEFAULT 10,
			active INTEGER NOT NULL DEFAULT 0,
			created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
		);`,
		`DROP TABLE IF EXISTS prompt_filter_secrets;`,
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
		{"accounts", "note", "TEXT DEFAULT ''"},
		{"accounts", "deleted_at", "TIMESTAMP NULL"},
		{"usage_logs", "channel", "TEXT DEFAULT ''"},
		{"usage_logs", "input_tokens", "INTEGER DEFAULT 0"},
		{"usage_logs", "output_tokens", "INTEGER DEFAULT 0"},
		{"usage_logs", "reasoning_tokens", "INTEGER DEFAULT 0"},
		{"usage_logs", "first_token_ms", "INTEGER DEFAULT 0"},
		{"usage_logs", "ws_acquire_ms", "INTEGER DEFAULT 0"},
		{"usage_logs", "reasoning_effort", "TEXT DEFAULT ''"},
		{"usage_logs", "effective_model", "TEXT DEFAULT ''"},
		{"usage_logs", "inbound_endpoint", "TEXT DEFAULT ''"},
		{"usage_logs", "upstream_endpoint", "TEXT DEFAULT ''"},
		{"usage_logs", "stream", "INTEGER DEFAULT 0"},
		{"usage_logs", "via_websocket", "INTEGER DEFAULT 0"},
		{"usage_logs", "compact", "INTEGER DEFAULT 0"},
		{"usage_logs", "has_compaction_history", "INTEGER DEFAULT 0"},
		{"usage_logs", "cached_tokens", "INTEGER DEFAULT 0"},
		{"usage_logs", "service_tier", "TEXT DEFAULT ''"},
		{"usage_logs", "requested_service_tier", "TEXT DEFAULT ''"},
		{"usage_logs", "actual_service_tier", "TEXT DEFAULT ''"},
		{"usage_logs", "billing_service_tier", "TEXT DEFAULT ''"},
		{"usage_logs", "api_key_id", "INTEGER DEFAULT 0"},
		{"usage_logs", "api_key_name", "TEXT DEFAULT ''"},
		{"usage_logs", "api_key_masked", "TEXT DEFAULT ''"},
		{"usage_logs", "client_ip", "TEXT DEFAULT ''"},
		{"usage_logs", "client_user_agent", "TEXT DEFAULT ''"},
		{"usage_logs", "upstream_user_agent", "TEXT DEFAULT ''"},
		{"usage_logs", "user_agent_overridden", "INTEGER DEFAULT 0"},
		{"usage_logs", "internal_reason", "TEXT DEFAULT ''"},
		{"usage_logs", "parent_request_id", "TEXT DEFAULT ''"},
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
		{"usage_logs", "credential_generation", "INTEGER NOT NULL DEFAULT 0"},
		{"system_settings", "continuous_retry_policy", "TEXT DEFAULT '{\"enabled\":false,\"catch_all\":false,\"categories\":[\"transport\",\"http_429\",\"http_5xx\",\"stream_error\"],\"status_codes\":[],\"error_codes\":[]}'"},
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
		{"account_groups", "base_concurrency_override", "INTEGER NULL"},
		{"account_groups", "proxy_urls", "TEXT DEFAULT '[]'"},
		{"account_groups", "channel", "TEXT DEFAULT 'codex'"},
		{"account_groups", "created_at", "TIMESTAMP DEFAULT CURRENT_TIMESTAMP"},
		{"account_groups", "updated_at", "TIMESTAMP DEFAULT CURRENT_TIMESTAMP"},
		{"system_settings", "site_name", "TEXT DEFAULT 'CodexProxy'"},
		{"system_settings", "site_logo", "TEXT DEFAULT ''"},
		{"system_settings", "background_config", "TEXT DEFAULT '{}'"},
		{"system_settings", "grok_config", "TEXT DEFAULT '{}'"},
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
		{"system_settings", "codex_ws_weak_network_mode", "INTEGER DEFAULT 0"},
		{"system_settings", "codex_ws_keepalive_enabled", "INTEGER DEFAULT 0"},
		{"system_settings", "codex_ws_keepalive_interval_sec", "INTEGER DEFAULT 60"},
		{"system_settings", "codex_ws_hide_upstream_errors", "INTEGER DEFAULT 1"},
		{"system_settings", "codex_ws_silent_retry_enabled", "INTEGER DEFAULT 1"},
		{"system_settings", "codex_ws_silent_max_retries", "INTEGER DEFAULT 2"},
		{"system_settings", "codex_ws_size_router_enabled", "INTEGER DEFAULT 1"},
		{"system_settings", "codex_ws_busy_acquire_max_wait_sec", "INTEGER DEFAULT 30"},
		{"system_settings", "codex_ws_busy_overflow_enabled", "INTEGER DEFAULT 0"},
		{"system_settings", "codex_ws_busy_patience_sec", "INTEGER DEFAULT 2"},
		{"system_settings", "codex_ws_stateless_slots", "INTEGER DEFAULT 8"},
		{"system_settings", "github_token", "TEXT DEFAULT ''"},
		{"system_settings", "github_proxy_url", "TEXT DEFAULT ''"},
		{"system_settings", "codex_overload_pause_enabled", "INTEGER DEFAULT 0"},
		{"system_settings", "codex_overload_threshold_percent", "INTEGER DEFAULT 20"},
		{"system_settings", "codex_overload_pause_minutes", "INTEGER DEFAULT 30"},
		{"system_settings", "codex_overload_window_minutes", "INTEGER DEFAULT 5"},
		{"system_settings", "overflow_auto_compact_enabled", "INTEGER DEFAULT 0"},
		{"system_settings", "compact_via_responses_enabled", "INTEGER DEFAULT 0"},
		{"system_settings", "codex_preflight_sse_passthrough_enabled", "INTEGER DEFAULT 0"},
		{"system_settings", "first_token_excludes_ws_acquire", "INTEGER DEFAULT 0"},
		{"system_settings", "codex_continue_thinking_enabled", "INTEGER DEFAULT 0"},
		{"system_settings", "codex_continue_max_rounds", "INTEGER DEFAULT 8"},
		{"system_settings", "retry_interval_ms", "INTEGER DEFAULT 0"},
		{"system_settings", "transport_retry_policy", "TEXT DEFAULT 'rotate'"},
		{"system_settings", "codex_synced_cli_version", "TEXT DEFAULT ''"},
		{"system_settings", "codex_cli_version_sync_enabled", "INTEGER DEFAULT 1"},
		{"system_settings", "codex_cli_version_sync_interval_hours", "INTEGER DEFAULT 12"},
		{"system_settings", "model_pricing_overrides", "TEXT DEFAULT '{}'"},
		{"system_settings", "model_pricing_sync_url", "TEXT DEFAULT ''"},
		{"system_settings", "ignore_usage_limit_status", "INTEGER DEFAULT 0"},
		{"system_settings", "auto_reset_credits_enabled", "INTEGER DEFAULT 0"},
		{"system_settings", "auto_reset_credits_before_expiry_min", "INTEGER DEFAULT 60"},
		{"system_settings", "utls_shutdown_timeout_minutes", "INTEGER DEFAULT 30"},
		{"system_settings", "codex_fingerprint_default_mode", "TEXT DEFAULT 'off'"},
		{"system_settings", "response_cache_local_max_bytes", "INTEGER NOT NULL DEFAULT 67108864"},
		{"system_settings", "response_cache_local_max_entry_bytes", "INTEGER NOT NULL DEFAULT 8388608"},
		{"system_settings", "response_cache_reconstruct_max_bytes", "INTEGER NOT NULL DEFAULT 67108864"},
		{"system_settings", "response_cache_config_generation", "INTEGER NOT NULL DEFAULT 1"},
		{"system_settings", "relay_model_cooldown_mode", "TEXT NOT NULL DEFAULT 'off'"},
		{"system_settings", "relay_model_cooldown_seconds", "INTEGER NOT NULL DEFAULT 2"},
		{"system_settings", "relay_model_cooldown_backoff_enabled", "INTEGER NOT NULL DEFAULT 0"},
		{"system_settings", "oauth_model_cooldown_mode", "TEXT NOT NULL DEFAULT 'adaptive'"},
		{"system_settings", "oauth_model_cooldown_seconds", "INTEGER NOT NULL DEFAULT 300"},
		{"system_settings", "oauth_model_cooldown_backoff_enabled", "INTEGER NOT NULL DEFAULT 1"},
		{"system_settings", "max_retries", "INTEGER DEFAULT 2"},
		{"system_settings", "max_rate_limit_retries", "INTEGER DEFAULT 1"},
		{"system_settings", "allow_remote_migration", "INTEGER DEFAULT 0"},
		{"system_settings", "model_mapping", "TEXT DEFAULT '{}'"},
		{"system_settings", "codex_model_mapping", "TEXT DEFAULT '{}'"},
		{"system_settings", "payload_rules", "TEXT DEFAULT '{}'"},
		{"system_settings", "reasoning_effort_models", "TEXT DEFAULT '[]'"},
		{"system_settings", "resin_url", "TEXT DEFAULT ''"},
		{"system_settings", "resin_platform_name", "TEXT DEFAULT ''"},
		{"system_settings", "prompt_filter_enabled", "INTEGER DEFAULT 0"},
		{"system_settings", "prompt_filter_mode", "TEXT DEFAULT 'monitor'"},
		{"system_settings", "prompt_filter_threshold", "INTEGER DEFAULT 50"},
		{"system_settings", "prompt_filter_strict_threshold", "INTEGER DEFAULT 90"},
		{"system_settings", "prompt_filter_strict_terminal_enabled", "INTEGER DEFAULT 0"},
		{"system_settings", "prompt_filter_advanced_config", "TEXT DEFAULT '{}'"},
		{"system_settings", "prompt_filter_log_matches", "INTEGER DEFAULT 1"},
		{"system_settings", "prompt_filter_max_text_length", "INTEGER DEFAULT 81920"},
		{"system_settings", "prompt_filter_sensitive_words", "TEXT DEFAULT ''"},
		{"system_settings", "prompt_filter_custom_patterns", "TEXT DEFAULT '[]'"},
		{"system_settings", "prompt_filter_disabled_patterns", "TEXT DEFAULT '[]'"},
		{"system_settings", "prompt_filter_review_enabled", "INTEGER DEFAULT 0"},
		{"system_settings", "prompt_filter_review_api_key", "TEXT DEFAULT ''"},
		{"system_settings", "prompt_filter_review_base_url", "TEXT DEFAULT 'https://api.deepseek.com'"},
		{"system_settings", "prompt_filter_review_model", "TEXT DEFAULT 'deepseek-v4-flash'"},
		{"system_settings", "prompt_filter_review_timeout_seconds", "INTEGER DEFAULT 10"},
		{"system_settings", "prompt_filter_review_fail_closed", "INTEGER DEFAULT 1"},
		{"prompt_filter_logs", "review_model", "TEXT DEFAULT ''"},
		{"prompt_filter_logs", "review_flagged", "INTEGER DEFAULT 0"},
		{"prompt_filter_logs", "review_error", "TEXT DEFAULT ''"},
		{"prompt_filter_logs", "reviewed", "INTEGER DEFAULT 0"},
		{"prompt_filter_logs", "review_confidence", "REAL NULL"},
		{"prompt_filter_logs", "review_threshold", "REAL NULL"},
		{"prompt_filter_logs", "review_reason", "TEXT DEFAULT ''"},
		{"prompt_filter_logs", "review_endpoint", "TEXT DEFAULT ''"},
		{"prompt_filter_logs", "review_request_mode", "TEXT DEFAULT ''"},
		{"prompt_filter_logs", "review_latency_ms", "INTEGER NULL"},
		{"prompt_filter_logs", "full_text", "TEXT DEFAULT ''"},
		{"prompt_filter_logs", "match_context", "TEXT DEFAULT ''"},
		{"prompt_filter_logs", "audit_score", "INTEGER DEFAULT 0"},
		{"prompt_filter_logs", "policy_profile", "TEXT DEFAULT ''"},
		{"prompt_filter_logs", "reason_code", "TEXT DEFAULT ''"},
		{"prompt_filter_logs", "primary_origin", "TEXT DEFAULT ''"},
		{"prompt_filter_logs", "strike_eligible", "INTEGER DEFAULT 0"},
		{"prompt_filter_logs", "request_protocol", "TEXT DEFAULT ''"},
		{"prompt_filter_logs", "request_provider", "TEXT DEFAULT ''"},
		{"system_settings", "client_compat_mode", "TEXT DEFAULT 'preserve'"},
		{"system_settings", "codex_min_cli_version", "TEXT DEFAULT '0.144.1'"},
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
		{"system_settings", "public_image_studio_page_enabled", "INTEGER DEFAULT 1"},
		{"system_settings", "public_account_portal_page_enabled", "INTEGER DEFAULT 0"},
		{"system_settings", "scheduler_mode", "TEXT DEFAULT 'round_robin'"},
		{"system_settings", "affinity_mode", "TEXT DEFAULT 'bounded'"},
		{"system_settings", "session_affinity_spread", "INTEGER DEFAULT 0"},
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
		{"proxies", "test_status", "TEXT NOT NULL DEFAULT 'untested'"},
	}
	for _, column := range columns {
		if err := db.ensureSQLiteColumn(ctx, column.table, column.name, column.def); err != nil {
			return err
		}
	}
	if _, err := db.conn.ExecContext(ctx, `
		UPDATE proxies
		SET test_status = 'success'
		WHERE COALESCE(test_status, 'untested') = 'untested'
		  AND (COALESCE(test_ip, '') <> '' OR COALESCE(test_location, '') <> '' OR COALESCE(test_latency_ms, 0) > 0)
	`); err != nil {
		return err
	}

	// 审查服务从未配置过(无 key、未启用)且仍是旧出厂默认时,迁移到新的
	// DeepSeek 默认供应商;真在用 OpenAI 审核的部署不受影响。
	if _, err := db.conn.ExecContext(ctx, `
		UPDATE system_settings
		SET prompt_filter_review_base_url = 'https://api.deepseek.com',
			prompt_filter_review_model = 'deepseek-v4-flash'
		WHERE COALESCE(prompt_filter_review_api_key, '') = ''
		  AND COALESCE(prompt_filter_review_enabled, 0) = 0
		  AND COALESCE(prompt_filter_review_base_url, '') = 'https://api.openai.com'
		  AND COALESCE(prompt_filter_review_model, '') = 'omni-moderation-latest'
	`); err != nil {
		return err
	}

	indexStatements := []string{
		`CREATE INDEX IF NOT EXISTS idx_accounts_status ON accounts(status);`,
		`CREATE INDEX IF NOT EXISTS idx_accounts_platform ON accounts(platform);`,
		`CREATE INDEX IF NOT EXISTS idx_accounts_cooldown_until ON accounts(cooldown_until);`,
		`CREATE INDEX IF NOT EXISTS idx_accounts_upstream_type_id ON accounts(LOWER(COALESCE(json_extract(credentials, '$.upstream_type'), '')), id);`,
		`CREATE INDEX IF NOT EXISTS idx_accounts_active_upstream_type_id ON accounts(LOWER(COALESCE(json_extract(credentials, '$.upstream_type'), '')), id) WHERE status <> 'deleted' AND COALESCE(error_message, '') <> 'deleted';`,
		`CREATE INDEX IF NOT EXISTS idx_accounts_created_id ON accounts(created_at, id);`,
		`CREATE INDEX IF NOT EXISTS idx_accounts_updated_id ON accounts(updated_at, id);`,
		`CREATE INDEX IF NOT EXISTS idx_usage_logs_created_at ON usage_logs(created_at);`,
		`CREATE INDEX IF NOT EXISTS idx_usage_logs_account_id ON usage_logs(account_id);`,
		`CREATE INDEX IF NOT EXISTS idx_usage_logs_account_created_at ON usage_logs(account_id, created_at);`,
		`CREATE INDEX IF NOT EXISTS idx_usage_logs_account_generation_created_at ON usage_logs(account_id, credential_generation, created_at);`,
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
		`CREATE INDEX IF NOT EXISTS idx_prompt_filter_logs_source_id ON prompt_filter_logs(source, id DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_prompt_filter_logs_reviewed_id ON prompt_filter_logs(reviewed, id DESC);`,
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
		  AND TRIM(COALESCE(internal_reason, '')) = ''
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

func (db *DB) getChartAggregationSQLite(ctx context.Context, start, end time.Time, bucketMinutes int, channel string) (*ChartAggregation, error) {
	startArg, endArg := db.timeRangeArgs(start, end)
	if bucketMinutes < 1 {
		bucketMinutes = 5
	}
	query := `
		SELECT
			datetime((CAST(strftime('%s', created_at) AS INTEGER) / ($3 * 60)) * ($3 * 60), 'unixepoch') AS bucket,
			COUNT(*), COALESCE(AVG(duration_ms), 0),
			COALESCE(SUM(input_tokens), 0), COALESCE(SUM(output_tokens), 0),
			COALESCE(SUM(reasoning_tokens), 0), COALESCE(SUM(cached_tokens), 0),
			COALESCE(SUM(CASE WHEN status_code >= 400 AND status_code < 500 THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN status_code >= 500 AND status_code < 600 THEN 1 ELSE 0 END), 0)
		FROM usage_logs
		WHERE created_at >= $1 AND created_at < $2
		  AND status_code <> 499
		  AND TRIM(COALESCE(internal_reason, '')) = ''
	`
	args := []interface{}{startArg, endArg, bucketMinutes}
	if channel != "" {
		query += " AND channel = $4"
		args = append(args, channel)
	}
	query += " GROUP BY 1 ORDER BY 1"
	rows, err := db.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := &ChartAggregation{}
	for rows.Next() {
		var point ChartTimelinePoint
		if err := rows.Scan(&point.Bucket, &point.Requests, &point.AvgLatency, &point.InputTokens,
			&point.OutputTokens, &point.ReasoningTokens, &point.CachedTokens, &point.Errors4xx, &point.Errors5xx); err != nil {
			return nil, err
		}
		point.Bucket = strings.Replace(point.Bucket, " ", "T", 1) + "Z"
		result.Timeline = append(result.Timeline, point)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if result.Timeline == nil {
		result.Timeline = []ChartTimelinePoint{}
	}

	modelQuery := `SELECT COALESCE(NULLIF(effective_model, ''), NULLIF(model, ''), 'unknown'), COUNT(*)
		FROM usage_logs WHERE created_at >= $1 AND created_at < $2 AND status_code <> 499
		  AND TRIM(COALESCE(internal_reason, '')) = ''`
	modelArgs := []interface{}{startArg, endArg}
	if channel != "" {
		modelQuery += " AND channel = $3"
		modelArgs = append(modelArgs, channel)
	}
	modelQuery += " GROUP BY 1 ORDER BY 2 DESC, 1 LIMIT 10"
	modelRows, err := db.conn.QueryContext(ctx, modelQuery, modelArgs...)
	if err != nil {
		return nil, err
	}
	defer modelRows.Close()
	for modelRows.Next() {
		var point ChartModelPoint
		if err := modelRows.Scan(&point.Model, &point.Requests); err != nil {
			return nil, err
		}
		result.Models = append(result.Models, point)
	}
	if result.Models == nil {
		result.Models = []ChartModelPoint{}
	}

	return result, modelRows.Err()
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
func (db *DB) getUsageStatsSQLite(ctx context.Context, rangeStart, rangeEnd time.Time, channel string, includeBreakdowns bool) (*UsageStats, error) {
	now := time.Now()
	explicitRange := !rangeStart.IsZero()
	if rangeStart.IsZero() {
		rangeStart = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	}
	minuteAgo := now.Add(-1 * time.Minute)

	query := `SELECT COUNT(*), COALESCE(SUM(total_tokens), 0), COALESCE(SUM(prompt_tokens), 0),
		COALESCE(SUM(completion_tokens), 0), COALESCE(SUM(cached_tokens), 0),
		COALESCE(SUM(account_billed), 0), COALESCE(SUM(user_billed), 0),
		COALESCE(AVG(duration_ms), 0),
		COALESCE(SUM(CASE WHEN first_token_ms > 0 THEN first_token_ms ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN first_token_ms > 0 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN cached_tokens > 0 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status_code >= 400 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN created_at >= $2 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN created_at >= $2 THEN total_tokens ELSE 0 END), 0)
	FROM usage_logs WHERE created_at >= $1 AND status_code <> 499
	  AND TRIM(COALESCE(internal_reason, '')) = ''`
	args := []interface{}{db.timeArg(rangeStart), db.timeArg(minuteAgo)}
	if !rangeEnd.IsZero() {
		query += fmt.Sprintf(" AND created_at < $%d", len(args)+1)
		args = append(args, db.timeArg(rangeEnd))
	}
	if channel != "" {
		query += fmt.Sprintf(" AND channel = $%d", len(args)+1)
		args = append(args, channel)
	}

	stats := &UsageStats{}
	var err error
	var todayErrors int64
	var totalFirstTokenMs float64
	var totalFirstTokenSamples int64
	var todayCacheHitRequests int64
	if err := db.conn.QueryRowContext(ctx, query, args...).Scan(
		&stats.TodayRequests, &stats.TodayTokens, &stats.TodayPrompt, &stats.TodayCompletion,
		&stats.TodayCachedTokens, &stats.TodayAccountBilled, &stats.TodayUserBilled,
		&stats.AvgDurationMs, &totalFirstTokenMs, &totalFirstTokenSamples,
		&todayCacheHitRequests, &todayErrors, &stats.RPM, &stats.TPM,
	); err != nil {
		return nil, err
	}

	if stats.TodayRequests > 0 {
		stats.ErrorRate = float64(todayErrors) / float64(stats.TodayRequests) * 100
		stats.TodayCacheRate = float64(todayCacheHitRequests) / float64(stats.TodayRequests) * 100
	}
	if totalFirstTokenSamples > 0 {
		stats.AvgFirstTokenMs = totalFirstTokenMs / float64(totalFirstTokenSamples)
	}

	rollup, err := db.loadUsageStatsRollup(ctx, channel)
	if err != nil {
		return nil, fmt.Errorf("读取用量累计汇总: %w", err)
	}
	stats.TotalRequests = rollup.TotalRequests
	stats.TotalTokens = rollup.TotalTokens
	stats.TotalPrompt = rollup.PromptTokens
	stats.TotalCompletion = rollup.CompletionTokens
	stats.TotalCachedTokens = rollup.CachedTokens
	stats.TotalAccountBilled = rollup.TotalAccountBilled
	stats.TotalUserBilled = rollup.TotalUserBilled
	if stats.TotalRequests > 0 {
		stats.TotalCacheRate = float64(rollup.CacheHitRequests) / float64(stats.TotalRequests) * 100
	}
	if !explicitRange && rollup.FirstTokenSamples > 0 {
		stats.AvgFirstTokenMs = rollup.FirstTokenMsSum / float64(rollup.FirstTokenSamples)
	}
	if stats.TotalRequests > 0 {
		stats.AvgAccountBilled = stats.TotalAccountBilled / float64(stats.TotalRequests)
		stats.AvgUserBilled = stats.TotalUserBilled / float64(stats.TotalRequests)
	}
	if includeBreakdowns {
		stats.ModelStats, err = db.getUsageModelStats(ctx, 10, rangeStart, rangeEnd, channel)
		if err != nil {
			return nil, err
		}
		if err := db.populateUsageBreakdownStats(ctx, stats, rangeStart, rangeEnd, channel); err != nil {
			return nil, err
		}
	} else {
		stats.ModelStats = []UsageModelStat{}
		stats.EndpointStats = []UsageEndpointStat{}
		stats.APIKeyStats = []UsageAPIKeyStat{}
	}

	return stats, nil
}
