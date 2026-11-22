package proxy

import (
	"reflect"
	"strings"
	"testing"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
)

// Catalog as returned by fetchAvailableModels on an Ultra account (2026-11-20).
var gemini4TierCatalog = []string{
	"gemini-3.8-flash-high", "gemini-3.8-flash-low", "gemini-3.8-flash-medium", "gemini-3.8-flash-tiered",
	"gemini-4-argon-high", "gemini-4-argon-low", "gemini-4-argon-medium",
	"gemini-4-carbon-high", "gemini-4-carbon-low", "gemini-4-carbon-medium",
	"gemini-4-hydrogen-high", "gemini-4-hydrogen-low", "gemini-4-hydrogen-medium", "gemini-4-hydrogen-sonar",
	"gemini-4-hydrogen-tiered-high", "gemini-4-hydrogen-tiered-xhigh", "gemini-4-hydrogen-xhigh",
	"gemini-4-neon-high", "gemini-4-neon-low", "gemini-4-neon-medium", "gemini-4-neon-xhigh",
}

func TestAntigravityGemini4TiersFoldIntoLogicalModels(t *testing.T) {
	account := &auth.Account{DBID: 4401, UpstreamType: auth.UpstreamAntigravity, AccessToken: "token", Models: gemini4TierCatalog}
	published := antigravityPublicModelsForAccount(account)
	joined := "|" + strings.Join(published, "|") + "|"
	for _, hidden := range []string{"gemini-4-hydrogen-tiered-high", "gemini-4-hydrogen-tiered-xhigh", "gemini-4-hydrogen-sonar", "gemini-3.8-flash-tiered"} {
		if strings.Contains(joined, "|"+hidden+"|") {
			t.Fatalf("role backing %s published: %v", hidden, published)
		}
	}
	models := make([]api.Model, 0, len(published))
	for _, id := range published {
		models = append(models, api.Model{ID: id})
	}
	got := map[string][]string{}
	defaults := map[string]string{}
	for _, model := range collapseAntigravityModelChoices(models) {
		for _, level := range model.SupportedReasoningLevels {
			got[model.ID] = append(got[model.ID], level.Effort)
		}
		defaults[model.ID] = model.DefaultReasoningLevel
	}
	want := map[string][]string{
		"gemini-4-neon":     {"low", "medium", "high", "xhigh"},
		"gemini-4-carbon":   {"low", "medium", "high"},
		"gemini-4-hydrogen": {"low", "medium", "high", "xhigh"},
		"gemini-4-argon":    {"low", "medium", "high"},
		"gemini-3.8-flash":  {"low", "medium", "high"},
	}
	for id, levels := range want {
		if !reflect.DeepEqual(got[id], levels) {
			t.Fatalf("%s levels = %v, want %v (all: %v)", id, got[id], levels, got)
		}
	}
	for id, level := range map[string]string{"gemini-4-neon": "high", "gemini-4-carbon": "high", "gemini-4-hydrogen": "high", "gemini-4-argon": "high", "gemini-3.8-flash": "low"} {
		if defaults[id] != level {
			t.Fatalf("%s default = %q, want %q", id, defaults[id], level)
		}
	}
	for id := range got {
		if strings.HasPrefix(id, "gemini-4-neon-") || strings.HasPrefix(id, "gemini-4-carbon-") || strings.HasPrefix(id, "gemini-4-hydrogen-") {
			t.Fatalf("tier %s was not folded", id)
		}
	}
}

func TestAntigravityGemini4TierWireAndThinkingLevel(t *testing.T) {
	for _, tc := range []struct{ model, effort, wire, level string }{
		{"gemini-4-neon-xhigh", "low", "gemini-4-neon-xhigh", "XHIGH"},
		{"gemini-4-neon-low", "", "gemini-4-neon-low", "LOW"},
		{"gemini-4-neon", "", "gemini-4-neon-high", "HIGH"},
		{"gemini-4-neon", "xhigh", "gemini-4-neon-xhigh", "XHIGH"},
		{"gemini-4-neon", "max", "gemini-4-neon-xhigh", "XHIGH"},
		{"gemini-4-neon", "medium", "gemini-4-neon-medium", "MEDIUM"},
		{"gemini-4-carbon", "xhigh", "gemini-4-carbon-high", "HIGH"},
		{"gemini-4-carbon-medium", "", "gemini-4-carbon-medium", "MEDIUM"},
		{"gemini-4-hydrogen", "low", "gemini-4-hydrogen-low", "LOW"},
		{"gemini-4-hydrogen-xhigh", "", "gemini-4-hydrogen-xhigh", "XHIGH"},
		{"gemini-4-argon-medium", "", "gemini-4-argon-high", "MEDIUM"},
		{"gemini-3.8-flash", "xhigh", "gemini-3.8-flash-tiered", "HIGH"},
		{"gemini-4-hydrogen-sonar", "high", "gemini-4-hydrogen-sonar", ""},
	} {
		t.Run(tc.model+"/"+tc.effort, func(t *testing.T) {
			body := `{"input":"hello"}`
			if tc.effort != "" {
				body = `{"input":"hello","reasoning":{"effort":"` + tc.effort + `"}}`
			}
			payload, err := responsesToGeminiInternal([]byte(body), "project", tc.model)
			if err != nil {
				t.Fatal(err)
			}
			if payload["model"] != tc.wire {
				t.Fatalf("wire = %v, want %s", payload["model"], tc.wire)
			}
			cfg, _ := payload["request"].(map[string]any)["generationConfig"].(map[string]any)
			thinking, _ := cfg["thinkingConfig"].(map[string]any)
			if tc.level == "" {
				if thinking != nil {
					t.Fatalf("raw backing got injected thinking %v", thinking)
				}
				return
			}
			if thinking["thinkingLevel"] != tc.level {
				t.Fatalf("thinking = %v, want %s", thinking, tc.level)
			}
			if _, exists := thinking["thinkingBudget"]; exists {
				t.Fatal("named-level tier got a numeric budget")
			}
		})
	}
}

func TestAntigravityGemini4FoldValidatesEffort(t *testing.T) {
	_, mapped, err := antigravityFoldLogicalModel([]byte(`{"model":"gemini-4-hydrogen","reasoning":{"effort":"xhigh"}}`), "gemini-4-hydrogen")
	if err != nil || mapped != "gemini-4-hydrogen-xhigh" {
		t.Fatalf("hydrogen xhigh folded to %q, err %v", mapped, err)
	}
	_, mapped, err = antigravityFoldLogicalModel([]byte(`{"model":"gemini-4-neon"}`), "gemini-4-neon")
	if err != nil || mapped != "gemini-4-neon-high" {
		t.Fatalf("bare neon folded to %q, err %v", mapped, err)
	}
	if _, _, err = antigravityFoldLogicalModel([]byte(`{"model":"gemini-4-carbon","reasoning":{"effort":"xhigh"}}`), "gemini-4-carbon"); err == nil {
		t.Fatal("carbon accepted an xhigh tier it does not have")
	}
}
