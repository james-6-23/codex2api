package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

// TestGrokBatchImportTimeoutScalesWithFiles 批量导入的整体超时必须跟文件数一起涨：
// 写死常量时每个文件分到的预算会被批量摊薄，5000 个文件只剩 12ms/个，
// 数据库稍慢就会中途超时、剩下的文件全部报 context deadline exceeded。
func TestGrokBatchImportTimeoutScalesWithFiles(t *testing.T) {
	cases := []struct {
		name  string
		files int
		want  time.Duration
	}{
		{"空批次只有基础预算", 0, grokBatchImportBaseTimeout},
		{"负数按 0 处理", -1, grokBatchImportBaseTimeout},
		{"500 个文件", 500, grokBatchImportBaseTimeout + 50*time.Second},
		{"5000 个文件", 5000, grokBatchImportBaseTimeout + 500*time.Second},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := grokBatchImportTimeout(tc.files); got != tc.want {
				t.Fatalf("grokBatchImportTimeout(%d) = %v, want %v", tc.files, got, tc.want)
			}
		})
	}
}

// TestGrokBatchImportTimeoutStaysBounded 上限存在的意义：一个超大请求不能无限期占住连接。
func TestGrokBatchImportTimeoutStaysBounded(t *testing.T) {
	if got := grokBatchImportTimeout(grokBatchImportMaxFiles * 10); got != grokBatchImportMaxTimeout {
		t.Fatalf("超大批次超时 = %v, want 封顶 %v", got, grokBatchImportMaxTimeout)
	}
}

// TestGrokBatchImportTimeoutCoversMaxFiles 满额导入不能一进来就撞上封顶，
// 否则每个文件的预算又会被摊薄回去。
func TestGrokBatchImportTimeoutCoversMaxFiles(t *testing.T) {
	got := grokBatchImportTimeout(grokBatchImportMaxFiles)
	if got >= grokBatchImportMaxTimeout {
		t.Fatalf("满额 %d 个文件的超时 %v 已经撞上封顶 %v，每个文件的预算会被压缩",
			grokBatchImportMaxFiles, got, grokBatchImportMaxTimeout)
	}
	if perFile := got / time.Duration(grokBatchImportMaxFiles); perFile < grokBatchImportPerFileTimeout {
		t.Fatalf("满额时每个文件只剩 %v，低于 %v", perFile, grokBatchImportPerFileTimeout)
	}
}

type grokBatchImportTestResponse struct {
	Total    int                   `json:"total"`
	Imported int                   `json:"imported"`
	Failed   int                   `json:"failed"`
	Items    []grokBatchImportItem `json:"items"`
}

func doGrokBatchImport(t *testing.T, handler *Handler, authJSON string) grokBatchImportTestResponse {
	t.Helper()
	body, err := json.Marshal(map[string]any{"files": []string{authJSON}})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	recorder := httptest.NewRecorder()
	requestContext, _ := gin.CreateTestContext(recorder)
	requestContext.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/grok/import", strings.NewReader(string(body)))
	requestContext.Request.Header.Set("Content-Type", "application/json")
	handler.BatchImportGrokAccounts(requestContext)
	if recorder.Code != http.StatusOK {
		t.Fatalf("BatchImportGrokAccounts status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	var resp grokBatchImportTestResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v, body=%s", err, recorder.Body.String())
	}
	return resp
}

// TestGrokBatchImportRevivesRecycledIdentity 覆盖 issue #602:删除过的 Grok 账号
// 仍占着凭据身份键,重新导入同一 refresh_token 不能报"已存在"死胡同,
// 必须把凭据合并回原账号并将其从回收站复活。
func TestGrokBatchImportRevivesRecycledIdentity(t *testing.T) {
	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, nil)
	t.Cleanup(store.Stop)
	handler := &Handler{db: db, store: store}
	ctx := context.Background()

	// Keep the imported credential usable so the detached post-import probe
	// cannot race this test by trying to refresh the intentionally fake token.
	authJSON := `{"refresh_token":"rt-issue-602","access_token":"at-issue-602","expires_at":"2099-01-01T00:00:00Z","client_id":"cli-602","user_id":"user-602","email":"u602@example.com"}`

	first := doGrokBatchImport(t, handler, authJSON)
	if first.Imported != 1 || len(first.Items) != 1 || !first.Items[0].OK {
		t.Fatalf("first import = %+v, want 1 new account", first)
	}
	accountID := first.Items[0].ID
	if accountID <= 0 {
		t.Fatalf("first import returned invalid account id: %+v", first.Items[0])
	}

	// 模拟 UI 删除:软删进回收站并移出运行时池。身份键仍归该账号持有。
	if err := db.SoftDeleteAccount(ctx, accountID); err != nil {
		t.Fatalf("SoftDeleteAccount: %v", err)
	}
	store.RemoveAccount(accountID)

	second := doGrokBatchImport(t, handler, authJSON)
	if second.Imported != 1 || len(second.Items) != 1 {
		t.Fatalf("re-import after delete = %+v, want revive success", second)
	}
	item := second.Items[0]
	if !item.OK || item.Error != "" {
		t.Fatalf("re-import item = %+v, want ok without error", item)
	}
	if item.ID != accountID {
		t.Fatalf("re-import merged into account %d, want original %d", item.ID, accountID)
	}
	if !item.Updated || !item.Revived {
		t.Fatalf("re-import item = %+v, want updated+revived flags", item)
	}

	row, err := db.GetAccountByID(ctx, accountID)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if !strings.EqualFold(row.Status, "active") || !row.Enabled {
		t.Fatalf("revived account status=%q enabled=%t, want active+enabled", row.Status, row.Enabled)
	}
	if store.FindByID(accountID) == nil {
		t.Fatal("revived account missing from runtime pool")
	}
}
