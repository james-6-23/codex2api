package proxy

import (
	"strings"

	"github.com/codex2api/auth"
)

// Kiro 模型 ID 规则移植自 ZyphrZero/kiro.rs 的 converter：已知 Claude 写法规范化为
// Kiro 的点号版本（claude-sonnet-4-6 → claude-sonnet-4.6），其余合法 ID 原样透传，
// 可用性由账号的模型目录与上游判定。

const kiroMaxModelIDLen = 256

var kiroClaudeFamilies = []string{"sonnet", "opus", "haiku", "fable", "mythos"}

// kiroMapModel 返回发给 Kiro 的模型 ID；非法 ID 返回空。
func kiroMapModel(model string) string {
	model = strings.TrimSpace(model)
	if model == "" || len(model) > kiroMaxModelIDLen || strings.ContainsFunc(model, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return ""
	}
	lower := strings.ToLower(model)
	if base, ok := strings.CutSuffix(lower, "-thinking"); ok && kiroModelUsesGPTReasoningEffort(base) {
		return base
	}
	if normalized := kiroNormalizeClaudeModel(model); normalized != "" {
		return normalized
	}
	return model
}

func kiroCanonicalVersion(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	first := parts[0]
	if len(parts) == 1 && strings.Contains(first, ".") {
		for _, part := range strings.Split(first, ".") {
			if part == "" || !isASCIIDigits(part) {
				return ""
			}
		}
		return first
	}
	if !isASCIIDigits(first) {
		return ""
	}
	switch len(parts) {
	case 1:
		return first
	case 2:
		if isASCIIDigits(parts[1]) {
			return first + "." + parts[1]
		}
	}
	return ""
}

