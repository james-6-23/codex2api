package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

func TestFolderImportSharedIDAcrossBatches(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	h := &Handler{db: db, store: auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2}),
		probeUsage: func(context.Context, *auth.Account) error { return nil },
	}
	// Separate multipart requests are the actual boundary between folder batches.
	for _, name := range []string{"first", "second", "first"} {
		content := fmt.Sprintf(`{"accounts":[{"name":%q,"credentials":{"email":%q,"refresh_token":%q,"access_token":%q,"chatgpt_account_id":"shared-export-id"}}]}`, name, name+"@example.invalid", "fixture-rt-"+name, "fixture-at-"+name)
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = newMultipartJSONRequest(t, name+".json", content)
		h.importAccountsJSON(c, importSettings{})
		if w.Code != http.StatusOK {
			t.Fatalf("import %s: status %d", name, w.Code)
		}
	}
	rows, err := db.ListActive(context.Background())
	if err != nil || len(rows) != 2 {
		t.Fatalf("after two batches and replay: got %d rows, err %v", len(rows), err)
	}
	seen := map[string]bool{}
	for _, row := range rows {
		seen[row.GetCredential("refresh_token")] = true
	}
	if !seen["fixture-rt-first"] || !seen["fixture-rt-second"] {
		t.Fatal("distinct imported credentials were lost or overwritten")
	}
}

const testAccountImportPath = "/api/admin/accounts/import"
const testFolderImportPath = "/api/admin/accounts/folder/import"

