package admin

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/codex2api/auth"
)

// resetAntigravityOAuthSettingsEnv 清空环境变量与包级配置态，避免用例间串扰。
func resetAntigravityOAuthSettingsEnv(t *testing.T) {
	t.Helper()
	t.Setenv("ANTIGRAVITY_OAUTH_CLIENTS", "")
	t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_KEY", "")
	auth.SetConfiguredAntigravityOAuth(auth.AntigravityOAuthSettings{})
	t.Cleanup(func() { auth.SetConfiguredAntigravityOAuth(auth.AntigravityOAuthSettings{}) })
}

func TestSettingsAntigravityOAuthUpdatePersistsAndKeepsSecret(t *testing.T) {
	resetAntigravityOAuthSettingsEnv(t)
	handler, db, _ := newResponseCacheSettingsAdminHandler(t)

	update := invokeResponseCacheSettingsAdmin(t, handler, http.MethodPut, map[string]any{
		"antigravity_oauth_clients": []map[string]any{
			{"key": " Primary ", "client_id": "id-1", "client_secret": "secret-1"},
		},
		"antigravity_oauth_client_key": "PRIMARY",
	})
	if update.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body=%s", update.Code, update.Body.String())
	}
	if strings.Contains(update.Body.String(), "secret-1") {
		t.Fatalf("settings response leaked client_secret: %s", update.Body.String())
	}
	response := decodeResponseCacheSettingsResponse(t, update)
	if len(response.AntigravityOAuthClients) != 1 ||
		response.AntigravityOAuthClients[0].Key != "primary" ||
		response.AntigravityOAuthClients[0].ClientID != "id-1" ||
		!response.AntigravityOAuthClients[0].HasSecret ||
		response.AntigravityOAuthClientKey != "primary" ||
		response.AntigravityOAuthActiveKeyEffective != "primary" {
		t.Fatalf("settings view = %+v", response.antigravityOAuthSettingsView)
	}

	raw, err := db.LoadAntigravityOAuthConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := auth.ParseAntigravityOAuthSettings(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted.Clients) != 1 || persisted.Clients[0].ClientSecret != "secret-1" || persisted.ActiveKey != "primary" {
		t.Fatalf("persisted = %+v", persisted)
	}

	// 编辑同 key 条目但不重填 secret：client_id 更新、secret 沿用已保存值。
	keep := invokeResponseCacheSettingsAdmin(t, handler, http.MethodPut, map[string]any{
		"antigravity_oauth_clients": []map[string]any{
			{"key": "primary", "client_id": "id-2", "client_secret": ""},
		},
	})
	if keep.Code != http.StatusOK {
		t.Fatalf("keep-secret PUT status = %d, body=%s", keep.Code, keep.Body.String())
	}
	raw, err = db.LoadAntigravityOAuthConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	persisted, err = auth.ParseAntigravityOAuthSettings(raw)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Clients[0].ClientID != "id-2" || persisted.Clients[0].ClientSecret != "secret-1" {
		t.Fatalf("keep-secret persisted = %+v", persisted)
	}
	if persisted.ActiveKey != "primary" {
		t.Fatalf("active key = %q, want retained primary", persisted.ActiveKey)
	}

	// 热更新可直接支撑授权 URL 构建（原 env 独占路径）。
	client, err := auth.NewAntigravityClient("")
	if err != nil {
		t.Fatal(err)
	}
	_, info, err := client.BuildOAuthAuthorizationURL(
		"http://127.0.0.1:43123/oauth-callback", "state-123", "challenge-456", "",
	)
	if err != nil {
		t.Fatalf("BuildOAuthAuthorizationURL() error: %v", err)
	}
	if info.Key != "primary" || info.ClientID != "id-2" {
		t.Fatalf("authorization client = %+v", info)
	}
}

