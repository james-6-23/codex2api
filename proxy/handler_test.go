package proxy

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

type errReadCloser struct {
	err error
}

func (r errReadCloser) Read([]byte) (int, error) {
	return 0, r.err
}

func (r errReadCloser) Close() error {
	return nil
}

func TestSupportedModelsIncludeLatestRequestedModels(t *testing.T) {
	for _, model := range []string{"gpt-5.5", "gpt-5.3-codex-spark", "gpt-5.2", "gpt-image-2", "gpt-image-2-2k", "gpt-image-2-4k"} {
		if !slices.Contains(SupportedModels, model) {
			t.Fatalf("SupportedModels missing %q", model)
		}
	}
}

func TestSupportedModelsExcludeBelowGPT52(t *testing.T) {
	for _, model := range []string{
		"gpt-5", "gpt-5-codex", "gpt-5-codex-mini",
		"gpt-5.1", "gpt-5.1-codex", "gpt-5.1-codex-mini", "gpt-5.1-codex-max",
		"gpt-5.2-codex",
	} {
		if slices.Contains(SupportedModels, model) {
			t.Fatalf("SupportedModels should not include %q", model)
		}
	}
}

func TestListModelsIncludesLatestRequestedModels(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	handler := &Handler{}

	handler.ListModels(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}

	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	ids := make([]string, 0, len(payload.Data))
	for _, model := range payload.Data {
		ids = append(ids, model.ID)
	}
	for _, model := range []string{"gpt-5.5", "gpt-5.3-codex-spark", "gpt-5.2", "gpt-image-2"} {
		if !slices.Contains(ids, model) {
			t.Fatalf("/v1/models missing %q in %v", model, ids)
		}
	}
	for _, model := range []string{"gpt-image-2-2k", "gpt-image-2-4k"} {
		if !slices.Contains(ids, model) {
			t.Fatalf("/v1/models missing image alias %q in %v", model, ids)
		}
	}

	for _, model := range []string{"gpt-5", "gpt-5.1", "gpt-5.2-codex"} {
		if slices.Contains(ids, model) {
			t.Fatalf("/v1/models should not include %q in %v", model, ids)
		}
	}
}

func TestSupportedModelIDsIncludesOpenAIResponsesModelMappingAliases(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	store.AddAccount(&auth.Account{
		DBID:         1,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      "https://api.openai.com",
		APIKey:       "sk-test",
		Models:       []string{"gpt-4.1-direct"},
		ModelMapping: `{"client-alias":"gpt-4.1-direct","client-*":"gpt-4.1-direct"}`,
	})
	handler := &Handler{store: store}

	models := handler.supportedModelIDs(context.Background())
	if !slices.Contains(models, "client-alias") {
		t.Fatalf("supported models should include exact account mapping alias; models=%v", models)
	}
	if slices.Contains(models, "client-*") {
		t.Fatalf("supported models should not expose wildcard mapping patterns; models=%v", models)
	}
}

func TestImageModelIsImageEndpointOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)

	sendImageOnlyModelError(ctx, "gpt-image-2")

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
	if !strings.Contains(recorder.Body.String(), "/v1/images/generations") {
		t.Fatalf("error body should point to images endpoints: %s", recorder.Body.String())
	}
}

func TestRegisterRoutesIncludesCodexDirectResponses(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	handler := &Handler{}

	handler.RegisterRoutes(router)

	postRoutes := make(map[string]bool)
	getRoutes := make(map[string]bool)
	for _, route := range router.Routes() {
		if route.Method == http.MethodPost {
			postRoutes[route.Path] = true
		}
		if route.Method == http.MethodGet {
			getRoutes[route.Path] = true
		}
	}

	for _, path := range []string{
		"/backend-api/codex/responses",
		"/backend-api/codex/responses/*subpath",
	} {
		if !postRoutes[path] {
			t.Fatalf("expected POST route %s to be registered; routes=%v", path, postRoutes)
		}
	}
	for _, path := range []string{
		"/v1/responses",
		"/responses",
		"/backend-api/codex/responses",
	} {
		if !getRoutes[path] {
			t.Fatalf("expected GET route %s to be registered; routes=%v", path, getRoutes)
		}
	}
}

func TestNormalizeResponsesWebSocketClientPayload(t *testing.T) {
	t.Run("defaults response create type", func(t *testing.T) {
		got, model, apiErr := normalizeResponsesWebSocketClientPayload([]byte(`{"model":"gpt-5.4","input":"hi"}`))
		if apiErr != nil {
			t.Fatalf("unexpected error: %v", apiErr)
		}
		if model != "gpt-5.4" {
			t.Fatalf("model = %q, want gpt-5.4", model)
		}
		if eventType := gjson.GetBytes(got, "type").String(); eventType != "response.create" {
			t.Fatalf("type = %q, want response.create; body=%s", eventType, got)
		}
	})

	t.Run("rejects append", func(t *testing.T) {
		_, _, apiErr := normalizeResponsesWebSocketClientPayload([]byte(`{"type":"response.append","model":"gpt-5.4"}`))
		if apiErr == nil || !strings.Contains(apiErr.Message, "response.append") {
			t.Fatalf("error = %#v, want response.append rejection", apiErr)
		}
	})

	t.Run("rejects message previous response id", func(t *testing.T) {
		_, _, apiErr := normalizeResponsesWebSocketClientPayload([]byte(`{"type":"response.create","model":"gpt-5.4","previous_response_id":"msg_123"}`))
		if apiErr == nil || !strings.Contains(apiErr.Message, "response.id") {
			t.Fatalf("error = %#v, want previous_response_id rejection", apiErr)
		}
	})
}

func TestResponsesWebSocketForwardsResponsesEvents(t *testing.T) {
	gin.SetMode(gin.TestMode)

	previousExec := WebsocketExecuteFunc
	t.Cleanup(func() {
		WebsocketExecuteFunc = previousExec
	})

	bodyCh := make(chan []byte, 2)
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		bodyCh <- append([]byte(nil), requestBody...)
		sse := "" +
			`data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n" +
			`data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2},"service_tier":"default"}}` + "\n\n"
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(sse)),
		}, nil
	}

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "at", PlanType: "plus", AccountID: "acct-1"})
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)

	router := gin.New()
	handler.RegisterRoutes(router)
	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		if resp != nil {
			t.Fatalf("dial websocket failed: %v status=%d", err, resp.StatusCode)
		}
		t.Fatalf("dial websocket failed: %v", err)
	}
	defer conn.Close()

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"model":"gpt-5.4","previous_response_id":"resp_prev","input":"hello"}`)); err != nil {
		t.Fatalf("write request: %v", err)
	}
	select {
	case gotBody := <-bodyCh:
		if gjson.GetBytes(gotBody, "type").String() != "response.create" {
			t.Fatalf("upstream type missing: %s", gotBody)
		}
		if prev := gjson.GetBytes(gotBody, "previous_response_id").String(); prev != "resp_prev" {
			t.Fatalf("previous_response_id = %q, want resp_prev; body=%s", prev, gotBody)
		}
		if store := gjson.GetBytes(gotBody, "store"); store.Exists() {
			t.Fatalf("websocket ingress should not force store=false, got %s; body=%s", store.Raw, gotBody)
		}
		if !gjson.GetBytes(gotBody, "stream").Bool() {
			t.Fatalf("upstream stream should be true: %s", gotBody)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for upstream request")
	}

	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	_, first, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read first event: %v", err)
	}
	if eventType := gjson.GetBytes(first, "type").String(); eventType != "response.output_text.delta" {
		t.Fatalf("first event type = %q body=%s", eventType, first)
	}
	_, second, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read terminal event: %v", err)
	}
	if eventType := gjson.GetBytes(second, "type").String(); eventType != "response.completed" {
		t.Fatalf("terminal event type = %q body=%s", eventType, second)
	}

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.4","input":"again"}`)); err != nil {
		t.Fatalf("write second request: %v", err)
	}
	select {
	case <-bodyCh:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for second upstream request")
	}
}

