package wsrelay

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ==================== WebSocket 执行器常量 ====================

const (
	// Beta header 用于启用 WebSocket 响应 API
	responsesWebsocketBetaHeader = "responses_websockets=2026-02-06"

	// Codex WebSocket 端点
	CodexWsEndpoint = "/responses"
)

func shouldSendWebsocketUserAgent() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CODEX_WS_SEND_USER_AGENT"))) {
	case "0", "false", "no", "n", "off":
		return false
	default:
		return true
	}
}

// ==================== WebSocket 执行器 ====================

// Executor WebSocket 执行器
type Executor struct {
	manager *Manager
	mu      sync.RWMutex
}

// NewExecutor 创建 WebSocket 执行器
func NewExecutor() *Executor {
	return &Executor{
		manager: GetManager(),
	}
}

// NewExecutorWithManager 创建带指定管理器的执行器
func NewExecutorWithManager(manager *Manager) *Executor {
	return &Executor{
		manager: manager,
	}
}

// ExecuteRequestViaWebsocket 通过 WebSocket 发送请求
func (e *Executor) ExecuteRequestViaWebsocket(
	ctx context.Context,
	account *auth.Account,
	requestBody []byte,
	sessionID string,
	proxyOverride string,
	apiKey string,
	deviceCfg *proxy.DeviceProfileConfig,
	ginHeaders http.Header,
	poolRouteKey string,
) (*WsResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}

	account.Mu().RLock()
	accessToken := account.AccessToken
	accountIDStr := account.AccountID
	account.Mu().RUnlock()

	if accessToken == "" {
		return nil, fmt.Errorf("无可用 access_token")
	}

	// 准备请求体
	wsBody := e.prepareWebsocketBody(requestBody, sessionID)

	// 握手头中的 Session_id/Conversation_id 会影响上游 prompt cache 路由，必须与
	// 请求体的确定性 prompt_cache_key 一致；stateless 连接 ID 是每请求随机的，
	// 发给上游会导致 prompt cache 永远 miss，它只用于本地连接池隔离。
	headerSessionID := sessionID
	if proxy.IsStatelessWebsocketSessionID(sessionID) {
		if cacheKey := strings.TrimSpace(gjson.GetBytes(wsBody, "prompt_cache_key").String()); cacheKey != "" {
			headerSessionID = cacheKey
		}
	}

	// 构建 WebSocket URL
	httpURL := proxy.CodexBaseURL + CodexWsEndpoint
	wsURL, err := buildWebsocketURL(httpURL)
	if err != nil {
		return nil, fmt.Errorf("构建 WebSocket URL 失败: %w", err)
	}

	// Resin 反向代理：改写 WS URL 为 Resin 反代地址
	if proxy.IsResinEnabled() {
		wsURL = proxy.BuildWebSocketURL(wsURL)
	}

	// 准备请求头
	headers := e.prepareWebsocketHeaders(accessToken, account, accountIDStr, headerSessionID, apiKey, deviceCfg, ginHeaders)

	// Resin 反代：注入账号身份头
	if proxy.IsResinEnabled() {
		headers.Set("X-Resin-Account", proxy.ResinAccountID(account))
	}

	// 获取或创建连接。无显式会话的请求（stateless 连接 ID）在确定性 cache key
	// 的槽位池内复用连接，避免持续高 RPM 下逐请求握手触发上游限流。
	//
	// 连接池 baseKey 必须按 API Key 稳定，绝不能等于每请求唯一的上游身份键，否则
	// 默认隔离模式下 headerSessionID 每请求都变 → 槽位池失效 → 逐请求握手触发 503。
	// poolRouteKey（来自上游确定性键）非空时优先用它作 baseKey：连接复用按 API Key
	// 稳定命中同一组 8 槽。
	//
	// 隔离说明：默认隔离模式下，每请求的上游身份隔离由写入每个 response.create 帧体的
	// 每请求唯一 prompt_cache_key 保证（见 proxy/executor.go 注入处）。握手头里的
	// Session_id/Conversation_id 只在建连时发送一次、对一条复用连接的生命周期保持不变
	// （复用连接上不是逐请求轮换），因此不能依赖它做逐请求隔离。
	poolSessionID := sessionID
	effectiveProxy := effectiveProxyURL(account, proxyOverride)
	var wc *WsConnection
	var pr *PendingRequest
	var err2 error
	if proxy.IsStatelessWebsocketSessionID(sessionID) && headerSessionID != sessionID {
		baseKey := headerSessionID
		if strings.TrimSpace(poolRouteKey) != "" {
			baseKey = poolRouteKey
		}
		wc, pr, poolSessionID, err2 = e.manager.AcquireReusableConnection(ctx, account, wsURL, baseKey, sessionID, StatelessConnectionSlots, headers, proxyOverride)
	} else {
		wc, pr, err2 = e.manager.AcquireConnection(ctx, account, wsURL, sessionID, headers, proxyOverride)
	}
	if err2 != nil {
		return nil, err2
	}

	// 发送请求，失败时最多重试 2 次（重建连接）
	sendErr := e.sendRequest(wc, wsBody, pr.RequestID)
	for retries := 0; sendErr != nil && retries < 2; retries++ {
		wc.session.RemovePendingRequest(pr.RequestID)
		e.manager.RemoveConnection(account.ID(), wsURL, poolSessionID, effectiveProxy)

		// 短暂退避，避免瞬间重连风暴
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(retries+1) * 200 * time.Millisecond):
		}

		wc, pr, err2 = e.manager.AcquireConnection(ctx, account, wsURL, poolSessionID, headers, proxyOverride)
		if err2 != nil {
			return nil, err2
		}
		sendErr = e.sendRequest(wc, wsBody, pr.RequestID)
	}
	if sendErr != nil {
		wc.session.RemovePendingRequest(pr.RequestID)
		e.manager.RemoveConnection(account.ID(), wsURL, poolSessionID, effectiveProxy)
		return nil, fmt.Errorf("发送 WebSocket 请求失败: %w", sendErr)
	}

	// 启动心跳
	e.manager.StartHeartbeat(wc)

	return &WsResponse{
		conn:        wc,
		pendingReq:  pr,
		sessionID:   poolSessionID,
		manager:     e.manager,
		readErrChan: make(chan error, 1),
	}, nil
}

