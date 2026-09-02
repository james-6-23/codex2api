package admin

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/google/uuid"
)

const (
	// grokProbeRunGuard 单轮探测的整体超时兜底,避免异常账号把整轮卡死。
	grokProbeRunGuard           = 15 * time.Minute
	grokMaintenancePollInterval = time.Second
	grokMaintenanceLease        = 20 * time.Minute
	// 批量与探测信号量(4)对齐:领 100 个但一次只能跑 4 个,队尾任务会在
	// 20 分钟租约内跑不到而被其它副本重复领取。
	grokMaintenanceBatchSize = 8
)

var errGrokMaintenanceProjectionIncomplete = errors.New("grok maintenance projection remains incomplete")

func grokRowUpstreamType(row *database.AccountRow) string {
	if row == nil {
		return ""
	}
	if value, ok := row.Credentials["upstream_type"].(string); ok {
		return value
	}
	return ""
}

// StartGrokStatusProbe starts two independent maintenance loops:
//   - an always-on due_at queue for control-plane facts, model catalogs, and
//     native capabilities. The queue is leased across replicas and therefore
//     does not rescan every Grok account every 30 seconds;
//   - the historical inference connectivity probe, still governed by the
//     operator's GrokProbeEnabled switch and configured interval.
//
// This separation is security-relevant: disabling generation probes must not
// let a live plan, allow_access gate, balance, or catalog remain stale forever.
func (h *Handler) StartGrokStatusProbe(ctx context.Context) {
	if h == nil || h.store == nil || h.db == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// One startup seed repairs rows created by older versions. New/changed Grok
	// accounts are enqueued by database triggers thereafter.
	h.startDBBackgroundTaskWithParent(ctx, func(ctx context.Context) {
		if err := h.db.SeedGrokMaintenanceJobs(ctx, time.Now()); err != nil {
			log.Printf("[grok-maintenance] 初始化到期任务失败: %v", err)
		}
		owner := "grok-" + uuid.NewString()
		h.runDueGrokMaintenanceJobs(ctx, owner)
		ticker := time.NewTicker(grokMaintenancePollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				h.runDueGrokMaintenanceJobs(ctx, owner)
			}
		}
	})

	// Generation/connectivity remains optional and retains the coarser settings
	// cadence. Keeping it in a separate loop prevents a disabled probe switch or
	// a slow generation attempt from suppressing freshness maintenance.
	h.startDBBackgroundTaskWithParent(ctx, func(ctx context.Context) {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		var lastRun time.Time
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			if !h.store.GrokProbeEnabled() {
				continue
			}
			interval := time.Duration(h.store.GrokProbeIntervalMinutes()) * time.Minute
			if !lastRun.IsZero() && time.Since(lastRun) < interval {
				continue
			}
			lastRun = time.Now()
			h.runGrokStatusProbe(ctx)
		}
	})
}

