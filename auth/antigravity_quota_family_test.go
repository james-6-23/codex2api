package auth

import (
	"fmt"
	"testing"
	"time"
)

// agFamilyBucket 构造一个可信观测的配额桶：remaining 为 nil 表示上游未报数值。
func agFamilyBucket(id, window string, remaining *float64, observed, reset time.Time) AntigravityQuotaBucket {
	b := AntigravityQuotaBucket{
		BucketID:          id,
		Window:            window,
		RemainingFraction: remaining,
		FractionPresent:   remaining != nil,
		ObservedAt:        observed,
		ObservationSeq:    7,
		TTLSeconds:        3600,
	}
	if !reset.IsZero() {
		b.ResetTime = reset.UTC().Format(time.RFC3339)
	}
	return b
}

func agFloat(v float64) *float64 { return &v }

// agFamilySnapshot 构造带可信 groups 元数据的 V2 快照，分组即上游真实的两组形态。
func agFamilySnapshot(observed time.Time, buckets ...AntigravityQuotaBucket) AntigravityQuotaSnapshot {
	return AntigravityQuotaSnapshot{
		SchemaVersion: 2,
		SourceMetadata: map[string]SourceMeta{
			"groups": {AppliedSeq: 7, ObservedAt: observed, PresenceVerified: true, SummaryIntegrity: true},
		},
		Groups: []AntigravityQuotaGroup{{DisplayName: "mixed", Buckets: buckets}},
	}
}

func TestAntigravityQuotaFamilyClassification(t *testing.T) {
	buckets := map[string]antigravityQuotaFamily{
		"gemini-weekly":           antigravityQuotaFamilyGemini,
		"gemini-5h":               antigravityQuotaFamilyGemini,
		"3p-weekly":               antigravityQuotaFamilyThirdParty,
		"3p-5h":                   antigravityQuotaFamilyThirdParty,
		"gemini-argon-wtu-weekly": antigravityQuotaFamilyArgon,
		"":                        antigravityQuotaFamilyUnknown,
		"w1":                      antigravityQuotaFamilyUnknown,
	}
	for id, want := range buckets {
		if got := antigravityBucketQuotaFamily(id); got != want {
			t.Errorf("bucket %q: got %q want %q", id, got, want)
		}
	}
	models := map[string]antigravityQuotaFamily{
		"gemini-3.8-flash-high":    antigravityQuotaFamilyGemini,
		"gemini-pro-agent":         antigravityQuotaFamilyGemini,
		"models/gemini-2.5-pro":    antigravityQuotaFamilyGemini,
		"gemini-4-argon-high":      antigravityQuotaFamilyArgon,
		"claude-sonnet-4-6":        antigravityQuotaFamilyThirdParty,
		"claude-opus-4-6-thinking": antigravityQuotaFamilyThirdParty,
		"gpt-oss-120b-medium":      antigravityQuotaFamilyThirdParty,
		"":                         antigravityQuotaFamilyUnknown,
		"mystery-model":            antigravityQuotaFamilyUnknown,
	}
	for model, want := range models {
		if got := antigravityModelQuotaFamily(model); got != want {
			t.Errorf("model %q: got %q want %q", model, got, want)
		}
	}
}

// 某一族周配额为零只挡该族的请求：整号不阻断，Claude 请求被挡、Gemini 请求放行。
func TestAntigravityQuotaGateIsolatesFamilies(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	weeklyReset := now.Add(48 * time.Hour)
	snap := agFamilySnapshot(now,
		agFamilyBucket("gemini-weekly", "weekly", agFloat(0.9), now, weeklyReset),
		agFamilyBucket("gemini-5h", "5h", agFloat(1), now, now.Add(3*time.Hour)),
		agFamilyBucket("3p-weekly", "weekly", agFloat(0), now, weeklyReset),
		agFamilyBucket("3p-5h", "5h", agFloat(1), now, now.Add(3*time.Hour)),
	)

	if blocked, _, _ := antigravityQuotaSnapshotGateBlock(&snap, now); blocked {
		t.Fatal("account must stay schedulable while the Gemini family still has quota")
	}
	blocked, reason, resetAt := antigravityQuotaSnapshotGateBlockForModel(&snap, "claude-sonnet-4-6", now)
	if !blocked || reason != "known_zero" || !resetAt.Equal(weeklyReset) {
		t.Fatalf("claude request: blocked=%v reason=%q reset=%v, want blocked until %v", blocked, reason, resetAt, weeklyReset)
	}
	if blocked, _, _ := antigravityQuotaSnapshotGateBlockForModel(&snap, "gemini-3.8-flash-high", now); blocked {
		t.Fatal("gemini request must not be blocked by the exhausted 3p family")
	}
	// 无法归族的模型退回严格口径：任一桶为零即阻断。
	if blocked, _, _ := antigravityQuotaSnapshotGateBlockForModel(&snap, "mystery-model", now); !blocked {
		t.Fatal("unclassified model must fall back to the strict any-bucket gate")
	}
}

