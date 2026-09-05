package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestNormalizeServiceTierField(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.4","serviceTier":"fast"}`)

	got := normalizeServiceTierField(raw)

	if tier := gjson.GetBytes(got, "service_tier").String(); tier != "fast" {
		t.Fatalf("service_tier mismatch: got %q want %q", tier, "fast")
	}
	if gjson.GetBytes(got, "serviceTier").Exists() {
		t.Fatal("serviceTier should be removed after normalization")
	}
}

func TestTranslateRequestRejectsOrphanToolMessage(t *testing.T) {
	_, err := TranslateRequest([]byte(`{"model":"grok-4.5","messages":[{"role":"tool","content":"result"}]}`))
	if err == nil || !strings.Contains(err.Error(), "tool_call_id") {
		t.Fatalf("orphan tool message error = %v", err)
	}
}

func TestTranslateRequestRejectsUnknownToolCallID(t *testing.T) {
	raw := []byte(`{
		"model":"grok-4.5",
		"messages":[
			{"role":"assistant","tool_calls":[{"id":"call_known","type":"function","function":{"name":"lookup","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_unknown","content":"result"}
		]
	}`)
	_, err := TranslateRequest(raw)
	if err == nil || !strings.Contains(err.Error(), `unknown tool_call_id "call_unknown"`) {
		t.Fatalf("unknown tool call error = %v", err)
	}
}

func TestTranslateRequestAcceptsParallelToolResults(t *testing.T) {
	raw := []byte(`{
		"model":"grok-4.5",
		"messages":[
			{"role":"assistant","tool_calls":[
				{"id":"call_a","type":"function","function":{"name":"first","arguments":"{}"}},
				{"id":"call_b","type":"function","function":{"name":"second","arguments":"{}"}}
			]},
			{"role":"tool","tool_call_id":"call_b","content":"B"},
			{"role":"tool","tool_call_id":"call_a","content":"A"}
		]
	}`)
	got, err := TranslateRequest(raw)
	if err != nil {
		t.Fatalf("parallel tool results should be valid: %v", err)
	}
	count := 0
	for _, item := range gjson.GetBytes(got, "input").Array() {
		if item.Get("type").String() == "function_call_output" {
			count++
		}
	}
	if count != 2 {
		t.Fatalf("function_call_output count = %d, want 2; body=%s", count, got)
	}
}

func TestTranslateRequestPreservesChatFileContentParts(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"messages":[{"role":"user","content":[
			{"type":"text","text":"summarize these"},
			{"type":"file","file":{"filename":"report.pdf","file_data":"data:application/pdf;base64,JVBERg=="}},
			{"type":"file","file":{"file_id":"file_abc"}},
			{"type":"file","file":{"filename":"empty.pdf"}}
		]}]
	}`)

	got, err := TranslateRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	parts := gjson.GetBytes(got, "input.0.content").Array()
	if len(parts) != 3 {
		t.Fatalf("content part count = %d, want 3; body=%s", len(parts), got)
	}
	if parts[1].Get("type").String() != "input_file" ||
		parts[1].Get("filename").String() != "report.pdf" ||
		parts[1].Get("file_data").String() != "data:application/pdf;base64,JVBERg==" {
		t.Fatalf("file_data part mismatch: %s", parts[1].Raw)
	}
	if parts[2].Get("type").String() != "input_file" || parts[2].Get("file_id").String() != "file_abc" {
		t.Fatalf("file_id part mismatch: %s", parts[2].Raw)
	}
}

func TestTranslateRequestDropsMalformedToolCallHistoryAsPair(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"messages":[
			{"role":"user","content":"start"},
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_good","type":"function","function":{"name":"good","arguments":""}},
				{"id":"call_bad","type":"function","function":{"name":"bad","arguments":"{\"cmd\":\"unterminated"}}
			]},
			{"role":"tool","tool_call_id":"call_bad","content":"poison"},
			{"role":"tool","tool_call_id":"call_good","content":"ok"},
			{"role":"user","content":"continue"}
		]
	}`)

	got, err := TranslateRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "call_bad") || strings.Contains(string(got), "poison") {
		t.Fatalf("malformed call and output should be removed together: %s", got)
	}
	if args := gjson.GetBytes(got, "input.1.arguments").String(); args != "{}" {
		t.Fatalf("blank valid arguments = %q, want {}; body=%s", args, got)
	}
	if callID := gjson.GetBytes(got, "input.2.call_id").String(); callID != "call_good" {
		t.Fatalf("valid parallel tool output was not preserved: %s", got)
	}
}

func TestTranslateChatToResponsesForGrokPreservesControls(t *testing.T) {
	raw := []byte(`{
		"model":"grok-4.5",
		"messages":[{"role":"user","content":"hello"}],
		"temperature":0.25,
		"top_p":0.8,
		"max_tokens":111,
		"max_completion_tokens":222,
		"stop":["END","DONE"],
		"seed":17,
		"presence_penalty":0.2,
		"frequency_penalty":-0.1,
		"parallel_tool_calls":false,
		"tools":[{"type":"function","function":{"name":"lookup","description":"Lookup","parameters":{"type":"object","uniqueItems":true}}}],
		"tool_choice":{"type":"function","function":{"name":"lookup"}},
		"response_format":{"type":"json_schema","json_schema":{"name":"answer","strict":true,"schema":{"type":"object","uniqueItems":true}}}
	}`)
	got, err := TranslateChatToResponsesForGrok(raw)
	if err != nil {
		t.Fatalf("TranslateChatToResponsesForGrok: %v", err)
	}
	checks := map[string]string{
		"temperature":             "0.25",
		"top_p":                   "0.8",
		"max_output_tokens":       "222",
		"stop.0":                  "END",
		"stop.1":                  "DONE",
		"seed":                    "17",
		"presence_penalty":        "0.2",
		"frequency_penalty":       "-0.1",
		"tool_choice.type":        "function",
		"tool_choice.name":        "lookup",
		"text.format.type":        "json_schema",
		"text.format.name":        "answer",
		"tools.0.parameters.type": "object",
	}
	for path, want := range checks {
		if value := gjson.GetBytes(got, path); value.String() != want {
			t.Fatalf("%s = %q, want %q; body=%s", path, value.String(), want, got)
		}
	}
	if value := gjson.GetBytes(got, "parallel_tool_calls"); !value.Exists() || value.Bool() {
		t.Fatalf("parallel_tool_calls should preserve explicit false; body=%s", got)
	}
	if !gjson.GetBytes(got, "tools.0.parameters.uniqueItems").Bool() || !gjson.GetBytes(got, "text.format.schema.uniqueItems").Bool() {
		t.Fatalf("Grok canonical conversion must not apply Codex schema stripping; body=%s", got)
	}
}

func TestTranslateRequestRemainsCodexSafeWhenChatControlsPresent(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.4","messages":[{"role":"user","content":"hello"}],"temperature":0.25,"max_tokens":10,"stop":"END","tool_choice":"none"}`)
	got, err := TranslateRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"temperature", "max_output_tokens", "stop", "tool_choice"} {
		if gjson.GetBytes(got, field).Exists() {
			t.Fatalf("Codex converter unexpectedly retained %s; body=%s", field, got)
		}
	}
}

func TestResolveServiceTier(t *testing.T) {
	if got := resolveServiceTier("fast", "default"); got != "fast" {
		t.Fatalf("expected actual tier to win, got %q", got)
	}
	if got := resolveServiceTier("", "fast"); got != "fast" {
		t.Fatalf("expected requested tier fallback, got %q", got)
	}
	if got := resolveServiceTier("default", "fast"); got != "default" {
		t.Fatalf("expected actual default to win for logging, got %q", got)
	}
	// priority 是 fast 的同义词，入库归一化为 fast，便于 UI 徽章/筛选统一识别
	if got := resolveServiceTier("priority", ""); got != "fast" {
		t.Fatalf("expected priority to normalize to fast, got %q", got)
	}
	if got := resolveServiceTier("", "priority"); got != "fast" {
		t.Fatalf("expected requested priority to normalize to fast, got %q", got)
	}
	if got := resolveServiceTier("priority", "default"); got != "fast" {
		t.Fatalf("expected actual priority to normalize to fast, got %q", got)
	}
	if got := resolveServiceTier("default", "priority"); got != "default" {
		t.Fatalf("expected requested priority + upstream default to log actual default, got %q", got)
	}
	// flex / default 等其它 tier 保持原值
	if got := resolveServiceTier("flex", ""); got != "flex" {
		t.Fatalf("expected flex tier to be preserved, got %q", got)
	}
	if got := resolveServiceTier("default", ""); got != "default" {
		t.Fatalf("expected default tier with no requested intent to stay default, got %q", got)
	}
}

func TestResolveBillingServiceTier(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	ApplyRuntimeSettings(RuntimeSettings{BillingTierPolicy: BillingTierPolicyActual})

	tests := []struct {
		name      string
		actual    string
		requested string
		want      string
	}{
		{name: "actual priority keeps requested priority", actual: "priority", requested: "fast", want: "priority"},
		{name: "actual default wins when requested fast downgrades", actual: "default", requested: "fast", want: "default"},
		{name: "unknown observation cannot lower requested fast", actual: "burst", requested: "fast", want: "priority"},
		{name: "upstream priority cannot raise untiered request", actual: "priority", requested: "", want: ""},
		{name: "upstream unknown cannot raise untiered request", actual: "burst", requested: "", want: ""},
		{name: "upstream default keeps untiered request at base", actual: "default", requested: "", want: ""},
		{name: "upstream default cannot raise requested flex", actual: "default", requested: "flex", want: "flex"},
		{name: "upstream flex lowers requested priority", actual: "flex", requested: "priority", want: "flex"},
		{name: "requested fast fallback bills priority", actual: "", requested: "fast", want: "priority"},
		{name: "requested priority fallback bills priority", actual: "", requested: "priority", want: "priority"},
		{name: "case and whitespace normalized", actual: " DEFAULT ", requested: " Fast ", want: "default"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveBillingServiceTier(tt.actual, tt.requested); got != tt.want {
				t.Fatalf("resolveBillingServiceTier(%q, %q) = %q, want %q", tt.actual, tt.requested, got, tt.want)
			}
		})
	}
}

func TestResolveBillingServiceTierRequestedPolicy(t *testing.T) {
	tests := []struct {
		name      string
		actual    string
		requested string
		want      string
	}{
		{name: "requested fast bills priority when upstream downgrades", actual: "default", requested: "fast", want: "priority"},
		{name: "requested priority bills priority when upstream downgrades", actual: "default", requested: "priority", want: "priority"},
		{name: "no requested tier stays base", actual: "priority", requested: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveBillingServiceTierForPolicy(tt.actual, tt.requested, BillingTierPolicyRequested); got != tt.want {
				t.Fatalf("resolveBillingServiceTierForPolicy(%q, %q, requested) = %q, want %q", tt.actual, tt.requested, got, tt.want)
			}
		})
	}
}

func TestResolveUsageServiceTiersSplitsRequestedActualBilling(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	ApplyRuntimeSettings(RuntimeSettings{BillingTierPolicy: BillingTierPolicyActual})

	got := resolveUsageServiceTiers("default", "priority")
	if got.RequestedServiceTier != "priority" {
		t.Fatalf("requested tier = %q, want priority", got.RequestedServiceTier)
	}
	if got.ActualServiceTier != "default" {
		t.Fatalf("actual tier = %q, want default", got.ActualServiceTier)
	}
	if got.ServiceTier != "default" {
		t.Fatalf("legacy service tier = %q, want default", got.ServiceTier)
	}
	if got.BillingServiceTier != "default" {
		t.Fatalf("billing tier = %q, want default", got.BillingServiceTier)
	}
}

func TestSanitizeServiceTierForUpstream_FastToPriority(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"service_tier":"fast"
	}`)

	got := sanitizeServiceTierForUpstream(raw)

	if tier := gjson.GetBytes(got, "service_tier").String(); tier != "priority" {
		t.Fatalf("fast should be mapped to priority for upstream, got %q", tier)
	}
}

func TestSanitizeServiceTierForUpstream_DropsUnsupportedClientTiers(t *testing.T) {
	for _, tier := range []string{"auto", "default", "flex", "scale"} {
		t.Run(tier, func(t *testing.T) {
			raw := []byte(fmt.Sprintf(`{
				"model":"gpt-5.4",
				"service_tier":%q,
				"serviceTier":%q
			}`, tier, tier))

			got := sanitizeServiceTierForUpstream(raw)

			if gjson.GetBytes(got, "service_tier").Exists() {
				t.Fatalf("%s service_tier should be omitted for upstream, got body=%s", tier, got)
			}
			if gjson.GetBytes(got, "serviceTier").Exists() {
				t.Fatalf("serviceTier should be removed for upstream, got body=%s", got)
			}
		})
	}
}

func TestTranslateRequest_PreservesSupportedServiceTier(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"messages":[{"role":"user","content":"hello"}],
		"serviceTier":"priority",
		"reasoning_effort":"high"
	}`)

	got, err := TranslateRequest(raw)
	if err != nil {
		t.Fatalf("TranslateRequest returned error: %v", err)
	}

	if tier := gjson.GetBytes(got, "service_tier").String(); tier != "priority" {
		t.Fatalf("service_tier mismatch: got %q want %q", tier, "priority")
	}
	if gjson.GetBytes(got, "serviceTier").Exists() {
		t.Fatal("serviceTier should not be present after translation")
	}
	if effort := gjson.GetBytes(got, "reasoning.effort").String(); effort != "high" {
		t.Fatalf("reasoning.effort mismatch: got %q want %q", effort, "high")
	}
}

func TestTranslateRequest_DropsUnsupportedClientServiceTier(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"messages":[{"role":"user","content":"hello"}],
		"service_tier":"flex"
	}`)

	got, err := TranslateRequest(raw)
	if err != nil {
		t.Fatalf("TranslateRequest returned error: %v", err)
	}

	if gjson.GetBytes(got, "service_tier").Exists() {
		t.Fatalf("unsupported client service_tier should be omitted for upstream, got body=%s", got)
	}
}

func TestPrepareResponsesBody_DropsUnsupportedClientServiceTier(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":"hello",
		"service_tier":"flex"
	}`)

	got, _ := PrepareResponsesBody(raw)

	if gjson.GetBytes(got, "service_tier").Exists() {
		t.Fatalf("unsupported client service_tier should be omitted for upstream, got body=%s", got)
	}
}

func TestTranslateRequest_BridgesToolMessageImage(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"messages":[
			{"role":"user","content":"look"},
			{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"shot","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":[
				{"type":"text","text":"here it is"},
				{"type":"image_url","image_url":{"url":"data:image/png;base64,QQQ"}}
			]}
		]
	}`)

	got, err := TranslateRequest(raw)
	if err != nil {
		t.Fatalf("TranslateRequest returned error: %v", err)
	}

	out := gjson.GetBytes(got, `input.#(type=="function_call_output").output`).String()
	if out != "here it is" {
		t.Fatalf("function_call_output.output = %q, want text only", out)
	}
	if strings.Contains(out, "data:image") {
		t.Fatalf("function_call_output leaked image: %q", out)
	}
	// 定位带 input_image 的合成 user 消息（可能不是第一条 user 消息）。
	var found bool
	for _, item := range gjson.GetBytes(got, "input").Array() {
		if item.Get("role").String() != "user" {
			continue
		}
		img := item.Get(`content.#(type=="input_image").image_url`).String()
		if strings.Contains(img, "data:image/png;base64,QQQ") {
			found = true
			attr := item.Get(`content.#(type=="input_text").text`).String()
			if !strings.Contains(attr, "Tool output image for call call_1") {
				t.Fatalf("attribution text = %q, want call_1 attribution", attr)
			}
		}
	}
	if !found {
		t.Fatalf("synthesized user message missing input_image; body=%s", got)
	}
}

func TestTranslateRequest_ToolMessageWithoutImageUnchanged(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"messages":[
			{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"shot","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_1","content":"plain string result"}
		]
	}`)

	got, err := TranslateRequest(raw)
	if err != nil {
		t.Fatalf("TranslateRequest returned error: %v", err)
	}
	out := gjson.GetBytes(got, `input.#(type=="function_call_output").output`).String()
	if out != "plain string result" {
		t.Fatalf("function_call_output.output = %q, want unchanged string", out)
	}
	// tool 消息无图片时不产生合成 user 消息（只有原始的 assistant/function_call）。
	if gjson.GetBytes(got, `input.#(type=="message")#|#(role=="user")`).Exists() {
		t.Fatalf("unexpected synthesized user message; body=%s", got)
	}
}

func TestTranslateRequest_NormalizesReasoningEffortAliases(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"messages":[{"role":"user","content":"hello"}],
		"reasoning_effort":"MAX"
	}`)

	got, err := TranslateRequest(raw)
	if err != nil {
		t.Fatalf("TranslateRequest returned error: %v", err)
	}

	if effort := gjson.GetBytes(got, "reasoning.effort").String(); effort != "xhigh" {
		t.Fatalf("reasoning.effort mismatch: got %q want %q", effort, "xhigh")
	}
}

func TestTranslateRequest_FillsMissingArrayItemsInToolSchema(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"messages":[{"role":"user","content":"test"}],
		"tools":[
			{
				"type":"function",
				"function":{
					"name":"godot-mcp_node_signal",
					"parameters":{
						"type":"object",
						"properties":{
							"args":{"type":"array"}
						}
					}
				}
			}
		]
	}`)

	got, err := TranslateRequest(raw)
	if err != nil {
		t.Fatalf("TranslateRequest returned error: %v", err)
	}

	items := gjson.GetBytes(got, "tools.0.parameters.properties.args.items")
	if !items.Exists() || items.Type != gjson.JSON {
		t.Fatalf("expected array schema items object to be injected, got %s", items.Raw)
	}
}

func TestTranslateRequest_DropsInvalidRequiredInToolSchema(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"messages":[{"role":"user","content":"test"}],
		"tools":[
			{
				"type":"function",
				"function":{
					"name":"session_status",
					"parameters":{
						"type":"object",
						"required":null,
						"properties":{
							"session_id":{"type":"string"},
							"metadata":{
								"type":"object",
								"required":[null, "", "kind"],
								"properties":{"kind":{"type":"string"}}
							}
						}
					}
				}
			}
		]
	}`)

	got, err := TranslateRequest(raw)
	if err != nil {
		t.Fatalf("TranslateRequest returned error: %v", err)
	}

	if required := gjson.GetBytes(got, "tools.0.parameters.required"); required.Exists() {
		t.Fatalf("null required should be removed from tool schema, got %s; body=%s", required.Raw, got)
	}
	nestedRequired := gjson.GetBytes(got, "tools.0.parameters.properties.metadata.required")
	if nestedRequired.Raw != `["kind"]` {
		t.Fatalf("nested required should keep only string entries, got %s; body=%s", nestedRequired.Raw, got)
	}
}

// 显式为 null 的 parameters.type 上游必拒。function 工具补成 object，
// 非 function 工具至少要把 null 摘掉（坏 schema 沉进多轮历史会每轮必 400）。
func TestPrepareResponsesBodyFixesNullParametersType(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],
		"tools":[
			{"type":"function","name":"automation_update","parameters":{"type":null,"properties":{}}},
			{"type":"tool_search","parameters":{"type":null}}
		]
	}`)

	got, _ := PrepareResponsesBody(raw)
	if strings.Contains(string(got), `"type":null`) {
		t.Fatalf("null schema type survived: %s", got)
	}
	if typ := gjson.GetBytes(got, `tools.#(name=="automation_update").parameters.type`).String(); typ != "object" {
		t.Fatalf("function tool parameters.type = %q, want object; body=%s", typ, got)
	}
	if gjson.GetBytes(got, `tools.#(type=="tool_search").parameters.type`).Exists() {
		t.Fatalf("non-function tool should have the null type dropped; body=%s", got)
	}
}

