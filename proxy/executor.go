package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"golang.org/x/net/http2"
)

// Codex/OpenAI HTTP/2 上游的连接健康探测参数。
//
// 标准库 net/http 会 ALPN 协商升级到 HTTP/2，但 http2.Transport 默认
// ReadIdleTimeout=0（不发保活 PING），无法感知被代理/NAT 静默掐断的
// “死连接”（两端都以为存活）。请求一旦落在死连接上，会一直挂到 OS TCP
// 重传超时（分钟级）才失败——表现为超长 TTFT。启用主动 PING：连接空闲
// ReadIdleTimeout 后发 PING，PingTimeout 内无响应即判定死连接并关闭，
// 从源头剔除，请求得以在别的连接上重建，而非挂死。
//
// 仅作用于 HTTP/2 直连（标准 transport 与 uTLS transport）；WebSocket
// relay 链路已有完整的 Ping/Pong 保活与复用前探活，不走这里。
// 默认值对直连是合适的；经高延迟代理或弱网出口时 15s 偏激进——一次 PING 未按时
// 应答就会拆掉整条连接及其上全部复用中的流（表现为 "http2: client connection
// lost"）。故允许用 CODEX_HTTP2_READ_IDLE_TIMEOUT / CODEX_HTTP2_PING_TIMEOUT
// 覆盖（Go duration 写法，如 "45s"；填 0 关闭主动 PING，退回标准库默认）。
const (
	defaultCodexHTTP2ReadIdleTimeout = 15 * time.Second
	defaultCodexHTTP2PingTimeout     = 15 * time.Second
)

var (
	codexHTTP2ReadIdleTimeout = durationFromEnv("CODEX_HTTP2_READ_IDLE_TIMEOUT", defaultCodexHTTP2ReadIdleTimeout)
	codexHTTP2PingTimeout     = durationFromEnv("CODEX_HTTP2_PING_TIMEOUT", defaultCodexHTTP2PingTimeout)
)

// durationFromEnv 读取 Go duration 格式的环境变量；缺省或非法值回退默认值，
// 显式的 "0" 是合法输入（用于关闭对应机制）。
func durationFromEnv(key string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d < 0 {
		log.Printf("[CodexTransport] %s=%q 非法，沿用默认 %s", key, raw, fallback)
		return fallback
	}
	return d
}

// enableCodexHTTP2KeepAlive 在标准 *http.Transport 上显式配置 HTTP/2 并
// 开启连接健康探测（ReadIdleTimeout/PingTimeout），返回底层 *http2.Transport
// 便于测试断言。配置失败（如该 transport 已注册过 h2）不影响 h1 回退，仅记录
// 日志并返回 nil——此时沿用标准库默认（无主动 PING）。
func enableCodexHTTP2KeepAlive(transport *http.Transport) *http2.Transport {
	h2, err := http2.ConfigureTransports(transport)
	if err != nil {
		log.Printf("[CodexTransport] 启用 HTTP/2 保活失败，沿用默认(无 PING): err=%v", err)
		return nil
	}
	if h2 != nil {
		h2.ReadIdleTimeout = codexHTTP2ReadIdleTimeout
		h2.PingTimeout = codexHTTP2PingTimeout
	}
	return h2
}

// ==================== HTTP 连接池（按账号隔离 + TTL 淘汰） ====================
//
// 设计要点：
//   - 按账号隔离：避免同一 TCP 连接被不同 token 复用（会被服务端检测）
//   - TTL 淘汰：只有活跃账号持有连接，不活跃的自动清理，几万账号也不爆内存
//   - 空闲连接极简：每账号只保留 1 条空闲连接，空闲 30s 后自动关闭

// poolEntry 包装 http.Client，追踪最后使用时间用于 TTL 淘汰
type poolEntry struct {
	client    *http.Client
	lastUsed  atomic.Int64 // UnixNano 时间戳
	createdAt int64        // UnixNano，用于最大寿命轮转
	// rotatable 标记该 entry 可按寿命轮转。仅标准 transport 置位：轮转靠
	// “移出池 + 关空闲连接”实现，在途请求继续跑在旧连接上直到自然结束；
	// uTLS transport 自管连接池，释放路径会对在途连接发 GOAWAY 并在超时后
	// 强制关闭，按寿命定期触发反而可能截断长回答，故不参与轮转。
	rotatable bool
}

func (e *poolEntry) touch() {
	e.lastUsed.Store(time.Now().UnixNano())
}

func (e *poolEntry) expiredByAge(now int64, maxAge time.Duration) bool {
	if e == nil || !e.rotatable || maxAge <= 0 || e.createdAt == 0 {
		return false
	}
	return now-e.createdAt > int64(maxAge)
}

var clientPool sync.Map // map[string]*poolEntry, key = accountID|proxyURL|transportMode

// Some Codex-only Responses relays require the official client installation
// metadata in addition to the User-Agent. Learn that capability after the
// relay returns codex_access_restricted so generic Responses APIs stay untouched.
var openAIResponsesCodexMetadataRequired sync.Map // map[accountID|baseURL]struct{}

// clientPoolTTL 未使用超过此时间的 Client 将被淘汰
const clientPoolTTL = 5 * time.Minute

// clientPoolMaxAge 是池化 Client 的最大寿命：持续繁忙的账号其连接永远不会
// 空闲到触发 TTL 淘汰，一条 HTTP/2 连接可以承载上千个请求（issue #491 报告里
// 出现过 stream ID 539，即同一条连接已复用约 270 次）。上游边缘对超长寿命连接
// 有自己的回收策略，赶上回收窗口时在途流会被 RST。到点主动换新连接即可错开：
// 轮转只是把 entry 移出池并关闭其空闲连接，新请求走新连接，在途请求不受影响。
// WS 链路已有同类机制（50 分钟主动轮转，issue #346）。设为 0 关闭本机制。
var clientPoolMaxAge = durationFromEnv("CODEX_HTTP_CLIENT_MAX_AGE", 30*time.Minute)

// clientPoolCleanupInterval 清理协程执行间隔
const clientPoolCleanupInterval = 60 * time.Second

func init() {
	// 后台清理：每 60 秒扫描一次，淘汰过期的 Client
	go func() {
		ticker := time.NewTicker(clientPoolCleanupInterval)
		defer ticker.Stop()
		for range ticker.C {
			evictExpiredClients()
		}
	}()
}

func evictExpiredClients() {
	now := time.Now()
	cutoff := now.Add(-clientPoolTTL).UnixNano()
	nowNanos := now.UnixNano()
	clientPool.Range(func(key, value any) bool {
		entry := value.(*poolEntry)
		idle := entry.lastUsed.Load() < cutoff
		aged := entry.expiredByAge(nowNanos, clientPoolMaxAge)
		if !idle && !aged {
			return true
		}
		// LoadAndDelete 保证只有一个清理者真正释放这个 entry；被并发的
		// getPooledClient 换上的新 entry 不会被误删。
		if actual, ok := clientPool.LoadAndDelete(key); ok {
			released := actual.(*poolEntry)
			releaseEvictedClient(released.client)
			if aged && !idle {
				log.Printf("[CodexTransport] 连接寿命到期轮转: key=%s age=%s", key, time.Duration(nowNanos-released.createdAt).Truncate(time.Second))
			}
		}
		return true
	})
}

// releaseEvictedClient 彻底释放一个已从连接池逐出、后续不会再被取用的 Client。
//
// 普通 transport 用 CloseIdleConnections 即可（剩下的在途请求结束后由
// IdleConnTimeout 回收）。但 uTLS transport 自管连接池，此处需要连带在途连接
// 一起摘掉（在途 stream 走优雅关闭）：否则 entry 一旦从 map 删除，就再没有
// 任何人持有该 transport，它名下的连接会泄漏到进程结束（issue #446）。
func releaseEvictedClient(client *http.Client) {
	if client == nil {
		return
	}
	if rt, ok := client.Transport.(*utlsRoundTripper); ok {
		rt.CloseAllConnections()
		return
	}
	client.CloseIdleConnections()
}

const (
	codexTransportModeStandard   = "standard"
	codexTransportModeUTLSChrome = "utls_chrome"
)

func codexTransportModeFromEnv() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CODEX_TRANSPORT_MODE"))) {
	case "", "standard", "go", "default":
		return codexTransportModeStandard
	case "utls", "utls_chrome", "chrome":
		return codexTransportModeUTLSChrome
	default:
		return codexTransportModeStandard
	}
}

func clientPoolKey(account *auth.Account, proxyURL, transportMode string) string {
	return fmt.Sprintf("%d|%s|%s", account.ID(), strings.TrimSpace(proxyURL), transportMode)
}

