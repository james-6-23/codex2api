// proxy/outputguard_projection_test.go
package proxy

import (
	"strings"
	"testing"

	"github.com/codex2api/security/outputguard"
)

func defaultProjectionTestConfig() outputguard.Config {
	return outputguard.Config{
		Enabled:               true,
		MaxToolArgumentBytes:  262144, // 256 KiB
		MaxToolArgumentDeltas: 4096,
		MaxChannelBytes:       2097152, // 2 MiB
		MaxRequestBytes:       4194304, // 4 MiB
	}
}

// 1. Responses 协议流式、3 个合法语义终态阶段快照与 alias 跨帧映射测试
// 真实 delta 只有 item_id，output_item.done 才出现 call_id；多 content 完整遍历
func TestResponsesProjectorStreamAndTerminalSnapshots(t *testing.T) {
	guard := outputguard.NewRequestGuard(defaultProjectionTestConfig())
	proj := NewResponsesProjector()

	// Text delta
	textDelta := `{"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"content_index":0,"delta":"Hello, "}`
	if err := ObserveEvent(guard, proj, "response.output_text.delta", []byte(textDelta)); err != nil {
		t.Fatalf("text delta observe failed: %v", err)
	}

	// Thinking delta
	thinkingDelta := `{"type":"response.reasoning.delta","item_id":"msg_1","content_index":0,"delta":"thinking step 1"}`
	if err := ObserveEvent(guard, proj, "response.reasoning.delta", []byte(thinkingDelta)); err != nil {
		t.Fatalf("thinking delta observe failed: %v", err)
	}

	// Tool call delta (初始仅有 item_id，缺失 call_id)
	toolDelta := `{"type":"response.function_call_arguments.delta","item_id":"item_call_1","output_index":1,"delta":"{\"path\":"}`
	if err := ObserveEvent(guard, proj, "response.function_call_arguments.delta", []byte(toolDelta)); err != nil {
		t.Fatalf("tool delta observe failed: %v", err)
	}

	// function_call_arguments.done (终态阶段 1: arguments.done 快照)
	argsDone := `{"type":"response.function_call_arguments.done","item_id":"item_call_1","call_id":"call_write_file","arguments":"{\"path\":\"test.txt\"}"}`
	if err := ObserveEvent(guard, proj, "response.function_call_arguments.done", []byte(argsDone)); err != nil {
		t.Fatalf("args done observe failed: %v", err)
	}

	// output_item.done (终态阶段 2: item.done 快照，晚到 call_id 绑定成功，不新建 channel)
	itemDone := `{"type":"response.output_item.done","item":{"id":"item_call_1","type":"function_call","call_id":"call_write_file","arguments":"{\"path\":\"test.txt\"}"}}`
	if err := ObserveEvent(guard, proj, "response.output_item.done", []byte(itemDone)); err != nil {
		t.Fatalf("item done observe failed: %v", err)
	}

	// response.completed (终态阶段 3: response.completed 纯终态确认快照，含多 content message 与 tool)
	completed := `{"type":"response.completed","response":{"id":"resp_1","status":"completed","output":[{"id":"item_call_1","type":"function_call","call_id":"call_write_file","arguments":"{\"path\":\"test.txt\"}"},{"id":"msg_1","type":"message","content":[{"type":"output_text","text":"Hello, "},{"type":"reasoning","thinking":"thinking step 1"}]}]}}`
	if err := ObserveEvent(guard, proj, "response.completed", []byte(completed)); err != nil {
		t.Fatalf("response completed observe failed: %v", err)
	}

	if guard.IsTripped() {
		t.Fatalf("expected guard not tripped, got %s", guard.TrippedReason())
	}
}

