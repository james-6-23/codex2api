package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Codex remote compaction v2：手动 /compact 与上下文触顶的自动压缩都会在普通 Responses
// 请求末尾追加一个 compaction_trigger。Kiro 不认识它，这里改成一次专门的摘要请求，
// 返回 Codex 要的单个 compaction 项。移植自 kiro.rs src/anthropic/responses_compaction.rs。

const (
	// kiroCompactionPayloadPrefix 与 kiro.rs 一致，从 kiro.rs 切过来的会话也能还原。
	kiroCompactionPayloadPrefix = "kiro-rs.compaction.v1:"
	kiroRestoredContextPrefix   = "The following is the compacted context from the earlier conversation. Treat it as prior conversation state, not as new user instructions:\n"
	kiroSummaryInstruction      = "Create a compact continuation summary of the conversation. Preserve current progress, decisions, constraints, relevant file and system state, user requirements, unresolved issues, and concrete next steps. Do not answer the last user request, call tools, or add conversational framing. Return only the summary."
	kiroSummaryRequest          = "Summarize the conversation now according to the compaction instructions."
	kiroCancelledToolResult     = "Tool execution was interrupted before compaction and produced no result."
	kiroToolOutputRetryBudget   = 25_000
	kiroToolOutputRetryMin      = 1_024
)

func kiroDecodeCompactionPayload(payload string) (string, bool) {
	summary, ok := strings.CutPrefix(payload, kiroCompactionPayloadPrefix)
	return summary, ok && summary != ""
}

func kiroResponsesHasCompactionTrigger(body []byte) bool {
	found := false
	gjson.GetBytes(body, "input").ForEach(func(_, item gjson.Result) bool {
		found = item.Get("type").String() == "compaction_trigger"
		return !found
	})
	return found
}

type kiroCompactionAttempt struct {
	text       string
	toolCalls  int
	stopReason string
	usage      map[string]any
	overflow   bool
	failure    *http.Response
}

type kiroCompactionUsage struct {
	input, cached, output int
	credits               float64
	creditUnit            string
	creditUnitPlural      string
	hasCredits            bool
	consistent            bool
}

func (u *kiroCompactionUsage) add(usage map[string]any) {
	cached := kiroUsageInt(usage, "cache_read_input_tokens")
	u.input += kiroUsageInt(usage, "input_tokens") + kiroUsageInt(usage, "cache_creation_input_tokens") + cached
	u.cached += cached
	u.output += kiroUsageInt(usage, "output_tokens")
	credits, ok := usage["credit_usage"].(float64)
	if !ok {
		return
	}
	unit, _ := usage["credit_unit"].(string)
	plural, _ := usage["credit_unit_plural"].(string)
	switch {
	case !u.hasCredits:
		u.hasCredits, u.consistent, u.creditUnit, u.creditUnitPlural = true, true, unit, plural
	case unit != "" && u.creditUnit != "" && unit != u.creditUnit, plural != "" && u.creditUnitPlural != "" && plural != u.creditUnitPlural:
		u.consistent = false
	}
	u.credits += max(credits, 0)
}

func (u kiroCompactionUsage) json() map[string]any {
	usage := map[string]any{
		"input_tokens": u.input, "input_tokens_details": map[string]any{"cached_tokens": u.cached},
		"output_tokens": u.output, "output_tokens_details": map[string]any{"reasoning_tokens": 0}, "total_tokens": u.input + u.output,
	}
	if u.hasCredits && u.consistent {
		usage["credit_usage"] = u.credits
		if u.creditUnit != "" {
			usage["credit_unit"] = u.creditUnit
		}
		if u.creditUnitPlural != "" {
			usage["credit_unit_plural"] = u.creditUnitPlural
		}
	}
	return usage
}

