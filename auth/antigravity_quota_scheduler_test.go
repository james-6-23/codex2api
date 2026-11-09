package auth

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// antigravityQuotaTestAccount 构造最小可参与的 Antigravity 运行时账号（纯内存，
// 不触库、不联网），用于 FakeClock 驱动的调度循环用例。
func antigravityQuotaTestAccount(id int64) *Account {
	return &Account{
		DBID:                 id,
		UpstreamType:         UpstreamAntigravity,
		AccessToken:          "access-token",
		RefreshToken:         "refresh-token",
		AntigravityProjectID: "project-id",
		HealthTier:           HealthTierHealthy,
	}
}

type antigravityQuotaCall struct {
	dbID int64
	at   time.Time
}

// antigravityQuotaHarness 用 FakeClock 驱动调度循环：假时钟只由测试推进，
// 因此每次刷新时观察到的 clock.Now() 完全确定，可直接断言名义槽位与退避时刻。
type antigravityQuotaHarness struct {
	t            *testing.T
	clock        *FakeClock
	store        *Store
	scheduler    *antigravityQuotaScheduler
	parkObserved func()
	parkCh       chan struct{}

	mu         sync.Mutex
	calls      []antigravityQuotaCall
	refreshErr error
	// onRefresh 模拟真实 refresh 成功后的副作用（把新观测写回账号内存快照）；在记录
	// 调用时刻之后、返回调用方之前执行，与生产 RefreshAntigravityQuotaSummary 的
	// 「网络成功 → 落库 → 投影内存」顺序一致。
	onRefresh func(*Account)
}

// newAntigravityQuotaHarness 构造默认相位的调度测试套件实例。
func newAntigravityQuotaHarness(t *testing.T, accounts ...*Account) *antigravityQuotaHarness {
	t.Helper()
	return newAntigravityQuotaHarnessWithPhase(t, time.Duration(0), false, accounts...)
}

// newAntigravityQuotaHarnessWithPhase 允许固定 theta（随机相位在本批用例里会把
// 槽位推到覆盖窗外，破坏确定性）；fixedPhase=false 时走生产构造路径。
func newAntigravityQuotaHarnessWithPhase(t *testing.T, theta time.Duration, fixedPhase bool, accounts ...*Account) *antigravityQuotaHarness {
	t.Helper()
	store := &Store{}
	store.accounts = append(store.accounts, accounts...)
	// 预热一次账号快照：循环内每次快照都走已发布的指针，不重复回落到 s.accounts。
	store.publishAccountSnapshot(append([]*Account(nil), store.accounts...))
	clock := NewFakeClock(time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC))
	h := &antigravityQuotaHarness{t: t, clock: clock, store: store, parkCh: make(chan struct{}, 64)}
	h.parkObserved = func() {
		select {
		case h.parkCh <- struct{}{}:
		default:
		}
	}
	var scheduler *antigravityQuotaScheduler
	if fixedPhase {
		scheduler = newAntigravityQuotaSchedulerWithPhase(store, clock, h.refresh, clock.Now(), theta)
	} else {
		scheduler = newAntigravityQuotaScheduler(store, clock, h.refresh)
	}
	scheduler.parkObserved = h.parkObserved
	h.scheduler = scheduler
	store.antigravityQuotaScheduler = scheduler
	return h
}

// refresh 模拟单次配额刷新操作，记录调用时标并触发可选的回调。
func (h *antigravityQuotaHarness) refresh(_ context.Context, acc *Account) error {
	h.mu.Lock()
	h.calls = append(h.calls, antigravityQuotaCall{dbID: acc.DBID, at: h.clock.Now()})
	err := h.refreshErr
	onRefresh := h.onRefresh
	h.mu.Unlock()
	if err == nil && onRefresh != nil {
		onRefresh(acc)
	}
	h.store.WakeAntigravityQuotaScheduler()
	return err
}

// setOnRefresh 注入「刷新成功后」的账号内存投影，用于复现真实 refresh 的观测时标
// 一定晚于名义槽位一个正 ε 这一事实。
func (h *antigravityQuotaHarness) setOnRefresh(fn func(*Account)) {
	h.mu.Lock()
	h.onRefresh = fn
	h.mu.Unlock()
}