func TestResponsesWebSocketFlushesSkeletonBeforeContent(t *testing.T) {
	gin.SetMode(gin.TestMode)

	previousExec := WebsocketExecuteFunc
	previousSettings := CurrentRuntimeSettings()
	t.Cleanup(func() {
		WebsocketExecuteFunc = previousExec
		ApplyRuntimeSettings(previousSettings)
	})
	// 配置首字超时使 ttftGuard != nil，启用首包前缓冲路径；取足够大的值避免误触发。
	nextSettings := previousSettings
	nextSettings.FirstTokenTimeoutSec = 60
	ApplyRuntimeSettings(nextSettings)

	release := make(chan struct{})
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		pr, pw := io.Pipe()
		go func() {
			// 骨架帧先到：created（生命周期，缓冲）+ output_item.added（结构帧，触发 flush）。
			_, _ = pw.Write([]byte(`data: {"type":"response.created","response":{}}` + "\n\n"))
			_, _ = pw.Write([]byte(`data: {"type":"response.output_item.added","item":{"type":"reasoning"}}` + "\n\n"))
			// 在内容到来前阻塞：issue #207 修复前，客户端会一直卡到首个内容才收到任何帧。
			<-release
			_, _ = pw.Write([]byte(`data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n"))
			_, _ = pw.Write([]byte(`data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"))
			_ = pw.Close()
		}()
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: pr}, nil
	}

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, TestConcurrency: 1, TestModel: "gpt-5.4"})
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "at-1", PlanType: "pro", AccountID: "acct-1"})
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)

	router := gin.New()
	handler.RegisterRoutes(router)
	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket failed: %v", err)
	}
	defer conn.Close()

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"model":"gpt-5.4","input":"hi"}`)); err != nil {
		t.Fatalf("write request: %v", err)
	}

	// 内容尚未发送（mock 仍阻塞在 <-release），客户端此时就应已收到骨架帧。
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, first, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("expected skeleton frame before content, got error: %v", err)
	}
	if et := gjson.GetBytes(first, "type").String(); et != "response.created" {
		t.Fatalf("first relayed event = %q, want response.created", et)
	}
	_, second, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read second skeleton frame: %v", err)
	}
	if et := gjson.GetBytes(second, "type").String(); et != "response.output_item.added" {
		t.Fatalf("second relayed event = %q, want response.output_item.added", et)
	}

	// 放行内容，确认其余事件照常透传。
	close(release)
	_, third, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read content delta: %v", err)
	}
	if et := gjson.GetBytes(third, "type").String(); et != "response.output_text.delta" {
		t.Fatalf("third relayed event = %q, want response.output_text.delta", et)
	}
}

func TestResponsesWebSocketRetriesFirstTokenTimeoutBeforeRelay(t *testing.T) {
	gin.SetMode(gin.TestMode)

	previousExec := WebsocketExecuteFunc
	previousSettings := CurrentRuntimeSettings()
	t.Cleanup(func() {
		WebsocketExecuteFunc = previousExec
		ApplyRuntimeSettings(previousSettings)
	})
	nextSettings := previousSettings
	nextSettings.FirstTokenTimeoutSec = 1
	ApplyRuntimeSettings(nextSettings)

	attemptCh := make(chan int64, 4)
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		attemptCh <- account.ID()
		if account.ID() == 1 {
			pr, pw := io.Pipe()
			go func() {
				_, _ = pw.Write([]byte(`data: {"type":"response.created","response":{}}` + "\n\n"))
				<-ctx.Done()
				_ = pw.CloseWithError(ctx.Err())
			}()
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       pr,
			}, nil
		}

		sse := "" +
			`data: {"type":"response.output_text.delta","delta":"retried"}` + "\n\n" +
			`data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2},"service_tier":"default"}}` + "\n\n"
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(sse)),
		}, nil
	}

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, MaxRetries: 1, TestConcurrency: 1, TestModel: "gpt-5.4"})
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "at-1", PlanType: "pro", AccountID: "acct-1"})
	store.AddAccount(&auth.Account{DBID: 2, AccessToken: "at-2", PlanType: "free", AccountID: "acct-2"})
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)

	router := gin.New()
	handler.RegisterRoutes(router)
	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		if resp != nil {
			t.Fatalf("dial websocket failed: %v status=%d", err, resp.StatusCode)
		}
		t.Fatalf("dial websocket failed: %v", err)
	}
	defer conn.Close()

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"model":"gpt-5.4","input":"hello"}`)); err != nil {
		t.Fatalf("write request: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, first, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read first relayed event: %v", err)
	}
	if eventType := gjson.GetBytes(first, "type").String(); eventType != "response.output_text.delta" {
		t.Fatalf("first relayed event type = %q body=%s", eventType, first)
	}
	if delta := gjson.GetBytes(first, "delta").String(); delta != "retried" {
		t.Fatalf("first relayed delta = %q, want retried; body=%s", delta, first)
	}

	_, second, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read terminal event: %v", err)
	}
	if eventType := gjson.GetBytes(second, "type").String(); eventType != "response.completed" {
		t.Fatalf("terminal event type = %q body=%s", eventType, second)
	}

	for _, want := range []int64{1, 2} {
		select {
		case got := <-attemptCh:
			if got != want {
				t.Fatalf("attempt account = %d, want %d", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for attempt account %d", want)
		}
	}
}

func TestResponsesWebSocketFallsBackToHTTPWhenUpstreamMessageTooBig(t *testing.T) {
	gin.SetMode(gin.TestMode)

	previousExec := WebsocketExecuteFunc
	previousResin := resinCfg.Load()
	t.Cleanup(func() {
		WebsocketExecuteFunc = previousExec
		resinCfg.Store(previousResin)
	})

	wsCalls := 0
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		wsCalls++
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       errReadCloser{err: errors.New("websocket read error: websocket: close 1009 (message too big)")},
		}, nil
	}

	httpCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpCalls++
		if !strings.HasSuffix(r.URL.Path, "/backend-api/codex/responses") {
			t.Fatalf("upstream path = %q, want Resin path ending /backend-api/codex/responses", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			`data: {"type":"response.output_text.delta","delta":"http-fallback"}` + "\n\n" +
				`data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2},"service_tier":"default"}}` + "\n\n",
		))
	}))
	defer upstream.Close()
	SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "test"})

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, TestConcurrency: 1, TestModel: "gpt-5.4"})
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "at-1", PlanType: "pro", AccountID: "acct-1"})
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)

	router := gin.New()
	handler.RegisterRoutes(router)
	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		if resp != nil {
			t.Fatalf("dial websocket failed: %v status=%d", err, resp.StatusCode)
		}
		t.Fatalf("dial websocket failed: %v", err)
	}
	defer conn.Close()

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"model":"gpt-5.4","input":"hello"}`)); err != nil {
		t.Fatalf("write request: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, first, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read fallback event: %v", err)
	}
	if delta := gjson.GetBytes(first, "delta").String(); delta != "http-fallback" {
		t.Fatalf("fallback delta = %q, want http-fallback; body=%s", delta, first)
	}
	_, second, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read terminal event: %v", err)
	}
	if eventType := gjson.GetBytes(second, "type").String(); eventType != "response.completed" {
		t.Fatalf("terminal event type = %q body=%s", eventType, second)
	}
	if wsCalls != 1 {
		t.Fatalf("websocket upstream calls = %d, want 1", wsCalls)
	}
	if httpCalls != 1 {
		t.Fatalf("HTTP upstream calls = %d, want 1", httpCalls)
	}
}

func TestResponsesHTTPIngressFallsBackToHTTPWhenForcedWebsocketMessageTooBig(t *testing.T) {
	gin.SetMode(gin.TestMode)

	previousExec := WebsocketExecuteFunc
	previousSettings := CurrentRuntimeSettings()
	previousResin := resinCfg.Load()
	t.Cleanup(func() {
		WebsocketExecuteFunc = previousExec
		ApplyRuntimeSettings(previousSettings)
		resinCfg.Store(previousResin)
	})
	nextSettings := previousSettings
	nextSettings.CodexForceWebsocket = true
	ApplyRuntimeSettings(nextSettings)

	wsCalls := 0
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		wsCalls++
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       errReadCloser{err: errors.New("websocket read error: websocket: close 1009 (message too big)")},
		}, nil
	}

	httpCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpCalls++
		if !strings.HasSuffix(r.URL.Path, "/backend-api/codex/responses") {
			t.Fatalf("upstream path = %q, want Resin path ending /backend-api/codex/responses", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			`data: {"type":"response.output_text.delta","delta":"http-fallback"}` + "\n\n" +
				`data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2},"service_tier":"default"}}` + "\n\n",
		))
	}))
	defer upstream.Close()
	SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "test"})

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, TestConcurrency: 1, TestModel: "gpt-5.4"})
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "at-1", PlanType: "pro", AccountID: "acct-1"})
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)

	body := []byte(`{"model":"gpt-5.4","input":"hello","stream":true}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = req

	handler.Responses(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "http-fallback") {
		t.Fatalf("response should come from HTTP fallback, body=%s", recorder.Body.String())
	}
	if wsCalls != 1 {
		t.Fatalf("websocket upstream calls = %d, want 1", wsCalls)
	}
	if httpCalls != 1 {
		t.Fatalf("HTTP upstream calls = %d, want 1", httpCalls)
	}
}

func TestResponsesWebSocketSilentRetryDisabledRelaysRetryableFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)

	previousExec := WebsocketExecuteFunc
	previousSettings := CurrentRuntimeSettings()
	t.Cleanup(func() {
		WebsocketExecuteFunc = previousExec
		ApplyRuntimeSettings(previousSettings)
	})
	nextSettings := previousSettings
	nextSettings.CodexWSSilentRetry = false
	nextSettings.CodexWSHideErrors = false
	nextSettings.CodexWSSilentRetries = 2
	ApplyRuntimeSettings(nextSettings)

	attemptCh := make(chan int64, 4)
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		attemptCh <- account.ID()
		sse := `data: {"type":"response.failed","response":{"error":{"type":"usage_limit_reached","message":"raw quota exhausted"}}}` + "\n\n"
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(sse)),
		}, nil
	}

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "at-1", PlanType: "pro", AccountID: "acct-1"})
	store.AddAccount(&auth.Account{DBID: 2, AccessToken: "at-2", PlanType: "pro", AccountID: "acct-2"})
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)

	router := gin.New()
	handler.RegisterRoutes(router)
	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		if resp != nil {
			t.Fatalf("dial websocket failed: %v status=%d", err, resp.StatusCode)
		}
		t.Fatalf("dial websocket failed: %v", err)
	}
	defer conn.Close()

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"model":"gpt-5.4","input":"hello"}`)); err != nil {
		t.Fatalf("write request: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	_, first, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read relayed failure: %v", err)
	}
	if eventType := gjson.GetBytes(first, "type").String(); eventType != "response.failed" {
		t.Fatalf("first event type = %q body=%s", eventType, first)
	}
	if !strings.Contains(string(first), "raw quota exhausted") {
		t.Fatalf("failure should include raw upstream message when hiding disabled: %s", first)
	}

	select {
	case <-attemptCh:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for first attempt")
	}
	select {
	case got := <-attemptCh:
		t.Fatalf("unexpected retry on account %d when silent retry is disabled", got)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestResponsesWebSocketHidesUpstreamErrorAfterSilentRetriesExhausted(t *testing.T) {
	gin.SetMode(gin.TestMode)

	previousExec := WebsocketExecuteFunc
	previousSettings := CurrentRuntimeSettings()
	t.Cleanup(func() {
		WebsocketExecuteFunc = previousExec
		ApplyRuntimeSettings(previousSettings)
	})
	nextSettings := previousSettings
	nextSettings.CodexWSSilentRetry = true
	nextSettings.CodexWSHideErrors = true
	nextSettings.CodexWSSilentRetries = 1
	ApplyRuntimeSettings(nextSettings)

	attemptCh := make(chan int64, 4)
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		attemptCh <- account.ID()
		sse := `data: {"type":"response.failed","response":{"error":{"type":"usage_limit_reached","message":"raw quota secret"}}}` + "\n\n"
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(sse)),
		}, nil
	}

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "at-1", PlanType: "pro", AccountID: "acct-1"})
	store.AddAccount(&auth.Account{DBID: 2, AccessToken: "at-2", PlanType: "pro", AccountID: "acct-2"})
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)

	router := gin.New()
	handler.RegisterRoutes(router)
	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		if resp != nil {
			t.Fatalf("dial websocket failed: %v status=%d", err, resp.StatusCode)
		}
		t.Fatalf("dial websocket failed: %v", err)
	}
	defer conn.Close()

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"model":"gpt-5.4","input":"hello"}`)); err != nil {
		t.Fatalf("write request: %v", err)
	}

	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	_, first, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read friendly failure: %v", err)
	}
	if eventType := gjson.GetBytes(first, "type").String(); eventType != "error" {
		t.Fatalf("first event type = %q body=%s", eventType, first)
	}
	if message := gjson.GetBytes(first, "error.message").String(); message != responsesWSFriendlyUpstreamErr {
		t.Fatalf("friendly message = %q, want %q; body=%s", message, responsesWSFriendlyUpstreamErr, first)
	}
	if strings.Contains(string(first), "raw quota secret") {
		t.Fatalf("friendly failure leaked raw upstream message: %s", first)
	}

	seenAttempts := make(map[int64]bool)
	for i := 0; i < 2; i++ {
		select {
		case got := <-attemptCh:
			seenAttempts[got] = true
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for attempt %d", i+1)
		}
	}
	if len(seenAttempts) != 2 {
		t.Fatalf("expected two distinct retry accounts, got %v", seenAttempts)
	}
}

func assertNoAvailableAccountResponse(t *testing.T, body []byte) {
	t.Helper()

	var payload struct {
		Error struct {
			Message string `json:"message"`
			Type    string `json:"type"`
			Code    string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, string(body))
	}
	if payload.Error.Message == "" {
		t.Fatalf("message is empty; body=%s", string(body))
	}
	if payload.Error.Type != ErrorTypeServerError {
		t.Fatalf("type = %q, want %q", payload.Error.Type, ErrorTypeServerError)
	}
	if payload.Error.Code != ErrorCodeNoAvailableAccount {
		t.Fatalf("code = %q, want %q", payload.Error.Code, ErrorCodeNoAvailableAccount)
	}
}

func TestUsageLogErrorMessageExtractsStructuredError(t *testing.T) {
	body := []byte(`{"error":{"code":"rate_limit_exceeded","type":"server_error","message":"Too many requests"}}`)

	got := usageLogErrorMessage(http.StatusTooManyRequests, body)

	if got != "rate_limit_exceeded · server_error · Too many requests" {
		t.Fatalf("usageLogErrorMessage() = %q", got)
	}
}

