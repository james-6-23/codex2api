package auth

import (
	"context"
	"testing"
	"time"
)

// plus5hExhausted 构造一个 5h 窗口已打满的 plus 账号：没有积分时它就是 rate_limited。
func plus5hExhausted() *Account {
	return &Account{
		DBID:                1,
		AccessToken:         "at-test",
		Status:              StatusReady,
		PlanType:            "plus",
		UsagePercent5h:      100,
		UsagePercent5hValid: true,
		Reset5hAt:           time.Now().Add(2 * time.Hour),
	}
}

// withCredits 打开两个信用开关并写入一份积分余额快照。
func withCredits(acc *Account, balance string, hasCredits, unlimited, overageReached bool) *Account {
	acc.CreditEnabled = true
	acc.CreditSkipUsageWindow = true
	acc.CreditsValid = true
	acc.CreditsBalance = balance
	acc.CreditsHasCredits = hasCredits
	acc.CreditsUnlimited = unlimited
	acc.CreditsOverageLimitReached = overageReached
	return acc
}

func TestCreditsBalanceValue(t *testing.T) {
	cases := []struct {
		raw  string
		want float64
	}{
		{"1000.0000000000", 1000},
		{"981.7471800000", 981.74718},
		{" 12.5 ", 12.5},
		{"0", 0},
		{"0.0000000000", 0},
		{"", 0},
		{"not-a-number", 0},
	}
	for _, tc := range cases {
		if got := creditsBalanceValue(tc.raw); got != tc.want {
			t.Errorf("creditsBalanceValue(%q) = %v, want %v", tc.raw, got, tc.want)
		}
	}
}

// 有积分时用量窗口限流被顶替，账号仍可调度，状态显示 using_credits。
func TestUsingCreditsSuppressesRateLimitWhenBalancePositive(t *testing.T) {
	acc := withCredits(plus5hExhausted(), "1000.0000000000", true, false, false)

	if acc.IsPremium5hRateLimited() {
		t.Error("IsPremium5hRateLimited() = true, want false while credits cover the window")
	}
	// 显示仍是限流（窗口客观上打满了），可调度与否由 IsAvailable 决定，
	// 「正在用积分顶」由并列的 UsingCredits 信号表达。
	if got := acc.RuntimeStatus(); got != "rate_limited" {
		t.Fatalf("RuntimeStatus() = %q, want rate_limited", got)
	}
	if !acc.IsAvailable() {
		t.Fatal("IsAvailable() = false, want true — the credits badge means it still gets scheduled")
	}
	if !acc.UsingCredits() {
		t.Fatal("UsingCredits() = false, want true while credits cover the window")
	}
}

// 积分归零后必须恢复成真实限流——否则调度会一直把请求送进注定 429 的账号。
func TestUsingCreditsFallsBackToRateLimitedWhenBalanceZero(t *testing.T) {
	acc := withCredits(plus5hExhausted(), "0.0000000000", true, false, false)

	if !acc.IsPremium5hRateLimited() {
		t.Error("IsPremium5hRateLimited() = false, want true once credits are gone")
	}
	if got := acc.RuntimeStatus(); got != "rate_limited" {
		t.Fatalf("RuntimeStatus() = %q, want rate_limited", got)
	}
	if acc.UsingCredits() {
		t.Error("UsingCredits() = true, want false once credits are gone")
	}
}

// has_credits=false 即使余额字符串非零也不放行：上游说没有就是没有。
func TestUsingCreditsRequiresHasCreditsFlag(t *testing.T) {
	acc := withCredits(plus5hExhausted(), "1000.0000000000", false, false, false)

	if got := acc.RuntimeStatus(); got != "rate_limited" {
		t.Fatalf("RuntimeStatus() = %q, want rate_limited when has_credits is false", got)
	}
}

// overage_limit_reached 是上游权威的「超额额度已用尽」信号，优先于余额数字。
func TestUsingCreditsRespectsOverageLimitReached(t *testing.T) {
	acc := withCredits(plus5hExhausted(), "1000.0000000000", true, false, true)

	if got := acc.RuntimeStatus(); got != "rate_limited" {
		t.Fatalf("RuntimeStatus() = %q, want rate_limited when overage limit is reached", got)
	}
}