func shouldRecyclePooledClient(err error) bool {
	if err == nil {
		return false
	}

	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection is shutting down") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "broken pipe")
}

func recyclePooledClient(account *auth.Account, proxyURL string) {
	key := clientPoolKey(account, proxyURL, codexTransportModeFromEnv())
	if v, ok := clientPool.LoadAndDelete(key); ok {
		releaseEvictedClient(v.(*poolEntry).client)
	}
}

func recyclePooledClientForAccount(account *auth.Account) {
	if account == nil {
		return
	}

	account.Mu().RLock()
	proxyURL := account.ProxyURL
	account.Mu().RUnlock()
	recyclePooledClient(account, proxyURL)
}

// codexTLSSessionCache 在所有标准 transport 间共享 TLS 会话缓存，
// 让重连(连接池 TTL 淘汰或 30s 空闲关闭后)走 TLS resumption(1-RTT)，降低重连握手成本。
var codexTLSSessionCache = tls.NewLRUClientSessionCache(256)

func newCodexStandardTransport(proxyURL string) http.RoundTripper {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = 4
	transport.IdleConnTimeout = 90 * time.Second
	// 兜住"连接建立后上游迟迟不回响应头"的假死场景。响应头（含 SSE 的
	// 200 头）在正常情况下远早于首 token 到达，5 分钟已非常宽裕。
	transport.ResponseHeaderTimeout = 5 * time.Minute
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{}
	}
	transport.TLSClientConfig.ClientSessionCache = codexTLSSessionCache
	baseDialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	transport.DialContext = baseDialer.DialContext
	if err := auth.ConfigureTransportProxy(transport, proxyURL, baseDialer); err != nil {
		log.Printf("[CodexTransport] 代理配置失败，回退直连: proxy=%s err=%v", proxyURL, err)
		transport.Proxy = nil
		transport.DialContext = baseDialer.DialContext
	}
	// 在代理/DialContext 敲定后再启用 HTTP/2 保活 PING，剔除被中间设备静默
	// 掐断的死连接，避免请求挂到 TCP 重传超时。
	enableCodexHTTP2KeepAlive(transport)
	return transport
}

func newCodexTransport(proxyURL string) http.RoundTripper {
	switch codexTransportModeFromEnv() {
	case codexTransportModeUTLSChrome:
		return NewUTLSTransport(proxyURL)
	default:
		return newCodexStandardTransport(proxyURL)
	}
}

func codexFingerprintDebugEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CODEX_FINGERPRINT_DEBUG"))) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

func shortHashForLog(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:6])
}

func logCodexFingerprintDebug(kind string, account *auth.Account, proxyURL string, headers http.Header) {
	if !codexFingerprintDebugEnabled() {
		return
	}
	accountID := int64(0)
	if account != nil {
		accountID = account.ID()
	}
	userAgent := strings.TrimSpace(headers.Get("User-Agent"))
	originator := strings.TrimSpace(headers.Get("Originator"))
	log.Printf("[CodexFingerprint] kind=%s account_id=%d transport_mode=%s proxy_enabled=%t official_client=%t ua_hash=%s originator=%s session_hash=%s stainless_present=%t",
		kind,
		accountID,
		codexTransportModeFromEnv(),
		strings.TrimSpace(proxyURL) != "",
		IsCodexOfficialClientByHeaders(userAgent, originator),
		shortHashForLog(userAgent),
		originator,
		shortHashForLog(headers.Get("Session_id")),
		headers.Get("X-Stainless-Package-Version") != "" ||
			headers.Get("X-Stainless-Runtime-Version") != "" ||
			headers.Get("X-Stainless-Os") != "" ||
			headers.Get("X-Stainless-Arch") != "",
	)
}

// getPooledClient 获取或创建连接池中的 HTTP Client（按账号隔离，TTL 自动淘汰）
func getPooledClient(account *auth.Account, proxyURL string) *http.Client {
	transportMode := codexTransportModeFromEnv()
	key := clientPoolKey(account, proxyURL, transportMode)
	if v, ok := clientPool.Load(key); ok {
		entry := v.(*poolEntry)
		entry.touch()
		return entry.client
	}

	transport := newCodexTransport(proxyURL)

	entry := &poolEntry{
		createdAt: time.Now().UnixNano(),
		rotatable: transportMode == codexTransportModeStandard,
		client: &http.Client{
			Transport: transport,
			// 不设整体超时：http.Client.Timeout 覆盖包括读响应体在内的完整
			// 生命周期，流式回答超过上限会在数据正常传输中被切断（issue #287，
			// 复杂任务单回合可超过 10 分钟）。生命周期由请求 context 控制
			// （下游断开即取消），假死场景由拨号超时 + ResponseHeaderTimeout
			// + 流层断流检测兜底，与 uTLS 路径(NewUTLSHttpClient)语义一致。
			Timeout: 0,
		},
	}
	entry.touch()

	if v, loaded := clientPool.LoadOrStore(key, entry); loaded {
		e := v.(*poolEntry)
		e.touch()
		return e.client
	}
	return entry.client
}

// Codex 上游常量
const (
	CodexBaseURL                     = "https://chatgpt.com/backend-api/codex"
	Originator                       = "codex-tui"
	codexResponsesLiteHeader         = "X-OpenAI-Internal-Codex-Responses-Lite"
	codexResponsesLiteWSMetadataPath = "client_metadata.ws_request_header_x_openai_internal_codex_responses_lite"
)

const (
	codexBetaFeaturesHeader = "X-Codex-Beta-Features"
	// defaultCodexBetaFeatures 是默认安装的真实 Codex 发出的会话级特性协商值。
	defaultCodexBetaFeatures = "remote_compaction_v2"
)

var codexAllowedForwardHeaders = []string{
	"X-Codex-Turn-State",
	"X-Codex-Turn-Metadata",
	"X-Client-Request-Id",
	"X-Codex-Beta-Features",
	codexResponsesLiteHeader,
	// DeviceCheck 设备认证头（上游 openai/codex#20619）。仅在下游真实 Codex
	// 客户端携带时原样透传——本代理无法（也不该）伪造：token 是 Apple 硬件
	// 背书、服务端向 Apple 验证，假值必然验证失败、比"不携带"更暴露特征。
	// 缺失是合法状态（纯 CLI / 非 macOS 客户端本就不发）。
	"X-Oai-Attestation",
}

func codexResponsesLiteRequested(requestBody []byte, headers http.Header) bool {
	if headers != nil && strings.EqualFold(strings.TrimSpace(headers.Get(codexResponsesLiteHeader)), "true") {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(gjson.GetBytes(requestBody, codexResponsesLiteWSMetadataPath).String()), "true")
}

// prepareCodexResponsesLiteTransport keeps the request-scoped Responses Lite
// signal intact when codex2api changes the upstream transport. HTTP carries the
// signal in a header; WebSocket carries it on each response.create frame so a
// pooled connection can safely serve both Lite and non-Lite requests.
func prepareCodexResponsesLiteTransport(requestBody []byte, headers http.Header, useWebsocket, enabled bool) ([]byte, http.Header) {
	forwardHeaders := headers
	headersCloned := false
	if headers != nil && headers.Get(codexResponsesLiteHeader) != "" {
		forwardHeaders = headers.Clone()
		headersCloned = true
		forwardHeaders.Del(codexResponsesLiteHeader)
	}
	if useWebsocket {
		if enabled {
			updated, err := sjson.SetBytes(requestBody, codexResponsesLiteWSMetadataPath, "true")
			if err == nil {
				requestBody = updated
			}
		} else if updated, err := sjson.DeleteBytes(requestBody, codexResponsesLiteWSMetadataPath); err == nil {
			// 信号被模型门禁剥离（或下游标记非 true）时，清掉体内残留标记，
			// 避免不支持 lite 的模型把标记带上 WS 上游触发 400。
			requestBody = updated
		}
		return requestBody, forwardHeaders
	}

	if updated, err := sjson.DeleteBytes(requestBody, codexResponsesLiteWSMetadataPath); err == nil {
		requestBody = updated
	}
	if enabled {
		if !headersCloned {
			forwardHeaders = headers.Clone()
		}
		if forwardHeaders == nil {
			forwardHeaders = make(http.Header)
		}
		forwardHeaders.Set(codexResponsesLiteHeader, "true")
	}
	return requestBody, forwardHeaders
}

