package auth

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

const (
	oauthRefreshLeaseNamespace = "oauth-refresh"
	oauthRefreshLeaseTTL       = 5 * time.Minute
	oauthRefreshLeaseHold      = 4 * time.Minute
	oauthRefreshLeaseWait      = 5*time.Minute + 5*time.Second
	oauthRefreshLeasePoll      = 100 * time.Millisecond
	// The legacy Grok refresher only understands the account-id refresh lock.
	// It is acquired after the family lease and held through refresh/CAS/runtime
	// publication. Its ownerless TTL outlives the family lease hold deadline.
	grokLegacyRefreshBridgeTTL = oauthRefreshLeaseHold + time.Minute
)

var oauthRefreshLeaseOwnerSequence atomic.Uint64

type oauthRefreshLocalLock struct {
	ch   chan struct{}
	refs int
}

type oauthRefreshLease struct {
	store       *Store
	fingerprint string
	local       *oauthRefreshLocalLock
	owner       string
	distributed bool
	ctx         context.Context
	cancel      context.CancelFunc
	// criticalCtx 与 ctx 共享 lease hold 期限,但不随调用方取消/超时:
	// RT 消费临界区(交换+CAS 落库)一旦开始,调用方(如管理端 10s 请求)中途
	// 取消会让上游已轮换的新 RT 永久丢失,账号只能人工重导。
	criticalCtx    context.Context
	criticalCancel context.CancelFunc
	released       atomic.Bool
}

// arm 同时创建等待用 ctx(随调用方取消)与临界区 ctx(仅受 hold 期限约束)。
func (lease *oauthRefreshLease) arm(parent context.Context) {
	lease.ctx, lease.cancel = context.WithTimeout(parent, oauthRefreshLeaseHold)
	lease.criticalCtx, lease.criticalCancel = context.WithTimeout(context.WithoutCancel(parent), oauthRefreshLeaseHold)
}

// grokLegacyRefreshBridgeLock is the rolling-upgrade bridge to Grok versions
// that only acquire TokenCache.AcquireRefreshLock(accountID). New versions take
// this lock after the stable family lease, and hold both through the credential
// CAS and runtime/cache publication. The legacy API has no owner token, so the
// TTL deliberately exceeds the maximum family-wait plus refresh-hold window.
//
// This bridge provides mutual exclusion with an old binary; it cannot make that
// binary understand credential generations or a new database fence. A strict
// "only the new version refreshes" rollout must still disable refresh on old
// instances before they overlap with the new deployment.
type grokLegacyRefreshBridgeLock struct {
	store     *Store
	accountID int64
	acquired  bool
	released  atomic.Bool
}

type grokOAuthRefreshLocks struct {
	legacy *grokLegacyRefreshBridgeLock
	family *oauthRefreshLease
}

func oauthRefreshTokenFingerprint(refreshToken string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(refreshToken)))
	return hex.EncodeToString(sum[:])
}

