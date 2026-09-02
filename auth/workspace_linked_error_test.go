package auth

import (
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/internal/openaiidentity"
)

func newWorkspaceLinkedAccount(id int64, workspaceID string) *Account {
	return &Account{
		DBID:        id,
		AccessToken: "at",
		AccountID:   workspaceID,
		Status:      StatusReady,
		HealthTier:  HealthTierHealthy,
	}
}

func TestMarkDeactivatedWorkspaceFansOutSameWorkspace(t *testing.T) {
	store := NewStore(nil, nil, nil)
	trigger := newWorkspaceLinkedAccount(1, "team-A")
	sameA := newWorkspaceLinkedAccount(2, "team-A")
	otherTeam := newWorkspaceLinkedAccount(3, "team-B")
	sameAAgain := newWorkspaceLinkedAccount(6, "team-A")
	alreadyError := newWorkspaceLinkedAccount(7, "team-A")
	alreadyError.Status = StatusError
	alreadyError.ErrorMsg = "old error"
	grok := newWorkspaceLinkedAccount(8, "team-A")
	grok.UpstreamType = UpstreamGrok
	responses := newWorkspaceLinkedAccount(9, "team-A")
	responses.UpstreamType = UpstreamOpenAIResponses
	responses.BaseURL = "https://api.openai.com/v1"
	responses.APIKey = "sk-test"
	for _, acc := range []*Account{trigger, sameA, otherTeam, sameAAgain, alreadyError, grok, responses} {
		store.AddAccount(acc)
	}

	store.MarkDeactivatedWorkspace(trigger, "上游返回 402: deactivated_workspace")

	if trigger.RuntimeStatus() != "error" {
		t.Fatalf("trigger status = %q, want error", trigger.RuntimeStatus())
	}
	if !strings.Contains(accountErrorMsg(trigger), "deactivated_workspace") {
		t.Fatalf("trigger ErrorMsg = %q, want original deactivated_workspace text", accountErrorMsg(trigger))
	}
	if strings.Contains(accountErrorMsg(trigger), "工作区联动") {
		t.Fatalf("trigger ErrorMsg = %q, must not use linked wording", accountErrorMsg(trigger))
	}

	for _, acc := range []*Account{sameA, sameAAgain} {
		if acc.RuntimeStatus() != "error" {
			t.Fatalf("sibling %d status = %q, want error", acc.DBID, acc.RuntimeStatus())
		}
		if atomic.LoadInt32(&acc.Disabled) == 0 {
			t.Fatalf("sibling %d should be instantly unschedulable", acc.DBID)
		}
		if !strings.Contains(accountErrorMsg(acc), "工作区联动") || !strings.Contains(accountErrorMsg(acc), "账号 1") {
			t.Fatalf("sibling %d ErrorMsg = %q, want linked trigger annotation", acc.DBID, accountErrorMsg(acc))
		}
	}

	if otherTeam.RuntimeStatus() == "error" {
		t.Fatal("other workspace must not be marked")
	}
	if alreadyError.ErrorMsg != "old error" {
		t.Fatalf("already-error sibling ErrorMsg = %q, want unchanged", alreadyError.ErrorMsg)
	}
	if grok.RuntimeStatus() == "error" {
		t.Fatal("grok sibling must not be linked")
	}
	if responses.RuntimeStatus() == "error" {
		t.Fatal("openai responses sibling must not be linked")
	}
}

func TestMarkDeactivatedWorkspaceDedupWithinTTL(t *testing.T) {
	store := NewStore(nil, nil, nil)
	first := newWorkspaceLinkedAccount(1, "team-A")
	second := newWorkspaceLinkedAccount(2, "team-A")
	store.AddAccount(first)
	store.AddAccount(second)

	store.MarkDeactivatedWorkspace(first, "上游返回 402: deactivated_workspace")

	late := newWorkspaceLinkedAccount(3, "team-A")
	store.AddAccount(late)
	store.MarkDeactivatedWorkspace(second, "上游返回 402: deactivated_workspace")

	if late.RuntimeStatus() == "error" {
		t.Fatal("same-workspace fan-out within TTL must be skipped")
	}
}