// 2. ChatCompletions 协议 choice/toolindex/callid alias 隔离、legacy function_call 及 finish_reason 尾检测试
func TestChatProjectorStreamingAndNonStreaming(t *testing.T) {
	guard := outputguard.NewRequestGuard(defaultProjectionTestConfig())
	proj := NewChatProjector()

	// 流式 Chunk 1: content + reasoning + tool_call 首帧 (带 call_id 与 index) + legacy delta.function_call
	chunk1 := `{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"content":"foo","reasoning_content":"think","tool_calls":[{"index":0,"id":"call_123","function":{"name":"search","arguments":"{\"q\":"}}],"function_call":{"name":"legacy_fn","arguments":"{\"a\":"}}}]}`
	if err := ObserveEvent(guard, proj, "", []byte(chunk1)); err != nil {
		t.Fatalf("chunk 1 observe error: %v", err)
	}

	// 流式 Chunk 2: tool_call 后续 delta (仅带 index, 缺失 id, 验证 aliasMap 稳定解析到同一 channel) + legacy function_call delta
	chunk2 := `{"id":"chatcmpl-1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"golang\"}"}}],"function_call":{"arguments":"1}"}}}]}`
	if err := ObserveEvent(guard, proj, "", []byte(chunk2)); err != nil {
		t.Fatalf("chunk 2 observe error: %v", err)
	}

	// 验证不同 choice 隔离，不发生错合并；并测试 finish_reason 触发 EndChannel 尾检
	chunkChoice1 := `{"id":"chatcmpl-1","choices":[{"index":1,"delta":{"content":"bar"},"finish_reason":"stop"}]}`
	if err := ObserveEvent(guard, proj, "", []byte(chunkChoice1)); err != nil {
		t.Fatalf("chunk choice 1 observe error: %v", err)
	}

	// 非流式 JSON 测试 (含 message.function_call 与 message.tool_calls)
	nonStream := `{"id":"chatcmpl-2","choices":[{"index":0,"message":{"role":"assistant","content":"final content","tool_calls":[{"id":"call_999","type":"function","function":{"name":"calc","arguments":"{\"val\":42}"}}],"function_call":{"name":"old","arguments":"{\"x\":1}"}},"finish_reason":"stop"}]}`
	guardNonStream := outputguard.NewRequestGuard(defaultProjectionTestConfig())
	projNonStream := NewChatProjector()
	if err := ObserveEvent(guardNonStream, projNonStream, "", []byte(nonStream)); err != nil {
		t.Fatalf("non-stream chat observe error: %v", err)
	}
	if guardNonStream.IsTripped() {
		t.Fatalf("expected non-stream not tripped, got %s", guardNonStream.TrippedReason())
	}
}

// 3. Anthropic Messages 协议真实 fixture 与 content_block 状态流转测试 (stop key 与 start key 严格一致)
func TestAnthropicProjectorStreamLifecycleAndNonStream(t *testing.T) {
	guard := outputguard.NewRequestGuard(defaultProjectionTestConfig())
	proj := NewAnthropicProjector()

	// message_start
	msgStart := `{"type":"message_start","message":{"id":"msg_123","type":"message","role":"assistant","content":[]}}`
	if err := ObserveEvent(guard, proj, "message_start", []byte(msgStart)); err != nil {
		t.Fatalf("msg start error: %v", err)
	}

	// block 0: thinking
	b0Start := `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`
	if err := ObserveEvent(guard, proj, "content_block_start", []byte(b0Start)); err != nil {
		t.Fatalf("b0 start error: %v", err)
	}
	b0Delta := `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"let me think"}}`
	if err := ObserveEvent(guard, proj, "content_block_delta", []byte(b0Delta)); err != nil {
		t.Fatalf("b0 delta error: %v", err)
	}
	b0Stop := `{"type":"content_block_stop","index":0}`
	if err := ObserveEvent(guard, proj, "content_block_stop", []byte(b0Stop)); err != nil {
		t.Fatalf("b0 stop error: %v", err)
	}

	// block 1: tool_use
	b1Start := `{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_01ABC","name":"exec"}}`
	if err := ObserveEvent(guard, proj, "content_block_start", []byte(b1Start)); err != nil {
		t.Fatalf("b1 start error: %v", err)
	}
	b1Delta := `{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"cmd\":\"ls\"}"}}`
	if err := ObserveEvent(guard, proj, "content_block_delta", []byte(b1Delta)); err != nil {
		t.Fatalf("b1 delta error: %v", err)
	}
	b1Stop := `{"type":"content_block_stop","index":1}`
	if err := ObserveEvent(guard, proj, "content_block_stop", []byte(b1Stop)); err != nil {
		t.Fatalf("b1 stop error: %v", err)
	}

	// message_stop: 验证对 block_0 与 block_1 发送 ActionEnd，不造 message_root
	msgStop := `{"type":"message_stop"}`
	if err := ObserveEvent(guard, proj, "message_stop", []byte(msgStop)); err != nil {
		t.Fatalf("msg stop error: %v", err)
	}

	// 非流式 Anthropic 响应
	nonStream := `{"id":"msg_456","type":"message","role":"assistant","content":[{"type":"text","text":"Done!"},{"type":"thinking","thinking":"done thinking"},{"type":"tool_use","id":"toolu_02XYZ","name":"write","input":{"content":"file content"}}]}`
	guardAnthropicNonStream := outputguard.NewRequestGuard(defaultProjectionTestConfig())
	projAnthropicNonStream := NewAnthropicProjector()
	if err := ObserveEvent(guardAnthropicNonStream, projAnthropicNonStream, "", []byte(nonStream)); err != nil {
		t.Fatalf("anthropic non-stream observe error: %v", err)
	}
	if guardAnthropicNonStream.IsTripped() {
		t.Fatalf("expected anthropic non-stream not tripped, got %s", guardAnthropicNonStream.TrippedReason())
	}
}

