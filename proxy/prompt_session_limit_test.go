package proxy

import (
	"net/http/httptest"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
)

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
