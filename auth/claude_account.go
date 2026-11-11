package auth

// Claude Code(Anthropic)账号在账号池中的运行时接线。
//
// 设计原则:尽量复用现有通用 OAuth 加载/调度框架,只新增 Claude 独有的部分。
//   - 加载:带 access_token + refresh_token 的 Claude 账号(upstream_type=claude)
//     直接走 buildAccountFromRow 的通用分支,无需改动那段 CRITICAL 代码。
//   - 刷新:Claude 的 RT 刷新端点与请求体和 ChatGPT/Codex 不同,故在
//     refreshAccountWithOptions 顶部按 IsClaudeOAuth() 早返回到这里的专用流程。

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"time"
)

// ClaudeDeviceIDCredentialKey 是 credentials.custom_headers 中可选的显式 device_id
// 覆盖键;缺省时由 ClaudeDeviceID 从账号身份确定性派生。
const ClaudeDeviceIDCredentialKey = "claude_device_id"

// UpstreamClaude 是 Claude Code OAuth 账号的 upstream_type 判别值。
const UpstreamClaude = "claude"

// ClaudeUsageProbeAtCredentialKey and ClaudeUsageProbeErrorCredentialKey are
// non-sensitive control-plane fields used by the admin account list to show
// whether an imported Claude account has completed its first native sampling
// request. They deliberately live alongside credentials so the existing
// SQLite/PostgreSQL projection remains backward compatible.
const (
	ClaudeUsageProbeAtCredentialKey    = "claude_usage_probe_at"
	ClaudeUsageProbeErrorCredentialKey = "claude_usage_probe_error"
	ClaudeUsageWindowsCredentialKey    = "claude_usage_windows"
)

// isClaudeOAuthLocked 判断账号是否为 Claude Code OAuth 账号。调用方需持有 a.mu。
func (a *Account) isClaudeOAuthLocked() bool {
	return strings.EqualFold(strings.TrimSpace(a.UpstreamType), UpstreamClaude)
}

// IsClaudeOAuth 是历史命名的 Claude 渠道判定,也包括 Setup Token 和 API Key。
func (a *Account) IsClaudeOAuth() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.isClaudeOAuthLocked()
}

// ClaudeAccountUUID 返回登录/刷新时写入的 Anthropic 账号 uuid（account_id 凭据）。
// 真实 CLI 的 metadata.user_id 里携带该值，缺失时返回空串。
func (a *Account) ClaudeAccountUUID() string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return strings.TrimSpace(a.AccountID)
}

// ClaudeDeviceID 返回该账号用于 metadata.user_id.device_id 的稳定 64-hex 值：
// 优先取显式配置的 claude_device_id，否则按账号身份确定性派生（同账号跨请求/
// 跨重启恒定，避免引入新的持久化字段）。真实 CLI 的 device_id 也是稳定设备指纹。
func (a *Account) ClaudeDeviceID() string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if v := strings.TrimSpace(a.CustomHeaders[ClaudeDeviceIDCredentialKey]); v != "" {
		return v
	}
	// The admin custom-header normalizer canonicalizes this metadata key to
	// Claude_device_id. Accept historical/mixed-case keys as well as the
	// original lowercase spelling, without making it an outbound HTTP header.
	for name, value := range a.CustomHeaders {
		if strings.EqualFold(strings.TrimSpace(name), ClaudeDeviceIDCredentialKey) {
			if value = strings.TrimSpace(value); value != "" {
				return value
			}
		}
	}
	seed := strings.TrimSpace(a.AccountID)
	if seed == "" {
		seed = fmt.Sprintf("db-%d", a.DBID)
	}
	sum := sha256.Sum256([]byte("claude-code-device:" + seed))
	return hex.EncodeToString(sum[:])
}

