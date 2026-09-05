package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

const modelQuotaTestKey = "sk-weekly-model-quota-test"
const modelQuotaSSE = "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_quota\"}}\n\n" +
	"data: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"message\"}}\n\n" +
	"data: {\"type\":\"response.output_text.delta\",\"delta\":\"OK\"}\n\n" +
	"data: {\"type\":\"response.output_text.done\"}\n\n" +
	"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_quota\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n"

func newModelQuotaTestHandler(t *testing.T, limit int64, upstream string, native bool) (*Handler, *database.APIKeyRow, *gin.Engine) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	previous := CurrentRuntimeSettings()
	settings := DefaultRuntimeSettings()
	settings.CodexForceWebsocket = false
	ApplyRuntimeSettings(settings)
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "quota.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	id, err := db.InsertAPIKeyWithOptions(context.Background(), database.APIKeyInput{
		Key: modelQuotaTestKey, Name: "weekly model quota",
		Limits: database.APIKeyLimits{ModelRequestLimits: []database.APIKeyModelRequestLimit{{Model: "gpt-6*", MaxRequests: limit}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	row, err := db.GetAPIKeyByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 4, MaxRetries: 0, MaxRateLimitRetries: 0})
	t.Cleanup(store.Stop)
	if native {
		store.AddAccount(&auth.Account{DBID: 1, AccessToken: "test-token", PlanType: "pro", Models: []string{"gpt-6-astra", "gpt-5.4"}})
	} else {
		// The early global mapping is deliberately outside the quota; only the
		// later account mapping exposes the actual model that must be charged.
		store.SetCodexModelMapping(`{"team-model":"gpt-5.4"}`)
		store.AddAccount(&auth.Account{DBID: 1, UpstreamType: auth.UpstreamOpenAIResponses,
			BaseURL: upstream, APIKey: "relay-test", PlanType: "api",
			Models: []string{"gpt-6-astra", "gpt-5.4"}, ModelMapping: `{"team-model":"gpt-6-astra"}`})
	}
	h := NewHandler(store, db, &config.Config{}, nil)
	r := gin.New()
	h.RegisterRoutes(r)
	return h, row, r
}

func performModelQuotaRequest(r http.Handler, endpoint, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, endpoint, strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+modelQuotaTestKey)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestModelRequestQuotaHTTPMappedModelAndProtocolErrors(t *testing.T) {
	var sent atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent.Add(1)
		body := readUpstreamRequestBody(r)
		model := gjson.GetBytes(body, "model").String()
		if model != "gpt-6-astra" && model != "gpt-5.4" {
			t.Errorf("unexpected wire model %q", model)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, modelQuotaSSE)
	}))
	defer upstream.Close()
	h, row, router := newModelQuotaTestHandler(t, 1, upstream.URL, false)
	invalid := performModelQuotaRequest(router, "/v1/responses", `{"model":7,"input":"hi"}`)
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("invalid status=%d body=%s", invalid.Code, invalid.Body.String())
	}
	usage, err := h.db.GetAPIKeyModelRequestUsage(context.Background(), row.ID, row.Limits.ModelRequestLimits, time.Now())
	if err != nil || usage[0].Used != 0 {
		t.Fatalf("local rejection charged quota: %#v %v", usage, err)
	}
	first := performModelQuotaRequest(router, "/v1/responses", `{"model":"team-model","input":"hi","stream":true}`)
	if first.Code != 200 || !strings.Contains(first.Body.String(), "response.completed") {
		t.Fatalf("first=%d %s", first.Code, first.Body.String())
	}
	for _, tc := range []struct{ path, body string }{
		{"/v1/responses", `{"model":"team-model","input":"hi","stream":true}`},
		{"/v1/chat/completions", `{"model":"team-model","messages":[{"role":"user","content":"hi"}]}`},
		{"/v1/messages", `{"model":"gpt-6-astra","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`},
		{"/v1/responses/compact", `{"model":"team-model","input":[{"role":"user","content":"hi"}]}`},
	} {
		w := performModelQuotaRequest(router, tc.path, tc.body)
		if w.Code != 429 || gjson.GetBytes(w.Body.Bytes(), "error.code").String() != "rate_limit_reached" || gjson.GetBytes(w.Body.Bytes(), "error.details.used").Int() != 1 || w.Header().Get("Retry-After") == "" {
			t.Errorf("%s quota response=%d %s headers=%v", tc.path, w.Code, w.Body.String(), w.Header())
		}
	}
	if sent.Load() != 1 {
		t.Fatalf("exhausted requests reached upstream: calls=%d", sent.Load())
	}
	other := performModelQuotaRequest(router, "/v1/responses", `{"model":"gpt-5.4","input":"hi","stream":true}`)
	if other.Code != 200 {
		t.Fatalf("other model blocked: %d %s", other.Code, other.Body.String())
	}
	if sent.Load() != 2 {
		t.Fatalf("upstream calls=%d", sent.Load())
	}
	for _, account := range h.store.Accounts() {
		if atomic.LoadInt64(&account.ActiveRequests) != 0 {
			t.Fatal("quota rejection leaked account lease")
		}
	}
}

