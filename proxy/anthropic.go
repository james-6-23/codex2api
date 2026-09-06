package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

var anthropicJSONNull = json.RawMessage("null")

const anthropicStreamPingEveryNDeltas = 40

func anthropicStopReason(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func anthropicStopReasonValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// ==================== Anthropic Messages API 类型定义 ====================

// anthropicRequest 表示 Anthropic Messages API 请求
type anthropicRequest struct {
	Model        string                 `json:"model"`
	MaxTokens    *int                   `json:"max_tokens"`
	System       json.RawMessage        `json:"system,omitempty"`
	Messages     []anthropicMessage     `json:"messages"`
	Tools        []anthropicTool        `json:"tools,omitempty"`
	Stream       bool                   `json:"stream,omitempty"`
	Temperature  *float64               `json:"temperature,omitempty"`
	TopP         *float64               `json:"top_p,omitempty"`
	TopK         json.RawMessage        `json:"top_k,omitempty"`
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
	Effort string          `json:"effort,omitempty"`
	Format json.RawMessage `json:"format,omitempty"`
}

type anthropicMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// anthropicContentBlock 统一内容块（text/thinking/tool_use/tool_result/image）
type anthropicContentBlock struct {
	Type         string                 `json:"type"`
	Text         string                 `json:"text,omitempty"`
	Thinking     string                 `json:"thinking,omitempty"`
	Signature    string                 `json:"signature,omitempty"`
	Source       *anthropicImageSource  `json:"source,omitempty"`
	ID           string                 `json:"id,omitempty"`
	Name         string                 `json:"name,omitempty"`
	Input        json.RawMessage        `json:"input,omitempty"`
	ToolUseID    string                 `json:"tool_use_id,omitempty"`
	Content      json.RawMessage        `json:"content,omitempty"`
	IsError      bool                   `json:"is_error,omitempty"`
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

type anthropicCacheControl struct {
	Type string `json:"type,omitempty"`
}

func (b anthropicContentBlock) MarshalJSON() ([]byte, error) {
	type contentBlock struct {
		Type      string                `json:"type"`
		Text      *string               `json:"text,omitempty"`
		Thinking  *string               `json:"thinking,omitempty"`
		Signature string                `json:"signature,omitempty"`
		Source    *anthropicImageSource `json:"source,omitempty"`
		ID        string                `json:"id,omitempty"`
		Name      string                `json:"name,omitempty"`
		Input     json.RawMessage       `json:"input,omitempty"`
		ToolUseID string                `json:"tool_use_id,omitempty"`
		Content   json.RawMessage       `json:"content,omitempty"`
		IsError   bool                  `json:"is_error,omitempty"`
	}

	out := contentBlock{
		Type:      b.Type,
		Signature: b.Signature,
		Source:    b.Source,
		ID:        b.ID,
		Name:      b.Name,
		Input:     b.Input,
		ToolUseID: b.ToolUseID,
		Content:   b.Content,
		IsError:   b.IsError,
	}
	if b.Type == "text" || b.Text != "" {
		out.Text = &b.Text
	}
	if b.Type == "thinking" || b.Thinking != "" {
		out.Thinking = &b.Thinking
	}
	return json.Marshal(out)
}

type anthropicImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

type anthropicTool struct {
	Type         string                 `json:"type,omitempty"`
	Name         string                 `json:"name"`
	Description  string                 `json:"description,omitempty"`
	InputSchema  json.RawMessage        `json:"input_schema"`
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

// ==================== Anthropic 响应类型 ====================

// anthropicResponse 非流式响应
type anthropicResponse struct {
	ID           string                  `json:"id"`
	Type         string                  `json:"type"`
	Role         string                  `json:"role"`
	Content      []anthropicContentBlock `json:"content"`
	Model        string                  `json:"model"`
	StopReason   *string                 `json:"stop_reason"`
	StopSequence *string                 `json:"stop_sequence"`
	StopDetails  json.RawMessage         `json:"stop_details"`
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
	Type         string          `json:"type,omitempty"`
	Text         string          `json:"text,omitempty"`
	PartialJSON  string          `json:"partial_json,omitempty"`
	Thinking     string          `json:"thinking,omitempty"`
	Signature    string          `json:"signature,omitempty"`
	StopReason   *string         `json:"stop_reason,omitempty"`
	StopSequence json.RawMessage `json:"stop_sequence,omitempty"`
	StopDetails  json.RawMessage `json:"stop_details,omitempty"`
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
	if id, _, custom := parseAnthropicCustomToolID(anthropicID); custom {
		return id
	}
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
	return strings.ToLower(strings.TrimSpace(speed)) == "fast"
}

// TranslateAnthropicToCodexWithModels 将 Anthropic Messages 请求转换为 Codex Responses 格式
// 返回: (codex 请求体, 原始 Anthropic model 名, error)
func TranslateAnthropicToCodexWithModels(rawJSON []byte, modelMappingJSON string, supportedModels []string) ([]byte, string, error) {
	return translateAnthropicToResponses(rawJSON, modelMappingJSON, supportedModels, false)
}

// TranslateAnthropicToResponsesForGrok converts Messages into a canonical
// Responses request for Grok routing while retaining all request controls that
// Responses can represent. The existing Codex converter intentionally omits
// these controls because the Codex endpoint rejects them.
func TranslateAnthropicToResponsesForGrok(rawJSON []byte, modelMappingJSON string, supportedModels []string) ([]byte, string, error) {
	return translateAnthropicToResponses(rawJSON, modelMappingJSON, supportedModels, true)
}

func translateAnthropicToResponses(rawJSON []byte, modelMappingJSON string, supportedModels []string, preserveControls bool) ([]byte, string, error) {
	var req anthropicRequest
	if err := json.Unmarshal(rawJSON, &req); err != nil {
		return nil, "", fmt.Errorf("parse anthropic request: %w", err)
	}

	originalModel := req.Model
	codexModel := resolveAnthropicModel(req.Model, modelMappingJSON, supportedModels)
	if err := validateAnthropicToolReferences(req.Messages); err != nil {
		return nil, originalModel, err
	}

	if err := validateAnthropicCustomToolHistory(req.Messages); err != nil {
		return nil, originalModel, err
	}
	// Grok 吃自动前缀缓存，system 必须按块拆开；Codex 仍拼成一条 developer。
	input := buildCodexInput(req.System, req.Messages)
	if preserveControls {
		input = buildGrokInput(req.System, req.Messages)
	}

	// 构建输出 map（对齐 PrepareResponsesBody 的字段处理）
	out := map[string]any{
		"model":   codexModel,
		"stream":  true,
		"store":   false,
		"include": []string{"reasoning.encrypted_content"},
		"input":   input,
	}
	if preserveControls {
		// Official Grok CLI include list: encrypted reasoning plus no_inline_citations.
		out["include"] = []string{"reasoning.encrypted_content", "no_inline_citations"}
	}

	// 注意：不设置 max_output_tokens，上游 Codex 不支持该字段

	// reasoning: Codex 固定 summary=auto。Grok 首轮保持 high+detailed，
	// 工具续轮和无工具小请求降一档，避免每轮都付满思考。
	effort := resolveReasoningEffort(req.OutputConfig, codexModel)
	summary := "auto"
	if preserveControls {
		effort, summary = resolveGrokReasoningControls(req, rawJSON, codexModel)
	}
	out["reasoning"] = map[string]any{
		"effort":  effort,
		"summary": summary,
	}

	if shouldUseCodexPriorityForAnthropicSpeed(req.Speed) {
		if upstreamTier, ok := upstreamServiceTier("priority"); ok {
			out["service_tier"] = upstreamTier
		}
	}

	// tools。Grok 前缀缓存会吃整表工具定义；按会话记住首次顺序，
	// 中途新出现的工具只追加在表尾，且不把 cache_control 传给上游。
	if len(req.Tools) > 0 {
		if preserveControls {
			out["tools"] = convertAnthropicToolsForGrok(req.Tools, grokToolOrderKey(rawJSON))
		} else {
			out["tools"] = convertAnthropicTools(req.Tools)
		}
	}

	// tool_choice
	if len(req.ToolChoice) > 0 {
		if tc := convertAnthropicToolChoice(req.ToolChoice); tc != nil {
			out["tool_choice"] = tc
		}
	}

	if preserveControls {
		if err := copyAnthropicResponsesControls(out, req); err != nil {
			return nil, originalModel, err
		}
	}

	restoreAnthropicCustomToolDeclarations(out)
	body, err := json.Marshal(out)
	if err != nil {
		return nil, "", fmt.Errorf("marshal codex request: %w", err)
	}
	return body, originalModel, nil
}

func copyAnthropicResponsesControls(out map[string]any, req anthropicRequest) error {
	if req.MaxTokens != nil {
		out["max_output_tokens"] = *req.MaxTokens
	}
	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if len(req.TopK) > 0 && strings.TrimSpace(string(req.TopK)) != "null" {
		return fmt.Errorf("Messages top_k cannot be represented by Responses")
	}
	if req.StopSeqs != nil {
		out["stop"] = append([]string(nil), req.StopSeqs...)
	}

	if len(req.ToolChoice) > 0 && strings.TrimSpace(string(req.ToolChoice)) != "null" {
		choice := convertAnthropicToolChoice(req.ToolChoice)
		if choice == nil {
			return fmt.Errorf("Messages tool_choice cannot be represented by Responses")
		}
		if choiceMap, ok := choice.(map[string]any); ok {
			function, _ := choiceMap["function"].(map[string]any)
			name := strings.TrimSpace(firstNonEmptyAnyString(choiceMap["name"]))
			if name == "" {
				name = strings.TrimSpace(firstNonEmptyAnyString(function["name"]))
			}
			if name == "" {
				return fmt.Errorf("Messages tool_choice type tool requires name")
			}
			choice = map[string]any{"type": "function", "name": name}
		}
		out["tool_choice"] = choice
	}

	format, err := anthropicOutputFormatToResponses(req.OutputConfig)
	if err != nil {
		return err
	}
	if format != nil {
		out["text"] = map[string]any{"format": format}
	}
	return nil
}

func anthropicOutputFormatToResponses(config *anthropicOutputConfig) (map[string]any, error) {
	if config == nil || len(config.Format) == 0 || strings.TrimSpace(string(config.Format)) == "null" {
		return nil, nil
	}
	var format map[string]any
	if json.Unmarshal(config.Format, &format) != nil || format == nil {
		return nil, fmt.Errorf("Messages output_config.format must be an object")
	}
	formatType := strings.TrimSpace(firstNonEmptyAnyString(format["type"]))
	switch formatType {
	case "text", "json_object":
		return map[string]any{"type": formatType}, nil
	case "json_schema":
		// The Grok Messages wire shape is {type,json_schema?} in some clients,
		// but its canonical xAI type is {type,schema}. Accept both spellings.
		schema := format["schema"]
		name := "structured_output"
		strict := true
		if nested, ok := format["json_schema"].(map[string]any); ok {
			if schema == nil {
				schema = nested["schema"]
			}
			if value := strings.TrimSpace(firstNonEmptyAnyString(nested["name"])); value != "" {
				name = value
			}
			if value, ok := nested["strict"].(bool); ok {
				strict = value
			}
		}
		if value := strings.TrimSpace(firstNonEmptyAnyString(format["name"])); value != "" {
			name = value
		}
		if value, ok := format["strict"].(bool); ok {
			strict = value
		}
		if schema == nil {
			return nil, fmt.Errorf("Messages output_config.format json_schema requires schema")
		}
		return map[string]any{"type": "json_schema", "name": name, "schema": schema, "strict": strict}, nil
	default:
		return nil, fmt.Errorf("Messages output_config.format type %q cannot be represented by Responses", formatType)
	}
}

// validateAnthropicToolReferences rejects request semantics that cannot be
// represented safely in Responses. A detached tool_result must not be silently
// converted into ordinary user text because doing so changes tool causality.
func validateAnthropicToolReferences(messages []anthropicMessage) error {
	known := make(map[string]struct{})
	for messageIndex, message := range messages {
		blocks := parseAnthropicContent(message.Content)
		for blockIndex, block := range blocks {
			switch block.Type {
			case "tool_use":
				id := strings.TrimSpace(block.ID)
				if id == "" {
					return fmt.Errorf("messages[%d].content[%d] tool_use requires id", messageIndex, blockIndex)
				}
				known[id] = struct{}{}
			case "tool_result":
				id := strings.TrimSpace(block.ToolUseID)
				if id == "" {
					return fmt.Errorf("messages[%d].content[%d] tool_result requires tool_use_id", messageIndex, blockIndex)
				}
				if _, ok := known[id]; !ok {
					return fmt.Errorf("messages[%d].content[%d] orphan tool_result for tool_use_id %q", messageIndex, blockIndex, id)
				}
			}
		}
	}
	return nil
}

func grokDeveloperMessage(text string) map[string]any {
	return map[string]any{
		"type": "message",
		"role": "developer",
		"content": []any{
			map[string]any{"type": "input_text", "text": text},
		},
	}
}

func anthropicSystemBlockCached(block anthropicContentBlock) bool {
	return block.CacheControl != nil && strings.TrimSpace(block.CacheControl.Type) != ""
}

func appendTranslatedMessages(input []any, messages []anthropicMessage) []any {
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

// buildCodexInput 将 Anthropic system + messages 转换为 Codex input 数组
func buildCodexInput(system json.RawMessage, messages []anthropicMessage) []any {
	var input []any
	if sysText := parseAnthropicSystem(system); sysText != "" {
		input = append(input, grokDeveloperMessage(sysText))
	}
	return appendTranslatedMessages(input, messages)
}

// buildGrokInput 把 system 按块拆成多条 developer 消息：带 cache_control
// 的静态块在前、动态块在后。Grok /v1/responses 没有 cache_control 字段，
// 只靠 input 前缀自动缓存，拼成一条文本会让动态尾巴把整段前缀打穿。
func buildGrokInput(system json.RawMessage, messages []anthropicMessage) []any {
	return appendTranslatedMessages(appendGrokSystemInput(nil, system), messages)
}

func appendGrokSystemInput(input []any, system json.RawMessage) []any {
	var cached, uncached []string
	for _, block := range parseAnthropicSystemBlocks(system) {
		if block.Type != "text" || block.Text == "" {
			continue
		}
		if anthropicSystemBlockCached(block) {
			cached = append(cached, block.Text)
			continue
		}
		uncached = append(uncached, block.Text)
	}
	for _, text := range cached {
		input = append(input, grokDeveloperMessage(text))
	}
	for _, text := range uncached {
		input = append(input, grokDeveloperMessage(text))
	}
	return input
}

// parseAnthropicSystem 解析 system 字段（string 或 []block）
func parseAnthropicSystem(raw json.RawMessage) string {
	var parts []string
	for _, block := range parseAnthropicSystemBlocks(raw) {
		if block.Type == "text" && block.Text != "" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n\n")
}

func parseAnthropicSystemBlocks(raw json.RawMessage) []anthropicContentBlock {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if s == "" {
			return nil
		}
		return []anthropicContentBlock{{Type: "text", Text: s}}
	}
	var blocks []anthropicContentBlock
	if json.Unmarshal(raw, &blocks) == nil {
		return blocks
	}
	return nil
}

// stableAnthropicSystemSeed 只取带 cache_control 的静态 system 文本，
// 避免 Claude Code 每轮刷新的 git/date 块把 Grok session 种子打漂。
func stableAnthropicSystemSeed(raw []byte) string {
	blocks := parseAnthropicSystemBlocks(raw)
	if len(blocks) == 0 {
		return strings.TrimSpace(string(raw))
	}
	var cached, all []string
	for _, block := range blocks {
		if block.Type != "text" || block.Text == "" {
			continue
		}
		all = append(all, block.Text)
		if anthropicSystemBlockCached(block) {
			cached = append(cached, block.Text)
		}
	}
	if len(cached) > 0 {
		return strings.Join(cached, "\n\n")
	}
	return strings.Join(all, "\n\n")
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
			images := extractToolResultImages(b)
			if output == "" && len(images) > 0 {
				// 纯图片工具结果：function_call_output 不能为空，留占位指向随后的 user 消息。
				output = toolResultImageMovedMarker
			}
			callID := toCodexCallID(b.ToolUseID)
			outputType := "function_call_output"
			if _, _, custom := parseAnthropicCustomToolID(b.ToolUseID); custom {
				outputType = "custom_tool_call_output"
			}
			input = append(input, map[string]any{
				"type":    outputType,
				"call_id": callID,
				"output":  output,
			})
			// Chat/Responses 协议下 function_call_output 只能是文本，图片视觉模型看不见；
			// 抽出来放进紧随其后的合成 user 消息，附带 call 归属说明。
			if len(images) > 0 {
				mediaParts := []any{map[string]any{
					"type": "input_text",
					"text": fmt.Sprintf(toolResultImageAttribution, callID),
				}}
				for _, dataURI := range images {
					mediaParts = append(mediaParts, map[string]any{"type": "input_image", "image_url": dataURI})
				}
				input = append(input, map[string]any{
					"type":    "message",
					"role":    "user",
					"content": mediaParts,
				})
			}
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
			if id, namespace, custom := parseAnthropicCustomToolID(b.ID); custom {
				item := map[string]any{"type": "custom_tool_call", "call_id": id, "name": b.Name, "input": gjson.GetBytes(b.Input, "input").String()}
				if namespace != "" {
					item["namespace"] = namespace
				}
				input = append(input, item)
				continue
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
			// 带 signature 的 thinking 块重建为 reasoning item 回传：signature 即
			// 输出侧下发的 reasoning encrypted_content，同账号上游可解，保留完整
			// 推理上下文。无 signature 的块仍跳过——凭空构造的 reasoning item
			// 无密文可解，上游可能拒绝。
			if b.Signature == "" {
				continue
			}
			if len(textParts) > 0 {
				input = append(input, map[string]any{
					"type":    "message",
					"role":    "assistant",
					"content": textParts,
				})
				textParts = nil
			}
			summary := []any{}
			if b.Thinking != "" {
				summary = append(summary, map[string]any{"type": "summary_text", "text": b.Thinking})
			}
			input = append(input, map[string]any{
				"type":              "reasoning",
				"summary":           summary,
				"encrypted_content": b.Signature,
			})
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

const (
	toolResultImageMovedMarker = "[Tool output image moved to the following user message]"
	toolResultImageAttribution = "[Tool output image for call %s]"
)

// extractToolResultImages 从 tool_result 块的 content 中提取图片，返回 data URI 列表。
// 只识别 content 为 []block 时的 image 块（Anthropic base64 source）；字符串或非法
// content 不含结构化图片，返回 nil。图片能力缺失时该函数是无副作用的 no-op。
func extractToolResultImages(b anthropicContentBlock) []string {
	if len(b.Content) == 0 || string(b.Content) == "null" {
		return nil
	}
	var blocks []anthropicContentBlock
	if json.Unmarshal(b.Content, &blocks) != nil {
		return nil
	}
	var images []string
	for _, cb := range blocks {
		if cb.Type == "image" && cb.Source != nil && cb.Source.Data != "" {
			images = append(images, fmt.Sprintf("data:%s;base64,%s", cb.Source.MediaType, cb.Source.Data))
		}
	}
	return images
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
func resolveReasoningEffort(outputConfig *anthropicOutputConfig, model string) string {
	if outputConfig != nil && strings.TrimSpace(outputConfig.Effort) != "" {
		return normalizeReasoningEffortForModel(outputConfig.Effort, model)
	}
	return "high"
}

const grokSmallNoToolsBodyLimit = 12 << 10

func anthropicMessagesHaveToolResult(messages []anthropicMessage) bool {
	for _, message := range messages {
		for _, block := range parseAnthropicContent(message.Content) {
			if block.Type == "tool_result" {
				return true
			}
		}
	}
	return false
}

func anthropicMessagesHaveAssistant(messages []anthropicMessage) bool {
	for _, message := range messages {
		if message.Role == "assistant" {
			return true
		}
	}
	return false
}

func capReasoningEffort(effort, cap string) string {
	rank := map[string]int{
		"minimal": 0,
		"low":     1,
		"medium":  2,
		"high":    3,
		"xhigh":   4,
		"max":     4,
	}
	if rank[strings.ToLower(strings.TrimSpace(effort))] > rank[strings.ToLower(strings.TrimSpace(cap))] {
		return cap
	}
	return effort
}

// resolveGrokReasoningControls 按轮次降思考：
//   - 带 tool_result 的续轮：Claude Code 已经选定工具，Grok 只需填参数/读结果，medium+auto
//   - 无 tools、无 assistant、体很小：并行小请求（标题/摘要/侧问），low+auto
//   - 其余首轮提问：保持 high+detailed
//
// 客户端显式写了更低的 effort 时不往上抬。
func resolveGrokReasoningControls(req anthropicRequest, rawJSON []byte, model string) (effort, summary string) {
	effort = resolveReasoningEffort(req.OutputConfig, model)
	policy := currentGrokFollowUpEffortConfig()
	if !policy.Enabled {
		return effort, "detailed"
	}
	if anthropicMessagesHaveToolResult(req.Messages) {
		return capReasoningEffort(effort, policy.ToolEffort), "auto"
	}
	if len(req.Tools) == 0 && !anthropicMessagesHaveAssistant(req.Messages) && len(rawJSON) <= grokSmallNoToolsBodyLimit {
		return capReasoningEffort(effort, policy.SmallEffort), "auto"
	}
	return effort, "detailed"
}

func convertAnthropicToolsForGrok(tools []anthropicTool, orderKey string) []any {
	return stabilizeGrokToolOrder(orderKey, convertAnthropicTools(tools))
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
	model                     string
	responseID                string
	messageStartSent          bool
	contentBlockIndex         int
	contentBlockOpen          bool
	currentBlockType          string // "text" | "thinking" | "tool_use"
	currentToolUseID          string
	currentToolUseName        string
	currentToolInputBuffer    strings.Builder
	currentToolCustom         bool
	currentToolInputFinalized bool
	toolInputError            error
	hasToolUse                bool
	inputTokens               int
	outputTokens              int
	cachedTokens              int
	pingAfterStartSent        bool
	deltasSincePing           int
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
	if t.toolInputError != nil {
		return nil
	}
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
	case "response.custom_tool_call_input.done":
		if t.currentToolCustom && t.contentBlockOpen {
			return t.finishCustomToolInput(gjson.GetBytes(eventData, "input"))
		}
		return nil

	case "response.output_text.done", "response.reasoning_summary_text.done",
		"response.reasoning_text.done":
		return t.handleContentDone()

	case "response.output_item.done":
		return t.handleOutputItemDone(eventData)

	// response.incomplete 是 max_output_tokens 截断的正常终态，走同一条收尾：
	// handleCompleted 已按 response.status/incomplete_details 推导 stop_reason。
	case "response.completed", "response.incomplete":
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
			ID:      t.responseID,
			Type:    "message",
			Role:    "assistant",
			Content: []anthropicContentBlock{},
			Model:   t.model,
			Usage:   anthropicUsage{},
		},
	}}
}

func (t *anthropicStreamTranslator) startContentBlock(block anthropicContentBlock) []anthropicStreamEvent {
	idx := t.contentBlockIndex
	t.contentBlockIndex++
	t.contentBlockOpen = true
	events := []anthropicStreamEvent{{
		Type:         "content_block_start",
		Index:        &idx,
		ContentBlock: &block,
	}}
	if !t.pingAfterStartSent {
		t.pingAfterStartSent = true
		events = append(events, anthropicStreamEvent{Type: "ping"})
	}
	return events
}

func (t *anthropicStreamTranslator) contentBlockDelta(delta anthropicDelta) []anthropicStreamEvent {
	idx := t.contentBlockIndex - 1
	events := []anthropicStreamEvent{{
		Type:  "content_block_delta",
		Index: &idx,
		Delta: &delta,
	}}
	t.deltasSincePing++
	if t.deltasSincePing >= anthropicStreamPingEveryNDeltas {
		t.deltasSincePing = 0
		events = append(events, anthropicStreamEvent{Type: "ping"})
	}
	return events
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
		t.currentBlockType = "thinking"
		events = append(events, t.startContentBlock(anthropicContentBlock{
			Type:     "thinking",
			Thinking: "",
		})...)

	case "function_call", "custom_tool_call":
		events = append(events, t.closeCurrentBlock()...)
		callID := fromCodexCallID(gjson.GetBytes(data, "item.call_id").String())
		if callID == "" {
			callID = fromCodexCallID(gjson.GetBytes(data, "item.id").String())
		}
		name := gjson.GetBytes(data, "item.name").String()
		t.currentBlockType = "tool_use"
		t.currentToolUseID = callID
		t.currentToolUseName = name
		t.currentToolCustom = itemType == "custom_tool_call"
		t.currentToolInputFinalized = false
		if t.currentToolCustom {
			id := gjson.GetBytes(data, "item.call_id").String()
			if id == "" {
				id = gjson.GetBytes(data, "item.id").String()
			}
			callID = anthropicCustomToolID(id, gjson.GetBytes(data, "item.namespace").String())
			t.currentToolUseID = callID
		}
		t.hasToolUse = true
		events = append(events, t.startContentBlock(anthropicContentBlock{
			Type:  "tool_use",
			ID:    callID,
			Name:  name,
			Input: json.RawMessage("{}"),
		})...)
		if t.currentToolCustom {
			events = append(events, t.contentBlockDelta(anthropicDelta{Type: "input_json_delta", PartialJSON: `{"input":"`})...)
		}

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
		t.currentBlockType = "text"
		events = append(events, t.startContentBlock(anthropicContentBlock{
			Type: "text",
			Text: "",
		})...)
	}

	events = append(events, t.contentBlockDelta(anthropicDelta{
		Type: "text_delta",
		Text: delta,
	})...)
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
		t.currentBlockType = "thinking"
		events = append(events, t.startContentBlock(anthropicContentBlock{
			Type:     "thinking",
			Thinking: "",
		})...)
	}

	events = append(events, t.contentBlockDelta(anthropicDelta{
		Type:     "thinking_delta",
		Thinking: delta,
	})...)
	return events
}

// handleToolInputDelta 按官方 Anthropic 形状把 arguments 碎片转成
// input_json_delta。空可选字段的清洗留到非流式聚合（compactAnthropicContent /
// buildAnthropicResponseFromCompleted），避免把整段 JSON 攒完再一次吐给 Claude Code。
func (t *anthropicStreamTranslator) handleToolInputDelta(data []byte) []anthropicStreamEvent {
	delta := gjson.GetBytes(data, "delta").String()
	if delta == "" {
		return nil
	}
	if t.currentToolCustom {
		return t.customToolInputDelta(delta)
	}
	t.currentToolInputBuffer.WriteString(delta)
	return t.contentBlockDelta(anthropicDelta{
		Type:        "input_json_delta",
		PartialJSON: delta,
	})
}

// handleContentDone 处理内容完成（文本/推理块）
func (t *anthropicStreamTranslator) handleContentDone() []anthropicStreamEvent {
	// thinking 块不在 *_text.done 关闭：reasoning item 的 encrypted_content 要到
	// output_item.done 才出现，提前关块就发不出 signature_delta。多段 summary part
	// 也因此合并进同一个 thinking 块。
	if t.contentBlockOpen && t.currentBlockType == "thinking" {
		return nil
	}
	return t.closeCurrentBlock()
}

// handleOutputItemDone 处理输出项完成。
// reasoning item 携带 encrypted_content 时先发 signature_delta 再关块：
// signature 即密文本身，输入侧据此重建 reasoning item 回传上游。
func (t *anthropicStreamTranslator) handleOutputItemDone(data []byte) []anthropicStreamEvent {
	if t.contentBlockOpen && t.currentToolCustom && t.currentBlockType == "tool_use" && gjson.GetBytes(data, "item.type").String() == "custom_tool_call" {
		events := t.finishCustomToolInput(gjson.GetBytes(data, "item.input"))
		if t.toolInputError != nil {
			return nil
		}
		return append(events, t.closeCurrentBlock()...)
	}
	if t.contentBlockOpen && t.currentBlockType == "thinking" &&
		gjson.GetBytes(data, "item.type").String() == "reasoning" {
		if sig := gjson.GetBytes(data, "item.encrypted_content").String(); sig != "" {
			events := t.contentBlockDelta(anthropicDelta{
				Type:      "signature_delta",
				Signature: sig,
			})
			return append(events, t.closeCurrentBlock()...)
		}
	}
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
			StopReason:   anthropicStopReason(stopReason),
			StopSequence: anthropicJSONNull,
			StopDetails:  anthropicJSONNull,
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
			StopReason:   anthropicStopReason("end_turn"),
			StopSequence: anthropicJSONNull,
			StopDetails:  anthropicJSONNull,
		},
		Usage: &anthropicUsage{},
	})
	events = append(events, anthropicStreamEvent{Type: "message_stop"})
	return events
}

