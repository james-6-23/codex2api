package proxy

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/tidwall/gjson"
)

// deriveContentSessionSeed 从请求体派生稳定的会话种子（参考 sub2api 的
// deriveOpenAIContentSessionSeed）。仅取对话全程不变的字段：model、instructions、
// system/developer 消息、tools 定义、首条 user 消息。
//
// 用途：客户端未携带任何显式会话标识（Session_id/Conversation_id/Idempotency-Key/
// prompt_cache_key）时，让"同一段对话的多轮请求"收敛到同一个本地账号粘性键，
// 不同对话互不干扰。单 API Key 供多终端用户共用的部署下，粘性粒度从"整个 Key
// 挤一个账号"细化为"每段对话独立粘定一个账号"。该种子只参与本地路由
// （账号粘性 affinityKey），默认隔离模式下不会发往上游。
//
// 返回 "" 表示无法派生（调用方回退到按 API Key 派生）：
//   - 请求带 previous_response_id：不重发历史的续链客户端 input 逐轮变化，
//     内容种子会逐轮漂移导致换号、破坏续链，保持稳定的按 Key 兜底；
//   - 没有可锚定的首条 user 消息：种子质量不足，不如按 Key 兜底可预测。
func deriveContentSessionSeed(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	if gjson.GetBytes(body, "previous_response_id").String() != "" {
		return ""
	}

	h := sha256.New()
	writeField := func(tag, val string) {
		if val == "" {
			return
		}
		h.Write([]byte(tag))
		h.Write([]byte{0})
		h.Write([]byte(val))
		h.Write([]byte{0})
	}

	writeField("model", gjson.GetBytes(body, "model").String())
	writeField("instructions", gjson.GetBytes(body, "instructions").String())
	if tools := gjson.GetBytes(body, "tools"); tools.IsArray() && tools.Raw != "[]" {
		writeField("tools", tools.Raw)
	}

	firstUserCaptured := false
	scanItems := func(items gjson.Result) {
		items.ForEach(func(_, item gjson.Result) bool {
			switch item.Get("role").String() {
			case "system", "developer":
				writeField("system", item.Get("content").Raw)
			case "user":
				if !firstUserCaptured {
					writeField("first_user", item.Get("content").Raw)
					firstUserCaptured = true
				}
			}
			return true
		})
	}

	// Responses 格式（input 为 string 或数组）优先；回退 Chat Completions 的 messages。
	if input := gjson.GetBytes(body, "input"); input.Exists() {
		if input.Type == gjson.String {
			if s := input.String(); s != "" {
				writeField("first_user", s)
				firstUserCaptured = true
			}
		} else if input.IsArray() {
			scanItems(input)
		}
	} else if msgs := gjson.GetBytes(body, "messages"); msgs.IsArray() {
		scanItems(msgs)
	}

	if !firstUserCaptured {
		return ""
	}
	sum := h.Sum(nil)
	return "content-" + hex.EncodeToString(sum[:16])
}
