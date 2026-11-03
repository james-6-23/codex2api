package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
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

func TestListModelsOrManifestServesAntigravityAsCodexManifest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	store.AddAccount(&auth.Account{
		DBID: 9, UpstreamType: auth.UpstreamAntigravity, AccessToken: "google-token", AntigravityProjectID: "project-id",
		Models: []string{"gemini-3.6-flash-low", "gemini-3.6-flash-medium", "gemini-3.6-flash-high", "claude-sonnet-4-6"},
	})
	handler := NewHandler(store, nil, nil, nil)
	row := &database.APIKeyRow{ID: 3, Limits: database.APIKeyLimits{UpstreamChannel: database.UpstreamChannelAntigravity}}

	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set(contextAPIKeyRow, row)
		c.Next()
	})
	router.GET("/v1/models", handler.listModelsOrManifest)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models?client_version=0.140.0", nil)
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("antigravity Codex manifest status = %d body=%s, want 200", rec.Code, rec.Body.String())
	}
	var payload struct {
		Models []struct {
			Slug             string `json:"slug"`
			PreferWebsockets bool   `json:"prefer_websockets"`
		} `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("manifest JSON: %v body=%s", err, rec.Body.String())
	}
	got := map[string]bool{}
	for _, model := range payload.Models {
		got[model.Slug] = true
		if model.PreferWebsockets {
			t.Fatalf("slug %s prefer_websockets=true, Antigravity must stay on HTTP", model.Slug)
		}
	}
	if !got["gemini-3.6-flash"] || !got["claude-sonnet-4-6"] || len(got) != 2 {
		t.Fatalf("manifest slugs = %v, want cockpit Antigravity models", got)
	}

	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"object":"list"`) {
		t.Fatalf("cockpit list status=%d body=%s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "gemini-3.6-flash-low") || !strings.Contains(rec.Body.String(), `"id":"gemini-3.6-flash"`) {
		t.Fatalf("cockpit list has wrong Antigravity surface: %s", rec.Body.String())
	}
}

func TestMergeCodexManifestModelsAppendsMissingRelaySlugs(t *testing.T) {
	merged, err := mergeCodexManifestModels(
		[]byte(`{"models":[{"slug":"gpt-5.4","display_name":"GPT"}],"future":true}`),
		[]api.Model{{ID: "gpt-5.4"}, {ID: "gemini-2.5-flash"}},
	)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Models []struct {
			Slug string `json:"slug"`
		} `json:"models"`
		Future bool `json:"future"`
	}
	if err := json.Unmarshal(merged, &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.Future || len(payload.Models) != 2 || payload.Models[0].Slug != "gpt-5.4" || payload.Models[1].Slug != "gemini-2.5-flash" {
		t.Fatalf("merged = %s", merged)
	}
}

func TestMergedCodexManifestUsesScopedCapabilitiesAndGatewayETag(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestModelRegistryDB(t)
	ctx := context.Background()
	id, err := db.InsertAccount(ctx, "manifest-caps", "test-refresh", "")
	if err != nil {
		t.Fatal(err)
	}
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(nil, nil, nil)
	store.AddAccount(&auth.Account{DBID: id, CredentialGeneration: row.CredentialGeneration, AccessToken: "test-only", PlanType: "plus"})
	h := NewHandler(store, db, nil, nil)
	source := []byte(`{"models":[{"slug":"gpt-6-astra","use_responses_lite":true,"experimental_supported_tools":["function"],"base_instructions":"keep source instructions"}],"future":{"keep":true}}`)
	router := gin.New()
	router.GET("/models", func(c *gin.Context) {
		c.Set(contextAPIKeyRow, &database.APIKeyRow{ID: 1})
		h.writeMergedCodexManifest(c, source, `W/"upstream-validator"`, nil)
	})
	request := func(etag string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/models", nil)
		req.Header.Set("If-None-Match", etag)
		router.ServeHTTP(rec, req)
		return rec
	}
	first := request(`W/"upstream-validator"`)
	if first.Code != http.StatusOK || first.Header().Get("ETag") == `W/"upstream-validator"` || first.Header().Get("ETag") == "" {
		t.Fatalf("source ETag bypassed gateway representation: status=%d etag=%q", first.Code, first.Header().Get("ETag"))
	}
	// An account that has not been observed yet cannot narrow the live descriptor.
	if !gjson.GetBytes(first.Body.Bytes(), "models.0.use_responses_lite").Bool() || gjson.GetBytes(first.Body.Bytes(), "models.0.experimental_supported_tools").Raw != `["function"]` || gjson.GetBytes(first.Body.Bytes(), "models.0.base_instructions").String() != "keep source instructions" || !gjson.GetBytes(first.Body.Bytes(), "future.keep").Bool() {
		t.Fatalf("unknown capabilities or source fields mishandled: %s", first.Body.String())
	}
	if cached := request(first.Header().Get("ETag")); cached.Code != http.StatusNotModified || cached.Body.Len() != 0 {
		t.Fatalf("unchanged gateway manifest did not cache: status=%d body=%s", cached.Code, cached.Body.String())
	}
	models := parseCodexModelCapabilities([]byte(`{"models":[{"slug":"gpt-6-astra","use_responses_lite":false,"experimental_supported_tools":[]}]}`))
	if err := db.SaveModelCapabilities(ctx, database.ModelCapabilitySnapshot{AccountID: id, CredentialGeneration: row.CredentialGeneration, ObservedAt: time.Now().UnixNano(), Models: models}); err != nil {
		t.Fatal(err)
	}
	updated := request(first.Header().Get("ETag"))
	if updated.Code != http.StatusOK || updated.Header().Get("ETag") == first.Header().Get("ETag") || gjson.GetBytes(updated.Body.Bytes(), "models.0.use_responses_lite").Bool() || gjson.GetBytes(updated.Body.Bytes(), "models.0.experimental_supported_tools").Raw != "[]" {
		t.Fatalf("new scoped capabilities hidden behind stale ETag: status=%d body=%s", updated.Code, updated.Body.String())
	}
}