// 所有已识别族都耗尽时整号阻断，到期取最早恢复的那一族；同族多个零桶取最晚到期。
func TestAntigravityQuotaGateAllFamiliesExhausted(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	snap := agFamilySnapshot(now,
		agFamilyBucket("gemini-weekly", "weekly", agFloat(0), now, now.Add(72*time.Hour)),
		agFamilyBucket("gemini-5h", "5h", agFloat(0), now, now.Add(2*time.Hour)),
		agFamilyBucket("3p-weekly", "weekly", agFloat(0.5), now, now.Add(72*time.Hour)),
		agFamilyBucket("3p-5h", "5h", agFloat(0), now, now.Add(4*time.Hour)),
	)
	blocked, _, resetAt := antigravityQuotaSnapshotGateBlock(&snap, now)
	if !blocked {
		t.Fatal("account with every family exhausted must be blocked")
	}
	// Gemini 族要等周重置（72h），3p 族 5h 后恢复：整号最早 4h 后可用。
	if want := now.Add(4 * time.Hour); !resetAt.Equal(want) {
		t.Fatalf("reset=%v want earliest family recovery %v", resetAt, want)
	}
	_, _, geminiReset := antigravityQuotaSnapshotGateBlockForModel(&snap, "gemini-3.8-flash-high", now)
	if want := now.Add(72 * time.Hour); !geminiReset.Equal(want) {
		t.Fatalf("gemini reset=%v want latest zero bucket %v", geminiReset, want)
	}
}

// 无法归族的零桶对所有请求生效（保守）。
func TestAntigravityQuotaGateUnknownBucketIsAccountWide(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	snap := agFamilySnapshot(now,
		agFamilyBucket("gemini-weekly", "weekly", agFloat(1), now, now.Add(72*time.Hour)),
		agFamilyBucket("mystery-weekly", "weekly", agFloat(0), now, now.Add(24*time.Hour)),
	)
	if blocked, _, _ := antigravityQuotaSnapshotGateBlock(&snap, now); !blocked {
		t.Fatal("unknown-family zero bucket must block the account")
	}
	if blocked, _, _ := antigravityQuotaSnapshotGateBlockForModel(&snap, "gemini-3.8-flash-high", now); !blocked {
		t.Fatal("unknown-family zero bucket must block every model")
	}
}

// Argon：有独立桶时只看 Argon 桶；没有时回退 Gemini 族。
func TestAntigravityQuotaGateArgonScope(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	withArgon := agFamilySnapshot(now,
		agFamilyBucket("gemini-weekly", "weekly", agFloat(1), now, now.Add(72*time.Hour)),
		agFamilyBucket("gemini-argon-wtu-weekly", "weekly", agFloat(0), now, now.Add(72*time.Hour)),
	)
	if blocked, _, _ := antigravityQuotaSnapshotGateBlockForModel(&withArgon, "gemini-4-argon-high", now); !blocked {
		t.Fatal("argon request must be blocked by the exhausted argon bucket")
	}
	if blocked, _, _ := antigravityQuotaSnapshotGateBlockForModel(&withArgon, "gemini-3.8-flash-high", now); blocked {
		t.Fatal("flash request must not be blocked by the argon bucket")
	}

	withoutArgon := agFamilySnapshot(now,
		agFamilyBucket("gemini-weekly", "weekly", agFloat(0), now, now.Add(72*time.Hour)),
		agFamilyBucket("3p-weekly", "weekly", agFloat(1), now, now.Add(72*time.Hour)),
	)
	if blocked, _, _ := antigravityQuotaSnapshotGateBlockForModel(&withoutArgon, "gemini-4-argon-high", now); !blocked {
		t.Fatal("argon request without an argon bucket must follow the gemini family")
	}
}

// 模型级过滤器：被门禁挡掉的账号不触发调用方 filter；空模型原样返回 filter。
func TestWithAntigravityModelQuotaFilter(t *testing.T) {
	now := time.Now()
	acc := &Account{UpstreamType: UpstreamAntigravity, AntigravityQuota: agFamilySnapshot(now,
		agFamilyBucket("gemini-weekly", "weekly", agFloat(1), now, now.Add(72*time.Hour)),
		agFamilyBucket("3p-weekly", "weekly", agFloat(0), now, now.Add(72*time.Hour)),
	)}
	calls := 0
	inner := func(*Account) bool { calls++; return true }

	if withAntigravityModelQuotaFilter(inner, "claude-sonnet-4-6")(acc) {
		t.Fatal("claude request must be filtered out")
	}
	if calls != 0 {
		t.Fatalf("inner filter must not run for a gated account, calls=%d", calls)
	}
	if !withAntigravityModelQuotaFilter(inner, "gemini-3.8-flash-high")(acc) || calls != 1 {
		t.Fatalf("gemini request must pass through to the inner filter once, calls=%d", calls)
	}
	if !withAntigravityModelQuotaFilter(nil, "gemini-3.8-flash-high")(acc) {
		t.Fatal("nil inner filter must accept an ungated account")
	}
	if withAntigravityModelQuotaFilter(nil, "  ") != nil {
		t.Fatal("empty model must leave the filter untouched")
	}
}

