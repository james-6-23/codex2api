package auth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// resetConfiguredAntigravityOAuth 清空系统设置侧的 OAuth client 配置并在测试
// 结束时保持清空，避免包级 atomic 状态串到其他用例。
func resetConfiguredAntigravityOAuth(t *testing.T) {
	t.Helper()
	SetConfiguredAntigravityOAuth(AntigravityOAuthSettings{})
	t.Cleanup(func() { SetConfiguredAntigravityOAuth(AntigravityOAuthSettings{}) })
}

func TestAntigravityOAuthClientsFallBackToOfficialDesktopClient(t *testing.T) {
	t.Setenv(antigravityOAuthClientsEnv, "")
	t.Setenv(antigravityActiveOAuthClientEnv, "")
	resetConfiguredAntigravityOAuth(t)
	clients, active := effectiveAntigravityOAuthClients()
	if len(clients) != 1 || active != AntigravityDefaultOAuthClientKey {
		t.Fatalf("clients/active = %#v/%q, want built-in official", clients, active)
	}
	if clients[0].ClientID != AntigravityDefaultOAuthClientID || clients[0].ClientSecret != AntigravityDefaultOAuthClientSecret {
		t.Fatalf("built-in client = %#v", clients[0])
	}
	if !UsingBuiltinAntigravityOAuth() {
		t.Fatal("UsingBuiltinAntigravityOAuth() = false, want true when nothing is configured")
	}
	client := newAntigravityClient(http.DefaultClient, AntigravityEndpoints{})
	gotURL, info, err := client.BuildOAuthAuthorizationURL(
		"http://127.0.0.1:43123/oauth-callback", "state-123", "challenge-456", "",
	)
	if err != nil {
		t.Fatalf("BuildOAuthAuthorizationURL() error: %v", err)
	}
	if info.Key != AntigravityDefaultOAuthClientKey || info.ClientID != AntigravityDefaultOAuthClientID {
		t.Fatalf("info = %+v", info)
	}
	if !strings.Contains(gotURL, "client_id="+url.QueryEscape(AntigravityDefaultOAuthClientID)) {
		t.Fatalf("authorization URL missing official client_id: %s", gotURL)
	}
}

func TestAntigravityOAuthClientsLoadConfiguredEntriesAndDefaultToFirst(t *testing.T) {
	t.Setenv(antigravityOAuthClientsEnv, "primary|client-id|client-secret;backup|backup-id|backup-secret")
	t.Setenv(antigravityActiveOAuthClientEnv, "")
	resetConfiguredAntigravityOAuth(t)
	clients, active := effectiveAntigravityOAuthClients()
	if len(clients) != 2 || active != "primary" {
		t.Fatalf("clients/active = %#v/%q", clients, active)
	}
	t.Setenv(antigravityActiveOAuthClientEnv, "BACKUP")
	_, active = effectiveAntigravityOAuthClients()
	if active != "backup" {
		t.Fatalf("active = %q, want backup", active)
	}
}

func TestAntigravityOAuthCandidatesAppendOfficialDesktopClient(t *testing.T) {
	client := &AntigravityClient{
		oauth: []antigravityOAuthClient{{Key: "custom", ClientID: "custom-id", ClientSecret: "custom-secret"}},
	}
	got := client.oauthCandidates(AntigravityCredential{RefreshToken: "rt"})
	if len(got) != 2 || got[0].Key != "custom" || got[1].Key != AntigravityDefaultOAuthClientKey {
		t.Fatalf("candidates = %#v", got)
	}
	// 已配置 official key 时不再重复追加内置条目。
	client.oauth = append(client.oauth, builtinAntigravityOAuthClient())
	got = client.oauthCandidates(AntigravityCredential{RefreshToken: "rt"})
	if len(got) != 2 || got[1].ClientID != AntigravityDefaultOAuthClientID {
		t.Fatalf("deduped candidates = %#v", got)
	}
}

