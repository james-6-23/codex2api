package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy/attestation"
	"github.com/gin-gonic/gin"
)

func codexTestIdentityCache(t *testing.T) {
	t.Helper()
	old, settings := codexClientVersions.Load(), CurrentRuntimeSettings()
	t.Cleanup(func() { codexClientVersions.Store(old); ApplyRuntimeSettings(settings) })
	target := CodexClientVersionTarget{ClientKind: string(CodexClientKindDesktop), TargetPlatform: "win32-x64", Status: "verified", Pairs: []CodexClientVersionPair{
		{AppVersion: "26.928.31416", CLIVersion: "0.158.0-alpha.2.1", Source: "third_party_windows_mapping"},
		{AppVersion: "26.927.1", CLIVersion: "0.159.2", Source: "third_party_windows_mapping"},
	}}
	mac := CodexClientVersionTarget{ClientKind: string(CodexClientKindDesktop), TargetPlatform: "darwin-arm64", Status: "verified", Pairs: []CodexClientVersionPair{{AppVersion: "26.928.31416", CLIVersion: "0.158.1-alpha.1", Source: "official_appcast"}}}
	codexClientVersions.Store(&codexClientVersionSnapshot{targets: map[string]CodexClientVersionTarget{codexClientVersionKey(target.ClientKind, target.TargetPlatform): target, codexClientVersionKey(mac.ClientKind, mac.TargetPlatform): mac}})
}

func TestCodexClientIdentitySelectsCompletePairAndManualOverrides(t *testing.T) {
	codexTestIdentityCache(t)
	spec, _ := codexUAKindSpecFor(CodexClientKindDesktop)
	cases := []struct {
		name        string
		choice      codexVersionSelection
		cli         string
		app         string
		unavailable bool
	}{
		{"newest app with alpha", codexVersionSelection{}, "0.158.0-alpha.2.1", "26.928.31416", false},
		{"stable floor excludes alpha", codexVersionSelection{VersionFloor: "0.158.0"}, "0.159.2", "26.927.1", false},
		{"unavailable floor", codexVersionSelection{VersionFloor: "0.160.0"}, "", "", true},
		{"CLI override keeps default app", codexVersionSelection{CLIOverride: "0.140.0", VersionFloor: "0.999.0"}, "0.140.0", "26.928.31416", false},
		{"app override leaves auto CLI floor", codexVersionSelection{AppOverride: "99.1.1", VersionFloor: "0.158.0"}, "0.159.2", "99.1.1", false},
		{"both overrides", codexVersionSelection{CLIOverride: "0.140.0-alpha.1", AppOverride: "99.1.1", VersionFloor: "9.999.0"}, "0.140.0-alpha.1", "99.1.1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.choice.OSName, tc.choice.Arch = "Windows", "x86_64"
			cli, app, err := resolveCodexCurrentVersions(spec, tc.choice)
			if cli != tc.cli || app != tc.app || (err != nil) != tc.unavailable {
				t.Fatalf("pair: %s / %s / %v", cli, app, err)
			}
		})
	}
}

func TestCodexClientIdentityPreviewMatchesOutboundAndArchitecture(t *testing.T) {
	codexTestIdentityCache(t)
	settings := CurrentRuntimeSettings()
	settings.ClientCompatMode, settings.CodexMinCLIVersion, settings.CodexSyncedCLIVersion = ClientCompatModeForce, "0.999.0", "0.200.0"
	for _, cfg := range []string{`{"client_kind":"codex-desktop","os_name":"Mac OS","arch":"arm64"}`, `{"client_kind":"codex-desktop","os_name":"Windows","arch":"x86_64"}`} {
		settings.CodexUserAgentConfig = cfg
		ApplyRuntimeSettings(settings)
		identity, err := ResolveCodexOutboundClientIdentity(CodexClientIdentityInput{})
		if err != nil {
			t.Fatal(err)
		}
		preview, err := PreviewCodexUserAgentConfig(cfg, "", nil)
		if err != nil || identity.Version != preview.Persona.Version || identity.UserAgent != preview.Persona.UserAgent || preview.Persona.AppVersion != "26.928.31416" {
			t.Fatalf("preview mismatch: %+v / %+v / %v", identity, preview, err)
		}
		if strings.Contains(cfg, "Mac OS") && identity.Version != "0.158.1-alpha.1" {
			t.Fatalf("wrong architecture: %+v", identity)
		}
	}
}

