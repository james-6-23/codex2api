package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
	"github.com/google/uuid"
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

var clientPool sync.Map // map[string]*poolEntry, key = accountID|proxyURL

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

func clientPoolKey(account *auth.Account, proxyURL string) string {
	return fmt.Sprintf("%d|%s", account.ID(), proxyURL)
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
	key := clientPoolKey(account, proxyURL)
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

// getPooledClient 获取或创建连接池中的 HTTP Client（按账号隔离，TTL 自动淘汰）
func getPooledClient(account *auth.Account, proxyURL string) *http.Client {
	key := clientPoolKey(account, proxyURL)
	if v, ok := clientPool.Load(key); ok {
		entry := v.(*poolEntry)
		entry.touch()
		return entry.client
	}

	transport := NewUTLSTransport(proxyURL)

	entry := &poolEntry{
		client: &http.Client{
			Transport: transport,
			Timeout:   0,
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
	Originator   = "codex_cli_rs"
)

// WebsocketExecuteFunc WebSocket 执行函数（由 wsrelay 包在 main.go 中注册，避免循环依赖）
var WebsocketExecuteFunc func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header) (*http.Response, error)

// ExecuteRequest 向 Codex 上游发送请求
// sessionID 可选，用于 prompt cache 会话绑定
// useWebsocket 可选，如果为 true 则使用 WebSocket 连接
// headers 下游请求头，用于设备指纹学习
func ExecuteRequest(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, useWebsocket ...bool) (*http.Response, error) {
	// 检查是否使用 WebSocket
	if len(useWebsocket) > 0 && useWebsocket[0] && WebsocketExecuteFunc != nil {
		return WebsocketExecuteFunc(ctx, account, requestBody, sessionID, proxyOverride, apiKey, deviceCfg, headers)
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
	requestBody, _ = sjson.DeleteBytes(requestBody, "prompt_cache_retention")
	requestBody, _ = sjson.DeleteBytes(requestBody, "safety_identifier")
	requestBody, _ = sjson.DeleteBytes(requestBody, "disable_response_storage")

	// 3. 注入 prompt_cache_key（如果请求体中没有，且 sessionID 不为空）
	existingCacheKey := strings.TrimSpace(gjson.GetBytes(requestBody, "prompt_cache_key").String())
	cacheKey := existingCacheKey
	if cacheKey == "" && sessionID != "" {
		cacheKey = sessionID
		requestBody, _ = sjson.SetBytes(requestBody, "prompt_cache_key", cacheKey)
	}

	endpoint := CodexBaseURL + "/responses"

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(requestBody))
	if err != nil {
		return nil, ErrInternalError("创建请求失败", err)
	}

	// ==================== 请求头（伪装 Codex CLI） ====================
	applyCodexRequestHeaders(req, account, accessToken, cacheKey, apiKey, deviceCfg, headers)

	// 获取连接池 HTTP 客户端（账号级隔离，复用 TCP/TLS 连接）
	client := getPooledClient(account, proxyURL)

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

	var profile deviceProfile
	version := ""
	if IsDeviceProfileStabilizationEnabled(deviceCfg) {
		profile = ResolveDeviceProfile(account, apiKey, downstreamHeaders, deviceCfg)
		ApplyDeviceProfileHeaders(req, profile)
		version = codexVersionFromProfile(profile, strings.TrimSpace(deviceCfg.PackageVersion))
	} else {
		clientProfile := ProfileForAccount(account.ID())
		req.Header.Set("User-Agent", clientProfile.UserAgent)
		version = clientProfile.Version
	}

	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Connection", "Keep-Alive")
	if version != "" {
		req.Header.Set("Version", version)
	}
	if originator := strings.TrimSpace(downstreamHeaders.Get("Originator")); originator != "" {
		req.Header.Set("Originator", originator)
	} else {
		req.Header.Set("Originator", Originator)
	}
	if accountID != "" {
		req.Header.Set("Chatgpt-Account-Id", accountID)
	}
	if cacheKey != "" {
		req.Header.Set("Session_id", cacheKey)
		req.Header.Del("Conversation_id")
	}
}

// ResolveSessionID 从下游请求提取或生成 session ID
// 优先级：
//  1. Header: Session_id
//  2. Header: Conversation_id
//  3. Header: Idempotency-Key
//  4. Body:   prompt_cache_key
//  5. 基于 Bearer API Key 的确定性 UUID
func ResolveSessionID(headers http.Header, body []byte) string {
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
