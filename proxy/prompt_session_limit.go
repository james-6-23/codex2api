package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const promptSessionLimitCleanupInterval = time.Minute

const promptSessionLimitCacheTimeout = 500 * time.Millisecond

func (h *Handler) ensurePromptSessionLimitsLoaded(subject string, now time.Time) {
	if h == nil || h.cache == nil || strings.TrimSpace(subject) == "" {
		return
	}
	if now.IsZero() {
		now = time.Now()
	}
	h.promptSessionLimitLoadMu.Lock()
	defer h.promptSessionLimitLoadMu.Unlock()
	h.promptSessionLimitMu.Lock()
	if h.promptSessionLimitsLoaded[subject] {
		h.promptSessionLimitMu.Unlock()
		return
	}
	h.promptSessionLimitMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), promptSessionLimitCacheTimeout)
	raw, found, err := h.cache.GetRuntime(ctx, cache.PromptSessionLimitRuntimeNamespace, subject)
	cancel()
	if err != nil {
		log.Printf("读取用户窗口限制缓存失败: subject=%s err=%v", subject, err)
		return
	}
	persisted := cache.PromptSessionLimitState{}
	if found {
		if err := json.Unmarshal(raw, &persisted); err != nil {
			log.Printf("解析用户窗口限制缓存失败: subject=%s err=%v", subject, err)
			found = false
		}
	}

	h.promptSessionLimitMu.Lock()
	if h.promptSessionLimits == nil {
		h.promptSessionLimits = make(map[string]map[string]time.Time)
	}
	if h.promptSessionLimitsLoaded == nil {
		h.promptSessionLimitsLoaded = make(map[string]bool)
	}
	if h.promptSessionWindowDetails == nil {
		h.promptSessionWindowDetails = make(map[string]map[string]cache.PromptSessionWindowDetail)
	}
	if found {
		sessions := h.promptSessionLimits[subject]
		if sessions == nil {
			sessions = make(map[string]time.Time)
			h.promptSessionLimits[subject] = sessions
		}
		for sessionHash, expiresAt := range persisted.Sessions {
			sessionHash = strings.TrimSpace(sessionHash)
			if sessionHash == "" || !expiresAt.After(now) {
				continue
			}
			if current, exists := sessions[sessionHash]; !exists || expiresAt.After(current) {
				sessions[sessionHash] = expiresAt
				if detail, ok := persisted.Details[sessionHash]; ok {
					if h.promptSessionWindowDetails[subject] == nil {
						h.promptSessionWindowDetails[subject] = make(map[string]cache.PromptSessionWindowDetail)
					}
					detail.ExpiresAt = expiresAt
					h.promptSessionWindowDetails[subject][sessionHash] = detail
				}
			}
		}
	}
	h.promptSessionLimitsLoaded[subject] = true
	h.promptSessionLimitMu.Unlock()
}

func (h *Handler) persistPromptSessionLimits(subject string, now time.Time) {
	if h == nil || h.cache == nil || strings.TrimSpace(subject) == "" {
		return
	}
	h.promptSessionLimitPersistMu.Lock()
	defer h.promptSessionLimitPersistMu.Unlock()
	if now.IsZero() {
		now = time.Now()
	}
	h.promptSessionLimitMu.Lock()
	sessions := h.promptSessionLimits[subject]
	persisted := cache.PromptSessionLimitState{
		Version: 2, Sessions: make(map[string]time.Time, len(sessions)),
		Details: make(map[string]cache.PromptSessionWindowDetail, len(sessions)),
	}
	details := h.promptSessionWindowDetails[subject]
	maxRemaining := time.Duration(0)
	for sessionHash, expiresAt := range sessions {
		if strings.TrimSpace(sessionHash) == "" || !expiresAt.After(now) {
			continue
		}
		persisted.Sessions[sessionHash] = expiresAt
		if detail, ok := details[sessionHash]; ok {
			detail.ExpiresAt = expiresAt
			persisted.Details[sessionHash] = detail
		}
		if remaining := expiresAt.Sub(now); remaining > maxRemaining {
			maxRemaining = remaining
		}
	}
	h.promptSessionLimitMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), promptSessionLimitCacheTimeout)
	defer cancel()
	if len(persisted.Sessions) == 0 || maxRemaining <= 0 {
		_ = h.cache.DeleteRuntime(ctx, cache.PromptSessionLimitRuntimeNamespace, subject)
		return
	}
	payload, err := json.Marshal(persisted)
	if err != nil {
		return
	}
	if err := h.cache.SetRuntime(ctx, cache.PromptSessionLimitRuntimeNamespace, subject, payload, maxRemaining); err != nil {
		log.Printf("写入用户窗口限制缓存失败: subject=%s err=%v", subject, err)
	}
}

