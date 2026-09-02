package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
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
		gotBody = readUpstreamRequestBody(r)
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

func TestExecuteGrokProtocolRequestBridgesResponsesToolsThroughChat(t *testing.T) {
	var captured []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		captured = readUpstreamRequestBody(r)
		aliases := make(map[string]string)
		for _, tool := range gjson.GetBytes(captured, "tools").Array() {
			if tool.Get("type").String() != "function" || tool.Get("function.name").String() == "" {
				t.Errorf("non-function Chat tool leaked upstream: %s", tool.Raw)
				continue
			}
			description := tool.Get("function.description").String()
			switch {
			case description == "synthetic custom tool":
				aliases["custom"] = tool.Get("function.name").String()
			case description == "synthetic namespace function":
				aliases["namespace"] = tool.Get("function.name").String()
			case description == "synthetic deferred custom tool":
				aliases["deferred"] = tool.Get("function.name").String()
			case strings.HasPrefix(description, "Search and load Codex tools"):
				aliases["tool_search"] = tool.Get("function.name").String()
			}
		}
		for _, name := range []string{"custom", "namespace", "deferred", "tool_search"} {
			if aliases[name] == "" {
				t.Errorf("%s tool was not converted: %s", name, captured)
			}
		}

		w.Header().Set("Content-Type", "text/event-stream")
		writeEvent := func(event any) {
			encoded, err := json.Marshal(event)
			if err != nil {
				t.Error(err)
				return
			}
			_, _ = w.Write([]byte("data: "))
			_, _ = w.Write(encoded)
			_, _ = w.Write([]byte("\n\n"))
		}
		writeEvent(map[string]any{
			"id": "chatcmpl_test", "model": "grok-4.5",
			"choices": []any{map[string]any{
				"delta": map[string]any{"tool_calls": []any{
					map[string]any{"index": 0, "id": "call_custom", "type": "function", "function": map[string]any{}},
				}},
				"finish_reason": nil,
			}},
		})
		writeEvent(map[string]any{
			"id": "chatcmpl_test", "model": "grok-4.5",
			"choices": []any{map[string]any{
				"delta": map[string]any{"tool_calls": []any{
					map[string]any{"index": 0, "function": map[string]any{"name": aliases["custom"], "arguments": `{"input":"patch text"}`}},
					map[string]any{"index": 1, "id": "call_namespace", "type": "function", "function": map[string]any{"name": aliases["namespace"], "arguments": `{"path":"README.md"}`}},
					map[string]any{"index": 2, "id": "call_search", "type": "function", "function": map[string]any{"name": aliases["tool_search"], "arguments": `{"query":"files","limit":2}`}},
				}},
				"finish_reason": nil,
			}},
		})
		writeEvent(map[string]any{
			"id":      "chatcmpl_test",
			"choices": []any{map[string]any{"delta": map[string]any{}, "finish_reason": "tool_calls"}},
			"usage":   map[string]any{"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8},
		})
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	account := &auth.Account{
		UpstreamType: auth.UpstreamGrok,
		APIKey:       "test",
		BaseURL:      server.URL + "/v1",
		ModelMapping: `{"gpt-5.5":"grok-4.5"}`,
	}
	account.SetGrokRoutingState(auth.GrokRoutingState{Models: []auth.GrokModelRoute{{ModelID: "grok-4.5", BaseURL: server.URL + "/v1", APIBackend: auth.GrokProtocolChatCompletions}}})
	body := []byte(`{
		"model":"gpt-5.5","stream":true,
		"tools":[
			{"type":"custom","name":"run_patch","description":"synthetic custom tool"},
			{"type":"namespace","name":"workspace","tools":[{"type":"function","name":"read_file","description":"synthetic namespace function","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}]},
			{"type":"tool_search"}
		],
		"input":[
			{"type":"additional_tools","tools":[{"type":"custom","name":"deferred_run","description":"synthetic deferred custom tool"}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"use the tools"}]}
		]
	}`)
	handler := NewHandler(nil, nil, nil, nil)
	mappedBody, mappedModel, mapped := handler.applyAccountModelMappingToBody(body, account)
	if !mapped || mappedModel != "grok-4.5" {
		t.Fatalf("mapped model = %q, applied=%t", mappedModel, mapped)
	}
	resp, err := ExecuteGrokProtocolRequest(context.Background(), account, GrokProtocolResponses, body, mappedBody, "", nil)
	if err != nil {
		t.Fatalf("ExecuteGrokProtocolRequest: %v", err)
	}
	defer resp.Body.Close()
	stream, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(captured, []byte(`"additional_tools"`)) || gjson.GetBytes(captured, "messages.#").Int() != 1 {
		t.Fatalf("additional_tools was not lifted before Chat conversion: %s", captured)
	}
	if got := gjson.GetBytes(captured, "tools.#").Int(); got != 4 {
		t.Fatalf("converted tool count = %d, want 4; body=%s", got, captured)
	}
	if got := gjson.GetBytes(captured, "model").String(); got != "grok-4.5" {
		t.Fatalf("mapped upstream model = %q, want grok-4.5; body=%s", got, captured)
	}

	events := grokResponseSSEEvents(stream)
	eventIndex := func(eventType, callID string) int {
		for i, event := range events {
			if event.Get("type").String() != eventType {
				continue
			}
			gotCallID := event.Get("call_id").String()
			if gotCallID == "" {
				gotCallID = event.Get("item.call_id").String()
			}
			if callID == "" || gotCallID == callID {
				return i
			}
		}
		return -1
	}
	customAdded := eventIndex("response.output_item.added", "call_custom")
	customInputDone := eventIndex("response.custom_tool_call_input.done", "call_custom")
	customDone := eventIndex("response.output_item.done", "call_custom")
	namespaceArgsDone := eventIndex("response.function_call_arguments.done", "call_namespace")
	namespaceDone := eventIndex("response.output_item.done", "call_namespace")
	searchDone := eventIndex("response.output_item.done", "call_search")
	completedIndex := eventIndex("response.completed", "")
	if customAdded < 0 || customInputDone <= customAdded || customDone <= customInputDone ||
		namespaceArgsDone < 0 || namespaceDone <= namespaceArgsDone || searchDone < 0 || completedIndex <= searchDone {
		t.Fatalf("restored tool lifecycle is incomplete or out of order: %s", stream)
	}
	if eventIndex("response.function_call_arguments.done", "call_search") >= 0 {
		t.Fatalf("tool_search arguments.done leaked downstream: %s", stream)
	}
	if got := events[customAdded].Get("item.type").String(); got != "custom_tool_call" {
		t.Fatalf("custom added type = %q", got)
	}
	if got := events[customInputDone].Get("input").String(); got != "patch text" {
		t.Fatalf("custom input = %q", got)
	}
	if got := events[namespaceDone].Get("item.name").String(); got != "read_file" || events[namespaceDone].Get("item.namespace").String() != "workspace" {
		t.Fatalf("namespace call was not restored: %s", events[namespaceDone].Raw)
	}
	if got := events[searchDone].Get("item.type").String(); got != "tool_search_call" || events[searchDone].Get("item.execution").String() != "client" || events[searchDone].Get("item.arguments.query").String() != "files" {
		t.Fatalf("tool_search call was not restored: %s", events[searchDone].Raw)
	}
	completed := events[completedIndex]
	if completed.Get(`response.output.#(call_id=="call_custom").type`).String() != "custom_tool_call" ||
		completed.Get(`response.output.#(call_id=="call_namespace").namespace`).String() != "workspace" ||
		completed.Get(`response.output.#(call_id=="call_search").type`).String() != "tool_search_call" {
		t.Fatalf("completed output lost restored tool identities: %s", completed.Raw)
	}
}

func TestExecuteGrokProtocolRequestBridgesCustomToolThroughMessages(t *testing.T) {
	var captured []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			t.Errorf("path = %q", r.URL.Path)
		}
		captured = readUpstreamRequestBody(r)
		alias := gjson.GetBytes(captured, "tools.0.name").String()
		if alias == "" || gjson.GetBytes(captured, "tools.0.input_schema.required.0").String() != "input" {
			t.Errorf("custom tool was not converted for Messages: %s", captured)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		writeEvent := func(event any) {
			encoded, err := json.Marshal(event)
			if err != nil {
				t.Error(err)
				return
			}
			_, _ = w.Write([]byte("data: "))
			_, _ = w.Write(encoded)
			_, _ = w.Write([]byte("\n\n"))
		}
		writeEvent(map[string]any{"type": "message_start", "message": map[string]any{"id": "msg_test", "model": "grok-4.5", "usage": map[string]any{"input_tokens": 2}}})
		writeEvent(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "tool_use", "id": "call_messages", "name": alias}})
		writeEvent(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "input_json_delta", "partial_json": `{"input":"message patch"}`}})
		writeEvent(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"}, "usage": map[string]any{"output_tokens": 1}})
		writeEvent(map[string]any{"type": "message_stop"})
	}))
	defer server.Close()

	account := &auth.Account{UpstreamType: auth.UpstreamGrok, APIKey: "test", BaseURL: server.URL + "/v1"}
	account.SetGrokRoutingState(auth.GrokRoutingState{Models: []auth.GrokModelRoute{{ModelID: "grok-4.5", BaseURL: server.URL + "/v1", APIBackend: auth.GrokProtocolMessages}}})
	body := []byte(`{
		"model":"grok-4.5","stream":true,
		"tools":[{"type":"custom","name":"run_patch","description":"synthetic custom tool"}],
		"input":[{"type":"message","role":"user","content":"use the tool"}]
	}`)
	resp, err := ExecuteGrokProtocolRequest(context.Background(), account, GrokProtocolResponses, nil, body, "", nil)
	if err != nil {
		t.Fatalf("ExecuteGrokProtocolRequest: %v", err)
	}
	defer resp.Body.Close()
	stream, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	events := grokResponseSSEEvents(stream)
	var inputDone, itemDone, completed gjson.Result
	for _, event := range events {
		switch event.Get("type").String() {
		case "response.custom_tool_call_input.done":
			inputDone = event
		case "response.output_item.done":
			itemDone = event
		case "response.completed":
			completed = event
		}
	}
	if inputDone.Get("input").String() != "message patch" || itemDone.Get("item.type").String() != "custom_tool_call" || completed.Get("response.output.0.type").String() != "custom_tool_call" {
		t.Fatalf("Messages custom tool was not restored: %s", stream)
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
		"tools":[{"name":"lookup","input_schema":{"type":"object"}}],
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
		"reasoning.summary": "detailed",
		"include.0":         "reasoning.encrypted_content",
		"include.1":         "no_inline_citations",
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

func TestResolveGrokRouteKeepsCatalogResponsesWhenMessagesProbeIsFresh(t *testing.T) {
	now := time.Now()
	account := &auth.Account{UpstreamType: auth.UpstreamGrok, AccessToken: "at", BaseURL: "https://default.example/v1"}
	account.SetGrokRoutingState(auth.GrokRoutingState{
		Models:       []auth.GrokModelRoute{{ModelID: "grok-4.5", BaseURL: "https://default.example/v1", APIBackend: auth.GrokProtocolResponses}},
		Capabilities: []auth.GrokProtocolCapability{{ModelID: "grok-4.5", Origin: "https://default.example/v1", Protocol: auth.GrokProtocolMessages, Status: auth.GrokCapabilityOK, ExpiresAt: now.Add(time.Hour)}},
	})
	route := ResolveGrokUpstreamRoute(account, "grok-4.5", GrokProtocolMessages, now)
	if route.Protocol != GrokProtocolResponses || route.Native || route.Endpoint != "https://default.example/v1/responses" {
		t.Fatalf("Claude Code Messages inbound must stay on catalog Responses: %#v", route)
	}
}

func TestExecuteGrokProtocolRequestReusesConversationHeaders(t *testing.T) {
	var sessions []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		sessions = append(sessions, r.Header.Get("x-grok-session-id"))
		if r.Header.Get("x-grok-session-id") == "" || r.Header.Get("x-grok-session-id") != r.Header.Get("x-grok-conv-id") {
			t.Fatalf("session=%q conv=%q", r.Header.Get("x-grok-session-id"), r.Header.Get("x-grok-conv-id"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n")
	}))
	defer server.Close()

	account := &auth.Account{UpstreamType: auth.UpstreamGrok, AccessToken: "at", BaseURL: server.URL + "/v1"}
	account.SetGrokRoutingState(auth.GrokRoutingState{
		Models: []auth.GrokModelRoute{{ModelID: "grok-4.6", BaseURL: server.URL + "/v1", APIBackend: auth.GrokProtocolResponses}},
	})
	turn1 := []byte(`{"model":"grok-4.6","system":"rules","messages":[{"role":"user","content":"分析项目"}]}`)
	turn2 := []byte(`{"model":"grok-4.6","system":"rules","messages":[{"role":"user","content":"分析项目"},{"role":"assistant","content":"ok"},{"role":"user","content":"继续"}]}`)
	stub := []byte(`{"model":"grok-4.6"}`)
	for _, inbound := range [][]byte{turn1, turn2} {
		resp, err := ExecuteGrokProtocolRequest(context.Background(), account, GrokProtocolMessages, inbound, stub, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.ReadAll(resp.Body)
		resp.Body.Close()
	}
	if len(sessions) != 2 || sessions[0] == "" || sessions[0] != sessions[1] {
		t.Fatalf("session headers = %#v", sessions)
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
		gotBody = readUpstreamRequestBody(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer server.Close()
	now := time.Now()
	account := &auth.Account{UpstreamType: auth.UpstreamGrok, APIKey: "xai", BaseURL: server.URL + "/v1", CredentialGeneration: 1}
	account.SetGrokRoutingState(auth.GrokRoutingState{
		CredentialGeneration: 1,
		Models:               []auth.GrokModelRoute{{ModelID: "mapped-model", BaseURL: server.URL + "/v1", APIBackend: auth.GrokProtocolChatCompletions}},
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
	if !bytes.Contains(data, []byte(`"call_id":"call3"`)) ||
		!bytes.Contains(data, []byte(`"type":"response.function_call_arguments.done"`)) ||
		!bytes.Contains(data, []byte(`"type":"response.output_item.done"`)) ||
		!bytes.Contains(data, []byte(`"type":"response.completed"`)) {
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

	truncated := io.NopCloser(bytes.NewBufferString(
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"model\":\"grok\",\"usage\":{\"input_tokens\":1}}}\n\n" +
			"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call_partial\",\"name\":\"lookup\"}}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"query\\\":\"}}\n\n" +
			"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"},\"usage\":{\"output_tokens\":1}}\n\n" +
			"data: {\"type\":\"message_stop\"}\n\n"))
	data, err = io.ReadAll(newMessagesToResponsesReader(truncated, "grok"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(data, []byte(`"type":"response.function_call_arguments.done"`)) || bytes.Contains(data, []byte(`"type":"response.output_item.done"`)) {
		t.Fatalf("truncated Messages tool input was marked complete: %s", data)
	}
	if !bytes.Contains(data, []byte(`"type":"response.incomplete"`)) || bytes.Contains(data, []byte(`"type":"response.completed"`)) {
		t.Fatalf("max_tokens did not use the incomplete terminal event: %s", data)
	}
	completed := completedGrokResponseEvent(t, data)
	if got := gjson.GetBytes(completed, "response.output.0.status").String(); got != "incomplete" {
		t.Fatalf("truncated Messages tool status = %q, want incomplete: %s", got, completed)
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

func TestMessagesAdapterEmitsCompleteTextAndThinkingLifecycle(t *testing.T) {
	source := io.NopCloser(bytes.NewBufferString(
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"m-life\",\"model\":\"grok\",\"usage\":{\"input_tokens\":1}}}\n\n" +
			"data: {\"type\":\"content_block_start\",\"index\":7,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":7,\"delta\":{\"type\":\"text_delta\",\"text\":\"A\"}}\n\n" +
			"data: {\"type\":\"content_block_stop\",\"index\":7}\n\n" +
			"data: {\"type\":\"content_block_start\",\"index\":2,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":2,\"delta\":{\"type\":\"text_delta\",\"text\":\"B\"}}\n\n" +
			"data: {\"type\":\"content_block_stop\",\"index\":2}\n\n" +
			"data: {\"type\":\"content_block_start\",\"index\":9,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":9,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"why\"}}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":9,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"ENC\"}}\n\n" +
			"data: {\"type\":\"content_block_stop\",\"index\":9}\n\n" +
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n" +
			"data: {\"type\":\"message_stop\"}\n\n"))
	stream, err := io.ReadAll(newMessagesToResponsesReader(source, "grok"))
	if err != nil {
		t.Fatal(err)
	}
	events := grokResponseSSEEvents(stream)
	wantTypes := []string{
		"response.created",
		"response.output_item.added", "response.content_part.added", "response.output_text.delta",
		"response.output_text.done", "response.content_part.done",
		"response.content_part.added", "response.output_text.delta",
		"response.output_text.done", "response.content_part.done", "response.output_item.done",
		"response.output_item.added", "response.reasoning_summary_part.added",
		"response.reasoning_summary_text.delta", "response.reasoning.encrypted_content.delta",
		"response.reasoning_summary_text.done", "response.reasoning_summary_part.done",
		"response.reasoning.encrypted_content.done", "response.output_item.done",
		"response.completed",
	}
	if len(events) != len(wantTypes) {
		t.Fatalf("event count = %d, want %d; stream=%s", len(events), len(wantTypes), stream)
	}
	for i, want := range wantTypes {
		if got := events[i].Get("type").String(); got != want {
			t.Fatalf("event[%d].type = %q, want %q; stream=%s", i, got, want, stream)
		}
	}

	messageID := events[1].Get("item.id").String()
	if messageID == "" || events[2].Get("item_id").String() != messageID || events[6].Get("item_id").String() != messageID || events[10].Get("item.id").String() != messageID {
		t.Fatalf("message item identity changed across lifecycle: %s", stream)
	}
	if got := events[2].Get("content_index").Int(); got != 0 {
		t.Fatalf("first text content_index = %d, want 0", got)
	}
	if got := events[6].Get("content_index").Int(); got != 1 {
		t.Fatalf("second text content_index = %d, want 1", got)
	}
	for _, index := range []int{2, 3, 4, 5} {
		if got := events[index].Get("content_index").Int(); got != 0 {
			t.Fatalf("event[%d] first part content_index = %d, want 0", index, got)
		}
	}
	for _, index := range []int{6, 7, 8, 9} {
		if got := events[index].Get("content_index").Int(); got != 1 {
			t.Fatalf("event[%d] second part content_index = %d, want 1", index, got)
		}
	}
	for _, index := range []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10} {
		if got := events[index].Get("output_index").Int(); got != 0 {
			t.Fatalf("event[%d] message output_index = %d, want 0", index, got)
		}
	}

	reasoningID := events[11].Get("item.id").String()
	if reasoningID == "" || events[12].Get("item_id").String() != reasoningID || events[14].Get("item_id").String() != reasoningID || events[18].Get("item.id").String() != reasoningID {
		t.Fatalf("reasoning item identity changed across lifecycle: %s", stream)
	}
	for _, index := range []int{11, 12, 13, 14, 15, 16, 17, 18} {
		if got := events[index].Get("output_index").Int(); got != 1 {
			t.Fatalf("event[%d] reasoning output_index = %d, want 1", index, got)
		}
	}
	completed := completedGrokResponseEvent(t, stream)
	if got := gjson.GetBytes(completed, "response.output.0.id").String(); got != messageID {
		t.Fatalf("terminal message id = %q, want %q", got, messageID)
	}
	if got := gjson.GetBytes(completed, "response.output.0.content.0.text").String(); got != "A" {
		t.Fatalf("terminal first text = %q, want A", got)
	}
	if got := gjson.GetBytes(completed, "response.output.0.content.1.text").String(); got != "B" {
		t.Fatalf("terminal second text = %q, want B", got)
	}
	if got := gjson.GetBytes(completed, "response.output.1.id").String(); got != reasoningID {
		t.Fatalf("terminal reasoning id = %q, want %q", got, reasoningID)
	}
	if got := gjson.GetBytes(completed, "response.output.1.encrypted_content").String(); got != "ENC" {
		t.Fatalf("terminal encrypted_content = %q, want ENC", got)
	}
}

func TestMessagesAdapterPreservesToolStartInputAndEmptyObjectPlaceholder(t *testing.T) {
	source := io.NopCloser(bytes.NewBufferString(
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"m-tools\",\"model\":\"grok\",\"usage\":{\"input_tokens\":1}}}\n\n" +
			"data: {\"type\":\"content_block_start\",\"index\":9,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call9\",\"name\":\"nine\",\"input\":{}}}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":9,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"x\\\":\"}}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":9,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"1}\"}}\n\n" +
			"data: {\"type\":\"content_block_stop\",\"index\":9}\n\n" +
			"data: {\"type\":\"content_block_start\",\"index\":2,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call2\",\"name\":\"two\",\"input\":{\"ready\":true}}}\n\n" +
			"data: {\"type\":\"content_block_stop\",\"index\":2}\n\n" +
			"data: {\"type\":\"content_block_start\",\"index\":4,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call4\",\"name\":\"four\",\"input\":{\"stale\":true}}}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":4,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"fresh\\\":true}\"}}\n\n" +
			"data: {\"type\":\"content_block_stop\",\"index\":4}\n\n" +
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":2}}\n\n" +
			"data: {\"type\":\"message_stop\"}\n\n"))
	stream, err := io.ReadAll(newMessagesToResponsesReader(source, "grok"))
	if err != nil {
		t.Fatal(err)
	}
	var deltas []gjson.Result
	for _, event := range grokResponseSSEEvents(stream) {
		if event.Get("type").String() == "response.function_call_arguments.delta" {
			deltas = append(deltas, event)
		}
	}
	if len(deltas) != 4 {
		t.Fatalf("argument delta count = %d, want 4; stream=%s", len(deltas), stream)
	}
	if got := deltas[0].Get("delta").String() + deltas[1].Get("delta").String(); got != `{"x":1}` {
		t.Fatalf("placeholder tool arguments = %q, want exact JSON; stream=%s", got, stream)
	}
	if got := deltas[2].Get("delta").String(); got != `{"ready":true}` {
		t.Fatalf("start-only tool arguments delta = %q, want exact JSON", got)
	}
	if got := deltas[3].Get("delta").String(); got != `{"fresh":true}` {
		t.Fatalf("delta did not replace provisional start input: %q", got)
	}
	if bytes.Contains(stream, []byte(`{}{"x":1}`)) {
		t.Fatalf("empty input placeholder was prefixed to streamed arguments: %s", stream)
	}
	completed := completedGrokResponseEvent(t, stream)
	output := gjson.GetBytes(completed, "response.output").Array()
	if len(output) != 3 || output[0].Get("call_id").String() != "call9" || output[1].Get("call_id").String() != "call2" || output[2].Get("call_id").String() != "call4" {
		t.Fatalf("sparse tool output order changed: %s", completed)
	}
	if got := output[0].Get("arguments").String(); got != `{"x":1}` {
		t.Fatalf("streamed tool arguments = %q, want exact JSON", got)
	}
	if got := output[1].Get("arguments").String(); got != `{"ready":true}` {
		t.Fatalf("start-only tool arguments = %q, want exact JSON", got)
	}
	if got := output[2].Get("arguments").String(); got != `{"fresh":true}` {
		t.Fatalf("delta-replaced tool arguments = %q, want exact JSON", got)
	}
}

func TestMessagesAdapterPreservesRedactedThinkingLifecycle(t *testing.T) {
	source := io.NopCloser(bytes.NewBufferString(
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"m-redacted\",\"model\":\"grok\"}}\n\n" +
			"data: {\"type\":\"content_block_start\",\"index\":5,\"content_block\":{\"type\":\"redacted_thinking\",\"data\":\"REDACTED\"}}\n\n" +
			"data: {\"type\":\"content_block_stop\",\"index\":5}\n\n" +
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
			"data: {\"type\":\"message_stop\"}\n\n"))
	stream, err := io.ReadAll(newMessagesToResponsesReader(source, "grok"))
	if err != nil {
		t.Fatal(err)
	}
	events := grokResponseSSEEvents(stream)
	if len(events) != 6 {
		t.Fatalf("redacted event count = %d, want 6; stream=%s", len(events), stream)
	}
	wantTypes := []string{
		"response.created", "response.output_item.added", "response.reasoning.encrypted_content.delta",
		"response.reasoning.encrypted_content.done", "response.output_item.done", "response.completed",
	}
	for i, want := range wantTypes {
		if got := events[i].Get("type").String(); got != want {
			t.Fatalf("event[%d].type = %q, want %q; stream=%s", i, got, want, stream)
		}
	}
	itemID := events[1].Get("item.id").String()
	if itemID == "" || events[2].Get("item_id").String() != itemID || events[4].Get("item.id").String() != itemID {
		t.Fatalf("redacted reasoning identity changed: %s", stream)
	}
	completed := completedGrokResponseEvent(t, stream)
	if got := gjson.GetBytes(completed, "response.output.0.encrypted_content").String(); got != "REDACTED" {
		t.Fatalf("redacted encrypted_content = %q, want REDACTED", got)
	}
}

func TestMessagesAdapterDoesNotFabricateDoneEventsOnFailure(t *testing.T) {
	tests := []struct {
		name   string
		suffix string
	}{
		{
			name:   "explicit error",
			suffix: "data: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}\n\n",
		},
		{name: "unexpected eof"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := io.NopCloser(bytes.NewBufferString(
				"data: {\"type\":\"message_start\",\"message\":{\"id\":\"m-failed\",\"model\":\"grok\"}}\n\n" +
					"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
					"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n" +
					test.suffix))
			stream, err := io.ReadAll(newMessagesToResponsesReader(source, "grok"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Contains(stream, []byte(`"type":"response.failed"`)) {
				t.Fatalf("failure terminal missing: %s", stream)
			}
			for _, eventType := range []string{
				"response.output_text.done", "response.content_part.done", "response.output_item.done",
			} {
				if bytes.Contains(stream, []byte(`"type":"`+eventType+`"`)) {
					t.Fatalf("%s was fabricated on failure: %s", eventType, stream)
				}
			}
		})
	}
}

func TestMessagesAdapterBalancesInterleavedOutputItemLifecycles(t *testing.T) {
	source := io.NopCloser(bytes.NewBufferString(
		"data: {\"type\":\"message_start\",\"message\":{\"id\":\"m-interleaved\",\"model\":\"grok\"}}\n\n" +
			"data: {\"type\":\"content_block_start\",\"index\":8,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":8,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"R\"}}\n\n" +
			"data: {\"type\":\"content_block_stop\",\"index\":8}\n\n" +
			"data: {\"type\":\"content_block_start\",\"index\":4,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":4,\"delta\":{\"type\":\"text_delta\",\"text\":\"T\"}}\n\n" +
			"data: {\"type\":\"content_block_stop\",\"index\":4}\n\n" +
			"data: {\"type\":\"content_block_start\",\"index\":9,\"content_block\":{\"type\":\"tool_use\",\"id\":\"call9\",\"name\":\"nine\",\"input\":{\"q\":1}}}\n\n" +
			"data: {\"type\":\"content_block_stop\",\"index\":9}\n\n" +
			"data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
			"data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"U\"}}\n\n" +
			"data: {\"type\":\"content_block_stop\",\"index\":1}\n\n" +
			"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n" +
			"data: {\"type\":\"message_stop\"}\n\n"))
	stream, err := io.ReadAll(newMessagesToResponsesReader(source, "grok"))
	if err != nil {
		t.Fatal(err)
	}
	var added, done []string
	for _, event := range grokResponseSSEEvents(stream) {
		switch event.Get("type").String() {
		case "response.output_item.added":
			added = append(added, event.Get("item.id").String())
		case "response.output_item.done":
			done = append(done, event.Get("item.id").String())
		}
	}
	if len(added) != 4 || len(done) != 4 {
		t.Fatalf("output lifecycle counts added=%d done=%d, want 4/4; stream=%s", len(added), len(done), stream)
	}
	for i := range added {
		if added[i] == "" || done[i] != added[i] {
			t.Fatalf("output lifecycle[%d] added=%q done=%q; stream=%s", i, added[i], done[i], stream)
		}
	}
	completed := completedGrokResponseEvent(t, stream)
	output := gjson.GetBytes(completed, "response.output").Array()
	if len(output) != len(added) {
		t.Fatalf("terminal output count = %d, want %d; event=%s", len(output), len(added), completed)
	}
	wantTypes := []string{"reasoning", "message", "function_call", "message"}
	for i := range output {
		if got := output[i].Get("id").String(); got != added[i] {
			t.Fatalf("terminal output[%d].id = %q, want announced %q", i, got, added[i])
		}
		if got := output[i].Get("type").String(); got != wantTypes[i] {
			t.Fatalf("terminal output[%d].type = %q, want %q", i, got, wantTypes[i])
		}
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

func TestChatAdapterDoesNotCompleteTruncatedToolInput(t *testing.T) {
	source := io.NopCloser(bytes.NewBufferString(
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_partial\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"query\\\":\"}}]},\"finish_reason\":null}]}\n\n" +
			"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"length\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n"))
	stream, err := io.ReadAll(newChatToResponsesReader(source, "grok"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(stream, []byte(`"status":"incomplete"`)) {
		t.Fatalf("truncated response did not remain incomplete: %s", stream)
	}
	if bytes.Contains(stream, []byte(`"type":"response.function_call_arguments.done"`)) || bytes.Contains(stream, []byte(`"type":"response.output_item.done"`)) {
		t.Fatalf("truncated tool input was marked complete: %s", stream)
	}
	completed := completedGrokResponseEvent(t, stream)
	if got := gjson.GetBytes(completed, "response.output.0.status").String(); got != "incomplete" {
		t.Fatalf("truncated tool item status = %q, want incomplete: %s", got, completed)
	}
}

func completedGrokResponseEvent(t *testing.T, stream []byte) []byte {
	t.Helper()
	for _, event := range grokResponseSSEEvents(stream) {
		eventType := event.Get("type").String()
		if eventType == "response.completed" || eventType == "response.incomplete" {
			return []byte(event.Raw)
		}
	}
	t.Fatalf("response terminal missing from stream: %s", stream)
	return nil
}

func grokResponseSSEEvents(stream []byte) []gjson.Result {
	var events []gjson.Result
	for _, frame := range bytes.Split(stream, []byte("\n\n")) {
		payload := bytes.TrimSpace(frame)
		payload = bytes.TrimSpace(bytes.TrimPrefix(payload, []byte("data:")))
		if !gjson.ValidBytes(payload) {
			continue
		}
		events = append(events, gjson.ParseBytes(append([]byte(nil), payload...)))
	}
	return events
}

func TestGrokSyntheticResponsesUseConsistentUnixCreatedAt(t *testing.T) {
	tests := []struct {
		name          string
		terminalEvent string
		reader        func() io.ReadCloser
	}{
		{
			name:          "chat completed",
			terminalEvent: "response.completed",
			reader: func() io.ReadCloser {
				return newChatToResponsesReader(io.NopCloser(strings.NewReader(
					"data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n"+
						"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n")), "grok")
			},
		},
		{
			name:          "chat failed",
			terminalEvent: "response.failed",
			reader: func() io.ReadCloser {
				return newChatToResponsesReader(io.NopCloser(strings.NewReader(
					"data: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}\n\n")), "grok")
			},
		},
		{
			name:          "messages completed",
			terminalEvent: "response.completed",
			reader: func() io.ReadCloser {
				return newMessagesToResponsesReader(io.NopCloser(strings.NewReader(
					"data: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"model\":\"grok\",\"usage\":{\"input_tokens\":1}}}\n\n"+
						"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n"+
						"data: {\"type\":\"message_stop\"}\n\n")), "grok")
			},
		},
		{
			name:          "messages failed",
			terminalEvent: "response.failed",
			reader: func() io.ReadCloser {
				return newMessagesToResponsesReader(io.NopCloser(strings.NewReader(
					"data: {\"type\":\"message_start\",\"message\":{\"id\":\"m1\",\"model\":\"grok\",\"usage\":{\"input_tokens\":1}}}\n\n"+
						"data: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":\"busy\"}}\n\n")), "grok")
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stream, err := io.ReadAll(test.reader())
			if err != nil {
				t.Fatal(err)
			}
			assertSyntheticResponseCreatedAt(t, stream, test.terminalEvent)
		})
	}
}

func assertSyntheticResponseCreatedAt(t *testing.T, stream []byte, terminalEvent string) {
	t.Helper()
	var createdAt, terminalAt gjson.Result
	for _, frame := range bytes.Split(stream, []byte("\n\n")) {
		for _, line := range bytes.Split(frame, []byte("\n")) {
			line = bytes.TrimSpace(line)
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
			if !gjson.ValidBytes(payload) {
				continue
			}
			event := gjson.ParseBytes(payload)
			switch event.Get("type").String() {
			case "response.created":
				createdAt = event.Get("response.created_at")
			case terminalEvent:
				terminalAt = event.Get("response.created_at")
			}
		}
	}
	if createdAt.Type != gjson.Number || terminalAt.Type != gjson.Number {
		t.Fatalf("created_at missing from created/terminal events: %s", stream)
	}
	now := time.Now().Unix()
	if createdAt.Int() <= 0 || createdAt.Int() > now || createdAt.Float() != float64(createdAt.Int()) {
		t.Fatalf("created_at = %s, want Unix seconds", createdAt.Raw)
	}
	if terminalAt.Int() != createdAt.Int() {
		t.Fatalf("created_at changed within one response: created=%s terminal=%s", createdAt.Raw, terminalAt.Raw)
	}
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
