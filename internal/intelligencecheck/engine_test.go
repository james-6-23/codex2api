package intelligencecheck

import (
	"strings"
	"testing"
)

func TestGrade(t *testing.T) {
	q := DefaultSettings().Questions[0]
	for _, tt := range []struct {
		name, out, mode string
		ms              int64
		complete        bool
		err, want       string
	}{
		{"fullwidth", " ２１ ", "answer", 20000, true, "", "normal"},
		{"fence", "```text\n21\n```", "answer", 1, true, "", "normal"},
		{"embedded", "wrong: 21 or 29", "answer", 1, true, "", "degraded"},
		{"partial", "21", "answer", 1, false, "", "error"},
		{"transport", "21", "answer", 1, true, "broken", "error"},
		{"boundary", "21", "latency", 20000, true, "", "degraded"},
		{"below", "wrong", "latency", 19999, true, "", "normal"},
		{"both", "wrong", "both", 19999, true, "", "degraded"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := Grade(q, tt.mode, Observation{Output: tt.out, DurationMS: tt.ms, Complete: tt.complete, Error: tt.err})
			if got.Status != tt.want {
				t.Fatalf("%+v", got)
			}
		})
	}
	q.MatchMode = "contains"
	if Grade(q, "answer", Observation{Output: "answer 21", Complete: true}).Status != "normal" {
		t.Fatal("contains")
	}
	q.MatchMode = "regex"
	q.ExpectedAnswer = "^2[01]$"
	if Grade(q, "answer", Observation{Output: "21", Complete: true}).Status != "normal" {
		t.Fatal("regex")
	}
}
func TestDefaultCandyPromptRequiresOfflineReasoning(t *testing.T) {
	p := DefaultSettings().Questions[0].Prompt
	if !strings.HasPrefix(p, "不允许调用工具和联网。") {
		t.Fatalf("candy prompt must forbid tools and network access: %q", p)
	}
	if !strings.HasSuffix(p, "最终只回复数字结果。") {
		t.Fatalf("candy prompt must ask for a numeric answer: %q", p)
	}
}

func TestValidation(t *testing.T) {
	s := DefaultSettings()
	if err := ValidateSettings(s); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Settings){func(s *Settings) { s.Action = "pause,switch_group" }, func(s *Settings) { s.Questions[0].MatchMode = "regex"; s.Questions[0].ExpectedAnswer = "(?=21)" }, func(s *Settings) { s.Concurrency = 0 }, func(s *Settings) { s.Enabled = true; s.Questions[0].Enabled = false }, func(s *Settings) { s.Action = "switch_group" }} {
		s = DefaultSettings()
		mutate(&s)
		if ValidateSettings(s) == nil {
			t.Fatal("accepted invalid settings")
		}
	}
}
func TestTransitionOwnership(t *testing.T) {
	c := DefaultSettings()
	c.Action = "pause"
	s := State{}
	s, a := Transition(c, s, "degraded")
	if a != "" || s.DegradeStreak != 1 {
		t.Fatal(s, a)
	}
	s, _ = Transition(c, s, "error")
	s, a = Transition(c, s, "degraded")
	if a != "pause" || s.OwnedAction != "pause" {
		t.Fatal(s, a)
	}
	c.Action = "none"
	s, a = Transition(c, s, "normal")
	if a != "" || s.OwnedAction != "pause" {
		t.Fatal(s, a)
	}
	c.Action = "pause"
	s, a = Transition(c, s, "normal")
	if a != "resume" || s.OwnedAction != "" {
		t.Fatal(s, a)
	}
	c.Action = "switch_group"
	s = State{DegradeStreak: 1}
	s, a = Transition(c, s, "degraded")
	if a != "switch_group" {
		t.Fatal(s, a)
	}
	c.Action = "none"
	s, a = Transition(c, s, "normal")
	if a != "restore_groups" {
		t.Fatal(s, a)
	}
	s = State{OwnedAction: "pause", DegradeStreak: 1}
	c.Action = "switch_group"
	s, a = Transition(c, s, "degraded")
	if a != "" || s.OwnedAction != "pause" {
		t.Fatal("stacked", s, a)
	}
}
