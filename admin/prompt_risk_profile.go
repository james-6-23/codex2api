package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
)

const promptRiskHistoryGuardrail = "画像只统计本地 warn/block 与上游 CY；影子审计和普通命中不再抬高风险。画像不会单独封禁当前请求，只控制可自动失效的模型复核豁免；达到阈值或再次出现 CY 时立即恢复同步审核。"
const promptAccountStatusGuardrail = "账号状态仅记录会话窗口与已验证的 NewAPI 用户关联，不计算风险分、不参与审核、锁定或封号。"

const promptRiskProfileListTimeout = 20 * time.Second

type promptRiskProfilesResponse struct {
	Profiles       []*database.PromptRiskProfile   `json:"profiles"`
	Total          int                             `json:"total"`
	Page           int                             `json:"page"`
	PageSize       int                             `json:"page_size"`
	ScoringVersion string                          `json:"scoring_version"`
	Guardrail      string                          `json:"guardrail"`
	AccountSummary *database.AccountSessionSummary `json:"account_summary,omitempty"`
}

type promptRiskProfileDetailResponse struct {
	Profile             *database.PromptRiskProfile           `json:"profile"`
	SessionLimit        *promptRiskSessionLimitResponse       `json:"session_limit,omitempty"`
	SessionWindows      []promptRiskSessionWindowResponse     `json:"session_windows"`
	SessionUsage        *database.SessionUsageStats           `json:"session_usage,omitempty"`
	ManualWindowLocks   []*database.PromptManualWindowLock    `json:"manual_window_locks"`
	Events              []*database.PromptRiskEvent           `json:"events"`
	TrustEvents         []*database.PromptRiskTrustEvent      `json:"trust_events"`
	AdaptiveReviewBasis promptRiskAdaptiveReviewBasisResponse `json:"adaptive_review_basis"`
	EventTotal          int                                   `json:"event_total"`
	EventPage           int                                   `json:"event_page"`
	EventPageSize       int                                   `json:"event_page_size"`
	TrustEventTotal     int                                   `json:"trust_event_total"`
	TrustEventPage      int                                   `json:"trust_event_page"`
	TrustEventPageSize  int                                   `json:"trust_event_page_size"`
	ScoringVersion      string                                `json:"scoring_version"`
	Guardrail           string                                `json:"guardrail"`
}

type promptRiskSessionLimitResponse struct {
	Mode                string `json:"mode"`
	Limit               int    `json:"limit"`
	WindowSeconds       int    `json:"window_seconds"`
	EffectiveEnabled    bool   `json:"effective_enabled"`
	EffectiveLimit      int    `json:"effective_limit"`
	EffectiveWindow     int    `json:"effective_window_seconds"`
	GlobalEnabled       bool   `json:"global_enabled"`
	GlobalLimit         int    `json:"global_limit"`
	GlobalWindowSeconds int    `json:"global_window_seconds"`
	Source              string `json:"source"`
}

type promptRiskSessionLimitUpdateRequest struct {
	Mode          string `json:"mode"`
	Limit         int    `json:"limit"`
	WindowSeconds int    `json:"window_seconds"`
}

type promptRiskSessionWindowResponse struct {
	SessionHash      string     `json:"session_hash"`
	CreatedAt        *time.Time `json:"created_at,omitempty"`
	ExpiresAt        time.Time  `json:"expires_at"`
	RemainingSeconds int64      `json:"remaining_seconds"`
	AccountID        int64      `json:"account_id,omitempty"`
	AccountName      string     `json:"account_name,omitempty"`
	Model            string     `json:"model,omitempty"`
	ReasoningEffort  string     `json:"reasoning_effort,omitempty"`
	ClientUserAgent  string     `json:"client_user_agent,omitempty"`
	PromptPreview    string     `json:"prompt_preview,omitempty"`
	Last500At        *time.Time `json:"last_500_at,omitempty"`
}

