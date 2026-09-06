package admin

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/internal/openaiidentity"
)

func antigravityPersistedStatus(row *database.AccountRow) (string, string) {
	if row == nil {
		return "error", "账号不存在"
	}
	if syncErr := strings.TrimSpace(row.GetCredential("antigravity_sync_error")); syncErr != "" {
		return "error", syncErr
	}
	if raw := strings.TrimSpace(row.GetCredential("antigravity_quota")); raw != "" {
		var quota auth.AntigravityQuotaSnapshot
		if json.Unmarshal([]byte(raw), &quota) == nil && quota.Forbidden {
			return "error", "Google quota API denied access"
		}
	}
	rawPermissions := strings.TrimSpace(row.GetCredential("antigravity_permissions"))
	if rawPermissions == "" {
		rawPermissions = strings.TrimSpace(row.GetCredential("antigravity_entitlements"))
	}
	if raw := rawPermissions; raw != "" {
		var permissions auth.AntigravityEntitlements
		if json.Unmarshal([]byte(raw), &permissions) == nil && !permissions.Allowed &&
			(permissions.Reason != "" || !permissions.UpdatedAt.IsZero()) {
			reason := strings.TrimSpace(permissions.Reason)
			if reason == "" {
				reason = "Google account is not allowed to use Antigravity"
			}
			return "error", reason
		}
	}
	status := strings.TrimSpace(row.Status)
	if status == "" {
		status = "active"
	}
	return status, row.ErrorMessage
}

