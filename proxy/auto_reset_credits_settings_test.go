package proxy

import (
	"testing"

	"github.com/codex2api/database"
)

func TestDefaultRuntimeSettingsAutoResetCreditsDisabled(t *testing.T) {
	settings := DefaultRuntimeSettings()
	if settings.AutoResetCreditsEnabled {
		t.Fatal("AutoResetCreditsEnabled = true, want false")
	}
	if settings.AutoResetCreditsBeforeExpiryMin != 60 {
		t.Fatalf("AutoResetCreditsBeforeExpiryMin = %d, want 60", settings.AutoResetCreditsBeforeExpiryMin)
	}
}

func TestNormalizeRuntimeSettingsAutoResetCreditsWindow(t *testing.T) {
	settings := DefaultRuntimeSettings()
	settings.AutoResetCreditsBeforeExpiryMin = 1
	settings = NormalizeRuntimeSettings(settings)
	if settings.AutoResetCreditsBeforeExpiryMin != 10 {
		t.Fatalf("below minimum = %d, want 10", settings.AutoResetCreditsBeforeExpiryMin)
	}

	settings.AutoResetCreditsBeforeExpiryMin = 20000
	settings = NormalizeRuntimeSettings(settings)
	if settings.AutoResetCreditsBeforeExpiryMin != 10080 {
		t.Fatalf("above maximum = %d, want 10080", settings.AutoResetCreditsBeforeExpiryMin)
	}
}

func TestApplyRuntimeSettingsFromSystemAutoResetCredits(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })

	settings := ApplyRuntimeSettingsFromSystem(&database.SystemSettings{
		AutoResetCreditsEnabled:         true,
		AutoResetCreditsBeforeExpiryMin: 90,
	})
	if !settings.AutoResetCreditsEnabled {
		t.Fatal("AutoResetCreditsEnabled = false, want true")
	}
	if settings.AutoResetCreditsBeforeExpiryMin != 90 {
		t.Fatalf("AutoResetCreditsBeforeExpiryMin = %d, want 90", settings.AutoResetCreditsBeforeExpiryMin)
	}
}
