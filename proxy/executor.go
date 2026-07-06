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
	"github.com/klauspost/compress/zstd"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

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
	// codexTransportModeUTLSRustls 用 utls HelloCustom 手搭 rustls 指纹，与官方
	// codex-rs（reqwest + rustls 0.23）的 JA3/JA4 对齐。默认不启用（防 regression），
	// 由 CODEX_TRANSPORT_MODE=utls_rustls 显式开启，灰度验证握手连通后再切。
	codexTransportModeUTLSRustls = "utls_rustls"
)

func codexTransportModeFromEnv() string {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CODEX_TRANSPORT_MODE"))) {
	case "standard", "go", "native":
		return codexTransportModeStandard
	case "utls", "utls_chrome", "chrome":
		return codexTransportModeUTLSChrome
	case "", "default", "utls_rustls", "rustls", "codex", "codex_cli_rs":
		// 新默认：对齐真实 codex-rs 的 rustls TLS 指纹 + h2 SETTINGS（握手失败自动回退）。
		return codexTransportModeUTLSRustls
	default:
		return codexTransportModeUTLSRustls
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
	return transport
}

func newCodexTransport(proxyURL string) http.RoundTripper {
	switch codexTransportModeFromEnv() {
	case codexTransportModeUTLSChrome:
		return NewUTLSTransport(proxyURL)
	case codexTransportModeStandard:
		return newCodexStandardTransport(proxyURL)
	case codexTransportModeUTLSRustls:
		return newRustlsFallbackTransport(proxyURL)
	default:
		return newRustlsFallbackTransport(proxyURL)
	}
}

// newRustlsFallbackTransport 首选对齐 codex-rs 的 rustls uTLS 传输；当**连接建立阶段**
// （拨号 / TLS 握手 / h2 建连）失败时，粘性回退到标准 Go 传输，保证不断连。
func newRustlsFallbackTransport(proxyURL string) http.RoundTripper {
	return &rustlsFallbackTransport{
		primary:  NewUTLSRustlsTransport(proxyURL),
		fallback: newCodexStandardTransport(proxyURL),
	}
}

// rustlsFallbackTransport 包装 rustls uTLS 主传输 + 标准回退传输。仅在建连阶段失败
// 且请求可安全重放（无 body 或存在 GetBody）时回退，避免重复副作用。一旦回退即粘住，
// 后续请求直接走标准传输，避免每次重复失败的握手开销。
type rustlsFallbackTransport struct {
	primary  http.RoundTripper
	fallback http.RoundTripper
	mu       sync.Mutex
	tripped  bool
}

func (t *rustlsFallbackTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	t.mu.Lock()
	tripped := t.tripped
	t.mu.Unlock()
	if tripped {
		return t.fallback.RoundTrip(req)
	}

	resp, err := t.primary.RoundTrip(req)
	if err == nil {
		return resp, nil
	}
	// 只在“连接没建起来”且请求可重放时回退——此时尚未发出任何请求字节，安全。
	if !isConnEstablishmentError(err) || !requestReplayable(req) {
		return resp, err
	}
	if req.GetBody != nil {
		if body, gbErr := req.GetBody(); gbErr == nil {
			req.Body = body
		} else {
			return resp, err
		}
	}
	t.mu.Lock()
	t.tripped = true
	t.mu.Unlock()
	log.Printf("[CodexTransport] rustls uTLS 建连失败，粘性回退 standard 传输: %v", err)
	return t.fallback.RoundTrip(req)
}

func (t *rustlsFallbackTransport) CloseIdleConnections() {
	type idleCloser interface{ CloseIdleConnections() }
	if c, ok := t.primary.(idleCloser); ok {
		c.CloseIdleConnections()
	}
	if c, ok := t.fallback.(idleCloser); ok {
		c.CloseIdleConnections()
	}
}

func requestReplayable(req *http.Request) bool {
	return req.Body == nil || req.Body == http.NoBody || req.GetBody != nil
}