// 嵌套在 properties/items 里的 null type 同样要清掉。
func TestPrepareResponsesBodyFixesNestedNullSchemaType(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],
		"tools":[{"type":"function","name":"f","parameters":{"type":"object","properties":{
			"a":{"type":null},
			"b":{"type":"array","items":{"type":null}}
		}}}]
	}`)

	got, _ := PrepareResponsesBody(raw)
	if strings.Contains(string(got), `"type":null`) {
		t.Fatalf("nested null schema type survived: %s", got)
	}
}

// Responses Lite 把工具搬进 input[].additional_tools.tools[]，那里的坏 schema
// 过去完全绕过顶层 tools[] 的全部修正直达上游。
func TestPrepareResponsesBodyNormalizesAdditionalToolsSchemas(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
			{"type":"additional_tools","tools":[
				{"type":"function","name":"automation_update","parameters":{"type":null,"properties":{}}}
			]}
		]
	}`)

	got, _ := PrepareResponsesBody(raw)
	if strings.Contains(string(got), `"type":null`) {
		t.Fatalf("null type inside additional_tools survived: %s", got)
	}
	var fixed bool
	for _, item := range gjson.GetBytes(got, "input").Array() {
		if item.Get("type").String() != "additional_tools" {
			continue
		}
		if typ := item.Get(`tools.#(name=="automation_update").parameters.type`).String(); typ == "object" {
			fixed = true
		}
	}
	if !fixed {
		t.Fatalf("additional_tools function schema was not normalized to object; body=%s", got)
	}
}

// 保留工具（collaboration.*）必须逐字透传，清洗不得碰它（issue #342）。
func TestPrepareResponsesBodyLeavesReservedToolsUntouched(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
			{"type":"additional_tools","tools":[
				{"type":"function","name":"collaboration.create_task","parameters":{"type":null}}
			]}
		]
	}`)

	got, _ := PrepareResponsesBody(raw)
	if !strings.Contains(string(got), `"type":null`) {
		t.Fatalf("reserved tool schema must pass through verbatim, even a null type; body=%s", got)
	}
}

// additional_tools 里的工具常按 OpenAI SDK 惯例省掉顶层 type。补 type 的前置处理
// 过去只跑在顶层 tools[]，于是这里的工具被判成非 function，parameters 只被摘掉
// null 而拿不到 object 兜底，上游照旧回 `got 'type: "None"'`。
func TestPrepareResponsesBodyNormalizesAdditionalToolsMissingType(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
			{"type":"additional_tools","tools":[
				{"name":"automation_update","parameters":{"type":null,"properties":{}}}
			]}
		]
	}`)

	got, _ := PrepareResponsesBody(raw)
	var checked bool
	for _, item := range gjson.GetBytes(got, "input").Array() {
		if item.Get("type").String() != "additional_tools" {
			continue
		}
		tool := item.Get(`tools.#(name=="automation_update")`)
		if tool.Get("type").String() != "function" {
			t.Fatalf("missing tool type was not filled in; tool=%s", tool.Raw)
		}
		if typ := tool.Get("parameters.type").String(); typ != "object" {
			t.Fatalf("parameters.type = %q, want object; tool=%s", typ, tool.Raw)
		}
		checked = true
	}
	if !checked {
		t.Fatalf("additional_tools carrier missing from output; body=%s", got)
	}
}

// additional_tools 里的 Chat 形态嵌套 function 子对象过去完全不摊平，坏 schema
// 连同 function 子对象一起原样送到上游。
func TestPrepareResponsesBodyFlattensAdditionalToolsNestedFunction(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
			{"type":"additional_tools","tools":[
				{"type":"function","function":{"name":"automation_update","parameters":{"type":null,"properties":{}}}}
			]}
		]
	}`)

	got, _ := PrepareResponsesBody(raw)
	if strings.Contains(string(got), `"type":null`) {
		t.Fatalf("null type survived inside additional_tools nested function; body=%s", got)
	}
	if strings.Contains(string(got), `"function":{`) {
		t.Fatalf("nested function object was not flattened; body=%s", got)
	}
}

// definitions 是 draft-07 的定义容器（pydantic v1、旧版 zod-to-json-schema 都生成
// 它而不是 $defs）。递归下钻过去只认 $defs，藏在这里的坏 schema 全部放行。
func TestSanitizeSchemaRecursesIntoDefinitions(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],
		"tools":[{"type":"function","name":"f","parameters":{
			"type":"object",
			"properties":{"a":{"$ref":"#/definitions/A"}},
			"definitions":{"A":{"type":null,"pattern":"^x$"}}
		}}]
	}`)

	got, _ := PrepareResponsesBody(raw)
	if strings.Contains(string(got), `"type":null`) {
		t.Fatalf("null type survived inside definitions; body=%s", got)
	}
	if strings.Contains(string(got), `"pattern"`) {
		t.Fatalf("unsupported key survived inside definitions; body=%s", got)
	}
}

// Chat Completions 路径共用同一套 sanitizer，definitions 缺口在那里同样存在。
func TestTranslateRequestRecursesIntoDefinitions(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"f","parameters":{
			"type":"object",
			"properties":{"a":{"$ref":"#/definitions/A"}},
			"definitions":{"A":{"type":null,"minLength":3}}
		}}}]
	}`)

	got, err := TranslateRequest(raw)
	if err != nil {
		t.Fatalf("TranslateRequest: %v", err)
	}
	if strings.Contains(string(got), `"type":null`) {
		t.Fatalf("null type survived inside definitions on chat path; body=%s", got)
	}
	if strings.Contains(string(got), `"minLength"`) {
		t.Fatalf("unsupported key survived inside definitions on chat path; body=%s", got)
	}
}

// draft-07 的 tuple 校验把 items 写成数组，旧的下钻只对 map 形态生效。
func TestSanitizeSchemaRecursesIntoTupleItems(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],
		"tools":[{"type":"function","name":"f","parameters":{
			"type":"object",
			"properties":{"a":{"type":"array","items":[{"type":null},{"type":"string","pattern":"^x$"}]}}
		}}]
	}`)

	got, _ := PrepareResponsesBody(raw)
	if strings.Contains(string(got), `"type":null`) {
		t.Fatalf("null type survived inside tuple items; body=%s", got)
	}
	if strings.Contains(string(got), `"pattern"`) {
		t.Fatalf("unsupported key survived inside tuple items; body=%s", got)
	}
}

// 组合类关键字下的子 schema 同样要清洗，否则坏 schema 换个位置就能绕过。
func TestSanitizeSchemaRecursesIntoCompositionKeywords(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}],
		"tools":[{"type":"function","name":"f","parameters":{
			"type":"object",
			"properties":{
				"a":{"type":"array","prefixItems":[{"type":null}]},
				"b":{"type":"object","patternProperties":{"^x":{"type":null}}},
				"c":{"not":{"type":null}},
				"d":{"if":{"type":null},"then":{"type":null},"else":{"type":null}},
				"e":{"type":"object","propertyNames":{"type":null}},
				"f":{"type":"array","contains":{"type":null}},
				"g":{"type":"object","dependentSchemas":{"x":{"type":null}}}
			}
		}}]
	}`)

	got, _ := PrepareResponsesBody(raw)
	if strings.Contains(string(got), `"type":null`) {
		t.Fatalf("null type survived under a composition keyword; body=%s", got)
	}
}

func TestPrepareResponsesBody_FillsMissingArrayItemsInToolSchema(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":"test",
		"tools":[
			{
				"type":"function",
				"name":"godot-mcp_node_signal",
				"parameters":{
					"type":"object",
					"properties":{
						"args":{"type":"array"}
					}
				}
			}
		]
	}`)

	got, _ := PrepareResponsesBody(raw)

	items := gjson.GetBytes(got, "tools.0.parameters.properties.args.items")
	if !items.Exists() || items.Type != gjson.JSON {
		t.Fatalf("expected array schema items object to be injected, got %s", items.Raw)
	}
}

func TestPrepareResponsesBody_DropsInvalidRequiredInToolSchema(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":"test",
		"tools":[
			{
				"type":"function",
				"name":"session_status",
				"parameters":{
					"type":"object",
					"required":null,
					"properties":{
						"session_id":{"type":"string"},
						"metadata":{
							"type":"object",
							"required":[null, "", "kind"],
							"properties":{"kind":{"type":"string"}}
						}
					}
				}
			}
		]
	}`)

	got, _ := PrepareResponsesBody(raw)

	if required := gjson.GetBytes(got, "tools.0.parameters.required"); required.Exists() {
		t.Fatalf("null required should be removed from tool schema, got %s; body=%s", required.Raw, got)
	}
	nestedRequired := gjson.GetBytes(got, "tools.0.parameters.properties.metadata.required")
	if nestedRequired.Raw != `["kind"]` {
		t.Fatalf("nested required should keep only string entries, got %s; body=%s", nestedRequired.Raw, got)
	}
}

func TestPrepareResponsesBody_DefaultsNullFunctionToolParameters(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":"test",
		"tools":[
			{
				"type":"function",
				"name":"Agent",
				"parameters":null
			}
		]
	}`)

	got, _ := PrepareResponsesBody(raw)

	if typ := gjson.GetBytes(got, "tools.0.parameters.type").String(); typ != "object" {
		t.Fatalf("expected default function schema type object, got %q; body=%s", typ, got)
	}
	if props := gjson.GetBytes(got, "tools.0.parameters.properties"); !props.Exists() || props.Type != gjson.JSON {
		t.Fatalf("expected default function schema properties object, got %s; body=%s", props.Raw, got)
	}
}

func TestPrepareResponsesBody_DefaultsMissingFunctionToolParameters(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":"test",
		"tools":[
			{
				"type":"function",
				"name":"Agent"
			}
		]
	}`)

	got, _ := PrepareResponsesBody(raw)

	if typ := gjson.GetBytes(got, "tools.0.parameters.type").String(); typ != "object" {
		t.Fatalf("expected default function schema type object, got %q; body=%s", typ, got)
	}
}

func TestTranslateRequest_InfersFunctionToolTypeWhenMissing(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[
			{"function":{"name":"get_weather","parameters":{"type":"object","properties":{}}}},
			{"name":"lookup","parameters":{"type":"object","properties":{}}}
		]
	}`)

	got, err := TranslateRequest(raw)
	if err != nil {
		t.Fatalf("TranslateRequest returned error: %v", err)
	}

	first := gjson.GetBytes(got, "tools.0")
	if toolType := first.Get("type").String(); toolType != "function" {
		t.Fatalf("tools.0.type = %q, want function; body=%s", toolType, got)
	}
	if name := first.Get("name").String(); name != "get_weather" {
		t.Fatalf("tools.0.name = %q, want get_weather; body=%s", name, got)
	}
	if first.Get("function").Exists() {
		t.Fatalf("tools.0 nested function object should be expanded, got %s", got)
	}
	second := gjson.GetBytes(got, "tools.1")
	if toolType := second.Get("type").String(); toolType != "function" {
		t.Fatalf("tools.1.type = %q, want function; body=%s", toolType, got)
	}
	if name := second.Get("name").String(); name != "lookup" {
		t.Fatalf("tools.1.name = %q, want lookup; body=%s", name, got)
	}
}

func TestTranslateRequest_DropsTypelessUnrecognizedTool(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"foo":"bar"},{"type":null,"description":"no shape"}]
	}`)

	got, err := TranslateRequest(raw)
	if err != nil {
		t.Fatalf("TranslateRequest returned error: %v", err)
	}

	for _, tool := range gjson.GetBytes(got, "tools").Array() {
		if strings.TrimSpace(tool.Get("type").String()) == "" {
			t.Fatalf("typeless tool should be dropped, got %s", got)
		}
	}
}

func TestTranslateRequest_DefaultsNullFunctionToolParameters(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"messages":[{"role":"user","content":"test"}],
		"tools":[
			{
				"type":"function",
				"function":{
					"name":"Agent",
					"parameters":null
				}
			}
		]
	}`)

	got, err := TranslateRequest(raw)
	if err != nil {
		t.Fatalf("TranslateRequest returned error: %v", err)
	}

	if typ := gjson.GetBytes(got, "tools.0.parameters.type").String(); typ != "object" {
		t.Fatalf("expected default function schema type object, got %q; body=%s", typ, got)
	}
}

func TestPrepareResponsesBody_NormalizesLegacyFileContentPart(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{
				"type":"message",
				"role":"user",
				"content":[
					{"type":"text","text":"read this"},
					{"type":"file","file":{"file_id":"file_abc","filename":"notes.pdf"}}
				]
			}
		]
	}`)

	got, expandedInputRaw := PrepareResponsesBody(raw)

	if typ := gjson.GetBytes(got, "input.0.content.0.type").String(); typ != "input_text" {
		t.Fatalf("expected legacy text part to normalize to input_text, got %q; body=%s", typ, got)
	}
	if typ := gjson.GetBytes(got, "input.0.content.1.type").String(); typ != "input_file" {
		t.Fatalf("expected legacy file part to normalize to input_file, got %q; body=%s", typ, got)
	}
	if fileID := gjson.GetBytes(got, "input.0.content.1.file_id").String(); fileID != "file_abc" {
		t.Fatalf("expected file_id to be flattened, got %q; body=%s", fileID, got)
	}
	if filename := gjson.GetBytes(got, "input.0.content.1.filename").String(); filename != "notes.pdf" {
		t.Fatalf("expected filename to be preserved, got %q; body=%s", filename, got)
	}
	if gjson.GetBytes(got, "input.0.content.1.file").Exists() {
		t.Fatalf("legacy file wrapper should be removed; body=%s", got)
	}
	if typ := gjson.Get(expandedInputRaw, "0.content.1.type").String(); typ != "input_file" {
		t.Fatalf("expanded input should contain normalized input_file, got %q; expanded=%s", typ, expandedInputRaw)
	}
}

func TestPrepareResponsesBody_NormalizesAssistantInputTextToOutputText(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.5",
		"input":[
			{
				"type":"message",
				"role":"assistant",
				"content":[
					{"type":"input_text","text":"prior assistant answer"}
				]
			}
		]
	}`)

	got, expandedInputRaw := PrepareResponsesBody(raw)

	if typ := gjson.GetBytes(got, "input.0.content.0.type").String(); typ != "output_text" {
		t.Fatalf("assistant input_text should normalize to output_text, got %q; body=%s", typ, got)
	}
	if typ := gjson.Get(expandedInputRaw, "0.content.0.type").String(); typ != "output_text" {
		t.Fatalf("expanded assistant content should normalize to output_text, got %q; expanded=%s", typ, expandedInputRaw)
	}
}

func TestPrepareResponsesBody_NormalizesUserOutputTextToInputText(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.5",
		"input":[
			{
				"type":"message",
				"role":"user",
				"content":[
					{"type":"output_text","text":"hello"}
				]
			}
		]
	}`)

	got, _ := PrepareResponsesBody(raw)

	if typ := gjson.GetBytes(got, "input.0.content.0.type").String(); typ != "input_text" {
		t.Fatalf("user output_text should normalize to input_text, got %q; body=%s", typ, got)
	}
}

// 上游 Codex 不接受 role=system（"System messages are not allowed"），原生
// Responses 直通路径须与 chat/Anthropic 链路一致改写为 developer（issue #409）。
func TestPrepareResponsesBody_RewritesSystemRoleToDeveloper(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.5",
		"input":[
			{
				"type":"message",
				"role":"system",
				"content":[
					{"type":"input_text","text":"you are a helpful assistant"}
				]
			},
			{"role":"system","content":"bare system without type"},
			{"type":"message","role":"user","content":"hi"},
			{"type":"function_call","call_id":"c1","name":"foo","arguments":"{}"}
		]
	}`)

	got, _ := PrepareResponsesBody(raw)

	if role := gjson.GetBytes(got, "input.0.role").String(); role != "developer" {
		t.Fatalf("typed system message should become developer, got %q; body=%s", role, got)
	}
	if text := gjson.GetBytes(got, "input.0.content.0.text").String(); text != "you are a helpful assistant" {
		t.Fatalf("system content should be preserved, got %q; body=%s", text, got)
	}
	if role := gjson.GetBytes(got, "input.1.role").String(); role != "developer" {
		t.Fatalf("bare system message should become developer, got %q; body=%s", role, got)
	}
	if role := gjson.GetBytes(got, "input.2.role").String(); role != "user" {
		t.Fatalf("user message must stay untouched, got %q; body=%s", role, got)
	}
	if name := gjson.GetBytes(got, "input.3.name").String(); name != "foo" {
		t.Fatalf("non-message item must stay untouched; body=%s", got)
	}
}

func TestPrepareResponsesWebSocketBody_RewritesSystemRoleToDeveloper(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.5",
		"input":[
			{"type":"message","role":"system","content":"system prompt"}
		]
	}`)

	got, _ := PrepareResponsesWebSocketBody(raw)

	if role := gjson.GetBytes(got, "input.0.role").String(); role != "developer" {
		t.Fatalf("WS path should also rewrite system to developer, got %q; body=%s", role, got)
	}
}

func TestPrepareResponsesBody_ConvertsOrphanToolOutputToMessage(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.5",
		"input":[
			{"type":"message","role":"user","content":"what is the weather?"},
			{"type":"function_call_output","call_id":"call_orphan1","output":"{\"temp\":21}"},
			{"type":"function_call","call_id":"call_ok","name":"get_weather","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_ok","output":"sunny"}
		]
	}`)

	got, _ := PrepareResponsesBody(raw)

	if typ := gjson.GetBytes(got, "input.1.type").String(); typ != "message" {
		t.Fatalf("orphan output should become a message, got %q; body=%s", typ, got)
	}
	if role := gjson.GetBytes(got, "input.1.role").String(); role != "user" {
		t.Fatalf("orphan output message should be user role, got %q; body=%s", role, got)
	}
	text := gjson.GetBytes(got, "input.1.content.0.text").String()
	if !strings.Contains(text, "call_orphan1") || !strings.Contains(text, `{"temp":21}`) {
		t.Fatalf("orphan output text should keep call_id and content, got %q", text)
	}
	if typ := gjson.GetBytes(got, "input.3.type").String(); typ != "function_call_output" {
		t.Fatalf("paired output must stay untouched, got %q; body=%s", typ, got)
	}
}

