package admin

import (
	"context"
	"log"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// Prompt 审核日志保留：定时按天数分批清理过期日志（跳过 CY 关联行），
// 手动「清空日志」也走同一条分批通道，不再受请求超时限制。

const (
	promptLogRetentionCheckInterval = time.Hour
	promptLogRetentionRunTimeout    = 30 * time.Minute
	promptLogPurgeBatchSize         = database.DefaultPromptLogPurgeBatch
	promptLogPurgeBatchPause        = 200 * time.Millisecond
)

// promptLogPurgeRunning 保证定时清理与手动清理不会同时跑（两者都是长时间分批删除）。
var promptLogPurgeRunning int32

type promptLogRetentionResponse struct {
	RetentionDays      int     `json:"retention_days"`
	Running            bool    `json:"running"`
	LastRunAt          *string `json:"last_run_at,omitempty"`
	LastDeletedLogs    int64   `json:"last_deleted_logs"`
	LastDeletedEvents  int64   `json:"last_deleted_events"`
	LastDeletedSources int64   `json:"last_deleted_sources"`
	LastDurationMs     int64   `json:"last_duration_ms"`
	LastError          string  `json:"last_error,omitempty"`
}

func promptLogRetentionResponseFrom(cfg *database.PromptLogRetentionConfig) promptLogRetentionResponse {
	resp := promptLogRetentionResponse{RetentionDays: database.DefaultPromptLogRetentionDays, Running: atomic.LoadInt32(&promptLogPurgeRunning) == 1}
	if cfg == nil {
		return resp
	}
	resp.RetentionDays = cfg.RetentionDays
	resp.LastDeletedLogs = cfg.LastDeletedLogs
	resp.LastDeletedEvents = cfg.LastDeletedEvents
	resp.LastDeletedSources = cfg.LastDeletedSources
	resp.LastDurationMs = cfg.LastDurationMs
	resp.LastError = cfg.LastError
	if cfg.LastRunAt.Valid {
		value := cfg.LastRunAt.Time.UTC().Format(time.RFC3339)
		resp.LastRunAt = &value
	}
	return resp
}

// GetPromptLogRetention 返回保留天数与上次清理统计（GET /api/admin/prompt-filter/retention）。
func (h *Handler) GetPromptLogRetention(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	cfg, err := h.db.GetPromptLogRetentionConfig(ctx)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, promptLogRetentionResponseFrom(cfg))
}

type updatePromptLogRetentionRequest struct {
	RetentionDays int `json:"retention_days"`
}

// UpdatePromptLogRetention 设置保留天数（0 = 关闭自动清理，最大 365）。
func (h *Handler) UpdatePromptLogRetention(c *gin.Context) {
	var req updatePromptLogRetentionRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.RetentionDays < 0 || req.RetentionDays > database.MaxPromptLogRetentionDays {
		writeError(c, http.StatusBadRequest, "保留天数必须在 0 到 365 之间（0 表示关闭自动清理）")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	cfg, err := h.db.UpdatePromptLogRetentionDays(ctx, req.RetentionDays)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, promptLogRetentionResponseFrom(cfg))
}

// RunPromptLogRetentionNow 立即在后台按当前保留天数清理一次；已有清理在跑时返回 409。
func (h *Handler) RunPromptLogRetentionNow(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	cfg, err := h.db.GetPromptLogRetentionConfig(ctx)
	cancel()
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if cfg.RetentionDays <= 0 {
		writeError(c, http.StatusBadRequest, "保留天数为 0（已关闭自动清理），请先设置保留天数")
		return
	}
	if !h.startPromptLogPurge(func(ctx context.Context) {
		h.runPromptLogRetention(ctx, cfg.RetentionDays)
	}) {
		writeError(c, http.StatusConflict, "已有清理任务在运行")
		return
	}
	c.JSON(http.StatusOK, gin.H{"started": true, "retention_days": cfg.RetentionDays})
}

// startPromptLogPurge 在后台启动一次分批清理；占用中返回 false。
func (h *Handler) startPromptLogPurge(run func(ctx context.Context)) bool {
	if !atomic.CompareAndSwapInt32(&promptLogPurgeRunning, 0, 1) {
		return false
	}
	go func() {
		defer atomic.StoreInt32(&promptLogPurgeRunning, 0)
		ctx, cancel := context.WithTimeout(context.Background(), promptLogRetentionRunTimeout)
		defer cancel()
		run(ctx)
	}()
	return true
}

// runPromptLogRetention 执行一次保留清理并记录结果。
func (h *Handler) runPromptLogRetention(ctx context.Context, days int) {
	started := time.Now()
	cutoff := started.Add(-time.Duration(days) * 24 * time.Hour)
	result, err := h.db.PurgeExpiredPromptLogs(ctx, cutoff, promptLogPurgeBatchSize, promptLogPurgeBatchPause)
	duration := time.Since(started)
	recordCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if recordErr := h.db.RecordPromptLogRetentionRun(recordCtx, started, result, duration, err); recordErr != nil {
		log.Printf("[prompt-retention] 记录清理结果失败: %v", recordErr)
	}
	if err != nil {
		log.Printf("[prompt-retention] 清理失败（保留 %d 天）: %v；已删 logs=%d events=%d sources=%d", days, err, result.Logs, result.Events, result.Sources)
		return
	}
	if result.Logs+result.Events+result.Sources > 0 || result.Interrupted {
		log.Printf("[prompt-retention] 清理完成（保留 %d 天，%s）: logs=%d events=%d sources=%d batches=%d interrupted=%v",
			days, duration.Round(time.Millisecond), result.Logs, result.Events, result.Sources, result.Batches, result.Interrupted)
	}
}

// StartPromptLogRetention 启动每小时一次的保留清理；保留天数为 0 时不做任何事。
func (h *Handler) StartPromptLogRetention(ctx context.Context) {
	if h == nil || h.db == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(promptLogRetentionCheckInterval)
		defer ticker.Stop()
		check := func() {
			readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			cfg, err := h.db.GetPromptLogRetentionConfig(readCtx)
			cancel()
			if err != nil {
				log.Printf("[prompt-retention] 读取保留设置失败: %v", err)
				return
			}
			if cfg == nil || cfg.RetentionDays <= 0 {
				return
			}
			if cfg.LastRunAt.Valid && time.Since(cfg.LastRunAt.Time) < promptLogRetentionCheckInterval/2 {
				return
			}
			days := cfg.RetentionDays
			h.startPromptLogPurge(func(runCtx context.Context) {
				merged, cancelMerged := context.WithCancel(runCtx)
				defer cancelMerged()
				go func() {
					select {
					case <-ctx.Done():
						cancelMerged()
					case <-merged.Done():
					}
				}()
				h.runPromptLogRetention(merged, days)
			})
		}
		// 启动后稍等再跑第一轮，避免与其他启动任务抢写锁。
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Minute):
			check()
		}
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				check()
			}
		}
	}()
}