// 无限积分账号一直放行。
func TestUsingCreditsHonorsUnlimited(t *testing.T) {
	acc := withCredits(plus5hExhausted(), "0", false, true, false)

	if got := acc.RuntimeStatus(); got != "rate_limited" {
		t.Fatalf("RuntimeStatus() = %q, want rate_limited for unlimited credits", got)
	}
	if !acc.UsingCredits() {
		t.Fatal("UsingCredits() = false, want true for unlimited credits")
	}
}

// 余额未知（wham 探针没跑过）按没积分处理：宁可白等一个窗口，也不送进必然 429 的账号。
func TestUsingCreditsTreatsUnknownBalanceAsEmpty(t *testing.T) {
	acc := plus5hExhausted()
	acc.CreditEnabled = true
	acc.CreditSkipUsageWindow = true
	acc.CreditsValid = false
	acc.CreditsBalance = "1000.0000000000"
	acc.CreditsHasCredits = true

	if got := acc.RuntimeStatus(); got != "rate_limited" {
		t.Fatalf("RuntimeStatus() = %q, want rate_limited when the balance was never probed", got)
	}
}

// 两个开关没开时积分余额完全不参与判断，保持原有限流语义。
func TestUsingCreditsRequiresBothToggles(t *testing.T) {
	onlyEnabled := plus5hExhausted()
	onlyEnabled.CreditEnabled = true
	onlyEnabled.CreditsValid = true
	onlyEnabled.CreditsBalance = "1000"
	onlyEnabled.CreditsHasCredits = true
	if got := onlyEnabled.RuntimeStatus(); got != "rate_limited" {
		t.Errorf("RuntimeStatus() = %q, want rate_limited when skip toggle is off", got)
	}

	neither := plus5hExhausted()
	neither.CreditsValid = true
	neither.CreditsBalance = "1000"
	neither.CreditsHasCredits = true
	if got := neither.RuntimeStatus(); got != "rate_limited" {
		t.Errorf("RuntimeStatus() = %q, want rate_limited when both toggles are off", got)
	}
}

// 上游真的 429（走 cooldown）说明积分没顶住，必须暴露真实状态，不能被 using_credits 盖掉。
func TestUsingCreditsDoesNotMaskUpstreamCooldown(t *testing.T) {
	acc := withCredits(plus5hExhausted(), "1000.0000000000", true, false, false)
	acc.SetCooldownUntil(time.Now().Add(30*time.Minute), "rate_limited_5h")

	if got := acc.RuntimeStatus(); got != "rate_limited_5h" {
		t.Fatalf("RuntimeStatus() = %q, want rate_limited_5h — a real upstream 429 must not be hidden", got)
	}
	if acc.UsingCredits() {
		t.Error("UsingCredits() = true, want false — a real 429 means credits did not cover it")
	}
}

// 本地用量窗口判罚产生的 cooldown 不该阻止「使用积分」——那正是积分要顶替的东西。
// 这是实测抓到的缺陷：一刀切拦所有 cooldown，会让徽章在最常见的场景
// （发现账号限流了才去开开关）永远不出现。
func TestUsingCreditsIgnoresLocalUsageWindowCooldown(t *testing.T) {
	store := NewStore(nil, nil, nil)
	acc := &Account{
		DBID:        1,
		AccessToken: "at-test",
		PlanType:    "plus",
		Status:      StatusReady,
		HealthTier:  HealthTierHealthy,
	}
	acc.SetUsagePercent7d(100)
	acc.SetReset7dAt(time.Now().Add(72 * time.Hour))

	// 先在没有积分的状态下自然进入用量窗口判罚。
	if !store.MarkUsage7dRateLimited(acc) {
		t.Fatal("MarkUsage7dRateLimited() = false, want true to set up the cooldown")
	}
	if !acc.UsageWindowCooldown() {
		t.Fatal("UsageWindowCooldown() = false, want true for a locally derived usage cooldown")
	}

	// 再打开信用开关并充上积分：应识别为「正在用积分顶」。
	acc.CreditEnabled = true
	acc.CreditSkipUsageWindow = true
	acc.SetCreditBalance("981.7471800000", true, false, false)

	if !acc.UsingCredits() {
		t.Fatal("UsingCredits() = false, want true — a local usage cooldown is exactly what credits cover")
	}
}

