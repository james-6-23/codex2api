package admin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

// updateClaudeAccountReq 是 Claude Code 账号的可编辑配置。
// 凭据由 OAuth 流程产出，管理台不接受手工改 token，因此这里只有路由侧参数。
type updateClaudeAccountReq struct {
	Name         string   `json:"name"`
	BaseURL      string   `json:"base_url"`
	Models       []string `json:"models"`
	ModelMapping string   `json:"model_mapping"`
	ProxyURL     string   `json:"proxy_url"`
}

// UpdateClaudeAccount 更新 Claude 账号的可编辑配置（PATCH /api/admin/accounts/:id/claude）。
// 语义与 Grok 一致：base_url / models / model_mapping / proxy 是整体重写（留空即清空）。
func (h *Handler) UpdateClaudeAccount(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}
	var req updateClaudeAccountReq
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	req.Name = security.SanitizeInput(req.Name)
	req.ProxyURL = security.SanitizeInput(req.ProxyURL)

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	row, err := h.db.GetAccountByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(c, http.StatusNotFound, "账号不存在")
			return
		}
		writeInternalError(c, err)
		return
	}
	if !strings.EqualFold(strings.TrimSpace(row.GetCredential("upstream_type")), auth.UpstreamClaude) {
		writeError(c, http.StatusBadRequest, "仅 Claude 账号支持该设置")
		return
	}

	baseURL, err := auth.NormalizeClaudeBaseURL(req.BaseURL)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := security.ValidateProxyURL(req.ProxyURL); err != nil {
		writeError(c, http.StatusBadRequest, "代理URL无效")
		return
	}
	models := auth.NormalizeAccountModels(req.Models)
	for _, model := range models {
		if err := security.ValidateModelName(model); err != nil {
			writeError(c, http.StatusBadRequest, fmt.Sprintf("模型名称无效: %s", model))
			return
		}
	}
	modelMapping, err := normalizeAccountModelMapping(req.ModelMapping)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	credentials := map[string]interface{}{
		"upstream_type": auth.UpstreamClaude,
		"base_url":      baseURL,
		"models":        models,
		"model_mapping": modelMapping,
	}
	if err := h.db.UpdateCredentials(ctx, id, credentials); err != nil {
		writeInternalError(c, err)
		return
	}
	// proxy_url 是独立列，UpdateCredentials 不会写它；不单独持久化的话代理只落到内存
	// store，重载/重启后被 DB 旧值覆盖（与 Grok 侧同一个坑）。
	if err := h.db.UpdateAccountProxyURL(ctx, id, req.ProxyURL); err != nil {
		writeInternalError(c, err)
		return
	}
	if req.Name != "" {
		_ = h.db.UpdateAccountName(ctx, id, req.Name)
	}
	if h.store != nil {
		h.store.ApplyClaudeConfig(id, baseURL, models, modelMapping, req.ProxyURL)
	}
	h.db.InsertAccountEventAsync(id, "updated", "manual_claude")
	writeMessage(c, http.StatusOK, "Claude 账号设置已更新")
}
