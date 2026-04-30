package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	_ "github.com/lib/pq"
	_ "modernc.org/sqlite"
)

// AccountRow 数据库中的账号行
type AccountRow struct {
	ID                      int64
	Name                    string
	Platform                string
	Type                    string
	Credentials             map[string]interface{}
	ProxyURL                string
	Status                  string
	CooldownReason          string
	CooldownUntil           sql.NullTime
	ErrorMessage            string
	Enabled                 bool
	Locked                  bool
	ScoreBiasOverride       sql.NullInt64
	BaseConcurrencyOverride sql.NullInt64
	CreatedAt               time.Time
	UpdatedAt               time.Time
}

type OptionalInt64Slice struct {
	Set    bool
	Values []int64
}

// GetCredential 从 credentials JSONB 获取字符串字段
func (a *AccountRow) GetCredential(key string) string {
	if a.Credentials == nil {
		return ""
	}
	v, ok := a.Credentials[key]
	if !ok || v == nil {
		return ""
	}
	switch val := v.(type) {
	case string:
		return val
	case float64:
		return fmt.Sprintf("%v", val)
	default:
		return ""
	}
}

func (a *AccountRow) GetCredentialInt64Slice(key string) []int64 {
	if a.Credentials == nil {
		return []int64{}
	}
	value, ok := a.Credentials[key]
	if !ok {
		return []int64{}
	}
	return int64SliceFromValue(value)
}

// DB PostgreSQL 数据库操作
type DB struct {
	conn   *sql.DB
	driver string

	// 使用日志批量写入缓冲
	logBuf  []usageLogEntry
	logMu   sync.Mutex
	logStop chan struct{}
	logWg   sync.WaitGroup
	// 预分配日志缓冲以减少内存分配
	logBufCap int
}

// usageLogEntry 日志缓冲条目
type usageLogEntry struct {
	AccountID        int64
	Endpoint         string
	Model            string
	EffectiveModel   string
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	StatusCode       int
	DurationMs       int
	InputTokens      int
	OutputTokens     int
	ReasoningTokens  int
	FirstTokenMs     int
	ReasoningEffort  string
	InboundEndpoint  string
	UpstreamEndpoint string
	Stream           bool
	CachedTokens     int
	ServiceTier      string
	APIKeyID         int64
	APIKeyName       string
	APIKeyMasked     string
	ImageCount       int
	ImageWidth       int
	ImageHeight      int
	ImageBytes       int
	ImageFormat      string
	ImageSize        string
	AccountBilled    float64
	UserBilled       float64
}

// New 创建数据库连接并自动建表。
func New(driver string, dsn string) (*DB, error) {
	driver = normalizeDriver(driver)
	driverName := driver
	if driver == "sqlite" {
		driverName = "sqlite"
	}

	conn, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}

	// ==================== 连接池优化 ====================
	if driver == "sqlite" {
		conn.SetMaxOpenConns(1)
		conn.SetMaxIdleConns(1)
	} else {
		// 高并发场景：大量 RT 刷新 + 前端查询 + 使用日志写入 并行
		conn.SetMaxOpenConns(100)                 // 增加最大打开连接数以处理更高并发
		conn.SetMaxIdleConns(50)                  // 增加空闲连接数以保持热连接
		conn.SetConnMaxLifetime(60 * time.Minute) // 增加连接最大生存时间
		conn.SetConnMaxIdleTime(30 * time.Minute) // 增加空闲连接最大闲置时间
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := conn.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("数据库连接测试失败: %w", err)
	}

	db := &DB{
		conn:      conn,
		driver:    driver,
		logStop:   make(chan struct{}),
		logBufCap: 128,
	}
	if db.isSQLite() {
		if err := db.configureSQLite(ctx); err != nil {
			return nil, fmt.Errorf("配置 SQLite 失败: %w", err)
		}
	} else {
		// PostgreSQL: 统一会话时区为 UTC，确保 NOW() 和时间字面量一致
		if _, err := conn.ExecContext(ctx, "SET timezone = 'UTC'"); err != nil {
			return nil, fmt.Errorf("设置数据库时区失败: %w", err)
		}
	}
	if err := db.migrate(ctx); err != nil {
		return nil, fmt.Errorf("数据库迁移失败: %w", err)
	}

	// 启动批量写入后台协程
	db.startLogFlusher()

	baselineInsert := `
		INSERT INTO usage_stats_baseline (id) VALUES (1) ON CONFLICT DO NOTHING
	`
	if db.isSQLite() {
		baselineInsert = `
			INSERT OR IGNORE INTO usage_stats_baseline (id) VALUES (1)
		`
	}
	_, err = db.conn.ExecContext(ctx, `
			CREATE TABLE IF NOT EXISTS usage_stats_baseline (
				id              INTEGER PRIMARY KEY DEFAULT 1 CHECK (id = 1),
				total_requests  BIGINT NOT NULL DEFAULT 0,
				total_tokens    BIGINT NOT NULL DEFAULT 0,
				prompt_tokens   BIGINT NOT NULL DEFAULT 0,
				completion_tokens BIGINT NOT NULL DEFAULT 0,
				cached_tokens   BIGINT NOT NULL DEFAULT 0,
				account_billed  DOUBLE PRECISION NOT NULL DEFAULT 0,
				user_billed     DOUBLE PRECISION NOT NULL DEFAULT 0
			)
		`)
	if err != nil {
		return nil, fmt.Errorf("创建 usage_stats_baseline 表失败: %w", err)
	}

	// 确保 baseline 行存在
	_, err = db.conn.ExecContext(ctx, baselineInsert)
	if err != nil {
		return nil, fmt.Errorf("初始化 usage_stats_baseline 失败: %w", err)
	}
	if err := db.ensureUsageStatsBaselineBillingColumns(ctx); err != nil {
		return nil, err
	}

	return db, nil
}

func (db *DB) ensureUsageStatsBaselineBillingColumns(ctx context.Context) error {
	if db.isSQLite() {
		columns, err := db.sqliteTableColumns(ctx, "usage_stats_baseline")
		if err != nil {
			return err
		}
		for _, column := range []struct {
			name string
			def  string
		}{
			{name: "account_billed", def: "REAL NOT NULL DEFAULT 0"},
			{name: "user_billed", def: "REAL NOT NULL DEFAULT 0"},
		} {
			if _, ok := columns[column.name]; ok {
				continue
			}
			if _, err := db.conn.ExecContext(ctx, fmt.Sprintf("ALTER TABLE usage_stats_baseline ADD COLUMN %s %s", column.name, column.def)); err != nil {
				return err
			}
		}
		return nil
	}
	_, err := db.conn.ExecContext(ctx, `
		ALTER TABLE usage_stats_baseline ADD COLUMN IF NOT EXISTS account_billed DOUBLE PRECISION NOT NULL DEFAULT 0;
		ALTER TABLE usage_stats_baseline ADD COLUMN IF NOT EXISTS user_billed DOUBLE PRECISION NOT NULL DEFAULT 0;
	`)
	return err
}

// Close 关闭数据库连接
func (db *DB) Close() error {
	// 停止批量写入并刷完缓冲
	close(db.logStop)
	db.logWg.Wait()
	db.flushLogs() // 最后一次 flush
	return db.conn.Close()
}