func TestPrepareResponsesBody_SynthesizesOutputForOrphanToolCall(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.5",
		"input":[
			{"type":"message","role":"user","content":"hi"},
			{"type":"function_call","call_id":"call_orphan2","name":"get_weather","arguments":"{}"},
			{"type":"message","role":"user","content":"continue"}
		]
	}`)

	got, _ := PrepareResponsesBody(raw)

	if typ := gjson.GetBytes(got, "input.2.type").String(); typ != "function_call_output" {
		t.Fatalf("orphan call should get a placeholder output right after it, got %q; body=%s", typ, got)
	}
	if callID := gjson.GetBytes(got, "input.2.call_id").String(); callID != "call_orphan2" {
		t.Fatalf("placeholder output call_id mismatch, got %q; body=%s", callID, got)
	}
	if typ := gjson.GetBytes(got, "input.1.type").String(); typ != "function_call" {
		t.Fatalf("orphan call itself must be preserved; body=%s", got)
	}
}

func TestPrepareResponsesBody_FlattensOrphanOutputContentParts(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.5",
		"input":[
			{"type":"function_call_output","call_id":"call_parts","output":[{"type":"output_text","text":"part a"},{"type":"output_text","text":"part b"}]}
		]
	}`)

	got, _ := PrepareResponsesBody(raw)

	text := gjson.GetBytes(got, "input.0.content.0.text").String()
	if !strings.Contains(text, "part a") || !strings.Contains(text, "part b") {
		t.Fatalf("content-part outputs should be flattened into text, got %q", text)
	}
}

func TestPrepareResponsesWebSocketBody_SkipsPairingRepairWithPreviousResponseID(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.5",
		"previous_response_id":"resp_123",
		"input":[
			{"type":"function_call_output","call_id":"call_linked","output":"result"}
		]
	}`)

	got, _ := PrepareResponsesWebSocketBody(raw)

	if typ := gjson.GetBytes(got, "input.0.type").String(); typ != "function_call_output" {
		t.Fatalf("orphan output is legitimate when previous_response_id is preserved, got %q; body=%s", typ, got)
	}
}

func TestPrepareResponsesWebSocketBody_RepairsPairingWithoutPreviousResponseID(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.5",
		"input":[
			{"type":"function_call_output","call_id":"call_ws_orphan","output":"result"}
		]
	}`)

	got, _ := PrepareResponsesWebSocketBody(raw)

	if typ := gjson.GetBytes(got, "input.0.type").String(); typ != "message" {
		t.Fatalf("WS path without previous_response_id should repair orphan output, got %q; body=%s", typ, got)
	}
}

func TestPrepareResponsesBody_NormalizesLegacyTopLevelFileInput(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"type":"file","file":"file_top"}
		]
	}`)

	got, _ := PrepareResponsesBody(raw)

	if typ := gjson.GetBytes(got, "input.0.type").String(); typ != "input_file" {
		t.Fatalf("expected top-level legacy file input to normalize to input_file, got %q; body=%s", typ, got)
	}
	if fileID := gjson.GetBytes(got, "input.0.file_id").String(); fileID != "file_top" {
		t.Fatalf("expected top-level file shorthand to become file_id, got %q; body=%s", fileID, got)
	}
	if gjson.GetBytes(got, "input.0.file").Exists() {
		t.Fatalf("legacy file wrapper should be removed; body=%s", got)
	}
}

func TestPrepareResponsesBody_NormalizesLegacyImageContentPart(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{
				"type":"message",
				"role":"user",
				"content":[
					{"type":"image_url","image_url":{"url":"https://example.com/cat.png"}}
				]
			}
		]
	}`)

	got, _ := PrepareResponsesBody(raw)

	if typ := gjson.GetBytes(got, "input.0.content.0.type").String(); typ != "input_image" {
		t.Fatalf("expected legacy image_url part to normalize to input_image, got %q; body=%s", typ, got)
	}
	if imageURL := gjson.GetBytes(got, "input.0.content.0.image_url").String(); imageURL != "https://example.com/cat.png" {
		t.Fatalf("expected image_url object to be flattened, got %q; body=%s", imageURL, got)
	}
}

func TestPrepareOpenAIResponsesBody_NormalizesLegacyImageContentPart(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"stream":false,
		"input":[
			{
				"type":"message",
				"role":"user",
				"content":[
					{"type":"image_url","image_url":{"url":"https://example.com/cat.png"}}
				]
			}
		]
	}`)

	got := PrepareOpenAIResponsesBody(raw)

	if typ := gjson.GetBytes(got, "input.0.content.0.type").String(); typ != "input_image" {
		t.Fatalf("expected legacy image_url part to normalize to input_image, got %q; body=%s", typ, got)
	}
	if imageURL := gjson.GetBytes(got, "input.0.content.0.image_url").String(); imageURL != "https://example.com/cat.png" {
		t.Fatalf("expected image_url object to be flattened, got %q; body=%s", imageURL, got)
	}
	if gjson.GetBytes(got, "include").Exists() {
		t.Fatalf("OpenAI Responses body should not get Codex include defaults; body=%s", got)
	}
	if gjson.GetBytes(got, "store").Exists() {
		t.Fatalf("OpenAI Responses body should not get Codex store defaults; body=%s", got)
	}
	if stream := gjson.GetBytes(got, "stream"); !stream.Exists() || stream.Bool() {
		t.Fatalf("OpenAI Responses body should preserve stream=false; body=%s", got)
	}
}

func TestPrepareOpenAIResponsesBody_ImageGenerationToolChoiceInjectsTool(t *testing.T) {
	tests := []struct {
		name string
		raw  string
	}{
		{
			name: "object tool_choice",
			raw: `{
				"model":"gpt-5.4",
				"input":"draw a cat",
				"tool_choice":{"type":"image_generation"}
			}`,
		},
		{
			name: "string tool_choice",
			raw: `{
				"model":"gpt-5.4",
				"input":"draw a cat",
				"tool_choice":"image_generation"
			}`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := PrepareOpenAIResponsesBody([]byte(tc.raw))

			if toolType := gjson.GetBytes(got, "tools.0.type").String(); toolType != "image_generation" {
				t.Fatalf("tool type = %q, want image_generation; body=%s", toolType, got)
			}
			if toolModel := gjson.GetBytes(got, "tools.0.model").String(); toolModel != defaultImagesToolModel {
				t.Fatalf("tool model = %q, want %q; body=%s", toolModel, defaultImagesToolModel, got)
			}
			if gjson.GetBytes(got, "include").Exists() {
				t.Fatalf("OpenAI Responses body should not get Codex include defaults; body=%s", got)
			}
			if gjson.GetBytes(got, "store").Exists() {
				t.Fatalf("OpenAI Responses body should not get Codex store defaults; body=%s", got)
			}
		})
	}
}

func TestPrepareResponsesBody_SanitizesTextFormatJSONSchema(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":"test",
		"text":{
			"format":{
				"type":"json_schema",
				"name":"codex_output_schema",
				"schema":{
					"type":"object",
					"properties":{
						"testEnvironmentContract":{
							"type":"object",
							"minProperties":1,
							"maxProperties":2,
							"properties":{
								"runtime":{"type":"string","minLength":1}
							}
						},
						"steps":{"type":"array"}
					}
				},
				"strict":true
			}
		}
	}`)

	got, _ := PrepareResponsesBody(raw)

	if name := gjson.GetBytes(got, "text.format.name").String(); name != "codex_output_schema" {
		t.Fatalf("expected text.format.name to be preserved, got %q; body=%s", name, got)
	}
	if gjson.GetBytes(got, "text.format.schema.properties.testEnvironmentContract.minProperties").Exists() {
		t.Fatalf("minProperties should be stripped from structured output schema; body=%s", got)
	}
	if gjson.GetBytes(got, "text.format.schema.properties.testEnvironmentContract.maxProperties").Exists() {
		t.Fatalf("maxProperties should be stripped from structured output schema; body=%s", got)
	}
	if gjson.GetBytes(got, "text.format.schema.properties.testEnvironmentContract.properties.runtime.minLength").Exists() {
		t.Fatalf("nested minLength should be stripped from structured output schema; body=%s", got)
	}
	if items := gjson.GetBytes(got, "text.format.schema.properties.steps.items"); !items.Exists() || items.Type != gjson.JSON {
		t.Fatalf("array items should be injected in structured output schema, got %s; body=%s", items.Raw, got)
	}
}

func TestPrepareResponsesBody_AlignsRequiredWithProperties(t *testing.T) {
	// 复现上游报错：In context=('properties','candidates','items'),
	// required 里出现了 properties 中不存在的 'title'，同时漏掉了已声明的 key。
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":"test",
		"text":{
			"format":{
				"type":"json_schema",
				"name":"codex_output_schema",
				"strict":true,
				"schema":{
					"type":"object",
					"properties":{
						"candidates":{
							"type":"array",
							"items":{
								"type":"object",
								"properties":{
									"name":{"type":"string"},
									"score":{"type":"number"}
								},
								"required":["name","title"]
							}
						}
					}
				}
			}
		}
	}`)

	got, _ := PrepareResponsesBody(raw)

	itemsRequired := gjson.GetBytes(got, "text.format.schema.properties.candidates.items.required")
	var names []string
	for _, v := range itemsRequired.Array() {
		names = append(names, v.String())
	}
	if len(names) != 2 || names[0] != "name" || names[1] != "score" {
		t.Fatalf("items.required should drop extra 'title' and backfill 'score', got %v; body=%s", names, got)
	}
	rootRequired := gjson.GetBytes(got, "text.format.schema.required")
	if len(rootRequired.Array()) != 1 || rootRequired.Array()[0].String() != "candidates" {
		t.Fatalf("root required should be backfilled to all property keys, got %s; body=%s", rootRequired.Raw, got)
	}
}

func TestPrepareResponsesBody_JSONSchemaDoesNotInjectImageBridge(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.5",
		"input":"Extract the name and age from: John is 30 years old",
		"text":{
			"format":{
				"type":"json_schema",
				"name":"person",
				"strict":true,
				"schema":{
					"type":"object",
					"properties":{
						"name":{"type":"string"},
						"age":{"type":"integer"}
					},
					"required":["name","age"],
					"additionalProperties":false
				}
			}
		}
	}`)

	got, _ := PrepareResponsesBody(raw)

	if gjson.GetBytes(got, "tools").Exists() {
		t.Fatalf("json_schema responses should not get implicit image tools; body=%s", got)
	}
	if gjson.GetBytes(got, "instructions").Exists() {
		t.Fatalf("json_schema responses should not get image bridge instructions; body=%s", got)
	}
	if typ := gjson.GetBytes(got, "text.format.type").String(); typ != "json_schema" {
		t.Fatalf("expected json_schema format to be preserved, got %q; body=%s", typ, got)
	}
	if input := gjson.GetBytes(got, "input.0.content").String(); input != "Extract the name and age from: John is 30 years old" {
		t.Fatalf("expected original input to be preserved, got %q; body=%s", input, got)
	}
}

func TestPrepareResponsesBody_ConvertsAndSanitizesLegacyResponseFormat(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":"test",
		"response_format":{
			"type":"json_schema",
			"json_schema":{
				"name":"codex_output_schema",
				"schema":{
					"type":"object",
					"properties":{
						"testEnvironmentContract":{
							"type":"object",
							"minProperties":1,
							"properties":{}
						}
					}
				},
				"strict":true
			}
		}
	}`)

	got, _ := PrepareResponsesBody(raw)

	if gjson.GetBytes(got, "response_format").Exists() {
		t.Fatalf("legacy response_format should be removed after conversion; body=%s", got)
	}
	if typ := gjson.GetBytes(got, "text.format.type").String(); typ != "json_schema" {
		t.Fatalf("expected response_format to convert to text.format json_schema, got %q; body=%s", typ, got)
	}
	if name := gjson.GetBytes(got, "text.format.name").String(); name != "codex_output_schema" {
		t.Fatalf("expected json_schema name to be preserved, got %q; body=%s", name, got)
	}
	if gjson.GetBytes(got, "text.format.schema.properties.testEnvironmentContract.minProperties").Exists() {
		t.Fatalf("minProperties should be stripped after response_format conversion; body=%s", got)
	}
	if v := gjson.GetBytes(got, "text.format.schema.additionalProperties"); !v.Exists() || v.Bool() {
		t.Fatalf("root object should get additionalProperties=false, got %s; body=%s", v.Raw, got)
	}
	if v := gjson.GetBytes(got, "text.format.schema.properties.testEnvironmentContract.additionalProperties"); !v.Exists() || v.Bool() {
		t.Fatalf("nested object should get additionalProperties=false, got %s; body=%s", v.Raw, got)
	}
}

func TestTranslateRequest_ConvertsAndSanitizesResponseFormat(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"messages":[{"role":"user","content":"test"}],
		"response_format":{
			"type":"json_schema",
			"json_schema":{
				"name":"codex_output_schema",
				"schema":{
					"type":"object",
					"properties":{
						"testEnvironmentContract":{
							"type":"object",
							"minProperties":1,
							"properties":{}
						}
					}
				}
			}
		}
	}`)

	got, err := TranslateRequest(raw)
	if err != nil {
		t.Fatalf("TranslateRequest returned error: %v", err)
	}

	if gjson.GetBytes(got, "response_format").Exists() {
		t.Fatalf("legacy response_format should not be forwarded, got %s", got)
	}
	if typ := gjson.GetBytes(got, "text.format.type").String(); typ != "json_schema" {
		t.Fatalf("expected text.format json_schema, got %q; body=%s", typ, got)
	}
	if gjson.GetBytes(got, "text.format.schema.properties.testEnvironmentContract.minProperties").Exists() {
		t.Fatalf("minProperties should be stripped in translated response_format schema; body=%s", got)
	}
	if v := gjson.GetBytes(got, "text.format.schema.additionalProperties"); !v.Exists() || v.Bool() {
		t.Fatalf("root object should get additionalProperties=false, got %s; body=%s", v.Raw, got)
	}
	if v := gjson.GetBytes(got, "text.format.schema.properties.testEnvironmentContract.additionalProperties"); !v.Exists() || v.Bool() {
		t.Fatalf("nested object should get additionalProperties=false, got %s; body=%s", v.Raw, got)
	}
}

func TestTranslateRequest_JSONObjectInjectsJSONHintWhenInputOmitsJSON(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"messages":[{"role":"user","content":"return name"}],
		"response_format":{"type":"json_object"}
	}`)

	got, err := TranslateRequest(raw)
	if err != nil {
		t.Fatalf("TranslateRequest returned error: %v", err)
	}

	if typ := gjson.GetBytes(got, "text.format.type").String(); typ != "json_object" {
		t.Fatalf("expected json_object text format, got %q; body=%s", typ, got)
	}
	if role := gjson.GetBytes(got, "input.0.role").String(); role != "developer" {
		t.Fatalf("expected injected developer hint, got role %q; body=%s", role, got)
	}
	if hint := gjson.GetBytes(got, "input.0.content.0.text").String(); !strings.Contains(strings.ToLower(hint), "json") {
		t.Fatalf("expected injected hint to mention JSON, got %q; body=%s", hint, got)
	}
	if userText := gjson.GetBytes(got, "input.1.content.0.text").String(); userText != "return name" {
		t.Fatalf("expected original user input to be preserved, got %q; body=%s", userText, got)
	}
}

func TestTranslateRequest_JSONObjectDoesNotInjectWhenInputMentionsJSON(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"messages":[{"role":"user","content":"return JSON with name"}],
		"response_format":{"type":"json_object"}
	}`)

	got, err := TranslateRequest(raw)
	if err != nil {
		t.Fatalf("TranslateRequest returned error: %v", err)
	}

	if role := gjson.GetBytes(got, "input.0.role").String(); role != "user" {
		t.Fatalf("did not expect injected hint when input already mentions JSON, got role %q; body=%s", role, got)
	}
	if inputLen := len(gjson.GetBytes(got, "input").Array()); inputLen != 1 {
		t.Fatalf("expected one input item, got %d; body=%s", inputLen, got)
	}
}

func TestPrepareResponsesBody_JSONObjectInjectsJSONHintWhenInputOmitsJSON(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":"return name",
		"text":{"format":{"type":"json_object"}}
	}`)

	got, _ := PrepareResponsesBody(raw)

	if role := gjson.GetBytes(got, "input.0.role").String(); role != "developer" {
		t.Fatalf("expected injected developer hint, got role %q; body=%s", role, got)
	}
	if hint := gjson.GetBytes(got, "input.0.content.0.text").String(); !strings.Contains(strings.ToLower(hint), "json") {
		t.Fatalf("expected injected hint to mention JSON, got %q; body=%s", hint, got)
	}
	if userText := gjson.GetBytes(got, "input.1.content").String(); userText != "return name" {
		t.Fatalf("expected original user input to be preserved, got %q; body=%s", userText, got)
	}
}

func TestPrepareOpenAIResponsesBody_JSONObjectPrefixesStringInputWhenInputOmitsJSON(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":"return name",
		"text":{"format":{"type":"json_object"}}
	}`)

	got := PrepareOpenAIResponsesBody(raw)

	input := gjson.GetBytes(got, "input").String()
	if !strings.Contains(strings.ToLower(input), "json") || !strings.Contains(input, "return name") {
		t.Fatalf("expected string input to include JSON hint and original input, got %q; body=%s", input, got)
	}
	if gjson.GetBytes(got, "include").Exists() {
		t.Fatalf("OpenAI Responses body should not get Codex include defaults; body=%s", got)
	}
}

func TestPrepareResponsesBody_DefaultsIncludeForResponses(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":"test"
	}`)

	got, _ := PrepareResponsesBody(raw)

	include := gjson.GetBytes(got, "include")
	if !include.Exists() || len(include.Array()) != 1 || include.Array()[0].String() != "reasoning.encrypted_content" {
		t.Fatalf("expected default include for responses, got %s", include.Raw)
	}
	if stream := gjson.GetBytes(got, "stream"); !stream.Exists() || !stream.Bool() {
		t.Fatalf("expected stream to be forced for responses, got %s", stream.Raw)
	}
	if store := gjson.GetBytes(got, "store"); !store.Exists() || store.Bool() {
		t.Fatalf("expected store=false for responses, got %s", store.Raw)
	}
	if gotTool := gjson.GetBytes(got, "tools.0.type").String(); gotTool != "image_generation" {
		t.Fatalf("expected default image_generation tool, got %s", string(got))
	}
	if model := gjson.GetBytes(got, "tools.0.model").String(); model != defaultImagesToolModel {
		t.Fatalf("expected default image model %q, got %q", defaultImagesToolModel, model)
	}
	if size := gjson.GetBytes(got, "tools.0.size").String(); size != defaultImages1KSize {
		t.Fatalf("expected default image size %q, got %q", defaultImages1KSize, size)
	}
	if format := gjson.GetBytes(got, "tools.0.output_format").String(); format != "png" {
		t.Fatalf("expected default image output_format png, got %q", format)
	}
	if instructions := gjson.GetBytes(got, "instructions").String(); !strings.Contains(instructions, codexImageGenerationBridgeMarker) {
		t.Fatalf("expected bridge instructions, got %q", instructions)
	}
}

func TestPrepareResponsesBody_MergesReasoningInclude(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":"test",
		"reasoning":{"effort":"high"},
		"include":["file_search_call.results"]
	}`)

	got, _ := PrepareResponsesBody(raw)

	include := gjson.GetBytes(got, "include").Array()
	if len(include) != 2 {
		t.Fatalf("include length = %d, want 2; body=%s", len(include), got)
	}
	if include[0].String() != "file_search_call.results" {
		t.Fatalf("existing include not preserved first, got %s; body=%s", include[0].Raw, got)
	}
	if include[1].String() != codexReasoningEncryptedContentInclude {
		t.Fatalf("reasoning include not appended, got %s; body=%s", include[1].Raw, got)
	}
}

