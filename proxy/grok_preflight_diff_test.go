package proxy

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/tidwall/gjson"
)

// 单次遍历的前置处理（prepareGrokUpstreamBody）必须与逐步改写实现语义等价。
// 逐步实现（normalizeGrokUpstreamTools → sanitizeGrokRequestBody → grokTurnIndex）
// 在此充当参照实现：它直白但对大请求体做全量 map 往返，只保留作为等价性基准。
//
// 只比语义不比字节：单次遍历会原样保留未改写部分的原始键序与格式，
// 逐步实现走 map 往返后键序被重排，两者的字节表示本就不同。

// legacyGrokPreflight 是参照实现：按旧顺序逐步改写，产出同一组结果。
func legacyGrokPreflight(body []byte) grokPreflightResult {
	normalized, aliases := normalizeGrokUpstreamTools(body)
	sanitized := sanitizeGrokRequestBody(normalized)
	return grokPreflightResult{
		Body:      sanitized,
		Aliases:   aliases,
		TurnIndex: grokTurnIndex(sanitized),
		Model:     gjson.GetBytes(sanitized, "model").String(),
	}
}

func grokDiffCorpus() []struct {
	name string
	body string
} {
	return []struct {
		name string
		body string
	}{
		{"空对象", `{}`},
		{"非法 JSON", `{"model":`},
		{"顶层是数组", `[1,2,3]`},
		{"顶层是字符串", `"hello"`},
		{"仅 model", `{"model":"grok-4.5"}`},
		{
			"干净的 message 历史",
			`{"model":"grok-4.5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"yo"}]}]}`,
		},
		{
			"function_call 带 id/status/metadata",
			`{"model":"grok-4.5","input":[{"type":"function_call","id":"fc_1","status":"completed","call_id":"c1","name":"run","arguments":"{}","internal_chat_message_metadata_passthrough":{"turn_id":"t1"}}]}`,
		},
		{
			"reasoning content 为 null",
			`{"model":"grok-4.5","input":[{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"t"}],"content":null,"encrypted_content":"BLOB","status":"completed"}]}`,
		},
		{
			"reasoning 去密文后无可回放内容",
			`{"model":"grok-4.5","input":[{"type":"reasoning","id":"rs_1","content":null,"summary":null,"status":"completed"}]}`,
		},
		{
			"compaction 项换边界消息",
			`{"model":"grok-4.5","input":[{"type":"compaction","encrypted_content":"BLOB"},{"type":"message","role":"user","content":"hi"}]}`,
		},
		{
			"namespace 工具展平",
			`{"model":"grok-4.5","tools":[{"type":"namespace","name":"shell","tools":[{"type":"function","name":"exec","parameters":{},"defer_loading":true},{"type":"function","name":"kill","parameters":{}}]}],"input":[{"type":"message","role":"user","content":"hi"}]}`,
		},
		{
			"namespace 工具 + function_call 引用",
			`{"model":"grok-4.5","tools":[{"type":"namespace","name":"shell","tools":[{"type":"function","name":"exec","parameters":{}}]}],"input":[{"type":"function_call","namespace":"shell","name":"exec","call_id":"c1","arguments":"{}"}]}`,
		},
		{
			"namespace 前缀已带分隔符",
			`{"model":"grok-4.5","tools":[{"type":"namespace","name":"shell__","tools":[{"type":"function","name":"exec","parameters":{}}]}]}`,
		},
		{
			"namespace 撞名消歧",
			`{"model":"grok-4.5","tools":[{"type":"namespace","name":"a__b","tools":[{"type":"function","name":"c","parameters":{}}]},{"type":"namespace","name":"a","tools":[{"type":"function","name":"b__c","parameters":{}}]}]}`,
		},
		{
			"web_search 剥离控制字段",
			`{"model":"grok-4.5","tools":[{"type":"web_search","external_web_access":true,"indexed_web_access":true,"search_content_types":["text"],"user_location":{"country":"US"},"filters":{"allowed_domains":["go.dev"]}}]}`,
		},
		{
			"web_search preview 变体",
			`{"model":"grok-4.5","tools":[{"type":"web_search_preview_2025_03_11","allowed_domains":["go.dev"]}]}`,
		},
		{
			"web_search 禁外网被移除且撤 tool_choice",
			`{"model":"grok-4.5","tools":[{"type":"web_search","external_web_access":false}],"tool_choice":{"type":"web_search"}}`,
		},
		{
			"web_search 移除后仍有其它工具",
			`{"model":"grok-4.5","tools":[{"type":"web_search","external_web_access":false},{"type":"function","name":"run","parameters":{}}],"tool_choice":{"type":"web_search"}}`,
		},
		{
			"空 tools 残留 tool_choice",
			`{"model":"grok-4.5","tools":[],"tool_choice":"auto","input":[{"type":"message","role":"user","content":"hi"}]}`,
		},
		{"缺 tools 残留 tool_choice", `{"model":"grok-4.5","tool_choice":"auto"}`},
		{"tools 非数组", `{"model":"grok-4.5","tools":{"type":"function"},"tool_choice":"auto"}`},
		{
			"tool_choice 带 namespace",
			`{"model":"grok-4.5","tools":[{"type":"function","name":"shell__exec","parameters":{}}],"tool_choice":{"type":"function","name":"exec","namespace":"shell"}}`,
		},
		{
			"剥离 Codex 顶层字段",
			`{"model":"grok-4.5","client_metadata":{"cli":"codex"},"prompt_cache_key":"pk","service_tier":"priority","safety_identifier":"sid","stream":true}`,
		},
		{"reasoning.effort xhigh", `{"model":"grok-4.5","reasoning":{"effort":"xhigh","summary":"detailed"}}`},
		{"reasoning.effort max", `{"model":"grok-4.5","reasoning":{"effort":"max"}}`},
		{"reasoning.effort minimal", `{"model":"grok-4.5","reasoning":{"effort":"minimal"}}`},
		{"reasoning.effort high 不变", `{"model":"grok-4.5","reasoning":{"effort":"high"}}`},
		{"reasoning.effort 未知不动", `{"model":"grok-4.5","reasoning":{"effort":"weird"}}`},
		{"reasoning_effort 顶层形态", `{"model":"grok-4.5","reasoning_effort":"xhigh"}`},
		{"4.6 reasoning.effort xhigh 放行", `{"model":"grok-4.6","reasoning":{"effort":"xhigh","summary":"detailed"}}`},
		{"4.6 reasoning.effort max→xhigh", `{"model":"grok-4.6","reasoning":{"effort":"max"}}`},
		{"4.6 reasoning 在 model 前", `{"reasoning":{"effort":"xhigh"},"model":"grok-4.6"}`},
		{"4.6 reasoning_effort 顶层形态", `{"model":"grok-4.6","reasoning_effort":"xhigh"}`},
		{"4.20-multi-agent xhigh 放行", `{"model":"grok-4.20-multi-agent","reasoning":{"effort":"xhigh"}}`},
		{"reasoning 非对象", `{"model":"grok-4.5","reasoning":"xhigh"}`},
		{"未知历史项类型", `{"model":"grok-4.5","input":[{"type":"totally_unknown","foo":"bar","baz":null}]}`},
		{"input 非数组", `{"model":"grok-4.5","input":"hi"}`},
		{"input 含非对象元素", `{"model":"grok-4.5","input":[1,"two",null,{"type":"message","role":"user","content":"hi"}]}`},
		{"message 省略 type", `{"model":"grok-4.5","input":[{"role":"user","content":"hi"}]}`},
		{"message 省略 type 且带扩展字段", `{"model":"grok-4.5","input":[{"role":"user","content":"hi","id":"m1"}]}`},
		{"type 非字符串", `{"model":"grok-4.5","input":[{"type":5,"role":"user","content":"hi"}]}`},
		{"type 非字符串且带扩展字段", `{"model":"grok-4.5","input":[{"type":5,"role":"user","content":"hi","id":"m1"}]}`},
		{"type 非字符串且无 role", `{"model":"grok-4.5","input":[{"type":5,"content":"hi","id":"m1"}]}`},
		{"role 非字符串", `{"model":"grok-4.5","input":[{"role":7,"content":"hi","id":"m1"}]}`},
		{"role 为 null", `{"model":"grok-4.5","input":[{"type":"message","role":null,"content":"hi"}]}`},
		{"type 为 null", `{"model":"grok-4.5","input":[{"type":null,"role":"user","content":"hi"}]}`},
		{"function_call namespace 为空白", `{"model":"grok-4.5","input":[{"type":"function_call","namespace":"   ","name":"run","call_id":"c1","arguments":"{}"}]}`},
		{"function_call namespace 非字符串", `{"model":"grok-4.5","input":[{"type":"function_call","namespace":9,"name":"run","call_id":"c1","arguments":"{}"}]}`},
		{"历史项键重复", `{"model":"grok-4.5","input":[{"type":"message","role":"user","content":"a","content":"b"}]}`},
		{"历史项键重复且需重建", `{"model":"grok-4.5","input":[{"type":"message","role":"user","content":"a","content":"b","id":"x"}]}`},
		{"顶层键重复", `{"model":"grok-4.5","model":"grok-4","service_tier":"priority"}`},
		{"role 大写", `{"model":"grok-4.5","input":[{"type":"message","role":"USER","content":"hi"}]}`},
		{
			"多轮 user 消息计数",
			`{"model":"grok-4.5","input":[{"type":"message","role":"user","content":"a"},{"type":"message","role":"assistant","content":"b"},{"type":"message","role":"user","content":"c"},{"type":"message","role":"user","content":"d"}]}`,
		},
		{
			"unicode 与转义原样保留",
			`{"model":"grok-4.5","input":[{"type":"message","role":"user","content":"引号\" 反斜杠\\ 换行\n 表情😀"}]}`,
		},
		{"数字形态", `{"model":"grok-4.5","temperature":0.7,"top_p":1,"max_output_tokens":1e5,"seed":-3}`},
		{
			"mcp 与 shell 历史项",
			`{"model":"grok-4.5","input":[{"type":"mcp_call","id":"m1","name":"do","server_label":"srv","arguments":"{}","output":null,"extra":"x"},{"type":"shell_call","id":"s1","call_id":"c1","action":{"cmd":["ls"]},"status":"completed","environment":"local"}]}`,
		},
		{
			"function_call_output 带 status",
			`{"model":"grok-4.5","input":[{"type":"function_call_output","id":"fco","call_id":"c1","output":"ok","status":"completed"}]}`,
		},
		{
			"全字段齐发",
			`{"model":"grok-4.5","include":["reasoning.encrypted_content"],"stream":true,"store":false,"client_metadata":{"cli":"codex"},"prompt_cache_key":"pk","service_tier":"priority","safety_identifier":"sid","reasoning":{"effort":"xhigh","summary":"detailed"},"tools":[{"type":"namespace","name":"shell","tools":[{"type":"function","name":"exec","parameters":{},"defer_loading":true}]},{"type":"web_search","external_web_access":true,"user_location":{"country":"US"}}],"tool_choice":{"type":"function","name":"exec","namespace":"shell"},"input":[{"type":"message","role":"user","content":"hi"},{"type":"reasoning","id":"rs","summary":[{"type":"summary_text","text":"t"}],"content":null,"encrypted_content":"BLOB","status":"completed"},{"type":"function_call","id":"fc","status":"completed","namespace":"shell","name":"exec","call_id":"c1","arguments":"{}"},{"type":"function_call_output","id":"fco","call_id":"c1","output":"ok","status":"completed"},{"type":"compaction","encrypted_content":"OLD"}]}`,
		},
	}
}

