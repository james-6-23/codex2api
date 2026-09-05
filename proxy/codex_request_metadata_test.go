package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func fingerprintMetadataFixture(test *testing.T, root, thread, parent, fork string, window int) (http.Header, []byte) {
	test.Helper()
	metadata := map[string]any{
		"installation_id": "client-install", "session_id": root, "thread_id": thread,
		"window_id": fmt.Sprintf("%s:%d", thread, window), "window_number": window,
		"context_window_id": "context-" + thread, "turn_id": "turn-" + thread,
		"request_kind": "turn", "thread_source": "user",
	}
	headers := make(http.Header)
	headers.Set("Originator", "codex_cli_rs")
	headers.Set(codexSessionIDHeader, root)
	headers.Set(codexThreadIDHeader, thread)
	headers.Set(codexClientRequestIDHeader, thread)
	headers.Set(codexWindowIDHeader, metadata["window_id"].(string))
	flat := map[string]any{"session_id": root, "thread_id": thread, "x-codex-window-id": metadata["window_id"], "x-client-request-id": thread}
	if parent != "" {
		metadata["parent_thread_id"] = parent
		metadata["subagent_kind"] = "review"
		metadata["thread_source"] = "subagent"
		headers.Set(codexParentThreadIDHeader, parent)
		headers.Set("X-OpenAI-Subagent", "review")
		flat["x-codex-parent-thread-id"] = parent
	}
	if fork != "" {
		metadata["forked_from_thread_id"] = fork
	}
	raw, err := json.Marshal(metadata)
	if err != nil {
		test.Fatal(err)
	}
	headers.Set(codexTurnMetadataHeader, string(raw))
	flat["x-codex-turn-metadata"] = string(raw)
	body, err := json.Marshal(map[string]any{"model": "gpt-6-astra", "client_metadata": flat})
	if err != nil {
		test.Fatal(err)
	}
	return headers, body
}

func TestCodexFingerprintPreservesThreadReferencesAndSnapshot(test *testing.T) {
	for _, mode := range []string{auth.CodexFingerprintModeOff, auth.CodexFingerprintModeDevice, auth.CodexFingerprintModeSession, auth.CodexFingerprintModeFull} {
		test.Run(mode, func(test *testing.T) {
			account := fingerprintAccount(test, mode)
			mappedThreads := make(map[string]string)
			for _, request := range []struct{ root, thread, parent, fork string }{
				{"root", "root", "", ""},
				{"root", "child", "root", ""},
				{"root", "grandchild", "child", ""},
				{"fork", "fork", "", "root"},
			} {
				headers, body := fingerprintMetadataFixture(test, request.root, request.thread, request.parent, request.fork, 2)
				originalHeaders := headers.Clone()
				originalBody := bytes.Clone(body)
				fingerprint := NewCodexFingerprint(account, headers, body)
				outbound := httptest.NewRequest(http.MethodPost, "/responses", nil)
				applyCodexRequestHeaders(outbound, account, "token", "isolated-cache", "key", nil, headers, fingerprint)
				forwarded := fingerprint.ApplyBody(body)
				canonical := gjson.GetBytes(forwarded, "client_metadata.x-codex-turn-metadata").String()
				thread := gjson.Get(canonical, "thread_id").String()
				mappedThreads[request.thread] = thread
				for _, field := range []string{"installation_id", "thread_id", "window_id", "context_window_id", "parent_thread_id", "forked_from_thread_id"} {
					if actual := gjson.Get(outbound.Header.Get(codexTurnMetadataHeader), field).String(); actual != gjson.Get(canonical, field).String() {
						test.Fatalf("%s header/body mismatch: %s", field, canonical)
					}
				}
				if window := outbound.Header.Get(codexWindowIDHeader); window != thread+":2" || window != gjson.GetBytes(forwarded, "client_metadata.x-codex-window-id").String() {
					test.Fatalf("window mismatch: %s, body=%s", window, forwarded)
				}
				if request.parent != "" {
					want := mappedThreads[request.parent]
					if outbound.Header.Get(codexParentThreadIDHeader) != want || gjson.Get(canonical, "parent_thread_id").String() != want || gjson.GetBytes(forwarded, "client_metadata.x-codex-parent-thread-id").String() != want {
						test.Fatalf("parent reference does not match parent's own thread: want=%s body=%s", want, forwarded)
					}
				}
				if request.fork != "" && gjson.Get(canonical, "forked_from_thread_id").String() != mappedThreads[request.fork] {
					test.Fatalf("fork reference does not match source: %s", forwarded)
				}
				if repeated := fingerprint.ApplyBody(forwarded); !bytes.Equal(repeated, forwarded) {
					test.Fatalf("snapshot reapplication changes body: %s -> %s", forwarded, repeated)
				}
				before := outbound.Header.Clone()
				fingerprint.ApplyHeaders(outbound.Header)
				if !reflect.DeepEqual(before, outbound.Header) {
					test.Fatal("snapshot reapplication changes headers")
				}
				if !reflect.DeepEqual(headers, originalHeaders) || !bytes.Equal(body, originalBody) {
					test.Fatal("input mutated")
				}
				if outbound.Header.Get(codexSessionIDHeader) != "isolated-cache" || outbound.Header.Get(codexInstallationIDHeader) != "" {
					test.Fatal("cache isolation changed or installation header invented")
				}
			}
			if mode == auth.CodexFingerprintModeFull && mappedThreads["root"] != mappedThreads["child"] {
				test.Fatal("full mode no longer collapses threads")
			}
			if mode != auth.CodexFingerprintModeFull && mappedThreads["root"] == mappedThreads["child"] {
				test.Fatal("independent threads collapsed")
			}
		})
	}
}