// refreshClaudeAccount 刷新一个 Claude Code OAuth 账号的 access token。
//
// 与 Grok/Codex 相比刷新逻辑刻意从简(自用场景账号数不多):复用跨实例共享的
// OAuth 刷新租约避免并发抢刷 + RT 轮换竞争,拿到新 token 后原子合并落库并更新
// 内存态与调度器。
func (s *Store) refreshClaudeAccount(ctx context.Context, acc *Account, forceRefresh bool) error {
	acc.mu.RLock()
	rt := strings.TrimSpace(acc.RefreshToken)
	dbID := acc.DBID
	proxyURL := strings.TrimSpace(acc.ProxyURL)
	lockedAccessToken := acc.AccessToken
	cooldownActive := acc.Status == StatusCooldown && time.Now().Before(acc.CooldownUtil)
	setupToken := acc.isClaudeSetupTokenLocked()
	apiKey := acc.isClaudeAPIKeyLocked()
	expiresAt := acc.ExpiresAt
	acc.mu.RUnlock()

	if apiKey {
		return nil
	}
	if setupToken {
		// Setup Token 是长效 AT,没有可刷新的 RT:未到期无需任何动作,到期只能重新授权。
		if !expiresAt.IsZero() && time.Now().After(expiresAt) {
			return fmt.Errorf("claude setup token 已过期,请重新授权或导入新的 Setup Token")
		}
		return nil
	}
	if rt == "" {
		return fmt.Errorf("claude refresh_token 为空")
	}

	// 跨实例共享刷新租约:等待期间别的实例可能已经轮换过 RT,拿到锁后重新读库,
	// 若已被刷新且可用则直接复用,避免第二次刷新消费掉刚轮换出来的新 RT。
	lease, lockErr := s.acquireOAuthRefreshLease(ctx, rt)
	if lockErr != nil {
		return lockErr
	}
	defer lease.Release()
	ctx = lease.Context()

	if changed, usable, reloadErr := s.reloadOAuthCredentialsAfterLock(ctx, acc, rt, lockedAccessToken); reloadErr != nil {
		// 读库失败不阻断刷新,继续用入口快照的 rt 尝试。
	} else if changed && usable && !forceRefresh {
		s.finishReloadedOAuthRefresh(ctx, acc)
		s.clearClaudeStaleAuthFailure(ctx, acc)
		return nil
	} else if changed {
		acc.mu.RLock()
		rt = strings.TrimSpace(acc.RefreshToken)
		acc.mu.RUnlock()
		if rt == "" {
			return fmt.Errorf("claude refresh_token 为空")
		}
	}

	var td *ClaudeTokenData
	var err error
	if s.claudeTokenRefresh != nil {
		td, err = s.claudeTokenRefresh(ctx, proxyURL, rt)
	} else {
		td, err = NewClaudeAuth(proxyURL).RefreshTokens(ctx, rt)
	}
	if err != nil {
		wrapped := fmt.Errorf("claude token 刷新失败: %w", err)
		// invalid_grant and the other permanent OAuth codes mean the refresh
		// token itself is gone. Record bounded backoff and do not treat a
		// transport or 5xx failure as the same revocation.
		if IsPermanentRefreshFailure(err) {
			s.markPermanentRefreshFailure(acc, wrapped)
		}
		return wrapped
	}
	if td == nil || strings.TrimSpace(td.AccessToken) == "" {
		return fmt.Errorf("claude 刷新响应缺少 access_token")
	}

	// 原子合并落库(JSONB ||,不覆盖其他字段)。
	updates := map[string]interface{}{
		"access_token": td.AccessToken,
		"expires_at":   td.ExpiresAt.Format(time.RFC3339),
	}
	if strings.TrimSpace(td.RefreshToken) != "" {
		updates["refresh_token"] = td.RefreshToken
	}
	if strings.TrimSpace(td.Email) != "" {
		updates["email"] = td.Email
	}
	// 订阅档位随 profile 变化(升降级)时同步更新。
	if plan := strings.TrimSpace(td.PlanType); plan != "" {
		updates["plan_type"] = plan
	}
	if strings.TrimSpace(td.AccountUUID) != "" {
		updates["account_id"] = td.AccountUUID
	}
	if s.db != nil {
		if err := s.db.UpdateCredentials(ctx, dbID, updates); err != nil {
			return fmt.Errorf("claude 刷新结果落库失败: %w", err)
		}
	}

	// 更新内存态与调度器。限流等非鉴权冷却保留,只换令牌。
	// unauthorized 冷却来自过期 AT 的 401:新令牌已经有效,不能继续盖住账号。
	acc.mu.Lock()
	acc.AccessToken = td.AccessToken
	if strings.TrimSpace(td.RefreshToken) != "" {
		acc.RefreshToken = td.RefreshToken
	}
	acc.ExpiresAt = td.ExpiresAt
	if strings.TrimSpace(td.Email) != "" {
		acc.Email = td.Email
	}
	if strings.TrimSpace(td.AccountUUID) != "" {
		acc.AccountID = td.AccountUUID
	}
	if plan := strings.TrimSpace(td.PlanType); plan != "" {
		acc.PlanType = plan
	}
	unauthorized := strings.EqualFold(strings.TrimSpace(acc.CooldownReason), "unauthorized")
	preserveCooldown := cooldownActive && !unauthorized
	if !preserveCooldown {
		acc.Status = StatusReady
		acc.CooldownUtil = time.Time{}
		acc.CooldownReason = ""
		acc.ErrorMsg = ""
	}
	if acc.Status != StatusError {
		acc.HealthTier = HealthTierHealthy
	}
	acc.recomputeSchedulerLocked(s.maxConcurrency.Load())
	acc.mu.Unlock()

	s.fastSchedulerUpdate(acc)
	if !preserveCooldown && s.db != nil {
		_ = s.db.ClearError(ctx, dbID)
		s.deleteCachedAccountCooldown(dbID)
	}
	s.clearClaudeStaleAuthFailure(ctx, acc)
	return nil
}

