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

// IsClaudeOAuth 判断账号是否为 Claude Code OAuth 账号。
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
	acc.mu.RUnlock()

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
		return nil
	} else if changed {
		acc.mu.RLock()
		rt = strings.TrimSpace(acc.RefreshToken)
		acc.mu.RUnlock()
		if rt == "" {
			return fmt.Errorf("claude refresh_token 为空")
		}
	}

	client := NewClaudeAuth(proxyURL)
	td, err := client.RefreshTokens(ctx, rt)
	if err != nil {
		return fmt.Errorf("claude token 刷新失败: %w", err)
	}
	if strings.TrimSpace(td.AccessToken) == "" {
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

	// 更新内存态与调度器。冷却中的账号保留冷却状态,仅刷新令牌。
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
	if !cooldownActive {
		acc.Status = StatusReady
		acc.CooldownUtil = time.Time{}
		acc.CooldownReason = ""
	}
	if acc.Status != StatusError {
		acc.HealthTier = HealthTierHealthy
	}
	acc.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
	acc.mu.Unlock()

	s.fastSchedulerUpdate(acc)
	if !cooldownActive && s.db != nil {
		_ = s.db.ClearError(ctx, dbID)
	}
	return nil
}