type promptRiskAdaptiveReviewBasisResponse struct {
	Enabled                    bool       `json:"enabled"`
	ReviewEnabled              bool       `json:"review_enabled"`
	Eligible                   bool       `json:"eligible"`
	Decision                   string     `json:"decision"`
	CleanReviewCount           int        `json:"clean_review_count"`
	PositiveEvidenceCount      int        `json:"positive_evidence_count"`
	MinCleanReviews            int        `json:"min_clean_reviews"`
	MinObservationHours        int        `json:"min_observation_hours"`
	ObservationHours           int        `json:"observation_hours"`
	SamplePercent              int        `json:"sample_percent"`
	ForceReviewIntervalMinutes int        `json:"force_review_interval_minutes"`
	TrustDurationHours         int        `json:"trust_duration_hours"`
	RiskThreshold              int        `json:"risk_threshold"`
	FirstCleanAt               *time.Time `json:"first_clean_at,omitempty"`
	LastCleanAt                *time.Time `json:"last_clean_at,omitempty"`
	NextForcedReviewAt         *time.Time `json:"next_forced_review_at,omitempty"`
	ForceReviewDue             bool       `json:"force_review_due"`
}

func (h *Handler) ListPromptRiskProfiles(c *gin.Context) {
	page := positiveQueryInt(c, "page", 1)
	pageSize := positiveQueryInt(c, "page_size", 20)
	apiKeyID := promptRiskPositiveInt64(c.Query("api_key_id"))
	accountID := promptRiskPositiveInt64(c.Query("account_id"))
	minScore := positiveQueryInt(c, "min_score", 0)
	if minScore > 100 {
		minScore = 100
	}
	activityState := strings.ToLower(strings.TrimSpace(c.Query("activity_state")))
	if activityState != "" && activityState != "all" && activityState != "active" && activityState != "identity_only" {
		writeError(c, http.StatusBadRequest, "activity_state 必须为 all、active 或 identity_only")
		return
	}
	// 生产画像列表需要聚合近 30 天事件。高流量实例可能包含数十万条记录，
	// 5 秒会在 SQLite 正常计算完成前主动取消，表现为稳定的 500。
	ctx, cancel := context.WithTimeout(c.Request.Context(), promptRiskProfileListTimeout)
	defer cancel()
	lockTTL, userCooldownTTL := h.promptConversationRestrictionTTLs()
	query := database.PromptRiskProfileQuery{
		Page: page, PageSize: pageSize, SubjectType: c.Query("subject_type"), Platform: c.Query("platform"),
		RiskLevel: c.Query("risk_level"), APIKeyID: apiKeyID, AccountID: accountID, MinScore: minScore, Query: c.Query("q"),
		UpstreamCYOnly: c.Query("cy_only") == "true", ActivityState: activityState,
		PrioritizeActiveLocks: true, ActiveLocksOnly: c.Query("locked_only") == "true",
		ConversationLockTTL: lockTTL, UserCyberCooldownTTL: userCooldownTTL,
	}
	profiles, total, err := h.db.ListPromptRiskProfiles(ctx, query)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if profiles == nil {
		profiles = []*database.PromptRiskProfile{}
	}
	accountStatus := strings.TrimSpace(c.Query("subject_type")) == database.PromptRiskSubjectAccountStatus
	guardrail := promptRiskHistoryGuardrail
	var accountSummary *database.AccountSessionSummary
	if accountStatus {
		guardrail = promptAccountStatusGuardrail
		accountSummary, err = h.db.GetAccountSessionSummary(ctx, query)
		if err != nil {
			writeInternalError(c, err)
			return
		}
	} else {
		h.attachPromptRiskTrustPolicies(ctx, profiles)
		h.attachPromptConversationLocks(ctx, profiles)
	}
	c.JSON(http.StatusOK, promptRiskProfilesResponse{
		Profiles: profiles, Total: total, Page: page, PageSize: pageSize,
		ScoringVersion: database.PromptRiskScoringVersion, Guardrail: guardrail,
		AccountSummary: accountSummary,
	})
}

