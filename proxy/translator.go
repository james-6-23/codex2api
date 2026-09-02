package proxy

import (
	"container/list"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ==================== 输入结构体（OpenAI Chat Completions 格式） ====================

// openAIRequest 表示 OpenAI Chat Completions 请求（仅解析翻译所需字段）
type openAIRequest struct {
	Model               string            `json:"model"`
	Messages            []openAIMessage   `json:"messages"`
	Tools               []json.RawMessage `json:"tools"`
	ResponseFormat      json.RawMessage   `json:"response_format,omitempty"`
	ReasoningEffort     string            `json:"reasoning_effort"`
	ServiceTier         string            `json:"service_tier"`
	ServiceTierAlt      string            `json:"serviceTier"` // 兼容驼峰命名
	Temperature         json.RawMessage   `json:"temperature,omitempty"`
	TopP                json.RawMessage   `json:"top_p,omitempty"`
	MaxTokens           json.RawMessage   `json:"max_tokens,omitempty"`
	MaxCompletionTokens json.RawMessage   `json:"max_completion_tokens,omitempty"`
	Stop                json.RawMessage   `json:"stop,omitempty"`
	Seed                json.RawMessage   `json:"seed,omitempty"`
	PresencePenalty     json.RawMessage   `json:"presence_penalty,omitempty"`
	FrequencyPenalty    json.RawMessage   `json:"frequency_penalty,omitempty"`
	ParallelToolCalls   json.RawMessage   `json:"parallel_tool_calls,omitempty"`
	ToolChoice          json.RawMessage   `json:"tool_choice,omitempty"`
}

// openAIMessage 表示一条 OpenAI 消息
type openAIMessage struct {
	Role       string           `json:"role"`
	Content    json.RawMessage  `json:"content"` // string 或 []contentPart
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
}

// openAIToolCall 表示 assistant 消息中的工具调用
type openAIToolCall struct {
	Type     string `json:"type"`
	ID       string `json:"id"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// openAIToolParsed 表示解析后的工具定义
type openAIToolParsed struct {
	Type     string          `json:"type"`
	Function *openAIToolFunc `json:"function,omitempty"`
}

// openAIToolFunc 表示工具的函数描述
type openAIToolFunc struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Strict      *bool           `json:"strict,omitempty"`
}

// openAIContentPart 表示多部分内容中的一项
type openAIContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url,omitempty"`
	File *struct {
		Filename string `json:"filename,omitempty"`
		FileData string `json:"file_data,omitempty"`
		FileID   string `json:"file_id,omitempty"`
	} `json:"file,omitempty"`
}

// ==================== 输出结构体（OpenAI 流式/非流式响应格式） ====================

// openAIStreamChunk 流式响应块
type openAIStreamChunk struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []streamChoice `json:"choices"`
	Usage   *UsageInfo     `json:"usage,omitempty"`
}

// streamChoice 流式块中的选项
type streamChoice struct {
	Index        int          `json:"index"`
	Delta        *streamDelta `json:"delta,omitempty"`
	FinishReason *string      `json:"finish_reason"`
}

// streamDelta 流式块中的增量内容。
//
// reasoning 字段同时输出两种命名,兼容不同客户端:
//   - reasoning:  OpenAI 官方 o1/GPT-5 风格(Cherry Studio 等默认走这个)
//   - reasoning_content: DeepSeek / OpenRouter / new-api 等克隆站点风格
type streamDelta struct {
	Role             string          `json:"role,omitempty"`
	Content          *string         `json:"content,omitempty"`
	Reasoning        *string         `json:"reasoning,omitempty"`
	ReasoningContent *string         `json:"reasoning_content,omitempty"`
	ToolCalls        []toolCallDelta `json:"tool_calls,omitempty"`
}

// toolCallDelta 工具调用增量
type toolCallDelta struct {
	Index    int               `json:"index"`
	ID       string            `json:"id,omitempty"`
	Type     string            `json:"type,omitempty"`
	Function toolCallFuncDelta `json:"function"`
}

// toolCallFuncDelta 工具函数增量
type toolCallFuncDelta struct {
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments"`
}

// openAICompactResponse 非流式完整响应
type openAICompactResponse struct {
	ID      string          `json:"id"`
	Object  string          `json:"object"`
	Created int64           `json:"created,omitempty"`
	Model   string          `json:"model"`
	Choices []compactChoice `json:"choices"`
	Usage   *UsageInfo      `json:"usage,omitempty"`
}

// compactChoice 非流式响应中的选项
type compactChoice struct {
	Index        int            `json:"index"`
	Message      compactMessage `json:"message"`
	FinishReason string         `json:"finish_reason"`
}

// compactMessage 非流式响应中的消息。reasoning / reasoning_content 同时输出兼容多端。
type compactMessage struct {
	Role             string               `json:"role"`
	Content          *string              `json:"content"`
	Reasoning        *string              `json:"reasoning,omitempty"`
	ReasoningContent *string              `json:"reasoning_content,omitempty"`
	ToolCalls        []compactToolCallOut `json:"tool_calls,omitempty"`
}

// compactToolCallOut 非流式响应中的工具调用
type compactToolCallOut struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// openAIErrorResponse 错误响应
type openAIErrorResponse struct {
	Error struct {
		Message string          `json:"message"`
		Type    string          `json:"type"`
		Details json.RawMessage `json:"details,omitempty"`
	} `json:"error"`
}

// ==================== LRU 请求解析缓存 ====================

const requestCacheSize = 256

// maxTools 上游 Codex API 允许的最大工具数量
const maxTools = 128

const (
	codexImageGenerationBridgeMarker = "<codex2api-codex-image-generation>"
	codexImageGenerationBridgeText   = codexImageGenerationBridgeMarker + "\nWhen the user asks for raster image generation or editing, use the OpenAI Responses native `image_generation` tool attached to this request. The local Codex client may not expose an `image_gen` namespace, but that does not mean image generation is unavailable. Do not ask the user to switch to CLI fallback solely because `image_gen` is absent.\n</codex2api-codex-image-generation>"
	jsonObjectFormatInputHint        = "Return a valid JSON object."
)

var responsesImageGenerationOptionFields = []string{
	"size",
	"quality",
	"background",
	"output_format",
	"output_compression",
	"moderation",
	"partial_images",
}

var responsesImageGenerationUnsupportedOptionFields = []string{
	"style",
}

type requestCacheEntry struct {
	key [32]byte
	req openAIRequest
}

type requestCache struct {
	mu    sync.Mutex
	order *list.List
	items map[[32]byte]*list.Element
}

var globalRequestCache = &requestCache{
	order: list.New(),
	items: make(map[[32]byte]*list.Element, requestCacheSize),
}

func firstNonEmptyAnyString(raw any) string {
	switch v := raw.(type) {
	case string:
		return strings.TrimSpace(v)
	case []byte:
		return strings.TrimSpace(string(v))
	default:
		return ""
	}
}

func appendResponseTextPart(parts *[]string, raw any) {
	text := firstNonEmptyAnyString(raw)
	if text != "" {
		*parts = append(*parts, text)
	}
}

func extractResponsesPromptText(body map[string]any) string {
	if len(body) == 0 {
		return ""
	}
	if prompt := firstNonEmptyAnyString(body["prompt"]); prompt != "" {
		return prompt
	}
	var parts []string
	extractResponsesInputText(body["input"], &parts)
	return strings.Join(parts, " ")
}

func extractResponsesInputText(raw any, parts *[]string) {
	switch v := raw.(type) {
	case string:
		appendResponseTextPart(parts, v)
	case []map[string]string:
		for _, item := range v {
			appendResponseTextPart(parts, item["content"])
		}
	case []map[string]any:
		for _, item := range v {
			extractResponsesMessageText(item, parts)
		}
	case []any:
		for _, item := range v {
			switch typed := item.(type) {
			case map[string]any:
				extractResponsesMessageText(typed, parts)
			case map[string]string:
				appendResponseTextPart(parts, typed["content"])
			case string:
				appendResponseTextPart(parts, typed)
			}
		}
	}
}

func extractResponsesMessageText(item map[string]any, parts *[]string) {
	if item == nil {
		return
	}
	appendResponseTextPart(parts, item["text"])
	content, ok := item["content"]
	if !ok {
		return
	}
	switch v := content.(type) {
	case string:
		appendResponseTextPart(parts, v)
	case []any:
		for _, rawPart := range v {
			part, ok := rawPart.(map[string]any)
			if !ok {
				continue
			}
			appendResponseTextPart(parts, part["text"])
		}
	case []map[string]any:
		for _, part := range v {
			appendResponseTextPart(parts, part["text"])
		}
	}
}

func hasResponsesImageGenerationTool(body map[string]any) bool {
	rawTools, ok := body["tools"]
	if !ok || rawTools == nil {
		return false
	}
	tools, ok := rawTools.([]any)
	if !ok {
		return false
	}
	for _, rawTool := range tools {
		toolMap, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		if strings.TrimSpace(firstNonEmptyAnyString(toolMap["type"])) == "image_generation" {
			return true
		}
	}
	return false
}

// isImageGenNamespaceTool 识别 namespace 形式的生图工具声明：
// { "type": "namespace", "name": "image_gen", ... }。Codex 的 /image 技能用
// 这种形式声明生图能力，而非扁平的 { "type": "image_generation" }。
func isImageGenNamespaceTool(tool map[string]any) bool {
	return strings.TrimSpace(firstNonEmptyAnyString(tool["type"])) == "namespace" &&
		strings.TrimSpace(firstNonEmptyAnyString(tool["name"])) == "image_gen"
}

// hasResponsesImageGenNamespaceTool 检测客户端是否自带 namespace 生图声明，
// 覆盖两个位置：顶层 tools[]，以及 input[] 里 type=additional_tools 项内嵌的
// 工具列表（Responses Lite 格式）。带这种声明的客户端已有自己的生图链路，
// 不应再叠加注入 hosted image_generation 工具和桥接 instructions——桥接文案
// 假设 image_gen namespace 缺席，在其在场时注入语义恰好相反。
func hasResponsesImageGenNamespaceTool(body map[string]any) bool {
	if tools, ok := body["tools"].([]any); ok {
		for _, rawTool := range tools {
			if toolMap, ok := rawTool.(map[string]any); ok && isImageGenNamespaceTool(toolMap) {
				return true
			}
		}
	}
	input, ok := body["input"].([]any)
	if !ok {
		return false
	}
	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		if strings.TrimSpace(firstNonEmptyAnyString(item["type"])) != "additional_tools" {
			continue
		}
		tools, ok := item["tools"].([]any)
		if !ok {
			continue
		}
		for _, rawTool := range tools {
			if toolMap, ok := rawTool.(map[string]any); ok && isImageGenNamespaceTool(toolMap) {
				return true
			}
		}
	}
	return false
}

func responsesImageGenerationToolChoice(body map[string]any) string {
	if len(body) == 0 {
		return ""
	}
	switch choice := body["tool_choice"].(type) {
	case string:
		return strings.TrimSpace(choice)
	case map[string]any:
		return strings.TrimSpace(firstNonEmptyAnyString(choice["type"]))
	default:
		return ""
	}
}

func hasResponsesImageGenerationToolChoice(body map[string]any) bool {
	return strings.EqualFold(responsesImageGenerationToolChoice(body), "image_generation")
}

func ensureResponsesImageGenerationTool(body map[string]any) bool {
	if len(body) == 0 {
		return false
	}
	defaultTool := map[string]any{
		"type":  "image_generation",
		"model": defaultImagesToolModel,
	}
	rawTools, ok := body["tools"]
	if !ok || rawTools == nil {
		body["tools"] = []any{defaultTool}
		return true
	}
	tools, ok := rawTools.([]any)
	if !ok {
		body["tools"] = []any{defaultTool}
		return true
	}
	for _, rawTool := range tools {
		toolMap, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		if strings.TrimSpace(firstNonEmptyAnyString(toolMap["type"])) == "image_generation" {
			return false
		}
	}
	if len(tools) >= maxTools {
		truncated := append([]any(nil), tools[:maxTools]...)
		truncated[maxTools-1] = defaultTool
		body["tools"] = truncated
		return true
	}
	body["tools"] = append(tools, defaultTool)
	return true
}

func firstResponsesImageGenerationTool(body map[string]any) map[string]any {
	rawTools, ok := body["tools"]
	if !ok || rawTools == nil {
		return nil
	}
	tools, ok := rawTools.([]any)
	if !ok {
		return nil
	}
	for _, rawTool := range tools {
		toolMap, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		if strings.TrimSpace(firstNonEmptyAnyString(toolMap["type"])) == "image_generation" {
			return toolMap
		}
	}
	return nil
}

func moveTopLevelResponsesImageOptions(body map[string]any) bool {
	toolMap := firstResponsesImageGenerationTool(body)
	if len(body) == 0 || toolMap == nil {
		return false
	}
	modified := false
	for _, key := range responsesImageGenerationOptionFields {
		value, exists := body[key]
		if !exists || value == nil {
			continue
		}
		_, toolHas := toolMap[key]
		if key == "output_format" && strings.TrimSpace(firstNonEmptyAnyString(toolMap["format"])) != "" {
			toolHas = true
		}
		if key == "output_compression" {
			if _, hasAlias := toolMap["compression"]; hasAlias {
				toolHas = true
			}
		}
		if !toolHas {
			toolMap[key] = value
		}
		delete(body, key)
		modified = true
	}
	for _, key := range responsesImageGenerationUnsupportedOptionFields {
		if _, exists := body[key]; exists {
			delete(body, key)
			modified = true
		}
	}
	return modified
}

// codexWebSearchAllowedFields 是 Codex 上游接受的 web_search 配置字段白名单。
// 实测来源：直连 chatgpt.com/backend-api/codex/responses 用 gpt-5.4-mini 探测，
// 这三个字段会被原样回显并生效；任何不在该集合的字段会触发
// 400 unknown_parameter。
var codexWebSearchAllowedFields = map[string]struct{}{
	"search_context_size": {},
	"user_location":       {},
	"filters":             {},
}

// normalizeResponsesWebSearchTools 把所有 OpenAI Responses 协议下的 web_search
// 变体（web_search_preview / web_search_preview_2025_03_11 /
// web_search_2025_08_26 等）归一为 Codex 上游唯一接受的 {"type":"web_search"}。
//
// Codex 后端只识别裸 "web_search"，对其他变体一律返回
// 400 {"detail":"Unsupported tool type: ..."}。OpenAI 原生 Responses
// 端点支持这些变体——所以本函数只能在 Codex 上游路径调用。
//
// 归一时保留 Codex 已知接受的配置字段（search_context_size / user_location /
// filters），其它未知字段一律丢弃，避免触发上游的 unknown_parameter 校验。
func normalizeResponsesWebSearchTools(body map[string]any) bool {
	rawTools, ok := body["tools"]
	if !ok || rawTools == nil {
		return false
	}
	tools, ok := rawTools.([]any)
	if !ok {
		return false
	}
	modified := false
	for i, rawTool := range tools {
		toolMap, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		toolType := strings.TrimSpace(firstNonEmptyAnyString(toolMap["type"]))
		if toolType == "" || !strings.HasPrefix(toolType, "web_search") {
			continue
		}
		normalized := normalizeCodexWebSearchTool(toolMap)
		if mapsEqual(toolMap, normalized) {
			continue
		}
		tools[i] = normalized
		modified = true
	}
	if modified {
		body["tools"] = tools
	}
	return modified
}

// normalizeCodexWebSearchTool 返回一个仅包含 {type, <白名单字段>} 的新 map。
// 调用前请确保 toolMap.type 以 "web_search" 开头。
func normalizeCodexWebSearchTool(toolMap map[string]any) map[string]any {
	out := map[string]any{"type": "web_search"}
	for k, v := range toolMap {
		if _, ok := codexWebSearchAllowedFields[k]; ok {
			out[k] = v
		}
	}
	return out
}

func mapsEqual(a, b map[string]any) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		vb, ok := b[k]
		if !ok {
			return false
		}
		if !reflect.DeepEqual(va, vb) {
			return false
		}
	}
	return true
}

func normalizeResponsesImageGenerationTools(body map[string]any, promptText string) bool {
	rawTools, ok := body["tools"]
	if !ok || rawTools == nil {
		return false
	}
	tools, ok := rawTools.([]any)
	if !ok {
		return false
	}
	modified := false
	for _, rawTool := range tools {
		toolMap, ok := rawTool.(map[string]any)
		if !ok || strings.TrimSpace(firstNonEmptyAnyString(toolMap["type"])) != "image_generation" {
			continue
		}
		rawModel := strings.TrimSpace(firstNonEmptyAnyString(toolMap["model"]))
		toolModel, defaultSize := normalizeImageToolModelForPrompt(rawModel, promptText)
		if rawModel != toolModel {
			toolMap["model"] = toolModel
			modified = true
		}
		sizeValue, hasSize := toolMap["size"]
		sizeString, sizeIsString := sizeValue.(string)
		if defaultSize != "" && (!hasSize || (sizeIsString && strings.TrimSpace(sizeString) == "")) {
			toolMap["size"] = defaultSize
			modified = true
		}
		if _, ok := toolMap["output_format"]; !ok {
			if value := strings.TrimSpace(firstNonEmptyAnyString(toolMap["format"])); value != "" {
				toolMap["output_format"] = value
			} else {
				toolMap["output_format"] = "png"
			}
			modified = true
		}
		if _, ok := toolMap["output_compression"]; !ok {
			if value, exists := toolMap["compression"]; exists && value != nil {
				toolMap["output_compression"] = value
				modified = true
			}
		}
		if _, ok := toolMap["format"]; ok {
			delete(toolMap, "format")
			modified = true
		}
		if _, ok := toolMap["compression"]; ok {
			delete(toolMap, "compression")
			modified = true
		}
		for _, key := range responsesImageGenerationUnsupportedOptionFields {
			if _, ok := toolMap[key]; ok {
				delete(toolMap, key)
				modified = true
			}
		}
	}
	return modified
}

