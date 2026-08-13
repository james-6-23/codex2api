package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

func TestExecuteGrokProtocolRequestUsesCatalogBackend(t *testing.T) {
	var gotPath string
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1,\"total_tokens\":3}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	account := &auth.Account{UpstreamType: auth.UpstreamGrok, APIKey: "xai", BaseURL: server.URL + "/v1"}
	account.SetGrokRoutingState(auth.GrokRoutingState{Models: []auth.GrokModelRoute{{ModelID: "grok-4.5", BaseURL: server.URL + "/v1", APIBackend: auth.GrokProtocolChatCompletions}}})
	responsesBody := []byte(`{"model":"grok-4.5","stream":true,"input":[{"role":"user","content":"hello"}]}`)
	resp, err := ExecuteGrokProtocolRequest(context.Background(), account, GrokProtocolResponses, nil, responsesBody, "", nil)
	if err != nil {
		t.Fatalf("ExecuteGrokProtocolRequest: %v", err)
	}
	defer resp.Body.Close()
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("path = %q", gotPath)
	}
	if gjson.GetBytes(gotBody, "messages.0.content").String() != "hello" || !gjson.GetBytes(gotBody, "stream").Bool() {
		t.Fatalf("converted chat body = %s", gotBody)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read adapted response: %v", err)
	}
	if !bytes.Contains(data, []byte(`"type":"response.output_text.delta"`)) || !bytes.Contains(data, []byte(`"type":"response.completed"`)) {
		t.Fatalf("adapted response is not Responses SSE: %s", data)
	}
	if !bytes.Contains(data, []byte(`"text":"hi"`)) {
		t.Fatalf("completed response lost authoritative output: %s", data)
	}
}

func TestPrepareRoutedGrokResponsesPreservesInboundChatControlsAndMappedModel(t *testing.T) {
	route := GrokUpstreamRoute{Model: "mapped-grok", Protocol: GrokProtocolResponses}
	inbound := []byte(`{
		"model":"client-alias","messages":[{"role":"user","content":"hello"}],
		"temperature":0.25,"top_p":0.8,"max_tokens":77,"stop":["END"],"seed":17
	}`)
	handlerCanonical := []byte(`{"model":"mapped-grok","input":"handler-normalized"}`)
	body, err := prepareRoutedGrokProtocolBody(route, GrokProtocolChatCompletions, inbound, handlerCanonical)
	if err != nil {
		t.Fatal(err)
	}
	checks := map[string]string{
		"model":             "mapped-grok",
		"temperature":       "0.25",
		"top_p":             "0.8",
		"max_output_tokens": "77",
		"stop.0":            "END",
		"seed":              "17",
		"input.0.role":      "user",
	}
	for path, want := range checks {
		if got := gjson.GetBytes(body, path).String(); got != want {
			t.Fatalf("%s = %q, want %q; body=%s", path, got, want, body)
		}
	}
}

func TestPrepareRoutedGrokResponsesPreservesInboundMessagesControls(t *testing.T) {
	route := GrokUpstreamRoute{Model: "mapped-grok", Protocol: GrokProtocolResponses}
	inbound := []byte(`{
		"model":"claude-alias","max_tokens":91,"messages":[{"role":"user","content":"hello"}],
		"temperature":0.4,"top_p":0.7,"stop_sequences":["END"],
		"output_config":{"effort":"high","format":{"type":"json_schema","schema":{"type":"object"}}}
	}`)
	body, err := prepareRoutedGrokProtocolBody(route, GrokProtocolMessages, inbound, []byte(`{"model":"mapped-grok"}`))
	if err != nil {
		t.Fatal(err)
	}
	checks := map[string]string{
		"model":             "mapped-grok",
		"max_output_tokens": "91",
		"temperature":       "0.4",
		"top_p":             "0.7",
		"stop.0":            "END",
		"text.format.type":  "json_schema",
		"reasoning.effort":  "high",
	}
	for path, want := range checks {
		if got := gjson.GetBytes(body, path).String(); got != want {
			t.Fatalf("%s = %q, want %q; body=%s", path, got, want, body)
		}
	}

	withTopK := []byte(`{"model":"grok","max_tokens":1,"top_k":40,"messages":[{"role":"user","content":"hi"}]}`)
	if _, err := prepareRoutedGrokProtocolBody(route, GrokProtocolMessages, withTopK, []byte(`{"model":"mapped-grok"}`)); err == nil {
		t.Fatal("Messages top_k must fail rather than silently disappear")
	}
}

