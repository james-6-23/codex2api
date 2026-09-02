package proxy

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"

	"github.com/codex2api/auth"
)

// Codex 设备指纹收敛：把出站请求里携带的客户端标识改写成账号级恒定值，
// 使上游看到的「设备数 / 会话数」收敛，而不是随共享该账号的下游用户数增长。
// 档位语义见 auth/codex_fingerprint_mode.go。
//
// 收敛面严格限定在两个载体：
//   - X-Codex-Turn-Metadata 请求头里的 JSON（本项目按白名单原样透传，见
//     codexAllowedForwardHeaders，客户端真实标识由此泄漏）
//   - 请求体 client_metadata（Codex 官方路径同样原样透传）
//
// 明确不改写的部分：
//   - 出站 Session_id 头：由 resolveUpstreamSessionID 独立决定（默认隔离模式下每请求
//     随机），收敛不介入，否则会改变 prompt cache 隔离语义。注意它与 metadata 里的
//     session_id 是两套独立身份，开启收敛后两者取值不同属预期。
//   - turn_id / turn_started_at_unix_ms：客户端逐轮随机值，不标识设备或会话；
//     重算反而会让头与体、turn_id 与其时间戳彼此不一致，本身就是特征。
//   - 客户端未携带的字段：只改写已存在的键，绝不新增，避免改变 metadata 的形状。
//     该规则同样适用于请求头——出站只覆盖下游确实发过的标识头，不凭空补发。

const (
	codexTurnMetadataHeader    = "X-Codex-Turn-Metadata"
	codexInstallationIDHeader  = "X-Codex-Installation-Id"
	codexWindowIDHeader        = "X-Codex-Window-Id"
	codexClientRequestIDHeader = "X-Client-Request-Id"
	codexThreadIDHeader        = "Thread-Id"
	codexParentThreadIDHeader  = "X-Codex-Parent-Thread-Id"
)

// codexFingerprintIDs 是一次请求的收敛目标值。全部字段都由「账号 + 下游请求头」
// 确定性推导，因此请求头改写点和请求体改写点各自推导也必然得到同一组值，
// 不需要跨函数传递即可保证一致。
type codexFingerprintIDs struct {
	mode           string
	accountID      int64
	installationID string
	sessionID      string
	threadID       string
	windowID       string
}

// deriveStableCodexUUID 从种子确定性派生一个 UUIDv4 格式的字符串：同一种子恒定返回
// 同一值，跨进程重启也不变，因此无需落库。
//
// 仅用于 installation_id：抓包实证真实客户端的 installation 标识是 v4，session/thread/
// window 则是 v7（见 deriveStableCodexUUIDv7）。这里没有复用 uuid.NewSHA1（本仓库其它
// 确定性 ID 的做法），因为那会产出 v5 UUID，版本位与真实 v4 不同，本身可被识别。
func deriveStableCodexUUID(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	var u uuid.UUID
	copy(u[:], sum[:16])
	u[6] = (u[6] & 0x0f) | 0x40 // version 4
	u[8] = (u[8] & 0x3f) | 0x80 // RFC 4122 variant
	return u.String()
}

// deriveStableCodexUUIDv7 从种子确定性派生一个 UUIDv7 格式的字符串：前 48 bit 是毫秒
// 时间戳（big-endian，按 RFC 9562），其余 74 bit 取种子哈希。同一 (种子, 时间戳) 恒定
// 返回同值，跨进程重启也不变，无需落库。
//
// 用于 session / thread / window：抓包实证真实客户端的这三个标识是 UUIDv7，时间戳位
// 解出的时刻与抓包时刻仅差数秒。之前一律写死 v4 有两处可识别特征——版本 nibble 与真实
// 不符；更关键的是 v4 无时间序，上游若按 UUID 排序（主键索引 / 日志），一个 v4 会因其
// 伪随机的"时间戳位"落到与真实会话完全无关的位置、跳到序列顶部或底部（见 #536 评论）。
// installation_id 真实是 v4，仍走 deriveStableCodexUUID。
func deriveStableCodexUUIDv7(seed string, unixMilli int64) string {
	sum := sha256.Sum256([]byte(seed))
	var u uuid.UUID
	copy(u[:], sum[:16])
	ms := uint64(unixMilli)
	u[0] = byte(ms >> 40)
	u[1] = byte(ms >> 32)
	u[2] = byte(ms >> 24)
	u[3] = byte(ms >> 16)
	u[4] = byte(ms >> 8)
	u[5] = byte(ms)
	u[6] = (u[6] & 0x0f) | 0x70 // version 7
	u[8] = (u[8] & 0x3f) | 0x80 // RFC 4122 variant
	return u.String()
}