func normalizeResponsesPromptCompat(body map[string]any) bool {
	rawPrompt, hasPrompt := body["prompt"]
	if len(body) == 0 || !hasPrompt {
		return false
	}
	if _, hasInput := body["input"]; !hasInput {
		if prompt := strings.TrimSpace(firstNonEmptyAnyString(rawPrompt)); prompt != "" {
			body["input"] = prompt
		}
	}
	delete(body, "prompt")
	return true
}

func applyResponsesImageGenerationBridgeInstructions(body map[string]any) bool {
	if len(body) == 0 || !hasResponsesImageGenerationTool(body) {
		return false
	}
	existing, _ := body["instructions"].(string)
	if strings.Contains(existing, codexImageGenerationBridgeMarker) {
		return false
	}
	existing = strings.TrimRight(existing, " \t\r\n")
	if strings.TrimSpace(existing) == "" {
		body["instructions"] = codexImageGenerationBridgeText
		return true
	}
	body["instructions"] = existing + "\n\n" + codexImageGenerationBridgeText
	return true
}

// removeCodexImageGenerationBridgeText 移除 instructions 中的生图桥接标记块，
// 是 applyResponsesImageGenerationBridgeInstructions 的逆操作。剥除随注入工具
// 一同追加的桥接文案时使用；标记块不在场则原样返回。
func removeCodexImageGenerationBridgeText(instructions string) string {
	const bridgeEndTag = "</codex2api-codex-image-generation>"
	for {
		start := strings.Index(instructions, codexImageGenerationBridgeMarker)
		if start < 0 {
			return instructions
		}
		rest := instructions[start:]
		end := strings.Index(rest, bridgeEndTag)
		head := strings.TrimRight(instructions[:start], " \t\r\n")
		if end < 0 {
			return head
		}
		tail := strings.TrimLeft(rest[end+len(bridgeEndTag):], " \t\r\n")
		switch {
		case head == "":
			instructions = tail
		case tail == "":
			instructions = head
		default:
			instructions = head + "\n\n" + tail
		}
	}
}

func hasTopLevelResponsesImageOptions(body map[string]any) bool {
	if len(body) == 0 {
		return false
	}
	for _, key := range responsesImageGenerationOptionFields {
		if value, exists := body[key]; exists && value != nil {
			return true
		}
	}
	return false
}

func isStructuredResponsesFormatType(formatType string) bool {
	switch strings.ToLower(strings.TrimSpace(formatType)) {
	case "json_schema", "json_object":
		return true
	default:
		return false
	}
}

func hasStructuredResponsesFormat(body map[string]any) bool {
	if len(body) == 0 {
		return false
	}
	if text, ok := body["text"].(map[string]any); ok {
		if format, ok := text["format"].(map[string]any); ok {
			if isStructuredResponsesFormatType(firstNonEmptyAnyString(format["type"])) {
				return true
			}
		}
	}
	if responseFormat, ok := body["response_format"].(map[string]any); ok {
		return isStructuredResponsesFormatType(firstNonEmptyAnyString(responseFormat["type"]))
	}
	return false
}

// responsesModelRejectsHostedImageTool 判断模型是否不支持 hosted image_generation
// 工具。gpt-5.3-codex-spark 是 ChatGPT 账号下的纯文本 Codex 模型，上游会直接拒绝
// 带 hosted 图片工具的请求（issue #230）。
func responsesModelRejectsHostedImageTool(body map[string]any) bool {
	model := strings.TrimSpace(firstNonEmptyAnyString(body["model"]))
	return strings.EqualFold(model, proOnlySparkModel)
}

func shouldAutoInjectResponsesImageGenerationTool(body map[string]any) bool {
	if len(body) == 0 || hasResponsesImageGenerationTool(body) {
		return false
	}
	// 客户端自带 namespace 生图声明时不叠加注入(见 hasResponsesImageGenNamespaceTool)。
	if hasResponsesImageGenNamespaceTool(body) {
		return false
	}
	// 不为拒绝 hosted 图片工具的模型自动注入默认图片工具及桥接 instructions；
	// 用户显式自带的图片工具仍由上面 hasResponsesImageGenerationTool 分支保留。
	if responsesModelRejectsHostedImageTool(body) {
		return false
	}
	if hasResponsesImageGenerationToolChoice(body) {
		return true
	}
	if hasTopLevelResponsesImageOptions(body) {
		return true
	}
	return !hasStructuredResponsesFormat(body)
}

func shouldInjectOpenAIResponsesImageGenerationTool(body map[string]any) bool {
	if len(body) == 0 || hasResponsesImageGenerationTool(body) {
		return false
	}
	// 与 ChatGPT 路径一致:namespace 生图声明在场时不叠加注入。
	if hasResponsesImageGenNamespaceTool(body) {
		return false
	}
	if hasResponsesImageGenerationToolChoice(body) {
		return true
	}
	if hasTopLevelResponsesImageOptions(body) {
		return true
	}
	return isImageOnlyModel(strings.TrimSpace(firstNonEmptyAnyString(body["model"])))
}

func normalizeResponsesImageOnlyModel(body map[string]any) bool {
	if len(body) == 0 {
		return false
	}
	imageModel := strings.TrimSpace(firstNonEmptyAnyString(body["model"]))
	if !isImageOnlyModel(imageModel) {
		return false
	}

	modified := false
	tools, _ := body["tools"].([]any)
	imageToolIndex := -1
	for i, rawTool := range tools {
		toolMap, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		if strings.TrimSpace(firstNonEmptyAnyString(toolMap["type"])) == "image_generation" {
			imageToolIndex = i
			break
		}
	}
	if imageToolIndex < 0 {
		tools = append(tools, map[string]any{
			"type":  "image_generation",
			"model": imageModel,
		})
		imageToolIndex = len(tools) - 1
		body["tools"] = tools
		modified = true
	}

	if toolMap, ok := tools[imageToolIndex].(map[string]any); ok {
		if strings.TrimSpace(firstNonEmptyAnyString(toolMap["model"])) == "" {
			toolMap["model"] = imageModel
			modified = true
		}
	}

	if _, ok := body["tool_choice"]; !ok {
		body["tool_choice"] = map[string]any{"type": "image_generation"}
		modified = true
	}
	if imageModel != defaultImagesMainModel {
		modified = true
	}
	body["model"] = defaultImagesMainModel
	return modified
}

// normalizeResponsesCompactionItems converts {"type":"compaction","summary":"..."}
// items in body["input"] into developer-role messages so the upstream Codex
// /responses endpoint accepts them. Items with empty or missing summary text
// are dropped. Codex CLI compresses prior turns into compaction items expecting
// them to be forwarded as conversation context; the upstream rejects the type
// with "Invalid input type 'compaction' at index N", so we translate in place.
//
// Opaque Compact v2 items are left untouched so they remain source-affine.
// Known reversible emulated envelopes are decoded into the same developer
// summary representation as plaintext compaction items.
func normalizeResponsesCompactionItems(body map[string]any) bool {
	if len(body) == 0 {
		return false
	}
	inputItems, ok := body["input"].([]any)
	if !ok {
		return false
	}

	modified := false
	out := make([]any, 0, len(inputItems))
	for _, raw := range inputItems {
		itemMap, ok := raw.(map[string]any)
		if !ok {
			out = append(out, raw)
			continue
		}
		itemType := firstNonEmptyAnyString(itemMap["type"])
		if !isResponsesCompactionItemType(itemType) {
			out = append(out, raw)
			continue
		}

		if encryptedContent := firstNonEmptyAnyString(itemMap["encrypted_content"]); encryptedContent != "" {
			rawEncryptedContent, _ := itemMap["encrypted_content"].(string)
			if summaryText, portable := decodePortableCompactionSummary(rawEncryptedContent); portable {
				out = append(out, responsesCompactionDeveloperMessage(summaryText))
				modified = true
				continue
			}
			// Unknown encrypted state must be forwarded verbatim.
			out = append(out, raw)
			continue
		}
		if itemType != "compaction" {
			out = append(out, raw)
			continue
		}

		summaryText := compactionSummaryText(itemMap["summary"])
		if summaryText == "" {
			summaryText = compactionSummaryText(itemMap["text"])
		}
		if summaryText == "" {
			modified = true
			continue
		}

		out = append(out, responsesCompactionDeveloperMessage(summaryText))
		modified = true
	}

	if modified {
		body["input"] = out
	}
	return modified
}

// normalizeResponsesSystemRoleMessages 把 input[] 中 role=system 的消息项改写为
// developer 角色。上游 Codex /responses 不接受 system 角色（报 "System messages
// are not allowed"），而 chat 与 Anthropic 两条翻译链路均已把 system 落到
// developer（buildInput / buildCodexInput）；原生 Responses 直通路径在此补齐
// 同样的语义（issue #409）。内容保持原样，content part 类型交由后续
// normalizeResponsesContentPartTypes 按角色归一。
func normalizeResponsesSystemRoleMessages(body map[string]any) bool {
	if len(body) == 0 {
		return false
	}
	inputItems, ok := body["input"].([]any)
	if !ok {
		return false
	}

	modified := false
	for _, raw := range inputItems {
		itemMap, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if !isResponsesMessageInputItem(itemMap) {
			continue
		}
		if strings.TrimSpace(firstNonEmptyAnyString(itemMap["role"])) != "system" {
			continue
		}
		itemMap["role"] = "developer"
		modified = true
	}
	return modified
}

// normalizeResponsesToolCallArgumentTypes 修正 input[] 中工具调用项 arguments 的
// JSON 类型。上游对不同 item 类型的要求不对称：function_call.arguments 必须是
// string（JSON 编码），tool_search_call.arguments 必须是 object。客户端与缓存
// 回放通常把上一轮输出项原样回灌，类型不符会被上游 400 拒绝：
// "Invalid type for 'input[N].arguments': expected an object, but got a string
// instead."（issue #330）。
func normalizeResponsesToolCallArgumentTypes(body map[string]any) bool {
	inputItems, ok := body["input"].([]any)
	if !ok {
		return false
	}

	modified := false
	for _, raw := range inputItems {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		args, hasArgs := item["arguments"]
		if !hasArgs {
			continue
		}
		switch firstNonEmptyAnyString(item["type"]) {
		case "function_call":
			if _, isString := args.(string); isString {
				continue
			}
			if encoded, err := json.Marshal(args); err == nil {
				item["arguments"] = string(encoded)
				modified = true
			}
		case "tool_search_call":
			s, isString := args.(string)
			if !isString {
				continue
			}
			var obj map[string]any
			if strings.TrimSpace(s) == "" {
				obj = map[string]any{}
			} else if err := json.Unmarshal([]byte(s), &obj); err != nil || obj == nil {
				continue
			}
			item["arguments"] = obj
			modified = true
		}
	}
	return modified
}

// normalizeOrdinaryFunctionCallArguments validates the JSON string required by
// an ordinary Responses function_call. Empty arguments are a widely emitted
// shorthand for an empty object; all other malformed values are rejected so a
// truncated call cannot be persisted and replayed into later turns.
func normalizeOrdinaryFunctionCallArguments(arguments string) (string, bool) {
	if strings.TrimSpace(arguments) == "" {
		return "{}", true
	}
	if !json.Valid([]byte(arguments)) {
		return "", false
	}
	return arguments, true
}

// sanitizeMalformedResponsesFunctionCalls removes malformed ordinary
// function_call history and its matching function_call_output. Provider-native
// and custom tool items deliberately remain untouched: their arguments/input
// may be free-form text or another provider-specific shape rather than JSON.
func sanitizeMalformedResponsesFunctionCalls(body map[string]any) bool {
	inputItems, ok := body["input"].([]any)
	if !ok || len(inputItems) == 0 {
		return false
	}

	invalidCallIDs := make(map[string]struct{})
	validCallIDs := make(map[string]struct{})
	invalidEmptyCalls := 0
	modified := false
	withoutInvalidCalls := make([]any, 0, len(inputItems))

	for _, raw := range inputItems {
		item, ok := raw.(map[string]any)
		if !ok || strings.TrimSpace(firstNonEmptyAnyString(item["type"])) != "function_call" {
			withoutInvalidCalls = append(withoutInvalidCalls, raw)
			continue
		}

		arguments, isString := item["arguments"].(string)
		if !isString {
			// normalizeResponsesToolCallArgumentTypes runs immediately before this
			// sanitizer. If marshaling failed there, fail closed here.
			callID := strings.TrimSpace(firstNonEmptyAnyString(item["call_id"]))
			if callID == "" {
				invalidEmptyCalls++
			} else {
				invalidCallIDs[callID] = struct{}{}
			}
			modified = true
			continue
		}
		normalized, valid := normalizeOrdinaryFunctionCallArguments(arguments)
		callID := strings.TrimSpace(firstNonEmptyAnyString(item["call_id"]))
		if !valid {
			if callID == "" {
				invalidEmptyCalls++
			} else {
				invalidCallIDs[callID] = struct{}{}
			}
			modified = true
			continue
		}
		if normalized != arguments {
			item["arguments"] = normalized
			modified = true
		}
		if callID != "" {
			validCallIDs[callID] = struct{}{}
		}
		withoutInvalidCalls = append(withoutInvalidCalls, raw)
	}

	for callID := range validCallIDs {
		delete(invalidCallIDs, callID)
	}
	if len(invalidCallIDs) == 0 && invalidEmptyCalls == 0 {
		if modified {
			body["input"] = withoutInvalidCalls
		}
		return modified
	}

	filtered := make([]any, 0, len(withoutInvalidCalls))
	for _, raw := range withoutInvalidCalls {
		item, ok := raw.(map[string]any)
		if !ok || strings.TrimSpace(firstNonEmptyAnyString(item["type"])) != "function_call_output" {
			filtered = append(filtered, raw)
			continue
		}
		callID := strings.TrimSpace(firstNonEmptyAnyString(item["call_id"]))
		if callID == "" && invalidEmptyCalls > 0 {
			invalidEmptyCalls--
			modified = true
			continue
		}
		if _, invalid := invalidCallIDs[callID]; invalid {
			modified = true
			continue
		}
		filtered = append(filtered, raw)
	}
	body["input"] = filtered
	return modified
}

// repairResponsesToolCallPairing 修复 input[] 中工具调用项与输出项的 call_id 配对。
// 部分客户端（如 VSCode Copilot 的 Responses 直连）在长会话做上下文裁剪/摘要时，
// 会丢掉配对中的一半：只剩 *_call_output 时上游 400 "No tool call found for
// function call output with call_id ..."（issue #414），只剩 *_call 时上游 400
// "No tool output found for function call ..."。修复策略：
//   - 孤儿 *_call_output（含缺 call_id 的）：改写为 user message 保留输出文本，
//     避免直接丢弃造成上下文缺失；
//   - 孤儿 function_call / custom_tool_call：紧随其后补一条占位 output；
//     其余 *_call 类型形态不明，原样保留不做合成。
func repairResponsesToolCallPairing(body map[string]any) bool {
	inputItems, ok := body["input"].([]any)
	if !ok || len(inputItems) == 0 {
		return false
	}

	callIDs := make(map[string]bool)
	outputIDs := make(map[string]bool)
	for _, raw := range inputItems {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		callID := strings.TrimSpace(firstNonEmptyAnyString(item["call_id"]))
		if callID == "" {
			continue
		}
		typ := strings.TrimSpace(firstNonEmptyAnyString(item["type"]))
		switch {
		case isCodexToolCallContextType(typ):
			callIDs[callID] = true
		case isCodexToolCallOutputType(typ):
			outputIDs[callID] = true
		}
	}

	orphanOutputs, orphanCalls := 0, 0
	out := make([]any, 0, len(inputItems))
	for _, raw := range inputItems {
		item, ok := raw.(map[string]any)
		if !ok {
			out = append(out, raw)
			continue
		}
		typ := strings.TrimSpace(firstNonEmptyAnyString(item["type"]))
		callID := strings.TrimSpace(firstNonEmptyAnyString(item["call_id"]))
		switch {
		case isCodexToolCallOutputType(typ):
			if callID != "" && callIDs[callID] {
				out = append(out, raw)
				continue
			}
			out = append(out, orphanToolOutputAsMessage(callID, item["output"]))
			orphanOutputs++
		case isCodexToolCallContextType(typ):
			out = append(out, raw)
			if callID == "" || outputIDs[callID] {
				continue
			}
			outputType := codexToolCallOutputTypeForCall(typ)
			if outputType == "" {
				continue
			}
			out = append(out, map[string]any{
				"type":    outputType,
				"call_id": callID,
				"output":  "[tool output was not recorded]",
			})
			// 标记已补齐，同 call_id 重复出现时不再重复合成
			outputIDs[callID] = true
			orphanCalls++
		default:
			out = append(out, raw)
		}
	}

	if orphanOutputs == 0 && orphanCalls == 0 {
		return false
	}
	body["input"] = out
	log.Printf("已修复 input 工具调用配对: 孤儿输出转消息 %d 条, 孤儿调用补占位输出 %d 条", orphanOutputs, orphanCalls)
	return true
}