func TestResponsesEndpointsAllowCompactionInputType(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler := NewHandler(auth.NewStore(nil, nil, nil), nil, nil, nil)
	body := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"type":"message","role":"user","content":"hello"},
			{"type":"compaction","summary":"previous context was compacted"}
		]
	}`)

	tests := []struct {
		name    string
		path    string
		handler gin.HandlerFunc
	}{
		{name: "responses", path: "/v1/responses", handler: handler.Responses},
		{name: "responses compact", path: "/v1/responses/compact", handler: handler.ResponsesCompact},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			req := httptest.NewRequest(http.MethodPost, test.path, bytes.NewReader(body)).WithContext(ctx)
			req.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			ginCtx, _ := gin.CreateTestContext(recorder)
			ginCtx.Request = req

			test.handler(ginCtx)

			if recorder.Code == http.StatusBadRequest && strings.Contains(recorder.Body.String(), "invalid_input_type") {
				t.Fatalf("compaction input type was rejected by local validation: %s", recorder.Body.String())
			}
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want %d after validation passes; body=%s", recorder.Code, http.StatusServiceUnavailable, recorder.Body.String())
			}
			assertNoAvailableAccountResponse(t, recorder.Body.Bytes())
		})
	}
}

func TestResponsesCompactUsesOpenAIResponsesAPIAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var seenPath string
	var seenAuth string
	var seenBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenAuth = r.Header.Get("Authorization")
		seenBody, _ = io.ReadAll(r.Body)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"resp_compact_test",
			"object":"response",
			"created_at":1710000000,
			"model":"gpt-4.1-direct",
			"output":[],
			"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5},
			"service_tier":"default"
		}`))
	}))
	defer upstream.Close()

	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:      2,
		MaxRetries:          0,
		MaxRateLimitRetries: 0,
	})
	store.AddAccount(&auth.Account{
		DBID:         1,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      upstream.URL,
		APIKey:       "sk-direct",
		Models:       []string{"gpt-4.1-direct"},
		PlanType:     "api",
	})
	handler := NewHandler(store, nil, nil, nil)

	body := []byte(`{
		"model":"gpt-4.1-direct",
		"input":"hello",
		"include":["reasoning.encrypted_content"],
		"store":true,
		"stream":true
	}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = req

	handler.ResponsesCompact(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if seenPath != "/v1/responses/compact" {
		t.Fatalf("upstream path = %q, want /v1/responses/compact", seenPath)
	}
	if seenAuth != "Bearer sk-direct" {
		t.Fatalf("Authorization = %q, want Bearer sk-direct", seenAuth)
	}
	for _, field := range []string{"include", "store", "stream"} {
		if gjson.GetBytes(seenBody, field).Exists() {
			t.Fatalf("upstream body should not include %s: %s", field, seenBody)
		}
	}
	if model := gjson.GetBytes(seenBody, "model").String(); model != "gpt-4.1-direct" {
		t.Fatalf("upstream model = %q, want gpt-4.1-direct; body=%s", model, seenBody)
	}
	if id := gjson.GetBytes(recorder.Body.Bytes(), "id").String(); id != "resp_compact_test" {
		t.Fatalf("response id = %q, want resp_compact_test; body=%s", id, recorder.Body.String())
	}
}

func TestResponsesCompactOpenAIReadErrorRetryReturnsBadGateway(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "128")
		_, _ = w.Write([]byte(`{"id":"truncated"}`))
	}))
	defer upstream.Close()

	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:      1,
		MaxRetries:          1,
		MaxRateLimitRetries: 0,
	})
	store.AddAccount(&auth.Account{
		DBID:         1,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      upstream.URL,
		APIKey:       "sk-direct",
		Models:       []string{"gpt-4.1-direct"},
		PlanType:     "api",
		Status:       auth.StatusReady,
	})
	handler := NewHandler(store, nil, nil, nil)

	body := []byte(`{"model":"gpt-4.1-direct","input":"hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = req

	handler.ResponsesCompact(ctx)

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusBadGateway, recorder.Body.String())
	}
	if got := gjson.GetBytes(recorder.Body.Bytes(), "error.code").String(); got != "upstream_502" {
		t.Fatalf("error.code = %q, want upstream_502; body=%s", got, recorder.Body.String())
	}
	if got := gjson.GetBytes(recorder.Body.Bytes(), "error.message").String(); !strings.Contains(got, "Failed to read upstream response") {
		t.Fatalf("error.message = %q, want read failure; body=%s", got, recorder.Body.String())
	}
}

