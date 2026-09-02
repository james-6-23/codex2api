package admin

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func exportRequestContext(t *testing.T, target string) *gin.Context {
	t.Helper()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodGet, target, nil)
	return c
}

// include_proxy 缺省时取渠道默认值,显式传入时压过默认值——Antigravity 默认导出
// 代理(保持既有往返能力),Codex/Grok 默认不导出(代理 URL 常含明文口令)。
func TestExportIncludeProxyParsing(t *testing.T) {
	cases := []struct {
		target       string
		defaultValue bool
		want         bool
	}{
		{"/api/admin/accounts/export", false, false},
		{"/api/admin/accounts/export", true, true},
		{"/api/admin/accounts/export?include_proxy=1", false, true},
		{"/api/admin/accounts/export?include_proxy=true", false, true},
		{"/api/admin/accounts/export?include_proxy=0", true, false},
		{"/api/admin/accounts/export?include_proxy=false", true, false},
		// 空值视为存在且为假,才能让前端用 include_proxy= 关掉默认开启的渠道。
		{"/api/admin/accounts/export?include_proxy=", true, false},
	}
	for _, tc := range cases {
		if got := exportIncludeProxy(exportRequestContext(t, tc.target), tc.defaultValue); got != tc.want {
			t.Fatalf("exportIncludeProxy(%q, default=%v) = %v, want %v", tc.target, tc.defaultValue, got, tc.want)
		}
	}
}

// 关闭时导出条目一个代理字段都不带,连账号行上已有的 ProxyURL 也不写出。
func TestExportProxyResolverDisabledDropsEverything(t *testing.T) {
	resolver := exportProxyResolver{}
	url, label, enabled := resolver.resolve("http://user:pass@127.0.0.1:8080")
	if url != "" || label != "" || enabled != nil {
		t.Fatalf("resolve while disabled = (%q, %q, %v), want all empty", url, label, enabled)
	}
}

// 开启时:代理表里有的补 label + enabled;账号自填的自定义代理只带 URL。
func TestExportProxyResolverEnabledResolvesTableMetadata(t *testing.T) {
	resolver := exportProxyResolver{include: true, byURL: map[string]*database.ProxyRow{
		"http://127.0.0.1:8080": {URL: "http://127.0.0.1:8080", Label: "hk-1", Enabled: false},
	}}

	url, label, enabled := resolver.resolve(" http://127.0.0.1:8080 ")
	if url != "http://127.0.0.1:8080" || label != "hk-1" {
		t.Fatalf("resolve(managed) = (%q, %q), want (http://127.0.0.1:8080, hk-1)", url, label)
	}
	if enabled == nil || *enabled {
		t.Fatalf("resolve(managed) enabled = %v, want explicit false", enabled)
	}

	url, label, enabled = resolver.resolve("socks5://127.0.0.1:1080")
	if url != "socks5://127.0.0.1:1080" || label != "" || enabled != nil {
		t.Fatalf("resolve(custom) = (%q, %q, %v), want URL only", url, label, enabled)
	}

	if url, label, enabled = resolver.resolve("   "); url != "" || label != "" || enabled != nil {
		t.Fatalf("resolve(blank) = (%q, %q, %v), want all empty", url, label, enabled)
	}
}

func codexExportRow() *database.AccountRow {
	return &database.AccountRow{
		ID:       11,
		Name:     "codex user",
		Platform: "openai",
		Enabled:  true,
		ProxyURL: "http://127.0.0.1:8080",
		Credentials: map[string]interface{}{
			"email":         "codex@example.com",
			"refresh_token": "rt-codex",
			"plan_type":     "plus",
		},
	}
}