func TestPrepareResponsesBody_DoesNotDuplicateReasoningInclude(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":"test",
		"reasoning":{"effort":"high"},
		"include":["reasoning.encrypted_content","file_search_call.results"]
	}`)

	got, _ := PrepareResponsesBody(raw)

	var count int
	for _, item := range gjson.GetBytes(got, "include").Array() {
		if item.String() == codexReasoningEncryptedContentInclude {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("reasoning include count = %d, want 1; body=%s", count, got)
	}
}

func TestPrepareResponsesBody_ToolChoiceImageGenerationAutoInjectsTool(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":"draw a poster",
		"tool_choice":{"type":"image_generation"},
		"text":{"format":{"type":"json_object"}}
	}`)

	got, _ := PrepareResponsesBody(raw)

	if toolType := gjson.GetBytes(got, "tools.0.type").String(); toolType != "image_generation" {
		t.Fatalf("tool type = %q, want image_generation; body=%s", toolType, got)
	}
	if toolModel := gjson.GetBytes(got, "tools.0.model").String(); toolModel != defaultImagesToolModel {
		t.Fatalf("tool model = %q, want %q; body=%s", toolModel, defaultImagesToolModel, got)
	}
}

func TestPrepareResponsesBody_SparkModelSkipsDefaultImageGenerationTool(t *testing.T) {
	raw := []byte(`{"model":"gpt-5.3-codex-spark","input":"hi"}`)

	got, _ := PrepareResponsesBody(raw)

	if gjson.GetBytes(got, "tools").Exists() {
		t.Fatalf("did not expect default image_generation tool for spark model, got %s", string(got))
	}
	if instructions := gjson.GetBytes(got, "instructions").String(); strings.Contains(instructions, codexImageGenerationBridgeMarker) {
		t.Fatalf("did not expect image generation bridge instructions for spark model, got %q", instructions)
	}
}

func TestPrepareResponsesBody_SparkModelKeepsExplicitImageGenerationTool(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.3-codex-spark",
		"input":"draw a poster",
		"tools":[{"type":"image_generation"}]
	}`)

	got, _ := PrepareResponsesBody(raw)

	if toolType := gjson.GetBytes(got, "tools.0.type").String(); toolType != "image_generation" {
		t.Fatalf("explicit image_generation tool should be kept for spark model, got %s", string(got))
	}
}

func TestPrepareResponsesBody_ImageGenNamespaceToolSkipsInjectionAndBridge(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.5",
		"input":"draw a poster",
		"tools":[{"type":"namespace","name":"image_gen","tools":[{"type":"function","name":"imagegen"}]}]
	}`)

	got, _ := PrepareResponsesBody(raw)

	tools := gjson.GetBytes(got, "tools")
	for _, tool := range tools.Array() {
		if tool.Get("type").String() == "image_generation" {
			t.Fatalf("hosted image_generation tool should not be injected alongside image_gen namespace, got %s", string(got))
		}
	}
	if gjson.GetBytes(got, "tools.0.name").String() != "image_gen" {
		t.Fatalf("namespace image_gen tool should be preserved, got %s", string(got))
	}
	if instructions := gjson.GetBytes(got, "instructions").String(); strings.Contains(instructions, codexImageGenerationBridgeMarker) {
		t.Fatalf("bridge instructions should not be injected when image_gen namespace is present, got %q", instructions)
	}
}

func TestPrepareResponsesBody_ImageGenNamespaceInAdditionalToolsSkipsInjection(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.5",
		"input":[
			{"type":"additional_tools","tools":[{"type":"namespace","name":"image_gen"}]},
			{"role":"user","content":"draw a poster"}
		]
	}`)

	got, _ := PrepareResponsesBody(raw)

	for _, tool := range gjson.GetBytes(got, "tools").Array() {
		if tool.Get("type").String() == "image_generation" {
			t.Fatalf("hosted image_generation tool should not be injected for additional_tools image_gen, got %s", string(got))
		}
	}
	if instructions := gjson.GetBytes(got, "instructions").String(); strings.Contains(instructions, codexImageGenerationBridgeMarker) {
		t.Fatalf("bridge instructions should not be injected, got %q", instructions)
	}
}

func TestPrepareResponsesBody_NonImageNamespaceToolStillInjectsDefault(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.5",
		"input":"hello",
		"tools":[{"type":"namespace","name":"code_tools","tools":[{"type":"function","name":"run"}]}]
	}`)

	got, _ := PrepareResponsesBody(raw)

	found := false
	for _, tool := range gjson.GetBytes(got, "tools").Array() {
		if tool.Get("type").String() == "image_generation" {
			found = true
		}
	}
	if !found {
		t.Fatalf("non-image namespace tool should not suppress default injection, got %s", string(got))
	}
}

func TestPrepareResponsesBody_NormalizesNestedReasoningEffortAliases(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":"test",
		"reasoning":{"effort":"MAX"}
	}`)

	got, _ := PrepareResponsesBody(raw)

	if effort := gjson.GetBytes(got, "reasoning.effort").String(); effort != "xhigh" {
		t.Fatalf("reasoning.effort mismatch: got %q want %q; body=%s", effort, "xhigh", got)
	}
}

func TestPrepareResponsesBody_DropsBlankReasoningEffort(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":"test",
		"reasoning_effort":"   "
	}`)

	got, _ := PrepareResponsesBody(raw)

	if gjson.GetBytes(got, "reasoning.effort").Exists() {
		t.Fatalf("reasoning.effort should be omitted for blank effort; body=%s", got)
	}
}

func TestPrepareResponsesBody_DropsBlankNestedReasoningEffort(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":"test",
		"reasoning":{"effort":"   ","summary":"auto"}
	}`)

	got, _ := PrepareResponsesBody(raw)

	if gjson.GetBytes(got, "reasoning.effort").Exists() {
		t.Fatalf("reasoning.effort should be omitted for blank nested effort; body=%s", got)
	}
	if summary := gjson.GetBytes(got, "reasoning.summary").String(); summary != "auto" {
		t.Fatalf("reasoning.summary mismatch: got %q want auto; body=%s", summary, got)
	}
}

func TestPrepareResponsesBody_ImageOnlyModelBuildsImageToolRequest(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-image-2",
		"prompt":"draw a cat",
		"size":"1024x1024",
		"quality":"high",
		"output_format":"webp",
		"partial_images":2
	}`)

	got, _ := PrepareResponsesBody(raw)

	if model := gjson.GetBytes(got, "model").String(); model != defaultImagesMainModel {
		t.Fatalf("model = %q, want %q; body=%s", model, defaultImagesMainModel, got)
	}
	if text := gjson.GetBytes(got, "input.0.content").String(); text != "draw a cat" {
		t.Fatalf("input text = %q, want draw a cat; body=%s", text, got)
	}
	if gjson.GetBytes(got, "prompt").Exists() {
		t.Fatalf("prompt should be removed, got %s", got)
	}
	if toolModel := gjson.GetBytes(got, "tools.0.model").String(); toolModel != "gpt-image-2" {
		t.Fatalf("tool model = %q, want gpt-image-2; body=%s", toolModel, got)
	}
	if size := gjson.GetBytes(got, "tools.0.size").String(); size != "1024x1024" {
		t.Fatalf("tool size = %q, want 1024x1024; body=%s", size, got)
	}
	if format := gjson.GetBytes(got, "tools.0.output_format").String(); format != "webp" {
		t.Fatalf("tool output_format = %q, want webp; body=%s", format, got)
	}
	if partial := gjson.GetBytes(got, "tools.0.partial_images").Int(); partial != 2 {
		t.Fatalf("partial_images = %d, want 2; body=%s", partial, got)
	}
	if choice := gjson.GetBytes(got, "tool_choice.type").String(); choice != "image_generation" {
		t.Fatalf("tool_choice.type = %q, want image_generation; body=%s", choice, got)
	}
}

func TestPrepareResponsesBody_ImageAliasSetsDefaultSizeAndRealToolModel(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-image-2-4k",
		"prompt":"draw a city wallpaper"
	}`)

	got, _ := PrepareResponsesBody(raw)

	if model := gjson.GetBytes(got, "model").String(); model != defaultImagesMainModel {
		t.Fatalf("model = %q, want %q; body=%s", model, defaultImagesMainModel, got)
	}
	if toolModel := gjson.GetBytes(got, "tools.0.model").String(); toolModel != defaultImagesToolModel {
		t.Fatalf("tool model = %q, want %q; body=%s", toolModel, defaultImagesToolModel, got)
	}
	if size := gjson.GetBytes(got, "tools.0.size").String(); size != defaultImages4KSize {
		t.Fatalf("tool size = %q, want %q; body=%s", size, defaultImages4KSize, got)
	}
}

func TestPrepareResponsesBody_ExplicitSizeOverridesImageAliasDefault(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-image-2-4k",
		"prompt":"draw a square logo",
		"size":"1536x1536"
	}`)

	got, _ := PrepareResponsesBody(raw)

	if toolModel := gjson.GetBytes(got, "tools.0.model").String(); toolModel != defaultImagesToolModel {
		t.Fatalf("tool model = %q, want %q; body=%s", toolModel, defaultImagesToolModel, got)
	}
	if size := gjson.GetBytes(got, "tools.0.size").String(); size != "1536x1536" {
		t.Fatalf("tool size = %q, want explicit size; body=%s", size, got)
	}
}

func TestPrepareResponsesBody_ToolImageAliasInfersPortraitSize(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4-mini",
		"input":"draw a poster",
		"tools":[{"type":"image_generation","model":"gpt-image-2-2k"}],
		"tool_choice":{"type":"image_generation"}
	}`)

	got, _ := PrepareResponsesBody(raw)

	if toolModel := gjson.GetBytes(got, "tools.0.model").String(); toolModel != defaultImagesToolModel {
		t.Fatalf("tool model = %q, want %q; body=%s", toolModel, defaultImagesToolModel, got)
	}
	if size := gjson.GetBytes(got, "tools.0.size").String(); size != defaultImages2KPortraitSize {
		t.Fatalf("tool size = %q, want %q; body=%s", size, defaultImages2KPortraitSize, got)
	}
}

func TestPrepareResponsesBody_ImageAliasInfersPortraitFromStructuredInput(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-image-2-4k",
		"input":[
			{
				"role":"user",
				"content":[
					{"type":"input_text","text":"mobile wallpaper portrait neon skyline"}
				]
			}
		]
	}`)

	got, _ := PrepareResponsesBody(raw)

	if model := gjson.GetBytes(got, "model").String(); model != defaultImagesMainModel {
		t.Fatalf("model = %q, want %q; body=%s", model, defaultImagesMainModel, got)
	}
	if toolModel := gjson.GetBytes(got, "tools.0.model").String(); toolModel != defaultImagesToolModel {
		t.Fatalf("tool model = %q, want %q; body=%s", toolModel, defaultImagesToolModel, got)
	}
	if size := gjson.GetBytes(got, "tools.0.size").String(); size != defaultImages4KPortraitSize {
		t.Fatalf("tool size = %q, want %q; body=%s", size, defaultImages4KPortraitSize, got)
	}
}

func TestPrepareResponsesBody_ImageAliasInfersSquareFromToolPrompt(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4-mini",
		"input":"square app icon logo",
		"tools":[{"type":"image_generation","model":"gpt-image-2-4k"}]
	}`)

	got, _ := PrepareResponsesBody(raw)

	if toolModel := gjson.GetBytes(got, "tools.0.model").String(); toolModel != defaultImagesToolModel {
		t.Fatalf("tool model = %q, want %q; body=%s", toolModel, defaultImagesToolModel, got)
	}
	if size := gjson.GetBytes(got, "tools.0.size").String(); size != defaultImages4KSquareSize {
		t.Fatalf("tool size = %q, want %q; body=%s", size, defaultImages4KSquareSize, got)
	}
}

func TestPrepareResponsesBody_InvalidImageSizeSurvivesForValidation(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-image-2-4k",
		"prompt":"draw a giant image",
		"size":1024
	}`)

	got, _ := PrepareResponsesBody(raw)

	if size := gjson.GetBytes(got, "tools.0.size"); size.Type != gjson.Number {
		t.Fatalf("expected invalid numeric size to survive validation, got %s; body=%s", size.Raw, got)
	}
	if err := validateResponsesImageGenerationSizes(got); err == nil {
		t.Fatalf("expected image size validation error; body=%s", got)
	}
}

func TestPrepareResponsesBody_PromptCompatAndTopLevelImageOptions(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4-mini",
		"prompt":"draw a skyline sticker",
		"size":"1536x1024",
		"quality":"high",
		"background":"transparent",
		"output_format":"webp"
	}`)

	got, _ := PrepareResponsesBody(raw)

	if text := gjson.GetBytes(got, "input.0.content").String(); text != "draw a skyline sticker" {
		t.Fatalf("input text = %q, want prompt text; body=%s", text, got)
	}
	for _, field := range []string{"prompt", "size", "quality", "background", "output_format"} {
		if gjson.GetBytes(got, field).Exists() {
			t.Fatalf("top-level %s should be removed, got %s", field, got)
		}
	}
	if size := gjson.GetBytes(got, "tools.0.size").String(); size != "1536x1024" {
		t.Fatalf("tool size = %q, want 1536x1024; body=%s", size, got)
	}
	if model := gjson.GetBytes(got, "tools.0.model").String(); model != defaultImagesToolModel {
		t.Fatalf("tool model = %q, want %q; body=%s", model, defaultImagesToolModel, got)
	}
	if quality := gjson.GetBytes(got, "tools.0.quality").String(); quality != "high" {
		t.Fatalf("tool quality = %q, want high; body=%s", quality, got)
	}
	if background := gjson.GetBytes(got, "tools.0.background").String(); background != "transparent" {
		t.Fatalf("tool background = %q, want transparent; body=%s", background, got)
	}
	if format := gjson.GetBytes(got, "tools.0.output_format").String(); format != "webp" {
		t.Fatalf("tool output_format = %q, want webp; body=%s", format, got)
	}
}

func TestPrepareResponsesBody_InjectsImageToolWithinToolLimit(t *testing.T) {
	tools := make([]any, maxTools)
	for i := range tools {
		tools[i] = map[string]any{
			"type":        "function",
			"name":        fmt.Sprintf("tool_%d", i),
			"description": "test tool",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{},
			},
		}
	}
	raw, err := json.Marshal(map[string]any{
		"model": "gpt-5.4-mini",
		"input": "test",
		"tools": tools,
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	got, _ := PrepareResponsesBody(raw)

	outTools := gjson.GetBytes(got, "tools").Array()
	if len(outTools) != maxTools {
		t.Fatalf("tools count = %d, want %d; body=%s", len(outTools), maxTools, got)
	}
	last := outTools[len(outTools)-1]
	if last.Get("type").String() != "image_generation" {
		t.Fatalf("last tool type = %q, want image_generation; body=%s", last.Get("type").String(), got)
	}
	if last.Get("model").String() != defaultImagesToolModel {
		t.Fatalf("image tool model = %q, want %q; body=%s", last.Get("model").String(), defaultImagesToolModel, got)
	}
}

func TestPrepareResponsesBody_PreservesExistingImageToolAndNormalizesAliases(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4-mini",
		"input":"draw a cat",
		"style":"cinematic",
		"tools":[
			{"type":"image_generation","model":"gpt-image-1.5","format":"webp","compression":45,"style":"cinematic"}
		],
		"instructions":"base"
	}`)

	got, _ := PrepareResponsesBody(raw)

	if count := len(gjson.GetBytes(got, "tools").Array()); count != 1 {
		t.Fatalf("tools count = %d, want 1; body=%s", count, got)
	}
	if model := gjson.GetBytes(got, "tools.0.model").String(); model != "gpt-image-1.5" {
		t.Fatalf("tool model = %q, want gpt-image-1.5; body=%s", model, got)
	}
	if format := gjson.GetBytes(got, "tools.0.output_format").String(); format != "webp" {
		t.Fatalf("output_format = %q, want webp; body=%s", format, got)
	}
	if compression := gjson.GetBytes(got, "tools.0.output_compression").Int(); compression != 45 {
		t.Fatalf("output_compression = %d, want 45; body=%s", compression, got)
	}
	if gjson.GetBytes(got, "tools.0.format").Exists() || gjson.GetBytes(got, "tools.0.compression").Exists() {
		t.Fatalf("legacy aliases should be removed, got %s", got)
	}
	if gjson.GetBytes(got, "style").Exists() || gjson.GetBytes(got, "tools.0.style").Exists() {
		t.Fatalf("unsupported style parameter should be removed, got %s", got)
	}
	instructions := gjson.GetBytes(got, "instructions").String()
	if !strings.HasPrefix(instructions, "base\n\n") {
		t.Fatalf("expected bridge to append after existing instructions, got %q", instructions)
	}
	if strings.Count(instructions, codexImageGenerationBridgeMarker) != 1 {
		t.Fatalf("expected bridge marker once, got %q", instructions)
	}

	gotAgain, _ := PrepareResponsesBody(got)
	if instructionsAgain := gjson.GetBytes(gotAgain, "instructions").String(); strings.Count(instructionsAgain, codexImageGenerationBridgeMarker) != 1 {
		t.Fatalf("expected bridge marker once after second pass, got %q", instructionsAgain)
	}
}

func TestPrepareCompactResponsesBody_RemovesUnsupportedInjectedFields(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":"test"
	}`)

	got, _ := PrepareCompactResponsesBody(raw)

	for _, field := range []string{"include", "store", "stream"} {
		if gjson.GetBytes(got, field).Exists() {
			t.Fatalf("expected %s to be removed for compact body", field)
		}
	}
	input := gjson.GetBytes(got, "input")
	if !input.Exists() || !input.IsArray() || len(input.Array()) != 1 {
		t.Fatalf("expected compact input to remain normalized, got %s", input.Raw)
	}
	if input.Array()[0].Get("content").String() != "test" {
		t.Fatalf("expected compact input content to be preserved, got %s", input.Raw)
	}
}

func TestPrepareOpenAIResponsesCompactBody_RemovesUnsupportedFields(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-4.1",
		"input":"hello",
		"include":["reasoning.encrypted_content"],
		"store":true,
		"stream":true
	}`)

	got := PrepareOpenAIResponsesCompactBody(raw)

	for _, field := range []string{"include", "store", "stream"} {
		if gjson.GetBytes(got, field).Exists() {
			t.Fatalf("expected %s to be removed for OpenAI Responses compact body, got %s", field, got)
		}
	}
	if model := gjson.GetBytes(got, "model").String(); model != "gpt-4.1" {
		t.Fatalf("model = %q, want gpt-4.1; body=%s", model, got)
	}
	if input := gjson.GetBytes(got, "input").String(); input != "hello" {
		t.Fatalf("input = %q, want hello; body=%s", input, got)
	}
}

