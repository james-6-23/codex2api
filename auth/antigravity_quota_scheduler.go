package auth

import (
	"context"
	"log"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Antigravity groups summary 的周期调度器（Spec §5.3）。
//
// 单实例内以「名义槽位」把 N 个资格账号均匀铺在一个刷新周期 I 上：
//
//	T_slot(j,k) = T_epoch + k*I + ((j + 1/2)*Δ + θ) mod I，Δ = I/N
//
// θ 与 T_epoch 只在调度器创建时各确定一次：配置里的 I 变长变短、账号集合增减都只
// 让槽位重新等距，绝不重置相位——否则每次改配置都会把全部账号重新对齐到同一时刻，
// 形成周期性惊群。

// antigravityQuotaFailure 是单账号的连续失败退避状态。
type antigravityQuotaFailure struct {
	consecutive int
	notBefore   time.Time
}

// antigravityQuotaSlot 是一个账号本轮要处理的槽位：nominal 为名义槽位（结算与覆盖
// 窗判定都以它为准），due 为叠加失败退避后真正允许派发的时刻。
type antigravityQuotaSlot struct {
	account *Account
	nominal time.Time
	due     time.Time
}

type antigravityQuotaScheduler struct {
	store   *Store
	clock   Clock
	refresh func(context.Context, *Account) error // 测试注入点；nil 时用 store.RefreshAntigravityQuotaSummary

	wake     chan struct{}
	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup

	mu       sync.Mutex
	epoch    time.Time
	theta    time.Duration
	failures map[int64]*antigravityQuotaFailure
	// settled 记录每个账号最近一次结算（派发或因覆盖窗跳过）的名义槽位。
	settled map[int64]time.Time
	// pending 记录已确定、尚未结算的候选槽位：候选一经确定就不随 now 前移而重算，
	// 否则到点后的时钟过冲会让该槽位被整体跳过一整个周期。
	pending map[int64]time.Time
	// lastAutoObserved 记录每个账号最近一次「由本调度器成功派发」所产生的观测时标。
	// 覆盖窗判定必须绕开这份观测：它的 observedAt 取预约之后的 time.Now()，必然落在
	// 自己槽位之后一个正 ε，也就一定落进下一个槽位的覆盖窗；若照单认作「fresh 已覆盖」，
	// 同一份观测会被连续两轮消费，稳态退化为每 2I 才联网一次。
	lastAutoObserved map[int64]time.Time
	// lastEnabled / lastInterval 是上一轮生效的配置，用于识别「由关闭转为启用」与
	// 「间隔变化」这两种需要丢弃挂起候选的迁移（见 reconcileConfig）。
	lastEnabled  bool
	lastInterval time.Duration

	// parkObserved 仅供测试注入：主循环每次进入等待前回调一次，使 FakeClock 驱动的
	// 用例能「等循环按当前槽位武装好定时器」之后再推进时钟。
	parkObserved func()
}

// newAntigravityQuotaScheduler 创建调度器：epoch 取创建时刻，θ 取 [0, I) 的随机
// 共有相位（多实例天然错开，单实例内只整体旋转槽位）。
func newAntigravityQuotaScheduler(store *Store, clock Clock, refresh func(context.Context, *Account) error) *antigravityQuotaScheduler {
	if clock == nil {
		clock = RealClock{}
	}
	epoch := clock.Now()
	var theta time.Duration
	if interval := antigravityQuotaInterval(ConfiguredAntigravitySettings()); interval > 0 {
		theta = time.Duration(rand.Int64N(int64(interval)))
	}
	return newAntigravityQuotaSchedulerWithPhase(store, clock, refresh, epoch, theta)
}

// newAntigravityQuotaSchedulerWithPhase 是相位可注入的构造入口（测试固定 θ 以得到
// 确定的覆盖窗关系），生产路径经 newAntigravityQuotaScheduler 传入随机相位。
func newAntigravityQuotaSchedulerWithPhase(store *Store, clock Clock, refresh func(context.Context, *Account) error, epoch time.Time, theta time.Duration) *antigravityQuotaScheduler {
	if clock == nil {
		clock = RealClock{}
	}
	return &antigravityQuotaScheduler{
		store:            store,
		clock:            clock,
		refresh:          refresh,
		wake:             make(chan struct{}, 1),
		stop:             make(chan struct{}),
		epoch:            epoch,
		theta:            theta,
		failures:         make(map[int64]*antigravityQuotaFailure),
		settled:          make(map[int64]time.Time),
		pending:          make(map[int64]time.Time),
		lastAutoObserved: make(map[int64]time.Time),
	}
}

// antigravityQuotaInterval 从 Antigravity 渠道设置中提取配额刷新周期时长。
func antigravityQuotaInterval(settings AntigravitySettings) time.Duration {
	return time.Duration(settings.GetQuotaRefreshIntervalMinutes()) * time.Minute
}

// StartAntigravityQuotaScheduler 是生产入口：幂等启动调度循环并挂进 Store 生命周期。
func (s *Store) StartAntigravityQuotaScheduler() {
	if s == nil {
		return
	}
	s.antigravityQuotaSchedulerMu.Lock()
	if s.antigravityQuotaScheduler != nil {
		s.antigravityQuotaSchedulerMu.Unlock()
		return
	}
	clock := DefaultClock
	if clock == nil {
		clock = RealClock{}
	}
	scheduler := newAntigravityQuotaScheduler(s, clock, nil)
	s.antigravityQuotaScheduler = scheduler
	s.antigravityQuotaSchedulerMu.Unlock()

	backgroundCtx := s.backgroundCtx
	if backgroundCtx == nil {
		backgroundCtx = context.Background()
	}
	// 运营可观测性：启动即打印生效参数，便于区分「巡检没跑」与「巡检跑了但被开关/资格集合挡住」。
	settings := ConfiguredAntigravitySettings()
	log.Printf("[Antigravity 配额巡检] 已启动: enabled=%v interval=%s",
		settings.GetQuotaRefreshEnabled(), antigravityQuotaInterval(settings))
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		scheduler.Run(backgroundCtx)
	}()
}