// pendingSlot 读取调度器为该账号记录的挂起候选槽位。
func (h *antigravityQuotaHarness) pendingSlot(dbID int64) (time.Time, bool) {
	h.scheduler.mu.Lock()
	defer h.scheduler.mu.Unlock()
	slot, ok := h.scheduler.pending[dbID]
	return slot, ok
}

// start 启动调度循环，并在用例结束时保证循环与已派发的一次性 goroutine 全部退出。
func (h *antigravityQuotaHarness) start() {
	h.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		h.scheduler.Run(ctx)
	}()
	h.t.Cleanup(func() {
		cancel()
		h.scheduler.Stop()
		select {
		case <-stopped:
		case <-time.After(5 * time.Second):
			h.t.Error("antigravity quota scheduler did not exit after Stop")
		}
		h.scheduler.Wait()
	})
	h.settle()
}

// settle 等调度循环把当前挂起的唤醒/到点事件消费完并重新挂起（parked）。
// 时钟只由测试推进，因此循环 parked 时定时器 deadline 与名义槽位严格对齐。
func (h *antigravityQuotaHarness) settle() {
	for {
		select {
		case <-h.parkCh:
		case <-time.After(30 * time.Millisecond):
			return
		}
	}
}

// advance 以固定步长推进假时钟，每步都等循环重新挂起。
func (h *antigravityQuotaHarness) advance(steps int, step time.Duration) {
	h.t.Helper()
	for i := 0; i < steps; i++ {
		h.clock.Advance(step)
		h.settle()
	}
}

// setRefreshErr 设置后续刷新模拟调用的预期返回错误。
func (h *antigravityQuotaHarness) setRefreshErr(err error) {
	h.mu.Lock()
	h.refreshErr = err
	h.mu.Unlock()
}

// callsFor 筛选指定账号 ID 发生的所有刷新调用记录。
func (h *antigravityQuotaHarness) callsFor(dbID int64) []antigravityQuotaCall {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []antigravityQuotaCall
	for _, call := range h.calls {
		if call.dbID == dbID {
			out = append(out, call)
		}
	}
	return out
}

// calledIDs 按调用顺序返回被刷新的账号 ID 列表。
func (h *antigravityQuotaHarness) calledIDs() []int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]int64, 0, len(h.calls))
	for _, call := range h.calls {
		out = append(out, call.dbID)
	}
	return out
}

// totalCalls 返回当前记录的总刷新调用次数。
func (h *antigravityQuotaHarness) totalCalls() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.calls)
}

// TestAntigravityQuotaSchedulerEligibility 断言资格集合：只有启用中的 OAuth 且已
// 绑定 project 的账号参与巡检；禁用、暂停调度与 API-Key 账号一律不派发（Spec §5.3.1）。
func TestAntigravityQuotaSchedulerEligibility(t *testing.T) {
	healthy := antigravityQuotaTestAccount(101)
	disabled := antigravityQuotaTestAccount(102)
	paused := antigravityQuotaTestAccount(103)
	apiKey := antigravityQuotaTestAccount(104)
	apiKey.APIKey = "AIza-demo-api-key"
	apiKey.RefreshToken = ""
	apiKey.AccessToken = ""
	noProject := antigravityQuotaTestAccount(105)
	noProject.AntigravityProjectID = ""

	atomic.StoreInt32(&disabled.Disabled, 1)
	atomic.StoreInt32(&paused.DispatchPaused, 1)

	h := newAntigravityQuotaHarness(t, apiKey, paused, disabled, healthy, noProject)
	h.start()
	// 覆盖三个完整周期，任何一次错误派发都会留下记录。
	h.advance(180, time.Minute)

	if got := len(h.callsFor(healthy.DBID)); got == 0 {
		t.Fatalf("eligible OAuth account must be refreshed, calls=%d", got)
	}
	for _, id := range h.calledIDs() {
		if id != healthy.DBID {
			t.Fatalf("only the eligible OAuth account may be refreshed, got calls=%v", h.calledIDs())
		}
	}
	// 单账号资格集合 n=1：三个周期内至多三次，不得重复补发（Spec §5.3 无历史补偿）。
	if got := len(h.callsFor(healthy.DBID)); got > 3 {
		t.Fatalf("one account must be refreshed at most once per interval, got %d calls", got)
	}
}

