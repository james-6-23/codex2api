package proxy

import (
	"context"
	"net/http"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

type backgroundAccountMatchContextKey struct{}

type backgroundAccountMatchDiagnostic struct {
	Result          string `json:"result"`
	ScopeHash       string `json:"scope_hash,omitempty"`
	SessionIDPrefix string `json:"session_id_prefix,omitempty"`
	AccountID       int64  `json:"account_id"`
	Generation      uint64 `json:"generation"`
	OwnerSource     string `json:"owner_source"`
}

type backgroundAccountMatch struct {
	handler    *Handler
	rootKey    string
	accountID  int64
	generation uint64
	persisted  bool
	diagnostic *backgroundAccountMatchDiagnostic
}

func backgroundAccountMatchFromContext(ctx context.Context) *backgroundAccountMatch {
	if ctx == nil {
		return nil
	}
	match, _ := ctx.Value(backgroundAccountMatchContextKey{}).(*backgroundAccountMatch)
	return match
}

func (handler *Handler) prepareBackgroundAccountMatch(request *gin.Context, rootKey string, body []byte) *api.APIError {
	if previous := backgroundAccountMatchFromContext(request.Request.Context()); previous != nil {
		if previous.rootKey != rootKey || ValidateBackgroundAccountMatch(request.Request.Context(), handler.store.FindByID(previous.accountID)) != nil {
			return api.NewAPIError(api.ErrCodeBackgroundRootUnavailable, "主窗口恢复后账号归属已变化或窗口失效，后台请求已停止，请重新发起请求。", api.ErrorTypeInvalidRequest)
		}
	}
	entry, persisted, err := handler.readSessionContinuity(request.Request.Context(), hashRiskIdentity(rootKey))
	if err != nil {
		return sessionContinuityError("ownership_unavailable")
	}
	live, active := handler.store.LiveSessionAccountID(rootKey, time.Now())
	accountID, generation, source := live, uint64(0), "live_root"
	if persisted {
		handler.attachSessionOutboundEpoch(request, hashRiskIdentity(rootKey), entry.Record)
		accountID, generation, source = entry.Record.AccountID, entry.Record.FailoverCount, "persistent_root"
		if generation > 0 {
			if failure := handler.validateMigratedSessionContext(request, body, entry.Record, rootKey); failure != nil {
				return failure
			}
			if active && live != accountID {
				handler.store.UnbindSessionAffinity(rootKey, live)
				live, active = handler.store.LiveSessionAccountID(rootKey, time.Now())
			}
		}
	}
	if previous := backgroundAccountMatchFromContext(request.Request.Context()); previous != nil &&
		(accountID != previous.accountID || generation != previous.generation || previous.persisted && !persisted) {
		previous.diagnostic.Result = "owner_changed"
		return api.NewAPIError(api.ErrCodeBackgroundRootUnavailable, "主窗口恢复后账号或换号代次已变化，后台请求已停止，请重新发起请求。", api.ErrorTypeInvalidRequest)
	}
	account := handler.store.FindByID(accountID)
	if account != nil && account.SessionCapacityLimits().Enabled {
		live, active = handler.store.AccountSessionAccountID(rootKey, time.Now())
	}
	diagnostic := &backgroundAccountMatchDiagnostic{
		Result: "matched", AccountID: accountID, Generation: generation, OwnerSource: source,
	}
	state := usageRequestDiagnosticState(request)
	diagnostic.SessionIDPrefix = state.SessionIDPrefix
	if diagnostic.SessionIDPrefix == "" {
		headers := request.Request.Header
		if isResponsesWebSocketUpgradeRequest(request.Request) {
			headers = nil
		}
		diagnostic.SessionIDPrefix = requestSessionIDPrefix(headers, body)
	}
	status, policy := handler.cachedNewAPIPolicyAuditState(request)
	diagnostic.ScopeHash = unlinkedFallbackScopeForRequest(request, body, policy, status == "verified" || status == "signed_response")
	state.BackgroundAccountMatch = diagnostic
	if !active || account == nil || live != accountID {
		diagnostic.Result = "root_owner_unavailable"
		return api.NewAPIError(api.ErrCodeBackgroundRootUnavailable, "后台请求未匹配到主会话当前账号的有效窗口，请先发起主请求。", api.ErrorTypeInvalidRequest)
	}
	match := &backgroundAccountMatch{handler: handler, rootKey: rootKey, accountID: accountID,
		generation: generation, persisted: persisted, diagnostic: diagnostic}
	request.Request = request.Request.WithContext(context.WithValue(request.Request.Context(), backgroundAccountMatchContextKey{}, match))
	recordUsageRootAccount(request, accountID, true)
	return nil
}

func ValidateBackgroundAccountMatch(ctx context.Context, account *auth.Account) error {
	match := backgroundAccountMatchFromContext(ctx)
	if match == nil {
		return validateSessionOutboundEpoch(ctx, account)
	}
	result := "matched"
	entry, found, err := match.handler.readSessionContinuity(ctx, hashRiskIdentity(match.rootKey))
	if err != nil || match.persisted && !found {
		result = "ownership_unavailable"
	} else if account == nil || account.ID() != match.accountID {
		result = "account_mismatch"
	} else if found && (entry.Record.AccountID != match.accountID || entry.Record.FailoverCount != match.generation) {
		result = "owner_changed"
	} else {
		live, active := match.handler.store.LiveSessionAccountID(match.rootKey, time.Now())
		if account.SessionCapacityLimits().Enabled {
			live, active = match.handler.store.AccountSessionAccountID(match.rootKey, time.Now())
		}
		if !active || live != match.accountID {
			result = "root_owner_unavailable"
		}
	}
	match.diagnostic.Result = result
	if result != "matched" {
		return &Error{Code: "codex_background_account_mismatch", Type: ErrorTypeInvalidRequest,
			HTTPStatus: http.StatusBadRequest, Message: "后台请求的主会话账号归属已变化或不可用，请重新发起请求；已停止发送请求正文。", Cause: err}
	}
	return validateSessionOutboundEpoch(ctx, account)
}
