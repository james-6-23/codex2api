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

// strict 模式的默认 UA 必须对齐实测 codex 0.142.5：
// codex_cli_rs 前缀，且**保留**尾部 "(name; ver)" 后缀（见 codex-fpcap/BASELINE.md）。
func TestStrictDefaultUserAgentMatchesOfficialShape(t *testing.T) {
	ua := strictDefaultCodexCLIUserAgent
	if !strings.HasPrefix(ua, "codex_cli_rs/"+latestCodexCLIVersion+" ") {
		t.Fatalf("strict UA should start with codex_cli_rs/<ver>, got %q", ua)
	}
	// 实测 0.142.5：UA 保留尾部 "(name; ver)" 后缀
	if !strings.HasSuffix(ua, " (codex_cli_rs; "+latestCodexCLIVersion+")") {
		t.Fatalf("strict UA must carry trailing (codex_cli_rs; ver) suffix, got %q", ua)
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
	// 实测 0.142.5：保留尾部后缀，且后缀内 originator 也应为 codex_cli_rs
	if !strings.Contains(p.UserAgent, "(codex_cli_rs;") {
		t.Fatalf("strict profile UA should carry trailing (codex_cli_rs; ver) suffix, got %q", p.UserAgent)
	}
	if strings.Contains(p.UserAgent, "codex-tui") {
		t.Fatalf("strict profile UA must not mention codex-tui, got %q", p.UserAgent)
	}
	// 同一账号在 legacy/strict 下平台段一致（仅 originator 名不同）
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

// strict 模式下 applyCodexRequestHeaders 必须对齐实测 codex 0.142.5：
// 不发 Originator（等于默认 codex_cli_rs）、发 session-id（连字符）、HTTP 路径**不发**
// OpenAI-Beta、发真实 x-codex-* 指纹集（beta-features/window-id(:0)/turn-metadata/
// x-client-request-id/thread-id），且**不发**不存在的 x-codex-installation-id。
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
	// 实测 0.142.5：HTTP /responses 不发 OpenAI-Beta
	if got := req.Header.Get("OpenAI-Beta"); got != "" {
		t.Fatalf("strict HTTP path must NOT send OpenAI-Beta, got %q", got)
	}
	// 真实 x-codex-* 指纹集
	if got := req.Header.Get("x-codex-installation-id"); got != "" {
		t.Fatalf("real codex does NOT send x-codex-installation-id header, got %q", got)
	}
	if got := req.Header.Get("x-codex-window-id"); got != "cache-key-1:0" {
		t.Fatalf("x-codex-window-id = %q, want cache-key-1:0", got)
	}
	if got := req.Header.Get("thread-id"); got != "cache-key-1" {
		t.Fatalf("thread-id = %q, want cache-key-1", got)
	}
	if got := req.Header.Get("x-client-request-id"); got == "" {
		t.Fatalf("strict must send x-client-request-id")
	}
	if got := req.Header.Get("x-codex-beta-features"); got == "" {
		t.Fatalf("strict must send x-codex-beta-features")
	}
	meta := req.Header.Get("x-codex-turn-metadata")
	if !strings.Contains(meta, `"session_id":"cache-key-1"`) || !strings.Contains(meta, `"turn_id"`) || !strings.Contains(meta, `"installation_id"`) {
		t.Fatalf("x-codex-turn-metadata missing expected fields: %q", meta)
	}
	// UA 走 strict 形态（含尾部后缀）
	if ua := req.Header.Get("User-Agent"); !strings.HasPrefix(ua, "codex_cli_rs/") || !strings.Contains(ua, "(codex_cli_rs;") {
		t.Fatalf("strict User-Agent should be codex_cli_rs/ with suffix, got %q", ua)
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
