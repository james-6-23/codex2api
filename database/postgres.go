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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/codex2api/internal/openaiidentity"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

const usageStatsRollupInitTimeout = 5 * time.Minute

const grokStateBackfillInitTimeout = 5 * time.Minute

// AccountRow 数据库中的账号行
type AccountRow struct {
	ID                      int64
	CredentialGeneration    int64
	CredentialFamilyID      string
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
	Note                    string
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

type BatchAccountMetadataUpdate struct {
	Enabled                 OptionalBool
	Locked                  OptionalBool
	ScoreBiasOverride       OptionalNullInt64
	BaseConcurrencyOverride OptionalNullInt64
	SkipWarmTier            OptionalBool
	AllowedAPIKeyIDs        OptionalInt64Slice
	Tags                    OptionalStringSlice
	GroupIDs                OptionalInt64Slice
	ProxyURL                OptionalString
	CredentialUpdates       map[string]interface{}
}

func (u BatchAccountMetadataUpdate) HasChanges() bool {
	return u.Enabled.Set ||
		u.Locked.Set ||
		u.ScoreBiasOverride.Set ||
		u.BaseConcurrencyOverride.Set ||
		u.SkipWarmTier.Set ||
		u.AllowedAPIKeyIDs.Set ||
		u.Tags.Set ||
		u.GroupIDs.Set ||
		u.ProxyURL.Set ||
		len(u.CredentialUpdates) > 0
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

func (a *AccountRow) GetCredentialInt64(key string) (int64, bool) {
	if a.Credentials == nil {
		return 0, false
	}
	value, ok := a.Credentials[key]
	if !ok || value == nil {
		return 0, false
	}
	switch typed := value.(type) {
	case int64:
		return typed, true
	case int:
		return int64(typed), true
	case float64:
		if math.Trunc(typed) != typed {
			return 0, false
		}
		return int64(typed), true
	case json.Number:
		parsed, err := typed.Int64()
		return parsed, err == nil
	case string:
		parsed, err := strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
		return parsed, err == nil
	default:
		return 0, false
	}
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

	promptFilterAudit *promptFilterAuditQueue

	backgroundTaskMu      sync.Mutex
	backgroundTaskWg      sync.WaitGroup
	backgroundTaskDrain   sync.Once
	backgroundTaskDrainOK bool
	backgroundTaskClosing bool
	backgroundTaskCtx     context.Context
	backgroundTaskCancel  context.CancelFunc

	// 使用日志批量写入缓冲
	logBuf  []usageLogEntry
	logMu   sync.Mutex
	logStop chan struct{}
	logWg   sync.WaitGroup

	// 缓冲溢出/脏数据丢弃的累计条数，暴露在运行状态里供运维观察
	usageLogDropped   int64
	usageLogDropLogAt time.Time // 溢出日志的限流时间戳，由 logMu 保护

	usageLogMode          atomic.Value // string: full|errors|off
	usageLogBatchSize     int64
	usageLogFlushInterval int64 // ns
	logFlushNotify        chan struct{}
	accountInsertMu       sync.Mutex
	sqliteWriteSem        chan struct{}
	sqliteSingleConn      bool

	// 配了 scope 累计额度的 API Key 集合（issue #439 v2）。落库热路径靠它跳过
	// 绝大多数 Key，60s 刷新一次；管理端保存后会主动失效。
	scopeQuotaMu        sync.Mutex
	scopeQuotaKeys      map[int64]struct{}
	scopeQuotaExpiresAt time.Time
}

const (
	UsageLogModeFull   = "full"
	UsageLogModeErrors = "errors"
	UsageLogModeOff    = "off"

	defaultUsageLogMode                 = UsageLogModeFull
	defaultUsageLogBatchSize            = 200
	defaultUsageLogFlushIntervalSeconds = 5
	minUsageLogBatchSize                = 1
	maxUsageLogBatchSize                = 1000
	minUsageLogFlushIntervalSeconds     = 1
	maxUsageLogFlushIntervalSeconds     = 300

	postgresMaxBindParams       = 65535
	usageLogInsertColumnCount   = 52
	maxUsageLogInsertRowsPerSQL = 1000

	// usageLogBufferHardLimit 内存缓冲的硬上限。PG 长时间不可用时（维护、主从切换、
	// 磁盘写满）失败批次会一直被放回缓冲区，没有上限的话内存一路涨到 OOM——那会把
	// 整个网关拖死，比丢日志严重得多。超限时丢最旧的，丢弃条数计入运行状态。
	usageLogBufferHardLimit = 20000
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
	StoreUsageLog          bool
	AccountID              int64
	CredentialGeneration   int64
	Channel                string
	ClientIP               string
	ClientUserAgent        string
	UpstreamUserAgent      string
	UserAgentOverridden    bool
	InternalReason         string
	ParentRequestID        string
	Endpoint               string
	Model                  string
	EffectiveModel         string
	PromptTokens           int
	CompletionTokens       int
	TotalTokens            int
	StatusCode             int
	DurationMs             int
	InputTokens            int
	OutputTokens           int
	ReasoningTokens        int
	FirstTokenMs           int
	WsAcquireMs            int
	ReasoningEffort        string
	InboundEndpoint        string
	UpstreamEndpoint       string
	Stream                 bool
	Compact                bool
	HasCompactionHistory   bool
	ViaWebsocket           bool
	CachedTokens           int
	CacheWrite5mTokens     int
	CacheWrite1hTokens     int
	ServiceTier            string
	RequestedServiceTier   string
	ActualServiceTier      string
	BillingServiceTier     string
	APIKeyID               int64
	APIKeyName             string
	APIKeyMasked           string
	ImageCount             int
	ImageWidth             int
	ImageHeight            int
	ImageBytes             int
	ImageFormat            string
	ImageSize              string
	AccountBilled          float64
	UserBilled             float64
	IsRetryAttempt         bool
	AttemptIndex           int
	UpstreamErrorKind      string
	ErrorMessage           string
	PromptPolicyIncidentID string
}

// New 创建数据库连接并自动建表。
// schema 仅对 PostgreSQL 生效；为空时保持数据库默认 search_path。
func New(driver string, dsn string, schema ...string) (*DB, error) {
	driver = normalizeDriver(driver)
	driverName := sqlOpenDriverName(driver)
	// 测试注册了 schema 模板且目标文件尚不存在时，直接复制模板并跳过迁移。
	fromSchemaTemplate := false
	if driver == "sqlite" {
		if target, ok := sqliteSchemaTemplateTarget(dsn); ok {
			fromSchemaTemplate = applySQLiteSchemaTemplate(target)
		}
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

	backgroundTaskCtx, backgroundTaskCancel := context.WithCancel(context.Background())
	db := &DB{
		conn:                 conn,
		driver:               driver,
		logStop:              make(chan struct{}),
		logFlushNotify:       make(chan struct{}, 1),
		sqliteSingleConn:     sqliteSingleConn,
		backgroundTaskCtx:    backgroundTaskCtx,
		backgroundTaskCancel: backgroundTaskCancel,
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
			quoted := quotePostgresIdent(pgSchema)
			if _, err := conn.ExecContext(ctx, "CREATE SCHEMA IF NOT EXISTS "+quoted); err != nil {
				return nil, fmt.Errorf("创建数据库 schema 失败: %w", err)
			}
			if _, err := conn.ExecContext(ctx, "SET search_path TO "+quoted+", public"); err != nil {
				return nil, fmt.Errorf("设置 search_path 失败: %w", err)
			}
		}
	}
	// 模板复制出来的库已经包含下面全部 schema（模板本身就是走完整路径建出来的），
	// 跳过迁移与各 ensure*：在 -race 下这些幂等 DDL 每次仍要几百毫秒。
	if !fromSchemaTemplate {
		if err := db.migrate(ctx); err != nil {
			return nil, fmt.Errorf("数据库迁移失败: %w", err)
		}
		grokStateCtx, grokStateCancel := grokStateStartupContext(ctx)
		grokStateErr := db.ensureGrokStateSchema(grokStateCtx)
		grokStateCancel()
		if grokStateErr != nil {
			return nil, fmt.Errorf("初始化 Grok 状态表失败: %w", grokStateErr)
		}
		// The detached Grok backfill may legitimately consume minutes. Do not reuse
		// the original ten-second startup context for the remaining small schemas.
		postGrokCtx, postGrokCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer postGrokCancel()
		ctx = postGrokCtx
		if err := db.ensurePromptFilterNewAPIBindingsTable(ctx); err != nil {
			return nil, fmt.Errorf("创建 NewAPI 平台绑定表失败: %w", err)
		}
		if err := db.ensurePromptRuleCandidatesTable(ctx); err != nil {
			return nil, fmt.Errorf("创建提示词规则候选表失败: %w", err)
		}
		if err := db.ensurePromptPolicyIncidentsTable(ctx); err != nil {
			return nil, fmt.Errorf("创建提示词策略事件表失败: %w", err)
		}
		if err := db.ensurePromptConversationLocksTable(ctx); err != nil {
			return nil, fmt.Errorf("创建提示词会话锁表失败: %w", err)
		}
	}
	if err := db.ensureProxyRiskScoringTables(ctx); err != nil {
		return nil, fmt.Errorf("创建代理风险评分表失败: %w", err)
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
	rollupCtx, rollupCancel := usageStatsRollupStartupContext(ctx)
	rollupErr := db.ensureUsageStatsRollup(rollupCtx)
	rollupCancel()
	if rollupErr != nil {
		return nil, fmt.Errorf("初始化用量累计汇总失败: %w", rollupErr)
	}
	db.promptFilterAudit = newPromptFilterAuditQueue(db)
	db.promptFilterAudit.start()
	db.RunBackgroundTask(func(taskCtx context.Context) {
		if err := db.backfillPromptRiskEvents(taskCtx); err != nil && !errors.Is(err, context.Canceled) {
			log.Printf("回填提示词风险画像失败，将在下次启动继续: %v", err)
		}
	})
	if !db.isSQLite() {
		db.RunBackgroundTask(func(taskCtx context.Context) {
			if err := db.ensureUsageLogsGenerationIndex(taskCtx); err != nil && !errors.Is(err, context.Canceled) {
				log.Printf("在线创建 usage_logs 代际索引失败（仅影响查询性能，将在下次启动重试）: %v", err)
			}
		})
	}

	return db, nil
}

// ensureUsageLogsGenerationIndex 在启动后台在线构建 usage_logs 的代际聚合索引。
// usage_logs 是有真实流量部署的最大表，普通 CREATE INDEX 会持 SHARE 锁阻塞全部
// 日志写入，放进 10 秒启动事务还会超时回滚形成崩溃循环，因此这里独立于启动
// 路径、用 CONCURRENTLY 构建（不阻塞写入），失败只降级查询性能，不影响服务。
// CONCURRENTLY 构建被中断会留下 INVALID 索引且使 IF NOT EXISTS 误判存在，
// 所以先探测有效性，无效则先删再建。
func (db *DB) ensureUsageLogsGenerationIndex(parent context.Context) error {
	const indexName = "idx_usage_logs_account_generation_created_at"
	ctx, cancel := context.WithTimeout(parent, 60*time.Minute)
	defer cancel()

	var exists, valid bool
	err := db.conn.QueryRowContext(ctx, `
		SELECT to_regclass($1) IS NOT NULL,
		       COALESCE((SELECT i.indisvalid FROM pg_index i WHERE i.indexrelid = to_regclass($1)), FALSE)
	`, indexName).Scan(&exists, &valid)
	if err != nil {
		return fmt.Errorf("探测索引 %s 状态失败: %w", indexName, err)
	}
	if exists && valid {
		return nil
	}
	if exists && !valid {
		if _, err := db.conn.ExecContext(ctx, `DROP INDEX `+quotePostgresIdent(indexName)); err != nil {
			return fmt.Errorf("清理无效索引 %s 失败: %w", indexName, err)
		}
	}
	// CONCURRENTLY 不能在事务块内执行；单条 ExecContext 走 autocommit，满足要求。
	if _, err := db.conn.ExecContext(ctx, `CREATE INDEX CONCURRENTLY IF NOT EXISTS `+quotePostgresIdent(indexName)+` ON usage_logs(account_id, credential_generation, created_at)`); err != nil {
		return fmt.Errorf("在线创建索引 %s 失败: %w", indexName, err)
	}
	log.Printf("usage_logs 代际索引 %s 已就绪", indexName)
	return nil
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

type usageStatsRollup struct {
	TotalRequests      int64
	TotalTokens        int64
	PromptTokens       int64
	CompletionTokens   int64
	CachedTokens       int64
	CacheHitRequests   int64
	FirstTokenMsSum    float64
	FirstTokenSamples  int64
	TotalAccountBilled float64
	TotalUserBilled    float64
}

// usageStatsRollupStartupContext preserves startup values but deliberately
// detaches the one-time full-history rollup from the shared ten-second schema
// initialization deadline. Large existing installations can contain millions
// of usage rows; the bounded five-minute deadline keeps the migration finite
// without weakening normal request or database-operation timeouts.
func usageStatsRollupStartupContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), usageStatsRollupInitTimeout)
}

// grokStateStartupContext detaches the one-time historical account backfill
// from the shared ten-second schema deadline while preserving startup values.
// A bounded deadline still prevents a stuck migration from hanging forever.
func grokStateStartupContext(parent context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(parent), grokStateBackfillInitTimeout)
}

