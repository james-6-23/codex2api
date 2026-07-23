package auth

import (
	"testing"
	"time"
)

func TestRefreshSchedulerIntegrationEnableDisable(t *testing.T) {
	store := &Store{}

	// 初始状态
	if store.RefreshSchedulerEnabled() {
		t.Fatal("RefreshSchedulerEnabled should be false by default")
	}

	// 启用
	config := DefaultRefreshConfig()
	config.MaxConcurrency = 5
	store.EnableRefreshScheduler(config)

	if !store.RefreshSchedulerEnabled() {
		t.Fatal("RefreshSchedulerEnabled should be true after enabling")
	}

	scheduler := store.GetRefreshScheduler()
	if scheduler == nil {
		t.Fatal("GetRefreshScheduler should return non-nil after enabling")
	}

	// 禁用
	store.DisableRefreshScheduler()

	if store.RefreshSchedulerEnabled() {
		t.Fatal("RefreshSchedulerEnabled should be false after disabling")
	}
}

func TestRefreshSchedulerIntegrationSchedule(t *testing.T) {
	store := &Store{}
	config := DefaultRefreshConfig()
	store.EnableRefreshScheduler(config)
	defer store.DisableRefreshScheduler()

	acc := newTestAccountWithExpiry(1, 10*time.Minute)

	// 调度刷新
	store.ScheduleAccountRefresh(acc)

	// 检查任务状态（调度器内部会检查，此处仅验证不 panic）
	state, attempts, _ := store.GetRefreshTaskStatus(1)
	// 由于是新创建的任务，状态应该是 Pending 或 Success（如果被立即处理）
	t.Logf("Task state: %d, attempts: %d", state, attempts)
}

func TestRefreshSchedulerIntegrationMetrics(t *testing.T) {
	store := &Store{}
	config := DefaultRefreshConfig()
	store.EnableRefreshScheduler(config)
	defer store.DisableRefreshScheduler()

	// 初始指标
	metrics := store.GetRefreshMetrics()
	if metrics.TotalTasks != 0 {
		t.Fatalf("initial TotalTasks = %d, want 0", metrics.TotalTasks)
	}

	// 添加任务
	acc := newTestAccountWithExpiry(1, 10*time.Minute)
	store.ScheduleAccountRefresh(acc)

	// 获取指标
	metrics = store.GetRefreshMetrics()
	if metrics.TotalTasks != 1 {
		t.Fatalf("after schedule TotalTasks = %d, want 1", metrics.TotalTasks)
	}
}

func TestRefreshSchedulerIntegrationCancel(t *testing.T) {
	store := &Store{}
	config := DefaultRefreshConfig()
	store.EnableRefreshScheduler(config)
	defer store.DisableRefreshScheduler()

	acc := newTestAccountWithExpiry(1, 10*time.Minute)
	store.ScheduleAccountRefresh(acc)

	// 取消任务
	cancelled := store.CancelRefreshTask(1)
	if !cancelled {
		t.Fatal("CancelRefreshTask should return true")
	}

	// 再次取消应该失败
	cancelled = store.CancelRefreshTask(1)
	if cancelled {
		t.Fatal("CancelRefreshTask should return false for already cancelled task")
	}
}

func TestStoreStopDisablesRefreshScheduler(t *testing.T) {
	store := &Store{stopCh: make(chan struct{})}
	store.EnableRefreshScheduler(DefaultRefreshConfig())
	if store.GetRefreshScheduler() == nil {
		t.Fatal("GetRefreshScheduler() = nil after EnableRefreshScheduler")
	}

	store.Stop()
	if store.GetRefreshScheduler() != nil {
		t.Fatal("GetRefreshScheduler() != nil after Store.Stop")
	}
}