func TestModelRequestQuotaExecutorRetriesAndFreshRequest(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(503)
		_, _ = io.WriteString(w, `{"error":{"message":"test transport attempt"}}`)
	}))
	defer upstream.Close()
	h, row, _ := newModelQuotaTestHandler(t, 1, upstream.URL, false)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Set(contextAPIKeyRow, row)
	h.attachAPIKeyModelRequestQuota(c, false)
	ctx := c.Request.Context()
	account := h.store.Accounts()[0]
	for attempt := 0; attempt < 3; attempt++ {
		resp, err := ExecuteOpenAIResponsesRequest(ctx, account, []byte(`{"model":"gpt-6-astra","input":"hi"}`), "", nil)
		if err != nil {
			t.Fatalf("internal retry %d: %v", attempt, err)
		}
		_ = resp.Body.Close()
		// Handler re-entry must retain the original request identity.
		h.attachAPIKeyModelRequestQuota(c, false)
	}
	usage, err := h.db.GetAPIKeyModelRequestUsage(context.Background(), row.ID, row.Limits.ModelRequestLimits, time.Now())
	if err != nil || usage[0].Used != 1 || calls.Load() != 3 {
		t.Fatalf("usage=%#v calls=%d err=%v", usage, calls.Load(), err)
	}
	h.attachAPIKeyModelRequestQuota(c, true)
	_, err = ExecuteOpenAIResponsesRequest(c.Request.Context(), account, []byte(`{"model":"gpt-6-astra"}`), "", nil)
	if apiKeyModelRequestError(err) == nil || calls.Load() != 3 {
		t.Fatalf("new request bypassed quota: %v calls=%d", err, calls.Load())
	}
	if isRetryableRequestErrorForContext(context.Background(), err, database.ContinuousRetryPolicy{Enabled: true, CatchAll: true}) {
		t.Fatal("local quota must never enter upstream retry policy")
	}
}

