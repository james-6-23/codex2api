package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const upstreamPromptSafetyReason = "upstream_invalid_prompt_policy"
const promptSafetyLockedReason = "conversation_safety_locked"
const upstreamPromptSafetyContextKey = "upstream_prompt_safety"
const upstreamPromptSafetyMessage = "上游安全检查拒绝了本次提示，已停止自动重试和换号重放。这不是账号已被封禁的通知，也不计入本地 CYB 处罚。"

func isUpstreamPromptSafetyRefusal(payload []byte) bool {
	if isExplicitUpstreamCyberPolicy(payload) {
		return false
	}
	body := gjson.ParseBytes(responseFailedErrorBody(payload))
	for _, value := range []gjson.Result{body.Get("error"), body} {
		if !value.IsObject() || !strings.EqualFold(strings.TrimSpace(value.Get("code").String()), "invalid_prompt") {
			continue
		}
		message := strings.ToLower(strings.Join(strings.Fields(value.Get("message").String()), " "))
		message = strings.TrimPrefix(message, "invalid prompt: ")
		if strings.HasPrefix(message, "your prompt was flagged as potentially violating our usage policy") {
			return true
		}
	}
	return false
}

func isHardStopUpstreamPolicy(payload []byte) bool {
	return isExplicitUpstreamCyberPolicy(payload) || isUpstreamPromptSafetyRefusal(payload)
}

func isHardStopUpstreamPolicyError(err error) bool {
	for depth := 0; err != nil && depth < 16; depth++ {
		if upstream, ok := err.(continuousRetryHTTPError); ok && isHardStopUpstreamPolicy(upstream.UpstreamErrorBody()) {
			return true
		}
		err = errors.Unwrap(err)
	}
	return false
}

func upstreamPromptSafetyRequestErrorBody(err error) []byte {
	for depth := 0; err != nil && depth < 16; depth++ {
		if upstream, ok := err.(continuousRetryHTTPError); ok && isUpstreamPromptSafetyRefusal(upstream.UpstreamErrorBody()) {
			return upstream.UpstreamErrorBody()
		}
		err = errors.Unwrap(err)
	}
	return nil
}

func (handler *Handler) rejectUpstreamPromptSafetyRequestError(request *gin.Context, err error) bool {
	body := upstreamPromptSafetyRequestErrorBody(err)
	if len(body) == 0 {
		return false
	}
	handler.recordUpstreamPromptSafety(request, request.Request.URL.Path, request.GetString("x-model"), body)
	if requestUsesAnthropicErrorEnvelope(request) {
		if writeCommittedAnthropicRetryError(request, "invalid_request_error", upstreamPromptSafetyMessage) {
			return true
		}
	} else if writeCommittedResponsesRetryError(request, upstreamPromptSafetyMessage) {
		return true
	}
	return writeUpstreamPromptSafetyError(request, body)
}

func promptSafetyDiagnostic(request *gin.Context) *database.PromptSafetyDiagnostic {
	if request == nil {
		return nil
	}
	value, _ := request.Get(upstreamPromptSafetyContextKey)
	diagnostic, _ := value.(*database.PromptSafetyDiagnostic)
	return diagnostic
}

func (handler *Handler) recordUpstreamPromptSafety(request *gin.Context, endpoint, model string, payload []byte) {
	if handler == nil || request == nil || !isUpstreamPromptSafetyRefusal(payload) || promptSafetyDiagnostic(request) != nil {
		return
	}
	diagnostic := &database.PromptSafetyDiagnostic{Reason: upstreamPromptSafetyReason, UpstreamCode: "invalid_prompt", LockResult: "disabled", Retry: "stop"}
	request.Set(upstreamPromptSafetyContextKey, diagnostic)
	usageRequestDiagnosticState(request).PromptSafety = diagnostic
	if !request.Writer.Written() {
		request.Header("X-Should-Retry", "false")
	}
	config := handler.promptFilterConfigForRequest(request)
	if config.Advanced.Enforcement.ConversationLockEnabled {
		diagnostic.LockResult = "storage_unavailable"
		if handler.db != nil {
			handler.lockUpstreamPromptSafety(request, config, endpoint, model, diagnostic)
		}
	}
	decision := promptfilter.Decision{Action: promptfilter.ActionBlock, ReasonCode: upstreamPromptSafetyReason, Terminal: true}
	verdict := promptfilter.Verdict{Enabled: true, Action: promptfilter.ActionBlock, Reason: upstreamPromptSafetyMessage, FullText: upstreamPromptSafetyMessage}
	handler.logPromptFilterVerdictWithDecision(request, endpoint, model, upstreamPromptSafetyReason, "invalid_prompt", verdict, &decision, nil)
}

