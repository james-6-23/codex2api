package proxy

import (
	"strings"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

const sessionModelUnavailableMessage = "当前会话绑定的上游账号不支持所选模型，请新开对话后使用该模型。"

func sessionModelUnavailableError() *api.APIError {
	return api.NewAPIError(api.ErrCodeSessionModelUnavailable, sessionModelUnavailableMessage, api.ErrorTypeInvalidRequest)
}

func sessionModelErrorForRequest(requestContext *gin.Context) *api.APIError {
	if selectionTraceForRequest(requestContext).SessionModelDenied() {
		return sessionModelUnavailableError()
	}
	return nil
}

func (handler *Handler) configureSessionModelAffinity(requestContext *gin.Context, identity requestSessionIdentity, key, originalModel, effectiveModel string, compact bool) *api.APIError {
	if !identity.stableIdentity || identity.unlinkedFallbackOnly || strings.TrimSpace(key) == "" || identity.bypassWindowAccounting {
		return nil
	}
	if handler.passiveInternalModelsAllowed(requestContext) && !identity.ownsRootBinding {
		return nil
	}
	trace := selectionTraceForRequest(requestContext)
	trace.SetSessionModelFilter(sessionModelSupportFilter(originalModel, effectiveModel, compact))
	if accountID, found := handler.store.LiveSessionAccountID(key, time.Now()); found {
		if !trace.CheckSessionModel(handler.store.FindByID(accountID)) {
			recordUsageRootAccount(requestContext, accountID, true)
			return sessionModelUnavailableError()
		}
	}
	return nil
}

func sessionModelSupportFilter(originalModel, effectiveModel string, compact bool) auth.AccountFilter {
	candidates := []string{originalModel, effectiveModel}
	var compactCandidates []compactMappingCandidate
	if compact {
		compactCandidates = compactMappingCandidates(originalModel, effectiveModel)
	}
	return func(account *auth.Account) bool {
		if account == nil {
			return false
		}
		if account.IsAntigravityAPI() {
			_, supported := antigravityResolvePublicModelForAccount(account, effectiveModel)
			return supported
		}
		if !account.IsRelayStyle() {
			return account.SupportsCodexModel(effectiveModel)
		}
		routedModel, mapped := resolveAccountModelMappingForCandidates(account, candidates...)
		if compact {
			routedModel, mapped = resolveAccountCompactModelMappingForCandidates(account, compactCandidates)
		}
		if !mapped || routedModel == "" {
			routedModel = effectiveModel
		}
		if account.IsClaudeOAuth() {
			return claudeAccountSupportsModel(account, routedModel)
		}
		if account.IsGrokAPI() {
			return account.GrokChannelSupportsModel(routedModel)
		}
		return account.SupportsOpenAIResponsesModel(routedModel)
	}
}