// isConnEstablishmentError 判断错误是否发生在连接建立阶段（拨号 / TLS 握手 / h2 建连），
// 这些错误发生在任何请求字节发出之前，回退是安全的。对应 utls_transport.go 里
// createConnection 包装的错误信息。
func isConnEstablishmentError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"tls 握手失败", "tcp 连接失败", "http/2 连接创建失败", "应用自定义 tls 指纹失败",
		"tls handshake", "handshake failure", "connection refused", "no route to host",
		"i/o timeout", "connection reset",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
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
			Timeout:   10 * time.Minute,
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
// 注意：账号粘性键(affinityKey)在 handler 中由独立的 sessionID(ResolveSessionID) 派生，
// 不经过本函数，因此隔离上游身份不会影响账号选择 / token 刷新行为。
func resolveUpstreamSessionID(apiKeyID int64, sessionID, explicitSessionID string, useWebsocket bool) string {
	if useWebsocket && explicitSessionID == "" {
		return ""
	}
	if explicitSessionID == "" && CurrentRuntimeSettings().IsolateRequestsByDefault() {
		return uuid.NewString()
	}
	return IsolateCodexSessionID(apiKeyID, sessionID)
}

// ExecuteRequest 向 Codex 上游发送请求
// sessionID 可选，用于 prompt cache 会话绑定
// useWebsocket 可选：未传时遵循全局强制 WS；传 true/false 时由调用方显式控制。
// headers 下游请求头，用于设备指纹学习
func ExecuteRequest(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, useWebsocket ...bool) (*http.Response, error) {
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

	if ctx == nil {
		ctx = context.Background()
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
	applyCodexRequestBodyEncoding(req, requestBody)

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
		return nil, ErrUpstream(0, "请求 OpenAI Responses API 失败", err)
	}
	return resp, nil
}

// ExecuteOpenAIResponsesCompactRequest 向中转（OpenAI Responses API）账号发送
// /responses/compact 请求。与 ExecuteOpenAIResponsesRequest 行为一致，但命中的是
// 上游自己的 compact 端点，从而让没有官方 Codex OAuth 账号、仅接入中转的用户也能
// 触发上下文自动压缩（参见 issue #174）。compact 始终为非流式。
func ExecuteOpenAIResponsesCompactRequest(ctx context.Context, account *auth.Account, requestBody []byte, proxyOverride string, headers http.Header) (*http.Response, error) {
	if ctx == nil {
		ctx = context.Background()
	}

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
	applyCodexRequestBodyEncoding(req, requestBody)

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
	if settings.MimicStrictHeaders() {
		// strict 模式：originator 前缀改 codex_cli_rs、去除 UA 尾部后缀，
		// 与官方 codex-rs get_codex_user_agent() 对齐。
		profile = StrictProfileForAccount(accountID)
	}
	userAgent := strings.TrimSpace(profile.UserAgent)
	version := strings.TrimSpace(profile.Version)
	if userAgent == "" {
		userAgent = defaultCodexCLIUserAgent
	}
	if version == "" {
		version = codexVersionFromUserAgent(userAgent, latestCodexCLIVersion)
	}
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
	if settings.MimicStrictHeaders() {
		return strictDefaultCodexCLIUserAgent, latestCodexCLIVersion, false
	}
	return defaultCodexCLIUserAgent, latestCodexCLIVersion, false
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

// codexOpenAIBetaHTTPValue 是 HTTP /responses 路径应携带的 OpenAI-Beta 值。
// 官方 codex-rs 在 WS 路径发 responses_websockets=2026-02-06；HTTP 路径历史上
// 由 responses=experimental 启用 Responses API。此处用于 strict 模式补齐 HTTP 头。
const codexOpenAIBetaHTTPValue = "responses=experimental"

// deterministicCodexClientUUID 生成账号级确定性 UUID（v5/SHA1），用于伪造
// x-codex-installation-id / x-codex-window-id 等客户端指纹头，使同一账号在多次请求
// 间保持稳定身份（真实 CLI 的这些 id 在一次安装/一个窗口内是固定的）。
func deterministicCodexClientUUID(kind, seed string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("codex2api:x-codex:"+kind+":"+seed)).String()
}

// codexBetaFeaturesValue 是实测 codex 0.142.5 /responses 请求携带的 x-codex-beta-features。
// 该值随版本/灰度变化，集中在此便于跟随真实客户端更新。
const codexBetaFeaturesValue = "remote_compaction_v2"

