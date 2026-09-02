package admin

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func newAntigravityStateTestHandler(t *testing.T, credentials map[string]any) (*Handler, *database.DB, int64) {
	t.Helper()
	db := newTestAdminDB(t)
	id, err := db.InsertAccountWithUpstream(context.Background(), "antigravity-state", "google", auth.UpstreamAntigravity, credentials, "")
	if err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(db, nil, nil)
	t.Cleanup(store.Stop)
	if err := store.LoadAccountByID(context.Background(), id); err != nil {
		t.Fatalf("load runtime account: %v", err)
	}
	return &Handler{db: db, store: store}, db, id
}

func callAntigravityAdminHandler(t *testing.T, method, target string, id int64, handler func(*gin.Context)) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(id, 10)}}
	c.Request = httptest.NewRequest(method, target, nil)
	handler(c)
	return recorder
}

func TestAntigravityStateIsSanitizedAndReadDoesNotProbe(t *testing.T) {
	permissions := `{"allowed":true,"project_id":"project-1","updated_at":"2026-08-21T00:00:00Z"}`
	quota := `{"models":[{"model_id":"gemini-3.5-flash-extra-low","display_name":"gemini-3.5-flash-extra-low","remaining_fraction":0.5,"remaining_percent":50},{"model_id":"gemini-3.5-flash-low","display_name":"gemini-3.5-flash-low","remaining_fraction":0.5,"remaining_percent":50},{"model_id":"gemini-3-flash-agent","display_name":"gemini-3-flash-agent","remaining_fraction":0.5,"remaining_percent":50}],"model_forwarding_rules":{"gemini-3.5-flash-extra-low":"gemini-pro-agent"},"updated_at":"2026-08-21T00:00:00Z"}`
	handler, _, id := newAntigravityStateTestHandler(t, map[string]any{
		"upstream_type": auth.UpstreamAntigravity, "access_token": "access-secret", "refresh_token": "refresh-secret",
		"antigravity_client_secret": "client-secret", "project_id": "project-1", "account_id": "subject-secret",
		"verified_email": true, "models": []string{"gemini-3.5-flash-extra-low", "gemini-3.5-flash-low", "gemini-3-flash-agent"}, "antigravity_permissions": permissions,
		"antigravity_quota": quota, "antigravity_last_synced_at": "2026-08-21T00:00:00Z",
	})
	var calls atomic.Int32
	handler.antigravityCapabilityProbe = func(context.Context, *auth.Account, string, []byte, bool, string) (*http.Response, error) {
		calls.Add(1)
		return nil, nil
	}
	recorder := callAntigravityAdminHandler(t, http.MethodGet, "/state", id, handler.GetAntigravityAccountState)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatal("ordinary state read ran a capability probe")
	}
	for _, secret := range []string{"access-secret", "refresh-secret", "client-secret", "subject-secret"} {
		if strings.Contains(recorder.Body.String(), secret) {
			t.Fatalf("state leaked %q: %s", secret, recorder.Body.String())
		}
	}
	var state antigravityAccountState
	if err := json.Unmarshal(recorder.Body.Bytes(), &state); err != nil {
		t.Fatal(err)
	}
	if state.CredentialKind != auth.AntigravityAuthKindOAuth || state.Identity.Status != "verified" || !state.Catalog.Verified || state.Permissions == nil || state.Quota == nil {
		t.Fatalf("sanitized state = %+v", state)
	}
	if !reflect.DeepEqual(state.Catalog.Models, []string{"gemini-3.5-flash-low", "gemini-3.5-flash-medium", "gemini-3.5-flash-high"}) ||
		len(state.Quota.Models) != 3 || state.Quota.Models[0].ModelID != "gemini-3.5-flash-low" ||
		state.Quota.ModelForwardingRules != nil || strings.Contains(recorder.Body.String(), "gemini-3.5-flash-extra-low") || strings.Contains(recorder.Body.String(), "gemini-pro-agent") {
		t.Fatalf("state exposed raw model facts: %s", recorder.Body.String())
	}
}

