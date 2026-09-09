package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
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

func promptSessionLimitVerifiedRootUserContext(sessionFingerprint, rootSessionFingerprint string) *gin.Context {
	c := promptSessionLimitTestContext("")
	c.Set(newAPIPolicyMetaContextKey, verifiedNewAPIPolicyContext{
		Identity: newAPIIdentity{UserID: "42"}, APIKeyID: 7, Platform: "newapi", MetaVerified: true,
		Meta: newAPIPolicyMeta{
			SessionFingerprint:     sessionFingerprint,
			RootSessionVersion:     1,
			RootSessionState:       newAPIPolicyRootSessionResolved,
			RootSessionFingerprint: rootSessionFingerprint,
		},
	})
	return c
}

func promptSessionLimitVerifiedTestHandler(t *testing.T) *Handler {
	t.Helper()
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1})
	t.Cleanup(store.Stop)
	store.ReplacePromptFilterNewAPIBindings([]*database.PromptFilterNewAPIBinding{{
		APIKeyID: 7, PlatformCode: "newapi", Enabled: true, RequireSignedIdentity: true,
	}})
	return &Handler{store: store}
}

func promptSessionLimitOverrideTestHandler(t *testing.T, item database.PromptSessionLimitOverride) *Handler {
	t.Helper()
	handler := promptSessionLimitVerifiedTestHandler(t)
	store := handler.store
	store.ApplyPromptSessionLimitOverride(item)
	return handler
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

func TestPromptSessionWindowReportsActualRecoveryWithoutRefreshingExpiry(test *testing.T) {
	handler := &Handler{}
	config := promptfilter.Config{}
	config.Advanced.Risk.SessionCreationLimitEnabled = true
	config.Advanced.Risk.SessionCreationLimit = 2
	config.Advanced.Risk.SessionCreationLimitWindowSeconds = 3600
	first := promptSessionLimitTestContext("recovery-first")
	firstStatus, exceeded := handler.checkPromptSessionCreationLimit(first, config, nil)
	if exceeded || first.Writer.Header().Get("X-Codex2API-Session-Next-Recovery-At") == "" {
		test.Fatal("new window did not report its actual expiry")
	}
	expiresAt := time.Now().Add(90 * time.Second).Truncate(time.Second)
	handler.promptSessionLimitMu.Lock()
	handler.promptSessionLimits[firstStatus.Subject][firstStatus.SessionHash] = expiresAt
	handler.promptSessionLimitMu.Unlock()
	for _, sessionID := range []string{"recovery-first", "recovery-second", "recovery-third"} {
		requestContext := promptSessionLimitTestContext(sessionID)
		status, rejected := handler.checkPromptSessionCreationLimit(requestContext, config, nil)
		if got := requestContext.Writer.Header().Get("X-Codex2API-Session-Next-Recovery-At"); got != fmt.Sprint(expiresAt.Unix()) {
			test.Fatalf("%s changed the next release: %s", sessionID, got)
		}
		if rejected != (sessionID == "recovery-third") {
			test.Fatalf("unexpected admission for %s: %+v", sessionID, status)
		}
		if rejected {
			sendPromptSessionCreationLimitError(requestContext, status)
			message := promptSessionCreationLimitAPIError(status).Message
			if !strings.Contains(message, expiresAt.UTC().Format("2006-01-02 15:04:05 UTC")) || !strings.Contains(message, "秒后") {
				test.Fatalf("missing user recovery estimate: %s", message)
			}
		}
	}
}

func TestPromptSessionCreationLimitRestoresFromRuntimeCacheAfterRestart(t *testing.T) {
	runtimeCache := cache.NewMemory(1)
	defer runtimeCache.Close()
	cfg := promptfilter.Config{}
	cfg.Advanced.Risk.SessionCreationLimitEnabled = true
	cfg.Advanced.Risk.SessionCreationLimit = 2
	cfg.Advanced.Risk.SessionCreationLimitWindowSeconds = 3600

	firstHandler := &Handler{cache: runtimeCache}
	if status, exceeded := firstHandler.checkPromptSessionCreationLimit(promptSessionLimitTestContext("restart-session-a"), cfg, nil); exceeded || status.Used != 1 {
		t.Fatalf("first process status=%#v exceeded=%v", status, exceeded)
	}

	secondHandler := &Handler{cache: runtimeCache}
	if status, exceeded := secondHandler.checkPromptSessionCreationLimit(promptSessionLimitTestContext("restart-session-b"), cfg, nil); exceeded || status.Used != 2 {
		t.Fatalf("restored second session status=%#v exceeded=%v", status, exceeded)
	}
	if status, exceeded := secondHandler.checkPromptSessionCreationLimit(promptSessionLimitTestContext("restart-session-c"), cfg, nil); !exceeded || status.Used != 2 {
		t.Fatalf("restored limit status=%#v exceeded=%v", status, exceeded)
	}
}

func TestPromptSessionCreationLimitCountsAlternateExplicitSessionHeaders(t *testing.T) {
	for _, header := range []string{"X-Session-ID", "OpenAI-Session-ID"} {
		t.Run(header, func(t *testing.T) {
			handler := &Handler{}
			cfg := promptfilter.Config{}
			cfg.Advanced.Risk.SessionCreationLimitEnabled = true
			cfg.Advanced.Risk.SessionCreationLimit = 1
			cfg.Advanced.Risk.SessionCreationLimitWindowSeconds = 3600

			request := func(value string) *gin.Context {
				c := promptSessionLimitTestContext("")
				c.Request.Header.Set(header, value)
				return c
			}
			if status, exceeded := handler.checkPromptSessionCreationLimit(request("session-a"), cfg, nil); exceeded || status.Used != 1 {
				t.Fatalf("first alternate session: status=%#v exceeded=%v", status, exceeded)
			}
			if status, exceeded := handler.checkPromptSessionCreationLimit(request("session-b"), cfg, nil); !exceeded || status.Used != 1 {
				t.Fatalf("second alternate session: status=%#v exceeded=%v", status, exceeded)
			}
		})
	}
}

func TestPromptSessionCreationLimitSameRootGuardianDoesNotDuplicate(t *testing.T) {
	handler := promptSessionLimitVerifiedTestHandler(t)
	cfg := promptfilter.Config{}
	cfg.Advanced.Risk.SessionCreationLimitEnabled = true
	cfg.Advanced.Risk.SessionCreationLimit = 1
	cfg.Advanced.Risk.SessionCreationLimitWindowSeconds = 3600
	root := promptSessionTestFingerprint("visible-root-session")

	mainRequest := promptSessionLimitVerifiedRootUserContext(promptSessionTestFingerprint("main-leaf"), root)
	status, exceeded := handler.checkPromptSessionCreationLimit(mainRequest, cfg, nil)
	if exceeded || status.Used != 1 || status.Existing {
		t.Fatalf("main request: status=%#v exceeded=%v", status, exceeded)
	}

	guardianRequest := promptSessionLimitVerifiedRootUserContext(promptSessionTestFingerprint("guardian-leaf"), root)
	status, exceeded = handler.checkPromptSessionCreationLimit(guardianRequest, cfg, nil)
	if exceeded || status.Used != 1 || !status.Existing {
		t.Fatalf("same-root guardian consumed a second window: status=%#v exceeded=%v", status, exceeded)
	}
}

func TestPromptSessionCreationLimitRelatedRequestNeverCreatesExpiredRoot(t *testing.T) {
	handler := promptSessionLimitVerifiedTestHandler(t)
	cfg := promptfilter.Config{}
	cfg.Advanced.Risk.SessionCreationLimitEnabled = true
	cfg.Advanced.Risk.SessionCreationLimit = 1
	cfg.Advanced.Risk.SessionCreationLimitWindowSeconds = 3600
	c := promptSessionLimitVerifiedRootUserContext(
		promptSessionTestFingerprint("related-leaf"),
		promptSessionTestFingerprint("expired-root"),
	)
	value, _ := c.Get(newAPIPolicyMetaContextKey)
	policyContext := value.(verifiedNewAPIPolicyContext)
	policyContext.Meta.RootSessionRelation = newAPIPolicyRootSessionRelationRelated
	policyContext.Meta.ThreadSource = "future_new_source"
	c.Set(newAPIPolicyMetaContextKey, policyContext)

	status, exceeded := handler.checkPromptSessionCreationLimit(c, cfg, []byte(`{"model":"gpt-5.6-luna","input":"review"}`))
	if exceeded || status.Used != 0 || len(handler.promptSessionLimits) != 0 {
		t.Fatalf("related request created a user window: status=%#v exceeded=%v sessions=%#v", status, exceeded, handler.promptSessionLimits)
	}
}

func TestPromptSessionCreationLimitUserCompactionDoesNotRestoreMissingRootWindow(t *testing.T) {
	handler := promptSessionLimitVerifiedTestHandler(t)
	cfg := promptfilter.Config{}
	cfg.Advanced.Risk.SessionCreationLimitEnabled = true
	cfg.Advanced.Risk.SessionCreationLimit = 1
	cfg.Advanced.Risk.SessionCreationLimitWindowSeconds = 3600
	c := promptSessionLimitVerifiedRootUserContext(
		promptSessionTestFingerprint("user-compaction-leaf"),
		promptSessionTestFingerprint("user-compaction-root"),
	)
	value, _ := c.Get(newAPIPolicyMetaContextKey)
	policyContext := value.(verifiedNewAPIPolicyContext)
	policyContext.Meta.RootSessionRelation = newAPIPolicyRootSessionRelationRelated
	policyContext.Meta.ThreadSource = "user"
	policyContext.Meta.RequestKind = "compaction"
	c.Set(newAPIPolicyMetaContextKey, policyContext)

	status, exceeded := handler.checkPromptSessionCreationLimit(c, cfg, []byte(`{"model":"gpt-5.6-sol","input":"continue"}`))
	if exceeded || status.Used != 0 || status.SessionHash == "" || len(handler.promptSessionLimits) != 0 {
		t.Fatalf("user compaction created a missing root window: status=%#v exceeded=%v sessions=%#v", status, exceeded, handler.promptSessionLimits)
	}
}

func TestPromptSessionCreationLimitProtocolCompactionDoesNotCreateWindow(t *testing.T) {
	for _, testCase := range []struct {
		name string
		path string
		body []byte
	}{
		{name: "response create trigger", path: "/v1/responses", body: []byte(`{"model":"gpt-5.6-sol","input":[{"type":"compaction_trigger"}]}`)},
		{name: "compact endpoint", path: "/v1/responses/compact", body: []byte(`{"model":"gpt-5.6-sol","input":"context"}`)},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			handler := promptSessionLimitVerifiedTestHandler(t)
			cfg := promptfilter.Config{}
			cfg.Advanced.Risk.SessionCreationLimitEnabled = true
			cfg.Advanced.Risk.SessionCreationLimit = 1
			cfg.Advanced.Risk.SessionCreationLimitWindowSeconds = 3600
			c := promptSessionLimitVerifiedRootUserContext(
				promptSessionTestFingerprint("protocol-compaction-leaf-"+testCase.name),
				promptSessionTestFingerprint("protocol-compaction-root-"+testCase.name),
			)
			c.Request.URL.Path = testCase.path

			status, exceeded := handler.checkPromptSessionCreationLimit(c, cfg, testCase.body)
			if exceeded || status.Used != 0 || status.SessionHash == "" || len(handler.promptSessionLimits) != 0 {
				t.Fatalf("protocol compaction created a user window: status=%#v exceeded=%v sessions=%#v", status, exceeded, handler.promptSessionLimits)
			}
		})
	}
}

