package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/internal/timezone"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func environmentTestBody(text string) []byte {
	body, _ := json.Marshal(map[string]any{
		"model": "gpt-6-astra", "stream": true,
		"input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}}}},
	})
	return body
}

func environmentTestText(date, zone string) string {
	return "<environment_context>\n  <cwd>F:\\codex</cwd>\n  <shell>powershell</shell>\n  <current_date>" + date + "</current_date>\n  <timezone>" + zone + "</timezone>\n  <filesystem><root>C:\\Users\\fixture</root></filesystem>\n</environment_context>"
}

func TestCodexEnvironmentRewriteBoundaries(test *testing.T) {
	location, _ := timezone.Load("America/Los_Angeles")
	for _, sample := range []struct{ instant, source, target string }{
		{"2026-09-06T00:30:00Z", "2026-09-06", "2026-09-05"},
		{"2026-09-06T16:30:00Z", "2026-09-07", "2026-09-06"},
		{"2026-01-06T07:30:00Z", "2026-01-06", "2026-01-05"},
		{"2026-07-06T07:30:00Z", "2026-07-06", "2026-07-06"},
	} {
		reference, _ := time.Parse(time.RFC3339, sample.instant)
		original := environmentTestText(sample.source, "Asia/Shanghai")
		body := environmentTestBody(original)
		updated := rewriteCodexEnvironment(body, location, reference)
		expected := environmentTestText(sample.target, location.String())
		if text := gjson.GetBytes(updated, "input.0.content.0.text").String(); text != expected {
			test.Fatalf("%s: %s", sample.instant, text)
		}
		if !bytes.Equal(updated, rewriteCodexEnvironment(updated, location, reference)) || gjson.GetBytes(body, "input.0.content.0.text").String() != original {
			test.Fatal("rewrite was not idempotent or mutated input")
		}
	}
	reference := time.Date(2026, 9, 6, 0, 30, 0, 0, time.UTC)
	base := environmentTestBody(environmentTestText("2026-09-06", "Asia/Shanghai"))
	for _, sample := range []struct {
		path  string
		value any
	}{
		{"input.0.role", "assistant"},
		{"input.0.role", "tool"},
		{"input.0.type", "function_call_output"},
		{"input.0.content.0.text", "Example:\n" + environmentTestText("2026-09-06", "Asia/Shanghai")},
		{"input.0.content.0.text", environmentTestText("2026-09-01", "Asia/Shanghai")},
		{"input.0.content.0.text", environmentTestText("2026-09-06", "Local")},
		{"input.0.content.0.text", "<environment_context><timezone>Asia/Shanghai</timezone><timezone>UTC</timezone></environment_context>"},
		{"input.0.content.0.text", "<environment_context><filesystem><timezone>Asia/Shanghai</timezone></filesystem></environment_context>"},
		{"input.0.content.0.text", "<environment_context><timezone>Asia/Shanghai</timezone><broken></environment_context>"},
		{"input", []any{map[string]any{"type": "compaction", "encrypted_content": "unchanged"}}},
	} {
		body, _ := sjson.SetBytes(base, sample.path, sample.value)
		if !bytes.Equal(body, rewriteCodexEnvironment(body, location, reference)) {
			test.Fatalf("unrelated/history/invalid text was rewritten: %+v", sample)
		}
	}
	withHistory, _ := sjson.SetRawBytes(base, "input.1", []byte(gjson.GetBytes(base, "input.0").Raw))
	updated := rewriteCodexEnvironment(withHistory, location, reference)
	if gjson.GetBytes(updated, "input.0").Raw != gjson.GetBytes(base, "input.0").Raw || !strings.Contains(gjson.GetBytes(updated, "input.1.content.0.text").String(), location.String()) {
		test.Fatal("rewrite did not restrict itself to the latest environment block")
	}
}