func (h *Handler) runDueGrokMaintenanceJobs(ctx context.Context, owner string) {
	if h == nil || h.db == nil || h.store == nil || ctx.Err() != nil {
		return
	}
	jobs, err := h.db.ClaimMaintenanceJobs(ctx, database.MaintenanceJobGrokFreshness, owner, time.Now(), grokMaintenanceLease, grokMaintenanceBatchSize)
	if err != nil {
		if !errors.Is(err, context.Canceled) {
			log.Printf("[grok-maintenance] 领取到期任务失败: %v", err)
		}
		return
	}
	if len(jobs) == 0 {
		return
	}
	started := time.Now()
	var wg sync.WaitGroup
	var mu sync.Mutex
	completed, failed := 0, 0
	for _, job := range jobs {
		job := job
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := h.runOneGrokMaintenanceJob(ctx, owner, job); err != nil {
				retryDelay := time.Minute
				if job.Attempts > 1 {
					retryDelay = time.Duration(min(job.Attempts, 5)) * time.Minute
				}
				failCtx, failCancel := context.WithTimeout(context.Background(), 10*time.Second)
				if failErr := h.db.FailMaintenanceJob(failCtx, job.EntityID, job.JobKind, owner, time.Now().Add(retryDelay), err); failErr != nil {
					log.Printf("[grok-maintenance] 记录任务失败状态出错 (账号 %d): %v", job.EntityID, failErr)
				}
				failCancel()
				mu.Lock()
				failed++
				mu.Unlock()
				return
			}
			mu.Lock()
			completed++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if completed > 0 || failed > 0 {
		log.Printf("[grok-maintenance] 到期任务完成: claimed=%d completed=%d failed=%d 耗时=%s", len(jobs), completed, failed, time.Since(started).Round(time.Millisecond))
	}
}

func (h *Handler) runOneGrokMaintenanceJob(ctx context.Context, owner string, job database.MaintenanceJob) error {
	account := h.store.FindByID(job.EntityID)
	if account == nil {
		// 本地投影可能落后于触发器入队(账号由其它副本刚创建)。只有数据库
		// 确认账号已删/非 Grok 才删任务,否则报错走退避重试等投影跟上。
		row, err := h.db.GetAccountByID(ctx, job.EntityID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if row == nil || err != nil || !strings.EqualFold(strings.TrimSpace(grokRowUpstreamType(row)), "grok") || !row.Enabled {
			return h.db.DeleteMaintenanceJob(ctx, job.EntityID, job.JobKind)
		}
		return fmt.Errorf("账号 %d 尚未投影到本地调度池,稍后重试", job.EntityID)
	}
	if !account.IsGrokAPI() {
		return h.db.DeleteMaintenanceJob(ctx, job.EntityID, job.JobKind)
	}
	if atomic.LoadInt32(&account.DispatchPaused) != 0 {
		// 运行时暂停≠删号:改期重试。数据库层 enabled=0 的任务删除由触发器
		// 负责;触发器已删行时 CompleteMaintenanceJob 返回 ErrNoRows,忽略。
		if err := h.db.CompleteMaintenanceJob(ctx, job.EntityID, job.JobKind, owner, time.Now().Add(30*time.Minute)); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		return nil
	}
	select {
	case grokImportProbeSlots <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-grokImportProbeSlots }()

	jobCtx, cancel := context.WithTimeout(ctx, grokProbeRunGuard)
	defer cancel()
	state, err := h.db.GetGrokAccountState(jobCtx, job.EntityID)
	if err != nil {
		return err
	}
	selection := grokPersistedStateRefreshSelection(account, state, time.Now())
	if !selection.empty() {
		result, syncErr := h.syncGrokAccountStateSelected(jobCtx, job.EntityID, selection)
		if syncErr != nil {
			return syncErr
		}
		state = result.State
	}
	generation := account.GetCredentialGeneration()
	if generation <= 0 {
		generation = 1
	}
	if grokGenerationNeedsCapabilityProbe(account, state, generation, time.Now()) {
		result, probeErr := h.runGrokCapabilityProbe(jobCtx, job.EntityID, false)
		if probeErr != nil {
			return probeErr
		}
		state = result.State
	}
	nextDue, err := grokNextMaintenanceDue(account, state, time.Now())
	if err != nil {
		// Persisted upstream failures deliberately expire immediately. Treating
		// that state as a successful one-second reschedule would create a tight
		// retry loop during an outage. Returning an error keeps the generic job's
		// attempt count and activates the bounded 1..5 minute backoff above.
		return err
	}
	return h.db.CompleteMaintenanceJob(jobCtx, job.EntityID, job.JobKind, owner, nextDue)
}

func grokNextMaintenanceDue(account *auth.Account, state *database.GrokAccountState, now time.Time) (time.Time, error) {
	if account == nil || state == nil {
		return time.Time{}, errGrokMaintenanceProjectionIncomplete
	}
	generation := account.GetCredentialGeneration()
	if generation <= 0 {
		generation = 1
	}
	if state.CredentialGeneration != generation {
		return time.Time{}, errGrokMaintenanceProjectionIncomplete
	}
	next := now.Add(24 * time.Hour)
	consider := func(value time.Time) bool {
		if value.IsZero() || !value.After(now) {
			return false
		}
		if value.Before(next) {
			next = value
		}
		return true
	}
	if account.GrokAuthKind() == auth.GrokAuthKindOAuth {
		for _, kind := range []string{database.GrokFactUser, database.GrokFactSettings, database.GrokFactBilling, database.GrokFactAutoTopup} {
			fact, ok := state.Facts[kind]
			if !ok || fact.CredentialGeneration != generation || !consider(fact.ExpiresAt) {
				return time.Time{}, errGrokMaintenanceProjectionIncomplete
			}
		}
	}
	origin, _ := account.GrokCredentials()
	origin = strings.TrimRight(strings.TrimSpace(origin), "/")
	catalogFound := false
	for _, catalog := range state.Catalogs {
		if catalog.Snapshot.CredentialGeneration == generation && strings.EqualFold(strings.TrimRight(strings.TrimSpace(catalog.Snapshot.Origin), "/"), origin) && catalog.Snapshot.Status == "ok" {
			catalogFound = true
			if !consider(catalog.Snapshot.ExpiresAt) {
				return time.Time{}, errGrokMaintenanceProjectionIncomplete
			}
			break
		}
	}
	if !catalogFound {
		return time.Time{}, errGrokMaintenanceProjectionIncomplete
	}
	targets, _ := grokCapabilityProbeTargets(account, state, generation)
	capabilities := make(map[string]database.GrokModelCapability, len(state.Capabilities))
	for _, capability := range state.Capabilities {
		if capability.CredentialGeneration == generation {
			capabilities[grokCapabilityProbeKey(capability.ModelID, capability.Origin, capability.Protocol)] = capability
		}
	}
	for _, target := range targets {
		for _, protocol := range []proxy.GrokProtocol{proxy.GrokProtocolResponses, proxy.GrokProtocolChatCompletions, proxy.GrokProtocolMessages} {
			capability, ok := capabilities[grokCapabilityProbeKey(target.model, target.origin, string(protocol))]
			if !ok || !consider(capability.ExpiresAt) {
				return time.Time{}, errGrokMaintenanceProjectionIncomplete
			}
		}
	}
	return next, nil
}

// grokImportProbeSlots bounds read-only post-import control-plane/catalog
// synchronization so a large archive cannot fan out thousands of requests.
var grokImportProbeSlots = make(chan struct{}, 4)

// triggerGrokUsageProbe keeps its historical import-call-site name. It first
// performs the fenced control-plane/catalog synchronization and then schedules
// the required three minimal native capability probes. The latter are bounded
// globally and serialised per account by runGrokCapabilityProbe.
func (h *Handler) triggerGrokUsageProbe(accountID int64) {
	if h == nil || h.store == nil || h.db == nil || accountID <= 0 {
		return
	}
	h.startDBBackgroundTask(func(parent context.Context) {
		select {
		case grokImportProbeSlots <- struct{}{}:
		case <-parent.Done():
			return
		}
		defer func() { <-grokImportProbeSlots }()

		syncCtx, cancel := context.WithTimeout(parent, 2*time.Minute)
		if _, err := h.syncGrokAccountState(syncCtx, accountID); err != nil {
			log.Printf("[账号 %d] 导入后 Grok 只读状态同步失败: %v", accountID, err)
			cancel()
			return
		}
		cancel()
		if _, err := h.runGrokCapabilityProbe(parent, accountID, false); err != nil {
			log.Printf("[账号 %d] 导入后 Grok 协议能力探针失败: %v", accountID, err)
		}
	})
}

func grokPersistedStateRefreshSelection(account *auth.Account, state *database.GrokAccountState, now time.Time) grokStateSyncSelection {
	if account == nil {
		return grokStateSyncSelection{}
	}
	generation := account.GetCredentialGeneration()
	if generation <= 0 {
		generation = 1
	}
	if state == nil || state.CredentialGeneration != generation {
		return fullGrokStateSyncSelection()
	}
	selection := grokStateSyncSelection{FactKinds: map[proxy.GrokControlPlaneFactKind]struct{}{}}
	if account.GrokAuthKind() == auth.GrokAuthKindOAuth {
		for _, item := range []struct {
			persisted string
			kind      proxy.GrokControlPlaneFactKind
		}{
			{database.GrokFactUser, proxy.GrokControlPlaneUser},
			{database.GrokFactSettings, proxy.GrokControlPlaneSettings},
			{database.GrokFactBilling, proxy.GrokControlPlaneBilling},
			{database.GrokFactAutoTopup, proxy.GrokControlPlaneAutoTopup},
		} {
			fact, ok := state.Facts[item.persisted]
			if !ok || fact.CredentialGeneration != state.CredentialGeneration || !now.Before(fact.ExpiresAt) {
				selection.FactKinds[item.kind] = struct{}{}
			}
		}
	}
	origin, _ := account.GrokCredentials()
	origin = strings.TrimRight(strings.TrimSpace(origin), "/")
	for _, catalog := range state.Catalogs {
		snapshot := catalog.Snapshot
		if snapshot.CredentialGeneration == state.CredentialGeneration &&
			strings.EqualFold(strings.TrimRight(strings.TrimSpace(snapshot.Origin), "/"), origin) &&
			snapshot.Status == "ok" && now.Before(snapshot.ExpiresAt) {
			return selection
		}
	}
	selection.Catalog = true
	return selection
}

func grokPersistedStateNeedsRefresh(account *auth.Account, state *database.GrokAccountState, now time.Time) bool {
	return !grokPersistedStateRefreshSelection(account, state, now).empty()
}

// refreshStaleGrokControlPlane keeps user/settings/billing/catalog snapshots at
// their declared freshness without consuming account reservations or dispatch
// counters. It is intentionally independent from GrokProbeEnabled.
func (h *Handler) refreshStaleGrokControlPlane(ctx context.Context, accounts []*auth.Account) (refreshed, failed int) {
	if h == nil || h.db == nil {
		return 0, 0
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, account := range accounts {
		if account == nil || account.ID() <= 0 {
			continue
		}
		state, err := h.db.GetGrokAccountState(ctx, account.ID())
		selection := grokPersistedStateRefreshSelection(account, state, time.Now())
		if err == nil && selection.empty() {
			continue
		}
		if err != nil {
			selection = fullGrokStateSyncSelection()
		}
		accountID := account.ID()
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case grokImportProbeSlots <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-grokImportProbeSlots }()
			syncCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			_, syncErr := h.syncGrokAccountStateSelected(syncCtx, accountID, selection)
			cancel()
			mu.Lock()
			if syncErr != nil {
				failed++
			} else {
				refreshed++
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	return refreshed, failed
}

// runGrokStatusProbe performs only the optional generation connectivity check.
func (h *Handler) runGrokStatusProbe(ctx context.Context) {
	accounts := h.store.EnabledGrokAccounts()
	if len(accounts) == 0 {
		return
	}
	probeCtx, cancel := context.WithTimeout(ctx, grokProbeRunGuard)
	defer cancel()
	start := time.Now()
	counts := h.runBatchTest(probeCtx, accounts, 0, h.runSingleBatchTest, nil)
	log.Printf("[grok-probe] 定期生成探测完成: total=%d success=%d rate_limited=%d banned=%d failed=%d 耗时=%s",
		counts.Total, counts.Success, counts.RateLimited, counts.Banned, counts.Failed, time.Since(start).Round(time.Millisecond))
}
