package intelligencecheck

// AdaptiveDefaults lets existing saved configurations adopt the new fields
// without enabling the policy or changing their recovery thresholds.
func AdaptiveDefaults(c Settings) Settings {
	if c.PauseInitialSeconds == 0 {
		c.PauseInitialSeconds = 1800
	}
	if c.PauseMaxSeconds == 0 {
		c.PauseMaxSeconds = 172800
	}
	if c.RecoveryInitialSeconds == 0 {
		c.RecoveryInitialSeconds = 300
	}
	if c.RecoveryMinSeconds == 0 {
		c.RecoveryMinSeconds = 120
	}
	return c
}

// PhaseQuotaWait marks an account whose automatic checks are postponed until
// its rate limit or quota recovers. It is set outside Schedule, because a
// postponed check is never graded, and the next graded result replaces it.
const PhaseQuotaWait = "quota_wait"

// Schedule applies timing after the ownership transition. PauseBackoffSeconds
// is retained through recovery; NextIntervalSeconds is the current nominal wait.
// Errors never become degraded grades, but do back off unavailable paused accounts.
func Schedule(c Settings, before, next State, status string) (State, int) {
	c = AdaptiveDefaults(c)
	delay := c.IntervalSeconds
	failed := status == "error" || status == "interrupted"
	if !c.AdaptiveEnabled {
		if failed {
			delay = c.RetrySeconds
		}
		next.PauseBackoffSeconds, next.ErrorRetrySeconds = 0, 0
		next.ProbePhase = ""
		next.NextIntervalSeconds = delay
		return next, delay
	}
	if next.PauseBackoffSeconds > 0 {
		next.PauseBackoffSeconds = min(c.PauseMaxSeconds, max(c.PauseInitialSeconds, next.PauseBackoffSeconds))
	}
	next.ProbePhase = "normal"
	if next.OwnedAction == "pause" {
		next.ErrorRetrySeconds = 0
		// An old pause starts at the initial interval, not an inferred historical
		// failure count. A failed recovery keeps its previous interval unchanged.
		if next.PauseBackoffSeconds == 0 {
			next.PauseBackoffSeconds = c.PauseInitialSeconds
		} else if before.OwnedAction == "pause" && before.RecoverStreak == 0 && status != "normal" {
			next.PauseBackoffSeconds = min(c.PauseMaxSeconds, next.PauseBackoffSeconds*2)
		}
		delay = next.PauseBackoffSeconds
		next.ProbePhase = "backoff"
		if status == "normal" && c.Action == "pause" {
			next.ProbePhase = "recovering"
			delay = c.RecoveryInitialSeconds
			// Round the next half down to whole minutes when the configured floor
			// is at least a minute: defaults produce 5m -> 2m -> 2m.
			for i := 1; i < next.RecoverStreak && delay > c.RecoveryMinSeconds; i++ {
				delay /= 2
				if c.RecoveryMinSeconds >= 60 {
					delay = delay / 60 * 60
				}
				delay = max(c.RecoveryMinSeconds, delay)
			}
		}
	} else if status == "normal" {
		next.ErrorRetrySeconds = 0
		// The confirmation which releases a pause is not a routine successful
		// check. Only subsequent healthy checks forgive the retained penalty.
		if before.OwnedAction != "pause" && next.PauseBackoffSeconds > 0 {
			next.PauseBackoffSeconds /= 2
			if next.PauseBackoffSeconds <= c.PauseInitialSeconds {
				next.PauseBackoffSeconds = 0
			}
		}
	} else if failed {
		ceiling := max(c.IntervalSeconds, c.RetrySeconds)
		next.ErrorRetrySeconds = c.RetrySeconds
		if before.ErrorRetrySeconds > 0 {
			next.ErrorRetrySeconds = min(ceiling, before.ErrorRetrySeconds*2)
		}
		delay = next.ErrorRetrySeconds
		next.ProbePhase = "retrying"
	} else {
		next.ErrorRetrySeconds = 0
	}
	next.NextIntervalSeconds = delay
	return next, delay
}
