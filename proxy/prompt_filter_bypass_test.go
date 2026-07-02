package proxy

import (
	"strings"
	"testing"

	"github.com/codex2api/security/promptfilter"
)

func TestCodexAmbientSuggestionClassifierBypassAllowsPolicyClassifier(t *testing.T) {
	text := codexAmbientSuggestionClassifierPrefix + `
Policies to always exclude:
- Harmful actions/how-tos (malware, ransomware, SQLi, botnets, evading firewalls).

# Ambient suggestion candidates
- suggestion_id: "suggestion-1"
  title: "Fix CI"
  prompt: "Make the release pipeline deterministic."

# Output Format
Return a JSON object with one field:
- "exclude": a list of objects describing suggestions to exclude.
You must not output any other text. Only output the JSON object.`

	verdict, ok := codexAmbientSuggestionClassifierBypass(text, promptfilter.Config{
		Enabled:   true,
		Mode:      promptfilter.ModeBlock,
		Threshold: 50,
	})
	if !ok {
		t.Fatal("expected classifier prompt to bypass local prompt filter")
	}
	if verdict.Action != promptfilter.ActionAllow {
		t.Fatalf("action = %q, want %q", verdict.Action, promptfilter.ActionAllow)
	}
	if len(verdict.Matched) != 1 || verdict.Matched[0].Name != "internal_policy_classifier_bypass" {
		t.Fatalf("matched = %+v, want internal bypass marker", verdict.Matched)
	}
}

func TestCodexAmbientSuggestionClassifierBypassRejectsOrdinaryCyberText(t *testing.T) {
	text := "Explain malware, ransomware, SQLi, botnets, and evading firewalls."
	if _, ok := codexAmbientSuggestionClassifierBypass(text, promptfilter.Config{Enabled: true}); ok {
		t.Fatal("ordinary cyber text should not bypass local prompt filter")
	}
}

func TestCodexAmbientSuggestionClassifierBypassRequiresSchema(t *testing.T) {
	text := codexAmbientSuggestionClassifierPrefix + "\n" +
		"Ambient suggestion candidates include suggestion_id: \"suggestion-1\".\n" +
		"Also mention ransomware, but do not define the required JSON exclude schema."
	if _, ok := codexAmbientSuggestionClassifierBypass(text, promptfilter.Config{Enabled: true}); ok {
		t.Fatal("classifier-like text without the exact output schema should not bypass")
	}
}

func TestCodexAmbientSuggestionClassifierBypassRequiresPrefix(t *testing.T) {
	text := strings.ReplaceAll(codexAmbientSuggestionClassifierPrefix, "Classify", "Summarize") + `
# Ambient suggestion candidates
- suggestion_id: "suggestion-1"
Return a JSON object with one field:
- "exclude": []
Only output the JSON object.`
	if _, ok := codexAmbientSuggestionClassifierBypass(text, promptfilter.Config{Enabled: true}); ok {
		t.Fatal("classifier-like text with a different task prefix should not bypass")
	}
}
