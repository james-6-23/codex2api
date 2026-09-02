package auth

import "testing"

func TestParseClaudeClientVersionRecognizesCLIUserAgents(t *testing.T) {
	tests := []struct {
		ua      string
		version string
		ok      bool
	}{
		{ua: "claude-cli/2.1.205 (external, cli)", version: "2.1.205", ok: true},
		{ua: "claude-code/2.1.251 linux/x64", version: "2.1.251", ok: true},
		{ua: "Claude Code 2.1.251", version: "2.1.251", ok: true},
		{ua: "curl/8.0", ok: false},
	}
	for _, tt := range tests {
		version, ok := ParseClaudeClientVersion(tt.ua)
		if ok != tt.ok || version != tt.version {
			t.Errorf("ParseClaudeClientVersion(%q) = %q, %v; want %q, %v", tt.ua, version, ok, tt.version, tt.ok)
		}
	}
}

func TestValidateClaudeClientRequestAppliesFableMinimum(t *testing.T) {
	decision, err := ValidateClaudeClientRequest(ClaudeClientPolicy{}, "claude-cli/2.1.205", "claude-fable-5-1-20260801")
	if err != nil {
		t.Fatalf("ValidateClaudeClientRequest: %v", err)
	}
	if decision.Allowed || decision.Code != "client_version_too_old" || decision.RequiredVersion != "2.1.251" {
		t.Fatalf("decision = %+v", decision)
	}
}

func TestValidateClaudeClientRequestRejectsNonCLIWhenLocked(t *testing.T) {
	decision, err := ValidateClaudeClientRequest(ClaudeClientPolicy{
		Platform:      ClaudeClientPlatformCLIOnly,
		VersionPolicy: ClaudeVersionPolicyPassthrough,
	}, "curl/8.0", "claude-sonnet-4-5")
	if err != nil {
		t.Fatalf("ValidateClaudeClientRequest: %v", err)
	}
	if decision.Allowed || decision.Code != "client_platform_not_allowed" {
		t.Fatalf("decision = %+v", decision)
	}
}

func TestValidateClaudeClientRequestMinimumAndFixedPolicies(t *testing.T) {
	minimum, err := ValidateClaudeClientRequest(ClaudeClientPolicy{
		VersionPolicy: ClaudeVersionPolicyMinimum,
		ClientVersion: "2.1.300",
	}, "claude-cli/2.1.251", "claude-sonnet-4-5")
	if err != nil {
		t.Fatalf("minimum validation: %v", err)
	}
	if minimum.Allowed || minimum.Code != "client_version_too_old" {
		t.Fatalf("minimum decision = %+v", minimum)
	}
	fixed, err := ValidateClaudeClientRequest(ClaudeClientPolicy{
		VersionPolicy: ClaudeVersionPolicyFixed,
		ClientVersion: "2.1.300",
	}, "claude-cli/2.1.205", "claude-fable-5-1")
	if err != nil {
		t.Fatalf("fixed validation: %v", err)
	}
	if !fixed.Allowed || fixed.RewriteVersion != "2.1.300" {
		t.Fatalf("fixed decision = %+v", fixed)
	}
	tooOldFixed, err := ValidateClaudeClientRequest(ClaudeClientPolicy{
		VersionPolicy: ClaudeVersionPolicyFixed,
		ClientVersion: "2.1.200",
	}, "claude-cli/2.1.205", "claude-fable-5-1")
	if err != nil {
		t.Fatalf("old fixed validation: %v", err)
	}
	if tooOldFixed.Allowed || tooOldFixed.RequiredVersion != "2.1.251" {
		t.Fatalf("old fixed decision = %+v", tooOldFixed)
	}
}

func TestNormalizeClaudeClientPolicyRejectsInvalidValues(t *testing.T) {
	if _, err := NormalizeClaudeClientPolicy(ClaudeClientPolicy{Platform: "desktop"}); err == nil {
		t.Fatal("invalid platform must be rejected")
	}
	if _, err := NormalizeClaudeClientPolicy(ClaudeClientPolicy{VersionPolicy: "latest", ClientVersion: "2.1"}); err == nil {
		t.Fatal("invalid version policy must be rejected")
	}
	if _, err := NormalizeClaudeClientPolicy(ClaudeClientPolicy{VersionPolicy: ClaudeVersionPolicyMinimum, ClientVersion: "2.1"}); err == nil {
		t.Fatal("incomplete SemVer must be rejected")
	}
}
