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
	userAgent, version, ok := codexUserAgentFromConfig(normalized, 0, "")
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

func TestCodexUserAgentConfigManualVersionOverridesFloor(t *testing.T) {
	raw := `{"client_name":"codex-tui","client_version":"0.142.0","os_name":"Linux","os_version":"Unknown","arch":"x86_64","terminal":"xterm-256color"}`
	normalized, err := NormalizeCodexUserAgentConfigJSON(raw)
	if err != nil {
		t.Fatalf("NormalizeCodexUserAgentConfigJSON() error = %v", err)
	}
	userAgent, version, ok := codexUserAgentFromConfig(normalized, 0, "0.150.0")
	if !ok {
		t.Fatal("codexUserAgentFromConfig() ok = false")
	}
	if version != "0.142.0" {
		t.Fatalf("version = %q, want 0.142.0", version)
	}
	if !strings.Contains(userAgent, "codex-tui/0.142.0 ") || !strings.Contains(userAgent, "(codex-tui; 0.142.0)") {
		t.Fatalf("User-Agent = %q, want version floor applied in both markers", userAgent)
	}
}

func TestCodexUserAgentConfigManualPrereleaseOverridesStableFloor(t *testing.T) {
	raw := `{"client_name":"codex-tui","client_version":"0.142.0-alpha.10","os_name":"Linux","os_version":"Unknown","arch":"x86_64","terminal":"xterm-256color"}`
	normalized, err := NormalizeCodexUserAgentConfigJSON(raw)
	if err != nil {
		t.Fatalf("NormalizeCodexUserAgentConfigJSON() error = %v", err)
	}
	userAgent, version, ok := codexUserAgentFromConfig(normalized, 0, "0.142.0")
	if !ok {
		t.Fatal("codexUserAgentFromConfig() ok = false")
	}
	if version != "0.142.0-alpha.10" {
		t.Fatalf("version = %q, want 0.142.0-alpha.10", version)
	}
	if !strings.Contains(userAgent, "codex-tui/0.142.0-alpha.10 ") || !strings.Contains(userAgent, "(codex-tui; 0.142.0-alpha.10)") {
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
	userAgent, version, ok := codexUserAgentFromConfig(normalized, 0, "0.150.0")
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

// raw UA 原样覆盖，不改写版本段。
func TestCodexUserAgentConfigRawOverridePreservesVersionSegments(t *testing.T) {
	prev := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(prev) })
	s := prev
	s.CodexSyncedCLIVersion = ""
	ApplyRuntimeSettings(s)

	normalized, err := NormalizeCodexUserAgentConfigJSON(`{"raw_user_agent":"codex-tui/0.100.0 (Linux Unknown; x86_64) xterm-256color (codex-tui; 0.100.0)"}`)
	if err != nil {
		t.Fatalf("NormalizeCodexUserAgentConfigJSON() error = %v", err)
	}
	userAgent, version, ok := codexUserAgentFromConfig(normalized, 0, "")
	if !ok {
		t.Fatal("codexUserAgentFromConfig() ok = false")
	}
	if version != "0.100.0" {
		t.Fatalf("version = %q, want raw 0.100.0", version)
	}
	want := "codex-tui/0.100.0 (Linux Unknown; x86_64) xterm-256color (codex-tui; 0.100.0)"
	if userAgent != want {
		t.Fatalf("User-Agent = %q, want exact raw override: %q", userAgent, want)
	}
}

