package admin

import (
	"context"
	"log"
	"time"
)

// grokProbeRunGuard 单轮探测的整体超时兜底,避免异常账号把整轮卡死。
const grokProbeRunGuard = 15 * time.Minute

// StartGrokStatusProbe 启动 Grok 账号状态定期探测的常驻后台任务。
//
// 背景:账号的限流/冷却状态虽已持久化并在重启时恢复,但上游是滚动窗口——账号可能在网关
// 无流量期间耗尽或恢复,状态就会与真实情况脱节。该任务按 grok 系统设置里的间隔,对所有
// 未停用的 Grok 账号复跑一次连通性测试(复用批量测试的写状态 testFn):200→清冷却转可用,
// 429→按 Grok 语义落权威用量快照并标 usage_limited。开关默认关,由设置页控制。
//
// 采用 1 分钟粗粒度轮询 + lastRun 判定间隔,使设置变更(开/关/改间隔)在一分钟内生效,
// 无需重启或额外的唤醒信号通道。
func (h *Handler) StartGrokStatusProbe(ctx context.Context) {
	if h == nil || h.store == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		var lastRun time.Time
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			if !h.store.GrokProbeEnabled() {
				continue
			}
			interval := time.Duration(h.store.GrokProbeIntervalMinutes()) * time.Minute
			if !lastRun.IsZero() && time.Since(lastRun) < interval {
				continue
			}
			lastRun = time.Now()
			h.runGrokStatusProbe(ctx)
		}
	}()
}

// runGrokStatusProbe 对所有未停用的 Grok 账号跑一轮写状态的连通性测试。
func (h *Handler) runGrokStatusProbe(ctx context.Context) {
	accounts := h.store.EnabledGrokAccounts()
	if len(accounts) == 0 {
		return
	}
	probeCtx, cancel := context.WithTimeout(ctx, grokProbeRunGuard)
	defer cancel()
	start := time.Now()
	counts := h.runBatchTest(probeCtx, accounts, 0, h.runSingleBatchTest, nil)
	log.Printf("[grok-probe] 定期探测完成: total=%d success=%d rate_limited=%d banned=%d failed=%d 耗时=%s",
		counts.Total, counts.Success, counts.RateLimited, counts.Banned, counts.Failed, time.Since(start).Round(time.Millisecond))
}
