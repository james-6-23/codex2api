package proxy

import (
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

func withAntigravityRedirects(t *testing.T, settings auth.AntigravitySettings) {
	t.Helper()
	previous := auth.ConfiguredAntigravitySettings()
	normalized, err := auth.NormalizeAntigravitySettings(settings)
	if err != nil {
		t.Fatal(err)
	}
	auth.SetConfiguredAntigravitySettings(normalized)
	t.Cleanup(func() { auth.SetConfiguredAntigravitySettings(previous) })
}

func TestAntigravityRedirectChoicesAndValidation(t *testing.T) {
	choices := AntigravityRedirectChoices()
	byModel := map[string]AntigravityRedirectChoice{}
	for _, choice := range choices {
		byModel[choice.Model] = choice
	}
	flash := byModel["gemini-3.8-flash"]
	if flash.DefaultLevel != "low" || strings.Join(flash.Tiers, ",") != "gemini-3.8-flash-low,gemini-3.8-flash-medium,gemini-3.8-flash-high" {
		t.Fatalf("gemini-3.8-flash choice = %+v", flash)
	}
	if pro := byModel["gemini-3.1-pro"]; strings.Join(pro.Tiers, ",") != "gemini-3.1-pro-low,gemini-3.1-pro-high" {
		t.Fatalf("gemini-3.1-pro tiers = %v, want only the tiers it actually exposes", pro.Tiers)
	}
	if err := ValidateAntigravityModelRedirects(map[string]string{"gemini-3.8-flash": "gemini-3.8-flash-high", "gemini-3.1-pro": "gemini-3.1-pro-low"}); err != nil {
		t.Fatalf("valid redirects rejected: %v", err)
	}
	for _, bad := range []map[string]string{
		{"gemini-3.8-flash": "gemini-3.6-flash-high"},
		{"gemini-3.8-flash-high": "gemini-3.8-flash-low"},
		{"gemini-3.8-flash": "gemini-3.8-flash-tiered"},
		{"gemini-3.1-pro": "gemini-3.1-pro-medium"},
		{"claude-sonnet-4-6": "claude-sonnet-4-6"},
	} {
		if err := ValidateAntigravityModelRedirects(bad); err == nil {
			t.Fatalf("redirect %v must be rejected", bad)
		}
	}
}

func TestAntigravityRedirectFillsMissingEffortOnly(t *testing.T) {
	withAntigravityRedirects(t, auth.AntigravitySettings{ModelRedirects: map[string]string{
		"gemini-3.8-flash": "gemini-3.8-flash-high",
		"gemini-3.6-flash": "gemini-3.6-flash-high",
	}})

	if level, ok := antigravityGeminiThinkingLevel("gemini-3.8-flash", "gemini-3.8-flash-tiered", nil); !ok || level != "HIGH" {
		t.Fatalf("bare gemini-3.8-flash level = %q ok=%v, want HIGH via redirect", level, ok)
	}
	if level, _ := antigravityGeminiThinkingLevel("gemini-3.8-flash", "gemini-3.8-flash-tiered", map[string]any{"effort": "low"}); level != "LOW" {
		t.Fatalf("explicit effort must still win by default, got %q", level)
	}
	if level, _ := antigravityGeminiThinkingLevel("gemini-3.8-flash", "gemini-3.8-flash-tiered", map[string]any{"summary": "auto"}); level != "HIGH" {
		t.Fatalf("reasoning without effort must count as missing effort, got %q", level)
	}
	if got := antigravityGeminiResolvedModel("gemini-3.6-flash", nil); got != "gemini-3.6-flash-high" {
		t.Fatalf("gemini-3.6-flash wire = %q, want redirected high backing", got)
	}
	if got := antigravityGeminiResolvedModel("gemini-3.6-flash", map[string]any{"effort": "low"}); got != "gemini-3.6-flash-low" {
		t.Fatalf("gemini-3.6-flash with effort low = %q", got)
	}
	// Fixed tiers and unconfigured models are untouched.
	if got := antigravityGeminiResolvedModel("gemini-3.6-flash-low", nil); got != "gemini-3.6-flash-low" {
		t.Fatalf("fixed tier changed: %q", got)
	}
	if level, _ := antigravityGeminiThinkingLevel("gemini-3.7-flash", "gemini-3.7-flash-tiered", nil); level != "" {
		t.Fatalf("unrelated model got a level from redirects: %q", level)
	}
	if budget, ok := antigravityGeminiThinkingBudget("gemini-3.6-flash", "gemini-3.6-flash-high", nil); !ok || budget != 24576 {
		t.Fatalf("redirected budget = %d ok=%v, want the high tier budget", budget, ok)
	}
}

func TestAntigravityRedirectOverrideReplacesExplicitEffort(t *testing.T) {
	withAntigravityRedirects(t, auth.AntigravitySettings{
		ModelRedirects:          map[string]string{"gemini-3.8-flash": "gemini-3.8-flash-medium"},
		RedirectOverridesEffort: true,
	})
	if level, _ := antigravityGeminiThinkingLevel("gemini-3.8-flash", "gemini-3.8-flash-tiered", map[string]any{"effort": "high"}); level != "MEDIUM" {
		t.Fatalf("override must replace explicit effort, got %q", level)
	}
	if level, _ := antigravityGeminiThinkingLevel("gemini-3.8-flash-high", "gemini-3.8-flash-tiered", map[string]any{"effort": "low"}); level != "HIGH" {
		t.Fatalf("explicit fixed tier must never be redirected, got %q", level)
	}
}

func TestAntigravityFoldLogicalModelHonoursRedirect(t *testing.T) {
	body := []byte(`{"model":"gemini-3.8-flash","input":"hi"}`)
	folded, mapped, apiErr := antigravityFoldLogicalModel(body, "gemini-3.8-flash")
	if apiErr != nil || mapped != "gemini-3.8-flash-low" || gjson.GetBytes(folded, "model").String() != "gemini-3.8-flash-low" {
		t.Fatalf("default fold = %q err=%v body=%s", mapped, apiErr, folded)
	}

	withAntigravityRedirects(t, auth.AntigravitySettings{ModelRedirects: map[string]string{"gemini-3.8-flash": "gemini-3.8-flash-high"}})
	folded, mapped, apiErr = antigravityFoldLogicalModel(body, "gemini-3.8-flash")
	if apiErr != nil || mapped != "gemini-3.8-flash-high" || gjson.GetBytes(folded, "model").String() != "gemini-3.8-flash-high" {
		t.Fatalf("redirected fold = %q err=%v body=%s", mapped, apiErr, folded)
	}
	withEffort := []byte(`{"model":"gemini-3.8-flash","reasoning":{"effort":"medium"},"input":"hi"}`)
	if _, mapped, apiErr = antigravityFoldLogicalModel(withEffort, "gemini-3.8-flash"); apiErr != nil || mapped != "gemini-3.8-flash-medium" {
		t.Fatalf("explicit effort fold = %q err=%v", mapped, apiErr)
	}
	badEffort := []byte(`{"model":"gemini-3.8-flash","reasoning":{"effort":"xhigh"},"input":"hi"}`)
	if _, _, apiErr = antigravityFoldLogicalModel(badEffort, "gemini-3.8-flash"); apiErr == nil {
		t.Fatal("unsupported effort must still be rejected when the redirect does not apply")
	}
	if _, mapped, _ = antigravityFoldLogicalModel([]byte(`{"model":"gemini-3.8-flash-low"}`), "gemini-3.8-flash-low"); mapped != "gemini-3.8-flash-low" {
		t.Fatalf("fixed tier fold changed model: %q", mapped)
	}
}
