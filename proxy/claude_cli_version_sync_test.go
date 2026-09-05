package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

func withClaudeVersionSources(t *testing.T, github, npm string) {
	t.Helper()
	claudeReleasesLatestURLForTest = github
	claudeNpmDistTagsURLForTest = npm
	t.Cleanup(func() {
		claudeReleasesLatestURLForTest = ""
		claudeNpmDistTagsURLForTest = ""
	})
}

func TestExtractClaudeCLIVersion(t *testing.T) {
	cases := map[string]string{"v2.1.258": "2.1.258", "2.1.258": "2.1.258", " V2.1.259 ": "2.1.259", "2.1.260-beta.1": "2.1.260", "rust-v0.1.0": "", "": "", "2.1": ""}
	for in, want := range cases {
		if got := extractClaudeCLIVersion(in); got != want {
			t.Errorf("extract(%q)=%q want %q", in, got, want)
		}
	}
}

func TestFetchLatestClaudeCLIVersion_PrefersGithub(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"name":"v2.1.258","tag_name":"v2.1.258"}`))
	}))
	defer gh.Close()
	npm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"latest":"2.1.999"}`))
	}))
	defer npm.Close()
	withClaudeVersionSources(t, gh.URL, npm.URL)
	got, err := FetchLatestClaudeCLIVersion(context.Background(), "")
	if err != nil || got != "2.1.258" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestFetchLatestClaudeCLIVersion_FallsBackToNpm(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusForbidden) }))
	defer gh.Close()
	npm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"stable":"2.1.236","latest":"2.1.258","next":"2.1.258"}`))
	}))
	defer npm.Close()
	withClaudeVersionSources(t, gh.URL, npm.URL)
	got, err := FetchLatestClaudeCLIVersion(context.Background(), "")
	if err != nil || got != "2.1.258" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestFetchLatestClaudeCLIVersion_BothFail(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	defer bad.Close()
	withClaudeVersionSources(t, bad.URL, bad.URL)
	if _, err := FetchLatestClaudeCLIVersion(context.Background(), ""); err == nil {
		t.Fatal("expected error when both sources fail")
	}
}

func TestSyncClaudeCLIVersion_RefreshesFingerprintsWithoutDB(t *testing.T) {
	t.Cleanup(func() { auth.SetClaudeSyncedCLIVersion("") })
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"name":"v2.1.300"}`))
	}))
	defer gh.Close()
	withClaudeVersionSources(t, gh.URL, gh.URL)
	store := auth.NewStore(nil, nil, nil)
	defer store.Stop()
	store.SetAccountsForTest([]*auth.Account{{DBID: 251, UpstreamType: auth.UpstreamClaude, CustomHeaders: map[string]string{"User-Agent": "claude-cli/2.1.219 (external, cli)"}}})

	result, err := SyncClaudeCLIVersion(context.Background(), nil, store, "")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Updated || result.EffectiveVersion != "2.1.300" || result.FetchedVersion != "2.1.300" || result.BuiltinVersion != auth.BuiltinClaudeCLIVersion {
		t.Fatalf("result = %+v", result)
	}
	if result.AccountsRefreshed != 1 {
		t.Fatalf("accounts_refreshed = %d", result.AccountsRefreshed)
	}
	if auth.EffectiveClaudeCLIVersion() != "2.1.300" {
		t.Fatal("runtime effective version must be published")
	}
}