// orphanToolOutputAsMessage 把无法配对的工具输出项改写为 user message，
// 保留输出文本供模型继续参考。
func orphanToolOutputAsMessage(callID string, output any) map[string]any {
	label := "[Tool output from an earlier turn]"
	if callID != "" {
		label = "[Tool output from an earlier turn, call_id " + callID + "]"
	}
	return map[string]any{
		"type": "message",
		"role": "user",
		"content": []any{
			map[string]any{
				"type": "input_text",
				"text": label + "\n" + flattenToolOutputText(output),
			},
		},
	}
}

// flattenToolOutputText 把 *_call_output 的 output 字段拍平成纯文本。
// output 可能是 string，也可能是 [{type:"output_text",text:"..."}] 形式的内容数组。
func flattenToolOutputText(output any) string {
	switch v := output.(type) {
	case nil:
		return ""
	case string:
		return v
	case []any:
		var sb strings.Builder
		for _, raw := range v {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if text := firstNonEmptyAnyString(part["text"]); text != "" {
				if sb.Len() > 0 {
					sb.WriteString("\n")
				}
				sb.WriteString(text)
			}
		}
		if sb.Len() > 0 {
			return sb.String()
		}
	}
	if encoded, err := json.Marshal(output); err == nil {
		return string(encoded)
	}
	return ""
}

// codexToolCallOutputTypeForCall 返回可安全合成占位输出的调用项对应的输出项类型。
func codexToolCallOutputTypeForCall(callType string) string {
	switch callType {
	case "function_call":
		return "function_call_output"
	case "custom_tool_call":
		return "custom_tool_call_output"
	default:
		return ""
	}
}

func normalizeResponsesInputMessageContent(body map[string]any) bool {
	inputItems, ok := body["input"].([]any)
	if !ok {
		return false
	}

	modified := false
	for _, raw := range inputItems {
		itemMap, ok := raw.(map[string]any)
		if !ok || !isResponsesMessageInputItem(itemMap) {
			continue
		}
		if content, exists := itemMap["content"]; !exists || content == nil {
			itemMap["content"] = ""
			modified = true
		}
	}
	return modified
}

func isResponsesMessageInputItem(item map[string]any) bool {
	itemType := strings.TrimSpace(firstNonEmptyAnyString(item["type"]))
	if itemType == "message" {
		return true
	}
	if itemType != "" {
		return false
	}

	switch strings.TrimSpace(firstNonEmptyAnyString(item["role"])) {
	case "user", "assistant", "developer", "system":
		return true
	default:
		return false
	}
}

func normalizeResponsesInputItemIDs(body map[string]any) bool {
	inputItems, ok := body["input"].([]any)
	if !ok {
		return false
	}

	modified := false
	for _, raw := range inputItems {
		itemMap, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if _, exists := itemMap["id"]; exists {
			delete(itemMap, "id")
			modified = true
		}
	}
	return modified
}

func normalizeResponsesContentPartTypes(body map[string]any) bool {
	inputItems, ok := body["input"].([]any)
	if !ok {
		return false
	}

	modified := false
	for _, raw := range inputItems {
		itemMap, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role := strings.TrimSpace(firstNonEmptyAnyString(itemMap["role"]))
		if normalizeResponsesContentItemType(itemMap, role) {
			modified = true
		}
		contentItems, ok := itemMap["content"].([]any)
		if !ok {
			continue
		}
		for _, rawContent := range contentItems {
			contentMap, ok := rawContent.(map[string]any)
			if !ok {
				continue
			}
			if normalizeResponsesContentItemType(contentMap, role) {
				modified = true
			}
		}
	}
	return modified
}

func normalizeResponsesContentItemType(item map[string]any, role string) bool {
	itemType := strings.TrimSpace(firstNonEmptyAnyString(item["type"]))
	modified := false

	switch itemType {
	case "file":
		item["type"] = "input_file"
		itemType = "input_file"
		modified = true
	case "image", "image_url":
		item["type"] = "input_image"
		itemType = "input_image"
		modified = true
	case "text":
		if strings.TrimSpace(role) == "assistant" {
			item["type"] = "output_text"
		} else {
			item["type"] = "input_text"
		}
		itemType = firstNonEmptyAnyString(item["type"])
		modified = true
	case "input_text":
		if strings.TrimSpace(role) == "assistant" {
			item["type"] = "output_text"
			itemType = "output_text"
			modified = true
		}
	case "output_text":
		if strings.TrimSpace(role) != "assistant" {
			item["type"] = "input_text"
			itemType = "input_text"
			modified = true
		}
	}

	if itemType == "input_file" {
		if normalizeResponsesInputFileFields(item) {
			modified = true
		}
	}
	if itemType == "input_image" || itemType == "computer_screenshot" {
		if normalizeResponsesImageURLField(item) {
			modified = true
		}
	}
	return modified
}

func isInvalidEncryptedContentError(statusCode int, body []byte) bool {
	if statusCode != http.StatusBadRequest {
		return false
	}
	if isMissingEncryptedContentError(body) {
		return true
	}
	for _, path := range []string{"error.code", "detail.code", "code"} {
		if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(body, path).String()), "invalid_encrypted_content") {
			return true
		}
	}
	msgParts := []string{
		gjson.GetBytes(body, "error.message").String(),
		gjson.GetBytes(body, "detail").String(),
		string(body),
	}
	for _, msg := range msgParts {
		msg = strings.ToLower(msg)
		if strings.Contains(msg, "invalid_encrypted_content") {
			return true
		}
		if strings.Contains(msg, "encrypted content") &&
			(strings.Contains(msg, "could not be verified") || strings.Contains(msg, "could not be decrypted")) {
			return true
		}
	}
	return false
}

func isMissingEncryptedContentError(body []byte) bool {
	code := strings.TrimSpace(gjson.GetBytes(body, "error.code").String())
	param := strings.TrimSpace(gjson.GetBytes(body, "error.param").String())
	if !strings.EqualFold(code, "missing_required_parameter") || !strings.HasSuffix(param, ".encrypted_content") {
		return false
	}
	msg := strings.ToLower(gjson.GetBytes(body, "error.message").String())
	return strings.Contains(msg, "encrypted_content")
}

func stripInvalidEncryptedContentFromResponsesBody(body []byte) ([]byte, bool) {
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil || root == nil {
		return body, false
	}
	input, ok := root["input"]
	if !ok {
		return body, false
	}
	strippedInput, changed, keep := stripInvalidEncryptedContentValue(input, false)
	if !changed {
		return body, false
	}
	if keep {
		root["input"] = strippedInput
	} else {
		delete(root, "input")
	}
	stripped, err := json.Marshal(root)
	if err != nil {
		return body, false
	}
	return stripped, true
}

// isEncryptedCompactionItemType 报告该 input 项类型是否属于「密文即必填」的压缩项。
// 与 gjsonResultIsCompactionHistory 同口径，另含响应侧回灌的 compaction_summary。
func isEncryptedCompactionItemType(itemType string) bool {
	switch strings.ToLower(strings.TrimSpace(itemType)) {
	case "compaction", "context_compaction", "compaction_summary":
		return true
	default:
		return false
	}
}

func stripInvalidEncryptedContentValue(value any, arrayItem bool) (any, bool, bool) {
	switch v := value.(type) {
	case []any:
		changed := false
		out := make([]any, 0, len(v))
		for _, item := range v {
			stripped, itemChanged, keep := stripInvalidEncryptedContentValue(item, true)
			if itemChanged {
				changed = true
			}
			if !keep {
				changed = true
				continue
			}
			out = append(out, stripped)
		}
		return out, changed, true
	case map[string]any:
		changed := false
		itemType := strings.TrimSpace(firstNonEmptyAnyString(v["type"]))
		_, hasEncrypted := v["encrypted_content"]
		switch {
		case itemType == "reasoning":
			if arrayItem {
				return nil, true, false
			}
			if hasEncrypted {
				delete(v, "encrypted_content")
			}
			if len(v) == 1 {
				return nil, true, false
			}
			changed = true
		case hasEncrypted && isEncryptedCompactionItemType(itemType):
			// 压缩项的 encrypted_content 是必填字段：只摘掉字段会留下
			// {"type":"compaction"} 空壳继续上行，上游转而以
			// missing_required_parameter 再拒一次，而单次重试闸此时已经用掉。
			// 带密文时整项丢弃，与上面的 reasoning 分支对称；不带密文的压缩项
			// 本就没有账号绑定，原样保留。
			return nil, true, false
		case hasEncrypted:
			delete(v, "encrypted_content")
			changed = true
		}
		for key, child := range v {
			stripped, childChanged, keep := stripInvalidEncryptedContentValue(child, false)
			if childChanged {
				changed = true
			}
			if keep {
				v[key] = stripped
			} else {
				delete(v, key)
			}
		}
		return v, changed, true
	default:
		return value, false, true
	}
}

func responsesInputRaw(body []byte) string {
	input := gjson.GetBytes(body, "input")
	if !input.Exists() {
		return ""
	}
	return input.Raw
}

func dropBareReasoningInputItems(body map[string]any) bool {
	input, ok := body["input"]
	if !ok {
		return false
	}
	cleaned, changed, keep := dropBareReasoningInputValue(input)
	if !changed {
		return false
	}
	if keep {
		body["input"] = cleaned
	} else {
		delete(body, "input")
	}
	return true
}

// codexEncryptedContentPrefix 是 Codex 上游 reasoning.encrypted_content 的
// Fernet 令牌前缀：版本字节 0x80 使 base64 编码恒以 "gAAAA" 开头。其他渠道
// （如 Grok）的密文是裸 base64，不含此前缀，可据此区分血统。
const codexEncryptedContentPrefix = "gAAAA"

func dropBareReasoningInputValue(value any) (any, bool, bool) {
	switch v := value.(type) {
	case []any:
		changed := false
		out := make([]any, 0, len(v))
		for _, item := range v {
			cleaned, itemChanged, keep := dropBareReasoningInputValue(item)
			if itemChanged {
				changed = true
			}
			if !keep {
				changed = true
				continue
			}
			out = append(out, cleaned)
		}
		return out, changed, true
	case map[string]any:
		if strings.TrimSpace(firstNonEmptyAnyString(v["type"])) == "reasoning" {
			// 无密文、或外渠道血统密文（非 Fernet 前缀）的 reasoning 项整项
			// 丢弃：Codex 上游要求 reasoning 必带自家可解的 encrypted_content，
			// 缺失报 missing_required_parameter，外来密文报
			// invalid_encrypted_content（issue #565）。
			ec := firstNonEmptyAnyString(v["encrypted_content"])
			if ec == "" || !strings.HasPrefix(ec, codexEncryptedContentPrefix) {
				return nil, true, false
			}
			// Codex reasoning schema 不认 status 字段（400 unknown_parameter），
			// 自家输出也从不携带；跨渠道会话中客户端可能裸回灌带 status 的
			// 外渠道 reasoning 输出（issue #565）。
			if _, has := v["status"]; has {
				delete(v, "status")
				return v, true, true
			}
		}
		return v, false, true
	default:
		return value, false, true
	}
}

func normalizeResponsesInputFileFields(item map[string]any) bool {
	rawFile, hasFile := item["file"]
	if !hasFile {
		return false
	}

	if fileMap, ok := rawFile.(map[string]any); ok {
		for _, key := range []string{"file_id", "file_data", "file_url", "filename"} {
			if _, exists := item[key]; exists {
				continue
			}
			if value, exists := fileMap[key]; exists && value != nil {
				item[key] = value
			}
		}
	} else if fileID := strings.TrimSpace(firstNonEmptyAnyString(rawFile)); fileID != "" {
		if _, exists := item["file_id"]; !exists {
			item["file_id"] = fileID
		}
	}
	delete(item, "file")
	return true
}

func normalizeResponsesImageURLField(item map[string]any) bool {
	rawImageURL, ok := item["image_url"]
	if !ok {
		return false
	}
	imageURLMap, ok := rawImageURL.(map[string]any)
	if !ok {
		return false
	}
	if url := strings.TrimSpace(firstNonEmptyAnyString(imageURLMap["url"])); url != "" {
		item["image_url"] = url
		return true
	}
	return false
}

// compactionSummaryText extracts a usable summary string from a compaction
// item's summary field. Strings pass through trimmed; non-string values are
// JSON-serialized so the model still receives the original payload as text.
func compactionSummaryText(raw any) string {
	if raw == nil {
		return ""
	}
	if s, ok := raw.(string); ok {
		return strings.TrimSpace(s)
	}
	if b, err := json.Marshal(raw); err == nil {
		return strings.TrimSpace(string(b))
	}
	return ""
}

func truncateToolsPreservingImageGeneration(tools []any) []any {
	if len(tools) <= maxTools {
		return tools
	}
	imageIndex := -1
	for i, rawTool := range tools {
		toolMap, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		if strings.TrimSpace(firstNonEmptyAnyString(toolMap["type"])) == "image_generation" {
			imageIndex = i
			break
		}
	}
	if imageIndex < 0 || imageIndex < maxTools {
		return tools[:maxTools]
	}
	truncated := append([]any(nil), tools[:maxTools]...)
	truncated[maxTools-1] = tools[imageIndex]
	return truncated
}

func (c *requestCache) get(key [32]byte) (openAIRequest, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	elem, ok := c.items[key]
	if !ok {
		return openAIRequest{}, false
	}
	c.order.MoveToFront(elem)
	return elem.Value.(*requestCacheEntry).req, true
}

func (c *requestCache) put(key [32]byte, req openAIRequest) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if elem, ok := c.items[key]; ok {
		c.order.MoveToFront(elem)
		elem.Value.(*requestCacheEntry).req = req
		return
	}
	elem := c.order.PushFront(&requestCacheEntry{key: key, req: req})
	c.items[key] = elem
	if c.order.Len() <= requestCacheSize {
		return
	}
	tail := c.order.Back()
	if tail == nil {
		return
	}
	c.order.Remove(tail)
	delete(c.items, tail.Value.(*requestCacheEntry).key)
}

// cachedOrParse 从缓存获取或解析请求，返回结构体（Unmarshal 至多一次）
func cachedOrParse(rawJSON []byte) openAIRequest {
	if len(rawJSON) == 0 {
		return openAIRequest{}
	}
	key := sha256.Sum256(rawJSON)
	if req, ok := globalRequestCache.get(key); ok {
		return req
	}
	var req openAIRequest
	_ = json.Unmarshal(rawJSON, &req)
	globalRequestCache.put(key, req)
	return req
}

// ==================== 请求翻译: OpenAI Chat Completions → Codex Responses ====================

// TranslateRequest 将 OpenAI Chat Completions 请求转换为 Codex Responses 格式
// 采用 Unmarshal→构造 map→Marshal 模式，只做一次 JSON 序列化
func TranslateRequest(rawJSON []byte) ([]byte, error) {
	req := sanitizeChatCompletionToolHistory(cachedOrParse(rawJSON))
	if err := validateChatCompletionFunctionNames(req); err != nil {
		return nil, err
	}
	out := buildChatResponsesRequest(req)
	return json.Marshal(out)
}

// TranslateChatToResponsesForGrok converts a Chat Completions request into a
// canonical Responses request for Grok routing. Unlike TranslateRequest, which
// remains Codex-safe, this entry point retains request controls that a real
// Responses backend can express (sampling, output limit, stop, penalties and
// tool selection). Keeping the two entry points separate prevents those fields
// from reaching the Codex backend, where they are unsupported.
func TranslateChatToResponsesForGrok(rawJSON []byte) ([]byte, error) {
	req := sanitizeChatCompletionToolHistory(cachedOrParse(rawJSON))
	if err := validateChatCompletionFunctionNames(req); err != nil {
		return nil, err
	}
	out := buildChatResponsesRequest(req)
	// Rebuild tools and structured output without Codex-only schema cleanup.
	if len(req.Tools) > 0 {
		if tools := convertChatToolsToResponsesForGrok(req.Tools); len(tools) > 0 {
			out["tools"] = tools
		}
	}
	if format := chatResponseFormatToResponses(req.ResponseFormat); format != nil {
		out["text"] = map[string]any{"format": format}
	}
	if err := copyChatResponsesControls(out, req); err != nil {
		return nil, err
	}
	return json.Marshal(out)
}

