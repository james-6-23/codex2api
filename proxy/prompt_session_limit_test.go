package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestSendPromptSessionCreationLimitErrorIsNonRetryableAndChinese(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	status := promptSessionCreationLimitStatus{Limit: 3}

	sendPromptSessionCreationLimitError(c, status)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	if got := gjson.GetBytes(recorder.Body.Bytes(), "error.code").String(); got != "session_creation_limit_exceeded" {
		t.Fatalf("error.code = %q", got)
	}
	if got := gjson.GetBytes(recorder.Body.Bytes(), "error.message").String(); got != promptSessionCreationLimitMessage(status) {
		t.Fatalf("error.message = %q, want %q", got, promptSessionCreationLimitMessage(status))
	}
}

func promptSessionLimitTestContext(sessionID string) *gin.Context {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	c.Request.Header.Set("Session-Id", sessionID)
	c.Set(contextAPIKeyID, int64(7))
	return c
}

func promptSessionLimitVerifiedUserContext(sessionFingerprint string) *gin.Context {
	c := promptSessionLimitTestContext("")
	c.Set(newAPIPolicyMetaContextKey, verifiedNewAPIPolicyContext{
		Identity: newAPIIdentity{UserID: "42"}, APIKeyID: 7, Platform: "newapi", MetaVerified: true,
		Meta: newAPIPolicyMeta{SessionFingerprint: sessionFingerprint},
	})
	return c
}

func promptSessionLimitOverrideTestHandler(item database.PromptSessionLimitOverride) *Handler {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1})
	store.ReplacePromptFilterNewAPIBindings([]*database.PromptFilterNewAPIBinding{{
		APIKeyID: 7, PlatformCode: "newapi", Enabled: true, RequireSignedIdentity: true,
	}})
	store.ApplyPromptSessionLimitOverride(item)
	return &Handler{store: store}
}

func TestPromptSessionCreationLimitCountsDistinctSessionsOnly(t *testing.T) {
	handler := &Handler{}
	cfg := promptfilter.Config{}
	cfg.Advanced.Risk.SessionCreationLimitEnabled = true
	cfg.Advanced.Risk.SessionCreationLimit = 2
	cfg.Advanced.Risk.SessionCreationLimitWindowSeconds = 3600

	first := promptSessionLimitTestContext("session-a")
	status, exceeded := handler.checkPromptSessionCreationLimit(first, cfg, nil)
	if exceeded || status.Used != 1 {
		t.Fatalf("first session: exceeded=%v used=%d", exceeded, status.Used)
	}
	repeat := promptSessionLimitTestContext("session-a")
	status, exceeded = handler.checkPromptSessionCreationLimit(repeat, cfg, nil)
	if exceeded || !status.Existing || status.Used != 1 {
		t.Fatalf("repeat session: exceeded=%v existing=%v used=%d", exceeded, status.Existing, status.Used)
	}
	second := promptSessionLimitTestContext("session-b")
	if status, exceeded = handler.checkPromptSessionCreationLimit(second, cfg, nil); exceeded || status.Used != 2 {
		t.Fatalf("second session: exceeded=%v used=%d", exceeded, status.Used)
	}
	third := promptSessionLimitTestContext("session-c")
	if status, exceeded = handler.checkPromptSessionCreationLimit(third, cfg, nil); !exceeded || status.Used != 2 || status.RetryAfter <= 0 {
		t.Fatalf("third session: exceeded=%v used=%d retry=%d", exceeded, status.Used, status.RetryAfter)
	}
}

func TestPromptSessionCreationLimitSkipsUnstableIdentity(t *testing.T) {
	handler := &Handler{}
	cfg := promptfilter.Config{}
	cfg.Advanced.Risk.SessionCreationLimitEnabled = true
	cfg.Advanced.Risk.SessionCreationLimit = 1
	cfg.Advanced.Risk.SessionCreationLimitWindowSeconds = 3600
	c := promptSessionLimitTestContext("")
	if status, exceeded := handler.checkPromptSessionCreationLimit(c, cfg, []byte(`{"input":"hello"}`)); exceeded || status.Used != 0 {
		t.Fatalf("unstable identity was counted: exceeded=%v used=%d", exceeded, status.Used)
	}
}

func TestPromptSessionCreationLimitDoesNotCountIdempotencyKey(t *testing.T) {
	handler := &Handler{}
	cfg := promptfilter.Config{}
	cfg.Advanced.Risk.SessionCreationLimitEnabled = true
	cfg.Advanced.Risk.SessionCreationLimit = 1
	cfg.Advanced.Risk.SessionCreationLimitWindowSeconds = 3600
	c := promptSessionLimitTestContext("")
	c.Request.Header.Set("Idempotency-Key", "request-unique-id")
	if status, exceeded := handler.checkPromptSessionCreationLimit(c, cfg, nil); exceeded || status.Used != 0 {
		t.Fatalf("idempotency key was counted as a conversation: exceeded=%v used=%d", exceeded, status.Used)
	}
}

