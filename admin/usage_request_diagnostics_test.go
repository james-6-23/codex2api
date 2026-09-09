package admin

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestUsageRequestDiagnosticsEndpointRequiresAdmin(test *testing.T) {
	db := newTestAdminDB(test)
	if err := db.InsertUsageLog(test.Context(), &database.UsageLogInput{StatusCode: 200, Endpoint: "/v1/responses", RequestType: "user", RequestDiagnostics: `{"version":1,"selected_account_id":17}`}); err != nil {
		test.Fatal(err)
	}
	db.FlushUsageLogs()
	logs, err := db.ListRecentUsageLogs(test.Context(), 10)
	if err != nil || len(logs) != 1 {
		test.Fatalf("logs=%+v, err=%v", logs, err)
	}
	handler := &Handler{db: db, adminSecretEnv: "test-diagnostic-admin"}
	router := gin.New()
	handler.RegisterRoutes(router)
	for _, item := range []struct {
		id     string
		secret string
		status int
	}{
		{fmt.Sprint(logs[0].ID), "", http.StatusUnauthorized},
		{fmt.Sprint(logs[0].ID), "invalid", http.StatusUnauthorized},
		{fmt.Sprint(logs[0].ID), "test-diagnostic-admin", http.StatusOK},
		{"-1", "test-diagnostic-admin", http.StatusBadRequest},
		{"invalid", "test-diagnostic-admin", http.StatusBadRequest},
		{"999999", "test-diagnostic-admin", http.StatusNotFound},
	} {
		request := httptest.NewRequest(http.MethodGet, "/api/admin/usage/logs/"+item.id+"/diagnostics", nil)
		request.Header.Set("X-Admin-Key", item.secret)
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if recorder.Code != item.status {
			test.Fatalf("id=%s auth=%t: status=%d, body=%s", item.id, item.secret != "", recorder.Code, recorder.Body.String())
		}
		if strings.Contains(recorder.Body.String(), "selected_account_id") != (item.status == http.StatusOK) {
			test.Fatalf("unexpected detail visibility: %s", recorder.Body.String())
		}
	}
}