func TestAntigravityAPIKeySyncIsLocalAndUnverified(t *testing.T) {
	handler, db, id := newAntigravityStateTestHandler(t, map[string]any{
		"upstream_type": auth.UpstreamAntigravity, "api_key": "api-secret", "models": []string{"gemini-3.5-flash-extra-low"},
	})
	var remoteCalls atomic.Int32
	handler.antigravitySyncAccount = func(context.Context, int64) antigravityRefreshItem {
		remoteCalls.Add(1)
		return antigravityRefreshItem{OK: true}
	}
	recorder := callAntigravityAdminHandler(t, http.MethodPost, "/sync", id, handler.SyncAntigravityAccountState)
	if recorder.Code != http.StatusOK || remoteCalls.Load() != 0 {
		t.Fatalf("status=%d remoteCalls=%d body=%s", recorder.Code, remoteCalls.Load(), recorder.Body.String())
	}
	var response antigravityStateSyncResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Remote || response.Verified || response.CatalogSource != "declared" || response.State.Catalog.Verified {
		t.Fatalf("API-key sync response = %+v", response)
	}
	row, err := db.GetAccountByID(context.Background(), id)
	if err != nil || row.GetCredential("antigravity_catalog_source") != "declared" || row.GetCredentialBool("antigravity_catalog_verified") {
		t.Fatalf("persisted row=%+v err=%v", row, err)
	}
}

func TestAntigravityOAuthSyncUsesControlPlaneRefresh(t *testing.T) {
	handler, _, id := newAntigravityStateTestHandler(t, map[string]any{
		"upstream_type": auth.UpstreamAntigravity, "access_token": "access", "project_id": "project", "models": []string{"gemini-3.5-flash-extra-low"},
		"antigravity_last_synced_at": "2026-08-21T00:00:00Z",
	})
	var called atomic.Int32
	handler.antigravitySyncAccount = func(_ context.Context, gotID int64) antigravityRefreshItem {
		if gotID != id {
			t.Errorf("sync id=%d want=%d", gotID, id)
		}
		called.Add(1)
		return antigravityRefreshItem{ID: id, OK: true}
	}
	recorder := callAntigravityAdminHandler(t, http.MethodPost, "/sync", id, handler.SyncAntigravityAccountState)
	if recorder.Code != http.StatusOK || called.Load() != 1 {
		t.Fatalf("status=%d called=%d body=%s", recorder.Code, called.Load(), recorder.Body.String())
	}
	var response antigravityStateSyncResponse
	_ = json.Unmarshal(recorder.Body.Bytes(), &response)
	if !response.Remote || !response.Verified || response.CatalogSource != "google_control_plane" {
		t.Fatalf("OAuth sync response = %+v", response)
	}
}