// sanitizeChatCompletionToolHistory removes truncated ordinary function calls
// together with their matching tool outputs before converting historical Chat
// messages to Responses input. Empty arguments are the common provider shorthand
// for an empty object and are normalized to {}. Custom/provider tool shapes are
// left untouched because their input is not required to be JSON.
func sanitizeChatCompletionToolHistory(req openAIRequest) openAIRequest {
	if len(req.Messages) == 0 {
		return req
	}

	invalidCallIDs := make(map[string]struct{})
	validCallIDs := make(map[string]struct{})
	invalidEmptyCalls := 0
	filtered := make([]openAIMessage, 0, len(req.Messages))
	modified := false

	for _, message := range req.Messages {
		if message.Role != "assistant" || len(message.ToolCalls) == 0 {
			filtered = append(filtered, message)
			continue
		}
		calls := make([]openAIToolCall, 0, len(message.ToolCalls))
		for _, toolCall := range message.ToolCalls {
			if toolCall.Type != "" && toolCall.Type != "function" {
				calls = append(calls, toolCall)
				continue
			}
			arguments := strings.TrimSpace(toolCall.Function.Arguments)
			if arguments == "" {
				toolCall.Function.Arguments = "{}"
				arguments = "{}"
				modified = true
			}
			if !json.Valid([]byte(arguments)) {
				modified = true
				if callID := strings.TrimSpace(toolCall.ID); callID != "" {
					invalidCallIDs[callID] = struct{}{}
				} else {
					invalidEmptyCalls++
				}
				continue
			}
			if callID := strings.TrimSpace(toolCall.ID); callID != "" {
				validCallIDs[callID] = struct{}{}
			}
			calls = append(calls, toolCall)
		}
		for callID := range validCallIDs {
			delete(invalidCallIDs, callID)
		}
		message.ToolCalls = calls
		if len(calls) == 0 && strings.TrimSpace(rawMessageToString(message.Content)) == "" {
			continue
		}
		filtered = append(filtered, message)
	}

	if len(invalidCallIDs) > 0 || invalidEmptyCalls > 0 {
		withoutOutputs := filtered[:0]
		for _, message := range filtered {
			if message.Role == "tool" {
				callID := strings.TrimSpace(message.ToolCallID)
				if callID == "" && invalidEmptyCalls > 0 {
					invalidEmptyCalls--
					modified = true
					continue
				}
				if _, invalid := invalidCallIDs[callID]; invalid {
					modified = true
					continue
				}
			}
			withoutOutputs = append(withoutOutputs, message)
		}
		filtered = withoutOutputs
	}
	if modified {
		req.Messages = filtered
	}
	return req
}

func buildChatResponsesRequest(req openAIRequest) map[string]any {
	// 构建输出 map（只包含 Codex 需要的字段）
	out := map[string]any{
		"model":   req.Model,
		"stream":  true,
		"store":   false,
		"include": []string{"reasoning.encrypted_content"},
	}

	// 1. messages → input
	out["input"] = convertMessagesToInputSlice(req.Messages)
	normalizeResponsesContentPartTypes(out)
	normalizeResponsesInputMessageContent(out)
	normalizeResponsesInputItemIDs(out)

	// 2. reasoning effort + summary
	// 显式向 Codex 请求 summary,否则上游不会发 response.reasoning_summary_text.delta,
	// chat/completions 客户端就拿不到思考内容(issue #156)。
	if effort := normalizeReasoningEffortForModel(req.ReasoningEffort, req.Model); effort != "" {
		out["reasoning"] = map[string]any{
			"effort":  effort,
			"summary": "auto",
		}
	} else {
		out["reasoning"] = map[string]any{"summary": "auto"}
	}

	// 3. service tier（兼容客户端字段；只有 fast/priority 会显式传给 Codex 上游）
	tier := req.ServiceTier
	if tier == "" {
		tier = req.ServiceTierAlt
	}
	tier = strings.TrimSpace(tier)
	if isAllowedServiceTier(tier) {
		if upstreamTier, ok := upstreamServiceTier(tier); ok {
			out["service_tier"] = upstreamTier
		}
	}

	// 4. tools 格式转换 + schema 清理
	if len(req.Tools) > 0 {
		if tools := convertToolsToCodexFormat(req.Tools); len(tools) > 0 {
			out["tools"] = tools
		}
	}

	// 5. response_format → Responses text.format，并清理结构化输出 schema
	if len(req.ResponseFormat) > 0 && string(req.ResponseFormat) != "null" {
		var responseFormat map[string]any
		if json.Unmarshal(req.ResponseFormat, &responseFormat) == nil && responseFormat != nil {
			out["response_format"] = responseFormat
			normalizeResponsesStructuredOutputFormat(out)
			delete(out, "response_format")
		}
	}

	return out
}

func copyRawJSONField(out map[string]any, name string, raw json.RawMessage) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return
	}
	var value any
	if json.Unmarshal(raw, &value) == nil {
		out[name] = value
	}
}

func copyChatResponsesControls(out map[string]any, req openAIRequest) error {
	copyRawJSONField(out, "temperature", req.Temperature)
	copyRawJSONField(out, "top_p", req.TopP)
	copyRawJSONField(out, "stop", req.Stop)
	copyRawJSONField(out, "seed", req.Seed)
	copyRawJSONField(out, "presence_penalty", req.PresencePenalty)
	copyRawJSONField(out, "frequency_penalty", req.FrequencyPenalty)
	copyRawJSONField(out, "parallel_tool_calls", req.ParallelToolCalls)

	// max_completion_tokens is the newer Chat spelling and therefore wins when
	// both aliases are supplied.
	maxOutputTokens := req.MaxTokens
	if len(req.MaxCompletionTokens) > 0 && strings.TrimSpace(string(req.MaxCompletionTokens)) != "null" {
		maxOutputTokens = req.MaxCompletionTokens
	}
	copyRawJSONField(out, "max_output_tokens", maxOutputTokens)

	if len(req.ToolChoice) == 0 || strings.TrimSpace(string(req.ToolChoice)) == "null" {
		return nil
	}
	var choice any
	if json.Unmarshal(req.ToolChoice, &choice) != nil {
		return fmt.Errorf("tool_choice must be a valid Chat Completions tool selection")
	}
	// Chat names a selected function under tool_choice.function.name whereas
	// Responses expects {type:"function",name:"..."}.
	switch typed := choice.(type) {
	case string:
		switch strings.TrimSpace(typed) {
		case "auto", "none", "required":
			out["tool_choice"] = typed
			return nil
		default:
			return fmt.Errorf("Chat Completions tool_choice %q cannot be represented by Responses", typed)
		}
	case map[string]any:
		if strings.TrimSpace(firstNonEmptyAnyString(typed["type"])) != "function" {
			return fmt.Errorf("Chat Completions tool_choice type %q cannot be represented by Responses", firstNonEmptyAnyString(typed["type"]))
		}
		name := strings.TrimSpace(firstNonEmptyAnyString(typed["name"]))
		if function, ok := typed["function"].(map[string]any); ok && name == "" {
			name = strings.TrimSpace(firstNonEmptyAnyString(function["name"]))
		}
		if name == "" {
			return fmt.Errorf("Chat Completions function tool_choice requires function.name")
		}
		out["tool_choice"] = map[string]any{"type": "function", "name": name}
		return nil
	default:
		return fmt.Errorf("Chat Completions tool_choice cannot be represented by Responses")
	}
}

func convertChatToolsToResponsesForGrok(rawTools []json.RawMessage) []any {
	tools := make([]any, 0, len(rawTools))
	for _, raw := range rawTools {
		var source map[string]any
		if json.Unmarshal(raw, &source) != nil || source == nil {
			continue
		}
		function, isFunction := source["function"].(map[string]any)
		toolType := strings.TrimSpace(firstNonEmptyAnyString(source["type"]))
		if function != nil && (toolType == "" || toolType == "function") {
			item := map[string]any{"type": "function"}
			for _, field := range []string{"name", "description", "parameters", "strict"} {
				if value, ok := function[field]; ok {
					item[field] = value
				}
			}
			tools = append(tools, item)
			continue
		}
		// A provider extension already using a Responses-shaped tool object can
		// be carried into the canonical request without reinterpretation.
		tools = append(tools, source)
		_ = isFunction
	}
	return tools
}

func chatResponseFormatToResponses(raw json.RawMessage) map[string]any {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return nil
	}
	var responseFormat map[string]any
	if json.Unmarshal(raw, &responseFormat) != nil || responseFormat == nil {
		return nil
	}
	return responsesTextFormatFromResponseFormat(responseFormat)
}

func invalidFunctionNameError(path string) error {
	return fmt.Errorf("Invalid '%s': empty string. Expected a string with minimum length 1, but got an empty string instead.", path)
}

func validateChatCompletionFunctionNames(req openAIRequest) error {
	knownCalls := make(map[string]struct{})
	for msgIdx, msg := range req.Messages {
		if msg.Role == "tool" {
			callID := strings.TrimSpace(msg.ToolCallID)
			if callID == "" {
				return fmt.Errorf("messages[%d] orphan tool message without tool_call_id", msgIdx)
			}
			if _, ok := knownCalls[callID]; !ok {
				return fmt.Errorf("messages[%d] orphan tool message for unknown tool_call_id %q", msgIdx, callID)
			}
		}
		for callIdx, toolCall := range msg.ToolCalls {
			if strings.TrimSpace(toolCall.Function.Name) == "" {
				return invalidFunctionNameError(fmt.Sprintf("messages[%d].tool_calls[%d].function.name", msgIdx, callIdx))
			}
			if callID := strings.TrimSpace(toolCall.ID); msg.Role == "assistant" && callID != "" {
				knownCalls[callID] = struct{}{}
			}
		}
	}
	for toolIdx, rawTool := range req.Tools {
		var parsed openAIToolParsed
		if err := json.Unmarshal(rawTool, &parsed); err != nil || parsed.Type != "function" || parsed.Function == nil {
			continue
		}
		if strings.TrimSpace(parsed.Function.Name) == "" {
			return invalidFunctionNameError(fmt.Sprintf("tools[%d].function.name", toolIdx))
		}
	}
	return nil
}

// ValidateResponsesFunctionNames rejects malformed tool-call names before they
// reach the upstream Responses API. The upstream reports these as HTTP 400
// empty_string errors; local validation makes the bad client field obvious.
// ValidateResponsesFunctionNames 校验 input[] 中 function_call 与 tools[] 中
// function 工具的 name 非空。采用 gjson 惰性遍历，只走 input/tools 两个数组，
// 不把整份请求体反序列化成 map[string]any——大请求体(曾 16MB)上全量 Unmarshal
// 会带来秒级开销且随后 prepare 阶段还会再解析一次(issue #417)。
func ValidateResponsesFunctionNames(rawBody []byte) error {
	// 无效 JSON 时 gjson 查询返回空结果、校验直接通过（与旧版 Unmarshal 失败即
	// 放行一致），无需先做一次全量 ValidBytes 扫描。
	var funcErr error
	gjson.GetBytes(rawBody, "input").ForEach(func(idx, item gjson.Result) bool {
		if strings.TrimSpace(item.Get("type").String()) != "function_call" {
			return true
		}
		if strings.TrimSpace(item.Get("name").String()) == "" {
			funcErr = invalidFunctionNameError(fmt.Sprintf("input[%d].name", idx.Int()))
			return false
		}
		return true
	})
	if funcErr != nil {
		return funcErr
	}

	gjson.GetBytes(rawBody, "tools").ForEach(func(idx, tool gjson.Result) bool {
		if strings.TrimSpace(tool.Get("type").String()) != "function" {
			return true
		}
		name := strings.TrimSpace(tool.Get("name").String())
		if name == "" {
			name = strings.TrimSpace(tool.Get("function.name").String())
		}
		if name == "" {
			path := fmt.Sprintf("tools[%d].name", idx.Int())
			if tool.Get("function").IsObject() {
				path = fmt.Sprintf("tools[%d].function.name", idx.Int())
			}
			funcErr = invalidFunctionNameError(path)
			return false
		}
		return true
	})
	return funcErr
}

func normalizeResponsesFunctionTools(body map[string]any) bool {
	tools, ok := body["tools"].([]any)
	if !ok {
		return false
	}
	kept, modified := normalizeFunctionToolsInArray(tools)
	if modified {
		body["tools"] = kept
	}
	return modified
}

// normalizeFunctionToolsInArray 处理一组工具声明的「形态」问题：补全缺失的 type、
// 把 Chat 形态的 function 子对象摊平到顶层、剔除无法识别的项。必须在
// normalizeResponsesToolSchema 之前跑——后者靠顶层 type 判断是不是 function 工具，
// 缺 type 就拿不到 parameters 根节点的 object 兜底。
func normalizeFunctionToolsInArray(tools []any) ([]any, bool) {
	modified := false
	kept := make([]any, 0, len(tools))
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			kept = append(kept, rawTool)
			continue
		}
		// 保留工具原样透传，不摊平 function 子对象、不改写字段（issue #342）。
		if isReservedCodexTool(tool) {
			kept = append(kept, tool)
			continue
		}
		toolType := strings.TrimSpace(firstNonEmptyAnyString(tool["type"]))
		if toolType == "" {
			// 上游对缺失或为 null 的工具 type 返回 400 "Unsupported tool
			// type: None"（issue #219）。带 function 形态（function 子对象
			// 或顶层 name）的工具按 OpenAI SDK 惯例视为 function；无法识别
			// 形态的工具直接剔除，避免整个请求被上游拒绝。
			function, _ := tool["function"].(map[string]any)
			if function == nil &&
				strings.TrimSpace(firstNonEmptyAnyString(tool["name"])) == "" {
				modified = true
				continue
			}
			tool["type"] = "function"
			modified = true
		} else if toolType != "function" {
			kept = append(kept, tool)
			continue
		}
		kept = append(kept, tool)
		function, _ := tool["function"].(map[string]any)
		if function == nil {
			continue
		}
		if strings.TrimSpace(firstNonEmptyAnyString(tool["name"])) == "" {
			if name := strings.TrimSpace(firstNonEmptyAnyString(function["name"])); name != "" {
				tool["name"] = name
				modified = true
			}
		}
		if _, ok := tool["description"]; !ok {
			if desc := strings.TrimSpace(firstNonEmptyAnyString(function["description"])); desc != "" {
				tool["description"] = desc
				modified = true
			}
		}
		if _, ok := tool["parameters"]; !ok {
			if params, ok := function["parameters"]; ok {
				tool["parameters"] = params
				modified = true
			}
		}
		if _, ok := tool["strict"]; !ok {
			if strict, ok := function["strict"]; ok {
				tool["strict"] = strict
				modified = true
			}
		}
		delete(tool, "function")
		modified = true
	}
	return kept, modified
}

func normalizeResponsesToolChoice(body map[string]any) bool {
	rawChoice, ok := body["tool_choice"]
	if !ok {
		return false
	}
	choice, ok := rawChoice.(map[string]any)
	if !ok {
		return false
	}

	modified := false
	toolType := strings.TrimSpace(firstNonEmptyAnyString(choice["type"]))
	function, _ := choice["function"].(map[string]any)
	name := strings.TrimSpace(firstNonEmptyAnyString(choice["name"]))
	if toolType == "" && (function != nil || name != "") {
		choice["type"] = "function"
		toolType = "function"
		modified = true
	}
	if toolType != "function" {
		return modified
	}
	if name == "" && function != nil {
		if nestedName := strings.TrimSpace(firstNonEmptyAnyString(function["name"])); nestedName != "" {
			choice["name"] = nestedName
			modified = true
		}
	}
	if function != nil {
		delete(choice, "function")
		modified = true
	}
	return modified
}

type responsesBodyPrepareOptions struct {
	forceStoreFalse            bool
	expandPreviousResponse     bool
	preservePreviousResponseID bool
	cachedResponseItems        []json.RawMessage
	// cacheOwner 是 previous_response_id 展开时使用的缓存归属命名空间
	//（见 responseCacheOwner）。owner 不匹配的缓存按未命中处理，防跨用户注入。
	cacheOwner string
}

// PrepareResponsesBody 将 Responses API 原始请求转换为上游可接受的格式
// 采用 Unmarshal→map 操作→Marshal 模式，替代逐字段 sjson 操作
// 返回: (处理后的 body, 展开后的 input JSON 原始文本)
func PrepareResponsesBody(rawBody []byte) ([]byte, string) {
	return PrepareResponsesBodyForOwner(rawBody, "")
}

// PrepareResponsesBodyForOwner 同 PrepareResponsesBody，但 previous_response_id
// 展开限定在 owner 的缓存命名空间内（owner 见 responseCacheOwner）。
func PrepareResponsesBodyForOwner(rawBody []byte, owner string) ([]byte, string) {
	prepared := prepareResponsesBodyForOwnerDetailed(rawBody, owner)
	return prepared.Body, prepared.ExpandedInputRaw
}

type responsesBodyPreparation struct {
	Body                 []byte
	ExpandedInputRaw     string
	PreviousResponseID   string
	CacheLookup          responseCacheLookupResult
	RequiresLocalContext bool
	Bypassed             bool
}

func prepareResponsesBodyForOwnerDetailed(rawBody []byte, owner string) responsesBodyPreparation {
	prevID := gjson.GetBytes(rawBody, "previous_response_id").String()
	preparation := responsesBodyPreparation{PreviousResponseID: prevID}
	if prevID != "" {
		currentInput := gjson.GetBytes(rawBody, "input")
		if currentInput.IsArray() && inputHasToolCallContext(currentInput) {
			preparation.Bypassed = true
		} else {
			preparation.RequiresLocalContext = currentInput.IsArray() && inputHasFunctionCallOutput(currentInput)
			preparation.CacheLookup = getResponseCacheResult(owner, prevID)
		}
	}
	preparedBody, expandedInputRaw := prepareResponsesBodyWithOptions(rawBody, responsesBodyPrepareOptions{
		forceStoreFalse:        true,
		expandPreviousResponse: false,
		cacheOwner:             owner,
		cachedResponseItems:    preparation.CacheLookup.Items,
	})
	preparation.Body = preparedBody
	preparation.ExpandedInputRaw = expandedInputRaw
	return preparation
}

