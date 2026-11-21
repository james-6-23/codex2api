package wsrelay

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
)

func TestPinnedAccountVersionFollowsSyncOnWebsocketHandshake(t *testing.T) {
	old := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(old) })
	t.Setenv("CODEX_DISABLE_GENERATED_ATTESTATION", "true")
	t.Setenv("CODEX_WS_SEND_USER_AGENT", "true")
	ua := "codex-tui/{{codex_cli_version}} (Windows 10.0.19045; x86_64) unknown (codex-tui; {{codex_cli_version}})"
	account := &auth.Account{DBID: 1, AccountID: "workspace", CustomHeaders: map[string]string{"User-Agent": ua, "Version": proxy.CodexCLIVersionPlaceholder, "Originator": "codex-tui"}}
	device := &proxy.DeviceProfileConfig{StabilizeDeviceProfile: true}
	downstream := http.Header{
		"User-Agent": {"Codex Desktop/0.169.0-alpha.11.1 (Windows 10.0.26200; x86_64) unknown (Codex Desktop; 26.1111.21308)"},
		"Originator": {"Codex Desktop"},
	}
	for _, test := range []struct{ mode, synced, want string }{
		{proxy.ClientCompatModeForce, "0.169.0", "0.170.0"},
		{proxy.ClientCompatModeForce, "0.171.0", "0.171.0"},
		{proxy.ClientCompatModePreserve, "0.172.0", "0.172.0"},
	} {
		settings := old
		settings.ClientCompatMode, settings.CodexSyncedCLIVersion = test.mode, test.synced
		proxy.ApplyRuntimeSettings(settings)
		version := test.want
		headers, err := NewExecutor().prepareWebsocketHeadersChecked(context.Background(), "token", account, "workspace", "session", "key", device, downstream, nil, "")
		if err != nil {
			t.Fatal(err)
		}
		if headers.Get("User-Agent") != strings.ReplaceAll(ua, proxy.CodexCLIVersionPlaceholder, version) || headers.Get("Version") != version || headers.Get("Originator") != "codex-tui" {
			t.Fatalf("mode=%s synced=%s: wrong handshake identity: %v", test.mode, test.synced, headers)
		}
	}
	if account.GetCustomHeaders()["User-Agent"] != ua {
		t.Fatal("stored template mutated")
	}
}
