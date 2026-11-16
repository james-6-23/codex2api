package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/google/uuid"
)

// Anthropic Messages → Kiro generateAssistantResponse 请求，逐段移植自
// ZyphrZero/kiro.rs src/anthropic/converter.rs（raw 工具兼容模式）。

const (
	kiroToolNameMaxLen          = 63
	kiroToolDescriptionMaxChars = 10000
	kiroThinkingBudgetDefault   = 20000
	kiroThinkingBudgetMax       = 24576

	kiroClientSystemPromptOpen  = "<client_system_prompt>"
	kiroClientSystemPromptClose = "</client_system_prompt>"
	kiroClientSystemPromptAck   = "Understood. I will follow the client system prompt above."
	kiroThinkingConfigAck       = "Understood. I will use the thinking configuration above."
	kiroInterruptedToolResult   = "No result was received for this tool call; it was interrupted before it returned."
	kiroPlaceholderToolDesc     = "Unavailable: this tool appeared earlier in the conversation but is not available now. Do not call it."
	kiroCompactionHistoryTool   = "kiro_compaction_history_tool"
	// kiroThinkingSignaturePlaceholder 是上游没给签名时网关补的占位签名，回放时必须丢弃。
	kiroThinkingSignaturePlaceholder = "kiro-rs-thinking-signature"
)

type kiroConversionPurpose int

const (
	kiroPurposeGenerate kiroConversionPurpose = iota
	kiroPurposeCompact
	kiroPurposeCompactFallback
)

// ---- 入站 Anthropic 请求 ----

type kiroInboundRequest struct {
	Model        string                   `json:"model"`
	MaxTokens    int                      `json:"max_tokens"`
	Messages     []kiroInboundMessage     `json:"messages"`
	Stream       bool                     `json:"stream"`
	System       json.RawMessage          `json:"system"`
	Tools        []kiroInboundTool        `json:"tools"`
	ToolChoice   json.RawMessage          `json:"tool_choice"`
	Thinking     *kiroInboundThinking     `json:"thinking"`
	OutputConfig *kiroInboundOutputConfig `json:"output_config"`
	Metadata     *kiroInboundMetadata     `json:"metadata"`
	raw          []byte
}

type kiroInboundMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type kiroInboundTool struct {
	Type        string          `json:"type,omitempty"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`
}

type kiroInboundThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

func (t *kiroInboundThinking) enabled() bool {
	return t != nil && (t.Type == "enabled" || t.Type == "adaptive")
}

type kiroInboundOutputConfig struct {
	Effort string          `json:"effort,omitempty"`
	Format json.RawMessage `json:"format,omitempty"`
}

type kiroInboundMetadata struct {
	UserID string `json:"user_id,omitempty"`
}

func parseKiroInboundRequest(body []byte) (*kiroInboundRequest, error) {
	var req kiroInboundRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, fmt.Errorf("Messages 请求体无法解析: %w", err)
	}
	req.raw = body
	if req.Thinking != nil {
		if req.Thinking.BudgetTokens <= 0 {
			req.Thinking.BudgetTokens = kiroThinkingBudgetDefault
		}
		if req.Thinking.BudgetTokens > kiroThinkingBudgetMax {
			req.Thinking.BudgetTokens = kiroThinkingBudgetMax
		}
	}
	kiroApplyThinkingSuffix(&req)
	return &req, nil
}

// kiroApplyThinkingSuffix：模型名带 -thinking 时覆写 thinking 配置（兼容旧客户端）。
func kiroApplyThinkingSuffix(req *kiroInboundRequest) {
	base, ok := strings.CutSuffix(strings.ToLower(strings.TrimSpace(req.Model)), "-thinking")
	if !ok {
		return
	}
	if kiroMapModel(base) == "claude-opus-4.6" {
		req.Thinking = &kiroInboundThinking{Type: "adaptive", BudgetTokens: kiroThinkingBudgetDefault}
		if req.OutputConfig == nil {
			req.OutputConfig = &kiroInboundOutputConfig{}
		}
		if strings.TrimSpace(req.OutputConfig.Effort) == "" {
			req.OutputConfig.Effort = "high"
		}
		return
	}
	req.Thinking = &kiroInboundThinking{Type: "enabled", BudgetTokens: kiroThinkingBudgetDefault}
}