// normalizeCodexResponsesLiteBody 按上游对 Responses Lite 请求的强制约束净化请求体：
// reasoning.context 必须为 all_turns、parallel_tool_calls 必须为 false，否则上游
// 400 unsupported_value（WS/HTTP 均校验）。HTTP 上游还要求 tools 仅含 function/
// custom/客户端执行的 tool search——网关自动注入的 hosted image_generation 工具
// （及其桥接 instructions）会让整个请求被拒，须在发出前剥除；WS 上游接受该工具、
// 生图桥接可用，不剥除以免功能回退。
func normalizeCodexResponsesLiteBody(requestBody []byte, stripHostedTools bool) []byte {
	requestBody, _ = sjson.SetBytes(requestBody, "parallel_tool_calls", false)
	requestBody, _ = sjson.SetBytes(requestBody, "reasoning.context", "all_turns")
	if !stripHostedTools {
		return requestBody
	}

	if tools := gjson.GetBytes(requestBody, "tools"); tools.IsArray() {
		kept := make([]json.RawMessage, 0, len(tools.Array()))
		removed := false
		for _, tool := range tools.Array() {
			if strings.EqualFold(strings.TrimSpace(tool.Get("type").String()), "image_generation") {
				removed = true
				continue
			}
			kept = append(kept, json.RawMessage(tool.Raw))
		}
		if removed {
			if len(kept) == 0 {
				requestBody, _ = sjson.DeleteBytes(requestBody, "tools")
			} else if raw, err := json.Marshal(kept); err == nil {
				requestBody, _ = sjson.SetRawBytes(requestBody, "tools", raw)
			}
			if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(requestBody, "tool_choice.type").String()), "image_generation") {
				requestBody, _ = sjson.DeleteBytes(requestBody, "tool_choice")
			}
		}
	}
	if instructions := gjson.GetBytes(requestBody, "instructions").String(); strings.Contains(instructions, codexImageGenerationBridgeMarker) {
		requestBody, _ = sjson.SetBytes(requestBody, "instructions", removeCodexImageGenerationBridgeText(instructions))
	}
	return requestBody
}

// WebsocketExecuteFunc WebSocket 执行函数（由 wsrelay 包在 main.go 中注册，避免循环依赖）
// poolRouteKey：本地连接池路由键（仅本地、永不发上游）。非空时 wsrelay 用它作 8 槽池的
// baseKey，从而把"上游会话身份(每请求唯一)"与"连接复用(按 API Key 稳定)"解耦；空时沿用
// headerSessionID 作 baseKey（显式会话 / per-api-key 模式的原有行为）。
var WebsocketExecuteFunc func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error)

// EnsureCodexAgentIdentityTaskFunc 由启动装配注册（store.EnsureCodexAgentIdentityTask），
// 在构建 Agent Identity 请求前保证 task_id 就绪；forceRefresh=true 用于 401 task 失效重注册。
// nil 时跳过（如嵌入式调用或初始化顺序问题）。
var EnsureCodexAgentIdentityTaskFunc func(ctx context.Context, account *auth.Account, forceRefresh bool) error

// IsolateCodexSessionID 把下游会话种子确定性映射到一个按 API Key 隔离的上游会话身份。
//
// 产出取 UUIDv7 形态：真实 Codex 客户端的 session_id 就是 v7，而这里原本产出的是
// 16 位裸十六进制，连 UUID 都不是——它同时出现在出站 session 头和请求体
// prompt_cache_key 上，是比任何 metadata 字段都显眼的形状差异。
//
// 换格式会让既有部署的确定性 cache key 整体换代，升级后首轮请求上游 prompt cache
// 必然 miss 一次，随后按新键重新聚合。这是一次性成本，换的是每个请求的形状正确。
func IsolateCodexSessionID(apiKeyID int64, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || apiKeyID <= 0 {
		return raw
	}
	return DeriveStableSessionUUIDv7(fmt.Sprintf("api-key:%d:%s", apiKeyID, raw))
}

// resolveUpstreamSessionID 决定传给上游的会话/缓存身份键。
//   - 显式会话（用户带了 Session_id/Conversation_id/Idempotency-Key/prompt_cache_key）：
//     保持 IsolateCodexSessionID 的确定性隔离行为，命中缓存、粘定会话。
//   - 无显式会话 + 默认隔离(isolated)：HTTP 返回每请求唯一 UUID（隔离上游 prompt_cache_key/
//     Session_id），WS 返回 ""（交给 ExecuteRequest 的 stateless 路径，连接池键单独稳定）。
//   - 无显式会话 + per-api-key：WS 返回 ""、HTTP 走 IsolateCodexSessionID（恢复旧的按 Key 共享）。
//
// 注意：账号粘性键由 requestSessionIdentity.affinityID 派生；本函数只接收
// upstreamSeed，因此本地 affinity header 不会影响上游身份。
func resolveUpstreamSessionID(apiKeyID int64, upstreamSeed, explicitSessionID string, useWebsocket bool) string {
	if useWebsocket && explicitSessionID == "" {
		return ""
	}
	if explicitSessionID == "" && CurrentRuntimeSettings().IsolateRequestsByDefault() {
		// v7 而非 v4：这是默认路径，绝大多数出站请求的会话键都由这里产出。
		return NewUpstreamSessionUUID()
	}
	return IsolateCodexSessionID(apiKeyID, upstreamSeed)
}

