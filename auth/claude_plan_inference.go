package auth

// Claude 套餐推断 hook。
//
// Setup Token 只有 user:inference,拿不到 profile,套餐档位默认是占位的 "claude";
// OAuth 账号的 profile 偶尔也会缺 organization_type。上游对「套餐不含的模型」返回
// 429 credits_required,而 Fable 这类高档模型只有 Max 及以上才包含。于是把两类观测
// 当作套餐信号:
//   - 任何模型命中 credits_required → 套餐降为 pro(套餐已由 profile 判定为 pro/free/
//     team/enterprise/business 时不再改动,只覆盖占位与 max 系列);
//   - Fable 等高档模型成功响应 → 占位或 pro 升为 max。
//
// 推断结果直接写进 credentials.plan_type;OAuth 账号下一次刷新若 profile 给出更准确
// 的档位仍会覆盖(profile 是权威来源,这里只是补空)。

import (
	"context"
	"strings"
)

const (
	// ClaudePlanUnknown 是导入时未拿到 profile 的占位套餐。
	ClaudePlanUnknown = "claude"
	// ClaudePlanPro / ClaudePlanMax 是推断 hook 写入的两个档位。
	ClaudePlanPro = "pro"
	ClaudePlanMax = "max"
)

// IsClaudeCreditsGatedModel 判断模型是否属于「仅 Max 及以上套餐包含」的高档模型:
// 成功调用它即可视为账号至少是 Max。当前只有 Fable 系列满足该定义。
func IsClaudeCreditsGatedModel(model string) bool {
	return strings.Contains(strings.ToLower(strings.TrimSpace(model)), "fable")
}

// claudePlanPinnedByProvider 判断套餐是否已是 profile 判定过的低档位:这类值不该被
// credits_required 再改写(pro 已是 pro;free/team/enterprise 各有各的模型集)。
func claudePlanPinnedByProvider(plan string) bool {
	switch strings.ToLower(strings.TrimSpace(plan)) {
	case ClaudePlanPro, "free", "team", "enterprise", "business",
		"claude-pro", "claude-free", "claude-team", "claude-enterprise", "claude-business":
		return true
	}
	return false
}

// ApplyClaudePlanFromCreditsRequired 在账号命中 credits_required 时把套餐降为 pro。
// 返回 (原套餐, 新套餐, 是否改动)。非 Claude 账号或套餐已被 provider 钉住时不改动。
func (s *Store) ApplyClaudePlanFromCreditsRequired(ctx context.Context, acc *Account) (previous, current string, changed bool) {
	if s == nil || acc == nil || !acc.IsClaudeOAuth() || acc.IsClaudeAPIKey() {
		return "", "", false
	}
	previous = acc.GetPlanType()
	if claudePlanPinnedByProvider(previous) {
		return previous, previous, false
	}
	if err := s.setClaudePlanType(ctx, acc, ClaudePlanPro); err != nil {
		return previous, previous, false
	}
	return previous, ClaudePlanPro, true
}

// ApplyClaudePlanFromGatedModelSuccess 在高档模型成功响应后把占位/pro 套餐升为 max。
// 已是 max 系列(max/max-5x/max-20x)或 team/enterprise 等不改动。
func (s *Store) ApplyClaudePlanFromGatedModelSuccess(ctx context.Context, acc *Account, model string) (previous, current string, changed bool) {
	if s == nil || acc == nil || !acc.IsClaudeOAuth() || acc.IsClaudeAPIKey() || !IsClaudeCreditsGatedModel(model) {
		return "", "", false
	}
	previous = acc.GetPlanType()
	switch strings.ToLower(strings.TrimSpace(previous)) {
	case "", ClaudePlanUnknown, ClaudePlanPro, "claude-pro":
	default:
		return previous, previous, false
	}
	if err := s.setClaudePlanType(ctx, acc, ClaudePlanMax); err != nil {
		return previous, previous, false
	}
	return previous, ClaudePlanMax, true
}

// setClaudePlanType 落库并同步内存态与调度器。
func (s *Store) setClaudePlanType(ctx context.Context, acc *Account, plan string) error {
	acc.mu.RLock()
	dbID := acc.DBID
	acc.mu.RUnlock()
	if s.db != nil && dbID > 0 {
		if ctx == nil {
			ctx = context.Background()
		}
		if err := s.db.UpdateCredentials(ctx, dbID, map[string]interface{}{"plan_type": plan}); err != nil {
			return err
		}
	}
	acc.mu.Lock()
	acc.PlanType = plan
	acc.mu.Unlock()
	s.fastSchedulerUpdate(acc)
	return nil
}