// migrate 自动建表
func (db *DB) migrate(ctx context.Context) error {
	if db.isSQLite() {
		return db.migrateSQLite(ctx)
	}
	query := `
	CREATE TABLE IF NOT EXISTS accounts (
		id            SERIAL PRIMARY KEY,
		name          VARCHAR(255) DEFAULT '',
		platform      VARCHAR(50) DEFAULT 'openai',
		type          VARCHAR(50) DEFAULT 'oauth',
		credentials   JSONB NOT NULL DEFAULT '{}',
		proxy_url     VARCHAR(500) DEFAULT '',
		status        VARCHAR(50) DEFAULT 'active',
		error_message TEXT DEFAULT '',
		deleted_at    TIMESTAMPTZ NULL,
		created_at    TIMESTAMPTZ DEFAULT NOW(),
		updated_at    TIMESTAMPTZ DEFAULT NOW()
	);

	ALTER TABLE accounts ADD COLUMN IF NOT EXISTS cooldown_reason VARCHAR(50) DEFAULT '';
	ALTER TABLE accounts ADD COLUMN IF NOT EXISTS cooldown_until TIMESTAMPTZ NULL;
	ALTER TABLE accounts ADD COLUMN IF NOT EXISTS enabled BOOLEAN DEFAULT TRUE;
	ALTER TABLE accounts ADD COLUMN IF NOT EXISTS locked BOOLEAN DEFAULT FALSE;
	ALTER TABLE accounts ADD COLUMN IF NOT EXISTS score_bias_override INT NULL;
	ALTER TABLE accounts ADD COLUMN IF NOT EXISTS base_concurrency_override INT NULL;
	ALTER TABLE accounts ADD COLUMN IF NOT EXISTS deleted_at TIMESTAMPTZ NULL;
	ALTER TABLE accounts ADD COLUMN IF NOT EXISTS image_quota_remaining INT NULL;
	ALTER TABLE accounts ADD COLUMN IF NOT EXISTS image_quota_total INT NULL;
	ALTER TABLE accounts ADD COLUMN IF NOT EXISTS today_used_count INT DEFAULT 0;
	ALTER TABLE accounts ADD COLUMN IF NOT EXISTS image_quota_reset_at TIMESTAMPTZ NULL;

	UPDATE accounts
	SET status = 'deleted',
		error_message = '',
		cooldown_reason = '',
		cooldown_until = NULL,
		deleted_at = COALESCE(deleted_at, updated_at, NOW()),
		updated_at = NOW()
	WHERE status <> 'deleted' AND COALESCE(error_message, '') = 'deleted';

	CREATE INDEX IF NOT EXISTS idx_accounts_status ON accounts(status);
	CREATE INDEX IF NOT EXISTS idx_accounts_platform ON accounts(platform);
	CREATE INDEX IF NOT EXISTS idx_accounts_cooldown_until ON accounts(cooldown_until);


	CREATE TABLE IF NOT EXISTS usage_logs (
		id             SERIAL PRIMARY KEY,
		account_id     INT DEFAULT 0,
		endpoint       VARCHAR(100) DEFAULT '',
		model          VARCHAR(100) DEFAULT '',
		prompt_tokens  INT DEFAULT 0,
		completion_tokens INT DEFAULT 0,
		total_tokens   INT DEFAULT 0,
		status_code    INT DEFAULT 0,
		duration_ms    INT DEFAULT 0,
		created_at     TIMESTAMPTZ DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_usage_logs_created_at ON usage_logs(created_at);
	CREATE INDEX IF NOT EXISTS idx_usage_logs_account_id ON usage_logs(account_id);
	CREATE INDEX IF NOT EXISTS idx_usage_logs_created_status ON usage_logs(created_at, status_code);
	CREATE INDEX IF NOT EXISTS idx_usage_logs_account_status ON usage_logs(account_id, status_code);

	-- 增强字段（向后兼容 ALTER）
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS input_tokens INT DEFAULT 0;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS output_tokens INT DEFAULT 0;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS reasoning_tokens INT DEFAULT 0;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS first_token_ms INT DEFAULT 0;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS reasoning_effort VARCHAR(20) DEFAULT '';
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS effective_model VARCHAR(100) DEFAULT '';
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS inbound_endpoint VARCHAR(100) DEFAULT '';
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS upstream_endpoint VARCHAR(100) DEFAULT '';
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS stream BOOLEAN DEFAULT false;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS cached_tokens INT DEFAULT 0;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS service_tier VARCHAR(20) DEFAULT '';
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS api_key_id INT DEFAULT 0;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS api_key_name VARCHAR(255) DEFAULT '';
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS api_key_masked VARCHAR(64) DEFAULT '';
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS image_count INT DEFAULT 0;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS image_width INT DEFAULT 0;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS image_height INT DEFAULT 0;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS image_bytes INT DEFAULT 0;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS image_format VARCHAR(20) DEFAULT '';
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS image_size VARCHAR(32) DEFAULT '';
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS account_billed DOUBLE PRECISION DEFAULT 0;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS user_billed DOUBLE PRECISION DEFAULT 0;

	CREATE INDEX IF NOT EXISTS idx_usage_logs_api_key_created_at ON usage_logs(api_key_id, created_at);

	CREATE TABLE IF NOT EXISTS api_keys (
		id         SERIAL PRIMARY KEY,
		name       VARCHAR(255) DEFAULT '',
		key        VARCHAR(255) NOT NULL UNIQUE,
		created_at TIMESTAMPTZ DEFAULT NOW()
	);

			CREATE TABLE IF NOT EXISTS system_settings (
				id                 INTEGER PRIMARY KEY DEFAULT 1 CHECK (id = 1),
				max_concurrency    INT DEFAULT 2,
			global_rpm         INT DEFAULT 0,
			test_model         VARCHAR(100) DEFAULT 'gpt-5.4',
			test_concurrency   INT DEFAULT 50,
			proxy_url          VARCHAR(500) DEFAULT '',
			pg_max_conns       INT DEFAULT 50,
			redis_pool_size    INT DEFAULT 30,
			auto_clean_unauthorized BOOLEAN DEFAULT FALSE,
			auto_clean_rate_limited BOOLEAN DEFAULT FALSE,
			background_refresh_interval_minutes INT DEFAULT 2,
			usage_probe_max_age_minutes INT DEFAULT 10,
			recovery_probe_interval_minutes INT DEFAULT 30
		);
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS pg_max_conns INT DEFAULT 50;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS redis_pool_size INT DEFAULT 30;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS auto_clean_unauthorized BOOLEAN DEFAULT FALSE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS auto_clean_rate_limited BOOLEAN DEFAULT FALSE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS admin_secret VARCHAR(255) DEFAULT '';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS auto_clean_full_usage BOOLEAN DEFAULT FALSE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS proxy_pool_enabled BOOLEAN DEFAULT FALSE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS fast_scheduler_enabled BOOLEAN DEFAULT FALSE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS max_retries INT DEFAULT 2;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS allow_remote_migration BOOLEAN DEFAULT FALSE;
		ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS auto_clean_error BOOLEAN DEFAULT FALSE;
		ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS auto_clean_expired BOOLEAN DEFAULT FALSE;
		ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS model_mapping TEXT DEFAULT '{}';
		ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS background_refresh_interval_minutes INT DEFAULT 2;
		ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS usage_probe_max_age_minutes INT DEFAULT 10;
		ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS recovery_probe_interval_minutes INT DEFAULT 30;
			ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS resin_url TEXT DEFAULT '';
			ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS resin_platform_name TEXT DEFAULT '';
			ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS prompt_filter_enabled BOOLEAN DEFAULT FALSE;
			ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS prompt_filter_mode VARCHAR(20) DEFAULT 'monitor';
			ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS prompt_filter_threshold INT DEFAULT 50;
			ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS prompt_filter_strict_threshold INT DEFAULT 90;
			ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS prompt_filter_log_matches BOOLEAN DEFAULT TRUE;
			ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS prompt_filter_max_text_length INT DEFAULT 81920;
			ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS prompt_filter_sensitive_words TEXT DEFAULT '';
			ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS prompt_filter_custom_patterns TEXT DEFAULT '[]';
			ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS prompt_filter_disabled_patterns TEXT DEFAULT '[]';

			CREATE TABLE IF NOT EXISTS prompt_filter_logs (
				id               SERIAL PRIMARY KEY,
				created_at       TIMESTAMPTZ DEFAULT NOW(),
				source           VARCHAR(50) DEFAULT '',
				endpoint         VARCHAR(100) DEFAULT '',
				model            VARCHAR(100) DEFAULT '',
				action           VARCHAR(20) DEFAULT '',
				mode             VARCHAR(20) DEFAULT '',
				score            INT DEFAULT 0,
				threshold_value  INT DEFAULT 0,
				matched_patterns TEXT DEFAULT '[]',
				text_preview     TEXT DEFAULT '',
				api_key_id       INT DEFAULT 0,
				api_key_name     VARCHAR(255) DEFAULT '',
				api_key_masked   VARCHAR(64) DEFAULT '',
				client_ip        VARCHAR(64) DEFAULT '',
				error_code       VARCHAR(100) DEFAULT ''
			);
			CREATE INDEX IF NOT EXISTS idx_prompt_filter_logs_created_at ON prompt_filter_logs(created_at);
			CREATE INDEX IF NOT EXISTS idx_prompt_filter_logs_action_created_at ON prompt_filter_logs(action, created_at);

			CREATE TABLE IF NOT EXISTS model_registry (
				id                     VARCHAR(100) PRIMARY KEY,
				enabled                BOOLEAN DEFAULT TRUE,
				category               VARCHAR(50) DEFAULT 'codex',
				source                 VARCHAR(50) DEFAULT 'manual',
				pro_only               BOOLEAN DEFAULT FALSE,
				api_key_auth_available BOOLEAN DEFAULT TRUE,
				last_seen_at           TIMESTAMPTZ NULL,
				updated_at             TIMESTAMPTZ DEFAULT NOW()
			);

			CREATE TABLE IF NOT EXISTS model_registry_sync (
				id             INTEGER PRIMARY KEY DEFAULT 1 CHECK (id = 1),
				source_url     TEXT DEFAULT '',
				last_synced_at TIMESTAMPTZ NULL
			);

			CREATE TABLE IF NOT EXISTS proxies (
			id         SERIAL PRIMARY KEY,
			url        VARCHAR(500) NOT NULL UNIQUE,
		label      VARCHAR(255) DEFAULT '',
		enabled    BOOLEAN DEFAULT TRUE,
		created_at TIMESTAMPTZ DEFAULT NOW()
	);
	ALTER TABLE proxies ADD COLUMN IF NOT EXISTS test_ip VARCHAR(100) DEFAULT '';
	ALTER TABLE proxies ADD COLUMN IF NOT EXISTS test_location VARCHAR(255) DEFAULT '';
	ALTER TABLE proxies ADD COLUMN IF NOT EXISTS test_latency_ms INT DEFAULT 0;

	CREATE TABLE IF NOT EXISTS account_events (
		id         SERIAL PRIMARY KEY,
		account_id INT NOT NULL DEFAULT 0,
		event_type VARCHAR(20) NOT NULL,
		source     VARCHAR(30) DEFAULT '',
		created_at TIMESTAMPTZ DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_account_events_created ON account_events(created_at);
	CREATE INDEX IF NOT EXISTS idx_account_events_type_created ON account_events(event_type, created_at);

	CREATE TABLE IF NOT EXISTS image_prompt_templates (
		id            SERIAL PRIMARY KEY,
		name          VARCHAR(255) NOT NULL DEFAULT '',
		prompt        TEXT NOT NULL DEFAULT '',
		model         VARCHAR(100) DEFAULT '',
		size          VARCHAR(32) DEFAULT '',
		quality       VARCHAR(32) DEFAULT '',
		output_format VARCHAR(32) DEFAULT '',
		background    VARCHAR(32) DEFAULT '',
		style         VARCHAR(64) DEFAULT '',
		tags          TEXT NOT NULL DEFAULT '[]',
		favorite      BOOLEAN DEFAULT FALSE,
		usage_count   INT DEFAULT 0,
		last_used_at  TIMESTAMPTZ NULL,
		created_at    TIMESTAMPTZ DEFAULT NOW(),
		updated_at    TIMESTAMPTZ DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_image_prompt_templates_updated ON image_prompt_templates(updated_at);
	CREATE INDEX IF NOT EXISTS idx_image_prompt_templates_favorite ON image_prompt_templates(favorite, updated_at);

	CREATE TABLE IF NOT EXISTS image_generation_jobs (
		id             SERIAL PRIMARY KEY,
		status         VARCHAR(32) NOT NULL DEFAULT 'queued',
		prompt         TEXT NOT NULL DEFAULT '',
		params_json    TEXT NOT NULL DEFAULT '{}',
		api_key_id     INT DEFAULT 0,
		api_key_name   VARCHAR(255) DEFAULT '',
		api_key_masked VARCHAR(64) DEFAULT '',
		error_message  TEXT DEFAULT '',
		duration_ms    INT DEFAULT 0,
		created_at     TIMESTAMPTZ DEFAULT NOW(),
		started_at     TIMESTAMPTZ NULL,
		completed_at   TIMESTAMPTZ NULL
	);
	CREATE INDEX IF NOT EXISTS idx_image_generation_jobs_created ON image_generation_jobs(created_at);
	CREATE INDEX IF NOT EXISTS idx_image_generation_jobs_status ON image_generation_jobs(status, created_at);

	CREATE TABLE IF NOT EXISTS image_assets (
		id             SERIAL PRIMARY KEY,
		job_id         INT NOT NULL DEFAULT 0,
		template_id    INT DEFAULT 0,
		filename       VARCHAR(255) NOT NULL DEFAULT '',
		storage_path   TEXT NOT NULL DEFAULT '',
		mime_type      VARCHAR(100) NOT NULL DEFAULT '',
		bytes          INT DEFAULT 0,
		width          INT DEFAULT 0,
		height         INT DEFAULT 0,
		model          VARCHAR(100) DEFAULT '',
		requested_size VARCHAR(32) DEFAULT '',
		actual_size    VARCHAR(32) DEFAULT '',
		quality        VARCHAR(32) DEFAULT '',
		output_format  VARCHAR(32) DEFAULT '',
		revised_prompt TEXT DEFAULT '',
		created_at     TIMESTAMPTZ DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_image_assets_created ON image_assets(created_at);
	CREATE INDEX IF NOT EXISTS idx_image_assets_job_id ON image_assets(job_id);
	`
	_, err := db.conn.ExecContext(ctx, query)
	if err != nil {
		return err
	}

	// 独立长超时：将已有 TIMESTAMP 列迁移为 TIMESTAMPTZ（大表 ALTER COLUMN TYPE 可能较慢）
	migrateQuery := `
	DO $$
	DECLARE
		_tbl  TEXT;
		_col  TEXT;
		_rec  RECORD;
	BEGIN
		FOR _rec IN
			SELECT table_name, column_name
			FROM information_schema.columns
			WHERE table_schema = current_schema()
			  AND data_type = 'timestamp without time zone'
			  AND table_name IN ('accounts', 'usage_logs', 'api_keys', 'proxies', 'account_events')
		LOOP
			EXECUTE format(
				'ALTER TABLE %I ALTER COLUMN %I TYPE TIMESTAMPTZ USING %I AT TIME ZONE current_setting(''TIMEZONE'')',
				_rec.table_name, _rec.column_name, _rec.column_name
			);
		END LOOP;
	END $$;
	`
	migrateCtx, migrateCancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer migrateCancel()
	_, err = db.conn.ExecContext(migrateCtx, migrateQuery)
	return err
}

// ==================== API Keys ====================

// APIKeyRow API 密钥行
type APIKeyRow struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Key       string    `json:"key"`
	CreatedAt time.Time `json:"created_at"`
}