// PrepareResponsesWebSocketBody keeps upstream response storage linkage for
// native Responses WebSocket sessions.
func PrepareResponsesWebSocketBody(rawBody []byte) ([]byte, string) {
	return prepareResponsesBodyWithOptions(rawBody, responsesBodyPrepareOptions{
		preservePreviousResponseID: true,
	})
}

const codexReasoningEncryptedContentInclude = "reasoning.encrypted_content"

func ensureDefaultCodexInclude(body map[string]any) {
	if body == nil {
		return
	}
	if _, ok := body["include"]; !ok {
		body["include"] = []string{codexReasoningEncryptedContentInclude}
	}
}

func ensureCodexReasoningInclude(body map[string]any) {
	if body == nil {
		return
	}
	if _, ok := body["reasoning"]; !ok {
		return
	}
	if _, ok := body["include"]; !ok {
		body["include"] = []string{codexReasoningEncryptedContentInclude}
		return
	}

	switch includes := body["include"].(type) {
	case []any:
		for _, item := range includes {
			if s, ok := item.(string); ok && s == codexReasoningEncryptedContentInclude {
				return
			}
		}
		body["include"] = append(includes, codexReasoningEncryptedContentInclude)
	case []string:
		for _, item := range includes {
			if item == codexReasoningEncryptedContentInclude {
				return
			}
		}
		body["include"] = append(includes, codexReasoningEncryptedContentInclude)
	}
}

func prepareResponsesBodyWithOptions(rawBody []byte, opts responsesBodyPrepareOptions) ([]byte, string) {
	var body map[string]any
	if err := json.Unmarshal(rawBody, &body); err != nil {
		return rawBody, ""
	}

	// 1. 强制设置 Codex 必需字段
	body["stream"] = true
	if opts.forceStoreFalse {
		body["store"] = false
	}
	ensureDefaultCodexInclude(body)

	normalizeResponsesImageOnlyModel(body)
	normalizeResponsesPromptCompat(body)

	// 2. 字符串 input → 数组包装（Codex 要求 input 为 list）
	if inputStr, ok := body["input"].(string); ok {
		body["input"] = []any{
			map[string]any{"role": "user", "content": inputStr},
		}
	}
	promptText := extractResponsesPromptText(body)

	// 3. reasoning_effort → reasoning.effort 自动转换 + 钳位（max 按模型放行）
	effortModel := firstNonEmptyAnyString(body["model"])
	if re, ok := body["reasoning_effort"].(string); ok {
		if normalized := normalizeReasoningEffortForModel(re, effortModel); normalized != "" {
			reasoning, _ := body["reasoning"].(map[string]any)
			if reasoning == nil {
				reasoning = map[string]any{}
			}
			if _, hasEffort := reasoning["effort"]; !hasEffort {
				reasoning["effort"] = normalized
				body["reasoning"] = reasoning
			}
		}
	}
	if reasoning, ok := body["reasoning"].(map[string]any); ok {
		if effort, ok := reasoning["effort"].(string); ok {
			if normalized := normalizeReasoningEffortForModel(effort, effortModel); normalized != "" {
				reasoning["effort"] = normalized
			} else {
				delete(reasoning, "effort")
			}
		}
	}
	ensureCodexReasoningInclude(body)

	// 4. service tier 清理（兼容客户端字段；只有 fast/priority 会显式传给 Codex 上游）
	delete(body, "serviceTier")
	if tier, ok := body["service_tier"].(string); ok {
		tier = strings.TrimSpace(tier)
		if !isAllowedServiceTier(tier) {
			delete(body, "service_tier")
		} else if upstreamTier, ok := upstreamServiceTier(tier); ok {
			body["service_tier"] = upstreamTier
		} else {
			delete(body, "service_tier")
		}
	}
	normalizeResponsesStructuredOutputFormat(body)
	normalizeResponsesFunctionTools(body)
	normalizeResponsesToolChoice(body)
	normalizeResponsesWebSearchTools(body)

	// 5. 工具描述补充 + schema 清理 + 上游数量限制
	if tools, ok := body["tools"].([]any); ok {
		if len(tools) > maxTools {
			tools = truncateToolsPreservingImageGeneration(tools)
			body["tools"] = tools
		}
		toolDescDefaults := map[string]string{
			"tool_search": "Search through available tools to find the most relevant one for the task.",
		}
		for _, t := range tools {
			normalizeResponsesToolSchema(t, toolDescDefaults)
		}
	}
	// Responses Lite 把工具声明搬进 input[] 的 additional_tools 载体项；顶层
	// tools[] 的清洗必须同样覆盖那里，否则坏 schema 绕过全部修正直达上游。
	normalizeResponsesAdditionalToolSchemas(body)
	if shouldAutoInjectResponsesImageGenerationTool(body) {
		ensureResponsesImageGenerationTool(body)
	}
	moveTopLevelResponsesImageOptions(body)
	normalizeResponsesImageGenerationTools(body, promptText)
	applyResponsesImageGenerationBridgeInstructions(body)

	// 6. 展开 previous_response_id（限定在请求归属的缓存命名空间，防跨用户注入）
	prevID, _ := body["previous_response_id"].(string)
	if prevID != "" {
		cached := opts.cachedResponseItems
		if opts.expandPreviousResponse && cached == nil {
			cached = getResponseCache(opts.cacheOwner, prevID)
		}
		if cached != nil {
			var cachedItems []any
			for _, item := range cached {
				var v any
				if json.Unmarshal(item, &v) == nil {
					cachedItems = append(cachedItems, v)
				}
			}
			currentInput, _ := body["input"].([]any)
			body["input"] = append(cachedItems, currentInput...)
		}
	}
	// 6b. 把 input[] 中的 compaction 项翻译为 developer message（上游不识别 compaction）
	normalizeResponsesCompactionItems(body)
	// system 角色消息 → developer（上游不接受 system 角色，issue #409）
	normalizeResponsesSystemRoleMessages(body)
	normalizeResponsesContentPartTypes(body)
	normalizeResponsesInputMessageContent(body)
	normalizeResponsesToolCallArgumentTypes(body)
	sanitizeMalformedResponsesFunctionCalls(body)
	normalizeResponsesInputItemIDs(body)
	dropBareReasoningInputItems(body)
	// 6c. 修复工具调用/输出的 call_id 配对（issue #414）。
	// previous_response_id 保留给上游的原生续链场景跳过：历史存于上游服务端，
	// 本地看似孤儿的输出项是合法续链，不能改写。
	if !(opts.preservePreviousResponseID && prevID != "") {
		repairResponsesToolCallPairing(body)
	}

	// 保存展开后的 input 原始 JSON（用于响应缓存链路）
	var expandedInputRaw string
	if inputVal, ok := body["input"]; ok {
		if b, err := json.Marshal(inputVal); err == nil {
			expandedInputRaw = string(b)
		}
	}

	// 7. 删除 Codex 不支持的字段
	// 注意：prompt_cache_retention 上游(HTTP 与 WS 路径)均不接受，会返回
	// 400 Unsupported parameter，因此在此一并剥离，executor / wsrelay 层也各自兜底删除。
	// 顶层 type 是 Responses WS 事件信封字段(response.create)，native WS ingress 会
	// 注入/保留它，HTTP /responses 上游不接受(400 Unsupported parameter: type)；
	// WS 出站由 wsrelay 统一重设该字段，此处删除对 WS 路径无影响(issue #548)。
	// map 上的 delete 只作用于顶层，input[]/tools 等嵌套对象里的合法 type 不受影响。
	for _, field := range []string{
		"max_output_tokens", "max_tokens", "max_completion_tokens",
		"temperature", "top_p", "frequency_penalty", "presence_penalty",
		"logprobs", "top_logprobs", "n", "seed", "stop", "user",
		"logit_bias", "response_format", "serviceTier", "metadata",
		"stream_options", "reasoning_effort", "truncation", "context_management",
		"disable_response_storage", "verbosity",
		"prompt_cache_retention", "safety_identifier", "type",
	} {
		delete(body, field)
	}
	if !opts.preservePreviousResponseID {
		delete(body, "previous_response_id")
	}

	result, err := json.Marshal(body)
	if err != nil {
		return rawBody, expandedInputRaw
	}
	result = normalizeCompactionTriggerFinal(result, false)
	if requestBodyHasCompactionTrigger(result) {
		expandedInputRaw = gjson.GetBytes(result, "input").Raw
	}
	return result, expandedInputRaw
}

// PrepareOpenAIResponsesBody keeps native OpenAI Responses requests compatible
// without applying Codex-specific fields such as store/include/tool injection.
func PrepareOpenAIResponsesBody(rawBody []byte) []byte {
	var body map[string]any
	if err := json.Unmarshal(rawBody, &body); err != nil {
		return rawBody
	}

	effortModel := firstNonEmptyAnyString(body["model"])
	if re, ok := body["reasoning_effort"].(string); ok {
		if normalized := normalizeReasoningEffortForModel(re, effortModel); normalized != "" {
			reasoning, _ := body["reasoning"].(map[string]any)
			if reasoning == nil {
				reasoning = map[string]any{}
			}
			if _, hasEffort := reasoning["effort"]; !hasEffort {
				reasoning["effort"] = normalized
				body["reasoning"] = reasoning
			}
		}
	}
	if reasoning, ok := body["reasoning"].(map[string]any); ok {
		if effort, ok := reasoning["effort"].(string); ok {
			if normalized := normalizeReasoningEffortForModel(effort, effortModel); normalized != "" {
				reasoning["effort"] = normalized
			} else {
				delete(reasoning, "effort")
			}
		}
	}

	normalizeResponsesStructuredOutputFormat(body)
	normalizeResponsesFunctionTools(body)
	normalizeResponsesToolChoice(body)
	normalizeResponsesContentPartTypes(body)
	normalizeResponsesInputMessageContent(body)
	if shouldInjectOpenAIResponsesImageGenerationTool(body) {
		ensureResponsesImageGenerationTool(body)
	}
	moveTopLevelResponsesImageOptions(body)
	normalizeResponsesImageGenerationTools(body, extractResponsesPromptText(body))

	result, err := json.Marshal(body)
	if err != nil {
		return rawBody
	}
	result = normalizeCompactionTriggerFinal(result, false)
	return result
}

// PrepareCompactResponsesBody 将 /responses/compact 请求转换为上游可接受的格式。
// 它复用通用 Responses 预处理，但会移除 compact 端点不接受的自动注入字段。
func PrepareCompactResponsesBody(rawBody []byte) ([]byte, string) {
	return PrepareCompactResponsesBodyForOwner(rawBody, "")
}

// PrepareCompactResponsesBodyForOwner 同 PrepareCompactResponsesBody，但
// previous_response_id 展开限定在 owner 的缓存命名空间内。
func PrepareCompactResponsesBodyForOwner(rawBody []byte, owner string) ([]byte, string) {
	prepared := prepareCompactResponsesBodyForOwnerDetailed(rawBody, owner)
	return prepared.Body, prepared.ExpandedInputRaw
}

func prepareCompactResponsesBodyForOwnerDetailed(rawBody []byte, owner string) responsesBodyPreparation {
	prepared := prepareResponsesBodyForOwnerDetailed(rawBody, owner)
	prepared.Body, _ = sjson.DeleteBytes(prepared.Body, "include")
	prepared.Body, _ = sjson.DeleteBytes(prepared.Body, "store")
	prepared.Body, _ = sjson.DeleteBytes(prepared.Body, "stream")
	// 普通 /responses 请求携带的客户端指纹元数据,compact 端点不认识该参数
	// (Unknown parameter: 'client_metadata')——body-signal 压缩提升会把普通
	// 请求形状的 body 送进本函数,须在此剥除。
	prepared.Body, _ = sjson.DeleteBytes(prepared.Body, "client_metadata")
	return prepared
}

// PrepareOpenAIResponsesCompactBody 为中转（OpenAI Responses API）账号准备
// /responses/compact 请求体。它复用 OpenAI Responses 预处理，并移除 compact
// 端点不接受的自动注入字段（include/store/stream/client_metadata）。
func PrepareOpenAIResponsesCompactBody(rawBody []byte) []byte {
	body := PrepareOpenAIResponsesBody(rawBody)
	body, _ = sjson.DeleteBytes(body, "include")
	body, _ = sjson.DeleteBytes(body, "store")
	body, _ = sjson.DeleteBytes(body, "stream")
	body, _ = sjson.DeleteBytes(body, "client_metadata")
	return body
}

// normalizeReasoningEffort 将 reasoning_effort 钳位到上游支持的值。
// max 仅 gpt-5.6 起的模型支持(旧模型上游 400),无模型上下文时安全钳到 xhigh;
// 有模型上下文的调用方用 normalizeReasoningEffortForModel。
func normalizeReasoningEffort(effort string) string {
	effort = strings.ToLower(strings.TrimSpace(effort))
	if effort == "" {
		return ""
	}
	switch effort {
	case "none", "minimal", "low", "medium", "high", "xhigh", "ultra":
		return effort
	case "max":
		return "xhigh"
	default:
		return "high"
	}
}

// normalizeReasoningEffortForModel 在通用钳位基础上按模型放行 max：
// gpt-5.6 起上游接受 effort=max 并原样回显；旧模型返回
// "Invalid value: 'max'"，一律钳到 xhigh。
func normalizeReasoningEffortForModel(effort, model string) string {
	if strings.ToLower(strings.TrimSpace(effort)) == "max" && modelSupportsMaxReasoningEffort(model) {
		return "max"
	}
	return normalizeReasoningEffort(effort)
}

// modelSupportsMaxReasoningEffort 判断模型是否支持 reasoning.effort=max
// （gpt-5.6 及更高版本；带变体后缀如 gpt-5.6-sol 同样识别）。
func modelSupportsMaxReasoningEffort(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	if !strings.HasPrefix(model, "gpt-") {
		return false
	}
	version := strings.TrimPrefix(model, "gpt-")
	if dash := strings.IndexByte(version, '-'); dash >= 0 {
		version = version[:dash]
	}
	parts := strings.Split(version, ".")
	if len(parts) < 2 {
		return false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return false
	}
	minor, err := strconv.Atoi(parts[1])
	if err != nil {
		return false
	}
	if major > 5 {
		return true
	}
	return major == 5 && minor >= 6
}

// isAllowedServiceTier 判断 service_tier 是否在上游允许的范围内
func isAllowedServiceTier(tier string) bool {
	switch tier {
	case "auto", "default", "flex", "priority", "scale", "fast":
		return true
	default:
		return false
	}
}

// upstreamServiceTier 将客户端 service_tier 映射为上游接受的值。
// Codex 上游当前只接受 priority；auto/default/flex/scale 都不应显式转发。
func upstreamServiceTier(tier string) (string, bool) {
	switch tier {
	case "fast", "priority":
		return "priority", true
	case "auto", "default", "flex", "scale":
		return "", false
	default:
		return "", false
	}
}

// convertMessagesToInputSlice 将 OpenAI messages 转换为 Codex input 数组（纯内存操作，零中间序列化）
func convertMessagesToInputSlice(messages []openAIMessage) []any {
	input := make([]any, 0, len(messages))

	for _, m := range messages {
		switch m.Role {
		case "tool":
			output, images := toolMessageOutputAndImages(m.Content)
			input = append(input, map[string]any{
				"type":    "function_call_output",
				"call_id": m.ToolCallID,
				"output":  output,
			})
			// function_call_output 只能是文本；数组 content 里的图片抽出来放进
			// 紧随其后的合成 user 消息，附带 call 归属说明。
			if len(images) > 0 {
				mediaParts := []any{map[string]any{
					"type": "input_text",
					"text": fmt.Sprintf(toolResultImageAttribution, m.ToolCallID),
				}}
				for _, url := range images {
					mediaParts = append(mediaParts, map[string]any{"type": "input_image", "image_url": url})
				}
				input = append(input, map[string]any{
					"type":    "message",
					"role":    "user",
					"content": mediaParts,
				})
			}

		case "assistant":
			if len(m.ToolCalls) > 0 {
				// 有 tool_calls 的 assistant 消息
				if text := rawMessageToString(m.Content); text != "" {
					input = append(input, map[string]any{
						"type": "message",
						"role": "assistant",
						"content": []any{
							map[string]any{"type": "output_text", "text": text},
						},
					})
				}
				for _, tc := range m.ToolCalls {
					input = append(input, map[string]any{
						"type":      "function_call",
						"call_id":   tc.ID,
						"name":      tc.Function.Name,
						"arguments": tc.Function.Arguments,
					})
				}
			} else {
				input = append(input, map[string]any{
					"type":    "message",
					"role":    "assistant",
					"content": buildContentPartsSlice("assistant", m.Content),
				})
			}

		case "system":
			input = append(input, map[string]any{
				"type":    "message",
				"role":    "developer",
				"content": buildContentPartsSlice("system", m.Content),
			})

		default:
			input = append(input, map[string]any{
				"type":    "message",
				"role":    "user",
				"content": buildContentPartsSlice("user", m.Content),
			})
		}
	}
	return input
}

