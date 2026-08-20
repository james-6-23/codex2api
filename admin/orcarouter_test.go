package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func newOrcaRouterTestHandler(t *testing.T) (*Handler, *database.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "orcarouter-handler.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	store := auth.NewStore(db, cache.NewMemory(1), &database.SystemSettings{TestModel: "orcarouter/auto"})
	return &Handler{db: db, store: store}, db
}

func TestAddOrcaRouterAccountHandler(t *testing.T) {
	handler, db := newOrcaRouterTestHandler(t)
	defer db.Close()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("path = %q, want /v1/models", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-orca-test" {
			t.Fatalf("Authorization = %q", got)
		}
		_, _ = w.Write([]byte(`{"data":[{"id":"orcarouter/auto"},{"id":"orcarouter/fusion"}]}`))
	}))
	defer server.Close()

	// Fetch models through the OrcaRouter endpoint first.
	body := `{"base_url":"` + server.URL + `/v1","api_key":"sk-orca-test"}`
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/orcarouter/models", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handler.FetchOrcaRouterModels(c)
	if recorder.Code != http.StatusOK {
		t.Fatalf("FetchOrcaRouterModels status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	var fetched struct {
		Models []string `json:"models"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &fetched); err != nil {
		t.Fatalf("decode fetch response: %v", err)
	}
	if len(fetched.Models) != 2 || fetched.Models[0] != "orcarouter/auto" {
		t.Fatalf("fetched models = %#v", fetched.Models)
	}

	// Add the account via the OrcaRouter route.
	body = `{"name":"orca","base_url":"` + server.URL + `/v1","api_key":"sk-orca-test","models":["orcarouter/auto","orcarouter/fusion"]}`
	recorder = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/orcarouter", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handler.AddOrcaRouterAccount(c)
	if recorder.Code != http.StatusOK {
		t.Fatalf("AddOrcaRouterAccount status = %d, body=%s", recorder.Code, recorder.Body.String())
	}

	var added struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &added); err != nil {
		t.Fatalf("decode add response: %v", err)
	}
	if added.ID <= 0 {
		t.Fatal("add did not return an account ID")
	}

	row, err := db.GetAccountByID(context.Background(), added.ID)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if got := strings.TrimSpace(row.GetCredential("upstream_type")); got != auth.UpstreamOrcaRouter {
		t.Fatalf("upstream_type = %q, want orcarouter", got)
	}
	if got := strings.TrimSpace(row.Platform); got != "orcarouter" {
		t.Fatalf("platform = %q, want orcarouter", got)
	}
	runtime := handler.store.FindByID(added.ID)
	if runtime == nil {
		t.Fatal("runtime account not found after add")
	}
	if !runtime.IsOrcaRouterAPI() {
		t.Fatal("runtime account must be IsOrcaRouterAPI")
	}
	if !runtime.IsOpenAIResponsesAPI() {
		t.Fatal("runtime account must dispatch via IsOpenAIResponsesAPI")
	}
}

func TestAddOrcaRouterAccountRejectsMissingModels(t *testing.T) {
	handler, db := newOrcaRouterTestHandler(t)
	defer db.Close()

	body := `{"base_url":"https://api.orcarouter.ai/v1","api_key":"sk-orca-test","models":[]}`
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/orcarouter", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	handler.AddOrcaRouterAccount(c)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty models, got %d", recorder.Code)
	}
}
