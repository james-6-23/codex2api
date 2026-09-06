package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
)

func TestCodexCapabilityValidationAndIntersection(t *testing.T) {
	a := parseCodexModelCapabilities([]byte(`{"models":[{"slug":"gpt-6-astra","context_window":1000,"supports_search_tool":true,"use_responses_lite":true,"supported_reasoning_levels":[{"effort":"low"},{"effort":"max"}],"base_instructions":"never persist","tool_mode":{"invalid":true}}]}`))
	b := parseCodexModelCapabilities([]byte(`{"models":[{"slug":"gpt-6-astra","context_window":800,"supports_search_tool":false,"use_responses_lite":true,"supported_reasoning_levels":["low","high"]}]}`))
	if len(a) != 1 || a["gpt-6-astra"]["base_instructions"] != nil || a["gpt-6-astra"]["tool_mode"] != nil {
		t.Fatalf("unsafe descriptor fields admitted: %+v", a)
	}
	got := intersectCodexCapabilities([]map[string]json.RawMessage{a["gpt-6-astra"], b["gpt-6-astra"]})
	if string(got["context_window"]) != "800" || string(got["supports_search_tool"]) != "false" || string(got["use_responses_lite"]) != "true" || gjson.GetBytes(got["supported_reasoning_levels"], "0.effort").String() != "low" {
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