func assertGrokPreflightEquivalent(t *testing.T, name string, body []byte) {
	t.Helper()
	want := legacyGrokPreflight(body)
	got := prepareGrokUpstreamBody(body)

	if !jsonSemanticallyEqual(t, want.Body, got.Body) {
		t.Errorf("%s: body 语义不等价\n参照实现: %s\n单次遍历: %s", name, want.Body, got.Body)
	}
	if want.TurnIndex != got.TurnIndex {
		t.Errorf("%s: TurnIndex = %d, 参照实现 = %d", name, got.TurnIndex, want.TurnIndex)
	}
	if want.Model != got.Model {
		t.Errorf("%s: Model = %q, 参照实现 = %q", name, got.Model, want.Model)
	}
	if !reflect.DeepEqual(normalizeAliasMap(want.Aliases), normalizeAliasMap(got.Aliases)) {
		t.Errorf("%s: 别名映射不一致\n参照实现: %#v\n单次遍历: %#v", name, want.Aliases, got.Aliases)
	}
}

// normalizeAliasMap 把 nil 与空映射视作等价，便于比较。
func normalizeAliasMap(m map[string]grokNsIdentity) map[string]grokNsIdentity {
	if len(m) == 0 {
		return map[string]grokNsIdentity{}
	}
	return m
}