func TestSettingsAntigravityOAuthUpdateRejectsInvalidPayload(t *testing.T) {
	resetAntigravityOAuthSettingsEnv(t)
	handler, _, _ := newResponseCacheSettingsAdminHandler(t)

	// 新增条目缺 secret：既没有已保存值可沿用，也不能落库空 secret。
	missingSecret := invokeResponseCacheSettingsAdmin(t, handler, http.MethodPut, map[string]any{
		"antigravity_oauth_clients": []map[string]any{
			{"key": "fresh", "client_id": "id", "client_secret": ""},
		},
	})
	if missingSecret.Code != http.StatusBadRequest {
		t.Fatalf("missing secret status = %d, body=%s", missingSecret.Code, missingSecret.Body.String())
	}

	unknownActive := invokeResponseCacheSettingsAdmin(t, handler, http.MethodPut, map[string]any{
		"antigravity_oauth_clients": []map[string]any{
			{"key": "k", "client_id": "id", "client_secret": "secret"},
		},
		"antigravity_oauth_client_key": "missing",
	})
	if unknownActive.Code != http.StatusBadRequest {
		t.Fatalf("unknown active key status = %d, body=%s", unknownActive.Code, unknownActive.Body.String())
	}
}

func TestSettingsAntigravityOAuthGetExposesEnvClientsReadOnly(t *testing.T) {
	resetAntigravityOAuthSettingsEnv(t)
	t.Setenv("ANTIGRAVITY_OAUTH_CLIENTS", "envkey|env-id|env-secret")
	handler, _, _ := newResponseCacheSettingsAdminHandler(t)

	get := invokeResponseCacheSettingsAdmin(t, handler, http.MethodGet, nil)
	if get.Code != http.StatusOK {
		t.Fatalf("GET status = %d, body=%s", get.Code, get.Body.String())
	}
	if strings.Contains(get.Body.String(), "env-secret") {
		t.Fatalf("settings response leaked env client_secret: %s", get.Body.String())
	}
	response := decodeResponseCacheSettingsResponse(t, get)
	if len(response.AntigravityOAuthEnvClients) != 1 ||
		response.AntigravityOAuthEnvClients[0].Key != "envkey" ||
		response.AntigravityOAuthEnvClients[0].ClientID != "env-id" {
		t.Fatalf("env clients = %+v", response.AntigravityOAuthEnvClients)
	}
	if response.AntigravityOAuthActiveKeyEffective != "envkey" {
		t.Fatalf("effective active key = %q", response.AntigravityOAuthActiveKeyEffective)
	}
	if response.AntigravityOAuthUsingBuiltin {
		t.Fatal("using_builtin = true, want false when env clients exist")
	}
}

func TestSettingsAntigravityOAuthGetUsesBuiltinWhenEmpty(t *testing.T) {
	resetAntigravityOAuthSettingsEnv(t)
	handler, _, _ := newResponseCacheSettingsAdminHandler(t)

	get := invokeResponseCacheSettingsAdmin(t, handler, http.MethodGet, nil)
	if get.Code != http.StatusOK {
		t.Fatalf("GET status = %d, body=%s", get.Code, get.Body.String())
	}
	if strings.Contains(get.Body.String(), auth.AntigravityDefaultOAuthClientSecret) {
		t.Fatalf("settings response leaked built-in client_secret: %s", get.Body.String())
	}
	response := decodeResponseCacheSettingsResponse(t, get)
	if !response.AntigravityOAuthUsingBuiltin {
		t.Fatal("using_builtin = false, want true when nothing is configured")
	}
	if response.AntigravityOAuthActiveKeyEffective != auth.AntigravityDefaultOAuthClientKey {
		t.Fatalf("effective active key = %q, want official", response.AntigravityOAuthActiveKeyEffective)
	}
	if response.AntigravityOAuthBuiltinClient.Key != auth.AntigravityDefaultOAuthClientKey ||
		response.AntigravityOAuthBuiltinClient.ClientID != auth.AntigravityDefaultOAuthClientID {
		t.Fatalf("builtin client = %+v", response.AntigravityOAuthBuiltinClient)
	}
}
