package proxy

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestGrokCustomToolCompatibilityMatrix(t *testing.T) {
	body := []byte(`{
		"model":"grok-4.6",
		"tools":[
			{"type":"custom","name":"apply_patch","description":"patch files","format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}},
			{"type":"namespace","name":"computer","tools":[{"type":"custom","name":"operate","description":"use computer"}]}
		],
		"tool_choice":{"type":"custom","name":"operate","namespace":"computer"},
		"input":[
			{"type":"custom_tool_call","call_id":"call_1","name":"apply_patch","input":"*** Begin Patch"},
			{"type":"custom_tool_call_output","call_id":"call_1","output":null}
		]
	}`)

	result := prepareGrokUpstreamBody(body)
	if strings.Contains(string(result.Body), `"type":"custom"`) || strings.Contains(string(result.Body), `"type":"namespace"`) {
		t.Fatalf("Codex-only tool variants leaked upstream: %s", result.Body)
	}
	if got := gjson.GetBytes(result.Body, "tools.0.type").String(); got != "function" {
		t.Fatalf("top-level custom type = %q", got)
	}
	if got := gjson.GetBytes(result.Body, "tools.0.parameters.required.0").String(); got != "input" {
		t.Fatalf("custom wrapper schema did not require input: %s", result.Body)
	}
	if got := gjson.GetBytes(result.Body, "tool_choice.type").String(); got != "function" {
		t.Fatalf("custom tool_choice type = %q", got)
	}
	choiceName := gjson.GetBytes(result.Body, "tool_choice.name").String()
	identity, ok := result.Aliases[choiceName]
	if !ok || !identity.Custom || identity.Namespace != "computer" || identity.Name != "operate" {
		t.Fatalf("custom tool_choice alias = %q %#v", choiceName, identity)
	}
	if got := gjson.GetBytes(result.Body, "input.0.type").String(); got != "function_call" {
		t.Fatalf("custom history call type = %q", got)
	}
	if got := gjson.GetBytes(result.Body, "input.0.arguments").String(); got != `{"input":"*** Begin Patch"}` {
		t.Fatalf("custom history arguments = %q", got)
	}
	if got := gjson.GetBytes(result.Body, "input.1.output").String(); got != "" {
		t.Fatalf("nil custom output = %q", got)
	}
}

func TestGrokCustomAndFunctionNameCollision(t *testing.T) {
	body := []byte(`{"tools":[{"type":"function","name":"same","parameters":{"type":"object"}},{"type":"custom","name":"same"}],"input":"x"}`)
	result := prepareGrokUpstreamBody(body)
	first := gjson.GetBytes(result.Body, "tools.0.name").String()
	second := gjson.GetBytes(result.Body, "tools.1.name").String()
	if first == second {
		t.Fatalf("function/custom collision reused alias %q: %s", first, result.Body)
	}
	if result.Aliases[first].Custom || !result.Aliases[second].Custom {
		t.Fatalf("alias identities lost function/custom distinction: %#v", result.Aliases)
	}
}

func TestGrokCustomResponseRestoration(t *testing.T) {
	aliases := map[string]grokNsIdentity{"apply_patch": {Name: "apply_patch", Custom: true}}
	nonStream := reverseGrokNamespaceJSON([]byte(`{"output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"apply_patch","arguments":"{\"input\":\"patch text\"}"}]}`), aliases)
	if got := gjson.GetBytes(nonStream, "output.0.type").String(); got != "custom_tool_call" {
		t.Fatalf("non-stream type = %q; body=%s", got, nonStream)
	}
	if got := gjson.GetBytes(nonStream, "output.0.input").String(); got != "patch text" {
		t.Fatalf("non-stream input = %q; body=%s", got, nonStream)
	}

	r := &grokStreamReverser{aliases: aliases, customItems: map[string]bool{}, toolSearchItems: map[string]bool{}, inputBytes: map[string]int{}}
	added := r.rewriteLine([]byte("data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_1\",\"name\":\"apply_patch\",\"arguments\":\"\"}}\n"))
	if got := gjson.GetBytes(bytesAfterSSEData(added), "item.type").String(); got != "custom_tool_call" {
		t.Fatalf("stream added type = %q; line=%s", got, added)
	}
	delta := r.rewriteLine([]byte("data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_1\",\"delta\":\"{\"}\n"))
	if delta != nil {
		t.Fatalf("custom function delta leaked: %s", delta)
	}
	done := r.rewriteLine([]byte("data: {\"type\":\"response.function_call_arguments.done\",\"item_id\":\"fc_1\",\"arguments\":\"{\\\"input\\\":\\\"patch text\\\"}\"}\n"))
	if got := gjson.GetBytes(bytesAfterSSEData(done), "type").String(); got != "response.custom_tool_call_input.done" {
		t.Fatalf("stream done type = %q; line=%s", got, done)
	}
	if got := gjson.GetBytes(bytesAfterSSEData(done), "input").String(); got != "patch text" {
		t.Fatalf("stream done input = %q; line=%s", got, done)
	}
}

