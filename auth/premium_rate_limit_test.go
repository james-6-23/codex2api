package auth

import (
	"context"
	"testing"
	"time"
)

func newPremium5hTestAccount(plan string, resetAt time.Time) *Account {
	return &Account{
		DBID:                1,
		AccessToken:         "token",
		PlanType:            plan,
		Status:              StatusReady,
		HealthTier:          HealthTierHealthy,
		UsagePercent5h:      100,
		UsagePercent5hValid: true,
		Reset5hAt:           resetAt,
		UsageUpdatedAt:      time.Now().Add(-20 * time.Minute),
	}
}

func TestPremium5hRateLimitedAccountIsFencedFromScheduling(t *testing.T) {
	acc := newPremium5hTestAccount("plus", time.Now().Add(45*time.Minute))

	snapshot := acc.GetSchedulerDebugSnapshot(4)
	if got := acc.RuntimeStatus(); got != "rate_limited" {
		t.Fatalf("RuntimeStatus() = %q, want rate_limited", got)
	}
	if acc.IsAvailable() {
		t.Fatal("IsAvailable() = true, want false for premium 5h rate limited account")
	}
	if snapshot.HealthTier != string(HealthTierRisky) {
		t.Fatalf("HealthTier = %q, want %q", snapshot.HealthTier, HealthTierRisky)
	}
	if snapshot.DynamicConcurrencyLimit != 1 {
		t.Fatalf("DynamicConcurrencyLimit = %d, want 1", snapshot.DynamicConcurrencyLimit)
	}
}

func TestPremium5hRateLimitExpiresAndUsageProbeResumes(t *testing.T) {
	acc := newPremium5hTestAccount("team", time.Now().Add(-time.Minute))
	acc.Status = StatusCooldown
	acc.CooldownReason = "rate_limited"
	acc.CooldownUtil = time.Now().Add(-time.Minute)

	snapshot := acc.GetSchedulerDebugSnapshot(4)
	if got := acc.RuntimeStatus(); got != "active" {
		t.Fatalf("RuntimeStatus() = %q, want active after reset expires", got)
	}
	if !acc.IsAvailable() {
		t.Fatal("IsAvailable() = false, want true after reset expires")
	}
	if snapshot.HealthTier != string(HealthTierHealthy) {
		t.Fatalf("HealthTier = %q, want %q", snapshot.HealthTier, HealthTierHealthy)
	}
	if snapshot.DynamicConcurrencyLimit != 4 {
		t.Fatalf("DynamicConcurrencyLimit = %d, want 4", snapshot.DynamicConcurrencyLimit)
	}
	if !acc.NeedsUsageProbe(10 * time.Minute) {
		t.Fatal("NeedsUsageProbe() = false, want true after reset expires and snapshot becomes stale")
	}
}

func TestPremium5hRateLimitedSkipsResponsesProbeButRefreshesResetCredits(t *testing.T) {
	acc := newPremium5hTestAccount("pro", time.Now().Add(30*time.Minute))

	// premium 5h 限流期间，重置次数过期时仍允许探针——但必须是 wham-only（InLimitedState=true），
	// 不会发 /responses 加重限流。
	if !acc.NeedsUsageProbe(10 * time.Minute) {
		t.Fatal("NeedsUsageProbe() = false, want true to refresh stale reset credits during premium 5h limit")
	}
	if !acc.InLimitedState() {
		t.Fatal("InLimitedState() = false, want true during premium 5h limit so the probe stays wham-only")
	}

	// 重置次数刚探测过：limit 未到期前不应再触发探针。
	acc.MarkResetCreditsProbed(time.Now())
	if acc.NeedsUsageProbe(10 * time.Minute) {
		t.Fatal("NeedsUsageProbe() = true, want false before premium 5h reset once reset credits are fresh")
	}
}

