package admin

import (
	"testing"

	"github.com/codex2api/database"
)

// A Claude account whose stored identity headers use Go's canonical casing
// ("X-Stainless-Os") must survive a same-timezone save: the fresh fingerprint
// is keyed "X-Stainless-OS", and a case-insensitive clash with a different
// random OS used to fail the whole update with
// "custom_headers 包含大小写重复且值冲突的请求头: X-Stainless-Os".
func TestPrepareClaudeTimezoneUpdate_KeepsCanonicalCasedIdentityOnSameTimezone(t *testing.T) {
	stored := map[string]string{
		"User-Agent": "claude-cli/2.1.259 (external, cli)", "X-App": "cli",
		"X-Stainless-Arch": "x64", "X-Stainless-Lang": "js", "X-Stainless-Os": "Windows",
		"X-Stainless-Package-Version": "0.65.0", "X-Stainless-Runtime": "node", "X-Stainless-Runtime-Version": "v20.18.1",
	}
	for i := 0; i < 30; i++ { // the generated OS is random; every save must succeed
		headersAny := make(map[string]interface{}, len(stored))
		for k, v := range stored {
			headersAny[k] = v
		}
		row := &database.AccountRow{Platform: "anthropic", Credentials: map[string]interface{}{
			"upstream_type": "claude", "timezone": "Asia/Shanghai", "custom_headers": headersAny,
		}}
		updates := map[string]interface{}{}
		applied, err := prepareClaudeTimezoneCredentialUpdateWithHeaders(row, "Asia/Shanghai", updates, nil)
		if err != nil {
			t.Fatalf("iteration %d: %v", i, err)
		}
		if !applied {
			t.Fatal("update must apply to a Claude row")
		}
		headers, _ := updates["custom_headers"].(map[string]string)
		if headers["X-Stainless-Os"] != "Windows" {
			t.Fatalf("iteration %d: stored identity must win on same timezone, got %v", i, headers)
		}
		if _, dup := headers["X-Stainless-OS"]; dup {
			t.Fatalf("iteration %d: headers must be canonical-cased only, got %v", i, headers)
		}
	}
}
