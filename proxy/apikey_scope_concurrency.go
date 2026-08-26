// Package proxy: 「该 Key × 某分组/账号」的并发上限（issue #439 v2）。
//
// 与 Key 级 max_concurrency 的区别：那个在请求入口就能判定（不需要知道会用哪个账号），
// 这个只有选中账号之后才知道命中哪条 scope，所以拆成两步：
//
//  1. 选号阶段：账号过滤链里读一次当前在途数，已满则把该 scope 的账号剔除，请求自然落到
//     其它分组；候选被剔空时返回 429 并说明是并发位已满。并发是瞬时状态，不套用预算的
//     on_exhausted=reject（那会让偶发争抢直接失败），一律按"换号"处理；
//  2. 选中之后：为命中的每条 scope 占一个位，请求结束（或换号重试）时释放。
//
// 刻意做成**软上限**：过滤判定与占位之间存在竞态，高并发下可能短暂超出一两个。要做硬上限
// 就得在占位失败后回到调度循环重选账号，那需要改动 6 条转发路径的重试结构，收益不抵风险。
// 位上带截止时间，即使某条路径漏了释放也会在 scopeLeaseMaxAge 后自愈，不会永久卡死一条 scope。
package proxy

import (
	"fmt"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// scopeLeaseMaxAge 是一个位的最长存活时间。正常路径都会显式释放；这是漏释放时的兜底，
// 取值远大于任何单次请求（含长流式与 WS 轮次）。
const scopeLeaseMaxAge = 15 * time.Minute

type scopeConcurrencyLease struct {
	id       uint64
	key      scopeSkipKey
	expireAt time.Time
}

type scopeConcurrencyTracker struct {
	mu     sync.Mutex
	nextID uint64
	// inflight[scope] = leaseID -> 截止时间
	inflight map[scopeSkipKey]map[uint64]time.Time
}

// apiKeyScopeConcurrency 是进程级单例：/v1 流量可能由多个 Handler 实例服务，
// 并发计数必须共享（与 apiKeyScopeSkipStats 同理）。
var apiKeyScopeConcurrency = &scopeConcurrencyTracker{
	inflight: make(map[scopeSkipKey]map[uint64]time.Time),
}

// count 返回某 scope 当前在途数，顺带清掉过期的位。
func (t *scopeConcurrencyTracker) count(key scopeSkipKey) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.countLocked(key, time.Now())
}

func (t *scopeConcurrencyTracker) countLocked(key scopeSkipKey, now time.Time) int {
	leases := t.inflight[key]
	for id, expireAt := range leases {
		if now.After(expireAt) {
			delete(leases, id)
		}
	}
	if len(leases) == 0 {
		delete(t.inflight, key)
		return 0
	}
	return len(leases)
}

func (t *scopeConcurrencyTracker) acquire(key scopeSkipKey) scopeConcurrencyLease {
	now := time.Now()
	t.mu.Lock()
	defer t.mu.Unlock()
	t.countLocked(key, now)
	t.nextID++
	lease := scopeConcurrencyLease{id: t.nextID, key: key, expireAt: now.Add(scopeLeaseMaxAge)}
	if t.inflight[key] == nil {
		t.inflight[key] = make(map[uint64]time.Time)
	}
	t.inflight[key][lease.id] = lease.expireAt
	return lease
}

func (t *scopeConcurrencyTracker) release(lease scopeConcurrencyLease) {
	if lease.id == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	leases := t.inflight[lease.key]
	if leases == nil {
		return
	}
	delete(leases, lease.id)
	if len(leases) == 0 {
		delete(t.inflight, lease.key)
	}
}

// APIKeyScopeInflight 返回某 scope 当前在途请求数，供管理端展示。
func APIKeyScopeInflight(apiKeyID int64, scopeType string, scopeID int64) int {
	if apiKeyID <= 0 || scopeID <= 0 {
		return 0
	}
	return apiKeyScopeConcurrency.count(scopeSkipKey{apiKeyID: apiKeyID, scopeType: scopeType, scopeID: scopeID})
}

// scopeConcurrencyFull 判断某条 scope 的并发位是否已满。
func scopeConcurrencyFull(apiKeyID int64, scope database.APIKeyScopeLimit, allowance int) bool {
	if scope.MaxConcurrency <= 0 {
		return false
	}
	key := scopeSkipKey{apiKeyID: apiKeyID, scopeType: scope.ResolveScopeType(), scopeID: scope.ScopeID}
	return apiKeyScopeConcurrency.count(key) >= scope.MaxConcurrency+max(0, allowance)
}

// scopeMatchesAccount 判断账号是否落在某条 scope 上。
func scopeMatchesAccount(scope database.APIKeyScopeLimit, account *auth.Account) bool {
	if account == nil {
		return false
	}
	if scope.ResolveScopeType() == database.APIKeyScopeTypeAccount {
		return account.ID() == scope.ScopeID
	}
	return account.InAnyGroup(map[int64]struct{}{scope.ScopeID: {}})
}

// AcquireAPIKeyScopeConcurrency 为选中的账号占位。同一请求换号重试时先释放上一轮的位，
// 因为一个请求同时只会占用一个账号。
func (h *Handler) AcquireAPIKeyScopeConcurrency(c *gin.Context, account *auth.Account) {
	gate := scopeBudgetGateFromContext(c)
	if gate == nil || account == nil || len(gate.concurrencyScopes) == 0 {
		return
	}
	gate.releaseLeases()
	leases := make([]scopeConcurrencyLease, 0, len(gate.concurrencyScopes))
	for _, scope := range gate.concurrencyScopes {
		if !scopeMatchesAccount(scope, account) {
			continue
		}
		leases = append(leases, apiKeyScopeConcurrency.acquire(scopeSkipKey{
			apiKeyID:  gate.apiKeyID,
			scopeType: scope.ResolveScopeType(),
			scopeID:   scope.ScopeID,
		}))
	}
	gate.setLeases(leases)
}

// ReleaseAPIKeyScopeConcurrency 释放本次请求占用的全部并发位。转发 handler 在入口 defer 它。
func (h *Handler) ReleaseAPIKeyScopeConcurrency(c *gin.Context) {
	if gate := scopeBudgetGateFromContext(c); gate != nil {
		gate.releaseLeases()
	}
}

// scopeConcurrencyMessage 是并发位已满时给下游的说明。
func scopeConcurrencyMessage(scopeLabel string, limit int) string {
	return fmt.Sprintf("API key scope concurrency limit reached: %s allows %d concurrent request(s)", scopeLabel, limit)
}