// ListAPIKeys 获取所有 API 密钥
func (db *DB) ListAPIKeys(ctx context.Context) ([]*APIKeyRow, error) {
	rows, err := db.conn.QueryContext(ctx, `SELECT id, name, key, created_at FROM api_keys ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var keys []*APIKeyRow
	for rows.Next() {
		k := &APIKeyRow{}
		var createdAtRaw interface{}
		if err := rows.Scan(&k.ID, &k.Name, &k.Key, &createdAtRaw); err != nil {
			return nil, err
		}
		k.CreatedAt, err = parseDBTimeValue(createdAtRaw)
		if err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// InsertAPIKey 插入新 API 密钥
func (db *DB) InsertAPIKey(ctx context.Context, name, key string) (int64, error) {
	return db.insertRowID(ctx,
		`INSERT INTO api_keys (name, key) VALUES ($1, $2) RETURNING id`,
		`INSERT INTO api_keys (name, key) VALUES ($1, $2)`,
		name, key,
	)
}

// ==================== System Settings ====================

// SystemSettings 运行时设置项
type SystemSettings struct {
	MaxConcurrency                   int
	GlobalRPM                        int
	TestModel                        string
	TestConcurrency                  int
	ProxyURL                         string
	PgMaxConns                       int
	RedisPoolSize                    int
	AutoCleanUnauthorized            bool
	AutoCleanRateLimited             bool
	AdminSecret                      string
	AutoCleanFullUsage               bool
	AutoCleanError                   bool
	AutoCleanExpired                 bool
	ProxyPoolEnabled                 bool
	FastSchedulerEnabled             bool
	MaxRetries                       int
	AllowRemoteMigration             bool
	ModelMapping                     string // JSON: {"anthropic_model": "codex_model", ...}
	BackgroundRefreshIntervalMinutes int
	UsageProbeMaxAgeMinutes          int
	RecoveryProbeIntervalMinutes     int
	ResinURL                         string // Resin 代理池地址（含 Token），例如 http://127.0.0.1:2260/my-token
	ResinPlatformName                string // Resin 平台标识，例如 codex2api
	PromptFilterEnabled              bool
	PromptFilterMode                 string
	PromptFilterThreshold            int
	PromptFilterStrictThreshold      int
	PromptFilterLogMatches           bool
	PromptFilterMaxTextLength        int
	PromptFilterSensitiveWords       string
	PromptFilterCustomPatterns       string
	PromptFilterDisabledPatterns     string
}

// GetSystemSettings 加载全局设置
func (db *DB) GetSystemSettings(ctx context.Context) (*SystemSettings, error) {
	s := &SystemSettings{}
	err := db.conn.QueryRowContext(ctx, `
		SELECT max_concurrency, global_rpm, test_model, test_concurrency, proxy_url, pg_max_conns, redis_pool_size,
		       auto_clean_unauthorized, auto_clean_rate_limited, COALESCE(admin_secret, ''), COALESCE(auto_clean_full_usage, false),
		       COALESCE(proxy_pool_enabled, false),
		       COALESCE(fast_scheduler_enabled, false),
		       COALESCE(max_retries, 2),
		       COALESCE(allow_remote_migration, false),
		       COALESCE(auto_clean_error, false),
		       COALESCE(auto_clean_expired, false),
		       COALESCE(model_mapping, '{}'),
		       COALESCE(background_refresh_interval_minutes, 2),
		       COALESCE(usage_probe_max_age_minutes, 10),
		       COALESCE(recovery_probe_interval_minutes, 30),
		       COALESCE(resin_url, ''),
		       COALESCE(resin_platform_name, ''),
		       COALESCE(prompt_filter_enabled, false),
		       COALESCE(prompt_filter_mode, 'monitor'),
		       COALESCE(prompt_filter_threshold, 50),
		       COALESCE(prompt_filter_strict_threshold, 90),
		       COALESCE(prompt_filter_log_matches, true),
		       COALESCE(prompt_filter_max_text_length, 81920),
		       COALESCE(prompt_filter_sensitive_words, ''),
		       COALESCE(prompt_filter_custom_patterns, '[]'),
		       COALESCE(prompt_filter_disabled_patterns, '[]')
		FROM system_settings WHERE id = 1
	`).Scan(
		&s.MaxConcurrency, &s.GlobalRPM, &s.TestModel, &s.TestConcurrency, &s.ProxyURL, &s.PgMaxConns, &s.RedisPoolSize,
		&s.AutoCleanUnauthorized, &s.AutoCleanRateLimited, &s.AdminSecret, &s.AutoCleanFullUsage,
		&s.ProxyPoolEnabled, &s.FastSchedulerEnabled, &s.MaxRetries, &s.AllowRemoteMigration,
		&s.AutoCleanError, &s.AutoCleanExpired, &s.ModelMapping,
		&s.BackgroundRefreshIntervalMinutes, &s.UsageProbeMaxAgeMinutes, &s.RecoveryProbeIntervalMinutes,
		&s.ResinURL, &s.ResinPlatformName,
		&s.PromptFilterEnabled, &s.PromptFilterMode, &s.PromptFilterThreshold, &s.PromptFilterStrictThreshold,
		&s.PromptFilterLogMatches, &s.PromptFilterMaxTextLength, &s.PromptFilterSensitiveWords,
		&s.PromptFilterCustomPatterns, &s.PromptFilterDisabledPatterns,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return s, err
}

// UpdateSystemSettings 更新全局设置（upsert：无行时自动插入）
func (db *DB) UpdateSystemSettings(ctx context.Context, s *SystemSettings) error {
	_, err := db.conn.ExecContext(ctx, `
			INSERT INTO system_settings (
				id, max_concurrency, global_rpm, test_model, test_concurrency, proxy_url, pg_max_conns, redis_pool_size,
				auto_clean_unauthorized, auto_clean_rate_limited, admin_secret, auto_clean_full_usage, proxy_pool_enabled,
				fast_scheduler_enabled, max_retries, allow_remote_migration, auto_clean_error, auto_clean_expired, model_mapping,
				background_refresh_interval_minutes, usage_probe_max_age_minutes, recovery_probe_interval_minutes,
				resin_url, resin_platform_name, prompt_filter_enabled, prompt_filter_mode, prompt_filter_threshold,
				prompt_filter_strict_threshold, prompt_filter_log_matches, prompt_filter_max_text_length,
				prompt_filter_sensitive_words, prompt_filter_custom_patterns, prompt_filter_disabled_patterns
			)
			VALUES (1, $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29, $30, $31, $32)
			ON CONFLICT (id) DO UPDATE SET
				max_concurrency         = EXCLUDED.max_concurrency,
				global_rpm              = EXCLUDED.global_rpm,
				test_model              = EXCLUDED.test_model,
				test_concurrency        = EXCLUDED.test_concurrency,
				proxy_url               = EXCLUDED.proxy_url,
			pg_max_conns            = EXCLUDED.pg_max_conns,
			redis_pool_size         = EXCLUDED.redis_pool_size,
			auto_clean_unauthorized = EXCLUDED.auto_clean_unauthorized,
			auto_clean_rate_limited = EXCLUDED.auto_clean_rate_limited,
				admin_secret            = EXCLUDED.admin_secret,
				auto_clean_full_usage   = EXCLUDED.auto_clean_full_usage,
				proxy_pool_enabled      = EXCLUDED.proxy_pool_enabled,
				fast_scheduler_enabled  = EXCLUDED.fast_scheduler_enabled,
				max_retries             = EXCLUDED.max_retries,
				allow_remote_migration  = EXCLUDED.allow_remote_migration,
				auto_clean_error        = EXCLUDED.auto_clean_error,
				auto_clean_expired      = EXCLUDED.auto_clean_expired,
				model_mapping           = EXCLUDED.model_mapping,
				background_refresh_interval_minutes = EXCLUDED.background_refresh_interval_minutes,
				usage_probe_max_age_minutes = EXCLUDED.usage_probe_max_age_minutes,
				recovery_probe_interval_minutes = EXCLUDED.recovery_probe_interval_minutes,
				resin_url               = EXCLUDED.resin_url,
				resin_platform_name     = EXCLUDED.resin_platform_name,
				prompt_filter_enabled   = EXCLUDED.prompt_filter_enabled,
				prompt_filter_mode      = EXCLUDED.prompt_filter_mode,
				prompt_filter_threshold = EXCLUDED.prompt_filter_threshold,
				prompt_filter_strict_threshold = EXCLUDED.prompt_filter_strict_threshold,
				prompt_filter_log_matches = EXCLUDED.prompt_filter_log_matches,
				prompt_filter_max_text_length = EXCLUDED.prompt_filter_max_text_length,
				prompt_filter_sensitive_words = EXCLUDED.prompt_filter_sensitive_words,
				prompt_filter_custom_patterns = EXCLUDED.prompt_filter_custom_patterns,
				prompt_filter_disabled_patterns = EXCLUDED.prompt_filter_disabled_patterns
		`, s.MaxConcurrency, s.GlobalRPM, s.TestModel, s.TestConcurrency, s.ProxyURL, s.PgMaxConns, s.RedisPoolSize,
		s.AutoCleanUnauthorized, s.AutoCleanRateLimited, s.AdminSecret, s.AutoCleanFullUsage, s.ProxyPoolEnabled,
		s.FastSchedulerEnabled, s.MaxRetries, s.AllowRemoteMigration, s.AutoCleanError, s.AutoCleanExpired, s.ModelMapping,
		s.BackgroundRefreshIntervalMinutes, s.UsageProbeMaxAgeMinutes, s.RecoveryProbeIntervalMinutes,
		s.ResinURL, s.ResinPlatformName, s.PromptFilterEnabled, s.PromptFilterMode, s.PromptFilterThreshold,
		s.PromptFilterStrictThreshold, s.PromptFilterLogMatches, s.PromptFilterMaxTextLength,
		s.PromptFilterSensitiveWords, s.PromptFilterCustomPatterns, s.PromptFilterDisabledPatterns)
	return err
}

// DeleteAPIKey 删除 API 密钥
func (db *DB) DeleteAPIKey(ctx context.Context, id int64) error {
	_, err := db.conn.ExecContext(ctx, `DELETE FROM api_keys WHERE id = $1`, id)
	return err
}

// GetAllAPIKeyValues 获取所有密钥值（用于鉴权）
func (db *DB) GetAllAPIKeyValues(ctx context.Context) ([]string, error) {
	rows, err := db.conn.QueryContext(ctx, `SELECT key FROM api_keys`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var keys []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// ==================== Proxies ====================

// ProxyRow 代理行
type ProxyRow struct {
	ID            int64     `json:"id"`
	URL           string    `json:"url"`
	Label         string    `json:"label"`
	Enabled       bool      `json:"enabled"`
	CreatedAt     time.Time `json:"created_at"`
	TestIP        string    `json:"test_ip"`
	TestLocation  string    `json:"test_location"`
	TestLatencyMs int       `json:"test_latency_ms"`
}

// ListProxies 获取所有代理
func (db *DB) ListProxies(ctx context.Context) ([]*ProxyRow, error) {
	rows, err := db.conn.QueryContext(ctx, `SELECT id, url, label, enabled, created_at, COALESCE(test_ip,''), COALESCE(test_location,''), COALESCE(test_latency_ms,0) FROM proxies ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var proxies []*ProxyRow
	for rows.Next() {
		p := &ProxyRow{}
		var createdAtRaw interface{}
		if err := rows.Scan(&p.ID, &p.URL, &p.Label, &p.Enabled, &createdAtRaw, &p.TestIP, &p.TestLocation, &p.TestLatencyMs); err != nil {
			return nil, err
		}
		p.CreatedAt, err = parseDBTimeValue(createdAtRaw)
		if err != nil {
			return nil, err
		}
		proxies = append(proxies, p)
	}
	return proxies, rows.Err()
}

// ListEnabledProxies 获取已启用的代理
func (db *DB) ListEnabledProxies(ctx context.Context) ([]*ProxyRow, error) {
	query := `SELECT id, url, label, enabled, created_at, COALESCE(test_ip,''), COALESCE(test_location,''), COALESCE(test_latency_ms,0) FROM proxies WHERE enabled = true ORDER BY id`
	if db.isSQLite() {
		query = `SELECT id, url, label, enabled, created_at, COALESCE(test_ip,''), COALESCE(test_location,''), COALESCE(test_latency_ms,0) FROM proxies WHERE enabled = 1 ORDER BY id`
	}
	rows, err := db.conn.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var proxies []*ProxyRow
	for rows.Next() {
		p := &ProxyRow{}
		var createdAtRaw interface{}
		if err := rows.Scan(&p.ID, &p.URL, &p.Label, &p.Enabled, &createdAtRaw, &p.TestIP, &p.TestLocation, &p.TestLatencyMs); err != nil {
			return nil, err
		}
		p.CreatedAt, err = parseDBTimeValue(createdAtRaw)
		if err != nil {
			return nil, err
		}
		proxies = append(proxies, p)
	}
	return proxies, rows.Err()
}

// InsertProxy 插入单个代理
func (db *DB) InsertProxy(ctx context.Context, url, label string) (int64, error) {
	return db.insertRowID(ctx,
		`INSERT INTO proxies (url, label) VALUES ($1, $2) ON CONFLICT (url) DO NOTHING RETURNING id`,
		`INSERT INTO proxies (url, label) VALUES ($1, $2) ON CONFLICT(url) DO NOTHING`,
		url, label,
	)
}

// InsertProxies 批量插入代理（跳过已存在的）
func (db *DB) InsertProxies(ctx context.Context, urls []string, label string) (int, error) {
	inserted := 0
	for _, u := range urls {
		if db.isSQLite() {
			res, err := db.conn.ExecContext(ctx, `INSERT INTO proxies (url, label) VALUES ($1, $2) ON CONFLICT(url) DO NOTHING`, u, label)
			if err != nil {
				continue
			}
			affected, _ := res.RowsAffected()
			if affected > 0 {
				inserted++
			}
			continue
		}
		var id int64
		err := db.conn.QueryRowContext(ctx,
			`INSERT INTO proxies (url, label) VALUES ($1, $2) ON CONFLICT (url) DO NOTHING RETURNING id`, u, label).Scan(&id)
		if err == nil {
			inserted++
		}
	}
	return inserted, nil
}

// DeleteProxy 删除单个代理
func (db *DB) DeleteProxy(ctx context.Context, id int64) error {
	_, err := db.conn.ExecContext(ctx, `DELETE FROM proxies WHERE id = $1`, id)
	return err
}

// DeleteProxies 批量删除代理
func (db *DB) DeleteProxies(ctx context.Context, ids []int64) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	// 构建 IN 子句
	args := make([]interface{}, len(ids))
	placeholders := make([]string, len(ids))
	for i, id := range ids {
		args[i] = id
		placeholders[i] = fmt.Sprintf("$%d", i+1)
	}
	query := fmt.Sprintf("DELETE FROM proxies WHERE id IN (%s)", strings.Join(placeholders, ","))
	res, err := db.conn.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	affected, _ := res.RowsAffected()
	return int(affected), nil
}

