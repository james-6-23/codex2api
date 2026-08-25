package proxy

import (
	"strings"

	"github.com/codex2api/auth"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
)

const projectTitleRequestContextKey = "project_title_request_v1"

type projectTitleRequestRoute struct {
	GroupID int64
}

// classifyProjectTitleRequest recognizes the native Codex project-title turn
// from its original transport metadata and Responses payload. The decision is
// local to Codex2API, so direct Codex clients and NewAPI relays use one path.
func classifyProjectTitleRequest(c *gin.Context, requestedModel string, body []byte, identity *requestSessionIdentity) bool {
	if c == nil || identity == nil {
		return false
	}
	c.Set(projectTitleRequestContextKey, nil)
	row := apiKeyRowFromContext(c)
	if row == nil || row.Limits.ProjectTitleGroupID <= 0 || !isProjectTitleModel(requestedModel) {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(identity.relatedSource.ThreadSource), "system") {
		return false
	}
	if _, ok := promptfilter.ClosedProjectTitleCandidate(body, normalizeCodexInternalRequestedModel(requestedModel)); !ok {
		return false
	}
	c.Set(projectTitleRequestContextKey, projectTitleRequestRoute{GroupID: row.Limits.ProjectTitleGroupID})
	c.Set(relatedSessionObservationContextKey, nil)
	identity.bypassWindowAccounting = true
	return true
}

func isProjectTitleModel(model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	if base, stripped := stripCompactModelSuffix(model); stripped {
		model = strings.ToLower(strings.TrimSpace(base))
	}
	return model == "gpt-5.6-luna"
}

func projectTitleRequestRouteFromContext(c *gin.Context) (projectTitleRequestRoute, bool) {
	if c == nil {
		return projectTitleRequestRoute{}, false
	}
	raw, exists := c.Get(projectTitleRequestContextKey)
	route, ok := raw.(projectTitleRequestRoute)
	return route, exists && ok && route.GroupID > 0
}

func isProjectTitleRequest(c *gin.Context) bool {
	_, ok := projectTitleRequestRouteFromContext(c)
	return ok
}

func projectTitleSchedulingAPIKeyID(c *gin.Context, apiKeyID int64) int64 {
	if apiKeyID > 0 && isProjectTitleRequest(c) {
		return -apiKeyID
	}
	return apiKeyID
}

// applyProjectTitleModelRouting replaces only the account-model capability
// filter. Scheduler health, concurrency, cooldown, API-key limits, channel
// constraints and retry exclusions still apply around it normally.
func applyProjectTitleModelRouting(c *gin.Context, effectiveModel string, allowRelay bool, filter auth.AccountFilter) auth.AccountFilter {
	route, ok := projectTitleRequestRouteFromContext(c)
	if !ok {
		return filter
	}
	groups := map[int64]struct{}{route.GroupID: {}}
	return groupMembershipFilter(groups, true, func(account *auth.Account) bool {
		return passiveInternalAccountEligible(account, effectiveModel, allowRelay)
	})
}
