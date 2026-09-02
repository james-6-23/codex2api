package admin

import (
	"encoding/json"
	"testing"
)

// Batch updates that only touch the Claude client policy must count as changes.
func TestAccountSchedulerUpdateClaudePolicyCountsAsChange(t *testing.T) {
	update, err := parseAccountSchedulerUpdate(updateAccountSchedulerReq{
		ClaudeClientPlatform: json.RawMessage(`"claude_code_cli_only"`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !update.hasChanges() {
		t.Fatal("claude_client_platform alone must be a change")
	}
	update, err = parseAccountSchedulerUpdate(updateAccountSchedulerReq{
		ClaudeVersionPolicy: json.RawMessage(`"minimum"`),
		ClaudeClientVersion: json.RawMessage(`"2.1.251"`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !update.hasChanges() {
		t.Fatal("claude_version_policy + version must be a change")
	}
}