const (
	// codexIdentityFallbackEpochMilli 是账号加入时间不可用时的时间戳基准，取一个固定
	// 的近期时刻（2025-01-01Z），确保派生的 v7 时间戳落在合理区间而不是 1970。
	codexIdentityFallbackEpochMilli int64 = 1_735_689_600_000
	// codexIdentitySpreadMilli 是会话时间戳相对基准的散布窗口（30 天）：让同账号的
	// session / thread / window 及不同线程的 v7 时间戳互不相同，又不至于漂移到远离
	// 账号存在期。真实客户端每个会话都会新建一批时间戳相近但互异的标识。
	codexIdentitySpreadMilli int64 = 30 * 24 * 60 * 60 * 1000
)

// codexIdentityUnixMilli 为账号的收敛身份 UUID 派生一个确定性毫秒时间戳，用作 v7 的
// 时间戳位。以账号加入时间为基准（随新账号自然前移，避免所有收敛值挤在同一固定时刻），
// 叠加一个由种子派生的有界偏移，使 session / thread / window 及不同线程的时间戳互不
// 相同而仍聚集在账号存在期附近。全程只依赖账号加入时间和种子，不引入 time.Now，因此
// 保持"同种子恒定、无需落库"的性质。
func codexIdentityUnixMilli(account *auth.Account, seed string) int64 {
	base := atomic.LoadInt64(&account.AddedAt) / int64(time.Millisecond)
	if base <= 0 {
		base = codexIdentityFallbackEpochMilli
	}
	sum := sha256.Sum256([]byte("codex2api:codex-identity-ts:v1:" + seed))
	offset := int64(binary.BigEndian.Uint64(sum[:8]) % uint64(codexIdentitySpreadMilli))
	return base + offset
}

// DeriveStableSessionUUIDv7 从种子确定性派生 UUIDv7 格式的会话身份，供没有账号
// 上下文的调用点使用（上游 Session_id / prompt_cache_key 的派生只拿得到 API Key
// 或 API Key ID）。时间戳位取固定基准加种子派生的有界偏移，因此同种子恒定返回
// 同值、跨进程重启不变，且不同种子的时间戳互不相同。
//
// 存在的理由是格式保真：真实 Codex 客户端的 session_id 是 UUIDv7。本网关此前两处
// 派生都不是——IsolateCodexSessionID 产出 16 位裸十六进制（连 UUID 都不是），
// deterministicPromptCacheKey 用 uuid.NewSHA1 产出 v5（版本 nibble 与真实不符）。
// 后者正是 deriveStableCodexUUID 注释里已经点名过的坑，当时只修了指纹模块这一侧。
func DeriveStableSessionUUIDv7(seed string) string {
	return deriveStableCodexUUIDv7(seed, seededIdentityUnixMilli(seed))
}

// NewUpstreamSessionUUID 生成一个每次都不同的 UUIDv7，供"每请求隔离"的上游会话
// 身份使用（默认隔离模式下的 prompt_cache_key 与 session-id 头）。
//
// 必须是 v7 而不是 uuid.NewString() 的 v4：真实 Codex 客户端的 session_id 恒为 v7，
// 而默认隔离是本网关最常走的路径——绝大多数出站请求的会话键都由这里产出，
// 版本位错了等于每个请求都带着一个与真实客户端不符的标记。v4 还没有时间序，
// 上游若按 UUID 排序（主键索引 / 日志），它会因伪随机的"时间戳位"落到与真实
// 会话完全无关的位置（与 deriveStableCodexUUIDv7 注释里记的是同一个问题）。
//
// NewV7 只在系统熵源不可用时报错，那种情况下回落到 v4 也比让请求失败强。
func NewUpstreamSessionUUID() string {
	if generated, err := uuid.NewV7(); err == nil {
		return generated.String()
	}
	return uuid.NewString()
}