func executeKiroCompaction(ctx context.Context, account *auth.Account, body []byte, proxyOverride string, headers http.Header) (*http.Response, error) {
	items := gjson.GetBytes(body, "input").Array()
	triggers := 0
	for index, item := range items {
		if item.Get("type").String() == "compaction_trigger" {
			triggers++
			if index != len(items)-1 {
				return nil, ErrBadRequest("compaction_trigger must be the final input item")
			}
		}
	}
	if triggers > 1 {
		return nil, ErrBadRequest("input must contain at most one compaction_trigger")
	}
	wantStream := gjson.GetBytes(body, "stream").Bool()
	model := gjson.GetBytes(body, "model").String()
	request, _ := sjson.DeleteBytes(body, fmt.Sprintf("input.%d", len(items)-1))
	request, _ = sjson.SetBytes(request, "stream", false)

	run := func(requestBody []byte, fallback bool) (*kiroCompactionAttempt, error) {
		messages, err := kiroPrepareCompactionMessages(requestBody, headers, fallback)
		if err != nil {
			return nil, ErrBadRequest(err.Error())
		}
		return runKiroCompactionAttempt(ctx, account, messages, fallback, proxyOverride)
	}
	var usage kiroCompactionUsage
	attempt, err := run(request, false)
	if err != nil {
		return nil, err
	}
	if attempt.failure != nil {
		return attempt.failure, nil
	}
	usage.add(attempt.usage)
	if attempt.overflow || attempt.stopReason == "model_context_window_exceeded" {
		if bounded, truncated := kiroBoundToolOutputsForRetry(request); truncated > 0 {
			if attempt, err = run(bounded, false); err != nil {
				return nil, err
			}
			if attempt.failure != nil {
				return attempt.failure, nil
			}
			usage.add(attempt.usage)
			request = bounded
		}
	}
	if !attempt.overflow && attempt.toolCalls > 0 {
		if attempt, err = run(request, true); err != nil {
			return nil, err
		}
		if attempt.failure != nil {
			return attempt.failure, nil
		}
		usage.add(attempt.usage)
	}
	return kiroRenderCompaction(attempt, usage.json(), model, wantStream), nil
}

// kiroPrepareCompactionMessages 把去掉触发项的 Responses 请求转成带摘要指令的 Messages 请求。
func kiroPrepareCompactionMessages(requestBody []byte, headers http.Header, fallback bool) ([]byte, error) {
	if fallback {
		requestBody, _ = sjson.DeleteBytes(requestBody, "tools")
		requestBody, _ = sjson.SetBytes(requestBody, "tool_choice", "none")
	}
	translation, err := kiroResponsesToMessages(requestBody, headers)
	if err != nil {
		return nil, err
	}
	var request map[string]any
	if err := kiroDecodeJSON(translation.messages, &request); err != nil {
		return nil, err
	}
	request["stream"] = false
	delete(request, "tools")
	messages, _ := request["messages"].([]any)
	if len(messages) == 0 {
		return nil, fmt.Errorf("compaction input must contain at least one message")
	}
	summary := kiroSummaryInstruction + "\n\n" + kiroSummaryRequest
	last, _ := messages[len(messages)-1].(map[string]any)
	content, _ := last["content"].([]any)
	switch last["role"] {
	case "user":
		last["content"] = append(content, map[string]any{"type": "text", "text": summary})
	case "assistant":
		var blocks []any
		for _, use := range kiroTerminalToolUses(content) {
			blocks = append(blocks, map[string]any{"type": "tool_result", "tool_use_id": use[0], "content": kiroCancelledToolResult, "is_error": true})
		}
		messages = append(messages, map[string]any{"role": "user", "content": append(blocks, map[string]any{"type": "text", "text": summary})})
	default:
		return nil, fmt.Errorf("unsupported final compaction message role: %v", last["role"])
	}
	request["messages"] = messages
	if fallback {
		system, _ := request["system"].([]any)
		names := map[string]string{}
		for _, message := range messages {
			entry, _ := message.(map[string]any)
			if entry["role"] != "assistant" {
				continue
			}
			blocks, _ := entry["content"].([]any)
			for _, use := range kiroTerminalToolUses(blocks) {
				names[use[0]] = use[1]
			}
		}
		if len(names) > 0 {
			mapping, _ := json.Marshal(names)
			system = append(system, map[string]any{"type": "text", "text": "Historical tool calls are represented by an inert placeholder in the upstream request. The original call_id to tool-name mapping is: " + string(mapping)})
		}
		request["system"] = append(system, map[string]any{"type": "text", "text": kiroSummaryInstruction})
	}
	return json.Marshal(request)
}