// 同步版本不覆盖手填的 raw UA。
func TestCodexUserAgentConfigRawOverrideIgnoresSyncedVersion(t *testing.T) {
	prev := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(prev) })
	s := prev
	s.CodexSyncedCLIVersion = "0.200.0"
	ApplyRuntimeSettings(s)

	normalized, err := NormalizeCodexUserAgentConfigJSON(`{"raw_user_agent":"codex-tui/0.144.1 (Mac OS 15.5.0; arm64) xterm-256color (codex-tui; 0.144.1)"}`)
	if err != nil {
		t.Fatalf("NormalizeCodexUserAgentConfigJSON() error = %v", err)
	}
	userAgent, version, ok := codexUserAgentFromConfig(normalized, 0, "")
	if !ok {
		t.Fatal("codexUserAgentFromConfig() ok = false")
	}
	if version != "0.144.1" {
		t.Fatalf("version = %q, want original 0.144.1", version)
	}
	if !strings.Contains(userAgent, "codex-tui/0.144.1 ") || !strings.Contains(userAgent, "(codex-tui; 0.144.1)") {
		t.Fatalf("User-Agent = %q, want original version in both markers", userAgent)
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
	userAgent, version, ok := codexUserAgentFromConfig(normalized, 0, "")
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

// raw UA 不受自动版本门槛约束。
func TestCodexUserAgentConfigRawOverrideIgnoresVersionFloor(t *testing.T) {
	prev := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(prev) })
	s := prev
	s.CodexSyncedCLIVersion = ""
	ApplyRuntimeSettings(s)

	normalized, err := NormalizeCodexUserAgentConfigJSON(`{"raw_user_agent":"codex-tui/0.100.0 (Linux Unknown; x86_64) xterm-256color (codex-tui; 0.100.0)"}`)
	if err != nil {
		t.Fatalf("NormalizeCodexUserAgentConfigJSON() error = %v", err)
	}
	_, version, ok := codexUserAgentFromConfig(normalized, 0, "0.160.0")
	if !ok {
		t.Fatal("codexUserAgentFromConfig() ok = false")
	}
	if version != "0.100.0" {
		t.Fatalf("version = %q, want raw 0.100.0", version)
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

func TestCodexUserAgentConfigAllowsSpacedClientName(t *testing.T) {
	// issue #653：ChatGPT 桌面端的 originator 是 "Codex Desktop"，客户端名必须允许空格；
	// 首尾空白与内部连续空白折叠成单个空格。手填的名字会推断为桌面端形态，
	// 末尾标记取目录里最新的桌面端构建号 26.930.51102，CLI 版本沿用手填值。
	raw := `{"client_name":"  Codex \t  Desktop ","client_version":"0.153.3","os_name":"Windows","os_version":"10.0.26100","arch":"x86_64","terminal":"unknown"}`
	normalized, err := NormalizeCodexUserAgentConfigJSON(raw)
	if err != nil {
		t.Fatalf("NormalizeCodexUserAgentConfigJSON() error = %v", err)
	}
	if !strings.Contains(normalized, `"client_name":"Codex Desktop"`) {
		t.Fatalf("normalized = %s, want collapsed client_name \"Codex Desktop\"", normalized)
	}
	userAgent, version, ok := codexUserAgentFromConfig(normalized, 0, "")
	if !ok {
		t.Fatal("codexUserAgentFromConfig() ok = false")
	}
	wantUA := "Codex Desktop/0.153.3 (Windows 10.0.26100; x86_64) unknown (Codex Desktop; 26.930.51102)"
	if userAgent != wantUA {
		t.Fatalf("User-Agent = %q, want %q", userAgent, wantUA)
	}
	if version != "0.153.3" {
		t.Fatalf("version = %q, want 0.153.3", version)
	}
	if !IsCodexOfficialClientByHeaders(userAgent, "") || !IsCodexStrictOfficialClientByHeaders(userAgent, "") {
		t.Fatalf("generated desktop User-Agent %q should be recognized as an official Codex client", userAgent)
	}
	if _, rawVersion, parsed := parseCodexClientVersionDetails(userAgent); !parsed || rawVersion != "0.153.3" {
		t.Fatalf("parseCodexClientVersionDetails(%q) = %q, %v; want 0.153.3, true", userAgent, rawVersion, parsed)
	}
}

func TestCodexUserAgentConfigRejectsStructuralCharacters(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{"client_name paren", `{"client_name":"Codex (Desktop)"}`},
		{"client_name semicolon", `{"client_name":"Codex;Desktop"}`},
		{"client_name control", `{"client_name":"Codex\u0001Desktop"}`},
		// arch/terminal 仍是单 token：真实值（x86_64、WindowsTerminal、vscode/1.100.0）从不含空格。
		{"arch space", `{"arch":"x86 64"}`},
		{"terminal space", `{"terminal":"Windows Terminal"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NormalizeCodexUserAgentConfigJSON(tc.raw); err == nil {
				t.Fatalf("NormalizeCodexUserAgentConfigJSON(%s) error = nil, want rejection", tc.raw)
			}
		})
	}
}

func TestCodexOriginatorForGeneratedUserAgent(t *testing.T) {
	cases := []struct {
		userAgent string
		want      string
	}{
		{"Codex Desktop/0.153.3 (Windows 10.0.26100; x86_64) unknown (Codex Desktop; 26.901.51231)", "Codex Desktop"},
		{"Codex Desktop/0.153.3 (Mac OS 26.4.0; arm64) dumb (codex_exec; 0.153.3)", "Codex Desktop"},
		{"codex-tui/0.153.3 (Mac OS 15.5.0; arm64) xterm-256color (codex-tui; 0.153.3)", "codex-tui"},
		{"codex_cli_rs/0.150.0 (Mac OS 15.5.0; arm64) Apple_Terminal/464", "codex_cli_rs"},
		{"opencode/1.2.3 (Linux Unknown; x86_64) xterm-256color", "opencode"},
		// 非官方前缀 / 无法识别的形状回退到默认 Originator。
		{"my-router", Originator},
		{"my-router/1.0 (custom)", Originator},
		{"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36", Originator},
		{"", Originator},
		{"/0.153.3", Originator},
	}
	for _, tc := range cases {
		if got := CodexOriginatorForGeneratedUserAgent(tc.userAgent); got != tc.want {
			t.Errorf("CodexOriginatorForGeneratedUserAgent(%q) = %q, want %q", tc.userAgent, got, tc.want)
		}
	}
}

// codexUserAgentFromConfig 是测试辅助：忽略版本不可用错误，只断言生成结果。
func codexUserAgentFromConfig(raw string, accountID int64, versionFloor string) (string, string, bool) {
	ua, version, ok, _ := codexUserAgentFromConfigChecked(raw, accountID, versionFloor)
	return ua, version, ok
}