func TestGrokAdditionalToolsAndToolSearchMatrix(t *testing.T) {
	body := []byte(`{
		"model":"grok-4.6",
		"input":[
			{"type":"additional_tools","tools":[{"type":"tool_search"},{"type":"namespace","name":"deferred","tools":[{"type":"custom","name":"run"}]}]},
			{"type":"message","role":"user","content":"find and run a tool"},
			{"type":"tool_search_call","call_id":"ts_1","execution":"client","arguments":{"query":"github","limit":3}},
			{"type":"tool_search_output","call_id":"ts_1","tools":[{"name":"github"}]}
		],
		"tool_choice":{"type":"tool_search"}
	}`)
	result := prepareGrokUpstreamBody(body)
	if gjson.GetBytes(result.Body, `input.#(type=="additional_tools")`).Exists() {
		t.Fatalf("additional_tools carrier leaked upstream: %s", result.Body)
	}
	if strings.Contains(string(result.Body), `"type":"tool_search"`) || strings.Contains(string(result.Body), `"type":"namespace"`) {
		t.Fatalf("Codex-only tool declaration leaked upstream: %s", result.Body)
	}
	proxyName := ""
	for alias, metadata := range result.Aliases {
		if metadata.ToolSearch {
			proxyName = alias
			break
		}
	}
	identity, ok := result.Aliases[proxyName]
	if !ok || !identity.ToolSearch {
		t.Fatalf("tool_search proxy identity missing: name=%q aliases=%#v body=%s", proxyName, result.Aliases, result.Body)
	}
	if got := gjson.GetBytes(result.Body, "input.1.type").String(); got != "function_call" {
		t.Fatalf("tool_search history call type = %q; body=%s", got, result.Body)
	}
	if got := gjson.GetBytes(result.Body, "input.2.type").String(); got != "function_call_output" {
		t.Fatalf("tool_search output type = %q; body=%s", got, result.Body)
	}
	if got := gjson.GetBytes(result.Body, "tool_choice.name").String(); got != proxyName {
		t.Fatalf("tool_search choice name = %q, want %q", got, proxyName)
	}

	nonStream := reverseGrokNamespaceJSON([]byte(`{"output":[{"type":"function_call","id":"fc_ts","call_id":"ts_2","name":"`+proxyName+`","arguments":"{\"query\":\"github\"}"}]}`), result.Aliases)
	if got := gjson.GetBytes(nonStream, "output.0.type").String(); got != "tool_search_call" {
		t.Fatalf("restored tool_search type = %q; body=%s", got, nonStream)
	}
	if got := gjson.GetBytes(nonStream, "output.0.arguments.query").String(); got != "github" {
		t.Fatalf("restored tool_search query = %q; body=%s", got, nonStream)
	}

	r := &grokStreamReverser{aliases: result.Aliases, customItems: map[string]bool{}, toolSearchItems: map[string]bool{}, inputBytes: map[string]int{}}
	added := r.rewriteLine([]byte("data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"id\":\"fc_ts\",\"call_id\":\"ts_2\",\"name\":\"" + proxyName + "\",\"arguments\":\"\"}}\n"))
	if got := gjson.GetBytes(bytesAfterSSEData(added), "item.type").String(); got != "tool_search_call" {
		t.Fatalf("stream tool_search added type = %q; line=%s", got, added)
	}
	if leaked := r.rewriteLine([]byte("data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_ts\",\"delta\":\"{}\"}\n")); leaked != nil {
		t.Fatalf("tool_search argument delta leaked: %s", leaked)
	}
	if leaked := r.rewriteLine([]byte("data: {\"type\":\"response.function_call_arguments.done\",\"item_id\":\"fc_ts\",\"arguments\":\"{}\"}\n")); leaked != nil {
		t.Fatalf("tool_search argument done leaked: %s", leaked)
	}
}

