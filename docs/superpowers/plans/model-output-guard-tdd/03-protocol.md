# Model Output Guard TDD Implementation Plan — Part 03: Protocol

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 构建模型输出失控防护（Model Output Guard）的协议感知投影层（Protocol Projector）、事件流水线分派器（ObserveEvent Dispatcher）与底层传输 Framing 有界读取与 Callsite 替换（SSE/WS/Response Body Framing）。

**Architecture:** 
1. 协议投影器将上游各类多样化协议（Responses HTTP/WS/Realtime、ChatCompletions、Anthropic Messages、Gemini streamGenerateContent/generateContent）的单事件 envelope 解析并投影为标准结构化的 Observation（`KindText`, `KindThinking`, `KindToolArguments`；动作 `ActionAppend`, `ActionSnapshot`, `ActionEnd`）；其中 Gemini 无 ID 工具调用受协议格式限制，无法跨 chunk 判别是否属于同一逻辑工具调用的续传，采用候选单调递增独立 channel 隔离，完整 object 或后续新 call 由请求级 channel 数量及字节预算统一兜底；
2. 事件分派器 `ObserveEvent` 严格按上游业务事件执行 1 次 `RecordEvent()` 记账（即使 control/unknown/empty 帧也扣减事件预算，排除心跳与 SSE 注释），然后调用投影出的各 Observation 执行 `ObserveAppendNoEvent`/`ObserveSnapshotNoEvent`/`EndChannel`；所有 Observation 检查全部通过后方可放行给下游消费者，一处触发则整事件拦截；
3. 传输层 Framing 机制提供资源硬上限保护（独立于 guard 业务开关），严格保留既有 `proxy/sse_line.go` 所有既有逻辑与小写符号，追加大写导出别名常量与错误桥接；保持 WebSocket `security.MaxRequestBodySize`（默认 48MB）与现有 queue budget、诊断 peek 豁免，安全替换全链路 Callsite。

**Tech Stack:** Go 1.26, `github.com/tidwall/gjson`, `github.com/tidwall/sjson`, `github.com/gorilla/websocket`, 标准库 `bufio`, `bytes`, `io`, `sync`.

**Spec:** [docs/superpowers/specs/2026-10-28-model-output-guard-design.md](../../specs/2026-10-28-model-output-guard-design.md)

## Global Constraints

- 全局启用三态存储：`output_guard_enabled`（默认 `true`，`*bool` 指针语义）。
- 默认硬预算：`output_guard_max_tool_argument_bytes` = 131072 (128 KiB)，`output_guard_max_tool_argument_deltas` = 4096，`output_guard_max_channel_bytes` = 2097152 (2 MiB)，`output_guard_max_request_bytes` = 4194304 (4 MiB)。
- 资源上限：每 request 65536 个业务事件、128 个累计 channel；单 channel 累计扫描预算为 `2 * limit`，整体最大数据处理与校验量严格受限于 `5 * limit`。
- 业务事件记账契约：每上游业务事件严格调用 1 次 `RecordEvent()`，非空与空内容/未知控制事件均计数；心跳/ping/SSE 注释除外。
- 投影边界：工具参数只解码当前 envelope，保留 inner JSON 转义，绝对不每 delta 全量 parse/repair inner JSON；原生 object 仅做单次紧凑序列化。
- 跨帧状态隔离与映射：稳定维护 channel key 与 aliasMap（如 index ↔ call_id ↔ item_id），保证 latealias 绑定到同一 channel，不发生错合并或配额重置。
- 传输 Framing 边界：严格保留 `proxy/sse_line.go` 既有符号与 8 MiB 限制，保留 WebSocket 现有 48MB `security.MaxRequestBodySize` 与 1013 Close code，保留 512B/1MiB 诊断 peek 豁免。

---

### Task 7: 统一协议感知投影层与跨帧状态机分派器

**Files:**
- Create: `proxy/outputguard_projection.go`
- Test: `proxy/outputguard_projection_test.go`

**Interfaces:**
- Consumes:
  - `security/outputguard`（严格消费 Task 3 定义的 exact 接口，不重复定义 guard.go）：
    ```go
    package outputguard

    type ChannelKind string
    const (
        KindText          ChannelKind = "text"
        KindThinking      ChannelKind = "thinking"
        KindToolArguments ChannelKind = "tool_arguments"
    )
    const (
        MaxRequestEvents   = 65536
        MaxRequestChannels = 128
        TriggeredCode      = "model_output_guard_triggered"
    )
    type Config struct {
        Enabled               bool
        MaxToolArgumentBytes  int64
        MaxToolArgumentDeltas int
        MaxChannelBytes       int64
        MaxRequestBytes       int64
    }
    type RequestGuard struct { ... }
    func NewRequestGuard(cfg Config) *RequestGuard
    func (g *RequestGuard) IsTripped() bool
    func (g *RequestGuard) TrippedReason() string
    func (g *RequestGuard) ActiveChannelCount() int
    func (g *RequestGuard) RecordEvent() error
    func (g *RequestGuard) ObserveAppend(channelID string, aliasID string, kind ChannelKind, delta []byte) error
    func (g *RequestGuard) ObserveSnapshot(channelID string, aliasID string, kind ChannelKind, snapshot []byte, terminalStage string) error
    func (g *RequestGuard) ObserveAppendNoEvent(channelID string, aliasID string, kind ChannelKind, delta []byte) error
    func (g *RequestGuard) ObserveSnapshotNoEvent(channelID string, aliasID string, kind ChannelKind, snapshot []byte, terminalStage string) error
    func (g *RequestGuard) EndChannel(channelID string) error
    ```
  - `github.com/tidwall/gjson`
- Produces:
  ```go
  package proxy

  import (
      "github.com/codex2api/security/outputguard"
  )

  type ObservationAction string
  const (
      ActionAppend   ObservationAction = "append"
      ActionSnapshot ObservationAction = "snapshot"
      ActionEnd      ObservationAction = "end"
  )

  type Observation struct {
      ChannelID     string
      AliasID       string
      Kind          outputguard.ChannelKind
      Action        ObservationAction
      Payload       []byte
      TerminalStage string // 严格白名单限制为 3 种合法语义终态阶段之一: "arguments.done", "item.done", "response.completed"，其余普通帧必须置为空字符串
  }

  type ProtocolProjector interface {
      Project(eventType string, data []byte) ([]Observation, error)
  }

  type ResponsesProjector struct {
      aliasMap map[string]string
  }
  func NewResponsesProjector() *ResponsesProjector
  func (p *ResponsesProjector) Project(eventType string, data []byte) ([]Observation, error)

  type ChatProjector struct {
      aliasMap    map[string]string
      choiceTools map[int]map[string]struct{}
  }
  func NewChatProjector() *ChatProjector
  func (p *ChatProjector) Project(eventType string, data []byte) ([]Observation, error)

  type AnthropicProjector struct {
      blockTypes map[int]string
      blockIDs   map[int]string
  }
  func NewAnthropicProjector() *AnthropicProjector
  func (p *AnthropicProjector) Project(eventType string, data []byte) ([]Observation, error)

  type GeminiProjectorMode int
  const (
      GeminiProjectorModeAuto GeminiProjectorMode = iota
      GeminiProjectorModeStream
      GeminiProjectorModeNonStream
  )
  type GeminiProjector struct {
      candidateToolCounters map[int]int
      candidateToolKeys     map[int][]string
      mode                  GeminiProjectorMode
      isStream              bool
  }
  func NewGeminiProjector() *GeminiProjector
  func NewGeminiProjectorWithMode(mode GeminiProjectorMode) *GeminiProjector
  func (p *GeminiProjector) Project(eventType string, data []byte) ([]Observation, error)

  func ObserveEvent(guard *outputguard.RequestGuard, projector ProtocolProjector, eventType string, data []byte) error
  ```

