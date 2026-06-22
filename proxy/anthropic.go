package proxy

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

// ==================== Anthropic Messages API 类型定义 ====================

// anthropicRequest 表示 Anthropic Messages API 请求
type anthropicRequest struct {
	Model        string                 `json:"model"`
	MaxTokens    int                    `json:"max_tokens"`
	System       json.RawMessage        `json:"system,omitempty"`
	Messages     []anthropicMessage     `json:"messages"`
	Tools        []anthropicTool        `json:"tools,omitempty"`
	Stream       bool                   `json:"stream,omitempty"`
	Temperature  *float64               `json:"temperature,omitempty"`
	TopP         *float64               `json:"top_p,omitempty"`
	StopSeqs     []string               `json:"stop_sequences,omitempty"`
	Thinking     *anthropicThinking     `json:"thinking,omitempty"`
	OutputConfig *anthropicOutputConfig `json:"output_config,omitempty"`
	ToolChoice   json.RawMessage        `json:"tool_choice,omitempty"`
	Metadata     json.RawMessage        `json:"metadata,omitempty"`
	Speed        string                 `json:"speed,omitempty"`
}

type anthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

type anthropicOutputConfig struct {
	Effort string `json:"effort,omitempty"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// anthropicContentBlock 统一内容块（text/thinking/tool_use/tool_result/image）
type anthropicContentBlock struct {
	Type      string                `json:"type"`
	Text      string                `json:"text,omitempty"`
	Thinking  string                `json:"thinking,omitempty"`
	Source    *anthropicImageSource `json:"source,omitempty"`
	ID        string                `json:"id,omitempty"`
	Name      string                `json:"name,omitempty"`
	Input     json.RawMessage       `json:"input,omitempty"`
	ToolUseID string                `json:"tool_use_id,omitempty"`
	Content   json.RawMessage       `json:"content,omitempty"`
	IsError   bool                  `json:"is_error,omitempty"`
}

type anthropicImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type anthropicTool struct {
	Type        string          `json:"type,omitempty"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// ==================== Anthropic 响应类型 ====================

// anthropicResponse 非流式响应
type anthropicResponse struct {
	ID           string                  `json:"id"`
	Type         string                  `json:"type"`
	Role         string                  `json:"role"`
	Content      []anthropicContentBlock `json:"content"`
	Model        string                  `json:"model"`
	StopReason   string                  `json:"stop_reason"`
	StopSequence *string                 `json:"stop_sequence"`
	Usage        anthropicUsage          `json:"usage"`
}

type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// ==================== Anthropic 流式事件类型 ====================

type anthropicStreamEvent struct {
	Type         string                 `json:"type"`
	Message      *anthropicResponse     `json:"message,omitempty"`
	Index        *int                   `json:"index,omitempty"`
	ContentBlock *anthropicContentBlock `json:"content_block,omitempty"`
	Delta        *anthropicDelta        `json:"delta,omitempty"`
	Usage        *anthropicUsage        `json:"usage,omitempty"`
}

type anthropicDelta struct {
	Type        string `json:"type,omitempty"`
	Text        string `json:"text,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
	Thinking    string `json:"thinking,omitempty"`
	StopReason  string `json:"stop_reason,omitempty"`
}

// ==================== 模型映射 ====================

// defaultAnthropicModelMap 默认的模型映射（当数据库中无配置时使用）
var defaultAnthropicModelMap = map[string]string{
	"claude-opus-4-6":            "gpt-5.4",
	"claude-opus-4-6-20250610":   "gpt-5.4",
	"claude-haiku-4-5-20251001":  "gpt-5.4-mini",
	"claude-haiku-4-5":           "gpt-5.4-mini",
	"claude-sonnet-4-6":          "gpt-5.3-codex",
	"claude-sonnet-4-5-20250929": "gpt-5.2",
	"claude-opus-4-5-20251101":   "gpt-5.3-codex",
	"claude-sonnet-4-5-20250514": "gpt-5.4",
	"claude-sonnet-4-5":          "gpt-5.4",
	"claude-sonnet-4.5":          "gpt-5.4",
	"claude-sonnet-4-20250514":   "gpt-5.4",
	"claude-sonnet-4":            "gpt-5.4",
	"claude-opus-4-20250514":     "gpt-5.4",
	"claude-opus-4":              "gpt-5.4",
	"claude-3-5-sonnet-20241022": "gpt-5.4",
	"claude-3-5-haiku-20241022":  "gpt-5.4-mini",
}

func canonicalizeCodexModel(model string, supportedModels []string) string {
	trimmed := strings.TrimSpace(model)
	if trimmed == "" {
		return ""
	}
	for _, supported := range supportedModels {
		if strings.EqualFold(trimmed, supported) {
			return supported
		}
	}

	lower := strings.ToLower(trimmed)
	aliases := map[string]string{
		"gpt5-5":       "gpt-5.5",
		"gpt5.5":       "gpt-5.5",
		"gpt5-4":       "gpt-5.4",
		"gpt5.4":       "gpt-5.4",
		"gpt5-4-mini":  "gpt-5.4-mini",
		"gpt5.4-mini":  "gpt-5.4-mini",
		"gpt-5.4mini":  "gpt-5.4-mini",
		"gpt5-3-codex": "gpt-5.3-codex",
		"gpt5.3-codex": "gpt-5.3-codex",
		"gpt5-2":       "gpt-5.2",
		"gpt5.2":       "gpt-5.2",
	}
	if canonical, ok := aliases[lower]; ok {
		for _, supported := range supportedModels {
			if canonical == supported {
				return canonical
			}
		}
		return trimmed
	}
	return trimmed
}

// resolveAnthropicModel 将 Anthropic 模型名解析为 Codex 模型名
// 优先使用数据库中的动态映射，回退到默认映射
func resolveAnthropicModel(model string, dynamicMappingJSON string, supportedModels []string) string {
	model = strings.TrimSpace(model)

	// 1. 尝试动态映射（从系统设置，支持精确和 * 通配）
	if mapped, ok := resolveConfiguredModelMapping(model, dynamicMappingJSON, supportedModels); ok {
		return mapped
	}

	// 2. 尝试默认映射
	if mapped, ok := defaultAnthropicModelMap[model]; ok {
		return canonicalizeCodexModel(mapped, supportedModels)
	}

	// 3. 允许直接传入 Codex 模型名
	if canonical := canonicalizeCodexModel(model, supportedModels); canonical != model || canonical != "" {
		for _, supported := range supportedModels {
			if canonical == supported {
				return canonical
			}
		}
	}

	// 4. 模糊匹配
	lower := strings.ToLower(model)
	if strings.Contains(lower, "haiku") {
		return "gpt-5.4-mini"
	}
	if strings.Contains(lower, "claude") {
		return "gpt-5.4"
	}

	// 5. 默认
	if len(supportedModels) > 0 {
		return supportedModels[0]
	}
	return "gpt-5.4"
}

// ==================== Call ID 转换 ====================

// toCodexCallID 将 Anthropic tool_use id 转换为 Codex call_id
func toCodexCallID(anthropicID string) string {
	if strings.HasPrefix(anthropicID, "fc_") {
		return anthropicID
	}
	return "fc_" + anthropicID
}

// fromCodexCallID 将 Codex call_id 转回 Anthropic tool_use id
func fromCodexCallID(codexID string) string {
	if after, ok := strings.CutPrefix(codexID, "fc_"); ok {
		if strings.HasPrefix(after, "toolu_") || strings.HasPrefix(after, "call_") {
			return after
		}
	}
	return codexID
}

// ==================== 请求翻译: Anthropic Messages → Codex Responses ====================

// TranslateAnthropicToCodex 将 Anthropic Messages 请求转换为 Codex Responses 格式
// 返回: (codex 请求体, 原始 Anthropic model 名, error)
func TranslateAnthropicToCodex(rawJSON []byte, modelMappingJSON string) ([]byte, string, error) {
	return TranslateAnthropicToCodexWithModels(rawJSON, modelMappingJSON, SupportedModels)
}

func shouldUseCodexPriorityForAnthropicSpeed(speed string) bool {
	if disableFastMode() {
		return false
	}
	return strings.ToLower(strings.TrimSpace(speed)) == "fast"
}

// TranslateAnthropicToCodexWithModels 将 Anthropic Messages 请求转换为 Codex Responses 格式
// 返回: (codex 请求体, 原始 Anthropic model 名, error)
func TranslateAnthropicToCodexWithModels(rawJSON []byte, modelMappingJSON string, supportedModels []string) ([]byte, string, error) {
	var req anthropicRequest
	if err := json.Unmarshal(rawJSON, &req); err != nil {
		return nil, "", fmt.Errorf("parse anthropic request: %w", err)
	}

	originalModel := req.Model
	codexModel := resolveAnthropicModel(req.Model, modelMappingJSON, supportedModels)

	// 构建 input 数组
	input := buildCodexInput(req.System, req.Messages)

	// 构建输出 map（对齐 PrepareResponsesBody 的字段处理）
	out := map[string]any{
		"model":   codexModel,
		"stream":  true,
		"store":   false,
		"include": []string{"reasoning.encrypted_content"},
		"input":   input,
	}

	// 注意：不设置 max_output_tokens，上游 Codex 不支持该字段

	// reasoning effort: align Claude Code /v1/messages with the Responses reasoning shape.
	out["reasoning"] = map[string]any{
		"effort":  resolveReasoningEffort(req.OutputConfig),
		"summary": "auto",
	}

	if shouldUseCodexPriorityForAnthropicSpeed(req.Speed) {
		if upstreamTier, ok := upstreamServiceTier("priority"); ok {
			out["service_tier"] = upstreamTier
		}
	}

	// tools
	if len(req.Tools) > 0 {
		out["tools"] = convertAnthropicTools(req.Tools)
	}

	// tool_choice
	if len(req.ToolChoice) > 0 {
		if tc := convertAnthropicToolChoice(req.ToolChoice); tc != nil {
			out["tool_choice"] = tc
		}
	}

	body, err := json.Marshal(out)
	if err != nil {
		return nil, "", fmt.Errorf("marshal codex request: %w", err)
	}
	return body, originalModel, nil
}

// buildCodexInput 将 Anthropic system + messages 转换为 Codex input 数组
func buildCodexInput(system json.RawMessage, messages []anthropicMessage) []any {
	var input []any

	// system prompt → developer message
	if sysText := parseAnthropicSystem(system); sysText != "" {
		input = append(input, map[string]any{
			"type": "message",
			"role": "developer",
			"content": []any{
				map[string]any{"type": "input_text", "text": sysText},
			},
		})
	}

	for _, msg := range messages {
		blocks := parseAnthropicContent(msg.Content)
		switch msg.Role {
		case "user":
			input = appendUserBlocks(input, blocks)
		case "assistant":
			input = appendAssistantBlocks(input, blocks)
		}
	}
	return input
}

// parseAnthropicSystem 解析 system 字段（string 或 []block）
func parseAnthropicSystem(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	// 尝试纯字符串
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	// 尝试块数组
	var blocks []anthropicContentBlock
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if b.Type == "text" && b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n\n")
	}
	return ""
}

// parseAnthropicContent 解析 message content（string 或 []block）
func parseAnthropicContent(raw json.RawMessage) []anthropicContentBlock {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	// 尝试纯字符串
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return []anthropicContentBlock{{Type: "text", Text: s}}
	}
	// 尝试块数组
	var blocks []anthropicContentBlock
	if json.Unmarshal(raw, &blocks) == nil {
		return blocks
	}
	return nil
}

// appendUserBlocks 将 user 消息的内容块转换并追加到 input
func appendUserBlocks(input []any, blocks []anthropicContentBlock) []any {
	var contentParts []any
	for _, b := range blocks {
		switch b.Type {
		case "text":
			contentParts = append(contentParts, map[string]any{"type": "input_text", "text": b.Text})
		case "image":
			if b.Source != nil {
				dataURI := fmt.Sprintf("data:%s;base64,%s", b.Source.MediaType, b.Source.Data)
				contentParts = append(contentParts, map[string]any{"type": "input_image", "image_url": dataURI})
			}
		case "tool_result":
			// tool_result → function_call_output（独立 item）
			output := extractToolResultText(b)
			input = append(input, map[string]any{
				"type":    "function_call_output",
				"call_id": toCodexCallID(b.ToolUseID),
				"output":  output,
			})
		}
	}
	if len(contentParts) > 0 {
		input = append(input, map[string]any{
			"type":    "message",
			"role":    "user",
			"content": contentParts,
		})
	}
	return input
}

// appendAssistantBlocks 将 assistant 消息的内容块转换并追加到 input
func appendAssistantBlocks(input []any, blocks []anthropicContentBlock) []any {
	var textParts []any
	for _, b := range blocks {
		switch b.Type {
		case "text":
			textParts = append(textParts, map[string]any{"type": "output_text", "text": b.Text})
		case "tool_use":
			// 先把已有文本作为 message 输出
			if len(textParts) > 0 {
				input = append(input, map[string]any{
					"type":    "message",
					"role":    "assistant",
					"content": textParts,
				})
				textParts = nil
			}
			args := "{}"
			if len(b.Input) > 0 {
				if cleaned := sanitizeToolInputJSON(b.Name, string(b.Input)); cleaned != "" {
					args = cleaned
				}
			}
			input = append(input, map[string]any{
				"type":      "function_call",
				"call_id":   toCodexCallID(b.ID),
				"name":      b.Name,
				"arguments": args,
			})
		case "thinking":
			// thinking 块跳过（Codex 不接受 thinking 作为输入）
		}
	}
	if len(textParts) > 0 {
		input = append(input, map[string]any{
			"type":    "message",
			"role":    "assistant",
			"content": textParts,
		})
	}
	return input
}

// extractToolResultText 从 tool_result 块提取文本输出
func extractToolResultText(b anthropicContentBlock) string {
	// content 可能是 string 或 []block
	if len(b.Content) == 0 || string(b.Content) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(b.Content, &s) == nil {
		return s
	}
	var blocks []anthropicContentBlock
	if json.Unmarshal(b.Content, &blocks) == nil {
		var parts []string
		for _, cb := range blocks {
			if cb.Type == "text" && cb.Text != "" {
				parts = append(parts, cb.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return string(b.Content)
}

// resolveReasoningEffort maps Claude output_config.effort to Responses reasoning.effort.
// Claude thinking.type/budget_tokens only indicates that thinking mode exists; it
// does not control effort on this OpenAI/Codex compatibility path.
func resolveReasoningEffort(outputConfig *anthropicOutputConfig) string {
	if outputConfig != nil && strings.TrimSpace(outputConfig.Effort) != "" {
		return normalizeReasoningEffort(outputConfig.Effort)
	}
	return "high"
}

// convertAnthropicTools 将 Anthropic 工具格式转为 Codex 格式
func convertAnthropicTools(tools []anthropicTool) []any {
	result := make([]any, 0, len(tools))
	for _, t := range tools {
		item := map[string]any{
			"type": "function",
			"name": t.Name,
		}
		if t.Description != "" {
			item["description"] = t.Description
		}
		if len(t.InputSchema) > 0 {
			var params map[string]any
			if json.Unmarshal(t.InputSchema, &params) == nil {
				sanitizeSchemaForUpstream(params)
				item["parameters"] = params
			}
		}
		result = append(result, item)
	}
	if len(result) > maxTools {
		result = result[:maxTools]
	}
	return result
}

// convertAnthropicToolChoice 将 Anthropic tool_choice 转为 Codex 格式
func convertAnthropicToolChoice(raw json.RawMessage) any {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	// 尝试对象格式
	var tc struct {
		Type string `json:"type"`
		Name string `json:"name,omitempty"`
	}
	if json.Unmarshal(raw, &tc) != nil {
		return nil
	}
	switch tc.Type {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		if tc.Name != "" {
			return map[string]any{
				"type": "function",
				"function": map[string]any{
					"name": tc.Name,
				},
			}
		}
		return "auto"
	default:
		return nil
	}
}

// ==================== 响应翻译: Codex SSE → Anthropic ====================

// anthropicStreamTranslator 有状态的流式响应翻译器（Codex → Anthropic）
type anthropicStreamTranslator struct {
	model                  string
	responseID             string
	messageStartSent       bool
	contentBlockIndex      int
	contentBlockOpen       bool
	currentBlockType       string // "text" | "thinking" | "tool_use"
	currentToolUseID       string
	currentToolUseName     string
	currentToolInputBuffer strings.Builder
	hasToolUse             bool
	inputTokens            int
	outputTokens           int
	cachedTokens           int
}

// newAnthropicStreamTranslator 创建流式翻译器
func newAnthropicStreamTranslator(model string) *anthropicStreamTranslator {
	return &anthropicStreamTranslator{
		model:      model,
		responseID: "msg_" + uuid.New().String()[:24],
	}
}

// translateEvent 将单个 Codex SSE 事件翻译为零或多个 Anthropic SSE 事件
func (t *anthropicStreamTranslator) translateEvent(eventData []byte) []anthropicStreamEvent {
	eventType := gjson.GetBytes(eventData, "type").String()

	switch eventType {
	case "response.created":
		return t.handleCreated()

	case "response.output_item.added":
		return t.handleOutputItemAdded(eventData)

	case "response.output_text.delta":
		return t.handleTextDelta(eventData)

	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		return t.handleThinkingDelta(eventData)

	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		return t.handleToolInputDelta(eventData)

	case "response.output_text.done", "response.reasoning_summary_text.done",
		"response.reasoning_text.done":
		return t.handleContentDone()

	case "response.output_item.done":
		return t.handleOutputItemDone()

	case "response.completed":
		return t.handleCompleted(eventData)

	case "response.failed":
		return t.handleFailed()

	default:
		return nil
	}
}

// handleCreated 处理 response.created → message_start
func (t *anthropicStreamTranslator) handleCreated() []anthropicStreamEvent {
	if t.messageStartSent {
		return nil
	}
	t.messageStartSent = true
	return []anthropicStreamEvent{{
		Type: "message_start",
		Message: &anthropicResponse{
			ID:    t.responseID,
			Type:  "message",
			Role:  "assistant",
			Model: t.model,
			Usage: anthropicUsage{},
		},
	}}
}

// handleOutputItemAdded 处理新的输出项（reasoning/message/function_call）
func (t *anthropicStreamTranslator) handleOutputItemAdded(data []byte) []anthropicStreamEvent {
	var events []anthropicStreamEvent

	// 确保 message_start 已发送
	if !t.messageStartSent {
		events = append(events, t.handleCreated()...)
	}

	itemType := gjson.GetBytes(data, "item.type").String()

	switch itemType {
	case "reasoning":
		// 关闭当前块
		events = append(events, t.closeCurrentBlock()...)
		idx := t.contentBlockIndex
		t.contentBlockIndex++
		t.contentBlockOpen = true
		t.currentBlockType = "thinking"
		events = append(events, anthropicStreamEvent{
			Type:  "content_block_start",
			Index: &idx,
			ContentBlock: &anthropicContentBlock{
				Type:     "thinking",
				Thinking: "",
			},
		})

	case "function_call", "custom_tool_call":
		events = append(events, t.closeCurrentBlock()...)
		callID := fromCodexCallID(gjson.GetBytes(data, "item.call_id").String())
		if callID == "" {
			callID = fromCodexCallID(gjson.GetBytes(data, "item.id").String())
		}
		name := gjson.GetBytes(data, "item.name").String()
		idx := t.contentBlockIndex
		t.contentBlockIndex++
		t.contentBlockOpen = true
		t.currentBlockType = "tool_use"
		t.currentToolUseID = callID
		t.currentToolUseName = name
		t.hasToolUse = true
		events = append(events, anthropicStreamEvent{
			Type:  "content_block_start",
			Index: &idx,
			ContentBlock: &anthropicContentBlock{
				Type:  "tool_use",
				ID:    callID,
				Name:  name,
				Input: json.RawMessage("{}"),
			},
		})

	case "message":
		// text block 延迟到第一个 delta 时打开
	}

	return events
}

// handleTextDelta 处理文本增量
func (t *anthropicStreamTranslator) handleTextDelta(data []byte) []anthropicStreamEvent {
	delta := gjson.GetBytes(data, "delta").String()
	if delta == "" {
		return nil
	}

	var events []anthropicStreamEvent

	// 确保 message_start 已发送
	if !t.messageStartSent {
		events = append(events, t.handleCreated()...)
	}

	// 懒开 text block
	if !t.contentBlockOpen || t.currentBlockType != "text" {
		events = append(events, t.closeCurrentBlock()...)
		idx := t.contentBlockIndex
		t.contentBlockIndex++
		t.contentBlockOpen = true
		t.currentBlockType = "text"
		events = append(events, anthropicStreamEvent{
			Type:  "content_block_start",
			Index: &idx,
			ContentBlock: &anthropicContentBlock{
				Type: "text",
				Text: "",
			},
		})
	}

	idx := t.contentBlockIndex - 1
	events = append(events, anthropicStreamEvent{
		Type:  "content_block_delta",
		Index: &idx,
		Delta: &anthropicDelta{
			Type: "text_delta",
			Text: delta,
		},
	})
	return events
}

// handleThinkingDelta 处理推理增量
func (t *anthropicStreamTranslator) handleThinkingDelta(data []byte) []anthropicStreamEvent {
	delta := gjson.GetBytes(data, "delta").String()
	if delta == "" {
		return nil
	}

	var events []anthropicStreamEvent
	if !t.messageStartSent {
		events = append(events, t.handleCreated()...)
	}

	// 确保 thinking block 已打开
	if !t.contentBlockOpen || t.currentBlockType != "thinking" {
		events = append(events, t.closeCurrentBlock()...)
		idx := t.contentBlockIndex
		t.contentBlockIndex++
		t.contentBlockOpen = true
		t.currentBlockType = "thinking"
		events = append(events, anthropicStreamEvent{
			Type:  "content_block_start",
			Index: &idx,
			ContentBlock: &anthropicContentBlock{
				Type:     "thinking",
				Thinking: "",
			},
		})
	}

	idx := t.contentBlockIndex - 1
	events = append(events, anthropicStreamEvent{
		Type:  "content_block_delta",
		Index: &idx,
		Delta: &anthropicDelta{
			Type:     "thinking_delta",
			Thinking: delta,
		},
	})
	return events
}

// handleToolInputDelta 缓冲工具调用参数增量。
// 不直接转发为 input_json_delta：上游模型偶尔会塞入空可选字段（如 gpt-5.5
// 给 Read 工具加 "pages":""），逐片透传后下游会看到污染后的入参。统一在
// closeCurrentBlock 时整段清洗后一次性下发。
func (t *anthropicStreamTranslator) handleToolInputDelta(data []byte) []anthropicStreamEvent {
	delta := gjson.GetBytes(data, "delta").String()
	if delta == "" {
		return nil
	}
	t.currentToolInputBuffer.WriteString(delta)
	return nil
}

// handleContentDone 处理内容完成（文本/推理块）
func (t *anthropicStreamTranslator) handleContentDone() []anthropicStreamEvent {
	return t.closeCurrentBlock()
}

// handleOutputItemDone 处理输出项完成
func (t *anthropicStreamTranslator) handleOutputItemDone() []anthropicStreamEvent {
	return t.closeCurrentBlock()
}

// handleCompleted 处理 response.completed → message_delta + message_stop
func (t *anthropicStreamTranslator) handleCompleted(data []byte) []anthropicStreamEvent {
	var events []anthropicStreamEvent

	if !t.messageStartSent {
		events = append(events, t.handleCreated()...)
	}

	events = append(events, t.closeCurrentBlock()...)

	// 提取 usage
	usage := gjson.GetBytes(data, "response.usage")
	if usage.Exists() {
		// OpenAI Responses 的 input_tokens 含缓存命中部分，而 Anthropic Messages
		// 的 input_tokens 不含缓存（缓存另计在 cache_read_input_tokens）。
		// 直接透传会把缓存 token 重复计入，导致费用偏高，因此这里扣除缓存部分。
		t.cachedTokens = int(usage.Get("input_tokens_details.cached_tokens").Int())
		t.inputTokens = max(int(usage.Get("input_tokens").Int())-t.cachedTokens, 0)
		t.outputTokens = int(usage.Get("output_tokens").Int())
	}

	// 确定 stop_reason
	stopReason := "end_turn"
	status := gjson.GetBytes(data, "response.status").String()
	if status == "incomplete" {
		reason := gjson.GetBytes(data, "response.incomplete_details.reason").String()
		if reason == "max_output_tokens" {
			stopReason = "max_tokens"
		}
	}
	if t.hasToolUse && stopReason == "end_turn" {
		stopReason = "tool_use"
	}

	events = append(events, anthropicStreamEvent{
		Type: "message_delta",
		Delta: &anthropicDelta{
			Type:       "message_delta",
			StopReason: stopReason,
		},
		Usage: &anthropicUsage{
			InputTokens:          t.inputTokens,
			OutputTokens:         t.outputTokens,
			CacheReadInputTokens: t.cachedTokens,
		},
	})

	events = append(events, anthropicStreamEvent{Type: "message_stop"})
	return events
}

// handleFailed 处理 response.failed
func (t *anthropicStreamTranslator) handleFailed() []anthropicStreamEvent {
	var events []anthropicStreamEvent
	if !t.messageStartSent {
		events = append(events, t.handleCreated()...)
	}
	events = append(events, t.closeCurrentBlock()...)
	events = append(events, anthropicStreamEvent{
		Type: "message_delta",
		Delta: &anthropicDelta{
			Type:       "message_delta",
			StopReason: "end_turn",
		},
		Usage: &anthropicUsage{},
	})
	events = append(events, anthropicStreamEvent{Type: "message_stop"})
	return events
}

// closeCurrentBlock 关闭当前打开的 content block。
// 关闭 tool_use 块时会先把累积的 arguments JSON 整段清洗（删除空字符串/null
// 的可选字段），再作为单次 input_json_delta 下发。
func (t *anthropicStreamTranslator) closeCurrentBlock() []anthropicStreamEvent {
	if !t.contentBlockOpen {
		return nil
	}
	t.contentBlockOpen = false
	idx := t.contentBlockIndex - 1

	var events []anthropicStreamEvent
	if t.currentBlockType == "tool_use" && t.currentToolInputBuffer.Len() > 0 {
		cleaned := sanitizeToolInputJSON(t.currentToolUseName, t.currentToolInputBuffer.String())
		if cleaned != "" {
			events = append(events, anthropicStreamEvent{
				Type:  "content_block_delta",
				Index: &idx,
				Delta: &anthropicDelta{
					Type:        "input_json_delta",
					PartialJSON: cleaned,
				},
			})
		}
		t.currentToolInputBuffer.Reset()
	}

	events = append(events, anthropicStreamEvent{
		Type:  "content_block_stop",
		Index: &idx,
	})
	return events
}

// finalize 在流结束时补齐缺失的事件
func (t *anthropicStreamTranslator) finalize() []anthropicStreamEvent {
	var events []anthropicStreamEvent
	if !t.messageStartSent {
		events = append(events, t.handleCreated()...)
	}
	events = append(events, t.closeCurrentBlock()...)
	events = append(events, anthropicStreamEvent{
		Type: "message_delta",
		Delta: &anthropicDelta{
			Type:       "message_delta",
			StopReason: "end_turn",
		},
		Usage: &anthropicUsage{
			InputTokens:  t.inputTokens,
			OutputTokens: t.outputTokens,
		},
	})
	events = append(events, anthropicStreamEvent{Type: "message_stop"})
	return events
}

// anthropicEventToSSE 将 Anthropic 事件序列化为 SSE 格式
func anthropicEventToSSE(evt anthropicStreamEvent) string {
	data, _ := json.Marshal(evt)
	return fmt.Sprintf("event: %s\ndata: %s\n\n", evt.Type, data)
}

// ==================== 非流式响应构建 ====================

type anthropicResponseAccumulator struct {
	response *anthropicResponse
	content  []anthropicContentBlock
}

func newAnthropicResponseAccumulator(model string) *anthropicResponseAccumulator {
	return &anthropicResponseAccumulator{
		response: &anthropicResponse{
			ID:      "msg_" + uuid.New().String()[:24],
			Type:    "message",
			Role:    "assistant",
			Content: []anthropicContentBlock{},
			Model:   model,
		},
	}
}

func (a *anthropicResponseAccumulator) apply(events []anthropicStreamEvent) {
	for _, evt := range events {
		switch evt.Type {
		case "message_start":
			if evt.Message != nil {
				msg := *evt.Message
				msg.Content = []anthropicContentBlock{}
				a.response = &msg
				a.content = a.content[:0]
			}
		case "content_block_start":
			if evt.Index == nil || evt.ContentBlock == nil {
				continue
			}
			a.ensureContentIndex(*evt.Index)
			a.content[*evt.Index] = *evt.ContentBlock
		case "content_block_delta":
			if evt.Index == nil || evt.Delta == nil {
				continue
			}
			a.ensureContentIndex(*evt.Index)
			switch evt.Delta.Type {
			case "text_delta":
				a.content[*evt.Index].Text += evt.Delta.Text
			case "thinking_delta":
				a.content[*evt.Index].Thinking += evt.Delta.Thinking
			case "input_json_delta":
				a.content[*evt.Index].Input = json.RawMessage(evt.Delta.PartialJSON)
			}
		case "message_delta":
			if evt.Delta != nil && evt.Delta.StopReason != "" {
				a.response.StopReason = evt.Delta.StopReason
			}
			if evt.Usage != nil {
				a.response.Usage = *evt.Usage
			}
		}
	}
}

func (a *anthropicResponseAccumulator) ensureContentIndex(idx int) {
	for len(a.content) <= idx {
		a.content = append(a.content, anthropicContentBlock{})
	}
}

func (a *anthropicResponseAccumulator) build(completedData []byte) *anthropicResponse {
	fallback := buildAnthropicResponseFromCompleted(completedData, a.response.Model)
	resp := *a.response
	resp.Content = compactAnthropicContent(a.content)

	if len(resp.Content) == 0 && len(fallback.Content) > 0 {
		return fallback
	}
	if resp.StopReason == "" {
		resp.StopReason = fallback.StopReason
	}
	if resp.StopReason == "" {
		resp.StopReason = "end_turn"
	}
	if resp.Usage == (anthropicUsage{}) {
		resp.Usage = fallback.Usage
	}
	return &resp
}

func compactAnthropicContent(content []anthropicContentBlock) []anthropicContentBlock {
	out := make([]anthropicContentBlock, 0, len(content))
	for _, block := range content {
		switch block.Type {
		case "text":
			if block.Text == "" {
				continue
			}
		case "thinking":
			if block.Thinking == "" {
				continue
			}
		case "tool_use":
			if block.Input == nil || !json.Valid(block.Input) {
				block.Input = json.RawMessage("{}")
			}
		default:
			continue
		}
		out = append(out, block)
	}
	return out
}

// buildAnthropicResponseFromCompleted 从 response.completed 事件构建完整的 Anthropic 响应
func buildAnthropicResponseFromCompleted(completedData []byte, model string) *anthropicResponse {
	responseID := "msg_" + uuid.New().String()[:24]

	resp := &anthropicResponse{
		ID:    responseID,
		Type:  "message",
		Role:  "assistant",
		Model: model,
	}

	// 提取 output 数组
	outputs := gjson.GetBytes(completedData, "response.output")
	if !outputs.Exists() || !outputs.IsArray() {
		return resp
	}

	var content []anthropicContentBlock
	lastBlockIsToolUse := false

	outputs.ForEach(func(_, item gjson.Result) bool {
		itemType := item.Get("type").String()
		switch itemType {
		case "reasoning":
			// reasoning → thinking block
			summaryText := ""
			item.Get("summary").ForEach(func(_, s gjson.Result) bool {
				if s.Get("type").String() == "summary_text" {
					summaryText += s.Get("text").String()
				}
				return true
			})
			if summaryText != "" {
				content = append(content, anthropicContentBlock{
					Type:     "thinking",
					Thinking: summaryText,
				})
				lastBlockIsToolUse = false
			}

		case "message":
			// message → text block(s)
			item.Get("content").ForEach(func(_, part gjson.Result) bool {
				if part.Get("type").String() == "output_text" {
					text := part.Get("text").String()
					if text != "" {
						content = append(content, anthropicContentBlock{
							Type: "text",
							Text: text,
						})
						lastBlockIsToolUse = false
					}
				}
				return true
			})

		case "function_call", "custom_tool_call":
			// function_call/custom_tool_call → tool_use block
			callID := fromCodexCallID(item.Get("call_id").String())
			if callID == "" {
				callID = fromCodexCallID(item.Get("id").String())
			}
			name := item.Get("name").String()
			args := item.Get("arguments").String()
			if itemType == "custom_tool_call" {
				args = item.Get("input").String()
			}
			if cleaned := sanitizeToolInputJSON(name, args); cleaned != "" {
				args = cleaned
			} else {
				args = "{}"
			}
			content = append(content, anthropicContentBlock{
				Type:  "tool_use",
				ID:    callID,
				Name:  name,
				Input: json.RawMessage(args),
			})
			lastBlockIsToolUse = true
		}
		return true
	})

	resp.Content = content

	// 确定 stop_reason
	status := gjson.GetBytes(completedData, "response.status").String()
	switch status {
	case "incomplete":
		reason := gjson.GetBytes(completedData, "response.incomplete_details.reason").String()
		if reason == "max_output_tokens" {
			resp.StopReason = "max_tokens"
		} else {
			resp.StopReason = "end_turn"
		}
	default:
		if lastBlockIsToolUse {
			resp.StopReason = "tool_use"
		} else {
			resp.StopReason = "end_turn"
		}
	}

	// usage
	usage := gjson.GetBytes(completedData, "response.usage")
	if usage.Exists() {
		// 见流式分支说明：扣除缓存命中部分，避免缓存 token 被重复计入。
		cached := int(usage.Get("input_tokens_details.cached_tokens").Int())
		input := max(int(usage.Get("input_tokens").Int())-cached, 0)
		resp.Usage = anthropicUsage{
			InputTokens:          input,
			OutputTokens:         int(usage.Get("output_tokens").Int()),
			CacheReadInputTokens: cached,
		}
	}

	return resp
}