func TestGrokToolSearchOutputInjectsDynamicTools(t *testing.T) {
	body := []byte(`{
		"model":"grok-4.6",
		"tools":[
			{"type":"tool_search"},
			{"type":"namespace","name":"codex_app","tools":[{"type":"function","name":"load_workspace_dependencies","inputSchema":{"type":"object","properties":{},"additionalProperties":false},"deferLoading":true}]}
		],
		"input":[
			{"type":"tool_search_call","call_id":"ts_1","execution":"client","arguments":{"query":"workspace dependencies"}},
			{"type":"tool_search_output","call_id":"ts_1","tools":[
				{"type":"namespace","name":"codex_app","tools":[
					{"type":"function","name":"load_workspace_dependencies","inputSchema":{"type":"object","properties":{},"additionalProperties":false},"deferLoading":true},
					{"type":"custom","name":"dynamic_patch","description":"dynamic custom tool","defer_loading":true}
				]}
			]},
			{"type":"additional_tools","tools":[{"type":"namespace","name":"codex_app","tools":[{"type":"function","name":"load_workspace_dependencies","input_schema":{"type":"object","properties":{}}}]}]},
			{"type":"message","role":"user","content":"use the loaded tool"}
		]
	}`)
	result := prepareGrokUpstreamBody(body)
	if gjson.GetBytes(result.Body, `input.#(type=="additional_tools")`).Exists() {
		t.Fatalf("additional_tools leaked upstream: %s", result.Body)
	}
	if got := gjson.GetBytes(result.Body, "input.1.type").String(); got != "function_call_output" {
		t.Fatalf("tool_search output history type = %q; body=%s", got, result.Body)
	}
	loadedCount := 0
	customCount := 0
	for _, raw := range gjson.GetBytes(result.Body, "tools").Array() {
		name := raw.Get("name").String()
		identity := result.Aliases[name]
		if identity.Namespace == "codex_app" && identity.Name == "load_workspace_dependencies" {
			loadedCount++
			if raw.Get("parameters.type").String() != "object" {
				t.Fatalf("dynamic function inputSchema was not normalized: %s", raw.Raw)
			}
			for _, field := range []string{"inputSchema", "input_schema", "deferLoading", "defer_loading"} {
				if raw.Get(field).Exists() {
					t.Fatalf("dynamic function leaked %s: %s", field, raw.Raw)
				}
			}
		}
		if identity.Namespace == "codex_app" && identity.Name == "dynamic_patch" && identity.Custom {
			customCount++
		}
	}
	if loadedCount != 1 {
		t.Fatalf("loaded dynamic function count = %d, want 1; aliases=%#v body=%s", loadedCount, result.Aliases, result.Body)
	}
	if customCount != 1 {
		t.Fatalf("loaded dynamic custom count = %d, want 1; aliases=%#v body=%s", customCount, result.Aliases, result.Body)
	}
}

func TestGrokGiantCustomToolGuard(t *testing.T) {
	body := []byte(`{"model":"grok-4.6","tools":[{"type":"custom","name":"apply_patch"}],"input":"patch"}`)
	result := prepareGrokUpstreamBody(body)
	if !strings.Contains(gjson.GetBytes(result.Body, "instructions").String(), grokGiantToolInstructionMarker) {
		t.Fatalf("apply_patch guard instructions missing: %s", result.Body)
	}
	aliases := result.Aliases
	r := &grokStreamReverser{aliases: aliases, customItems: map[string]bool{}, toolSearchItems: map[string]bool{}, inputBytes: map[string]int{}}
	_ = r.rewriteLine([]byte("data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"id\":\"fc_big\",\"name\":\"apply_patch\"}}\n"))
	large := strings.Repeat("x", grokToolCallHardLimitBytes+1)
	line := []byte("data: " + string(mustJSON(t, map[string]any{"type": "response.function_call_arguments.delta", "item_id": "fc_big", "delta": large})) + "\n")
	failure := r.rewriteLine(line)
	if got := gjson.GetBytes(bytesAfterSSEData(failure), "type").String(); got != "response.failed" {
		t.Fatalf("oversized custom call did not fail: type=%q line=%s", got, failure)
	}
	if got := gjson.GetBytes(bytesAfterSSEData(failure), "response.error.code").String(); got != "invalid_prompt" {
		t.Fatalf("oversized custom error code = %q", got)
	}
}