// 释放只针对本地用量判罚：上游 429 的冷却（时长不对齐 Reset7dAt）必须保留。
func TestReleaseUsageWindowCooldownKeepsUpstream429(t *testing.T) {
	store := NewStore(nil, nil, nil)
	acc := &Account{
		DBID:        1,
		AccessToken: "at-test",
		PlanType:    "plus",
		Status:      StatusReady,
		HealthTier:  HealthTierHealthy,
	}
	acc.SetUsagePercent7d(100)
	acc.SetReset7dAt(time.Now().Add(72 * time.Hour))
	acc.CreditEnabled = true
	acc.CreditSkipUsageWindow = true
	acc.SetCreditBalance("1000", true, false, false)

	// 上游 429：冷却时长来自限流决策，不会落在 7d 重置时刻上。
	store.MarkCooldown(acc, 15*time.Minute, "rate_limited")

	if acc.UsageWindowCooldown() {
		t.Fatal("UsageWindowCooldown() = true, want false for an upstream 429 cooldown")
	}
	if store.ReleaseUsageWindowCooldownForCredits(acc) {
		t.Fatal("released an upstream 429 cooldown, want it preserved")
	}
	if !acc.HasActiveCooldown() {
		t.Fatal("upstream 429 cooldown was cleared, want it intact")
	}
	if acc.UsingCredits() {
		t.Error("UsingCredits() = true, want false — a real 429 means credits did not cover it")
	}
}

// 开关打开后应主动释放已落下的用量判罚，账号立刻回到可调度。
func TestReleaseUsageWindowCooldownReturnsAccountToScheduling(t *testing.T) {
	store := NewStore(nil, nil, nil)
	acc := &Account{
		DBID:        1,
		AccessToken: "at-test",
		PlanType:    "plus",
		Status:      StatusReady,
		HealthTier:  HealthTierHealthy,
	}
	acc.SetUsagePercent7d(100)
	acc.SetReset7dAt(time.Now().Add(72 * time.Hour))
	if !store.MarkUsage7dRateLimited(acc) {
		t.Fatal("MarkUsage7dRateLimited() = false, want true to set up the cooldown")
	}
	if acc.IsAvailable() {
		t.Fatal("IsAvailable() = true, want false while the usage cooldown stands")
	}

	acc.CreditEnabled = true
	acc.CreditSkipUsageWindow = true
	acc.SetCreditBalance("1000", true, false, false)

	if !store.ReleaseUsageWindowCooldownForCredits(acc) {
		t.Fatal("ReleaseUsageWindowCooldownForCredits() = false, want true")
	}
	if acc.HasActiveCooldown() {
		t.Error("cooldown still active after release")
	}
	if !acc.IsAvailable() {
		t.Error("IsAvailable() = false after release, want true — the badge would otherwise be a lie")
	}
}

// 积分为零时不释放：否则等于把请求送回注定 429 的账号。
func TestReleaseUsageWindowCooldownSkipsWhenNoCredits(t *testing.T) {
	store := NewStore(nil, nil, nil)
	acc := &Account{
		DBID:        1,
		AccessToken: "at-test",
		PlanType:    "plus",
		Status:      StatusReady,
		HealthTier:  HealthTierHealthy,
	}
	acc.SetUsagePercent7d(100)
	acc.SetReset7dAt(time.Now().Add(72 * time.Hour))
	if !store.MarkUsage7dRateLimited(acc) {
		t.Fatal("MarkUsage7dRateLimited() = false, want true to set up the cooldown")
	}

	acc.CreditEnabled = true
	acc.CreditSkipUsageWindow = true
	acc.SetCreditBalance("0", true, false, false)

	if store.ReleaseUsageWindowCooldownForCredits(acc) {
		t.Fatal("released the cooldown with zero credits, want it preserved")
	}
	if !acc.HasActiveCooldown() {
		t.Fatal("cooldown cleared with zero credits")
	}
}