// systemTexts 接受字符串或 [{type,text}] 两种写法。
func (r *kiroInboundRequest) systemTexts() []string {
	if len(r.System) == 0 {
		return nil
	}
	var text string
	if json.Unmarshal(r.System, &text) == nil {
		return []string{text}
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(r.System, &blocks) != nil {
		return nil
	}
	out := make([]string, 0, len(blocks))
	for _, block := range blocks {
		out = append(out, block.Text)
	}
	return out
}

// kiroContent 把 content 解成「字符串或块数组」。
func kiroContent(raw json.RawMessage) (text string, isString bool, blocks []map[string]any) {
	if len(raw) == 0 {
		return "", false, nil
	}
	if json.Unmarshal(raw, &text) == nil {
		return text, true, nil
	}
	_ = kiroDecodeJSON(raw, &blocks)
	return "", false, blocks
}

// kiroDecodeJSON 保留数字原文：透传的工具参数与历史经 float64 往返会丢失大整数精度。
func kiroDecodeJSON(data []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	// More 只判断当前数组 / 对象里还有没有元素，顶层多出的 } 或 ] 要靠再读一次确认已到 EOF。
	if err := decoder.Decode(new(json.RawMessage)); err != io.EOF {
		return fmt.Errorf("unexpected data after JSON value")
	}
	return nil
}

func kiroString(block map[string]any, key string) string {
	value, _ := block[key].(string)
	return value
}

// ---- 出站 Kiro 请求 ----

type kiroRequest struct {
	ConversationState            kiroConversationState `json:"conversationState"`
	ProfileARN                   string                `json:"profileArn,omitempty"`
	AdditionalModelRequestFields *kiroAdditionalFields `json:"additionalModelRequestFields,omitempty"`
}

// kiroAdditionalFields 外层 camelCase，内层 output_config 保持 snake_case（真实线上格式）。
type kiroAdditionalFields struct {
	OutputConfig *kiroEffort `json:"output_config,omitempty"`
	Reasoning    *kiroEffort `json:"reasoning,omitempty"`
}

type kiroEffort struct {
	Effort string `json:"effort"`
}

type kiroConversationState struct {
	AgentContinuationID string             `json:"agentContinuationId,omitempty"`
	AgentTaskType       string             `json:"agentTaskType,omitempty"`
	ChatTriggerType     string             `json:"chatTriggerType,omitempty"`
	CurrentMessage      kiroCurrentMessage `json:"currentMessage"`
	ConversationID      string             `json:"conversationId"`
	History             []kiroHistoryEntry `json:"history,omitempty"`
}

type kiroCurrentMessage struct {
	UserInputMessage kiroUserMessage `json:"userInputMessage"`
}

type kiroUserMessage struct {
	UserInputMessageContext *kiroUserContext `json:"userInputMessageContext,omitempty"`
	Content                 string           `json:"content"`
	ModelID                 string           `json:"modelId"`
	Images                  []kiroImage      `json:"images,omitempty"`
	Origin                  string           `json:"origin,omitempty"`
}

type kiroUserContext struct {
	EnvState    kiroEnvState     `json:"envState"`
	ToolResults []kiroToolResult `json:"toolResults,omitempty"`
	Tools       []kiroTool       `json:"tools,omitempty"`
}

type kiroEnvState struct {
	OperatingSystem         string `json:"operatingSystem"`
	CurrentWorkingDirectory string `json:"currentWorkingDirectory"`
}

type kiroImage struct {
	Format string          `json:"format"`
	Source kiroImageSource `json:"source"`
}

type kiroImageSource struct {
	Bytes string `json:"bytes"`
}

type kiroToolResult struct {
	ToolUseID string           `json:"toolUseId"`
	Content   []map[string]any `json:"content"`
	Status    string           `json:"status,omitempty"`
	IsError   bool             `json:"isError,omitempty"`
}

func newKiroToolResult(id, text string, isError bool) kiroToolResult {
	status := "success"
	if isError {
		status = "error"
	}
	return kiroToolResult{ToolUseID: id, Content: []map[string]any{{"text": text}}, Status: status, IsError: isError}
}

func (r kiroToolResult) text() string {
	parts := make([]string, 0, len(r.Content))
	for _, item := range r.Content {
		if text, ok := item["text"].(string); ok {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

type kiroTool struct {
	ToolSpecification kiroToolSpecification `json:"toolSpecification"`
}

type kiroToolSpecification struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema kiroInputSchema `json:"inputSchema"`
}

type kiroInputSchema struct {
	JSON any `json:"json"`
}

type kiroHistoryEntry struct {
	User      *kiroUserMessage      `json:"userInputMessage,omitempty"`
	Assistant *kiroAssistantMessage `json:"assistantResponseMessage,omitempty"`
}

type kiroAssistantMessage struct {
	Content          string                `json:"content"`
	ToolUses         []kiroToolUse         `json:"toolUses,omitempty"`
	ReasoningContent *kiroReasoningContent `json:"reasoningContent,omitempty"`
}

type kiroToolUse struct {
	ToolUseID string `json:"toolUseId"`
	Name      string `json:"name"`
	Input     any    `json:"input"`
}

// kiroReasoningContent 按 serde 外部标签序列化：{"reasoningText":{...}} 或 {"redactedContent":"..."}。
type kiroReasoningContent struct {
	ReasoningText   *kiroReasoningText `json:"reasoningText,omitempty"`
	RedactedContent string             `json:"redactedContent,omitempty"`
}

type kiroReasoningText struct {
	Text      string `json:"text"`
	Signature string `json:"signature,omitempty"`
}

var kiroEnvCurrentDirectory = func() string {
	if cwd, err := os.Getwd(); err == nil && cwd != "" {
		return cwd
	}
	return "/"
}()

func newKiroUserContext() *kiroUserContext {
	return &kiroUserContext{EnvState: kiroEnvState{OperatingSystem: "macos", CurrentWorkingDirectory: kiroEnvCurrentDirectory}}
}

// kiroConversion 是一次转换的结果；ToolNameMap 记录 Kiro 侧短名 → 客户端原名。
type kiroConversion struct {
	Request        kiroRequest
	ModelID        string
	ToolNameMap    map[string]string
	KnownToolNames map[string]struct{}
}

// kiroImageDedup 在整个会话里只发第一次出现的图片原图（上游按内容前缀缓存）。
type kiroImageDedup map[string]string

type kiroConverter struct {
	req         *kiroInboundRequest
	modelID     string
	purpose     kiroConversionPurpose
	toolNameMap map[string]string
	dedup       kiroImageDedup
}

// convertKiroRequest 把已解析的 Messages 请求转换成 Kiro 请求。
func convertKiroRequest(req *kiroInboundRequest, purpose kiroConversionPurpose) (*kiroConversion, error) {
	modelID := kiroMapModel(req.Model)
	if modelID == "" {
		return nil, fmt.Errorf("无效模型 ID: %q", req.Model)
	}
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("消息列表为空")
	}
	messages := kiroNormalizeSystemMessages(req.Messages)
	if len(messages) == 0 {
		return nil, fmt.Errorf("消息列表为空")
	}
	// Kiro 不支持续写：末尾是 assistant（prefill）时截到最后一条 user 重新生成。
	if messages[len(messages)-1].Role == "assistant" {
		last := -1
		for i := len(messages) - 1; i >= 0; i-- {
			if messages[i].Role == "user" {
				last = i
				break
			}
		}
		if last < 0 {
			return nil, fmt.Errorf("消息列表为空")
		}
		messages = messages[:last+1]
	}
	if purpose == kiroPurposeGenerate {
		messages = kiroMergeTrailingUserMessages(messages)
	}

	conversationID := ""
	if req.Metadata != nil {
		conversationID = kiroExtractSessionID(req.Metadata.UserID)
	}
	if conversationID == "" {
		conversationID = kiroDeriveConversationID(req)
	}
	c := &kiroConverter{req: req, modelID: modelID, purpose: purpose, toolNameMap: map[string]string{}, dedup: kiroImageDedup{}}

	fallback := purpose == kiroPurposeCompactFallback
	var tools []kiroTool
	known := map[string]struct{}{}
	if !fallback {
		tools = c.convertTools()
		for _, tool := range req.Tools {
			known[tool.Name] = struct{}{}
		}
	}

	history, err := c.buildHistory(messages)
	if err != nil {
		return nil, err
	}
	var images []kiroImage
	text, toolResults := c.processUserContent(messages[len(messages)-1].Content, &images)
	var validated []kiroToolResult
	if purpose != kiroPurposeGenerate {
		if err := kiroValidateCompactionToolSequence(history, toolResults); err != nil {
			return nil, err
		}
		validated = toolResults
	} else {
		if resent := kiroResentToolResultNotes(history, toolResults); len(resent) > 0 {
			if text != "" {
				resent = append(resent, text)
			}
			text = strings.Join(resent, "\n")
		}
		var orphaned map[string]struct{}
		validated, orphaned = kiroValidateToolPairing(history, toolResults)
		validated = append(validated, kiroPairOrphanedToolUses(history, orphaned)...)
	}

	// Kiro 要求历史里引用过的工具都在 tools 列表中（名称比较忽略大小写）。
	historyTools := kiroCollectHistoryToolNames(history)
	if fallback {
		if len(historyTools) > 0 {
			tool := kiroPlaceholderTool(kiroCompactionHistoryTool)
			tool.ToolSpecification.Description = "Inert placeholder for structured tool calls in compacted history. Do not call it."
			tools = append(tools, tool)
			known[kiroCompactionHistoryTool] = struct{}{}
		}
	} else {
		existing := map[string]struct{}{}
		for _, tool := range tools {
			existing[strings.ToLower(tool.ToolSpecification.Name)] = struct{}{}
		}
		for _, name := range historyTools {
			if _, ok := existing[strings.ToLower(name)]; !ok {
				tools = append(tools, kiroPlaceholderTool(name))
			}
		}
	}
	for short := range c.toolNameMap {
		known[short] = struct{}{}
	}

	context := newKiroUserContext()
	context.Tools = tools
	context.ToolResults = validated
	current := kiroUserMessage{UserInputMessageContext: context, Content: text, ModelID: modelID, Images: images, Origin: "AI_EDITOR"}
	state := kiroConversationState{
		AgentContinuationID: uuid.NewSHA1(uuid.NameSpaceOID, []byte("agent-continuation:"+conversationID)).String(),
		AgentTaskType:       "vibe",
		ChatTriggerType:     "MANUAL",
		CurrentMessage:      kiroCurrentMessage{UserInputMessage: current},
		ConversationID:      conversationID,
		History:             history,
	}
	return &kiroConversion{
		Request:        kiroRequest{ConversationState: state, AdditionalModelRequestFields: kiroBuildAdditionalFields(req, modelID)},
		ModelID:        modelID,
		ToolNameMap:    c.toolNameMap,
		KnownToolNames: known,
	}, nil
}

// kiroNormalizeSystemMessages：role=system 的文本消息原位变成 <system-reminder> user 消息，
// 空 system 消息与未知角色丢弃。
func kiroNormalizeSystemMessages(messages []kiroInboundMessage) []kiroInboundMessage {
	out := make([]kiroInboundMessage, 0, len(messages))
	for _, message := range messages {
		switch message.Role {
		case "user", "assistant":
			out = append(out, message)
		case "system":
			text, isString, blocks := kiroContent(message.Content)
			if !isString {
				parts := []string{}
				for _, block := range blocks {
					if value := kiroString(block, "text"); value != "" {
						parts = append(parts, value)
					}
				}
				text = strings.Join(parts, "\n")
			}
			if strings.TrimSpace(text) == "" {
				continue
			}
			content, _ := json.Marshal([]map[string]any{{"type": "text", "text": "<system-reminder>\n" + text + "\n</system-reminder>"}})
			out = append(out, kiroInboundMessage{Role: "user", Content: content})
		}
	}
	return out
}

// kiroMergeTrailingUserMessages：tool_result 后紧跟的客户端提醒并进当前消息，
// 否则历史里会多一条伪造的 assistant 回复。
func kiroMergeTrailingUserMessages(messages []kiroInboundMessage) []kiroInboundMessage {
	start := 0
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role != "user" {
			start = i + 1
			break
		}
	}
	if len(messages)-start < 2 {
		return messages
	}
	var blocks []map[string]any
	for _, message := range messages[start:] {
		text, isString, parts := kiroContent(message.Content)
		if isString {
			blocks = append(blocks, map[string]any{"type": "text", "text": text})
		} else {
			blocks = append(blocks, parts...)
		}
	}
	content, _ := json.Marshal(blocks)
	out := append([]kiroInboundMessage(nil), messages[:start]...)
	return append(out, kiroInboundMessage{Role: "user", Content: content})
}

func kiroExtractSessionID(userID string) string {
	var parsed struct {
		SessionID string `json:"session_id"`
	}
	if json.Unmarshal([]byte(userID), &parsed) == nil && kiroIsUUID(parsed.SessionID) {
		return parsed.SessionID
	}
	if idx := strings.Index(userID, "session_"); idx >= 0 {
		rest := userID[idx+len("session_"):]
		if len(rest) >= 36 && kiroIsUUID(rest[:36]) {
			return rest[:36]
		}
	}
	return ""
}

func kiroIsUUID(value string) bool {
	if len(value) != 36 {
		return false
	}
	_, err := uuid.Parse(value)
	return err == nil
}

// kiroDeriveConversationID 没有会话 id 时按系统提示 + 首条 user 派生稳定 id，
// 让按会话固定账号的粘性路由能命中。
func kiroDeriveConversationID(req *kiroInboundRequest) string {
	hasher := sha256.New()
	hasher.Write([]byte("kiro-rs.derived-conversation.v1\x00"))
	for _, text := range req.systemTexts() {
		hasher.Write([]byte(text))
		hasher.Write([]byte{0})
	}
	hasher.Write([]byte{1})
	for _, message := range req.Messages {
		if message.Role != "user" {
			continue
		}
		text, isString, blocks := kiroContent(message.Content)
		if isString {
			hasher.Write([]byte(text))
			hasher.Write([]byte{0})
		}
		for _, block := range blocks {
			if value, ok := block["text"].(string); ok {
				hasher.Write([]byte(value))
			} else if source, ok := block["source"].(map[string]any); ok && kiroString(source, "data") != "" {
				hasher.Write([]byte(kiroString(source, "data")))
			} else if value := kiroString(block, "type"); value != "" {
				hasher.Write([]byte(value))
			}
			hasher.Write([]byte{0})
		}
		break
	}
	digest := hex.EncodeToString(hasher.Sum(nil))
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(digest)).String()
}

