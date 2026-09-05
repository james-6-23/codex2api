package proxy

import (
	"net/http"
	"strings"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// claudeModelThinkingAlwaysOn 报告模型是否属于"思考常开、仅自适应"的系列。这些模型
// 拒绝 thinking.type=disabled（400），文档要求直接省略 thinking 参数。
func claudeModelThinkingAlwaysOn(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	return strings.HasPrefix(m, "claude-fable-") || strings.HasPrefix(m, "claude-mythos-")
}

// dropClaudeDisabledThinking 在发送前移除思考常开模型上的 thinking.type=disabled。
// 其它模型原样保留（Opus 5 在 effort<=high 时接受 disabled，由上游裁决）。
func dropClaudeDisabledThinking(body []byte) ([]byte, bool) {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return body, false
	}
	if !claudeModelThinkingAlwaysOn(gjson.GetBytes(body, "model").String()) {
		return body, false
	}
	if !strings.EqualFold(gjson.GetBytes(body, "thinking.type").String(), "disabled") {
		return body, false
	}
	out, err := sjson.DeleteBytes(body, "thinking")
	if err != nil {
		return body, false
	}
	return out, true
}

// isClaudeThinkingDisabledUnsupportedError 识别 Anthropic 对 thinking.type=disabled 的拒绝，
// 包括"该模型不支持 disabled"与"effort 过高时不允许关闭思考"两种文案；两者的修正都是
// 移除 thinking 参数让模型回到默认的自适应思考。
func isClaudeThinkingDisabledUnsupportedError(statusCode int, body []byte) bool {
	if statusCode != http.StatusBadRequest || len(body) == 0 {
		return false
	}
	message := strings.ToLower(gjson.GetBytes(body, "error.message").String() + " " + gjson.GetBytes(body, "message").String())
	if strings.Contains(message, "thinking.type.disabled") && strings.Contains(message, "not supported") {
		return true
	}
	return strings.Contains(message, "not supported when thinking is disabled")
}