// ExecuteRequest 向 Codex 上游发送请求
// sessionID 可选，用于 prompt cache 会话绑定
// useWebsocket 可选：未传时遵循全局强制 WS；传 true/false 时由调用方显式控制。
// headers 下游请求头，用于设备指纹学习
func ExecuteRequest(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, useWebsocket ...bool) (upstreamResponse *http.Response, upstreamErr error) {
	// Defense in depth: this executor sends account.AccessToken to ChatGPT.
	// Relay/Grok/Antigravity credentials must never cross that provider boundary,
	// even if a future routing regression selects the wrong account type.
	if account == nil || account.IsRelayStyle() {
		return nil, ErrNoAvailableAccount()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	resetUpstreamUserAgentAudit(ctx)
	resetWsAcquireAudit(ctx)
	var encryptedAttempt *encryptedContentAttempt
	requestBody, encryptedAttempt = prepareEncryptedContentAttempt(ctx, account, requestBody, sessionID, headers)
	defer func() { encryptedAttempt.observeResponse(upstreamResponse, requestBody) }()

	// Payload 规则改写：在 WS/HTTP 分叉前统一应用，两条上游路径共享改写结果。
	// 生图请求跳过——其 instructions/工具由网关自行构造，改写会破坏桥接协议。
	if !responsesBodyRequestsImageGeneration(requestBody) {
		RecordObservedInstructions(requestBody, headers)
		requestBody = ApplyPayloadRulesToBody(requestBody, gjson.GetBytes(requestBody, "model").String(), headers, PayloadRuleIdentityFromContext(ctx))
		// 规则改写发生在各 handler 的 service_tier 净化之后，规则注入的 flex/auto 等
		// 上游不接受的层级会原样发出并触发 400，这里补一次净化兜底。用量日志的
		// requested tier 归因走 EffectiveRequestedServiceTier（净化前取值），不受影响。
		requestBody = sanitizeServiceTierForUpstream(requestBody)
	}
	// 指纹收敛在 WS/HTTP 分叉前统一改写请求体，两条上游路径共享结果；请求头侧的
	// 收敛（ApplyCodexFingerprintHeaders）从同一份「账号 + 下游头」推导，取值一致。
	requestBody = ApplyCodexFingerprintToBody(requestBody, account, headers)
	// lite 信号收敛：签名在 payload 规则改写后采集（规则可注入/删除 WS 标记，改写
	// 前采集会让注入失效、删除被回填），模型也已被入口映射/规则定稿——已知不支持
	// lite 的模型带信号上游必 400，发出前剥离。
	responsesLite := gateResponsesLiteForAccount(codexResponsesLiteRequested(requestBody, headers), requestBody, account)
	wantWebsocket := CurrentRuntimeSettings().CodexForceWebsocket
	if len(useWebsocket) > 0 {
		wantWebsocket = useWebsocket[0]
	}
	// Agent Identity 账号强制走 HTTP：其鉴权是每请求动态签名的 AgentAssertion 头，
	// 长连接 WS 的一次握手鉴权模型不适配，v1 统一走 HTTP。
	if account.IsCodexAgentIdentity() {
		wantWebsocket = false
	}
	poolRouteKey := ""
	if wantWebsocket {
		sessionID = strings.TrimSpace(sessionID)
		if sessionID == "" {
			// stateless 连接 ID 仅用于 WS 连接池隔离，保证同一 API Key 的并发请求
			// 不挤在一条连接上排队。
			sessionID = statelessWebsocketSessionID()
			if strings.TrimSpace(gjson.GetBytes(requestBody, "prompt_cache_key").String()) == "" {
				det := deterministicPromptCacheKey(apiKey, account)
				if CurrentRuntimeSettings().IsolateRequestsByDefault() {
					// 默认隔离：每请求唯一的 prompt_cache_key 写入 response.create 帧体，实现上游
					// 身份隔离（互不串味）；连接池 baseKey 用稳定的确定性键单独传，保住 8 槽复用与
					// 抗握手限流(503)。注意：上游会话隔离靠帧体 prompt_cache_key，而非握手头
					// Session_id/Conversation_id（后者对复用连接是逐连接、非逐请求）。
					requestBody, _ = sjson.SetBytes(requestBody, "prompt_cache_key", NewUpstreamSessionUUID())
					poolRouteKey = det
					if poolRouteKey == "" {
						// det 仅在既无 API Key 又无账号 ID 时为空（生产路径不可达）；用固定哨兵兜底，
						// 避免 baseKey 退化为每请求唯一键而触发握手风暴。
						poolRouteKey = "ws-pool-default"
					}
				} else if det != "" {
					// per-api-key：保留与 HTTP 路径同源的确定性 prompt cache key（既是上游身份也是
					// baseKey），否则上游 prompt cache 每次请求都会 miss（v2.2.7 引入的回归）。
					requestBody, _ = sjson.SetBytes(requestBody, "prompt_cache_key", det)
				}
			}
		}
	}
	if wantWebsocket && WebsocketExecuteFunc != nil {
		requestBody, headers = prepareCodexResponsesLiteTransport(requestBody, headers, true, responsesLite)
		if responsesLite {
			requestBody = normalizeCodexResponsesLiteBody(requestBody, false)
		}
		// 出站前最后兜底：任何中间改写都不能把普通 input 项放到
		// compaction_trigger 后面，否则上游直接返回 invalid_request_error。
		requestBody = normalizeCompactionTriggerFinal(requestBody, false)
		traceProxy := proxyOverride
		if traceProxy == "" {
			account.Mu().RLock()
			traceProxy = account.ProxyURL
			account.Mu().RUnlock()
		}
		recordTrace := beginUpstreamTrace(ctx, account, traceProxy, true)
		resp, err := WebsocketExecuteFunc(ctx, account, requestBody, sessionID, proxyOverride, apiKey, deviceCfg, headers, poolRouteKey)
		recordTrace(resp)
		return resp, err
	}
	if wantWebsocket && WebsocketExecuteFunc == nil {
		// 请求/配置要求走 WebSocket，但 WS 执行器未注册（如嵌入式调用或初始化顺序问题）。
		// 静默落回 HTTP 会让“以为开了 WS 实际走 HTTP”难以排查，这里显式告警。
		log.Printf("[WS] 警告: 期望走 WebSocket 上游，但 WebsocketExecuteFunc 未注册，已回退到 HTTP (account %d)", account.ID())
	}
	requestBody, headers = prepareCodexResponsesLiteTransport(requestBody, headers, false, responsesLite)
	if responsesLite {
		requestBody = normalizeCodexResponsesLiteBody(requestBody, true)
	}
	requestBody = normalizeCompactionTriggerFinal(requestBody, false)

	account.Mu().RLock()
	accessToken := account.AccessToken
	proxyURL := account.ProxyURL
	account.Mu().RUnlock()

	// 代理池优先级: proxyOverride (来自 NextProxy) > account.ProxyURL
	if proxyOverride != "" {
		proxyURL = proxyOverride
	}

	isAgentIdentity := account.IsCodexAgentIdentity()
	// Agent Identity 无 access_token，鉴权靠 AgentAssertion；请求前确保 task 已注册。
	if isAgentIdentity {
		if EnsureCodexAgentIdentityTaskFunc != nil {
			if err := EnsureCodexAgentIdentityTaskFunc(ctx, account, false); err != nil {
				return nil, ErrUpstream(0, "agent identity task 注册失败", err)
			}
		}
	} else if accessToken == "" {
		return nil, ErrNoAvailableAccount()
	}

	// ==================== Codex 请求体优化 ====================
	// 参考 CLIProxyAPI/codex_executor.go + sub2api 的实现

	// 1. 确保 instructions 字段存在（Codex 后端要求）
	if !gjson.GetBytes(requestBody, "instructions").Exists() {
		requestBody, _ = sjson.SetBytes(requestBody, "instructions", "")
	}

	// 2. 清理可能导致上游报错的多余字段
	requestBody, _ = sjson.DeleteBytes(requestBody, "previous_response_id")
	// 注意：HTTP /responses 上游不接受 prompt_cache_retention（会 400），必须删除；
	// 该字段的 cache 收益只在 WS 路径注入（见 wsrelay 的 prepareWebsocketBody）。
	requestBody, _ = sjson.DeleteBytes(requestBody, "prompt_cache_retention")
	requestBody, _ = sjson.DeleteBytes(requestBody, "safety_identifier")
	requestBody, _ = sjson.DeleteBytes(requestBody, "disable_response_storage")
	// 顶层 type 是 Responses WS 事件信封字段（response.create），native WS ingress 的
	// 1009 降级、生图强制 HTTP、Agent Identity 强制 HTTP 都会复用带信封的 body，而
	// HTTP /responses 上游不接受它（400 Unsupported parameter: type）。此处为出站
	// 收口兜底；sjson 只删顶层路径，input[] 等嵌套 type 不受影响（issue #548）。
	requestBody, _ = sjson.DeleteBytes(requestBody, "type")

	// 3. 注入 prompt_cache_key（如果请求体中没有，且 sessionID 不为空）
	existingCacheKey := strings.TrimSpace(gjson.GetBytes(requestBody, "prompt_cache_key").String())
	cacheKey := existingCacheKey
	if sessionID != "" {
		cacheKey = sessionID
		requestBody, _ = sjson.SetBytes(requestBody, "prompt_cache_key", cacheKey)
	}

	endpoint := CodexBaseURL + "/responses"

	// 出站字节在选客户端之前定稿：send() 会因 Agent Identity 401 重注册而重放，
	// 两次重放必须发同一份字节。routing hint 等需要读字段的改写点继续用明文
	// requestBody——它们解析 JSON，拿到压缩帧只会静默失配。
	outboundBody, contentEncoding := CompressCodexRequestBody(requestBody)

	// Resin 反向代理模式：改写 URL，使用标准 HTTP 客户端
	var client *http.Client
	if IsResinEnabled() {
		endpoint = BuildReverseProxyURL(endpoint)
		client = getResinHTTPClient(account)
	} else {
		client = getPooledClient(account, proxyURL)
	}

	send := func() (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(outboundBody))
		if err != nil {
			return nil, ErrInternalError("创建请求失败", err)
		}

		// ==================== 请求头（伪装 Codex CLI） ====================
		applyCodexRequestHeaders(req, account, accessToken, cacheKey, apiKey, deviceCfg, headers)
		// Content-Encoding 在通用头装配之后设置：真实客户端也是在编码完成时才补这个头
		// （codex-rs/http-client/src/request.rs prepare_encoded_json），且账号自定义头
		// 不该有能力声明一个与实际字节不符的编码。
		if contentEncoding != "" {
			req.Header.Set("Content-Encoding", contentEncoding)
		}
		// routing hint 由网关按最终出站 body 合成，须在账号自定义头之后设置。
		ApplyCodexRoutingHint(req.Header, account, requestBody)

		// Resin 反代：注入账号身份头
		if IsResinEnabled() {
			req.Header.Set("X-Resin-Account", ResinAccountID(account))
		}
		logCodexFingerprintDebug("http", account, proxyURL, req.Header)

		if err := ConsumeAPIKeyModelRequestQuota(ctx, gjson.GetBytes(requestBody, "model").String()); err != nil {
			return nil, err
		}
		resp, err := doTracedUpstreamRequest(client, req, account, proxyURL)
		if err != nil {
			if shouldRecyclePooledClient(err) {
				recyclePooledClient(account, proxyURL)
			}
			return nil, ErrUpstream(0, "请求上游失败", err)
		}
		return resp, nil
	}

	resp, err := send()
	if err != nil {
		return nil, err
	}

	// Agent Identity：task 失效（401 invalid_task_id 等）时重注册并重试一次。
	if isAgentIdentity && resp != nil && resp.StatusCode == http.StatusUnauthorized && EnsureCodexAgentIdentityTaskFunc != nil {
		peeked, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
		_ = resp.Body.Close()
		if auth.IsAgentIdentityTaskInvalidResponse(resp.StatusCode, peeked) {
			if regErr := EnsureCodexAgentIdentityTaskFunc(ctx, account, true); regErr == nil {
				return send()
			}
		}
		// 非 task 失效或重注册失败：把已读走的 body 还原后原样返回给上层错误处理。
		resp.Body = io.NopCloser(bytes.NewReader(peeked))
	}

	return resp, nil
}