func TestAntigravityAuthorizationURLIncludesPKCEAndOfflineAccess(t *testing.T) {
	client := newAntigravityClient(http.DefaultClient, AntigravityEndpoints{})
	client.oauth = []antigravityOAuthClient{{Key: "custom", ClientID: "client-id", ClientSecret: "client-secret"}}
	client.activeKey = "custom"

	gotURL, info, err := client.BuildOAuthAuthorizationURL(
		"http://127.0.0.1:43123/oauth-callback", "state-123", "challenge-456", "custom",
	)
	if err != nil {
		t.Fatalf("BuildOAuthAuthorizationURL() error: %v", err)
	}
	if info.Key != "custom" || info.ClientID != "client-id" {
		t.Fatalf("client info = %+v", info)
	}
	parsed, err := url.Parse(gotURL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	checks := map[string]string{
		"client_id":              "client-id",
		"redirect_uri":           "http://127.0.0.1:43123/oauth-callback",
		"response_type":          "code",
		"access_type":            "offline",
		"prompt":                 "consent",
		"include_granted_scopes": "true",
		"state":                  "state-123",
		"code_challenge":         "challenge-456",
		"code_challenge_method":  "S256",
	}
	for key, want := range checks {
		if got := query.Get(key); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	if query.Get("scope") == "" || !strings.Contains(query.Get("scope"), "openid") {
		t.Fatalf("scope = %q", query.Get("scope"))
	}
}

func TestAntigravityAuthorizationURLRejectsMalformedRedirectOrClient(t *testing.T) {
	client := newAntigravityClient(http.DefaultClient, AntigravityEndpoints{})
	client.oauth = []antigravityOAuthClient{{Key: "custom", ClientID: "client-id", ClientSecret: "client-secret"}}
	for name, redirect := range map[string]string{
		"missing scheme": "127.0.0.1:43123/oauth-callback",
		"missing host":   "http:///oauth-callback",
		"empty redirect": "",
		"wrong scheme":   "https://127.0.0.1:43123/oauth-callback",
		"wrong path":     "http://127.0.0.1:43123/other",
		"query":          "http://127.0.0.1:43123/oauth-callback?next=1",
		"fragment":       "http://127.0.0.1:43123/oauth-callback#fragment",
		"userinfo":       "http://user@127.0.0.1:43123/oauth-callback",
		"external host":  "http://192.0.2.1:43123/oauth-callback",
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := client.BuildOAuthAuthorizationURL(redirect, "state", "challenge", "custom"); err == nil {
				t.Fatalf("redirect %q unexpectedly accepted", redirect)
			}
		})
	}
	if _, _, err := client.BuildOAuthAuthorizationURL("http://127.0.0.1:43123/oauth-callback", "state", "challenge", "unknown"); err == nil {
		t.Fatal("unknown OAuth client unexpectedly accepted")
	}
}

