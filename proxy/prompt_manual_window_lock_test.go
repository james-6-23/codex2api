package proxy

import (
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestManualWindowLockBlocksRootAndChildrenWithoutAutomaticPunishment(test *testing.T) {
	handler := promptSessionLimitVerifiedTestHandler(test)
	db, err := database.New("sqlite", filepath.Join(test.TempDir(), "manual-lock.db"))
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = db.Close() })
	handler.db = db
	root := promptSessionTestFingerprint("locked-root")
	if _, err := db.LockPromptUserWindow(test.Context(), "newapi", "42", hashRiskIdentity(root), time.Now(), time.Hour); err != nil {
		test.Fatal(err)
	}
	body := []byte(`{"model":"gpt-6-astra","input":"hello"}`)
	for _, scenario := range []struct {
		name, userID, platform, root, source string
		blocked                              bool
	}{
		{"main", "42", "newapi", root, "user", true},
		{"child", "42", "newapi", root, "guardian_review", true},
		{"other-window", "42", "newapi", promptSessionTestFingerprint("other-root"), "user", false},
		{"other-user", "43", "newapi", root, "user", false},
		{"other-platform", "42", "other", root, "user", false},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			request := promptSessionLimitVerifiedRootUserContext(promptSessionTestFingerprint(scenario.name), scenario.root)
			policy := request.MustGet(newAPIPolicyMetaContextKey).(verifiedNewAPIPolicyContext)
			policy.Identity.UserID, policy.Platform = scenario.userID, scenario.platform
			policy.Meta.ThreadSource = scenario.source
			if scenario.source == "guardian_review" {
				policy.Meta.RootSessionRelation = newAPIPolicyRootSessionRelationRelated
			}
			_, policy.BodySHA256 = promptRequestBodyDigest(request, body)
			request.Set(newAPIPolicyMetaContextKey, policy)
			recorder := httptest.NewRecorder()
			output, _ := gin.CreateTestContext(recorder)
			request.Writer = output.Writer
			cfg := handler.promptFilterConfigForRequest(request)
			cfg.Enabled = false
			cfg.Advanced.Enforcement.ConversationLockEnabled = false
			if blocked := handler.rejectLockedPromptConversation(request, cfg, body, body, "/v1/responses", "gpt-6-astra"); blocked != scenario.blocked {
				test.Fatalf("blocked=%v body=%s", blocked, recorder.Body.String())
			}
			if scenario.blocked && (recorder.Code != 400 || gjson.GetBytes(recorder.Body.Bytes(), "error.code").String() != string(promptManualWindowLockedCode)) {
				test.Fatalf("response=%d %s", recorder.Code, recorder.Body.String())
			}
			if recorder.Header().Get("X-Codex2API-Policy-Decision-ID") != "" || recorder.Header().Get("X-Codex2API-Policy-Restriction-Scope") != "" {
				test.Fatal("manual lock emitted automatic punishment metadata")
			}
		})
	}
	if err := db.UnlockPromptUserWindow(test.Context(), "newapi", "42", hashRiskIdentity(root), time.Now()); err != nil {
		test.Fatal(err)
	}
	request := promptSessionLimitVerifiedRootUserContext(root, root)
	policy := request.MustGet(newAPIPolicyMetaContextKey).(verifiedNewAPIPolicyContext)
	_, policy.BodySHA256 = promptRequestBodyDigest(request, nil)
	request.Set(newAPIPolicyMetaContextKey, policy)
	if apiErr := handler.promptManualWindowLockError(request, handler.promptFilterConfigForRequest(request), nil, nil); apiErr != nil {
		test.Fatalf("unlock did not take effect: %+v", apiErr)
	}
}

