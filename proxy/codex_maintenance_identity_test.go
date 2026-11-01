package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tidwall/gjson"

	"github.com/codex2api/auth"
)

const testDesktopUserAgentConfig = `{"client_kind":"codex-desktop","os_name":"Mac OS","arch":"arm64"}`

// applyMaintenanceIdentitySettings 固定版本目录后改写运行时设置，测试结束自动恢复。
func applyMaintenanceIdentitySettings(t *testing.T, mutate func(*RuntimeSettings)) {
	t.Helper()
	codexTestIdentityCache(t)
	settings := CurrentRuntimeSettings()
	mutate(&settings)
	ApplyRuntimeSettings(settings)
}

func chatOutboundIdentityHeaders(t *testing.T, account *auth.Account) http.Header {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "https://example.com/backend-api/codex/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyCodexRequestHeaders(req, account, "token", "", "", nil, http.Header{}); err != nil {
		t.Fatalf("applyCodexRequestHeaders: %v", err)
	}
	return req.Header
}

func TestCodexMaintenanceIdentityMatchesChatOutbound(t *testing.T) {
	cases := []struct {
		name   string
		mode   string
		config string
	}{
		{"single desktop persona", ClientCompatModePreserve, testDesktopUserAgentConfig},
		{"pool personas per account", ClientCompatModePreserve, `{"mode":"pool","pool_mix":{"codex-desktop":50,"codex-tui":50}}`},
		{"force without config uses per-account profile", ClientCompatModeForce, "{}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			applyMaintenanceIdentitySettings(t, func(s *RuntimeSettings) {
				s.ClientCompatMode = tc.mode
				s.CodexUserAgentConfig = tc.config
			})
			for id := int64(1); id <= 12; id++ {
				account := &auth.Account{DBID: id}
				identity := mustResolveCodexMaintenanceIdentity(t, account, nil)
				chat := chatOutboundIdentityHeaders(t, account)
				if identity.UserAgent != chat.Get("User-Agent") || identity.Originator != chat.Get("Originator") || identity.Version != chat.Get("Version") {
					t.Fatalf("account %d: maintenance %+v, chat UA=%q Originator=%q Version=%q",
						id, identity, chat.Get("User-Agent"), chat.Get("Originator"), chat.Get("Version"))
				}
			}
		})
	}
}

func TestCodexMaintenanceIdentityDesktopOriginatorFollowsUserAgent(t *testing.T) {
	applyMaintenanceIdentitySettings(t, func(s *RuntimeSettings) {
		s.CodexUserAgentConfig = testDesktopUserAgentConfig
	})
	identity := mustResolveCodexMaintenanceIdentity(t, &auth.Account{DBID: 3}, nil)
	if !strings.HasPrefix(identity.UserAgent, "Codex Desktop/"+identity.Version+" ") || identity.Originator != "Codex Desktop" {
		t.Fatalf("desktop persona must keep UA prefix, Version and Originator aligned: %+v", identity)
	}
}

func TestCodexMaintenanceIdentityAppliesAccountIdentityHeaders(t *testing.T) {
	applyMaintenanceIdentitySettings(t, func(s *RuntimeSettings) {
		s.CodexUserAgentConfig = testDesktopUserAgentConfig
	})
	account := &auth.Account{DBID: 5, CustomHeaders: map[string]string{
		"user-agent": "codex_vscode/0.150.0 (Windows 10.0.26100; x86_64) unknown (codex_vscode; 0.150.0)",
		"Originator": "codex_vscode",
		"Version":    "0.150.0",
	}}
	identity := mustResolveCodexMaintenanceIdentity(t, account, nil)
	chat := chatOutboundIdentityHeaders(t, account)
	if identity.UserAgent != chat.Get("User-Agent") || identity.Originator != chat.Get("Originator") || identity.Version != chat.Get("Version") {
		t.Fatalf("pinned account headers must win like on chat: maintenance %+v, chat UA=%q Originator=%q Version=%q",
			identity, chat.Get("User-Agent"), chat.Get("Originator"), chat.Get("Version"))
	}
	if identity.Originator != "codex_vscode" || identity.Version != "0.150.0" {
		t.Fatalf("custom identity headers not applied: %+v", identity)
	}
}