- [ ] **Step 1: Write failing test for protocol projection and event dispatcher**

```go
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
		MaxToolArgumentBytes:  131072, // 128 KiB
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

	// 构造 65536 个 unknown / empty 业务事件洪泛
	for i := 0; i < outputguard.MaxRequestEvents; i++ {
		unknownEvent := `{"type":"response.unknown_control_event","id":"ctrl_1"}`
		if err := ObserveEvent(guard, proj, "response.unknown_control_event", []byte(unknownEvent)); err != nil {
			t.Fatalf("event %d unexpectedly failed: %v", i, err)
		}
	}

	// 第 65537 个事件必须被 request_events 坚决拦截
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
```

- [ ] **Step 2: Run test to verify failure (Red)**

Run: `GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v ./proxy -run "TestResponsesProjector.*|TestChatProjector.*|TestAnthropicProjector.*|TestGeminiProjector.*|TestObserveEvent.*|TestCustomToolArgumentsEnvelopePreservation|TestRegression03.*"`
Expected: FAIL with compilation error: undefined: NewResponsesProjector, NewChatProjector, NewAnthropicProjector, NewGeminiProjector, ObserveEvent

- [ ] **Step 3: Write minimal implementation**

新建 `proxy/outputguard_projection.go`:
```go
package proxy

import (
	"bytes"
	"fmt"

	"github.com/codex2api/security/outputguard"
	"github.com/tidwall/gjson"
)

type ObservationAction string

const (
	ActionAppend   ObservationAction = "append"
	ActionSnapshot ObservationAction = "snapshot"
	ActionEnd      ObservationAction = "end"
)

type Observation struct {
	ChannelID     string
	AliasID       string
	Kind          outputguard.ChannelKind
	Action        ObservationAction
	Payload       []byte
	TerminalStage string // 严格限制为 3 种合法语义终态阶段: "arguments.done", "item.done", "response.completed"，其余普通帧置空
}

type ProtocolProjector interface {
	Project(eventType string, data []byte) ([]Observation, error)
}

// -------------------------------------------------------------
// 1. ResponsesProjector (HTTP SSE, WebSocket, Realtime Text)
// -------------------------------------------------------------

type ResponsesProjector struct {
	aliasMap map[string]string
}

func NewResponsesProjector() *ResponsesProjector {
	return &ResponsesProjector{
		aliasMap: make(map[string]string),
	}
}

func (p *ResponsesProjector) Project(eventType string, data []byte) ([]Observation, error) {
	if len(data) == 0 {
		return nil, nil
	}
	parsed := gjson.ParseBytes(data)
	t := eventType
	if t == "" {
		t = parsed.Get("type").String()
	}

	var observations []Observation

	switch t {
	case "response.output_text.delta":
		itemID := parsed.Get("item_id").String()
		delta := parsed.Get("delta").String()
		cIdx := parsed.Get("content_index").Int()
		if itemID == "" {
			itemID = fmt.Sprintf("output_%d", parsed.Get("output_index").Int())
		}
		cid := fmt.Sprintf("%s:content:%d", itemID, cIdx)
		observations = append(observations, Observation{
			ChannelID: cid,
			AliasID:   itemID,
			Kind:      outputguard.KindText,
			Action:    ActionAppend,
			Payload:   []byte(delta),
		})

	case "response.reasoning.delta", "response.thinking.delta":
		itemID := parsed.Get("item_id").String()
		delta := parsed.Get("delta").String()
		cIdx := parsed.Get("content_index").Int()
		if itemID == "" {
			itemID = fmt.Sprintf("output_%d", parsed.Get("output_index").Int())
		}
		cid := fmt.Sprintf("%s:thinking:%d", itemID, cIdx)
		observations = append(observations, Observation{
			ChannelID: cid,
			AliasID:   fmt.Sprintf("%s.thinking", itemID),
			Kind:      outputguard.KindThinking,
			Action:    ActionAppend,
			Payload:   []byte(delta),
		})

	case "response.function_call_arguments.delta":
		callID := parsed.Get("call_id").String()
		itemID := parsed.Get("item_id").String()
		delta := parsed.Get("delta").String()
		targetID := callID
		if targetID == "" {
			targetID = itemID
		}
		if targetID == "" {
			targetID = fmt.Sprintf("output_%d", parsed.Get("output_index").Int())
		}
		observations = append(observations, Observation{
			ChannelID: targetID,
			AliasID:   itemID,
			Kind:      outputguard.KindToolArguments,
			Action:    ActionAppend,
			Payload:   []byte(delta),
		})

	case "response.custom_tool_call_input.delta":
		callID := parsed.Get("call_id").String()
		itemID := parsed.Get("item_id").String()
		delta := parsed.Get("delta").String()
		targetID := callID
		if targetID == "" {
			targetID = itemID
		}
		if targetID == "" {
			targetID = fmt.Sprintf("output_%d", parsed.Get("output_index").Int())
		}
		observations = append(observations, Observation{
			ChannelID: targetID,
			AliasID:   itemID,
			Kind:      outputguard.KindToolArguments,
			Action:    ActionAppend,
			Payload:   []byte(delta),
		})

	case "response.function_call_arguments.done":
		callID := parsed.Get("call_id").String()
		itemID := parsed.Get("item_id").String()
		args := parsed.Get("arguments").String()
		targetID := callID
		if targetID == "" {
			targetID = itemID
		}
		observations = append(observations, Observation{
			ChannelID:     targetID,
			AliasID:       itemID,
			Kind:          outputguard.KindToolArguments,
			Action:        ActionSnapshot,
			Payload:       []byte(args),
			TerminalStage: "arguments.done",
		})

	case "response.custom_tool_call_input.done":
		callID := parsed.Get("call_id").String()
		itemID := parsed.Get("item_id").String()
		inputVal := parsed.Get("input").String()
		targetID := callID
		if targetID == "" {
			targetID = itemID
		}
		observations = append(observations, Observation{
			ChannelID:     targetID,
			AliasID:       itemID,
			Kind:          outputguard.KindToolArguments,
			Action:        ActionSnapshot,
			Payload:       []byte(inputVal),
			TerminalStage: "arguments.done",
		})

	case "response.output_item.done":
		item := parsed.Get("item")
		itemType := item.Get("type").String()
		itemID := item.Get("id").String()
		if itemID == "" {
			itemID = parsed.Get("item_id").String()
		}

		if itemType == "function_call" {
			callID := item.Get("call_id").String()
			args := item.Get("arguments").String()
			targetID := callID
			if targetID == "" {
				targetID = itemID
			}
			observations = append(observations, Observation{
				ChannelID:     targetID,
				AliasID:       itemID,
				Kind:          outputguard.KindToolArguments,
				Action:        ActionSnapshot,
				Payload:       []byte(args),
				TerminalStage: "item.done",
			})
		} else if itemType == "custom_tool_call" {
			callID := item.Get("call_id").String()
			inputVal := item.Get("input").String()
			targetID := callID
			if targetID == "" {
				targetID = itemID
			}
			observations = append(observations, Observation{
				ChannelID:     targetID,
				AliasID:       itemID,
				Kind:          outputguard.KindToolArguments,
				Action:        ActionSnapshot,
				Payload:       []byte(inputVal),
				TerminalStage: "item.done",
			})
		} else if itemType == "message" {
			contentArray := item.Get("content").Array()
			for idx, cBlock := range contentArray {
				cType := cBlock.Get("type").String()
				text := cBlock.Get("text").String()
				if cType == "output_text" || (cType == "" && text != "") {
					if text != "" {
						observations = append(observations, Observation{
							ChannelID:     fmt.Sprintf("%s:content:%d", itemID, idx),
							AliasID:       itemID,
							Kind:          outputguard.KindText,
							Action:        ActionSnapshot,
							Payload:       []byte(text),
							TerminalStage: "item.done",
						})
					}
				} else if cType == "reasoning" || cType == "thinking" {
					thinkingText := cBlock.Get("thinking").String()
					if thinkingText == "" {
						thinkingText = cBlock.Get("text").String()
					}
					if thinkingText != "" {
						observations = append(observations, Observation{
							ChannelID:     fmt.Sprintf("%s:thinking:%d", itemID, idx),
							AliasID:       fmt.Sprintf("%s.thinking", itemID),
							Kind:          outputguard.KindThinking,
							Action:        ActionSnapshot,
							Payload:       []byte(thinkingText),
							TerminalStage: "item.done",
						})
					}
				}
			}
		}

	case "response.completed":
		outputs := parsed.Get("response.output").Array()
		for _, item := range outputs {
			observations = append(observations, p.projectResponsesOutputItem(item, "response.completed")...)
		}

	default:
		// 兼容非流式完整响应 JSON
		if parsed.Get("status").String() == "completed" && parsed.Get("output").Exists() {
			for _, item := range parsed.Get("output").Array() {
				observations = append(observations, p.projectResponsesOutputItem(item, "response.completed")...)
			}
		}
	}

	return observations, nil
}

func (p *ResponsesProjector) projectResponsesOutputItem(item gjson.Result, stage string) []Observation {
	var observations []Observation
	itemType := item.Get("type").String()
	itemID := item.Get("id").String()

	if itemType == "function_call" {
		callID := item.Get("call_id").String()
		args := item.Get("arguments").String()
		targetID := callID
		if targetID == "" {
			targetID = itemID
		}
		observations = append(observations, Observation{
			ChannelID:     targetID,
			AliasID:       itemID,
			Kind:          outputguard.KindToolArguments,
			Action:        ActionSnapshot,
			Payload:       []byte(args),
			TerminalStage: stage,
		})
	} else if itemType == "custom_tool_call" {
		callID := item.Get("call_id").String()
		inputVal := item.Get("input").String()
		targetID := callID
		if targetID == "" {
			targetID = itemID
		}
		observations = append(observations, Observation{
			ChannelID:     targetID,
			AliasID:       itemID,
			Kind:          outputguard.KindToolArguments,
			Action:        ActionSnapshot,
			Payload:       []byte(inputVal),
			TerminalStage: stage,
		})
	} else if itemType == "message" {
		contentArray := item.Get("content").Array()
		for idx, cBlock := range contentArray {
			cType := cBlock.Get("type").String()
			text := cBlock.Get("text").String()
			if cType == "output_text" || (cType == "" && text != "") {
				if text != "" {
					observations = append(observations, Observation{
						ChannelID:     fmt.Sprintf("%s:content:%d", itemID, idx),
						AliasID:       itemID,
						Kind:          outputguard.KindText,
						Action:        ActionSnapshot,
						Payload:       []byte(text),
						TerminalStage: stage,
					})
				}
			} else if cType == "reasoning" || cType == "thinking" {
				thinkingText := cBlock.Get("thinking").String()
				if thinkingText == "" {
					thinkingText = cBlock.Get("text").String()
				}
				if thinkingText != "" {
					observations = append(observations, Observation{
						ChannelID:     fmt.Sprintf("%s:thinking:%d", itemID, idx),
						AliasID:       fmt.Sprintf("%s.thinking", itemID),
						Kind:          outputguard.KindThinking,
						Action:        ActionSnapshot,
						Payload:       []byte(thinkingText),
						TerminalStage: stage,
					})
				}
			}
		}
	}
	return observations
}

// -------------------------------------------------------------
// 2. ChatProjector (OpenAI /v1/chat/completions Stream & JSON)
// -------------------------------------------------------------

type ChatProjector struct {
	aliasMap    map[string]string
	choiceTools map[int]map[string]struct{}
}

func NewChatProjector() *ChatProjector {
	return &ChatProjector{
		aliasMap:    make(map[string]string),
		choiceTools: make(map[int]map[string]struct{}),
	}
}

func (p *ChatProjector) Project(eventType string, data []byte) ([]Observation, error) {
	if len(data) == 0 {
		return nil, nil
	}
	parsed := gjson.ParseBytes(data)
	var observations []Observation

	choices := parsed.Get("choices").Array()
	for _, choice := range choices {
		choiceIdx := choice.Get("index").Int()
		finishReason := choice.Get("finish_reason").String()

		// 流式 delta
		delta := choice.Get("delta")
		if delta.Exists() {
			if content := delta.Get("content").String(); content != "" {
				observations = append(observations, Observation{
					ChannelID: fmt.Sprintf("choice_%d.text", choiceIdx),
					Kind:      outputguard.KindText,
					Action:    ActionAppend,
					Payload:   []byte(content),
				})
			}
			reasoning := delta.Get("reasoning_content").String()
			if reasoning == "" {
				reasoning = delta.Get("reasoning").String()
			}
			if reasoning != "" {
				observations = append(observations, Observation{
					ChannelID: fmt.Sprintf("choice_%d.thinking", choiceIdx),
					Kind:      outputguard.KindThinking,
					Action:    ActionAppend,
					Payload:   []byte(reasoning),
				})
			}

			// 标准 tool_calls
			toolCalls := delta.Get("tool_calls").Array()
			for _, tc := range toolCalls {
				toolIdx := int(tc.Get("index").Int())
				callID := tc.Get("id").String()
				channelID := fmt.Sprintf("choice_%d.tool_%d", choiceIdx, toolIdx)
				if callID != "" {
					p.aliasMap[callID] = channelID
				}
				if p.choiceTools[int(choiceIdx)] == nil {
					p.choiceTools[int(choiceIdx)] = make(map[string]struct{})
				}
				p.choiceTools[int(choiceIdx)][channelID] = struct{}{}
				args := tc.Get("function.arguments").String()
				if args != "" {
					observations = append(observations, Observation{
						ChannelID: channelID,
						AliasID:   callID,
						Kind:      outputguard.KindToolArguments,
						Action:    ActionAppend,
						Payload:   []byte(args),
					})
				}
			}

			// 兼容 legacy delta.function_call (Spec §4.1)
			if fnCall := delta.Get("function_call"); fnCall.Exists() {
				args := fnCall.Get("arguments").String()
				channelID := fmt.Sprintf("choice_%d.function_call", choiceIdx)
				if args != "" {
					observations = append(observations, Observation{
						ChannelID: channelID,
						Kind:      outputguard.KindToolArguments,
						Action:    ActionAppend,
						Payload:   []byte(args),
					})
				}
			}
		}

		// 非流式 message
		msg := choice.Get("message")
		if msg.Exists() {
			if content := msg.Get("content").String(); content != "" {
				observations = append(observations, Observation{
					ChannelID:     fmt.Sprintf("choice_%d.text", choiceIdx),
					Kind:          outputguard.KindText,
					Action:        ActionSnapshot,
					Payload:       []byte(content),
					TerminalStage: "response.completed",
				})
			}
			reasoning := msg.Get("reasoning_content").String()
			if reasoning == "" {
				reasoning = msg.Get("reasoning").String()
			}
			if reasoning != "" {
				observations = append(observations, Observation{
					ChannelID:     fmt.Sprintf("choice_%d.thinking", choiceIdx),
					Kind:          outputguard.KindThinking,
					Action:        ActionSnapshot,
					Payload:       []byte(reasoning),
					TerminalStage: "response.completed",
				})
			}

			// 标准 tool_calls
			toolCalls := msg.Get("tool_calls").Array()
			for idx, tc := range toolCalls {
				callID := tc.Get("id").String()
				channelID := fmt.Sprintf("choice_%d.tool_%d", choiceIdx, idx)
				args := tc.Get("function.arguments").String()
				observations = append(observations, Observation{
					ChannelID:     channelID,
					AliasID:       callID,
					Kind:          outputguard.KindToolArguments,
					Action:        ActionSnapshot,
					Payload:       []byte(args),
					TerminalStage: "response.completed",
				})
			}

			// 兼容 legacy message.function_call (Spec §4.1)
			if fnCall := msg.Get("function_call"); fnCall.Exists() {
				args := fnCall.Get("arguments").String()
				channelID := fmt.Sprintf("choice_%d.function_call", choiceIdx)
				observations = append(observations, Observation{
					ChannelID:     channelID,
					Kind:          outputguard.KindToolArguments,
					Action:        ActionSnapshot,
					Payload:       []byte(args),
					TerminalStage: "response.completed",
				})
			}
		}

		// finish_reason 尾检触发 ActionEnd
		if finishReason != "" {
			observations = append(observations, Observation{
				ChannelID: fmt.Sprintf("choice_%d.text", choiceIdx),
				Kind:      outputguard.KindText,
				Action:    ActionEnd,
			})
			observations = append(observations, Observation{
				ChannelID: fmt.Sprintf("choice_%d.thinking", choiceIdx),
				Kind:      outputguard.KindThinking,
				Action:    ActionEnd,
			})
			observations = append(observations, Observation{
				ChannelID: fmt.Sprintf("choice_%d.function_call", choiceIdx),
				Kind:      outputguard.KindToolArguments,
				Action:    ActionEnd,
			})
			if tools, exists := p.choiceTools[int(choiceIdx)]; exists {
				for toolKey := range tools {
					observations = append(observations, Observation{
						ChannelID: toolKey,
						Kind:      outputguard.KindToolArguments,
						Action:    ActionEnd,
					})
				}
			}
		}
	}

	return observations, nil
}

// -------------------------------------------------------------
// 3. AnthropicProjector (Claude /v1/messages Stream & JSON)
// -------------------------------------------------------------

type AnthropicProjector struct {
	blockTypes map[int]string
	blockIDs   map[int]string
}

func NewAnthropicProjector() *AnthropicProjector {
	return &AnthropicProjector{
		blockTypes: make(map[int]string),
		blockIDs:   make(map[int]string),
	}
}

func (p *AnthropicProjector) Project(eventType string, data []byte) ([]Observation, error) {
	if len(data) == 0 {
		return nil, nil
	}
	parsed := gjson.ParseBytes(data)
	t := eventType
	if t == "" {
		t = parsed.Get("type").String()
	}

	var observations []Observation

	switch t {
	case "content_block_start":
		idx := int(parsed.Get("index").Int())
		block := parsed.Get("content_block")
		bType := block.Get("type").String()
		p.blockTypes[idx] = bType
		if bType == "tool_use" {
			p.blockIDs[idx] = block.Get("id").String()
		}

	case "content_block_delta":
		idx := int(parsed.Get("index").Int())
		delta := parsed.Get("delta")
		dType := delta.Get("type").String()

		switch dType {
		case "text_delta":
			text := delta.Get("text").String()
			observations = append(observations, Observation{
				ChannelID: fmt.Sprintf("block_%d", idx),
				Kind:      outputguard.KindText,
				Action:    ActionAppend,
				Payload:   []byte(text),
			})
		case "thinking_delta":
			thinking := delta.Get("thinking").String()
			observations = append(observations, Observation{
				ChannelID: fmt.Sprintf("block_%d", idx),
				Kind:      outputguard.KindThinking,
				Action:    ActionAppend,
				Payload:   []byte(thinking),
			})
		case "input_json_delta":
			partJSON := delta.Get("partial_json").String()
			callID := p.blockIDs[idx]
			observations = append(observations, Observation{
				ChannelID: fmt.Sprintf("block_%d", idx),
				AliasID:   callID,
				Kind:      outputguard.KindToolArguments,
				Action:    ActionAppend,
				Payload:   []byte(partJSON),
			})
		}

	case "content_block_stop":
		idx := int(parsed.Get("index").Int())
		bType := p.blockTypes[idx]
		if bType == "tool_use" {
			observations = append(observations, Observation{
				ChannelID: fmt.Sprintf("block_%d", idx),
				AliasID:   p.blockIDs[idx],
				Kind:      outputguard.KindToolArguments,
				Action:    ActionEnd,
			})
		} else if bType == "thinking" {
			observations = append(observations, Observation{
				ChannelID: fmt.Sprintf("block_%d", idx),
				Kind:      outputguard.KindThinking,
				Action:    ActionEnd,
			})
		} else {
			observations = append(observations, Observation{
				ChannelID: fmt.Sprintf("block_%d", idx),
				Kind:      outputguard.KindText,
				Action:    ActionEnd,
			})
		}

	case "message_stop":
		// stop key 与 start key 严格一致 (block_%d)，不造 message_root 伪 channel
		for blockIdx, blockType := range p.blockTypes {
			kind := outputguard.KindText
			if blockType == "tool_use" {
				kind = outputguard.KindToolArguments
			} else if blockType == "thinking" {
				kind = outputguard.KindThinking
			}
			observations = append(observations, Observation{
				ChannelID: fmt.Sprintf("block_%d", blockIdx),
				AliasID:   p.blockIDs[blockIdx],
				Kind:      kind,
				Action:    ActionEnd,
			})
		}

	default:
		// 非流式 JSON (type == "message")
		if parsed.Get("type").String() == "message" && parsed.Get("content").IsArray() {
			contentBlocks := parsed.Get("content").Array()
			for idx, b := range contentBlocks {
				bType := b.Get("type").String()
				switch bType {
				case "text":
					text := b.Get("text").String()
					observations = append(observations, Observation{
						ChannelID:     fmt.Sprintf("block_%d", idx),
						Kind:          outputguard.KindText,
						Action:        ActionSnapshot,
						Payload:       []byte(text),
						TerminalStage: "response.completed",
					})
				case "thinking":
					thinking := b.Get("thinking").String()
					observations = append(observations, Observation{
						ChannelID:     fmt.Sprintf("block_%d", idx),
						Kind:          outputguard.KindThinking,
						Action:        ActionSnapshot,
						Payload:       []byte(thinking),
						TerminalStage: "response.completed",
					})
				case "tool_use":
					callID := b.Get("id").String()
					inputObj := b.Get("input")
					var inputBytes []byte
					if inputObj.IsObject() {
						inputBytes = []byte(inputObj.Raw)
					} else {
						inputBytes = []byte(inputObj.String())
					}
					observations = append(observations, Observation{
						ChannelID:     fmt.Sprintf("block_%d", idx),
						AliasID:       callID,
						Kind:          outputguard.KindToolArguments,
						Action:        ActionSnapshot,
						Payload:       inputBytes,
						TerminalStage: "response.completed",
					})
				}
			}
		}
	}

	return observations, nil
}

// -------------------------------------------------------------
// 4. GeminiProjector (Native streamGenerateContent & generateContent)
// -------------------------------------------------------------

type GeminiProjectorMode int

const (
	GeminiProjectorModeAuto GeminiProjectorMode = iota
	GeminiProjectorModeStream
	GeminiProjectorModeNonStream
)

type GeminiProjector struct {
	candidateToolCounters map[int]int
	candidateToolKeys     map[int][]string
	mode                  GeminiProjectorMode
	isStream              bool
}

func NewGeminiProjector() *GeminiProjector {
	return &GeminiProjector{
		candidateToolCounters: make(map[int]int),
		candidateToolKeys:     make(map[int][]string),
		mode:                  GeminiProjectorModeAuto,
		isStream:              false,
	}
}

func NewGeminiProjectorWithMode(mode GeminiProjectorMode) *GeminiProjector {
	p := &GeminiProjector{
		candidateToolCounters: make(map[int]int),
		candidateToolKeys:     make(map[int][]string),
		mode:                  mode,
		isStream:              mode == GeminiProjectorModeStream,
	}
	return p
}

func (p *GeminiProjector) Project(eventType string, data []byte) ([]Observation, error) {
	if len(data) == 0 {
		return nil, nil
	}
	parsed := gjson.ParseBytes(data)
	var observations []Observation

	candidates := parsed.Get("candidates").Array()
	// 若处于 Auto 模式：当前事件未带 finishReason，说明处于多 chunk 流式传输中；若已进入流式，则后续 chunk 均为流式
	if p.mode == GeminiProjectorModeAuto && !p.isStream && len(candidates) > 0 {
		hasFinishReason := false
		for _, cand := range candidates {
			if cand.Get("finishReason").String() != "" {
				hasFinishReason = true
				break
			}
		}
		if !hasFinishReason {
			p.isStream = true
		}
	}

	for candIdx, cand := range candidates {
		finishReason := cand.Get("finishReason").String()
		parts := cand.Get("content.parts").Array()

		for partIdx, part := range parts {
			// 1. Text / Thought
			if part.Get("text").Exists() {
				text := part.Get("text").String()
				isThought := part.Get("thought").Bool()
				kind := outputguard.KindText
				suffix := "text"
				if isThought {
					kind = outputguard.KindThinking
					suffix = "thinking"
				}
				action := ActionAppend
				var stage string
				if !p.isStream && finishReason != "" {
					action = ActionSnapshot
					stage = "response.completed"
				}
				observations = append(observations, Observation{
					ChannelID:     fmt.Sprintf("candidate_%d.part_%d.%s", candIdx, partIdx, suffix),
					Kind:          kind,
					Action:        action,
					Payload:       []byte(text),
					TerminalStage: stage,
				})
			}

			// 2. Function Call (Native object bounded serialization)
			if fnCall := part.Get("functionCall"); fnCall.Exists() {
				callID := fnCall.Get("id").String()
				args := fnCall.Get("args")
				var argBytes []byte
				if args.IsObject() {
					argBytes = []byte(args.Raw)
				} else {
					argBytes = []byte(args.String())
				}

				action := ActionAppend
				var stage string
				if !p.isStream && finishReason != "" {
					action = ActionSnapshot
					stage = "response.completed"
				}

				// 稳定映射规则：有 callID 用 callID；无 callID 流式形态使用 candidateToolCounters 跨 chunk 单调计数。
				// 关联限制说明：对于无 ID 工具调用，Gemini 协议缺乏连续性标识，无法跨 chunk 判别同逻辑续传；
				// 原生完整 object 与后续新 call 由请求级 channel 数量及字节预算兜底，不作跨 chunk 合并的绝对宣称。
				var channelID string
				if callID != "" {
					channelID = fmt.Sprintf("candidate_%d.tool_%s", candIdx, callID)
					// 记录有 ID 工具 key 以便 finishReason 触发 ActionEnd 尾检
					found := false
					for _, k := range p.candidateToolKeys[candIdx] {
						if k == channelID {
							found = true
							break
						}
					}
					if !found {
						p.candidateToolKeys[candIdx] = append(p.candidateToolKeys[candIdx], channelID)
					}
				} else {
					currentToolIdx := p.candidateToolCounters[candIdx]
					p.candidateToolCounters[candIdx]++
					channelID = fmt.Sprintf("candidate_%d.tool_idx_%d", candIdx, currentToolIdx)
				}

				observations = append(observations, Observation{
					ChannelID:     channelID,
					AliasID:       callID,
					Kind:          outputguard.KindToolArguments,
					Action:        action,
					Payload:       argBytes,
					TerminalStage: stage,
				})
			}
		}

		// finishReason 尾检触发 ActionEnd
		if finishReason != "" {
			for i := 0; i < len(parts); i++ {
				observations = append(observations, Observation{
					ChannelID: fmt.Sprintf("candidate_%d.part_%d.text", candIdx, i),
					Kind:      outputguard.KindText,
					Action:    ActionEnd,
				})
				observations = append(observations, Observation{
					ChannelID: fmt.Sprintf("candidate_%d.part_%d.thinking", candIdx, i),
					Kind:      outputguard.KindThinking,
					Action:    ActionEnd,
				})
			}
			if totalTools, exists := p.candidateToolCounters[candIdx]; exists {
				for tIdx := 0; tIdx < totalTools; tIdx++ {
					observations = append(observations, Observation{
						ChannelID: fmt.Sprintf("candidate_%d.tool_idx_%d", candIdx, tIdx),
						Kind:      outputguard.KindToolArguments,
						Action:    ActionEnd,
					})
				}
			}
			if toolKeys, exists := p.candidateToolKeys[candIdx]; exists {
				for _, k := range toolKeys {
					observations = append(observations, Observation{
						ChannelID: k,
						Kind:      outputguard.KindToolArguments,
						Action:    ActionEnd,
					})
				}
			}
		}
	}

	return observations, nil
}

// -------------------------------------------------------------
// 5. ObserveEvent Dispatcher (单上游事件严格计 1 次，流水线批调度)
// -------------------------------------------------------------

func ObserveEvent(guard *outputguard.RequestGuard, projector ProtocolProjector, eventType string, data []byte) error {
	// 心跳/注释帧直接放行，不计入业务事件预算
	trimmed := bytes.TrimSpace(data)
	if bytes.HasPrefix(trimmed, []byte(":")) || eventType == "ping" {
		return nil
	}

	// 步骤 1: 严格每上游业务事件计 1 次 requestEvents（即使是 control/unknown 帧）
	if err := guard.RecordEvent(); err != nil {
		return err
	}

	obs, err := projector.Project(eventType, data)
	if err != nil {
		return err
	}
	if len(obs) == 0 {
		return nil
	}

	// 依次将 observations 推入 guard，执行判定流水线
	// 单 event 所有 obs 检查通过再允许 consumer，一 obs 命中整 event 不可见
	for _, o := range obs {
		switch o.Action {
		case ActionAppend:
			if err := guard.ObserveAppendNoEvent(o.ChannelID, o.AliasID, o.Kind, o.Payload); err != nil {
				return err
			}
		case ActionSnapshot:
			if err := guard.ObserveSnapshotNoEvent(o.ChannelID, o.AliasID, o.Kind, o.Payload, o.TerminalStage); err != nil {
				return err
			}
		case ActionEnd:
			// ActionEnd 目标解析：当 Observation 携带 AliasID（如 tool call_id）时优先使用；
			// 依赖 RequestGuard.aliasMap 内部双向索引反查至对应 primary channel，保持解耦契约。
			target := o.ChannelID
			if o.AliasID != "" {
				target = o.AliasID
			}
			if err := guard.EndChannel(target); err != nil {
				return err
			}
		}
	}

	return nil
}
```