func TestManualWindowLockUsesWebSocketFrameRootAndRefreshesOnNextFrame(test *testing.T) {
	handler := promptSessionLimitVerifiedTestHandler(test)
	db, err := database.New("sqlite", filepath.Join(test.TempDir(), "ws-lock.db"))
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = db.Close() })
	handler.db = db
	root := newAPIRootSessionFingerprint("newapi", "42", testRootSessionA)
	request := promptSessionLimitVerifiedUserContext(promptSessionTestFingerprint("handshake-leaf"))
	request.Request.Header = nativeSessionHeaders(testRootSessionB, testRootSessionB, 0)
	request.Request.Header.Set("Connection", "Upgrade")
	request.Request.Header.Set("Upgrade", "websocket")
	policy := request.MustGet(newAPIPolicyMetaContextKey).(verifiedNewAPIPolicyContext)
	_, policy.BodySHA256 = promptRequestBodyDigest(request, nil)
	request.Set(newAPIPolicyMetaContextKey, policy)
	frame := []byte(`{"type":"response.create","model":"gpt-6-astra","client_metadata":{"session_id":"` + testRootSessionA + `","thread_id":"` + testRootSessionA + `","x-codex-window-id":"` + testRootSessionA + `:0"}}`)
	if blocked, _ := handler.inspectPromptFilterOpenAIForWebSocket(request, nil, frame, "/v1/responses", "gpt-6-astra", ""); blocked {
		test.Fatal("unlocked frame blocked")
	}
	if _, err := db.LockPromptUserWindow(test.Context(), "newapi", "42", hashRiskIdentity(root), time.Now(), time.Hour); err != nil {
		test.Fatal(err)
	}
	for _, body := range [][]byte{frame, []byte(`{"type":"response.create","model":"gpt-6-astra"}`)} {
		resetPromptRequestSecurityFrame(request)
		if blocked, delegated := handler.inspectPromptFilterOpenAIForWebSocket(request, nil, body, "/v1/responses", "gpt-6-astra", ""); !blocked || delegated {
			test.Fatalf("locked continuation blocked=%v delegated=%v", blocked, delegated)
		}
	}
	if err := db.UnlockPromptUserWindow(test.Context(), "newapi", "42", hashRiskIdentity(root), time.Now()); err != nil {
		test.Fatal(err)
	}
	resetPromptRequestSecurityFrame(request)
	if blocked, _ := handler.inspectPromptFilterOpenAIForWebSocket(request, nil, frame, "/v1/responses", "gpt-6-astra", ""); blocked {
		test.Fatal("manual lock cached after unlock")
	}
}

func TestWindowErrorGenerationCapturedBeforeDispatchAndClearedOnNextSelection(test *testing.T) {
	handler := promptSessionLimitVerifiedTestHandler(test)
	cfg := handler.store.GetPromptFilterConfig()
	cfg.Advanced.Risk.SessionCreationLimitEnabled = true
	cfg.Advanced.Risk.SessionCreationLimit = 5
	cfg.Advanced.Risk.SessionCreationLimitWindowSeconds = 3600
	handler.store.SetPromptFilterConfig(cfg)
	account := &auth.Account{DBID: 9, SessionCapacityEnabled: true, SessionCapacityMax: 5, SessionCapacityIdleTTLSeconds: 3600}
	root := promptSessionTestFingerprint("window-error-root")
	request := promptSessionLimitVerifiedRootUserContext(root, root)
	status, exceeded := handler.checkPromptSessionCreationLimitForSelectedAccount(request, nil, account)
	if exceeded || status.SessionHash == "" {
		test.Fatalf("status=%+v exceeded=%v", status, exceeded)
	}
	first := &database.UsageLogInput{AccountID: 9, StatusCode: 500}
	handler.populateAccountSessionObservation(request, first)
	if first.SessionWindowExpiresAt.IsZero() || first.SessionHash != status.SessionHash {
		test.Fatalf("missing window identity: %+v", first)
	}
	handler.promptSessionLimitMu.Lock()
	handler.promptSessionLimits[status.Subject][status.SessionHash] = time.Now().Add(-time.Second)
	handler.promptSessionLimitMu.Unlock()
	cfg.Advanced.Risk.SessionCreationLimitWindowSeconds = 7200
	handler.store.SetPromptFilterConfig(cfg)
	resetPromptRequestSecurityFrame(request)
	cleared := &database.UsageLogInput{AccountID: 9, StatusCode: 500}
	handler.populateAccountSessionObservation(request, cleared)
	if !cleared.SessionWindowExpiresAt.IsZero() {
		test.Fatalf("previous frame generation leaked before admission: %+v", cleared)
	}
	handler.checkPromptSessionCreationLimitForSelectedAccount(request, nil, account)
	next := &database.UsageLogInput{AccountID: 9, StatusCode: 500}
	handler.populateAccountSessionObservation(request, next)
	if next.SessionWindowExpiresAt.IsZero() || next.SessionWindowExpiresAt.Equal(first.SessionWindowExpiresAt) {
		test.Fatalf("new window reused old generation: old=%+v new=%+v", first, next)
	}
	handler.checkPromptSessionCreationLimitForSelectedAccount(request, nil, nil)
	cleared = &database.UsageLogInput{AccountID: 9, StatusCode: 500}
	handler.populateAccountSessionObservation(request, cleared)
	if !cleared.SessionWindowExpiresAt.IsZero() {
		test.Fatalf("previous window generation leaked: %+v", cleared)
	}
}
