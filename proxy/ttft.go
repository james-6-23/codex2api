package proxy

import (
	"strings"

	"github.com/tidwall/gjson"
)

// isFirstTokenEvent 判断 codex SSE 事件类型是否可能代表"首个有内容产出"。
// 纯生命周期/结构事件不算首 token；完整 payload 场景应优先使用 isFirstTokenResult。
func isFirstTokenEvent(eventType string) bool {
	eventType = strings.TrimSpace(eventType)
	switch eventType {
	case "",
		"response.created",
		"response.in_progress",
		"response.completed",
		"response.failed",
		"response.incomplete",
		"response.cancelled",
		"response.canceled",
		"response.output_item.added",
		"response.output_item.done",
		"response.content_part.added":
		return false
	}
	if strings.Contains(eventType, ".delta") {
		return true
	}
	if strings.HasPrefix(eventType, "response.output_text") {
		return true
	}
	if strings.HasPrefix(eventType, "response.output") {
		return true
	}
	if strings.HasPrefix(eventType, "response.image_generation_call.") {
		return true
	}
	return false
}

// isPreContentLifecycleEvent 识别不含模型产出的 response.created / in_progress。
// 宽松首字统计排除这些帧；下游提交与首内容前重试另由 isFirstTokenResult 判断，
// 不能用这个生命周期分类决定结构帧是否应立即下发。
func isPreContentLifecycleEvent(eventType string) bool {
	switch strings.TrimSpace(eventType) {
	case "response.created", "response.in_progress":
		return true
	}
	return false
}

func isFirstTokenPayload(data []byte) bool {
	return isFirstTokenResult(gjson.ParseBytes(data))
}

func isFirstTokenResult(parsed gjson.Result) bool {
	eventType := strings.TrimSpace(parsed.Get("type").String())
	switch eventType {
	case "response.output_item.done":
		return outputItemHasFirstTokenContent(parsed.Get("item"))
	case "response.content_part.done":
		return contentPartHasFirstTokenContent(parsed.Get("part"))
	case "response.reasoning_summary_part.added":
		return parsed.Get("part.type").String() == "summary_text" &&
			contentPartHasFirstTokenContent(parsed.Get("part"))
	case "response.function_call_arguments.done":
		return stringFieldHasValue(parsed, "arguments")
	}
	if strings.Contains(eventType, ".delta") {
		return deltaEventHasContent(parsed)
	}
	if eventType == "response.image_generation_call.partial_image" {
		return stringFieldHasValue(parsed, "partial_image_b64") ||
			stringFieldHasValue(parsed, "partial_image")
	}
	if strings.HasPrefix(eventType, "response.output_text") {
		return stringFieldHasValue(parsed, "text") ||
			stringFieldHasValue(parsed, "delta") ||
			isFirstTokenEvent(eventType)
	}
	return isFirstTokenEvent(eventType)
}

// isLooseFirstTokenResult 是首字统计的唯一口径（宽松首字）：response.created /
// in_progress 之外的首个非终态响应事件即记为首字，结构帧也算。严格判定
// isFirstTokenResult 仍用于断流重试的 contentTokenSeen 等内容感知逻辑。
func isLooseFirstTokenResult(parsed gjson.Result) bool {
	eventType := strings.TrimSpace(parsed.Get("type").String())
	if eventType == "" || isPreContentLifecycleEvent(eventType) {
		return false
	}
	switch eventType {
	case "error",
		"response.error",
		"response.completed",
		"response.failed",
		"response.incomplete",
		"response.cancelled",
		"response.canceled":
		return false
	default:
		return true
	}
}

func deltaEventHasContent(parsed gjson.Result) bool {
	return stringFieldHasValue(parsed, "delta") ||
		stringFieldHasValue(parsed, "partial_image_b64") ||
		stringFieldHasValue(parsed, "partial_image")
}

func outputItemHasFirstTokenContent(item gjson.Result) bool {
	if !item.Exists() {
		return false
	}
	switch strings.TrimSpace(item.Get("type").String()) {
	case "message":
		return contentArrayHasFirstTokenContent(item.Get("content"))
	case "function_call":
		return stringFieldHasValue(item, "arguments")
	case "image_generation_call":
		return stringFieldHasValue(item, "result") ||
			stringFieldHasValue(item, "partial_image_b64") ||
			stringFieldHasValue(item, "partial_image")
	case "reasoning":
		return stringFieldHasValue(item, "encrypted_content") ||
			contentArrayHasFirstTokenContent(item.Get("summary"))
	default:
		return false
	}
}

func contentArrayHasFirstTokenContent(content gjson.Result) bool {
	if !content.IsArray() {
		return contentPartHasFirstTokenContent(content)
	}
	for _, part := range content.Array() {
		if contentPartHasFirstTokenContent(part) {
			return true
		}
	}
	return false
}

func contentPartHasFirstTokenContent(part gjson.Result) bool {
	if !part.Exists() {
		return false
	}
	return stringFieldHasValue(part, "text") ||
		stringFieldHasValue(part, "output_text") ||
		stringFieldHasValue(part, "refusal") ||
		stringFieldHasValue(part, "summary_text") ||
		stringFieldHasValue(part, "encrypted_content")
}

func stringFieldHasValue(result gjson.Result, path string) bool {
	field := result.Get(path)
	return field.Exists() && len(field.String()) > 0
}
