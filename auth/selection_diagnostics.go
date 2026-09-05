package auth

import (
	"context"
	"sort"
	"sync"
	"time"
)

type SelectionDiagnostic struct {
	Stage       string   `json:"stage"`
	Reason      string   `json:"reason"`
	Reasons     []string `json:"reasons"`
	RootAccount int64    `json:"root_account,omitempty"`
	Retry       string   `json:"retry"`
	Incomplete  bool     `json:"incomplete,omitempty"`
}

type SelectionTrace struct {
	mu          sync.Mutex
	reasons     map[string]struct{}
	rootAccount int64
	suspended   int
	frozen      bool
}

type selectionTraceContextKey struct{}

func WithSelectionTrace(ctx context.Context, trace *SelectionTrace) context.Context {
	return context.WithValue(ctx, selectionTraceContextKey{}, trace)
}

func SelectionTraceFromContext(ctx context.Context) *SelectionTrace {
	if ctx == nil {
		return nil
	}
	trace, _ := ctx.Value(selectionTraceContextKey{}).(*SelectionTrace)
	return trace
}

func selectionTrace(traces []*SelectionTrace) *SelectionTrace {
	if len(traces) == 0 {
		return nil
	}
	return traces[0]
}

func (trace *SelectionTrace) Reject(reason string) {
	if trace == nil {
		return
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	if trace.frozen || trace.suspended > 0 {
		return
	}
	if trace.reasons == nil {
		trace.reasons = make(map[string]struct{})
	}
	if len(trace.reasons) < 23 {
		trace.reasons[reason] = struct{}{}
	} else if _, known := trace.reasons[reason]; !known {
		trace.reasons["diagnosis_incomplete"] = struct{}{}
	}
}

func (trace *SelectionTrace) Bind(accountID int64) {
	if trace == nil {
		return
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	if !trace.frozen {
		trace.rootAccount = accountID
	}
}

func (trace *SelectionTrace) Freeze() {
	if trace != nil {
		trace.mu.Lock()
		trace.frozen = true
		trace.mu.Unlock()
	}
}

func (trace *SelectionTrace) Pause() func() {
	if trace == nil {
		return func() {}
	}
	trace.mu.Lock()
	trace.suspended++
	trace.mu.Unlock()
	return func() {
		trace.mu.Lock()
		trace.suspended--
		trace.mu.Unlock()
	}
}

func (trace *SelectionTrace) Reset() {
	if trace != nil {
		trace.mu.Lock()
		trace.reasons = nil
		trace.rootAccount = 0
		trace.frozen = false
		trace.mu.Unlock()
	}
}

func (account *Account) selectionUnavailableReasonLocked(now time.Time) string {
	switch {
	case account.Status == StatusError:
		return "account_error"
	case account.healthTierLocked() == HealthTierBanned:
		return "account_banned"
	case account.Status == StatusCooldown && now.Before(account.CooldownUtil):
		return "account_cooldown"
	case account.usageWindowBlocksFreshDispatchLocked(now), account.quotaAutoPausedLocked(now):
		return "account_usage_exhausted"
	case !account.hasDispatchCredentialLocked():
		return "credential_unavailable"
	default:
		return "account_unavailable"
	}
}

func (trace *SelectionTrace) Filter(reason string, filter AccountFilter) AccountFilter {
	if trace == nil || filter == nil {
		return filter
	}
	return func(account *Account) bool {
		if filter(account) {
			return true
		}
		trace.Reject(reason)
		return false
	}
}

func (trace *SelectionTrace) Snapshot() SelectionDiagnostic {
	result := SelectionDiagnostic{Stage: "account_selection", Reason: "diagnosis_incomplete", Retry: "default", Incomplete: true, Reasons: []string{}}
	if trace == nil {
		return result
	}
	trace.mu.Lock()
	defer trace.mu.Unlock()
	for reason := range trace.reasons {
		result.Reasons = append(result.Reasons, reason)
	}
	sort.Strings(result.Reasons)
	result.RootAccount = trace.rootAccount
	if len(result.Reasons) == 0 {
		return result
	}
	result.Incomplete = false
	result.Reason = "mixed_constraints"
	result.Retry = "backoff_same_route"
	for _, reason := range result.Reasons {
		if reason == "indexed_candidates_unavailable" || reason == "diagnosis_incomplete" || reason == "dispatch_state_changed" || reason == "lazy_account_unavailable" || reason == "lazy_refresh_failed" || reason == "lazy_refresh_pending" || reason == "account_unavailable" {
			result.Incomplete = true
		}
		if reason != "account_cooldown" && reason != "model_cooldown" && reason != "concurrency_exhausted" && reason != "scope_concurrency_exhausted" && reason != "session_capacity_exhausted" {
			result.Retry = "stop"
		}
	}
	if len(result.Reasons) == 1 {
		result.Reason = result.Reasons[0]
	}
	if result.RootAccount > 0 {
		result.Reason = "root_owner_unavailable"
	}
	if result.Incomplete {
		result.Retry = "default"
	}
	return result
}
