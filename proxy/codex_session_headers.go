package proxy

import (
	"net/http"
	"os"
	"strings"

	"github.com/codex2api/auth"
)

// Codex 会话标识头保真。
//
// 真实客户端在每个 HTTP /responses 上发三个会话头（codex-rs/codex-api/src/endpoint/
// responses.rs stream_request + requests/headers.rs build_session_headers）：
//
//	x-client-request-id: {thread_id}
//	session-id:          {session_id}
//	thread-id:           {thread_id}
//
// WS 握手同形（codex-rs/core/src/client.rs build_websocket_headers）。本网关此前只发
// 一个 Session_id：HTTP/2 会把头名小写化成 session_id，与真实的 session-id 是**两个
// 不同的头**；thread-id 与 x-client-request-id 则完全缺失。也就是说，凡是走本网关的
// 请求，其会话头集合与任何真实 Codex 客户端都不重合，且这个差异每请求都在。
//
// 逃生阀：CODEX_SESSION_HEADER_MODE=legacy 恢复旧的 Session_id 形态。默认 native，
// 因为旧形态是缺陷而非可选风格——但如果哪天发现上游反而依赖下划线写法，
// 单个环境变量就能整体回退，不必改代码重新发版。
const (
	codexSessionHeaderModeNative = "native"
	codexSessionHeaderModeLegacy = "legacy"

	codexSessionIDHeader       = "Session-Id"
	codexLegacySessionIDHeader = "Session_id"
	codexConversationIDHeader  = "Conversation_id"
)

// codexSessionHeaderModeFromEnv 读取会话头形态档位。只有显式写 legacy 才回退，
// 其余取值（含空、含拼错）都按 native 处理。
func codexSessionHeaderModeFromEnv() string {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("CODEX_SESSION_HEADER_MODE")), codexSessionHeaderModeLegacy) {
		return codexSessionHeaderModeLegacy
	}
	return codexSessionHeaderModeNative
}

// codexSessionHeaderAlignsConverged 报告是否让出站 session-id 头改用收敛后的会话
// 身份，与 turn metadata / client_metadata 里的 session_id 对齐。
//
// 默认关，因为这会推翻本仓库一条既有的明确约束：出站会话键只归
// resolveUpstreamSessionID 管，收敛不介入（见 codex_fingerprint.go 顶部说明与
// auth/codex_fingerprint_mode.go）。那条约束保护的是 prompt cache 隔离——请求体的
// prompt_cache_key 确实仍由 resolveUpstreamSessionID 独立决定、不受本开关影响，
// 但上游是否**也**拿 session-id 头参与缓存分组，从客户端源码里看不出来。真要是
// 参与了，开启后同账号下所有下游用户就共用一份缓存前缀，那是隐私和正确性问题，
// 不是形状问题。
//
// 开启的收益：真实客户端的 session-id 头与 metadata.session_id 恒等
// （core/src/client.rs 用同一个 session_id 填两处）。收敛只改 metadata 一侧，
// 会让这两处各说各话，构成一个可比对的破绽——但这个比对需要上游主动关联
// 头与体，成本远高于看一眼 UUID 版本位。所以默认留在安全的一侧，
// 由部署者在确认上游缓存行为后显式打开。
func codexSessionHeaderAlignsConverged() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CODEX_SESSION_HEADER_ALIGN_CONVERGED"))) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

// ConvergedCodexSessionIdentity 返回本次请求收敛后的 (session, thread) 标识；
// 收敛关闭（off 档）或推导不出时返回空串，由调用方回落到网关自己的会话键。
func ConvergedCodexSessionIdentity(account *auth.Account, downstreamHeaders http.Header) (sessionID, threadID string) {
	ids := resolveCodexFingerprintIDs(account, downstreamHeaders)
	if ids == nil {
		return "", ""
	}
	return ids.sessionID, ids.threadID
}

// ApplyCodexSessionHeaders 写出站会话标识头。
//
// fallbackSessionID 是网关自己的会话/缓存键（resolveUpstreamSessionID 的产出）；
// 为空时整体跳过——既有行为就是"没有会话键就不发会话头"，这里不改变它。
//
// legacyConversationID 只在 legacy 档生效：WS 握手历史上会连带发一个 Conversation_id，
// HTTP 路径则显式删掉它。legacy 的契约是逐路径复原旧行为，所以这个差异由调用方声明，
// 而不是在这里替两条链路选一种。native 档两条路径都不发——真实客户端的 WS 握手头
// （build_websocket_headers）里根本没有这个头。
//
// 必须在指纹收敛之后、账号自定义头之前调用，与 ApplyCodexFingerprintHeaders
// 保持同一套优先级约定。
func ApplyCodexSessionHeaders(outbound http.Header, account *auth.Account, fallbackSessionID string, downstreamHeaders http.Header, legacyConversationID bool) {
	if outbound == nil {
		return
	}
	fallbackSessionID = strings.TrimSpace(fallbackSessionID)
	if fallbackSessionID == "" {
		return
	}

	if codexSessionHeaderModeFromEnv() == codexSessionHeaderModeLegacy {
		outbound.Set(codexLegacySessionIDHeader, fallbackSessionID)
		if legacyConversationID {
			outbound.Set(codexConversationIDHeader, fallbackSessionID)
		} else {
			outbound.Del(codexConversationIDHeader)
		}
		return
	}

	convergedSessionID, threadID := ConvergedCodexSessionIdentity(account, downstreamHeaders)
	if threadID == "" {
		// off/device 档不会生成收敛后的 thread_id，但真实 Codex 的
		// Thread-Id 仍是区分父任务与子 Agent 的会话语义。优先保留下游
		// 自报值（显式头优先、turn metadata 兜底），仅在确实缺失时才
		// 回落到单线程会话形态。full 档会在上一步给出收敛值，仍按配置
		// 有意把多条线程折叠为同一上游身份。
		_, threadID = extractClientCodexIdentity(downstreamHeaders)
	}

	// session-id 默认仍是网关自己的会话键：收敛不介入上游会话身份是既有约束，
	// 只有显式开启对齐才改用收敛值（见 codexSessionHeaderAlignsConverged）。
	sessionID := fallbackSessionID
	if convergedSessionID != "" && codexSessionHeaderAlignsConverged() {
		sessionID = convergedSessionID
	}

	// thread-id 用收敛值不受上面那条约束影响：这个头本来就不存在，没有既有行为可破坏，
	// 而它必须与 x-client-request-id 取同一个值——后者已经由 ApplyCodexFingerprintHeaders
	// 收敛过了，这里再回落到未收敛的值就自相矛盾。
	if threadID == "" {
		// 真实客户端的单线程会话里 thread_id 与 session_id 本就是同一个值
		// （core/src/client.rs 用同一个标识填两处），所以这个回落不是杜撰形状。
		threadID = sessionID
	}

	outbound.Set(codexSessionIDHeader, sessionID)
	outbound.Set(codexThreadIDHeader, threadID)
	// x-client-request-id 实测恒等于 thread_id，且真实客户端无条件发送。下游若已
	// 携带（白名单透传 + 收敛改写）就保留其取值，避免覆盖掉已经对齐的收敛结果。
	if strings.TrimSpace(outbound.Get(codexClientRequestIDHeader)) == "" {
		outbound.Set(codexClientRequestIDHeader, threadID)
	}
	// 下划线写法与 Conversation_id 都不属于真实形态；native 档下清掉，
	// 否则等于同时发新旧两套会话头，比只发旧的更可疑。
	outbound.Del(codexLegacySessionIDHeader)
	outbound.Del(codexConversationIDHeader)
}