// prepareWebsocketBody 准备 WebSocket 请求体
func (e *Executor) prepareWebsocketBody(body []byte, sessionID string) []byte {
	if len(body) == 0 {
		return nil
	}

	// 克隆并修改请求体
	wsBody := bytes.Clone(body)

	// 1. 确保 instructions 字段存在
	if !gjson.GetBytes(wsBody, "instructions").Exists() {
		wsBody, _ = sjson.SetBytes(wsBody, "instructions", "")
	}

	// 2. 清理多余字段（prompt_cache_retention 上游不接受，会返回 400 Unsupported parameter，必须删除）
	wsBody, _ = sjson.DeleteBytes(wsBody, "prompt_cache_retention")
	wsBody, _ = sjson.DeleteBytes(wsBody, "safety_identifier")
	wsBody, _ = sjson.DeleteBytes(wsBody, "disable_response_storage")

	// 3. 注入 prompt_cache_key
	// stateless sessionID 只是连接池隔离用的一次性随机 ID，注入它会让上游
	// prompt cache 每次请求都 miss；此时保留请求体中已有的确定性 cache key
	//（由 proxy.ExecuteRequest 注入或客户端自带）。
	existingCacheKey := strings.TrimSpace(gjson.GetBytes(wsBody, "prompt_cache_key").String())
	if sessionID != "" && !proxy.IsStatelessWebsocketSessionID(sessionID) {
		wsBody, _ = sjson.SetBytes(wsBody, "prompt_cache_key", sessionID)
	} else if existingCacheKey != "" {
		wsBody, _ = sjson.SetBytes(wsBody, "prompt_cache_key", existingCacheKey)
	}

	// 4. 设置请求类型和 stream
	wsBody, _ = sjson.SetBytes(wsBody, "type", "response.create")
	wsBody, _ = sjson.SetBytes(wsBody, "stream", true)

	return wsBody
}