// 正在用积分顶的账号显示为限流，但它其实在正常干活——「一键清理限流账号」必须放过它，
// 否则用户点一下就把好账号删了。这是「显示限流但仍参与调度」这个设计带来的直接风险。
func TestCleanRateLimitedSkipsAccountsUsingCredits(t *testing.T) {
	store := NewStore(nil, nil, nil)

	covered := withCredits(plus5hExhausted(), "981.7471800000", true, false, false)
	covered.DBID = 1
	drained := withCredits(plus5hExhausted(), "0", true, false, false)
	drained.DBID = 2

	// 前置条件：两者都显示为限流，区别只在积分。
	if got := covered.RuntimeStatus(); got != "rate_limited" {
		t.Fatalf("covered RuntimeStatus() = %q, want rate_limited", got)
	}
	if got := drained.RuntimeStatus(); got != "rate_limited" {
		t.Fatalf("drained RuntimeStatus() = %q, want rate_limited", got)
	}
	if !covered.UsingCredits() {
		t.Fatal("covered UsingCredits() = false, want true")
	}
	if drained.UsingCredits() {
		t.Fatal("drained UsingCredits() = true, want false")
	}

	store.AddAccounts([]*Account{covered, drained})

	// 无 db 时清理不会真正删除，但计数反映了哪些账号被选中。
	cleaned := store.CleanRateLimitedManual(context.Background())
	if cleaned != 1 {
		t.Fatalf("CleanRateLimitedManual() = %d, want 1 (only the drained account)", cleaned)
	}
}

// error 态同样优先于 using_credits。
func TestUsingCreditsDoesNotMaskError(t *testing.T) {
	acc := withCredits(plus5hExhausted(), "1000.0000000000", true, false, false)
	acc.Status = StatusError
	acc.ErrorMsg = "boom"

	if got := acc.RuntimeStatus(); got != "error" {
		t.Fatalf("RuntimeStatus() = %q, want error", got)
	}
}

// 窗口没打满时不该显示 using_credits——那会把健康账号说成正在烧积分。
func TestUsingCreditsOnlyWhenWindowActuallyExhausted(t *testing.T) {
	acc := withCredits(plus5hExhausted(), "1000.0000000000", true, false, false)
	acc.UsagePercent5h = 40

	if got := acc.RuntimeStatus(); got != "active" {
		t.Fatalf("RuntimeStatus() = %q, want active while the window still has room", got)
	}
	if acc.UsingCredits() {
		t.Error("UsingCredits() = true, want false — a healthy window is not burning credits")
	}
}

// Free 账号 7d 耗尽同样走积分门：有积分顶替、归零恢复 usage_exhausted。
func TestUsingCreditsCoversFreeUsageExhausted(t *testing.T) {
	base := func() *Account {
		return &Account{
			DBID:                1,
			AccessToken:         "at-test",
			Status:              StatusReady,
			PlanType:            "free",
			UsagePercent7d:      100,
			UsagePercent7dValid: true,
		}
	}

	withBalance := withCredits(base(), "50.0000000000", true, false, false)
	if got := withBalance.RuntimeStatus(); got != "usage_exhausted" {
		t.Errorf("RuntimeStatus() = %q, want usage_exhausted for a free account with credits", got)
	}
	if !withBalance.IsAvailable() {
		t.Error("IsAvailable() = false, want true — credits keep a free account schedulable")
	}
	if !withBalance.UsingCredits() {
		t.Error("UsingCredits() = false, want true for a free account with credits")
	}

	drained := withCredits(base(), "0", true, false, false)
	if got := drained.RuntimeStatus(); got != "usage_exhausted" {
		t.Errorf("RuntimeStatus() = %q, want usage_exhausted once credits are gone", got)
	}
}

// ignore_usage_limit_status 是另一条独立开关：它只放行已绑定活跃轮次，
// 新轮次仍被阻止，因此后台继续显示限流，但不能显示成「使用积分」。
func TestIgnoreUsageLimitStatusKeepsFreshDispatchRateLimitedWithoutCredits(t *testing.T) {
	acc := plus5hExhausted()
	ignore := true
	acc.IgnoreUsageLimitStatusOverride = &ignore
	acc.recomputeEffectiveIgnoreUsageLimitStatus(false)

	if got := acc.RuntimeStatus(); got != "rate_limited" {
		t.Fatalf("RuntimeStatus() = %q, want rate_limited while fresh dispatch remains blocked", got)
	}
	if acc.UsingCredits() {
		t.Error("UsingCredits() = true, want false — ignore-usage-limit is a separate switch, not credits")
	}
}