func TestPromptSessionCreationLimitUserCustomOverrideBeatsDisabledGlobal(t *testing.T) {
	handler := promptSessionLimitOverrideTestHandler(database.PromptSessionLimitOverride{
		Platform: "newapi", NewAPIUserID: "42", Mode: database.PromptSessionLimitModeCustom,
		Limit: 1, WindowSeconds: 4800,
	})
	cfg := promptfilter.Config{}
	first := promptSessionLimitVerifiedUserContext(promptSessionTestFingerprint("user-session-a"))
	status, exceeded := handler.checkPromptSessionCreationLimit(first, cfg, nil)
	if exceeded || status.Used != 1 || status.Limit != 1 || status.WindowSeconds != 4800 {
		t.Fatalf("first status=%#v exceeded=%v", status, exceeded)
	}
	second := promptSessionLimitVerifiedUserContext(promptSessionTestFingerprint("user-session-b"))
	if status, exceeded = handler.checkPromptSessionCreationLimit(second, cfg, nil); !exceeded || status.Used != 1 {
		t.Fatalf("second status=%#v exceeded=%v", status, exceeded)
	}
}

func TestPromptSessionCreationLimitUserOffOverrideBeatsEnabledGlobal(t *testing.T) {
	handler := promptSessionLimitOverrideTestHandler(database.PromptSessionLimitOverride{
		Platform: "newapi", NewAPIUserID: "42", Mode: database.PromptSessionLimitModeOff,
	})
	cfg := promptfilter.Config{}
	cfg.Advanced.Risk.SessionCreationLimitEnabled = true
	cfg.Advanced.Risk.SessionCreationLimit = 1
	cfg.Advanced.Risk.SessionCreationLimitWindowSeconds = 3600
	status, exceeded := handler.checkPromptSessionCreationLimit(promptSessionLimitVerifiedUserContext(promptSessionTestFingerprint("off-session")), cfg, nil)
	if exceeded || status.Enabled || status.Used != 0 {
		t.Fatalf("off override status=%#v exceeded=%v", status, exceeded)
	}
}

func TestPromptSessionCreationLimitIgnoresSelectedAccountWithoutWindowControl(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1})
	cfg := promptfilter.Config{}
	cfg.Advanced.Risk.SessionCreationLimitEnabled = true
	cfg.Advanced.Risk.SessionCreationLimit = 1
	cfg.Advanced.Risk.SessionCreationLimitWindowSeconds = 3600
	store.SetPromptFilterConfig(cfg)
	handler := &Handler{store: store}
	account := &auth.Account{DBID: 91, SessionCapacityEnabled: false}

	for _, sessionID := range []string{"passive-luna-a", "passive-luna-b"} {
		status, exceeded := handler.checkPromptSessionCreationLimitForSelectedAccount(promptSessionLimitTestContext(sessionID), nil, account)
		if exceeded || status.Enabled || status.Used != 0 {
			t.Fatalf("disabled account counted %q: status=%#v exceeded=%v", sessionID, status, exceeded)
		}
	}
	if len(handler.promptSessionLimits) != 0 {
		t.Fatalf("disabled account populated user windows: %#v", handler.promptSessionLimits)
	}
}

func TestPromptSessionCreationLimitCountsOnlyAfterWindowControlledAccountSelected(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1})
	cfg := promptfilter.Config{}
	cfg.Advanced.Risk.SessionCreationLimitEnabled = true
	cfg.Advanced.Risk.SessionCreationLimit = 1
	cfg.Advanced.Risk.SessionCreationLimitWindowSeconds = 3600
	store.SetPromptFilterConfig(cfg)
	handler := &Handler{store: store}
	disabled := &auth.Account{DBID: 91, SessionCapacityEnabled: false}
	enabled := &auth.Account{
		DBID: 92, SessionCapacityEnabled: true, SessionCapacityMax: 5,
		SessionCapacityIdleTTLSeconds: 3600,
	}

	if status, exceeded := handler.checkPromptSessionCreationLimitForSelectedAccount(promptSessionLimitTestContext("audit-window"), nil, disabled); exceeded || status.Used != 0 {
		t.Fatalf("passive API request was counted: status=%#v exceeded=%v", status, exceeded)
	}
	if status, exceeded := handler.checkPromptSessionCreationLimitForSelectedAccount(promptSessionLimitTestContext("main-window"), nil, enabled); exceeded || status.Used != 1 {
		t.Fatalf("window-controlled account did not count first session: status=%#v exceeded=%v", status, exceeded)
	}
	if status, exceeded := handler.checkPromptSessionCreationLimitForSelectedAccount(promptSessionLimitTestContext("second-main-window"), nil, enabled); !exceeded || status.Used != 1 {
		t.Fatalf("window-controlled account did not enforce limit: status=%#v exceeded=%v", status, exceeded)
	}
}

func TestPromptSessionCreationLimitIgnoresInternalReviewRequest(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1})
	cfg := promptfilter.Config{}
	cfg.Advanced.Risk.SessionCreationLimitEnabled = true
	cfg.Advanced.Risk.SessionCreationLimit = 1
	cfg.Advanced.Risk.SessionCreationLimitWindowSeconds = 3600
	store.SetPromptFilterConfig(cfg)
	handler := &Handler{store: store}
	account := &auth.Account{
		DBID: 92, SessionCapacityEnabled: true, SessionCapacityMax: 5,
		SessionCapacityIdleTTLSeconds: 3600,
	}
	c := promptSessionLimitTestContext("internal-review-window")
	c.Set("prompt_intelligence_internal", true)

	status, exceeded := handler.checkPromptSessionCreationLimitForSelectedAccount(c, nil, account)
	if exceeded || status.Enabled || status.Used != 0 || len(handler.promptSessionLimits) != 0 {
		t.Fatalf("internal review request was counted: status=%#v exceeded=%v sessions=%#v", status, exceeded, handler.promptSessionLimits)
	}
}
