package auth

import (
	"fmt"
	"log"
	"strings"
	"sync/atomic"
	"time"
)

const workspaceLinkedErrorDedupTTL = 60 * time.Second

type workspaceLinkedMark struct {
	until     time.Time
	triggerID int64
}

// MarkDeactivatedWorkspace 把触发账号标为 error，并把同一工作区
// （原生 AccountID / chatgpt_account_id）下的其余 Codex 账号一并隔离。
// ChatGPT Team / K12 被停用时，同空间座位会一起失效；只标触发号会让调度继续打兄弟号。
//
// 分组键是请求实际打到的工作区（EffectiveAccountID，含账号级 Chatgpt-Account-Id 覆盖）。
// 兄弟号优先按原生 AccountID 匹配；Team/K12/edu 座位再按生效工作区匹配
// （每人可能有独立 AccountID，但流量打到同一空间）。
// 仅请求头覆盖到该空间的个人号不会整号打死。
// Grok / OpenAI Responses API 账号不参与联动。默认生效，无配置开关。
func (s *Store) MarkDeactivatedWorkspace(trigger *Account, errorMsg string) {
	if s == nil || trigger == nil {
		return
	}
	targets := s.workspaceLinkedTargets(trigger)
	for _, acc := range targets {
		atomic.StoreInt32(&acc.Disabled, 1)
	}
	s.MarkError(trigger, errorMsg)
	if len(targets) == 0 {
		return
	}
	linkedMsg := deactivatedWorkspaceLinkedMessage(trigger.DBID)
	for _, acc := range targets {
		s.MarkError(acc, linkedMsg)
	}
	log.Printf("账号 %d 工作区已停用，联动标记 %d 个同空间账号", trigger.DBID, len(targets))
}

func deactivatedWorkspaceLinkedMessage(triggerID int64) string {
	return fmt.Sprintf("上游返回: deactivated_workspace（工作区联动，由账号 %d 触发）", triggerID)
}

func (s *Store) workspaceLinkedTargets(trigger *Account) []*Account {
	if trigger.IsGrokAPI() || trigger.IsOpenAIResponsesAPI() || trigger.IsClaudeOAuth() {
		return nil
	}
	workspaceID := strings.TrimSpace(trigger.EffectiveAccountID())
	if workspaceID == "" {
		return nil
	}
	if !s.markWorkspaceLinkedFired(workspaceID, trigger.DBID) {
		return nil
	}
	var targets []*Account
	for _, acc := range s.Accounts() {
		if !shouldLinkDeactivatedWorkspace(trigger, acc, workspaceID) {
			continue
		}
		targets = append(targets, acc)
	}
	return targets
}

func shouldLinkDeactivatedWorkspace(trigger, sibling *Account, workspaceID string) bool {
	if sibling == nil || trigger == nil || sibling.DBID == trigger.DBID {
		return false
	}
	if sibling.IsGrokAPI() || sibling.IsOpenAIResponsesAPI() || sibling.IsClaudeOAuth() {
		return false
	}
	if siblingErrorStatus(sibling) {
		return false
	}
	if nativeWorkspaceID(sibling) == workspaceID {
		return true
	}
	// Team/K12 座位的原生 AccountID 可能是个人座位，但 WHAM/流量打到同一工作区。
	return isWorkspaceSeatPlan(sibling) && strings.TrimSpace(sibling.EffectiveAccountID()) == workspaceID
}

func isWorkspaceSeatPlan(acc *Account) bool {
	if acc == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(acc.GetPlanType())) {
	case "team", "teamplus", "k12", "edu", "education":
		return true
	default:
		return false
	}
}

func nativeWorkspaceID(acc *Account) string {
	if acc == nil {
		return ""
	}
	acc.mu.RLock()
	defer acc.mu.RUnlock()
	return strings.TrimSpace(acc.AccountID)
}

func siblingErrorStatus(acc *Account) bool {
	if acc == nil {
		return false
	}
	acc.mu.RLock()
	defer acc.mu.RUnlock()
	return acc.Status == StatusError
}

// LinkedDeactivatedWorkspaceResult 供批量测试在打 WHAM 前短路：
// 该账号已因同空间停用被标错，或所属工作区刚被停用。
func (s *Store) LinkedDeactivatedWorkspaceResult(acc *Account) (string, bool) {
	if s == nil || acc == nil || acc.IsGrokAPI() || acc.IsOpenAIResponsesAPI() || acc.IsClaudeOAuth() {
		return "", false
	}
	acc.mu.RLock()
	status := acc.Status
	errMsg := acc.ErrorMsg
	acc.mu.RUnlock()
	if status == StatusError && strings.Contains(strings.ToLower(errMsg), "deactivated_workspace") {
		return errMsg, true
	}
	workspaceID := strings.TrimSpace(acc.EffectiveAccountID())
	nativeID := nativeWorkspaceID(acc)
	triggerID, ok := s.lookupDeactivatedWorkspace(workspaceID, nativeID)
	if !ok {
		return "", false
	}
	if nativeID != workspaceID && !isWorkspaceSeatPlan(acc) {
		return "", false
	}
	return deactivatedWorkspaceLinkedMessage(triggerID), true
}

func (s *Store) lookupDeactivatedWorkspace(ids ...string) (int64, bool) {
	now := time.Now()
	s.workspaceLinkedMu.Lock()
	defer s.workspaceLinkedMu.Unlock()
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if mark, ok := s.workspaceLinkedRecent[id]; ok && mark.until.After(now) {
			return mark.triggerID, true
		}
	}
	return 0, false
}

// markWorkspaceLinkedFired 以 workspaceID 为键做进程内去重：TTL 内同一工作区只允许一次 fan-out。
func (s *Store) markWorkspaceLinkedFired(workspaceID string, triggerID int64) bool {
	now := time.Now()
	s.workspaceLinkedMu.Lock()
	defer s.workspaceLinkedMu.Unlock()
	if mark, ok := s.workspaceLinkedRecent[workspaceID]; ok && mark.until.After(now) {
		return false
	}
	if s.workspaceLinkedRecent == nil {
		s.workspaceLinkedRecent = make(map[string]workspaceLinkedMark)
	}
	for k, mark := range s.workspaceLinkedRecent {
		if !mark.until.After(now) {
			delete(s.workspaceLinkedRecent, k)
		}
	}
	s.workspaceLinkedRecent[workspaceID] = workspaceLinkedMark{
		until:     now.Add(workspaceLinkedErrorDedupTTL),
		triggerID: triggerID,
	}
	return true
}
