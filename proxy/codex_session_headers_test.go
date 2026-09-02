package proxy

import (
	"net/http"
	"testing"

	"github.com/codex2api/auth"
)

func TestApplyCodexSessionHeadersNativeShape(t *testing.T) {
	outbound := http.Header{}
	ApplyCodexSessionHeaders(outbound, nil, "cache-key", nil, false)

	// 真实客户端的三个会话头（codex-rs/codex-api/src/endpoint/responses.rs）。
	for name, want := range map[string]string{
		"Session-Id":          "cache-key",
		"Thread-Id":           "cache-key",
		"X-Client-Request-Id": "cache-key",
	} {
		if got := outbound.Get(name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	// 下划线写法与 Conversation_id 都不属于真实形态。同时发新旧两套比只发旧的更可疑。
	for _, name := range []string{"Session_id", "Conversation_id"} {
		if got := outbound.Get(name); got != "" {
			t.Fatalf("%s = %q, want empty", name, got)
		}
	}
}

func TestApplyCodexSessionHeadersClearsLegacyLeftovers(t *testing.T) {
	// 上游改写链路里可能已经有人写过旧头（如账号自定义头、历史透传）；native 档
	// 必须把它们清掉，否则出站同时带两套会话头。
	outbound := http.Header{}
	outbound.Set("Session_id", "stale")
	outbound.Set("Conversation_id", "stale")

	ApplyCodexSessionHeaders(outbound, nil, "cache-key", nil, false)

	for _, name := range []string{"Session_id", "Conversation_id"} {
		if got := outbound.Get(name); got != "" {
			t.Fatalf("%s = %q, want cleared", name, got)
		}
	}
}

func TestApplyCodexSessionHeadersPreservesForwardedClientRequestID(t *testing.T) {
	// x-client-request-id 已由白名单透传 + 指纹收敛定稿时不得覆盖，
	// 否则会把已经对齐的收敛结果冲掉。
	outbound := http.Header{}
	outbound.Set("X-Client-Request-Id", "converged-thread")

	ApplyCodexSessionHeaders(outbound, nil, "cache-key", nil, false)

	if got := outbound.Get("X-Client-Request-Id"); got != "converged-thread" {
		t.Fatalf("X-Client-Request-Id = %q, want 保留已有取值", got)
	}
}

func TestApplyCodexSessionHeadersPreservesRawThreadWithoutSessionConvergence(t *testing.T) {
	downstream := http.Header{}
	downstream.Set("Session-Id", "client-session")
	downstream.Set("Thread-Id", "client-child-thread")

	tests := []struct {
		name    string
		account *auth.Account
	}{
		{name: "off", account: fingerprintAccount(t, auth.CodexFingerprintModeOff)},
		{name: "device", account: fingerprintAccount(t, auth.CodexFingerprintModeDevice)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			outbound := http.Header{}
			ApplyCodexSessionHeaders(outbound, tt.account, "gateway-cache-key", downstream, false)
			if got := outbound.Get("Session-Id"); got != "gateway-cache-key" {
				t.Fatalf("Session-Id = %q, want gateway cache key", got)
			}
			if got := outbound.Get("Thread-Id"); got != "client-child-thread" {
				t.Fatalf("Thread-Id = %q, want raw child thread", got)
			}
			if got := outbound.Get("X-Client-Request-Id"); got != "client-child-thread" {
				t.Fatalf("X-Client-Request-Id = %q, want raw child thread", got)
			}
		})
	}
}

func TestApplyCodexSessionHeadersUsesMetadataThreadFallback(t *testing.T) {
	downstream := http.Header{}
	downstream.Set("Session-Id", "client-session")
	downstream.Set("X-Codex-Turn-Metadata", `{"session_id":"metadata-session","thread_id":"metadata-child"}`)

	outbound := http.Header{}
	ApplyCodexSessionHeaders(outbound, nil, "gateway-cache-key", downstream, false)
	if got := outbound.Get("Thread-Id"); got != "metadata-child" {
		t.Fatalf("Thread-Id = %q, want metadata fallback", got)
	}

	downstream.Set("Thread-Id", "header-child")
	outbound = http.Header{}
	ApplyCodexSessionHeaders(outbound, nil, "gateway-cache-key", downstream, false)
	if got := outbound.Get("Thread-Id"); got != "header-child" {
		t.Fatalf("Thread-Id = %q, want explicit header precedence", got)
	}
}

func TestApplyCodexSessionHeadersFingerprintThreadMatrix(t *testing.T) {
	downstream := http.Header{}
	downstream.Set("Session-Id", "client-session")
	downstream.Set("Thread-Id", "client-child-thread")

	sessionAccount := fingerprintAccount(t, auth.CodexFingerprintModeSession)
	_, sessionThread := ConvergedCodexSessionIdentity(sessionAccount, downstream)
	if sessionThread == "" {
		t.Fatal("session mode did not derive a thread identity")
	}
	sessionOutbound := http.Header{}
	ApplyCodexSessionHeaders(sessionOutbound, sessionAccount, "gateway-cache-key", downstream, false)
	if got := sessionOutbound.Get("Thread-Id"); got != sessionThread {
		t.Fatalf("session Thread-Id = %q, want converged %q", got, sessionThread)
	}

	fullAccount := fingerprintAccount(t, auth.CodexFingerprintModeFull)
	fullSession, fullThread := ConvergedCodexSessionIdentity(fullAccount, downstream)
	if fullSession == "" || fullThread != fullSession {
		t.Fatalf("full mode identity = (%q, %q), want collapsed non-empty values", fullSession, fullThread)
	}
	fullOutbound := http.Header{}
	ApplyCodexSessionHeaders(fullOutbound, fullAccount, "gateway-cache-key", downstream, false)
	if got := fullOutbound.Get("Thread-Id"); got != fullThread {
		t.Fatalf("full Thread-Id = %q, want collapsed %q", got, fullThread)
	}
}

func TestApplyCodexSessionHeadersSkipsEmptySessionID(t *testing.T) {
	// 会话键为空是合法状态（stateless WS + 默认隔离时 resolveHandshakeSessionID
	// 刻意返回空串，好让上游没有任何可绑定的连接级会话身份）。此时一个头都不该发。
	outbound := http.Header{}
	ApplyCodexSessionHeaders(outbound, nil, "   ", nil, false)

	if len(outbound) != 0 {
		t.Fatalf("空会话键仍写出了头: %v", outbound)
	}
}

func TestApplyCodexSessionHeadersLegacyModeRestoresOldShape(t *testing.T) {
	t.Setenv("CODEX_SESSION_HEADER_MODE", "legacy")

	httpHeaders := http.Header{}
	ApplyCodexSessionHeaders(httpHeaders, nil, "cache-key", nil, false)
	if got := httpHeaders.Get("Session_id"); got != "cache-key" {
		t.Fatalf("legacy HTTP Session_id = %q, want %q", got, "cache-key")
	}
	if got := httpHeaders.Get("Conversation_id"); got != "" {
		t.Fatalf("legacy HTTP Conversation_id = %q, want empty（HTTP 旧行为就是删它）", got)
	}
	if got := httpHeaders.Get("Session-Id"); got != "" {
		t.Fatalf("legacy 档不该写连字符头, got %q", got)
	}

	// WS 握手旧行为会连带发 Conversation_id，legacy 的契约是逐路径复原。
	wsHeaders := http.Header{}
	ApplyCodexSessionHeaders(wsHeaders, nil, "cache-key", nil, true)
	if got := wsHeaders.Get("Conversation_id"); got != "cache-key" {
		t.Fatalf("legacy WS Conversation_id = %q, want %q", got, "cache-key")
	}
}

func TestApplyCodexSessionHeadersConvergedAlignmentIsOptIn(t *testing.T) {
	account := fingerprintAccount(t, auth.CodexFingerprintModeSession)
	downstream := http.Header{}
	downstream.Set("Session-Id", "01a00e75-8856-7542-89bf-35812620690f")
	downstream.Set("Thread-Id", "01a00e75-8856-7542-89bf-35812620690f")

	convergedSessionID, convergedThreadID := ConvergedCodexSessionIdentity(account, downstream)
	if convergedSessionID == "" || convergedThreadID == "" {
		t.Fatal("session 档未推导出收敛身份")
	}

	// 默认：出站会话键仍归 resolveUpstreamSessionID 管，收敛不介入——这是本仓库
	// 既有约束，保护的是 prompt cache 隔离语义。
	off := http.Header{}
	ApplyCodexSessionHeaders(off, account, "gateway-cache-key", downstream, false)
	if got := off.Get("Session-Id"); got != "gateway-cache-key" {
		t.Fatalf("默认档 Session-Id = %q, want 网关会话键未被收敛覆盖", got)
	}
	// thread-id 不受该约束限制：这个头此前不存在，且必须与已收敛的
	// x-client-request-id 同值。
	if got := off.Get("Thread-Id"); got != convergedThreadID {
		t.Fatalf("Thread-Id = %q, want converged %q", got, convergedThreadID)
	}

	// 显式开启后 session-id 与 metadata.session_id 对齐。
	t.Setenv("CODEX_SESSION_HEADER_ALIGN_CONVERGED", "1")
	on := http.Header{}
	ApplyCodexSessionHeaders(on, account, "gateway-cache-key", downstream, false)
	if got := on.Get("Session-Id"); got != convergedSessionID {
		t.Fatalf("对齐开启后 Session-Id = %q, want converged %q", got, convergedSessionID)
	}
}