// seededIdentityUnixMilli 是 codexIdentityUnixMilli 的无账号版本：拿不到账号加入
// 时间时以固定基准代替，其余散布逻辑一致。
func seededIdentityUnixMilli(seed string) int64 {
	sum := sha256.Sum256([]byte("codex2api:session-identity-ts:v1:" + seed))
	offset := int64(binary.BigEndian.Uint64(sum[:8]) % uint64(codexIdentitySpreadMilli))
	return codexIdentityFallbackEpochMilli + offset
}

// resolveCodexFingerprintIDs 按账号配置推导收敛目标值，off 档或账号缺失时返回 nil。
//
// downstreamHeaders 是下游客户端的原始请求头，用于取客户端真实会话/线程标识来派生
// thread_id。session 档：同一客户端会话一条线程；会话与线程不同（子代理）时按真实
// thread 再拆一条，避免 X-Client-Request-Id 塌缩后 previous_response_id 找不到。
func resolveCodexFingerprintIDs(account *auth.Account, downstreamHeaders http.Header) *codexFingerprintIDs {
	if account == nil {
		return nil
	}
	mode := account.EffectiveCodexFingerprintMode()
	if mode == auth.CodexFingerprintModeOff {
		return nil
	}
	accountID := account.ID()
	if accountID <= 0 {
		return nil
	}

	ids := &codexFingerprintIDs{
		mode:           mode,
		accountID:      accountID,
		installationID: resolveConvergedInstallationID(account, accountID),
	}
	if ids.installationID == "" {
		return nil
	}
	if mode == auth.CodexFingerprintModeDevice {
		return ids
	}

	sessionSeed := fmt.Sprintf("codex2api:codex-session-id:v1:%d", accountID)
	ids.sessionID = deriveStableCodexUUIDv7(sessionSeed, codexIdentityUnixMilli(account, sessionSeed))
	ids.threadID = ids.sessionID
	if mode == auth.CodexFingerprintModeSession {
		clientSessionID, clientThreadID := extractClientCodexIdentity(downstreamHeaders)
		var threadSeed string
		switch {
		case clientThreadID != "" && clientSessionID != "" && clientThreadID != clientSessionID:
			// 子代理 / 多窗口：真实 thread 与 session 不同。仍按会话派生会把
			// 多条线程收成一条，X-Client-Request-Id（实测恒等于 thread-id）
			// 跟着塌缩后，上游按线程查找 previous_response_id 就会 400（#541）。
			threadSeed = fmt.Sprintf("codex2api:codex-thread-id:v2:%d:%s", accountID, clientThreadID)
		case clientSessionID != "":
			threadSeed = fmt.Sprintf("codex2api:codex-thread-id:v1:%d:%s", accountID, clientSessionID)
		case clientThreadID != "":
			threadSeed = fmt.Sprintf("codex2api:codex-thread-id:v1:%d:%s", accountID, clientThreadID)
		}
		if threadSeed != "" {
			ids.threadID = deriveStableCodexUUIDv7(threadSeed, codexIdentityUnixMilli(account, threadSeed))
		}
	}
	ids.windowID = ids.threadID + ":0"
	return ids
}

// resolveConvergedInstallationID 返回账号级恒定的 installation_id。
// 账号自定义头里显式配置的 installation id 优先——运维借此把账号钉到一个已知的
// 真实设备标识上，且该值本来就会覆盖出站头（自定义头最后应用），这里一并采用可
// 保证请求头与 metadata 中的取值一致。
func resolveConvergedInstallationID(account *auth.Account, accountID int64) string {
	for name, value := range account.GetCustomHeaders() {
		if strings.EqualFold(strings.TrimSpace(name), codexInstallationIDHeader) {
			if pinned := strings.TrimSpace(value); pinned != "" {
				return pinned
			}
		}
	}
	return deriveStableCodexUUID(fmt.Sprintf("codex2api:codex-install-id:v1:%d", accountID))
}

