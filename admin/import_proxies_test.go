package admin

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// newImportProxyTestHandler 构造一个带真实 sqlite 库和已开启代理池的 Handler,
// 因为 registerImportedProxies 的核心契约（写表 → 同步池）必须端到端验证。
func newImportProxyTestHandler(t *testing.T) (*Handler, *sql.DB) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "import-proxies.sqlite")
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

	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 4, ProxyPoolEnabled: true})
	t.Cleanup(store.Stop)
	store.SetProxyPoolEnabled(true)
	if err := store.ReloadProxyPool(); err != nil {
		t.Fatalf("ReloadProxyPool: %v", err)
	}
	return &Handler{db: db, store: store}, rawDB
}

func proxyURLsInTable(t *testing.T, rawDB *sql.DB) []string {
	t.Helper()
	rows, err := rawDB.Query(`SELECT url FROM proxies ORDER BY id`)
	if err != nil {
		t.Fatalf("query proxies: %v", err)
	}
	defer rows.Close()
	var urls []string
	for rows.Next() {
		var url string
		if err := rows.Scan(&url); err != nil {
			t.Fatalf("scan proxy: %v", err)
		}
		urls = append(urls, url)
	}
	return urls
}

// 去重 + 规范化 + 非法条目跳过。非法代理的账号必须把 proxyURL 清空,才能退回
// 表单填写的代理,而不是绑着一个没进池的 URL 变成不可调度。
func TestRegisterImportedProxiesDedupsAndSkipsInvalid(t *testing.T) {
	handler, rawDB := newImportProxyTestHandler(t)

	tokens := []importToken{
		{refreshToken: "rt-1", proxyURL: "http://127.0.0.1:8080"},
		{refreshToken: "rt-2", proxyURL: "  http://127.0.0.1:8080  "},
		{refreshToken: "rt-3", proxyURL: "socks5://127.0.0.1:1080"},
		{refreshToken: "rt-4", proxyURL: "not a proxy url"},
		{refreshToken: "rt-5"},
	}

	outcome, err := handler.registerImportedProxies(context.Background(), tokens)
	if err != nil {
		t.Fatalf("registerImportedProxies: %v", err)
	}
	if outcome.inserted != 2 {
		t.Fatalf("inserted = %d, want 2 unique proxies", outcome.inserted)
	}
	if outcome.skipped != 1 {
		t.Fatalf("skipped = %d, want 1 invalid proxy", outcome.skipped)
	}
	if !strings.Contains(outcome.warning(), "格式无效") {
		t.Fatalf("warning = %q, want it to mention the invalid proxy", outcome.warning())
	}

	if got := proxyURLsInTable(t, rawDB); !reflect.DeepEqual(got, []string{"http://127.0.0.1:8080", "socks5://127.0.0.1:1080"}) {
		t.Fatalf("proxies table = %v, want the two unique URLs", got)
	}

	wantTokenProxies := []string{"http://127.0.0.1:8080", "http://127.0.0.1:8080", "socks5://127.0.0.1:1080", "", ""}
	for i, want := range wantTokenProxies {
		if tokens[i].proxyURL != want {
			t.Fatalf("tokens[%d].proxyURL = %q, want %q", i, tokens[i].proxyURL, want)
		}
	}
}

// 重复导入同一份文件不该反复插入。
func TestRegisterImportedProxiesIsIdempotent(t *testing.T) {
	handler, rawDB := newImportProxyTestHandler(t)
	newTokens := func() []importToken {
		return []importToken{{refreshToken: "rt-1", proxyURL: "http://127.0.0.1:8080"}}
	}

	first, err := handler.registerImportedProxies(context.Background(), newTokens())
	if err != nil {
		t.Fatalf("first registerImportedProxies: %v", err)
	}
	second, err := handler.registerImportedProxies(context.Background(), newTokens())
	if err != nil {
		t.Fatalf("second registerImportedProxies: %v", err)
	}
	if first.inserted != 1 || second.inserted != 0 {
		t.Fatalf("inserted = %d then %d, want 1 then 0", first.inserted, second.inserted)
	}
	if got := proxyURLsInTable(t, rawDB); len(got) != 1 {
		t.Fatalf("proxies table = %v, want a single row", got)
	}
}

