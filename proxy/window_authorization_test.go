package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func newWindowAuthorizationHandler(test *testing.T) *Handler {
	test.Helper()
	handler := newRootlessPassiveModelTestHandler(test)
	db, err := database.New("sqlite", filepath.Join(test.TempDir(), "authorization.db"))
	require.NoError(test, err)
	test.Cleanup(func() { require.NoError(test, db.Close()) })
	handler.db = db
	config := handler.store.GetPromptFilterConfig()
	config.Advanced.Risk.SessionCreationLimitEnabled = true
	config.Advanced.Risk.SessionCreationLimit = 1
	config.Advanced.Risk.SessionCreationLimitWindowSeconds = 3600
	config.Advanced.Risk.SessionCreationCooldown.Mode = "off"
	handler.store.SetPromptFilterConfig(config)
	return handler
}

func quoteWindowAuthorization(test *testing.T, handler *Handler, root, owner, source string) signedWindowGrant {
	test.Helper()
	body, err := json.Marshal(windowControlRequest{Operation: "quote", AllowExpansion: true, ExtraLimit: 2, Multiplier: 1.5, ReservationID: owner})
	require.NoError(test, err)
	meta := newAPIPolicyMeta{RootSessionVersion: 1, RootSessionState: newAPIPolicyRootSessionResolved, RootSessionRelation: newAPIPolicyRootSessionRelationRoot, RootSessionFingerprint: promptSessionTestFingerprint(root), ThreadSource: source, RequestKind: "turn"}
	request, recorder := windowExpansionTestContext(test, "/v1/session-windows", body, meta)
	handler.ControlNewAPIUserWindows(request)
	require.Equal(test, http.StatusOK, recorder.Code, recorder.Body.String())
	var result struct {
		Ticket string `json:"ticket"`
	}
	require.NoError(test, json.Unmarshal(recorder.Body.Bytes(), &result))
	grant, err := decodeWindowGrant("integration-secret", result.Ticket)
	require.NoError(test, err)
	return grant
}

func windowAuthorizationRequest(test *testing.T, handler *Handler, grant signedWindowGrant, source string) *gin.Context {
	test.Helper()
	ticket, err := encodeWindowGrant("integration-secret", grant)
	require.NoError(test, err)
	meta := newAPIPolicyMeta{RootSessionVersion: 1, RootSessionState: newAPIPolicyRootSessionResolved, RootSessionRelation: newAPIPolicyRootSessionRelationRoot, RootSessionFingerprint: grant.Fingerprint, ThreadSource: source, RequestKind: "turn", WindowGrant: ticket}
	if source != "user" {
		meta.RootSessionRelation = newAPIPolicyRootSessionRelationRelated
		meta.PassiveFeature = newAPIPassiveFeatureRelatedInternal
	}
	body := []byte(`{"model":"gpt-5.6-sol","input":"test"}`)
	request, _ := windowExpansionTestContext(test, "/v1/responses", body, meta)
	handler.primeNewAPIPolicyContext(request, body)
	return request
}

func TestWindowAuthorizationNoCapacityAccountConfirmsWithoutConsumingWindow(test *testing.T) {
	handler := newWindowAuthorizationHandler(test)
	grant := quoteWindowAuthorization(test, handler, "main", "first-request", "user")
	request := windowAuthorizationRequest(test, handler, grant, "user")
	require.Nil(test, handler.requestWindowGrantError(request))
	status, blocked := handler.checkPromptSessionCreationLimitForSelectedAccountAdmission(request, nil, &auth.Account{DBID: 71}, "", 0)
	require.False(test, blocked, "%+v", status)
	confirmed, err := decodeWindowGrant("integration-secret", request.Writer.Header().Get(windowGrantResponseHeader))
	require.NoError(test, err)
	require.True(test, confirmed.Grant.Confirmed)
	require.True(test, confirmed.Grant.NoWindow)
	require.Equal(test, 1.0, confirmed.Grant.Multiplier)
	subject := cache.PromptSessionLimitSubject(grant.Platform, grant.UserID)
	require.Empty(test, handler.userWindowControlSnapshot(subject, time.Now()))
	grant.Grant.PendingUntil = time.Now().Add(-time.Second)
	continued := windowAuthorizationRequest(test, handler, grant, "user")
	require.Nil(test, handler.requestWindowGrantError(continued))
	require.True(test, windowGrantForRequest(continued).Grant.Confirmed)
	next := quoteWindowAuthorization(test, handler, "another", "next", "user")
	require.False(test, next.Grant.Expanded, "non-window authorization must not consume an ordinary slot")
	converted := windowAuthorizationRequest(test, handler, confirmed, "user")
	require.Nil(test, handler.requestWindowGrantError(converted))
	status, blocked = handler.checkPromptSessionCreationLimitForSelectedAccountAdmission(converted, nil, &auth.Account{DBID: 71, SessionCapacityEnabled: true, SessionCapacityMax: 10, SessionCapacityIdleTTLSeconds: 60}, "", 0)
	require.True(test, blocked, "enabling account capacity must not steal another request's reserved ordinary slot")
	require.Equal(test, api.ErrorCode("window_billing_refresh_required"), status.AdmissionCode)
	updated := quoteWindowAuthorization(test, handler, "main", "updated", "user")
	require.True(test, updated.Grant.Expanded)
	require.Equal(test, 1.5, updated.Grant.Multiplier)
}

