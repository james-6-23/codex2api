package proxy

import (
	"fmt"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ==================== 请求翻译: OpenAI Chat Completions → Codex Responses ====================

// TranslateRequest 将 OpenAI Chat Completions 请求转换为 Codex Responses 格式
func TranslateRequest(rawJSON []byte) ([]byte, error) {
	result := rawJSON

	// 1. 转换 messages → input
	messages := gjson.GetBytes(result, "messages")
	if messages.Exists() && messages.IsArray() {
		input := convertMessagesToInput(messages)
		result, _ = sjson.SetRawBytes(result, "input", input)
		result, _ = sjson.DeleteBytes(result, "messages")
	}

	// 2. 强制设置 Codex 必需字段
	result, _ = sjson.SetBytes(result, "stream", true)
	result, _ = sjson.SetBytes(result, "store", false)

	// 3. 删除 Codex 不支持的字段
	unsupportedFields := []string{
		"max_tokens", "max_completion_tokens", "temperature", "top_p",
		"frequency_penalty", "presence_penalty", "logprobs", "top_logprobs",
		"n", "seed", "stop", "user", "logit_bias", "response_format",
		"service_tier", "stream_options", "truncation", "context_management",
	}
	for _, field := range unsupportedFields {
		result, _ = sjson.DeleteBytes(result, field)
	}

	// 4. system → developer 角色转换
	result = convertSystemRoleToDeveloper(result)

	// 5. 添加 include
	result, _ = sjson.SetBytes(result, "include", []string{"reasoning.encrypted_content"})

	return result, nil
}

// convertMessagesToInput 将 OpenAI messages 格式转换为 Codex input 格式
func convertMessagesToInput(messages gjson.Result) []byte {
	var items []string

	messages.ForEach(func(_, msg gjson.Result) bool {
		role := msg.Get("role").String()
		content := msg.Get("content")

		// 角色映射
		switch role {
		case "system":
			role = "developer"
		case "assistant":
			role = "assistant"
		default:
			role = "user"
		}

		if content.Type == gjson.String {
			// 简单文本内容
			item := fmt.Sprintf(`{"type":"message","role":"%s","content":[{"type":"input_text","text":%s}]}`,
				role, escapeJSON(content.String()))
			items = append(items, item)
		} else if content.IsArray() {
			// 多部分内容（text / image_url 等）
			var parts []string
			content.ForEach(func(_, part gjson.Result) bool {
				partType := part.Get("type").String()
				switch partType {
				case "text":
					text := part.Get("text").String()
					parts = append(parts, fmt.Sprintf(`{"type":"input_text","text":%s}`, escapeJSON(text)))
				case "image_url":
					imgURL := part.Get("image_url.url").String()
					parts = append(parts, fmt.Sprintf(`{"type":"input_image","image_url":"%s"}`, imgURL))
				}
				return true
			})
			if len(parts) > 0 {
				item := fmt.Sprintf(`{"type":"message","role":"%s","content":[%s]}`,
					role, strings.Join(parts, ","))
				items = append(items, item)
			}
		}
		return true
	})

	return []byte("[" + strings.Join(items, ",") + "]")
}

// convertSystemRoleToDeveloper 将 input 中的 system 角色转为 developer
func convertSystemRoleToDeveloper(rawJSON []byte) []byte {
	inputResult := gjson.GetBytes(rawJSON, "input")
	if !inputResult.IsArray() {
		return rawJSON
	}

	result := rawJSON
	for i := 0; i < int(inputResult.Get("#").Int()); i++ {
		rolePath := fmt.Sprintf("input.%d.role", i)
		if gjson.GetBytes(result, rolePath).String() == "system" {
			result, _ = sjson.SetBytes(result, rolePath, "developer")
		}
	}
	return result
}

// ==================== 响应翻译: Codex SSE → OpenAI SSE ====================

// TranslateStreamChunk 将 Codex SSE 数据块翻译为 OpenAI Chat Completions 流式格式
func TranslateStreamChunk(eventData []byte, model string, chunkID string) ([]byte, bool) {
	eventType := gjson.GetBytes(eventData, "type").String()

	switch eventType {
	case "response.output_text.delta":
		delta := gjson.GetBytes(eventData, "delta").String()
		return buildOpenAIChunk(chunkID, model, delta, ""), false

	case "response.content_part.done":
		// 内容部分完成，不需要翻译
		return nil, false

	case "response.output_item.done":
		// 输出项完成
		return nil, false

	case "response.completed":
		// 生成完成，发送 [DONE]
		usage := extractUsage(eventData)
		chunk := buildOpenAIFinalChunk(chunkID, model, usage)
		return chunk, true

	case "response.failed":
		errMsg := gjson.GetBytes(eventData, "response.error.message").String()
		if errMsg == "" {
			errMsg = "Codex upstream error"
		}
		return buildOpenAIError(errMsg), true

	case "response.created", "response.in_progress",
		"response.output_item.added", "response.content_part.added",
		"response.reasoning_summary_text.delta", "response.reasoning_summary_text.done",
		"response.reasoning_summary_part.added", "response.reasoning_summary_part.done":
		// 这些事件不需要转发给下游
		return nil, false

	default:
		// 未知事件类型，尝试提取 delta
		if delta := gjson.GetBytes(eventData, "delta"); delta.Exists() && delta.Type == gjson.String {
			return buildOpenAIChunk(chunkID, model, delta.String(), ""), false
		}
		return nil, false
	}
}

// UsageInfo token 使用统计
type UsageInfo struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	InputTokens      int `json:"input_tokens"`
	OutputTokens     int `json:"output_tokens"`
	ReasoningTokens  int `json:"reasoning_tokens"`
}

