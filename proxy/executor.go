package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
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
const (
	codexHTTP2ReadIdleTimeout = 15 * time.Second
	codexHTTP2PingTimeout     = 15 * time.Second
)

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
	client   *http.Client
	lastUsed atomic.Int64 // UnixNano 时间戳
}

func (e *poolEntry) touch() {
	e.lastUsed.Store(time.Now().UnixNano())
}

var clientPool sync.Map // map[string]*poolEntry, key = accountID|proxyURL|transportMode

// Some Codex-only Responses relays require the official client installation
// metadata in addition to the User-Agent. Learn that capability after the
// relay returns codex_access_restricted so generic Responses APIs stay untouched.
var openAIResponsesCodexMetadataRequired sync.Map // map[accountID|baseURL]struct{}

// clientPoolTTL 未使用超过此时间的 Client 将被淘汰
const clientPoolTTL = 5 * time.Minute

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
	cutoff := time.Now().Add(-clientPoolTTL).UnixNano()
	clientPool.Range(func(key, value any) bool {
		entry := value.(*poolEntry)
		if entry.lastUsed.Load() < cutoff {
			clientPool.Delete(key)
			entry.client.CloseIdleConnections()
		}
		return true
	})
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
		v.(*poolEntry).client.CloseIdleConnections()
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
	CodexBaseURL = "https://chatgpt.com/backend-api/codex"
	Originator   = "codex-tui"
)

var codexAllowedForwardHeaders = []string{
	"X-Codex-Turn-State",
	"X-Codex-Turn-Metadata",
	"X-Client-Request-Id",
	"X-Codex-Beta-Features",
	// DeviceCheck 设备认证头（上游 openai/codex#20619）。仅在下游真实 Codex
	// 客户端携带时原样透传——本代理无法（也不该）伪造：token 是 Apple 硬件
	// 背书、服务端向 Apple 验证，假值必然验证失败、比"不携带"更暴露特征。
	// 缺失是合法状态（纯 CLI / 非 macOS 客户端本就不发）。
	"X-Oai-Attestation",
}

// WebsocketExecuteFunc WebSocket 执行函数（由 wsrelay 包在 main.go 中注册，避免循环依赖）
// poolRouteKey：本地连接池路由键（仅本地、永不发上游）。非空时 wsrelay 用它作 8 槽池的
// baseKey，从而把"上游会话身份(每请求唯一)"与"连接复用(按 API Key 稳定)"解耦；空时沿用
// headerSessionID 作 baseKey（显式会话 / per-api-key 模式的原有行为）。
var WebsocketExecuteFunc func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error)

func IsolateCodexSessionID(apiKeyID int64, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || apiKeyID <= 0 {
		return raw
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("api-key:%d:%s", apiKeyID, raw)))
	return hex.EncodeToString(sum[:8])
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
		return uuid.NewString()
	}
	return IsolateCodexSessionID(apiKeyID, upstreamSeed)
}

