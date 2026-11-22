package auth

import (
	"container/heap"
	"context"
	"log"
	"math"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"
)

// RefreshState 刷新任务状态
type RefreshState int

const (
	RefreshStatePending   RefreshState = iota // 等待执行
	RefreshStateRunning                       // 执行中
	RefreshStateSuccess                       // 成功
	RefreshStateFailed                        // 失败
	RefreshStateRetrying                      // 重试中
)

// RefreshTask 刷新任务
type RefreshTask struct {
	Account     *Account
	ScheduledAt time.Time      // 计划执行时间
	Priority    int            // 优先级（数值越大优先级越高）
	Attempts    int            // 已尝试次数
	State       RefreshState   // 当前状态
	LastError   error          // 上次错误
	NextRetryAt time.Time      // 下次重试时间
	mu          sync.RWMutex   // 任务级锁
	heapIndex   int            // 在堆中的索引，用于 heap.Remove
}

// RefreshMetrics 刷新指标（用于监控）
type RefreshMetrics struct {
	TotalTasks      atomic.Int64 // 总任务数
	PendingTasks    atomic.Int64 // 等待任务数
	RunningTasks    atomic.Int64 // 运行中任务数
	SuccessCount    atomic.Int64 // 成功次数
	FailedCount     atomic.Int64 // 失败次数
	RetryCount      atomic.Int64 // 重试次数
	AvgDurationMs   atomic.Int64 // 平均耗时（毫秒）
	LastRefreshAt   atomic.Int64 // 上次刷新时间戳
}

// RefreshConfig 刷新调度器配置
type RefreshConfig struct {
	MaxConcurrency     int           // 最大并发刷新数
	MinInterval        time.Duration // 最小刷新间隔（避免刷新风暴）
	PreExpireWindow    time.Duration // 提前刷新窗口（过期前多久开始刷新）
	RetryBackoffBase   time.Duration // 重试退避基数
	RetryMaxAttempts   int           // 最大重试次数
	JitterPercent      float64       // 抖动百分比（0-1）
	EnableBatchOptimize bool         // 启用批量优化
	BatchSize          int           // 批量大小
}

// DefaultRefreshConfig 返回默认配置
func DefaultRefreshConfig() RefreshConfig {
	return RefreshConfig{
		MaxConcurrency:     10,
		MinInterval:        100 * time.Millisecond,
		PreExpireWindow:    5 * time.Minute,
		RetryBackoffBase:   1 * time.Second,
		RetryMaxAttempts:   3,
		JitterPercent:      0.1,
		EnableBatchOptimize: true,
		BatchSize:          5,
	}
}

// taskPriorityQueue 任务优先级队列（小顶堆，ScheduledAt 越早优先级越高）
type taskPriorityQueue []*RefreshTask

func (pq taskPriorityQueue) Len() int { return len(pq) }

func (pq taskPriorityQueue) Less(i, j int) bool {
	// 优先级高的在前
	if pq[i].Priority != pq[j].Priority {
		return pq[i].Priority > pq[j].Priority
	}
	// 优先级相同，按计划时间排序
	return pq[i].ScheduledAt.Before(pq[j].ScheduledAt)
}

func (pq taskPriorityQueue) Swap(i, j int) {
	pq[i], pq[j] = pq[j], pq[i]
	pq[i].heapIndex = i
	pq[j].heapIndex = j
}

func (pq *taskPriorityQueue) Push(x interface{}) {
	task := x.(*RefreshTask)
	task.heapIndex = len(*pq)
	*pq = append(*pq, task)
}

func (pq *taskPriorityQueue) Pop() interface{} {
	old := *pq
	n := len(old)
	x := old[n-1]
	old[n-1] = nil // 避免内存泄漏
	x.heapIndex = -1
	*pq = old[0 : n-1]
	return x
}

// RefreshScheduler 智能刷新调度器
type RefreshScheduler struct {
	config     RefreshConfig
	store      *Store
	queue      chan *RefreshTask
	semaphore  chan struct{}
	pq         *taskPriorityQueue
	pqMu       sync.Mutex
	tasks      map[int64]*RefreshTask // dbID -> task
	tasksMu    sync.RWMutex
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	metrics    *RefreshMetrics
	lastBatch  time.Time
	batchMu    sync.Mutex
}