// buildAccountResponse enriches one database row with its in-memory scheduler
// state and the already-scoped request/usage aggregates. Keeping this logic in
// one place ensures the paged list and the on-demand detail endpoint expose the
// same account shape without re-querying the whole pool.
func (h *Handler) buildAccountResponse(
	row *database.AccountRow,
	runtimeAccount *auth.Account,
	requestCount *database.AccountRequestCount,
	usage5h *database.AccountTimeRangeUsage,
	usage7d *database.AccountTimeRangeUsage,
	includeDetails bool,
) accountResponse {
	upstreamType := strings.TrimSpace(row.GetCredential("upstream_type"))
	isOpenAIResponsesAccount := strings.EqualFold(upstreamType, auth.UpstreamOpenAIResponses)
	isGrokAccount := strings.EqualFold(upstreamType, auth.UpstreamGrok)
	isAntigravityAccount := strings.EqualFold(upstreamType, auth.UpstreamAntigravity)
	isClaudeAccount := strings.EqualFold(upstreamType, auth.UpstreamClaude)
	antigravityAuthKind := ""
	if isAntigravityAccount {
		if strings.TrimSpace(row.GetCredential("api_key")) != "" {
			antigravityAuthKind = auth.AntigravityAuthKindAPIKey
		} else {
			antigravityAuthKind = auth.AntigravityAuthKindOAuth
		}
	}
	grokAuthKind := ""
	var grokBilling json.RawMessage
	var antigravityQuota json.RawMessage
	var antigravityPermissions json.RawMessage
	if isGrokAccount {
		if strings.TrimSpace(row.GetCredential("api_key")) != "" {
			grokAuthKind = auth.GrokAuthKindAPIKey
		} else {
			grokAuthKind = auth.GrokAuthKindOAuth
		}
		// Quota bars on the paged Grok list need this compact credential
		// (issue #521). It is a small JSON projection, not the expensive
		// control-plane / cooldown / mapping payload gated by includeDetails.
		if detail := strings.TrimSpace(row.GetCredential("grok_billing_detail")); detail != "" && json.Valid([]byte(detail)) {
			grokBilling = json.RawMessage(detail)
		}
	}
	if isAntigravityAccount {
		antigravityQuota = antigravityPublishedQuotaJSON(row.GetCredential("antigravity_quota"))
		if raw := strings.TrimSpace(row.GetCredential("antigravity_permissions")); raw != "" && json.Valid([]byte(raw)) {
			antigravityPermissions = json.RawMessage(raw)
		} else if raw := strings.TrimSpace(row.GetCredential("antigravity_entitlements")); raw != "" && json.Valid([]byte(raw)) {
			antigravityPermissions = json.RawMessage(raw)
		}
	}
	email := row.GetCredential("email")
	baseURL := row.GetCredential("base_url")
	if isOpenAIResponsesAccount && email == "" {
		email = baseURL
	}
	planType := row.GetCredential("plan_type")
	if isOpenAIResponsesAccount && planType == "" {
		planType = "api"
	}
	if isGrokAccount && grokAuthKind == auth.GrokAuthKindAPIKey {
		planType = "api"
	}
	if isGrokAccount && runtimeAccount != nil {
		if runtimePlan := runtimeAccount.GetPlanType(); runtimePlan != "" {
			planType = runtimePlan
		}
	}
	var grokPlan *auth.GrokPlan
	if isGrokAccount {
		if resolved, ok := auth.ResolveGrokPlan(planType); ok {
			grokPlan = &resolved
		}
	}
	codexClientMetadataMode := ""
	if isOpenAIResponsesAccount && includeDetails {
		codexClientMetadataMode = auth.NormalizeCodexClientMetadataMode(row.GetCredential("codex_client_metadata_mode"))
	}
	balanceQueryURL := ""
	if isOpenAIResponsesAccount && includeDetails {
		balanceQueryURL = row.GetCredential(openAIResponsesBalanceQueryURLCredential)
	}
	// 指纹收敛只作用于 Codex 官方出站路径，中转/Grok 账号不暴露该字段。
	codexFingerprintMode := ""
	if !isOpenAIResponsesAccount && !isGrokAccount && !isAntigravityAccount && !isClaudeAccount {
		codexFingerprintMode = auth.NormalizeCodexFingerprintMode(row.GetCredential(auth.CodexFingerprintModeCredentialKey))
	}
	// Claude Code 指纹收敛模式 + 绑定时区,仅 Claude OAuth 账号暴露。
	claudeFingerprintMode := ""
	accountTimezone := ""
	claudeClientPlatformOverride := ""
	claudeVersionPolicyOverride := ""
	claudeClientVersionOverride := ""
	claudeClientPolicy := auth.ClaudeClientPolicy{}
	if strings.EqualFold(strings.TrimSpace(row.GetCredential("upstream_type")), auth.UpstreamClaude) {
		claudeClientPolicy = auth.ClaudeClientPolicy{Platform: auth.ClaudeClientPlatformAny, VersionPolicy: auth.ClaudeVersionPolicyPassthrough}
		claudeFingerprintMode = auth.NormalizeClaudeFingerprintMode(row.GetCredential(auth.ClaudeFingerprintModeCredentialKey))
		accountTimezone = strings.TrimSpace(row.GetCredential("timezone"))
		claudeClientPlatformOverride = strings.ToLower(strings.TrimSpace(row.GetCredential(auth.ClaudeClientPlatformCredentialKey)))
		claudeVersionPolicyOverride = strings.ToLower(strings.TrimSpace(row.GetCredential(auth.ClaudeVersionPolicyCredentialKey)))
		claudeClientVersionOverride = strings.TrimSpace(row.GetCredential(auth.ClaudeClientVersionCredentialKey))
		if h.store != nil {
			claudeClientPolicy = h.store.ClaudeClientPolicy()
		}
		if claudeClientPlatformOverride != "" {
			claudeClientPolicy.Platform = auth.ClaudeClientPlatform(claudeClientPlatformOverride)
		}
		if claudeVersionPolicyOverride != "" {
			claudeClientPolicy.VersionPolicy = auth.ClaudeVersionPolicy(claudeVersionPolicyOverride)
		}
		if claudeClientVersionOverride != "" {
			claudeClientPolicy.ClientVersion = claudeClientVersionOverride
		}
		if normalized, err := auth.NormalizeClaudeClientPolicy(claudeClientPolicy); err == nil {
			claudeClientPolicy = normalized
		}
	}
	ignoreUsageLimitStatusOverride := row.GetCredentialOptionalBool("ignore_usage_limit_status_override")
	ignoreUsageLimitStatusEffective := h.store.IgnoreUsageLimitStatus()
	if ignoreUsageLimitStatusOverride != nil {
		ignoreUsageLimitStatusEffective = *ignoreUsageLimitStatusOverride
	}
	modelMapping := ""
	var customHeaders map[string]string
	var allowedAPIKeyIDs []int64
	claudeUserAgent := ""
	// 工作区 ID 不是密钥:Team/K12 徽章悬停要显示空间 ID。当前页
	// ListActiveByIDs 已带完整凭据;custom_headers 只用来算生效空间,
	// 摘要响应仍会剥掉原文。
	headers := row.GetCredentialStringMap("custom_headers")
	tokenWorkspaceID := openaiidentity.NormalizeWorkspaceID(row.GetCredential("workspace_id"))
	workspaceIDOverride := openaiidentity.WorkspaceOverrideFromHeaders(headers)
	effectiveWorkspaceID := openaiidentity.EffectiveWorkspaceID(tokenWorkspaceID, headers)
	if includeDetails {
		modelMapping = row.GetCredential("model_mapping")
		if isClaudeAccount {
			// Claude detail responses may be consumed by admin tooling, but must
			// never expose arbitrary historical custom headers such as
			// Authorization/Cookie/x-api-key. Keep only the provider identity
			// headers needed to inspect the stable fingerprint.
			customHeaders = claudeExportFingerprintHeaders(headers)
			for name, value := range customHeaders {
				if strings.EqualFold(strings.TrimSpace(name), "user-agent") {
					claudeUserAgent = strings.TrimSpace(value)
					break
				}
			}
		} else {
			customHeaders = headers
		}
		allowedAPIKeyIDs = row.GetCredentialInt64Slice("allowed_api_key_ids")
	}
	resp := accountResponse{
		DetailLoaded:                 includeDetails,
		ID:                           row.ID,
		Name:                         row.Name,
		Email:                        email,
		EmailDomain:                  accountEmailDomain(email),
		ChatGPTAccountID:             row.GetCredential("account_id"),
		TokenWorkspaceID:             tokenWorkspaceID,
		WorkspaceIDOverride:          workspaceIDOverride,
		EffectiveWorkspaceID:         effectiveWorkspaceID,
		PlanType:                     planType,
		SubscriptionExpiresAt:        row.GetCredential("subscription_expires_at"),
		Status:                       row.Status,
		ErrorMessage:                 row.ErrorMessage,
		ATOnly:                       !isOpenAIResponsesAccount && !isGrokAccount && !isAntigravityAccount && !isClaudeAccount && row.GetCredential("refresh_token") == "" && row.GetCredential("access_token") != "",
		CreditEnabled:                row.CreditEnabled,
		CreditSkipUsageWindow:        row.CreditSkipUsageWindow,
		SkipWarmTier:                 row.SkipWarmTier,
		AccountType:                  row.Type,
		AccessTokenType:              accountAccessTokenType(row),
		OpenAIResponsesAPI:           isOpenAIResponsesAccount,
		GrokAPI:                      isGrokAccount,
		AntigravityAPI:               isAntigravityAccount,
		ClaudeAPI:                    isClaudeAccount,
		ClaudeAuthKind:               claudeAuthKindForRow(row, isClaudeAccount),
		ClaudeBaseURL:                row.GetCredential(auth.ClaudeBaseURLCredentialKey),
		AntigravityAuthKind:          antigravityAuthKind,
		AgentIdentity:                isAgentIdentityCredentialRow(row),
		GrokAuthKind:                 grokAuthKind,
		GrokPlan:                     grokPlan,
		GrokBilling:                  grokBilling,
		AvatarURL:                    row.GetCredential("avatar_url"),
		VerifiedEmail:                row.GetCredentialBool("verified_email"),
		ProjectID:                    row.GetCredential("project_id"),
		AntigravityQuota:             antigravityQuota,
		AntigravityPermissions:       antigravityPermissions,
		AntigravitySyncWarning:       row.GetCredential("antigravity_sync_warning"),
		BaseURL:                      baseURL,
		BalanceQueryURL:              balanceQueryURL,
		Models:                       row.GetCredentialStringSlice("models"),
		ModelMapping:                 modelMapping,
		CodexClientMetadataMode:      codexClientMetadataMode,
		CodexFingerprintMode:         codexFingerprintMode,
		ClaudeFingerprintMode:        claudeFingerprintMode,
		ClaudeUserAgent:              claudeUserAgent,
		ClaudeClientPlatform:         string(claudeClientPolicy.Platform),
		ClaudeVersionPolicy:          string(claudeClientPolicy.VersionPolicy),
		ClaudeClientVersion:          claudeClientPolicy.ClientVersion,
		ClaudeClientPlatformOverride: claudeClientPlatformOverride,
		ClaudeVersionPolicyOverride:  claudeVersionPolicyOverride,
		ClaudeClientVersionOverride:  claudeClientVersionOverride,
		Timezone:                     accountTimezone,
		CustomHeaders:                customHeaders,
		UpstreamRequestIDHeader:      row.GetCredential(auth.UpstreamRequestIDHeaderCredentialKey),
		ProxyURL:                     row.ProxyURL,
		Enabled:                      row.Enabled,
		Locked:                       row.Locked,
		AllowedAPIKeyIDs:             allowedAPIKeyIDs,
		Tags:                         append([]string(nil), row.Tags...),
		Note:                         row.Note,
		ScoreBiasOverride:            nullableInt64Pointer(row.ScoreBiasOverride),
		ScoreBiasEffective:           effectiveScoreBias(planType, row.ScoreBiasOverride),
		BaseConcurrencyOverride:      nullableInt64Pointer(row.BaseConcurrencyOverride),
		BaseConcurrencyEffective:     effectiveBaseConcurrency(row.BaseConcurrencyOverride, int64(h.store.GetMaxConcurrency())),
		CreatedAt:                    row.CreatedAt.Format(time.RFC3339),
		UpdatedAt:                    row.UpdatedAt.Format(time.RFC3339),
		CodexUsageUpdatedAt:          row.GetCredential("codex_usage_updated_at"),
		Codex5HUsageUpdatedAt:        row.GetCredential("codex_5h_usage_updated_at"),
		ClaudeUsageProbeAt:           row.GetCredential(auth.ClaudeUsageProbeAtCredentialKey),
		ClaudeUsageProbeError:        row.GetCredential(auth.ClaudeUsageProbeErrorCredentialKey),
		ClaudeUsageWindows:           parseClaudeUsageWindows(row.GetCredential(auth.ClaudeUsageWindowsCredentialKey)),
		UsageLimitOverride:           ignoreUsageLimitStatusOverride,
		UsageLimitEffective:          ignoreUsageLimitStatusEffective,
	}
	// 凭据里只要存在 usage 窗口键(哪怕是空数组)就代表 OAuth usage 采样跑过。
	resp.ClaudeUsageWindowsProbed = strings.TrimSpace(row.GetCredential(auth.ClaudeUsageWindowsCredentialKey)) != ""
	if isAntigravityAccount {
		resp.Models = antigravityPublishedModelsOrDefault(row.GetCredentialStringSlice("models"))
	}
	resp.AutoPause5hThreshold = accountQuotaAutoPauseThreshold(row, "auto_pause_5h_threshold")
	resp.AutoPause7dThreshold = accountQuotaAutoPauseThreshold(row, "auto_pause_7d_threshold")
	resp.AutoPause5hDisabled = row.GetCredentialBool("auto_pause_5h_disabled")
	resp.AutoPause7dDisabled = row.GetCredentialBool("auto_pause_7d_disabled")
	if includeDetails {
		resp.DispatchCountLimit = accountDispatchCountLimit(row)
	}
	resp.SchedulerPriority = accountSchedulerPriority(row)

	now := time.Now()
	if runtimeAccount != nil {
		if includeDetails {
			resp.ModelCooldownModeOverride, resp.ModelCooldownSecondsOverride, resp.ModelCooldownBackoffOverride = runtimeAccount.GetModelCooldownPolicyOverride()
			effectiveCooldownPolicy := h.store.ResolveModelCooldownPolicy(runtimeAccount)
			resp.ModelCooldownModeEffective = effectiveCooldownPolicy.Mode
			resp.ModelCooldownSecondsEffective = effectiveCooldownPolicy.Seconds
			resp.ModelCooldownBackoffEffective = effectiveCooldownPolicy.BackoffEnabled
		}
		resp.UsageLimitOverride = runtimeAccount.GetIgnoreUsageLimitStatusOverride()
		resp.UsageLimitEffective = runtimeAccount.IgnoresUsageLimitStatus()
		if isGrokAccount {
			if snap, hasSnap := runtimeAccount.GetGrokRateLimitSnapshot(); hasSnap {
				resp.GrokRateLimit = &snap
			}
			if snap, hasSnap := runtimeAccount.GetGrokFreeQuotaSnapshot(); hasSnap {
				resp.GrokFreeQuota = &snap
			}
		}
		runtimeAccount.Mu().RLock()
		resp.GroupIDs = append([]int64(nil), runtimeAccount.GroupIDs...)
		runtimeAccount.Mu().RUnlock()
		resp.ActiveRequests = runtimeAccount.GetActiveRequests()
		resp.OccupiedRequests = runtimeAccount.GetOccupiedRequests()
		resp.SessionSlotBufferEnabled = h.store.SessionSlotBufferEnabled()
		resp.TotalRequests = runtimeAccount.GetTotalRequests()
		debug := runtimeAccount.GetSchedulerDebugSnapshot(int64(h.store.GetMaxConcurrency()))
		resp.HealthTier = debug.HealthTier
		resp.SchedulerScore = debug.SchedulerScore
		resp.ConcurrencyCap = debug.DynamicConcurrencyLimit
		if dispatchScore, ok := reflectFloat64Field(debug, "DispatchScore"); ok {
			resp.DispatchScore = dispatchScore
		}
		if scoreBiasEffective, ok := reflectInt64Field(debug, "ScoreBiasEffective"); ok {
			resp.ScoreBiasEffective = scoreBiasEffective
		}
		if baseConcurrencyEffective, ok := reflectInt64Field(debug, "BaseConcurrencyEffective"); ok {
			resp.BaseConcurrencyEffective = baseConcurrencyEffective
		}
		if includeDetails {
			resp.ScoreBreakdown = schedulerBreakdownResponse{
				UnauthorizedPenalty: debug.Breakdown.UnauthorizedPenalty,
				RateLimitPenalty:    debug.Breakdown.RateLimitPenalty,
				TimeoutPenalty:      debug.Breakdown.TimeoutPenalty,
				ServerPenalty:       debug.Breakdown.ServerPenalty,
				FailurePenalty:      debug.Breakdown.FailurePenalty,
				SuccessBonus:        debug.Breakdown.SuccessBonus,
				UsagePenalty7d:      debug.Breakdown.UsagePenalty7d,
				UsageUrgencyBonus5h: debug.Breakdown.UsageUrgencyBonus5h,
				UsageUrgencyBonus7d: debug.Breakdown.UsageUrgencyBonus7d,
				ExpiryUrgencyBonus:  debug.Breakdown.ExpiryUrgencyBonus,
				LatencyPenalty:      debug.Breakdown.LatencyPenalty,
				SuccessRatePenalty:  debug.Breakdown.SuccessRatePenalty,
			}
		}
		if usagePct, ok := runtimeAccount.GetUsagePercent7d(); ok {
			resp.UsagePercent7d = &usagePct
		}
		if usagePct5h, ok := runtimeAccount.GetUsagePercent5h(); ok {
			resp.UsagePercent5h = &usagePct5h
		}
		if usagePctSpark, ok := runtimeAccount.GetUsagePercentSpark(); ok {
			resp.UsagePercentSpark = &usagePctSpark
		}
		if credits, ok := runtimeAccount.GetRateLimitResetCredits(); ok {
			resp.RateLimitResetCredits = &credits
		}
		if applicable, ok := runtimeAccount.GetApplicableResetCredits(); ok {
			resp.ApplicableResetCredits = &applicable
		}
		if balance, hasCredits, unlimited, overage, ok := runtimeAccount.GetCreditBalance(); ok {
			resp.CreditsBalance = &balance
			resp.CreditsHasCredits = &hasCredits
			resp.CreditsUnlimited = &unlimited
			resp.CreditsOverageLimitReached = &overage
		}
		if includeDetails {
			if snapshot := runtimeAccount.GetDispatchCountSnapshot(); snapshot.Limit > 0 {
				limit := snapshot.Limit
				resp.DispatchCountLimit = &limit
				resp.DispatchCountUsed = snapshot.Used
				resp.DispatchCountLimited = snapshot.Limited
				if !snapshot.ResetAt.IsZero() {
					resp.DispatchCountResetAt = snapshot.ResetAt.Format(time.RFC3339)
				}
			}
		}
		if t := runtimeAccount.GetReset5hAt(); !t.IsZero() {
			resp.Reset5hAt = t.Format(time.RFC3339)
		}
		if t := runtimeAccount.GetReset7dAt(); !t.IsZero() {
			resp.Reset7dAt = t.Format(time.RFC3339)
		}
		if t := runtimeAccount.GetResetSparkAt(); !t.IsZero() {
			resp.ResetSparkAt = t.Format(time.RFC3339)
		}
		if sec := runtimeAccount.GetWindow7dSeconds(); sec > 0 {
			resp.Window7dSeconds = &sec
			resp.Window7dKind = runtimeAccount.Window7dKind()
		}
		if t := runtimeAccount.GetLastUsedAt(); !t.IsZero() {
			resp.LastUsedAt = t.Format(time.RFC3339)
		}
		if !debug.LastUnauthorizedAt.IsZero() {
			resp.LastUnauthorizedAt = debug.LastUnauthorizedAt.Format(time.RFC3339)
		}
		if !debug.LastRateLimitedAt.IsZero() {
			resp.LastRateLimitedAt = debug.LastRateLimitedAt.Format(time.RFC3339)
		}
		if !debug.LastTimeoutAt.IsZero() {
			resp.LastTimeoutAt = debug.LastTimeoutAt.Format(time.RFC3339)
		}
		if !debug.LastServerErrorAt.IsZero() {
			resp.LastServerErrorAt = debug.LastServerErrorAt.Format(time.RFC3339)
		}
		if reason, until := runtimeAccount.GetCooldownSnapshot(); !until.IsZero() && until.After(now) {
			resp.CooldownReason = reason
			resp.CooldownUntil = until.Format(time.RFC3339)
		}
		if includeDetails {
			for _, cooldown := range runtimeAccount.ActiveModelCooldowns() {
				resp.ModelCooldowns = append(resp.ModelCooldowns, modelCooldownResponse{
					Model:     cooldown.Model,
					Reason:    cooldown.Reason,
					ResetAt:   cooldown.ResetAt.Format(time.RFC3339),
					Remaining: int64(time.Until(cooldown.ResetAt).Seconds()),
				})
			}
		}
		if !isAntigravityAccount {
			resp.Status = runtimeAccount.RuntimeStatus()
			resp.UsingCredits = runtimeAccount.UsingCredits()
			runtimeAccount.Mu().RLock()
			resp.ErrorMessage = runtimeAccount.ErrorMsg
			runtimeAccount.Mu().RUnlock()
		}
	} else if row.CooldownUntil.Valid && row.CooldownUntil.Time.After(now) {
		resp.CooldownReason = row.CooldownReason
		resp.CooldownUntil = row.CooldownUntil.Time.Format(time.RFC3339)
	}
	if isAntigravityAccount {
		resp.Status, resp.ErrorMessage = antigravityPersistedStatus(row)
	}
	if resp.DispatchScore == 0 {
		resp.DispatchScore = dispatchScoreFallback(resp.SchedulerScore, resp.ScoreBiasEffective, resp.HealthTier, resp.Status)
	}
	if requestCount != nil {
		resp.SuccessRequests = requestCount.SuccessCount
		resp.ErrorRequests = requestCount.ErrorCount
		resp.RetryErrorRequests = requestCount.RetryErrorCount
		resp.RateLimitAttempts = requestCount.RateLimitAttemptCount
		if len(requestCount.ErrorStatusCounts) > 0 {
			resp.ErrorStatusCounts = make(map[string]int64, len(requestCount.ErrorStatusCounts))
			for code, count := range requestCount.ErrorStatusCounts {
				resp.ErrorStatusCounts[strconv.Itoa(code)] = count
			}
		}
		if len(requestCount.SuccessModelCounts) > 0 {
			resp.SuccessModelCounts = make(map[string]int64, len(requestCount.SuccessModelCounts))
			for model, count := range requestCount.SuccessModelCounts {
				resp.SuccessModelCounts[model] = count
			}
		}
	}
	if usage5h != nil {
		resp.Usage5hDetail = &accountUsageWindow{
			Requests:      usage5h.Requests,
			Tokens:        usage5h.Tokens,
			AccountBilled: usage5h.AccountBilled,
			UserBilled:    usage5h.UserBilled,
		}
	}
	if usage7d != nil {
		resp.Usage7dDetail = &accountUsageWindow{
			Requests:      usage7d.Requests,
			Tokens:        usage7d.Tokens,
			AccountBilled: usage7d.AccountBilled,
			UserBilled:    usage7d.UserBilled,
		}
	}
	if !includeDetails {
		stripAccountDetailFields(&resp)
	}
	return resp
}

func parseClaudeUsageWindows(raw string) []auth.ClaudeUsageWindow {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var windows []auth.ClaudeUsageWindow
	if err := json.Unmarshal([]byte(raw), &windows); err != nil {
		return nil
	}
	return windows
}

func stripAccountDetailFields(resp *accountResponse) {
	if resp == nil {
		return
	}
	resp.ModelMapping = ""
	resp.CodexClientMetadataMode = ""
	resp.CustomHeaders = nil
	resp.AllowedAPIKeyIDs = nil
	resp.Usage5hDetail = nil
	resp.Usage7dDetail = nil
	resp.Billed5h = nil
	resp.Billed7d = nil
	resp.ScoreBreakdown = schedulerBreakdownResponse{}
	resp.ModelCooldowns = nil
	resp.ModelCooldownModeOverride = nil
	resp.ModelCooldownSecondsOverride = nil
	resp.ModelCooldownBackoffOverride = nil
	resp.ModelCooldownModeEffective = ""
	resp.ModelCooldownSecondsEffective = 0
	resp.ModelCooldownBackoffEffective = false
	resp.DispatchCountLimit = nil
	resp.DispatchCountUsed = 0
	resp.DispatchCountResetAt = ""
	resp.DispatchCountLimited = false
}
