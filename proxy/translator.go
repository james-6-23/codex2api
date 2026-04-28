package proxy

import (
	"container/list"
	"crypto/sha256"
	"encoding/json"
	"strings"
	"sync"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ==================== 输入结构体（OpenAI Chat Completions 格式） ====================

// openAIRequest 表示 OpenAI Chat Completions 请求（仅解析翻译所需字段）
type openAIRequest struct {
	Model           string            `json:"model"`
	Messages        []openAIMessage   `json:"messages"`
	Tools           []json.RawMessage `json:"tools"`
	ReasoningEffort string            `json:"reasoning_effort"`
	ServiceTier     string            `json:"service_tier"`
	ServiceTierAlt  string            `json:"serviceTier"` // 兼容驼峰命名
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

// streamDelta 流式块中的增量内容
type streamDelta struct {
	Role             string          `json:"role,omitempty"`
	Content          *string         `json:"content,omitempty"`
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

// compactMessage 非流式响应中的消息
type compactMessage struct {
	Role      string               `json:"role"`
	Content   *string              `json:"content"`
	ToolCalls []compactToolCallOut `json:"tool_calls,omitempty"`
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
		Message string `json:"message"`
		Type    string `json:"type"`
	} `json:"error"`
}

// ==================== LRU 请求解析缓存 ====================

const requestCacheSize = 256

// maxTools 上游 Codex API 允许的最大工具数量
const maxTools = 128

const (
	codexImageGenerationBridgeMarker = "<codex2api-codex-image-generation>"
	codexImageGenerationBridgeText   = codexImageGenerationBridgeMarker + "\nWhen the user asks for raster image generation or editing, use the OpenAI Responses native `image_generation` tool attached to this request. The local Codex client may not expose an `image_gen` namespace, but that does not mean image generation is unavailable. Do not ask the user to switch to CLI fallback solely because `image_gen` is absent.\n</codex2api-codex-image-generation>"
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
func normalizeResponsesCompactionItems(body map[string]any) bool {
	if len(body) == 0 {
		return false
	}
	inputItems, ok := body["input"].([]any)
	if !ok {
		return false
	}

	const summaryPrefix = "[Conversation summary from earlier turns]\n"

	modified := false
	out := make([]any, 0, len(inputItems))
	for _, raw := range inputItems {
		itemMap, ok := raw.(map[string]any)
		if !ok {
			out = append(out, raw)
			continue
		}
		if firstNonEmptyAnyString(itemMap["type"]) != "compaction" {
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

		out = append(out, map[string]any{
			"type": "message",
			"role": "developer",
			"content": []any{
				map[string]any{
					"type": "input_text",
					"text": summaryPrefix + summaryText,
				},
			},
		})
		modified = true
	}

	if modified {
		body["input"] = out
	}
	return modified
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
	req := cachedOrParse(rawJSON)

	// 构建输出 map（只包含 Codex 需要的字段）
	out := map[string]any{
		"model":   req.Model,
		"stream":  true,
		"store":   false,
		"include": []string{"reasoning.encrypted_content"},
	}

	// 1. messages → input
	out["input"] = convertMessagesToInputSlice(req.Messages)

	// 2. reasoning effort
	if effort := normalizeReasoningEffort(req.ReasoningEffort); effort != "" {
		out["reasoning"] = map[string]any{"effort": effort}
	}

	// 3. service tier（保留合法值，丢弃不支持的；fast 映射为上游接受的 priority）
	tier := req.ServiceTier
	if tier == "" {
		tier = req.ServiceTierAlt
	}
	tier = strings.TrimSpace(tier)
	if isAllowedServiceTier(tier) {
		out["service_tier"] = upstreamServiceTier(tier)
	}

	// 4. tools 格式转换 + schema 清理
	if len(req.Tools) > 0 {
		if tools := convertToolsToCodexFormat(req.Tools); len(tools) > 0 {
			out["tools"] = tools
		}
	}

	return json.Marshal(out)
}

// PrepareResponsesBody 将 Responses API 原始请求转换为上游可接受的格式
// 采用 Unmarshal→map 操作→Marshal 模式，替代逐字段 sjson 操作
// 返回: (处理后的 body, 展开后的 input JSON 原始文本)
func PrepareResponsesBody(rawBody []byte) ([]byte, string) {
	var body map[string]any
	if err := json.Unmarshal(rawBody, &body); err != nil {
		return rawBody, ""
	}

	// 1. 强制设置 Codex 必需字段
	body["stream"] = true
	body["store"] = false
	if _, ok := body["include"]; !ok {
		body["include"] = []string{"reasoning.encrypted_content"}
	}

	normalizeResponsesImageOnlyModel(body)
	normalizeResponsesPromptCompat(body)

	// 2. 字符串 input → 数组包装（Codex 要求 input 为 list）
	if inputStr, ok := body["input"].(string); ok {
		body["input"] = []map[string]string{
			{"role": "user", "content": inputStr},
		}
	}
	promptText := extractResponsesPromptText(body)

	// 3. reasoning_effort → reasoning.effort 自动转换 + 钳位
	if re, ok := body["reasoning_effort"].(string); ok {
		if normalized := normalizeReasoningEffort(re); normalized != "" {
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
			if normalized := normalizeReasoningEffort(effort); normalized != "" {
				reasoning["effort"] = normalized
			} else {
				delete(reasoning, "effort")
			}
		}
	}

	// 4. service tier 清理（fast 映射为上游接受的 priority）
	delete(body, "serviceTier")
	if tier, ok := body["service_tier"].(string); ok {
		tier = strings.TrimSpace(tier)
		if !isAllowedServiceTier(tier) {
			delete(body, "service_tier")
		} else {
			body["service_tier"] = upstreamServiceTier(tier)
		}
	}

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
			toolMap, ok := t.(map[string]any)
			if !ok {
				continue
			}
			// 补充默认描述
			if toolType, _ := toolMap["type"].(string); toolType != "" {
				if defaultDesc, ok := toolDescDefaults[toolType]; ok {
					desc, _ := toolMap["description"].(string)
					if desc == "" {
						toolMap["description"] = defaultDesc
					}
				}
			}
			// 递归清理不支持的 JSON Schema 关键字，并修正上游要求的结构
			if params, ok := toolMap["parameters"].(map[string]any); ok {
				sanitizeSchemaForUpstream(params)
			}
		}
	}
	ensureResponsesImageGenerationTool(body)
	moveTopLevelResponsesImageOptions(body)
	normalizeResponsesImageGenerationTools(body, promptText)
	applyResponsesImageGenerationBridgeInstructions(body)

	// 6. 展开 previous_response_id
	prevID, _ := body["previous_response_id"].(string)
	if prevID != "" {
		if cached := getResponseCache(prevID); cached != nil {
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

	// 保存展开后的 input 原始 JSON（用于响应缓存链路）
	var expandedInputRaw string
	if inputVal, ok := body["input"]; ok {
		if b, err := json.Marshal(inputVal); err == nil {
			expandedInputRaw = string(b)
		}
	}

	// 7. 删除 Codex 不支持的字段
	for _, field := range []string{
		"max_output_tokens", "max_tokens", "max_completion_tokens",
		"temperature", "top_p", "frequency_penalty", "presence_penalty",
		"logprobs", "top_logprobs", "n", "seed", "stop", "user",
		"logit_bias", "response_format", "serviceTier",
		"stream_options", "reasoning_effort", "truncation", "context_management",
		"disable_response_storage", "verbosity",
	} {
		delete(body, field)
	}

	result, err := json.Marshal(body)
	if err != nil {
		return rawBody, expandedInputRaw
	}
	return result, expandedInputRaw
}

// PrepareCompactResponsesBody 将 /responses/compact 请求转换为上游可接受的格式。
// 它复用通用 Responses 预处理，但会移除 compact 端点不接受的自动注入字段。
func PrepareCompactResponsesBody(rawBody []byte) ([]byte, string) {
	body, expandedInputRaw := PrepareResponsesBody(rawBody)
	body, _ = sjson.DeleteBytes(body, "include")
	body, _ = sjson.DeleteBytes(body, "store")
	body, _ = sjson.DeleteBytes(body, "stream")
	return body, expandedInputRaw
}

// normalizeReasoningEffort 将 reasoning_effort 钳位到上游支持的值
func normalizeReasoningEffort(effort string) string {
	effort = strings.ToLower(strings.TrimSpace(effort))
	if effort == "" {
		return ""
	}
	switch effort {
	case "low", "medium", "high", "xhigh":
		return effort
	case "max":
		return "xhigh"
	default:
		return "high"
	}
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

// upstreamServiceTier 将客户端 service_tier 映射为上游接受的值（fast → priority）
func upstreamServiceTier(tier string) string {
	if tier == "fast" {
		return "priority"
	}
	return tier
}

// convertMessagesToInputSlice 将 OpenAI messages 转换为 Codex input 数组（纯内存操作，零中间序列化）
func convertMessagesToInputSlice(messages []openAIMessage) []any {
	input := make([]any, 0, len(messages))

	for _, m := range messages {
		switch m.Role {
		case "tool":
			input = append(input, map[string]any{
				"type":    "function_call_output",
				"call_id": m.ToolCallID,
				"output":  rawMessageToString(m.Content),
			})

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
			}
		}
		return parts
	default:
		return parts
	}
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

		if parsed.Type != "function" || parsed.Function == nil {
			// 非 function 类型 → 透传原始 JSON
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
			if json.Unmarshal(parsed.Function.Parameters, &params) == nil {
				sanitizeSchemaForUpstream(params)
				item["parameters"] = params
			}
		}
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
		// fast 映射为上游接受的 priority
		if tier == "fast" {
			body, _ = sjson.SetBytes(body, "service_tier", "priority")
		}
		return body
	default:
		body, _ = sjson.DeleteBytes(body, "service_tier")
		body, _ = sjson.DeleteBytes(body, "serviceTier")
		return body
	}
}

// resolveServiceTier 从实际 tier 和请求 tier 中选择最终值
func resolveServiceTier(actualTier, requestedTier string) string {
	requestedTier = strings.TrimSpace(requestedTier)
	if requestedTier == "fast" {
		return requestedTier
	}
	actualTier = strings.TrimSpace(actualTier)
	if actualTier != "" {
		return actualTier
	}
	return requestedTier
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

// stripUnsupportedSchemaKeys 递归删除 schema 中上游不支持的关键字
func stripUnsupportedSchemaKeys(schema map[string]interface{}) {
	for key := range unsupportedSchemaKeys {
		delete(schema, key)
	}
	if props, ok := schema["properties"].(map[string]interface{}); ok {
		for _, v := range props {
			if sub, ok := v.(map[string]interface{}); ok {
				stripUnsupportedSchemaKeys(sub)
			}
		}
	}
	if items, ok := schema["items"].(map[string]interface{}); ok {
		stripUnsupportedSchemaKeys(items)
	}
	for _, key := range []string{"allOf", "anyOf", "oneOf"} {
		if arr, ok := schema[key].([]interface{}); ok {
			for _, item := range arr {
				if sub, ok := item.(map[string]interface{}); ok {
					stripUnsupportedSchemaKeys(sub)
				}
			}
		}
	}
	if addProps, ok := schema["additionalProperties"].(map[string]interface{}); ok {
		stripUnsupportedSchemaKeys(addProps)
	}
	if defs, ok := schema["$defs"].(map[string]interface{}); ok {
		for _, v := range defs {
			if sub, ok := v.(map[string]interface{}); ok {
				stripUnsupportedSchemaKeys(sub)
			}
		}
	}
}

func sanitizeSchemaForUpstream(schema map[string]interface{}) {
	stripUnsupportedSchemaKeys(schema)
	ensureArrayItems(schema)
}

// ensureArrayItems 递归为缺失 items 的数组 schema 补上空 schema，
// 兼容上游对 array 必须声明 items 的校验。
func ensureArrayItems(schema map[string]interface{}) {
	if schemaDeclaresArray(schema) {
		if _, ok := schema["items"]; !ok {
			schema["items"] = map[string]interface{}{}
		}
	}
	if props, ok := schema["properties"].(map[string]interface{}); ok {
		for _, v := range props {
			if sub, ok := v.(map[string]interface{}); ok {
				ensureArrayItems(sub)
			}
		}
	}
	if items, ok := schema["items"].(map[string]interface{}); ok {
		ensureArrayItems(items)
	}
	for _, key := range []string{"allOf", "anyOf", "oneOf"} {
		if arr, ok := schema[key].([]interface{}); ok {
			for _, item := range arr {
				if sub, ok := item.(map[string]interface{}); ok {
					ensureArrayItems(sub)
				}
			}
		}
	}
	if addProps, ok := schema["additionalProperties"].(map[string]interface{}); ok {
		ensureArrayItems(addProps)
	}
	if defs, ok := schema["$defs"].(map[string]interface{}); ok {
		for _, v := range defs {
			if sub, ok := v.(map[string]interface{}); ok {
				ensureArrayItems(sub)
			}
		}
	}
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

// newReasoningChunk 构建推理内容流式块
func newReasoningChunk(id, model string, created int64, reasoning string) []byte {
	chunk := openAIStreamChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []streamChoice{{
			Index: 0,
			Delta: &streamDelta{ReasoningContent: &reasoning},
		}},
	}
	b, _ := json.Marshal(chunk)
	return b
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

// newFinalChunk 构建最终流式块（含 finish_reason 和可选 usage）
func newFinalChunk(id, model string, created int64, finishReason string, usage *UsageInfo) []byte {
	chunk := openAIStreamChunk{
		ID: id, Object: "chat.completion.chunk", Created: created, Model: model,
		Choices: []streamChoice{{
			Index:        0,
			FinishReason: &finishReason,
		}},
		Usage: usage,
	}
	b, _ := json.Marshal(chunk)
	return b
}

// newErrorResponse 构建错误响应
func newErrorResponse(message string) []byte {
	resp := openAIErrorResponse{}
	resp.Error.Message = message
	resp.Error.Type = "upstream_error"
	b, _ := json.Marshal(resp)
	return b
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

	case "response.completed":
		usage := extractUsage(eventData)
		return newFinalChunk(chunkID, model, created, "stop", usage), true

	case "response.failed":
		errMsg := gjson.GetBytes(eventData, "response.error.message").String()
		if errMsg == "" {
			errMsg = "Codex upstream error"
		}
		return newErrorResponse(errMsg), true

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
	Model        string
	ChunkID      string
	Created      int64
	HasToolCalls bool
	toolCallMap  map[string]int // Codex item.id → OpenAI tool_calls index
	nextIdx      int
}

// NewStreamTranslator 创建流式翻译器实例
func NewStreamTranslator(chunkID, model string, created int64) *StreamTranslator {
	return &StreamTranslator{
		Model:       model,
		ChunkID:     chunkID,
		Created:     created,
		toolCallMap: make(map[string]int),
	}
}

// Translate 将单个 Codex SSE 事件翻译为 OpenAI Chat Completions 流式格式
func (st *StreamTranslator) Translate(eventData []byte) ([]byte, bool) {
	eventType := gjson.GetBytes(eventData, "type").String()

	switch eventType {
	case "response.output_text.delta":
		delta := gjson.GetBytes(eventData, "delta").String()
		return newContentChunk(st.ChunkID, st.Model, st.Created, delta), false

	case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
		delta := gjson.GetBytes(eventData, "delta").String()
		return newReasoningChunk(st.ChunkID, st.Model, st.Created, delta), false

	case "response.output_item.added":
		itemType := gjson.GetBytes(eventData, "item.type").String()
		if itemType != "function_call" {
			return nil, false
		}
		callID := gjson.GetBytes(eventData, "item.call_id").String()
		name := gjson.GetBytes(eventData, "item.name").String()
		itemID := gjson.GetBytes(eventData, "item.id").String()

		tcIdx := st.nextIdx
		st.toolCallMap[itemID] = tcIdx
		st.nextIdx++
		st.HasToolCalls = true

		return newToolCallAnnouncementChunk(st.ChunkID, st.Model, st.Created, tcIdx, callID, name), false

	case "response.function_call_arguments.delta":
		itemID := gjson.GetBytes(eventData, "item_id").String()
		tcIdx, ok := st.toolCallMap[itemID]
		if !ok {
			return nil, false
		}
		delta := gjson.GetBytes(eventData, "delta").String()
		return newToolCallDeltaChunk(st.ChunkID, st.Model, st.Created, tcIdx, delta), false

	case "response.function_call_arguments.done":
		return nil, false

	case "response.completed":
		usage := extractUsage(eventData)
		finishReason := "stop"
		if st.HasToolCalls {
			finishReason = "tool_calls"
		}
		return newFinalChunk(st.ChunkID, st.Model, st.Created, finishReason, usage), true

	case "response.failed":
		errMsg := gjson.GetBytes(eventData, "response.error.message").String()
		if errMsg == "" {
			errMsg = "Codex upstream error"
		}
		return newErrorResponse(errMsg), true

	case "response.content_part.done", "response.output_item.done",
		"response.created", "response.in_progress",
		"response.content_part.added",
		"response.reasoning_summary_text.done",
		"response.reasoning.encrypted_content.delta", "response.reasoning.encrypted_content.done",
		"response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		return nil, false

	default:
		if delta := gjson.GetBytes(eventData, "delta"); delta.Exists() && delta.Type == gjson.String {
			return newContentChunk(st.ChunkID, st.Model, st.Created, delta.String()), false
		}
		return nil, false
	}
}

// ==================== 非流式响应翻译 ====================

// TranslateCompactResponse 将 Codex 非流式响应转换为 OpenAI 格式
func TranslateCompactResponse(responseData []byte, model string, id string) []byte {
	var outputText string
	output := gjson.GetBytes(responseData, "output")
	if output.IsArray() {
		output.ForEach(func(_, item gjson.Result) bool {
			if item.Get("type").String() == "message" {
				content := item.Get("content")
				if content.IsArray() {
					content.ForEach(func(_, part gjson.Result) bool {
						if part.Get("type").String() == "output_text" {
							outputText += part.Get("text").String()
						}
						return true
					})
				}
			}
			return true
		})
	}

	usage := extractUsage(responseData)

	resp := openAICompactResponse{
		ID:     id,
		Object: "chat.completion",
		Model:  model,
		Choices: []compactChoice{{
			Index: 0,
			Message: compactMessage{
				Role:    "assistant",
				Content: &outputText,
			},
			FinishReason: "stop",
		}},
		Usage: usage,
	}
	b, _ := json.Marshal(resp)
	return b
}

// BuildCompactResponse 构建非流式完整响应（供 handler.go 调用，替代内联 sjson）
// 当有 toolCalls 且 content 为空时，content 输出为 JSON null
func BuildCompactResponse(id, model string, created int64, content string, toolCalls []ToolCallResult, usage *UsageInfo) []byte {
	finishReason := "stop"
	msg := compactMessage{
		Role:    "assistant",
		Content: &content,
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

// ExtractToolCallsFromOutput 从 response.completed 事件的 output 数组中提取 function_call 项
func ExtractToolCallsFromOutput(eventData []byte) []ToolCallResult {
	var toolCalls []ToolCallResult
	output := gjson.GetBytes(eventData, "response.output")
	if !output.IsArray() {
		return nil
	}
	output.ForEach(func(_, item gjson.Result) bool {
		if item.Get("type").String() == "function_call" {
			toolCalls = append(toolCalls, ToolCallResult{
				ID:        item.Get("call_id").String(),
				Name:      item.Get("name").String(),
				Arguments: item.Get("arguments").String(),
			})
		}
		return true
	})
	return toolCalls
}