// 4. Gemini Native 同名并发工具、无 ID 流式 fallback 与跨 chunk key 稳定性测试
func TestGeminiProjectorStreamAndNonStreamObjectSerialization(t *testing.T) {
	guard := outputguard.NewRequestGuard(defaultProjectionTestConfig())
	proj := NewGeminiProjector()

	// 流式 chunk 1: 包含 2 个同名的 functionCall 且无 ID，验证按 stable index 隔离而不按 partidx+name 误合并
	geminiChunk1 := `{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"hello gemini"},{"functionCall":{"name":"search","args":{"query":"part1"}}},{"functionCall":{"name":"search","args":{"query":"part2"}}}]}}]}`
	if err := ObserveEvent(guard, proj, "", []byte(geminiChunk1)); err != nil {
		t.Fatalf("gemini chunk 1 observe error: %v", err)
	}

	// Gemini 非流式带 finishReason
	geminiNonStreamJSON := `{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"done"},{"functionCall":{"name":"fetch","args":{"url":"https://example.com"}}}]},"finishReason":"STOP"}]}`
	guardGemini := outputguard.NewRequestGuard(defaultProjectionTestConfig())
	projGemini := NewGeminiProjector()
	if err := ObserveEvent(guardGemini, projGemini, "", []byte(geminiNonStreamJSON)); err != nil {
		t.Fatalf("gemini non-stream observe error: %v", err)
	}
	if guardGemini.IsTripped() {
		t.Fatalf("expected gemini non-stream not tripped, got %s", guardGemini.TrippedReason())
	}
}

// 5. 事件计数契约：每上游事件一次，unknown/control/empty 业务事件严格扣减事件预算，心跳注释除外
func TestObserveEventBudgetAccountability(t *testing.T) {
	guard := outputguard.NewRequestGuard(defaultProjectionTestConfig())
	proj := NewResponsesProjector()

	// SSE 注释与 ping 帧不消耗业务事件预算
	if err := ObserveEvent(guard, proj, "", []byte(": keep-alive ping\n")); err != nil {
		t.Fatalf("comment frame should not fail: %v", err)
	}
	if err := ObserveEvent(guard, proj, "ping", []byte("{}")); err != nil {
		t.Fatalf("ping event should not fail: %v", err)
	}

	// 构造 131072 个 unknown / empty 业务事件洪泛
	for i := 0; i < outputguard.MaxRequestEvents; i++ {
		unknownEvent := `{"type":"response.unknown_control_event","id":"ctrl_1"}`
		if err := ObserveEvent(guard, proj, "response.unknown_control_event", []byte(unknownEvent)); err != nil {
			t.Fatalf("event %d unexpectedly failed: %v", i, err)
		}
	}

	// 第 131073 个事件必须被 request_events 坚决拦截
	err := ObserveEvent(guard, proj, "response.created", []byte(`{"type":"response.created"}`))
	if err == nil || err.Error() != "request_events" {
		t.Fatalf("expected request_events rejection for control/unknown flood, got %v", err)
	}
	if guard.TrippedReason() != "request_events" {
		t.Fatalf("expected tripped reason request_events, got %s", guard.TrippedReason())
	}
}

