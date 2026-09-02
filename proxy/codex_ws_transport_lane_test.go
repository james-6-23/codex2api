package proxy

import (
	"net/http"
	"strings"
	"testing"
)

func TestResolveCodexWebsocketTransportSessionKeySeparatesChildThread(t *testing.T) {
	const upstream = "isolated-upstream-session"
	parentHeaders := http.Header{"Session-Id": []string{"client-session"}, "Thread-Id": []string{"client-session"}}
	childHeaders := http.Header{"Session-Id": []string{"client-session"}, "Thread-Id": []string{"client-child-thread"}}

	if got := ResolveCodexWebsocketTransportSessionKey(upstream, parentHeaders); got != upstream {
		t.Fatalf("parent lane = %q, want shared upstream session", got)
	}
	child := ResolveCodexWebsocketTransportSessionKey(upstream, childHeaders)
	if child == upstream {
		t.Fatal("child thread collapsed onto shared upstream session")
	}
	if repeat := ResolveCodexWebsocketTransportSessionKey(upstream, childHeaders); repeat != child {
		t.Fatalf("child lane is not stable: first=%q repeat=%q", child, repeat)
	}
	for _, raw := range []string{upstream, "client-session", "client-child-thread"} {
		if strings.Contains(child, raw) {
			t.Fatalf("raw identity %q leaked into transport lane %q", raw, child)
		}
	}
}

func TestResolveCodexWebsocketTransportSessionKeyMetadataAndFallbacks(t *testing.T) {
	const upstream = "isolated-upstream-session"
	metadata := http.Header{}
	metadata.Set("X-Codex-Turn-Metadata", `{"session_id":"client-session","thread_id":"client-child-thread"}`)
	if got := ResolveCodexWebsocketTransportSessionKey(upstream, metadata); got == upstream {
		t.Fatal("metadata child thread did not get a separate lane")
	}
	if got := ResolveCodexWebsocketTransportSessionKey(upstream, http.Header{}); got != upstream {
		t.Fatalf("missing identity lane = %q, want legacy upstream session", got)
	}
	if got := ResolveCodexWebsocketTransportSessionKey("stateless-request", metadata); got != "stateless-request" {
		t.Fatalf("stateless lane = %q, want existing stateless routing", got)
	}

	equalMetadata := http.Header{}
	equalMetadata.Set("X-Codex-Turn-Metadata", `{"session_id":"same","thread_id":"same"}`)
	if got := ResolveCodexWebsocketTransportSessionKey(upstream, equalMetadata); got != upstream {
		t.Fatalf("equal metadata identity lane = %q, want legacy upstream session", got)
	}
	malformed := http.Header{}
	malformed.Set("X-Codex-Turn-Metadata", `{not-json`)
	if got := ResolveCodexWebsocketTransportSessionKey(upstream, malformed); got != upstream {
		t.Fatalf("malformed metadata lane = %q, want legacy upstream session", got)
	}
	explicitWins := metadata.Clone()
	explicitWins.Set("Thread-Id", "explicit-child")
	if got := ResolveCodexWebsocketTransportSessionKey(upstream, explicitWins); got == ResolveCodexWebsocketTransportSessionKey(upstream, metadata) {
		t.Fatal("explicit Thread-Id did not take precedence over metadata thread_id")
	}
}