func TestWindowAuthorizationExpiryRequestsRenewalAndStorageErrorIsDistinct(test *testing.T) {
	handler := newWindowAuthorizationHandler(test)
	grant := quoteWindowAuthorization(test, handler, "main", "first", "user")
	grant.Grant.PendingUntil = time.Now().Add(-time.Second)
	subject := cache.PromptSessionLimitSubject(grant.Platform, grant.UserID)
	require.NoError(test, handler.db.UpdateUserWindowAdmissions(context.Background(), subject, func(state *database.UserWindowAdmissionState) error {
		state.Windows[grant.Grant.Root].PendingUntil = grant.Grant.PendingUntil
		return nil
	}))
	request := windowAuthorizationRequest(test, handler, grant, "user")
	apiErr := handler.requestWindowGrantError(request)
	require.NotNil(test, apiErr)
	require.Equal(test, api.ErrorCode("window_billing_refresh_required"), apiErr.Code)
	require.Equal(test, http.StatusBadRequest, api.HTTPStatusCode(apiErr.Code))
	renewed := quoteWindowAuthorization(test, handler, "main", "renewed", "user")
	require.NotEqual(test, grant.Grant.ID, renewed.Grant.ID)
	require.Equal(test, grant.Grant.Multiplier, renewed.Grant.Multiplier)
	require.Nil(test, handler.requestWindowGrantError(windowAuthorizationRequest(test, handler, renewed, "user")))
	canceled := windowAuthorizationRequest(test, handler, grant, "user")
	ctx, cancel := context.WithCancel(canceled.Request.Context())
	cancel()
	canceled.Request = canceled.Request.WithContext(ctx)
	apiErr = handler.requestWindowGrantError(canceled)
	require.Equal(test, api.ErrCodeServiceUnavailable, apiErr.Code)
}

func TestWindowAuthorizationReleaseCannotDeleteAnotherReservationOrConfirmedGrant(test *testing.T) {
	handler := newWindowAuthorizationHandler(test)
	first := quoteWindowAuthorization(test, handler, "main", "owner-one", "user")
	second := quoteWindowAuthorization(test, handler, "main", "owner-two", "user")
	require.Equal(test, first.Grant.ID, second.Grant.ID)
	background := quoteWindowAuthorization(test, handler, "main", "background", "thread_title")
	require.Equal(test, first.Grant.ID, background.Grant.ID)
	subject := cache.PromptSessionLimitSubject(first.Platform, first.UserID)
	state, err := handler.db.ReadUserWindowAdmissions(context.Background(), subject)
	require.NoError(test, err)
	require.Len(test, state.Reservations[first.Grant.Root], 2)
	body, err := json.Marshal(windowControlRequest{Operation: "release", GrantID: first.Grant.ID, ReservationID: first.ReservationID, Multiplier: 1})
	require.NoError(test, err)
	meta := newAPIPolicyMeta{RootSessionVersion: 1, RootSessionState: newAPIPolicyRootSessionResolved, RootSessionRelation: newAPIPolicyRootSessionRelationRoot, RootSessionFingerprint: first.Fingerprint, ThreadSource: "user"}
	release, _ := windowExpansionTestContext(test, "/v1/session-windows", body, meta)
	handler.ControlNewAPIUserWindows(release)
	state, err = handler.db.ReadUserWindowAdmissions(context.Background(), subject)
	require.NoError(test, err)
	require.NotNil(test, state.Windows[first.Grant.Root])
	require.Contains(test, state.Reservations[first.Grant.Root], second.ReservationID)
	request := windowAuthorizationRequest(test, handler, second, "user")
	require.Nil(test, handler.requestWindowGrantError(request))
	require.NoError(test, handler.confirmRequestWindowGrant(request))
	release, _ = windowExpansionTestContext(test, "/v1/session-windows", body, meta)
	handler.ControlNewAPIUserWindows(release)
	state, err = handler.db.ReadUserWindowAdmissions(context.Background(), subject)
	require.NoError(test, err)
	require.True(test, state.Windows[first.Grant.Root].Confirmed)
	require.Empty(test, state.Reservations[first.Grant.Root])
}
