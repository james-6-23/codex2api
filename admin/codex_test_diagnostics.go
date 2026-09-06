package admin

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/security/promptfilter"
	"github.com/tidwall/gjson"
)

// Codex 测连诊断:记录本次 Responses 探针的 HTTP 状态、分段耗时、上游返回的
// 请求/响应标识、x-codex-* 用量窗口头、终态 usage,以及脱敏后的响应头/原始正文。
// 只描述本次请求,未观测到的字段保持缺省,不用零值顶替。

const codexTestBodyLimit = 64 << 10

type codexTestHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// codexTestWindow 是 x-codex-primary-* / x-codex-secondary-* 三件套的原样投影,
// window_minutes 决定它是 5h(300)还是 7d(10080)窗口,由前端按分钟数标注。
type codexTestWindow struct {
	UsedPercent       *float64 `json:"used_percent,omitempty"`
	WindowMinutes     *float64 `json:"window_minutes,omitempty"`
	ResetAfterSeconds *float64 `json:"reset_after_seconds,omitempty"`
}

func (w *codexTestWindow) empty() bool {
	return w == nil || (w.UsedPercent == nil && w.WindowMinutes == nil && w.ResetAfterSeconds == nil)
}

type codexTestUsage struct {
	InputTokens     *int64 `json:"input_tokens,omitempty"`
	OutputTokens    *int64 `json:"output_tokens,omitempty"`
	TotalTokens     *int64 `json:"total_tokens,omitempty"`
	CachedTokens    *int64 `json:"cached_input_tokens,omitempty"`
	ReasoningTokens *int64 `json:"reasoning_output_tokens,omitempty"`
}

type codexTestDiagnostics struct {
	HTTPStatus     int    `json:"http_status,omitempty"`
	DurationMS     *int64 `json:"duration_ms,omitempty"`
	HeadersMS      *int64 `json:"headers_ms,omitempty"`
	FirstContentMS *int64 `json:"first_content_ms,omitempty"`
	Model          string `json:"model"`
	ResponseModel  string `json:"response_model,omitempty"`
	Transport      string `json:"transport,omitempty"`
	RequestID      string `json:"request_id,omitempty"`
	ResponseID     string `json:"response_id,omitempty"`
	CFRay          string `json:"cf_ray,omitempty"`
	PlanType       string `json:"plan_type,omitempty"`
	// 安全缓冲:上游可能为额外审查而扣住输出;enabled 只说明该模型开着这项能力,
	// faster_model 是官方 CLI "Retry with a faster model" 的切换目标,buffered
	// 才表示本轮真的被缓冲过(事件级 safety_buffering=true)。
	SafetyBufferingEnabled     *bool             `json:"safety_buffering_enabled,omitempty"`
	SafetyBufferingFasterModel string            `json:"safety_buffering_faster_model,omitempty"`
	SafetyBuffered             bool              `json:"safety_buffered,omitempty"`
	ResponseStatus             string            `json:"response_status,omitempty"`
	IncompleteReason           string            `json:"incomplete_reason,omitempty"`
	ErrorType                  string            `json:"error_type,omitempty"`
	ErrorCode                  string            `json:"error_code,omitempty"`
	PrimaryWindow              *codexTestWindow  `json:"primary_window,omitempty"`
	SecondaryWindow            *codexTestWindow  `json:"secondary_window,omitempty"`
	Usage                      *codexTestUsage   `json:"usage,omitempty"`
	ResponseHeaders            []codexTestHeader `json:"response_headers,omitempty"`
	ResponseBody               string            `json:"response_body,omitempty"`
	BodyTruncated              bool              `json:"body_truncated,omitempty"`
}

// codexTestCapture 旁路留存上游正文预览;超限只打截断标记,绝不截断真正被
// 解析器消费的流。
type codexTestCapture struct {
	bytes.Buffer
	limit     int
	truncated bool
}

func (b *codexTestCapture) Write(p []byte) (int, error) {
	n := len(p)
	remaining := max(0, b.limit-b.Len())
	if len(p) > remaining {
		b.truncated = true
		p = p[:remaining]
	}
	_, _ = b.Buffer.Write(p)
	return n, nil
}

var codexTestProxyCredentials = regexp.MustCompile(`(?i)((?:https?|socks5h?)://)[^\s/]+@`)

// codexTestSecrets 收集本次探针可能出现在响应正文/头里的账号凭据,用于脱敏。
func codexTestSecrets(account *auth.Account) []string {
	if account == nil {
		return nil
	}
	secrets := make([]string, 0, 2)
	if token := account.GetAccessToken(); token != "" {
		secrets = append(secrets, token)
	}
	account.Mu().RLock()
	apiKey := strings.TrimSpace(account.APIKey)
	account.Mu().RUnlock()
	if apiKey != "" {
		secrets = append(secrets, apiKey)
	}
	return secrets
}