// UpdateProxy 更新代理
func (db *DB) UpdateProxy(ctx context.Context, id int64, label *string, enabled *bool) error {
	if label != nil {
		if _, err := db.conn.ExecContext(ctx, `UPDATE proxies SET label = $1 WHERE id = $2`, *label, id); err != nil {
			return err
		}
	}
	if enabled != nil {
		if _, err := db.conn.ExecContext(ctx, `UPDATE proxies SET enabled = $1 WHERE id = $2`, *enabled, id); err != nil {
			return err
		}
	}
	return nil
}

// UpdateProxyTestResult 更新代理测试结果
func (db *DB) UpdateProxyTestResult(ctx context.Context, id int64, ip, location string, latencyMs int) error {
	_, err := db.conn.ExecContext(ctx,
		`UPDATE proxies SET test_ip = $1, test_location = $2, test_latency_ms = $3 WHERE id = $4`,
		ip, location, latencyMs, id)
	return err
}

// ==================== Usage Logs（批量写入） ====================

// UsageLog 请求日志行
type UsageLog struct {
	ID               int64     `json:"id"`
	AccountID        int64     `json:"account_id"`
	Endpoint         string    `json:"endpoint"`
	Model            string    `json:"model"`
	EffectiveModel   string    `json:"effective_model"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	TotalTokens      int       `json:"total_tokens"`
	StatusCode       int       `json:"status_code"`
	DurationMs       int       `json:"duration_ms"`
	InputTokens      int       `json:"input_tokens"`
	OutputTokens     int       `json:"output_tokens"`
	ReasoningTokens  int       `json:"reasoning_tokens"`
	FirstTokenMs     int       `json:"first_token_ms"`
	ReasoningEffort  string    `json:"reasoning_effort"`
	InboundEndpoint  string    `json:"inbound_endpoint"`
	UpstreamEndpoint string    `json:"upstream_endpoint"`
	Stream           bool      `json:"stream"`
	CachedTokens     int       `json:"cached_tokens"`
	ServiceTier      string    `json:"service_tier"`
	APIKeyID         int64     `json:"api_key_id"`
	APIKeyName       string    `json:"api_key_name"`
	APIKeyMasked     string    `json:"api_key_masked"`
	ImageCount       int       `json:"image_count"`
	ImageWidth       int       `json:"image_width"`
	ImageHeight      int       `json:"image_height"`
	ImageBytes       int       `json:"image_bytes"`
	ImageFormat      string    `json:"image_format"`
	ImageSize        string    `json:"image_size"`
	AccountEmail     string    `json:"account_email"`
	CreatedAt        time.Time `json:"created_at"`
	AccountBilled    float64   `json:"account_billed"`
	UserBilled       float64   `json:"user_billed"`
	InputCost        float64   `json:"input_cost"`
	OutputCost       float64   `json:"output_cost"`
	CacheReadCost    float64   `json:"cache_read_cost"`
	TotalCost        float64   `json:"total_cost"`
	InputPrice       float64   `json:"input_price_per_mtoken"`
	OutputPrice      float64   `json:"output_price_per_mtoken"`
	CacheReadPrice   float64   `json:"cache_read_price_per_mtoken"`
	RateMultiplier   float64   `json:"rate_multiplier"`
}

// InsertUsageLog 将日志追加到内存缓冲（非阻塞）
func (db *DB) InsertUsageLog(ctx context.Context, log *UsageLogInput) error {
	// 计算计费金额（基于 input/output tokens 和模型）
	// 使用 EffectiveModel 作为计费模型（如果有映射则使用映射后的模型）
	billingModel := log.EffectiveModel
	if billingModel == "" {
		billingModel = log.Model
	}

	// 计算账号计费金额（标准费用）
	accountBilled := calculateCost(log.InputTokens, log.OutputTokens, log.CachedTokens, billingModel, log.ServiceTier)

	// 用户计费金额与账号计费金额相同（简化版，未来可支持倍率）
	userBilled := accountBilled

	db.logMu.Lock()
	db.logBuf = append(db.logBuf, usageLogEntry{
		AccountID:        log.AccountID,
		Endpoint:         log.Endpoint,
		Model:            log.Model,
		EffectiveModel:   log.EffectiveModel,
		PromptTokens:     log.PromptTokens,
		CompletionTokens: log.CompletionTokens,
		TotalTokens:      log.TotalTokens,
		StatusCode:       log.StatusCode,
		DurationMs:       log.DurationMs,
		InputTokens:      log.InputTokens,
		OutputTokens:     log.OutputTokens,
		ReasoningTokens:  log.ReasoningTokens,
		FirstTokenMs:     log.FirstTokenMs,
		ReasoningEffort:  log.ReasoningEffort,
		InboundEndpoint:  log.InboundEndpoint,
		UpstreamEndpoint: log.UpstreamEndpoint,
		Stream:           log.Stream,
		CachedTokens:     log.CachedTokens,
		ServiceTier:      log.ServiceTier,
		APIKeyID:         log.APIKeyID,
		APIKeyName:       log.APIKeyName,
		APIKeyMasked:     log.APIKeyMasked,
		ImageCount:       log.ImageCount,
		ImageWidth:       log.ImageWidth,
		ImageHeight:      log.ImageHeight,
		ImageBytes:       log.ImageBytes,
		ImageFormat:      log.ImageFormat,
		ImageSize:        log.ImageSize,
		AccountBilled:    accountBilled,
		UserBilled:       userBilled,
	})
	bufLen := len(db.logBuf)
	db.logMu.Unlock()

	// 增加触发 flush 的阈值，减少 flush 频率
	if bufLen >= 200 {
		go db.flushLogs()
	}
	return nil
}

// UsageLogInput 日志写入参数
type UsageLogInput struct {
	AccountID        int64
	Endpoint         string
	Model            string
	EffectiveModel   string
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	StatusCode       int
	DurationMs       int
	InputTokens      int
	OutputTokens     int
	ReasoningTokens  int
	FirstTokenMs     int
	ReasoningEffort  string
	InboundEndpoint  string
	UpstreamEndpoint string
	Stream           bool
	CachedTokens     int
	ServiceTier      string
	APIKeyID         int64
	APIKeyName       string
	APIKeyMasked     string
	ImageCount       int
	ImageWidth       int
	ImageHeight      int
	ImageBytes       int
	ImageFormat      string
	ImageSize        string
}

func (l *UsageLog) populateBillingBreakdown() {
	billingModel := l.EffectiveModel
	if billingModel == "" {
		billingModel = l.Model
	}
	breakdown := calculateCostBreakdown(l.InputTokens, l.OutputTokens, l.CachedTokens, billingModel, l.ServiceTier)
	l.InputCost = breakdown.InputCost
	l.OutputCost = breakdown.OutputCost
	l.CacheReadCost = breakdown.CacheReadCost
	l.TotalCost = breakdown.TotalCost
	l.InputPrice = breakdown.InputPricePerMToken
	l.OutputPrice = breakdown.OutputPricePerMToken
	l.CacheReadPrice = breakdown.CacheReadPricePerMToken
	l.RateMultiplier = breakdown.ServiceTierCostMultiplier

	displayTotal := l.UserBilled
	if displayTotal <= 0 {
		displayTotal = l.AccountBilled
	}
	if displayTotal > 0 && breakdown.TotalCost > 0 && displayTotal != breakdown.TotalCost {
		scale := displayTotal / breakdown.TotalCost
		l.InputCost *= scale
		l.OutputCost *= scale
		l.CacheReadCost *= scale
		l.TotalCost = displayTotal
		l.InputPrice *= scale
		l.OutputPrice *= scale
		l.CacheReadPrice *= scale
	}
}

// startLogFlusher 启动后台定时 flush 协程（每 5 秒一次）
func (db *DB) startLogFlusher() {
	db.logWg.Add(1)
	go func() {
		defer db.logWg.Done()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				db.flushLogs()
			case <-db.logStop:
				return
			}
		}
	}()
}

// flushLogs 将缓冲中的日志批量写入 PG
func (db *DB) flushLogs() {
	db.logMu.Lock()
	if len(db.logBuf) == 0 {
		db.logMu.Unlock()
		return
	}
	batch := db.logBuf
	db.logBuf = make([]usageLogEntry, 0, db.logBufCap)
	db.logMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second) // 增加超时时间
	defer cancel()

	// 使用批处理插入优化性能
	if db.driver == "postgres" {
		err := db.batchInsertLogs(ctx, batch)
		if err != nil {
			log.Printf("批量写入日志失败: %v", err)
		}
		return
	}

	// SQLite 使用事务插入
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		log.Printf("批量写入日志失败（开始事务）: %v", err)
		return
	}

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO usage_logs (account_id, endpoint, model, effective_model, prompt_tokens, completion_tokens, total_tokens, status_code, duration_ms,
		  input_tokens, output_tokens, reasoning_tokens, first_token_ms, reasoning_effort, inbound_endpoint, upstream_endpoint, stream, cached_tokens, service_tier,
		  api_key_id, api_key_name, api_key_masked, image_count, image_width, image_height, image_bytes, image_format, image_size, account_billed, user_billed)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29, $30)`)
	if err != nil {
		tx.Rollback()
		log.Printf("批量写入日志失败（准备语句）: %v", err)
		return
	}
	defer stmt.Close()

	for _, e := range batch {
		if _, err := stmt.ExecContext(ctx, e.AccountID, e.Endpoint, e.Model, e.EffectiveModel, e.PromptTokens, e.CompletionTokens, e.TotalTokens, e.StatusCode, e.DurationMs,
			e.InputTokens, e.OutputTokens, e.ReasoningTokens, e.FirstTokenMs, e.ReasoningEffort, e.InboundEndpoint, e.UpstreamEndpoint, e.Stream, e.CachedTokens, e.ServiceTier,
			e.APIKeyID, e.APIKeyName, e.APIKeyMasked, e.ImageCount, e.ImageWidth, e.ImageHeight, e.ImageBytes, e.ImageFormat, e.ImageSize, e.AccountBilled, e.UserBilled); err != nil {
			tx.Rollback()
			log.Printf("批量写入日志失败（执行）: %v", err)
			return
		}
	}

	if err := tx.Commit(); err != nil {
		log.Printf("批量写入日志失败（提交）: %v", err)
		return
	}

	if len(batch) > 10 {
		log.Printf("批量写入 %d 条使用日志", len(batch))
	}
}

// batchInsertLogs 使用 PostgreSQL 的批量插入优化
// 分批处理以避免 PostgreSQL 65535 参数限制（每行 28 个参数，每批最多 2340 行）
func (db *DB) batchInsertLogs(ctx context.Context, batch []usageLogEntry) error {
	if len(batch) == 0 {
		return nil
	}

	const maxRowsPerBatch = 2300

	// 分批处理
	for start := 0; start < len(batch); start += maxRowsPerBatch {
		end := start + maxRowsPerBatch
		if end > len(batch) {
			end = len(batch)
		}
		subBatch := batch[start:end]

		if err := db.batchInsertLogsChunk(ctx, subBatch); err != nil {
			return err
		}
	}
	return nil
}

// batchInsertLogsChunk 插入单批日志（内部辅助函数）
func (db *DB) batchInsertLogsChunk(ctx context.Context, batch []usageLogEntry) error {
	if len(batch) == 0 {
		return nil
	}

	// 使用 COPY 或批量 VALUES 优化插入性能
	valueStrings := make([]string, 0, len(batch))
	valueArgs := make([]interface{}, 0, len(batch)*30)
	argIdx := 1

	for _, e := range batch {
		valueStrings = append(valueStrings, fmt.Sprintf("($%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d)",
			argIdx, argIdx+1, argIdx+2, argIdx+3, argIdx+4, argIdx+5, argIdx+6, argIdx+7, argIdx+8, argIdx+9,
			argIdx+10, argIdx+11, argIdx+12, argIdx+13, argIdx+14, argIdx+15, argIdx+16, argIdx+17, argIdx+18, argIdx+19,
			argIdx+20, argIdx+21, argIdx+22, argIdx+23, argIdx+24, argIdx+25, argIdx+26, argIdx+27, argIdx+28, argIdx+29))
		valueArgs = append(valueArgs, e.AccountID, e.Endpoint, e.Model, e.EffectiveModel, e.PromptTokens, e.CompletionTokens, e.TotalTokens, e.StatusCode, e.DurationMs,
			e.InputTokens, e.OutputTokens, e.ReasoningTokens, e.FirstTokenMs, e.ReasoningEffort, e.InboundEndpoint, e.UpstreamEndpoint, e.Stream, e.CachedTokens, e.ServiceTier,
			e.APIKeyID, e.APIKeyName, e.APIKeyMasked, e.ImageCount, e.ImageWidth, e.ImageHeight, e.ImageBytes, e.ImageFormat, e.ImageSize, e.AccountBilled, e.UserBilled)
		argIdx += 30
	}

	query := fmt.Sprintf(`INSERT INTO usage_logs (account_id, endpoint, model, effective_model, prompt_tokens, completion_tokens, total_tokens, status_code, duration_ms,
		input_tokens, output_tokens, reasoning_tokens, first_token_ms, reasoning_effort, inbound_endpoint, upstream_endpoint, stream, cached_tokens, service_tier,
		api_key_id, api_key_name, api_key_masked, image_count, image_width, image_height, image_bytes, image_format, image_size, account_billed, user_billed)
		VALUES %s`, strings.Join(valueStrings, ","))

	_, err := db.conn.ExecContext(ctx, query, valueArgs...)
	return err
}

