package auth

import (
	"testing"
	"time"
)

func TestShouldActivate5hWindow(t *testing.T) {
	now := time.Date(2026, 8, 27, 4, 0, 0, 0, time.UTC)
	resetAt := now.Add(-time.Minute)

	base := func() *Account {
		account := &Account{
			AccessToken: "at-x",
			Status:      StatusReady,
			PlanType:    "team",
		}
		account.SetUsageSnapshot5hAt(80, resetAt, now.Add(-2*time.Hour))
		return account
	}

	t.Run("reset passed with observed 5h", func(t *testing.T) {
		if !base().ShouldActivate5hWindow(now) {
			t.Fatal("expected activation after 5h reset")
		}
	})

	t.Run("plus account with observed 5h is eligible", func(t *testing.T) {
		account := base()
		account.PlanType = "plus"
		if !account.ShouldActivate5hWindow(now) {
			t.Fatal("observed 5h should not be gated on team plan")
		}
	})

	t.Run("reset still in the future", func(t *testing.T) {
		account := base()
		account.SetUsageSnapshot5hAt(80, now.Add(time.Hour), now)
		if account.ShouldActivate5hWindow(now) {
			t.Fatal("future Reset5hAt must not activate")
		}
	})

	t.Run("missing 5h snapshot", func(t *testing.T) {
		account := &Account{AccessToken: "at-x", Status: StatusReady, PlanType: "team"}
		if account.ShouldActivate5hWindow(now) {
			t.Fatal("absent 5h window must be skipped")
		}
	})

	t.Run("already activated this reset", func(t *testing.T) {
		account := base()
		account.Mark5hWindowActivated(resetAt)
		if account.ShouldActivate5hWindow(now) {
			t.Fatal("same Reset5hAt must activate at most once")
		}
	})

	t.Run("new reset after previous activation", func(t *testing.T) {
		account := base()
		account.Mark5hWindowActivated(resetAt.Add(-5 * time.Hour))
		if !account.ShouldActivate5hWindow(now) {
			t.Fatal("a newer Reset5hAt should be eligible again")
		}
	})

	t.Run("error account", func(t *testing.T) {
		account := base()
		account.Status = StatusError
		if account.ShouldActivate5hWindow(now) {
			t.Fatal("error accounts must be skipped")
		}
	})

	t.Run("cooldown still active", func(t *testing.T) {
		account := base()
		account.Status = StatusCooldown
		account.CooldownReason = "unauthorized"
		account.CooldownUtil = now.Add(time.Hour)
		if account.ShouldActivate5hWindow(now) {
			t.Fatal("unavailable cooldown accounts must be skipped")
		}
	})

	t.Run("7d auto-pause still active", func(t *testing.T) {
		account := base()
		account.UsagePercent7d = 95
		account.UsagePercent7dValid = true
		account.Reset7dAt = now.Add(24 * time.Hour)
		account.effectiveAutoPause7d = 0.9
		if account.ShouldActivate5hWindow(now) {
			t.Fatal("auto-paused accounts must be skipped")
		}
	})

	t.Run("relay account", func(t *testing.T) {
		account := base()
		account.UpstreamType = UpstreamOpenAIResponses
		account.BaseURL = "https://relay.example"
		account.APIKey = "sk-relay"
		if account.ShouldActivate5hWindow(now) {
			t.Fatal("relay accounts must be skipped")
		}
	})
}