// ExecuteRequest 向 Codex 上游发送请求
// sessionID 可选，用于 prompt cache 会话绑定
// useWebsocket 可选：未传时遵循全局强制 WS；传 true/false 时由调用方显式控制。
// headers 下游请求头，用于设备指纹学习
func ExecuteRequest(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, useWebsocket ...bool) (*http.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	resetUpstreamUserAgentAudit(ctx)

	// Payload 规则改写：在 WS/HTTP 分叉前统一应用，两条上游路径共享改写结果。
	// 生图请求跳过——其 instructions/工具由网关自行构造，改写会破坏桥接协议。
	if !responsesBodyRequestsImageGeneration(requestBody) {
		RecordObservedInstructions(requestBody, headers)
		requestBody = ApplyPayloadRulesToBody(requestBody, gjson.GetBytes(requestBody, "model").String(), headers, PayloadRuleIdentityFromContext(ctx))
	}
	wantWebsocket := CurrentRuntimeSettings().CodexForceWebsocket
	if len(useWebsocket) > 0 {
		wantWebsocket = useWebsocket[0]
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
					requestBody, _ = sjson.SetBytes(requestBody, "prompt_cache_key", uuid.NewString())
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
		return WebsocketExecuteFunc(ctx, account, requestBody, sessionID, proxyOverride, apiKey, deviceCfg, headers, poolRouteKey)
	}
	if wantWebsocket && WebsocketExecuteFunc == nil {
		// 请求/配置要求走 WebSocket，但 WS 执行器未注册（如嵌入式调用或初始化顺序问题）。
		// 静默落回 HTTP 会让“以为开了 WS 实际走 HTTP”难以排查，这里显式告警。
		log.Printf("[WS] 警告: 期望走 WebSocket 上游，但 WebsocketExecuteFunc 未注册，已回退到 HTTP (account %d)", account.ID())
	}

	account.Mu().RLock()
	accessToken := account.AccessToken
	proxyURL := account.ProxyURL
	account.Mu().RUnlock()

	// 代理池优先级: proxyOverride (来自 NextProxy) > account.ProxyURL
	if proxyOverride != "" {
		proxyURL = proxyOverride
	}

	if accessToken == "" {
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

	// 3. 注入 prompt_cache_key（如果请求体中没有，且 sessionID 不为空）
	existingCacheKey := strings.TrimSpace(gjson.GetBytes(requestBody, "prompt_cache_key").String())
	cacheKey := existingCacheKey
	if sessionID != "" {
		cacheKey = sessionID
		requestBody, _ = sjson.SetBytes(requestBody, "prompt_cache_key", cacheKey)
	}

	endpoint := CodexBaseURL + "/responses"

	// Resin 反向代理模式：改写 URL，使用标准 HTTP 客户端
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

	// ==================== 请求头（伪装 Codex CLI） ====================
	applyCodexRequestHeaders(req, account, accessToken, cacheKey, apiKey, deviceCfg, headers)

	// Resin 反代：注入账号身份头
	if IsResinEnabled() {
		req.Header.Set("X-Resin-Account", ResinAccountID(account))
	}
	logCodexFingerprintDebug("http", account, proxyURL, req.Header)

	resp, err := client.Do(req)
	if err != nil {
		if shouldRecyclePooledClient(err) {
			recyclePooledClient(account, proxyURL)
		}
		return nil, ErrUpstream(0, "请求上游失败", err)
	}

	return resp, nil
}

func ExecuteOpenAIResponsesRequest(ctx context.Context, account *auth.Account, requestBody []byte, proxyOverride string, headers http.Header) (*http.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	resetUpstreamUserAgentAudit(ctx)

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
		resp, err := client.Do(req)
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
func ExecuteOpenAIResponsesCompactRequest(ctx context.Context, account *auth.Account, requestBody []byte, proxyOverride string, headers http.Header) (*http.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	resetUpstreamUserAgentAudit(ctx)

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

	resp, err := getPooledClient(account, proxyURL).Do(req)
	if err != nil {
		if shouldRecyclePooledClient(err) {
			recyclePooledClient(account, proxyURL)
		}
		return nil, ErrUpstream(0, "请求 OpenAI Responses API compact 失败", err)
	}
	return resp, nil
}

// ExecuteCompactRequest 向 Codex 上游发送 /responses/compact 请求（非流式压缩接口）
func ExecuteCompactRequest(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header) (*http.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	resetUpstreamUserAgentAudit(ctx)

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

	if IsResinEnabled() {
		req.Header.Set("X-Resin-Account", ResinAccountID(account))
	}
	logCodexFingerprintDebug("compact", account, proxyURL, req.Header)

	resp, err := client.Do(req)
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

	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Connection", "Keep-Alive")
	if version != "" {
		req.Header.Set("Version", version)
	}
	if originator := strings.TrimSpace(downstreamHeaders.Get("Originator")); !usedGeneratedHeaders && originator != "" && IsCodexOfficialClientByHeaders("", originator) {
		req.Header.Set("Originator", originator)
	} else {
		req.Header.Set("Originator", Originator)
	}
	applyCodexAllowedForwardHeaders(req, downstreamHeaders)
	if accountID != "" {
		req.Header.Set("Chatgpt-Account-Id", accountID)
	}
	if cacheKey != "" {
		req.Header.Set("Session_id", cacheKey)
		req.Header.Del("Conversation_id")
	}
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
		for _, key := range []string{"OpenAI-Organization", "OpenAI-Project", "Idempotency-Key"} {
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
	affinityID         string
	upstreamSeed       string
	explicitUpstreamID string
}

// ResolveSessionID 从下游请求提取或生成 session ID
// 优先级：
//  1. Header: X-Codex2API-Affinity-Key（仅本地使用，先哈希再参与绑定）
//  2. Header: Session_id
//  3. Header: Conversation_id
//  4. Header: Idempotency-Key
//  5. Body:   prompt_cache_key
//  6. Body:   内容派生种子（model+instructions+system+首条 user 消息，见
//     deriveContentSessionSeed；带 previous_response_id 的续链请求跳过）
//  7. 基于 Bearer API Key 的确定性 UUID
//
// 第 6 级让"同一段对话的多轮请求"收敛到同一账号粘性键：单 API Key 供多终端
// 用户共用时，粘性粒度从"整个 Key 挤一个账号"细化为"每段对话独立粘定"。
// 专用 affinity header 永不参与上游 session ID / prompt_cache_key，也不会被转发；
// 下游网关可用它传稳定的最终用户/对话标识，在共享 Bearer Key 时仍实现一人一号式绑定。
func ResolveSessionID(headers http.Header, body []byte) string {
	return resolveRequestSessionIdentity(headers, body).affinityID
}

func resolveRequestSessionIdentity(headers http.Header, body []byte) requestSessionIdentity {
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
			upstreamSeed = uuid.NewSHA1(uuid.NameSpaceOID, []byte("codex2api:prompt-cache:"+apiKey)).String()
		}
	}
	if upstreamSeed == "" {
		// 最后兜底：本地路由和上游 seed 共享同一个随机 UUID。
		upstreamSeed = uuid.New().String()
	}

	affinityID := upstreamSeed
	if localAffinityID := resolveDownstreamAffinityID(headers); localAffinityID != "" {
		affinityID = localAffinityID
	}
	return requestSessionIdentity{
		affinityID:         affinityID,
		upstreamSeed:       upstreamSeed,
		explicitUpstreamID: explicitID,
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
		if v := strings.TrimSpace(headers.Get("Session_id")); v != "" {
			return v
		}
		if v := strings.TrimSpace(headers.Get("Conversation_id")); v != "" {
			return v
		}
		if v := strings.TrimSpace(headers.Get("Idempotency-Key")); v != "" {
			return v
		}
	}
	// 优先从 body 的 prompt_cache_key 提取
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
func deterministicPromptCacheKey(apiKey string, account *auth.Account) string {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey != "" {
		return uuid.NewSHA1(uuid.NameSpaceOID, []byte("codex2api:prompt-cache:"+apiKey)).String()
	}
	if account != nil {
		if id := account.ID(); id > 0 {
			return uuid.NewSHA1(uuid.NameSpaceOID, []byte(fmt.Sprintf("codex2api:prompt-cache:auth:%d", id))).String()
		}
	}
	return ""
}

// ReadSSEStream 从上游 SSE 响应读取事件流
// callback 返回 true 表示继续读取，false 表示停止
func ReadSSEStream(body io.Reader, callback func(data []byte) bool) error {
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

	emitEvent := func() bool {
		if len(dataLines) == 0 {
			return true
		}

		data := bytes.Join(dataLines, []byte("\n"))
		dataLines = dataLines[:0]
		if bytes.Equal(data, []byte("[DONE]")) {
			return false
		}
		return callback(data)
	}

	for {
		n, err := body.Read(buf)
		if n > 0 {
			lineBuf = append(lineBuf, buf[:n]...)

			// 按行处理
			for {
				idx := bytes.IndexByte(lineBuf, '\n')
				if idx < 0 {
					break
				}

				line := bytes.TrimRight(lineBuf[:idx], "\r")
				lineBuf = lineBuf[idx+1:]

				if len(line) == 0 {
					if !emitEvent() {
						return nil
					}
					continue
				}

				if bytes.HasPrefix(line, []byte(":")) {
					continue
				}

				// 解析 SSE data: 前缀，支持标准多行 data 聚合
				if bytes.HasPrefix(line, []byte("data:")) {
					data := bytes.TrimPrefix(line, []byte("data:"))
					data = bytes.TrimPrefix(data, []byte(" "))
					// 使用 copy 避免底层数组共享导致的内存泄漏
					dataCopy := make([]byte, len(data))
					copy(dataCopy, data)
					dataLines = append(dataLines, dataCopy)
				}
			}

			// 缩容：已消费数据超过一半时，将剩余数据移到头部释放前端内存
			if len(lineBuf) > 0 && cap(lineBuf) > 4096 && len(lineBuf) < cap(lineBuf)/4 {
				compact := make([]byte, len(lineBuf), cap(lineBuf)/2)
				copy(compact, lineBuf)
				lineBuf = compact
			}
		}

		if err != nil {
			if err == io.EOF {
				if len(lineBuf) > 0 {
					line := bytes.TrimRight(lineBuf, "\r")
					if bytes.HasPrefix(line, []byte("data:")) {
						data := bytes.TrimPrefix(line, []byte("data:"))
						data = bytes.TrimPrefix(data, []byte(" "))
						dataCopy := make([]byte, len(data))
						copy(dataCopy, data)
						dataLines = append(dataLines, dataCopy)
					}
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