// antigravityQuotaSchedulerRef 在读锁下取出调度器实例（未启动时为 nil）。
func (s *Store) antigravityQuotaSchedulerRef() *antigravityQuotaScheduler {
	if s == nil {
		return nil
	}
	s.antigravityQuotaSchedulerMu.Lock()
	defer s.antigravityQuotaSchedulerMu.Unlock()
	return s.antigravityQuotaScheduler
}

// WakeAntigravityQuotaScheduler 非阻塞唤醒调度循环重算槽位：配置变更、账号增减与
// 一次刷新收尾都走这里；缓冲区容量 1，多次唤醒合并成一次重算。
func (s *Store) WakeAntigravityQuotaScheduler() {
	scheduler := s.antigravityQuotaSchedulerRef()
	if scheduler == nil {
		return
	}
	select {
	case scheduler.wake <- struct{}{}:
	default:
	}
}

// Stop 幂等关闭调度循环。
func (s *antigravityQuotaScheduler) Stop() {
	if s == nil {
		return
	}
	s.stopOnce.Do(func() {
		// 停机即放弃全部按账号累积的调度状态（含 lastAutoObserved）：重启后的调度器
		// 是全新的相位消费者，不得带着上一世的观测记录复活。
		s.mu.Lock()
		s.failures = make(map[int64]*antigravityQuotaFailure)
		s.settled = make(map[int64]time.Time)
		s.pending = make(map[int64]time.Time)
		s.lastAutoObserved = make(map[int64]time.Time)
		s.mu.Unlock()
		close(s.stop)
	})
}

// Wait 等待所有已派发的刷新 goroutine 收尾（测试与优雅停机用）。
func (s *antigravityQuotaScheduler) Wait() {
	if s == nil {
		return
	}
	s.wg.Wait()
}