func TestPrepareCompactResponsesBody_RemovesClientSuppliedInclude(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":"test",
		"include":["reasoning.encrypted_content"]
	}`)

	got, _ := PrepareCompactResponsesBody(raw)

	if gjson.GetBytes(got, "include").Exists() {
		t.Fatalf("expected client-supplied include to be removed for compact body, got %s", string(got))
	}
}

func TestPrepareResponsesBody_ConvertsPlaintextCompactionToDeveloperMessage(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"type":"message","role":"user","content":"hello"},
			{"type":"compaction","text":"previous context was compacted"}
		]
	}`)

	got, expandedInputRaw := PrepareResponsesBody(raw)

	input := gjson.GetBytes(got, "input")
	if gotType := input.Get("1.type").String(); gotType == "compaction" {
		t.Fatalf("plaintext compaction item should not be sent upstream, got %s", input.Raw)
	}
	if gotRole := input.Get("1.role").String(); gotRole != "developer" {
		t.Fatalf("converted compaction role = %q, want developer; input=%s", gotRole, input.Raw)
	}
	if gotText := input.Get("1.content.0.text").String(); !strings.Contains(gotText, "previous context was compacted") {
		t.Fatalf("converted compaction text = %q, want summary; input=%s", gotText, input.Raw)
	}

	expanded := gjson.Parse(expandedInputRaw)
	if gotType := expanded.Get("1.type").String(); gotType == "compaction" {
		t.Fatalf("expanded input cache should not retain plaintext compaction, got %s", expanded.Raw)
	}
	if gotRole := expanded.Get("1.role").String(); gotRole != "developer" {
		t.Fatalf("expanded compaction role = %q, want developer; input=%s", gotRole, expanded.Raw)
	}
}

func TestPrepareResponsesBody_PassesThroughCompactV2Items(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"type":"compaction","encrypted_content":"opaque-blob"},
			{"type":"message","role":"user","content":"hello"},
			{"type":"compaction_trigger"}
		]
	}`)

	got, _ := PrepareResponsesBody(raw)

	input := gjson.GetBytes(got, "input")
	if gotType := input.Get("0.type").String(); gotType != "compaction" {
		t.Fatalf("encrypted compaction item type = %q, want compaction (verbatim passthrough); input=%s", gotType, input.Raw)
	}
	if gotContent := input.Get("0.encrypted_content").String(); gotContent != "opaque-blob" {
		t.Fatalf("encrypted compaction content = %q, want opaque-blob; input=%s", gotContent, input.Raw)
	}
	if gotType := input.Get("2.type").String(); gotType != "compaction_trigger" {
		t.Fatalf("compaction_trigger item type = %q, want compaction_trigger (verbatim passthrough); input=%s", gotType, input.Raw)
	}
}

func TestPrepareResponsesBody_MovesCompactionTriggerToFinalInputItem(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.6-sol",
		"input":[
			{"type":"message","role":"user","content":"compact this"},
			{"type":"compaction_trigger"},
			{"type":"function_call_output","call_id":"call_1","output":"ok"}
		]
	}`)

	got, _ := PrepareResponsesBody(raw)
	input := gjson.GetBytes(got, "input").Array()
	if len(input) != 3 {
		t.Fatalf("input length = %d, want 3; body=%s", len(input), got)
	}
	if gotType := input[1].Get("type").String(); gotType == "compaction_trigger" {
		t.Fatalf("compaction_trigger must not remain before the final item; body=%s", got)
	}
	if gotType := input[2].Get("type").String(); gotType != "compaction_trigger" {
		t.Fatalf("final input type = %q, want compaction_trigger; body=%s", gotType, got)
	}
}

func TestPrepareOpenAIResponsesBody_MovesCompactionTriggerToFinalInputItem(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.6-sol",
		"stream":true,
		"input":[
			{"type":"compaction_trigger"},
			{"type":"context_compaction","encrypted_content":"opaque"},
			{"type":"function_call_output","call_id":"call_1","output":"ok"}
		]
	}`)

	got := PrepareOpenAIResponsesBody(raw)
	input := gjson.GetBytes(got, "input").Array()
	if len(input) != 3 {
		t.Fatalf("input length = %d, want 3; body=%s", len(input), got)
	}
	if gotType := input[0].Get("type").String(); gotType != "context_compaction" {
		t.Fatalf("input[0].type = %q, want context_compaction; body=%s", gotType, got)
	}
	if gotType := input[2].Get("type").String(); gotType != "compaction_trigger" {
		t.Fatalf("final input type = %q, want compaction_trigger; body=%s", gotType, got)
	}
}

func TestPrepareResponsesBody_CachedHistoryStillKeepsCompactionTriggerFinal(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.6-sol",
		"previous_response_id":"resp_previous",
		"input":[
			{"type":"compaction_trigger"},
			{"type":"function_call_output","call_id":"call_1","output":"ok"}
		]
	}`)
	cached := []json.RawMessage{
		json.RawMessage(`{"type":"message","role":"user","content":"earlier question"}`),
		json.RawMessage(`{"type":"function_call","call_id":"call_1","name":"run","arguments":"{}"}`),
	}

	got, expanded := prepareResponsesBodyWithOptions(raw, responsesBodyPrepareOptions{
		forceStoreFalse:        true,
		expandPreviousResponse: true,
		cachedResponseItems:    cached,
	})
	input := gjson.GetBytes(got, "input").Array()
	if len(input) != 4 {
		t.Fatalf("input length = %d, want 4; body=%s", len(input), got)
	}
	if gotType := input[len(input)-1].Get("type").String(); gotType != "compaction_trigger" {
		t.Fatalf("final input type = %q, want compaction_trigger; body=%s", gotType, got)
	}
	if expandedType := gjson.Get(expanded, "#(type==\"compaction_trigger\").type").String(); expandedType != "compaction_trigger" {
		t.Fatalf("expanded cache input lost compaction_trigger: %s", expanded)
	}
	if gotType := gjson.Get(expanded, "3.type").String(); gotType != "compaction_trigger" {
		t.Fatalf("expanded final input type = %q, want compaction_trigger; expanded=%s", gotType, expanded)
	}
}

func TestPrepareCompactResponsesBody_ConvertsPlaintextCompactionToDeveloperMessage(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"type":"message","role":"user","content":"hello"},
			{"type":"compaction","summary":"previous context was compacted"}
		]
	}`)

	got, _ := PrepareCompactResponsesBody(raw)

	input := gjson.GetBytes(got, "input")
	if gotType := input.Get("1.type").String(); gotType == "compaction" {
		t.Fatalf("plaintext compaction item should not be sent to compact upstream, got %s", input.Raw)
	}
	if gotRole := input.Get("1.role").String(); gotRole != "developer" {
		t.Fatalf("converted compaction role = %q, want developer; input=%s", gotRole, input.Raw)
	}
	if gotText := input.Get("1.content.0.text").String(); !strings.Contains(gotText, "previous context was compacted") {
		t.Fatalf("converted compaction text = %q, want summary; input=%s", gotText, input.Raw)
	}
}

func TestPrepareResponsesBody_DefaultsMissingMessageContent(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"type":"message","role":"user","content":"hello"},
			{"type":"function_call","call_id":"call_abc","name":"lookup","arguments":"{}"},
			{"type":"message","role":"user"}
		]
	}`)

	got, expandedInputRaw := PrepareResponsesBody(raw)

	// 孤儿 function_call 会被配对修复在其后补一条占位 output，
	// 缺省 content 的 message 顺移到下标 3。
	if content := gjson.GetBytes(got, "input.3.content"); !content.Exists() || content.String() != "" {
		t.Fatalf("missing message content should be defaulted to empty string, got %s; body=%s", content.Raw, got)
	}
	if gjson.GetBytes(got, "input.1.content").Exists() {
		t.Fatalf("non-message tool context item should not receive content, got %s", got)
	}
	if content := gjson.Get(expandedInputRaw, "3.content"); !content.Exists() || content.String() != "" {
		t.Fatalf("expanded input should include normalized empty content, got %s; expanded=%s", content.Raw, expandedInputRaw)
	}
}

func TestValidateResponsesFunctionNamesRejectsEmptyInputName(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"type":"message","role":"user","content":"hello"},
			{"type":"function_call","call_id":"call_abc","name":"","arguments":"{}"}
		]
	}`)

	err := ValidateResponsesFunctionNames(raw)
	if err == nil {
		t.Fatal("expected empty function_call name to be rejected")
	}
	if !strings.Contains(err.Error(), "input[1].name") {
		t.Fatalf("error should identify input item name, got %v", err)
	}
}

func TestValidateResponsesFunctionNamesRejectsEmptyToolName(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":"hello",
		"tools":[{"type":"function","name":"  ","parameters":{"type":"object"}}]
	}`)

	err := ValidateResponsesFunctionNames(raw)
	if err == nil {
		t.Fatal("expected empty function tool name to be rejected")
	}
	if !strings.Contains(err.Error(), "tools[0].name") {
		t.Fatalf("error should identify tool name, got %v", err)
	}
}

func TestValidateResponsesFunctionNamesRejectsEmptyNestedToolName(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":"hello",
		"tools":[{"type":"function","function":{"name":" ","parameters":{"type":"object"}}}]
	}`)

	err := ValidateResponsesFunctionNames(raw)
	if err == nil {
		t.Fatal("expected empty nested function tool name to be rejected")
	}
	if !strings.Contains(err.Error(), "tools[0].function.name") {
		t.Fatalf("error should identify nested tool name, got %v", err)
	}
}

func TestValidateResponsesFunctionNamesAllowsValidFunctionNames(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"type":"function_call","call_id":"call_abc","name":"lookup","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_abc","output":"ok"}
		],
		"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]
	}`)

	if err := ValidateResponsesFunctionNames(raw); err != nil {
		t.Fatalf("valid function names should pass, got %v", err)
	}
}

func TestValidateResponsesFunctionNamesIgnoresInvalidJSON(t *testing.T) {
	if err := ValidateResponsesFunctionNames([]byte(`{not json`)); err != nil {
		t.Fatalf("invalid JSON must pass through (upstream validates), got %v", err)
	}
}

// buildManyTurnResponsesBody 模拟 issue #417 的长会话形态：大量小文本轮次。
func buildManyTurnResponsesBody(turns int) []byte {
	var b strings.Builder
	b.WriteString(`{"model":"gpt-5.5","input":[`)
	txt := strings.Repeat("analyze this module carefully ", 40)
	for i := 0; i < turns; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"type":"message","role":"user","content":[{"type":"input_text","text":"`)
		b.WriteString(txt)
		b.WriteString(`"}]}`)
	}
	b.WriteString(`],"tools":[{"type":"function","name":"lookup","parameters":{}}]}`)
	return []byte(b.String())
}

// BenchmarkValidateResponsesFunctionNamesManyTurns 守护 issue #417 的优化：校验器
// 只做惰性 gjson 遍历，不再对大请求体做全量 map[string]any 反序列化。
func BenchmarkValidateResponsesFunctionNamesManyTurns(b *testing.B) {
	body := buildManyTurnResponsesBody(4000)
	b.ReportAllocs()
	b.SetBytes(int64(len(body)))
	for i := 0; i < b.N; i++ {
		if err := ValidateResponsesFunctionNames(body); err != nil {
			b.Fatal(err)
		}
	}
}

func TestPrepareResponsesBody_InfersFunctionToolTypeWhenMissing(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":"hi",
		"tools":[
			{"name":"get_weather","parameters":{"type":"object","properties":{}}},
			{"type":null,"function":{"name":"lookup"}}
		]
	}`)

	got, _ := PrepareResponsesBody(raw)

	first := gjson.GetBytes(got, "tools.0")
	if toolType := first.Get("type").String(); toolType != "function" {
		t.Fatalf("tools.0.type = %q, want function; body=%s", toolType, got)
	}
	if name := first.Get("name").String(); name != "get_weather" {
		t.Fatalf("tools.0.name = %q, want get_weather; body=%s", name, got)
	}
	second := gjson.GetBytes(got, "tools.1")
	if toolType := second.Get("type").String(); toolType != "function" {
		t.Fatalf("tools.1.type = %q, want function; body=%s", toolType, got)
	}
	if name := second.Get("name").String(); name != "lookup" {
		t.Fatalf("tools.1.name = %q, want lookup; body=%s", name, got)
	}
	if second.Get("function").Exists() {
		t.Fatalf("tools.1 nested function object should be removed, got %s", got)
	}
}

func TestPrepareResponsesBody_DropsTypelessUnrecognizedTool(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":"hi",
		"tools":[{"foo":"bar"}]
	}`)

	got, _ := PrepareResponsesBody(raw)

	for _, tool := range gjson.GetBytes(got, "tools").Array() {
		if strings.TrimSpace(tool.Get("type").String()) == "" {
			t.Fatalf("typeless tool should be dropped, got %s", got)
		}
		if tool.Get("foo").Exists() {
			t.Fatalf("unrecognized tool should be dropped, got %s", got)
		}
	}
}

func TestPrepareResponsesBodyNormalizesChatStyleFunctionTool(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":"hello",
		"tools":[{
			"type":"function",
			"function":{
				"name":"lookup",
				"description":"Lookup data",
				"parameters":{"type":"object","properties":{"q":{"type":"string"}}},
				"strict":true
			}
		}]
	}`)

	if err := ValidateResponsesFunctionNames(raw); err != nil {
		t.Fatalf("chat-style tool with nested name should pass validation, got %v", err)
	}

	got, _ := PrepareResponsesBody(raw)
	tool := gjson.GetBytes(got, "tools.0")
	if name := tool.Get("name").String(); name != "lookup" {
		t.Fatalf("nested function name should be promoted, got %q; body=%s", name, got)
	}
	if tool.Get("function").Exists() {
		t.Fatalf("nested function object should be removed after normalization, got %s", got)
	}
	if desc := tool.Get("description").String(); desc != "Lookup data" {
		t.Fatalf("nested function description should be promoted, got %q; body=%s", desc, got)
	}
	if strict := tool.Get("strict"); !strict.Bool() {
		t.Fatalf("nested function strict should be promoted, got %s; body=%s", strict.Raw, got)
	}
}

func TestPrepareResponsesBodyNormalizesChatStyleFunctionToolChoice(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":"hello",
		"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],
		"tool_choice":{
			"type":"function",
			"function":{"name":"lookup"}
		}
	}`)

	got, _ := PrepareResponsesBody(raw)
	choice := gjson.GetBytes(got, "tool_choice")
	if choice.Get("type").String() != "function" {
		t.Fatalf("tool_choice.type = %q, want function; body=%s", choice.Get("type").String(), got)
	}
	if choice.Get("name").String() != "lookup" {
		t.Fatalf("tool_choice.name = %q, want lookup; body=%s", choice.Get("name").String(), got)
	}
	if choice.Get("function").Exists() {
		t.Fatalf("tool_choice nested function object should be removed, got %s", got)
	}
}

func TestPrepareResponsesBodyInfersFunctionToolChoiceTypeWhenMissing(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":"hello",
		"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],
		"tool_choice":{"function":{"name":"lookup"}}
	}`)

	got, _ := PrepareResponsesBody(raw)
	choice := gjson.GetBytes(got, "tool_choice")
	if choice.Get("type").String() != "function" {
		t.Fatalf("tool_choice.type = %q, want function; body=%s", choice.Get("type").String(), got)
	}
	if choice.Get("name").String() != "lookup" {
		t.Fatalf("tool_choice.name = %q, want lookup; body=%s", choice.Get("name").String(), got)
	}
	if choice.Get("function").Exists() {
		t.Fatalf("tool_choice nested function object should be removed, got %s", got)
	}
}

func TestPrepareOpenAIResponsesBodyNormalizesChatStyleFunctionToolChoice(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":"hello",
		"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],
		"tool_choice":{
			"type":"function",
			"function":{"name":"lookup"}
		}
	}`)

	got := PrepareOpenAIResponsesBody(raw)
	choice := gjson.GetBytes(got, "tool_choice")
	if choice.Get("type").String() != "function" {
		t.Fatalf("tool_choice.type = %q, want function; body=%s", choice.Get("type").String(), got)
	}
	if choice.Get("name").String() != "lookup" {
		t.Fatalf("tool_choice.name = %q, want lookup; body=%s", choice.Get("name").String(), got)
	}
	if choice.Get("function").Exists() {
		t.Fatalf("tool_choice nested function object should be removed, got %s", got)
	}
}

func TestPrepareResponsesBody_DefaultsNullMessageContent(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"role":"developer","content":null},
			{"type":"message","role":"assistant","content":null}
		]
	}`)

	got, _ := PrepareResponsesBody(raw)

	for i := 0; i < 2; i++ {
		if content := gjson.GetBytes(got, fmt.Sprintf("input.%d.content", i)); !content.Exists() || content.String() != "" {
			t.Fatalf("null message content at input[%d] should be defaulted, got %s; body=%s", i, content.Raw, got)
		}
	}
}

func TestPrepareResponsesBody_StripsInputItemIDsForStoreFalse(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"type":"reasoning","id":"rs_123","encrypted_content":"gAAAAopaque"},
			{"type":"message","id":"msg_123","role":"user","content":"continue"},
			{"type":"function_call","id":"fc_123","call_id":"call_123","name":"lookup","namespace":"code_tools","arguments":"{}"}
		]
	}`)

	got, expandedInputRaw := PrepareResponsesBody(raw)

	for i := 0; i < 3; i++ {
		if id := gjson.GetBytes(got, fmt.Sprintf("input.%d.id", i)); id.Exists() {
			t.Fatalf("input[%d].id should be stripped for store=false, got %s; body=%s", i, id.Raw, got)
		}
		if id := gjson.Get(expandedInputRaw, fmt.Sprintf("%d.id", i)); id.Exists() {
			t.Fatalf("expanded input[%d].id should be stripped for cache replay, got %s; expanded=%s", i, id.Raw, expandedInputRaw)
		}
	}
	if encrypted := gjson.GetBytes(got, "input.0.encrypted_content").String(); encrypted != "gAAAAopaque" {
		t.Fatalf("reasoning encrypted_content should be preserved, got %q; body=%s", encrypted, got)
	}
	if callID := gjson.GetBytes(got, "input.2.call_id").String(); callID != "call_123" {
		t.Fatalf("function_call call_id should be preserved, got %q; body=%s", callID, got)
	}
	if namespace := gjson.GetBytes(got, "input.2.namespace").String(); namespace != "code_tools" {
		t.Fatalf("function_call namespace should be preserved, got %q; body=%s", namespace, got)
	}
}