// NewRefreshScheduler 创建刷新调度器
func NewRefreshScheduler(store *Store, config RefreshConfig) *RefreshScheduler {
	if config.MaxConcurrency <= 0 {
		config.MaxConcurrency = 10
	}
	if config.BatchSize <= 0 {
		config.BatchSize = 5
	}

	ctx, cancel := context.WithCancel(context.Background())
	pq := &taskPriorityQueue{}
	heap.Init(pq)

	return &RefreshScheduler{
		config:    config,
		store:     store,
		queue:     make(chan *RefreshTask, 1000),
		semaphore: make(chan struct{}, config.MaxConcurrency),
		pq:        pq,
		tasks:     make(map[int64]*RefreshTask),
		ctx:       ctx,
		cancel:    cancel,
		metrics:   &RefreshMetrics{},
		lastBatch: time.Now(),
	}
}

// Schedule 调度一个账号刷新
func (s *RefreshScheduler) Schedule(account *Account) {
	if account == nil {
		return
	}

	s.tasksMu.Lock()
	defer s.tasksMu.Unlock()

	// 检查是否已有待处理任务
	if existing, ok := s.tasks[account.DBID]; ok {
		existing.mu.Lock()
		// 只有在非运行状态时才能重新调度
		if existing.State != RefreshStateRunning {
			existing.State = RefreshStatePending
			existing.ScheduledAt = s.calculateOptimalTime(account)
			existing.Priority = s.calculatePriority(account)
			existing.Attempts = 0
			existing.LastError = nil
			existing.mu.Unlock()
			// 任务已在堆中，pushToQueue 会使用 heap.Fix
			s.pushToQueue(existing)
		} else {
			existing.mu.Unlock()
		}
		return
	}

	// 创建新任务
	task := &RefreshTask{
		Account:     account,
		ScheduledAt: s.calculateOptimalTime(account),
		Priority:    s.calculatePriority(account),
		State:       RefreshStatePending,
		Attempts:    0,
		heapIndex:   -1, // 未入堆，-1 避免与有效索引 0 冲突
	}

	s.tasks[account.DBID] = task
	s.metrics.TotalTasks.Add(1)
	s.metrics.PendingTasks.Add(1)

	s.pushToQueue(task)
}

// ScheduleImmediate 立即调度刷新（用于紧急刷新）
func (s *RefreshScheduler) ScheduleImmediate(account *Account) {
	if account == nil {
		return
	}

	s.tasksMu.Lock()
	defer s.tasksMu.Unlock()

	task := &RefreshTask{
		Account:     account,
		ScheduledAt: time.Now(),
		Priority:    1000, // 最高优先级
		State:       RefreshStatePending,
		Attempts:    0,
		heapIndex:   -1,
	}

	// 如果已有任务，从堆中移除旧任务
	if existing, ok := s.tasks[account.DBID]; ok {
		existing.mu.Lock()
		existing.State = RefreshStateSuccess // 标记为完成，避免重复执行
		existing.mu.Unlock()
		s.removeFromQueue(existing)
		s.metrics.PendingTasks.Add(-1)
	} else {
		// 新任务才增加 TotalTasks
		s.metrics.TotalTasks.Add(1)
		s.metrics.PendingTasks.Add(1)
	}

	s.tasks[account.DBID] = task

	// 直接放入队列，不经过优先队列
	select {
	case s.queue <- task:
		// ScheduleImmediate 直接入队，不在 pq 中，所以减少 PendingTasks
		s.metrics.PendingTasks.Add(-1)
	default:
		log.Printf("[RefreshScheduler] 队列已满，任务 %d 被丢弃", account.DBID)
	}
}

// calculateOptimalTime 计算最佳刷新时间
func (s *RefreshScheduler) calculateOptimalTime(account *Account) time.Time {
	account.mu.RLock()
	expiresAt := account.ExpiresAt
	account.mu.RUnlock()

	// 基于过期时间计算
	var targetTime time.Time
	if expiresAt.IsZero() {
		targetTime = time.Now().Add(30 * time.Second)
	} else {
		timeUntilExpire := time.Until(expiresAt)
		if timeUntilExpire < s.config.PreExpireWindow {
			// 即将过期，立即刷新
			targetTime = time.Now().Add(s.randomJitter(100 * time.Millisecond))
		} else {
			// 在过期前 PreExpireWindow 刷新
			targetTime = expiresAt.Add(-s.config.PreExpireWindow)
			// 添加抖动避免集中刷新
			targetTime = targetTime.Add(s.randomJitter(s.config.MinInterval))
		}
	}

	// 确保不小于当前时间
	if targetTime.Before(time.Now()) {
		targetTime = time.Now().Add(s.randomJitter(100 * time.Millisecond))
	}

	return targetTime
}

