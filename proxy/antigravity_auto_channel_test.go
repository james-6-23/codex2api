package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// auto 渠道：auto 渠道 Key（requestUpstreamChannel 为空）也可能选中 Antigravity 账号
// （native Gemini 入口把候选池硬限定为 Antigravity OAuth，Responses / Chat / Messages
// 也允许），因此冷却过滤器必须对 Antigravity 候选做 public / wire / family 三键检查，
// 把另一实例写入的 wire 与 family 冷却一并拦下。
func TestWithRequestModelCooldownFilterAutoChannelInterceptsAntigravityCandidate(t *testing.T) {
	tokenCache := cache.NewMemory(8)
	defer tokenCache.Close()

	writer := auth.NewStore(nil, tokenCache, &database.SystemSettings{MaxConcurrency: 4})
	defer writer.Stop()
	hotAccount := &auth.Account{
		DBID: 9110, UpstreamType: auth.UpstreamAntigravity, AccessToken: "google-token",
		AntigravityProjectID: "google-project", Status: auth.StatusReady,
		Models: []string{"gemini-3.8-flash-tiered"},
	}
	writer.AddAccount(hotAccount)
	writer.MarkModelCooldownUntil(hotAccount, "gemini-3.8-flash-tiered", "rate_limited_model", time.Now().Add(time.Minute))

	reader := auth.NewStore(nil, tokenCache, &database.SystemSettings{MaxConcurrency: 4})
	defer reader.Stop()
	handler := NewHandler(reader, nil, nil, nil)
	remote := &auth.Account{
		DBID: 9110, UpstreamType: auth.UpstreamAntigravity, AccessToken: "google-token",
		AntigravityProjectID: "google-project", Status: auth.StatusReady,
	}
	if remote.IsModelRateLimited("gemini-3.8-flash-tiered") {
		t.Fatal("the reading-side account must start with no local cooldown")
	}

	// auto 渠道（未限定）+ Antigravity 候选：wire 冷却必须拦截并 hydrate 进本地内存。
	autoCtx := newChannelTestGin("")
	filter := handler.withRequestModelCooldownFilter(autoCtx, context.Background(), "gemini-3.8-flash-low", nil)
	if filter(remote) {
		t.Fatal("the auto-channel filter must apply the three-key check to an Antigravity candidate")
	}
	if !remote.IsModelRateLimited("gemini-3.8-flash-tiered") {
		t.Fatal("the cross-instance wire cooldown was not synchronized into the local account")
	}

	// family 冷却同样要拦住 auto 渠道下的 Antigravity 候选。
	familyWriter := auth.NewStore(nil, tokenCache, &database.SystemSettings{MaxConcurrency: 4})
	defer familyWriter.Stop()
	quotaAccount := &auth.Account{
		DBID: 9111, UpstreamType: auth.UpstreamAntigravity, AccessToken: "google-token",
		AntigravityProjectID: "google-project", Status: auth.StatusReady,
	}
	familyWriter.AddAccount(quotaAccount)
	familyWriter.MarkModelCooldownUntil(quotaAccount, antigravityGeminiQuotaCooldownKey("gemini-3.8-flash-low", ""), "quota_exhausted", time.Now().Add(time.Minute))
	remoteFamily := &auth.Account{
		DBID: 9111, UpstreamType: auth.UpstreamAntigravity, AccessToken: "google-token",
		AntigravityProjectID: "google-project", Status: auth.StatusReady,
	}
	if filter(remoteFamily) {
		t.Fatal("the auto-channel filter must apply the Gemini family cooldown to an Antigravity candidate")
	}
}

// auto 渠道：auto 渠道下的非 Antigravity 候选必须保持单键语义——看不到 wire / family 冷却，
// 但 publicModel 的跨实例冷却仍然照常拦截。
func TestWithRequestModelCooldownFilterAutoChannelKeepsSingleKeyForOtherAccounts(t *testing.T) {
	tokenCache := cache.NewMemory(8)
	defer tokenCache.Close()

	writer := auth.NewStore(nil, tokenCache, &database.SystemSettings{MaxConcurrency: 4})
	defer writer.Stop()
	wireHot := &auth.Account{DBID: 9112, AccessToken: "openai-token", Status: auth.StatusReady}
	publicHot := &auth.Account{DBID: 9113, AccessToken: "openai-token", Status: auth.StatusReady}
	writer.AddAccount(wireHot)
	writer.AddAccount(publicHot)
	writer.MarkModelCooldownUntil(wireHot, "gemini-3.8-flash-tiered", "rate_limited_model", time.Now().Add(time.Minute))
	writer.MarkModelCooldownUntil(publicHot, "gemini-3.8-flash-low", "rate_limited_model", time.Now().Add(time.Minute))
	writer.MarkModelCooldownUntil(publicHot, antigravityGeminiQuotaCooldownKey("gemini-3.8-flash-low", ""), "quota_exhausted", time.Now().Add(time.Minute))

	reader := auth.NewStore(nil, tokenCache, &database.SystemSettings{MaxConcurrency: 4})
	defer reader.Stop()
	handler := NewHandler(reader, nil, nil, nil)
	autoCtx := newChannelTestGin("")
	filter := handler.withRequestModelCooldownFilter(autoCtx, context.Background(), "gemini-3.8-flash-low", nil)

	// 只有 wire 冷却的非 Antigravity 账号：单键语义下必须放行。
	if !filter(&auth.Account{DBID: 9112, AccessToken: "openai-token", Status: auth.StatusReady}) {
		t.Fatal("a non-Antigravity candidate must not be filtered by the wire-model cooldown")
	}
	// public 冷却仍然拦截（family 冷却与它同批写入，不能成为放行的理由）。
	if filter(&auth.Account{DBID: 9113, AccessToken: "openai-token", Status: auth.StatusReady}) {
		t.Fatal("a non-Antigravity candidate must still be filtered by its own public-model cooldown")
	}

	// 显式渠道（codex）保持单键：既看不到 wire，也看 family 之外的键。
	codexCtx := newChannelTestGin(database.UpstreamChannelCodex)
	codexFilter := handler.withRequestModelCooldownFilter(codexCtx, context.Background(), "gemini-3.8-flash-low", nil)
	if !codexFilter(&auth.Account{DBID: 9112, AccessToken: "openai-token", Status: auth.StatusReady}) {
		t.Fatal("an explicit non-Antigravity channel must keep the single public-model check")
	}
}