// prepareWebsocketHeaders 准备 WebSocket 请求头
func (e *Executor) prepareWebsocketHeaders(accessToken string, account *auth.Account, accountID, sessionID, apiKey string, deviceCfg *proxy.DeviceProfileConfig, ginHeaders http.Header) http.Header {
	headers := http.Header{}

	// 认证头
	headers.Set("Authorization", "Bearer "+accessToken)

	// Beta header 启用 WebSocket 响应 API
	headers.Set("OpenAI-Beta", responsesWebsocketBetaHeader)

	usedGeneratedHeaders := false
	if shouldSendWebsocketUserAgent() {
		if account == nil {
			account = &auth.Account{AccountID: accountID}
		}
		var userAgent, version string
		userAgent, version, usedGeneratedHeaders = proxy.ResolveCodexOutboundClientHeadersWithDecision(account, apiKey, deviceCfg, ginHeaders)
		headers.Set("User-Agent", userAgent)
		if version != "" {
			headers.Set("Version", version)
		}
	}
	if betaFeatures := strings.TrimSpace(ginHeaders.Get("X-Codex-Beta-Features")); betaFeatures != "" {
		headers.Set("X-Codex-Beta-Features", betaFeatures)
	} else if deviceCfg != nil && strings.TrimSpace(deviceCfg.BetaFeatures) != "" {
		headers.Set("X-Codex-Beta-Features", strings.TrimSpace(deviceCfg.BetaFeatures))
	}

	strict := proxy.MimicStrictHeadersEnabled()
	downstreamOriginator := strings.TrimSpace(ginHeaders.Get("Originator"))

	// Originator（legacy：始终发；strict：由 ApplyStrictCodexWSHeaders 统一处理）
	if !strict {
		if !usedGeneratedHeaders && downstreamOriginator != "" && proxy.IsCodexOfficialClientByHeaders("", downstreamOriginator) {
			headers.Set("Originator", downstreamOriginator)
		} else {
			headers.Set("Originator", proxy.Originator)
		}
	}
	for _, name := range []string{"X-Codex-Turn-State", "X-Codex-Turn-Metadata", "X-Client-Request-Id", "X-Responsesapi-Include-Timing-Metrics"} {
		if value := strings.TrimSpace(ginHeaders.Get(name)); value != "" {
			headers.Set(name, value)
		}
	}

	// Account ID
	if accountID != "" {
		headers.Set("Chatgpt-Account-Id", accountID)
	}

	sessionID = strings.TrimSpace(sessionID)
	if strict {
		// strict：originator 条件移除 + session-id/thread-id + x-codex-* 指纹，
		// 与 HTTP 路径及官方 codex-rs 客户端逐字节一致。
		proxy.ApplyStrictCodexWSHeaders(headers, accountID, apiKey, sessionID, downstreamOriginator, usedGeneratedHeaders)
	} else if sessionID != "" {
		headers.Set("Session_id", sessionID)
		headers.Set("Conversation_id", sessionID)
	}
	for name, value := range account.GetCustomHeaders() {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		headers.Set(name, value)
	}

	return headers
}

// sendRequest 发送 WebSocket 请求
func (e *Executor) sendRequest(wc *WsConnection, body []byte, requestID string) error {
	if !wc.IsConnected() {
		return fmt.Errorf("websocket connection is not connected")
	}
	return wc.WriteMessage(websocket.TextMessage, body)
}

// ==================== WebSocket 响应处理 ====================