// jsonSemanticallyEqual 比较两段 JSON 的语义（忽略键序与空白）。
// 非法 JSON 退化为字节比较——参照实现对非法输入原样返回，这里也必须一致。
func jsonSemanticallyEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var av, bv any
	aErr := json.Unmarshal(a, &av)
	bErr := json.Unmarshal(b, &bv)
	if aErr != nil || bErr != nil {
		return string(a) == string(b)
	}
	return reflect.DeepEqual(av, bv)
}

func TestGrokPreflightMatchesLegacyChain(t *testing.T) {
	for _, tc := range grokDiffCorpus() {
		t.Run(tc.name, func(t *testing.T) {
			assertGrokPreflightEquivalent(t, tc.name, []byte(tc.body))
		})
	}
}

// TestGrokPreflightMatchesLegacyOnLargeBodies 用基准 fixture 覆盖真实规模的请求体，
// 确保等价性在多轮大 body 上同样成立。
func TestGrokPreflightMatchesLegacyOnLargeBodies(t *testing.T) {
	for _, dirty := range []bool{true, false} {
		for _, size := range []int{16 * 1024, 256 * 1024} {
			body := buildGrokCodexBody(size, dirty)
			name := "clean"
			if dirty {
				name = "dirty"
			}
			t.Run(name, func(t *testing.T) {
				assertGrokPreflightEquivalent(t, name, body)
			})
		}
	}
}

