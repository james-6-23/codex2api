package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestSingleMachineFingerprintTopologyAndCarriers(t *testing.T) {
	t.Setenv("CODEX_SESSION_HEADER_MODE", "native")
	t.Setenv("CODEX_SESSION_HEADER_ALIGN_CONVERGED", "false")
	a := fingerprintAccount(t, auth.CodexFingerprintModeSingleMachineMultiWindow)
	parent := codexClientHeaders(`{"session_id":"root","thread_id":"root"}`, "root")
	parentIDs := resolveCodexFingerprintIDs(a, parent)
	raw := `{"installation_id":"device","session_id":"root","thread_id":"child","parent_thread_id":"root","forked_from_thread_id":"root","turn_id":"unchanged-turn","turn_started_at_unix_ms":123,"subagent_kind":"worker"}`
	h := codexClientHeaders(raw, "root")
	h.Set("Thread-Id", "child")
	h.Set(codexParentThreadIDHeader, "root")
	h.Set(codexClientRequestIDHeader, "child")
	body := []byte(`{"prompt_cache_key":"tenant-cache","previous_response_id":"resp-private","client_metadata":{"installation_id":"device","session_id":"root","thread_id":"child","parent_thread_id":"root","forked_from_thread_id":"root","x-codex-window-id":"child:0"}}`)
	body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata", raw)
	rewritten := ApplyCodexFingerprintToBody(body, a, h)
	req, _ := http.NewRequest(http.MethodPost, "https://example.test/responses", nil)
	applyCodexRequestHeaders(req, a, "token", "tenant-cache", "key", nil, h)
	m := gjson.Parse(req.Header.Get(codexTurnMetadataHeader))
	bm := gjson.GetBytes(rewritten, "client_metadata")
	ids := resolveCodexFingerprintIDs(a, h)
	if ids.threadID == parentIDs.threadID || ids.sessionID != parentIDs.sessionID {
		t.Fatal("child topology lost")
	}
	for _, key := range []string{"session_id", "thread_id", "parent_thread_id", "installation_id", "forked_from_thread_id"} {
		if m.Get(key).String() != bm.Get(key).String() {
			t.Fatalf("header/body %s mismatch", key)
		}
	}
	if m.Get("parent_thread_id").String() != parentIDs.threadID || req.Header.Get(codexParentThreadIDHeader) != parentIDs.threadID {
		t.Fatal("parent reference mismatch")
	}
	if req.Header.Get("Session-Id") != ids.sessionID || req.Header.Get("Thread-Id") != ids.threadID || req.Header.Get(codexClientRequestIDHeader) != ids.threadID {
		t.Fatal("identity headers differ", req.Header)
	}
	if m.Get("turn_id").String() != "unchanged-turn" || m.Get("turn_started_at_unix_ms").Int() != 123 || m.Get("subagent_kind").String() != "worker" {
		t.Fatal("turn/real subagent metadata changed")
	}
	if gjson.GetBytes(rewritten, "prompt_cache_key").String() != "tenant-cache" || gjson.GetBytes(rewritten, "previous_response_id").String() != "resp-private" {
		t.Fatal("cache/continuation changed")
	}
	if embedded := bm.Get("x-codex-turn-metadata").String(); embedded != m.Raw {
		t.Fatalf("embedded/header mismatch %s %s", embedded, m.Raw)
	}
	// Concurrent unrelated windows stay independent and stable, not a fixed-size pool.
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			h := codexClientHeaders("", fmt.Sprint(i))
			a1 := resolveCodexFingerprintIDs(a, h)
			a2 := resolveCodexFingerprintIDs(a, h)
			if !reflect.DeepEqual(a1, a2) || a1.threadID == ids.threadID {
				t.Errorf("unstable window %d", i)
			}
		}(i)
	}
	wg.Wait()
}

