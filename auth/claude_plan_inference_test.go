package auth

import (
	"context"
	"testing"
)

func TestApplyClaudePlanFromCreditsRequired(t *testing.T) {
	s := NewStore(nil, nil, nil)
	defer s.Stop()
	cases := []struct {
		plan        string
		wantPlan    string
		wantChanged bool
	}{
		{"claude", "pro", true},
		{"", "pro", true},
		{"max", "pro", true},
		{"max-20x", "pro", true},
		{"pro", "pro", false},
		{"team", "team", false},
		{"free", "free", false},
		{"enterprise", "enterprise", false},
	}
	for _, c := range cases {
		acc := &Account{UpstreamType: UpstreamClaude, AccessToken: "at", PlanType: c.plan}
		_, got, changed := s.ApplyClaudePlanFromCreditsRequired(context.Background(), acc)
		if changed != c.wantChanged || acc.GetPlanType() != c.wantPlan || (changed && got != c.wantPlan) {
			t.Fatalf("plan %q -> (%q changed=%v), account plan=%q; want %q changed=%v", c.plan, got, changed, acc.GetPlanType(), c.wantPlan, c.wantChanged)
		}
	}
	codex := &Account{PlanType: "plus", AccessToken: "at"}
	if _, _, changed := s.ApplyClaudePlanFromCreditsRequired(context.Background(), codex); changed || codex.GetPlanType() != "plus" {
		t.Fatal("non-Claude account must never be touched")
	}
}

func TestApplyClaudePlanFromGatedModelSuccess(t *testing.T) {
	s := NewStore(nil, nil, nil)
	defer s.Stop()
	if !IsClaudeCreditsGatedModel("claude-fable-5") || IsClaudeCreditsGatedModel("claude-sonnet-4-5") {
		t.Fatal("gated model detection drifted")
	}
	for plan, want := range map[string]string{"claude": "max", "": "max", "pro": "max", "max-5x": "max-5x", "team": "team"} {
		acc := &Account{UpstreamType: UpstreamClaude, AccessToken: "at", PlanType: plan}
		s.ApplyClaudePlanFromGatedModelSuccess(context.Background(), acc, "claude-fable-5")
		if acc.GetPlanType() != want {
			t.Fatalf("plan %q after fable success = %q, want %q", plan, acc.GetPlanType(), want)
		}
	}
	acc := &Account{UpstreamType: UpstreamClaude, AccessToken: "at", PlanType: "claude"}
	if _, _, changed := s.ApplyClaudePlanFromGatedModelSuccess(context.Background(), acc, "claude-sonnet-4-5"); changed {
		t.Fatal("non-gated model success must not infer max")
	}
}