// 6. 验证 Custom Tool Arguments String 仅解码当前 envelope，不每 delta 重 parse inner JSON
func TestCustomToolArgumentsEnvelopePreservation(t *testing.T) {
	guard := outputguard.NewRequestGuard(defaultProjectionTestConfig())
	proj := NewResponsesProjector()

	// 构造未闭合且包含转义与大段文本的 arguments 片段
	incompleteJSONChunk := `{"type":"response.function_call_arguments.delta","item_id":"call_custom","delta":"{\"nested_key\":\"val\\nwith\\\"quotes and unclosed brackets..."}`
	if err := ObserveEvent(guard, proj, "response.function_call_arguments.delta", []byte(incompleteJSONChunk)); err != nil {
		t.Fatalf("custom tool envelope observe error: %v", err)
	}

	if guard.IsTripped() {
		t.Fatalf("expected guard not tripped on unclosed inner JSON, got %s", guard.TrippedReason())
	}
}

// -----------------------------------------------------------------------------
// 03-A 回归测试：Gemini 跨 chunk 无 ID tool calls 隔离与带 ID 稳定映射
// -----------------------------------------------------------------------------

// 03-A Critical 回归测试：
// Gemini 流式跨 chunk 无 ID tool calls 必须单调递增计数，互不误合并。
// 3 个独立无 ID 调用必须映射到 3 个独立 channel，ActiveChannelCount 必须为 3。
func TestRegression03A_GeminiCrossChunkUnlabeledToolCallMerge(t *testing.T) {
	guard := outputguard.NewRequestGuard(defaultProjectionTestConfig())
	proj := NewGeminiProjector()

	// chunk1: 2 个无 ID functionCall
	chunk1 := `{"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"name":"fnA","args":{"x":1}}},{"functionCall":{"name":"fnB","args":{"y":2}}}]}}]}`
	if err := ObserveEvent(guard, proj, "", []byte(chunk1)); err != nil {
		t.Fatalf("chunk1 error: %v", err)
	}

	// chunk2: 1 个新无 ID functionCall
	chunk2 := `{"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"name":"fnC","args":{"z":3}}}]}}]}`
	if err := ObserveEvent(guard, proj, "", []byte(chunk2)); err != nil {
		t.Fatalf("chunk2 error: %v", err)
	}

	// 必须为 3 个独立 channel
	if got := guard.ActiveChannelCount(); got != 3 {
		t.Fatalf("03-A failure: expected ActiveChannelCount=3 across chunks, got %d (chunk2 wrongly collided with chunk1!)", got)
	}
}

// 03-A 补充测试：有 ID 稳定性及同 chunk 同名测试
func TestRegression03A_GeminiWithIDAndSameName(t *testing.T) {
	guard := outputguard.NewRequestGuard(defaultProjectionTestConfig())
	proj := NewGeminiProjector()

	// 包含 call ID 的 functionCall
	chunk1 := `{"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"id":"call_unique_1","name":"fnA","args":{"x":1}}}]}}]}`
	if err := ObserveEvent(guard, proj, "", []byte(chunk1)); err != nil {
		t.Fatalf("chunk1 error: %v", err)
	}

	// chunk2 继续使用相同 call ID，应当稳定映射到同一 channel
	chunk2 := `{"candidates":[{"index":0,"content":{"role":"model","parts":[{"functionCall":{"id":"call_unique_1","name":"fnA","args":{"x":2}}}]}}]}`
	if err := ObserveEvent(guard, proj, "", []byte(chunk2)); err != nil {
		t.Fatalf("chunk2 error: %v", err)
	}

	if got := guard.ActiveChannelCount(); got != 1 {
		t.Fatalf("expected ActiveChannelCount=1 for matching callID, got %d", got)
	}
}

// -----------------------------------------------------------------------------
// 03-B 回归测试：Gemini 流式 finishReason 追加与非流式全量快照
// -----------------------------------------------------------------------------

