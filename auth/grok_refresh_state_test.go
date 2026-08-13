package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	"github.com/codex2api/cache"
	"github.com/codex2api/database"
)

func newGrokRefreshStateFixture(t *testing.T, provider *httptest.Server) (*Store, *database.DB, int64) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "grok-refresh-state.db")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	providerURL, err := url.Parse(provider.URL)
	if err != nil {
		t.Fatalf("parse provider URL: %v", err)
	}
	t.Setenv(EnvGrokOAuthHostAllowlist, providerURL.Host)

	// RefreshGrokAccessToken 克隆 http.DefaultTransport;换成信任 httptest
	// 自签证书的 transport 才能真正打通交换。包内测试默认串行,可安全恢复。
	origTransport := http.DefaultTransport
	http.DefaultTransport = provider.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = origTransport })

	ctx := context.Background()
	accountID, err := db.InsertAccountWithUpstream(ctx, "grok-refresh-state", "xai", UpstreamGrok, map[string]interface{}{
		"upstream_type":       UpstreamGrok,
		"access_token":        "old-at",
		"refresh_token":       "old-rt",
		"expires_at":          time.Now().Add(time.Minute).UTC().Format(time.RFC3339),
		"grok_client_id":      "state-client",
		"grok_token_endpoint": provider.URL + "/oauth2/token",
		"grok_oidc_issuer":    provider.URL,
		"grok_principal_type": "user",
		"grok_principal_id":   "principal-state",
	}, "")
	if err != nil {
		t.Fatalf("insert account: %v", err)
	}

	store := NewStore(db, cache.NewMemory(16), &database.SystemSettings{MaxConcurrency: 2})
	t.Cleanup(store.Stop)
	if err := store.LoadAccountByID(ctx, accountID); err != nil {
		t.Fatalf("load account: %v", err)
	}
	return store, db, accountID
}

// 刷新等待/交换期间由其它 goroutine 设置的冷却是比入口快照更新的事实,
// 刷新成功发布时不得清零(AT 续期不代表上游限流解除)。
func TestRefreshGrokAccountKeepsCooldownSetDuringExchange(t *testing.T) {
	var store *Store
	var accountID int64
	cooldownUntil := time.Now().Add(5 * time.Minute)
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// 模拟交换在途时另一条请求路径把账号打入冷却。
		if account := store.FindByID(accountID); account != nil {
			account.mu.Lock()
			account.Status = StatusCooldown
			account.CooldownUtil = cooldownUntil
			account.CooldownReason = "upstream_429"
			account.mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"new-at","refresh_token":"new-rt","expires_in":3600}`))
	}))
	defer provider.Close()

	store, _, accountID = newGrokRefreshStateFixture(t, provider)
	if err := store.RefreshGrokAccountByID(context.Background(), accountID); err != nil {
		t.Fatalf("refresh: %v", err)
	}

	account := store.FindByID(accountID)
	account.mu.RLock()
	defer account.mu.RUnlock()
	if account.AccessToken != "new-at" || account.RefreshToken != "new-rt" {
		t.Fatalf("credentials not published: at=%q rt=%q", account.AccessToken, account.RefreshToken)
	}
	if account.Status != StatusCooldown || account.CooldownReason != "upstream_429" || !account.CooldownUtil.Equal(cooldownUntil) {
		t.Fatalf("cooldown set during exchange was overwritten: status=%v reason=%q until=%v",
			account.Status, account.CooldownReason, account.CooldownUtil)
	}
}

// 调用方 ctx 在交换在途时取消/超时,不得中断已开始的 RT 消费临界区:
// 上游已完成轮换,放弃等于把新 RT 永久丢失。
func TestRefreshGrokAccountSurvivesCallerCancelDuringExchange(t *testing.T) {
	provider := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(1 * time.Second) // 让调用方 ctx 在交换在途时先超时
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"rotated-at","refresh_token":"rotated-rt","expires_in":3600}`))
	}))
	defer provider.Close()

	store, db, accountID := newGrokRefreshStateFixture(t, provider)
	callerCtx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := store.RefreshGrokAccountByID(callerCtx, accountID); err != nil {
		t.Fatalf("refresh aborted by caller cancellation: %v", err)
	}

	row, err := db.GetAccountByID(context.Background(), accountID)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	if got := row.GetCredential("refresh_token"); got != "rotated-rt" {
		t.Fatalf("rotated RT not persisted, refresh_token=%q", got)
	}
	account := store.FindByID(accountID)
	account.mu.RLock()
	defer account.mu.RUnlock()
	if account.Status == StatusError {
		t.Fatalf("account marked error after caller cancel: %s", account.ErrorMsg)
	}
	if account.AccessToken != "rotated-at" || account.RefreshToken != "rotated-rt" {
		t.Fatalf("rotated credentials not published: at=%q rt=%q", account.AccessToken, account.RefreshToken)
	}
}