func TestPromptSessionCreationLimitUserCompactionReusesExistingRootWithoutChangingCreation(t *testing.T) {
	handler := promptSessionLimitVerifiedTestHandler(t)
	cfg := promptfilter.Config{MaxTextLength: promptfilter.DefaultMaxTextLength}
	cfg.Advanced.Risk.SessionCreationLimitEnabled = true
	cfg.Advanced.Risk.SessionCreationLimit = 2
	cfg.Advanced.Risk.SessionCreationLimitWindowSeconds = 3600
	root := promptSessionTestFingerprint("user-compaction-existing-root")

	mainRequest := promptSessionLimitVerifiedRootUserContext(promptSessionTestFingerprint("user-main-leaf"), root)
	status, exceeded := handler.checkPromptSessionCreationLimit(mainRequest, cfg, []byte(`{"model":"gpt-5.6-sol","input":"original user prompt"}`))
	if exceeded || status.Used != 1 || status.Existing {
		t.Fatalf("main request: status=%#v exceeded=%v", status, exceeded)
	}
	handler.promptSessionLimitMu.Lock()
	beforeExpiry := handler.promptSessionLimits[status.Subject][status.SessionHash]
	before := handler.promptSessionWindowDetails[status.Subject][status.SessionHash]
	handler.promptSessionLimitMu.Unlock()

	compaction := promptSessionLimitVerifiedRootUserContext(promptSessionTestFingerprint("user-compaction-leaf"), root)
	policy := compaction.MustGet(newAPIPolicyMetaContextKey).(verifiedNewAPIPolicyContext)
	policy.Meta.RootSessionRelation = newAPIPolicyRootSessionRelationRelated
	policy.Meta.ThreadSource = "user"
	policy.Meta.RequestKind = "compaction"
	compaction.Set(newAPIPolicyMetaContextKey, policy)
	status, exceeded = handler.checkPromptSessionCreationLimit(compaction, cfg, []byte(`{"model":"gpt-5.4","input":"CONTEXT CHECKPOINT COMPACTION"}`))
	if exceeded || status.Used != 1 || !status.Existing {
		t.Fatalf("compaction reuse: status=%#v exceeded=%v", status, exceeded)
	}
	handler.promptSessionLimitMu.Lock()
	afterExpiry := handler.promptSessionLimits[status.Subject][status.SessionHash]
	after := handler.promptSessionWindowDetails[status.Subject][status.SessionHash]
	handler.promptSessionLimitMu.Unlock()
	if !afterExpiry.Equal(beforeExpiry) || !after.CreatedAt.Equal(before.CreatedAt) || after.Model != before.Model || after.PromptPreview != before.PromptPreview {
		t.Fatalf("compaction changed root creation detail: before=%#v after=%#v", before, after)
	}
}

