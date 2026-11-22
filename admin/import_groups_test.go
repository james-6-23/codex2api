package admin

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// itoa 让测试里的 JSON 片段拼接短一些。
func itoa(v int64) string { return strconv.FormatInt(v, 10) }

func newImportGroupsTestHandler(t *testing.T) (*Handler, *database.DB, *auth.Store, int64) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 4})
	t.Cleanup(store.Stop)
	groupID, err := db.CreateAccountGroup(context.Background(), "premium", "", "", 0, 0, sql.NullInt64{})
	if err != nil {
		t.Fatalf("CreateAccountGroup: %v", err)
	}
	return &Handler{db: db, store: store}, db, store, groupID
}

func TestResolveImportGroupIDsRejectsUnknownGroup(t *testing.T) {
	handler, _, _, groupID := newImportGroupsTestHandler(t)
	ctx := context.Background()

	ids, err := handler.resolveImportGroupIDsJSON(ctx, json.RawMessage(`[`+itoa(groupID)+`]`))
	if err != nil || len(ids) != 1 || ids[0] != groupID {
		t.Fatalf("resolve existing group = %v, %v; want [%d]", ids, err, groupID)
	}
	if _, err := handler.resolveImportGroupIDsJSON(ctx, json.RawMessage(`[999999]`)); err == nil {
		t.Fatal("unknown group must be rejected so a typo cannot silently skip binding")
	}
	if _, err := handler.resolveImportGroupIDsJSON(ctx, json.RawMessage(`[0]`)); err == nil {
		t.Fatal("non-positive group id must be rejected")
	}
	// 未传 / 空数组表示"不绑分组"，不是错误，也不是"清空"。
	if ids, err := handler.resolveImportGroupIDsJSON(ctx, nil); err != nil || ids != nil {
		t.Fatalf("absent group_ids = %v, %v; want nil, nil", ids, err)
	}
	if ids, err := handler.resolveImportGroupIDsJSON(ctx, json.RawMessage(`[]`)); err != nil || ids != nil {
		t.Fatalf("empty group_ids = %v, %v; want nil, nil", ids, err)
	}
}

func TestResolveImportGroupIDsFormAcceptsBothSyntaxes(t *testing.T) {
	handler, db, _, groupID := newImportGroupsTestHandler(t)
	ctx := context.Background()
	second, err := db.CreateAccountGroup(ctx, "cheap", "", "", 0, 0, sql.NullInt64{})
	if err != nil {
		t.Fatalf("CreateAccountGroup: %v", err)
	}

	jsonForm, err := handler.resolveImportGroupIDsForm(ctx, "["+itoa(groupID)+","+itoa(second)+"]")
	if err != nil || len(jsonForm) != 2 {
		t.Fatalf("json array form = %v, %v; want two ids", jsonForm, err)
	}
	// 逗号分隔便于 curl 手测。
	csvForm, err := handler.resolveImportGroupIDsForm(ctx, itoa(groupID)+", "+itoa(second))
	if err != nil || len(csvForm) != 2 {
		t.Fatalf("csv form = %v, %v; want two ids", csvForm, err)
	}
	if ids, err := handler.resolveImportGroupIDsForm(ctx, "   "); err != nil || ids != nil {
		t.Fatalf("blank form = %v, %v; want nil, nil", ids, err)
	}
	if _, err := handler.resolveImportGroupIDsForm(ctx, "abc"); err == nil {
		t.Fatal("garbage form value must be rejected")
	}
}

func TestBindImportedAccountGroupsSyncsRuntimePool(t *testing.T) {
	handler, db, store, groupID := newImportGroupsTestHandler(t)
	ctx := context.Background()
	account := &auth.Account{DBID: 77, AccessToken: "token"}
	store.AddAccount(account)

	if err := handler.bindImportedAccountGroups(ctx, []int64{77}, []int64{groupID}); err != nil {
		t.Fatalf("bindImportedAccountGroups: %v", err)
	}

	persisted, err := db.GetAccountGroupIDs(ctx, 77)
	if err != nil {
		t.Fatalf("GetAccountGroupIDs: %v", err)
	}
	if len(persisted) != 1 || persisted[0] != groupID {
		t.Fatalf("persisted groups = %v, want [%d]", persisted, groupID)
	}
	// 运行时同步是关键：分组影响并发上限/自动暂停/Key 白名单，漏了要等下次全量加载才生效。
	if runtime := account.GroupIDSnapshot(); len(runtime) != 1 || runtime[0] != groupID {
		t.Fatalf("runtime groups = %v, want [%d]", runtime, groupID)
	}

	// 空分组表示不绑，不该把已有归属清掉。
	if err := handler.bindImportedAccountGroups(ctx, []int64{77}, nil); err != nil {
		t.Fatalf("bind with empty groups: %v", err)
	}
	if persisted, _ := db.GetAccountGroupIDs(ctx, 77); len(persisted) != 1 {
		t.Fatalf("persisted groups after empty bind = %v, want them untouched", persisted)
	}
}

