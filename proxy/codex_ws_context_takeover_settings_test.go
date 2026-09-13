package proxy

import (
	"testing"

	"github.com/codex2api/database"
)

func TestCodexWSContextTakeoverSettingsRuntime(test *testing.T) {
	previous := CurrentRuntimeSettings()
	test.Cleanup(func() { ApplyRuntimeSettings(previous) })
	if DefaultRuntimeSettings().CodexWSContextTakeover || NormalizeRuntimeSettings(RuntimeSettings{}).CodexWSContextTakeover {
		test.Fatal("WS context takeover must default off")
	}
	for _, enabled := range []bool{true, false} {
		next := ApplyRuntimeSettingsFromSystem(&database.SystemSettings{
			CodexWSContextTakeover:      enabled,
			CodexRequestCompression:     !enabled,
			CodexSessionFailoverEnabled: !enabled,
		})
		if next.CodexWSContextTakeover != enabled || CurrentRuntimeSettings().CodexWSContextTakeover != enabled {
			test.Fatalf("persisted context takeover setting did not reach runtime: want %t", enabled)
		}
		if next.CodexRequestCompression != !enabled || next.CodexSessionFailoverEnabled != !enabled {
			test.Fatal("WS dictionary reuse changed HTTP compression or session failover")
		}
		if NormalizeRuntimeSettings(next).CodexWSContextTakeover != enabled {
			test.Fatal("normalization changed WS context takeover")
		}
		next = UpdateRuntimeSettings(func(current RuntimeSettings) RuntimeSettings {
			current.CodexCapacityRetryEnabled = true
			return current
		})
		if next.CodexWSContextTakeover != enabled {
			test.Fatal("unrelated runtime update changed WS context takeover")
		}
	}
	ApplyRuntimeSettings(RuntimeSettings{CodexWSContextTakeover: true})
	if ApplyRuntimeSettingsFromSystem(nil).CodexWSContextTakeover || CurrentRuntimeSettings().CodexWSContextTakeover {
		test.Fatal("missing system settings must reset WS context takeover to off")
	}
}