func (h *Handler) GetPromptRiskProfile(c *gin.Context) {
	subjectType := strings.TrimSpace(c.Param("subject_type"))
	subjectKey := strings.TrimSpace(c.Param("subject_key"))
	if !validPromptRiskSubjectType(subjectType) || subjectKey == "" {
		writeError(c, http.StatusBadRequest, "风险画像标识无效")
		return
	}
	eventPage := positiveQueryInt(c, "event_page", 1)
	eventPageSize := positiveQueryInt(c, "event_page_size", 20)
	trustEventPage := positiveQueryInt(c, "trust_event_page", 1)
	trustEventPageSize := positiveQueryInt(c, "trust_event_page_size", 20)
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	lockTTL, userCooldownTTL := h.promptConversationRestrictionTTLs()
	profiles, _, err := h.db.ListPromptRiskProfiles(ctx, database.PromptRiskProfileQuery{
		Page: 1, PageSize: 1, SubjectType: subjectType, SubjectKey: subjectKey,
		PrioritizeActiveLocks: true, ConversationLockTTL: lockTTL, UserCyberCooldownTTL: userCooldownTTL,
	})
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if len(profiles) == 0 {
		writeError(c, http.StatusNotFound, "风险画像不存在")
		return
	}
	profile := profiles[0]
	events, total, err := h.db.ListPromptRiskEvents(ctx, subjectType, subjectKey, database.PromptRiskEventQuery{Page: eventPage, PageSize: eventPageSize})
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if events == nil {
		events = []*database.PromptRiskEvent{}
	}
	h.attachPromptRiskTrustPolicies(ctx, []*database.PromptRiskProfile{profile})
	h.attachPromptConversationLocks(ctx, []*database.PromptRiskProfile{profile})
	trustEvents, trustEventTotal, err := h.db.ListPromptRiskTrustEventsPage(ctx, subjectType, subjectKey, trustEventPage, trustEventPageSize)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if trustEvents == nil {
		trustEvents = []*database.PromptRiskTrustEvent{}
	}
	adaptive := promptfilter.DefaultAdvancedConfig().AdaptiveReview
	reviewEnabled := false
	if h.store != nil {
		cfg := h.store.GetPromptFilterConfig()
		adaptive = promptfilter.NormalizeAdvancedConfig(cfg.Advanced).AdaptiveReview
		reviewEnabled = cfg.Review.Enabled
	}
	now := time.Now().UTC()
	basis, err := h.db.GetPromptRiskAdaptiveReviewBasis(ctx, subjectType, subjectKey, now.Add(-time.Duration(adaptive.TrustDurationHours)*time.Hour))
	if err != nil {
		writeInternalError(c, err)
		return
	}
	adaptiveBasis := buildPromptRiskAdaptiveReviewBasis(profile, basis, adaptive, reviewEnabled, now)
	sessionLimit := h.promptRiskSessionLimitResponse(profile)
	sessionWindows := h.promptRiskSessionWindows(ctx, profile, now)
	var sessionUsage *database.SessionUsageStats
	manualLocks := make([]*database.PromptManualWindowLock, 0)
	if profile.SubjectType == database.PromptRiskSubjectNewAPIUser {
		sessionUsage, err = h.db.GetNewAPIUserSessionUsage(ctx, profile.Platform, profile.NewAPIUserID)
		if err != nil {
			writeInternalError(c, err)
			return
		}
		manualLocks, err = h.db.ListPromptUserWindowLocks(ctx, profile.Platform, profile.NewAPIUserID, now)
		if err != nil {
			writeInternalError(c, err)
			return
		}
		if err := h.attachPromptWindowAccountErrors(ctx, profile, sessionWindows); err != nil {
			writeInternalError(c, err)
			return
		}
	}
	c.JSON(http.StatusOK, promptRiskProfileDetailResponse{
		Profile: profile, SessionLimit: sessionLimit, SessionWindows: sessionWindows, Events: events, TrustEvents: trustEvents, AdaptiveReviewBasis: adaptiveBasis,
		SessionUsage:      sessionUsage,
		ManualWindowLocks: manualLocks,
		EventTotal:        total, EventPage: eventPage, EventPageSize: eventPageSize,
		TrustEventTotal: trustEventTotal, TrustEventPage: trustEventPage, TrustEventPageSize: trustEventPageSize,
		ScoringVersion: database.PromptRiskScoringVersion, Guardrail: promptRiskHistoryGuardrail,
	})
}

