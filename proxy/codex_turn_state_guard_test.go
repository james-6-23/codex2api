package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

func newTurnStateTestContext(t *testing.T) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	req, err := http.NewRequest(http.MethodPost, "/v1/responses", nil)
	if err != nil {
		t.Fatalf("http.NewRequest: %v", err)
	}
	c.Request = req
	return c, recorder
}

// 跨账号回声守卫:换号后客户端回带旧账号铸造的 turn-state 必须剥离;
// 同账号、无溯源记录或无会话标识时保持透传。
func TestGuardCodexTurnStateEchoStripsCrossAccountEcho(t *testing.T) {
	minter := &auth.Account{DBID: 101}
	other := &auth.Account{DBID: 202}
	affinityKey := "turn-guard-conv-1::api-key:9"
	t.Cleanup(func() { codexTurnStateOrigins.Delete(affinityKey) })

	c, _ := newTurnStateTestContext(t)
	upstream := http.Header{}
	upstream.Set(codexTurnStateHeader, "blob-from-minter")
	relayCodexTurnStateResponseHeader(c, affinityKey, minter, upstream)

	echo := http.Header{}
	echo.Set(codexTurnStateHeader, "blob-from-minter")
	guardCodexTurnStateEcho(affinityKey, minter, echo)
	if got := echo.Get(codexTurnStateHeader); got != "blob-from-minter" {
		t.Fatalf("same-account echo was modified: %q", got)
	}

	guardCodexTurnStateEcho(affinityKey, other, echo)
	if got := echo.Get(codexTurnStateHeader); got != "" {
		t.Fatalf("cross-account echo was forwarded: %q", got)
	}

	untracked := http.Header{}
	untracked.Set(codexTurnStateHeader, "blob-unknown")
	guardCodexTurnStateEcho("turn-guard-conv-untracked", other, untracked)
	if got := untracked.Get(codexTurnStateHeader); got != "blob-unknown" {
		t.Fatalf("untracked echo was modified: %q", got)
	}

	noSession := http.Header{}
	noSession.Set(codexTurnStateHeader, "blob-no-session")
	guardCodexTurnStateEcho("", other, noSession)
	if got := noSession.Get(codexTurnStateHeader); got != "blob-no-session" {
		t.Fatalf("sessionless echo was modified: %q", got)
	}
}

func TestGuardCodexTurnStateEchoExpiredProvenanceIsDropped(t *testing.T) {
	affinityKey := "turn-guard-conv-expired::api-key:9"
	t.Cleanup(func() { codexTurnStateOrigins.Delete(affinityKey) })
	codexTurnStateOrigins.Store(affinityKey, codexTurnStateOrigin{
		accountID: 101,
		expiresAt: time.Now().Add(-time.Second),
	})

	echo := http.Header{}
	echo.Set(codexTurnStateHeader, "blob-expired")
	guardCodexTurnStateEcho(affinityKey, &auth.Account{DBID: 202}, echo)
	if got := echo.Get(codexTurnStateHeader); got != "blob-expired" {
		t.Fatalf("expired provenance still stripped echo: %q", got)
	}
	if _, ok := codexTurnStateOrigins.Load(affinityKey); ok {
		t.Fatal("expired provenance record was not deleted")
	}
}

// 上游无 turn-state 时必须清除 writer 上残留的上一 failover attempt 的值,
// 防止旧账号的 blob 粘到新账号的响应上。
func TestRelayCodexTurnStateClearsStaleHeaderOnFailover(t *testing.T) {
	affinityKey := "turn-guard-conv-stale::api-key:9"
	t.Cleanup(func() { codexTurnStateOrigins.Delete(affinityKey) })

	c, recorder := newTurnStateTestContext(t)
	first := http.Header{}
	first.Set(codexTurnStateHeader, "blob-attempt-1")
	relayCodexTurnStateResponseHeader(c, affinityKey, &auth.Account{DBID: 101}, first)
	if got := recorder.Header().Get(codexTurnStateHeader); got != "blob-attempt-1" {
		t.Fatalf("first relay header = %q", got)
	}

	relayCodexTurnStateResponseHeader(c, affinityKey, &auth.Account{DBID: 202}, http.Header{})
	if got := recorder.Header().Get(codexTurnStateHeader); got != "" {
		t.Fatalf("stale turn-state survived failover relay: %q", got)
	}
}

