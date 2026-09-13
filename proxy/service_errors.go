package proxy

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const serviceErrorContextKey = "service_error_audit"

type serviceErrorAudit struct {
	started         time.Time
	recorded        atomic.Bool
	sessionRecorded atomic.Bool
	usageMu         sync.Mutex
	usageStatus     int
	usageError      string
	authenticated   bool
	websocket       bool
	apiKeyID        int64
	apiKeyName      string
}

type serviceErrorResponseWriter struct {
	gin.ResponseWriter
	mu   sync.Mutex
	body []byte
}

func (writer *serviceErrorResponseWriter) Unwrap() http.ResponseWriter {
	return writer.ResponseWriter
}

func (writer *serviceErrorResponseWriter) capture(payload []byte) {
	if writer.Status() < 400 {
		return
	}
	writer.mu.Lock()
	defer writer.mu.Unlock()
	remaining := 8192 - len(writer.body)
	if remaining > 0 {
		writer.body = append(writer.body, payload[:min(remaining, len(payload))]...)
	}
}

func (writer *serviceErrorResponseWriter) Write(payload []byte) (int, error) {
	written, err := writer.ResponseWriter.Write(payload)
	writer.capture(payload[:written])
	return written, err
}

func (writer *serviceErrorResponseWriter) WriteString(payload string) (int, error) {
	written, err := writer.ResponseWriter.WriteString(payload)
	if writer.Status() >= 400 {
		writer.capture([]byte(payload[:min(written, 8192)]))
	}
	return written, err
}

func serviceErrorAuditForRequest(ctx *gin.Context) *serviceErrorAudit {
	if ctx == nil {
		return nil
	}
	value, _ := ctx.Get(serviceErrorContextKey)
	state, _ := value.(*serviceErrorAudit)
	return state
}

func resetServiceErrorFrame(ctx *gin.Context) {
	if previous := serviceErrorAuditForRequest(ctx); previous != nil {
		ctx.Set(serviceErrorContextKey, &serviceErrorAudit{
			started: time.Now(), authenticated: previous.authenticated, websocket: true,
			apiKeyID: previous.apiKeyID, apiKeyName: previous.apiKeyName,
		})
	}
	ctx.Set("x-model", "")
	ctx.Set(usageRequestDiagnosticsContextKey, nil)
	ctx.Set(sessionOperationsContextKey, nil)
}

func (handler *Handler) beginServiceErrorAudit(ctx *gin.Context) func() {
	if handler == nil || handler.db == nil || serviceErrorAuditForRequest(ctx) != nil {
		return func() {}
	}
	ctx.Set(serviceErrorContextKey, &serviceErrorAudit{started: time.Now()})
	api.SetErrorObserver(ctx, handler.recordObservedError)
	writer := &serviceErrorResponseWriter{ResponseWriter: ctx.Writer}
	ctx.Writer = writer
	return func() {
		state := serviceErrorAuditForRequest(ctx)
		if writer.Status() < 400 || writer.Status() > 599 || state.websocket {
			handler.finishSessionErrorAudit(ctx)
			return
		}
		writer.mu.Lock()
		body := append([]byte(nil), writer.body...)
		writer.mu.Unlock()
		message, code, errorType := "Service request rejected", fmt.Sprintf("http_%d", writer.Status()), "server_error"
		if writer.Status() < 500 {
			errorType = string(api.ErrorTypeInvalidRequest)
		}
		switch writer.Status() {
		case http.StatusUnauthorized:
			errorType = string(api.ErrorTypeAuthentication)
		case http.StatusForbidden:
			errorType = string(api.ErrorTypePermission)
		case http.StatusNotFound:
			errorType = string(api.ErrorTypeNotFound)
		case http.StatusTooManyRequests:
			errorType = string(api.ErrorTypeRateLimit)
		}
		if gjson.ValidBytes(body) {
			parsed := gjson.ParseBytes(body)
			for _, fields := range []gjson.Result{parsed, parsed.Get("error")} {
				if value := fields.Get("message"); value.Type == gjson.String && value.String() != "" {
					message = value.String()
				}
				if value := fields.Get("code"); value.Type == gjson.String && value.String() != "" {
					code = value.String()
				}
				if value := fields.Get("type"); value.Type == gjson.String && value.String() != "" {
					errorType = value.String()
				}
			}
			if failure := parsed.Get("error"); failure.Type == gjson.String && failure.String() != "" {
				message = failure.String()
			}
		}
		failure := api.NewAPIError(api.ErrorCode(code), message, api.ErrorType(errorType))
		handler.recordSessionError(ctx, writer.Status(), failure)
		if writer.Status() == http.StatusInternalServerError || code == overloadErrorCode {
			handler.finishSessionErrorAudit(ctx)
		}
		if !state.recorded.Load() && snapshotUpstreamTrace(ctx.Request.Context()).accountID == 0 {
			handler.recordServiceError(ctx, writer.Status(), failure)
		}
	}
}

