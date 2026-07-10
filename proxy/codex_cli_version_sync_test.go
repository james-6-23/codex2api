package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExtractCodexCLIVersion(t *testing.T) {
	cases := map[string]string{
		"0.144.1":           "0.144.1",
		"v0.144.1":          "0.144.1",
		"rust-v0.144.1":     "0.144.1",
		"0.144.0-alpha.10":  "", // 预发布带 - 会被 LastIndexByte 截断，这里验证非法回退
		"":                  "",
		"not-a-version":     "",
		"codex-cli-0.145.0": "0.145.0",
	}
	for in, want := range cases {
		if got := extractCodexCLIVersion(in); got != want {
			t.Errorf("extractCodexCLIVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFetchLatestCodexCLIVersion_PrefersNameThenTag(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"name":"0.144.1","tag_name":"rust-v0.144.1","prerelease":false}`))
	}))
	defer server.Close()
	old := codexReleasesLatestURLForTest
	codexReleasesLatestURLForTest = server.URL
	defer func() { codexReleasesLatestURLForTest = old }()

	got, err := FetchLatestCodexCLIVersion(context.Background(), "")
	if err != nil {
		t.Fatalf("FetchLatestCodexCLIVersion error: %v", err)
	}
	if got != "0.144.1" {
		t.Fatalf("version = %q, want 0.144.1", got)
	}
}

func TestFetchLatestCodexCLIVersion_TagFallback(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"name":"Some Release Title","tag_name":"rust-v0.150.0"}`))
	}))
	defer server.Close()
	old := codexReleasesLatestURLForTest
	codexReleasesLatestURLForTest = server.URL
	defer func() { codexReleasesLatestURLForTest = old }()

	got, err := FetchLatestCodexCLIVersion(context.Background(), "")
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if got != "0.150.0" {
		t.Fatalf("version = %q, want 0.150.0", got)
	}
}

// effectiveLatestCodexCLIVersion 绝不低于内置常量：远端返回旧版本时保持内置常量。
func TestEffectiveLatestCodexCLIVersion_NeverDowngrades(t *testing.T) {
	prev := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(prev) })

	s := prev
	s.CodexSyncedCLIVersion = "0.100.0" // 远低于内置
	ApplyRuntimeSettings(s)
	if got := effectiveLatestCodexCLIVersion(); got != latestCodexCLIVersion {
		t.Fatalf("effective = %q, want builtin %q (no downgrade)", got, latestCodexCLIVersion)
	}

	s.CodexSyncedCLIVersion = "9.9.9" // 高于内置
	ApplyRuntimeSettings(s)
	if got := effectiveLatestCodexCLIVersion(); got != "9.9.9" {
		t.Fatalf("effective = %q, want 9.9.9 (upgrade honored)", got)
	}
}

// 同步到高于内置的版本后,生成的出站 UA / 版本号应抬升到该版本。
func TestGeneratedCodexClientHeaders_UsesSyncedVersion(t *testing.T) {
	prev := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(prev) })

	s := prev
	s.ClientCompatMode = ClientCompatModeForce
	s.CodexUserAgentConfig = "{}"
	s.CodexSyncedCLIVersion = "0.200.0"
	ApplyRuntimeSettings(s)

	ua, version := generatedCodexClientHeaders(nil, CurrentRuntimeSettings())
	if version != "0.200.0" {
		t.Fatalf("generated version = %q, want 0.200.0", version)
	}
	if !strings.Contains(ua, "0.200.0") {
		t.Fatalf("generated UA = %q, want to contain 0.200.0", ua)
	}
}
