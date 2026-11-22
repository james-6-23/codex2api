package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
)

func TestCodexCapabilityValidationAndIntersection(t *testing.T) {
	a := parseCodexModelCapabilities([]byte(`{"models":[{"slug":"gpt-6-astra","context_window":1000,"supports_search_tool":true,"use_responses_lite":true,"experimental_supported_tools":["function","web_search"],"supported_reasoning_levels":[{"effort":"low"},{"effort":"max"}],"base_instructions":"never persist","tool_mode":{"invalid":true}}]}`))
	b := parseCodexModelCapabilities([]byte(`{"models":[{"slug":"gpt-6-astra","context_window":800,"supports_search_tool":false,"use_responses_lite":true,"experimental_supported_tools":["function","web_search","computer"],"supported_reasoning_levels":["low","high"]}]}`))
	if len(a) != 1 || a["gpt-6-astra"]["base_instructions"] != nil || a["gpt-6-astra"]["tool_mode"] != nil {
		t.Fatalf("unsafe descriptor fields admitted: %+v", a)
	}
	got := intersectCodexCapabilities([]map[string]json.RawMessage{a["gpt-6-astra"], b["gpt-6-astra"]})
	if string(got["context_window"]) != "800" || string(got["supports_search_tool"]) != "false" || string(got["use_responses_lite"]) != "true" || string(got["experimental_supported_tools"]) != `["function","web_search"]` || gjson.GetBytes(got["supported_reasoning_levels"], "0.effort").String() != "low" {
		t.Fatalf("unsafe intersection: %+v", got)
	}
	got = intersectCodexCapabilities([]map[string]json.RawMessage{a["gpt-6-astra"], nil})
	if string(got["use_responses_lite"]) != "false" || string(got["context_window"]) != "null" {
		t.Fatalf("unknown account inherited capabilities: %+v", got)
	}
	for _, body := range []string{`{}`, `{"models":[]}`, `invalid`} {
		if len(parseCodexModelCapabilities([]byte(body))) != 0 {
			t.Fatal("invalid listing changed snapshot")
		}
	}
}

func TestCodexManifestSaveCapabilitiesUsesObservedGeneration(t *testing.T) {
	db := newTestModelRegistryDB(t)
	ctx := context.Background()
	id, err := db.InsertAccount(ctx, "caps", "sync-refresh", "")
	if err != nil {
		t.Fatal(err)
	}
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	account := &auth.Account{DBID: id, CredentialGeneration: row.CredentialGeneration, AccessToken: "test-only", PlanType: "plus"}
	manifest := &CodexModelsManifest{
		Body:              []byte(`{"models":[{"slug":"gpt-6.1-sol","context_window":500000,"max_context_window":872000}]}`),
		capabilityAccount: account,
		capabilitySnapshot: database.ModelCapabilitySnapshot{
			AccountID: id, CredentialGeneration: row.CredentialGeneration, ObservedAt: time.Now().UnixNano(),
		},
	}
	if err := manifest.SaveCapabilities(ctx, db); err != nil {
		t.Fatal(err)
	}
	snapshots, err := db.ListModelCapabilities(ctx, []int64{id})
	if err != nil {
		t.Fatal(err)
	}
	if got := string(snapshots[id].Models["gpt-6.1-sol"]["context_window"]); got != "500000" {
		t.Fatalf("context window = %q, want 500000", got)
	}

	account.CredentialGeneration++
	if err := manifest.SaveCapabilities(ctx, db); err == nil {
		t.Fatal("stale manifest was accepted after credential refresh")
	}
}