func TestSingleMachineFingerprintMissingIdentityAndBodyFallback(t *testing.T) {
	a := fingerprintAccount(t, auth.CodexFingerprintModeSingleMachineMultiWindow)
	if ids := resolveCodexFingerprintIDs(a, nil); ids != nil {
		t.Fatal("missing identity collapsed")
	}
	h := http.Header{"User-Agent": []string{"plain-sdk"}}
	body := []byte(`{"prompt_cache_key":"not-a-session","client_metadata":{"session_id":"body-session","thread_id":"body-thread"}}`)
	prepared := PrepareCodexFingerprintHeaders(a, h, body)
	if h.Get("Session-Id") != "" || prepared.Get("Session-Id") != "body-session" {
		t.Fatal("mutated caller or lost body identity")
	}
	ids := resolveCodexFingerprintIDs(a, prepared)
	if ids == nil || ids.threadID != singleMachineIdentity(a.ID(), "body-thread") {
		t.Fatal("wrong identity")
	}
	for _, raw := range []string{`{}`, `{"client_metadata":null}`, `{"client_metadata":{"session_id":42}}`, `{"prompt_cache_key":"not-a-session"}`} {
		if got := resolveCodexFingerprintIDs(a, PrepareCodexFingerprintHeaders(a, nil, []byte(raw))); got != nil {
			t.Fatalf("guessed identity from %s", raw)
		}
	}
	if got := ApplyCodexFingerprintToBody([]byte(`{"input":"hello"}`), a, prepared); string(got) != `{"input":"hello"}` {
		t.Fatal("invented metadata")
	}
	for _, mode := range []string{auth.CodexFingerprintModeSingleMachineMultiWindow} {
		for _, provider := range []string{auth.UpstreamOpenAIResponses, auth.UpstreamGrok} {
			relay := &auth.Account{DBID: 1, UpstreamType: provider, BaseURL: "https://relay.example.test", APIKey: "relay-key", CodexFingerprintMode: mode}
			if resolveCodexFingerprintIDs(relay, prepared) != nil {
				t.Fatalf("relay %s rewritten", provider)
			}
		}
	}
}

func TestSingleMachineFingerprintPoolIdentityIsolation(t *testing.T) {
	a := fingerprintAccount(t, auth.CodexFingerprintModeSingleMachineMultiWindow)
	h1 := codexClientHeaders("", "session-a")
	h2 := codexClientHeaders("", "session-b")
	k1 := ScopeCodexFingerprintTransportKey("api-key-pool", a, h1)
	if k1 == ScopeCodexFingerprintTransportKey("api-key-pool", a, h2) {
		t.Fatal("different windows share frozen handshake")
	}
	if k1 != ScopeCodexFingerprintTransportKey("api-key-pool", a, h1) {
		t.Fatal("pool identity unstable")
	}
	if k1 == ScopeCodexFingerprintTransportKey("another-api-key", a, h1) {
		t.Fatal("key partition lost")
	}
	h1.Set("Thread-Id", "child")
	if k1 == ScopeCodexFingerprintTransportKey("api-key-pool", a, h1) {
		t.Fatal("child and parent share handshake")
	}
	a.CodexFingerprintMode = auth.CodexFingerprintModeDevice
	if ScopeCodexFingerprintTransportKey("legacy-pool", a, h1) != "legacy-pool" {
		t.Fatal("device-only pool changed")
	}
}

func TestSingleMachineFingerprintHTTPAndCompactWire(t *testing.T) {
	t.Setenv("CODEX_SESSION_HEADER_MODE", "native")
	for _, compact := range []bool{false, true} {
		t.Run(fmt.Sprintf("compact=%v", compact), func(t *testing.T) {
			a := fingerprintAccount(t, auth.CodexFingerprintModeSingleMachineMultiWindow)
			a.AccessToken = "test-token"
			// Body-only identity: real executor must share original IDs with headers.
			body := []byte(`{"model":"gpt-5.6-codex","input":[],"prompt_cache_key":"private-cache","client_metadata":{"installation_id":"device","session_id":"root","thread_id":"child","parent_thread_id":"root"}}`)
			h := http.Header{}
			expected := resolveCodexFingerprintIDs(a, PrepareCodexFingerprintHeaders(a, h, body))
			var gotBody []byte
			var gotHeaders http.Header
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotBody = readUpstreamRequestBody(r)
				gotHeaders = r.Header.Clone()
				w.Header().Set("Content-Type", "application/json")
				fmt.Fprint(w, `{}`)
			}))
			defer server.Close()
			previous := resinCfg.Load()
			t.Cleanup(func() { resinCfg.Store(previous) })
			SetResinConfig(&ResinConfig{BaseURL: server.URL, PlatformName: "test"})
			clientPool.Delete(fmt.Sprintf("resin|%d", a.ID()))
			var resp *http.Response
			var err error
			if compact {
				resp, err = ExecuteCompactRequest(context.Background(), a, body, "private-cache", "", "test-key", nil, h)
			} else {
				resp, err = ExecuteRequest(context.Background(), a, body, "private-cache", "", "test-key", nil, h, false)
			}
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if gotHeaders.Get("Session-Id") != expected.sessionID || gotHeaders.Get("Thread-Id") != expected.threadID {
				t.Fatalf("wire headers differ: %v", gotHeaders)
			}
			if gjson.GetBytes(gotBody, "client_metadata.thread_id").String() != expected.threadID {
				t.Fatalf("wire body identity differs: %s", gotBody)
			}
			if gjson.GetBytes(gotBody, "prompt_cache_key").String() != "private-cache" {
				t.Fatalf("wire cache key differs: %s", gotBody)
			}
			if len(h) != 0 {
				t.Fatal("caller headers mutated")
			}
		})
	}
}
