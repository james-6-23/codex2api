package admin

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
)

var (
	errWhamDailyUsageUnavailable  = errors.New("官方用量统计不可用")
	errWhamDailyUsageUnsupported  = errors.New("codex_at 类型账号不支持官方用量统计")
	errWhamDailyUsageUnauthorized = errors.New("上游鉴权失败（401），请先刷新账号后重试")
	errWhamDailyUsageRateLimited  = errors.New("上游限流（429），请稍后重试")
)

func upstreamDailyUsageError(status int, body []byte) error {
	snippet := strings.TrimSpace(string(body))
	if len(snippet) > 200 {
		snippet = snippet[:200]
	}
	if snippet == "" {
		return fmt.Errorf("上游返回 %d", status)
	}
	return fmt.Errorf("上游返回 %d：%s", status, snippet)
}

const (
	// whamDailyUsageProbeTick 是调度检查间隔。每一跳只挑「到期」的账号刷，
	// 不是每跳全量：真实间隔由下面两个按账号状态的值决定。
	whamDailyUsageProbeTick = 30 * time.Minute

	// whamDailyUsageProbeBaseInterval 是正常账号的刷新间隔。
	whamDailyUsageProbeBaseInterval = time.Hour

	// whamDailyUsageProbeRateLimitedInterval 是限流/额度耗尽账号的刷新间隔。
	// 这类账号短期内额度不会有新消耗，没必要按小时打上游。
	whamDailyUsageProbeRateLimitedInterval = 6 * time.Hour

	// whamDailyUsageProbeConcurrency 限制并发，避免大号池同时打上游。
	whamDailyUsageProbeConcurrency = 4

	// whamDailyUsageBackfillCooldown 是列表页即时回补的最短间隔。page-stats
	// 会随翻页/前端重试反复打来，同一账号在冷却内只回补一次。
	whamDailyUsageBackfillCooldown = 2 * time.Minute

	// whamDailyUsageBackfillFailureCooldown 是回补失败后的冷却。上游拉不到
	//（401/网络/滞后）短时间内重试也不会好，按普通冷却每 2 分钟打一次只是
	// 白白消耗上游配额；失败账号交给小时级探针兜底即可。
	whamDailyUsageBackfillFailureCooldown = 15 * time.Minute

	// whamDailyUsageKeepDays 是本地快照保留期。上游只有 7 天，本地留一年足够看趋势。
	whamDailyUsageKeepDays = 365

	// whamDailyUsageMinAccountAge 是自动刷新官方结算的最短账号年龄。
	// 官方统计基本在账号产生请求后的次日才出数，导入当天拉上游只会空转。
	whamDailyUsageMinAccountAge = 24 * time.Hour
)

var whamDailyUsageProbeOnce sync.Once

// StartWhamDailyUsageProbe 周期性把官方结算用量落库。
//
// 上游 daily-workspace-usage-counts 只保留 7 天，过期即永久丢失，所以这个任务是
// 长期历史的唯一来源；漏跑超过 7 天就会留下永久空洞。
func (h *Handler) StartWhamDailyUsageProbe(ctx context.Context) {
	if h == nil || h.db == nil || h.store == nil {
		return
	}
	whamDailyUsageProbeOnce.Do(func() {
		go func() {
			// 启动后先等一会：避开进程刚起来时的账号加载与刷新高峰。
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Minute):
			}
			// lastAttempt 只在这个循环 goroutine 里读写，不需要锁。
			// 记录的是尝试时间而非成功时间：失败也等下一个整间隔再试，
			// 避免坏号每跳都打一次上游。
			lastAttempt := map[int64]time.Time{}
			h.runWhamDailyUsageProbe(ctx, lastAttempt)
			ticker := time.NewTicker(whamDailyUsageProbeTick)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					h.runWhamDailyUsageProbe(ctx, lastAttempt)
				}
			}
		}()
	})
}

// whamDailyUsageProbeIntervalFor 按账号当前状态给出刷新间隔：限流/额度耗尽的
// 账号额度短期不会变化，降频到 6 小时；其余账号按小时刷新。
func whamDailyUsageProbeIntervalFor(account *auth.Account) time.Duration {
	switch account.RuntimeStatus() {
	case "rate_limited", auth.ResponsesRateLimitedCooldownReason, "rate_limited_7d", "usage_limited", "usage_limit", "usage_exhausted":
		return whamDailyUsageProbeRateLimitedInterval
	}
	return whamDailyUsageProbeBaseInterval
}