func isASCIIDigits(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// kiroNormalizeClaudeModel 规范化 Anthropic 客户端常见的 Claude ID，不猜测非 Claude 模型。
func kiroNormalizeClaudeModel(model string) string {
	normalized := strings.ToLower(model)
	for {
		stripped := false
		for _, suffix := range []string{"-thinking", "-latest"} {
			if base, ok := strings.CutSuffix(normalized, suffix); ok {
				normalized, stripped = base, true
			}
		}
		if !stripped {
			break
		}
	}
	if idx := strings.LastIndex(normalized, "-"); idx >= 0 && len(normalized)-idx-1 == 8 && isASCIIDigits(normalized[idx+1:]) {
		normalized = normalized[:idx]
	}
	body, ok := strings.CutPrefix(normalized, "claude-")
	if !ok {
		return ""
	}
	for _, family := range kiroClaudeFamilies {
		if rest, ok := strings.CutPrefix(body, family); ok {
			if trimmed, cut := strings.CutPrefix(rest, "-"); cut {
				rest = trimmed
			} else if trimmed, cut := strings.CutPrefix(rest, "."); cut {
				rest = trimmed
			}
			version := kiroCanonicalVersion(strings.Split(rest, "-"))
			if version == "" {
				return ""
			}
			return "claude-" + family + "-" + version
		}
	}
	// 旧式日期 ID 把系列名放在版本之后，例如 claude-3-5-sonnet。
	parts := strings.Split(body, "-")
	for index, part := range parts {
		for _, family := range kiroClaudeFamilies {
			if part != family {
				continue
			}
			if index == 0 || index+1 != len(parts) {
				return ""
			}
			version := kiroCanonicalVersion(parts[:index])
			if version == "" {
				return ""
			}
			return "claude-" + family + "-" + version
		}
	}
	return ""
}

var kiroMillionTokenModels = map[string]struct{}{
	"claude-sonnet-4.6": {}, "claude-sonnet-4.8": {}, "claude-sonnet-5": {}, "claude-sonnet-5.5": {},
	"claude-opus-4.6": {}, "claude-opus-4.7": {}, "claude-opus-4.8": {}, "claude-opus-5": {}, "claude-opus-5.5": {},
	"claude-haiku-5.5": {}, "claude-fable-5": {},
	"gpt-5.6-sol": {}, "gpt-5.6-terra": {}, "gpt-5.6-luna": {},
}

// kiroContextWindow 把 contextUsageEvent 的百分比换算成 token 数时使用；漏配 1M 模型会让
// 客户端的上下文进度与自动压缩阈值缩小五倍。
func kiroContextWindow(model string) int {
	mapped := kiroMapModel(model)
	if _, ok := kiroMillionTokenModels[mapped]; ok {
		return 1_000_000
	}
	switch {
	case mapped == "grok-4.7":
		return 500_000
	case strings.HasPrefix(mapped, "gpt"):
		return 272_000
	}
	return 200_000
}

func kiroModelUsesGPTReasoningEffort(model string) bool {
	switch strings.ToLower(model) {
	case "gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna":
		return true
	}
	return false
}

// kiroModelUsesReasoningWireField：GPT-5.6 与 Grok 4.7 走 reasoning.effort，
// Grok 4.7 的 schema 拒绝 output_config。
func kiroModelUsesReasoningWireField(model string) bool {
	return kiroModelUsesGPTReasoningEffort(model) || strings.EqualFold(model, "grok-4.7")
}

func kiroModelSupportsNativeReasoning(model string) bool {
	if kiroModelUsesReasoningWireField(model) {
		return true
	}
	m := strings.ToLower(model)
	switch m {
	case "claude-opus-4.6", "claude-opus-4.7", "claude-opus-4.8", "claude-sonnet-4.6":
		return true
	}
	for _, marker := range []string{"fable-5", "mythos-5", "sonnet-5", "opus-5", "haiku-5", "claude-5"} {
		if strings.Contains(m, marker) {
			return true
		}
	}
	return false
}

func kiroModelSupportsXHighEffort(model string) bool {
	m := strings.ToLower(model)
	for _, marker := range []string{"opus-4.7", "opus-4.8", "fable-5", "mythos-5", "claude-5"} {
		if strings.Contains(m, marker) {
			return true
		}
	}
	switch m {
	case "claude-opus-4.6", "claude-sonnet-4.6", "claude-opus-4.5", "claude-sonnet-4.5", "claude-haiku-4.5":
		return false
	}
	return true
}

// kiroModelKeepsPriorThinking：较新的 Claude 允许回放已结束回合的 thinking，旧模型只保留当前回合。
func kiroModelKeepsPriorThinking(model string) bool {
	rest, ok := strings.CutPrefix(strings.ToLower(strings.TrimSpace(model)), "claude-")
	if !ok {
		return false
	}
	family, version, _ := strings.Cut(rest, "-")
	if isASCIIDigits(family) {
		return atoiSafe(family) >= 5
	}
	numbers := strings.FieldsFunc(version, func(r rune) bool { return r == '.' || r == '-' })
	major, minor := -1, 0
	if len(numbers) > 0 && isASCIIDigits(numbers[0]) {
		major = atoiSafe(numbers[0])
		if len(numbers) > 1 && isASCIIDigits(numbers[1]) {
			minor = atoiSafe(numbers[1])
		}
	}
	if major < 0 {
		return false
	}
	switch family {
	case "opus":
		return major > 4 || major == 4 && minor >= 5
	case "sonnet":
		return major > 4 || major == 4 && minor >= 6
	}
	return major >= 5
}

func atoiSafe(value string) int {
	n := 0
	for _, r := range value {
		n = n*10 + int(r-'0')
		if n > 1<<20 {
			return n
		}
	}
	return n
}

// kiroNormalizeEffort 把客户端 effort 收敛到该模型接受的档位。
func kiroNormalizeEffort(model, raw string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	if raw == "" {
		return ""
	}
	switch raw {
	case "x-high", "x_high":
		raw = "xhigh"
	}
	switch raw {
	case "none":
		if !kiroModelUsesGPTReasoningEffort(model) {
			return "high"
		}
	case "low", "medium", "high":
	case "xhigh":
		if !kiroModelSupportsXHighEffort(model) {
			return "high"
		}
	case "max":
		if strings.EqualFold(model, "grok-4.7") {
			return "xhigh"
		}
	default:
		return "high"
	}
	return raw
}

func kiroEffortFromBudgetTokens(tokens int) string {
	switch {
	case tokens <= 4_000:
		return "low"
	case tokens <= 16_000:
		return "medium"
	case tokens <= 64_000:
		return "high"
	}
	return "xhigh"
}

// kiroDefaultModels 是账号尚未同步模型目录时的保守默认集（取自 KIRO FREE 账号
// 2026-11 的 ListAvailableModels）。空目录不能放行任意 ID，否则 auto Key 下
// Kiro 账号会接走 Codex 等渠道的模型。
var kiroDefaultModels = []string{
	"claude-opus-5.5", "claude-sonnet-5.5", "claude-haiku-5.5", "claude-opus-5", "claude-sonnet-5",
	"claude-opus-4.8", "claude-opus-4.7", "claude-opus-4.6", "claude-sonnet-4.6",
	"claude-opus-4.5", "claude-sonnet-4.5", "claude-haiku-4.5",
	"gpt-5.6-sol", "gpt-5.6-terra", "gpt-5.6-luna", "grok-4.7", "kimi-k3",
	"deepseek-3.2", "minimax-m2.5", "minimax-m2.1", "glm-5", "qwen3-coder-next",
}

// KiroAccountModelIDs 返回账号可服务的 Kiro 模型 ID（已同步目录优先）。
func KiroAccountModelIDs(account *auth.Account) []string {
	if account == nil {
		return append([]string(nil), kiroDefaultModels...)
	}
	account.Mu().RLock()
	models := append([]string(nil), account.Models...)
	account.Mu().RUnlock()
	if len(models) == 0 {
		return append([]string(nil), kiroDefaultModels...)
	}
	return models
}

// kiroAccountSupportsModel 按 Kiro 规范化后的 ID 匹配账号目录（claude-sonnet-4-6 与
// claude-sonnet-4.6 视为同一个模型）。
func kiroAccountSupportsModel(account *auth.Account, model string) bool {
	if account == nil || !account.IsKiro() {
		return false
	}
	mapped := kiroMapModel(model)
	if mapped == "" {
		return false
	}
	for _, candidate := range KiroAccountModelIDs(account) {
		if strings.EqualFold(strings.TrimSpace(candidate), mapped) {
			return true
		}
	}
	return false
}

func kiroChannelAccountFilterWithGate(model string, gate modelRateLimitGate) auth.AccountFilter {
	model = strings.TrimSpace(model)
	return func(account *auth.Account) bool {
		return account != nil && account.IsKiro() && !gate.blocks(account, model) && kiroAccountSupportsModel(account, model)
	}
}
