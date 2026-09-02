package proxy

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	promptConversationLockedReasonCode = "conversation_cyber_locked"
	promptFingerprintReplayReasonCode  = "fingerprint_replay_cooldown"
	promptConversationLockedMessage    = "当前对话因上游 CYB 已被锁定。本次锁定拦截不会重复累计处罚；可等待自动到期，或由管理员在「Prompt 检查 → 风险画像 → 会话详情」手动解锁。解除后再次触发 CYB 可能会停用账号。"
	promptUserCyberCooldownReasonCode  = "user_cyber_cooldown"
	promptUserCyberCooldownMessage     = "该用户因上游 CYB 现处于安全冷却期。冷却期间的新请求不会继续转发，也不会重复累计处罚；可等待自动到期，或由管理员在「Prompt 检查 → 风险画像 → 用户详情」手动解除冷却。"
	promptConversationLockCacheTTL     = 30 * time.Second
)

type promptCyberRestriction struct {
	ReasonCode        string
	Message           string
	Scope             string
	LockedAt          time.Time
	ExpiresAt         time.Time
	RetryAfterSeconds int64
	IncidentID        string
	DecisionID        string
	RequestID         string
	AuditReference    string
	TriggerReasonCode string
}

type promptConversationLockIdentity struct {
	Kind               string
	LockKey            string
	Platform           string
	NewAPIUserID       string
	SessionFingerprint string
	SessionHash        string
}

func verifiedPromptUserCooldownIdentity(c *gin.Context, policyContext verifiedNewAPIPolicyContext) (promptConversationLockIdentity, bool) {
	if c == nil || !policyContext.MetaVerified {
		return promptConversationLockIdentity{}, false
	}
	platform := normalizedNewAPIPlatform(policyContext.Platform)
	userID := strings.TrimSpace(policyContext.Identity.UserID)
	if platform == "" || userID == "" {
		return promptConversationLockIdentity{}, false
	}
	digest := sha256.Sum256([]byte("prompt-user-cooldown-v1\x00" + platform + "\x00" + userID))
	return promptConversationLockIdentity{
		Kind:    database.PromptConversationLockIdentityNewAPI,
		LockKey: hex.EncodeToString(digest[:]), Platform: platform, NewAPIUserID: userID,
	}, true
}

// promptConversationLockFallbackPlatform 标记降级身份来源,避免与真实 NewAPI
// 平台代码混淆。
const promptConversationLockFallbackPlatform = "codex-local"

// promptSessionFingerprint32 产出 32 位十六进制会话指纹,与 NewAPI 透传指纹的
// 长度约定一致,因此降级身份可以复用同一套存储与校验不变量。
func promptSessionFingerprint32(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("prompt-conversation-session-v1\x00" + strings.TrimSpace(value)))
	return hex.EncodeToString(sum[:16])
}

// promptConversationSessionSignal 提取 Codex 客户端自带的稳定会话标识。Codex
// 请求必带 x-codex-* 头,其中 window-id / installation-id 与 session-id 头一起
// 构成不依赖 NewAPI 的会话身份。
func promptConversationSessionSignal(c *gin.Context) string {
	if c == nil {
		return ""
	}
	if value := promptSessionID(c); value != "" {
		return value
	}
	body := ingressRequestBody(c, nil)
	for _, path := range []string{"client_metadata.x-codex-window-id", "client_metadata.x-codex-installation-id"} {
		if value := strings.TrimSpace(gjson.GetBytes(body, path).String()); value != "" {
			return value
		}
	}
	for _, name := range []string{"X-Codex-Window-Id", "X-Codex-Installation-Id", "Thread-Id"} {
		if value := strings.TrimSpace(c.GetHeader(name)); value != "" {
			return value
		}
	}
	return ""
}

// promptConversationLockFallbackSessionHash 统一 codex-local 降级身份的 session
// hash 派生。锁表和审计日志必须使用同一条信号提取与指纹链路，后台才能用审计
// 记录里的 session_hash 定位并解锁对应会话。
func promptConversationLockFallbackSessionHash(c *gin.Context) string {
	return hashRiskIdentity(promptSessionFingerprint32(promptConversationSessionSignal(c)))
}