// 这是整个方案的核心竞态锁:代理写完表后必须立刻同步进内存代理池。否则账号绑上
// 一个「在 managedProxySet 却不在 proxyPoolSet」的 URL,会被判定为无可用出口而
// 整批不可调度。
func TestRegisterImportedProxiesReloadsPoolSoAccountsAreSchedulable(t *testing.T) {
	handler, _ := newImportProxyTestHandler(t)
	store := handler.store

	// 前置断言:导入前这个 URL 对代理池完全陌生,不会被误判成"已托管但被禁用"。
	if store.ManagedProxyUnavailable("http://127.0.0.1:8080") {
		t.Fatal("proxy is unavailable before import, test premise is wrong")
	}

	tokens := []importToken{{refreshToken: "rt-1", proxyURL: "http://127.0.0.1:8080"}}
	outcome, err := handler.registerImportedProxies(context.Background(), tokens)
	if err != nil {
		t.Fatalf("registerImportedProxies: %v", err)
	}
	if outcome.unusable != 0 {
		t.Fatalf("unusable = %d, want 0 right after registration", outcome.unusable)
	}
	// 关键断言:registerImportedProxies 一返回,代理就已经在启用集里——写账号
	// 不会开出 fail-closed 窗口。
	if store.ManagedProxyUnavailable("http://127.0.0.1:8080") {
		t.Fatal("imported proxy is still unusable after registration: proxy pool was not reloaded")
	}
	if unusable := store.UnusableManagedProxies([]string{"http://127.0.0.1:8080"}); len(unusable) != 0 {
		t.Fatalf("UnusableManagedProxies = %v, want empty", unusable)
	}
}

// 目标端早就存在同 URL 且被禁用时,ON CONFLICT DO NOTHING 不会复活它,绑定它的
// 账号一律不可调度——必须显式告警而不是静默导入一批废号。
func TestRegisterImportedProxiesWarnsWhenTargetProxyIsDisabled(t *testing.T) {
	handler, rawDB := newImportProxyTestHandler(t)

	if _, err := handler.db.InsertProxies(context.Background(), []string{"http://127.0.0.1:8080"}, "pre-existing"); err != nil {
		t.Fatalf("seed proxy: %v", err)
	}
	if _, err := rawDB.Exec(`UPDATE proxies SET enabled = 0 WHERE url = ?`, "http://127.0.0.1:8080"); err != nil {
		t.Fatalf("disable seeded proxy: %v", err)
	}

	tokens := []importToken{{refreshToken: "rt-1", proxyURL: "http://127.0.0.1:8080"}}
	outcome, err := handler.registerImportedProxies(context.Background(), tokens)
	if err != nil {
		t.Fatalf("registerImportedProxies: %v", err)
	}
	if outcome.inserted != 0 {
		t.Fatalf("inserted = %d, want 0 (URL already present)", outcome.inserted)
	}
	if outcome.unusable != 1 {
		t.Fatalf("unusable = %d, want 1", outcome.unusable)
	}
	if !strings.Contains(outcome.warning(), "禁用或测试失败") {
		t.Fatalf("warning = %q, want it to flag the disabled target proxy", outcome.warning())
	}
	// 标签保持原样:导入不该悄悄改写目标端已有代理的元数据。
	var label string
	if err := rawDB.QueryRow(`SELECT label FROM proxies WHERE url = ?`, "http://127.0.0.1:8080").Scan(&label); err != nil {
		t.Fatalf("read label: %v", err)
	}
	if label != "pre-existing" {
		t.Fatalf("label = %q, want the pre-existing label untouched", label)
	}
}