func TestNormalizePlanTypeFoldsProliteIntoPro(t *testing.T) {
	cases := map[string]string{
		"prolite":   "pro",
		"ProLite":   "pro",
		" prolite ": "pro",
		"pro_lite":  "pro",
		"pro-lite":  "pro",
		"pro":       "pro",
		"plus":      "plus",
		"free":      "free",
		"":          "",
	}
	for input, want := range cases {
		if got := NormalizePlanType(input); got != want {
			t.Errorf("NormalizePlanType(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestProliteIsTreatedAsPremium5hPlan(t *testing.T) {
	acc := newPremium5hTestAccount("prolite", time.Now().Add(30*time.Minute))
	if !acc.IsPremium5hPlan() {
		t.Fatal("prolite should be recognized as a premium 5h plan")
	}
	if !IsPlusOrHigherPlan("prolite") {
		t.Fatal("prolite should qualify as plus-or-higher for image routing")
	}
	if got := defaultScoreBiasForPlan("prolite"); got != 50 {
		t.Fatalf("defaultScoreBiasForPlan(prolite) = %d, want 50", got)
	}
}

// issue #306/#309: k12(教育)等付费工作区计划有 5h 窗口，必须纳入 premium 5h 限流
// 语义，否则 429 后用量探针会把冷却清掉、账号显示可用但实际仍限流。
func TestPaidWorkspacePlansAreTreatedAsPremium5hPlans(t *testing.T) {
	for _, plan := range []string{"k12", "edu", "education", "go", "teamplus", "enterprise", "business"} {
		if !isPremium5hPlan(plan) {
			t.Errorf("isPremium5hPlan(%q) = false, want true", plan)
		}
	}
	for _, plan := range []string{"free", ""} {
		if isPremium5hPlan(plan) {
			t.Errorf("isPremium5hPlan(%q) = true, want false", plan)
		}
	}
}

func TestK12RateLimitedAccountIsFencedFromScheduling(t *testing.T) {
	acc := newPremium5hTestAccount("k12", time.Now().Add(45*time.Minute))

	if got := acc.RuntimeStatus(); got != "rate_limited" {
		t.Fatalf("RuntimeStatus() = %q, want rate_limited", got)
	}
	if acc.IsAvailable() {
		t.Fatal("IsAvailable() = true, want false for k12 5h rate limited account (issue #306/#309)")
	}
	if !acc.InLimitedState() {
		t.Fatal("InLimitedState() = false, want true so the usage probe stays wham-only")
	}
}

// issue #282: k12 是教育版 team 工作区，调度权重应与 team 对齐。
func TestK12GetsTeamSchedulerBias(t *testing.T) {
	if got := defaultScoreBiasForPlan("k12"); got != 50 {
		t.Fatalf("defaultScoreBiasForPlan(k12) = %d, want 50 (same as team)", got)
	}
	if !IsPlusOrHigherPlan("k12") {
		t.Fatal("IsPlusOrHigherPlan(k12) = false, want true")
	}
}

func TestCleanByRuntimeStatusSkipsPremium5hRateLimitedAccount(t *testing.T) {
	acc := newPremium5hTestAccount("plus", time.Now().Add(20*time.Minute))
	store := &Store{
		accounts: []*Account{acc},
	}

	if cleaned := store.CleanByRuntimeStatus(context.Background(), "rate_limited"); cleaned != 0 {
		t.Fatalf("CleanByRuntimeStatus() cleaned = %d, want 0", cleaned)
	}
	if store.AccountCount() != 1 {
		t.Fatalf("AccountCount() = %d, want 1", store.AccountCount())
	}
}

func TestCleanRateLimitedManualClearsAllRateLimitFlavors(t *testing.T) {
	premium := newPremium5hTestAccount("plus", time.Now().Add(20*time.Minute))
	premium.DBID = 1

	// Free 7d 用尽 → RuntimeStatus = "usage_exhausted"
	exhausted := &Account{
		DBID:                2,
		AccessToken:         "token-exhausted",
		PlanType:            "free",
		Status:              StatusReady,
		HealthTier:          HealthTierHealthy,
		UsagePercent7d:      100,
		UsagePercent7dValid: true,
		Reset7dAt:           time.Now().Add(48 * time.Hour),
		UsageUpdatedAt:      time.Now().Add(-1 * time.Minute),
	}

	// 普通正常账号 → 不应被清理
	healthy := &Account{
		DBID:        3,
		AccessToken: "token-healthy",
		PlanType:    "plus",
		Status:      StatusReady,
		HealthTier:  HealthTierHealthy,
	}

	// 锁定的限流账号 → 不应被清理
	lockedRL := newPremium5hTestAccount("plus", time.Now().Add(20*time.Minute))
	lockedRL.DBID = 4
	lockedRL.Locked = 1

	store := &Store{accounts: []*Account{premium, exhausted, healthy, lockedRL}}

	cleaned := store.CleanRateLimitedManual(context.Background())
	if cleaned != 2 {
		t.Fatalf("CleanRateLimitedManual() cleaned = %d, want 2 (premium + exhausted)", cleaned)
	}
	if store.AccountCount() != 2 {
		t.Fatalf("AccountCount() = %d, want 2 (healthy + locked stay)", store.AccountCount())
	}
	if store.FindByID(3) == nil {
		t.Fatal("healthy account should remain")
	}
	if store.FindByID(4) == nil {
		t.Fatal("locked rate-limited account should remain")
	}
}
