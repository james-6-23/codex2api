package auth

import (
	"context"
	"strings"
)

// DropAccountModel 把某个模型从账号的显式模型白名单中移除并持久化，用于上游明确
// 表示该账号套餐不支持该模型（如 credits_required）的场景。白名单为空表示
// "放行全部 claude-*"，此时无法用移除表达排除，返回 false 交由冷却兜底。
func (s *Store) DropAccountModel(ctx context.Context, acc *Account, model string) (bool, error) {
	if s == nil || acc == nil {
		return false, nil
	}
	target := strings.ToLower(strings.TrimSpace(model))
	if target == "" {
		return false, nil
	}
	acc.mu.RLock()
	current := append([]string(nil), acc.Models...)
	dbID := acc.DBID
	acc.mu.RUnlock()
	if len(current) == 0 {
		return false, nil
	}
	remaining := make([]string, 0, len(current))
	removed := false
	for _, m := range current {
		if strings.ToLower(strings.TrimSpace(m)) == target {
			removed = true
			continue
		}
		remaining = append(remaining, m)
	}
	if !removed {
		return false, nil
	}
	if s.db != nil && dbID > 0 {
		if err := s.db.UpdateCredentials(ctx, dbID, map[string]interface{}{"models": remaining}); err != nil {
			return false, err
		}
	}
	acc.mu.Lock()
	acc.Models = remaining
	acc.mu.Unlock()
	s.fastSchedulerUpdate(acc)
	return true, nil
}
