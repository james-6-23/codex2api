package auth

import "time"

type usageFrameObservation struct {
	at    time.Time
	apply func()
}

const maxPendingUsageFrames = 64

// ApplyUsageFrameObservation keeps the sole WS reader non-blocking even when a
// WHAM observation holds usageSyncMu during a write. A bounded FIFO preserves
// partial-frame semantics and original timestamps; callbacks must not be merged
// as whole frames. On overflow retain the newest bounded tail and request the
// existing authoritative probe. Queued callbacks never perform database I/O.
func (s *Store) ApplyUsageFrameObservation(acc *Account, at time.Time, apply func()) {
	if acc == nil || apply == nil {
		return
	}
	if at.IsZero() {
		at = time.Now()
	}
	guarded := func() {
		if s != nil {
			select {
			case <-s.stopCh:
				return
			default:
			}
		}
		apply()
	}
	acc.usageFrameObservationMu.Lock()
	// Do not let a newly arriving partial frame jump past a queued frame and
	// make that frame appear stale before its worker acquires usageSyncMu.
	if !acc.usageFrameObservationRunning && len(acc.usageFramePending) == 0 && acc.TryApplyUsageObservation(at, guarded) {
		acc.usageFrameObservationMu.Unlock()
		return
	}
	acc.mu.RLock()
	stale := at.Before(acc.usageObservedAt)
	acc.mu.RUnlock()
	if stale {
		acc.usageFrameObservationMu.Unlock()
		return
	}
	overflow := len(acc.usageFramePending) == maxPendingUsageFrames
	wakeProbe := overflow && !acc.usageFrameObservationOverflow
	if overflow {
		copy(acc.usageFramePending, acc.usageFramePending[1:])
		acc.usageFramePending = acc.usageFramePending[:maxPendingUsageFrames-1]
		acc.usageFrameObservationOverflow = true
	}
	acc.usageFramePending = append(acc.usageFramePending, usageFrameObservation{at: at, apply: guarded})
	if acc.usageFrameObservationRunning {
		acc.usageFrameObservationMu.Unlock()
		if wakeProbe && s != nil {
			s.TriggerUsageProbeForAccountAsync(acc)
		}
		return
	}
	acc.usageFrameObservationRunning = true
	acc.usageFrameObservationMu.Unlock()
	go func() {
		for {
			acc.usageFrameObservationMu.Lock()
			if len(acc.usageFramePending) == 0 {
				acc.usageFrameObservationRunning = false
				acc.usageFrameObservationOverflow = false
				acc.usageFramePending = nil
				acc.usageFrameObservationMu.Unlock()
				return
			}
			pending := acc.usageFramePending[0]
			acc.usageFramePending[0] = usageFrameObservation{}
			acc.usageFramePending = acc.usageFramePending[1:]
			acc.usageFrameObservationMu.Unlock()
			acc.ApplyUsageObservation(pending.at, pending.apply)
		}
	}()
}

// MarkUsageFrameCooldowns is called inside the frame observation ordering gate.
// Only runtime state changes here; the existing worker owns DB/cache writes.
func (s *Store) MarkUsageFrameCooldowns(acc *Account, at time.Time) (bool, bool) {
	if s == nil || acc == nil {
		return false, false
	}
	acc.mu.RLock()
	isolated := acc.Status == StatusError || acc.healthTierLocked() == HealthTierBanned
	acc.mu.RUnlock()
	if isolated {
		return false, false
	}
	limited7d := s.markUsage7dRateLimited(acc, false)
	pct, reset, valid := acc.GetUsageSnapshot5h()
	limited5h := valid && pct >= 100 && acc.IsPremium5hPlan() && !acc.SkipsUsageWindowLimits()
	if limited5h {
		s.markPremium5hRateLimited(acc, reset, at, premium5hCooldownReason, false)
	}
	return limited7d, limited5h
}
