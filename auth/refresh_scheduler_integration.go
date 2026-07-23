package auth

import (
	"context"
	"log"
	"os"
	"strconv"
	"sync/atomic"
	"time"
)

// RefreshSchedulerIntegration 为 Store 提供刷新调度器集成功能
type RefreshSchedulerIntegration struct {
	store     *Store
	scheduler *RefreshScheduler
	enabled   atomic.Bool
}

// EnableRefreshScheduler 启用智能刷新调度器
func (s *Store) EnableRefreshScheduler(config RefreshConfig) {
	if s == nil {
		return
	}

	if config.MaxConcurrency <= 0 {
		config.MaxConcurrency = DefaultRefreshConfig().MaxConcurrency
	}
	if config.MinInterval <= 0 {
		config.MinInterval = DefaultRefreshConfig().MinInterval
	}
	if config.PreExpireWindow <= 0 {
		config.PreExpireWindow = DefaultRefreshConfig().PreExpireWindow
	}
	if config.RetryBackoffBase <= 0 {
		config.RetryBackoffBase = DefaultRefreshConfig().RetryBackoffBase
	}
	if config.RetryMaxAttempts <= 0 {
		config.RetryMaxAttempts = DefaultRefreshConfig().RetryMaxAttempts
	}
	if config.BatchSize <= 0 {
		config.BatchSize = DefaultRefreshConfig().BatchSize
	}
	if config.JitterPercent < 0 {
		config.JitterPercent = DefaultRefreshConfig().JitterPercent
	}

	integration := &RefreshSchedulerIntegration{
		store:     s,
		scheduler: NewRefreshScheduler(s, config),
	}
	integration.enabled.Store(true)

	// 启动调度器（生命周期由 scheduler.Stop() 管理，不能在此 defer cancel）
	integration.scheduler.Start(context.Background())

	// 存储到 store
	s.refreshScheduler.Store(integration)

	log.Printf("[Store] 智能刷新调度器已启用（并发: %d, 预过期窗口: %v）",
		config.MaxConcurrency, config.PreExpireWindow)
}

// DisableRefreshScheduler 禁用智能刷新调度器
func (s *Store) DisableRefreshScheduler() {
	if s == nil {
		return
	}

	if integration := s.refreshScheduler.Load(); integration != nil {
		integration.enabled.Store(false)
		integration.scheduler.Stop()
		s.refreshScheduler.Store(nil)
		log.Println("[Store] 智能刷新调度器已禁用")
	}
}

// RefreshSchedulerEnabled 检查智能刷新调度器是否启用
func (s *Store) RefreshSchedulerEnabled() bool {
	if s == nil {
		return false
	}
	if integration := s.refreshScheduler.Load(); integration != nil {
		return integration.enabled.Load()
	}
	return false
}

// GetRefreshScheduler 获取刷新调度器实例
func (s *Store) GetRefreshScheduler() *RefreshScheduler {
	if s == nil {
		return nil
	}
	if integration := s.refreshScheduler.Load(); integration != nil {
		return integration.scheduler
	}
	return nil
}

// ScheduleAccountRefresh 调度账号刷新
func (s *Store) ScheduleAccountRefresh(acc *Account) {
	if s == nil || acc == nil {
		return
	}

	// 优先使用智能调度器
	if scheduler := s.GetRefreshScheduler(); scheduler != nil {
		scheduler.Schedule(acc)
		return
	}

	// 回退到传统方式：立即刷新
	s.startDBBackgroundTask(func(parent context.Context) {
		ctx, cancel := context.WithTimeout(parent, 30*time.Second)
		defer cancel()
		if err := s.refreshAccount(ctx, acc); err != nil {
			log.Printf("[账号 %d] 异步刷新失败: %v", acc.DBID, err)
		}
	})
}