func TestAntigravityAuthorizationCodeExchangeSendsPKCEParameters(t *testing.T) {
	fixedNow := time.Date(2026, 8, 16, 6, 0, 0, 0, time.UTC)
	var gotForm url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
			t.Fatalf("request = %s %s content-type=%q", r.Method, r.URL.Path, r.Header.Get("Content-Type"))
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		gotForm = r.Form
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"access_token":"access-token","refresh_token":"refresh-token","id_token":"id-token","expires_in":3600,"scope":"openid profile"}`)
	}))
	defer server.Close()

	client := newAntigravityClient(server.Client(), AntigravityEndpoints{TokenURL: server.URL})
	client.oauth = []antigravityOAuthClient{{Key: "custom", ClientID: "client-id", ClientSecret: "client-secret"}}
	client.activeKey = "custom"
	client.now = func() time.Time { return fixedNow }

	credential, err := client.ExchangeOAuthAuthorizationCode(context.Background(), "authorization-code", "http://127.0.0.1:43123/oauth-callback", "pkce-verifier", "custom")
	if err != nil {
		t.Fatalf("ExchangeOAuthAuthorizationCode() error: %v", err)
	}
	for key, want := range map[string]string{
		"client_id":     "client-id",
		"client_secret": "client-secret",
		"code":          "authorization-code",
		"redirect_uri":  "http://127.0.0.1:43123/oauth-callback",
		"grant_type":    "authorization_code",
		"code_verifier": "pkce-verifier",
	} {
		if got := gotForm.Get(key); got != want {
			t.Errorf("form %s = %q, want %q", key, got, want)
		}
	}
	if credential.AccessToken != "access-token" || credential.RefreshToken != "refresh-token" || credential.IDToken != "id-token" || credential.OAuthClientKey != "custom" || credential.ClientID != "client-id" || credential.ClientSecret != "client-secret" || credential.Scope != "openid profile" || !credential.ExpiresAt.Equal(fixedNow.Add(time.Hour)) {
		t.Fatalf("credential = %+v", credential)
	}
}

func TestAntigravityAuthorizationCodeExchangeRequiresPKCEInputs(t *testing.T) {
	client := newAntigravityClient(http.DefaultClient, AntigravityEndpoints{})
	client.oauth = []antigravityOAuthClient{{Key: "custom", ClientID: "client-id", ClientSecret: "client-secret"}}
	for name, args := range map[string][3]string{
		"missing code":     {"", "http://127.0.0.1:1/oauth-callback", "verifier"},
		"missing redirect": {"code", "", "verifier"},
		"missing verifier": {"code", "http://127.0.0.1:1/oauth-callback", ""},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := client.ExchangeOAuthAuthorizationCode(context.Background(), args[0], args[1], args[2], "custom"); err == nil {
				t.Fatal("missing PKCE input unexpectedly accepted")
			}
		})
	}
}

func TestAntigravityRefreshPersistsFallbackOAuthClientMetadataForNextRefresh(t *testing.T) {
	var requestedClientIDs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		clientID := r.Form.Get("client_id")
		requestedClientIDs = append(requestedClientIDs, clientID)
		w.Header().Set("Content-Type", "application/json")
		switch clientID {
		case "custom-client":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"invalid_client"}`)
		case "fallback-client":
			_, _ = io.WriteString(w, `{"access_token":"access-new","refresh_token":"refresh-new","expires_in":3600}`)
		default:
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"invalid_client"}`)
		}
	}))
	defer server.Close()

	client := newAntigravityClient(server.Client(), AntigravityEndpoints{TokenURL: server.URL})
	client.oauth = []antigravityOAuthClient{{Key: "fallback", ClientID: "fallback-client", ClientSecret: "fallback-secret"}}
	client.activeKey = "fallback"
	credential := AntigravityCredential{
		RefreshToken: "refresh-old", OAuthClientKey: "custom",
		ClientID: "custom-client", ClientSecret: "custom-secret",
	}

	if err := client.refreshCredential(context.Background(), &credential); err != nil {
		t.Fatalf("first refreshCredential() error: %v", err)
	}
	if credential.OAuthClientKey != "fallback" || credential.ClientID != "fallback-client" || credential.ClientSecret != "fallback-secret" {
		t.Fatalf("fallback OAuth metadata = %q/%q/%q, want coherent fallback tuple", credential.OAuthClientKey, credential.ClientID, credential.ClientSecret)
	}
	if err := client.refreshCredential(context.Background(), &credential); err != nil {
		t.Fatalf("second refreshCredential() with persisted fallback metadata error: %v", err)
	}
	if got, want := strings.Join(requestedClientIDs, ","), "custom-client,fallback-client,fallback-client"; got != want {
		t.Fatalf("token client sequence = %q, want %q", got, want)
	}
}

func TestAntigravityRefreshMixedCandidateFailuresRemainRetryable(t *testing.T) {
	var requestedClientIDs []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		clientID := r.Form.Get("client_id")
		requestedClientIDs = append(requestedClientIDs, clientID)
		w.Header().Set("Content-Type", "application/json")
		switch clientID {
		case "custom-client":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":"invalid_client"}`)
		case "fallback-client":
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":"temporarily_unavailable"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newAntigravityClient(server.Client(), AntigravityEndpoints{TokenURL: server.URL})
	client.oauth = []antigravityOAuthClient{{Key: "fallback", ClientID: "fallback-client", ClientSecret: "fallback-secret"}}
	client.activeKey = "fallback"
	credential := AntigravityCredential{
		RefreshToken: "refresh-old", OAuthClientKey: "custom",
		ClientID: "custom-client", ClientSecret: "custom-secret",
	}

	err := client.refreshCredential(context.Background(), &credential)
	if err == nil {
		t.Fatal("refreshCredential() unexpectedly succeeded")
	}
	if IsPermanentRefreshFailure(err) {
		t.Fatalf("mixed permanent/transient candidate error was classified permanent: %v", err)
	}
	if got, want := strings.Join(requestedClientIDs, ","), "custom-client,fallback-client"; got != want {
		t.Fatalf("token client sequence = %q, want %q", got, want)
	}
}

