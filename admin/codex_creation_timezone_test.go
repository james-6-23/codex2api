package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/gin-gonic/gin"
)

func TestCodexCreationTimezone(t *testing.T) {
	identity := `{"agent_identity":{"agent_runtime_id":"agent-timezone","agent_private_key":"` + newTestAgentPrivateKey(t) + `","account_id":"acc-timezone","chatgpt_user_id":"user-timezone"}}`
	for _, tc := range []struct {
		name    string
		path    string
		handler func(*Handler, *gin.Context)
		body    map[string]any
		want    int
	}{
		{"refresh token", "/accounts", (*Handler).AddAccount, map[string]any{"refresh_token": "tz-rt", "skip_refresh": true}, 1},
		{"session token batch", "/accounts?stream=true", (*Handler).AddAccount, map[string]any{"session_token": "tz-st-1\ntz-st-2", "skip_refresh": true}, 2},
		{"access token", "/accounts/at", (*Handler).AddATAccount, map[string]any{"access_token": "tz-at"}, 1},
		{"access token batch", "/accounts/at?stream=true", (*Handler).AddATAccount, map[string]any{"access_token": "tz-at-1\ntz-at-2"}, 2},
		{"agent identity", "/accounts/codex/agent-identity", (*Handler).ImportCodexAgentIdentity, map[string]any{"auth_json": identity}, 1},
		{"agent identity files", "/accounts/codex/agent-identity/import", (*Handler).BatchImportCodexAgentIdentity, map[string]any{"files": []string{identity}}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newTestAdminDB(t)
			store := auth.NewStore(db, cache.NewMemory(1), nil)
			t.Cleanup(store.Stop)
			h := &Handler{db: db, store: store, probeUsage: func(context.Context, *auth.Account) error { return nil }}
			submit := func(zone string) int {
				tc.body["timezone"] = zone
				body, err := json.Marshal(tc.body)
				if err != nil {
					t.Fatal(err)
				}
				return invokeAccountGroupHandler(t, http.MethodPost, "/api/admin"+tc.path, nil, string(body), func(c *gin.Context) { tc.handler(h, c) }).Code
			}
			if code := submit("Local"); code != http.StatusBadRequest {
				t.Fatalf("invalid timezone returned %d", code)
			}
			if n, err := db.CountAll(context.Background()); err != nil || n != 0 {
				t.Fatalf("invalid timezone created accounts: count=%d err=%v", n, err)
			}
			if code := submit(" Asia/Tokyo "); code != http.StatusOK {
				t.Fatalf("create returned %d", code)
			}
			rows, err := db.ListActive(context.Background())
			if err != nil || len(rows) != tc.want {
				t.Fatalf("created %d accounts, want %d, err=%v", len(rows), tc.want, err)
			}
			for _, row := range rows {
				if got := row.GetCredential(auth.AccountTimezoneCredentialKey); got != "Asia/Tokyo" {
					t.Errorf("stored timezone = %q", got)
				}
				if got := store.FindByID(row.ID).EffectiveCodexTimezone(); got != "Asia/Tokyo" {
					t.Errorf("initial runtime timezone = %q", got)
				}
			}
		})
	}
}