// calculatePriority 计算任务优先级
func (s *RefreshScheduler) calculatePriority(account *Account) int {
	account.mu.RLock()
	expiresAt := account.ExpiresAt
	tier := account.HealthTier
	account.mu.RUnlock()

	priority := 0

	// 基础优先级：健康层级
	switch tier {
	case HealthTierHealthy:
		priority += 30
	case HealthTierWarm:
		priority += 20
	case HealthTierRisky:
		priority += 10
	}

	// 紧急程度：基于剩余时间
	if !expiresAt.IsZero() {
		timeUntilExpire := time.Until(expiresAt)
		switch {
		case timeUntilExpire < 30*time.Second:
			priority += 100 // 非常紧急
		case timeUntilExpire < 2*time.Minute:
			priority += 50
		case timeUntilExpire < 5*time.Minute:
			priority += 20
		}
	}

	return priority
}

// randomJitter 生成随机抖动
func (s *RefreshScheduler) randomJitter(base time.Duration) time.Duration {
	if s.config.JitterPercent <= 0 {
		return base
	}
	jitter := float64(base) * s.config.JitterPercent * (2*rand.Float64() - 1)
	return base + time.Duration(jitter)
}

// pushToQueue 推入优先队列
func (s *RefreshScheduler) pushToQueue(task *RefreshTask) {
	s.pqMu.Lock()
	defer s.pqMu.Unlock()

	// 如果任务已在堆中且索引有效，使用 heap.Fix 更新位置
	task.mu.RLock()
	idx := task.heapIndex
	task.mu.RUnlock()

	if idx >= 0 && idx < s.pq.Len() {
		heap.Fix(s.pq, idx)
	} else {
		task.mu.Lock()
		task.heapIndex = -1 // 重置索引
		task.mu.Unlock()
		heap.Push(s.pq, task)
	}
}

// removeFromQueue 从优先队列移除任务
func (s *RefreshScheduler) removeFromQueue(task *RefreshTask) {
	s.pqMu.Lock()
	defer s.pqMu.Unlock()

	task.mu.RLock()
	idx := task.heapIndex
	task.mu.RUnlock()

	if idx >= 0 && idx < s.pq.Len() {
		heap.Remove(s.pq, idx)
	}
}

// popFromQueue 从优先队列弹出
func (s *RefreshScheduler) popFromQueue() *RefreshTask {
	s.pqMu.Lock()
	defer s.pqMu.Unlock()

	if s.pq.Len() == 0 {
		return nil
	}
	return heap.Pop(s.pq).(*RefreshTask)
}

// Start 启动调度器
func (s *RefreshScheduler) Start(ctx context.Context) {
	// 使用传入的 ctx 派生内部上下文
	s.ctx, s.cancel = context.WithCancel(ctx)

	s.wg.Add(3)

	// 调度循环：从优先队列到执行队列
	go s.scheduleLoop()

	// 执行循环：处理执行队列
	go s.executeLoop()

	// 重试循环：处理失败重试
	go s.retryLoop()

	log.Printf("[RefreshScheduler] 调度器已启动，最大并发: %d", s.config.MaxConcurrency)
}

// Stop 停止调度器
func (s *RefreshScheduler) Stop() {
	s.cancel()
	s.wg.Wait()
	close(s.queue)
	log.Printf("[RefreshScheduler] 调度器已停止")
}

// scheduleLoop 调度循环：将到期的任务从优先队列移到执行队列
func (s *RefreshScheduler) scheduleLoop() {
	defer s.wg.Done()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.processScheduledTasks()
		}
	}
}

// processScheduledTasks 处理计划任务
func (s *RefreshScheduler) processScheduledTasks() {
	now := time.Now()

	for {
		task := s.peekQueue()
		if task == nil {
			break
		}

		task.mu.RLock()
		scheduledAt := task.ScheduledAt
		state := task.State
		task.mu.RUnlock()

		// 如果任务还未到执行时间，停止处理
		if scheduledAt.After(now) {
			break
		}

		// 如果任务状态不是 Pending，弹出并继续处理下一个
		if state != RefreshStatePending {
			task = s.popFromQueue()
			if task == nil {
				break
			}
			// 继续循环处理下一个任务
			continue
		}

		// 弹出任务
		task = s.popFromQueue()
		if task == nil {
			break
		}

		// 批量优化：检查是否应该批量执行
		// 注意：此函数目前仅更新 lastBatch 时间，实际批量策略待实现
		if s.config.EnableBatchOptimize && s.shouldBatch(task) {
			s.batchMu.Lock()
			s.lastBatch = time.Now()
			s.batchMu.Unlock()
		}

		// 发送到执行队列
		select {
		case s.queue <- task:
			s.metrics.PendingTasks.Add(-1)
		default:
			// 队列满，重新推入优先队列
			s.pushToQueue(task)
			return
		}
	}
}