// Run 是调度主循环：每轮重新读配置并重新快照账号集合，绝不缓存过期成员。
func (s *antigravityQuotaScheduler) Run(ctx context.Context) {
	if s == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	for {
		settings := ConfiguredAntigravitySettings()
		enabled := settings.GetQuotaRefreshEnabled()
		interval := antigravityQuotaInterval(settings)
		s.reconcileConfig(enabled, interval, s.clock.Now())
		if !enabled {
			// 止血开关关闭：不派发、不联网，只等唤醒/停止（Spec §8.3）。
			if !s.park(ctx, nil) {
				return
			}
			continue
		}
		accounts := s.eligible()
		if len(accounts) == 0 {
			// 集合清空同样要丢弃按账号累积的状态，不得留待下一次快照。
			s.prunePending(nil)
			if !s.park(ctx, nil) {
				return
			}
			continue
		}
		now := s.clock.Now()
		dueAt, due := s.nextDue(accounts, interval, now)
		if dueAt.IsZero() {
			if !s.park(ctx, nil) {
				return
			}
			continue
		}
		if dueAt.After(now) {
			if !s.park(ctx, s.clock.NewTimer(dueAt.Sub(now))) {
				return
			}
			continue
		}
		s.dispatch(ctx, due, interval)
	}
}

// reconcileConfig 把调度状态与新配置对齐，只管 pending，绝不动 epoch/theta（相位
// 一经确定永不因配置变更重置）：
//   - 间隔变化：清空挂起候选，让新间隔立即生效（旧候选是按旧 I 的网格算出的）；
//   - 由关闭转为启用：丢弃关闭期间遗留的挂起候选，其中已在关闭期间过期、从未派发的
//     陈旧槽位按「已结算」处理，避免启用瞬间把它立即补发一次（Spec §5.3 无历史补偿）。
func (s *antigravityQuotaScheduler) reconcileConfig(enabled bool, interval time.Duration, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !enabled {
		s.lastEnabled = false
		s.lastInterval = interval
		return
	}
	switch {
	case !s.lastEnabled:
		for dbID, slot := range s.pending {
			if !slot.After(now) && slot.After(s.settled[dbID]) {
				s.settled[dbID] = slot
			}
			delete(s.pending, dbID)
		}
	case s.lastInterval > 0 && interval != s.lastInterval:
		for dbID := range s.pending {
			delete(s.pending, dbID)
		}
	}
	s.lastEnabled = true
	s.lastInterval = interval
}

// eligible 返回资格集合 S（Spec §5.3.1）。刻意不因硬门禁（known_zero）剔除：门禁
// 只能挡调度，不能断巡检，否则被阻断的账号永远拿不到「配额已恢复」的新观测。
// 唯一被剔除的运行时状态是 RT 谱系断裂的永久失败标记——它不可能靠再查一次 summary
// 恢复，派发只会空耗后台许可（见 antigravityAccountPermanentlyIsolatedLocked）。
func (s *antigravityQuotaScheduler) eligible() []*Account {
	if s.store == nil {
		return nil
	}
	accounts := s.store.accountSnapshotAccounts()
	eligible := make([]*Account, 0, len(accounts))
	for _, acc := range accounts {
		if acc == nil {
			continue
		}
		if acc.AntigravityAuthKind() != AntigravityAuthKindOAuth {
			continue
		}
		if atomic.LoadInt32(&acc.Disabled) != 0 || atomic.LoadInt32(&acc.DispatchPaused) != 0 {
			continue
		}
		acc.mu.RLock()
		dbID := acc.DBID
		projectID := strings.TrimSpace(acc.AntigravityProjectID)
		pendingRevalidation := acc.antigravityNeedsIdentityRevalidationLocked()
		isolated := acc.antigravityAccountPermanentlyIsolatedLocked()
		acc.mu.RUnlock()
		if dbID == 0 || (projectID == "" && !pendingRevalidation) || isolated {
			continue
		}
		eligible = append(eligible, acc)
	}
	// 稳定排序：j 即排序后下标，集合不变时槽位分配也不变。
	sort.SliceStable(eligible, func(i, j int) bool { return eligible[i].DBID < eligible[j].DBID })
	return eligible
}

