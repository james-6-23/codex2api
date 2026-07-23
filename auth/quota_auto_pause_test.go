package auth

import (
	"math"
	"testing"
	"time"

	"github.com/codex2api/database"
)

const quotaAutoPauseFloatTolerance = 1e-6

func nearlyEqualFloat64(a, b float64) bool {
	diff := math.Abs(a - b)
	return diff <= quotaAutoPauseFloatTolerance && !math.IsNaN(diff)
}

func newQuotaAutoPauseTestAccount() *Account {
	acc := &Account{
		DBID:        1,
		AccessToken: "token",
		PlanType:    "plus",
		Status:      StatusReady,
		HealthTier:  HealthTierHealthy,
	}
	return acc
}

func setAutoPauseThresholdsWithGuard(acc *Account, guardBandPercent float64) {
	setAutoPauseThresholdsWithGuardConcurrency(acc, guardBandPercent, 1)
}

func setAutoPauseThresholdsWithGuardConcurrency(acc *Account, guardBandPercent float64, guardConcurrency int) {
	store := NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:              4,
		TestConcurrency:             1,
		TestModel:                   "gpt-5.4",
		AutoPause5hGuardBandPercent: guardBandPercent,
		AutoPause5hGuardConcurrency: guardConcurrency,
	})
	acc.recomputeEffectiveAutoPause(store)
}

func TestQuotaAutoPause5hThresholdFencesAccount(t *testing.T) {
	acc := newQuotaAutoPauseTestAccount()
	acc.AutoPause5hThreshold = 0.95
	acc.UsagePercent5h = 95
	acc.UsagePercent5hValid = true
	acc.Reset5hAt = time.Now().Add(time.Hour)
	setAutoPauseThresholdsWithGuard(acc, 5)

	if acc.IsAvailable() {
		t.Fatal("IsAvailable() = true, want false after 5h auto-pause threshold is reached")
	}
	if got := acc.RuntimeStatus(); got != "quota_paused" {
		t.Fatalf("RuntimeStatus() = %q, want quota_paused after auto-pause threshold is reached", got)
	}
	_, _, _, _, available := acc.fastSchedulerSnapshot(4, time.Now())
	if available {
		t.Fatal("fastSchedulerSnapshot available = true, want false")
	}
}

func TestQuotaAutoPause5hThresholdAllowsMissingOptionalWindowBeforeFencing(t *testing.T) {
	acc := newQuotaAutoPauseTestAccount()
	acc.AutoPause5hThreshold = 0.95
	acc.UsagePercent7d = 12
	acc.UsagePercent7dValid = true
	acc.UsageUpdatedAt = time.Now()
	setAutoPauseThresholdsWithGuard(acc, 5)
	acc.MarkResetCreditsProbed(time.Now())

	if acc.NeedsUsageProbe(10 * time.Minute) {
		t.Fatal("NeedsUsageProbe() = true, want false when the optional 5h window is absent")
	}

	acc.SetUsageSnapshot5h(95, time.Now().Add(time.Hour))

	if acc.IsAvailable() {
		t.Fatal("IsAvailable() = true, want false after refreshed 5h usage reaches the threshold")
	}
	if got := acc.RuntimeStatus(); got != "quota_paused" {
		t.Fatalf("RuntimeStatus() = %q, want quota_paused after refreshed 5h usage reaches the threshold", got)
	}
}

func TestQuotaAutoPauseIgnoresBelowThresholdAndDisabledWindow(t *testing.T) {
	acc := newQuotaAutoPauseTestAccount()
	acc.AutoPause5hThreshold = 0.95
	acc.UsagePercent5h = 94.9
	acc.UsagePercent5hValid = true
	acc.Reset5hAt = time.Now().Add(time.Hour)
	setAutoPauseThresholdsWithGuard(acc, 5)

	if !acc.IsAvailable() {
		t.Fatal("IsAvailable() = false, want true below threshold")
	}

	acc.UsagePercent5h = 99
	acc.AutoPause5hDisabled = true
	setAutoPauseThresholdsWithGuard(acc, 5)
	if !acc.IsAvailable() {
		t.Fatal("IsAvailable() = false, want true when 5h auto-pause is disabled")
	}
	if got := acc.RuntimeStatus(); got != "active" {
		t.Fatalf("RuntimeStatus() = %q, want active when 5h auto-pause is disabled", got)
	}
}

func TestQuotaAutoPauseStopsAfterResetTime(t *testing.T) {
	acc := newQuotaAutoPauseTestAccount()
	acc.AutoPause5hThreshold = 0.95
	acc.UsagePercent5h = 99
	acc.UsagePercent5hValid = true
	acc.Reset5hAt = time.Now().Add(-time.Minute)
	setAutoPauseThresholdsWithGuard(acc, 5)

	if !acc.IsAvailable() {
		t.Fatal("IsAvailable() = false, want true after reset time has passed")
	}
}

