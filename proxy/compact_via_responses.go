package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ==================== compact 端点 body-signal 兼容 ====================
// 上游已下线专用的 /responses/compact 端点（返回 404），会话压缩迁移到普通
// /responses 请求体内嵌 type=compaction_trigger 的 input item（新版 Codex CLI
// 的 body-signal 形态）。系统设置 compact_via_responses_enabled 开启后，官方
// Codex OAuth 账号的 /v1/responses/compact 透传改写为该形态：注入触发器走
// /responses SSE，再把流聚合回 compact 的一次性 JSON 返回下游。中转（OpenAI
// Responses API）账号不受影响，仍请求中转自身的 /responses/compact。

// appendCompactionTriggerToResponsesBody 把已准备好的 Codex compact body 改写成
// body-signal 压缩形态。compact 预处理为旧专用端点剥除了 store/stream/include，
// 而 /responses 上游要求 stream=true 且 store=false（实测缺 store 直接
// 400 "Store must be set to false"），这里补回，并在 input 末尾追加
// compaction_trigger。客户端已经携带触发器时也会归一到 input 最后一项。
func appendCompactionTriggerToResponsesBody(body []byte) []byte {
	out, _ := sjson.SetBytes(body, "stream", true)
	out, _ = sjson.SetBytes(out, "store", false)
	if !gjson.GetBytes(out, "include").Exists() {
		out, _ = sjson.SetBytes(out, "include", []string{codexReasoningEncryptedContentInclude})
	}
	return normalizeCompactionTriggerFinal(out, true)
}

// normalizeCompactionTriggerFinal keeps at most one direct input-level
// compaction_trigger and moves it behind every history/message/tool item.
// Upstream rejects a trigger followed by any other input item with
// "The 'compaction_trigger' item must be the final input item."
//
// addIfMissing is true only when rewriting the legacy /responses/compact
// endpoint to body-signal compaction. Plain /responses preparation uses false:
// it repairs the order only when the client already supplied a trigger.
func normalizeCompactionTriggerFinal(body []byte, addIfMissing bool) []byte {
	trigger := json.RawMessage(`{"type":"compaction_trigger"}`)
	input := gjson.GetBytes(body, "input")
	switch {
	case input.IsArray():
		items := input.Array()
		triggerCount := 0
		for _, item := range items {
			if isDirectCompactionTrigger(item) {
				triggerCount++
			}
		}
		if triggerCount == 0 && !addIfMissing {
			return body
		}
		if triggerCount == 1 && len(items) > 0 && isCanonicalCompactionTrigger(items[len(items)-1]) {
			return body
		}

		normalized := make([]json.RawMessage, 0, len(items)+1)
		for _, item := range items {
			if isDirectCompactionTrigger(item) {
				continue
			}
			raw := strings.TrimSpace(item.Raw)
			if raw == "" {
				encoded, err := json.Marshal(item.Value())
				if err != nil {
					return body
				}
				raw = string(encoded)
			}
			normalized = append(normalized, json.RawMessage(raw))
		}
		normalized = append(normalized, trigger)
		encoded, err := json.Marshal(normalized)
		if err != nil {
			return body
		}
		out, err := sjson.SetRawBytes(body, "input", encoded)
		if err != nil {
			return body
		}
		return out
	case input.IsObject() && isDirectCompactionTrigger(input):
		encoded, err := json.Marshal([]json.RawMessage{trigger})
		if err != nil {
			return body
		}
		out, err := sjson.SetRawBytes(body, "input", encoded)
		if err != nil {
			return body
		}
		return out
	case addIfMissing:
		normalized := make([]json.RawMessage, 0, 2)
		switch {
		case input.Type == gjson.String:
			// input 为字符串时先转成标准 message item，再追加触发器。
			msg, err := json.Marshal(map[string]any{
				"type": "message",
				"role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": input.String()},
				},
			})
			if err != nil {
				return body
			}
			normalized = append(normalized, json.RawMessage(msg))
		case input.IsObject() && !isDirectCompactionTrigger(input):
			normalized = append(normalized, json.RawMessage(input.Raw))
		}
		normalized = append(normalized, trigger)
		encoded, err := json.Marshal(normalized)
		if err != nil {
			return body
		}
		out, err := sjson.SetRawBytes(body, "input", encoded)
		if err != nil {
			return body
		}
		return out
	default:
		return body
	}
}

func isDirectCompactionTrigger(item gjson.Result) bool {
	return item.IsObject() &&
		strings.EqualFold(strings.TrimSpace(item.Get("type").String()), "compaction_trigger")
}

func isCanonicalCompactionTrigger(item gjson.Result) bool {
	return item.IsObject() && item.Get("type").String() == "compaction_trigger"
}

// collectCompactResponsesSSE 把 body-signal 压缩的上游 /responses SSE 流聚合成
// compact 端点的一次性 JSON：取 response.completed 的 response 对象，并用沿途的
// response.output_item.done（含 compaction 项）补齐 output。上游以 response.failed
// 终止时原样返回该事件负载，由调用方按上游错误处理；流在终止事件前结束视为读错误。
func collectCompactResponsesSSE(body io.Reader) (responseJSON []byte, failedPayload []byte, err error) {
	return collectCompactResponsesSSEWithContinuousRetryKeepalive(context.Background(), body)
}

func collectCompactResponsesSSEWithContinuousRetryKeepalive(ctx context.Context, body io.Reader) (responseJSON []byte, failedPayload []byte, err error) {
	outputItems := make([]json.RawMessage, 0, 2)
	seenOutputItems := make(map[string]struct{})
	var completed []byte
	readErr := readSSEStreamWithContinuousRetryKeepalive(ctx, body, func(_ string, data []byte) bool {
		if item, ok := extractResponseOutputItemDone(data, seenOutputItems); ok {
			outputItems = append(outputItems, item)
		}
		switch gjson.GetBytes(data, "type").String() {
		case "response.completed":
			completed = append([]byte(nil), data...)
			return false
		case "response.failed":
			failedPayload = append([]byte(nil), data...)
			return false
		}
		return true
	})
	if readErr != nil {
		return nil, nil, readErr
	}
	if len(failedPayload) > 0 {
		return nil, failedPayload, nil
	}
	if len(completed) == 0 {
		return nil, nil, errors.New("upstream stream ended before terminal event")
	}
	responseObj := gjson.GetBytes(completed, "response")
	if !responseObj.Exists() || !responseObj.IsObject() {
		return nil, nil, errors.New("response.completed missing response object")
	}
	return restoreMissingResponseOutputs([]byte(responseObj.Raw), outputItems), nil, nil
}