// antigravityAccountPermanentlyIsolatedLocked 判断账号是否带着「永久不可恢复」的凭据
// 标记 credentials.antigravity_permanent_refresh_error（RT 谱系断裂，需管理员重建身份）。
// 运行时的 *Account 不持有 credentials 原文，装载路径把该标记投影成
// AntigravityHardBlocked + PermanentRefreshFailures 触顶（见 store.go 里
// antigravityPersistedHardFence 的两处投影），这对字段就是账号侧的等价读取口径；
// 两者同时成立才判定为永久隔离，避免把可自愈的权限/配额硬门禁一起剔除。
// 调用方必须持有该账号的读锁。
func (a *Account) antigravityAccountPermanentlyIsolatedLocked() bool {
	if a == nil {
		return false
	}
	return a.AntigravityHardBlocked && a.PermanentRefreshFailures >= permanentRefreshFailureTerminalLimit
}

// antigravityNeedsIdentityRevalidationLocked 判断账号是否因换票后 /userinfo 瞬时失败
// 处于待复验隔离状态（antigravityIdentityRevalidationErrorPrefix）。这类账号在未完成复验前
// 清空了 project_id 并挂起硬门禁以阻断线上派发，但并非 RT 谱系永久断裂，必须保留在后台巡检
// 与 ComputeBackoff 退避队列中以自动完成身份复验与恢复。调用方必须持有该账号的读锁。
func (a *Account) antigravityNeedsIdentityRevalidationLocked() bool {
	if a == nil || a.antigravityAccountPermanentlyIsolatedLocked() {
		return false
	}
	return a.AntigravityHardBlocked &&
		strings.HasPrefix(strings.TrimSpace(a.AntigravityHardBlockReason), antigravityIdentityRevalidationErrorPrefix)
}

// nextDue 计算全部资格账号的下一次候选时刻（名义槽位叠加失败退避），返回全局最早
// 的候选时刻与在该时刻到期的账号。
//
// 候选一旦确定就登记在 pending 里，直到被结算才推进：真实时钟与 FakeClock 都会在
// 到点后过冲一瞬，若每轮都按「晚于 now」重算，刚过期的槽位会被永久跳过（整个周期
// 白等）。间隔变化与重新启用由 reconcileConfig 清空 pending，账号增减由 prunePending
// 丢弃已退出集合的候选；两者都只动候选，绝不重置 epoch/theta 相位。
func (s *antigravityQuotaScheduler) nextDue(accounts []*Account, interval time.Duration, now time.Time) (time.Time, []antigravityQuotaSlot) {
	n := len(accounts)
	if n <= 0 || interval <= 0 {
		return time.Time{}, nil
	}
	slots := make([]antigravityQuotaSlot, 0, n)
	seen := make(map[int64]struct{}, n)
	for j, acc := range accounts {
		// seen 表达的是「仍在资格集合内」，与 candidate 是否算出可用槽位无关：账号还在
		// 集合里就绝不丢弃它的挂起候选与本调度器观测记录。
		seen[acc.DBID] = struct{}{}
		nominal, ok := s.candidate(acc.DBID, j, n, interval, now)
		if !ok {
			continue
		}
		due := nominal
		if at := s.backoffNotBefore(acc.DBID); at.After(due) {
			due = at
		}
		slots = append(slots, antigravityQuotaSlot{account: acc, nominal: nominal, due: due})
	}
	if len(slots) == 0 {
		s.prunePending(seen)
		return time.Time{}, nil
	}
	earliest := slots[0].due
	for _, slot := range slots[1:] {
		if slot.due.Before(earliest) {
			earliest = slot.due
		}
	}
	due := make([]antigravityQuotaSlot, 0, len(slots))
	for _, slot := range slots {
		if !slot.due.After(earliest) {
			due = append(due, slot)
		}
	}
	s.prunePending(seen)
	return earliest, due
}