func (handler *Handler) ServiceErrorMiddleware() gin.HandlerFunc {
	return func(ctx *gin.Context) {
		path := ctx.Request.URL.Path
		collect := strings.HasPrefix(path, "/v1/") || strings.HasPrefix(path, "/v1beta/") || strings.HasPrefix(path, "/backend-api/codex/")
		for _, prefix := range []string{"/responses", "/chat/completions", "/messages", "/images/", "/videos/", "/realtime", "/models", "/alpha/search", "/live"} {
			if path == strings.TrimSuffix(prefix, "/") || strings.HasPrefix(path, strings.TrimSuffix(prefix, "/")+"/") {
				collect = true
				break
			}
		}
		if !collect {
			ctx.Next()
			return
		}
		attachUpstreamTrace(ctx, handler.store)
		finish := handler.beginServiceErrorAudit(ctx)
		defer func() {
			if panicValue := recover(); panicValue != nil {
				api.ObserveError(ctx, http.StatusInternalServerError, api.NewAPIError(api.ErrCodeServerError, "Service handler panic", api.ErrorTypeServer))
				panic(panicValue)
			}
			finish()
		}()
		ctx.Next()
	}
}

func serviceErrorIsUpstream(apiError *api.APIError) bool {
	code := strings.ToLower(string(apiError.Code))
	return apiError.Type == api.ErrorTypeUpstream || strings.HasPrefix(code, "upstream_") ||
		strings.HasPrefix(code, "account_pool_") || code == "server_is_overloaded" || code == "slow_down" ||
		code == "usage_limit_reached" || code == "insufficient_quota"
}

func serviceErrorStage(state *serviceErrorAudit, status int, code string) string {
	switch {
	case status == http.StatusTooManyRequests:
		return "rate_limit"
	case !state.authenticated && (status == http.StatusUnauthorized || status == http.StatusForbidden || code == "service_unavailable"):
		return "authentication"
	case strings.Contains(code, "root") || strings.Contains(code, "naming"):
		return "root_binding"
	case strings.Contains(code, "window") || strings.Contains(code, "session") || strings.Contains(code, "sticky"):
		return "window"
	case strings.Contains(code, "policy") || strings.Contains(code, "prompt") || strings.Contains(code, "newapi"):
		return "policy"
	case code == "service_unavailable" || code == "no_available_account" || strings.HasPrefix(code, "codex_dispatch"):
		return "dispatch"
	case status < 500:
		return "validation"
	default:
		return "internal"
	}
}

func serviceErrorSafeText(ctx *gin.Context, value string, limit int) string {
	if value == "" {
		return ""
	}
	value = security.SafeTruncate(value, 8192)
	if secret := ctx.GetString("apiKey"); secret != "" {
		value = strings.ReplaceAll(value, secret, "[REDACTED]")
	}
	if ctx.Request != nil {
		if secret := strings.TrimSpace(strings.TrimPrefix(downstreamAuthorizationHeader(ctx.Request), "Bearer ")); secret != "" {
			value = strings.ReplaceAll(value, secret, "[REDACTED]")
		}
	}
	return strings.Clone(security.SafeTruncate(security.MaskSensitiveData(value), limit))
}