func TestAntigravityClientSyncRefreshesIdentityAndQuota(t *testing.T) {
	fixedNow := time.Date(2026, 8, 16, 6, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	quotaBodies := make([]string, 0, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if r.Form.Get("client_id") != "client-id" || r.Form.Get("client_secret") != "client-secret" || r.Form.Get("refresh_token") != "refresh-old" {
				t.Fatalf("unexpected refresh form: %v", r.Form)
			}
			_, _ = io.WriteString(w, `{"access_token":"access-new","refresh_token":"refresh-rotated","expires_in":3600,"token_type":"Bearer","scope":"scope-a scope-b"}`)
		case "/userinfo":
			if got := r.Header.Get("Authorization"); got != "Bearer access-new" {
				t.Fatalf("authorization = %q", got)
			}
			_, _ = io.WriteString(w, `{"id":"google-subject","email":"user@example.com","verified_email":true,"name":"User Name","picture":"https://example.com/avatar.png"}`)
		case "/load":
			_, _ = io.WriteString(w, `{"cloudaicompanionProject":"project-1","paidTier":{"id":"ultra","name":"Google AI Ultra"},"allowedTiers":[{"id":"free","name":"Free","is_default":true}]}`)
		case "/quota":
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			quotaBodies = append(quotaBodies, string(body))
			call := len(quotaBodies)
			mu.Unlock()
			if call == 1 {
				w.WriteHeader(http.StatusForbidden)
				_, _ = io.WriteString(w, `{"error":"project denied"}`)
				return
			}
			_, _ = io.WriteString(w, `{"models":{"gemini-2.5-pro":{"quotaInfo":{"remainingFraction":0.73,"resetTime":"2026-08-16T11:00:00Z"},"displayName":"Gemini 2.5 Pro","supportsThinking":true},"internal-chat":{"quotaInfo":{"remainingFraction":1}}},"deprecatedModelIds":{"gemini-old":{"newModelId":"gemini-2.5-pro"}}}`)
		case "/summary":
			_, _ = io.WriteString(w, `{"groups":[{"displayName":"Gemini Models","buckets":[{"bucketId":"gemini-5h","window":"5h","remainingFraction":0.61,"resetTime":"2026-08-16T10:30:00Z"}]}]}`)
		case "/credits":
			_, _ = io.WriteString(w, `{"paidTier":{"availableCredits":[{"creditAmount":"123"}]}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newAntigravityClient(server.Client(), AntigravityEndpoints{
		TokenURL: server.URL + "/token", UserInfoURL: server.URL + "/userinfo",
		LoadProject: []string{server.URL + "/load"}, Quota: []string{server.URL + "/quota"},
		QuotaSummary: []string{server.URL + "/summary"}, AICredits: []string{server.URL + "/credits"},
	})
	client.now = func() time.Time { return fixedNow }
	client.oauth = nil
	client.activeKey = ""

	result, err := client.Sync(context.Background(), AntigravityCredential{
		RefreshToken: "refresh-old", ClientID: "client-id", ClientSecret: "client-secret", OAuthClientKey: "custom",
	})
	if err != nil {
		t.Fatalf("Sync() error: %v", err)
	}
	if result.Credential.AccessToken != "access-new" || result.Credential.RefreshToken != "refresh-rotated" {
		t.Fatalf("credential = %+v", result.Credential)
	}
	if result.Credential.Scope != "scope-a scope-b" || !result.Credential.ExpiresAt.Equal(fixedNow.Add(time.Hour)) {
		t.Fatalf("refreshed metadata = %+v", result.Credential)
	}
	if result.Profile.ID != "google-subject" || result.Profile.Email != "user@example.com" {
		t.Fatalf("profile = %+v", result.Profile)
	}
	if result.Entitlements.ProjectID != "project-1" || result.Entitlements.EffectiveTier != "Google AI Ultra" || result.Entitlements.Restricted {
		t.Fatalf("entitlements = %+v", result.Entitlements)
	}
	if len(result.Quota.Models) != 1 || result.Quota.Models[0].ModelID != "gemini-2.5-pro" || result.Quota.Models[0].RemainingPercent != 73 {
		t.Fatalf("models = %+v", result.Quota.Models)
	}
	if len(result.Quota.Groups) != 1 || len(result.Quota.Groups[0].Buckets) != 1 {
		t.Fatalf("groups = %+v", result.Quota.Groups)
	}
	if result.Quota.AICredits == nil || result.Quota.AICredits.Credits != 123 {
		t.Fatalf("AI credits = %+v", result.Quota.AICredits)
	}
	if result.Quota.ModelForwardingRules["gemini-old"] != "gemini-2.5-pro" {
		t.Fatalf("forwarding rules = %+v", result.Quota.ModelForwardingRules)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(quotaBodies) != 2 || !strings.Contains(quotaBodies[0], `"project":"project-1"`) || quotaBodies[1] != "{}" {
		t.Fatalf("quota bodies = %v", quotaBodies)
	}
}

func TestNormalizeAntigravityEntitlementsRestrictedFallback(t *testing.T) {
	now := time.Now()
	payload := antigravityLoadProjectResponse{
		AllowedTiers: []antigravityTierPayload{{ID: "free", Name: "Free", IsDefault: true}},
		IneligibleTiers: []struct {
			ReasonCode string `json:"reasonCode"`
		}{{ReasonCode: "REGION"}},
	}
	got := normalizeAntigravityEntitlements(payload, now)
	if !got.Restricted || got.EffectiveTier != "Free (Restricted)" {
		t.Fatalf("entitlements = %+v", got)
	}
	encoded, err := json.Marshal(got)
	if err != nil || !strings.Contains(string(encoded), "REGION") {
		t.Fatalf("encoded entitlements = %s, err=%v", encoded, err)
	}
}

func TestAntigravityClientQuotaFinalForbiddenIsSnapshot(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":"forbidden"}`)
	}))
	defer server.Close()
	client := newAntigravityClient(server.Client(), AntigravityEndpoints{Quota: []string{server.URL}})
	quota, err := client.fetchQuota(context.Background(), "access", "")
	if err != nil || !quota.Forbidden || len(quota.Models) != 0 {
		t.Fatalf("quota = %+v, err=%v", quota, err)
	}
}