- [ ] **Step 4: Run test to verify passing (Green)**

Run: `GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v ./proxy -run "TestResponsesProjector.*|TestChatProjector.*|TestAnthropicProjector.*|TestGeminiProjector.*|TestObserveEvent.*|TestCustomToolArgumentsEnvelopePreservation|TestRegression03.*"`
Expected: PASS ok github.com/codex2api/proxy

- [ ] **Step 5: Commit**

```bash
git add proxy/outputguard_projection.go proxy/outputguard_projection_test.go
git commit -m "feat(proxy): implement protocol projectors and observe event dispatcher with strict event budgeting"
```

---

### Task 8: 传输 Framing 有界 Reader 与全链路 Callsite 替换

> 注：本节参考代码以已提交实现（`proxy/sse_line.go`、`proxy/executor.go`，FW1 修复 4522ee05）为准。

**Files:**
- Modify: `proxy/sse_line.go`
- Modify: `proxy/sse_line_test.go`
- Modify: `proxy/executor.go:1592-1715`
- Review-only: `proxy/handler.go:5538-5585,7650-7750`（保持原样，由后续 04 接线）
- Review-only: `proxy/handler_gemini.go:390-420`（保持原样，由后续 04 接线）
- Review-only: `proxy/responses_ws.go:142-190`（保持原样，由后续 04 接线）