// whamDailyUsageDueTargets 从候选账号里挑出到期该刷的，并就地维护 lastAttempt
// 计时表（记录本轮尝试、清掉已不存在的账号）。
func whamDailyUsageDueTargets(all []*auth.Account, lastAttempt map[int64]time.Time, now time.Time) []*auth.Account {
	targets := make([]*auth.Account, 0, len(all))
	current := make(map[int64]struct{}, len(all))
	for _, account := range all {
		current[account.DBID] = struct{}{}
		// 减去半个 tick 的裕量：不然 1h 间隔恰好落在 1h tick 边界上，
		// 计时误差会让账号每次都差一点点到期、实际两小时才刷一次。
		due := whamDailyUsageProbeIntervalFor(account) - whamDailyUsageProbeTick/2
		if last, ok := lastAttempt[account.DBID]; ok && now.Sub(last) < due {
			continue
		}
		lastAttempt[account.DBID] = now
		targets = append(targets, account)
	}
	// 已删除的账号从计时表清掉，防止长期运行下缓慢泄漏。
	for id := range lastAttempt {
		if _, ok := current[id]; !ok {
			delete(lastAttempt, id)
		}
	}
	return targets
}

func (h *Handler) runWhamDailyUsageProbe(ctx context.Context, lastAttempt map[int64]time.Time) {
	all := h.whamDailyUsageProbeTargets()
	if len(all) == 0 {
		return
	}
	targets := whamDailyUsageDueTargets(all, lastAttempt, time.Now())
	if len(targets) == 0 {
		return
	}
	started := time.Now()
	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		okCount   int
		failCount int
	)
	sem := make(chan struct{}, whamDailyUsageProbeConcurrency)
	for _, account := range targets {
		if ctx.Err() != nil {
			break
		}
		wg.Add(1)
		go func(acc *auth.Account) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()

			reqCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			_, err := h.syncWhamDailyUsage(reqCtx, acc)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				failCount++
				// 401 在这里不裁决账号状态：wham 侧鉴权失败不代表账号不可用
				// （与 usage 探针的既有语义一致），只记录等下一轮。
				log.Printf("官方用量快照失败 account=%d: %v", acc.DBID, err)
				return
			}
			okCount++
		}(account)
	}
	wg.Wait()

	if okCount > 0 {
		pruneCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
		if err := h.db.PruneAccountDailyUsage(pruneCtx, whamDailyUsageKeepDays); err != nil {
			log.Printf("清理官方用量快照失败: %v", err)
		}
		cancel()
	}
	log.Printf("官方用量快照完成 成功=%d 失败=%d 耗时=%s", okCount, failCount, time.Since(started).Round(time.Millisecond))
}

// enqueueWhamDailyUsageBackfill 给当前页缺少官方结算快照的账号补一次上游拉取。
// 后台小时级探针会覆盖全池，但列表打开时快照往往还是空的：page-stats 只读本地表，
// 不在这里即时回补的话，「官方结算」胶囊要等用户手动刷新或下一次探针才会出现。
func (h *Handler) enqueueWhamDailyUsageBackfill(ids []int64) {
	if h == nil || h.store == nil || len(ids) == 0 {
		return
	}
	now := time.Now()
	targets := make([]*auth.Account, 0, len(ids))
	h.whamDailyBackfillMu.Lock()
	if h.whamDailyBackfillLast == nil {
		h.whamDailyBackfillLast = map[int64]time.Time{}
	}
	if h.whamDailyBackfillInFlight == nil {
		h.whamDailyBackfillInFlight = map[int64]struct{}{}
	}
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, flying := h.whamDailyBackfillInFlight[id]; flying {
			continue
		}
		if last, ok := h.whamDailyBackfillLast[id]; ok && now.Sub(last) < whamDailyUsageBackfillCooldown {
			continue
		}
		if failedAt, ok := h.whamDailyBackfillFailedAt[id]; ok && now.Sub(failedAt) < whamDailyUsageBackfillFailureCooldown {
			continue
		}
		account := h.store.FindByID(id)
		if !whamDailyUsageAutoRefreshEligible(account, now) {
			continue
		}
		h.whamDailyBackfillInFlight[id] = struct{}{}
		h.whamDailyBackfillLast[id] = now
		targets = append(targets, account)
	}
	h.whamDailyBackfillMu.Unlock()
	if len(targets) == 0 {
		return
	}
	go h.runWhamDailyUsageBackfill(targets)
}