func sanitizeCodexTestText(text string, secrets []string) string {
	for _, secret := range secrets {
		if secret != "" {
			text = strings.ReplaceAll(text, secret, "[REDACTED]")
		}
	}
	text = codexTestProxyCredentials.ReplaceAllString(text, "${1}[REDACTED]@")
	return promptfilter.RedactSensitive(text)
}

type codexTestRecorder struct {
	details *codexTestDiagnostics
	start   time.Time
	secrets []string
	capture codexTestCapture
}

func newCodexTestRecorder(resp *http.Response, model string, account *auth.Account, start time.Time) *codexTestRecorder {
	secrets := codexTestSecrets(account)
	lookahead := 0
	for _, secret := range secrets {
		lookahead = max(lookahead, len(secret))
	}
	r := &codexTestRecorder{
		details: &codexTestDiagnostics{Model: model},
		start:   start,
		secrets: secrets,
		// 多留一段,保证跨越预览边界的凭据也能被整体替换。
		capture: codexTestCapture{limit: codexTestBodyLimit + lookahead},
	}
	if resp == nil {
		return r
	}
	r.details.HTTPStatus = resp.StatusCode
	ms := max(int64(0), time.Since(start).Milliseconds())
	r.details.HeadersMS = &ms
	r.details.Transport = codexTestTransport(resp.Header)
	r.details.RequestID = r.safeValue(codexTestRequestID(resp.Header, account))
	r.details.CFRay = r.safeValue(resp.Header.Get("cf-ray"))
	r.details.PlanType = r.safeValue(resp.Header.Get("x-codex-plan-type"))
	r.details.PrimaryWindow = parseCodexTestWindowHeaders(resp.Header, "x-codex-primary-")
	r.details.SecondaryWindow = parseCodexTestWindowHeaders(resp.Header, "x-codex-secondary-")
	r.observeSafetyBufferingHeaders(resp.Header)
	r.appendHeaders(resp.Header)
	if resp.Body != nil {
		resp.Body = struct {
			io.Reader
			io.Closer
		}{io.TeeReader(resp.Body, &r.capture), resp.Body}
	}
	return r
}

// codexTestTransport 区分 HTTP 直连与强制 WebSocket:WS 路径合成的响应头是 101
// 握手头,带 Upgrade/Sec-WebSocket-Accept,此时 x-codex-* 用量会以 codex.rate_limits
// 帧而非响应头出现。
func codexTestTransport(header http.Header) string {
	if strings.EqualFold(strings.TrimSpace(header.Get("Upgrade")), "websocket") || header.Get("Sec-Websocket-Accept") != "" {
		return "websocket"
	}
	return "http"
}

// codexTestRequestID 优先取账号自定义的上游请求 ID 头,再回退到常见命名。
func codexTestRequestID(header http.Header, account *auth.Account) string {
	if override := strings.TrimSpace(account.GetUpstreamRequestIDHeader()); override != "" && auth.ValidateUpstreamRequestIDHeader(override) == nil {
		if value := strings.TrimSpace(header.Get(override)); value != "" {
			return value
		}
	}
	for _, name := range []string{"x-request-id", "request-id", "x-openai-request-id", "x-oai-request-id", "x-goog-request-id"} {
		if value := strings.TrimSpace(header.Get(name)); value != "" {
			return value
		}
	}
	return ""
}

// observeSafetyBufferingHeaders 读取 x-codex-safety-buffering-* 头;缺席保持缺省。
func (r *codexTestRecorder) observeSafetyBufferingHeaders(header http.Header) {
	if raw := strings.TrimSpace(header.Get("x-codex-safety-buffering-enabled")); raw != "" {
		if enabled, err := strconv.ParseBool(raw); err == nil {
			r.details.SafetyBufferingEnabled = &enabled
		}
	}
	if model := strings.TrimSpace(header.Get("x-codex-safety-buffering-faster-model")); model != "" {
		r.details.SafetyBufferingFasterModel = r.safeValue(model)
	}
}

func (r *codexTestRecorder) appendHeaders(header http.Header) {
	keys := make([]string, 0, len(header))
	for key := range header {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		name := strings.ToLower(key)
		if !codexTestHeaderAllowed(name) {
			continue
		}
		for _, value := range header[key] {
			if len(r.details.ResponseHeaders) >= 64 {
				return
			}
			r.details.ResponseHeaders = append(r.details.ResponseHeaders, codexTestHeader{Name: name, Value: r.safeValue(value)})
		}
	}
}