func TestCodexMaintenanceIdentityRejectsUnavailableVersion(t *testing.T) {
	applyMaintenanceIdentitySettings(t, func(s *RuntimeSettings) {
		s.ClientCompatMode = ClientCompatModeAuto
		s.CodexMinCLIVersion = "99.0.0"
		s.CodexUserAgentConfig = "{}"
	})
	if _, err := ResolveCodexMaintenanceIdentity(&auth.Account{DBID: 1}, nil); err == nil {
		t.Fatal("unresolvable identity must return an error")
	}
}

func TestQueryWhamUsageUsesUnifiedIdentity(t *testing.T) {
	applyMaintenanceIdentitySettings(t, func(s *RuntimeSettings) {
		s.CodexUserAgentConfig = testDesktopUserAgentConfig
	})
	var got http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"plan_type": "plus", "rate_limit": {}}`))
	}))
	defer server.Close()

	account := &auth.Account{DBID: 9, AccessToken: "at-123", AccountID: "acc-1"}
	if _, _, err := queryWhamUsageWithURL(context.Background(), account, "", server.URL); err != nil {
		t.Fatalf("queryWhamUsageWithURL: %v", err)
	}
	want := mustResolveCodexMaintenanceIdentity(t, account, nil)
	if got.Get("User-Agent") != want.UserAgent || got.Get("Originator") != "Codex Desktop" {
		t.Fatalf("wham probe identity UA=%q Originator=%q, want %+v", got.Get("User-Agent"), got.Get("Originator"), want)
	}
	// 维护请求不凭空补发真实客户端不会带的身份头。
	if got.Get("Version") != "" || got.Get(codexInstallationIDHeader) != "" || got.Get("X-Oai-Attestation") != "" {
		t.Fatalf("wham probe must not add Version/installation headers: Version=%q installation=%q", got.Get("Version"), got.Get(codexInstallationIDHeader))
	}
}

func TestQueryChatGPTSubscriptionUsesUnifiedIdentity(t *testing.T) {
	applyMaintenanceIdentitySettings(t, func(s *RuntimeSettings) {
		s.CodexUserAgentConfig = testDesktopUserAgentConfig
	})
	var gotUA, gotOriginator string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA, gotOriginator = r.Header.Get("User-Agent"), r.Header.Get("Originator")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"plan_type":"plus","active_until":"2026-07-29T14:54:57Z"}`))
	}))
	defer server.Close()
	defer SetSubscriptionsURLForTest(server.URL)()

	account := &auth.Account{DBID: 4, AccessToken: "test-at", AccountID: testWorkspaceUUID}
	if _, err := QueryChatGPTSubscription(context.Background(), account, ""); err != nil {
		t.Fatalf("QueryChatGPTSubscription: %v", err)
	}
	if !strings.HasPrefix(gotUA, "Codex Desktop/") || gotOriginator != "Codex Desktop" {
		t.Fatalf("subscription probe UA=%q Originator=%q, want configured desktop identity", gotUA, gotOriginator)
	}
}

