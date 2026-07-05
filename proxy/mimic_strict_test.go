package proxy

import (
	"net/http"
	"strings"
	"testing"

	"github.com/codex2api/auth"
)

// withMimicMode 临时切换伪装模式并在测试结束后恢复，避免污染其他用例。
func withMimicMode(t *testing.T, mode string) {
	t.Helper()
	prev := CurrentRuntimeSettings()
	next := prev
	next.CodexMimicMode = mode
	ApplyRuntimeSettings(next)
	t.Cleanup(func() { ApplyRuntimeSettings(prev) })
}

func TestNormalizeCodexMimicMode(t *testing.T) {
	cases := map[string]string{
		"":             CodexMimicModeLegacy,
		"legacy":       CodexMimicModeLegacy,
		"unknown":      CodexMimicModeLegacy,
		"strict":       CodexMimicModeStrict,
		"STRICT":       CodexMimicModeStrict,
		"codex_cli_rs": CodexMimicModeStrict,
		"real":         CodexMimicModeStrict,
	}
	for in, want := range cases {
		if got := NormalizeCodexMimicMode(in); got != want {
			t.Fatalf("NormalizeCodexMimicMode(%q) = %q, want %q", in, got, want)
		}
	}
}

// strict 模式的默认 UA 必须对齐官方 codex-rs get_codex_user_agent()：
// codex_cli_rs 前缀，且无尾部 "(name; ver)" 后缀。
func TestStrictDefaultUserAgentMatchesOfficialShape(t *testing.T) {
	ua := strictDefaultCodexCLIUserAgent
	if !strings.HasPrefix(ua, "codex_cli_rs/"+latestCodexCLIVersion+" ") {
		t.Fatalf("strict UA should start with codex_cli_rs/<ver>, got %q", ua)
	}
	// 官方格式共 4 段：name/ver (os osver; arch) terminal —— 不含尾部 "(name; ver)"
	if strings.Contains(ua, "(codex_cli_rs;") || strings.Contains(ua, "(codex-tui;") {
		t.Fatalf("strict UA must NOT carry trailing (name; ver) suffix, got %q", ua)
	}
	if strings.Contains(ua, "codex-tui") {
		t.Fatalf("strict UA must not mention codex-tui, got %q", ua)
	}
}

func TestStripCodexUserAgentSuffix(t *testing.T) {
	legacy := "codex-tui/0.142.3 (Mac OS 15.5.0; arm64) xterm-256color (codex-tui; 0.142.3)"
	got := stripCodexUserAgentSuffix(legacy)
	want := "codex-tui/0.142.3 (Mac OS 15.5.0; arm64) xterm-256color"
	if got != want {
		t.Fatalf("stripCodexUserAgentSuffix() = %q, want %q", got, want)
	}
	// 已经是无后缀的应保持不变（幂等）
	if again := stripCodexUserAgentSuffix(got); again != want {
		t.Fatalf("stripCodexUserAgentSuffix() not idempotent: %q", again)
	}
}

func TestStrictProfileForAccountShape(t *testing.T) {
	p := StrictProfileForAccount(7692)
	if !strings.HasPrefix(p.UserAgent, "codex_cli_rs/") {
		t.Fatalf("strict profile UA should start with codex_cli_rs/, got %q", p.UserAgent)
	}
	if strings.Contains(p.UserAgent, "(codex_cli_rs;") {
		t.Fatalf("strict profile UA must not carry trailing suffix, got %q", p.UserAgent)
	}
	// 同一账号在 legacy/strict 下平台段一致（只有 name 前缀与后缀不同）
	legacy := ProfileForAccount(7692)
	legacyStripped := strictProfileUserAgent(legacy.UserAgent)
	if p.UserAgent != legacyStripped {
		t.Fatalf("strict profile should be deterministic mapping of legacy: %q vs %q", p.UserAgent, legacyStripped)
	}
}