// extractClientCodexSessionID 取下游客户端的原始会话标识。
// 只读请求头：请求头改写点拿不到请求体，限定在同一输入才能保证两处推导一致。
func extractClientCodexSessionID(headers http.Header) string {
	if headers == nil {
		return ""
	}
	// Codex CLI 走 HTTP/2 发的是连字符形式 session-id；下划线形式是旧写法，两者都认。
	for _, name := range []string{"Session-Id", "Session_id"} {
		if value := strings.TrimSpace(headers.Get(name)); value != "" {
			return value
		}
	}
	// 客户端未发会话头时，退回 turn metadata 里自报的会话标识。
	raw := strings.TrimSpace(headers.Get(codexTurnMetadataHeader))
	if raw == "" || !gjson.Valid(raw) {
		return ""
	}
	for _, path := range []string{"session_id", "thread_id"} {
		if value := strings.TrimSpace(gjson.Get(raw, path).String()); value != "" {
			return value
		}
	}
	return ""
}

// ApplyCodexFingerprintHeaders 按账号配置收敛出站请求头中的设备指纹。
// 必须在白名单透传（applyCodexAllowedForwardHeaders）之后调用，才能覆盖掉客户端原值；
// 也应在账号自定义头之前调用，让运维显式配置的头保持最终优先。
//
// downstreamHeaders 用于判断这次请求是否真的来自 Codex 客户端：非 Codex 下游
// （普通 OpenAI SDK 等）不会补发 x-codex-* 头，否则等于给一个本来没有 Codex 特征的
// 请求伪造出半套客户端特征，比不发更可疑。
func ApplyCodexFingerprintHeaders(outbound http.Header, account *auth.Account, downstreamHeaders http.Header) {
	if outbound == nil {
		return
	}
	ids := resolveCodexFingerprintIDs(account, downstreamHeaders)
	if ids == nil {
		return
	}

	if raw := strings.TrimSpace(outbound.Get(codexTurnMetadataHeader)); raw != "" {
		if rewritten, changed := rewriteCodexTurnMetadataJSON(raw, ids); changed {
			outbound.Set(codexTurnMetadataHeader, rewritten)
		}
	}

	// X-Client-Request-Id 按白名单原样透传（codexAllowedForwardHeaders）。实测真实
	// 客户端把它取成与自身 thread_id 相同的值，因此不处理就等于把下游用户的原始
	// 线程标识直接送到上游，与已收敛的 metadata 各说各话。只在它确实复用了客户端
	// 自报的会话/线程标识时改写；取值本就与身份无关时保持原样。
	if converged := convergedClientRequestID(outbound.Get(codexClientRequestIDHeader), ids, downstreamHeaders); converged != "" {
		outbound.Set(codexClientRequestIDHeader, converged)
	}

	// 仅对确有 Codex 引擎特征的下游请求处理标识头。
	if !EvaluateEngineFingerprint(downstreamHeaders, nil, nil) {
		return
	}
	// 只覆盖下游确实发过的标识头。实测真实 Codex CLI 只发 x-codex-window-id，
	// 从不发 x-codex-installation-id（installation_id 只存在于 turn metadata JSON 里）：
	// 无条件补发等于给请求添一个真实客户端不会有的头，与收敛目标相反。
	overrideExistingHeader(outbound, downstreamHeaders, codexInstallationIDHeader, ids.installationID)
	overrideExistingHeader(outbound, downstreamHeaders, codexWindowIDHeader, ids.windowID)
	// x-codex-parent-thread-id 与 metadata 里的 parent_thread_id 必须报同一个值：
	// 真实客户端这两处是同一份快照的两个投影（responses_metadata.rs
	// compatibility_headers），一处收敛一处不收敛就成了可直接比对的破绽。
	// device 档不做会话级收敛，与该档位其余字段保持一致。
	if ids.mode != auth.CodexFingerprintModeDevice && downstreamHeaders != nil {
		if original := strings.TrimSpace(downstreamHeaders.Get(codexParentThreadIDHeader)); original != "" {
			overrideExistingHeader(outbound, downstreamHeaders, codexParentThreadIDHeader,
				convergeCodexLineageValue(ids.accountID, "parent_thread_id", original))
		}
	}
}

// overrideExistingHeader 仅在下游确实发过该头时，用收敛值覆盖出站取值。
// 判定读下游头而非出站头：这两个标识头都不在 codexAllowedForwardHeaders 里，
// 下游即使发了，到这一步出站请求上也没有。
func overrideExistingHeader(outbound, downstreamHeaders http.Header, name, value string) {
	if value == "" || downstreamHeaders == nil {
		return
	}
	if strings.TrimSpace(downstreamHeaders.Get(name)) == "" {
		return
	}
	outbound.Set(name, value)
}

