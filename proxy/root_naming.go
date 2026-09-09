package proxy

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/cache"
	"github.com/gin-gonic/gin"
)

func (handler *Handler) claimRequestRootNaming(request *gin.Context, body []byte) *api.APIError {
	identity := handler.resolveRequestRootSessionIdentityForContext(request, body)
	if strings.EqualFold(strings.TrimSpace(identity.requestKind), "compaction") || (request.Request != nil && request.Request.URL != nil && isCompactUsageEndpoint(request.Request.URL.Path)) {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(identity.threadSource)) {
	case "thread_title", "thread_title_reconsideration", "thread_description":
	default:
		return nil
	}
	diagnostic := usageRequestDiagnosticState(request)
	if claimed, _ := request.Get("root_naming_claimed"); claimed == diagnostic {
		return nil
	}
	if !identity.stable || identity.conflict || identity.sessionID == "" {
		return api.NewAPIError(api.ErrCodeBackgroundRootUnavailable, "命名请求没有有效主根，已停止请求。", api.ErrorTypeInvalidRequest)
	}
	subject := fmt.Sprintf("api-key:%d", requestAPIKeyID(request))
	status, policy := handler.cachedNewAPIPolicyAuditState(request)
	if (status == "verified" || status == "signed_response") && policy.MetaVerified && policy.Identity.UserID != "" {
		subject = cache.PromptSessionLimitSubject(policy.Platform, policy.Identity.UserID)
	}
	if handler.db == nil {
		return api.NewAPIError(api.ErrCodeServiceUnavailable, "命名状态暂时无法确认，请稍后重试。", api.ErrorTypeServer)
	}
	ctx, cancel := context.WithTimeout(request.Request.Context(), time.Second)
	defer cancel()
	claimed, err := handler.db.ClaimRootNaming(ctx, subject, hashRiskIdentity(identity.sessionID))
	if err != nil {
		diagnostic.Naming = "unavailable"
		return api.NewAPIError(api.ErrCodeServiceUnavailable, "命名状态暂时无法确认，请稍后重试。", api.ErrorTypeServer)
	}
	if !claimed {
		diagnostic.Naming = "duplicate"
		return api.NewAPIError(api.ErrorCode("codex_root_already_named"), "该主会话已触发过命名，不允许重复命名。未请求其他账号。", api.ErrorTypeInvalidRequest)
	}
	diagnostic.Naming = "claimed"
	request.Set("root_naming_claimed", diagnostic)
	return nil
}