// peekQueue 查看队列头部（不移除）
func (s *RefreshScheduler) peekQueue() *RefreshTask {
	s.pqMu.Lock()
	defer s.pqMu.Unlock()

	if s.pq.Len() == 0 {
		return nil
	}
	return (*s.pq)[0]
}

// shouldBatch 判断是否应该批量执行
func (s *RefreshScheduler) shouldBatch(task *RefreshTask) bool {
	s.batchMu.Lock()
	defer s.batchMu.Unlock()

	// 距离上次批量执行时间过短，建议等待
	if time.Since(s.lastBatch) < s.config.MinInterval {
		return true
	}
	return false
}

// executeLoop 执行循环
func (s *RefreshScheduler) executeLoop() {
	defer s.wg.Done()

	for {
		select {
		case <-s.ctx.Done():
			return
		case task, ok := <-s.queue:
			if !ok {
				return
			}
			s.executeTask(task)
		}
	}
}

// executeTask 执行任务
func (s *RefreshScheduler) executeTask(task *RefreshTask) {
	// 获取信号量
	select {
	case s.semaphore <- struct{}{}:
	case <-s.ctx.Done():
		return
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		defer func() { <-s.semaphore }()

		start := time.Now()
		s.metrics.RunningTasks.Add(1)

		task.mu.Lock()
		task.State = RefreshStateRunning
		task.mu.Unlock()

		// 执行刷新，带超时控制
		ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
		err := s.store.refreshAccount(ctx, task.Account)
		cancel()

		duration := time.Since(start)
		s.updateAvgDuration(duration)
		s.metrics.RunningTasks.Add(-1)
		s.metrics.LastRefreshAt.Store(time.Now().Unix())

		task.mu.Lock()
		if err != nil {
			task.State = RefreshStateFailed
			task.LastError = err
			task.Attempts++
			s.metrics.FailedCount.Add(1)

			// 判断是否可重试
			if task.Attempts < s.config.RetryMaxAttempts && !isNonRetryable(err) {
				task.State = RefreshStateRetrying
				task.NextRetryAt = s.calculateRetryTime(task.Attempts)
				log.Printf("[RefreshScheduler] 账号 %d 刷新失败，将在 %v 后重试: %v",
					task.Account.DBID, task.NextRetryAt.Sub(time.Now()), err)
			} else {
				log.Printf("[RefreshScheduler] 账号 %d 刷新最终失败: %v", task.Account.DBID, err)
			}
		} else {
			task.State = RefreshStateSuccess
			s.metrics.SuccessCount.Add(1)
			log.Printf("[RefreshScheduler] 账号 %d 刷新成功，耗时 %v", task.Account.DBID, duration)
		}
		task.mu.Unlock()

		// 清理已完成的任务
		if task.State == RefreshStateSuccess || (task.State == RefreshStateFailed && task.Attempts >= s.config.RetryMaxAttempts) {
			s.tasksMu.Lock()
			delete(s.tasks, task.Account.DBID)
			s.tasksMu.Unlock()
		}
	}()
}

// calculateRetryTime 计算重试时间
func (s *RefreshScheduler) calculateRetryTime(attempts int) time.Time {
	// 指数退避：base * 2^attempts + jitter
	backoff := s.config.RetryBackoffBase * time.Duration(math.Pow(2, float64(attempts-1)))
	if backoff > 5*time.Minute {
		backoff = 5 * time.Minute
	}
	return time.Now().Add(backoff + s.randomJitter(backoff/10))
}

// updateAvgDuration 更新平均耗时
func (s *RefreshScheduler) updateAvgDuration(d time.Duration) {
	current := s.metrics.AvgDurationMs.Load()
	newMs := d.Milliseconds()
	if current == 0 {
		s.metrics.AvgDurationMs.Store(newMs)
	} else {
		// 指数移动平均
		avg := (current*9 + newMs) / 10
		s.metrics.AvgDurationMs.Store(avg)
	}
}

