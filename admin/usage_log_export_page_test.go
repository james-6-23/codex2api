package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestUsageLogExportPagesFreezeSnapshotAndKeepExactDiagnosticNumbers(test *testing.T) {
	handler := &Handler{db: newTestAdminDB(test)}
	for index := 0; index < 250; index++ {
		require.NoError(test, handler.db.InsertUsageLog(test.Context(), &database.UsageLogInput{StatusCode: 200, RequestType: "user", NewAPIUserName: "paged-user", RequestID: fmt.Sprintf("original-%d", index), RequestDiagnostics: `{"counter":9007199254740993,"Authorization":"Bearer never-export"}`}))
	}
	handler.db.FlushUsageLogs()
	query := url.Values{"scope": {"all"}, "confirmed": {"true"}, "paged": {"true"}}
	seen := make(map[int64]bool)
	metadata := ""
	for pageIndex := 0; pageIndex < 5; pageIndex++ {
		response := httptest.NewRecorder()
		request, _ := gin.CreateTestContext(response)
		request.Request = httptest.NewRequest(http.MethodPost, "/usage/logs/export?"+query.Encode(), nil)
		handler.ExportUsageLogs(request)
		require.Equal(test, http.StatusOK, response.Code, response.Body.String())
		var page struct {
			Metadata string   `json:"metadata"`
			Records  []string `json:"records"`
			Next     string   `json:"next_cursor"`
			Complete bool     `json:"complete"`
		}
		require.NoError(test, json.Unmarshal(response.Body.Bytes(), &page))
		if metadata == "" {
			metadata = page.Metadata
		}
		require.Equal(test, metadata, page.Metadata)
		require.LessOrEqual(test, len(page.Records), 200)
		for _, raw := range page.Records {
			var record struct {
				ID        int64  `json:"id"`
				RequestID string `json:"request_id"`
			}
			require.NoError(test, json.Unmarshal([]byte(raw), &record))
			require.False(test, seen[record.ID])
			seen[record.ID] = true
			require.Contains(test, record.RequestID, "original-")
			require.Contains(test, raw, "9007199254740993")
			require.NotContains(test, raw, "never-export")
		}
		if page.Complete {
			require.Empty(test, page.Next)
			break
		}
		require.NotEmpty(test, page.Next)
		query.Set("cursor", page.Next)
		if pageIndex == 0 {
			require.NoError(test, handler.db.InsertUsageLog(test.Context(), &database.UsageLogInput{StatusCode: 200, RequestID: "new-during-export"}))
			handler.db.FlushUsageLogs()
		}
	}
	require.Len(test, seen, 250)
	query.Set("scope", "filtered")
	query.Set("start", time.Now().Add(-time.Hour).Format(time.RFC3339))
	query.Set("end", time.Now().Add(time.Hour).Format(time.RFC3339))
	response := httptest.NewRecorder()
	request, _ := gin.CreateTestContext(response)
	request.Request = httptest.NewRequest(http.MethodPost, "/usage/logs/export?"+query.Encode(), nil)
	handler.ExportUsageLogs(request)
	require.Equal(test, http.StatusBadRequest, response.Code)
	require.False(test, handler.usageLogExportBusy.Load())
}

func TestUsageLogExportPagesEmptyInvalidAndUnconfirmed(test *testing.T) {
	handler := &Handler{db: newTestAdminDB(test)}
	for _, scenario := range []struct {
		query  string
		status int
	}{
		{"scope=all&paged=true&confirmed=true", http.StatusOK},
		{"scope=all&paged=true", http.StatusBadRequest},
		{"scope=all&paged=true&confirmed=true&cursor=bad", http.StatusBadRequest},
	} {
		response := httptest.NewRecorder()
		request, _ := gin.CreateTestContext(response)
		request.Request = httptest.NewRequest(http.MethodPost, "/usage/logs/export?"+scenario.query, nil)
		handler.ExportUsageLogs(request)
		require.Equal(test, scenario.status, response.Code, response.Body.String())
		if scenario.status == http.StatusOK {
			require.Contains(test, response.Body.String(), `"records":[]`)
			require.Contains(test, response.Body.String(), `"complete":true`)
		}
	}
}

func TestUsageLogExportPageByteLimitDoesNotSkipTheNextRecord(test *testing.T) {
	handler := &Handler{db: newTestAdminDB(test)}
	userAgent := strings.Repeat("a", 96*1024)
	for index := 0; index < 55; index++ {
		require.NoError(test, handler.db.InsertUsageLog(test.Context(), &database.UsageLogInput{StatusCode: 200, RequestID: fmt.Sprintf("large-%d", index), ClientUserAgent: userAgent}))
	}
	handler.db.FlushUsageLogs()
	query := url.Values{"scope": {"all"}, "confirmed": {"true"}, "paged": {"true"}}
	seen := make(map[int64]bool)
	for pageIndex := 0; pageIndex < 5; pageIndex++ {
		response := httptest.NewRecorder()
		request, _ := gin.CreateTestContext(response)
		request.Request = httptest.NewRequest(http.MethodPost, "/usage/logs/export?"+query.Encode(), nil)
		handler.ExportUsageLogs(request)
		require.Equal(test, http.StatusOK, response.Code)
		var page struct {
			Records  []string `json:"records"`
			Next     string   `json:"next_cursor"`
			Complete bool     `json:"complete"`
		}
		require.NoError(test, json.Unmarshal(response.Body.Bytes(), &page))
		bytes := 0
		for _, raw := range page.Records {
			var record struct {
				ID int64 `json:"id"`
			}
			require.NoError(test, json.Unmarshal([]byte(raw), &record))
			require.False(test, seen[record.ID])
			seen[record.ID] = true
			bytes += len(raw)
		}
		require.LessOrEqual(test, bytes, 4*1024*1024)
		if pageIndex == 0 {
			require.False(test, page.Complete)
		}
		if page.Complete {
			break
		}
		query.Set("cursor", page.Next)
	}
	require.Len(test, seen, 55)
}