// convergedClientRequestID 在 X-Client-Request-Id 复用了客户端自报身份时返回对应的
// 收敛值，否则返回空串表示不改写。device 档没有会话级收敛值，恒不改写。
func convergedClientRequestID(current string, ids *codexFingerprintIDs, downstreamHeaders http.Header) string {
	current = strings.TrimSpace(current)
	if current == "" || ids == nil || ids.threadID == "" {
		return ""
	}
	clientSessionID, clientThreadID := extractClientCodexIdentity(downstreamHeaders)
	switch {
	case clientThreadID != "" && current == clientThreadID:
		return ids.threadID
	case clientSessionID != "" && current == clientSessionID:
		return ids.sessionID
	default:
		return ""
	}
}

// extractClientCodexIdentity 取下游自报的原始会话/线程标识，用于判断别的头是否
// 重复携带了同一个值。与 extractClientCodexSessionID 分开实现：后者是收敛值的派生
// 输入，改动它会让既有账号已经稳定的 thread_id 发生漂移。
func extractClientCodexIdentity(headers http.Header) (sessionID, threadID string) {
	if headers == nil {
		return "", ""
	}
	for _, name := range []string{"Session-Id", "Session_id"} {
		if value := strings.TrimSpace(headers.Get(name)); value != "" {
			sessionID = value
			break
		}
	}
	threadID = strings.TrimSpace(headers.Get(codexThreadIDHeader))
	raw := strings.TrimSpace(headers.Get(codexTurnMetadataHeader))
	if raw == "" || !gjson.Valid(raw) {
		return sessionID, threadID
	}
	if sessionID == "" {
		sessionID = strings.TrimSpace(gjson.Get(raw, "session_id").String())
	}
	if threadID == "" {
		threadID = strings.TrimSpace(gjson.Get(raw, "thread_id").String())
	}
	return sessionID, threadID
}

// ApplyCodexFingerprintToBody 收敛请求体 client_metadata 中的设备指纹。
// 请求体没有 client_metadata 时原样返回。
func ApplyCodexFingerprintToBody(body []byte, account *auth.Account, downstreamHeaders http.Header) []byte {
	ids := resolveCodexFingerprintIDs(account, downstreamHeaders)
	if ids == nil {
		return body
	}
	if !gjson.GetBytes(body, "client_metadata").IsObject() {
		return body
	}

	body = setExistingJSONString(body, "client_metadata.x-codex-installation-id", ids.installationID)
	if ids.mode != auth.CodexFingerprintModeDevice {
		body = setExistingJSONString(body, "client_metadata.session_id", ids.sessionID)
		body = setExistingJSONString(body, "client_metadata.thread_id", ids.threadID)
		body = setExistingJSONString(body, "client_metadata.x-codex-window-id", ids.windowID)
	}

	// client_metadata 里可能内嵌一份 turn metadata JSON 字符串，同样要收敛。
	const embeddedPath = "client_metadata.x-codex-turn-metadata"
	if embedded := gjson.GetBytes(body, embeddedPath); embedded.Type == gjson.String {
		if rewritten, changed := rewriteCodexTurnMetadataJSON(embedded.String(), ids); changed {
			if updated, err := sjson.SetBytes(body, embeddedPath, rewritten); err == nil {
				body = updated
			}
		}
	}
	return body
}

// rewriteCodexTurnMetadataJSON 收敛 turn metadata JSON 里的身份字段。
// 用 sjson 原地替换而非 map 反序列化再序列化：后者会按字典序重排键、丢掉原始键序，
// 这个形状差异本身就是可识别特征。未出现的键不会被补上。
func rewriteCodexTurnMetadataJSON(raw string, ids *codexFingerprintIDs) (string, bool) {
	if ids == nil || !gjson.Valid(raw) {
		return raw, false
	}
	updates := [][2]string{{"installation_id", ids.installationID}}
	if ids.mode != auth.CodexFingerprintModeDevice {
		updates = append(updates,
			[2]string{"session_id", ids.sessionID},
			[2]string{"thread_id", ids.threadID},
			[2]string{"window_id", ids.windowID},
		)
	}

	changed := false
	for _, update := range updates {
		path, value := update[0], update[1]
		if value == "" {
			continue
		}
		existing := gjson.Get(raw, path)
		if !existing.Exists() || existing.String() == value {
			continue
		}
		updated, err := sjson.Set(raw, path, value)
		if err != nil {
			continue
		}
		raw = updated
		changed = true
	}
	if ids.mode != auth.CodexFingerprintModeDevice {
		if rewritten, ok := convergeCodexLineageMetadata(raw, ids.accountID); ok {
			raw = rewritten
			changed = true
		}
	}
	if scrubbed, ok := scrubCodexWorkspaces(raw, ids.accountID); ok {
		raw = scrubbed
		changed = true
	}
	return raw, changed
}