func TestPrepareResponsesWebSocketBodyPreservesPreviousResponseID(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"previous_response_id":"resp_123",
		"input":"continue"
	}`)

	got, expandedInputRaw := PrepareResponsesWebSocketBody(raw)

	if prev := gjson.GetBytes(got, "previous_response_id").String(); prev != "resp_123" {
		t.Fatalf("previous_response_id = %q, want resp_123; body=%s", prev, got)
	}
	if store := gjson.GetBytes(got, "store"); store.Exists() {
		t.Fatalf("store should not be forced for websocket continuity, got %s; body=%s", store.Raw, got)
	}
	if !gjson.GetBytes(got, "stream").Bool() {
		t.Fatalf("stream should be true; body=%s", got)
	}
	if content := gjson.Get(expandedInputRaw, "0.content").String(); content != "continue" {
		t.Fatalf("expanded input content = %q, want continue; expanded=%s", content, expandedInputRaw)
	}
}

func TestInvalidEncryptedContentErrorDetection(t *testing.T) {
	body := []byte(`{
		"error":{
			"code":"invalid_encrypted_content",
			"type":"invalid_request_error",
			"message":"The encrypted content gAAA...Vw== could not be verified. Reason: Encrypted content could not be decrypted or parsed."
		}
	}`)

	if !isInvalidEncryptedContentError(http.StatusBadRequest, body) {
		t.Fatalf("expected invalid encrypted content error to be detected")
	}
	if isInvalidEncryptedContentError(http.StatusInternalServerError, body) {
		t.Fatalf("non-400 response should not trigger encrypted content fallback")
	}

	missingBody := []byte(`{
		"error":{
			"message":"Missing required parameter: 'input[8].encrypted_content'.",
			"type":"invalid_request_error",
			"param":"input[8].encrypted_content",
			"code":"missing_required_parameter"
		}
	}`)
	if !isInvalidEncryptedContentError(http.StatusBadRequest, missingBody) {
		t.Fatalf("missing encrypted_content error should trigger encrypted content fallback")
	}
}

func TestIsPreviousResponseNotFoundBody(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    bool
	}{
		{
			name:    "http error body",
			payload: `{"error":{"code":"previous_response_not_found","type":"invalid_request_error","message":"Previous response with id 'resp_1' not found."}}`,
			want:    true,
		},
		{
			name:    "response.failed frame",
			payload: `{"type":"response.failed","response":{"status":"failed","error":{"code":"previous_response_not_found","type":"invalid_request_error","message":"Previous response with id 'resp_1' not found."}}}`,
			want:    true,
		},
		{
			name:    "bare error frame",
			payload: `{"type":"error","code":"previous_response_not_found","message":"Previous response with id 'resp_1' not found."}`,
			want:    true,
		},
		{
			name:    "message only",
			payload: `{"error":{"type":"invalid_request_error","message":"Previous response with id 'resp_1' not found."}}`,
			want:    true,
		},
		{
			name:    "unrelated upstream error",
			payload: `{"error":{"code":"rate_limit_exceeded","type":"rate_limit_error","message":"slow down"}}`,
			want:    false,
		},
		{
			name:    "empty payload",
			payload: ``,
			want:    false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPreviousResponseNotFoundBody([]byte(tc.payload)); got != tc.want {
				t.Fatalf("isPreviousResponseNotFoundBody(%s) = %v, want %v", tc.payload, got, tc.want)
			}
		})
	}
}

func TestStripInvalidEncryptedContentFromResponsesBody(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"type":"message","role":"user","content":"hello"},
			{"type":"reasoning","id":"rs_bad","encrypted_content":"gAAA"},
			{"type":"function_call","call_id":"call_123","name":"lookup","arguments":"{}"}
		]
	}`)

	got, changed := stripInvalidEncryptedContentFromResponsesBody(raw)
	if !changed {
		t.Fatalf("expected body to be changed")
	}
	items := gjson.GetBytes(got, "input").Array()
	if len(items) != 2 {
		t.Fatalf("expected reasoning item to be removed, got %d items: %s", len(items), got)
	}
	if typ := gjson.GetBytes(got, "input.0.type").String(); typ != "message" {
		t.Fatalf("first input should remain message, got %q; body=%s", typ, got)
	}
	if typ := gjson.GetBytes(got, "input.1.type").String(); typ != "function_call" {
		t.Fatalf("function call should remain, got %q; body=%s", typ, got)
	}
	if strings.Contains(string(got), "encrypted_content") {
		t.Fatalf("encrypted_content should be removed from retry body: %s", got)
	}
}

// 压缩项的 encrypted_content 是必填字段：只摘字段会留下 {"type":"compaction"}
// 空壳，上游转而以 missing_required_parameter 再拒一次，而重试闸已经用掉。
// 带密文的压缩项必须整项丢弃，与 reasoning 分支对称。
func TestStripInvalidEncryptedContentDropsEncryptedCompactionItems(t *testing.T) {
	for _, itemType := range []string{"compaction", "context_compaction", "compaction_summary"} {
		t.Run(itemType, func(t *testing.T) {
			raw := []byte(`{
				"model":"gpt-5.4",
				"input":[
					{"type":"message","role":"user","content":"hello"},
					{"type":"` + itemType + `","encrypted_content":"gAAA","summary":"stale"},
					{"type":"function_call","call_id":"call_123","name":"lookup","arguments":"{}"}
				]
			}`)

			got, changed := stripInvalidEncryptedContentFromResponsesBody(raw)
			if !changed {
				t.Fatalf("expected body to be changed")
			}
			items := gjson.GetBytes(got, "input").Array()
			if len(items) != 2 {
				t.Fatalf("expected encrypted %s item to be dropped whole, got %d items: %s", itemType, len(items), got)
			}
			// 不能留下空壳：改写后不应再出现该 type。
			if strings.Contains(string(got), itemType) {
				t.Fatalf("%s shell survived the strip: %s", itemType, got)
			}
			if strings.Contains(string(got), "encrypted_content") {
				t.Fatalf("encrypted_content should be gone: %s", got)
			}
			if typ := gjson.GetBytes(got, "input.1.type").String(); typ != "function_call" {
				t.Fatalf("function call should remain, got %q; body=%s", typ, got)
			}
		})
	}
}

// 不带密文的压缩项没有账号绑定，原样保留（避免误伤正常压缩历史）。
func TestStripInvalidEncryptedContentKeepsPlainCompactionItems(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"type":"compaction","summary":"plain compaction, no ciphertext"},
			{"type":"reasoning","id":"rs_bad","encrypted_content":"gAAA"}
		]
	}`)

	got, changed := stripInvalidEncryptedContentFromResponsesBody(raw)
	if !changed {
		t.Fatalf("expected body to be changed by the reasoning item")
	}
	items := gjson.GetBytes(got, "input").Array()
	if len(items) != 1 {
		t.Fatalf("plain compaction should survive, got %d items: %s", len(items), got)
	}
	if typ := gjson.GetBytes(got, "input.0.type").String(); typ != "compaction" {
		t.Fatalf("input.0.type = %q, want compaction preserved; body=%s", typ, got)
	}
	if summary := gjson.GetBytes(got, "input.0.summary").String(); summary == "" {
		t.Fatalf("plain compaction summary should be untouched; body=%s", got)
	}
}

// ==================== Function Calling 测试 ====================

func TestStripInvalidEncryptedContentFromResponsesBodyDropsBareReasoning(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"type":"message","role":"user","content":"hello"},
			{"type":"reasoning","summary":[{"type":"summary_text","text":"stale"}]},
			{"type":"reasoning"},
			{"type":"function_call","call_id":"call_123","name":"lookup","arguments":"{}"}
		]
	}`)

	got, changed := stripInvalidEncryptedContentFromResponsesBody(raw)
	if !changed {
		t.Fatalf("expected body to be changed")
	}
	items := gjson.GetBytes(got, "input").Array()
	if len(items) != 2 {
		t.Fatalf("expected bare reasoning items to be removed, got %d items: %s", len(items), got)
	}
	if typ := gjson.GetBytes(got, "input.0.type").String(); typ != "message" {
		t.Fatalf("first input should remain message, got %q; body=%s", typ, got)
	}
	if typ := gjson.GetBytes(got, "input.1.type").String(); typ != "function_call" {
		t.Fatalf("function call should remain, got %q; body=%s", typ, got)
	}
}

func TestPrepareResponsesBodyDropsBareReasoningItems(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"type":"message","role":"user","content":"hello"},
			{"type":"reasoning","summary":[{"type":"summary_text","text":"stale"}]},
			{"type":"reasoning"},
			{"type":"reasoning","encrypted_content":"gAAAAopaque"},
			{"type":"function_call","call_id":"call_123","name":"lookup","arguments":"{}"}
		]
	}`)

	got, expandedInputRaw := PrepareResponsesBody(raw)

	// 无 output 的 function_call 会被配对修复补一条占位 output，
	// 因此裸 reasoning 清理后剩 3 条原始项 + 1 条合成 output。
	items := gjson.GetBytes(got, "input").Array()
	if len(items) != 4 {
		t.Fatalf("expected bare reasoning items to be dropped before upstream, got %d items: %s", len(items), got)
	}
	if typ := gjson.GetBytes(got, "input.0.type").String(); typ != "message" {
		t.Fatalf("first input should remain message, got %q; body=%s", typ, got)
	}
	if encrypted := gjson.GetBytes(got, "input.1.encrypted_content").String(); encrypted != "gAAAAopaque" {
		t.Fatalf("encrypted reasoning should be preserved, got %q; body=%s", encrypted, got)
	}
	if typ := gjson.GetBytes(got, "input.2.type").String(); typ != "function_call" {
		t.Fatalf("function call should remain, got %q; body=%s", typ, got)
	}
	if typ := gjson.GetBytes(got, "input.3.type").String(); typ != "function_call_output" {
		t.Fatalf("orphan function call should get a placeholder output, got %q; body=%s", typ, got)
	}
	if strings.Contains(expandedInputRaw, `"summary_text"`) {
		t.Fatalf("expanded input cache should use cleaned input, got %s", expandedInputRaw)
	}
}

func TestPrepareResponsesBodyDropsForeignReasoningAndStripsStatus(t *testing.T) {
	// 跨渠道会话（issue #565）：Grok 轮输出的 reasoning 项带外渠道密文
	// （裸 base64，非 Fernet gAAAA 前缀）与 status 字段，裸回灌给 Codex 上游
	// 会依次触发 400 unknown_parameter(input[N].status) 与
	// invalid_encrypted_content。前者须剥离、后者须整项丢弃。
	raw := []byte(`{
		"model":"gpt-5.6",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"plan"}]},
			{"id":"rs_grok","type":"reasoning","summary":[{"type":"summary_text","text":"grok thoughts"}],"encrypted_content":"uFvR4+NBforeign","status":"completed"},
			{"id":"msg_grok","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"the plan"}]},
			{"id":"rs_codex","type":"reasoning","summary":[],"encrypted_content":"gAAAABnative","status":"completed"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"execute"}]}
		]
	}`)

	got, _ := PrepareResponsesBody(raw)

	items := gjson.GetBytes(got, "input").Array()
	if len(items) != 4 {
		t.Fatalf("expected foreign reasoning to be dropped, got %d items: %s", len(items), got)
	}
	for i, item := range items {
		if item.Get("type").String() == "reasoning" && item.Get("status").Exists() {
			t.Fatalf("input[%d] reasoning status should be stripped: %s", i, item.Raw)
		}
		if strings.Contains(item.Raw, "uFvR4+NBforeign") {
			t.Fatalf("foreign encrypted_content should not survive: %s", item.Raw)
		}
	}
	if encrypted := gjson.GetBytes(got, "input.2.encrypted_content").String(); encrypted != "gAAAABnative" {
		t.Fatalf("codex-native reasoning should be preserved, got %q; body=%s", encrypted, got)
	}
	// 非 reasoning 项的 status 上游可接受，保持原样不做多余改写。
	if status := gjson.GetBytes(got, "input.1.status").String(); status != "completed" {
		t.Fatalf("assistant message status should be untouched, got %q; body=%s", status, got)
	}
}

func TestConvertMessagesToInput_ToolRole(t *testing.T) {
	raw := []byte(`{
		"messages":[
			{"role":"assistant","content":null,"tool_calls":[{"id":"call_abc","type":"function","function":{"name":"weather","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_abc","content":"{\"temp\":72}"}
		]
	}`)
	got, err := TranslateRequest(raw)
	if err != nil {
		t.Fatal(err)
	}

	input := gjson.GetBytes(got, "input")
	if !input.IsArray() {
		t.Fatal("input should be an array")
	}

	item := input.Array()[1]
	if item.Get("type").String() != "function_call_output" {
		t.Fatalf("expected type function_call_output, got %q", item.Get("type").String())
	}
	if item.Get("call_id").String() != "call_abc" {
		t.Fatalf("expected call_id call_abc, got %q", item.Get("call_id").String())
	}
	if item.Get("output").String() != `{"temp":72}` {
		t.Fatalf("expected output to match, got %q", item.Get("output").String())
	}
}

func TestConvertMessagesToInput_AssistantWithToolCalls(t *testing.T) {
	raw := []byte(`{
		"messages":[
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_123","type":"function","function":{"name":"get_weather","arguments":"{\"city\":\"NYC\"}"}}
			]}
		]
	}`)
	got, err := TranslateRequest(raw)
	if err != nil {
		t.Fatal(err)
	}

	input := gjson.GetBytes(got, "input")
	items := input.Array()
	if len(items) != 1 {
		t.Fatalf("expected 1 input item, got %d", len(items))
	}

	fc := items[0]
	if fc.Get("type").String() != "function_call" {
		t.Fatalf("expected type function_call, got %q", fc.Get("type").String())
	}
	if fc.Get("call_id").String() != "call_123" {
		t.Fatalf("expected call_id call_123, got %q", fc.Get("call_id").String())
	}
	if fc.Get("name").String() != "get_weather" {
		t.Fatalf("expected name get_weather, got %q", fc.Get("name").String())
	}
	if fc.Get("arguments").String() != `{"city":"NYC"}` {
		t.Fatalf("expected arguments to match, got %q", fc.Get("arguments").String())
	}
}

func TestTranslateRequestRejectsEmptyToolCallName(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"messages":[
			{"role":"user","content":"Call a tool"},
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_123","type":"function","function":{"name":"","arguments":"{}"}}
			]}
		]
	}`)

	_, err := TranslateRequest(raw)
	if err == nil {
		t.Fatal("expected empty tool call name to be rejected")
	}
	if !strings.Contains(err.Error(), "messages[1].tool_calls[0].function.name") {
		t.Fatalf("error should identify tool call name, got %v", err)
	}
}