func testFolderImportRequest(t *testing.T, router *gin.Engine, key, phase, session string, index int, files []string, opts map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	path := testAccountImportPath
	if phase != "" {
		path = testFolderImportPath
	}
	var body bytes.Buffer
	var contentType string
	if len(files) > 0 {
		form := multipart.NewWriter(&body)
		for name, value := range opts {
			if err := form.WriteField(name, value); err != nil {
				t.Fatal(err)
			}
		}
		for i, file := range files {
			part, err := form.CreateFormFile("file", fmt.Sprintf("fixture-%d.json", i))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := part.Write([]byte(file)); err != nil {
				t.Fatal(err)
			}
		}
		if err := form.Close(); err != nil {
			t.Fatal(err)
		}
		contentType = form.FormDataContentType()
	}
	req := httptest.NewRequest(http.MethodPost, path, &body)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("X-Admin-Key", key)
	if phase != "" {
		req.Header.Set(accountImportPhaseHeader, phase)
		req.Header.Set(accountImportBatchHeader, strconv.Itoa(index))
		req.Header.Set(accountImportSessionHeader, session)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

func testFolderImportRouter(h *Handler) *gin.Engine {
	router := gin.New()
	router.Use(security.RequestSizeLimiter(security.DefaultMaxRequestBodySize))
	h.RegisterRoutes(router)
	return router
}

func testFolderImportResult(t *testing.T, w *httptest.ResponseRecorder) importEvent {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("import status = %d: %s", w.Code, w.Body.String())
	}
	var result importEvent
	if !strings.HasPrefix(w.Header().Get("Content-Type"), "text/event-stream") {
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if payload, found := strings.CutPrefix(line, "data: "); found {
			if err := json.Unmarshal([]byte(payload), &result); err != nil {
				t.Fatal(err)
			}
			if result.Type == "complete" {
				return result
			}
		}
	}
	t.Fatal("missing complete event")
	return importEvent{}
}

func TestFolderImportOAuthConflictAcrossBatches(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const key = "folder-fixture-admin"
	ctx := context.Background()
	entry := func(email, workspace, token string) string {
		// Upstream derives the workspace from JWT claims, not an exported workspace_id.
		idToken := makeOAuthTestIDToken(strings.ToLower(strings.TrimSpace(email)), workspace, "team")
		return fmt.Sprintf(`{"credentials":{"email":%q,"id_token":%q,"refresh_token":%q,"access_token":%q,"chatgpt_account_id":"shared-export-id"}}`, email, idToken, "fixture-rt-"+token, "fixture-at-"+token)
	}
	doc := func(accounts ...string) string { return `{"accounts":[` + strings.Join(accounts, ",") + `]}` }
	for _, tc := range []struct {
		name      string
		files     int
		batchSize int
		reverse   bool
		format    string
	}{
		{"100-single", 100, 100, false, "json"},
		{"101-split", 101, 100, false, "json"},
		{"101-reverse", 101, 100, true, "json"},
		{"100-early-split", 100, 50, false, "json"},
		{"101-prefer-at", 101, 100, false, "json_at"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newTestAdminDB(t)
			h := &Handler{db: db, adminSecretEnv: key, store: auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2}),
				probeUsage: func(context.Context, *auth.Account) error { return nil },
			}
			router := testFolderImportRouter(h)
			h.store.SetLazyMode(true)
			t.Cleanup(h.store.Stop)
			existingID, err := db.InsertAccountWithCredentials(ctx, "existing", map[string]any{
				"email": "existing@example.invalid", "workspace_id": "workspace-existing",
				"refresh_token": "fixture-rt-existing-original", "access_token": "fixture-at-existing-original",
			}, "")
			if err != nil {
				t.Fatal(err)
			}

			normal := entry("normal@example.invalid", "workspace-normal", "normal")
			files := make([]string, tc.files)
			for i := range files {
				files[i] = "[]"
			}
			files[0] = doc(entry(" Absent@Example.Invalid ", "workspace-absent", "absent-first"), entry("existing@example.invalid", "workspace-existing", "existing-first"), normal)
			files[1] = doc(entry("normal@example.invalid", "workspace-other", "other"))
			files[len(files)-1] = doc(entry("absent@example.invalid", "workspace-absent", "absent-second"), entry(" Existing@Example.Invalid ", "workspace-existing", "existing-second"), normal)
			if tc.reverse {
				slices.Reverse(files)
			}
			var totals importEvent
			opts := map[string]string{"format": tc.format}
			if tc.files == tc.batchSize {
				totals = testFolderImportResult(t, testFolderImportRequest(t, router, key, "", "", 0, files, opts))
			} else {
				first := testFolderImportRequest(t, router, key, "stage", "", 0, files[:tc.batchSize], opts)
				session := first.Header().Get(accountImportSessionHeader)
				if first.Code != http.StatusNoContent || session == "" {
					t.Fatalf("first stage status = %d", first.Code)
				}
				if first.Header().Get("X-Codex2API-Max-Request-Body-Bytes") != strconv.FormatInt(security.MaxImportBodySize, 10) {
					t.Fatal("folder staging must inherit the existing import body limit")
				}
				defer testFolderImportRequest(t, router, key, "cancel", session, 0, nil, nil)
				last := testFolderImportRequest(t, router, key, "stage-last", session, 1, files[tc.batchSize:], nil)
				if last.Code != http.StatusNoContent {
					t.Fatalf("last stage status = %d: %s", last.Code, last.Body.String())
				}
				for i := range 2 {
					if early := testFolderImportRequest(t, router, key, "commit", session, 0, nil, nil); early.Code != http.StatusConflict {
						t.Fatalf("premature commit status = %d", early.Code)
					}
					prepared := testFolderImportRequest(t, router, key, "prepare", session, i, nil, nil)
					if prepared.Code != http.StatusNoContent {
						t.Fatalf("prepare %d status = %d: %s", i, prepared.Code, prepared.Body.String())
					}
				}
				if rows, err := db.ListActive(ctx); err != nil || len(rows) != 1 || rows[0].GetCredential("access_token") != "fixture-at-existing-original" {
					t.Fatalf("preflight mutated accounts: rows=%d err=%v", len(rows), err)
				}
				for i := range 2 {
					result := testFolderImportResult(t, testFolderImportRequest(t, router, key, "commit", session, i, nil, nil))
					totals.Total += result.Total
					totals.Success += result.Success
					totals.Duplicate += result.Duplicate
					totals.Updated += result.Updated
					totals.Failed += result.Failed
				}
			}
			if totals.Total != 6 || totals.Success != 2 || totals.Duplicate != 4 || totals.Updated != 0 || totals.Failed != 0 {
				t.Fatalf("unexpected import counters: %+v", totals)
			}
			rows, err := db.ListActive(ctx)
			if err != nil || len(rows) != 3 {
				t.Fatalf("after import: %d rows, err %v", len(rows), err)
			}
			seen := map[string]bool{}
			for _, row := range rows {
				seen[row.GetCredential("access_token")] = true
				if row.ID == existingID && row.GetCredential("refresh_token") != "fixture-rt-existing-original" {
					t.Fatal("existing refresh token was overwritten")
				}
				if tc.format == "json_at" && row.ID != existingID && row.GetCredential("refresh_token") != "" {
					t.Fatal("AT-only folder import kept a refresh token")
				}
			}
			if !seen["fixture-at-existing-original"] || !seen["fixture-at-normal"] || !seen["fixture-at-other"] {
				t.Fatal("distinct workspaces with the same export ID must survive, and old credentials must be unchanged")
			}
			// A later explicit, unambiguous API import can still update this identity.
			explicit := doc(entry("existing@example.invalid", "workspace-existing", "explicit-update"))
			updated := testFolderImportResult(t, testFolderImportRequest(t, router, key, "", "", 0, []string{explicit}, opts))
			row, err := db.GetAccountByID(ctx, existingID)
			if err != nil || updated.Updated != 1 || row.GetCredential("access_token") != "fixture-at-explicit-update" {
				t.Fatalf("explicit API import did not update the original row: result %+v err %v", updated, err)
			}
		})
	}
}