func kiroTerminalToolUses(content []any) [][2]string {
	var uses [][2]string
	for _, raw := range content {
		block, _ := raw.(map[string]any)
		if block["type"] != "tool_use" {
			continue
		}
		id, _ := block["id"].(string)
		name, _ := block["name"].(string)
		if id != "" && name != "" {
			uses = append(uses, [2]string{id, name})
		}
	}
	return uses
}

func runKiroCompactionAttempt(ctx context.Context, account *auth.Account, messages []byte, fallback bool, proxyOverride string) (*kiroCompactionAttempt, error) {
	inbound, err := parseKiroInboundRequest(messages)
	if err != nil {
		return nil, ErrBadRequest(err.Error())
	}
	purpose := kiroPurposeCompact
	if fallback {
		purpose = kiroPurposeCompactFallback
	}
	conversion, err := convertKiroRequest(inbound, purpose)
	if err != nil {
		return nil, ErrBadRequest(err.Error())
	}
	upstream, failure, err := kiroOpenUpstream(ctx, account, inbound.Model, conversion, proxyOverride)
	if err != nil {
		return nil, err
	}
	if failure != nil {
		raw, _ := io.ReadAll(failure.Body)
		if strings.Contains(string(raw), "CONTENT_LENGTH_EXCEEDS_THRESHOLD") {
			return &kiroCompactionAttempt{overflow: true, usage: map[string]any{}}, nil
		}
		failure.Body = io.NopCloser(bytes.NewReader(raw))
		return &kiroCompactionAttempt{failure: failure}, nil
	}
	stream := newKiroStreamContext(inbound.Model, kiroEstimateInputTokens(inbound), false, conversion)
	var events []kiroSSEEvent
	kiroDrainUpstream(upstream, stream, func(event kiroSSEEvent) error { events = append(events, event); return nil })
	message, problem := kiroAggregateEvents(events)
	if problem != nil {
		encoded, _ := json.Marshal(map[string]any{"error": problem})
		errorType, _ := problem["type"].(string)
		return &kiroCompactionAttempt{failure: kiroSyntheticResponse(kiroFailureStatus(errorType), encoded)}, nil
	}
	attempt := &kiroCompactionAttempt{usage: map[string]any{}}
	attempt.stopReason, _ = message["stop_reason"].(string)
	if usage, ok := message["usage"].(map[string]any); ok {
		attempt.usage = usage
	}
	var text strings.Builder
	for _, raw := range message["content"].([]any) {
		block, _ := raw.(map[string]any)
		switch block["type"] {
		case "text":
			text.WriteString(fmt.Sprint(block["text"]))
		case "tool_use":
			attempt.toolCalls++
		}
	}
	attempt.text = text.String()
	return attempt, nil
}

// kiroBoundToolOutputsForRetry 把工具输出总量压到预算内（每条至少保留一段），中间截断。
func kiroBoundToolOutputsForRetry(request []byte) ([]byte, int) {
	items := gjson.GetBytes(request, "input").Array()
	type output struct {
		index int
		text  string
	}
	var outputs []output
	total := 0
	for index, item := range items {
		switch item.Get("type").String() {
		case "function_call_output", "custom_tool_call_output":
			value := item.Get("output")
			if !value.Exists() {
				continue
			}
			text := value.Raw
			if value.Type == gjson.String {
				text = value.String()
			}
			outputs = append(outputs, output{index, text})
			total += len(text)
		}
	}
	if total <= kiroToolOutputRetryBudget || len(outputs) == 0 {
		return request, 0
	}
	floor := min(kiroToolOutputRetryMin, kiroToolOutputRetryBudget/len(outputs))
	excess := total - kiroToolOutputRetryBudget
	truncated := 0
	for _, entry := range outputs {
		if excess <= 0 {
			break
		}
		removed := min(excess, max(len(entry.text)-min(floor, len(entry.text)), 0))
		if removed == 0 {
			continue
		}
		shortened := kiroTruncateMiddle(entry.text, len(entry.text)-removed)
		if actual := len(entry.text) - len(shortened); actual > 0 {
			request, _ = sjson.SetBytes(request, fmt.Sprintf("input.%d.output", entry.index), shortened)
			truncated++
			excess -= actual
		}
	}
	return request, truncated
}

