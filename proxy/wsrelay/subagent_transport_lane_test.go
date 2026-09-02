package wsrelay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestWebsocketTransportSessionKeySeparatesSubagentThreads(t *testing.T) {
	const session = "shared-upstream-session"
	parentHeaders := http.Header{"Session-Id": []string{session}, "Thread-Id": []string{"parent-thread"}}
	childHeaders := http.Header{"Session-Id": []string{session}, "Thread-Id": []string{"child-thread"}}

	parent := proxy.ResolveCodexWebsocketTransportSessionKey(session, parentHeaders)
	child := proxy.ResolveCodexWebsocketTransportSessionKey(session, childHeaders)
	if parent == session || child == session {
		t.Fatalf("threaded lanes fell back to shared session: parent=%q child=%q", parent, child)
	}
	if parent == child {
		t.Fatalf("parent and child collapsed onto one transport lane: %q", parent)
	}
	if repeat := proxy.ResolveCodexWebsocketTransportSessionKey(session, childHeaders); repeat != child {
		t.Fatalf("child lane is not stable: first=%q repeat=%q", child, repeat)
	}
	for _, raw := range []string{session, "parent-thread", "child-thread"} {
		if strings.Contains(parent, raw) || strings.Contains(child, raw) {
			t.Fatalf("raw identity %q leaked into transport lane", raw)
		}
	}
}

func TestWebsocketTransportSessionKeyFallbacks(t *testing.T) {
	const session = "shared-upstream-session"
	if got := proxy.ResolveCodexWebsocketTransportSessionKey(session, http.Header{}); got != session {
		t.Fatalf("missing thread lane = %q, want legacy session", got)
	}
	same := http.Header{"Session-Id": []string{session}, "Thread-Id": []string{session}}
	if got := proxy.ResolveCodexWebsocketTransportSessionKey(session, same); got != session {
		t.Fatalf("single-thread lane = %q, want legacy session", got)
	}
	if got := proxy.ResolveCodexWebsocketTransportSessionKey("stateless-request", http.Header{"Thread-Id": []string{"thread"}}); got != "stateless-request" {
		t.Fatalf("stateless lane = %q, want existing stateless routing", got)
	}
}

func TestSubagentTransportLanesDoNotChangeUpstreamSessionOrPromptCache(t *testing.T) {
	const session = "shared-upstream-session"
	executor := NewExecutorWithManager(NewManager())
	t.Cleanup(executor.manager.Stop)
	account := &auth.Account{DBID: 44, AccountID: "workspace-account"}
	requestBody := []byte(`{"model":"gpt-5.4","input":"hello"}`)

	parentHeaders := http.Header{"Session-Id": []string{"client-session"}, "Thread-Id": []string{"parent-thread"}}
	childHeaders := http.Header{"Session-Id": []string{"client-session"}, "Thread-Id": []string{"child-thread"}}
	parentLane := proxy.ResolveCodexWebsocketTransportSessionKey(session, parentHeaders)
	childLane := proxy.ResolveCodexWebsocketTransportSessionKey(session, childHeaders)
	if parentLane == childLane {
		t.Fatal("parent and child transport lanes collapsed")
	}

	parentBody := executor.prepareWebsocketBody(requestBody, session)
	childBody := executor.prepareWebsocketBody(requestBody, session)
	for name, body := range map[string][]byte{"parent": parentBody, "child": childBody} {
		if got := gjson.GetBytes(body, "prompt_cache_key").String(); got != session {
			t.Fatalf("%s prompt_cache_key = %q, want shared upstream session", name, got)
		}
	}

	parentOutbound := executor.prepareWebsocketHeaders("token", account, account.AccountID, session, "api-key", nil, parentHeaders, parentBody)
	childOutbound := executor.prepareWebsocketHeaders("token", account, account.AccountID, session, "api-key", nil, childHeaders, childBody)
	for name, headers := range map[string]http.Header{"parent": parentOutbound, "child": childOutbound} {
		if got := headers.Get("Session-Id"); got != session {
			t.Fatalf("%s outbound Session-Id = %q, want shared upstream session", name, got)
		}
	}
	if got := parentOutbound.Get("Thread-Id"); got != "parent-thread" {
		t.Fatalf("parent outbound Thread-Id = %q", got)
	}
	if got := childOutbound.Get("Thread-Id"); got != "child-thread" {
		t.Fatalf("child outbound Thread-Id = %q", got)
	}
}

