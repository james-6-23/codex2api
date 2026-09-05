package admin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
)

func TestBuildClaudeConnectionTestPayloadUsesMessagesShape(t *testing.T) {
	payload := buildClaudeConnectionTestPayload(nil, "claude-sonnet-4-5", auth.DefaultClaudeSecurityConfig())
	var body map[string]interface{}
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatalf("Claude connection payload is invalid JSON: %v", err)
	}
	if body["model"] != "claude-sonnet-4-5" || body["stream"] != true {
		t.Fatalf("Claude connection payload = %#v", body)
	}
	if _, ok := body["messages"]; !ok {
		t.Fatalf("Claude connection payload missing messages: %#v", body)
	}
	if _, ok := body["input"]; ok {
		t.Fatalf("Claude connection payload must not use Responses input: %#v", body)
	}
}

func TestClaudeProbePayloadsRespectOutputCap(t *testing.T) {
	for _, tc := range []struct {
		limit int64
		want  int64
	}{
		{0, 4096}, {1, 1}, {32, 32}, {1024, 1024},
		{4095, 4095}, {4096, 4096}, {8192, 4096},
	} {
		t.Run(strconv.FormatInt(tc.limit, 10), func(t *testing.T) {
			cfg := auth.ClaudeSecurityConfig{MaxOutputTokens: tc.limit}
			for name, payload := range map[string][]byte{
				"model_probe":     buildClaudeModelProbePayload("claude-sonnet-4-5", cfg),
				"connection_test": buildClaudeConnectionTestPayload(nil, "claude-sonnet-4-5", cfg),
			} {
				var body struct {
					MaxTokens int64 `json:"max_tokens"`
				}
				if err := json.Unmarshal(payload, &body); err != nil {
					t.Fatalf("%s invalid JSON: %v", name, err)
				}
				if body.MaxTokens != tc.want {
					t.Errorf("%s max_tokens = %d, want %d for cap %d", name, body.MaxTokens, tc.want, tc.limit)
				}
			}
		})
	}
}

func TestReadClaudeProbeStreamClassifiesNativeMessagesSuccess(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\"}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n" + "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n" + "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	))}
	status, detail := readClaudeProbeStream(context.Background(), resp)
	if status != modelProbeAvailable || detail != "模型响应正常" {
		t.Fatalf("Claude probe result = (%q, %q), want available/model response normal", status, detail)
	}
}

func TestReadClaudeMessagesStreamEmitsTextDeltas(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	))}
	var got strings.Builder
	status, detail := readClaudeMessagesStream(context.Background(), resp, func(text string) { _, _ = got.WriteString(text) })
	if status != "success" || detail != "测试通过" || got.String() != "hello" {
		t.Fatalf("Claude stream result = (%q, %q, %q)", status, detail, got.String())
	}
}

func TestReadClaudeMessagesStreamAcceptsThinkingOnlyResponse(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"sig\"}}\n\n" +
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	))}
	status, detail := readClaudeMessagesStream(context.Background(), resp, nil)
	if status != "success" || detail != "测试通过" {
		t.Fatalf("Claude thinking-only stream result = (%q, %q), want success", status, detail)
	}
}

func TestReadClaudeMessagesStreamStillFailsWithoutAnyContent(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\"}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	))}
	status, detail := readClaudeMessagesStream(context.Background(), resp, nil)
	if status != "failed" || detail != "Claude 探测未返回文本内容" {
		t.Fatalf("empty Claude stream result = (%q, %q), want failed/no text", status, detail)
	}
}

func TestReadClaudeMessagesStreamAcceptsNonStreamingThinkingOnlyMessage(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(
		`{"type":"message","content":[{"type":"thinking","thinking":"hmm"}],"stop_reason":"max_tokens"}`,
	))}
	status, detail := readClaudeMessagesStream(context.Background(), resp, nil)
	if status != "success" || detail != "测试通过" {
		t.Fatalf("Claude non-stream thinking-only result = (%q, %q), want success", status, detail)
	}
}

func TestReadClaudeMessagesStreamClassifiesRateLimitError(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(
		"event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\",\"message\":\"slow down\"}}\n\n",
	))}
	status, detail := readClaudeMessagesStream(context.Background(), resp, nil)
	if status != "rate_limited" || detail != "slow down" {
		t.Fatalf("Claude rate-limit result = (%q, %q)", status, detail)
	}
}

func TestReadClaudeMessagesStreamAcceptsNonStreamingMessageJSON(t *testing.T) {
	resp := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(
		`{"type":"message","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn"}`,
	))}
	status, detail := readClaudeMessagesStream(context.Background(), resp, nil)
	if status != "success" || detail != "测试通过" {
		t.Fatalf("Claude non-stream result = (%q, %q)", status, detail)
	}
}