// ---- 工具 ----

func kiroShortenToolName(name string) string {
	sum := sha256.Sum256([]byte(name))
	prefixMax := kiroToolNameMaxLen - 1 - 8
	cut := min(prefixMax, len(name))
	for cut > 0 && !isUTF8Boundary(name, cut) {
		cut--
	}
	return name[:cut] + "_" + hex.EncodeToString(sum[:])[:8]
}

func isUTF8Boundary(value string, index int) bool {
	return index >= len(value) || value[index]&0xC0 != 0x80
}

func (c *kiroConverter) mapToolName(name string) string {
	if len(name) <= kiroToolNameMaxLen {
		return name
	}
	short := kiroShortenToolName(name)
	c.toolNameMap[short] = name
	return short
}

func (c *kiroConverter) convertTools() []kiroTool {
	seen := map[string]struct{}{}
	out := make([]kiroTool, 0, len(c.req.Tools))
	for _, tool := range c.req.Tools {
		name := c.mapToolName(tool.Name)
		// Kiro 工具名大小写不敏感：首个出现者胜出。
		if _, dup := seen[strings.ToLower(name)]; dup {
			continue
		}
		seen[strings.ToLower(name)] = struct{}{}
		description := tool.Description
		if strings.TrimSpace(description) == "" {
			description = tool.Name
		}
		if runes := []rune(description); len(runes) > kiroToolDescriptionMaxChars {
			description = string(runes[:kiroToolDescriptionMaxChars])
		}
		var schema any
		_ = json.Unmarshal(tool.InputSchema, &schema)
		out = append(out, kiroTool{ToolSpecification: kiroToolSpecification{Name: name, Description: description, InputSchema: kiroInputSchema{JSON: kiroNormalizeJSONSchema(schema)}}})
	}
	return out
}

