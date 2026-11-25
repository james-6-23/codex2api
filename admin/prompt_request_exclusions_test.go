package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestPromptRequestKeyExclusionsStartNewRecordingAfterRestore(t *testing.T) {
	h, db := newTestAdminHandlerFixture(t)
	ctx := context.Background()
	addKey := func(name, token string) int64 {
		t.Helper()
		id, err := db.InsertAPIKey(ctx, name, token)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	first := addKey("Same name", "first-test-key")
	second := addKey("Same name", "second-test-key")
	legacy := addKey("Legacy key", "legacy-test-key")
	body := []byte(`{"conversation_id":"test-thread","input":"Test request"}`)
	record := func(keyID, groupKeyID int64, name string) int64 {
		t.Helper()
		id, err := db.InsertPromptRequest(ctx, database.PromptRequestBody{
			PromptRequestRecord: database.PromptRequestRecord{
				CreatedAt: time.Now().UTC(), Endpoint: "/v1/responses", Transport: "http", APIKey: name,
			},
			APIKeyID: keyID, Body: body,
			ConversationGroup: database.PromptRequestConversationGroup(body, groupKeyID),
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	oldFirst := record(first, first, "Same name")
	oldSecond := record(second, second, "Same name")
	singleLegacy := record(0, 0, "Legacy key")
	groupedLegacy := record(0, first, "Old name for first key")

	router := gin.New()
	h.RegisterRoutes(router)
	request := func(method, path, body string, want int) []byte {
		t.Helper()
		req := httptest.NewRequest(method, "/api/admin/prompt-filter/"+path, strings.NewReader(body))
		req.Header.Set("X-Admin-Key", "admin-secret")
		req.Header.Set("Content-Type", "application/json")
		res := httptest.NewRecorder()
		router.ServeHTTP(res, req)
		if res.Code != want {
			t.Fatalf("%s %s: got %d, want %d (%s)", method, path, res.Code, want, res.Body.String())
		}
		return res.Body.Bytes()
	}
	checkGroups := func(want map[int64]int) {
		t.Helper()
		var page struct {
			Groups []database.PromptRequestConversation `json:"groups"`
		}
		if err := json.Unmarshal(request(http.MethodGet, "request-conversations", "", http.StatusOK), &page); err != nil {
			t.Fatal(err)
		}
		if len(page.Groups) != len(want) {
			t.Fatalf("got %d conversations, want %d", len(page.Groups), len(want))
		}
		for _, group := range page.Groups {
			if group.Rounds != want[group.ID] {
				t.Fatalf("conversation #%d has %d requests, want %d", group.ID, group.Rounds, want[group.ID])
			}
		}
	}
	checkHidden := func() {
		t.Helper()
		for _, hidden := range []int64{oldFirst, singleLegacy, groupedLegacy} {
			request(http.MethodGet, fmt.Sprintf("requests/%d", hidden), "", http.StatusNotFound)
			request(http.MethodGet, fmt.Sprintf("request-conversations/%d/tasks", hidden), "", http.StatusNotFound)
			if _, err := db.GetStoredPromptRequest(ctx, hidden); err != nil {
				t.Fatalf("retained record #%d: %v", hidden, err)
			}
		}
	}

	const settings = "requests/key-exclusions"
	checkGroups(map[int64]int{groupedLegacy: 2, oldSecond: 1, singleLegacy: 1})
	request(http.MethodPut, settings, `{}`, http.StatusBadRequest)
	request(http.MethodPut, settings, fmt.Sprintf(`{"excluded_key_ids":[%d,99999]}`, first), http.StatusBadRequest)
	checkGroups(map[int64]int{groupedLegacy: 2, oldSecond: 1, singleLegacy: 1})
	exclude := fmt.Sprintf(`{"excluded_key_ids":[%d,%d]}`, first, legacy)
	request(http.MethodPut, settings, exclude, http.StatusOK)
	request(http.MethodPut, settings, exclude, http.StatusOK)
	settingsJSON := request(http.MethodGet, settings, "", http.StatusOK)
	var config struct {
		Keys []database.PromptRequestKeyRecording `json:"keys"`
	}
	if err := json.Unmarshal(settingsJSON, &config); err != nil {
		t.Fatal(err)
	}
	if len(config.Keys) != 3 || config.Keys[0].ID != first || !config.Keys[0].Excluded ||
		config.Keys[1].ID != second || config.Keys[1].Excluded || !config.Keys[2].Excluded ||
		strings.Contains(string(settingsJSON), "first-test-key") {
		t.Fatalf("wrong exclusions or key material returned: %s", settingsJSON)
	}
	if id := record(first, first, "Same name"); id != 0 {
		t.Fatalf("excluded key produced record #%d", id)
	}
	latestSecond := record(second, second, "Same name")
	checkGroups(map[int64]int{latestSecond: 2})
	checkHidden()

	request(http.MethodPut, settings, `{"excluded_key_ids":[]}`, http.StatusOK)
	checkGroups(map[int64]int{latestSecond: 2})
	firstNew := record(first, first, "Same name")
	checkGroups(map[int64]int{firstNew: 1, latestSecond: 2})
	latestFirst := record(first, first, "Same name")
	checkGroups(map[int64]int{latestFirst: 2, latestSecond: 2})
	checkHidden()
}
