package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	_ "github.com/lib/pq"
)

// AccountRow 数据库中的账号行
type AccountRow struct {
	ID           int64
	Name         string
	Platform     string
	Type         string
	Credentials  map[string]interface{}
	ProxyURL     string
	Status       string
	ErrorMessage string
	CreatedAt    time.Time
	UpdatedAt    time.Time
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

// DB PostgreSQL 数据库操作
type DB struct {
	conn *sql.DB

	// 使用日志批量写入缓冲
	logBuf   []usageLogEntry
	logMu    sync.Mutex
	logStop  chan struct{}
	logWg    sync.WaitGroup
}

// usageLogEntry 日志缓冲条目
type usageLogEntry struct {
	AccountID        int64
	Endpoint         string
	Model            string
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	StatusCode       int
	DurationMs       int
	// 新增字段
	InputTokens      int
	OutputTokens     int
	ReasoningTokens  int
	FirstTokenMs     int
	ReasoningEffort  string
	InboundEndpoint  string
	UpstreamEndpoint string
	Stream           bool
}

// New 创建数据库连接并自动建表
func New(dsn string) (*DB, error) {
	conn, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("连接数据库失败: %w", err)
	}

	// ==================== 连接池优化 ====================
	// 高并发场景：大量 RT 刷新 + 前端查询 + 使用日志写入 并行
	conn.SetMaxOpenConns(50)              // 最大打开连接数（默认无限制，限制避免 PG too many connections）
	conn.SetMaxIdleConns(25)              // 空闲连接数（保持足够的热连接避免频繁建连）
	conn.SetConnMaxLifetime(30 * time.Minute) // 连接最大生存时间（避免长连接僵死）
	conn.SetConnMaxIdleTime(10 * time.Minute) // 空闲连接最大闲置时间

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := conn.PingContext(ctx); err != nil {
		return nil, fmt.Errorf("数据库连接测试失败: %w", err)
	}

	db := &DB{
		conn:    conn,
		logStop: make(chan struct{}),
	}
	if err := db.migrate(ctx); err != nil {
		return nil, fmt.Errorf("数据库迁移失败: %w", err)
	}

	// 启动批量写入后台协程
	db.startLogFlusher()

	return db, nil
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
		created_at    TIMESTAMP DEFAULT NOW(),
		updated_at    TIMESTAMP DEFAULT NOW()
	);

	CREATE INDEX IF NOT EXISTS idx_accounts_status ON accounts(status);
	CREATE INDEX IF NOT EXISTS idx_accounts_platform ON accounts(platform);

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
		created_at     TIMESTAMP DEFAULT NOW()
	);

	-- 复合索引
	CREATE INDEX IF NOT EXISTS idx_usage_logs_created_at ON usage_logs(created_at);
	CREATE INDEX IF NOT EXISTS idx_usage_logs_account_id ON usage_logs(account_id);
	CREATE INDEX IF NOT EXISTS idx_usage_logs_created_status ON usage_logs(created_at, status_code);

	-- 增强字段（向后兼容 ALTER）
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS input_tokens INT DEFAULT 0;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS output_tokens INT DEFAULT 0;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS reasoning_tokens INT DEFAULT 0;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS first_token_ms INT DEFAULT 0;
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS reasoning_effort VARCHAR(20) DEFAULT '';
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS inbound_endpoint VARCHAR(100) DEFAULT '';
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS upstream_endpoint VARCHAR(100) DEFAULT '';
	ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS stream BOOLEAN DEFAULT false;

	CREATE TABLE IF NOT EXISTS api_keys (
		id         SERIAL PRIMARY KEY,
		name       VARCHAR(255) DEFAULT '',
		key        VARCHAR(255) NOT NULL UNIQUE,
		created_at TIMESTAMP DEFAULT NOW()
	);
	`
	_, err := db.conn.ExecContext(ctx, query)
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
		if err := rows.Scan(&k.ID, &k.Name, &k.Key, &k.CreatedAt); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}

// InsertAPIKey 插入新 API 密钥
func (db *DB) InsertAPIKey(ctx context.Context, name, key string) (int64, error) {
	var id int64
	err := db.conn.QueryRowContext(ctx,
		`INSERT INTO api_keys (name, key) VALUES ($1, $2) RETURNING id`, name, key).Scan(&id)
	return id, err
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

// ==================== Usage Logs（批量写入） ====================

// UsageLog 请求日志行
type UsageLog struct {
	ID               int64     `json:"id"`
	AccountID        int64     `json:"account_id"`
	Endpoint         string    `json:"endpoint"`
	Model            string    `json:"model"`
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
	CreatedAt        time.Time `json:"created_at"`
}

// InsertUsageLog 将日志追加到内存缓冲（非阻塞）
func (db *DB) InsertUsageLog(ctx context.Context, log *UsageLogInput) error {
	db.logMu.Lock()
	db.logBuf = append(db.logBuf, usageLogEntry{
		AccountID:        log.AccountID,
		Endpoint:         log.Endpoint,
		Model:            log.Model,
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
	})
	bufLen := len(db.logBuf)
	db.logMu.Unlock()

	if bufLen >= 100 {
		go db.flushLogs()
	}
	return nil
}

// UsageLogInput 日志写入参数
type UsageLogInput struct {
	AccountID        int64
	Endpoint         string
	Model            string
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
}

// startLogFlusher 启动后台定时 flush 协程（每 3 秒一次）
func (db *DB) startLogFlusher() {
	db.logWg.Add(1)
	go func() {
		defer db.logWg.Done()
		ticker := time.NewTicker(3 * time.Second)
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
	db.logBuf = make([]usageLogEntry, 0, 64)
	db.logMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		log.Printf("批量写入日志失败（开始事务）: %v", err)
		return
	}

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO usage_logs (account_id, endpoint, model, prompt_tokens, completion_tokens, total_tokens, status_code, duration_ms,
		  input_tokens, output_tokens, reasoning_tokens, first_token_ms, reasoning_effort, inbound_endpoint, upstream_endpoint, stream)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)`)
	if err != nil {
		tx.Rollback()
		log.Printf("批量写入日志失败（准备语句）: %v", err)
		return
	}
	defer stmt.Close()

	for _, e := range batch {
		if _, err := stmt.ExecContext(ctx, e.AccountID, e.Endpoint, e.Model, e.PromptTokens, e.CompletionTokens, e.TotalTokens, e.StatusCode, e.DurationMs,
			e.InputTokens, e.OutputTokens, e.ReasoningTokens, e.FirstTokenMs, e.ReasoningEffort, e.InboundEndpoint, e.UpstreamEndpoint, e.Stream); err != nil {
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

// UsageStats 使用统计
type UsageStats struct {
	TotalRequests   int64   `json:"total_requests"`
	TotalTokens     int64   `json:"total_tokens"`
	TotalPrompt     int64   `json:"total_prompt_tokens"`
	TotalCompletion int64   `json:"total_completion_tokens"`
	TodayRequests   int64   `json:"today_requests"`
	TodayTokens     int64   `json:"today_tokens"`
	RPM             float64 `json:"rpm"`
	TPM             float64 `json:"tpm"`
	AvgDurationMs   float64 `json:"avg_duration_ms"`
	ErrorRate       float64 `json:"error_rate"`
}

// GetUsageStats 获取使用统计（单条 SQL，避免 5 次查询）
func (db *DB) GetUsageStats(ctx context.Context) (*UsageStats, error) {
	stats := &UsageStats{}

	// 合并为单条 SQL：使用条件聚合（FILTER 子句），一次扫描完成所有统计
	query := `
	SELECT
		-- 总量
		COUNT(*)                                                     AS total_requests,
		COALESCE(SUM(total_tokens), 0)                               AS total_tokens,
		COALESCE(SUM(prompt_tokens), 0)                              AS total_prompt,
		COALESCE(SUM(completion_tokens), 0)                          AS total_completion,
		-- 今日
		COUNT(*)    FILTER (WHERE created_at >= CURRENT_DATE)        AS today_requests,
		COALESCE(SUM(total_tokens) FILTER (WHERE created_at >= CURRENT_DATE), 0) AS today_tokens,
		-- RPM / TPM（最近 1 分钟）
		COUNT(*)    FILTER (WHERE created_at >= NOW() - INTERVAL '1 minute')     AS rpm,
		COALESCE(SUM(total_tokens) FILTER (WHERE created_at >= NOW() - INTERVAL '1 minute'), 0) AS tpm,
		-- 平均延迟（今日）
		COALESCE(AVG(duration_ms) FILTER (WHERE created_at >= CURRENT_DATE), 0)  AS avg_duration_ms,
		-- 今日错误数
		COUNT(*)    FILTER (WHERE created_at >= CURRENT_DATE AND status_code >= 400) AS today_errors
	FROM usage_logs
	`

	var todayErrors int64
	err := db.conn.QueryRowContext(ctx, query).Scan(
		&stats.TotalRequests, &stats.TotalTokens, &stats.TotalPrompt, &stats.TotalCompletion,
		&stats.TodayRequests, &stats.TodayTokens,
		&stats.RPM, &stats.TPM,
		&stats.AvgDurationMs,
		&todayErrors,
	)
	if err != nil {
		return nil, err
	}

	if stats.TodayRequests > 0 {
		stats.ErrorRate = float64(todayErrors) / float64(stats.TodayRequests) * 100
	}

	return stats, nil
}

// ListRecentUsageLogs 获取最近的请求日志
func (db *DB) ListRecentUsageLogs(ctx context.Context, limit int) ([]*UsageLog, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	query := `SELECT id, account_id, endpoint, model, prompt_tokens, completion_tokens, total_tokens, status_code, duration_ms,
	            COALESCE(input_tokens, 0), COALESCE(output_tokens, 0), COALESCE(reasoning_tokens, 0),
	            COALESCE(first_token_ms, 0), COALESCE(reasoning_effort, ''), COALESCE(inbound_endpoint, ''),
	            COALESCE(upstream_endpoint, ''), COALESCE(stream, false), created_at
	           FROM usage_logs ORDER BY id DESC LIMIT $1`
	rows, err := db.conn.QueryContext(ctx, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var logs []*UsageLog
	for rows.Next() {
		l := &UsageLog{}
		if err := rows.Scan(&l.ID, &l.AccountID, &l.Endpoint, &l.Model, &l.PromptTokens, &l.CompletionTokens, &l.TotalTokens, &l.StatusCode, &l.DurationMs,
			&l.InputTokens, &l.OutputTokens, &l.ReasoningTokens, &l.FirstTokenMs, &l.ReasoningEffort, &l.InboundEndpoint, &l.UpstreamEndpoint, &l.Stream, &l.CreatedAt); err != nil {
			return nil, err
		}
		logs = append(logs, l)
	}
	return logs, rows.Err()
}

// ==================== Accounts ====================

// ListActive 获取所有状态为 active 的账号
func (db *DB) ListActive(ctx context.Context) ([]*AccountRow, error) {
	query := `
		SELECT id, name, platform, type, credentials, proxy_url, status, error_message, created_at, updated_at
		FROM accounts
		WHERE status = 'active'
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
		var credJSON []byte
		if err := rows.Scan(&a.ID, &a.Name, &a.Platform, &a.Type, &credJSON, &a.ProxyURL, &a.Status, &a.ErrorMessage, &a.CreatedAt, &a.UpdatedAt); err != nil {
			return nil, fmt.Errorf("扫描账号行失败: %w", err)
		}
		if err := json.Unmarshal(credJSON, &a.Credentials); err != nil {
			log.Printf("[账号 %d] 解析 credentials 失败: %v", a.ID, err)
			a.Credentials = make(map[string]interface{})
		}
		accounts = append(accounts, a)
	}
	return accounts, rows.Err()
}

// UpdateCredentials 原子合并更新账号的 credentials（JSONB || 运算符，不覆盖已有字段）
// 解决并发刷新时一个进程覆盖另一个进程写入的字段的问题
func (db *DB) UpdateCredentials(ctx context.Context, id int64, credentials map[string]interface{}) error {
	credJSON, err := json.Marshal(credentials)
	if err != nil {
		return fmt.Errorf("序列化 credentials 失败: %w", err)
	}

	// 使用 || 运算符原子合并 JSONB，而非整体覆盖
	// 例如：进程 A 更新 access_token，进程 B 同时更新 email，两者不会互相覆盖
	query := `UPDATE accounts SET credentials = credentials || $1::jsonb, updated_at = NOW() WHERE id = $2`
	_, err = db.conn.ExecContext(ctx, query, credJSON, id)
	return err
}

// SetError 标记账号错误状态
func (db *DB) SetError(ctx context.Context, id int64, errorMsg string) error {
	query := `UPDATE accounts SET status = 'error', error_message = $1, updated_at = NOW() WHERE id = $2`
	_, err := db.conn.ExecContext(ctx, query, errorMsg, id)
	return err
}

// ClearError 清除账号错误状态
func (db *DB) ClearError(ctx context.Context, id int64) error {
	query := `UPDATE accounts SET status = 'active', error_message = '', updated_at = NOW() WHERE id = $1`
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

	var id int64
	query := `INSERT INTO accounts (name, credentials, proxy_url) VALUES ($1, $2, $3) RETURNING id`
	err = db.conn.QueryRowContext(ctx, query, name, credJSON, proxyURL).Scan(&id)
	return id, err
}

// CountAll 获取账号总数
func (db *DB) CountAll(ctx context.Context) (int, error) {
	var count int
	err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts`).Scan(&count)
	return count, err
}