func kiroPlaceholderTool(name string) kiroTool {
	return kiroTool{ToolSpecification: kiroToolSpecification{Name: name, Description: kiroPlaceholderToolDesc, InputSchema: kiroInputSchema{JSON: map[string]any{
		"$schema": "http://json-schema.org/draft-07/schema#", "type": "object", "properties": map[string]any{}, "required": []any{}, "additionalProperties": true,
	}}}}
}

// kiroNormalizeJSONSchema：Kiro 只接受 Draft 07 且顶层必须是 object，不合规会 400。
func kiroNormalizeJSONSchema(schema any) map[string]any {
	obj, ok := schema.(map[string]any)
	if !ok {
		return map[string]any{"type": "object", "properties": map[string]any{}, "required": []any{}, "additionalProperties": true}
	}
	delete(obj, "$schema")
	hadProperties := obj["properties"] != nil
	for _, combinator := range []string{"oneOf", "anyOf", "allOf"} {
		variants, ok := obj[combinator].([]any)
		delete(obj, combinator)
		if !ok || hadProperties || obj["properties"] != nil {
			continue
		}
		for _, variant := range variants {
			fields, ok := variant.(map[string]any)
			if !ok || fields["type"] != "object" {
				continue
			}
			for _, key := range []string{"properties", "required", "additionalProperties", "description"} {
				if value, exists := fields[key]; exists {
					if _, present := obj[key]; !present {
						obj[key] = value
					}
				}
			}
			break
		}
	}
	obj["type"] = "object"
	properties := map[string]any{}
	if props, ok := obj["properties"].(map[string]any); ok {
		for key, value := range props {
			properties[key] = kiroNormalizePropertySchema(value)
		}
	}
	obj["properties"] = properties
	required := []any{}
	if items, ok := obj["required"].([]any); ok {
		for _, item := range items {
			if name, ok := item.(string); ok {
				required = append(required, name)
			}
		}
	}
	obj["required"] = required
	switch obj["additionalProperties"].(type) {
	case bool, map[string]any:
	default:
		obj["additionalProperties"] = true
	}
	return obj
}