// 选号与 429 判定：3p 族耗尽的号池，Claude 请求拿不到号且判定为配额耗尽，Gemini 照常。
func TestAntigravityModelQuotaGateSelectionAndExhaustion(t *testing.T) {
	store := newAntigravityBalanceStore(t, 2)
	defer store.Stop()
	now := time.Now()
	for _, id := range []int64{1, 2} {
		acc := store.FindByID(id)
		acc.mu.Lock()
		acc.AntigravityQuota = agFamilySnapshot(now,
			agFamilyBucket("gemini-weekly", "weekly", agFloat(0.8), now, now.Add(72*time.Hour)),
			agFamilyBucket("3p-weekly", "weekly", agFloat(0), now, now.Add(72*time.Hour)),
		)
		acc.mu.Unlock()
	}

	claude := antigravitySelectionOpts()
	claude.Model = "claude-sonnet-4-6"
	if acc, _, _ := store.NextForSessionWithDispatchGuardForProvider(claude, "claude-session", 0, nil, nil); acc != nil {
		store.Release(acc)
		t.Fatalf("claude request selected account %d despite exhausted 3p family", acc.DBID)
	}
	if err := store.AntigravityQuotaGateExhaustion("claude-session", 0, nil, nil, false, claude.Model); err == nil {
		t.Fatal("claude request must be reported as quota exhausted")
	}

	gemini := antigravitySelectionOpts()
	acc, _, _ := store.NextForSessionWithDispatchGuardForProvider(gemini, "gemini-session", 0, nil, nil)
	if acc == nil {
		t.Fatal("gemini request must still get an account")
	}
	store.Release(acc)
	if err := store.AntigravityQuotaGateExhaustion("gemini-session-2", 0, nil, nil, false, gemini.Model); err != nil {
		t.Fatalf("gemini request must not be reported as quota exhausted: %v", err)
	}
}

func TestComputeQuotaPressureForModelUsesFamilyBuckets(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	snap := agFamilySnapshot(now,
		agFamilyBucket("gemini-weekly", "weekly", agFloat(0.9), now, now.Add(72*time.Hour)),
		agFamilyBucket("3p-weekly", "weekly", agFloat(0.1), now, now.Add(72*time.Hour)),
	)
	if p := snap.ComputeQuotaPressureForModel("gemini-3.8-flash-high", now); p != 0.1 {
		t.Fatalf("gemini pressure=%v want 0.1", p)
	}
	if p := snap.ComputeQuotaPressureForModel("claude-sonnet-4-6", now); p != 0.9 {
		t.Fatalf("claude pressure=%v want 0.9", p)
	}
	// 整号口径：同窗口取最紧桶。
	if p := snap.ComputeQuotaPressure(now); p != 0.9 {
		t.Fatalf("account-wide pressure=%v want 0.9", p)
	}
}