func ExecuteOpenAIResponsesRequest(ctx context.Context, account *auth.Account, requestBody []byte, proxyOverride string, headers http.Header) (upstreamResponse *http.Response, upstreamErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	resetUpstreamUserAgentAudit(ctx)
	resetWsAcquireAudit(ctx)
	var encryptedAttempt *encryptedContentAttempt
	requestBody, encryptedAttempt = prepareEncryptedContentAttempt(ctx, account, requestBody, "", headers)
	defer func() { encryptedAttempt.observeResponse(upstreamResponse, requestBody) }()
	responsesLite := gateResponsesLiteForAccount(codexResponsesLiteRequested(requestBody, headers), requestBody, account)
	requestBody, headers = prepareCodexResponsesLiteTransport(requestBody, headers, false, responsesLite)
	requestBody = normalizeCompactionTriggerFinal(requestBody, false)

	baseURL, apiKey := account.OpenAIResponsesCredentials()
	account.Mu().RLock()
	proxyURL := account.ProxyURL
	account.Mu().RUnlock()
	if proxyOverride != "" {
		proxyURL = proxyOverride
	}
	if baseURL == "" || apiKey == "" {
		return nil, ErrNoAvailableAccount()
	}

	endpoint := auth.OpenAIResponsesEndpoint(baseURL, "/v1/responses")
	capabilityKey := openAIResponsesCodexMetadataCapabilityKey(account, baseURL)
	clientMetadataMode := account.OpenAIResponsesCodexClientMetadataMode()
	proxyInjectedMetadata := false
	if clientMetadataMode == auth.CodexClientMetadataModeAlways {
		requestBody, proxyInjectedMetadata = ensureCodexClientInstallationMetadata(requestBody, account, headers)
	} else if clientMetadataMode == auth.CodexClientMetadataModeAuto {
		_, required := openAIResponsesCodexMetadataRequired.Load(capabilityKey)
		if required {
			requestBody, proxyInjectedMetadata = ensureCodexClientInstallationMetadata(requestBody, account, headers)
		}
	}

	client := getPooledClient(account, proxyURL)
	send := func(body []byte) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, ErrInternalError("创建请求失败", err)
		}
		applyOpenAIResponsesRequestHeaders(req, account, apiKey, headers)
		if err := ConsumeAPIKeyModelRequestQuota(ctx, gjson.GetBytes(body, "model").String()); err != nil {
			return nil, err
		}
		resp, err := doTracedUpstreamRequest(client, req, account, proxyURL)
		if err != nil {
			if shouldRecyclePooledClient(err) {
				recyclePooledClient(account, proxyURL)
			}
			return nil, ErrUpstream(0, "请求 OpenAI Responses API 失败", err)
		}
		return resp, nil
	}

	resp, err := send(requestBody)
	if err != nil {
		return nil, err
	}
	if clientMetadataMode == auth.CodexClientMetadataModeOff || clientMetadataMode == auth.CodexClientMetadataModeAlways {
		return resp, nil
	}
	if proxyInjectedMetadata {
		if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
			openAIResponsesCodexMetadataRequired.Store(capabilityKey, struct{}{})
		}
		return resp, nil
	}
	if codexClientInstallationID(requestBody) != "" {
		// A pass-through client ID does not prove that this relay requires generated metadata.
		return resp, nil
	}
	if !isCodexAccessRestrictedResponse(resp) {
		return resp, nil
	}

	retryBody, injected := ensureCodexClientInstallationMetadata(requestBody, account, headers)
	if !injected {
		return resp, nil
	}
	_ = resp.Body.Close()
	retryResp, err := send(retryBody)
	if err != nil {
		return nil, err
	}
	if retryResp.StatusCode >= http.StatusOK && retryResp.StatusCode < http.StatusMultipleChoices {
		openAIResponsesCodexMetadataRequired.Store(capabilityKey, struct{}{})
	}
	return retryResp, nil
}

func openAIResponsesCodexMetadataCapabilityKey(account *auth.Account, baseURL string) string {
	accountID := int64(0)
	if account != nil {
		accountID = account.ID()
	}
	return fmt.Sprintf("%d|%s", accountID, strings.ToLower(strings.TrimSpace(baseURL)))
}

func codexClientInstallationID(requestBody []byte) string {
	return strings.TrimSpace(gjson.GetBytes(requestBody, "client_metadata.x-codex-installation-id").String())
}

func ensureCodexClientInstallationMetadata(requestBody []byte, account *auth.Account, headers http.Header) ([]byte, bool) {
	if !gjson.ValidBytes(requestBody) || codexClientInstallationID(requestBody) != "" {
		return requestBody, false
	}

	seed := ""
	if headers != nil {
		seed = strings.TrimSpace(headers.Get("Authorization"))
	}
	if seed == "" && account != nil {
		baseURL, apiKey := account.OpenAIResponsesCredentials()
		seed = fmt.Sprintf("%d|%s|%s", account.ID(), baseURL, apiKey)
	}
	if seed == "" {
		seed = "default"
	}
	installationID := uuid.NewSHA1(uuid.NameSpaceOID, []byte("codex2api:client-installation:"+seed)).String()
	updatedBody, err := sjson.SetBytes(requestBody, "client_metadata.x-codex-installation-id", installationID)
	if err != nil {
		return requestBody, false
	}
	return updatedBody, true
}

func isCodexAccessRestrictedResponse(resp *http.Response) bool {
	if resp == nil || resp.StatusCode != http.StatusForbidden || resp.Body == nil {
		return false
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))
	resp.ContentLength = int64(len(body))
	if err != nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(gjson.GetBytes(body, "error.code").String()), "codex_access_restricted")
}

// ExecuteOpenAIResponsesCompactRequest 向中转（OpenAI Responses API）账号发送
// /responses/compact 请求。与 ExecuteOpenAIResponsesRequest 行为一致，但命中的是
// 上游自己的 compact 端点，从而让没有官方 Codex OAuth 账号、仅接入中转的用户也能
// 触发上下文自动压缩（参见 issue #174）。compact 始终为非流式。
func ExecuteOpenAIResponsesCompactRequest(ctx context.Context, account *auth.Account, requestBody []byte, proxyOverride string, headers http.Header) (upstreamResponse *http.Response, upstreamErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	resetUpstreamUserAgentAudit(ctx)
	resetWsAcquireAudit(ctx)
	var encryptedAttempt *encryptedContentAttempt
	requestBody, encryptedAttempt = prepareEncryptedContentAttempt(ctx, account, requestBody, "", headers)
	defer func() { encryptedAttempt.observeResponse(upstreamResponse, requestBody) }()
	responsesLite := gateResponsesLiteForAccount(codexResponsesLiteRequested(requestBody, headers), requestBody, account)
	requestBody, headers = prepareCodexResponsesLiteTransport(requestBody, headers, false, responsesLite)

	baseURL, apiKey := account.OpenAIResponsesCredentials()
	account.Mu().RLock()
	proxyURL := account.ProxyURL
	account.Mu().RUnlock()
	if proxyOverride != "" {
		proxyURL = proxyOverride
	}
	if baseURL == "" || apiKey == "" {
		return nil, ErrNoAvailableAccount()
	}

	endpoint := auth.OpenAIResponsesEndpoint(baseURL, "/v1/responses/compact")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(requestBody))
	if err != nil {
		return nil, ErrInternalError("创建请求失败", err)
	}
	applyOpenAIResponsesRequestHeaders(req, account, apiKey, headers)

	if err := ConsumeAPIKeyModelRequestQuota(ctx, gjson.GetBytes(requestBody, "model").String()); err != nil {
		return nil, err
	}
	resp, err := doTracedUpstreamRequest(getPooledClient(account, proxyURL), req, account, proxyURL)
	if err != nil {
		if shouldRecyclePooledClient(err) {
			recyclePooledClient(account, proxyURL)
		}
		return nil, ErrUpstream(0, "请求 OpenAI Responses API compact 失败", err)
	}
	return resp, nil
}

