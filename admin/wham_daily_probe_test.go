package admin

import (
	"testing"
	"time"

	"github.com/codex2api/auth"
)

func rateLimitedProbeAccount(id int64, now time.Time) *auth.Account {
	return &auth.Account{
		DBID:           id,
		AccessToken:    "token",
		Status:         auth.StatusCooldown,
		CooldownUtil:   now.Add(time.Hour),
		CooldownReason: "rate_limited",
	}
}

func TestWhamDailyUsageDueTargetsUsesPerStatusIntervals(t *testing.T) {
	now := time.Now()
	active := &auth.Account{DBID: 1, AccessToken: "token"}
	limited := rateLimitedProbeAccount(2, now)
	authoritative := &auth.Account{DBID: 3, AccessToken: "token"}
	authoritative.SetCooldownUntil(now.Add(12*time.Hour), auth.ResponsesRateLimitedCooldownReason)
	all := []*auth.Account{active, limited, authoritative}

	// 首轮：没有计时记录，三个账号都要刷。
	lastAttempt := map[int64]time.Time{}
	if targets := whamDailyUsageDueTargets(all, lastAttempt, now); len(targets) != 3 {
		t.Fatalf("first round targets = %d, want 3", len(targets))
	}

	// 半小时后：正常账号未到 1h，限流账号未到 6h，都不刷。
	half := now.Add(30 * time.Minute)
	if targets := whamDailyUsageDueTargets(all, lastAttempt, half); len(targets) != 0 {
		t.Fatalf("30m targets = %d, want 0", len(targets))
	}

	// 一小时后：正常账号到期，限流账号继续等。
	hourLater := now.Add(time.Hour)
	targets := whamDailyUsageDueTargets(all, lastAttempt, hourLater)
	if len(targets) != 1 || targets[0].DBID != 1 {
		t.Fatalf("1h targets = %+v, want only account 1", targets)
	}

	// 六小时后：限流账号也到期了。
	sixLater := now.Add(6 * time.Hour)
	targets = whamDailyUsageDueTargets(all, lastAttempt, sixLater)
	found := map[int64]bool{}
	for _, account := range targets {
		found[account.DBID] = true
	}
	if !found[2] {
		t.Fatalf("6h targets = %+v, want rate-limited account 2 due", targets)
	}
	if !found[3] {
		t.Fatalf("6h targets = %+v, want Responses-limited account 3 due", targets)
	}
}

func TestWhamDailyUsageDueTargetsRecoveredAccountResumesHourly(t *testing.T) {
	now := time.Now()
	limited := rateLimitedProbeAccount(3, now)
	lastAttempt := map[int64]time.Time{}
	if targets := whamDailyUsageDueTargets([]*auth.Account{limited}, lastAttempt, now); len(targets) != 1 {
		t.Fatalf("first round targets = %d, want 1", len(targets))
	}

	// 冷却结束恢复后，按正常 1h 间隔判定，而不是继续背着 6h。
	recovered := &auth.Account{DBID: 3, AccessToken: "token"}
	hourLater := now.Add(time.Hour)
	if targets := whamDailyUsageDueTargets([]*auth.Account{recovered}, lastAttempt, hourLater); len(targets) != 1 {
		t.Fatalf("recovered 1h targets = %d, want 1", len(targets))
	}
}

func TestWhamDailyUsageAutoRefreshEligibleSkipsErrorBannedAndNewAccounts(t *testing.T) {
	now := time.Now()
	ready := &auth.Account{DBID: 1, AccessToken: "at", AddedAt: now.Add(-25 * time.Hour).UnixNano()}
	if !whamDailyUsageAutoRefreshEligible(ready, now) {
		t.Fatal("account older than one day should auto-refresh")
	}

	fresh := &auth.Account{DBID: 2, AccessToken: "at", AddedAt: now.Add(-2 * time.Hour).UnixNano()}
	if whamDailyUsageAutoRefreshEligible(fresh, now) {
		t.Fatal("account imported within one day should wait for official settlement")
	}

	errored := &auth.Account{DBID: 3, AccessToken: "at", Status: auth.StatusError, AddedAt: now.Add(-48 * time.Hour).UnixNano()}
	if whamDailyUsageAutoRefreshEligible(errored, now) {
		t.Fatal("error account should not auto-refresh official usage")
	}

	banned := &auth.Account{
		DBID: 4, AccessToken: "at", Status: auth.StatusCooldown,
		CooldownUtil: now.Add(time.Hour), CooldownReason: "unauthorized",
		AddedAt: now.Add(-48 * time.Hour).UnixNano(),
	}
	if whamDailyUsageAutoRefreshEligible(banned, now) {
		t.Fatal("banned account should not auto-refresh official usage")
	}
}

func TestWhamDailyUsageBackfillEligibleSkipsRelayGrokAndCodexAT(t *testing.T) {
	if !whamDailyUsageBackfillEligible(&auth.Account{DBID: 1, AccessToken: "at"}) {
		t.Fatal("codex oauth account should be eligible")
	}
	if whamDailyUsageBackfillEligible(&auth.Account{DBID: 2}) {
		t.Fatal("missing access token should be skipped")
	}
	grok := &auth.Account{DBID: 3, AccessToken: "at", UpstreamType: auth.UpstreamGrok, APIKey: "gk"}
	if !grok.IsGrokAPI() {
		t.Fatal("test grok account is not classified as grok")
	}
	if whamDailyUsageBackfillEligible(grok) {
		t.Fatal("grok account should be skipped")
	}
	if whamDailyUsageBackfillEligible(&auth.Account{DBID: 4, AccessToken: "at-opaque"}) {
		t.Fatal("codex_at account should be skipped")
	}
	claude := &auth.Account{DBID: 5, AccessToken: "claude-token", RefreshToken: "claude-refresh", UpstreamType: auth.UpstreamClaude}
	if whamDailyUsageBackfillEligible(claude) {
		t.Fatal("Claude account must not use the ChatGPT WHAM daily usage endpoint")
	}
}

func TestWhamDailyUsageDueTargetsPrunesRemovedAccounts(t *testing.T) {
	now := time.Now()
	kept := &auth.Account{DBID: 10, AccessToken: "token"}
	removed := &auth.Account{DBID: 11, AccessToken: "token"}
	lastAttempt := map[int64]time.Time{}
	whamDailyUsageDueTargets([]*auth.Account{kept, removed}, lastAttempt, now)
	whamDailyUsageDueTargets([]*auth.Account{kept}, lastAttempt, now.Add(time.Hour))
	if _, ok := lastAttempt[11]; ok {
		t.Fatalf("removed account should be pruned from timer map: %v", lastAttempt)
	}
	if _, ok := lastAttempt[10]; !ok {
		t.Fatalf("kept account timer entry missing: %v", lastAttempt)
	}
}