**Interfaces:**
- Consumes:
  - Go 标准库 `bufio`, `bytes`, `errors`, `io`, `net/http`, `sync`
  - `github.com/gorilla/websocket` (go.mod)
  - `security.MaxRequestBodySize` (48MB, 严格保留)
- Produces:
  ```go
  package proxy

  // 既有常量与导出常量别名桥接
  const (
      maxSSELineBytes      = 8 << 20  // 8 MiB (既有常量)
      maxSSEEventBytes     = 8 << 20  // 8 MiB (既有常量)
      MaxSSELineBytes      = maxSSELineBytes  // 导出别名
      MaxSSEEventBytes     = maxSSEEventBytes // 导出别名
      MaxResponseBodyBytes = 16 << 20 // 16 MiB (新导出常量)
  )

  // 既有 error 与导出 error 别名桥接
  var (
      errSSELineTooLong       = errors.New("upstream SSE line exceeds limit")  // 既有未导出
      errSSEEventTooLarge     = errors.New("upstream SSE event exceeds limit") // 既有未导出
      ErrSSELineTooLong       = errSSELineTooLong                             // 导出桥接
      ErrSSEEventTooLarge     = errSSEEventTooLarge                            // 导出桥接
      ErrResponseBodyTooLarge = errors.New("response_body_too_large")
  )

  // 严格保留既有签名与实现，不重复定义
  func readBoundedSSELine(reader *bufio.Reader, limit int) ([]byte, error)
  func ReadSSEStreamWithEvent(body io.Reader, callback func(event string, data []byte) bool) error
  func ReadBoundedResponseBody(body io.Reader, limit int64) ([]byte, error)
  ```