// 源端禁用的代理一律以启用态导入(照搬禁用会让账号立刻 fail-closed),但要告警。
func TestRegisterImportedProxiesWarnsWhenSourceMarkedDisabled(t *testing.T) {
	handler, _ := newImportProxyTestHandler(t)
	disabled := false
	enabled := true

	tokens := []importToken{
		{refreshToken: "rt-1", proxyURL: "http://127.0.0.1:8080", proxyEnabled: &disabled},
		{refreshToken: "rt-2", proxyURL: "http://127.0.0.1:8081", proxyEnabled: &enabled},
		// 老文件不带 proxy_enabled,按启用处理,不该计入告警。
		{refreshToken: "rt-3", proxyURL: "http://127.0.0.1:8082"},
	}
	outcome, err := handler.registerImportedProxies(context.Background(), tokens)
	if err != nil {
		t.Fatalf("registerImportedProxies: %v", err)
	}
	if outcome.inserted != 3 {
		t.Fatalf("inserted = %d, want 3", outcome.inserted)
	}
	if !strings.Contains(outcome.warning(), "源端是禁用状态") {
		t.Fatalf("warning = %q, want it to flag the source-disabled proxy", outcome.warning())
	}
	// 以启用态导入,所以对代理池而言立即可用。
	if outcome.unusable != 0 {
		t.Fatalf("unusable = %d, want 0", outcome.unusable)
	}
}

// 超限时一条都不注册,并把所有 token 的代理清空退回表单值——半生效状态最难解释。
func TestRegisterImportedProxiesRejectsWholeBatchOverCap(t *testing.T) {
	handler, rawDB := newImportProxyTestHandler(t)

	tokens := make([]importToken, 0, maxImportedProxies+1)
	for i := 0; i <= maxImportedProxies; i++ {
		tokens = append(tokens, importToken{
			refreshToken: fmt.Sprintf("rt-%d", i),
			proxyURL:     fmt.Sprintf("http://127.0.0.1:%d", 10000+i),
		})
	}

	outcome, err := handler.registerImportedProxies(context.Background(), tokens)
	if err != nil {
		t.Fatalf("registerImportedProxies: %v", err)
	}
	if outcome.inserted != 0 {
		t.Fatalf("inserted = %d, want 0 when over the cap", outcome.inserted)
	}
	if outcome.skipped != maxImportedProxies+1 {
		t.Fatalf("skipped = %d, want %d", outcome.skipped, maxImportedProxies+1)
	}
	if !strings.Contains(outcome.warning(), "上限") {
		t.Fatalf("warning = %q, want it to mention the cap", outcome.warning())
	}
	if got := proxyURLsInTable(t, rawDB); len(got) != 0 {
		t.Fatalf("proxies table has %d rows, want none", len(got))
	}
	for i := range tokens {
		if tokens[i].proxyURL != "" {
			t.Fatalf("tokens[%d].proxyURL = %q, want cleared", i, tokens[i].proxyURL)
		}
	}
}

// 代理池同步失败必须整次导入中止:继续写账号只会产出一批不可调度的号。
func TestRegisterImportedProxiesFailsWhenPoolReloadFails(t *testing.T) {
	handler, rawDB := newImportProxyTestHandler(t)
	// 换一个没有配 loader 的 store,让 ReloadProxyPool 必然报错;代理表本身仍可写,
	// 才能验证"表已写入但池没同步"这条路径确实被拦下。
	brokenStore := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 4, ProxyPoolEnabled: true})
	t.Cleanup(brokenStore.Stop)
	brokenStore.SetProxyPoolEnabled(true)
	handler.store = brokenStore

	tokens := []importToken{{refreshToken: "rt-1", proxyURL: "http://127.0.0.1:8080"}}
	if _, err := handler.registerImportedProxies(context.Background(), tokens); err == nil {
		t.Fatal("registerImportedProxies returned nil error, want the import to abort")
	}
	// 代理已经落表是预期的(下次导入会 ON CONFLICT 跳过),关键是调用方拿到错误
	// 后不会继续写账号。
	if got := proxyURLsInTable(t, rawDB); len(got) != 1 {
		t.Fatalf("proxies table = %v, want the row that was written before the reload failed", got)
	}
}

