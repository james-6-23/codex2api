package auth

import (
	"testing"
	"time"
)

func TestSmartPacingRatio(t *testing.T) {
	now := time.Now()

	tests := []struct {
		name      string
		usage     float64
		valid     bool
		resetAt   time.Time
		window    time.Duration
		wantOK    bool
		wantThrot bool // 期望 ratio < 1（需要限速）
	}{
		{
			name:    "无效用量信号不介入",
			usage:   90,
			valid:   false,
			resetAt: now.Add(time.Hour),
			window:  smartPacingWindow5h,
			wantOK:  false,
		},
		{
			name:    "重置时间未知不介入",
			usage:   90,
			valid:   true,
			resetAt: time.Time{},
			window:  smartPacingWindow5h,
			wantOK:  false,
		},
		{
			name:    "窗口已翻新不介入",
			usage:   90,
			valid:   true,
			resetAt: now.Add(-time.Minute),
			window:  smartPacingWindow5h,
			wantOK:  false,
		},
		{
			name:    "已耗尽交给限流逻辑",
			usage:   100,
			valid:   true,
			resetAt: now.Add(time.Hour),
			window:  smartPacingWindow5h,
			wantOK:  false,
		},
		{
			name:      "燃烧过快需限速",
			usage:     95, // 剩 5%，但还剩 1h（应匀速到重置）
			valid:     true,
			resetAt:   now.Add(time.Hour),
			window:    smartPacingWindow5h,
			wantOK:    true,
			wantThrot: true,
		},
		{
			name:      "恰好匀速不限速",
			usage:     50, // 剩 50%，剩 2.5h（半窗），正好匀速
			valid:     true,
			resetAt:   now.Add(150 * time.Minute),
			window:    smartPacingWindow5h,
			wantOK:    true,
			wantThrot: false,
		},
		{
			name:      "落后于进度不限速",
			usage:     10,
			valid:     true,
			resetAt:   now.Add(4 * time.Hour),
			window:    smartPacingWindow5h,
			wantOK:    true,
			wantThrot: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ratio, ok := smartPacingRatio(tc.usage, tc.valid, tc.resetAt, tc.window, now)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (ratio=%.4f)", ok, tc.wantOK, ratio)
			}
			if !ok {
				return
			}
			if tc.wantThrot && ratio >= 1 {
				t.Fatalf("ratio = %.4f, 期望 < 1（需限速）", ratio)
			}
			if !tc.wantThrot && ratio < 1 {
				t.Fatalf("ratio = %.4f, 期望 >= 1（不限速）", ratio)
			}
		})
	}
}

func TestSmartPacingConcurrencyLimitLocked(t *testing.T) {
	now := time.Now()

	newOverBurnAccount := func() *Account {
		return &Account{
			smartPacingEnabled:        true,
			smartPacingMinConcurrency: 1,
			smartPacingWindows5h:      true,
			smartPacingWindows7d:      true,
			UsagePercent5h:            95, // 剩 5%
			UsagePercent5hValid:       true,
			Reset5hAt:                 now.Add(time.Hour), // 还剩 1h，ratio≈0.25
		}
	}

	t.Run("关闭时不改动", func(t *testing.T) {
		a := newOverBurnAccount()
		a.smartPacingEnabled = false
		if got := a.smartPacingConcurrencyLimitLocked(8, now); got != 8 {
			t.Fatalf("got %d, want 8 (关闭不应限速)", got)
		}
	})

	t.Run("燃烧过快按比例压并发", func(t *testing.T) {
		a := newOverBurnAccount()
		// ratio = 5*18000/(100*3600) = 0.25 → ceil(8*0.25)=2
		if got := a.smartPacingConcurrencyLimitLocked(8, now); got != 2 {
			t.Fatalf("got %d, want 2", got)
		}
	})

	t.Run("不低于并发下限", func(t *testing.T) {
		a := newOverBurnAccount()
		a.smartPacingMinConcurrency = 3
		if got := a.smartPacingConcurrencyLimitLocked(8, now); got != 3 {
			t.Fatalf("got %d, want 3 (下限保护)", got)
		}
	})

	t.Run("下限>=上限时不改动", func(t *testing.T) {
		a := newOverBurnAccount()
		a.smartPacingMinConcurrency = 8
		if got := a.smartPacingConcurrencyLimitLocked(8, now); got != 8 {
			t.Fatalf("got %d, want 8", got)
		}
	})

	t.Run("limit<=1 不改动", func(t *testing.T) {
		a := newOverBurnAccount()
		if got := a.smartPacingConcurrencyLimitLocked(1, now); got != 1 {
			t.Fatalf("got %d, want 1", got)
		}
	})

	t.Run("匀速时不改动", func(t *testing.T) {
		a := newOverBurnAccount()
		a.UsagePercent5h = 50
		a.Reset5hAt = now.Add(150 * time.Minute)
		if got := a.smartPacingConcurrencyLimitLocked(8, now); got != 8 {
			t.Fatalf("got %d, want 8 (匀速不限速)", got)
		}
	})

	t.Run("只配速被选中的窗口", func(t *testing.T) {
		a := newOverBurnAccount()
		a.smartPacingWindows5h = false // 只看 7d，而 7d 信号无效
		a.UsagePercent7dValid = false
		if got := a.smartPacingConcurrencyLimitLocked(8, now); got != 8 {
			t.Fatalf("got %d, want 8 (5h 未选中且 7d 无效)", got)
		}
	})

	t.Run("取更严格的窗口", func(t *testing.T) {
		a := newOverBurnAccount()
		// 5h 匀速（ratio=1），7d 燃烧过快（ratio≈0.25）→ 取 7d
		a.UsagePercent5h = 50
		a.Reset5hAt = now.Add(150 * time.Minute)
		a.UsagePercent7d = 95
		a.UsagePercent7dValid = true
		a.Reset7dAt = now.Add(100 * time.Hour) // 剩 5%，但 7d 窗口还剩 100h → 严重超前燃烧
		got := a.smartPacingConcurrencyLimitLocked(8, now)
		if got >= 8 || got < 1 {
			t.Fatalf("got %d, 期望被 7d 窗口压到 [1,8)", got)
		}
	})

	// team 月窗按真实 30 天周期配速，而非固定 7 天：同样的用量/剩余时间下，
	// 误当 7 天会过度限流，用真实月窗则判为匀速不限流。
	t.Run("team 月窗按真实周期配速", func(t *testing.T) {
		newTeamMonthly := func() *Account {
			return &Account{
				smartPacingEnabled:        true,
				smartPacingMinConcurrency: 1,
				smartPacingWindows5h:      false, // 隔离，只看长窗口
				smartPacingWindows7d:      true,
				UsagePercent7d:            50,
				UsagePercent7dValid:       true,
				Reset7dAt:                 now.Add(15 * 24 * time.Hour), // 30天窗口过半、用量过半 → 匀速
			}
		}

		// 已知月窗长度(30天=2592000s)：ratio=1.0 → 不限流。
		a := newTeamMonthly()
		a.Window7dSeconds = 2_592_000
		if got := a.smartPacingConcurrencyLimitLocked(8, now); got != 8 {
			t.Fatalf("月窗按真实周期应判匀速不限流，got %d want 8", got)
		}

		// 未知长度(=0)回退固定 7 天：同样数据会被误判超前燃烧而压并发，
		// 印证若不修分类，team 账号会被过度限流。
		b := newTeamMonthly()
		if got := b.smartPacingConcurrencyLimitLocked(8, now); got >= 8 {
			t.Fatalf("回退 7 天应过度限流(证明缺口)，got %d want <8", got)
		}
	})
}