// auto 渠道：native Gemini 入口（generateContent / streamGenerateContent / countTokens）的
// 候选池被硬限定为 Antigravity OAuth，SelectionOptions.Provider 必须恒为 antigravity，
// 否则 auto 渠道 Key 下加权避险整体失效。
func TestGeminiNativeSelectionOptionsAlwaysPinAntigravityProvider(t *testing.T) {
	cases := []struct {
		name    string
		channel string
	}{
		{name: "auto key", channel: ""},
		{name: "antigravity key", channel: database.UpstreamChannelAntigravity},
		{name: "codex key", channel: database.UpstreamChannelCodex},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			opts := geminiNativeSelectionOptions(newChannelTestGin(tc.channel), "gemini-3.8-flash-high", auth.DispatchPolicyStandard)
			if opts.Provider != database.UpstreamChannelAntigravity {
				t.Fatalf("Provider = %q, want %q", opts.Provider, database.UpstreamChannelAntigravity)
			}
			if opts.Model != "gemini-3.8-flash-high" || opts.Policy != auth.DispatchPolicyStandard {
				t.Fatalf("SelectionOptions = %+v, want the model and policy preserved", opts)
			}
		})
	}

	// 对照：通用入口不覆盖渠道，auto Key 下 Provider 仍为空（加权避险由渠道判定关闭）。
	if opts := selectionOptionsForRequest(newChannelTestGin(""), "gemini-3.8-flash-high", auth.DispatchPolicyStandard); opts.Provider != "" {
		t.Fatalf("the generic entry must keep the request channel, got %q", opts.Provider)
	}
}

// auto 渠道端到端：auto 渠道 Key 打到 native countTokens 时，候选池里的 Antigravity 账号
// 必须参与加权避险——刚被限流（惩罚最高）的账号在号池空闲时一次都不该被选中。
func TestGeminiNativeCountTokensAutoKeyUsesWeightedSelection(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var mu sync.Mutex
	served := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 上游按凭据识别账号：每个账号的 access token 不同，直接映射回账号。
		bearer := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer"))
		label := "unknown"
		switch bearer {
		case "at-hot":
			label = "hot"
		case "at-healthy":
			label = "healthy"
		}
		mu.Lock()
		served[label]++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"totalTokens":3}`)
	}))
	defer server.Close()
	previous := antigravityOAuthEndpointBases
	antigravityOAuthEndpointBases = []string{server.URL}
	t.Cleanup(func() { antigravityOAuthEndpointBases = previous })

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 4, TestConcurrency: 1, TestModel: "gemini-3.8-flash-high"})
	defer store.Stop()
	store.SetSchedulerMode("round_robin")
	store.SetMaxRetries(0)
	store.SetMaxRateLimitRetries(0)
	newAccount := func(id int64, project, token string) *auth.Account {
		acc := &auth.Account{
			DBID: id, UpstreamType: auth.UpstreamAntigravity, AccessToken: token,
			AntigravityProjectID: project, Status: auth.StatusReady,
			Models: []string{"gemini-3.8-flash-tiered"},
		}
		store.AddAccount(acc)
		return acc
	}
	hot := newAccount(9120, "project-hot", "at-hot")
	newAccount(9121, "project-healthy", "at-healthy")

	auth.SetAntigravityBalancedAffinity(true)
	t.Cleanup(func() { auth.SetAntigravityBalancedAffinity(true) })
	auth.RecordAntigravityRateLimitEvent(hot, auth.AntigravityRateLimitEvent{
		StatusCode:  429,
		Kind:        "rate_limit",
		Scope:       auth.AGScopeModelRateLimit,
		PublicModel: "gemini-3.8-flash-high",
		WireModel:   "gemini-3.8-flash-tiered",
		Weight:      auth.AntigravityPenaltyWeightExplicitRateLimit,
		ObservedAt:  time.Now(),
	})
	if hot.AntigravityPenalty(time.Now()) <= 0 {
		t.Fatal("the penalized account lost its penalty before the run")
	}

	router := gin.New()
	handler := NewHandler(store, nil, nil, nil)
	router.Use(func(c *gin.Context) {
		// auto 渠道 Key：不限定上游渠道，requestUpstreamChannel 返回空串。
		c.Set(contextAPIKeyRow, &database.APIKeyRow{ID: 7, Limits: database.APIKeyLimits{UpstreamChannel: database.UpstreamChannelAuto}})
		c.Next()
	})
	router.POST("/v1beta/models/*action", handler.GeminiModelsAction)

	for i := 0; i < 12; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-3.8-flash-high:countTokens",
			strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"probe-`+string(rune('a'+i))+`"}]}]}`))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d body=%s", i, rec.Code, rec.Body.String())
		}
	}

	mu.Lock()
	defer mu.Unlock()
	if served["healthy"] == 0 {
		t.Fatalf("no request reached the healthy account: %v", served)
	}
	if served["hot"] != 0 {
		t.Fatalf("the penalized account still served %d of %d fresh sessions: %v", served["hot"], served["hot"]+served["healthy"], served)
	}
}
