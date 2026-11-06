package proxy

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// newChannelTestGin 构造带指定上游渠道 API Key 的 gin 上下文；channel 为空表示不限渠道。
func newChannelTestGin(channel string) *gin.Context {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	if channel != "" {
		c.Set(contextAPIKeyRow, &database.APIKeyRow{ID: 7, Limits: database.APIKeyLimits{UpstreamChannel: channel}})
	}
	return c
}

// TestWithRequestModelCooldownFilterInterceptsCrossInstanceAntigravity 覆盖 auto 渠道：
// Antigravity 渠道请求的冷却过滤器必须解析 publicModel 对应的 wireModel 与 Gemini
// familyKey，对「其它实例写入」的 wire / family 冷却一并拦截（跨 tier、跨实例）；
// 非 Antigravity 渠道仍只检查单一 publicModel，行为不变。
func TestWithRequestModelCooldownFilterInterceptsCrossInstanceAntigravity(t *testing.T) {
	tokenCache := cache.NewMemory(8)
	defer tokenCache.Close()

	// 写入实例：账号 9100 在 gemini-3.8-flash-high 上被限流，冷却落在共享 wire 模型上。
	writer := auth.NewStore(nil, tokenCache, &database.SystemSettings{MaxConcurrency: 4})
	defer writer.Stop()
	hotAccount := &auth.Account{
		DBID: 9100, UpstreamType: auth.UpstreamAntigravity, AccessToken: "google-token",
		AntigravityProjectID: "google-project", Status: auth.StatusReady,
		Models: []string{"gemini-3.8-flash-tiered"},
	}
	writer.AddAccount(hotAccount)
	writer.MarkModelCooldownUntil(hotAccount, "gemini-3.8-flash-tiered", "rate_limited_model", time.Now().Add(time.Minute))

	// 读取实例：与写入实例共享缓存，但账号在本地内存里没有任何冷却记录（模拟另一实例视角）。
	reader := auth.NewStore(nil, tokenCache, &database.SystemSettings{MaxConcurrency: 4})
	defer reader.Stop()
	handler := NewHandler(reader, nil, nil, nil)
	remote := &auth.Account{
		DBID: 9100, UpstreamType: auth.UpstreamAntigravity, AccessToken: "google-token",
		AntigravityProjectID: "google-project", Status: auth.StatusReady,
	}
	if remote.IsModelRateLimited("gemini-3.8-flash-tiered") {
		t.Fatal("the reading-side account must start with no local cooldown")
	}

	// Antigravity 渠道 + publicModel=gemini-3.8-flash-low（wire=gemini-3.8-flash-tiered）：
	// 三键过滤器必须命中另一实例写入的 wire 冷却，把该账号过滤掉。
	antigravityCtx := newChannelTestGin(database.UpstreamChannelAntigravity)
	filter := handler.withRequestModelCooldownFilter(antigravityCtx, context.Background(), "gemini-3.8-flash-low", nil)
	if filter(remote) {
		t.Fatal("the cross-instance wire-model cooldown must filter the Antigravity candidate")
	}
	if !remote.IsModelRateLimited("gemini-3.8-flash-tiered") {
		t.Fatal("the remote wire cooldown was not synchronized into the local account")
	}
}

// TestWithRequestModelCooldownFilterInterceptsGeminiFamilyCooldown 覆盖 familyKey：
// 另一实例写入的 Gemini 家族共享冷却（antigravity:gemini）必须拦截任意 Gemini publicModel
// 的 Antigravity 候选，即使该 publicModel 与其 wire 都没有单独冷却。
func TestWithRequestModelCooldownFilterInterceptsGeminiFamilyCooldown(t *testing.T) {
	tokenCache := cache.NewMemory(8)
	defer tokenCache.Close()

	writer := auth.NewStore(nil, tokenCache, &database.SystemSettings{MaxConcurrency: 4})
	defer writer.Stop()
	hotAccount := &auth.Account{
		DBID: 9101, UpstreamType: auth.UpstreamAntigravity, AccessToken: "google-token",
		AntigravityProjectID: "google-project", Status: auth.StatusReady,
	}
	writer.AddAccount(hotAccount)
	writer.MarkModelCooldownUntil(hotAccount, antigravityGeminiQuotaCooldownKey("gemini-3.8-flash-low", ""), "rate_limited_model", time.Now().Add(time.Minute))

	reader := auth.NewStore(nil, tokenCache, &database.SystemSettings{MaxConcurrency: 4})
	defer reader.Stop()
	handler := NewHandler(reader, nil, nil, nil)
	remote := &auth.Account{
		DBID: 9101, UpstreamType: auth.UpstreamAntigravity, AccessToken: "google-token",
		AntigravityProjectID: "google-project", Status: auth.StatusReady,
	}

	// publicModel 与 wire 都没有冷却，只有家族键有：Antigravity 过滤器仍应拦截。
	antigravityCtx := newChannelTestGin(database.UpstreamChannelAntigravity)
	filter := handler.withRequestModelCooldownFilter(antigravityCtx, context.Background(), "gemini-3.8-flash-medium", nil)
	if filter(remote) {
		t.Fatal("the cross-instance Gemini family cooldown must filter the Antigravity candidate")
	}
}

// TestWithRequestModelCooldownFilterNonAntigravityKeepsSingleModelCheck 证明渠道感知：
// 非 Antigravity 渠道（无渠道限定）沿用单一 publicModel 检查，看不到 wire 冷却，账号放行。
func TestWithRequestModelCooldownFilterNonAntigravityKeepsSingleModelCheck(t *testing.T) {
	tokenCache := cache.NewMemory(8)
	defer tokenCache.Close()

	writer := auth.NewStore(nil, tokenCache, &database.SystemSettings{MaxConcurrency: 4})
	defer writer.Stop()
	hotAccount := &auth.Account{
		DBID: 9102, AccessToken: "openai-token", Status: auth.StatusReady,
	}
	writer.AddAccount(hotAccount)
	writer.MarkModelCooldownUntil(hotAccount, "gemini-3.8-flash-tiered", "rate_limited_model", time.Now().Add(time.Minute))

	reader := auth.NewStore(nil, tokenCache, &database.SystemSettings{MaxConcurrency: 4})
	defer reader.Stop()
	handler := NewHandler(reader, nil, nil, nil)
	remote := &auth.Account{DBID: 9102, AccessToken: "openai-token", Status: auth.StatusReady}

	// 无渠道限定：只检查 publicModel=gemini-3.8-flash-low，wire 冷却不可见 → 放行。
	plainCtx := newChannelTestGin("")
	filter := handler.withRequestModelCooldownFilter(plainCtx, context.Background(), "gemini-3.8-flash-low", nil)
	if !filter(remote) {
		t.Fatal("a non-Antigravity channel must not see the wire-model cooldown and must pass the candidate")
	}
}
