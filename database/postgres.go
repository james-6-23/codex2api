package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lib/pq"
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
	CreditEnabled           bool
	CreditSkipUsageWindow   bool
	SkipWarmTier            bool
	ScoreBiasOverride       sql.NullInt64
	BaseConcurrencyOverride sql.NullInt64
	Tags                    []string
	CreatedAt               time.Time
	UpdatedAt               time.Time
	DeletedAt               sql.NullTime
}

type AccountModelCooldownRow struct {
	AccountID int64
	Model     string
	Reason    string
	ResetAt   time.Time
	UpdatedAt time.Time
}

type OptionalInt64Slice struct {
	Set    bool
	Values []int64
}

type OptionalStringSlice struct {
	Set    bool
	Values []string
}

type OptionalString struct {
	Set   bool
	Value string
}

type OptionalBool struct {
	Set   bool
	Value bool
}

type OptionalNullInt64 struct {
	Set   bool
	Value sql.NullInt64
}

// AccountCredentialIndex holds pre-built sets of existing credentials for fast import dedup.
type AccountCredentialIndex struct {
	RefreshTokens map[string]bool
	AccessTokens  map[string]bool
	SessionTokens map[string]bool
	AccountIDs    map[string]bool
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

func (a *AccountRow) GetCredentialStringSlice(key string) []string {
	if a.Credentials == nil {
		return []string{}
	}
	value, ok := a.Credentials[key]
	if !ok || value == nil {
		return []string{}
	}
	return stringSliceFromValue(value)
}

type sqlExecer interface {
	ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
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

	usageLogMode          atomic.Value // string: full|errors|off
	usageLogBatchSize     int64
	usageLogFlushInterval int64 // ns
	logFlushNotify        chan struct{}
	accountInsertMu       sync.Mutex
	sqliteWriteSem        chan struct{}
	sqliteSingleConn      bool
}

const (
	UsageLogModeFull   = "full"
	UsageLogModeErrors = "errors"
	UsageLogModeOff    = "off"

	defaultUsageLogMode                 = UsageLogModeFull
	defaultUsageLogBatchSize            = 200
	defaultUsageLogFlushIntervalSeconds = 5
	minUsageLogBatchSize                = 1
	maxUsageLogBatchSize                = 10000
	minUsageLogFlushIntervalSeconds     = 1
	maxUsageLogFlushIntervalSeconds     = 300
)

var ErrDuplicateAccountCredential = errors.New("duplicate account credential")

func NormalizeUsageLogMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", UsageLogModeFull:
		return UsageLogModeFull
	case UsageLogModeErrors:
		return UsageLogModeErrors
	case UsageLogModeOff:
		return UsageLogModeOff
	default:
		return UsageLogModeFull
	}
}

func NormalizeUsageLogBatchSize(n int) int {
	if n < minUsageLogBatchSize {
		return defaultUsageLogBatchSize
	}
	if n > maxUsageLogBatchSize {
		return maxUsageLogBatchSize
	}
	return n
}

func NormalizeUsageLogFlushIntervalSeconds(n int) int {
	if n < minUsageLogFlushIntervalSeconds {
		return defaultUsageLogFlushIntervalSeconds
	}
	if n > maxUsageLogFlushIntervalSeconds {
		return maxUsageLogFlushIntervalSeconds
	}
	return n
}

// usageLogEntry 日志缓冲条目
type usageLogEntry struct {
	AccountID            int64
	ClientIP             string
	Endpoint             string
	Model                string
	EffectiveModel       string
	PromptTokens         int
	CompletionTokens     int
	TotalTokens          int
	StatusCode           int
	DurationMs           int
	InputTokens          int
	OutputTokens         int
	ReasoningTokens      int
	FirstTokenMs         int
	ReasoningEffort      string
	InboundEndpoint      string
	UpstreamEndpoint     string
	Stream               bool
	Compact              bool
	ViaWebsocket         bool
	CachedTokens         int
	ServiceTier          string
	RequestedServiceTier string
	ActualServiceTier    string
	BillingServiceTier   string
	APIKeyID             int64
	APIKeyName           string
	APIKeyMasked         string
	ImageCount           int
	ImageWidth           int
	ImageHeight          int
	ImageBytes           int
	ImageFormat          string
	ImageSize            string
	AccountBilled        float64
	UserBilled           float64
	IsRetryAttempt       bool
	AttemptIndex         int
	UpstreamErrorKind    string
	ErrorMessage         string
}

