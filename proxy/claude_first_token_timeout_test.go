package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

func TestClaudeFirstTokenTimeoutFor_PrefersClaudeSettingForClaudeAccounts(t *testing.T) {
	prev := CurrentRuntimeSettings()
	ApplyRuntimeSettings(NormalizeRuntimeSettings(RuntimeSettings{FirstTokenTimeoutSec: 30}))
	t.Cleanup(func() { ApplyRuntimeSettings(prev) })

	store := auth.NewStore(nil, nil, nil)
	defer store.Stop()
	store.SetClaudeFirstTokenTimeoutSeconds(45)
	claude := &auth.Account{DBID: 251, UpstreamType: auth.UpstreamClaude}
	codex := &auth.Account{DBID: 1}

	if got := claudeFirstTokenTimeoutFor(store, claude); got != 45*time.Second {
		t.Fatalf("claude account must use the Claude setting: %s", got)
	}
	if got := claudeFirstTokenTimeoutFor(store, codex); got != 30*time.Second {
		t.Fatalf("non-claude account must keep the global timeout: %s", got)
	}
	store.SetClaudeFirstTokenTimeoutSeconds(0)
	if got := claudeFirstTokenTimeoutFor(store, claude); got != 30*time.Second {
		t.Fatalf("Claude setting 0 must fall back to global: %s", got)
	}
	if got := claudeFirstTokenTimeoutFor(nil, claude); got != 30*time.Second {
		t.Fatalf("nil store must fall back to global: %s", got)
	}
}

func TestClaudeNativeFirstTokenOutcome_MapsGuardTimeoutToFirstTokenTimeout(t *testing.T) {
	_, cancel := context.WithCancel(context.Background())
	defer cancel()
	guard := newFirstTokenTimeoutGuard(5*time.Millisecond, cancel)
	time.Sleep(30 * time.Millisecond)
	if !guard.TimedOut() {
		t.Fatal("guard must have fired")
	}
	broken := streamOutcome{logStatusCode: logStatusUpstreamStreamBreak, failureMessage: "上游流中断"}
	got := claudeNativeFirstTokenOutcome(guard, 0, broken, 5*time.Millisecond)
	if got.failureKind != "timeout" || got.logStatusCode != logStatusUpstreamStreamBreak || !got.penalize {
		t.Fatalf("timed-out attempt without a visible token must become a first-token timeout outcome: %+v", got)
	}
	// A visible token arrived before the guard fired: keep the real outcome.
	if got := claudeNativeFirstTokenOutcome(guard, 1200, broken, 5*time.Millisecond); got.failureKind != "" {
		t.Fatalf("visible token must keep the original outcome: %+v", got)
	}
	ok := streamOutcome{logStatusCode: http.StatusOK}
	if got := claudeNativeFirstTokenOutcome(guard, 0, ok, 5*time.Millisecond); got.logStatusCode != http.StatusOK {
		t.Fatalf("successful stream must stay successful: %+v", got)
	}
	if got := claudeNativeFirstTokenOutcome(nil, 0, broken, 0); got.failureKind != "" {
		t.Fatalf("nil guard must keep the original outcome: %+v", got)
	}
}

func TestActivateClaudeStreamKeepalive(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	defer store.Stop()
	claude := &auth.Account{DBID: 251, UpstreamType: auth.UpstreamClaude}
	codex := &auth.Account{DBID: 1}

	newCtx := func() (*gin.Context, func()) {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
		stop := installContinuousRetrySSEKeepalive(c, true, "text/event-stream; charset=utf-8")
		return c, stop
	}

	c, stop := newCtx()
	activateClaudeStreamKeepalive(c.Request.Context(), store, claude, true)
	if !continuousRetryKeepaliveActive(c.Request.Context()) {
		t.Fatal("claude stream must activate the pre-first-token keepalive")
	}
	stop()

	c, stop = newCtx()
	activateClaudeStreamKeepalive(c.Request.Context(), store, codex, true)
	if continuousRetryKeepaliveActive(c.Request.Context()) {
		t.Fatal("non-claude account must not activate the keepalive")
	}
	stop()

	c, stop = newCtx()
	activateClaudeStreamKeepalive(c.Request.Context(), store, claude, false)
	if continuousRetryKeepaliveActive(c.Request.Context()) {
		t.Fatal("non-stream request must not activate the keepalive")
	}
	stop()

	store.SetClaudeStreamKeepaliveEnabled(false)
	c, stop = newCtx()
	activateClaudeStreamKeepalive(c.Request.Context(), store, claude, true)
	if continuousRetryKeepaliveActive(c.Request.Context()) {
		t.Fatal("disabled switch must not activate the keepalive")
	}
	stop()
}

func TestClaudeFirstTokenSlow(t *testing.T) {
	if claudeFirstTokenSlow(59_999) {
		t.Fatal("below threshold must not be slow")
	}
	if !claudeFirstTokenSlow(60_000) {
		t.Fatal("threshold must count as slow")
	}
	if claudeFirstTokenSlow(0) {
		t.Fatal("no first token recorded must not be reported as slow")
	}
}
