package intelligencecheck

import "testing"

func adaptiveTestSettings() Settings {
	c := DefaultSettings()
	c.AdaptiveEnabled, c.Action = true, "pause"
	c.IntervalSeconds, c.RetrySeconds = 1800, 300
	c.DegradeThreshold, c.RecoverThreshold = 3, 3
	return c
}

func step(c Settings, before State, status string) (State, string, int) {
	next, action := Transition(c, before, status)
	next, delay := Schedule(c, before, next, status)
	return next, action, delay
}

func TestAdaptivePauseRecoveryAndRelapse(t *testing.T) {
	c := adaptiveTestSettings()
	s := State{DegradeStreak: 2}
	s, action, delay := step(c, s, "degraded")
	if action != "pause" || delay != 1800 {
		t.Fatal(s, action, delay)
	}
	for i, want := range []int{3600, 7200, 14400, 28800, 57600, 115200, 172800, 172800} {
		status := "degraded"
		if i%2 == 0 {
			status = "error"
		}
		s, action, delay = step(c, s, status)
		if delay != want || action != "" || s.ProbePhase != "backoff" {
			t.Fatal(i, s, action, delay)
		}
	}
	for i, want := range []int{300, 120, 1800} {
		s, action, delay = step(c, s, "normal")
		if delay != want || (action == "resume") != (i == 2) {
			t.Fatal(i, s, action, delay)
		}
	}
	if s.PauseBackoffSeconds != 172800 {
		t.Fatal("confirmation erased fault memory", s)
	}
	s, _, delay = step(c, s, "normal")
	if s.PauseBackoffSeconds != 86400 || delay != 1800 {
		t.Fatal(s, delay)
	}
	for i := 0; i < 3; i++ {
		s, action, delay = step(c, s, "degraded")
	}
	if action != "pause" || delay != 86400 {
		t.Fatal("relapse must retain reduced penalty", s, action, delay)
	}
}

func TestAdaptiveFailedConfirmationKeepsPenalty(t *testing.T) {
	for _, status := range []string{"error", "interrupted", "degraded"} {
		t.Run(status, func(t *testing.T) {
			c := adaptiveTestSettings()
			s := State{OwnedAction: "pause", PauseBackoffSeconds: 28800}
			s, _, _ = step(c, s, "normal")
			s, action, delay := step(c, s, status)
			if action != "" || delay != 28800 || s.RecoverStreak != 0 {
				t.Fatal(s, action, delay)
			}
			s, _, delay = step(c, s, "error")
			if delay != 57600 {
				t.Fatal("next failed probe should resume backoff", s, delay)
			}
		})
	}
}

func TestAdaptiveActiveErrorsAndLegacyPolicy(t *testing.T) {
	c := adaptiveTestSettings()
	s := State{}
	for _, want := range []int{300, 600, 1200, 1800, 1800} {
		var delay int
		s, _, delay = step(c, s, "error")
		if delay != want || s.DegradeStreak != 0 {
			t.Fatal(s, delay)
		}
	}
	s, _, _ = step(c, s, "normal")
	s, _, delay := step(c, s, "error")
	if delay != 300 {
		t.Fatal(s, delay)
	}
	c.AdaptiveEnabled = false
	s = State{OwnedAction: "pause", RecoverStreak: 1, PauseBackoffSeconds: 172800}
	s, _, delay = step(c, s, "error")
	if delay != 300 || s.RecoverStreak != 1 || s.PauseBackoffSeconds != 0 {
		t.Fatal("legacy behavior changed", s, delay)
	}
}

func TestAdaptiveConfigBoundsAndHistoricalPause(t *testing.T) {
	c := adaptiveTestSettings()
	c.PauseInitialSeconds, c.PauseMaxSeconds = 900, 259200
	s := State{OwnedAction: "pause"}
	s, _, delay := step(c, s, "error")
	if delay != 900 {
		t.Fatal(s, delay)
	}
	for i := 0; i < 12; i++ {
		s, _, delay = step(c, s, "degraded")
	}
	if delay != 259200 {
		t.Fatal("maximum hard-coded to 48h", delay)
	}
	c.PauseMaxSeconds = 3600
	s, _, delay = step(c, s, "error")
	if delay != 3600 {
		t.Fatal("reduced cap ignored", delay)
	}
	for _, mutate := range []func(*Settings){
		func(c *Settings) { c.PauseMaxSeconds = c.PauseInitialSeconds - 1 },
		func(c *Settings) { c.PauseMaxSeconds = 30*86400 + 1 },
		func(c *Settings) { c.RecoveryMinSeconds = c.RecoveryInitialSeconds + 1 },
		func(c *Settings) { c.RecoveryInitialSeconds = c.PauseInitialSeconds + 1 },
	} {
		bad := c
		mutate(&bad)
		if ValidateSettings(bad) == nil {
			t.Fatal("accepted invalid settings", bad)
		}
	}
}

func TestAdaptiveDoesNotAccelerateUnownedRecovery(t *testing.T) {
	c := adaptiveTestSettings()
	c.Action = "none"
	s := State{OwnedAction: "pause", PauseBackoffSeconds: 86400, RecoverStreak: 4}
	s, action, delay := step(c, s, "normal")
	if action != "" || delay != 86400 || s.ProbePhase != "backoff" {
		t.Fatal(s, action, delay)
	}
}