func TestPromptSessionCreationLimitUserForkStillCreatesIndependentWindow(t *testing.T) {
	handler := promptSessionLimitVerifiedTestHandler(t)
	cfg := promptfilter.Config{}
	cfg.Advanced.Risk.SessionCreationLimitEnabled = true
	cfg.Advanced.Risk.SessionCreationLimit = 2
	cfg.Advanced.Risk.SessionCreationLimitWindowSeconds = 3600
	sourceRoot := promptSessionTestFingerprint("fork-source-root")
	forkRoot := promptSessionTestFingerprint("fork-current-root")

	if status, exceeded := handler.checkPromptSessionCreationLimit(promptSessionLimitVerifiedRootUserContext(promptSessionTestFingerprint("source-leaf"), sourceRoot), cfg, nil); exceeded || status.Used != 1 {
		t.Fatalf("source request: status=%#v exceeded=%v", status, exceeded)
	}
	fork := promptSessionLimitVerifiedRootUserContext(promptSessionTestFingerprint("fork-leaf"), forkRoot)
	policy := fork.MustGet(newAPIPolicyMetaContextKey).(verifiedNewAPIPolicyContext)
	policy.Meta.RootSessionRelation = newAPIPolicyRootSessionRelationRelated
	policy.Meta.ThreadSource = "user"
	policy.Meta.RequestKind = "turn"
	policy.Meta.ForkedFromSessionFingerprint = sourceRoot
	fork.Set(newAPIPolicyMetaContextKey, policy)

	status, exceeded := handler.checkPromptSessionCreationLimit(fork, cfg, nil)
	if exceeded || status.Used != 2 || status.Existing {
		t.Fatalf("fork did not create an independent window: status=%#v exceeded=%v", status, exceeded)
	}
}

