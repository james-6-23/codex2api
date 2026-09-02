package admin

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

// officialPricingSyncSlot ensures a manual sync and the background poller never
// fetch/write pricing at the same time. Waiting is context-aware, so shutdown and
// cancelled HTTP requests do not get stuck behind a slow official site.
var officialPricingSyncSlot = func() chan struct{} {
	ch := make(chan struct{}, 1)
	ch <- struct{}{}
	return ch
}()

type officialPricingSyncConfigResponse struct {
	Enabled         bool    `json:"enabled"`
	IntervalMinutes int     `json:"interval_minutes"`
	IncludeOpenAI   bool    `json:"include_openai"`
	IncludeGrok     bool    `json:"include_grok"`
	IncludeClaude   bool    `json:"include_claude"`
	LastAttemptAt   *string `json:"last_attempt_at,omitempty"`
	LastSuccessAt   *string `json:"last_success_at,omitempty"`
	LastError       string  `json:"last_error,omitempty"`
	LastWarning     string  `json:"last_warning,omitempty"`
}

func officialPricingConfigResponse(cfg *database.OfficialPricingSyncConfig) officialPricingSyncConfigResponse {
	response := officialPricingSyncConfigResponse{
		IntervalMinutes: database.DefaultOfficialPricingSyncIntervalMinutes,
		IncludeOpenAI:   true,
		IncludeGrok:     true,
		IncludeClaude:   true,
	}
	if cfg == nil {
		return response
	}
	response.Enabled = cfg.Enabled
	response.IntervalMinutes = cfg.IntervalMinutes
	response.IncludeOpenAI = cfg.IncludeOpenAI
	response.IncludeGrok = cfg.IncludeGrok
	response.IncludeClaude = cfg.IncludeClaude
	response.LastError = cfg.LastError
	response.LastWarning = cfg.LastWarning
	if cfg.LastAttemptAt.Valid {
		value := cfg.LastAttemptAt.Time.UTC().Format(time.RFC3339)
		response.LastAttemptAt = &value
	}
	if cfg.LastSuccessAt.Valid {
		value := cfg.LastSuccessAt.Time.UTC().Format(time.RFC3339)
		response.LastSuccessAt = &value
	}
	return response
}

func (h *Handler) officialPricingModelIDs(ctx context.Context) []string {
	models := proxy.SupportedModelIDs(ctx, h.db)
	models = append(models, h.grokBillingModelIDs()...)
	models = append(models, h.claudeChannelModels()...)
	return models
}

func (h *Handler) runOfficialPricingSync(ctx context.Context, includeOpenAI, includeGrok, includeClaude bool) (*proxy.OfficialPricingSyncResult, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-officialPricingSyncSlot:
	}
	defer func() { officialPricingSyncSlot <- struct{}{} }()

	proxyURL := ""
	if h.store != nil {
		proxyURL = h.store.GetProxyURL()
	}
	attemptedAt := time.Now().UTC()
	result, syncErr := proxy.SyncOfficialModelPricing(ctx, h.db, proxyURL, proxy.OfficialPricingSyncOptions{
		Models:        h.officialPricingModelIDs(ctx),
		IncludeOpenAI: includeOpenAI,
		IncludeGrok:   includeGrok,
		IncludeClaude: includeClaude,
	})
	recordCtx, recordCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer recordCancel()
	warnings := []string(nil)
	if result != nil {
		warnings = result.Warnings
	}
	if recordErr := h.db.RecordOfficialPricingSyncResult(recordCtx, attemptedAt, syncErr, warnings); recordErr != nil {
		if syncErr != nil {
			return result, fmt.Errorf("%v；记录同步状态失败: %w", syncErr, recordErr)
		}
		return result, fmt.Errorf("官方价格已同步，但记录状态失败: %w", recordErr)
	}
	return result, syncErr
}

type updateOfficialPricingSyncConfigRequest struct {
	Enabled         bool `json:"enabled"`
	IntervalMinutes int  `json:"interval_minutes"`
	IncludeOpenAI   bool `json:"include_openai"`
	IncludeGrok     bool `json:"include_grok"`
	IncludeClaude   bool `json:"include_claude"`
}