func TestStoredModelCapabilitiesRestoreAndScope(t *testing.T) {
	db := newTestModelRegistryDB(t)
	ctx := context.Background()
	store := auth.NewStore(nil, nil, nil)
	for i := 0; i < 2; i++ {
		id, err := db.InsertAccount(ctx, "caps", fmt.Sprintf("refresh-%d", i), "")
		if err != nil {
			t.Fatal(err)
		}
		row, err := db.GetAccountByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		account := &auth.Account{DBID: id, CredentialGeneration: row.CredentialGeneration, AccessToken: fmt.Sprintf("test-%d", id), PlanType: "plus"}
		if i == 1 {
			account.SetAllowedAPIKeyIDs([]int64{2})
		}
		store.AddAccount(account)
		value := json.RawMessage("true")
		contextSize := json.RawMessage("1000")
		if i == 1 {
			value = json.RawMessage("false")
			contextSize = json.RawMessage("800")
		}
		err = db.SaveModelCapabilities(ctx, database.ModelCapabilitySnapshot{AccountID: id, CredentialGeneration: row.CredentialGeneration, ObservedAt: time.Now().UnixNano(), Models: map[string]map[string]json.RawMessage{"gpt-5.6-sol": {"use_responses_lite": value, "supports_search_tool": value, "context_window": contextSize}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	h := NewHandler(store, db, nil, nil)
	for _, account := range store.Accounts() {
		if _, known := account.ModelSupportsResponsesLite("gpt-5.6-sol"); !known {
			t.Fatal("runtime capabilities not restored")
		}
	}
	body, err := buildScopedCodexManifest([]api.Model{{ID: "gpt-5.6-sol", OwnedBy: "openai"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		key    int64
		lite   bool
		window int64
	}{{1, true, 1000}, {2, false, 800}} {
		got := h.applyStoredModelCapabilities(ctx, &database.APIKeyRow{ID: tc.key}, body)
		if gjson.GetBytes(got, "models.0.use_responses_lite").Bool() != tc.lite || gjson.GetBytes(got, "models.0.context_window").Int() != tc.window {
			t.Fatalf("scope %d manifest: %s", tc.key, got)
		}
	}
}

func TestCodexCapabilityInvalidModalityDefaultsToText(t *testing.T) {
	var handler *Handler
	unknownModalities := handler.applyUpstreamModelCapabilities(context.Background(), &database.APIKeyRow{ID: 1}, []byte(`{"models":[{"slug":"gpt-6-astra","input_modalities":["video"]}]}`))
	if got := gjson.GetBytes(unknownModalities, "models.0.input_modalities").Raw; got != `["text"]` {
		t.Fatalf("invalid modalities restored an implicit image capability: %s", unknownModalities)
	}
}

func TestLiveCapabilitiesRespectRelayTransportAndVisibility(t *testing.T) {
	for _, observed := range []bool{false, true} {
		for _, transport := range []string{auth.OpenAIResponsesTransportHTTP, auth.OpenAIResponsesTransportWebsocket} {
			t.Run(fmt.Sprintf("%s/observed=%v", transport, observed), func(t *testing.T) {
				db := newTestModelRegistryDB(t)
				ctx := context.Background()
				store := auth.NewStore(nil, nil, nil)
				store.AddAccount(&auth.Account{DBID: 100, AccessToken: "codex-test", PlanType: "plus"})
				id, err := db.InsertOpenAIResponsesAccount(ctx, "relay", map[string]interface{}{"api_key": "test-token", "base_url": "https://relay.example", "models": []string{"gpt-6-astra"}}, "")
				if err != nil {
					t.Fatal(err)
				}
				row, err := db.GetAccountByID(ctx, id)
				if err != nil {
					t.Fatal(err)
				}
				relay := &auth.Account{DBID: id, CredentialGeneration: row.CredentialGeneration, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: "https://relay.example", APIKey: "test-token", Models: []string{"gpt-6-astra"}, ResponsesUpstreamTransport: transport}
				relay.SetAllowedAPIKeyIDs([]int64{2})
				store.AddAccount(relay)
				source := []byte(`{"models":[{"slug":"gpt-6-astra","prefer_websockets":true,"context_window":1000,"use_responses_lite":true}]}`)
				if observed {
					if err := db.SaveModelCapabilities(ctx, database.ModelCapabilitySnapshot{AccountID: id, CredentialGeneration: row.CredentialGeneration, ObservedAt: 1, Models: parseCodexModelCapabilities(source)}); err != nil {
						t.Fatal(err)
					}
				}
				h := NewHandler(store, db, nil, nil)
				for _, key := range []int64{1, 2} {
					result := h.applyUpstreamModelCapabilities(ctx, &database.APIKeyRow{ID: key}, source)
					wantWS := key == 1 || transport == auth.OpenAIResponsesTransportWebsocket
					if gjson.GetBytes(result, "models.0.prefer_websockets").Bool() != wantWS ||
						gjson.GetBytes(result, "models.0.context_window").Int() != 1000 || !gjson.GetBytes(result, "models.0.use_responses_lite").Bool() {
						t.Fatalf("key=%d want websocket=%v; descriptor=%s", key, wantWS, result)
					}
				}
			})
		}
	}
}

func TestCodexCapabilityStringListsRejectOpaqueAndUnboundedValues(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		valid bool
	}{
		{"tools", `["function","web_search"]`, true},
		{"empty", `[]`, true},
		{"object", `[{"id":"function","description":"provider instructions"}]`, false},
		{"number", `[1]`, false},
		{"empty name", `[""]`, false},
		{"whitespace name", `["  "]`, false},
		{"control", `["web\nsearch"]`, false},
		{"oversize name", `["` + strings.Repeat("a", 513) + `"]`, false},
		{"oversize list", `[` + strings.TrimSuffix(strings.Repeat(`"function",`, 33), ",") + `]`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			models := parseCodexModelCapabilities([]byte(`{"models":[{"slug":"gpt-6-astra","experimental_supported_tools":` + tc.value + `,"base_instructions":"private instructions","access_token":"private credential"}]}`))
			fields := models["gpt-6-astra"]
			if got := fields["experimental_supported_tools"] != nil; got != tc.valid {
				t.Fatalf("admitted = %v, want %v", got, tc.valid)
			}
			encoded, err := json.Marshal(fields)
			if err != nil || strings.Contains(string(encoded), "private") || fields["base_instructions"] != nil || fields["access_token"] != nil {
				t.Fatalf("opaque descriptor data admitted: %s, err=%v", encoded, err)
			}
		})
	}
	upper := parseCodexModelCapabilities([]byte(`{"models":[{"slug":" GPT-6-ASTRA ","experimental_supported_tools":["function","Web_Search"]}]}`))
	lower := parseCodexModelCapabilities([]byte(`{"models":[{"slug":"gpt-6-astra","experimental_supported_tools":["function","web_search"]}]}`))
	got := intersectCodexCapabilities([]map[string]json.RawMessage{upper["gpt-6-astra"], lower["gpt-6-astra"]})
	if string(got["experimental_supported_tools"]) != `["function"]` {
		t.Fatalf("tool identifiers must match exactly: %s", got["experimental_supported_tools"])
	}
	for _, tc := range []struct {
		value string
		valid bool
	}{
		{`["text","image","audio"]`, true},
		{`["text","video"]`, false},
		{`["Text"]`, false},
		{`[{"id":"text"}]`, false},
	} {
		models := parseCodexModelCapabilities([]byte(`{"models":[{"slug":"gpt-6-astra","input_modalities":` + tc.value + `}]}`))
		if got := models["gpt-6-astra"]["input_modalities"] != nil; got != tc.valid {
			t.Fatalf("modality validation %s = %v, want %v", tc.value, got, tc.valid)
		}
	}
}

func TestStoredCapabilitiesUnknownAndCredentialGenerationAreConservative(t *testing.T) {
	db := newTestModelRegistryDB(t)
	ctx := context.Background()
	store := auth.NewStore(nil, nil, nil)
	var known *auth.Account
	for i := 0; i < 2; i++ {
		id, err := db.InsertAccount(ctx, "caps", fmt.Sprintf("cap-refresh-%d", i), "")
		if err != nil {
			t.Fatal(err)
		}
		row, err := db.GetAccountByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		account := &auth.Account{DBID: id, CredentialGeneration: row.CredentialGeneration, AccessToken: "test-only", PlanType: "plus"}
		if i == 0 {
			known = account
			models := parseCodexModelCapabilities([]byte(`{"models":[{"slug":"gpt-6-astra","use_responses_lite":true,"prefer_websockets":true,"experimental_supported_tools":["function","web_search"],"context_window":1000}]}`))
			if err := db.SaveModelCapabilities(ctx, database.ModelCapabilitySnapshot{AccountID: id, CredentialGeneration: row.CredentialGeneration, ObservedAt: 1, Models: models}); err != nil {
				t.Fatal(err)
			}
		} else {
			account.SetAllowedAPIKeyIDs([]int64{2})
		}
		store.AddAccount(account)
	}
	h := NewHandler(store, db, nil, nil)
	body, err := buildScopedCodexManifest([]api.Model{{ID: "gpt-6-astra"}})
	if err != nil {
		t.Fatal(err)
	}
	assertUnknown := func(t *testing.T, body []byte) {
		t.Helper()
		if gjson.GetBytes(body, "models.0.use_responses_lite").Bool() || gjson.GetBytes(body, "models.0.prefer_websockets").Bool() || gjson.GetBytes(body, "models.0.context_window").Exists() || gjson.GetBytes(body, "models.0.experimental_supported_tools").Raw != "[]" {
			t.Fatalf("unknown credentials inherited capabilities: %s", body)
		}
	}
	visible := h.applyStoredModelCapabilities(ctx, &database.APIKeyRow{ID: 1}, body)
	if !gjson.GetBytes(visible, "models.0.use_responses_lite").Bool() || gjson.GetBytes(visible, "models.0.experimental_supported_tools").Raw != `["function","web_search"]` {
		t.Fatalf("known scoped account lost declarations: %s", visible)
	}
	t.Run("unknown visible account", func(t *testing.T) {
		assertUnknown(t, h.applyStoredModelCapabilities(ctx, &database.APIKeyRow{ID: 2}, body))
	})
	t.Run("unknown snapshot does not erase live source manifest", func(t *testing.T) {
		source := []byte(`{"models":[{"slug":"gpt-6-astra","use_responses_lite":true,"prefer_websockets":true,"experimental_supported_tools":["function","web_search"],"context_window":1000,"base_instructions":"retain source prompt"}]}`)
		result := h.applyUpstreamModelCapabilities(ctx, &database.APIKeyRow{ID: 2}, source)
		if !gjson.GetBytes(result, "models.0.use_responses_lite").Bool() || !gjson.GetBytes(result, "models.0.prefer_websockets").Bool() || gjson.GetBytes(result, "models.0.context_window").Int() != 1000 || gjson.GetBytes(result, "models.0.experimental_supported_tools").Raw != `["function","web_search"]` {
			t.Fatalf("unobserved account erased live descriptor: %s", result)
		}
		if gjson.GetBytes(result, "models.0.base_instructions").String() != "retain source prompt" {
			t.Fatal("gateway rewrote source instructions")
		}
	})
	t.Run("fresh upstream is an upper bound", func(t *testing.T) {
		source := []byte(`{"models":[{"slug":"gpt-6-astra","use_responses_lite":false,"prefer_websockets":true,"experimental_supported_tools":["function"],"context_window":800}]}`)
		result := h.applyUpstreamModelCapabilities(ctx, &database.APIKeyRow{ID: 1}, source)
		if gjson.GetBytes(result, "models.0.use_responses_lite").Bool() || !gjson.GetBytes(result, "models.0.prefer_websockets").Bool() || gjson.GetBytes(result, "models.0.experimental_supported_tools").Raw != `["function"]` || gjson.GetBytes(result, "models.0.context_window").Int() != 800 {
			t.Fatalf("stored snapshot widened current descriptor: %s", result)
		}
	})
	t.Run("credential generation changed", func(t *testing.T) {
		known.CredentialGeneration++
		assertUnknown(t, h.applyStoredModelCapabilities(ctx, &database.APIKeyRow{ID: 1}, body))
	})
	// A failed or absent persistence layer must not restore source claims.
	t.Run("no persistence", func(t *testing.T) {
		withoutDB := NewHandler(store, nil, nil, nil)
		assertUnknown(t, withoutDB.applyUpstreamModelCapabilities(ctx, &database.APIKeyRow{ID: 1}, []byte(`{"models":[{"slug":"gpt-6-astra","use_responses_lite":true,"prefer_websockets":true,"experimental_supported_tools":["function"],"context_window":1000}]}`)))
	})
}
