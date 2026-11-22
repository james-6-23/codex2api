package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

func TestGetAccountLiveStateReturnsVisibleInflightCounts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := auth.NewStore(nil, nil, nil)
	account := &auth.Account{DBID: 42, AccessToken: "token"}
	account.ActiveRequests.Store(3)
	account.OccupiedRequests.Store(5)
	store.AddAccount(account)
	store.SetSessionSlotBufferEnabled(true)
	// AddAccount recomputes scheduler state; pin the maintained caps afterwards
	// so the endpoint is checked against a degraded (warm-style) limit.
	account.BaseConcurrencyEffective = 50
	account.DynamicConcurrencyLimit = 25
	handler := &Handler{store: store}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/admin/accounts/live?ids=42,99", nil)
	handler.GetAccountLiveState(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Accounts                 map[string]accountLiveItem `json:"accounts"`
		SessionSlotBufferEnabled bool                       `json:"session_slot_buffer_enabled"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if got := response.Accounts["42"].ActiveRequests; got != 3 {
		t.Fatalf("active_requests = %d, want 3", got)
	}
	if got := response.Accounts["42"].OccupiedRequests; got != 5 {
		t.Fatalf("occupied_requests = %d, want 5", got)
	}
	if got := response.Accounts["42"].DynamicConcurrencyLimit; got != 25 {
		t.Fatalf("dynamic_concurrency_limit = %d, want 25", got)
	}
	if got := response.Accounts["42"].BaseConcurrencyEffective; got != 50 {
		t.Fatalf("base_concurrency_effective = %d, want 50", got)
	}
	if !response.SessionSlotBufferEnabled {
		t.Fatal("session slot buffer enabled state was not returned")
	}
	if _, exists := response.Accounts["99"]; exists {
		t.Fatal("missing account unexpectedly returned")
	}
}

func TestGetAccountLiveStateReturnsRuntimeUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := auth.NewStore(nil, nil, nil)
	now := time.Now()
	account := &auth.Account{
		DBID:        7,
		AccessToken: "token",
		PlanType:    "plus",
		Status:      auth.StatusReady,
		HealthTier:  auth.HealthTierHealthy,
	}
	account.UsagePercent5h = 42
	account.UsagePercent5hValid = true
	account.Reset5hAt = now.Add(time.Hour)
	account.UsageUpdatedAt5h = now
	account.UsagePercent7d = 12
	account.UsagePercent7dValid = true
	account.Reset7dAt = now.Add(24 * time.Hour)
	account.UsageUpdatedAt = now
	store.AddAccount(account)
	store.ObserveUsageFramePlan(account, "pro")
	balance := "12.5"
	account.ApplySparseCreditsHeaders(true, false, &balance, nil, "")
	handler := &Handler{store: store}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/admin/accounts/live?ids=7", nil)
	handler.GetAccountLiveState(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Accounts map[string]accountLiveItem `json:"accounts"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	item := response.Accounts["7"]
	if item.UsagePercent5h == nil || *item.UsagePercent5h != 42 {
		t.Fatalf("usage_percent_5h = %v, want 42", item.UsagePercent5h)
	}
	if item.UsagePercent7d == nil || *item.UsagePercent7d != 12 {
		t.Fatalf("usage_percent_7d = %v, want 12", item.UsagePercent7d)
	}
	if item.Reset5hAt == nil || item.Reset7dAt == nil {
		t.Fatalf("reset times missing: %+v", item)
	}
	if item.Status == nil || *item.Status == "" {
		t.Fatalf("status missing: %+v", item)
	}
	if item.UsingCredits == nil {
		t.Fatalf("using_credits missing: %+v", item)
	}
	var raw struct {
		Accounts map[string]map[string]json.RawMessage `json:"accounts"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{"plan_type": `"pro"`, "credits_valid": "true", "credits_balance": `"12.5"`, "credits_has_credits": "true", "credits_unlimited": "false", "credits_spend_control_reached": "null"} {
		if got := string(raw.Accounts["7"][key]); got != want {
			t.Errorf("live %s=%s, want %s", key, got, want)
		}
	}
}

func TestGetAccountLiveStateSerializesNullWindowsAndSkipsAntigravityFields(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := auth.NewStore(nil, nil, nil)
	codex := &auth.Account{
		DBID:        11,
		AccessToken: "token",
		Status:      auth.StatusReady,
		HealthTier:  auth.HealthTierHealthy,
	}
	antigravity := &auth.Account{
		DBID:         12,
		AccessToken:  "token",
		UpstreamType: auth.UpstreamAntigravity,
	}
	store.AddAccount(codex)
	store.AddAccount(antigravity)
	codex.MarkUsageFrameObserved("codex.rate_limits", time.Now())
	handler := &Handler{store: store}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/admin/accounts/live?ids=11,12", nil)
	handler.GetAccountLiveState(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	var raw struct {
		Accounts map[string]map[string]json.RawMessage `json:"accounts"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	codexItem := raw.Accounts["11"]
	if got := string(codexItem["usage_percent_5h"]); got != "null" {
		t.Fatalf("codex usage_percent_5h = %s, want null", got)
	}
	if got := string(codexItem["status"]); got == "" || got == "null" {
		t.Fatalf("codex status missing: %s", got)
	}
	if got := string(codexItem["codex_usage_frame_kind"]); got != `"codex.rate_limits"` {
		t.Fatalf("codex frame kind = %s, want codex.rate_limits", got)
	}
	if got := string(codexItem["codex_usage_frame_at"]); got == "" || got == "null" {
		t.Fatalf("codex frame time missing: %s", got)
	}
	antigravityItem := raw.Accounts["12"]
	for _, key := range []string{"status", "using_credits", "plan_type", "credits_valid", "credits_balance", "codex_usage_frame_at", "codex_usage_frame_kind"} {
		if value, ok := antigravityItem[key]; ok {
			t.Fatalf("antigravity item unexpectedly carries %s=%s", key, value)
		}
	}
	if got := string(antigravityItem["usage_percent_5h"]); got != "null" {
		t.Fatalf("antigravity usage_percent_5h = %s, want null", got)
	}
}