// buildContentPartsSlice 将 content（string 或 []contentPart）转为 []any
func buildContentPartsSlice(role string, raw json.RawMessage) []any {
	parts := make([]any, 0)
	if len(raw) == 0 {
		return parts
	}

	contentType := "input_text"
	if role == "assistant" {
		contentType = "output_text"
	}

	first := firstNonSpace(raw)
	switch first {
	case '"':
		var s string
		if json.Unmarshal(raw, &s) != nil || s == "" {
			return parts
		}
		return append(parts, map[string]any{"type": contentType, "text": s})
	case '[':
		var arr []openAIContentPart
		if json.Unmarshal(raw, &arr) != nil {
			return parts
		}
		for _, item := range arr {
			switch item.Type {
			case "text":
				parts = append(parts, map[string]any{"type": contentType, "text": item.Text})
			case "image_url":
				if item.ImageURL != nil && item.ImageURL.URL != "" {
					parts = append(parts, map[string]any{"type": "input_image", "image_url": item.ImageURL.URL})
				}
			case "file":
				if item.File != nil && (strings.TrimSpace(item.File.FileID) != "" || strings.TrimSpace(item.File.FileData) != "") {
					part := map[string]any{"type": "input_file"}
					if filename := strings.TrimSpace(item.File.Filename); filename != "" {
						part["filename"] = filename
					}
					if fileData := strings.TrimSpace(item.File.FileData); fileData != "" {
						part["file_data"] = fileData
					}
					if fileID := strings.TrimSpace(item.File.FileID); fileID != "" {
						part["file_id"] = fileID
					}
					parts = append(parts, part)
				}
			}
		}
		return parts
	default:
		return parts
	}
}

// toolMessageOutputAndImages 计算 tool 消息的 function_call_output 文本，并抽出
// 数组 content 里的图片 URL。无图片时 output 与 rawMessageToString 完全一致（含数组
// 原始字节），保证无图请求的 prompt-cache 前缀不变。
func toolMessageOutputAndImages(raw json.RawMessage) (string, []string) {
	if firstNonSpace(raw) != '[' {
		return rawMessageToString(raw), nil
	}
	var arr []openAIContentPart
	if json.Unmarshal(raw, &arr) != nil {
		return rawMessageToString(raw), nil
	}
	var images []string
	for _, item := range arr {
		if item.Type == "image_url" && item.ImageURL != nil && item.ImageURL.URL != "" {
			images = append(images, item.ImageURL.URL)
		}
	}
	if len(images) == 0 {
		// 无图片：保持原字节路径，不改写。
		return rawMessageToString(raw), nil
	}
	var textParts []string
	for _, item := range arr {
		if item.Type == "text" && item.Text != "" {
			textParts = append(textParts, item.Text)
		}
	}
	output := strings.Join(textParts, "\n")
	if output == "" {
		output = toolResultImageMovedMarker
	}
	return output, images
}

// rawMessageToString 安全地将 json.RawMessage 转为 Go string
func rawMessageToString(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return string(raw)
}

func firstNonSpace(raw json.RawMessage) byte {
	for _, b := range raw {
		if b != ' ' && b != '\n' && b != '\r' && b != '\t' {
			return b
		}
	}
	return 0
}

// convertToolsToCodexFormat 将 OpenAI 工具格式转为 Codex 格式（纯内存操作）
// OpenAI: {type:"function", function:{name, description, parameters}}
// Codex:  {type:"function", name, description, parameters}
// 上游限制最多 128 个工具，超出部分静默截断
func convertToolsToCodexFormat(rawTools []json.RawMessage) []any {
	cap := len(rawTools)
	if cap > maxTools {
		cap = maxTools
		rawTools = rawTools[:maxTools]
	}
	tools := make([]any, 0, cap)
	for _, raw := range rawTools {
		var parsed openAIToolParsed
		if json.Unmarshal(raw, &parsed) != nil {
			continue
		}

		// type 缺失或为 null 时按 OpenAI SDK 惯例视为 function（前提是带
		// function 对象）；上游对空 type 一律返回 400 "Unsupported tool
		// type: None"（issue #219），无法识别形态的工具直接丢弃。
		isFunction := parsed.Function != nil &&
			(parsed.Type == "function" || parsed.Type == "")
		if !isFunction {
			if parsed.Type == "" {
				// 无 function 对象但有顶层 name（Codex 格式缺 type）→ 补全
				// type 后保留；其余直接丢弃。
				var toolMap map[string]any
				if json.Unmarshal(raw, &toolMap) == nil &&
					strings.TrimSpace(firstNonEmptyAnyString(toolMap["name"])) != "" {
					toolMap["type"] = "function"
					normalizeFunctionToolParameters(toolMap)
					tools = append(tools, toolMap)
				}
				continue
			}
			// 非 function 类型 → 透传原始 JSON
			// 例外：把 web_search_preview 等变体归一为 web_search，
			// Codex 上游只认裸 "web_search"。归一时保留白名单字段，
			// 与 PrepareResponsesBody 路径行为一致。
			if strings.HasPrefix(parsed.Type, "web_search") {
				var toolMap map[string]any
				if json.Unmarshal(raw, &toolMap) == nil && toolMap != nil {
					tools = append(tools, normalizeCodexWebSearchTool(toolMap))
				} else {
					tools = append(tools, map[string]any{"type": "web_search"})
				}
				continue
			}
			var passThrough any
			_ = json.Unmarshal(raw, &passThrough)
			tools = append(tools, passThrough)
			continue
		}

		// function 类型 → 提升 function 内字段到顶层
		item := map[string]any{
			"type": "function",
			"name": parsed.Function.Name,
		}
		if parsed.Function.Description != "" {
			item["description"] = parsed.Function.Description
		}
		if len(parsed.Function.Parameters) > 0 {
			var params map[string]any
			if json.Unmarshal(parsed.Function.Parameters, &params) == nil && params != nil {
				item["parameters"] = params
			}
		}
		normalizeFunctionToolParameters(item)
		if parsed.Function.Strict != nil {
			item["strict"] = *parsed.Function.Strict
		}
		tools = append(tools, item)
	}
	return tools
}

// ==================== 向后兼容: 辅助函数 ====================

func normalizeServiceTierField(body []byte) []byte {
	tier := strings.TrimSpace(gjson.GetBytes(body, "service_tier").String())
	if tier == "" {
		tier = strings.TrimSpace(gjson.GetBytes(body, "serviceTier").String())
	}
	if tier == "" {
		return body
	}
	body, _ = sjson.SetBytes(body, "service_tier", tier)
	body, _ = sjson.DeleteBytes(body, "serviceTier")
	return body
}

func sanitizeServiceTierForUpstream(body []byte) []byte {
	tier := strings.TrimSpace(gjson.GetBytes(body, "service_tier").String())
	if tier == "" {
		body, _ = sjson.DeleteBytes(body, "serviceTier")
		return body
	}
	switch tier {
	case "auto", "default", "flex", "priority", "scale", "fast":
		body, _ = sjson.DeleteBytes(body, "serviceTier")
		if upstreamTier, ok := upstreamServiceTier(tier); ok {
			body, _ = sjson.SetBytes(body, "service_tier", upstreamTier)
		} else {
			body, _ = sjson.DeleteBytes(body, "service_tier")
		}
		return body
	default:
		body, _ = sjson.DeleteBytes(body, "service_tier")
		body, _ = sjson.DeleteBytes(body, "serviceTier")
		return body
	}
}

type usageServiceTiers struct {
	ServiceTier          string
	RequestedServiceTier string
	ActualServiceTier    string
	BillingServiceTier   string
}

func normalizeServiceTierValue(tier string) string {
	return strings.ToLower(strings.TrimSpace(tier))
}

func normalizeDisplayServiceTier(tier string) string {
	tier = normalizeServiceTierValue(tier)
	if tier == "priority" {
		return "fast"
	}
	return tier
}

func normalizeBillingServiceTier(tier string) string {
	tier = normalizeServiceTierValue(tier)
	if tier == "priority" || tier == "fast" {
		return "priority"
	}
	return tier
}

// resolveServiceTier is the legacy service_tier field for usage logs.
// It now prefers the upstream actual tier so response.service_tier="default"
// is not masked by a requested fast/priority intent.
func resolveServiceTier(actualTier, requestedTier string) string {
	if actual := normalizeDisplayServiceTier(actualTier); actual != "" {
		return actual
	}
	return normalizeDisplayServiceTier(requestedTier)
}

func resolveUsageServiceTiers(actualTier, requestedTier string) usageServiceTiers {
	return usageServiceTiers{
		ServiceTier:          resolveServiceTier(actualTier, requestedTier),
		RequestedServiceTier: normalizeServiceTierValue(requestedTier),
		ActualServiceTier:    normalizeServiceTierValue(actualTier),
		BillingServiceTier:   resolveBillingServiceTier(actualTier, requestedTier),
	}
}

// resolveBillingServiceTier selects the tier used for money. The default policy
// treats the requested tier as a ceiling: an upstream observation may lower the
// bill, but it must never raise it. requested policy ignores observations and
// preserves the explicit client intent.
func resolveBillingServiceTier(actualTier, requestedTier string) string {
	return resolveBillingServiceTierForPolicy(actualTier, requestedTier, CurrentRuntimeSettings().BillingTierPolicy)
}

func resolveBillingServiceTierForPolicy(actualTier, requestedTier, policy string) string {
	actualTier = normalizeBillingServiceTier(actualTier)
	requestedTier = normalizeBillingServiceTier(requestedTier)

	if NormalizeBillingTierPolicy(policy) == BillingTierPolicyRequested {
		return requestedTier
	}

	if actualTier == "" || actualTier == requestedTier {
		return requestedTier
	}
	actualRank, actualKnown := billingServiceTierCostRank(actualTier)
	requestedRank, requestedKnown := billingServiceTierCostRank(requestedTier)
	if !actualKnown || !requestedKnown || actualRank >= requestedRank {
		return requestedTier
	}
	return actualTier
}

// billingServiceTierCostRank orders known tiers by relative price. Empty is the
// ordinary base tier: an unsolicited priority observation therefore cannot turn
// an untiered request into a Fast charge. Unknown tiers are never trusted to
// change billing in either direction.
func billingServiceTierCostRank(tier string) (int, bool) {
	switch normalizeBillingServiceTier(tier) {
	case "flex":
		return 0, true
	case "", "default", "standard", "auto", "scale":
		return 1, true
	case "priority":
		return 2, true
	default:
		return 1, false
	}
}

// 上游不支持的 JSON Schema 验证约束关键字
var unsupportedSchemaKeys = map[string]bool{
	"uniqueItems":      true,
	"minItems":         true,
	"maxItems":         true,
	"minimum":          true,
	"maximum":          true,
	"exclusiveMinimum": true,
	"exclusiveMaximum": true,
	"multipleOf":       true,
	"pattern":          true,
	"minLength":        true,
	"maxLength":        true,
	"format":           true,
	"minProperties":    true,
	"maxProperties":    true,
}

// 值为「名称 → 子 schema」映射的关键字。`definitions` 是 draft-07 的写法，
// pydantic v1 和旧版 zod-to-json-schema 都生成它而不是 `$defs`。
var schemaSubSchemaMapKeys = []string{
	"properties",
	"patternProperties",
	"dependentSchemas",
	"$defs",
	"definitions",
}

// 值为子 schema 数组的关键字。`prefixItems` 是 draft 2020-12 的 tuple 写法。
var schemaSubSchemaListKeys = []string{"allOf", "anyOf", "oneOf", "prefixItems"}

// 值为单个子 schema 的关键字。
var schemaSubSchemaKeys = []string{
	"additionalProperties",
	"not",
	"if",
	"then",
	"else",
	"propertyNames",
	"contains",
}

// forEachSubSchema 遍历 schema 的全部直接子 schema。所有递归清洗函数都必须走
// 这里，而不是各自复制一份下钻逻辑——否则新增一种 schema 关键字要改多处，漏一处
// 就静默放行坏 schema（`definitions` 与 tuple 形态的 `items` 就是这么漏的）。
func forEachSubSchema(schema map[string]interface{}, visit func(map[string]interface{})) {
	for _, key := range schemaSubSchemaMapKeys {
		entries, ok := schema[key].(map[string]interface{})
		if !ok {
			continue
		}
		for _, v := range entries {
			if sub, ok := v.(map[string]interface{}); ok {
				visit(sub)
			}
		}
	}
	for _, key := range schemaSubSchemaListKeys {
		arr, ok := schema[key].([]interface{})
		if !ok {
			continue
		}
		for _, item := range arr {
			if sub, ok := item.(map[string]interface{}); ok {
				visit(sub)
			}
		}
	}
	for _, key := range schemaSubSchemaKeys {
		if sub, ok := schema[key].(map[string]interface{}); ok {
			visit(sub)
		}
	}
	// items 有两种合法形态：单个 schema，或 draft-07 tuple 校验的 schema 数组。
	switch items := schema["items"].(type) {
	case map[string]interface{}:
		visit(items)
	case []interface{}:
		for _, item := range items {
			if sub, ok := item.(map[string]interface{}); ok {
				visit(sub)
			}
		}
	}
}

// stripUnsupportedSchemaKeys 递归删除 schema 中上游不支持的关键字
func stripUnsupportedSchemaKeys(schema map[string]interface{}) {
	for key := range unsupportedSchemaKeys {
		delete(schema, key)
	}
	// 显式写成 null 的 type 上游一律拒收（Codex Desktop 的 automation_update
	// 会这么发）。删掉即退回「未声明类型」的合法 schema；function 工具的根节点
	// 随后还会被 ensureFunctionParametersRootObject 补成 object。坏 schema 一旦
	// 沉进多轮历史，不修就每轮必 400。
	if rawType, exists := schema["type"]; exists && rawType == nil {
		delete(schema, "type")
	}
	forEachSubSchema(schema, stripUnsupportedSchemaKeys)
}

func sanitizeSchemaForUpstream(schema map[string]interface{}) {
	stripUnsupportedSchemaKeys(schema)
	normalizeSchemaRequiredFields(schema)
	ensureArrayItems(schema)
}

func sanitizeStructuredOutputSchemaForUpstream(schema map[string]interface{}) {
	sanitizeSchemaForUpstream(schema)
	ensureObjectAdditionalPropertiesFalse(schema)
	alignRequiredWithProperties(schema)
}

func normalizeResponsesStructuredOutputFormat(body map[string]any) bool {
	if len(body) == 0 {
		return false
	}

	modified := false
	if responseFormat, ok := body["response_format"].(map[string]any); ok {
		if textFormat := responsesTextFormatFromResponseFormat(responseFormat); textFormat != nil {
			text, _ := body["text"].(map[string]any)
			if text == nil {
				text = map[string]any{}
				body["text"] = text
			}
			if _, hasFormat := text["format"]; !hasFormat {
				text["format"] = textFormat
				modified = true
			}
		}
		if sanitizeStructuredOutputSchema(responseFormat) {
			modified = true
		}
	}

	text, ok := body["text"].(map[string]any)
	if !ok {
		return modified
	}
	format, ok := text["format"].(map[string]any)
	if !ok {
		return modified
	}
	if sanitizeStructuredOutputSchema(format) {
		modified = true
	}
	if ensureJSONModeInputMentionsJSON(body, format) {
		modified = true
	}
	return modified
}

func ensureJSONModeInputMentionsJSON(body map[string]any, format map[string]any) bool {
	if strings.TrimSpace(firstNonEmptyAnyString(format["type"])) != "json_object" {
		return false
	}
	input, ok := body["input"]
	if !ok || responsesInputContainsJSON(input) {
		return false
	}

	switch inputValue := input.(type) {
	case string:
		body["input"] = jsonObjectFormatInputHint + "\n\n" + inputValue
		return true
	case []any:
		body["input"] = append([]any{jsonObjectDeveloperMessage()}, inputValue...)
		return true
	case []map[string]string:
		inputItems := make([]any, 0, len(inputValue)+1)
		inputItems = append(inputItems, jsonObjectDeveloperMessage())
		for _, item := range inputValue {
			inputItems = append(inputItems, item)
		}
		body["input"] = inputItems
		return true
	case []map[string]any:
		inputItems := make([]any, 0, len(inputValue)+1)
		inputItems = append(inputItems, jsonObjectDeveloperMessage())
		for _, item := range inputValue {
			inputItems = append(inputItems, item)
		}
		body["input"] = inputItems
		return true
	default:
		return false
	}
}

func jsonObjectDeveloperMessage() map[string]any {
	return map[string]any{
		"type": "message",
		"role": "developer",
		"content": []any{
			map[string]any{"type": "input_text", "text": jsonObjectFormatInputHint},
		},
	}
}

func responsesInputContainsJSON(value any) bool {
	switch v := value.(type) {
	case string:
		return strings.Contains(strings.ToLower(v), "json")
	case []any:
		for _, item := range v {
			if responsesInputContainsJSON(item) {
				return true
			}
		}
	case []map[string]string:
		for _, item := range v {
			if responsesInputContainsJSON(item) {
				return true
			}
		}
	case []map[string]any:
		for _, item := range v {
			if responsesInputContainsJSON(item) {
				return true
			}
		}
	case map[string]any:
		for _, key := range []string{"content", "text", "output"} {
			if child, ok := v[key]; ok && responsesInputContainsJSON(child) {
				return true
			}
		}
	case map[string]string:
		for _, key := range []string{"content", "text", "output"} {
			if child, ok := v[key]; ok && responsesInputContainsJSON(child) {
				return true
			}
		}
	}
	return false
}