// ScheduleImmediateRefresh 立即调度账号刷新
func (s *Store) ScheduleImmediateRefresh(acc *Account) {
	if s == nil || acc == nil {
		return
	}

	if scheduler := s.GetRefreshScheduler(); scheduler != nil {
		scheduler.ScheduleImmediate(acc)
		return
	}

	// 回退到传统方式
	s.ScheduleAccountRefresh(acc)
}

// GetRefreshMetrics 获取刷新指标
func (s *Store) GetRefreshMetrics() RefreshMetricsSnapshot {
	if scheduler := s.GetRefreshScheduler(); scheduler != nil {
		return scheduler.GetMetrics()
	}
	return RefreshMetricsSnapshot{}
}

// GetRefreshTaskStatus 获取刷新任务状态
func (s *Store) GetRefreshTaskStatus(dbID int64) (RefreshState, int, error) {
	if scheduler := s.GetRefreshScheduler(); scheduler != nil {
		return scheduler.GetTaskStatus(dbID)
	}
	return RefreshStateSuccess, 0, nil
}

// CancelRefreshTask 取消刷新任务
func (s *Store) CancelRefreshTask(dbID int64) bool {
	if scheduler := s.GetRefreshScheduler(); scheduler != nil {
		return scheduler.CancelTask(dbID)
	}
	return false
}

// InitRefreshSchedulerFromEnv 从环境变量初始化刷新调度器
func (s *Store) InitRefreshSchedulerFromEnv() {
	if s == nil {
		return
	}

	// 检查是否启用
	if !refreshSchedulerEnabledFromEnv() {
		return
	}

	config := DefaultRefreshConfig()

	// 从环境变量读取配置
	if v := getEnvInt("REFRESH_MAX_CONCURRENCY", 0); v > 0 {
		config.MaxConcurrency = v
	}
	if v := getEnvDuration("REFRESH_PRE_EXPIRE_WINDOW", 0); v > 0 {
		config.PreExpireWindow = v
	}
	if v := getEnvDuration("REFRESH_MIN_INTERVAL", 0); v > 0 {
		config.MinInterval = v
	}
	if v := getEnvDuration("REFRESH_RETRY_BACKOFF_BASE", 0); v > 0 {
		config.RetryBackoffBase = v
	}
	if v := getEnvInt("REFRESH_RETRY_MAX_ATTEMPTS", 0); v > 0 {
		config.RetryMaxAttempts = v
	}
	if v := getEnvFloat("REFRESH_JITTER_PERCENT", -1); v >= 0 {
		config.JitterPercent = v
	}
	if v := getEnvInt("REFRESH_BATCH_SIZE", 0); v > 0 {
		config.BatchSize = v
	}
	config.EnableBatchOptimize = truthyEnv(refreshGetEnv("REFRESH_BATCH_OPTIMIZE", "true"))

	s.EnableRefreshScheduler(config)
}

// refreshSchedulerEnabledFromEnv 检查环境变量是否启用刷新调度器
func refreshSchedulerEnabledFromEnv() bool {
	for _, key := range []string{"REFRESH_SCHEDULER_ENABLED", "SMART_REFRESH_ENABLED"} {
		if truthyEnv(refreshGetEnv(key, "")) {
			return true
		}
	}
	return false
}

// refreshGetEnv 获取环境变量
func refreshGetEnv(key, defaultValue string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultValue
}

// getEnvInt 获取整数环境变量
func getEnvInt(key string, defaultValue int) int {
	v := os.Getenv(key)
	if v == "" {
		return defaultValue
	}
	i, err := strconv.Atoi(v)
	if err != nil {
		return defaultValue
	}
	return i
}

// getEnvDuration 获取持续时间环境变量
func getEnvDuration(key string, defaultValue time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return defaultValue
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return defaultValue
	}
	return d
}

// getEnvFloat 获取浮点数环境变量
func getEnvFloat(key string, defaultValue float64) float64 {
	v := os.Getenv(key)
	if v == "" {
		return defaultValue
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return defaultValue
	}
	return f
}
