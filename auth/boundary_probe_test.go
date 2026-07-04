package auth

import (
	"testing"
	"time"
)

// TestNextProbeBoundaryPicksEarliest 验证 nextProbeBoundary 在多个候选边界中取最近的未来时刻，
// 并正确忽略「已过期」「快照已刷新到重置后」「unauthorized 冷却」等不该探的情形。
func TestNextProbeBoundaryPicksEarliest(t *testing.T) {
	now := time.Now()

	t.Run("取最近未来边界(5h 冷却先于 7d 重置)", func(t *testing.T) {
		a := &Account{
			AccessToken:         "tok",
			Status:              StatusCooldown,
			CooldownReason:      "rate_limited",
			CooldownUtil:        now.Add(30 * time.Second),
			UsagePercent7dValid: true,
			Reset7dAt:           now.Add(6 * 24 * time.Hour),
			UsageUpdatedAt:      now.Add(-time.Hour),
		}
		got, ok := a.nextProbeBoundary(now)
		if !ok || !got.Equal(a.CooldownUtil) {
			t.Fatalf("got %v,%v，want %v,true（应取更近的冷却结束时刻）", got, ok, a.CooldownUtil)
		}
	})

	t.Run("5h 窗口重置", func(t *testing.T) {
		a := &Account{
			AccessToken:         "tok",
			UsagePercent5hValid: true,
			Reset5hAt:           now.Add(90 * time.Second),
			UsageUpdatedAt5h:    now.Add(-time.Hour), // 快照采集于重置前
		}
		got, ok := a.nextProbeBoundary(now)
		if !ok || !got.Equal(a.Reset5hAt) {
			t.Fatalf("got %v,%v，want %v,true", got, ok, a.Reset5hAt)
		}
	})

	t.Run("快照已刷新到重置之后 → 不再需要该边界", func(t *testing.T) {
		a := &Account{
			AccessToken:         "tok",
			UsagePercent5hValid: true,
			Reset5hAt:           now.Add(90 * time.Second),
			UsageUpdatedAt5h:    now.Add(2 * time.Minute), // 快照已晚于重置
		}
		if got, ok := a.nextProbeBoundary(now); ok {
			t.Fatalf("got %v,%v，want zero,false（快照已覆盖该窗口，无需再探）", got, ok)
		}
	})

	t.Run("unauthorized 冷却不武装(探针必 401)", func(t *testing.T) {
		a := &Account{
			AccessToken:    "tok",
			Status:         StatusCooldown,
			CooldownReason: "unauthorized",
			CooldownUtil:   now.Add(time.Minute),
		}
		if got, ok := a.nextProbeBoundary(now); ok {
			t.Fatalf("got %v,%v，want zero,false", got, ok)
		}
	})

	t.Run("边界已过 → 忽略", func(t *testing.T) {
		a := &Account{
			AccessToken:         "tok",
			UsagePercent5hValid: true,
			Reset5hAt:           now.Add(-time.Minute),
			UsageUpdatedAt5h:    now.Add(-time.Hour),
		}
		if got, ok := a.nextProbeBoundary(now); ok {
			t.Fatalf("got %v,%v，want zero,false（已过期边界交给常规探针）", got, ok)
		}
	})

	t.Run("无凭据/错误态 → 不武装", func(t *testing.T) {
		a := &Account{
			Status:              StatusCooldown,
			CooldownReason:      "rate_limited",
			CooldownUtil:        now.Add(time.Minute),
			UsagePercent5hValid: true,
			Reset5hAt:           now.Add(time.Minute),
			UsageUpdatedAt5h:    now.Add(-time.Hour),
		}
		// AccessToken 为空
		if _, ok := a.nextProbeBoundary(now); ok {
			t.Fatal("无 AccessToken 应返回 false")
		}
		a.AccessToken = "tok"
		a.Status = StatusError
		if _, ok := a.nextProbeBoundary(now); ok {
			t.Fatal("StatusError 应返回 false")
		}
	})
}

