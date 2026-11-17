package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

func officialFieldsFixture(kind string) []byte {
	metadata := `{"installation_id":"client-installation","session_id":"client-thread","thread_id":"client-thread","request_kind":"` + kind + `"}`
	body, _ := json.Marshal(map[string]any{
		"model": "gpt-6-luna", "store": false,
		"client_metadata": map[string]string{"x-codex-installation-id": "client-installation", "x-codex-turn-metadata": metadata},
		"input": []any{map[string]any{"type": "message", "id": "msg_original", "role": "user",
			"content": []any{map[string]any{"type": "input_text", "text": "Reply OK"}},
			"internal_chat_message_metadata_passthrough": map[string]any{"content_item_kinds": []string{"user.text"}}}},
	})
	return body
}

func TestOfficialFieldsPreparationScopeAndRollback(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, official := range []bool{false, true} {
			for _, ws := range []bool{false, true} {
				t.Run(fmt.Sprintf("enabled=%v/official=%v/ws=%v", enabled, official, ws), func(t *testing.T) {
					t.Setenv("CODEX_PRESERVE_OFFICIAL_REQUEST_FIELDS", fmt.Sprint(enabled))
					body := officialFieldsFixture("turn")
					if !official {
						var decoded map[string]any
						json.Unmarshal(body, &decoded)
						delete(decoded, "client_metadata")
						body, _ = json.Marshal(decoded)
					}
					var prepared []byte
					if ws {
						prepared, _ = PrepareResponsesWebSocketBody(body)
					} else {
						prepared, _ = PrepareResponsesBody(body)
					}
					wantPreserved := enabled && official
					for _, path := range []string{"input.0.id", "input.0.internal_chat_message_metadata_passthrough"} {
						if gjson.GetBytes(prepared, path).Exists() != wantPreserved {
							t.Fatalf("%s preserved incorrectly: %s", path, prepared)
						}
					}
					if PreserveCodexOfficialRequestFields(prepared) != wantPreserved {
						t.Fatal("preparation and output-boundary scope disagree")
					}
					if wantPreserved && gjson.GetBytes(prepared, "instructions").Exists() {
						t.Fatal("official preparation invented instructions")
					}
				})
			}
		}
	}
}

func TestOfficialFieldsFinalHTTPRetainsIdentityIsolation(t *testing.T) {
	t.Setenv("CODEX_PRESERVE_OFFICIAL_REQUEST_FIELDS", "true")
	t.Setenv("CODEX_ACCOUNT_INSTALLATION_IDS", officialWirePins)
	t.Setenv("CODEX_DISABLE_GENERATED_ATTESTATION", "true")
	runtime := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(runtime) })
	next := runtime
	next.CodexForceWebsocket = false
	next.CodexRequestCompression = true
	ApplyRuntimeSettings(next)
	for _, kind := range []string{"turn", "compaction"} {
		for _, id := range []int64{1, 3} {
			t.Run(fmt.Sprintf("%s/account=%d", kind, id), func(t *testing.T) {
				body, _ := PrepareResponsesBody(officialFieldsFixture(kind))
				var captured []byte
				var encoding string
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					encoding = r.Header.Get("Content-Encoding")
					captured = readUpstreamRequestBody(r)
					fmt.Fprint(w, `{}`)
				}))
				defer server.Close()
				previous := resinCfg.Load()
				t.Cleanup(func() { resinCfg.Store(previous) })
				SetResinConfig(&ResinConfig{BaseURL: server.URL, PlatformName: "test"})
				clientPool.Delete(fmt.Sprintf("resin|%d", id))
				account := &auth.Account{DBID: id, AccessToken: "dummy-token", CodexFingerprintMode: auth.CodexFingerprintModeSingleMachineMultiWindow}
				response, err := ExecuteRequest(context.Background(), account, body, "isolated-session", "", "dummy-key", nil, http.Header{}, false)
				if err != nil {
					t.Fatal(err)
				}
				response.Body.Close()
				if encoding != "zstd" || gjson.GetBytes(captured, "instructions").Exists() {
					t.Fatal("HTTP compression or absent instructions changed")
				}
				if gjson.GetBytes(captured, "input.0.id").String() != "msg_original" || !gjson.GetBytes(captured, "input.0.internal_chat_message_metadata_passthrough").Exists() {
					t.Fatal("official input fields were lost at final HTTP boundary")
				}
				if gjson.GetBytes(captured, "client_metadata.x-codex-installation-id").String() != configuredCodexInstallationID(account) {
					t.Fatal("preserving fields bypassed account identity isolation")
				}
			})
		}
	}
}

func TestOfficialFieldsRejectsIncompleteMetadataShape(t *testing.T) {
	t.Setenv("CODEX_PRESERVE_OFFICIAL_REQUEST_FIELDS", "true")
	for _, body := range []string{
		`{"client_metadata":{"x-codex-installation-id":"client"}}`,
		`{"client_metadata":{"x-codex-installation-id":"client","x-codex-turn-metadata":"not-json"}}`,
		`{"client_metadata":{"x-codex-installation-id":"client","x-codex-turn-metadata":"{\"installation_id\":\"other\",\"session_id\":\"s\",\"thread_id\":\"s\",\"request_kind\":\"turn\"}"}}`,
	} {
		if PreserveCodexOfficialRequestFields([]byte(body)) {
			t.Fatal("incomplete metadata enabled official field preservation")
		}
	}
}