func TestResponsesCompactCodexReadErrorRetryReturnsBadGatewayAndSyncsUsage(t *testing.T) {
	gin.SetMode(gin.TestMode)

	previousResin := resinCfg.Load()
	t.Cleanup(func() {
		resinCfg.Store(previousResin)
	})

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/backend-api/codex/responses/compact") {
			t.Fatalf("upstream path = %q, want Resin path ending /backend-api/codex/responses/compact", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", "128")
		w.Header().Set("x-codex-primary-used-percent", "100")
		w.Header().Set("x-codex-primary-window-minutes", "300")
		w.Header().Set("x-codex-primary-reset-after-seconds", "900")
		_, _ = w.Write([]byte(`{"id":"truncated"}`))
	}))
	defer upstream.Close()
	SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "test"})

	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:      1,
		MaxRetries:          1,
		MaxRateLimitRetries: 0,
	})
	account := &auth.Account{
		DBID:        1,
		AccessToken: "at-1",
		Models:      []string{"gpt-5.4"},
		PlanType:    "team",
		Status:      auth.StatusReady,
	}
	store.AddAccount(account)
	handler := NewHandler(store, nil, nil, nil)

	body := []byte(`{"model":"gpt-5.4","input":"hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = req

	handler.ResponsesCompact(ctx)

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusBadGateway, recorder.Body.String())
	}
	if got := gjson.GetBytes(recorder.Body.Bytes(), "error.code").String(); got != "upstream_502" {
		t.Fatalf("error.code = %q, want upstream_502; body=%s", got, recorder.Body.String())
	}
	if !account.IsPremium5hRateLimited() {
		t.Fatal("account should sync Codex usage headers and enter premium 5h rate_limited state")
	}
}

// newOpenAIResponsesSSEUpstream 模拟仅支持 OpenAI Responses API 的中转上游，
// 返回一段最小可用的 Responses SSE 流（issue #181 回归用）。
func newOpenAIResponsesSSEUpstream(seenPath *string, seenAuth *string, seenBody *[]byte) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seenPath = r.URL.Path
		*seenAuth = r.Header.Get("Authorization")
		*seenBody, _ = io.ReadAll(r.Body)

		w.Header().Set("Content-Type", "text/event-stream")
		events := []string{
			`{"type":"response.created","response":{"id":"resp_relay_test"}}`,
			`{"type":"response.output_item.added","item":{"type":"message"}}`,
			`{"type":"response.output_text.delta","delta":"OK"}`,
			`{"type":"response.output_text.done"}`,
			`{"type":"response.completed","response":{"id":"resp_relay_test","status":"completed","usage":{"input_tokens":10,"output_tokens":2}}}`,
		}
		for _, event := range events {
			_, _ = io.WriteString(w, "data: "+event+"\n\n")
		}
	}))
}

func newOpenAIResponsesRelayStore(upstreamURL string) *auth.Store {
	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:      2,
		MaxRetries:          0,
		MaxRateLimitRetries: 0,
	})
	store.AddAccount(&auth.Account{
		DBID:         1,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      upstreamURL,
		APIKey:       "sk-direct",
		Models:       []string{"gpt-4.1-direct"},
		PlanType:     "api",
	})
	return store
}

func newOpenAIResponsesRelayStoreWithModelMapping(upstreamURL string) *auth.Store {
	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:      2,
		MaxRetries:          0,
		MaxRateLimitRetries: 0,
	})
	store.SetCodexModelMapping(`{"client-alias":"gpt-5.4"}`)
	store.AddAccount(&auth.Account{
		DBID:         1,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      upstreamURL,
		APIKey:       "sk-direct",
		Models:       []string{"gpt-4.1-direct"},
		ModelMapping: `{"client-alias":"gpt-4.1-direct"}`,
		PlanType:     "api",
	})
	return store
}

func newOpenAIResponsesRelayStoreWithWildcardModelMapping(upstreamURL string) *auth.Store {
	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:      2,
		MaxRetries:          0,
		MaxRateLimitRetries: 0,
	})
	store.AddAccount(&auth.Account{
		DBID:         1,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      upstreamURL,
		APIKey:       "sk-direct",
		Models:       []string{"gpt-4.1-direct"},
		ModelMapping: `{"client-*":"gpt-4.1-direct"}`,
		PlanType:     "api",
	})
	return store
}

func TestMessagesUsesOpenAIResponsesAPIAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var seenPath, seenAuth string
	var seenBody []byte
	upstream := newOpenAIResponsesSSEUpstream(&seenPath, &seenAuth, &seenBody)
	defer upstream.Close()

	handler := NewHandler(newOpenAIResponsesRelayStore(upstream.URL), nil, nil, nil)

	body := []byte(`{
		"model":"gpt-4.1-direct",
		"max_tokens":128,
		"messages":[{"role":"user","content":"hi"}]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = req

	handler.Messages(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if seenPath != "/v1/responses" {
		t.Fatalf("upstream path = %q, want /v1/responses", seenPath)
	}
	if seenAuth != "Bearer sk-direct" {
		t.Fatalf("Authorization = %q, want Bearer sk-direct", seenAuth)
	}
	if model := gjson.GetBytes(seenBody, "model").String(); model != "gpt-4.1-direct" {
		t.Fatalf("upstream model = %q, want gpt-4.1-direct; body=%s", model, seenBody)
	}
	if !gjson.GetBytes(seenBody, "stream").Bool() {
		t.Fatalf("upstream body should request stream: %s", seenBody)
	}
	respBody := recorder.Body.Bytes()
	if text := gjson.GetBytes(respBody, "content.0.text").String(); text != "OK" {
		t.Fatalf("content text = %q, want OK; body=%s", text, respBody)
	}
	if got := gjson.GetBytes(respBody, "usage.input_tokens").Int(); got != 10 {
		t.Fatalf("usage.input_tokens = %d, want 10; body=%s", got, respBody)
	}
}

func TestChatCompletionsUsesOpenAIResponsesAPIAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var seenPath, seenAuth string
	var seenBody []byte
	upstream := newOpenAIResponsesSSEUpstream(&seenPath, &seenAuth, &seenBody)
	defer upstream.Close()

	handler := NewHandler(newOpenAIResponsesRelayStore(upstream.URL), nil, nil, nil)

	body := []byte(`{
		"model":"gpt-4.1-direct",
		"messages":[{"role":"user","content":"hi"}]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = req

	handler.ChatCompletions(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if seenPath != "/v1/responses" {
		t.Fatalf("upstream path = %q, want /v1/responses", seenPath)
	}
	if seenAuth != "Bearer sk-direct" {
		t.Fatalf("Authorization = %q, want Bearer sk-direct", seenAuth)
	}
	if model := gjson.GetBytes(seenBody, "model").String(); model != "gpt-4.1-direct" {
		t.Fatalf("upstream model = %q, want gpt-4.1-direct; body=%s", model, seenBody)
	}
	respBody := recorder.Body.Bytes()
	if content := gjson.GetBytes(respBody, "choices.0.message.content").String(); content != "OK" {
		t.Fatalf("message content = %q, want OK; body=%s", content, respBody)
	}
	if got := gjson.GetBytes(respBody, "usage.prompt_tokens").Int(); got != 10 {
		t.Fatalf("usage.prompt_tokens = %d, want 10; body=%s", got, respBody)
	}
}

func TestChatCompletionsUsesOpenAIResponsesAccountModelMapping(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var seenPath, seenAuth string
	var seenBody []byte
	upstream := newOpenAIResponsesSSEUpstream(&seenPath, &seenAuth, &seenBody)
	defer upstream.Close()

	handler := NewHandler(newOpenAIResponsesRelayStoreWithModelMapping(upstream.URL), nil, nil, nil)

	body := []byte(`{
		"model":"client-alias",
		"messages":[{"role":"user","content":"hi"}]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = req

	handler.ChatCompletions(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if seenPath != "/v1/responses" {
		t.Fatalf("upstream path = %q, want /v1/responses", seenPath)
	}
	if seenAuth != "Bearer sk-direct" {
		t.Fatalf("Authorization = %q, want Bearer sk-direct", seenAuth)
	}
	if model := gjson.GetBytes(seenBody, "model").String(); model != "gpt-4.1-direct" {
		t.Fatalf("upstream model = %q, want gpt-4.1-direct; body=%s", model, seenBody)
	}
}

func TestChatCompletionsUsesOpenAIResponsesAccountWildcardModelMapping(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var seenPath, seenAuth string
	var seenBody []byte
	upstream := newOpenAIResponsesSSEUpstream(&seenPath, &seenAuth, &seenBody)
	defer upstream.Close()

	handler := NewHandler(newOpenAIResponsesRelayStoreWithWildcardModelMapping(upstream.URL), nil, nil, nil)

	body := []byte(`{
		"model":"client-wild",
		"messages":[{"role":"user","content":"hi"}]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = req

	handler.ChatCompletions(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if seenPath != "/v1/responses" {
		t.Fatalf("upstream path = %q, want /v1/responses", seenPath)
	}
	if seenAuth != "Bearer sk-direct" {
		t.Fatalf("Authorization = %q, want Bearer sk-direct", seenAuth)
	}
	if model := gjson.GetBytes(seenBody, "model").String(); model != "gpt-4.1-direct" {
		t.Fatalf("upstream model = %q, want gpt-4.1-direct; body=%s", model, seenBody)
	}
}

func TestPopulateCompactUsageMetaFromRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)

	t.Run("responses compact endpoint", func(t *testing.T) {
		input := &database.UsageLogInput{Endpoint: "/v1/responses/compact"}

		populateCompactUsageMetaFromRequest(nil, input)

		if !input.Compact {
			t.Fatal("Compact = false, want true for /v1/responses/compact")
		}
	})

	t.Run("responses compact endpoint with suffix noise", func(t *testing.T) {
		input := &database.UsageLogInput{InboundEndpoint: " /v1/responses/compact/?trace=1 "}

		populateCompactUsageMetaFromRequest(nil, input)

		if !input.Compact {
			t.Fatal("Compact = false, want true for normalized /v1/responses/compact endpoint")
		}
	})

	t.Run("compaction input item", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Set("raw_body", []byte(`{
			"model":"gpt-5.4",
			"input":[
				{"type":"message","role":"user","content":"hello"},
				{"type":"compaction","summary":"previous context was compacted"}
			]
		}`))
		input := &database.UsageLogInput{Endpoint: "/v1/responses"}

		populateCompactUsageMetaFromRequest(ctx, input)

		if !input.Compact {
			t.Fatal("Compact = false, want true for compaction input item")
		}
	})

	t.Run("nested compaction input item", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Set("raw_body", []byte(`{
			"model":"gpt-5.4",
			"input":[
				{
					"type":"message",
					"role":"developer",
					"content":[
						{"type":"input_text","text":"keep this context"},
						{"type":"compaction","summary":"previous context was compacted"}
					]
				}
			]
		}`))
		input := &database.UsageLogInput{Endpoint: "/v1/responses"}

		populateCompactUsageMetaFromRequest(ctx, input)

		if !input.Compact {
			t.Fatal("Compact = false, want true for nested compaction input item")
		}
	})

	t.Run("normal responses request", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Set("raw_body", []byte(`{"model":"gpt-5.4","input":[{"type":"message","role":"user","content":"hello"}]}`))
		input := &database.UsageLogInput{Endpoint: "/v1/responses"}

		populateCompactUsageMetaFromRequest(ctx, input)

		if input.Compact {
			t.Fatal("Compact = true, want false for normal responses input")
		}
	})
}

func TestPopulateClientIPFromRequest(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.RemoteAddr = "203.0.113.42:53124"
	ctx.Request = req
	input := &database.UsageLogInput{}

	populateClientIPFromRequest(ctx, input)

	if input.ClientIP != "203.0.113.42" {
		t.Fatalf("ClientIP = %q, want 203.0.113.42", input.ClientIP)
	}

	input.ClientIP = "198.51.100.9"
	populateClientIPFromRequest(ctx, input)
	if input.ClientIP != "198.51.100.9" {
		t.Fatalf("existing ClientIP was overwritten: %q", input.ClientIP)
	}
}

func TestResponsesEndpointsAllowGPT55MaxOutputTokens128K(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler := NewHandler(auth.NewStore(nil, nil, nil), nil, nil, nil)
	body := []byte(`{"model":"gpt-5.5","input":"hello","max_output_tokens":128000}`)

	tests := []struct {
		name    string
		path    string
		handler gin.HandlerFunc
	}{
		{name: "responses", path: "/v1/responses", handler: handler.Responses},
		{name: "responses compact", path: "/v1/responses/compact", handler: handler.ResponsesCompact},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			req := httptest.NewRequest(http.MethodPost, test.path, bytes.NewReader(body)).WithContext(ctx)
			req.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			ginCtx, _ := gin.CreateTestContext(recorder)
			ginCtx.Request = req

			test.handler(ginCtx)

			if recorder.Code == http.StatusBadRequest && strings.Contains(recorder.Body.String(), "max_output_tokens") {
				t.Fatalf("gpt-5.5 128k max_output_tokens was rejected by local validation: %s", recorder.Body.String())
			}
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want %d after validation passes; body=%s", recorder.Code, http.StatusServiceUnavailable, recorder.Body.String())
			}
			assertNoAvailableAccountResponse(t, recorder.Body.Bytes())
		})
	}
}

func TestResponsesNoAvailableAccountFailsFastWithoutCancelledContext(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler := NewHandler(auth.NewStore(nil, nil, nil), nil, nil, nil)
	body := []byte(`{"model":"gpt-5.4","input":"hello"}`)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = req

	start := time.Now()
	handler.Responses(ginCtx)
	elapsed := time.Since(start)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusServiceUnavailable, recorder.Body.String())
	}
	assertNoAvailableAccountResponse(t, recorder.Body.Bytes())
	if elapsed > 150*time.Millisecond {
		t.Fatalf("Responses took %s with no dispatch candidates; want fast failure", elapsed)
	}
}

func TestResponsesEnforcesAPIKeyModelAllowlistBeforeDispatch(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler := NewHandler(auth.NewStore(nil, nil, nil), nil, nil, nil)
	body := []byte(`{"model":"gpt-5.4","input":"hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = req
	ginCtx.Set(contextAPIKeyRow, &database.APIKeyRow{
		ID:   42,
		Name: "limited",
		Limits: database.APIKeyLimits{
			ModelAllow: []string{"gpt-5.5", "gpt-5.4-mini"},
		},
	})

	handler.Responses(ginCtx)

	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusForbidden, recorder.Body.String())
	}
	if got := gjson.GetBytes(recorder.Body.Bytes(), "error.message").String(); !strings.Contains(got, "gpt-5.4") || !strings.Contains(got, "not allowed") {
		t.Fatalf("error.message = %q, want model allowlist rejection; body=%s", got, recorder.Body.String())
	}
}

func TestExtractResponseImageGenerationOutputDedupes(t *testing.T) {
	event := []byte(`{"type":"response.output_item.done","item":{"id":"ig_1","type":"image_generation_call","result":"` + tinyPNGBase64 + `","output_format":"png"}}`)
	seen := make(map[string]struct{})

	raw, ok := extractResponseImageGenerationOutput(event, seen)
	if !ok {
		t.Fatal("expected image_generation_call output to be extracted")
	}

	var item map[string]any
	if err := json.Unmarshal(raw, &item); err != nil {
		t.Fatalf("decode extracted image item: %v", err)
	}
	if item["result"] != tinyPNGBase64 {
		t.Fatalf("result = %v, want tiny PNG", item["result"])
	}
	if item["bytes"] != float64(tinyPNGByteSize(t)) || item["width"] != float64(1) || item["height"] != float64(1) {
		t.Fatalf("image stats = bytes:%v width:%v height:%v", item["bytes"], item["width"], item["height"])
	}

	if _, ok := extractResponseImageGenerationOutput(event, seen); ok {
		t.Fatal("expected duplicate image_generation_call output to be ignored")
	}
}

func TestRestoreMissingResponseOutputsUsesOutputItemDone(t *testing.T) {
	response := []byte(`{"id":"resp_1","object":"response","output":[]}`)
	outputItems := []json.RawMessage{
		json.RawMessage(`{"id":"rs_1","type":"reasoning","encrypted_content":"opaque","summary":[]}`),
		json.RawMessage(`{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"{\"age\":30,\"name\":\"John\"}"}]}`),
	}

	got := restoreMissingResponseOutputs(response, outputItems)

	output := gjson.GetBytes(got, "output")
	if !output.IsArray() || len(output.Array()) != 2 {
		t.Fatalf("output count = %d, want 2; body=%s", len(output.Array()), got)
	}
	if typ := output.Array()[0].Get("type").String(); typ != "reasoning" {
		t.Fatalf("first output type = %q, want reasoning; body=%s", typ, got)
	}
	if text := output.Array()[1].Get("content.0.text").String(); text != `{"age":30,"name":"John"}` {
		t.Fatalf("message text = %q, want structured JSON; body=%s", text, got)
	}
}

func TestRestoreMissingResponseOutputsPreservesCompletedOutput(t *testing.T) {
	response := []byte(`{"id":"resp_1","object":"response","output":[{"id":"msg_existing","type":"message","content":[{"type":"output_text","text":"done"}]}]}`)
	outputItems := []json.RawMessage{
		json.RawMessage(`{"id":"msg_1","type":"message","content":[{"type":"output_text","text":"fallback"}]}`),
	}

	got := restoreMissingResponseOutputs(response, outputItems)

	if string(got) != string(response) {
		t.Fatalf("non-empty completed output should be preserved, got %s", got)
	}
}

func TestAppendMissingResponseImageOutputsAddsOutputItemDone(t *testing.T) {
	response := []byte(`{"id":"resp_1"}`)
	imageOutputs := []json.RawMessage{
		json.RawMessage(`{"id":"ig_1","type":"image_generation_call","result":"` + tinyPNGBase64 + `","output_format":"png"}`),
	}

	got := appendMissingResponseImageOutputs(response, imageOutputs)

	var payload struct {
		Output []map[string]any `json:"output"`
	}
	if err := json.Unmarshal(got, &payload); err != nil {
		t.Fatalf("decode merged response: %v", err)
	}
	if len(payload.Output) != 1 {
		t.Fatalf("output count = %d, want 1; body=%s", len(payload.Output), got)
	}
	if payload.Output[0]["type"] != "image_generation_call" || payload.Output[0]["result"] != tinyPNGBase64 {
		t.Fatalf("unexpected output item: %#v", payload.Output[0])
	}
	if payload.Output[0]["bytes"] != float64(tinyPNGByteSize(t)) || payload.Output[0]["width"] != float64(1) || payload.Output[0]["height"] != float64(1) {
		t.Fatalf("image stats = bytes:%v width:%v height:%v", payload.Output[0]["bytes"], payload.Output[0]["width"], payload.Output[0]["height"])
	}

	gotAgain := appendMissingResponseImageOutputs(got, imageOutputs)
	if err := json.Unmarshal(gotAgain, &payload); err != nil {
		t.Fatalf("decode merged response again: %v", err)
	}
	if len(payload.Output) != 1 {
		t.Fatalf("duplicate output count = %d, want 1; body=%s", len(payload.Output), gotAgain)
	}
}

func TestAppendMissingResponseImageOutputsAnnotatesExistingOutput(t *testing.T) {
	response := []byte(`{"id":"resp_1","output":[{"id":"ig_1","type":"image_generation_call","result":"` + tinyPNGBase64 + `","output_format":"png"}]}`)

	got := appendMissingResponseImageOutputs(response, nil)

	var payload struct {
		Output []map[string]any `json:"output"`
	}
	if err := json.Unmarshal(got, &payload); err != nil {
		t.Fatalf("decode annotated response: %v", err)
	}
	if len(payload.Output) != 1 {
		t.Fatalf("output count = %d, want 1; body=%s", len(payload.Output), got)
	}
	if payload.Output[0]["bytes"] != float64(tinyPNGByteSize(t)) || payload.Output[0]["width"] != float64(1) || payload.Output[0]["height"] != float64(1) {
		t.Fatalf("image stats = bytes:%v width:%v height:%v", payload.Output[0]["bytes"], payload.Output[0]["width"], payload.Output[0]["height"])
	}
}

func TestAccountFilterForSparkAllowsNonFreeOrUnknownPlans(t *testing.T) {
	filter := accountFilterForModel("gpt-5.3-codex-spark")
	if filter == nil {
		t.Fatal("expected filter for spark model")
	}
	for _, planType := range []string{"pro", "prolite", "plus", "team", "business", "enterprise", "", "unknown"} {
		if !filter(&auth.Account{PlanType: planType}) {
			t.Fatalf("spark filter should allow plan_type=%q", planType)
		}
	}
	for _, planType := range []string{"free", "api"} {
		if filter(&auth.Account{PlanType: planType}) {
			t.Fatalf("spark filter should reject plan_type=%q", planType)
		}
	}
	normalFilter := accountFilterForModel("gpt-5.3-codex")
	if normalFilter == nil || !normalFilter(&auth.Account{PlanType: "plus"}) {
		t.Fatal("non-spark model filter should allow available accounts")
	}
	directOpenAIAccount := &auth.Account{
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      "https://api.openai.com",
		APIKey:       "sk-test",
		Models:       []string{"gpt-4.1"},
	}
	if normalFilter(directOpenAIAccount) {
		t.Fatal("codex account filter should reject direct OpenAI Responses accounts")
	}
	responsesFilter := accountFilterForResponsesModel("gpt-4.1", false)
	if !responsesFilter(directOpenAIAccount) {
		t.Fatal("responses filter should allow direct OpenAI account for configured model")
	}
	if responsesFilter(&auth.Account{AccessToken: "codex-at", PlanType: "plus"}) {
		t.Fatal("responses filter should reject codex accounts for direct-only models")
	}
	if !accountFilterForResponsesModel("gpt-4.1", true)(&auth.Account{AccessToken: "codex-at", PlanType: "plus"}) {
		t.Fatal("responses filter should allow codex accounts when model is in Codex catalog")
	}
	if accountFilterForResponsesModel("gpt-4.2", false)(directOpenAIAccount) {
		t.Fatal("responses filter should reject direct OpenAI account for unconfigured model")
	}
	cooled := &auth.Account{PlanType: "pro"}
	cooled.SetModelCooldownUntil("gpt-5.3-codex-spark", "model_capacity", time.Now().Add(time.Minute))
	if filter(cooled) {
		t.Fatal("filter should reject model-cooled accounts")
	}
}

func TestSupportedModelIDsIncludesOpenAIResponsesAccountModels(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	store.AddAccount(&auth.Account{
		DBID:         1,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      "https://api.openai.com",
		APIKey:       "sk-test",
		Models:       []string{"gpt-4.1-direct"},
	})

	handler := &Handler{store: store}
	models := handler.supportedModelIDs(context.Background())
	for _, model := range models {
		if model == "gpt-4.1-direct" {
			return
		}
	}
	t.Fatalf("supported models missing direct OpenAI model: %v", models)
}

func TestClassify429UsageLimitExactResetUsesAccountCooldown(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	decision := classify429RateLimit(&auth.Account{PlanType: "team"}, []byte(`{"error":{"type":"usage_limit_reached","resets_in_seconds":120}}`), nil, now, "gpt-5.4")
	if decision.Scope != rateLimitScopeAccount || decision.Reason != "usage_limit" {
		t.Fatalf("decision = %#v, want account usage_limit", decision)
	}
	if decision.Cooldown != 120*time.Second {
		t.Fatalf("Cooldown = %v, want 120s", decision.Cooldown)
	}
}

func TestClassify429CapacityUsesModelCooldown(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	body := []byte(`{"error":{"message":"Selected model is at capacity. Please try a different model."}}`)
	decision := classify429RateLimit(&auth.Account{PlanType: "team"}, body, nil, now, "gpt-5.4")
	if decision.Scope != rateLimitScopeModel || decision.Reason != "model_capacity" {
		t.Fatalf("decision = %#v, want model capacity cooldown", decision)
	}
	if decision.Cooldown != 5*time.Minute {
		t.Fatalf("Cooldown = %v, want 5m", decision.Cooldown)
	}
}

func TestClassify429Header7dUsesAccountCooldown(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	resp := &http.Response{Header: make(http.Header)}
	resp.Header.Set("x-codex-secondary-used-percent", "100")
	resp.Header.Set("x-codex-secondary-window-minutes", "10080")
	resp.Header.Set("x-codex-secondary-reset-after-seconds", "3600")
	decision := classify429RateLimit(&auth.Account{PlanType: "team"}, nil, resp, now, "gpt-5.4")
	if decision.Scope != rateLimitScopeAccount || decision.Reason != "rate_limited_7d" {
		t.Fatalf("decision = %#v, want 7d account cooldown", decision)
	}
	if decision.Cooldown != time.Hour {
		t.Fatalf("Cooldown = %v, want 1h", decision.Cooldown)
	}
}

func TestShouldRetryHTTPStatusSplitsRateLimitBudget(t *testing.T) {
	generalRetries := 0
	rateLimitRetries := 0
	if !shouldRetryHTTPStatus(http.StatusTooManyRequests, &generalRetries, &rateLimitRetries, 2, 1) {
		t.Fatal("first 429 should consume rate-limit retry budget")
	}
	if shouldRetryHTTPStatus(http.StatusTooManyRequests, &generalRetries, &rateLimitRetries, 2, 1) {
		t.Fatal("second 429 should be blocked by rate-limit retry budget")
	}
	if !shouldRetryHTTPStatus(http.StatusServiceUnavailable, &generalRetries, &rateLimitRetries, 2, 1) {
		t.Fatal("503 should still use general retry budget")
	}
	if generalRetries != 1 || rateLimitRetries != 1 {
		t.Fatalf("budgets = general %d rate %d, want 1/1", generalRetries, rateLimitRetries)
	}
}

func TestDeactivatedWorkspace402MarksAccountError(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: 42, AccessToken: "at"}
	handler := &Handler{store: store}
	body := []byte(`{"detail":{"code":"deactivated_workspace"}}`)

	if !IsDeactivatedWorkspaceError(body) {
		t.Fatal("expected deactivated workspace body to be detected")
	}
	if got := upstreamErrorKind(http.StatusPaymentRequired, body, codex429Decision{}); got != "deactivated_workspace" {
		t.Fatalf("upstreamErrorKind = %q, want deactivated_workspace", got)
	}

	handler.applyCooldownForModel(account, http.StatusPaymentRequired, body, &http.Response{Header: make(http.Header)}, "gpt-5.4")

	if got := account.RuntimeStatus(); got != "error" {
		t.Fatalf("RuntimeStatus() = %q, want error", got)
	}
	account.Mu().RLock()
	errorMsg := account.ErrorMsg
	account.Mu().RUnlock()
	if !strings.Contains(errorMsg, "deactivated_workspace") {
		t.Fatalf("ErrorMsg = %q, want deactivated_workspace", errorMsg)
	}
}

func TestSendFinalUpstreamError_UsageLimitRewrites429(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)

	handler := &Handler{}
	body := []byte(`{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","plan_type":"free","resets_at":1775317531,"resets_in_seconds":602705}}`)

	handler.sendFinalUpstreamError(ctx, http.StatusTooManyRequests, body)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
	if got := recorder.Header().Get("Retry-After"); got != "602705" {
		t.Fatalf("Retry-After = %q, want %q", got, "602705")
	}

	var payload struct {
		Error struct {
			Message         string `json:"message"`
			Type            string `json:"type"`
			Code            string `json:"code"`
			PlanType        string `json:"plan_type"`
			ResetsAt        int64  `json:"resets_at"`
			ResetsInSeconds int64  `json:"resets_in_seconds"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.Error.Type != "server_error" {
		t.Fatalf("type = %q, want %q", payload.Error.Type, "server_error")
	}
	if payload.Error.Code != "account_pool_usage_limit_reached" {
		t.Fatalf("code = %q, want %q", payload.Error.Code, "account_pool_usage_limit_reached")
	}
	if payload.Error.PlanType != "free" {
		t.Fatalf("plan_type = %q, want %q", payload.Error.PlanType, "free")
	}
	if payload.Error.ResetsAt != 1775317531 {
		t.Fatalf("resets_at = %d, want %d", payload.Error.ResetsAt, 1775317531)
	}
	if payload.Error.ResetsInSeconds != 602705 {
		t.Fatalf("resets_in_seconds = %d, want %d", payload.Error.ResetsInSeconds, 602705)
	}
	if payload.Error.Message == "" {
		t.Fatal("expected non-empty aggregated error message")
	}
}

func TestSendFinalUpstreamError_FallsBackForNonUsageLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)

	handler := &Handler{}
	body := []byte(`{"error":{"type":"rate_limit_error","message":"Too many requests"}}`)

	handler.sendFinalUpstreamError(ctx, http.StatusTooManyRequests, body)

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusTooManyRequests)
	}
	if got := recorder.Header().Get("Retry-After"); got != "" {
		t.Fatalf("Retry-After = %q, want empty", got)
	}
}

func TestSendFinalUpstreamError_UsageLimitMissingTimeFields(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)

	handler := &Handler{}
	// usage_limit_reached 但不含 resets_at / resets_in_seconds
	body := []byte(`{"error":{"type":"usage_limit_reached","message":"limit reached"}}`)

	handler.sendFinalUpstreamError(ctx, http.StatusTooManyRequests, body)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
	// 无 resets_in_seconds 时不应设置 Retry-After
	if got := recorder.Header().Get("Retry-After"); got != "" {
		t.Fatalf("Retry-After = %q, want empty (no resets_in_seconds)", got)
	}

	// 验证零值字段不出现在响应中
	var raw map[string]map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	errObj := raw["error"]
	if _, exists := errObj["resets_at"]; exists {
		t.Fatal("resets_at should be omitted when 0")
	}
	if _, exists := errObj["resets_in_seconds"]; exists {
		t.Fatal("resets_in_seconds should be omitted when 0")
	}
	if _, exists := errObj["plan_type"]; exists {
		t.Fatal("plan_type should be omitted when empty")
	}
}

func TestSendFinalUpstreamError_Non429StatusPassthrough(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)

	handler := &Handler{}
	body := []byte(`{"error":{"type":"server_error","message":"internal failure"}}`)

	handler.sendFinalUpstreamError(ctx, http.StatusInternalServerError, body)

	// 非 429 直接透传原状态码
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}
}

func TestSendFinalUpstreamError_UsageLimitRewrites500(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)

	handler := &Handler{}
	body := []byte(`{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","plan_type":"free","resets_in_seconds":3600}}`)

	handler.sendFinalUpstreamError(ctx, http.StatusInternalServerError, body)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
	if got := recorder.Header().Get("Retry-After"); got != "3600" {
		t.Fatalf("Retry-After = %q, want 3600", got)
	}
	var payload struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.Error.Code != "account_pool_usage_limit_reached" {
		t.Fatalf("code = %q, want account_pool_usage_limit_reached", payload.Error.Code)
	}
}

// TestSendFinalUpstreamError_UpstreamUnauthorizedRemappedTo503 验证上游账号 401
// (OAuth token 失效)重试耗尽后改写为 503 池级错误，不原样以 401 透传给客户端，
// 避免客户端误判自己的 API key 失效 (issue #323)。
func TestSendFinalUpstreamError_UpstreamUnauthorizedRemappedTo503(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)

	handler := &Handler{}
	body := []byte(`{"error":{"message":"Encountered invalidated oauth token for user, failing request","code":"token_revoked"},"status":401}`)

	handler.sendFinalUpstreamError(ctx, http.StatusUnauthorized, body)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d (upstream 401 must not surface as client 401)", recorder.Code, http.StatusServiceUnavailable)
	}
	var payload struct {
		Error struct {
			Code string `json:"code"`
			Type string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.Error.Code != "account_pool_unauthorized" {
		t.Fatalf("code = %q, want account_pool_unauthorized", payload.Error.Code)
	}
}

// TestSendFinalUpstreamError_MissingScope401Passthrough 验证 missing_scope 类 401
// 仍按原状态码透传(它是可保留在池中的良性 401，不应被当作账号鉴权失效改写)。
func TestSendFinalUpstreamError_MissingScope401Passthrough(t *testing.T) {
	gin.SetMode(gin.TestMode)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)

	handler := &Handler{}
	body := []byte(`{"error":{"message":"missing scope api.responses.write","code":"missing_scope"}}`)

	handler.sendFinalUpstreamError(ctx, http.StatusUnauthorized, body)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d (missing_scope 401 passes through)", recorder.Code, http.StatusUnauthorized)
	}
}

func TestCompute429CooldownPlusUsesWindowHeaders(t *testing.T) {
	handler := &Handler{}
	account := &auth.Account{PlanType: "plus"}
	resp := &http.Response{
		Header: make(http.Header),
	}
	resp.Header.Set("x-codex-primary-used-percent", "100")
	resp.Header.Set("x-codex-primary-window-minutes", "300")
	resp.Header.Set("x-codex-secondary-used-percent", "20")
	resp.Header.Set("x-codex-secondary-window-minutes", "10080")

	got := handler.compute429Cooldown(account, []byte(`{"error":{"type":"usage_limit_reached"}}`), resp)
	want := 5 * time.Hour
	if got != want {
		t.Fatalf("cooldown = %v, want %v", got, want)
	}
}

func TestCompute429CooldownPlusPrefersExactResetTime(t *testing.T) {
	handler := &Handler{}
	account := &auth.Account{PlanType: "plus"}
	resp := &http.Response{
		Header: make(http.Header),
	}
	resp.Header.Set("x-codex-primary-used-percent", "100")
	resp.Header.Set("x-codex-primary-window-minutes", "10080")

	got := handler.compute429Cooldown(account, []byte(`{"error":{"type":"usage_limit_reached","resets_in_seconds":1800}}`), resp)
	want := 30 * time.Minute
	if got != want {
		t.Fatalf("cooldown = %v, want %v", got, want)
	}
}

func TestApply429CooldownPremiumMarks5hRateLimitFromWindow(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: 101, PlanType: "plus"}
	resp := &http.Response{Header: make(http.Header)}
	resp.Header.Set("x-codex-primary-used-percent", "100")
	resp.Header.Set("x-codex-primary-window-minutes", "300")
	resp.Header.Set("x-codex-primary-reset-after-seconds", "900")

	decision := Apply429Cooldown(store, account, []byte(`{"error":{"type":"usage_limit_reached"}}`), resp, "gpt-5.4")

	if decision.Scope != rateLimitScopeAccount || decision.Reason != "rate_limited_5h" {
		t.Fatalf("decision = %#v, want premium 5h account decision", decision)
	}
	if !account.IsPremium5hRateLimited() {
		t.Fatal("expected account to enter premium 5h rate limited state")
	}
	pct5h, resetAt, ok := account.GetUsageSnapshot5h()
	if !ok {
		t.Fatal("expected 5h snapshot to be set")
	}
	if pct5h != 100 {
		t.Fatalf("usage_percent_5h = %v, want 100", pct5h)
	}
	if got := time.Until(resetAt); got < 14*time.Minute || got > 16*time.Minute {
		t.Fatalf("resetAt delta = %v, want about 15m", got)
	}
}

func TestApply429CooldownUsageLimitUpdatesFreePlanMetadata(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New 返回错误: %v", err)
	}
	defer db.Close()

	id, err := db.InsertAccountWithCredentials(ctx, "usage-limit-account", map[string]interface{}{
		"plan_type": "pro",
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials 返回错误: %v", err)
	}

	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: id, AccessToken: "at", PlanType: "pro"}
	body := []byte(`{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","plan_type":"free","resets_in_seconds":3600}}`)

	decision := Apply429Cooldown(store, account, body, &http.Response{Header: make(http.Header)}, "gpt-5.4")

	if decision.Scope != rateLimitScopeAccount || decision.Reason != "usage_limit" {
		t.Fatalf("decision = %#v, want account usage_limit", decision)
	}
	if got := account.GetPlanType(); got != "free" {
		t.Fatalf("account plan_type = %q, want free", got)
	}
	pct, ok := account.GetUsagePercent7d()
	if !ok || pct != 100 {
		t.Fatalf("usage_percent_7d = %v ok=%v, want 100 true", pct, ok)
	}
	if got := account.RuntimeStatus(); got != "usage_exhausted" {
		t.Fatalf("RuntimeStatus() = %q, want usage_exhausted", got)
	}

	resetDelta := time.Until(account.GetReset7dAt())
	if resetDelta < 59*time.Minute || resetDelta > 61*time.Minute {
		t.Fatalf("reset_7d_at delta = %v, want about 1h", resetDelta)
	}

	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatalf("GetAccountByID 返回错误: %v", err)
	}
	if got := row.GetCredential("plan_type"); got != "free" {
		t.Fatalf("persisted plan_type = %q, want free", got)
	}
	if got := row.GetCredential("codex_7d_used_percent"); got != "100" {
		t.Fatalf("persisted codex_7d_used_percent = %q, want 100", got)
	}
	if got := row.GetCredential("codex_7d_reset_at"); got == "" {
		t.Fatal("persisted codex_7d_reset_at is empty")
	}
}