// ExecuteCompactRequest 向 Codex 上游发送 /responses/compact 请求（非流式压缩接口）
func ExecuteCompactRequest(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header) (upstreamResponse *http.Response, upstreamErr error) {
	if account == nil || account.IsRelayStyle() {
		return nil, ErrNoAvailableAccount()
	}
	if ctx == nil {
		ctx = context.Background()
	}
	resetUpstreamUserAgentAudit(ctx)
	resetWsAcquireAudit(ctx)
	var encryptedAttempt *encryptedContentAttempt
	requestBody, encryptedAttempt = prepareEncryptedContentAttempt(ctx, account, requestBody, sessionID, headers)
	defer func() { encryptedAttempt.observeResponse(upstreamResponse, requestBody) }()
	responsesLite := gateResponsesLiteForAccount(codexResponsesLiteRequested(requestBody, headers), requestBody, account)

	account.Mu().RLock()
	accessToken := account.AccessToken
	proxyURL := account.ProxyURL
	account.Mu().RUnlock()

	if proxyOverride != "" {
		proxyURL = proxyOverride
	}

	if accessToken == "" {
		return nil, ErrNoAvailableAccount()
	}

	// 与 ExecuteRequest 相同的请求体优化
	if !gjson.GetBytes(requestBody, "instructions").Exists() {
		requestBody, _ = sjson.SetBytes(requestBody, "instructions", "")
	}
	requestBody, _ = sjson.DeleteBytes(requestBody, "previous_response_id")
	// compact 端点同样走 HTTP，不接受 prompt_cache_retention，必须删除。
	requestBody, _ = sjson.DeleteBytes(requestBody, "prompt_cache_retention")
	requestBody, _ = sjson.DeleteBytes(requestBody, "safety_identifier")
	requestBody, _ = sjson.DeleteBytes(requestBody, "disable_response_storage")
	// 顶层 type 是 WS 事件信封字段，compact HTTP 端点同样不接受，兜底删除(issue #548)。
	requestBody, _ = sjson.DeleteBytes(requestBody, "type")
	requestBody, headers = prepareCodexResponsesLiteTransport(requestBody, headers, false, responsesLite)
	// 指纹收敛：与 ExecuteRequest 同样在请求体定稿后、构造出站请求前改写
	// client_metadata。漏掉这一步会让 compact 路径只收敛请求头、请求体仍带客户端
	// 真实标识，上游看到「头说设备 A、体说设备 B」这种真实客户端不会有的矛盾。
	// 必须用 prepareCodexResponsesLiteTransport 之后的 headers（它可能返回克隆），
	// 与下方 applyCodexRequestHeaders 取同一份下游头，两处推导结果才一致。
	requestBody = ApplyCodexFingerprintToBody(requestBody, account, headers)

	existingCacheKey := strings.TrimSpace(gjson.GetBytes(requestBody, "prompt_cache_key").String())
	cacheKey := existingCacheKey
	if sessionID != "" {
		cacheKey = sessionID
		requestBody, _ = sjson.SetBytes(requestBody, "prompt_cache_key", cacheKey)
	}

	// compact 端点
	endpoint := CodexBaseURL + "/responses/compact"

	// Resin 反向代理模式
	var client *http.Client
	if IsResinEnabled() {
		endpoint = BuildReverseProxyURL(endpoint)
		client = getResinHTTPClient(account)
	} else {
		client = getPooledClient(account, proxyURL)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(requestBody))
	if err != nil {
		return nil, ErrInternalError("创建请求失败", err)
	}

	applyCodexRequestHeaders(req, account, accessToken, cacheKey, apiKey, deviceCfg, headers)
	// routing hint 由网关按最终出站 body 合成，须在账号自定义头之后设置。
	ApplyCodexRoutingHint(req.Header, account, requestBody)

	if IsResinEnabled() {
		req.Header.Set("X-Resin-Account", ResinAccountID(account))
	}
	logCodexFingerprintDebug("compact", account, proxyURL, req.Header)

	if err := ConsumeAPIKeyModelRequestQuota(ctx, gjson.GetBytes(requestBody, "model").String()); err != nil {
		return nil, err
	}
	resp, err := doTracedUpstreamRequest(client, req, account, proxyURL)
	if err != nil {
		if shouldRecyclePooledClient(err) {
			recyclePooledClient(account, proxyURL)
		}
		return nil, ErrUpstream(0, "请求上游失败", err)
	}

	return resp, nil
}

func codexVersionFromProfile(profile deviceProfile, fallback string) string {
	if profile.HasVersion {
		return fmt.Sprintf("%d.%d.%d", profile.Version.major, profile.Version.minor, profile.Version.patch)
	}
	return strings.TrimSpace(fallback)
}

func codexVersionFromUserAgent(userAgent, fallback string) string {
	if _, rawVersion, ok := parseCodexClientVersionDetails(userAgent); ok {
		return rawVersion
	}
	return strings.TrimSpace(fallback)
}

func codexVersionFromString(raw string) (cliVersion, bool) {
	raw = strings.TrimSpace(strings.TrimPrefix(raw, "v"))
	if raw == "" {
		return cliVersion{}, false
	}
	return parseCodexClientVersion("codex_cli_rs/" + raw)
}

func generatedCodexClientHeaders(account *auth.Account, settings RuntimeSettings) (string, string) {
	versionFloor := ""
	if settings.ClientCompatMode == ClientCompatModeAuto {
		versionFloor = settings.CodexMinCLIVersion
	}
	if userAgent, version, ok := codexUserAgentFromConfig(settings.CodexUserAgentConfig, versionFloor); ok {
		return userAgent, version
	}
	accountID := int64(0)
	if account != nil {
		accountID = account.ID()
	}
	profile := ProfileForAccount(accountID)
	userAgent := strings.TrimSpace(profile.UserAgent)
	version := strings.TrimSpace(profile.Version)
	if userAgent == "" {
		userAgent = defaultCodexCLIUserAgent
	}
	if version == "" {
		version = codexVersionFromUserAgent(userAgent, latestCodexCLIVersion)
	}
	// 画像池钉的是内置常量版本；抬升到当前生效的最新版（含远端同步值），
	// 再叠加显式的最低版本门槛。
	version = effectiveCodexClientVersion(version, effectiveLatestCodexCLIVersion())
	version = effectiveCodexClientVersion(version, versionFloor)
	userAgent = replaceCodexUserAgentVersion(userAgent, version)
	return userAgent, version
}

func shouldGenerateCodexClientHeaders(settings RuntimeSettings, userAgent, originator string) bool {
	switch settings.ClientCompatMode {
	case ClientCompatModeForce:
		return true
	case ClientCompatModeAuto:
		version, ok := parseCodexClientVersion(userAgent)
		if !ok {
			return false
		}
		minVersion, ok := codexVersionFromString(settings.CodexMinCLIVersion)
		if !ok {
			minVersion, _ = codexVersionFromString(defaultCodexMinCLIVersion)
		}
		return IsCodexStrictOfficialClientByHeaders(userAgent, originator) && version.Compare(minVersion) < 0
	default:
		return false
	}
}

func resolveCodexOutboundClientHeaders(account *auth.Account, apiKey string, deviceCfg *DeviceProfileConfig, downstreamHeaders http.Header) (userAgent, version string, usedGenerated bool) {
	if IsDeviceProfileStabilizationEnabled(deviceCfg) {
		profile := ResolveDeviceProfile(account, apiKey, downstreamHeaders, deviceCfg)
		userAgent = strings.TrimSpace(profile.UserAgent)
		version = codexVersionFromProfile(profile, strings.TrimSpace(deviceCfg.PackageVersion))
		if userAgent == "" {
			userAgent = defaultCodexCLIUserAgent
		}
		return userAgent, strings.TrimSpace(version), false
	}

	userAgent = strings.TrimSpace(downstreamHeaders.Get("User-Agent"))
	originator := strings.TrimSpace(downstreamHeaders.Get("Originator"))
	settings := CurrentRuntimeSettings()
	if shouldGenerateCodexClientHeaders(settings, userAgent, originator) {
		userAgent, version = generatedCodexClientHeaders(account, settings)
		return userAgent, version, true
	}
	if IsCodexOfficialClientByHeaders(userAgent, originator) && userAgent != "" {
		version = firstNonEmptyHeader(downstreamHeaders, "Version", codexVersionFromUserAgent(userAgent, latestCodexCLIVersion))
		return userAgent, version, false
	}
	versionFloor := ""
	if settings.ClientCompatMode == ClientCompatModeAuto {
		versionFloor = settings.CodexMinCLIVersion
	}
	if userAgent, version, ok := codexUserAgentFromConfig(settings.CodexUserAgentConfig, versionFloor); ok {
		return userAgent, version, true
	}
	effectiveVersion := effectiveLatestCodexCLIVersion()
	return replaceCodexUserAgentVersion(defaultCodexCLIUserAgent, effectiveVersion), effectiveVersion, false
}