// candidate 返回账号 j 下一次要结算的名义槽位：严格晚于该账号上一次结算的槽位，且
// 不回补一个完整周期之前的陈旧槽位（Spec §5.3 无历史补偿）。首次以 now 为下界，
// 因此新建调度器只等下一个未来槽位，不补发历史任务。
func (s *antigravityQuotaScheduler) candidate(dbID int64, j, n int, interval time.Duration, now time.Time) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if pending, ok := s.pending[dbID]; ok && !pending.IsZero() {
		return pending, true
	}
	floor := now.Add(-interval)
	if settled, ok := s.settled[dbID]; ok && settled.After(floor) {
		floor = settled
	}
	slot := NominalSlotTime(j, 0, interval, n, s.epoch, s.theta)
	if !slot.After(floor) {
		if delta := floor.Sub(slot) / interval; delta > 0 {
			slot = NominalSlotTime(j, int(delta), interval, n, s.epoch, s.theta)
		}
		for guard := 0; !slot.After(floor) && guard < 4; guard++ {
			slot = slot.Add(interval)
		}
	}
	if !slot.After(floor) {
		return time.Time{}, false
	}
	s.pending[dbID] = slot
	return slot, true
}

// prunePending 丢弃已退出资格集合账号的挂起候选，避免账号增减后旧候选复活；账号退出
// 集合时一并丢弃它的 lastAutoObserved 记录（重新入场时不得带着上一世的观测记录）。
func (s *antigravityQuotaScheduler) prunePending(seen map[int64]struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for dbID := range s.pending {
		if _, ok := seen[dbID]; !ok {
			delete(s.pending, dbID)
		}
	}
	for dbID := range s.lastAutoObserved {
		if _, ok := seen[dbID]; !ok {
			delete(s.lastAutoObserved, dbID)
		}
	}
}

// backoffNotBefore 返回该账号失败退避的「不早于」时刻（无失败记录时为零值）。
func (s *antigravityQuotaScheduler) backoffNotBefore(dbID int64) time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	if state := s.failures[dbID]; state != nil {
		return state.notBefore
	}
	return time.Time{}
}

// park 挂起等待到点/唤醒/停止；timer 为 nil 表示无限期等待（配置关闭或无资格账号，
// 不启动 timer）。返回 false 表示调用方应当退出循环。
//
// 入口不排空 wake：唤醒事件可能携带「刚记录的退避」，排空会让循环拿着过期候选睡
// 过退避窗口。缓冲区容量 1 已保证多次唤醒合并成一次重算。
func (s *antigravityQuotaScheduler) park(ctx context.Context, timer Timer) bool {
	if timer != nil {
		defer timer.Stop()
	}
	var fired <-chan time.Time
	if timer != nil {
		fired = timer.C()
	}
	var storeStop <-chan struct{}
	if s.store != nil {
		storeStop = s.store.stopCh
	}
	if s.parkObserved != nil {
		s.parkObserved()
	}
	select {
	case <-fired:
		return true
	case <-s.wake:
		return true
	case <-s.stop:
		return false
	case <-ctx.Done():
		return false
	case <-storeStop:
		return false
	}
}

