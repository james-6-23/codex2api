package proxy

import (
	"fmt"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestPopulateAccountSessionObservationDeduplicatesConversation(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	c.Request.Header.Set("Session-Id", "stable-session")
	handler := &Handler{}
	first := &database.UsageLogInput{AccountID: 9}
	handler.populateAccountSessionObservation(c, first)
	if !first.RecordSessionObservation || first.SessionHash == "" {
		t.Fatalf("first session observation was not recorded: %+v", first)
	}
	second := &database.UsageLogInput{AccountID: 9}
	handler.populateAccountSessionObservation(c, second)
	if second.RecordSessionObservation || second.SessionHash != first.SessionHash {
		t.Fatalf("repeat conversation was not deduplicated: first=%+v second=%+v", first, second)
	}
}

func TestPopulateAccountSessionObservationRefreshesLastSeen(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("POST", "/v1/responses", nil)
	c.Request.Header.Set("Session-Id", "stable-session-refresh")
	handler := &Handler{}
	first := &database.UsageLogInput{AccountID: 9}
	handler.populateAccountSessionObservation(c, first)
	key := "9:" + first.SessionHash
	handler.accountSessionObservations[key] = accountSessionObservationCacheEntry{
		RecordedAt: time.Now().Add(-accountSessionObservationRefreshInterval - time.Second),
	}
	second := &database.UsageLogInput{AccountID: 9}
	handler.populateAccountSessionObservation(c, second)
	if !second.RecordSessionObservation || second.ObservedAt.IsZero() {
		t.Fatalf("stale observation was not refreshed: %+v", second)
	}
}

func TestPopulateAccountSessionObservationUsesRootButKeepsLeafForAudit(t *testing.T) {
	handler := promptSessionLimitVerifiedTestHandler()
	root := promptSessionTestFingerprint("account-visible-root")
	firstContext := promptSessionLimitVerifiedRootUserContext(promptSessionTestFingerprint("account-main-leaf"), root)
	policyContext := firstContext.MustGet(newAPIPolicyMetaContextKey).(verifiedNewAPIPolicyContext)

	audit := handler.capturePromptFilterAuditContext(firstContext)
	if audit.SessionHash == audit.RootSessionHash || audit.SessionHash != hashRiskIdentity(policyContext.Meta.SessionFingerprint) || audit.RootSessionHash != hashRiskIdentity(root) {
		t.Fatalf("leaf/root audit identities were not separated: %+v", audit)
	}

	first := &database.UsageLogInput{AccountID: 9}
	handler.populateAccountSessionObservation(firstContext, first)
	if !first.RecordSessionObservation || first.SessionHash != audit.RootSessionHash {
		t.Fatalf("first root observation was not recorded: %+v", first)
	}

	guardianContext := promptSessionLimitVerifiedRootUserContext(promptSessionTestFingerprint("account-guardian-leaf"), root)
	guardian := &database.UsageLogInput{AccountID: 9}
	handler.populateAccountSessionObservation(guardianContext, guardian)
	if guardian.RecordSessionObservation || guardian.SessionHash != first.SessionHash {
		t.Fatalf("same-root Guardian was counted as another account window: first=%+v guardian=%+v", first, guardian)
	}
}

func TestPopulateAccountSessionObservationSkipsVerifiedAmbientBackgroundRequest(t *testing.T) {
	handler := promptSessionLimitVerifiedTestHandler()
	c := promptSessionLimitVerifiedRootUserContext(
		promptSessionTestFingerprint("ambient-leaf-observation"),
		promptSessionTestFingerprint("ambient-root-observation"),
	)
	policyContext := c.MustGet(newAPIPolicyMetaContextKey).(verifiedNewAPIPolicyContext)
	policyContext.Meta.RootSessionRelation = newAPIPolicyRootSessionRelationRoot
	policyContext.Meta.ThreadSource = "system"
	policyContext.Meta.RequestedModel = "gpt-5.4"
	policyContext.Meta.SessionAccounting = newAPISessionAccountingBypass
	policyContext.Meta.PassiveFeature = newAPIPassiveFeatureSystemPassive
	c.Set(newAPIPolicyMetaContextKey, policyContext)
	body := standaloneAmbientSuggestionsBody("gpt-5.4")
	setRawRequestBody(c, body)
	if identity := handler.resolveRequestSessionIdentityForContext(c, body); !identity.bypassWindowAccounting {
		t.Fatalf("verified ambient request was not body-validated: %+v", identity)
	}
	if audit := handler.capturePromptFilterAuditContext(c); audit.SessionHash == "" || audit.RootSessionHash == "" {
		t.Fatalf("ambient request lost normal audit identity: %+v", audit)
	}

	input := &database.UsageLogInput{AccountID: 9}
	handler.populateAccountSessionObservation(c, input)
	if input.RecordSessionObservation || input.SessionHash != "" {
		t.Fatalf("ambient request created an operational account window: %+v", input)
	}
	if input.NewAPIUserID != "42" || input.NewAPIPlatform != "newapi" {
		t.Fatalf("ambient usage log lost verified NewAPI attribution: %+v", input)
	}
}

func TestPopulateAccountSessionObservationSkipsStandaloneNativeAmbientBackgroundRequest(t *testing.T) {
	handler := &Handler{}
	c := promptSessionLimitTestContext("")
	c.Request.Header = nativeSessionHeaders(testRootSessionA, testRootSessionA, 0)
	c.Request.Header.Set(codexTurnMetadataHeader, `{"session_id":"`+testRootSessionA+`","thread_id":"`+testRootSessionA+`","window_id":"`+testRootSessionA+`:0","thread_source":"system","request_kind":"turn"}`)
	body := standaloneAmbientSuggestionsBody("gpt-5.4")
	setRawRequestBody(c, body)
	identity := handler.resolveRequestSessionIdentityForContext(c, body)
	if !identity.bypassWindowAccounting {
		t.Fatalf("standalone ambient request was not classified: %+v", identity)
	}

	input := &database.UsageLogInput{AccountID: 9}
	handler.populateAccountSessionObservation(c, input)
	if input.RecordSessionObservation || input.SessionHash != "" {
		t.Fatalf("standalone ambient request created an operational account window: %+v", input)
	}
}

func TestPopulateAccountSessionObservationUsesCurrentWebSocketFrameRoot(t *testing.T) {
	handler := &Handler{}
	frame := func(leaf string, sequence int) *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest("GET", "/v1/responses", nil)
		c.Request.Header.Set("Connection", "Upgrade")
		c.Request.Header.Set("Upgrade", "websocket")
		setRawRequestBody(c, []byte(fmt.Sprintf(`{"type":"response.create","model":"gpt-5.5","client_metadata":{"session_id":%q,"thread_id":%q,"x-codex-window-id":%q,"x-codex-parent-thread-id":%q}}`, testRootSessionA, leaf, fmt.Sprintf("%s:%d", leaf, sequence), testRootSessionA)))
		return c
	}

	main := &database.UsageLogInput{AccountID: 9}
	handler.populateAccountSessionObservation(frame(testRootSessionA, 0), main)
	if !main.RecordSessionObservation || main.SessionHash == "" {
		t.Fatalf("direct WS main root was not recorded: %+v", main)
	}

	guardian := &database.UsageLogInput{AccountID: 9}
	handler.populateAccountSessionObservation(frame(testLeafSessionA, 1), guardian)
	if guardian.RecordSessionObservation || guardian.SessionHash != main.SessionHash {
		t.Fatalf("direct WS Guardian was not grouped with its root: main=%+v guardian=%+v", main, guardian)
	}
}

