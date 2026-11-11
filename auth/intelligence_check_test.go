package auth

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestIntelligencePauseFencesBusinessButAllowsBoundedProbe(t *testing.T) {
	s := NewStore(nil, nil, nil)
	s.SetMaxConcurrency(1)
	a := &Account{DBID: 1, AccessToken: "test", Status: StatusReady, HealthTier: HealthTierHealthy}
	s.AddAccount(a)
	atomic.StoreInt32(&a.IntelligencePaused, 1)
	if !accountDispatchBlocked(a) || a.ModelCatalogEligible() {
		t.Fatal("detection pause must block business/catalog")
	}
	_, _, _, _, available := a.fastSchedulerSnapshot(4, time.Now())
	if available {
		t.Fatal("fast scheduler ignored detection pause")
	}
	if !s.TryAcquireIntelligenceCheck(a) {
		t.Fatal("detection must continue during its own pause")
	}
	if s.TryAcquireIntelligenceCheck(a) {
		t.Fatal("hard default capacity exceeded")
	}
	s.Release(a)
	atomic.StoreInt32(&a.DispatchPaused, 1)
	atomic.StoreInt32(&a.IntelligencePaused, 0)
	if !accountDispatchBlocked(a) {
		t.Fatal("clearing quality pause must not enable manually disabled account")
	}
}

func TestIntelligenceCheckQuotaHold(t *testing.T) {
	now := time.Now()
	in := func(d time.Duration) time.Time { return now.Add(d) }
	creditsOn := func(a *Account) {
		a.CreditEnabled, a.CreditSkipUsageWindow = true, true
		a.CreditsValid, a.CreditsHasCredits = true, true
	}
	for _, tc := range []struct {
		name   string
		model  string
		policy DispatchPolicy
		setup  func(*Account)
		held   bool
		until  time.Time
	}{
		{name: "ready account", setup: func(*Account) {}},
		{name: "expired cooldown", setup: func(a *Account) { a.SetCooldownUntil(in(-time.Minute), "rate_limited") }},
		{name: "usage limit cooldown", held: true, until: in(3 * time.Hour), setup: func(a *Account) { a.SetCooldownUntil(in(3*time.Hour), "usage_limit") }},
		{name: "responses 429 cooldown", held: true, until: in(time.Hour), setup: func(a *Account) { a.SetCooldownUntil(in(time.Hour), ResponsesRateLimitedCooldownReason) }},
		// Authentication failures and errors have their own policy.
		{name: "unauthorized cooldown", setup: func(a *Account) { a.SetCooldownUntil(in(time.Hour), "unauthorized") }},
		{name: "errored account", setup: func(a *Account) {
			a.SetCooldownUntil(in(time.Hour), "rate_limited")
			a.Status = StatusError
		}},
		{name: "premium 5h window full", held: true, until: in(2 * time.Hour), setup: func(a *Account) {
			a.PlanType = "plus"
			a.UsagePercent5h, a.UsagePercent5hValid, a.Reset5hAt = 100, true, in(2*time.Hour)
		}},
		{name: "later 7d reset wins", held: true, until: in(48 * time.Hour), setup: func(a *Account) {
			a.PlanType = "plus"
			a.UsagePercent5h, a.UsagePercent5hValid, a.Reset5hAt = 100, true, in(2*time.Hour)
			a.UsagePercent7d, a.UsagePercent7dValid, a.Reset7dAt = 100, true, in(48*time.Hour)
		}},
		{name: "free window full without reset", held: true, setup: func(a *Account) {
			a.PlanType = "free"
			a.UsagePercent7d, a.UsagePercent7dValid = 100, true
		}},
		// Credits keep the account dispatchable, so it is still worth checking.
		{name: "credits cover full window", setup: func(a *Account) {
			a.PlanType = "plus"
			a.UsagePercent5h, a.UsagePercent5hValid, a.Reset5hAt = 100, true, in(2*time.Hour)
			creditsOn(a)
		}},
		{name: "below the limit", setup: func(a *Account) {
			a.PlanType = "plus"
			a.UsagePercent5h, a.UsagePercent5hValid, a.Reset5hAt = 99, true, in(2*time.Hour)
		}},
		{name: "model quota cooldown", model: "GPT-Test", held: true, until: in(10 * time.Minute), setup: func(a *Account) {
			a.SetModelCooldownUntil("gpt-test", "model_capacity", in(10*time.Minute))
		}},
		{name: "cooldown on another model", model: "gpt-test", setup: func(a *Account) {
			a.SetModelCooldownUntil("gpt-other", "model_capacity", in(10*time.Minute))
		}},
		{name: "unsupported model is not a quota wait", model: "gpt-test", setup: func(a *Account) {
			a.SetModelCooldownUntil("gpt-test", ModelCooldownReasonUnsupported, in(10*time.Minute))
		}},
		// Spark ignores the account windows and has its own.
		{name: "spark ignores 5h window", policy: DispatchPolicySpark, setup: func(a *Account) {
			a.PlanType = "pro"
			a.UsagePercent5h, a.UsagePercent5hValid, a.Reset5hAt = 100, true, in(2*time.Hour)
			a.SetCooldownUntil(in(2*time.Hour), premium5hCooldownReason)
		}},
		{name: "spark window full", policy: DispatchPolicySpark, held: true, until: in(4 * time.Hour), setup: func(a *Account) {
			a.UsagePercentSpark, a.UsagePercentSparkValid, a.ResetSparkAt = 100, true, in(4*time.Hour)
		}},
		{name: "grok billing exhausted", held: true, setup: func(a *Account) {
			a.UpstreamType = UpstreamGrok
			a.GrokBillingExhausted, a.GrokBillingExpiresAt = true, in(time.Hour)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &Account{DBID: 1, AccessToken: "test", Status: StatusReady, HealthTier: HealthTierHealthy}
			tc.setup(a)
			until, held := a.IntelligenceCheckQuotaHold(tc.model, tc.policy, now)
			if held != tc.held || !until.Equal(tc.until) {
				t.Fatalf("hold=%v until=%v, want %v %v", held, until, tc.held, tc.until)
			}
		})
	}
	if _, held := (*Account)(nil).IntelligenceCheckQuotaHold("gpt-test", DispatchPolicyStandard, now); held {
		t.Fatal("missing account reported as held")
	}
}

// The hold must agree with dispatch: a held account is one fresh requests avoid.
func TestIntelligenceCheckQuotaHoldMatchesDispatch(t *testing.T) {
	now := time.Now()
	a := &Account{DBID: 1, AccessToken: "test", Status: StatusReady, HealthTier: HealthTierHealthy, PlanType: "plus"}
	if _, held := a.IntelligenceCheckQuotaHold("", DispatchPolicyStandard, now); held || !a.IsAvailable() {
		t.Fatal("ready account must be available and unheld")
	}
	a.UsagePercent5h, a.UsagePercent5hValid, a.Reset5hAt = 100, true, now.Add(time.Hour)
	if _, held := a.IntelligenceCheckQuotaHold("", DispatchPolicyStandard, now); !held || a.IsAvailable() {
		t.Fatal("exhausted account must be unavailable and held")
	}
}