func TestScopedAntigravityManifestKeepsOnlyAvailableEfforts(t *testing.T) {
	body, err := buildScopedCodexManifest([]api.Model{
		{ID: "gemini-3.7-flash-high", OwnedBy: "google"},
		{ID: "gemini-3.6-flash-medium", OwnedBy: "google"},
		{ID: "gemini-3.8-flash-low", OwnedBy: "google"},
		{ID: "gemini-3.1-pro-high", OwnedBy: "google"},
		{ID: "claude-sonnet-4-6", OwnedBy: "google"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Models []scopedCodexManifestItem `json:"models"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	for _, item := range payload.Models {
		if strings.HasPrefix(item.Slug, "gemini-") && len(item.SupportedReasoningLevels) != 1 {
			t.Fatalf("%s must expose only its scoped effort: %v", item.Slug, item.SupportedReasoningLevels)
		}
	}
}

func TestAntigravityManifestDoesNotInferReasoningFromNonGeminiNames(t *testing.T) {
	body, err := buildScopedCodexManifest([]api.Model{
		{ID: "claude-opus-4-6-thinking", OwnedBy: "google"},
		{ID: "claude-sonnet-4-6", OwnedBy: "google"},
		{ID: "gpt-oss-120b-medium", OwnedBy: "google"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Models []scopedCodexManifestItem `json:"models"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	for _, model := range payload.Models {
		if len(model.SupportedReasoningLevels) != 0 {
			t.Fatalf("%s unexpectedly advertises reasoning levels: %v", model.Slug, model.SupportedReasoningLevels)
		}
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
	manifest, err := fetchCodexModelsManifestWithURL(context.Background(), account, "", server.URL, "0.140.0", "", nil)
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

func TestFetchCodexModelsManifestRejectsConfiguredOversizeBody(t *testing.T) {
	previous := CurrentRuntimeSettings()
	next := previous
	next.ModelsListReadMaxBytes = database.MinModelsListReadMaxBytes
	ApplyRuntimeSettings(next)
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", int(database.MinModelsListReadMaxBytes)+1)))
	}))
	defer server.Close()

	account := &auth.Account{DBID: 1, AccessToken: "at-123"}
	_, err := fetchCodexModelsManifestWithURL(context.Background(), account, "", server.URL, "0.140.0", "", nil)
	if !errors.Is(err, ErrModelsListResponseTooLarge) {
		t.Fatalf("error = %v, want ErrModelsListResponseTooLarge", err)
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
	manifest, err := fetchCodexModelsManifestWithURL(context.Background(), account, "", server.URL, "0.140.0", `W/"abc123"`, nil)
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
	_, err := fetchCodexModelsManifestWithURL(context.Background(), account, "", server.URL, "0.140.0", "", nil)
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
	if _, err := fetchCodexModelsManifestWithURL(context.Background(), account, "", server.URL, "", "", nil); err != nil {
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
	if _, err := fetchCodexModelsManifestWithURL(context.Background(), account, "", server.URL, "0.140.0", "", nil); err != nil {
		t.Fatalf("fetchCodexModelsManifestWithURL error: %v", err)
	}
	if gotAccountID != "acc-override" {
		t.Errorf("chatgpt-account-id = %q, want acc-override", gotAccountID)
	}
}
