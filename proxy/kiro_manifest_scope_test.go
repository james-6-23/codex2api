package proxy

import (
	"context"
	"testing"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
)

func TestKiroImageManifestSupportsAllAvailableModels(t *testing.T) {
	for _, model := range []string{"claude-sonnet-4.6", "gpt-5.6-luna", "future-image-model", "glm-5"} {
		for _, reverse := range []bool{false, true} {
			store := auth.NewStore(nil, nil, nil)
			t.Cleanup(store.Stop)
			vision := &auth.Account{DBID: 910001, UpstreamType: auth.UpstreamKiro, AccessToken: "at", Models: []string{model}, Kiro: auth.KiroCredential{ImageModels: []string{model}}}
			textOnly := &auth.Account{DBID: 910002, UpstreamType: auth.UpstreamKiro, AccessToken: "at", Models: []string{model}}
			if reverse {
				vision.DBID, textOnly.DBID = textOnly.DBID, vision.DBID
			}
			store.AddAccount(vision)
			store.AddAccount(textOnly)
			handler := &Handler{store: store, db: newTestModelRegistryDB(t)}
			key := &database.APIKeyRow{ID: 1}
			check := func(want string) {
				t.Helper()
				for _, models := range [][]api.Model{handler.scopedModels(context.Background(), key), handler.extraRelayManifestModels(context.Background(), key)} {
					body, err := buildScopedCodexManifest(models)
					if err != nil {
						t.Fatal(err)
					}
					for _, live := range []bool{false, true} {
						result := handler.applyModelCapabilities(context.Background(), key, body, live)
						if got := gjson.GetBytes(result, "models.0.input_modalities").Raw; got != want {
							t.Fatalf("%s reverse=%v live=%v got %s want %s", model, reverse, live, got, want)
						}
					}
				}
			}
			check(`["text","image"]`)
			textOnly.SetAllowedAPIKeyIDs([]int64{2})
			check(`["text","image"]`)
		}
	}
}

func TestKiroMappedImageManifestWithReadableCapabilityStore(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	t.Cleanup(store.Stop)
	account := &auth.Account{DBID: 910003, UpstreamType: auth.UpstreamKiro, AccessToken: "at", Models: []string{"gpt-5.6-luna", "gpt-5.6-sol"},
		ModelMapping: `{"gpt-5.6-luna":"gpt-5.6-sol"}`, Kiro: auth.KiroCredential{ImageModels: []string{"gpt-5.6-sol"}}}
	store.AddAccount(account)
	handler := &Handler{store: store, db: newTestModelRegistryDB(t)}
	key := &database.APIKeyRow{ID: 1}
	if target, ok := resolveAccountModelMapping(account, "gpt-5.6-luna"); !ok || target != "gpt-5.6-sol" {
		t.Fatal("fixture must map luna to sol")
	}
	for _, models := range [][]api.Model{handler.scopedModels(context.Background(), key), handler.extraRelayManifestModels(context.Background(), key)} {
		body, err := buildScopedCodexManifest(models)
		if err != nil {
			t.Fatal(err)
		}
		for _, live := range []bool{false, true} {
			result := handler.applyModelCapabilities(context.Background(), key, body, live)
			for _, item := range gjson.GetBytes(result, "models").Array() {
				if item.Get("input_modalities").Raw != `["text","image"]` || !item.Get("supports_parallel_tool_calls").Bool() {
					t.Fatalf("live=%v lost Kiro capabilities: %s", live, item.Raw)
				}
			}
		}
	}
}