func TestAntigravityCapabilityProbePersistsSuccessfulInteractionsObservation(t *testing.T) {
	handler, db, id := newAntigravityStateTestHandler(t, map[string]any{
		"upstream_type": auth.UpstreamAntigravity, "api_key": "api-secret", "models": []string{"gemini-3.5-flash-extra-low", "gemini-3.5-flash-low", "gemini-3-flash-agent"},
	})
	handler.antigravityCapabilityProbe = func(_ context.Context, account *auth.Account, model string, body []byte, stream bool, _ string) (*http.Response, error) {
		if account.AntigravityAPIKey() != "api-secret" || model != "gemini-3.5-flash-low" || stream || !strings.Contains(string(body), `"max_output_tokens":1`) {
			t.Fatalf("probe args account=%+v model=%q body=%s stream=%v", account, model, body, stream)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json; charset=utf-8"}}, Body: io.NopCloser(strings.NewReader(`{"id":"interaction-1","outputs":[]}`))}, nil
	}
	recorder := callAntigravityAdminHandler(t, http.MethodPost, "/probe", id, handler.ProbeAntigravityAccountCapabilities)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Result antigravityCapabilityObservation `json:"result"`
		State  antigravityAccountState          `json:"state"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.Result.Verified || payload.Result.Protocol != "interactions" || payload.Result.Status != "ok" || len(payload.State.Capabilities) != 1 {
		t.Fatalf("probe payload = %+v", payload)
	}
	row, err := db.GetAccountByID(context.Background(), id)
	if err != nil || !strings.Contains(row.GetCredential("antigravity_capabilities"), `"verified":true`) {
		t.Fatalf("persisted observation=%q err=%v", row.GetCredential("antigravity_capabilities"), err)
	}
}

func TestAntigravityCapabilityProbeRejectsFailedTwoHundredEnvelope(t *testing.T) {
	handler, _, id := newAntigravityStateTestHandler(t, map[string]any{
		"upstream_type": auth.UpstreamAntigravity, "api_key": "api-secret", "models": []string{"gemini-3.5-flash-extra-low", "gemini-3.5-flash-low", "gemini-3-flash-agent"},
	})
	handler.antigravityCapabilityProbe = func(context.Context, *auth.Account, string, []byte, bool, string) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"status":"failed","error":{"message":"not compatible"}}`))}, nil
	}
	recorder := callAntigravityAdminHandler(t, http.MethodPost, "/probe", id, handler.ProbeAntigravityAccountCapabilities)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"verified":false`) || !strings.Contains(recorder.Body.String(), `"status":"invalid_response"`) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestAntigravityCapabilityProbeErrorPersistsUnverifiedResult(t *testing.T) {
	handler, _, id := newAntigravityStateTestHandler(t, map[string]any{
		"upstream_type": auth.UpstreamAntigravity, "api_key": "api-secret",
	})
	handler.antigravityCapabilityProbe = func(context.Context, *auth.Account, string, []byte, bool, string) (*http.Response, error) {
		return nil, context.DeadlineExceeded
	}
	recorder := callAntigravityAdminHandler(t, http.MethodPost, "/probe", id, handler.ProbeAntigravityAccountCapabilities)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"verified":false`) || !strings.Contains(recorder.Body.String(), `"warning"`) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestAntigravityStateWrongChannelNotFoundAndMissingCredential(t *testing.T) {
	db := newTestAdminDB(t)
	codexID, err := db.InsertAccount(context.Background(), "codex", "codex-refresh", "")
	if err != nil {
		t.Fatal(err)
	}
	missingID, err := db.InsertAccountWithUpstream(context.Background(), "missing", "google", auth.UpstreamAntigravity, map[string]any{"upstream_type": auth.UpstreamAntigravity}, "")
	if err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(db, nil, nil)
	t.Cleanup(store.Stop)
	handler := &Handler{db: db, store: store}
	wrong := callAntigravityAdminHandler(t, http.MethodGet, "/state", codexID, handler.GetAntigravityAccountState)
	if wrong.Code != http.StatusBadRequest {
		t.Fatalf("wrong-channel status=%d body=%s", wrong.Code, wrong.Body.String())
	}
	missing := callAntigravityAdminHandler(t, http.MethodPost, "/probe", missingID, handler.ProbeAntigravityAccountCapabilities)
	if missing.Code != http.StatusBadRequest {
		t.Fatalf("missing-credential status=%d body=%s", missing.Code, missing.Body.String())
	}
	notFound := callAntigravityAdminHandler(t, http.MethodGet, "/state", 999999, handler.GetAntigravityAccountState)
	if notFound.Code != http.StatusNotFound {
		t.Fatalf("not-found status=%d body=%s", notFound.Code, notFound.Body.String())
	}
}

func TestAntigravityStateCapabilityTimestampRoundTrip(t *testing.T) {
	observed := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	encoded, _ := json.Marshal([]antigravityCapabilityObservation{{CredentialGeneration: 3, Protocol: "interactions", ModelID: "gemini-pro-agent", Status: "ok", Verified: true, Source: "explicit_probe", ObservedAt: observed}})
	state := antigravityStateFromRow(&database.AccountRow{ID: 1, CredentialGeneration: 3, Credentials: map[string]any{
		"upstream_type": auth.UpstreamAntigravity, "api_key": "secret", "antigravity_capabilities": string(encoded),
	}})
	if len(state.Capabilities) != 1 || !state.Capabilities[0].ObservedAt.Equal(observed) || state.Capabilities[0].ModelID != "gemini-3.1-pro-high" {
		t.Fatalf("state capabilities = %+v", state.Capabilities)
	}
}

func TestAntigravityStateProjectsAmbiguousWireCapabilityAgainstCatalog(t *testing.T) {
	observed := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	encoded, _ := json.Marshal([]antigravityCapabilityObservation{{CredentialGeneration: 4, Protocol: "interactions", ModelID: "gemini-3.5-flash-low", Status: "ok", Verified: true, Source: "explicit_probe", ObservedAt: observed}})
	state := antigravityStateFromRow(&database.AccountRow{ID: 1, CredentialGeneration: 4, Credentials: map[string]any{
		"upstream_type": auth.UpstreamAntigravity, "api_key": "secret", "models": []string{"gemini-3.5-flash-extra-low", "gemini-3.5-flash-low", "gemini-3-flash-agent"}, "antigravity_capabilities": string(encoded),
	}})
	if !reflect.DeepEqual(state.Catalog.Models, []string{"gemini-3.5-flash-low", "gemini-3.5-flash-medium", "gemini-3.5-flash-high"}) || len(state.Capabilities) != 1 || state.Capabilities[0].ModelID != "gemini-3.5-flash-low" {
		t.Fatalf("ambiguous raw capability projection = catalog %v capabilities %+v", state.Catalog.Models, state.Capabilities)
	}
}
