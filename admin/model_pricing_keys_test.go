package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestModelPricingManagementKeysKeepsIndependentAlias(t *testing.T) {
	got := modelPricingManagementKeys([]string{
		"gpt-5.4",
		"codex-auto-review",
		"gpt-5.4-openai-compact",
	})
	want := []string{"gpt-5.4", "codex-auto-review"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("modelPricingManagementKeys() = %v, want %v", got, want)
	}
}

func TestAntigravityChannelModelsIncludesLogicalModels(t *testing.T) {
	h := &Handler{}
	models := h.antigravityChannelModels()
	hasLogical := false
	for _, m := range models {
		if m == "gemini-3.8-flash" {
			hasLogical = true
			break
		}
	}
	if !hasLogical {
		t.Fatalf("antigravityChannelModels() should include bare logical model gemini-3.8-flash, got: %v", models)
	}
}

func TestListModelPricingIncludesDefaultClaudeChannel(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	store := auth.NewStore(nil, nil, nil)
	defer store.Stop()
	// Add an Antigravity account declaring claude-sonnet-4-6 to verify native
	// Claude models still stay grouped under the Claude channel.
	store.AddAccount(&auth.Account{
		DBID:         1,
		UpstreamType: auth.UpstreamAntigravity,
		AccessToken:  "ag-token",
		Models:       []string{"gemini-3.8-flash", "claude-sonnet-4-6"},
	})
	h := &Handler{db: db, store: store}

	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/model-pricing", nil)
	h.ListModelPricing(c)

	if w.Code != http.StatusOK {
		t.Fatalf("ListModelPricing status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp struct {
		Models []modelPricingRow `json:"models"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	claudeModels := make(map[string]modelPricingRow)
	for _, row := range resp.Models {
		if row.Channel == database.UpstreamChannelClaude {
			claudeModels[row.Model] = row
		}
	}
	for _, required := range []string{"claude-sonnet-4-6", "claude-opus-4-6", "claude-haiku-4-5", "claude-sonnet-5"} {
		row, ok := claudeModels[required]
		if !ok {
			t.Fatalf("expected %q under claude channel, got claude models: %v", required, claudeModels)
		}
		if row.Pricing.Input <= 0 || row.Pricing.Output <= 0 {
			t.Fatalf("expected positive default pricing for %q, got %+v", required, row.Pricing)
		}
	}
}