type promptSessionCreationLimitStatus struct {
	Enabled       bool
	Subject       string
	SessionHash   string
	Used          int
	Limit         int
	WindowSeconds int
	RetryAfter    int
	Existing      bool
	// IdentityConflict means signed NewAPI root metadata and the current
	// response.create frame describe different conversations. It is not a
	// capacity exhaustion and must be surfaced with its own error code.
	IdentityConflict bool
}

func promptSessionWindowRequestDetail(c *gin.Context, body []byte, account *auth.Account, cfg promptfilter.Config) cache.PromptSessionWindowDetail {
	detail := cache.PromptSessionWindowDetail{}
	if account != nil {
		detail.AccountID = account.ID()
	}
	if c != nil {
		detail.ClientUserAgent = normalizeUsageLogUserAgent(c.GetHeader("User-Agent"))
	}
	if len(body) == 0 {
		return detail
	}
	detail.Model = strings.TrimSpace(gjson.GetBytes(body, "model").String())
	detail.ReasoningEffort = strings.TrimSpace(extractReasoningEffort(body))
	if detail.ReasoningEffort == "" {
		detail.ReasoningEffort = strings.TrimSpace(gjson.GetBytes(body, "output_config.effort").String())
	}
	endpoint := ""
	if c != nil && c.Request != nil && c.Request.URL != nil {
		endpoint = c.Request.URL.Path
	}
	maxTextLength := cfg.MaxTextLength
	if maxTextLength <= 0 {
		maxTextLength = promptfilter.DefaultMaxTextLength
	}
	envelope := promptfilter.BuildEnvelope(body, endpoint, detail.Model, promptfilter.TransportHTTP, maxTextLength)
	parts := make([]string, 0, 2)
	for _, segment := range envelope.SegmentsForOrigin(promptfilter.OriginCurrentUser) {
		if text := strings.TrimSpace(segment.Text); text != "" {
			parts = append(parts, text)
		}
	}
	detail.PromptPreview = promptfilter.RedactedPreview(strings.Join(parts, "\n"), 500)
	return detail
}

// checkPromptSessionCreationLimitForSelectedAccount deliberately runs only
// after scheduling has selected the concrete upstream account. The user-level
// creation limit is an extension of account session-window control: relay/API
// accounts that have not enabled session capacity must not make a passive
// internal request consume one of the user's windows merely because the
// downstream Codex client carried its normal Session-Id.
func (h *Handler) checkPromptSessionCreationLimitForSelectedAccount(c *gin.Context, body []byte, account *auth.Account) (promptSessionCreationLimitStatus, bool) {
	return h.checkPromptSessionCreationLimitForSelectedAccountAdmission(c, body, account, "", 0)
}

// checkPromptSessionCreationLimitForSelectedAccountAdmission keeps account
// affinity admission separate from user-window creation. priorSessionAccountID
// describes the account-session state before scheduling selected account.
func (h *Handler) checkPromptSessionCreationLimitForSelectedAccountAdmission(c *gin.Context, body []byte, account *auth.Account, affinityKey string, priorSessionAccountID int64) (promptSessionCreationLimitStatus, bool) {
	if account == nil || (c != nil && c.GetBool("prompt_intelligence_internal")) {
		return promptSessionCreationLimitStatus{}, false
	}
	enabled, _, _ := account.SessionCapacityConfig()
	if !enabled {
		return promptSessionCreationLimitStatus{}, false
	}
	return h.checkPromptSessionCreationLimitWithAccountAdmission(c, h.promptFilterConfigForRequest(c), body, account, affinityKey, priorSessionAccountID)
}

