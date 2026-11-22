package proxy

import (
	"strings"
	"testing"
)

func TestCodexUACatalogIntegrity(t *testing.T) {
	for _, kind := range codexUAKindOrder {
		spec, ok := codexUAKindSpecFor(kind)
		if !ok {
			t.Fatalf("kind %s missing from catalog", kind)
		}
		if spec.ClientName == "" || !validCodexUserAgentClientName(spec.ClientName) {
			t.Fatalf("%s: invalid client name %q", kind, spec.ClientName)
		}
		if spec.AppFollowsCLI == (len(spec.VersionPairs) > 0) {
			t.Fatalf("%s: AppFollowsCLI=%v must be exclusive with version pairs (%d)", kind, spec.AppFollowsCLI, len(spec.VersionPairs))
		}
		if len(spec.Platforms) == 0 || len(spec.Terminals) == 0 || len(spec.AppNames) == 0 {
			t.Fatalf("%s: platforms/terminals/app names must not be empty", kind)
		}
		if !codexUAHasPlatform(spec.Platforms, spec.DefaultPlatform) {
			t.Fatalf("%s: default platform %+v is not an observed platform", kind, spec.DefaultPlatform)
		}
		if !codexUAHasOption(spec.Terminals, spec.DefaultTerminal) {
			t.Fatalf("%s: default terminal %q is not an observed terminal", kind, spec.DefaultTerminal)
		}
		for _, p := range spec.Platforms {
			if p.Weight <= 0 || !validCodexUserAgentPlatformPart(p.OSName) || !validCodexUserAgentPlatformPart(p.OSVersion) || !validCodexUserAgentToken(p.Arch) {
				t.Fatalf("%s: bad platform entry %+v", kind, p)
			}
		}
		for _, term := range spec.Terminals {
			if term.Weight <= 0 || !validCodexUserAgentToken(term.Value) {
				t.Fatalf("%s: bad terminal entry %+v", kind, term)
			}
		}
		for _, term := range spec.ReferenceTerminals {
			// Codex 会把 [A-Za-z0-9-_./] 以外的字符替换成 "_",预设不能出现真实客户端发不出的 token。
			if !validCodexUserAgentToken(term) || strings.IndexFunc(term, func(r rune) bool {
				return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_./", r))
			}) >= 0 {
				t.Fatalf("%s: bad reference terminal %q", kind, term)
			}
		}
		for _, p := range spec.ReferencePlatforms {
			if !validCodexUserAgentPlatformPart(p.OSName) || !validCodexUserAgentPlatformPart(p.OSVersion) || !validCodexUserAgentToken(p.Arch) {
				t.Fatalf("%s: bad reference platform %+v", kind, p)
			}
		}
		for i, pair := range spec.VersionPairs {
			if pair.Weight <= 0 || !validCodexClientVersionString(pair.CLIVersion) || !validCodexUserAgentToken(pair.AppVersion) {
				t.Fatalf("%s: bad version pair %+v", kind, pair)
			}
			if i > 0 {
				if cmp, ok := compareCodexClientVersions(spec.VersionPairs[0].CLIVersion, pair.CLIVersion); !ok || cmp < 0 {
					t.Fatalf("%s: version pairs must start with the newest CLI version, %s precedes %s", kind, spec.VersionPairs[0].CLIVersion, pair.CLIVersion)
				}
			}
		}
	}
	// TUI 默认值必须与历史常量一致,保证既有部署出站字节不变。
	tui := codexUACatalog[CodexClientKindTUI]
	if tui.DefaultPlatform.OSName != defaultCodexUserAgentOSName || tui.DefaultPlatform.OSVersion != defaultCodexUserAgentOSVersion || tui.DefaultPlatform.Arch != defaultCodexUserAgentArch || tui.DefaultTerminal != defaultCodexUserAgentTerminal {
		t.Fatalf("codex-tui defaults drifted from legacy constants: %+v %q", tui.DefaultPlatform, tui.DefaultTerminal)
	}
}

func TestInferCodexClientKind(t *testing.T) {
	cases := map[string]CodexClientKind{
		"":              CodexClientKindTUI,
		"codex-tui":     CodexClientKindTUI,
		"Codex Desktop": CodexClientKindDesktop,
		"codex desktop": CodexClientKindDesktop,
		"codex_vscode":  CodexClientKindVSCode,
		"codex_exec":    CodexClientKindExec,
		"my-router":     CodexClientKindCustom,
		"codex_cli_rs":  CodexClientKindCustom,
	}
	for name, want := range cases {
		if got := inferCodexClientKind(name); got != want {
			t.Errorf("inferCodexClientKind(%q) = %s, want %s", name, got, want)
		}
	}
	if got := effectiveCodexClientKind(CodexUserAgentConfig{ClientKind: "codex-exec", ClientName: "Codex Desktop"}); got != CodexClientKindExec {
		t.Fatalf("explicit client_kind must win over inference, got %s", got)
	}
}