// WsResponse WebSocket 响应包装器
type WsResponse struct {
	conn        *WsConnection
	pendingReq  *PendingRequest
	sessionID   string
	manager     *Manager
	readErrChan chan error
	closed      bool
	// connBroken 标记读流因上游 WS 异常(非正常关闭)或下游写入失败而终止；
	// Close() 据此销毁坏连接而非归还连接池复用。受 mu 保护。
	connBroken bool
	// streamCompleted 标记读流已消费到明确的终止边界(response.completed /
	// response.failed / 上游 error 帧)。Close() 只在此标记为 true 且未标记
	// connBroken 时才归还连接复用；其余情况(下游断开、ctx 取消、上游关闭、
	// 握手失败后未读流等)上游可能仍在该连接上推送残留帧，归还复用会把上一个
	// 请求的响应串给下一个用户(issue #308)，必须销毁。受 mu 保护。
	streamCompleted bool
	mu              sync.Mutex
}

// ReadStream 读取 SSE 流
func (r *WsResponse) ReadStream(callback func(data []byte) bool) error {
	if r.conn == nil || !r.conn.IsConnected() {
		return fmt.Errorf("websocket connection is not available")
	}

	for {
		msgType, payload, err := r.conn.ReadMessage()
		if err != nil {
			// 检查是否是正常关闭
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway) {
				return nil
			}
			// 非正常关闭(close 1006/1009/1011、broken pipe、unexpected EOF、读超时等)：
			// 连接已不可靠，标记为坏连接，Close() 时销毁并移出连接池，避免复用与 CLOSE_WAIT 滞留。
			r.markConnBroken()
			return fmt.Errorf("websocket read error: %w", err)
		}

		// 只处理文本消息
		if msgType != websocket.TextMessage {
			if msgType == websocket.BinaryMessage {
				return fmt.Errorf("unexpected binary message from websocket")
			}
			continue
		}

		// 清理消息
		payload = bytes.TrimSpace(payload)
		if len(payload) == 0 {
			continue
		}

		// 解析并处理消息
		if err := r.handleMessage(payload, callback); err != nil {
			if err == io.EOF {
				// 到达终止边界(完成/失败/错误帧)。若中途下游写入失败已标记
				// connBroken，Close() 仍会销毁连接。
				r.markStreamCompleted()
				return nil
			}
			return err
		}
	}
}

// handleMessage 处理单条 WebSocket 消息
func (r *WsResponse) handleMessage(payload []byte, callback func(data []byte) bool) error {
	// 上游错误帧：透传给下游(转成 SSE 错误事件)，而不是转成 Go error 后静默关闭 pipe。
	// 否则下游只会读到一个底层 read error → 表现为空响应，无从得知具体错误。
	if errEvent, isErr := r.buildErrorEvent(payload); isErr {
		// 把错误内容作为 SSE 数据写给下游，让客户端看到完整错误 JSON。
		callback(errEvent)
		// 错误即终止：结束流(等价于 response.failed)。
		return io.EOF
	}

	// 标准化完成事件类型
	payload = normalizeCompletionEvent(payload)

	// 调用回调
	if !callback(payload) {
		// 下游写入失败(broken pipe / 客户端断开)：响应流在非终止边界被截断，
		// 上游仍会在这条连接上继续推送本响应的剩余帧。连接必须销毁，
		// 归还池中复用会把残留帧串给下一个请求(issue #308)。
		r.markConnBroken()
		return io.EOF
	}

	// 检查是否是终止事件
	eventType := gjson.GetBytes(payload, "type").String()
	if eventType == "response.completed" || eventType == "response.failed" {
		return io.EOF
	}

	return nil
}