func kiroNormalizePropertySchema(schema any) any {
	obj, ok := schema.(map[string]any)
	if !ok {
		return schema
	}
	delete(obj, "$schema")
	for _, key := range []string{"exclusiveMinimum", "exclusiveMaximum"} {
		if _, isNumber := obj[key].(float64); isNumber {
			delete(obj, key)
		}
	}
	for _, key := range []string{"maximum", "minimum"} {
		if value, ok := obj[key].(float64); ok && (value > 2_147_483_647 || value < -2_147_483_648) {
			delete(obj, key)
		}
	}
	if props, ok := obj["properties"].(map[string]any); ok {
		for key, value := range props {
			props[key] = kiroNormalizePropertySchema(value)
		}
	}
	if items, exists := obj["items"]; exists {
		obj["items"] = kiroNormalizePropertySchema(items)
	}
	return obj
}

// ---- 内容块 ----

func kiroUnsupportedBlockPlaceholder(block map[string]any) string {
	blockType := kiroString(block, "type")
	if blockType == "" {
		blockType = "unknown"
	}
	if blockType == "document" {
		if title := kiroString(block, "title"); title != "" {
			return fmt.Sprintf("[document %q omitted: documents are not supported by this gateway]", title)
		}
		return "[document omitted: documents are not supported by this gateway]"
	}
	return "[" + blockType + " content omitted: not supported by this gateway]"
}

func kiroImageFormat(mediaType string) string {
	switch strings.ToLower(strings.TrimSpace(mediaType)) {
	case "image/jpeg", "image/jpg":
		return "jpeg"
	case "image/png":
		return "png"
	case "image/gif":
		return "gif"
	case "image/webp":
		return "webp"
	}
	return ""
}

// attachImage 返回 (编号, 占位文本)。编号 >0 表示已附图；否则用占位文本替代。
func (c *kiroConverter) attachImage(block map[string]any, origin string, images *[]kiroImage) (int, string) {
	source, _ := block["source"].(map[string]any)
	if source == nil {
		return 0, "[image omitted: the image block has no source]"
	}
	data := kiroString(source, "data")
	if data == "" {
		kind := kiroString(source, "type")
		if kind == "" {
			kind = "unknown"
		}
		return 0, "[image omitted: " + kind + " image source is not supported by this gateway, only base64 images are]"
	}
	format := kiroImageFormat(kiroString(source, "media_type"))
	if format == "" {
		return 0, "[image omitted: unsupported image format " + kiroString(source, "media_type") + "]"
	}
	sum := sha256.Sum256([]byte(data))
	digest := hex.EncodeToString(sum[:])
	if firstSeen, dup := c.dedup[digest]; dup {
		return 0, "[image omitted: same image as in " + firstSeen + " in this conversation]"
	}
	c.dedup[digest] = origin
	format, data = kiroShrinkImage(format, data)
	*images = append(*images, kiroImage{Format: format, Source: kiroImageSource{Bytes: data}})
	return len(*images), ""
}

func (c *kiroConverter) processUserContent(raw json.RawMessage, images *[]kiroImage) (string, []kiroToolResult) {
	text, isString, blocks := kiroContent(raw)
	if isString {
		return text, nil
	}
	var parts []string
	var results []kiroToolResult
	for _, block := range blocks {
		switch kiroString(block, "type") {
		case "text":
			if value, ok := block["text"].(string); ok {
				parts = append(parts, value)
			}
		case "image":
			if n, placeholder := c.attachImage(block, "an earlier user message", images); n == 0 {
				parts = append(parts, placeholder)
			}
		case "tool_result":
			id := kiroString(block, "tool_use_id")
			if id == "" {
				continue
			}
			isError, _ := block["is_error"].(bool)
			results = append(results, newKiroToolResult(id, c.toolResultContent(block["content"], id, images), isError))
		case "tool_use", "thinking", "redacted_thinking":
		default:
			parts = append(parts, kiroUnsupportedBlockPlaceholder(block))
		}
	}
	return strings.Join(parts, "\n"), results
}

func (c *kiroConverter) toolResultContent(content any, toolUseID string, images *[]kiroImage) string {
	switch value := content.(type) {
	case nil:
		return ""
	case string:
		return value
	case []any:
		origin := "tool result " + toolUseID
		parts := make([]string, 0, len(value))
		for _, item := range value {
			block, _ := item.(map[string]any)
			switch kiroString(block, "type") {
			case "image":
				if n, placeholder := c.attachImage(block, origin, images); n > 0 {
					parts = append(parts, fmt.Sprintf("[image #%d attached to this message]", n))
				} else {
					parts = append(parts, placeholder)
				}
			case "text", "":
				if text, ok := block["text"].(string); ok {
					parts = append(parts, text)
				}
			default:
				parts = append(parts, kiroUnsupportedBlockPlaceholder(block))
			}
		}
		return strings.Join(parts, "\n")
	default:
		encoded, _ := json.Marshal(value)
		return string(encoded)
	}
}

