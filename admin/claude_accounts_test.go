package admin

import (
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

func TestValidateAccountModelsForClaude(t *testing.T) {
	claude := &auth.Account{UpstreamType: auth.UpstreamClaude}
	if err := validateAccountModelsForAccount(claude, []string{"claude-sonnet-4-5", "claude-haiku-4-5"}); err != nil {
		t.Fatalf("valid Claude models rejected: %v", err)
	}
	if err := validateAccountModelsForAccount(claude, []string{"gpt-5.4"}); err == nil {
		t.Fatal("non-Claude model must be rejected for Claude account")
	}
	if err := validateAccountModelsForAccount(claude, nil); err != nil {
		t.Fatalf("empty Claude allowlist should clear the override: %v", err)
	}
	if err := validateAccountModelsForAccount(&auth.Account{UpstreamType: auth.UpstreamOpenAIResponses}, []string{"gpt-5.4"}); err != nil {
		t.Fatalf("non-Claude account model list changed semantics: %v", err)
	}
}

func TestBuildAccountResponseMarksClaudeProvider(t *testing.T) {
	row := &database.AccountRow{
		ID:      901,
		Name:    "claude-test",
		Status:  "active",
		Enabled: true,
		Credentials: map[string]interface{}{
			"upstream_type":                         auth.UpstreamClaude,
			"access_token":                          "claude-token",
			"plan_type":                             "claude",
			"codex_fingerprint_mode":                "full",
			auth.ClaudeClientPlatformCredentialKey:  "claude_code_cli_only",
			auth.ClaudeVersionPolicyCredentialKey:   "minimum",
			auth.ClaudeClientVersionCredentialKey:   "2.1.251",
			auth.ClaudeUsageProbeAtCredentialKey:    "2026-08-29T05:00:00Z",
			auth.ClaudeUsageProbeErrorCredentialKey: "",
			auth.ClaudeUsageWindowsCredentialKey:    `[{"name":"7d_fable","label":"Fable 5.x","utilization":63,"reset_at":"2026-09-08T00:00:00Z","model_scoped":true,"model_family":"fable"}]`,
		},
	}
	response := (&Handler{store: auth.NewStore(nil, nil, nil)}).buildAccountResponse(row, nil, nil, nil, nil, false)
	if !response.ClaudeAPI {
		t.Fatal("Claude account response must carry claude_api=true")
	}
	if response.ATOnly {
		t.Fatal("Claude account must not be mislabeled as Codex AT-only")
	}
	if response.CodexFingerprintMode != "" {
		t.Fatalf("Claude account leaked Codex fingerprint mode %q", response.CodexFingerprintMode)
	}
	if response.ClaudeClientPlatform != string(auth.ClaudeClientPlatformCLIOnly) || response.ClaudeVersionPolicy != string(auth.ClaudeVersionPolicyMinimum) || response.ClaudeClientVersion != "2.1.251" {
		t.Fatalf("Claude effective client policy = %q/%q/%q", response.ClaudeClientPlatform, response.ClaudeVersionPolicy, response.ClaudeClientVersion)
	}
	if response.ClaudeUsageProbeAt != "2026-08-29T05:00:00Z" || response.ClaudeUsageProbeError != "" {
		t.Fatalf("Claude sampling metadata = at=%q error=%q", response.ClaudeUsageProbeAt, response.ClaudeUsageProbeError)
	}
	if len(response.ClaudeUsageWindows) != 1 || response.ClaudeUsageWindows[0].Name != "7d_fable" || response.ClaudeUsageWindows[0].Utilization != 63 {
		t.Fatalf("Claude model-scoped usage = %+v", response.ClaudeUsageWindows)
	}
}

func TestClaudeImportedProbeDoesNotEnterCodexIdentityMerge(t *testing.T) {
	if shouldMergeImportedIdentity(&auth.Account{UpstreamType: auth.UpstreamClaude, AccessToken: "claude"}) {
		t.Fatal("Claude imports must not enter Codex workspace duplicate merge")
	}
	if !shouldMergeImportedIdentity(&auth.Account{UpstreamType: auth.UpstreamOpenAIResponses, AccessToken: "relay"}) {
		t.Fatal("non-Claude, non-Agent imports should retain identity merge behavior")
	}
}

func TestClaudeOAuthPutTake_OneTimeUse(t *testing.T) {
	claudeOAuthPut("state-a", "verifier-a")
	v, ok := claudeOAuthTake("state-a")
	if !ok || v != "verifier-a" {
		t.Fatalf("首次 take 应成功返回 verifier, got=(%q,%v)", v, ok)
	}
	// 一次性:再次 take 应失败。
	if _, ok := claudeOAuthTake("state-a"); ok {
		t.Fatal("同一 state 不应被 take 两次")
	}
}

func TestClaudeOAuthTake_Missing(t *testing.T) {
	if _, ok := claudeOAuthTake("no-such-state"); ok {
		t.Fatal("不存在的 state 应返回 false")
	}
}