// TestWakeBoundaryProbeOnlyNudgesForEarlier 验证 WakeBoundaryProbe 的「仅更早才打扰」节流：
// 更早的边界会触发唤醒，更晚或已过的边界不会，避免高频重排。
func TestWakeBoundaryProbeOnlyNudgesForEarlier(t *testing.T) {
	now := time.Now()
	s := &Store{boundaryProbeWakeCh: make(chan struct{}, 1)}

	drain := func() bool {
		select {
		case <-s.boundaryProbeWakeCh:
			return true
		default:
			return false
		}
	}

	// 未武装(armedBoundaryAt=0)：任何未来边界都应唤醒。
	s.WakeBoundaryProbe(now.Add(time.Minute))
	if !drain() {
		t.Fatal("未武装时未来边界应触发唤醒")
	}

	// 模拟已武装到 +30s。
	s.armedBoundaryAt = now.Add(30 * time.Second).UnixNano()

	// 更晚的边界(+2min) → 不打扰。
	s.WakeBoundaryProbe(now.Add(2 * time.Minute))
	if drain() {
		t.Fatal("更晚的边界不应触发唤醒")
	}

	// 更早的边界(+5s) → 打扰。
	s.WakeBoundaryProbe(now.Add(5 * time.Second))
	if !drain() {
		t.Fatal("更早的边界应触发唤醒")
	}

	// 已过期的边界 → 不打扰。
	s.WakeBoundaryProbe(now.Add(-time.Second))
	if drain() {
		t.Fatal("已过期边界不应触发唤醒")
	}

	// 零值 → 强制唤醒(未知时间，要求全量重排)。
	s.WakeBoundaryProbe(time.Time{})
	if !drain() {
		t.Fatal("零值应强制唤醒")
	}
}

// TestArmNextBoundaryProbeArmsTimer 验证 armNextBoundaryProbe 在有/无待处理边界时的定时器行为，
// 并同步更新 armedBoundaryAt。
func TestArmNextBoundaryProbeArmsTimer(t *testing.T) {
	now := time.Now()
	s := &Store{boundaryProbeWakeCh: make(chan struct{}, 1)}
	s.accounts = []*Account{
		{ // 最近边界：+40ms 的 5h 重置
			AccessToken:         "tok",
			UsagePercent5hValid: true,
			Reset5hAt:           now.Add(40 * time.Millisecond),
			UsageUpdatedAt5h:    now.Add(-time.Hour),
		},
		{ // 更远：+10min 冷却
			AccessToken:    "tok",
			Status:         StatusCooldown,
			CooldownReason: "rate_limited",
			CooldownUtil:   now.Add(10 * time.Minute),
		},
	}

	timer := time.NewTimer(time.Hour)
	if !timer.Stop() {
		<-timer.C
	}

	s.armNextBoundaryProbe(timer)

	if s.armedBoundaryAt != s.accounts[0].Reset5hAt.UnixNano() {
		t.Fatalf("armedBoundaryAt=%d，want %d（最近边界）", s.armedBoundaryAt, s.accounts[0].Reset5hAt.UnixNano())
	}

	// 定时器应在最近边界(+滞后 probeBoundaryLag)后触发，这里给足余量。
	select {
	case <-timer.C:
	case <-time.After(probeBoundaryLag + time.Second):
		t.Fatal("定时器应在最近边界到点后触发")
	}

	// 无任何待处理边界 → 停表并清零 armedBoundaryAt。
	s.accounts = []*Account{{AccessToken: "tok"}}
	timer2 := time.NewTimer(time.Hour)
	s.armNextBoundaryProbe(timer2)
	if s.armedBoundaryAt != 0 {
		t.Fatalf("armedBoundaryAt=%d，want 0（无待处理边界）", s.armedBoundaryAt)
	}
	select {
	case <-timer2.C:
		t.Fatal("无边界时定时器不应触发")
	case <-time.After(100 * time.Millisecond):
	}
}
