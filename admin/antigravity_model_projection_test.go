package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestAntigravityModelsForPersistenceExpandsLogicalModelsButKeepsAliasesFixed(t *testing.T) {
	logical := antigravityModelsForPersistence([]string{"gemini-3.5-flash", "gemini-3.1-pro"})
	wantLogical := []string{
		"gemini-3-flash-agent",
		"gemini-3.1-pro-low",
		"gemini-3.5-flash-extra-low",
		"gemini-3.5-flash-low",
		"gemini-pro-agent",
	}
	if !reflect.DeepEqual(logical, wantLogical) {
		t.Fatalf("logical persisted models = %v, want %v", logical, wantLogical)
	}

	aliases := antigravityModelsForPersistence([]string{
		"gemini-3.5-flash-low",
		"gemini-3.6-flash-high",
		"gemini-3.1-pro-high",
	})
	wantAliases := []string{"gemini-3.5-flash-extra-low", "gemini-3.6-flash-high", "gemini-pro-agent"}
	if !reflect.DeepEqual(aliases, wantAliases) {
		t.Fatalf("alias persisted models = %v, want %v", aliases, wantAliases)
	}
	wantPublished := []string{"gemini-3.5-flash-low", "gemini-3.6-flash-high", "gemini-3.1-pro-high"}
	if published := antigravityPublishedModels(aliases); !reflect.DeepEqual(published, wantPublished) {
		t.Fatalf("fixed-tier backings published = %v, want %v", published, wantPublished)
	}
}

func TestAntigravityAccountResponseProjectsRawModelsAndQuota(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	rawModels := []string{
		"gemini-3.5-flash-extra-low",
		"gemini-3.5-flash-low",
		"gemini-3-flash-agent",
		"gemini-3.6-flash-low",
		"gemini-3.6-flash-medium",
		"gemini-3.6-flash-high",
		"gemini-3.7-flash-tiered",
		"gemini-3.1-pro-low",
		"gemini-pro-agent",
		"claude-opus-4-6-thinking",
		"claude-sonnet-4-6",
		"gpt-oss-120b-medium",
	}
	rawQuotaModels := make([]auth.AntigravityModelQuota, 0, len(rawModels))
	for index, model := range rawModels {
		rawQuotaModels = append(rawQuotaModels, auth.AntigravityModelQuota{
			ModelID: model, DisplayName: "Provider " + model, RemainingPercent: 100 - index,
		})
	}
	quotaJSON, err := json.Marshal(auth.AntigravityQuotaSnapshot{
		Models:               rawQuotaModels,
		ModelForwardingRules: map[string]string{rawModels[0]: rawModels[3]},
		UpdatedAt:            now,
	})
	if err != nil {
		t.Fatal(err)
	}
	row := &database.AccountRow{
		ID: 1, Name: "Antigravity", Status: "active", Enabled: true, CreatedAt: now, UpdatedAt: now,
		Credentials: map[string]any{
			"upstream_type":     auth.UpstreamAntigravity,
			"access_token":      "token",
			"models":            rawModels,
			"antigravity_quota": string(quotaJSON),
		},
	}
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 4})
	t.Cleanup(store.Stop)
	response := (&Handler{store: store}).buildAccountResponse(row, nil, nil, nil, nil, true)
	wantModels := []string{
		"gemini-3.5-flash-low", "gemini-3.5-flash-medium", "gemini-3.5-flash-high",
		"gemini-3.6-flash-low", "gemini-3.6-flash-medium", "gemini-3.6-flash-high",
		"gemini-3.7-flash-low", "gemini-3.7-flash-medium", "gemini-3.7-flash-high",
		"gemini-3.1-pro-low", "gemini-3.1-pro-high",
		"claude-opus-4-6-thinking",
		"claude-sonnet-4-6",
		"gpt-oss-120b-medium",
	}
	if !reflect.DeepEqual(response.Models, wantModels) {
		t.Fatalf("response models = %v, want %v", response.Models, wantModels)
	}
	var quota auth.AntigravityQuotaSnapshot
	if err := json.Unmarshal(response.AntigravityQuota, &quota); err != nil {
		t.Fatal(err)
	}
	quotaModels := make([]string, 0, len(quota.Models))
	for _, model := range quota.Models {
		quotaModels = append(quotaModels, model.ModelID)
		if model.DisplayName != model.ModelID {
			t.Fatalf("quota display name = %q, model = %q", model.DisplayName, model.ModelID)
		}
	}
	if !reflect.DeepEqual(quotaModels, wantModels) || quota.ModelForwardingRules != nil {
		t.Fatalf("projected quota = %+v", quota)
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"gemini-3.5-flash-extra-low", "gemini-3-flash-agent", "gemini-3.7-flash-tiered", "gemini-pro-agent"} {
		if bytes.Contains(encoded, []byte(raw)) {
			t.Fatalf("account response leaked raw model %q: %s", raw, encoded)
		}
	}
	if got := row.GetCredentialStringSlice("models"); !reflect.DeepEqual(got, rawModels) || !bytes.Contains([]byte(row.GetCredential("antigravity_quota")), []byte("gemini-3.7-flash-tiered")) {
		t.Fatalf("response projection mutated durable raw facts: models=%v quota=%s", got, row.GetCredential("antigravity_quota"))
	}
}