func TestAntigravityClientQuotaFallsBackAfterForbiddenEndpoint(t *testing.T) {
	var sandboxCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/sandbox":
			sandboxCalls++
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":"sandbox denied"}`)
		case "/daily":
			_, _ = io.WriteString(w, `{"models":{"gemini-2.5-pro":{"quotaInfo":{"remainingFraction":0.5}}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newAntigravityClient(server.Client(), AntigravityEndpoints{
		Quota: []string{server.URL + "/sandbox", server.URL + "/daily"},
	})
	quota, err := client.fetchQuota(context.Background(), "access", "project")
	if err != nil || quota.Forbidden || len(quota.Models) != 1 || quota.Models[0].RemainingPercent != 50 {
		t.Fatalf("quota = %+v, err=%v", quota, err)
	}
	if sandboxCalls != 2 {
		t.Fatalf("sandbox calls = %d, want project and no-project attempts", sandboxCalls)
	}
}

func TestAntigravityClientQuotaUnauthorizedRebuildsIdentityContext(t *testing.T) {
	var userInfoCalls, entitlementCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			_, _ = io.WriteString(w, `{"access_token":"access-b","refresh_token":"refresh-b-rotated","expires_in":3600}`)
		case "/userinfo":
			userInfoCalls++
			if r.Header.Get("Authorization") == "Bearer access-a" {
				_, _ = io.WriteString(w, `{"id":"subject-a","email":"a@example.com","verified_email":true,"name":"Account A"}`)
				return
			}
			_, _ = io.WriteString(w, `{"id":"subject-b","email":"b@example.com","verified_email":true,"name":"Account B"}`)
		case "/load":
			entitlementCalls++
			if r.Header.Get("Authorization") == "Bearer access-a" {
				_, _ = io.WriteString(w, `{"cloudaicompanionProject":"project-a","paidTier":{"name":"Tier A"}}`)
				return
			}
			_, _ = io.WriteString(w, `{"cloudaicompanionProject":"project-b","paidTier":{"name":"Tier B"}}`)
		case "/quota":
			if r.Header.Get("Authorization") == "Bearer access-a" {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = io.WriteString(w, `{"error":"expired"}`)
				return
			}
			_, _ = io.WriteString(w, `{"models":{"gemini-2.5-pro":{"quotaInfo":{"remainingFraction":0.8}}}}`)
		case "/summary":
			_, _ = io.WriteString(w, `{"groups":[]}`)
		case "/credits":
			_, _ = io.WriteString(w, `{"paidTier":{"availableCredits":[]}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newAntigravityClient(server.Client(), AntigravityEndpoints{
		TokenURL: server.URL + "/token", UserInfoURL: server.URL + "/userinfo",
		LoadProject: []string{server.URL + "/load"}, Quota: []string{server.URL + "/quota"},
		QuotaSummary: []string{server.URL + "/summary"}, AICredits: []string{server.URL + "/credits"},
	})
	client.oauth = nil
	result, err := client.Sync(context.Background(), AntigravityCredential{
		AccessToken: "access-a", RefreshToken: "refresh-b", IDToken: "id-token-a", ClientID: "client-id", ClientSecret: "client-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Profile.ID != "subject-b" || result.Profile.Email != "b@example.com" || result.Credential.AccessToken != "access-b" || result.Credential.RefreshToken != "refresh-b-rotated" || result.Credential.IDToken != "" {
		t.Fatalf("mixed identity result = %+v / %+v", result.Profile, result.Credential)
	}
	if result.Entitlements.ProjectID != "project-b" || result.Entitlements.EffectiveTier != "Tier B" {
		t.Fatalf("entitlements = %+v", result.Entitlements)
	}
	if userInfoCalls != 2 || entitlementCalls != 2 {
		t.Fatalf("identity context calls = userinfo %d entitlements %d, want 2/2", userInfoCalls, entitlementCalls)
	}
}

func TestAntigravityClientReturnsRotatedCredentialOnQuotaFailure(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/token":
			_, _ = io.WriteString(w, `{"access_token":"access-new","refresh_token":"refresh-rotated","expires_in":3600}`)
		case "/userinfo":
			_, _ = io.WriteString(w, `{"id":"subject","email":"user@example.com","verified_email":true}`)
		case "/load":
			_, _ = io.WriteString(w, `{"cloudaicompanionProject":"project","paidTier":{"name":"Pro"}}`)
		case "/quota":
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"temporary"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newAntigravityClient(server.Client(), AntigravityEndpoints{
		TokenURL: server.URL + "/token", UserInfoURL: server.URL + "/userinfo",
		LoadProject: []string{server.URL + "/load"}, Quota: []string{server.URL + "/quota"},
	})
	client.oauth = nil
	result, err := client.Sync(context.Background(), AntigravityCredential{
		RefreshToken: "refresh-old", ClientID: "client-id", ClientSecret: "client-secret",
	})
	if err == nil {
		t.Fatal("Sync() unexpectedly succeeded")
	}
	if result.Credential.AccessToken != "access-new" || result.Credential.RefreshToken != "refresh-rotated" || result.Profile.Email != "user@example.com" || !result.EntitlementsObserved {
		t.Fatalf("partial result = %+v", result)
	}
}

func TestAntigravitySyncSurfacesValidationRequiredQuotaWarning(t *testing.T) {
	// 真实 VALIDATION_REQUIRED 错误体超过 512 字节,验证 URL 位于尾部:
	// 该用例同时证明 warning 解析的是未截断 rawBody,而非渲染用的 Body。
	validationURL := "https://accounts.google.com/signin/continue?sarp=1&scc=1&continue=https://developers.google.com/gemini-code-assist/auth/auth_success_gemini&plt=AKgnsbv6rSViHMUsE" + strings.Repeat("x", 280)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/userinfo":
			_, _ = io.WriteString(w, `{"id":"subject","email":"user@example.com","verified_email":true}`)
		case "/load":
			_, _ = io.WriteString(w, `{"cloudaicompanionProject":"project","paidTier":{"name":"Pro"}}`)
		case "/quota":
			_, _ = io.WriteString(w, `{"models":{"gemini-2.5-pro":{"quotaInfo":{"remainingFraction":0.5}}}}`)
		case "/summary":
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":{"code":403,"message":"Verify your account to continue.","status":"PERMISSION_DENIED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"VALIDATION_REQUIRED","domain":"cloudcode-pa.googleapis.com","metadata":{"validation_error_message":"Verify your account to continue.","validation_url":"`+validationURL+`"}}]}}`)
		case "/credits":
			_, _ = io.WriteString(w, `{"paidTier":{"availableCredits":[]}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newAntigravityClient(server.Client(), AntigravityEndpoints{
		UserInfoURL: server.URL + "/userinfo",
		LoadProject: []string{server.URL + "/load"}, Quota: []string{server.URL + "/quota"},
		QuotaSummary: []string{server.URL + "/summary"}, AICredits: []string{server.URL + "/credits"},
	})
	client.oauth = nil

	result, err := client.Sync(context.Background(), AntigravityCredential{AccessToken: "access"})
	if err != nil {
		t.Fatalf("Sync() error = %v (quota summary failure must not fail the sync)", err)
	}
	if result.QuotaGroupsObserved {
		t.Fatal("QuotaGroupsObserved = true, want false after 403")
	}
	if !strings.Contains(result.Warning, "Verify your account to continue.") {
		t.Fatalf("warning missing upstream message: %q", result.Warning)
	}
	if !strings.Contains(result.Warning, validationURL) {
		t.Fatalf("warning missing untruncated verification URL: %q", result.Warning)
	}
	if strings.Contains(result.Warning, "temporarily unavailable") {
		t.Fatalf("VALIDATION_REQUIRED rendered as temporarily unavailable: %q", result.Warning)
	}
}

