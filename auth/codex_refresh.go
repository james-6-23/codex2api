package auth

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/internal/openaiidentity"
)

func (s *Store) GetCodexOAuthKeepalive() bool        { return s.codexOAuthKeepalive.Load() }
func (s *Store) SetCodexOAuthKeepalive(enabled bool) { s.codexOAuthKeepalive.Store(enabled) }

// Refresh cancellation is allowed while waiting, but not after consuming a
// rotating RT. Persist before publishing, and retry the persistence operation
// (not OAuth) when the database temporarily fails.
func (s *Store) refreshCodexAccount(ctx context.Context, acc *Account, force bool) error {
	var lease *oauthRefreshLease
	for retries := 0; ; retries++ {
		if retries >= 4 {
			return fmt.Errorf("Codex 凭据连续变更，请稍后重试")
		}
		acc.mu.RLock()
		rt, at := strings.TrimSpace(acc.RefreshToken), acc.AccessToken
		acc.mu.RUnlock()
		if rt == "" {
			break
		}
		var err error
		lease, err = s.acquireOAuthRefreshLease(ctx, rt)
		if err != nil {
			return err
		}
		changed, usable, err := s.reloadOAuthCredentialsAfterLock(ctx, acc, rt, at)
		if err != nil {
			lease.Release()
			return fmt.Errorf("刷新前读取最新凭据失败: %w", err)
		}
		if changed {
			lease.Release()
			lease = nil
			// Concurrent forced refreshes share the result of the first exchange.
			if usable {
				return s.publishCodexRefresh(ctx, acc)
			}
			continue
		}
		defer lease.Release()
		break
	}
	acc.mu.RLock()
	rt, st, oldAT, dbID, generation := strings.TrimSpace(acc.RefreshToken), acc.SessionToken, acc.AccessToken, acc.DBID, acc.CredentialGeneration
	acc.mu.RUnlock()
	if !force && s.tokenCache != nil {
		cached, err := s.tokenCache.GetAccessToken(ctx, dbID)
		if err != nil && s.tokenCache.SharedAcrossInstances() {
			return fmt.Errorf("读取 Token 缓存失败: %w", err)
		}
		if cached != "" {
			// Do not invent an expiry for the same token; a stale cache must not
			// postpone the actual exchange indefinitely.
			acc.mu.RLock()
			fresh := cached == acc.AccessToken && time.Until(acc.ExpiresAt) > 5*time.Minute
			acc.mu.RUnlock()
			if fresh {
				return nil
			}
		}
	}
	proxyURL := s.ResolveProxyForAccount(acc)
	if strings.TrimSpace(proxyURL) == "" && s.GetProxyPoolEnabled() {
		return fmt.Errorf("账号 %d 代理池已启用但无可用代理，已拒绝直连刷新", dbID)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if lease != nil {
		ctx = lease.CriticalContext()
	}
	if rt != "" {
		identityCtx, err := withCodexOAuthIdentity(ctx, acc, force)
		if err != nil {
			return err
		}
		ctx = identityCtx
	}
	var attempt *database.CodexRefreshAttempt
	if rt != "" && s.db != nil {
		var err error
		attempt, err = s.db.BeginCodexRefresh(ctx, dbID, generation, rt)
		if err != nil {
			if errors.Is(err, database.ErrCodexRefreshUncertain) {
				s.MarkError(acc, err.Error())
			}
			return err
		}
	}
	resinID := fmt.Sprint(dbID)
	var td *TokenData
	var info *AccountInfo
	var err error
	if rt != "" {
		td, info, err = RefreshWithRetry(ctx, rt, proxyURL, resinID)
	} else {
		err = fmt.Errorf("refresh_token 为空")
	}
	warning := ""
	if err != nil && st != "" && !isOAuthRefreshUncertain(err) {
		rtErr := err
		td, info, err = RefreshWithSessionTokenRetry(ctx, st, proxyURL, resinID)
		if err == nil {
			td.RefreshToken = rt
			warning = "RT 刷新失败，已使用 session_token 临时续期；请更新 OAuth 授权"
		} else {
			err = fmt.Errorf("RT 刷新失败: %v；session_token 回退失败: %w", rtErr, err)
		}
	}
	if err != nil {
		uncertain := isOAuthRefreshUncertain(err)
		message := err.Error()
		if uncertain {
			message = database.ErrCodexRefreshUncertain.Error() + ": " + message
		}
		if IsPermanentRefreshFailure(err) {
			message = "OAuth 授权已失效，请重新登录并导入最新凭据: " + message
		}
		if attempt != nil {
			failureCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			if saveErr := s.db.FailCodexRefresh(failureCtx, attempt, message, uncertain); saveErr != nil {
				log.Printf("[账号 %d] 保存刷新失败状态失败，保留刷新保护记录: %v", dbID, saveErr)
			}
			cancel()
		}
		if uncertain {
			s.MarkError(acc, message)
		} else if IsPermanentRefreshFailure(err) {
			// Another credential replacement may have won while OAuth was in flight.
			changed, usable, reloadErr := s.reloadOAuthCredentialsAfterLock(ctx, acc, rt, oldAT)
			if reloadErr == nil && changed && usable {
				return s.publishCodexRefresh(ctx, acc)
			}
			s.markPermanentRefreshFailure(acc, errors.New(message))
		}
		return errors.New(message)
	}
	updates := codexRefreshedCredentials(acc, td, info, warning)
	if st != "" {
		updates["session_token"] = st
	}
	var publishedIDs []int64
	if attempt != nil {
		// Lock the new RT before exposing it so old and new lease namespaces
		// cannot overlap during publication on another instance.
		if td.RefreshToken != "" && td.RefreshToken != rt {
			rotated, lockErr := s.acquireOAuthRefreshLease(ctx, td.RefreshToken)
			if lockErr != nil {
				return fmt.Errorf("新 RT 尚未保存，刷新保护记录已保留: %w", lockErr)
			}
			defer rotated.Release()
		}
		for n := 0; n < 4; n++ {
			publishedIDs, err = s.db.FinishCodexRefresh(ctx, attempt, updates)
			if err == nil {
				break
			}
			if n == 3 {
				break
			}
			timer := time.NewTimer(time.Duration(1<<n) * 100 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
			case <-timer.C:
			}
			if ctx.Err() != nil {
				break
			}
		}
		if err != nil {
			message := "新 Token 保存失败，已停止重复消费旧 RT；请重新授权导入凭据"
			s.MarkError(acc, message)
			return fmt.Errorf("%s: %w", message, err)
		}
	} else if s.db != nil {
		// Session-token-only accounts do not consume an OAuth RT.
		if err := s.db.UpdateCredentials(ctx, dbID, updates); err != nil {
			return fmt.Errorf("保存 session 凭据失败: %w", err)
		}
		publishedIDs = []int64{dbID}
	} else {
		applyCodexCredentialValues(acc, updates, generation)
		s.finishCodexRefresh(ctx, acc, warning)
		return nil
	}
	// Reload durable values rather than publishing a stale response over a
	// concurrent administrative replacement. Other instances use the outbox.
	for _, id := range publishedIDs {
		if account := s.FindByID(id); account != nil {
			if err := s.publishCodexRefresh(ctx, account); err != nil {
				return err
			}
		}
	}
	if !containsCodexRefreshID(publishedIDs, dbID) {
		return s.publishCodexRefresh(ctx, acc)
	}
	return nil
}

func containsCodexRefreshID(ids []int64, id int64) bool {
	for _, candidate := range ids {
		if candidate == id {
			return true
		}
	}
	return false
}

func codexRefreshedCredentials(acc *Account, td *TokenData, info *AccountInfo, warning string) map[string]any {
	now := time.Now()
	updates := map[string]any{"access_token": td.AccessToken, "expires_at": td.ExpiresAt.Format(time.RFC3339),
		"codex_last_refresh_at": now.UTC().Format(time.RFC3339), "codex_refresh_error": warning}
	if td.RefreshToken != "" {
		updates["refresh_token"] = td.RefreshToken
	}
	if td.IDToken != "" {
		updates["id_token"] = td.IDToken
	}
	if info != nil {
		if info.Email != "" {
			updates["email"] = info.Email
		}
		if info.ChatGPTAccountID != "" {
			updates["account_id"] = info.ChatGPTAccountID
		}
		acc.mu.RLock()
		planSnapshot := Account{PlanType: acc.PlanType, UsagePercent7dValid: acc.UsagePercent7dValid, Reset7dAt: acc.Reset7dAt}
		oldSubscription := acc.SubscriptionExpiresAt
		subscriptionSource := acc.subscriptionMeta.Source
		acc.mu.RUnlock()
		if plan, applied := planSnapshot.applyRefreshedPlanTypeLocked(info.PlanType, now); applied {
			updates["plan_type"] = plan
		}
		if !info.SubscriptionExpiresAt.IsZero() && !StaleSubscriptionExpiry(planSnapshot.PlanType, info.SubscriptionExpiresAt, now) {
			// 令牌里的 chatgpt_subscription_active_until 续费后长期停留在旧值：
			// 已有订阅提供方权威到期时间时，不让更早的令牌值把它打回去。
			jwtOlderThanAuthoritative := subscriptionSource == SubscriptionSourceProviderAPI &&
				!oldSubscription.IsZero() && info.SubscriptionExpiresAt.Before(oldSubscription)
			if !jwtOlderThanAuthoritative {
				updates["subscription_expires_at"] = info.SubscriptionExpiresAt.Format(time.RFC3339)
				if !info.SubscriptionExpiresAt.Equal(oldSubscription) {
					updates[SubscriptionSourceCredentialKey] = SubscriptionSourceJWT
				}
			}
		} else if StaleSubscriptionExpiry(planSnapshot.PlanType, oldSubscription, now) {
			updates["subscription_expires_at"] = ""
		}
	}
	if email, workspace := openaiidentity.TokenIdentity(td.IDToken, td.AccessToken); workspace != "" {
		updates["email"], updates["account_id"], updates["workspace_id"] = email, workspace, workspace
	}
	return updates
}

func applyCodexCredentialValues(acc *Account, credentials map[string]any, generation int64) bool {
	row := database.AccountRow{Credentials: credentials}
	acc.mu.Lock()
	defer acc.mu.Unlock()
	if generation > 0 && acc.CredentialGeneration > generation {
		return false
	}
	acc.AccessToken = row.GetCredential("access_token")
	acc.RefreshToken = row.GetCredential("refresh_token")
	acc.SessionToken = row.GetCredential("session_token")
	acc.ExpiresAt = parseOAuthCredentialExpiry(row.GetCredential("expires_at"))
	if plan := row.GetCredential("plan_type"); plan != "" {
		acc.applyRefreshedPlanTypeLocked(plan, time.Now())
	}
	if email := row.GetCredential("email"); email != "" {
		acc.Email = email
	}
	if id := row.GetCredential("account_id"); id != "" {
		acc.AccountID = id
	}
	if expiry, ok := credentials["subscription_expires_at"]; ok {
		acc.SubscriptionExpiresAt = parseOAuthCredentialExpiry(fmt.Sprint(expiry))
	}
	if _, ok := credentials[SubscriptionSyncStateCredentialKey]; ok {
		// 整行重载：以库中元数据为准。
		acc.subscriptionMeta = SubscriptionMetaFromCredentials(row.GetCredential)
	} else if src, ok := credentials[SubscriptionSourceCredentialKey]; ok {
		// 无库路径只带了来源一个键，别用空值覆盖其余元数据。
		acc.subscriptionMeta.Source = strings.TrimSpace(fmt.Sprint(src))
	}
	acc.CredentialGeneration = generation
	acc.PermanentRefreshFailures = 0
	return true
}

func (s *Store) publishCodexRefresh(ctx context.Context, acc *Account) error {
	if s.db == nil {
		s.finishCodexRefresh(ctx, acc, "")
		return nil
	}
	row, err := s.db.GetAccountByID(ctx, acc.DBID)
	if err != nil {
		return fmt.Errorf("新凭据已保存，重载失败: %w", err)
	}
	if row.Status == "deleted" {
		s.RemoveAccount(acc.DBID)
		return database.ErrCodexCredentialsChanged
	}
	upstream := strings.ToLower(strings.TrimSpace(row.GetCredential("upstream_type")))
	if upstream != "" && upstream != "codex" {
		return database.ErrCodexCredentialsChanged
	}
	if !applyCodexCredentialValues(acc, row.Credentials, row.CredentialGeneration) {
		return nil
	}
	s.finishCodexRefresh(ctx, acc, row.GetCredential("codex_refresh_error"))
	s.invalidateRoutingSchedulers()
	return nil
}

func (s *Store) finishCodexRefresh(ctx context.Context, acc *Account, warning string) {
	acc.mu.Lock()
	now := time.Now()
	if acc.Status != StatusCooldown || !now.Before(acc.CooldownUtil) {
		acc.Status = StatusReady
		acc.CooldownUtil, acc.CooldownReason = time.Time{}, ""
		if acc.HealthTier == HealthTierBanned {
			acc.HealthTier = HealthTierWarm
		}
	}
	acc.ErrorMsg = warning
	acc.recomputeSchedulerLocked(s.maxConcurrency.Load())
	at, expiry, id := acc.AccessToken, acc.ExpiresAt, acc.DBID
	acc.mu.Unlock()
	if s.tokenCache != nil && time.Until(expiry) > 5*time.Minute {
		if err := s.tokenCache.SetAccessToken(ctx, id, at, time.Until(expiry)-5*time.Minute); err != nil {
			log.Printf("[账号 %d] 新凭据已保存，更新 Token 缓存失败: %v", id, err)
		}
	}
	s.fastSchedulerUpdate(acc)
}

func (s *Store) shouldBackgroundRefresh(acc *Account, codexOnly bool) bool {
	acc.mu.RLock()
	defer acc.mu.RUnlock()
	codex := !acc.isRelayStyleLocked() && !acc.isCodexAgentIdentityLocked()
	if codex && atomic.LoadInt32(&acc.Disabled) != 0 {
		return false
	}
	// Grok 的 RT 家族每次刷新都会轮换：禁用账号继续后台刷新会让已交给 Grok CLI 的
	// 凭据失效。Grok 的 DispatchPaused 只来自 enabled=false，没有额度类自动暂停。
	if acc.isGrokAPILocked() && (atomic.LoadInt32(&acc.Disabled) != 0 || atomic.LoadInt32(&acc.DispatchPaused) != 0) {
		return false
	}
	if codexOnly && !codex {
		return false
	}
	if acc.isAntigravityAPILocked() || acc.Status == StatusError || acc.healthTierLocked() == HealthTierBanned || strings.TrimSpace(acc.RefreshToken) == "" {
		return false
	}
	if time.Until(acc.ExpiresAt) >= 5*time.Minute {
		return false
	}
	if acc.Status == StatusCooldown && time.Now().Before(acc.CooldownUtil) {
		if !codex {
			return false
		}
		switch acc.CooldownReason {
		case "rate_limited", "rate_limited_5h", "rate_limited_7d", "usage_limit", "usage_limited", ResponsesRateLimitedCooldownReason:
			return true
		default:
			return false
		}
	}
	return !codex || atomic.LoadInt32(&acc.DispatchPaused) == 0
}
