package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

const antigravityDeferredToolsFixture = `{
	"input":[
		{"role":"user","content":"Run a synthetic independent review."},
		{"type":"additional_tools","tools":[
			{"type":"namespace","name":"functions","tools":[
				{"type":"custom","name":"apply_patch","description":"Apply a synthetic patch."}
			]}
		]}
	],
	"tools":[
		{"type":"tool_search","execution":"client"},
		{"type":"namespace","name":"multi_agent_v1","tools":[
			{"type":"function","name":"spawn_agent","input_schema":{
				"type":"object","properties":{"message":{"type":"string"},"model":{"type":"string"}},
				"required":["message","model"]
			}}
		]},
		{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"query":{"type":"string"}}}}
	]
}`

func TestAntigravityDeferredToolsDeclarations(t *testing.T) {
	t.Setenv(antigravityFunctionToolsEnv, "true")
	got, err := responsesToGeminiInternal([]byte(antigravityDeferredToolsFixture), "synthetic-project", "gemini-3-flash-agent")
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	declarations := gjson.GetBytes(encoded, "request.tools.0.functionDeclarations")
	for _, want := range []struct{ name, property, kind string }{
		{"codex_tool_search", "query", "string"},
		{"multi_agent_v1__spawn_agent", "model", "string"},
		{"functions__apply_patch", "input", "string"},
		{"lookup", "query", "string"},
	} {
		declaration := declarations.Get(`#(name=="` + want.name + `")`)
		if actual := declaration.Get("parametersJsonSchema.properties." + want.property + ".type").String(); actual != want.kind {
			t.Errorf("%s.%s type = %q, want %s; declarations=%s", want.name, want.property, actual, want.kind, declarations)
		}
	}
	if declarations.Get("#").Int() != 4 {
		t.Fatalf("declarations = %s, want exactly four callable tools", declarations)
	}
}

func TestAntigravityDeferredToolsHistory(t *testing.T) {
	t.Setenv(antigravityFunctionToolsEnv, "true")
	got, err := responsesToGeminiInternal([]byte(`{
		"tools":[{"type":"tool_search","execution":"client"}],
		"input":[
			{"role":"user","content":"Synthetic expert review."},
			{"type":"tool_search_call","call_id":"search_1","execution":"client","arguments":{"query":"spawn_agent","limit":1}},
			{"type":"tool_search_output","call_id":"search_1","tools":[
				{"type":"namespace","name":"multi_agent_v1","tools":[
					{"type":"function","name":"spawn_agent","parameters":{"type":"object","properties":{"model":{"type":"string"}}}}
				]}
			]},
			{"type":"function_call","namespace":"multi_agent_v1","name":"spawn_agent","call_id":"spawn_1","arguments":"{\"model\":\"gpt-6.1-sol\"}"},
			{"type":"function_call_output","call_id":"spawn_1","output":"{\"agent_id\":\"synthetic-child\"}"},
			{"type":"custom_tool_call","namespace":"functions","name":"apply_patch","call_id":"patch_1","input":"synthetic patch"},
			{"type":"custom_tool_call_output","call_id":"patch_1","output":"synthetic result"}
		]
	}`), "synthetic-project", "gemini-3-flash-agent")
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(got)
	contents := gjson.GetBytes(encoded, "request.contents")
	for _, want := range []struct{ path, value string }{
		{"1.parts.0.functionCall.name", "codex_tool_search"},
		{"1.parts.0.functionCall.args.query", "spawn_agent"},
		{"2.parts.0.functionResponse.name", "codex_tool_search"},
		{"3.parts.0.functionCall.name", "multi_agent_v1__spawn_agent"},
		{"4.parts.0.functionResponse.name", "multi_agent_v1__spawn_agent"},
		{"5.parts.0.functionCall.name", "functions__apply_patch"},
		{"5.parts.0.functionCall.args.input", "synthetic patch"},
	} {
		if actual := contents.Get(want.path).String(); actual != want.value {
			t.Errorf("%s = %q, want %q; contents=%s", want.path, actual, want.value, contents)
		}
	}
	if !strings.Contains(gjson.GetBytes(encoded, "request.tools").Raw, "multi_agent_v1__spawn_agent") {
		t.Fatalf("search-loaded tool not declared: %s", encoded)
	}
}

