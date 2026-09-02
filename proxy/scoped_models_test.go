package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

type scopedModelJSON struct {
	ID      string `json:"id"`
	Owner   string `json:"owned_by"`
	Created int64  `json:"created"`
}

func scopedModelByID(models []scopedModelJSON, id string) (string, int64, bool) {
	for _, model := range models {
		if model.ID == id {
			return model.Owner, model.Created, true
		}
	}
	return "", 0, false
}

func listScopedModelsForTest(t *testing.T, handler *Handler, row *database.APIKeyRow) []scopedModelJSON {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	c.Set(contextAPIKeyRow, row)
	handler.ListModels(c)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Data []scopedModelJSON `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode models: %v", err)
	}
	return payload.Data
}

func TestScopedModelsAppliesKeyAccountAndHardGrokGates(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	allowed := &auth.Account{DBID: 1, UpstreamType: auth.UpstreamGrok, APIKey: "xai-1", Models: []string{"grok-allowed"}, GroupIDs: []int64{10}}
	allowed.SetGrokRoutingState(auth.GrokRoutingState{Models: []auth.GrokModelRoute{{ModelID: "grok-allowed", APIBackend: auth.GrokProtocolResponses}}})
	allowed.SetAllowedAPIKeyIDs([]int64{77})
	blockedAccess := &auth.Account{DBID: 2, UpstreamType: auth.UpstreamGrok, APIKey: "xai-2", Models: []string{"grok-blocked"}, GroupIDs: []int64{10}, CredentialGeneration: 1, GrokFactsGeneration: 1}
	blockedAccess.SetGrokRoutingState(auth.GrokRoutingState{Models: []auth.GrokModelRoute{{ModelID: "grok-blocked", APIBackend: auth.GrokProtocolResponses}}})
	deny := false
	blockedAccess.GrokAccessAllowed = &deny
	blockedAccess.GrokAccessExpiresAt = time.Now().Add(time.Minute)
	otherGroup := &auth.Account{DBID: 3, UpstreamType: auth.UpstreamGrok, APIKey: "xai-3", Models: []string{"grok-other"}, GroupIDs: []int64{20}}
	store.AddAccount(allowed)
	store.AddAccount(blockedAccess)
	store.AddAccount(otherGroup)
	handler := NewHandler(store, nil, nil, nil)
	row := &database.APIKeyRow{ID: 77, AllowedGroupIDs: []int64{10}, Limits: database.APIKeyLimits{ModelAllow: []string{"grok-allowed", "grok-blocked", "grok-other"}}}

	models := listScopedModelsForTest(t, handler, row)
	if len(models) != 1 || models[0].ID != "grok-allowed" || models[0].Owner != "xai" {
		t.Fatalf("models = %+v, want only xai grok-allowed", models)
	}
	if allowed.ActiveRequests != 0 || allowed.TotalRequests != 0 {
		t.Fatalf("model listing reserved account: active=%d total=%d", allowed.ActiveRequests, allowed.TotalRequests)
	}
}

func TestScopedModelsIgnoresTransientCooldownButHonorsPaused(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
	cooled := &auth.Account{DBID: 1, UpstreamType: auth.UpstreamGrok, APIKey: "xai-1", Models: []string{"grok-4.5"}}
	cooled.SetCooldownWithReason(time.Minute, "rate_limited")
	paused := &auth.Account{DBID: 2, UpstreamType: auth.UpstreamGrok, APIKey: "xai-2", Models: []string{"grok-paused"}}
	atomic.StoreInt32(&paused.DispatchPaused, 1)
	store.AddAccount(cooled)
	store.AddAccount(paused)
	handler := NewHandler(store, nil, nil, nil)
	models := listScopedModelsForTest(t, handler, &database.APIKeyRow{ID: 1})
	// cooled 账号(短冷却)保留展示,paused 账号整号隐藏;媒体模型集随账号可见性
	// 一起出现/消失,这里只校验文本模型。
	textModels := make([]string, 0, len(models))
	for _, model := range models {
		if !isGrokMediaModel(model.ID) {
			textModels = append(textModels, model.ID)
		}
	}
	if len(textModels) != 1 || textModels[0] != "grok-4.5" {
		t.Fatalf("text models = %v, want stable cooled model only", textModels)
	}
}

func TestScopedModelsPlanMissingFailsClosedAndEmptyIs200(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
	store.AddAccount(&auth.Account{DBID: 1, UpstreamType: auth.UpstreamGrok, AccessToken: "at", RefreshToken: "rt", Models: []string{"grok-4.5"}, PlanType: "free", CredentialGeneration: 1})
	handler := NewHandler(store, nil, nil, nil)
	models := listScopedModelsForTest(t, handler, &database.APIKeyRow{ID: 1, Limits: database.APIKeyLimits{PlanAllow: []string{"free"}}})
	if len(models) != 0 {
		t.Fatalf("archive plan leaked through live plan gate: %+v", models)
	}
}

func TestScopedModelsAliasesOwnersFirstSeenAndDeterministicOrder(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	firstSeen := time.Unix(1_765_432_100, 0).UTC()
	grok := &auth.Account{DBID: 2, UpstreamType: auth.UpstreamGrok, APIKey: "xai", Models: []string{"gpt-5.4", "grok-only"}, ModelMapping: `{"grok-alias":"grok-only","bad-alias":"missing"}`}
	grok.SetGrokRoutingState(auth.GrokRoutingState{Models: []auth.GrokModelRoute{
		{ModelID: "gpt-5.4", APIBackend: auth.GrokProtocolResponses, FirstSeenAt: firstSeen},
		{ModelID: "grok-only", APIBackend: auth.GrokProtocolResponses, FirstSeenAt: firstSeen},
	}})
	codex := &auth.Account{DBID: 1, AccessToken: "codex", Models: []string{"gpt-5.4"}}
	store.AddAccount(grok)
	store.AddAccount(codex)
	store.SetCodexModelMapping(`{"global-alias":"gpt-5.4","dead-global":"missing"}`)
	handler := NewHandler(store, nil, nil, nil)
	models := listScopedModelsForTest(t, handler, &database.APIKeyRow{ID: 9})

	ids := make([]string, 0, len(models))
	for _, model := range models {
		ids = append(ids, model.ID)
	}
	if !slices.IsSorted(ids) {
		t.Fatalf("models are not deterministic sorted: %v", ids)
	}
	if owner, _, ok := scopedModelByID(models, "gpt-5.4"); !ok || owner != "codex2api" {
		t.Fatalf("shared owner = %q ok=%v, want codex2api", owner, ok)
	}
	if owner, created, ok := scopedModelByID(models, "grok-only"); !ok || owner != "xai" || created != firstSeen.Unix() {
		t.Fatalf("grok-only owner=%q created=%d ok=%v", owner, created, ok)
	}
	for _, alias := range []string{"grok-alias", "global-alias"} {
		if owner, _, ok := scopedModelByID(models, alias); !ok || owner != "codex2api" {
			t.Fatalf("alias %s owner=%q ok=%v", alias, owner, ok)
		}
	}
	for _, absent := range []string{"bad-alias", "dead-global"} {
		if _, _, ok := scopedModelByID(models, absent); ok {
			t.Fatalf("unrouteable alias %s was exposed", absent)
		}
	}
}

func TestFilterCodexManifestRetainsUnknownFieldsAndStableETag(t *testing.T) {
	body := []byte(`{"models":[{"slug":"gpt-a","display_name":"A"},{"slug":"gpt-b","new_field":{"x":1}}],"future":true}`)
	filtered, etag, err := filterCodexManifest(body, `W/"upstream"`, func(slug string) bool { return slug == "gpt-b" })
	if err != nil {
		t.Fatalf("filterCodexManifest: %v", err)
	}
	if etag == "" {
		t.Fatal("gateway ETag is empty")
	}
	var payload struct {
		Models []map[string]any `json:"models"`
		Future bool             `json:"future"`
	}
	if err := json.Unmarshal(filtered, &payload); err != nil {
		t.Fatal(err)
	}
	if !payload.Future || len(payload.Models) != 1 || payload.Models[0]["slug"] != "gpt-b" || payload.Models[0]["new_field"] == nil {
		t.Fatalf("filtered payload lost schema fields: %s", filtered)
	}
	filteredAgain, etagAgain, err := filterCodexManifest(body, `W/"upstream"`, func(slug string) bool { return slug == "gpt-b" })
	if err != nil || string(filteredAgain) != string(filtered) || etagAgain != etag {
		t.Fatalf("filtered manifest is unstable: err=%v etag=%q/%q", err, etag, etagAgain)
	}
}

func TestFilterCodexManifestUnknownSchemaFailsClosed(t *testing.T) {
	for _, body := range [][]byte{[]byte(`{"data":[]}`), []byte(`{"models":[{"id":"gpt-a"}]}`), []byte(`not json`)} {
		if _, _, err := filterCodexManifest(body, "", func(string) bool { return true }); err == nil {
			t.Fatalf("filterCodexManifest(%s) accepted unsafe schema", body)
		}
	}
}

func TestScopedCodexManifestAccountHonorsGrokOnlyChannel(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "codex"})
	store.AddAccount(&auth.Account{DBID: 2, UpstreamType: auth.UpstreamGrok, APIKey: "xai"})
	handler := NewHandler(store, nil, nil, nil)
	row := &database.APIKeyRow{ID: 11, Limits: database.APIKeyLimits{UpstreamChannel: database.UpstreamChannelGrok}}
	if account := handler.scopedCodexManifestAccount(row); account != nil {
		t.Fatalf("grok-only key selected codex manifest account %d", account.DBID)
	}
}

func TestScopedModelsModelAllowAppliesToAliasName(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
	store.AddAccount(&auth.Account{DBID: 1, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: "https://relay.example", APIKey: "sk", Models: []string{"target"}, ModelMapping: `{"public-alias":"target"}`})
	handler := NewHandler(store, nil, nil, nil)
	models := listScopedModelsForTest(t, handler, &database.APIKeyRow{ID: 1, Limits: database.APIKeyLimits{ModelAllow: []string{"public-alias"}}})
	if len(models) != 1 || models[0].ID != "public-alias" || models[0].Owner != "codex2api" {
		t.Fatalf("models = %+v, want alias only", models)
	}
}

func TestScopedModelRecordsDoesNotNeedDatabase(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
	store.AddAccount(&auth.Account{DBID: 1, UpstreamType: auth.UpstreamGrok, APIKey: "xai"})
	handler := NewHandler(store, nil, nil, nil)
	if got := handler.scopedModelRecords(context.Background(), &database.APIKeyRow{ID: 1}); len(got) == 0 {
		t.Fatal("conservative Grok defaults missing")
	}
}

func TestScopedModelsIncludeAntigravityAccounts(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	store.AddAccount(&auth.Account{
		DBID: 99, UpstreamType: auth.UpstreamAntigravity, AccessToken: "google-token", AntigravityProjectID: "project-id",
		Models: []string{"gemini-3.7-flash-tiered"},
	})
	handler := NewHandler(store, nil, nil, nil)
	models := listScopedModelsForTest(t, handler, &database.APIKeyRow{ID: 7})
	found := map[string]bool{}
	for _, model := range models {
		found[model.ID] = true
	}
	if !found["gemini-3.7-flash-low"] || !found["gemini-3.7-flash-medium"] || !found["gemini-3.7-flash-high"] {
		t.Fatal("projected Antigravity public model missing from /v1/models")
	}
}

func TestScopedModelsClaudeOnlyKeyDoesNotExposeCodexAliases(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	defer store.Stop()
	store.SetModelMapping(`{"client-alias":"claude-sonnet-4-5"}`)
	store.AddAccount(&auth.Account{
		DBID: 100, UpstreamType: auth.UpstreamClaude, AccessToken: "claude-token", Status: auth.StatusReady,
		Models: []string{"claude-sonnet-4-5"},
	})
	handler := NewHandler(store, nil, nil, nil)
	models := listScopedModelsForTest(t, handler, &database.APIKeyRow{ID: 8, Limits: database.APIKeyLimits{UpstreamChannel: database.UpstreamChannelClaude}})
	if _, _, ok := scopedModelByID(models, "claude-sonnet-4-5"); !ok {
		t.Fatalf("Claude native model missing from Claude-only catalog: %+v", models)
	}
	if _, _, ok := scopedModelByID(models, "client-alias"); ok {
		t.Fatalf("Codex/global alias leaked into Claude-only catalog: %+v", models)
	}
}

func TestScopedModelsDeclaredListCannotOverrideCatalogVisibility(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
	account := &auth.Account{DBID: 1, UpstreamType: auth.UpstreamGrok, APIKey: "xai", Models: []string{"declared-only", "hidden", "visible"}}
	account.SetGrokRoutingState(auth.GrokRoutingState{Models: []auth.GrokModelRoute{
		{ModelID: "hidden", APIBackend: auth.GrokProtocolResponses, Hidden: true},
		{ModelID: "visible", APIBackend: auth.GrokProtocolResponses},
	}})
	store.AddAccount(account)
	handler := NewHandler(store, nil, nil, nil)
	models := listScopedModelsForTest(t, handler, &database.APIKeyRow{ID: 1})
	// 媒体模型是独立能力轴(不在文本目录里),纯文本白名单不关闭它们;
	// 这里只校验文本模型:目录外/隐藏条目不得被白名单复活。
	textModels := make([]string, 0, len(models))
	for _, model := range models {
		if !isGrokMediaModel(model.ID) {
			textModels = append(textModels, model.ID)
		}
	}
	if len(textModels) != 1 || textModels[0] != "visible" {
		t.Fatalf("text models = %v, explicit config must only narrow visible catalog", textModels)
	}
	if relayAccountSupportsModel(account, "declared-only") || relayAccountSupportsModel(account, "hidden") {
		t.Fatal("request admission bypassed catalog visibility")
	}
}
