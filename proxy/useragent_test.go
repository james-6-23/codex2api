package proxy

import (
	"strings"
	"testing"
)

func TestClientProfilesUseLatestCodexCLI(t *testing.T) {
	if len(clientProfiles) == 0 {
		t.Fatal("clientProfiles should not be empty")
	}

	wantUA := "codex-tui/" + latestCodexCLIVersion
	for _, profile := range clientProfiles {
		if profile.Version != latestCodexCLIVersion {
			t.Fatalf("clientProfiles should only use latest Codex CLI %s, got %s", latestCodexCLIVersion, profile.Version)
		}
		if !strings.Contains(profile.UserAgent, wantUA) {
			t.Fatalf("%s profile has mismatched User-Agent: %q", latestCodexCLIVersion, profile.UserAgent)
		}
		if !strings.Contains(profile.UserAgent, " (codex-tui; "+latestCodexCLIVersion+")") {
			t.Fatalf("%s profile is missing official trailing client marker: %q", latestCodexCLIVersion, profile.UserAgent)
		}
	}
}

func TestDefaultClientProfileUsesLatestCodexCLI(t *testing.T) {
	profile := ProfileForAccount(1)
	if profile.Version != latestCodexCLIVersion {
		t.Fatalf("ProfileForAccount returned Codex CLI version %s, want %s", profile.Version, latestCodexCLIVersion)
	}
}

func TestCodexUserAgentConfigBuildsOfficialCLIShape(t *testing.T) {
	raw := `{"client_name":"codex-tui","client_version":"0.142.0-alpha.10","os_name":"Mac OS","os_version":"13.7.8","arch":"arm64","terminal":"xterm-256color"}`
	normalized, err := NormalizeCodexUserAgentConfigJSON(raw)
	if err != nil {
		t.Fatalf("NormalizeCodexUserAgentConfigJSON() error = %v", err)
	}
	userAgent, version, ok := codexUserAgentFromConfig(normalized, "")
	if !ok {
		t.Fatal("codexUserAgentFromConfig() ok = false")
	}
	wantUA := "codex-tui/0.142.0-alpha.10 (Mac OS 13.7.8; arm64) xterm-256color (codex-tui; 0.142.0-alpha.10)"
	if userAgent != wantUA {
		t.Fatalf("User-Agent = %q, want %q", userAgent, wantUA)
	}
	if version != "0.142.0-alpha.10" {
		t.Fatalf("version = %q, want 0.142.0-alpha.10", version)
	}
}

func TestCodexUserAgentConfigRaisesStructuredVersionToFloor(t *testing.T) {
	raw := `{"client_name":"codex-tui","client_version":"0.142.0","os_name":"Linux","os_version":"Unknown","arch":"x86_64","terminal":"xterm-256color"}`
	normalized, err := NormalizeCodexUserAgentConfigJSON(raw)
	if err != nil {
		t.Fatalf("NormalizeCodexUserAgentConfigJSON() error = %v", err)
	}
	userAgent, version, ok := codexUserAgentFromConfig(normalized, "0.150.0")
	if !ok {
		t.Fatal("codexUserAgentFromConfig() ok = false")
	}
	if version != "0.150.0" {
		t.Fatalf("version = %q, want 0.150.0", version)
	}
	if !strings.Contains(userAgent, "codex-tui/0.150.0 ") || !strings.Contains(userAgent, "(codex-tui; 0.150.0)") {
		t.Fatalf("User-Agent = %q, want version floor applied in both markers", userAgent)
	}
}

func TestCodexUserAgentConfigRaisesPrereleaseVersionToStableFloor(t *testing.T) {
	raw := `{"client_name":"codex-tui","client_version":"0.142.0-alpha.10","os_name":"Linux","os_version":"Unknown","arch":"x86_64","terminal":"xterm-256color"}`
	normalized, err := NormalizeCodexUserAgentConfigJSON(raw)
	if err != nil {
		t.Fatalf("NormalizeCodexUserAgentConfigJSON() error = %v", err)
	}
	userAgent, version, ok := codexUserAgentFromConfig(normalized, "0.142.0")
	if !ok {
		t.Fatal("codexUserAgentFromConfig() ok = false")
	}
	if version != "0.142.0" {
		t.Fatalf("version = %q, want 0.142.0", version)
	}
	if !strings.Contains(userAgent, "codex-tui/0.142.0 ") || !strings.Contains(userAgent, "(codex-tui; 0.142.0)") {
		t.Fatalf("User-Agent = %q, want stable version floor applied in both markers", userAgent)
	}
}