func verifiedPromptConversationLockIdentity(c *gin.Context, policyContext verifiedNewAPIPolicyContext) (promptConversationLockIdentity, bool) {
	if c == nil || !policyContext.MetaVerified {
		return promptConversationLockIdentity{}, false
	}
	platform := normalizedNewAPIPlatform(policyContext.Platform)
	userID := strings.TrimSpace(policyContext.Identity.UserID)
	fingerprint := strings.ToLower(strings.TrimSpace(policyContext.Meta.SessionFingerprint))
	if platform == "" || userID == "" || len(fingerprint) != 32 {
		return promptConversationLockIdentity{}, false
	}
	lockMaterial := "prompt-conversation-lock-v1\x00" + platform + "\x00" + userID + "\x00" + fingerprint
	if policyContext.Meta.ChannelID > 0 {
		lockMaterial = "prompt-conversation-lock-v2\x00" + platform + "\x00" + userID + "\x00" + strconv.Itoa(policyContext.Meta.ChannelID) + "\x00" + fingerprint
	}
	digest := sha256.Sum256([]byte(lockMaterial))
	return promptConversationLockIdentity{
		Kind:    database.PromptConversationLockIdentityNewAPI,
		LockKey: hex.EncodeToString(digest[:]), Platform: platform, NewAPIUserID: userID,
		SessionFingerprint: fingerprint, SessionHash: hashRiskIdentity(fingerprint),
	}, true
}

func promptConversationLockTTL(cfg promptfilter.Config) time.Duration {
	hours := promptfilter.NormalizeAdvancedConfig(cfg.Advanced).Enforcement.ConversationLockTTLHours
	return time.Duration(hours) * time.Hour
}

func promptUserCyberCooldownTTL(cfg promptfilter.Config) time.Duration {
	minutes := promptfilter.NormalizeAdvancedConfig(cfg.Advanced).Enforcement.UserCyberCooldownMinutes
	return time.Duration(minutes) * time.Minute
}

func promptConversationLockExpired(item *database.PromptConversationLock, ttl time.Duration) bool {
	return item == nil || (ttl > 0 && !item.LockedAt.After(time.Now().UTC().Add(-ttl)))
}

func promptCyberRestrictionDecision(item *database.PromptConversationLock, cfg promptfilter.Config) promptCyberRestriction {
	result := promptCyberRestriction{
		ReasonCode: promptConversationLockedReasonCode,
		Message:    promptConversationLockedMessage,
		Scope:      database.PromptConversationRestrictionScopeConversation,
	}
	ttl := promptConversationLockTTL(cfg)
	if item != nil {
		result.LockedAt = item.LockedAt.UTC()
		result.IncidentID = strings.TrimSpace(item.IncidentID)
		result.DecisionID = strings.TrimSpace(item.DecisionID)
		result.RequestID = strings.TrimSpace(item.RequestID)
		result.TriggerReasonCode = strings.TrimSpace(item.ReasonCode)
		result.AuditReference = result.IncidentID
		if result.AuditReference == "" {
			result.AuditReference = result.RequestID
		}
		// 旧版本的本地锁没有单独保存 request_id，但 decision_id 一直使用
		// local-block:<request_correlation_id>。兼容推导后，历史锁也能直接
		// 粘贴定位到原始审核日志，而不需要等待新数据产生。
		if result.AuditReference == "" && strings.HasPrefix(result.DecisionID, "local-block:") {
			result.AuditReference = strings.TrimSpace(strings.TrimPrefix(result.DecisionID, "local-block:"))
		}
		if item.ReasonCode == promptUserCyberCooldownReasonCode {
			result.ReasonCode = promptUserCyberCooldownReasonCode
			result.Scope = database.PromptConversationRestrictionScopeUserCooldown
			ttl = promptUserCyberCooldownTTL(cfg)
		} else if item.IdentityKind == database.PromptConversationLockIdentityFingerprintReplay || item.ReasonCode == promptFingerprintReplayReasonCode {
			result.ReasonCode = promptFingerprintReplayReasonCode
			result.Scope = database.PromptConversationRestrictionScopeFingerprintReplay
			ttl = promptUserCyberCooldownTTL(cfg)
		}
	}
	if !result.LockedAt.IsZero() && ttl > 0 {
		result.ExpiresAt = result.LockedAt.Add(ttl)
		remaining := time.Until(result.ExpiresAt)
		if remaining > 0 {
			result.RetryAfterSeconds = int64((remaining + time.Second - 1) / time.Second)
		}
	}
	remainingText := promptCyberRestrictionRemainingText(result.RetryAfterSeconds)
	auditText := ""
	if result.AuditReference != "" {
		auditText = fmt.Sprintf("审计编号：%s；", result.AuditReference)
	}
	if result.Scope == database.PromptConversationRestrictionScopeUserCooldown {
		result.Message = fmt.Sprintf("该用户因上游 CYB 进入安全冷却，剩余约 %s；冷却期间所有新请求均不会转发，也不会重复累计处罚。%s管理员可在「Prompt 检查 → 风险画像 → 用户详情」解除冷却。错误码：%s。", remainingText, auditText, result.ReasonCode)
	} else if result.Scope == database.PromptConversationRestrictionScopeFingerprintReplay {
		result.Message = fmt.Sprintf("相同 Prompt 指纹近期已收到上游 CYB，当前请求进入精确重放冷却，剩余约 %s；不会封禁整个 API Key。%s管理员可在「Prompt 检查 → 风险画像 → 会话详情」手动解锁。错误码：%s。", remainingText, auditText, result.ReasonCode)
	} else if result.TriggerReasonCode != "" && result.TriggerReasonCode != newAPIUpstreamCyberPolicyReasonCode {
		result.Message = fmt.Sprintf("当前对话因本地高风险规则已锁定，剩余约 %s；后续请求不会转发或重复累计处罚。触发原因：%s；%s管理员可在「Prompt 检查 → 风险画像 → 会话详情」审核并手动解锁。错误码：%s。", remainingText, result.TriggerReasonCode, auditText, result.ReasonCode)
	} else {
		result.Message = fmt.Sprintf("当前对话因上游 CYB 已锁定，剩余约 %s；后续请求不会转发或重复累计处罚。%s管理员可在「Prompt 检查 → 风险画像 → 会话详情」手动解锁。错误码：%s。", remainingText, auditText, result.ReasonCode)
	}
	return result
}

