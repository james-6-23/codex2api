package auth

import (
	"context"
	"log"
	"sync"
	"time"
)

// usageFramePersistJob is a snapshot of the fields needed to persist one
// frame-driven usage observation. Values are captured at enqueue time so the
// write is coherent even if newer frames arrive while it is queued.
type usageFramePersistJob struct {
	account      *Account
	dbID         int64
	observedAt   time.Time
	pct5h        float64
	reset5hAt    time.Time
	has5h        bool
	updatedAt5h  time.Time
	pct7d        float64
	reset7dAt    time.Time
	has7d        bool
	updatedAt7d  time.Time
	persistUsage bool
	planObserved bool
}

// usageFramePersistQueue coalesces frame snapshots per account and writes them
// on a worker goroutine. The upstream read path only captures and enqueues, so
// a slow or locked database cannot stall a streaming response.
type usageFramePersistQueue struct {
	mu      sync.Mutex
	pending map[int64]usageFramePersistJob
	wake    chan struct{}
}

func newUsageFramePersistQueue() *usageFramePersistQueue {
	return &usageFramePersistQueue{
		pending: make(map[int64]usageFramePersistJob),
		wake:    make(chan struct{}, 1),
	}
}

func (q *usageFramePersistQueue) start(s *Store) {
	if q == nil || s == nil || s.db == nil {
		return
	}
	s.wg.Add(1)
	go q.run(s)
}

func (q *usageFramePersistQueue) run(s *Store) {
	defer s.wg.Done()
	for {
		select {
		case <-s.stopCh:
			// Usage snapshots are a cache that WHAM can rebuild; drop pending
			// jobs instead of delaying shutdown on a blocked database.
			return
		case <-q.wake:
			for {
				select {
				case <-s.stopCh:
					return
				default:
				}
				job, ok := q.take()
				if !ok {
					break
				}
				q.persist(s, job)
			}
		}
	}
}