func TestAddATAccountBindsGroupsOnlyForNewAccounts(t *testing.T) {
	handler, db, _, groupID := newImportGroupsTestHandler(t)

	body := `{"access_token":"import-at-token-1","group_ids":[` + itoa(groupID) + `]}`
	recorder := invokeAccountGroupHandler(t, http.MethodPost, "/api/admin/accounts/at", nil, body, handler.AddATAccount)
	if recorder.Code != http.StatusOK {
		t.Fatalf("add status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Success     int     `json:"success"`
		Duplicate   int     `json:"duplicate"`
		BoundGroups bool    `json:"bound_groups"`
		GroupIDs    []int64 `json:"group_ids"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if payload.Success != 1 || !payload.BoundGroups {
		t.Fatalf("payload = %+v, want one account bound to the group", payload)
	}

	ctx := context.Background()
	accounts, err := db.ListActive(ctx)
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	if len(accounts) != 1 {
		t.Fatalf("accounts = %d, want 1", len(accounts))
	}
	newID := accounts[0].ID
	groups, err := db.GetAccountGroupIDs(ctx, newID)
	if err != nil {
		t.Fatalf("GetAccountGroupIDs: %v", err)
	}
	if len(groups) != 1 || groups[0] != groupID {
		t.Fatalf("groups = %v, want [%d]", groups, groupID)
	}

	// 同一个 AT 再导一次：命中重复跳过，且不该动已有账号的分组（此处改成另一个分组来验证）。
	other, err := db.CreateAccountGroup(ctx, "other", "", "", 0, 0, sql.NullInt64{})
	if err != nil {
		t.Fatalf("CreateAccountGroup: %v", err)
	}
	body = `{"access_token":"import-at-token-1","group_ids":[` + itoa(other) + `]}`
	recorder = invokeAccountGroupHandler(t, http.MethodPost, "/api/admin/accounts/at", nil, body, handler.AddATAccount)
	if recorder.Code != http.StatusOK {
		t.Fatalf("re-add status = %d: %s", recorder.Code, recorder.Body.String())
	}
	groups, err = db.GetAccountGroupIDs(ctx, newID)
	if err != nil {
		t.Fatalf("GetAccountGroupIDs: %v", err)
	}
	if len(groups) != 1 || groups[0] != groupID {
		t.Fatalf("groups after duplicate import = %v, want the original [%d] untouched", groups, groupID)
	}
}

func TestAddATAccountRejectsUnknownGroupBeforeCreatingAccounts(t *testing.T) {
	handler, db, _, _ := newImportGroupsTestHandler(t)

	body := `{"access_token":"import-at-token-2","group_ids":[424242]}`
	recorder := invokeAccountGroupHandler(t, http.MethodPost, "/api/admin/accounts/at", nil, body, handler.AddATAccount)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", recorder.Code, recorder.Body.String())
	}
	// 关键：校验在插账号之前，所以不该有任何账号被创建。
	accounts, err := db.ListActive(context.Background())
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	if len(accounts) != 0 {
		t.Fatalf("accounts = %d, want none created when the group id is invalid", len(accounts))
	}
}

func TestImportAccountsFormBindsGroupsForNewAccounts(t *testing.T) {
	handler, db, _, groupID := newImportGroupsTestHandler(t)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	_ = writer.WriteField("format", "at_txt")
	_ = writer.WriteField("group_ids", "["+itoa(groupID)+"]")
	filePart, err := writer.CreateFormFile("file", "accounts.txt")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := filePart.Write([]byte("file-import-at-1\nfile-import-at-2\n")); err != nil {
		t.Fatalf("write file: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/import", body)
	ginContext.Request.Header.Set("Content-Type", writer.FormDataContentType())
	handler.ImportAccounts(ginContext)

	if recorder.Code != http.StatusOK {
		t.Fatalf("import status = %d: %s", recorder.Code, recorder.Body.String())
	}

	ctx := context.Background()
	accounts, err := db.ListActive(ctx)
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	if len(accounts) != 2 {
		t.Fatalf("accounts = %d, want 2 imported", len(accounts))
	}
	for _, account := range accounts {
		groups, err := db.GetAccountGroupIDs(ctx, account.ID)
		if err != nil {
			t.Fatalf("GetAccountGroupIDs(%d): %v", account.ID, err)
		}
		if len(groups) != 1 || groups[0] != groupID {
			t.Fatalf("account %d groups = %v, want [%d]", account.ID, groups, groupID)
		}
	}
}

func TestImportAccountsRejectsUnknownGroupBeforeParsingFiles(t *testing.T) {
	handler, db, _, _ := newImportGroupsTestHandler(t)

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	_ = writer.WriteField("format", "at_txt")
	_ = writer.WriteField("group_ids", "[987654]")
	filePart, _ := writer.CreateFormFile("file", "accounts.txt")
	_, _ = filePart.Write([]byte("file-import-at-3\n"))
	_ = writer.Close()

	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/import", body)
	ginContext.Request.Header.Set("Content-Type", writer.FormDataContentType())
	handler.ImportAccounts(ginContext)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", recorder.Code, recorder.Body.String())
	}
	accounts, err := db.ListActive(context.Background())
	if err != nil {
		t.Fatalf("ListAccounts: %v", err)
	}
	if len(accounts) != 0 {
		t.Fatalf("accounts = %d, want none imported when the group id is invalid", len(accounts))
	}
}