// ClaudeOAuthRefreshable reports whether an upstream 401 can be recovered by
// refreshing this account's Claude OAuth refresh token once.
func (a *Account) ClaudeOAuthRefreshable() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.isClaudeOAuthLocked() || a.isClaudeAPIKeyLocked() || a.isClaudeSetupTokenLocked() {
		return false
	}
	return strings.TrimSpace(a.RefreshToken) != ""
}

// RefreshClaudeOAuthAfterUnauthorized exchanges the refresh token once after
// an upstream 401, serialized by the shared OAuth refresh lease. A successful
// exchange clears stale disabled, banned, and unauthorized cooldown state. A
// revoked refresh token (invalid_grant) keeps the bounded permanent-refresh
// backoff. Transport and upstream failures are returned without banning.
func (s *Store) RefreshClaudeOAuthAfterUnauthorized(ctx context.Context, acc *Account) error {
	if s == nil || acc == nil || !acc.ClaudeOAuthRefreshable() {
		return fmt.Errorf("account is not a refreshable Claude OAuth account")
	}
	return s.refreshClaudeAccount(ctx, acc, false)
}

// SetClaudeTokenRefreshForTest replaces the Claude token endpoint. The
// returned function restores the previous hook.
func (s *Store) SetClaudeTokenRefreshForTest(fn func(context.Context, string, string) (*ClaudeTokenData, error)) func() {
	if s == nil {
		return func() {}
	}
	previous := s.claudeTokenRefresh
	s.claudeTokenRefresh = fn
	return func() { s.claudeTokenRefresh = previous }
}

// claudeAuthFailureNeedsBackgroundRefreshLocked reports a Claude OAuth account
// whose access token was latched unauthorized while the refresh token may
// still be valid. The caller holds acc.mu. Permanent refresh failures and
// terminal errors stay excluded so a revoked token is not retried forever.
func claudeAuthFailureNeedsBackgroundRefreshLocked(acc *Account) bool {
	if acc == nil || !acc.isClaudeOAuthLocked() || acc.isClaudeAPIKeyLocked() || acc.isClaudeSetupTokenLocked() {
		return false
	}
	if acc.Status == StatusError || acc.PermanentRefreshFailures > 0 || strings.TrimSpace(acc.RefreshToken) == "" {
		return false
	}
	if atomic.LoadInt32(&acc.Disabled) != 0 || acc.healthTierLocked() == HealthTierBanned {
		return true
	}
	return acc.Status == StatusCooldown && strings.EqualFold(strings.TrimSpace(acc.CooldownReason), "unauthorized")
}

// clearClaudeStaleAuthFailure drops the 401 latch after a Claude access token
// has been refreshed or reloaded from another instance. Rate-limit cooldowns
// are left in place.
func (s *Store) clearClaudeStaleAuthFailure(ctx context.Context, acc *Account) {
	if s == nil || acc == nil || !acc.IsClaudeOAuth() || acc.IsClaudeAPIKey() {
		return
	}
	atomic.StoreInt32(&acc.Disabled, 0)
	acc.mu.Lock()
	unauthorized := strings.EqualFold(strings.TrimSpace(acc.CooldownReason), "unauthorized")
	if unauthorized {
		acc.Status = StatusReady
		acc.CooldownUtil = time.Time{}
		acc.CooldownReason = ""
		acc.ErrorMsg = ""
	}
	if acc.Status != StatusError && (unauthorized || acc.HealthTier == HealthTierBanned) {
		acc.HealthTier = HealthTierHealthy
	}
	acc.PermanentRefreshFailures = 0
	acc.recomputeSchedulerLocked(s.maxConcurrency.Load())
	dbID := acc.DBID
	acc.mu.Unlock()
	s.fastSchedulerUpdate(acc)
	if !unauthorized {
		return
	}
	s.deleteCachedAccountCooldown(dbID)
	if s.db == nil || dbID <= 0 {
		return
	}
	if err := s.db.ClearError(ctx, dbID); err != nil {
		log.Printf("[账号 %d] 清理 Claude unauthorized 状态失败: %v", dbID, err)
	}
}