// UsageStats 使用统计
type UsageStats struct {
	TotalRequests      int64   `json:"total_requests"`
	TotalTokens        int64   `json:"total_tokens"`
	TotalPrompt        int64   `json:"total_prompt_tokens"`
	TotalCompletion    int64   `json:"total_completion_tokens"`
	TotalCachedTokens  int64   `json:"total_cached_tokens"`
	TotalAccountBilled float64 `json:"total_account_billed"`
	TotalUserBilled    float64 `json:"total_user_billed"`
	TodayRequests      int64   `json:"today_requests"`
	TodayTokens        int64   `json:"today_tokens"`
	TodayAccountBilled float64 `json:"today_account_billed"`
	TodayUserBilled    float64 `json:"today_user_billed"`
	RPM                float64 `json:"rpm"`
	TPM                float64 `json:"tpm"`
	AvgDurationMs      float64 `json:"avg_duration_ms"`
	ErrorRate          float64 `json:"error_rate"`
}

// TrafficSnapshot 近实时流量快照
type TrafficSnapshot struct {
	QPS     float64 `json:"qps"`
	QPSPeak float64 `json:"qps_peak"`
	TPS     float64 `json:"tps"`
	TPSPeak float64 `json:"tps_peak"`
}

// GetUsageStats 获取使用统计（基线 + 当前日志）
func (db *DB) GetUsageStats(ctx context.Context) (*UsageStats, error) {
	if db.isSQLite() {
		return db.getUsageStatsSQLite(ctx)
	}

	stats := &UsageStats{}
	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	minuteAgo := now.Add(-1 * time.Minute)

	todayQuery := `
	SELECT
		COUNT(*) AS today_requests,
		COALESCE(SUM(total_tokens), 0) AS today_tokens,
			COALESCE(SUM(prompt_tokens), 0) AS today_prompt,
			COALESCE(SUM(completion_tokens), 0) AS today_completion,
			COALESCE(SUM(cached_tokens), 0) AS today_cached,
			COALESCE(SUM(account_billed), 0) AS today_account_billed,
			COALESCE(SUM(user_billed), 0) AS today_user_billed,
			COALESCE(SUM(CASE WHEN created_at >= $2 THEN 1 ELSE 0 END), 0) AS rpm,
			COALESCE(SUM(CASE WHEN created_at >= $2 THEN total_tokens ELSE 0 END), 0) AS tpm,
			COALESCE(AVG(duration_ms), 0) AS avg_duration_ms,
		COALESCE(SUM(CASE WHEN status_code >= 400 THEN 1 ELSE 0 END), 0) AS today_errors
	FROM usage_logs
	WHERE created_at >= $1
	  AND status_code <> 499
	`

	var todayErrors int64
	err := db.conn.QueryRowContext(ctx, todayQuery, todayStart, minuteAgo).Scan(
		&stats.TodayRequests, &stats.TodayTokens, &stats.TotalPrompt, &stats.TotalCompletion, &stats.TotalCachedTokens,
		&stats.TodayAccountBilled, &stats.TodayUserBilled,
		&stats.RPM, &stats.TPM,
		&stats.AvgDurationMs,
		&todayErrors,
	)
	if err != nil {
		return nil, err
	}

	// 统计当前可见请求总数和计费总额（排除 499，保证与使用统计列表口径一致）
	var visibleTotal int64
	var currentAccountBilled, currentUserBilled float64
	_ = db.conn.QueryRowContext(ctx, `
			SELECT COUNT(*), COALESCE(SUM(account_billed), 0), COALESCE(SUM(user_billed), 0)
			FROM usage_logs
			WHERE status_code <> 499
		`).Scan(&visibleTotal, &currentAccountBilled, &currentUserBilled)

	// 加上基线值（清空日志前保存的累计值）
	var bReq, bTok, bPrompt, bComp, bCached int64
	var bAccountBilled, bUserBilled float64
	_ = db.conn.QueryRowContext(ctx, `
			SELECT total_requests, total_tokens, prompt_tokens, completion_tokens, cached_tokens, account_billed, user_billed
			FROM usage_stats_baseline WHERE id = 1
		`).Scan(&bReq, &bTok, &bPrompt, &bComp, &bCached, &bAccountBilled, &bUserBilled)

	stats.TotalRequests = visibleTotal + bReq
	stats.TotalTokens = stats.TodayTokens + bTok
	stats.TotalPrompt += bPrompt
	stats.TotalCompletion += bComp
	stats.TotalCachedTokens += bCached
	stats.TotalAccountBilled = currentAccountBilled + bAccountBilled
	stats.TotalUserBilled = currentUserBilled + bUserBilled

	if stats.TodayRequests > 0 {
		stats.ErrorRate = float64(todayErrors) / float64(stats.TodayRequests) * 100
	}

	return stats, nil
}

// GetTrafficSnapshot 获取近实时流量快照
func (db *DB) GetTrafficSnapshot(ctx context.Context) (*TrafficSnapshot, error) {
	if db.isSQLite() {
		return db.getTrafficSnapshotSQLite(ctx)
	}

	snapshot := &TrafficSnapshot{}
	query := `
	WITH per_second AS (
		SELECT
			date_trunc('second', created_at) AS sec,
			COUNT(*)::float8 AS req_count,
			COALESCE(SUM(total_tokens), 0)::float8 AS token_count
		FROM usage_logs
		WHERE created_at >= NOW() - INTERVAL '5 minutes'
		GROUP BY 1
	),
	current_window AS (
		SELECT
			COALESCE(SUM(req_count), 0)::float8 AS req_10s,
			COALESCE(SUM(token_count), 0)::float8 AS tok_10s
		FROM per_second
		WHERE sec >= date_trunc('second', NOW() - INTERVAL '10 seconds')
	)
	SELECT
		COALESCE((SELECT req_10s FROM current_window), 0) / 10.0 AS qps,
		COALESCE(MAX(req_count), 0) AS qps_peak,
		COALESCE((SELECT tok_10s FROM current_window), 0) / 10.0 AS tps,
		COALESCE(MAX(token_count), 0) AS tps_peak
	FROM per_second
	`

	err := db.conn.QueryRowContext(ctx, query).Scan(
		&snapshot.QPS,
		&snapshot.QPSPeak,
		&snapshot.TPS,
		&snapshot.TPSPeak,
	)
	if err != nil {
		return nil, err
	}

	return snapshot, nil
}