func TestCodexEnvironmentSnapshotAndFrameReference(test *testing.T) {
	previousResin := GetResinConfig()
	SetResinConfig(nil)
	test.Cleanup(func() { SetResinConfig(previousResin) })
	reference := time.Date(2026, 9, 6, 0, 30, 0, 0, time.UTC)
	body := environmentTestBody(environmentTestText("2026-09-06", "Asia/Shanghai"))
	zone := "America/Los_Angeles"
	lookups := 0
	ctx := WithCodexEnvironment(context.Background(), func(string) *time.Location {
		lookups++
		location, _ := timezone.Load(zone)
		return location
	}, reference)
	first := ApplyCodexEnvironment(ctx, body, "proxy-a")
	zone = "Asia/Tokyo"
	if !bytes.Equal(first, ApplyCodexEnvironment(ctx, body, "proxy-a")) || lookups != 1 {
		test.Fatal("retry recomputed timezone")
	}
	if bytes.Equal(first, ApplyCodexEnvironment(ctx, body, "proxy-b")) {
		test.Fatal("different proxy reused the earlier timezone")
	}
	if !bytes.Equal(body, ApplyCodexEnvironment(ctx, body, "")) {
		test.Fatal("direct egress was rewritten")
	}
	headers := make(http.Header)
	headers.Set(codexTurnMetadataHeader, `{"turn_started_at_unix_ms":1}`)
	body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata", fmt.Sprintf(`{"turn_started_at_unix_ms":%d}`, reference.UnixMilli()))
	if actual := codexEnvironmentReference(CodexRequestMetadataHeaders(headers, body), reference.Add(time.Hour)); !actual.Equal(reference) {
		test.Fatal("frame timestamp did not override stale handshake timestamp")
	}
	SetResinConfig(&ResinConfig{BaseURL: "http://resin.invalid", PlatformName: "test"})
	if !bytes.Equal(body, ApplyCodexEnvironment(ctx, body, "proxy-a")) {
		test.Fatal("unknown Resin egress was inferred from an unused proxy")
	}
}

type environmentTestTransport struct{ received *[]byte }

func (transport environmentTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	*transport.received = readUpstreamRequestBody(request)
	return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`)), Request: request}, nil
}

func TestCodexEnvironmentHTTPAndCompactForwarding(test *testing.T) {
	previousResin := GetResinConfig()
	SetResinConfig(nil)
	test.Cleanup(func() { SetResinConfig(previousResin) })
	reference := time.Date(2026, 9, 6, 0, 30, 0, 0, time.UTC)
	location, _ := timezone.Load("America/Los_Angeles")
	for _, mode := range []string{auth.CodexFingerprintModeOff, auth.CodexFingerprintModeDevice, auth.CodexFingerprintModeSession, auth.CodexFingerprintModeFull} {
		account := &auth.Account{DBID: 71092, AccessToken: "dummy", ProxyURL: "http://unused.invalid", CodexFingerprintMode: mode}
		proxyURL := "http://selected.invalid"
		poolKey := clientPoolKey(account, proxyURL, codexTransportModeFromEnv())
		var received []byte
		entry := &poolEntry{client: &http.Client{Transport: environmentTestTransport{&received}}}
		entry.touch()
		clientPool.Store(poolKey, entry)
		test.Cleanup(func() { clientPool.Delete(poolKey) })
		ctx := WithCodexEnvironment(context.Background(), func(selected string) *time.Location {
			if selected != proxyURL {
				test.Fatalf("timezone selected for wrong egress: %s", selected)
			}
			return location
		}, reference)
		for _, compact := range []bool{false, true} {
			body := environmentTestBody(environmentTestText("2026-09-06", "Asia/Shanghai"))
			var response *http.Response
			var err error
			if compact {
				response, err = ExecuteCompactRequest(ctx, account, body, "session", proxyURL, "key", nil, http.Header{})
			} else {
				response, err = ExecuteRequest(ctx, account, body, "session", proxyURL, "key", nil, http.Header{}, false)
			}
			if err != nil {
				test.Fatal(err)
			}
			response.Body.Close()
			if actual := gjson.GetBytes(received, "input.0.content.0.text").String(); actual != environmentTestText("2026-09-05", location.String()) {
				test.Fatalf("mode=%s compact=%t body=%s", mode, compact, received)
			}
		}
	}
}