func (handler *Handler) lockUpstreamPromptSafety(request *gin.Context, config promptfilter.Config, endpoint, model string, diagnostic *database.PromptSafetyDiagnostic) {
	body := ingressRequestBody(request, nil)
	if isResponsesWebSocketUpgradeRequest(request.Request) {
		body, _ = rawRequestBodyFromContext(request)
	}
	policy, verified := handler.verifyNewAPIPolicyContext(request, config.Advanced.NewAPI, ingressRequestBody(request, nil))
	identity, known := verifiedPromptConversationLockIdentity(request, policy)
	root := handler.resolveRequestRootSessionIdentityForContext(request, body)
	if !verified {
		identity, known = promptConversationLockFallbackIdentity(request)
		if root.stable && !root.conflict {
			identity, known = promptConversationLockFallbackIdentityForSession(requestAPIKeyID(request), root.sessionID)
		}
	}
	if !known || !root.stable || root.conflict {
		diagnostic.LockResult = "identity_unavailable"
		return
	}
	digest := sha256.Sum256([]byte("prompt-safety-lock-v1\x00" + identity.LockKey))
	identity.LockKey = hex.EncodeToString(digest[:])
	if _, failure := handler.promptLockLineageKeys(request, body, policy, verified, "cyber", identity.LockKey); failure != nil {
		diagnostic.LockResult = "lineage_unavailable"
		return
	}
	ctx, cancel := context.WithTimeout(request.Request.Context(), 3*time.Second)
	defer cancel()
	correlationID := ensurePromptPolicyRequestCorrelationID(request)
	item, _, err := handler.db.LockPromptConversation(ctx, database.PromptConversationLockInput{
		LockKey: identity.LockKey, IdentityKind: identity.Kind, Platform: identity.Platform, NewAPIUserID: identity.NewAPIUserID,
		SessionFingerprint: identity.SessionFingerprint, SessionHash: identity.SessionHash,
		DecisionID: "upstream-safety:" + correlationID, RequestID: correlationID, ReasonCode: upstreamPromptSafetyReason,
		Endpoint: endpoint, Model: model, LockedAt: time.Now().UTC(),
	})
	if err != nil {
		return
	}
	diagnostic.LockResult = "locked"
	if item == nil || item.Status != database.PromptConversationLockStatusActive {
		diagnostic.LockResult = "not_active"
		return
	}
	diagnostic.Locked = true
	expires := item.LockedAt.Add(promptConversationLockTTL(config))
	diagnostic.ExpiresAt = &expires
}

func upstreamPromptSafetyAPIError(request *gin.Context, payload []byte) *api.APIError {
	details := gin.H{"reason_code": upstreamPromptSafetyReason, "upstream_code": "invalid_prompt", "retry": "stop", "strike_eligible": false}
	message := upstreamPromptSafetyMessage
	if diagnostic := promptSafetyDiagnostic(request); diagnostic != nil {
		details["lock_result"], details["conversation_locked"] = diagnostic.LockResult, diagnostic.Locked
		if diagnostic.Locked {
			message += "当前会话已被网关安全锁定，派生对话同样受限；请联系管理员审核解锁，或等待锁定到期。"
			details["manual_unlock_path"] = "/admin/prompt-filter/profiles"
		}
		if diagnostic.ExpiresAt != nil {
			details["expires_at"] = diagnostic.ExpiresAt.Format(time.RFC3339)
		}
	}
	return api.NewAPIErrorWithDetails("invalid_prompt", message, api.ErrorTypeInvalidRequest, details)
}

func attachUpstreamPromptSafetyDetails(request *gin.Context, payload []byte) []byte {
	if !isUpstreamPromptSafetyRefusal(payload) {
		return payload
	}
	path := "error"
	if gjson.GetBytes(payload, "response.error").IsObject() {
		path = "response.error"
	} else if gjson.GetBytes(payload, "response.status_details.error").IsObject() {
		path = "response.status_details.error"
	} else if !gjson.GetBytes(payload, path).IsObject() {
		path = ""
	}
	if path != "" {
		path += "."
	}
	apiError := upstreamPromptSafetyAPIError(request, payload)
	updated, err := sjson.SetBytes(payload, path+"details.codex2api_safety", apiError.Details)
	if err != nil {
		return payload
	}
	updated, err = sjson.SetBytes(updated, path+"details.codex2api_safety.message", apiError.Message)
	if err != nil {
		return payload
	}
	return updated
}

func writeUpstreamPromptSafetyError(request *gin.Context, payload []byte) bool {
	if !isUpstreamPromptSafetyRefusal(payload) {
		return false
	}
	request.Header("X-Should-Retry", "false")
	apiError := upstreamPromptSafetyAPIError(request, payload)
	if requestUsesAnthropicErrorEnvelope(request) {
		request.JSON(http.StatusBadRequest, gin.H{"type": "error", "error": gin.H{"type": string(apiError.Type), "code": apiError.Code, "message": apiError.Message, "details": apiError.Details}})
	} else {
		api.SendErrorWithStatus(request, apiError, http.StatusBadRequest)
	}
	return true
}
