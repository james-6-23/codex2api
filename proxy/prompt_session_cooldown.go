package proxy

import (
	"context"
	"log"
	"math"
	"sort"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const sessionCooldownReceiptKey = "session_creation_cooldown_receipt"

type sessionCooldownReceipt struct {
	Subject      string
	Root         string
	Lease        string
	CreatedAt    int64
	WindowExpiry time.Time
	Successful   bool
	Stop         chan struct{}
	Done         chan struct{}
}

func cooldownReceipt(c *gin.Context) *sessionCooldownReceipt {
	if c == nil {
		return nil
	}
	value, _ := c.Get(sessionCooldownReceiptKey)
	receipt, _ := value.(*sessionCooldownReceipt)
	return receipt
}

func pruneSessionCooldown(state *database.SessionCooldownState, now time.Time, retention time.Duration) {
	for key, root := range state.Roots {
		if root == nil {
			delete(state.Roots, key)
			continue
		}
		for lease, expiresAt := range root.Leases {
			if expiresAt <= now.UnixMilli() {
				delete(root.Leases, lease)
			}
		}
		if len(root.Leases) == 0 && (!root.Confirmed || root.CreatedAt <= now.Add(-retention).UnixMilli()) {
			delete(state.Roots, key)
		}
	}
}

func sessionCooldownRecovery(state *database.SessionCooldownState, cfg promptfilter.SessionCreationCooldownConfig, interval int, now time.Time) (time.Time, int) {
	var creations []int64
	for _, root := range state.Roots {
		if root != nil && (root.Confirmed || len(root.Leases) > 0) && root.CreatedAt > now.Add(-time.Duration(cfg.FrequencyWindowSeconds)*time.Second).UnixMilli() {
			creations = append(creations, root.CreatedAt)
		}
	}
	if interval <= 0 || len(creations) < cfg.FreeCreations {
		return time.Time{}, len(creations)
	}
	sort.Slice(creations, func(left, right int) bool { return creations[left] < creations[right] })
	spacingRecovery := time.UnixMilli(creations[len(creations)-1]).Add(time.Duration(interval) * time.Second)
	frequencyRecovery := time.UnixMilli(creations[len(creations)-cfg.FreeCreations]).Add(time.Duration(cfg.FrequencyWindowSeconds) * time.Second)
	if frequencyRecovery.Before(spacingRecovery) {
		spacingRecovery = frequencyRecovery
	}
	if !spacingRecovery.After(now) {
		return time.Time{}, len(creations)
	}
	return spacingRecovery, len(creations)
}

func (h *Handler) sessionCooldownAverage(c *gin.Context, identity verifiedNewAPIPolicyContext, cfg promptfilter.SessionCreationCooldownConfig, now time.Time) (float64, int, error) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), time.Second)
	defer cancel()
	samples, err := h.db.SessionCooldownSamples(ctx, identity.Platform, identity.Identity.UserID, now.Add(-time.Duration(cfg.HistoryDays)*24*time.Hour), now, cfg.MaxSamples+1000)
	if err != nil {
		return 0, 0, err
	}
	activeByAccount := make(map[int64]map[string]bool)
	total, count := 0.0, 0
	for _, sample := range samples {
		if _, loaded := activeByAccount[sample.AccountID]; !loaded {
			activeByAccount[sample.AccountID] = h.store.ActiveAccountUsagePeriods(sample.AccountID, now)
		}
		if activeByAccount[sample.AccountID][sample.PeriodID] {
			continue
		}
		total += math.Round(sample.DurationSeconds*1000) / 1000
		count++
		if count >= cfg.MaxSamples {
			break
		}
	}
	if count == 0 {
		return 0, 0, nil
	}
	return total / float64(count), count, nil
}

