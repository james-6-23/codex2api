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
	"errors"
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"time"

	"github.com/codex2api/security"
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
	ClaudeUsageProbeAtCredentialKey       = "claude_usage_probe_at"
	ClaudeUsageProbeErrorCredentialKey    = "claude_usage_probe_error"
	ClaudeUsageWindowsCredentialKey       = "claude_usage_windows"
	ClaudeLastRefreshAtCredentialKey      = "claude_last_refresh_at"
	ClaudeRefreshErrorCredentialKey       = "claude_refresh_error"
	ClaudeRefreshNextRetryAtCredentialKey = "claude_refresh_next_retry_at"

	// claudeBackgroundRefreshLead 对齐 sub2api 的 30 分钟提前续期窗口：
	// Claude OAuth AT 默认 8h，相比 5 分钟窗口更能缓冲短暂网络抖动或 429 退避，
	// 又避免高频轮换单次 RT。
	claudeBackgroundRefreshLead     = 30 * time.Minute
	claudeIneffectiveRefreshBackoff = 5 * time.Minute
	claudeRefreshErrorMaxLen        = 512
)

func (s *Store) GetClaudeOAuthKeepalive() bool        { return s.claudeOAuthKeepalive.Load() }
func (s *Store) SetClaudeOAuthKeepalive(enabled bool) { s.claudeOAuthKeepalive.Store(enabled) }

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
// 复用跨实例共享的 OAuth 刷新租约并在 RT 变更时重获新租约，避免并发抢刷 + RT 轮换竞争；
// 交换后使用不可取消上下文带重试落库，防止调用方断连丢弃已轮换的新 RT；
// 区分永久 OAuth 失败与瞬时网络/429/5xx 失败，记录脱敏状态与指数退避。
func (s *Store) refreshClaudeAccount(ctx context.Context, acc *Account, forceRefresh bool) error {
	acc.mu.RLock()
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

	var lease *oauthRefreshLease
	for retries := 0; ; retries++ {
		if retries >= 4 {
			return fmt.Errorf("Claude 凭据连续变更，请稍后重试")
		}
		acc.mu.RLock()
		rt := strings.TrimSpace(acc.RefreshToken)
		lockedAccessToken := acc.AccessToken
		acc.mu.RUnlock()
		if rt == "" {
			return fmt.Errorf("claude refresh_token 为空")
		}

		// 跨实例共享刷新租约:等待期间别的实例可能已经轮换过 RT,拿到锁后重新读库,
		// 若已被刷新且可用则直接复用,避免第二次刷新消费掉刚轮换出来的新 RT。
		var lockErr error
		lease, lockErr = s.acquireOAuthRefreshLease(ctx, rt)
		if lockErr != nil {
			return lockErr
		}
		changed, usable, reloadErr := s.reloadOAuthCredentialsAfterLock(ctx, acc, rt, lockedAccessToken)
		if reloadErr != nil {
			lease.Release()
			return fmt.Errorf("刷新前读取最新凭据失败: %w", reloadErr)
		}
		if changed {
			lease.Release()
			lease = nil
			if usable {
				s.finishReloadedOAuthRefresh(ctx, acc)
				s.clearClaudeStaleAuthFailure(ctx, acc)
				s.invalidateRoutingSchedulers()
				return nil
			}
			continue
		}
		defer lease.Release()
		break
	}

	acc.mu.RLock()
	rt := strings.TrimSpace(acc.RefreshToken)
	oldAT := acc.AccessToken
	dbID := acc.DBID
	acc.mu.RUnlock()
	if rt == "" {
		return fmt.Errorf("claude refresh_token 为空")
	}

	proxyURL := s.ResolveProxyForAccount(acc)
	if strings.TrimSpace(proxyURL) == "" && s.GetProxyPoolEnabled() {
		proxyErr := fmt.Errorf("账号 %d 代理池已启用但无可用代理，已拒绝直连刷新", dbID)
		s.recordClaudeTransientRefreshFailure(ctx, acc, dbID, proxyErr, rt, oldAT)
		return proxyErr
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if lease != nil {
		ctx = lease.CriticalContext()
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
		// token itself is gone. First re-read DB to recover from a concurrent
		// rotation race (sub2api / Codex pattern), then record bounded permanent
		// failure without leaking secrets.
		if IsPermanentRefreshFailure(err) {
			changed, usable, reloadErr := s.reloadOAuthCredentialsAfterLock(context.WithoutCancel(ctx), acc, rt, oldAT)
			if reloadErr == nil && changed && usable {
				s.finishReloadedOAuthRefresh(ctx, acc)
				s.clearClaudeStaleAuthFailure(ctx, acc)
				s.invalidateRoutingSchedulers()
				return nil
			}
			s.recordClaudePermanentRefreshFailure(ctx, acc, dbID, wrapped, rt, oldAT)
			return wrapped
		}
		if !errors.Is(err, context.Canceled) {
			s.recordClaudeTransientRefreshFailure(ctx, acc, dbID, wrapped, rt, oldAT)
		}
		return wrapped
	}
	if td == nil || strings.TrimSpace(td.AccessToken) == "" {
		emptyErr := fmt.Errorf("claude 刷新响应缺少 access_token")
		s.recordClaudeTransientRefreshFailure(ctx, acc, dbID, emptyErr, rt, oldAT)
		return emptyErr
	}

	now := time.Now()
	nextRetryAt := time.Time{}
	nextRetryRaw := ""
	if !td.ExpiresAt.IsZero() && td.ExpiresAt.Sub(now) < claudeBackgroundRefreshLead {
		// 参考 CPA refreshIneffectiveBackoff：若上游返回的 AT 仍落在提前续期窗口内，
		// 写入最小退避避免后台巡检高频空转。
		nextRetryAt = now.Add(claudeIneffectiveRefreshBackoff)
		nextRetryRaw = nextRetryAt.UTC().Format(time.RFC3339)
	}

	// 原子合并落库(JSONB ||,不覆盖其他字段)。
	// 使用 WithoutCancel + 重试保护已轮换的 RT，避免请求断连或瞬时 DB 抖动丢凭据。
	updates := map[string]interface{}{
		"access_token":                        td.AccessToken,
		"expires_at":                          td.ExpiresAt.Format(time.RFC3339),
		ClaudeLastRefreshAtCredentialKey:      now.UTC().Format(time.RFC3339),
		ClaudeRefreshErrorCredentialKey:       "",
		ClaudeRefreshNextRetryAtCredentialKey: nextRetryRaw,
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
	persistCtx := context.WithoutCancel(ctx)
	if s.db != nil {
		var saveErr error
		for n := 0; n < 4; n++ {
			attemptCtx, cancel := context.WithTimeout(persistCtx, 5*time.Second)
			saveErr = s.db.UpdateCredentials(attemptCtx, dbID, updates)
			cancel()
			if saveErr == nil || n == 3 {
				break
			}
			timer := time.NewTimer(time.Duration(1<<n) * 100 * time.Millisecond)
			<-timer.C
		}
		if saveErr != nil {
			wrappedSaveErr := fmt.Errorf("claude 刷新结果落库失败: %w", saveErr)
			s.recordClaudeTransientRefreshFailure(persistCtx, acc, dbID, wrappedSaveErr, rt, oldAT, td.RefreshToken, td.AccessToken)
			return wrappedSaveErr
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
	acc.PermanentRefreshFailures = 0
	acc.claudeRefreshFailures = 0
	acc.claudeRefreshNextRetryAt = nextRetryAt
	unauthorized := strings.EqualFold(strings.TrimSpace(acc.CooldownReason), "unauthorized")
	preserveCooldown := acc.Status == StatusCooldown && now.Before(acc.CooldownUtil) && !unauthorized
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
	at, expiry := acc.AccessToken, acc.ExpiresAt
	acc.mu.Unlock()

	if s.tokenCache != nil && time.Until(expiry) > 5*time.Minute {
		cacheCtx, cancel := context.WithTimeout(persistCtx, 2*time.Second)
		if cacheErr := s.tokenCache.SetAccessToken(cacheCtx, dbID, at, time.Until(expiry)-5*time.Minute); cacheErr != nil {
			log.Printf("[账号 %d] Claude 新凭据已保存，更新 Token 缓存失败: %v", dbID, cacheErr)
		}
		cancel()
	}
	s.fastSchedulerUpdate(acc)
	if !preserveCooldown && s.db != nil {
		clearCtx, cancel := context.WithTimeout(persistCtx, 3*time.Second)
		_ = s.db.ClearError(clearCtx, dbID)
		cancel()
		s.deleteCachedAccountCooldown(dbID)
	}
	s.clearClaudeStaleAuthFailure(persistCtx, acc)
	s.invalidateRoutingSchedulers()
	return nil
}

func claudeTransientRefreshBackoff(failures int, retryAfter time.Duration) time.Duration {
	schedule := [...]time.Duration{
		2 * time.Minute,
		5 * time.Minute,
		15 * time.Minute,
		30 * time.Minute,
		60 * time.Minute,
	}
	idx := failures - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(schedule) {
		idx = len(schedule) - 1
	}
	backoff := schedule[idx]
	if retryAfter > backoff {
		backoff = retryAfter
	}
	if backoff > time.Hour {
		backoff = time.Hour
	}
	return backoff
}

func sanitizeClaudeRefreshError(err error, secrets ...string) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	for _, secret := range secrets {
		if trimmed := strings.TrimSpace(secret); trimmed != "" {
			msg = strings.ReplaceAll(msg, trimmed, "[REDACTED]")
		}
	}
	msg = strings.TrimSpace(security.SanitizeLog(msg))
	if len(msg) > claudeRefreshErrorMaxLen {
		msg = msg[:claudeRefreshErrorMaxLen]
	}
	return msg
}

func (s *Store) persistClaudeRefreshStatus(ctx context.Context, dbID int64, errMsg string, nextRetryAt time.Time) {
	if s == nil || s.db == nil || dbID <= 0 {
		return
	}
	nextRetryRaw := ""
	if !nextRetryAt.IsZero() {
		nextRetryRaw = nextRetryAt.UTC().Format(time.RFC3339)
	}
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	if err := s.db.UpdateCredentials(writeCtx, dbID, map[string]interface{}{
		ClaudeRefreshErrorCredentialKey:       errMsg,
		ClaudeRefreshNextRetryAtCredentialKey: nextRetryRaw,
	}); err != nil {
		log.Printf("[账号 %d] 保存 Claude 刷新诊断状态失败: %v", dbID, err)
	}
}

func (s *Store) recordClaudeTransientRefreshFailure(ctx context.Context, acc *Account, dbID int64, err error, secrets ...string) {
	if acc == nil || err == nil {
		return
	}
	now := time.Now()
	acc.mu.Lock()
	acc.claudeRefreshFailures++
	backoff := claudeTransientRefreshBackoff(acc.claudeRefreshFailures, oauthRefreshRetryAfter(err))
	nextRetry := now.Add(backoff)
	// 参考 CPA：AT 仍未过期时退避不超过过期时刻，保证过期边界能及时续期。
	if strings.TrimSpace(acc.AccessToken) != "" && !acc.ExpiresAt.IsZero() && acc.ExpiresAt.After(now) && nextRetry.After(acc.ExpiresAt) {
		nextRetry = acc.ExpiresAt
	}
	acc.claudeRefreshNextRetryAt = nextRetry
	acc.mu.Unlock()
	s.persistClaudeRefreshStatus(ctx, dbID, sanitizeClaudeRefreshError(err, secrets...), nextRetry)
}

func (s *Store) recordClaudePermanentRefreshFailure(ctx context.Context, acc *Account, dbID int64, wrapped error, secrets ...string) {
	if acc == nil || wrapped == nil {
		return
	}
	acc.mu.Lock()
	acc.claudeRefreshNextRetryAt = time.Time{}
	acc.mu.Unlock()
	s.persistClaudeRefreshStatus(ctx, dbID, sanitizeClaudeRefreshError(wrapped, secrets...), time.Time{})
	s.markPermanentRefreshFailure(acc, wrapped)
}

func (a *Account) isClaudeOAuthRefreshableLocked() bool {
	if a == nil || !a.isClaudeOAuthLocked() || a.isClaudeAPIKeyLocked() || a.isClaudeSetupTokenLocked() {
		return false
	}
	return strings.TrimSpace(a.RefreshToken) != ""
}

// ClaudeOAuthRefreshable reports whether an upstream 401 can be recovered by
// refreshing this account's Claude OAuth refresh token once.
func (a *Account) ClaudeOAuthRefreshable() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.isClaudeOAuthRefreshableLocked()
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
	if !acc.isClaudeOAuthRefreshableLocked() || atomic.LoadInt32(&acc.DispatchPaused) != 0 {
		return false
	}
	if acc.Status == StatusError || acc.PermanentRefreshFailures > 0 {
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
