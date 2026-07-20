package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// ==================== 上下文超窗自动压缩 ====================
// 上游对超出模型上下文窗口的输入返回确定性 400（context_length_exceeded），
// 换号重试也必然失败，此前网关只能把错误透传给下游（issue #310/#415）。
// 上游不接受 truncation/context_management 参数（实测 400 Unsupported parameter），
// 服务端截断不可用；不做压缩的客户端（chat UI / SDK）只能新开会话。
//
// 本模块提供网关侧兜底：per-key 开关（limits.auto_compact_overflow）打开时，
// 收到超窗错误后把 input[] 的旧轮次摘要成一条 developer message，保留最近的
// 轮次原文，重试一次。摘要通过内部 Responses 调用完成；摘要失败时退化为
// 直接丢弃旧轮次并插入省略标记（保证重试仍能发出）。

const (
	// overflowCompactTailBytesDefault 压缩后保留原文的“最近轮次”预算（字节）。
	overflowCompactTailBytesDefault = 200 * 1024
	// overflowCompactSummaryInputBytesDefault 送去摘要的旧轮次文本上限（字节），
	// 保证摘要调用自身远离窗口限制。
	overflowCompactSummaryInputBytesDefault = 512 * 1024
	// overflowCompactSummaryTimeout 摘要调用的超时。
	overflowCompactSummaryTimeout = 120 * time.Second
	// overflowCompactMinItems input 少于该条数时不值得压缩（多半是单条超大输入，
	// 压缩救不了，直接透传原错误）。
	overflowCompactMinItems = 4
	// overflowCompactItemTextCap 单条旧轮次拍平后计入摘要转写的文本上限。
	overflowCompactItemTextCap = 8 * 1024
)

const overflowCompactSummaryPrefix = "[Conversation summary from earlier turns]\n"
const overflowCompactOmittedMarker = "[Earlier conversation turns were omitted because the input exceeded the model context window.]"

func overflowCompactTailBytes() int {
	if v := strings.TrimSpace(os.Getenv("CODEX_OVERFLOW_COMPACT_TAIL_KB")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n * 1024
		}
	}
	return overflowCompactTailBytesDefault
}

func overflowCompactSummaryInputBytes() int {
	if v := strings.TrimSpace(os.Getenv("CODEX_OVERFLOW_COMPACT_SUMMARY_INPUT_KB")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n * 1024
		}
	}
	return overflowCompactSummaryInputBytesDefault
}

// autoCompactOverflowEnabled 判断当前请求是否开启超窗自动压缩：
// 全局实验性开关（系统设置 overflow_auto_compact_enabled）或 per-key
// limits.auto_compact_overflow 任一开启即生效。
// 内部调用（无鉴权 Key 上下文）恒为 false，天然阻断摘要调用自身的压缩递归。
func autoCompactOverflowEnabled(c *gin.Context) bool {
	row := apiKeyRowFromContext(c)
	if row == nil {
		return false
	}
	if row.Limits.AutoCompactOnOverflow {
		return true
	}
	return CurrentRuntimeSettings().OverflowAutoCompact
}

// isCodexPreflightSSEEvent 判断是否为上游在响应内容前发送的元数据事件：
// codex.*（codex.rate_limits / codex.response.metadata，WS 传输路径）与裸
// response.metadata（HTTP 传输路径），均不携带模型产出。
func isCodexPreflightSSEEvent(eventType string) bool {
	eventType = strings.TrimSpace(eventType)
	if eventType == "response.metadata" {
		return true
	}
	return strings.HasPrefix(eventType, "codex.")
}

// isContextLengthExceededBody 判断上游错误体（HTTP 错误响应或 response.failed
// 的 error 对象）是否为上下文超窗。
func isContextLengthExceededBody(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	for _, path := range []string{"error.code", "error.type", "code", "detail.code"} {
		if strings.Contains(strings.ToLower(gjson.GetBytes(body, path).String()), "context_length") {
			return true
		}
	}
	for _, path := range []string{"error.message", "message", "detail"} {
		msg := strings.ToLower(gjson.GetBytes(body, path).String())
		if strings.Contains(msg, "exceeds the context window") || strings.Contains(msg, "context length") {
			return true
		}
	}
	return false
}

