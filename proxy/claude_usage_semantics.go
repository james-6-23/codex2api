package proxy

import (
	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
)

// applyAnthropicUsageSemantics 把 Anthropic Messages 的用量口径转换成计费层期望的口径。
//
// Anthropic 的 input_tokens 只包含最后一个缓存断点之后的 token，缓存命中与缓存写入
// 分别记在 cache_read_input_tokens / cache_creation_input_tokens；而计费层沿用
// OpenAI 语义（input 已包含缓存部分，未缓存 = input − cached − 写入）。不做转换时
// 缓存命中会被钳到 input 以内，成本几乎归零。转换是幂等的。
func applyAnthropicUsageSemantics(usage *UsageInfo) {
	if usage == nil {
		return
	}
	uncached := usage.InputTokens
	if uncached == 0 && usage.PromptTokens > 0 {
		uncached = usage.PromptTokens
	}
	total := uncached + usage.CachedTokens + usage.CacheWriteTokens
	if usage.anthropicTotalApplied || total <= uncached {
		usage.anthropicTotalApplied = true
		return
	}
	usage.InputTokens = total
	usage.PromptTokens = total
	usage.TotalTokens = total + usage.OutputTokens
	if usage.CachedTokens > 0 {
		details := &TokenDetails{CachedTokens: usage.CachedTokens}
		usage.PromptTokensDetails = details
		usage.InputTokensDetails = details
	}
	usage.anthropicTotalApplied = true
}

// claudeFirstCacheControlTTL 返回请求里第一个 cache_control 块声明的 ttl（按 Anthropic 的
// 处理顺序 tools → system → messages）。空串表示没有显式 ttl（默认 5 分钟）。
func claudeFirstCacheControlTTL(body []byte) string {
	if !gjson.ValidBytes(body) {
		return ""
	}
	var ttl string
	found := false
	visit := func(items gjson.Result) {
		if found || !items.IsArray() {
			return
		}
		for _, item := range items.Array() {
			if cc := item.Get("cache_control"); cc.Exists() {
				ttl = cc.Get("ttl").String()
				found = true
				return
			}
		}
	}
	visit(gjson.GetBytes(body, "tools"))
	visit(gjson.GetBytes(body, "system"))
	if messages := gjson.GetBytes(body, "messages"); messages.IsArray() {
		for _, msg := range messages.Array() {
			visit(msg.Get("content"))
			if found {
				break
			}
		}
	}
	return ttl
}

// applyUsageCacheWritesToLog 把上游用量里的缓存写入 token 复制到待落库的用量记录。
func applyUsageCacheWritesToLog(log *database.UsageLogInput, usage *UsageInfo) {
	if log == nil || usage == nil {
		return
	}
	log.CacheWrite5mTokens, log.CacheWrite1hTokens = splitClaudeCacheWrites(usage)
}

// splitClaudeCacheWrites 返回缓存写入的 5m/1h 细分；上游只给了总数时按默认的 5 分钟缓存计。
func splitClaudeCacheWrites(usage *UsageInfo) (write5m, write1h int) {
	if usage == nil {
		return 0, 0
	}
	write5m, write1h = usage.CacheWrite5mTokens, usage.CacheWrite1hTokens
	if write5m+write1h == 0 && usage.CacheWriteTokens > 0 {
		write5m = usage.CacheWriteTokens
	}
	return write5m, write1h
}