func TestClaudeConnectionTestPreservesAuthoritativeRejectedCooldown(t *testing.T) {
	account := &auth.Account{UpstreamType: auth.UpstreamClaude, AccessToken: "claude-token"}
	account.SetCooldownWithReason(time.Hour, auth.ResponsesRateLimitedCooldownReason)
	headers := make(http.Header)
	headers.Set("anthropic-ratelimit-unified-status", "rejected")
	headers.Set("anthropic-ratelimit-unified-5h-utilization", "1")
	headers.Set("anthropic-ratelimit-unified-representative-claim", "five_hour")
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     headers,
	}
	if !claudeResponseHasUsageLimitSignal(resp) {
		t.Fatal("rejected Claude response should carry a usage-limit signal")
	}
	if !claudeConnectionTestShouldPreserveUsageCooldown(account, resp) {
		t.Fatal("manual Claude test must preserve an authoritative cooldown")
	}
}

func TestClaudeConnectionTestAllowsNormalResponseToClearOldCooldown(t *testing.T) {
	account := &auth.Account{UpstreamType: auth.UpstreamClaude, AccessToken: "claude-token"}
	account.SetCooldownWithReason(time.Hour, auth.ResponsesRateLimitedCooldownReason)
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"anthropic-ratelimit-unified-status": []string{"allowed"}}}
	if claudeConnectionTestShouldPreserveUsageCooldown(account, resp) {
		t.Fatal("normal Claude response should not preserve an old cooldown")
	}
}

func TestClaudeConnectionTestPreservesUsageSignalWithoutExistingCooldown(t *testing.T) {
	account := &auth.Account{UpstreamType: auth.UpstreamClaude, AccessToken: "claude-token"}
	headers := make(http.Header)
	headers.Set("anthropic-ratelimit-unified-status", "rejected")
	headers.Set("anthropic-ratelimit-unified-representative-claim", "five_hour")
	headers.Set("anthropic-ratelimit-unified-5h-utilization", "1")
	resp := &http.Response{StatusCode: http.StatusOK, Header: headers}
	if !claudeConnectionTestShouldPreserveUsageCooldown(account, resp) {
		t.Fatal("an authoritative rejected usage signal must prevent transient restore even before local cooldown exists")
	}
}

func TestClaudeConnectionStreamFailureAppliesShortCooldown(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	defer store.Stop()
	account := &auth.Account{UpstreamType: auth.UpstreamClaude, AccessToken: "claude-token", Status: auth.StatusReady}
	h := &Handler{store: store}
	applyClaudeConnectionStreamFailure(h, account, "claude-haiku-4-5", "rate_limited", "slow down", &http.Response{Header: make(http.Header)})
	if !account.HasActiveCooldown() {
		t.Fatal("body-only Claude rate limit from a connection test must apply a cooldown")
	}
}

func TestClaudeConnectionStreamAuthFailureAppliesUnauthorizedCooldown(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	defer store.Stop()
	account := &auth.Account{UpstreamType: auth.UpstreamClaude, AccessToken: "claude-token", Status: auth.StatusReady}
	h := &Handler{store: store}
	applyClaudeConnectionStreamFailure(h, account, "claude-haiku-4-5", "failed", "invalid token", nil)
	reason, _ := account.GetCooldownSnapshot()
	if reason != "unauthorized" {
		t.Fatalf("Claude auth failure cooldown reason = %q, want unauthorized", reason)
	}
}

func TestClaudeConnectionStreamRateLimitDoesNotReplacePreciseWindow(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	defer store.Stop()
	account := &auth.Account{UpstreamType: auth.UpstreamClaude, AccessToken: "claude-token", Status: auth.StatusReady}
	reset := time.Now().Add(3 * time.Hour)
	headers := make(http.Header)
	headers.Set("anthropic-ratelimit-unified-status", "rejected")
	headers.Set("anthropic-ratelimit-unified-representative-claim", "five_hour")
	headers.Set("anthropic-ratelimit-unified-5h-utilization", "1")
	headers.Set("anthropic-ratelimit-unified-5h-reset", strconv.FormatInt(reset.Unix(), 10))
	resp := &http.Response{StatusCode: http.StatusOK, Header: headers}
	proxy.SyncClaudeUsageState(store, account, resp)
	_, before := account.GetCooldownSnapshot()
	applyClaudeConnectionStreamFailure(&Handler{store: store}, account, "claude-haiku-4-5", "rate_limited", "slow down", resp)
	reason, after := account.GetCooldownSnapshot()
	if reason != auth.ResponsesRateLimitedCooldownReason || after.Before(before.Add(-time.Second)) || after.After(before.Add(time.Second)) {
		t.Fatalf("connection test replaced precise Claude cooldown: reason=%q before=%v after=%v", reason, before, after)
	}
}

func TestClaudeConnectionCreditsRequiredIsModelScoped(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	defer store.Stop()
	account := &auth.Account{UpstreamType: auth.UpstreamClaude, AccessToken: "claude-token", Status: auth.StatusReady}
	if handled := syncClaudeTestUsageState(store, account, "claude-fable-5", &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     make(http.Header),
	}, []byte(`{"error":{"type":"rate_limit_error","message":"Usage credits are required for this model.","details":{"error_code":"credits_required","model":"claude-fable-5"}}}`)); !handled {
		t.Fatal("credits_required HTTP failure should be handled as a model-level result")
	}
	if account.HasActiveCooldown() || account.RuntimeStatus() == "rate_limited" {
		t.Fatalf("credits_required must not cool down the account: status=%q", account.RuntimeStatus())
	}
	if !account.IsModelRateLimited("claude-fable-5") {
		t.Fatal("credits_required should cool down only Fable 5")
	}
}