func (h *Handler) UpdateOfficialPricingSyncConfig(c *gin.Context) {
	var req updateOfficialPricingSyncConfigRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.IntervalMinutes < database.MinOfficialPricingSyncIntervalMinutes || req.IntervalMinutes > database.MaxOfficialPricingSyncIntervalMinutes {
		writeError(c, http.StatusBadRequest, fmt.Sprintf("轮询间隔必须在 %d 到 %d 分钟之间", database.MinOfficialPricingSyncIntervalMinutes, database.MaxOfficialPricingSyncIntervalMinutes))
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	cfg, err := h.db.UpdateOfficialPricingSyncConfig(ctx, database.OfficialPricingSyncConfig{
		Enabled:         req.Enabled,
		IntervalMinutes: req.IntervalMinutes,
		IncludeOpenAI:   req.IncludeOpenAI,
		IncludeGrok:     req.IncludeGrok,
		IncludeClaude:   req.IncludeClaude,
	})
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	c.JSON(http.StatusOK, officialPricingConfigResponse(cfg))
}

type syncOfficialPricingRequest struct {
	IncludeOpenAI *bool `json:"include_openai"`
	IncludeGrok   *bool `json:"include_grok"`
	IncludeClaude *bool `json:"include_claude"`
}

func (h *Handler) SyncOfficialPricingNow(c *gin.Context) {
	var req syncOfficialPricingRequest
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&req); err != nil {
			writeError(c, http.StatusBadRequest, "invalid request body")
			return
		}
	}
	includeOpenAI, includeGrok, includeClaude := true, true, true
	if req.IncludeOpenAI != nil {
		includeOpenAI = *req.IncludeOpenAI
	}
	if req.IncludeGrok != nil {
		includeGrok = *req.IncludeGrok
	}
	if req.IncludeClaude != nil {
		includeClaude = *req.IncludeClaude
	}
	if !includeOpenAI && !includeGrok && !includeClaude {
		writeError(c, http.StatusBadRequest, "至少选择一个官方价格来源")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 90*time.Second)
	defer cancel()
	result, err := h.runOfficialPricingSync(ctx, includeOpenAI, includeGrok, includeClaude)
	if err != nil {
		writeError(c, http.StatusBadGateway, err.Error())
		return
	}
	c.JSON(http.StatusOK, result)
}

// StartOfficialPricingSync starts the optional official price poller. It is off
// by default and performs no network access until the administrator enables it.
func (h *Handler) StartOfficialPricingSync(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()

		check := func() {
			readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			cfg, err := h.db.GetOfficialPricingSyncConfig(readCtx)
			cancel()
			if err != nil {
				log.Printf("读取官方价格轮询设置失败: %v", err)
				return
			}
			if cfg == nil || !cfg.Enabled || (!cfg.IncludeOpenAI && !cfg.IncludeGrok && !cfg.IncludeClaude) {
				return
			}
			lastRun := time.Time{}
			if cfg.LastAttemptAt.Valid {
				lastRun = cfg.LastAttemptAt.Time
			}
			if !lastRun.IsZero() && time.Since(lastRun) < time.Duration(cfg.IntervalMinutes)*time.Minute {
				return
			}

			syncCtx, syncCancel := context.WithTimeout(ctx, 90*time.Second)
			result, syncErr := h.runOfficialPricingSync(syncCtx, cfg.IncludeOpenAI, cfg.IncludeGrok, cfg.IncludeClaude)
			syncCancel()
			if syncErr != nil {
				log.Printf("官方模型价格自动同步失败: %v", syncErr)
				return
			}
			if result == nil {
				log.Printf("官方模型价格自动同步返回空结果")
				return
			}
			warning := ""
			if result != nil && len(result.Warnings) > 0 {
				warning = "，警告: " + strings.Join(result.Warnings, "; ")
			}
			log.Printf("官方模型价格自动同步完成: fetched=%d applied=%d skipped=%d%s", result.Fetched, result.Applied, result.Skipped, warning)
		}

		check()
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