func TestQuotaAutoPause5hNearThresholdLimitsConcurrency(t *testing.T) {
	acc := newQuotaAutoPauseTestAccount()
	acc.AutoPause5hThreshold = 0.9
	acc.BaseConcurrencyOverride = int64Ptr(4)
	acc.SetUsageSnapshot5h(86, time.Now().Add(time.Hour))
	setAutoPauseThresholdsWithGuard(acc, 5)
	recomputeTestAccount(acc, 4)

	if !acc.IsAvailable() {
		t.Fatal("IsAvailable() = false, want true before threshold is reached")
	}
	if acc.DynamicConcurrencyLimit != 1 {
		t.Fatalf("DynamicConcurrencyLimit = %d, want 1 near 5h auto-pause threshold", acc.DynamicConcurrencyLimit)
	}
	_, _, limit, _, available := acc.fastSchedulerSnapshot(4, time.Now())
	if !available {
		t.Fatal("fastSchedulerSnapshot available = false, want true before threshold is reached")
	}
	if limit != 1 {
		t.Fatalf("fastSchedulerSnapshot limit = %d, want 1 near 5h auto-pause threshold", limit)
	}
}

func TestQuotaAutoPause5hNearThresholdUsesConfiguredGuardConcurrency(t *testing.T) {
	acc := newQuotaAutoPauseTestAccount()
	acc.AutoPause5hThreshold = 0.9
	acc.BaseConcurrencyOverride = int64Ptr(4)
	acc.SetUsageSnapshot5h(86, time.Now().Add(time.Hour))
	setAutoPauseThresholdsWithGuardConcurrency(acc, 5, 2)
	recomputeTestAccount(acc, 4)

	if acc.DynamicConcurrencyLimit != 2 {
		t.Fatalf("DynamicConcurrencyLimit = %d, want configured guard concurrency 2", acc.DynamicConcurrencyLimit)
	}
	_, _, limit, _, available := acc.fastSchedulerSnapshot(4, time.Now())
	if !available {
		t.Fatal("fastSchedulerSnapshot available = false, want true before threshold is reached")
	}
	if limit != 2 {
		t.Fatalf("fastSchedulerSnapshot limit = %d, want configured guard concurrency 2", limit)
	}
}

func TestQuotaAutoPause5hGuardConcurrencyCanBeDisabled(t *testing.T) {
	acc := newQuotaAutoPauseTestAccount()
	acc.AutoPause5hThreshold = 0.9
	acc.BaseConcurrencyOverride = int64Ptr(4)
	acc.SetUsageSnapshot5h(86, time.Now().Add(time.Hour))
	setAutoPauseThresholdsWithGuardConcurrency(acc, 5, 0)
	recomputeTestAccount(acc, 4)

	if acc.DynamicConcurrencyLimit != 4 {
		t.Fatalf("DynamicConcurrencyLimit = %d, want base limit when guard concurrency is 0", acc.DynamicConcurrencyLimit)
	}
}

func TestQuotaAutoPause5hGuardConcurrencyDoesNotIncreaseLimit(t *testing.T) {
	acc := newQuotaAutoPauseTestAccount()
	acc.AutoPause5hThreshold = 0.9
	acc.BaseConcurrencyOverride = int64Ptr(2)
	acc.SetUsageSnapshot5h(86, time.Now().Add(time.Hour))
	setAutoPauseThresholdsWithGuardConcurrency(acc, 5, 4)
	recomputeTestAccount(acc, 2)

	if acc.DynamicConcurrencyLimit != 2 {
		t.Fatalf("DynamicConcurrencyLimit = %d, want original limit when guard concurrency is higher", acc.DynamicConcurrencyLimit)
	}
}

func TestQuotaAutoPause5hGuardKeepsNormalConcurrencyOutsideGuardBand(t *testing.T) {
	acc := newQuotaAutoPauseTestAccount()
	acc.AutoPause5hThreshold = 0.9
	acc.BaseConcurrencyOverride = int64Ptr(4)
	acc.SetUsageSnapshot5h(84.9, time.Now().Add(time.Hour))
	setAutoPauseThresholdsWithGuard(acc, 5)
	recomputeTestAccount(acc, 4)

	if acc.DynamicConcurrencyLimit != 4 {
		t.Fatalf("DynamicConcurrencyLimit = %d, want base limit outside guard band", acc.DynamicConcurrencyLimit)
	}
}

func TestQuotaAutoPause5hGuardCanBeDisabledByBand(t *testing.T) {
	acc := newQuotaAutoPauseTestAccount()
	acc.AutoPause5hThreshold = 0.9
	acc.BaseConcurrencyOverride = int64Ptr(4)
	acc.SetUsageSnapshot5h(86, time.Now().Add(time.Hour))
	setAutoPauseThresholdsWithGuard(acc, 0)
	recomputeTestAccount(acc, 4)

	if acc.DynamicConcurrencyLimit != 4 {
		t.Fatalf("DynamicConcurrencyLimit = %d, want base limit when guard band is 0", acc.DynamicConcurrencyLimit)
	}
}