func TestResolveCodexVersionPairDesktop(t *testing.T) {
	spec := codexUACatalog[CodexClientKindDesktop]
	cases := []struct {
		name          string
		cli, app      string
		floor         string
		wantCLI, want string
	}{
		{"auto default is latest build", "", "", "", "0.160.0", "26.930.51102"},
		{"manual CLI overrides latest default", "0.153.3", "", "", "0.153.3", "26.930.51102"},
		{"manual newer CLI retained", "0.153.5", "", "", "0.153.5", "26.930.51102"},
		{"manual old CLI ignores floor", "0.152.0", "", "0.153.0", "0.152.0", "26.930.51102"},
		{"unavailable floor produces no invented pair", "", "", "0.161.0", "", ""},
		{"explicit build untouched", "0.153.4", "26.901.41600", "", "0.153.4", "26.901.41600"},
		{"both overrides ignore floor", "0.152.0", "26.831.20005", "0.153.0", "0.152.0", "26.831.20005"},
		{"both overrides ignore unavailable floor", "0.152.0", "26.831.20005", "0.161.0", "0.152.0", "26.831.20005"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotCLI, gotApp := resolveCodexVersionPair(spec, tc.cli, tc.app, tc.floor)
			if gotCLI != tc.wantCLI || gotApp != tc.want {
				t.Fatalf("resolveCodexVersionPair(%q, %q, floor %q) = (%s, %s), want (%s, %s)", tc.cli, tc.app, tc.floor, gotCLI, gotApp, tc.wantCLI, tc.want)
			}
		})
	}
}

func TestResolveCodexVersionPairFollowsCLIForTUI(t *testing.T) {
	spec := codexUACatalog[CodexClientKindTUI]
	if cli, app := resolveCodexVersionPair(spec, "", "", ""); cli != latestCodexCLIVersion || app != cli {
		t.Fatalf("tui default = (%s, %s), want latest CLI twice", cli, app)
	}
	if cli, app := resolveCodexVersionPair(spec, "0.140.0", "", "0.150.0"); cli != "0.140.0" || app != "0.140.0" {
		t.Fatalf("tui floor = (%s, %s), want manual 0.140.0 twice", cli, app)
	}
	if cli, app := resolveCodexVersionPair(nil, "0.140.0", "9.9", ""); cli != "0.140.0" || app != "9.9" {
		t.Fatalf("custom explicit = (%s, %s), want (0.140.0, 9.9)", cli, app)
	}
}

func TestBuildCodexStructuredUserAgentByKind(t *testing.T) {
	cases := []struct {
		name   string
		raw    string
		wantUA string
		wantV  string
	}{
		{"desktop preset", `{"client_kind":"codex-desktop"}`,
			"Codex Desktop/0.160.0 (Windows 10.0.26200; x86_64) unknown (Codex Desktop; 26.930.51102)", "0.160.0"},
		{"vscode preset with cursor host", `{"client_kind":"codex-vscode","app_name":"Cursor"}`,
			"codex_vscode/0.160.0 (Ubuntu 22.4.0; x86_64) unknown (Cursor; 26.930.41038)", "0.160.0"},
		{"exec preset follows cli", `{"client_kind":"codex-exec"}`,
			"codex_exec/" + latestCodexCLIVersion + " (Windows 10.0.19045; x86_64) unknown (codex_exec; " + latestCodexCLIVersion + ")", latestCodexCLIVersion},
		{"legacy tui config unchanged", `{"client_name":"codex-tui"}`,
			"codex-tui/" + latestCodexCLIVersion + " (Mac OS 15.5.0; arm64) xterm-256color (codex-tui; " + latestCodexCLIVersion + ")", latestCodexCLIVersion},
		{"custom keeps free-form marker", `{"client_kind":"custom","client_name":"my-router","app_name":"My App","app_version":"9.9"}`,
			"my-router/" + latestCodexCLIVersion + " (Mac OS 15.5.0; arm64) xterm-256color (My App; 9.9)", latestCodexCLIVersion},
		{"desktop with mac platform and explicit cli", `{"client_kind":"codex-desktop","client_version":"0.153.3","os_name":"Mac OS","os_version":"26.5.2","arch":"arm64"}`,
			"Codex Desktop/0.153.3 (Mac OS 26.5.2; arm64) unknown (Codex Desktop; 26.930.51102)", "0.153.3"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			normalized, err := NormalizeCodexUserAgentConfigJSON(tc.raw)
			if err != nil {
				t.Fatalf("NormalizeCodexUserAgentConfigJSON() error = %v", err)
			}
			ua, version, ok := codexUserAgentFromConfig(normalized, 0, "")
			if !ok {
				t.Fatal("codexUserAgentFromConfig() ok = false")
			}
			if ua != tc.wantUA {
				t.Fatalf("User-Agent = %q, want %q", ua, tc.wantUA)
			}
			if version != tc.wantV {
				t.Fatalf("version = %q, want %q", version, tc.wantV)
			}
			if got := CodexOriginatorForGeneratedUserAgent(ua); tc.name != "custom keeps free-form marker" && got == Originator && !strings.HasPrefix(ua, "codex-tui/") {
				t.Fatalf("Originator for %q fell back to default", ua)
			}
		})
	}
}