// ---- 历史 ----

func (c *kiroConverter) thinkingPrefix() string {
	if kiroBuildAdditionalFields(c.req, c.modelID) != nil || c.req.Thinking == nil {
		return ""
	}
	switch {
	case c.req.Thinking.Type == "adaptive" || c.req.Thinking.Type == "enabled" && kiroModelSupportsNativeReasoning(c.modelID):
		return "<thinking_mode>adaptive</thinking_mode><thinking_effort>" + kiroSelectEffort(c.req, c.modelID) + "</thinking_effort>"
	case c.req.Thinking.Type == "enabled":
		return fmt.Sprintf("<thinking_mode>enabled</thinking_mode><max_thinking_length>%d</max_thinking_length>", c.req.Thinking.BudgetTokens)
	}
	return ""
}

func (c *kiroConverter) buildHistory(messages []kiroInboundMessage) ([]kiroHistoryEntry, error) {
	var history []kiroHistoryEntry
	prefix := c.thinkingPrefix()
	// Kiro 请求没有 system 字段：系统提示作为第一对 user + assistant 发出。
	systemContent := strings.Join(c.req.systemTexts(), "\n")
	if strings.TrimSpace(systemContent) != "" {
		var parts []string
		if prefix != "" && !strings.Contains(systemContent, "<thinking_mode>") && !strings.Contains(systemContent, "<max_thinking_length>") {
			parts = append(parts, prefix)
		}
		parts = append(parts, kiroClientSystemPromptOpen+"\n"+systemContent+"\n"+kiroClientSystemPromptClose)
		history = append(history, kiroHistoryEntry{User: &kiroUserMessage{Content: strings.Join(parts, "\n"), ModelID: c.modelID, Origin: "AI_EDITOR"}},
			kiroHistoryEntry{Assistant: &kiroAssistantMessage{Content: kiroClientSystemPromptAck}})
	} else if prefix != "" {
		history = append(history, kiroHistoryEntry{User: &kiroUserMessage{Content: prefix, ModelID: c.modelID, Origin: "AI_EDITOR"}},
			kiroHistoryEntry{Assistant: &kiroAssistantMessage{Content: kiroThinkingConfigAck}})
	}

	finishedTurns := 0
	if !kiroModelKeepsPriorThinking(c.modelID) {
		finishedTurns = kiroFinishedAssistantTurns(c.req.Messages)
	}
	assistantOrdinal := 0
	bufferKeepsThinking := true
	var userBuffer, assistantBuffer []kiroInboundMessage
	flushAssistant := func() {
		if len(assistantBuffer) > 0 {
			merged := c.mergeAssistantMessages(assistantBuffer, bufferKeepsThinking)
			history = append(history, kiroHistoryEntry{Assistant: &merged})
			assistantBuffer = nil
		}
	}
	for _, message := range messages[:len(messages)-1] {
		switch message.Role {
		case "user":
			flushAssistant()
			userBuffer = append(userBuffer, message)
		case "assistant":
			if len(userBuffer) > 0 {
				merged := c.mergeUserMessages(userBuffer)
				history = append(history, kiroHistoryEntry{User: &merged})
				userBuffer = nil
			}
			// 连续多条 assistant 之间没有 user，同属一个回合，按第一条判定。
			if len(assistantBuffer) == 0 {
				bufferKeepsThinking = assistantOrdinal >= finishedTurns
			}
			assistantOrdinal++
			assistantBuffer = append(assistantBuffer, message)
		}
	}
	flushAssistant()
	if len(userBuffer) > 0 {
		if c.purpose == kiroPurposeGenerate {
			return nil, fmt.Errorf("消息序列无效: history ends with a user message that has no assistant reply")
		}
		return nil, fmt.Errorf("消息序列无效: compaction would move an unanswered user message into history")
	}
	return history, nil
}

// kiroFinishedAssistantTurns 数出最后一个「开启新回合」的 user 消息之前的 assistant 条数。
func kiroFinishedAssistantTurns(messages []kiroInboundMessage) int {
	startsTurn := func(message kiroInboundMessage) bool {
		if message.Role != "user" {
			return false
		}
		text, isString, blocks := kiroContent(message.Content)
		if isString {
			return strings.TrimSpace(text) != ""
		}
		for _, block := range blocks {
			switch kiroString(block, "type") {
			case "tool_result":
			case "text":
				if strings.TrimSpace(kiroString(block, "text")) != "" {
					return true
				}
			default:
				return true
			}
		}
		return false
	}
	boundary := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if startsTurn(messages[i]) {
			boundary = i
			break
		}
	}
	count := 0
	for i := 0; i < boundary; i++ {
		if messages[i].Role == "assistant" {
			count++
		}
	}
	return count
}

func (c *kiroConverter) mergeUserMessages(messages []kiroInboundMessage) kiroUserMessage {
	var parts []string
	var images []kiroImage
	var results []kiroToolResult
	for _, message := range messages {
		text, toolResults := c.processUserContent(message.Content, &images)
		if text != "" {
			parts = append(parts, text)
		}
		results = append(results, toolResults...)
	}
	user := kiroUserMessage{Content: strings.Join(parts, "\n"), ModelID: c.modelID, Origin: "AI_EDITOR", Images: images}
	if len(results) > 0 {
		context := newKiroUserContext()
		context.ToolResults = results
		user.UserInputMessageContext = context
	}
	return user
}