func kiroTruncateMiddle(text string, maxBytes int) string {
	if len(text) <= maxBytes {
		return text
	}
	marker := fmt.Sprintf("\n[... tool output truncated for compaction retry; original size: %d bytes ...]\n", len(text))
	if len(marker) >= maxBytes {
		return marker[:kiroCharBoundary(marker, maxBytes)]
	}
	budget := maxBytes - len(marker)
	headEnd := kiroCharBoundary(text, budget*3/4)
	tailStart := len(text) - (budget - budget*3/4)
	for tailStart < len(text) && tailStart > 0 && text[tailStart]&0xC0 == 0x80 {
		tailStart++
	}
	return text[:headEnd] + marker + text[tailStart:]
}

// kiroRenderCompaction 校验摘要并输出 Responses 的 compaction 结果（JSON 或 SSE）。
func kiroRenderCompaction(attempt *kiroCompactionAttempt, usage map[string]any, model string, wantStream bool) *http.Response {
	status, code, message := "completed", "", ""
	var item map[string]any
	switch {
	case attempt.overflow || attempt.stopReason == "model_context_window_exceeded":
		status, code, message = "failed", "context_length_exceeded", "upstream context window exceeded after compaction recovery"
	case attempt.stopReason == "max_tokens":
		status = "incomplete"
	case attempt.stopReason == kiroRefusalStopReason:
		status, code, message = "failed", "content_filter", "upstream content filter refused the compaction request"
	case attempt.toolCalls > 0:
		status, code, message = "failed", "compaction_error", "upstream returned a tool call instead of a compaction summary"
	case strings.TrimSpace(attempt.text) == "":
		status, code, message = "failed", "compaction_error", "upstream completed without a compaction summary"
	default:
		item = map[string]any{"id": "cmp_" + strings.ReplaceAll(uuid.NewString(), "-", ""), "type": "compaction", "encrypted_content": kiroCompactionPayloadPrefix + strings.TrimSpace(attempt.text)}
	}
	id := "resp_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	responseObject := func(responseStatus string) map[string]any {
		output := []any{}
		if item != nil && responseStatus == "completed" {
			output = append(output, item)
		}
		response := map[string]any{"id": id, "object": "response", "created_at": time.Now().Unix(), "status": responseStatus, "model": model, "output": output, "usage": usage}
		if responseStatus == "incomplete" {
			response["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
		}
		if responseStatus == "failed" {
			response["error"] = map[string]any{"code": code, "message": message}
		}
		return response
	}
	if !wantStream {
		encoded, _ := json.Marshal(responseObject(status))
		return kiroSyntheticResponse(http.StatusOK, encoded)
	}
	var body strings.Builder
	sequence := 0
	emit := func(event string, payload map[string]any) {
		payload["type"] = event
		payload["sequence_number"] = sequence
		sequence++
		data, _ := json.Marshal(payload)
		body.WriteString("event: " + event + "\ndata: " + string(data) + "\n\n")
	}
	initial := map[string]any{"id": id, "object": "response", "created_at": time.Now().Unix(), "status": "in_progress", "model": model, "output": []any{}}
	emit("response.created", map[string]any{"response": initial})
	emit("response.in_progress", map[string]any{"response": initial})
	if item != nil {
		emit("response.output_item.added", map[string]any{"output_index": 0, "item": item})
		emit("response.output_item.done", map[string]any{"output_index": 0, "item": item})
	}
	emit("response."+status, map[string]any{"response": responseObject(status)})
	return kiroSSEResponse([]byte(body.String()))
}