func TestCodexPoolPersonaDeterministicAndHonorsMix(t *testing.T) {
	normalized, err := NormalizeCodexUserAgentConfigJSON(`{"mode":"pool","pool_mix":{"codex-desktop":100,"codex-tui":0}}`)
	if err != nil {
		t.Fatalf("NormalizeCodexUserAgentConfigJSON() error = %v", err)
	}
	distinct := map[string]struct{}{}
	for id := int64(1); id <= 50; id++ {
		ua, version, ok := codexUserAgentFromConfig(normalized, id, "")
		if !ok {
			t.Fatalf("account %d: ok = false", id)
		}
		if !strings.HasPrefix(ua, "Codex Desktop/") || !strings.Contains(ua, " (Codex Desktop; 26.") {
			t.Fatalf("account %d: pool with desktop-only mix produced %q", id, ua)
		}
		if !strings.HasPrefix(ua, "Codex Desktop/"+version+" ") {
			t.Fatalf("account %d: Version header %q does not match UA %q", id, version, ua)
		}
		again, _, _ := codexUserAgentFromConfig(normalized, id, "")
		if again != ua {
			t.Fatalf("account %d: persona not deterministic: %q vs %q", id, ua, again)
		}
		distinct[ua] = struct{}{}
	}
	if len(distinct) < 5 {
		t.Fatalf("pool should spread personas across accounts, got %d distinct", len(distinct))
	}

	defaultMix, err := NormalizeCodexUserAgentConfigJSON(`{"mode":"pool"}`)
	if err != nil {
		t.Fatalf("NormalizeCodexUserAgentConfigJSON(default mix) error = %v", err)
	}
	seen := map[string]int{}
	for id := int64(1); id <= 300; id++ {
		ua, _, ok := codexUserAgentFromConfig(defaultMix, id, "")
		if !ok {
			t.Fatalf("account %d: ok = false", id)
		}
		seen[codexUserAgentClientName(ua)]++
	}
	for _, name := range []string{"Codex Desktop", "codex_vscode", "codex-tui"} {
		if seen[name] == 0 {
			t.Fatalf("default pool mix never produced %s: %v", name, seen)
		}
	}
	if seen["Codex Desktop"] <= seen["codex-tui"] {
		t.Fatalf("default mix should favor desktop over tui: %v", seen)
	}
}

func TestCodexPoolPersonaRespectsVersionFloor(t *testing.T) {
	normalized, err := NormalizeCodexUserAgentConfigJSON(`{"mode":"pool","pool_mix":{"codex-desktop":1,"codex-vscode":1}}`)
	if err != nil {
		t.Fatalf("NormalizeCodexUserAgentConfigJSON() error = %v", err)
	}
	for id := int64(1); id <= 60; id++ {
		ua, version, ok := codexUserAgentFromConfig(normalized, id, "0.153.3")
		if !ok {
			t.Fatalf("account %d: ok = false", id)
		}
		if cmp, parsed := compareCodexClientVersions(version, "0.153.3"); !parsed || cmp < 0 {
			t.Fatalf("account %d: version %s below floor in %q", id, version, ua)
		}
	}
}

func TestCodexUserAgentConfigValidatesNewFields(t *testing.T) {
	bad := []string{
		`{"client_kind":"desktop"}`,
		`{"mode":"random"}`,
		`{"mode":"pool","pool_mix":{"opencode":1}}`,
		`{"mode":"pool","pool_mix":{"custom":1}}`,
		`{"mode":"pool","pool_mix":{"codex-tui":-1}}`,
		`{"mode":"pool","pool_mix":{"codex-tui":0}}`,
		`{"app_name":"VS (Code)"}`,
		`{"app_version":"26.901 51231"}`,
	}
	for _, raw := range bad {
		if _, err := NormalizeCodexUserAgentConfigJSON(raw); err == nil {
			t.Errorf("NormalizeCodexUserAgentConfigJSON(%s) error = nil, want rejection", raw)
		}
	}
	got, err := NormalizeCodexUserAgentConfigJSON(`{"mode":"single","client_kind":"CODEX-DESKTOP","app_name":"  VS   Code "}`)
	if err != nil {
		t.Fatalf("NormalizeCodexUserAgentConfigJSON() error = %v", err)
	}
	if got != `{"client_kind":"codex-desktop","app_name":"VS Code"}` {
		t.Fatalf("normalized = %s", got)
	}
	if got, _ := NormalizeCodexUserAgentConfigJSON(`{"mode":"single"}`); got != "{}" {
		t.Fatalf("mode=single alone should normalize to empty config, got %s", got)
	}
}