func TestCodexClientIdentityUnavailableStopsHTTPBeforeUpstream(t *testing.T) {
	codexTestIdentityCache(t)
	ApplyRuntimeSettings(RuntimeSettings{ClientCompatMode: ClientCompatModeAuto, CodexMinCLIVersion: "0.999.0", CodexUserAgentConfig: `{"client_kind":"codex-desktop"}`})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(http.StatusOK) }))
	defer server.Close()
	account := &auth.Account{DBID: 1, AccessToken: "test-token", AccountID: "test-account"}
	_, err := ExecuteRequest(context.Background(), account, []byte(`{"model":"gpt-5.4","input":[]}`), "session", server.URL, "", nil, nil, false)
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code != ErrorCodeCodexClientVersionUnavailable || apiErr.HTTPStatus != http.StatusServiceUnavailable || apiErr.Retryable {
		t.Fatalf("error: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("upstream called %d times", calls.Load())
	}
	policy := database.ContinuousRetryPolicy{Enabled: true, CatchAll: true, StatusCodes: []int{503}}
	if isRetryableRequestErrorForContext(context.Background(), err, policy) {
		t.Fatal("local version error retried")
	}
	if _, err := PreviewCodexUserAgentConfig(`{"client_kind":"codex-desktop"}`, "0.999.0", nil); err == nil {
		t.Fatal("preview hid unavailable version")
	}
}

func TestCodexClientIdentityAutoComparesFullPrerelease(t *testing.T) {
	settings := RuntimeSettings{ClientCompatMode: ClientCompatModeAuto, CodexMinCLIVersion: "0.158.0"}
	ua := "codex-tui/0.158.0-alpha.2.1 (Linux Unknown; x86_64) unknown"
	if !shouldGenerateCodexClientHeaders(settings, ua, "codex-tui") {
		t.Fatal("alpha incorrectly passed stable floor")
	}
	profile := deviceProfile{UserAgent: ua, HasVersion: true}
	if codexVersionFromProfile(profile, "0.158.0") != "0.158.0-alpha.2.1" {
		t.Fatal("profile truncated alpha")
	}
}

func TestCodexClientIdentityUnavailablePropagatesThroughResponsesRelay(t *testing.T) {
	codexTestIdentityCache(t)
	ApplyRuntimeSettings(RuntimeSettings{ClientCompatMode: ClientCompatModeAuto, CodexMinCLIVersion: "0.999.0", CodexUserAgentConfig: `{"client_kind":"codex-desktop"}`})
	request := httptest.NewRequest(http.MethodPost, "http://example.test/v1/responses", nil)
	err := applyOpenAIResponsesRequestHeaders(request, &auth.Account{DBID: 1}, "api-key", nil)
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code != ErrorCodeCodexClientVersionUnavailable {
		t.Fatalf("HTTP relay hid error: %v", err)
	}
	_, err = openAIResponsesWebsocketHeadersChecked(openAIResponsesWSHeaderInput{ctx: context.Background(), account: &auth.Account{DBID: 1}, apiKey: "api-key", endpoint: "http://example.test/v1/responses"})
	if !errors.As(err, &apiErr) || apiErr.Code != ErrorCodeCodexClientVersionUnavailable {
		t.Fatalf("WS relay hid error: %v", err)
	}
}

func TestCodexClientIdentityUnavailableStopsLiveSideband(t *testing.T) {
	codexTestIdentityCache(t)
	ApplyRuntimeSettings(RuntimeSettings{ClientCompatMode: ClientCompatModeAuto, CodexMinCLIVersion: "0.999.0", CodexUserAgentConfig: `{"client_kind":"codex-desktop"}`})
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
	account := &auth.Account{DBID: 11, AccessToken: "at"}
	store.AddAccount(account)
	handler := NewHandler(store, nil, &config.Config{AdminSecret: "secret"}, nil)
	cipher, err := encryptLiveAttestation(`{"v":1}`, "secret")
	if err != nil {
		t.Fatal(err)
	}
	record := &liveCallRecord{CallID: "rtc_identity", CallHash: hashLiveCallID("rtc_identity"), AccountID: account.ID(), AttestationCiphertext: cipher}
	headers, err := handler.liveSidebandHeaders(context.Background(), record)
	var apiErr *Error
	if !errors.As(err, &apiErr) || apiErr.Code != ErrorCodeCodexClientVersionUnavailable || headers != nil {
		t.Fatalf("sideband built partial headers: %v / %v", headers, err)
	}
}

func TestCodexAlphaSearchKeepsVersionUnavailableStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)
	codexTestIdentityCache(t)
	ApplyRuntimeSettings(RuntimeSettings{ClientCompatMode: ClientCompatModeAuto, CodexMinCLIVersion: "0.999.0", CodexUserAgentConfig: `{"client_kind":"codex-desktop"}`})
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer upstream.Close()
	codexAlphaSearchURLForTest = upstream.URL
	defer func() { codexAlphaSearchURLForTest = "" }()
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "at-search", AccountID: "acc-s", PlanType: "plus"})
	handler := NewHandler(store, nil, nil, nil)
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/alpha/search", strings.NewReader(`{"commands":{"search_query":[{"q":"go"}]}}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.CodexAlphaSearchHandler(ctx)
	if rec.Code != http.StatusServiceUnavailable || calls.Load() != 0 {
		t.Fatalf("status=%d upstream calls=%d body=%s", rec.Code, calls.Load(), rec.Body.String())
	}
}