// isContextLengthExceededFailedPayload 判断 response.failed 事件是否为上下文超窗。
func isContextLengthExceededFailedPayload(payload []byte) bool {
	if len(payload) == 0 {
		return false
	}
	errObj := gjson.GetBytes(payload, "response.error")
	if !errObj.Exists() {
		return false
	}
	return isContextLengthExceededBody([]byte(errObj.Raw))
}

// compactOverflowResponsesBody 把已准备好的 Codex body 中 input[] 的旧轮次
// 压缩为一条 developer 摘要消息，保留尾部最近轮次原文。返回压缩后的 body。
// 无法压缩（input 太短 / 结构不符合预期）时返回 ok=false，调用方透传原错误。
func (h *Handler) compactOverflowResponsesBody(ctx context.Context, codexBody []byte) ([]byte, bool) {
	var body map[string]any
	if err := json.Unmarshal(codexBody, &body); err != nil {
		return nil, false
	}
	inputItems, ok := body["input"].([]any)
	if !ok || len(inputItems) < overflowCompactMinItems {
		return nil, false
	}

	// 从尾部往前累计字节数，保留 tailBudget 内的最近轮次原文。
	tailBudget := overflowCompactTailBytes()
	cut := len(inputItems)
	used := 0
	for i := len(inputItems) - 1; i >= 0; i-- {
		encoded, err := json.Marshal(inputItems[i])
		if err != nil {
			break
		}
		if used+len(encoded) > tailBudget && cut < len(inputItems) {
			break
		}
		used += len(encoded)
		cut = i
	}
	// 首条 developer/system 消息通常承载系统提示，始终保留原文。
	headStart := 0
	if first, ok := inputItems[0].(map[string]any); ok && isResponsesMessageInputItem(first) {
		switch strings.TrimSpace(firstNonEmptyAnyString(first["role"])) {
		case "developer", "system":
			headStart = 1
		}
	}
	if cut <= headStart {
		// 尾部预算已covers全部旧轮次，没有可压缩的头部——说明超窗来自
		// 单条超大 item 或预算过大，压缩无意义。
		return nil, false
	}
	head := inputItems[headStart:cut]
	tail := inputItems[cut:]

	summaryText := h.summarizeOverflowItems(ctx, firstNonEmptyAnyString(body["model"]), head)
	var replacement string
	if summaryText != "" {
		replacement = overflowCompactSummaryPrefix + summaryText
	} else {
		replacement = overflowCompactOmittedMarker
	}

	newInput := make([]any, 0, headStart+1+len(tail))
	newInput = append(newInput, inputItems[:headStart]...)
	newInput = append(newInput, map[string]any{
		"type": "message",
		"role": "developer",
		"content": []any{
			map[string]any{"type": "input_text", "text": replacement},
		},
	})
	newInput = append(newInput, tail...)
	body["input"] = newInput

	// 压缩边界可能切断工具调用配对（尾部 output 的 call 被摘要掉），复用配对修复。
	repairResponsesToolCallPairing(body)

	result, err := json.Marshal(body)
	if err != nil {
		return nil, false
	}
	log.Printf("超窗自动压缩: 旧轮次 %d 条 -> 摘要(%s), 保留最近 %d 条, body %dKB -> %dKB",
		len(head), map[bool]string{true: "模型摘要", false: "省略标记"}[summaryText != ""],
		len(tail), len(codexBody)/1024, len(result)/1024)
	return result, true
}