func TestMarkDeactivatedWorkspaceMissingWorkspaceIDDoesNothing(t *testing.T) {
	store := NewStore(nil, nil, nil)
	trigger := &Account{DBID: 9, AccessToken: "at", Status: StatusReady, HealthTier: HealthTierHealthy}
	sibling := newWorkspaceLinkedAccount(2, "team-A")
	store.AddAccount(trigger)
	store.AddAccount(sibling)

	store.MarkDeactivatedWorkspace(trigger, "上游返回 402: deactivated_workspace")

	if trigger.RuntimeStatus() != "error" {
		t.Fatalf("trigger status = %q, want error", trigger.RuntimeStatus())
	}
	if sibling.RuntimeStatus() == "error" {
		t.Fatal("sibling must stay untouched when trigger has no workspace id")
	}
}

func TestMarkDeactivatedWorkspaceOverrideTargetsNativeWorkspace(t *testing.T) {
	store := NewStore(nil, nil, nil)
	trigger := newWorkspaceLinkedAccount(1, "team-B")
	trigger.CustomHeaders = map[string]string{openaiidentity.ChatGPTAccountIDHeader: "team-A"}
	nativeA := newWorkspaceLinkedAccount(2, "team-A")
	nativeB := newWorkspaceLinkedAccount(3, "team-B")
	store.AddAccount(trigger)
	store.AddAccount(nativeA)
	store.AddAccount(nativeB)

	store.MarkDeactivatedWorkspace(trigger, "上游返回 402: deactivated_workspace")

	if nativeA.RuntimeStatus() != "error" {
		t.Fatal("native members of the overridden workspace should be marked")
	}
	if nativeB.RuntimeStatus() == "error" {
		t.Fatal("trigger native workspace must not be marked when 402 is about the override target")
	}
}

func TestMarkDeactivatedWorkspaceLinksTeamAndK12SeatsByEffectiveWorkspace(t *testing.T) {
	store := NewStore(nil, nil, nil)
	trigger := newWorkspaceLinkedAccount(1, "seat-trigger")
	trigger.PlanType = "team"
	trigger.CustomHeaders = map[string]string{openaiidentity.ChatGPTAccountIDHeader: "org-ws"}
	k12Seat := newWorkspaceLinkedAccount(2, "seat-k12")
	k12Seat.PlanType = "k12"
	k12Seat.CustomHeaders = map[string]string{openaiidentity.ChatGPTAccountIDHeader: "org-ws"}
	plusRouted := newWorkspaceLinkedAccount(3, "personal-plus")
	plusRouted.PlanType = "plus"
	plusRouted.CustomHeaders = map[string]string{openaiidentity.ChatGPTAccountIDHeader: "org-ws"}
	otherK12 := newWorkspaceLinkedAccount(4, "seat-other")
	otherK12.PlanType = "k12"
	otherK12.CustomHeaders = map[string]string{openaiidentity.ChatGPTAccountIDHeader: "other-ws"}
	for _, acc := range []*Account{trigger, k12Seat, plusRouted, otherK12} {
		store.AddAccount(acc)
	}

	store.MarkDeactivatedWorkspace(trigger, "上游返回 402: deactivated_workspace")

	if k12Seat.RuntimeStatus() != "error" {
		t.Fatal("k12 seat in the same effective workspace should be marked")
	}
	if plusRouted.RuntimeStatus() == "error" {
		t.Fatal("header-only plus account must not be marked")
	}
	if otherK12.RuntimeStatus() == "error" {
		t.Fatal("k12 seat in another workspace must not be marked")
	}
}