func TestAntigravityDeferredToolsTransportRoundTrip(t *testing.T) {
	for _, mode := range []struct {
		name           string
		upstreamStream bool
		sseOut         bool
	}{
		{"json", false, false},
		{"stream", true, true},
		{"buffered", false, true},
	} {
		t.Run(mode.name, func(t *testing.T) {
			t.Setenv(antigravityFunctionToolsEnv, "true")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var envelope map[string]any
				if err := json.NewDecoder(r.Body).Decode(&envelope); err != nil {
					t.Errorf("decode synthetic request: %v", err)
				}
				raw, _ := json.Marshal(envelope)
				if gjson.GetBytes(raw, `request.tools.0.functionDeclarations.#(name=="codex_tool_search")`).Exists() == false {
					t.Error("tool search entrypoint was dropped before upstream dispatch")
				}
				candidate := `{"candidates":[{"content":{"parts":[
					{"functionCall":{"name":"codex_tool_search","id":"search_1","args":{"query":"spawn_agent","limit":1}}},
					{"functionCall":{"name":"multi_agent_v1__spawn_agent","id":"spawn_1","args":{"message":"synthetic","model":"gpt-6.1-sol"}}},
					{"functionCall":{"name":"functions__apply_patch","id":"patch_1","args":{"input":"synthetic patch"}}},
					{"functionCall":{"name":"lookup","id":"lookup_1","args":{"query":"synthetic"}}}
				]},"finishReason":"STOP"}]}`
				if mode.upstreamStream {
					w.Header().Set("Content-Type", "text/event-stream")
					var compact any
					_ = json.Unmarshal([]byte(candidate), &compact)
					line, _ := json.Marshal(compact)
					_, _ = io.WriteString(w, "data: "+string(line)+"\n\n")
				} else {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, candidate)
				}
			}))
			defer server.Close()
			previous := antigravityOAuthEndpointBases
			antigravityOAuthEndpointBases = []string{server.URL}
			t.Cleanup(func() { antigravityOAuthEndpointBases = previous })
			account := &auth.Account{DBID: 99021, UpstreamType: auth.UpstreamAntigravity, AccessToken: "synthetic-token", AntigravityProjectID: "synthetic-project"}
			var resp *http.Response
			var err error
			if !mode.upstreamStream && mode.sseOut {
				resp, err = ExecuteAntigravityResponsesRequestBuffered(context.Background(), account, "gemini-3-flash-agent", []byte(antigravityDeferredToolsFixture), "")
			} else {
				resp, err = ExecuteAntigravityResponsesRequest(context.Background(), account, "gemini-3-flash-agent", []byte(antigravityDeferredToolsFixture), mode.sseOut, "")
			}
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			raw, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatal(err)
			}
			terminal := gjson.ParseBytes(raw)
			if mode.sseOut {
				terminal = gjson.Result{}
				added := 0
				for _, line := range strings.Split(string(raw), "\n") {
					if !strings.HasPrefix(line, "data:") {
						continue
					}
					event := gjson.Parse(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
					switch event.Get("type").String() {
					case "response.output_item.added", "response.output_item.done":
						item := event.Get("item")
						switch item.Get("call_id").String() {
						case "search_1":
							assertAntigravitySearchItem(t, item, event.Get("type").String() == "response.output_item.done")
						case "spawn_1":
							if item.Get("namespace").String() != "multi_agent_v1" || item.Get("name").String() != "spawn_agent" {
								t.Errorf("namespace not restored: %s", item)
							}
						}
						if event.Get("type").String() == "response.output_item.added" {
							added++
						}
					case "response.function_call_arguments.delta", "response.function_call_arguments.done":
						if callID := event.Get("call_id").String(); callID == "search_1" || callID == "patch_1" {
							t.Errorf("bridged call leaked ordinary argument event: %s", event)
						}
					case "response.completed":
						terminal = event.Get("response")
					}
				}
				if added != 4 {
					t.Fatalf("added items = %d, want 4; SSE=%s", added, raw)
				}
			}
			output := terminal.Get("output")
			assertAntigravitySearchItem(t, output.Get("0"), true)
			if item := output.Get("1"); item.Get("type").String() != "function_call" || item.Get("namespace").String() != "multi_agent_v1" || item.Get("name").String() != "spawn_agent" {
				t.Errorf("spawn identity not restored: %s", item)
			}
			if item := output.Get("2"); item.Get("type").String() != "custom_tool_call" || item.Get("namespace").String() != "functions" || item.Get("input").String() != "synthetic patch" {
				t.Errorf("custom call not restored: %s", item)
			}
			if item := output.Get("3"); item.Get("type").String() != "function_call" || item.Get("name").String() != "lookup" || item.Get("namespace").Exists() {
				t.Errorf("ordinary function changed: %s", item)
			}
		})
	}
}