func TestAntigravityExpiryUrgency(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	horizon := 24 * time.Hour
	mk := func(buckets ...AntigravityQuotaBucket) *Account {
		return &Account{UpstreamType: UpstreamAntigravity, AntigravityQuota: agFamilySnapshot(now, buckets...)}
	}

	acc := mk(
		agFamilyBucket("gemini-weekly", "weekly", agFloat(0.6), now, now.Add(12*time.Hour)),
		agFamilyBucket("gemini-5h", "5h", agFloat(1), now, now.Add(1*time.Hour)),
		agFamilyBucket("3p-weekly", "weekly", agFloat(0.6), now, now.Add(100*time.Hour)),
	)
	score, urgent := acc.antigravityExpiryUrgency("gemini-3.8-flash-high", horizon, now)
	if !urgent || score != 0.05 {
		t.Fatalf("gemini urgency=%v urgent=%v, want 0.6/12=0.05", score, urgent)
	}
	if _, urgent := acc.antigravityExpiryUrgency("claude-sonnet-4-6", horizon, now); urgent {
		t.Fatal("3p weekly resets beyond the horizon: claude must not be urgent")
	}
	if _, urgent := acc.antigravityExpiryUrgency("mystery-model", horizon, now); urgent {
		t.Fatal("unclassified model must never be urgent")
	}

	// 5h 窗口不参与：只有 5h 桶时不临期。
	only5h := mk(agFamilyBucket("gemini-5h", "5h", agFloat(1), now, now.Add(1*time.Hour)))
	if _, urgent := only5h.antigravityExpiryUrgency("gemini-3.8-flash-high", horizon, now); urgent {
		t.Fatal("5h-only snapshot must not be urgent")
	}
	// 存量不足 5% 不冲刺。
	low := mk(agFamilyBucket("gemini-weekly", "weekly", agFloat(0.04), now, now.Add(2*time.Hour)))
	if _, urgent := low.antigravityExpiryUrgency("gemini-3.8-flash-high", horizon, now); urgent {
		t.Fatal("nearly exhausted weekly quota must not be urgent")
	}
	// 观测过期不冲刺。
	stale := mk(agFamilyBucket("gemini-weekly", "weekly", agFloat(0.9), now.Add(-2*time.Hour), now.Add(6*time.Hour)))
	if _, urgent := stale.antigravityExpiryUrgency("gemini-3.8-flash-high", horizon, now); urgent {
		t.Fatal("stale observation must not be urgent")
	}
	// 分母下限：离重置 6 分钟时按 0.5h 计。
	soon := mk(agFamilyBucket("gemini-weekly", "weekly", agFloat(0.5), now, now.Add(6*time.Minute)))
	if score, urgent := soon.antigravityExpiryUrgency("gemini-3.8-flash-high", horizon, now); !urgent || score != 1 {
		t.Fatalf("near-reset urgency=%v urgent=%v, want 0.5/0.5=1", score, urgent)
	}
}

// setAntigravityExpiryPriorityForTest 打开临期优先并在测试结束时恢复原配置。
func setAntigravityExpiryPriorityForTest(t *testing.T, enabled bool, hours int) {
	t.Helper()
	prev := ConfiguredAntigravitySettings()
	t.Cleanup(func() { SetConfiguredAntigravitySettings(prev) })
	next := prev
	next.ExpiryPriorityEnabled = &enabled
	next.ExpiryPriorityHorizonHours = &hours
	SetConfiguredAntigravitySettings(next)
}

// 临期优先：开启后新会话集中到请求模型所在族周配额临期的账号；其它族的请求、或开关
// 关闭时，仍按原打分摊开。
func TestAntigravityBalancedSelectionPrefersExpiringFamily(t *testing.T) {
	store := newAntigravityBalanceStore(t, 4)
	defer store.Stop()
	now := time.Now()
	for _, id := range []int64{1, 2, 3, 4} {
		geminiReset := now.Add(120 * time.Hour)
		if id == 3 {
			geminiReset = now.Add(10 * time.Hour) // 账号 3 的 Gemini 周配额临期
		}
		acc := store.FindByID(id)
		acc.mu.Lock()
		acc.AntigravityQuota = agFamilySnapshot(now,
			agFamilyBucket("gemini-weekly", "weekly", agFloat(0.7), now, geminiReset),
			agFamilyBucket("3p-weekly", "weekly", agFloat(0.7), now, now.Add(120*time.Hour)),
		)
		acc.mu.Unlock()
	}

	pick := func(model, prefix string) map[int64]int {
		opts := antigravitySelectionOpts()
		opts.Model = model
		counts := map[int64]int{}
		for i := 0; i < 40; i++ {
			acc := store.nextAccountForFreshAffinityWithDispatch(fmt.Sprintf("%s-%d", prefix, i), 0, nil, nil, opts)
			if acc == nil {
				t.Fatal("selection returned no account")
			}
			counts[acc.DBID]++
			store.Release(acc)
		}
		return counts
	}

	if counts := pick("gemini-3.8-flash-high", "off"); len(counts) < 2 {
		t.Fatalf("expiry priority disabled: selections must spread, got %v", counts)
	}

	setAntigravityExpiryPriorityForTest(t, true, 24)
	if counts := pick("gemini-3.8-flash-high", "on"); counts[3] != 40 {
		t.Fatalf("expiring gemini account 3 must take every new session, got %v", counts)
	}
	if counts := pick("claude-sonnet-4-6", "claude"); len(counts) < 2 {
		t.Fatalf("claude requests must not be pulled onto the gemini-expiring account, got %v", counts)
	}

	// 临期账号并发占满后，回落到其余账号。
	saturateAntigravityAccount(store.FindByID(3))
	opts := antigravitySelectionOpts()
	acc := store.nextAccountForFreshAffinityWithDispatch("saturated", 0, nil, nil, opts)
	if acc == nil || acc.DBID == 3 {
		t.Fatalf("saturated expiring account must yield to others, got %v", acc)
	}
	store.Release(acc)
}
