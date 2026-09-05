package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Prompt 审核日志保留策略。
//
// 三张"日志型"表会随流量无限增长：prompt_filter_logs（本地过滤 / 复核日志）、
// prompt_risk_events（风险事件）、prompt_risk_event_sources（事件来源去重表）。
// 保留策略按天数清理过期行，但**与仍存在的上游 CY 记录（prompt_policy_incidents）
// 关联的行永不清理**——它们是 CY 的证据链。管理员删除 CY 记录时，关联的审核日志随之
// 级联删除；风险画像保留，之后按保留天数自然过期。
//
// 删除一律分批（默认 5000 行/批）并在批间让出写锁，避免一条大 DELETE 长时间锁住
// SQLite；清理循环直到没有可删行或上下文结束。

const (
	DefaultPromptLogRetentionDays = 7
	MaxPromptLogRetentionDays     = 365
	DefaultPromptLogPurgeBatch    = 5000
)

type PromptLogRetentionConfig struct {
	RetentionDays      int          `json:"retention_days"`
	LastRunAt          sql.NullTime `json:"-"`
	LastDeletedLogs    int64        `json:"last_deleted_logs"`
	LastDeletedEvents  int64        `json:"last_deleted_events"`
	LastDeletedSources int64        `json:"last_deleted_sources"`
	LastDurationMs     int64        `json:"last_duration_ms"`
	LastError          string       `json:"last_error,omitempty"`
}

// PromptLogPurgeResult 是一次清理的统计。Interrupted 表示因上下文结束提前停止，
// 剩余过期行会留给下一轮。
type PromptLogPurgeResult struct {
	Logs        int64 `json:"logs"`
	Events      int64 `json:"events"`
	Sources     int64 `json:"sources"`
	Batches     int   `json:"batches"`
	Interrupted bool  `json:"interrupted"`
}

// PromptLogPurgeFilter 限定 prompt_filter_logs 的清理范围（手动清空按钮用）；
// 零值表示按 Cutoff 清理全部来源。
type PromptLogPurgeFilter struct {
	Reviewed *bool
	Source   string
}

var (
	promptRetentionConfigInitMu sync.Mutex
	promptRetentionConfigReady  = make(map[*DB]bool)
)

func NormalizePromptLogRetentionDays(days int) int {
	if days < 0 {
		return 0
	}
	if days > MaxPromptLogRetentionDays {
		return MaxPromptLogRetentionDays
	}
	return days
}