- [ ] **Step 1: Write failing test for bounded readers and callsites**

> **测试清理说明**：清理测试文件中可能导致编译错误的无用 import（如 `unused bufio/bytes`）。测试用例必须使用真实的 `io.Reader`，严格验证行为不走自证逻辑。

```go
// proxy/sse_line_test.go (在现有 TestReadSSEDataLine* 测试之后追加 Task 8 测试用例)
package proxy

import (
	"errors"
	"io"
	"strings"
	"testing"
)

// 1. 测试跨 Read 块的 split chunk 正常解析
func TestReadSSEStreamWithEventSplitChunks(t *testing.T) {
	input := "data: hello "
	input2 := "world\n\n"
	r := io.MultiReader(strings.NewReader(input), strings.NewReader(input2))

	var collected []string
	err := ReadSSEStreamWithEvent(r, func(event string, data []byte) bool {
		collected = append(collected, string(data))
		return true
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(collected) != 1 || collected[0] != "hello world" {
		t.Fatalf("collected want ['hello world'], got %v", collected)
	}
}

// 2. 测试标准多行 data 聚合
func TestReadSSEStreamWithEventMultilineData(t *testing.T) {
	input := "event: update\ndata: line1\ndata: line2\ndata: line3\n\n"
	var events []string
	var payloads []string
	err := ReadSSEStreamWithEvent(strings.NewReader(input), func(event string, data []byte) bool {
		events = append(events, event)
		payloads = append(payloads, string(data))
		return true
	})
	if err != nil {
		t.Fatalf("multiline read error: %v", err)
	}
	if len(events) != 1 || events[0] != "update" || payloads[0] != "line1\nline2\nline3" {
		t.Fatalf("multiline want update with line1\\nline2\\nline3, got %v: %v", events, payloads)
	}
}

// 3. 测试无换行符超大单行硬 cap 拒绝，返回 ErrSSELineTooLong (与既有 errSSELineTooLong 等价)
func TestReadSSEStreamWithEventNoNewlineOversized(t *testing.T) {
	oversized := strings.Repeat("x", MaxSSELineBytes+1024)
	err := ReadSSEStreamWithEvent(strings.NewReader(oversized), func(event string, data []byte) bool {
		return true
	})
	if err == nil {
		t.Fatalf("expected ErrSSELineTooLong for oversized line without newline, got nil")
	}
	if !errors.Is(err, ErrSSELineTooLong) {
		t.Fatalf("expected ErrSSELineTooLong, got %v", err)
	}
}

// 4. 测试累积多行 data 整事件超标拒绝，返回 ErrSSEEventTooLarge (与既有 errSSEEventTooLarge 等价)
func TestReadSSEStreamWithEventNeverendingMultilineOversized(t *testing.T) {
	chunk := "data: " + strings.Repeat("a", 1<<20) + "\n"
	multilineOversized := strings.Repeat(chunk, 9) + "\n"

	err := ReadSSEStreamWithEvent(strings.NewReader(multilineOversized), func(event string, data []byte) bool {
		return true
	})
	if err == nil {
		t.Fatalf("expected ErrSSEEventTooLarge for oversized multiline event, got nil")
	}
	if !errors.Is(err, ErrSSEEventTooLarge) {
		t.Fatalf("expected ErrSSEEventTooLarge, got %v", err)
	}
}

// 5. 测试非流式超大 Response Body 有界拦截，返回 ErrResponseBodyTooLarge
func TestReadBoundedResponseBodyOversized(t *testing.T) {
	oversizedBody := strings.Repeat("b", 1024)
	_, err := ReadBoundedResponseBody(strings.NewReader(oversizedBody), 512)
	if err == nil {
		t.Fatalf("expected ErrResponseBodyTooLarge, got nil")
	}
	if !errors.Is(err, ErrResponseBodyTooLarge) {
		t.Fatalf("expected ErrResponseBodyTooLarge, got %v", err)
	}

	// 正常大小读取成功
	normalBody := "{\"status\":\"ok\"}"
	data, err := ReadBoundedResponseBody(strings.NewReader(normalBody), MaxResponseBodyBytes)
	if err != nil || string(data) != normalBody {
		t.Fatalf("normal read error = %v, got %s", err, string(data))
	}
}

// 6. 验证 Transport Framing 独立于 Guard 开关（关闭语义 guard 仍保留 wire cap）
func TestTransportFramingIndependentOfGuard(t *testing.T) {
	oversized := strings.Repeat("x", MaxSSELineBytes+100)
	err := ReadSSEStreamWithEvent(strings.NewReader(oversized), func(event string, data []byte) bool {
		return true
	})
	if !errors.Is(err, ErrSSELineTooLong) {
		t.Fatalf("wire cap must remain active regardless of guard settings, got %v", err)
	}
}
```