// TestAntigravityQuotaSchedulerSpacing 断言均匀分散：N=2、I=60m 时每个账号一个
// 周期恰好一次，且同一账号相邻两次刷新严格相差一个周期。
func TestAntigravityQuotaSchedulerSpacing(t *testing.T) {
	first := antigravityQuotaTestAccount(201)
	second := antigravityQuotaTestAccount(202)

	h := newAntigravityQuotaHarness(t, first, second)
	h.start()
	h.advance(190, time.Minute)

	interval := time.Duration(ConfiguredAntigravitySettings().GetQuotaRefreshIntervalMinutes()) * time.Minute
	for _, acc := range []*Account{first, second} {
		calls := h.callsFor(acc.DBID)
		if len(calls) < 3 {
			t.Fatalf("account %d: expected at least 3 refreshes in 190m, got %d", acc.DBID, len(calls))
		}
		if len(calls) > 4 {
			t.Fatalf("account %d: expected at most one refresh per interval, got %d", acc.DBID, len(calls))
		}
		for i := 1; i < len(calls); i++ {
			gap := calls[i].at.Sub(calls[i-1].at)
			if diff := gap - interval; diff > time.Second || diff < -time.Second {
				t.Fatalf("account %d: gap between attempt %d and %d is %v, want %v", acc.DBID, i, i+1, gap, interval)
			}
		}
	}

	// 两账号槽位必须错开（Δ = I/N = 30m），不得挤在同一时刻。
	for _, a := range h.callsFor(first.DBID) {
		for _, b := range h.callsFor(second.DBID) {
			if a.at.Equal(b.at) {
				t.Fatalf("N=2 slots must be phase-shifted by Δ=I/2, both refreshed at %v", a.at)
			}
		}
	}
}

// TestAntigravityQuotaSchedulerBackoff 断言失败退避真正生效：第二次尝试不早于
// ComputeBackoff(I, 1)，失败后绝不立即重试。
func TestAntigravityQuotaSchedulerBackoff(t *testing.T) {
	acc := antigravityQuotaTestAccount(301)
	h := newAntigravityQuotaHarness(t, acc)
	h.setRefreshErr(errors.New("upstream 503"))
	h.start()

	// 等到第一次尝试出现（至多一个周期 + 一步）。
	var firstCall time.Time
	for i := 0; i < 130; i++ {
		h.clock.Advance(time.Minute)
		h.settle()
		if calls := h.callsFor(acc.DBID); len(calls) > 0 {
			firstCall = calls[0].at
			break
		}
	}
	if firstCall.IsZero() {
		t.Fatal("expected the first attempt within one interval")
	}
	// 失败后立刻推进一分钟：退避窗口内绝不允许出现第二次尝试。
	h.advance(2, time.Minute)
	if got := len(h.callsFor(acc.DBID)); got != 1 {
		t.Fatalf("must not retry immediately after a failure, calls=%d", got)
	}

	interval := time.Duration(ConfiguredAntigravitySettings().GetQuotaRefreshIntervalMinutes()) * time.Minute
	backoff := ComputeBackoff(interval, 1)
	// 推进到退避之后（留一个周期的余量）。
	for i := 0; i < 400; i++ {
		h.clock.Advance(time.Minute)
		h.settle()
		if len(h.callsFor(acc.DBID)) >= 2 {
			break
		}
	}
	calls := h.callsFor(acc.DBID)
	if len(calls) < 2 {
		t.Fatalf("expected a second attempt after the backoff window, got %d calls", len(calls))
	}
	gap := calls[1].at.Sub(calls[0].at)
	if gap < backoff {
		t.Fatalf("second attempt after %v, want >= %v (backoff for 1 failure)", gap, backoff)
	}
	if gap > backoff+2*interval {
		t.Fatalf("second attempt deferred too far: %v, backoff=%v", gap, backoff)
	}
}

