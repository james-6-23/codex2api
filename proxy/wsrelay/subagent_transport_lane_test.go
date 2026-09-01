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
	parentHeaders := http.Header{"Session-Id": []string{"client-session"}, "Thread-Id": []string{"parent-thread"}}
	childHeaders := http.Header{"Session-Id": []string{"client-session"}, "Thread-Id": []string{"child-thread"}}

	parent := proxy.ResolveCodexWebsocketTransportSessionKey(session, parentHeaders)
	child := proxy.ResolveCodexWebsocketTransportSessionKey(session, childHeaders)
	if parent == session || child == session || parent == child {
		t.Fatalf("thread lanes collapsed: parent=%q child=%q", parent, child)
	}
}

func TestSubagentTransportLanesDoNotChangeUpstreamSessionOrPromptCache(t *testing.T) {
	const session = "shared-upstream-session"
	executor := NewExecutorWithManager(NewManager())
	t.Cleanup(executor.manager.Stop)
	account := &auth.Account{DBID: 44, AccountID: "workspace-account"}
	requestBody := []byte(`{"model":"gpt-5.4","input":"hello"}`)

	for name, headers := range map[string]http.Header{
		"parent": {"Session-Id": []string{"client-session"}, "Thread-Id": []string{"parent-thread"}},
		"child":  {"Session-Id": []string{"client-session"}, "Thread-Id": []string{"child-thread"}},
	} {
		body := executor.prepareWebsocketBody(requestBody, session)
		if got := gjson.GetBytes(body, "prompt_cache_key").String(); got != session {
			t.Fatalf("%s prompt_cache_key = %q, want shared session", name, got)
		}
		outbound := executor.prepareWebsocketHeaders("token", account, account.AccountID, session, "api-key", nil, headers, body)
		if got := outbound.Get("Session-Id"); got != session {
			t.Fatalf("%s upstream Session-Id = %q", name, got)
		}
		if got := outbound.Get("Thread-Id"); got != headers.Get("Thread-Id") {
			t.Fatalf("%s upstream Thread-Id = %q, want %q", name, got, headers.Get("Thread-Id"))
		}
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
	manager := NewManager()
	t.Cleanup(manager.Stop)

	const session = "shared-session"
	parentHeaders := http.Header{"Session-Id": []string{session}, "Thread-Id": []string{"parent-thread"}}
	childHeaders := http.Header{"Session-Id": []string{session}, "Thread-Id": []string{"child-thread"}}
	parentLane := proxy.ResolveCodexWebsocketTransportSessionKey(session, parentHeaders)
	childLane := proxy.ResolveCodexWebsocketTransportSessionKey(session, childHeaders)

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
		t.Fatalf("send parent: %v", err)
	}
	select {
	case got := <-frames:
		if got != "parent-thread" {
			t.Fatalf("parent Thread-Id = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("upstream did not receive parent")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	child, childPending, err := manager.AcquireConnection(ctx, account, wsURL, childLane, childHeaders, "")
	if err != nil {
		t.Fatalf("child waited behind parent: %v", err)
	}
	if err := executor.sendRequest(child, []byte(`{"type":"response.create","input":"child"}`), childPending.RequestID); err != nil {
		t.Fatalf("send child: %v", err)
	}
	select {
	case got := <-frames:
		if got != "child-thread" {
			t.Fatalf("child Thread-Id = %q", got)
		}
	case <-time.After(time.Second):
		t.Fatal("upstream did not receive child while parent remained pending")
	}
	child.session.RemovePendingRequest(childPending.RequestID)
	manager.DiscardConnection(child)
	if child == parent || child.PoolKey == parent.PoolKey {
		t.Fatal("parent and child reused the same frozen-handshake connection")
	}
}

func TestSubagentTransportLaneKeepsPreviousResponseConnectionPriority(t *testing.T) {
	manager := NewManager()
	t.Cleanup(manager.Stop)
	manager.probeFunc = func(*WsConnection) bool { return true }

	const session = "shared-session"
	parentHeaders := http.Header{"Session-Id": []string{session}, "Thread-Id": []string{"parent-thread"}}
	parentLane := proxy.ResolveCodexWebsocketTransportSessionKey(session, parentHeaders)
	parent := newBoundTestConn(t, manager, 7, parentLane)
	manager.BindResponseConn("resp_parent", parent, parentLane, 7, "api-key")

	got, pending, slotKey := manager.AcquirePreferredConnection("resp_parent", 7, "api-key")
	if got != parent || pending == nil || slotKey != parentLane {
		t.Fatalf("preferred connection = %p pending=%v lane=%q", got, pending != nil, slotKey)
	}
	defer parent.session.RemovePendingRequest(pending.RequestID)
}

func TestActualWebsocketPoolSessionIDUsesOverflowSlot(t *testing.T) {
	const baseLane = "ws-thread-base"
	overflow := &WsConnection{session: &Session{ID: baseLane + "#ovf-1"}}
	if got := actualWebsocketPoolSessionID(overflow, baseLane); got != baseLane+"#ovf-1" {
		t.Fatalf("actual pool session = %q", got)
	}
	if got := actualWebsocketPoolSessionID(nil, baseLane); got != baseLane {
		t.Fatalf("nil fallback = %q", got)
	}
}