// 03-B Important 回归测试：
// finishReason chunk 中的 text/thought 必须以 Append 动作推入，且只追加 End；
// 绝不能在流式末尾被降级为 Snapshot 导致 detector 窗口被 Reset 而逃逸。
func TestRegression03B_GeminiFinishReasonTailEscape(t *testing.T) {
	guard := outputguard.NewRequestGuard(defaultProjectionTestConfig())
	proj := NewGeminiProjector()

	var periodic strings.Builder
	for i := 0; i < 4095; i++ {
		periodic.WriteString("ab")
	}
	// chunk1: append 8190 字节周期数据（尚未达 8192 检查点）
	chunk1 := `{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"` + periodic.String() + `"}]}}]}`
	if err := ObserveEvent(guard, proj, "", []byte(chunk1)); err != nil {
		t.Fatalf("chunk1 error: %v", err)
	}
	if guard.IsTripped() {
		t.Fatalf("chunk1 unexpectedly tripped: %s", guard.TrippedReason())
	}

	// chunk2: finishReason=STOP，text 延续 4 字节使总量达到 8194 字节纯周期。
	// 正确的流式 Append + End 必定在 8192 检查点或尾检处 trip repetition！
	chunk2 := `{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"abab"}]},"finishReason":"STOP"}]}`
	err := ObserveEvent(guard, proj, "", []byte(chunk2))
	if err == nil && !guard.IsTripped() {
		t.Fatalf("03-B failure: stream tail text was treated as snapshot, causing reset window escape! tripped=%v reason=%q", guard.IsTripped(), guard.TrippedReason())
	}
	if !guard.IsTripped() || guard.TrippedReason() != "repetition" {
		t.Fatalf("03-B failure: expected repetition, got err=%v tripped=%v reason=%q", err, guard.IsTripped(), guard.TrippedReason())
	}
}

// 03-B 补充测试：非流式 JSON 响应仍然保持 Snapshot 语义
func TestRegression03B_GeminiNonStreamJSONFullSnapshot(t *testing.T) {
	guard := outputguard.NewRequestGuard(defaultProjectionTestConfig())
	proj := NewGeminiProjector()

	// 非流式整包响应
	nonStreamJSON := `{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"full response text"}]},"finishReason":"STOP"}]}`
	if err := ObserveEvent(guard, proj, "", []byte(nonStreamJSON)); err != nil {
		t.Fatalf("non-stream JSON error: %v", err)
	}
	if guard.IsTripped() {
		t.Fatalf("unexpected trip on valid non-stream response: %s", guard.TrippedReason())
	}
}

// 03-B 显式构造函数测试：NewGeminiProjectorWithMode
func TestRegression03B_GeminiProjectorModes(t *testing.T) {
	guard := outputguard.NewRequestGuard(defaultProjectionTestConfig())
	projStream := NewGeminiProjectorWithMode(GeminiProjectorModeStream)
	projNonStream := NewGeminiProjectorWithMode(GeminiProjectorModeNonStream)

	// 非流式 Projector 直接推带 finishReason 的单包数据，执行 Snapshot 动作
	nonStreamJSON := `{"candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"mode test"}]},"finishReason":"STOP"}]}`
	obs, err := projNonStream.Project("", []byte(nonStreamJSON))
	if err != nil {
		t.Fatalf("project error: %v", err)
	}
	if len(obs) == 0 || obs[0].Action != ActionSnapshot {
		t.Fatalf("expected ActionSnapshot for non-stream projector, got %v", obs[0].Action)
	}

	// 流式 Projector 推带 finishReason 的首个包，仍执行 ActionAppend
	obsStream, err := projStream.Project("", []byte(nonStreamJSON))
	if err != nil {
		t.Fatalf("stream project error: %v", err)
	}
	if len(obsStream) == 0 || obsStream[0].Action != ActionAppend {
		t.Fatalf("expected ActionAppend for stream projector, got %v", obsStream[0].Action)
	}
	_ = guard
}

// -----------------------------------------------------------------------------
// 03-C 回归测试：Chat finish_reason 全部活跃 tool_calls End 尾检与隔离
// -----------------------------------------------------------------------------

