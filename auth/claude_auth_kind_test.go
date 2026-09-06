package auth

import (
	"net/url"
	"strings"
	"testing"
)

func TestNormalizeAndInferClaudeAuthKind(t *testing.T) {
	if got := NormalizeClaudeAuthKind("", true); got != ClaudeAuthKindOAuth {
		t.Fatalf("undeclared with RT = %q, want oauth", got)
	}
	if got := NormalizeClaudeAuthKind("", false); got != ClaudeAuthKindOAuth {
		t.Fatalf("undeclared without RT must stay oauth (legacy default), got %q", got)
	}
	if got := NormalizeClaudeAuthKind(" Setup_Token ", true); got != ClaudeAuthKindSetupToken {
		t.Fatalf("explicit setup_token = %q", got)
	}
	if got := InferClaudeAuthKind("", "sk-ant-oat01-abcdef", ""); got != ClaudeAuthKindSetupToken {
		t.Fatalf("setup-token shaped AT without RT should infer setup_token, got %q", got)
	}
	if got := InferClaudeAuthKind("", "sk-ant-oat01-abcdef", "rt"); got != ClaudeAuthKindOAuth {
		t.Fatalf("RT present must win over token shape, got %q", got)
	}
	if got := InferClaudeAuthKind("oauth", "sk-ant-oat01-abcdef", ""); got != ClaudeAuthKindOAuth {
		t.Fatalf("explicit oauth must win over token shape, got %q", got)
	}
	if !IsValidClaudeAuthKind("") || !IsValidClaudeAuthKind("oauth") || !IsValidClaudeAuthKind("setup_token") || !IsValidClaudeAuthKind("api_key") || IsValidClaudeAuthKind("invalid") {
		t.Fatal("IsValidClaudeAuthKind vocabulary drifted")
	}
}

func TestExtractClaudeSetupTokens(t *testing.T) {
	text := "第一个 sk-ant-oat01-AAA_bbb-CCC, sk-ant-oat01-AAA_bbb-CCC\nsk-ant-oat01-second;sk-ant-oat01-\n\"sk-ant-oat01-third\""
	got := ExtractClaudeSetupTokens(text)
	want := []string{"sk-ant-oat01-AAA_bbb-CCC", "sk-ant-oat01-second", "sk-ant-oat01-third"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("ExtractClaudeSetupTokens = %v, want %v", got, want)
	}
	if len(ExtractClaudeSetupTokens("nothing here")) != 0 {
		t.Fatal("text without tokens must yield none")
	}
	mixed := "sk-ant-oat01-setup sk-ant-ort01-refreshA,sk-ant-ort01-refreshA\nsk-ant-ort01-refreshB"
	if got := ExtractClaudeSetupTokens(mixed); len(got) != 1 || got[0] != "sk-ant-oat01-setup" {
		t.Fatalf("setup tokens from mixed text = %v", got)
	}
	if got := ExtractClaudeRefreshTokens(mixed); strings.Join(got, "|") != "sk-ant-ort01-refreshA|sk-ant-ort01-refreshB" {
		t.Fatalf("refresh tokens from mixed text = %v", got)
	}
	if !LooksLikeClaudeRefreshToken("sk-ant-ort01-x") || LooksLikeClaudeRefreshToken("sk-ant-oat01-x") {
		t.Fatal("LooksLikeClaudeRefreshToken prefix check drifted")
	}
}

func TestAccountClaudeSetupTokenDetection(t *testing.T) {
	explicit := &Account{UpstreamType: UpstreamClaude, AccessToken: "at", ClaudeAuthKind: ClaudeAuthKindSetupToken}
	if !explicit.IsClaudeSetupToken() || explicit.EffectiveClaudeAuthKind() != ClaudeAuthKindSetupToken {
		t.Fatal("explicit setup_token account not detected")
	}
	legacy := &Account{UpstreamType: UpstreamClaude, AccessToken: "at", RefreshToken: "rt"}
	if legacy.IsClaudeSetupToken() || legacy.EffectiveClaudeAuthKind() != ClaudeAuthKindOAuth {
		t.Fatal("legacy OAuth account misclassified")
	}
	shaped := &Account{UpstreamType: UpstreamClaude, AccessToken: "sk-ant-oat01-xyz"}
	if !shaped.IsClaudeSetupToken() {
		t.Fatal("setup-token shaped AT without RT should be detected")
	}
	codex := &Account{AccessToken: "sk-ant-oat01-xyz"}
	if codex.IsClaudeSetupToken() || codex.EffectiveClaudeAuthKind() != "" {
		t.Fatal("non-Claude account must never be a Claude setup token")
	}
}

func TestStartClaudeLoginSetupTokenOptions(t *testing.T) {
	s, err := StartClaudeLoginWithOptions(ClaudeLoginOptions{SetupToken: true})
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(s.AuthURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("scope") != ClaudeOAuthScopeInference {
		t.Fatalf("setup token scope = %q", q.Get("scope"))
	}
	if q.Get("redirect_uri") != ClaudeOAuthRedirectURI || s.RedirectURI != ClaudeOAuthRedirectURI {
		t.Fatalf("redirect_uri = %q / %q", q.Get("redirect_uri"), s.RedirectURI)
	}
	if !s.SetupToken || s.Scope != ClaudeOAuthScopeInference {
		t.Fatalf("session = %+v", s)
	}
	opts := s.ExchangeOptions()
	if !opts.SetupToken || opts.RedirectURI != ClaudeOAuthRedirectURI {
		t.Fatalf("exchange options = %+v", opts)
	}
	legacy := &ClaudeLoginSession{State: "s", Verifier: "v"}
	if got := legacy.ExchangeOptions().RedirectURI; got != ClaudeOAuthLocalRedirectURI {
		t.Fatalf("legacy session must fall back to local redirect, got %q", got)
	}
}

func TestParseClaudeCodeAndStateAcceptsCallbackURL(t *testing.T) {
	code, state := parseClaudeCodeAndState("https://platform.claude.com/oauth/code/callback?code=abc123&state=st-9")
	if code != "abc123" || state != "st-9" {
		t.Fatalf("url form parsed = (%q,%q)", code, state)
	}
	code, state = parseClaudeCodeAndState("http://localhost:54545/callback?code=only")
	if code != "only" || state != "" {
		t.Fatalf("url without state = (%q,%q)", code, state)
	}
}