func TestApplyCooldownUsageLimit500UpdatesFreePlanMetadata(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: 201, AccessToken: "at", PlanType: "free"}
	handler := &Handler{store: store}
	body := []byte(`{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","plan_type":"free","resets_in_seconds":7200}}`)

	decision := handler.applyCooldownForModel(account, http.StatusInternalServerError, body, &http.Response{Header: make(http.Header)}, "gpt-5.4")

	if decision.Scope != rateLimitScopeAccount || decision.Reason != "usage_limit" {
		t.Fatalf("decision = %#v, want account usage_limit", decision)
	}
	if got := upstreamErrorKind(http.StatusInternalServerError, body, decision); got != "usage_limit" {
		t.Fatalf("upstreamErrorKind = %q, want usage_limit", got)
	}
	pct, ok := account.GetUsagePercent7d()
	if !ok || pct != 100 {
		t.Fatalf("usage_percent_7d = %v ok=%v, want 100 true", pct, ok)
	}
	if got := account.RuntimeStatus(); got != "usage_exhausted" {
		t.Fatalf("RuntimeStatus() = %q, want usage_exhausted", got)
	}
}

func TestApplyResponseFailedUsageLimitRemovesAccountFromScheduling(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: 301, AccessToken: "at", PlanType: "pro", Status: auth.StatusReady}
	store.AddAccount(account)
	handler := &Handler{store: store}
	payload := []byte(`{"type":"response.failed","response":{"error":{"type":"usage_limit_reached","message":"The usage limit has been reached","plan_type":"free","resets_in_seconds":3600}}}`)

	decision := handler.applyResponseFailedCooldown(account, payload, &http.Response{Header: make(http.Header)}, "gpt-5.4")

	if decision.Scope != rateLimitScopeAccount || decision.Reason != "usage_limit" {
		t.Fatalf("decision = %#v, want account usage_limit", decision)
	}
	if got := account.GetPlanType(); got != "free" {
		t.Fatalf("account plan_type = %q, want free", got)
	}
	pct, ok := account.GetUsagePercent7d()
	if !ok || pct != 100 {
		t.Fatalf("usage_percent_7d = %v ok=%v, want 100 true", pct, ok)
	}
	if got := account.RuntimeStatus(); got != "usage_exhausted" {
		t.Fatalf("RuntimeStatus() = %q, want usage_exhausted", got)
	}
	if account.IsAvailable() {
		t.Fatal("IsAvailable() = true, want false after response.failed usage_limit")
	}
	if next := store.Next(); next != nil {
		t.Fatalf("store.Next() returned account %d, want nil after usage exhaustion", next.ID())
	}
}

