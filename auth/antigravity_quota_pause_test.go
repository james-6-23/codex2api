package auth

import (
	"testing"
	"time"
)

// 用量自动暂停按模型族生效：Gemini 5h 用到阈值只挡 Gemini，Claude 照常；整号不阻断。
func TestAntigravityQuotaPauseIsolatesFamilies(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	reset5h := now.Add(2 * time.Hour)
	snap := agFamilySnapshot(now,
		agFamilyBucket("gemini-5h", "5h", agFloat(0.05), now, reset5h),
		agFamilyBucket("gemini-weekly", "weekly", agFloat(0.6), now, now.Add(72*time.Hour)),
		agFamilyBucket("3p-5h", "5h", agFloat(0.5), now, now.Add(3*time.Hour)),
		agFamilyBucket("3p-weekly", "weekly", agFloat(0.9), now, now.Add(72*time.Hour)),
	)
	pause := antigravityQuotaPause{fiveHour: 0.95}

	blocked, reason, resetAt := antigravityQuotaSnapshotGateBlockForModel(&snap, "gemini-3.8-flash-high", now, pause)
	if !blocked || reason != "auto_pause" || !resetAt.Equal(reset5h) {
		t.Fatalf("gemini at 95%% used must pause until 5h reset, got %v %q %v", blocked, reason, resetAt)
	}
	if blocked, _, _ := antigravityQuotaSnapshotGateBlockForModel(&snap, "claude-sonnet-4-6", now, pause); blocked {
		t.Fatal("claude family below threshold must stay schedulable")
	}
	if blocked, _, _ := antigravityQuotaSnapshotGateBlock(&snap, now, pause); blocked {
		t.Fatal("whole account must stay available while one family is usable")
	}
	if blocked, _, _ := antigravityQuotaSnapshotGateBlockForModel(&snap, "gemini-3.8-flash-high", now, antigravityQuotaPause{}); blocked {
		t.Fatal("no threshold configured must keep the hard-zero-only behavior")
	}
	// 周阈值只看周桶：Gemini 周已用 40% 不触发 0.5 以上的周阈值，Claude 周已用 10% 也不触发。
	if blocked, _, _ := antigravityQuotaSnapshotGateBlockForModel(&snap, "gemini-3.8-flash-high", now, antigravityQuotaPause{weekly: 0.5}); blocked {
		t.Fatal("weekly threshold must not read the 5h bucket")
	}
	if blocked, _, _ := antigravityQuotaSnapshotGateBlockForModel(&snap, "gemini-3.8-flash-high", now, antigravityQuotaPause{weekly: 0.4}); !blocked {
		t.Fatal("weekly usage at the weekly threshold must pause the family")
	}
	// 重置时刻已过：窗口已滚动，阈值不再生效。
	if blocked, _, _ := antigravityQuotaSnapshotGateBlockForModel(&snap, "gemini-3.8-flash-high", reset5h.Add(time.Minute), pause); blocked {
		t.Fatal("threshold pause must lift after the window reset")
	}
}

// 账号级：阈值走账号生效值，禁用开关优先。
func TestAntigravityAccountQuotaPauseUsesEffectiveThresholds(t *testing.T) {
	now := time.Now().UTC()
	acc := &Account{UpstreamType: UpstreamAntigravity, AntigravityQuota: agFamilySnapshot(now,
		agFamilyBucket("gemini-5h", "5h", agFloat(0.02), now, now.Add(time.Hour)),
		agFamilyBucket("3p-5h", "5h", agFloat(0.01), now, now.Add(2*time.Hour)),
	)}
	acc.effectiveAutoPause5h = 0.95
	if exhausted, reason, _ := acc.IsAntigravityQuotaExhaustedLocked(now); !exhausted || reason != "auto_pause" {
		t.Fatalf("both families over the 5h threshold must pause the account, got %v %q", exhausted, reason)
	}
	acc.AutoPause5hDisabled = true
	if exhausted, _, _ := acc.IsAntigravityQuotaExhaustedLocked(now); exhausted {
		t.Fatal("disabled 5h auto-pause must not block")
	}
}

// 请求次数限流跟最早到期的 Antigravity 配额窗口重置，而不是 24h 兜底。
func TestAntigravityDispatchCountResetFollowsQuotaWindows(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	soonest := now.Add(90 * time.Minute)
	acc := &Account{UpstreamType: UpstreamAntigravity, AntigravityQuota: agFamilySnapshot(now,
		agFamilyBucket("gemini-weekly", "weekly", agFloat(0.9), now, now.Add(72*time.Hour)),
		agFamilyBucket("gemini-5h", "5h", agFloat(0.9), now, now.Add(3*time.Hour)),
		agFamilyBucket("3p-5h", "5h", agFloat(1), now, soonest),
	)}
	if got := acc.dispatchCountResetAtLocked(now); !got.Equal(soonest) {
		t.Fatalf("reset = %v, want soonest quota reset %v", got, soonest)
	}
	empty := &Account{UpstreamType: UpstreamAntigravity}
	if got := empty.dispatchCountResetAtLocked(now); !got.Equal(now.Add(dispatchCountFallbackWindow)) {
		t.Fatalf("without quota resets the 24h fallback must remain, got %v", got)
	}
}