func responsesTextFormatFromResponseFormat(responseFormat map[string]any) map[string]any {
	formatType := strings.TrimSpace(firstNonEmptyAnyString(responseFormat["type"]))
	switch formatType {
	case "json_schema":
		if jsonSchema, ok := responseFormat["json_schema"].(map[string]any); ok && jsonSchema != nil {
			out := make(map[string]any, len(jsonSchema)+1)
			for key, value := range jsonSchema {
				out[key] = value
			}
			out["type"] = "json_schema"
			return out
		}
		out := make(map[string]any, len(responseFormat))
		for key, value := range responseFormat {
			if key == "json_schema" {
				continue
			}
			out[key] = value
		}
		out["type"] = "json_schema"
		return out
	case "json_object", "text":
		return map[string]any{"type": formatType}
	default:
		return nil
	}
}

func sanitizeStructuredOutputSchema(format map[string]any) bool {
	modified := false
	if schema, ok := format["schema"].(map[string]any); ok && schema != nil {
		sanitizeStructuredOutputSchemaForUpstream(schema)
		modified = true
	}
	if jsonSchema, ok := format["json_schema"].(map[string]any); ok && jsonSchema != nil {
		if schema, ok := jsonSchema["schema"].(map[string]any); ok && schema != nil {
			sanitizeStructuredOutputSchemaForUpstream(schema)
			modified = true
		}
	}
	return modified
}

func isFunctionTool(tool map[string]any) bool {
	toolType, _ := tool["type"].(string)
	return strings.TrimSpace(toolType) == "function"
}

// reservedCodexToolNamePrefixes 列出上游为模型保留的工具命名空间。这类工具
// （如 gpt-5.6 multi-agent v2 的 collaboration.spawn_agent）要求 schema 与上游
// 官方配置逐字匹配，代理的通用 schema 清洗（stripUnsupportedSchemaKeys 等）会破坏
// 匹配，导致上游 400 "reserved for use by this model and must match the configured
// schema"（issue #342）。这类工具必须原样透传，不补描述、不清洗、不摊平。
var reservedCodexToolNamePrefixes = []string{"collaboration."}

// isReservedCodexTool 判断工具名是否落在上游保留命名空间。
// 兼容 Responses 扁平形态（顶层 name）与 Chat Completions 嵌套形态（function.name）。
func isReservedCodexTool(tool map[string]any) bool {
	if tool == nil {
		return false
	}
	name := strings.TrimSpace(firstNonEmptyAnyString(tool["name"]))
	if name == "" {
		if fn, ok := tool["function"].(map[string]any); ok {
			name = strings.TrimSpace(firstNonEmptyAnyString(fn["name"]))
		}
	}
	if name == "" {
		return false
	}
	lower := strings.ToLower(name)
	for _, prefix := range reservedCodexToolNamePrefixes {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	return false
}

// normalizeResponsesToolSchema 清洗单个工具声明：补默认描述，递归清理 JSON Schema
// 里上游不支持的关键字，并修正 function 工具要求的根结构。保留工具
// （collaboration.* 等）原样透传——上游要求其 schema 逐字匹配官方配置，
// 任何补描述/清洗都会破坏匹配并被拒（issue #342）。
func normalizeResponsesToolSchema(rawTool any, descDefaults map[string]string) {
	toolMap, ok := rawTool.(map[string]any)
	if !ok || isReservedCodexTool(toolMap) {
		return
	}
	if toolType, _ := toolMap["type"].(string); toolType != "" {
		if defaultDesc, ok := descDefaults[toolType]; ok {
			if desc, _ := toolMap["description"].(string); desc == "" {
				toolMap["description"] = defaultDesc
			}
		}
	}
	if isFunctionTool(toolMap) {
		normalizeFunctionToolParameters(toolMap)
	} else if params, ok := toolMap["parameters"].(map[string]any); ok {
		sanitizeSchemaForUpstream(params)
	}
}

// normalizeResponsesAdditionalToolSchemas 对 input[] 里 type=additional_tools
// 载体项内嵌的工具列表套用与顶层 tools[] 相同的清洗（Responses Lite 格式）。
func normalizeResponsesAdditionalToolSchemas(body map[string]any) {
	input, ok := body["input"].([]any)
	if !ok {
		return
	}
	for _, rawItem := range input {
		item, ok := rawItem.(map[string]any)
		if !ok {
			continue
		}
		if strings.TrimSpace(firstNonEmptyAnyString(item["type"])) != "additional_tools" {
			continue
		}
		tools, ok := item["tools"].([]any)
		if !ok {
			continue
		}
		tools, _ = normalizeFunctionToolsInArray(tools)
		item["tools"] = tools
		for _, rawTool := range tools {
			normalizeResponsesToolSchema(rawTool, nil)
		}
	}
}

func normalizeFunctionToolParameters(tool map[string]any) {
	params, ok := tool["parameters"].(map[string]any)
	if !ok || params == nil {
		tool["parameters"] = defaultFunctionParametersSchema()
		return
	}
	sanitizeSchemaForUpstream(params)
	ensureFunctionParametersRootObject(params)
}

func defaultFunctionParametersSchema() map[string]any {
	return map[string]any{
		"type":       "object",
		"properties": map[string]any{},
	}
}

func ensureFunctionParametersRootObject(schema map[string]any) {
	if schemaType, ok := schema["type"].(string); !ok || strings.TrimSpace(schemaType) != "object" {
		schema["type"] = "object"
	}
	if props, ok := schema["properties"].(map[string]any); !ok || props == nil {
		schema["properties"] = map[string]any{}
	}
}

func normalizeSchemaRequiredFields(schema map[string]interface{}) {
	if rawRequired, exists := schema["required"]; exists {
		required, ok := rawRequired.([]interface{})
		if !ok {
			delete(schema, "required")
		} else {
			cleaned := make([]interface{}, 0, len(required))
			for _, item := range required {
				if name, ok := item.(string); ok && strings.TrimSpace(name) != "" {
					cleaned = append(cleaned, name)
				}
			}
			if len(cleaned) == 0 {
				delete(schema, "required")
			} else {
				schema["required"] = cleaned
			}
		}
	}
	forEachSubSchema(schema, normalizeSchemaRequiredFields)
}

// ensureArrayItems 递归为缺失 items 的数组 schema 补上空 schema，
// 兼容上游对 array 必须声明 items 的校验。
func ensureArrayItems(schema map[string]interface{}) {
	if schemaDeclaresArray(schema) {
		if _, ok := schema["items"]; !ok {
			schema["items"] = map[string]interface{}{}
		}
	}
	forEachSubSchema(schema, ensureArrayItems)
}

func ensureObjectAdditionalPropertiesFalse(schema map[string]interface{}) {
	if schemaDeclaresObject(schema) {
		schema["additionalProperties"] = false
	}
	forEachSubSchema(schema, ensureObjectAdditionalPropertiesFalse)
}

// alignRequiredWithProperties 让每个带 properties 的对象节点满足上游严格模式的
// 校验：required 必须恰好等于 properties 的全部 key。多出来的 required 项直接
// 剔除（strict 模式下 additionalProperties=false，声明一个不存在的必填字段只会
// 被上游 400 拒收），缺失的 key 按字典序补齐。
func alignRequiredWithProperties(schema map[string]interface{}) {
	if props, ok := schema["properties"].(map[string]interface{}); ok {
		required := make([]interface{}, 0, len(props))
		seen := make(map[string]bool, len(props))
		if existing, ok := schema["required"].([]interface{}); ok {
			for _, item := range existing {
				name, ok := item.(string)
				if !ok || seen[name] {
					continue
				}
				if _, exists := props[name]; !exists {
					continue
				}
				seen[name] = true
				required = append(required, name)
			}
		}
		missing := make([]string, 0, len(props))
		for name := range props {
			if !seen[name] {
				missing = append(missing, name)
			}
		}
		sort.Strings(missing)
		for _, name := range missing {
			required = append(required, name)
		}
		if len(required) == 0 {
			delete(schema, "required")
		} else {
			schema["required"] = required
		}
	}
	forEachSubSchema(schema, alignRequiredWithProperties)
}

func schemaDeclaresArray(schema map[string]interface{}) bool {
	switch t := schema["type"].(type) {
	case string:
		return t == "array"
	case []interface{}:
		for _, item := range t {
			if s, ok := item.(string); ok && s == "array" {
				return true
			}
		}
	}
	return false
}

func schemaDeclaresObject(schema map[string]interface{}) bool {
	switch t := schema["type"].(type) {
	case string:
		return t == "object"
	case []interface{}:
		for _, item := range t {
			if s, ok := item.(string); ok && s == "object" {
				return true
			}
		}
	}
	return false
}

// ==================== 响应翻译: Codex SSE → OpenAI SSE ====================

// UsageInfo token 使用统计
type TokenDetails struct {
	CachedTokens int `json:"cached_tokens,omitempty"`
}

type UsageInfo struct {
	PromptTokens        int           `json:"prompt_tokens"`
	CompletionTokens    int           `json:"completion_tokens"`
	TotalTokens         int           `json:"total_tokens"`
	InputTokens         int           `json:"input_tokens,omitempty"`
	OutputTokens        int           `json:"output_tokens,omitempty"`
	ReasoningTokens     int           `json:"reasoning_tokens,omitempty"`
	CachedTokens        int           `json:"cached_tokens,omitempty"`
	PromptTokensDetails *TokenDetails `json:"prompt_tokens_details,omitempty"`
	InputTokensDetails  *TokenDetails `json:"input_tokens_details,omitempty"`
}

func newUsageInfo(inputTokens, outputTokens, reasoningTokens, cachedTokens int) *UsageInfo {
	usage := &UsageInfo{
		PromptTokens:     inputTokens,
		CompletionTokens: outputTokens,
		TotalTokens:      inputTokens + outputTokens,
		InputTokens:      inputTokens,
		OutputTokens:     outputTokens,
		ReasoningTokens:  reasoningTokens,
		CachedTokens:     cachedTokens,
	}
	if cachedTokens > 0 {
		details := &TokenDetails{CachedTokens: cachedTokens}
		usage.PromptTokensDetails = details
		usage.InputTokensDetails = details
	}
	return usage
}

// newContentChunk 构建文本内容流式块
func newContentChunk(id, model string, created int64, content string) []byte {
	chunk := openAIStreamChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []streamChoice{{
			Index: 0,
			Delta: &streamDelta{Content: &content},
		}},
	}
	b, _ := json.Marshal(chunk)
	return b
}

// newReasoningChunk 构建推理内容流式块。
// 同时填入 reasoning 与 reasoning_content,兼容 OpenAI/DeepSeek 两套客户端风格。
func newReasoningChunk(id, model string, created int64, reasoning string) []byte {
	chunk := openAIStreamChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []streamChoice{{
			Index: 0,
			Delta: &streamDelta{
				Reasoning:        &reasoning,
				ReasoningContent: &reasoning,
			},
		}},
	}
	b, _ := json.Marshal(chunk)
	return b
}

func isCodexToolCallItemType(itemType string) bool {
	switch itemType {
	case "function_call", "custom_tool_call":
		return true
	default:
		return false
	}
}

func isCodexToolInputDeltaEvent(eventType string) bool {
	switch eventType {
	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		return true
	default:
		return false
	}
}

func isCodexToolInputDoneEvent(eventType string) bool {
	switch eventType {
	case "response.function_call_arguments.done", "response.custom_tool_call_input.done":
		return true
	default:
		return false
	}
}

// newToolCallAnnouncementChunk 构建 tool call 首块（含 id、type、function.name）
func newToolCallAnnouncementChunk(id, model string, created int64, tcIndex int, callID, funcName string) []byte {
	chunk := openAIStreamChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []streamChoice{{
			Index: 0,
			Delta: &streamDelta{
				Role: "assistant",
				ToolCalls: []toolCallDelta{{
					Index: tcIndex,
					ID:    callID,
					Type:  "function",
					Function: toolCallFuncDelta{
						Name:      funcName,
						Arguments: "",
					},
				}},
			},
		}},
	}
	b, _ := json.Marshal(chunk)
	return b
}

// newToolCallDeltaChunk 构建 tool call 参数增量块
func newToolCallDeltaChunk(id, model string, created int64, tcIndex int, argsDelta string) []byte {
	chunk := openAIStreamChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []streamChoice{{
			Index: 0,
			Delta: &streamDelta{
				ToolCalls: []toolCallDelta{{
					Index:    tcIndex,
					Function: toolCallFuncDelta{Arguments: argsDelta},
				}},
			},
		}},
	}
	b, _ := json.Marshal(chunk)
	return b
}

// newFinalChunk 构建最终流式块（含 finish_reason 和可选 usage）。
//
// delta 必须显式带上空对象:OpenAI 官方终结块形如
// {"index":0,"delta":{},"finish_reason":"stop"},严格反序列化的客户端
// (Rust/serde 系)把 delta 当必填字段,缺失会直接报
// "missing field `delta`" 并让整轮对话失败。
func newFinalChunk(id, model string, created int64, finishReason string, usage *UsageInfo) []byte {
	chunk := openAIStreamChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []streamChoice{{
			Index:        0,
			Delta:        &streamDelta{},
			FinishReason: &finishReason,
		}},
		Usage: usage,
	}
	b, _ := json.Marshal(chunk)
	return b
}

// newErrorResponse 构建错误响应
func newErrorResponse(message string, details ...json.RawMessage) []byte {
	resp := openAIErrorResponse{}
	resp.Error.Message = message
	resp.Error.Type = "upstream_error"
	if len(details) > 0 && len(details[0]) > 0 {
		resp.Error.Details = details[0]
	}
	b, _ := json.Marshal(resp)
	return b
}

// ChatStreamTranslation keeps terminal success and terminal failure distinct.
// Both end the upstream Responses stream, but only a successful completion may
// be followed by Chat Completions' [DONE] sentinel. Treating a response.failed
// as the old undifferentiated done=true result makes a failed generation look
// like a cleanly completed Chat stream to downstream clients.
type ChatStreamTranslation struct {
	Chunk    []byte
	Terminal bool
	Failed   bool
}

func newChatStreamTranslation(eventType string, chunk []byte, terminal bool) ChatStreamTranslation {
	return ChatStreamTranslation{
		Chunk:    chunk,
		Terminal: terminal,
		Failed:   terminal && eventType == "response.failed",
	}
}

// TranslateStreamChunkResult is the terminal-aware form of
// TranslateStreamChunk. Existing callers that only need the historical
// (chunk, done) pair can keep using TranslateStreamChunk.
func TranslateStreamChunkResult(eventData []byte, model string, chunkID string, created int64) ChatStreamTranslation {
	eventType := gjson.GetBytes(eventData, "type").String()
	chunk, terminal := TranslateStreamChunk(eventData, model, chunkID, created)
	return newChatStreamTranslation(eventType, chunk, terminal)
}

// TranslateStreamChunk 将 Codex SSE 数据块翻译为 OpenAI Chat Completions 流式格式（无状态）
func TranslateStreamChunk(eventData []byte, model string, chunkID string, created int64) ([]byte, bool) {
	eventType := gjson.GetBytes(eventData, "type").String()

	switch eventType {
	case "response.output_text.delta":
		delta := gjson.GetBytes(eventData, "delta").String()
		return newContentChunk(chunkID, model, created, delta), false

	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		delta := gjson.GetBytes(eventData, "delta").String()
		return newReasoningChunk(chunkID, model, created, delta), false

	case "response.custom_tool_call_input.delta", "response.custom_tool_call_input.done":
		return nil, false

	case "response.completed":
		usage := extractUsage(eventData)
		return newFinalChunk(chunkID, model, created, "stop", usage), true

	// max_output_tokens 截断的正常终态：Chat 侧对应 finish_reason=length。
	case "response.incomplete":
		usage := extractUsage(eventData)
		reason := gjson.GetBytes(eventData, "response.incomplete_details.reason").String()
		return newFinalChunk(chunkID, model, created, responsesIncompleteFinishReason(eventType, reason), usage), true

	case "response.failed":
		errMsg := gjson.GetBytes(eventData, "response.error.message").String()
		if errMsg == "" {
			errMsg = "Codex upstream error"
		}
		var details json.RawMessage
		if raw := gjson.GetBytes(eventData, "response.error.details"); raw.Exists() && raw.IsObject() {
			details = json.RawMessage(raw.Raw)
		}
		return newErrorResponse(errMsg, details), true

	case "response.content_part.done", "response.output_item.done",
		"response.created", "response.in_progress",
		"response.output_item.added", "response.content_part.added",
		"response.reasoning_summary_text.done",
		"response.reasoning.encrypted_content.delta", "response.reasoning.encrypted_content.done",
		"response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		return nil, false

	default:
		if delta := gjson.GetBytes(eventData, "delta"); delta.Exists() && delta.Type == gjson.String {
			return newContentChunk(chunkID, model, created, delta.String()), false
		}
		return nil, false
	}
}

// ==================== 有状态流式转换器（支持 Function Calling） ====================