// buildErrorEvent 判断 payload 是否为上游错误帧；若是，返回一个下游可识别的
// response.failed SSE 事件(保留原始错误内容)，第二个返回值标记是否为错误帧。
func (r *WsResponse) buildErrorEvent(payload []byte) ([]byte, bool) {
	if len(payload) == 0 {
		return nil, false
	}
	if gjson.GetBytes(payload, "type").String() != "error" {
		return nil, false
	}

	status := int(gjson.GetBytes(payload, "status").Int())
	if status == 0 {
		status = int(gjson.GetBytes(payload, "status_code").Int())
	}

	errMsg := gjson.GetBytes(payload, "error.message").String()
	if errMsg == "" {
		errMsg = gjson.GetBytes(payload, "message").String()
	}
	if errMsg == "" && status > 0 {
		errMsg = http.StatusText(status)
	}
	if errMsg == "" {
		errMsg = "upstream websocket error"
	}

	// 构造 response.failed 事件：下游 ReadSSEStream 已识别该类型为终止失败，
	// 与 HTTP 路径的错误语义对齐；同时保留原始上游错误对象供客户端排查。
	errObj := gjson.GetBytes(payload, "error").Raw
	if errObj == "" {
		errObj = fmt.Sprintf(`{"message":%q,"code":%d}`, errMsg, status)
	}
	event := fmt.Sprintf(`{"type":"response.failed","response":{"status":"failed","error":%s}}`, errObj)
	if status > 0 {
		event = fmt.Sprintf(`{"type":"response.failed","response":{"status":"failed","status_code":%d,"error":%s}}`, status, errObj)
	}
	return []byte(event), true
}

// normalizeCompletionEvent 标准化完成事件类型
func normalizeCompletionEvent(payload []byte) []byte {
	if gjson.GetBytes(payload, "type").String() == "response.done" {
		updated, err := sjson.SetBytes(payload, "type", "response.completed")
		if err == nil && len(updated) > 0 {
			return updated
		}
	}
	return payload
}

// markConnBroken 标记底层连接因上游 WS 异常或下游写入失败而不可复用（幂等，受 mu 保护）。
func (r *WsResponse) markConnBroken() {
	r.mu.Lock()
	r.connBroken = true
	r.mu.Unlock()
}

// markStreamCompleted 标记读流已消费到明确的终止边界（幂等，受 mu 保护）。
func (r *WsResponse) markStreamCompleted() {
	r.mu.Lock()
	r.streamCompleted = true
	r.mu.Unlock()
}

// Close 关闭响应并归还连接
func (r *WsResponse) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.closed {
		return nil
	}

	r.closed = true

	// 移除等待请求
	if r.conn != nil && r.conn.session != nil {
		r.conn.session.RemovePendingRequest(r.pendingReq.RequestID)
	}

	// 根据读流的结束方式决定连接去向：
	//   - 读到终止边界(completed/failed/error 帧)且无异常：归还连接池继续复用。
	//   - 其余任何情况一律销毁并移出连接池：
	//     * 上游 WS 异常(close 1006/1009/1011、read error) → 坏连接复用会断流且 fd 滞留 CLOSE_WAIT；
	//     * 下游写入失败 / ctx 取消 / 上游正常关闭 / 握手失败后未读流 → 流没消费到边界，
	//       上游可能仍在推送残留帧，复用会串会话(issue #308)。
	if r.conn != nil {
		if !r.connBroken && r.streamCompleted {
			r.manager.ReleaseConnection(r.conn)
		} else {
			r.manager.DiscardConnection(r.conn)
		}
	}

	return nil
}

// HTTPResponse 返回 HTTP 握手响应
func (r *WsResponse) HTTPResponse() *http.Response {
	if r.conn != nil {
		return r.conn.HTTPResponse()
	}
	return nil
}

// ==================== 辅助函数 ====================

// buildWebsocketURL 从 HTTP URL 构建 WebSocket URL
func buildWebsocketURL(httpURL string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(httpURL))
	if err != nil {
		return "", err
	}

	switch strings.ToLower(parsed.Scheme) {
	case "http":
		parsed.Scheme = "ws"
	case "https":
		parsed.Scheme = "wss"
	}

	return parsed.String(), nil
}