// codexLineageMetadataPaths 是 turn metadata 里剩下的、会跨请求稳定地标识下游
// 会话/线程的字段（字段定义见 codex-rs/core/src/responses_metadata.rs
// CodexTurnMetadataPayload）。前面那批收敛只覆盖了 installation/session/thread/window
// 四个键，这些原样透传出去，等于换个字段泄漏同样的东西：
//
//   - context_window_id：Uuid::now_v7，会话内恒定、随 auto-compact 递增
//     （core/src/session/mod.rs current_window）。它跨请求稳定，单独一个字段
//     就能把同一个下游用户的多次请求串成一串，前面几项收敛随之失去意义。
//   - parent_thread_id / forked_from_thread_id：下游真实 ThreadId，语义与 thread_id
//     同级，泄漏面也相同。
//
// 刻意不含 parent_turn_id / root_turn_id：它们是轮次级标识，与本文件顶部对
// turn_id 的判断同理——逐轮变化、不标识设备或会话，重算反而会与同轮的
// turn_id / turn_started_at_unix_ms 失去一致性，那种不一致本身就是特征。
var codexLineageMetadataPaths = []string{
	"context_window_id",
	"parent_thread_id",
	"forked_from_thread_id",
}

// convergeCodexLineageMetadata 把谱系字段替换成按账号 + 原值派生的收敛值。
//
// 按原值派生（而非全部塌缩成一个常量）保住了形态：不同下游窗口/父线程在上游
// 看来仍是不同的值，这正是真实客户端的样子；变的只是它们不再等于下游的真实标识。
// 种子带 accountID，与本文件其它收敛值的惯例一致——否则同一个下游用户的同一
// 窗口在不同账号下会派生出相同占位值，上游据此就能把两个账号关联到同一个人。
//
// 只改写已存在且非空的字符串键，不新增、不改变 metadata 形状。
func convergeCodexLineageMetadata(raw string, accountID int64) (string, bool) {
	changed := false
	for _, path := range codexLineageMetadataPaths {
		existing := gjson.Get(raw, path)
		if existing.Type != gjson.String {
			continue
		}
		original := strings.TrimSpace(existing.String())
		if original == "" {
			continue
		}
		converged := convergeCodexLineageValue(accountID, path, original)
		if converged == original {
			continue
		}
		updated, err := sjson.Set(raw, path, converged)
		if err != nil {
			continue
		}
		raw = updated
		changed = true
	}
	return raw, changed
}

// convergeCodexLineageValue 为单个谱系标识派生收敛值。key 参与种子，因此同一个
// 原始 UUID 出现在不同字段上会得到不同结果——上游据此无法把"某人的父线程"和
// "某人的上下文窗口"认作同一个对象。
func convergeCodexLineageValue(accountID int64, key, original string) string {
	seed := fmt.Sprintf("codex2api:codex-lineage:v1:%s:%d:%s", key, accountID, original)
	return deriveStableCodexUUIDv7(seed, seededIdentityUnixMilli(seed))
}

