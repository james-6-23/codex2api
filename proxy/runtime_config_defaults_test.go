package proxy

import "testing"

func TestDefaultRuntimeSettingsCodexMinCLIVersion(t *testing.T) {
	if got := DefaultRuntimeSettings().CodexMinCLIVersion; got != "0.160.0" {
		t.Fatalf("CodexMinCLIVersion = %q, want 0.160.0", got)
	}
}
