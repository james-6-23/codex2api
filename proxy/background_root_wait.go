package proxy

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

const backgroundRootAccountWaitTimeout = 60 * time.Second

func requiresBackgroundRootAccount(source string) bool {
	source = strings.TrimSpace(source)
	return source != "" && !strings.EqualFold(source, "user")
}

func (handler *Handler) waitForBackgroundRootAccount(requestContext *gin.Context, identity requestSessionIdentity) *api.APIError {
	if apiRelaySessionExempt(requestContext) {
		usageRequestDiagnosticState(requestContext).RootAccountWait = "api_relay_exempt"
		return nil
	}
	if blocked := handler.sessionBlacklistError(requestContext); blocked != nil {
		return blocked
	}
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
	defer func() { state.RootAccountWaitMillis = time.Since(started).Milliseconds() }()
	key := sessionAffinityKey(identity.affinityID, requestAPIKeyID(requestContext))
	if handler.db != nil {
		entry, found, err := handler.readSessionContinuity(requestContext.Request.Context(), hashRiskIdentity(key))
		if err != nil {
			return sessionContinuityError("ownership_unavailable")
		}
		if found {
			state.RootAccountWait = "found"
			if entry.Record.FailoverCount > 0 {
				state.RootAccountWait = "persistent_failover_owner"
			}
			recordUsageRootAccount(requestContext, entry.Record.AccountID, true)
			return handler.waitForBackgroundActiveWindow(waitContext, requestContext, key, entry.Record.AccountID, &entry.Record)
		}
	}
	accountID, waitErr := handler.store.WaitForRootAccount(waitContext, key)
	if waitErr == nil {
		state.RootAccountWait = "found"
		recordUsageRootAccount(requestContext, accountID, true)
		return handler.waitForBackgroundActiveWindow(waitContext, requestContext, key, accountID, nil)
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
	return api.NewAPIError(api.ErrCodeRootAccountWaitTimeout, "Main conversation account was not bound within the 60-second wait limit. Background request stopped.", api.ErrorTypeInvalidRequest)
}

func (handler *Handler) waitForBackgroundActiveWindow(ctx context.Context, request *gin.Context, rootKey string, accountID int64, expected *database.SessionContinuityRecord) *api.APIError {
	diagnostic := &database.BackgroundWindowWaitDiagnostic{Result: "waiting", AccountID: accountID}
	if expected != nil {
		diagnostic.Generation = expected.FailoverCount
	}
	usageRequestDiagnosticState(request).BackgroundWindowWait = diagnostic
	started := time.Now()
	defer func() { diagnostic.DurationMs = time.Since(started).Milliseconds() }()
	if ctx.Err() != nil {
		if request.Request.Context().Err() != nil {
			diagnostic.Result = "canceled"
			return api.NewAPIError(api.ErrCodeInvalidRequest, "Background request was canceled while waiting for its main conversation window.", api.ErrorTypeInvalidRequest)
		}
		diagnostic.Result = "timeout"
		return api.NewAPIError(api.ErrCodeRootAccountWaitTimeout, "主会话账号已找到，但有效窗口未在共享等待预算内恢复，后台请求已停止。", api.ErrorTypeInvalidRequest)
	}
	entry, persisted, err := handler.readSessionContinuity(ctx, hashRiskIdentity(rootKey))
	if err != nil {
		diagnostic.Result = "ownership_unavailable"
		return sessionContinuityError("ownership_unavailable")
	}
	if expected != nil && (!persisted || entry.Record.AccountID != expected.AccountID || entry.Record.FailoverCount != expected.FailoverCount) {
		diagnostic.Result = "owner_changed"
		return api.NewAPIError(api.ErrCodeBackgroundRootUnavailable, "等待期间主会话账号或换号代次已变化，后台请求已停止，请重新发起请求。", api.ErrorTypeInvalidRequest)
	}
	body := ingressRequestBody(request, nil)
	if persisted {
		accountID = entry.Record.AccountID
		diagnostic.AccountID, diagnostic.Generation = accountID, entry.Record.FailoverCount
		if entry.Record.FailoverCount > 0 {
			if failure := handler.validateMigratedSessionContext(request, body, entry.Record, rootKey); failure != nil {
				diagnostic.Result = "context_unavailable"
				return failure
			}
			if live, active := handler.store.LiveSessionAccountID(rootKey, time.Now()); active && live != accountID {
				handler.store.UnbindSessionAffinity(rootKey, live)
			}
		}
	}
	if handler.store.FindByID(accountID) == nil {
		diagnostic.Result = "account_unavailable"
		return api.NewAPIError(api.ErrCodeBackgroundRootUnavailable, "主会话绑定账号不存在，后台请求已停止，未选择其他账号。", api.ErrorTypeInvalidRequest)
	}
	recordUsageRootAccount(request, accountID, true)
	if failure := handler.waitForBackgroundWindowGrant(ctx, request); failure != nil {
		diagnostic.Result = "grant_unavailable"
		return failure
	}
	if err := handler.store.WaitForRootAccountWindow(ctx, rootKey, accountID); err != nil {
		switch {
		case request.Request.Context().Err() != nil:
			diagnostic.Result = "canceled"
			return api.NewAPIError(api.ErrCodeInvalidRequest, "Background request was canceled while waiting for its main conversation window.", api.ErrorTypeInvalidRequest)
		case errors.Is(err, context.DeadlineExceeded):
			diagnostic.Result = "timeout"
			return api.NewAPIError(api.ErrCodeRootAccountWaitTimeout, "主会话账号已找到，但有效窗口未在共享等待预算内恢复，后台请求已停止。", api.ErrorTypeInvalidRequest)
		case errors.Is(err, auth.ErrRootAccountOwnerChanged):
			diagnostic.Result = "owner_changed"
		default:
			diagnostic.Result = "unavailable"
		}
		return api.NewAPIError(api.ErrCodeBackgroundRootUnavailable, "等待期间主会话账号归属已变化或窗口暂不可用，后台请求已停止，未选择其他账号。", api.ErrorTypeInvalidRequest)
	}
	if failure := handler.prepareBackgroundAccountMatch(request, rootKey, body); failure != nil {
		diagnostic.Result = "validation_failed"
		return failure
	}
	match := backgroundAccountMatchFromContext(request.Request.Context())
	if match.accountID != accountID || match.generation != diagnostic.Generation || persisted && !match.persisted {
		diagnostic.Result = "owner_changed"
		match.diagnostic.Result = "owner_changed"
		return api.NewAPIError(api.ErrCodeBackgroundRootUnavailable, "等待期间主会话账号或换号代次已变化，后台请求已停止，请重新发起请求。", api.ErrorTypeInvalidRequest)
	}
	diagnostic.Result = "ready"
	return nil
}