func promptCyberRestrictionRemainingText(seconds int64) string {
	if seconds <= 0 {
		return "不到 1 分钟"
	}
	minutes := (seconds + 59) / 60
	if minutes < 60 {
		return fmt.Sprintf("%d 分钟", minutes)
	}
	hours := minutes / 60
	remainingMinutes := minutes % 60
	if remainingMinutes == 0 {
		return fmt.Sprintf("%d 小时", hours)
	}
	return fmt.Sprintf("%d 小时 %d 分钟", hours, remainingMinutes)
}

func promptCyberRestrictionDetails(restriction promptCyberRestriction, signedDetails gin.H) gin.H {
	details := gin.H{}
	for key, value := range signedDetails {
		details[key] = value
	}
	details["reason_code"] = restriction.ReasonCode
	details["restriction_scope"] = restriction.Scope
	details["retry_after_seconds"] = restriction.RetryAfterSeconds
	details["manual_unlock_path"] = "/admin/prompt-filter/profiles"
	if !restriction.LockedAt.IsZero() {
		details["locked_at"] = restriction.LockedAt.Format(time.RFC3339)
	}
	if !restriction.ExpiresAt.IsZero() {
		details["expires_at"] = restriction.ExpiresAt.Format(time.RFC3339)
	}
	if restriction.IncidentID != "" {
		details["incident_id"] = restriction.IncidentID
	}
	if restriction.DecisionID != "" {
		details["decision_id"] = restriction.DecisionID
	}
	if restriction.RequestID != "" {
		details["request_id"] = restriction.RequestID
	}
	if restriction.AuditReference != "" {
		details["audit_reference"] = restriction.AuditReference
	}
	if restriction.TriggerReasonCode != "" {
		details["trigger_reason_code"] = restriction.TriggerReasonCode
	}
	return details
}

func promptCyberRestrictionAPIError(restriction promptCyberRestriction, signedDetails gin.H) *api.APIError {
	return api.NewAPIErrorWithDetails(
		api.ErrorCode(restriction.ReasonCode), restriction.Message,
		api.ErrorTypeInvalidRequest, promptCyberRestrictionDetails(restriction, signedDetails),
	)
}

func writePromptCyberRestrictionHeaders(c *gin.Context, restriction promptCyberRestriction) {
	if c == nil {
		return
	}
	c.Header("X-Codex2API-Policy-Restriction-Scope", restriction.Scope)
	if restriction.RetryAfterSeconds > 0 {
		c.Header("Retry-After", strconv.FormatInt(restriction.RetryAfterSeconds, 10))
	}
	if !restriction.ExpiresAt.IsZero() {
		c.Header("X-Codex2API-Policy-Restriction-Expires-At", restriction.ExpiresAt.Format(time.RFC3339))
	}
}

