package admin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestUsageStatsShowRenamedAPIKeyWithWarmCache(t *testing.T) {
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "usage.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := t.Context()
	keyID, err := db.InsertAPIKey(ctx, "Original key", "sk-test-usage-rename")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := db.InsertUsageLog(ctx, &database.UsageLogInput{
		APIKeyID: keyID, APIKeyName: "Original key", Model: "gpt-5.5", StatusCode: 200, TotalTokens: 100, CreatedAt: now.Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	db.FlushUsageLogs()

	tc := cache.NewMemory(1)
	t.Cleanup(func() { tc.Close() })
	h := &Handler{db: db, cache: tc}
	router := gin.New()
	router.PATCH("/keys/:id", h.UpdateAPIKey)
	router.GET("/usage/api-keys", h.GetAPIKeyTokenStats)
	router.GET("/usage/stats", h.GetUsageStats)
	query := url.Values{"start": {now.Add(-time.Hour).Format(time.RFC3339)}, "end": {now.Format(time.RFC3339)}}

	get := func(path string, want string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path+"?"+query.Encode(), nil).WithContext(ctx)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		var body struct {
			Items       []database.APIKeyTokenStat `json:"items"`
			APIKeyStats []database.UsageAPIKeyStat `json:"api_key_stats"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", path, rec.Code, rec.Body.String())
		}
		switch path {
		case "/usage/api-keys":
			if len(body.Items) != 1 || body.Items[0].Label != want || body.Items[0].TotalTokens != 100 {
				t.Errorf("GET %s should show %q with 100 tokens: %+v", path, want, body.Items)
			}
		case "/usage/stats":
			if len(body.APIKeyStats) != 1 || body.APIKeyStats[0].Label != want || body.APIKeyStats[0].Tokens != 100 {
				t.Errorf("GET %s should show %q with 100 tokens: %+v", path, want, body.APIKeyStats)
			}
		}
	}

	get("/usage/api-keys", "Original key")
	get("/usage/stats", "Original key")
	req := httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/keys/%d", keyID), strings.NewReader(`{"name":"Renamed key"}`)).WithContext(ctx)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("PATCH = %d %s", rec.Code, rec.Body.String())
	}
	get("/usage/api-keys", "Renamed key")
	get("/usage/stats", "Renamed key")
}