// dispatch 对到点账号各起一个一次性 goroutine 执行刷新，并立即结算本轮名义槽位。
func (s *antigravityQuotaScheduler) dispatch(ctx context.Context, due []antigravityQuotaSlot, interval time.Duration) {
	for _, slot := range due {
		acc := slot.account
		if acc == nil {
			continue
		}
		s.settle(acc.DBID, slot.nominal)

		// 覆盖窗（启动恢复）：已有观测落在本名义槽位的覆盖窗内即视为 fresh 已覆盖，
		// 本轮跳过联网，但槽位已经结算，下一轮照常推进（验收 #14）。
		//
		// 必须排除本调度器自己刚派发产生的那份观测：它的 observedAt 取预约后的
		// time.Now()，一定晚于自己的槽位一个正 ε，因此必然落进下一个槽位的覆盖窗。
		// 若照单认作「fresh 已覆盖」，一份观测会被连续两轮消费，稳态退化成每 2I 才
		// 联网一次——巡检频率凭空减半。
		if observedAt, ok := s.observedAt(acc); ok && CheckCoverageWindow(slot.nominal, observedAt, interval) {
			if !s.isAutoObserved(acc.DBID, observedAt) {
				// 启动恢复：已有 fresh 观测覆盖本槽位，跳过联网但槽位已结算。
				// 打一行便于区分「巡检没跑」与「巡检跑了但被覆盖窗吸收」。
				log.Printf("[Antigravity 配额巡检] 账号 %d 已有覆盖本轮的观测(%s)，跳过本次联网",
					acc.DBID, observedAt.UTC().Format(time.RFC3339))
				continue
			}
		}

		refresh := s.refresh
		if refresh == nil && s.store != nil {
			refresh = s.store.RefreshAntigravityQuotaSummary
		}
		if refresh == nil {
			continue
		}
		account := acc
		// 运营可观测性：每次派发一行（正常 60min/账号，量级可控），便于确认巡检确实在跑。
		log.Printf("[Antigravity 配额巡检] 派发账号 %d（名义槽位 %s）",
			account.DBID, slot.nominal.UTC().Format(time.RFC3339))
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			err := refresh(ctx, account)
			attemptedAt := s.clock.Now()
			s.recordOutcome(account.DBID, err, interval, attemptedAt)
			if err != nil {
				// 只打一行：不带 token、不带响应正文。
				log.Printf("Antigravity 账号 %d groups summary 自动刷新失败: %v", account.DBID, err)
			} else if observedAt, ok := s.observedAt(account); ok {
				// 刷新返回 nil：观测已推进（Latest-Success-Wins 拒绝返回的 nil 也算，
				// 此时读到的仍是既有观测）。记下这份观测的时标，覆盖窗据此绝不消费它
				// 第二次。读不到观测（零值）时不记，失败路径更不记。
				s.recordAutoObserved(account.DBID, observedAt)
			}
			if s.store != nil {
				s.store.WakeAntigravityQuotaScheduler()
			}
		}()
	}
}

// isAutoObserved 判断给定的观测时标是否正是本调度器上一次成功派发产生的那一份。
func (s *antigravityQuotaScheduler) isAutoObserved(dbID int64, observedAt time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	recorded, ok := s.lastAutoObserved[dbID]
	return ok && recorded.Equal(observedAt)
}

// recordAutoObserved 记下本调度器成功派发产生的观测时标；零值不记。
func (s *antigravityQuotaScheduler) recordAutoObserved(dbID int64, observedAt time.Time) {
	if observedAt.IsZero() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastAutoObserved[dbID] = observedAt
}

// settle 记下该账号本轮名义槽位已结算：候选出队，下一轮从它之后重新取槽位。
func (s *antigravityQuotaScheduler) settle(dbID int64, nominal time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.settled[dbID] = nominal
	delete(s.pending, dbID)
}

// observedAt 读取内存快照里 groups 来源的观测时标。
func (s *antigravityQuotaScheduler) observedAt(acc *Account) (time.Time, bool) {
	acc.mu.RLock()
	defer acc.mu.RUnlock()
	meta, ok := acc.AntigravityQuota.SourceMetadata[antigravityQuotaSummarySource]
	if !ok || meta.ObservedAt.IsZero() {
		return time.Time{}, false
	}
	return meta.ObservedAt, true
}

// recordOutcome 结算一次刷新结果：失败按 ComputeBackoff 递增退避；成功（含
// Latest-Success-Wins 拒绝返回的 nil）清零。
func (s *antigravityQuotaScheduler) recordOutcome(dbID int64, err error, interval time.Duration, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		delete(s.failures, dbID)
		return
	}
	state := s.failures[dbID]
	if state == nil {
		state = &antigravityQuotaFailure{}
		s.failures[dbID] = state
	}
	state.consecutive++
	state.notBefore = at.Add(ComputeBackoff(interval, state.consecutive))
}
