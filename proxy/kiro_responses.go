package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// Kiro 专用 Responses 桥（ADR-0002），移植自 kiro.rs src/anthropic/responses.rs：
// Responses 请求 → Anthropic Messages → Kiro 内核；Anthropic 事件 → Responses 事件。
// Codex 的 custom（自由文本）工具在上游表现为 {"input": string} 的 function，回程按
// 声明种类还原成 custom_tool_call；namespace 工具展平成 ns__name。

const (
	kiroResponsesDefaultMaxTokens = 32000
	kiroWebSearchNudgeStrict      = "You have a web_search tool that returns live results. For anything time-sensitive — current events, news, recent sports results, prices, releases, or facts that may be newer than your training data — call web_search before answering, and never claim something did not happen without searching first. Do not call any other tool."
	kiroWebSearchNudgeSoft        = "You have a web_search tool that returns live results. For anything time-sensitive — current events, news, recent sports results, prices, releases, or facts that may be newer than your training data — call web_search before answering, and never claim something did not happen without searching first. Use your other tools normally for all other work."
)

type kiroDeclaredTool struct {
	custom    bool
	name      string
	namespace string
}

type kiroToolKinds map[string]kiroDeclaredTool

func kiroFlatToolName(namespace, name string) string {
	if namespace != "" {
		return namespace + "__" + name
	}
	return name
}

// resolve 先按展平名精确匹配；上游回吐裸名时只接受唯一声明。
func (k kiroToolKinds) resolve(upstreamName string) (kiroDeclaredTool, bool) {
	if declared, ok := k[upstreamName]; ok {
		return declared, true
	}
	var found kiroDeclaredTool
	matches := 0
	for _, declared := range k {
		if declared.name == upstreamName {
			found = declared
			matches++
		}
	}
	return found, matches == 1
}

type kiroResponsesConfig struct {
	parallelToolCalls bool
	toolChoice        any
	tools             []any
}

type kiroResponsesTranslation struct {
	messages []byte
	kinds    kiroToolKinds
	config   kiroResponsesConfig
	model    string
	stream   bool
	// webSearch 表示声明了托管 web_search，需要走服务端搜索循环。
	webSearch bool
}

const kiroSingleToolCallInstruction = "Call at most one tool in each response. Wait for its result before calling another tool."

// kiroRestoreChatControls 把 Codex 形态的 Chat 翻译丢掉、而 Kiro 桥要读的控制项从原始 Chat 体补回。
func kiroRestoreChatControls(body, chat []byte) []byte {
	root := gjson.ParseBytes(chat)
	// max_completion_tokens 是新写法，排在后面覆盖 max_tokens。
	for _, field := range [][2]string{{"parallel_tool_calls", "parallel_tool_calls"}, {"tool_choice", "tool_choice"}, {"max_tokens", "max_output_tokens"}, {"max_completion_tokens", "max_output_tokens"}} {
		if value := root.Get(field[0]); value.Exists() && value.Type != gjson.Null {
			if next, err := sjson.SetRawBytes(body, field[1], []byte(value.Raw)); err == nil {
				body = next
			}
		}
	}
	return body
}

