package admin

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"

	"github.com/gin-gonic/gin"
)

// ApplyKiroSettings 让 Kiro 设置在本进程生效（启动加载与保存后调用）。
func ApplyKiroSettings(settings database.KiroSettings) {
	settings = settings.Normalized()
	auth.SetKiroAPIDomain(settings.APIDomain)
	proxy.SetKiroCacheSimulation(settings.CacheSimulation)
}

// ensureKiroBearer 在 AT 缺失或 5 分钟内过期时先刷新（同 kiro.rs 请求前的 ensure_token），
// 避免只导入了 RT 的账号测连必然 401。非 Kiro 账号直接返回。
func (h *Handler) ensureKiroBearer(ctx context.Context, account *auth.Account) error {
	if h == nil || h.store == nil || !account.IsKiro() || !account.KiroRefreshable() {
		return nil
	}
	current := account.KiroRequestAuth()
	if current.BearerToken != "" && (current.ExpiresAt.IsZero() || time.Until(current.ExpiresAt) > 5*time.Minute) {
		return nil
	}
	return h.store.RefreshKiroAccount(ctx, account)
}

// kiroConnectionTestModel 选测试模型：显式指定的必须在账号目录里；否则渠道配置优先，
// 再挑倍率最低的 haiku，最后取第一个未冷却的模型。
func kiroConnectionTestModel(account *auth.Account, requested, configured string) (string, error) {
	models := proxy.KiroAccountModelIDs(account)
	if requested != "" {
		for _, model := range models {
			if strings.EqualFold(model, requested) {
				return model, nil
			}
		}
		return "", fmt.Errorf("该 Kiro 账号不支持测试模型: %s", requested)
	}
	pick := func(match func(string) bool) string {
		for _, model := range models {
			if match(model) && !account.IsModelRateLimited(model) {
				return model
			}
		}
		return ""
	}
	for _, match := range []func(string) bool{
		func(model string) bool { return configured != "" && strings.EqualFold(model, configured) },
		func(model string) bool { return strings.Contains(strings.ToLower(model), "haiku") },
		func(string) bool { return true },
	} {
		if model := pick(match); model != "" {
			return model, nil
		}
	}
	return "", fmt.Errorf("该 Kiro 账号的模型均处于模型级冷却")
}

// GetKiroSettings 返回 Kiro 渠道设置。
// GET /api/admin/settings/kiro
func (h *Handler) GetKiroSettings(c *gin.Context) {
	if h.db == nil {
		c.JSON(http.StatusOK, database.KiroSettings{}.Normalized())
		return
	}
	settings, err := h.db.LoadKiroSettings(c.Request.Context())
	if err != nil {
		writeError(c, http.StatusInternalServerError, "读取 Kiro 设置失败: "+err.Error())
		return
	}
	c.JSON(http.StatusOK, settings)
}

// UpdateKiroSettings 只更新请求里出现的字段。
// PUT /api/admin/settings/kiro  {"api_domain":"kiro","cache_simulation":true}
func (h *Handler) UpdateKiroSettings(c *gin.Context) {
	var req struct {
		APIDomain       *string `json:"api_domain"`
		CacheSimulation *bool   `json:"cache_simulation"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	if req.APIDomain == nil && req.CacheSimulation == nil {
		writeError(c, http.StatusBadRequest, "缺少 api_domain 或 cache_simulation 字段")
		return
	}
	if h.db == nil {
		writeError(c, http.StatusServiceUnavailable, "数据库不可用")
		return
	}
	settings, err := h.db.LoadKiroSettings(c.Request.Context())
	if err != nil {
		writeError(c, http.StatusInternalServerError, "读取 Kiro 设置失败: "+err.Error())
		return
	}
	if req.APIDomain != nil {
		domain := strings.ToLower(strings.TrimSpace(*req.APIDomain))
		if domain != "amazonaws" && domain != "kiro" {
			writeError(c, http.StatusBadRequest, "api_domain 只能是 amazonaws 或 kiro")
			return
		}
		settings.APIDomain = domain
	}
	if req.CacheSimulation != nil {
		settings.CacheSimulation = *req.CacheSimulation
	}
	if err := h.db.SaveKiroSettings(c.Request.Context(), settings); err != nil {
		writeError(c, http.StatusInternalServerError, "保存失败: "+err.Error())
		return
	}
	ApplyKiroSettings(settings)
	c.JSON(http.StatusOK, settings.Normalized())
}