func TestAntigravitySyncQuotaSummaryTransientFailureKeepsDiagnostic(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/userinfo":
			_, _ = io.WriteString(w, `{"id":"subject","email":"user@example.com","verified_email":true}`)
		case "/load":
			_, _ = io.WriteString(w, `{"cloudaicompanionProject":"project","paidTier":{"name":"Pro"}}`)
		case "/quota":
			_, _ = io.WriteString(w, `{"models":{"gemini-2.5-pro":{"quotaInfo":{"remainingFraction":0.5}}}}`)
		case "/summary":
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"code":503,"message":"backend restarting","status":"UNAVAILABLE"}}`)
		case "/credits":
			_, _ = io.WriteString(w, `{"paidTier":{"availableCredits":[]}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newAntigravityClient(server.Client(), AntigravityEndpoints{
		UserInfoURL: server.URL + "/userinfo",
		LoadProject: []string{server.URL + "/load"}, Quota: []string{server.URL + "/quota"},
		QuotaSummary: []string{server.URL + "/summary"}, AICredits: []string{server.URL + "/credits"},
	})
	client.oauth = nil

	result, err := client.Sync(context.Background(), AntigravityCredential{AccessToken: "access"})
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	if !strings.Contains(result.Warning, "Antigravity quota summary is temporarily unavailable") ||
		!strings.Contains(result.Warning, "backend restarting") {
		t.Fatalf("warning = %q, want transient wording with upstream detail", result.Warning)
	}
}