func TestResponseFailedRetryableClassification(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    bool
	}{
		{
			name:    "usage_limit nested in response.error",
			payload: `{"type":"response.failed","response":{"error":{"type":"usage_limit_reached","message":"limit"}}}`,
			want:    true,
		},
		{
			name:    "rate_limit top-level error",
			payload: `{"type":"response.failed","error":{"type":"rate_limit_exceeded","message":"slow down"}}`,
			want:    true,
		},
		{
			name:    "5xx server error",
			payload: `{"type":"response.failed","response":{"status_code":503,"error":{"type":"server_error"}}}`,
			want:    true,
		},
		{
			name:    "unauthorized",
			payload: `{"type":"response.failed","response":{"error":{"type":"invalid_api_key"}}}`,
			want:    true,
		},
		{
			name:    "non-retryable invalid_request",
			payload: `{"type":"response.failed","response":{"error":{"type":"invalid_request_error","message":"bad input"}}}`,
			want:    false,
		},
		{
			name:    "empty payload",
			payload: ``,
			want:    false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := responseFailedRetryable([]byte(tc.payload)); got != tc.want {
				t.Fatalf("responseFailedRetryable(%s) = %v, want %v", tc.payload, got, tc.want)
			}
		})
	}
}

func TestSyncCodexUsageStateUpdatesPlanTypeFromHeader(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New returned error: %v", err)
	}
	defer db.Close()

	id, err := db.InsertAccountWithCredentials(ctx, "plan-header-account", map[string]interface{}{
		"plan_type": "free",
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials returned error: %v", err)
	}

	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: id, AccessToken: "at", PlanType: "free"}
	resp := &http.Response{Header: make(http.Header)}
	resp.Header.Set("x-codex-plan-type", "Enterprise")
	resp.Header.Set("x-codex-primary-used-percent", "12")
	resp.Header.Set("x-codex-primary-window-minutes", "300")
	resp.Header.Set("x-codex-primary-reset-after-seconds", "1200")
	resp.Header.Set("x-codex-secondary-used-percent", "3")
	resp.Header.Set("x-codex-secondary-window-minutes", "10080")
	resp.Header.Set("x-codex-secondary-reset-after-seconds", "600000")

	result := SyncCodexUsageState(store, account, resp)

	if got := account.GetPlanType(); got != "enterprise" {
		t.Fatalf("account plan_type = %q, want enterprise", got)
	}
	if !result.Used5hHeaders || !result.HasUsage5h || !result.HasUsage7d {
		t.Fatalf("usage sync result = %#v, want 5h and 7d headers detected", result)
	}
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatalf("GetAccountByID returned error: %v", err)
	}
	if got := row.GetCredential("plan_type"); got != "enterprise" {
		t.Fatalf("persisted plan_type = %q, want enterprise", got)
	}
}