func TestCodexUserAgentConfigNormalizesClientVersionBeforeStrippingPrefix(t *testing.T) {
	normalized, err := NormalizeCodexUserAgentConfigJSON(`{"client_version":" v0.142.3 "}`)
	if err != nil {
		t.Fatalf("NormalizeCodexUserAgentConfigJSON() error = %v", err)
	}
	cfg := codexUserAgentConfigFromJSON(normalized)
	if cfg.ClientVersion != "0.142.3" {
		t.Fatalf("ClientVersion = %q, want 0.142.3", cfg.ClientVersion)
	}
}

func TestCodexUserAgentConfigRejectsInvalidClientVersion(t *testing.T) {
	if _, err := NormalizeCodexUserAgentConfigJSON(`{"client_version":"bogus"}`); err == nil {
		t.Fatal("NormalizeCodexUserAgentConfigJSON() accepted invalid client_version")
	}
}

func TestCodexUserAgentConfigRawOverrideWithoutVersionDoesNotSynthesizeVersion(t *testing.T) {
	normalized, err := NormalizeCodexUserAgentConfigJSON(`{"raw_user_agent":"my-router","client_name":"codex-tui"}`)
	if err != nil {
		t.Fatalf("NormalizeCodexUserAgentConfigJSON() error = %v", err)
	}
	userAgent, version, ok := codexUserAgentFromConfig(normalized, "0.150.0")
	if !ok {
		t.Fatal("codexUserAgentFromConfig() ok = false")
	}
	if userAgent != "my-router" {
		t.Fatalf("User-Agent = %q, want raw override", userAgent)
	}
	if version != "" {
		t.Fatalf("version = %q, want empty", version)
	}
}

// raw UA 只贡献指纹形状:可解析的版本段出站前重建为当前生效版本(前缀与尾部标识组两处)。
func TestCodexUserAgentConfigRawOverrideRebuildsVersionSegments(t *testing.T) {
	prev := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(prev) })
	s := prev
	s.CodexSyncedCLIVersion = ""
	ApplyRuntimeSettings(s)

	normalized, err := NormalizeCodexUserAgentConfigJSON(`{"raw_user_agent":"codex-tui/0.100.0 (Linux Unknown; x86_64) xterm-256color (codex-tui; 0.100.0)"}`)
	if err != nil {
		t.Fatalf("NormalizeCodexUserAgentConfigJSON() error = %v", err)
	}
	userAgent, version, ok := codexUserAgentFromConfig(normalized, "")
	if !ok {
		t.Fatal("codexUserAgentFromConfig() ok = false")
	}
	if version != latestCodexCLIVersion {
		t.Fatalf("version = %q, want builtin %q", version, latestCodexCLIVersion)
	}
	want := "codex-tui/" + latestCodexCLIVersion + " (Linux Unknown; x86_64) xterm-256color (codex-tui; " + latestCodexCLIVersion + ")"
	if userAgent != want {
		t.Fatalf("User-Agent = %q, want both version segments rebuilt: %q", userAgent, want)
	}
}

// 远端同步到更高版本后,raw UA 的版本段跟随同步值。
func TestCodexUserAgentConfigRawOverrideFollowsSyncedVersion(t *testing.T) {
	prev := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(prev) })
	s := prev
	s.CodexSyncedCLIVersion = "0.200.0"
	ApplyRuntimeSettings(s)

	normalized, err := NormalizeCodexUserAgentConfigJSON(`{"raw_user_agent":"codex-tui/0.144.1 (Mac OS 15.5.0; arm64) xterm-256color (codex-tui; 0.144.1)"}`)
	if err != nil {
		t.Fatalf("NormalizeCodexUserAgentConfigJSON() error = %v", err)
	}
	userAgent, version, ok := codexUserAgentFromConfig(normalized, "")
	if !ok {
		t.Fatal("codexUserAgentFromConfig() ok = false")
	}
	if version != "0.200.0" {
		t.Fatalf("version = %q, want synced 0.200.0", version)
	}
	if !strings.Contains(userAgent, "codex-tui/0.200.0 ") || !strings.Contains(userAgent, "(codex-tui; 0.200.0)") {
		t.Fatalf("User-Agent = %q, want synced version in both markers", userAgent)
	}
}

