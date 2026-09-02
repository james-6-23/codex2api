package auth

import (
	"strings"
	"testing"
)

func TestParseAntigravityOAuthSettingsEmptyAndRoundTrip(t *testing.T) {
	for _, raw := range []string{"", "   ", "{}"} {
		settings, err := ParseAntigravityOAuthSettings(raw)
		if err != nil || len(settings.Clients) != 0 || settings.ActiveKey != "" {
			t.Fatalf("ParseAntigravityOAuthSettings(%q) = %#v, %v", raw, settings, err)
		}
	}

	source := AntigravityOAuthSettings{
		Clients: []AntigravityOAuthClientConfig{
			{Key: " Primary ", ClientID: "id-1", ClientSecret: "secret-1"},
			{Key: "backup", ClientID: "id-2", ClientSecret: "secret-2"},
		},
		ActiveKey: "BACKUP",
	}
	encoded, err := EncodeAntigravityOAuthSettings(source)
	if err != nil {
		t.Fatalf("EncodeAntigravityOAuthSettings() error: %v", err)
	}
	decoded, err := ParseAntigravityOAuthSettings(encoded)
	if err != nil {
		t.Fatalf("ParseAntigravityOAuthSettings() error: %v", err)
	}
	if len(decoded.Clients) != 2 || decoded.Clients[0].Key != "primary" || decoded.ActiveKey != "backup" {
		t.Fatalf("round trip = %#v", decoded)
	}

	if empty, err := EncodeAntigravityOAuthSettings(AntigravityOAuthSettings{}); err != nil || empty != "{}" {
		t.Fatalf("empty encode = %q, %v", empty, err)
	}
}

func TestNormalizeAntigravityOAuthSettingsRejectsInvalidInput(t *testing.T) {
	cases := []struct {
		name     string
		settings AntigravityOAuthSettings
		want     string
	}{
		{
			name:     "empty key",
			settings: AntigravityOAuthSettings{Clients: []AntigravityOAuthClientConfig{{Key: " ", ClientID: "id", ClientSecret: "secret"}}},
			want:     "empty key",
		},
		{
			name:     "separator in key",
			settings: AntigravityOAuthSettings{Clients: []AntigravityOAuthClientConfig{{Key: "a|b", ClientID: "id", ClientSecret: "secret"}}},
			want:     "must not contain '|' or ';'",
		},
		{
			name:     "empty client_id",
			settings: AntigravityOAuthSettings{Clients: []AntigravityOAuthClientConfig{{Key: "k", ClientID: "", ClientSecret: "secret"}}},
			want:     "empty client_id",
		},
		{
			name:     "empty client_secret",
			settings: AntigravityOAuthSettings{Clients: []AntigravityOAuthClientConfig{{Key: "k", ClientID: "id", ClientSecret: ""}}},
			want:     "empty client_secret",
		},
		{
			name: "duplicate key",
			settings: AntigravityOAuthSettings{Clients: []AntigravityOAuthClientConfig{
				{Key: "k", ClientID: "id", ClientSecret: "secret"},
				{Key: "K", ClientID: "id2", ClientSecret: "secret2"},
			}},
			want: "duplicated",
		},
		{
			name: "unknown active key",
			settings: AntigravityOAuthSettings{
				Clients:   []AntigravityOAuthClientConfig{{Key: "k", ClientID: "id", ClientSecret: "secret"}},
				ActiveKey: "missing",
			},
			want: "does not match any configured client",
		},
		{
			name:     "control character in client_id",
			settings: AntigravityOAuthSettings{Clients: []AntigravityOAuthClientConfig{{Key: "k", ClientID: "id\x00", ClientSecret: "secret"}}},
			want:     "client_id",
		},
	}
	for _, tc := range cases {
		if _, err := NormalizeAntigravityOAuthSettings(tc.settings); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: err = %v, want contains %q", tc.name, err, tc.want)
		}
	}
}

func TestEffectiveAntigravityOAuthClientsMergesEnvAndConfigured(t *testing.T) {
	t.Setenv(antigravityOAuthClientsEnv, "shared|env-id|env-secret;envonly|env-id-2|env-secret-2")
	t.Setenv(antigravityActiveOAuthClientEnv, "")
	resetConfiguredAntigravityOAuth(t)
	SetConfiguredAntigravityOAuth(AntigravityOAuthSettings{
		Clients: []AntigravityOAuthClientConfig{
			{Key: "shared", ClientID: "db-id", ClientSecret: "db-secret"},
			{Key: "dbonly", ClientID: "db-id-2", ClientSecret: "db-secret-2"},
		},
		ActiveKey: "dbonly",
	})

	clients, active := effectiveAntigravityOAuthClients()
	if len(clients) != 3 {
		t.Fatalf("clients = %#v", clients)
	}
	// 同 key 冲突时环境变量条目生效。
	if clients[0].Key != "shared" || clients[0].ClientID != "env-id" {
		t.Fatalf("shared client = %#v, want env entry to win", clients[0])
	}
	if clients[2].Key != "dbonly" || clients[2].ClientSecret != "db-secret-2" {
		t.Fatalf("db client = %#v", clients[2])
	}
	// 环境变量未指定活跃 key 时以系统设置为准。
	if active != "dbonly" {
		t.Fatalf("active = %q, want dbonly", active)
	}

	t.Setenv(antigravityActiveOAuthClientEnv, "envonly")
	if _, active = effectiveAntigravityOAuthClients(); active != "envonly" {
		t.Fatalf("active = %q, want env override envonly", active)
	}
}

func TestEffectiveAntigravityOAuthClientsConfiguredOnly(t *testing.T) {
	t.Setenv(antigravityOAuthClientsEnv, "")
	t.Setenv(antigravityActiveOAuthClientEnv, "")
	resetConfiguredAntigravityOAuth(t)
	SetConfiguredAntigravityOAuth(AntigravityOAuthSettings{
		Clients: []AntigravityOAuthClientConfig{{Key: "primary", ClientID: "id", ClientSecret: "secret"}},
	})

	clients, active := effectiveAntigravityOAuthClients()
	if len(clients) != 1 || clients[0].ClientID != "id" || active != "primary" {
		t.Fatalf("clients/active = %#v/%q", clients, active)
	}
	if UsingBuiltinAntigravityOAuth() {
		t.Fatal("UsingBuiltinAntigravityOAuth() = true, want false when settings exist")
	}

	// 系统设置里的配置要能直接支撑授权 URL 构建（原来 env 独占的路径）。
	client := newAntigravityClient(nil, AntigravityEndpoints{})
	gotURL, info, err := client.BuildOAuthAuthorizationURL(
		"http://127.0.0.1:43123/oauth-callback", "state-123", "challenge-456", "",
	)
	if err != nil {
		t.Fatalf("BuildOAuthAuthorizationURL() error: %v", err)
	}
	if info.Key != "primary" || info.ClientID != "id" || !strings.Contains(gotURL, "client_id=id") {
		t.Fatalf("info = %+v url = %s", info, gotURL)
	}
}
