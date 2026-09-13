package proxy

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Reopen the database and construct a fresh Store, then switch upstream accounts
// within one request context. This exercises startup reload and egress-keyed
// snapshots together, without probing real proxies or changing live settings.
func TestCodexEnvironmentRestartAndAccountSwitch(t *testing.T) {
	previousResin := GetResinConfig()
	SetResinConfig(nil)
	t.Cleanup(func() { SetResinConfig(previousResin) })
	path := filepath.Join(t.TempDir(), "environment-restart.db")
	for restart := 0; restart < 2; restart++ {
		func() {
			db, err := database.New("sqlite", path)
			require.NoError(t, err)
			defer db.Close()
			zones := []string{"America/Los_Angeles", "Asia/Tokyo"}
			proxies := []string{"http://timezone-a.invalid:8080", "http://timezone-b.invalid:8080"}
			if restart == 0 {
				for i, url := range proxies {
					id, err := db.InsertProxy(t.Context(), url, "timezone fixture")
					require.NoError(t, err)
					require.NoError(t, db.UpdateProxyTestResult(t.Context(), id, url, database.ProxyTestStatusSuccess, "192.0.2.1", "", 1, zones[i]))
				}
			}
			memory := cache.NewMemory(4)
			defer memory.Close()
			store := auth.NewStore(db, memory, &database.SystemSettings{})
			defer store.Stop()
			reference := time.Date(2026, 9, 6, 0, 30, 0, 0, time.UTC)
			ctx := WithCodexEnvironment(context.Background(), store.ProxyTimezone, reference)
			body := environmentTestBody(environmentTestText("2026-09-06", "Asia/Shanghai"))
			for i, url := range proxies {
				account := &auth.Account{DBID: int64(89001 + i), AccessToken: "fixture", ProxyURL: url, CodexFingerprintMode: auth.CodexFingerprintModeOff}
				location := store.ProxyTimezone(url)
				require.NotNil(t, location, "startup must restore saved proxy timezone")
				require.Equal(t, zones[i], location.String())
				var received []byte
				entry := &poolEntry{client: &http.Client{Transport: environmentTestTransport{received: &received}}}
				entry.touch()
				key := clientPoolKey(account, url, codexTransportModeFromEnv())
				clientPool.Store(key, entry)
				defer clientPool.Delete(key)
				response, err := ExecuteRequest(ctx, account, body, "timezone-fixture", "", "fixture-key", nil, http.Header{}, false)
				require.NoError(t, err)
				require.NoError(t, response.Body.Close())
				require.Equal(t, environmentTestText(reference.In(location).Format(time.DateOnly), zones[i]), gjson.GetBytes(received, "input.0.content.0.text").String(), "restart=%d account=%d", restart, i)
			}
		}()
	}
}