func (handler *Handler) recordServiceError(ctx *gin.Context, status int, apiError *api.APIError) {
	state := serviceErrorAuditForRequest(ctx)
	if state == nil || handler.db == nil || apiError == nil || status < 400 || status > 599 || serviceErrorIsUpstream(apiError) || !state.recorded.CompareAndSwap(false, true) {
		return
	}
	trace := snapshotUpstreamTrace(ctx.Request.Context())
	requestID := trace.RequestID
	if requestID == "" {
		requestID = NewUpstreamSessionUUID()
	}
	transport := "http"
	if state.websocket {
		transport = "websocket"
	} else if strings.Contains(ctx.Writer.Header().Get("Content-Type"), "text/event-stream") {
		transport = "sse"
	}
	endpoint := ctx.FullPath()
	if endpoint == "" {
		endpoint = ctx.Request.URL.Path
	}
	event := database.ServiceErrorEvent{
		ID: NewUpstreamSessionUUID(), CreatedAt: time.Now().UTC(), RequestID: requestID,
		StatusCode: status, Code: diagnosticLabel(string(apiError.Code)), ErrorType: diagnosticLabel(string(apiError.Type)),
		Message: serviceErrorSafeText(ctx, apiError.Message, 2048), Stage: serviceErrorStage(state, status, string(apiError.Code)),
		Method: ctx.Request.Method, Endpoint: serviceErrorSafeText(ctx, endpoint, 256), Transport: transport,
		Model: diagnosticLabel(ctx.GetString("x-model")), DurationMs: time.Since(state.started).Milliseconds(),
		APIKeyID: state.apiKeyID, APIKeyName: serviceErrorSafeText(ctx, state.apiKeyName, 160), RequestType: "unknown",
	}
	var incoming map[string]map[string]string
	upstream := trace.Transport
	if upstream == nil {
		upstream = &UpstreamTransportDiagnostic{Transport: "not_started", EgressKind: "unknown", PublicEgressIPStatus: "not_observed", SendPhase: "before_payload", ErrorSource: "gateway", ErrorStage: event.Stage}
	}
	if upstream.ErrorSource == "" {
		upstream.ErrorSource, upstream.ErrorStage = "gateway", event.Stage
	}
	event.UpstreamInfo = []byte(transportDiagnosticJSON(upstream))
	if value, exists := ctx.Get(usageRequestDiagnosticsContextKey); exists {
		if diagnostics, ok := value.(*usageRequestDiagnostics); ok && diagnostics != nil {
			incoming = diagnostics.Incoming
			event.AccountFailover = diagnostics.AccountFailover
			event.PromptSafety = diagnostics.PromptSafety
			if event.AccountFailover == nil && diagnostics.Continuity != nil {
				event.AccountFailover = diagnostics.Continuity.AccountFailover
			}
			event.NewAPIRequestID, event.ScopeHash = diagnostics.NewAPIRequestID, diagnostics.Recent.Scope
			event.RootAccountLookup, event.RootAccountWait, event.RootAccountWaitMs = diagnostics.RootAccountLookup, diagnostics.RootAccountWait, diagnostics.RootAccountWaitMillis
			event.BackgroundWindowWait = diagnostics.BackgroundWindowWait
			if resolved := diagnostics.Resolved; resolved != nil {
				event.ThreadSource, event.RequestKind, event.SubagentKind = resolved.ThreadSource, resolved.RequestKind, resolved.SubagentKind
				event.RootFingerprint = resolved.RootFingerprint
				switch {
				case resolved.RequestKind == "compaction":
					event.RequestType = "compaction"
				case resolved.Passive && resolved.Related:
					event.RequestType = "related_internal"
				case resolved.Passive:
					event.RequestType = "independent_internal"
				case resolved.ThreadSource == "user":
					event.RequestType = "user"
				}
			}
			for _, source := range []string{"turn_metadata_header", "client_metadata.x-codex-turn-metadata", "client_metadata", "headers"} {
				metadata := diagnostics.Incoming[source]
				if event.ThreadID == "" {
					event.ThreadID = firstNonEmptyString(metadata["thread_id"], metadata["Thread-Id"])
				}
				if event.WindowID == "" {
					event.WindowID = firstNonEmptyString(metadata["window_id"], metadata["x-codex-window-id"], metadata["X-Codex-Window-Id"])
				}
			}
		}
	}
	if incoming == nil {
		incoming = map[string]map[string]string{"headers": captureUsageDiagnosticHeaders(ctx)}
		if metadata := ctx.GetHeader(codexTurnMetadataHeader); metadata != "" && len(metadata) <= 16384 {
			incoming["turn_metadata_header"] = diagnosticMetadata(gjson.Parse(metadata))
		}
	}
	event.ClientInfo = usageDiagnosticClientInfo(incoming)
	if endpoint == "/v1/session-windows" {
		if operation := ctx.GetString(windowControlOperationContextKey); operation != "" {
			if event.ClientInfo == nil {
				event.ClientInfo = make(map[string]string)
			}
			event.ClientInfo["window_control.operation"] = operation
		}
	}
	value, _ := ctx.Get(newAPIPolicyMetaContextKey)
	frameDiagnostics, _ := ctx.Get(usageRequestDiagnosticsContextKey)
	if policy, valid := value.(verifiedNewAPIPolicyContext); valid && policy.MetaVerified && (!state.websocket || frameDiagnostics != nil) {
		event.NewAPIRequestID = diagnosticRequestID(policy.Identity.RequestID)
		event.NewAPIIdentityVerified = true
		event.NewAPIUserID = serviceErrorSafeText(ctx, policy.Identity.UserID, 160)
		event.NewAPIUserName = serviceErrorSafeText(ctx, policy.Meta.UserName, 160)
		if policy.Meta.InstallationID != "" {
			event.ClientInfo["signed_newapi.installation_id"] = diagnosticIdentifier(policy.Meta.InstallationID)
		}
	} else if event.NewAPIRequestID == "" {
		event.NewAPIRequestID = diagnosticRequestID(ctx.GetHeader("X-NewAPI-Request-ID"))
	}
	if event.ThreadID == "" {
		event.ThreadID = diagnosticIdentifier(ctx.GetHeader("Thread-Id"))
	}
	if event.WindowID == "" {
		event.WindowID = diagnosticIdentifier(ctx.GetHeader("X-Codex-Window-Id"))
	}
	if event.ThreadSource == "" {
		if metadata := ctx.GetHeader("X-Codex-Turn-Metadata"); len(metadata) <= 16384 {
			parsed := gjson.Parse(metadata)
			event.ThreadSource = diagnosticLabel(parsed.Get("thread_source").String())
			event.RequestKind = diagnosticLabel(parsed.Get("request_kind").String())
			event.SubagentKind = diagnosticLabel(parsed.Get("subagent_kind").String())
		}
	}
	if selection := selectionTraceForRequest(ctx); selection != nil {
		event.CandidateRejections = selection.Snapshot().Reasons
	}
	if endpoint == "/v1/session-windows" {
		event.RequestType, event.RequestKind = "gateway_internal", "window_control"
	}
	handler.db.EnqueueServiceError(event)
}
