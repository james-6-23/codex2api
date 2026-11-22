package proxy

import (
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"sync"
	"testing"

	"github.com/codex2api/auth"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const identityTimeFixture = "01a1e838-fc93-7ef0-b32b-c0c0c3466899"
const identityTimeFixtureMilli int64 = 1794897411219

func configureIdentityTimeForTest(t *testing.T, value string) {
	t.Helper()
	previous := CodexIdentityV2FromUnixMilli()
	t.Cleanup(func() { codexIdentityV2From.Store(previous) })
	t.Setenv(codexIdentityV2FromEnv, value)
	if err := ConfigureCodexIdentityTimeFromEnv(); err != nil {
		t.Fatal(err)
	}
}

func identityTimeMillis(t *testing.T, raw string) int64 {
	t.Helper()
	u := uuid.MustParse(raw)
	if u.Version() != 7 || u.Variant() != uuid.RFC4122 {
		t.Fatalf("not an RFC variant UUIDv7: %s", raw)
	}
	seconds, nanos := u.Time().UnixTime()
	return seconds*1000 + nanos/1_000_000
}

func TestCodexIdentityTimeConfiguration(t *testing.T) {
	configureIdentityTimeForTest(t, "")
	for _, value := range []string{"", "0", " 1794897411219 ", "281474976710655"} {
		t.Setenv(codexIdentityV2FromEnv, value)
		if err := ConfigureCodexIdentityTimeFromEnv(); err != nil {
			t.Fatalf("valid value %q rejected: %v", value, err)
		}
	}
	previous := CodexIdentityV2FromUnixMilli()
	for _, value := range []string{"tomorrow", "-1", "281474976710656", "9223372036854775808", "1794897411219.5"} {
		t.Setenv(codexIdentityV2FromEnv, value)
		if err := ConfigureCodexIdentityTimeFromEnv(); err == nil {
			t.Fatalf("invalid cutoff %q accepted", value)
		}
		if CodexIdentityV2FromUnixMilli() != previous {
			t.Fatal("invalid configuration changed effective mapping")
		}
	}
}

func TestCodexIdentityTimeLegacyGoldenAndBoundary(t *testing.T) {
	// Captured from the pre-upgrade account C wire probe, not calculated by
	// the new implementation. This existing conversation must keep its ID.
	const legacy = "01947252-285f-706c-b1fc-de83a71b52aa"
	configureIdentityTimeForTest(t, "")
	if got := singleMachineIdentity(4, identityTimeFixture); got != legacy {
		t.Fatalf("disabled mapping = %s, want captured %s", got, legacy)
	}
	configureIdentityTimeForTest(t, strconv.FormatInt(identityTimeFixtureMilli+1, 10))
	if got := singleMachineIdentity(4, identityTimeFixture); got != legacy {
		t.Fatalf("pre-cutover conversation changed: %s", got)
	}
	configureIdentityTimeForTest(t, strconv.FormatInt(identityTimeFixtureMilli, 10))
	got := singleMachineIdentity(4, identityTimeFixture)
	if got == legacy || got == identityTimeFixture || identityTimeMillis(t, got) != identityTimeFixtureMilli {
		t.Fatalf("cutover millisecond not time-preserving and isolated: %s", got)
	}
	// Reloading the same persistent setting cannot move an existing identity.
	if err := ConfigureCodexIdentityTimeFromEnv(); err != nil {
		t.Fatal(err)
	}
	if singleMachineIdentity(4, identityTimeFixture) != got {
		t.Fatal("identity changed after configuration reload")
	}
}

func TestCodexIdentityTimeOpaqueAndSharedHelpersStayLegacy(t *testing.T) {
	configureIdentityTimeForTest(t, "1")
	badVariant := uuid.MustParse(identityTimeFixture)
	badVariant[8] = 0x01
	for _, original := range []string{"ses_opaque", "", "00000000-0000-4000-8000-000000000001", badVariant.String()} {
		seed := fmt.Sprintf("api-key:9:%s", original)
		if got := deriveCodexIdentityUUIDv7(seed, original); got != DeriveStableSessionUUIDv7(seed) {
			t.Fatalf("opaque/non-v7 identity changed: %q", original)
		}
	}
	// Key-only fallbacks and the helper shared with Claude retain old behavior.
	for _, seed := range []string{"codex2api:prompt-cache:secret", "codex2api:claude-session:" + identityTimeFixture} {
		if got := DeriveStableSessionUUIDv7(seed); identityTimeMillis(t, got) >= identityTimeFixtureMilli {
			t.Fatalf("legacy shared helper unexpectedly acquired request time: %s", got)
		}
	}
	if IsolateCodexSessionID(0, identityTimeFixture) != identityTimeFixture || IsolateCodexSessionID(9, "") != "" {
		t.Fatal("empty/unscoped session contract changed")
	}
}

func TestCodexIdentityTimeAccountAndAPIKeyIsolationConcurrent(t *testing.T) {
	configureIdentityTimeForTest(t, "1")
	var wg sync.WaitGroup
	for accountID := int64(1); accountID <= 4; accountID++ {
		want := singleMachineIdentity(accountID, identityTimeFixture)
		if identityTimeMillis(t, want) != identityTimeFixtureMilli {
			t.Fatal("source time changed")
		}
		for other := int64(1); other < accountID; other++ {
			if singleMachineIdentity(other, identityTimeFixture) == want {
				t.Fatal("different accounts share identity")
			}
		}
		for i := 0; i < 16; i++ {
			wg.Go(func() {
				if got := singleMachineIdentity(accountID, identityTimeFixture); got != want {
					t.Errorf("concurrent identity drift: %s != %s", got, want)
				}
			})
		}
	}
	wg.Wait()
	first := IsolateCodexSessionID(11, identityTimeFixture)
	if identityTimeMillis(t, first) != identityTimeFixtureMilli || first == IsolateCodexSessionID(12, identityTimeFixture) {
		t.Fatal("explicit cache identity lost time or API-key isolation")
	}
}

func TestCodexIdentityTimeMixedGenerationTopologyAndCarriers(t *testing.T) {
	configureIdentityTimeForTest(t, strconv.FormatInt(identityTimeFixtureMilli, 10))
	t.Setenv("CODEX_SESSION_HEADER_MODE", "native")
	a := fingerprintAccount(t, auth.CodexFingerprintModeSingleMachineMultiWindow)
	parent := deriveStableCodexUUIDv7("parent-fixture", identityTimeFixtureMilli-1)
	contextID := deriveStableCodexUUIDv7("context-fixture", identityTimeFixtureMilli+20)
	metadata := fmt.Sprintf(`{"installation_id":"old-device","session_id":%q,"thread_id":%q,"parent_thread_id":%q,"forked_from_thread_id":%q,"context_window_id":%q,"turn_id":"turn-untouched","turn_started_at_unix_ms":123}`, parent, identityTimeFixture, parent, parent, contextID)
	headers := codexClientHeaders(metadata, parent)
	headers.Set(codexThreadIDHeader, identityTimeFixture)
	headers.Set(codexParentThreadIDHeader, parent)
	body := []byte(`{"prompt_cache_key":"custom-cache","previous_response_id":"resp_owned","client_metadata":` + metadata + `}`)
	body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata", metadata)
	originalHeaders := headers.Clone()
	outBody := ApplyCodexFingerprintToBody(body, a, headers)
	req, _ := http.NewRequest(http.MethodPost, "https://example.test/responses", nil)
	applyCodexRequestHeaders(req, a, "token", "custom-cache", "key", nil, headers)
	outMetadata := gjson.Parse(req.Header.Get(codexTurnMetadataHeader))
	for _, field := range []string{"session_id", "thread_id", "parent_thread_id", "forked_from_thread_id", "context_window_id", "installation_id"} {
		if outMetadata.Get(field).String() != gjson.GetBytes(outBody, "client_metadata."+field).String() {
			t.Fatalf("header/body disagree for %s", field)
		}
	}
	parentSeed := fmt.Sprintf("codex2api:single-machine-identity:v1:%d:%s", a.ID(), parent)
	legacyParent := DeriveStableSessionUUIDv7(parentSeed)
	if outMetadata.Get("parent_thread_id").String() != legacyParent || outMetadata.Get("session_id").String() != legacyParent || req.Header.Get(codexParentThreadIDHeader) != legacyParent {
		t.Fatal("old parent/new child lost legacy parent reference")
	}
	if identityTimeMillis(t, outMetadata.Get("thread_id").String()) != identityTimeFixtureMilli || identityTimeMillis(t, outMetadata.Get("context_window_id").String()) != identityTimeFixtureMilli+20 {
		t.Fatal("new child/context lost their individual creation time")
	}
	if outMetadata.Get("turn_id").String() != "turn-untouched" || outMetadata.Get("turn_started_at_unix_ms").Int() != 123 || gjson.GetBytes(outBody, "prompt_cache_key").String() != "custom-cache" || gjson.GetBytes(outBody, "previous_response_id").String() != "resp_owned" {
		t.Fatal("unrelated turn/cache/continuation fields changed")
	}
	if !reflect.DeepEqual(headers, originalHeaders) {
		t.Fatal("caller headers mutated")
	}
	if gjson.GetBytes(outBody, "client_metadata.x-codex-turn-metadata").String() != outMetadata.Raw {
		t.Fatal("embedded metadata differs from header")
	}
	// Earlier fingerprint modes retain their existing context identity policy.
	ids := &codexFingerprintIDs{accountID: a.ID(), mode: auth.CodexFingerprintModeSession}
	if convergeFingerprintLineageValue(ids, "context_window_id", contextID) != convergeCodexLineageValue(a.ID(), "context_window_id", contextID) {
		t.Fatal("legacy fingerprint mode changed")
	}
}