func TestApply429CooldownUnknown429UsesModelCooldown(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: 102, PlanType: "pro"}

	decision := Apply429Cooldown(store, account, []byte(`{"error":{"type":"rate_limit_error","message":"Too many requests"}}`), &http.Response{Header: make(http.Header)}, "gpt-5.4")

	if decision.Scope != rateLimitScopeModel {
		t.Fatalf("decision.Scope = %q, want model", decision.Scope)
	}
	if got := time.Until(decision.ResetAt); got < 4*time.Minute || got > 6*time.Minute {
		t.Fatalf("resetAt delta = %v, want about 5m", got)
	}
	if !account.IsModelRateLimited("gpt-5.4") {
		t.Fatal("expected model cooldown")
	}
}

func TestSyncCodexUsageStateTriggersPremium5hLimitWith5hHeadersOnly(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: 103, PlanType: "team"}
	resp := &http.Response{Header: make(http.Header)}
	resp.Header.Set("x-codex-primary-used-percent", "100")
	resp.Header.Set("x-codex-primary-window-minutes", "300")
	resp.Header.Set("x-codex-primary-reset-after-seconds", "600")

	result := SyncCodexUsageState(store, account, resp)

	if !result.Used5hHeaders {
		t.Fatal("expected 5h headers to be detected")
	}
	if result.HasUsage7d {
		t.Fatal("expected no 7d usage snapshot")
	}
	if !result.HasUsage5h {
		t.Fatal("expected 5h usage snapshot")
	}
	if !result.Persisted5hOnly {
		t.Fatal("expected 5h-only persistence path to be selected")
	}
	if !result.Premium5hRateLimited {
		t.Fatal("expected premium 5h rate limit to trigger")
	}
	if !account.IsPremium5hRateLimited() {
		t.Fatal("expected account to be premium 5h rate limited")
	}
}

func TestSyncCodexUsageState5hOnlyDoesNotRefreshStale7dProbeFreshness(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{
		DBID:                104,
		AccessToken:         "at",
		PlanType:            "plus",
		Status:              auth.StatusReady,
		UsagePercent7d:      40,
		UsagePercent7dValid: true,
		UsageUpdatedAt:      time.Now().Add(-20 * time.Minute),
	}
	resp := &http.Response{Header: make(http.Header)}
	resp.Header.Set("x-codex-primary-used-percent", "83")
	resp.Header.Set("x-codex-primary-window-minutes", "300")
	resp.Header.Set("x-codex-primary-reset-after-seconds", "600")

	result := SyncCodexUsageState(store, account, resp)

	if !result.Used5hHeaders || !result.HasUsage5h {
		t.Fatalf("usage sync result = %#v, want 5h headers to populate a 5h snapshot", result)
	}
	if result.HasUsage7d {
		t.Fatalf("usage sync result = %#v, want no 7d snapshot from 5h-only headers", result)
	}
	if !result.Persisted5hOnly {
		t.Fatalf("usage sync result = %#v, want 5h-only persistence path", result)
	}
	if !account.NeedsUsageProbe(10 * time.Minute) {
		t.Fatal("NeedsUsageProbe() = false, want true because 5h-only header sync must not refresh stale 7d freshness")
	}
}

func TestSyncCodexUsageStateMarks7dUsageLimited(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New returned error: %v", err)
	}
	defer db.Close()

	id, err := db.InsertAccountWithCredentials(ctx, "limited-7d", map[string]interface{}{
		"plan_type": "team",
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials returned error: %v", err)
	}

	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: id, AccessToken: "at", PlanType: "team", Status: auth.StatusReady, HealthTier: auth.HealthTierHealthy}
	resp := &http.Response{Header: make(http.Header)}
	resp.Header.Set("x-codex-primary-used-percent", "20")
	resp.Header.Set("x-codex-primary-window-minutes", "300")
	resp.Header.Set("x-codex-primary-reset-after-seconds", "1200")
	resp.Header.Set("x-codex-secondary-used-percent", "100")
	resp.Header.Set("x-codex-secondary-window-minutes", "10080")
	resp.Header.Set("x-codex-secondary-reset-after-seconds", "3600")

	result := SyncCodexUsageState(store, account, resp)

	if !result.Usage7dRateLimited {
		t.Fatalf("Usage7dRateLimited = false, result=%+v", result)
	}
	if got := account.RuntimeStatus(); got != "rate_limited" {
		t.Fatalf("RuntimeStatus() = %q, want rate_limited", got)
	}
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatalf("GetAccountByID returned error: %v", err)
	}
	if row.CooldownReason != "rate_limited" || !row.CooldownUntil.Valid {
		t.Fatalf("persisted cooldown = (%q, %v), want active rate_limited", row.CooldownReason, row.CooldownUntil)
	}
}

func TestSyncCodexUsageStateCreditAccountSkips7dUsageLimit(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New returned error: %v", err)
	}
	defer db.Close()

	id, err := db.InsertAccountWithCredentials(ctx, "credit-7d", map[string]interface{}{
		"plan_type": "team",
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials returned error: %v", err)
	}

	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.5"})
	account := &auth.Account{
		DBID:                  id,
		AccessToken:           "at",
		PlanType:              "team",
		Status:                auth.StatusReady,
		HealthTier:            auth.HealthTierHealthy,
		CreditEnabled:         true,
		CreditSkipUsageWindow: true,
	}
	resp := &http.Response{Header: make(http.Header)}
	resp.Header.Set("x-codex-primary-used-percent", "20")
	resp.Header.Set("x-codex-primary-window-minutes", "300")
	resp.Header.Set("x-codex-primary-reset-after-seconds", "1200")
	resp.Header.Set("x-codex-secondary-used-percent", "100")
	resp.Header.Set("x-codex-secondary-window-minutes", "10080")
	resp.Header.Set("x-codex-secondary-reset-after-seconds", "3600")

	result := SyncCodexUsageState(store, account, resp)

	if !result.HasUsage7d || result.UsagePct7d != 100 {
		t.Fatalf("usage sync result = %+v, want 7d snapshot at 100", result)
	}
	if result.Usage7dRateLimited {
		t.Fatalf("Usage7dRateLimited = true, want false for credit account")
	}
	if got := account.RuntimeStatus(); got != "active" {
		t.Fatalf("RuntimeStatus() = %q, want active for credit account", got)
	}
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatalf("GetAccountByID returned error: %v", err)
	}
	if row.CooldownReason != "" || row.CooldownUntil.Valid {
		t.Fatalf("persisted cooldown = (%q, %v), want no cooldown", row.CooldownReason, row.CooldownUntil)
	}
	pct7d, ok := account.GetUsagePercent7d()
	if !ok || pct7d != 100 {
		t.Fatalf("usage_percent_7d = (%v, %v), want 100 with valid snapshot", pct7d, ok)
	}
}