func TestCodexRequestMetadataHeadersReplacesStaleSnapshot(test *testing.T) {
	stale, _ := fingerprintMetadataFixture(test, "old-root", "old-child", "old-parent", "old-fork", 0)
	stale.Set("X-OpenAI-Memgen-Request", "true")
	stale.Set("X-Codex-Turn-State", "old-state")
	_, current := fingerprintMetadataFixture(test, "new-root", "new-thread", "", "", 3)
	resolved := CodexRequestMetadataHeaders(stale, current)
	for _, field := range []string{codexParentThreadIDHeader, "X-OpenAI-Subagent", "X-OpenAI-Memgen-Request", "X-Codex-Turn-State"} {
		if resolved.Get(field) != "" {
			test.Fatalf("stale %s survived: %s", field, resolved.Get(field))
		}
	}
	if resolved.Get(codexThreadIDHeader) != "new-thread" || resolved.Get(codexWindowIDHeader) != "new-thread:3" {
		test.Fatal("current frame identity lost")
	}
	for _, raw := range []string{`{}`, `null`, `""`} {
		cleared := CodexRequestMetadataHeaders(stale, []byte(`{"client_metadata":{"x-codex-turn-metadata":`+raw+`}}`))
		if cleared.Get(codexParentThreadIDHeader) != "" || cleared.Get(codexWindowIDHeader) != "" {
			test.Fatalf("empty metadata inherited old identity: %v", cleared)
		}
	}
}

func TestCodexFingerprintPreservesCurrentCompatibilityFields(test *testing.T) {
	headers, body := fingerprintMetadataFixture(test, "root", "thread", "parent", "fork", 1)
	headers.Set(codexClientRequestIDHeader, "independent-request-id")
	headers.Set("X-Codex-Turn-State", "current-state")
	body, _ = sjson.SetBytes(body, "client_metadata.x-client-request-id", "independent-request-id")
	body, _ = sjson.SetRawBytes(body, "client_metadata.x-codex-turn-metadata", []byte(headers.Get(codexTurnMetadataHeader)))
	account := fingerprintAccount(test, auth.CodexFingerprintModeSession)
	fingerprint := NewCodexFingerprint(account, headers, body)
	forwarded := fingerprint.ApplyBody(body)
	if !gjson.GetBytes(forwarded, "client_metadata.x-codex-turn-metadata").IsObject() {
		test.Fatal("object carrier changed shape")
	}
	if gjson.GetBytes(forwarded, "client_metadata.x-client-request-id").String() != "independent-request-id" {
		test.Fatal("unrelated request id changed")
	}
	if fingerprint.DownstreamHeaders().Get("X-Codex-Turn-State") != "current-state" {
		test.Fatal("current request's turn state discarded")
	}
	if repeated := fingerprint.ApplyBody(forwarded); !bytes.Equal(repeated, forwarded) {
		test.Fatal("object carrier rehashed on repeated snapshot application")
	}
	if parent := gjson.GetBytes(forwarded, "client_metadata.x-codex-turn-metadata.parent_thread_id").String(); parent != gjson.GetBytes(forwarded, "client_metadata.x-codex-parent-thread-id").String() || parent == "parent" {
		test.Fatal("object and flat parent projections disagree")
	}
}

func TestExecuteRequestKeepsFingerprintCarriersAligned(test *testing.T) {
	test.Setenv("CODEX_SESSION_HEADER_MODE", "native")
	test.Setenv("CODEX_SESSION_HEADER_ALIGN_CONVERGED", "false")
	previousResin := GetResinConfig()
	test.Cleanup(func() { SetResinConfig(previousResin) })
	type capture struct {
		headers http.Header
		body    []byte
	}
	received := make(chan capture, 4)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		received <- capture{request.Header.Clone(), readUpstreamRequestBody(request)}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{}`))
	}))
	test.Cleanup(server.Close)
	SetResinConfig(&ResinConfig{BaseURL: server.URL, PlatformName: "metadata-test"})
	for _, mode := range []string{auth.CodexFingerprintModeOff, auth.CodexFingerprintModeDevice, auth.CodexFingerprintModeSession, auth.CodexFingerprintModeFull} {
		test.Run(mode, func(test *testing.T) {
			account := &auth.Account{DBID: 1902, AccessToken: "dummy-token", CodexFingerprintMode: mode}
			headers, body := fingerprintMetadataFixture(test, "root", "child", "parent", "fork", 4)
			expected := NewCodexFingerprint(account, headers, body).ApplyBody(body)
			response, err := ExecuteRequest(context.Background(), account, body, "isolated-cache", "", "key", nil, headers, false)
			if err != nil {
				test.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
			sent := <-received
			canonical := gjson.GetBytes(sent.body, "client_metadata.x-codex-turn-metadata").String()
			if canonical != gjson.GetBytes(expected, "client_metadata.x-codex-turn-metadata").String() || canonical != sent.headers.Get(codexTurnMetadataHeader) {
				test.Fatalf("HTTP snapshot diverged: %s", sent.body)
			}
			if window := sent.headers.Get(codexWindowIDHeader); window == "" || window != gjson.Get(canonical, "window_id").String() {
				test.Fatal("HTTP window header missing or mismatched")
			}
			if sent.headers.Get(codexSessionIDHeader) != "isolated-cache" {
				test.Fatal("cache isolation changed")
			}
		})
	}
}