func TestCommitResponsesStreamAttemptStagesWinningTurnStateBeforeHeadersCommit(t *testing.T) {
	affinityKey := "turn-guard-winning-header::api-key:9"
	t.Cleanup(func() { codexTurnStateOrigins.Delete(affinityKey) })
	c, recorder := newTurnStateTestContext(t)
	flusher, _ := c.Writer.(http.Flusher)
	attempt := newContinuousRetryStreamAttempt(true, c.Writer, flusher)
	t.Cleanup(func() { _ = attempt.Close() })
	if _, err := attempt.replay.Write([]byte("data: {\"type\":\"response.completed\"}\n\n")); err != nil {
		t.Fatalf("buffer attempt: %v", err)
	}
	upstream := http.Header{}
	upstream.Set(codexTurnStateHeader, "winning-turn-state")
	account := &auth.Account{DBID: 303}

	if err := (&Handler{}).commitResponsesStreamAttempt(c, attempt, affinityKey, account, upstream); err != nil {
		t.Fatalf("commit response attempt: %v", err)
	}
	result := recorder.Result()
	if got := result.Header.Get(codexTurnStateHeader); got != "winning-turn-state" {
		t.Fatalf("committed response header = %q, want winning-turn-state", got)
	}
	raw, ok := codexTurnStateOrigins.Load(affinityKey)
	origin, valid := raw.(codexTurnStateOrigin)
	if !ok || !valid || origin.accountID != account.ID() {
		t.Fatalf("winning turn-state provenance = %#v, present=%v", raw, ok)
	}
}

func TestCommitResponsesStreamAttemptDoesNotForgeTurnStateAfterHeartbeat(t *testing.T) {
	affinityKey := "turn-guard-heartbeat-committed::api-key:9"
	t.Cleanup(func() { codexTurnStateOrigins.Delete(affinityKey) })
	c, recorder := newTurnStateTestContext(t)
	_, _ = c.Writer.WriteString(continuousRetryKeepaliveComment)
	c.Writer.Flush()
	flusher, _ := c.Writer.(http.Flusher)
	attempt := newContinuousRetryStreamAttempt(true, c.Writer, flusher)
	t.Cleanup(func() { _ = attempt.Close() })
	if _, err := attempt.replay.Write([]byte("data: {\"type\":\"response.completed\"}\n\n")); err != nil {
		t.Fatalf("buffer attempt: %v", err)
	}
	upstream := http.Header{}
	upstream.Set(codexTurnStateHeader, "late-turn-state")

	if err := (&Handler{}).commitResponsesStreamAttempt(c, attempt, affinityKey, &auth.Account{DBID: 404}, upstream); err != nil {
		t.Fatalf("commit response attempt: %v", err)
	}
	result := recorder.Result()
	if got := result.Header.Get(codexTurnStateHeader); got != "" {
		t.Fatalf("late turn-state was forged into committed headers: %q", got)
	}
	if _, ok := codexTurnStateOrigins.Load(affinityKey); ok {
		t.Fatal("undelivered turn-state recorded provenance")
	}
}

// 会话级 beta-features:未声明时补默认(deviceCfg 优先于内置默认),
// 客户端显式声明的原样保留(即使不含 remote_compaction_v2)。
func TestApplyCodexRequestHeadersSessionLevelBetaFeatures(t *testing.T) {
	acc := &auth.Account{DBID: 42, AccountID: "acct-42"}

	plain, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	if err != nil {
		t.Fatalf("http.NewRequest: %v", err)
	}
	applyCodexRequestHeaders(plain, acc, "token-123", "cache-key-1", "api-key-1", nil, http.Header{})
	if got := plain.Header.Get(codexBetaFeaturesHeader); got != defaultCodexBetaFeatures {
		t.Fatalf("undeclared beta features = %q, want %q", got, defaultCodexBetaFeatures)
	}

	device, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	if err != nil {
		t.Fatalf("http.NewRequest: %v", err)
	}
	applyCodexRequestHeaders(device, acc, "token-123", "cache-key-1", "api-key-1", &DeviceProfileConfig{BetaFeatures: "multi_agent"}, http.Header{})
	if got := device.Header.Get(codexBetaFeaturesHeader); got != "multi_agent" {
		t.Fatalf("device-profile beta features = %q, want multi_agent", got)
	}

	declared, err := http.NewRequest(http.MethodPost, "https://example.com/v1/responses", nil)
	if err != nil {
		t.Fatalf("http.NewRequest: %v", err)
	}
	applyCodexRequestHeaders(declared, acc, "token-123", "cache-key-1", "api-key-1", nil, http.Header{
		codexBetaFeaturesHeader: []string{"custom_feature"},
	})
	if got := declared.Header.Get(codexBetaFeaturesHeader); got != "custom_feature" {
		t.Fatalf("client-declared beta features rewritten: %q", got)
	}
}