// TestGrokPreflightPreservesUntouchedBody 无需改写的请求体必须原样返回，
// 不为等价内容白付一次重建。
func TestGrokPreflightPreservesUntouchedBody(t *testing.T) {
	body := []byte(`{"model":"grok-4.5","stream":true,"input":[{"type":"message","role":"user","content":"hi"}]}`)
	got := prepareGrokUpstreamBody(body)
	if string(got.Body) != string(body) {
		t.Fatalf("无改写时应原样返回\n输入: %s\n输出: %s", body, got.Body)
	}
	if got.TurnIndex != 1 {
		t.Fatalf("TurnIndex = %d, want 1", got.TurnIndex)
	}
	if got.Model != "grok-4.5" {
		t.Fatalf("Model = %q, want grok-4.5", got.Model)
	}
}

// TestGrokPrepareHistoryItemMatchesMapRebuild 逐项守护 raw 拼接重建与 map 版
// rebuildGrokHistoryItem 的等价性：改写与否的判定必须一致，改写结果必须语义相同。
func TestGrokPrepareHistoryItemMatchesMapRebuild(t *testing.T) {
	register := func(ns, name string, _, _ bool) string { return ns + "__" + name }
	for _, tc := range grokDiffCorpus() {
		input := gjson.Get(tc.body, "input")
		if !input.IsArray() {
			continue
		}
		input.ForEach(func(_, item gjson.Result) bool {
			if !item.IsObject() {
				return true
			}
			decoded, ok := grokDecodeObject(item.Raw)
			if !ok {
				return true
			}
			wantItem, wantChanged := rebuildGrokHistoryItem(decoded, register)
			var buf bytes.Buffer
			gotChanged, _ := grokWriteHistoryItem(&buf, item, register)
			gotRaw := buf.Bytes()
			if wantChanged != gotChanged {
				t.Errorf("%s: 改写判定不一致 (map=%t, raw=%t): %s", tc.name, wantChanged, gotChanged, item.Raw)
				return true
			}
			if !wantChanged {
				return true
			}
			wantRaw, err := json.Marshal(wantItem)
			if err != nil {
				return true
			}
			if !jsonSemanticallyEqual(t, wantRaw, gotRaw) {
				t.Errorf("%s: 重建结果不等价\nmap 版: %s\nraw 版: %s", tc.name, wantRaw, gotRaw)
			}
			return true
		})
	}
}