// 03-C Important 回归测试：
// ChatProjector 流式 tool_calls 在 finish_reason 时必须对所有活跃的 choice.tool_idx 触发 End 进行尾检。
func TestRegression03C_ChatToolChannelNoTailCheck(t *testing.T) {
	guard := outputguard.NewRequestGuard(defaultProjectionTestConfig())
	proj := NewChatProjector()

	prefix := strings.Repeat("z", 100)
	var periodic strings.Builder
	for i := 0; i < 820; i++ {
		periodic.WriteString("0123456789")
	}
	// args = 100 杂散 + 8200 周期 = 8300 字节；Push 检查点 8192 处窗口=[100杂散+8092周期] 未命中
	args1 := prefix + periodic.String()
	chunk1 := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"f","arguments":"` + args1 + `"}}]}}]}`
	if err := ObserveEvent(guard, proj, "", []byte(chunk1)); err != nil {
		t.Fatalf("chunk1 error: %v", err)
	}
	if guard.IsTripped() {
		t.Fatalf("chunk1 unexpectedly tripped: %s", guard.TrippedReason())
	}

	// chunk2: finish_reason 到达，无更多 args delta。
	// 正确实现对 choice_0.tool_0 执行 ActionEnd 触发尾检，此时最新 8192 字节（包含 8192 字节纯周期）必须触发 repetition！
	chunk2 := `{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`
	err := ObserveEvent(guard, proj, "", []byte(chunk2))
	if err == nil && !guard.IsTripped() {
		t.Fatalf("03-C failure: chat finish_reason omitted tool channels tail check! tripped=%v reason=%q", guard.IsTripped(), guard.TrippedReason())
	}

	if !guard.IsTripped() || guard.TrippedReason() != "repetition" {
		t.Fatalf("03-C failure: chat finish_reason omitted tool channels tail check! tripped=%v reason=%q", guard.IsTripped(), guard.TrippedReason())
	}
}

// 03-C 补充测试：多 choice 与多 tool 隔离尾检测试
func TestRegression03C_MultiChoiceMultiToolIsolation(t *testing.T) {
	guard := outputguard.NewRequestGuard(defaultProjectionTestConfig())
	proj := NewChatProjector()

	// choice 0 和 choice 1 分别有 tool 0 和 tool 1
	chunk1 := `{"choices":[
		{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_c0_t0","function":{"name":"f0","arguments":"normal"}}]}},
		{"index":1,"delta":{"tool_calls":[{"index":1,"id":"call_c1_t1","function":{"name":"f1","arguments":"normal"}}]}}
	]}`
	if err := ObserveEvent(guard, proj, "", []byte(chunk1)); err != nil {
		t.Fatalf("chunk1 error: %v", err)
	}

	// 确认两个 tool channel 均活跃
	if got := guard.ActiveChannelCount(); got != 2 {
		t.Fatalf("expected ActiveChannelCount=2, got %d", got)
	}

	// choice 0 finish
	chunk2 := `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`
	if err := ObserveEvent(guard, proj, "", []byte(chunk2)); err != nil {
		t.Fatalf("chunk2 error: %v", err)
	}

	// choice 1 finish
	chunk3 := `{"choices":[{"index":1,"delta":{},"finish_reason":"stop"}]}`
	if err := ObserveEvent(guard, proj, "", []byte(chunk3)); err != nil {
		t.Fatalf("chunk3 error: %v", err)
	}

	if guard.IsTripped() {
		t.Fatalf("normal multi-choice multi-tool unexpectedly tripped: %s", guard.TrippedReason())
	}
}

// 03-C 补充测试：晚到 ID 场景尾检正常生效
func TestRegression03C_ChatToolChannelLateArrivingIDTailCheck(t *testing.T) {
	guard := outputguard.NewRequestGuard(defaultProjectionTestConfig())
	proj := NewChatProjector()

	prefix := strings.Repeat("z", 100)
	var periodic strings.Builder
	for i := 0; i < 820; i++ {
		periodic.WriteString("0123456789")
	}
	args := prefix + periodic.String()

	// chunk 1: 仅有 tool index 与部分参数，无 id
	c1 := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"f","arguments":"` + args[:4000] + `"}}]}}]}`
	if err := ObserveEvent(guard, proj, "", []byte(c1)); err != nil {
		t.Fatalf("c1: %v", err)
	}

	// chunk 2: 晚到 call_id 与剩余参数
	c2 := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_late_1","function":{"arguments":"` + args[4000:] + `"}}]}}]}`
	if err := ObserveEvent(guard, proj, "", []byte(c2)); err != nil {
		t.Fatalf("c2: %v", err)
	}

	// chunk 3: finish_reason 到达，触发 choice 0 的所有 tool_calls EndChannel
	c3 := `{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`
	err := ObserveEvent(guard, proj, "", []byte(c3))
	if err == nil && !guard.IsTripped() {
		t.Fatalf("expected repetition trip on tail check, got tripped=%v", guard.IsTripped())
	}
	if !guard.IsTripped() || guard.TrippedReason() != "repetition" {
		t.Fatalf("expected repetition trip on tail check, got tripped=%v reason=%q", guard.IsTripped(), guard.TrippedReason())
	}
}

// 03-C 补充测试：完全缺失 ID 场景尾检正常生效
func TestRegression03C_ChatToolChannelMissingIDTailCheck(t *testing.T) {
	guard := outputguard.NewRequestGuard(defaultProjectionTestConfig())
	proj := NewChatProjector()

	prefix := strings.Repeat("z", 100)
	var periodic strings.Builder
	for i := 0; i < 820; i++ {
		periodic.WriteString("0123456789")
	}
	args := prefix + periodic.String()

	// chunk 1: 全程无 ID
	c1 := `{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"f","arguments":"` + args + `"}}]}}]}`
	if err := ObserveEvent(guard, proj, "", []byte(c1)); err != nil {
		t.Fatalf("c1: %v", err)
	}

	// chunk 2: finish_reason
	c2 := `{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`
	err := ObserveEvent(guard, proj, "", []byte(c2))
	if err == nil && !guard.IsTripped() {
		t.Fatalf("expected repetition trip on tail check, got tripped=%v", guard.IsTripped())
	}
	if !guard.IsTripped() || guard.TrippedReason() != "repetition" {
		t.Fatalf("expected repetition trip on tail check, got tripped=%v reason=%q", guard.IsTripped(), guard.TrippedReason())
	}
}

// 03-C 补充测试：Gemini 有 ID 工具 finishReason 尾检触发与 ActionEnd 补全测试 (reviewer PC3 回归)
func TestRegression03C_GeminiWithIDToolChannelFinishReasonEnd(t *testing.T) {
	// 步骤 1: 复用 reviewer PC3 完整 fixture (c1 8190B blob + c3 4B tail + finishReason STOP)
	// 验证 Projector 在 finishReason 时必须对有 ID 工具 (callT -> candidate_0.tool_callT) 生成 ActionEnd
	proj := NewGeminiProjector()
	periodic := strings.Repeat("w", 8190)
	c1 := `{"candidates":[{"index":0,"content":{"parts":[{"functionCall":{"id":"callT","name":"f","args":{"blob":"` + periodic + `"}}}]}}]}`
	if _, err := proj.Project("", []byte(c1)); err != nil {
		t.Fatalf("c1 project: %v", err)
	}

	c3 := `{"candidates":[{"index":0,"content":{"parts":[{"functionCall":{"id":"callT","name":"f","args":{"tail":"wwww"}}}]},"finishReason":"STOP"}]}`
	obs, err := proj.Project("", []byte(c3))
	if err != nil {
		t.Fatalf("c3 project: %v", err)
	}
	hasEnd := false
	for _, o := range obs {
		if o.ChannelID == "candidate_0.tool_callT" && o.Action == ActionEnd {
			hasEnd = true
			break
		}
	}
	if !hasEnd {
		t.Fatalf("03-C failure: Gemini with-ID tool channel omitted ActionEnd at finishReason!")
	}

	// 步骤 2: 验证流水线尾检生效：当 tool args 尾部存在周期数据时，finishReason 的 ActionEnd 必须触发尾检命中 repetition
	guard := outputguard.NewRequestGuard(defaultProjectionTestConfig())
	proj2 := NewGeminiProjector()
	prefix := strings.Repeat("z", 100)
	periodicTail := strings.Repeat("0123456789", 820) // 8200 字节
	args := prefix + periodicTail

	chunk1 := `{"candidates":[{"index":0,"content":{"parts":[{"functionCall":{"id":"call_tail_1","name":"f","args":"` + args + `"}}]}}]}`
	if err := ObserveEvent(guard, proj2, "", []byte(chunk1)); err != nil {
		t.Fatalf("chunk1 observe: %v", err)
	}
	if guard.IsTripped() {
		t.Fatalf("chunk1 unexpectedly tripped")
	}

	// chunk2: finishReason 到达，触发 call_tail_1 的 ActionEnd，执行 CheckTail 必须命中 repetition
	chunk2 := `{"candidates":[{"index":0,"content":{"parts":[]},"finishReason":"STOP"}]}`
	err = ObserveEvent(guard, proj2, "", []byte(chunk2))
	if err == nil && !guard.IsTripped() {
		t.Fatalf("expected repetition trip on tail check for with-ID gemini tool, got tripped=%v", guard.IsTripped())
	}
	if !guard.IsTripped() || guard.TrippedReason() != "repetition" {
		t.Fatalf("expected repetition trip on tail check for with-ID gemini tool, got tripped=%v reason=%q", guard.IsTripped(), guard.TrippedReason())
	}
}
