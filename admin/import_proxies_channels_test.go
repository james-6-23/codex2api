package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// grokFileProxyBinding 是 Grok 侧唯一读取代理的地方（ParseGrokAuthJSON 只认凭据
// 字段），解析漏了就等于开关整体失效。
func TestGrokFileProxyBinding(t *testing.T) {
	enabled := true
	disabled := false
	cases := []struct {
		name        string
		content     string
		wantURL     string
		wantEnabled *bool
	}{
		{
			name:    "顶层代理",
			content: `{"refresh_token":"rt","proxy_url":"http://127.0.0.1:8080"}`,
			wantURL: "http://127.0.0.1:8080",
		},
		{
			name:        "带启用状态",
			content:     `{"refresh_token":"rt","proxy_url":"http://127.0.0.1:8080","proxy_enabled":true}`,
			wantURL:     "http://127.0.0.1:8080",
			wantEnabled: &enabled,
		},
		{
			name:        "源端禁用",
			content:     `{"refresh_token":"rt","proxy_url":"http://127.0.0.1:8080","proxy_enabled":false}`,
			wantURL:     "http://127.0.0.1:8080",
			wantEnabled: &disabled,
		},
		{
			name:    "tokens 包装形态下探一层",
			content: `{"tokens":{"refresh_token":"rt","proxy_url":"http://127.0.0.1:9090"}}`,
			wantURL: "http://127.0.0.1:9090",
		},
		{
			name:    "顶层优先于 tokens",
			content: `{"proxy_url":"http://top:8080","tokens":{"proxy_url":"http://nested:8080"}}`,
			wantURL: "http://top:8080",
		},
		{name: "旧文件不带代理", content: `{"refresh_token":"rt"}`},
		{name: "空字符串按没带处理", content: `{"proxy_url":"   "}`},
		// 文件是否合法由 ParseGrokAuthJSON 判定并报错，这里再报一次只会搅乱错误信息。
		{name: "非法 JSON 静默当作没带", content: `{not json`},
		{name: "proxy_url 类型不对", content: `{"proxy_url":123}`},
		{name: "顶层是数组", content: `[{"proxy_url":"http://127.0.0.1:8080"}]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := grokFileProxyBinding(tc.content)
			if got.url != tc.wantURL {
				t.Fatalf("url = %q, want %q", got.url, tc.wantURL)
			}
			switch {
			case tc.wantEnabled == nil && got.enabled != nil:
				t.Fatalf("enabled = %t, want nil", *got.enabled)
			case tc.wantEnabled != nil && got.enabled == nil:
				t.Fatalf("enabled = nil, want %t", *tc.wantEnabled)
			case tc.wantEnabled != nil && *got.enabled != *tc.wantEnabled:
				t.Fatalf("enabled = %t, want %t", *got.enabled, *tc.wantEnabled)
			}
		})
	}
}

// proxy_enabled 类型不对时不能把它当成 false —— 那会误报"源端禁用"告警。
func TestGrokFileProxyBindingIgnoresMalformedEnabled(t *testing.T) {
	got := grokFileProxyBinding(`{"proxy_url":"http://127.0.0.1:8080","proxy_enabled":"maybe"}`)
	if got.url != "http://127.0.0.1:8080" {
		t.Fatalf("url = %q, want the proxy to still be picked up", got.url)
	}
	if got.enabled != nil {
		t.Fatalf("enabled = %t, want nil for a malformed value", *got.enabled)
	}
}

func newGrokProxyImportHandler(t *testing.T) (*Handler, *auth.Store, *database.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, &database.SystemSettings{
		MaxConcurrency: 2, ProxyPoolEnabled: true,
	})
	t.Cleanup(store.Stop)
	store.SetProxyPoolEnabled(true)
	if err := store.ReloadProxyPool(); err != nil {
		t.Fatalf("ReloadProxyPool: %v", err)
	}
	return &Handler{db: db, store: store}, store, db
}

type grokProxyImportResponse struct {
	Imported        int                   `json:"imported"`
	Items           []grokBatchImportItem `json:"items"`
	ProxiesImported *int                  `json:"proxies_imported"`
	ProxiesSkipped  *int                  `json:"proxies_skipped"`
	ProxyWarning    string                `json:"proxy_warning"`
}

func doGrokProxyImport(t *testing.T, handler *Handler, body map[string]any) grokProxyImportResponse {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	recorder := httptest.NewRecorder()
	requestContext, _ := gin.CreateTestContext(recorder)
	requestContext.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/grok/import", strings.NewReader(string(encoded)))
	requestContext.Request.Header.Set("Content-Type", "application/json")
	handler.BatchImportGrokAccounts(requestContext)
	if recorder.Code != http.StatusOK {
		t.Fatalf("BatchImportGrokAccounts status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	var resp grokProxyImportResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v, body=%s", err, recorder.Body.String())
	}
	return resp
}

func grokAccountRowByRefreshToken(t *testing.T, db *database.DB, refreshToken string) *database.AccountRow {
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

// 全链路:Grok 文件带代理 → 代理注册进表和内存池 → 账号绑上它 → 立刻可调度。
// 少了池同步那一环，账号绑的就是个已在 managedProxySet 却不在 proxyPoolSet 的
// URL，整批号会被判为无可用出口。
func TestBatchImportGrokBindsAndRegistersFileProxy(t *testing.T) {
	handler, store, db := newGrokProxyImportHandler(t)

	resp := doGrokProxyImport(t, handler, map[string]any{
		"files": []string{
			`{"refresh_token":"rt-grok-proxy","client_id":"cli","user_id":"u-1","email":"g1@example.com","proxy_url":"http://127.0.0.1:8080"}`,
		},
		"import_proxy": true,
	})
	if resp.Imported != 1 {
		t.Fatalf("imported = %d, want 1 (items=%+v)", resp.Imported, resp.Items)
	}
	if resp.ProxiesImported == nil || *resp.ProxiesImported != 1 {
		t.Fatalf("proxies_imported = %v, want 1", resp.ProxiesImported)
	}
	if resp.ProxyWarning != "" {
		t.Fatalf("proxy_warning = %q, want none", resp.ProxyWarning)
	}

	row := grokAccountRowByRefreshToken(t, db, "rt-grok-proxy")
	if row.ProxyURL != "http://127.0.0.1:8080" {
		t.Fatalf("account proxy_url = %q, want the file-carried proxy", row.ProxyURL)
	}
	account := store.FindByID(row.ID)
	if account == nil {
		t.Fatal("imported account missing from runtime pool")
	}
	if !store.AccountHasUsableEgress(account) {
		t.Fatal("账号绑了文件内代理却没有可用出口，代理池没同步上")
	}
}

// 开关关闭时文件里的 proxy_url 完全不生效：账号用表单代理，代理表保持干净。
func TestBatchImportGrokIgnoresFileProxyWhenSwitchOff(t *testing.T) {
	handler, _, db := newGrokProxyImportHandler(t)

	resp := doGrokProxyImport(t, handler, map[string]any{
		"files": []string{
			`{"refresh_token":"rt-grok-off","client_id":"cli","user_id":"u-2","email":"g2@example.com","proxy_url":"http://127.0.0.1:8080"}`,
		},
		"proxy_url": "http://127.0.0.1:7070",
	})
	if resp.Imported != 1 {
		t.Fatalf("imported = %d, want 1 (items=%+v)", resp.Imported, resp.Items)
	}
	if resp.ProxiesImported != nil {
		t.Fatalf("proxies_imported = %v, want the field to be absent when the switch is off", *resp.ProxiesImported)
	}

	row := grokAccountRowByRefreshToken(t, db, "rt-grok-off")
	if row.ProxyURL != "http://127.0.0.1:7070" {
		t.Fatalf("account proxy_url = %q, want the form proxy", row.ProxyURL)
	}
	proxies, err := db.ListProxies(context.Background())
	if err != nil {
		t.Fatalf("ListProxies: %v", err)
	}
	if len(proxies) != 0 {
		t.Fatalf("proxies table = %+v, want empty when the switch is off", proxies)
	}
}

// 命中既有身份时,文件带来的代理不覆盖对方已有的绑定——那里可能已经做过精细
// 分配。表单填的代理是显式换绑意图,维持覆盖语义。
//
// 走的是「删除后重新导入」这条真实路径:活跃账号会在身份去重那一步被直接跳过,
// 只有回收站里的账号才会命中合并/复活分支(issue #602),而这恰恰是代理覆盖语义
// 唯一生效的地方。
func TestBatchImportGrokProxyOverwriteSemantics(t *testing.T) {
	authJSON := `{"refresh_token":"rt-grok-merge","client_id":"cli","user_id":"u-3","email":"g3@example.com","proxy_url":"http://127.0.0.1:8080"}`

	// importThenRecycle 先导入一个账号(可带表单代理),再把它软删进回收站,
	// 返回账号 ID,供下一次导入命中合并分支。
	importThenRecycle := func(t *testing.T, handler *Handler, db *database.DB, store *auth.Store, formProxy string) int64 {
		t.Helper()
		body := map[string]any{"files": []string{authJSON}}
		if formProxy != "" {
			body["proxy_url"] = formProxy
		}
		first := doGrokProxyImport(t, handler, body)
		if first.Imported != 1 || len(first.Items) != 1 || first.Items[0].ID <= 0 {
			t.Fatalf("first import = %+v, want one new account", first)
		}
		accountID := first.Items[0].ID
		if err := db.SoftDeleteAccount(context.Background(), accountID); err != nil {
			t.Fatalf("SoftDeleteAccount: %v", err)
		}
		store.RemoveAccount(accountID)
		return accountID
	}

	accountProxyURL := func(t *testing.T, db *database.DB, accountID int64) string {
		t.Helper()
		row, err := db.GetAccountByIDIncludingDeleted(context.Background(), accountID)
		if err != nil {
			t.Fatalf("GetAccountByIDIncludingDeleted: %v", err)
		}
		return row.ProxyURL
	}

	t.Run("文件代理保留既有绑定", func(t *testing.T) {
		handler, store, db := newGrokProxyImportHandler(t)
		accountID := importThenRecycle(t, handler, db, store, "http://127.0.0.1:7070")

		second := doGrokProxyImport(t, handler, map[string]any{
			"files": []string{authJSON}, "import_proxy": true,
		})
		if second.Imported != 1 || !second.Items[0].Updated {
			t.Fatalf("re-import = %+v, want a merge into the existing account", second)
		}
		if got := accountProxyURL(t, db, accountID); got != "http://127.0.0.1:7070" {
			t.Fatalf("account proxy_url = %q, want the pre-existing binding preserved", got)
		}
	})

	t.Run("表单代理覆盖既有绑定", func(t *testing.T) {
		handler, store, db := newGrokProxyImportHandler(t)
		accountID := importThenRecycle(t, handler, db, store, "http://127.0.0.1:7070")

		second := doGrokProxyImport(t, handler, map[string]any{
			"files": []string{authJSON}, "proxy_url": "http://127.0.0.1:9090",
		})
		if second.Imported != 1 || !second.Items[0].Updated {
			t.Fatalf("re-import = %+v, want a merge into the existing account", second)
		}
		if got := accountProxyURL(t, db, accountID); got != "http://127.0.0.1:9090" {
			t.Fatalf("account proxy_url = %q, want the form proxy to overwrite", got)
		}
	})

	// 账号还没绑代理时,文件代理该填进去——"不覆盖"不等于"永不写入"。
	t.Run("文件代理填补空绑定", func(t *testing.T) {
		handler, store, db := newGrokProxyImportHandler(t)
		accountID := importThenRecycle(t, handler, db, store, "")
		if got := accountProxyURL(t, db, accountID); got != "" {
			t.Fatalf("first import bound proxy %q, want none", got)
		}

		second := doGrokProxyImport(t, handler, map[string]any{
			"files": []string{authJSON}, "import_proxy": true,
		})
		if second.Imported != 1 || !second.Items[0].Updated {
			t.Fatalf("re-import = %+v, want a merge into the existing account", second)
		}
		if got := accountProxyURL(t, db, accountID); got != "http://127.0.0.1:8080" {
			t.Fatalf("account proxy_url = %q, want the file proxy to fill the empty binding", got)
		}
	})
}

// 一批文件里的重复代理只注册一条,非法的跳过并让对应账号退回表单代理。
func TestBatchImportGrokDedupsAndFallsBackOnInvalidProxy(t *testing.T) {
	handler, _, db := newGrokProxyImportHandler(t)

	resp := doGrokProxyImport(t, handler, map[string]any{
		"files": []string{
			`{"refresh_token":"rt-a","client_id":"cli","user_id":"u-a","email":"a@example.com","proxy_url":"http://127.0.0.1:8080"}`,
			`{"refresh_token":"rt-b","client_id":"cli","user_id":"u-b","email":"b@example.com","proxy_url":"http://127.0.0.1:8080"}`,
			`{"refresh_token":"rt-c","client_id":"cli","user_id":"u-c","email":"c@example.com","proxy_url":"not a proxy url"}`,
		},
		"proxy_url":    "http://127.0.0.1:7070",
		"import_proxy": true,
	})
	if resp.Imported != 3 {
		t.Fatalf("imported = %d, want 3 (items=%+v)", resp.Imported, resp.Items)
	}
	if resp.ProxiesImported == nil || *resp.ProxiesImported != 1 {
		t.Fatalf("proxies_imported = %v, want 1 after dedup", resp.ProxiesImported)
	}
	if resp.ProxiesSkipped == nil || *resp.ProxiesSkipped != 1 {
		t.Fatalf("proxies_skipped = %v, want 1", resp.ProxiesSkipped)
	}
	if !strings.Contains(resp.ProxyWarning, "格式无效") {
		t.Fatalf("proxy_warning = %q, want the invalid-proxy warning", resp.ProxyWarning)
	}
	for _, tc := range []struct{ token, want string }{
		{"rt-a", "http://127.0.0.1:8080"},
		{"rt-b", "http://127.0.0.1:8080"},
		// 非法代理被清空后必须退回表单值，而不是绑着一个没进池的 URL。
		{"rt-c", "http://127.0.0.1:7070"},
	} {
		if row := grokAccountRowByRefreshToken(t, db, tc.token); row.ProxyURL != tc.want {
			t.Fatalf("account %s proxy_url = %q, want %q", tc.token, row.ProxyURL, tc.want)
		}
	}
}

type antigravityProxyImportResponse struct {
	Imported        int                       `json:"imported"`
	Items           []AntigravityImportItemJS `json:"items"`
	ProxiesImported *int                      `json:"proxies_imported"`
	ProxyWarning    string                    `json:"proxy_warning"`
}

// AntigravityImportItemJS 只取断言要用的字段，避免和内部结构体耦合。
type AntigravityImportItemJS struct {
	ID    int64  `json:"id"`
	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

func doAntigravityProxyImport(t *testing.T, handler *Handler, body map[string]any) antigravityProxyImportResponse {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	recorder := httptest.NewRecorder()
	requestContext, _ := gin.CreateTestContext(recorder)
	requestContext.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/antigravity/import", strings.NewReader(string(encoded)))
	requestContext.Request.Header.Set("Content-Type", "application/json")
	handler.BatchImportAntigravityAccounts(requestContext)
	if recorder.Code != http.StatusOK {
		t.Fatalf("BatchImportAntigravityAccounts status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	var resp antigravityProxyImportResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v, body=%s", err, recorder.Body.String())
	}
	return resp
}

// Antigravity 导入一直会采用文件里的 proxy_url，开关只决定它要不要入代理表。
// 入表这一步不能少：代理池开启后，绑着未入表 URL 的账号在管理页看不见、也进不了
// 轮转；而一旦入表就必须同步内存池，否则整批号会被判为无可用出口。
func TestBatchImportAntigravityRegistersFileProxy(t *testing.T) {
	file := `{"auth_kind":"api_key","api_key":"sk-antigravity-1","name":"ag-1","proxy_url":"http://127.0.0.1:8080"}`

	t.Run("开关打开时代理入表并同步池", func(t *testing.T) {
		handler, store, db := newGrokProxyImportHandler(t)
		resp := doAntigravityProxyImport(t, handler, map[string]any{
			"files": []string{file}, "import_proxy": true,
		})
		if resp.Imported != 1 || len(resp.Items) != 1 || !resp.Items[0].OK {
			t.Fatalf("import = %+v, want one imported account", resp)
		}
		if resp.ProxiesImported == nil || *resp.ProxiesImported != 1 {
			t.Fatalf("proxies_imported = %v, want 1", resp.ProxiesImported)
		}
		row, err := db.GetAccountByID(context.Background(), resp.Items[0].ID)
		if err != nil {
			t.Fatalf("GetAccountByID: %v", err)
		}
		if row.ProxyURL != "http://127.0.0.1:8080" {
			t.Fatalf("account proxy_url = %q, want the file-carried proxy", row.ProxyURL)
		}
		if account := store.FindByID(row.ID); account != nil && !store.AccountHasUsableEgress(account) {
			t.Fatal("账号绑了文件内代理却没有可用出口，代理池没同步上")
		}
		proxies, err := db.ListProxies(context.Background())
		if err != nil {
			t.Fatalf("ListProxies: %v", err)
		}
		if len(proxies) != 1 || proxies[0].URL != "http://127.0.0.1:8080" {
			t.Fatalf("proxies table = %+v, want the file proxy registered", proxies)
		}
	})

	// 开关关闭时维持既有行为：账号照旧绑上文件里的代理，只是不入表。
	t.Run("开关关闭时仍绑定但不入表", func(t *testing.T) {
		handler, _, db := newGrokProxyImportHandler(t)
		resp := doAntigravityProxyImport(t, handler, map[string]any{"files": []string{file}})
		if resp.Imported != 1 || len(resp.Items) != 1 || !resp.Items[0].OK {
			t.Fatalf("import = %+v, want one imported account", resp)
		}
		if resp.ProxiesImported != nil {
			t.Fatalf("proxies_imported = %v, want the field to be absent when the switch is off", *resp.ProxiesImported)
		}
		row, err := db.GetAccountByID(context.Background(), resp.Items[0].ID)
		if err != nil {
			t.Fatalf("GetAccountByID: %v", err)
		}
		if row.ProxyURL != "http://127.0.0.1:8080" {
			t.Fatalf("account proxy_url = %q, want the file proxy to still be bound", row.ProxyURL)
		}
		proxies, err := db.ListProxies(context.Background())
		if err != nil {
			t.Fatalf("ListProxies: %v", err)
		}
		if len(proxies) != 0 {
			t.Fatalf("proxies table = %+v, want empty when the switch is off", proxies)
		}
	})
}

// Antigravity 导出写 proxy_enabled，导入侧要认下来，否则"源端禁用"的告警永远不触发。
func TestAntigravityImportDocumentCarriesProxyEnabled(t *testing.T) {
	proxyEnabled := true
	proxyDisabled := false
	cases := []struct {
		name        string
		content     string
		wantURL     string
		wantEnabled *bool
	}{
		{
			name:    "带代理不带启用状态",
			content: `{"auth_kind":"api_key","api_key":"sk-1","proxy_url":"http://127.0.0.1:8080"}`,
			wantURL: "http://127.0.0.1:8080",
		},
		{
			name:        "源端禁用",
			content:     `{"auth_kind":"api_key","api_key":"sk-1","proxy_url":"http://127.0.0.1:8080","proxy_enabled":false}`,
			wantURL:     "http://127.0.0.1:8080",
			wantEnabled: &proxyDisabled,
		},
		{
			name:        "源端启用",
			content:     `{"auth_kind":"api_key","api_key":"sk-1","proxy_url":"http://127.0.0.1:8080","proxy_enabled":true}`,
			wantURL:     "http://127.0.0.1:8080",
			wantEnabled: &proxyEnabled,
		},
		{
			name:    "旧文件不带代理",
			content: `{"auth_kind":"api_key","api_key":"sk-1"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			documents, err := parseAntigravityImportDocuments(tc.content, antigravityImportDefaults{})
			if err != nil {
				t.Fatalf("parseAntigravityImportDocuments: %v", err)
			}
			if len(documents) != 1 {
				t.Fatalf("documents = %d, want 1", len(documents))
			}
			if documents[0].ProxyURL != tc.wantURL {
				t.Fatalf("ProxyURL = %q, want %q", documents[0].ProxyURL, tc.wantURL)
			}
			got := documents[0].ProxyEnabled
			switch {
			case tc.wantEnabled == nil && got != nil:
				t.Fatalf("ProxyEnabled = %t, want nil", *got)
			case tc.wantEnabled != nil && got == nil:
				t.Fatalf("ProxyEnabled = nil, want %t", *tc.wantEnabled)
			case tc.wantEnabled != nil && *got != *tc.wantEnabled:
				t.Fatalf("ProxyEnabled = %t, want %t", *got, *tc.wantEnabled)
			}
		})
	}
}