// TestAntigravityQuotaSchedulerDisabledSetting 断言 quota_refresh_enabled=false
// 时调度器不派发、不联网；改回 true 并 Wake 后立即恢复。
func TestAntigravityQuotaSchedulerDisabledSetting(t *testing.T) {
	previous := ConfiguredAntigravitySettings()
	t.Cleanup(func() { SetConfiguredAntigravitySettings(previous) })

	disabled := false
	minutes := 3
	SetConfiguredAntigravitySettings(AntigravitySettings{QuotaRefreshEnabled: &disabled, QuotaRefreshIntervalMinutes: &minutes})

	acc := antigravityQuotaTestAccount(401)
	h := newAntigravityQuotaHarness(t, acc)
	h.start()
	h.advance(200, time.Minute)
	if got := h.totalCalls(); got != 0 {
		t.Fatalf("quota_refresh_enabled=false must not dispatch anything, calls=%d", got)
	}

	enabled := true
	SetConfiguredAntigravitySettings(AntigravitySettings{QuotaRefreshEnabled: &enabled, QuotaRefreshIntervalMinutes: &minutes})
	h.store.WakeAntigravityQuotaScheduler()
	h.settle()
	h.advance(200, time.Minute)
	if got := h.totalCalls(); got == 0 {
		t.Fatal("re-enabling quota refresh must resume dispatching after Wake")
	}
	if got := len(h.callsFor(acc.DBID)); got == 0 {
		t.Fatalf("expected refreshes for the eligible account, got %d", got)
	}
}

// TestAntigravityQuotaSchedulerCoverageWindow 断言启动恢复：已有观测落在当前名义
// 覆盖窗内时跳过本轮（fresh 已覆盖不重复联网），下一轮才刷新。
func TestAntigravityQuotaSchedulerCoverageWindow(t *testing.T) {
	previous := ConfiguredAntigravitySettings()
	minutes := 60
	SetConfiguredAntigravitySettings(AntigravitySettings{QuotaRefreshEnabled: ptr(true), QuotaRefreshIntervalMinutes: &minutes})
	t.Cleanup(func() { SetConfiguredAntigravitySettings(previous) })

	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	acc := antigravityQuotaTestAccount(501)
	acc.AntigravityQuota = AntigravityQuotaSnapshot{
		SchemaVersion: 2,
		SourceMetadata: map[string]SourceMeta{
			"groups": {AppliedSeq: 3, ObservedAt: now.Add(-10 * time.Minute), PresenceVerified: true},
		},
	}

	// theta=0 且 n=1 时名义槽位落在 epoch+I/2（30m）：observed_at=now-10m 落在
	// (nominalDue-I, nominalDue] = (00:00, 12:30] 覆盖窗内 -> 第一轮跳过。
	h := newAntigravityQuotaHarnessWithPhase(t, 0, true, acc)
	h.start()
	h.advance(45, time.Minute)
	if got := h.totalCalls(); got != 0 {
		t.Fatalf("fresh observation inside the coverage window must skip the round, calls=%d", got)
	}

	h.advance(45, time.Minute)
	calls := h.callsFor(acc.DBID)
	if len(calls) == 0 {
		t.Fatal("the next nominal slot after the coverage window must refresh")
	}
	if first := calls[0].at.Sub(now); first < 60*time.Minute {
		t.Fatalf("first refresh at +%v is still inside the covered slot, want > 60m", first)
	}
}