func TestTranslateRequestRejectsEmptyFunctionToolName(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"messages":[{"role":"user","content":"hello"}],
		"tools":[
			{"type":"function","function":{"name":" ","description":"bad","parameters":{"type":"object"}}}
		]
	}`)

	_, err := TranslateRequest(raw)
	if err == nil {
		t.Fatal("expected empty function tool name to be rejected")
	}
	if !strings.Contains(err.Error(), "tools[0].function.name") {
		t.Fatalf("error should identify function tool name, got %v", err)
	}
}

func TestConvertMessagesToInput_FullMultiTurn(t *testing.T) {
	raw := []byte(`{
		"messages":[
			{"role":"user","content":"What is the weather in NYC?"},
			{"role":"assistant","content":null,"tool_calls":[
				{"id":"call_001","type":"function","function":{"name":"get_weather","arguments":"{\"location\":\"NYC\"}"}}
			]},
			{"role":"tool","tool_call_id":"call_001","content":"{\"temperature\":72}"},
			{"role":"user","content":"Thanks!"}
		]
	}`)
	got, err := TranslateRequest(raw)
	if err != nil {
		t.Fatal(err)
	}

	input := gjson.GetBytes(got, "input")
	items := input.Array()
	if len(items) != 4 {
		t.Fatalf("expected 4 input items, got %d", len(items))
	}

	// 用户消息
	if items[0].Get("type").String() != "message" || items[0].Get("role").String() != "user" {
		t.Fatal("first item should be user message")
	}
	// function_call
	if items[1].Get("type").String() != "function_call" {
		t.Fatalf("second item should be function_call, got %q", items[1].Get("type").String())
	}
	// function_call_output
	if items[2].Get("type").String() != "function_call_output" {
		t.Fatalf("third item should be function_call_output, got %q", items[2].Get("type").String())
	}
	// 用户消息
	if items[3].Get("type").String() != "message" || items[3].Get("role").String() != "user" {
		t.Fatal("fourth item should be user message")
	}
}

func TestStreamTranslator_FunctionCall(t *testing.T) {
	st := NewStreamTranslator("chatcmpl-test", "gpt-5.4", 0)

	// 1. output_item.added: function_call
	addedEvent := []byte(`{
		"type":"response.output_item.added",
		"output_index":0,
		"item":{"type":"function_call","id":"fc_001","call_id":"call_abc","name":"get_weather","arguments":"","status":"in_progress"}
	}`)
	chunk, done := st.Translate(addedEvent)
	if done {
		t.Fatal("should not be done after output_item.added")
	}
	if chunk == nil {
		t.Fatal("should emit chunk for function_call added")
	}
	// 验证首块包含 tool_calls
	tc := gjson.GetBytes(chunk, "choices.0.delta.tool_calls.0")
	if tc.Get("id").String() != "call_abc" {
		t.Fatalf("expected call_id call_abc, got %q", tc.Get("id").String())
	}
	if tc.Get("function.name").String() != "get_weather" {
		t.Fatalf("expected function name get_weather, got %q", tc.Get("function.name").String())
	}
	if tc.Get("index").Int() != 0 {
		t.Fatalf("expected index 0, got %d", tc.Get("index").Int())
	}

	// 2. function_call_arguments.delta
	deltaEvent := []byte(`{
		"type":"response.function_call_arguments.delta",
		"item_id":"fc_001",
		"output_index":0,
		"delta":"{\"city\":"
	}`)
	chunk, done = st.Translate(deltaEvent)
	if done {
		t.Fatal("should not be done after arguments delta")
	}
	if chunk == nil {
		t.Fatal("should emit chunk for arguments delta")
	}
	argsDelta := gjson.GetBytes(chunk, "choices.0.delta.tool_calls.0.function.arguments").String()
	if argsDelta != `{"city":` {
		t.Fatalf("expected arguments delta, got %q", argsDelta)
	}

	// 3. function_call_arguments.done
	doneEvent := []byte(`{
		"type":"response.function_call_arguments.done",
		"item_id":"fc_001",
		"output_index":0,
		"arguments":"{\"city\":\"NYC\"}"
	}`)
	chunk, done = st.Translate(doneEvent)
	if done || chunk == nil {
		t.Fatal("function_call_arguments.done should emit the canonical missing suffix")
	}
	if suffix := gjson.GetBytes(chunk, "choices.0.delta.tool_calls.0.function.arguments").String(); suffix != `"NYC"}` {
		t.Fatalf("canonical arguments suffix = %q, want %q", suffix, `"NYC"}`)
	}

	// 4. response.completed
	completedEvent := []byte(`{
		"type":"response.completed",
		"response":{
			"usage":{"input_tokens":10,"output_tokens":5},
			"output":[{"type":"function_call","call_id":"call_abc","name":"get_weather","arguments":"{\"city\":\"NYC\"}"}]
		}
	}`)
	chunk, done = st.Translate(completedEvent)
	if !done {
		t.Fatal("should be done after response.completed")
	}
	if chunk == nil {
		t.Fatal("should emit final chunk")
	}
	finishReason := gjson.GetBytes(chunk, "choices.0.finish_reason").String()
	if finishReason != "tool_calls" {
		t.Fatalf("expected finish_reason tool_calls, got %q", finishReason)
	}

	if !st.HasToolCalls {
		t.Fatal("HasToolCalls should be true")
	}
}

func TestStreamTranslator_CustomToolCallInputDelta(t *testing.T) {
	st := NewStreamTranslator("chatcmpl-test", "gpt-5.4", 0)

	addedEvent := []byte(`{
		"type":"response.output_item.added",
		"output_index":0,
		"item":{"type":"custom_tool_call","id":"ctc_001","call_id":"call_custom","name":"run_custom"}
	}`)
	chunk, done := st.Translate(addedEvent)
	if done {
		t.Fatal("should not be done after custom_tool_call added")
	}
	if chunk == nil {
		t.Fatal("should emit chunk for custom_tool_call added")
	}
	if got := gjson.GetBytes(chunk, "choices.0.delta.tool_calls.0.id").String(); got != "call_custom" {
		t.Fatalf("tool call id = %q, want call_custom; chunk=%s", got, chunk)
	}
	if got := gjson.GetBytes(chunk, "choices.0.delta.tool_calls.0.function.name").String(); got != "run_custom" {
		t.Fatalf("tool call name = %q, want run_custom; chunk=%s", got, chunk)
	}

	deltaEvent := []byte(`{
		"type":"response.custom_tool_call_input.delta",
		"item_id":"ctc_001",
		"delta":"{\"cmd\":"
	}`)
	chunk, done = st.Translate(deltaEvent)
	if done {
		t.Fatal("should not be done after custom_tool_call_input delta")
	}
	if chunk == nil {
		t.Fatal("should emit chunk for custom_tool_call_input delta")
	}
	if got := gjson.GetBytes(chunk, "choices.0.delta.tool_calls.0.function.arguments").String(); got != `{"cmd":` {
		t.Fatalf("custom tool input delta = %q, want arguments delta; chunk=%s", got, chunk)
	}

	callIDDeltaEvent := []byte(`{
		"type":"response.custom_tool_call_input.delta",
		"call_id":"call_custom",
		"delta":"\"pwd\"}"
	}`)
	chunk, done = st.Translate(callIDDeltaEvent)
	if done {
		t.Fatal("should not be done after custom_tool_call_input call_id delta")
	}
	if chunk == nil {
		t.Fatal("should emit chunk for custom_tool_call_input call_id delta")
	}
	if got := gjson.GetBytes(chunk, "choices.0.delta.tool_calls.0.function.arguments").String(); got != `"pwd"}` {
		t.Fatalf("custom tool input call_id delta = %q, want arguments delta; chunk=%s", got, chunk)
	}

	completedEvent := []byte(`{
		"type":"response.completed",
		"response":{"usage":{"input_tokens":10,"output_tokens":5}}
	}`)
	chunk, done = st.Translate(completedEvent)
	if !done {
		t.Fatal("should be done after response.completed")
	}
	if finishReason := gjson.GetBytes(chunk, "choices.0.finish_reason").String(); finishReason != "tool_calls" {
		t.Fatalf("expected finish_reason tool_calls, got %q", finishReason)
	}
}

// 终结块必须带 "delta":{}(OpenAI 官方形状)。缺失会让 Rust/serde 系客户端
// 报 "missing field `delta`" 并整轮失败。
func TestFinalChunk_AlwaysCarriesEmptyDeltaObject(t *testing.T) {
	assertDelta := func(t *testing.T, label string, chunk []byte) {
		t.Helper()
		delta := gjson.GetBytes(chunk, "choices.0.delta")
		if !delta.Exists() {
			t.Fatalf("%s: final chunk missing choices.0.delta; chunk=%s", label, chunk)
		}
		if !delta.IsObject() {
			t.Fatalf("%s: choices.0.delta is not an object; chunk=%s", label, chunk)
		}
	}

	// 无状态翻译:文本轮结束
	statelessChunk, done := TranslateStreamChunk(
		[]byte(`{"type":"response.completed","response":{"usage":{"input_tokens":5,"output_tokens":3}}}`),
		"grok-4.5", "chatcmpl-test", 0,
	)
	if !done {
		t.Fatal("stateless: response.completed should be terminal")
	}
	assertDelta(t, "stateless", statelessChunk)

	// 有状态翻译:纯文本轮(finish_reason=stop)
	stText := NewStreamTranslator("chatcmpl-test", "grok-4.5", 0)
	stText.Translate([]byte(`{"type":"response.output_text.delta","delta":"Hello"}`))
	textChunk, done := stText.Translate([]byte(`{"type":"response.completed","response":{"usage":{"input_tokens":5,"output_tokens":3}}}`))
	if !done {
		t.Fatal("stateful text: response.completed should be terminal")
	}
	assertDelta(t, "stateful text", textChunk)

	// 有状态翻译:工具调用轮(finish_reason=tool_calls)
	stTool := NewStreamTranslator("chatcmpl-test", "grok-4.5", 0)
	stTool.Translate([]byte(`{
		"type":"response.output_item.added",
		"output_index":0,
		"item":{"type":"function_call","id":"fc_001","call_id":"call_1","name":"func_a","arguments":""}
	}`))
	toolChunk, done := stTool.Translate([]byte(`{"type":"response.completed","response":{"usage":{"input_tokens":10,"output_tokens":5}}}`))
	if !done {
		t.Fatal("stateful tool: response.completed should be terminal")
	}
	if got := gjson.GetBytes(toolChunk, "choices.0.finish_reason").String(); got != "tool_calls" {
		t.Fatalf("stateful tool: finish_reason = %q, want tool_calls", got)
	}
	assertDelta(t, "stateful tool", toolChunk)
}

func TestChatStreamTranslationDistinguishesFailedTerminal(t *testing.T) {
	failed := NewStreamTranslator("chatcmpl-test", "grok-4.5", 0).TranslateParsedResult(gjson.Parse(
		`{"type":"response.failed","response":{"error":{"message":"boom"}}}`,
	))
	if !failed.Terminal || !failed.Failed || !gjson.GetBytes(failed.Chunk, "error").Exists() {
		t.Fatalf("failed translation = %#v, chunk=%s", failed, failed.Chunk)
	}

	completed := NewStreamTranslator("chatcmpl-test", "grok-4.5", 0).TranslateParsedResult(gjson.Parse(
		`{"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1}}}`,
	))
	if !completed.Terminal || completed.Failed {
		t.Fatalf("completed translation = %#v", completed)
	}
}

func TestStreamTranslatorRejectsMalformedFunctionArgumentsBeforeSuccess(t *testing.T) {
	st := NewStreamTranslator("chatcmpl-test", "gpt-5.4", 0)
	st.Translate([]byte(`{
		"type":"response.output_item.added",
		"output_index":3,
		"item":{"type":"function_call","id":"fc_bad","call_id":"call_bad","name":"exec","arguments":""}
	}`))
	st.Translate([]byte(`{"type":"response.function_call_arguments.delta","output_index":3,"delta":"{\"cmd\":\"unterminated"}`))

	result := st.TranslateParsedResult(gjson.Parse(
		`{"type":"response.function_call_arguments.done","output_index":3,"arguments":"{\"cmd\":\"unterminated"}`,
	))
	if !result.Terminal || !result.Failed {
		t.Fatalf("malformed done result = %#v", result)
	}
	if !gjson.GetBytes(result.Chunk, "error").Exists() || st.ToolArgumentsError() == nil {
		t.Fatalf("malformed call should produce an explicit protocol error: %s", result.Chunk)
	}
}

func TestStreamTranslatorKeepsValidToolCallAtOutputLimit(t *testing.T) {
	st := NewStreamTranslator("chatcmpl-test", "gpt-5.4", 0)
	st.Translate([]byte(`{
		"type":"response.output_item.added",
		"output_index":2,
		"item":{"type":"function_call","id":"fc_ok","call_id":"call_ok","name":"exec","arguments":""}
	}`))
	st.Translate([]byte(`{"type":"response.function_call_arguments.delta","output_index":2,"delta":"{\"cmd\":\"ls\"}"}`))
	chunk, done := st.Translate([]byte(`{"type":"response.function_call_arguments.done","output_index":2,"arguments":"{\"cmd\":\"ls\"}"}`))
	if done || chunk != nil {
		t.Fatalf("matching done event should not duplicate arguments: done=%v chunk=%s", done, chunk)
	}

	result := st.TranslateParsedResult(gjson.Parse(
		`{"type":"response.incomplete","response":{"incomplete_details":{"reason":"max_output_tokens"}}}`,
	))
	if !result.Terminal || result.Failed {
		t.Fatalf("valid tool call at limit should remain a successful incomplete result: %#v", result)
	}
	if reason := gjson.GetBytes(result.Chunk, "choices.0.finish_reason").String(); reason != "length" {
		t.Fatalf("finish_reason = %q, want length; chunk=%s", reason, result.Chunk)
	}
}

func TestStreamTranslatorEmitsCanonicalArgumentsMissingFromDeltas(t *testing.T) {
	st := NewStreamTranslator("chatcmpl-test", "gpt-5.4", 0)
	st.Translate([]byte(`{
		"type":"response.output_item.added",
		"output_index":0,
		"item":{"type":"function_call","id":"fc_ok","call_id":"call_ok","name":"exec","arguments":""}
	}`))
	chunk, done := st.Translate([]byte(`{"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","id":"fc_ok","call_id":"call_ok","name":"exec","arguments":"{}"}}`))
	if done {
		t.Fatal("output_item.done should not terminate the response")
	}
	if args := gjson.GetBytes(chunk, "choices.0.delta.tool_calls.0.function.arguments").String(); args != "{}" {
		t.Fatalf("canonical missing arguments delta = %q, want {}; chunk=%s", args, chunk)
	}
}

func TestStreamTranslator_TextOnly(t *testing.T) {
	st := NewStreamTranslator("chatcmpl-test", "gpt-5.4", 0)

	// 文本 delta
	textEvent := []byte(`{"type":"response.output_text.delta","delta":"Hello"}`)
	chunk, done := st.Translate(textEvent)
	if done {
		t.Fatal("should not be done")
	}
	if chunk == nil {
		t.Fatal("should emit chunk")
	}
	if gjson.GetBytes(chunk, "choices.0.delta.content").String() != "Hello" {
		t.Fatal("content mismatch")
	}

	// completed
	completedEvent := []byte(`{"type":"response.completed","response":{"usage":{"input_tokens":5,"output_tokens":3}}}`)
	chunk, done = st.Translate(completedEvent)
	if !done {
		t.Fatal("should be done")
	}
	finishReason := gjson.GetBytes(chunk, "choices.0.finish_reason").String()
	if finishReason != "stop" {
		t.Fatalf("expected finish_reason stop for text-only, got %q", finishReason)
	}

	if st.HasToolCalls {
		t.Fatal("HasToolCalls should be false for text-only")
	}
}

func TestStreamTranslator_CachedTokenDetails(t *testing.T) {
	st := NewStreamTranslator("chatcmpl-test", "gpt-5.4", 0)

	completedEvent := []byte(`{
		"type":"response.completed",
		"response":{
			"usage":{
				"input_tokens":12,
				"output_tokens":4,
				"input_tokens_details":{"cached_tokens":7}
			}
		}
	}`)

	chunk, done := st.Translate(completedEvent)
	if !done {
		t.Fatal("should be done")
	}
	if got := gjson.GetBytes(chunk, "usage.cached_tokens").Int(); got != 7 {
		t.Fatalf("usage.cached_tokens = %d, want 7; chunk=%s", got, chunk)
	}
	if got := gjson.GetBytes(chunk, "usage.prompt_tokens_details.cached_tokens").Int(); got != 7 {
		t.Fatalf("usage.prompt_tokens_details.cached_tokens = %d, want 7; chunk=%s", got, chunk)
	}
	if got := gjson.GetBytes(chunk, "usage.input_tokens_details.cached_tokens").Int(); got != 7 {
		t.Fatalf("usage.input_tokens_details.cached_tokens = %d, want 7; chunk=%s", got, chunk)
	}
}

func TestStreamTranslator_MultipleFunctionCalls(t *testing.T) {
	st := NewStreamTranslator("chatcmpl-test", "gpt-5.4", 0)

	// 第一个 function call
	event1 := []byte(`{
		"type":"response.output_item.added",
		"output_index":0,
		"item":{"type":"function_call","id":"fc_001","call_id":"call_1","name":"func_a","arguments":""}
	}`)
	chunk, _ := st.Translate(event1)
	if gjson.GetBytes(chunk, "choices.0.delta.tool_calls.0.index").Int() != 0 {
		t.Fatal("first tool call should have index 0")
	}

	// 第二个 function call
	event2 := []byte(`{
		"type":"response.output_item.added",
		"output_index":1,
		"item":{"type":"function_call","id":"fc_002","call_id":"call_2","name":"func_b","arguments":""}
	}`)
	chunk, _ = st.Translate(event2)
	if gjson.GetBytes(chunk, "choices.0.delta.tool_calls.0.index").Int() != 1 {
		t.Fatal("second tool call should have index 1")
	}

	if st.nextIdx != 2 {
		t.Fatalf("expected nextIdx 2, got %d", st.nextIdx)
	}
}

func TestExtractToolCallsFromOutput(t *testing.T) {
	event := []byte(`{
		"type":"response.completed",
		"response":{
			"output":[
				{"type":"message","content":[{"type":"output_text","text":"hi"}]},
				{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"NYC\"}"},
				{"type":"function_call","call_id":"call_2","name":"get_time","arguments":"{}"},
				{"type":"custom_tool_call","id":"call_3","name":"custom_exec","input":"{\"cmd\":\"pwd\"}"}
			],
			"usage":{"input_tokens":10,"output_tokens":5}
		}
	}`)

	tcs := ExtractToolCallsFromOutput(event)
	if len(tcs) != 3 {
		t.Fatalf("expected 3 tool calls, got %d", len(tcs))
	}
	if tcs[0].ID != "call_1" || tcs[0].Name != "get_weather" {
		t.Fatalf("first tool call mismatch: %+v", tcs[0])
	}
	if tcs[1].ID != "call_2" || tcs[1].Name != "get_time" {
		t.Fatalf("second tool call mismatch: %+v", tcs[1])
	}
	if tcs[2].ID != "call_3" || tcs[2].Name != "custom_exec" || tcs[2].Arguments != `{"cmd":"pwd"}` {
		t.Fatalf("custom tool call mismatch: %+v", tcs[2])
	}
}

func TestExtractToolCallsFromOutputValidatedRejectsMalformedOrdinaryCall(t *testing.T) {
	event := []byte(`{"type":"response.completed","response":{"output":[
		{"type":"function_call","call_id":"call_bad","name":"exec","arguments":"{\"cmd\":"},
		{"type":"custom_tool_call","call_id":"call_custom","name":"raw","input":"not-json"}
	]}}`)

	if calls, err := ExtractToolCallsFromOutputValidated(event); err == nil || calls != nil {
		t.Fatalf("calls=%v err=%v, want malformed ordinary call error", calls, err)
	}
}

func TestMalformedToolArgumentsFailurePayloadCarriesCreatedAt(t *testing.T) {
	payload := malformedToolArgumentsFailurePayload(errors.New("bad arguments"))
	if got := gjson.GetBytes(payload, "type").String(); got != "response.failed" {
		t.Fatalf("type = %q, want response.failed; payload=%s", got, payload)
	}
	if got := gjson.GetBytes(payload, "response.created_at").Int(); got <= 0 {
		t.Fatalf("response.created_at = %d, want a positive Unix timestamp", got)
	}
}

func TestExtractToolCallsFromOutputValidatedNormalizesBlankAndKeepsCustom(t *testing.T) {
	event := []byte(`{"type":"response.completed","response":{"output":[
		{"type":"function_call","call_id":"call_empty","name":"noop","arguments":""},
		{"type":"custom_tool_call","call_id":"call_custom","name":"raw","input":"not-json"}
	]}}`)

	calls, err := ExtractToolCallsFromOutputValidated(event)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0].Arguments != "{}" || calls[1].Arguments != "not-json" {
		t.Fatalf("validated calls mismatch: %+v", calls)
	}
}

// issue #330：上游要求 tool_search_call.arguments 为 object、function_call.arguments
// 为 string；回放历史时类型不符会被上游 400 拒绝，需在发送前修正。
func TestNormalizeResponsesToolCallArgumentTypes(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.5",
		"input":[
			{"type":"tool_search_call","call_id":"c1","arguments":"{\"queries\":[\"weather\"]}"},
			{"type":"tool_search_call","call_id":"c2","arguments":""},
			{"type":"tool_search_call","call_id":"c3","arguments":{"queries":["ok"]}},
			{"type":"function_call","call_id":"c4","name":"get_weather","arguments":{"city":"NYC"}},
			{"type":"function_call","call_id":"c5","name":"get_time","arguments":"{}"},
			{"type":"tool_search_call","call_id":"c6","arguments":"not-json"},
			{"type":"message","role":"user","content":"hi"}
		]
	}`)

	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if !normalizeResponsesToolCallArgumentTypes(body) {
		t.Fatal("expected modification, got false")
	}

	got, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	items := gjson.GetBytes(got, "input").Array()

	if !items[0].Get("arguments").IsObject() {
		t.Fatalf("tool_search_call string arguments should be parsed to object: %s", items[0].Raw)
	}
	if items[0].Get("arguments.queries.0").String() != "weather" {
		t.Fatalf("parsed arguments should keep content: %s", items[0].Raw)
	}
	if !items[1].Get("arguments").IsObject() {
		t.Fatalf("empty string arguments should become empty object: %s", items[1].Raw)
	}
	if !items[2].Get("arguments").IsObject() {
		t.Fatalf("object arguments should stay object: %s", items[2].Raw)
	}
	if items[3].Get("arguments").Type != gjson.String {
		t.Fatalf("function_call object arguments should be serialized to string: %s", items[3].Raw)
	}
	if items[3].Get("arguments").String() != `{"city":"NYC"}` {
		t.Fatalf("serialized arguments should keep content, got %q", items[3].Get("arguments").String())
	}
	if items[4].Get("arguments").String() != "{}" {
		t.Fatalf("function_call string arguments should be untouched: %s", items[4].Raw)
	}
	if items[5].Get("arguments").String() != "not-json" {
		t.Fatalf("unparseable tool_search_call arguments should be left as-is: %s", items[5].Raw)
	}
}

func TestPrepareResponsesBodyDropsMalformedOrdinaryFunctionCallPair(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"type":"function_call","call_id":"bad","name":"exec","arguments":"{\"cmd\":"},
			{"type":"function_call_output","call_id":"bad","output":"poison"},
			{"type":"function_call","call_id":"empty","name":"noop","arguments":""},
			{"type":"function_call_output","call_id":"empty","output":"ok"},
			{"type":"custom_tool_call","call_id":"custom","name":"shell","input":"not-json"},
			{"type":"custom_tool_call_output","call_id":"custom","output":"custom-ok"},
			{"type":"tool_search_call","call_id":"search","arguments":{"query":"weather"}},
			{"type":"tool_search_call_output","call_id":"search","output":"sunny"}
		]
	}`)

	got, _ := PrepareResponsesBody(raw)
	if strings.Contains(string(got), `"call_id":"bad"`) || strings.Contains(string(got), "poison") {
		t.Fatalf("malformed ordinary function call pair survived: %s", got)
	}
	items := gjson.GetBytes(got, "input").Array()
	if len(items) != 6 {
		t.Fatalf("input count = %d, want 6; body=%s", len(items), got)
	}
	if args := items[0].Get("arguments").String(); args != "{}" {
		t.Fatalf("blank ordinary function arguments = %q, want {}", args)
	}
	if input := items[2].Get("input").String(); input != "not-json" {
		t.Fatalf("custom tool free-form input was changed: %s", items[2].Raw)
	}
	if !items[4].Get("arguments").IsObject() {
		t.Fatalf("tool_search arguments should remain an object: %s", items[4].Raw)
	}
}