// kiroResponsesToMessages 把 Responses 请求翻成 Anthropic Messages 请求体。
func kiroResponsesToMessages(body []byte, headers http.Header) (*kiroResponsesTranslation, error) {
	root := gjson.ParseBytes(body)
	if !root.IsObject() {
		return nil, fmt.Errorf("Responses request must be a JSON object")
	}
	out := &kiroResponsesTranslation{kinds: kiroToolKinds{}, model: root.Get("model").String(), stream: root.Get("stream").Bool()}
	out.config.parallelToolCalls = !root.Get("parallel_tool_calls").Exists() || root.Get("parallel_tool_calls").Bool()
	out.config.toolChoice = "auto"
	if choice := root.Get("tool_choice"); choice.Exists() {
		out.config.toolChoice = choice.Value()
	}
	var declared []gjson.Result
	root.Get("tools").ForEach(func(_, tool gjson.Result) bool {
		declared = append(declared, tool)
		out.config.tools = append(out.config.tools, tool.Value())
		return true
	})
	var system []string
	if instructions := root.Get("instructions").String(); strings.TrimSpace(instructions) != "" {
		system = append(system, instructions)
	}
	type turn struct {
		role   string
		blocks []any
	}
	var merged []turn
	push := func(role string, blocks []any) {
		if len(blocks) == 0 {
			return
		}
		if n := len(merged); n > 0 && merged[n-1].role == role {
			merged[n-1].blocks = append(merged[n-1].blocks, blocks...)
			return
		}
		merged = append(merged, turn{role, blocks})
	}
	input := root.Get("input")
	if input.Type == gjson.String && input.String() != "" {
		push("user", []any{map[string]any{"type": "text", "text": input.String()}})
	}
	var conversionErr error
	input.ForEach(func(_, item gjson.Result) bool {
		switch item.Get("type").String() {
		case "additional_tools":
			item.Get("tools").ForEach(func(_, tool gjson.Result) bool {
				declared = append(declared, tool)
				out.config.tools = append(out.config.tools, tool.Value())
				return true
			})
		case "function_call":
			callID := firstNonEmptyString(item.Get("call_id").String(), item.Get("id").String())
			arguments := item.Get("arguments").String()
			if arguments == "" {
				arguments = "{}"
			}
			var parsed any
			if err := kiroDecodeJSON([]byte(arguments), &parsed); err != nil {
				conversionErr = fmt.Errorf("input function_call %s has invalid JSON arguments: %w", callID, err)
				return false
			}
			push("assistant", []any{map[string]any{"type": "tool_use", "id": callID, "name": kiroFlatToolName(item.Get("namespace").String(), item.Get("name").String()), "input": parsed}})
		case "custom_tool_call":
			callID := firstNonEmptyString(item.Get("call_id").String(), item.Get("id").String())
			push("assistant", []any{map[string]any{"type": "tool_use", "id": callID, "name": kiroFlatToolName(item.Get("namespace").String(), item.Get("name").String()), "input": map[string]any{"input": item.Get("input").String()}}})
		case "function_call_output", "custom_tool_call_output":
			push("user", []any{map[string]any{"type": "tool_result", "tool_use_id": item.Get("call_id").String(), "content": kiroStringifyOutput(item.Get("output"))}})
		case "compaction":
			if summary, ok := kiroDecodeCompactionPayload(item.Get("encrypted_content").String()); ok {
				system = append(system, kiroRestoredContextPrefix+summary)
			}
		case "reasoning", "web_search_call", "compaction_trigger":
		default:
			switch role := item.Get("role").String(); role {
			case "system", "developer":
				system = append(system, kiroCollectTexts(item.Get("content"))...)
			case "user", "assistant":
				push(role, kiroResponsesContentBlocks(item.Get("content")))
			}
		}
		return true
	})
	if conversionErr != nil {
		return nil, conversionErr
	}
	if len(merged) == 0 {
		return nil, fmt.Errorf("input must contain at least one user/assistant message")
	}
	messages := make([]any, 0, len(merged))
	for _, entry := range merged {
		messages = append(messages, map[string]any{"role": entry.role, "content": entry.blocks})
	}
	request := map[string]any{"model": out.model, "max_tokens": kiroResponsesDefaultMaxTokens, "messages": messages, "stream": true}
	if maxTokens := root.Get("max_output_tokens").Int(); maxTokens > 0 {
		request["max_tokens"] = maxTokens
	}
	var toolChoice map[string]any
	if choice := root.Get("tool_choice"); choice.Exists() {
		converted, err := kiroConvertResponsesToolChoice(choice)
		if err != nil {
			return nil, err
		}
		toolChoice = converted
		request["tool_choice"] = converted
	}
	choiceNone := toolChoice != nil && toolChoice["type"] == "none"
	hostedWebSearch := false
	for _, tool := range declared {
		if tool.Get("type").String() == "web_search" {
			hostedWebSearch = true
		}
	}
	tools := kiroConvertResponsesTools(declared, out.kinds, "")
	if !choiceNone && hostedWebSearch {
		out.webSearch = true
		if len(tools) == 0 {
			tools = append(tools, map[string]any{"name": "noop", "description": "Placeholder tool; never call this.", "input_schema": map[string]any{"type": "object", "properties": map[string]any{}}})
			system = append(system, kiroWebSearchNudgeStrict)
		} else {
			system = append(system, kiroWebSearchNudgeSoft)
		}
		tools = append(tools, map[string]any{"type": "web_search_20250305", "name": "web_search", "max_uses": 5})
	}
	if !choiceNone && len(tools) > 0 {
		request["tools"] = tools
		// Kiro 上游没有关闭并行工具调用的字段：先提示模型，流里再兜底只保留第一个调用。
		if !out.config.parallelToolCalls {
			system = append(system, kiroSingleToolCallInstruction)
		}
	}
	if len(system) > 0 {
		blocks := make([]any, 0, len(system))
		for _, text := range system {
			blocks = append(blocks, map[string]any{"type": "text", "text": text})
		}
		request["system"] = blocks
	}
	outputConfig := map[string]any{}
	if effort := strings.TrimSpace(root.Get("reasoning.effort").String()); effort != "" {
		outputConfig["effort"] = effort
		request["thinking"] = map[string]any{"type": "enabled", "budget_tokens": kiroThinkingBudgetDefault}
	}
	// text.format 接到 Messages 侧的结构化输出模拟（output_config.format）。
	switch format := root.Get("text.format"); format.Get("type").String() {
	case "json_schema":
		if schema := format.Get("schema"); schema.IsObject() {
			outputConfig["format"] = map[string]any{"type": "json_schema", "schema": json.RawMessage(schema.Raw)}
		}
	case "json_object":
		outputConfig["format"] = map[string]any{"type": "json_schema", "schema": map[string]any{"type": "object"}}
	}
	if len(outputConfig) > 0 {
		request["output_config"] = outputConfig
	}
	if sessionID := kiroResolveSessionID(root.Get("prompt_cache_key").String(), headers); sessionID != "" {
		request["metadata"] = map[string]any{"user_id": "session_" + sessionID}
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	out.messages = encoded
	return out, nil
}

// kiroResolveSessionID 从 prompt_cache_key 或会话头里取出 UUID，作为上游会话 id。
func kiroResolveSessionID(promptCacheKey string, headers http.Header) string {
	candidates := []string{promptCacheKey}
	for _, name := range []string{"x-session-affinity", "x-session-id", "x-client-request-id", "session_id"} {
		candidates = append(candidates, headers.Get(name))
	}
	for _, candidate := range candidates {
		raw := strings.TrimPrefix(strings.TrimPrefix(candidate, "session_"), "session-")
		if parsed, err := uuid.Parse(raw); err == nil {
			return parsed.String()
		}
	}
	return ""
}

func kiroConvertResponsesTools(entries []gjson.Result, kinds kiroToolKinds, namespace string) []any {
	var out []any
	for _, entry := range entries {
		toolType := entry.Get("type").String()
		if toolType == "" {
			toolType = "function"
		}
		switch toolType {
		case "function":
			fn := entry
			if entry.Get("function").Exists() {
				fn = entry.Get("function")
			}
			name := fn.Get("name").String()
			if name == "" {
				continue
			}
			schema := map[string]any{}
			if params, ok := fn.Get("parameters").Value().(map[string]any); ok {
				schema = params
			}
			flat := kiroFlatToolName(namespace, name)
			kinds[flat] = kiroDeclaredTool{name: name, namespace: namespace}
			out = append(out, map[string]any{"name": flat, "description": fn.Get("description").String(), "input_schema": schema})
		case "custom":
			name := entry.Get("name").String()
			if name == "" {
				continue
			}
			description := entry.Get("description").String()
			if definition := entry.Get("format.definition"); definition.Exists() {
				syntax := entry.Get("format.syntax").String()
				if syntax == "" {
					syntax = "grammar"
				}
				description += "\n\nInput format (" + syntax + " grammar):\n" + definition.String()
			}
			flat := kiroFlatToolName(namespace, name)
			kinds[flat] = kiroDeclaredTool{custom: true, name: name, namespace: namespace}
			out = append(out, map[string]any{"name": flat, "description": description, "input_schema": map[string]any{
				"type":                 "object",
				"properties":           map[string]any{"input": map[string]any{"type": "string", "description": "The complete raw tool input text. Do NOT wrap it in JSON or escape it."}},
				"required":             []any{"input"},
				"additionalProperties": false,
			}})
		case "namespace":
			ns := entry.Get("name").String()
			if ns == "" || namespace != "" {
				continue
			}
			var nested []gjson.Result
			entry.Get("tools").ForEach(func(_, tool gjson.Result) bool {
				nested = append(nested, tool)
				return true
			})
			converted := kiroConvertResponsesTools(nested, kinds, ns)
			if description := entry.Get("description").String(); description != "" {
				for _, tool := range converted {
					spec := tool.(map[string]any)
					spec["description"] = "[" + ns + "] " + description + "\n" + fmt.Sprint(spec["description"])
				}
			}
			out = append(out, converted...)
		}
	}
	return out
}

func kiroConvertResponsesToolChoice(choice gjson.Result) (map[string]any, error) {
	kind := choice.String()
	if choice.IsObject() {
		kind = choice.Get("type").String()
		if kind == "" {
			kind = "function"
		}
	}
	switch kind {
	case "auto":
		return map[string]any{"type": "auto"}, nil
	case "required":
		return map[string]any{"type": "any"}, nil
	case "none":
		return map[string]any{"type": "none"}, nil
	case "function", "custom":
		// custom 工具在桥里按扁平名声明成普通工具，响应时再还原成 custom_tool_call。
		if !choice.IsObject() {
			break
		}
		name := firstNonEmptyString(choice.Get("name").String(), choice.Get("function.name").String(), choice.Get("custom.name").String())
		if name == "" {
			return nil, fmt.Errorf("tool_choice %s is missing name", kind)
		}
		return map[string]any{"type": "tool", "name": kiroFlatToolName(choice.Get("namespace").String(), name)}, nil
	}
	return nil, fmt.Errorf("unsupported tool_choice value: %s", choice.Raw)
}

func kiroResponsesContentBlocks(content gjson.Result) []any {
	if content.Type == gjson.String {
		if content.String() == "" {
			return nil
		}
		return []any{map[string]any{"type": "text", "text": content.String()}}
	}
	var out []any
	content.ForEach(func(_, part gjson.Result) bool {
		switch part.Get("type").String() {
		case "input_text", "output_text", "text":
			if text := part.Get("text").String(); text != "" {
				out = append(out, map[string]any{"type": "text", "text": text})
			}
		case "input_image":
			url := part.Get("image_url").String()
			if rest, ok := strings.CutPrefix(url, "data:"); ok {
				if mediaType, data, ok := strings.Cut(rest, ";base64,"); ok {
					out = append(out, map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": mediaType, "data": data}})
				}
			} else if url != "" {
				// 与 Messages 路径一致：远程图片交给转换器换成占位文字，让模型知道少了一张图。
				out = append(out, map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": url}})
			}
		}
		return true
	})
	return out
}

func kiroCollectTexts(content gjson.Result) []string {
	if content.Type == gjson.String {
		if content.String() == "" {
			return nil
		}
		return []string{content.String()}
	}
	var out []string
	content.ForEach(func(_, part gjson.Result) bool {
		if text := part.Get("text").String(); text != "" {
			out = append(out, text)
		}
		return true
	})
	return out
}

func kiroStringifyOutput(output gjson.Result) string {
	switch {
	case output.Type == gjson.String:
		return output.String()
	case output.IsArray():
		return strings.Join(kiroCollectTexts(output), "\n")
	case output.Exists():
		return output.Raw
	}
	return ""
}

// kiroCustomInputText 从 {"input": "..."} 里取出自由文本；模型偶尔换了键名也兼容唯一字符串字段。
func kiroCustomInputText(arguments string) string {
	var parsed any
	if json.Unmarshal([]byte(arguments), &parsed) != nil {
		return arguments
	}
	switch value := parsed.(type) {
	case map[string]any:
		if text, ok := value["input"].(string); ok {
			return text
		}
		if len(value) == 1 {
			for _, only := range value {
				if text, ok := only.(string); ok {
					return text
				}
			}
		}
	case string:
		return value
	}
	return arguments
}

// ---- Anthropic 事件 → Responses 事件 ----

type kiroResponsesBlock struct {
	kind        string // text / reasoning / tool / web_search / ignored
	itemID      string
	outputIndex int
	text        strings.Builder
	callID      string
	query       string
}

type kiroResponsesStream struct {
	responseID     string
	createdAt      int64
	model          string
	kinds          kiroToolKinds
	config         kiroResponsesConfig
	sequence       int
	nextOutput     int
	blocks         map[int]*kiroResponsesBlock
	output         []any
	usage          map[string]any
	stopReason     string
	sawMessageStop bool
	terminal       bool
	failure        map[string]any
	toolCalls      int
}

func newKiroResponsesStream(translation *kiroResponsesTranslation) *kiroResponsesStream {
	return &kiroResponsesStream{
		responseID: "resp_" + strings.ReplaceAll(uuid.NewString(), "-", ""),
		createdAt:  time.Now().Unix(),
		model:      translation.model,
		kinds:      translation.kinds,
		config:     translation.config,
		blocks:     map[int]*kiroResponsesBlock{},
		usage:      map[string]any{},
	}
}

func kiroNewItemID(prefix string) string {
	return prefix + "_" + strings.ReplaceAll(uuid.NewString(), "-", "")
}

func (s *kiroResponsesStream) emit(eventType string, payload map[string]any) []byte {
	payload["type"] = eventType
	payload["sequence_number"] = s.sequence
	s.sequence++
	data, _ := json.Marshal(payload)
	return []byte("event: " + eventType + "\ndata: " + string(data) + "\n\n")
}

func (s *kiroResponsesStream) initialEvents() [][]byte {
	response := map[string]any{"id": s.responseID, "object": "response", "created_at": s.createdAt, "status": "in_progress", "model": s.model, "output": []any{}}
	return [][]byte{s.emit("response.created", map[string]any{"response": response}), s.emit("response.in_progress", map[string]any{"response": response})}
}

func (s *kiroResponsesStream) allocateOutput() int {
	index := s.nextOutput
	s.nextOutput++
	return index
}

// handle 翻译一条 Anthropic 事件。
func (s *kiroResponsesStream) handle(event kiroSSEEvent) [][]byte {
	if s.terminal {
		return nil
	}
	data := event.Data
	index, _ := data["index"].(int)
	switch event.Event {
	case "message_start":
		if message, ok := data["message"].(map[string]any); ok {
			s.updateUsage(message["usage"])
		}
	case "content_block_start":
		return s.blockStart(index, data["content_block"])
	case "content_block_delta":
		return s.blockDelta(index, data["delta"])
	case "content_block_stop":
		return s.blockStop(index)
	case "message_delta":
		s.updateUsage(data["usage"])
		if delta, ok := data["delta"].(map[string]any); ok {
			s.stopReason, _ = delta["stop_reason"].(string)
		}
	case "message_stop":
		s.sawMessageStop = true
		return s.finish()
	case "error":
		failure, _ := data["error"].(map[string]any)
		errorType, _ := failure["type"].(string)
		message, _ := failure["message"].(string)
		if errorType == "" {
			errorType = "server_error"
		}
		if message == "" {
			message = "upstream stream failed"
		}
		return [][]byte{s.fail(errorType, message)}
	}
	return nil
}

func (s *kiroResponsesStream) blockStart(index int, raw any) [][]byte {
	block, _ := raw.(map[string]any)
	state := &kiroResponsesBlock{kind: "ignored", outputIndex: -1}
	switch block["type"] {
	case "text":
		state.kind, state.itemID = "text", kiroNewItemID("msg")
	case "thinking":
		state.kind, state.itemID = "reasoning", kiroNewItemID("rs")
	case "tool_use":
		// parallel_tool_calls=false：只交出第一个工具调用，其余丢弃，模型会在下一轮重新发起。
		if !s.config.parallelToolCalls && s.toolCalls > 0 {
			break
		}
		s.toolCalls++
		state.kind = "tool"
		state.callID, _ = block["id"].(string)
		state.query, _ = block["name"].(string)
	case "server_tool_use":
		if block["name"] == "web_search" {
			state.kind = "web_search"
			state.itemID, _ = block["id"].(string)
			if state.itemID == "" {
				state.itemID = kiroNewItemID("ws")
			}
			if input, ok := block["input"].(map[string]any); ok {
				state.query, _ = input["query"].(string)
			}
			state.outputIndex = s.allocateOutput()
			s.blocks[index] = state
			return [][]byte{s.emit("response.output_item.added", map[string]any{"output_index": state.outputIndex, "item": map[string]any{
				"type": "web_search_call", "id": state.itemID, "status": "in_progress", "action": map[string]any{"type": "search", "query": state.query},
			}})}
		}
	}
	s.blocks[index] = state
	return nil
}

func (s *kiroResponsesStream) blockDelta(index int, raw any) [][]byte {
	delta, _ := raw.(map[string]any)
	block := s.blocks[index]
	if block == nil || delta == nil {
		return nil
	}
	switch delta["type"] {
	case "text_delta":
		text, _ := delta["text"].(string)
		if text == "" || block.kind != "text" {
			return nil
		}
		var out [][]byte
		if block.outputIndex < 0 {
			block.outputIndex = s.allocateOutput()
			out = append(out,
				s.emit("response.output_item.added", map[string]any{"output_index": block.outputIndex, "item": map[string]any{"type": "message", "id": block.itemID, "status": "in_progress", "role": "assistant", "content": []any{}}}),
				s.emit("response.content_part.added", map[string]any{"item_id": block.itemID, "output_index": block.outputIndex, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}}))
		}
		block.text.WriteString(text)
		return append(out, s.emit("response.output_text.delta", map[string]any{"item_id": block.itemID, "output_index": block.outputIndex, "content_index": 0, "delta": text}))
	case "thinking_delta":
		text, _ := delta["thinking"].(string)
		if text == "" || block.kind != "reasoning" {
			return nil
		}
		var out [][]byte
		if block.outputIndex < 0 {
			block.outputIndex = s.allocateOutput()
			out = append(out, s.emit("response.output_item.added", map[string]any{"output_index": block.outputIndex, "item": map[string]any{"type": "reasoning", "id": block.itemID, "summary": []any{}}}),
				// 官方 Responses 累加器按 summary_index 写入，必须先创建这一段摘要。
				s.emit("response.reasoning_summary_part.added", map[string]any{"item_id": block.itemID, "output_index": block.outputIndex, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": ""}}))
		}
		block.text.WriteString(text)
		return append(out, s.emit("response.reasoning_summary_text.delta", map[string]any{"item_id": block.itemID, "output_index": block.outputIndex, "summary_index": 0, "delta": text}))
	case "input_json_delta":
		if block.kind == "tool" {
			partial, _ := delta["partial_json"].(string)
			block.text.WriteString(partial)
		}
	}
	return nil
}

func (s *kiroResponsesStream) blockStop(index int) [][]byte {
	block := s.blocks[index]
	delete(s.blocks, index)
	if block == nil {
		return nil
	}
	text := block.text.String()
	switch block.kind {
	case "text":
		if block.outputIndex < 0 {
			return nil
		}
		item := map[string]any{"type": "message", "id": block.itemID, "status": "completed", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}}
		s.output = append(s.output, item)
		return [][]byte{
			s.emit("response.output_text.done", map[string]any{"item_id": block.itemID, "output_index": block.outputIndex, "content_index": 0, "text": text}),
			s.emit("response.content_part.done", map[string]any{"item_id": block.itemID, "output_index": block.outputIndex, "content_index": 0, "part": map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}),
			s.emit("response.output_item.done", map[string]any{"output_index": block.outputIndex, "item": item}),
		}
	case "reasoning":
		if block.outputIndex < 0 {
			return nil
		}
		item := map[string]any{"type": "reasoning", "id": block.itemID, "summary": []any{map[string]any{"type": "summary_text", "text": text}}}
		s.output = append(s.output, item)
		return [][]byte{
			s.emit("response.reasoning_summary_text.done", map[string]any{"item_id": block.itemID, "output_index": block.outputIndex, "summary_index": 0, "text": text}),
			s.emit("response.reasoning_summary_part.done", map[string]any{"item_id": block.itemID, "output_index": block.outputIndex, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": text}}),
			s.emit("response.output_item.done", map[string]any{"output_index": block.outputIndex, "item": item}),
		}
	case "tool":
		return s.finishTool(block.callID, block.query, text)
	case "web_search":
		item := map[string]any{"type": "web_search_call", "id": block.itemID, "status": "completed", "action": map[string]any{"type": "search", "query": block.query}}
		s.output = append(s.output, item)
		return [][]byte{s.emit("response.output_item.done", map[string]any{"output_index": block.outputIndex, "item": item})}
	}
	return nil
}

// finishTool 按声明种类发 function_call 或 custom_tool_call；种类不一致会让 Codex 直接终止本轮。
func (s *kiroResponsesStream) finishTool(callID, flatName, arguments string) [][]byte {
	if strings.TrimSpace(arguments) == "" {
		arguments = "{}"
	}
	outputIndex := s.allocateOutput()
	declared, ok := s.kinds.resolve(flatName)
	name, namespace, custom := flatName, "", false
	if ok {
		name, namespace, custom = declared.name, declared.namespace, declared.custom
	}
	var item, added map[string]any
	var deltaEvent, doneEvent, doneKey, value string
	if custom {
		value = kiroCustomInputText(arguments)
		item = map[string]any{"type": "custom_tool_call", "id": kiroNewItemID("ctc"), "call_id": callID, "name": name, "input": value, "status": "completed"}
		deltaEvent, doneEvent, doneKey = "response.custom_tool_call_input.delta", "response.custom_tool_call_input.done", "input"
	} else {
		value = arguments
		item = map[string]any{"type": "function_call", "id": kiroNewItemID("fc"), "call_id": callID, "name": name, "arguments": arguments, "status": "completed"}
		deltaEvent, doneEvent, doneKey = "response.function_call_arguments.delta", "response.function_call_arguments.done", "arguments"
	}
	if namespace != "" {
		item["namespace"] = namespace
	}
	added = map[string]any{}
	for key, v := range item {
		added[key] = v
	}
	added["status"] = "in_progress"
	if !custom {
		added["arguments"] = ""
	}
	s.output = append(s.output, item)
	itemID := item["id"]
	return [][]byte{
		s.emit("response.output_item.added", map[string]any{"output_index": outputIndex, "item": added}),
		s.emit(deltaEvent, map[string]any{"item_id": itemID, "output_index": outputIndex, "delta": value}),
		s.emit(doneEvent, map[string]any{"item_id": itemID, "output_index": outputIndex, doneKey: value}),
		s.emit("response.output_item.done", map[string]any{"output_index": outputIndex, "item": item}),
	}
}

func (s *kiroResponsesStream) updateUsage(raw any) {
	usage, _ := raw.(map[string]any)
	for key, value := range usage {
		s.usage[key] = value
	}
}

func kiroUsageInt(usage map[string]any, key string) int {
	switch value := usage[key].(type) {
	case int:
		return max(value, 0)
	case float64:
		return max(int(value), 0)
	}
	return 0
}

func (s *kiroResponsesStream) responseUsage() map[string]any {
	cached := kiroUsageInt(s.usage, "cache_read_input_tokens")
	input := kiroUsageInt(s.usage, "input_tokens") + kiroUsageInt(s.usage, "cache_creation_input_tokens") + cached
	output := kiroUsageInt(s.usage, "output_tokens")
	usage := map[string]any{
		"input_tokens":          input,
		"input_tokens_details":  map[string]any{"cached_tokens": cached},
		"output_tokens":         output,
		"output_tokens_details": map[string]any{"reasoning_tokens": kiroUsageInt(s.usage, "reasoning_tokens")},
		"total_tokens":          input + output,
	}
	for _, key := range []string{"credit_usage", "credit_unit", "credit_unit_plural"} {
		if value, ok := s.usage[key]; ok {
			usage[key] = value
		}
	}
	return usage
}

func (s *kiroResponsesStream) responseObject(status string) map[string]any {
	output := s.output
	if output == nil {
		output = []any{}
	}
	tools := s.config.tools
	if tools == nil {
		tools = []any{}
	}
	return map[string]any{
		"id": s.responseID, "object": "response", "created_at": s.createdAt, "status": status, "model": s.model,
		"output": output, "usage": s.responseUsage(), "parallel_tool_calls": s.config.parallelToolCalls,
		"tool_choice": s.config.toolChoice, "tools": tools,
	}
}

func kiroIncompleteReason(stopReason string) string {
	switch stopReason {
	case "max_tokens", "model_context_window_exceeded":
		return "max_output_tokens"
	case kiroRefusalStopReason:
		return "content_filter"
	}
	return ""
}

func (s *kiroResponsesStream) finish() [][]byte {
	if s.terminal {
		return nil
	}
	if !s.sawMessageStop {
		return [][]byte{s.fail("server_error", "upstream stream ended before message_stop")}
	}
	s.terminal = true
	reason := kiroIncompleteReason(s.stopReason)
	if reason == "" {
		return [][]byte{s.emit("response.completed", map[string]any{"response": s.responseObject("completed")})}
	}
	response := s.responseObject("incomplete")
	response["incomplete_details"] = map[string]any{"reason": reason}
	return [][]byte{s.emit("response.incomplete", map[string]any{"response": response})}
}

func (s *kiroResponsesStream) fail(errorType, message string) []byte {
	s.terminal = true
	response := s.responseObject("failed")
	response["error"] = map[string]any{"code": errorType, "message": message}
	s.failure = map[string]any{"type": errorType, "message": message}
	return s.emit("response.failed", map[string]any{"response": response})
}

// ExecuteKiroResponsesRequest 经 Kiro 专用桥服务 /v1/responses 与 /v1/chat/completions。
func ExecuteKiroResponsesRequest(ctx context.Context, account *auth.Account, body []byte, proxyOverride string, headers http.Header) (*http.Response, error) {
	if kiroResponsesHasCompactionTrigger(body) {
		return executeKiroCompaction(ctx, account, body, proxyOverride, headers)
	}
	translation, err := kiroResponsesToMessages(body, headers)
	if err != nil {
		return nil, ErrBadRequest(err.Error())
	}
	inbound, err := parseKiroInboundRequest(translation.messages)
	if err != nil {
		return nil, ErrBadRequest(err.Error())
	}
	// 结构化输出要求先注入，托管 web_search 路径也要生效（kiro.rs 在此只告警、原样返回）；
	// 搜索词只取用户消息，注入的 system 不影响搜索请求。
	schema := kiroInjectStructuredInstruction(inbound)
	if translation.webSearch {
		return executeKiroWebSearchResponses(ctx, account, inbound, translation, proxyOverride, schema)
	}
	conversion, err := convertKiroRequest(inbound, kiroPurposeGenerate)
	if err != nil {
		return nil, ErrBadRequest(err.Error())
	}
	upstream, failure, err := kiroOpenUpstream(ctx, account, inbound.Model, conversion, proxyOverride)
	if err != nil || failure != nil {
		return failure, err
	}
	stream := newKiroStreamContext(inbound.Model, kiroEstimateInputTokens(inbound), inbound.Thinking.enabled(), conversion)
	stream.cache = kiroComputeCacheUsage(ctx, inbound)
	if schema != nil {
		return kiroServeStructuredResponses(upstream, stream, translation, schema), nil
	}
	return kiroServeResponses(upstream, stream, translation), nil
}

// kiroServeStructuredResponses 与 Messages 路径一致：结构化输出要等全文到齐才能抽出合规 JSON，
// 流式请求也整条缓冲后再回放成 Responses 事件。
func kiroServeStructuredResponses(upstream *http.Response, stream *kiroStreamContext, translation *kiroResponsesTranslation, schema map[string]any) *http.Response {
	var events []kiroSSEEvent
	kiroDrainUpstream(upstream, stream, func(event kiroSSEEvent) error { events = append(events, event); return nil })
	responses := newKiroResponsesStream(translation)
	var body bytes.Buffer
	if translation.stream {
		for _, chunk := range responses.initialEvents() {
			body.Write(chunk)
		}
	}
	for _, event := range kiroRewriteStructuredEvents(events, schema) {
		for _, chunk := range responses.handle(event) {
			body.Write(chunk)
		}
	}
	if !translation.stream {
		return kiroResponsesJSON(responses)
	}
	if !responses.terminal {
		for _, chunk := range responses.finish() {
			body.Write(chunk)
		}
	}
	return kiroSSEResponse(body.Bytes())
}

// kiroServeResponses 把 Anthropic 事件流按客户端要求输出成 Responses SSE 或 JSON。
func kiroServeResponses(upstream *http.Response, stream *kiroStreamContext, translation *kiroResponsesTranslation) *http.Response {
	responses := newKiroResponsesStream(translation)
	if translation.stream {
		started := false
		return kiroStreamingResponse(upstream, stream, func(event kiroSSEEvent, write func([]byte) error) error {
			chunks := responses.handle(event)
			if !started {
				started = true
				chunks = append(responses.initialEvents(), chunks...)
			}
			for _, chunk := range chunks {
				if err := write(chunk); err != nil {
					return err
				}
			}
			return nil
		})
	}
	kiroDrainUpstream(upstream, stream, func(event kiroSSEEvent) error {
		responses.handle(event)
		return nil
	})
	return kiroResponsesJSON(responses)
}

// kiroResponsesJSON 输出非流式 Responses 结果；流内失败按错误类型映射状态码。
func kiroResponsesJSON(responses *kiroResponsesStream) *http.Response {
	if !responses.terminal {
		responses.finish()
	}
	if responses.failure != nil {
		encoded, _ := json.Marshal(map[string]any{"error": responses.failure})
		errorType, _ := responses.failure["type"].(string)
		return kiroSyntheticResponse(kiroFailureStatus(errorType), encoded)
	}
	response := responses.responseObject("completed")
	if reason := kiroIncompleteReason(responses.stopReason); reason != "" {
		response["status"] = "incomplete"
		response["incomplete_details"] = map[string]any{"reason": reason}
	}
	encoded, _ := json.Marshal(response)
	return kiroSyntheticResponse(http.StatusOK, encoded)
}
