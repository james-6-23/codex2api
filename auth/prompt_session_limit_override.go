package auth

import (
	"strings"

	"github.com/codex2api/database"
)

func promptSessionLimitOverrideKey(platform, userID string) string {
	platform = strings.ToLower(strings.TrimSpace(platform))
	userID = strings.TrimSpace(userID)
	if platform == "" || userID == "" {
		return ""
	}
	return platform + "\x00" + userID
}

// ReplacePromptSessionLimitOverrides atomically loads the persisted control
// plane snapshot. Only explicit custom/off rows are stored; a miss means the
// verified person inherits the global Prompt session policy.
func (s *Store) ReplacePromptSessionLimitOverrides(items []database.PromptSessionLimitOverride) {
	if s == nil {
		return
	}
	next := make(map[string]database.PromptSessionLimitOverride, len(items))
	for _, item := range items {
		if key := promptSessionLimitOverrideKey(item.Platform, item.NewAPIUserID); key != "" {
			next[key] = item
		}
	}
	s.promptSessionLimitOverridesMu.Lock()
	s.promptSessionLimitOverrides = next
	s.promptSessionLimitOverridesMu.Unlock()
}

func (s *Store) ApplyPromptSessionLimitOverride(item database.PromptSessionLimitOverride) {
	if s == nil {
		return
	}
	key := promptSessionLimitOverrideKey(item.Platform, item.NewAPIUserID)
	if key == "" {
		return
	}
	s.promptSessionLimitOverridesMu.Lock()
	if s.promptSessionLimitOverrides == nil {
		s.promptSessionLimitOverrides = make(map[string]database.PromptSessionLimitOverride)
	}
	s.promptSessionLimitOverrides[key] = item
	s.promptSessionLimitOverridesMu.Unlock()
}

func (s *Store) DeletePromptSessionLimitOverride(platform, userID string) {
	if s == nil {
		return
	}
	key := promptSessionLimitOverrideKey(platform, userID)
	if key == "" {
		return
	}
	s.promptSessionLimitOverridesMu.Lock()
	delete(s.promptSessionLimitOverrides, key)
	s.promptSessionLimitOverridesMu.Unlock()
}

func (s *Store) GetPromptSessionLimitOverride(platform, userID string) (database.PromptSessionLimitOverride, bool) {
	if s == nil {
		return database.PromptSessionLimitOverride{}, false
	}
	key := promptSessionLimitOverrideKey(platform, userID)
	if key == "" {
		return database.PromptSessionLimitOverride{}, false
	}
	s.promptSessionLimitOverridesMu.RLock()
	item, ok := s.promptSessionLimitOverrides[key]
	s.promptSessionLimitOverridesMu.RUnlock()
	return item, ok
}
