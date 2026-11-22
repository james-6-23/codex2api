package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/codex2api/auth"
)

func TestPinnedAccountVersionFollowsSyncOnHTTPCompactAndMaintenance(t *testing.T) {
	old := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(old) })
	t.Setenv("CODEX_DISABLE_GENERATED_ATTESTATION", "true")
	t.Setenv("CODEX_ACCOUNT_INSTALLATION_IDS", officialWirePins)
	ua := "codex-tui/" + CodexCLIVersionPlaceholder + " (Windows 10.0.19045; x86_64) unknown (codex-tui; " + CodexCLIVersionPlaceholder + ")"
	account := &auth.Account{DBID: 1, AccessToken: "fixture", CodexFingerprintMode: auth.CodexFingerprintModeSingleMachineMultiWindow,
		CustomHeaders: map[string]string{"User-Agent": ua, "Originator": "codex-tui", "Version": CodexCLIVersionPlaceholder, "X-Custom": "literal"}}
	original := account.GetCustomHeaders()
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		fmt.Fprint(w, `{}`)
	}))
	defer server.Close()
	priorResin := resinCfg.Load()
	t.Cleanup(func() { resinCfg.Store(priorResin) })
	SetResinConfig(&ResinConfig{BaseURL: server.URL, PlatformName: "test"})
	clientPool.Delete("resin|1")
	device := &DeviceProfileConfig{StabilizeDeviceProfile: true}
	downstream := http.Header{
		"User-Agent": {"Codex Desktop/0.169.0-alpha.11.1 (Windows 10.0.26200; x86_64) unknown (Codex Desktop; 26.1111.21308)"},
		"Originator": {"Codex Desktop"},
	}
	for _, test := range []struct{ mode, synced, want string }{
		{ClientCompatModeForce, "0.169.0", "0.170.0"},
		{ClientCompatModeForce, "0.171.0", "0.171.0"},
		{ClientCompatModePreserve, "0.172.0", "0.172.0"},
	} {
		settings := old
		settings.ClientCompatMode, settings.CodexSyncedCLIVersion = test.mode, test.synced
		ApplyRuntimeSettings(settings)
		version := test.want
		wantUA := strings.ReplaceAll(ua, CodexCLIVersionPlaceholder, version)
		for _, compact := range []bool{false, true} {
			var response *http.Response
			var err error
			if compact {
				response, err = ExecuteCompactRequest(context.Background(), account, []byte(`{"input":[]}`), "session", "", "key", device, downstream)
			} else {
				response, err = ExecuteRequest(context.Background(), account, []byte(`{"input":[]}`), "session", "", "key", device, downstream, false)
			}
			if err != nil {
				t.Fatal(err)
			}
			response.Body.Close()
			if got.Get("User-Agent") != wantUA || got.Get("Version") != version || got.Get("Originator") != "codex-tui" || got.Get("X-Custom") != "literal" {
				t.Fatalf("mode=%s synced=%s compact=%v: wrong wire identity: %v", test.mode, test.synced, compact, got)
			}
		}
		maintenance, err := ResolveCodexMaintenanceIdentity(account, http.Header{"User-Agent": {"different-client/1"}})
		if err != nil || maintenance.UserAgent != wantUA || maintenance.Version != version || maintenance.Originator != "codex-tui" {
			t.Fatalf("maintenance identity: %+v, %v", maintenance, err)
		}
	}
	if !reflect.DeepEqual(original, account.GetCustomHeaders()) {
		t.Fatal("stored identity template was mutated")
	}
	if configuredCodexInstallationID(account) != configuredCodexInstallationID(&auth.Account{DBID: 2}) {
		t.Fatal("shared installation changed")
	}
}

func TestPinnedAccountVersionPreservesLiteralsAndUsesSafeFallback(t *testing.T) {
	old := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(old) })
	account := &auth.Account{CustomHeaders: map[string]string{"User-Agent": "codex-tui/0.160.0", "Version": "0.160.0", "X-Custom": CodexCLIVersionPlaceholder}}
	for _, synced := range []string{"0.171.0", "invalid", "0.1.0", ""} {
		settings := old
		settings.CodexSyncedCLIVersion = synced
		ApplyRuntimeSettings(settings)
		if !reflect.DeepEqual(account.GetCustomHeaders(), CodexAccountCustomHeaders(account)) {
			t.Fatal("literal header changed")
		}
		template := &auth.Account{CustomHeaders: map[string]string{"Version": CodexCLIVersionPlaceholder}}
		want := latestCodexCLIVersion
		if synced == "0.171.0" {
			want = synced
		}
		if got := CodexAccountCustomHeaders(template)["Version"]; got != want {
			t.Fatalf("synced=%q: got %s, want %s", synced, got, want)
		}
	}
	if CodexAccountCustomHeaders(nil) != nil {
		t.Fatal("nil account")
	}
}