func TestModelRequestQuotaWebsocketFramesAndOtherModel(t *testing.T) {
	previousExecute := WebsocketExecuteFunc
	t.Cleanup(func() { WebsocketExecuteFunc = previousExecute })
	var sent atomic.Int32
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, body []byte, sessionID, proxyURL, key string, cfg *DeviceProfileConfig, headers http.Header, poolKey string) (*http.Response, error) {
		if err := ConsumeAPIKeyModelRequestQuota(ctx, gjson.GetBytes(body, "model").String()); err != nil {
			return nil, err
		}
		sent.Add(1)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(modelQuotaSSE))}, nil
	}
	h, _, router := newModelQuotaTestHandler(t, 1, "", true)
	server := httptest.NewServer(router)
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", http.Header{"Authorization": {"Bearer " + modelQuotaTestKey}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	for i, model := range []string{"gpt-6-astra", "gpt-6-astra", "gpt-5.4"} {
		if err := conn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"type":"response.create","model":%q,"input":"hi"}`, model))); err != nil {
			t.Fatal(err)
		}
		_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
		for {
			_, payload, err := conn.ReadMessage()
			if err != nil {
				t.Fatalf("frame %d: %v", i, err)
			}
			typ := gjson.GetBytes(payload, "type").String()
			if typ == "error" {
				if i != 1 || gjson.GetBytes(payload, "error.details.remaining").Int() != 0 || gjson.GetBytes(payload, "error.code").String() != "rate_limit_reached" {
					t.Fatalf("frame %d: %s", i, payload)
				}
				break
			}
			if typ == "response.completed" {
				if i == 1 {
					t.Fatal("second frame was not charged independently")
				}
				break
			}
		}
	}
	if sent.Load() != 2 {
		t.Fatalf("sent=%d", sent.Load())
	}
	for _, account := range h.store.Accounts() {
		deadline := time.Now().Add(time.Second)
		for atomic.LoadInt64(&account.ActiveRequests) != 0 && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if atomic.LoadInt64(&account.ActiveRequests) != 0 {
			t.Fatal("WS quota rejection leaked account lease")
		}
	}
}

func TestModelRequestQuotaPreviouslyUnrestrictedSnapshotCannotBypassNewRule(t *testing.T) {
	h, row, _ := newModelQuotaTestHandler(t, 1, "", true)
	stale := *row
	stale.Limits = database.APIKeyLimits{}
	newRequest := func() context.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		c.Set(contextAPIKeyRow, &stale)
		h.attachAPIKeyModelRequestQuota(c, false)
		return c.Request.Context()
	}
	if err := ConsumeAPIKeyModelRequestQuota(newRequest(), "gpt-6-astra"); err != nil {
		t.Fatal(err)
	}
	if err := ConsumeAPIKeyModelRequestQuota(newRequest(), "gpt-6-astra"); apiKeyModelRequestError(err) == nil {
		t.Fatalf("stale unrestricted auth cache bypassed saved budget: %v", err)
	}
}

func TestModelRequestQuotaAntigravityUsesFinalWireModel(t *testing.T) {
	var sent atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent.Add(1)
		body := readUpstreamRequestBody(r)
		if model := gjson.GetBytes(body, "model").String(); model != "gemini-pro-agent" {
			t.Errorf("wire model=%s", model)
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()
	previous := antigravityInteractionsEndpoint
	antigravityInteractionsEndpoint = upstream.URL
	t.Cleanup(func() { antigravityInteractionsEndpoint = previous })
	h, row, _ := newModelQuotaTestHandler(t, 1, "", true)
	limits := database.APIKeyLimits{ModelRequestLimits: []database.APIKeyModelRequestLimit{{Model: "gemini-pro-agent", MaxRequests: 1}}}
	if err := h.db.UpdateAPIKeyLimits(context.Background(), row.ID, limits); err != nil {
		t.Fatal(err)
	}
	row, err := h.db.GetAPIKeyByID(context.Background(), row.ID)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Set(contextAPIKeyRow, row)
	account := &auth.Account{DBID: 55, UpstreamType: auth.UpstreamAntigravity, APIKey: "test-key"}
	for request := 0; request < 2; request++ {
		h.attachAPIKeyModelRequestQuota(c, true)
		resp, err := executeAntigravityInteractionsRequest(c.Request.Context(), account, "gemini-3.1-pro-high", []byte(`{"input":"hi"}`), false, "")
		if request == 0 {
			if err != nil {
				t.Fatal(err)
			}
			_ = resp.Body.Close()
		} else if apiKeyModelRequestError(err) == nil {
			t.Fatalf("public alias bypassed wire budget: %v", err)
		}
	}
	if sent.Load() != 1 {
		t.Fatalf("sent %d upstream requests", sent.Load())
	}
}

func TestModelRequestQuotaErrorClaimsRetryTerminal(t *testing.T) {
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(nil)
	deadline := &continuousRetryDeadline{duration: time.Hour, cancel: cancel}
	ctx = context.WithValue(ctx, continuousRetryDeadlineContextKey{}, deadline)
	deadline.Activate()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
	err := &apiKeyModelRequestQuotaError{http.StatusTooManyRequests, api.NewAPIError(api.ErrCodeRateLimitReached, "weekly limit", api.ErrorTypeRateLimit)}
	if !sendAPIKeyModelRequestQuotaError(c, err) {
		t.Fatal("quota error was not handled")
	}
	deadline.mu.Lock()
	settled := deadline.succeeded && deadline.stopped
	deadline.mu.Unlock()
	if !settled || w.Code != 429 {
		t.Fatalf("quota terminal did not settle deadline: settled=%v status=%d", settled, w.Code)
	}
}