// releaseSelectedAccountAfterPromptSessionRejection undoes only the capacity
// admission made by the current selection. An already-active session may have
// existed before this request and must remain available for its other turns.
func (h *Handler) releaseSelectedAccountAfterPromptSessionRejection(account *auth.Account, affinityKey string, priorSessionAccountID int64) {
	if h == nil || h.store == nil || account == nil {
		return
	}
	if priorSessionAccountID != account.ID() {
		h.store.RemoveAccountSession(account.ID(), affinityKey)
	}
	h.store.Release(account)
}

func (h *Handler) checkPromptSessionCreationLimit(c *gin.Context, cfg promptfilter.Config, body []byte) (promptSessionCreationLimitStatus, bool) {
	return h.checkPromptSessionCreationLimitWithAccount(c, cfg, body, nil)
}

func (h *Handler) checkPromptSessionCreationLimitWithAccount(c *gin.Context, cfg promptfilter.Config, body []byte, account *auth.Account) (promptSessionCreationLimitStatus, bool) {
	return h.checkPromptSessionCreationLimitWithAccountAdmission(c, cfg, body, account, "", 0)
}

func (h *Handler) checkPromptSessionCreationLimitWithAccountAdmission(c *gin.Context, cfg promptfilter.Config, body []byte, account *auth.Account, affinityKey string, priorSessionAccountID int64) (promptSessionCreationLimitStatus, bool) {
	risk := cfg.Advanced.Risk
	status := promptSessionCreationLimitStatus{
		Enabled:       risk.SessionCreationLimitEnabled,
		Limit:         risk.SessionCreationLimit,
		WindowSeconds: risk.SessionCreationLimitWindowSeconds,
	}
	if h == nil || c == nil {
		return status, false
	}
	policyStatus, policyContext := h.cachedNewAPIPolicyAuditState(c)
	verifiedPerson := (policyStatus == "verified" || policyStatus == "signed_response") &&
		policyContext.MetaVerified && strings.TrimSpace(policyContext.Identity.UserID) != ""
	if verifiedPerson && h.store != nil {
		if override, ok := h.store.GetPromptSessionLimitOverride(policyContext.Platform, policyContext.Identity.UserID); ok {
			switch override.Mode {
			case database.PromptSessionLimitModeOff:
				status.Enabled = false
				status.Limit = 0
				status.WindowSeconds = 0
			case database.PromptSessionLimitModeCustom:
				status.Enabled = true
				status.Limit = override.Limit
				status.WindowSeconds = override.WindowSeconds
			}
		}
	}
	if !status.Enabled || status.Limit <= 0 || status.WindowSeconds <= 0 {
		return status, false
	}

	rootIdentity := h.resolveRequestRootSessionIdentityForContext(c, body)
	if h.verifiedNewAPISessionAccountingBypass(c) {
		setLocalSessionAccountingBypass(c, true)
		return status, false
	}
	if requestSessionAccountingBypass(c) {
		return status, false
	}
	if classifyLocalCodexIndependentSessionAccounting(c, rootIdentity) {
		setLocalSessionAccountingBypass(c, true)
		return status, false
	}
	if rootIdentity.authoritative && rootIdentity.conflict {
		status.IdentityConflict = true
		return status, true
	}
	// Hidden related requests borrow the root conversation. Compaction also
	// borrows it even when Codex labels the turn as user-authored: it may recover
	// account affinity, but it is not a user window creation event. Forks remain
	// independent because their request_kind is a normal turn and their current
	// root differs from forked_from_thread_id.
	reuseOnlyRequest := rootIdentity.related && !rootIdentity.ownsUserRootBinding() && rootIdentity.stable && !rootIdentity.conflict
	if strings.EqualFold(strings.TrimSpace(rootIdentity.requestKind), "compaction") ||
		requestBodyCompactionMeta(body).ProtocolTriggered ||
		(c.Request != nil && c.Request.URL != nil && isCompactUsageEndpoint(c.Request.URL.Path)) {
		reuseOnlyRequest = true
	}
	sessionID := ""
	if rootIdentity.stable && !rootIdentity.conflict {
		sessionID = strings.TrimSpace(rootIdentity.sessionID)
	}
	// A conflicting native graph must not be collapsed, but it also must not
	// turn the existing exact-session limit into a bypass. Fall back to the
	// legacy stable leaf identity when no trustworthy root was resolved.
	if sessionID == "" && !rootIdentity.authoritative {
		sessionID = ResolveStableExplicitSessionID(c.Request.Header, body)
	}
	if verifiedPerson && sessionID != "" {
		status.Subject = cache.PromptSessionLimitSubject(policyContext.Platform, policyContext.Identity.UserID)
	}
	if status.Subject == "" {
		if keyID := requestAPIKeyID(c); keyID > 0 {
			status.Subject = fmt.Sprintf("api-key:%d", keyID)
		}
	}
	// Strong enforcement requires both a stable conversation identity and a
	// stable authenticated subject. Content/IP fallbacks deliberately do not count.
	if sessionID == "" || status.Subject == "" {
		return status, false
	}
	status.SessionHash = hashRiskIdentity(sessionID)
	now := time.Now()
	expiresAt := now.Add(time.Duration(status.WindowSeconds) * time.Second)
	requestDetail := cache.PromptSessionWindowDetail{}
	if !reuseOnlyRequest {
		requestDetail = promptSessionWindowRequestDetail(c, body, account, cfg)
	}
	h.ensurePromptSessionLimitsLoaded(status.Subject, now)

	h.promptSessionLimitMu.Lock()
	if h.promptSessionLimits == nil {
		h.promptSessionLimits = make(map[string]map[string]time.Time)
	}
	if h.promptSessionWindowDetails == nil {
		h.promptSessionWindowDetails = make(map[string]map[string]cache.PromptSessionWindowDetail)
	}
	if h.promptSessionLastCleanup.IsZero() || now.Sub(h.promptSessionLastCleanup) >= promptSessionLimitCleanupInterval {
		for subject, subjectSessions := range h.promptSessionLimits {
			for key, sessionExpiresAt := range subjectSessions {
				if !sessionExpiresAt.After(now) {
					delete(subjectSessions, key)
					delete(h.promptSessionWindowDetails[subject], key)
				}
			}
			if len(subjectSessions) == 0 {
				delete(h.promptSessionLimits, subject)
				delete(h.promptSessionWindowDetails, subject)
				delete(h.promptSessionLimitsLoaded, subject)
			}
		}
		h.promptSessionLastCleanup = now
	}
	sessions := h.promptSessionLimits[status.Subject]
	if sessions == nil {
		sessions = make(map[string]time.Time)
		h.promptSessionLimits[status.Subject] = sessions
	}
	earliestExpiry := time.Time{}
	for key, sessionExpiresAt := range sessions {
		if !sessionExpiresAt.After(now) {
			delete(sessions, key)
			delete(h.promptSessionWindowDetails[status.Subject], key)
			continue
		}
		if earliestExpiry.IsZero() || sessionExpiresAt.Before(earliestExpiry) {
			earliestExpiry = sessionExpiresAt
		}
	}
	if len(sessions) == 0 {
		delete(h.promptSessionLimits, status.Subject)
		sessions = make(map[string]time.Time)
		h.promptSessionLimits[status.Subject] = sessions
	}
	if _, exists := sessions[status.SessionHash]; exists {
		status.Existing = true
		status.Used = len(sessions)
		detailChanged := false
		details := h.promptSessionWindowDetails[status.Subject]
		if details == nil {
			details = make(map[string]cache.PromptSessionWindowDetail)
			h.promptSessionWindowDetails[status.Subject] = details
		}
		current, found := details[status.SessionHash]
		if !found {
			current.ExpiresAt = sessions[status.SessionHash]
		}
		// AccountID represents the current binding, so it may be refreshed by a
		// reuse-only request. Model, effort, client UA, and prompt describe
		// creation and must never be backfilled from a later request or a legacy
		// v1 cache entry.
		if account != nil && account.ID() > 0 && current.AccountID != account.ID() {
			current.AccountID = account.ID()
			detailChanged = true
		}
		if !found || detailChanged {
			details[status.SessionHash] = current
		}
		h.promptSessionLimitMu.Unlock()
		if detailChanged {
			h.persistPromptSessionLimits(status.Subject, now)
		}
		writePromptSessionLimitHeaders(c, status)
		return status, false
	}
	if reuseOnlyRequest {
		status.Used = len(sessions)
		if len(sessions) == 0 {
			delete(h.promptSessionLimits, status.Subject)
		}
		h.promptSessionLimitMu.Unlock()
		if account != nil && priorSessionAccountID == account.ID() {
			log.Printf("event=window_state_drift 账号会话已复用但用户窗口不存在 subject=%s session_hash=%s account=%d affinity_key_present=%t", status.Subject, status.SessionHash, account.ID(), strings.TrimSpace(affinityKey) != "")
		}
		writePromptSessionLimitHeaders(c, status)
		return status, false
	}
	if len(sessions) >= status.Limit {
		status.Used = len(sessions)
		status.RetryAfter = int(earliestExpiry.Sub(now).Seconds())
		if status.RetryAfter < 1 {
			status.RetryAfter = 1
		}
		h.promptSessionLimitMu.Unlock()
		writePromptSessionLimitHeaders(c, status)
		return status, true
	}
	// Store each session's own expiry instead of its creation time. Different
	// users may have different window durations, so a later request must never
	// clean another user's sessions using the later request's TTL.
	sessions[status.SessionHash] = expiresAt
	requestDetail.CreatedAt = now
	requestDetail.ExpiresAt = expiresAt
	details := h.promptSessionWindowDetails[status.Subject]
	if details == nil {
		details = make(map[string]cache.PromptSessionWindowDetail)
		h.promptSessionWindowDetails[status.Subject] = details
	}
	details[status.SessionHash] = requestDetail
	status.Used = len(sessions)
	h.promptSessionLimitMu.Unlock()
	h.persistPromptSessionLimits(status.Subject, now)
	writePromptSessionLimitHeaders(c, status)
	return status, false
}

