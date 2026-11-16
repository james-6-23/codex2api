package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/database"
)

func TestNormalizeKiroAuthKind(t *testing.T) {
	cases := []struct {
		raw, endpoint  string
		apiKey, secret bool
		want           string
	}{
		{"social", "", false, false, KiroAuthKindSocial},
		{"builder-id", "", false, true, KiroAuthKindIdC},
		{"IAM", "", false, false, KiroAuthKindIdC},
		{"AzureAD", "", false, false, KiroAuthKindExternalIdP},
		{"social", "https://login.microsoftonline.com/t/oauth2/v2.0/token", false, false, KiroAuthKindExternalIdP},
		{"", "", true, false, KiroAuthKindAPIKey},
		{"", "", false, true, KiroAuthKindIdC},
		{"", "", false, false, KiroAuthKindSocial},
	}
	for _, tc := range cases {
		if got := NormalizeKiroAuthKind(tc.raw, tc.endpoint, tc.apiKey, tc.secret); got != tc.want {
			t.Errorf("NormalizeKiroAuthKind(%q,%q,%t,%t)=%q want %q", tc.raw, tc.endpoint, tc.apiKey, tc.secret, got, tc.want)
		}
	}
}

func TestKiroCredentialRegionsAndProfileARN(t *testing.T) {
	enterprise := KiroCredential{AuthKind: KiroAuthKindIdC, Region: "us-east-1", ProfileARN: "arn:aws:codewhisperer:eu-central-1:123:profile/ABC"}
	if got := enterprise.EffectiveAPIRegion(); got != "eu-central-1" {
		t.Fatalf("api region from profile ARN = %q", got)
	}
	builder := KiroCredential{AuthKind: KiroAuthKindIdC, ProfileARN: KiroBuilderIDProfileARN}
	if builder.EffectiveProfileARN() != "" || builder.StreamingProfileARN() != KiroBuilderIDProfileARN || builder.EffectiveAPIRegion() != KiroDefaultRegion {
		t.Fatalf("builder ID placeholder must stream but never count as a real profile: %+v", builder)
	}
	social := KiroCredential{AuthKind: KiroAuthKindSocial}
	if social.StreamingProfileARN() != KiroSocialProfileARN {
		t.Fatalf("social default ARN = %q", social.StreamingProfileARN())
	}
	apiKey := KiroCredential{AuthKind: KiroAuthKindAPIKey, ProfileARN: KiroSocialProfileARN}
	if apiKey.StreamingProfileARN() != "" || apiKey.TokenTypeHeader() != "API_KEY" {
		t.Fatal("API Key credentials must not carry a profile ARN")
	}
}

func TestKiroMachineIDIsStable(t *testing.T) {
	if got := NormalizeKiroMachineID("0123456789ABCDEF0123456789abcdef"); got != "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" {
		t.Fatalf("uuid-form machine id = %q", got)
	}
	first, second := DeriveKiroMachineID("", "rt-1", ""), DeriveKiroMachineID("bad", "rt-1", "")
	if first != second || len(first) != 64 {
		t.Fatalf("derived machine id not stable: %q %q", first, second)
	}
	if DeriveKiroMachineID("", "rt-1", "ksk_x") == first {
		t.Fatal("API Key must take precedence over refresh token as the seed")
	}
}