func (q *usageFramePersistQueue) enqueue(job usageFramePersistJob) {
	q.mu.Lock()
	if prior, ok := q.pending[job.dbID]; ok {
		if job.observedAt.Before(prior.observedAt) {
			job, prior = prior, job
		}
		job.persistUsage = job.persistUsage || prior.persistUsage
		job.planObserved = job.planObserved || prior.planObserved
	}
	q.pending[job.dbID] = job
	q.mu.Unlock()
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func (q *usageFramePersistQueue) take() (usageFramePersistJob, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for id, job := range q.pending {
		delete(q.pending, id)
		return job, true
	}
	return usageFramePersistJob{}, false
}

func (q *usageFramePersistQueue) persist(s *Store, job usageFramePersistJob) {
	if s == nil || s.db == nil || job.dbID == 0 {
		return
	}
	acc := s.FindByID(job.dbID)
	if acc == nil || acc != job.account {
		return
	}
	// Serialize with the synchronous snapshot writers and re-check inside the
	// lock: a WHAM probe or a newer frame may apply while this job waits, and
	// its write must win. Holding usageSyncMu here would stall the upstream
	// read path, so use this account's snapshot-write lock instead.
	acc.usageSnapshotPersistMu.Lock()
	if s.FindByID(job.dbID) != acc {
		acc.usageSnapshotPersistMu.Unlock()
		return
	}
	acc.mu.RLock()
	superseded := acc.usageObservedAt.After(job.observedAt)
	acc.mu.RUnlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var err error
	// Supersession fences the captured quota values, not the pending side
	// effects below, which always read current runtime state under their locks.
	if job.persistUsage && !superseded {
		switch {
		case job.has7d && job.has5h:
			err = s.db.UpdateUsageSnapshotFull(ctx, job.dbID, job.pct7d, job.reset7dAt, job.pct5h, job.reset5hAt, job.updatedAt7d, job.updatedAt5h)
		case job.has7d:
			err = s.db.UpdateCredentials(ctx, job.dbID, map[string]interface{}{
				"codex_7d_used_percent":  job.pct7d,
				"codex_7d_reset_at":      job.reset7dAt.Format(time.RFC3339),
				"codex_usage_updated_at": job.updatedAt7d.Format(time.RFC3339),
			})
		case job.has5h:
			err = s.db.UpdateUsageSnapshot5h(ctx, job.dbID, job.pct5h, job.reset5hAt, job.updatedAt5h)
		default:
			// Credits-only observations need no quota write.
		}
		if err != nil {
			// Leave the throttle marker untouched; the next frame observation sees
			// that no successful write happened and enqueues again.
			log.Printf("[账号 %d] 异步持久化用量快照失败: %v", job.dbID, err)
		} else {
			acc.MarkUsageFramePersisted(job.observedAt, job.pct5h, job.has5h, job.pct7d, job.has7d)
			acc.mu.Lock()
			if !acc.usageObservedAt.After(job.observedAt) {
				acc.usageFramePersistPending = false
			}
			acc.mu.Unlock()
		}
	}
	if err := s.persistUsageFramePlan(acc); err != nil {
		log.Printf("[账号 %d] 异步持久化套餐失败: %v", acc.DBID, err)
	}
	acc.usageSnapshotPersistMu.Unlock()
	// Credit/cooldown publication and subscription hooks own their synchronization.
	acc.mu.RLock()
	creditsValid := acc.CreditsValid
	cooldownPending := acc.usageCooldownPersistPending && acc.Status == StatusCooldown
	acc.mu.RUnlock()
	if creditsValid {
		s.persistCreditSnapshot(acc, true)
	}
	if cooldownPending {
		s.persistUsageCooldown(acc)
	}
	if job.planObserved {
		s.ClearStaleSubscriptionExpiresAt(acc)
	}
}

// EnqueueUsageFramePersist schedules a frame snapshot write off the upstream
// read path. The latest snapshot per account wins while a write is queued.
func (s *Store) EnqueueUsageFramePersist(acc *Account, observedAt time.Time) {
	s.EnqueueUsageFrameEffects(acc, observedAt, true, false)
}

// EnqueueUsageFrameEffects coalesces quota, credits, plan and cooldown writes.
// The quota dirty bit survives a newer credits-only/throttled observation so a
// superseded job cannot silently lose an already requested snapshot write.
func (s *Store) EnqueueUsageFrameEffects(acc *Account, observedAt time.Time, persistUsage, planObserved bool) {
	if s == nil || acc == nil || s.db == nil || s.usageFramePersist == nil || acc.DBID == 0 {
		return
	}
	select {
	case <-s.stopCh:
		return
	default:
	}
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	acc.mu.Lock()
	if persistUsage && (acc.UsagePercent5hValid || acc.UsagePercent7dValid) {
		acc.usageFramePersistPending = true
	}
	job := usageFramePersistJob{
		account:      acc,
		dbID:         acc.DBID,
		observedAt:   observedAt,
		pct5h:        acc.UsagePercent5h,
		reset5hAt:    acc.Reset5hAt,
		has5h:        acc.UsagePercent5hValid,
		updatedAt5h:  acc.UsageUpdatedAt5h,
		pct7d:        acc.UsagePercent7d,
		reset7dAt:    acc.Reset7dAt,
		has7d:        acc.UsagePercent7dValid,
		updatedAt7d:  acc.UsageUpdatedAt,
		persistUsage: acc.usageFramePersistPending,
		planObserved: planObserved,
	}
	acc.mu.Unlock()
	s.usageFramePersist.enqueue(job)
}

// Called with acc.usageSnapshotPersistMu held, including synchronous plan writes.
func (s *Store) persistUsageFramePlan(acc *Account) error {
	acc.mu.RLock()
	pending, plan := acc.usageFramePlanPending, acc.PlanType
	acc.mu.RUnlock()
	if !pending {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := s.db.UpdateCredentials(ctx, acc.DBID, map[string]interface{}{"plan_type": plan}); err != nil {
		return err
	}
	acc.mu.Lock()
	if acc.PlanType == plan {
		acc.usageFramePlanPending = false
	}
	acc.mu.Unlock()
	return nil
}
