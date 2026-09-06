package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestAntigravityBothModelListFormatsExposeOneChoice(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 4})
	store.AddAccount(&auth.Account{DBID: 3800, UpstreamType: auth.UpstreamAntigravity, AccessToken: "test-token", AntigravityProjectID: "test-project", Models: []string{"gemini-3.8-flash-tiered"}, HealthTier: auth.HealthTierHealthy, Status: auth.StatusReady})
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	for _, channel := range []string{database.UpstreamChannelAntigravity, database.UpstreamChannelAuto} {
		for _, scope := range []struct {
			allow []string
			want  []string
		}{
			{nil, []string{"low", "medium", "high"}},
			{[]string{"gemini-3.8-flash-low"}, []string{"low"}},
			{[]string{"gemini-3.8-flash-high"}, []string{"high"}},
		} {
			row := &database.APIKeyRow{ID: 3800, Limits: database.APIKeyLimits{UpstreamChannel: channel, ModelAllow: scope.allow}}
			for _, url := range []string{"/v1/models", "/v1/models?client_version=0.153.0"} {
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodGet, url, nil)
				c.Set(contextAPIKeyRow, row)
				handler.listModelsOrManifest(c)
				if recorder.Code != 200 {
					t.Fatalf("%s: %d %s", url, recorder.Code, recorder.Body.String())
				}
				var payload struct {
					Data   []api.Model               `json:"data"`
					Models []scopedCodexManifestItem `json:"models"`
				}
				if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
					t.Fatal(err)
				}
				var id, defaultLevel string
				var levels []api.ModelReasoningLevel
				if url == "/v1/models" {
					if len(payload.Data) != 1 {
						t.Fatalf("duplicate ordinary choices: %s", recorder.Body.String())
					}
					id, defaultLevel, levels = payload.Data[0].ID, payload.Data[0].DefaultReasoningLevel, payload.Data[0].SupportedReasoningLevels
				} else {
					if len(payload.Models) != 1 {
						t.Fatalf("duplicate native choices: %s", recorder.Body.String())
					}
					id, defaultLevel, levels = payload.Models[0].Slug, payload.Models[0].DefaultReasoningLevel, payload.Models[0].SupportedReasoningLevels
				}
				var actual []string
				for _, level := range levels {
					actual = append(actual, level.Effort)
				}
				if id != "gemini-3.8-flash" || defaultLevel != scope.want[0] || !reflect.DeepEqual(actual, scope.want) {
					t.Fatalf("%s: id=%s default=%s efforts=%v", url, id, defaultLevel, actual)
				}
			}
		}
	}
}

func TestAntigravityModelChoicesKeepUnrelatedModels(t *testing.T) {
	models := []api.Model{{ID: "gemini-3.8-flash-high"}, {ID: "gemini-3.8-flash-low"}, {ID: "gemini-3.8-flash-medium"}, {ID: "gemini-3.8-flash-lite"}, {ID: "gemini-Future-high"}, {ID: "gpt-oss-120b-medium"}}
	choices := collapseAntigravityModelChoices(models)
	var ids []string
	for _, choice := range choices {
		ids = append(ids, choice.ID)
	}
	if !reflect.DeepEqual(ids, []string{"gemini-3.8-flash", "gemini-3.8-flash-lite", "gemini-Future-high", "gpt-oss-120b-medium"}) {
		t.Fatalf("incorrect grouping: %v", ids)
	}
	if models[0].ID != "gemini-3.8-flash-high" {
		t.Fatal("advertising changed routing catalog")
	}
}

func TestAntigravityPublishedChoicesExcludeInternalAndDuplicateBackings(t *testing.T) {
	raw := []string{"chat_20706", "chat_23310", "tab_flash_lite_preview", "tab_jump_flash_lite_preview", "gemini-3.6-flash-tiered", "gemini-3.6-flash-low", "gemini-3.6-flash-medium", "gemini-3.6-flash-high", "gemini-3.8-flash-tiered", "gemini-3.1-flash-lite", "gemini-2.5-flash", "gemini-2.5-flash-lite", "gemini-2.5-flash-thinking", "gemini-Future-V4", "gemini-2.5-pro"}
	var records []api.Model
	for _, id := range AntigravityPublishedModelIDs(raw) {
		records = append(records, api.Model{ID: id})
	}
	got := map[string]bool{}
	for _, model := range collapseAntigravityModelChoices(records) {
		got[model.ID] = true
	}
	want := map[string]bool{"gemini-3.6-flash": true, "gemini-3.8-flash": true, "gemini-3.1-flash-lite": true, "gemini-Future-V4": true, "gemini-2.5-pro": true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("chat catalog = %v, want %v", got, want)
	}
}
