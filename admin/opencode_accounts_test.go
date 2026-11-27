package admin

import (
	"testing"

	"github.com/codex2api/auth"
)

func TestParseOpenCodeImportEntries(t *testing.T) {
	// 1. Plain text lines (key & name,key)
	lines := "# comment\nsk-plain-1\nmy-zen,sk-plain-2\n"
	entries, err := parseOpenCodeImportEntries(lines, "go", "", "", "")
	if err != nil {
		t.Fatalf("plain text parse failed: %v", err)
	}
	if len(entries) != 2 || entries[0].APIKey != "sk-plain-1" || entries[1].Name != "my-zen" || entries[1].APIKey != "sk-plain-2" {
		t.Fatalf("unexpected plain entries: %+v", entries)
	}

	// 2. Sub2API export JSON (mixed platforms: filters to opencode_go only)
	sub2apiJSON := `{
		"accounts": [
			{"name": "codex-1", "platform": "openai", "credentials": {"api_key": "sk-ignore"}},
			{
				"name": "oc-go-1",
				"platform": "opencode_go",
				"credentials": {
					"api_key": "sk-oc-go",
					"account_mode": "go",
					"base_url": "https://opencode.ai/zen/go/v1",
					"api_base_urls": {"anthropic": "https://opencode.ai/zen/go"},
					"protocol_rules": [{"pattern": "grok-*", "protocol": "responses"}]
				}
			},
			{
				"name": "oc-zen-1",
				"platform": "opencode_go",
				"credentials": {
					"api_key": "sk-oc-zen",
					"account_mode": "zen",
					"base_url": "https://opencode.ai/zen/v1"
				}
			}
		]
	}`
	entries, err = parseOpenCodeImportEntries(sub2apiJSON, "go", "", "", "")
	if err != nil {
		t.Fatalf("sub2api parse failed: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 opencode accounts, got %d", len(entries))
	}
	if entries[0].Name != "oc-go-1" || entries[0].Mode != "go" || entries[0].AnthropicBaseURL != "https://opencode.ai/zen/go" || entries[0].ProtocolRules == nil || len(*entries[0].ProtocolRules) != 1 {
		t.Fatalf("unexpected first entry: %+v", entries[0])
	}
	if entries[1].Name != "oc-zen-1" || entries[1].Mode != "zen" || entries[1].APIKey != "sk-oc-zen" {
		t.Fatalf("unexpected second entry: %+v", entries[1])
	}
}

func TestSanitizeOpenCodeCustomHeaders(t *testing.T) {
	if _, err := sanitizeOpenCodeCustomHeaders(map[string]string{"Authorization": "Bearer x"}); err == nil {
		t.Fatal("expected reserved Authorization header to be rejected")
	}
	if _, err := sanitizeOpenCodeCustomHeaders(map[string]string{"x-api-key": "secret"}); err == nil {
		t.Fatal("expected reserved x-api-key header to be rejected")
	}
	cleaned, err := sanitizeOpenCodeCustomHeaders(map[string]string{"User-Agent": "opencode/1.20.0", "X-Custom": "val"})
	if err != nil {
		t.Fatalf("expected valid custom headers to pass, got %v", err)
	}
	if cleaned["User-Agent"] != "opencode/1.20.0" || cleaned["X-Custom"] != "val" {
		t.Fatalf("unexpected cleaned headers: %+v", cleaned)
	}
}

func TestOpenCodeConnectionTestModel(t *testing.T) {
	acc := &auth.Account{
		UpstreamType: auth.UpstreamOpenCode,
		APIKey:       "sk-test",
		OpenCode:     auth.OpenCodeCredential{Mode: auth.OpenCodeModeGo},
	}
	m, err := openCodeConnectionTestModel(acc, "", "")
	if err != nil || m != auth.DefaultOpenCodeTestModel {
		t.Fatalf("default test model = (%q, %v), want %q", m, err, auth.DefaultOpenCodeTestModel)
	}
	if _, err := openCodeConnectionTestModel(acc, "gemini-3.8-flash", ""); err == nil {
		t.Fatal("expected unsupported gemini model to fail connection test validation")
	}
}
