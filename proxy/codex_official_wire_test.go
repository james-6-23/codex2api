package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

const officialWirePins = `{"1":"11111111-1111-4111-8111-111111111111","2":"11111111-1111-4111-8111-111111111111","3":"22222222-2222-4222-8222-222222222222"}`

// Capture actual HTTP and compact requests after all header/body stages.
// Fixtures follow openai/codex a956835d: responses_metadata.rs:325-404.
func TestOfficialWireHTTPAndCompact(t *testing.T) {
	t.Setenv("CODEX_ACCOUNT_INSTALLATION_IDS", officialWirePins)
	t.Setenv("CODEX_DISABLE_GENERATED_ATTESTATION", "true")
	t.Setenv("CODEX_SESSION_HEADER_MODE", "native")
	for _, compact := range []bool{false, true} {
		for _, id := range []int64{1, 2, 3} {
			for _, caller := range []string{"client-one", "client-two"} {
				t.Run(fmt.Sprintf("compact=%v/account=%d/%s", compact, id, caller), func(t *testing.T) {
					a := &auth.Account{DBID: id, AccessToken: "mock-token", CodexFingerprintMode: auth.CodexFingerprintModeSingleMachineMultiWindow,
						CustomHeaders: map[string]string{"User-Agent": "codex-tui/0.160.0 (Windows 10.0.19045; x86_64) unknown (codex-tui; 0.160.0)", "X-Codex-Installation-Id": "stale-header-pin"}}
					embedded := `{"installation_id":"` + caller + `","session_id":"thread-one","thread_id":"thread-one","window_id":"thread-one:0","request_kind":"responses","tool_namespaces_info":{"tools":[]}}`
					metadata := map[string]string{"x-codex-installation-id": caller, "session_id": "thread-one", "thread_id": "thread-one", "x-codex-window-id": "thread-one:0", "x-codex-turn-metadata": embedded}
					body, _ := json.Marshal(map[string]any{"model": "gpt-6-luna", "input": []any{}, "client_metadata": metadata})
					var gotBody []byte
					var gotHeaders http.Header
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						gotBody, gotHeaders = readUpstreamRequestBody(r), r.Header.Clone()
						w.Header().Set("Content-Type", "application/json")
						fmt.Fprint(w, `{}`)
					}))
					defer server.Close()
					previous := resinCfg.Load()
					t.Cleanup(func() { resinCfg.Store(previous) })
					SetResinConfig(&ResinConfig{BaseURL: server.URL, PlatformName: "test"})
					clientPool.Delete(fmt.Sprintf("resin|%d", id))
					var resp *http.Response
					var err error
					if compact {
						resp, err = ExecuteCompactRequest(context.Background(), a, body, "cache", "", "mock-key", nil, http.Header{})
					} else {
						resp, err = ExecuteRequest(context.Background(), a, body, "cache", "", "mock-key", nil, http.Header{}, false)
					}
					if err != nil {
						t.Fatal(err)
					}
					resp.Body.Close()
					if gotHeaders.Get("X-Codex-Installation-Id") != "" || gotHeaders.Get("X-Oai-Attestation") != "" {
						t.Fatal("unexpected synthetic identity header")
					}
					if got := gjson.GetBytes(gotBody, "client_metadata.x-codex-installation-id").String(); got != configuredCodexInstallationID(a) {
						t.Fatalf("installation ID: %s", got)
					}
					if got := gjson.Get(gotHeaders.Get(codexTurnMetadataHeader), "installation_id").String(); got != configuredCodexInstallationID(a) {
						t.Fatal("header projection identity differs")
					}
					if gjson.Get(gotHeaders.Get(codexTurnMetadataHeader), "tool_namespaces_info").Exists() {
						t.Fatal("unbounded inventory leaked into header")
					}
					if !gjson.Get(gjson.GetBytes(gotBody, "client_metadata.x-codex-turn-metadata").String(), "tool_namespaces_info").Exists() {
						t.Fatal("body inventory lost")
					}
					if gotHeaders.Get("Thread-Id") != gjson.GetBytes(gotBody, "client_metadata.thread_id").String() || gotHeaders.Get("X-Client-Request-Id") != gotHeaders.Get("Thread-Id") {
						t.Fatal("thread identities differ")
					}
					actualKeys := map[string]bool{}
					for key := range gjson.GetBytes(gotBody, "client_metadata").Map() {
						actualKeys[key] = true
					}
					expectedKeys := map[string]bool{}
					for key := range metadata {
						expectedKeys[key] = true
					}
					if !reflect.DeepEqual(actualKeys, expectedKeys) {
						t.Fatal("metadata key set changed")
					}
				})
			}
		}
	}
}

func TestOfficialWireInstallationWithoutConversation(t *testing.T) {
	t.Setenv("CODEX_ACCOUNT_INSTALLATION_IDS", officialWirePins)
	a := &auth.Account{DBID: 1, CodexFingerprintMode: auth.CodexFingerprintModeSingleMachineMultiWindow}
	body := ApplyCodexFingerprintToBody([]byte(`{"client_metadata":{"x-codex-installation-id":"caller"}}`), a, nil)
	if gjson.GetBytes(body, "client_metadata.x-codex-installation-id").String() != configuredCodexInstallationID(a) || len(gjson.GetBytes(body, "client_metadata").Map()) != 1 {
		t.Fatal("installation pin lost or fields added")
	}
	if got := string(ApplyCodexFingerprintToBody([]byte(`{"input":[]}`), a, nil)); got != `{"input":[]}` {
		t.Fatal("metadata invented")
	}
}

func TestOfficialWireRejectsInvalidPins(t *testing.T) {
	for _, raw := range []string{`oops`, `{"1":"not-a-uuid"}`, `{"-1":"11111111-1111-4111-8111-111111111111"}`} {
		t.Setenv("CODEX_ACCOUNT_INSTALLATION_IDS", raw)
		if ValidateCodexInstallationPins() == nil {
			t.Fatal("invalid identity config accepted")
		}
	}
}

func TestOfficialWireUnconfiguredThreadOnlyIdentity(t *testing.T) {
	t.Setenv("CODEX_ACCOUNT_INSTALLATION_IDS", "")
	account := &auth.Account{DBID: 1, CodexFingerprintMode: auth.CodexFingerprintModeSingleMachineMultiWindow}
	headers := http.Header{"Thread-Id": []string{"client-thread"}}
	ids := resolveCodexFingerprintIDs(account, headers)
	if ids == nil || ids.sessionID == "" || ids.sessionID != ids.threadID {
		t.Fatal("thread-only requests lost the existing session fallback without a pin")
	}
	if ids := resolveCodexFingerprintIDs(account, nil); ids != nil {
		t.Fatal("unconfigured anonymous requests acquired a converged identity")
	}
}