// legacy 模式下 applyCodexRequestHeaders 必须保持历史行为（零 regression）：
// 发 Originator: codex-tui、Session_id（下划线）、不发 OpenAI-Beta、不发 x-codex-*。
func TestApplyCodexRequestHeadersLegacyUnchanged(t *testing.T) {
	withMimicMode(t, CodexMimicModeLegacy)
	req, _ := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
	acc := &auth.Account{AccountID: "acct-123"}
	applyCodexRequestHeaders(req, acc, "tok", "cache-key-1", "apikey-1", nil, http.Header{})

	if got := req.Header.Get("Originator"); got != Originator {
		t.Fatalf("legacy Originator = %q, want %q", got, Originator)
	}
	if got := req.Header.Get("Session_id"); got != "cache-key-1" {
		t.Fatalf("legacy Session_id = %q, want cache-key-1", got)
	}
	if got := req.Header.Get("session-id"); got != "" {
		t.Fatalf("legacy must not send hyphen session-id, got %q", got)
	}
	if got := req.Header.Get("OpenAI-Beta"); got != "" {
		t.Fatalf("legacy must not send OpenAI-Beta, got %q", got)
	}
	if got := req.Header.Get("x-codex-installation-id"); got != "" {
		t.Fatalf("legacy must not send x-codex-installation-id, got %q", got)
	}
}

// strict 模式下 applyCodexRequestHeaders 必须对齐官方 codex-rs：
// 不发 Originator（等于默认 codex_cli_rs）、发 session-id（连字符）、发 OpenAI-Beta、
// 发确定性 x-codex-* 指纹头。
func TestApplyCodexRequestHeadersStrictMatchesOfficial(t *testing.T) {
	withMimicMode(t, CodexMimicModeStrict)
	req, _ := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
	acc := &auth.Account{AccountID: "acct-123"}
	applyCodexRequestHeaders(req, acc, "tok", "cache-key-1", "apikey-1", nil, http.Header{})

	// add_originator_header：默认 originator 时不发 Originator 头
	if got := req.Header.Get("Originator"); got != "" {
		t.Fatalf("strict must omit Originator for default originator, got %q", got)
	}
	// build_session_headers：连字符 session-id，且不发下划线遗留头
	if got := req.Header.Get("session-id"); got != "cache-key-1" {
		t.Fatalf("strict session-id = %q, want cache-key-1", got)
	}
	if got := req.Header.Get("Session_id"); got != "" {
		t.Fatalf("strict must not send legacy Session_id, got %q", got)
	}
	if got := req.Header.Get("Conversation_id"); got != "" {
		t.Fatalf("strict must not send Conversation_id, got %q", got)
	}
	// OpenAI-Beta 补齐
	if got := req.Header.Get("OpenAI-Beta"); got != codexOpenAIBetaHTTPValue {
		t.Fatalf("strict OpenAI-Beta = %q, want %q", got, codexOpenAIBetaHTTPValue)
	}
	// x-codex-* 确定性指纹
	inst := req.Header.Get("x-codex-installation-id")
	win := req.Header.Get("x-codex-window-id")
	if inst == "" || win == "" {
		t.Fatalf("strict must send x-codex-installation-id/window-id, got inst=%q win=%q", inst, win)
	}
	if inst == win {
		t.Fatalf("installation-id and window-id must differ, both=%q", inst)
	}
	// UA 走 strict 形态
	if ua := req.Header.Get("User-Agent"); !strings.HasPrefix(ua, "codex_cli_rs/") {
		t.Fatalf("strict User-Agent should start with codex_cli_rs/, got %q", ua)
	}
}

// x-codex-* 指纹必须账号级确定性（同账号稳定、跨账号不同）。
func TestDeterministicCodexClientUUIDStability(t *testing.T) {
	a1 := deterministicCodexClientUUID("installation", "acct-123")
	a2 := deterministicCodexClientUUID("installation", "acct-123")
	if a1 != a2 {
		t.Fatalf("same seed must yield stable UUID: %q vs %q", a1, a2)
	}
	b := deterministicCodexClientUUID("installation", "acct-999")
	if a1 == b {
		t.Fatalf("different seeds must differ")
	}
	win := deterministicCodexClientUUID("window", "acct-123")
	if a1 == win {
		t.Fatalf("different kinds must differ")
	}
}

// strict 模式下若下游显式带了非默认官方 originator，应转发它（而非省略）。
func TestApplyCodexRequestHeadersStrictForwardsExplicitOriginator(t *testing.T) {
	withMimicMode(t, CodexMimicModeStrict)
	req, _ := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
	down := http.Header{}
	down.Set("Originator", "codex_vscode")
	down.Set("User-Agent", "codex_vscode/0.142.3 (Mac OS 15.5.0; arm64) vscode/1.100.0")
	acc := &auth.Account{AccountID: "acct-123"}
	applyCodexRequestHeaders(req, acc, "tok", "", "apikey-1", nil, down)

	if got := req.Header.Get("Originator"); got != "codex_vscode" {
		t.Fatalf("strict should forward explicit non-default originator, got %q", got)
	}
}
