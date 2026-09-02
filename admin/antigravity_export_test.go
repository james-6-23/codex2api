package admin

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func insertAntigravityExportAccount(t *testing.T, handler *Handler, name string, credentials map[string]any) int64 {
	t.Helper()
	id, err := handler.db.InsertAccountWithUpstream(context.Background(), name, "google", auth.UpstreamAntigravity, credentials, "")
	if err != nil {
		t.Fatalf("insert Antigravity account: %v", err)
	}
	return id
}

func TestAntigravityExportOAuthRoundTripsThroughImporter(t *testing.T) {
	entry, ok := antigravityAccountRowToExportEntry(&database.AccountRow{
		ID: 7, Name: "OAuth user", Platform: "google", ProxyURL: "http://127.0.0.1:18080", Enabled: false, UpdatedAt: time.Now(),
		Credentials: map[string]any{
			"upstream_type": auth.UpstreamAntigravity, "email": "oauth@example.com",
			"access_token": "access-secret", "refresh_token": "refresh-secret", "id_token": "id-secret",
			"project_id": "project-1", "oauth_client_key": "enterprise",
			"antigravity_client_id": "client-id", "antigravity_client_secret": "client-secret",
			"oauth_scope": "scope-a", "expires_at": "2026-08-22T00:00:00Z",
		},
	}, exportProxyResolver{include: true})
	if !ok || entry.AuthKind != auth.AntigravityAuthKindOAuth {
		t.Fatalf("OAuth export entry = %+v, ok=%v", entry, ok)
	}
	encoded, err := marshalAntigravityExportEntry(entry)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parseAntigravityImportContent(string(encoded), antigravityImportDefaults{})
	if err != nil || len(parsed) != 1 {
		t.Fatalf("parse export: credentials=%+v err=%v", parsed, err)
	}
	if parsed[0].RefreshToken != "refresh-secret" || parsed[0].ClientSecret != "client-secret" || parsed[0].ProjectID != "project-1" {
		t.Fatalf("round-trip credential = %+v", parsed[0])
	}
	documents, err := parseAntigravityImportDocuments(string(encoded), antigravityImportDefaults{})
	if err != nil || len(documents) != 1 {
		t.Fatalf("parse lossless export: documents=%+v err=%v", documents, err)
	}
	document := documents[0]
	if document.AuthKind != auth.AntigravityAuthKindOAuth || !document.DisabledPresent || !document.Disabled ||
		document.Name != "OAuth user" || document.ProxyURL != "http://127.0.0.1:18080" {
		t.Fatalf("lossless OAuth document = %+v", document)
	}
}

func TestAntigravityExportAPIKeyIsExplicitAndSecretBearing(t *testing.T) {
	entry, ok := antigravityAccountRowToExportEntry(&database.AccountRow{
		ID: 8, Name: "API key user", Platform: "google", ProxyURL: "socks5://127.0.0.1:1080", Enabled: false,
		Credentials: map[string]any{
			"upstream_type": auth.UpstreamAntigravity, "api_key": "google-secret-key",
			"models": []string{"gemini-test"}, "model_mapping": `{"alias":"gemini-test"}`,
		},
	}, exportProxyResolver{include: true})
	if !ok || entry.AuthKind != auth.AntigravityAuthKindAPIKey || entry.APIKey != "google-secret-key" {
		t.Fatalf("API-key entry = %+v, ok=%v", entry, ok)
	}
	if entry.AccessToken != "" || entry.RefreshToken != "" {
		t.Fatalf("API-key export mixed OAuth tokens: %+v", entry)
	}
	encoded, err := marshalAntigravityExportEntry(entry)
	if err != nil {
		t.Fatal(err)
	}
	documents, err := parseAntigravityImportDocuments(string(encoded), antigravityImportDefaults{
		OAuthClientKey: "must-not-apply", ClientID: "must-not-apply", ClientSecret: "must-not-apply",
	})
	if err != nil || len(documents) != 1 {
		t.Fatalf("parse API-key export: documents=%+v err=%v", documents, err)
	}
	document := documents[0]
	if document.AuthKind != auth.AntigravityAuthKindAPIKey || document.APIKey != "google-secret-key" ||
		len(document.Models) != 1 || document.Models[0] != "gemini-test" || document.ModelMapping != `{"alias":"gemini-test"}` ||
		!document.DisabledPresent || !document.Disabled || document.Name != "API key user" || document.ProxyURL != "socks5://127.0.0.1:1080" {
		t.Fatalf("API-key round-trip document = %+v", document)
	}
	if document.Credential.ClientID != "" || document.Credential.ClientSecret != "" || document.Credential.OAuthClientKey != "" {
		t.Fatalf("OAuth defaults leaked into API-key document = %+v", document.Credential)
	}
	if _, err := parseAntigravityImportContent(string(encoded), antigravityImportDefaults{}); err == nil || strings.Contains(err.Error(), "google-secret-key") {
		t.Fatalf("legacy OAuth parser error = %v", err)
	}
}

