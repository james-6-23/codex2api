package proxy

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/codex2api/api"
	"github.com/gin-gonic/gin"
)

const backgroundRootAccountWaitTimeout = 60 * time.Second

func requiresBackgroundRootAccount(source string) bool {
	source = strings.TrimSpace(source)
	return source != "" && !strings.EqualFold(source, "user")
}

func (handler *Handler) waitForBackgroundRootAccount(requestContext *gin.Context, identity requestSessionIdentity) *api.APIError {
	if !identity.requiresRootAccount {
		return nil
	}
	if !identity.relatedToRoot || !identity.stableIdentity || identity.unlinkedFallbackOnly || strings.TrimSpace(identity.affinityID) == "" {
		state := usageRequestDiagnosticState(requestContext)
		state.RootAccountWait = "unresolved"
		return api.NewAPIError(api.ErrCodeBackgroundRootUnavailable, "后台请求尚未关联有效主会话，请先发起主请求。请求已停止，未选择其他账号。", api.ErrorTypeInvalidRequest)
	}
	state := usageRequestDiagnosticState(requestContext)
	deadline := state.StartedAt.Add(backgroundRootAccountWaitTimeout)
	status, policy := handler.cachedNewAPIPolicyAuditState(requestContext)
	if (status == "verified" || status == "signed_response") && policy.MetaVerified && policy.Meta.RootAccountWaitMillis != nil {
		remaining := min(max(*policy.Meta.RootAccountWaitMillis, 0), backgroundRootAccountWaitTimeout.Milliseconds())
		if sharedDeadline := state.StartedAt.Add(time.Duration(remaining) * time.Millisecond); sharedDeadline.Before(deadline) {
			deadline = sharedDeadline
		}
	}
	waitContext, cancelWait := context.WithDeadline(requestContext.Request.Context(), deadline)
	defer cancelWait()
	started := time.Now()
	accountID, waitErr := handler.store.WaitForRootAccount(waitContext, sessionAffinityKey(identity.affinityID, requestAPIKeyID(requestContext)))
	state.RootAccountWaitMillis = time.Since(started).Milliseconds()
	if waitErr == nil {
		state.RootAccountWait = "found"
		recordUsageRootAccount(requestContext, accountID, true)
		return handler.waitForBackgroundWindowGrant(waitContext, requestContext)
	}
	state.RootAccountWait = "timeout"
	if requestContext.Request.Context().Err() != nil {
		state.RootAccountWait = "canceled"
		return api.NewAPIError(api.ErrCodeInvalidRequest, "Background request was canceled while waiting for its main conversation.", api.ErrorTypeInvalidRequest)
	}
	if !errors.Is(waitErr, context.DeadlineExceeded) {
		state.RootAccountWait = "unavailable"
		return api.NewAPIError(api.ErrCodeBackgroundRootUnavailable, "主会话绑定暂不可用或等待请求过多，请稍后再试。未选择其他账号。", api.ErrorTypeInvalidRequest)
	}
	return api.NewAPIError(api.ErrCodeRootAccountWaitTimeout, "Main conversation account was not bound within the 60-second wait limit. Background request stopped; no other account was selected.", api.ErrorTypeInvalidRequest)
}