- [ ] **Step 2: Run test to verify failure (Red)**

Run: `GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v ./proxy -run "TestReadSSEStreamWithEvent.*|TestReadBoundedResponseBody.*|TestTransportFramingIndependentOfGuard"`
Expected: FAIL with compilation error: undefined symbols ErrSSELineTooLong, ErrSSEEventTooLarge, ReadBoundedResponseBody

- [ ] **Step 3: Write minimal implementation**

修改 `proxy/sse_line.go`:
保留既有全部内容，仅在文件尾部追加导出别名与 `ReadBoundedResponseBody`：
```go
// proxy/sse_line.go
package proxy

import (
	"bufio"
	"errors"
	"io"
)

// SSE wire limits are resource bounds, not a declaration that larger events
// are invalid upstream. Base64 media and large tool results can exceed them.
const maxSSELineBytes = 8 << 20

const maxSSEEventBytes = 8 << 20

var errSSELineTooLong = errors.New("upstream SSE line exceeds limit")

var errSSEEventTooLarge = errors.New("upstream SSE event exceeds limit")

// readBoundedSSELine 读取一行，内存占用不超过 limit（不含 bufio 自身缓冲）。
func readBoundedSSELine(reader *bufio.Reader, limit int) ([]byte, error) {
	var line []byte
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(line)+len(fragment) > limit {
			return nil, errSSELineTooLong
		}
		line = append(line, fragment...)
		if err != bufio.ErrBufferFull {
			return line, err
		}
	}
}

// -------------------------------------------------------------------------
// 增补部分：导出常量别名与非流式有界读取器（严格保留既有小写符号兼容性）
// -------------------------------------------------------------------------

const (
	MaxSSELineBytes      = maxSSELineBytes
	MaxSSEEventBytes     = maxSSEEventBytes
	MaxResponseBodyBytes = 16 << 20 // 16 MiB (非流式响应体默认上限)
)

var (
	ErrSSELineTooLong       = errSSELineTooLong
	ErrSSEEventTooLarge     = errSSEEventTooLarge
	ErrResponseBodyTooLarge = errors.New("response_body_too_large")
)

// ReadBoundedResponseBody 有界读取非流式响应体。
func ReadBoundedResponseBody(body io.Reader, limit int64) ([]byte, error) {
	if limit <= 0 {
		limit = MaxResponseBodyBytes
	}
	// 预留 +1 探测超限时需要 limit+1；MaxInt64 时加一会溢出为负，导致
	// io.LimitReader 立即 EOF 并静默返回空数据，因此先做 clamp。
	if limit > math.MaxInt64-1 {
		limit = math.MaxInt64 - 1
	}
	lr := io.LimitReader(body, limit+1)
	data, err := io.ReadAll(lr)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, ErrResponseBodyTooLarge
	}
	return data, nil
}
```