func TestCountDisabledAtSource(t *testing.T) {
	disabled := false
	enabled := true
	bindings := []importProxyBinding{
		{url: "http://a:8080", enabled: &disabled},
		// 同一代理被多个账号引用时只算一条。
		{url: "http://a:8080", enabled: &disabled},
		{url: "http://b:8080", enabled: &enabled},
		{url: "http://c:8080"},
		{enabled: &disabled},
	}
	if got := countDisabledAtSource(bindings); got != 1 {
		t.Fatalf("countDisabledAtSource = %d, want 1", got)
	}
}

// 文件内代理优先于表单代理;开关关闭时文件内代理完全不生效。
func TestImportSettingsProxyForToken(t *testing.T) {
	fileToken := importToken{proxyURL: "http://from-file:8080"}
	bareToken := importToken{}

	cases := []struct {
		name     string
		settings importSettings
		token    importToken
		want     string
	}{
		{"file wins over form", importSettings{defaultProxyURL: "http://from-form:8080", importProxies: true}, fileToken, "http://from-file:8080"},
		{"form fills the gap", importSettings{defaultProxyURL: "http://from-form:8080", importProxies: true}, bareToken, "http://from-form:8080"},
		{"switch off ignores file", importSettings{defaultProxyURL: "http://from-form:8080"}, fileToken, "http://from-form:8080"},
		{"nothing configured", importSettings{}, fileToken, ""},
		{"form value is trimmed", importSettings{defaultProxyURL: "  http://from-form:8080  "}, bareToken, "http://from-form:8080"},
	}
	for _, tc := range cases {
		if got := tc.settings.proxyForToken(tc.token); got != tc.want {
			t.Fatalf("%s: proxyForToken = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// 文件带来的代理是被动数据,不覆盖目标端已有绑定;表单代理是操作员的显式换绑
// 意图,维持既有的覆盖语义。
func TestImportSettingsProxyOverwritePolicy(t *testing.T) {
	fileToken := importToken{proxyURL: "http://from-file:8080"}

	if got := (importSettings{importProxies: true}).proxyOverwritePolicyForToken(fileToken); got != preserveAccountProxy {
		t.Fatal("file-carried proxy should preserve the existing account binding")
	}
	if got := (importSettings{importProxies: true, defaultProxyURL: "http://from-form:8080"}).proxyOverwritePolicyForToken(importToken{}); got != overwriteAccountProxy {
		t.Fatal("form-entered proxy should keep overwriting the existing binding")
	}
	if got := (importSettings{defaultProxyURL: "http://from-form:8080"}).proxyOverwritePolicyForToken(fileToken); got != overwriteAccountProxy {
		t.Fatal("file proxy must not change the policy while the switch is off")
	}
}

// 回归锁:代理字段绝不能进去重指纹。同一份凭据配不同代理仍然是同一个账号,
// 参与指纹会让它被当成两条独立记录导入两遍。
func TestImportDedupIsUnaffectedByProxyFields(t *testing.T) {
	enabled := true
	base := importToken{
		refreshToken:     "rt-shared",
		accessToken:      "at-shared",
		email:            "user@example.com",
		chatgptAccountID: "workspace-1",
		planType:         "plus",
		// 固定过期时间:缺省值是"此刻 +1h",两次调用会差出几百纳秒,把真正的
		// 代理字段漂移淹没掉。
		expiresAt: "2026-08-31T12:00:00Z",
	}
	withProxy := base
	withProxy.proxyURL = "http://127.0.0.1:8080"
	withProxy.proxyLabel = "hk-1"
	withProxy.proxyEnabled = &enabled

	conflicts := conflictingImportChatGPTIDs([]importToken{base, withProxy})
	if len(conflicts) != 0 {
		t.Fatalf("conflictingImportChatGPTIDs = %v, want no conflict from proxy-only differences", conflicts)
	}
	if got, want := importTokenCredentialFingerprint(withProxy, conflicts), importTokenCredentialFingerprint(base, conflicts); got != want {
		t.Fatalf("credential fingerprint drifted with proxy: %q vs %q", got, want)
	}
	if got, want := importTokenOAuthIdentityKey(withProxy, conflicts), importTokenOAuthIdentityKey(base, conflicts); got != want {
		t.Fatalf("oauth identity key drifted with proxy: %q vs %q", got, want)
	}
	if got, want := importTokenSeed(withProxy, conflicts), importTokenSeed(base, conflicts); !reflect.DeepEqual(got, want) {
		t.Fatalf("credential seed drifted with proxy:\n got %+v\nwant %+v", got, want)
	}
}

// 三种 JSON 形态都要把代理三件套带进 importToken。
func TestParseImportJSONTokensStreamCarriesProxyFields(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"top-level array", `[{"refresh_token":"rt-1","email":"a@example.com","proxy_url":"http://127.0.0.1:8080","proxy_label":"hk-1","proxy_enabled":false}]`},
		{"single flat object", `{"refresh_token":"rt-1","email":"a@example.com","proxy_url":"http://127.0.0.1:8080","proxy_label":"hk-1","proxy_enabled":false}`},
		// 代理是账号属性,不同导出实现有的写在条目根上、有的塞进 credentials,两处都要收。
		{"sub2api wrapper, proxy on entry", `{"accounts":[{"credentials":{"refresh_token":"rt-1","email":"a@example.com"},"proxy_url":"http://127.0.0.1:8080","proxy_label":"hk-1","proxy_enabled":false}]}`},
		{"sub2api wrapper, proxy in credentials", `{"accounts":[{"credentials":{"refresh_token":"rt-1","email":"a@example.com","proxy_url":"http://127.0.0.1:8080","proxy_label":"hk-1","proxy_enabled":false}}]}`},
	}
	for _, tc := range cases {
		tokens, err := parseImportJSONTokensStream(strings.NewReader(tc.body))
		if err != nil {
			t.Fatalf("%s: parse: %v", tc.name, err)
		}
		if len(tokens) != 1 {
			t.Fatalf("%s: parsed %d tokens, want 1", tc.name, len(tokens))
		}
		tok := tokens[0]
		if tok.proxyURL != "http://127.0.0.1:8080" || tok.proxyLabel != "hk-1" {
			t.Fatalf("%s: proxy = (%q, %q), want (http://127.0.0.1:8080, hk-1)", tc.name, tok.proxyURL, tok.proxyLabel)
		}
		// 显式 false 必须能和"字段缺失"区分开,否则源端禁用状态无声丢失。
		if tok.proxyEnabled == nil || *tok.proxyEnabled {
			t.Fatalf("%s: proxy_enabled = %v, want explicit false", tc.name, tok.proxyEnabled)
		}
	}
}

// 条目根和 credentials 同时带代理时以条目根为准,且 label/enabled 跟着 URL 走,
// 不能跨两处拼出一组配错的元数据。
func TestSub2apiEntryProxyFieldsPreferEntryLevel(t *testing.T) {
	entryEnabled := true
	credentialEnabled := false
	entry := sub2apiAccountEntry{
		ProxyURL:     " http://entry:8080 ",
		ProxyLabel:   "entry-label",
		ProxyEnabled: &entryEnabled,
		Credentials: sub2apiAccountCredentials{
			ProxyURL:     "http://credentials:8080",
			ProxyLabel:   "credentials-label",
			ProxyEnabled: &credentialEnabled,
		},
	}
	url, label, enabled := entry.proxyFields()
	if url != "http://entry:8080" || label != "entry-label" || enabled == nil || !*enabled {
		t.Fatalf("proxyFields = (%q, %q, %v), want the entry-level trio", url, label, enabled)
	}

	// 根上没有 URL 时整组回退到 credentials。
	entry.ProxyURL = ""
	url, label, enabled = entry.proxyFields()
	if url != "http://credentials:8080" || label != "credentials-label" || enabled == nil || *enabled {
		t.Fatalf("proxyFields = (%q, %q, %v), want the credentials-level trio", url, label, enabled)
	}
}

// 老文件不带代理字段时 proxyEnabled 保持 nil(按启用处理),不能被零值填成 false。
func TestParseImportJSONTokensStreamLeavesProxyUnsetForLegacyFiles(t *testing.T) {
	tokens, err := parseImportJSONTokensStream(strings.NewReader(`[{"refresh_token":"rt-1","email":"a@example.com"}]`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(tokens) != 1 {
		t.Fatalf("parsed %d tokens, want 1", len(tokens))
	}
	if tokens[0].proxyURL != "" || tokens[0].proxyLabel != "" || tokens[0].proxyEnabled != nil {
		t.Fatalf("legacy entry produced proxy fields: %+v", tokens[0])
	}
}

// Agent Identity 条目走的是另一条分支,代理同样要带上。
func TestParseImportJSONTokensStreamCarriesProxyForAgentIdentity(t *testing.T) {
	body := `[{"auth_mode":"agentIdentity","agent_runtime_id":"runtime-1","agent_private_key":"key-1",` +
		`"email":"agent@example.com","proxy_url":"http://127.0.0.1:8080","proxy_label":"hk-1","proxy_enabled":true}]`
	tokens, err := parseImportJSONTokensStream(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(tokens) != 1 {
		t.Fatalf("parsed %d tokens, want 1", len(tokens))
	}
	if !tokens[0].isAgentIdentity() {
		t.Fatalf("token is not recognised as agent identity: %+v", tokens[0])
	}
	if tokens[0].proxyURL != "http://127.0.0.1:8080" || tokens[0].proxyLabel != "hk-1" {
		t.Fatalf("agent identity proxy = (%q, %q), want it carried through", tokens[0].proxyURL, tokens[0].proxyLabel)
	}
}

func TestImportedProxyLabelIsBatchScoped(t *testing.T) {
	label := importedProxyLabel()
	if !strings.HasPrefix(label, "imported-") || len(label) != len("imported-20060102-1504") {
		t.Fatalf("importedProxyLabel = %q, want an imported-<timestamp> batch label", label)
	}
}

// newImportEndToEndHandler 组一套「真实 sqlite + 已开代理池的 Store」的 Handler，
// 用来端到端验证导入后账号确实绑上代理且立刻可调度。
func newImportEndToEndHandler(t *testing.T) (*Handler, *auth.Store, *database.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, &database.SystemSettings{
		MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4", ProxyPoolEnabled: true,
	})
	t.Cleanup(store.Stop)
	store.SetProxyPoolEnabled(true)
	if err := store.ReloadProxyPool(); err != nil {
		t.Fatalf("ReloadProxyPool: %v", err)
	}
	handler := &Handler{
		db:         db,
		store:      store,
		probeUsage: func(context.Context, *auth.Account) error { return nil },
	}
	return handler, store, db
}

func runImportAccountsCommon(t *testing.T, handler *Handler, tokens []importToken, settings importSettings) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/import", nil)
	handler.importAccountsCommon(ctx, tokens, settings)
	return recorder
}

func accountRowByRefreshToken(t *testing.T, db *database.DB, refreshToken string) *database.AccountRow {
	t.Helper()
	rows, err := db.ListActive(context.Background())
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	for _, row := range rows {
		if row.GetCredential("refresh_token") == refreshToken {
			return row
		}
	}
	t.Fatalf("no active account with refresh_token %q", refreshToken)
	return nil
}

// 全链路:文件带代理 → 代理注册进表和内存池 → 账号绑上它 → 立刻可调度。
// 这条路径就是整个功能的目的,少任何一环都会退化成"导入成功但账号全废"。
func TestImportAccountsCommonBindsAndRegistersFileProxy(t *testing.T) {
	handler, store, db := newImportEndToEndHandler(t)

	recorder := runImportAccountsCommon(t, handler, []importToken{{
		name: "imported", refreshToken: "rt-file-proxy", accessToken: "at-file-proxy",
		email: "file@example.com", proxyURL: "http://127.0.0.1:8080",
	}}, importSettings{importProxies: true})

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"proxies_imported":1`) {
		t.Fatalf("import stream did not report the registered proxy: %s", recorder.Body.String())
	}

	row := accountRowByRefreshToken(t, db, "rt-file-proxy")
	if row.ProxyURL != "http://127.0.0.1:8080" {
		t.Fatalf("account proxy_url = %q, want the file-carried proxy", row.ProxyURL)
	}

	// 关键:代理池已同步,账号有可用出口,不是绑了个未入池 URL 的死号。
	account := &auth.Account{DBID: row.ID, ProxyURL: row.ProxyURL}
	if !store.AccountHasUsableEgress(account) {
		t.Fatal("imported account has no usable egress: the proxy pool was not in sync when the account was written")
	}
	if got := store.ResolveProxyForAccount(account); got != "http://127.0.0.1:8080" {
		t.Fatalf("ResolveProxyForAccount = %q, want the imported proxy", got)
	}
}

// 开关关闭时文件内代理完全不生效:不注册、不绑定,表单代理照常生效。
func TestImportAccountsCommonIgnoresFileProxyWhenSwitchOff(t *testing.T) {
	handler, _, db := newImportEndToEndHandler(t)

	recorder := runImportAccountsCommon(t, handler, []importToken{{
		name: "imported", refreshToken: "rt-switch-off", accessToken: "at-switch-off",
		email: "off@example.com", proxyURL: "http://from-file:8080",
	}}, importSettings{defaultProxyURL: "http://from-form:8080"})

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "proxies_imported") {
		t.Fatalf("import stream reported proxy registration while the switch is off: %s", recorder.Body.String())
	}

	if row := accountRowByRefreshToken(t, db, "rt-switch-off"); row.ProxyURL != "http://from-form:8080" {
		t.Fatalf("account proxy_url = %q, want the form-entered proxy", row.ProxyURL)
	}
	proxies, err := db.ListProxies(context.Background())
	if err != nil {
		t.Fatalf("ListProxies: %v", err)
	}
	if len(proxies) != 0 {
		t.Fatalf("proxies table has %d rows, want none while the switch is off", len(proxies))
	}
}

// 文件带来的代理不覆盖目标端已有绑定——那里可能已经做过精细分配(自动均衡等),
// 静默冲掉属于数据损坏。而表单填写的代理是操作员的显式换绑意图,维持覆盖语义。
func TestImportAccountsCommonProxyOverwriteSemantics(t *testing.T) {
	cases := []struct {
		name     string
		settings importSettings
		token    importToken
		want     string
	}{
		{
			name:     "file proxy preserves the existing binding",
			settings: importSettings{importProxies: true, allowDuplicate: true},
			token:    importToken{proxyURL: "http://from-file:8080"},
			want:     "http://existing:8080",
		},
		{
			name:     "form proxy still overwrites",
			settings: importSettings{defaultProxyURL: "http://from-form:8080", allowDuplicate: true},
			want:     "http://from-form:8080",
		},
		{
			name:     "empty proxy never clears the existing binding",
			settings: importSettings{importProxies: true, allowDuplicate: true},
			want:     "http://existing:8080",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler, _, db := newImportEndToEndHandler(t)
			idToken := makeOAuthTestIDToken("bound@example.com", "acc-bound", "team")
			if _, err := db.InsertAccountWithCredentials(context.Background(), "existing", map[string]interface{}{
				"refresh_token": "rt-old",
				"email":         "bound@example.com",
				"account_id":    "acc-bound",
				"workspace_id":  "acc-bound",
			}, "http://existing:8080"); err != nil {
				t.Fatalf("InsertAccountWithCredentials: %v", err)
			}

			token := tc.token
			token.refreshToken = "rt-new"
			token.accessToken = "at-new"
			token.idToken = idToken
			token.email = "bound@example.com"
			token.accountID = "acc-bound"
			token.planType = "team"

			recorder := runImportAccountsCommon(t, handler, []importToken{token}, tc.settings)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
			}

			rows, err := db.ListActive(context.Background())
			if err != nil {
				t.Fatalf("ListActive: %v", err)
			}
			if len(rows) != 1 {
				t.Fatalf("active rows = %d, want the single updated account", len(rows))
			}
			if rows[0].ProxyURL != tc.want {
				t.Fatalf("account proxy_url = %q, want %q", rows[0].ProxyURL, tc.want)
			}
		})
	}
}