func (c *kiroConverter) historyReasoning(messages []kiroInboundMessage, keepThinking bool) *kiroReasoningContent {
	if !keepThinking || c.purpose != kiroPurposeGenerate {
		return nil
	}
	var found map[string]any
	for _, message := range messages {
		_, _, blocks := kiroContent(message.Content)
		for _, block := range blocks {
			switch kiroString(block, "type") {
			case "thinking", "redacted_thinking":
				if found != nil {
					return nil // 多段 thinking 无法落成 Kiro 的单个 reasoningContent。
				}
				found = block
			}
		}
	}
	if found == nil {
		return nil
	}
	if kiroString(found, "type") == "thinking" {
		text, signature := kiroString(found, "thinking"), strings.TrimSpace(kiroString(found, "signature"))
		if text == "" || signature == "" || signature == kiroThinkingSignaturePlaceholder {
			return nil
		}
		return &kiroReasoningContent{ReasoningText: &kiroReasoningText{Text: text, Signature: kiroString(found, "signature")}}
	}
	data := kiroString(found, "data")
	if decoded, err := base64.StdEncoding.DecodeString(data); err != nil || len(decoded) == 0 {
		return nil
	}
	return &kiroReasoningContent{RedactedContent: data}
}

func (c *kiroConverter) convertAssistantMessage(message kiroInboundMessage) kiroAssistantMessage {
	var segments []string
	var run strings.Builder
	droppedThinking := false
	var toolUses []kiroToolUse
	text, isString, blocks := kiroContent(message.Content)
	if isString {
		run.WriteString(text)
	}
	for _, block := range blocks {
		switch kiroString(block, "type") {
		case "thinking", "redacted_thinking":
			if run.Len() > 0 {
				segments = append(segments, run.String())
				run.Reset()
			}
			droppedThinking = true
		case "text":
			run.WriteString(kiroString(block, "text"))
		case "tool_use":
			id, name := kiroString(block, "id"), kiroString(block, "name")
			if id == "" || name == "" {
				continue
			}
			input := block["input"]
			if input == nil {
				input = map[string]any{}
			}
			if c.purpose == kiroPurposeCompactFallback {
				name = kiroCompactionHistoryTool
			} else {
				name = c.mapToolName(name)
			}
			toolUses = append(toolUses, kiroToolUse{ToolUseID: id, Name: name, Input: input})
		}
	}
	if run.Len() > 0 {
		segments = append(segments, run.String())
	}
	content := strings.Join(segments, "\n\n")
	// Kiro 不接受空 content：只有工具调用（或 thinking 被丢掉）时放一个空格。
	if len(segments) == 0 && (len(toolUses) > 0 || droppedThinking) {
		content = " "
	}
	return kiroAssistantMessage{Content: content, ToolUses: toolUses}
}

func (c *kiroConverter) mergeAssistantMessages(messages []kiroInboundMessage, keepThinking bool) kiroAssistantMessage {
	if len(messages) == 1 {
		merged := c.convertAssistantMessage(messages[0])
		merged.ReasoningContent = c.historyReasoning(messages, keepThinking)
		return merged
	}
	var parts []string
	var toolUses []kiroToolUse
	for _, message := range messages {
		converted := c.convertAssistantMessage(message)
		if strings.TrimSpace(converted.Content) != "" {
			parts = append(parts, converted.Content)
		}
		toolUses = append(toolUses, converted.ToolUses...)
	}
	content := strings.Join(parts, "\n\n")
	if len(parts) == 0 {
		content = " "
	}
	return kiroAssistantMessage{Content: content, ToolUses: toolUses, ReasoningContent: c.historyReasoning(messages, keepThinking)}
}

// ---- 工具配对 ----

func kiroHistoryToolIDs(history []kiroHistoryEntry) (uses []string, results map[string]struct{}) {
	results = map[string]struct{}{}
	for _, entry := range history {
		if entry.Assistant != nil {
			for _, use := range entry.Assistant.ToolUses {
				uses = append(uses, use.ToolUseID)
			}
		}
		if entry.User != nil && entry.User.UserInputMessageContext != nil {
			for _, result := range entry.User.UserInputMessageContext.ToolResults {
				results[result.ToolUseID] = struct{}{}
			}
		}
	}
	return uses, results
}

// kiroValidateToolPairing 只保留能配上历史里未回答 tool_use 的结果，并返回仍缺结果的 id。
func kiroValidateToolPairing(history []kiroHistoryEntry, results []kiroToolResult) ([]kiroToolResult, map[string]struct{}) {
	uses, answered := kiroHistoryToolIDs(history)
	unpaired := map[string]struct{}{}
	for _, id := range uses {
		if _, ok := answered[id]; !ok {
			unpaired[id] = struct{}{}
		}
	}
	var filtered []kiroToolResult
	for _, result := range results {
		if _, ok := unpaired[result.ToolUseID]; ok {
			filtered = append(filtered, result)
			delete(unpaired, result.ToolUseID)
		}
	}
	return filtered, unpaired
}