func TestSyncClaudeCLIVersion_NeverDowngrades(t *testing.T) {
	t.Cleanup(func() { auth.SetClaudeSyncedCLIVersion("") })
	auth.SetClaudeSyncedCLIVersion("2.1.300")
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"name":"v2.1.259"}`))
	}))
	defer gh.Close()
	withClaudeVersionSources(t, gh.URL, gh.URL)
	store := auth.NewStore(nil, nil, nil)
	defer store.Stop()
	acc := &auth.Account{DBID: 260, UpstreamType: auth.UpstreamClaude, CustomHeaders: map[string]string{"User-Agent": "claude-cli/2.1.219 (external, cli)"}}
	store.SetAccountsForTest([]*auth.Account{acc})

	result, err := SyncClaudeCLIVersion(context.Background(), nil, store, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.Updated {
		t.Fatalf("must not update when fetched version is not higher: result = %+v", result)
	}
	if result.FetchedVersion != "2.1.259" {
		t.Fatalf("fetched_version = %q, want 2.1.259", result.FetchedVersion)
	}
	if result.EffectiveVersion != "2.1.300" {
		t.Fatalf("effective_version = %q, want 2.1.300 (must not regress)", result.EffectiveVersion)
	}
	if auth.EffectiveClaudeCLIVersion() != "2.1.300" {
		t.Fatal("runtime effective version must not regress")
	}
	if result.AccountsRefreshed != 1 {
		t.Fatalf("accounts_refreshed = %d, want 1 (refresh still runs against the effective version)", result.AccountsRefreshed)
	}
	if got := acc.CustomHeaders["User-Agent"]; got != "claude-cli/2.1.300 (external, cli)" {
		t.Fatalf("account User-Agent = %q, want claude-cli/2.1.300 (external, cli)", got)
	}
}

func TestSyncClaudeCLIVersion_PersistsToDatabase(t *testing.T) {
	t.Cleanup(func() { auth.SetClaudeSyncedCLIVersion("") })
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "sync.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()

	id, err := db.InsertAccountWithUpstream(ctx, "claude-a", "anthropic", "oauth", map[string]interface{}{
		"upstream_type":  "claude",
		"access_token":   "tok",
		"custom_headers": map[string]interface{}{"User-Agent": "claude-cli/2.1.219 (external, cli)"},
	}, "")
	if err != nil {
		t.Fatal(err)
	}

	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"name":"v2.1.300"}`))
	}))
	defer gh.Close()
	withClaudeVersionSources(t, gh.URL, gh.URL)

	store := auth.NewStore(nil, nil, nil)
	defer store.Stop()
	acc := &auth.Account{DBID: id, UpstreamType: auth.UpstreamClaude, CustomHeaders: map[string]string{"User-Agent": "claude-cli/2.1.219 (external, cli)"}}
	store.SetAccountsForTest([]*auth.Account{acc})

	result, err := SyncClaudeCLIVersion(ctx, db, store, "")
	if err != nil {
		t.Fatal(err)
	}
	if !result.Updated {
		t.Fatalf("expected update: result = %+v", result)
	}

	if got, err := db.GetClaudeSyncedCLIVersion(ctx); err != nil || got != "2.1.300" {
		t.Fatalf("persisted synced version = %q, %v", got, err)
	}

	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if headers := row.GetCredentialStringMap("custom_headers"); headers["User-Agent"] != "claude-cli/2.1.300 (external, cli)" {
		t.Fatalf("db custom_headers User-Agent = %v", headers)
	}
	if got := acc.CustomHeaders["User-Agent"]; got != "claude-cli/2.1.300 (external, cli)" {
		t.Fatalf("in-memory account User-Agent = %q", got)
	}
}

func TestClaudeCLIVersionSyncDisabled(t *testing.T) {
	cases := map[string]bool{
		"":      false,
		"0":     false,
		"false": false,
		"1":     true,
		"true":  true,
		"yes":   true,
		"on":    true,
		" ON ":  true,
	}
	for value, want := range cases {
		t.Run(value, func(t *testing.T) {
			t.Setenv("CLAUDE_DISABLE_CLI_VERSION_SYNC", value)
			if got := ClaudeCLIVersionSyncDisabled(); got != want {
				t.Errorf("ClaudeCLIVersionSyncDisabled() with env %q = %v, want %v", value, got, want)
			}
		})
	}
}