func ResolveCodexOutboundClientHeaders(account *auth.Account, apiKey string, deviceCfg *DeviceProfileConfig, downstreamHeaders http.Header) (userAgent, version string) {
	userAgent, version, _ = ResolveCodexOutboundClientHeadersWithDecision(account, apiKey, deviceCfg, downstreamHeaders)
	return userAgent, version
}

func ResolveCodexOutboundClientHeadersWithDecision(account *auth.Account, apiKey string, deviceCfg *DeviceProfileConfig, downstreamHeaders http.Header) (userAgent, version string, usedGenerated bool) {
	return resolveCodexOutboundClientHeaders(account, apiKey, deviceCfg, downstreamHeaders)
}

func applyCodexAllowedForwardHeaders(req *http.Request, downstreamHeaders http.Header) {
	if req == nil || downstreamHeaders == nil {
		return
	}
	for _, name := range codexAllowedForwardHeaders {
		if value := strings.TrimSpace(downstreamHeaders.Get(name)); value != "" {
			req.Header.Set(name, value)
		}
	}
}

func applyAccountCustomHeaders(req *http.Request, account *auth.Account) {
	if req == nil || account == nil {
		return
	}
	for name, value := range account.GetCustomHeaders() {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		req.Header.Set(name, value)
	}
}

func applyCodexRequestHeaders(req *http.Request, account *auth.Account, accessToken, cacheKey, apiKey string, deviceCfg *DeviceProfileConfig, downstreamHeaders http.Header) {
	if req == nil {
		return
	}

	accountID := ""
	if account != nil {
		account.Mu().RLock()
		accountID = account.AccountID
		account.Mu().RUnlock()
	}

	userAgent, version, usedGeneratedHeaders := resolveCodexOutboundClientHeaders(account, apiKey, deviceCfg, downstreamHeaders)
	req.Header.Set("User-Agent", userAgent)

	// Agent Identity 账号用动态签名的 AgentAssertion 头替代 Bearer（task 已由调用方确保就绪）。
	if account != nil && account.IsCodexAgentIdentity() {
		if assertion, err := account.BuildCodexAgentAssertion(time.Now()); err == nil {
			req.Header.Set("Authorization", assertion)
		}
	} else {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	// 不发 Connection：这是 HTTP/2 明令禁止的 connection-specific 头（RFC 9113 §8.2.2），
	// 而 Codex 官方上游走的就是 h2——Go 的 http2 transport 会把它剥掉，剥不掉的代理
	// 链路上它则是个真实客户端不会有的多余头。真实 Codex 用 reqwest，同样不发。
	if version != "" {
		req.Header.Set("Version", version)
	}
	if originator := strings.TrimSpace(downstreamHeaders.Get("Originator")); !usedGeneratedHeaders && originator != "" && IsCodexOfficialClientByHeaders("", originator) {
		req.Header.Set("Originator", originator)
	} else {
		req.Header.Set("Originator", Originator)
	}
	applyCodexAllowedForwardHeaders(req, downstreamHeaders)
	// 会话级 beta-features:真实 Codex 每个 /responses 请求、WS 握手与 compact 都带
	// x-codex-beta-features,默认恰为 remote_compaction_v2(codex-rs
	// build_model_client_beta_features_header,无实验特性默认开启)。下游声明的原样
	// 保留——非空但无 v2 表示用户显式关闭,不改写;未声明时补默认,避免"只有部分
	// 请求带头"这种真实客户端不会产生的模式。
	if strings.TrimSpace(req.Header.Get(codexBetaFeaturesHeader)) == "" {
		value := defaultCodexBetaFeatures
		if deviceCfg != nil && strings.TrimSpace(deviceCfg.BetaFeatures) != "" {
			value = strings.TrimSpace(deviceCfg.BetaFeatures)
		}
		req.Header.Set(codexBetaFeaturesHeader, value)
	}
	// 指纹收敛必须在白名单透传之后（覆盖客户端原值）、账号自定义头之前（运维显式
	// 配置保持最终优先）。off 档为空操作。
	ApplyCodexFingerprintHeaders(req.Header, account, downstreamHeaders)
	if accountID != "" {
		req.Header.Set("Chatgpt-Account-Id", accountID)
	}
	// 会话标识头按真实客户端形态写出（session-id / thread-id / x-client-request-id）；
	// 收敛开启时与 turn metadata 报同一组身份。CODEX_SESSION_HEADER_MODE=legacy
	// 可整体退回旧的 Session_id 形态。
	ApplyCodexSessionHeaders(req.Header, account, cacheKey, downstreamHeaders, false)
	applyAccountCustomHeaders(req, account)
	RecordUpstreamUserAgent(req.Context(), req.Header.Get("User-Agent"))
}

func applyOpenAIResponsesRequestHeaders(req *http.Request, account *auth.Account, apiKey string, headers http.Header) {
	if req == nil {
		return
	}
	userAgent, version, _ := resolveCodexOutboundClientHeaders(account, "", nil, headers)
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("User-Agent", userAgent)
	if version != "" {
		req.Header.Set("Version", version)
	}
	if headers != nil {
		for _, key := range []string{"OpenAI-Organization", "OpenAI-Project", "Idempotency-Key", codexResponsesLiteHeader} {
			if value := firstNonEmptyHeader(headers, key, ""); value != "" {
				req.Header.Set(key, value)
			}
		}
	}
	applyAccountCustomHeaders(req, account)
	RecordUpstreamUserAgent(req.Context(), req.Header.Get("User-Agent"))
}

const downstreamAffinityHeader = "X-Codex2API-Affinity-Key"

// requestSessionIdentity keeps the local account-routing identity separate
// from the seed used to derive an upstream Session_id/prompt_cache_key. The
// dedicated downstream affinity header may only replace affinityID; it must
// never change upstreamSeed or become an explicit upstream session.
type requestSessionIdentity struct {
	affinityID            string
	upstreamSeed          string
	explicitUpstreamID    string
	hasDownstreamAffinity bool
	hasRequestFingerprint bool
}

// ResolveSessionID 从下游请求提取或生成 session ID
// 优先级：
//  1. Header: X-Codex2API-Affinity-Key（仅本地使用，先哈希再参与绑定）
//  2. Header: Session_id
//  3. Header: Conversation_id
//  4. Header: Idempotency-Key
//  5. Header: X-Session-Id / X-Session-Affinity（opencode 等第三方客户端）
//  6. Body:   prompt_cache_key
//  7. Body:   内容派生种子（model+instructions+system+首条 user 消息，见
//     deriveContentSessionSeed；带 previous_response_id 的续链请求跳过）
//  8. 基于 Bearer API Key 的确定性 UUID
//
// 第 7 级让"同一段对话的多轮请求"收敛到同一账号粘性键：单 API Key 供多终端
// 用户共用时，粘性粒度从"整个 Key 挤一个账号"细化为"每段对话独立粘定"。
// 专用 affinity header 永不参与上游 session ID / prompt_cache_key，也不会被转发；
// 下游网关可用它传稳定的最终用户/对话标识，在共享 Bearer Key 时仍实现一人一号式绑定。
func ResolveSessionID(headers http.Header, body []byte) string {
	return resolveRequestSessionIdentity(headers, body).affinityID
}

func resolveRequestSessionIdentity(headers http.Header, body []byte) requestSessionIdentity {
	hasEngineFingerprint := EvaluateEngineFingerprint(headers, body, nil)
	explicitID := ResolveExplicitSessionID(headers, body)
	upstreamSeed := explicitID
	if upstreamSeed == "" {
		upstreamSeed = deriveContentSessionSeed(body)
	}
	if upstreamSeed == "" {
		// 基于下游用户的 API Key 生成确定性 cache key（参考 CLIProxyAPI codex_executor.go:621）
		authHeader := ""
		if headers != nil {
			authHeader = headers.Get("Authorization")
		}
		apiKey := strings.TrimSpace(strings.TrimPrefix(authHeader, "Bearer "))
		if apiKey != "" {
			// 必须与 deterministicPromptCacheKey 用同一条派生：两处共享种子字符串，
			// 产出不同就会让 HTTP 与 WS 路径对同一个 API Key 算出两个上游身份。
			upstreamSeed = DeriveStableSessionUUIDv7("codex2api:prompt-cache:" + apiKey)
		}
	}
	if upstreamSeed == "" {
		// 最后兜底：本地路由和上游 seed 共享同一个随机 UUID。取 v7——apiKeyID<=0 时
		// IsolateCodexSessionID 会原样返回这个种子，它就直接成了出站会话身份。
		upstreamSeed = NewUpstreamSessionUUID()
	}

	affinityID := upstreamSeed
	if localAffinityID := resolveDownstreamAffinityID(headers); localAffinityID != "" {
		affinityID = localAffinityID
		return requestSessionIdentity{
			affinityID:            affinityID,
			upstreamSeed:          upstreamSeed,
			explicitUpstreamID:    explicitID,
			hasDownstreamAffinity: true,
			hasRequestFingerprint: true,
		}
	}
	return requestSessionIdentity{
		affinityID:            affinityID,
		upstreamSeed:          upstreamSeed,
		explicitUpstreamID:    explicitID,
		hasRequestFingerprint: hasEngineFingerprint,
	}
}

func resolveDownstreamAffinityID(headers http.Header) string {
	if headers == nil {
		return ""
	}
	raw := strings.TrimSpace(headers.Get(downstreamAffinityHeader))
	if raw == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("codex2api:downstream-affinity:" + raw))
	return "affinity-" + hex.EncodeToString(sum[:16])
}