func (h *Handler) sendNewAPIPromptCyberRestriction(c *gin.Context, cfg promptfilter.Config, decision promptfilter.Decision, verdict promptfilter.Verdict, body []byte, endpoint, model string, signedBody []byte, restriction promptCyberRestriction) bool {
	policyContext, verified := h.verifyNewAPIPolicyContext(c, cfg.Advanced.NewAPI, signedBody)
	if !verified {
		return false
	}
	metadata := buildNewAPIPolicyDecisionMetadataWithSecret(policyContext.Identity, decision, verdict, cfg, body, endpoint, model, "", policyContext.VerificationSecret)
	writeNewAPIPolicyDecisionHeaders(c, metadata)
	writePromptCyberRestrictionHeaders(c, restriction)
	apiErr := promptCyberRestrictionAPIError(restriction, newAPIPolicyDecisionDetails(metadata))
	if requestUsesAnthropicErrorEnvelope(c) {
		c.JSON(http.StatusBadRequest, gin.H{"type": "error", "error": gin.H{
			"type": string(apiErr.Type), "message": apiErr.Message, "details": apiErr.Details,
		}})
		return true
	}
	api.SendErrorWithStatus(c, apiErr, http.StatusBadRequest)
	return true
}

func promptCyberRestrictionLock(item *database.PromptConversationLock, exactConversation bool) *database.PromptConversationLock {
	if item == nil {
		return nil
	}
	copy := *item
	if exactConversation {
		if item.IdentityKind == database.PromptConversationLockIdentityFingerprintReplay {
			copy.ReasonCode = promptFingerprintReplayReasonCode
		} else {
			copy.ReasonCode = promptConversationLockedReasonCode
		}
	} else {
		copy.ReasonCode = promptUserCyberCooldownReasonCode
	}
	return &copy
}

// promptConversationLockFallbackIdentity 在没有 NewAPI 透传签名时,用下游
// API Key 加 Codex 自带会话标识构成锁定身份。没有这条降级路径,未接 NewAPI 的
// 部署完全无法锁定会话,拦截就只能逐条命中,无法封死一个正在试探的会话。
func promptConversationLockFallbackIdentity(c *gin.Context) (promptConversationLockIdentity, bool) {
	if c == nil {
		return promptConversationLockIdentity{}, false
	}
	apiKeyID := requestAPIKeyID(c)
	fingerprint := promptSessionFingerprint32(promptConversationSessionSignal(c))
	if apiKeyID == 0 || len(fingerprint) != 32 {
		return promptConversationLockIdentity{}, false
	}
	subject := fmt.Sprintf("apikey:%d", apiKeyID)
	digest := sha256.Sum256([]byte("prompt-conversation-lock-v1\x00" +
		database.PromptConversationLockIdentityCodexSession + "\x00" + subject + "\x00" + fingerprint))
	return promptConversationLockIdentity{
		Kind:               database.PromptConversationLockIdentityCodexSession,
		LockKey:            hex.EncodeToString(digest[:]),
		Platform:           promptConversationLockFallbackPlatform,
		NewAPIUserID:       subject,
		SessionFingerprint: fingerprint,
		SessionHash:        promptConversationLockFallbackSessionHash(c),
	}, true
}

// promptConversationFingerprintReplayIdentity is the last-resort identity for
// unsigned requests that have neither a verified NewAPI user nor a stable
// Codex session signal. It requires the client IP hash and scopes the lock to
// the API key, so a shared key cannot be globally blocked.
func (h *Handler) promptConversationFingerprintReplayIdentity(c *gin.Context, signedBody []byte, endpoint, model string) (promptConversationLockIdentity, bool) {
	if h == nil || c == nil {
		return promptConversationLockIdentity{}, false
	}
	apiKeyID := requestAPIKeyID(c)
	if apiKeyID == 0 {
		return promptConversationLockIdentity{}, false
	}
	audit := h.capturePromptFilterAuditContext(c)
	clientIPHash := strings.TrimSpace(audit.ClientIPHash)
	if clientIPHash == "" {
		return promptConversationLockIdentity{}, false
	}
	fingerprint := h.promptConversationReplayFingerprint(c, signedBody, endpoint, model)
	if len(fingerprint) != sha256.Size*2 {
		return promptConversationLockIdentity{}, false
	}
	subject := fmt.Sprintf("apikey:%d:ip:%s", apiKeyID, clientIPHash)
	scope := strings.Join([]string{subject, fingerprint}, "\x00")
	return promptConversationLockIdentity{
		Kind:               database.PromptConversationLockIdentityFingerprintReplay,
		LockKey:            promptfilter.StableEvidenceFingerprint("prompt-fingerprint-replay-lock", scope),
		Platform:           "codex-fingerprint",
		NewAPIUserID:       subject,
		SessionFingerprint: fingerprint[:32],
		SessionHash:        fingerprint,
	}, true
}

