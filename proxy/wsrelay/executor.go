package wsrelay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// statelessOneShotEnabled 是否禁用无显式会话请求的 WS 连接复用（每请求独享连接、
// 用完即毁）。这是杜绝一切连接级状态跨请求/跨用户泄漏的硬隔离逃生阀，代价是
// 逐请求握手（高 RPM 下可能触发上游握手限流 503）。
func statelessOneShotEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CODEX_WS_STATELESS_ONESHOT"))) {
	case "1", "true", "yes", "y", "on":
		return true
	default:
		return false
	}
}

// resolveHandshakeSessionID 决定 WS 握手头 Session_id/Conversation_id 的取值。
// 该头是逐连接冻结的：建连时发送一次，连接复用时永不更新。因此对会在多个请求
// （乃至共享同一 API Key 的多个终端用户）间复用的 stateless 连接，绝不能携带任何
// "单个请求的身份"（如每请求随机 prompt_cache_key）——若上游按连接级
// Conversation_id 绑定会话状态，第一个请求的对话身份会泄漏给后续复用该连接的
// 所有用户，造成跨用户上下文污染（issue #268/#308 同类，"用户2串到用户1的上下文"）。
//
//   - 显式会话（非 stateless）：连接按会话专用，头 = 会话 ID（原行为）。
//   - stateless + 默认隔离（poolRouteKey 非空）：返回空串 → 不发送该组头，
//     上游没有任何可绑定的连接级会话身份；逐请求身份完全由帧体内每请求唯一的
//     prompt_cache_key 承担。
//   - stateless + per-api-key 模式（poolRouteKey 为空）：沿用帧体的确定性
//     cache key（该模式显式选择按 Key 共享上游缓存，头与帧体一致才有缓存收益）。
func resolveHandshakeSessionID(sessionID, poolRouteKey string, wsBody []byte) string {
	if !proxy.IsStatelessWebsocketSessionID(sessionID) {
		return sessionID
	}
	if strings.TrimSpace(poolRouteKey) != "" {
		return ""
	}
	if cacheKey := strings.TrimSpace(gjson.GetBytes(wsBody, "prompt_cache_key").String()); cacheKey != "" {
		return cacheKey
	}
	return sessionID
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

	headerSessionID := resolveHandshakeSessionID(sessionID, poolRouteKey, wsBody)

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
	// Record the attempted handshake UA immediately so failed handshakes are
	// still auditable. A reused connection replaces this below with the UA that
	// was actually sent when that connection was established.
	proxy.RecordUpstreamUserAgent(ctx, headers.Get("User-Agent"))

	// Resin 反代：注入账号身份头
	if proxy.IsResinEnabled() {
		headers.Set("X-Resin-Account", proxy.ResinAccountID(account))
	}

	// 获取或创建连接。无显式会话的请求（stateless 连接 ID）在确定性 cache key
	// 的槽位池内复用连接，避免持续高 RPM 下逐请求握手触发上游限流。
	//
	// 连接池 baseKey 必须按 API Key 稳定，绝不能等于每请求唯一的上游身份键，否则
	// 默认隔离模式下每请求都变 → 槽位池失效 → 逐请求握手触发 503。
	// poolRouteKey（来自上游确定性键）非空时优先用它作 baseKey：连接复用按 API Key
	// 稳定命中同一组 8 槽。
	//
	// 隔离说明：默认隔离模式下，每请求的上游身份隔离由写入每个 response.create 帧体的
	// 每请求唯一 prompt_cache_key 保证（见 proxy/executor.go 注入处）。握手头里的
	// Session_id/Conversation_id 是逐连接冻结的，绝不能携带任何单个请求的身份
	// （见 resolveHandshakeSessionID）。
	//
	// CODEX_WS_STATELESS_ONESHOT=1 时禁用槽位复用：每个无会话请求独享一条连接、
	// 用完即毁（彻底杜绝任何连接级状态跨请求泄漏，代价是逐请求握手）。
	// 续链亲和：上游无服务端存储时，previous_response_id 的上下文只存活在产出
	// 该响应的那条 WS 连接里。带续链 ID 的请求优先取回原连接（独占成功才用），
	// 否则落到随机槽位会触发上游 "previous response not found"。
	poolSessionID := sessionID
	var wc *WsConnection
	var pr *PendingRequest
	var err2 error
	if prevRespID := strings.TrimSpace(gjson.GetBytes(wsBody, "previous_response_id").String()); prevRespID != "" {
		if pwc, ppr, slotKey := e.manager.AcquirePreferredConnection(prevRespID, account.ID(), apiKey); pwc != nil {
			wc, pr, poolSessionID = pwc, ppr, slotKey
		}
	}
	baseKey := strings.TrimSpace(poolRouteKey)
	if baseKey == "" && headerSessionID != sessionID {
		baseKey = headerSessionID
	}
	if wc == nil {
		if proxy.IsStatelessWebsocketSessionID(sessionID) && baseKey != "" && !statelessOneShotEnabled() {
			wc, pr, poolSessionID, err2 = e.manager.AcquireReusableConnection(ctx, account, wsURL, baseKey, sessionID, StatelessConnectionSlots, headers, proxyOverride)
		} else {
			wc, pr, err2 = e.manager.AcquireConnection(ctx, account, wsURL, sessionID, headers, proxyOverride)
		}
	}
	if err2 != nil {
		return nil, err2
	}
	if wc.upstreamUserAgentKnown {
		proxy.RecordUpstreamUserAgent(ctx, wc.upstreamUserAgent)
	}

	// 发送请求，失败时最多重试 2 次（重建连接）。
	// 用 DiscardConnection 按连接指针精确清理：续链亲和取回的连接其 PoolKey
	// 可能与当前请求的 proxy 组合不同，按参数重算 key 会漏删。
	sendErr := e.sendRequest(wc, wsBody, pr.RequestID)
	for retries := 0; shouldRetryWebsocketSendError(sendErr) && retries < 2; retries++ {
		wc.session.RemovePendingRequest(pr.RequestID)
		e.manager.DiscardConnection(wc)

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
		if wc.upstreamUserAgentKnown {
			proxy.RecordUpstreamUserAgent(ctx, wc.upstreamUserAgent)
		}
		sendErr = e.sendRequest(wc, wsBody, pr.RequestID)
	}
	if sendErr != nil {
		wc.session.RemovePendingRequest(pr.RequestID)
		e.manager.DiscardConnection(wc)
		return nil, fmt.Errorf("发送 WebSocket 请求失败: %w", sendErr)
	}

	// 启动心跳
	e.manager.StartHeartbeat(wc)

	return &WsResponse{
		conn:        wc,
		pendingReq:  pr,
		sessionID:   poolSessionID,
		manager:     e.manager,
		apiKey:      apiKey,
		readErrChan: make(chan error, 1),
	}, nil
}