func TestResolveGrokRouteFreshCapabilityOverridesCatalog(t *testing.T) {
	now := time.Now()
	account := &auth.Account{UpstreamType: auth.UpstreamGrok, AccessToken: "at", BaseURL: "https://default.example/v1"}
	account.SetGrokRoutingState(auth.GrokRoutingState{
		Models:       []auth.GrokModelRoute{{ModelID: "grok-4.5", APIBackend: auth.GrokProtocolResponses}},
		Capabilities: []auth.GrokProtocolCapability{{ModelID: "grok-4.5", Origin: "https://default.example/v1", Protocol: auth.GrokProtocolMessages, Status: auth.GrokCapabilityOK, ExpiresAt: now.Add(time.Hour)}},
	})
	route := ResolveGrokUpstreamRoute(account, "grok-4.5", GrokProtocolMessages, now)
	if route.Protocol != GrokProtocolMessages || !route.Native || route.Endpoint != "https://default.example/v1/messages" {
		t.Fatalf("route = %#v", route)
	}
}

func TestExecuteGrokNativeProtocolProbeForcesExactEndpoint(t *testing.T) {
	var gotPath string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","content":[],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer server.Close()
	account := &auth.Account{UpstreamType: auth.UpstreamGrok, APIKey: "xai", BaseURL: server.URL + "/v1"}
	account.SetGrokRoutingState(auth.GrokRoutingState{Models: []auth.GrokModelRoute{{ModelID: "grok-4.5", APIBackend: auth.GrokProtocolResponses}}})
	resp, err := ExecuteGrokNativeProtocolProbe(context.Background(), account, GrokProtocolMessages, "grok-4.5", []byte(`{"model":"grok-4.5","messages":[{"role":"user","content":"hi"}],"max_tokens":1}`), "")
	if err != nil {
		t.Fatalf("native probe: %v", err)
	}
	resp.Body.Close()
	if gotPath != "/v1/messages" {
		t.Fatalf("probe path = %q, want exact Messages path", gotPath)
	}
}