func (h *Handler) promptConversationReplayFingerprint(c *gin.Context, signedBody []byte, endpoint, model string) string {
	if c == nil || h == nil {
		return ""
	}
	if _, exists := c.Get("raw_body"); !exists {
		if body := ingressRequestBody(c, signedBody); len(body) > 0 {
			setRawRequestBody(c, body)
		}
	}
	body := ingressRequestBody(c, signedBody)
	if len(body) == 0 {
		if raw, exists := c.Get("raw_body"); exists {
			body, _ = raw.([]byte)
		}
	}
	if strings.TrimSpace(endpoint) == "" {
		endpoint = "/v1/responses"
	}
	if strings.TrimSpace(model) == "" {
		model = strings.TrimSpace(gjson.GetBytes(body, "model").String())
	}
	captured, ok := h.promptRuleLearningEvidenceForUpstreamFailure(c, endpoint, model)
	if !ok {
		return ""
	}
	learningSourceText := strings.TrimSpace(envelopeDirectCurrentUserText(captured.Envelope))
	if learningSourceText == "" && captured.PrimaryOrigin == string(promptfilter.OriginApplicationCandidate) {
		learningSourceText = strings.TrimSpace(captured.Text)
	}
	if learningSourceText == "" && len(captured.Envelope.Segments) == 0 {
		learningSourceText = strings.TrimSpace(captured.Text)
	}
	if learningSourceText == "" {
		_, learningSourceText = promptPolicyLearningContext(captured.Envelope)
	}
	return promptfilter.PromptEvidenceFingerprint(learningSourceText)
}

func (h *Handler) unsignedUpstreamCyberLockIdentity(c *gin.Context, signedBody []byte, endpoint, model string) (promptConversationLockIdentity, string, bool) {
	if identity, ok := promptConversationLockFallbackIdentity(c); ok {
		return identity, newAPIUpstreamCyberPolicyReasonCode, true
	}
	if identity, ok := h.promptConversationFingerprintReplayIdentity(c, signedBody, endpoint, model); ok {
		return identity, promptFingerprintReplayReasonCode, true
	}
	return promptConversationLockIdentity{}, "", false
}

// resolvePromptConversationLockIdentity 优先使用已验证的 NewAPI 身份,缺失时
// 退化到 Codex 会话身份。NewAPI 场景的行为与升级前一致。
func (h *Handler) resolvePromptConversationLockIdentity(c *gin.Context, cfg promptfilter.Config, signedBody []byte) (promptConversationLockIdentity, bool) {
	if h == nil || c == nil {
		return promptConversationLockIdentity{}, false
	}
	if policyContext, verified := h.verifyNewAPIPolicyContext(c, cfg.Advanced.NewAPI, signedBody); verified {
		if identity, ok := verifiedPromptConversationLockIdentity(c, policyContext); ok {
			return identity, true
		}
	}
	return promptConversationLockFallbackIdentity(c)
}

// 指纹重放冷却的存在性闸门:回退身份需要构建完整请求 envelope(全量 body
// 遍历与脱敏),而绝大多数时间全局根本没有活动的指纹冷却。以短 TTL 缓存
// "是否存在活动指纹锁"的结论,把每请求的 envelope 构建换成至多每 10 秒一次
// 的轻量存在性查询。本实例新建指纹锁时立即置位;跨实例的新锁最多延迟一个
// 闸门 TTL 才可见,对 best-effort 冷却可接受。
const promptFingerprintReplayGateTTL = 10 * time.Second

func (h *Handler) hasActiveFingerprintReplayLocks(ctx context.Context, cooldownTTL time.Duration) bool {
	if h == nil || h.db == nil {
		return false
	}
	now := time.Now()
	h.fpReplayGateMu.Lock()
	if !h.fpReplayGateAt.IsZero() && now.Sub(h.fpReplayGateAt) < promptFingerprintReplayGateTTL {
		active := h.fpReplayGateActive
		h.fpReplayGateMu.Unlock()
		return active
	}
	h.fpReplayGateMu.Unlock()
	checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	active, err := h.db.HasActivePromptFingerprintReplayLocks(checkCtx, cooldownTTL)
	if err != nil {
		// 查询失败不缓存结论,并按"可能有锁"放行后续检查:冷却语义优先于省 CPU。
		return true
	}
	h.fpReplayGateMu.Lock()
	h.fpReplayGateAt = now
	h.fpReplayGateActive = active
	h.fpReplayGateMu.Unlock()
	return active
}

func (h *Handler) markFingerprintReplayLockCreated() {
	if h == nil {
		return
	}
	h.fpReplayGateMu.Lock()
	h.fpReplayGateAt = time.Now()
	h.fpReplayGateActive = true
	h.fpReplayGateMu.Unlock()
}

