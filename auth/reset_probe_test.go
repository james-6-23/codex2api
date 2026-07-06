package auth

import (
	"testing"
	"time"
)

// TestNeedsUsageProbeFiresAtResetBoundary 验证：5h/7d 窗口重置时刻一到，
// 若用量快照仍停留在重置前采集的数据，就立即触发一次探测（用于识别"倒计时归零后
// 看似可用、实际已被封禁"的账号）；探测刷新快照后不再重复触发。
func TestNeedsUsageProbeFiresAtResetBoundary(t *testing.T) {
	now := time.Now()
	maxAge := 10 * time.Minute

	// base 构造一个「除待测窗口外一切正常/新鲜」的账号，隔离其它探测分支。
	base := func() *Account {
		return &Account{
			AccessToken:          "at-x",
			Status:               StatusReady,
			PlanType:             "", // 非 premium，避开 premium5h 分支
			resetCreditsProbedAt: now,
			UsagePercent7d:       50,
			UsagePercent7dValid:  true,
			UsageUpdatedAt:       now,
			Reset7dAt:            now.Add(time.Hour),
			UsagePercent5h:       10,
			UsagePercent5hValid:  true,
			UsageUpdatedAt5h:     now,
			Reset5hAt:            now.Add(time.Hour),
		}
	}

	t.Run("5h 重置到点且快照过期→探测", func(t *testing.T) {
		a := base()
		a.UsagePercent5h = 100
		a.Reset5hAt = now.Add(-time.Minute)
		a.UsageUpdatedAt5h = now.Add(-6 * time.Minute) // 快照早于重置时刻
		if !a.NeedsUsageProbe(maxAge) {
			t.Fatal("期望 5h 重置到点立即触发探测")
		}
	})

	t.Run("5h 重置后已刷新→不再重复探测", func(t *testing.T) {
		a := base()
		a.UsagePercent5h = 0
		a.Reset5hAt = now.Add(-time.Minute)
		a.UsageUpdatedAt5h = now // 已在重置之后刷新
		if a.NeedsUsageProbe(maxAge) {
			t.Fatal("重置后快照已刷新，不应再触发")
		}
	})

	t.Run("5h 未到重置且快照新鲜→不触发", func(t *testing.T) {
		a := base()
		a.UsagePercent5h = 100
		a.Reset5hAt = now.Add(time.Hour)
		a.UsageUpdatedAt5h = now
		if a.NeedsUsageProbe(maxAge) {
			t.Fatal("5h 未到重置且快照新鲜，不应触发")
		}
	})

	t.Run("7d 重置到点且快照过期→探测", func(t *testing.T) {
		a := base()
		a.Reset7dAt = now.Add(-time.Minute)
		a.UsageUpdatedAt = now.Add(-2 * time.Minute) // 早于 7d 重置，但仍 < maxAge
		if !a.NeedsUsageProbe(maxAge) {
			t.Fatal("期望 7d 重置到点立即触发探测")
		}
	})

	t.Run("状态为 error 时不触发", func(t *testing.T) {
		a := base()
		a.Status = StatusError
		a.UsagePercent5h = 100
		a.Reset5hAt = now.Add(-time.Minute)
		a.UsageUpdatedAt5h = now.Add(-6 * time.Minute)
		if a.NeedsUsageProbe(maxAge) {
			t.Fatal("error 账号不应触发探测")
		}
	})
}
