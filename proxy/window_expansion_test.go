package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"net/http/httptest"
)

func windowExpansionTestContext(test *testing.T, path string, body []byte, meta newAPIPolicyMeta) (*gin.Context, *httptest.ResponseRecorder) {
	request, response := signedRootlessPassiveModelContext(test, http.MethodPost, path, body, meta)
	setSignedNewAPIRequestHeaders(test, request.Request, body, uuid.NewString(), newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}, "test-platform", "integration-secret", promptSessionTestFingerprint(test.Name()))
	meta.PlatformID, meta.Profile, meta.Mode = "test-platform", "balanced", "enforce"
	meta.SessionFingerprint = promptSessionTestFingerprint(test.Name())
	meta.Provider, meta.Protocol = "openai", "responses"
	addSignedNewAPIPolicyMeta(test, request, meta, true)
	return request, response
}

func TestWindowExpansionQuoteAdmissionAndFixedTariff(test *testing.T) {
	handler := newRootlessPassiveModelTestHandler(test)
	db, err := database.New("sqlite", filepath.Join(test.TempDir(), "windows.db"))
	if err != nil {
		test.Fatal("failed: NoError err")
	}
	test.Cleanup(func() { _ = db.Close() })
	handler.db = db
	config := handler.store.GetPromptFilterConfig()
	config.Advanced.Risk.SessionCreationLimitEnabled = true
	config.Advanced.Risk.SessionCreationLimit = 1
	config.Advanced.Risk.SessionCreationLimitWindowSeconds = 3600
	config.Advanced.Risk.SessionCreationCooldown.Mode = "off"
	handler.store.SetPromptFilterConfig(config)
	quote := func(root string, allow bool) (string, int) {
		body, encodeErr := json.Marshal(windowControlRequest{Operation: "quote", AllowExpansion: allow, ExtraLimit: 1, Multiplier: 1.5})
		if encodeErr != nil {
			test.Fatal("failed: NoError encodeErr")
		}
		meta := newAPIPolicyMeta{RootSessionVersion: 1, RootSessionState: newAPIPolicyRootSessionResolved, RootSessionRelation: newAPIPolicyRootSessionRelationRoot, RootSessionFingerprint: promptSessionTestFingerprint(root), ThreadSource: "user", RequestKind: "turn"}
		request, response := windowExpansionTestContext(test, "/v1/session-windows", body, meta)
		handler.ControlNewAPIUserWindows(request)
		var result struct {
			Ticket string `json:"ticket"`
		}
		if json.Unmarshal(response.Body.Bytes(), &result) != nil {
			test.Fatal("failed: NoError json.Unmarshal(response.Body.Bytes(), &result) / response.Body.String()")
		}
		return result.Ticket, response.Code
	}
	ordinary, status := quote("first", false)
	if 200 != status {
		test.Fatal("failed: Equal 200 / status")
	}
	freeGrant, err := decodeWindowGrant("integration-secret", ordinary)
	if err != nil {
		test.Fatal("failed: NoError err")
	}
	if freeGrant.Grant.Expanded {
		test.Fatal("failed: False freeGrant.Grant.Expanded")
	}
	if 1.0 != freeGrant.Grant.Multiplier {
		test.Fatal("failed: Equal 1.0 / freeGrant.Grant.Multiplier")
	}
	unexpected, status := quote("second", false)
	if 400 != status {
		second, _ := decodeWindowGrant("integration-secret", unexpected)
		test.Fatalf("unexpected quote status=%d first=%+v second=%+v limit=%d", status, freeGrant, second, handler.store.GetPromptFilterConfig().Advanced.Risk.SessionCreationLimit)
	}
	paid, status := quote("second", true)
	if 200 != status {
		test.Fatal("failed: Equal 200 / status")
	}
	paidGrant, err := decodeWindowGrant("integration-secret", paid)
	if err != nil {
		test.Fatal("failed: NoError err")
	}
	if !(paidGrant.Grant.Expanded) {
		test.Fatal("failed: True paidGrant.Grant.Expanded")
	}
	if 1.5 != paidGrant.Grant.Multiplier {
		test.Fatal("failed: Equal 1.5 / paidGrant.Grant.Multiplier")
	}
	_, status = quote("third", true)
	if 400 != status {
		test.Fatal("failed: Equal 400 / status")
	}
	body := []byte(`{"model":"gpt-5.6-sol","input":"hello"}`)
	meta := newAPIPolicyMeta{RootSessionVersion: 1, RootSessionState: newAPIPolicyRootSessionResolved, RootSessionRelation: newAPIPolicyRootSessionRelationRoot, RootSessionFingerprint: paidGrant.Fingerprint, ThreadSource: "user", RequestKind: "turn", WindowGrant: paid}
	request, _ := windowExpansionTestContext(test, "/v1/responses", body, meta)
	handler.primeNewAPIPolicyContext(request, body)
	if handler.validateRequestWindowGrant(request) != nil {
		test.Fatal("failed: NoError handler.validateRequestWindowGrant(request)")
	}
	admission, blocked := handler.checkPromptSessionCreationLimit(request, config, body)
	if blocked {
		test.Fatalf("expanded admission rejected: %+v", admission)
	}
	if paidGrant.Grant.Root != admission.SessionHash {
		test.Fatal("failed: Equal paidGrant.Grant.Root / admission.SessionHash")
	}
	windows := handler.userWindowControlSnapshot(admission.Subject, time.Now())
	if !(windows[admission.SessionHash].Expanded) {
		test.Fatal("failed: True windows[admission.SessionHash].Expanded")
	}
	if 1.5 != windows[admission.SessionHash].Multiplier {
		test.Fatal("failed: Equal 1.5 / windows[admission.SessionHash].Multiplier")
	}
	afterDisable, status := quote("second", false)
	if 200 != status {
		test.Fatal("failed: Equal 200 / status")
	}
	continued, err := decodeWindowGrant("integration-secret", afterDisable)
	if err != nil {
		test.Fatal("failed: NoError err")
	}
	if paidGrant.Grant.ID != continued.Grant.ID {
		test.Fatal("failed: Equal paidGrant.Grant.ID / continued.Grant.ID")
	}
	if 1.5 != continued.Grant.Multiplier {
		test.Fatal("failed: Equal 1.5 / continued.Grant.Multiplier")
	}
	meta.WindowGrant = ""
	missing, _ := windowExpansionTestContext(test, "/v1/responses", body, meta)
	handler.primeNewAPIPolicyContext(missing, body)
	if handler.validateRequestWindowGrant(missing) == nil {
		test.Fatal("failed: Error handler.validateRequestWindowGrant(missing)")
	}
	state, err := db.ReadUserWindowAdmissions(context.Background(), admission.Subject)
	if err != nil {
		test.Fatal("failed: NoError err")
	}
	if !(state.Windows[admission.SessionHash].Confirmed) {
		test.Fatal("failed: True state.Windows[admission.SessionHash].Confirmed")
	}
	config.Advanced.Risk.SessionCreationCooldown.Mode = "enforce"
	meta.RootSessionFingerprint, meta.WindowGrant = freeGrant.Fingerprint, ordinary
	failed, _ := windowExpansionTestContext(test, "/v1/responses", body, meta)
	handler.primeNewAPIPolicyContext(failed, body)
	if err := handler.validateRequestWindowGrant(failed); err != nil {
		test.Fatal(err)
	}
	failedStatus, blocked := handler.checkPromptSessionCreationLimit(failed, config, body)
	if blocked || cooldownReceipt(failed) == nil {
		test.Fatalf("expected provisional root: %+v", failedStatus)
	}
	handler.finishSessionCooldown(failed)
	state, err = db.ReadUserWindowAdmissions(context.Background(), failedStatus.Subject)
	if err != nil {
		test.Fatal(err)
	}
	if state.Windows[failedStatus.SessionHash] == nil || !state.Windows[failedStatus.SessionHash].Confirmed {
		test.Fatal("failed request removed a confirmed billing grant")
	}
}

func TestWindowGrantRejectsModifiedTariffAndScope(test *testing.T) {
	grant := signedWindowGrant{Version: 1, Platform: "test", UserID: "42", Fingerprint: "root", Grant: database.UserWindowGrant{ID: "test", Multiplier: 1.5, Expanded: true, ExtraLimit: 2, ExpiresAt: time.Now().Add(time.Hour)}}
	token, err := encodeWindowGrant("secret", grant)
	if err != nil {
		test.Fatal("failed: NoError err")
	}
	_, err = decodeWindowGrant("other-secret", token)
	if err == nil {
		test.Fatal("failed: Error err")
	}
	_, err = decodeWindowGrant("secret", token+"changed")
	if err == nil {
		test.Fatal("failed: Error err")
	}
}