func (h *Handler) activePromptConversationLock(c *gin.Context, cfg promptfilter.Config, signedBody []byte, endpoint, model string) (*database.PromptConversationLock, bool) {
	if h == nil || h.db == nil || c == nil || !cfg.Advanced.Enforcement.ConversationLockEnabled {
		return nil, false
	}
	lockTTL := promptConversationLockTTL(cfg)
	policyContext, verified := h.verifyNewAPIPolicyContext(c, cfg.Advanced.NewAPI, signedBody)
	if verified {
		userIdentity, ok := verifiedPromptUserCooldownIdentity(c, policyContext)
		if !ok {
			return nil, false
		}
		conversationIdentity, hasConversationIdentity := verifiedPromptConversationLockIdentity(c, policyContext)
		lockKey := ""
		if hasConversationIdentity {
			lockKey = conversationIdentity.LockKey
		}
		if hasConversationIdentity && h.cache != nil {
			if raw, found, err := h.cache.GetRuntime(c.Request.Context(), database.PromptConversationLockCacheNamespace, conversationIdentity.LockKey); err == nil && found {
				var item database.PromptConversationLock
				if json.Unmarshal(raw, &item) == nil && item.Status == database.PromptConversationLockStatusActive && !promptConversationLockExpired(&item, lockTTL) {
					return promptCyberRestrictionLock(&item, true), true
				}
				_ = h.cache.DeleteRuntime(c.Request.Context(), database.PromptConversationLockCacheNamespace, conversationIdentity.LockKey)
			}
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()
		item, exactConversation, err := h.db.GetActivePromptConversationRestriction(
			ctx, lockKey, userIdentity.Platform, userIdentity.NewAPIUserID,
			lockTTL, promptUserCyberCooldownTTL(cfg),
		)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false
		}
		if err != nil {
			log.Printf("check prompt conversation restriction failed identity=%s: %v", userIdentity.LockKey[:12], err)
			return nil, false
		}
		if exactConversation {
			h.cachePromptConversationLock(c.Request.Context(), item, lockTTL)
		}
		return promptCyberRestrictionLock(item, exactConversation), true
	}

	// 未接入签名 NewAPI 的部署只按 API Key + Codex 会话锁定当前会话；不应用
	// 用户级冷却，避免共享 Key 下不同用户相互影响。
	identity, ok := promptConversationLockFallbackIdentity(c)
	if !ok && h.hasActiveFingerprintReplayLocks(c.Request.Context(), promptUserCyberCooldownTTL(cfg)) {
		identity, ok = h.promptConversationFingerprintReplayIdentity(c, signedBody, endpoint, model)
	}
	if !ok {
		return nil, false
	}
	effectiveTTL := lockTTL
	if identity.Kind == database.PromptConversationLockIdentityFingerprintReplay {
		effectiveTTL = promptUserCyberCooldownTTL(cfg)
	}
	if h.cache != nil {
		if raw, found, err := h.cache.GetRuntime(c.Request.Context(), database.PromptConversationLockCacheNamespace, identity.LockKey); err == nil && found {
			var item database.PromptConversationLock
			if json.Unmarshal(raw, &item) == nil && item.Status == database.PromptConversationLockStatusActive && !promptConversationLockExpired(&item, effectiveTTL) {
				return promptCyberRestrictionLock(&item, true), true
			}
			_ = h.cache.DeleteRuntime(c.Request.Context(), database.PromptConversationLockCacheNamespace, identity.LockKey)
		}
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
	defer cancel()
	item, err := h.db.GetActivePromptConversationLockWithTTL(ctx, identity.LockKey, effectiveTTL)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false
	}
	if err != nil {
		log.Printf("check fallback prompt conversation lock failed identity=%s: %v", identity.LockKey[:12], err)
		return nil, false
	}
	h.cachePromptConversationLock(c.Request.Context(), item, effectiveTTL)
	return promptCyberRestrictionLock(item, true), true
}

func (h *Handler) cachePromptConversationLock(ctx context.Context, item *database.PromptConversationLock, lockTTL time.Duration) {
	if h == nil || h.cache == nil || item == nil || item.Status != database.PromptConversationLockStatusActive {
		return
	}
	cacheTTL := promptConversationLockCacheTTL
	if lockTTL > 0 {
		remaining := time.Until(item.LockedAt.Add(lockTTL))
		if remaining <= 0 {
			return
		}
		if remaining < cacheTTL {
			cacheTTL = remaining
		}
	}
	raw, err := json.Marshal(item)
	if err == nil {
		_ = h.cache.SetRuntime(ctx, database.PromptConversationLockCacheNamespace, item.LockKey, raw, cacheTTL)
	}
}