// ListRecentUsageLogs 获取最近的请求日志
func (db *DB) ListRecentUsageLogs(ctx context.Context, limit int) ([]*UsageLog, error) {
	if limit <= 0 || limit > 5000 {
		limit = 50
	}
	query := `SELECT u.id, u.account_id, u.endpoint, u.model, COALESCE(u.effective_model, ''), u.prompt_tokens, u.completion_tokens, u.total_tokens, u.status_code, u.duration_ms,
	            COALESCE(u.input_tokens, 0), COALESCE(u.output_tokens, 0), COALESCE(u.reasoning_tokens, 0),
	            COALESCE(u.first_token_ms, 0), COALESCE(u.reasoning_effort, ''), COALESCE(u.inbound_endpoint, ''),
	            COALESCE(u.upstream_endpoint, ''), COALESCE(u.stream, false), COALESCE(u.cached_tokens, 0), COALESCE(u.service_tier, ''),
	            COALESCE(u.api_key_id, 0), COALESCE(u.api_key_name, ''), COALESCE(u.api_key_masked, ''),
	            COALESCE(u.image_count, 0), COALESCE(u.image_width, 0), COALESCE(u.image_height, 0), COALESCE(u.image_bytes, 0),
		            COALESCE(u.image_format, ''), COALESCE(u.image_size, ''),
		            COALESCE(u.account_billed, 0), COALESCE(u.user_billed, 0),
		            COALESCE(CAST(a.credentials AS TEXT), '{}'), u.created_at
	           FROM usage_logs u
	           LEFT JOIN accounts a ON u.account_id = a.id
	           WHERE u.status_code <> 499
	           ORDER BY u.id DESC LIMIT $1`
	rows, err := db.conn.QueryContext(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var logs []*UsageLog
	for rows.Next() {
		l := &UsageLog{}
		var credentialRaw interface{}
		var createdAtRaw interface{}
		if err := rows.Scan(&l.ID, &l.AccountID, &l.Endpoint, &l.Model, &l.EffectiveModel, &l.PromptTokens, &l.CompletionTokens, &l.TotalTokens, &l.StatusCode, &l.DurationMs,
			&l.InputTokens, &l.OutputTokens, &l.ReasoningTokens, &l.FirstTokenMs, &l.ReasoningEffort, &l.InboundEndpoint, &l.UpstreamEndpoint, &l.Stream, &l.CachedTokens, &l.ServiceTier,
			&l.APIKeyID, &l.APIKeyName, &l.APIKeyMasked, &l.ImageCount, &l.ImageWidth, &l.ImageHeight, &l.ImageBytes, &l.ImageFormat, &l.ImageSize, &l.AccountBilled, &l.UserBilled,
			&credentialRaw, &createdAtRaw); err != nil {
			return nil, err
		}
		l.AccountEmail = accountEmailFromRawCredentials(credentialRaw)
		l.CreatedAt, err = parseDBTimeValue(createdAtRaw)
		if err != nil {
			return nil, err
		}
		l.populateBillingBreakdown()
		logs = append(logs, l)
	}
	return logs, rows.Err()
}

// ==================== 图表聚合（服务端） ====================

// ChartTimelinePoint 时间轴聚合点
type ChartTimelinePoint struct {
	Bucket          string  `json:"bucket"`
	Requests        int64   `json:"requests"`
	AvgLatency      float64 `json:"avg_latency"`
	InputTokens     int64   `json:"input_tokens"`
	OutputTokens    int64   `json:"output_tokens"`
	ReasoningTokens int64   `json:"reasoning_tokens"`
	CachedTokens    int64   `json:"cached_tokens"`
	Errors401       int64   `json:"errors_401"`
}

// ChartModelPoint 模型排行聚合点
type ChartModelPoint struct {
	Model    string `json:"model"`
	Requests int64  `json:"requests"`
}

// ChartAggregation 仪表盘图表聚合结果
type ChartAggregation struct {
	Timeline []ChartTimelinePoint `json:"timeline"`
	Models   []ChartModelPoint    `json:"models"`
}

// AccountEventPoint 账号事件趋势数据点
type AccountEventPoint struct {
	Bucket  string `json:"bucket"`
	Added   int    `json:"added"`
	Deleted int    `json:"deleted"`
}

// AccountModelStat 单个模型的使用统计
type AccountModelStat struct {
	Model    string `json:"model"`
	Requests int64  `json:"requests"`
	Tokens   int64  `json:"tokens"`
}

// AccountUsageDetail 单账号用量详情
type AccountUsageDetail struct {
	TotalRequests   int64              `json:"total_requests"`
	TotalTokens     int64              `json:"total_tokens"`
	InputTokens     int64              `json:"input_tokens"`
	OutputTokens    int64              `json:"output_tokens"`
	ReasoningTokens int64              `json:"reasoning_tokens"`
	CachedTokens    int64              `json:"cached_tokens"`
	Models          []AccountModelStat `json:"models"`
}

// GetChartAggregation 在数据库层完成图表数据的分桶聚合（无需传输原始行）
func (db *DB) GetChartAggregation(ctx context.Context, start, end time.Time, bucketMinutes int) (*ChartAggregation, error) {
	if db.isSQLite() {
		return db.getChartAggregationSQLite(ctx, start, end, bucketMinutes)
	}

	if bucketMinutes < 1 {
		bucketMinutes = 5
	}
	result := &ChartAggregation{}

	// 时间轴聚合：按 bucketMinutes 分桶
	timelineQuery := `
	SELECT
		TO_CHAR(
			date_trunc('minute', created_at)
			- (EXTRACT(MINUTE FROM created_at)::int % $3) * INTERVAL '1 minute',
			'YYYY-MM-DD"T"HH24:MI:SS'
		) AS bucket,
		COUNT(*)                              AS requests,
		COALESCE(AVG(duration_ms), 0)         AS avg_latency,
		COALESCE(SUM(input_tokens), 0)        AS input_tokens,
		COALESCE(SUM(output_tokens), 0)       AS output_tokens,
		COALESCE(SUM(reasoning_tokens), 0)    AS reasoning_tokens,
		COALESCE(SUM(cached_tokens), 0)       AS cached_tokens,
		COALESCE(SUM(CASE WHEN status_code = 401 THEN 1 ELSE 0 END), 0) AS errors_401
	FROM usage_logs
	WHERE created_at >= $1 AND created_at <= $2
	  AND status_code <> 499
	GROUP BY 1
	ORDER BY 1`

	rows, err := db.conn.QueryContext(ctx, timelineQuery, start, end, bucketMinutes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var p ChartTimelinePoint
		if err := rows.Scan(&p.Bucket, &p.Requests, &p.AvgLatency, &p.InputTokens, &p.OutputTokens, &p.ReasoningTokens, &p.CachedTokens, &p.Errors401); err != nil {
			return nil, err
		}
		result.Timeline = append(result.Timeline, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if result.Timeline == nil {
		result.Timeline = []ChartTimelinePoint{}
	}

	// 模型排行聚合：Top 10
	modelQuery := `
	SELECT COALESCE(model, 'unknown'), COUNT(*) AS requests
	FROM usage_logs
	WHERE created_at >= $1 AND created_at <= $2
	  AND status_code <> 499
	GROUP BY 1
	ORDER BY 2 DESC
	LIMIT 10`

	mRows, err := db.conn.QueryContext(ctx, modelQuery, start, end)
	if err != nil {
		return nil, err
	}
	defer mRows.Close()

	for mRows.Next() {
		var m ChartModelPoint
		if err := mRows.Scan(&m.Model, &m.Requests); err != nil {
			return nil, err
		}
		result.Models = append(result.Models, m)
	}
	if result.Models == nil {
		result.Models = []ChartModelPoint{}
	}

	return result, mRows.Err()
}

// GetAccountUsageStats 查询单个账号的用量统计和模型分布
func (db *DB) GetAccountUsageStats(ctx context.Context, accountID int64) (*AccountUsageDetail, error) {
	result := &AccountUsageDetail{}

	// 汇总统计
	summaryQuery := `
	SELECT
		COUNT(*),
		COALESCE(SUM(total_tokens), 0),
		COALESCE(SUM(input_tokens), 0),
		COALESCE(SUM(output_tokens), 0),
		COALESCE(SUM(reasoning_tokens), 0),
		COALESCE(SUM(cached_tokens), 0)
	FROM usage_logs
	WHERE account_id = $1 AND status_code <> 499`

	if err := db.conn.QueryRowContext(ctx, summaryQuery, accountID).Scan(
		&result.TotalRequests, &result.TotalTokens,
		&result.InputTokens, &result.OutputTokens,
		&result.ReasoningTokens, &result.CachedTokens,
	); err != nil {
		return nil, err
	}

	// 模型分布
	modelQuery := `
	SELECT COALESCE(model, 'unknown'), COUNT(*) AS requests, COALESCE(SUM(total_tokens), 0) AS tokens
	FROM usage_logs
	WHERE account_id = $1 AND status_code <> 499
	GROUP BY 1
	ORDER BY 2 DESC`

	rows, err := db.conn.QueryContext(ctx, modelQuery, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var m AccountModelStat
		if err := rows.Scan(&m.Model, &m.Requests, &m.Tokens); err != nil {
			return nil, err
		}
		result.Models = append(result.Models, m)
	}
	if result.Models == nil {
		result.Models = []AccountModelStat{}
	}

	return result, rows.Err()
}

// ListUsageLogsByTimeRange 按时间范围查询请求日志
func (db *DB) ListUsageLogsByTimeRange(ctx context.Context, start, end time.Time) ([]*UsageLog, error) {
	startArg, endArg := db.timeRangeArgs(start, end)
	query := `SELECT u.id, u.account_id, u.endpoint, u.model, COALESCE(u.effective_model, ''), u.prompt_tokens, u.completion_tokens, u.total_tokens, u.status_code, u.duration_ms,
	            COALESCE(u.input_tokens, 0), COALESCE(u.output_tokens, 0), COALESCE(u.reasoning_tokens, 0),
	            COALESCE(u.first_token_ms, 0), COALESCE(u.reasoning_effort, ''), COALESCE(u.inbound_endpoint, ''),
	            COALESCE(u.upstream_endpoint, ''), COALESCE(u.stream, false), COALESCE(u.cached_tokens, 0), COALESCE(u.service_tier, ''),
	            COALESCE(u.api_key_id, 0), COALESCE(u.api_key_name, ''), COALESCE(u.api_key_masked, ''),
	            COALESCE(u.image_count, 0), COALESCE(u.image_width, 0), COALESCE(u.image_height, 0), COALESCE(u.image_bytes, 0),
		            COALESCE(u.image_format, ''), COALESCE(u.image_size, ''),
		            COALESCE(u.account_billed, 0), COALESCE(u.user_billed, 0),
		            COALESCE(CAST(a.credentials AS TEXT), '{}'), u.created_at
	           FROM usage_logs u
	           LEFT JOIN accounts a ON u.account_id = a.id
	           WHERE u.created_at >= $1 AND u.created_at <= $2
	             AND u.status_code <> 499
	           ORDER BY u.created_at ASC`
	rows, err := db.conn.QueryContext(ctx, query, startArg, endArg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var logs []*UsageLog
	for rows.Next() {
		l := &UsageLog{}
		var credentialRaw interface{}
		var createdAtRaw interface{}
		if err := rows.Scan(&l.ID, &l.AccountID, &l.Endpoint, &l.Model, &l.EffectiveModel, &l.PromptTokens, &l.CompletionTokens, &l.TotalTokens, &l.StatusCode, &l.DurationMs,
			&l.InputTokens, &l.OutputTokens, &l.ReasoningTokens, &l.FirstTokenMs, &l.ReasoningEffort, &l.InboundEndpoint, &l.UpstreamEndpoint, &l.Stream, &l.CachedTokens, &l.ServiceTier,
			&l.APIKeyID, &l.APIKeyName, &l.APIKeyMasked, &l.ImageCount, &l.ImageWidth, &l.ImageHeight, &l.ImageBytes, &l.ImageFormat, &l.ImageSize, &l.AccountBilled, &l.UserBilled,
			&credentialRaw, &createdAtRaw); err != nil {
			return nil, err
		}
		l.AccountEmail = accountEmailFromRawCredentials(credentialRaw)
		l.CreatedAt, err = parseDBTimeValue(createdAtRaw)
		if err != nil {
			return nil, err
		}
		l.populateBillingBreakdown()
		logs = append(logs, l)
	}
	return logs, rows.Err()
}

// UsageLogPage 分页日志结果
type UsageLogPage struct {
	Logs  []*UsageLog `json:"logs"`
	Total int64       `json:"total"`
}

// UsageLogFilter 日志查询过滤条件
type UsageLogFilter struct {
	Start      time.Time
	End        time.Time
	Page       int
	PageSize   int
	Email      string // LIKE 模糊匹配
	Model      string // 精确匹配
	Endpoint   string // 精确匹配 inbound_endpoint
	APIKeyID   *int64 // nil=全部
	FastOnly   *bool  // nil=全部, true=仅fast, false=仅非fast
	StreamOnly *bool  // nil=全部, true=仅stream, false=仅sync
}

// ListUsageLogsByTimeRangePaged 按时间范围分页查询请求日志（支持筛选）
func (db *DB) ListUsageLogsByTimeRangePaged(ctx context.Context, f UsageLogFilter) (*UsageLogPage, error) {
	if f.Page < 1 {
		f.Page = 1
	}
	if f.PageSize < 1 || f.PageSize > 200 {
		f.PageSize = 20
	}

	// 动态拼接 WHERE 条件
	where := `u.created_at >= $1 AND u.created_at <= $2 AND u.status_code <> 499`
	startArg, endArg := db.timeRangeArgs(f.Start, f.End)
	args := []interface{}{startArg, endArg}
	paramIdx := 3

	if f.Email != "" {
		where += fmt.Sprintf(` AND LOWER(COALESCE(CAST(a.credentials AS TEXT), '')) LIKE LOWER($%d)`, paramIdx)
		args = append(args, "%"+f.Email+"%")
		paramIdx++
	}
	if f.Model != "" {
		where += fmt.Sprintf(` AND (u.model = $%d OR COALESCE(u.effective_model, '') = $%d)`, paramIdx, paramIdx)
		args = append(args, f.Model)
		paramIdx++
	}
	if f.Endpoint != "" {
		where += fmt.Sprintf(` AND u.inbound_endpoint = $%d`, paramIdx)
		args = append(args, f.Endpoint)
		paramIdx++
	}
	if f.APIKeyID != nil {
		where += fmt.Sprintf(` AND COALESCE(u.api_key_id, 0) = $%d`, paramIdx)
		args = append(args, *f.APIKeyID)
		paramIdx++
	}
	if f.FastOnly != nil {
		if *f.FastOnly {
			where += ` AND COALESCE(u.service_tier, '') = 'fast'`
		} else {
			where += ` AND COALESCE(u.service_tier, '') <> 'fast'`
		}
	}
	if f.StreamOnly != nil {
		where += fmt.Sprintf(` AND COALESCE(u.stream, false) = $%d`, paramIdx)
		args = append(args, *f.StreamOnly)
		paramIdx++
	}

	offset := (f.Page - 1) * f.PageSize
	where += fmt.Sprintf(` ORDER BY u.created_at DESC LIMIT $%d OFFSET $%d`, paramIdx, paramIdx+1)
	args = append(args, f.PageSize, offset)

	query := `SELECT u.id, u.account_id, u.endpoint, u.model, COALESCE(u.effective_model, ''), u.prompt_tokens, u.completion_tokens, u.total_tokens, u.status_code, u.duration_ms,
	            COALESCE(u.input_tokens, 0), COALESCE(u.output_tokens, 0), COALESCE(u.reasoning_tokens, 0),
	            COALESCE(u.first_token_ms, 0), COALESCE(u.reasoning_effort, ''), COALESCE(u.inbound_endpoint, ''),
	            COALESCE(u.upstream_endpoint, ''), COALESCE(u.stream, false), COALESCE(u.cached_tokens, 0), COALESCE(u.service_tier, ''),
	            COALESCE(u.api_key_id, 0), COALESCE(u.api_key_name, ''), COALESCE(u.api_key_masked, ''),
	            COALESCE(u.image_count, 0), COALESCE(u.image_width, 0), COALESCE(u.image_height, 0), COALESCE(u.image_bytes, 0),
		            COALESCE(u.image_format, ''), COALESCE(u.image_size, ''),
		            COALESCE(u.account_billed, 0), COALESCE(u.user_billed, 0),
		            COALESCE(CAST(a.credentials AS TEXT), '{}'), u.created_at,
	            COUNT(*) OVER() AS total_count
	           FROM usage_logs u
	           LEFT JOIN accounts a ON u.account_id = a.id
	           WHERE ` + where

	rows, err := db.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := &UsageLogPage{}
	for rows.Next() {
		l := &UsageLog{}
		var credentialRaw interface{}
		var createdAtRaw interface{}
		if err := rows.Scan(&l.ID, &l.AccountID, &l.Endpoint, &l.Model, &l.EffectiveModel, &l.PromptTokens, &l.CompletionTokens, &l.TotalTokens, &l.StatusCode, &l.DurationMs,
			&l.InputTokens, &l.OutputTokens, &l.ReasoningTokens, &l.FirstTokenMs, &l.ReasoningEffort, &l.InboundEndpoint, &l.UpstreamEndpoint, &l.Stream, &l.CachedTokens,
			&l.ServiceTier, &l.APIKeyID, &l.APIKeyName, &l.APIKeyMasked, &l.ImageCount, &l.ImageWidth, &l.ImageHeight, &l.ImageBytes, &l.ImageFormat, &l.ImageSize,
			&l.AccountBilled, &l.UserBilled, &credentialRaw, &createdAtRaw, &result.Total); err != nil {
			return nil, err
		}
		l.AccountEmail = accountEmailFromRawCredentials(credentialRaw)
		l.CreatedAt, err = parseDBTimeValue(createdAtRaw)
		if err != nil {
			return nil, err
		}
		l.populateBillingBreakdown()
		result.Logs = append(result.Logs, l)
	}
	if result.Logs == nil {
		result.Logs = []*UsageLog{}
	}
	return result, rows.Err()
}

// ClearUsageLogs 清空所有使用日志（先快照累计值到基线表）
func (db *DB) ClearUsageLogs(ctx context.Context) error {
	// 先将当前日志的累计值叠加到基线表
	_, err := db.conn.ExecContext(ctx, `
		UPDATE usage_stats_baseline SET
			total_requests  = total_requests  + COALESCE((SELECT COUNT(*) FROM usage_logs WHERE status_code <> 499), 0),
				total_tokens    = total_tokens    + COALESCE((SELECT SUM(total_tokens) FROM usage_logs WHERE status_code <> 499), 0),
				prompt_tokens   = prompt_tokens   + COALESCE((SELECT SUM(prompt_tokens) FROM usage_logs WHERE status_code <> 499), 0),
				completion_tokens = completion_tokens + COALESCE((SELECT SUM(completion_tokens) FROM usage_logs WHERE status_code <> 499), 0),
				cached_tokens   = cached_tokens   + COALESCE((SELECT SUM(cached_tokens) FROM usage_logs WHERE status_code <> 499), 0),
				account_billed  = account_billed  + COALESCE((SELECT SUM(account_billed) FROM usage_logs WHERE status_code <> 499), 0),
				user_billed     = user_billed     + COALESCE((SELECT SUM(user_billed) FROM usage_logs WHERE status_code <> 499), 0)
			WHERE id = 1
		`)
	if err != nil {
		return fmt.Errorf("快照统计基线失败: %w", err)
	}

	// 再清空日志
	if db.isSQLite() {
		if _, err = db.conn.ExecContext(ctx, `DELETE FROM usage_logs`); err != nil {
			return err
		}
		_, err = db.conn.ExecContext(ctx, `DELETE FROM sqlite_sequence WHERE name = 'usage_logs'`)
		return err
	}
	_, err = db.conn.ExecContext(ctx, `TRUNCATE TABLE usage_logs RESTART IDENTITY`)
	return err
}

// Ping 检查 PostgreSQL 连通性
func (db *DB) Ping(ctx context.Context) error {
	return db.conn.PingContext(ctx)
}

// Stats 返回 PostgreSQL 连接池状态
func (db *DB) Stats() sql.DBStats {
	return db.conn.Stats()
}

// AccountRequestCount 每个账号的请求统计
type AccountRequestCount struct {
	AccountID    int64
	SuccessCount int64
	ErrorCount   int64
}

// AccountTimeRangeUsage 每个账号在指定时间窗口内的真实请求/token 统计。
type AccountTimeRangeUsage struct {
	AccountID     int64
	Requests      int64
	Tokens        int64
	AccountBilled float64
	UserBilled    float64
}

// GetAccountRequestCounts 按 account_id 聚合近 7 天成功/失败请求数
func (db *DB) GetAccountRequestCounts(ctx context.Context) (map[int64]*AccountRequestCount, error) {
	since := time.Now().AddDate(0, 0, -7)
	query := `
	SELECT account_id,
		COALESCE(SUM(CASE WHEN status_code < 400 THEN 1 ELSE 0 END), 0) AS success_count,
		COALESCE(SUM(CASE WHEN status_code >= 400 THEN 1 ELSE 0 END), 0) AS error_count
	FROM usage_logs
	WHERE created_at >= $1
	GROUP BY account_id
	`
	rows, err := db.conn.QueryContext(ctx, query, db.timeArg(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[int64]*AccountRequestCount)
	for rows.Next() {
		rc := &AccountRequestCount{}
		if err := rows.Scan(&rc.AccountID, &rc.SuccessCount, &rc.ErrorCount); err != nil {
			return nil, err
		}
		result[rc.AccountID] = rc
	}
	return result, rows.Err()
}

// GetAccountTimeRangeUsage 按 account_id 聚合 since 之后的请求数和 token 数。
func (db *DB) GetAccountTimeRangeUsage(ctx context.Context, since time.Time) (map[int64]*AccountTimeRangeUsage, error) {
	query := `
	SELECT account_id,
		COUNT(*) AS requests,
		COALESCE(SUM(total_tokens), 0) AS tokens,
		COALESCE(SUM(account_billed), 0) AS account_billed,
		COALESCE(SUM(user_billed), 0) AS user_billed
	FROM usage_logs
	WHERE created_at >= $1 AND status_code <> 499
	GROUP BY account_id
	`
	rows, err := db.conn.QueryContext(ctx, query, db.timeArg(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[int64]*AccountTimeRangeUsage)
	for rows.Next() {
		usage := &AccountTimeRangeUsage{}
		if err := rows.Scan(&usage.AccountID, &usage.Requests, &usage.Tokens, &usage.AccountBilled, &usage.UserBilled); err != nil {
			return nil, err
		}
		result[usage.AccountID] = usage
	}
	return result, rows.Err()
}

// ==================== Accounts ====================

// ListActive 获取所有未删除账号。
func (db *DB) ListActive(ctx context.Context) ([]*AccountRow, error) {
	query := `
		SELECT id, name, platform, type, credentials, proxy_url, status, cooldown_reason, cooldown_until, error_message, COALESCE(enabled, true), COALESCE(locked, false), score_bias_override, base_concurrency_override, created_at, updated_at
		FROM accounts
		WHERE status <> 'deleted' AND COALESCE(error_message, '') <> 'deleted'
		ORDER BY id
	`
	rows, err := db.conn.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("查询账号失败: %w", err)
	}
	defer rows.Close()

	var accounts []*AccountRow
	for rows.Next() {
		a := &AccountRow{}
		var credRaw interface{}
		var cooldownUntilRaw interface{}
		var createdAtRaw interface{}
		var updatedAtRaw interface{}
		if err := rows.Scan(
			&a.ID,
			&a.Name,
			&a.Platform,
			&a.Type,
			&credRaw,
			&a.ProxyURL,
			&a.Status,
			&a.CooldownReason,
			&cooldownUntilRaw,
			&a.ErrorMessage,
			&a.Enabled,
			&a.Locked,
			&a.ScoreBiasOverride,
			&a.BaseConcurrencyOverride,
			&createdAtRaw,
			&updatedAtRaw,
		); err != nil {
			return nil, fmt.Errorf("扫描账号行失败: %w", err)
		}
		a.Credentials = decodeCredentials(credRaw)
		a.CooldownUntil, err = parseDBNullTimeValue(cooldownUntilRaw)
		if err != nil {
			return nil, fmt.Errorf("解析 cooldown_until 失败: %w", err)
		}
		a.CreatedAt, err = parseDBTimeValue(createdAtRaw)
		if err != nil {
			return nil, fmt.Errorf("解析 created_at 失败: %w", err)
		}
		a.UpdatedAt, err = parseDBTimeValue(updatedAtRaw)
		if err != nil {
			return nil, fmt.Errorf("解析 updated_at 失败: %w", err)
		}
		accounts = append(accounts, a)
	}
	return accounts, rows.Err()
}

// GetAccountByID 获取未删除账号的完整数据库行。
func (db *DB) GetAccountByID(ctx context.Context, id int64) (*AccountRow, error) {
	query := `
		SELECT id, name, platform, type, credentials, proxy_url, status, cooldown_reason, cooldown_until, error_message, COALESCE(enabled, true), COALESCE(locked, false), score_bias_override, base_concurrency_override, created_at, updated_at
		FROM accounts
		WHERE id = $1 AND status <> 'deleted' AND COALESCE(error_message, '') <> 'deleted'
		LIMIT 1
	`
	a := &AccountRow{}
	var credRaw interface{}
	var cooldownUntilRaw interface{}
	var createdAtRaw interface{}
	var updatedAtRaw interface{}
	err := db.conn.QueryRowContext(ctx, query, id).Scan(
		&a.ID,
		&a.Name,
		&a.Platform,
		&a.Type,
		&credRaw,
		&a.ProxyURL,
		&a.Status,
		&a.CooldownReason,
		&cooldownUntilRaw,
		&a.ErrorMessage,
		&a.Enabled,
		&a.Locked,
		&a.ScoreBiasOverride,
		&a.BaseConcurrencyOverride,
		&createdAtRaw,
		&updatedAtRaw,
	)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, sql.ErrNoRows
		}
		return nil, fmt.Errorf("查询账号失败: %w", err)
	}
	a.Credentials = decodeCredentials(credRaw)
	a.CooldownUntil, err = parseDBNullTimeValue(cooldownUntilRaw)
	if err != nil {
		return nil, fmt.Errorf("解析 cooldown_until 失败: %w", err)
	}
	a.CreatedAt, err = parseDBTimeValue(createdAtRaw)
	if err != nil {
		return nil, fmt.Errorf("解析 created_at 失败: %w", err)
	}
	a.UpdatedAt, err = parseDBTimeValue(updatedAtRaw)
	if err != nil {
		return nil, fmt.Errorf("解析 updated_at 失败: %w", err)
	}
	return a, nil
}

// UpdateAccountSchedulerConfig 更新账号调度配置。
func (db *DB) UpdateAccountSchedulerConfig(ctx context.Context, id int64, scoreBiasOverride sql.NullInt64, baseConcurrencyOverride sql.NullInt64, allowedAPIKeyIDs OptionalInt64Slice) error {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	result, err := tx.ExecContext(ctx, `
		UPDATE accounts
		SET score_bias_override = $1,
		    base_concurrency_override = $2,
		    updated_at = CURRENT_TIMESTAMP
		WHERE id = $3
	`, nullableInt64Value(scoreBiasOverride), nullableInt64Value(baseConcurrencyOverride), id)
	if err != nil {
		return err
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rowsAffected == 0 {
		return sql.ErrNoRows
	}

	if allowedAPIKeyIDs.Set {
		selectQuery := `SELECT credentials FROM accounts WHERE id = $1`
		if db.isSQLite() {
			selectQuery = `SELECT credentials FROM accounts WHERE id = ?`
		} else {
			selectQuery += ` FOR UPDATE`
		}

		var currentRaw interface{}
		if err := tx.QueryRowContext(ctx, selectQuery, id).Scan(&currentRaw); err != nil {
			return err
		}

		merged := mergeCredentialMaps(decodeCredentials(currentRaw), map[string]interface{}{
			"allowed_api_key_ids": normalizePositiveInt64Slice(allowedAPIKeyIDs.Values),
		})
		credJSON, err := json.Marshal(merged)
		if err != nil {
			return fmt.Errorf("序列化 credentials 失败: %w", err)
		}

		updateQuery := `UPDATE accounts SET credentials = $1, updated_at = CURRENT_TIMESTAMP WHERE id = $2`
		if !db.isSQLite() {
			updateQuery = `UPDATE accounts SET credentials = $1::jsonb, updated_at = CURRENT_TIMESTAMP WHERE id = $2`
		}
		if _, err := tx.ExecContext(ctx, updateQuery, credJSON, id); err != nil {
			return err
		}
	}

	return tx.Commit()
}

func nullableInt64Value(v sql.NullInt64) interface{} {
	if !v.Valid {
		return nil
	}
	return v.Int64
}

// SetAccountEnabled 设置账号是否参与调度选择
func (db *DB) SetAccountEnabled(ctx context.Context, id int64, enabled bool) error {
	res, err := db.conn.ExecContext(ctx, `UPDATE accounts SET enabled = $1, updated_at = CURRENT_TIMESTAMP WHERE id = $2`, enabled, id)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SetAccountLocked 设置账号的锁定状态
func (db *DB) SetAccountLocked(ctx context.Context, id int64, locked bool) error {
	_, err := db.conn.ExecContext(ctx, `UPDATE accounts SET locked = $1 WHERE id = $2`, locked, id)
	return err
}

// UpdateCredentials 原子合并更新账号的 credentials（JSONB || 运算符，不覆盖已有字段）
// 解决并发刷新时一个进程覆盖另一个进程写入的字段的问题
func (db *DB) UpdateCredentials(ctx context.Context, id int64, credentials map[string]interface{}) error {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	selectQuery := `SELECT credentials FROM accounts WHERE id = $1`
	if !db.isSQLite() {
		selectQuery += ` FOR UPDATE`
	}

	var currentRaw interface{}
	if err := tx.QueryRowContext(ctx, selectQuery, id).Scan(&currentRaw); err != nil {
		return err
	}

	merged := mergeCredentialMaps(decodeCredentials(currentRaw), credentials)
	credJSON, err := json.Marshal(merged)
	if err != nil {
		return fmt.Errorf("序列化 credentials 失败: %w", err)
	}

	updateQuery := `UPDATE accounts SET credentials = $1, updated_at = CURRENT_TIMESTAMP WHERE id = $2`
	if !db.isSQLite() {
		updateQuery = `UPDATE accounts SET credentials = $1::jsonb, updated_at = CURRENT_TIMESTAMP WHERE id = $2`
	}
	if _, err := tx.ExecContext(ctx, updateQuery, credJSON, id); err != nil {
		return err
	}
	return tx.Commit()
}

// UpdateUsageSnapshot 持久化账号用量快照（7d + 5h）
func (db *DB) UpdateUsageSnapshot(ctx context.Context, id int64, pct7d float64, updatedAt time.Time) error {
	return db.UpdateCredentials(ctx, id, map[string]interface{}{
		"codex_7d_used_percent":  pct7d,
		"codex_usage_updated_at": updatedAt.Format(time.RFC3339),
	})
}

// UpdateUsageSnapshotFull 持久化完整用量快照（5h + 7d + 重置时间）
func (db *DB) UpdateUsageSnapshotFull(ctx context.Context, id int64, pct7d float64, reset7dAt time.Time, pct5h float64, reset5hAt time.Time, updatedAt time.Time) error {
	fields := map[string]interface{}{
		"codex_7d_used_percent":  pct7d,
		"codex_7d_reset_at":      reset7dAt.Format(time.RFC3339),
		"codex_5h_used_percent":  pct5h,
		"codex_5h_reset_at":      reset5hAt.Format(time.RFC3339),
		"codex_usage_updated_at": updatedAt.Format(time.RFC3339),
	}
	return db.UpdateCredentials(ctx, id, fields)
}

// SetError 标记账号错误状态
func (db *DB) SetError(ctx context.Context, id int64, errorMsg string) error {
	query := `UPDATE accounts SET status = 'error', error_message = $1, cooldown_reason = '', cooldown_until = NULL, updated_at = CURRENT_TIMESTAMP WHERE id = $2`
	_, err := db.conn.ExecContext(ctx, query, errorMsg, id)
	return err
}

// BatchSetError 批量标记账号错误状态，分批执行避免 SQL 参数过多
func (db *DB) BatchSetError(ctx context.Context, ids []int64, errorMsg string) error {
	const batchSize = 500
	for i := 0; i < len(ids); i += batchSize {
		end := i + batchSize
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[i:end]

		// 构建 $2, $3, ... 占位符（$1 留给 errorMsg）
		placeholders := make([]string, len(batch))
		args := make([]interface{}, 0, len(batch)+1)
		args = append(args, errorMsg)
		for j, id := range batch {
			placeholders[j] = fmt.Sprintf("$%d", j+2)
			args = append(args, id)
		}

		query := fmt.Sprintf(
			`UPDATE accounts SET status = 'error', error_message = $1, cooldown_reason = '', cooldown_until = NULL, updated_at = CURRENT_TIMESTAMP WHERE id IN (%s)`,
			strings.Join(placeholders, ","),
		)
		if _, err := db.conn.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("batch %d-%d failed: %w", i, end, err)
		}
	}
	return nil
}

// SoftDeleteAccount 将账号标记为 deleted，保留数据用于审计和事件追溯。
func (db *DB) SoftDeleteAccount(ctx context.Context, id int64) error {
	query := `
		UPDATE accounts
		SET status = 'deleted',
			error_message = '',
			cooldown_reason = '',
			cooldown_until = NULL,
			deleted_at = CURRENT_TIMESTAMP,
			updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 AND status <> 'deleted'
	`
	_, err := db.conn.ExecContext(ctx, query, id)
	return err
}

// BatchSoftDeleteAccounts 批量软删除账号，分批执行避免 SQL 参数过多。
func (db *DB) BatchSoftDeleteAccounts(ctx context.Context, ids []int64) error {
	const batchSize = 500
	for i := 0; i < len(ids); i += batchSize {
		end := i + batchSize
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[i:end]

		placeholders := make([]string, len(batch))
		args := make([]interface{}, 0, len(batch))
		for j, id := range batch {
			placeholders[j] = fmt.Sprintf("$%d", j+1)
			args = append(args, id)
		}

		query := fmt.Sprintf(
			`UPDATE accounts
			SET status = 'deleted',
				error_message = '',
				cooldown_reason = '',
				cooldown_until = NULL,
				deleted_at = CURRENT_TIMESTAMP,
				updated_at = CURRENT_TIMESTAMP
			WHERE status <> 'deleted' AND id IN (%s)`,
			strings.Join(placeholders, ","),
		)
		if _, err := db.conn.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("batch %d-%d failed: %w", i, end, err)
		}
	}
	return nil
}

// BatchInsertAccountEventsAsync 批量异步插入账号事件
func (db *DB) BatchInsertAccountEventsAsync(ids []int64, eventType string, source string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()

		const batchSize = 500
		for i := 0; i < len(ids); i += batchSize {
			end := i + batchSize
			if end > len(ids) {
				end = len(ids)
			}
			batch := ids[i:end]

			// 构建 VALUES ($1,$2,$3), ($4,$2,$3), ...
			placeholders := make([]string, len(batch))
			args := make([]interface{}, 0, len(batch)+2)
			args = append(args, eventType, source) // $1=eventType, $2=source
			for j, id := range batch {
				paramIdx := j + 3 // $3, $4, ...
				placeholders[j] = fmt.Sprintf("($%d, $1, $2)", paramIdx)
				args = append(args, id)
			}

			query := fmt.Sprintf(
				`INSERT INTO account_events (account_id, event_type, source) VALUES %s`,
				strings.Join(placeholders, ","),
			)
			if _, err := db.conn.ExecContext(ctx, query, args...); err != nil {
				log.Printf("[账号事件] 批量插入失败 (%d 条): %v", len(batch), err)
			}
		}
	}()
}

// ClearError 清除账号错误状态
func (db *DB) ClearError(ctx context.Context, id int64) error {
	query := `UPDATE accounts SET status = 'active', error_message = '', cooldown_reason = '', cooldown_until = NULL, updated_at = CURRENT_TIMESTAMP WHERE id = $1`
	_, err := db.conn.ExecContext(ctx, query, id)
	return err
}

// SetCooldown 持久化账号冷却状态
func (db *DB) SetCooldown(ctx context.Context, id int64, reason string, until time.Time) error {
	query := `UPDATE accounts SET cooldown_reason = $1, cooldown_until = $2, updated_at = CURRENT_TIMESTAMP WHERE id = $3`
	_, err := db.conn.ExecContext(ctx, query, reason, until, id)
	return err
}

// ClearCooldown 清除账号冷却状态
func (db *DB) ClearCooldown(ctx context.Context, id int64) error {
	query := `UPDATE accounts SET cooldown_reason = '', cooldown_until = NULL, updated_at = CURRENT_TIMESTAMP WHERE id = $1`
	_, err := db.conn.ExecContext(ctx, query, id)
	return err
}

// InsertAccount 插入新账号
func (db *DB) InsertAccount(ctx context.Context, name string, refreshToken string, proxyURL string) (int64, error) {
	credentials := map[string]interface{}{
		"refresh_token": refreshToken,
	}
	credJSON, err := json.Marshal(credentials)
	if err != nil {
		return 0, err
	}

	return db.insertRowID(ctx,
		`INSERT INTO accounts (name, credentials, proxy_url) VALUES ($1, $2, $3) RETURNING id`,
		`INSERT INTO accounts (name, credentials, proxy_url) VALUES ($1, $2, $3)`,
		name, credJSON, proxyURL,
	)
}

// CountAll 获取账号总数
func (db *DB) CountAll(ctx context.Context) (int, error) {
	var count int
	err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts`).Scan(&count)
	return count, err
}

// GetAllRefreshTokens 获取所有已存在的 refresh_token（用于导入去重，排除已删除账号）
func (db *DB) GetAllRefreshTokens(ctx context.Context) (map[string]bool, error) {
	rows, err := db.conn.QueryContext(ctx, `SELECT credentials FROM accounts WHERE status <> 'deleted' AND COALESCE(error_message, '') <> 'deleted'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]bool)
	for rows.Next() {
		var raw interface{}
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		rt := credentialString(raw, "refresh_token")
		if rt != "" {
			result[rt] = true
		}
	}
	return result, rows.Err()
}