func (h *Handler) reserveSessionCooldown(c *gin.Context, identity verifiedNewAPIPolicyContext, cfg promptfilter.SessionCreationCooldownConfig, status *promptSessionCreationLimitStatus, existing, readOnly bool, expiresAt, now time.Time) bool {
	if h.db == nil || h.store == nil || cfg.Mode == "off" || cfg.Validate() != nil || cooldownReceipt(c) != nil {
		return false
	}
	average, samples := 0.0, 0
	if !existing {
		var err error
		average, samples, err = h.sessionCooldownAverage(c, identity, cfg, now)
		if err != nil {
			log.Printf("event=session_creation_cooldown_unavailable subject=%s err=%v", status.Subject, err)
			return false
		}
	}
	interval := cfg.Interval(average, samples)
	receipt := &sessionCooldownReceipt{Subject: status.Subject, Root: status.SessionHash, Lease: uuid.NewString(), WindowExpiry: expiresAt}
	var recovery time.Time
	var recent int
	reserved := false
	ctx, cancel := context.WithTimeout(c.Request.Context(), time.Second)
	err := h.db.UpdateSessionCooldown(ctx, status.Subject, func(state *database.SessionCooldownState) error {
		if !existing {
			state.AverageSeconds, state.Samples, state.EvaluatedAt = average, samples, now
		}
		retention := time.Duration(max(cfg.FrequencyWindowSeconds, cfg.MaxIntervalSeconds, status.WindowSeconds)) * time.Second
		pruneSessionCooldown(state, now, retention)
		root := state.Roots[status.SessionHash]
		if root != nil && root.Confirmed {
			return nil
		}
		if root == nil {
			if !existing {
				recovery, recent = sessionCooldownRecovery(state, cfg, interval, now)
			}
			if readOnly || (!recovery.IsZero() && cfg.Mode == "enforce") {
				return nil
			}
			createdAt := now.UnixMilli()
			if existing {
				if status.WindowCreatedAt.IsZero() {
					return nil
				}
				createdAt = status.WindowCreatedAt.UnixMilli()
			}
			root = &database.SessionCooldownRoot{CreatedAt: createdAt, PreserveWindow: existing}
			state.Roots[status.SessionHash] = root
		}
		if readOnly {
			return nil
		}
		if root.Leases == nil {
			root.Leases = make(map[string]int64)
		}
		root.Leases[receipt.Lease] = now.Add(2 * time.Minute).UnixMilli()
		receipt.CreatedAt = root.CreatedAt
		reserved = true
		return nil
	})
	cancel()
	if err != nil {
		log.Printf("event=session_creation_cooldown_unavailable subject=%s err=%v", status.Subject, err)
		return false
	}
	if !recovery.IsZero() {
		log.Printf("event=session_creation_cooldown mode=%s subject=%s root=%s average_seconds=%.2f samples=%d recent_creations=%d interval_seconds=%d available_at=%s", cfg.Mode, status.Subject, status.SessionHash, average, samples, recent, interval, recovery.UTC().Format(time.RFC3339))
		if cfg.Mode == "enforce" {
			status.Cooldown = true
			if !readOnly || recovery.After(status.NextRecoveryAt) {
				status.NextRecoveryAt = recovery
			}
			status.RetryAfter = max(1, int((status.NextRecoveryAt.Sub(now)+time.Second-1)/time.Second))
			return true
		}
	}
	if reserved {
		receipt.Stop = make(chan struct{})
		receipt.Done = make(chan struct{})
		c.Set(sessionCooldownReceiptKey, receipt)
		go h.maintainSessionCooldownLease(receipt)
	}
	return false
}

func (h *Handler) maintainSessionCooldownLease(receipt *sessionCooldownReceipt) {
	defer close(receipt.Done)
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-receipt.Stop:
			return
		case now := <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			err := h.db.UpdateSessionCooldown(ctx, receipt.Subject, func(state *database.SessionCooldownState) error {
				if root := state.Roots[receipt.Root]; root != nil && root.CreatedAt == receipt.CreatedAt {
					if _, exists := root.Leases[receipt.Lease]; exists {
						root.Leases[receipt.Lease] = now.Add(2 * time.Minute).UnixMilli()
					}
				}
				return nil
			})
			cancel()
			if err != nil {
				log.Printf("event=session_creation_cooldown_lease_error subject=%s err=%v", receipt.Subject, err)
			}
		}
	}
}

func (h *Handler) finishSessionCooldown(c *gin.Context) {
	receipt := cooldownReceipt(c)
	if receipt == nil {
		return
	}
	c.Set(sessionCooldownReceiptKey, (*sessionCooldownReceipt)(nil))
	close(receipt.Stop)
	<-receipt.Done
	removed := false
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	err := h.db.UpdateSessionCooldown(ctx, receipt.Subject, func(state *database.SessionCooldownState) error {
		root := state.Roots[receipt.Root]
		if root == nil || root.CreatedAt != receipt.CreatedAt {
			return nil
		}
		root.Confirmed = root.Confirmed || receipt.Successful
		delete(root.Leases, receipt.Lease)
		if !root.Confirmed && len(root.Leases) == 0 {
			delete(state.Roots, receipt.Root)
			removed = !root.PreserveWindow
		}
		return nil
	})
	cancel()
	if err != nil {
		log.Printf("event=session_creation_cooldown_finish_error subject=%s err=%v", receipt.Subject, err)
		return
	}
	if removed {
		if grant := windowGrantForRequest(c); grant != nil && grant.Grant.Confirmed {
			return
		}
		removedWindow := false
		h.promptSessionLimitMu.Lock()
		detail := h.promptSessionWindowDetails[receipt.Subject][receipt.Root]
		if current := h.promptSessionLimits[receipt.Subject][receipt.Root]; current.Equal(receipt.WindowExpiry) && (detail.CreatedAt.IsZero() || detail.CreatedAt.UnixMilli() == receipt.CreatedAt) {
			delete(h.promptSessionLimits[receipt.Subject], receipt.Root)
			delete(h.promptSessionWindowDetails[receipt.Subject], receipt.Root)
			removedWindow = true
		}
		h.promptSessionLimitMu.Unlock()
		if !removedWindow {
			return
		}
		h.persistPromptSessionLimits(receipt.Subject, time.Now())
		if grant := windowGrantForRequest(c); grant != nil {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			err := h.db.UpdateUserWindowAdmissions(ctx, receipt.Subject, func(state *database.UserWindowAdmissionState) error {
				if current := state.Windows[receipt.Root]; current != nil && current.ID == grant.Grant.ID && current.ExpiresAt.Equal(receipt.WindowExpiry) {
					delete(state.Windows, receipt.Root)
				}
				return nil
			})
			cancel()
			if err != nil {
				log.Printf("event=window_grant_cleanup_failed subject=%s", receipt.Subject)
			}
		}
	}
}