func TestGrokGiantToolGuardIsCaseInsensitiveAndPreservesNumbers(t *testing.T) {
	body := []byte(`{"seed":9007199254740993,"tools":[{"type":"custom","name":"APPLY_PATCH"}],"input":"patch"}`)
	guarded, changed := addGrokGiantToolInstructions(body)
	if !changed {
		t.Fatal("case-insensitive apply_patch name did not enable guard")
	}
	if !strings.Contains(string(guarded), `"seed":9007199254740993`) {
		t.Fatalf("large integer changed during guard injection: %s", guarded)
	}
}

func TestGrokAdditionalToolsPreservesNumbers(t *testing.T) {
	body := []byte(`{"seed":9007199254740993,"input":[{"type":"additional_tools","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}]}`)
	lifted, changed := liftGrokAdditionalTools(body)
	if !changed {
		t.Fatal("additional_tools carrier was not lifted")
	}
	if !strings.Contains(string(lifted), `"seed":9007199254740993`) {
		t.Fatalf("large integer changed while lifting tools: %s", lifted)
	}
}

func TestGrokPlainFunctionDoneBypassesBridgedSizeLimit(t *testing.T) {
	r := &grokStreamReverser{aliases: map[string]grokNsIdentity{}, customItems: map[string]bool{}, toolSearchItems: map[string]bool{}, inputBytes: map[string]int{}}
	arguments := strings.Repeat("x", grokToolCallHardLimitBytes+1)
	line := []byte("data: " + string(mustJSON(t, map[string]any{"type": "response.function_call_arguments.done", "item_id": "plain", "arguments": arguments})) + "\n")
	got := r.rewriteLine(line)
	if string(got) != string(line) {
		t.Fatalf("plain function call was rewritten or rejected: %s", got)
	}
}