// TestAntigravityQuotaSchedulerSteadyStateGapIsOneInterval 是「覆盖窗双跳」回归用例：
// 真实 refresh 落库的观测时标取预约之后的 time.Now()，必然比名义槽位晚一个正 ε，
// 于是下一轮名义槽位的覆盖窗 (nominal₂-I, nominal₂] 一定把这份观测判成「fresh 已覆盖」。
// 若调度器照单跳过，同一份观测会被连续两轮消费，稳态退化为每 2I 才联网一次。
//
// 本用例注入与生产同行为的 refresh（观测时标 = 槽位 + 1ms），断言稳态相邻派发间隔
// 恰好一个周期 I，绝不是 2I。
func TestAntigravityQuotaSchedulerSteadyStateGapIsOneInterval(t *testing.T) {
	previous := ConfiguredAntigravitySettings()
	minutes := 60
	SetConfiguredAntigravitySettings(AntigravitySettings{QuotaRefreshEnabled: ptr(true), QuotaRefreshIntervalMinutes: &minutes})
	t.Cleanup(func() { SetConfiguredAntigravitySettings(previous) })

	first := antigravityQuotaTestAccount(601)
	second := antigravityQuotaTestAccount(602)
	h := newAntigravityQuotaHarnessWithPhase(t, 0, true, first, second)

	var seq int64
	h.setOnRefresh(func(acc *Account) {
		// 关键：观测时标晚于本次槽位一个正 ε（生产里它是 time.Now().UTC()）。
		observedAt := h.clock.Now().Add(time.Millisecond)
		seq++
		acc.mu.Lock()
		acc.AntigravityQuota = AntigravityQuotaSnapshot{
			SchemaVersion: 2,
			SourceMetadata: map[string]SourceMeta{
				"groups": {AppliedSeq: seq, ObservedAt: observedAt, PresenceVerified: true},
			},
		}
		acc.mu.Unlock()
	})

	h.start()
	// 5 个完整周期（N=2、θ=0 时槽位落在 12:15 / 12:45，15m 步进与槽位对齐）。
	h.advance(20, 15*time.Minute)

	interval := time.Duration(ConfiguredAntigravitySettings().GetQuotaRefreshIntervalMinutes()) * time.Minute
	expected := 5 * 2
	if got := h.totalCalls(); got < expected-1 || got > expected+1 {
		t.Fatalf("expected about %d refreshes in 5 intervals, got %d", expected, got)
	}
	for _, acc := range []*Account{first, second} {
		calls := h.callsFor(acc.DBID)
		if len(calls) < 4 {
			t.Fatalf("account %d: expected at least 4 refreshes in 5 intervals, got %d", acc.DBID, len(calls))
		}
		for i := 1; i < len(calls); i++ {
			gap := calls[i].at.Sub(calls[i-1].at)
			if diff := gap - interval; diff > time.Second || diff < -time.Second {
				t.Fatalf("account %d: steady-state gap between attempt %d and %d is %v, want exactly %v (double-skip regression)",
					acc.DBID, i, i+1, gap, interval)
			}
		}
	}
}

// TestAntigravityQuotaSchedulerRefreshesQuotaExhaustedAccount 断言硬门禁只挡调度、
// 绝不断巡检：weekly known_zero（可信 metadata + 未来 resetAt，门禁确实为真）的账号
// 必须继续被周期派发，否则它永远拿不到「配额已恢复」的新观测（Spec §5.3.1）。
func TestAntigravityQuotaSchedulerRefreshesQuotaExhaustedAccount(t *testing.T) {
	previous := ConfiguredAntigravitySettings()
	minutes := 60
	SetConfiguredAntigravitySettings(AntigravitySettings{
		QuotaRefreshEnabled: ptr(true), QuotaRefreshIntervalMinutes: &minutes, QuotaGateEnabled: ptr(true),
	})
	t.Cleanup(func() { SetConfiguredAntigravitySettings(previous) })

	zero := 0.0
	observed := time.Now().UTC().Add(-time.Minute)
	acc := antigravityQuotaTestAccount(701)
	acc.AntigravityQuota = AntigravityQuotaSnapshot{
		SchemaVersion:  2,
		SourceMetadata: map[string]SourceMeta{"groups": {AppliedSeq: 5, ObservedAt: observed, PresenceVerified: true}},
		Groups: []AntigravityQuotaGroup{{Buckets: []AntigravityQuotaBucket{{
			Window:            "weekly",
			RemainingFraction: &zero,
			FractionPresent:   true,
			ResetTime:         observed.Add(48 * time.Hour).Format(time.RFC3339),
			ObservedAt:        observed,
			ObservationSeq:    5,
			TTLSeconds:        3600,
		}}}},
	}
	// 先自证硬门禁确实为真，否则本用例什么也没保住。
	if blocked, _ := antigravityAccountQuotaGateBlock(acc, ""); !blocked {
		t.Fatal("fixture must carry a live known_zero hard gate")
	}

	h := newAntigravityQuotaHarnessWithPhase(t, 0, true, acc)
	h.start()
	h.advance(5, 30*time.Minute)

	calls := h.callsFor(acc.DBID)
	if len(calls) < 2 {
		t.Fatalf("a known_zero-gated account must keep being polled, got %d calls", len(calls))
	}
	interval := time.Duration(ConfiguredAntigravitySettings().GetQuotaRefreshIntervalMinutes()) * time.Minute
	for i := 1; i < len(calls); i++ {
		if gap := calls[i].at.Sub(calls[i-1].at); gap != interval {
			t.Fatalf("account %d: gap between attempt %d and %d is %v, want %v", acc.DBID, i, i+1, gap, interval)
		}
	}
}