// summarizeOverflowItems 用内部 Responses 调用把旧轮次转写摘要成一段文本。
// 失败返回空串（调用方退化为省略标记）。
func (h *Handler) summarizeOverflowItems(ctx context.Context, model string, items []any) string {
	transcript := flattenOverflowItemsTranscript(items, overflowCompactSummaryInputBytes())
	if strings.TrimSpace(transcript) == "" || strings.TrimSpace(model) == "" {
		return ""
	}

	reqBody, err := json.Marshal(map[string]any{
		"model":     model,
		"stream":    true,
		"reasoning": map[string]any{"effort": "low"},
		"input": []any{
			map[string]any{
				"type":    "message",
				"role":    "developer",
				"content": []any{map[string]any{"type": "input_text", "text": "You are a conversation compaction assistant. Summarize the earlier conversation transcript provided by the user into a dense briefing for the assistant that will continue this conversation. Preserve: key facts and decisions, user goals and constraints, file paths, code identifiers, tool results that matter, and unresolved tasks. Output only the summary text."}},
			},
			map[string]any{
				"type":    "message",
				"role":    "user",
				"content": []any{map[string]any{"type": "input_text", "text": transcript}},
			},
		},
	})
	if err != nil {
		return ""
	}

	callCtx, cancel := context.WithTimeout(ctx, overflowCompactSummaryTimeout)
	defer cancel()
	status, respBody := h.ExecuteInternalResponse(callCtx, reqBody)
	if status != 200 {
		log.Printf("超窗自动压缩: 摘要调用失败 (status %d), 退化为省略标记", status)
		return ""
	}
	text := extractResponsesSSEOutputText(respBody)
	if strings.TrimSpace(text) == "" {
		log.Printf("超窗自动压缩: 摘要调用无文本输出, 退化为省略标记")
	}
	return strings.TrimSpace(text)
}

// flattenOverflowItemsTranscript 把 input items 拍平成"role: text"转写文本，
// 单条超长内容截断，总量超出 cap 时保留头尾、丢弃中段。
func flattenOverflowItemsTranscript(items []any, capBytes int) string {
	var lines []string
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		typ := strings.TrimSpace(firstNonEmptyAnyString(item["type"]))
		switch {
		case isResponsesMessageInputItem(item):
			role := strings.TrimSpace(firstNonEmptyAnyString(item["role"]))
			if role == "" {
				role = "user"
			}
			text := flattenMessageContentText(item["content"])
			if text != "" {
				lines = append(lines, role+": "+capText(text, overflowCompactItemTextCap))
			}
		case isCodexToolCallContextType(typ):
			name := strings.TrimSpace(firstNonEmptyAnyString(item["name"]))
			args := capText(firstNonEmptyAnyString(item["arguments"]), 1024)
			lines = append(lines, "assistant tool call "+name+"("+args+")")
		case isCodexToolCallOutputType(typ):
			lines = append(lines, "tool output: "+capText(flattenToolOutputText(item["output"]), overflowCompactItemTextCap))
		}
	}
	transcript := strings.Join(lines, "\n")
	if len(transcript) <= capBytes {
		return transcript
	}
	// 头尾各留一半，中段用标记替换。
	half := capBytes / 2
	return transcript[:half] + "\n[... middle of the transcript truncated ...]\n" + transcript[len(transcript)-half:]
}

// flattenMessageContentText 把 message content（string 或 content parts 数组）拍平成文本。
func flattenMessageContentText(content any) string {
	switch v := content.(type) {
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
		return sb.String()
	}
	return ""
}

func capText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "...[truncated]"
}

// extractResponsesSSEOutputText 从内部 Responses 调用的 SSE 响应体中提取
// response.completed 的全部 output_text。
func extractResponsesSSEOutputText(sse []byte) string {
	scanner := bufio.NewScanner(bytes.NewReader(sse))
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		payload := line[len("data: "):]
		if gjson.GetBytes(payload, "type").String() != "response.completed" {
			continue
		}
		var sb strings.Builder
		gjson.GetBytes(payload, "response.output").ForEach(func(_, item gjson.Result) bool {
			if item.Get("type").String() != "message" {
				return true
			}
			item.Get("content").ForEach(func(_, part gjson.Result) bool {
				if part.Get("type").String() == "output_text" {
					sb.WriteString(part.Get("text").String())
				}
				return true
			})
			return true
		})
		return sb.String()
	}
	return ""
}