func TestAntigravityExportSelectedAccountsZIPAndWrongChannelNotFound(t *testing.T) {
	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, nil)
	t.Cleanup(store.Stop)
	handler := &Handler{db: db, store: store}
	first := insertAntigravityExportAccount(t, handler, "first", map[string]any{
		"upstream_type": auth.UpstreamAntigravity, "email": "../../a@example.com", "refresh_token": "refresh-a",
	})
	second := insertAntigravityExportAccount(t, handler, "second", map[string]any{
		"upstream_type": auth.UpstreamAntigravity, "email": "a@example.com", "api_key": "key-b",
	})
	notExportable := insertAntigravityExportAccount(t, handler, "empty", map[string]any{
		"upstream_type": auth.UpstreamAntigravity,
	})
	codex, err := db.InsertAccount(context.Background(), "codex", "codex-refresh-secret", "")
	if err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/admin/accounts/antigravity/export?ids="+strconv.FormatInt(first, 10)+","+strconv.FormatInt(second, 10)+","+strconv.FormatInt(notExportable, 10), nil)
	handler.ExportAntigravityAccounts(c)
	if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != "application/zip" {
		t.Fatalf("status=%d content-type=%q body=%s", recorder.Code, recorder.Header().Get("Content-Type"), recorder.Body.String())
	}
	if recorder.Header().Get("Cache-Control") != "no-store, max-age=0" || recorder.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("secret download headers = %#v", recorder.Header())
	}
	if recorder.Header().Get("X-Export-Count") != "2" {
		t.Fatalf("X-Export-Count = %q, want actual credential count 2", recorder.Header().Get("X-Export-Count"))
	}
	reader, err := zip.NewReader(bytes.NewReader(recorder.Body.Bytes()), int64(recorder.Body.Len()))
	if err != nil || len(reader.File) != 2 {
		t.Fatalf("zip files=%v err=%v", len(reader.File), err)
	}
	for _, member := range reader.File {
		if strings.Contains(member.Name, "/") || strings.Contains(member.Name, "\\") {
			t.Fatalf("unsafe ZIP member name %q", member.Name)
		}
		body, openErr := member.Open()
		if openErr != nil {
			t.Fatal(openErr)
		}
		data, _ := io.ReadAll(body)
		_ = body.Close()
		var decoded map[string]any
		if json.Unmarshal(data, &decoded) != nil || decoded["type"] != "antigravity" {
			t.Fatalf("invalid member %q: %s", member.Name, data)
		}
	}

	notFound := httptest.NewRecorder()
	c, _ = gin.CreateTestContext(notFound)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/admin/accounts/antigravity/export?ids="+strconv.FormatInt(codex, 10), nil)
	handler.ExportAntigravityAccounts(c)
	if notFound.Code != http.StatusNotFound || strings.Contains(notFound.Body.String(), "codex-refresh-secret") {
		t.Fatalf("wrong-channel response status=%d body=%s", notFound.Code, notFound.Body.String())
	}
}

func TestAntigravityExportSingleSetsActualCount(t *testing.T) {
	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, nil)
	t.Cleanup(store.Stop)
	handler := &Handler{db: db, store: store}
	insertAntigravityExportAccount(t, handler, "api", map[string]any{
		"upstream_type": auth.UpstreamAntigravity, "api_key": "single-secret",
	})
	insertAntigravityExportAccount(t, handler, "empty", map[string]any{
		"upstream_type": auth.UpstreamAntigravity,
	})

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/admin/accounts/antigravity/export", nil)
	handler.ExportAntigravityAccounts(c)
	if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("status=%d content-type=%q body=%s", recorder.Code, recorder.Header().Get("Content-Type"), recorder.Body.String())
	}
	if recorder.Header().Get("X-Export-Count") != "1" {
		t.Fatalf("X-Export-Count = %q, want actual credential count 1", recorder.Header().Get("X-Export-Count"))
	}
}

func TestAntigravityExportInvalidIDsFailClosed(t *testing.T) {
	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, nil)
	t.Cleanup(store.Stop)
	handler := &Handler{db: db, store: store}
	insertAntigravityExportAccount(t, handler, "api", map[string]any{
		"upstream_type": auth.UpstreamAntigravity, "api_key": "must-not-export",
	})
	for _, query := range []string{"ids=", "ids=not-a-number", "ids=1,bad", "ids=0"} {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodGet, "/api/admin/accounts/antigravity/export?"+query, nil)
		handler.ExportAntigravityAccounts(c)
		if recorder.Code != http.StatusBadRequest || strings.Contains(recorder.Body.String(), "must-not-export") {
			t.Fatalf("query=%q status=%d body=%s", query, recorder.Code, recorder.Body.String())
		}
	}
}

func TestAntigravityExportRouteRequiresAdminAuthorization(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	tokenCache := cache.NewMemory(4)
	t.Cleanup(func() { _ = tokenCache.Close() })
	store := auth.NewStore(db, tokenCache, nil)
	t.Cleanup(store.Stop)
	handler := NewHandler(store, db, tokenCache, nil, "admin-secret")
	insertAntigravityExportAccount(t, handler, "api", map[string]any{"upstream_type": auth.UpstreamAntigravity, "api_key": "never-in-auth-error"})
	router := gin.New()
	handler.RegisterRoutes(router)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/accounts/antigravity/export", nil))
	if recorder.Code != http.StatusUnauthorized || strings.Contains(recorder.Body.String(), "never-in-auth-error") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestAntigravityOrdinaryAccountResponseDoesNotLeakSecrets(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	t.Cleanup(store.Stop)
	row := &database.AccountRow{ID: 9, Platform: "google", Enabled: true, Credentials: map[string]any{
		"upstream_type": auth.UpstreamAntigravity, "access_token": "access-leak", "refresh_token": "refresh-leak",
		"api_key": "api-key-leak", "antigravity_client_secret": "client-secret-leak",
	}}
	encoded, err := json.Marshal((&Handler{store: store}).buildAccountResponse(row, nil, nil, nil, nil, true))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"access-leak", "refresh-leak", "api-key-leak", "client-secret-leak"} {
		if bytes.Contains(encoded, []byte(secret)) {
			t.Fatalf("ordinary response leaked %q: %s", secret, encoded)
		}
	}
}