func TestAntigravitySyncSurfacesTOSViolationAppealLink(t *testing.T) {
	const appealURL = "https://forms.gle/hGzM9MEUv2azZsrb9"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/userinfo":
			_, _ = io.WriteString(w, `{"id":"subject","email":"user@example.com","verified_email":true}`)
		case "/load":
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":{"code":403,"message":"This service has been disabled in this account for violation of Terms of Service. Please submit an appeal to continue using this product.","status":"PERMISSION_DENIED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"TOS_VIOLATION","domain":"cloudcode-pa.googleapis.com","metadata":{"appeal_url_link_text":"Submit Appeal","appeal_url":"`+appealURL+`","uiMessage":"true"}}]}}`)
		case "/quota":
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":"forbidden"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newAntigravityClient(server.Client(), AntigravityEndpoints{
		UserInfoURL: server.URL + "/userinfo",
		LoadProject: []string{server.URL + "/load"}, Quota: []string{server.URL + "/quota"},
	})
	client.oauth = nil

	result, err := client.Sync(context.Background(), AntigravityCredential{AccessToken: "access"})
	if err != nil {
		t.Fatalf("Sync() error = %v (a forbidden quota snapshot must not fail the sync)", err)
	}
	if !result.Quota.Forbidden || result.EntitlementsObserved {
		t.Fatalf("quota/entitlements = forbidden=%t observed=%t, want true/false", result.Quota.Forbidden, result.EntitlementsObserved)
	}
	if !strings.Contains(result.Warning, "Terms of Service violation") || !strings.Contains(result.Warning, appealURL) {
		t.Fatalf("warning = %q, want short TOS summary with appeal URL", result.Warning)
	}
	if strings.Contains(result.Warning, `"details"`) {
		t.Fatalf("warning still embeds the raw upstream JSON blob: %q", result.Warning)
	}
}

func TestAntigravityClientOnboardsProjectWhenLoadCodeAssistHasNone(t *testing.T) {
	var mu sync.Mutex
	onboardCalls := 0
	var onboardBody map[string]any
	var onboardAPIClient string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/userinfo":
			_, _ = io.WriteString(w, `{"id":"google-subject","email":"fresh@example.com","verified_email":true,"name":"Fresh"}`)
		case "/load":
			// A never-onboarded account: no companion project, camelCase isDefault.
			_, _ = io.WriteString(w, `{"currentTier":{"id":"free-tier","name":"Free"},"allowedTiers":[{"id":"free-tier","name":"Free","isDefault":true},{"id":"g1-pro-tier","name":"Pro"}]}`)
		case "/onboard":
			mu.Lock()
			onboardCalls++
			call := onboardCalls
			onboardAPIClient = r.Header.Get("X-Goog-Api-Client")
			_ = json.NewDecoder(r.Body).Decode(&onboardBody)
			mu.Unlock()
			if call == 1 {
				_, _ = io.WriteString(w, `{"name":"operations/1","done":false}`)
				return
			}
			_, _ = io.WriteString(w, `{"name":"operations/1","done":true,"response":{"cloudaicompanionProject":{"id":"provisioned-project"}}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newAntigravityClient(server.Client(), AntigravityEndpoints{
		UserInfoURL: server.URL + "/userinfo",
		LoadProject: []string{server.URL + "/load"},
		OnboardUser: []string{server.URL + "/onboard"},
	})
	sleeps := 0
	client.sleep = func(context.Context, time.Duration) error { sleeps++; return nil }

	credential := AntigravityCredential{AccessToken: "access-token"}
	_, entitlements, entitlementErr, err := client.fetchIdentityContext(context.Background(), &credential, false)
	if err != nil {
		t.Fatalf("fetchIdentityContext() error: %v", err)
	}
	if entitlementErr != nil {
		t.Fatalf("entitlement error: %v", entitlementErr)
	}
	if entitlements.ProjectID != "provisioned-project" || credential.ProjectID != "provisioned-project" {
		t.Fatalf("project = %q / %q, want provisioned-project", entitlements.ProjectID, credential.ProjectID)
	}
	if onboardCalls != 2 || sleeps != 1 {
		t.Fatalf("onboard calls = %d, sleeps = %d, want 2 polls with one wait", onboardCalls, sleeps)
	}
	if onboardBody["tierId"] != "free-tier" {
		t.Fatalf("onboard tierId = %#v, want the advertised default tier", onboardBody["tierId"])
	}
	metadata, _ := onboardBody["metadata"].(map[string]any)
	if metadata["ideType"] != "ANTIGRAVITY" || metadata["pluginType"] != "GEMINI" {
		t.Fatalf("onboard metadata = %#v", onboardBody["metadata"])
	}
	if onboardAPIClient != antigravityOnboardAPIClient {
		t.Fatalf("X-Goog-Api-Client = %q", onboardAPIClient)
	}
	if len(entitlements.AllowedTiers) != 2 || !entitlements.AllowedTiers[0].IsDefault || entitlements.AllowedTiers[1].IsDefault {
		t.Fatalf("camelCase isDefault was not parsed: %+v", entitlements.AllowedTiers)
	}
}