// retryLoop 重试循环
func (s *RefreshScheduler) retryLoop() {
	defer s.wg.Done()

	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.processRetries()
		}
	}
}

// processRetries 处理重试
func (s *RefreshScheduler) processRetries() {
	now := time.Now()

	s.tasksMu.RLock()
	tasks := make([]*RefreshTask, 0)
	for _, task := range s.tasks {
		task.mu.RLock()
		if task.State == RefreshStateRetrying && task.NextRetryAt.Before(now) {
			tasks = append(tasks, task)
		}
		task.mu.RUnlock()
	}
	s.tasksMu.RUnlock()

	for _, task := range tasks {
		task.mu.Lock()
		task.State = RefreshStatePending
		task.ScheduledAt = now
		task.mu.Unlock()

		s.metrics.RetryCount.Add(1)
		s.metrics.PendingTasks.Add(1) // 重试任务重新入队时增加 PendingTasks
		s.pushToQueue(task)
		log.Printf("[RefreshScheduler] 账号 %d 开始第 %d 次重试", task.Account.DBID, task.Attempts)
	}
}

// GetMetrics 获取当前指标
func (s *RefreshScheduler) GetMetrics() RefreshMetricsSnapshot {
	return RefreshMetricsSnapshot{
		TotalTasks:    s.metrics.TotalTasks.Load(),
		PendingTasks:  s.metrics.PendingTasks.Load(),
		RunningTasks:  s.metrics.RunningTasks.Load(),
		SuccessCount:  s.metrics.SuccessCount.Load(),
		FailedCount:   s.metrics.FailedCount.Load(),
		RetryCount:    s.metrics.RetryCount.Load(),
		AvgDurationMs: s.metrics.AvgDurationMs.Load(),
		LastRefreshAt: time.Unix(s.metrics.LastRefreshAt.Load(), 0),
		QueueSize:     len(s.queue),
	}
}

// RefreshMetricsSnapshot 刷新指标快照
type RefreshMetricsSnapshot struct {
	TotalTasks    int64     `json:"total_tasks"`
	PendingTasks  int64     `json:"pending_tasks"`
	RunningTasks  int64     `json:"running_tasks"`
	SuccessCount  int64     `json:"success_count"`
	FailedCount   int64     `json:"failed_count"`
	RetryCount    int64     `json:"retry_count"`
	AvgDurationMs int64     `json:"avg_duration_ms"`
	LastRefreshAt time.Time `json:"last_refresh_at"`
	QueueSize     int       `json:"queue_size"`
}

// ScheduleBatch 批量调度刷新
func (s *RefreshScheduler) ScheduleBatch(accounts []*Account) {
	if len(accounts) == 0 {
		return
	}

	// 添加抖动避免集中刷新
	batchSize := s.config.BatchSize
	if batchSize <= 0 {
		batchSize = 5
	}

	for i, acc := range accounts {
		// 分批添加延迟
		batchDelay := time.Duration(i/batchSize) * s.config.MinInterval
		if batchDelay > 0 {
			account := acc // 创建局部变量副本，避免闭包捕获循环变量
			time.AfterFunc(batchDelay, func() {
				s.Schedule(account)
			})
		} else {
			s.Schedule(acc)
		}
	}

	log.Printf("[RefreshScheduler] 批量调度 %d 个账号刷新", len(accounts))
}

// CancelTask 取消指定账号的刷新任务
func (s *RefreshScheduler) CancelTask(dbID int64) bool {
	s.tasksMu.Lock()
	defer s.tasksMu.Unlock()

	if task, ok := s.tasks[dbID]; ok {
		task.mu.Lock()
		if task.State == RefreshStatePending || task.State == RefreshStateRetrying {
			task.State = RefreshStateSuccess // 标记为完成，跳过执行
			task.mu.Unlock()
			// 从堆中移除任务
			s.removeFromQueue(task)
			delete(s.tasks, dbID)
			return true
		}
		task.mu.Unlock()
	}
	return false
}

// GetTaskStatus 获取任务状态
func (s *RefreshScheduler) GetTaskStatus(dbID int64) (RefreshState, int, error) {
	s.tasksMu.RLock()
	task, ok := s.tasks[dbID]
	s.tasksMu.RUnlock()

	if !ok {
		return RefreshStateSuccess, 0, nil // 无任务表示已完成
	}

	task.mu.RLock()
	state := task.State
	attempts := task.Attempts
	lastErr := task.LastError
	task.mu.RUnlock()

	return state, attempts, lastErr
}