// ==================== 全局执行器实例 ====================

var globalExecutor *Executor
var executorOnce sync.Once

// GetExecutor 获取全局执行器实例
func GetExecutor() *Executor {
	executorOnce.Do(func() {
		globalExecutor = NewExecutor()
	})
	return globalExecutor
}

// ShutdownExecutor 关闭全局执行器和管理器
func ShutdownExecutor() {
	ShutdownManager()
}

// ExecuteRequestWebsocket 通过 WebSocket 发送请求
// 返回一个模拟的 http.Response 用于兼容现有代码
func ExecuteRequestWebsocket(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *proxy.DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
	exec := GetExecutor()
	wsResp, err := exec.ExecuteRequestViaWebsocket(ctx, account, requestBody, sessionID, proxyOverride, apiKey, deviceCfg, headers, poolRouteKey)
	if err != nil {
		return nil, err
	}

	// 检查 HTTP 握手响应状态。WebSocket 握手成功的标准状态是 101，
	// 但这里要包装成现有 handler 可消费的 SSE HTTP 200 响应。
	statusCode, handshakeHeader, handshakeFailed := normalizeWebsocketHandshakeResponse(wsResp.HTTPResponse())
	if handshakeFailed {
		wsResp.Close()
		return &http.Response{
			StatusCode: statusCode,
			Header:     handshakeHeader.Clone(),
			Body:       io.NopCloser(strings.NewReader(fmt.Sprintf("websocket handshake failed: %d", statusCode))),
		}, nil
	}

	return websocketResponseToHTTP(ctx, wsResp, statusCode, handshakeHeader), nil
}

func websocketResponseToHTTP(ctx context.Context, wsResp *WsResponse, statusCode int, handshakeHeader http.Header) *http.Response {
	if ctx == nil {
		ctx = context.Background()
	}

	pr, pw := io.Pipe()
	resp := &http.Response{
		StatusCode: statusCode,
		Header:     make(http.Header),
		Body:       pr,
	}

	// 从 HTTP 握手响应中复制头信息
	if handshakeHeader != nil {
		for key, values := range handshakeHeader {
			for _, v := range values {
				resp.Header.Add(key, v)
			}
		}
	}

	// 设置 SSE 响应头
	resp.Header.Set("Content-Type", "text/event-stream")
	resp.Header.Set("Cache-Control", "no-cache")
	resp.Header.Set("Connection", "keep-alive")

	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			// 先关 pipe 再关 WS 响应：pipe 以第一个错误为准，保证下游读到的是
			// cancellation 而不是随后销毁连接引发的 read error。
			_ = pw.CloseWithError(ctx.Err())
			_ = wsResp.Close()
		case <-done:
		}
	}()

	// 在后台读取 WebSocket 流并写入 pipe
	go func() {
		defer close(done)
		defer pw.Close()
		defer wsResp.Close()

		err := wsResp.ReadStream(func(data []byte) bool {
			// 将数据编码为 SSE 格式
			if _, err := pw.Write([]byte("data: ")); err != nil {
				return false
			}
			if _, err := pw.Write(data); err != nil {
				return false
			}
			if _, err := pw.Write([]byte("\n\n")); err != nil {
				return false
			}
			return true
		})

		if err != nil && err != io.EOF {
			pw.CloseWithError(err)
		}
	}()

	return resp
}

func normalizeWebsocketHandshakeResponse(handshakeResp *http.Response) (statusCode int, header http.Header, failed bool) {
	if handshakeResp == nil {
		return http.StatusOK, http.Header{}, false
	}

	statusCode = handshakeResp.StatusCode
	header = handshakeResp.Header
	if statusCode == http.StatusSwitchingProtocols || (statusCode >= 200 && statusCode < 300) {
		return http.StatusOK, header, false
	}
	return statusCode, header, true
}