func (s *Store) acquireOAuthRefreshLease(ctx context.Context, refreshToken string) (*oauthRefreshLease, error) {
	if s == nil {
		return nil, fmt.Errorf("账号存储未配置")
	}
	fingerprint := oauthRefreshTokenFingerprint(refreshToken)
	if fingerprint == oauthRefreshTokenFingerprint("") {
		return nil, fmt.Errorf("refresh_token 为空")
	}

	local, err := s.acquireOAuthRefreshLocalLock(ctx, fingerprint)
	if err != nil {
		return nil, err
	}
	lease := &oauthRefreshLease{
		store:       s,
		fingerprint: fingerprint,
		local:       local,
		owner:       fmt.Sprintf("%p-%d-%d", s, time.Now().UnixNano(), oauthRefreshLeaseOwnerSequence.Add(1)),
	}
	if s.tokenCache == nil {
		lease.arm(ctx)
		return lease, nil
	}

	deadline := time.Now().Add(oauthRefreshLeaseWait)
	for {
		acquired, err := s.tokenCache.AcquireLease(
			ctx,
			oauthRefreshLeaseNamespace,
			fingerprint,
			lease.owner,
			oauthRefreshLeaseTTL,
		)
		if err != nil {
			// A shared-cache failure cannot safely degrade to the process-local
			// lock: two instances could both consume a rotating refresh token. CAS
			// would reject the second database write, but cannot undo the provider's
			// already-completed rotation. Fail closed whenever this cache represents
			// cross-instance coordination; in-memory caches remain intentionally
			// single-process and their lease errors are not expected in normal use.
			if s.tokenCache.SharedAcrossInstances() {
				lease.Release()
				return nil, fmt.Errorf("获取 OAuth 跨实例刷新 lease 失败: %w", err)
			}
			log.Printf("获取 OAuth 本地刷新 lease 失败，保留进程内锁: %v", err)
			lease.arm(ctx)
			return lease, nil
		}
		if acquired {
			lease.distributed = true
			lease.arm(ctx)
			return lease, nil
		}
		if time.Now().After(deadline) {
			lease.Release()
			return nil, fmt.Errorf("等待同一 OAuth 凭据的刷新任务超时")
		}

		timer := time.NewTimer(oauthRefreshLeasePoll)
		select {
		case <-ctx.Done():
			timer.Stop()
			lease.Release()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// acquireOAuthRefreshFamilyLease serializes Grok rotations by a stable,
// irreversible family id. Unlike an RT fingerprint, the key does not change
// when the provider rotates refresh_token.
func (s *Store) acquireOAuthRefreshFamilyLease(ctx context.Context, familyID string) (*oauthRefreshLease, error) {
	familyID = strings.TrimSpace(familyID)
	if familyID == "" {
		return nil, fmt.Errorf("credential_family_id 为空")
	}
	return s.acquireOAuthRefreshLease(ctx, "grok-family:"+familyID)
}

// acquireGrokOAuthRefreshLocks fixes the rolling-upgrade lock order as:
//
//  1. the stable credential-family lease used by current binaries;
//  2. the legacy account-id refresh lock understood by old binaries.
//
// New instances serialize on the stable family before contending with an old
// binary. After both locks are held the caller must re-read the complete stored
// credentials, because an old binary may have rotated them while it held only
// the account lock.
// Shared-cache errors fail closed: falling back to a process-local family lock
// would allow another instance to consume the same rotating refresh token.
func (s *Store) acquireGrokOAuthRefreshLocks(ctx context.Context, accountID int64, familyID string) (*grokOAuthRefreshLocks, error) {
	if s == nil {
		return nil, fmt.Errorf("账号存储未配置")
	}
	if accountID <= 0 {
		return nil, fmt.Errorf("无效的 Grok 账号 ID")
	}

	family, err := s.acquireOAuthRefreshFamilyLease(ctx, familyID)
	if err != nil {
		return nil, err
	}
	legacy := &grokLegacyRefreshBridgeLock{store: s, accountID: accountID}
	if s.tokenCache != nil {
		deadline := time.Now().Add(oauthRefreshLeaseWait)
		lockCtx := family.Context()
		for {
			acquired, err := s.tokenCache.AcquireRefreshLock(lockCtx, accountID, grokLegacyRefreshBridgeTTL)
			if err != nil {
				if s.tokenCache.SharedAcrossInstances() {
					family.Release()
					return nil, fmt.Errorf("获取 Grok 旧版兼容刷新锁失败: %w", err)
				}
				log.Printf("获取 Grok 本地旧版兼容刷新锁失败，继续使用家族进程内锁: %v", err)
				break
			}
			if acquired {
				legacy.acquired = true
				break
			}
			if time.Now().After(deadline) {
				family.Release()
				return nil, fmt.Errorf("等待账号 %d 的旧版 Grok 刷新任务超时", accountID)
			}
			timer := time.NewTimer(oauthRefreshLeasePoll)
			select {
			case <-lockCtx.Done():
				timer.Stop()
				family.Release()
				return nil, lockCtx.Err()
			case <-timer.C:
			}
		}
	}
	return &grokOAuthRefreshLocks{legacy: legacy, family: family}, nil
}

func (locks *grokOAuthRefreshLocks) Context() context.Context {
	if locks == nil || locks.family == nil {
		return context.Background()
	}
	return locks.family.Context()
}

func (locks *grokOAuthRefreshLocks) Release() {
	if locks == nil {
		return
	}
	// Release in reverse acquisition order.
	if locks.legacy != nil {
		locks.legacy.Release()
	}
	if locks.family != nil {
		locks.family.Release()
	}
}

func (lock *grokLegacyRefreshBridgeLock) Release() {
	if lock == nil || lock.store == nil || !lock.acquired || lock.released.Swap(true) {
		return
	}
	if lock.store.tokenCache == nil {
		return
	}
	releaseCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	if err := lock.store.tokenCache.ReleaseRefreshLock(releaseCtx, lock.accountID); err != nil {
		log.Printf("释放 Grok 旧版兼容刷新锁失败: %v", err)
	}
	cancel()
}

func (s *Store) acquireOAuthRefreshLocalLock(ctx context.Context, fingerprint string) (*oauthRefreshLocalLock, error) {
	s.oauthRefreshLocksMu.Lock()
	local := s.oauthRefreshLocks[fingerprint]
	if local == nil {
		local = &oauthRefreshLocalLock{ch: make(chan struct{}, 1)}
		s.oauthRefreshLocks[fingerprint] = local
	}
	local.refs++
	s.oauthRefreshLocksMu.Unlock()

	select {
	case local.ch <- struct{}{}:
		return local, nil
	case <-ctx.Done():
		s.releaseOAuthRefreshLocalLockRef(fingerprint, local)
		return nil, ctx.Err()
	}
}

func (s *Store) releaseOAuthRefreshLocalLockRef(fingerprint string, local *oauthRefreshLocalLock) {
	s.oauthRefreshLocksMu.Lock()
	local.refs--
	if local.refs == 0 && s.oauthRefreshLocks[fingerprint] == local {
		delete(s.oauthRefreshLocks, fingerprint)
	}
	s.oauthRefreshLocksMu.Unlock()
}

func (lease *oauthRefreshLease) Context() context.Context {
	if lease == nil || lease.ctx == nil {
		return context.Background()
	}
	return lease.ctx
}

// CriticalContext 返回 RT 消费临界区专用 ctx:不随调用方取消,仅受 lease hold
// 期限与 Release 约束。持有 family lease 后进入交换/落库段时必须切换到它。
func (lease *oauthRefreshLease) CriticalContext() context.Context {
	if lease == nil || lease.criticalCtx == nil {
		return context.Background()
	}
	return lease.criticalCtx
}

func (locks *grokOAuthRefreshLocks) CriticalContext() context.Context {
	if locks == nil || locks.family == nil {
		return context.Background()
	}
	return locks.family.CriticalContext()
}

func (lease *oauthRefreshLease) Release() {
	if lease == nil || lease.store == nil || lease.local == nil || lease.released.Swap(true) {
		return
	}
	if lease.distributed && lease.store.tokenCache != nil {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		if err := lease.store.tokenCache.ReleaseLease(
			releaseCtx,
			oauthRefreshLeaseNamespace,
			lease.fingerprint,
			lease.owner,
		); err != nil {
			log.Printf("释放 OAuth 跨实例刷新 lease 失败: %v", err)
		}
		cancel()
	}
	if lease.cancel != nil {
		lease.cancel()
	}
	if lease.criticalCancel != nil {
		lease.criticalCancel()
	}

	<-lease.local.ch
	lease.store.releaseOAuthRefreshLocalLockRef(lease.fingerprint, lease.local)
}

func (s *Store) reloadOAuthCredentialsAfterLock(
	ctx context.Context,
	acc *Account,
	lockedRefreshToken string,
	lockedAccessToken string,
) (changed bool, usable bool, err error) {
	if s == nil || s.db == nil || acc == nil || acc.DBID <= 0 {
		return false, false, nil
	}
	row, err := s.db.GetAccountByID(ctx, acc.DBID)
	if err != nil {
		return false, false, err
	}
	refreshToken := strings.TrimSpace(row.GetCredential("refresh_token"))
	accessToken := strings.TrimSpace(row.GetCredential("access_token"))
	refreshChanged := refreshToken != "" && refreshToken != strings.TrimSpace(lockedRefreshToken)
	accessChanged := accessToken != "" && accessToken != strings.TrimSpace(lockedAccessToken)
	if !refreshChanged && !accessChanged {
		return false, false, nil
	}

	sessionToken := strings.TrimSpace(row.GetCredential("session_token"))
	expiresAt := parseOAuthCredentialExpiry(row.GetCredential("expires_at"))

	acc.mu.Lock()
	if refreshToken != "" {
		acc.RefreshToken = refreshToken
	}
	if accessToken != "" {
		acc.AccessToken = accessToken
	}
	if sessionToken != "" {
		acc.SessionToken = sessionToken
	}
	if !expiresAt.IsZero() {
		acc.ExpiresAt = expiresAt
	}
	if accountID := strings.TrimSpace(row.GetCredential("account_id")); accountID != "" {
		acc.AccountID = accountID
	}
	if email := strings.TrimSpace(row.GetCredential("email")); email != "" {
		acc.Email = email
	}
	if planType := strings.TrimSpace(row.GetCredential("plan_type")); planType != "" {
		acc.PlanType = planType
	}
	effectiveAccessToken := acc.AccessToken
	effectiveExpiresAt := acc.ExpiresAt
	acc.mu.Unlock()

	usable = effectiveAccessToken != "" &&
		(effectiveExpiresAt.IsZero() || time.Until(effectiveExpiresAt) > 5*time.Minute)
	return true, usable, nil
}

func parseOAuthCredentialExpiry(value string) time.Time {
	value = strings.TrimSpace(value)
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if parsed, err := time.Parse(layout, value); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

// finishReloadedOAuthRefresh 在复用他处已完成的刷新结果后发布账号状态。
// 冷却判定基于当前内存状态而非调用方入口快照:等 lease 可达分钟级,期间其它
// goroutine 可能已把账号置入/延长冷却(如上游 429),那份事实更新,不能被
// 过期快照清零或回退。
func (s *Store) finishReloadedOAuthRefresh(ctx context.Context, acc *Account) {
	acc.mu.Lock()
	now := time.Now()
	activeCooldown := acc.Status == StatusCooldown && now.Before(acc.CooldownUtil)
	expiredCooldown := acc.Status == StatusCooldown && !now.Before(acc.CooldownUtil)
	if !activeCooldown {
		acc.Status = StatusReady
		acc.CooldownUtil = time.Time{}
		acc.CooldownReason = ""
	}
	acc.ErrorMsg = ""
	acc.recomputeSchedulerLocked(atomic.LoadInt64(&s.maxConcurrency))
	accessToken := acc.AccessToken
	expiresAt := acc.ExpiresAt
	dbID := acc.DBID
	acc.mu.Unlock()
	s.fastSchedulerUpdate(acc)

	if s.tokenCache != nil && accessToken != "" {
		ttl := time.Until(expiresAt) - 5*time.Minute
		if expiresAt.IsZero() {
			ttl = 30 * time.Minute
		}
		if ttl > 0 {
			_ = s.tokenCache.SetAccessToken(ctx, dbID, accessToken, ttl)
		}
	}
	if expiredCooldown {
		s.deleteCachedAccountCooldown(dbID)
		if s.db != nil {
			_ = s.db.ClearCooldown(ctx, dbID)
		}
	} else if !activeCooldown && s.db != nil {
		_ = s.db.ClearError(ctx, dbID)
	}
}