修改 `proxy/executor.go:1602-1715` 中的 `ReadSSEStreamWithEvent`：
添加对单行及多行累积事件的硬 Cap 判定（返回 `errSSELineTooLong` 与 `errSSEEventTooLarge`）：
```go
// proxy/executor.go
func ReadSSEStreamWithEvent(body io.Reader, callback func(event string, data []byte) bool) error {
	buf := sseBufferPool.Get().([]byte)
	defer sseBufferPool.Put(buf)

	lineBufPtr := sseLineBufPool.Get().(*[]byte)
	lineBuf := (*lineBufPtr)[:0]
	defer func() {
		if cap(lineBuf) <= 256*1024 {
			*lineBufPtr = lineBuf[:0]
			sseLineBufPool.Put(lineBufPtr)
		}
	}()

	var dataLines [][]byte
	var eventName string
	var currentEventBytes int

	emitEvent := func() (bool, error) {
		event := eventName
		eventName = ""
		currentEventBytes = 0
		if len(dataLines) == 0 {
			return true, nil
		}

		data := dataLines[0]
		if len(dataLines) > 1 {
			data = bytes.Join(dataLines, []byte("\n"))
		}
		isDone := bytes.Equal(data, []byte("[DONE]"))
		keepReading := !isDone && callback(event, data)
		for i := range dataLines {
			dataLines[i] = nil
		}
		dataLines = dataLines[:0]
		return keepReading, nil
	}

	consumeField := func(line []byte) error {
		if bytes.HasPrefix(line, []byte("data:")) {
			data := bytes.TrimPrefix(line, []byte("data:"))
			data = bytes.TrimPrefix(data, []byte(" "))
			// 单行与多行累积事件都必须在 wire 层硬性受限，避免超大事件
			// 长期占用内存。空 data 行（len(data)==0）也必须至少计 1 字节：
			// 否则无穷多的 "data:" 行在事件边界缺失时会让 dataLines 无界增长。
			if currentEventBytes+max(len(data), 1) > maxSSEEventBytes {
				return errSSEEventTooLarge
			}
			currentEventBytes += max(len(data), 1)
			dataCopy := make([]byte, len(data))
			copy(dataCopy, data)
			dataLines = append(dataLines, dataCopy)
			return nil
		}
		if bytes.HasPrefix(line, []byte("event:")) {
			event := bytes.TrimPrefix(line, []byte("event:"))
			event = bytes.TrimPrefix(event, []byte(" "))
			eventName = string(event)
		}
		return nil
	}

	for {
		n, err := body.Read(buf)
		if n > 0 {
			// 未完成行在缓冲区中持续累积，必须在 append 前就拒绝超过
			// wire 上限的输入，避免无换行巨量数据无限增长。
			if len(lineBuf)+n > maxSSELineBytes {
				return errSSELineTooLong
			}
			lineBuf = append(lineBuf, buf[:n]...)

			consumed := 0
			for {
				idx := bytes.IndexByte(lineBuf[consumed:], '\n')
				if idx < 0 {
					// 深度防御（defense in depth）：上面的 append 前检查已保证
					// len(lineBuf) <= maxSSELineBytes 且 consumed >= 0，此处数学上
					// 不可达，仅作为未来改动的兜底。
					if len(lineBuf)-consumed > maxSSELineBytes {
						return errSSELineTooLong
					}
					break
				}

				lineEnd := consumed + idx
				line := bytes.TrimRight(lineBuf[consumed:lineEnd], "\r")
				consumed = lineEnd + 1

				if len(line) == 0 {
					keep, emitErr := emitEvent()
					if emitErr != nil {
						return emitErr
					}
					if !keep {
						return nil
					}
					continue
				}

				if bytes.HasPrefix(line, []byte(":")) {
					continue
				}

				if err := consumeField(line); err != nil {
					return err
				}
			}

			if consumed > 0 {
				remaining := copy(lineBuf, lineBuf[consumed:])
				lineBuf = lineBuf[:remaining]
			}
		}

		if err != nil {
			if err == io.EOF {
				if len(lineBuf) > 0 {
					if len(lineBuf) > maxSSELineBytes {
						return errSSELineTooLong
					}
					line := bytes.TrimRight(lineBuf, "\r")
					if err := consumeField(line); err != nil {
						return err
					}
				}
				_, emitErr := emitEvent()
				if emitErr != nil {
					return emitErr
				}
				return nil
			}
			return err
		}
	}
}
```