func TestExecuteGrokNativeProtocolProbeUsesExplicitCatalogOrigin(t *testing.T) {
	var defaultCalls, catalogCalls int
	defaultServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defaultCalls++
		w.WriteHeader(http.StatusTeapot)
	}))
	defer defaultServer.Close()
	catalogServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		catalogCalls++
		if r.URL.Path != "/v1/messages" {
			t.Fatalf("catalog path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_catalog","type":"message","content":[]}`)
	}))
	defer catalogServer.Close()

	account := &auth.Account{UpstreamType: auth.UpstreamGrok, APIKey: "xai", BaseURL: defaultServer.URL + "/v1"}
	resp, err := ExecuteGrokNativeProtocolProbeAtOrigin(context.Background(), account, GrokProtocolMessages, "grok-4.5", nil, catalogServer.URL+"/v1", "")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if defaultCalls != 0 || catalogCalls != 1 {
		t.Fatalf("default calls=%d catalog calls=%d", defaultCalls, catalogCalls)
	}
}

func TestExecuteGrokProtocolRequestNativePreservesInboundBody(t *testing.T) {
	var gotBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()
	now := time.Now()
	account := &auth.Account{UpstreamType: auth.UpstreamGrok, APIKey: "xai", BaseURL: server.URL + "/v1", CredentialGeneration: 1}
	account.SetGrokRoutingState(auth.GrokRoutingState{
		CredentialGeneration: 1,
		Models:               []auth.GrokModelRoute{{ModelID: "mapped-model", BaseURL: server.URL + "/v1", APIBackend: auth.GrokProtocolResponses}},
		Capabilities:         []auth.GrokProtocolCapability{{ModelID: "mapped-model", Origin: server.URL + "/v1", Protocol: auth.GrokProtocolChatCompletions, Status: auth.GrokCapabilityOK, ExpiresAt: now.Add(time.Hour)}},
	})
	inbound := []byte(`{"model":"client-alias","messages":[{"role":"user","content":"hi"}],"stream":false,"future_standard_field":{"keep":true}}`)
	responses := []byte(`{"model":"mapped-model","input":"translated","stream":true}`)
	resp, err := ExecuteGrokProtocolRequest(context.Background(), account, GrokProtocolChatCompletions, inbound, responses, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	responseBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if gjson.GetBytes(gotBody, "model").String() != "mapped-model" || gjson.GetBytes(gotBody, "stream").Bool() {
		t.Fatalf("native body was not model-only rewritten: %s", gotBody)
	}
	if !gjson.GetBytes(gotBody, "future_standard_field.keep").Bool() || gjson.GetBytes(gotBody, "messages.0.content").String() != "hi" {
		t.Fatalf("native body lost unknown/input fields: %s", gotBody)
	}
	if gjson.GetBytes(responseBody, "id").String() != "chatcmpl-1" || bytes.Contains(responseBody, []byte("response.completed")) {
		t.Fatalf("native non-stream response was not transparent: %s", responseBody)
	}
}

func TestPrepareRoutedGrokCatalogSameProtocolPreservesUnknownFields(t *testing.T) {
	tests := []struct {
		name     string
		protocol GrokProtocol
		inbound  []byte
		path     string
	}{
		{
			name: "responses", protocol: GrokProtocolResponses,
			inbound: []byte(`{"model":"client-alias","input":"hi","stream":false,"future_standard":{"keep":true}}`),
			path:    "future_standard.keep",
		},
		{
			name: "chat", protocol: GrokProtocolChatCompletions,
			inbound: []byte(`{"model":"client-alias","messages":[{"role":"user","content":"hi"}],"stream":false,"future_standard":{"keep":true}}`),
			path:    "future_standard.keep",
		},
		{
			name: "messages", protocol: GrokProtocolMessages,
			inbound: []byte(`{"model":"client-alias","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"stream":false,"future_standard":{"keep":true}}`),
			path:    "future_standard.keep",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			route := GrokUpstreamRoute{Model: "mapped-grok", Protocol: tc.protocol, Native: false}
			got, err := prepareRoutedGrokProtocolBody(route, tc.protocol, tc.inbound, []byte(`{"model":"mapped-grok","input":"handler-canonical"}`))
			if err != nil {
				t.Fatal(err)
			}
			if gjson.GetBytes(got, "model").String() != "mapped-grok" || !gjson.GetBytes(got, tc.path).Bool() {
				t.Fatalf("catalog same-protocol body was not transparent: %s", got)
			}
			// 非 native 路由的响应必须经 SSE 投影管线消费,客户端的 stream=false
			// 必须被强制为流式,否则非流式 JSON 形状进管线会确定性失败。
			if !gjson.GetBytes(got, "stream").Bool() {
				t.Fatalf("non-native same-protocol route must force upstream streaming: %s", got)
			}
			if tc.protocol == GrokProtocolChatCompletions && !gjson.GetBytes(got, "stream_options.include_usage").Bool() {
				t.Fatalf("forced chat streaming must request the usage chunk: %s", got)
			}
		})
	}
}