// TestAntigravityQuotaSchedulerExcludesPermanentlyIsolatedAccount 断言 RT 谱系断裂的
// 永久失败标记（credentials 的 antigravity_permanent_refresh_error，运行时投影为
// AntigravityHardBlocked + PermanentRefreshFailures 触顶）把账号踢出资格集合：这类
// 账号无法靠再查一次 summary 恢复，派发只会空耗许可。
func TestAntigravityQuotaSchedulerExcludesPermanentlyIsolatedAccount(t *testing.T) {
	previous := ConfiguredAntigravitySettings()
	minutes := 60
	SetConfiguredAntigravitySettings(AntigravitySettings{QuotaRefreshEnabled: ptr(true), QuotaRefreshIntervalMinutes: &minutes})
	t.Cleanup(func() { SetConfiguredAntigravitySettings(previous) })

	healthy := antigravityQuotaTestAccount(801)
	isolated := antigravityQuotaTestAccount(802)
	isolated.AntigravityHardBlocked = true
	isolated.AntigravityHardBlockReason = "Antigravity token refresh failed: invalid_grant"
	isolated.PermanentRefreshFailures = permanentRefreshFailureTerminalLimit

	h := newAntigravityQuotaHarnessWithPhase(t, 0, true, healthy, isolated)
	h.start()
	// 三小时 = 三个周期，任何错误派发都会留下记录。
	h.advance(6, 30*time.Minute)

	if got := len(h.callsFor(healthy.DBID)); got < 2 {
		t.Fatalf("eligible account must keep being refreshed, got %d calls", got)
	}
	if got := h.callsFor(isolated.DBID); len(got) != 0 {
		t.Fatalf("permanently isolated account must never be dispatched, got %d calls", len(got))
	}
}

// TestAntigravityQuotaSchedulerResetsPendingOnIntervalChange 断言配置间隔变化时挂起
// 候选被清空并按新间隔重算（注释承诺「让新间隔立即生效」）。
func TestAntigravityQuotaSchedulerResetsPendingOnIntervalChange(t *testing.T) {
	previous := ConfiguredAntigravitySettings()
	defer func() { SetConfiguredAntigravitySettings(previous) }()

	ten := 10
	SetConfiguredAntigravitySettings(AntigravitySettings{QuotaRefreshEnabled: ptr(true), QuotaRefreshIntervalMinutes: &ten})

	acc := antigravityQuotaTestAccount(811)
	h := newAntigravityQuotaHarnessWithPhase(t, 0, true, acc)
	base := h.clock.Now()
	h.start()
	h.advance(4, time.Minute)

	if slot, ok := h.pendingSlot(acc.DBID); !ok || !slot.Equal(base.Add(5*time.Minute)) {
		t.Fatalf("expected the pending candidate on the 10m grid (%v), got %v (ok=%v)", base.Add(5*time.Minute), slot, ok)
	}

	twentyFive := 25
	SetConfiguredAntigravitySettings(AntigravitySettings{QuotaRefreshEnabled: ptr(true), QuotaRefreshIntervalMinutes: &twentyFive})
	h.store.WakeAntigravityQuotaScheduler()
	h.settle()

	// 新间隔 I=25m 的网格落在 epoch + 12.5m；沿用旧候选（12:05）即为未清空。
	want := base.Add(12*time.Minute + 30*time.Second)
	slot, ok := h.pendingSlot(acc.DBID)
	if !ok || !slot.Equal(want) {
		t.Fatalf("changing the interval must recompute the pending candidate onto the new grid (%v), got %v (ok=%v)", want, slot, ok)
	}

	h.advance(7, time.Minute)
	if got := h.totalCalls(); got != 0 {
		t.Fatalf("the stale 10m slot must not be dispatched after the interval changed, calls=%d", got)
	}
}

