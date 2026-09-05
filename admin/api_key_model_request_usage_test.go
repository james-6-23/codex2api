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

func callModelRequestKeyMutation(t *testing.T, h *Handler, method string, id int64, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = gin.Params{{Key: "id", Value: fmt.Sprint(id)}}
	c.Request = httptest.NewRequest(method, "/api/admin/keys", strings.NewReader(string(encoded)))
	c.Request.Header.Set("Content-Type", "application/json")
	if method == http.MethodPost {
		h.CreateAPIKey(c)
	} else {
		h.UpdateAPIKey(c)
	}
	return rec
}

func TestAPIKeyModelRequestRulesPersistAndKeepCountersOnEdit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	h := &Handler{db: db}
	rec := callModelRequestKeyMutation(t, h, http.MethodPost, 0, map[string]any{
		"name": "Model budget", "key": "sk-model-request-admin-1234567890",
		"limits": database.APIKeyLimits{ModelRequestLimits: []database.APIKeyModelRequestLimit{
			{Model: "gpt-6*", MaxRequests: 50},
			{Model: "gpt-6-astra", MaxRequests: 10},
		}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var created createAPIKeyResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	row, err := db.GetAPIKeyByID(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	rules := row.Limits.ModelRequestLimits
	if len(rules) != 2 || rules[0].ID == "" || rules[0].ID == rules[1].ID || rules[0].Window != "week" || rules[0].Timezone != "Asia/Shanghai" || rules[0].ResetWeekday != 1 || rules[0].ResetTime != "00:00" {
		t.Fatalf("saved rules lack normalized fields: %+v", rules)
	}
	if exhausted, err := db.ConsumeAPIKeyModelRequest(ctx, created.ID, "admin-budget-request", "gpt-6-astra", rules, time.Now()); err != nil || exhausted != nil {
		t.Fatalf("consume: exhaustion=%+v err=%v", exhausted, err)
	}
	// Renaming without supplying limits preserves existing rules.
	rec = callModelRequestKeyMutation(t, h, http.MethodPatch, created.ID, map[string]any{"name": "Renamed"})
	if rec.Code != http.StatusOK {
		t.Fatalf("rename: %d %s", rec.Code, rec.Body.String())
	}
	updated := []database.APIKeyModelRequestLimit{rules[1], rules[0]}
	updated[1].MaxRequests = 60
	rec = callModelRequestKeyMutation(t, h, http.MethodPatch, created.ID, map[string]any{
		"limits": database.APIKeyLimits{ModelRequestLimits: updated},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("edit: %d %s", rec.Code, rec.Body.String())
	}
	row, err = db.GetAPIKeyByID(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Name != "Renamed" || len(row.Limits.ModelRequestLimits) != 2 || row.Limits.ModelRequestLimits[0].ID != rules[1].ID || row.Limits.ModelRequestLimits[1].ID != rules[0].ID || row.Limits.ModelRequestLimits[1].MaxRequests != 60 {
		t.Fatalf("edit lost rules or IDs: %+v", row)
	}
	usage, err := db.GetAPIKeyModelRequestUsage(ctx, created.ID, row.Limits.ModelRequestLimits, time.Now())
	if err != nil || len(usage) != 2 || usage[0].Used != 1 || usage[1].Used != 1 || usage[1].Remaining != 59 {
		t.Fatalf("counter changed after reordering/increasing limit: %+v err=%v", usage, err)
	}
	// Return generated IDs immediately, so reopening an editor before its list
	// refresh cannot accidentally submit a newly added rule as another new rule.
	updated = append(updated, database.APIKeyModelRequestLimit{Model: "gpt-5*", MaxRequests: 20})
	rec = callModelRequestKeyMutation(t, h, http.MethodPatch, created.ID, map[string]any{
		"limits": database.APIKeyLimits{ModelRequestLimits: updated},
	})
	var patchResponse struct {
		Limits database.APIKeyLimits `json:"limits"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &patchResponse) != nil || len(patchResponse.Limits.ModelRequestLimits) != 3 || patchResponse.Limits.ModelRequestLimits[2].ID == "" {
		t.Fatalf("patch must return generated rule ID: %d %s", rec.Code, rec.Body.String())
	}
	newID := patchResponse.Limits.ModelRequestLimits[2].ID
	rec = callModelRequestKeyMutation(t, h, http.MethodPatch, created.ID, map[string]any{"limits": patchResponse.Limits})
	if rec.Code != http.StatusOK {
		t.Fatalf("immediate resave: %d %s", rec.Code, rec.Body.String())
	}
	row, err = db.GetAPIKeyByID(ctx, created.ID)
	if err != nil || row.Limits.ModelRequestLimits[2].ID != newID {
		t.Fatalf("immediate resave changed generated ID: row=%+v err=%v", row, err)
	}
}

func TestAPIKeyModelRequestRulesRejectInvalidAndChangedRules(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	h := &Handler{db: db}
	for name, raw := range map[string]map[string]any{
		"empty model":    {"model": "", "max_requests": 50},
		"zero limit":     {"model": "gpt-6*", "max_requests": 0},
		"negative limit": {"model": "gpt-6*", "max_requests": -1},
		"window":         {"model": "gpt-6*", "max_requests": 50, "window": "month"},
		"timezone":       {"model": "gpt-6*", "max_requests": 50, "timezone": "Not/AZone"},
		"weekday":        {"model": "gpt-6*", "max_requests": 50, "reset_weekday": 8},
		"time":           {"model": "gpt-6*", "max_requests": 50, "reset_time": "25:00"},
		"supplied ID":    {"id": "mr_other", "model": "gpt-6*", "max_requests": 50},
	} {
		t.Run(name, func(t *testing.T) {
			rec := callModelRequestKeyMutation(t, h, http.MethodPost, 0, map[string]any{
				"name": "Invalid budget", "limits": map[string]any{"model_request_limits": []any{raw}},
			})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("invalid rule accepted: %d %s", rec.Code, rec.Body.String())
			}
		})
	}
	id, err := db.InsertAPIKeyWithOptions(context.Background(), database.APIKeyInput{
		Name: "Immutable", Key: "sk-model-budget-immutable-1234567890",
		Limits: database.APIKeyLimits{ModelRequestLimits: []database.APIKeyModelRequestLimit{{Model: "gpt-6*", MaxRequests: 50}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	row, err := db.GetAPIKeyByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	original := row.Limits.ModelRequestLimits[0]
	for name, mutate := range map[string]func(*database.APIKeyModelRequestLimit){
		"model":      func(r *database.APIKeyModelRequestLimit) { r.Model = "gpt-5*" },
		"timezone":   func(r *database.APIKeyModelRequestLimit) { r.Timezone = "UTC" },
		"weekday":    func(r *database.APIKeyModelRequestLimit) { r.ResetWeekday = 2 },
		"time":       func(r *database.APIKeyModelRequestLimit) { r.ResetTime = "12:00" },
		"foreign ID": func(r *database.APIKeyModelRequestLimit) { r.ID = "mr_foreign" },
	} {
		t.Run(name, func(t *testing.T) {
			rule := original
			mutate(&rule)
			rec := callModelRequestKeyMutation(t, h, http.MethodPatch, id, map[string]any{
				"limits": database.APIKeyLimits{ModelRequestLimits: []database.APIKeyModelRequestLimit{rule}},
			})
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("immutable field accepted: %d %s", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestAPIKeyModelRequestUsageRoutesEnforceAuthAndKeyIsolation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	ctx := context.Background()
	keys := []string{"sk-budget-owner-a-1234567890", "sk-budget-owner-b-1234567890", "sk-budget-disabled-1234567890"}
	ids := make([]int64, len(keys))
	for i, key := range keys {
		limits := database.APIKeyLimits{}
		if i < 2 {
			limits.ModelRequestLimits = []database.APIKeyModelRequestLimit{{Model: "gpt-6*", MaxRequests: 50}}
		}
		id, err := db.InsertAPIKeyWithOptions(ctx, database.APIKeyInput{Name: fmt.Sprintf("Owner %d", i), Key: key, Limits: limits})
		if err != nil {
			t.Fatal(err)
		}
		ids[i] = id
		row, err := db.GetAPIKeyByID(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		for n := 0; n < i+1 && i < 2; n++ {
			if exhausted, err := db.ConsumeAPIKeyModelRequest(ctx, id, fmt.Sprintf("request-%d", n), "gpt-6-astra", row.Limits.ModelRequestLimits, time.Now()); err != nil || exhausted != nil {
				t.Fatalf("consume: %+v %v", exhausted, err)
			}
		}
	}
	h := &Handler{db: db, adminSecretEnv: "test-model-budget-admin"}
	router := gin.New()
	h.RegisterRoutes(router)
	get := func(path, bearer, admin string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		if admin != "" {
			req.Header.Set("X-Admin-Key", admin)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	adminPath := fmt.Sprintf("/api/admin/keys/%d/model-request-usage", ids[0])
	for _, bearer := range []string{"", keys[0]} {
		if rec := get(adminPath, bearer, ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("admin accepted non-admin key: %d %s", rec.Code, rec.Body.String())
		}
	}
	for _, suffix := range []string{"summary", "me"} {
		if rec := get("/api/key-usage/"+suffix, "", ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("public accepted no key: %d %s", rec.Code, rec.Body.String())
		}
		if rec := get("/api/key-usage/"+suffix, "sk-not-a-valid-key", ""); rec.Code != http.StatusUnauthorized {
			t.Fatalf("public accepted unknown key: %d %s", rec.Code, rec.Body.String())
		}
		for i, key := range keys {
			rec := get(fmt.Sprintf("/api/key-usage/%s?range=all&key_id=%d", suffix, ids[(i+1)%len(ids)]), key, "")
			if rec.Code != http.StatusOK {
				t.Fatalf("public read: %d %s", rec.Code, rec.Body.String())
			}
			var payload publicAPIKeyUsageResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Key.Name != fmt.Sprintf("Owner %d", i) || strings.Contains(rec.Body.String(), key) {
				t.Fatalf("public identity mismatch or secret leaked: %s", rec.Body.String())
			}
			if i == 2 {
				if payload.ModelRequestUsage == nil || len(payload.ModelRequestUsage) != 0 {
					t.Fatalf("unconfigured budget must return []: %s", rec.Body.String())
				}
			} else if len(payload.ModelRequestUsage) != 1 || payload.ModelRequestUsage[0].Used != int64(i+1) {
				t.Fatalf("public budget isolation failed: %+v", payload.ModelRequestUsage)
			}
		}
	}
	rec := get(adminPath, "", "test-model-budget-admin")
	var usage struct {
		Items []database.APIKeyModelRequestUsage `json:"model_request_usage"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &usage) != nil || len(usage.Items) != 1 || usage.Items[0].Used != 1 {
		t.Fatalf("admin usage: %d %s", rec.Code, rec.Body.String())
	}
	for _, badID := range []string{"0", "-1", "invalid"} {
		if rec := get("/api/admin/keys/"+badID+"/model-request-usage", "", "test-model-budget-admin"); rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid ID: %d %s", rec.Code, rec.Body.String())
		}
	}
	if rec := get("/api/admin/keys/999999/model-request-usage", "", "test-model-budget-admin"); rec.Code != http.StatusNotFound {
		t.Fatalf("missing key: %d %s", rec.Code, rec.Body.String())
	}
	settings, err := db.GetSystemSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if settings == nil {
		settings = &database.SystemSettings{}
	}
	settings.PublicKeyUsagePageEnabled = false
	if err := db.UpdateSystemSettings(ctx, settings); err != nil {
		t.Fatal(err)
	}
	if rec := get("/api/key-usage/summary", keys[0], ""); rec.Code != http.StatusNotFound {
		t.Fatalf("disabled public usage: %d %s", rec.Code, rec.Body.String())
	}
}