func TestPrepareRoutedGrokNativeSameProtocolPreservesClientStreamFalse(t *testing.T) {
	// native 直通路由按线格式原样转发响应,stream=false 必须原样保留。
	route := GrokUpstreamRoute{Model: "mapped-grok", Protocol: GrokProtocolChatCompletions, Native: true}
	inbound := []byte(`{"model":"client-alias","messages":[{"role":"user","content":"hi"}],"stream":false}`)
	got, err := prepareRoutedGrokProtocolBody(route, GrokProtocolChatCompletions, inbound, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stream := gjson.GetBytes(got, "stream"); !stream.Exists() || stream.Bool() {
		t.Fatalf("native passthrough must preserve explicit stream=false: %s", got)
	}
	if gjson.GetBytes(got, "stream_options").Exists() {
		t.Fatalf("native passthrough must not inject stream_options: %s", got)
	}
}

func TestPrepareRoutedGrokProtocolBodyRejectsUnrepresentableSemantic(t *testing.T) {
	route := GrokUpstreamRoute{Protocol: GrokProtocolMessages}
	_, err := prepareRoutedGrokProtocolBody(route, GrokProtocolResponses, nil, []byte(`{"model":"grok-4.5","input":"hi","tools":[{"type":"web_search"}]}`))
	if err == nil {
		t.Fatal("hosted tool conversion must fail rather than silently drop")
	}
	structured := ErrBadRequest(err.Error())
	if structured.HTTPStatus != http.StatusBadRequest || structured.Retryable {
		t.Fatalf("conversion error = %#v", structured)
	}
}

func TestResponsesStructuredOutputMapsToChatAndMessages(t *testing.T) {
	body := []byte(`{
		"model":"grok-4.5","input":"return JSON","stop":["END","STOP"],
		"reasoning":{"effort":"high"},
		"text":{"format":{"type":"json_schema","name":"answer","schema":{"type":"object","properties":{"answer":{"type":"string"}},"required":["answer"],"additionalProperties":false},"strict":true}}
	}`)
	chat, err := convertResponsesToChatRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(chat, "response_format.type").String() != "json_schema" ||
		gjson.GetBytes(chat, "response_format.json_schema.name").String() != "answer" ||
		!gjson.GetBytes(chat, "response_format.json_schema.strict").Bool() {
		t.Fatalf("chat structured output mapping = %s", chat)
	}
	if gjson.GetBytes(chat, "reasoning_effort").String() != "high" {
		t.Fatalf("chat reasoning mapping = %s", chat)
	}

	messages, err := convertResponsesToMessagesRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(messages, "output_config.format.type").String() != "json_schema" ||
		gjson.GetBytes(messages, "output_config.format.json_schema.name").String() != "answer" ||
		gjson.GetBytes(messages, "output_config.effort").String() != "high" {
		t.Fatalf("messages structured output mapping = %s", messages)
	}
	if got := gjson.GetBytes(messages, "stop_sequences.#").Int(); got != 2 {
		t.Fatalf("messages stop_sequences count = %d; body=%s", got, messages)
	}
}

func TestResponsesEncryptedReasoningCannotConvertToChat(t *testing.T) {
	_, err := convertResponsesToChatRequest([]byte(`{
		"model":"grok-4.5",
		"input":[{"type":"reasoning","summary":[{"type":"summary_text","text":"visible"}],"encrypted_content":"opaque"}]
	}`))
	if err == nil || !bytes.Contains([]byte(err.Error()), []byte("encrypted reasoning")) {
		t.Fatalf("encrypted reasoning conversion error = %v", err)
	}
}

func TestResponsesImagesToMessagesRequireDataURI(t *testing.T) {
	remote := []byte(`{
		"model":"grok-4.5",
		"input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"https://example.com/private.png"}]}]
	}`)
	if _, err := convertResponsesToMessagesRequest(remote); err == nil || !bytes.Contains([]byte(err.Error()), []byte("remote image")) {
		t.Fatalf("remote image conversion error = %v", err)
	}

	dataURI := []byte(`{
		"model":"grok-4.5",
		"input":[{"type":"message","role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,AAAB"}]}]
	}`)
	messages, err := convertResponsesToMessagesRequest(dataURI)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(messages, "messages.0.content.0.source.type").String() != "base64" ||
		gjson.GetBytes(messages, "messages.0.content.0.source.media_type").String() != "image/png" ||
		gjson.GetBytes(messages, "messages.0.content.0.source.data").String() != "AAAB" {
		t.Fatalf("data URI image mapping = %s", messages)
	}
}

func TestResponsesConvertersRejectUnknownToolResultCall(t *testing.T) {
	body := []byte(`{"model":"grok-4.5","input":[{"type":"function_call_output","call_id":"missing","output":"result"}]}`)
	if _, err := convertResponsesToChatRequest(body); err == nil || !bytes.Contains([]byte(err.Error()), []byte("unknown call_id")) {
		t.Fatalf("Chat orphan conversion error = %v", err)
	}
	if _, err := convertResponsesToMessagesRequest(body); err == nil || !bytes.Contains([]byte(err.Error()), []byte("unknown call_id")) {
		t.Fatalf("Messages orphan conversion error = %v", err)
	}
}

func TestResponsesToolChoiceMapsAcrossProtocols(t *testing.T) {
	body := []byte(`{
		"model":"grok-4.5","input":"hi","parallel_tool_calls":false,
		"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],
		"tool_choice":{"type":"function","name":"lookup"}
	}`)
	chat, err := convertResponsesToChatRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(chat, "tool_choice.function.name").String() != "lookup" || gjson.GetBytes(chat, "parallel_tool_calls").Bool() {
		t.Fatalf("Chat tool choice mapping = %s", chat)
	}
	messages, err := convertResponsesToMessagesRequest(body)
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(messages, "tool_choice.type").String() != "tool" ||
		gjson.GetBytes(messages, "tool_choice.name").String() != "lookup" ||
		!gjson.GetBytes(messages, "tool_choice.disable_parallel_tool_use").Bool() {
		t.Fatalf("Messages tool choice mapping = %s", messages)
	}
}

func TestExecuteGrokProtocolRequest426RefreshesSettingsAndRetriesOnce(t *testing.T) {
	var inferenceCalls, settingsCalls int
	var versions []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/settings":
			settingsCalls++
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"min_client_version":"0.2.999"}`)
		case "/v1/responses":
			inferenceCalls++
			versions = append(versions, r.Header.Get("x-grok-client-version"))
			w.Header().Set("Content-Type", "text/event-stream")
			if inferenceCalls == 1 {
				w.WriteHeader(http.StatusUpgradeRequired)
				_, _ = io.WriteString(w, `{"error":{"code":"client_upgrade_required"}}`)
				return
			}
			_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")
		default:
			t.Fatalf("unexpected path %q", r.URL.Path)
		}
	}))
	defer server.Close()
	account := &auth.Account{UpstreamType: auth.UpstreamGrok, AccessToken: "at", BaseURL: server.URL + "/v1"}
	resp, err := ExecuteGrokProtocolRequest(context.Background(), account, GrokProtocolResponses, nil, []byte(`{"model":"grok-4.5","input":"hi","stream":true}`), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || inferenceCalls != 2 || settingsCalls != 1 {
		t.Fatalf("status/calls = %d inference=%d settings=%d", resp.StatusCode, inferenceCalls, settingsCalls)
	}
	if len(versions) != 2 || versions[1] != "0.2.999" {
		t.Fatalf("versions = %#v", versions)
	}
}

func TestMessagesAdapterRequiresMessageStopAndKeepsSparseTools(t *testing.T) {
	source := io.NopCloser(bytes.NewBufferString(
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"model\":\"grok\",\"usage\":{\"input_tokens\":1}}}\n\n" +
			"data: {\"type\":\"content_block_start\",\"index\":3,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call3\",\"name\":\"three\"}}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":3,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{}\"}}\n\n" +
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":1}}\n\n" +
			"data: {\"type\":\"message_stop\"}\n\n"))
	data, err := io.ReadAll(newMessagesToResponsesReader(source, "grok"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"call_id":"call3"`)) || !bytes.Contains(data, []byte(`"type":"response.completed"`)) {
		t.Fatalf("sparse tool terminal lost: %s", data)
	}

	missingStop := io.NopCloser(bytes.NewBufferString(
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\"}}\n\n" +
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n"))
	data, err = io.ReadAll(newMessagesToResponsesReader(missingStop, "grok"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(`"type":"response.completed"`)) || !bytes.Contains(data, []byte(ErrorCodeUpstreamStreamBreak)) {
		t.Fatalf("missing message_stop was treated as success: %s", data)
	}
}

func TestMessagesAdapterKeepsReasoningSignatureInFinalOutput(t *testing.T) {
	source := io.NopCloser(bytes.NewBufferString(
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"model\":\"grok\",\"usage\":{\"input_tokens\":1}}}\n\n" +
			"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\"}}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"why\"}}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"ENC\"}}\n\n" +
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
			"data: {\"type\":\"message_stop\"}\n\n"))
	data, err := io.ReadAll(newMessagesToResponsesReader(source, "grok"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"encrypted_content":"ENC"`)) || !bytes.Contains(data, []byte(`"text":"why"`)) {
		t.Fatalf("reasoning final output lost signature or summary: %s", data)
	}
}

func TestChatAdapterPropagatesErrorAndSparseTool(t *testing.T) {
	errorSource := io.NopCloser(bytes.NewBufferString("data: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}\n\n"))
	data, err := io.ReadAll(newChatToResponsesReader(errorSource, "grok"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"type":"response.failed"`)) || bytes.Contains(data, []byte(`"type":"response.completed"`)) {
		t.Fatalf("chat error was hidden: %s", data)
	}

	toolSource := io.NopCloser(bytes.NewBufferString(
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":4,\"id\":\"call4\",\"function\":{\"name\":\"four\",\"arguments\":\"{}\"}}]},\"finish_reason\":null}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n"))
	data, err = io.ReadAll(newChatToResponsesReader(toolSource, "grok"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte(`"call_id":"call4"`)) || !bytes.Contains(data, []byte(`"type":"response.completed"`)) {
		t.Fatalf("chat sparse tool lost: %s", data)
	}
}

func TestChatAdapterCapturesUsageOnlyChunkAfterFinish(t *testing.T) {
	// include_usage 流:usage 在 finish chunk 之后以 choices 为空的独立 chunk 下发,
	// 终态必须携带这份 usage 而不是提前发出零值。
	source := io.NopCloser(bytes.NewBufferString(
		"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":null}\n\n" +
			"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2,\"total_tokens\":9,\"prompt_tokens_details\":{\"cached_tokens\":3}}}\n\n" +
			"data: [DONE]\n\n"))
	stream, err := io.ReadAll(newChatToResponsesReader(source, "grok"))
	if err != nil {
		t.Fatal(err)
	}
	completed := completedGrokResponseEvent(t, stream)
	usage := gjson.GetBytes(completed, "response.usage")
	if usage.Get("input_tokens").Int() != 7 || usage.Get("output_tokens").Int() != 2 || usage.Get("total_tokens").Int() != 9 {
		t.Fatalf("usage-only chunk lost: %s", completed)
	}
	if usage.Get("input_tokens_details.cached_tokens").Int() != 3 {
		t.Fatalf("cached tokens lost: %s", completed)
	}
	if count := bytes.Count(stream, []byte(`"type":"response.completed"`)); count != 1 {
		t.Fatalf("terminal emitted %d times, want 1: %s", count, stream)
	}
}

func TestChatAdapterFinishWithoutUsageChunkStillCompletesAtEOF(t *testing.T) {
	// 上游承诺 include_usage 但实际没发 usage chunk 也没发 [DONE]:EOF 时应按
	// 挂起的 finish_reason 正常收尾,而不是误判为断流。
	source := io.NopCloser(bytes.NewBufferString(
		"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":null}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
	stream, err := io.ReadAll(newChatToResponsesReader(source, "grok"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(stream, []byte(`"type":"response.completed"`)) || bytes.Contains(stream, []byte(`"type":"response.failed"`)) {
		t.Fatalf("pending finish was not completed at EOF: %s", stream)
	}
}

func completedGrokResponseEvent(t *testing.T, stream []byte) []byte {
	t.Helper()
	for _, frame := range bytes.Split(stream, []byte("\n\n")) {
		payload := bytes.TrimSpace(frame)
		payload = bytes.TrimSpace(bytes.TrimPrefix(payload, []byte("data:")))
		if gjson.GetBytes(payload, "type").String() == "response.completed" {
			return append([]byte(nil), payload...)
		}
	}
	t.Fatalf("response.completed missing from stream: %s", stream)
	return nil
}

func TestChatAdapterFinalOutputPreservesFirstEventOrderAndToolIdentity(t *testing.T) {
	source := io.NopCloser(bytes.NewBufferString(
		"data: {\"choices\":[{\"delta\":{\"content\":\"before\"},\"finish_reason\":null}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":8,\"id\":\"call-z\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"a\\\":\"}}]},\"finish_reason\":null}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"think\"},\"finish_reason\":null}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":2,\"id\":\"call-y\",\"function\":{\"name\":\"other\",\"arguments\":\"{\\\"b\\\":2}\"}}]},\"finish_reason\":null}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":8,\"id\":\"call-overwrite\",\"function\":{\"name\":\"overwrite\",\"arguments\":\"1}\"}}]},\"finish_reason\":null}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{\"content\":\"after\"},\"finish_reason\":null}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":2,\"total_tokens\":3}}\n\n"))
	stream, err := io.ReadAll(newChatToResponsesReader(source, "grok"))
	if err != nil {
		t.Fatal(err)
	}
	completed := completedGrokResponseEvent(t, stream)
	output := gjson.GetBytes(completed, "response.output").Array()
	if len(output) != 4 {
		t.Fatalf("output length = %d, want 4; event=%s", len(output), completed)
	}
	wantTypes := []string{"message", "function_call", "reasoning", "function_call"}
	for i, want := range wantTypes {
		if got := output[i].Get("type").String(); got != want {
			t.Fatalf("output[%d].type = %q, want %q; event=%s", i, got, want, completed)
		}
	}
	if got := output[0].Get("content.0.text").String(); got != "beforeafter" {
		t.Fatalf("message text = %q, want beforeafter", got)
	}
	if got := output[1].Get("call_id").String(); got != "call-z" {
		t.Fatalf("first tool call_id = %q, want call-z", got)
	}
	if got := output[1].Get("name").String(); got != "lookup" {
		t.Fatalf("first tool name = %q, want lookup", got)
	}
	if got := output[1].Get("arguments").String(); got != `{"a":1}` {
		t.Fatalf("first tool arguments = %q, want exact concatenation", got)
	}
	if got := output[2].Get("summary.0.text").String(); got != "think" {
		t.Fatalf("reasoning text = %q, want think", got)
	}
	if got := output[3].Get("call_id").String(); got != "call-y" {
		t.Fatalf("second tool call_id = %q, want call-y", got)
	}
}

func TestMessagesAdapterFinalOutputPreservesBlockOrderAndBoundaries(t *testing.T) {
	source := io.NopCloser(bytes.NewBufferString(
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"m-order\",\"model\":\"grok\",\"usage\":{\"input_tokens\":1}}}\n\n" +
			"data: {\"type\":\"content_block_start\",\"index\":7,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":7,\"delta\":{\"type\":\"text_delta\",\"text\":\"A\"}}\n\n" +
			"data: {\"type\":\"content_block_start\",\"index\":2,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":2,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"T1\"}}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":2,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"SIG1\"}}\n\n" +
			"data: {\"type\":\"content_block_start\",\"index\":9,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call9\",\"name\":\"nine\"}}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":9,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"x\\\":9}\"}}\n\n" +
			"data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"B\"}}\n\n" +
			"data: {\"type\":\"content_block_start\",\"index\":3,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":3,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"T2\"}}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":3,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"SIG2\"}}\n\n" +
			"data: {\"type\":\"content_block_start\",\"index\":5,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call5\",\"name\":\"five\"}}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":5,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"[]\"}}\n\n" +
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":2}}\n\n" +
			"data: {\"type\":\"message_stop\"}\n\n"))
	stream, err := io.ReadAll(newMessagesToResponsesReader(source, "grok"))
	if err != nil {
		t.Fatal(err)
	}
	completed := completedGrokResponseEvent(t, stream)
	output := gjson.GetBytes(completed, "response.output").Array()
	if len(output) != 6 {
		t.Fatalf("output length = %d, want 6; event=%s", len(output), completed)
	}
	wantTypes := []string{"message", "reasoning", "function_call", "message", "reasoning", "function_call"}
	for i, want := range wantTypes {
		if got := output[i].Get("type").String(); got != want {
			t.Fatalf("output[%d].type = %q, want %q; event=%s", i, got, want, completed)
		}
	}
	if got := output[0].Get("content.0.text").String(); got != "A" {
		t.Fatalf("first text block = %q, want A", got)
	}
	if got := output[1].Get("summary.0.text").String(); got != "T1" {
		t.Fatalf("first reasoning block = %q, want T1", got)
	}
	if got := output[1].Get("encrypted_content").String(); got != "SIG1" {
		t.Fatalf("first reasoning signature = %q, want SIG1", got)
	}
	if got := output[2].Get("call_id").String(); got != "call9" {
		t.Fatalf("first tool call_id = %q, want call9", got)
	}
	if got := output[2].Get("arguments").String(); got != `{"x":9}` {
		t.Fatalf("first tool arguments = %q, want exact JSON", got)
	}
	if got := output[3].Get("content.0.text").String(); got != "B" {
		t.Fatalf("second text block = %q, want B", got)
	}
	if got := output[4].Get("summary.0.text").String(); got != "T2" {
		t.Fatalf("second reasoning block = %q, want T2", got)
	}
	if got := output[5].Get("call_id").String(); got != "call5" {
		t.Fatalf("second tool call_id = %q, want call5", got)
	}
	if got := output[5].Get("arguments").String(); got != `[]` {
		t.Fatalf("second tool arguments = %q, want []", got)
	}
}