// 积分快照必须能穿过重启：只留在内存的话，重启后账号被判成「没积分」，
// 积分顶替限流失效，还会被「清理限流账号」当成真限流删掉。
func TestCreditBalanceSnapshotRoundTrip(t *testing.T) {
	raw, err := MarshalCreditBalanceSnapshot(CreditBalanceSnapshot{
		Balance:    "981.7471800000",
		HasCredits: true,
		UpdatedAt:  time.Unix(1754200000, 0),
	})
	if err != nil {
		t.Fatalf("MarshalCreditBalanceSnapshot: %v", err)
	}

	// 重启后的账号：两个开关来自库里，积分快照未恢复时按"没积分"处理。
	acc := plus5hExhausted()
	acc.CreditEnabled = true
	acc.CreditSkipUsageWindow = true
	if acc.UsingCredits() {
		t.Fatal("UsingCredits() = true before restore, want false — an unprobed balance is not credits")
	}

	if !acc.RestoreCreditBalanceFromJSON(raw) {
		t.Fatal("RestoreCreditBalanceFromJSON() = false, want true")
	}
	balance, hasCredits, unlimited, overage, ok := acc.GetCreditBalance()
	if !ok || balance != "981.7471800000" || !hasCredits || unlimited || overage {
		t.Fatalf("GetCreditBalance() = %q/%t/%t/%t/%t, want 981.7471800000/true/false/false/true",
			balance, hasCredits, unlimited, overage, ok)
	}
	if !acc.UsingCredits() {
		t.Error("UsingCredits() = false after restore, want true — the restored balance must keep the override alive")
	}
	if got := acc.RuntimeStatus(); got != "rate_limited" {
		t.Errorf("RuntimeStatus() = %q, want rate_limited — 状态仍显示限流，只是被积分顶着", got)
	}
}

// 没有快照 / 快照损坏 / 全空快照都不能凭空造出一个可用余额。
func TestRestoreCreditBalanceRejectsEmptyOrInvalid(t *testing.T) {
	empty, err := MarshalCreditBalanceSnapshot(CreditBalanceSnapshot{})
	if err != nil {
		t.Fatalf("MarshalCreditBalanceSnapshot: %v", err)
	}
	for _, raw := range []string{"", "   ", "not-json", empty} {
		acc := plus5hExhausted()
		acc.CreditEnabled = true
		acc.CreditSkipUsageWindow = true
		if acc.RestoreCreditBalanceFromJSON(raw) {
			t.Errorf("RestoreCreditBalanceFromJSON(%q) = true, want false", raw)
		}
		if _, _, _, _, ok := acc.GetCreditBalance(); ok {
			t.Errorf("RestoreCreditBalanceFromJSON(%q) marked the balance as probed", raw)
		}
		if acc.UsingCredits() {
			t.Errorf("RestoreCreditBalanceFromJSON(%q) enabled the credits override", raw)
		}
	}
}

// 余额为 0 的快照要能恢复：它是「积分已用尽」的权威结论，恢复后应恢复真实限流，
// 而不是退回「未探测」——否则重启后又会把请求送进注定 429 的账号。
func TestRestoreCreditBalanceKeepsDrainedBalance(t *testing.T) {
	raw, err := MarshalCreditBalanceSnapshot(CreditBalanceSnapshot{Balance: "0", HasCredits: true})
	if err != nil {
		t.Fatalf("MarshalCreditBalanceSnapshot: %v", err)
	}
	acc := plus5hExhausted()
	acc.CreditEnabled = true
	acc.CreditSkipUsageWindow = true
	if !acc.RestoreCreditBalanceFromJSON(raw) {
		t.Fatal("RestoreCreditBalanceFromJSON() = false, want true for a drained balance")
	}
	if acc.UsingCredits() {
		t.Error("UsingCredits() = true for a zero balance, want false")
	}
	if acc.IsAvailable() {
		t.Error("IsAvailable() = true for a drained credit account, want false")
	}
}
