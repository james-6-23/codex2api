package proxy

import (
	"reflect"
	"testing"

	"github.com/codex2api/auth"
)

func TestAntigravityPublicModelCatalogIsExact(t *testing.T) {
	want := []string{
		"gemini-3.5-flash-low", "gemini-3.5-flash-medium", "gemini-3.5-flash-high",
		"gemini-3.6-flash-low", "gemini-3.6-flash-medium", "gemini-3.6-flash-high",
		"gemini-3.7-flash-low", "gemini-3.7-flash-medium", "gemini-3.7-flash-high",
		"gemini-3.1-pro-low", "gemini-3.1-pro-high",
		"claude-opus-4-6-thinking",
		"claude-sonnet-4-6",
		"gpt-oss-120b-medium",
	}
	if got := antigravityPublicModelIDs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("public catalog = %v, want %v", got, want)
	}

	for _, logical := range []string{
		"gemini-3.5-flash", "gemini-3.6-flash", "gemini-3.7-flash", "gemini-3.1-pro",
	} {
		if _, ok := antigravityPublicModel(logical); ok {
			t.Fatalf("logical compatibility model %q leaked into the public catalog", logical)
		}
		if _, ok := antigravityLogicalCompatibilityModel(logical); !ok {
			t.Fatalf("logical compatibility model %q is not accepted", logical)
		}
	}
}

func TestAntigravityFixedTierModelsDoNotExposeReasoningLevels(t *testing.T) {
	for _, model := range antigravityPublicModelIDs() {
		if got := antigravityCodexReasoningLevels(model); len(got) != 0 {
			t.Fatalf("%s reasoning levels = %v, want none", model, got)
		}
	}
}

func TestAntigravityLogicalModelsResolveEffortToBacking(t *testing.T) {
	for _, test := range []struct {
		model, effort, wire string
		budget              int
	}{
		{model: "gemini-3.5-flash", effort: "low", wire: "gemini-3.5-flash-extra-low", budget: 1000},
		{model: "gemini-3.5-flash", effort: "medium", wire: "gemini-3.5-flash-low", budget: 4000},
		{model: "gemini-3.5-flash", effort: "high", wire: "gemini-3-flash-agent", budget: 10000},
		{model: "gemini-3.6-flash", effort: "low", wire: "gemini-3.6-flash-low", budget: 4096},
		{model: "gemini-3.6-flash", effort: "medium", wire: "gemini-3.6-flash-medium", budget: 8192},
		{model: "gemini-3.6-flash", effort: "high", wire: "gemini-3.6-flash-high", budget: 24576},
		{model: "gemini-3.7-flash", effort: "low", wire: "gemini-3.7-flash-tiered", budget: 4096},
		{model: "gemini-3.7-flash", effort: "medium", wire: "gemini-3.7-flash-tiered", budget: 8192},
		{model: "gemini-3.7-flash", effort: "high", wire: "gemini-3.7-flash-tiered", budget: 24576},
		{model: "gemini-3.1-pro", effort: "low", wire: "gemini-3.1-pro-low", budget: 1001},
		{model: "gemini-3.1-pro", effort: "high", wire: "gemini-pro-agent", budget: 10001},
		{model: "gemini-3.1-pro", effort: "medium", wire: "gemini-pro-agent", budget: 10001},
	} {
		t.Run(test.model+"/"+test.effort, func(t *testing.T) {
			reasoning := map[string]any{"effort": test.effort}
			variant, ok := antigravityResolvedVariant(test.model, reasoning)
			if !ok || variant.wireModel != test.wire || variant.thinkingBudget != test.budget {
				t.Fatalf("variant = %#v, want wire=%q budget=%d", variant, test.wire, test.budget)
			}
		})
	}
	if variant, _ := antigravityResolvedVariant("gemini-3.5-flash", nil); variant.level != "medium" {
		t.Fatalf("Flash default = %q, want medium", variant.level)
	}
	if variant, _ := antigravityResolvedVariant("gemini-3.1-pro", nil); variant.level != "high" {
		t.Fatalf("Pro default = %q, want high", variant.level)
	}
}

func TestAntigravityPublishedModelsProjectCompleteRawCatalog(t *testing.T) {
	raw := []string{
		"gemini-3.5-flash-extra-low", "gemini-3.5-flash-low", "gemini-3-flash-agent",
		"gemini-3.6-flash-low", "gemini-3.6-flash-medium", "gemini-3.6-flash-high",
		"gemini-3.7-flash-tiered", "gemini-3.1-pro-low", "gemini-pro-agent",
		"claude-opus-4-6-thinking", "claude-sonnet-4-6", "gpt-oss-120b-medium",
	}
	want := antigravityPublicModelIDs()
	if got := AntigravityPublishedModelIDs(raw); !reflect.DeepEqual(got, want) {
		t.Fatalf("published raw projection = %v, want %v", got, want)
	}
	if got := AntigravityPublishedModelIDs(auth.AntigravityDefaultModelIDs()); !reflect.DeepEqual(got, want) {
		t.Fatalf("default raw projection = %v, want %v", got, want)
	}

	account := &auth.Account{UpstreamType: auth.UpstreamAntigravity, AccessToken: "token", Models: raw}
	if got := antigravityPublicModelsForAccount(account); !reflect.DeepEqual(got, want) {
		t.Fatalf("account public models = %v, want %v", got, want)
	}
	for _, model := range append(want, "gemini-3.7-flash-high") {
		if !antigravityAccountSupportsPublicModel(account, model) {
			t.Fatalf("account support helper rejected %q", model)
		}
	}
}

func TestAntigravityPublishedModelsRequireCompleteLogicalFamily(t *testing.T) {
	raw := []string{"gemini-3.5-flash-low", "gemini-3.7-flash-tiered"}
	want := []string{"gemini-3.5-flash-medium", "gemini-3.7-flash-low", "gemini-3.7-flash-medium", "gemini-3.7-flash-high"}
	if got := AntigravityPublishedModelIDs(raw); !reflect.DeepEqual(got, want) {
		t.Fatalf("partial raw projection = %v, want %v", got, want)
	}
}

func TestAntigravityTextBridgeRejectsImageModels(t *testing.T) {
	for _, model := range []string{"imagen-3", "gemini-3.1-flash-image", "image-generation"} {
		if antigravityResponsesTextModel(model) {
			t.Fatalf("image model %q was admitted", model)
		}
	}
}
