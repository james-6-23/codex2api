package proxy

import (
	"net/http"
	"testing"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

func TestCodexDeviceIdentityUsesPersistedValueAcrossModes(t *testing.T) {
	const installationID = "11111111-aaaa-4111-8111-111111111111"
	account := &auth.Account{DBID: 1536, CodexInstallationID: installationID}
	for _, mode := range []string{auth.CodexFingerprintModeDevice, auth.CodexFingerprintModeOff, auth.CodexFingerprintModeSession, auth.CodexFingerprintModeFull, auth.CodexFingerprintModeDevice} {
		account.CodexFingerprintMode = mode
		account.AccountID = "upstream-identity-arrived-later"
		account.AccessToken = "refreshed-token"
		ids := resolveCodexFingerprintIDs(account, nil)
		if mode == auth.CodexFingerprintModeOff {
			if ids != nil {
				t.Fatal("off mode must not converge")
			}
		} else if ids == nil || ids.installationID != installationID {
			t.Fatalf("mode %q: installation identity = %+v", mode, ids)
		}
		if account.EffectiveCodexInstallationID() != installationID {
			t.Fatal("retained installation ID changed")
		}
	}
	other := &auth.Account{DBID: 1536, CodexInstallationID: "22222222-bbbb-4222-8222-222222222222", CodexFingerprintMode: auth.CodexFingerprintModeDevice}
	if resolveCodexFingerprintIDs(other, nil).installationID == installationID {
		t.Fatal("same database ID in another deployment reused the installation ID")
	}
}

func TestCodexDeviceIdentityAlignsHeaderBodyAndCustomOverride(t *testing.T) {
	account := &auth.Account{
		DBID: 1536, CodexInstallationID: "11111111-aaaa-4111-8111-111111111111",
		CodexFingerprintMode: auth.CodexFingerprintModeDevice,
	}
	const metadata = `{"installation_id":"client-device","session_id":"client-session"}`
	downstream := codexClientHeaders(metadata, "client-session")
	downstream.Set(codexInstallationIDHeader, "client-device")
	for _, custom := range []string{"", "pinned-device", ""} {
		account.CustomHeaders = map[string]string{"x-codex-installation-id": custom}
		want := account.EffectiveCodexInstallationID()
		outbound := http.Header{codexTurnMetadataHeader: {metadata}}
		ApplyCodexFingerprintHeaders(outbound, account, downstream)
		body := ApplyCodexFingerprintToBody([]byte(`{"client_metadata":{"x-codex-installation-id":"client-device","x-codex-turn-metadata":"{\"installation_id\":\"client-device\"}"}}`), account, downstream)
		for _, got := range []string{
			outbound.Get(codexInstallationIDHeader),
			gjson.Get(outbound.Get(codexTurnMetadataHeader), "installation_id").String(),
			gjson.GetBytes(body, "client_metadata.x-codex-installation-id").String(),
			gjson.Get(gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata").String(), "installation_id").String(),
		} {
			if got != want {
				t.Fatalf("installation ID = %q, want %q", got, want)
			}
		}
	}
}
