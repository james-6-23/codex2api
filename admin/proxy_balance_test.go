package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestIsOAuthProxyBalanceTarget(t *testing.T) {
	tests := []struct {
		name string
		row  *database.AccountRow
		want bool
	}{
		{
			name: "codex oauth",
			row: &database.AccountRow{
				Type:        "oauth",
				Credentials: map[string]interface{}{"refresh_token": "rt-codex"},
			},
			want: true,
		},
		{
			name: "grok oauth",
			row: &database.AccountRow{
				Type: "grok",
				Credentials: map[string]interface{}{
					"upstream_type": auth.UpstreamGrok,
					"refresh_token": "rt-grok",
				},
			},
			want: true,
		},
		{
			name: "antigravity oauth",
			row: &database.AccountRow{
				Type: auth.UpstreamAntigravity,
				Credentials: map[string]interface{}{
					"upstream_type": auth.UpstreamAntigravity,
					"refresh_token": "rt-antigravity",
				},
			},
			want: true,
		},
		{
			name: "claude oauth",
			row: &database.AccountRow{
				Type: "anthropic",
				Credentials: map[string]interface{}{
					"upstream_type": auth.UpstreamClaude,
					"refresh_token": "rt-claude",
				},
			},
			want: true,
		},
		{
			name: "codex access token only",
			row: &database.AccountRow{
				Type:        "oauth",
				Credentials: map[string]interface{}{"access_token": "at-only"},
			},
		},
		{
			name: "openai responses api",
			row: &database.AccountRow{
				Type: "responses_api",
				Credentials: map[string]interface{}{
					"upstream_type": auth.UpstreamOpenAIResponses,
					"api_key":       "sk-relay",
				},
			},
		},
		{
			name: "grok api key",
			row: &database.AccountRow{
				Type: "grok",
				Credentials: map[string]interface{}{
					"upstream_type": auth.UpstreamGrok,
					"api_key":       "xai-key",
				},
			},
		},
		{
			name: "ambiguous api key wins over refresh token",
			row: &database.AccountRow{
				Type: "grok",
				Credentials: map[string]interface{}{
					"upstream_type": auth.UpstreamGrok,
					"refresh_token": "rt-grok",
					"api_key":       "xai-key",
				},
			},
		},
		{
			name: "unknown upstream",
			row: &database.AccountRow{
				Type: "oauth",
				Credentials: map[string]interface{}{
					"upstream_type": "future-relay",
					"refresh_token": "rt-relay",
				},
			},
		},
		{name: "nil row"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isOAuthProxyBalanceTarget(tt.row); got != tt.want {
				t.Fatalf("isOAuthProxyBalanceTarget() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestSelectOAuthProxyBalanceTargetsHonorsMode(t *testing.T) {
	rows := []*database.AccountRow{
		{
			ID:          1,
			Type:        "oauth",
			Credentials: map[string]interface{}{"refresh_token": "rt-bound"},
			ProxyURL:    "  http://bound  ",
		},
		{
			ID:          2,
			Type:        "oauth",
			Credentials: map[string]interface{}{"refresh_token": "rt-unbound"},
		},
		{
			ID:   3,
			Type: "responses_api",
			Credentials: map[string]interface{}{
				"upstream_type": auth.UpstreamOpenAIResponses,
				"api_key":       "sk-relay",
			},
		},
	}

	all := selectOAuthProxyBalanceTargets(rows, "all")
	if len(all) != 2 || all[0].id != 1 || all[0].current != "http://bound" || all[1].id != 2 {
		t.Fatalf("all targets = %+v, want OAuth accounts 1 and 2", all)
	}

	unbound := selectOAuthProxyBalanceTargets(rows, "unbound")
	if len(unbound) != 1 || unbound[0].id != 2 || unbound[0].current != "" {
		t.Fatalf("unbound targets = %+v, want only OAuth account 2", unbound)
	}
}

func TestAutoBalanceProxiesAssignsOnlyOAuthAccounts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newAdminProxyTestDB(t)
	ctx := context.Background()
	const (
		candidateProxy = "http://candidate.example:8080"
		manualProxy    = "http://manual.example:8080"
	)
	if _, err := db.InsertProxy(ctx, candidateProxy, "candidate"); err != nil {
		t.Fatalf("InsertProxy: %v", err)
	}

	codexOAuthID, err := db.InsertAccount(ctx, "codex-oauth", "rt-codex", "")
	if err != nil {
		t.Fatalf("InsertAccount codex OAuth: %v", err)
	}
	grokOAuthID, err := db.InsertAccountWithUpstream(ctx, "grok-oauth", "xai", auth.UpstreamGrok, map[string]interface{}{
		"upstream_type": auth.UpstreamGrok,
		"refresh_token": "rt-grok",
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithUpstream Grok OAuth: %v", err)
	}
	responsesAPIID, err := db.InsertOpenAIResponsesAccount(ctx, "responses-api", map[string]interface{}{
		"upstream_type": auth.UpstreamOpenAIResponses,
		"base_url":      "https://relay.example/v1",
		"api_key":       "sk-relay",
	}, manualProxy)
	if err != nil {
		t.Fatalf("InsertOpenAIResponsesAccount: %v", err)
	}
	grokAPIKeyID, err := db.InsertAccountWithUpstream(ctx, "grok-api-key", "xai", auth.UpstreamGrok, map[string]interface{}{
		"upstream_type": auth.UpstreamGrok,
		"api_key":       "xai-key",
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithUpstream Grok API key: %v", err)
	}
	atOnlyID, err := db.InsertATAccount(ctx, "codex-at-only", "at-codex", manualProxy)
	if err != nil {
		t.Fatalf("InsertATAccount: %v", err)
	}

	store := newAdminProxyTestStore(t, db)
	handler := &Handler{db: db, store: store}
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(
		http.MethodPost,
		"/api/admin/proxies/auto-balance",
		strings.NewReader(`{"mode":"all"}`),
	)
	ginCtx.Request.Header.Set("Content-Type", "application/json")

	handler.AutoBalanceProxies(ginCtx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var payload struct {
		Assigned int `json:"assigned"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.Assigned != 2 {
		t.Fatalf("assigned = %d, want 2 OAuth accounts", payload.Assigned)
	}

	assertProxyURL := func(id int64, want string) {
		t.Helper()
		row, err := db.GetAccountByID(ctx, id)
		if err != nil {
			t.Fatalf("GetAccountByID(%d): %v", id, err)
		}
		if row.ProxyURL != want {
			t.Fatalf("account %d proxy = %q, want %q", id, row.ProxyURL, want)
		}
	}
	assertProxyURL(codexOAuthID, candidateProxy)
	assertProxyURL(grokOAuthID, candidateProxy)
	assertProxyURL(responsesAPIID, manualProxy)
	assertProxyURL(grokAPIKeyID, "")
	assertProxyURL(atOnlyID, manualProxy)
}

// 未绑定账号按最少负载均匀摊开,并列时取 URL 字典序保证确定性。
func TestComputeProxyAssignmentsSpreadsEvenly(t *testing.T) {
	targets := []balanceAccount{{id: 1}, {id: 2}, {id: 3}, {id: 4}, {id: 5}, {id: 6}}
	candidates := []string{"http://p1", "http://p2", "http://p3"}

	result := computeProxyAssignments(targets, candidates, nil, 0)

	if len(result.assignments) != 6 || result.kept != 0 || result.skipped != 0 {
		t.Fatalf("assignments=%d kept=%d skipped=%d, want 6/0/0", len(result.assignments), result.kept, result.skipped)
	}
	for _, url := range candidates {
		if result.load[url] != 2 {
			t.Fatalf("load[%s] = %d, want 2 (result: %+v)", url, result.load[url], result.load)
		}
	}
	// 确定性:ID 升序 × URL 字典序 → 1→p1 2→p2 3→p3 4→p1 ...
	if result.assignments[1] != "http://p1" || result.assignments[2] != "http://p2" || result.assignments[4] != "http://p1" {
		t.Fatalf("assignment order not deterministic: %+v", result.assignments)
	}
}

// 基线负载(其它渠道的既有绑定)计入,新账号先填空闲代理。
func TestComputeProxyAssignmentsRespectsBaseline(t *testing.T) {
	targets := []balanceAccount{{id: 10}, {id: 11}}
	candidates := []string{"http://busy", "http://idle"}
	baseline := map[string]int64{"http://busy": 5}

	result := computeProxyAssignments(targets, candidates, baseline, 0)

	if result.assignments[10] != "http://idle" || result.assignments[11] != "http://idle" {
		t.Fatalf("accounts should land on idle proxy first: %+v", result.assignments)
	}
	if result.load["http://idle"] != 2 || result.load["http://busy"] != 5 {
		t.Fatalf("load = %+v, want idle=2 busy=5", result.load)
	}
}

// all 模式:仍在候选池且未超上限的现有绑定保持不动(减少换 IP),失效绑定被重排。
func TestComputeProxyAssignmentsKeepsValidBindings(t *testing.T) {
	targets := []balanceAccount{
		{id: 1, current: "http://p1"},   // 有效绑定 → 保持
		{id: 2, current: "http://dead"}, // 绑定不在候选池 → 重排
		{id: 3},                         // 未绑定 → 分配
	}
	candidates := []string{"http://p1", "http://p2"}

	result := computeProxyAssignments(targets, candidates, nil, 0)

	if result.kept != 1 {
		t.Fatalf("kept = %d, want 1", result.kept)
	}
	if _, ok := result.assignments[1]; ok {
		t.Fatalf("account 1 should keep its binding, got reassigned: %+v", result.assignments)
	}
	if result.assignments[2] != "http://p2" {
		t.Fatalf("account 2 should move to least-loaded p2, got %q", result.assignments[2])
	}
	// 账号 3 分配时 p1/p2 均为 1,并列取字典序 → p1。
	if result.assignments[3] != "http://p1" {
		t.Fatalf("account 3 tie-break should pick p1, got %q", result.assignments[3])
	}
	if result.load["http://p1"] != 2 || result.load["http://p2"] != 1 {
		t.Fatalf("load = %+v, want p1=2 p2=1", result.load)
	}
}

// 每代理上限:超出容量的账号计入 skipped,不强塞。
func TestComputeProxyAssignmentsHonorsCap(t *testing.T) {
	targets := []balanceAccount{{id: 1}, {id: 2}, {id: 3}, {id: 4}, {id: 5}}
	candidates := []string{"http://p1", "http://p2"}

	result := computeProxyAssignments(targets, candidates, nil, 2)

	if len(result.assignments) != 4 || result.skipped != 1 {
		t.Fatalf("assignments=%d skipped=%d, want 4/1", len(result.assignments), result.skipped)
	}
	if result.load["http://p1"] != 2 || result.load["http://p2"] != 2 {
		t.Fatalf("load = %+v, want both at cap 2", result.load)
	}
}

// 上限也约束"保持现有绑定":已超上限的代理上的存量绑定会被挪走。
func TestComputeProxyAssignmentsCapEvictsOverloadedKeep(t *testing.T) {
	targets := []balanceAccount{
		{id: 1, current: "http://p1"},
		{id: 2, current: "http://p1"},
		{id: 3, current: "http://p1"},
	}
	candidates := []string{"http://p1", "http://p2"}

	result := computeProxyAssignments(targets, candidates, nil, 2)

	if result.kept != 2 {
		t.Fatalf("kept = %d, want 2 (cap keeps first two on p1)", result.kept)
	}
	if result.assignments[3] != "http://p2" {
		t.Fatalf("account 3 should overflow to p2, got %+v", result.assignments)
	}
}