func TestMarkDeactivatedWorkspaceOverridesRateLimitedDisplay(t *testing.T) {
	store := NewStore(nil, nil, nil)
	trigger := newWorkspaceLinkedAccount(1, "org-ws")
	trigger.PlanType = "team"
	limited := newWorkspaceLinkedAccount(2, "org-ws")
	limited.PlanType = "k12"
	limited.SetUsagePercent7d(100)
	limited.SetReset7dAt(time.Now().Add(time.Hour))
	if !store.MarkUsage7dRateLimited(limited) {
		t.Fatal("expected 7d rate limit")
	}
	if got := limited.RuntimeStatus(); got != "rate_limited" {
		t.Fatalf("before link RuntimeStatus() = %q, want rate_limited", got)
	}
	store.AddAccount(trigger)
	store.AddAccount(limited)

	store.MarkDeactivatedWorkspace(trigger, "上游返回 402: deactivated_workspace")

	if got := limited.RuntimeStatus(); got != "error" {
		t.Fatalf("after link RuntimeStatus() = %q, want error (not rate_limited)", got)
	}
	if !strings.Contains(accountErrorMsg(limited), "工作区联动") {
		t.Fatalf("ErrorMsg = %q, want linked annotation", accountErrorMsg(limited))
	}
}

func TestLinkedDeactivatedWorkspaceResultSkipsSameWorkspaceSeats(t *testing.T) {
	store := NewStore(nil, nil, nil)
	trigger := newWorkspaceLinkedAccount(1, "org-ws")
	trigger.PlanType = "team"
	lateK12 := newWorkspaceLinkedAccount(2, "seat-late")
	lateK12.PlanType = "k12"
	lateK12.CustomHeaders = map[string]string{openaiidentity.ChatGPTAccountIDHeader: "org-ws"}
	plusRouted := newWorkspaceLinkedAccount(3, "personal-plus")
	plusRouted.PlanType = "plus"
	plusRouted.CustomHeaders = map[string]string{openaiidentity.ChatGPTAccountIDHeader: "org-ws"}
	store.AddAccount(trigger)
	store.MarkDeactivatedWorkspace(trigger, "上游返回 402: deactivated_workspace")

	msg, ok := store.LinkedDeactivatedWorkspaceResult(lateK12)
	if !ok || !strings.Contains(msg, "工作区联动") || !strings.Contains(msg, "账号 1") {
		t.Fatalf("late k12 skip = (%q, %v)", msg, ok)
	}
	if msg, ok := store.LinkedDeactivatedWorkspaceResult(plusRouted); ok {
		t.Fatalf("header-only plus must not skip probe: %q", msg)
	}
}

func TestMarkDeactivatedWorkspaceSkipsGrokAndResponsesTriggers(t *testing.T) {
	store := NewStore(nil, nil, nil)
	sibling := newWorkspaceLinkedAccount(2, "team-A")
	store.AddAccount(sibling)

	grok := newWorkspaceLinkedAccount(1, "team-A")
	grok.UpstreamType = UpstreamGrok
	store.AddAccount(grok)
	store.MarkDeactivatedWorkspace(grok, "上游返回 402: deactivated_workspace")
	if sibling.RuntimeStatus() == "error" {
		t.Fatal("grok trigger must not fan out")
	}

	store.workspaceLinkedRecent = nil
	responses := newWorkspaceLinkedAccount(3, "team-A")
	responses.UpstreamType = UpstreamOpenAIResponses
	responses.BaseURL = "https://api.openai.com/v1"
	responses.APIKey = "sk-test"
	store.AddAccount(responses)
	store.MarkDeactivatedWorkspace(responses, "上游返回 402: deactivated_workspace")
	if sibling.RuntimeStatus() == "error" {
		t.Fatal("openai responses trigger must not fan out")
	}

	store.workspaceLinkedRecent = nil
	claude := newWorkspaceLinkedAccount(4, "team-A")
	claude.UpstreamType = UpstreamClaude
	store.AddAccount(claude)
	store.MarkDeactivatedWorkspace(claude, "upstream Claude workspace error")
	if sibling.RuntimeStatus() == "error" {
		t.Fatal("Claude trigger must not fan out into Codex workspace accounts")
	}
}

func accountErrorMsg(acc *Account) string {
	acc.Mu().RLock()
	defer acc.Mu().RUnlock()
	return acc.ErrorMsg
}
