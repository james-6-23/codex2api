package promptfilter

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestInspectTextHighlightsNonSensitiveMatchContext(t *testing.T) {
	cfg := testConfig(ModeBlock)
	v := InspectText("Classify Codex ambient suggestion candidates for policy safety. Later text asks for a ddos attack plan.", cfg)
	if len(v.Matched) == 0 {
		t.Fatalf("matched = 0; verdict=%+v", v)
	}
	if !strings.Contains(v.MatchContext, HitStartMarker) || !strings.Contains(v.MatchContext, HitEndMarker) {
		t.Fatalf("match_context = %q, want hit markers", v.MatchContext)
	}
	if !strings.Contains(v.MatchContext, "ddos") {
		t.Fatalf("match_context = %q, want hit context containing ddos", v.MatchContext)
	}
	if !strings.Contains(v.TextPreview, "Classify Codex ambient suggestion") {
		t.Fatalf("text_preview = %q, want the actual inspected message", v.TextPreview)
	}
}

func TestInspectTextSkipsHighlightWhenContextContainsSensitiveData(t *testing.T) {
	cfg := testConfig(ModeBlock)
	v := InspectText("Classify Codex ambient suggestion candidates for policy safety. Authorization: Bearer sk-sensitive123456 Later text asks for a ddos attack plan.", cfg)
	if len(v.Matched) == 0 {
		t.Fatalf("matched = 0; verdict=%+v", v)
	}
	var found Match
	for _, match := range v.Matched {
		if match.Name == "ddos_attack" {
			found = match
			break
		}
	}
	if found.Name == "" {
		t.Fatalf("ddos_attack not found in matches: %+v", v.Matched)
	}
	if strings.Contains(v.TextPreview, "sk-sensitive123456") {
		t.Fatalf("text_preview leaked API key: %q", v.TextPreview)
	}
	if strings.Contains(v.MatchContext, HitStartMarker) || strings.Contains(v.MatchContext, HitEndMarker) {
		t.Fatalf("match_context = %q, want no hit markers when sensitive data is present", v.MatchContext)
	}
	if !strings.Contains(v.MatchContext, "ddos") {
		t.Fatalf("match_context = %q, want hit context containing ddos", v.MatchContext)
	}
	if !strings.Contains(v.MatchContext, "[REDACTED]") && !strings.Contains(v.MatchContext, "[REDACTED_API_KEY]") {
		t.Fatalf("match_context = %q, want redaction marker", v.MatchContext)
	}
}

func TestMatchesJSONDoesNotIncludeContextFields(t *testing.T) {
	matches := []Match{{Name: "rule", Weight: 50, Category: "custom"}}
	raw := MatchesJSON(matches)
	var parsed []map[string]any
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		t.Fatalf("MatchesJSON returned invalid JSON %q: %v", raw, err)
	}
	if len(parsed) != 1 {
		t.Fatalf("parsed matches length = %d, want 1", len(parsed))
	}
	if _, ok := parsed[0]["sample"]; ok {
		t.Fatalf("matched_patterns unexpectedly contains sample: %q", raw)
	}
	if _, ok := parsed[0]["context"]; ok {
		t.Fatalf("matched_patterns unexpectedly contains context: %q", raw)
	}
}

func TestRedactSensitiveMasksCommonSecrets(t *testing.T) {
	awsKey := "AKIA" + "ABCDEFGHIJKLMNOP"
	githubToken := "ghp_" + "ABCDEFGHIJKLMNOPQRSTUVWXYZ1234567890"
	githubFineGrainedToken := "github_pat_" + "11AA0_exampletokenABCDEFGHIJKLMNOPQRSTUVWXYZ"
	slackToken := "xoxb-" + "1234567890-abcdefghijklmnop"
	input := "Authorization: Bearer sk-live123456 password=\"hunter2\" api_key=sk-testabcdef email admin@example.com " +
		"jwt eyJabc1234567890.eyJdef1234567890.sig1234567890 " +
		awsKey + " " + githubToken + " " + githubFineGrainedToken + " " + slackToken + " " +
		"-----BEGIN PRIVATE KEY-----\nprivate-material\n-----END PRIVATE KEY-----"
	got := RedactSensitive(input)
	for _, leaked := range []string{
		"sk-live123456", "hunter2", "sk-testabcdef", "admin@example.com",
		"eyJabc1234567890.eyJdef1234567890.sig1234567890", awsKey,
		githubToken, githubFineGrainedToken, slackToken, "private-material",
	} {
		if strings.Contains(got, leaked) {
			t.Fatalf("RedactSensitive leaked %q in %q", leaked, got)
		}
	}
}