// ToolCallResult 表示一个完整的工具调用结果
type ToolCallResult struct {
	ID        string
	Name      string
	Arguments string
}

// StreamTranslator 有状态的流式响应翻译器，跟踪 function_call 索引映射
type StreamTranslator struct {
	Model                 string
	ChunkID               string
	Created               int64
	HasToolCalls          bool
	toolCallMap           map[string]int // Codex item.id/call_id → OpenAI tool_calls index
	toolCallOutputIndexes map[int]int    // Responses output_index → OpenAI tool_calls index
	toolCallTypes         map[int]string
	toolCallNames         map[int]string
	toolCallArguments     map[int]string
	toolCallFinalized     map[int]bool
	invalidToolArguments  error
	nextIdx               int
}

// NewStreamTranslator 创建流式翻译器实例
func NewStreamTranslator(chunkID, model string, created int64) *StreamTranslator {
	return &StreamTranslator{
		Model:                 model,
		ChunkID:               chunkID,
		Created:               created,
		toolCallMap:           make(map[string]int),
		toolCallOutputIndexes: make(map[int]int),
		toolCallTypes:         make(map[int]string),
		toolCallNames:         make(map[int]string),
		toolCallArguments:     make(map[int]string),
		toolCallFinalized:     make(map[int]bool),
	}
}

// Translate 将单个 Codex SSE 事件翻译为 OpenAI Chat Completions 流式格式
func (st *StreamTranslator) Translate(eventData []byte) ([]byte, bool) {
	return st.TranslateParsed(gjson.ParseBytes(eventData))
}

// TranslateParsedResult exposes whether a terminal translation represents a
// successful completion or a failure. This lets the HTTP handler terminate a
// failed stream without appending the success-only [DONE] sentinel.
func (st *StreamTranslator) TranslateParsedResult(parsed gjson.Result) ChatStreamTranslation {
	chunk, terminal := st.TranslateParsed(parsed)
	result := newChatStreamTranslation(parsed.Get("type").String(), chunk, terminal)
	if terminal && st.invalidToolArguments != nil {
		result.Failed = true
	}
	return result
}

// ToolArgumentsError reports an upstream protocol failure detected while
// rebuilding a streamed ordinary function call. The handler uses this to make
// the failure retryable before any private buffered attempt is committed.
func (st *StreamTranslator) ToolArgumentsError() error {
	if st == nil {
		return nil
	}
	return st.invalidToolArguments
}

func (st *StreamTranslator) toolCallIndex(parsed gjson.Result) (int, bool) {
	itemID := parsed.Get("item_id").String()
	if itemID == "" {
		itemID = parsed.Get("call_id").String()
	}
	if itemID == "" {
		itemID = parsed.Get("item.id").String()
	}
	if itemID == "" {
		itemID = parsed.Get("item.call_id").String()
	}
	if itemID != "" {
		if idx, ok := st.toolCallMap[itemID]; ok {
			return idx, true
		}
	}
	if outputIndex := parsed.Get("output_index"); outputIndex.Exists() {
		idx, ok := st.toolCallOutputIndexes[int(outputIndex.Int())]
		return idx, ok
	}
	return 0, false
}

func (st *StreamTranslator) failToolArguments(idx int, reason string) ([]byte, bool) {
	if st.invalidToolArguments == nil {
		name := strings.TrimSpace(st.toolCallNames[idx])
		if name == "" {
			name = "unknown"
		}
		st.invalidToolArguments = fmt.Errorf("upstream function call %q arguments %s", name, reason)
	}
	return newErrorResponse(st.invalidToolArguments.Error()), true
}

// finalizeOrdinaryToolArguments reconciles streamed deltas with the canonical
// arguments carried by the done item. A canonical suffix may be emitted when a
// provider omitted the final delta; divergent or invalid JSON fails the attempt
// instead of producing a completed poisoned tool call.
func (st *StreamTranslator) finalizeOrdinaryToolArguments(idx int, finalArguments string) ([]byte, bool) {
	current := st.toolCallArguments[idx]
	candidate := finalArguments
	if strings.TrimSpace(candidate) == "" && current != "" {
		candidate = current
	}
	normalized, valid := normalizeOrdinaryFunctionCallArguments(candidate)
	if !valid {
		return st.failToolArguments(idx, "contain invalid JSON")
	}
	if current == normalized {
		st.toolCallFinalized[idx] = true
		return nil, false
	}
	if !strings.HasPrefix(normalized, current) {
		return st.failToolArguments(idx, "do not match the streamed deltas")
	}
	st.toolCallArguments[idx] = normalized
	st.toolCallFinalized[idx] = true
	return newToolCallDeltaChunk(st.ChunkID, st.Model, st.Created, idx, normalized[len(current):]), false
}

func (st *StreamTranslator) validateTerminalFunctionCalls(parsed gjson.Result) {
	if st.invalidToolArguments != nil {
		return
	}
	output := parsed.Get("response.output")
	if output.IsArray() {
		output.ForEach(func(_, item gjson.Result) bool {
			if item.Get("type").String() != "function_call" {
				return true
			}
			if _, valid := normalizeOrdinaryFunctionCallArguments(item.Get("arguments").String()); !valid {
				name := strings.TrimSpace(item.Get("name").String())
				if name == "" {
					name = "unknown"
				}
				st.invalidToolArguments = fmt.Errorf("upstream function call %q arguments contain invalid JSON", name)
				return false
			}
			return true
		})
	}
	if st.invalidToolArguments != nil {
		return
	}
	for idx, itemType := range st.toolCallTypes {
		if itemType != "function_call" || st.toolCallFinalized[idx] {
			continue
		}
		if _, valid := normalizeOrdinaryFunctionCallArguments(st.toolCallArguments[idx]); !valid {
			_, _ = st.failToolArguments(idx, "contain invalid JSON")
			return
		}
	}
}

// TranslateParsed 将已解析的 Codex SSE 事件翻译为 OpenAI Chat Completions 流式格式。
func (st *StreamTranslator) TranslateParsed(parsed gjson.Result) ([]byte, bool) {
	eventType := parsed.Get("type").String()

	switch eventType {
	case "response.output_text.delta":
		delta := parsed.Get("delta").String()
		return newContentChunk(st.ChunkID, st.Model, st.Created, delta), false

	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		delta := parsed.Get("delta").String()
		return newReasoningChunk(st.ChunkID, st.Model, st.Created, delta), false

	case "response.output_item.added":
		itemType := parsed.Get("item.type").String()
		if !isCodexToolCallItemType(itemType) {
			return nil, false
		}
		callID := parsed.Get("item.call_id").String()
		if callID == "" {
			callID = parsed.Get("item.id").String()
		}
		name := parsed.Get("item.name").String()
		itemID := parsed.Get("item.id").String()
		if itemID == "" {
			itemID = callID
		}

		tcIdx := st.nextIdx
		st.toolCallMap[itemID] = tcIdx
		if callID != "" && callID != itemID {
			st.toolCallMap[callID] = tcIdx
		}
		if outputIndex := parsed.Get("output_index"); outputIndex.Exists() {
			st.toolCallOutputIndexes[int(outputIndex.Int())] = tcIdx
		}
		st.nextIdx++
		st.HasToolCalls = true
		st.toolCallTypes[tcIdx] = itemType
		st.toolCallNames[tcIdx] = name

		return newToolCallAnnouncementChunk(st.ChunkID, st.Model, st.Created, tcIdx, callID, name), false

	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta":
		tcIdx, ok := st.toolCallIndex(parsed)
		if !ok {
			return nil, false
		}
		delta := parsed.Get("delta").String()
		if eventType == "response.function_call_arguments.delta" && st.toolCallTypes[tcIdx] == "function_call" {
			if st.toolCallFinalized[tcIdx] {
				return st.failToolArguments(tcIdx, "continued after the done event")
			}
			st.toolCallArguments[tcIdx] += delta
		}
		return newToolCallDeltaChunk(st.ChunkID, st.Model, st.Created, tcIdx, delta), false

	case "response.function_call_arguments.done":
		tcIdx, ok := st.toolCallIndex(parsed)
		if !ok || st.toolCallTypes[tcIdx] != "function_call" {
			return nil, false
		}
		return st.finalizeOrdinaryToolArguments(tcIdx, parsed.Get("arguments").String())

	case "response.custom_tool_call_input.done":
		return nil, false

	case "response.output_item.done":
		if parsed.Get("item.type").String() != "function_call" {
			return nil, false
		}
		tcIdx, ok := st.toolCallIndex(parsed)
		if !ok {
			return nil, false
		}
		return st.finalizeOrdinaryToolArguments(tcIdx, parsed.Get("item.arguments").String())

	case "response.completed", "response.incomplete":
		st.validateTerminalFunctionCalls(parsed)
		if st.invalidToolArguments != nil {
			return newErrorResponse(st.invalidToolArguments.Error()), true
		}
		usage := extractUsageFromResult(parsed.Get("response.usage"))
		finishReason := "stop"
		if st.HasToolCalls {
			finishReason = "tool_calls"
		}
		// 截断态覆盖推导值：stop / tool_calls 会把半截输出说成正常收尾。
		if override := responsesIncompleteFinishReason(eventType, parsed.Get("response.incomplete_details.reason").String()); override != "" {
			finishReason = override
		}
		return newFinalChunk(st.ChunkID, st.Model, st.Created, finishReason, usage), true

	case "response.failed":
		errMsg := parsed.Get("response.error.message").String()
		if errMsg == "" {
			errMsg = "Codex upstream error"
		}
		var details json.RawMessage
		if raw := parsed.Get("response.error.details"); raw.Exists() && raw.IsObject() {
			details = json.RawMessage(raw.Raw)
		}
		return newErrorResponse(errMsg, details), true

	case "response.content_part.done",
		"response.created", "response.in_progress",
		"response.content_part.added",
		"response.reasoning_summary_text.done",
		"response.reasoning.encrypted_content.delta", "response.reasoning.encrypted_content.done",
		"response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		return nil, false

	default:
		if delta := parsed.Get("delta"); delta.Exists() && delta.Type == gjson.String {
			return newContentChunk(st.ChunkID, st.Model, st.Created, delta.String()), false
		}
		return nil, false
	}
}

// ==================== 非流式响应翻译 ====================

// TranslateCompactResponse 将 Codex 非流式响应转换为 OpenAI 格式
func TranslateCompactResponse(responseData []byte, model string, id string) []byte {
	var outputText, reasoningText string
	output := gjson.GetBytes(responseData, "output")
	if output.IsArray() {
		output.ForEach(func(_, item gjson.Result) bool {
			switch item.Get("type").String() {
			case "message":
				content := item.Get("content")
				if content.IsArray() {
					content.ForEach(func(_, part gjson.Result) bool {
						if part.Get("type").String() == "output_text" {
							outputText += part.Get("text").String()
						}
						return true
					})
				}
			case "reasoning":
				// Codex 在 response.output 里把思考过程作为 reasoning item,
				// content/summary 数组下每个元素是 {type, text} 形式。
				summary := item.Get("summary")
				if summary.IsArray() {
					summary.ForEach(func(_, part gjson.Result) bool {
						reasoningText += part.Get("text").String()
						return true
					})
				}
				content := item.Get("content")
				if content.IsArray() {
					content.ForEach(func(_, part gjson.Result) bool {
						reasoningText += part.Get("text").String()
						return true
					})
				}
			}
			return true
		})
	}

	usage := extractUsage(responseData)

	msg := compactMessage{
		Role:    "assistant",
		Content: &outputText,
	}
	if reasoningText != "" {
		r := reasoningText
		msg.Reasoning = &r
		msg.ReasoningContent = &r
	}

	resp := openAICompactResponse{
		ID:     id,
		Object: "chat.completion",
		Model:  model,
		Choices: []compactChoice{{
			Index:        0,
			Message:      msg,
			FinishReason: "stop",
		}},
		Usage: usage,
	}
	b, _ := json.Marshal(resp)
	return b
}

// BuildCompactResponse 构建非流式完整响应（供 handler.go 调用，替代内联 sjson）
// 当有 toolCalls 且 content 为空时，content 输出为 JSON null
// reasoning 为思考过程拼接文本,空字符串时 reasoning / reasoning_content 字段被省略。
func BuildCompactResponse(id, model string, created int64, content, reasoning string, toolCalls []ToolCallResult, usage *UsageInfo) []byte {
	return BuildCompactResponseWithFinishReason(id, model, created, content, reasoning, toolCalls, usage, "")
}

// BuildCompactResponseWithFinishReason 同上，额外允许调用方覆盖 finish_reason。
// 上游按 max_output_tokens 截断时终态是 response.incomplete，推导值 stop /
// tool_calls 会把截断响应说成正常收尾，需要覆盖成 length。空串表示不覆盖。
func BuildCompactResponseWithFinishReason(id, model string, created int64, content, reasoning string, toolCalls []ToolCallResult, usage *UsageInfo, finishReasonOverride string) []byte {
	finishReason := "stop"
	msg := compactMessage{
		Role:    "assistant",
		Content: &content,
	}
	if reasoning != "" {
		r := reasoning
		msg.Reasoning = &r
		msg.ReasoningContent = &r
	}

	if len(toolCalls) > 0 {
		finishReason = "tool_calls"
		if content == "" {
			msg.Content = nil // JSON null
		}
		msg.ToolCalls = make([]compactToolCallOut, len(toolCalls))
		for i, tc := range toolCalls {
			msg.ToolCalls[i] = compactToolCallOut{
				ID:   tc.ID,
				Type: "function",
			}
			msg.ToolCalls[i].Function.Name = tc.Name
			msg.ToolCalls[i].Function.Arguments = tc.Arguments
		}
	}
	if finishReasonOverride != "" {
		finishReason = finishReasonOverride
	}

	resp := openAICompactResponse{
		ID:      id,
		Object:  "chat.completion",
		Created: created,
		Model:   model,
		Choices: []compactChoice{{
			Index:        0,
			Message:      msg,
			FinishReason: finishReason,
		}},
		Usage: usage,
	}
	b, _ := json.Marshal(resp)
	return b
}

// ==================== 公共工具函数 ====================

// extractUsage 从 response.completed 事件提取 usage
func extractUsage(eventData []byte) *UsageInfo {
	return extractUsageFromResult(gjson.GetBytes(eventData, "response.usage"))
}

// extractUsageFromResult 从已解析的 gjson.Result 提取 usage（避免重复解析）
func extractUsageFromResult(usage gjson.Result) *UsageInfo {
	if !usage.Exists() {
		return nil
	}
	inputTokens := int(usage.Get("input_tokens").Int())
	outputTokens := int(usage.Get("output_tokens").Int())
	reasoningTokens := int(usage.Get("output_tokens_details.reasoning_tokens").Int())
	cachedTokens := int(usage.Get("input_tokens_details.cached_tokens").Int())
	return newUsageInfo(inputTokens, outputTokens, reasoningTokens, cachedTokens)
}

// ExtractToolCallsFromOutputValidated extracts completed tool calls and rejects
// malformed ordinary function arguments. Custom tool input remains free-form.
func ExtractToolCallsFromOutputValidated(eventData []byte) ([]ToolCallResult, error) {
	var toolCalls []ToolCallResult
	output := gjson.GetBytes(eventData, "response.output")
	if !output.IsArray() {
		return nil, nil
	}
	var validationErr error
	output.ForEach(func(_, item gjson.Result) bool {
		itemType := item.Get("type").String()
		if isCodexToolCallItemType(itemType) {
			callID := item.Get("call_id").String()
			if callID == "" {
				callID = item.Get("id").String()
			}
			arguments := item.Get("arguments").String()
			if itemType == "custom_tool_call" {
				arguments = item.Get("input").String()
			} else {
				normalized, valid := normalizeOrdinaryFunctionCallArguments(arguments)
				if !valid {
					name := strings.TrimSpace(item.Get("name").String())
					if name == "" {
						name = "unknown"
					}
					validationErr = fmt.Errorf("upstream function call %q arguments contain invalid JSON", name)
					return false
				}
				arguments = normalized
			}
			toolCalls = append(toolCalls, ToolCallResult{
				ID:        callID,
				Name:      item.Get("name").String(),
				Arguments: arguments,
			})
		}
		return true
	})
	if validationErr != nil {
		return nil, validationErr
	}
	return toolCalls, nil
}

// ExtractToolCallsFromOutput preserves the historical convenience API. New
// response paths should use ExtractToolCallsFromOutputValidated so malformed
// upstream output becomes an explicit failed attempt rather than disappearing.
func ExtractToolCallsFromOutput(eventData []byte) []ToolCallResult {
	toolCalls, _ := ExtractToolCallsFromOutputValidated(eventData)
	return toolCalls
}

func malformedToolArgumentsFailurePayload(err error) []byte {
	message := "upstream function call arguments contain invalid JSON"
	if err != nil && strings.TrimSpace(err.Error()) != "" {
		message = err.Error()
	}
	payload := map[string]any{
		"type": "response.failed",
		"response": map[string]any{
			"created_at": time.Now().Unix(),
			"status":     "failed",
			"error": map[string]any{
				"code":        "bad_gateway",
				"type":        "upstream_protocol_error",
				"status_code": http.StatusBadGateway,
				"message":     message,
			},
		},
	}
	encoded, _ := json.Marshal(payload)
	return encoded
}
