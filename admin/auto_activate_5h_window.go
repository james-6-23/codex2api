package admin

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
)

const (
	autoActivate5hScanInterval   = time.Minute
	autoActivate5hAccountTimeout = 45 * time.Second
	autoActivate5hConcurrency    = 4
)

type autoActivate5hScanStats struct {
	Enabled    bool
	Scanned    int
	Candidates int
	Activated  int
	Failed     int
}

// StartAutoActivate5hWindow 启动 5h 窗口到点后的最小真实请求扫描。设置默认关闭；
// 开启后 UpdateSettings 会唤醒本循环立即扫描，之后每分钟检查一次。
func (h *Handler) StartAutoActivate5hWindow(ctx context.Context) {
	if h == nil || h.store == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	h.autoActivate5hStartOnce.Do(func() {
		if h.autoActivate5hWake == nil {
			h.autoActivate5hWake = make(chan struct{}, 1)
		}
		h.autoActivate5hWG.Add(1)
		go func() {
			defer h.autoActivate5hWG.Done()
			ticker := time.NewTicker(autoActivate5hScanInterval)
			defer ticker.Stop()
			firstScan := true
			for {
				if firstScan {
					firstScan = false
				} else {
					select {
					case <-ctx.Done():
						return
					case <-h.autoActivate5hWake:
					case <-ticker.C:
					}
				}
			drainSignals:
				for {
					select {
					case <-h.autoActivate5hWake:
					case <-ticker.C:
					default:
						break drainSignals
					}
				}
				if ctx.Err() != nil {
					return
				}

				stats := h.runAutoActivate5hScan(ctx, time.Time{})
				if stats.Enabled {
					log.Printf("[auto-activate-5h] 扫描完成: scanned=%d candidates=%d activated=%d failed=%d",
						stats.Scanned, stats.Candidates, stats.Activated, stats.Failed)
				}
			}
		}()
	})
}

// WaitAutoActivate5hWindow 等待后台扫描退出；调用前应先取消传给 Start 的 context。
func (h *Handler) WaitAutoActivate5hWindow() {
	if h != nil {
		h.autoActivate5hWG.Wait()
	}
}

func (h *Handler) triggerAutoActivate5hScan() {
	if h == nil || h.autoActivate5hWake == nil {
		return
	}
	select {
	case h.autoActivate5hWake <- struct{}{}:
	default:
	}
}

func (h *Handler) runAutoActivate5hScan(ctx context.Context, now time.Time) autoActivate5hScanStats {
	enabled, settingsErr := h.loadAutoActivate5hEnabled(ctx)
	stats := autoActivate5hScanStats{Enabled: enabled}
	if settingsErr != nil {
		stats.Failed = 1
		log.Printf("[auto-activate-5h] 读取系统设置失败，已跳过本轮扫描: %v", settingsErr)
		return stats
	}
	if !enabled || h == nil || h.store == nil {
		return stats
	}

	decisionNow := autoActivate5hDecisionTime(now)
	accounts := h.store.Accounts()
	sem := make(chan struct{}, autoActivate5hConcurrency)
	var wg sync.WaitGroup
	var statsMu sync.Mutex

	for _, account := range accounts {
		if account == nil {
			continue
		}
		stats.Scanned++
		if !account.ShouldActivate5hWindow(decisionNow) {
			continue
		}

		select {
		case <-ctx.Done():
			wg.Wait()
			return stats
		case sem <- struct{}{}:
		}
		wg.Add(1)
		go func(account *auth.Account) {
			defer wg.Done()
			defer func() { <-sem }()

			accountCtx, cancel := context.WithTimeout(ctx, autoActivate5hAccountTimeout)
			defer cancel()
			candidate, activated, err := h.autoActivate5hForAccount(accountCtx, account, decisionNow)

			statsMu.Lock()
			if candidate {
				stats.Candidates++
			}
			if activated {
				stats.Activated++
			}
			if err != nil {
				stats.Failed++
			}
			statsMu.Unlock()
			if err != nil {
				log.Printf("[auto-activate-5h] 账号 %d 开窗失败: %v", account.DBID, err)
			}
		}(account)
	}
	wg.Wait()
	return stats
}

func (h *Handler) autoActivate5hForAccount(ctx context.Context, account *auth.Account, now time.Time) (candidate, activated bool, err error) {
	if account == nil {
		return false, false, nil
	}
	enabled, settingsErr := h.loadAutoActivate5hEnabled(ctx)
	if settingsErr != nil {
		return false, false, settingsErr
	}
	if !enabled || !account.ShouldActivate5hWindow(now) {
		return false, false, nil
	}
	candidate = true
	_, resetAt, ok := account.GetUsageSnapshot5h()
	if !ok || resetAt.IsZero() {
		return true, false, nil
	}
	if !account.TryBeginUsageProbe() {
		return true, false, nil
	}
	defer account.FinishUsageProbe()

	if err := h.activate5hWindowRequest(ctx, account); err != nil {
		return true, false, err
	}
	account.Mark5hWindowActivated(resetAt)
	h.store.Persist5hWindowActivated(account)
	log.Printf("[auto-activate-5h] 账号 %d 已发送最小 /responses 以激活下一轮 5h 窗口: reset_at=%s",
		account.DBID, resetAt.UTC().Format(time.RFC3339))
	return true, true, nil
}

func (h *Handler) activate5hWindowRequest(ctx context.Context, account *auth.Account) error {
	if h != nil && h.activate5hWindow != nil {
		return h.activate5hWindow(ctx, account)
	}
	return h.probeUsageViaResponses(ctx, account)
}

func (h *Handler) loadAutoActivate5hEnabled(ctx context.Context) (bool, error) {
	enabled := proxy.CurrentRuntimeSettings().AutoActivate5hWindowEnabled
	if h == nil || h.db == nil {
		return enabled, nil
	}
	settings, err := h.db.GetSystemSettings(ctx)
	if err != nil {
		return false, err
	}
	if settings == nil {
		return false, nil
	}
	return settings.AutoActivate5hWindowEnabled, nil
}

func autoActivate5hDecisionTime(reference time.Time) time.Time {
	if reference.IsZero() {
		return time.Now()
	}
	return reference
}