func TestGrokStreamFastPathIncludesFunctionArgumentsEvents(t *testing.T) {
	r := &grokStreamReverser{aliases: map[string]grokNsIdentity{}, customItems: map[string]bool{"custom": true}, toolSearchItems: map[string]bool{}, inputBytes: map[string]int{}}
	line := []byte("data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"custom\",\"delta\":\"{}\"}\n")
	if got := r.rewriteLine(line); got != nil {
		t.Fatalf("bridged arguments delta bypassed stream handling: %s", got)
	}
	if r.inputBytes["custom"] != 2 {
		t.Fatalf("bridged arguments delta was not accounted: %d", r.inputBytes["custom"])
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	out, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestGrokToolMappingsAreRequestLocal(t *testing.T) {
	const workers = 32
	errCh := make(chan string, workers)
	for i := 0; i < workers; i++ {
		go func(i int) {
			name := "custom_" + strconv.Itoa(i)
			body := []byte(`{"tools":[{"type":"custom","name":"` + name + `"}],"input":"x"}`)
			result := prepareGrokUpstreamBody(body)
			if len(result.Aliases) != 1 || !result.Aliases[name].Custom {
				errCh <- name
				return
			}
			errCh <- ""
		}(i)
	}
	for i := 0; i < workers; i++ {
		if name := <-errCh; name != "" {
			t.Fatalf("request-local mapping lost for %s", name)
		}
	}
}

func bytesAfterSSEData(line []byte) []byte {
	trimmed := strings.TrimSpace(string(line))
	return []byte(strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")))
}

func TestFlattenGrokNamespaceTools(t *testing.T) {
	body := []byte(`{
		"model":"grok-4.5",
		"tools":[
			{"type":"namespace","name":"mcp__calendar__","description":"Calendar","tools":[
				{"type":"function","name":"list","parameters":{"type":"object"}},
				{"type":"function","name":"create","parameters":{"type":"object"}}
			]},
			{"type":"function","name":"plain","parameters":{"type":"object"}}
		],
		"tool_choice":{"type":"function","name":"list","namespace":"mcp__calendar__"},
		"input":[
			{"type":"function_call","call_id":"c1","name":"list","namespace":"mcp__calendar__","arguments":"{}"}
		]
	}`)

	out, aliases := normalizeGrokUpstreamTools(body)
	if aliases == nil {
		t.Fatal("expected alias map")
	}
	if _, ok := aliases["mcp__calendar__list"]; !ok {
		t.Fatalf("expected alias mcp__calendar__list, got %v", aliases)
	}

	// namespace 工具应被展平：没有 type:"namespace" 残留，子函数升到顶层且改名。
	tools := gjson.GetBytes(out, "tools").Array()
	names := map[string]bool{}
	for _, tl := range tools {
		if tl.Get("type").String() == "namespace" {
			t.Fatalf("namespace tool not flattened: %s", tl.Raw)
		}
		names[tl.Get("name").String()] = true
	}
	for _, want := range []string{"mcp__calendar__list", "mcp__calendar__create", "plain"} {
		if !names[want] {
			t.Fatalf("missing flattened tool %q in %v", want, names)
		}
	}

	// tool_choice 改写为扁平名、去掉 namespace。
	if got := gjson.GetBytes(out, "tool_choice.name").String(); got != "mcp__calendar__list" {
		t.Fatalf("tool_choice.name = %q", got)
	}
	if gjson.GetBytes(out, "tool_choice.namespace").Exists() {
		t.Fatal("tool_choice.namespace should be removed")
	}

	// input 历史 function_call 也改写为扁平名。
	if got := gjson.GetBytes(out, "input.0.name").String(); got != "mcp__calendar__list" {
		t.Fatalf("input function_call name = %q", got)
	}
}

func TestFlattenGrokNamespaceToolsNoop(t *testing.T) {
	body := []byte(`{"model":"grok-4.5","tools":[{"type":"function","name":"a"}]}`)
	out, aliases := normalizeGrokUpstreamTools(body)
	if aliases != nil {
		t.Fatalf("expected nil aliases, got %v", aliases)
	}
	if string(out) != string(body) {
		t.Fatal("body should be unchanged")
	}
}

func TestNormalizeGrokWebSearchStripsControlFields(t *testing.T) {
	// Codex 的 web_search 带 external_web_access 等控制字段 → 上游 400；应剥离为最小形态。
	body := []byte(`{"model":"grok-4.5","tools":[{"type":"web_search","external_web_access":true,"indexed_web_access":true,"search_content_types":["text"],"search_context_size":"low","user_location":{"type":"approximate","country":"CN"},"filters":{"allowed_domains":["example.com"]}}]}`)
	out, _ := normalizeGrokUpstreamTools(body)
	tool := gjson.GetBytes(out, "tools.0")
	if tool.Get("type").String() != "web_search" {
		t.Fatalf("type = %q", tool.Get("type").String())
	}
	for _, banned := range []string{"external_web_access", "indexed_web_access", "search_content_types", "search_context_size", "user_location"} {
		if tool.Get(banned).Exists() {
			t.Fatalf("control field %q not stripped: %s", banned, tool.Raw)
		}
	}
	// allowed_domains 约束保留在 filters 内。
	if got := gjson.GetBytes(out, "tools.0.filters.allowed_domains.0").String(); got != "example.com" {
		t.Fatalf("allowed_domains not preserved: %s", tool.Raw)
	}
}

func TestNormalizeGrokWebSearchDisabledDropsTool(t *testing.T) {
	// external_web_access:false 无法在上游表达，整体移除工具并撤掉指向它的 tool_choice。
	body := []byte(`{"model":"grok-4.5","tools":[{"type":"web_search","external_web_access":false}],"tool_choice":{"type":"web_search"}}`)
	out, _ := normalizeGrokUpstreamTools(body)
	if gjson.GetBytes(out, "tools").Exists() && len(gjson.GetBytes(out, "tools").Array()) != 0 {
		t.Fatalf("web_search tool should be dropped: %s", gjson.GetBytes(out, "tools").Raw)
	}
	if gjson.GetBytes(out, "tool_choice").Exists() {
		t.Fatalf("tool_choice should be removed: %s", string(out))
	}
}

func TestNormalizeGrokWebSearchPreviewVariant(t *testing.T) {
	// preview 变体应归一为 web_search（Grok 不认 web_search_preview）。
	body := []byte(`{"model":"grok-4.5","tools":[{"type":"web_search_preview","search_context_size":"medium"}]}`)
	out, _ := normalizeGrokUpstreamTools(body)
	if got := gjson.GetBytes(out, "tools.0.type").String(); got != "web_search" {
		t.Fatalf("type = %q", got)
	}
}

func TestStripGrokReasoningEncryptedContent(t *testing.T) {
	// 真实形态：reasoning 项带 encrypted_content 外来密文；应删掉密文、保留明文 summary。
	body := []byte(`{"model":"grok-4.5","input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},` +
		`{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking"}],"content":null,"encrypted_content":"PefVPtNeWwpXfOREIGN"}` +
		`]}`)
	out := stripGrokUndecodableBlobs(body)
	if gjson.GetBytes(out, "input.1.encrypted_content").Exists() {
		t.Fatalf("encrypted_content not stripped: %s", string(out))
	}
	if got := gjson.GetBytes(out, "input.1.type").String(); got != "reasoning" {
		t.Fatalf("reasoning item type changed to %q", got)
	}
	if got := gjson.GetBytes(out, "input.1.summary.0.text").String(); got != "thinking" {
		t.Fatalf("plaintext summary lost: %s", string(out))
	}
	// content:null 必须被删除，否则去密文后的 reasoning 变体不匹配（422）。
	if gjson.GetBytes(out, "input.1.content").Exists() {
		t.Fatalf("null content not dropped: %s", string(out))
	}
	if gjson.GetBytes(out, "input.0.content.0.text").String() != "hi" {
		t.Fatal("retained message altered")
	}
}

func TestStripGrokEmptyReasoningBecomesBoundary(t *testing.T) {
	// reasoning 项去掉密文后无明文可回放 → 替换成 developer 边界消息。
	body := []byte(`{"model":"grok-4.5","input":[{"type":"reasoning","summary":[],"content":null,"encrypted_content":"FOREIGN"}]}`)
	out := stripGrokUndecodableBlobs(body)
	if gjson.GetBytes(out, "input.0.type").String() != "message" || gjson.GetBytes(out, "input.0.role").String() != "developer" {
		t.Fatalf("empty reasoning not replaced with boundary: %s", string(out))
	}
}

func TestStripGrokCompactionItem(t *testing.T) {
	// remote_compaction_v2 的 type:"compaction" 项整体替换成边界消息。
	body := []byte(`{"model":"grok-4.5","input":[{"type":"compaction","encrypted_content":"OPAQUE"}]}`)
	out := stripGrokUndecodableBlobs(body)
	if gjson.GetBytes(out, `input.#(type=="compaction")`).Exists() {
		t.Fatalf("compaction item not stripped: %s", string(out))
	}
	if gjson.GetBytes(out, "input.0.role").String() != "developer" {
		t.Fatalf("compaction not replaced: %s", string(out))
	}
}

func TestStripGrokUndecodableBlobsNoop(t *testing.T) {
	body := []byte(`{"model":"grok-4.5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	if string(stripGrokUndecodableBlobs(body)) != string(body) {
		t.Fatal("body without encrypted content should be unchanged")
	}
}

func TestReverseGrokNamespaceJSON(t *testing.T) {
	aliases := map[string]grokNsIdentity{
		"mcp__calendar__list": {Namespace: "mcp__calendar__", Name: "list"},
	}
	// 完整响应里的 function_call 应被反解回 {name, namespace}。
	data := []byte(`{"output":[{"type":"function_call","call_id":"c1","name":"mcp__calendar__list","arguments":"{}"}]}`)
	out := reverseGrokNamespaceJSON(data, aliases)
	var parsed map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	call := parsed["output"].([]any)[0].(map[string]any)
	if call["name"] != "list" || call["namespace"] != "mcp__calendar__" {
		t.Fatalf("function_call not restored: %v", call)
	}
}

func TestReverseGrokNamespaceSSELine(t *testing.T) {
	aliases := map[string]grokNsIdentity{
		"mcp__calendar__list": {Namespace: "mcp__calendar__", Name: "list"},
	}
	line := []byte(`data: {"type":"response.output_item.added","item":{"type":"function_call","name":"mcp__calendar__list","call_id":"c1"}}` + "\n")
	out := reverseGrokNamespaceSSELine(line, aliases)
	if !gjson.GetBytes(out[len("data: "):], "item.name").Exists() {
		t.Fatal("malformed output")
	}
	if got := gjson.GetBytes(out[len("data: "):], "item.name").String(); got != "list" {
		t.Fatalf("item.name = %q", got)
	}
	if got := gjson.GetBytes(out[len("data: "):], "item.namespace").String(); got != "mcp__calendar__" {
		t.Fatalf("item.namespace = %q", got)
	}
	// 非 function_call 行原样返回。
	plain := []byte(`data: {"type":"response.output_text.delta","delta":"hi"}` + "\n")
	if string(reverseGrokNamespaceSSELine(plain, aliases)) != string(plain) {
		t.Fatal("plain line should be unchanged")
	}
}

func TestRebuildGrokHistoryToNativeContract(t *testing.T) {
	reg := func(ns, name string, _, _ bool) string { return ns + "__" + name }
	// Codex 发的历史项带扩展字段/null；重建后应只剩 Grok 原生字段。
	cases := []struct {
		name   string
		in     string
		typ    string
		reject []string // 重建后不应出现的字段
		keep   map[string]string
	}{
		{
			name:   "function_call drops id/status/metadata",
			in:     `{"type":"function_call","id":"fc_1","status":"completed","call_id":"c1","name":"run","arguments":"{}","internal_chat_message_metadata_passthrough":{"turn_id":"t1"}}`,
			typ:    "function_call",
			reject: []string{"id", "status", "internal_chat_message_metadata_passthrough"},
			keep:   map[string]string{"call_id": "c1", "name": "run"},
		},
		{
			name:   "reasoning drops null content, keeps summary+encrypted",
			in:     `{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"t"}],"content":null,"encrypted_content":"BLOB","status":"completed"}`,
			typ:    "reasoning",
			reject: []string{"content", "status"},
			keep:   map[string]string{"id": "rs_1", "encrypted_content": "BLOB"},
		},
		{
			name:   "function_call_output keeps only call_id+output",
			in:     `{"type":"function_call_output","id":"fco_1","call_id":"c1","output":"done","status":"completed"}`,
			typ:    "function_call_output",
			reject: []string{"id", "status"},
			keep:   map[string]string{"call_id": "c1", "output": "done"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var item map[string]any
			_ = json.Unmarshal([]byte(tc.in), &item)
			rebuilt, changed := rebuildGrokHistoryItem(item, reg)
			if !changed {
				t.Fatalf("expected rebuild")
			}
			out, _ := json.Marshal(rebuilt)
			for _, f := range tc.reject {
				if gjson.GetBytes(out, f).Exists() {
					t.Fatalf("field %q should be dropped: %s", f, out)
				}
			}
			for k, want := range tc.keep {
				if got := gjson.GetBytes(out, k).String(); got != want {
					t.Fatalf("field %q = %q, want %q", k, got, want)
				}
			}
			if gjson.GetBytes(out, "type").String() != tc.typ {
				t.Fatalf("type lost")
			}
		})
	}
}

func TestNormalizeGrokUpstreamToolsCleansHistory(t *testing.T) {
	// 端到端：多轮 body（无 namespace/web_search，仅历史项)也应触发重建。
	body := []byte(`{"model":"grok-4.5","input":[` +
		`{"type":"message","role":"user","content":"hi"},` +
		`{"type":"function_call","id":"fc","status":"completed","call_id":"c1","name":"run","arguments":"{}"},` +
		`{"type":"function_call_output","id":"o","call_id":"c1","output":"ok","status":"completed"}` +
		`]}`)
	out, _ := normalizeGrokUpstreamTools(body)
	if gjson.GetBytes(out, "input.1.id").Exists() || gjson.GetBytes(out, "input.1.status").Exists() {
		t.Fatalf("function_call extension fields not stripped: %s", gjson.GetBytes(out, "input.1").Raw)
	}
	if gjson.GetBytes(out, "input.2.status").Exists() {
		t.Fatalf("function_call_output status not stripped: %s", gjson.GetBytes(out, "input.2").Raw)
	}
}
