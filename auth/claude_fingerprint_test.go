package auth

import (
	"strings"
	"testing"
)

func TestGenerateClaudeFingerprint_Fields(t *testing.T) {
	fp := GenerateClaudeFingerprint("")
	if !strings.HasPrefix(fp.UserAgent, "claude-cli/") || !strings.Contains(fp.UserAgent, "(external, cli)") {
		t.Fatalf("UA 不像 Claude Code CLI: %s", fp.UserAgent)
	}
	if fp.XApp != "cli" {
		t.Errorf("x-app 应为 cli, got %s", fp.XApp)
	}
	if fp.StainlessLang != "js" || fp.StainlessRuntime != "node" {
		t.Errorf("stainless lang/runtime 不符: %s/%s", fp.StainlessLang, fp.StainlessRuntime)
	}
	if fp.StainlessOS == "" || fp.StainlessArch == "" || fp.StainlessRuntimeVersion == "" || fp.StainlessPackageVersion == "" {
		t.Error("stainless os/arch/runtime-version/package-version 不应为空")
	}
}

func TestGenerateClaudeFingerprint_TimezoneValidation(t *testing.T) {
	if fp := GenerateClaudeFingerprint("Asia/Shanghai"); fp.Timezone != "Asia/Shanghai" {
		t.Errorf("合法时区应保留, got %q", fp.Timezone)
	}
	if fp := GenerateClaudeFingerprint("Not/A_Zone"); fp.Timezone != "" {
		t.Errorf("非法时区应丢弃, got %q", fp.Timezone)
	}
}

func TestClaudeFingerprintHeaders(t *testing.T) {
	fp := GenerateClaudeFingerprint("")
	h := fp.Headers()
	for _, k := range []string{"User-Agent", "X-App", "X-Stainless-Lang", "X-Stainless-OS", "X-Stainless-Arch", "X-Stainless-Runtime", "X-Stainless-Runtime-Version", "X-Stainless-Package-Version"} {
		if strings.TrimSpace(h[k]) == "" {
			t.Errorf("Headers() 缺少 %s", k)
		}
	}
}

func TestGenerateClaudeFingerprint_ArchMatchesOS(t *testing.T) {
	// Windows 只应出现 x64（池约束）。多次抽样验证不越界。
	for i := 0; i < 30; i++ {
		fp := GenerateClaudeFingerprint("")
		valid := claudeArchByOS[fp.StainlessOS]
		found := false
		for _, a := range valid {
			if a == fp.StainlessArch {
				found = true
			}
		}
		if !found {
			t.Fatalf("os=%s 的 arch=%s 不在允许集 %v", fp.StainlessOS, fp.StainlessArch, valid)
		}
	}
}

func TestGenerateClaudeFingerprint_UsesEffectiveCLIVersion(t *testing.T) {
	t.Cleanup(func() { SetClaudeSyncedCLIVersion("") })
	SetClaudeSyncedCLIVersion("2.1.300")
	for i := 0; i < 10; i++ {
		fp := GenerateClaudeFingerprint("")
		if fp.UserAgent != "claude-cli/2.1.300 (external, cli)" {
			t.Fatalf("UA 应使用生效版本, got %s", fp.UserAgent)
		}
	}
	SetClaudeSyncedCLIVersion("")
	if fp := GenerateClaudeFingerprint(""); fp.UserAgent != "claude-cli/"+BuiltinClaudeCLIVersion+" (external, cli)" {
		t.Fatalf("无同步值时应使用内置版本, got %s", fp.UserAgent)
	}
}