func TestSubagentTransportLanesAvoidSharedSessionBusyWait(t *testing.T) {
	frames := make(chan string, 4)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
			frames <- r.Header.Get("Thread-Id")
		}
	}))
	t.Cleanup(server.Close)
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
	account := &auth.Account{DBID: 42, DynamicConcurrencyLimit: 4}

	t.Run("shared session reproduces serialization", func(t *testing.T) {
		manager := NewManager()
		t.Cleanup(manager.Stop)
		first, firstPending, err := manager.AcquireConnection(context.Background(), account, wsURL, "shared-session", http.Header{}, "")
		if err != nil {
			t.Fatalf("acquire first connection: %v", err)
		}
		t.Cleanup(func() {
			first.session.RemovePendingRequest(firstPending.RequestID)
			manager.DiscardConnection(first)
		})

		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		if _, _, err := manager.AcquireConnection(ctx, account, wsURL, "shared-session", http.Header{}, ""); err == nil {
			t.Fatal("second request unexpectedly bypassed the busy shared session")
		}
	})

	t.Run("distinct threads acquire in parallel", func(t *testing.T) {
		manager := NewManager()
		t.Cleanup(manager.Stop)
		const session = "shared-session"
		parentHeaders := http.Header{"Session-Id": []string{session}, "Thread-Id": []string{"parent-thread"}}
		childHeaders := http.Header{"Session-Id": []string{session}, "Thread-Id": []string{"child-thread"}}
		parentLane := proxy.ResolveCodexWebsocketTransportSessionKey(session, parentHeaders)
		childLane := proxy.ResolveCodexWebsocketTransportSessionKey(session, childHeaders)
		if parentLane == childLane {
			t.Fatal("parallel transport setup collapsed parent and child lanes")
		}

		parent, parentPending, err := manager.AcquireConnection(context.Background(), account, wsURL, parentLane, parentHeaders, "")
		if err != nil {
			t.Fatalf("acquire parent lane: %v", err)
		}
		t.Cleanup(func() {
			parent.session.RemovePendingRequest(parentPending.RequestID)
			manager.DiscardConnection(parent)
		})
		executor := NewExecutorWithManager(manager)
		if err := executor.sendRequest(parent, []byte(`{"type":"response.create","input":"parent"}`), parentPending.RequestID); err != nil {
			t.Fatalf("send parent request: %v", err)
		}
		select {
		case got := <-frames:
			if got != "parent-thread" {
				t.Fatalf("fake upstream parent Thread-Id = %q", got)
			}
		case <-time.After(time.Second):
			t.Fatal("fake upstream did not receive the blocked parent request")
		}

		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		child, childPending, err := manager.AcquireConnection(ctx, account, wsURL, childLane, childHeaders, "")
		if err != nil {
			t.Fatalf("child lane waited behind parent: %v", err)
		}
		if err := executor.sendRequest(child, []byte(`{"type":"response.create","input":"child"}`), childPending.RequestID); err != nil {
			t.Fatalf("send child request: %v", err)
		}
		select {
		case got := <-frames:
			if got != "child-thread" {
				t.Fatalf("fake upstream child Thread-Id = %q", got)
			}
		case <-time.After(time.Second):
			t.Fatal("fake upstream did not receive child while parent remained pending")
		}
		child.session.RemovePendingRequest(childPending.RequestID)
		manager.DiscardConnection(child)
		if child == parent || child.PoolKey == parent.PoolKey {
			t.Fatal("parent and child reused the same frozen-handshake connection")
		}
	})

	t.Run("account capacity remains authoritative", func(t *testing.T) {
		manager := NewManager()
		t.Cleanup(manager.Stop)
		limitedAccount := &auth.Account{DBID: 43, DynamicConcurrencyLimit: 1}
		const session = "capacity-session"
		parentHeaders := http.Header{"Session-Id": []string{session}, "Thread-Id": []string{"parent-thread"}}
		childHeaders := http.Header{"Session-Id": []string{session}, "Thread-Id": []string{"child-thread"}}
		parentLane := proxy.ResolveCodexWebsocketTransportSessionKey(session, parentHeaders)
		childLane := proxy.ResolveCodexWebsocketTransportSessionKey(session, childHeaders)
		if parentLane == childLane {
			t.Fatal("capacity control setup collapsed parent and child lanes")
		}

		parent, parentPending, err := manager.AcquireConnection(context.Background(), limitedAccount, wsURL, parentLane, parentHeaders, "")
		if err != nil {
			t.Fatalf("acquire capacity parent: %v", err)
		}
		t.Cleanup(func() {
			parent.session.RemovePendingRequest(parentPending.RequestID)
			manager.DiscardConnection(parent)
		})

		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()
		if _, _, err := manager.AcquireConnection(ctx, limitedAccount, wsURL, childLane, childHeaders, ""); err == nil {
			t.Fatal("child bypassed the account connection capacity limit")
		}
	})
}

func TestSubagentTransportLaneKeepsPreviousResponseConnectionPriority(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)
	manager.probeFunc = func(*WsConnection) bool { return true }

	const session = "shared-session"
	parentHeaders := http.Header{"Session-Id": []string{session}, "Thread-Id": []string{"parent-thread"}}
	childHeaders := http.Header{"Session-Id": []string{session}, "Thread-Id": []string{"child-thread"}}
	parentLane := proxy.ResolveCodexWebsocketTransportSessionKey(session, parentHeaders)
	childLane := proxy.ResolveCodexWebsocketTransportSessionKey(session, childHeaders)
	if parentLane == childLane {
		t.Fatal("test setup collapsed parent and child lanes")
	}

	parent := newBoundTestConn(t, manager, 7, parentLane)
	manager.BindResponseConn("resp_parent", parent, parentLane, 7, "api-key")

	poolSessionID := childLane
	got, pending, slotKey := manager.AcquirePreferredConnection("resp_parent", 7, "api-key")
	if got != nil {
		poolSessionID = slotKey
	}
	if got != parent || pending == nil {
		t.Fatal("previous_response_id did not acquire the producing parent connection")
	}
	defer parent.session.RemovePendingRequest(pending.RequestID)
	if poolSessionID != parentLane {
		t.Fatalf("continuation pool lane = %q, want producer lane %q", poolSessionID, parentLane)
	}
	if got := manager.ConnectionCount(); got != 1 {
		t.Fatalf("preferred continuation created an extra child-lane connection: count=%d", got)
	}
}

func TestActualWebsocketPoolSessionIDUsesOverflowSlot(t *testing.T) {
	const baseLane = "ws-thread-base"
	overflow := &WsConnection{session: &Session{ID: baseLane + "#ovf-1"}}
	if got := actualWebsocketPoolSessionID(overflow, baseLane); got != baseLane+"#ovf-1" {
		t.Fatalf("actual pool session = %q, want overflow slot", got)
	}
	if got := actualWebsocketPoolSessionID(nil, baseLane); got != baseLane {
		t.Fatalf("nil connection fallback = %q, want %q", got, baseLane)
	}
}