func writePromptSessionLimitHeaders(c *gin.Context, status promptSessionCreationLimitStatus) {
	if c == nil || !status.Enabled {
		return
	}
	c.Header("X-Codex2API-Session-Limit", strconv.Itoa(status.Limit))
	c.Header("X-Codex2API-Session-Used", strconv.Itoa(status.Used))
	c.Header("X-Codex2API-Session-Window-Seconds", strconv.Itoa(status.WindowSeconds))
	if status.RetryAfter > 0 {
		c.Header("Retry-After", strconv.Itoa(status.RetryAfter))
	}
}

func sendPromptSessionCreationLimitError(c *gin.Context, status promptSessionCreationLimitStatus) {
	api.SendErrorWithStatus(c, promptSessionCreationLimitAPIError(status), http.StatusBadRequest)
}

func promptSessionCreationLimitMessage(status promptSessionCreationLimitStatus) string {
	if status.IdentityConflict {
		return "会话标识发生冲突，请新建连接后重试"
	}
	return "当前时间内创建窗口已达到上限，请复用已有会话或稍后再试"
}

func promptSessionCreationLimitAPIError(status promptSessionCreationLimitStatus) *api.APIError {
	code := api.ErrorCode("session_creation_limit_exceeded")
	if status.IdentityConflict {
		code = api.ErrorCode("session_identity_conflict")
	}
	return api.NewAPIError(code, promptSessionCreationLimitMessage(status), api.ErrorTypeInvalidRequest)
}