// codexTestHeaderAllowed 只放行诊断价值明确的响应头:用量窗口、限流、请求标识、
// 计时与错误摘要;Cookie/认证类头永远不进白名单。
func codexTestHeaderAllowed(name string) bool {
	for _, prefix := range []string{"x-codex-", "x-ratelimit-", "openai-"} {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	switch name {
	case "request-id", "x-request-id", "x-openai-request-id", "x-oai-request-id", "x-goog-request-id",
		"retry-after", "content-type", "date", "server-timing", "cf-ray", "cf-cache-status", "x-should-retry",
		"x-error-code", "x-error-message", "www-authenticate", "upgrade":
		return true
	}
	return false
}

func parseCodexTestWindowHeaders(header http.Header, prefix string) *codexTestWindow {
	window := &codexTestWindow{
		UsedPercent:       parseCodexTestFloat(header.Get(prefix + "used-percent")),
		WindowMinutes:     parseCodexTestFloat(header.Get(prefix + "window-minutes")),
		ResetAfterSeconds: parseCodexTestFloat(header.Get(prefix + "reset-after-seconds")),
	}
	if window.empty() {
		return nil
	}
	return window
}

func parseCodexTestFloat(raw string) *float64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil || value < 0 {
		return nil
	}
	return &value
}

func (r *codexTestRecorder) safeValue(value string) string {
	return truncate(sanitizeCodexTestText(value, r.secrets), 2048)
}

func (r *codexTestRecorder) contentReceived() {
	if r.details.FirstContentMS == nil {
		ms := max(int64(0), time.Since(r.start).Milliseconds())
		r.details.FirstContentMS = &ms
	}
}

// observe 从一帧 Responses SSE 事件(或非 200 的 JSON 错误正文)里提取响应标识、
// 终态、错误类型与 usage。事件 usage 是终态全量值,直接覆盖而不累加。
func (r *codexTestRecorder) observe(data []byte) {
	event := gjson.ParseBytes(data)
	if !event.IsObject() {
		return
	}
	eventType := event.Get("type").String()
	switch eventType {
	case "codex.rate_limits":
		r.observeRateLimitsFrame(event)
		return
	case "codex.response.metadata", "response.metadata":
		r.observeMetadataFrame(event)
		return
	}
	if flag := event.Get("safety_buffering"); flag.Type == gjson.True {
		r.details.SafetyBuffered = true
	}
	response := event.Get("response")
	// 生命周期字段(id/model/status)只认 Responses 对象:流式事件的 response 包裹,
	// 或非流式顶层 object=response。4xx 错误正文的顶层 status 是数字 HTTP 码,
	// 流内 error 帧也可能带数字 status,都不能混进 response_status。
	lifecycle := response.IsObject()
	if !lifecycle {
		response = event
		lifecycle = event.Get("object").String() == "response"
	}
	if lifecycle {
		if id := response.Get("id"); id.Type == gjson.String && id.String() != "" {
			r.details.ResponseID = r.safeValue(id.String())
		}
		if model := response.Get("model"); model.Type == gjson.String && model.String() != "" {
			r.details.ResponseModel = r.safeValue(model.String())
		}
		if status := response.Get("status"); status.Type == gjson.String && status.String() != "" {
			r.details.ResponseStatus = r.safeValue(status.String())
		}
		if reason := response.Get("incomplete_details.reason").String(); reason != "" {
			r.details.IncompleteReason = r.safeValue(reason)
		}
	}
	for _, candidate := range []gjson.Result{
		event.Get("error"),
		response.Get("error"),
		response.Get("status_details.error"),
	} {
		if !candidate.IsObject() {
			continue
		}
		if typ := candidate.Get("type").String(); typ != "" {
			r.details.ErrorType = r.safeValue(typ)
		}
		if code := candidate.Get("code").String(); code != "" {
			r.details.ErrorCode = r.safeValue(code)
		}
	}
	// 流内 error 事件把 code 放在顶层:{"type":"error","code":"...","message":"..."}。
	if eventType == "error" {
		if code := event.Get("code").String(); code != "" {
			r.details.ErrorCode = r.safeValue(code)
		}
	}
	if eventType == "response.output_text.delta" && event.Get("delta").String() != "" {
		r.contentReceived()
	}
	usage := response.Get("usage")
	if !lifecycle || !usage.IsObject() {
		return
	}
	if r.details.Usage == nil {
		r.details.Usage = &codexTestUsage{}
	}
	u := r.details.Usage
	updateCodexTestCount(&u.InputTokens, usage.Get("input_tokens"))
	updateCodexTestCount(&u.OutputTokens, usage.Get("output_tokens"))
	updateCodexTestCount(&u.TotalTokens, usage.Get("total_tokens"))
	updateCodexTestCount(&u.CachedTokens, usage.Get("input_tokens_details.cached_tokens"))
	updateCodexTestCount(&u.ReasoningTokens, usage.Get("output_tokens_details.reasoning_tokens"))
}

