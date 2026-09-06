package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func schemaPolicyTestRequest(model string, lite, legacyFormat bool) []byte {
	schema := map[string]any{"type": "object", "properties": map[string]any{"title": map[string]any{"type": "string", "minLength": 1, "maxLength": 36, "pattern": "^[a-z]+$"}}, "$defs": map[string]any{"nested": map[string]any{"type": "string", "minLength": 2, "maxLength": 20}}}
	format := map[string]any{"type": "json_schema", "name": "length_test", "strict": true, "schema": schema}
	body := map[string]any{"model": model, "input": "Return a title.", "tools": []any{map[string]any{"type": "function", "name": "read", "parameters": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string", "minLength": 1}}}}}}
	if lite {
		body["client_metadata"] = map[string]any{"ws_request_header_x_openai_internal_codex_responses_lite": "true"}
	}
	if legacyFormat {
		body["response_format"] = map[string]any{"type": "json_schema", "json_schema": format}
	} else {
		body["text"] = map[string]any{"format": format}
	}
	raw, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return raw
}

func TestResponsesStructuredSchemaPolicy(t *testing.T) {
	for _, tc := range []struct {
		name, model                 string
		lite, websocket, wantLength bool
	}{
		{"astra_lite_ws", "gpt-6-astra", true, true, true},
		{"luna_lite_ws", "gpt-5.6-luna", true, true, true},
		{"unknown_model", "gpt-6-future", true, true, false},
		{"old_model", "gpt-5.4", true, true, false},
		{"without_lite", "gpt-6-astra", false, true, false},
		{"http", "gpt-6-astra", true, false, false},
	} {
		for _, legacy := range []bool{false, true} {
			t.Run(tc.name+map[bool]string{true: "/response_format", false: "/text_format"}[legacy], func(t *testing.T) {
				raw := schemaPolicyTestRequest(tc.model, tc.lite, legacy)
				var got []byte
				if tc.websocket {
					got, _ = PrepareResponsesWebSocketBody(raw)
				} else {
					got, _ = PrepareResponsesBody(raw)
				}
				got = normalizeCodexStructuredOutputForTransport(got, tc.websocket, tc.lite)
				for _, path := range []string{"text.format.schema.properties.title.minLength", "text.format.schema.properties.title.maxLength", "text.format.schema.$defs.nested.minLength", "text.format.schema.$defs.nested.maxLength"} {
					if gjson.GetBytes(got, path).Exists() != tc.wantLength {
						t.Fatalf("%s retained=%t want=%t", path, gjson.GetBytes(got, path).Exists(), tc.wantLength)
					}
				}
				if gjson.GetBytes(got, "text.format.schema.properties.title.pattern").Exists() {
					t.Fatal("an unverified structured constraint was preserved")
				}
				if gjson.GetBytes(got, "tools.0.parameters.properties.path.minLength").Exists() {
					t.Fatal("structured-output policy leaked into tool schemas")
				}
				if !gjson.GetBytes(got, "text.format.schema.required").IsArray() || gjson.GetBytes(got, "text.format.schema.additionalProperties").Type != gjson.False {
					t.Fatal("structural schema repair regressed")
				}
			})
		}
	}
}

func TestResponsesSchemaPolicyUsesFinalModelAndTransport(t *testing.T) {
	const injectLite = `{"override":[{"models":["gpt-6-astra","gpt-5.6-luna"],"params":{"client_metadata.ws_request_header_x_openai_internal_codex_responses_lite":"true"}}]}`
	const removeLite = `{"override":[{"models":["gpt-6-astra"],"params":{"client_metadata.ws_request_header_x_openai_internal_codex_responses_lite":"false"}}]}`
	for _, tc := range []struct {
		name, model, rules           string
		clientLite, headerLite       bool
		websocket, wantLite          bool
		wantLength, legacyFormat     bool
		finalModel                   string
		missingExecutor, rejectsLite bool
	}{
		{name: "supported_ws", clientLite: true, websocket: true, wantLite: true, wantLength: true},
		{name: "injected_lite_astra", rules: injectLite, websocket: true, wantLite: true, wantLength: true},
		{name: "injected_lite_luna_legacy", model: "gpt-5.6-luna", rules: injectLite, websocket: true, wantLite: true, wantLength: true, legacyFormat: true},
		{name: "without_lite", websocket: true},
		{name: "header_only", headerLite: true, websocket: true, wantLite: true, wantLength: true},
		{name: "http_fallback", clientLite: true, wantLite: true},
		{name: "injected_lite_http_fallback", rules: injectLite, wantLite: true},
		{name: "missing_ws_executor", rules: injectLite, websocket: true, wantLite: true, missingExecutor: true},
		{name: "rewritten_model", clientLite: true, websocket: true, finalModel: "gpt-5.4"},
		{name: "unknown_model", model: "gpt-6-future", clientLite: true, websocket: true, wantLite: true},
		{name: "removed_lite", clientLite: true, rules: removeLite, websocket: true},
		{name: "account_rejects_lite", rules: injectLite, websocket: true, rejectsLite: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rules := tc.rules
			if rules == "" {
				rules = `{}`
			}
			withPayloadRules(t, rules)
			var captured []byte
			var capturedHeaders http.Header
			stubWebsocketExecute(t, &captured, &capturedHeaders)
			if tc.missingExecutor {
				WebsocketExecuteFunc = nil
			}
			type httpRequest struct {
				body    []byte
				headers http.Header
			}
			httpRequests := make(chan httpRequest, 1)
			previousResin := resinCfg.Load()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				httpRequests <- httpRequest{body: readUpstreamRequestBody(r), headers: r.Header.Clone()}
				w.Header().Set("Content-Type", "application/json")
				w.Write([]byte(`{"id":"resp_test"}`))
			}))
			t.Cleanup(server.Close)
			SetResinConfig(&ResinConfig{BaseURL: server.URL, PlatformName: "test"})
			t.Cleanup(func() { resinCfg.Store(previousResin) })
			account := &auth.Account{DBID: 8191, AccessToken: "test-token", AccountID: "test-account"}
			model := tc.model
			if model == "" {
				model = "gpt-6-astra"
			}
			if tc.rejectsLite {
				account.ApplyModelCapabilities(database.ModelCapabilitySnapshot{
					AccountID: account.ID(),
					Models: map[string]map[string]json.RawMessage{
						model: {"use_responses_lite": json.RawMessage(`false`)},
					},
				})
			}
			headers := make(http.Header)
			if tc.headerLite {
				headers.Set(codexResponsesLiteHeader, "true")
			}
			// Match native WS ingress: preparation runs before account selection
			// and ExecuteRequest applies payload rules and transport gates.
			body, _ := PrepareResponsesWebSocketBody(schemaPolicyTestRequest(model, tc.clientLite, tc.legacyFormat))
			if tc.finalModel != "" {
				var err error
				body, err = sjson.SetBytes(body, "model", tc.finalModel)
				if err != nil {
					t.Fatal(err)
				}
			}
			resp, err := ExecuteRequest(context.Background(), account, body, "schema-test", "", "test-key", nil, headers, tc.websocket)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if !tc.websocket || tc.missingExecutor {
				request := <-httpRequests
				captured, capturedHeaders = request.body, request.headers
			}
			if lite := codexResponsesLiteRequested(captured, capturedHeaders); lite != tc.wantLite {
				t.Fatalf("outbound Lite=%t want=%t", lite, tc.wantLite)
			}
			for path, value := range map[string]int64{
				"text.format.schema.properties.title.minLength": 1,
				"text.format.schema.properties.title.maxLength": 36,
				"text.format.schema.$defs.nested.minLength":     2,
				"text.format.schema.$defs.nested.maxLength":     20,
			} {
				got := gjson.GetBytes(captured, path)
				if got.Exists() != tc.wantLength || (tc.wantLength && got.Int() != value) {
					t.Fatalf("outbound %s=%s want retained=%t value=%d", path, got.Raw, tc.wantLength, value)
				}
			}
		})
	}
}

func TestResponsesSchemaPolicyHandlesEscapedKeywords(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","text":{"format":{"type":"json_schema","schema":{"type":"string","min\u004cength":1,"max\u004cength":10}}}}`)
	got := normalizeCodexStructuredOutputForTransport(body, false, false)
	var parsed map[string]any
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatal(err)
	}
	format := parsed["text"].(map[string]any)["format"].(map[string]any)
	schema := format["schema"].(map[string]any)
	if _, exists := schema["minLength"]; exists {
		t.Fatal("escaped length keyword bypassed the policy")
	}
	if _, exists := schema["maxLength"]; exists {
		t.Fatal("escaped length keyword bypassed the policy")
	}
}
