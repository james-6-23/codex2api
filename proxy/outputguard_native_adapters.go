package proxy

import (
	"errors"
	"strings"

	"github.com/codex2api/auth"
	"github.com/codex2api/security/outputguard"
)

// errModelOutputGuardTriggered 是 Native HTTP 流式转发在 Guard 命中后返回的
// 请求内哨兵：它不会进入下游或日志，只用于让调用方识别“流已被安全终止”。
var errModelOutputGuardTriggered = errors.New("model output guard triggered")

// nativeGuardCoreConfig 从当前已应用的运行时快照构造请求级 Guard 配置。
// 快照在请求开始时取一次，后台轮询器的新 generation 不会篡改在途请求。
func nativeGuardCoreConfig() outputguard.Config {
	applied := CurrentOutputGuardConfig()
	return outputguard.Config{
		Enabled:               applied.Enabled,
		MaxToolArgumentBytes:  applied.MaxToolArgumentBytes,
		MaxToolArgumentDeltas: applied.MaxToolArgumentDeltas,
		MaxChannelBytes:       applied.MaxChannelBytes,
		MaxRequestBytes:       applied.MaxRequestBytes,
	}
}

// selectNativeProjector 为原生/中转通道选择真实 upstream projector。
// 严禁按客户端下游请求路径盲目推测上游协议：原生 Claude 永远是 Anthropic SSE，
// Codex/Responses 中转永远是 Responses SSE，其余按上游协议名分派。
func selectNativeProjector(protocol string, isNativeClaude bool, isCodexResponses bool) ProtocolProjector {
	return selectNativeProjectorWithScope(protocol, isNativeClaude, isCodexResponses, "")
}

// selectNativeProjectorWithScope 为指定 attempt 作用域选择投影器，
// 确保不同 attempt 内同名 block / part 的通道身份物理隔离。
func selectNativeProjectorWithScope(protocol string, isNativeClaude bool, isCodexResponses bool, scope string) ProtocolProjector {
	if isNativeClaude {
		return NewAnthropicProjectorWithScope(scope)
	}
	if isCodexResponses {
		return NewResponsesProjector()
	}
	norm := strings.ToLower(strings.TrimSpace(protocol))
	switch norm {
	case "anthropic", "messages":
		return NewAnthropicProjectorWithScope(scope)
	case "gemini":
		return NewGeminiProjectorWithScopeAndMode(scope, GeminiProjectorModeAuto)
	case "chat", "chat_completions", "chatcompletions":
		return NewChatProjector()
	case "responses":
		return NewResponsesProjector()
	default:
		return NewChatProjector()
	}
}

// grokProtocolGuardName 把上游 Grok 协议映射为 04a 终止原语使用的下游协议枚举
// （严格小写："anthropic"、"chat"、"responses"）。ChatCompletions 不能直接写成
// "chat_completions"，否则 04a 会退化到通用 envelope。
func grokProtocolGuardName(protocol GrokProtocol) string {
	switch auth.NormalizeGrokProtocol(string(protocol)) {
	case auth.GrokProtocolMessages:
		return "anthropic"
	case auth.GrokProtocolChatCompletions:
		return "chat"
	default:
		return "responses"
	}
}