func TestPopulateAccountSessionObservationDoesNotLeakRootAcrossWebSocketFrames(t *testing.T) {
	handler := &Handler{}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest("GET", "/v1/responses", nil)
	c.Request.Header.Set("Connection", "Upgrade")
	c.Request.Header.Set("Upgrade", "websocket")
	setFrame := func(root, leaf string, sequence int) {
		setRawRequestBody(c, []byte(fmt.Sprintf(`{"type":"response.create","model":"gpt-5.5","client_metadata":{"session_id":%q,"thread_id":%q,"x-codex-window-id":%q,"x-codex-parent-thread-id":%q}}`, root, leaf, fmt.Sprintf("%s:%d", leaf, sequence), root)))
	}

	setFrame(testRootSessionA, testRootSessionA, 0)
	first := &database.UsageLogInput{AccountID: 9}
	handler.populateAccountSessionObservation(c, first)
	if !first.RecordSessionObservation || first.SessionHash == "" {
		t.Fatalf("first WS root was not recorded: %+v", first)
	}

	setFrame(testRootSessionB, testRootSessionB, 0)
	second := &database.UsageLogInput{AccountID: 9}
	handler.populateAccountSessionObservation(c, second)
	if !second.RecordSessionObservation || second.SessionHash == "" || second.SessionHash == first.SessionHash {
		t.Fatalf("second WS frame inherited the first root: first=%+v second=%+v", first, second)
	}

	setFrame(testRootSessionA, testLeafSessionA, 1)
	guardian := &database.UsageLogInput{AccountID: 9}
	handler.populateAccountSessionObservation(c, guardian)
	if guardian.RecordSessionObservation || guardian.SessionHash != first.SessionHash {
		t.Fatalf("third WS frame did not return to its own root: first=%+v guardian=%+v", first, guardian)
	}
}