func TestPromptSessionCreationLimitLegacyWindowIsNotBackfilledFromCompaction(t *testing.T) {
	runtimeCache := cache.NewMemory(1)
	defer runtimeCache.Close()
	store := auth.NewStore(nil, runtimeCache, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1})
	t.Cleanup(store.Stop)
	store.ReplacePromptFilterNewAPIBindings([]*database.PromptFilterNewAPIBinding{{
		APIKeyID: 7, PlatformCode: "newapi", Enabled: true, RequireSignedIdentity: true,
	}})
	cfg := promptfilter.Config{MaxTextLength: promptfilter.DefaultMaxTextLength}
	cfg.Advanced.Risk.SessionCreationLimitEnabled = true
	cfg.Advanced.Risk.SessionCreationLimit = 2
	cfg.Advanced.Risk.SessionCreationLimitWindowSeconds = 3600
	store.SetPromptFilterConfig(cfg)
	handler := &Handler{store: store, cache: runtimeCache}
	account := &auth.Account{DBID: 92, SessionCapacityEnabled: true, SessionCapacityMax: 5, SessionCapacityIdleTTLSeconds: 3600}
	root := promptSessionTestFingerprint("legacy-compaction-root")
	sessionHash := hashRiskIdentity(root)
	subject := cache.PromptSessionLimitSubject("newapi", "42")
	expiresAt := time.Now().Add(time.Hour)
	payload, err := json.Marshal(cache.PromptSessionLimitState{
		Version:  1,
		Sessions: map[string]time.Time{sessionHash: expiresAt},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := runtimeCache.SetRuntime(context.Background(), cache.PromptSessionLimitRuntimeNamespace, subject, payload, time.Hour); err != nil {
		t.Fatal(err)
	}

	compaction := promptSessionLimitVerifiedRootUserContext(promptSessionTestFingerprint("legacy-compaction-leaf"), root)
	policy := compaction.MustGet(newAPIPolicyMetaContextKey).(verifiedNewAPIPolicyContext)
	policy.Meta.RootSessionRelation = newAPIPolicyRootSessionRelationRelated
	policy.Meta.ThreadSource = "user"
	policy.Meta.RequestKind = "compaction"
	compaction.Set(newAPIPolicyMetaContextKey, policy)
	status, exceeded := handler.checkPromptSessionCreationLimitForSelectedAccountAdmission(
		compaction,
		[]byte(`{"model":"gpt-5.6-sol","input":"CONTEXT CHECKPOINT COMPACTION"}`),
		account,
		"newapi-root-session:"+root+"::api-key:7",
		account.ID(),
	)
	if exceeded || !status.Existing || status.Used != 1 {
		t.Fatalf("legacy compaction reuse: status=%#v exceeded=%v", status, exceeded)
	}
	handler.promptSessionLimitMu.Lock()
	detail := handler.promptSessionWindowDetails[subject][sessionHash]
	handler.promptSessionLimitMu.Unlock()
	if !detail.CreatedAt.IsZero() || detail.Model != "" || detail.ReasoningEffort != "" || detail.ClientUserAgent != "" || detail.PromptPreview != "" || detail.AccountID != account.ID() {
		t.Fatalf("legacy creation metadata was fabricated from compaction: %#v", detail)
	}
}

func TestPromptSessionCreationLimitSkipsVerifiedAmbientBackgroundRequest(t *testing.T) {
	handler := promptSessionLimitVerifiedTestHandler(t)
	cfg := promptfilter.Config{}
	cfg.Advanced.Risk.SessionCreationLimitEnabled = true
	cfg.Advanced.Risk.SessionCreationLimit = 1
	cfg.Advanced.Risk.SessionCreationLimitWindowSeconds = 3600
	c := promptSessionLimitVerifiedRootUserContext(
		promptSessionTestFingerprint("ambient-leaf"),
		promptSessionTestFingerprint("ambient-root"),
	)
	policyContext := c.MustGet(newAPIPolicyMetaContextKey).(verifiedNewAPIPolicyContext)
	policyContext.Meta.RootSessionRelation = newAPIPolicyRootSessionRelationRoot
	policyContext.Meta.ThreadSource = "system"
	policyContext.Meta.RequestedModel = "gpt-5.4"
	policyContext.Meta.SessionAccounting = newAPISessionAccountingBypass
	policyContext.Meta.PassiveFeature = newAPIPassiveFeatureSystemPassive
	c.Set(newAPIPolicyMetaContextKey, policyContext)

	status, exceeded := handler.checkPromptSessionCreationLimit(c, cfg, standaloneAmbientSuggestionsBody("gpt-5.4"))
	if exceeded || status.Used != 0 || status.SessionHash != "" || len(handler.promptSessionLimits) != 0 {
		t.Fatalf("ambient request changed user windows: status=%#v exceeded=%v sessions=%#v", status, exceeded, handler.promptSessionLimits)
	}
}

func TestPromptSessionCreationLimitSkipsStandaloneNativeAmbientBackgroundRequest(t *testing.T) {
	handler := &Handler{}
	cfg := promptfilter.Config{}
	cfg.Advanced.Risk.SessionCreationLimitEnabled = true
	cfg.Advanced.Risk.SessionCreationLimit = 1
	cfg.Advanced.Risk.SessionCreationLimitWindowSeconds = 3600
	c := promptSessionLimitTestContext("")
	c.Request.Header = nativeSessionHeaders(testRootSessionA, testRootSessionA, 0)
	c.Request.Header.Set(codexTurnMetadataHeader, `{"session_id":"`+testRootSessionA+`","thread_id":"`+testRootSessionA+`","window_id":"`+testRootSessionA+`:0","thread_source":"system","request_kind":"turn"}`)
	body := standaloneAmbientSuggestionsBody("gpt-5.4")

	status, exceeded := handler.checkPromptSessionCreationLimit(c, cfg, body)
	if exceeded || status.Used != 0 || status.SessionHash != "" || len(handler.promptSessionLimits) != 0 {
		t.Fatalf("standalone ambient request changed user windows: status=%#v exceeded=%v sessions=%#v", status, exceeded, handler.promptSessionLimits)
	}
}

func TestPromptSessionCreationLimitRejectsSignedWebSocketRootConflict(t *testing.T) {
	handler := promptSessionLimitVerifiedTestHandler(t)
	cfg := promptfilter.Config{}
	cfg.Advanced.Risk.SessionCreationLimitEnabled = true
	cfg.Advanced.Risk.SessionCreationLimit = 3
	cfg.Advanced.Risk.SessionCreationLimitWindowSeconds = 3600

	signedRoot := newAPIRootSessionFingerprint("newapi", "42", testRootSessionA)
	c := promptSessionLimitVerifiedRootUserContext(promptSessionTestFingerprint("ws-leaf"), signedRoot)
	body := []byte(`{"type":"response.create","model":"gpt-5.5","client_metadata":{"session_id":"` + testRootSessionB + `","thread_id":"` + testLeafSessionB + `","x-codex-window-id":"` + testLeafSessionB + `:1","x-codex-parent-thread-id":"` + testRootSessionB + `"}}`)

	status, rejected := handler.checkPromptSessionCreationLimit(c, cfg, body)
	if !rejected || !status.IdentityConflict || status.Used != 0 {
		t.Fatalf("root conflict status=%#v rejected=%v", status, rejected)
	}
	apiErr := promptSessionCreationLimitAPIError(status)
	if apiErr.Code != api.ErrorCode("session_identity_conflict") || !strings.Contains(apiErr.Message, "会话标识发生冲突") {
		t.Fatalf("root conflict error=%#v", apiErr)
	}
	if len(handler.promptSessionLimits) != 0 {
		t.Fatalf("root conflict changed counters: %#v", handler.promptSessionLimits)
	}
}

func TestPromptSessionCreationLimitHonorsAuthoritativeRootStates(t *testing.T) {
	cfg := promptfilter.Config{}
	cfg.Advanced.Risk.SessionCreationLimitEnabled = true
	cfg.Advanced.Risk.SessionCreationLimit = 1
	cfg.Advanced.Risk.SessionCreationLimitWindowSeconds = 3600

	t.Run("conflict rejects without counting leaf", func(t *testing.T) {
		handler := promptSessionLimitVerifiedTestHandler(t)
		c := promptSessionLimitVerifiedUserContext(promptSessionTestFingerprint("conflicting-leaf"))
		policyContext := c.MustGet(newAPIPolicyMetaContextKey).(verifiedNewAPIPolicyContext)
		policyContext.Meta.RootSessionVersion = 1
		policyContext.Meta.RootSessionState = newAPIPolicyRootSessionConflict
		c.Set(newAPIPolicyMetaContextKey, policyContext)

		status, rejected := handler.checkPromptSessionCreationLimit(c, cfg, nil)
		if !rejected || !status.IdentityConflict || status.Used != 0 || len(handler.promptSessionLimits) != 0 {
			t.Fatalf("signed conflict status=%#v rejected=%v sessions=%#v", status, rejected, handler.promptSessionLimits)
		}
	})

	t.Run("unavailable skips leaf accounting", func(t *testing.T) {
		handler := promptSessionLimitVerifiedTestHandler(t)
		c := promptSessionLimitVerifiedUserContext(promptSessionTestFingerprint("unavailable-leaf"))
		policyContext := c.MustGet(newAPIPolicyMetaContextKey).(verifiedNewAPIPolicyContext)
		policyContext.Meta.RootSessionVersion = 1
		policyContext.Meta.RootSessionState = newAPIPolicyRootSessionUnavailable
		c.Set(newAPIPolicyMetaContextKey, policyContext)

		status, rejected := handler.checkPromptSessionCreationLimit(c, cfg, nil)
		if rejected || status.Used != 0 || status.SessionHash != "" || len(handler.promptSessionLimits) != 0 {
			t.Fatalf("signed unavailable status=%#v rejected=%v sessions=%#v", status, rejected, handler.promptSessionLimits)
		}
	})
}

func TestPromptSessionCreationLimitNestedGuardianLocalGraphDoesNotDuplicate(t *testing.T) {
	handler := &Handler{}
	cfg := promptfilter.Config{}
	cfg.Advanced.Risk.SessionCreationLimitEnabled = true
	cfg.Advanced.Risk.SessionCreationLimit = 1
	cfg.Advanced.Risk.SessionCreationLimitWindowSeconds = 3600

	mainRequest := promptSessionLimitTestContext(testRootSessionA)
	for name, values := range nativeSessionHeaders(testRootSessionA, testRootSessionA, 0) {
		mainRequest.Request.Header[name] = append([]string(nil), values...)
	}
	status, exceeded := handler.checkPromptSessionCreationLimit(mainRequest, cfg, nil)
	if exceeded || status.Used != 1 || status.Existing {
		t.Fatalf("main request: status=%#v exceeded=%v", status, exceeded)
	}

	guardianRequest := promptSessionLimitTestContext(testLeafSessionA)
	for name, values := range nativeSessionHeaders(testLeafSessionA, testLeafSessionA, 18) {
		guardianRequest.Request.Header[name] = append([]string(nil), values...)
	}
	guardianRequest.Request.Header.Set("X-Codex-Parent-Thread-Id", testIntermediate)
	guardianRequest.Request.Header.Set(codexTurnMetadataHeader, `{"session_id":"`+testRootSessionA+`","thread_id":"`+testLeafSessionA+`","window_id":"`+testLeafSessionA+`:18","parent_thread_id":"`+testIntermediate+`"}`)
	status, exceeded = handler.checkPromptSessionCreationLimit(guardianRequest, cfg, nil)
	if exceeded || status.Used != 1 || !status.Existing {
		t.Fatalf("nested Guardian consumed a second local window: status=%#v exceeded=%v", status, exceeded)
	}
}

func TestPromptSessionCreationLimitDifferentRootsCountSeparately(t *testing.T) {
	handler := promptSessionLimitVerifiedTestHandler(t)
	cfg := promptfilter.Config{}
	cfg.Advanced.Risk.SessionCreationLimitEnabled = true
	cfg.Advanced.Risk.SessionCreationLimit = 1
	cfg.Advanced.Risk.SessionCreationLimitWindowSeconds = 3600

	first := promptSessionLimitVerifiedRootUserContext(
		promptSessionTestFingerprint("first-leaf"),
		promptSessionTestFingerprint("first-root"),
	)
	status, exceeded := handler.checkPromptSessionCreationLimit(first, cfg, nil)
	if exceeded || status.Used != 1 {
		t.Fatalf("first root: status=%#v exceeded=%v", status, exceeded)
	}

	second := promptSessionLimitVerifiedRootUserContext(
		promptSessionTestFingerprint("second-leaf"),
		promptSessionTestFingerprint("second-root"),
	)
	status, exceeded = handler.checkPromptSessionCreationLimit(second, cfg, nil)
	if !exceeded || status.Used != 1 || status.Existing {
		t.Fatalf("different root was not counted separately: status=%#v exceeded=%v", status, exceeded)
	}
}

func TestPromptSessionCreationLimitConflictingGraphFallsBackToExactLeaf(t *testing.T) {
	handler := &Handler{}
	cfg := promptfilter.Config{}
	cfg.Advanced.Risk.SessionCreationLimitEnabled = true
	cfg.Advanced.Risk.SessionCreationLimit = 1
	cfg.Advanced.Risk.SessionCreationLimitWindowSeconds = 3600

	request := func(leaf, claimedRoot string) *gin.Context {
		c := promptSessionLimitTestContext(leaf)
		c.Request.Header.Set("Thread-Id", leaf)
		c.Request.Header.Set("X-Client-Request-Id", leaf)
		c.Request.Header.Set("X-Codex-Window-Id", leaf+":0")
		c.Request.Header.Set(codexTurnMetadataHeader, `{"session_id":"`+claimedRoot+`","thread_id":"`+leaf+`","window_id":"`+leaf+`:0"}`)
		return c
	}

	first := request(testLeafSessionA, testRootSessionA)
	status, exceeded := handler.checkPromptSessionCreationLimit(first, cfg, nil)
	if exceeded || status.Used != 1 {
		t.Fatalf("first conflicting leaf: status=%#v exceeded=%v", status, exceeded)
	}

	second := request(testLeafSessionB, testRootSessionB)
	status, exceeded = handler.checkPromptSessionCreationLimit(second, cfg, nil)
	if !exceeded || status.Used != 1 || status.Existing {
		t.Fatalf("conflicting graph bypassed exact-leaf limit: status=%#v exceeded=%v", status, exceeded)
	}
}

func TestPromptSessionCreationLimitDifferentTTLsDoNotInterfere(t *testing.T) {
	handler := &Handler{}
	longWindow := promptfilter.Config{}
	longWindow.Advanced.Risk.SessionCreationLimitEnabled = true
	longWindow.Advanced.Risk.SessionCreationLimit = 2
	longWindow.Advanced.Risk.SessionCreationLimitWindowSeconds = 3600
	shortWindow := longWindow
	shortWindow.Advanced.Risk.SessionCreationLimitWindowSeconds = 1

	longSession := promptSessionLimitTestContext("long-lived-session")
	status, exceeded := handler.checkPromptSessionCreationLimit(longSession, longWindow, nil)
	if exceeded || status.Used != 1 {
		t.Fatalf("long session: status=%#v exceeded=%v", status, exceeded)
	}

	// Advance only the stored record enough that the former implementation,
	// which interpreted every timestamp using the current request's TTL, would
	// incorrectly remove it during a one-second user's global cleanup. With an
	// expiry timestamp the record still has almost an hour remaining.
	handler.promptSessionLimitMu.Lock()
	for key, expiresAt := range handler.promptSessionLimits["api-key:7"] {
		handler.promptSessionLimits["api-key:7"][key] = expiresAt.Add(-2 * time.Second)
	}
	handler.promptSessionLastCleanup = time.Time{}
	handler.promptSessionLimitMu.Unlock()

	shortSession := promptSessionLimitTestContext("short-lived-session")
	shortSession.Set(contextAPIKeyID, int64(8))
	if status, exceeded = handler.checkPromptSessionCreationLimit(shortSession, shortWindow, nil); exceeded || status.Used != 1 {
		t.Fatalf("short session: status=%#v exceeded=%v", status, exceeded)
	}

	repeatLong := promptSessionLimitTestContext("long-lived-session")
	status, exceeded = handler.checkPromptSessionCreationLimit(repeatLong, longWindow, nil)
	if exceeded || !status.Existing || status.Used != 1 {
		t.Fatalf("short TTL removed long session: status=%#v exceeded=%v", status, exceeded)
	}
}

func TestPromptSessionCreationLimitConcurrentAdmissionIsSafe(t *testing.T) {
	const (
		limit    = 8
		attempts = 64
	)
	handler := &Handler{}
	cfg := promptfilter.Config{}
	cfg.Advanced.Risk.SessionCreationLimitEnabled = true
	cfg.Advanced.Risk.SessionCreationLimit = limit
	cfg.Advanced.Risk.SessionCreationLimitWindowSeconds = 3600

	var admitted atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			c := promptSessionLimitTestContext(fmt.Sprintf("concurrent-session-%d", index))
			if _, exceeded := handler.checkPromptSessionCreationLimit(c, cfg, nil); !exceeded {
				admitted.Add(1)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if got := admitted.Load(); got != limit {
		t.Fatalf("admitted = %d, want %d", got, limit)
	}
	handler.promptSessionLimitMu.Lock()
	used := len(handler.promptSessionLimits["api-key:7"])
	handler.promptSessionLimitMu.Unlock()
	if used != limit {
		t.Fatalf("stored sessions = %d, want %d", used, limit)
	}
}

func TestPromptSessionCreationLimitSameRootConcurrentAdmissionUsesOneSlot(t *testing.T) {
	const attempts = 64
	handler := &Handler{}
	cfg := promptfilter.Config{}
	cfg.Advanced.Risk.SessionCreationLimitEnabled = true
	cfg.Advanced.Risk.SessionCreationLimit = 1
	cfg.Advanced.Risk.SessionCreationLimitWindowSeconds = 3600

	var exceededCount atomic.Int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < attempts; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, exceeded := handler.checkPromptSessionCreationLimit(promptSessionLimitTestContext("same-concurrent-root"), cfg, nil); exceeded {
				exceededCount.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()

	if got := exceededCount.Load(); got != 0 {
		t.Fatalf("same root exceeded %d times, want 0", got)
	}
	handler.promptSessionLimitMu.Lock()
	used := len(handler.promptSessionLimits["api-key:7"])
	handler.promptSessionLimitMu.Unlock()
	if used != 1 {
		t.Fatalf("stored same-root sessions = %d, want 1", used)
	}
}

func TestPromptSessionCreationLimitExistingSessionDoesNotRefreshExpiry(t *testing.T) {
	handler := &Handler{}
	cfg := promptfilter.Config{}
	cfg.Advanced.Risk.SessionCreationLimitEnabled = true
	cfg.Advanced.Risk.SessionCreationLimit = 1
	cfg.Advanced.Risk.SessionCreationLimitWindowSeconds = 3600

	c := promptSessionLimitTestContext("fixed-expiry-session")
	status, exceeded := handler.checkPromptSessionCreationLimit(c, cfg, nil)
	if exceeded || status.Used != 1 {
		t.Fatalf("first request: status=%#v exceeded=%v", status, exceeded)
	}
	handler.promptSessionLimitMu.Lock()
	before := handler.promptSessionLimits[status.Subject][status.SessionHash]
	handler.promptSessionLimitMu.Unlock()

	status, exceeded = handler.checkPromptSessionCreationLimit(promptSessionLimitTestContext("fixed-expiry-session"), cfg, nil)
	if exceeded || !status.Existing {
		t.Fatalf("repeat request: status=%#v exceeded=%v", status, exceeded)
	}
	handler.promptSessionLimitMu.Lock()
	after := handler.promptSessionLimits[status.Subject][status.SessionHash]
	handler.promptSessionLimitMu.Unlock()
	if !after.Equal(before) {
		t.Fatalf("existing session expiry refreshed from %v to %v", before, after)
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
	handler := promptSessionLimitOverrideTestHandler(t, database.PromptSessionLimitOverride{
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
	handler := promptSessionLimitOverrideTestHandler(t, database.PromptSessionLimitOverride{
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
	t.Cleanup(store.Stop)
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
	t.Cleanup(store.Stop)
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

func TestPromptSessionCreationLimitStoresCurrentWindowDetailWithoutRefreshingExpiry(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1})
	t.Cleanup(store.Stop)
	cfg := promptfilter.Config{MaxTextLength: promptfilter.DefaultMaxTextLength}
	cfg.Advanced.Risk.SessionCreationLimitEnabled = true
	cfg.Advanced.Risk.SessionCreationLimit = 2
	cfg.Advanced.Risk.SessionCreationLimitWindowSeconds = 3600
	store.SetPromptFilterConfig(cfg)
	runtimeCache := cache.NewMemory(1)
	defer runtimeCache.Close()
	handler := &Handler{store: store, cache: runtimeCache}
	firstAccount := &auth.Account{DBID: 92, SessionCapacityEnabled: true, SessionCapacityMax: 5, SessionCapacityIdleTTLSeconds: 3600}
	secondAccount := &auth.Account{DBID: 93, SessionCapacityEnabled: true, SessionCapacityMax: 5, SessionCapacityIdleTTLSeconds: 3600}
	body := []byte(`{"model":"gpt-5.6-sol","reasoning":{"effort":"high"},"input":[{"role":"user","content":[{"type":"input_text","text":"hello active window"}]}]}`)
	firstContext := promptSessionLimitTestContext("window-detail")
	firstContext.Request.Header.Set("User-Agent", "codex_cli_rs/0.128.0")

	status, exceeded := handler.checkPromptSessionCreationLimitForSelectedAccount(firstContext, body, firstAccount)
	if exceeded || status.Used != 1 {
		t.Fatalf("first request: status=%#v exceeded=%v", status, exceeded)
	}
	handler.promptSessionLimitMu.Lock()
	beforeExpiry := handler.promptSessionLimits[status.Subject][status.SessionHash]
	before := handler.promptSessionWindowDetails[status.Subject][status.SessionHash]
	handler.promptSessionLimitMu.Unlock()
	if before.CreatedAt.IsZero() || !before.ExpiresAt.Equal(beforeExpiry) || before.AccountID != 92 || before.Model != "gpt-5.6-sol" || before.ReasoningEffort != "high" || before.ClientUserAgent != "codex_cli_rs/0.128.0" || !strings.Contains(before.PromptPreview, "hello active window") {
		t.Fatalf("first detail = %#v expiry=%v", before, beforeExpiry)
	}

	repeatBody := []byte(`{"model":"gpt-5.4","reasoning":{"effort":"low"},"input":"later prompt must not replace the creation prompt"}`)
	repeatContext := promptSessionLimitTestContext("window-detail")
	repeatContext.Request.Header.Set("User-Agent", "later-client/9.9")
	status, exceeded = handler.checkPromptSessionCreationLimitForSelectedAccount(repeatContext, repeatBody, secondAccount)
	if exceeded || !status.Existing {
		t.Fatalf("repeat request: status=%#v exceeded=%v", status, exceeded)
	}
	handler.promptSessionLimitMu.Lock()
	afterExpiry := handler.promptSessionLimits[status.Subject][status.SessionHash]
	after := handler.promptSessionWindowDetails[status.Subject][status.SessionHash]
	handler.promptSessionLimitMu.Unlock()
	if !afterExpiry.Equal(beforeExpiry) || !after.CreatedAt.Equal(before.CreatedAt) {
		t.Fatalf("existing window timing changed: before=%#v after=%#v", before, after)
	}
	if after.AccountID != 93 || after.Model != before.Model || after.ReasoningEffort != before.ReasoningEffort || after.ClientUserAgent != before.ClientUserAgent || after.PromptPreview != before.PromptPreview {
		t.Fatalf("existing window detail = %#v, want account update with creation metadata preserved", after)
	}
}

func TestPromptSessionCreationLimitIgnoresInternalReviewRequest(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1})
	t.Cleanup(store.Stop)
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
