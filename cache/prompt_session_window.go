package cache

import (
	"strings"
	"time"
)

// PromptSessionLimitRuntimeNamespace is shared by the proxy admission path and
// the administrator-only active-window view. Keep the existing namespace so a
// rolling upgrade does not discard windows that are still counting down.
const PromptSessionLimitRuntimeNamespace = "prompt-session-limit-v1"

// PromptSessionWindowDetail contains only operational metadata for one active
// NewAPI user window. PromptPreview is redacted and bounded before persistence.
// The authoritative expiry remains in PromptSessionLimitState.Sessions for
// backward compatibility with the original v1 payload.
type PromptSessionWindowDetail struct {
	CreatedAt       time.Time `json:"created_at,omitempty"`
	ExpiresAt       time.Time `json:"expires_at,omitempty"`
	AccountID       int64     `json:"account_id,omitempty"`
	Model           string    `json:"model,omitempty"`
	ReasoningEffort string    `json:"reasoning_effort,omitempty"`
	ClientUserAgent string    `json:"client_user_agent,omitempty"`
	PromptPreview   string    `json:"prompt_preview,omitempty"`
}

// PromptSessionLimitState is the shared runtime-cache payload for one user.
// Sessions preserves the legacy session-hash -> expiry representation, while
// Details enriches only currently active entries.
type PromptSessionLimitState struct {
	Version  int                                  `json:"version"`
	Sessions map[string]time.Time                 `json:"sessions"`
	Details  map[string]PromptSessionWindowDetail `json:"details,omitempty"`
}

func PromptSessionLimitSubject(platform, userID string) string {
	platform = strings.ToLower(strings.TrimSpace(platform))
	userID = strings.TrimSpace(userID)
	if platform == "" || userID == "" {
		return ""
	}
	return "newapi:" + platform + ":" + userID
}