func TestPreviewCodexUserAgentConfig(t *testing.T) {
	preview, err := PreviewCodexUserAgentConfig(`{"client_kind":"codex-desktop","terminal":"WindowsTerminal"}`, "", nil)
	if err != nil {
		t.Fatalf("PreviewCodexUserAgentConfig() error = %v", err)
	}
	if preview.Mode != CodexUserAgentModeSingle || preview.Kind != string(CodexClientKindDesktop) || preview.Persona == nil {
		t.Fatalf("unexpected preview shape: %+v", preview)
	}
	if preview.Persona.Originator != "Codex Desktop" || preview.Persona.Version != "0.160.0" {
		t.Fatalf("persona = %+v", preview.Persona)
	}
	if len(preview.Warnings) != 1 || preview.Warnings[0] != "terminal" {
		t.Fatalf("warnings = %v, want [terminal] for a terminal never seen with the desktop app", preview.Warnings)
	}

	pool, err := PreviewCodexUserAgentConfig(`{"mode":"pool"}`, "", []int64{11, 22, 33})
	if err != nil {
		t.Fatalf("PreviewCodexUserAgentConfig(pool) error = %v", err)
	}
	if pool.Mode != CodexUserAgentModePool || len(pool.Samples) != 3 || pool.Samples[1].AccountID != 22 {
		t.Fatalf("unexpected pool preview: %+v", pool)
	}
	for _, s := range pool.Samples {
		if s.UserAgent == "" || s.Originator == "" || s.Version == "" {
			t.Fatalf("incomplete sample %+v", s)
		}
	}

	ref, err := PreviewCodexUserAgentConfig(`{"client_kind":"codex-tui","terminal":"iTerm.app/3.7.3"}`, "", nil)
	if err != nil || len(ref.Warnings) != 0 || !strings.Contains(ref.Persona.UserAgent, " iTerm.app/3.7.3 ") {
		t.Fatalf("reference terminal preview = %+v, err %v", ref, err)
	}
	mac27, err := PreviewCodexUserAgentConfig(`{"client_kind":"codex-desktop","os_name":"Mac OS","os_version":"27.0.0","arch":"arm64"}`, "", nil)
	if err != nil || len(mac27.Warnings) != 0 || !strings.Contains(mac27.Persona.UserAgent, " (Mac OS 27.0.0; arm64) ") {
		t.Fatalf("reference platform preview = %+v, err %v", mac27, err)
	}

	empty, err := PreviewCodexUserAgentConfig(`{}`, "", nil)
	if err != nil || empty.Persona == nil || !strings.HasPrefix(empty.Persona.UserAgent, "codex-tui/") {
		t.Fatalf("empty config preview = %+v, err %v", empty, err)
	}
	if _, err := PreviewCodexUserAgentConfig(`{"client_kind":"nope"}`, "", nil); err == nil {
		t.Fatal("invalid config must surface as error")
	}
}

func TestCodexUserAgentCatalogReferenceTerminals(t *testing.T) {
	view := CodexUserAgentCatalog()
	for _, kind := range view.Kinds {
		spec := codexUACatalog[CodexClientKind(kind.Kind)]
		if kind.ReferenceTerminals == nil {
			t.Fatalf("%s: reference_terminals must serialize as an array", kind.Kind)
		}
		for _, term := range kind.ReferenceTerminals {
			if codexUAHasOption(spec.Terminals, term) {
				t.Fatalf("%s: reference terminal %q duplicates an observed terminal", kind.Kind, term)
			}
		}
		if len(kind.ReferencePlatforms) == 0 {
			t.Fatalf("%s: Mac OS 27.0.0 reference platform missing", kind.Kind)
		}
		for _, p := range kind.ReferencePlatforms {
			if codexUAHasPlatform(spec.Platforms, codexUAPlatform{OSName: p.OSName, OSVersion: p.OSVersion, Arch: p.Arch}) {
				t.Fatalf("%s: reference platform %+v duplicates an observed platform", kind.Kind, p)
			}
		}
	}
	if len(codexUACatalog[CodexClientKindDesktop].ReferenceTerminals) != 0 {
		t.Fatal("desktop app never runs inside a terminal; it must not offer terminal presets")
	}
}