// kiroPairOrphanedToolUses 给没拿到结果的工具调用补「被中断」结果：历史中段的补到紧随的
// user 消息，最后一条 assistant 的作为当前消息的结果返回。
func kiroPairOrphanedToolUses(history []kiroHistoryEntry, orphaned map[string]struct{}) []kiroToolResult {
	if len(orphaned) == 0 {
		return nil
	}
	handled := map[string]struct{}{}
	var pending []kiroToolResult
	for _, entry := range history {
		if entry.Assistant != nil {
			for _, use := range entry.Assistant.ToolUses {
				if _, ok := orphaned[use.ToolUseID]; !ok {
					continue
				}
				if _, done := handled[use.ToolUseID]; done {
					continue
				}
				handled[use.ToolUseID] = struct{}{}
				pending = append(pending, newKiroToolResult(use.ToolUseID, kiroInterruptedToolResult, true))
			}
		}
		if entry.User != nil && len(pending) > 0 {
			if entry.User.UserInputMessageContext == nil {
				entry.User.UserInputMessageContext = newKiroUserContext()
			}
			entry.User.UserInputMessageContext.ToolResults = append(entry.User.UserInputMessageContext.ToolResults, pending...)
			pending = nil
		}
	}
	return pending
}

func kiroValidateCompactionToolSequence(history []kiroHistoryEntry, current []kiroToolResult) error {
	pending, seen := map[string]struct{}{}, map[string]struct{}{}
	for _, entry := range history {
		if entry.Assistant != nil {
			for _, use := range entry.Assistant.ToolUses {
				if _, dup := seen[use.ToolUseID]; dup {
					return fmt.Errorf("消息序列无效: duplicate tool_use id %s", use.ToolUseID)
				}
				seen[use.ToolUseID] = struct{}{}
				pending[use.ToolUseID] = struct{}{}
			}
		}
		if entry.User != nil && entry.User.UserInputMessageContext != nil {
			for _, result := range entry.User.UserInputMessageContext.ToolResults {
				if _, ok := pending[result.ToolUseID]; !ok {
					return fmt.Errorf("消息序列无效: tool_result %s has no preceding tool_use", result.ToolUseID)
				}
				delete(pending, result.ToolUseID)
			}
		}
	}
	for _, result := range current {
		if _, ok := pending[result.ToolUseID]; !ok {
			return fmt.Errorf("消息序列无效: tool_result %s has no preceding tool_use", result.ToolUseID)
		}
		delete(pending, result.ToolUseID)
	}
	if len(pending) > 0 {
		ids := make([]string, 0, len(pending))
		for id := range pending {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		return fmt.Errorf("消息序列无效: tool_use without result: %s", strings.Join(ids, ", "))
	}
	return nil
}

// kiroResentToolResultNotes：已在历史里回答过、这次内容又不同的结果并进正文，不悄悄丢。
func kiroResentToolResultNotes(history []kiroHistoryEntry, results []kiroToolResult) []string {
	if len(results) == 0 {
		return nil
	}
	earlier := map[string]string{}
	for _, entry := range history {
		if entry.User == nil || entry.User.UserInputMessageContext == nil {
			continue
		}
		for _, result := range entry.User.UserInputMessageContext.ToolResults {
			if _, ok := earlier[result.ToolUseID]; !ok {
				earlier[result.ToolUseID] = result.text()
			}
		}
	}
	var notes []string
	for _, result := range results {
		previous, ok := earlier[result.ToolUseID]
		if !ok || previous == result.text() {
			continue
		}
		notes = append(notes, "[tool result for "+result.ToolUseID+" sent again with different content]\n"+result.text())
	}
	return notes
}

func kiroCollectHistoryToolNames(history []kiroHistoryEntry) []string {
	var names []string
	seen := map[string]struct{}{}
	for _, entry := range history {
		if entry.Assistant == nil {
			continue
		}
		for _, use := range entry.Assistant.ToolUses {
			if _, ok := seen[use.Name]; !ok {
				seen[use.Name] = struct{}{}
				names = append(names, use.Name)
			}
		}
	}
	return names
}

// ---- reasoning effort ----

func kiroNativeReasoningRequested(req *kiroInboundRequest, modelID string) bool {
	if modelID == "claude-opus-4.6" {
		return req.Thinking != nil && req.Thinking.Type == "adaptive"
	}
	return req.Thinking.enabled() || req.OutputConfig != nil && strings.TrimSpace(req.OutputConfig.Effort) != ""
}

func kiroSelectEffort(req *kiroInboundRequest, modelID string) string {
	raw := ""
	if req.OutputConfig != nil {
		raw = strings.TrimSpace(req.OutputConfig.Effort)
	}
	if raw == "" && req.Thinking.enabled() {
		raw = kiroEffortFromBudgetTokens(req.Thinking.BudgetTokens)
	}
	if raw == "" {
		raw = "high"
	}
	if effort := kiroNormalizeEffort(modelID, raw); effort != "" {
		return effort
	}
	return "high"
}

// kiroBuildAdditionalFields 只给明确接受该字段的模型下发 effort；其余模型会以
// "additionalModelRequestFields is not supported for this model" 拒绝。
func kiroBuildAdditionalFields(req *kiroInboundRequest, modelID string) *kiroAdditionalFields {
	if req.Thinking != nil && req.Thinking.Type == "disabled" {
		return nil
	}
	if !kiroModelSupportsNativeReasoning(modelID) || !kiroNativeReasoningRequested(req, modelID) {
		return nil
	}
	effort := &kiroEffort{Effort: kiroSelectEffort(req, modelID)}
	if kiroModelUsesReasoningWireField(modelID) {
		return &kiroAdditionalFields{Reasoning: effort}
	}
	return &kiroAdditionalFields{OutputConfig: effort}
}