func TestSyncCodexClientVersionsSharedRunSurvivesFirstCallerCancel(t *testing.T) {
	previous := codexClientVersionSyncRun
	t.Cleanup(func() { codexClientVersionSyncRun = previous })
	started, release := make(chan struct{}), make(chan struct{})
	var runErr atomic.Value
	var startOnce sync.Once
	codexClientVersionSyncRun = func(ctx context.Context, _ codexClientSyncKey) (*CodexClientVersionsSyncResult, error) {
		startOnce.Do(func() { close(started) })
		<-release
		runErr.Store(fmt.Sprint(ctx.Err()))
		return &CodexClientVersionsSyncResult{}, nil
	}
	db := new(database.DB)
	firstCtx, cancelFirst := context.WithCancel(context.Background())
	firstDone := make(chan error, 1)
	go func() { _, err := SyncCodexClientVersions(firstCtx, db, ""); firstDone <- err }()
	<-started
	cancelFirst()
	// 发起者取消后应立即返回，不必等共享同步跑完。
	if err := <-firstDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("first caller err = %v, want canceled", err)
	}
	waiterDone := make(chan error, 1)
	go func() { _, err := SyncCodexClientVersions(context.Background(), db, ""); waiterDone <- err }()
	close(release)
	if err := <-waiterDone; err != nil {
		t.Fatalf("waiting caller err = %v", err)
	}
	if got := runErr.Load(); got != "<nil>" {
		t.Fatalf("shared run ctx err = %v", got)
	}
}

func TestLiveAttestationIgnoresSuppliedHeaders(t *testing.T) {
	handler := NewHandler(nil, nil, &config.Config{AdminSecret: "secret"}, nil)
	req, _ := http.NewRequest(http.MethodPost, "https://example.com/v1/live", nil)
	err := handler.applyLiveUpstreamHeaders(req, liveUpstreamHeaderInput{
		Account: &auth.Account{DBID: 42, CustomHeaders: map[string]string{liveAttestationHeader: "account-proof"}},
		Downstream: http.Header{
			"User-Agent":          {"codex-tui/0.164.0 (Windows 11; x86_64) unknown"},
			liveAttestationHeader: {"client-proof"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	proof := req.Header.Get(liveAttestationHeader)
	if !strings.HasPrefix(proof, `{"v":1,"s":0,"t":"v1.`) {
		t.Fatalf("expected project-generated proof, got %q", proof)
	}
	cipher, err := encryptLiveAttestation(proof, "secret")
	if err != nil {
		t.Fatal(err)
	}
	got, err := decryptLiveAttestation(cipher, "secret")
	if err != nil || proof != got {
		t.Fatalf("decrypt=%q err=%v", got, err)
	}
}

func TestLiveAttestationLinuxPreservesAbsence(t *testing.T) {
	handler := NewHandler(nil, nil, &config.Config{AdminSecret: "secret"}, nil)
	req, _ := http.NewRequest(http.MethodPost, "https://example.com/v1/live", nil)
	err := handler.applyLiveUpstreamHeaders(req, liveUpstreamHeaderInput{
		Account: &auth.Account{DBID: 42, CustomHeaders: map[string]string{liveAttestationHeader: "account-proof"}},
		Downstream: http.Header{
			"User-Agent":          {"codex-tui/0.164.0 (Linux Unknown; x86_64) unknown"},
			liveAttestationHeader: {"client-proof"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := encryptLiveAttestation(req.Header.Get(liveAttestationHeader), "secret")
	if err != nil {
		t.Fatal(err)
	}
	got, err := handler.decryptLiveAttestation(&liveCallRecord{AttestationCiphertext: cipher})
	if err != nil || "" != got {
		t.Fatalf("decrypt=%q err=%v", got, err)
	}
}

func TestLiveAttestationMacFailureUsesToken(t *testing.T) {
	previous := codexAttestationService
	t.Cleanup(func() { codexAttestationService = previous })
	codexAttestationService = attestation.NewService(func(context.Context) (attestation.NativeResult, error) {
		return attestation.NativeResult{}, errors.New("native runtime unavailable")
	})
	handler := NewHandler(nil, nil, nil, nil)
	req, _ := http.NewRequest(http.MethodPost, "https://example.com/v1/live", nil)
	err := handler.applyLiveUpstreamHeaders(req, liveUpstreamHeaderInput{
		Account: &auth.Account{DBID: 42, CustomHeaders: map[string]string{liveAttestationHeader: "account-proof"}},
		Downstream: http.Header{
			"User-Agent":          {"codex-tui/0.164.0 (Mac OS 15.5.0; arm64) unknown"},
			liveAttestationHeader: {"client-proof"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var envelope struct {
		Version int    `json:"v"`
		Status  int    `json:"s"`
		Token   string `json:"t"`
	}
	if err := json.Unmarshal([]byte(req.Header.Get(liveAttestationHeader)), &envelope); err != nil {
		t.Fatal(err)
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(envelope.Token, "v1."))
	if err != nil || envelope.Version != 1 || envelope.Status != 0 || !bytes.Contains(payload, append([]byte("error_code"), byte(attestation.GenerationFailed))) {
		t.Fatalf("missing DeviceCheck failure state: %q / %v", payload, err)
	}
}