// TestAntigravityQuotaSchedulerResetsPendingOnReenable 断言 quota_refresh_enabled
// 由 false 回到 true 时挂起候选被清空：关闭期间已过期、从未派发的陈旧槽位不得在
// 启用瞬间补发一次（Spec §5.3 无历史补偿）。
func TestAntigravityQuotaSchedulerResetsPendingOnReenable(t *testing.T) {
	previous := ConfiguredAntigravitySettings()
	defer func() { SetConfiguredAntigravitySettings(previous) }()

	minutes := 10
	enabled := true
	SetConfiguredAntigravitySettings(AntigravitySettings{QuotaRefreshEnabled: &enabled, QuotaRefreshIntervalMinutes: &minutes})

	acc := antigravityQuotaTestAccount(812)
	h := newAntigravityQuotaHarnessWithPhase(t, 0, true, acc)
	h.start()
	h.advance(6, time.Minute)

	if got := len(h.callsFor(acc.DBID)); got != 1 {
		t.Fatalf("expected exactly one refresh by 12:06, got %d", got)
	}
	if slot, ok := h.pendingSlot(acc.DBID); !ok || slot.After(h.clock.Now().Add(10*time.Minute)) {
		t.Fatalf("expected a pending candidate queued while enabled, got %v (ok=%v)", slot, ok)
	}

	disabled := false
	SetConfiguredAntigravitySettings(AntigravitySettings{QuotaRefreshEnabled: &disabled, QuotaRefreshIntervalMinutes: &minutes})
	h.store.WakeAntigravityQuotaScheduler()
	h.settle()
	// 关闭期间跨过 12:15 这个挂起槽位：不得派发。
	h.advance(11, time.Minute)
	if got := len(h.callsFor(acc.DBID)); got != 1 {
		t.Fatalf("quota_refresh_enabled=false must not dispatch, got %d calls", got)
	}

	SetConfiguredAntigravitySettings(AntigravitySettings{QuotaRefreshEnabled: &enabled, QuotaRefreshIntervalMinutes: &minutes})
	h.store.WakeAntigravityQuotaScheduler()
	h.settle()

	if got := len(h.callsFor(acc.DBID)); got != 1 {
		t.Fatalf("re-enabling must not replay the stale pending slot, got %d calls", got)
	}
	slot, ok := h.pendingSlot(acc.DBID)
	if !ok || !slot.After(h.clock.Now()) {
		t.Fatalf("re-enabling must recompute the pending candidate into the future, got %v (ok=%v, now=%v)", slot, ok, h.clock.Now())
	}
}

// ptr 返回指向给定布尔值的指针辅助函数。
func ptr(v bool) *bool { return &v }

func TestAntigravityQuotaSchedulerRetainsPendingIdentityRevalidationAccount(t *testing.T) {
	previous := ConfiguredAntigravitySettings()
	minutes := 60
	SetConfiguredAntigravitySettings(AntigravitySettings{QuotaRefreshEnabled: ptr(true), QuotaRefreshIntervalMinutes: &minutes})
	t.Cleanup(func() { SetConfiguredAntigravitySettings(previous) })

	revalidating := antigravityQuotaTestAccount(805)
	revalidating.AntigravityProjectID = ""
	revalidating.AntigravityHardBlocked = true
	revalidating.AntigravityHardBlockReason = antigravityIdentityRevalidationErrorPrefix + ": Get https://www.googleapis.com/oauth2/v2/userinfo: net/http: TLS handshake timeout"
	revalidating.PermanentRefreshFailures = 0

	h := newAntigravityQuotaHarnessWithPhase(t, 0, true, revalidating)
	h.start()
	h.advance(2, 30*time.Minute)

	if got := len(h.callsFor(revalidating.DBID)); got == 0 {
		t.Fatalf("account pending identity revalidation must remain eligible for scheduled recovery, got %d calls", got)
	}
}