func TestAntigravityAPIKeyAdminModelWritesPersistWireIDs(t *testing.T) {
	handler, db, _, _ := newImportGroupsTestHandler(t)
	handler.store = nil
	ctx := context.Background()

	create := httptest.NewRecorder()
	createContext, _ := gin.CreateTestContext(create)
	createContext.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/antigravity", strings.NewReader(`{
		"name":"public models",
		"auth_kind":"api_key",
		"api_key":"public-model-key",
		"models":["gemini-3.5-flash","gemini-3.7-flash"]
	}`))
	createContext.Request.Header.Set("Content-Type", "application/json")
	handler.AddAntigravityAccount(createContext)
	if create.Code != http.StatusOK {
		t.Fatalf("create response = %d %s", create.Code, create.Body.String())
	}
	var created struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(create.Body.Bytes(), &created); err != nil || created.ID <= 0 {
		t.Fatalf("create payload = %+v, err=%v", created, err)
	}
	row, err := db.GetAccountByID(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantCreated := []string{"gemini-3-flash-agent", "gemini-3.5-flash-extra-low", "gemini-3.5-flash-low", "gemini-3.7-flash-tiered"}
	if got := row.GetCredentialStringSlice("models"); !reflect.DeepEqual(got, wantCreated) {
		t.Fatalf("created persisted models = %v, want %v", got, wantCreated)
	}

	update := httptest.NewRecorder()
	updateContext, _ := gin.CreateTestContext(update)
	updateContext.Params = gin.Params{{Key: "id", Value: itoa(created.ID)}}
	updateContext.Request = httptest.NewRequest(http.MethodPatch, "/api/admin/accounts/"+itoa(created.ID)+"/antigravity", strings.NewReader(`{
		"models":["gemini-3.1-pro","gemini-3.6-flash"]
	}`))
	updateContext.Request.Header.Set("Content-Type", "application/json")
	handler.UpdateAntigravityAccount(updateContext)
	if update.Code != http.StatusOK {
		t.Fatalf("update response = %d %s", update.Code, update.Body.String())
	}
	row, err = db.GetAccountByID(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantUpdated := []string{"gemini-3.1-pro-low", "gemini-3.6-flash-high", "gemini-3.6-flash-low", "gemini-3.6-flash-medium", "gemini-pro-agent"}
	if got := row.GetCredentialStringSlice("models"); !reflect.DeepEqual(got, wantUpdated) {
		t.Fatalf("updated persisted models = %v, want %v", got, wantUpdated)
	}

	batch := httptest.NewRecorder()
	batchContext, _ := gin.CreateTestContext(batch)
	batchContext.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/antigravity/batch-models", strings.NewReader(`{
		"ids":[`+itoa(created.ID)+`],
		"models":["gemini-3.7-flash","claude-sonnet-4-6"]
	}`))
	batchContext.Request.Header.Set("Content-Type", "application/json")
	handler.BatchUpdateAntigravityModels(batchContext)
	if batch.Code != http.StatusOK || strings.Contains(batch.Body.String(), "gemini-3.7-flash-tiered") {
		t.Fatalf("batch response = %d %s", batch.Code, batch.Body.String())
	}
	var batchPayload struct {
		Models []string `json:"models"`
	}
	if err := json.Unmarshal(batch.Body.Bytes(), &batchPayload); err != nil {
		t.Fatal(err)
	}
	wantPublished := []string{"gemini-3.7-flash-low", "gemini-3.7-flash-medium", "gemini-3.7-flash-high", "claude-sonnet-4-6"}
	if !reflect.DeepEqual(batchPayload.Models, wantPublished) {
		t.Fatalf("batch public models = %v, want %v", batchPayload.Models, wantPublished)
	}
	row, err = db.GetAccountByID(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantBatchRaw := []string{"claude-sonnet-4-6", "gemini-3.7-flash-tiered"}
	if got := row.GetCredentialStringSlice("models"); !reflect.DeepEqual(got, wantBatchRaw) {
		t.Fatalf("batch persisted models = %v, want %v", got, wantBatchRaw)
	}
}

func TestFetchAntigravityModelsProjectsWireCatalog(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/antigravity/models", strings.NewReader(`{
		"models":["gemini-3.5-flash","gemini-3.7-flash","gemini-3.1-pro","claude-sonnet-4-6"]
	}`))
	c.Request.Header.Set("Content-Type", "application/json")
	(&Handler{}).FetchAntigravityModels(c)
	if recorder.Code != http.StatusOK {
		t.Fatalf("fetch response = %d %s", recorder.Code, recorder.Body.String())
	}
	for _, raw := range []string{"gemini-3.5-flash-extra-low", "gemini-3-flash-agent", "gemini-3.7-flash-tiered", "gemini-pro-agent"} {
		if strings.Contains(recorder.Body.String(), raw) {
			t.Fatalf("model selection leaked raw model %q: %s", raw, recorder.Body.String())
		}
	}
	for _, publicID := range []string{"gemini-3.5-flash-low", "gemini-3.5-flash-medium", "gemini-3.5-flash-high", "gemini-3.7-flash-low", "gemini-3.7-flash-medium", "gemini-3.7-flash-high", "gemini-3.1-pro-low", "gemini-3.1-pro-high", "claude-sonnet-4-6"} {
		if !strings.Contains(recorder.Body.String(), publicID) {
			t.Fatalf("model selection missing %q: %s", publicID, recorder.Body.String())
		}
	}
}
