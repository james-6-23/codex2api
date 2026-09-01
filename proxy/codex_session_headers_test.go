package proxy

import (
	"net/http"
	"testing"

	"github.com/codex2api/auth"
)

func TestApplyCodexSessionHeadersNativeShape(t *testing.T) {
	outbound := http.Header{}
	ApplyCodexSessionHeaders(outbound, nil, "cache-key", nil, false)
	for name, want := range map[string]string{
		"Session-Id":          "cache-key",
		"Thread-Id":           "cache-key",
		"X-Client-Request-Id": "cache-key",
	} {
		if got := outbound.Get(name); got != want {
			t.Fatalf("%s = %q, want %q", name, got, want)
		}
	}
	for _, name := range []string{"Session_id", "Conversation_id"} {
		if got := outbound.Get(name); got != "" {
			t.Fatalf("%s = %q, want empty", name, got)
		}
	}
}

func TestApplyCodexSessionHeadersPreservesRawThreadWithoutSessionConvergence(t *testing.T) {
	downstream := http.Header{}
	downstream.Set("Session-Id", "client-session")
	downstream.Set("Thread-Id", "client-child-thread")

	for _, tt := range []struct {
		name    string
		account *auth.Account
	}{
		{name: "off", account: fingerprintAccount(t, auth.CodexFingerprintModeOff)},
		{name: "device", account: fingerprintAccount(t, auth.CodexFingerprintModeDevice)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			outbound := http.Header{}
			ApplyCodexSessionHeaders(outbound, tt.account, "gateway-cache-key", downstream, false)
			if got := outbound.Get("Session-Id"); got != "gateway-cache-key" {
				t.Fatalf("Session-Id = %q", got)
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
	downstream := http.Header{"Session-Id": []string{"client-session"}, "Thread-Id": []string{"client-child-thread"}}

	sessionAccount := fingerprintAccount(t, auth.CodexFingerprintModeSession)
	_, sessionThread := ConvergedCodexSessionIdentity(sessionAccount, downstream)
	sessionOutbound := http.Header{}
	ApplyCodexSessionHeaders(sessionOutbound, sessionAccount, "gateway-cache-key", downstream, false)
	if sessionThread == "" || sessionOutbound.Get("Thread-Id") != sessionThread {
		t.Fatalf("session Thread-Id = %q, want converged %q", sessionOutbound.Get("Thread-Id"), sessionThread)
	}

	fullAccount := fingerprintAccount(t, auth.CodexFingerprintModeFull)
	fullSession, fullThread := ConvergedCodexSessionIdentity(fullAccount, downstream)
	fullOutbound := http.Header{}
	ApplyCodexSessionHeaders(fullOutbound, fullAccount, "gateway-cache-key", downstream, false)
	if fullSession == "" || fullThread != fullSession || fullOutbound.Get("Thread-Id") != fullThread {
		t.Fatalf("full identity = (%q, %q), outbound thread=%q", fullSession, fullThread, fullOutbound.Get("Thread-Id"))
	}
}

func TestApplyCodexSessionHeadersLegacyModeRestoresOldShape(t *testing.T) {
	t.Setenv("CODEX_SESSION_HEADER_MODE", "legacy")

	httpHeaders := http.Header{}
	ApplyCodexSessionHeaders(httpHeaders, nil, "cache-key", nil, false)
	if httpHeaders.Get("Session_id") != "cache-key" || httpHeaders.Get("Conversation_id") != "" {
		t.Fatalf("legacy HTTP headers = %v", httpHeaders)
	}

	wsHeaders := http.Header{}
	ApplyCodexSessionHeaders(wsHeaders, nil, "cache-key", nil, true)
	if wsHeaders.Get("Session_id") != "cache-key" || wsHeaders.Get("Conversation_id") != "cache-key" {
		t.Fatalf("legacy WS headers = %v", wsHeaders)
	}
}

func TestApplyCodexSessionHeadersConvergedAlignmentIsOptIn(t *testing.T) {
	account := fingerprintAccount(t, auth.CodexFingerprintModeSession)
	downstream := http.Header{"Session-Id": []string{"01a00e75-8856-7542-89bf-35812620690f"}, "Thread-Id": []string{"01a00e75-8856-7542-89bf-35812620690f"}}
	convergedSession, _ := ConvergedCodexSessionIdentity(account, downstream)

	outbound := http.Header{}
	ApplyCodexSessionHeaders(outbound, account, "gateway-cache-key", downstream, false)
	if got := outbound.Get("Session-Id"); got != "gateway-cache-key" {
		t.Fatalf("default Session-Id = %q", got)
	}

	t.Setenv("CODEX_SESSION_HEADER_ALIGN_CONVERGED", "1")
	outbound = http.Header{}
	ApplyCodexSessionHeaders(outbound, account, "gateway-cache-key", downstream, false)
	if got := outbound.Get("Session-Id"); got != convergedSession {
		t.Fatalf("aligned Session-Id = %q, want %q", got, convergedSession)
	}
}