// InsertATAccount 插入 AT-only 账号（无 refresh_token）
func (db *DB) InsertATAccount(ctx context.Context, name string, accessToken string, proxyURL string) (int64, error) {
	credentials := map[string]interface{}{
		"access_token": accessToken,
	}
	credJSON, err := json.Marshal(credentials)
	if err != nil {
		return 0, err
	}

	return db.insertRowID(ctx,
		`INSERT INTO accounts (name, credentials, proxy_url) VALUES ($1, $2, $3) RETURNING id`,
		`INSERT INTO accounts (name, credentials, proxy_url) VALUES ($1, $2, $3)`,
		name, credJSON, proxyURL,
	)
}

// InsertAccountWithCredentials 插入带完整 credentials 的账号。
func (db *DB) InsertAccountWithCredentials(ctx context.Context, name string, credentials map[string]interface{}, proxyURL string) (int64, error) {
	if credentials == nil {
		credentials = map[string]interface{}{}
	}
	credJSON, err := json.Marshal(credentials)
	if err != nil {
		return 0, err
	}

	return db.insertRowID(ctx,
		`INSERT INTO accounts (name, credentials, proxy_url) VALUES ($1, $2, $3) RETURNING id`,
		`INSERT INTO accounts (name, credentials, proxy_url) VALUES ($1, $2, $3)`,
		name, credJSON, proxyURL,
	)
}