func (h *Handler) lockPromptConversationAfterUpstreamCYB(c *gin.Context, endpoint, model, incidentID string, metadata newAPIPolicyDecisionMetadata) bool {
	if h == nil || h.db == nil || c == nil || metadata.ReasonCode != newAPIUpstreamCyberPolicyReasonCode || metadata.DecisionID == "" {
		return false
	}
	cfg := h.promptFilterConfigForRequest(c)
	if !cfg.Advanced.Enforcement.ConversationLockEnabled {
		return false
	}
	policyContext, verified := h.verifyNewAPIPolicyContext(c, cfg.Advanced.NewAPI, ingressRequestBody(c, nil))
	identity := promptConversationLockIdentity{}
	exactConversation := false
	if verified {
		userIdentity, ok := verifiedPromptUserCooldownIdentity(c, policyContext)
		if !ok {
			return false
		}
		identity = userIdentity
		if conversationIdentity, ok := verifiedPromptConversationLockIdentity(c, policyContext); ok {
			identity = conversationIdentity
			exactConversation = true
		}
	} else {
		var ok bool
		identity, ok = promptConversationLockFallbackIdentity(c)
		if !ok {
			return false
		}
		exactConversation = true
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	item, _, err := h.db.LockPromptConversation(ctx, database.PromptConversationLockInput{
		LockKey: identity.LockKey, IdentityKind: identity.Kind,
		Platform: identity.Platform, NewAPIUserID: identity.NewAPIUserID,
		SessionFingerprint: identity.SessionFingerprint, SessionHash: identity.SessionHash,
		IncidentID: incidentID, DecisionID: metadata.DecisionID, RequestID: metadata.RequestID,
		ReasonCode: metadata.ReasonCode, Endpoint: endpoint, Model: model, LockedAt: time.Now().UTC(),
	})
	if err != nil {
		log.Printf("persist prompt conversation lock failed decision=%s: %v", metadata.DecisionID, err)
		return false
	}
	if !exactConversation {
		return false
	}
	h.cachePromptConversationLock(c.Request.Context(), item, promptConversationLockTTL(cfg))
	return item != nil && item.Status == database.PromptConversationLockStatusActive
}

// lockPromptConversationAfterUnsignedUpstreamCYB applies only a scoped
// session/fingerprint replay lock. It never emits a NewAPI strike because the
// request has no verified identity. A stable Codex session gets conversation
// scope; otherwise the exact prompt fingerprint is paired with API key + IP.
func (h *Handler) lockPromptConversationAfterUnsignedUpstreamCYB(c *gin.Context, endpoint, model, incidentID string) bool {
	if h == nil || h.db == nil || c == nil {
		return false
	}
	cfg := h.promptFilterConfigForRequest(c)
	if !cfg.Advanced.Enforcement.ConversationLockEnabled {
		return false
	}
	signedBody := ingressRequestBody(c, nil)
	identity, reasonCode, ok := h.unsignedUpstreamCyberLockIdentity(c, signedBody, endpoint, model)
	if !ok {
		return false
	}
	requestID := ensurePromptPolicyRequestCorrelationID(c)
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	item, _, err := h.db.LockPromptConversation(ctx, database.PromptConversationLockInput{
		LockKey: identity.LockKey, IdentityKind: identity.Kind,
		Platform: identity.Platform, NewAPIUserID: identity.NewAPIUserID,
		SessionFingerprint: identity.SessionFingerprint, SessionHash: identity.SessionHash,
		IncidentID: incidentID, DecisionID: "upstream-cy:" + requestID, RequestID: requestID,
		ReasonCode: reasonCode, Endpoint: endpoint, Model: model, LockedAt: time.Now().UTC(),
	})
	if err != nil {
		log.Printf("persist unsigned upstream CY replay lock failed request=%s: %v", requestID, err)
		return false
	}
	lockTTL := promptConversationLockTTL(cfg)
	if identity.Kind == database.PromptConversationLockIdentityFingerprintReplay {
		lockTTL = promptUserCyberCooldownTTL(cfg)
		h.markFingerprintReplayLockCreated()
	}
	h.cachePromptConversationLock(c.Request.Context(), item, lockTTL)
	return item != nil && item.Status == database.PromptConversationLockStatusActive
}

// promptGuardBlockHasLocalEvidence 判断最终 block 是否有本地检测证据(规则命中/
// 终局类别/分类器信号)支撑。仅由外部审核层产生的 block——审核模型对本地放行的
// 请求打了 flag,或 fail-closed 下审核调用本身失败——只拒绝当次请求,不触发会话
// 锁:概率型审核分数(如 moderation 各分类阈值)对正常会话存在误报,
// finalizePromptGuardDecision 也早已把这类命中归为不计违规笔数的弱证据。
// 一次误报锁死整段会话七天,代价与证据强度不匹配(issue #527)。
func promptGuardBlockHasLocalEvidence(decision promptfilter.Decision, verdict promptfilter.Verdict) bool {
	if decision.PrimaryDetector == promptGuardDetectorExternalReview {
		return false
	}
	if verdict.Reviewed && verdict.ReviewError != "" && len(verdict.Matched) == 0 &&
		!verdict.TerminalStrictHit && !verdict.TerminalCategoryHit {
		return false
	}
	return true
}

// lockPromptConversationOnLocalBlock 在本地规则判定 block 时立即锁定会话,
// 使风险在**发往上游供应商之前**就被扼杀。
//
// 为什么必须前置:纯正则单轮命中率有限(见 promptfilter 对抗性基线,12 种绕过
// 变形仅 3 种被拦),攻击者只要改写措辞、拆词、换语言就能让下一条请求穿透本地
// 规则打到上游,从而产生真实的 cyber_policy 封号信号。等上游返回 CYB 再锁,
// 风险已经泄露。会话级封锁把"逐条命中"变成"一次命中即封死整段会话",这是纵深
// 防御里唯一能覆盖未知变形的一层。
func (h *Handler) lockPromptConversationOnLocalBlock(c *gin.Context, cfg promptfilter.Config, signedBody []byte, endpoint, model string, decision promptfilter.Decision, verdict promptfilter.Verdict) bool {
	if h == nil || h.db == nil || c == nil || !cfg.Advanced.Enforcement.ConversationLockEnabled {
		return false
	}
	if !promptGuardBlockHasLocalEvidence(decision, verdict) {
		return false
	}
	identity, ok := h.resolvePromptConversationLockIdentity(c, cfg, signedBody)
	if !ok {
		return false
	}
	reasonCode := strings.TrimSpace(decision.ReasonCode)
	if reasonCode == "" {
		reasonCode = "local_prompt_block"
	}
	// 每次本地 block 使用独立的决策 ID,使 trigger_count 如实反映命中次数;
	// 同一请求重放则保持幂等。
	decisionID := "local-block:" + ensurePromptPolicyRequestCorrelationID(c)
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()
	item, _, err := h.db.LockPromptConversation(ctx, database.PromptConversationLockInput{
		LockKey: identity.LockKey, IdentityKind: identity.Kind,
		Platform: identity.Platform, NewAPIUserID: identity.NewAPIUserID,
		SessionFingerprint: identity.SessionFingerprint, SessionHash: identity.SessionHash,
		DecisionID: decisionID, ReasonCode: reasonCode,
		Endpoint: endpoint, Model: model, LockedAt: time.Now().UTC(),
	})
	if err != nil {
		log.Printf("persist local-block prompt conversation lock failed decision=%s: %v", decisionID, err)
		return false
	}
	h.cachePromptConversationLock(c.Request.Context(), item, promptConversationLockTTL(cfg))
	return item != nil && item.Status == database.PromptConversationLockStatusActive
}

func (h *Handler) rejectLockedPromptConversation(c *gin.Context, cfg promptfilter.Config, signedBody, responseBody []byte, endpoint, model string) bool {
	item, locked := h.activePromptConversationLock(c, cfg, signedBody, endpoint, model)
	if !locked {
		return false
	}
	restriction := promptCyberRestrictionDecision(item, cfg)
	profile := strings.ToLower(strings.TrimSpace(cfg.Advanced.Guard.DefaultProfile))
	switch profile {
	case promptfilter.GuardProfileBalanced, promptfilter.GuardProfileStrict, promptfilter.GuardProfileResearch:
	default:
		profile = promptfilter.GuardProfileBalanced
	}
	decision := promptfilter.Decision{
		Action: promptfilter.ActionBlock, Profile: profile,
		ReasonCode: restriction.ReasonCode, StrikeEligible: false, Terminal: true,
	}
	verdict := promptfilter.Verdict{Action: promptfilter.ActionBlock, Reason: restriction.Message, FullText: restriction.ReasonCode}
	if h.sendNewAPIPromptCyberRestriction(c, cfg, decision, verdict, responseBody, endpoint, model, signedBody, restriction) {
		return true
	}
	writePromptCyberRestrictionHeaders(c, restriction)
	apiErr := promptCyberRestrictionAPIError(restriction, nil)
	if requestUsesAnthropicErrorEnvelope(c) {
		c.JSON(http.StatusBadRequest, gin.H{"type": "error", "error": gin.H{
			"type": string(apiErr.Type), "message": apiErr.Message, "details": apiErr.Details,
		}})
		return true
	}
	api.SendErrorWithStatus(c, apiErr, http.StatusBadRequest)
	return true
}