func TestNormalizeResponsesCompactionItemsConvertsToDeveloperMessage(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"type":"message","role":"user","content":"hello from earlier"},
			{"type":"compaction","summary":"用户问候并讨论了 X 主题"},
			{"type":"message","role":"user","content":"继续上面的话题"}
		]
	}`)

	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if !normalizeResponsesCompactionItems(body) {
		t.Fatal("expected modification, got false")
	}

	got, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	items := gjson.GetBytes(got, "input").Array()
	if len(items) != 3 {
		t.Fatalf("expected 3 items, got %d: %s", len(items), got)
	}

	if items[0].Get("type").String() != "message" || items[0].Get("role").String() != "user" {
		t.Fatalf("item 0 should be untouched user message: %s", items[0].Raw)
	}
	if items[2].Get("type").String() != "message" || items[2].Get("role").String() != "user" {
		t.Fatalf("item 2 should be untouched user message: %s", items[2].Raw)
	}

	converted := items[1]
	if converted.Get("type").String() != "message" {
		t.Fatalf("compaction item should become message, got type=%q", converted.Get("type").String())
	}
	if converted.Get("role").String() != "developer" {
		t.Fatalf("compaction item should use developer role, got %q", converted.Get("role").String())
	}
	contentParts := converted.Get("content").Array()
	if len(contentParts) != 1 {
		t.Fatalf("expected 1 content part, got %d: %s", len(contentParts), converted.Raw)
	}
	if contentParts[0].Get("type").String() != "input_text" {
		t.Fatalf("content part type should be input_text, got %q", contentParts[0].Get("type").String())
	}
	text := contentParts[0].Get("text").String()
	if !strings.HasPrefix(text, "[Conversation summary from earlier turns]") {
		t.Fatalf("text should carry summary prefix, got %q", text)
	}
	if !strings.Contains(text, "用户问候并讨论了 X 主题") {
		t.Fatalf("text should contain original summary, got %q", text)
	}
}

func TestNormalizeResponsesCompactionItemsDropsEmptySummary(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"type":"message","role":"user","content":"keep me"},
			{"type":"compaction","summary":""},
			{"type":"compaction"},
			{"type":"compaction","summary":"   "},
			{"type":"message","role":"user","content":"keep me too"}
		]
	}`)

	var body map[string]any
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if !normalizeResponsesCompactionItems(body) {
		t.Fatal("expected modification, got false")
	}

	got, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	items := gjson.GetBytes(got, "input").Array()
	if len(items) != 2 {
		t.Fatalf("empty/missing-summary compaction items should be dropped, got %d items: %s", len(items), got)
	}
	for i, item := range items {
		if item.Get("type").String() != "message" || item.Get("role").String() != "user" {
			t.Fatalf("remaining item %d should be original user message, got %s", i, item.Raw)
		}
	}
}

func TestPrepareResponsesBodyHandlesMultipleCompactionItems(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"type":"compaction","summary":"first summary"},
			{"type":"message","role":"user","content":"middle"},
			{"type":"compaction","summary":"second summary"},
			{"type":"message","role":"user","content":"latest"}
		]
	}`)

	codexBody, _ := PrepareResponsesBody(raw)

	items := gjson.GetBytes(codexBody, "input").Array()
	if len(items) != 4 {
		t.Fatalf("expected 4 items after normalization, got %d: %s", len(items), codexBody)
	}

	expected := []struct {
		role    string
		summary string
	}{
		{"developer", "first summary"},
		{"user", ""},
		{"developer", "second summary"},
		{"user", ""},
	}
	for i, want := range expected {
		item := items[i]
		if item.Get("type").String() != "message" {
			t.Fatalf("item %d should be message, got type=%q", i, item.Get("type").String())
		}
		if item.Get("role").String() != want.role {
			t.Fatalf("item %d role = %q, want %q", i, item.Get("role").String(), want.role)
		}
		if want.summary != "" {
			text := item.Get("content.0.text").String()
			if !strings.HasPrefix(text, "[Conversation summary from earlier turns]") {
				t.Fatalf("item %d missing summary prefix, got %q", i, text)
			}
			if !strings.Contains(text, want.summary) {
				t.Fatalf("item %d should contain %q, got %q", i, want.summary, text)
			}
		}
	}

	if gjson.GetBytes(codexBody, "input.0.type").String() == "compaction" ||
		gjson.GetBytes(codexBody, "input.2.type").String() == "compaction" {
		t.Fatalf("compaction type should not survive in upstream body: %s", codexBody)
	}
}

func TestPrepareResponsesBody_NormalizesWebSearchPreviewToolType(t *testing.T) {
	cases := []struct {
		name string
		raw  []byte
	}{
		{
			name: "preview alias",
			raw: []byte(`{
				"model":"gpt-5.5",
				"input":"hi",
				"tools":[{"type":"web_search_preview"}]
			}`),
		},
		{
			name: "dated preview alias",
			raw: []byte(`{
				"model":"gpt-5.5",
				"input":"hi",
				"tools":[{"type":"web_search_preview_2025_03_11"}]
			}`),
		},
		{
			name: "dated GA alias",
			raw: []byte(`{
				"model":"gpt-5.5",
				"input":"hi",
				"tools":[{"type":"web_search_2025_08_26"}]
			}`),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _ := PrepareResponsesBody(tc.raw)

			toolType := gjson.GetBytes(got, "tools.0.type").String()
			if toolType != "web_search" {
				t.Fatalf("expected tools.0.type=web_search, got %q; body=%s", toolType, got)
			}
		})
	}
}

func TestPrepareResponsesBody_PreservesWebSearchAllowedConfigFields(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.5",
		"input":"hi",
		"tools":[{
			"type":"web_search_preview",
			"search_context_size":"high",
			"user_location":{"type":"approximate","country":"JP","city":"Tokyo"},
			"filters":{"allowed_domains":["example.com"]}
		}]
	}`)

	got, _ := PrepareResponsesBody(raw)

	if toolType := gjson.GetBytes(got, "tools.0.type").String(); toolType != "web_search" {
		t.Fatalf("expected tools.0.type=web_search, got %q; body=%s", toolType, got)
	}
	if size := gjson.GetBytes(got, "tools.0.search_context_size").String(); size != "high" {
		t.Fatalf("expected search_context_size=high, got %q; body=%s", size, got)
	}
	if country := gjson.GetBytes(got, "tools.0.user_location.country").String(); country != "JP" {
		t.Fatalf("expected user_location.country=JP, got %q; body=%s", country, got)
	}
	if city := gjson.GetBytes(got, "tools.0.user_location.city").String(); city != "Tokyo" {
		t.Fatalf("expected user_location.city=Tokyo, got %q; body=%s", city, got)
	}
	if dom := gjson.GetBytes(got, "tools.0.filters.allowed_domains.0").String(); dom != "example.com" {
		t.Fatalf("expected filters.allowed_domains[0]=example.com, got %q; body=%s", dom, got)
	}
}

func TestPrepareResponsesBody_DropsUnknownWebSearchFields(t *testing.T) {
	// Codex 上游对未知字段严格校验，会回 400 unknown_parameter。
	// 归一时必须丢弃白名单以外的字段。
	raw := []byte(`{
		"model":"gpt-5.5",
		"input":"hi",
		"tools":[{
			"type":"web_search",
			"search_context_size":"low",
			"totally_made_up":"yes",
			"another_garbage":123
		}]
	}`)

	got, _ := PrepareResponsesBody(raw)

	if size := gjson.GetBytes(got, "tools.0.search_context_size").String(); size != "low" {
		t.Fatalf("expected search_context_size to survive, got %q; body=%s", size, got)
	}
	for _, k := range []string{"totally_made_up", "another_garbage"} {
		if gjson.GetBytes(got, "tools.0."+k).Exists() {
			t.Fatalf("expected tools.0.%s to be stripped; body=%s", k, got)
		}
	}
}

func TestPrepareResponsesBody_KeepsBareWebSearchToolUnchanged(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.5",
		"input":"hi",
		"tools":[{"type":"web_search"}]
	}`)

	got, _ := PrepareResponsesBody(raw)

	if toolType := gjson.GetBytes(got, "tools.0.type").String(); toolType != "web_search" {
		t.Fatalf("expected tools.0.type=web_search, got %q; body=%s", toolType, got)
	}
}

func TestTranslateRequest_NormalizesWebSearchPreviewToolType(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.5",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"web_search_preview","search_context_size":"high","totally_made_up":"yes"}]
	}`)

	got, err := TranslateRequest(raw)
	if err != nil {
		t.Fatalf("TranslateRequest returned error: %v", err)
	}

	toolType := gjson.GetBytes(got, "tools.0.type").String()
	if toolType != "web_search" {
		t.Fatalf("expected tools.0.type=web_search, got %q; body=%s", toolType, got)
	}
	if size := gjson.GetBytes(got, "tools.0.search_context_size").String(); size != "high" {
		t.Fatalf("expected search_context_size to survive, got %q; body=%s", size, got)
	}
	if gjson.GetBytes(got, "tools.0.totally_made_up").Exists() {
		t.Fatalf("expected unknown field to be stripped; body=%s", got)
	}
}

// compact 端点不接受 client_metadata(Unknown parameter),两条 compact 准备
// 路径都必须剥除;普通 /responses 路径不剥(引擎指纹与中转链依赖透传)。
func TestPrepareCompactBodiesStripClientMetadata(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.4",
		"stream":true,
		"client_metadata":{"x-codex-window-id":"w-1"},
		"input":[{"role":"user","content":"hello"},{"type":"compaction_trigger"}]
	}`)

	codexBody, _ := PrepareCompactResponsesBodyForOwner(raw, "")
	if gjson.GetBytes(codexBody, "client_metadata").Exists() {
		t.Fatalf("codex compact body should strip client_metadata, got %s", codexBody)
	}

	relayBody := PrepareOpenAIResponsesCompactBody(raw)
	if gjson.GetBytes(relayBody, "client_metadata").Exists() {
		t.Fatalf("relay compact body should strip client_metadata, got %s", relayBody)
	}
}

// max 档位按模型放行:gpt-5.6 起上游接受并回显,旧模型上游 400,须钳到 xhigh。
func TestNormalizeReasoningEffortForModel_MaxGatedByModel(t *testing.T) {
	cases := []struct {
		effort, model, want string
	}{
		{"max", "gpt-5.6-sol", "max"},
		{"MAX", "gpt-5.6-terra", "max"},
		{"max", "gpt-5.6", "max"},
		{"max", "gpt-6.0", "max"},
		{"max", "gpt-5.4", "xhigh"},
		{"max", "gpt-5.5", "xhigh"},
		{"max", "", "xhigh"},
		{"max", "claude-opus", "xhigh"},
		{"xhigh", "gpt-5.6-sol", "xhigh"},
		{"none", "gpt-5.4", "none"},
	}
	for _, tc := range cases {
		if got := normalizeReasoningEffortForModel(tc.effort, tc.model); got != tc.want {
			t.Errorf("normalizeReasoningEffortForModel(%q, %q) = %q, want %q", tc.effort, tc.model, got, tc.want)
		}
	}
}

func TestPrepareResponsesBody_MaxEffortPassthroughByModel(t *testing.T) {
	got, _ := PrepareResponsesBody([]byte(`{"model":"gpt-5.6-sol","input":"hi","reasoning":{"effort":"max"}}`))
	if effort := gjson.GetBytes(got, "reasoning.effort").String(); effort != "max" {
		t.Fatalf("gpt-5.6-sol effort = %q, want max; body=%s", effort, got)
	}

	got, _ = PrepareResponsesBody([]byte(`{"model":"gpt-5.4","input":"hi","reasoning":{"effort":"max"}}`))
	if effort := gjson.GetBytes(got, "reasoning.effort").String(); effort != "xhigh" {
		t.Fatalf("gpt-5.4 effort = %q, want xhigh; body=%s", effort, got)
	}
}

// issue #342: gpt-5.6 multi-agent 保留工具 collaboration.* 必须原样透传，
// 通用 schema 清洗(剥 format/minItems/pattern 等)会破坏上游要求的逐字匹配。
func TestPrepareResponsesBody_ReservedCollaborationToolPassthrough(t *testing.T) {
	raw := []byte(`{
		"model":"gpt-5.6-sol",
		"input":"hi",
		"tools":[
			{"type":"function","name":"collaboration.spawn_agent","description":"reserved","parameters":{
				"type":"object",
				"properties":{
					"agent":{"type":"string","pattern":"^[a-z]+$","minLength":1},
					"tasks":{"type":"array","items":{"type":"string"},"minItems":1,"uniqueItems":true}
				},
				"required":["agent"],
				"additionalProperties":false
			}},
			{"type":"function","name":"my_tool","parameters":{
				"type":"object",
				"properties":{"x":{"type":"string","minLength":2,"format":"email"}}
			}}
		]
	}`)

	got, _ := PrepareResponsesBody(raw)
	tools := gjson.GetBytes(got, "tools").Array()

	// 定位两个工具（顺序可能变，按 name 找）。
	var reserved, normal gjson.Result
	for _, tl := range tools {
		switch tl.Get("name").String() {
		case "collaboration.spawn_agent":
			reserved = tl
		case "my_tool":
			normal = tl
		}
	}

	// 保留工具：受限 schema 关键字必须全部保留。
	if !reserved.Exists() {
		t.Fatalf("reserved tool missing; body=%s", got)
	}
	for _, path := range []string{
		"parameters.properties.agent.pattern",
		"parameters.properties.agent.minLength",
		"parameters.properties.tasks.minItems",
		"parameters.properties.tasks.uniqueItems",
	} {
		if !reserved.Get(path).Exists() {
			t.Fatalf("reserved tool lost schema key %q (sanitizer mangled it); body=%s", path, got)
		}
	}
	if reserved.Get("parameters.additionalProperties").String() != "false" {
		t.Fatalf("reserved tool additionalProperties changed; body=%s", got)
	}

	// 对照：普通工具仍被清洗（受限关键字被剥离）。
	if !normal.Exists() {
		t.Fatalf("normal tool missing; body=%s", got)
	}
	if normal.Get("parameters.properties.x.minLength").Exists() ||
		normal.Get("parameters.properties.x.format").Exists() {
		t.Fatalf("normal function tool should still be sanitized; body=%s", got)
	}
}

func TestIsReservedCodexTool(t *testing.T) {
	cases := []struct {
		tool map[string]any
		want bool
	}{
		{map[string]any{"type": "function", "name": "collaboration.spawn_agent"}, true},
		{map[string]any{"type": "function", "name": "Collaboration.Send_Message"}, true},
		{map[string]any{"type": "function", "function": map[string]any{"name": "collaboration.wait"}}, true},
		{map[string]any{"type": "function", "name": "my_tool"}, false},
		{map[string]any{"type": "function", "name": "collaborate_now"}, false},
		{map[string]any{"type": "function"}, false},
	}
	for i, c := range cases {
		if got := isReservedCodexTool(c.tool); got != c.want {
			t.Errorf("case %d: isReservedCodexTool = %v, want %v", i, got, c.want)
		}
	}
}

// 顶层 type 是 Responses WS 事件信封字段，HTTP /responses 上游不接受
// (400 Unsupported parameter: type)；prepare 阶段必须剥离顶层、保留嵌套 (issue #548)。
func TestPrepareResponsesBodyStripsTopLevelWebSocketEnvelopeType(t *testing.T) {
	raw := []byte(`{"type":"response.create","model":"gpt-5.4","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	for name, prepare := range map[string]func([]byte) ([]byte, string){
		"http": PrepareResponsesBody,
		"ws":   PrepareResponsesWebSocketBody,
	} {
		got, _ := prepare(raw)
		if gjson.GetBytes(got, "type").Exists() {
			t.Fatalf("%s: top-level type should be stripped: %s", name, got)
		}
		if it := gjson.GetBytes(got, "input.0.type").String(); it != "message" {
			t.Fatalf("%s: nested input type = %q, want message; body=%s", name, it, got)
		}
		if ct := gjson.GetBytes(got, "input.0.content.0.type").String(); ct != "input_text" {
			t.Fatalf("%s: nested content type = %q, want input_text; body=%s", name, ct, got)
		}
	}
}

func TestModelSupportsMaxReasoningEffort(t *testing.T) {
	cases := map[string]bool{
		"gpt-5.6-sol":              true,
		"gpt-5.6":                  true,
		"gpt-6-astra":              true, // official model page lists Max for Astra; major-only ids follow major > 5
		"gpt-6":                    true,
		"gpt-7.0":                  true,
		"gpt-5.5":                  false,
		"gpt-5.4-mini":             false,
		"gpt-daybreak-blue-latest": true, // alias of gpt-5.6-sol (issue #624)
		"gpt-daybreak-red-latest":  true, // alias of gpt-5.6-cyber
		"daybreak":                 false,
		"gpt-5.4-daybreak":         false, // versioned ids follow their own version rule
		"grok-4.6":                 false,
	}
	for model, want := range cases {
		if got := modelSupportsMaxReasoningEffort(model); got != want {
			t.Errorf("modelSupportsMaxReasoningEffort(%q) = %v, want %v", model, got, want)
		}
	}
	if got := normalizeConfiguredReasoningEffort("max", "gpt-daybreak-blue-latest"); got != "max" {
		t.Fatalf("daybreak max effort clamped to %q", got)
	}
}