// GetAllAccessTokens 获取所有已存在的 access_token（用于 AT 导入去重，排除已删除账号）
func (db *DB) GetAllAccessTokens(ctx context.Context) (map[string]bool, error) {
	rows, err := db.conn.QueryContext(ctx, `SELECT credentials FROM accounts WHERE status <> 'deleted' AND COALESCE(error_message, '') <> 'deleted'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]bool)
	for rows.Next() {
		var raw interface{}
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		at := credentialString(raw, "access_token")
		if at != "" {
			result[at] = true
		}
	}
	return result, rows.Err()
}

// GetAllSessionTokens 获取所有已存在的 session_token（用于导入去重，排除已删除账号）
func (db *DB) GetAllSessionTokens(ctx context.Context) (map[string]bool, error) {
	rows, err := db.conn.QueryContext(ctx, `SELECT credentials FROM accounts WHERE status <> 'deleted' AND COALESCE(error_message, '') <> 'deleted'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]bool)
	for rows.Next() {
		var raw interface{}
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		st := credentialString(raw, "session_token")
		if st != "" {
			result[st] = true
		}
	}
	return result, rows.Err()
}

// ==================== 账号事件 ====================

// InsertAccountEvent 插入一条账号事件记录
func (db *DB) InsertAccountEvent(ctx context.Context, accountID int64, eventType string, source string) error {
	_, err := db.conn.ExecContext(ctx,
		`INSERT INTO account_events (account_id, event_type, source) VALUES ($1, $2, $3)`,
		accountID, eventType, source,
	)
	return err
}

// InsertAccountEventAsync 异步插入账号事件（不阻塞调用方，SQLite 下带重试）
func (db *DB) InsertAccountEventAsync(accountID int64, eventType string, source string) {
	go func() {
		var err error
		for attempt := 0; attempt < 3; attempt++ {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err = db.InsertAccountEvent(ctx, accountID, eventType, source)
			cancel()
			if err == nil {
				return
			}
			time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
		}
		if err != nil {
			log.Printf("[账号事件] 记录失败（已重试3次）: account=%d type=%s source=%s err=%v", accountID, eventType, source, err)
		}
	}()
}

// GetAccountEventTrend 按时间桶聚合账号增删事件
func (db *DB) GetAccountEventTrend(ctx context.Context, start, end time.Time, bucketMinutes int) ([]AccountEventPoint, error) {
	if db.isSQLite() {
		return db.getAccountEventTrendSQLite(ctx, start, end, bucketMinutes)
	}

	if bucketMinutes < 1 {
		bucketMinutes = 60
	}

	query := `
	SELECT
		TO_CHAR(
			date_trunc('minute', created_at)
			- (EXTRACT(MINUTE FROM created_at)::int % $3) * INTERVAL '1 minute',
			'YYYY-MM-DD"T"HH24:MI:SS'
		) AS bucket,
		COALESCE(SUM(CASE WHEN event_type = 'added' THEN 1 ELSE 0 END), 0) AS added,
		COALESCE(SUM(CASE WHEN event_type = 'deleted' AND source = 'manual' THEN 1 ELSE 0 END), 0) AS deleted
	FROM account_events
	WHERE created_at >= $1 AND created_at <= $2
	  AND (event_type = 'added' OR (event_type = 'deleted' AND source = 'manual'))
	GROUP BY 1
	ORDER BY 1`

	rows, err := db.conn.QueryContext(ctx, query, start, end, bucketMinutes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var result []AccountEventPoint
	for rows.Next() {
		var p AccountEventPoint
		if err := rows.Scan(&p.Bucket, &p.Added, &p.Deleted); err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	if result == nil {
		result = []AccountEventPoint{}
	}
	return result, rows.Err()
}