func (h *Handler) promptRiskSessionWindows(ctx context.Context, profile *database.PromptRiskProfile, now time.Time) []promptRiskSessionWindowResponse {
	items := make([]promptRiskSessionWindowResponse, 0)
	if h == nil || h.cache == nil || profile == nil || profile.SubjectType != database.PromptRiskSubjectNewAPIUser {
		return items
	}
	subject := cache.PromptSessionLimitSubject(profile.Platform, profile.NewAPIUserID)
	if subject == "" {
		return items
	}
	raw, found, err := h.cache.GetRuntime(ctx, cache.PromptSessionLimitRuntimeNamespace, subject)
	if err != nil || !found {
		return items
	}
	state := cache.PromptSessionLimitState{}
	if json.Unmarshal(raw, &state) != nil {
		return items
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}
	for sessionHash, expiresAt := range state.Sessions {
		sessionHash = strings.TrimSpace(sessionHash)
		if sessionHash == "" || !expiresAt.After(now) {
			continue
		}
		detail := state.Details[sessionHash]
		remaining := int64((expiresAt.Sub(now) + time.Second - 1) / time.Second)
		item := promptRiskSessionWindowResponse{
			SessionHash: sessionHash, ExpiresAt: expiresAt.UTC(), RemainingSeconds: remaining,
			AccountID: detail.AccountID, Model: strings.TrimSpace(detail.Model), ReasoningEffort: strings.TrimSpace(detail.ReasoningEffort),
			ClientUserAgent: strings.TrimSpace(detail.ClientUserAgent), PromptPreview: strings.TrimSpace(detail.PromptPreview),
		}
		if !detail.CreatedAt.IsZero() {
			createdAt := detail.CreatedAt.UTC()
			item.CreatedAt = &createdAt
		}
		if detail.AccountID > 0 && h.store != nil {
			if account := h.store.FindByID(detail.AccountID); account != nil {
				item.AccountName = strings.TrimSpace(account.Email)
			}
		}
		items = append(items, item)
	}
	sort.Slice(items, func(i, j int) bool {
		if items[i].CreatedAt != nil && items[j].CreatedAt != nil && !items[i].CreatedAt.Equal(*items[j].CreatedAt) {
			return items[i].CreatedAt.After(*items[j].CreatedAt)
		}
		return items[i].ExpiresAt.Before(items[j].ExpiresAt)
	})
	return items
}

func (h *Handler) promptRiskSessionLimitResponse(profile *database.PromptRiskProfile) *promptRiskSessionLimitResponse {
	if profile == nil || profile.SubjectType != database.PromptRiskSubjectNewAPIUser || strings.TrimSpace(profile.NewAPIUserID) == "" {
		return nil
	}
	risk := promptfilter.DefaultAdvancedConfig().Risk
	if h.store != nil {
		risk = promptfilter.NormalizeAdvancedConfig(h.store.GetPromptFilterConfig().Advanced).Risk
	}
	result := &promptRiskSessionLimitResponse{
		Mode:                "inherit",
		EffectiveEnabled:    risk.SessionCreationLimitEnabled,
		EffectiveLimit:      risk.SessionCreationLimit,
		EffectiveWindow:     risk.SessionCreationLimitWindowSeconds,
		GlobalEnabled:       risk.SessionCreationLimitEnabled,
		GlobalLimit:         risk.SessionCreationLimit,
		GlobalWindowSeconds: risk.SessionCreationLimitWindowSeconds,
		Source:              "global",
	}
	if h.store == nil {
		return result
	}
	if override, ok := h.store.GetPromptSessionLimitOverride(profile.Platform, profile.NewAPIUserID); ok {
		result.Mode = override.Mode
		result.Limit = override.Limit
		result.WindowSeconds = override.WindowSeconds
		result.Source = "user"
		if override.Mode == database.PromptSessionLimitModeOff {
			result.EffectiveEnabled = false
			result.EffectiveLimit = 0
			result.EffectiveWindow = 0
		} else {
			result.EffectiveEnabled = true
			result.EffectiveLimit = override.Limit
			result.EffectiveWindow = override.WindowSeconds
		}
	}
	return result
}

