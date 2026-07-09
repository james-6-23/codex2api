package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

// TestListModelsOrManifest_DispatchesByClientVersion 验证 /models 分发:
// 带 client_version 的请求走 manifest 透传(无可用账号时 fast-fail 503),
// 不带的保持 OpenAI 兼容列表。
func TestListModelsOrManifest_DispatchesByClientVersion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := auth.NewStore(nil, nil, nil)
	handler := NewHandler(store, nil, nil, nil)

	router := gin.New()
	router.GET("/v1/models", handler.listModelsOrManifest)

	// Codex 客户端形态:分发到 manifest 路径,空账号池 fast-fail。
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models?client_version=0.140.0", nil)
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("manifest dispatch status = %d, want 503", rec.Code)
	}

	// 普通 OpenAI 客户端形态:返回兼容列表。
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("list dispatch status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"object":"list"`) && !strings.Contains(rec.Body.String(), `"object": "list"`) {
		t.Errorf("list body = %s, want OpenAI list shape", rec.Body.String())
	}
}

func TestFetchCodexModelsManifest_PassesThroughBodyAndETag(t *testing.T) {
	const manifestBody = `{"models":[{"slug":"gpt-5.6-sol"},{"slug":"gpt-5.6-terra"}]}`

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer at-123" {
			t.Errorf("Authorization = %q, want Bearer at-123", got)
		}
		if got := r.Header.Get("chatgpt-account-id"); got != "acc-1" {
			t.Errorf("chatgpt-account-id = %q, want acc-1", got)
		}
		if got := r.Header.Get("Originator"); got != Originator {
			t.Errorf("Originator = %q, want %q", got, Originator)
		}
		if !strings.HasPrefix(r.Header.Get("User-Agent"), "codex-tui/") {
			t.Errorf("User-Agent = %q, want codex-tui prefix", r.Header.Get("User-Agent"))
		}
		if got := r.Header.Get("Version"); got != "0.140.0" {
			t.Errorf("Version = %q, want 0.140.0", got)
		}
		if got := r.URL.Query().Get("client_version"); got != "0.140.0" {
			t.Errorf("client_version = %q, want 0.140.0", got)
		}
		w.Header().Set("ETag", `W/"abc123"`)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(manifestBody))
	}))
	defer server.Close()

	account := &auth.Account{DBID: 1, AccessToken: "at-123", AccountID: "acc-1"}
	manifest, err := fetchCodexModelsManifestWithURL(context.Background(), account, "", server.URL, "0.140.0", "")
	if err != nil {
		t.Fatalf("fetchCodexModelsManifestWithURL error: %v", err)
	}
	if manifest.NotModified {
		t.Error("NotModified = true, want false")
	}
	if string(manifest.Body) != manifestBody {
		t.Errorf("Body = %q, want %q", manifest.Body, manifestBody)
	}
	if manifest.ETag != `W/"abc123"` {
		t.Errorf("ETag = %q, want W/\"abc123\"", manifest.ETag)
	}
}

func TestFetchCodexModelsManifest_NotModified(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("If-None-Match"); got != `W/"abc123"` {
			t.Errorf("If-None-Match = %q, want W/\"abc123\"", got)
		}
		w.Header().Set("ETag", `W/"abc123"`)
		w.WriteHeader(http.StatusNotModified)
	}))
	defer server.Close()

	account := &auth.Account{DBID: 1, AccessToken: "at-123"}
	manifest, err := fetchCodexModelsManifestWithURL(context.Background(), account, "", server.URL, "0.140.0", `W/"abc123"`)
	if err != nil {
		t.Fatalf("fetchCodexModelsManifestWithURL error: %v", err)
	}
	if !manifest.NotModified {
		t.Error("NotModified = false, want true")
	}
	if manifest.ETag != `W/"abc123"` {
		t.Errorf("ETag = %q, want W/\"abc123\"", manifest.ETag)
	}
}

func TestFetchCodexModelsManifest_UpstreamErrorFastFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"detail":"blocked"}`))
	}))
	defer server.Close()

	account := &auth.Account{DBID: 1, AccessToken: "at-123"}
	_, err := fetchCodexModelsManifestWithURL(context.Background(), account, "", server.URL, "0.140.0", "")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "403") {
		t.Errorf("error = %v, want to contain 403", err)
	}
}

func TestFetchCodexModelsManifest_EmptyClientVersionFallsBack(t *testing.T) {
	var gotVersion string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotVersion = r.URL.Query().Get("client_version")
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer server.Close()

	account := &auth.Account{DBID: 1, AccessToken: "at-123"}
	if _, err := fetchCodexModelsManifestWithURL(context.Background(), account, "", server.URL, "", ""); err != nil {
		t.Fatalf("fetchCodexModelsManifestWithURL error: %v", err)
	}
	if gotVersion != latestCodexCLIVersion {
		t.Errorf("client_version = %q, want %q", gotVersion, latestCodexCLIVersion)
	}
}

func TestFetchCodexModelsManifest_UsesCustomHeaderAccountIDOverride(t *testing.T) {
	var gotAccountID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccountID = r.Header.Get("chatgpt-account-id")
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer server.Close()

	account := &auth.Account{
		DBID:          1,
		AccessToken:   "at-123",
		AccountID:     "acc-1",
		CustomHeaders: map[string]string{"Chatgpt-Account-Id": "acc-override"},
	}
	if _, err := fetchCodexModelsManifestWithURL(context.Background(), account, "", server.URL, "0.140.0", ""); err != nil {
		t.Fatalf("fetchCodexModelsManifestWithURL error: %v", err)
	}
	if gotAccountID != "acc-override" {
		t.Errorf("chatgpt-account-id = %q, want acc-override", gotAccountID)
	}
}