func TestQuotaAutoPause5hGuardIgnoresDisabledOrExpiredWindow(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		acc := newQuotaAutoPauseTestAccount()
		acc.AutoPause5hThreshold = 0.9
		acc.AutoPause5hDisabled = true
		acc.BaseConcurrencyOverride = int64Ptr(4)
		acc.SetUsageSnapshot5h(89, time.Now().Add(time.Hour))
		setAutoPauseThresholdsWithGuard(acc, 5)
		recomputeTestAccount(acc, 4)

		if acc.DynamicConcurrencyLimit != 4 {
			t.Fatalf("DynamicConcurrencyLimit = %d, want base limit when 5h auto-pause is disabled", acc.DynamicConcurrencyLimit)
		}
	})

	t.Run("expired", func(t *testing.T) {
		acc := newQuotaAutoPauseTestAccount()
		acc.AutoPause5hThreshold = 0.9
		acc.BaseConcurrencyOverride = int64Ptr(4)
		acc.SetUsageSnapshot5h(89, time.Now().Add(-time.Minute))
		setAutoPauseThresholdsWithGuard(acc, 5)
		recomputeTestAccount(acc, 4)

		if acc.DynamicConcurrencyLimit != 4 {
			t.Fatalf("DynamicConcurrencyLimit = %d, want base limit after 5h window reset", acc.DynamicConcurrencyLimit)
		}
	})
}

func TestQuotaAutoPause5hGuardReducesDispatchScoreNearThreshold(t *testing.T) {
	resetAt := time.Now().Add(premium5hUrgencyWindow + time.Hour)
	guarded := newQuotaAutoPauseTestAccount()
	guarded.AutoPause5hThreshold = 0.9
	guarded.SetUsageSnapshot5h(87, resetAt) // remaining 3pp inside a 5pp band => 40% of max penalty
	setAutoPauseThresholdsWithGuard(guarded, 5)
	recomputeTestAccount(guarded, 4)

	baseline := newQuotaAutoPauseTestAccount()
	baseline.AutoPause5hThreshold = 0.9
	baseline.SetUsageSnapshot5h(87, resetAt)
	setAutoPauseThresholdsWithGuard(baseline, 0)
	recomputeTestAccount(baseline, 4)

	penalty := baseline.DispatchScore - guarded.DispatchScore
	if !nearlyEqualFloat64(penalty, 20) {
		t.Fatalf("guard penalty = %v, want 20 (baseline=%v guarded=%v)", penalty, baseline.DispatchScore, guarded.DispatchScore)
	}
}

func TestQuotaAutoPause5hGuardDoesNotReduceDispatchScoreOutsideBandOrDisabled(t *testing.T) {
	t.Run("outside band", func(t *testing.T) {
		resetAt := time.Now().Add(premium5hUrgencyWindow + time.Hour)
		guarded := newQuotaAutoPauseTestAccount()
		guarded.AutoPause5hThreshold = 0.9
		guarded.SetUsageSnapshot5h(84.9, resetAt)
		setAutoPauseThresholdsWithGuard(guarded, 5)
		recomputeTestAccount(guarded, 4)

		baseline := newQuotaAutoPauseTestAccount()
		baseline.AutoPause5hThreshold = 0.9
		baseline.SetUsageSnapshot5h(84.9, resetAt)
		setAutoPauseThresholdsWithGuard(baseline, 0)
		recomputeTestAccount(baseline, 4)

		if !nearlyEqualFloat64(guarded.DispatchScore, baseline.DispatchScore) {
			t.Fatalf("DispatchScore = %v, want baseline %v outside guard band", guarded.DispatchScore, baseline.DispatchScore)
		}
	})

	t.Run("guard concurrency disabled", func(t *testing.T) {
		resetAt := time.Now().Add(premium5hUrgencyWindow + time.Hour)
		acc := newQuotaAutoPauseTestAccount()
		acc.AutoPause5hThreshold = 0.9
		acc.SetUsageSnapshot5h(87, resetAt)
		setAutoPauseThresholdsWithGuardConcurrency(acc, 5, 0)
		recomputeTestAccount(acc, 4)

		baseline := newQuotaAutoPauseTestAccount()
		baseline.AutoPause5hThreshold = 0.9
		baseline.SetUsageSnapshot5h(87, resetAt)
		setAutoPauseThresholdsWithGuard(baseline, 0)
		recomputeTestAccount(baseline, 4)

		if !nearlyEqualFloat64(acc.DispatchScore, baseline.DispatchScore) {
			t.Fatalf("DispatchScore = %v, want baseline %v when guard concurrency is disabled", acc.DispatchScore, baseline.DispatchScore)
		}
	})
}

func TestQuotaAutoPause7dThresholdFencesAccount(t *testing.T) {
	acc := newQuotaAutoPauseTestAccount()
	acc.AutoPause7dThreshold = 0.9
	acc.UsagePercent7d = 91
	acc.UsagePercent7dValid = true
	setAutoPauseThresholdsWithGuard(acc, 5)

	if acc.IsAvailable() {
		t.Fatal("IsAvailable() = true, want false after 7d auto-pause threshold is reached")
	}
}