// New 创建数据库连接并自动建表。
// schema 仅对 PostgreSQL 生效；为空时保持数据库默认 search_path。
func New(driver string, dsn string, schema ...string) (*DB, error) {
	driver = normalizeDriver(driver)
	driverName := driver
	if driver == "sqlite" {
		driverName = "sqlite"
		dsn = sqliteConnectDSN(dsn)
	}

	pgSchema := ""
	if len(schema) > 0 {
		pgSchema = strings.TrimSpace(schema[0])
	}
	sqliteSingleConn := driver == "sqlite" && strings.TrimSpace(dsn) == ":memory:"

	conn, err := sql.Open(driverName, dsn)
	if err != nil {
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}

	// ==================== 连接池优化 ====================
	if driver == "sqlite" {
		if sqliteSingleConn {
			conn.SetMaxOpenConns(1)
			conn.SetMaxIdleConns(1)
		} else {
			applySQLiteConnLimits(conn, defaultSQLiteMaxOpenConns)
		}
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
		conn:             conn,
		driver:           driver,
		logStop:          make(chan struct{}),
		logFlushNotify:   make(chan struct{}, 1),
		sqliteSingleConn: sqliteSingleConn,
	}
	if db.isSQLite() {
		db.sqliteWriteSem = make(chan struct{}, 1)
	}
	db.SetUsageLogConfig(defaultUsageLogMode, defaultUsageLogBatchSize, defaultUsageLogFlushIntervalSeconds)
	if db.isSQLite() {
		if err := db.configureSQLite(ctx); err != nil {
			return nil, fmt.Errorf("配置 SQLite 失败: %w", err)
		}
	} else {
		// PostgreSQL: 统一会话时区为 UTC，确保 NOW() 和时间字面量一致
		if _, err := conn.ExecContext(ctx, "SET timezone = 'UTC'"); err != nil {
			return nil, fmt.Errorf("设置数据库时区失败: %w", err)
		}
		// 自定义 schema：确保 schema 存在并确认当前会话 search_path 已生效。
		// search_path 已通过 DSN 的 options=-c search_path=... 在所有连接启动时设置；
		// 这里仅做一次幂等的 CREATE SCHEMA + SET 兜底，便于首次部署时自动建好 schema。
		if pgSchema != "" {
			quoted := pq.QuoteIdentifier(pgSchema)
			if _, err := conn.ExecContext(ctx, "CREATE SCHEMA IF NOT EXISTS "+quoted); err != nil {
				return nil, fmt.Errorf("创建数据库 schema 失败: %w", err)
			}
			if _, err := conn.ExecContext(ctx, "SET search_path TO "+quoted+", public"); err != nil {
				return nil, fmt.Errorf("设置 search_path 失败: %w", err)
			}
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
				cache_hit_requests BIGINT NOT NULL DEFAULT 0,
				first_token_ms_sum DOUBLE PRECISION NOT NULL DEFAULT 0,
				first_token_samples BIGINT NOT NULL DEFAULT 0,
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
			{name: "cache_hit_requests", def: "INTEGER NOT NULL DEFAULT 0"},
			{name: "first_token_ms_sum", def: "REAL NOT NULL DEFAULT 0"},
			{name: "first_token_samples", def: "INTEGER NOT NULL DEFAULT 0"},
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
		ALTER TABLE usage_stats_baseline ADD COLUMN IF NOT EXISTS cache_hit_requests BIGINT NOT NULL DEFAULT 0;
		ALTER TABLE usage_stats_baseline ADD COLUMN IF NOT EXISTS first_token_ms_sum DOUBLE PRECISION NOT NULL DEFAULT 0;
		ALTER TABLE usage_stats_baseline ADD COLUMN IF NOT EXISTS first_token_samples BIGINT NOT NULL DEFAULT 0;
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

func (db *DB) SetUsageLogConfig(mode string, batchSize int, flushIntervalSeconds int) {
	if db == nil {
		return
	}
	mode = NormalizeUsageLogMode(mode)
	batchSize = NormalizeUsageLogBatchSize(batchSize)
	flushIntervalSeconds = NormalizeUsageLogFlushIntervalSeconds(flushIntervalSeconds)
	db.usageLogMode.Store(mode)
	atomic.StoreInt64(&db.usageLogBatchSize, int64(batchSize))
	atomic.StoreInt64(&db.usageLogFlushInterval, int64(time.Duration(flushIntervalSeconds)*time.Second))
}

func (db *DB) GetUsageLogMode() string {
	if db == nil {
		return defaultUsageLogMode
	}
	if v, ok := db.usageLogMode.Load().(string); ok && v != "" {
		return NormalizeUsageLogMode(v)
	}
	return defaultUsageLogMode
}

func (db *DB) GetUsageLogBatchSize() int {
	if db == nil {
		return defaultUsageLogBatchSize
	}
	n := int(atomic.LoadInt64(&db.usageLogBatchSize))
	return NormalizeUsageLogBatchSize(n)
}

func (db *DB) GetUsageLogFlushIntervalSeconds() int {
	if db == nil {
		return defaultUsageLogFlushIntervalSeconds
	}
	d := time.Duration(atomic.LoadInt64(&db.usageLogFlushInterval))
	if d <= 0 {
		return defaultUsageLogFlushIntervalSeconds
	}
	return NormalizeUsageLogFlushIntervalSeconds(int(d / time.Second))
}

// UsageLogRuntimeStats 描述 usage_logs 写入器当前的运行态。
type UsageLogRuntimeStats struct {
	Mode                 string
	Enabled              bool
	BatchSize            int
	FlushIntervalSeconds int
	BufferLength         int
	BufferCapacity       int
}

// GetUsageLogRuntimeStats 返回 usage_logs 配置和当前内存缓冲长度。
func (db *DB) GetUsageLogRuntimeStats() UsageLogRuntimeStats {
	stats := UsageLogRuntimeStats{
		Mode:                 defaultUsageLogMode,
		Enabled:              true,
		BatchSize:            defaultUsageLogBatchSize,
		FlushIntervalSeconds: defaultUsageLogFlushIntervalSeconds,
	}
	if db == nil {
		return stats
	}

	stats.Mode = db.GetUsageLogMode()
	stats.Enabled = stats.Mode != UsageLogModeOff
	stats.BatchSize = db.GetUsageLogBatchSize()
	stats.FlushIntervalSeconds = db.GetUsageLogFlushIntervalSeconds()

	db.logMu.Lock()
	stats.BufferLength = len(db.logBuf)
	stats.BufferCapacity = cap(db.logBuf)
	db.logMu.Unlock()

	return stats
}

func (db *DB) getUsageLogFlushInterval() time.Duration {
	if db == nil {
		return time.Duration(defaultUsageLogFlushIntervalSeconds) * time.Second
	}
	d := time.Duration(atomic.LoadInt64(&db.usageLogFlushInterval))
	if d <= 0 {
		return time.Duration(defaultUsageLogFlushIntervalSeconds) * time.Second
	}
	return d
}

func (db *DB) shouldStoreUsageLog(input *UsageLogInput) bool {
	switch db.GetUsageLogMode() {
	case UsageLogModeOff:
		return false
	case UsageLogModeErrors:
		return input != nil && input.StatusCode >= 400
	default:
		return true
	}
}

func (db *DB) notifyLogFlush() {
	if db == nil || db.logFlushNotify == nil {
		return
	}
	select {
	case db.logFlushNotify <- struct{}{}:
	default:
	}
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
	ALTER TABLE accounts ADD COLUMN IF NOT EXISTS tags JSONB DEFAULT '[]'::jsonb;
	ALTER TABLE accounts ADD COLUMN IF NOT EXISTS credit_enabled BOOLEAN DEFAULT FALSE;
	ALTER TABLE accounts ADD COLUMN IF NOT EXISTS credit_skip_usage_window BOOLEAN DEFAULT FALSE;
	ALTER TABLE accounts ADD COLUMN IF NOT EXISTS skip_warm_tier BOOLEAN DEFAULT FALSE;

	CREATE TABLE IF NOT EXISTS account_groups (
		id          SERIAL PRIMARY KEY,
		name        VARCHAR(80) UNIQUE NOT NULL,
		description TEXT DEFAULT '',
		color       VARCHAR(20) DEFAULT '',
		sort_order  INT DEFAULT 0,
		created_at  TIMESTAMPTZ DEFAULT NOW(),
		updated_at  TIMESTAMPTZ DEFAULT NOW()
	);
	ALTER TABLE account_groups ADD COLUMN IF NOT EXISTS description TEXT DEFAULT '';
	ALTER TABLE account_groups ADD COLUMN IF NOT EXISTS color VARCHAR(20) DEFAULT '';
	ALTER TABLE account_groups ADD COLUMN IF NOT EXISTS sort_order INT DEFAULT 0;
	ALTER TABLE account_groups ADD COLUMN IF NOT EXISTS created_at TIMESTAMPTZ DEFAULT NOW();
	ALTER TABLE account_groups ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ DEFAULT NOW();

	CREATE TABLE IF NOT EXISTS account_group_members (
		account_id BIGINT NOT NULL,
		group_id   BIGINT NOT NULL,
		PRIMARY KEY (account_id, group_id)
	);
	CREATE INDEX IF NOT EXISTS idx_account_group_members_group ON account_group_members(group_id);
	CREATE INDEX IF NOT EXISTS idx_account_group_members_account ON account_group_members(account_id);

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
	CREATE INDEX IF NOT EXISTS idx_usage_logs_account_created_at ON usage_logs(account_id, created_at);
	CREATE INDEX IF NOT EXISTS idx_usage_logs_created_status ON usage_logs(created_at, status_code);
	CREATE INDEX IF NOT EXISTS idx_usage_logs_account_status ON usage_logs(account_id, status_code);

	-- 增强字段（向后兼容 ALTER）
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS input_tokens INT DEFAULT 0;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS output_tokens INT DEFAULT 0;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS reasoning_tokens INT DEFAULT 0;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS first_token_ms INT DEFAULT 0;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS reasoning_effort VARCHAR(20) DEFAULT '';
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS via_websocket BOOLEAN DEFAULT FALSE;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS effective_model VARCHAR(100) DEFAULT '';
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS inbound_endpoint VARCHAR(100) DEFAULT '';
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS upstream_endpoint VARCHAR(100) DEFAULT '';
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS stream BOOLEAN DEFAULT false;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS compact BOOLEAN DEFAULT false;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS cached_tokens INT DEFAULT 0;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS service_tier VARCHAR(20) DEFAULT '';
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS requested_service_tier VARCHAR(20) DEFAULT '';
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS actual_service_tier VARCHAR(20) DEFAULT '';
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS billing_service_tier VARCHAR(20) DEFAULT '';
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS api_key_id INT DEFAULT 0;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS api_key_name VARCHAR(255) DEFAULT '';
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS api_key_masked VARCHAR(64) DEFAULT '';
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS client_ip VARCHAR(64) DEFAULT '';
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS image_count INT DEFAULT 0;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS image_width INT DEFAULT 0;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS image_height INT DEFAULT 0;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS image_bytes INT DEFAULT 0;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS image_format VARCHAR(20) DEFAULT '';
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS image_size VARCHAR(32) DEFAULT '';
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS account_billed DOUBLE PRECISION DEFAULT 0;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS user_billed DOUBLE PRECISION DEFAULT 0;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS is_retry_attempt BOOLEAN DEFAULT FALSE;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS attempt_index INT DEFAULT 0;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS upstream_error_kind VARCHAR(64) DEFAULT '';
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS error_message TEXT DEFAULT '';

	CREATE INDEX IF NOT EXISTS idx_usage_logs_api_key_created_at ON usage_logs(api_key_id, created_at);

	CREATE TABLE IF NOT EXISTS api_keys (
		id         SERIAL PRIMARY KEY,
		name       VARCHAR(255) DEFAULT '',
		key        VARCHAR(255) NOT NULL UNIQUE,
		quota_limit DOUBLE PRECISION DEFAULT 0,
		quota_used  DOUBLE PRECISION DEFAULT 0,
		expires_at  TIMESTAMPTZ NULL,
		created_at TIMESTAMPTZ DEFAULT NOW()
	);
	ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS quota_limit DOUBLE PRECISION DEFAULT 0;
	ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS quota_used DOUBLE PRECISION DEFAULT 0;
	ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS expires_at TIMESTAMPTZ NULL;
	CREATE INDEX IF NOT EXISTS idx_api_keys_expires_at ON api_keys(expires_at);

	ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS allowed_group_ids JSONB DEFAULT '[]'::jsonb;
	ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS limits JSONB DEFAULT '{}'::jsonb;

			CREATE TABLE IF NOT EXISTS system_settings (
				id                 INTEGER PRIMARY KEY DEFAULT 1 CHECK (id = 1),
				site_name          TEXT DEFAULT 'CodexProxy',
				site_logo          TEXT DEFAULT '',
				background_config  TEXT DEFAULT '{}',
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
				usage_probe_concurrency INT DEFAULT 16,
				usage_probe_responses_fallback_enabled BOOLEAN DEFAULT TRUE,
				recovery_probe_interval_minutes INT DEFAULT 30,
			scheduler_mode VARCHAR(20) DEFAULT 'round_robin'
		);
	CREATE TABLE IF NOT EXISTS account_model_cooldowns (
		account_id BIGINT NOT NULL,
		model VARCHAR(100) NOT NULL,
		reason VARCHAR(64) DEFAULT '',
		reset_at TIMESTAMPTZ NOT NULL,
		updated_at TIMESTAMPTZ DEFAULT NOW(),
		PRIMARY KEY (account_id, model)
	);
	CREATE INDEX IF NOT EXISTS idx_account_model_cooldowns_reset_at ON account_model_cooldowns(reset_at);
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS site_name TEXT DEFAULT 'CodexProxy';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS site_logo TEXT DEFAULT '';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS background_config TEXT DEFAULT '{}';
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
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS max_rate_limit_retries INT DEFAULT 1;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS auto_clean_error BOOLEAN DEFAULT FALSE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS auto_clean_expired BOOLEAN DEFAULT FALSE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS lazy_mode BOOLEAN DEFAULT FALSE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS model_mapping TEXT DEFAULT '{}';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS codex_model_mapping TEXT DEFAULT '{}';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS reasoning_effort_models TEXT DEFAULT '[]';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS background_refresh_interval_minutes INT DEFAULT 2;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS usage_probe_max_age_minutes INT DEFAULT 10;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS usage_probe_concurrency INT DEFAULT 16;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS usage_probe_responses_fallback_enabled BOOLEAN DEFAULT TRUE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS recovery_probe_interval_minutes INT DEFAULT 30;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS scheduler_mode VARCHAR(20) DEFAULT 'round_robin';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS affinity_mode VARCHAR(16) DEFAULT 'bounded';
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
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS client_compat_mode VARCHAR(20) DEFAULT 'preserve';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS codex_min_cli_version VARCHAR(32) DEFAULT '0.118.0';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS usage_log_mode VARCHAR(20) DEFAULT 'full';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS usage_log_batch_size INT DEFAULT 200;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS usage_log_flush_interval_seconds INT DEFAULT 5;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS stream_flush_policy VARCHAR(20) DEFAULT 'immediate';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS stream_flush_interval_ms INT DEFAULT 20;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS first_token_mode VARCHAR(20) DEFAULT 'strict';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS first_token_timeout_seconds INT DEFAULT 0;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS billing_tier_policy VARCHAR(20) DEFAULT 'actual';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS image_storage_config TEXT DEFAULT '{}';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS show_full_usage_numbers BOOLEAN DEFAULT FALSE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS codex_force_websocket BOOLEAN DEFAULT FALSE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS codex_ws_keepalive_enabled BOOLEAN DEFAULT FALSE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS codex_ws_keepalive_interval_sec INT DEFAULT 60;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS codex_ws_hide_upstream_errors BOOLEAN DEFAULT TRUE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS codex_ws_silent_retry_enabled BOOLEAN DEFAULT TRUE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS codex_ws_silent_max_retries INT DEFAULT 2;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS auto_pause_5h_threshold DOUBLE PRECISION DEFAULT 0;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS auto_pause_7d_threshold DOUBLE PRECISION DEFAULT 0;

	ALTER TABLE account_groups ADD COLUMN IF NOT EXISTS auto_pause_5h_threshold DOUBLE PRECISION DEFAULT 0;
	ALTER TABLE account_groups ADD COLUMN IF NOT EXISTS auto_pause_7d_threshold DOUBLE PRECISION DEFAULT 0;

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
	ID              int64        `json:"id"`
	Name            string       `json:"name"`
	Key             string       `json:"key"`
	QuotaLimit      float64      `json:"quota_limit"`
	QuotaUsed       float64      `json:"quota_used"`
	ExpiresAt       sql.NullTime `json:"expires_at"`
	AllowedGroupIDs []int64      `json:"allowed_group_ids"`
	Limits          APIKeyLimits `json:"limits"`
	CreatedAt       time.Time    `json:"created_at"`
}

// APIKeyLimits 是 API Key 级别的细粒度限流/配额配置。
// 0 或空字段表示该项不限。落库为 JSON,允许平滑扩展字段。
//
// - ModelAllow / ModelDeny: 模型白/黑名单。同时配置时白名单生效,黑名单忽略。
// - RPM: 每分钟请求数 (滑动 60s 窗口)。
// - RPD: 每天请求数 (滑动 24h 窗口)。
// - MaxConcurrency: 同一 API Key 在当前实例内允许的最大并发请求数。
// - CostLimit5h / CostLimit7d: 美元成本上限,滑动 5h / 7d 窗口,与账号侧窗口语义一致。
// - TokenLimit5h / TokenLimit7d: token 上限,滑动 5h / 7d 窗口。
type APIKeyLimits struct {
	ModelAllow     []string `json:"model_allow,omitempty"`
	ModelDeny      []string `json:"model_deny,omitempty"`
	RPM            int      `json:"rpm,omitempty"`
	RPD            int      `json:"rpd,omitempty"`
	MaxConcurrency int      `json:"max_concurrency,omitempty"`
	CostLimit5h    float64  `json:"cost_limit_5h,omitempty"`
	CostLimit7d    float64  `json:"cost_limit_7d,omitempty"`
	TokenLimit5h   int64    `json:"token_limit_5h,omitempty"`
	TokenLimit7d   int64    `json:"token_limit_7d,omitempty"`
}

// IsZero 判断是否为空 limits(全部字段都未配置)
func (l APIKeyLimits) IsZero() bool {
	return len(l.ModelAllow) == 0 && len(l.ModelDeny) == 0 &&
		l.RPM == 0 && l.RPD == 0 && l.MaxConcurrency == 0 &&
		l.CostLimit5h == 0 && l.CostLimit7d == 0 &&
		l.TokenLimit5h == 0 && l.TokenLimit7d == 0
}

type APIKeyInput struct {
	Name            string
	Key             string
	QuotaLimit      float64
	QuotaUsed       float64
	ExpiresAt       sql.NullTime
	AllowedGroupIDs []int64
	Limits          APIKeyLimits
}

type APIKeyUpdate struct {
	Name               string
	NameSet            bool
	QuotaLimit         float64
	QuotaLimitSet      bool
	ExpiresAt          sql.NullTime
	ExpiresAtSet       bool
	AllowedGroupIDs    []int64
	AllowedGroupIDsSet bool
	Limits             APIKeyLimits
	LimitsSet          bool
}

const apiKeySelectColumns = `id, name, key, created_at, COALESCE(quota_limit, 0), COALESCE(quota_used, 0), expires_at, COALESCE(allowed_group_ids, '[]'), COALESCE(limits, '{}')`

// ListAPIKeys 获取所有 API 密钥
func (db *DB) ListAPIKeys(ctx context.Context) ([]*APIKeyRow, error) {
	rows, err := db.conn.QueryContext(ctx, `SELECT `+apiKeySelectColumns+` FROM api_keys ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var keys []*APIKeyRow
	for rows.Next() {
		k, err := scanAPIKeyRow(rows)
		if err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// CountAPIKeys 返回当前 API Key 数量。
func (db *DB) CountAPIKeys(ctx context.Context) (int, error) {
	var count int
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM api_keys`).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

// GetAPIKeyByValue 通过完整 API Key 查找元数据，用于鉴权热路径的按 key 缓存。
func (db *DB) GetAPIKeyByValue(ctx context.Context, key string) (*APIKeyRow, error) {
	rows, err := db.conn.QueryContext(ctx, `SELECT `+apiKeySelectColumns+` FROM api_keys WHERE key = $1`, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, sql.ErrNoRows
	}
	return scanAPIKeyRow(rows)
}

// InsertAPIKey 插入新 API 密钥
func (db *DB) InsertAPIKey(ctx context.Context, name, key string) (int64, error) {
	return db.InsertAPIKeyWithOptions(ctx, APIKeyInput{Name: name, Key: key})
}

func (db *DB) InsertAPIKeyWithOptions(ctx context.Context, input APIKeyInput) (int64, error) {
	if input.QuotaLimit < 0 {
		input.QuotaLimit = 0
	}
	if input.QuotaUsed < 0 {
		input.QuotaUsed = 0
	}
	return db.insertRowID(ctx,
		`INSERT INTO api_keys (name, key, quota_limit, quota_used, expires_at, allowed_group_ids, limits) VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7::jsonb) RETURNING id`,
		`INSERT INTO api_keys (name, key, quota_limit, quota_used, expires_at, allowed_group_ids, limits) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		input.Name, input.Key, input.QuotaLimit, input.QuotaUsed, nullableTimeArg(input.ExpiresAt), encodeInt64SliceJSON(input.AllowedGroupIDs), encodeAPIKeyLimits(input.Limits),
	)
}

func nullableTimeArg(value sql.NullTime) interface{} {
	if !value.Valid {
		return nil
	}
	return value.Time
}

func (row *APIKeyRow) IsExpired(now time.Time) bool {
	return row != nil && row.ExpiresAt.Valid && !row.ExpiresAt.Time.After(now)
}

func (row *APIKeyRow) IsQuotaExhausted() bool {
	return row != nil && row.QuotaLimit > 0 && row.QuotaUsed >= row.QuotaLimit
}

func (row *APIKeyRow) HasAccessConstraints() bool {
	return row != nil && (row.QuotaLimit > 0 || row.ExpiresAt.Valid || len(row.AllowedGroupIDs) > 0 || !row.Limits.IsZero())
}

// UpdateAPIKeyName updates the display name of an API key without changing the key value.
func (db *DB) UpdateAPIKeyName(ctx context.Context, id int64, name string) error {
	res, err := db.conn.ExecContext(ctx, `UPDATE api_keys SET name = $1 WHERE id = $2`, name, id)
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

// UpdateAPIKeyQuotaLimit updates the quota ceiling. A non-positive value clears the limit.
func (db *DB) UpdateAPIKeyQuotaLimit(ctx context.Context, id int64, quotaLimit float64) error {
	if quotaLimit < 0 {
		quotaLimit = 0
	}
	res, err := db.conn.ExecContext(ctx, `UPDATE api_keys SET quota_limit = $1 WHERE id = $2`, quotaLimit, id)
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

// UpdateAPIKeyExpiresAt updates or clears the key expiration.
func (db *DB) UpdateAPIKeyExpiresAt(ctx context.Context, id int64, expiresAt sql.NullTime) error {
	res, err := db.conn.ExecContext(ctx, `UPDATE api_keys SET expires_at = $1 WHERE id = $2`, nullableTimeArg(expiresAt), id)
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

// UpdateAPIKeyAllowedGroups persists the allowed-group scope for an API key.
// Empty slice clears the scope (key may schedule any account).
func (db *DB) UpdateAPIKeyAllowedGroups(ctx context.Context, id int64, groupIDs []int64) error {
	payload := encodeInt64SliceJSON(groupIDs)
	var (
		res sql.Result
		err error
	)
	if db.isSQLite() {
		res, err = db.conn.ExecContext(ctx, `UPDATE api_keys SET allowed_group_ids = $1 WHERE id = $2`, payload, id)
	} else {
		res, err = db.conn.ExecContext(ctx, `UPDATE api_keys SET allowed_group_ids = $1::jsonb WHERE id = $2`, payload, id)
	}
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

func (db *DB) UpdateAPIKeyAllowedGroupIDs(ctx context.Context, id int64, groupIDs []int64) error {
	return db.UpdateAPIKeyAllowedGroups(ctx, id, groupIDs)
}

// UpdateAPIKeyLimits persists the per-key rate / quota / model limit configuration.
// 空 APIKeyLimits 等价于"清除所有限制",对应数据库列为 '{}'。
func (db *DB) UpdateAPIKeyLimits(ctx context.Context, id int64, limits APIKeyLimits) error {
	payload := encodeAPIKeyLimits(limits)
	var (
		res sql.Result
		err error
	)
	if db.isSQLite() {
		res, err = db.conn.ExecContext(ctx, `UPDATE api_keys SET limits = $1 WHERE id = $2`, payload, id)
	} else {
		res, err = db.conn.ExecContext(ctx, `UPDATE api_keys SET limits = $1::jsonb WHERE id = $2`, payload, id)
	}
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

// UpdateAPIKey applies multiple editable fields in one transaction.
// Omitted fields keep their existing values.
func (db *DB) UpdateAPIKey(ctx context.Context, id int64, update APIKeyUpdate) error {
	sets := make([]string, 0, 4)
	args := make([]interface{}, 0, 5)
	placeholder := func() string {
		args = append(args, nil)
		if db.isSQLite() {
			return "?"
		}
		return fmt.Sprintf("$%d", len(args))
	}
	setArg := func(value interface{}) string {
		ph := placeholder()
		args[len(args)-1] = value
		return ph
	}
	if update.NameSet {
		sets = append(sets, "name = "+setArg(update.Name))
	}
	if update.QuotaLimitSet {
		quotaLimit := update.QuotaLimit
		if quotaLimit < 0 {
			quotaLimit = 0
		}
		sets = append(sets, "quota_limit = "+setArg(quotaLimit))
	}
	if update.ExpiresAtSet {
		sets = append(sets, "expires_at = "+setArg(nullableTimeArg(update.ExpiresAt)))
	}
	if update.AllowedGroupIDsSet {
		payload := encodeInt64SliceJSON(update.AllowedGroupIDs)
		ph := setArg(payload)
		if db.isSQLite() {
			sets = append(sets, "allowed_group_ids = "+ph)
		} else {
			sets = append(sets, "allowed_group_ids = "+ph+"::jsonb")
		}
	}
	if update.LimitsSet {
		payload := encodeAPIKeyLimits(update.Limits)
		ph := setArg(payload)
		if db.isSQLite() {
			sets = append(sets, "limits = "+ph)
		} else {
			sets = append(sets, "limits = "+ph+"::jsonb")
		}
	}
	if len(sets) == 0 {
		return nil
	}
	idPlaceholder := placeholder()
	args[len(args)-1] = id
	res, err := db.conn.ExecContext(ctx, "UPDATE api_keys SET "+strings.Join(sets, ", ")+" WHERE id = "+idPlaceholder, args...)
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

// ==================== System Settings ====================

const DefaultSiteName = "CodexProxy"

func NormalizeSiteName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return DefaultSiteName
	}
	runes := []rune(value)
	if len(runes) > 80 {
		return string(runes[:80])
	}
	return value
}

// SystemSettings 运行时设置项
type SystemSettings struct {
	SiteName                           string
	SiteLogo                           string
	BackgroundConfig                   string // JSON: {"image":"...","opacity":18,"blur":0}
	MaxConcurrency                     int
	GlobalRPM                          int
	TestModel                          string
	TestConcurrency                    int
	ProxyURL                           string
	PgMaxConns                         int
	RedisPoolSize                      int
	AutoCleanUnauthorized              bool
	AutoCleanRateLimited               bool
	AdminSecret                        string
	AutoCleanFullUsage                 bool
	AutoCleanError                     bool
	AutoCleanExpired                   bool
	LazyMode                           bool
	ProxyPoolEnabled                   bool
	FastSchedulerEnabled               bool
	MaxRetries                         int
	MaxRateLimitRetries                int
	AllowRemoteMigration               bool
	ModelMapping                       string // JSON: {"anthropic_model": "codex_model", ...}
	CodexModelMapping                  string // JSON: {"requested_codex_model": "upstream_codex_model", ...}
	ReasoningEffortModels              string // JSON: [{"model":"gpt-5.5","effort":"xhigh"}, ...]
	BackgroundRefreshIntervalMinutes   int
	UsageProbeMaxAgeMinutes            int
	UsageProbeConcurrency              int
	UsageProbeResponsesFallbackEnabled bool
	RecoveryProbeIntervalMinutes       int
	SchedulerMode                      string
	AffinityMode                       string // session 粘性模式: bounded / off / strict
	ResinURL                           string // Resin 代理池地址（含 Token），例如 http://127.0.0.1:2260/my-token
	ResinPlatformName                  string // Resin 平台标识，例如 codex2api
	PromptFilterEnabled                bool
	PromptFilterMode                   string
	PromptFilterThreshold              int
	PromptFilterStrictThreshold        int
	PromptFilterLogMatches             bool
	PromptFilterMaxTextLength          int
	PromptFilterSensitiveWords         string
	PromptFilterCustomPatterns         string
	PromptFilterDisabledPatterns       string
	ClientCompatMode                   string
	CodexMinCLIVersion                 string
	UsageLogMode                       string
	UsageLogBatchSize                  int
	UsageLogFlushIntervalSeconds       int
	StreamFlushPolicy                  string
	StreamFlushIntervalMS              int
	FirstTokenMode                     string
	FirstTokenTimeoutSeconds           int
	BillingTierPolicy                  string
	ImageStorageConfig                 string // JSON: {"backend":"s3","endpoint":"...","region":"...","bucket":"...","access_key":"...","secret_key":"...","prefix":"...","force_path_style":false}
	ShowFullUsageNumbers               bool
	CodexForceWebsocket                bool // 强制 Codex 上游走 WebSocket（复用连接池），默认 false
	CodexWSKeepaliveEnabled            bool // 启用上游 WS 空闲连接保活（仅 Ping，不发业务帧），默认 false
	CodexWSKeepaliveIntervalSec        int  // WS 保活 Ping 间隔（秒），默认 60
	CodexWSHideUpstreamErrors          bool // 隐藏上游 WS 原始错误，默认 true
	CodexWSSilentRetryEnabled          bool // 首包前 WS 上游错误静默换号重试，默认 true
	CodexWSSilentMaxRetries            int  // WS 静默换号最大重试次数，默认 2
	AutoPause5hThreshold               float64
	AutoPause7dThreshold               float64
}

func normalizeBillingTierPolicy(policy string) string {
	switch strings.ToLower(strings.TrimSpace(policy)) {
	case "requested":
		return "requested"
	default:
		return "actual"
	}
}

func normalizeFirstTokenMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "loose":
		return "loose"
	default:
		return "strict"
	}
}

// GetSystemSettings 加载全局设置
func (db *DB) GetSystemSettings(ctx context.Context) (*SystemSettings, error) {
	s := &SystemSettings{}
	err := db.conn.QueryRowContext(ctx, `
		SELECT COALESCE(site_name, 'CodexProxy'), COALESCE(site_logo, ''),
		       max_concurrency, global_rpm, test_model, test_concurrency, proxy_url, pg_max_conns, redis_pool_size,
		       auto_clean_unauthorized, auto_clean_rate_limited, COALESCE(admin_secret, ''), COALESCE(auto_clean_full_usage, false),
		       COALESCE(proxy_pool_enabled, false),
		       COALESCE(fast_scheduler_enabled, false),
		       COALESCE(max_retries, 2),
		       COALESCE(max_rate_limit_retries, 1),
		       COALESCE(allow_remote_migration, false),
		       COALESCE(auto_clean_error, false),
		       COALESCE(auto_clean_expired, false),
		       COALESCE(lazy_mode, false),
		       COALESCE(model_mapping, '{}'),
		       COALESCE(codex_model_mapping, '{}'),
			       COALESCE(background_refresh_interval_minutes, 2),
			       COALESCE(usage_probe_max_age_minutes, 10),
			       COALESCE(usage_probe_concurrency, 16),
			       COALESCE(usage_probe_responses_fallback_enabled, true),
			       COALESCE(recovery_probe_interval_minutes, 30),
		       COALESCE(scheduler_mode, 'round_robin'),
		       COALESCE(affinity_mode, 'bounded'),
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
		       COALESCE(prompt_filter_disabled_patterns, '[]'),
		       COALESCE(client_compat_mode, 'preserve'),
		       COALESCE(codex_min_cli_version, '0.118.0'),
		       COALESCE(usage_log_mode, 'full'),
		       COALESCE(usage_log_batch_size, 200),
		       COALESCE(usage_log_flush_interval_seconds, 5),
			       COALESCE(stream_flush_policy, 'immediate'),
			       COALESCE(stream_flush_interval_ms, 20),
			       COALESCE(NULLIF(TRIM(first_token_mode), ''), 'strict'),
			       COALESCE(first_token_timeout_seconds, 0),
			       COALESCE(NULLIF(TRIM(billing_tier_policy), ''), 'actual'),
			       COALESCE(image_storage_config, '{}'),
		       COALESCE(background_config, '{}'),
		       COALESCE(show_full_usage_numbers, false),
			       COALESCE(reasoning_effort_models, '[]'),
			       COALESCE(codex_force_websocket, false),
			       COALESCE(codex_ws_keepalive_enabled, false),
			       COALESCE(codex_ws_keepalive_interval_sec, 60),
			       COALESCE(codex_ws_hide_upstream_errors, true),
			       COALESCE(codex_ws_silent_retry_enabled, true),
			       COALESCE(codex_ws_silent_max_retries, 2),
			       COALESCE(auto_pause_5h_threshold, 0),
			       COALESCE(auto_pause_7d_threshold, 0)
			FROM system_settings WHERE id = 1
		`).Scan(
		&s.SiteName, &s.SiteLogo,
		&s.MaxConcurrency, &s.GlobalRPM, &s.TestModel, &s.TestConcurrency, &s.ProxyURL, &s.PgMaxConns, &s.RedisPoolSize,
		&s.AutoCleanUnauthorized, &s.AutoCleanRateLimited, &s.AdminSecret, &s.AutoCleanFullUsage,
		&s.ProxyPoolEnabled, &s.FastSchedulerEnabled, &s.MaxRetries, &s.MaxRateLimitRetries, &s.AllowRemoteMigration,
		&s.AutoCleanError, &s.AutoCleanExpired, &s.LazyMode, &s.ModelMapping, &s.CodexModelMapping,
		&s.BackgroundRefreshIntervalMinutes, &s.UsageProbeMaxAgeMinutes, &s.UsageProbeConcurrency, &s.UsageProbeResponsesFallbackEnabled, &s.RecoveryProbeIntervalMinutes,
		&s.SchedulerMode,
		&s.AffinityMode,
		&s.ResinURL, &s.ResinPlatformName,
		&s.PromptFilterEnabled, &s.PromptFilterMode, &s.PromptFilterThreshold, &s.PromptFilterStrictThreshold,
		&s.PromptFilterLogMatches, &s.PromptFilterMaxTextLength, &s.PromptFilterSensitiveWords,
		&s.PromptFilterCustomPatterns, &s.PromptFilterDisabledPatterns,
		&s.ClientCompatMode, &s.CodexMinCLIVersion, &s.UsageLogMode, &s.UsageLogBatchSize,
		&s.UsageLogFlushIntervalSeconds, &s.StreamFlushPolicy, &s.StreamFlushIntervalMS,
		&s.FirstTokenMode,
		&s.FirstTokenTimeoutSeconds,
		&s.BillingTierPolicy,
		&s.ImageStorageConfig,
		&s.BackgroundConfig,
		&s.ShowFullUsageNumbers,
		&s.ReasoningEffortModels,
		&s.CodexForceWebsocket,
		&s.CodexWSKeepaliveEnabled,
		&s.CodexWSKeepaliveIntervalSec,
		&s.CodexWSHideUpstreamErrors,
		&s.CodexWSSilentRetryEnabled,
		&s.CodexWSSilentMaxRetries,
		&s.AutoPause5hThreshold,
		&s.AutoPause7dThreshold,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	s.SiteName = NormalizeSiteName(s.SiteName)
	s.SiteLogo = strings.TrimSpace(s.SiteLogo)
	if strings.TrimSpace(s.ReasoningEffortModels) == "" {
		s.ReasoningEffortModels = "[]"
	}
	s.FirstTokenMode = normalizeFirstTokenMode(s.FirstTokenMode)
	s.BillingTierPolicy = normalizeBillingTierPolicy(s.BillingTierPolicy)
	return s, err
}

// UpdateSystemSettings 更新全局设置（upsert：无行时自动插入）
func (db *DB) UpdateSystemSettings(ctx context.Context, s *SystemSettings) error {
	reasoningEffortModels := strings.TrimSpace(s.ReasoningEffortModels)
	if reasoningEffortModels == "" {
		reasoningEffortModels = "[]"
	}
	firstTokenMode := normalizeFirstTokenMode(s.FirstTokenMode)
	billingTierPolicy := normalizeBillingTierPolicy(s.BillingTierPolicy)
	_, err := db.conn.ExecContext(ctx, `
			INSERT INTO system_settings (
				id, site_name, site_logo, max_concurrency, global_rpm, test_model, test_concurrency, proxy_url, pg_max_conns, redis_pool_size,
				auto_clean_unauthorized, auto_clean_rate_limited, admin_secret, auto_clean_full_usage, proxy_pool_enabled,
				fast_scheduler_enabled, max_retries, max_rate_limit_retries, allow_remote_migration, auto_clean_error, auto_clean_expired, lazy_mode, model_mapping, codex_model_mapping,
					background_refresh_interval_minutes, usage_probe_max_age_minutes, recovery_probe_interval_minutes,
					usage_probe_concurrency, usage_probe_responses_fallback_enabled,
				resin_url, resin_platform_name, prompt_filter_enabled, prompt_filter_mode, prompt_filter_threshold,
				prompt_filter_strict_threshold, prompt_filter_log_matches, prompt_filter_max_text_length,
				prompt_filter_sensitive_words, prompt_filter_custom_patterns, prompt_filter_disabled_patterns,
				client_compat_mode, codex_min_cli_version, usage_log_mode, usage_log_batch_size,
					usage_log_flush_interval_seconds, stream_flush_policy, stream_flush_interval_ms,
					first_token_timeout_seconds,
					first_token_mode,
					billing_tier_policy,
					image_storage_config,
				scheduler_mode,
				affinity_mode,
				background_config,
				show_full_usage_numbers,
					reasoning_effort_models,
					codex_force_websocket,
					codex_ws_keepalive_enabled,
					codex_ws_keepalive_interval_sec,
					codex_ws_hide_upstream_errors,
					codex_ws_silent_retry_enabled,
					codex_ws_silent_max_retries,
					auto_pause_5h_threshold,
					auto_pause_7d_threshold
					)
						VALUES (1, $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29, $30, $31, $32, $33, $34, $35, $36, $37, $38, $39, $40, $41, $42, $43, $44, $45, $46, $47, $48, $49, $50, $51, $52, $53, $54, $55, $56, $57, $58, $59, $60, $61, $62, $63)
				ON CONFLICT (id) DO UPDATE SET
				site_name               = EXCLUDED.site_name,
				site_logo               = EXCLUDED.site_logo,
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
				max_rate_limit_retries  = EXCLUDED.max_rate_limit_retries,
				allow_remote_migration  = EXCLUDED.allow_remote_migration,
				auto_clean_error        = EXCLUDED.auto_clean_error,
				auto_clean_expired      = EXCLUDED.auto_clean_expired,
				lazy_mode               = EXCLUDED.lazy_mode,
				model_mapping           = EXCLUDED.model_mapping,
				codex_model_mapping     = EXCLUDED.codex_model_mapping,
					background_refresh_interval_minutes = EXCLUDED.background_refresh_interval_minutes,
					usage_probe_max_age_minutes = EXCLUDED.usage_probe_max_age_minutes,
					usage_probe_concurrency = EXCLUDED.usage_probe_concurrency,
					usage_probe_responses_fallback_enabled = EXCLUDED.usage_probe_responses_fallback_enabled,
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
				prompt_filter_disabled_patterns = EXCLUDED.prompt_filter_disabled_patterns,
				client_compat_mode = EXCLUDED.client_compat_mode,
				codex_min_cli_version = EXCLUDED.codex_min_cli_version,
				usage_log_mode = EXCLUDED.usage_log_mode,
				usage_log_batch_size = EXCLUDED.usage_log_batch_size,
				usage_log_flush_interval_seconds = EXCLUDED.usage_log_flush_interval_seconds,
					stream_flush_policy = EXCLUDED.stream_flush_policy,
					stream_flush_interval_ms = EXCLUDED.stream_flush_interval_ms,
					first_token_timeout_seconds = EXCLUDED.first_token_timeout_seconds,
					first_token_mode = EXCLUDED.first_token_mode,
					billing_tier_policy = EXCLUDED.billing_tier_policy,
					image_storage_config = EXCLUDED.image_storage_config,
				scheduler_mode = EXCLUDED.scheduler_mode,
				affinity_mode = EXCLUDED.affinity_mode,
				background_config = EXCLUDED.background_config,
				show_full_usage_numbers = EXCLUDED.show_full_usage_numbers,
					reasoning_effort_models = EXCLUDED.reasoning_effort_models,
					codex_force_websocket = EXCLUDED.codex_force_websocket,
					codex_ws_keepalive_enabled = EXCLUDED.codex_ws_keepalive_enabled,
					codex_ws_keepalive_interval_sec = EXCLUDED.codex_ws_keepalive_interval_sec,
					codex_ws_hide_upstream_errors = EXCLUDED.codex_ws_hide_upstream_errors,
					codex_ws_silent_retry_enabled = EXCLUDED.codex_ws_silent_retry_enabled,
					codex_ws_silent_max_retries = EXCLUDED.codex_ws_silent_max_retries,
					auto_pause_5h_threshold = EXCLUDED.auto_pause_5h_threshold,
					auto_pause_7d_threshold = EXCLUDED.auto_pause_7d_threshold
			`, NormalizeSiteName(s.SiteName), strings.TrimSpace(s.SiteLogo),
		s.MaxConcurrency, s.GlobalRPM, s.TestModel, s.TestConcurrency, s.ProxyURL, s.PgMaxConns, s.RedisPoolSize,
		s.AutoCleanUnauthorized, s.AutoCleanRateLimited, s.AdminSecret, s.AutoCleanFullUsage, s.ProxyPoolEnabled,
		s.FastSchedulerEnabled, s.MaxRetries, s.MaxRateLimitRetries, s.AllowRemoteMigration, s.AutoCleanError, s.AutoCleanExpired, s.LazyMode, s.ModelMapping, s.CodexModelMapping,
		s.BackgroundRefreshIntervalMinutes, s.UsageProbeMaxAgeMinutes, s.RecoveryProbeIntervalMinutes,
		s.UsageProbeConcurrency, s.UsageProbeResponsesFallbackEnabled,
		s.ResinURL, s.ResinPlatformName, s.PromptFilterEnabled, s.PromptFilterMode, s.PromptFilterThreshold,
		s.PromptFilterStrictThreshold, s.PromptFilterLogMatches, s.PromptFilterMaxTextLength,
		s.PromptFilterSensitiveWords, s.PromptFilterCustomPatterns, s.PromptFilterDisabledPatterns,
		s.ClientCompatMode, s.CodexMinCLIVersion, s.UsageLogMode, s.UsageLogBatchSize,
		s.UsageLogFlushIntervalSeconds, s.StreamFlushPolicy, s.StreamFlushIntervalMS,
		s.FirstTokenTimeoutSeconds, firstTokenMode, billingTierPolicy, s.ImageStorageConfig, s.SchedulerMode, normalizeAffinityMode(s.AffinityMode), s.BackgroundConfig, s.ShowFullUsageNumbers, reasoningEffortModels,
		s.CodexForceWebsocket, s.CodexWSKeepaliveEnabled, normalizeCodexWSKeepaliveInterval(s.CodexWSKeepaliveIntervalSec),
		s.CodexWSHideUpstreamErrors, s.CodexWSSilentRetryEnabled, normalizeCodexWSSilentMaxRetries(s.CodexWSSilentMaxRetries),
		s.AutoPause5hThreshold, s.AutoPause7dThreshold)
	return err
}

// normalizeCodexWSKeepaliveInterval 把 WS 保活间隔(秒)归一,非正值 → 默认 60。
func normalizeCodexWSKeepaliveInterval(sec int) int {
	if sec <= 0 {
		return 60
	}
	return sec
}

// normalizeCodexWSSilentMaxRetries 把 WS 静默重试次数限制在 0-10。
func normalizeCodexWSSilentMaxRetries(retries int) int {
	if retries < 0 {
		return 0
	}
	if retries > 10 {
		return 10
	}
	return retries
}

// normalizeAffinityMode 把 SystemSettings.AffinityMode 落库前归一,空字符串 → "bounded"。
func normalizeAffinityMode(mode string) string {
	switch strings.TrimSpace(mode) {
	case "bounded", "off", "strict":
		return strings.TrimSpace(mode)
	default:
		return "bounded"
	}
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
				return inserted, err
			}
			affected, err := res.RowsAffected()
			if err != nil {
				return inserted, err
			}
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
			continue
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return inserted, err
		}
	}
	return inserted, nil
}

// DeleteProxy 删除单个代理
func (db *DB) DeleteProxy(ctx context.Context, id int64) error {
	res, err := db.conn.ExecContext(ctx, `DELETE FROM proxies WHERE id = $1`, id)
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
func (db *DB) UpdateProxy(ctx context.Context, id int64, urlValue *string, label *string, enabled *bool) error {
	if urlValue == nil && label == nil && enabled == nil {
		var exists int
		if err := db.conn.QueryRowContext(ctx, `SELECT 1 FROM proxies WHERE id = $1`, id).Scan(&exists); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return sql.ErrNoRows
			}
			return err
		}
		return nil
	}
	assignments := make([]string, 0, 3)
	args := make([]interface{}, 0, 4)
	if urlValue != nil {
		args = append(args, *urlValue)
		assignments = append(assignments, fmt.Sprintf("url = $%d", len(args)))
	}
	if label != nil {
		args = append(args, *label)
		assignments = append(assignments, fmt.Sprintf("label = $%d", len(args)))
	}
	if enabled != nil {
		args = append(args, *enabled)
		assignments = append(assignments, fmt.Sprintf("enabled = $%d", len(args)))
	}
	args = append(args, id)
	query := fmt.Sprintf("UPDATE proxies SET %s WHERE id = $%d", strings.Join(assignments, ", "), len(args))
	res, err := db.conn.ExecContext(ctx, query, args...)
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
	ID                   int64     `json:"id"`
	AccountID            int64     `json:"account_id"`
	ClientIP             string    `json:"client_ip"`
	Endpoint             string    `json:"endpoint"`
	Model                string    `json:"model"`
	EffectiveModel       string    `json:"effective_model"`
	PromptTokens         int       `json:"prompt_tokens"`
	CompletionTokens     int       `json:"completion_tokens"`
	TotalTokens          int       `json:"total_tokens"`
	StatusCode           int       `json:"status_code"`
	DurationMs           int       `json:"duration_ms"`
	InputTokens          int       `json:"input_tokens"`
	OutputTokens         int       `json:"output_tokens"`
	ReasoningTokens      int       `json:"reasoning_tokens"`
	FirstTokenMs         int       `json:"first_token_ms"`
	ReasoningEffort      string    `json:"reasoning_effort"`
	InboundEndpoint      string    `json:"inbound_endpoint"`
	UpstreamEndpoint     string    `json:"upstream_endpoint"`
	Stream               bool      `json:"stream"`
	Compact              bool      `json:"compact"`
	ViaWebsocket         bool      `json:"via_websocket"`
	CachedTokens         int       `json:"cached_tokens"`
	ServiceTier          string    `json:"service_tier"`
	RequestedServiceTier string    `json:"requested_service_tier"`
	ActualServiceTier    string    `json:"actual_service_tier"`
	BillingServiceTier   string    `json:"billing_service_tier"`
	APIKeyID             int64     `json:"api_key_id"`
	APIKeyName           string    `json:"api_key_name"`
	APIKeyMasked         string    `json:"api_key_masked"`
	ImageCount           int       `json:"image_count"`
	ImageWidth           int       `json:"image_width"`
	ImageHeight          int       `json:"image_height"`
	ImageBytes           int       `json:"image_bytes"`
	ImageFormat          string    `json:"image_format"`
	ImageSize            string    `json:"image_size"`
	AccountEmail         string    `json:"account_email"`
	CreatedAt            time.Time `json:"created_at"`
	AccountBilled        float64   `json:"account_billed"`
	UserBilled           float64   `json:"user_billed"`
	InputCost            float64   `json:"input_cost"`
	OutputCost           float64   `json:"output_cost"`
	CacheReadCost        float64   `json:"cache_read_cost"`
	TotalCost            float64   `json:"total_cost"`
	InputPrice           float64   `json:"input_price_per_mtoken"`
	OutputPrice          float64   `json:"output_price_per_mtoken"`
	CacheReadPrice       float64   `json:"cache_read_price_per_mtoken"`
	RateMultiplier       float64   `json:"rate_multiplier"`
	LongContext          bool      `json:"long_context"`
	LongContextThreshold int       `json:"long_context_threshold"`
	IsRetryAttempt       bool      `json:"is_retry_attempt"`
	AttemptIndex         int       `json:"attempt_index"`
	UpstreamErrorKind    string    `json:"upstream_error_kind"`
	ErrorMessage         string    `json:"error_message"`
}

// InsertUsageLog 将日志追加到内存缓冲（非阻塞）
func (db *DB) InsertUsageLog(ctx context.Context, log *UsageLogInput) error {
	if db == nil || log == nil || !db.shouldStoreUsageLog(log) {
		return nil
	}

	// 计算计费金额（基于 input/output tokens 和模型）
	// 使用 EffectiveModel 作为计费模型（如果有映射则使用映射后的模型）
	billingModel := log.EffectiveModel
	if billingModel == "" {
		billingModel = log.Model
	}

	billingServiceTier := log.BillingServiceTier
	if billingServiceTier == "" {
		billingServiceTier = log.ActualServiceTier
	}
	if billingServiceTier == "" {
		billingServiceTier = log.ServiceTier
	}

	serviceTier := log.ServiceTier
	if serviceTier == "" {
		serviceTier = log.ActualServiceTier
	}
	if serviceTier == "" {
		serviceTier = log.RequestedServiceTier
	}

	// 计算账号计费金额（基于实际计费 service tier）
	accountBilled := calculateCost(log.InputTokens, log.OutputTokens, log.CachedTokens, billingModel, billingServiceTier)

	// 用户计费金额与账号计费金额相同（简化版，未来可支持倍率）
	userBilled := accountBilled

	db.logMu.Lock()
	db.logBuf = append(db.logBuf, usageLogEntry{
		AccountID:            log.AccountID,
		ClientIP:             log.ClientIP,
		Endpoint:             log.Endpoint,
		Model:                log.Model,
		EffectiveModel:       log.EffectiveModel,
		PromptTokens:         log.PromptTokens,
		CompletionTokens:     log.CompletionTokens,
		TotalTokens:          log.TotalTokens,
		StatusCode:           log.StatusCode,
		DurationMs:           log.DurationMs,
		InputTokens:          log.InputTokens,
		OutputTokens:         log.OutputTokens,
		ReasoningTokens:      log.ReasoningTokens,
		FirstTokenMs:         log.FirstTokenMs,
		ReasoningEffort:      log.ReasoningEffort,
		InboundEndpoint:      log.InboundEndpoint,
		UpstreamEndpoint:     log.UpstreamEndpoint,
		Stream:               log.Stream,
		Compact:              log.Compact,
		ViaWebsocket:         log.ViaWebsocket,
		CachedTokens:         log.CachedTokens,
		ServiceTier:          serviceTier,
		RequestedServiceTier: log.RequestedServiceTier,
		ActualServiceTier:    log.ActualServiceTier,
		BillingServiceTier:   billingServiceTier,
		APIKeyID:             log.APIKeyID,
		APIKeyName:           log.APIKeyName,
		APIKeyMasked:         log.APIKeyMasked,
		ImageCount:           log.ImageCount,
		ImageWidth:           log.ImageWidth,
		ImageHeight:          log.ImageHeight,
		ImageBytes:           log.ImageBytes,
		ImageFormat:          log.ImageFormat,
		ImageSize:            log.ImageSize,
		AccountBilled:        accountBilled,
		UserBilled:           userBilled,
		IsRetryAttempt:       log.IsRetryAttempt,
		AttemptIndex:         log.AttemptIndex,
		UpstreamErrorKind:    log.UpstreamErrorKind,
		ErrorMessage:         log.ErrorMessage,
	})
	bufLen := len(db.logBuf)
	db.logMu.Unlock()

	// 增加触发 flush 的阈值，减少 flush 频率
	if bufLen >= db.GetUsageLogBatchSize() {
		db.notifyLogFlush()
	}
	return nil
}

// UsageLogInput 日志写入参数
type UsageLogInput struct {
	AccountID            int64
	ClientIP             string
	Endpoint             string
	Model                string
	EffectiveModel       string
	PromptTokens         int
	CompletionTokens     int
	TotalTokens          int
	StatusCode           int
	DurationMs           int
	InputTokens          int
	OutputTokens         int
	ReasoningTokens      int
	FirstTokenMs         int
	ReasoningEffort      string
	InboundEndpoint      string
	UpstreamEndpoint     string
	Stream               bool
	Compact              bool
	ViaWebsocket         bool
	CachedTokens         int
	ServiceTier          string
	RequestedServiceTier string
	ActualServiceTier    string
	BillingServiceTier   string
	APIKeyID             int64
	APIKeyName           string
	APIKeyMasked         string
	ImageCount           int
	ImageWidth           int
	ImageHeight          int
	ImageBytes           int
	ImageFormat          string
	ImageSize            string
	IsRetryAttempt       bool
	AttemptIndex         int
	UpstreamErrorKind    string
	ErrorMessage         string
}

func (l *UsageLog) populateBillingBreakdown() {
	billingModel := l.EffectiveModel
	if billingModel == "" {
		billingModel = l.Model
	}
	billingServiceTier := l.BillingServiceTier
	if billingServiceTier == "" {
		billingServiceTier = l.ServiceTier
	}
	breakdown := calculateCostBreakdown(l.InputTokens, l.OutputTokens, l.CachedTokens, billingModel, billingServiceTier)
	l.InputCost = breakdown.InputCost
	l.OutputCost = breakdown.OutputCost
	l.CacheReadCost = breakdown.CacheReadCost
	l.TotalCost = breakdown.TotalCost
	l.InputPrice = breakdown.InputPricePerMToken
	l.OutputPrice = breakdown.OutputPricePerMToken
	l.CacheReadPrice = breakdown.CacheReadPricePerMToken
	l.RateMultiplier = breakdown.ServiceTierCostMultiplier
	l.LongContext = breakdown.LongContext
	l.LongContextThreshold = breakdown.LongContextThreshold

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
		for {
			timer := time.NewTimer(db.getUsageLogFlushInterval())
			select {
			case <-timer.C:
				db.flushLogs()
			case <-db.logFlushNotify:
				stopTimer(timer)
				db.flushLogs()
			case <-db.logStop:
				stopTimer(timer)
				return
			}
		}
	}()
}

func stopTimer(timer *time.Timer) {
	if timer == nil {
		return
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

// flushLogs 将缓冲中的日志批量写入 PG
func (db *DB) flushLogs() {
	db.logMu.Lock()
	if len(db.logBuf) == 0 {
		db.logMu.Unlock()
		return
	}
	batch := db.logBuf
	db.logBuf = make([]usageLogEntry, 0, db.GetUsageLogBatchSize())
	db.logMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second) // 增加超时时间
	defer cancel()

	var err error
	// 使用批处理插入优化性能
	if db.driver == "postgres" {
		err = db.batchInsertLogs(ctx, batch)
	} else {
		err = db.insertSQLiteUsageLogBatch(ctx, batch)
	}
	if err != nil {
		log.Printf("批量写入日志失败，已重新放回缓冲区等待重试: %v", err)
		db.requeueUsageLogBatch(batch)
		return
	}

	if len(batch) > 10 {
		log.Printf("批量写入 %d 条使用日志", len(batch))
	}
}

func (db *DB) requeueUsageLogBatch(batch []usageLogEntry) {
	if len(batch) == 0 {
		return
	}
	db.logMu.Lock()
	defer db.logMu.Unlock()

	if len(db.logBuf) == 0 {
		requeued := make([]usageLogEntry, len(batch), len(batch)+db.GetUsageLogBatchSize())
		copy(requeued, batch)
		db.logBuf = requeued
		return
	}

	requeued := make([]usageLogEntry, 0, len(batch)+len(db.logBuf))
	requeued = append(requeued, batch...)
	requeued = append(requeued, db.logBuf...)
	db.logBuf = requeued
}

func (db *DB) insertSQLiteUsageLogBatch(ctx context.Context, batch []usageLogEntry) error {
	if len(batch) == 0 {
		return nil
	}

	// SQLite 使用事务插入
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开始事务: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO usage_logs (account_id, client_ip, endpoint, model, effective_model, prompt_tokens, completion_tokens, total_tokens, status_code, duration_ms,
			  input_tokens, output_tokens, reasoning_tokens, first_token_ms, reasoning_effort, inbound_endpoint, upstream_endpoint, stream, compact, cached_tokens, service_tier,
			  requested_service_tier, actual_service_tier, billing_service_tier,
			  api_key_id, api_key_name, api_key_masked, image_count, image_width, image_height, image_bytes, image_format, image_size, account_billed, user_billed,
			  is_retry_attempt, attempt_index, upstream_error_kind, error_message, via_websocket)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29, $30, $31, $32, $33, $34, $35, $36, $37, $38, $39, $40)`)
	if err != nil {
		return fmt.Errorf("准备语句: %w", err)
	}
	defer stmt.Close()

	for _, e := range batch {
		if _, err := stmt.ExecContext(ctx, e.AccountID, e.ClientIP, e.Endpoint, e.Model, e.EffectiveModel, e.PromptTokens, e.CompletionTokens, e.TotalTokens, e.StatusCode, e.DurationMs,
			e.InputTokens, e.OutputTokens, e.ReasoningTokens, e.FirstTokenMs, e.ReasoningEffort, e.InboundEndpoint, e.UpstreamEndpoint, e.Stream, e.Compact, e.CachedTokens, e.ServiceTier,
			e.RequestedServiceTier, e.ActualServiceTier, e.BillingServiceTier,
			e.APIKeyID, e.APIKeyName, e.APIKeyMasked, e.ImageCount, e.ImageWidth, e.ImageHeight, e.ImageBytes, e.ImageFormat, e.ImageSize, e.AccountBilled, e.UserBilled,
			e.IsRetryAttempt, e.AttemptIndex, e.UpstreamErrorKind, e.ErrorMessage, e.ViaWebsocket); err != nil {
			return fmt.Errorf("执行插入: %w", err)
		}
	}

	if err := db.applyAPIKeyQuotaUsageWithExec(ctx, tx, batch); err != nil {
		return fmt.Errorf("更新 API Key 额度用量: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交事务: %w", err)
	}
	return nil
}

// batchInsertLogs 使用 PostgreSQL 的批量插入优化
// 分批处理以避免 PostgreSQL 65535 参数限制（每行 40 个参数）。
func (db *DB) batchInsertLogs(ctx context.Context, batch []usageLogEntry) error {
	if len(batch) == 0 {
		return nil
	}

	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开始事务: %w", err)
	}
	defer tx.Rollback()

	const maxRowsPerBatch = 1600

	// 分批处理
	for start := 0; start < len(batch); start += maxRowsPerBatch {
		end := start + maxRowsPerBatch
		if end > len(batch) {
			end = len(batch)
		}
		subBatch := batch[start:end]

		if err := db.batchInsertLogsChunk(ctx, tx, subBatch); err != nil {
			return err
		}
	}
	if err := db.applyAPIKeyQuotaUsageWithExec(ctx, tx, batch); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交事务: %w", err)
	}
	return nil
}

// batchInsertLogsChunk 插入单批日志（内部辅助函数）
func (db *DB) batchInsertLogsChunk(ctx context.Context, execer sqlExecer, batch []usageLogEntry) error {
	if len(batch) == 0 {
		return nil
	}

	// 使用 COPY 或批量 VALUES 优化插入性能
	valueStrings := make([]string, 0, len(batch))
	valueArgs := make([]interface{}, 0, len(batch)*40)
	argIdx := 1

	for _, e := range batch {
		valueStrings = append(valueStrings, fmt.Sprintf("($%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d, $%d)",
			argIdx, argIdx+1, argIdx+2, argIdx+3, argIdx+4, argIdx+5, argIdx+6, argIdx+7, argIdx+8, argIdx+9,
			argIdx+10, argIdx+11, argIdx+12, argIdx+13, argIdx+14, argIdx+15, argIdx+16, argIdx+17, argIdx+18, argIdx+19,
			argIdx+20, argIdx+21, argIdx+22, argIdx+23, argIdx+24, argIdx+25, argIdx+26, argIdx+27, argIdx+28, argIdx+29,
			argIdx+30, argIdx+31, argIdx+32, argIdx+33, argIdx+34, argIdx+35, argIdx+36, argIdx+37, argIdx+38, argIdx+39))
		valueArgs = append(valueArgs, e.AccountID, e.ClientIP, e.Endpoint, e.Model, e.EffectiveModel, e.PromptTokens, e.CompletionTokens, e.TotalTokens, e.StatusCode, e.DurationMs,
			e.InputTokens, e.OutputTokens, e.ReasoningTokens, e.FirstTokenMs, e.ReasoningEffort, e.InboundEndpoint, e.UpstreamEndpoint, e.Stream, e.Compact, e.CachedTokens, e.ServiceTier,
			e.RequestedServiceTier, e.ActualServiceTier, e.BillingServiceTier,
			e.APIKeyID, e.APIKeyName, e.APIKeyMasked, e.ImageCount, e.ImageWidth, e.ImageHeight, e.ImageBytes, e.ImageFormat, e.ImageSize, e.AccountBilled, e.UserBilled,
			e.IsRetryAttempt, e.AttemptIndex, e.UpstreamErrorKind, e.ErrorMessage, e.ViaWebsocket)
		argIdx += 40
	}

	query := fmt.Sprintf(`INSERT INTO usage_logs (account_id, client_ip, endpoint, model, effective_model, prompt_tokens, completion_tokens, total_tokens, status_code, duration_ms,
		input_tokens, output_tokens, reasoning_tokens, first_token_ms, reasoning_effort, inbound_endpoint, upstream_endpoint, stream, compact, cached_tokens, service_tier,
		requested_service_tier, actual_service_tier, billing_service_tier,
		api_key_id, api_key_name, api_key_masked, image_count, image_width, image_height, image_bytes, image_format, image_size, account_billed, user_billed,
		is_retry_attempt, attempt_index, upstream_error_kind, error_message, via_websocket)
		VALUES %s`, strings.Join(valueStrings, ","))

	_, err := execer.ExecContext(ctx, query, valueArgs...)
	return err
}

func (db *DB) applyAPIKeyQuotaUsage(ctx context.Context, batch []usageLogEntry) error {
	if db == nil {
		return nil
	}
	return db.applyAPIKeyQuotaUsageWithExec(ctx, db.conn, batch)
}

func (db *DB) applyAPIKeyQuotaUsageWithExec(ctx context.Context, execer sqlExecer, batch []usageLogEntry) error {
	if db == nil || execer == nil || len(batch) == 0 {
		return nil
	}
	usageByKey := make(map[int64]float64)
	for _, entry := range batch {
		if entry.APIKeyID <= 0 || entry.UserBilled <= 0 || entry.StatusCode == 499 {
			continue
		}
		usageByKey[entry.APIKeyID] += entry.UserBilled
	}
	for id, amount := range usageByKey {
		if amount <= 0 {
			continue
		}
		if _, err := execer.ExecContext(ctx, `UPDATE api_keys SET quota_used = COALESCE(quota_used, 0) + $1 WHERE id = $2`, amount, id); err != nil {
			return err
		}
	}
	return nil
}

// UsageStats 使用统计
type UsageStats struct {
	TotalRequests      int64               `json:"total_requests"`
	TotalTokens        int64               `json:"total_tokens"`
	TotalPrompt        int64               `json:"total_prompt_tokens"`
	TotalCompletion    int64               `json:"total_completion_tokens"`
	TotalCachedTokens  int64               `json:"total_cached_tokens"`
	TotalCacheRate     float64             `json:"total_cache_rate"`
	TotalAccountBilled float64             `json:"total_account_billed"`
	TotalUserBilled    float64             `json:"total_user_billed"`
	AvgAccountBilled   float64             `json:"avg_account_billed_per_request"`
	AvgUserBilled      float64             `json:"avg_user_billed_per_request"`
	TodayRequests      int64               `json:"today_requests"`
	TodayTokens        int64               `json:"today_tokens"`
	TodayPrompt        int64               `json:"today_prompt_tokens"`
	TodayCompletion    int64               `json:"today_completion_tokens"`
	TodayCachedTokens  int64               `json:"today_cached_tokens"`
	TodayCacheRate     float64             `json:"today_cache_rate"`
	TodayAccountBilled float64             `json:"today_account_billed"`
	TodayUserBilled    float64             `json:"today_user_billed"`
	RPM                float64             `json:"rpm"`
	TPM                float64             `json:"tpm"`
	AvgFirstTokenMs    float64             `json:"avg_first_token_ms"`
	AvgDurationMs      float64             `json:"avg_duration_ms"`
	ErrorRate          float64             `json:"error_rate"`
	FeatureStats       UsageFeatureStat    `json:"feature_stats"`
	ModelStats         []UsageModelStat    `json:"model_stats"`
	EndpointStats      []UsageEndpointStat `json:"endpoint_stats"`
	APIKeyStats        []UsageAPIKeyStat   `json:"api_key_stats"`
}

// UsageModelStat 按计费模型聚合的请求和金额统计。
type UsageModelStat struct {
	Model         string  `json:"model"`
	Requests      int64   `json:"requests"`
	Tokens        int64   `json:"tokens"`
	InputTokens   int64   `json:"input_tokens"`
	OutputTokens  int64   `json:"output_tokens"`
	CachedTokens  int64   `json:"cached_tokens"`
	AccountBilled float64 `json:"account_billed"`
	UserBilled    float64 `json:"user_billed"`
	ErrorCount    int64   `json:"error_count"`
}

// UsageFeatureStat codex2api 代理能力维度的请求构成。
type UsageFeatureStat struct {
	StreamRequests    int64 `json:"stream_requests"`
	SyncRequests      int64 `json:"sync_requests"`
	FastRequests      int64 `json:"fast_requests"`
	CacheHitRequests  int64 `json:"cache_hit_requests"`
	ReasoningRequests int64 `json:"reasoning_requests"`
	ImageRequests     int64 `json:"image_requests"`
	RetryRequests     int64 `json:"retry_requests"`
	ErrorRequests     int64 `json:"error_requests"`
}

// UsageEndpointStat 按入口端点聚合的使用统计。
type UsageEndpointStat struct {
	Endpoint   string  `json:"endpoint"`
	Requests   int64   `json:"requests"`
	Tokens     int64   `json:"tokens"`
	ErrorCount int64   `json:"error_count"`
	UserBilled float64 `json:"user_billed"`
}

// UsageAPIKeyStat 按 API Key 聚合的使用统计。
type UsageAPIKeyStat struct {
	APIKeyID   int64   `json:"api_key_id"`
	Label      string  `json:"label"`
	Requests   int64   `json:"requests"`
	Tokens     int64   `json:"tokens"`
	ErrorCount int64   `json:"error_count"`
	UserBilled float64 `json:"user_billed"`
}

// TrafficSnapshot 近实时流量快照
type TrafficSnapshot struct {
	QPS     float64 `json:"qps"`
	QPSPeak float64 `json:"qps_peak"`
	TPS     float64 `json:"tps"`
	TPSPeak float64 `json:"tps_peak"`
}

// GetUsageStats 获取使用统计（基线 + 当前日志）。
// 当 rangeStart 为零值时回落到"今日"(本地 0 点起),与历史行为一致;
// 当传入显式区间时,today_* 字段语义变为"该区间内的统计",total_* 字段始终是全量累计。
// rangeEnd 为零值表示"至今"。
func (db *DB) GetUsageStats(ctx context.Context, rangeStart, rangeEnd time.Time) (*UsageStats, error) {
	if db.isSQLite() {
		return db.getUsageStatsSQLite(ctx, rangeStart, rangeEnd)
	}

	stats := &UsageStats{}
	now := time.Now()
	if rangeStart.IsZero() {
		rangeStart = time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	}
	minuteAgo := now.Add(-1 * time.Minute)
	endClause := ""
	args := []interface{}{rangeStart, minuteAgo}
	if !rangeEnd.IsZero() {
		endClause = " AND created_at < $3"
		args = append(args, rangeEnd)
	}

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
		COALESCE(AVG(NULLIF(first_token_ms, 0)), 0) AS avg_first_token_ms,
		COALESCE(AVG(duration_ms), 0) AS avg_duration_ms,
		COALESCE(SUM(CASE WHEN cached_tokens > 0 THEN 1 ELSE 0 END), 0) AS today_cache_hit_requests,
		COALESCE(SUM(CASE WHEN status_code >= 400 THEN 1 ELSE 0 END), 0) AS today_errors
	FROM usage_logs
	WHERE created_at >= $1` + endClause + `
	  AND status_code <> 499
	`

	var todayErrors int64
	var todayCacheHitRequests int64
	var todayCached int64
	err := db.conn.QueryRowContext(ctx, todayQuery, args...).Scan(
		&stats.TodayRequests, &stats.TodayTokens, &stats.TodayPrompt, &stats.TodayCompletion, &todayCached,
		&stats.TodayAccountBilled, &stats.TodayUserBilled,
		&stats.RPM, &stats.TPM,
		&stats.AvgFirstTokenMs,
		&stats.AvgDurationMs,
		&todayCacheHitRequests,
		&todayErrors,
	)
	if err != nil {
		return nil, err
	}

	// 统计当前可见请求总数和计费总额（排除 499，保证与使用统计列表口径一致）
	var visibleTotal int64
	var visibleCacheHitRequests int64
	var visibleFirstTokenSamples int64
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

	// 加上基线值（清空日志前保存的累计值）
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
	stats.TodayCachedTokens = todayCached
	stats.TotalAccountBilled = currentAccountBilled + bAccountBilled
	stats.TotalUserBilled = currentUserBilled + bUserBilled
	if stats.TodayRequests > 0 {
		stats.TodayCacheRate = float64(todayCacheHitRequests) / float64(stats.TodayRequests) * 100
	}
	if stats.TotalRequests > 0 {
		stats.TotalCacheRate = float64(visibleCacheHitRequests+bCacheHitRequests) / float64(stats.TotalRequests) * 100
	}
	if totalFirstTokenSamples := visibleFirstTokenSamples + bFirstTokenSamples; totalFirstTokenSamples > 0 {
		stats.AvgFirstTokenMs = (currentFirstTokenMsSum + bFirstTokenMsSum) / float64(totalFirstTokenSamples)
	}
	if stats.TotalRequests > 0 {
		stats.AvgAccountBilled = stats.TotalAccountBilled / float64(stats.TotalRequests)
		stats.AvgUserBilled = stats.TotalUserBilled / float64(stats.TotalRequests)
	}

	if stats.TodayRequests > 0 {
		stats.ErrorRate = float64(todayErrors) / float64(stats.TodayRequests) * 100
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

func (db *DB) usageStatsTimeWhere(column string, rangeStart, rangeEnd time.Time) (string, []interface{}) {
	if strings.TrimSpace(column) == "" {
		column = "created_at"
	}
	where := fmt.Sprintf("%s >= $1", column)
	args := []interface{}{db.timeArg(rangeStart)}
	if !rangeEnd.IsZero() {
		where += fmt.Sprintf(" AND %s < $2", column)
		args = append(args, db.timeArg(rangeEnd))
	}
	return where, args
}

func (db *DB) getUsageModelStats(ctx context.Context, limit int, rangeStart, rangeEnd time.Time) ([]UsageModelStat, error) {
	if limit <= 0 {
		limit = 10
	}
	timeWhere, args := db.usageStatsTimeWhere("created_at", rangeStart, rangeEnd)
	limitPlaceholder := fmt.Sprintf("$%d", len(args)+1)
	args = append(args, limit)
	rows, err := db.conn.QueryContext(ctx, `
		SELECT
			COALESCE(NULLIF(effective_model, ''), NULLIF(model, ''), 'unknown') AS model_name,
			COUNT(*) AS requests,
			COALESCE(SUM(total_tokens), 0) AS tokens,
			COALESCE(SUM(input_tokens), 0) AS input_tokens,
			COALESCE(SUM(output_tokens), 0) AS output_tokens,
			COALESCE(SUM(cached_tokens), 0) AS cached_tokens,
			COALESCE(SUM(account_billed), 0) AS account_billed,
			COALESCE(SUM(user_billed), 0) AS user_billed,
			COALESCE(SUM(CASE WHEN status_code >= 400 THEN 1 ELSE 0 END), 0) AS error_count
		FROM usage_logs
		WHERE `+timeWhere+` AND status_code <> 499
		GROUP BY 1
		ORDER BY user_billed DESC, requests DESC, model_name ASC
		LIMIT `+limitPlaceholder+`
	`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	stats := make([]UsageModelStat, 0, limit)
	for rows.Next() {
		var item UsageModelStat
		if err := rows.Scan(
			&item.Model,
			&item.Requests,
			&item.Tokens,
			&item.InputTokens,
			&item.OutputTokens,
			&item.CachedTokens,
			&item.AccountBilled,
			&item.UserBilled,
			&item.ErrorCount,
		); err != nil {
			return nil, err
		}
		stats = append(stats, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if stats == nil {
		stats = []UsageModelStat{}
	}
	return stats, nil
}

func (db *DB) populateUsageBreakdownStats(ctx context.Context, stats *UsageStats, rangeStart, rangeEnd time.Time) error {
	if stats == nil {
		return nil
	}
	timeWhere, args := db.usageStatsTimeWhere("created_at", rangeStart, rangeEnd)
	if err := db.conn.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN stream THEN 1 ELSE 0 END), 0) AS stream_requests,
			COALESCE(SUM(CASE WHEN NOT stream THEN 1 ELSE 0 END), 0) AS sync_requests,
				COALESCE(SUM(CASE WHEN LOWER(COALESCE(NULLIF(billing_service_tier, ''), service_tier, '')) IN ('fast', 'priority') THEN 1 ELSE 0 END), 0) AS fast_requests,
			COALESCE(SUM(CASE WHEN cached_tokens > 0 THEN 1 ELSE 0 END), 0) AS cache_hit_requests,
			COALESCE(SUM(CASE WHEN reasoning_tokens > 0 OR NULLIF(reasoning_effort, '') IS NOT NULL THEN 1 ELSE 0 END), 0) AS reasoning_requests,
			COALESCE(SUM(CASE WHEN LOWER(COALESCE(NULLIF(inbound_endpoint, ''), endpoint, '')) LIKE '%/images/%' OR LOWER(COALESCE(model, '')) LIKE 'gpt-image-%' OR image_count > 0 THEN 1 ELSE 0 END), 0) AS image_requests,
			COALESCE(SUM(CASE WHEN is_retry_attempt OR attempt_index > 0 THEN 1 ELSE 0 END), 0) AS retry_requests,
			COALESCE(SUM(CASE WHEN status_code >= 400 THEN 1 ELSE 0 END), 0) AS error_requests
		FROM usage_logs
		WHERE `+timeWhere+` AND status_code <> 499
	`, args...).Scan(
		&stats.FeatureStats.StreamRequests,
		&stats.FeatureStats.SyncRequests,
		&stats.FeatureStats.FastRequests,
		&stats.FeatureStats.CacheHitRequests,
		&stats.FeatureStats.ReasoningRequests,
		&stats.FeatureStats.ImageRequests,
		&stats.FeatureStats.RetryRequests,
		&stats.FeatureStats.ErrorRequests,
	); err != nil {
		return err
	}

	endpoints, err := db.getUsageEndpointStats(ctx, 8, rangeStart, rangeEnd)
	if err != nil {
		return err
	}
	apiKeys, err := db.getUsageAPIKeyStats(ctx, 8, rangeStart, rangeEnd)
	if err != nil {
		return err
	}
	stats.EndpointStats = endpoints
	stats.APIKeyStats = apiKeys
	return nil
}

func (db *DB) getUsageEndpointStats(ctx context.Context, limit int, rangeStart, rangeEnd time.Time) ([]UsageEndpointStat, error) {
	if limit <= 0 {
		limit = 8
	}
	timeWhere, args := db.usageStatsTimeWhere("created_at", rangeStart, rangeEnd)
	limitPlaceholder := fmt.Sprintf("$%d", len(args)+1)
	args = append(args, limit)
	rows, err := db.conn.QueryContext(ctx, `
		SELECT
			COALESCE(NULLIF(inbound_endpoint, ''), NULLIF(endpoint, ''), 'unknown') AS endpoint_name,
			COUNT(*) AS requests,
			COALESCE(SUM(total_tokens), 0) AS tokens,
			COALESCE(SUM(CASE WHEN status_code >= 400 THEN 1 ELSE 0 END), 0) AS error_count,
			COALESCE(SUM(user_billed), 0) AS user_billed
		FROM usage_logs
		WHERE `+timeWhere+` AND status_code <> 499
		GROUP BY 1
		ORDER BY requests DESC, endpoint_name ASC
		LIMIT `+limitPlaceholder+`
	`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]UsageEndpointStat, 0, limit)
	for rows.Next() {
		var item UsageEndpointStat
		if err := rows.Scan(&item.Endpoint, &item.Requests, &item.Tokens, &item.ErrorCount, &item.UserBilled); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if items == nil {
		items = []UsageEndpointStat{}
	}
	return items, nil
}

func (db *DB) getUsageAPIKeyStats(ctx context.Context, limit int, rangeStart, rangeEnd time.Time) ([]UsageAPIKeyStat, error) {
	if limit <= 0 {
		limit = 8
	}
	timeWhere, args := db.usageStatsTimeWhere("created_at", rangeStart, rangeEnd)
	limitPlaceholder := fmt.Sprintf("$%d", len(args)+1)
	args = append(args, limit)
	rows, err := db.conn.QueryContext(ctx, `
		SELECT
			COALESCE(api_key_id, 0) AS api_key_id,
			COALESCE(NULLIF(api_key_name, ''), NULLIF(api_key_masked, ''), 'unknown') AS api_key_label,
			COUNT(*) AS requests,
			COALESCE(SUM(total_tokens), 0) AS tokens,
			COALESCE(SUM(CASE WHEN status_code >= 400 THEN 1 ELSE 0 END), 0) AS error_count,
			COALESCE(SUM(user_billed), 0) AS user_billed
		FROM usage_logs
		WHERE `+timeWhere+` AND status_code <> 499
		GROUP BY 1, 2
		ORDER BY requests DESC, api_key_label ASC
		LIMIT `+limitPlaceholder+`
	`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	items := make([]UsageAPIKeyStat, 0, limit)
	for rows.Next() {
		var item UsageAPIKeyStat
		if err := rows.Scan(&item.APIKeyID, &item.Label, &item.Requests, &item.Tokens, &item.ErrorCount, &item.UserBilled); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if items == nil {
		items = []UsageAPIKeyStat{}
	}
	return items, nil
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
	query := `SELECT u.id, u.account_id, COALESCE(u.client_ip, ''), u.endpoint, u.model, COALESCE(u.effective_model, ''), u.prompt_tokens, u.completion_tokens, u.total_tokens, u.status_code, u.duration_ms,
	            COALESCE(u.input_tokens, 0), COALESCE(u.output_tokens, 0), COALESCE(u.reasoning_tokens, 0),
	            COALESCE(u.first_token_ms, 0), COALESCE(u.reasoning_effort, ''), COALESCE(u.inbound_endpoint, ''),
	            COALESCE(u.upstream_endpoint, ''), COALESCE(u.stream, false), COALESCE(u.compact, false), COALESCE(u.via_websocket, false), COALESCE(u.cached_tokens, 0), COALESCE(u.service_tier, ''),
	            COALESCE(u.requested_service_tier, ''), COALESCE(u.actual_service_tier, ''), COALESCE(u.billing_service_tier, ''),
	            COALESCE(u.api_key_id, 0), COALESCE(u.api_key_name, ''), COALESCE(u.api_key_masked, ''),
	            COALESCE(u.image_count, 0), COALESCE(u.image_width, 0), COALESCE(u.image_height, 0), COALESCE(u.image_bytes, 0),
		            COALESCE(u.image_format, ''), COALESCE(u.image_size, ''),
		            COALESCE(u.account_billed, 0), COALESCE(u.user_billed, 0),
		            COALESCE(u.is_retry_attempt, false), COALESCE(u.attempt_index, 0), COALESCE(u.upstream_error_kind, ''), COALESCE(u.error_message, ''),
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
		if err := rows.Scan(&l.ID, &l.AccountID, &l.ClientIP, &l.Endpoint, &l.Model, &l.EffectiveModel, &l.PromptTokens, &l.CompletionTokens, &l.TotalTokens, &l.StatusCode, &l.DurationMs,
			&l.InputTokens, &l.OutputTokens, &l.ReasoningTokens, &l.FirstTokenMs, &l.ReasoningEffort, &l.InboundEndpoint, &l.UpstreamEndpoint, &l.Stream, &l.Compact, &l.ViaWebsocket, &l.CachedTokens, &l.ServiceTier,
			&l.RequestedServiceTier, &l.ActualServiceTier, &l.BillingServiceTier,
			&l.APIKeyID, &l.APIKeyName, &l.APIKeyMasked, &l.ImageCount, &l.ImageWidth, &l.ImageHeight, &l.ImageBytes, &l.ImageFormat, &l.ImageSize, &l.AccountBilled, &l.UserBilled,
			&l.IsRetryAttempt, &l.AttemptIndex, &l.UpstreamErrorKind, &l.ErrorMessage, &credentialRaw, &createdAtRaw); err != nil {
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
	Errors4xx       int64   `json:"errors_4xx"`
	Errors5xx       int64   `json:"errors_5xx"`
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
	Model           string  `json:"model"`
	Requests        int64   `json:"requests"`
	Tokens          int64   `json:"tokens"`
	InputTokens     int64   `json:"input_tokens"`
	OutputTokens    int64   `json:"output_tokens"`
	ReasoningTokens int64   `json:"reasoning_tokens"`
	CachedTokens    int64   `json:"cached_tokens"`
	AccountBilled   float64 `json:"account_billed"`
	UserBilled      float64 `json:"user_billed"`
}

type AccountUsageDayStat struct {
	Date          string  `json:"date"`
	Label         string  `json:"label"`
	Requests      int64   `json:"requests"`
	Tokens        int64   `json:"tokens"`
	AccountBilled float64 `json:"account_billed"`
	UserBilled    float64 `json:"user_billed"`
}

// AccountUsageDetail 单账号用量详情
type AccountUsageDetail struct {
	PeriodDays            int                   `json:"period_days"`
	ActiveDays            int                   `json:"active_days"`
	TotalRequests         int64                 `json:"total_requests"`
	TotalTokens           int64                 `json:"total_tokens"`
	InputTokens           int64                 `json:"input_tokens"`
	OutputTokens          int64                 `json:"output_tokens"`
	ReasoningTokens       int64                 `json:"reasoning_tokens"`
	CachedTokens          int64                 `json:"cached_tokens"`
	CacheHitRate          float64               `json:"cache_hit_rate"`
	TotalAccountBilled    float64               `json:"total_account_billed"`
	TotalUserBilled       float64               `json:"total_user_billed"`
	AvgDailyAccountBilled float64               `json:"avg_daily_account_billed"`
	AvgDailyUserBilled    float64               `json:"avg_daily_user_billed"`
	AvgDailyRequests      float64               `json:"avg_daily_requests"`
	AvgDailyTokens        float64               `json:"avg_daily_tokens"`
	AvgDurationMs         float64               `json:"avg_duration_ms"`
	AvgFirstTokenMs       float64               `json:"avg_first_token_ms"`
	P95DurationMs         float64               `json:"p95_duration_ms"`
	ErrorRequests         int64                 `json:"error_requests"`
	ErrorRate             float64               `json:"error_rate"`
	RetryRequests         int64                 `json:"retry_requests"`
	FirstTokenSamples     int64                 `json:"first_token_samples"`
	StreamRequests        int64                 `json:"stream_requests"`
	StreamRate            float64               `json:"stream_rate"`
	CompactRequests       int64                 `json:"compact_requests"`
	CompactRate           float64               `json:"compact_rate"`
	Today                 AccountUsageDayStat   `json:"today"`
	HighestCostDay        *AccountUsageDayStat  `json:"highest_cost_day,omitempty"`
	HighestRequestDay     *AccountUsageDayStat  `json:"highest_request_day,omitempty"`
	History               []AccountUsageDayStat `json:"history"`
	Models                []AccountModelStat    `json:"models"`
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
		COALESCE(SUM(CASE WHEN status_code >= 400 AND status_code < 500 THEN 1 ELSE 0 END), 0) AS errors_4xx,
		COALESCE(SUM(CASE WHEN status_code >= 500 AND status_code < 600 THEN 1 ELSE 0 END), 0) AS errors_5xx
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
		if err := rows.Scan(&p.Bucket, &p.Requests, &p.AvgLatency, &p.InputTokens, &p.OutputTokens, &p.ReasoningTokens, &p.CachedTokens, &p.Errors4xx, &p.Errors5xx); err != nil {
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

// GetAccountUsageStats 查询单个账号的用量统计和模型分布。days<=0 表示全量。
func (db *DB) GetAccountUsageStats(ctx context.Context, accountID int64, days int) (*AccountUsageDetail, error) {
	periodDays := days
	if periodDays > 3650 {
		periodDays = 3650
	}
	allTime := periodDays <= 0
	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	periodEnd := todayStart.AddDate(0, 0, 1)
	periodStart := todayStart.AddDate(0, 0, -(periodDays - 1))
	if allTime {
		periodStart = time.Time{}
		periodDays = 0
	}
	result := &AccountUsageDetail{
		PeriodDays: periodDays,
		Today: AccountUsageDayStat{
			Date:  todayStart.Format("2006-01-02"),
			Label: todayStart.Format("01/02"),
		},
		History: []AccountUsageDayStat{},
	}

	// 汇总统计
	dayExpr := "DATE(created_at)"
	if db.isSQLite() {
		dayExpr = "date(created_at)"
	}
	timeWhere := "created_at >= $2 AND created_at < $3"
	queryArgs := []interface{}{accountID, db.timeArg(periodStart), db.timeArg(periodEnd)}
	if allTime {
		timeWhere = "created_at < $2"
		queryArgs = []interface{}{accountID, db.timeArg(periodEnd)}
	}

	summaryQuery := `
	SELECT
		COUNT(*),
		COALESCE(SUM(total_tokens), 0),
		COALESCE(SUM(input_tokens), 0),
		COALESCE(SUM(output_tokens), 0),
		COALESCE(SUM(reasoning_tokens), 0),
		COALESCE(SUM(cached_tokens), 0),
		COALESCE(SUM(account_billed), 0),
		COALESCE(SUM(user_billed), 0),
		COALESCE(AVG(NULLIF(duration_ms, 0)), 0),
		COALESCE(SUM(CASE WHEN cached_tokens > 0 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN status_code >= 400 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN is_retry_attempt OR attempt_index > 0 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN first_token_ms > 0 THEN first_token_ms ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN first_token_ms > 0 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN stream THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN compact THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN duration_ms > 0 THEN 1 ELSE 0 END), 0),
		COUNT(DISTINCT ` + dayExpr + `)
	FROM usage_logs
	WHERE account_id = $1
	  AND ` + timeWhere + `
	  AND status_code <> 499`

	var cacheHitRequests int64
	var firstTokenMsSum float64
	var durationSamples int64
	if err := db.conn.QueryRowContext(ctx, summaryQuery, queryArgs...).Scan(
		&result.TotalRequests, &result.TotalTokens,
		&result.InputTokens, &result.OutputTokens,
		&result.ReasoningTokens, &result.CachedTokens,
		&result.TotalAccountBilled, &result.TotalUserBilled,
		&result.AvgDurationMs,
		&cacheHitRequests,
		&result.ErrorRequests,
		&result.RetryRequests,
		&firstTokenMsSum,
		&result.FirstTokenSamples,
		&result.StreamRequests,
		&result.CompactRequests,
		&durationSamples,
		&result.ActiveDays,
	); err != nil {
		return nil, err
	}
	if result.TotalRequests > 0 {
		result.CacheHitRate = float64(cacheHitRequests) / float64(result.TotalRequests) * 100
		result.ErrorRate = float64(result.ErrorRequests) / float64(result.TotalRequests) * 100
		result.StreamRate = float64(result.StreamRequests) / float64(result.TotalRequests) * 100
		result.CompactRate = float64(result.CompactRequests) / float64(result.TotalRequests) * 100
	}
	if result.FirstTokenSamples > 0 {
		result.AvgFirstTokenMs = firstTokenMsSum / float64(result.FirstTokenSamples)
	}
	if result.ActiveDays > 0 {
		activeDays := float64(result.ActiveDays)
		result.AvgDailyAccountBilled = result.TotalAccountBilled / activeDays
		result.AvgDailyUserBilled = result.TotalUserBilled / activeDays
		result.AvgDailyRequests = float64(result.TotalRequests) / activeDays
		result.AvgDailyTokens = float64(result.TotalTokens) / activeDays
	}
	if durationSamples > 0 {
		offset := int64(math.Ceil(float64(durationSamples)*0.95)) - 1
		if offset < 0 {
			offset = 0
		}
		paramIdx := len(queryArgs) + 1
		p95Query := fmt.Sprintf(`
	SELECT duration_ms
	FROM usage_logs
	WHERE account_id = $1
	  AND %s
	  AND status_code <> 499
	  AND duration_ms > 0
	ORDER BY duration_ms ASC
	LIMIT $%d OFFSET $%d`, timeWhere, paramIdx, paramIdx+1)
		p95Args := append(append([]interface{}{}, queryArgs...), 1, offset)
		var p95Duration int64
		if err := db.conn.QueryRowContext(ctx, p95Query, p95Args...).Scan(&p95Duration); err != nil {
			return nil, err
		}
		result.P95DurationMs = float64(p95Duration)
	}

	todayQuery := `
	SELECT
		COUNT(*),
		COALESCE(SUM(total_tokens), 0),
		COALESCE(SUM(account_billed), 0),
		COALESCE(SUM(user_billed), 0)
	FROM usage_logs
	WHERE account_id = $1
	  AND created_at >= $2 AND created_at < $3
	  AND status_code <> 499`
	if err := db.conn.QueryRowContext(ctx, todayQuery, accountID, db.timeArg(todayStart), db.timeArg(periodEnd)).Scan(
		&result.Today.Requests,
		&result.Today.Tokens,
		&result.Today.AccountBilled,
		&result.Today.UserBilled,
	); err != nil {
		return nil, err
	}

	dateExpr := "TO_CHAR(created_at, 'YYYY-MM-DD')"
	labelExpr := "TO_CHAR(created_at, 'MM/DD')"
	if db.isSQLite() {
		dateExpr = "strftime('%Y-%m-%d', created_at)"
		labelExpr = "strftime('%m/%d', created_at)"
	}
	dayStatsQuery := `
	SELECT
		` + dateExpr + ` AS day_date,
		` + labelExpr + ` AS day_label,
		COUNT(*) AS requests,
		COALESCE(SUM(total_tokens), 0) AS tokens,
		COALESCE(SUM(account_billed), 0) AS account_billed,
		COALESCE(SUM(user_billed), 0) AS user_billed
	FROM usage_logs
	WHERE account_id = $1
	  AND ` + timeWhere + `
	  AND status_code <> 499
	GROUP BY 1, 2
	ORDER BY 1`
	dayRows, err := db.conn.QueryContext(ctx, dayStatsQuery, queryArgs...)
	if err != nil {
		return nil, err
	}
	defer dayRows.Close()
	for dayRows.Next() {
		var day AccountUsageDayStat
		if err := dayRows.Scan(&day.Date, &day.Label, &day.Requests, &day.Tokens, &day.AccountBilled, &day.UserBilled); err != nil {
			return nil, err
		}
		result.History = append(result.History, day)
		if result.HighestCostDay == nil ||
			day.AccountBilled > result.HighestCostDay.AccountBilled ||
			(day.AccountBilled == result.HighestCostDay.AccountBilled && day.Requests > result.HighestCostDay.Requests) {
			copyDay := day
			result.HighestCostDay = &copyDay
		}
		if result.HighestRequestDay == nil ||
			day.Requests > result.HighestRequestDay.Requests ||
			(day.Requests == result.HighestRequestDay.Requests && day.AccountBilled > result.HighestRequestDay.AccountBilled) {
			copyDay := day
			result.HighestRequestDay = &copyDay
		}
	}
	if err := dayRows.Err(); err != nil {
		return nil, err
	}

	// 模型分布
	modelQuery := `
	SELECT
		COALESCE(NULLIF(effective_model, ''), NULLIF(model, ''), 'unknown'),
		COUNT(*) AS requests,
		COALESCE(SUM(total_tokens), 0) AS tokens,
		COALESCE(SUM(input_tokens), 0) AS input_tokens,
		COALESCE(SUM(output_tokens), 0) AS output_tokens,
		COALESCE(SUM(reasoning_tokens), 0) AS reasoning_tokens,
		COALESCE(SUM(cached_tokens), 0) AS cached_tokens,
		COALESCE(SUM(account_billed), 0) AS account_billed,
		COALESCE(SUM(user_billed), 0) AS user_billed
	FROM usage_logs
	WHERE account_id = $1
	  AND ` + timeWhere + `
	  AND status_code <> 499
	GROUP BY 1
	ORDER BY 2 DESC`

	rows, err := db.conn.QueryContext(ctx, modelQuery, queryArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var m AccountModelStat
		if err := rows.Scan(
			&m.Model,
			&m.Requests,
			&m.Tokens,
			&m.InputTokens,
			&m.OutputTokens,
			&m.ReasoningTokens,
			&m.CachedTokens,
			&m.AccountBilled,
			&m.UserBilled,
		); err != nil {
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
	query := `SELECT u.id, u.account_id, COALESCE(u.client_ip, ''), u.endpoint, u.model, COALESCE(u.effective_model, ''), u.prompt_tokens, u.completion_tokens, u.total_tokens, u.status_code, u.duration_ms,
	            COALESCE(u.input_tokens, 0), COALESCE(u.output_tokens, 0), COALESCE(u.reasoning_tokens, 0),
	            COALESCE(u.first_token_ms, 0), COALESCE(u.reasoning_effort, ''), COALESCE(u.inbound_endpoint, ''),
	            COALESCE(u.upstream_endpoint, ''), COALESCE(u.stream, false), COALESCE(u.compact, false), COALESCE(u.via_websocket, false), COALESCE(u.cached_tokens, 0), COALESCE(u.service_tier, ''),
	            COALESCE(u.requested_service_tier, ''), COALESCE(u.actual_service_tier, ''), COALESCE(u.billing_service_tier, ''),
	            COALESCE(u.api_key_id, 0), COALESCE(u.api_key_name, ''), COALESCE(u.api_key_masked, ''),
	            COALESCE(u.image_count, 0), COALESCE(u.image_width, 0), COALESCE(u.image_height, 0), COALESCE(u.image_bytes, 0),
		            COALESCE(u.image_format, ''), COALESCE(u.image_size, ''),
		            COALESCE(u.account_billed, 0), COALESCE(u.user_billed, 0),
		            COALESCE(u.is_retry_attempt, false), COALESCE(u.attempt_index, 0), COALESCE(u.upstream_error_kind, ''), COALESCE(u.error_message, ''),
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
		if err := rows.Scan(&l.ID, &l.AccountID, &l.ClientIP, &l.Endpoint, &l.Model, &l.EffectiveModel, &l.PromptTokens, &l.CompletionTokens, &l.TotalTokens, &l.StatusCode, &l.DurationMs,
			&l.InputTokens, &l.OutputTokens, &l.ReasoningTokens, &l.FirstTokenMs, &l.ReasoningEffort, &l.InboundEndpoint, &l.UpstreamEndpoint, &l.Stream, &l.Compact, &l.ViaWebsocket, &l.CachedTokens, &l.ServiceTier,
			&l.RequestedServiceTier, &l.ActualServiceTier, &l.BillingServiceTier,
			&l.APIKeyID, &l.APIKeyName, &l.APIKeyMasked, &l.ImageCount, &l.ImageWidth, &l.ImageHeight, &l.ImageBytes, &l.ImageFormat, &l.ImageSize, &l.AccountBilled, &l.UserBilled,
			&l.IsRetryAttempt, &l.AttemptIndex, &l.UpstreamErrorKind, &l.ErrorMessage, &credentialRaw, &createdAtRaw); err != nil {
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
	Start           time.Time
	End             time.Time
	Page            int
	PageSize        int
	Email           string // LIKE 模糊匹配
	Model           string // 精确匹配
	Endpoint        string // 精确匹配 inbound_endpoint
	APIKeyID        *int64 // nil=全部
	AccountID       *int64 // nil=全部
	FastOnly        *bool  // nil=全部, true=仅fast, false=仅非fast
	StreamOnly      *bool  // nil=全部, true=仅stream, false=仅sync
	ErrorOnly       bool
	IncludeCanceled bool
	StatusCode      int
	StatusFamily    string
	ErrorKind       string
	Query           string
}

func (db *DB) buildUsageLogWhere(f UsageLogFilter) (string, []interface{}) {
	startArg, endArg := db.timeRangeArgs(f.Start, f.End)
	parts := []string{`u.created_at >= $1 AND u.created_at <= $2`}
	args := []interface{}{startArg, endArg}
	paramIdx := 3
	addArg := func(value interface{}) string {
		placeholder := fmt.Sprintf("$%d", paramIdx)
		args = append(args, value)
		paramIdx++
		return placeholder
	}

	if !f.IncludeCanceled {
		parts = append(parts, `u.status_code <> 499`)
	}
	if f.ErrorOnly {
		parts = append(parts, `(u.status_code >= 400 OR COALESCE(u.error_message, '') <> '' OR COALESCE(u.upstream_error_kind, '') <> '')`)
	}
	if f.Email != "" {
		p := addArg("%" + f.Email + "%")
		parts = append(parts, fmt.Sprintf(`(LOWER(COALESCE(CAST(a.credentials AS TEXT), '')) LIKE LOWER(%[1]s) OR LOWER(COALESCE(u.client_ip, '')) LIKE LOWER(%[1]s))`, p))
	}
	if f.Model != "" {
		p := addArg(f.Model)
		parts = append(parts, fmt.Sprintf(`(u.model = %s OR COALESCE(u.effective_model, '') = %s)`, p, p))
	}
	if f.Endpoint != "" {
		p := addArg(f.Endpoint)
		parts = append(parts, fmt.Sprintf(`u.inbound_endpoint = %s`, p))
	}
	if f.APIKeyID != nil {
		p := addArg(*f.APIKeyID)
		parts = append(parts, fmt.Sprintf(`u.api_key_id = %s`, p))
	}
	if f.AccountID != nil {
		p := addArg(*f.AccountID)
		parts = append(parts, fmt.Sprintf(`u.account_id = %s`, p))
	}
	if f.FastOnly != nil {
		tierExpr := `LOWER(COALESCE(NULLIF(u.billing_service_tier, ''), u.service_tier, ''))`
		if *f.FastOnly {
			parts = append(parts, tierExpr+` IN ('fast', 'priority')`)
		} else {
			parts = append(parts, tierExpr+` NOT IN ('fast', 'priority')`)
		}
	}
	if f.StreamOnly != nil {
		p := addArg(*f.StreamOnly)
		parts = append(parts, fmt.Sprintf(`COALESCE(u.stream, false) = %s`, p))
	}
	if f.StatusCode > 0 {
		p := addArg(f.StatusCode)
		parts = append(parts, fmt.Sprintf(`u.status_code = %s`, p))
	}
	switch strings.ToLower(strings.TrimSpace(f.StatusFamily)) {
	case "4xx":
		parts = append(parts, `u.status_code >= 400 AND u.status_code < 500`)
	case "5xx":
		parts = append(parts, `u.status_code >= 500 AND u.status_code < 600`)
	}
	if f.ErrorKind != "" {
		p := addArg(f.ErrorKind)
		parts = append(parts, fmt.Sprintf(`COALESCE(u.upstream_error_kind, '') = %s`, p))
	}
	if f.Query != "" {
		p := addArg("%" + f.Query + "%")
		parts = append(parts, fmt.Sprintf(`(
			LOWER(COALESCE(u.error_message, '')) LIKE LOWER(%[1]s)
			OR LOWER(COALESCE(u.upstream_error_kind, '')) LIKE LOWER(%[1]s)
			OR LOWER(COALESCE(u.model, '')) LIKE LOWER(%[1]s)
			OR LOWER(COALESCE(u.effective_model, '')) LIKE LOWER(%[1]s)
			OR LOWER(COALESCE(u.inbound_endpoint, '')) LIKE LOWER(%[1]s)
				OR LOWER(COALESCE(u.upstream_endpoint, '')) LIKE LOWER(%[1]s)
				OR LOWER(COALESCE(u.api_key_name, '')) LIKE LOWER(%[1]s)
				OR LOWER(COALESCE(u.api_key_masked, '')) LIKE LOWER(%[1]s)
				OR LOWER(COALESCE(u.client_ip, '')) LIKE LOWER(%[1]s)
				OR LOWER(COALESCE(CAST(a.credentials AS TEXT), '')) LIKE LOWER(%[1]s)
		)`, p))
	}

	return strings.Join(parts, " AND "), args
}

type UsageErrorSummary struct {
	TotalErrors   int64   `json:"total_errors"`
	Status4xx     int64   `json:"status_4xx"`
	Status5xx     int64   `json:"status_5xx"`
	Unauthorized  int64   `json:"unauthorized"`
	RateLimited   int64   `json:"rate_limited"`
	Canceled      int64   `json:"canceled"`
	Timeouts      int64   `json:"timeouts"`
	RetryAttempts int64   `json:"retry_attempts"`
	AvgDurationMs float64 `json:"avg_duration_ms"`
}

func (db *DB) GetUsageErrorSummary(ctx context.Context, f UsageLogFilter) (*UsageErrorSummary, error) {
	f.ErrorOnly = true
	f.IncludeCanceled = true
	where, args := db.buildUsageLogWhere(f)
	query := `SELECT
		COUNT(*),
		COALESCE(SUM(CASE WHEN u.status_code >= 400 AND u.status_code < 500 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN u.status_code >= 500 AND u.status_code < 600 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN u.status_code = 401 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN u.status_code = 429 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN u.status_code = 499 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN
			LOWER(COALESCE(u.upstream_error_kind, '')) LIKE '%timeout%'
			OR LOWER(COALESCE(u.error_message, '')) LIKE '%timeout%'
			OR LOWER(COALESCE(u.error_message, '')) LIKE '%deadline%'
		THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN COALESCE(u.is_retry_attempt, false) THEN 1 ELSE 0 END), 0),
		COALESCE(AVG(u.duration_ms), 0)
		FROM usage_logs u
		LEFT JOIN accounts a ON u.account_id = a.id
		WHERE ` + where

	result := &UsageErrorSummary{}
	if err := db.conn.QueryRowContext(ctx, query, args...).Scan(
		&result.TotalErrors,
		&result.Status4xx,
		&result.Status5xx,
		&result.Unauthorized,
		&result.RateLimited,
		&result.Canceled,
		&result.Timeouts,
		&result.RetryAttempts,
		&result.AvgDurationMs,
	); err != nil {
		return nil, err
	}
	return result, nil
}

// ListUsageLogsByTimeRangePaged 按时间范围分页查询请求日志（支持筛选）
func (db *DB) ListUsageLogsByTimeRangePaged(ctx context.Context, f UsageLogFilter) (*UsageLogPage, error) {
	if f.Page < 1 {
		f.Page = 1
	}
	if f.PageSize < 1 || f.PageSize > 500 {
		f.PageSize = 20
	}

	where, args := db.buildUsageLogWhere(f)
	offset := (f.Page - 1) * f.PageSize
	paramIdx := len(args) + 1
	where += fmt.Sprintf(` ORDER BY u.created_at DESC LIMIT $%d OFFSET $%d`, paramIdx, paramIdx+1)
	args = append(args, f.PageSize, offset)

	query := `SELECT u.id, u.account_id, COALESCE(u.client_ip, ''), u.endpoint, u.model, COALESCE(u.effective_model, ''), u.prompt_tokens, u.completion_tokens, u.total_tokens, u.status_code, u.duration_ms,
	            COALESCE(u.input_tokens, 0), COALESCE(u.output_tokens, 0), COALESCE(u.reasoning_tokens, 0),
	            COALESCE(u.first_token_ms, 0), COALESCE(u.reasoning_effort, ''), COALESCE(u.inbound_endpoint, ''),
	            COALESCE(u.upstream_endpoint, ''), COALESCE(u.stream, false), COALESCE(u.compact, false), COALESCE(u.via_websocket, false), COALESCE(u.cached_tokens, 0), COALESCE(u.service_tier, ''),
	            COALESCE(u.requested_service_tier, ''), COALESCE(u.actual_service_tier, ''), COALESCE(u.billing_service_tier, ''),
	            COALESCE(u.api_key_id, 0), COALESCE(u.api_key_name, ''), COALESCE(u.api_key_masked, ''),
	            COALESCE(u.image_count, 0), COALESCE(u.image_width, 0), COALESCE(u.image_height, 0), COALESCE(u.image_bytes, 0),
		            COALESCE(u.image_format, ''), COALESCE(u.image_size, ''),
			            COALESCE(u.account_billed, 0), COALESCE(u.user_billed, 0),
			            COALESCE(u.is_retry_attempt, false), COALESCE(u.attempt_index, 0), COALESCE(u.upstream_error_kind, ''), COALESCE(u.error_message, ''),
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
		if err := rows.Scan(&l.ID, &l.AccountID, &l.ClientIP, &l.Endpoint, &l.Model, &l.EffectiveModel, &l.PromptTokens, &l.CompletionTokens, &l.TotalTokens, &l.StatusCode, &l.DurationMs,
			&l.InputTokens, &l.OutputTokens, &l.ReasoningTokens, &l.FirstTokenMs, &l.ReasoningEffort, &l.InboundEndpoint, &l.UpstreamEndpoint, &l.Stream, &l.Compact, &l.ViaWebsocket, &l.CachedTokens,
			&l.ServiceTier, &l.RequestedServiceTier, &l.ActualServiceTier, &l.BillingServiceTier, &l.APIKeyID, &l.APIKeyName, &l.APIKeyMasked, &l.ImageCount, &l.ImageWidth, &l.ImageHeight, &l.ImageBytes, &l.ImageFormat, &l.ImageSize,
			&l.AccountBilled, &l.UserBilled, &l.IsRetryAttempt, &l.AttemptIndex, &l.UpstreamErrorKind, &l.ErrorMessage, &credentialRaw, &createdAtRaw, &result.Total); err != nil {
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

// ListUsageLogsByFilter 按过滤条件查询请求日志，不分页，用于导出。
func (db *DB) ListUsageLogsByFilter(ctx context.Context, f UsageLogFilter) ([]*UsageLog, error) {
	where, args := db.buildUsageLogWhere(f)
	where += ` ORDER BY u.created_at DESC`

	query := `SELECT u.id, u.account_id, COALESCE(u.client_ip, ''), u.endpoint, u.model, COALESCE(u.effective_model, ''), u.prompt_tokens, u.completion_tokens, u.total_tokens, u.status_code, u.duration_ms,
			COALESCE(u.input_tokens, 0), COALESCE(u.output_tokens, 0), COALESCE(u.reasoning_tokens, 0),
			COALESCE(u.first_token_ms, 0), COALESCE(u.reasoning_effort, ''), COALESCE(u.inbound_endpoint, ''),
			COALESCE(u.upstream_endpoint, ''), COALESCE(u.stream, false), COALESCE(u.compact, false), COALESCE(u.via_websocket, false), COALESCE(u.cached_tokens, 0), COALESCE(u.service_tier, ''),
			COALESCE(u.requested_service_tier, ''), COALESCE(u.actual_service_tier, ''), COALESCE(u.billing_service_tier, ''),
			COALESCE(u.api_key_id, 0), COALESCE(u.api_key_name, ''), COALESCE(u.api_key_masked, ''),
			COALESCE(u.image_count, 0), COALESCE(u.image_width, 0), COALESCE(u.image_height, 0), COALESCE(u.image_bytes, 0),
			COALESCE(u.image_format, ''), COALESCE(u.image_size, ''),
			COALESCE(u.account_billed, 0), COALESCE(u.user_billed, 0),
			COALESCE(u.is_retry_attempt, false), COALESCE(u.attempt_index, 0), COALESCE(u.upstream_error_kind, ''), COALESCE(u.error_message, ''),
			COALESCE(CAST(a.credentials AS TEXT), '{}'), u.created_at
		FROM usage_logs u
		LEFT JOIN accounts a ON u.account_id = a.id
		WHERE ` + where

	rows, err := db.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var logs []*UsageLog
	for rows.Next() {
		l := &UsageLog{}
		var credentialRaw interface{}
		var createdAtRaw interface{}
		if err := rows.Scan(&l.ID, &l.AccountID, &l.ClientIP, &l.Endpoint, &l.Model, &l.EffectiveModel, &l.PromptTokens, &l.CompletionTokens, &l.TotalTokens, &l.StatusCode, &l.DurationMs,
			&l.InputTokens, &l.OutputTokens, &l.ReasoningTokens, &l.FirstTokenMs, &l.ReasoningEffort, &l.InboundEndpoint, &l.UpstreamEndpoint, &l.Stream, &l.Compact, &l.ViaWebsocket, &l.CachedTokens,
			&l.ServiceTier, &l.RequestedServiceTier, &l.ActualServiceTier, &l.BillingServiceTier, &l.APIKeyID, &l.APIKeyName, &l.APIKeyMasked, &l.ImageCount, &l.ImageWidth, &l.ImageHeight, &l.ImageBytes, &l.ImageFormat, &l.ImageSize,
			&l.AccountBilled, &l.UserBilled, &l.IsRetryAttempt, &l.AttemptIndex, &l.UpstreamErrorKind, &l.ErrorMessage, &credentialRaw, &createdAtRaw); err != nil {
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
	if logs == nil {
		logs = []*UsageLog{}
	}
	return logs, rows.Err()
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
			cache_hit_requests = cache_hit_requests + COALESCE((SELECT SUM(CASE WHEN cached_tokens > 0 THEN 1 ELSE 0 END) FROM usage_logs WHERE status_code <> 499), 0),
			first_token_ms_sum = first_token_ms_sum + COALESCE((SELECT SUM(CASE WHEN first_token_ms > 0 THEN first_token_ms ELSE 0 END) FROM usage_logs WHERE status_code <> 499), 0),
			first_token_samples = first_token_samples + COALESCE((SELECT SUM(CASE WHEN first_token_ms > 0 THEN 1 ELSE 0 END) FROM usage_logs WHERE status_code <> 499), 0),
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
	AccountID             int64
	SuccessCount          int64
	ErrorCount            int64
	RetryErrorCount       int64
	RateLimitAttemptCount int64
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
	retryFalse := "COALESCE(is_retry_attempt, false) = false"
	retryTrue := "COALESCE(is_retry_attempt, false) = true"
	if db.isSQLite() {
		retryFalse = "COALESCE(is_retry_attempt, 0) = 0"
		retryTrue = "COALESCE(is_retry_attempt, 0) = 1"
	}
	query := fmt.Sprintf(`
	SELECT account_id,
		COALESCE(SUM(CASE WHEN status_code < 400 AND %s THEN 1 ELSE 0 END), 0) AS success_count,
		COALESCE(SUM(CASE WHEN status_code >= 400 AND %s THEN 1 ELSE 0 END), 0) AS error_count,
		COALESCE(SUM(CASE WHEN status_code >= 400 AND %s THEN 1 ELSE 0 END), 0) AS retry_error_count,
		COALESCE(SUM(CASE WHEN status_code = 429 THEN 1 ELSE 0 END), 0) AS rate_limit_attempt_count
	FROM usage_logs
	WHERE created_at >= $1
	GROUP BY account_id
	`, retryFalse, retryFalse, retryTrue)
	rows, err := db.conn.QueryContext(ctx, query, db.timeArg(since))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[int64]*AccountRequestCount)
	for rows.Next() {
		rc := &AccountRequestCount{}
		if err := rows.Scan(&rc.AccountID, &rc.SuccessCount, &rc.ErrorCount, &rc.RetryErrorCount, &rc.RateLimitAttemptCount); err != nil {
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

// GetAccountBilledSince 返回指定时间戳以来 account_billed 的总和
func (db *DB) GetAccountBilledSince(ctx context.Context, accountID int64, since time.Time) (float64, error) {
	var billed float64
	err := db.conn.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(account_billed), 0) FROM usage_logs WHERE account_id = $1 AND created_at >= $2 AND status_code <> 499`,
		accountID, db.timeArg(since)).Scan(&billed)
	return billed, err
}

// GetAccountsBilledSince 批量返回每个账号在各自 since 之后的 account_billed 总和。
func (db *DB) GetAccountsBilledSince(ctx context.Context, windows map[int64]time.Time) (map[int64]float64, error) {
	result := make(map[int64]float64, len(windows))
	if len(windows) == 0 {
		return result, nil
	}

	ids := make([]int64, 0, len(windows))
	for accountID, since := range windows {
		if accountID <= 0 || since.IsZero() {
			continue
		}
		ids = append(ids, accountID)
		result[accountID] = 0
	}
	if len(ids) == 0 {
		return result, nil
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })

	const maxRowsPerBatch = 1000
	for start := 0; start < len(ids); start += maxRowsPerBatch {
		end := start + maxRowsPerBatch
		if end > len(ids) {
			end = len(ids)
		}
		if err := db.getAccountsBilledSinceChunk(ctx, ids[start:end], windows, result); err != nil {
			return nil, err
		}
	}
	return result, nil
}

func (db *DB) getAccountsBilledSinceChunk(ctx context.Context, ids []int64, windows map[int64]time.Time, result map[int64]float64) error {
	if len(ids) == 0 {
		return nil
	}

	values := make([]string, 0, len(ids))
	args := make([]interface{}, 0, len(ids)*2)
	argIdx := 1
	for _, accountID := range ids {
		if db.isSQLite() {
			values = append(values, fmt.Sprintf("($%d, $%d)", argIdx, argIdx+1))
		} else {
			values = append(values, fmt.Sprintf("($%d::BIGINT, $%d::TIMESTAMPTZ)", argIdx, argIdx+1))
		}
		args = append(args, accountID, db.timeArg(windows[accountID]))
		argIdx += 2
	}

	query := fmt.Sprintf(`
	WITH billing_windows(account_id, since_at) AS (
		VALUES %s
	)
	SELECT billing_windows.account_id, COALESCE(SUM(usage_logs.account_billed), 0) AS account_billed
	FROM billing_windows
	LEFT JOIN usage_logs
		ON usage_logs.account_id = billing_windows.account_id
		AND usage_logs.created_at >= billing_windows.since_at
		AND usage_logs.status_code <> 499
	GROUP BY billing_windows.account_id
	`, strings.Join(values, ","))

	rows, err := db.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()

	for rows.Next() {
		var accountID int64
		var billed float64
		if err := rows.Scan(&accountID, &billed); err != nil {
			return err
		}
		result[accountID] = billed
	}
	return rows.Err()
}

// ==================== Accounts ====================

// ListActive 获取所有未删除账号。
func (db *DB) ListActive(ctx context.Context) ([]*AccountRow, error) {
	query := `
		SELECT id, name, platform, type, credentials, proxy_url, status, cooldown_reason, cooldown_until, error_message, COALESCE(enabled, true), COALESCE(locked, false), COALESCE(credit_enabled, false), COALESCE(credit_skip_usage_window, false), COALESCE(skip_warm_tier, false), score_bias_override, base_concurrency_override, COALESCE(tags, '[]'), created_at, updated_at
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
		var tagsRaw interface{}
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
			&a.CreditEnabled,
			&a.CreditSkipUsageWindow,
			&a.SkipWarmTier,
			&a.ScoreBiasOverride,
			&a.BaseConcurrencyOverride,
			&tagsRaw,
			&createdAtRaw,
			&updatedAtRaw,
		); err != nil {
			return nil, fmt.Errorf("扫描账号行失败: %w", err)
		}
		a.Credentials = decodeCredentials(credRaw)
		a.Tags = decodeTagsValue(tagsRaw)
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

func (db *DB) ListActiveModelCooldowns(ctx context.Context) ([]*AccountModelCooldownRow, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT account_id, model, COALESCE(reason, ''), reset_at, updated_at
		FROM account_model_cooldowns
		WHERE reset_at > $1
		ORDER BY account_id, model
	`, db.timeArg(time.Now()))
	if err != nil {
		return nil, fmt.Errorf("查询模型冷却失败: %w", err)
	}
	defer rows.Close()

	var result []*AccountModelCooldownRow
	for rows.Next() {
		row := &AccountModelCooldownRow{}
		var resetRaw interface{}
		var updatedRaw interface{}
		if err := rows.Scan(&row.AccountID, &row.Model, &row.Reason, &resetRaw, &updatedRaw); err != nil {
			return nil, err
		}
		var parseErr error
		row.ResetAt, parseErr = parseDBTimeValue(resetRaw)
		if parseErr != nil {
			return nil, fmt.Errorf("解析模型冷却 reset_at 失败: %w", parseErr)
		}
		row.UpdatedAt, parseErr = parseDBTimeValue(updatedRaw)
		if parseErr != nil {
			return nil, fmt.Errorf("解析模型冷却 updated_at 失败: %w", parseErr)
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

func (db *DB) SetModelCooldown(ctx context.Context, accountID int64, model, reason string, resetAt time.Time) error {
	model = strings.TrimSpace(model)
	if accountID == 0 || model == "" || resetAt.IsZero() {
		return nil
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "rate_limited"
	}
	if db.isSQLite() {
		_, err := db.conn.ExecContext(ctx, `
			INSERT INTO account_model_cooldowns (account_id, model, reason, reset_at, updated_at)
			VALUES ($1, $2, $3, $4, CURRENT_TIMESTAMP)
			ON CONFLICT(account_id, model) DO UPDATE SET
				reason = excluded.reason,
				reset_at = excluded.reset_at,
				updated_at = CURRENT_TIMESTAMP
		`, accountID, model, reason, db.timeArg(resetAt))
		return err
	}
	_, err := db.conn.ExecContext(ctx, `
		INSERT INTO account_model_cooldowns (account_id, model, reason, reset_at, updated_at)
		VALUES ($1, $2, $3, $4, NOW())
		ON CONFLICT(account_id, model) DO UPDATE SET
			reason = EXCLUDED.reason,
			reset_at = EXCLUDED.reset_at,
			updated_at = NOW()
	`, accountID, model, reason, db.timeArg(resetAt))
	return err
}

func (db *DB) ClearModelCooldown(ctx context.Context, accountID int64, model string) error {
	model = strings.TrimSpace(model)
	if accountID == 0 || model == "" {
		return nil
	}
	_, err := db.conn.ExecContext(ctx, `DELETE FROM account_model_cooldowns WHERE account_id = $1 AND model = $2`, accountID, model)
	return err
}

func (db *DB) ClearExpiredModelCooldowns(ctx context.Context) error {
	_, err := db.conn.ExecContext(ctx, `DELETE FROM account_model_cooldowns WHERE reset_at <= $1`, db.timeArg(time.Now()))
	return err
}

// GetAccountByID 获取未删除账号的完整数据库行。
func (db *DB) GetAccountByID(ctx context.Context, id int64) (*AccountRow, error) {
	return db.getAccountByID(ctx, id, false)
}

// GetAccountByIDIncludingDeleted 获取账号（包含回收站中的已删除账号），
// 用于回收站测试连接等不依赖运行时池的场景。
func (db *DB) GetAccountByIDIncludingDeleted(ctx context.Context, id int64) (*AccountRow, error) {
	return db.getAccountByID(ctx, id, true)
}

func (db *DB) getAccountByID(ctx context.Context, id int64, includeDeleted bool) (*AccountRow, error) {
	deletedFilter := "AND status <> 'deleted' AND COALESCE(error_message, '') <> 'deleted'"
	if includeDeleted {
		deletedFilter = ""
	}
	query := `
		SELECT id, name, platform, type, credentials, proxy_url, status, cooldown_reason, cooldown_until, error_message, COALESCE(enabled, true), COALESCE(locked, false), COALESCE(credit_enabled, false), COALESCE(credit_skip_usage_window, false), COALESCE(skip_warm_tier, false), score_bias_override, base_concurrency_override, COALESCE(tags, '[]'), created_at, updated_at
		FROM accounts
		WHERE id = $1 ` + deletedFilter + `
		LIMIT 1
	`
	a := &AccountRow{}
	var credRaw interface{}
	var cooldownUntilRaw interface{}
	var tagsRaw interface{}
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
		&a.CreditEnabled,
		&a.CreditSkipUsageWindow,
		&a.SkipWarmTier,
		&a.ScoreBiasOverride,
		&a.BaseConcurrencyOverride,
		&tagsRaw,
		&createdAtRaw,
		&updatedAtRaw,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, sql.ErrNoRows
		}
		return nil, fmt.Errorf("查询账号失败: %w", err)
	}
	a.Credentials = decodeCredentials(credRaw)
	a.Tags = decodeTagsValue(tagsRaw)
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
func (db *DB) UpdateAccountSchedulerConfig(ctx context.Context, id int64, scoreBiasOverride OptionalNullInt64, baseConcurrencyOverride OptionalNullInt64, allowedAPIKeyIDs OptionalInt64Slice) error {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if scoreBiasOverride.Set || baseConcurrencyOverride.Set {
		sets := make([]string, 0, 3)
		args := make([]interface{}, 0, 3)
		add := func(column string, value interface{}) {
			args = append(args, value)
			ph := "?"
			if !db.isSQLite() {
				ph = fmt.Sprintf("$%d", len(args))
			}
			sets = append(sets, column+" = "+ph)
		}
		if scoreBiasOverride.Set {
			add("score_bias_override", nullableInt64Value(scoreBiasOverride.Value))
		}
		if baseConcurrencyOverride.Set {
			add("base_concurrency_override", nullableInt64Value(baseConcurrencyOverride.Value))
		}
		sets = append(sets, "updated_at = CURRENT_TIMESTAMP")
		args = append(args, id)
		ph := "?"
		if !db.isSQLite() {
			ph = fmt.Sprintf("$%d", len(args))
		}
		result, err := tx.ExecContext(ctx, "UPDATE accounts SET "+strings.Join(sets, ", ")+" WHERE id = "+ph+" AND status <> 'deleted' AND COALESCE(error_message, '') <> 'deleted'", args...)
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
	} else {
		query := `SELECT 1 FROM accounts WHERE id = $1 AND status <> 'deleted' AND COALESCE(error_message, '') <> 'deleted'`
		if db.isSQLite() {
			query = `SELECT 1 FROM accounts WHERE id = ? AND status <> 'deleted' AND COALESCE(error_message, '') <> 'deleted'`
		}
		var exists int
		if err := tx.QueryRowContext(ctx, query, id).Scan(&exists); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return sql.ErrNoRows
			}
			return err
		}
	}

	if allowedAPIKeyIDs.Set {
		selectQuery := `SELECT credentials FROM accounts WHERE id = $1 AND status <> 'deleted' AND COALESCE(error_message, '') <> 'deleted'`
		if db.isSQLite() {
			selectQuery = `SELECT credentials FROM accounts WHERE id = ? AND status <> 'deleted' AND COALESCE(error_message, '') <> 'deleted'`
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

// UpdateAccountSchedulerMetadata applies scheduler overrides and UI metadata in
// one transaction. Runtime store updates should happen only after this returns.
func (db *DB) UpdateAccountSchedulerMetadata(ctx context.Context, id int64, scoreBiasOverride OptionalNullInt64, baseConcurrencyOverride OptionalNullInt64, skipWarmTier OptionalBool, allowedAPIKeyIDs OptionalInt64Slice, tags OptionalStringSlice, groupIDs OptionalInt64Slice, proxyURL OptionalString, credentialUpdates map[string]interface{}) error {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	query := `SELECT credentials FROM accounts WHERE id = $1 AND status <> 'deleted' AND COALESCE(error_message, '') <> 'deleted'`
	if db.isSQLite() {
		query = `SELECT credentials FROM accounts WHERE id = ? AND status <> 'deleted' AND COALESCE(error_message, '') <> 'deleted'`
	} else {
		query += ` FOR UPDATE`
	}
	var currentRaw interface{}
	if err := tx.QueryRowContext(ctx, query, id).Scan(&currentRaw); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return sql.ErrNoRows
		}
		return err
	}

	sets := make([]string, 0, 6)
	args := make([]interface{}, 0, 8)
	add := func(column string, value interface{}) {
		args = append(args, value)
		ph := "?"
		if !db.isSQLite() {
			ph = fmt.Sprintf("$%d", len(args))
		}
		sets = append(sets, column+" = "+ph)
	}
	if scoreBiasOverride.Set {
		add("score_bias_override", nullableInt64Value(scoreBiasOverride.Value))
	}
	if baseConcurrencyOverride.Set {
		add("base_concurrency_override", nullableInt64Value(baseConcurrencyOverride.Value))
	}
	if skipWarmTier.Set {
		add("skip_warm_tier", skipWarmTier.Value)
	}
	if tags.Set {
		if db.isSQLite() {
			add("tags", encodeTagsJSON(tags.Values))
		} else {
			args = append(args, encodeTagsJSON(tags.Values))
			sets = append(sets, fmt.Sprintf("tags = $%d::jsonb", len(args)))
		}
	}
	if proxyURL.Set {
		add("proxy_url", strings.TrimSpace(proxyURL.Value))
	}
	if allowedAPIKeyIDs.Set {
		if credentialUpdates == nil {
			credentialUpdates = make(map[string]interface{}, 1)
		}
		credentialUpdates["allowed_api_key_ids"] = normalizePositiveInt64Slice(allowedAPIKeyIDs.Values)
	}
	if len(credentialUpdates) > 0 {
		merged := mergeCredentialMaps(decodeCredentials(currentRaw), credentialUpdates)
		credJSON, err := json.Marshal(merged)
		if err != nil {
			return fmt.Errorf("序列化 credentials 失败: %w", err)
		}
		if db.isSQLite() {
			add("credentials", credJSON)
		} else {
			args = append(args, credJSON)
			sets = append(sets, fmt.Sprintf("credentials = $%d::jsonb", len(args)))
		}
	}
	if len(sets) > 0 {
		sets = append(sets, "updated_at = CURRENT_TIMESTAMP")
		args = append(args, id)
		ph := "?"
		if !db.isSQLite() {
			ph = fmt.Sprintf("$%d", len(args))
		}
		if _, err := tx.ExecContext(ctx, "UPDATE accounts SET "+strings.Join(sets, ", ")+" WHERE id = "+ph, args...); err != nil {
			return err
		}
	}
	if groupIDs.Set {
		ph := "$1"
		insertQ := "INSERT INTO account_group_members (account_id, group_id) VALUES ($1, $2)"
		if db.isSQLite() {
			ph = "?"
			insertQ = "INSERT INTO account_group_members (account_id, group_id) VALUES (?, ?)"
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM account_group_members WHERE account_id = "+ph, id); err != nil {
			return err
		}
		for _, gid := range normalizeIDSlice(groupIDs.Values) {
			if _, err := tx.ExecContext(ctx, insertQ, id, gid); err != nil {
				return err
			}
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

// UpdateAccountCredit 更新账号的信用设置（credit_enabled / credit_skip_usage_window）
// 传入 nil 表示不修改该字段，仅 SET 非 nil 的列。
func (db *DB) UpdateAccountCredit(ctx context.Context, id int64, creditEnabled, creditSkipUsageWindow *bool) error {
	var setClauses []string
	var args []interface{}
	argIdx := 1

	if creditEnabled != nil {
		setClauses = append(setClauses, fmt.Sprintf("credit_enabled = $%d", argIdx))
		args = append(args, *creditEnabled)
		argIdx++
	}
	if creditSkipUsageWindow != nil {
		setClauses = append(setClauses, fmt.Sprintf("credit_skip_usage_window = $%d", argIdx))
		args = append(args, *creditSkipUsageWindow)
		argIdx++
	}

	if len(setClauses) == 0 {
		return nil // 没有要更新的字段
	}

	setClauses = append(setClauses, "updated_at = CURRENT_TIMESTAMP")
	query := "UPDATE accounts SET " + strings.Join(setClauses, ", ") + fmt.Sprintf(" WHERE id = $%d", argIdx)
	args = append(args, id)

	res, err := db.conn.ExecContext(ctx, query, args...)
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

// UpdateCredentials 原子合并更新账号的 credentials（JSONB || 运算符，不覆盖已有字段）
// 解决并发刷新时一个进程覆盖另一个进程写入的字段的问题
func (db *DB) UpdateCredentials(ctx context.Context, id int64, credentials map[string]interface{}) error {
	if db.isSQLite() {
		return db.updateCredentialsSQLite(ctx, id, credentials)
	}
	return db.updateCredentialsReadMerge(ctx, id, credentials)
}

func (db *DB) updateCredentialsReadMerge(ctx context.Context, id int64, credentials map[string]interface{}) error {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	selectQuery := `SELECT credentials FROM accounts WHERE id = $1`
	selectQuery += ` FOR UPDATE`

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

func (db *DB) updateCredentialsSQLite(ctx context.Context, id int64, credentials map[string]interface{}) error {
	return db.withSQLiteWriteLock(ctx, func() error {
		if len(credentials) == 0 {
			return nil
		}

		args := make([]interface{}, 0, len(credentials)*2+1)
		jsonSetArgs := make([]string, 0, len(credentials)*2)
		argIdx := 1
		for key, value := range credentials {
			if !sqliteJSONSetKeySupported(key) {
				return db.updateCredentialsReadMergeSQLite(ctx, id, credentials)
			}
			valueJSON, err := json.Marshal(value)
			if err != nil {
				return fmt.Errorf("序列化 credentials 失败: %w", err)
			}
			jsonSetArgs = append(jsonSetArgs, fmt.Sprintf("$%d, json($%d)", argIdx, argIdx+1))
			args = append(args, "$."+key, string(valueJSON))
			argIdx += 2
		}
		args = append(args, id)

		query := fmt.Sprintf(
			`UPDATE accounts SET credentials = json_set(COALESCE(NULLIF(credentials, ''), '{}'), %s), updated_at = CURRENT_TIMESTAMP WHERE id = $%d`,
			strings.Join(jsonSetArgs, ", "), argIdx,
		)
		res, err := db.conn.ExecContext(ctx, query, args...)
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
	})
}

func (db *DB) updateCredentialsReadMergeSQLite(ctx context.Context, id int64, credentials map[string]interface{}) error {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var currentRaw interface{}
	if err := tx.QueryRowContext(ctx, `SELECT credentials FROM accounts WHERE id = $1`, id).Scan(&currentRaw); err != nil {
		return err
	}

	merged := mergeCredentialMaps(decodeCredentials(currentRaw), credentials)
	credJSON, err := json.Marshal(merged)
	if err != nil {
		return fmt.Errorf("序列化 credentials 失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE accounts SET credentials = $1, updated_at = CURRENT_TIMESTAMP WHERE id = $2`, credJSON, id); err != nil {
		return err
	}
	return tx.Commit()
}

func sqliteJSONSetKeySupported(key string) bool {
	if key == "" {
		return false
	}
	for _, r := range key {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			continue
		}
		return false
	}
	return true
}

func (db *DB) UpdateOpenAIResponsesAccount(ctx context.Context, id int64, name string, credentials map[string]interface{}, proxyURL string) error {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	selectQuery := `SELECT credentials FROM accounts WHERE id = $1 AND status <> 'deleted' AND COALESCE(error_message, '') <> 'deleted'`
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

	updateQuery := `UPDATE accounts SET name = $1, credentials = $2, proxy_url = $3, platform = 'openai', type = 'responses_api', updated_at = CURRENT_TIMESTAMP WHERE id = $4`
	if !db.isSQLite() {
		updateQuery = `UPDATE accounts SET name = $1, credentials = $2::jsonb, proxy_url = $3, platform = 'openai', type = 'responses_api', updated_at = CURRENT_TIMESTAMP WHERE id = $4`
	}
	res, err := tx.ExecContext(ctx, updateQuery, name, credJSON, proxyURL, id)
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
func (db *DB) UpdateUsageSnapshotFull(ctx context.Context, id int64, pct7d float64, reset7dAt time.Time, pct5h float64, reset5hAt time.Time, updatedAt7d time.Time, updatedAt5h time.Time) error {
	fields := map[string]interface{}{
		"codex_7d_used_percent":     pct7d,
		"codex_7d_reset_at":         reset7dAt.Format(time.RFC3339),
		"codex_5h_used_percent":     pct5h,
		"codex_5h_reset_at":         reset5hAt.Format(time.RFC3339),
		"codex_usage_updated_at":    updatedAt7d.Format(time.RFC3339),
		"codex_5h_usage_updated_at": updatedAt5h.Format(time.RFC3339),
	}
	return db.UpdateCredentials(ctx, id, fields)
}

// SetError 标记账号错误状态
func (db *DB) SetError(ctx context.Context, id int64, errorMsg string) error {
	return db.withSQLiteWriteLock(ctx, func() error {
		query := `UPDATE accounts SET status = 'error', error_message = $1, cooldown_reason = '', cooldown_until = NULL, updated_at = CURRENT_TIMESTAMP WHERE id = $2`
		_, err := db.conn.ExecContext(ctx, query, errorMsg, id)
		return err
	})
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
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

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
	res, err := tx.ExecContext(ctx, query, id)
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
	if _, err := tx.ExecContext(ctx, `DELETE FROM account_group_members WHERE account_id = $1`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// ListDeleted 获取回收站中的账号（被软删除、尚未彻底清除的账号）。
func (db *DB) ListDeleted(ctx context.Context) ([]*AccountRow, error) {
	query := `
		SELECT id, name, platform, type, credentials, proxy_url, status, cooldown_reason, cooldown_until, error_message, COALESCE(enabled, true), COALESCE(locked, false), COALESCE(credit_enabled, false), COALESCE(credit_skip_usage_window, false), COALESCE(skip_warm_tier, false), score_bias_override, base_concurrency_override, COALESCE(tags, '[]'), created_at, updated_at, deleted_at
		FROM accounts
		WHERE status = 'deleted' OR COALESCE(error_message, '') = 'deleted'
		ORDER BY deleted_at DESC, id DESC
	`
	rows, err := db.conn.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("查询回收站账号失败: %w", err)
	}
	defer rows.Close()

	var accounts []*AccountRow
	for rows.Next() {
		a := &AccountRow{}
		var credRaw interface{}
		var cooldownUntilRaw interface{}
		var tagsRaw interface{}
		var createdAtRaw interface{}
		var updatedAtRaw interface{}
		var deletedAtRaw interface{}
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
			&a.CreditEnabled,
			&a.CreditSkipUsageWindow,
			&a.SkipWarmTier,
			&a.ScoreBiasOverride,
			&a.BaseConcurrencyOverride,
			&tagsRaw,
			&createdAtRaw,
			&updatedAtRaw,
			&deletedAtRaw,
		); err != nil {
			return nil, fmt.Errorf("扫描回收站账号行失败: %w", err)
		}
		a.Credentials = decodeCredentials(credRaw)
		a.Tags = decodeTagsValue(tagsRaw)
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
		a.DeletedAt, err = parseDBNullTimeValue(deletedAtRaw)
		if err != nil {
			return nil, fmt.Errorf("解析 deleted_at 失败: %w", err)
		}
		accounts = append(accounts, a)
	}
	return accounts, rows.Err()
}

// RestoreAccount 将回收站中的账号恢复为 active 状态。
func (db *DB) RestoreAccount(ctx context.Context, id int64) error {
	query := `
		UPDATE accounts
		SET status = 'active',
			error_message = '',
			cooldown_reason = '',
			cooldown_until = NULL,
			deleted_at = NULL,
			updated_at = CURRENT_TIMESTAMP
		WHERE id = $1 AND (status = 'deleted' OR COALESCE(error_message, '') = 'deleted')
	`
	res, err := db.conn.ExecContext(ctx, query, id)
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

// PurgeAccount 从回收站彻底删除账号（物理删除，不可恢复）。
func (db *DB) PurgeAccount(ctx context.Context, id int64) error {
	query := `DELETE FROM accounts WHERE id = $1 AND (status = 'deleted' OR COALESCE(error_message, '') = 'deleted')`
	res, err := db.conn.ExecContext(ctx, query, id)
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

// PurgeDeletedAccounts 清空回收站，返回被彻底删除的账号数量。
func (db *DB) PurgeDeletedAccounts(ctx context.Context) (int64, error) {
	res, err := db.conn.ExecContext(ctx, `DELETE FROM accounts WHERE status = 'deleted' OR COALESCE(error_message, '') = 'deleted'`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
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
	return db.withSQLiteWriteLock(ctx, func() error {
		query := `UPDATE accounts SET status = 'active', error_message = '', cooldown_reason = '', cooldown_until = NULL, updated_at = CURRENT_TIMESTAMP WHERE id = $1`
		_, err := db.conn.ExecContext(ctx, query, id)
		return err
	})
}

// SetCooldown 持久化账号冷却状态
func (db *DB) SetCooldown(ctx context.Context, id int64, reason string, until time.Time) error {
	return db.withSQLiteWriteLock(ctx, func() error {
		query := `UPDATE accounts SET cooldown_reason = $1, cooldown_until = $2, updated_at = CURRENT_TIMESTAMP WHERE id = $3`
		_, err := db.conn.ExecContext(ctx, query, reason, until, id)
		return err
	})
}

// SetCooldownWithError 持久化账号冷却状态，并保留本次错误详情。
func (db *DB) SetCooldownWithError(ctx context.Context, id int64, reason string, until time.Time, errorMsg string) error {
	return db.withSQLiteWriteLock(ctx, func() error {
		query := `UPDATE accounts SET cooldown_reason = $1, cooldown_until = $2, error_message = $3, updated_at = CURRENT_TIMESTAMP WHERE id = $4`
		_, err := db.conn.ExecContext(ctx, query, reason, until, errorMsg, id)
		return err
	})
}

// ClearCooldown 清除账号冷却状态
func (db *DB) ClearCooldown(ctx context.Context, id int64) error {
	return db.withSQLiteWriteLock(ctx, func() error {
		query := `UPDATE accounts SET cooldown_reason = '', cooldown_until = NULL, updated_at = CURRENT_TIMESTAMP WHERE id = $1`
		_, err := db.conn.ExecContext(ctx, query, id)
		return err
	})
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

func (db *DB) InsertOpenAIResponsesAccount(ctx context.Context, name string, credentials map[string]interface{}, proxyURL string) (int64, error) {
	if credentials == nil {
		credentials = map[string]interface{}{}
	}
	credJSON, err := json.Marshal(credentials)
	if err != nil {
		return 0, err
	}

	return db.insertRowID(ctx,
		`INSERT INTO accounts (name, platform, type, credentials, proxy_url) VALUES ($1, 'openai', 'responses_api', $2, $3) RETURNING id`,
		`INSERT INTO accounts (name, platform, type, credentials, proxy_url) VALUES ($1, 'openai', 'responses_api', $2, $3)`,
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

// GetAllChatGPTAccountIDs 获取所有已存在的 chatgpt_account_id（用于导入去重，排除已删除账号）。
// 兼容历史字段名：account_id / chatgpt_account_id。
func (db *DB) GetAllChatGPTAccountIDs(ctx context.Context) (map[string]bool, error) {
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
		if id := strings.TrimSpace(credentialString(raw, "chatgpt_account_id")); id != "" {
			result[id] = true
			continue
		}
		if id := strings.TrimSpace(credentialString(raw, "account_id")); id != "" {
			result[id] = true
		}
	}
	return result, rows.Err()
}

func (db *DB) GetAllOpenAIAPIKeys(ctx context.Context) (map[string]bool, error) {
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
		apiKey := strings.TrimSpace(credentialString(raw, "api_key"))
		upstreamType := strings.TrimSpace(credentialString(raw, "upstream_type"))
		if apiKey != "" && upstreamType == "openai_responses" {
			result[apiKey] = true
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