func (db *DB) ensurePromptLogRetentionConfig(ctx context.Context) error {
	if db == nil || db.conn == nil {
		return fmt.Errorf("数据库不可用")
	}
	promptRetentionConfigInitMu.Lock()
	defer promptRetentionConfigInitMu.Unlock()
	if promptRetentionConfigReady[db] {
		return nil
	}
	if _, err := db.conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS prompt_log_retention_config (
		singleton_id INTEGER PRIMARY KEY CHECK (singleton_id = 1),
		retention_days INTEGER NOT NULL DEFAULT 7,
		last_run_at TIMESTAMP NULL,
		last_deleted_logs BIGINT NOT NULL DEFAULT 0,
		last_deleted_events BIGINT NOT NULL DEFAULT 0,
		last_deleted_sources BIGINT NOT NULL DEFAULT 0,
		last_duration_ms BIGINT NOT NULL DEFAULT 0,
		last_error TEXT NOT NULL DEFAULT ''
	)`); err != nil {
		return err
	}
	_, err := db.conn.ExecContext(ctx, `INSERT INTO prompt_log_retention_config (singleton_id, retention_days)
		VALUES (1, $1) ON CONFLICT (singleton_id) DO NOTHING`, DefaultPromptLogRetentionDays)
	if err == nil {
		promptRetentionConfigReady[db] = true
	}
	return err
}

func (db *DB) GetPromptLogRetentionConfig(ctx context.Context) (*PromptLogRetentionConfig, error) {
	if err := db.ensurePromptLogRetentionConfig(ctx); err != nil {
		return nil, err
	}
	var cfg PromptLogRetentionConfig
	err := db.conn.QueryRowContext(ctx, `SELECT retention_days, last_run_at, last_deleted_logs, last_deleted_events,
		last_deleted_sources, last_duration_ms, COALESCE(last_error, '')
		FROM prompt_log_retention_config WHERE singleton_id = 1`).Scan(
		&cfg.RetentionDays, &cfg.LastRunAt, &cfg.LastDeletedLogs, &cfg.LastDeletedEvents,
		&cfg.LastDeletedSources, &cfg.LastDurationMs, &cfg.LastError,
	)
	if err != nil {
		return nil, err
	}
	cfg.RetentionDays = NormalizePromptLogRetentionDays(cfg.RetentionDays)
	return &cfg, nil
}

func (db *DB) UpdatePromptLogRetentionDays(ctx context.Context, days int) (*PromptLogRetentionConfig, error) {
	if err := db.ensurePromptLogRetentionConfig(ctx); err != nil {
		return nil, err
	}
	days = NormalizePromptLogRetentionDays(days)
	if _, err := db.conn.ExecContext(ctx, `UPDATE prompt_log_retention_config SET retention_days = $1 WHERE singleton_id = 1`, days); err != nil {
		return nil, err
	}
	return db.GetPromptLogRetentionConfig(ctx)
}

func (db *DB) RecordPromptLogRetentionRun(ctx context.Context, ranAt time.Time, result PromptLogPurgeResult, duration time.Duration, runErr error) error {
	if err := db.ensurePromptLogRetentionConfig(ctx); err != nil {
		return err
	}
	lastError := ""
	if runErr != nil {
		lastError = strings.TrimSpace(runErr.Error())
		if len(lastError) > 1000 {
			lastError = lastError[:1000]
		}
	}
	_, err := db.conn.ExecContext(ctx, `UPDATE prompt_log_retention_config SET
		last_run_at = $1, last_deleted_logs = $2, last_deleted_events = $3, last_deleted_sources = $4,
		last_duration_ms = $5, last_error = $6
		WHERE singleton_id = 1`,
		db.timeArg(ranAt.UTC()), result.Logs, result.Events, result.Sources, duration.Milliseconds(), lastError)
	return err
}

// ==================== 清理 ====================

// promptLogProtectedByIncidentSQL 是"该日志行受 CY 记录保护"的条件（l 为 prompt_filter_logs 别名）：
// 与某条仍存在的 CY 共享 request_correlation_id（CY 与本地审核日志由同一请求的
// correlation id 关联；不反查 prompt_risk_events，该表没有 prompt_filter_log_id 索引）。
const promptLogProtectedByIncidentSQL = `(l.request_correlation_id <> '' AND EXISTS (
	SELECT 1 FROM prompt_policy_incidents i WHERE i.request_correlation_id = l.request_correlation_id))`

// promptEventProtectedSQL 是"该风险事件受保护"的条件（e 为 prompt_risk_events 别名）：
// 挂在仍存在的 CY 上，或其来源日志仍存在（日志本身受保护或尚未过期）。
const promptEventProtectedSQL = `(
	(e.incident_id <> '' AND EXISTS (SELECT 1 FROM prompt_policy_incidents i WHERE i.incident_id = e.incident_id))
	OR (e.prompt_filter_log_id > 0 AND EXISTS (SELECT 1 FROM prompt_filter_logs l WHERE l.id = e.prompt_filter_log_id)))`

// PurgeExpiredPromptLogs 按 cutoff 分批清理三张日志表中过期且不受 CY 保护的行。
// 顺序：日志 → 风险事件 → 事件来源，保证后一张表的保护判定能看到前一张表的最终状态。
func (db *DB) PurgeExpiredPromptLogs(ctx context.Context, cutoff time.Time, batchSize int, pause time.Duration) (PromptLogPurgeResult, error) {
	return db.purgePromptLogs(ctx, cutoff, PromptLogPurgeFilter{}, batchSize, pause, true)
}

// PurgePromptFilterLogs 只清理 prompt_filter_logs（手动清空按钮）：
// 同样跳过 CY 关联行；风险事件与来源记录不动（与"风险画像已保留"的既有语义一致）。
func (db *DB) PurgePromptFilterLogs(ctx context.Context, cutoff time.Time, filter PromptLogPurgeFilter, batchSize int, pause time.Duration) (PromptLogPurgeResult, error) {
	return db.purgePromptLogs(ctx, cutoff, filter, batchSize, pause, false)
}

func (db *DB) purgePromptLogs(ctx context.Context, cutoff time.Time, filter PromptLogPurgeFilter, batchSize int, pause time.Duration, includeEvents bool) (PromptLogPurgeResult, error) {
	var result PromptLogPurgeResult
	if db == nil || db.conn == nil {
		return result, fmt.Errorf("数据库不可用")
	}
	if batchSize <= 0 {
		batchSize = DefaultPromptLogPurgeBatch
	}
	// 三张表都按需建表；清理前确保存在，避免在从未产生过 CY / 风险事件的部署上报错。
	if err := db.ensurePromptPolicyIncidentsTable(ctx); err != nil {
		return result, err
	}
	if err := db.ensurePromptRiskEventsTable(ctx); err != nil {
		return result, err
	}
	cutoffArg := db.timeArg(cutoff.UTC())

	logWhere := `l.created_at < $1 AND NOT ` + promptLogProtectedByIncidentSQL
	logArgs := []interface{}{cutoffArg}
	if filter.Reviewed != nil {
		logArgs = append(logArgs, *filter.Reviewed)
		logWhere += fmt.Sprintf(` AND l.reviewed = $%d`, len(logArgs))
	}
	if source := strings.TrimSpace(filter.Source); source != "" {
		logArgs = append(logArgs, source)
		logWhere += fmt.Sprintf(` AND l.source = $%d`, len(logArgs))
	}
	logStmt := fmt.Sprintf(`DELETE FROM prompt_filter_logs WHERE id IN (
		SELECT l.id FROM prompt_filter_logs l WHERE %s LIMIT $%d)`, logWhere, len(logArgs)+1)
	logArgs = append(logArgs, batchSize)

	deleted, err := db.purgeInBatches(ctx, logStmt, logArgs, batchSize, pause, &result)
	result.Logs = deleted
	if err != nil {
		return result, err
	}

	if !includeEvents {
		// 手动清空日志只动 prompt_filter_logs：风险画像（事件/来源）按现有语义保留，
		// 只由保留策略按天龄清理。
		return result, nil
	}

	eventWhere := `e.created_at < $1 AND NOT ` + promptEventProtectedSQL
	eventArgs := []interface{}{cutoffArg}
	eventStmt := fmt.Sprintf(`DELETE FROM prompt_risk_events WHERE id IN (
		SELECT e.id FROM prompt_risk_events e WHERE %s LIMIT $%d)`, eventWhere, len(eventArgs)+1)
	eventArgs = append(eventArgs, batchSize)
	deleted, err = db.purgeInBatches(ctx, eventStmt, eventArgs, batchSize, pause, &result)
	result.Events = deleted
	if err != nil {
		return result, err
	}

	deleted, err = db.purgeOrphanPromptRiskSources(ctx, cutoffArg, batchSize, pause, &result)
	result.Sources = deleted
	return result, err
}

// purgeOrphanPromptRiskSources 清理过期且已无任何风险事件引用的来源记录。
func (db *DB) purgeOrphanPromptRiskSources(ctx context.Context, cutoffArg interface{}, batchSize int, pause time.Duration, result *PromptLogPurgeResult) (int64, error) {
	stmt := `DELETE FROM prompt_risk_event_sources WHERE (source_type, source_id) IN (
		SELECT s.source_type, s.source_id FROM prompt_risk_event_sources s
		WHERE s.processed_at < $1 AND NOT EXISTS (
			SELECT 1 FROM prompt_risk_events e WHERE e.source_type = s.source_type AND e.source_id = s.source_id)
		LIMIT $2)`
	return db.purgeInBatches(ctx, stmt, []interface{}{cutoffArg, batchSize}, batchSize, pause, result)
}

// purgeInBatches 反复执行带 LIMIT 的删除语句直到一批不满或没有可删行；
// 每批持有一次 SQLite 写锁，批间 pause 让出给正常请求。
func (db *DB) purgeInBatches(ctx context.Context, stmt string, args []interface{}, batchSize int, pause time.Duration, result *PromptLogPurgeResult) (int64, error) {
	var total int64
	for {
		if ctx.Err() != nil {
			result.Interrupted = true
			return total, nil
		}
		var affected int64
		err := db.withSQLiteWriteLock(ctx, func() error {
			res, execErr := db.conn.ExecContext(ctx, stmt, args...)
			if execErr != nil {
				return execErr
			}
			affected, _ = res.RowsAffected()
			return nil
		})
		if err != nil {
			if ctx.Err() != nil {
				result.Interrupted = true
				return total, nil
			}
			return total, err
		}
		result.Batches++
		total += affected
		if affected < int64(batchSize) {
			return total, nil
		}
		if pause > 0 {
			select {
			case <-ctx.Done():
				result.Interrupted = true
				return total, nil
			case <-time.After(pause):
			}
		}
	}
}

// ==================== CY 级联 ====================

// deletePromptIncidentEvidenceTx 在删除 CY 记录前清掉与之关联的审核日志
// （共享 request_correlation_id，且没有其他 CY 还引用同一 correlation id）。
// 风险画像（prompt_risk_events / 来源记录）不在级联范围内：它们是账号 / 用户维度的
// 历史，按既有语义在删除 CY 后仍保留，失去 CY 和日志后由保留策略按天龄清理。
// 调用方必须在同一事务里随后删除 CY 本身。
func deletePromptIncidentEvidenceTx(ctx context.Context, tx *sql.Tx, incidentID string) error {
	var correlationID string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(request_correlation_id, '') FROM prompt_policy_incidents WHERE incident_id=$1`, incidentID).Scan(&correlationID); err != nil {
		return err
	}
	if correlationID == "" {
		return nil
	}
	var others int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM prompt_policy_incidents WHERE request_correlation_id=$1 AND incident_id<>$2`, correlationID, incidentID).Scan(&others); err != nil {
		return err
	}
	if others > 0 {
		return nil
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM prompt_filter_logs WHERE request_correlation_id=$1`, correlationID)
	return err
}

// deleteAllPromptIncidentEvidenceTx 是「清空 CY」的级联：删除所有与任一 CY 共享
// correlation id 的审核日志；风险画像同样保留。
func deleteAllPromptIncidentEvidenceTx(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `DELETE FROM prompt_filter_logs WHERE request_correlation_id <> '' AND EXISTS (
		SELECT 1 FROM prompt_policy_incidents i WHERE i.request_correlation_id = prompt_filter_logs.request_correlation_id)`)
	return err
}