// UpdatePromptRiskProfileSessionLimit configures a verified NewAPI person's
// session creation policy. inherit deletes the override, custom takes priority
// over the global Prompt setting, and off explicitly exempts this person.
func (h *Handler) UpdatePromptRiskProfileSessionLimit(c *gin.Context) {
	subjectType := strings.TrimSpace(c.Param("subject_type"))
	subjectKey := strings.TrimSpace(c.Param("subject_key"))
	if subjectType != database.PromptRiskSubjectNewAPIUser || subjectKey == "" {
		writeError(c, http.StatusBadRequest, "只有 NewAPI 用户画像可以单独配置会话限制")
		return
	}
	var req promptRiskSessionLimitUpdateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	req.Mode = strings.ToLower(strings.TrimSpace(req.Mode))
	if req.Mode != "inherit" && req.Mode != database.PromptSessionLimitModeCustom && req.Mode != database.PromptSessionLimitModeOff {
		writeError(c, http.StatusBadRequest, "会话限制模式必须是 inherit、custom 或 off")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	profile, err := h.db.GetPromptRiskProfile(ctx, subjectType, subjectKey)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(c, http.StatusNotFound, "风险画像不存在")
		return
	}
	if err != nil {
		writeInternalError(c, err)
		return
	}
	platform := strings.ToLower(strings.TrimSpace(profile.Platform))
	userID := strings.TrimSpace(profile.NewAPIUserID)
	if platform == "" || userID == "" {
		writeError(c, http.StatusBadRequest, "该画像缺少已验证的 NewAPI 用户身份")
		return
	}
	if req.Mode == "inherit" {
		if err := h.db.DeletePromptSessionLimitOverride(ctx, platform, userID); err != nil {
			writeInternalError(c, err)
			return
		}
		if h.store != nil {
			h.store.DeletePromptSessionLimitOverride(platform, userID)
		}
	} else {
		item, err := h.db.UpsertPromptSessionLimitOverride(ctx, database.PromptSessionLimitOverride{
			Platform: platform, NewAPIUserID: userID, Mode: req.Mode,
			Limit: req.Limit, WindowSeconds: req.WindowSeconds,
		})
		if err != nil {
			if errors.Is(err, database.ErrInvalidPromptSessionLimitOverride) {
				writeError(c, http.StatusBadRequest, err.Error())
			} else {
				log.Printf("保存用户会话限制失败: %v", err)
				writeError(c, http.StatusInternalServerError, "保存会话限制失败，请稍后重试")
			}
			return
		}
		if h.store != nil {
			h.store.ApplyPromptSessionLimitOverride(*item)
		}
	}
	c.JSON(http.StatusOK, gin.H{"session_limit": h.promptRiskSessionLimitResponse(profile)})
}

func buildPromptRiskAdaptiveReviewBasis(profile *database.PromptRiskProfile, basis database.PromptRiskAdaptiveReviewBasis, adaptive promptfilter.AdaptiveReviewConfig, reviewEnabled bool, now time.Time) promptRiskAdaptiveReviewBasisResponse {
	result := promptRiskAdaptiveReviewBasisResponse{
		Enabled: adaptive.Enabled, ReviewEnabled: reviewEnabled,
		CleanReviewCount: basis.CleanReviewCount, PositiveEvidenceCount: basis.PositiveEvidenceCount,
		MinCleanReviews: adaptive.MinCleanReviews, MinObservationHours: adaptive.MinObservationHours,
		SamplePercent: adaptive.SamplePercent, ForceReviewIntervalMinutes: adaptive.ForceReviewIntervalMinutes,
		TrustDurationHours: adaptive.TrustDurationHours, RiskThreshold: 35,
		FirstCleanAt: basis.FirstCleanAt, LastCleanAt: basis.LastCleanAt,
	}
	if profile == nil {
		result.Decision = "unavailable"
		return result
	}
	if profile.TrustPolicy != nil && profile.TrustPolicy.RiskThreshold > 0 {
		result.RiskThreshold = profile.TrustPolicy.RiskThreshold
	}
	if basis.FirstCleanAt != nil && now.After(*basis.FirstCleanAt) {
		result.ObservationHours = int(now.Sub(*basis.FirstCleanAt).Hours())
	}
	result.Eligible = reviewEnabled && adaptive.Enabled && profile.IsPerson &&
		basis.CleanReviewCount >= adaptive.MinCleanReviews && basis.PositiveEvidenceCount == 0 &&
		result.ObservationHours >= adaptive.MinObservationHours && profile.RiskScore < 15 && profile.RiskLevel == database.PromptRiskLevelLow
	if !reviewEnabled || !adaptive.Enabled {
		result.Decision = "disabled"
	} else if !profile.IsPerson {
		result.Decision = "not_person"
	} else if profile.TrustPolicy != nil && profile.TrustPolicy.Status == database.PromptRiskTrustStatusActive {
		result.Decision = "adaptive_active"
	} else if profile.TrustPolicy != nil && profile.TrustPolicy.Status == database.PromptRiskTrustStatusSuspended {
		result.Decision = "suspended"
	} else if result.Eligible {
		result.Decision = "eligible"
	} else {
		result.Decision = "building_history"
	}
	if profile.TrustPolicy != nil && profile.TrustPolicy.Status == database.PromptRiskTrustStatusActive {
		if profile.TrustPolicy.LastModelReviewAt == nil || adaptive.ForceReviewIntervalMinutes <= 0 {
			result.ForceReviewDue = true
		} else {
			next := profile.TrustPolicy.LastModelReviewAt.Add(time.Duration(adaptive.ForceReviewIntervalMinutes) * time.Minute)
			result.NextForcedReviewAt = &next
			result.ForceReviewDue = !next.After(now)
		}
	}
	return result
}