func (db *DB) ensureUsageStatsRollup(ctx context.Context) error {
	if _, err := db.conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS usage_stats_rollup (
		channel VARCHAR(32) PRIMARY KEY,
		total_requests BIGINT NOT NULL DEFAULT 0,
		total_tokens BIGINT NOT NULL DEFAULT 0,
		prompt_tokens BIGINT NOT NULL DEFAULT 0,
		completion_tokens BIGINT NOT NULL DEFAULT 0,
		cached_tokens BIGINT NOT NULL DEFAULT 0,
		cache_hit_requests BIGINT NOT NULL DEFAULT 0,
		first_token_ms_sum DOUBLE PRECISION NOT NULL DEFAULT 0,
		first_token_samples BIGINT NOT NULL DEFAULT 0,
		account_billed DOUBLE PRECISION NOT NULL DEFAULT 0,
		user_billed DOUBLE PRECISION NOT NULL DEFAULT 0
	);
	CREATE TABLE IF NOT EXISTS usage_stats_rollup_state (
		id INTEGER PRIMARY KEY,
		initialized INTEGER NOT NULL DEFAULT 0,
		last_log_id BIGINT NOT NULL DEFAULT 0,
		updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		return err
	}
	if db.isSQLite() {
		columns, err := db.sqliteTableColumns(ctx, "usage_stats_rollup_state")
		if err != nil {
			return err
		}
		if _, ok := columns["aggregation_version"]; !ok {
			if _, err := db.conn.ExecContext(ctx, `ALTER TABLE usage_stats_rollup_state ADD COLUMN aggregation_version INTEGER NOT NULL DEFAULT 1`); err != nil {
				return err
			}
		}
	} else if _, err := db.conn.ExecContext(ctx, `ALTER TABLE usage_stats_rollup_state ADD COLUMN IF NOT EXISTS aggregation_version INTEGER NOT NULL DEFAULT 1`); err != nil {
		return err
	}
	var initialized, aggregationVersion int
	err := db.conn.QueryRowContext(ctx, `SELECT initialized, aggregation_version FROM usage_stats_rollup_state WHERE id=1`).Scan(&initialized, &aggregationVersion)
	if err == nil && initialized == 1 && aggregationVersion >= 2 {
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	return db.rebuildUsageStatsRollup(ctx)
}

func (db *DB) rebuildUsageStatsRollup(ctx context.Context) error {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM usage_stats_rollup`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO usage_stats_rollup (
		channel, total_requests, total_tokens, prompt_tokens, completion_tokens, cached_tokens,
		cache_hit_requests, first_token_ms_sum, first_token_samples, account_billed, user_billed
	) SELECT '', b.total_requests + COUNT(u.id), b.total_tokens + COALESCE(SUM(u.total_tokens), 0),
		b.prompt_tokens + COALESCE(SUM(u.prompt_tokens), 0), b.completion_tokens + COALESCE(SUM(u.completion_tokens), 0),
		b.cached_tokens + COALESCE(SUM(u.cached_tokens), 0),
		b.cache_hit_requests + COALESCE(SUM(CASE WHEN u.cached_tokens > 0 THEN 1 ELSE 0 END), 0),
		b.first_token_ms_sum + COALESCE(SUM(CASE WHEN u.first_token_ms > 0 THEN u.first_token_ms ELSE 0 END), 0),
		b.first_token_samples + COALESCE(SUM(CASE WHEN u.first_token_ms > 0 THEN 1 ELSE 0 END), 0),
		b.account_billed + COALESCE(SUM(u.account_billed), 0), b.user_billed + COALESCE(SUM(u.user_billed), 0)
	FROM usage_stats_baseline b LEFT JOIN usage_logs u ON u.status_code <> 499
		AND TRIM(COALESCE(u.internal_reason, '')) = '' WHERE b.id=1
	GROUP BY b.total_requests, b.total_tokens, b.prompt_tokens, b.completion_tokens, b.cached_tokens,
		b.cache_hit_requests, b.first_token_ms_sum, b.first_token_samples, b.account_billed, b.user_billed`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO usage_stats_rollup (
		channel, total_requests, total_tokens, prompt_tokens, completion_tokens, cached_tokens,
		cache_hit_requests, first_token_ms_sum, first_token_samples, account_billed, user_billed
	) SELECT TRIM(COALESCE(channel, '')), COUNT(*), COALESCE(SUM(total_tokens), 0), COALESCE(SUM(prompt_tokens), 0),
		COALESCE(SUM(completion_tokens), 0), COALESCE(SUM(cached_tokens), 0),
		COALESCE(SUM(CASE WHEN cached_tokens > 0 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN first_token_ms > 0 THEN first_token_ms ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN first_token_ms > 0 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(account_billed), 0), COALESCE(SUM(user_billed), 0)
	FROM usage_logs WHERE status_code <> 499 AND TRIM(COALESCE(internal_reason, '')) = ''
		AND TRIM(COALESCE(channel, '')) <> '' GROUP BY TRIM(COALESCE(channel, ''))`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO usage_stats_rollup_state (id, initialized, last_log_id, aggregation_version, updated_at)
		VALUES (1, 1, COALESCE((SELECT MAX(id) FROM usage_logs), 0), 2, CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET initialized=1, last_log_id=excluded.last_log_id,
			aggregation_version=excluded.aggregation_version, updated_at=CURRENT_TIMESTAMP`); err != nil {
		return err
	}
	return tx.Commit()
}

func (db *DB) loadUsageStatsRollup(ctx context.Context, channel string) (usageStatsRollup, error) {
	var result usageStatsRollup
	var lastLogID, currentMaxID int64
	if err := db.conn.QueryRowContext(ctx, `SELECT last_log_id FROM usage_stats_rollup_state WHERE id=1 AND initialized=1`).Scan(&lastLogID); err != nil {
		if rebuildErr := db.rebuildUsageStatsRollup(ctx); rebuildErr != nil {
			return result, rebuildErr
		}
		if err := db.conn.QueryRowContext(ctx, `SELECT last_log_id FROM usage_stats_rollup_state WHERE id=1`).Scan(&lastLogID); err != nil {
			return result, err
		}
	}
	if err := db.conn.QueryRowContext(ctx, `SELECT COALESCE(MAX(id), 0) FROM usage_logs`).Scan(&currentMaxID); err != nil {
		return result, err
	}
	if currentMaxID != lastLogID {
		if err := db.rebuildUsageStatsRollup(ctx); err != nil {
			return result, err
		}
	}
	err := db.conn.QueryRowContext(ctx, `SELECT total_requests, total_tokens, prompt_tokens, completion_tokens,
		cached_tokens, cache_hit_requests, first_token_ms_sum, first_token_samples, account_billed, user_billed
		FROM usage_stats_rollup WHERE channel=$1`, strings.TrimSpace(channel)).Scan(
		&result.TotalRequests, &result.TotalTokens, &result.PromptTokens, &result.CompletionTokens,
		&result.CachedTokens, &result.CacheHitRequests, &result.FirstTokenMsSum, &result.FirstTokenSamples,
		&result.TotalAccountBilled, &result.TotalUserBilled)
	if errors.Is(err, sql.ErrNoRows) {
		return usageStatsRollup{}, nil
	}
	return result, err
}

func applyUsageStatsRollupWithExec(ctx context.Context, execer sqlExecer, batch []usageLogEntry) error {
	if execer == nil || len(batch) == 0 {
		return nil
	}
	rollups := make(map[string]*usageStatsRollup)
	add := func(channel string, entry usageLogEntry) {
		item := rollups[channel]
		if item == nil {
			item = &usageStatsRollup{}
			rollups[channel] = item
		}
		item.TotalRequests++
		item.TotalTokens += int64(entry.TotalTokens)
		item.PromptTokens += int64(entry.PromptTokens)
		item.CompletionTokens += int64(entry.CompletionTokens)
		item.CachedTokens += int64(entry.CachedTokens)
		if entry.CachedTokens > 0 {
			item.CacheHitRequests++
		}
		if entry.FirstTokenMs > 0 {
			item.FirstTokenMsSum += float64(entry.FirstTokenMs)
			item.FirstTokenSamples++
		}
		item.TotalAccountBilled += entry.AccountBilled
		item.TotalUserBilled += entry.UserBilled
	}
	for _, entry := range batch {
		if !entry.StoreUsageLog || entry.StatusCode == 499 || strings.TrimSpace(entry.InternalReason) != "" {
			continue
		}
		add("", entry)
		if channel := strings.TrimSpace(entry.Channel); channel != "" {
			add(channel, entry)
		}
	}
	for channel, item := range rollups {
		if _, err := execer.ExecContext(ctx, `INSERT INTO usage_stats_rollup (
			channel, total_requests, total_tokens, prompt_tokens, completion_tokens, cached_tokens,
			cache_hit_requests, first_token_ms_sum, first_token_samples, account_billed, user_billed
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT(channel) DO UPDATE SET
			total_requests=usage_stats_rollup.total_requests+excluded.total_requests,
			total_tokens=usage_stats_rollup.total_tokens+excluded.total_tokens,
			prompt_tokens=usage_stats_rollup.prompt_tokens+excluded.prompt_tokens,
			completion_tokens=usage_stats_rollup.completion_tokens+excluded.completion_tokens,
			cached_tokens=usage_stats_rollup.cached_tokens+excluded.cached_tokens,
			cache_hit_requests=usage_stats_rollup.cache_hit_requests+excluded.cache_hit_requests,
			first_token_ms_sum=usage_stats_rollup.first_token_ms_sum+excluded.first_token_ms_sum,
			first_token_samples=usage_stats_rollup.first_token_samples+excluded.first_token_samples,
			account_billed=usage_stats_rollup.account_billed+excluded.account_billed,
			user_billed=usage_stats_rollup.user_billed+excluded.user_billed`, channel, item.TotalRequests,
			item.TotalTokens, item.PromptTokens, item.CompletionTokens, item.CachedTokens, item.CacheHitRequests,
			item.FirstTokenMsSum, item.FirstTokenSamples, item.TotalAccountBilled, item.TotalUserBilled); err != nil {
			return err
		}
	}
	_, err := execer.ExecContext(ctx, `UPDATE usage_stats_rollup_state SET initialized=1,
		last_log_id=COALESCE((SELECT MAX(id) FROM usage_logs), 0), updated_at=CURRENT_TIMESTAMP WHERE id=1`)
	return err
}

// Close 关闭数据库连接
func (db *DB) Close() error {
	if !db.DrainBackgroundTasks(2 * time.Second) {
		log.Printf("数据库后台任务超过优雅关闭窗口，已取消并等待退出")
	}
	// 停止批量写入并刷完缓冲。这里必须用 FlushUsageLogs 而不是 flushLogs：
	// 后者每次只取 usage_log_batch_size 条，剩余部分靠 notifyLogFlush 让后台协程接着刷，
	// 而此刻 flusher 已经退出，没人消费这个信号，超出一个批次的日志会被静默丢弃。
	close(db.logStop)
	db.logWg.Wait()
	db.FlushUsageLogs() // 最后一次 flush，刷完整个缓冲
	if db.promptFilterAudit != nil {
		db.promptFilterAudit.close(2 * time.Second)
	}
	return db.conn.Close()
}

// RunBackgroundTask starts a best-effort task whose lifetime is tied to the
// database. Close first drains these tasks, then cancels and waits for any task
// that exceeds the grace period, so no background writer can outlive the SQL
// connection.
func (db *DB) RunBackgroundTask(task func(context.Context)) bool {
	if db == nil || task == nil {
		return false
	}

	db.backgroundTaskMu.Lock()
	if db.backgroundTaskClosing {
		db.backgroundTaskMu.Unlock()
		return false
	}
	if db.backgroundTaskCtx == nil {
		db.backgroundTaskCtx, db.backgroundTaskCancel = context.WithCancel(context.Background())
	}
	db.backgroundTaskWg.Add(1)
	ctx := db.backgroundTaskCtx
	db.backgroundTaskMu.Unlock()

	go func() {
		defer db.backgroundTaskWg.Done()
		task(ctx)
	}()
	return true
}

// DrainBackgroundTasks stops accepting detached database work, waits for the
// current tasks to finish, and cancels them after the grace period. After
// cancellation it still waits for every tracked task to exit, preserving the
// invariant that no task can access SQL after Close returns. It is safe to call
// before dependent services are torn down; Close calls it again as a final
// safeguard. The return value reports whether cancellation was unnecessary.
func (db *DB) DrainBackgroundTasks(timeout time.Duration) bool {
	if db == nil {
		return true
	}
	db.backgroundTaskDrain.Do(func() {
		db.backgroundTaskDrainOK = db.drainBackgroundTasks(timeout)
	})
	return db.backgroundTaskDrainOK
}

func (db *DB) drainBackgroundTasks(timeout time.Duration) bool {
	db.backgroundTaskMu.Lock()
	db.backgroundTaskClosing = true
	db.backgroundTaskMu.Unlock()

	done := make(chan struct{})
	go func() {
		db.backgroundTaskWg.Wait()
		close(done)
	}()

	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		if db.backgroundTaskCancel != nil {
			db.backgroundTaskCancel()
		}
		return true
	case <-timer.C:
		if db.backgroundTaskCancel != nil {
			db.backgroundTaskCancel()
		}
	}

	<-done
	return false
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
	// BufferLimit 内存缓冲硬上限，DroppedTotal 是启动以来因溢出或脏数据丢掉的日志条数。
	BufferLimit  int
	DroppedTotal int64
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
	stats.BufferLimit = usageLogBufferHardLimit
	stats.DroppedTotal = atomic.LoadInt64(&db.usageLogDropped)

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

	CREATE TABLE IF NOT EXISTS scheduler_outbox (
		id          BIGSERIAL PRIMARY KEY,
		entity_type VARCHAR(32) NOT NULL,
		entity_id   BIGINT NOT NULL DEFAULT 0,
		event_type  VARCHAR(32) NOT NULL,
		created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
	);
	CREATE INDEX IF NOT EXISTS idx_scheduler_outbox_created ON scheduler_outbox(created_at, id);
	CREATE TABLE IF NOT EXISTS maintenance_jobs (
		entity_id BIGINT NOT NULL,
		job_kind VARCHAR(64) NOT NULL,
		due_at TIMESTAMPTZ NOT NULL,
		lease_owner TEXT NOT NULL DEFAULT '',
		lease_until TIMESTAMPTZ NULL,
		attempts INT NOT NULL DEFAULT 0,
		last_error TEXT NOT NULL DEFAULT '',
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		PRIMARY KEY(entity_id, job_kind)
	);
	CREATE INDEX IF NOT EXISTS idx_maintenance_jobs_due ON maintenance_jobs(job_kind, due_at, entity_id);

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
	ALTER TABLE accounts ADD COLUMN IF NOT EXISTS note TEXT DEFAULT '';
	ALTER TABLE accounts ADD COLUMN IF NOT EXISTS credential_generation BIGINT NOT NULL DEFAULT 1;

	CREATE TABLE IF NOT EXISTS account_groups (
		id                        SERIAL PRIMARY KEY,
		name                      VARCHAR(80) UNIQUE NOT NULL,
		description               TEXT DEFAULT '',
		color                     VARCHAR(20) DEFAULT '',
		sort_order                INT DEFAULT 0,
		base_concurrency_override INT NULL,
		proxy_urls                TEXT DEFAULT '[]',
		created_at                TIMESTAMPTZ DEFAULT NOW(),
		updated_at                TIMESTAMPTZ DEFAULT NOW()
	);
	ALTER TABLE account_groups ADD COLUMN IF NOT EXISTS description TEXT DEFAULT '';
	ALTER TABLE account_groups ADD COLUMN IF NOT EXISTS color VARCHAR(20) DEFAULT '';
	ALTER TABLE account_groups ADD COLUMN IF NOT EXISTS sort_order INT DEFAULT 0;
	ALTER TABLE account_groups ADD COLUMN IF NOT EXISTS base_concurrency_override INT NULL;
	ALTER TABLE account_groups ADD COLUMN IF NOT EXISTS proxy_urls TEXT DEFAULT '[]';
	ALTER TABLE account_groups ADD COLUMN IF NOT EXISTS channel VARCHAR(16) DEFAULT 'codex';
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
	CREATE INDEX IF NOT EXISTS idx_accounts_upstream_type_id ON accounts ((LOWER(COALESCE(credentials->>'upstream_type', ''))), id);
	CREATE INDEX IF NOT EXISTS idx_accounts_active_upstream_type_id ON accounts ((LOWER(COALESCE(credentials->>'upstream_type', ''))), id)
		WHERE status <> 'deleted' AND COALESCE(error_message, '') <> 'deleted';
	CREATE INDEX IF NOT EXISTS idx_accounts_created_id ON accounts(created_at, id);
	CREATE INDEX IF NOT EXISTS idx_accounts_updated_id ON accounts(updated_at, id);


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
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS ws_acquire_ms INT DEFAULT 0;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS reasoning_effort VARCHAR(100) DEFAULT '';
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS via_websocket BOOLEAN DEFAULT FALSE;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS effective_model VARCHAR(100) DEFAULT '';
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS inbound_endpoint VARCHAR(100) DEFAULT '';
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS upstream_endpoint VARCHAR(100) DEFAULT '';
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS stream BOOLEAN DEFAULT false;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS compact BOOLEAN DEFAULT false;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS has_compaction_history BOOLEAN DEFAULT false;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS cached_tokens INT DEFAULT 0;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS cache_write_5m_tokens INT DEFAULT 0;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS cache_write_1h_tokens INT DEFAULT 0;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS service_tier VARCHAR(100) DEFAULT '';
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS requested_service_tier VARCHAR(100) DEFAULT '';
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS actual_service_tier VARCHAR(100) DEFAULT '';
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS billing_service_tier VARCHAR(100) DEFAULT '';
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS api_key_id INT DEFAULT 0;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS api_key_name VARCHAR(255) DEFAULT '';
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS api_key_masked VARCHAR(64) DEFAULT '';
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS client_ip VARCHAR(64) DEFAULT '';
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS client_user_agent TEXT DEFAULT '';
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS upstream_user_agent TEXT DEFAULT '';
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS user_agent_overridden BOOLEAN DEFAULT FALSE;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS internal_reason VARCHAR(64) DEFAULT '';
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS parent_request_id VARCHAR(128) DEFAULT '';
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS image_count INT DEFAULT 0;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS image_width INT DEFAULT 0;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS image_height INT DEFAULT 0;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS image_bytes INT DEFAULT 0;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS image_format VARCHAR(100) DEFAULT '';
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS image_size VARCHAR(32) DEFAULT '';
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS account_billed DOUBLE PRECISION DEFAULT 0;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS user_billed DOUBLE PRECISION DEFAULT 0;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS is_retry_attempt BOOLEAN DEFAULT FALSE;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS attempt_index INT DEFAULT 0;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS upstream_error_kind VARCHAR(64) DEFAULT '';
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS error_message TEXT DEFAULT '';
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS credential_generation BIGINT NOT NULL DEFAULT 0;
	-- idx_usage_logs_account_generation_created_at 不在此批次内创建：usage_logs 是
	-- 生产最大表，非 CONCURRENTLY 建索引会持锁阻塞写入，且本批次运行在 10 秒启动
	-- 超时的单个隐式事务里，大表下会超时回滚导致启动崩溃循环。改由
	-- ensureUsageLogsGenerationIndex 在启动后用 CREATE INDEX CONCURRENTLY 在线构建。
	-- 上游渠道（codex/grok），写入时按调度账号固化，供仪表盘/用量分渠道聚合
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS channel VARCHAR(16) DEFAULT '';
	ALTER TABLE usage_logs ALTER COLUMN reasoning_effort TYPE VARCHAR(100);
	ALTER TABLE usage_logs ALTER COLUMN service_tier TYPE VARCHAR(100);
	ALTER TABLE usage_logs ALTER COLUMN requested_service_tier TYPE VARCHAR(100);
	ALTER TABLE usage_logs ALTER COLUMN actual_service_tier TYPE VARCHAR(100);
	ALTER TABLE usage_logs ALTER COLUMN billing_service_tier TYPE VARCHAR(100);
	ALTER TABLE usage_logs ALTER COLUMN image_format TYPE VARCHAR(100);

	CREATE INDEX IF NOT EXISTS idx_usage_logs_api_key_created_at ON usage_logs(api_key_id, created_at);
	CREATE INDEX IF NOT EXISTS idx_usage_logs_channel_created_at ON usage_logs(channel, created_at);

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

	ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS total_used DOUBLE PRECISION NOT NULL DEFAULT 0;
	ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS reset_count INTEGER NOT NULL DEFAULT 0;
	ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS last_reset_at TIMESTAMPTZ;

	ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS allowed_group_ids JSONB DEFAULT '[]'::jsonb;
	ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS limits JSONB DEFAULT '{}'::jsonb;
	ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS enabled BOOLEAN NOT NULL DEFAULT TRUE;

			CREATE TABLE IF NOT EXISTS system_settings (
				id                 INTEGER PRIMARY KEY DEFAULT 1 CHECK (id = 1),
				site_name          TEXT DEFAULT 'CodexProxy',
				site_logo          TEXT DEFAULT '',
				background_config  TEXT DEFAULT '{}',
				grok_config        TEXT DEFAULT '{}',
				claude_config      TEXT DEFAULT '{}',
				antigravity_oauth_config TEXT DEFAULT '{}',
				invite_guide_config TEXT DEFAULT '{}',
				visible_channels_config TEXT DEFAULT '{}',
				max_concurrency    INT DEFAULT 2,
			global_rpm         INT DEFAULT 0,
			test_model         VARCHAR(100) DEFAULT 'gpt-5.4',
			test_content       TEXT DEFAULT 'hi',
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
			scheduler_mode VARCHAR(20) DEFAULT 'round_robin',
			response_cache_local_max_bytes BIGINT NOT NULL DEFAULT 67108864,
			response_cache_local_max_entry_bytes BIGINT NOT NULL DEFAULT 8388608,
			response_cache_reconstruct_max_bytes BIGINT NOT NULL DEFAULT 67108864,
			response_cache_write_policy VARCHAR(20) NOT NULL DEFAULT 'always',
			response_cache_config_generation BIGINT NOT NULL DEFAULT 1,
			models_list_read_max_bytes BIGINT NOT NULL DEFAULT 8388608
		);
	CREATE TABLE IF NOT EXISTS api_key_model_request_counters (
		api_key_id BIGINT NOT NULL,
		rule_id VARCHAR(80) NOT NULL,
		window_start BIGINT NOT NULL,
		reset_at BIGINT NOT NULL,
		used_requests BIGINT NOT NULL DEFAULT 0,
		PRIMARY KEY (api_key_id, rule_id, window_start)
	);
	CREATE TABLE IF NOT EXISTS api_key_model_request_ledger (
		api_key_id BIGINT NOT NULL,
		rule_id VARCHAR(80) NOT NULL,
		request_id VARCHAR(200) NOT NULL,
		window_start BIGINT NOT NULL,
		created_at BIGINT NOT NULL,
		PRIMARY KEY (api_key_id, rule_id, request_id)
	);
	CREATE TABLE IF NOT EXISTS api_key_scope_counters (
		api_key_id BIGINT NOT NULL,
		scope_type VARCHAR(16) NOT NULL,
		scope_id BIGINT NOT NULL,
		used_cost DOUBLE PRECISION NOT NULL DEFAULT 0,
		used_tokens BIGINT NOT NULL DEFAULT 0,
		used_requests BIGINT NOT NULL DEFAULT 0,
		reset_count INTEGER NOT NULL DEFAULT 0,
		last_reset_at TIMESTAMPTZ,
		updated_at TIMESTAMPTZ DEFAULT NOW(),
		PRIMARY KEY (api_key_id, scope_type, scope_id)
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
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS grok_config TEXT DEFAULT '{}';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS claude_config TEXT DEFAULT '{}';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS antigravity_oauth_config TEXT DEFAULT '{}';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS invite_guide_config TEXT DEFAULT '{}';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS visible_channels_config TEXT DEFAULT '{}';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS test_content TEXT DEFAULT 'hi';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS pg_max_conns INT DEFAULT 50;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS redis_pool_size INT DEFAULT 30;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS auto_clean_unauthorized BOOLEAN DEFAULT FALSE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS auto_clean_rate_limited BOOLEAN DEFAULT FALSE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS admin_secret VARCHAR(255) DEFAULT '';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS auto_clean_full_usage BOOLEAN DEFAULT FALSE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS proxy_pool_enabled BOOLEAN DEFAULT FALSE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS fast_scheduler_enabled BOOLEAN DEFAULT FALSE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS scheduler_engine VARCHAR(20) DEFAULT '';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS max_retries INT DEFAULT 2;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS allow_remote_migration BOOLEAN DEFAULT FALSE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS max_rate_limit_retries INT DEFAULT 1;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS auto_clean_error BOOLEAN DEFAULT FALSE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS auto_clean_expired BOOLEAN DEFAULT FALSE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS lazy_mode BOOLEAN DEFAULT FALSE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS model_mapping TEXT DEFAULT '{}';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS codex_model_mapping TEXT DEFAULT '{}';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS payload_rules TEXT DEFAULT '{}';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS reasoning_effort_models TEXT DEFAULT '[]';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS background_refresh_interval_minutes INT DEFAULT 2;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS usage_probe_max_age_minutes INT DEFAULT 10;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS usage_probe_concurrency INT DEFAULT 16;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS usage_probe_responses_fallback_enabled BOOLEAN DEFAULT TRUE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS recovery_probe_interval_minutes INT DEFAULT 30;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS scheduler_mode VARCHAR(20) DEFAULT 'round_robin';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS affinity_mode VARCHAR(16) DEFAULT 'bounded';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS session_affinity_spread BOOLEAN DEFAULT FALSE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS session_slot_buffer_enabled BOOLEAN DEFAULT FALSE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS session_slot_buffer_seconds INT DEFAULT 10;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS models_list_read_max_bytes BIGINT NOT NULL DEFAULT 8388608;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS resin_url TEXT DEFAULT '';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS resin_platform_name TEXT DEFAULT '';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS prompt_filter_enabled BOOLEAN DEFAULT FALSE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS prompt_filter_mode VARCHAR(20) DEFAULT 'monitor';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS prompt_filter_threshold INT DEFAULT 50;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS prompt_filter_strict_threshold INT DEFAULT 90;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS prompt_filter_strict_terminal_enabled BOOLEAN DEFAULT FALSE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS prompt_filter_advanced_config TEXT DEFAULT '{}';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS prompt_filter_log_matches BOOLEAN DEFAULT TRUE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS prompt_filter_max_text_length INT DEFAULT 81920;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS prompt_filter_sensitive_words TEXT DEFAULT '';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS prompt_filter_custom_patterns TEXT DEFAULT '[]';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS prompt_filter_disabled_patterns TEXT DEFAULT '[]';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS prompt_filter_review_enabled BOOLEAN DEFAULT FALSE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS prompt_filter_review_api_key TEXT DEFAULT '';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS prompt_filter_review_base_url TEXT DEFAULT 'https://api.deepseek.com';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS prompt_filter_review_model TEXT DEFAULT 'deepseek-v4-flash';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS prompt_filter_review_timeout_seconds INT DEFAULT 10;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS prompt_filter_review_fail_closed BOOLEAN DEFAULT TRUE;
	-- 审查服务从未配置过(无 key、未启用)且仍是旧出厂默认时,迁移到新的
	-- DeepSeek 默认供应商;真在用 OpenAI 审核的部署不受影响。
	UPDATE system_settings
	SET prompt_filter_review_base_url = 'https://api.deepseek.com',
		prompt_filter_review_model = 'deepseek-v4-flash'
	WHERE COALESCE(prompt_filter_review_api_key, '') = ''
	  AND COALESCE(prompt_filter_review_enabled, FALSE) = FALSE
	  AND COALESCE(prompt_filter_review_base_url, '') = 'https://api.openai.com'
	  AND COALESCE(prompt_filter_review_model, '') = 'omni-moderation-latest';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS client_compat_mode VARCHAR(20) DEFAULT 'preserve';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS codex_min_cli_version VARCHAR(32) DEFAULT '0.153.3';
	ALTER TABLE system_settings ALTER COLUMN codex_min_cli_version SET DEFAULT '0.153.3';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS codex_user_agent_config TEXT DEFAULT '{}';
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
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS public_key_usage_page_enabled BOOLEAN DEFAULT TRUE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS public_image_studio_page_enabled BOOLEAN DEFAULT TRUE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS public_account_portal_page_enabled BOOLEAN DEFAULT FALSE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS codex_force_websocket BOOLEAN DEFAULT FALSE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS codex_request_compression BOOLEAN DEFAULT TRUE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS codex_ws_weak_network_mode BOOLEAN DEFAULT FALSE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS codex_ws_keepalive_enabled BOOLEAN DEFAULT FALSE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS codex_ws_keepalive_interval_sec INT DEFAULT 60;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS codex_ws_hide_upstream_errors BOOLEAN DEFAULT TRUE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS codex_ws_silent_retry_enabled BOOLEAN DEFAULT TRUE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS codex_ws_size_router_enabled BOOLEAN DEFAULT TRUE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS codex_ws_busy_acquire_max_wait_sec INT DEFAULT 30;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS codex_ws_busy_overflow_enabled BOOLEAN DEFAULT FALSE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS codex_ws_busy_patience_sec INT DEFAULT 2;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS codex_ws_stateless_slots INT DEFAULT 8;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS github_token TEXT DEFAULT '';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS github_proxy_url TEXT DEFAULT '';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS codex_overload_pause_enabled BOOLEAN DEFAULT FALSE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS codex_overload_threshold_percent INT DEFAULT 20;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS codex_overload_pause_minutes INT DEFAULT 30;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS codex_overload_window_minutes INT DEFAULT 5;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS overflow_auto_compact_enabled BOOLEAN DEFAULT FALSE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS compact_via_responses_enabled BOOLEAN DEFAULT FALSE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS codex_preflight_sse_passthrough_enabled BOOLEAN DEFAULT FALSE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS first_token_excludes_ws_acquire BOOLEAN DEFAULT FALSE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS codex_ws_silent_max_retries INT DEFAULT 2;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS codex_continue_thinking_enabled BOOLEAN DEFAULT FALSE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS codex_continue_max_rounds INT DEFAULT 8;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS codex_synced_cli_version TEXT DEFAULT '';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS codex_cli_version_sync_enabled BOOLEAN DEFAULT TRUE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS codex_cli_version_sync_interval_hours INT DEFAULT 12;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS claude_synced_cli_version TEXT DEFAULT '';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS model_pricing_overrides TEXT DEFAULT '{}';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS model_pricing_sync_url TEXT DEFAULT '';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS auto_pause_5h_threshold DOUBLE PRECISION DEFAULT 0;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS auto_pause_7d_threshold DOUBLE PRECISION DEFAULT 0;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS auto_pause_5h_guard_band_percent DOUBLE PRECISION DEFAULT 5;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS auto_pause_5h_guard_concurrency INT DEFAULT 1;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS smart_pacing_enabled BOOLEAN DEFAULT FALSE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS smart_pacing_min_concurrency INT DEFAULT 1;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS smart_pacing_windows TEXT DEFAULT '5h,7d';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS retry_interval_ms INT DEFAULT 0;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS transport_retry_policy VARCHAR(20) DEFAULT 'rotate';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS continuous_retry_policy TEXT DEFAULT '{"enabled":false,"catch_all":false,"categories":["transport","http_429","http_5xx","stream_error"],"status_codes":[],"error_codes":[],"max_duration_seconds":600}';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS ignore_usage_limit_status BOOLEAN DEFAULT FALSE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS auto_reset_credits_enabled BOOLEAN DEFAULT FALSE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS auto_reset_credits_before_expiry_min INT DEFAULT 60;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS auto_activate_5h_window_enabled BOOLEAN DEFAULT FALSE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS utls_shutdown_timeout_minutes INT DEFAULT 30;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS codex_fingerprint_default_mode VARCHAR(20) DEFAULT 'off';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS response_cache_local_max_bytes BIGINT NOT NULL DEFAULT 67108864;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS response_cache_local_max_entry_bytes BIGINT NOT NULL DEFAULT 8388608;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS response_cache_reconstruct_max_bytes BIGINT NOT NULL DEFAULT 67108864;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS response_cache_write_policy VARCHAR(20) NOT NULL DEFAULT 'always';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS response_cache_config_generation BIGINT NOT NULL DEFAULT 1;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS relay_model_cooldown_mode VARCHAR(20) NOT NULL DEFAULT 'off';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS relay_model_cooldown_seconds INT NOT NULL DEFAULT 2;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS relay_model_cooldown_backoff_enabled BOOLEAN NOT NULL DEFAULT FALSE;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS oauth_model_cooldown_mode VARCHAR(20) NOT NULL DEFAULT 'adaptive';
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS oauth_model_cooldown_seconds INT NOT NULL DEFAULT 300;
	ALTER TABLE system_settings ADD COLUMN IF NOT EXISTS oauth_model_cooldown_backoff_enabled BOOLEAN NOT NULL DEFAULT TRUE;

	ALTER TABLE account_groups ADD COLUMN IF NOT EXISTS auto_pause_5h_threshold DOUBLE PRECISION DEFAULT 0;
	ALTER TABLE account_groups ADD COLUMN IF NOT EXISTS auto_pause_7d_threshold DOUBLE PRECISION DEFAULT 0;

			CREATE TABLE IF NOT EXISTS prompt_filter_logs (
				id               SERIAL PRIMARY KEY,
				created_at       TIMESTAMPTZ DEFAULT NOW(),
				source           VARCHAR(50) DEFAULT '',
				endpoint         VARCHAR(256) DEFAULT '',
				request_protocol VARCHAR(64) DEFAULT '',
				request_provider VARCHAR(64) DEFAULT '',
				model            VARCHAR(100) DEFAULT '',
				action           VARCHAR(20) DEFAULT '',
				mode             VARCHAR(20) DEFAULT '',
				score            INT DEFAULT 0,
				audit_score      INT DEFAULT 0,
				threshold_value  INT DEFAULT 0,
				policy_profile   VARCHAR(32) DEFAULT '',
				reason_code      VARCHAR(100) DEFAULT '',
				primary_origin   VARCHAR(50) DEFAULT '',
				strike_eligible  BOOLEAN DEFAULT FALSE,
				matched_patterns TEXT DEFAULT '[]',
				text_preview     TEXT DEFAULT '',
				match_context    TEXT DEFAULT '',
				api_key_id       INT DEFAULT 0,
				api_key_name     VARCHAR(255) DEFAULT '',
				api_key_masked   VARCHAR(64) DEFAULT '',
				client_ip        VARCHAR(64) DEFAULT '',
				error_code       VARCHAR(100) DEFAULT '',
				review_model     VARCHAR(100) DEFAULT '',
				review_flagged   BOOLEAN DEFAULT FALSE,
				review_error     TEXT DEFAULT '',
				reviewed         BOOLEAN DEFAULT FALSE,
				review_confidence DOUBLE PRECISION NULL,
				review_threshold DOUBLE PRECISION NULL,
				review_reason    TEXT DEFAULT '',
				review_endpoint  VARCHAR(512) DEFAULT '',
				review_request_mode VARCHAR(32) DEFAULT '',
				review_latency_ms BIGINT NULL,
				full_text        TEXT DEFAULT ''
			);
			CREATE TABLE IF NOT EXISTS prompt_review_profiles (
				id VARCHAR(64) PRIMARY KEY,
				name VARCHAR(120) NOT NULL,
				base_url VARCHAR(512) NOT NULL DEFAULT '',
				model VARCHAR(128) NOT NULL DEFAULT '',
				request_mode VARCHAR(32) NOT NULL DEFAULT 'moderations',
				adapter_json TEXT NOT NULL DEFAULT '{}',
				api_keys TEXT NOT NULL DEFAULT '',
				timeout_seconds INTEGER NOT NULL DEFAULT 10,
				active BOOLEAN NOT NULL DEFAULT FALSE,
				created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
				updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
			);
			ALTER TABLE prompt_filter_logs ADD COLUMN IF NOT EXISTS review_model VARCHAR(100) DEFAULT '';
			ALTER TABLE prompt_filter_logs ADD COLUMN IF NOT EXISTS review_flagged BOOLEAN DEFAULT FALSE;
			ALTER TABLE prompt_filter_logs ADD COLUMN IF NOT EXISTS review_error TEXT DEFAULT '';
			ALTER TABLE prompt_filter_logs ADD COLUMN IF NOT EXISTS reviewed BOOLEAN DEFAULT FALSE;
			ALTER TABLE prompt_filter_logs ADD COLUMN IF NOT EXISTS review_confidence DOUBLE PRECISION NULL;
			ALTER TABLE prompt_filter_logs ADD COLUMN IF NOT EXISTS review_threshold DOUBLE PRECISION NULL;
			ALTER TABLE prompt_filter_logs ADD COLUMN IF NOT EXISTS review_reason TEXT DEFAULT '';
			ALTER TABLE prompt_filter_logs ADD COLUMN IF NOT EXISTS review_endpoint VARCHAR(512) DEFAULT '';
			ALTER TABLE prompt_filter_logs ADD COLUMN IF NOT EXISTS review_request_mode VARCHAR(32) DEFAULT '';
			ALTER TABLE prompt_filter_logs ADD COLUMN IF NOT EXISTS review_latency_ms BIGINT NULL;
			ALTER TABLE prompt_filter_logs ADD COLUMN IF NOT EXISTS full_text TEXT DEFAULT '';
			ALTER TABLE prompt_filter_logs ADD COLUMN IF NOT EXISTS match_context TEXT DEFAULT '';
			ALTER TABLE prompt_filter_logs ADD COLUMN IF NOT EXISTS audit_score INT DEFAULT 0;
			ALTER TABLE prompt_filter_logs ADD COLUMN IF NOT EXISTS policy_profile VARCHAR(32) DEFAULT '';
			ALTER TABLE prompt_filter_logs ADD COLUMN IF NOT EXISTS reason_code VARCHAR(100) DEFAULT '';
			ALTER TABLE prompt_filter_logs ADD COLUMN IF NOT EXISTS primary_origin VARCHAR(50) DEFAULT '';
			ALTER TABLE prompt_filter_logs ADD COLUMN IF NOT EXISTS strike_eligible BOOLEAN DEFAULT FALSE;
			ALTER TABLE prompt_filter_logs ADD COLUMN IF NOT EXISTS request_protocol VARCHAR(64) DEFAULT '';
			ALTER TABLE prompt_filter_logs ADD COLUMN IF NOT EXISTS request_provider VARCHAR(64) DEFAULT '';
			ALTER TABLE prompt_filter_logs ALTER COLUMN endpoint TYPE VARCHAR(256);
			CREATE INDEX IF NOT EXISTS idx_prompt_filter_logs_created_at ON prompt_filter_logs(created_at);
			CREATE INDEX IF NOT EXISTS idx_prompt_filter_logs_action_created_at ON prompt_filter_logs(action, created_at);
			CREATE INDEX IF NOT EXISTS idx_prompt_filter_logs_source_id ON prompt_filter_logs(source, id DESC);
			CREATE INDEX IF NOT EXISTS idx_prompt_filter_logs_reviewed_id ON prompt_filter_logs(reviewed, id DESC);
			DROP TABLE IF EXISTS prompt_filter_secrets;
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
	ALTER TABLE proxies ADD COLUMN IF NOT EXISTS test_status VARCHAR(20) NOT NULL DEFAULT 'untested';
	UPDATE proxies
	SET test_status = 'success'
	WHERE COALESCE(test_status, 'untested') = 'untested'
	  AND (COALESCE(test_ip, '') <> '' OR COALESCE(test_location, '') <> '' OR COALESCE(test_latency_ms, 0) > 0);

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
	if err := db.installSchedulerOutboxTriggers(ctx); err != nil {
		return fmt.Errorf("install scheduler outbox triggers: %w", err)
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
	if _, err = db.conn.ExecContext(migrateCtx, migrateQuery); err != nil {
		return err
	}
	return db.runDataMigrationsWithTimeout()
}

// ==================== API Keys ====================

// APIKeyRow API 密钥行
type APIKeyRow struct {
	ID              int64        `json:"id"`
	Name            string       `json:"name"`
	Key             string       `json:"key"`
	QuotaLimit      float64      `json:"quota_limit"`
	QuotaUsed       float64      `json:"quota_used"`
	TotalUsed       float64      `json:"total_used"`
	ResetCount      int          `json:"reset_count"`
	LastResetAt     sql.NullTime `json:"last_reset_at"`
	ExpiresAt       sql.NullTime `json:"expires_at"`
	AllowedGroupIDs []int64      `json:"allowed_group_ids"`
	Limits          APIKeyLimits `json:"limits"`
	Enabled         bool         `json:"enabled"`
	CreatedAt       time.Time    `json:"created_at"`
}

// APIKeyLimits 是 API Key 级别的细粒度限流/配额配置。
// 0 或空字段表示该项不限。落库为 JSON,允许平滑扩展字段。
//
//   - ModelAllow / ModelDeny: 模型白/黑名单。同时配置时白名单生效,黑名单忽略。
//   - RPM: 每分钟请求数 (滑动 60s 窗口)。
//   - RPD: 每天请求数 (滑动 24h 窗口)。
//   - MaxConcurrency: 同一 API Key 在当前实例内允许的最大并发请求数。
//   - CostLimit5h / CostLimit7d: 美元成本上限,滑动 5h / 7d 窗口,与账号侧窗口语义一致。
//   - TokenLimit5h / TokenLimit7d: token 上限,滑动 5h / 7d 窗口。
//   - CostLimitDaily / TokenLimitDaily: 自然日(服务器本地时区,零点清零)上限。与滑动窗口
//     不同,到点全额恢复;与 scope 维度的 1d(滚动 24h)也刻意区分(issue #460)。
//   - PlanAllow: 账号套餐白名单(plus/pro/team/...)。非空时该 Key 仅调度命中其一的账号,
//     语义与 AllowedGroupIDs 类似,均在账号选择阶段过滤。空表示不限套餐。
type APIKeyLimits struct {
	ModelRequestLimits []APIKeyModelRequestLimit `json:"model_request_limits,omitempty"`
	ModelAllow         []string                  `json:"model_allow,omitempty"`
	ModelDeny          []string                  `json:"model_deny,omitempty"`
	PlanAllow          []string                  `json:"plan_allow,omitempty"`
	// NoAffinityGroupIDs 指定未携带 Codex 引擎指纹或 X-Codex2API-Affinity-Key 的请求使用的账号分组。
	// 空表示不启用分流，继续沿用 AllowedGroupIDs 的现有行为。
	NoAffinityGroupIDs []int64 `json:"no_affinity_group_ids,omitempty"`
	RPM                int     `json:"rpm,omitempty"`
	RPD                int     `json:"rpd,omitempty"`
	MaxConcurrency     int     `json:"max_concurrency,omitempty"`
	CostLimit5h        float64 `json:"cost_limit_5h,omitempty"`
	CostLimit7d        float64 `json:"cost_limit_7d,omitempty"`
	CostLimit30d       float64 `json:"cost_limit_30d,omitempty"`
	CostLimitDaily     float64 `json:"cost_limit_daily,omitempty"`
	TokenLimit5h       int64   `json:"token_limit_5h,omitempty"`
	TokenLimit7d       int64   `json:"token_limit_7d,omitempty"`
	TokenLimit30d      int64   `json:"token_limit_30d,omitempty"`
	TokenLimitDaily    int64   `json:"token_limit_daily,omitempty"`
	// DisableImageGeneration 为 true 时，该 Key 禁止访问生图模型(gpt-image-*)与
	// 生图工具链路(image_generation 工具 / /v1/images 端点)，命中一律 403。
	// 保留为向后兼容字段：新配置改用 ImageGenerationPolicy；未设 policy 时该 bool=true
	// 等价于 policy=block（见 ResolveImageGenerationPolicy）。
	DisableImageGeneration bool `json:"disable_image_generation,omitempty"`
	// ImageGenerationPolicy 控制该 Key 遇到 Codex 图片工具能力时的处理策略：
	//   - ""/allow: 正常放行(默认行为)
	//   - strip:    剥离图片工具声明后作为普通文本请求继续转发上游(不返回 403)
	//   - block:    命中生图能力一律 403(等价旧 DisableImageGeneration=true)
	ImageGenerationPolicy string `json:"image_generation_policy,omitempty"`
	// AutoCompactOnOverflow 为 true 时，该 Key 的请求收到上游上下文超窗错误
	// (context_length_exceeded)后，网关把 input 旧轮次摘要压缩并重试一次，
	// 而不是直接把 400 透传给下游。默认关闭。
	AutoCompactOnOverflow bool `json:"auto_compact_overflow,omitempty"`
	// AllowLive 为 true 时，该 Key 可以使用 ChatGPT Live（/v1/live 与
	// /backend-api/codex/realtime/calls）。默认关闭。
	AllowLive bool `json:"allow_live,omitempty"`
	// UpstreamChannel 限定该 Key 的请求只调度到指定上游渠道的账号：
	//   - ""/auto: 不限（默认，按模型路由）
	//   - codex:   仅 Codex OAuth / OpenAI Responses 中转账号
	//   - grok:    仅 Grok 账号（此时不再要求账号声明模型，直接透传请求模型）
	//   - antigravity: Antigravity 管理渠道
	//   - claude:      仅 Claude OAuth / Anthropic Messages 账号
	UpstreamChannel string `json:"upstream_channel,omitempty"`
	// ScopeLimits 是「该 Key × 某账号分组 / 某账号」维度的用量上限（issue #439）。
	// 与上面的 Cost/Token 限额不同，它只统计该 Key 打到对应 scope 的用量，超额后默认
	// 把该 scope 的账号从候选池剔除（自动落到其它分组），详见 APIKeyScopeLimit。
	ScopeLimits []APIKeyScopeLimit `json:"scope_limits,omitempty"`
}

// 图片工具策略取值。
const (
	ImageGenerationPolicyAllow = "allow"
	ImageGenerationPolicyStrip = "strip"
	ImageGenerationPolicyBlock = "block"
)

// 上游渠道限定取值。
const (
	UpstreamChannelAuto        = ""
	UpstreamChannelCodex       = "codex"
	UpstreamChannelGrok        = "grok"
	UpstreamChannelAntigravity = "antigravity"
	UpstreamChannelClaude      = "claude"
)

// ResolveUpstreamChannel 归一 Key 的上游渠道限定；未知值一律视为不限（auto）。
func (l APIKeyLimits) ResolveUpstreamChannel() string {
	switch strings.ToLower(strings.TrimSpace(l.UpstreamChannel)) {
	case UpstreamChannelCodex:
		return UpstreamChannelCodex
	case UpstreamChannelGrok:
		return UpstreamChannelGrok
	case UpstreamChannelAntigravity:
		return UpstreamChannelAntigravity
	case UpstreamChannelClaude:
		return UpstreamChannelClaude
	}
	return UpstreamChannelAuto
}

// accountChannelFilterSQL returns the shared account-provider predicate used by
// both full account reads and the sanitized admin-list projection.
func accountChannelFilterSQL(channel, upstreamTypeExpr string) string {
	switch strings.ToLower(strings.TrimSpace(channel)) {
	case UpstreamChannelGrok:
		return ` AND ` + upstreamTypeExpr + ` = 'grok'`
	case UpstreamChannelAntigravity:
		return ` AND ` + upstreamTypeExpr + ` = 'antigravity'`
	case UpstreamChannelClaude:
		return ` AND ` + upstreamTypeExpr + ` = 'claude'`
	case UpstreamChannelCodex:
		// Blank legacy rows and OpenAI Responses relays remain in the Codex view.
		return ` AND ` + upstreamTypeExpr + ` NOT IN ('grok', 'antigravity', 'claude')`
	default:
		return ""
	}
}

// ResolveImageGenerationPolicy 归一 Key 的图片工具策略，统一新旧两种配置来源：
// 显式 ImageGenerationPolicy 优先；未设时旧 DisableImageGeneration=true 映射为 block；
// 其余一律 allow。返回值恒为 allow/strip/block 之一。
func (l APIKeyLimits) ResolveImageGenerationPolicy() string {
	switch strings.ToLower(strings.TrimSpace(l.ImageGenerationPolicy)) {
	case ImageGenerationPolicyStrip:
		return ImageGenerationPolicyStrip
	case ImageGenerationPolicyBlock:
		return ImageGenerationPolicyBlock
	case ImageGenerationPolicyAllow:
		return ImageGenerationPolicyAllow
	}
	if l.DisableImageGeneration {
		return ImageGenerationPolicyBlock
	}
	return ImageGenerationPolicyAllow
}

// IsZero 判断是否为空 limits(全部字段都未配置)
func (l APIKeyLimits) IsZero() bool {
	return len(l.ModelAllow) == 0 && len(l.ModelDeny) == 0 && len(l.PlanAllow) == 0 &&
		len(l.NoAffinityGroupIDs) == 0 &&
		l.RPM == 0 && l.RPD == 0 && l.MaxConcurrency == 0 &&
		l.CostLimit5h == 0 && l.CostLimit7d == 0 && l.CostLimit30d == 0 && l.CostLimitDaily == 0 &&
		l.TokenLimit5h == 0 && l.TokenLimit7d == 0 && l.TokenLimit30d == 0 && l.TokenLimitDaily == 0 &&
		len(l.ScopeLimits) == 0 && len(l.ModelRequestLimits) == 0 &&
		!l.DisableImageGeneration &&
		!l.AutoCompactOnOverflow &&
		!l.AllowLive &&
		l.ResolveImageGenerationPolicy() == ImageGenerationPolicyAllow &&
		l.ResolveUpstreamChannel() == UpstreamChannelAuto
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
	ResetQuota         bool
	ExpiresAt          sql.NullTime
	ExpiresAtSet       bool
	AllowedGroupIDs    []int64
	AllowedGroupIDsSet bool
	Limits             APIKeyLimits
	LimitsSet          bool
	Enabled            bool
	EnabledSet         bool
}

const apiKeySelectColumns = `id, name, key, created_at, COALESCE(quota_limit, 0), COALESCE(quota_used, 0), COALESCE(total_used, 0), COALESCE(reset_count, 0), last_reset_at, expires_at, COALESCE(allowed_group_ids, '[]'), COALESCE(limits, '{}'), COALESCE(enabled, TRUE)`

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
	rules, err := NormalizeAPIKeyModelRequestLimits(input.Limits.ModelRequestLimits)
	if err != nil {
		return 0, err
	}
	input.Limits.ModelRequestLimits = rules
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
	return row != nil && (row.QuotaLimit > 0 || row.ExpiresAt.Valid || len(row.AllowedGroupIDs) > 0 || !row.Limits.IsZero() || !row.Enabled)
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
	rules, err := NormalizeAPIKeyModelRequestLimits(limits.ModelRequestLimits)
	if err != nil {
		return err
	}
	limits.ModelRequestLimits = rules
	return db.withWriteTx(ctx, func(tx *sql.Tx) error {
		if err := db.validateAPIKeyModelRequestRuleUpdate(ctx, tx, id, rules); err != nil {
			return err
		}
		query := `UPDATE api_keys SET limits = $1::jsonb WHERE id = $2`
		if db.isSQLite() {
			query = `UPDATE api_keys SET limits = $1 WHERE id = $2`
		}
		_, err := tx.ExecContext(ctx, query, encodeAPIKeyLimits(limits), id)
		return err
	})
}

// UpdateAPIKey applies multiple editable fields in one transaction.
// Omitted fields keep their existing values.
func (db *DB) UpdateAPIKey(ctx context.Context, id int64, update APIKeyUpdate) error {
	if update.LimitsSet {
		rules, err := NormalizeAPIKeyModelRequestLimits(update.Limits.ModelRequestLimits)
		if err != nil {
			return err
		}
		update.Limits.ModelRequestLimits = rules
	}
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
	if update.ResetQuota {
		sets = append(sets, "quota_used = 0")
		sets = append(sets, "reset_count = COALESCE(reset_count, 0) + 1")
		if db.isSQLite() {
			sets = append(sets, "last_reset_at = CURRENT_TIMESTAMP")
		} else {
			sets = append(sets, "last_reset_at = NOW()")
		}
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
	if update.EnabledSet {
		sets = append(sets, "enabled = "+setArg(update.Enabled))
	}
	if len(sets) == 0 {
		return nil
	}
	idPlaceholder := placeholder()
	args[len(args)-1] = id
	return db.withWriteTx(ctx, func(tx *sql.Tx) error {
		if update.LimitsSet {
			if err := db.validateAPIKeyModelRequestRuleUpdate(ctx, tx, id, update.Limits.ModelRequestLimits); err != nil {
				return err
			}
		}
		res, err := tx.ExecContext(ctx, "UPDATE api_keys SET "+strings.Join(sets, ", ")+" WHERE id = "+idPlaceholder, args...)
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

// APIKeyQuotaResetTarget identifies one row changed by a quota reset. Returning
// the raw key lets callers evict exactly the affected authentication cache.
type APIKeyQuotaResetTarget struct {
	ID  int64
	Key string
}

// ResetAPIKeyQuota clears cumulative usage and records a cutoff that restarts
// the API key's 5h/7d windows without deleting historical usage logs.
func (db *DB) ResetAPIKeyQuota(ctx context.Context, id int64) (*APIKeyQuotaResetTarget, error) {
	db.FlushUsageLogs()
	target := &APIKeyQuotaResetTarget{}
	err := db.conn.QueryRowContext(ctx, `
		UPDATE api_keys
		SET quota_used = 0,
			reset_count = COALESCE(reset_count, 0) + 1,
			last_reset_at = $1
		WHERE id = $2
		RETURNING id, key
	`, db.timeArg(time.Now()), id).Scan(&target.ID, &target.Key)
	if err != nil {
		return nil, err
	}
	return target, nil
}

// ResetAllAPIKeyQuotas restarts cumulative and 5h/7d usage periods for every
// API key in one statement and returns the exact affected rows for cache eviction.
func (db *DB) ResetAllAPIKeyQuotas(ctx context.Context) ([]APIKeyQuotaResetTarget, error) {
	db.FlushUsageLogs()
	rows, err := db.conn.QueryContext(ctx, `
		UPDATE api_keys
		SET quota_used = 0,
			reset_count = COALESCE(reset_count, 0) + 1,
			last_reset_at = $1
		RETURNING id, key
	`, db.timeArg(time.Now()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	targets := make([]APIKeyQuotaResetTarget, 0)
	for rows.Next() {
		var target APIKeyQuotaResetTarget
		if err := rows.Scan(&target.ID, &target.Key); err != nil {
			return nil, err
		}
		targets = append(targets, target)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return targets, nil
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

const (
	DefaultModelsListReadMaxBytes int64 = 8 << 20
	MinModelsListReadMaxBytes     int64 = 1 << 20
	MaxModelsListReadMaxBytes     int64 = 256 << 20
)

// ValidateModelsListReadMaxBytes 校验模型列表响应的最大读取字节数。
func ValidateModelsListReadMaxBytes(value int64) error {
	if value < MinModelsListReadMaxBytes || value > MaxModelsListReadMaxBytes {
		return fmt.Errorf("models_list_read_max_bytes 必须在 %d 到 %d 字节之间", MinModelsListReadMaxBytes, MaxModelsListReadMaxBytes)
	}
	return nil
}

// NormalizeModelsListReadMaxBytes 把缺失或越界旧值收敛到安全范围。
func NormalizeModelsListReadMaxBytes(value int64) int64 {
	if value <= 0 {
		return DefaultModelsListReadMaxBytes
	}
	if value < MinModelsListReadMaxBytes {
		return MinModelsListReadMaxBytes
	}
	if value > MaxModelsListReadMaxBytes {
		return MaxModelsListReadMaxBytes
	}
	return value
}

// SystemSettings 运行时设置项
type SystemSettings struct {
	SiteName                           string
	SiteLogo                           string
	BackgroundConfig                   string // JSON: {"image":"...","opacity":18,"blur":0}
	GrokConfig                         string // JSON: {"affinity_mode":"strict"}
	ClaudeConfig                       string // JSON: {"fingerprint_mode":"preserve","default_timezone":"","session_window_limit":0}
	MaxConcurrency                     int
	GlobalRPM                          int
	TestModel                          string
	TestContent                        string
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
	SchedulerEngine                    string
	MaxRetries                         int
	MaxRateLimitRetries                int
	ContinuousRetryPolicy              string // JSON: database.ContinuousRetryPolicy
	AllowRemoteMigration               bool
	ModelMapping                       string // JSON: {"anthropic_model": "codex_model", ...}
	CodexModelMapping                  string // JSON: {"requested_codex_model": "upstream_codex_model", ...}
	PayloadRules                       string // JSON: 请求体重写规则（default/override/append/filter 等规则组）
	ReasoningEffortModels              string // JSON: [{"model":"gpt-5.5","effort":"xhigh"}, ...]
	BackgroundRefreshIntervalMinutes   int
	UsageProbeMaxAgeMinutes            int
	UsageProbeConcurrency              int
	UsageProbeResponsesFallbackEnabled bool
	RecoveryProbeIntervalMinutes       int
	SchedulerMode                      string
	AffinityMode                       string // session 粘性模式: bounded / off / strict
	SessionAffinitySpread              bool   // 新亲和键按 HRW 哈希散列选号(issue #484)
	SessionSlotBufferEnabled           bool   // 成功请求结束后为原会话短暂保留并发槽
	SessionSlotBufferSeconds           int    // 会话并发槽缓冲时间，默认 10 秒，范围 1..60
	ModelsListReadMaxBytes             int64  // 上游 /models 与 Codex 模型清单的最大读取字节数，默认 8 MiB
	ResinURL                           string // Resin 代理池地址（含 Token），例如 http://127.0.0.1:2260/my-token
	ResinPlatformName                  string // Resin 平台标识，例如 codex2api
	PromptFilterEnabled                bool
	PromptFilterMode                   string
	PromptFilterThreshold              int
	PromptFilterStrictThreshold        int
	PromptFilterStrictTerminalEnabled  bool
	PromptFilterAdvancedConfig         string
	PromptFilterLogMatches             bool
	PromptFilterMaxTextLength          int
	PromptFilterSensitiveWords         string
	PromptFilterCustomPatterns         string
	PromptFilterDisabledPatterns       string
	PromptFilterReviewEnabled          bool
	PromptFilterReviewAPIKey           string
	PromptFilterReviewBaseURL          string
	PromptFilterReviewModel            string
	PromptFilterReviewTimeoutSeconds   int
	PromptFilterReviewFailClosed       bool
	ClientCompatMode                   string
	CodexMinCLIVersion                 string
	CodexUserAgentConfig               string
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
	PublicKeyUsagePageEnabled          bool
	PublicImageStudioPageEnabled       bool
	PublicAccountPortalPageEnabled     bool // 账号自助添加公开门户开关，默认 false
	CodexForceWebsocket                bool // 强制 Codex 上游走 WebSocket（复用连接池），默认 false
	CodexRequestCompression            bool // HTTP /responses 请求体 zstd 压缩（对齐真实客户端），默认 true
	CodexWSWeakNetworkMode             bool // WS 弱网保守复用模式，默认 false
	CodexWSKeepaliveEnabled            bool // 启用上游 WS 空闲连接保活（仅 Ping，不发业务帧），默认 false
	CodexWSKeepaliveIntervalSec        int  // WS 保活 Ping 间隔（秒），默认 60
	CodexWSHideUpstreamErrors          bool // 隐藏上游 WS 原始错误，默认 true
	CodexWSSilentRetryEnabled          bool // 首包前 WS 上游错误静默换号重试，默认 true
	CodexWSSilentMaxRetries            int  // WS 静默换号最大重试次数，默认 2
	CodexWSSizeRouterEnabled           bool // 1009 自学习体积路由：超大请求直接首发 HTTP，默认 true
	CodexWSBusyAcquireMaxWaitSec       int  // busy session/容量等待的累计上限（秒），默认 30（issue #413）
	CodexWSBusyOverflowEnabled         bool // busy session 溢出到同账号兄弟连接，默认 false（issue #413）
	CodexWSBusyPatienceSec             int  // 触发溢出前的短等待（秒），默认 2（issue #413）
	CodexWSStatelessSlots              int  // 无状态请求每 (账号, cacheKey) 的持久连接槽位数，默认 8，范围 1-32（issue #522）
	// GithubToken 用于 api.github.com 请求的 Personal Access Token（提升限流配额，
	// 只发给 api.github.com，绝不发给镜像/其他主机；空表示未配置，issue #522）。
	GithubToken string
	// GithubProxyURL GitHub 域名（github.com / api.github.com / *.githubusercontent.com）
	// 专用出站代理；空表示回落全局代理/环境代理（issue #522）。
	GithubProxyURL string
	// Codex 过载熔断：单账号滑动窗口内 server_is_overloaded 占比达到阈值时
	// 自动暂停调度一段时间（默认关闭）。
	CodexOverloadPauseEnabled           bool
	CodexOverloadThresholdPercent       int  // 触发比例（%），默认 20，范围 1-100
	CodexOverloadPauseMinutes           int  // 暂停时长（分钟），默认 30，范围 1-1440
	CodexOverloadWindowMinutes          int  // 统计窗口（分钟），默认 5，范围 1-120
	OverflowAutoCompactEnabled          bool // 上下文超窗时自动摘要旧轮次并重试一次（实验性，默认 false，issue #415）
	CompactViaResponsesEnabled          bool // /v1/responses/compact 改写为 /responses body-signal 压缩（上游已下线专用端点，默认 false）
	CodexPreflightSSEPassthroughEnabled bool // 前置元数据 SSE 事件立即透传下游（旧版兼容，默认 false，issue #425）
	FirstTokenExcludesWsAcquire         bool // 落库 first_token_ms 扣除 WS 取连耗时，默认 false（原始值 = first_token_ms + ws_acquire_ms）
	CodexContinueThinkingEnabled        bool // 检测到上游截断思考时自动续想并折叠成单响应，默认 false
	CodexContinueMaxRounds              int  // 单次请求最大续想轮数（含首轮），默认 8
	UTLSShutdownTimeoutMinutes          int  // uTLS 连接被摘出池后等待在途 stream 收尾的上限（分钟，默认 30，范围 1-240，issue #446）
	// CodexFingerprintDefaultMode 是新导入/新建 Codex 账号默认盖上的指纹收敛档位
	// （off/device/session/full，默认 off）。只影响导入之后新建的账号，已有账号不变。
	CodexFingerprintDefaultMode string
	AutoPause5hThreshold        float64
	AutoPause7dThreshold        float64
	AutoPause5hGuardBandPercent float64
	AutoPause5hGuardConcurrency int
	SmartPacingEnabled          bool   // issue #312 智能配速总开关
	SmartPacingMinConcurrency   int    // 配速并发下限
	SmartPacingWindows          string // "5h,7d" / "5h" / "7d"
	IgnoreUsageLimitStatus      bool   // 用量窗口仅作参考，以 Responses 成功/usage_limit_reached 判定可用性
	RetryIntervalMS             int    // 重试间隔毫秒（0 = 立即重试，保持旧行为）
	TransportRetryPolicy        string // 传输错误重试策略: rotate（换号，旧行为）/ sticky（同号延迟重试）
	// CodexSyncedCLIVersion 是从 openai/codex releases 同步到的最新 Codex CLI 版本缓存，
	// 用于抬升出站 UA / manifest 的模拟版本（绝不低于内置常量），空表示尚未同步。
	CodexSyncedCLIVersion string
	// CodexCLIVersionSyncEnabled 控制是否后台定时自动同步 Codex CLI 版本（默认 true）。
	CodexCLIVersionSyncEnabled bool
	// CodexCLIVersionSyncIntervalHours 是定时同步间隔（小时，默认 12，范围 1-720）。
	CodexCLIVersionSyncIntervalHours int
	// AutoResetCreditsEnabled 控制 Plus/Pro 主动重置次数的临期自动消费（默认关闭）。
	AutoResetCreditsEnabled bool
	// AutoResetCreditsBeforeExpiryMin 是进入临期窗口的提前分钟数（默认 60，范围 10-10080）。
	AutoResetCreditsBeforeExpiryMin int
	// AutoActivate5hWindowEnabled 控制 5h 窗口重置后是否发送一次最小真实 /responses 以启动下一轮窗口（默认关闭，issue #581）。
	AutoActivate5hWindowEnabled bool
	// ModelPricingOverrides 是模型定价覆盖 JSON（model → ModelPricingOverride），
	// custom/synced 覆盖代码默认；空为 "{}"。
	ModelPricingOverrides string
	// ModelPricingSyncURL 是「从 JSON URL 同步定价」的来源地址，空时用内置默认。
	ModelPricingSyncURL              string
	RelayModelCooldownMode           string
	RelayModelCooldownSeconds        int
	RelayModelCooldownBackoffEnabled bool
	OAuthModelCooldownMode           string
	OAuthModelCooldownSeconds        int
	OAuthModelCooldownBackoffEnabled bool

	// PreservePromptFilterCustomPatterns is an update-only concurrency guard.
	// When true, an existing row keeps its current custom-pattern value instead
	// of accepting a potentially stale full-settings snapshot.
	PreservePromptFilterCustomPatterns bool
	// PreservePromptFilterReviewAPIKey is the same guard for the review key
	// list: individual keys can be deleted out-of-band, so a full-settings
	// snapshot that didn't change the field must not write it back.
	PreservePromptFilterReviewAPIKey bool
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

// NormalizeAutoResetCreditsBeforeExpiryMinutes 将临期自动消费阈值限制在
// 10 分钟到 7 天；非正值回退默认 60 分钟。
func NormalizeAutoResetCreditsBeforeExpiryMinutes(minutes int) int {
	if minutes <= 0 {
		return 60
	}
	if minutes < 10 {
		return 10
	}
	if minutes > 10080 {
		return 10080
	}
	return minutes
}

// normalizeSmartPacingMinConcurrencyDB 归一化智能配速并发下限（1..1000，默认 1）。
func normalizeSmartPacingMinConcurrencyDB(value int) int {
	if value < 1 {
		return 1
	}
	if value > 1000 {
		return 1000
	}
	return value
}

// normalizeSmartPacingWindowsDB 归一化配速窗口为 "5h,7d" / "5h" / "7d"，非法回退 "5h,7d"。
func normalizeSmartPacingWindowsDB(raw string) string {
	var w5h, w7d bool
	for _, part := range strings.Split(strings.ToLower(strings.TrimSpace(raw)), ",") {
		switch strings.TrimSpace(part) {
		case "5h":
			w5h = true
		case "7d":
			w7d = true
		}
	}
	switch {
	case w5h && w7d:
		return "5h,7d"
	case w5h:
		return "5h"
	case w7d:
		return "7d"
	default:
		return "5h,7d"
	}
}

// NormalizeSessionSlotBufferSeconds normalizes the configurable reservation
// duration. Enablement is stored separately, so zero values fall back to 10s.
func NormalizeSessionSlotBufferSeconds(seconds int) int {
	if seconds <= 0 {
		return 10
	}
	if seconds > 60 {
		return 60
	}
	return seconds
}

// NormalizeSchedulerEngine preserves the old fast_scheduler_enabled setting
// for upgraded databases whose new scheduler_engine column is still blank.
func NormalizeSchedulerEngine(value string, legacyFastEnabled bool) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "legacy", "shadow", "indexed":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		if legacyFastEnabled {
			return "indexed"
		}
		return "legacy"
	}
}

// GetSystemSettings 加载全局设置
func (db *DB) GetSystemSettings(ctx context.Context) (*SystemSettings, error) {
	s := &SystemSettings{}
	err := db.conn.QueryRowContext(ctx, `
		SELECT COALESCE(site_name, 'CodexProxy'), COALESCE(site_logo, ''),
		       max_concurrency, global_rpm, test_model, COALESCE(test_content, 'hi'), test_concurrency, proxy_url, pg_max_conns, redis_pool_size,
		       auto_clean_unauthorized, auto_clean_rate_limited, COALESCE(admin_secret, ''), COALESCE(auto_clean_full_usage, false),
		       COALESCE(proxy_pool_enabled, false),
		       COALESCE(fast_scheduler_enabled, false),
		       COALESCE(scheduler_engine, ''),
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
		       COALESCE(session_affinity_spread, false),
		       COALESCE(resin_url, ''),
		       COALESCE(resin_platform_name, ''),
		       COALESCE(prompt_filter_enabled, false),
		       COALESCE(prompt_filter_mode, 'monitor'),
		       COALESCE(prompt_filter_threshold, 50),
		       COALESCE(prompt_filter_strict_threshold, 90),
		       COALESCE(prompt_filter_strict_terminal_enabled, false),
		       COALESCE(prompt_filter_advanced_config, '{}'),
		       COALESCE(prompt_filter_log_matches, true),
		       COALESCE(prompt_filter_max_text_length, 81920),
		       COALESCE(prompt_filter_sensitive_words, ''),
		       COALESCE(prompt_filter_custom_patterns, '[]'),
		       COALESCE(prompt_filter_disabled_patterns, '[]'),
		       COALESCE(prompt_filter_review_enabled, false),
		       COALESCE(prompt_filter_review_api_key, ''),
		       COALESCE(prompt_filter_review_base_url, 'https://api.deepseek.com'),
		       COALESCE(prompt_filter_review_model, 'deepseek-v4-flash'),
		       COALESCE(prompt_filter_review_timeout_seconds, 10),
		       COALESCE(prompt_filter_review_fail_closed, true),
		       COALESCE(client_compat_mode, 'preserve'),
		       COALESCE(codex_min_cli_version, '0.153.3'),
		       COALESCE(codex_user_agent_config, '{}'),
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
		       COALESCE(grok_config, '{}'),
		       COALESCE(show_full_usage_numbers, false),
		       COALESCE(public_key_usage_page_enabled, true),
		       COALESCE(public_image_studio_page_enabled, true),
		       COALESCE(public_account_portal_page_enabled, false),
			       COALESCE(reasoning_effort_models, '[]'),
			       COALESCE(codex_force_websocket, false),
			       COALESCE(codex_request_compression, true),
			       COALESCE(codex_ws_keepalive_enabled, false),
			       COALESCE(codex_ws_keepalive_interval_sec, 60),
			       COALESCE(codex_ws_hide_upstream_errors, true),
			       COALESCE(codex_ws_silent_retry_enabled, true),
			       COALESCE(codex_ws_silent_max_retries, 2),
			       COALESCE(codex_continue_thinking_enabled, false),
			       COALESCE(codex_continue_max_rounds, 8),
			       COALESCE(auto_pause_5h_threshold, 0),
			       COALESCE(auto_pause_7d_threshold, 0),
			       COALESCE(auto_pause_5h_guard_band_percent, 5),
			       COALESCE(auto_pause_5h_guard_concurrency, 1),
			       COALESCE(smart_pacing_enabled, false),
			       COALESCE(smart_pacing_min_concurrency, 1),
			       COALESCE(smart_pacing_windows, '5h,7d'),
			       COALESCE(retry_interval_ms, 0),
			       COALESCE(NULLIF(TRIM(transport_retry_policy), ''), 'rotate'),
			       COALESCE(codex_synced_cli_version, ''),
			       COALESCE(codex_cli_version_sync_enabled, true),
			       COALESCE(codex_cli_version_sync_interval_hours, 12),
			       COALESCE(model_pricing_overrides, '{}'),
			       COALESCE(model_pricing_sync_url, ''),
			       COALESCE(ignore_usage_limit_status, false),
			       COALESCE(auto_reset_credits_enabled, false),
			       COALESCE(auto_reset_credits_before_expiry_min, 60),
			       COALESCE(payload_rules, '{}'),
			       COALESCE(codex_ws_size_router_enabled, true),
			       COALESCE(codex_ws_busy_acquire_max_wait_sec, 30),
			       COALESCE(codex_ws_busy_overflow_enabled, false),
			       COALESCE(codex_ws_busy_patience_sec, 2),
			       COALESCE(overflow_auto_compact_enabled, false),
			       COALESCE(first_token_excludes_ws_acquire, false),
			       COALESCE(codex_preflight_sse_passthrough_enabled, false),
			       COALESCE(utls_shutdown_timeout_minutes, 30),
			       COALESCE(codex_ws_weak_network_mode, false),
			       COALESCE(NULLIF(TRIM(codex_fingerprint_default_mode), ''), 'off'),
			       COALESCE(compact_via_responses_enabled, false),
		       COALESCE(codex_ws_stateless_slots, 8),
		       COALESCE(github_token, ''),
		       COALESCE(github_proxy_url, ''),
		       COALESCE(codex_overload_pause_enabled, false),
		       COALESCE(codex_overload_threshold_percent, 20),
		       COALESCE(codex_overload_pause_minutes, 30),
		       COALESCE(codex_overload_window_minutes, 5),
		       COALESCE(session_slot_buffer_enabled, false),
		       COALESCE(session_slot_buffer_seconds, 10),
		       COALESCE(models_list_read_max_bytes, 8388608),
		       COALESCE(auto_activate_5h_window_enabled, false),
		       COALESCE(claude_config, '{}')
			FROM system_settings WHERE id = 1
		`).Scan(
		&s.SiteName, &s.SiteLogo,
		&s.MaxConcurrency, &s.GlobalRPM, &s.TestModel, &s.TestContent, &s.TestConcurrency, &s.ProxyURL, &s.PgMaxConns, &s.RedisPoolSize,
		&s.AutoCleanUnauthorized, &s.AutoCleanRateLimited, &s.AdminSecret, &s.AutoCleanFullUsage,
		&s.ProxyPoolEnabled, &s.FastSchedulerEnabled, &s.SchedulerEngine, &s.MaxRetries, &s.MaxRateLimitRetries, &s.AllowRemoteMigration,
		&s.AutoCleanError, &s.AutoCleanExpired, &s.LazyMode, &s.ModelMapping, &s.CodexModelMapping,
		&s.BackgroundRefreshIntervalMinutes, &s.UsageProbeMaxAgeMinutes, &s.UsageProbeConcurrency, &s.UsageProbeResponsesFallbackEnabled, &s.RecoveryProbeIntervalMinutes,
		&s.SchedulerMode,
		&s.AffinityMode,
		&s.SessionAffinitySpread,
		&s.ResinURL, &s.ResinPlatformName,
		&s.PromptFilterEnabled, &s.PromptFilterMode, &s.PromptFilterThreshold, &s.PromptFilterStrictThreshold, &s.PromptFilterStrictTerminalEnabled, &s.PromptFilterAdvancedConfig,
		&s.PromptFilterLogMatches, &s.PromptFilterMaxTextLength, &s.PromptFilterSensitiveWords,
		&s.PromptFilterCustomPatterns, &s.PromptFilterDisabledPatterns,
		&s.PromptFilterReviewEnabled, &s.PromptFilterReviewAPIKey, &s.PromptFilterReviewBaseURL,
		&s.PromptFilterReviewModel, &s.PromptFilterReviewTimeoutSeconds, &s.PromptFilterReviewFailClosed,
		&s.ClientCompatMode, &s.CodexMinCLIVersion, &s.CodexUserAgentConfig, &s.UsageLogMode, &s.UsageLogBatchSize,
		&s.UsageLogFlushIntervalSeconds, &s.StreamFlushPolicy, &s.StreamFlushIntervalMS,
		&s.FirstTokenMode,
		&s.FirstTokenTimeoutSeconds,
		&s.BillingTierPolicy,
		&s.ImageStorageConfig,
		&s.BackgroundConfig,
		&s.GrokConfig,
		&s.ShowFullUsageNumbers,
		&s.PublicKeyUsagePageEnabled,
		&s.PublicImageStudioPageEnabled,
		&s.PublicAccountPortalPageEnabled,
		&s.ReasoningEffortModels,
		&s.CodexForceWebsocket,
		&s.CodexRequestCompression,
		&s.CodexWSKeepaliveEnabled,
		&s.CodexWSKeepaliveIntervalSec,
		&s.CodexWSHideUpstreamErrors,
		&s.CodexWSSilentRetryEnabled,
		&s.CodexWSSilentMaxRetries,
		&s.CodexContinueThinkingEnabled,
		&s.CodexContinueMaxRounds,
		&s.AutoPause5hThreshold,
		&s.AutoPause7dThreshold,
		&s.AutoPause5hGuardBandPercent,
		&s.AutoPause5hGuardConcurrency,
		&s.SmartPacingEnabled,
		&s.SmartPacingMinConcurrency,
		&s.SmartPacingWindows,
		&s.RetryIntervalMS,
		&s.TransportRetryPolicy,
		&s.CodexSyncedCLIVersion,
		&s.CodexCLIVersionSyncEnabled,
		&s.CodexCLIVersionSyncIntervalHours,
		&s.ModelPricingOverrides,
		&s.ModelPricingSyncURL,
		&s.IgnoreUsageLimitStatus,
		&s.AutoResetCreditsEnabled,
		&s.AutoResetCreditsBeforeExpiryMin,
		&s.PayloadRules,
		&s.CodexWSSizeRouterEnabled,
		&s.CodexWSBusyAcquireMaxWaitSec,
		&s.CodexWSBusyOverflowEnabled,
		&s.CodexWSBusyPatienceSec,
		&s.OverflowAutoCompactEnabled,
		&s.FirstTokenExcludesWsAcquire,
		&s.CodexPreflightSSEPassthroughEnabled,
		&s.UTLSShutdownTimeoutMinutes,
		&s.CodexWSWeakNetworkMode,
		&s.CodexFingerprintDefaultMode,
		&s.CompactViaResponsesEnabled,
		&s.CodexWSStatelessSlots,
		&s.GithubToken,
		&s.GithubProxyURL,
		&s.CodexOverloadPauseEnabled,
		&s.CodexOverloadThresholdPercent,
		&s.CodexOverloadPauseMinutes,
		&s.CodexOverloadWindowMinutes,
		&s.SessionSlotBufferEnabled,
		&s.SessionSlotBufferSeconds,
		&s.ModelsListReadMaxBytes,
		&s.AutoActivate5hWindowEnabled,
		&s.ClaudeConfig,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	s.SiteName = NormalizeSiteName(s.SiteName)
	s.SiteLogo = strings.TrimSpace(s.SiteLogo)
	s.TestContent = strings.TrimSpace(s.TestContent)
	if s.TestContent == "" {
		s.TestContent = "hi"
	}
	if strings.TrimSpace(s.ReasoningEffortModels) == "" {
		s.ReasoningEffortModels = "[]"
	}
	if strings.TrimSpace(s.CodexUserAgentConfig) == "" {
		s.CodexUserAgentConfig = "{}"
	}
	if strings.TrimSpace(s.ModelPricingOverrides) == "" {
		s.ModelPricingOverrides = "{}"
	}
	if strings.TrimSpace(s.PayloadRules) == "" {
		s.PayloadRules = "{}"
	}
	var continuousRetryRaw sql.NullString
	if policyErr := db.conn.QueryRowContext(ctx, `SELECT COALESCE(continuous_retry_policy, '') FROM system_settings WHERE id = 1`).Scan(&continuousRetryRaw); policyErr == nil {
		s.ContinuousRetryPolicy = continuousRetryRaw.String
	} else if !errors.Is(policyErr, sql.ErrNoRows) {
		return nil, policyErr
	}
	if strings.TrimSpace(s.ContinuousRetryPolicy) == "" {
		s.ContinuousRetryPolicy = EncodeContinuousRetryPolicy(DefaultContinuousRetryPolicy())
	}
	s.FirstTokenMode = normalizeFirstTokenMode(s.FirstTokenMode)
	s.BillingTierPolicy = normalizeBillingTierPolicy(s.BillingTierPolicy)
	s.AutoResetCreditsBeforeExpiryMin = NormalizeAutoResetCreditsBeforeExpiryMinutes(s.AutoResetCreditsBeforeExpiryMin)
	s.CodexFingerprintDefaultMode = NormalizeCodexFingerprintDefaultMode(s.CodexFingerprintDefaultMode)
	s.SessionSlotBufferSeconds = NormalizeSessionSlotBufferSeconds(s.SessionSlotBufferSeconds)
	s.ModelsListReadMaxBytes = NormalizeModelsListReadMaxBytes(s.ModelsListReadMaxBytes)
	s.SchedulerEngine = NormalizeSchedulerEngine(s.SchedulerEngine, s.FastSchedulerEnabled)
	s.FastSchedulerEnabled = s.SchedulerEngine != "legacy"
	return s, err
}

// UpdateContinuousRetryPolicy atomically merges one admin partial update into
// the latest persisted policy. The row lock prevents concurrent tabs or
// replicas from replacing fields they did not edit with a stale JSON snapshot.
func (db *DB) UpdateContinuousRetryPolicy(ctx context.Context, update ContinuousRetryPolicyUpdate) (ContinuousRetryPolicy, error) {
	if db == nil || db.conn == nil {
		return DefaultContinuousRetryPolicy(), nil
	}

	var committed ContinuousRetryPolicy
	err := db.withSQLiteWriteLock(ctx, func() error {
		tx, err := db.conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()

		defaultRaw := EncodeContinuousRetryPolicy(DefaultContinuousRetryPolicy())
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO system_settings (id, continuous_retry_policy)
			VALUES (1, $1)
			ON CONFLICT (id) DO NOTHING
		`, defaultRaw); err != nil {
			return err
		}

		var currentRaw string
		if err := tx.QueryRowContext(ctx, continuousRetryPolicySelectQuery(!db.isSQLite())).Scan(&currentRaw); err != nil {
			return err
		}
		current := DefaultContinuousRetryPolicy()
		if strings.TrimSpace(currentRaw) != "" {
			current = ParseContinuousRetryPolicy(currentRaw)
		}
		next := current
		if update.Enabled != nil {
			next.Enabled = *update.Enabled
		}
		if update.CatchAll != nil {
			next.CatchAll = *update.CatchAll
		}
		if update.Categories != nil {
			next.Categories = append([]string(nil), (*update.Categories)...)
		}
		if update.StatusCodes != nil {
			next.StatusCodes = append([]int(nil), (*update.StatusCodes)...)
		}
		if update.ErrorCodes != nil {
			next.ErrorCodes = append([]string(nil), (*update.ErrorCodes)...)
		}
		if update.MaxDurationSeconds != nil {
			next.MaxDurationSeconds = *update.MaxDurationSeconds
		}
		next = NormalizeContinuousRetryPolicy(next)
		nextRaw := EncodeContinuousRetryPolicy(next)
		if nextRaw != strings.TrimSpace(currentRaw) {
			if _, err := tx.ExecContext(ctx, `
				UPDATE system_settings
				SET continuous_retry_policy = $1
				WHERE id = 1
			`, nextRaw); err != nil {
				return err
			}
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		committed = next
		return nil
	})
	if err != nil {
		return ContinuousRetryPolicy{}, err
	}
	return committed, nil
}

func continuousRetryPolicySelectQuery(forUpdate bool) string {
	query := `
		SELECT COALESCE(continuous_retry_policy, '')
		FROM system_settings
		WHERE id = 1
	`
	if forUpdate {
		query += " FOR UPDATE"
	}
	return query
}

// NormalizeCodexFingerprintDefaultMode 把新账号默认指纹收敛档位归一到四个已知
// 取值之一；空值和非法值回落 off（与 auth.NormalizeCodexFingerprintMode 语义一致，
// database 包不能反向依赖 auth，故此处独立实现）。
func NormalizeCodexFingerprintDefaultMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "device":
		return "device"
	case "session":
		return "session"
	case "full":
		return "full"
	default:
		return "off"
	}
}

// UpdateSystemSettings 更新全局设置（upsert：无行时自动插入）。
// codex_synced_cli_version 与 model_pricing_* 由各自的窄更新独立维护；冲突更新时
// 保留数据库当前值，避免管理员保存其他设置时回滚后台同步刚写入的数据。
func (db *DB) UpdateSystemSettings(ctx context.Context, s *SystemSettings) error {
	reasoningEffortModels := strings.TrimSpace(s.ReasoningEffortModels)
	if reasoningEffortModels == "" {
		reasoningEffortModels = "[]"
	}
	codexUserAgentConfig := strings.TrimSpace(s.CodexUserAgentConfig)
	if codexUserAgentConfig == "" {
		codexUserAgentConfig = "{}"
	}
	payloadRules := strings.TrimSpace(s.PayloadRules)
	if payloadRules == "" {
		payloadRules = "{}"
	}
	firstTokenMode := normalizeFirstTokenMode(s.FirstTokenMode)
	billingTierPolicy := normalizeBillingTierPolicy(s.BillingTierPolicy)
	testContent := strings.TrimSpace(s.TestContent)
	if testContent == "" {
		testContent = "hi"
	}
	_, err := db.conn.ExecContext(ctx, `
			INSERT INTO system_settings (
				id, site_name, site_logo, max_concurrency, global_rpm, test_model, test_content, test_concurrency, proxy_url, pg_max_conns, redis_pool_size,
				auto_clean_unauthorized, auto_clean_rate_limited, admin_secret, auto_clean_full_usage, proxy_pool_enabled,
				fast_scheduler_enabled, max_retries, max_rate_limit_retries, allow_remote_migration, auto_clean_error, auto_clean_expired, lazy_mode, model_mapping, codex_model_mapping,
					background_refresh_interval_minutes, usage_probe_max_age_minutes, recovery_probe_interval_minutes,
					usage_probe_concurrency, usage_probe_responses_fallback_enabled,
				resin_url, resin_platform_name, prompt_filter_enabled, prompt_filter_mode, prompt_filter_threshold,
				prompt_filter_strict_threshold, prompt_filter_log_matches, prompt_filter_max_text_length,
				prompt_filter_sensitive_words, prompt_filter_custom_patterns, prompt_filter_disabled_patterns,
				prompt_filter_review_enabled, prompt_filter_review_api_key, prompt_filter_review_base_url,
				prompt_filter_review_model, prompt_filter_review_timeout_seconds, prompt_filter_review_fail_closed,
				client_compat_mode, codex_min_cli_version, codex_user_agent_config, usage_log_mode, usage_log_batch_size,
					usage_log_flush_interval_seconds, stream_flush_policy, stream_flush_interval_ms,
					first_token_timeout_seconds,
					first_token_mode,
					billing_tier_policy,
					image_storage_config,
				scheduler_mode,
				affinity_mode,
				session_affinity_spread,
				background_config,
				grok_config,
				show_full_usage_numbers,
				public_key_usage_page_enabled,
				public_image_studio_page_enabled,
					reasoning_effort_models,
					codex_force_websocket,
					codex_ws_keepalive_enabled,
					codex_ws_keepalive_interval_sec,
					codex_ws_hide_upstream_errors,
					codex_ws_silent_retry_enabled,
					codex_ws_silent_max_retries,
					auto_pause_5h_threshold,
					auto_pause_7d_threshold,
					auto_pause_5h_guard_band_percent,
					auto_pause_5h_guard_concurrency,
					smart_pacing_enabled,
					smart_pacing_min_concurrency,
					smart_pacing_windows,
					retry_interval_ms,
					transport_retry_policy,
					codex_continue_thinking_enabled,
					codex_continue_max_rounds,
					codex_synced_cli_version,
					codex_cli_version_sync_enabled,
					codex_cli_version_sync_interval_hours,
					model_pricing_overrides,
					model_pricing_sync_url,
					ignore_usage_limit_status,
					auto_reset_credits_enabled,
					auto_reset_credits_before_expiry_min,
					prompt_filter_strict_terminal_enabled,
					prompt_filter_advanced_config,
					payload_rules,
					public_account_portal_page_enabled,
					codex_ws_size_router_enabled,
					codex_ws_busy_acquire_max_wait_sec,
					codex_ws_busy_overflow_enabled,
					codex_ws_busy_patience_sec,
					overflow_auto_compact_enabled,
					first_token_excludes_ws_acquire,
					codex_preflight_sse_passthrough_enabled,
					utls_shutdown_timeout_minutes,
					codex_ws_weak_network_mode,
					codex_fingerprint_default_mode,
					compact_via_responses_enabled,
					codex_ws_stateless_slots,
					github_token,
					github_proxy_url,
					codex_overload_pause_enabled,
					codex_overload_threshold_percent,
					codex_overload_pause_minutes,
					codex_overload_window_minutes,
					session_slot_buffer_enabled,
					session_slot_buffer_seconds,
					scheduler_engine,
					codex_request_compression,
					auto_activate_5h_window_enabled
					)
						VALUES (1, $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29, $30, $31, $32, $33, $34, $35, $36, $37, $38, $39, $40, $41, $42, $43, $44, $45, $46, $47, $48, $49, $50, $51, $52, $53, $54, $55, $56, $57, $58, $59, $60, $61, $62, $63, $64, $65, $66, $67, $68, $69, $70, $71, $72, $73, $74, $75, $76, $77, $78, $79, $80, $81, $82, $83, $84, $85, $86, $87, $88, $89, $90, $91, $92, $93, $94, $95, $96, $97, $98, $99, $100, $101, $102, $103, $104, $105, $106, $107, $108, $109, $110, $111, $112, $113, $114, $115, $116, $117, $118, $119)
				ON CONFLICT (id) DO UPDATE SET
				site_name               = EXCLUDED.site_name,
				site_logo               = EXCLUDED.site_logo,
				max_concurrency         = EXCLUDED.max_concurrency,
				global_rpm              = EXCLUDED.global_rpm,
				test_model              = EXCLUDED.test_model,
				test_content            = EXCLUDED.test_content,
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
				prompt_filter_custom_patterns = CASE WHEN $120 THEN system_settings.prompt_filter_custom_patterns ELSE EXCLUDED.prompt_filter_custom_patterns END,
				prompt_filter_disabled_patterns = EXCLUDED.prompt_filter_disabled_patterns,
				prompt_filter_review_enabled = EXCLUDED.prompt_filter_review_enabled,
				prompt_filter_review_api_key = CASE WHEN $121 THEN system_settings.prompt_filter_review_api_key ELSE EXCLUDED.prompt_filter_review_api_key END,
				prompt_filter_review_base_url = EXCLUDED.prompt_filter_review_base_url,
				prompt_filter_review_model = EXCLUDED.prompt_filter_review_model,
				prompt_filter_review_timeout_seconds = EXCLUDED.prompt_filter_review_timeout_seconds,
				prompt_filter_review_fail_closed = EXCLUDED.prompt_filter_review_fail_closed,
				client_compat_mode = EXCLUDED.client_compat_mode,
				codex_min_cli_version = EXCLUDED.codex_min_cli_version,
				codex_user_agent_config = EXCLUDED.codex_user_agent_config,
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
				session_affinity_spread = EXCLUDED.session_affinity_spread,
				background_config = EXCLUDED.background_config,
				grok_config = EXCLUDED.grok_config,
				show_full_usage_numbers = EXCLUDED.show_full_usage_numbers,
				public_key_usage_page_enabled = EXCLUDED.public_key_usage_page_enabled,
				public_image_studio_page_enabled = EXCLUDED.public_image_studio_page_enabled,
					reasoning_effort_models = EXCLUDED.reasoning_effort_models,
					codex_force_websocket = EXCLUDED.codex_force_websocket,
					codex_request_compression = EXCLUDED.codex_request_compression,
					codex_ws_keepalive_enabled = EXCLUDED.codex_ws_keepalive_enabled,
					codex_ws_keepalive_interval_sec = EXCLUDED.codex_ws_keepalive_interval_sec,
					codex_ws_hide_upstream_errors = EXCLUDED.codex_ws_hide_upstream_errors,
					codex_ws_silent_retry_enabled = EXCLUDED.codex_ws_silent_retry_enabled,
					codex_ws_silent_max_retries = EXCLUDED.codex_ws_silent_max_retries,
					auto_pause_5h_threshold = EXCLUDED.auto_pause_5h_threshold,
					auto_pause_7d_threshold = EXCLUDED.auto_pause_7d_threshold,
					auto_pause_5h_guard_band_percent = EXCLUDED.auto_pause_5h_guard_band_percent,
					auto_pause_5h_guard_concurrency = EXCLUDED.auto_pause_5h_guard_concurrency,
					smart_pacing_enabled = EXCLUDED.smart_pacing_enabled,
					smart_pacing_min_concurrency = EXCLUDED.smart_pacing_min_concurrency,
					smart_pacing_windows = EXCLUDED.smart_pacing_windows,
					retry_interval_ms = EXCLUDED.retry_interval_ms,
					transport_retry_policy = EXCLUDED.transport_retry_policy,
					codex_continue_thinking_enabled = EXCLUDED.codex_continue_thinking_enabled,
					codex_continue_max_rounds = EXCLUDED.codex_continue_max_rounds,
					codex_cli_version_sync_enabled = EXCLUDED.codex_cli_version_sync_enabled,
					codex_cli_version_sync_interval_hours = EXCLUDED.codex_cli_version_sync_interval_hours,
					ignore_usage_limit_status = EXCLUDED.ignore_usage_limit_status,
					auto_reset_credits_enabled = EXCLUDED.auto_reset_credits_enabled,
					auto_reset_credits_before_expiry_min = EXCLUDED.auto_reset_credits_before_expiry_min,
					prompt_filter_strict_terminal_enabled = EXCLUDED.prompt_filter_strict_terminal_enabled,
					prompt_filter_advanced_config = EXCLUDED.prompt_filter_advanced_config,
					payload_rules = EXCLUDED.payload_rules,
					public_account_portal_page_enabled = EXCLUDED.public_account_portal_page_enabled,
					codex_ws_size_router_enabled = EXCLUDED.codex_ws_size_router_enabled,
					codex_ws_busy_acquire_max_wait_sec = EXCLUDED.codex_ws_busy_acquire_max_wait_sec,
					codex_ws_busy_overflow_enabled = EXCLUDED.codex_ws_busy_overflow_enabled,
					codex_ws_busy_patience_sec = EXCLUDED.codex_ws_busy_patience_sec,
					overflow_auto_compact_enabled = EXCLUDED.overflow_auto_compact_enabled,
					first_token_excludes_ws_acquire = EXCLUDED.first_token_excludes_ws_acquire,
					codex_preflight_sse_passthrough_enabled = EXCLUDED.codex_preflight_sse_passthrough_enabled,
					utls_shutdown_timeout_minutes = EXCLUDED.utls_shutdown_timeout_minutes,
					codex_ws_weak_network_mode = EXCLUDED.codex_ws_weak_network_mode,
					codex_fingerprint_default_mode = EXCLUDED.codex_fingerprint_default_mode,
					compact_via_responses_enabled = EXCLUDED.compact_via_responses_enabled,
					codex_ws_stateless_slots = EXCLUDED.codex_ws_stateless_slots,
					github_token = EXCLUDED.github_token,
					github_proxy_url = EXCLUDED.github_proxy_url,
					codex_overload_pause_enabled = EXCLUDED.codex_overload_pause_enabled,
					codex_overload_threshold_percent = EXCLUDED.codex_overload_threshold_percent,
					codex_overload_pause_minutes = EXCLUDED.codex_overload_pause_minutes,
					codex_overload_window_minutes = EXCLUDED.codex_overload_window_minutes,
					session_slot_buffer_enabled = EXCLUDED.session_slot_buffer_enabled,
					session_slot_buffer_seconds = EXCLUDED.session_slot_buffer_seconds,
					scheduler_engine = EXCLUDED.scheduler_engine,
					auto_activate_5h_window_enabled = EXCLUDED.auto_activate_5h_window_enabled
			`, NormalizeSiteName(s.SiteName), strings.TrimSpace(s.SiteLogo),
		s.MaxConcurrency, s.GlobalRPM, s.TestModel, testContent, s.TestConcurrency, s.ProxyURL, s.PgMaxConns, s.RedisPoolSize,
		s.AutoCleanUnauthorized, s.AutoCleanRateLimited, s.AdminSecret, s.AutoCleanFullUsage, s.ProxyPoolEnabled,
		s.FastSchedulerEnabled, s.MaxRetries, s.MaxRateLimitRetries, s.AllowRemoteMigration, s.AutoCleanError, s.AutoCleanExpired, s.LazyMode, s.ModelMapping, s.CodexModelMapping,
		s.BackgroundRefreshIntervalMinutes, s.UsageProbeMaxAgeMinutes, s.RecoveryProbeIntervalMinutes,
		s.UsageProbeConcurrency, s.UsageProbeResponsesFallbackEnabled,
		s.ResinURL, s.ResinPlatformName, s.PromptFilterEnabled, s.PromptFilterMode, s.PromptFilterThreshold,
		s.PromptFilterStrictThreshold, s.PromptFilterLogMatches, s.PromptFilterMaxTextLength,
		s.PromptFilterSensitiveWords, s.PromptFilterCustomPatterns, s.PromptFilterDisabledPatterns,
		s.PromptFilterReviewEnabled, s.PromptFilterReviewAPIKey, s.PromptFilterReviewBaseURL,
		s.PromptFilterReviewModel, s.PromptFilterReviewTimeoutSeconds, s.PromptFilterReviewFailClosed,
		s.ClientCompatMode, s.CodexMinCLIVersion, codexUserAgentConfig, s.UsageLogMode, s.UsageLogBatchSize,
		s.UsageLogFlushIntervalSeconds, s.StreamFlushPolicy, s.StreamFlushIntervalMS,
		s.FirstTokenTimeoutSeconds, firstTokenMode, billingTierPolicy, s.ImageStorageConfig, s.SchedulerMode, normalizeAffinityMode(s.AffinityMode), s.SessionAffinitySpread, s.BackgroundConfig, normalizeGrokConfig(s.GrokConfig), s.ShowFullUsageNumbers, s.PublicKeyUsagePageEnabled, s.PublicImageStudioPageEnabled, reasoningEffortModels,
		s.CodexForceWebsocket, s.CodexWSKeepaliveEnabled, normalizeCodexWSKeepaliveInterval(s.CodexWSKeepaliveIntervalSec),
		s.CodexWSHideUpstreamErrors, s.CodexWSSilentRetryEnabled, normalizeCodexWSSilentMaxRetries(s.CodexWSSilentMaxRetries),
		s.AutoPause5hThreshold, s.AutoPause7dThreshold, s.AutoPause5hGuardBandPercent, s.AutoPause5hGuardConcurrency,
		s.SmartPacingEnabled, normalizeSmartPacingMinConcurrencyDB(s.SmartPacingMinConcurrency), normalizeSmartPacingWindowsDB(s.SmartPacingWindows),
		normalizeRetryIntervalMSDB(s.RetryIntervalMS), NormalizeTransportRetryPolicy(s.TransportRetryPolicy),
		s.CodexContinueThinkingEnabled, NormalizeCodexContinueMaxRounds(s.CodexContinueMaxRounds),
		strings.TrimSpace(s.CodexSyncedCLIVersion),
		s.CodexCLIVersionSyncEnabled, NormalizeCodexCLIVersionSyncIntervalHours(s.CodexCLIVersionSyncIntervalHours),
		normalizeModelPricingOverridesJSON(s.ModelPricingOverrides), strings.TrimSpace(s.ModelPricingSyncURL),
		s.IgnoreUsageLimitStatus, s.AutoResetCreditsEnabled,
		NormalizeAutoResetCreditsBeforeExpiryMinutes(s.AutoResetCreditsBeforeExpiryMin),
		s.PromptFilterStrictTerminalEnabled, s.PromptFilterAdvancedConfig, payloadRules, s.PublicAccountPortalPageEnabled,
		s.CodexWSSizeRouterEnabled,
		NormalizeCodexWSBusyAcquireMaxWaitSec(s.CodexWSBusyAcquireMaxWaitSec),
		s.CodexWSBusyOverflowEnabled,
		NormalizeCodexWSBusyPatienceSec(s.CodexWSBusyPatienceSec),
		s.OverflowAutoCompactEnabled,
		s.FirstTokenExcludesWsAcquire,
		s.CodexPreflightSSEPassthroughEnabled,
		NormalizeUTLSShutdownTimeoutMinutes(s.UTLSShutdownTimeoutMinutes),
		s.CodexWSWeakNetworkMode,
		NormalizeCodexFingerprintDefaultMode(s.CodexFingerprintDefaultMode),
		s.CompactViaResponsesEnabled,
		NormalizeCodexWSStatelessSlots(s.CodexWSStatelessSlots),
		strings.TrimSpace(s.GithubToken),
		strings.TrimSpace(s.GithubProxyURL),
		s.CodexOverloadPauseEnabled,
		NormalizeCodexOverloadThresholdPercent(s.CodexOverloadThresholdPercent),
		NormalizeCodexOverloadPauseMinutes(s.CodexOverloadPauseMinutes),
		NormalizeCodexOverloadWindowMinutes(s.CodexOverloadWindowMinutes),
		s.SessionSlotBufferEnabled,
		NormalizeSessionSlotBufferSeconds(s.SessionSlotBufferSeconds),
		NormalizeSchedulerEngine(s.SchedulerEngine, s.FastSchedulerEnabled),
		s.CodexRequestCompression,
		s.AutoActivate5hWindowEnabled,
		s.PreservePromptFilterCustomPatterns,
		s.PreservePromptFilterReviewAPIKey)
	return err
}

// UpdateCodexSyncedCLIVersion 只更新后台同步得到的 Codex CLI 版本，避免用
// 读取到的旧 SystemSettings 快照覆盖管理员刚保存的其他设置。
func (db *DB) UpdateCodexSyncedCLIVersion(ctx context.Context, version string) error {
	_, err := db.conn.ExecContext(ctx, `
		INSERT INTO system_settings (id, codex_synced_cli_version)
		VALUES (1, $1)
		ON CONFLICT (id) DO UPDATE SET
			codex_synced_cli_version = EXCLUDED.codex_synced_cli_version
	`, strings.TrimSpace(version))
	return err
}

// UpdateModelsListReadMaxBytes 原子更新模型列表读取上限，不回写整行设置。
func (db *DB) UpdateModelsListReadMaxBytes(ctx context.Context, value int64) error {
	if err := ValidateModelsListReadMaxBytes(value); err != nil {
		return err
	}
	_, err := db.conn.ExecContext(ctx, `
		INSERT INTO system_settings (id, models_list_read_max_bytes)
		VALUES (1, $1)
		ON CONFLICT (id) DO UPDATE SET
			models_list_read_max_bytes = EXCLUDED.models_list_read_max_bytes
	`, value)
	return err
}

// UpdateModelPricingSettings 原子更新模型定价覆盖及其同步来源，不回写整行设置。
func (db *DB) UpdateModelPricingSettings(ctx context.Context, overridesJSON, syncURL string) error {
	_, err := db.conn.ExecContext(ctx, `
		INSERT INTO system_settings (id, model_pricing_overrides, model_pricing_sync_url)
		VALUES (1, $1, $2)
		ON CONFLICT (id) DO UPDATE SET
			model_pricing_overrides = EXCLUDED.model_pricing_overrides,
			model_pricing_sync_url = EXCLUDED.model_pricing_sync_url
	`, normalizeModelPricingOverridesJSON(overridesJSON), strings.TrimSpace(syncURL))
	return err
}

// normalizeModelPricingOverridesJSON 空/非法 JSON 归一为 "{}"。
func normalizeModelPricingOverridesJSON(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return "{}"
	}
	if _, err := ParseModelPricingOverridesJSON(s); err != nil {
		return "{}"
	}
	return s
}

// NormalizeCodexCLIVersionSyncIntervalHours 把定时同步间隔(小时)限制在 1-720，非正值回落默认 12。
func NormalizeCodexCLIVersionSyncIntervalHours(hours int) int {
	if hours <= 0 {
		return 12
	}
	if hours > 720 {
		return 720
	}
	return hours
}

// normalizeCodexWSKeepaliveInterval 把 WS 保活间隔(秒)归一,非正值 → 默认 60。
func normalizeCodexWSKeepaliveInterval(sec int) int {
	if sec <= 0 {
		return 60
	}
	return sec
}

// normalizeRetryIntervalMSDB 把重试间隔限制在 0-30000ms(0 = 立即重试)。
func normalizeRetryIntervalMSDB(ms int) int {
	if ms < 0 {
		return 0
	}
	if ms > 30000 {
		return 30000
	}
	return ms
}

// NormalizeTransportRetryPolicy 归一化传输错误重试策略,空/未知值回落到 rotate(换号,旧行为)。
func NormalizeTransportRetryPolicy(policy string) string {
	switch strings.ToLower(strings.TrimSpace(policy)) {
	case "sticky":
		return "sticky"
	default:
		return "rotate"
	}
}

// NormalizeCodexWSBusyAcquireMaxWaitSec 把 busy 等待上限限制在 1-300 秒,非正值回落默认 30。
func NormalizeCodexWSBusyAcquireMaxWaitSec(seconds int) int {
	if seconds <= 0 {
		return 30
	}
	if seconds > 300 {
		return 300
	}
	return seconds
}

// NormalizeCodexWSBusyPatienceSec 把溢出前短等待限制在 0-300 秒,负值回落默认 2。
func NormalizeCodexWSBusyPatienceSec(seconds int) int {
	if seconds < 0 {
		return 2
	}
	if seconds > 300 {
		return 300
	}
	return seconds
}

// NormalizeCodexOverloadThresholdPercent 把过载熔断触发比例限制在 1-100，非正值回落默认 20。
func NormalizeCodexOverloadThresholdPercent(percent int) int {
	if percent <= 0 {
		return 20
	}
	if percent > 100 {
		return 100
	}
	return percent
}

// NormalizeCodexOverloadPauseMinutes 把过载暂停时长限制在 1-1440 分钟，非正值回落默认 30。
func NormalizeCodexOverloadPauseMinutes(minutes int) int {
	if minutes <= 0 {
		return 30
	}
	if minutes > 1440 {
		return 1440
	}
	return minutes
}

// NormalizeCodexOverloadWindowMinutes 把过载统计窗口限制在 1-120 分钟，非正值回落默认 5。
func NormalizeCodexOverloadWindowMinutes(minutes int) int {
	if minutes <= 0 {
		return 5
	}
	if minutes > 120 {
		return 120
	}
	return minutes
}

// NormalizeCodexWSStatelessSlots 把无状态 WS 连接槽位数限制在 1-32，非正值回落默认 8（issue #522）。
func NormalizeCodexWSStatelessSlots(slots int) int {
	if slots <= 0 {
		return 8
	}
	if slots > 32 {
		return 32
	}
	return slots
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

// UTLS 优雅关闭等待上限的边界（分钟，issue #446）。
const (
	defaultUTLSShutdownTimeoutMinutes = 30
	minUTLSShutdownTimeoutMinutes     = 1
	maxUTLSShutdownTimeoutMinutes     = 240
)

// NormalizeUTLSShutdownTimeoutMinutes 把 uTLS 连接优雅关闭的等待上限夹到
// 1-240 分钟，非正值回落默认 30。
func NormalizeUTLSShutdownTimeoutMinutes(minutes int) int {
	if minutes <= 0 {
		return defaultUTLSShutdownTimeoutMinutes
	}
	if minutes < minUTLSShutdownTimeoutMinutes {
		return minUTLSShutdownTimeoutMinutes
	}
	if minutes > maxUTLSShutdownTimeoutMinutes {
		return maxUTLSShutdownTimeoutMinutes
	}
	return minutes
}

// NormalizeCodexContinueMaxRounds 把续想最大轮数限制在 1-32,非正值回落默认 8。
func NormalizeCodexContinueMaxRounds(rounds int) int {
	if rounds <= 0 {
		return 8
	}
	if rounds > 32 {
		return 32
	}
	return rounds
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

// normalizeClaudeConfig 校验 claude_config JSON,非法或空则回落到默认 {}。
func normalizeClaudeConfig(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || !json.Valid([]byte(raw)) {
		return "{}"
	}
	return raw
}

// UpdateClaudeConfig 定向更新 claude_config 单列(不回写整行设置,避免触碰大 UPSERT)。
func (db *DB) UpdateClaudeConfig(ctx context.Context, raw string) error {
	value := normalizeClaudeConfig(raw)
	return db.withSQLiteWriteLock(ctx, func() error {
		_, err := db.conn.ExecContext(ctx, `
			INSERT INTO system_settings (id, claude_config) VALUES (1, $1)
			ON CONFLICT (id) DO UPDATE SET claude_config = EXCLUDED.claude_config`, value)
		return err
	})
}

// normalizeGrokConfig 校验 grok_config JSON,非法或空则回落到默认 {}。
func normalizeGrokConfig(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "{}"
	}
	if !json.Valid([]byte(raw)) {
		return "{}"
	}
	return raw
}

// DeleteAPIKey 删除 API 密钥
func (db *DB) DeleteAPIKey(ctx context.Context, id int64) error {
	err := db.withSQLiteWriteLock(ctx, func() error {
		tx, err := db.conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err := tx.ExecContext(ctx, `DELETE FROM prompt_filter_newapi_bindings WHERE api_key_id = $1`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM api_keys WHERE id = $1`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM api_key_model_request_counters WHERE api_key_id = $1`, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM api_key_model_request_ledger WHERE api_key_id = $1`, id); err != nil {
			return err
		}
		// scope 累计计数器没有外键约束，删 Key 时顺带清掉，避免留下永远不会再被读到的孤行。
		if _, err := tx.ExecContext(ctx, `DELETE FROM api_key_scope_counters WHERE api_key_id = $1`, id); err != nil {
			return err
		}
		return tx.Commit()
	})
	if err != nil {
		return err
	}
	db.InvalidateScopeQuotaKeyCache()
	return nil
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

const (
	ProxyTestStatusUntested = "untested"
	ProxyTestStatusSuccess  = "success"
	ProxyTestStatusError    = "error"
)

var ErrProxyTestTargetChanged = errors.New("proxy test target changed")

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
	TestStatus    string    `json:"test_status"`
	// BoundCount 是绑定到该代理的账号数,由列表接口按 proxy_url 聚合填充,
	// 前端据此免拉全量账号(代理页大号池卡死问题)。
	BoundCount int64                   `json:"bound_count"`
	RiskScore  *ProxyRiskScoreSnapshot `json:"risk_score,omitempty"`
}

// SetAccountProxyURLs 在单事务里批量更新账号的 proxy_url(代理均衡绑定)。
// 调用方负责同步运行时 store(ApplyAccountProxyURL)。
func (db *DB) SetAccountProxyURLs(ctx context.Context, assignments map[int64]string) error {
	if len(assignments) == 0 {
		return nil
	}
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	query := `UPDATE accounts SET proxy_url = $1, updated_at = CURRENT_TIMESTAMP WHERE id = $2`
	if db.isSQLite() {
		query = `UPDATE accounts SET proxy_url = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`
	}
	stmt, err := tx.PrepareContext(ctx, query)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for id, proxyURL := range assignments {
		if _, err := stmt.ExecContext(ctx, strings.TrimSpace(proxyURL), id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// CountAccountsByProxyURL 统计各 proxy_url 绑定的未删除账号数。
// 供代理列表页展示绑定数,替代前端拉全量账号自行聚合。
func (db *DB) CountAccountsByProxyURL(ctx context.Context) (map[string]int64, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT TRIM(proxy_url), COUNT(*)
		FROM accounts
		WHERE TRIM(COALESCE(proxy_url, '')) <> ''
		  AND status <> 'deleted'
		  AND COALESCE(error_message, '') <> 'deleted'
		GROUP BY TRIM(proxy_url)
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[string]int64)
	for rows.Next() {
		var url string
		var count int64
		if err := rows.Scan(&url, &count); err != nil {
			return nil, err
		}
		out[url] = count
	}
	return out, rows.Err()
}

// ListProxies 获取所有代理
func (db *DB) ListProxies(ctx context.Context) ([]*ProxyRow, error) {
	rows, err := db.conn.QueryContext(ctx, `SELECT id, url, label, enabled, created_at, COALESCE(test_ip,''), COALESCE(test_location,''), COALESCE(test_latency_ms,0), COALESCE(test_status,'untested') FROM proxies ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var proxies []*ProxyRow
	for rows.Next() {
		p := &ProxyRow{}
		var createdAtRaw interface{}
		if err := rows.Scan(&p.ID, &p.URL, &p.Label, &p.Enabled, &createdAtRaw, &p.TestIP, &p.TestLocation, &p.TestLatencyMs, &p.TestStatus); err != nil {
			return nil, err
		}
		p.CreatedAt, err = parseDBTimeValue(createdAtRaw)
		if err != nil {
			return nil, err
		}
		proxies = append(proxies, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	ids := make([]int64, 0, len(proxies))
	for _, proxy := range proxies {
		ids = append(ids, proxy.ID)
	}
	if scores, err := db.ListLatestProxyRiskScores(ctx, ids); err == nil {
		for _, proxy := range proxies {
			proxy.RiskScore = scores[proxy.ID]
		}
	}
	return proxies, nil
}

// GetProxy returns one proxy by ID.
func (db *DB) GetProxy(ctx context.Context, id int64) (*ProxyRow, error) {
	p := &ProxyRow{}
	var createdAtRaw interface{}
	err := db.conn.QueryRowContext(ctx, `
		SELECT id, url, label, enabled, created_at,
		       COALESCE(test_ip,''), COALESCE(test_location,''),
		       COALESCE(test_latency_ms,0), COALESCE(test_status,'untested')
		FROM proxies
		WHERE id = $1
	`, id).Scan(
		&p.ID,
		&p.URL,
		&p.Label,
		&p.Enabled,
		&createdAtRaw,
		&p.TestIP,
		&p.TestLocation,
		&p.TestLatencyMs,
		&p.TestStatus,
	)
	if err != nil {
		return nil, err
	}
	p.CreatedAt, err = parseDBTimeValue(createdAtRaw)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// ListProxiesByIDs returns only the requested proxy rows.
func (db *DB) ListProxiesByIDs(ctx context.Context, ids []int64) ([]*ProxyRow, error) {
	if len(ids) == 0 {
		return []*ProxyRow{}, nil
	}
	query := fmt.Sprintf(`
		SELECT id, url, label, enabled, created_at,
		       COALESCE(test_ip,''), COALESCE(test_location,''),
		       COALESCE(test_latency_ms,0), COALESCE(test_status,'untested')
		FROM proxies
		WHERE id IN (%s)
		ORDER BY id
	`, strings.Join(dbPlaceholders(db.isSQLite(), 1, len(ids)), ","))
	rows, err := db.conn.QueryContext(ctx, query, argsFromInt64s(ids)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	proxies := make([]*ProxyRow, 0, len(ids))
	for rows.Next() {
		p := &ProxyRow{}
		var createdAtRaw interface{}
		if err := rows.Scan(
			&p.ID,
			&p.URL,
			&p.Label,
			&p.Enabled,
			&createdAtRaw,
			&p.TestIP,
			&p.TestLocation,
			&p.TestLatencyMs,
			&p.TestStatus,
		); err != nil {
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
	query := `SELECT id, url, label, enabled, created_at, COALESCE(test_ip,''), COALESCE(test_location,''), COALESCE(test_latency_ms,0), COALESCE(test_status,'untested') FROM proxies WHERE enabled = true AND COALESCE(test_status,'untested') <> 'error' ORDER BY id`
	if db.isSQLite() {
		query = `SELECT id, url, label, enabled, created_at, COALESCE(test_ip,''), COALESCE(test_location,''), COALESCE(test_latency_ms,0), COALESCE(test_status,'untested') FROM proxies WHERE enabled = 1 AND COALESCE(test_status,'untested') <> 'error' ORDER BY id`
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
		if err := rows.Scan(&p.ID, &p.URL, &p.Label, &p.Enabled, &createdAtRaw, &p.TestIP, &p.TestLocation, &p.TestLatencyMs, &p.TestStatus); err != nil {
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
		normalizedURL := strings.TrimSpace(*urlValue)
		args = append(args, normalizedURL)
		urlPlaceholder := fmt.Sprintf("$%d", len(args))
		assignments = append(assignments,
			fmt.Sprintf("test_status = CASE WHEN url <> %s THEN 'untested' ELSE test_status END", urlPlaceholder),
			fmt.Sprintf("test_ip = CASE WHEN url <> %s THEN '' ELSE test_ip END", urlPlaceholder),
			fmt.Sprintf("test_location = CASE WHEN url <> %s THEN '' ELSE test_location END", urlPlaceholder),
			fmt.Sprintf("test_latency_ms = CASE WHEN url <> %s THEN 0 ELSE test_latency_ms END", urlPlaceholder),
			fmt.Sprintf("url = %s", urlPlaceholder),
		)
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

// UpdateProxyTestResult 仅在代理 URL 与测试目标仍一致时更新测试结果。
func (db *DB) UpdateProxyTestResult(ctx context.Context, id int64, expectedURL, status, ip, location string, latencyMs int) error {
	switch status {
	case ProxyTestStatusUntested, ProxyTestStatusSuccess, ProxyTestStatusError:
	default:
		return fmt.Errorf("invalid proxy test status: %q", status)
	}
	if status != ProxyTestStatusSuccess {
		ip = ""
		location = ""
		latencyMs = 0
	}
	res, err := db.conn.ExecContext(ctx,
		`UPDATE proxies SET test_status = $1, test_ip = $2, test_location = $3, test_latency_ms = $4 WHERE id = $5 AND url = $6`,
		status, ip, location, latencyMs, id, expectedURL)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrProxyTestTargetChanged
	}
	return nil
}

// ProxyErrorCleanupResult 汇总错误代理清理的持久化结果。
type ProxyErrorCleanupResult struct {
	Deleted           int
	Unbound           int
	UnboundAccountIDs []int64
	DeletedProxyURLs  []string
}

// CleanErrorProxies 删除全部测试错误的代理，并在同一事务中解绑引用它们的账号。
func (db *DB) CleanErrorProxies(ctx context.Context) (ProxyErrorCleanupResult, error) {
	var result ProxyErrorCleanupResult
	err := db.withSQLiteWriteLock(ctx, func() error {
		tx, err := db.conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()

		lockClause := ""
		if !db.isSQLite() {
			lockClause = " FOR UPDATE"
		}
		rows, err := tx.QueryContext(ctx, `
			SELECT id, TRIM(url)
			FROM proxies
			WHERE test_status = $1
			ORDER BY id`+lockClause,
			ProxyTestStatusError,
		)
		if err != nil {
			return err
		}
		var proxyIDs []int64
		var proxyURLs []string
		for rows.Next() {
			var id int64
			var proxyURL string
			if err := rows.Scan(&id, &proxyURL); err != nil {
				rows.Close()
				return err
			}
			proxyIDs = append(proxyIDs, id)
			proxyURLs = append(proxyURLs, proxyURL)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(proxyIDs) == 0 {
			return tx.Commit()
		}

		unbindArgs := make([]interface{}, len(proxyURLs))
		for i, proxyURL := range proxyURLs {
			unbindArgs[i] = proxyURL
		}
		unbindQuery := fmt.Sprintf(`
			UPDATE accounts
			SET proxy_url = '', updated_at = CURRENT_TIMESTAMP
			WHERE TRIM(COALESCE(proxy_url, '')) IN (%s)
			RETURNING id
		`, strings.Join(dbPlaceholders(db.isSQLite(), 1, len(proxyURLs)), ","))
		rows, err = tx.QueryContext(ctx, unbindQuery, unbindArgs...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var accountID int64
			if err := rows.Scan(&accountID); err != nil {
				rows.Close()
				return err
			}
			result.UnboundAccountIDs = append(result.UnboundAccountIDs, accountID)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		result.Unbound = len(result.UnboundAccountIDs)

		deleteQuery := fmt.Sprintf(
			`DELETE FROM proxies WHERE id IN (%s) RETURNING id, TRIM(url)`,
			strings.Join(dbPlaceholders(db.isSQLite(), 1, len(proxyIDs)), ","),
		)
		rows, err = tx.QueryContext(ctx, deleteQuery, argsFromInt64s(proxyIDs)...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var proxyID int64
			var proxyURL string
			if err := rows.Scan(&proxyID, &proxyURL); err != nil {
				rows.Close()
				return err
			}
			result.Deleted++
			result.DeletedProxyURLs = append(result.DeletedProxyURLs, proxyURL)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}

		return tx.Commit()
	})
	if err != nil {
		return ProxyErrorCleanupResult{}, err
	}
	return result, nil
}

func scanUnboundAccountIDs(rows *sql.Rows) ([]int64, error) {
	var ids []int64
	for rows.Next() {
		var accountID int64
		if err := rows.Scan(&accountID); err != nil {
			return nil, err
		}
		ids = append(ids, accountID)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ids, nil
}

func unbindAccountsFromProxyURLsTx(ctx context.Context, tx *sql.Tx, sqlite bool, proxyURLs []string) ([]int64, error) {
	if len(proxyURLs) == 0 {
		return nil, nil
	}
	args := make([]interface{}, len(proxyURLs))
	for i, proxyURL := range proxyURLs {
		args[i] = proxyURL
	}
	query := fmt.Sprintf(`
		UPDATE accounts
		SET proxy_url = '', updated_at = CURRENT_TIMESTAMP
		WHERE TRIM(COALESCE(proxy_url, '')) IN (%s)
		RETURNING id
	`, strings.Join(dbPlaceholders(sqlite, 1, len(proxyURLs)), ","))
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanUnboundAccountIDs(rows)
}

// RetireProxiesByIDs 删除指定代理，并在同一事务中解绑仍引用这些 URL 的账号。
// 用于单条/批量删除，避免删除后账号 proxy_url 继续把流量打到已不存在的出口(issue #517)。
func (db *DB) RetireProxiesByIDs(ctx context.Context, ids []int64) (ProxyErrorCleanupResult, error) {
	var result ProxyErrorCleanupResult
	if len(ids) == 0 {
		return result, nil
	}
	err := db.withSQLiteWriteLock(ctx, func() error {
		tx, err := db.conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()

		lockClause := ""
		if !db.isSQLite() {
			lockClause = " FOR UPDATE"
		}
		selectQuery := fmt.Sprintf(`
			SELECT id, TRIM(url)
			FROM proxies
			WHERE id IN (%s)
			ORDER BY id`+lockClause,
			strings.Join(dbPlaceholders(db.isSQLite(), 1, len(ids)), ","),
		)
		rows, err := tx.QueryContext(ctx, selectQuery, argsFromInt64s(ids)...)
		if err != nil {
			return err
		}
		var proxyIDs []int64
		var proxyURLs []string
		for rows.Next() {
			var id int64
			var proxyURL string
			if err := rows.Scan(&id, &proxyURL); err != nil {
				rows.Close()
				return err
			}
			proxyIDs = append(proxyIDs, id)
			proxyURLs = append(proxyURLs, proxyURL)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(proxyIDs) == 0 {
			return tx.Commit()
		}

		unboundIDs, err := unbindAccountsFromProxyURLsTx(ctx, tx, db.isSQLite(), proxyURLs)
		if err != nil {
			return err
		}
		result.UnboundAccountIDs = unboundIDs
		result.Unbound = len(unboundIDs)

		deleteQuery := fmt.Sprintf(
			`DELETE FROM proxies WHERE id IN (%s) RETURNING id, TRIM(url)`,
			strings.Join(dbPlaceholders(db.isSQLite(), 1, len(proxyIDs)), ","),
		)
		rows, err = tx.QueryContext(ctx, deleteQuery, argsFromInt64s(proxyIDs)...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var proxyID int64
			var proxyURL string
			if err := rows.Scan(&proxyID, &proxyURL); err != nil {
				rows.Close()
				return err
			}
			result.Deleted++
			result.DeletedProxyURLs = append(result.DeletedProxyURLs, proxyURL)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		return tx.Commit()
	})
	if err != nil {
		return ProxyErrorCleanupResult{}, err
	}
	return result, nil
}

// RebindAccountProxyURLs 把仍指向 oldURL 的账号绑定改写为 newURL（代理行改 URL 时跟上）。
func (db *DB) RebindAccountProxyURLs(ctx context.Context, oldURL, newURL string) ([]int64, error) {
	oldURL = strings.TrimSpace(oldURL)
	newURL = strings.TrimSpace(newURL)
	if oldURL == "" || newURL == "" || oldURL == newURL {
		return nil, nil
	}
	var ids []int64
	err := db.withSQLiteWriteLock(ctx, func() error {
		rows, err := db.conn.QueryContext(ctx, `
			UPDATE accounts
			SET proxy_url = $1, updated_at = CURRENT_TIMESTAMP
			WHERE TRIM(COALESCE(proxy_url, '')) = $2
			RETURNING id
		`, newURL, oldURL)
		if err != nil {
			return err
		}
		defer rows.Close()
		ids, err = scanUnboundAccountIDs(rows)
		return err
	})
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// ==================== Usage Logs（批量写入） ====================

// UsageLog 请求日志行
type UsageLog struct {
	ID                     int64     `json:"id"`
	AccountID              int64     `json:"account_id"`
	CredentialGeneration   int64     `json:"credential_generation,omitempty"`
	Channel                string    `json:"channel,omitempty"`
	ClientIP               string    `json:"client_ip"`
	ClientUserAgent        string    `json:"client_user_agent"`
	UpstreamUserAgent      string    `json:"upstream_user_agent"`
	UserAgentOverridden    bool      `json:"user_agent_overridden"`
	InternalReason         string    `json:"internal_reason"`
	ParentRequestID        string    `json:"parent_request_id"`
	Endpoint               string    `json:"endpoint"`
	Model                  string    `json:"model"`
	EffectiveModel         string    `json:"effective_model"`
	PromptTokens           int       `json:"prompt_tokens"`
	CompletionTokens       int       `json:"completion_tokens"`
	TotalTokens            int       `json:"total_tokens"`
	StatusCode             int       `json:"status_code"`
	DurationMs             int       `json:"duration_ms"`
	InputTokens            int       `json:"input_tokens"`
	OutputTokens           int       `json:"output_tokens"`
	ReasoningTokens        int       `json:"reasoning_tokens"`
	FirstTokenMs           int       `json:"first_token_ms"`
	WsAcquireMs            int       `json:"ws_acquire_ms"`
	ReasoningEffort        string    `json:"reasoning_effort"`
	InboundEndpoint        string    `json:"inbound_endpoint"`
	UpstreamEndpoint       string    `json:"upstream_endpoint"`
	Stream                 bool      `json:"stream"`
	Compact                bool      `json:"compact"`
	HasCompactionHistory   bool      `json:"has_compaction_history"`
	ViaWebsocket           bool      `json:"via_websocket"`
	CachedTokens           int       `json:"cached_tokens"`
	CacheWrite5mTokens     int       `json:"cache_write_5m_tokens"`
	CacheWrite1hTokens     int       `json:"cache_write_1h_tokens"`
	ServiceTier            string    `json:"service_tier"`
	RequestedServiceTier   string    `json:"requested_service_tier"`
	ActualServiceTier      string    `json:"actual_service_tier"`
	BillingServiceTier     string    `json:"billing_service_tier"`
	APIKeyID               int64     `json:"api_key_id"`
	APIKeyName             string    `json:"api_key_name"`
	APIKeyMasked           string    `json:"api_key_masked"`
	ImageCount             int       `json:"image_count"`
	ImageWidth             int       `json:"image_width"`
	ImageHeight            int       `json:"image_height"`
	ImageBytes             int       `json:"image_bytes"`
	ImageFormat            string    `json:"image_format"`
	ImageSize              string    `json:"image_size"`
	AccountName            string    `json:"account_name"`
	AccountEmail           string    `json:"account_email"`
	CreatedAt              time.Time `json:"created_at"`
	AccountBilled          float64   `json:"account_billed"`
	UserBilled             float64   `json:"user_billed"`
	InputCost              float64   `json:"input_cost"`
	OutputCost             float64   `json:"output_cost"`
	CacheReadCost          float64   `json:"cache_read_cost"`
	CacheWrite5mCost       float64   `json:"cache_write_5m_cost"`
	CacheWrite1hCost       float64   `json:"cache_write_1h_cost"`
	CacheWrite5mPrice      float64   `json:"cache_write_5m_price_per_mtoken"`
	CacheWrite1hPrice      float64   `json:"cache_write_1h_price_per_mtoken"`
	TotalCost              float64   `json:"total_cost"`
	InputPrice             float64   `json:"input_price_per_mtoken"`
	OutputPrice            float64   `json:"output_price_per_mtoken"`
	CacheReadPrice         float64   `json:"cache_read_price_per_mtoken"`
	RateMultiplier         float64   `json:"rate_multiplier"`
	LongContext            bool      `json:"long_context"`
	LongContextThreshold   int       `json:"long_context_threshold"`
	IsRetryAttempt         bool      `json:"is_retry_attempt"`
	AttemptIndex           int       `json:"attempt_index"`
	UpstreamErrorKind      string    `json:"upstream_error_kind"`
	ErrorMessage           string    `json:"error_message"`
	PromptPolicyIncidentID string    `json:"prompt_policy_incident_id,omitempty"`
}

// usage_logs 中受 varchar 长度约束的列宽。这些字段大多直接来自下游请求体或上游响应
// （reasoning_effort、service_tier、image_format 等），长度不受网关控制。一条超长值会让
// 整条批量 INSERT 回滚，失败的 batch 又会被原样放回缓冲区头部，下一轮继续失败——
// 单条脏数据就能永久堵死整个日志写入。因此写入前按列宽截断。
const (
	usageLogChannelMaxLen    = 16  // channel
	usageLogImageSizeMaxLen  = 32  // image_size
	usageLogShortTextMaxLen  = 64  // client_ip / api_key_masked / upstream_error_kind
	usageLogTextMaxLen       = 100 // endpoint / model / *_service_tier / reasoning_effort ...
	usageLogRequestIDMaxLen  = 128 // parent_request_id
	usageLogAPIKeyNameMaxLen = 255 // api_key_name
)

// clampUsageLogText 按字符数截断：PostgreSQL varchar(n) 限制的是字符而非字节，
// 且按字节切会切出非法 UTF-8 序列。
func clampUsageLogText(s string, maxRunes int) string {
	if maxRunes <= 0 || len(s) <= maxRunes {
		return s
	}
	if utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	count := 0
	for i := range s {
		count++
		if count > maxRunes {
			return s[:i]
		}
	}
	return s
}

// trimUsageLogBufferLocked 把缓冲裁到硬上限以内，丢最旧的日志。调用方必须持有 logMu。
// 丢弃的日志同时会丢掉它们那份 API Key 额度累加（额度计数器和日志在同一个事务里落库），
// 这是过载/长时间断库下的取舍：宁可少记一段用量，也不能让进程 OOM。
func (db *DB) trimUsageLogBufferLocked() {
	overflow := len(db.logBuf) - usageLogBufferHardLimit
	if overflow <= 0 {
		return
	}
	db.logBuf = append(db.logBuf[:0], db.logBuf[overflow:]...)
	total := atomic.AddInt64(&db.usageLogDropped, int64(overflow))
	if now := time.Now(); now.Sub(db.usageLogDropLogAt) >= 30*time.Second {
		db.usageLogDropLogAt = now
		log.Printf("用量日志缓冲已达上限 %d 条，丢弃最旧的 %d 条（累计丢弃 %d 条），请检查数据库是否可写",
			usageLogBufferHardLimit, overflow, total)
	}
}

// InsertUsageLog 将用量事件追加到内存缓冲（非阻塞）。
func (db *DB) InsertUsageLog(ctx context.Context, log *UsageLogInput) error {
	if db == nil || log == nil {
		return nil
	}
	storeUsageLog := db.shouldStoreUsageLog(log)

	billingServiceTier := usageLogBillingServiceTier(log)

	serviceTier := log.ServiceTier
	if serviceTier == "" {
		serviceTier = log.ActualServiceTier
	}
	if serviceTier == "" {
		serviceTier = log.RequestedServiceTier
	}

	// 计算账号计费金额（基于实际计费 service tier）
	accountBilled := UsageLogBilledCost(log)

	// 用户计费金额与账号计费金额相同（简化版，未来可支持倍率）
	userBilled := accountBilled
	if !storeUsageLog && (log.APIKeyID <= 0 || userBilled <= 0 || log.StatusCode == 499) {
		return nil
	}

	db.logMu.Lock()
	db.logBuf = append(db.logBuf, usageLogEntry{
		StoreUsageLog:          storeUsageLog,
		AccountID:              log.AccountID,
		CredentialGeneration:   log.CredentialGeneration,
		Channel:                clampUsageLogText(log.Channel, usageLogChannelMaxLen),
		ClientIP:               clampUsageLogText(log.ClientIP, usageLogShortTextMaxLen),
		ClientUserAgent:        log.ClientUserAgent,
		UpstreamUserAgent:      log.UpstreamUserAgent,
		UserAgentOverridden:    log.UserAgentOverridden,
		InternalReason:         clampUsageLogText(log.InternalReason, usageLogShortTextMaxLen),
		ParentRequestID:        clampUsageLogText(log.ParentRequestID, usageLogRequestIDMaxLen),
		Endpoint:               clampUsageLogText(log.Endpoint, usageLogTextMaxLen),
		Model:                  clampUsageLogText(log.Model, usageLogTextMaxLen),
		EffectiveModel:         clampUsageLogText(log.EffectiveModel, usageLogTextMaxLen),
		PromptTokens:           log.PromptTokens,
		CompletionTokens:       log.CompletionTokens,
		TotalTokens:            log.TotalTokens,
		StatusCode:             log.StatusCode,
		DurationMs:             log.DurationMs,
		InputTokens:            log.InputTokens,
		OutputTokens:           log.OutputTokens,
		ReasoningTokens:        log.ReasoningTokens,
		FirstTokenMs:           log.FirstTokenMs,
		WsAcquireMs:            log.WsAcquireMs,
		ReasoningEffort:        clampUsageLogText(log.ReasoningEffort, usageLogTextMaxLen),
		InboundEndpoint:        clampUsageLogText(log.InboundEndpoint, usageLogTextMaxLen),
		UpstreamEndpoint:       clampUsageLogText(log.UpstreamEndpoint, usageLogTextMaxLen),
		Stream:                 log.Stream,
		Compact:                log.Compact,
		HasCompactionHistory:   log.HasCompactionHistory,
		ViaWebsocket:           log.ViaWebsocket,
		CachedTokens:           log.CachedTokens,
		CacheWrite5mTokens:     log.CacheWrite5mTokens,
		CacheWrite1hTokens:     log.CacheWrite1hTokens,
		ServiceTier:            clampUsageLogText(serviceTier, usageLogTextMaxLen),
		RequestedServiceTier:   clampUsageLogText(log.RequestedServiceTier, usageLogTextMaxLen),
		ActualServiceTier:      clampUsageLogText(log.ActualServiceTier, usageLogTextMaxLen),
		BillingServiceTier:     clampUsageLogText(billingServiceTier, usageLogTextMaxLen),
		APIKeyID:               log.APIKeyID,
		APIKeyName:             clampUsageLogText(log.APIKeyName, usageLogAPIKeyNameMaxLen),
		APIKeyMasked:           clampUsageLogText(log.APIKeyMasked, usageLogShortTextMaxLen),
		ImageCount:             log.ImageCount,
		ImageWidth:             log.ImageWidth,
		ImageHeight:            log.ImageHeight,
		ImageBytes:             log.ImageBytes,
		ImageFormat:            clampUsageLogText(log.ImageFormat, usageLogTextMaxLen),
		ImageSize:              clampUsageLogText(log.ImageSize, usageLogImageSizeMaxLen),
		AccountBilled:          accountBilled,
		UserBilled:             userBilled,
		IsRetryAttempt:         log.IsRetryAttempt,
		AttemptIndex:           log.AttemptIndex,
		UpstreamErrorKind:      clampUsageLogText(log.UpstreamErrorKind, usageLogShortTextMaxLen),
		ErrorMessage:           log.ErrorMessage,
		PromptPolicyIncidentID: clampUsageLogText(log.PromptPolicyIncidentID, usageLogShortTextMaxLen),
	})
	db.trimUsageLogBufferLocked()
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
	AccountID int64
	// CredentialGeneration attributes internally-generated Grok traffic to the
	// credential snapshot that issued it. Zero is legacy/unscoped traffic.
	CredentialGeneration int64
	// Channel 是处理该请求的上游渠道（codex/grok），写入时固化，空值表示未知。
	Channel                string
	ClientIP               string
	ClientUserAgent        string
	UpstreamUserAgent      string
	UserAgentOverridden    bool
	InternalReason         string
	ParentRequestID        string
	Endpoint               string
	Model                  string
	EffectiveModel         string
	PromptTokens           int
	CompletionTokens       int
	TotalTokens            int
	StatusCode             int
	DurationMs             int
	InputTokens            int
	OutputTokens           int
	ReasoningTokens        int
	FirstTokenMs           int
	WsAcquireMs            int
	ReasoningEffort        string
	InboundEndpoint        string
	UpstreamEndpoint       string
	Stream                 bool
	Compact                bool
	HasCompactionHistory   bool
	ViaWebsocket           bool
	CachedTokens           int
	CacheWrite5mTokens     int
	CacheWrite1hTokens     int
	ServiceTier            string
	RequestedServiceTier   string
	ActualServiceTier      string
	BillingServiceTier     string
	APIKeyID               int64
	APIKeyName             string
	APIKeyMasked           string
	ImageCount             int
	ImageWidth             int
	ImageHeight            int
	ImageBytes             int
	ImageFormat            string
	ImageSize              string
	IsRetryAttempt         bool
	AttemptIndex           int
	UpstreamErrorKind      string
	ErrorMessage           string
	PromptPolicyIncidentID string
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
	breakdown := CalculateCostBreakdownWithCacheWrites(l.InputTokens, l.OutputTokens, l.CachedTokens, l.CacheWrite5mTokens, l.CacheWrite1hTokens, billingModel, billingServiceTier)
	l.InputCost = breakdown.InputCost
	l.OutputCost = breakdown.OutputCost
	l.CacheReadCost = breakdown.CacheReadCost
	l.CacheWrite5mCost = breakdown.CacheWrite5mCost
	l.CacheWrite1hCost = breakdown.CacheWrite1hCost
	l.CacheWrite5mPrice = breakdown.CacheWrite5mPricePerMToken
	l.CacheWrite1hPrice = breakdown.CacheWrite1hPricePerMToken
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
		l.CacheWrite5mCost *= scale
		l.CacheWrite1hCost *= scale
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

// FlushUsageLogs 同步刷完当前用量日志缓冲。正常运行由后台 flusher 按批/按间隔触发，
// 这里给需要「写入后立刻可查」的调用方（测试、诊断路径）一个确定性入口。
func (db *DB) FlushUsageLogs() {
	if db == nil {
		return
	}
	for db.flushLogBatch(true) {
	}
}

// flushLogs 将缓冲中的日志按配置批量写入 PG。
// 高并发下 logBuf 可能在一个 flush 间隔内积累到数千条；这里每次只取
// usage_log_batch_size，避免一次事务过大，也避免 PostgreSQL 65535 bind 参数上限。
func (db *DB) flushLogs() {
	db.flushLogBatch(false)
}

func (db *DB) flushLogBatch(drain bool) bool {
	if db == nil {
		return false
	}
	batchSize := db.GetUsageLogBatchSize()
	if batchSize <= 0 {
		batchSize = defaultUsageLogBatchSize
	}

	db.logMu.Lock()
	if len(db.logBuf) == 0 {
		db.logMu.Unlock()
		return false
	}
	take := len(db.logBuf)
	if take > batchSize {
		take = batchSize
	}
	batch := make([]usageLogEntry, take)
	copy(batch, db.logBuf[:take])
	remaining := len(db.logBuf) - take
	if remaining == 0 {
		db.logBuf = make([]usageLogEntry, 0, batchSize)
	} else {
		next := make([]usageLogEntry, remaining, remaining+batchSize)
		copy(next, db.logBuf[take:])
		db.logBuf = next
	}
	db.logMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second) // 增加超时时间
	defer cancel()

	if err := db.withSQLiteWriteLock(ctx, func() error {
		return db.insertUsageLogBatch(ctx, batch)
	}); err != nil {
		// 瞬时故障（连接断开、超时、死锁）原样放回缓冲区重试，一条都不能丢。
		if !isUsageLogDataError(err) {
			log.Printf("批量写入日志失败，已重新放回缓冲区等待重试: %v", err)
			db.requeueUsageLogBatch(batch)
			return false
		}
		// 脏数据重试多少次都写不进去，隔离出来丢掉，其余照常落库。
		pending, dropped := db.salvageUsageLogBatch(ctx, batch, err)
		if dropped > 0 {
			total := atomic.AddInt64(&db.usageLogDropped, int64(dropped))
			log.Printf("批量写入命中写不进去的日志：已丢弃 %d 条（累计 %d 条），其余继续落库。首个错误: %v",
				dropped, total, err)
		}
		if len(pending) > 0 {
			log.Printf("批量写入日志部分失败，%d 条已放回缓冲区等待重试", len(pending))
			db.requeueUsageLogBatch(pending)
			return false
		}
	}

	if storedLogCount := countStoredUsageLogs(batch); storedLogCount > 10 {
		log.Printf("批量写入 %d 条使用日志", storedLogCount)
	}
	if remaining > 0 {
		if drain {
			return true
		}
		db.notifyLogFlush()
	}
	return false
}

// insertUsageLogBatch 按驱动把一批日志写进去。整批是一个事务：日志行、API Key 累计额度
// 计数器、api_keys.quota_used 要么一起成功，要么一起回滚。
func (db *DB) insertUsageLogBatch(ctx context.Context, batch []usageLogEntry) error {
	if db.driver == "postgres" {
		return db.batchInsertLogs(ctx, batch)
	}
	return db.insertSQLiteUsageLogBatch(ctx, batch)
}

// isUsageLogDataError 判断失败是不是「这批数据本身写不进去」。PostgreSQL 的 SQLSTATE
// class 22（数据异常：超长、非法 UTF-8 字节、数值溢出…）和 class 23（约束冲突）重试多少次
// 都不会成功；其余错误（连接断开、超时、死锁、只读事务）是瞬时故障，必须继续重试，
// 绝不能顺手把日志丢掉。
func isUsageLogDataError(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || len(pgErr.Code) < 2 {
		return false
	}
	switch pgErr.Code[:2] {
	case "22", "23":
		return true
	}
	return false
}

// salvageUsageLogBatch 二分隔离写不进去的日志：能写的照常落库，脏数据丢掉并计数，
// 途中遇到瞬时故障就把还没落库的部分交回调用方重试（已经写进去的不会再交回，避免重复计费）。
//
// 不做隔离的话，一条脏数据会让整批回滚、原样放回缓冲区头部、下一轮继续失败——日志永久停更，
// 同一事务里的 API Key 额度计数器也跟着冻结，带预算的 Key 会一直判定为未超额。
func (db *DB) salvageUsageLogBatch(ctx context.Context, batch []usageLogEntry, cause error) (pending []usageLogEntry, dropped int) {
	return salvageUsageLogBatchWith(batch, cause,
		func(chunk []usageLogEntry) error { return db.insertUsageLogBatch(ctx, chunk) },
		func(e usageLogEntry, err error) {
			log.Printf("丢弃 1 条写不进去的用量日志(endpoint=%s model=%s status=%d api_key_id=%d): %v",
				e.Endpoint, e.Model, e.StatusCode, e.APIKeyID, err)
		})
}

func salvageUsageLogBatchWith(
	batch []usageLogEntry,
	cause error,
	insert func([]usageLogEntry) error,
	onDrop func(usageLogEntry, error),
) (pending []usageLogEntry, dropped int) {
	if len(batch) == 0 {
		return nil, 0
	}
	if len(batch) == 1 {
		onDrop(batch[0], cause)
		return nil, 1
	}

	mid := len(batch) / 2
	for _, half := range [][]usageLogEntry{batch[:mid], batch[mid:]} {
		err := insert(half)
		if err == nil {
			continue
		}
		if !isUsageLogDataError(err) {
			pending = append(pending, half...)
			continue
		}
		halfPending, halfDropped := salvageUsageLogBatchWith(half, err, insert, onDrop)
		pending = append(pending, halfPending...)
		dropped += halfDropped
	}
	return pending, dropped
}

func countStoredUsageLogs(batch []usageLogEntry) int {
	count := 0
	for _, entry := range batch {
		if entry.StoreUsageLog {
			count++
		}
	}
	return count
}

func storedUsageLogs(batch []usageLogEntry) []usageLogEntry {
	storedCount := countStoredUsageLogs(batch)
	if storedCount == len(batch) {
		return batch
	}
	stored := make([]usageLogEntry, 0, storedCount)
	for _, entry := range batch {
		if entry.StoreUsageLog {
			stored = append(stored, entry)
		}
	}
	return stored
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
		db.trimUsageLogBufferLocked()
		return
	}

	requeued := make([]usageLogEntry, 0, len(batch)+len(db.logBuf))
	requeued = append(requeued, batch...)
	requeued = append(requeued, db.logBuf...)
	db.logBuf = requeued
	db.trimUsageLogBufferLocked()
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

	// usage_log_mode 只过滤审计行；额度仍按完整 batch 在同一事务内更新。
	logsToStore := storedUsageLogs(batch)
	if len(logsToStore) > 0 {
		stmt, err := tx.PrepareContext(ctx,
			`INSERT INTO usage_logs (account_id, credential_generation, channel, client_ip, endpoint, model, effective_model, prompt_tokens, completion_tokens, total_tokens, status_code, duration_ms,
				  input_tokens, output_tokens, reasoning_tokens, first_token_ms, ws_acquire_ms, reasoning_effort, inbound_endpoint, upstream_endpoint, stream, compact, has_compaction_history, cached_tokens, cache_write_5m_tokens, cache_write_1h_tokens, service_tier,
				  requested_service_tier, actual_service_tier, billing_service_tier,
				  api_key_id, api_key_name, api_key_masked, image_count, image_width, image_height, image_bytes, image_format, image_size, account_billed, user_billed,
				  is_retry_attempt, attempt_index, upstream_error_kind, error_message, via_websocket,
				  client_user_agent, upstream_user_agent, user_agent_overridden, internal_reason, parent_request_id, prompt_policy_incident_id)
				 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29, $30, $31, $32, $33, $34, $35, $36, $37, $38, $39, $40, $41, $42, $43, $44, $45, $46, $47, $48, $49, $50, $51, $52)`)
		if err != nil {
			return fmt.Errorf("准备语句: %w", err)
		}
		defer stmt.Close()

		for _, e := range logsToStore {
			if _, err := stmt.ExecContext(ctx, e.AccountID, e.CredentialGeneration, e.Channel, e.ClientIP, e.Endpoint, e.Model, e.EffectiveModel, e.PromptTokens, e.CompletionTokens, e.TotalTokens, e.StatusCode, e.DurationMs,
				e.InputTokens, e.OutputTokens, e.ReasoningTokens, e.FirstTokenMs, e.WsAcquireMs, e.ReasoningEffort, e.InboundEndpoint, e.UpstreamEndpoint, e.Stream, e.Compact, e.HasCompactionHistory, e.CachedTokens, e.CacheWrite5mTokens, e.CacheWrite1hTokens, e.ServiceTier,
				e.RequestedServiceTier, e.ActualServiceTier, e.BillingServiceTier,
				e.APIKeyID, e.APIKeyName, e.APIKeyMasked, e.ImageCount, e.ImageWidth, e.ImageHeight, e.ImageBytes, e.ImageFormat, e.ImageSize, e.AccountBilled, e.UserBilled,
				e.IsRetryAttempt, e.AttemptIndex, e.UpstreamErrorKind, e.ErrorMessage, e.ViaWebsocket,
				e.ClientUserAgent, e.UpstreamUserAgent, e.UserAgentOverridden, e.InternalReason, e.ParentRequestID, nullablePromptPolicyIncidentID(e.PromptPolicyIncidentID)); err != nil {
				return fmt.Errorf("执行插入: %w", err)
			}
		}
	}

	if err := db.applyAPIKeyScopeCountersWithExec(ctx, tx, batch); err != nil {
		return fmt.Errorf("更新 scope 累计额度: %w", err)
	}
	if err := db.applyAPIKeyQuotaUsageWithExec(ctx, tx, batch); err != nil {
		return fmt.Errorf("更新 API Key 额度用量: %w", err)
	}
	if err := applyUsageStatsRollupWithExec(ctx, tx, logsToStore); err != nil {
		return fmt.Errorf("更新用量累计汇总: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交事务: %w", err)
	}
	return nil
}

// batchInsertLogs 使用 PostgreSQL 的批量插入优化。
// PostgreSQL 单条语句最多 65535 个 bind 参数；usage_logs 当前每行 47 个参数，
// 因此单条 INSERT 的行数必须稳定低于 floor(65535/47)=1394。
func (db *DB) batchInsertLogs(ctx context.Context, batch []usageLogEntry) error {
	if len(batch) == 0 {
		return nil
	}

	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开始事务: %w", err)
	}
	defer tx.Rollback()

	logsToStore := storedUsageLogs(batch)
	maxRowsPerBatch := maxUsageLogInsertRowsPerSQL
	if paramSafeRows := postgresMaxBindParams / usageLogInsertColumnCount; paramSafeRows > 0 && maxRowsPerBatch > paramSafeRows {
		maxRowsPerBatch = paramSafeRows
	}

	// 分批处理
	for start := 0; start < len(logsToStore); start += maxRowsPerBatch {
		end := start + maxRowsPerBatch
		if end > len(logsToStore) {
			end = len(logsToStore)
		}
		subBatch := logsToStore[start:end]

		if err := db.batchInsertLogsChunk(ctx, tx, subBatch); err != nil {
			return err
		}
	}
	if err := db.applyAPIKeyScopeCountersWithExec(ctx, tx, batch); err != nil {
		return fmt.Errorf("更新 scope 累计额度: %w", err)
	}
	if err := db.applyAPIKeyQuotaUsageWithExec(ctx, tx, batch); err != nil {
		return err
	}
	if err := applyUsageStatsRollupWithExec(ctx, tx, logsToStore); err != nil {
		return fmt.Errorf("更新用量累计汇总: %w", err)
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
	valueArgs := make([]interface{}, 0, len(batch)*usageLogInsertColumnCount)
	argIdx := 1

	for _, e := range batch {
		placeholders := make([]string, usageLogInsertColumnCount)
		for i := range placeholders {
			placeholders[i] = fmt.Sprintf("$%d", argIdx+i)
		}
		valueStrings = append(valueStrings, "("+strings.Join(placeholders, ", ")+")")
		valueArgs = append(valueArgs, e.AccountID, e.CredentialGeneration, e.Channel, e.ClientIP, e.Endpoint, e.Model, e.EffectiveModel, e.PromptTokens, e.CompletionTokens, e.TotalTokens, e.StatusCode, e.DurationMs,
			e.InputTokens, e.OutputTokens, e.ReasoningTokens, e.FirstTokenMs, e.WsAcquireMs, e.ReasoningEffort, e.InboundEndpoint, e.UpstreamEndpoint, e.Stream, e.Compact, e.HasCompactionHistory, e.CachedTokens, e.CacheWrite5mTokens, e.CacheWrite1hTokens, e.ServiceTier,
			e.RequestedServiceTier, e.ActualServiceTier, e.BillingServiceTier,
			e.APIKeyID, e.APIKeyName, e.APIKeyMasked, e.ImageCount, e.ImageWidth, e.ImageHeight, e.ImageBytes, e.ImageFormat, e.ImageSize, e.AccountBilled, e.UserBilled,
			e.IsRetryAttempt, e.AttemptIndex, e.UpstreamErrorKind, e.ErrorMessage, e.ViaWebsocket,
			e.ClientUserAgent, e.UpstreamUserAgent, e.UserAgentOverridden, e.InternalReason, e.ParentRequestID, nullablePromptPolicyIncidentID(e.PromptPolicyIncidentID))
		argIdx += usageLogInsertColumnCount
	}

	query := fmt.Sprintf(`INSERT INTO usage_logs (account_id, credential_generation, channel, client_ip, endpoint, model, effective_model, prompt_tokens, completion_tokens, total_tokens, status_code, duration_ms,
		input_tokens, output_tokens, reasoning_tokens, first_token_ms, ws_acquire_ms, reasoning_effort, inbound_endpoint, upstream_endpoint, stream, compact, has_compaction_history, cached_tokens, cache_write_5m_tokens, cache_write_1h_tokens, service_tier,
		requested_service_tier, actual_service_tier, billing_service_tier,
		api_key_id, api_key_name, api_key_masked, image_count, image_width, image_height, image_bytes, image_format, image_size, account_billed, user_billed,
		is_retry_attempt, attempt_index, upstream_error_kind, error_message, via_websocket,
		client_user_agent, upstream_user_agent, user_agent_overridden, internal_reason, parent_request_id, prompt_policy_incident_id)
		VALUES %s`, strings.Join(valueStrings, ","))

	_, err := execer.ExecContext(ctx, query, valueArgs...)
	return err
}

func nullablePromptPolicyIncidentID(value string) any {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	return value
}

func (db *DB) applyAPIKeyQuotaUsage(ctx context.Context, batch []usageLogEntry) error {
	if db == nil {
		return nil
	}
	if err := db.applyAPIKeyScopeCountersWithExec(ctx, db.conn, batch); err != nil {
		return err
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
		if _, err := execer.ExecContext(ctx, `UPDATE api_keys SET quota_used = COALESCE(quota_used, 0) + $1, total_used = COALESCE(total_used, 0) + $1 WHERE id = $2`, amount, id); err != nil {
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
// GetUsageStats 聚合用量统计。channel 非空（codex/grok/antigravity/claude）时按渠道过滤；
// 渠道视图下的「累计」只覆盖现存 usage_logs（清空日志前的 baseline 无渠道维度，不计入）。
func (db *DB) GetUsageStats(ctx context.Context, rangeStart, rangeEnd time.Time, channel string) (*UsageStats, error) {
	return db.getUsageStats(ctx, rangeStart, rangeEnd, channel, true)
}

// GetUsageStatsSummary returns only the aggregate fields used by the dashboard.
// It deliberately skips model, endpoint, API-key and feature breakdowns, which
// otherwise require four additional scans over the selected usage-log range.
func (db *DB) GetUsageStatsSummary(ctx context.Context, rangeStart, rangeEnd time.Time, channel string) (*UsageStats, error) {
	return db.getUsageStats(ctx, rangeStart, rangeEnd, channel, false)
}

func (db *DB) getUsageStats(ctx context.Context, rangeStart, rangeEnd time.Time, channel string, includeBreakdowns bool) (*UsageStats, error) {
	channel = strings.TrimSpace(channel)
	explicitRange := !rangeStart.IsZero()
	if db.isSQLite() {
		return db.getUsageStatsSQLite(ctx, rangeStart, rangeEnd, channel, includeBreakdowns)
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
	if channel != "" {
		endClause += fmt.Sprintf(" AND channel = $%d", len(args)+1)
		args = append(args, channel)
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
	  AND TRIM(COALESCE(internal_reason, '')) = ''
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

	rollup, err := db.loadUsageStatsRollup(ctx, channel)
	if err != nil {
		return nil, fmt.Errorf("读取用量累计汇总: %w", err)
	}
	stats.TotalRequests = rollup.TotalRequests
	stats.TotalTokens = rollup.TotalTokens
	stats.TotalPrompt = rollup.PromptTokens
	stats.TotalCompletion = rollup.CompletionTokens
	stats.TotalCachedTokens = rollup.CachedTokens
	stats.TodayCachedTokens = todayCached
	stats.TotalAccountBilled = rollup.TotalAccountBilled
	stats.TotalUserBilled = rollup.TotalUserBilled
	if stats.TodayRequests > 0 {
		stats.TodayCacheRate = float64(todayCacheHitRequests) / float64(stats.TodayRequests) * 100
	}
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

	if stats.TodayRequests > 0 {
		stats.ErrorRate = float64(todayErrors) / float64(stats.TodayRequests) * 100
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

// CountTodayRequestsByChannel 统计今日各渠道请求数（与 GetUsageStats 的"今日"口径一致：
// 本地今日零点起、排除 499）。供仪表盘账号池概览按渠道展示。
func (db *DB) CountTodayRequestsByChannel(ctx context.Context) (map[string]int64, error) {
	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	rows, err := db.conn.QueryContext(ctx, `
		SELECT COALESCE(channel, ''), COUNT(*)
		FROM usage_logs
		WHERE created_at >= $1 AND status_code <> 499
		  AND TRIM(COALESCE(internal_reason, '')) = ''
		GROUP BY 1`, db.timeArg(todayStart))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]int64, 2)
	for rows.Next() {
		var channel string
		var count int64
		if err := rows.Scan(&channel, &count); err != nil {
			return nil, err
		}
		out[channel] = count
	}
	return out, rows.Err()
}

func (db *DB) usageStatsTimeWhere(column string, rangeStart, rangeEnd time.Time, channel string) (string, []interface{}) {
	if strings.TrimSpace(column) == "" {
		column = "created_at"
	}
	where := fmt.Sprintf("%s >= $1", column)
	args := []interface{}{db.timeArg(rangeStart)}
	if !rangeEnd.IsZero() {
		where += fmt.Sprintf(" AND %s < $%d", column, len(args)+1)
		args = append(args, db.timeArg(rangeEnd))
	}
	if channel = strings.TrimSpace(channel); channel != "" {
		where += fmt.Sprintf(" AND channel = $%d", len(args)+1)
		args = append(args, channel)
	}
	return where, args
}

func (db *DB) getUsageModelStats(ctx context.Context, limit int, rangeStart, rangeEnd time.Time, channel string) ([]UsageModelStat, error) {
	if limit <= 0 {
		limit = 10
	}
	timeWhere, args := db.usageStatsTimeWhere("created_at", rangeStart, rangeEnd, channel)
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
		  AND TRIM(COALESCE(internal_reason, '')) = ''
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

func (db *DB) populateUsageBreakdownStats(ctx context.Context, stats *UsageStats, rangeStart, rangeEnd time.Time, channel string) error {
	if stats == nil {
		return nil
	}
	timeWhere, args := db.usageStatsTimeWhere("created_at", rangeStart, rangeEnd, channel)
	if err := db.conn.QueryRowContext(ctx, `
		SELECT
			COALESCE(SUM(CASE WHEN stream THEN 1 ELSE 0 END), 0) AS stream_requests,
			COALESCE(SUM(CASE WHEN NOT stream THEN 1 ELSE 0 END), 0) AS sync_requests,
				COALESCE(SUM(CASE WHEN LOWER(COALESCE(NULLIF(billing_service_tier, ''), service_tier, '')) IN ('fast', 'priority') THEN 1 ELSE 0 END), 0) AS fast_requests,
			COALESCE(SUM(CASE WHEN cached_tokens > 0 THEN 1 ELSE 0 END), 0) AS cache_hit_requests,
			COALESCE(SUM(CASE WHEN reasoning_tokens > 0 OR NULLIF(reasoning_effort, '') IS NOT NULL THEN 1 ELSE 0 END), 0) AS reasoning_requests,
			COALESCE(SUM(CASE WHEN LOWER(COALESCE(NULLIF(inbound_endpoint, ''), endpoint, '')) LIKE '%/images/%' OR LOWER(COALESCE(model, '')) LIKE 'gpt-image-%' OR image_count > 0 THEN 1 ELSE 0 END), 0) AS image_requests,
			-- attempt_index 是 1-based（首次尝试写 1，第一次重试写 2），所以「重试出来的请求」
			-- 只能用 > 1。写成 > 0 会把每个请求都算进去，这个指标就恒等于总请求数（100%）；
			-- is_retry_attempt 标的是「本次失败且将要重试」的那条失败记录，算进来会重复计一次。
			COALESCE(SUM(CASE WHEN attempt_index > 1 THEN 1 ELSE 0 END), 0) AS retry_requests,
			COALESCE(SUM(CASE WHEN status_code >= 400 THEN 1 ELSE 0 END), 0) AS error_requests
		FROM usage_logs
		WHERE `+timeWhere+` AND status_code <> 499
		  AND TRIM(COALESCE(internal_reason, '')) = ''
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

	endpoints, err := db.getUsageEndpointStats(ctx, 8, rangeStart, rangeEnd, channel)
	if err != nil {
		return err
	}
	apiKeys, err := db.getUsageAPIKeyStats(ctx, 8, rangeStart, rangeEnd, channel)
	if err != nil {
		return err
	}
	stats.EndpointStats = endpoints
	stats.APIKeyStats = apiKeys
	return nil
}

func (db *DB) getUsageEndpointStats(ctx context.Context, limit int, rangeStart, rangeEnd time.Time, channel string) ([]UsageEndpointStat, error) {
	if limit <= 0 {
		limit = 8
	}
	timeWhere, args := db.usageStatsTimeWhere("created_at", rangeStart, rangeEnd, channel)
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
		  AND TRIM(COALESCE(internal_reason, '')) = ''
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

func (db *DB) getUsageAPIKeyStats(ctx context.Context, limit int, rangeStart, rangeEnd time.Time, channel string) ([]UsageAPIKeyStat, error) {
	if limit <= 0 {
		limit = 8
	}
	timeWhere, args := db.usageStatsTimeWhere("created_at", rangeStart, rangeEnd, channel)
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
		  AND TRIM(COALESCE(internal_reason, '')) = ''
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
		  AND TRIM(COALESCE(internal_reason, '')) = ''
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
	            COALESCE(u.first_token_ms, 0), COALESCE(u.ws_acquire_ms, 0), COALESCE(u.reasoning_effort, ''), COALESCE(u.inbound_endpoint, ''),
	            COALESCE(u.upstream_endpoint, ''), COALESCE(u.stream, false), COALESCE(u.compact, false), COALESCE(u.has_compaction_history, false), COALESCE(u.via_websocket, false), COALESCE(u.cached_tokens, 0), COALESCE(u.cache_write_5m_tokens, 0), COALESCE(u.cache_write_1h_tokens, 0), COALESCE(u.service_tier, ''),
	            COALESCE(u.requested_service_tier, ''), COALESCE(u.actual_service_tier, ''), COALESCE(u.billing_service_tier, ''),
	            COALESCE(u.api_key_id, 0), COALESCE(u.api_key_name, ''), COALESCE(u.api_key_masked, ''),
	            COALESCE(u.image_count, 0), COALESCE(u.image_width, 0), COALESCE(u.image_height, 0), COALESCE(u.image_bytes, 0),
		            COALESCE(u.image_format, ''), COALESCE(u.image_size, ''),
	            COALESCE(u.account_billed, 0), COALESCE(u.user_billed, 0),
	            COALESCE(u.is_retry_attempt, false), COALESCE(u.attempt_index, 0), COALESCE(u.upstream_error_kind, ''), COALESCE(u.error_message, ''),
	            COALESCE(u.client_user_agent, ''), COALESCE(u.upstream_user_agent, ''), COALESCE(u.user_agent_overridden, false), COALESCE(u.channel, ''),
	            COALESCE(u.internal_reason, ''), COALESCE(u.parent_request_id, ''), COALESCE(u.prompt_policy_incident_id, ''),
	            COALESCE(CAST(a.credentials AS TEXT), '{}'), COALESCE(a.name, ''), u.created_at
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
			&l.InputTokens, &l.OutputTokens, &l.ReasoningTokens, &l.FirstTokenMs, &l.WsAcquireMs, &l.ReasoningEffort, &l.InboundEndpoint, &l.UpstreamEndpoint, &l.Stream, &l.Compact, &l.HasCompactionHistory, &l.ViaWebsocket, &l.CachedTokens, &l.CacheWrite5mTokens, &l.CacheWrite1hTokens, &l.ServiceTier,
			&l.RequestedServiceTier, &l.ActualServiceTier, &l.BillingServiceTier,
			&l.APIKeyID, &l.APIKeyName, &l.APIKeyMasked, &l.ImageCount, &l.ImageWidth, &l.ImageHeight, &l.ImageBytes, &l.ImageFormat, &l.ImageSize, &l.AccountBilled, &l.UserBilled,
			&l.IsRetryAttempt, &l.AttemptIndex, &l.UpstreamErrorKind, &l.ErrorMessage, &l.ClientUserAgent, &l.UpstreamUserAgent, &l.UserAgentOverridden, &l.Channel,
			&l.InternalReason, &l.ParentRequestID, &l.PromptPolicyIncidentID,
			&credentialRaw, &l.AccountName, &createdAtRaw); err != nil {
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

// AccountKeyStat 单账号内按下游 Key 拆分的用量：某个 Key 调用了这个账号多少。
type AccountKeyStat struct {
	APIKeyID      int64   `json:"api_key_id"`
	APIKeyName    string  `json:"api_key_name"`
	APIKeyMasked  string  `json:"api_key_masked"`
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
	ByAPIKey              []AccountKeyStat      `json:"by_api_key"`
}

// GetChartAggregation 在数据库层完成图表数据的分桶聚合（无需传输原始行）
func (db *DB) GetChartAggregation(ctx context.Context, start, end time.Time, bucketMinutes int, channel string) (*ChartAggregation, error) {
	channel = strings.TrimSpace(channel)
	if db.isSQLite() {
		return db.getChartAggregationSQLite(ctx, start, end, bucketMinutes, channel)
	}

	if bucketMinutes < 1 {
		bucketMinutes = 5
	}
	channelClause := ""
	if channel != "" {
		channelClause = " AND channel = $4"
	}
	result := &ChartAggregation{}

	// One GROUPING SETS query produces both the timeline and model ranking. The
	// epoch-based bucket works for every interval, including 6h and 24h; the old
	// minute-of-hour modulo accidentally returned hourly points for those ranges.
	timelineQuery := `
	WITH filtered AS (
		SELECT
			TO_TIMESTAMP(FLOOR(EXTRACT(EPOCH FROM created_at) / ($3 * 60)) * ($3 * 60)) AS bucket,
			COALESCE(NULLIF(effective_model, ''), NULLIF(model, ''), 'unknown') AS model_name,
			duration_ms, input_tokens, output_tokens, reasoning_tokens, cached_tokens, status_code
		FROM usage_logs
		WHERE created_at >= $1 AND created_at < $2
		  AND status_code <> 499
		  AND TRIM(COALESCE(internal_reason, '')) = ''` + channelClause + `
	)
	SELECT
		CASE WHEN GROUPING(bucket) = 0 THEN 'timeline' ELSE 'model' END AS row_kind,
		CASE WHEN GROUPING(bucket) = 0
			THEN TO_CHAR(bucket AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS"Z"')
			ELSE model_name
		END AS row_key,
		COUNT(*) AS requests,
		COALESCE(AVG(duration_ms), 0) AS avg_latency,
		COALESCE(SUM(input_tokens), 0) AS input_tokens,
		COALESCE(SUM(output_tokens), 0) AS output_tokens,
		COALESCE(SUM(reasoning_tokens), 0) AS reasoning_tokens,
		COALESCE(SUM(cached_tokens), 0) AS cached_tokens,
		COALESCE(SUM(CASE WHEN status_code >= 400 AND status_code < 500 THEN 1 ELSE 0 END), 0) AS errors_4xx,
		COALESCE(SUM(CASE WHEN status_code >= 500 AND status_code < 600 THEN 1 ELSE 0 END), 0) AS errors_5xx
	FROM filtered
	GROUP BY GROUPING SETS ((bucket), (model_name))
	ORDER BY GROUPING(bucket), bucket, requests DESC, row_key`

	timelineArgs := []interface{}{start, end, bucketMinutes}
	if channel != "" {
		timelineArgs = append(timelineArgs, channel)
	}
	rows, err := db.conn.QueryContext(ctx, timelineQuery, timelineArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	for rows.Next() {
		var rowKind, rowKey string
		var requests int64
		var avgLatency float64
		var inputTokens, outputTokens, reasoningTokens, cachedTokens, errors4xx, errors5xx int64
		if err := rows.Scan(&rowKind, &rowKey, &requests, &avgLatency, &inputTokens, &outputTokens, &reasoningTokens, &cachedTokens, &errors4xx, &errors5xx); err != nil {
			return nil, err
		}
		if rowKind == "model" {
			result.Models = append(result.Models, ChartModelPoint{Model: rowKey, Requests: requests})
			continue
		}
		result.Timeline = append(result.Timeline, ChartTimelinePoint{
			Bucket: rowKey, Requests: requests, AvgLatency: avgLatency,
			InputTokens: inputTokens, OutputTokens: outputTokens, ReasoningTokens: reasoningTokens,
			CachedTokens: cachedTokens, Errors4xx: errors4xx, Errors5xx: errors5xx,
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if result.Timeline == nil {
		result.Timeline = []ChartTimelinePoint{}
	}

	if len(result.Models) > 10 {
		result.Models = result.Models[:10]
	}
	if result.Models == nil {
		result.Models = []ChartModelPoint{}
	}

	return result, nil
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
		History:  []AccountUsageDayStat{},
		Models:   []AccountModelStat{},
		ByAPIKey: []AccountKeyStat{},
	}
	timeWhere := "created_at >= $2 AND created_at < $3"
	queryArgs := []interface{}{accountID, db.timeArg(periodStart), db.timeArg(periodEnd)}
	if allTime {
		timeWhere = "created_at < $2"
		queryArgs = []interface{}{accountID, db.timeArg(periodEnd)}
	}
	rows, err := db.conn.QueryContext(ctx, `SELECT created_at,
		COALESCE(NULLIF(effective_model, ''), NULLIF(model, ''), 'unknown'),
		COALESCE(api_key_id, 0), COALESCE(NULLIF(api_key_name, ''), ''), COALESCE(api_key_masked, ''),
		COALESCE(total_tokens, 0), COALESCE(input_tokens, 0), COALESCE(output_tokens, 0),
		COALESCE(reasoning_tokens, 0), COALESCE(cached_tokens, 0),
		COALESCE(account_billed, 0), COALESCE(user_billed, 0), COALESCE(duration_ms, 0),
		COALESCE(first_token_ms, 0), status_code, COALESCE(is_retry_attempt, false),
		COALESCE(attempt_index, 0), COALESCE(stream, false), COALESCE(compact, false)
	FROM usage_logs
	WHERE account_id = $1 AND `+timeWhere+` AND status_code <> 499
	  AND TRIM(COALESCE(internal_reason, '')) = ''`, queryArgs...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type accountUsageKey struct {
		ID     int64
		Name   string
		Masked string
	}
	dayStats := make(map[string]*AccountUsageDayStat)
	modelStats := make(map[string]*AccountModelStat)
	keyStats := make(map[accountUsageKey]*AccountKeyStat)
	durations := make([]int64, 0, 1024)
	var cacheHitRequests, durationSamples int64
	var durationMsSum, firstTokenMsSum float64
	for rows.Next() {
		var createdAtRaw any
		var model, apiKeyName, apiKeyMasked string
		var apiKeyID int64
		var totalTokens, inputTokens, outputTokens, reasoningTokens, cachedTokens int64
		var accountBilled, userBilled float64
		var durationMs, firstTokenMs int64
		var statusCode, attemptIndex int
		var retryAttempt, stream, compact bool
		if err := rows.Scan(&createdAtRaw, &model, &apiKeyID, &apiKeyName, &apiKeyMasked,
			&totalTokens, &inputTokens, &outputTokens, &reasoningTokens, &cachedTokens,
			&accountBilled, &userBilled, &durationMs, &firstTokenMs, &statusCode,
			&retryAttempt, &attemptIndex, &stream, &compact); err != nil {
			return nil, err
		}
		createdAt, parseErr := parseDBTimeValue(createdAtRaw)
		if parseErr != nil {
			return nil, parseErr
		}
		localCreatedAt := createdAt.In(now.Location())
		dayKey := localCreatedAt.Format("2006-01-02")
		day := dayStats[dayKey]
		if day == nil {
			day = &AccountUsageDayStat{Date: dayKey, Label: localCreatedAt.Format("01/02")}
			dayStats[dayKey] = day
		}
		day.Requests++
		day.Tokens += totalTokens
		day.AccountBilled += accountBilled
		day.UserBilled += userBilled

		result.TotalRequests++
		result.TotalTokens += totalTokens
		result.InputTokens += inputTokens
		result.OutputTokens += outputTokens
		result.ReasoningTokens += reasoningTokens
		result.CachedTokens += cachedTokens
		result.TotalAccountBilled += accountBilled
		result.TotalUserBilled += userBilled
		if cachedTokens > 0 {
			cacheHitRequests++
		}
		if statusCode >= 400 {
			result.ErrorRequests++
		}
		// attempt_index 是 1-based：首次尝试写 1，第一次重试写 2，所以「重试出来的请求」只能用 > 1。
		// 写成 attemptIndex > 0 会把每个请求都计成重试（指标恒等于总请求数）；is_retry_attempt 标的是
		// 「本次失败且将要重试」的那条失败记录，算进来会把一次重试重复计两次。
		if attemptIndex > 1 {
			result.RetryRequests++
		}
		if durationMs > 0 {
			durationSamples++
			durationMsSum += float64(durationMs)
			durations = append(durations, durationMs)
		}
		if firstTokenMs > 0 {
			result.FirstTokenSamples++
			firstTokenMsSum += float64(firstTokenMs)
		}
		if stream {
			result.StreamRequests++
		}
		if compact {
			result.CompactRequests++
		}
		if !createdAt.Before(todayStart) && createdAt.Before(periodEnd) {
			result.Today.Requests++
			result.Today.Tokens += totalTokens
			result.Today.AccountBilled += accountBilled
			result.Today.UserBilled += userBilled
		}

		modelStat := modelStats[model]
		if modelStat == nil {
			modelStat = &AccountModelStat{Model: model}
			modelStats[model] = modelStat
		}
		modelStat.Requests++
		modelStat.Tokens += totalTokens
		modelStat.InputTokens += inputTokens
		modelStat.OutputTokens += outputTokens
		modelStat.ReasoningTokens += reasoningTokens
		modelStat.CachedTokens += cachedTokens
		modelStat.AccountBilled += accountBilled
		modelStat.UserBilled += userBilled

		key := accountUsageKey{ID: apiKeyID, Name: apiKeyName, Masked: apiKeyMasked}
		keyStat := keyStats[key]
		if keyStat == nil {
			keyStat = &AccountKeyStat{APIKeyID: apiKeyID, APIKeyName: apiKeyName, APIKeyMasked: apiKeyMasked}
			keyStats[key] = keyStat
		}
		keyStat.Requests++
		keyStat.Tokens += totalTokens
		keyStat.AccountBilled += accountBilled
		keyStat.UserBilled += userBilled
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	result.ActiveDays = len(dayStats)
	for _, day := range dayStats {
		result.History = append(result.History, *day)
		if result.HighestCostDay == nil || day.AccountBilled > result.HighestCostDay.AccountBilled ||
			(day.AccountBilled == result.HighestCostDay.AccountBilled && day.Requests > result.HighestCostDay.Requests) {
			copyDay := *day
			result.HighestCostDay = &copyDay
		}
		if result.HighestRequestDay == nil || day.Requests > result.HighestRequestDay.Requests ||
			(day.Requests == result.HighestRequestDay.Requests && day.AccountBilled > result.HighestRequestDay.AccountBilled) {
			copyDay := *day
			result.HighestRequestDay = &copyDay
		}
	}
	sort.Slice(result.History, func(i, j int) bool { return result.History[i].Date < result.History[j].Date })
	for _, item := range modelStats {
		result.Models = append(result.Models, *item)
	}
	sort.Slice(result.Models, func(i, j int) bool {
		if result.Models[i].Requests == result.Models[j].Requests {
			return result.Models[i].Model < result.Models[j].Model
		}
		return result.Models[i].Requests > result.Models[j].Requests
	})
	for _, item := range keyStats {
		result.ByAPIKey = append(result.ByAPIKey, *item)
	}
	sort.Slice(result.ByAPIKey, func(i, j int) bool {
		if result.ByAPIKey[i].Requests == result.ByAPIKey[j].Requests {
			if result.ByAPIKey[i].APIKeyID == result.ByAPIKey[j].APIKeyID {
				return result.ByAPIKey[i].APIKeyName < result.ByAPIKey[j].APIKeyName
			}
			return result.ByAPIKey[i].APIKeyID < result.ByAPIKey[j].APIKeyID
		}
		return result.ByAPIKey[i].Requests > result.ByAPIKey[j].Requests
	})

	if result.TotalRequests > 0 {
		result.CacheHitRate = float64(cacheHitRequests) / float64(result.TotalRequests) * 100
		result.ErrorRate = float64(result.ErrorRequests) / float64(result.TotalRequests) * 100
		result.StreamRate = float64(result.StreamRequests) / float64(result.TotalRequests) * 100
		result.CompactRate = float64(result.CompactRequests) / float64(result.TotalRequests) * 100
	}
	if durationSamples > 0 {
		result.AvgDurationMs = durationMsSum / float64(durationSamples)
		sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
		index := int(math.Ceil(float64(len(durations))*0.95)) - 1
		if index < 0 {
			index = 0
		}
		result.P95DurationMs = float64(durations[index])
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

	return result, nil
}

// ListUsageLogsByTimeRange 按时间范围查询请求日志
func (db *DB) ListUsageLogsByTimeRange(ctx context.Context, start, end time.Time) ([]*UsageLog, error) {
	startArg, endArg := db.timeRangeArgs(start, end)
	query := `SELECT u.id, u.account_id, COALESCE(u.client_ip, ''), u.endpoint, u.model, COALESCE(u.effective_model, ''), u.prompt_tokens, u.completion_tokens, u.total_tokens, u.status_code, u.duration_ms,
	            COALESCE(u.input_tokens, 0), COALESCE(u.output_tokens, 0), COALESCE(u.reasoning_tokens, 0),
	            COALESCE(u.first_token_ms, 0), COALESCE(u.ws_acquire_ms, 0), COALESCE(u.reasoning_effort, ''), COALESCE(u.inbound_endpoint, ''),
	            COALESCE(u.upstream_endpoint, ''), COALESCE(u.stream, false), COALESCE(u.compact, false), COALESCE(u.has_compaction_history, false), COALESCE(u.via_websocket, false), COALESCE(u.cached_tokens, 0), COALESCE(u.cache_write_5m_tokens, 0), COALESCE(u.cache_write_1h_tokens, 0), COALESCE(u.service_tier, ''),
	            COALESCE(u.requested_service_tier, ''), COALESCE(u.actual_service_tier, ''), COALESCE(u.billing_service_tier, ''),
	            COALESCE(u.api_key_id, 0), COALESCE(u.api_key_name, ''), COALESCE(u.api_key_masked, ''),
	            COALESCE(u.image_count, 0), COALESCE(u.image_width, 0), COALESCE(u.image_height, 0), COALESCE(u.image_bytes, 0),
		            COALESCE(u.image_format, ''), COALESCE(u.image_size, ''),
	            COALESCE(u.account_billed, 0), COALESCE(u.user_billed, 0),
	            COALESCE(u.is_retry_attempt, false), COALESCE(u.attempt_index, 0), COALESCE(u.upstream_error_kind, ''), COALESCE(u.error_message, ''),
	            COALESCE(u.client_user_agent, ''), COALESCE(u.upstream_user_agent, ''), COALESCE(u.user_agent_overridden, false), COALESCE(u.channel, ''),
	            COALESCE(u.internal_reason, ''), COALESCE(u.parent_request_id, ''), COALESCE(u.prompt_policy_incident_id, ''),
	            COALESCE(CAST(a.credentials AS TEXT), '{}'), COALESCE(a.name, ''), u.created_at
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
			&l.InputTokens, &l.OutputTokens, &l.ReasoningTokens, &l.FirstTokenMs, &l.WsAcquireMs, &l.ReasoningEffort, &l.InboundEndpoint, &l.UpstreamEndpoint, &l.Stream, &l.Compact, &l.HasCompactionHistory, &l.ViaWebsocket, &l.CachedTokens, &l.CacheWrite5mTokens, &l.CacheWrite1hTokens, &l.ServiceTier,
			&l.RequestedServiceTier, &l.ActualServiceTier, &l.BillingServiceTier,
			&l.APIKeyID, &l.APIKeyName, &l.APIKeyMasked, &l.ImageCount, &l.ImageWidth, &l.ImageHeight, &l.ImageBytes, &l.ImageFormat, &l.ImageSize, &l.AccountBilled, &l.UserBilled,
			&l.IsRetryAttempt, &l.AttemptIndex, &l.UpstreamErrorKind, &l.ErrorMessage, &l.ClientUserAgent, &l.UpstreamUserAgent, &l.UserAgentOverridden, &l.Channel,
			&l.InternalReason, &l.ParentRequestID, &l.PromptPolicyIncidentID,
			&credentialRaw, &l.AccountName, &createdAtRaw); err != nil {
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
	Start                 time.Time
	End                   time.Time
	Page                  int
	PageSize              int
	Email                 string // LIKE 模糊匹配
	Model                 string // 精确匹配
	Endpoint              string // 精确匹配 inbound_endpoint
	APIKeyID              *int64 // nil=全部
	AccountID             *int64 // nil=全部
	FastOnly              *bool  // nil=全部, true=仅fast, false=仅非fast
	StreamOnly            *bool  // nil=全部, true=仅stream, false=仅sync
	CompactOnly           *bool  // nil=全部, true=本次触发压缩, false=本次未触发压缩
	CompactionHistoryOnly *bool  // nil=全部, true=携带压缩历史, false=未携带压缩历史
	ErrorOnly             bool
	IncludeCanceled       bool
	StatusCode            int
	StatusFamily          string
	ErrorKind             string
	Query                 string
	Channel               string // 上游渠道（codex/grok），空=全部
	RetryOnly             *bool  // nil=全部, true=仅重试请求, false=仅首次请求
	ViaWebsocketOnly      *bool  // nil=全部, true=仅 WebSocket, false=仅 HTTP
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
		parts = append(parts, fmt.Sprintf(`(
			LOWER(COALESCE(u.client_ip, '')) LIKE LOWER(%[1]s)
			OR u.account_id IN (
				SELECT search_accounts.id
				FROM accounts search_accounts
				WHERE LOWER(COALESCE(search_accounts.name, '')) LIKE LOWER(%[1]s)
					OR LOWER(COALESCE(CAST(search_accounts.credentials AS TEXT), '')) LIKE LOWER(%[1]s)
			)
		)`, p))
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
	if f.CompactOnly != nil {
		p := addArg(*f.CompactOnly)
		parts = append(parts, fmt.Sprintf(`COALESCE(u.compact, false) = %s`, p))
	}
	if f.CompactionHistoryOnly != nil {
		p := addArg(*f.CompactionHistoryOnly)
		parts = append(parts, fmt.Sprintf(`COALESCE(u.has_compaction_history, false) = %s`, p))
	}
	if f.RetryOnly != nil {
		p := addArg(*f.RetryOnly)
		parts = append(parts, fmt.Sprintf(`COALESCE(u.is_retry_attempt, false) = %s`, p))
	}
	if f.ViaWebsocketOnly != nil {
		p := addArg(*f.ViaWebsocketOnly)
		parts = append(parts, fmt.Sprintf(`COALESCE(u.via_websocket, false) = %s`, p))
	}
	if f.StatusCode > 0 {
		p := addArg(f.StatusCode)
		parts = append(parts, fmt.Sprintf(`u.status_code = %s`, p))
	}
	switch strings.ToLower(strings.TrimSpace(f.StatusFamily)) {
	case "2xx":
		parts = append(parts, `u.status_code >= 200 AND u.status_code < 300`)
	case "4xx":
		parts = append(parts, `u.status_code >= 400 AND u.status_code < 500`)
	case "5xx":
		parts = append(parts, `u.status_code >= 500 AND u.status_code < 600`)
	}
	if f.ErrorKind != "" {
		p := addArg(f.ErrorKind)
		parts = append(parts, fmt.Sprintf(`COALESCE(u.upstream_error_kind, '') = %s`, p))
	}
	if channel := strings.TrimSpace(f.Channel); channel != "" {
		p := addArg(channel)
		parts = append(parts, fmt.Sprintf(`COALESCE(u.channel, '') = %s`, p))
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
				OR u.account_id IN (
					SELECT search_accounts.id
					FROM accounts search_accounts
					WHERE LOWER(COALESCE(search_accounts.name, '')) LIKE LOWER(%[1]s)
						OR LOWER(COALESCE(CAST(search_accounts.credentials AS TEXT), '')) LIKE LOWER(%[1]s)
				)
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
	            COALESCE(u.first_token_ms, 0), COALESCE(u.ws_acquire_ms, 0), COALESCE(u.reasoning_effort, ''), COALESCE(u.inbound_endpoint, ''),
	            COALESCE(u.upstream_endpoint, ''), COALESCE(u.stream, false), COALESCE(u.compact, false), COALESCE(u.has_compaction_history, false), COALESCE(u.via_websocket, false), COALESCE(u.cached_tokens, 0), COALESCE(u.cache_write_5m_tokens, 0), COALESCE(u.cache_write_1h_tokens, 0), COALESCE(u.service_tier, ''),
	            COALESCE(u.requested_service_tier, ''), COALESCE(u.actual_service_tier, ''), COALESCE(u.billing_service_tier, ''),
	            COALESCE(u.api_key_id, 0), COALESCE(u.api_key_name, ''), COALESCE(u.api_key_masked, ''),
	            COALESCE(u.image_count, 0), COALESCE(u.image_width, 0), COALESCE(u.image_height, 0), COALESCE(u.image_bytes, 0),
		            COALESCE(u.image_format, ''), COALESCE(u.image_size, ''),
			            COALESCE(u.account_billed, 0), COALESCE(u.user_billed, 0),
			            COALESCE(u.is_retry_attempt, false), COALESCE(u.attempt_index, 0), COALESCE(u.upstream_error_kind, ''), COALESCE(u.error_message, ''),
			            COALESCE(u.client_user_agent, ''), COALESCE(u.upstream_user_agent, ''), COALESCE(u.user_agent_overridden, false), COALESCE(u.channel, ''),
			            COALESCE(u.internal_reason, ''), COALESCE(u.parent_request_id, ''), COALESCE(u.prompt_policy_incident_id, ''),
			            COALESCE(CAST(a.credentials AS TEXT), '{}'), COALESCE(a.name, ''), u.created_at,
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
			&l.InputTokens, &l.OutputTokens, &l.ReasoningTokens, &l.FirstTokenMs, &l.WsAcquireMs, &l.ReasoningEffort, &l.InboundEndpoint, &l.UpstreamEndpoint, &l.Stream, &l.Compact, &l.HasCompactionHistory, &l.ViaWebsocket, &l.CachedTokens, &l.CacheWrite5mTokens, &l.CacheWrite1hTokens,
			&l.ServiceTier, &l.RequestedServiceTier, &l.ActualServiceTier, &l.BillingServiceTier, &l.APIKeyID, &l.APIKeyName, &l.APIKeyMasked, &l.ImageCount, &l.ImageWidth, &l.ImageHeight, &l.ImageBytes, &l.ImageFormat, &l.ImageSize,
			&l.AccountBilled, &l.UserBilled, &l.IsRetryAttempt, &l.AttemptIndex, &l.UpstreamErrorKind, &l.ErrorMessage,
			&l.ClientUserAgent, &l.UpstreamUserAgent, &l.UserAgentOverridden, &l.Channel, &l.InternalReason, &l.ParentRequestID, &l.PromptPolicyIncidentID,
			&credentialRaw, &l.AccountName, &createdAtRaw, &result.Total); err != nil {
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
			COALESCE(u.first_token_ms, 0), COALESCE(u.ws_acquire_ms, 0), COALESCE(u.reasoning_effort, ''), COALESCE(u.inbound_endpoint, ''),
			COALESCE(u.upstream_endpoint, ''), COALESCE(u.stream, false), COALESCE(u.compact, false), COALESCE(u.has_compaction_history, false), COALESCE(u.via_websocket, false), COALESCE(u.cached_tokens, 0), COALESCE(u.cache_write_5m_tokens, 0), COALESCE(u.cache_write_1h_tokens, 0), COALESCE(u.service_tier, ''),
			COALESCE(u.requested_service_tier, ''), COALESCE(u.actual_service_tier, ''), COALESCE(u.billing_service_tier, ''),
			COALESCE(u.api_key_id, 0), COALESCE(u.api_key_name, ''), COALESCE(u.api_key_masked, ''),
			COALESCE(u.image_count, 0), COALESCE(u.image_width, 0), COALESCE(u.image_height, 0), COALESCE(u.image_bytes, 0),
			COALESCE(u.image_format, ''), COALESCE(u.image_size, ''),
			COALESCE(u.account_billed, 0), COALESCE(u.user_billed, 0),
			COALESCE(u.is_retry_attempt, false), COALESCE(u.attempt_index, 0), COALESCE(u.upstream_error_kind, ''), COALESCE(u.error_message, ''),
			COALESCE(u.client_user_agent, ''), COALESCE(u.upstream_user_agent, ''), COALESCE(u.user_agent_overridden, false), COALESCE(u.channel, ''),
			COALESCE(u.internal_reason, ''), COALESCE(u.parent_request_id, ''), COALESCE(u.prompt_policy_incident_id, ''),
			COALESCE(CAST(a.credentials AS TEXT), '{}'), COALESCE(a.name, ''), u.created_at
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
			&l.InputTokens, &l.OutputTokens, &l.ReasoningTokens, &l.FirstTokenMs, &l.WsAcquireMs, &l.ReasoningEffort, &l.InboundEndpoint, &l.UpstreamEndpoint, &l.Stream, &l.Compact, &l.HasCompactionHistory, &l.ViaWebsocket, &l.CachedTokens, &l.CacheWrite5mTokens, &l.CacheWrite1hTokens,
			&l.ServiceTier, &l.RequestedServiceTier, &l.ActualServiceTier, &l.BillingServiceTier, &l.APIKeyID, &l.APIKeyName, &l.APIKeyMasked, &l.ImageCount, &l.ImageWidth, &l.ImageHeight, &l.ImageBytes, &l.ImageFormat, &l.ImageSize,
			&l.AccountBilled, &l.UserBilled, &l.IsRetryAttempt, &l.AttemptIndex, &l.UpstreamErrorKind, &l.ErrorMessage,
			&l.ClientUserAgent, &l.UpstreamUserAgent, &l.UserAgentOverridden, &l.Channel, &l.InternalReason, &l.ParentRequestID, &l.PromptPolicyIncidentID,
			&credentialRaw, &l.AccountName, &createdAtRaw); err != nil {
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
	// 先校验增量汇总是否与明细日志同步。这也兼容测试、手工 SQL 等绕过正常写入队列的场景。
	if _, err := db.loadUsageStatsRollup(ctx, ""); err != nil {
		return fmt.Errorf("读取清理前完整累计失败: %w", err)
	}
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if !db.isSQLite() {
		// 先锁明细表，等待正在写入的批次完整提交；之后的新写入在清理事务结束前不会插入。
		if _, err := tx.ExecContext(ctx, `LOCK TABLE usage_logs IN ACCESS EXCLUSIVE MODE`); err != nil {
			return err
		}
	}
	var rollup usageStatsRollup
	if err := tx.QueryRowContext(ctx, `SELECT total_requests, total_tokens, prompt_tokens, completion_tokens,
		cached_tokens, cache_hit_requests, first_token_ms_sum, first_token_samples, account_billed, user_billed
		FROM usage_stats_rollup WHERE channel=''`).Scan(&rollup.TotalRequests, &rollup.TotalTokens,
		&rollup.PromptTokens, &rollup.CompletionTokens, &rollup.CachedTokens, &rollup.CacheHitRequests,
		&rollup.FirstTokenMsSum, &rollup.FirstTokenSamples, &rollup.TotalAccountBilled, &rollup.TotalUserBilled); err != nil {
		return fmt.Errorf("锁定后读取完整累计失败: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE usage_stats_baseline SET
		total_requests=$1, total_tokens=$2, prompt_tokens=$3, completion_tokens=$4,
		cached_tokens=$5, cache_hit_requests=$6, first_token_ms_sum=$7, first_token_samples=$8,
		account_billed=$9, user_billed=$10 WHERE id=1`, rollup.TotalRequests, rollup.TotalTokens,
		rollup.PromptTokens, rollup.CompletionTokens, rollup.CachedTokens, rollup.CacheHitRequests,
		rollup.FirstTokenMsSum, rollup.FirstTokenSamples, rollup.TotalAccountBilled, rollup.TotalUserBilled); err != nil {
		return fmt.Errorf("快照统计基线失败: %w", err)
	}
	if db.isSQLite() {
		if _, err = tx.ExecContext(ctx, `DELETE FROM usage_logs`); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, `DELETE FROM sqlite_sequence WHERE name = 'usage_logs'`); err != nil {
			return err
		}
	} else if _, err = tx.ExecContext(ctx, `TRUNCATE TABLE usage_logs RESTART IDENTITY`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM usage_stats_rollup WHERE channel <> ''`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE usage_stats_rollup_state SET initialized=1, last_log_id=0, updated_at=CURRENT_TIMESTAMP WHERE id=1`); err != nil {
		return err
	}
	return tx.Commit()
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
	// ErrorStatusCounts is the 7-day non-retry 4xx/5xx breakdown keyed by
	// HTTP status. It matches ErrorCount so the list tooltip can show each
	// code's share of the red capsule.
	ErrorStatusCounts map[int]int64
	// SuccessModelCounts is the matching 7-day non-retry 2xx/3xx breakdown
	// keyed by effective model (falling back to inbound model).
	SuccessModelCounts map[string]int64
}

// AccountTimeRangeUsage 每个账号在指定时间窗口内的真实请求/token 统计。
type AccountTimeRangeUsage struct {
	AccountID     int64
	Requests      int64
	Tokens        int64
	AccountBilled float64
	UserBilled    float64
}

// AccountModelCount 某个模型在指定窗口内的请求数、成功数与平均首字时长。
type AccountModelCount struct {
	Requests        int64
	Success         int64
	AvgFirstTokenMs float64
}

// nonRetryUsageLogPredicate keeps transport retry attempts out of end-user
// request, token, and billing aggregates. Historical rows can contain NULL;
// SQLite stores booleans as integers while PostgreSQL uses BOOLEAN.
func (db *DB) nonRetryUsageLogPredicate() string {
	if db.isSQLite() {
		return "COALESCE(is_retry_attempt, 0) = 0"
	}
	return "COALESCE(is_retry_attempt, false) = false"
}

// endUserUsageLogPredicate excludes maintenance/probe traffic from account
// request, token, and billing aggregates. Internal rows remain queryable in the
// raw usage log for diagnostics.
func (db *DB) endUserUsageLogPredicate() string {
	return "TRIM(COALESCE(internal_reason, '')) = ''"
}

// currentAccountUsageGenerationPredicate keeps generation-scoped internal
// traffic (currently Grok capability probes) attached to the credential
// identity that issued it. Legacy and ordinary request rows use generation 0
// and remain visible. The correlated account lookup also makes a buffered old
// probe harmless if credentials rotate after the probe's final pre-write CAS.
func (db *DB) currentAccountUsageGenerationPredicate() string {
	return `(COALESCE(usage_logs.credential_generation, 0) = 0 OR
		usage_logs.credential_generation = COALESCE((SELECT accounts.credential_generation FROM accounts WHERE accounts.id = usage_logs.account_id), usage_logs.credential_generation))`
}

// GetAccountRequestCounts 按 account_id 聚合近 7 天成功/失败请求数，499 客户端取消不计失败。
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
		COALESCE(SUM(CASE WHEN status_code >= 400 AND status_code <> 499 AND %s THEN 1 ELSE 0 END), 0) AS error_count,
		COALESCE(SUM(CASE WHEN status_code >= 400 AND status_code <> 499 AND %s THEN 1 ELSE 0 END), 0) AS retry_error_count,
		COALESCE(SUM(CASE WHEN status_code = 429 THEN 1 ELSE 0 END), 0) AS rate_limit_attempt_count
	FROM usage_logs
	WHERE created_at >= $1 AND %s
	GROUP BY account_id
	`, retryFalse, retryFalse, retryTrue, db.endUserUsageLogPredicate())
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
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := db.attachErrorStatusCounts(ctx, result, nil); err != nil {
		return nil, err
	}
	if err := db.attachSuccessModelCounts(ctx, result, nil); err != nil {
		return nil, err
	}
	return result, nil
}

// GetAccountTimeRangeUsage 按 account_id 聚合 since 之后的请求数和 token 数。
func (db *DB) GetAccountTimeRangeUsage(ctx context.Context, since time.Time) (map[int64]*AccountTimeRangeUsage, error) {
	query := fmt.Sprintf(`
	SELECT account_id,
		COUNT(*) AS requests,
		COALESCE(SUM(total_tokens), 0) AS tokens,
		COALESCE(SUM(account_billed), 0) AS account_billed,
		COALESCE(SUM(user_billed), 0) AS user_billed
	FROM usage_logs
	WHERE created_at >= $1 AND status_code <> 499 AND %s AND %s AND %s
	GROUP BY account_id
	`, db.nonRetryUsageLogPredicate(), db.currentAccountUsageGenerationPredicate(), db.endUserUsageLogPredicate())
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

// GetAccountUsageWindows 在一次 7 天索引扫描内同时计算短窗口和长窗口，
// 避免账号列表刷新时对 usage_logs 连续做两次大范围 GROUP BY。
func (db *DB) GetAccountUsageWindows(ctx context.Context, shortSince, longSince time.Time) (map[int64]*AccountTimeRangeUsage, map[int64]*AccountTimeRangeUsage, error) {
	if shortSince.Before(longSince) {
		shortSince, longSince = longSince, shortSince
	}
	query := fmt.Sprintf(`SELECT account_id,
		COALESCE(SUM(CASE WHEN created_at >= $1 THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN created_at >= $1 THEN total_tokens ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN created_at >= $1 THEN account_billed ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN created_at >= $1 THEN user_billed ELSE 0 END), 0),
		COUNT(*), COALESCE(SUM(total_tokens), 0), COALESCE(SUM(account_billed), 0), COALESCE(SUM(user_billed), 0)
	FROM usage_logs
	WHERE created_at >= $2 AND status_code <> 499 AND %s AND %s AND %s
	GROUP BY account_id`, db.nonRetryUsageLogPredicate(), db.currentAccountUsageGenerationPredicate(), db.endUserUsageLogPredicate())
	rows, err := db.conn.QueryContext(ctx, query, db.timeArg(shortSince), db.timeArg(longSince))
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	shortWindow := make(map[int64]*AccountTimeRangeUsage)
	longWindow := make(map[int64]*AccountTimeRangeUsage)
	for rows.Next() {
		shortUsage := &AccountTimeRangeUsage{}
		longUsage := &AccountTimeRangeUsage{}
		if err := rows.Scan(&shortUsage.AccountID, &shortUsage.Requests, &shortUsage.Tokens, &shortUsage.AccountBilled, &shortUsage.UserBilled,
			&longUsage.Requests, &longUsage.Tokens, &longUsage.AccountBilled, &longUsage.UserBilled); err != nil {
			return nil, nil, err
		}
		longUsage.AccountID = shortUsage.AccountID
		shortWindow[shortUsage.AccountID] = shortUsage
		longWindow[longUsage.AccountID] = longUsage
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return shortWindow, longWindow, nil
}

// GetAccountBilledSince 返回指定时间戳以来 account_billed 的总和
func (db *DB) GetAccountBilledSince(ctx context.Context, accountID int64, since time.Time) (float64, error) {
	var billed float64
	err := db.conn.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(account_billed), 0) FROM usage_logs WHERE account_id = $1 AND created_at >= $2
		 AND status_code <> 499 AND TRIM(COALESCE(internal_reason, '')) = ''`,
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
		AND TRIM(COALESCE(usage_logs.internal_reason, '')) = ''
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
	return db.ListActiveByChannel(ctx, "")
}

// ListActiveByChannel 返回未删除账号；channel 为空返回全部，其他已知渠道
// 按 credentials.upstream_type 显式隔离。缺省 upstream_type 的历史账号归 Codex。
func (db *DB) ListActiveByChannel(ctx context.Context, channel string) ([]*AccountRow, error) {
	where := `status <> 'deleted' AND COALESCE(error_message, '') <> 'deleted'`
	upstreamTypeExpr := `LOWER(COALESCE(credentials->>'upstream_type', ''))`
	if db.isSQLite() {
		upstreamTypeExpr = `LOWER(COALESCE(json_extract(credentials, '$.upstream_type'), ''))`
	}
	where += accountChannelFilterSQL(channel, upstreamTypeExpr)

	query := `
		SELECT id, name, platform, type, credentials, proxy_url, status, cooldown_reason, cooldown_until, error_message, COALESCE(enabled, true), COALESCE(locked, false), COALESCE(credit_enabled, false), COALESCE(credit_skip_usage_window, false), COALESCE(skip_warm_tier, false), score_bias_override, base_concurrency_override, COALESCE(tags, '[]'), COALESCE(note, ''), created_at, updated_at, COALESCE(credential_generation, 1), COALESCE(credential_family_id, '')
		FROM accounts
		WHERE ` + where + `
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
			&a.Note,
			&createdAtRaw,
			&updatedAtRaw,
			&a.CredentialGeneration,
			&a.CredentialFamilyID,
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

// ListActiveModelCooldownsForAccount is the indexed, single-account variant
// used outside batched scheduler projection refreshes.
func (db *DB) ListActiveModelCooldownsForAccount(ctx context.Context, accountID int64) ([]*AccountModelCooldownRow, error) {
	if accountID <= 0 {
		return nil, nil
	}
	return db.ListActiveModelCooldownsForAccounts(ctx, []int64{accountID})
}

// ListActiveModelCooldownsForAccounts loads only the cooldown rows touched by
// one scheduler-outbox batch. The consumer caps that batch at 500 account IDs,
// so both PostgreSQL and SQLite stay comfortably within bind limits.
func (db *DB) ListActiveModelCooldownsForAccounts(ctx context.Context, accountIDs []int64) ([]*AccountModelCooldownRow, error) {
	accountIDs = positiveUniqueIDs(accountIDs)
	if len(accountIDs) == 0 {
		return nil, nil
	}
	args := make([]interface{}, 0, len(accountIDs)+1)
	placeholders := make([]string, 0, len(accountIDs))
	for _, accountID := range accountIDs {
		args = append(args, accountID)
		if db.isSQLite() {
			placeholders = append(placeholders, "?")
		} else {
			placeholders = append(placeholders, fmt.Sprintf("$%d", len(args)))
		}
	}
	args = append(args, db.timeArg(time.Now()))
	resetPlaceholder := "?"
	if !db.isSQLite() {
		resetPlaceholder = fmt.Sprintf("$%d", len(args))
	}
	rows, err := db.conn.QueryContext(ctx, `
		SELECT account_id, model, COALESCE(reason, ''), reset_at, updated_at
		FROM account_model_cooldowns
		WHERE account_id IN (`+strings.Join(placeholders, ",")+`) AND reset_at > `+resetPlaceholder+`
		ORDER BY account_id, model
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("批量查询账号模型冷却失败: %w", err)
	}
	defer rows.Close()

	var result []*AccountModelCooldownRow
	for rows.Next() {
		row := &AccountModelCooldownRow{}
		var resetRaw, updatedRaw interface{}
		if err := rows.Scan(&row.AccountID, &row.Model, &row.Reason, &resetRaw, &updatedRaw); err != nil {
			return nil, err
		}
		row.ResetAt, err = parseDBTimeValue(resetRaw)
		if err != nil {
			return nil, fmt.Errorf("解析模型冷却 reset_at 失败: %w", err)
		}
		row.UpdatedAt, err = parseDBTimeValue(updatedRaw)
		if err != nil {
			return nil, fmt.Errorf("解析模型冷却 updated_at 失败: %w", err)
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

func (db *DB) ClearAllModelCooldowns(ctx context.Context, accountID int64) error {
	if accountID == 0 {
		return nil
	}
	_, err := db.conn.ExecContext(ctx, `DELETE FROM account_model_cooldowns WHERE account_id = $1`, accountID)
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
		SELECT id, name, platform, type, credentials, proxy_url, status, cooldown_reason, cooldown_until, error_message, COALESCE(enabled, true), COALESCE(locked, false), COALESCE(credit_enabled, false), COALESCE(credit_skip_usage_window, false), COALESCE(skip_warm_tier, false), score_bias_override, base_concurrency_override, COALESCE(tags, '[]'), COALESCE(note, ''), created_at, updated_at, COALESCE(credential_generation, 1), COALESCE(credential_family_id, '')
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
		&a.Note,
		&createdAtRaw,
		&updatedAtRaw,
		&a.CredentialGeneration,
		&a.CredentialFamilyID,
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
		credJSON, err := json.Marshal(encryptSensitiveCredentials(merged))
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
	return db.withSQLiteWriteLock(ctx, func() error {
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
			current := decodeCredentials(currentRaw)
			merged := mergeCredentialMaps(cloneCredentialUpdates(current), credentialUpdates)
			identityChanged := grokIdentityCredentialChanged(current, merged)
			credJSON, err := json.Marshal(encryptSensitiveCredentials(merged))
			if err != nil {
				return fmt.Errorf("序列化 credentials 失败: %w", err)
			}
			if db.isSQLite() {
				add("credentials", credJSON)
			} else {
				args = append(args, credJSON)
				sets = append(sets, fmt.Sprintf("credentials = $%d::jsonb", len(args)))
			}
			if identityChanged {
				// Keep the credentials document and its identity generation in the
				// same row update/transaction. Configuration-only merges deliberately
				// leave the generation untouched.
				sets = append(sets, "credential_generation = credential_generation + 1")
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
	})
}

func (db *DB) BatchUpdateAccountMetadata(ctx context.Context, ids []int64, update BatchAccountMetadataUpdate) ([]int64, error) {
	ids = normalizeIDSlice(ids)
	if len(ids) == 0 || !update.HasChanges() {
		return nil, nil
	}
	var updatedIDs []int64
	err := db.withSQLiteWriteLock(ctx, func() error {
		tx, err := db.conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()

		credentialUpdates := cloneCredentialUpdates(update.CredentialUpdates)
		if update.AllowedAPIKeyIDs.Set {
			if credentialUpdates == nil {
				credentialUpdates = make(map[string]interface{}, 1)
			}
			credentialUpdates["allowed_api_key_ids"] = normalizePositiveInt64Slice(update.AllowedAPIKeyIDs.Values)
		}

		active, selectErr := db.selectBatchAccounts(ctx, tx, ids, len(credentialUpdates) > 0)
		if selectErr != nil {
			return selectErr
		}
		if len(active.ids) == 0 {
			return tx.Commit()
		}

		if updateErr := db.batchUpdateAccountColumns(ctx, tx, active.ids, update); updateErr != nil {
			return updateErr
		}
		if len(credentialUpdates) > 0 {
			if updateErr := db.batchUpdateAccountCredentials(ctx, tx, active.credentials, credentialUpdates); updateErr != nil {
				return updateErr
			}
		}
		if update.GroupIDs.Set {
			if updateErr := db.batchReplaceAccountGroups(ctx, tx, active.ids, update.GroupIDs.Values); updateErr != nil {
				return updateErr
			}
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return commitErr
		}
		updatedIDs = active.ids
		return nil
	})
	return updatedIDs, err
}

type batchAccountCredentials struct {
	ids         []int64
	credentials map[int64]map[string]interface{}
}

func (db *DB) selectBatchAccounts(ctx context.Context, tx *sql.Tx, ids []int64, includeCredentials bool) (batchAccountCredentials, error) {
	placeholders := dbPlaceholders(db.isSQLite(), 1, len(ids))
	columns := "id"
	if includeCredentials {
		columns = "id, credentials"
	}
	query := fmt.Sprintf(`SELECT %s FROM accounts WHERE status <> 'deleted' AND COALESCE(error_message, '') <> 'deleted' AND id IN (%s)`, columns, strings.Join(placeholders, ","))
	if !db.isSQLite() {
		query += ` FOR UPDATE`
	}
	rows, err := tx.QueryContext(ctx, query, argsFromInt64s(ids)...)
	if err != nil {
		return batchAccountCredentials{}, err
	}
	defer rows.Close()

	out := batchAccountCredentials{
		ids:         make([]int64, 0, len(ids)),
		credentials: make(map[int64]map[string]interface{}, len(ids)),
	}
	for rows.Next() {
		var id int64
		if includeCredentials {
			var raw interface{}
			if err := rows.Scan(&id, &raw); err != nil {
				return batchAccountCredentials{}, err
			}
			out.credentials[id] = decodeCredentials(raw)
		} else if err := rows.Scan(&id); err != nil {
			return batchAccountCredentials{}, err
		}
		out.ids = append(out.ids, id)
	}
	return out, rows.Err()
}

func cloneCredentialUpdates(updates map[string]interface{}) map[string]interface{} {
	if len(updates) == 0 {
		return nil
	}
	out := make(map[string]interface{}, len(updates))
	for key, value := range updates {
		out[key] = value
	}
	return out
}

func (db *DB) batchUpdateAccountColumns(ctx context.Context, tx *sql.Tx, ids []int64, update BatchAccountMetadataUpdate) error {
	sets := make([]string, 0, 8)
	args := make([]interface{}, 0, 10+len(ids))
	touchUpdatedAt := false
	add := func(column string, value interface{}, touch bool) {
		args = append(args, value)
		ph := "?"
		if !db.isSQLite() {
			ph = fmt.Sprintf("$%d", len(args))
		}
		sets = append(sets, column+" = "+ph)
		touchUpdatedAt = touchUpdatedAt || touch
	}
	if update.Enabled.Set {
		add("enabled", update.Enabled.Value, true)
	}
	if update.Locked.Set {
		add("locked", update.Locked.Value, false)
	}
	if update.ScoreBiasOverride.Set {
		add("score_bias_override", nullableInt64Value(update.ScoreBiasOverride.Value), true)
	}
	if update.BaseConcurrencyOverride.Set {
		add("base_concurrency_override", nullableInt64Value(update.BaseConcurrencyOverride.Value), true)
	}
	if update.SkipWarmTier.Set {
		add("skip_warm_tier", update.SkipWarmTier.Value, true)
	}
	if update.Tags.Set {
		if db.isSQLite() {
			add("tags", encodeTagsJSON(update.Tags.Values), true)
		} else {
			args = append(args, encodeTagsJSON(update.Tags.Values))
			sets = append(sets, fmt.Sprintf("tags = $%d::jsonb", len(args)))
			touchUpdatedAt = true
		}
	}
	if update.ProxyURL.Set {
		add("proxy_url", strings.TrimSpace(update.ProxyURL.Value), true)
	}
	if len(sets) == 0 {
		return nil
	}

	if touchUpdatedAt {
		sets = append(sets, "updated_at = CURRENT_TIMESTAMP")
	}
	start := len(args) + 1
	placeholders := dbPlaceholders(db.isSQLite(), start, len(ids))
	args = append(args, argsFromInt64s(ids)...)
	query := fmt.Sprintf("UPDATE accounts SET %s WHERE id IN (%s)", strings.Join(sets, ", "), strings.Join(placeholders, ","))
	_, err := tx.ExecContext(ctx, query, args...)
	return err
}

func (db *DB) batchUpdateAccountCredentials(ctx context.Context, tx *sql.Tx, current map[int64]map[string]interface{}, updates map[string]interface{}) error {
	query := `UPDATE accounts SET credentials = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`
	identityQuery := `UPDATE accounts SET credentials = ?, credential_generation = credential_generation + 1, updated_at = CURRENT_TIMESTAMP WHERE id = ?`
	if !db.isSQLite() {
		query = `UPDATE accounts SET credentials = $1::jsonb, updated_at = CURRENT_TIMESTAMP WHERE id = $2`
		identityQuery = `UPDATE accounts SET credentials = $1::jsonb, credential_generation = credential_generation + 1, updated_at = CURRENT_TIMESTAMP WHERE id = $2`
	}
	for id, credentials := range current {
		// Each account can start with a different value. Compare independently
		// so an idempotent update for one row does not inherit another row's
		// generation bump.
		merged := mergeCredentialMaps(cloneCredentialUpdates(credentials), updates)
		identityChanged := grokIdentityCredentialChanged(credentials, merged)
		credJSON, err := json.Marshal(encryptSensitiveCredentials(merged))
		if err != nil {
			return fmt.Errorf("序列化 credentials 失败: %w", err)
		}
		updateQuery := query
		if identityChanged {
			updateQuery = identityQuery
		}
		if _, err := tx.ExecContext(ctx, updateQuery, credJSON, id); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) batchReplaceAccountGroups(ctx context.Context, tx *sql.Tx, accountIDs []int64, groupIDs []int64) error {
	placeholders := dbPlaceholders(db.isSQLite(), 1, len(accountIDs))
	args := argsFromInt64s(accountIDs)
	if _, err := tx.ExecContext(ctx, fmt.Sprintf("DELETE FROM account_group_members WHERE account_id IN (%s)", strings.Join(placeholders, ",")), args...); err != nil {
		return err
	}

	groupIDs = normalizeIDSlice(groupIDs)
	if len(groupIDs) == 0 {
		return nil
	}
	insertQ := "INSERT INTO account_group_members (account_id, group_id) VALUES ($1, $2)"
	if db.isSQLite() {
		insertQ = "INSERT INTO account_group_members (account_id, group_id) VALUES (?, ?)"
	}
	for _, accountID := range accountIDs {
		for _, groupID := range groupIDs {
			if _, err := tx.ExecContext(ctx, insertQ, accountID, groupID); err != nil {
				return err
			}
		}
	}
	return nil
}

func dbPlaceholders(sqlite bool, start, n int) []string {
	placeholders := make([]string, n)
	for i := 0; i < n; i++ {
		if sqlite {
			placeholders[i] = "?"
		} else {
			placeholders[i] = fmt.Sprintf("$%d", start+i)
		}
	}
	return placeholders
}

func argsFromInt64s(ids []int64) []interface{} {
	args := make([]interface{}, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	return args
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

// UpdateAccountNote 设置账号备注（通用标识字段，如自助提交联系人）。
func (db *DB) UpdateAccountNote(ctx context.Context, id int64, note string) error {
	res, err := db.conn.ExecContext(ctx, `UPDATE accounts SET note = $1, updated_at = CURRENT_TIMESTAMP WHERE id = $2`, note, id)
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

// SetAccountTags 覆盖设置账号标签（JSON 数组），dialect 安全。
func (db *DB) SetAccountTags(ctx context.Context, id int64, tags []string) error {
	encoded := encodeTagsJSON(tags)
	var query string
	if db.isSQLite() {
		query = `UPDATE accounts SET tags = ?, updated_at = CURRENT_TIMESTAMP WHERE id = ?`
	} else {
		query = `UPDATE accounts SET tags = $1::jsonb, updated_at = CURRENT_TIMESTAMP WHERE id = $2`
	}
	_, err := db.conn.ExecContext(ctx, query, encoded, id)
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
	identityChanged := grokIdentityCredentialChanged(decodeCredentials(currentRaw), merged)
	credJSON, err := json.Marshal(encryptSensitiveCredentials(merged))
	if err != nil {
		return fmt.Errorf("序列化 credentials 失败: %w", err)
	}

	generationUpdate := ""
	if identityChanged {
		generationUpdate = ", credential_generation = credential_generation + 1"
	}
	updateQuery := `UPDATE accounts SET credentials = $1` + generationUpdate + `, updated_at = CURRENT_TIMESTAMP WHERE id = $2`
	if !db.isSQLite() {
		updateQuery = `UPDATE accounts SET credentials = $1::jsonb` + generationUpdate + `, updated_at = CURRENT_TIMESTAMP WHERE id = $2`
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
		if grokIdentityUpdateKeysPresent(credentials) {
			return db.updateCredentialsReadMergeSQLiteUnlocked(ctx, id, credentials)
		}

		args := make([]interface{}, 0, len(credentials)*2+1)
		jsonSetArgs := make([]string, 0, len(credentials)*2)
		argIdx := 1
		for key, value := range credentials {
			if !sqliteJSONSetKeySupported(key) {
				return db.updateCredentialsReadMergeSQLiteUnlocked(ctx, id, credentials)
			}
			// SQLite 逐键写:敏感字段在此处按键加密(密钥未设时 no-op)。
			if _, sensitive := sensitiveCredentialKeys[key]; sensitive {
				if s, isStr := value.(string); isStr {
					value = encryptCredentialValue(key, s)
				}
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
	return db.withSQLiteWriteLock(ctx, func() error {
		return db.updateCredentialsReadMergeSQLiteUnlocked(ctx, id, credentials)
	})
}

func (db *DB) updateCredentialsReadMergeSQLiteUnlocked(ctx context.Context, id int64, credentials map[string]interface{}) error {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var currentRaw interface{}
	if err := tx.QueryRowContext(ctx, `SELECT credentials FROM accounts WHERE id = $1`, id).Scan(&currentRaw); err != nil {
		return err
	}

	current := decodeCredentials(currentRaw)
	merged := mergeCredentialMaps(decodeCredentials(currentRaw), credentials)
	identityChanged := grokIdentityCredentialChanged(current, merged)
	credJSON, err := json.Marshal(encryptSensitiveCredentials(merged))
	if err != nil {
		return fmt.Errorf("序列化 credentials 失败: %w", err)
	}
	generationUpdate := ""
	if identityChanged {
		generationUpdate = ", credential_generation = credential_generation + 1"
	}
	if _, err := tx.ExecContext(ctx, `UPDATE accounts SET credentials = $1`+generationUpdate+`, updated_at = CURRENT_TIMESTAMP WHERE id = $2`, credJSON, id); err != nil {
		return err
	}
	return tx.Commit()
}

var grokIdentityCredentialKeys = map[string]struct{}{
	"access_token": {}, "refresh_token": {}, "api_key": {}, "upstream_type": {},
	"base_url": {}, "grok_client_id": {}, "grok_token_endpoint": {},
	"grok_oidc_issuer": {}, "grok_principal_type": {}, "grok_principal_id": {},
	"account_id": {},
}

func grokIdentityUpdateKeysPresent(updates map[string]interface{}) bool {
	for key := range updates {
		if _, ok := grokIdentityCredentialKeys[key]; ok {
			return true
		}
	}
	return false
}

func grokIdentityCredentialChanged(before, after map[string]interface{}) bool {
	if !strings.EqualFold(strings.TrimSpace(credentialStringFromMap(after, "upstream_type")), "grok") {
		return false
	}
	for key := range grokIdentityCredentialKeys {
		if strings.TrimSpace(credentialStringFromMap(before, key)) != strings.TrimSpace(credentialStringFromMap(after, key)) {
			return true
		}
	}
	return false
}

func openAIResponsesIdentityCredentialChanged(before, after map[string]interface{}) bool {
	if !strings.EqualFold(strings.TrimSpace(credentialStringFromMap(after, "upstream_type")), "openai_responses") {
		return false
	}
	return strings.TrimRight(strings.TrimSpace(credentialStringFromMap(before, "base_url")), "/") !=
		strings.TrimRight(strings.TrimSpace(credentialStringFromMap(after, "base_url")), "/") ||
		strings.TrimSpace(credentialStringFromMap(before, "api_key")) !=
			strings.TrimSpace(credentialStringFromMap(after, "api_key"))
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

	current := decodeCredentials(currentRaw)
	merged := mergeCredentialMaps(cloneCredentialUpdates(current), credentials)
	identityChanged := openAIResponsesIdentityCredentialChanged(current, merged)
	credJSON, err := json.Marshal(encryptSensitiveCredentials(merged))
	if err != nil {
		return fmt.Errorf("序列化 credentials 失败: %w", err)
	}

	identityUpdate := ""
	if identityChanged {
		identityUpdate = ", credential_generation = credential_generation + 1, status = 'active', error_message = '', cooldown_reason = '', cooldown_until = NULL"
	}
	updateQuery := `UPDATE accounts SET name = $1, credentials = $2, proxy_url = $3, platform = 'openai', type = 'responses_api'` + identityUpdate + `, updated_at = CURRENT_TIMESTAMP WHERE id = $4`
	if !db.isSQLite() {
		updateQuery = `UPDATE accounts SET name = $1, credentials = $2::jsonb, proxy_url = $3, platform = 'openai', type = 'responses_api'` + identityUpdate + `, updated_at = CURRENT_TIMESTAMP WHERE id = $4`
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
	if identityChanged {
		if _, err := tx.ExecContext(ctx, `DELETE FROM account_model_cooldowns WHERE account_id = $1`, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (db *DB) UpdateOAuthAccountCredentials(ctx context.Context, id int64, credentials map[string]interface{}, proxyURL string) error {
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
	credJSON, err := json.Marshal(encryptSensitiveCredentials(merged))
	if err != nil {
		return fmt.Errorf("序列化 credentials 失败: %w", err)
	}

	updateQuery := `UPDATE accounts SET credentials = $1, proxy_url = $2, platform = 'openai', type = 'oauth', updated_at = CURRENT_TIMESTAMP WHERE id = $3`
	if !db.isSQLite() {
		updateQuery = `UPDATE accounts SET credentials = $1::jsonb, proxy_url = $2, platform = 'openai', type = 'oauth', updated_at = CURRENT_TIMESTAMP WHERE id = $3`
	}
	res, err := tx.ExecContext(ctx, updateQuery, credJSON, proxyURL, id)
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
	return db.withSQLiteWriteLock(ctx, func() error {
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
		// Keep the last group membership snapshot on the soft-deleted account.
		// Usage reports need it to attribute historical requests after an account
		// moves to the recycle bin. Active-account queries already exclude deleted
		// accounts, while restoring the account reuses the retained memberships.
		return tx.Commit()
	})
}

// ListDeleted 获取回收站中的账号（被软删除、尚未彻底清除的账号）。
func (db *DB) ListDeleted(ctx context.Context) ([]*AccountRow, error) {
	query := `
		SELECT id, name, platform, type, credentials, proxy_url, status, cooldown_reason, cooldown_until, error_message, COALESCE(enabled, true), COALESCE(locked, false), COALESCE(credit_enabled, false), COALESCE(credit_skip_usage_window, false), COALESCE(skip_warm_tier, false), score_bias_override, base_concurrency_override, COALESCE(tags, '[]'), COALESCE(note, ''), created_at, updated_at, deleted_at, COALESCE(credential_generation, 1), COALESCE(credential_family_id, '')
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
			&a.Note,
			&createdAtRaw,
			&updatedAtRaw,
			&deletedAtRaw,
			&a.CredentialGeneration,
			&a.CredentialFamilyID,
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
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Grok observations intentionally do not rely on foreign-key cascades; keep
	// purge behavior consistent across SQLite and PostgreSQL.
	if err := db.deleteGrokAccountStateTx(ctx, tx, "= $1", id); err != nil {
		return err
	}
	query := `DELETE FROM accounts WHERE id = $1 AND (status = 'deleted' OR COALESCE(error_message, '') = 'deleted')`
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

// PurgeDeletedAccounts 清空回收站，返回被彻底删除的账号数量。
func (db *DB) PurgeDeletedAccounts(ctx context.Context) (int64, error) {
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	deletedPredicate := `IN (SELECT id FROM accounts WHERE status = 'deleted' OR COALESCE(error_message, '') = 'deleted')`
	if err := db.deleteGrokAccountStateTx(ctx, tx, deletedPredicate); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM account_group_members
		WHERE account_id IN (SELECT id FROM accounts WHERE status = 'deleted' OR COALESCE(error_message, '') = 'deleted')
	`); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM accounts WHERE status = 'deleted' OR COALESCE(error_message, '') = 'deleted'`)
	if err != nil {
		return 0, err
	}
	count, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}

// BatchSoftDeleteAccounts 批量软删除账号，分批执行避免 SQL 参数过多。
func (db *DB) BatchSoftDeleteAccounts(ctx context.Context, ids []int64) error {
	return db.withSQLiteWriteLock(ctx, func() error {
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
	})
}

// BatchInsertAccountEvents 批量插入账号事件。
func (db *DB) BatchInsertAccountEvents(ctx context.Context, ids []int64, eventType string, source string) error {
	if len(ids) == 0 {
		return nil
	}
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
			return fmt.Errorf("批量插入账号事件 (%d 条): %w", len(batch), err)
		}
	}
	return nil
}

// BatchInsertAccountEventsAsync 批量异步插入账号事件。
func (db *DB) BatchInsertAccountEventsAsync(ids []int64, eventType string, source string) {
	if len(ids) == 0 {
		return
	}
	ids = append([]int64(nil), ids...)
	db.RunBackgroundTask(func(parent context.Context) {
		ctx, cancel := context.WithTimeout(parent, 30*time.Second)
		defer cancel()
		if err := db.BatchInsertAccountEvents(ctx, ids, eventType, source); err != nil {
			log.Printf("[账号事件] %v", err)
		}
	})
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
	credJSON, err := json.Marshal(encryptSensitiveCredentials(credentials))
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
	credJSON, err := json.Marshal(encryptSensitiveCredentials(credentials))
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
	credJSON, err := json.Marshal(encryptSensitiveCredentials(credentials))
	if err != nil {
		return 0, err
	}

	return db.insertAccountRowWithFamily(ctx,
		`INSERT INTO accounts (name, credentials, proxy_url) VALUES ($1, $2, $3) RETURNING id`,
		`INSERT INTO accounts (name, credentials, proxy_url) VALUES ($1, $2, $3)`,
		credentials,
		name, credJSON, proxyURL,
	)
}

func (db *DB) InsertOpenAIResponsesAccount(ctx context.Context, name string, credentials map[string]interface{}, proxyURL string) (int64, error) {
	if credentials == nil {
		credentials = map[string]interface{}{}
	}
	credJSON, err := json.Marshal(encryptSensitiveCredentials(credentials))
	if err != nil {
		return 0, err
	}

	return db.insertAccountRowWithFamily(ctx,
		`INSERT INTO accounts (name, platform, type, credentials, proxy_url) VALUES ($1, 'openai', 'responses_api', $2, $3) RETURNING id`,
		`INSERT INTO accounts (name, platform, type, credentials, proxy_url) VALUES ($1, 'openai', 'responses_api', $2, $3)`,
		credentials,
		name, credJSON, proxyURL,
	)
}

// InsertAccountWithUpstream 插入一个指定 platform / type 的账号（用于 Grok 等
// 非 Codex 上游），credentials 全量入库。
func (db *DB) InsertAccountWithUpstream(ctx context.Context, name, platform, accountType string, credentials map[string]interface{}, proxyURL string) (int64, error) {
	if credentials == nil {
		credentials = map[string]interface{}{}
	}
	if strings.TrimSpace(platform) == "" {
		platform = "xai"
	}
	if strings.TrimSpace(accountType) == "" {
		accountType = "api"
	}
	credJSON, err := json.Marshal(encryptSensitiveCredentials(credentials))
	if err != nil {
		return 0, err
	}
	return db.insertAccountRowWithFamily(ctx,
		`INSERT INTO accounts (name, platform, type, credentials, proxy_url) VALUES ($1, $2, $3, $4, $5) RETURNING id`,
		`INSERT INTO accounts (name, platform, type, credentials, proxy_url) VALUES ($1, $2, $3, $4, $5)`,
		credentials,
		name, platform, accountType, credJSON, proxyURL,
	)
}

// insertAccountRowWithFamily keeps legacy insert SQL untouched while ensuring
// every newly-created account immediately receives its stable family key.
func (db *DB) insertAccountRowWithFamily(ctx context.Context, postgresQuery, sqliteQuery string, credentials map[string]interface{}, args ...interface{}) (int64, error) {
	candidate := credentialFamilyCandidate(credentials)
	returnID := int64(0)
	err := db.withSQLiteWriteLock(ctx, func() error {
		tx, err := db.conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if db.isSQLite() {
			query := strings.TrimSpace(sqliteQuery)
			if _, err = tx.ExecContext(ctx, query, args...); err != nil {
				return err
			}
			if err = tx.QueryRowContext(ctx, "SELECT last_insert_rowid()").Scan(&returnID); err != nil {
				return err
			}
		} else if err = tx.QueryRowContext(ctx, strings.TrimSpace(postgresQuery), args...).Scan(&returnID); err != nil {
			return err
		}
		if strings.TrimSpace(candidate) == "" {
			candidate = "cf_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		}
		if _, err = tx.ExecContext(ctx, `UPDATE accounts SET credential_family_id=$1 WHERE id=$2 AND COALESCE(credential_family_id,'')=''`, candidate, returnID); err != nil {
			return err
		}
		return tx.Commit()
	})
	if err != nil {
		return 0, err
	}
	return returnID, nil
}

// UpdateAccountName 仅更新账号名称。
func (db *DB) UpdateAccountName(ctx context.Context, id int64, name string) error {
	res, err := db.conn.ExecContext(ctx,
		`UPDATE accounts SET name = $1, updated_at = CURRENT_TIMESTAMP WHERE id = $2`, name, id)
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

// FindActiveAccountByOAuthIdentity returns the first non-deleted account with
// the same normalized email and non-empty workspace_id.
func (db *DB) FindActiveAccountByOAuthIdentity(ctx context.Context, email, workspaceID string, excludeIDs ...int64) (int64, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	workspaceID = openaiidentity.NormalizeWorkspaceID(workspaceID)
	if email == "" || workspaceID == "" {
		return 0, sql.ErrNoRows
	}
	excluded := make(map[int64]struct{}, len(excludeIDs))
	for _, id := range excludeIDs {
		if id > 0 {
			excluded[id] = struct{}{}
		}
	}

	query := `SELECT id, credentials
		FROM accounts
		WHERE status <> 'deleted'
		  AND COALESCE(error_message, '') <> 'deleted'
		  AND LOWER(TRIM(json_extract(credentials, '$.email'))) = ?`
	if db.driver == "postgres" {
		query = `SELECT id, credentials
			FROM accounts
			WHERE status <> 'deleted'
			  AND COALESCE(error_message, '') <> 'deleted'
			  AND LOWER(BTRIM(COALESCE(credentials->>'email', ''))) = $1`
	}
	rows, err := db.conn.QueryContext(ctx, query, email)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	for rows.Next() {
		var id int64
		var raw interface{}
		if err := rows.Scan(&id, &raw); err != nil {
			return 0, err
		}
		if _, ok := excluded[id]; ok {
			continue
		}
		if strings.ToLower(strings.TrimSpace(credentialString(raw, "email"))) != email {
			continue
		}
		if openaiidentity.NormalizeWorkspaceID(credentialString(raw, "workspace_id")) == workspaceID {
			return id, nil
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return 0, sql.ErrNoRows
}

// FindActiveAccountByOAuthRouteIdentity returns the first non-deleted account
// that targets the same effective workspace. A per-account
// Chatgpt-Account-Id header represents a distinct workspace route even when
// multiple routes share the same OAuth token identity.
func (db *DB) FindActiveAccountByOAuthRouteIdentity(ctx context.Context, email, effectiveWorkspaceID string, excludeIDs ...int64) (int64, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	effectiveWorkspaceID = strings.TrimSpace(effectiveWorkspaceID)
	if email == "" || effectiveWorkspaceID == "" {
		return 0, sql.ErrNoRows
	}
	excluded := make(map[int64]struct{}, len(excludeIDs))
	for _, id := range excludeIDs {
		if id > 0 {
			excluded[id] = struct{}{}
		}
	}

	query := `SELECT id, credentials
		FROM accounts
		WHERE status <> 'deleted'
		  AND COALESCE(error_message, '') <> 'deleted'
		  AND LOWER(TRIM(json_extract(credentials, '$.email'))) = ?`
	if db.driver == "postgres" {
		query = `SELECT id, credentials
			FROM accounts
			WHERE status <> 'deleted'
			  AND COALESCE(error_message, '') <> 'deleted'
			  AND LOWER(BTRIM(COALESCE(credentials->>'email', ''))) = $1`
	}
	rows, err := db.conn.QueryContext(ctx, query, email)
	if err != nil {
		return 0, err
	}
	defer rows.Close()

	for rows.Next() {
		var id int64
		var raw interface{}
		if err := rows.Scan(&id, &raw); err != nil {
			return 0, err
		}
		if _, ok := excluded[id]; ok {
			continue
		}
		if strings.ToLower(strings.TrimSpace(credentialString(raw, "email"))) != email {
			continue
		}
		candidateWorkspaceID := openaiidentity.EffectiveWorkspaceID(
			credentialString(raw, "workspace_id"),
			credentialStringMap(raw, "custom_headers"),
		)
		if candidateWorkspaceID == effectiveWorkspaceID {
			return id, nil
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return 0, sql.ErrNoRows
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
	db.RunBackgroundTask(func(ctx context.Context) {
		var err error
		for attempt := 0; attempt < 3; attempt++ {
			attemptCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err = db.InsertAccountEvent(attemptCtx, accountID, eventType, source)
			cancel()
			if err == nil {
				return
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Duration(attempt+1) * 500 * time.Millisecond):
			}
		}
		if err != nil {
			log.Printf("[账号事件] 记录失败（已重试3次）: account=%d type=%s source=%s err=%v", accountID, eventType, source, err)
		}
	})
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
