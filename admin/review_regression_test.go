package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func TestReviewDuplicateClaudeRefreshDoesNotConsumeCredential(t *testing.T) {
	db := newTestAdminDB(t)
	ctx := context.Background()
	id, err := db.InsertAccountWithUpstream(ctx, "existing", "anthropic", auth.UpstreamClaude, map[string]interface{}{
		"upstream_type": auth.UpstreamClaude, "refresh_token": "existing-rotating-token", "access_token": "existing-access", "account_id": "existing-id",
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(db, nil, nil)
	t.Cleanup(store.Stop)
	h := &Handler{db: db, store: store, refreshClaudeTokensForImport: func(context.Context, string, string) (*auth.ClaudeTokenData, error) {
		t.Fatal("duplicate import consumed the refresh token")
		return nil, nil
	}}
	_, err = h.createClaudeAccount(ctx, "", "", "", &auth.ClaudeTokenData{RefreshToken: "existing-rotating-token"}, "test", nil)
	if conflict, ok := err.(*claudeAccountCreateError); !ok || conflict.Status != http.StatusConflict {
		t.Fatalf("expected conflict, got %v", err)
	}
	row, err := db.GetAccountByID(ctx, id)
	if err != nil || row.GetCredential("refresh_token") != "existing-rotating-token" || row.GetCredential("access_token") != "existing-access" {
		t.Fatal("duplicate changed existing credentials")
	}
}

func TestReviewAstraLongOnlyOverridePreservesExistingPricing(t *testing.T) {
	db := newTestAdminDB(t)
	t.Cleanup(func() { database.SetModelPricingOverrides(nil) })
	_, err := db.MutateModelPricingSettings(context.Background(), nil, func(m map[string]database.ModelPricingOverride) error {
		m["gpt-6-astra"] = database.ModelPricingOverride{Source: "custom", Input: 7}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{db: db}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/admin/model-pricing", strings.NewReader(`{"model":"gpt-6-astra","pricing":{"input_long":20,"output_long":75}}`))
	h.UpdateModelPricing(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", w.Code, w.Body)
	}
	settings, err := db.GetSystemSettings(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	overrides, err := database.ParseModelPricingOverridesJSON(settings.ModelPricingOverrides)
	if err != nil || overrides["gpt-6-astra"].Input != 7 {
		t.Fatal("rejected pricing request erased an existing override")
	}
}

func TestReviewClaudeBillingReprobeRestoresWhitelist(t *testing.T) {
	db := newTestAdminDB(t)
	ctx := context.Background()
	models := []string{"claude-haiku-4-5", "claude-fable-5-1"}
	id, err := db.InsertAccountWithUpstream(ctx, "probe", "anthropic", auth.UpstreamClaude, map[string]interface{}{"upstream_type": auth.UpstreamClaude, "models": models, "access_token": "test-token"}, "")
	if err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(db, nil, nil)
	t.Cleanup(store.Stop)
	account := &auth.Account{DBID: id, UpstreamType: auth.UpstreamClaude, AccessToken: "test-token", Models: models}
	store.AddAccount(account)
	h := &Handler{db: db, store: store}
	if !proxy.HandleClaudeModelBillingRejection(store, account, models[1], 429, []byte(`{"error":{"message":"Usage credits are required for this model."}}`)) {
		t.Fatal("billing rejection not handled")
	}
	if got, err := h.connectionTestModelForAccount(ctx, account, models[1]); err != nil || got != models[1] {
		t.Fatalf("explicit reprobe rejected: %q %v", got, err)
	}
	if _, err := h.connectionTestModelForAccount(ctx, account, "claude-unrelated"); err == nil {
		t.Fatal("unrelated whitelist exclusion was bypassed")
	}
	if err := store.RestoreClaudeAccountModel(ctx, account, models[1]); err != nil {
		t.Fatal(err)
	}
	store.ClearModelCooldown(account, models[1])
	row, err := db.GetAccountByID(ctx, id)
	if err != nil || len(row.GetCredentialStringSlice("models")) != 2 {
		t.Fatal("successful probe did not restore the persisted catalog")
	}
}

func TestReviewShortDiagnosticSecretsPreserveIDs(t *testing.T) {
	got := sanitizeCodexTestText(`{"response_id":"resp_0123","usage":1234,"api_key":"1","echo":"1 1"}`, []string{"1"})
	var parsed map[string]any
	if json.Unmarshal([]byte(got), &parsed) != nil || parsed["response_id"] != "resp_0123" || parsed["usage"] != float64(1234) {
		t.Fatalf("short placeholder corrupted diagnostics: %s", got)
	}
	if strings.Contains(got, `"1"`) || strings.Contains(got, `1 1`) {
		t.Fatal("standalone credential was exposed")
	}
}
