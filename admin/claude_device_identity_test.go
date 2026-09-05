package admin

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestClaudeDeviceIDSurvivesAdminSaveExportAndImport(t *testing.T) {
	db := newTestAdminDB(t)
	id, err := db.InsertAccountWithUpstream(context.Background(), "device-test", "anthropic", auth.UpstreamClaude, map[string]interface{}{
		"upstream_type": auth.UpstreamClaude,
		"access_token":  "at-device-test", "refresh_token": "rt-device-test", "account_id": "acct-device-test",
		"models": []string{"claude-haiku-4-5"},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(db, nil, nil)
	t.Cleanup(store.Stop)
	account := &auth.Account{DBID: id, UpstreamType: auth.UpstreamClaude, AccountID: "acct-device-test", AccessToken: "at-device-test", RefreshToken: "rt-device-test"}
	store.AddAccount(account)
	h := &Handler{db: db, store: store}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(id, 10)}}
	c.Request = httptest.NewRequest(http.MethodPatch, "/api/admin/accounts/"+strconv.FormatInt(id, 10)+"/scheduler", strings.NewReader(`{"custom_headers":{"claude_device_id":"explicit-device-id"}}`))
	h.UpdateAccountScheduler(c)
	if recorder.Code != http.StatusOK {
		t.Fatalf("save status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	row, err := db.GetAccountByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if row.GetCredentialStringMap("custom_headers")["Claude_device_id"] != "explicit-device-id" {
		t.Fatal("save must canonicalize the metadata key")
	}
	if got := account.ClaudeDeviceID(); got != "explicit-device-id" {
		t.Fatalf("runtime device ID after save = %q", got)
	}
	entry, ok := claudeAccountRowToExportEntry(row, nil)
	if !ok {
		t.Fatal("saved account must be exportable")
	}
	encoded, err := marshalClaudeExportEntry(entry)
	if err != nil {
		t.Fatal(err)
	}
	importDB := newTestAdminDB(t)
	importHandler := &Handler{db: importDB}
	importRecorder := httptest.NewRecorder()
	c, _ = gin.CreateTestContext(importRecorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/claude/import", bytes.NewReader(encoded))
	importHandler.ImportClaudeToken(c)
	if importRecorder.Code != http.StatusOK {
		t.Fatalf("import status=%d body=%s", importRecorder.Code, importRecorder.Body.String())
	}
	rows, err := importDB.ListActiveByChannel(context.Background(), database.UpstreamChannelClaude)
	if err != nil || len(rows) != 1 {
		t.Fatalf("imported rows=%d err=%v", len(rows), err)
	}
	reloaded := &auth.Account{AccountID: rows[0].GetCredential("account_id"), CustomHeaders: rows[0].GetCredentialStringMap("custom_headers")}
	if got := reloaded.ClaudeDeviceID(); got != "explicit-device-id" {
		t.Fatalf("reloaded imported device ID = %q", got)
	}
}

func TestClaudeDeviceIDSurvivesTimezoneFingerprintRebuild(t *testing.T) {
	for _, tc := range []struct {
		name     string
		timezone string
		headers  map[string]string
		want     string
	}{
		{"same timezone", "Asia/Shanghai", nil, "stored-device"},
		{"new timezone", "America/New_York", nil, "stored-device"},
		{"header patch omits metadata", "America/New_York", map[string]string{"X-Request-Id": "new-trace"}, "stored-device"},
		{"explicit metadata replacement", "America/New_York", map[string]string{"CLAUDE_DEVICE_ID": "replacement-device"}, "replacement-device"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			row := &database.AccountRow{Platform: "anthropic", Credentials: map[string]interface{}{
				"upstream_type": auth.UpstreamClaude, "timezone": "Asia/Shanghai",
				"custom_headers": map[string]string{"Claude_device_id": "stored-device", "Authorization": "must-drop"},
			}}
			updates := make(map[string]interface{})
			applied, err := prepareClaudeTimezoneCredentialUpdateWithHeaders(row, tc.timezone, updates, tc.headers)
			if err != nil || !applied {
				t.Fatalf("timezone rebuild applied=%t err=%v", applied, err)
			}
			headers, ok := updates["custom_headers"].(map[string]string)
			if !ok {
				t.Fatalf("rebuilt headers have type %T", updates["custom_headers"])
			}
			account := &auth.Account{CustomHeaders: headers}
			if got := account.ClaudeDeviceID(); got != tc.want {
				t.Fatalf("device ID after rebuild = %q, want %q", got, tc.want)
			}
			if headers["Authorization"] != "" {
				t.Fatal("preserving identity metadata must not preserve arbitrary credentials")
			}
		})
	}
}