// 三个渠道的导出条目在开关关闭时都不得泄漏代理 URL。
func TestAccountRowToExportEntryOmitsProxyByDefault(t *testing.T) {
	codex, ok := accountRowToCPAExportEntry(codexExportRow(), exportProxyResolver{})
	if !ok {
		t.Fatal("codex export entry not produced")
	}
	if codex.ProxyURL != "" || codex.ProxyLabel != "" || codex.ProxyEnabled != nil {
		t.Fatalf("codex entry carried proxy while disabled: %+v", codex)
	}

	grokRow := grokOAuthRow()
	grokRow.ProxyURL = "http://127.0.0.1:8080"
	grokEntry, ok := grokAccountRowToExportEntry(grokRow, exportProxyResolver{})
	if !ok {
		t.Fatal("grok export entry not produced")
	}
	if grokEntry.ProxyURL != "" || grokEntry.ProxyLabel != "" || grokEntry.ProxyEnabled != nil {
		t.Fatalf("grok entry carried proxy while disabled: %+v", grokEntry)
	}

	antigravityEntry, ok := antigravityAccountRowToExportEntry(&database.AccountRow{
		ID: 12, Platform: "google", Enabled: true, ProxyURL: "http://127.0.0.1:8080",
		Credentials: map[string]any{
			"upstream_type": auth.UpstreamAntigravity, "email": "ag@example.com",
			"access_token": "at", "refresh_token": "rt",
		},
	}, exportProxyResolver{})
	if !ok {
		t.Fatal("antigravity export entry not produced")
	}
	if antigravityEntry.ProxyURL != "" || antigravityEntry.ProxyLabel != "" || antigravityEntry.ProxyEnabled != nil {
		t.Fatalf("antigravity entry carried proxy while disabled: %+v", antigravityEntry)
	}
}

// 开启后 Codex / Grok 条目都补上 URL + label + enabled。
func TestAccountRowToExportEntryCarriesProxyWhenEnabled(t *testing.T) {
	resolver := exportProxyResolver{include: true, byURL: map[string]*database.ProxyRow{
		"http://127.0.0.1:8080": {URL: "http://127.0.0.1:8080", Label: "hk-1", Enabled: true},
	}}

	// 走对外的分发器,确认 resolver 确实被透传到 Codex 分支而不是半途丢掉。
	dispatched, ok := accountRowToExportEntry(codexExportRow(), resolver)
	if !ok {
		t.Fatal("codex export entry not produced")
	}
	codex, isCPA := dispatched.(cpaExportEntry)
	if !isCPA {
		t.Fatalf("dispatched entry type = %T, want cpaExportEntry", dispatched)
	}
	if codex.ProxyURL != "http://127.0.0.1:8080" || codex.ProxyLabel != "hk-1" {
		t.Fatalf("codex proxy = (%q, %q), want (http://127.0.0.1:8080, hk-1)", codex.ProxyURL, codex.ProxyLabel)
	}
	if codex.ProxyEnabled == nil || !*codex.ProxyEnabled {
		t.Fatalf("codex proxy_enabled = %v, want explicit true", codex.ProxyEnabled)
	}

	grokRow := grokOAuthRow()
	grokRow.ProxyURL = "http://127.0.0.1:8080"
	grokEntry, ok := grokAccountRowToExportEntry(grokRow, resolver)
	if !ok {
		t.Fatal("grok export entry not produced")
	}
	if grokEntry.ProxyURL != "http://127.0.0.1:8080" || grokEntry.ProxyLabel != "hk-1" {
		t.Fatalf("grok proxy = (%q, %q), want (http://127.0.0.1:8080, hk-1)", grokEntry.ProxyURL, grokEntry.ProxyLabel)
	}
	if grokEntry.ProxyEnabled == nil || !*grokEntry.ProxyEnabled {
		t.Fatalf("grok proxy_enabled = %v, want explicit true", grokEntry.ProxyEnabled)
	}
}

// 代理表读失败不该让整次导出失败:降级成只带 URL。
func TestNewExportProxyResolverDegradesWhenProxyTableUnavailable(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "export-proxy-degrade.sqlite")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	t.Cleanup(func() { _ = rawDB.Close() })
	if _, err := rawDB.Exec(`DROP TABLE proxies`); err != nil {
		t.Fatalf("drop proxies: %v", err)
	}
	handler := &Handler{db: db}

	resolver := handler.newExportProxyResolver(context.Background(), true)
	if !resolver.include {
		t.Fatal("resolver.include = false, want export to continue with URL only")
	}
	url, label, enabled := resolver.resolve("http://127.0.0.1:8080")
	if url != "http://127.0.0.1:8080" || label != "" || enabled != nil {
		t.Fatalf("degraded resolve = (%q, %q, %v), want URL only", url, label, enabled)
	}
}

func TestNewExportProxyResolverDisabledSkipsQuery(t *testing.T) {
	// db 为 nil 时仍不能 panic:关闭态压根不该碰代理表。
	handler := &Handler{}
	if resolver := handler.newExportProxyResolver(context.Background(), false); resolver.include {
		t.Fatal("resolver.include = true while include_proxy is off")
	}
}