// raw UA 钉了高于生效版本的版本号时保留(版本抬升绝不降级)。
func TestCodexUserAgentConfigRawOverrideKeepsAheadPin(t *testing.T) {
	prev := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(prev) })
	s := prev
	s.CodexSyncedCLIVersion = ""
	ApplyRuntimeSettings(s)

	normalized, err := NormalizeCodexUserAgentConfigJSON(`{"raw_user_agent":"codex-tui/9.999.0 (Linux Unknown; x86_64) xterm-256color (codex-tui; 9.999.0)"}`)
	if err != nil {
		t.Fatalf("NormalizeCodexUserAgentConfigJSON() error = %v", err)
	}
	userAgent, version, ok := codexUserAgentFromConfig(normalized, "")
	if !ok {
		t.Fatal("codexUserAgentFromConfig() ok = false")
	}
	if version != "9.999.0" {
		t.Fatalf("version = %q, want ahead pin 9.999.0 preserved", version)
	}
	if !strings.Contains(userAgent, "codex-tui/9.999.0 ") {
		t.Fatalf("User-Agent = %q, want ahead pin preserved", userAgent)
	}
}

// raw UA 分支同样叠加最低版本门槛(floor 高于生效版本时以 floor 为准)。
func TestCodexUserAgentConfigRawOverrideAppliesVersionFloor(t *testing.T) {
	prev := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(prev) })
	s := prev
	s.CodexSyncedCLIVersion = ""
	ApplyRuntimeSettings(s)

	normalized, err := NormalizeCodexUserAgentConfigJSON(`{"raw_user_agent":"codex-tui/0.100.0 (Linux Unknown; x86_64) xterm-256color (codex-tui; 0.100.0)"}`)
	if err != nil {
		t.Fatalf("NormalizeCodexUserAgentConfigJSON() error = %v", err)
	}
	_, version, ok := codexUserAgentFromConfig(normalized, "0.160.0")
	if !ok {
		t.Fatal("codexUserAgentFromConfig() ok = false")
	}
	if version != "0.160.0" {
		t.Fatalf("version = %q, want floor 0.160.0", version)
	}
}

func TestCodexUserAgentConfigRejectsHeaderBreaks(t *testing.T) {
	if _, err := NormalizeCodexUserAgentConfigJSON(`{"raw_user_agent":"codex-tui/0.142.3\r\nX-Bad: yes"}`); err == nil {
		t.Fatal("NormalizeCodexUserAgentConfigJSON() accepted a raw UA with CRLF")
	}
}

func TestIsCodexOfficialClientByHeaders(t *testing.T) {
	tests := []struct {
		name       string
		userAgent  string
		originator string
		want       bool
	}{
		{name: "tui ua", userAgent: "codex-tui/0.142.0", want: true},
		{name: "legacy cli ua", userAgent: "codex_cli_rs/0.128.0", want: true},
		{name: "vscode ua", userAgent: "codex_vscode/1.2.3", want: true},
		{name: "tui originator", originator: "codex-tui", want: true},
		{name: "desktop originator", originator: "codex_chatgpt_desktop", want: true},
		{name: "opencode ua", userAgent: "opencode/0.5.0", want: true},
		{name: "opencode originator", originator: "opencode", want: true},
		{name: "legacy contains codex token", userAgent: "Mozilla/5.0 codex_cli_rs/0.128.0", want: true},
		{name: "non official", userAgent: "curl/8.0", originator: "random-client", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsCodexOfficialClientByHeaders(tc.userAgent, tc.originator); got != tc.want {
				t.Fatalf("IsCodexOfficialClientByHeaders() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsCodexStrictOfficialClientByHeaders(t *testing.T) {
	tests := []struct {
		name       string
		userAgent  string
		originator string
		want       bool
	}{
		{name: "tui ua", userAgent: "codex-tui/0.142.0", want: true},
		{name: "legacy cli ua", userAgent: "codex_cli_rs/0.128.0", want: true},
		{name: "vscode ua", userAgent: "codex_vscode/1.2.3", want: true},
		{name: "tui originator", originator: "codex-tui", want: true},
		{name: "desktop originator", originator: "codex_chatgpt_desktop", want: true},
		{name: "unknown codex-like originator rejected", originator: "codex_random", want: false},
		{name: "codex spaced ua", userAgent: "codex 0.136.0", want: true},
		{name: "codex spaced without version rejected", userAgent: "codex ", want: false},
		{name: "embedded cli token rejected", userAgent: "Mozilla/5.0 codex_cli_rs/0.128.0", want: false},
		{name: "random codex token rejected", userAgent: "random-codex-client", want: false},
		{name: "opencode kept out of strict official", userAgent: "opencode/0.5.0", originator: "opencode", want: false},
		{name: "non official", userAgent: "curl/8.0", originator: "random-client", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsCodexStrictOfficialClientByHeaders(tc.userAgent, tc.originator); got != tc.want {
				t.Fatalf("IsCodexStrictOfficialClientByHeaders() = %v, want %v", got, tc.want)
			}
		})
	}
}