// scrubCodexWorkspaces 抹掉 turn metadata 里的工作区身份。
//
// workspaces 以本地绝对路径为键（含系统用户名），条目里带 git remote URL 和 commit
// hash。多人共享一个账号时，这些字段会把每个下游用户的身份原样送到上游——同一个
// installation_id 下面挂着一堆互不相干的仓库地址和用户名目录，既是隐私泄漏，也让
// 前面几项标识的收敛失去意义。
//
// 处理方式是收敛而非删除：路径键换成按原值派生的占位符（条目数与互异性都保留，
// 不改变 metadata 形状），remote URL 整体移除，commit hash 换成派生占位值——
// commit hash 是全局唯一的仓库标记，公开仓库可直接反查到归属，留着它等于换个
// 字段泄漏 remote URL 同样的身份。
//
// 所有派生值都带 accountID：否则同一个下游用户的同一路径在不同账号下会得到相同
// 占位符，上游据此就能把两个账号关联到同一个人。这也与本文件其它收敛值的种子
// 惯例一致（见 resolveConvergedInstallationID）。
func scrubCodexWorkspaces(raw string, accountID int64) (string, bool) {
	workspaces := gjson.Get(raw, "workspaces")
	if !workspaces.IsObject() {
		return raw, false
	}

	rebuilt := "{}"
	changed := false
	failed := false
	workspaces.ForEach(func(key, value gjson.Result) bool {
		entry := value.Raw
		if gjson.Get(entry, "associated_remote_urls").Exists() {
			if updated, err := sjson.Delete(entry, "associated_remote_urls"); err == nil {
				entry = updated
				changed = true
			}
		}
		originalPath := key.String()
		placeholder := originalPath
		// 已是占位符就保持原样：重试链路可能把改写过的载荷再送进来一次，
		// 二次哈希会让同一工作区在不同尝试间漂移。
		if !isPlaceholderWorkspacePath(originalPath) {
			placeholder = placeholderWorkspacePath(accountID, originalPath)
			changed = true
		}
		// commit hash 的占位值从「已派生的占位路径」导出，而不是从原哈希导出：
		// 二次处理时路径已是占位符、导出结果不变，幂等性随之成立，同时不同工作区
		// 仍得到互不相同的哈希。
		if hash := gjson.Get(entry, "latest_git_commit_hash"); hash.Type == gjson.String && hash.String() != "" {
			if replacement := placeholderCommitHash(accountID, placeholder); replacement != hash.String() {
				if updated, err := sjson.Set(entry, "latest_git_commit_hash", replacement); err == nil {
					entry = updated
					changed = true
				}
			}
		}
		updated, err := sjson.SetRaw(rebuilt, placeholder, entry)
		if err != nil {
			failed = true
			return false
		}
		rebuilt = updated
		return true
	})
	if failed || !changed {
		return raw, false
	}

	updated, err := sjson.SetRaw(raw, "workspaces", rebuilt)
	if err != nil {
		return raw, false
	}
	return updated, true
}

// placeholderWorkspacePath 把本地路径映射成稳定占位符：同一路径恒定得到同一占位符，
// 不同路径互不相同，且不携带用户名或项目名。返回值不含 sjson 的路径元字符
// （. * ?），可直接作为 sjson 路径使用。
func placeholderWorkspacePath(accountID int64, original string) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "codex2api:workspace-path:v2:%d:%s", accountID, original))
	return workspacePlaceholderPrefix + fmt.Sprintf("%x", sum[:workspacePlaceholderDigestBytes])
}

// placeholderCommitHash 派生一个与真实 commit hash 等长（40 位十六进制）的占位值，
// 保持字段形状不变。种子取占位路径而非原哈希，见调用点说明。
func placeholderCommitHash(accountID int64, placeholderPath string) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "codex2api:workspace-commit:v1:%d:%s", accountID, placeholderPath))
	return fmt.Sprintf("%x", sum[:20])
}

const (
	workspacePlaceholderPrefix = "/workspace/"
	// 32 bit 对低熵的本地路径来说太窄——既可能撞键把两个工作区并成一个，
	// 也便于用候选路径表反查。64 bit 无额外代价。
	workspacePlaceholderDigestBytes = 8
)

// isPlaceholderWorkspacePath 判断路径是否已经是本函数产出的占位符，用于让抹除保持幂等。
func isPlaceholderWorkspacePath(path string) bool {
	suffix, ok := strings.CutPrefix(path, workspacePlaceholderPrefix)
	if !ok || len(suffix) != workspacePlaceholderDigestBytes*2 {
		return false
	}
	for _, r := range suffix {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}

// setExistingJSONString 只在目标路径已存在时替换其值，避免给客户端未携带该字段的
// 请求新增字段。
func setExistingJSONString(body []byte, path, value string) []byte {
	if value == "" {
		return body
	}
	existing := gjson.GetBytes(body, path)
	if !existing.Exists() || existing.String() == value {
		return body
	}
	updated, err := sjson.SetBytes(body, path, value)
	if err != nil {
		return body
	}
	return updated
}
