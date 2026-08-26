package proxy

import (
	"strings"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

const projectTitleRequestContextKey = "project_title_request_v1"

type projectTitleRequestRoute struct {
	GroupID int64
}

// classifyProjectTitleRequest recognizes the native system field or NewAPI's
// signed system_passive field. Model names and prompt wording are deliberately
// not part of this Codex2API routing decision.
func classifyProjectTitleRequest(c *gin.Context, _ string, _ []byte, identity *requestSessionIdentity) bool {
	if c == nil || identity == nil {
		return false
	}
	c.Set(projectTitleRequestContextKey, nil)
	row := apiKeyRowFromContext(c)
	if row == nil || row.Limits.ProjectTitleGroupID <= 0 {
		return false
	}
	fieldMatched := !identity.bypassWindowAccounting && strings.EqualFold(strings.TrimSpace(identity.relatedSource.ThreadSource), "system")
	if raw, exists := c.Get(newAPIPolicyMetaContextKey); exists {
		policy, ok := raw.(verifiedNewAPIPolicyContext)
		fieldMatched = ok && policy.MetaVerified && policy.Meta.PassiveFeature == newAPIPassiveFeatureSystemPassive
	}
	if !fieldMatched {
		return false
	}
	c.Set(projectTitleRequestContextKey, projectTitleRequestRoute{GroupID: row.Limits.ProjectTitleGroupID})
	c.Set(relatedSessionObservationContextKey, nil)
	identity.bypassWindowAccounting = true
	return true
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