func TestClaudeConnectionStreamCreditsRequiredIsModelScoped(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	defer store.Stop()
	account := &auth.Account{UpstreamType: auth.UpstreamClaude, AccessToken: "claude-token", Status: auth.StatusReady}
	applyClaudeConnectionStreamFailure(&Handler{store: store}, account, "claude-fable-5", "rate_limited", "Usage credits are required for this model.", &http.Response{Header: make(http.Header)})
	if account.HasActiveCooldown() || account.RuntimeStatus() == "rate_limited" {
		t.Fatalf("stream credits_required must not cool down the account: status=%q", account.RuntimeStatus())
	}
	if !account.IsModelRateLimited("claude-fable-5") {
		t.Fatal("stream credits_required should cool down only Fable 5")
	}
}

func TestClaudeProbeModelIDsPreferAccountModels(t *testing.T) {
	account := &auth.Account{UpstreamType: auth.UpstreamClaude, Models: []string{"claude-sonnet-4-5", "claude-haiku-4-5"}}
	got := claudeProbeModelIDs(account)
	if len(got) != 2 || got[0] != "claude-sonnet-4-5" || got[1] != "claude-haiku-4-5" {
		t.Fatalf("Claude probe models = %v", got)
	}
}

func TestClaudeProbeModelIDsRejectNonClaudeCatalogEntries(t *testing.T) {
	account := &auth.Account{UpstreamType: auth.UpstreamClaude, Models: []string{"gpt-5.4", "gemini-2.5-pro"}}
	got := claudeProbeModelIDs(account)
	if len(got) != 0 {
		t.Fatalf("explicit non-Claude catalog should fail closed, got %v", got)
	}
}

func TestClaudeProbeModelIDsUsesFallbackOnlyWithoutExplicitCatalog(t *testing.T) {
	got := claudeProbeModelIDs(&auth.Account{UpstreamType: auth.UpstreamClaude})
	if len(got) == 0 {
		t.Fatal("Claude probe should expose the safe native fallback when no catalog is configured")
	}
	for _, model := range got {
		if !strings.HasPrefix(strings.ToLower(model), "claude-") {
			t.Fatalf("probe model %q crossed the Claude provider boundary", model)
		}
	}
}

func TestClaudeProbeModelIDsRejectsExplicitInvalidCatalog(t *testing.T) {
	account := &auth.Account{UpstreamType: auth.UpstreamClaude, Models: []string{"gpt-5.4"}}
	if got := claudeProbeModelIDs(account); len(got) != 0 {
		t.Fatalf("explicit invalid Claude catalog fell back to models: %v", got)
	}
}

func TestConnectionTestModelForClaudeUsesNativeCatalog(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	defer store.Stop()
	h := &Handler{store: store}
	account := &auth.Account{UpstreamType: auth.UpstreamClaude, Models: []string{"claude-opus-4-5", "claude-haiku-4-5"}}
	model, err := h.connectionTestModelForAccount(context.Background(), account, "")
	if err != nil || model != "claude-haiku-4-5" {
		t.Fatalf("Claude connection test model = (%q, %v), want cheapest Haiku model", model, err)
	}
}

func TestConnectionTestModelForClaudeRejectsStaleRuntimeModel(t *testing.T) {
	db := newTestAdminDB(t)
	ctx := context.Background()
	id, err := db.InsertAccountWithUpstream(ctx, "claude-stale", "anthropic", "oauth", map[string]interface{}{
		"upstream_type": "claude",
		"access_token":  "claude-token",
		"refresh_token": "claude-refresh",
		"models":        []string{"claude-sonnet-5"},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(db, nil, nil)
	defer store.Stop()
	account := &auth.Account{
		DBID:         id,
		UpstreamType: auth.UpstreamClaude,
		AccessToken:  "claude-token",
		Models:       []string{"claude-fable-5", "claude-sonnet-5"},
	}
	store.AddAccount(account)
	h := &Handler{store: store, db: db}
	if _, err := h.connectionTestModelForAccount(ctx, account, "claude-fable-5"); err == nil || !strings.Contains(err.Error(), "持久化模型") {
		t.Fatalf("stale runtime Fable model error = %v, want persisted catalog rejection", err)
	}
}

func TestConnectionTestModelForClaudeSkipsModelCooldown(t *testing.T) {
	account := &auth.Account{
		UpstreamType: auth.UpstreamClaude,
		Models:       []string{"claude-haiku-4-5", "claude-sonnet-5"},
	}
	account.SetModelCooldownUntil("claude-haiku-4-5", "credits_required", time.Now().Add(time.Hour))
	model, err := (&Handler{}).connectionTestModelForAccount(context.Background(), account, "")
	if err != nil || model != "claude-sonnet-5" {
		t.Fatalf("default Claude connection test model=(%q,%v), want cooldown-free sonnet", model, err)
	}
}