// extractUsage 从 response.completed 事件提取 usage
func extractUsage(eventData []byte) *UsageInfo {
	usage := gjson.GetBytes(eventData, "response.usage")
	if !usage.Exists() {
		return nil
	}
	inputTokens := int(usage.Get("input_tokens").Int())
	outputTokens := int(usage.Get("output_tokens").Int())
	reasoningTokens := int(usage.Get("output_tokens_details.reasoning_tokens").Int())
	return &UsageInfo{
		PromptTokens:     inputTokens,
		CompletionTokens: outputTokens,
		TotalTokens:      inputTokens + outputTokens,
		InputTokens:      inputTokens,
		OutputTokens:     outputTokens,
		ReasoningTokens:  reasoningTokens,
	}
}

// buildOpenAIChunk 构建 OpenAI 流式响应块
func buildOpenAIChunk(id, model, content, finishReason string) []byte {
	chunk := []byte(`{}`)
	chunk, _ = sjson.SetBytes(chunk, "id", id)
	chunk, _ = sjson.SetBytes(chunk, "object", "chat.completion.chunk")
	chunk, _ = sjson.SetBytes(chunk, "created", 0) // 由调用方填充
	chunk, _ = sjson.SetBytes(chunk, "model", model)

	if content != "" {
		chunk, _ = sjson.SetBytes(chunk, "choices.0.index", 0)
		chunk, _ = sjson.SetBytes(chunk, "choices.0.delta.content", content)
	}
	if finishReason != "" {
		chunk, _ = sjson.SetBytes(chunk, "choices.0.finish_reason", finishReason)
	} else {
		chunk, _ = sjson.SetRawBytes(chunk, "choices.0.finish_reason", []byte("null"))
	}

	return chunk
}

// buildOpenAIFinalChunk 构建最终的 OpenAI 流式响应块（包含 usage）
func buildOpenAIFinalChunk(id, model string, usage *UsageInfo) []byte {
	chunk := buildOpenAIChunk(id, model, "", "stop")
	if usage != nil {
		chunk, _ = sjson.SetBytes(chunk, "usage.prompt_tokens", usage.PromptTokens)
		chunk, _ = sjson.SetBytes(chunk, "usage.completion_tokens", usage.CompletionTokens)
		chunk, _ = sjson.SetBytes(chunk, "usage.total_tokens", usage.TotalTokens)
	}
	return chunk
}

// buildOpenAIError 构建错误响应
func buildOpenAIError(message string) []byte {
	result := []byte(`{}`)
	result, _ = sjson.SetBytes(result, "error.message", message)
	result, _ = sjson.SetBytes(result, "error.type", "upstream_error")
	return result
}

// TranslateCompactResponse 将 Codex 非流式响应转换为 OpenAI 格式
func TranslateCompactResponse(responseData []byte, model string, id string) []byte {
	// 提取输出文本
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

	// 构建 OpenAI 非流式响应
	result := []byte(`{}`)
	result, _ = sjson.SetBytes(result, "id", id)
	result, _ = sjson.SetBytes(result, "object", "chat.completion")
	result, _ = sjson.SetBytes(result, "model", model)
	result, _ = sjson.SetBytes(result, "choices.0.index", 0)
	result, _ = sjson.SetBytes(result, "choices.0.message.role", "assistant")
	result, _ = sjson.SetBytes(result, "choices.0.message.content", outputText)
	result, _ = sjson.SetBytes(result, "choices.0.finish_reason", "stop")

	// 提取 usage
	usage := extractUsage(responseData)
	if usage == nil {
		usage = gjson.GetBytes(responseData, "usage").Value().(*UsageInfo)
	}
	if usage != nil {
		result, _ = sjson.SetBytes(result, "usage.prompt_tokens", usage.PromptTokens)
		result, _ = sjson.SetBytes(result, "usage.completion_tokens", usage.CompletionTokens)
		result, _ = sjson.SetBytes(result, "usage.total_tokens", usage.TotalTokens)
	}

	return result
}

// escapeJSON 安全转义 JSON 字符串
func escapeJSON(s string) string {
	b, _ := sjson.SetBytes([]byte(`{"v":""}`), "v", s)
	return gjson.GetBytes(b, "v").Raw
}