func whamDailyUsageBackfillEligible(account *auth.Account) bool {
	if account == nil || account.DBID <= 0 {
		return false
	}
	// WHAM is a ChatGPT-only control-plane endpoint. Claude OAuth credentials
	// belong to Anthropic Messages and must never be sent to WHAM (even though
	// they carry an access token and are relay-style accounts).
	if account.IsOpenAIResponsesAPI() || account.IsGrokAPI() || account.IsClaudeOAuth() {
		return false
	}
	if isCodexATAccount(account) {
		return false
	}
	return account.GetAccessToken() != ""
}

// isCodexATAccount 识别 at-... 形态的纯 AT 凭据。这类凭据能调用
// Codex Responses，但不具备 ChatGPT WHAM analytics 稳定需要的工作区鉴权，
// 不能把「Responses 可用」等同于「官方结算统计可用」。(issue #564)
func isCodexATAccount(account *auth.Account) bool {
	if account == nil {
		return false
	}
	return accessTokenTypeForToken(account.GetAccessToken()) == accessTokenTypeCodexAT
}

func whamDailyUsageAutoRefreshEligible(account *auth.Account, now time.Time) bool {
	if !whamDailyUsageBackfillEligible(account) {
		return false
	}
	switch account.RuntimeStatus() {
	case "unauthorized", "error":
		return false
	}
	addedAt := time.Unix(0, atomic.LoadInt64(&account.AddedAt))
	if addedAt.Unix() > 0 && now.Sub(addedAt) < whamDailyUsageMinAccountAge {
		return false
	}
	return true
}

func (h *Handler) finishWhamDailyUsageBackfill(id int64) {
	if h == nil {
		return
	}
	h.whamDailyBackfillMu.Lock()
	delete(h.whamDailyBackfillInFlight, id)
	h.whamDailyBackfillMu.Unlock()
}

// markWhamDailySynced 记录该账号至少成功同步过一次官方用量——即使上游返回
// 空数据（官方统计有滞后，或账号确实没有官方客户端消耗）。page-stats 据此
// 下发显式空态，前端停止转圈与重拉，也不再触发回补。进程内存即可：重启后
// 最多多回补一次就重新标记。
func (h *Handler) markWhamDailySynced(id int64) {
	if h == nil || id <= 0 {
		return
	}
	h.whamDailyBackfillMu.Lock()
	if h.whamDailySyncedOnce == nil {
		h.whamDailySyncedOnce = map[int64]struct{}{}
	}
	h.whamDailySyncedOnce[id] = struct{}{}
	delete(h.whamDailyBackfillFailedAt, id)
	h.whamDailyBackfillMu.Unlock()
}

func (h *Handler) whamDailySyncedOnceFor(id int64) bool {
	if h == nil {
		return false
	}
	h.whamDailyBackfillMu.Lock()
	_, ok := h.whamDailySyncedOnce[id]
	h.whamDailyBackfillMu.Unlock()
	return ok
}

func (h *Handler) markWhamDailyBackfillFailed(id int64) {
	if h == nil || id <= 0 {
		return
	}
	h.whamDailyBackfillMu.Lock()
	if h.whamDailyBackfillFailedAt == nil {
		h.whamDailyBackfillFailedAt = map[int64]time.Time{}
	}
	h.whamDailyBackfillFailedAt[id] = time.Now()
	h.whamDailyBackfillMu.Unlock()
}

func (h *Handler) runWhamDailyUsageBackfill(accounts []*auth.Account) {
	sem := make(chan struct{}, whamDailyUsageProbeConcurrency)
	var wg sync.WaitGroup
	for _, account := range accounts {
		wg.Add(1)
		go func(acc *auth.Account) {
			defer wg.Done()
			defer h.finishWhamDailyUsageBackfill(acc.DBID)
			sem <- struct{}{}
			defer func() { <-sem }()

			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if _, err := h.syncWhamDailyUsage(ctx, acc); err != nil {
				h.markWhamDailyBackfillFailed(acc.DBID)
				log.Printf("官方用量即时回补失败 account=%d: %v", acc.DBID, err)
			}
		}(account)
	}
	wg.Wait()
}

// whamDailyUsageProbeTargets 挑出该拉统计的账号：只有 OAuth 登录的 Codex 账号
// 才有 wham analytics 端点权限，codex_at、中转和 Grok 账号没有。
func (h *Handler) whamDailyUsageProbeTargets() []*auth.Account {
	if h == nil || h.store == nil {
		return nil
	}
	accounts := h.store.Accounts()
	out := make([]*auth.Account, 0, len(accounts))
	now := time.Now()
	for _, account := range accounts {
		if !whamDailyUsageAutoRefreshEligible(account, now) {
			continue
		}
		out = append(out, account)
	}
	return out
}
