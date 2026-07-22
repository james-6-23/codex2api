package proxy

import (
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"
)

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
	reg := func(ns, name string) string { return ns + "__" + name }
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
