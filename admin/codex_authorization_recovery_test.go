package admin

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
)

func TestNativeCodexAuthorizationRecoveryThroughUsageProbe(t *testing.T) {
	const (
		oldDetail = "original 401"
		completed = "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
	)
	for _, tc := range []struct {
		name                     string
		refresh, restore, paused bool
		whamOnly, whamOK         bool
		responseStatus           int
	}{
		{name: "rotated AT remains quarantined until completed Responses", refresh: true, restore: true, responseStatus: 200},
		{name: "handled Responses 401 persists another cooldown", refresh: true, responseStatus: 401},
		{name: "unrelated success does not certify a handled 429", responseStatus: 429},
		{name: "new runtime 401 wins", responseStatus: 200},
		{name: "new persisted 401 wins", responseStatus: 200},
		{name: "new durable generation wins", refresh: true, responseStatus: 200},
		{name: "new runtime generation wins", responseStatus: 200},
		{name: "new durable AT only bearer wins", responseStatus: 200},
		{name: "AT only falls back from ambiguous WHAM 401", restore: true, responseStatus: 200},
		{name: "AT only WHAM proof with fallback disabled", restore: true, whamOnly: true, whamOK: true},
		{name: "AT only ambiguous WHAM 401 with fallback disabled", whamOnly: true},
		{name: "paused native account verifies but remains unschedulable", restore: true, paused: true, responseStatus: 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			db, err := database.New("sqlite", filepath.Join(t.TempDir(), "recovery.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			now := time.Now().UTC()
			// Keep sub-second precision, just like a cooldown written by a live 401.
			deadline := now.Add(-time.Hour)
			credentials := map[string]any{"access_token": "initial-at", "expires_at": now.Add(time.Hour).Format(time.RFC3339)}
			wantAT := "initial-at"
			if tc.refresh {
				credentials["refresh_token"] = "initial-rt"
				credentials["expires_at"] = now.Add(-time.Hour).Format(time.RFC3339)
				wantAT = "rotated-at"
			}
			id, err := db.InsertAccountWithCredentials(ctx, "recovery-test", credentials, "")
			if err != nil {
				t.Fatal(err)
			}
			if err := db.SetCooldownWithError(ctx, id, "unauthorized", deadline, oldDetail); err != nil {
				t.Fatal(err)
			}
			if tc.paused {
				if err := db.SetAccountEnabled(ctx, id, false); err != nil {
					t.Fatal(err)
				}
			}
			store := auth.NewStore(db, nil, nil)
			t.Cleanup(store.Stop)
			store.SetUsageProbeResponsesFallbackEnabled(!tc.whamOnly)
			if err := store.LoadAccountByID(ctx, id); err != nil {
				t.Fatal(err)
			}
			account := store.FindByID(id)
			if !account.NeedsRecoveryProbe(0) || account.IsAvailable() {
				t.Fatal("reloaded account must be quarantined but eligible for maintenance")
			}
			if tc.paused {
				atomic.StoreInt32(&account.IntelligencePaused, 1)
			}
			originalGeneration := account.GetCredentialGeneration()
			wantGeneration := originalGeneration
			if tc.refresh {
				wantGeneration++
			}
			var oauthCalls, whamCalls, responsesCalls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/oauth":
					oauthCalls.Add(1)
					_, _ = io.WriteString(w, `{"access_token":"rotated-at","refresh_token":"rotated-rt","expires_in":3600}`)
				case "/wham":
					whamCalls.Add(1)
					if r.Header.Get("Authorization") != "Bearer "+wantAT {
						t.Error("WHAM did not use the expected bearer")
					}
					if tc.whamOK {
						_, _ = io.WriteString(w, `{"rate_limit":{"allowed":true,"limit_reached":false}}`)
						return
					}
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = io.WriteString(w, `{"detail":"missing workspace claim"}`)
				default:
					t.Error("unexpected upstream path", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			t.Cleanup(upstream.Close)
			previous := auth.ResinRequestDecorator
			auth.ResinRequestDecorator = func(string, string) string { return upstream.URL + "/oauth" }
			t.Cleanup(func() { auth.ResinRequestDecorator = previous })
			t.Cleanup(proxy.SetWhamUsageURLForTest(upstream.URL + "/wham"))
			h := &Handler{store: store, db: db, executeUsageProbe: func(ctx context.Context, acc *auth.Account, _ []byte, _, _, _ string, _ *proxy.DeviceProfileConfig, _ http.Header, _ ...bool) (*http.Response, error) {
				responsesCalls.Add(1)
				// This boundary is after credential publication and before the probe produces a result.
				row, err := db.GetAccountByID(ctx, id)
				if err != nil || row.CredentialGeneration != wantGeneration || row.GetCredential("access_token") != wantAT || row.CooldownReason != "unauthorized" || !row.CooldownUntil.Time.Equal(deadline) || row.ErrorMessage != oldDetail {
					t.Error("credential publication changed or failed to retain the durable 401", err)
				}
				if acc != account || acc.GetAccessToken() != wantAT || acc.IsAvailable() || acc.RuntimeStatus() != "unauthorized" {
					t.Error("account was not quarantined with the expected bearer before probe completion")
				}
				if selected := store.Next(); selected != nil {
					store.Release(selected)
					t.Error("scheduler selected account before probe completion")
				}
				body := completed
				switch tc.responseStatus {
				case http.StatusUnauthorized:
					body = `{"error":{"code":"invalid_token"}}`
				case http.StatusTooManyRequests:
					body = `{"error":{"type":"usage_limit_reached"}}`
				}
				switch tc.name {
				case "unrelated success does not certify a handled 429":
					store.ReportRequestSuccess(acc, 0)
				case "new runtime 401 wins":
					store.MarkCooldownWithError(acc, 24*time.Hour, "unauthorized", "new runtime 401")
				case "new persisted 401 wins":
					if err := db.SetCooldownWithError(ctx, id, "unauthorized", now.Add(time.Hour), oldDetail); err != nil {
						t.Error(err)
					}
				case "new durable generation wins", "new durable AT only bearer wins":
					if err := db.UpdateCredentials(ctx, id, map[string]any{"access_token": "replacement-at"}); err != nil {
						t.Error(err)
					}
				case "new runtime generation wins":
					acc.Mu().Lock()
					acc.CredentialGeneration++
					acc.Mu().Unlock()
				}
				return &http.Response{StatusCode: tc.responseStatus, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			}}
			store.SetUsageProbeFunc(h.ProbeUsageSnapshot)
			store.TriggerRecoveryProbeAsync()
			if !db.DrainBackgroundTasks(5 * time.Second) {
				t.Fatal("recovery probe did not finish")
			}
			if (oauthCalls.Load() == 1) != tc.refresh || whamCalls.Load() != 1 || (responsesCalls.Load() == 1) != (tc.responseStatus != 0) {
				t.Fatalf("unexpected upstream calls: OAuth=%d WHAM=%d Responses=%d", oauthCalls.Load(), whamCalls.Load(), responsesCalls.Load())
			}
			row, err := db.GetAccountByID(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			wantAvailable := tc.restore && !tc.paused
			if account.IsAvailable() != wantAvailable || row.Enabled == tc.paused {
				t.Fatalf("available=%t (want %t), enabled=%t", account.IsAvailable(), wantAvailable, row.Enabled)
			}
			if tc.paused && (atomic.LoadInt32(&account.DispatchPaused) != 1 || atomic.LoadInt32(&account.IntelligencePaused) != 1) {
				t.Fatal("authorization recovery cleared a dispatch pause")
			}
			if tc.restore {
				if row.CooldownReason != "" || row.CooldownUntil.Valid || row.ErrorMessage != "" || account.GetCooldownReason() != "" {
					t.Fatal("explicit success did not clear the original 401")
				}
			} else if row.CooldownReason != "unauthorized" || !row.CooldownUntil.Valid || row.ErrorMessage == "" || account.GetCooldownReason() != "unauthorized" {
				t.Fatal("unproven or stale result erased the 401")
			}
			if tc.responseStatus == http.StatusUnauthorized && (!row.CooldownUntil.Time.After(now) || row.ErrorMessage == oldDetail) {
				t.Fatal("handled Responses 401 did not persist its new rejection")
			}
			if tc.name == "new durable generation wins" && row.CredentialGeneration != wantGeneration+1 || tc.name == "new durable AT only bearer wins" && row.CredentialGeneration != wantGeneration {
				t.Fatal("credential replacement did not exercise the intended persistence fence")
			}
			reloaded := auth.NewStore(db, nil, nil)
			t.Cleanup(reloaded.Stop)
			if err := reloaded.LoadAccountByID(ctx, id); err != nil {
				t.Fatal(err)
			}
			if restored := reloaded.FindByID(id); restored.IsAvailable() != wantAvailable || restored.GetCooldownReason() != row.CooldownReason {
				t.Fatal("authorization or pause state did not survive reload")
			}
		})
	}
}