func TestAntigravityDeferredToolsNestedCustom(t *testing.T) {
	t.Setenv(antigravityFunctionToolsEnv, "true")
	body := []byte(`{
		"tools":[{"type":"custom","custom":{"name":"apply_patch","description":"Synthetic nested tool"}}],
		"input":"Synthetic custom tool check."
	}`)
	got, err := responsesToGeminiInternal(body, "synthetic-project", "gemini-3-flash-agent")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(got)
	declaration := gjson.GetBytes(raw, `request.tools.0.functionDeclarations.#(name=="apply_patch")`)
	if declaration.Get("description").String() != "Synthetic nested tool" ||
		declaration.Get("parametersJsonSchema.properties.input.type").String() != "string" {
		t.Fatalf("nested custom declaration lost: %s", declaration)
	}
	_, aliases := prepareAntigravityResponsesTools(body)
	restored := reverseGrokNamespaceJSON([]byte(`{"output":[{"type":"function_call","name":"apply_patch","arguments":"{\"input\":\"synthetic patch\"}"}]}`), aliases)
	if gjson.GetBytes(restored, "output.0.type").String() != "custom_tool_call" ||
		gjson.GetBytes(restored, "output.0.input").String() != "synthetic patch" {
		t.Fatalf("nested custom identity not restored: %s", restored)
	}
}

func assertAntigravitySearchItem(t *testing.T, item gjson.Result, completed bool) {
	t.Helper()
	if item.Get("type").String() != "tool_search_call" || item.Get("execution").String() != "client" || !strings.HasPrefix(item.Get("id").String(), "tsc_") || item.Get("name").Exists() || !item.Get("arguments").IsObject() {
		t.Errorf("invalid client tool-search item: %s", item)
	}
	if completed && (!item.Get("arguments").IsObject() || item.Get("arguments.query").String() != "spawn_agent") {
		t.Errorf("tool-search arguments not restored to an object: %s", item)
	}
}