func TestFetchCodexModelsManifestUnifiedIdentityAlignsClientVersion(t *testing.T) {
	applyMaintenanceIdentitySettings(t, func(s *RuntimeSettings) {
		s.CodexUserAgentConfig = testDesktopUserAgentConfig
	})
	var gotUA, gotOriginator, gotVersion, gotClientVersion string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA, gotOriginator, gotVersion = r.Header.Get("User-Agent"), r.Header.Get("Originator"), r.Header.Get("Version")
		gotClientVersion = r.URL.Query().Get("client_version")
		_, _ = w.Write([]byte(`{"models":[]}`))
	}))
	defer server.Close()
	account := &auth.Account{DBID: 2, AccessToken: "at-123"}

	t.Run("background fetch uses configured persona", func(t *testing.T) {
		if _, err := fetchCodexModelsManifestWithURL(context.Background(), account, "", server.URL, "0.140.0", "", nil); err != nil {
			t.Fatalf("fetch: %v", err)
		}
		want := mustResolveCodexMaintenanceIdentity(t, account, nil)
		if gotUA != want.UserAgent || gotOriginator != "Codex Desktop" || gotVersion != want.Version || gotClientVersion != want.Version {
			t.Fatalf("UA=%q Originator=%q Version=%q client_version=%q, want %+v", gotUA, gotOriginator, gotVersion, gotClientVersion, want)
		}
	})

	t.Run("passthrough of official client follows chat rules", func(t *testing.T) {
		downstream := http.Header{}
		downstream.Set("User-Agent", "codex-tui/0.160.0 (Mac OS 15.5.0; arm64) xterm-256color (codex-tui; 0.160.0)")
		downstream.Set("Originator", "codex-tui")
		if _, err := fetchCodexModelsManifestWithURL(context.Background(), account, "", server.URL, "0.160.0", "", downstream); err != nil {
			t.Fatalf("fetch: %v", err)
		}
		if gotUA != downstream.Get("User-Agent") || gotOriginator != "codex-tui" || gotVersion != "0.160.0" || gotClientVersion != "0.160.0" {
			t.Fatalf("UA=%q Originator=%q Version=%q client_version=%q", gotUA, gotOriginator, gotVersion, gotClientVersion)
		}
	})
}

func TestCodexModelDiscoveryUnifiedIdentityMatchesRelayChat(t *testing.T) {
	applyMaintenanceIdentitySettings(t, func(s *RuntimeSettings) {
		s.CodexUserAgentConfig = testDesktopUserAgentConfig
	})
	const baseURL, apiKey = "https://relay.example.com/v1", "sk-relay"
	account := &auth.Account{DBID: 7, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: baseURL, APIKey: apiKey}

	discovery := http.Header{}
	ApplyCodexModelDiscoveryHeaders(discovery, account, baseURL, apiKey)

	req, err := http.NewRequest(http.MethodPost, baseURL+"/responses", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyOpenAIResponsesRequestHeaders(req, account, apiKey, nil); err != nil {
		t.Fatalf("applyOpenAIResponsesRequestHeaders: %v", err)
	}
	for _, name := range []string{"User-Agent", "Originator", "Version", "x-codex-app-version"} {
		if discovery.Get(name) == "" || discovery.Get(name) != req.Header.Get(name) {
			t.Fatalf("%s: discovery %q, relay chat %q", name, discovery.Get(name), req.Header.Get(name))
		}
	}

	installationID := discovery.Get(codexInstallationIDHeader)
	for _, downstreamKey := range []string{"Bearer sk-downstream-1", "Bearer sk-downstream-2"} {
		body, injected := ensureCodexClientInstallationMetadata([]byte(`{"model":"gpt-5.6"}`), account, http.Header{"Authorization": {downstreamKey}})
		if !injected {
			t.Fatal("installation metadata was not injected")
		}
		if got := gjson.GetBytes(body, "client_metadata.x-codex-installation-id").String(); got != installationID {
			t.Fatalf("downstream %s: body installation id %q, discovery header %q", downstreamKey, got, installationID)
		}
	}
}

func mustResolveCodexMaintenanceIdentity(t *testing.T, account *auth.Account, headers http.Header) CodexMaintenanceIdentity {
	t.Helper()
	identity, err := ResolveCodexMaintenanceIdentity(account, headers)
	if err != nil {
		t.Fatal(err)
	}
	return identity
}