func TestFolderImportPrepareAndCancelDoNotPersistProxies(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h, rawDB := newImportProxyTestHandler(t)
	h.adminSecretEnv = "fixture-first-admin"
	router := testFolderImportRouter(h)
	files := []string{`{"refresh_token":"fixture-rt","proxy_url":"http://127.0.0.1:8080"}`}
	opts := map[string]string{"format": "json", "import_proxy": "true"}
	staged := testFolderImportRequest(t, router, h.adminSecretEnv, "stage-last", "", 0, files, opts)
	session := staged.Header().Get(accountImportSessionHeader)
	if staged.Code != http.StatusNoContent || session == "" {
		t.Fatalf("stage status = %d", staged.Code)
	}
	h.adminSecretEnv = "fixture-second-admin"
	if got := testFolderImportRequest(t, router, h.adminSecretEnv, "prepare", session, 0, nil, nil); got.Code != http.StatusConflict {
		t.Fatalf("a different admin credential claimed the folder session: status %d", got.Code)
	}
	h.adminSecretEnv = "fixture-first-admin"
	if got := testFolderImportRequest(t, router, h.adminSecretEnv, "prepare", session, 0, nil, nil); got.Code != http.StatusNoContent {
		t.Fatalf("prepare status = %d", got.Code)
	}
	if got := testFolderImportRequest(t, router, h.adminSecretEnv, "cancel", session, 0, nil, nil); got.Code != http.StatusNoContent {
		t.Fatalf("cancel status = %d", got.Code)
	}
	if got := testFolderImportRequest(t, router, h.adminSecretEnv, "commit", session, 0, nil, nil); got.Code != http.StatusConflict {
		t.Fatalf("commit after cancel status = %d", got.Code)
	}
	if len(proxyURLsInTable(t, rawDB)) != 0 || h.accountImportFolders.bytes != 0 {
		t.Fatal("abandoned folder retained proxies or retained budget")
	}
	if rows, err := h.db.ListActive(context.Background()); err != nil || len(rows) != 0 {
		t.Fatalf("abandoned folder changed accounts: rows=%d err=%v", len(rows), err)
	}
}

func TestFolderImportUsesEffectiveWorkspaceRoute(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const key = "folder-fixture-admin"
	db := newTestAdminDB(t)
	router := testFolderImportRouter(&Handler{db: db, adminSecretEnv: key})
	const email = "routed@example.invalid"
	file := func(workspace, rt string) string {
		return fmt.Sprintf(`{"email":%q,"id_token":%q,"refresh_token":%q}`, email, makeOAuthTestIDToken(email, workspace, "team"), rt)
	}
	opts := map[string]string{"format": "json", "custom_headers": `{"ChatGPT-Account-ID":"chosen-workspace"}`, "allow_duplicate": "true"}
	first := testFolderImportRequest(t, router, key, "stage", "", 0, []string{file("jwt-one", "fixture-one")}, opts)
	session := first.Header().Get(accountImportSessionHeader)
	if first.Code != http.StatusNoContent || session == "" {
		t.Fatalf("first stage: %d %s", first.Code, first.Body.String())
	}
	defer testFolderImportRequest(t, router, key, "cancel", session, 0, nil, nil)
	if last := testFolderImportRequest(t, router, key, "stage-last", session, 1, []string{file("jwt-two", "fixture-two")}, nil); last.Code != http.StatusNoContent {
		t.Fatalf("last stage: %d %s", last.Code, last.Body.String())
	}
	for i := range 2 {
		if prepared := testFolderImportRequest(t, router, key, "prepare", session, i, nil, nil); prepared.Code != http.StatusNoContent {
			t.Fatalf("prepare %d: %d %s", i, prepared.Code, prepared.Body.String())
		}
	}
	for i := range 2 {
		got := testFolderImportResult(t, testFolderImportRequest(t, router, key, "commit", session, i, nil, nil))
		if got.Total != 1 || got.Duplicate != 1 || got.Success != 0 || got.Updated != 0 || got.Failed != 0 {
			t.Fatalf("batch %d did not skip the routed identity: %+v", i, got)
		}
	}
	if rows, err := db.ListActive(context.Background()); err != nil || len(rows) != 0 {
		t.Fatalf("conflicting routed identity persisted: rows=%d err=%v", len(rows), err)
	}
}

func TestFolderImportBatchSize(t *testing.T) {
	header := func(size int64) *multipart.FileHeader { return &multipart.FileHeader{Size: size} }
	atFileLimit := make([]*multipart.FileHeader, 100)
	for i := range atFileLimit {
		atFileLimit[i] = header(1)
	}
	const MiB = 1 << 20
	for _, tc := range []struct {
		files  []*multipart.FileHeader
		bytes  int64
		status int
	}{
		{atFileLimit, 100, 0},
		{append(atFileLimit, header(1)), 0, http.StatusBadRequest},
		{[]*multipart.FileHeader{header(149 * MiB), header(MiB)}, 150 * MiB, 0},
		{[]*multipart.FileHeader{header(149 * MiB), header(MiB + 1)}, 0, http.StatusRequestEntityTooLarge},
		{[]*multipart.FileHeader{header(151 * MiB)}, 151 * MiB, 0},
	} {
		if bytes, status := folderImportBatchSize(tc.files); bytes != tc.bytes || status != tc.status {
			t.Fatalf("%d files: got (%d, %d), want (%d, %d)", len(tc.files), bytes, status, tc.bytes, tc.status)
		}
	}
}