// applyCodexClientFingerprintHeaders 注入与真实 codex 0.142.5 一致的 x-codex-* 客户端指纹头
// （见 codex-fpcap/BASELINE.md）。真实客户端**不发** x-codex-installation-id（installation_id
// 放在 x-codex-turn-metadata JSON 内），而是发：x-codex-beta-features、x-codex-window-id(带:0)、
// x-codex-turn-metadata(JSON)、x-client-request-id、thread-id。installation/session 用账号级
// 确定性 UUID 保持同账号稳定；turn_id 每请求新生成。HTTP 与 WS 两条路径共用本函数。
func applyCodexClientFingerprintHeaders(h http.Header, accountID, apiKey, sessionID string) {
	if h == nil {
		return
	}
	seed := strings.TrimSpace(accountID)
	if seed == "" {
		seed = strings.TrimSpace(apiKey)
	}
	if seed == "" {
		return
	}
	installationID := deterministicCodexClientUUID("installation", seed)
	sid := strings.TrimSpace(sessionID)
	if sid == "" {
		sid = deterministicCodexClientUUID("session", seed)
	}
	threadID := sid
	windowID := sid + ":0"
	turnID := uuid.NewString()

	if h.Get("x-codex-beta-features") == "" {
		h.Set("x-codex-beta-features", codexBetaFeaturesValue)
	}
	if h.Get("x-codex-window-id") == "" {
		h.Set("x-codex-window-id", windowID)
	}
	if h.Get("x-codex-turn-metadata") == "" {
		meta := fmt.Sprintf(`{"installation_id":%q,"session_id":%q,"thread_id":%q,"turn_id":%q,"window_id":%q,"request_kind":"turn","thread_source":"user","sandbox":"none","turn_started_at_unix_ms":%d}`,
			installationID, sid, threadID, turnID, windowID, time.Now().UnixMilli())
		h.Set("x-codex-turn-metadata", meta)
	}
	if h.Get("x-client-request-id") == "" {
		h.Set("x-client-request-id", sid)
	}
	if h.Get("thread-id") == "" {
		h.Set("thread-id", threadID)
	}
}

// MimicStrictHeadersEnabled 导出当前是否处于 strict 伪装模式，供 wsrelay 等其他包复用，
// 使 HTTP 与 WS 两条上游路径的伪装行为保持一致。
func MimicStrictHeadersEnabled() bool {
	return CurrentRuntimeSettings().MimicStrictHeaders()
}

// StrictDefaultOriginatorValue 导出官方默认 originator（codex_cli_rs）。
func StrictDefaultOriginatorValue() string { return StrictDefaultOriginator }

// ApplyStrictCodexWSHeaders 在 strict 模式下调整 WS 握手头，使其与 HTTP 路径及官方
// codex-rs 客户端一致：originator 等于默认值时移除、会话头改用 session-id/thread-id、
// 注入 x-codex-* 客户端指纹头。仅在 strict 模式调用。
//   - hadDownstreamOriginator: 下游是否显式带了非默认官方 originator（决定是否保留）
//   - sessionID: 会话/缓存键（空则不设会话头）
func ApplyStrictCodexWSHeaders(headers http.Header, accountID, apiKey, sessionID, downstreamOriginator string, usedGeneratedHeaders bool) {
	if headers == nil {
		return
	}
	// Originator：对齐 add_originator_header
	if !usedGeneratedHeaders && strings.TrimSpace(downstreamOriginator) != "" &&
		!strings.EqualFold(strings.TrimSpace(downstreamOriginator), StrictDefaultOriginator) &&
		IsCodexOfficialClientByHeaders("", downstreamOriginator) {
		headers.Set("Originator", strings.TrimSpace(downstreamOriginator))
	} else {
		headers.Del("Originator")
	}
	// 会话头：session-id / thread-id（连字符）
	if strings.TrimSpace(sessionID) != "" {
		headers.Del("Session_id")
		headers.Del("Conversation_id")
		headers.Set("session-id", sessionID)
	}
	// x-codex-* 客户端指纹（与 HTTP 路径共用，对齐真实 codex 0.142.5）
	applyCodexClientFingerprintHeaders(headers, accountID, apiKey, sessionID)
}

// codexZstdEncoder 无状态一次性 zstd 编码器（EncodeAll 并发安全）。
var codexZstdEncoder, _ = zstd.NewWriter(nil)

func codexZstdRequestEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CODEX_ZSTD_REQUEST_BODY"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// applyCodexRequestBodyEncoding 用 zstd 压缩请求体以对齐真实 codex 0.142.5
// （content-encoding: zstd）。默认关闭——因请求体编码影响每一次请求且无法在此环境
// 用真实会话验证 200，需运营侧用金丝雀账号确认后，通过 CODEX_ZSTD_REQUEST_BODY=1 开启。
// 仅在 strict 伪装模式下生效；同时重设 GetBody 以保证回退/重试可安全重放。
func applyCodexRequestBodyEncoding(req *http.Request, rawBody []byte) {
	if req == nil || len(rawBody) == 0 {
		return
	}
	if !CurrentRuntimeSettings().MimicStrictHeaders() || !codexZstdRequestEnabled() {
		return
	}
	if strings.TrimSpace(req.Header.Get("Content-Encoding")) != "" {
		return
	}
	compressed := codexZstdEncoder.EncodeAll(rawBody, nil)
	req.Body = io.NopCloser(bytes.NewReader(compressed))
	req.ContentLength = int64(len(compressed))
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(compressed)), nil
	}
	req.Header.Set("Content-Encoding", "zstd")
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

	strict := CurrentRuntimeSettings().MimicStrictHeaders()

	userAgent, version, usedGeneratedHeaders := resolveCodexOutboundClientHeaders(account, apiKey, deviceCfg, downstreamHeaders)
	req.Header.Set("User-Agent", userAgent)

	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Connection", "Keep-Alive")
	if version != "" {
		req.Header.Set("Version", version)
	}

	// ---- Originator ----
	// strict：对齐官方 add_originator_header —— 当 originator 等于默认值(codex_cli_rs)
	// 时不发 Originator 头（服务端从 UA 解析）；仅当下游显式带了非默认的官方 originator
	// 才转发。legacy：保持历史行为，始终发 Originator（默认 codex-tui）。
	downstreamOriginator := strings.TrimSpace(downstreamHeaders.Get("Originator"))
	if strict {
		if !usedGeneratedHeaders && downstreamOriginator != "" &&
			!strings.EqualFold(downstreamOriginator, StrictDefaultOriginator) &&
			IsCodexOfficialClientByHeaders("", downstreamOriginator) {
			req.Header.Set("Originator", downstreamOriginator)
		} else {
			req.Header.Del("Originator")
		}
	} else {
		if !usedGeneratedHeaders && downstreamOriginator != "" && IsCodexOfficialClientByHeaders("", downstreamOriginator) {
			req.Header.Set("Originator", downstreamOriginator)
		} else {
			req.Header.Set("Originator", Originator)
		}
	}

	// ---- OpenAI-Beta ----
	// 实测 codex 0.142.5：HTTP /responses 回退路径**不发** OpenAI-Beta（仅 WebSocket
	// 握手发 responses_websockets=2026-02-06，见 wsrelay）。故 strict 下 HTTP 不再补该头。
	// 若下游显式带了 OpenAI-Beta 则透传（保持兼容）。

	applyCodexAllowedForwardHeaders(req, downstreamHeaders)
	if accountID != "" {
		req.Header.Set("Chatgpt-Account-Id", accountID)
	}

	// ---- 会话/线程头 ----
	// strict：对齐官方 build_session_headers —— 用连字符 session-id/thread-id。
	// legacy：保持历史 Session_id + 删除 Conversation_id。
	if cacheKey != "" {
		if strict {
			req.Header.Del("Session_id")
			req.Header.Del("Conversation_id")
			req.Header.Set("session-id", cacheKey)
		} else {
			req.Header.Set("Session_id", cacheKey)
			req.Header.Del("Conversation_id")
		}
	}

	if strict {
		applyCodexClientFingerprintHeaders(req.Header, accountID, apiKey, cacheKey)
	}
	applyAccountCustomHeaders(req, account)
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
}

// ResolveSessionID 从下游请求提取或生成 session ID
// 优先级：
//  1. Header: Session_id
//  2. Header: Conversation_id
//  3. Header: Idempotency-Key
//  4. Body:   prompt_cache_key
//  5. 基于 Bearer API Key 的确定性 UUID
func ResolveSessionID(headers http.Header, body []byte) string {
	if explicit := ResolveExplicitSessionID(headers, body); explicit != "" {
		return explicit
	}

	// 基于下游用户的 API Key 生成确定性 cache key（参考 CLIProxyAPI codex_executor.go:621）
	authHeader := ""
	if headers != nil {
		authHeader = headers.Get("Authorization")
	}
	apiKey := strings.TrimPrefix(authHeader, "Bearer ")
	apiKey = strings.TrimSpace(apiKey)
	if apiKey != "" {
		return uuid.NewSHA1(uuid.NameSpaceOID, []byte("codex2api:prompt-cache:"+apiKey)).String()
	}

	// 最后兜底：生成随机 UUID
	return uuid.New().String()
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