func TestAntigravityClientOnboardFailureSurfacesAsEntitlementWarning(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/userinfo":
			_, _ = io.WriteString(w, `{"id":"google-subject","email":"fresh@example.com","verified_email":true}`)
		case "/load":
			_, _ = io.WriteString(w, `{"allowedTiers":[{"id":"free-tier","isDefault":true}]}`)
		case "/onboard":
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":{"code":403,"message":"onboarding denied","status":"PERMISSION_DENIED"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newAntigravityClient(server.Client(), AntigravityEndpoints{
		UserInfoURL: server.URL + "/userinfo",
		LoadProject: []string{server.URL + "/load"},
		OnboardUser: []string{server.URL + "/onboard"},
	})
	client.sleep = func(context.Context, time.Duration) error { return nil }

	credential := AntigravityCredential{AccessToken: "access-token"}
	_, entitlements, entitlementErr, err := client.fetchIdentityContext(context.Background(), &credential, false)
	if err != nil {
		t.Fatalf("fetchIdentityContext() error: %v", err)
	}
	if entitlementErr == nil || !strings.Contains(entitlementErr.Error(), "onboardUser") {
		t.Fatalf("entitlement error = %v, want onboardUser failure", entitlementErr)
	}
	if entitlements.ProjectID != "" {
		t.Fatalf("project = %q, want empty after failed onboarding", entitlements.ProjectID)
	}
}

func TestAntigravityClientKeepsPreservedProjectWithoutOnboarding(t *testing.T) {
	onboardCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/userinfo":
			_, _ = io.WriteString(w, `{"id":"google-subject","email":"user@example.com","verified_email":true}`)
		case "/load":
			_, _ = io.WriteString(w, `{"allowedTiers":[{"id":"free-tier","isDefault":true}]}`)
		case "/onboard":
			onboardCalls++
			_, _ = io.WriteString(w, `{"done":true,"response":{"cloudaicompanionProject":"other"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := newAntigravityClient(server.Client(), AntigravityEndpoints{
		UserInfoURL: server.URL + "/userinfo",
		LoadProject: []string{server.URL + "/load"},
		OnboardUser: []string{server.URL + "/onboard"},
	})
	credential := AntigravityCredential{AccessToken: "access-token", ProjectID: "existing-project"}
	_, entitlements, entitlementErr, err := client.fetchIdentityContext(context.Background(), &credential, true)
	if err != nil || entitlementErr != nil {
		t.Fatalf("errors: %v / %v", err, entitlementErr)
	}
	if entitlements.ProjectID != "existing-project" || onboardCalls != 0 {
		t.Fatalf("project = %q, onboard calls = %d; a preserved project must not trigger onboarding", entitlements.ProjectID, onboardCalls)
	}
}
