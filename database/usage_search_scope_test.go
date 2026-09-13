package database

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestUsageSearchScopeIsolatesFieldsAcrossPagesSummaryAndExport(test *testing.T) {
	db := newGrokStateTestDB(test)
	const needle = "01a09012"
	accountID, err := db.InsertAccountWithCredentials(test.Context(), needle, map[string]interface{}{"email": "scope@example.com"}, "")
	require.NoError(test, err)
	rows := []struct {
		scope string
		input UsageLogInput
	}{
		{"session", UsageLogInput{SessionIDPrefix: needle}},
		{"request", UsageLogInput{RequestID: needle}},
		{"error", UsageLogInput{ErrorMessage: needle}},
		{"model", UsageLogInput{Model: needle}},
		{"endpoint", UsageLogInput{InboundEndpoint: needle}},
		{"account", UsageLogInput{AccountID: accountID}},
		{"newapi_user", UsageLogInput{NewAPIUserName: needle}},
		{"ip", UsageLogInput{ClientIP: needle}},
		{"user_agent", UsageLogInput{ClientUserAgent: needle}},
		{"api_key", UsageLogInput{APIKeyName: needle}},
	}
	for _, row := range rows {
		input := row.input
		input.StatusCode, input.Endpoint = 500, "/v1/responses"
		require.NoError(test, db.InsertUsageLog(test.Context(), &input))
	}
	db.FlushUsageLogs()
	filter := UsageLogFilter{Start: time.Now().Add(-time.Hour), End: time.Now().Add(time.Hour), Query: needle, Page: 1, PageSize: 1}
	for _, row := range rows {
		test.Run(row.scope, func(test *testing.T) {
			filter.SearchScope = row.scope
			page, err := db.ListUsageLogsByTimeRangePaged(test.Context(), filter)
			require.NoError(test, err)
			require.EqualValues(test, 1, page.Total, "the same text in unrelated fields must not match")
			require.Len(test, page.Logs, 1)
			summary, err := db.GetUsageErrorSummary(test.Context(), filter)
			require.NoError(test, err)
			require.EqualValues(test, 1, summary.TotalErrors)
			var ids []int64
			require.NoError(test, db.WalkUsageLogsForExport(test.Context(), &filter, func(entry *UsageLogExportEntry) error {
				ids = append(ids, entry.ID)
				return nil
			}))
			require.Equal(test, []int64{page.Logs[0].ID}, ids)
		})
	}
	for _, scope := range []string{"", "all"} {
		filter.SearchScope = scope
		page, err := db.ListUsageLogsByTimeRangePaged(test.Context(), filter)
		require.NoError(test, err)
		require.EqualValues(test, len(rows), page.Total, "legacy search keeps all-field behavior")
	}
	filter.SearchScope, filter.Query = "error", "' OR 1=1 --"
	page, err := db.ListUsageLogsByTimeRangePaged(test.Context(), filter)
	require.NoError(test, err)
	require.Zero(test, page.Total)
}