func (h *Handler) attachPromptRiskTrustPolicies(ctx context.Context, profiles []*database.PromptRiskProfile) {
	if h == nil || h.db == nil || len(profiles) == 0 {
		return
	}
	policies, err := h.db.ListAllPromptRiskTrustPolicies(ctx, "all")
	if err != nil {
		return
	}
	bySubject := make(map[string]*database.PromptRiskTrustPolicy, len(policies))
	for _, policy := range policies {
		if policy != nil {
			bySubject[policy.SubjectType+"\x00"+policy.SubjectKey] = policy
		}
	}
	for _, profile := range profiles {
		if profile != nil {
			profile.TrustPolicy = bySubject[profile.SubjectType+"\x00"+profile.SubjectKey]
		}
	}
}

func (h *Handler) attachPromptConversationLocks(ctx context.Context, profiles []*database.PromptRiskProfile) {
	if h == nil || h.db == nil {
		return
	}
	lockTTL, userCooldownTTL := h.promptConversationRestrictionTTLs()
	for _, profile := range profiles {
		if profile == nil {
			continue
		}
		switch profile.SubjectType {
		case database.PromptRiskSubjectSession:
			if strings.TrimSpace(profile.SubjectKey) == "" {
				continue
			}
			item, err := h.db.GetActivePromptConversationLockBySessionHash(ctx, profile.SubjectKey)
			if err == nil {
				effectiveTTL := lockTTL
				if item != nil && item.IdentityKind == database.PromptConversationLockIdentityFingerprintReplay {
					effectiveTTL = userCooldownTTL
				}
				if effectiveTTL > 0 && (item == nil || !item.LockedAt.After(time.Now().UTC().Add(-effectiveTTL))) {
					item = nil
				}
				scope := database.PromptConversationRestrictionScopeConversation
				if item != nil && item.IdentityKind == database.PromptConversationLockIdentityFingerprintReplay {
					scope = database.PromptConversationRestrictionScopeFingerprintReplay
				}
				decoratePromptConversationRestriction(item, scope, effectiveTTL)
				profile.ConversationLock = item
			}
		case database.PromptRiskSubjectNewAPIUser:
			if strings.TrimSpace(profile.Platform) == "" || strings.TrimSpace(profile.NewAPIUserID) == "" {
				continue
			}
			item, _, err := h.db.GetActivePromptConversationRestriction(
				ctx, "", profile.Platform, profile.NewAPIUserID, lockTTL, userCooldownTTL,
			)
			if err == nil {
				decoratePromptConversationRestriction(item, database.PromptConversationRestrictionScopeUserCooldown, userCooldownTTL)
				profile.ConversationLock = item
			}
		}
	}
}

func (h *Handler) promptConversationRestrictionTTLs() (time.Duration, time.Duration) {
	advanced := promptfilter.DefaultAdvancedConfig()
	if h != nil && h.store != nil {
		advanced = promptfilter.NormalizeAdvancedConfig(h.store.GetPromptFilterConfig().Advanced)
	}
	return time.Duration(advanced.Enforcement.ConversationLockTTLHours) * time.Hour,
		time.Duration(advanced.Enforcement.UserCyberCooldownMinutes) * time.Minute
}

func decoratePromptConversationRestriction(item *database.PromptConversationLock, scope string, ttl time.Duration) {
	if item == nil {
		return
	}
	item.RestrictionScope = scope
	if ttl <= 0 || item.LockedAt.IsZero() {
		return
	}
	expiresAt := item.LockedAt.UTC().Add(ttl)
	item.ExpiresAt = &expiresAt
	remaining := time.Until(expiresAt)
	if remaining > 0 {
		item.RemainingSeconds = int64((remaining + time.Second - 1) / time.Second)
	}
}

func promptRiskPositiveInt64(raw string) int64 {
	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || value <= 0 {
		return 0
	}
	return value
}

func validPromptRiskSubjectType(value string) bool {
	switch value {
	case database.PromptRiskSubjectNewAPIUser, database.PromptRiskSubjectSession, database.PromptRiskSubjectAPIKey,
		database.PromptRiskSubjectClientIP, database.PromptRiskSubjectUpstreamAccount:
		return true
	default:
		return false
	}
}
