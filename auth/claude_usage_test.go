package auth

import (
	"testing"
	"time"
)

func TestParseClaudeOAuthUsageIncludesFableModelScopedWindow(t *testing.T) {
	body := []byte(`{
      "five_hour":{"utilization":14,"resets_at":"2026-09-02T12:00:00Z"},
      "seven_day":{"utilization":1,"resets_at":"2026-09-08T00:00:00Z"},
      "limits":[{"group":"weekly","percent":63,"resets_at":"2026-09-08T00:00:00Z","scope":{"model":{"display_name":"Claude Fable 5"}}}]
    }`)
	windows, err := ParseClaudeOAuthUsage(body)
	if err != nil {
		t.Fatalf("ParseClaudeOAuthUsage: %v", err)
	}
	if len(windows) != 3 {
		t.Fatalf("windows len = %d, want 3 (%+v)", len(windows), windows)
	}
	var fable *ClaudeUsageWindow
	for i := range windows {
		if windows[i].Name == "7d_fable" {
			fable = &windows[i]
		}
	}
	if fable == nil || fable.Utilization != 63 || fable.Label != "Fable 5.x" || !fable.ModelScoped {
		t.Fatalf("Fable window = %+v", fable)
	}
	if fable.ResetAt.IsZero() || !fable.ResetAt.Equal(time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("Fable reset = %v", fable.ResetAt)
	}
}
