package admin

import (
	"context"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// 网关自身发起的账号探测请求也要进用量记录,并带上可区分的内部原因,让用量页
// 能把它们和下游流量分开显示:测连(connection_test)与降智检测(quality_test)。
// 记录写在探针收尾处,与诊断帧共用同一份观测值(耗时、终态 usage、上游标识)。
const (
	internalReasonConnectionTest = "connection_test"
	internalReasonQualityTest    = "quality_test"

	// 与网关流内断流记账保持同一个内部状态码:请求还没拿到上游响应就失败。
	connectionTestTransportStatus = 598

	contextConnectionTestLastError = "connection_test_last_error"
)

// connectionTestUsageInput 是探针收尾时从诊断对象抽出的记账字段。
type connectionTestUsageInput struct {
	Reason            string
	Endpoint          string
	Model             string
	EffectiveModel    string
	ReasoningEffort   string
	StatusCode        int
	DurationMs        int
	FirstTokenMs      int
	InputTokens       int
	OutputTokens      int
	CachedTokens      int
	ReasoningTokens   int
	CacheWrite5m      int
	CacheWrite1h      int
	ViaWebsocket      bool
	UpstreamRequestID string
	ErrorMessage      string
	UpstreamErrorKind string
}

func connectionTestReason(quality *qualityTestRequest) string {
	if quality != nil {
		return internalReasonQualityTest
	}
	return internalReasonConnectionTest
}

// connectionTestReasoningEffort 从探针请求体里取推理档位:Responses 形状在
// reasoning.effort,Claude Messages 形状在 output_config.effort。
func connectionTestReasoningEffort(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	if effort := strings.TrimSpace(gjson.GetBytes(payload, "reasoning.effort").String()); effort != "" {
		return effort
	}
	return strings.TrimSpace(gjson.GetBytes(payload, "output_config.effort").String())
}

func connectionTestEndpoint(account *auth.Account) string {
	if account != nil && account.IsClaudeOAuth() {
		return "/v1/messages"
	}
	return "/v1/responses"
}

func usageChannelForAccount(account *auth.Account) string {
	switch {
	case account == nil:
		return database.UpstreamChannelCodex
	case account.IsGrokAPI():
		return database.UpstreamChannelGrok
	case account.IsAntigravityAPI():
		return database.UpstreamChannelAntigravity
	case account.IsClaudeOAuth():
		return database.UpstreamChannelClaude
	default:
		return database.UpstreamChannelCodex
	}
}

// rememberConnectionTestError 记下最后一条推给客户端的错误文案,收尾记账时作为
// error_message 落库,让用量页的"仅错误"筛选也能看到失败的探针。
func rememberConnectionTestError(c *gin.Context, event testEvent) {
	if c == nil || event.Type != "error" {
		return
	}
	if msg := strings.TrimSpace(event.Error); msg != "" {
		c.Set(contextConnectionTestLastError, msg)
	}
}

func connectionTestLastError(c *gin.Context) string {
	if c == nil {
		return ""
	}
	if value, exists := c.Get(contextConnectionTestLastError); exists {
		msg, _ := value.(string)
		return strings.TrimSpace(msg)
	}
	return ""
}

func int64PtrValue(v *int64) int {
	if v == nil {
		return 0
	}
	return int(*v)
}

// connectionTestUsageFromCodex 把 Codex/Responses 探针诊断(Codex、Grok、Antigravity
// 共用)投影成记账字段。诊断里的 usage 是终态全量值,直接采用。
func connectionTestUsageFromCodex(d *codexTestDiagnostics, model string) connectionTestUsageInput {
	in := connectionTestUsageInput{Model: model, EffectiveModel: model}
	if d == nil {
		return in
	}
	in.StatusCode = d.HTTPStatus
	in.DurationMs = int64PtrValue(d.DurationMS)
	in.FirstTokenMs = int64PtrValue(d.FirstContentMS)
	if in.FirstTokenMs == 0 {
		in.FirstTokenMs = int64PtrValue(d.FirstFrameMS)
	}
	if d.ResponseModel != "" {
		in.EffectiveModel = d.ResponseModel
	}
	in.ViaWebsocket = d.Transport == "websocket"
	in.UpstreamRequestID = d.RequestID
	if d.Usage != nil {
		in.InputTokens = int64PtrValue(d.Usage.InputTokens)
		in.OutputTokens = int64PtrValue(d.Usage.OutputTokens)
		in.CachedTokens = int64PtrValue(d.Usage.CachedTokens)
		in.ReasoningTokens = int64PtrValue(d.Usage.ReasoningTokens)
	}
	return in
}

// connectionTestUsageFromClaude 把 Claude Messages 探针诊断投影成记账字段。
// Anthropic 的 input_tokens 不含缓存部分,这里对齐网关记账口径:input = 未缓存 +
// 缓存命中 + 缓存写入;只给了缓存写入总数时按 5 分钟缓存计。
func connectionTestUsageFromClaude(d *claudeTestDiagnostics, model string) connectionTestUsageInput {
	in := connectionTestUsageInput{Model: model, EffectiveModel: model}
	if d == nil {
		return in
	}
	in.StatusCode = d.HTTPStatus
	in.DurationMs = int64PtrValue(d.DurationMS)
	in.FirstTokenMs = int64PtrValue(d.FirstContentMS)
	if d.ResponseModel != "" {
		in.EffectiveModel = d.ResponseModel
	}
	in.UpstreamRequestID = d.RequestID
	if d.Usage == nil {
		return in
	}
	uncached := int64PtrValue(d.Usage.InputTokens)
	in.OutputTokens = int64PtrValue(d.Usage.OutputTokens)
	in.CachedTokens = int64PtrValue(d.Usage.CacheReadTokens)
	if d.Usage.CacheCreation != nil {
		in.CacheWrite5m = int64PtrValue(d.Usage.CacheCreation.FiveMinute)
		in.CacheWrite1h = int64PtrValue(d.Usage.CacheCreation.OneHour)
	}
	if in.CacheWrite5m+in.CacheWrite1h == 0 {
		in.CacheWrite5m = int64PtrValue(d.Usage.CacheCreationTokens)
	}
	in.InputTokens = uncached + in.CachedTokens + in.CacheWrite5m + in.CacheWrite1h
	return in
}

// logConnectionTestUsage 把一次探针写进 usage_logs。不经过下游 API Key 记账链路:
// 探针没有下游身份,只按账号/渠道归属,成本由写入层按模型定价计算。
func (h *Handler) logConnectionTestUsage(c *gin.Context, account *auth.Account, in connectionTestUsageInput) {
	if h == nil || h.db == nil || account == nil {
		return
	}
	if in.ErrorMessage == "" {
		in.ErrorMessage = connectionTestLastError(c)
	}
	if in.ErrorMessage != "" && in.UpstreamErrorKind == "" && in.StatusCode >= 200 && in.StatusCode < 300 {
		// HTTP 200 但流内失败(response.failed / error 事件 / 无输出),按流内错误归类。
		in.UpstreamErrorKind = "stream_error"
	}
	if in.StatusCode == 0 {
		in.StatusCode = connectionTestTransportStatus
	}
	if in.DurationMs < 0 {
		in.DurationMs = 0
	}
	input := &database.UsageLogInput{
		AccountID:            account.ID(),
		CredentialGeneration: account.GetCredentialGeneration(),
		Channel:              usageChannelForAccount(account),
		InternalReason:       in.Reason,
		Endpoint:             in.Endpoint,
		InboundEndpoint:      in.Endpoint,
		UpstreamEndpoint:     in.Endpoint,
		UpstreamRequestID:    in.UpstreamRequestID,
		Model:                in.Model,
		EffectiveModel:       in.EffectiveModel,
		ReasoningEffort:      in.ReasoningEffort,
		StatusCode:           in.StatusCode,
		DurationMs:           in.DurationMs,
		FirstTokenMs:         in.FirstTokenMs,
		InputTokens:          in.InputTokens,
		OutputTokens:         in.OutputTokens,
		PromptTokens:         in.InputTokens,
		CompletionTokens:     in.OutputTokens,
		TotalTokens:          in.InputTokens + in.OutputTokens,
		CachedTokens:         in.CachedTokens,
		ReasoningTokens:      in.ReasoningTokens,
		CacheWrite5mTokens:   in.CacheWrite5m,
		CacheWrite1hTokens:   in.CacheWrite1h,
		Stream:               true,
		ViaWebsocket:         in.ViaWebsocket,
		ErrorMessage:         in.ErrorMessage,
		UpstreamErrorKind:    in.UpstreamErrorKind,
	}
	if c != nil {
		input.ClientIP = strings.TrimSpace(c.ClientIP())
		if c.Request != nil {
			input.ClientUserAgent = strings.TrimSpace(c.Request.UserAgent())
		}
	}
	_ = h.db.InsertUsageLog(context.Background(), input)
}

// logConnectionTestTransportFailure 记录连上游前就失败的探针(拨号/代理/TLS 等)。
func (h *Handler) logConnectionTestTransportFailure(c *gin.Context, account *auth.Account, reason, endpoint, model, effort string, start time.Time, err error) {
	msg := ""
	if err != nil {
		msg = strings.TrimSpace(err.Error())
	}
	h.logConnectionTestUsage(c, account, connectionTestUsageInput{
		Reason:            reason,
		Endpoint:          endpoint,
		Model:             model,
		EffectiveModel:    model,
		ReasoningEffort:   effort,
		StatusCode:        connectionTestTransportStatus,
		DurationMs:        int(max(int64(0), time.Since(start).Milliseconds())),
		ErrorMessage:      msg,
		UpstreamErrorKind: "transport",
	})
}