func TestAuthMiddlewareSetsAPIKeyContext(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New 返回错误: %v", err)
	}
	defer db.Close()

	key := "sk-test-auth-1234567890"
	id, err := db.InsertAPIKey(context.Background(), "Team A", key)
	if err != nil {
		t.Fatalf("InsertAPIKey 返回错误: %v", err)
	}

	handler := NewHandler(nil, db, nil, nil)
	router := gin.New()
	router.Use(handler.authMiddleware())
	router.GET("/ok", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"id":     c.MustGet(contextAPIKeyID),
			"name":   c.MustGet(contextAPIKeyName),
			"masked": c.MustGet(contextAPIKeyMasked),
			"raw":    c.MustGet("apiKey"),
		})
	})

	req := httptest.NewRequest(http.MethodGet, "/ok", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}

	var payload struct {
		ID     int64  `json:"id"`
		Name   string `json:"name"`
		Masked string `json:"masked"`
		Raw    string `json:"raw"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("json.Unmarshal 返回错误: %v", err)
	}

	if payload.ID != id {
		t.Fatalf("id = %d, want %d", payload.ID, id)
	}
	if payload.Name != "Team A" {
		t.Fatalf("name = %q, want %q", payload.Name, "Team A")
	}
	if payload.Masked == "" || payload.Masked == key {
		t.Fatalf("masked = %q, want masked value", payload.Masked)
	}
	if payload.Raw != key {
		t.Fatalf("raw = %q, want %q", payload.Raw, key)
	}
}

func TestAuthMiddlewareRejectsExpiredAPIKey(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New 返回错误: %v", err)
	}
	defer db.Close()

	key := "sk-test-expired-1234567890"
	if _, err := db.InsertAPIKeyWithOptions(context.Background(), database.APIKeyInput{
		Name:      "Expired",
		Key:       key,
		ExpiresAt: sql.NullTime{Time: time.Now().Add(-time.Hour), Valid: true},
	}); err != nil {
		t.Fatalf("InsertAPIKeyWithOptions 返回错误: %v", err)
	}

	handler := NewHandler(nil, db, nil, nil)
	router := gin.New()
	router.Use(handler.authMiddleware())
	router.GET("/ok", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })

	req := httptest.NewRequest(http.MethodGet, "/ok", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d, body=%s", recorder.Code, http.StatusUnauthorized, recorder.Body.String())
	}
	if got := gjson.GetBytes(recorder.Body.Bytes(), "error.code").String(); got != string(api.ErrCodeInvalidAuth) {
		t.Fatalf("error.code = %q, want %q", got, api.ErrCodeInvalidAuth)
	}
}

func TestAuthMiddlewareRejectsQuotaExhaustedAPIKey(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New 返回错误: %v", err)
	}
	defer db.Close()

	key := "sk-test-quota-1234567890"
	if _, err := db.InsertAPIKeyWithOptions(context.Background(), database.APIKeyInput{
		Name:       "Quota",
		Key:        key,
		QuotaLimit: 0.01,
		QuotaUsed:  0.01,
	}); err != nil {
		t.Fatalf("InsertAPIKeyWithOptions 返回错误: %v", err)
	}

	handler := NewHandler(nil, db, nil, nil)
	router := gin.New()
	router.Use(handler.authMiddleware())
	router.GET("/ok", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })

	req := httptest.NewRequest(http.MethodGet, "/ok", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want %d, body=%s", recorder.Code, http.StatusTooManyRequests, recorder.Body.String())
	}
	if got := gjson.GetBytes(recorder.Body.Bytes(), "error.code").String(); got != string(api.ErrCodeRateLimitReached) {
		t.Fatalf("error.code = %q, want %q", got, api.ErrCodeRateLimitReached)
	}
}

func TestAuthMiddlewareUsesRuntimeAPIKeyCache(t *testing.T) {
	gin.SetMode(gin.TestMode)

	key := "sk-test-runtime-cache-1234567890"
	tc := cache.NewMemory(1)
	ctx := context.Background()
	keyPayload, _ := json.Marshal(apiKeyRuntimeRecord{
		ID:        42,
		Name:      "Cached Team",
		CreatedAt: time.Now(),
	})
	if err := tc.SetRuntime(ctx, apiKeyCacheNamespace, key, keyPayload, time.Minute); err != nil {
		t.Fatalf("SetRuntime api key: %v", err)
	}
	countPayload, _ := json.Marshal(apiKeyCountRuntimeRecord{Count: 1})
	if err := tc.SetRuntime(ctx, apiKeyCountCacheNamespace, "all", countPayload, time.Minute); err != nil {
		t.Fatalf("SetRuntime api key count: %v", err)
	}

	handler := NewHandler(nil, nil, nil, nil)
	handler.SetRuntimeCache(tc)
	router := gin.New()
	router.Use(handler.authMiddleware())
	router.GET("/ok", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{
			"id":   c.MustGet(contextAPIKeyID),
			"name": c.MustGet(contextAPIKeyName),
			"raw":  c.MustGet("apiKey"),
		})
	})

	req := httptest.NewRequest(http.MethodGet, "/ok", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	recorder := httptest.NewRecorder()

	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var payload struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
		Raw  string `json:"raw"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("json.Unmarshal 返回错误: %v", err)
	}
	if payload.ID != 42 || payload.Name != "Cached Team" || payload.Raw != key {
		t.Fatalf("payload = %#v", payload)
	}
}

func TestSessionAffinityKeySeparatesDifferentAPIKeys(t *testing.T) {
	key1 := sessionAffinityKey("session-1", 1)
	key2 := sessionAffinityKey("session-1", 2)

	if key1 == key2 {
		t.Fatalf("sessionAffinityKey should differ for different apiKeyID: %q", key1)
	}
	if got := sessionAffinityKey("session-1", 0); got != "session-1" {
		t.Fatalf("sessionAffinityKey() with apiKeyID=0 = %q, want session-1", got)
	}
}

func TestAccountAffinityUsesOnlyExplicitSessionID(t *testing.T) {
	headers := http.Header{}
	headers.Set("Authorization", "Bearer sk-test")
	body := []byte(`{}`)

	if fallback := ResolveSessionID(headers, body); fallback == "" {
		t.Fatal("ResolveSessionID should still provide an upstream fallback session")
	}
	explicit := ResolveExplicitSessionID(headers, body)
	if explicit != "" {
		t.Fatalf("explicit session = %q, want empty", explicit)
	}
	if got := sessionAffinityKey(explicit, 1); got != "" {
		t.Fatalf("account affinity key from API-key fallback = %q, want empty", got)
	}

	headers.Set("Session_id", "session-1")
	explicit = ResolveExplicitSessionID(headers, body)
	if got := sessionAffinityKey(explicit, 1); got != "session-1::api-key:1" {
		t.Fatalf("explicit account affinity key = %q, want session-1::api-key:1", got)
	}
}

// TestResponsesWebSocketStripsInjectedImageTool verifies that a plain
// conversation request — which PrepareResponsesWebSocketBody auto-injects an
// image_generation tool into — has that tool stripped before going to the
// WebSocket upstream, so the model can't autonomously generate an image and
// hang the WS stream (issue #220).
func TestResponsesWebSocketStripsInjectedImageTool(t *testing.T) {
	gin.SetMode(gin.TestMode)

	previousExec := WebsocketExecuteFunc
	t.Cleanup(func() { WebsocketExecuteFunc = previousExec })

	bodyCh := make(chan []byte, 1)
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		bodyCh <- append([]byte(nil), requestBody...)
		sse := `data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2},"service_tier":"default"}}` + "\n\n"
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(sse)),
		}, nil
	}

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "at", PlanType: "plus", AccountID: "acct-1"})
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)

	router := gin.New()
	handler.RegisterRoutes(router)
	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket failed: %v", err)
	}
	defer conn.Close()

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"model":"gpt-5.4","input":"hello"}`)); err != nil {
		t.Fatalf("write request: %v", err)
	}

	select {
	case gotBody := <-bodyCh:
		// 整个请求体不应再出现 image_generation（工具与桥接 instructions 均已剥离）。
		if strings.Contains(string(gotBody), "image_generation") {
			t.Fatalf("websocket upstream body should not mention image_generation: %s", gotBody)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for upstream request")
	}
}

// TestResolveAPIKeyDistinguishesDBFailureFrom404 验证 resolveAPIKey 区分三态：
// 命中、查无此 key、DB 故障。DB 故障(如连接耗尽 "too many clients")必须返回
// 非 nil error，让中间件回 503 而非误报 401 invalid_api_key (issue #323)。
func TestResolveAPIKeyDistinguishesDBFailureFrom404(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}

	if _, err := db.InsertAPIKey(ctx, "tester", "sk-valid-123"); err != nil {
		t.Fatalf("InsertAPIKey: %v", err)
	}

	h := &Handler{db: db, configKeys: map[string]bool{}}

	// 1. 命中
	row, ok, resolveErr := h.resolveAPIKey("sk-valid-123")
	if !ok || resolveErr != nil || row == nil {
		t.Fatalf("valid key: row=%v ok=%v err=%v, want hit", row, ok, resolveErr)
	}

	// 2. 查无此 key → (nil,false,nil)，中间件据此回 401
	row, ok, resolveErr = h.resolveAPIKey("sk-does-not-exist")
	if ok || resolveErr != nil || row != nil {
		t.Fatalf("missing key: row=%v ok=%v err=%v, want (nil,false,nil)", row, ok, resolveErr)
	}

	// 3. DB 故障(关闭连接后查询出错) → err 非 nil，中间件据此回 503
	if err := db.Close(); err != nil {
		t.Fatalf("db.Close: %v", err)
	}
	row, ok, resolveErr = h.resolveAPIKey("sk-valid-123")
	if ok || row != nil {
		t.Fatalf("db failure: row=%v ok=%v, want not-ok", row, ok)
	}
	if resolveErr == nil {
		t.Fatal("db failure must return a non-nil error so middleware answers 503, not 401")
	}
}

// body-signal compact:中转账号池收到带 compaction_trigger 的普通 /responses
// 请求时,必须整体提升到 compact 专用链路(上游命中 /v1/responses/compact),
// 否则中转上游返回非压缩响应,客户端报 expected exactly one compaction output item。
func TestResponses_BodySignalCompactPromotedOnRelayOnlyPool(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var seenPath string
	var seenBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"resp_compaction_test",
			"object":"response.compaction",
			"created_at":1710000000,
			"model":"gpt-4.1-direct",
			"output":[{"type":"compaction_summary","summary":"user likes blue"}],
			"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}
		}`))
	}))
	defer upstream.Close()

	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:      2,
		MaxRetries:          0,
		MaxRateLimitRetries: 0,
	})
	store.AddAccount(&auth.Account{
		DBID:         1,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      upstream.URL,
		APIKey:       "sk-direct",
		Models:       []string{"gpt-4.1-direct"},
		PlanType:     "api",
	})
	handler := NewHandler(store, nil, nil, nil)

	body := []byte(`{
		"model":"gpt-4.1-direct",
		"stream":true,
		"input":[
			{"role":"user","content":"my favorite color is blue"},
			{"type":"compaction_trigger"}
		]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = req

	handler.Responses(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if seenPath != "/v1/responses/compact" {
		t.Fatalf("upstream path = %q, want /v1/responses/compact (body-signal must be promoted)", seenPath)
	}
	if gjson.GetBytes(seenBody, "stream").Exists() {
		t.Fatalf("promoted upstream body should not carry stream, got %s", seenBody)
	}
	if id := gjson.GetBytes(recorder.Body.Bytes(), "id").String(); id != "resp_compaction_test" {
		t.Fatalf("response id = %q, want resp_compaction_test; body=%s", id, recorder.Body.String())
	}
}

// 不带 compaction_trigger 的普通请求不受提升逻辑影响,仍走 /v1/responses。
func TestResponses_PlainRequestNotPromotedOnRelayOnlyPool(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var seenPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"resp_plain_test",
			"object":"response",
			"created_at":1710000000,
			"model":"gpt-4.1-direct",
			"output":[],
			"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}
		}`))
	}))
	defer upstream.Close()

	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:      2,
		MaxRetries:          0,
		MaxRateLimitRetries: 0,
	})
	store.AddAccount(&auth.Account{
		DBID:         1,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      upstream.URL,
		APIKey:       "sk-direct",
		Models:       []string{"gpt-4.1-direct"},
		PlanType:     "api",
	})
	handler := NewHandler(store, nil, nil, nil)

	body := []byte(`{"model":"gpt-4.1-direct","input":"hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = req

	handler.Responses(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	if seenPath != "/v1/responses" {
		t.Fatalf("upstream path = %q, want /v1/responses (no promotion for plain request)", seenPath)
	}
}

// 池中还有可用官方账号时,body-signal 请求被钉在官方账号上(不落中转)。
func TestBodySignalCompactFilters(t *testing.T) {
	relay := &auth.Account{
		DBID:         1,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      "https://relay.example.com",
		APIKey:       "sk-relay",
	}
	codex := &auth.Account{DBID: 2, AccessToken: "at-codex"}

	filter := excludeRelayAccountsFilter(nil)
	if filter(relay) {
		t.Fatal("relay account must be excluded by pinned filter")
	}
	if !filter(codex) {
		t.Fatal("codex account must pass pinned filter")
	}

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	handler := NewHandler(store, nil, nil, nil)
	if handler.storeHasAvailableCodexAccount() {
		t.Fatal("empty pool should report no codex account")
	}
	store.AddAccount(relay)
	if handler.storeHasAvailableCodexAccount() {
		t.Fatal("relay-only pool should report no codex account")
	}
	store.AddAccount(codex)
	if !handler.storeHasAvailableCodexAccount() {
		t.Fatal("pool with codex account should report available")
	}
}
