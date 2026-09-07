package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestMergedAntigravityManifestReplacesStaleCapabilities(t *testing.T) {
	// A mixed upstream catalog can retain an old base model and effort aliases.
	// The gateway's allowed variants must win, without changing Grok metadata.
	body := []byte(`{"future":true,"models":[{"slug":"gemini-3.8-flash","default_reasoning_level":"xhigh","supported_reasoning_levels":[{"effort":"xhigh"}]},{"slug":"gemini-3.8-flash-low"},{"slug":"gemini-3.8-flash-high"},{"slug":"grok-4.5","supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"high"}]},{"slug":"grok-4.6","supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"high"},{"effort":"xhigh"}]}]}`)
	for _, allowed := range [][]string{{"low", "medium", "high"}, {"high"}} {
		var extras []api.Model
		for _, effort := range allowed {
			extras = append(extras, api.Model{ID: "gemini-3.8-flash-" + effort})
		}
		merged, err := mergeCodexManifestModels(body, extras)
		if err != nil {
			t.Fatal(err)
		}
		var root struct {
			Future bool                      `json:"future"`
			Models []scopedCodexManifestItem `json:"models"`
		}
		if err := json.Unmarshal(merged, &root); err != nil {
			t.Fatal(err)
		}
		if !root.Future || len(root.Models) != 3 {
			t.Fatalf("stale aliases or root metadata lost: %s", merged)
		}
		for _, model := range root.Models {
			var got []string
			for _, option := range model.SupportedReasoningLevels {
				got = append(got, option.Effort)
			}
			switch model.Slug {
			case "gemini-3.8-flash":
				if !reflect.DeepEqual(got, allowed) || model.DefaultReasoningLevel != allowed[0] {
					t.Fatalf("stale or unauthorized capabilities retained: %s", merged)
				}
			case "grok-4.5":
				if !reflect.DeepEqual(got, []string{"low", "medium", "high"}) {
					t.Fatal("changed Grok 4.5")
				}
			case "grok-4.6":
				if !reflect.DeepEqual(got, []string{"low", "medium", "high", "xhigh"}) {
					t.Fatal("changed Grok 4.6")
				}
			default:
				t.Fatalf("unexpected model: %s", model.Slug)
			}
		}
		if scopedCodexManifestETag(body) == scopedCodexManifestETag(merged) {
			t.Fatal("stale manifest validator retained")
		}
	}
}

func TestAntigravityNativeManifestContract(t *testing.T) {
	var models []api.Model
	for _, id := range AntigravityPublishedModelIDs([]string{"gemini-3.8-flash-tiered", "gemini-3.7-flash-tiered", "gemini-4-future", "claude-opus-4-6-thinking"}) {
		models = append(models, api.Model{ID: id, OwnedBy: "google"})
	}
	body, err := buildScopedCodexManifest(models)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Models []scopedCodexManifestItem `json:"models"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Models) != 4 {
		t.Fatalf("duplicate effort models: %s", body)
	}
	for _, model := range payload.Models {
		if model.Slug == "gemini-3.8-flash" && (len(model.InputModalities) != 1 || model.InputModalities[0] != "text") {
			t.Fatal("manifest advertises unsupported image inputs")
		}
		if model.Slug == "gemini-3.8-flash" || model.Slug == "gemini-3.7-flash" {
			if len(model.SupportedReasoningLevels) != 3 || model.DefaultReasoningLevel != "low" {
				t.Fatalf("invalid native reasoning contract: %+v", model)
			}
		} else if len(model.SupportedReasoningLevels) != 0 {
			t.Fatalf("invented future/non-Gemini controls: %+v", model)
		}
	}
	if path := os.Getenv("CODEX_MODEL_MANIFEST_FIXTURE_PATH"); path != "" {
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAntigravityDiscoveredModelPreservesActualWireID(t *testing.T) {
	account := &auth.Account{UpstreamType: auth.UpstreamAntigravity, AccessToken: "test-token", AntigravityProjectID: "test-project", Models: []string{"Future-Model-V4"}}
	models := AntigravityPublishedModelIDs(account.AntigravityModels())
	if len(models) != 1 || models[0] != "Future-Model-V4" {
		t.Fatalf("discovery changed actual model ID: %v", models)
	}
	wire, ok := antigravityResolvePublicModelForAccount(account, "future-model-v4")
	if !ok || wire != "Future-Model-V4" {
		t.Fatalf("wire ID = %q, supported = %v", wire, ok)
	}
	if _, ok := antigravityResolvePublicModelForAccount(account, "future-model-v5"); ok {
		t.Fatal("undiscovered model accepted")
	}
}

func TestAntigravityNativeEffortReachesWireAndHonorsKeyScope(t *testing.T) {
	requests := make(chan map[string]any, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		requests <- payload
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"response":{"candidates":[{"content":{"parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":2,"totalTokenCount":7}}}`)
	}))
	defer server.Close()
	previous := antigravityOAuthEndpointBases
	antigravityOAuthEndpointBases = []string{server.URL}
	t.Cleanup(func() { antigravityOAuthEndpointBases = previous })
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 4})
	store.AddAccount(&auth.Account{DBID: 3810, UpstreamType: auth.UpstreamAntigravity, AccessToken: "test-token", AntigravityProjectID: "test-project", Models: []string{"gemini-3.8-flash-tiered"}, HealthTier: auth.HealthTierHealthy, Status: auth.StatusReady})
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	for _, channel := range []string{database.UpstreamChannelAntigravity, database.UpstreamChannelAuto} {
		for _, tc := range []struct {
			model, effort, want string
			allow               []string
			status              int
		}{
			{"gemini-3.8-flash", "", "LOW", nil, 200},
			{"gemini-3.8-flash", "medium", "MEDIUM", nil, 200},
			{"gemini-3.8-flash", "high", "HIGH", nil, 200},
			{"gemini-3.8-flash", "xhigh", "", nil, 400},
			{"gemini-3.8-flash-high", "low", "HIGH", nil, 200},
			{"gemini-3.8-flash", "high", "", []string{"gemini-3.8-flash-low"}, 403},
			{"gemini-3.8-flash", "low", "LOW", []string{"gemini-3.8-flash-low"}, 200},
		} {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"`+tc.model+`","input":"hi","stream":false,"reasoning":{"effort":"`+tc.effort+`"}}`))
			c.Set(contextAPIKeyRow, &database.APIKeyRow{ID: 3810, Limits: database.APIKeyLimits{UpstreamChannel: channel, ModelAllow: tc.allow}})
			handler.Responses(c)
			if rec.Code != tc.status {
				t.Fatalf("%s/%s status=%d body=%s", tc.model, tc.effort, rec.Code, rec.Body.String())
			}
			if tc.status == 200 {
				payload := <-requests
				thinking := payload["request"].(map[string]any)["generationConfig"].(map[string]any)["thinkingConfig"].(map[string]any)
				if payload["model"] != "gemini-3.8-flash-tiered" || thinking["thinkingLevel"] != tc.want {
					t.Fatalf("actual upstream payload = %v", payload)
				}
			} else if len(requests) != 0 {
				t.Fatal("denied effort reached upstream")
			}
		}
	}
}
