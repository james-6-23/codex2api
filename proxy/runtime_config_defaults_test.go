package proxy

import "testing"

func TestDefaultRuntimeSettingsCodexMinCLIVersion(t *testing.T) {
	if got := DefaultRuntimeSettings().CodexMinCLIVersion; got != "0.153.3" {
		t.Fatalf("CodexMinCLIVersion = %q, want 0.153.3", got)
	}
}