// observeRateLimitsFrame 处理 WebSocket 传输下代替响应头出现的 codex.rate_limits 帧。
func (r *codexTestRecorder) observeRateLimitsFrame(event gjson.Result) {
	if plan := firstNonEmptyGJSON(event, "plan_type", "rate_limits.plan_type"); plan != "" {
		r.details.PlanType = r.safeValue(plan)
	}
	if window := parseCodexTestWindowFrame(event.Get("rate_limits.primary")); window != nil {
		r.details.PrimaryWindow = window
	}
	if window := parseCodexTestWindowFrame(event.Get("rate_limits.secondary")); window != nil {
		r.details.SecondaryWindow = window
	}
}

// observeMetadataFrame 把 codex.response.metadata / response.metadata 帧里的上游头
// 并入白名单头列表(WS 路径下 x-codex-* 只在这里出现)。
func (r *codexTestRecorder) observeMetadataFrame(event gjson.Result) {
	headers := event.Get("headers")
	if !headers.IsObject() {
		return
	}
	merged := make(http.Header)
	headers.ForEach(func(key, value gjson.Result) bool {
		if value.Type == gjson.String {
			merged.Add(key.String(), value.String())
		}
		return true
	})
	r.appendHeaders(merged)
	r.observeSafetyBufferingHeaders(merged)
	if r.details.PlanType == "" {
		r.details.PlanType = r.safeValue(merged.Get("x-codex-plan-type"))
	}
	if r.details.PrimaryWindow.empty() {
		r.details.PrimaryWindow = parseCodexTestWindowHeaders(merged, "x-codex-primary-")
	}
	if r.details.SecondaryWindow.empty() {
		r.details.SecondaryWindow = parseCodexTestWindowHeaders(merged, "x-codex-secondary-")
	}
}

func parseCodexTestWindowFrame(node gjson.Result) *codexTestWindow {
	if !node.IsObject() {
		return nil
	}
	window := &codexTestWindow{
		UsedPercent:   parseCodexTestNumber(node.Get("used_percent")),
		WindowMinutes: parseCodexTestNumber(node.Get("window_minutes")),
	}
	if reset := parseCodexTestNumber(node.Get("resets_in_seconds"), node.Get("reset_after_seconds")); reset != nil {
		window.ResetAfterSeconds = reset
	} else if at := node.Get("resets_at"); at.Type == gjson.Number {
		remaining := float64(time.Until(time.Unix(int64(at.Float()), 0)) / time.Second)
		if remaining >= 0 {
			window.ResetAfterSeconds = &remaining
		}
	}
	if window.empty() {
		return nil
	}
	return window
}

func parseCodexTestNumber(values ...gjson.Result) *float64 {
	for _, value := range values {
		if value.Type != gjson.Number || value.Float() < 0 {
			continue
		}
		n := value.Float()
		return &n
	}
	return nil
}

func firstNonEmptyGJSON(node gjson.Result, paths ...string) string {
	for _, path := range paths {
		if value := strings.TrimSpace(node.Get(path).String()); value != "" {
			return value
		}
	}
	return ""
}

func updateCodexTestCount(target **int64, value gjson.Result) {
	if value.Type != gjson.Number {
		return
	}
	n, err := strconv.ParseInt(value.Raw, 10, 64)
	if err == nil && n >= 0 {
		*target = &n
	}
}

func (r *codexTestRecorder) finish() *codexTestDiagnostics {
	ms := max(int64(0), time.Since(r.start).Milliseconds())
	r.details.DurationMS = &ms
	body := sanitizeCodexTestText(r.capture.String(), r.secrets)
	var pretty bytes.Buffer
	if json.Indent(&pretty, []byte(body), "", "  ") == nil {
		body = pretty.String()
	}
	r.details.BodyTruncated = r.capture.truncated || len(body) > codexTestBodyLimit
	if len(body) > codexTestBodyLimit {
		body = strings.ToValidUTF8(body[:codexTestBodyLimit], "")
	}
	r.details.ResponseBody = body
	return r.details
}