func ResolveExplicitSessionID(headers http.Header, body []byte) string {
	if headers != nil {
		// 注意：Codex CLI 发的是连字符头 session-id / conversation-id（HTTP/2 全小写，
		// 服务端规范化成 Session-Id / Conversation-Id），与旧的下划线写法 Session_id 不同，
		// 两种都要认，否则取不到显式会话 id、affinity 只能退回内容种子。
		for _, key := range []string{"Session-Id", "Session_id", "Conversation-Id", "Conversation_id", "Idempotency-Key"} {
			if v := strings.TrimSpace(headers.Get(key)); v != "" {
				return v
			}
		}
		// opencode 等第三方 CLI 客户端用 x-session-id / x-session-affinity 标识会话
		// （值为 ses_...，非 UUID，出站上由 claudeUpstreamSessionID 等确定性派生为
		// UUIDv7）。优先级低于既有显式头，高于 body prompt_cache_key。
		for _, key := range []string{"X-Session-Id", "X-Session-Affinity"} {
			if v := strings.TrimSpace(headers.Get(key)); v != "" {
				return v
			}
		}
	}
	// 没有显式会话头时，从 body 的 prompt_cache_key 提取。
	if v := strings.TrimSpace(gjson.GetBytes(body, "prompt_cache_key").String()); v != "" {
		return v
	}

	return ""
}

const statelessWebsocketSessionPrefix = "stateless-"

func statelessWebsocketSessionID() string {
	return statelessWebsocketSessionPrefix + uuid.NewString()
}

// IsStatelessWebsocketSessionID 判断是否为 WS 路径生成的一次性连接 ID。
// 这类 ID 只用于连接池隔离，不能当作 prompt cache key 发往上游。
func IsStatelessWebsocketSessionID(sessionID string) bool {
	return strings.HasPrefix(sessionID, statelessWebsocketSessionPrefix)
}

// deterministicPromptCacheKey 生成与 ResolveSessionID 兜底逻辑同源的确定性
// prompt cache key：优先按下游 API Key 派生，无 API Key 时按账号派生。
//
// 产出取 UUIDv7 形态而非 uuid.NewSHA1 的 v5：v5 的版本 nibble 是 5，真实客户端
// 的 session_id / prompt_cache_key 恒为 v7，这个差异对任何解析 UUID 版本位的
// 一侧都是直接可见的。同 IsolateCodexSessionID，换格式的代价是一次性 cache miss。
func deterministicPromptCacheKey(apiKey string, account *auth.Account) string {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey != "" {
		return DeriveStableSessionUUIDv7("codex2api:prompt-cache:" + apiKey)
	}
	if account != nil {
		if id := account.ID(); id > 0 {
			return DeriveStableSessionUUIDv7(fmt.Sprintf("codex2api:prompt-cache:auth:%d", id))
		}
	}
	return ""
}

// ReadSSEStream 从上游 SSE 响应读取事件流
// callback 返回 true 表示继续读取，false 表示停止
func ReadSSEStream(body io.Reader, callback func(data []byte) bool) error {
	return ReadSSEStreamWithEvent(body, func(_ string, data []byte) bool {
		return callback(data)
	})
}

// ReadSSEStreamWithEvent preserves the optional SSE event field while keeping
// ReadSSEStream's data-only API compatible for existing callers.
func ReadSSEStreamWithEvent(body io.Reader, callback func(event string, data []byte) bool) error {
	// 使用 sync.Pool 复用缓冲区，减少 GC 压力
	buf := sseBufferPool.Get().([]byte)
	defer sseBufferPool.Put(buf)

	lineBufPtr := sseLineBufPool.Get().(*[]byte)
	lineBuf := (*lineBufPtr)[:0]
	defer func() {
		// 归还时限制容量，避免异常大的缓冲区长期驻留池中
		if cap(lineBuf) <= 256*1024 {
			*lineBufPtr = lineBuf[:0]
			sseLineBufPool.Put(lineBufPtr)
		}
	}()

	var dataLines [][]byte
	var eventName string

	emitEvent := func() bool {
		event := eventName
		eventName = ""
		if len(dataLines) == 0 {
			return true
		}

		// 绝大多数上游事件只有一条 data: 行，直接交给 callback，避免
		// bytes.Join 为每个 token 事件再复制一遍 payload。多行 SSE 才合并。
		data := dataLines[0]
		if len(dataLines) > 1 {
			data = bytes.Join(dataLines, []byte("\n"))
		}
		isDone := bytes.Equal(data, []byte("[DONE]"))
		keepReading := !isDone && callback(event, data)
		// 清掉 backing array 中的切片引用，避免最后一个大事件一直被
		// dataLines 的容量槽位持有到整条流结束。
		for i := range dataLines {
			dataLines[i] = nil
		}
		dataLines = dataLines[:0]
		return keepReading
	}

	consumeField := func(line []byte) {
		if bytes.HasPrefix(line, []byte("data:")) {
			data := bytes.TrimPrefix(line, []byte("data:"))
			data = bytes.TrimPrefix(data, []byte(" "))
			// 使用 copy 避免底层数组共享导致的内存泄漏
			dataCopy := make([]byte, len(data))
			copy(dataCopy, data)
			dataLines = append(dataLines, dataCopy)
			return
		}
		if bytes.HasPrefix(line, []byte("event:")) {
			event := bytes.TrimPrefix(line, []byte("event:"))
			event = bytes.TrimPrefix(event, []byte(" "))
			eventName = string(event)
		}
	}

	for {
		n, err := body.Read(buf)
		if n > 0 {
			lineBuf = append(lineBuf, buf[:n]...)

			// 按行处理。用偏移量扫描，最后一次性把未完成行搬到缓冲区头部；
			// 不能反复 lineBuf = lineBuf[idx+1:]，否则每消费一行都会缩短 cap，
			// 下一次 64KB Read 几乎必然重新分配，池化缓冲区形同失效。
			consumed := 0
			for {
				idx := bytes.IndexByte(lineBuf[consumed:], '\n')
				if idx < 0 {
					break
				}

				lineEnd := consumed + idx
				line := bytes.TrimRight(lineBuf[consumed:lineEnd], "\r")
				consumed = lineEnd + 1

				if len(line) == 0 {
					if !emitEvent() {
						return nil
					}
					continue
				}

				if bytes.HasPrefix(line, []byte(":")) {
					continue
				}

				// 解析 SSE event/data 字段，支持标准多行 data 聚合。
				consumeField(line)
			}

			if consumed > 0 {
				remaining := copy(lineBuf, lineBuf[consumed:])
				lineBuf = lineBuf[:remaining]
			}
		}

		if err != nil {
			if err == io.EOF {
				if len(lineBuf) > 0 {
					line := bytes.TrimRight(lineBuf, "\r")
					consumeField(line)
				}
				if !emitEvent() {
					return nil
				}
				return nil
			}
			return err
		}
	}
}

// sseBufferPool 用于复用 SSE 读取缓冲区（64KB 以适应 reasoning 模型的大 thinking block）
var sseBufferPool = sync.Pool{
	New: func() interface{} {
		return make([]byte, 64*1024)
	},
}

// sseLineBufPool 用于复用行缓冲区，减少频繁分配
var sseLineBufPool = sync.Pool{
	New: func() interface{} {
		b := make([]byte, 0, 64*1024)
		return &b
	},
}