// closeCurrentBlock 关闭当前打开的 content block。
// tool_use 的 arguments 已在 handleToolInputDelta 按碎片下发，这里只关块。
func (t *anthropicStreamTranslator) closeCurrentBlock() []anthropicStreamEvent {
	if !t.contentBlockOpen {
		return nil
	}
	var events []anthropicStreamEvent
	if t.currentBlockType == "tool_use" && t.currentToolCustom && !t.currentToolInputFinalized {
		events = append(events, t.finishCustomToolInput(gjson.Result{})...)
	}
	t.contentBlockOpen = false
	t.currentToolCustom = false
	idx := t.contentBlockIndex - 1
	t.currentToolInputBuffer.Reset()

	return append(events, anthropicStreamEvent{Type: "content_block_stop", Index: &idx})
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
			case "signature_delta":
				a.content[*evt.Index].Signature += evt.Delta.Signature
			case "input_json_delta":
				prev := a.content[*evt.Index].Input
				fragment := []byte(evt.Delta.PartialJSON)
				if len(prev) == 0 || bytes.Equal(bytes.TrimSpace(prev), []byte("{}")) {
					a.content[*evt.Index].Input = json.RawMessage(fragment)
				} else {
					merged := make([]byte, 0, len(prev)+len(fragment))
					merged = append(merged, prev...)
					merged = append(merged, fragment...)
					a.content[*evt.Index].Input = merged
				}
			}
		case "message_delta":
			if evt.Delta != nil && evt.Delta.StopReason != nil {
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
	if anthropicStopReasonValue(resp.StopReason) == "" {
		resp.StopReason = fallback.StopReason
	}
	if anthropicStopReasonValue(resp.StopReason) == "" {
		resp.StopReason = anthropicStopReason("end_turn")
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
			// 只有 signature 没有摘要文本的块也保留：密文是回传推理上下文的载体。
			if block.Thinking == "" && block.Signature == "" {
				continue
			}
		case "tool_use":
			if cleaned := sanitizeToolInputJSON(block.Name, string(block.Input)); cleaned != "" {
				block.Input = json.RawMessage(cleaned)
			} else if block.Input == nil || !json.Valid(block.Input) {
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
		ID:      responseID,
		Type:    "message",
		Role:    "assistant",
		Content: []anthropicContentBlock{},
		Model:   model,
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
			// reasoning → thinking block（encrypted_content 作为 signature 下发，
			// 供客户端多轮回传重建推理上下文）
			summaryText := ""
			item.Get("summary").ForEach(func(_, s gjson.Result) bool {
				if s.Get("type").String() == "summary_text" {
					summaryText += s.Get("text").String()
				}
				return true
			})
			signature := item.Get("encrypted_content").String()
			if summaryText != "" || signature != "" {
				content = append(content, anthropicContentBlock{
					Type:      "thinking",
					Thinking:  summaryText,
					Signature: signature,
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
				args = string(wrappedCustomToolInput(item.Get("input").String()))
				id := item.Get("call_id").String()
				if id == "" {
					id = item.Get("id").String()
				}
				callID = anthropicCustomToolID(id, item.Get("namespace").String())
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
			resp.StopReason = anthropicStopReason("max_tokens")
		} else {
			resp.StopReason = anthropicStopReason("end_turn")
		}
	default:
		if lastBlockIsToolUse {
			resp.StopReason = anthropicStopReason("tool_use")
		} else {
			resp.StopReason = anthropicStopReason("end_turn")
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
