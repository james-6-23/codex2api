package auth

import "strings"

// Codex 设备指纹收敛模式。多人共享同一个 Codex OAuth 账号时，每个下游用户的
// Codex 客户端携带各自的 installation_id / session_id / thread_id，上游据此
// 判定这一个账号背后有多少台设备、多少个会话。收敛模式把这些标识改写成账号级
// 恒定值，让上游看到的设备/会话数收敛到接近单人使用的形态。
//
// 只影响出站请求里的 x-codex-turn-metadata 头和请求体 client_metadata；
// 出站 Session_id 头由 resolveUpstreamSessionID 独立决定，收敛不参与，
// 因此 prompt cache 隔离行为和 isolate_requests_by_default 设置不受影响。
const (
	// CodexFingerprintModeOff 不做任何收敛，客户端标识原样透传（默认）。
	CodexFingerprintModeOff = "off"
	// CodexFingerprintModeDevice 仅把 installation_id 收敛为账号级恒定值。
	// 上游看到 1 台设备 + 每个下游用户各自的会话。
	CodexFingerprintModeDevice = "device"
	// CodexFingerprintModeSession 收敛 installation_id + session_id，并按客户端
	// 原始会话标识确定性派生 thread_id：每个真实 Codex 会话得到一个独立线程。
	// 上游看到 1 台设备 + 1 会话 + N 线程，接近单人开多窗口/spawn 子代理的形态。
	CodexFingerprintModeSession = "session"
	// CodexFingerprintModeFull 令 thread_id 等于 session_id，上游看到
	// 1 台设备 + 1 会话 + 1 线程。最激进，也最不像真实客户端的并发形态。
	CodexFingerprintModeFull = "full"
)

// CodexFingerprintModeCredentialKey 是该模式在账号 credentials 中的存储键。
const CodexFingerprintModeCredentialKey = "codex_fingerprint_mode"

// NormalizeCodexFingerprintMode 归一化模式取值；空值和非法值都落到 off，
// 保证既有账号在升级后出站行为完全不变，必须显式配置才启用收敛。
func NormalizeCodexFingerprintMode(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case CodexFingerprintModeDevice:
		return CodexFingerprintModeDevice
	case CodexFingerprintModeSession:
		return CodexFingerprintModeSession
	case CodexFingerprintModeFull:
		return CodexFingerprintModeFull
	default:
		return CodexFingerprintModeOff
	}
}

// IsValidCodexFingerprintMode 报告取值是否为四个已知档位之一。
func IsValidCodexFingerprintMode(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case CodexFingerprintModeOff, CodexFingerprintModeDevice, CodexFingerprintModeSession, CodexFingerprintModeFull:
		return true
	default:
		return false
	}
}

// EffectiveCodexFingerprintMode 返回账号生效的收敛模式。
// 中转型账号（OpenAI Responses / Grok）不走 Codex 官方出站路径，恒为 off。
func (a *Account) EffectiveCodexFingerprintMode() string {
	if a == nil {
		return CodexFingerprintModeOff
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.isRelayStyleLocked() {
		return CodexFingerprintModeOff
	}
	return NormalizeCodexFingerprintMode(a.CodexFingerprintMode)
}

func (a *Account) EffectiveCodexInstallationID() string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.isRelayStyleLocked() {
		return ""
	}
	for name, value := range a.CustomHeaders {
		if strings.EqualFold(strings.TrimSpace(name), "X-Codex-Installation-Id") {
			if pinned := strings.TrimSpace(value); pinned != "" {
				return pinned
			}
		}
	}
	return strings.TrimSpace(a.CodexInstallationID)
}