func TestPopulateAccountSessionObservationWaitsForSignedWebSocketRoot(t *testing.T) {
	handler := promptSessionLimitVerifiedTestHandler()
	c := promptSessionLimitVerifiedUserContext(promptSessionTestFingerprint("ws-handshake-leaf"))
	policyContext := c.MustGet(newAPIPolicyMetaContextKey).(verifiedNewAPIPolicyContext)
	policyContext.Meta.RootSessionVersion = 1
	policyContext.Meta.RootSessionState = newAPIPolicyRootSessionUnavailable
	c.Set(newAPIPolicyMetaContextKey, policyContext)
	c.Request.Method = "GET"
	c.Request.Header.Set("Connection", "Upgrade")
	c.Request.Header.Set("Upgrade", "websocket")
	setRawRequestBody(c, []byte(`{"type":"response.create","model":"gpt-5.5","input":"metadata pending"}`))

	input := &database.UsageLogInput{AccountID: 9}
	handler.populateAccountSessionObservation(c, input)
	if input.RecordSessionObservation || input.SessionHash != "" {
		t.Fatalf("metadata-free signed WS frame consumed the leaf as an account window: %+v", input)
	}
}

func TestPopulateAccountSessionObservationDoesNotUseLeafForAuthoritativeUnavailableHTTP(t *testing.T) {
	handler := promptSessionLimitVerifiedTestHandler()
	c := promptSessionLimitVerifiedUserContext(promptSessionTestFingerprint("http-unavailable-leaf"))
	policyContext := c.MustGet(newAPIPolicyMetaContextKey).(verifiedNewAPIPolicyContext)
	policyContext.Meta.RootSessionVersion = 1
	policyContext.Meta.RootSessionState = newAPIPolicyRootSessionUnavailable
	c.Set(newAPIPolicyMetaContextKey, policyContext)

	audit := handler.capturePromptFilterAuditContext(c)
	if audit.SessionHash == "" || audit.RootSessionHash != "" {
		t.Fatalf("authoritative unavailable identity mixed leaf into operational root: %+v", audit)
	}
	input := &database.UsageLogInput{AccountID: 9}
	handler.populateAccountSessionObservation(c, input)
	if input.RecordSessionObservation || input.SessionHash != "" {
		t.Fatalf("authoritative unavailable HTTP request recorded a leaf window: %+v", input)
	}
}

func TestPopulateAccountSessionObservationUsesFrameRootWithLegacySignedMetadata(t *testing.T) {
	handler := promptSessionLimitVerifiedTestHandler()
	c := promptSessionLimitVerifiedUserContext(promptSessionTestFingerprint("legacy-newapi-leaf"))
	c.Request.Header.Set("Connection", "Upgrade")
	c.Request.Header.Set("Upgrade", "websocket")
	setRawRequestBody(c, []byte(`{"type":"response.create","model":"gpt-5.5","client_metadata":{"session_id":"`+testRootSessionA+`","thread_id":"`+testLeafSessionA+`","x-codex-window-id":"`+testLeafSessionA+`:2","x-codex-parent-thread-id":"`+testRootSessionA+`"}}`))

	audit := handler.capturePromptFilterAuditContext(c)
	expectedRoot := hashRiskIdentity(newAPIRootSessionFingerprint("newapi", "42", testRootSessionA))
	if audit.SessionHash != hashRiskIdentity(promptSessionTestFingerprint("legacy-newapi-leaf")) || audit.RootSessionHash != expectedRoot {
		t.Fatalf("legacy signed WS did not keep leaf audit and root operation identities separate: %+v", audit)
	}
}