func TestAntigravityDeferredToolsStreamPreservesDistinctCalls(t *testing.T) {
	for _, withIDs := range []bool{true, false} {
		name := "generated_ids"
		if withIDs {
			name = "upstream_ids"
		}
		t.Run(name, func(t *testing.T) {
			calls := make([]any, 0, 2)
			for _, id := range []string{"spawn_1", "spawn_2"} {
				call := map[string]any{
					"name": "multi_agent_v1__spawn_agent",
					"args": map[string]any{"message": "synthetic", "model": "gpt-6.1-sol"},
				}
				if withIDs {
					call["id"] = id
				}
				calls = append(calls, map[string]any{"functionCall": call})
			}
			frame := func(parts []any, finish string) string {
				encoded, err := json.Marshal(map[string]any{
					"candidates": []any{map[string]any{
						"content": map[string]any{"parts": parts}, "finishReason": finish,
					}},
				})
				if err != nil {
					t.Fatal(err)
				}
				return "data: " + string(encoded) + "\n\n"
			}
			input := frame(calls, "")
			if withIDs {
				// A later update to the same upstream ID must not add a third call.
				input += frame(calls[:1], "STOP")
			} else {
				input += frame(nil, "STOP")
			}
			_, aliases := prepareAntigravityResponsesTools([]byte(antigravityDeferredToolsFixture))
			body := newGrokNamespaceReverser(newAntigravitySSEResponseBody(io.NopCloser(strings.NewReader(input))), true, aliases)
			defer body.Close()
			raw, err := io.ReadAll(body)
			if err != nil {
				t.Fatal(err)
			}
			var output gjson.Result
			for _, line := range strings.Split(string(raw), "\n") {
				if strings.HasPrefix(line, "data:") {
					event := gjson.Parse(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
					if event.Get("type").String() == "response.completed" {
						output = event.Get("response.output")
					}
				}
			}
			if len(output.Array()) != 2 {
				t.Fatalf("distinct calls = %d, want 2; SSE=%s", len(output.Array()), raw)
			}
			first, second := output.Get("0"), output.Get("1")
			if first.Get("call_id").String() == second.Get("call_id").String() {
				t.Fatal("distinct calls must retain distinct IDs")
			}
			for _, item := range output.Array() {
				if item.Get("namespace").String() != "multi_agent_v1" || item.Get("name").String() != "spawn_agent" {
					t.Fatalf("namespace identity not restored: %s", item)
				}
			}
		})
	}
}

func TestAntigravityDeferredToolsAliasCollisions(t *testing.T) {
	body := []byte(`{
		"tools":[
			{"type":"namespace","name":"multi_agent_v1","tools":[{"type":"function","name":"spawn_agent","parameters":{"type":"object"}}]},
			{"type":"function","name":"multi_agent_v1__spawn_agent","parameters":{"type":"object"}}
		],
		"input":[
			{"role":"user","content":"Synthetic collision check."},
			{"type":"function_call","name":"multi_agent_v1__spawn_agent","call_id":"plain","arguments":"{}"},
			{"type":"function_call_output","call_id":"plain","output":"synthetic"}
		]
	}`)
	normalized, aliases := prepareAntigravityResponsesTools(body)
	tools := gjson.GetBytes(normalized, "tools")
	if tools.Get("#").Int() != 2 {
		t.Fatalf("colliding declaration lost: %s", normalized)
	}
	plainName := tools.Get("1.name").String()
	if got := gjson.GetBytes(normalized, "input.1.name").String(); got != plainName {
		t.Fatalf("history name = %q, want declaration %q", got, plainName)
	}
	response := []byte(`{"output":[{"type":"function_call","name":"` + plainName + `","arguments":"{}"}]}`)
	restored := reverseGrokNamespaceJSON(response, aliases)
	if gjson.GetBytes(restored, "output.0.name").String() != "multi_agent_v1__spawn_agent" || gjson.GetBytes(restored, "output.0.namespace").Exists() {
		t.Fatalf("colliding plain function identity changed: %s", restored)
	}
}

func TestAntigravityDeferredToolsLongNames(t *testing.T) {
	namespace := strings.Repeat("mcp_namespace_", 5)
	first, second := strings.Repeat("long_tool_", 8)+"first", strings.Repeat("long_tool_", 8)+"second"
	payload := map[string]any{
		"tools": []any{map[string]any{"type": "namespace", "name": namespace, "tools": []any{
			map[string]any{"type": "function", "name": first},
			map[string]any{"type": "function", "name": second},
		}}},
		"input": []any{
			map[string]any{"role": "user", "content": "Synthetic long tool names."},
			map[string]any{"type": "function_call", "namespace": namespace, "name": second, "call_id": "long", "arguments": "{}"},
		},
	}
	body, _ := json.Marshal(payload)
	normalized, aliases := prepareAntigravityResponsesTools(body)
	tools := gjson.GetBytes(normalized, "tools")
	one, two := tools.Get("0.name").String(), tools.Get("1.name").String()
	if one == two || len(one) > 64 || len(two) > 64 || one == "" || two == "" {
		t.Fatalf("invalid Gemini names: %q, %q", one, two)
	}
	if gjson.GetBytes(normalized, "input.1.name").String() != two {
		t.Fatalf("long history name does not match declaration: %s", normalized)
	}
	response := []byte(`{"output":[{"type":"function_call","name":"` + two + `","arguments":"{}"}]}`)
	restored := reverseGrokNamespaceJSON(response, aliases)
	if gjson.GetBytes(restored, "output.0.namespace").String() != namespace || gjson.GetBytes(restored, "output.0.name").String() != second {
		t.Fatalf("long identity not restored: %s", restored)
	}
}

func TestAntigravityDeferredToolsChoice(t *testing.T) {
	t.Setenv(antigravityFunctionToolsEnv, "true")
	for _, test := range []struct {
		choice map[string]any
		name   string
	}{
		{map[string]any{"type": "tool_search"}, "codex_tool_search"},
		{map[string]any{"type": "function", "namespace": "multi_agent_v1", "name": "spawn_agent"}, "multi_agent_v1__spawn_agent"},
	} {
		var request map[string]any
		if err := json.Unmarshal([]byte(antigravityDeferredToolsFixture), &request); err != nil {
			t.Fatal(err)
		}
		request["tool_choice"] = test.choice
		body, err := json.Marshal(request)
		if err != nil {
			t.Fatal(err)
		}
		got, err := responsesToGeminiInternal(body, "synthetic-project", "gemini-3-flash-agent")
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(got)
		config := gjson.GetBytes(raw, "request.toolConfig.functionCallingConfig")
		if config.Get("mode").String() != "ANY" || config.Get("allowedFunctionNames.0").String() != test.name {
			t.Errorf("forced choice not preserved for %s: %s", test.name, config)
		}
	}
}
