package admin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	ic "github.com/codex2api/internal/intelligencecheck"
	"github.com/codex2api/proxy"
)

func (h *Handler) StartIntelligenceChecks(ctx context.Context, deviceConfig ...*proxy.DeviceProfileConfig) {
	if len(deviceConfig) > 0 {
		h.intelligenceDeviceConfig = deviceConfig[0]
	}
	if h.db == nil || h.store == nil {
		return
	}
	h.intelligenceOnce.Do(func() {
		h.intelligenceWG.Add(1)
		go func() {
			defer h.intelligenceWG.Done()
			ticker := time.NewTicker(time.Second)
			defer ticker.Stop()
			for {
				if ctx.Err() != nil {
					return
				}
				if err := h.intelligenceTick(ctx); err != nil && ctx.Err() == nil {
					log.Printf("降智检测调度失败: %v", err)
				}
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	})
}
func (h *Handler) WaitIntelligenceChecks() { h.intelligenceWG.Wait() }
func (h *Handler) intelligenceTick(ctx context.Context) error {
	opCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := h.db.ExpireIntelligenceCheckLeases(opCtx, time.Now()); err != nil {
		return err
	}
	hold := func(id int64, model string) (time.Time, bool) {
		return h.intelligenceQuotaHold(h.store.FindByID(id), model)
	}
	if _, err := h.db.EnqueueDueIntelligenceChecks(opCtx, time.Now(), 100, hold); err != nil {
		return err
	}
	for i := 0; i < 100 && ctx.Err() == nil; i++ {
		var token [16]byte
		if _, err := rand.Read(token[:]); err != nil {
			return err
		}
		owner := hex.EncodeToString(token[:])
		run, err := h.db.ClaimIntelligenceCheck(opCtx, owner, time.Now(), 30*time.Second)
		if err != nil {
			return err
		}
		if run == nil {
			return nil
		}
		account := h.store.FindByID(run.AccountID)
		if account != nil && !h.store.TryAcquireIntelligenceCheck(account) {
			if err := h.db.RequeueIntelligenceCheck(opCtx, run.ID, owner, time.Now()); err != nil {
				return err
			}
			continue
		}
		h.intelligenceWG.Add(1)
		go func(run database.IntelligenceCheckRun) {
			defer h.intelligenceWG.Done()
			if account != nil {
				defer h.store.Release(account)
			}
			h.runIntelligenceCheck(ctx, run, account)
		}(*run)
	}
	return nil
}

// intelligenceQuotaHold reports whether a detector request to account would
// only be rejected for rate-limit or quota reasons, and when that should end.
func (h *Handler) intelligenceQuotaHold(account *auth.Account, model string) (time.Time, bool) {
	return account.IntelligenceCheckQuotaHold(model, proxy.DispatchPolicyForModel(model), time.Now())
}

func (h *Handler) runIntelligenceCheck(parent context.Context, run database.IntelligenceCheckRun, account *auth.Account) {
	ctx, cancel := context.WithTimeout(parent, time.Duration(run.Settings.TimeoutSeconds)*time.Second)
	result := intelligenceExecution{}
	if h.db != nil {
		if err := h.db.ValidateIntelligenceCheckAccountSettings(ctx, run.Settings, run.Channel, run.AccountID); err != nil {
			result.Error = "配置错误：" + err.Error()
			result.Interrupted = true
		}
	}
	// A rate-limited or exhausted account says nothing about model quality, so
	// neither outcome below is graded. An automatic check that was queued before
	// the limit was known is not sent; an explicit manual check still is.
	if result.Error == "" && run.Source == "automatic" {
		if _, held := h.intelligenceQuotaHold(account, run.Model); held {
			result.Error = "账号限流或额度耗尽，已跳过本次自动检测"
			result.Interrupted = true
		}
	}
	if result.Error == "" {
		result = h.executeIntelligenceCheck(ctx, account, run.Model, run.ReasoningEffort, run.Question.Prompt)
		if result.Error != "" && !result.Interrupted && !result.CredentialRejected {
			if _, held := h.intelligenceQuotaHold(account, run.Model); held {
				result.Error = "账号限流或额度耗尽，本次检测不计入结果：" + result.Error
				result.Interrupted = true
			}
		}
	}
	cancel()
	observation := ic.Observation{CredentialRejected: result.CredentialRejected, Interrupted: result.Interrupted, Output: result.Output, Error: result.Error, Complete: result.Complete, DurationMS: result.DurationMS, InputTokens: int64(result.InputTokens), OutputTokens: int64(result.OutputTokens), ReasoningTokens: int64(result.ReasoningTokens), ResponseModel: result.ResponseModel, EffectiveContext: result.EffectiveContext}
	if parent.Err() != nil {
		observation.Interrupted = true
		observation.Error = "服务关闭，检测已中断"
		observation.Complete = false
	}
	finishCtx, finishCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer finishCancel()
	committed, err := h.db.FinishIntelligenceCheck(finishCtx, run.ID, run.LeaseOwner, observation, time.Now())
	if err != nil {
		log.Printf("降智检测 %d 保存失败: %v", run.ID, err)
		return
	}
	if committed {
		if err = h.store.RefreshIntelligenceAccount(finishCtx, run.AccountID); err != nil {
			log.Printf("降智检测 %d 调度同步失败，等待 outbox: %v", run.ID, err)
		}
		h.invalidateAccountSnapshotCaches()
	}
}