func TestValidateKiroExternalIdPEndpoint(t *testing.T) {
	for _, ok := range []string{"https://login.microsoftonline.com/t/oauth2/v2.0/token", "https://login.microsoftonline.us/t/token"} {
		if err := ValidateKiroExternalIdPEndpoint(ok); err != nil {
			t.Errorf("%s rejected: %v", ok, err)
		}
	}
	for _, bad := range []string{"http://login.microsoftonline.com/t", "https://evil-microsoftonline.com/token", "https://microsoftonline.com/token", "https://127.0.0.1/token"} {
		if ValidateKiroExternalIdPEndpoint(bad) == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestKiroRefreshUpdatesTokensAndClearsUnauthorized(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	t.Cleanup(store.Stop)
	account := &Account{DBID: 901, UpstreamType: UpstreamKiro, AccessToken: "old-at", RefreshToken: "rt", Status: StatusReady,
		ExpiresAt: time.Now().Add(time.Minute), Kiro: KiroCredential{AuthKind: KiroAuthKindSocial, MachineID: DeriveKiroMachineID("", "rt", "")}}
	store.AddAccount(account)
	store.MarkCooldownWithError(account, time.Hour, "unauthorized", "upstream 401")
	previous := kiroTokenRefreshFunc
	t.Cleanup(func() { kiroTokenRefreshFunc = previous })
	kiroTokenRefreshFunc = func(_ context.Context, cred KiroCredential, rt, _ string) (*KiroTokenData, error) {
		if cred.AuthKind != KiroAuthKindSocial || rt != "rt" {
			t.Fatalf("unexpected refresh input %+v %q", cred, rt)
		}
		return &KiroTokenData{AccessToken: "new-at", ProfileARN: KiroSocialProfileARN, ExpiresAt: time.Now().Add(time.Hour)}, nil
	}
	if !account.IsKiro() || !account.IsRelayStyle() || !account.KiroRefreshable() {
		t.Fatal("Kiro account must be relay style and refreshable")
	}
	if err := store.RefreshKiroAccount(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	auth := account.KiroRequestAuth()
	if auth.BearerToken != "new-at" || auth.Credential.ProfileARN != KiroSocialProfileARN || account.HasActiveCooldown() || account.RefreshToken != "rt" {
		t.Fatalf("refresh not applied: %+v cooldown=%t", auth, account.HasActiveCooldown())
	}

	kiroTokenRefreshFunc = func(context.Context, KiroCredential, string, string) (*KiroTokenData, error) {
		return nil, &kiroRefreshInvalidError{message: "invalid_grant"}
	}
	err := store.RefreshKiroAccount(context.Background(), account)
	var invalid *kiroRefreshInvalidError
	if !errors.As(err, &invalid) || !IsPermanentRefreshFailure(err) {
		t.Fatalf("invalid_grant must be a permanent refresh failure: %v", err)
	}
}

func TestKiroAPIKeyAccountUsesKeyAsBearer(t *testing.T) {
	account := &Account{UpstreamType: UpstreamKiro, APIKey: "ksk_test", Kiro: KiroCredential{AuthKind: KiroAuthKindAPIKey}}
	if !account.IsKiro() || account.KiroRefreshable() || account.KiroRequestAuth().BearerToken != "ksk_test" {
		t.Fatal("API Key Kiro account must use the key as bearer and never refresh")
	}
}

func TestKiroRefreshSerializesAcrossRotationAndKeepsNewCooldown(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	t.Cleanup(store.Stop)
	account := &Account{DBID: 902, UpstreamType: UpstreamKiro, AccessToken: "at", RefreshToken: "rt-1", Status: StatusReady,
		ExpiresAt: time.Now().Add(time.Minute), Kiro: KiroCredential{AuthKind: KiroAuthKindIdC, ClientID: "c", ClientSecret: "s", Region: "us-east-1"}}
	store.AddAccount(account)
	previous := kiroTokenRefreshFunc
	t.Cleanup(func() { kiroTokenRefreshFunc = previous })

	var mu sync.Mutex
	spent := map[string]bool{}
	kiroTokenRefreshFunc = func(_ context.Context, _ KiroCredential, rt, _ string) (*KiroTokenData, error) {
		mu.Lock()
		defer mu.Unlock()
		if spent[rt] {
			return nil, &kiroRefreshInvalidError{message: "refresh token already used: " + rt}
		}
		spent[rt] = true
		time.Sleep(20 * time.Millisecond)
		return &KiroTokenData{AccessToken: "at-" + rt, RefreshToken: rt + "+", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- store.RefreshKiroAccount(context.Background(), account)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent refreshes must not spend the same rotated refresh token: %v", err)
		}
	}

	kiroTokenRefreshFunc = func(_ context.Context, _ KiroCredential, rt, _ string) (*KiroTokenData, error) {
		// 刷新进行中，另一个请求给账号设了限流冷却。
		store.MarkCooldown(account, time.Hour, "rate_limited")
		return &KiroTokenData{AccessToken: "at-new", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}
	if err := store.RefreshKiroAccount(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	if !account.HasActiveCooldown() {
		t.Fatal("a rate-limit cooldown set during refresh must survive the refresh")
	}
}

func TestKiroTransientRefreshFailureIsNotPermanent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"Service Unavailable"}`, http.StatusServiceUnavailable)
	}))
	defer server.Close()
	request, _ := http.NewRequest(http.MethodPost, server.URL, nil)
	_, err := doKiroTokenRequest(server.Client(), request, "test")
	if err == nil || IsPermanentRefreshFailure(err) {
		t.Fatalf("a 503 from the token endpoint is temporary, got %v", err)
	}
}