### 全链路 Callsite 替换与诊断 Peek 豁免分析

在进行全链路 Callsite 替换时，不能机械替换全部 `LimitReader`。对现有生产代码中各 `LimitReader` 调用点进行审慎分类：

1. **必须保留的诊断 Peek / 元数据读取豁免（不得机械替换为 16MB MaxResponseBodyBytes）**：
   - `proxy/executor.go:829`: `io.LimitReader(resp.Body, 64*1024)` 用于错误诊断前缀 peek，仅需 64KB，保持不变；
   - `proxy/executor.go:1047`: `io.LimitReader(resp.Body, peekLimit)` 用于探测响应前缀，保持不变；
   - `proxy/claude_cli_version_sync.go:95`: `io.LimitReader(resp.Body, 512)` 用于诊断前缀 peek，保持不变；
   - `proxy/modeltrace_bank_sync.go:75`: `io.LimitReader(resp.Body, 512)` 用于诊断前缀 peek，保持不变；
   - `proxy/model_pricing_sync.go:104`: `io.LimitReader(resp.Body, 512)` 用于诊断前缀 peek，保持不变；
   - `proxy/codex_cli_version_sync.go:74`: `io.LimitReader(resp.Body, 512)` 用于诊断前缀 peek，保持不变；
   - `proxy/grok_runtime_facts.go:157`: `io.LimitReader(original, grokRuntimeErrorPeekLimit)` 诊断错误前缀，保持不变。

2. **必须保留的高额与专用上限（WebSocket 与媒体传输）**：
   - `proxy/responses_ws.go:272`: `conn.SetReadLimit(int64(security.MaxRequestBodySize))` 严格保留 48MB 上限与 1013 Close code，不得新加 `SetReadLimit(8MB)` 收紧；
   - `proxy/realtime_ws.go:66`: `conn.SetReadLimit(int64(security.MaxRequestBodySize))` 严格保留 48MB 上限；
   - `proxy/wsrelay/read_pump.go:174`: `wc.conn.SetReadLimit(readPumpMaxQueuedPayload)` 保留 16MB 队列预算；
   - `proxy/openai_responses_ws.go:329`: `conn.SetReadLimit(openAIResponsesWSMaxMessageBytes)` 保留 32MB 压缩解压消息上限。

3. **实际替换的生成 Body 读取 Callsite 与现有专用预算**：
   - 非流式 HTTP 响应完整读取：现有架构中 `proxy/handler.go:4983` Responses 原生非流式已有 `maxUpstreamResponseBodyBytes` (64 MiB)，`proxy/handler_gemini.go:337,723` Gemini Native 非流式已有 `antigravityResponseBodyLimit` (8 MiB)，均严格保留专用 wire 预算，不收紧 64 MiB 亦不扩大 8 MiB；
   - `proxy/handler.go:7650` 属于 chat 协议流式 SSE 转发分支，不存在 `io.ReadAll`；
   - `ReadBoundedResponseBody(resp.Body, 16 MiB)`（及 `MaxResponseBodyBytes`）专用于新增/未受保护的非流式响应路径与后续任务（如 Task 10 的 Files / 模型元数据或无界响应读取）提供统一安全保底。

- [ ] **Step 4: Run test to verify passing (Green)**

Run: `GOCACHE=/tmp/go-build GOPATH=/tmp/go go test -v ./proxy -run "TestReadSSEStreamWithEvent.*|TestReadBoundedResponseBody.*|TestTransportFramingIndependentOfGuard"`
Expected: PASS ok github.com/codex2api/proxy

- [ ] **Step 5: Commit**

```bash
git add proxy/sse_line.go proxy/sse_line_test.go proxy/executor.go
git commit -m "feat(proxy): harden SSE framing with 8MiB wire limits and export alias bridge"
```