func shouldRetryWebsocketSendError(err error) bool {
	if err == nil {
		return false
	}
	var closeErr *websocket.CloseError
	return !errors.As(err, &closeErr) || closeErr.Code != websocket.CloseMessageTooBig
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
	} else {
		// Keep an explicit empty header entry so net/http Request.Write suppresses
		// its implicit Go-http-client/1.1 fallback during the WS handshake.
		headers["User-Agent"] = []string{""}
	}
	if betaFeatures := strings.TrimSpace(ginHeaders.Get("X-Codex-Beta-Features")); betaFeatures != "" {
		headers.Set("X-Codex-Beta-Features", betaFeatures)
	} else if deviceCfg != nil && strings.TrimSpace(deviceCfg.BetaFeatures) != "" {
		headers.Set("X-Codex-Beta-Features", strings.TrimSpace(deviceCfg.BetaFeatures))
	}

	// Originator
	if originator := strings.TrimSpace(ginHeaders.Get("Originator")); !usedGeneratedHeaders && originator != "" && proxy.IsCodexOfficialClientByHeaders("", originator) {
		headers.Set("Originator", originator)
	} else {
		headers.Set("Originator", proxy.Originator)
	}
	// X-Oai-Attestation：DeviceCheck 设备认证头（上游 openai/codex#20619），
	// 仅在下游携带时透传，本代理不伪造（假 token 服务端验证必败，反而暴露）。
	for _, name := range []string{"X-Codex-Turn-State", "X-Codex-Turn-Metadata", "X-Client-Request-Id", "X-Responsesapi-Include-Timing-Metrics", "X-Oai-Attestation"} {
		if value := strings.TrimSpace(ginHeaders.Get(name)); value != "" {
			headers.Set(name, value)
		}
	}

	// Account ID
	if accountID != "" {
		headers.Set("Chatgpt-Account-Id", accountID)
	}
	if sessionID = strings.TrimSpace(sessionID); sessionID != "" {
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
	if err := wc.ensureReadLeaseForSend(requestID); err != nil {
		return err
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
	// apiKey 发起本请求的下游 API Key，用于 response_id → 连接绑定的归属校验。
	apiKey string
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
	if r.conn == nil {
		return fmt.Errorf("websocket connection is not available")
	}
	if r.pendingReq != nil {
		if err := r.conn.ensureReadLeaseForResponse(r.pendingReq.RequestID); err != nil {
			return fmt.Errorf("websocket connection is not available: %w", err)
		}
	}

	for {
		msgType, payload, err := r.conn.ReadMessage()
		if err != nil {
			// ReadStream only returns successfully after consuming an explicit
			// response terminal frame. Any socket close here, including 1000/1001,
			// is premature and must preserve the real close error for the consumer.
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
		// 连接级寿命限制错误：针对连接而非单个请求，这条连接上的后续
		// response.create 一律失败，而 Ping 探活仍会成功；归还池会持续毒害
		// 后续请求（含续链亲和定向回来的），必须标记销毁 (issue #346)。
		if isConnLimitErrorFrame(payload) {
			r.markConnBroken()
		}
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
		// 续链亲和：记录本响应由哪条连接产出，后续带 previous_response_id 的
		// 请求可回到原连接（上游无服务端存储时上下文只存活在连接内）。
		if eventType == "response.completed" && r.manager != nil && r.conn != nil {
			if respID := gjson.GetBytes(payload, "response.id").String(); respID != "" {
				accountID := int64(0)
				if r.conn.session != nil {
					accountID = r.conn.session.AccountID
				}
				r.manager.BindResponseConn(respID, r.conn, r.sessionID, accountID, r.apiKey)
			}
		}
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
	// 上游错误对象可能是带换行的 pretty-printed JSON，必须压缩成单行，
	// 否则经 SSE data: 行编码后下游只能读到第一行（错误信息被截断）。
	errObj := compactJSONOneLine(gjson.GetBytes(payload, "error").Raw)
	if errObj == "" {
		errObj = fmt.Sprintf(`{"message":%q,"code":%d}`, errMsg, status)
	}
	event := fmt.Sprintf(`{"type":"response.failed","response":{"status":"failed","error":%s}}`, errObj)
	if status > 0 {
		event = fmt.Sprintf(`{"type":"response.failed","response":{"status":"failed","status_code":%d,"error":%s}}`, status, errObj)
	}
	return []byte(event), true
}

// isConnLimitErrorFrame 判断上游错误帧是否为连接级寿命限制错误
// (websocket_connection_limit_reached)：该错误按连接而非按请求生效，
// 复用此连接必然继续失败。
func isConnLimitErrorFrame(payload []byte) bool {
	code := gjson.GetBytes(payload, "error.code").String()
	if code == "" {
		code = gjson.GetBytes(payload, "code").String()
	}
	return code == "websocket_connection_limit_reached"
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

// compactJSONOneLine 把可能带换行的 JSON 压缩为单行；非法 JSON 或空串返回 ""。
func compactJSONOneLine(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(raw)); err != nil {
		return ""
	}
	return buf.String()
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
	if r.conn != nil && r.conn.session != nil && r.pendingReq != nil {
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
		// 握手阶段的上游 401（token 失效/撤销）还原成真实状态码的 HTTP 响应返回，
		// 而不是 transport 错误：否则 401 在使用日志里只会以 598/transport 出现，
		// 且账号既不触发 unauthorized 冷却也不触发鉴权探针，失效账号会一直留在
		// 调度池里被反复拨号（对比 HTTP 路径的 401 直接可见并立即冷却）。
		if resp, ok := handshakeUnauthorizedHTTPResponse(err); ok {
			return resp, nil
		}
		return nil, err
	}

	// 检查 HTTP 握手响应状态。WebSocket 握手成功的标准状态是 101，
	// 但这里要包装成现有 handler 可消费的 SSE HTTP 200 响应。
	handshakeResp := wsResp.HTTPResponse()
	statusCode, handshakeHeader, handshakeFailed := normalizeWebsocketHandshakeResponse(handshakeResp)
	if handshakeFailed {
		detail := formatFailedHandshakeHTTPBody(statusCode, handshakeResp)
		wsResp.Close()
		return &http.Response{
			StatusCode: statusCode,
			Header:     handshakeHeader.Clone(),
			Body:       io.NopCloser(strings.NewReader(detail)),
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
			// SSE 的 data: 负载以换行为界，含换行的帧（如 pretty-printed JSON）
			// 必须先压缩成单行，否则下游解析器只能读到第一行。
			if bytes.IndexByte(data, '\n') >= 0 {
				if compacted := compactJSONOneLine(string(data)); compacted != "" {
					data = []byte(compacted)
				} else {
					data = bytes.ReplaceAll(data, []byte("\n"), []byte(" "))
				}
			}
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
