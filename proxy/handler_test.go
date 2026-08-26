package proxy

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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

type dataThenErrorReadCloser struct {
	data []byte
	err  error
	sent bool
}

func (r *dataThenErrorReadCloser) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, r.data), nil
	}
	return 0, r.err
}

func (r *dataThenErrorReadCloser) Close() error {
	return nil
}

func TestSupportedModelsIncludeLatestRequestedModels(t *testing.T) {
	for _, model := range []string{"gpt-5.5", "gpt-5.3-codex-spark", "gpt-image-2", "gpt-image-2-2k", "gpt-image-2-4k"} {
		if !slices.Contains(SupportedModels, model) {
			t.Fatalf("SupportedModels missing %q", model)
		}
	}
}

func TestSupportedModelsExcludeBelowGPT52(t *testing.T) {
	// 5.3 只保留 spark；gpt-5.3-codex、gpt-5.2 及更低模型已下线。
	for _, model := range []string{
		"gpt-5", "gpt-5-codex", "gpt-5-codex-mini",
		"gpt-5.1", "gpt-5.1-codex", "gpt-5.1-codex-mini", "gpt-5.1-codex-max",
		"gpt-5.2", "gpt-5.2-codex", "gpt-5.3-codex",
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
	for _, model := range []string{"gpt-5.5", "gpt-5.3-codex-spark", "gpt-image-2"} {
		if !slices.Contains(ids, model) {
			t.Fatalf("/v1/models missing %q in %v", model, ids)
		}
	}
	for _, model := range []string{"gpt-image-2-2k", "gpt-image-2-4k"} {
		if !slices.Contains(ids, model) {
			t.Fatalf("/v1/models missing image alias %q in %v", model, ids)
		}
	}

	for _, model := range []string{"gpt-5", "gpt-5.1", "gpt-5.2", "gpt-5.2-codex", "gpt-5.3-codex"} {
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
		"/v1/live",
		"/live",
		"/backend-api/codex/realtime/calls",
	} {
		if !postRoutes[path] {
			t.Fatalf("expected POST route %s to be registered; routes=%v", path, postRoutes)
		}
	}
	for _, path := range []string{
		"/v1/responses",
		"/v1/realtime",
		"/v1/live/:call_id",
		"/responses",
		"/realtime",
		"/live/:call_id",
		"/backend-api/codex/responses",
		"/backend-api/codex/:call_id",
	} {
		if !getRoutes[path] {
			t.Fatalf("expected GET route %s to be registered; routes=%v", path, getRoutes)
		}
	}
}

func TestRealtimeWebSocketTranslatesTextConversationToResponses(t *testing.T) {
	gin.SetMode(gin.TestMode)

	previousExec := WebsocketExecuteFunc
	t.Cleanup(func() { WebsocketExecuteFunc = previousExec })

	bodyCh := make(chan []byte, 2)
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		bodyCh <- append([]byte(nil), requestBody...)
		sse := `data: {"type":"response.output_text.delta","delta":"hi"}` + "\n\n" +
			`data: {"type":"response.completed","response":{"id":"resp_realtime_1","output":[{"id":"msg_realtime_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"hi"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(sse))}, nil
	}

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "at", PlanType: "plus", AccountID: "acct-realtime"})
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	router := gin.New()
	handler.RegisterRoutes(router)
	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/realtime?model=gpt-5.4"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		if resp != nil {
			t.Fatalf("dial realtime websocket failed: %v status=%d", err, resp.StatusCode)
		}
		t.Fatalf("dial realtime websocket failed: %v", err)
	}
	defer conn.Close()
	if resp == nil || resp.Header.Get(newAPIPolicyWebSocketCapabilityHeader) != newAPIPolicyWebSocketCapabilityV1 {
		t.Fatalf("missing websocket policy event-id capability: response=%v", resp)
	}
	_, created, err := conn.ReadMessage()
	if err != nil || gjson.GetBytes(created, "type").String() != "session.created" {
		t.Fatalf("session created event = %s err=%v", created, err)
	}

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"session.update","session":{"instructions":"Answer briefly."}}`)); err != nil {
		t.Fatal(err)
	}
	_, ack, err := conn.ReadMessage()
	if err != nil || gjson.GetBytes(ack, "type").String() != "session.updated" {
		t.Fatalf("session ack = %s err=%v", ack, err)
	}

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"conversation.item.create","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}}`)); err != nil {
		t.Fatal(err)
	}
	_, ack, err = conn.ReadMessage()
	if err != nil || gjson.GetBytes(ack, "type").String() != "conversation.item.created" {
		t.Fatalf("item ack = %s err=%v", ack, err)
	}

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","response":{"max_output_tokens":64},"__newapi_policy_event_id":"client-controlled"}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case gotBody := <-bodyCh:
		// 顶层 type 信封在 prepare 阶段剥离（HTTP 上游不接受，issue #548）；
		// WS 出站帧的 type 由 wsrelay 的 prepareWebsocketBody 统一重设，不在此边界。
		if gjson.GetBytes(gotBody, "type").Exists() {
			t.Fatalf("upstream body should not carry envelope type: %s", gotBody)
		}
		if got := gjson.GetBytes(gotBody, "model").String(); got != "gpt-5.4" {
			t.Fatalf("upstream model = %q body=%s", got, gotBody)
		}
		if got := gjson.GetBytes(gotBody, "instructions").String(); got != "Answer briefly." {
			t.Fatalf("upstream instructions = %q body=%s", got, gotBody)
		}
		if got := gjson.GetBytes(gotBody, "input.0.content.0.text").String(); got != "hello" {
			t.Fatalf("upstream input text = %q body=%s", got, gotBody)
		}
		if gjson.GetBytes(gotBody, newAPIPolicyWebSocketEventField).Exists() {
			t.Fatalf("reserved policy event id leaked upstream: %s", gotBody)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for translated realtime request")
	}

	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	_, event, err := conn.ReadMessage()
	if err != nil || gjson.GetBytes(event, "type").String() != "response.output_text.delta" {
		t.Fatalf("first response event = %s err=%v", event, err)
	}
	_, event, err = conn.ReadMessage()
	if err != nil || gjson.GetBytes(event, "type").String() != "response.done" {
		t.Fatalf("realtime terminal event = %s err=%v", event, err)
	}
	if got := gjson.GetBytes(event, "response.usage.total_tokens").Int(); got != 2 {
		t.Fatalf("realtime terminal usage = %d body=%s", got, event)
	}

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"conversation.item.create","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"again"}]}}`)); err != nil {
		t.Fatal(err)
	}
	_, ack, err = conn.ReadMessage()
	if err != nil || gjson.GetBytes(ack, "type").String() != "conversation.item.created" {
		t.Fatalf("second item ack = %s err=%v", ack, err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","response":{"max_output_tokens":64}}`)); err != nil {
		t.Fatal(err)
	}
	select {
	case gotBody := <-bodyCh:
		if gjson.GetBytes(gotBody, "previous_response_id").Exists() {
			t.Fatalf("realtime history must not depend on upstream previous_response_id: %s", gotBody)
		}
		if count := gjson.GetBytes(gotBody, "input.#").Int(); count != 3 {
			t.Fatalf("second realtime history count = %d body=%s", count, gotBody)
		}
		if got := gjson.GetBytes(gotBody, "input.0.content.0.text").String(); got != "hello" {
			t.Fatalf("first user history = %q body=%s", got, gotBody)
		}
		if got := gjson.GetBytes(gotBody, "input.1.content.0.text").String(); got != "hi" {
			t.Fatalf("assistant history = %q body=%s", got, gotBody)
		}
		if gjson.GetBytes(gotBody, "input.1.id").Exists() || gjson.GetBytes(gotBody, "input.1.status").Exists() {
			t.Fatalf("assistant history retained server-owned fields: %s", gotBody)
		}
		if got := gjson.GetBytes(gotBody, "input.2.content.0.text").String(); got != "again" {
			t.Fatalf("second realtime input = %q body=%s", got, gotBody)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for second realtime request")
	}
}

func TestRealtimeTextHistoryIsBoundedAndDropsServerOwnedFields(t *testing.T) {
	state := &realtimeTextSession{}
	for i := 0; i < realtimeTextHistoryMaxItems+10; i++ {
		state.appendHistory(json.RawMessage(fmt.Sprintf(`{"id":"msg_%d","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"item-%d"}]}`, i, i)))
	}
	if len(state.History) != realtimeTextHistoryMaxItems {
		t.Fatalf("history items = %d, want %d", len(state.History), realtimeTextHistoryMaxItems)
	}
	if state.historyBytes <= 0 || state.historyBytes > realtimeTextHistoryMaxBytes {
		t.Fatalf("history bytes = %d", state.historyBytes)
	}
	if gjson.GetBytes(state.History[0], "id").Exists() || gjson.GetBytes(state.History[0], "status").Exists() {
		t.Fatalf("server-owned fields remained: %s", state.History[0])
	}
	if got := gjson.GetBytes(state.History[0], "content.0.text").String(); got != "item-10" {
		t.Fatalf("oldest retained item = %q, want item-10", got)
	}
}

func TestRealtimeResponsesClientEventOnlyRenamesTerminalEvent(t *testing.T) {
	completed := []byte(`{"type":"response.completed","response":{"id":"resp_1","usage":{"total_tokens":3}}}`)
	got := realtimeResponsesClientEvent(completed)
	if eventType := gjson.GetBytes(got, "type").String(); eventType != "response.done" {
		t.Fatalf("terminal type = %q body=%s", eventType, got)
	}
	if id := gjson.GetBytes(got, "response.id").String(); id != "resp_1" {
		t.Fatalf("response id = %q body=%s", id, got)
	}
	if usage := gjson.GetBytes(got, "response.usage.total_tokens").Int(); usage != 3 {
		t.Fatalf("usage = %d body=%s", usage, got)
	}

	delta := []byte(`{"type":"response.output_text.delta","delta":"hi"}`)
	if transformed := realtimeResponsesClientEvent(delta); string(transformed) != string(delta) {
		t.Fatalf("non-terminal event changed: %s", transformed)
	}
}

func TestNormalizeRealtimeTextClientEventRejectsAudioWithoutClosingTextSession(t *testing.T) {
	state := &realtimeTextSession{Model: "gpt-5.4"}
	ack, forward, apiErr := normalizeRealtimeTextClientEvent(state, []byte(`{"type":"input_audio_buffer.append","audio":"AAAA"}`))
	if apiErr == nil || !strings.Contains(apiErr.Message, "text events only") {
		t.Fatalf("audio event error = %#v", apiErr)
	}
	if len(ack) != 0 || len(forward) != 0 {
		t.Fatalf("audio event produced ack=%s forward=%s", ack, forward)
	}
	if state.Model != "gpt-5.4" {
		t.Fatalf("text session state was mutated: %+v", state)
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

	t.Run("preserves v2 prewarm fields", func(t *testing.T) {
		got, model, apiErr := normalizeResponsesWebSocketClientPayload([]byte(`{"type":"response.create","model":"gpt-5.6-sol","input":[],"generate":false,"client_metadata":{"x-codex-turn-state":"turn_123"}}`))
		if apiErr != nil {
			t.Fatalf("unexpected error: %v", apiErr)
		}
		if model != "gpt-5.6-sol" || gjson.GetBytes(got, "generate").Bool() {
			t.Fatalf("v2 prewarm changed: model=%q body=%s", model, got)
		}
		if turnState := gjson.GetBytes(got, "client_metadata.x-codex-turn-state").String(); turnState != "turn_123" {
			t.Fatalf("turn state = %q, want turn_123; body=%s", turnState, got)
		}
		prepared, _ := PrepareResponsesWebSocketBody(got)
		if generate := gjson.GetBytes(prepared, "generate"); !generate.Exists() || generate.Bool() {
			t.Fatalf("prepared v2 prewarm lost generate=false: %s", prepared)
		}
		if turnState := gjson.GetBytes(prepared, "client_metadata.x-codex-turn-state").String(); turnState != "turn_123" {
			t.Fatalf("prepared turn state = %q, want turn_123; body=%s", turnState, prepared)
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
	store.SetCodexModelMapping(`{"client-ws-alias":"gpt-5.4"}`)
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

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"model":"client-ws-alias","previous_response_id":"resp_prev","input":"hello"}`)); err != nil {
		t.Fatalf("write request: %v", err)
	}
	select {
	case gotBody := <-bodyCh:
		// 顶层 type 信封在 prepare 阶段剥离（HTTP 上游不接受，issue #548）；
		// WS 出站帧的 type 由 wsrelay 的 prepareWebsocketBody 统一重设，不在此边界。
		if gjson.GetBytes(gotBody, "type").Exists() {
			t.Fatalf("upstream body should not carry envelope type: %s", gotBody)
		}
		if model := gjson.GetBytes(gotBody, "model").String(); model != "gpt-5.4" {
			t.Fatalf("upstream model = %q, want mapped gpt-5.4; body=%s", model, gotBody)
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

// TestResponsesWebSocket1009FallbackStripsEnvelopeType 覆盖 issue #548：native WS
// ingress 收到上游 close 1009 后降级 HTTP，fallback 请求体必须剥掉 WS 事件信封的
// 顶层 type（HTTP 上游会 400 Unsupported parameter: type），嵌套 type 保持完整；
// 同时 1009 体积喂给体积路由，后续同尺寸请求直接首发 HTTP 不再进 WS。
func TestResponsesWebSocket1009FallbackStripsEnvelopeType(t *testing.T) {
	gin.SetMode(gin.TestMode)

	previousExec := WebsocketExecuteFunc
	previousResin := resinCfg.Load()
	t.Cleanup(func() {
		WebsocketExecuteFunc = previousExec
		resinCfg.Store(previousResin)
		globalWSSizeRouter = websocketSizeRouter{}
	})
	globalWSSizeRouter = websocketSizeRouter{}

	var wsCalls int32
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		atomic.AddInt32(&wsCalls, 1)
		return nil, errors.New("websocket send error: websocket: close 1009 (message too big)")
	}

	httpBodyCh := make(chan []byte, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		httpBodyCh <- body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			`data: {"type":"response.output_text.delta","delta":"fallback"}` + "\n\n" +
				`data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n",
		))
	}))
	defer upstream.Close()
	SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "test"})

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

	// 大输入让 1009 时的体积达到体积路由的学习样本下限（64KB）。
	bigText := strings.Repeat("issue548 ", 8192)
	payload := fmt.Sprintf(`{"type":"response.create","model":"gpt-5.4","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"%s"}]}]}`, bigText)

	readTurnEvents := func(turn string) {
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, first, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("%s: read first event: %v", turn, err)
		}
		if eventType := gjson.GetBytes(first, "type").String(); eventType != "response.output_text.delta" {
			t.Fatalf("%s: first event type = %q body=%s", turn, eventType, first)
		}
		_, second, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("%s: read terminal event: %v", turn, err)
		}
		if eventType := gjson.GetBytes(second, "type").String(); eventType != "response.completed" {
			t.Fatalf("%s: terminal event type = %q body=%s", turn, eventType, second)
		}
	}

	if err := conn.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
		t.Fatalf("write request: %v", err)
	}
	select {
	case gotBody := <-httpBodyCh:
		if gjson.GetBytes(gotBody, "type").Exists() {
			t.Fatalf("HTTP fallback body should not carry envelope type: %.300s", gotBody)
		}
		if it := gjson.GetBytes(gotBody, "input.0.type").String(); it != "message" {
			t.Fatalf("nested input type = %q, want message", it)
		}
		if ct := gjson.GetBytes(gotBody, "input.0.content.0.type").String(); ct != "input_text" {
			t.Fatalf("nested content type = %q, want input_text", ct)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for HTTP fallback request")
	}
	if got := atomic.LoadInt32(&wsCalls); got != 1 {
		t.Fatalf("wsCalls = %d, want 1 (WS 尝试一次后降级 HTTP)", got)
	}
	readTurnEvents("turn-1")

	// 第二帧同体积：体积路由已从 1009 学习阈值，应直接首发 HTTP，WS 调用数不变。
	if err := conn.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
		t.Fatalf("write second request: %v", err)
	}
	select {
	case gotBody := <-httpBodyCh:
		if gjson.GetBytes(gotBody, "type").Exists() {
			t.Fatalf("size-routed HTTP body should not carry envelope type: %.300s", gotBody)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for size-routed HTTP request")
	}
	if got := atomic.LoadInt32(&wsCalls); got != 1 {
		t.Fatalf("wsCalls = %d, want 1 (体积路由应跳过第二次 WS 尝试)", got)
	}
	readTurnEvents("turn-2")
}

// TestResponsesWebSocket1009FallbackExpandsPreviousResponseFromCache 覆盖 issue #548
// 的续链缺口：HTTP fallback 前若本地响应缓存命中 previous_response_id，历史应展开进
// input[] 并剥离续链 id，避免 executor 出站删除该字段时静默丢失上游会话历史。
func TestResponsesWebSocket1009FallbackExpandsPreviousResponseFromCache(t *testing.T) {
	gin.SetMode(gin.TestMode)

	previousExec := WebsocketExecuteFunc
	previousResin := resinCfg.Load()
	t.Cleanup(func() {
		WebsocketExecuteFunc = previousExec
		resinCfg.Store(previousResin)
		globalWSSizeRouter = websocketSizeRouter{}
	})
	globalWSSizeRouter = websocketSizeRouter{}

	// 匿名请求的缓存归属是 "anon"；历史含 function_call 才会入缓存。
	cacheCompletedResponse("anon",
		[]byte(`[{"type":"message","role":"user","content":[{"type":"input_text","text":"earlier"}]}]`),
		[]byte(`{"type":"response.completed","response":{"id":"resp_issue548","output":[{"type":"function_call","call_id":"call_548","name":"lookup","arguments":"{}"}]}}`))

	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		return nil, errors.New("websocket send error: websocket: close 1009 (message too big)")
	}

	httpBodyCh := make(chan []byte, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		httpBodyCh <- body
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"))
	}))
	defer upstream.Close()
	SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "test"})

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

	payload := `{"type":"response.create","model":"gpt-5.4","previous_response_id":"resp_issue548","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}]}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
		t.Fatalf("write request: %v", err)
	}

	select {
	case gotBody := <-httpBodyCh:
		if prev := gjson.GetBytes(gotBody, "previous_response_id"); prev.Exists() {
			t.Fatalf("previous_response_id should be expanded away before HTTP fallback: %s", gotBody)
		}
		if gjson.GetBytes(gotBody, "type").Exists() {
			t.Fatalf("HTTP fallback body should not carry envelope type: %s", gotBody)
		}
		input := gjson.GetBytes(gotBody, "input")
		if !input.IsArray() {
			t.Fatalf("input should be an array: %s", gotBody)
		}
		foundHistory := false
		foundCurrent := false
		input.ForEach(func(_, item gjson.Result) bool {
			if item.Get("call_id").String() == "call_548" {
				foundHistory = true
			}
			if strings.Contains(item.Get("content.0.text").String(), "continue") {
				foundCurrent = true
			}
			return true
		})
		if !foundHistory {
			t.Fatalf("cached history (call_548) missing from expanded input: %s", gotBody)
		}
		if !foundCurrent {
			t.Fatalf("current turn message missing from expanded input: %s", gotBody)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for HTTP fallback request")
	}

	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, event, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read terminal event: %v", err)
	}
	if eventType := gjson.GetBytes(event, "type").String(); eventType != "response.completed" {
		t.Fatalf("terminal event type = %q body=%s", eventType, event)
	}
}

func TestResponsesWebSocketContinuationKeepsBoundAccountPastBoundedLimit(t *testing.T) {
	gin.SetMode(gin.TestMode)

	previousExec := WebsocketExecuteFunc
	t.Cleanup(func() {
		WebsocketExecuteFunc = previousExec
	})

	var store *auth.Store
	var mu sync.Mutex
	responseOwners := make(map[string]int64)
	responseSequence := 0
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()

		previousResponseID := gjson.GetBytes(requestBody, "previous_response_id").String()
		if ownerID := responseOwners[previousResponseID]; previousResponseID != "" && ownerID != account.ID() {
			body := fmt.Sprintf(`{"error":{"type":"invalid_request_error","code":"previous_response_not_found","message":"Previous response with id '%s' not found."}}`, previousResponseID)
			return &http.Response{
				StatusCode: http.StatusBadRequest,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(body)),
			}, nil
		}

		responseSequence++
		responseID := fmt.Sprintf("resp_%d", responseSequence)
		responseOwners[responseID] = account.ID()
		if responseSequence == 1 {
			priority := int64(20)
			if !store.ApplyAccountSchedulerPriority(2, &priority) {
				t.Fatal("failed to raise fallback account scheduler priority")
			}
		}
		sse := fmt.Sprintf(`data: {"type":"response.completed","response":{"id":"%s","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n", responseID)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(sse)),
		}, nil
	}

	store = auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	primary := &auth.Account{DBID: 1, AccessToken: "at-1", PlanType: "plus", AccountID: "acct-1"}
	primary.SetSchedulerPriority(10)
	store.AddAccount(primary)
	store.AddAccount(&auth.Account{DBID: 2, AccessToken: "at-2", PlanType: "plus", AccountID: "acct-2"})
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

	previousResponseID := ""
	for turn := 1; turn <= 52; turn++ {
		payload := fmt.Sprintf(`{"type":"response.create","model":"gpt-5.4","prompt_cache_key":"conversation-1","input":"turn-%d"}`, turn)
		if previousResponseID != "" {
			payload = fmt.Sprintf(`{"type":"response.create","model":"gpt-5.4","prompt_cache_key":"conversation-1","previous_response_id":"%s","input":"turn-%d","client_metadata":{"x-codex-turn-state":"turn-state-1"}}`, previousResponseID, turn)
		}
		if err := conn.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
			t.Fatalf("turn %d write request: %v", turn, err)
		}

		_ = conn.SetReadDeadline(time.Now().Add(time.Second))
		_, event, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("turn %d read event: %v", turn, err)
		}
		if eventType := gjson.GetBytes(event, "type").String(); eventType != "response.completed" {
			t.Fatalf("turn %d event type = %q, want response.completed; body=%s", turn, eventType, event)
		}
		previousResponseID = gjson.GetBytes(event, "response.id").String()
		if previousResponseID == "" {
			t.Fatalf("turn %d response.id missing: %s", turn, event)
		}
	}
}

// 上游认不出续链 id 时（换号、上游未落库、跨实例等），网关应剥离
// previous_response_id 降级重试一次，而不是把 400 甩给客户端（issue #400）。
func TestResponsesWebSocketContinuationDegradesWhenUpstreamRejectsPreviousResponse(t *testing.T) {
	previousResponseNotFoundBody := `{"error":{"type":"invalid_request_error","code":"previous_response_not_found","message":"Previous response with id 'resp_stale' not found."}}`
	completedSSE := "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_new\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"

	cases := []struct {
		name     string
		rejected func() *http.Response
	}{
		{
			name: "http status rejection",
			rejected: func() *http.Response {
				return &http.Response{
					StatusCode: http.StatusBadRequest,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(previousResponseNotFoundBody)),
				}
			},
		},
		{
			// WS 上游把同一个错误发成流内先导 error 帧 + response.failed。
			name: "in-stream response.failed",
			rejected: func() *http.Response {
				sse := "data: {\"type\":\"error\",\"code\":\"previous_response_not_found\",\"message\":\"Previous response with id 'resp_stale' not found.\"}\n\n" +
					"data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"type\":\"invalid_request_error\",\"code\":\"previous_response_not_found\",\"message\":\"Previous response with id 'resp_stale' not found.\"}}}\n\n"
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(sse)),
				}
			},
		},
		{
			// 真实 ChatGPT WS 几乎总会先推 rate_limits / metadata。本机 2004 还开了
			// loose + preflight passthrough，这两帧会先落到客户端。降级不能被它们挡住。
			name: "in-stream response.failed after preflight",
			rejected: func() *http.Response {
				sse := "data: {\"type\":\"codex.rate_limits\",\"plan_type\":\"plus\"}\n\n" +
					"data: {\"type\":\"codex.response.metadata\",\"headers\":{\"x-codex-turn-state\":\"turn\"}}\n\n" +
					"data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"type\":\"invalid_request_error\",\"code\":\"previous_response_not_found\",\"message\":\"Previous response with id 'resp_stale' not found.\"}}}\n\n"
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(sse)),
				}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gin.SetMode(gin.TestMode)
			if tc.name == "in-stream response.failed after preflight" {
				prev := CurrentRuntimeSettings()
				next := prev
				next.FirstTokenMode = FirstTokenModeLoose
				next.CodexPreflightSSEPassthrough = true
				ApplyRuntimeSettings(next)
				t.Cleanup(func() { ApplyRuntimeSettings(prev) })
			}

			previousExec := WebsocketExecuteFunc
			t.Cleanup(func() {
				WebsocketExecuteFunc = previousExec
			})

			var mu sync.Mutex
			attempts := 0
			retriedWithoutContinuation := false
			WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
				mu.Lock()
				defer mu.Unlock()
				attempts++
				if gjson.GetBytes(requestBody, "previous_response_id").String() != "" {
					return tc.rejected(), nil
				}
				retriedWithoutContinuation = true
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(completedSSE)),
				}, nil
			}

			store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
			store.AddAccount(&auth.Account{DBID: 1, AccessToken: "at-1", PlanType: "plus", AccountID: "acct-1"})
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

			payload := `{"type":"response.create","model":"gpt-5.4","prompt_cache_key":"conversation-1","previous_response_id":"resp_stale","input":"continue"}`
			if err := conn.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
				t.Fatalf("write request: %v", err)
			}

			_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			event := readResponsesWSTerminalEvent(t, conn)
			if eventType := gjson.GetBytes(event, "type").String(); eventType != "response.completed" {
				t.Fatalf("event type = %q, want response.completed; body=%s", eventType, event)
			}

			mu.Lock()
			defer mu.Unlock()
			if !retriedWithoutContinuation {
				t.Fatal("gateway never retried without previous_response_id")
			}
			if attempts != 2 {
				t.Fatalf("upstream attempts = %d, want 2 (rejected + degraded retry)", attempts)
			}
		})
	}
}

// 绑定账号被本次请求硬排除（限流等）后，续链请求不应死等它 30 秒再整轮失败：
// 剥离 previous_response_id 后换号继续。
func TestResponsesWebSocketContinuationDegradesWhenBoundAccountExcluded(t *testing.T) {
	gin.SetMode(gin.TestMode)

	previousExec := WebsocketExecuteFunc
	t.Cleanup(func() {
		WebsocketExecuteFunc = previousExec
	})

	var mu sync.Mutex
	var servedAccounts []int64
	var strippedOnAccount int64
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		mu.Lock()
		defer mu.Unlock()
		servedAccounts = append(servedAccounts, account.ID())
		if gjson.GetBytes(requestBody, "previous_response_id").String() != "" {
			// 绑定账号（1 号）被限流：硬排除后本次请求不会再选到它。
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"error":{"type":"rate_limit_error","message":"slow down"}}`)),
			}, nil
		}
		strippedOnAccount = account.ID()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_new\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")),
		}, nil
	}

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	primary := &auth.Account{DBID: 1, AccessToken: "at-1", PlanType: "plus", AccountID: "acct-1"}
	primary.SetSchedulerPriority(10)
	store.AddAccount(primary)
	store.AddAccount(&auth.Account{DBID: 2, AccessToken: "at-2", PlanType: "plus", AccountID: "acct-2"})
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

	payload := `{"type":"response.create","model":"gpt-5.4","prompt_cache_key":"conversation-1","previous_response_id":"resp_stale","input":"continue"}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(payload)); err != nil {
		t.Fatalf("write request: %v", err)
	}

	// 降级必须在等待窗口（30s）之前发生，否则这一轮会超时。
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	_, event, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read event: %v", err)
	}
	if eventType := gjson.GetBytes(event, "type").String(); eventType != "response.completed" {
		t.Fatalf("event type = %q, want response.completed; body=%s", eventType, event)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(servedAccounts) != 2 {
		t.Fatalf("upstream attempts = %v, want 2 (rate-limited bound account + degraded retry)", servedAccounts)
	}
	if strippedOnAccount == 0 || strippedOnAccount == servedAccounts[0] {
		t.Fatalf("degraded retry account = %d, want a different account than %d", strippedOnAccount, servedAccounts[0])
	}
}

func TestResponsesWebSocketSuccessPreservesNewerUsageLimitCooldown(t *testing.T) {
	gin.SetMode(gin.TestMode)

	previousExec := WebsocketExecuteFunc
	t.Cleanup(func() {
		WebsocketExecuteFunc = previousExec
	})

	started := make(chan struct{}, 1)
	release := make(chan struct{}, 1)
	t.Cleanup(func() {
		select {
		case release <- struct{}{}:
		default:
		}
	})
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		pr, pw := io.Pipe()
		started <- struct{}{}
		go func() {
			<-release
			_, _ = io.WriteString(pw, `data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`+"\n\n")
			_ = pw.Close()
		}()
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: pr}, nil
	}

	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:         1,
		MaxRetries:             0,
		MaxRateLimitRetries:    0,
		IgnoreUsageLimitStatus: true,
	})
	account := &auth.Account{DBID: 1, AccessToken: "at-1", PlanType: "pro", AccountID: "acct-1"}
	store.AddAccount(account)
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
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for upstream request")
	}

	store.MarkCooldown(account, time.Hour, "rate_limited")
	release <- struct{}{}

	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	_, completed, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read terminal event: %v", err)
	}
	if eventType := gjson.GetBytes(completed, "type").String(); eventType != "response.completed" {
		t.Fatalf("terminal event type = %q body=%s", eventType, completed)
	}

	deadline := time.Now().Add(time.Second)
	for atomic.LoadInt64(&account.ActiveRequests) != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got := atomic.LoadInt64(&account.ActiveRequests); got != 0 {
		t.Fatalf("ActiveRequests = %d after completed response, want 0", got)
	}
	if !account.HasActiveCooldown() || account.IsAvailable() {
		t.Fatal("a stale WebSocket success must not clear a newer usage-limit cooldown")
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

func TestEmitResponsesPhaseTimingsSetsHeaderAndSegments(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	base := time.Now().Add(-100 * time.Millisecond)
	emitResponsesPhaseTimings(ctx, "gpt-5.6-sol", 300*1024,
		base, base.Add(10*time.Millisecond), base.Add(30*time.Millisecond), base.Add(70*time.Millisecond))

	header := recorder.Header().Get(responsesPhaseTimingHeader)
	if header == "" {
		t.Fatalf("%s header not set", responsesPhaseTimingHeader)
	}
	for _, segment := range []string{"read=10", "validate=20", "prepare=40", "schedule=", "body_kb=300"} {
		if !strings.Contains(header, segment) {
			t.Fatalf("timing header = %q, missing segment %q", header, segment)
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
	wsAccountIDs := make(chan int64, 4)
	wsLiteMetadata := make(chan string, 4)
	wsNamespaces := make(chan string, 4)
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		wsCalls++
		wsAccountIDs <- account.ID()
		wsLiteMetadata <- gjson.GetBytes(requestBody, "client_metadata.ws_request_header_x_openai_internal_codex_responses_lite").String()
		wsNamespaces <- gjson.GetBytes(requestBody, "input.0.namespace").String()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       errReadCloser{err: errors.New("websocket read error: websocket: close 1009 (message too big)")},
		}, nil
	}

	httpCalls := 0
	httpAccountIDs := make(chan string, 4)
	httpLiteHeaders := make(chan string, 4)
	httpNamespaces := make(chan string, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpCalls++
		httpAccountIDs <- r.Header.Get("X-Resin-Account")
		httpLiteHeaders <- r.Header.Get("X-OpenAI-Internal-Codex-Responses-Lite")
		requestBody, _ := io.ReadAll(r.Body)
		httpNamespaces <- gjson.GetBytes(requestBody, "input.0.namespace").String()
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
	primary := &auth.Account{DBID: 1, AccessToken: "at-1", PlanType: "pro", AccountID: "acct-1"}
	primary.SetDispatchCountLimit(1)
	store.AddAccount(primary)
	secondary := &auth.Account{DBID: 2, AccessToken: "at-2", PlanType: "free", AccountID: "acct-2"}
	store.AddAccount(secondary)
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

	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{
		"model":"gpt-5.6-sol",
		"input":[{"type":"function_call","name":"run","namespace":"code_tools","arguments":"{}","call_id":"call_1"}],
		"client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":"true"}
	}`)); err != nil {
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
	if got := <-wsLiteMetadata; got != "true" {
		t.Fatalf("WS upstream Lite metadata = %q, want true", got)
	}
	if got := <-wsNamespaces; got != "code_tools" {
		t.Fatalf("WS upstream namespace = %q, want code_tools", got)
	}
	if got := <-httpLiteHeaders; got != "true" {
		t.Fatalf("HTTP fallback Lite header = %q, want true", got)
	}
	if got := <-httpNamespaces; got != "code_tools" {
		t.Fatalf("HTTP fallback namespace = %q, want code_tools", got)
	}
	wsAccountID := <-wsAccountIDs
	httpAccountID := <-httpAccountIDs
	if httpAccountID != fmt.Sprint(wsAccountID) {
		t.Fatalf("HTTP fallback account = %q, want same leased WS account %d", httpAccountID, wsAccountID)
	}
	deadline := time.Now().Add(time.Second)
	for atomic.LoadInt64(&primary.ActiveRequests) != 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := atomic.LoadInt64(&primary.ActiveRequests); got != 0 {
		t.Fatalf("primary ActiveRequests after fallback = %d, want 0", got)
	}
	if got := atomic.LoadInt64(&primary.TotalRequests); got != 1 {
		t.Fatalf("primary TotalRequests = %d, want one logical dispatch", got)
	}
	if got := atomic.LoadInt64(&secondary.TotalRequests); got != 0 {
		t.Fatalf("secondary TotalRequests = %d, want no fallback redispatch", got)
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
	nextSettings.RequestIsolationMode = RequestIsolationModePerAPIKey
	ApplyRuntimeSettings(nextSettings)

	wsCalls := 0
	wsAccountIDs := make(chan int64, 4)
	wsLiteMetadata := make(chan string, 4)
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		wsCalls++
		wsAccountIDs <- account.ID()
		wsLiteMetadata <- gjson.GetBytes(requestBody, "client_metadata.ws_request_header_x_openai_internal_codex_responses_lite").String()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       errReadCloser{err: errors.New("websocket read error: websocket: close 1009 (message too big)")},
		}, nil
	}

	httpCalls := 0
	httpAccountIDs := make(chan string, 4)
	httpSessionIDs := make(chan string, 4)
	httpCacheKeys := make(chan string, 4)
	httpLiteHeaders := make(chan string, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpCalls++
		httpAccountIDs <- r.Header.Get("X-Resin-Account")
		httpSessionIDs <- r.Header.Get("Session_id")
		httpLiteHeaders <- r.Header.Get("X-OpenAI-Internal-Codex-Responses-Lite")
		requestBody, _ := io.ReadAll(r.Body)
		httpCacheKeys <- gjson.GetBytes(requestBody, "prompt_cache_key").String()
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
	primary := &auth.Account{DBID: 1, AccessToken: "at-1", PlanType: "pro", AccountID: "acct-1"}
	primary.SetDispatchCountLimit(1)
	store.AddAccount(primary)
	secondary := &auth.Account{DBID: 2, AccessToken: "at-2", PlanType: "free", AccountID: "acct-2"}
	store.AddAccount(secondary)
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)

	body := []byte(`{"model":"gpt-5.6-sol","input":"hello","stream":true}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Codex2API-Affinity-Key", "tenant-user-42")
	req.Header.Set("X-OpenAI-Internal-Codex-Responses-Lite", "true")
	sessionIdentity := resolveRequestSessionIdentity(req.Header, body)
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
	if got := <-wsLiteMetadata; got != "true" {
		t.Fatalf("WS upstream Lite metadata = %q, want true", got)
	}
	if got := <-httpLiteHeaders; got != "true" {
		t.Fatalf("HTTP fallback Lite header = %q, want true", got)
	}
	expectedUpstreamID := resolveUpstreamSessionID(0, sessionIdentity.upstreamSeed, sessionIdentity.explicitUpstreamID, false)
	httpSessionID := <-httpSessionIDs
	httpCacheKey := <-httpCacheKeys
	if httpSessionID != expectedUpstreamID || httpCacheKey != expectedUpstreamID {
		t.Fatalf("HTTP fallback upstream identity = header %q body %q, want header-independent seed %q", httpSessionID, httpCacheKey, expectedUpstreamID)
	}
	if localAffinityID := resolveDownstreamAffinityID(req.Header); httpSessionID == localAffinityID || httpCacheKey == localAffinityID {
		t.Fatalf("local affinity id leaked into HTTP fallback: local=%q header=%q body=%q", localAffinityID, httpSessionID, httpCacheKey)
	}
	wsAccountID := <-wsAccountIDs
	httpAccountID := <-httpAccountIDs
	if httpAccountID != fmt.Sprint(wsAccountID) {
		t.Fatalf("HTTP fallback account = %q, want same leased WS account %d", httpAccountID, wsAccountID)
	}
	if got := atomic.LoadInt64(&primary.ActiveRequests); got != 0 {
		t.Fatalf("primary ActiveRequests after fallback = %d, want 0", got)
	}
	if got := atomic.LoadInt64(&primary.TotalRequests); got != 1 {
		t.Fatalf("primary TotalRequests = %d, want one logical dispatch", got)
	}
	if got := atomic.LoadInt64(&secondary.TotalRequests); got != 0 {
		t.Fatalf("secondary TotalRequests = %d, want no fallback redispatch", got)
	}
}

func TestResponsesHTTPIngressKeepsDownstreamAliveDuringUpstreamSilence(t *testing.T) {
	gin.SetMode(gin.TestMode)

	previousExec := WebsocketExecuteFunc
	previousSettings := CurrentRuntimeSettings()
	previousInterval := downstreamSSEKeepaliveInterval
	t.Cleanup(func() {
		WebsocketExecuteFunc = previousExec
		ApplyRuntimeSettings(previousSettings)
		downstreamSSEKeepaliveInterval = previousInterval
	})

	nextSettings := previousSettings
	nextSettings.CodexForceWebsocket = true
	nextSettings.CodexContinueThinking = false
	ApplyRuntimeSettings(nextSettings)
	downstreamSSEKeepaliveInterval = 5 * time.Millisecond

	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		pr, pw := io.Pipe()
		go func() {
			_, _ = pw.Write([]byte(`data: {"type":"response.output_text.delta","delta":"started"}` + "\n\n"))
			time.Sleep(30 * time.Millisecond)
			_, _ = pw.Write([]byte(`data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n"))
			_ = pw.Close()
		}()
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: pr}, nil
	}

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, TestConcurrency: 1, TestModel: "gpt-5.6-sol"})
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "at-1", PlanType: "pro", AccountID: "acct-1"})
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)

	body := []byte(`{"model":"gpt-5.6-sol","input":"hello","stream":true}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = req

	handler.Responses(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", recorder.Code, recorder.Body.String())
	}
	got := recorder.Body.String()
	for _, want := range []string{`"delta":"started"`, downstreamSSEKeepaliveComment, `"type":"response.completed"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("stream missing %q; body=%q", want, got)
		}
	}
}

func TestResponsesSkipsWebsocketWhenBodyReachesLearnedTooBigThreshold(t *testing.T) {
	gin.SetMode(gin.TestMode)

	previousExec := WebsocketExecuteFunc
	previousSettings := CurrentRuntimeSettings()
	previousResin := resinCfg.Load()
	t.Cleanup(func() {
		WebsocketExecuteFunc = previousExec
		ApplyRuntimeSettings(previousSettings)
		resinCfg.Store(previousResin)
		globalWSSizeRouter = websocketSizeRouter{}
	})
	nextSettings := previousSettings
	nextSettings.CodexForceWebsocket = true
	ApplyRuntimeSettings(nextSettings)

	wsCalls := 0
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		wsCalls++
		return nil, errors.New("websocket send error: websocket: close 1009 (message too big)")
	}

	httpCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpCalls++
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			`data: {"type":"response.output_text.delta","delta":"size-routed"}` + "\n\n" +
				`data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2},"service_tier":"default"}}` + "\n\n",
		))
	}))
	defer upstream.Close()
	SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "test"})

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: 1, AccessToken: "at-1", PlanType: "pro", AccountID: "acct-1"}
	account.SetDispatchCountLimit(1)
	store.AddAccount(account)
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)

	// 预置学习状态:任何体积的请求都视为达到已知 1009 阈值
	globalWSSizeRouter = websocketSizeRouter{minTooBig: 1, learnedAt: time.Now()}

	body := []byte(`{"model":"gpt-5.4","input":"hello","stream":true}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = req

	handler.Responses(ctx)

	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "size-routed") {
		t.Fatalf("status = %d, body = %q", recorder.Code, recorder.Body.String())
	}
	if wsCalls != 0 {
		t.Fatalf("wsCalls = %d, want 0 (体积路由应完全跳过 WS)", wsCalls)
	}
	if httpCalls != 1 {
		t.Fatalf("httpCalls = %d, want 1", httpCalls)
	}
}

func TestResponsesHTTPIngressRetainsAccountWhenWebsocketRequestReturnsMessageTooBig(t *testing.T) {
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
		return nil, errors.New("websocket send error: websocket: close 1009 (message too big)")
	}

	httpCalls := 0
	httpAccountIDs := make(chan string, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpCalls++
		httpAccountIDs <- r.Header.Get("X-Resin-Account")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			`data: {"type":"response.output_text.delta","delta":"http-fallback"}` + "\n\n" +
				`data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2},"service_tier":"default"}}` + "\n\n",
		))
	}))
	defer upstream.Close()
	SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "test"})

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: 1, AccessToken: "at-1", PlanType: "pro", AccountID: "acct-1"}
	account.SetDispatchCountLimit(1)
	store.AddAccount(account)
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)

	body := []byte(`{"model":"gpt-5.4","input":"hello","stream":true}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = req

	handler.Responses(ctx)

	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "http-fallback") {
		t.Fatalf("fallback response = status %d body %s", recorder.Code, recorder.Body.String())
	}
	if wsCalls != 1 || httpCalls != 1 {
		t.Fatalf("upstream calls = WS %d HTTP %d, want 1 each", wsCalls, httpCalls)
	}
	if got := <-httpAccountIDs; got != "1" {
		t.Fatalf("HTTP fallback account = %q, want retained account 1", got)
	}
	if got := atomic.LoadInt64(&account.TotalRequests); got != 1 {
		t.Fatalf("TotalRequests = %d, want one logical dispatch", got)
	}
	if got := atomic.LoadInt64(&account.ActiveRequests); got != 0 {
		t.Fatalf("ActiveRequests after fallback = %d, want 0", got)
	}
}

func TestCompatibilityEndpointsRetainAccountForWebsocketMessageTooBigHTTPFallback(t *testing.T) {
	tests := []struct {
		name       string
		path       string
		body       string
		invoke     func(*Handler, *gin.Context)
		resultPath string
	}{
		{
			name:       "chat completions",
			path:       "/v1/chat/completions",
			body:       `{"model":"gpt-5.4","messages":[{"role":"user","content":"hello"}]}`,
			invoke:     func(handler *Handler, ctx *gin.Context) { handler.ChatCompletions(ctx) },
			resultPath: "choices.0.message.content",
		},
		{
			name:       "anthropic messages",
			path:       "/v1/messages",
			body:       `{"model":"claude-opus-4-6","max_tokens":128,"messages":[{"role":"user","content":"hello"}]}`,
			invoke:     func(handler *Handler, ctx *gin.Context) { handler.Messages(ctx) },
			resultPath: "content.0.text",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
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

			var wsCalls atomic.Int32
			wsAccountIDs := make(chan int64, 2)
			WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
				wsCalls.Add(1)
				wsAccountIDs <- account.ID()
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       errReadCloser{err: errors.New("websocket read error: websocket: close 1009 (message too big)")},
				}, nil
			}

			var httpCalls atomic.Int32
			httpAccountIDs := make(chan string, 2)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				httpCalls.Add(1)
				httpAccountIDs <- r.Header.Get("X-Resin-Account")
				w.Header().Set("Content-Type", "text/event-stream")
				for _, event := range []string{
					`{"type":"response.created","response":{"id":"resp_fallback_test"}}`,
					`{"type":"response.output_item.added","item":{"type":"message"}}`,
					`{"type":"response.output_text.delta","delta":"http-fallback"}`,
					`{"type":"response.output_text.done"}`,
					`{"type":"response.completed","response":{"id":"resp_fallback_test","status":"completed","usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`,
				} {
					_, _ = io.WriteString(w, "data: "+event+"\n\n")
				}
			}))
			t.Cleanup(upstream.Close)
			SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "test"})

			settings := &database.SystemSettings{MaxConcurrency: 1, TestConcurrency: 1, TestModel: "gpt-5.4"}
			store := auth.NewStore(nil, nil, settings)
			t.Cleanup(store.Stop)
			primary := &auth.Account{DBID: 1, AccessToken: "at-1", PlanType: "pro", AccountID: "acct-1"}
			primary.SetDispatchCountLimit(1)
			store.AddAccount(primary)
			secondary := &auth.Account{DBID: 2, AccessToken: "at-2", PlanType: "free", AccountID: "acct-2"}
			store.AddAccount(secondary)
			handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)

			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body))
			ctx.Request.Header.Set("Content-Type", "application/json")

			tc.invoke(handler, ctx)

			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
			}
			if got := gjson.GetBytes(recorder.Body.Bytes(), tc.resultPath).String(); got != "http-fallback" {
				t.Fatalf("fallback result %s = %q, want http-fallback; body=%s", tc.resultPath, got, recorder.Body.String())
			}
			if got := wsCalls.Load(); got != 1 {
				t.Fatalf("WebSocket upstream calls = %d, want 1", got)
			}
			if got := httpCalls.Load(); got != 1 {
				t.Fatalf("HTTP upstream calls = %d, want 1", got)
			}
			wsAccountID := <-wsAccountIDs
			httpAccountID := <-httpAccountIDs
			if httpAccountID != fmt.Sprint(wsAccountID) {
				t.Fatalf("HTTP fallback account = %q, want same leased WS account %d", httpAccountID, wsAccountID)
			}
			if got := atomic.LoadInt64(&primary.ActiveRequests); got != 0 {
				t.Fatalf("primary ActiveRequests after fallback = %d, want 0", got)
			}
			if got := atomic.LoadInt64(&primary.TotalRequests); got != 1 {
				t.Fatalf("primary TotalRequests = %d, want one logical dispatch", got)
			}
			if got := atomic.LoadInt64(&secondary.TotalRequests); got != 0 {
				t.Fatalf("secondary TotalRequests = %d, want no fallback redispatch", got)
			}
		})
	}
}

func TestResponsesHTTPFallbackRetryKeepsCorrelationThroughRelaySuccess(t *testing.T) {
	gin.SetMode(gin.TestMode)

	previousExec := WebsocketExecuteFunc
	previousSettings := CurrentRuntimeSettings()
	previousResin := resinCfg.Load()
	previousLogWriter := log.Writer()
	previousLogFlags := log.Flags()
	var logs bytes.Buffer
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() {
		WebsocketExecuteFunc = previousExec
		ApplyRuntimeSettings(previousSettings)
		resinCfg.Store(previousResin)
		log.SetOutput(previousLogWriter)
		log.SetFlags(previousLogFlags)
	})
	nextSettings := previousSettings
	nextSettings.CodexForceWebsocket = true
	ApplyRuntimeSettings(nextSettings)

	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       errReadCloser{err: errors.New("websocket read error: websocket: close 1009 (message too big)")},
		}, nil
	}

	httpCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpCalls++
		if httpCalls == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"retry me"}}`))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(
			`data: {"type":"response.output_text.delta","delta":"relay-success"}` + "\n\n" +
				`data: {"type":"response.completed","response":{"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}` + "\n\n",
		))
	}))
	defer upstream.Close()
	SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "test"})

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, MaxRetries: 1, TestConcurrency: 1, TestModel: "gpt-5.4"})
	primary := &auth.Account{DBID: 1, AccessToken: "at-1", PlanType: "pro", AccountID: "acct-1"}
	primary.SetDispatchCountLimit(1)
	store.AddAccount(primary)
	store.AddAccount(&auth.Account{
		DBID:         2,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      upstream.URL,
		APIKey:       "sk-relay",
		Models:       []string{"gpt-5.4"},
	})
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)

	body := []byte(`{"model":"gpt-5.4","input":"hello","stream":true}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = req

	handler.Responses(ctx)

	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "relay-success") {
		t.Fatalf("fallback retry response = status %d body %s", recorder.Code, recorder.Body.String())
	}
	if httpCalls != 2 {
		t.Fatalf("HTTP calls = %d, want failed fallback plus successful relay retry", httpCalls)
	}
	completionLines := make([]string, 0, 2)
	for _, line := range strings.Split(logs.String(), "\n") {
		if strings.Contains(line, "HTTP 降级尝试结束") {
			completionLines = append(completionLines, line)
		}
	}
	if len(completionLines) != 2 {
		t.Fatalf("fallback completion log lines = %d, want failed and final attempts; logs=%s", len(completionLines), logs.String())
	}
	if !strings.Contains(completionLines[0], "status=500") || !strings.Contains(completionLines[1], "status=200") {
		t.Fatalf("fallback completion statuses are not failure then success: %v", completionLines)
	}
	firstID := strings.SplitN(strings.SplitN(completionLines[0], "fallback_id=", 2)[1], ",", 2)[0]
	secondID := strings.SplitN(strings.SplitN(completionLines[1], "fallback_id=", 2)[1], ",", 2)[0]
	if firstID == "" || firstID != secondID {
		t.Fatalf("fallback ids differ across HTTP retry: %q vs %q; logs=%v", firstID, secondID, completionLines)
	}
}

func TestResponsesDoesNotFallbackOrPenalizeAfterWebSocketContent(t *testing.T) {
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

	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body: &dataThenErrorReadCloser{
				data: []byte(`data: {"type":"response.output_text.delta","delta":"already-sent"}` + "\n\n"),
				err:  errors.New("websocket read error: websocket: close 1009 (message too big)"),
			},
		}, nil
	}

	httpCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		httpCalls++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()
	SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "test"})

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: 1, AccessToken: "at-1", PlanType: "pro", AccountID: "acct-1"}
	store.AddAccount(account)
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
	if !strings.Contains(recorder.Body.String(), "already-sent") {
		t.Fatalf("downstream content was not preserved: %s", recorder.Body.String())
	}
	if httpCalls != 0 {
		t.Fatalf("HTTP fallback calls = %d, want 0 after downstream content", httpCalls)
	}
	account.Mu().RLock()
	failureStreak := account.FailureStreak
	recentResults := account.RecentResultsCnt
	account.Mu().RUnlock()
	if failureStreak != 0 || recentResults != 0 {
		t.Fatalf("1009 changed account health: FailureStreak=%d RecentResultsCnt=%d", failureStreak, recentResults)
	}
	if got := atomic.LoadInt64(&account.ActiveRequests); got != 0 {
		t.Fatalf("ActiveRequests after non-fallback 1009 = %d, want 0", got)
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

func TestUsageLogAndClientErrorsOmitUnstructuredUpstreamBodies(t *testing.T) {
	body := []byte(`<html><body>secret request-id and internal route</body></html>`)
	if got := usageLogErrorMessage(http.StatusBadGateway, body); got != "HTTP 502" {
		t.Fatalf("usageLogErrorMessage() = %q", got)
	}

	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	(&Handler{}).sendUpstreamError(ctx, http.StatusBadGateway, body)
	if strings.Contains(recorder.Body.String(), "secret request-id") || strings.Contains(recorder.Body.String(), "internal route") {
		t.Fatalf("unstructured upstream body leaked to client: %s", recorder.Body.String())
	}
}

func TestUsageLogFailureMessageKeepsGatewayDiagnostics(t *testing.T) {
	// 断流类诊断是网关自产纯文本，必须保留原文而不是退化成裸状态码（issue #524）。
	diag := "上游流读取失败: stream error: stream ID 277; INTERNAL_ERROR; received from peer [上游帧 147, 下游写阻塞 0ms, 距上游末帧 52484ms]"
	got := usageLogFailureMessage(logStatusUpstreamStreamBreak, diag)
	if !strings.Contains(got, "INTERNAL_ERROR") || !strings.Contains(got, "stream ID 277") {
		t.Fatalf("usageLogFailureMessage() dropped diagnostics: %q", got)
	}

	// 诊断文本包含上游 JSON 错误帧时仍走结构化提取。
	jsonDiag := `{"error":{"code":"rate_limit_exceeded","message":"Too many requests"}}`
	if got := usageLogFailureMessage(http.StatusTooManyRequests, jsonDiag); got != "rate_limit_exceeded · Too many requests" {
		t.Fatalf("usageLogFailureMessage() json path = %q", got)
	}

	// 空文本仍回落裸状态码。
	if got := usageLogFailureMessage(http.StatusBadGateway, "  "); got != "HTTP 502" {
		t.Fatalf("usageLogFailureMessage() empty = %q", got)
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

func TestResponsesEndpointAllowsEncryptedContentInputType(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler := NewHandler(auth.NewStore(nil, nil, nil), nil, nil, nil)
	body := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{"type":"encrypted_content","content":"opaque-ciphertext"},
			{"type":"input_text","text":"hello"}
		]
	}`)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = req

	handler.Responses(ginCtx)

	if recorder.Code == http.StatusBadRequest && strings.Contains(recorder.Body.String(), "invalid_input_type") {
		t.Fatalf("encrypted_content input type was rejected by local validation: %s", recorder.Body.String())
	}
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d after validation passes; body=%s", recorder.Code, http.StatusServiceUnavailable, recorder.Body.String())
	}
	assertNoAvailableAccountResponse(t, recorder.Body.Bytes())
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
	store.SetCodexModelMapping(`{"client-compact-alias":"gpt-4.1-direct","gpt-4.1-direct":"gpt-4.1-second"}`)
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
		"model":"client-compact-alias",
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

func TestResponsesCompactAppliesAccountMappingBeforeSuffixFallback(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var seenBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"resp_compact_mapped",
			"object":"response",
			"model":"gpt-5.5",
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
		Models:       []string{"gpt-5.5"},
		ModelMapping: `{"gpt-5.6-sol-openai-compact":"gpt-5.5"}`,
		PlanType:     "api",
	})
	handler := NewHandler(store, nil, nil, nil)

	body := []byte(`{"model":"gpt-5.6-sol","input":"hello","stream":true}`)
	requestCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(body)).WithContext(requestCtx)
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = req

	handler.ResponsesCompact(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if model := gjson.GetBytes(seenBody, "model").String(); model != "gpt-5.5" {
		t.Fatalf("upstream model = %q, want gpt-5.5; body=%s", model, seenBody)
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

	var upstreamMu sync.Mutex
	var liteHeaders []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/backend-api/codex/responses/compact") {
			t.Fatalf("upstream path = %q, want Resin path ending /backend-api/codex/responses/compact", r.URL.Path)
		}
		upstreamMu.Lock()
		liteHeaders = append(liteHeaders, r.Header.Get(codexResponsesLiteHeader))
		upstreamMu.Unlock()
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
		Models:      []string{"gpt-5.6-sol"},
		PlanType:    "team",
		Status:      auth.StatusReady,
	}
	store.AddAccount(account)
	handler := NewHandler(store, nil, nil, nil)

	body := []byte(`{"model":"gpt-5.6-sol","input":"hello"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(codexResponsesLiteHeader, "true")
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
	upstreamMu.Lock()
	defer upstreamMu.Unlock()
	if len(liteHeaders) == 0 {
		t.Fatal("compact upstream was not called")
	}
	for attempt, got := range liteHeaders {
		if got != "true" {
			t.Fatalf("compact attempt %d Lite header = %q, want true", attempt+1, got)
		}
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

func TestResponsesAcceptsCustomOpenAIResponsesModelID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const customModel = "openrouter/gpt-5:free"
	var seenPath, seenAuth string
	var seenBody []byte
	upstream := newOpenAIResponsesSSEUpstream(&seenPath, &seenAuth, &seenBody)
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
		Models:       []string{customModel},
		PlanType:     "api",
	})
	handler := NewHandler(store, nil, nil, nil)

	body := []byte(`{"model":"openrouter/gpt-5:free","input":"hello","stream":true}`)
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
		t.Fatalf("upstream path = %q, want /v1/responses", seenPath)
	}
	if seenAuth != "Bearer sk-direct" {
		t.Fatalf("Authorization = %q, want Bearer sk-direct", seenAuth)
	}
	if model := gjson.GetBytes(seenBody, "model").String(); model != customModel {
		t.Fatalf("upstream model = %q, want %q; body=%s", model, customModel, seenBody)
	}
	if !strings.Contains(recorder.Body.String(), `"type":"response.completed"`) {
		t.Fatalf("downstream stream missing response.completed; body=%s", recorder.Body.String())
	}
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

	t.Run("durable compaction history item", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Set("raw_body", []byte(`{
			"model":"gpt-5.4",
			"input":[
				{"type":"message","role":"user","content":"hello"},
				{"type":"compaction","encrypted_content":"opaque-history"}
			]
		}`))
		input := &database.UsageLogInput{Endpoint: "/v1/responses"}

		populateCompactUsageMetaFromRequest(ctx, input)

		if input.Compact {
			t.Fatal("Compact = true, want false for durable compaction history item")
		}
		if !input.HasCompactionHistory {
			t.Fatal("HasCompactionHistory = false, want true for durable compaction history item")
		}
	})

	t.Run("durable context compaction history item", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Set("raw_body", []byte(`{
			"model":"gpt-5.4",
			"input":{"type":"context_compaction","id":"cmp_123"}
		}`))
		input := &database.UsageLogInput{Endpoint: "/v1/responses"}

		populateCompactUsageMetaFromRequest(ctx, input)

		if input.Compact {
			t.Fatal("Compact = true, want false for durable context_compaction history item")
		}
		if !input.HasCompactionHistory {
			t.Fatal("HasCompactionHistory = false, want true for durable context_compaction history item")
		}
	})

	t.Run("top level compaction trigger array item", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Set("raw_body", []byte(`{
			"model":"gpt-5.4",
			"input":[
				{"type":"message","role":"user","content":"hello"},
				{"type":"compaction_trigger"}
			]
		}`))
		input := &database.UsageLogInput{Endpoint: "/v1/responses"}

		populateCompactUsageMetaFromRequest(ctx, input)

		if !input.Compact {
			t.Fatal("Compact = false, want true for top-level compaction_trigger array item")
		}
	})

	t.Run("top level compaction trigger object", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Set("raw_body", []byte(`{
			"model":"gpt-5.4",
			"input":{"type":"compaction_trigger"}
		}`))
		input := &database.UsageLogInput{Endpoint: "/v1/responses"}

		populateCompactUsageMetaFromRequest(ctx, input)

		if !input.Compact {
			t.Fatal("Compact = false, want true for top-level compaction_trigger object")
		}
	})

	t.Run("compaction trigger and history coexist", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		ctx.Set("raw_body", []byte(`{
			"model":"gpt-5.6-sol",
			"input":[
				{"type":"compaction_trigger"},
				{"type":"compaction","encrypted_content":"opaque-history"}
			]
		}`))
		input := &database.UsageLogInput{Endpoint: "/v1/responses"}

		populateCompactUsageMetaFromRequest(ctx, input)

		if !input.Compact || !input.HasCompactionHistory {
			t.Fatalf("got Compact=%v HasCompactionHistory=%v, want true/true", input.Compact, input.HasCompactionHistory)
		}
	})

	t.Run("explicit compact endpoint and history coexist", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses/compact", nil)
		ctx.Set("raw_body", []byte(`{
			"model":"gpt-5.6-sol",
			"input":{"type":"context_compaction","id":"cmp_123"}
		}`))
		input := &database.UsageLogInput{
			Endpoint:         "/v1/responses/compact",
			InboundEndpoint:  "/v1/responses/compact",
			UpstreamEndpoint: "/v1/responses/compact",
		}

		populateCompactUsageMetaFromRequest(ctx, input)

		if !input.Compact || !input.HasCompactionHistory {
			t.Fatalf("got Compact=%v HasCompactionHistory=%v, want true/true", input.Compact, input.HasCompactionHistory)
		}
	})

	t.Run("rewritten upstream compact endpoint does not create an inbound signal", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		ctx.Set("raw_body", []byte(`{"model":"gpt-5.6-sol","input":"hello"}`))
		input := &database.UsageLogInput{
			Endpoint:         "/v1/responses",
			InboundEndpoint:  "/v1/responses",
			UpstreamEndpoint: "/v1/responses/compact",
		}

		populateCompactUsageMetaFromRequest(ctx, input)

		if input.Compact || input.HasCompactionHistory {
			t.Fatalf("got Compact=%v HasCompactionHistory=%v, want false/false", input.Compact, input.HasCompactionHistory)
		}
	})

	t.Run("nested compaction trigger in tool output", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Set("raw_body", []byte(`{
			"model":"gpt-5.4",
			"input":[
				{
					"type":"function_call_output",
					"call_id":"call_123",
					"output":{"type":"compaction_trigger","value":"ordinary tool data"}
				}
			]
		}`))
		input := &database.UsageLogInput{Endpoint: "/v1/responses"}

		populateCompactUsageMetaFromRequest(ctx, input)

		if input.Compact {
			t.Fatal("Compact = true, want false for nested compaction_trigger in tool output")
		}
		if input.HasCompactionHistory {
			t.Fatal("HasCompactionHistory = true, want false for nested tool output")
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
		if input.HasCompactionHistory {
			t.Fatal("HasCompactionHistory = true, want false for normal responses input")
		}
	})

	t.Run("metadata only manual compact", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		ctx.Set("raw_body", []byte(`{
			"model":"gpt-5.6-sol",
			"input":[{"type":"message","role":"user","content":"summarize"}],
			"client_metadata":{
				"x-codex-turn-metadata":"{\"request_kind\":\"compaction\",\"thread_source\":\"user\"}"
			}
		}`))
		input := &database.UsageLogInput{Endpoint: "/v1/responses"}

		populateCompactUsageMetaFromRequest(ctx, input)

		if !input.Compact {
			t.Fatal("Compact = false, want true for metadata-only manual compact")
		}
		if input.HasCompactionHistory {
			t.Fatal("HasCompactionHistory = true, want false without a direct history item")
		}
	})

	t.Run("http turn metadata header", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		ctx.Request.Header.Set("X-Codex-Turn-Metadata", `{"request_kind":"compaction","trigger":"manual"}`)
		ctx.Set("raw_body", []byte(`{"model":"gpt-5.6-sol","input":"summarize"}`))
		input := &database.UsageLogInput{Endpoint: "/v1/responses"}

		populateCompactUsageMetaFromRequest(ctx, input)

		if !input.Compact {
			t.Fatal("Compact = false, want true for HTTP turn metadata header")
		}
	})

	t.Run("metadata compaction and history", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		ctx.Set("raw_body", []byte(`{
			"model":"gpt-5.6-sol",
			"input":[{"type":"compaction","encrypted_content":"opaque-history"}],
			"client_metadata":{"x-codex-turn-metadata":"{\"request_kind\":\"compaction\"}"}
		}`))
		input := &database.UsageLogInput{Endpoint: "/v1/responses"}

		populateCompactUsageMetaFromRequest(ctx, input)

		if !input.Compact || !input.HasCompactionHistory {
			t.Fatalf("got Compact=%v HasCompactionHistory=%v, want true/true", input.Compact, input.HasCompactionHistory)
		}
	})

	t.Run("malformed and unknown metadata fail closed", func(t *testing.T) {
		tests := []struct {
			name string
			body string
		}{
			{
				name: "malformed JSON string",
				body: `{"model":"gpt-5.6-sol","input":"hello","client_metadata":{"x-codex-turn-metadata":"{"}}`,
			},
			{
				name: "non string metadata",
				body: `{"model":"gpt-5.6-sol","input":"hello","client_metadata":{"x-codex-turn-metadata":{"request_kind":"compaction"}}}`,
			},
			{
				name: "unknown request kind",
				body: `{"model":"gpt-5.6-sol","input":"hello","client_metadata":{"x-codex-turn-metadata":"{\"request_kind\":\"turn\"}"}}`,
			},
			{
				name: "nested tool output",
				body: `{"model":"gpt-5.6-sol","input":[{"type":"function_call_output","output":{"client_metadata":{"x-codex-turn-metadata":"{\"request_kind\":\"compaction\"}"}}}]}`,
			},
		}
		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				recorder := httptest.NewRecorder()
				ctx, _ := gin.CreateTestContext(recorder)
				ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
				ctx.Set("raw_body", []byte(test.body))
				input := &database.UsageLogInput{Endpoint: "/v1/responses"}

				populateCompactUsageMetaFromRequest(ctx, input)

				if input.Compact || input.HasCompactionHistory {
					t.Fatalf("got Compact=%v HasCompactionHistory=%v, want false/false", input.Compact, input.HasCompactionHistory)
				}
			})
		}
	})

	t.Run("cached websocket frame state is replaced", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)

		cacheRequestCompactionMeta(ctx, requestBodyCompactionMeta([]byte(`{
			"model":"gpt-5.6-sol",
			"client_metadata":{"x-codex-turn-metadata":"{\"request_kind\":\"compaction\"}"}
		}`)))
		first := &database.UsageLogInput{Endpoint: "/v1/responses"}
		populateCompactUsageMetaFromRequest(ctx, first)
		if !first.Compact {
			t.Fatal("first frame Compact = false, want true")
		}

		cacheRequestCompactionMeta(ctx, requestBodyCompactionMeta([]byte(`{"model":"gpt-5.6-sol","input":"next"}`)))
		second := &database.UsageLogInput{Endpoint: "/v1/responses"}
		populateCompactUsageMetaFromRequest(ctx, second)
		if second.Compact || second.HasCompactionHistory {
			t.Fatalf("second frame leaked prior state: Compact=%v HasCompactionHistory=%v", second.Compact, second.HasCompactionHistory)
		}

		cacheRequestCompactionMeta(ctx, requestBodyCompactionMeta([]byte(`{
			"model":"gpt-5.6-sol",
			"client_metadata":{"x-codex-turn-metadata":"{"}
		}`)))
		third := &database.UsageLogInput{Endpoint: "/v1/responses"}
		populateCompactUsageMetaFromRequest(ctx, third)
		if third.Compact || third.HasCompactionHistory {
			t.Fatalf("malformed third frame leaked prior state: Compact=%v HasCompactionHistory=%v", third.Compact, third.HasCompactionHistory)
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

func TestAccountFilterForModelRespectsAccountModelWhitelist(t *testing.T) {
	filter := accountFilterForModel("gpt-5.6-sol")
	if !filter(&auth.Account{PlanType: "plus"}) {
		t.Fatal("空白名单账号应放行任意模型")
	}
	if !filter(&auth.Account{PlanType: "pro", Models: []string{"GPT-5.6-SOL", "gpt-5.3-codex"}}) {
		t.Fatal("白名单命中（大小写不敏感）应放行")
	}
	restricted := &auth.Account{PlanType: "plus", Models: []string{"gpt-5.3-codex"}}
	if filter(restricted) {
		t.Fatal("白名单未包含请求模型时应拒绝")
	}
	if !accountFilterForModel("gpt-5.3-codex")(restricted) {
		t.Fatal("白名单内模型应放行")
	}
	if !accountFilterForModel("")(restricted) {
		t.Fatal("无模型信息的请求不应被白名单拦截")
	}
}

func TestCodexOnlyTransportsExcludeGrokAccounts(t *testing.T) {
	grok := &auth.Account{
		DBID: 99, UpstreamType: auth.UpstreamGrok, AccessToken: "grok-at",
		Models: []string{"gpt-5.6-sol", "grok-4.5"},
	}
	if accountFilterForModel("gpt-5.6-sol")(grok) {
		t.Fatal("Responses WebSocket/Realtime Codex filter admitted a Grok account")
	}
	if accountFilterForCompactResponsesModelWithOriginal("grok-4.5", "grok-4.5", false)(grok) {
		t.Fatal("Responses Compact filter admitted a Grok account")
	}
}

func TestIsCodexModelUnsupportedError(t *testing.T) {
	unsupported := []byte(`{"error":{"message":"The 'gpt-5.6-sol' model is not supported when using Codex with a ChatGPT account.","type":"invalid_request_error"}}`)
	if !isCodexModelUnsupportedError(unsupported) {
		t.Fatal("应识别模型不支持错误")
	}
	unknownProvider := []byte(`{"error":{"type":"upstream_error","message":"unknown provider for model gpt-5.6-sol"}}`)
	if !isCodexModelUnsupportedError(unknownProvider) {
		t.Fatal("应识别中转上游缺少模型提供商错误")
	}
	if isCodexModelUnsupportedError([]byte(`{"error":{"message":"Invalid value for 'temperature'","type":"invalid_request_error"}}`)) {
		t.Fatal("普通 invalid_request 不应命中")
	}
	if isCodexModelUnsupportedError(nil) {
		t.Fatal("空 body 不应命中")
	}

	general, rate := 0, 0
	if !shouldRetryHTTPStatus(http.StatusBadRequest, unsupported, &general, &rate, 2, 1) {
		t.Fatal("模型不支持的 400 应可换号重试")
	}
	general, rate = 0, 0
	if !shouldRetryHTTPStatus(http.StatusBadRequest, unknownProvider, &general, &rate, 2, 1) {
		t.Fatal("中转上游缺少模型提供商的 400 应可换号重试")
	}
	general, rate = 0, 0
	if shouldRetryHTTPStatus(http.StatusBadRequest, []byte(`{"error":{"message":"bad request"}}`), &general, &rate, 2, 1) {
		t.Fatal("普通 400 不应重试")
	}
}

func TestResponseFailedModelUnsupportedRetryable(t *testing.T) {
	payload := []byte(`{"type":"response.failed","response":{"error":{"code":"invalid_request_error","message":"The 'gpt-5.6-sol' model is not supported when using Codex with a ChatGPT account."}}}`)
	if !responseFailedRetryable(payload) {
		t.Fatal("模型不支持的 response.failed 应视为可换号重试")
	}
	unknownProvider := []byte(`{"type":"response.failed","response":{"status_code":400,"error":{"type":"upstream_error","message":"unknown provider for model gpt-5.6-sol"}}}`)
	if !responseFailedRetryable(unknownProvider) {
		t.Fatal("中转上游缺少模型提供商的 response.failed 应视为可换号重试")
	}
	plain := []byte(`{"type":"response.failed","response":{"error":{"code":"invalid_request_error","message":"Invalid value for 'temperature'"}}}`)
	if responseFailedRetryable(plain) {
		t.Fatal("普通 invalid_request 的 response.failed 不应重试")
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
	if !shouldRetryHTTPStatus(http.StatusTooManyRequests, nil, &generalRetries, &rateLimitRetries, 2, 1) {
		t.Fatal("first 429 should consume rate-limit retry budget")
	}
	if shouldRetryHTTPStatus(http.StatusTooManyRequests, nil, &generalRetries, &rateLimitRetries, 2, 1) {
		t.Fatal("second 429 should be blocked by rate-limit retry budget")
	}
	if !shouldRetryHTTPStatus(http.StatusServiceUnavailable, nil, &generalRetries, &rateLimitRetries, 2, 1) {
		t.Fatal("503 should still use general retry budget")
	}
	if generalRetries != 1 || rateLimitRetries != 1 {
		t.Fatalf("budgets = general %d rate %d, want 1/1", generalRetries, rateLimitRetries)
	}
}

func TestDeactivatedWorkspace402MarksAccountError(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: 42, AccessToken: "at", AccountID: "team-A", Status: auth.StatusReady}
	sibling := &auth.Account{DBID: 43, AccessToken: "at-2", AccountID: "team-A", Status: auth.StatusReady}
	other := &auth.Account{DBID: 44, AccessToken: "at-3", AccountID: "team-B", Status: auth.StatusReady}
	store.AddAccount(account)
	store.AddAccount(sibling)
	store.AddAccount(other)
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
	if sibling.RuntimeStatus() != "error" {
		t.Fatalf("same-workspace sibling status = %q, want error", sibling.RuntimeStatus())
	}
	sibling.Mu().RLock()
	siblingMsg := sibling.ErrorMsg
	sibling.Mu().RUnlock()
	if !strings.Contains(siblingMsg, "工作区联动") {
		t.Fatalf("sibling ErrorMsg = %q, want workspace-linked annotation", siblingMsg)
	}
	if other.RuntimeStatus() == "error" {
		t.Fatal("other workspace must not be marked")
	}
}

func TestGeneric402DoesNotLinkWorkspaceSiblings(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: 42, AccessToken: "at", AccountID: "team-A", Status: auth.StatusReady}
	sibling := &auth.Account{DBID: 43, AccessToken: "at-2", AccountID: "team-A", Status: auth.StatusReady}
	store.AddAccount(account)
	store.AddAccount(sibling)
	handler := &Handler{store: store}

	handler.applyCooldownForModel(account, http.StatusPaymentRequired, []byte(`{"error":{"message":"insufficient balance"}}`), &http.Response{Header: make(http.Header)}, "gpt-5.4")

	if sibling.RuntimeStatus() == "error" {
		t.Fatal("generic 402 must not fan out to workspace siblings")
	}
}

// TestAgentRuntimeDeleted403MarksAccountBanned 验证代理请求会将 runtime 已删除的账号标记为封禁。
func TestAgentRuntimeDeleted403MarksAccountBanned(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: 42, AccessToken: "at", Status: auth.StatusReady, HealthTier: auth.HealthTierHealthy}
	handler := &Handler{store: store}
	body := []byte(`{"error":{"message":"Agent runtime has been deleted.","type":null,"code":"biscuit_baker_service_agent_error_status","param":null},"status":403}`)

	if !IsAgentRuntimeDeletedError(body) {
		t.Fatal("expected deleted Agent runtime body to be detected")
	}
	if IsAgentRuntimeDeletedError([]byte(`{"error":{"message":"Agent runtime has been deleted.","code":"permission_denied"}}`)) {
		t.Fatal("unrelated error code must not be detected as deleted Agent runtime")
	}
	if got := upstreamErrorKind(http.StatusForbidden, body, codex429Decision{}); got != "agent_runtime_deleted" {
		t.Fatalf("upstreamErrorKind = %q, want agent_runtime_deleted", got)
	}

	handler.applyCooldownForModel(account, http.StatusForbidden, body, &http.Response{Header: make(http.Header)}, "gpt-5.4")

	if got := account.RuntimeStatus(); got != "unauthorized" {
		t.Fatalf("RuntimeStatus() = %q, want unauthorized", got)
	}
	if !account.IsBanned() {
		t.Fatal("account should be in banned health tier")
	}
	_, cooldownUntil := account.GetCooldownSnapshot()
	if remaining := time.Until(cooldownUntil); remaining < 23*time.Hour+59*time.Minute || remaining > 24*time.Hour {
		t.Fatalf("cooldown remaining = %s, want approximately 24h", remaining)
	}
	account.Mu().RLock()
	errorMsg := account.ErrorMsg
	account.Mu().RUnlock()
	if !strings.Contains(errorMsg, "Agent runtime has been deleted") {
		t.Fatalf("ErrorMsg = %q, want deleted runtime message", errorMsg)
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
	if got := recorder.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1", got)
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

// TestShouldRetryHTTPStatus403 验证上游 403 现在参与换号重试（issue #396），
// 受 general-retry 预算限制。
func TestShouldRetryHTTPStatus403(t *testing.T) {
	if !isRetryableStatus(http.StatusForbidden) {
		t.Fatal("403 应被视为可重试状态")
	}
	generalRetries := 0
	rateLimitRetries := 0
	if !shouldRetryHTTPStatus(http.StatusForbidden, nil, &generalRetries, &rateLimitRetries, 2, 1) {
		t.Fatal("首个 403（未达上限）应可重试")
	}
	if !shouldRetryHTTPStatus(http.StatusForbidden, nil, &generalRetries, &rateLimitRetries, 2, 1) {
		t.Fatal("第二个 403（仍未达上限）应可重试")
	}
	if shouldRetryHTTPStatus(http.StatusForbidden, nil, &generalRetries, &rateLimitRetries, 2, 1) {
		t.Fatal("达到 general 重试上限后 403 不应再重试")
	}
	if generalRetries != 2 {
		t.Fatalf("generalRetries = %d, want 2", generalRetries)
	}
	if rateLimitRetries != 0 {
		t.Fatalf("403 不应消耗限流预算，rateLimitRetries = %d, want 0", rateLimitRetries)
	}
}

// TestSendFinalUpstreamError_Forbidden403RemappedTo503 验证上游账号 403（额度/套餐/
// 工作区受限）重试耗尽后改写为 503 池级错误，不原样以 403 透传（issue #396），
// 避免 Claude Code 误判自身无权限而停工。
func TestSendFinalUpstreamError_Forbidden403RemappedTo503(t *testing.T) {
	gin.SetMode(gin.TestMode)

	for _, body := range [][]byte{
		[]byte(`{"error":{"message":"You have hit your usage limit.","code":"insufficient_quota"},"status":403}`),
		[]byte(`{"detail":{"code":"deactivated_workspace"}}`),
		[]byte(`{"error":{"code":"codex_access_restricted"}}`),
	} {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		handler := &Handler{}

		handler.sendFinalUpstreamError(ctx, http.StatusForbidden, body)

		if recorder.Code != http.StatusServiceUnavailable {
			t.Fatalf("body=%s status = %d, want %d (上游 403 不应以客户端 403 透传)", body, recorder.Code, http.StatusServiceUnavailable)
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
		if payload.Error.Code != "account_pool_forbidden" {
			t.Fatalf("body=%s code = %q, want account_pool_forbidden", body, payload.Error.Code)
		}
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

func TestApply429CooldownSparkUsageLimitDoesNotMarkAccount(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: 202, AccessToken: "token", PlanType: "pro", Status: auth.StatusReady}
	account.SetUsageSnapshot5h(40, time.Now().Add(2*time.Hour))

	decision := Apply429Cooldown(
		store,
		account,
		[]byte(`{"error":{"type":"usage_limit_reached","resets_in_seconds":1800}}`),
		&http.Response{Header: make(http.Header)},
		"gpt-5.3-codex-spark",
	)
	if decision.Scope != rateLimitScopeAccount {
		t.Fatalf("decision.Scope = %q, want account", decision.Scope)
	}
	pct, resetAt, ok := account.GetUsageSnapshotSpark()
	if !ok || pct != 100 {
		t.Fatalf("spark snapshot = (%v, %v), want 100", pct, ok)
	}
	if got := time.Until(resetAt); got < 25*time.Minute || got > 35*time.Minute {
		t.Fatalf("spark reset delta = %v, want about 30m", got)
	}
	if account.IsPremium5hRateLimited() {
		t.Fatal("spark usage_limit_reached must not enter rate_limited_5h")
	}
	if reason := account.GetCooldownReason(); reason != "" {
		t.Fatalf("cooldown_reason = %q, want empty", reason)
	}
	if pct5h, ok := account.GetUsagePercent5h(); !ok || pct5h != 40 {
		t.Fatalf("main 5h snapshot = (%v, %v), want unchanged 40", pct5h, ok)
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
			name:    "previous response not found",
			payload: `{"type":"response.failed","response":{"error":{"code":"previous_response_not_found","message":"missing response"}}}`,
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

func TestApply429CooldownRelayDefaultDoesNotPersistCooldown(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	account := &auth.Account{
		DBID:         103,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      "https://api.example.test",
		APIKey:       "test-key",
		PlanType:     "api",
	}

	decision := Apply429Cooldown(
		store,
		account,
		[]byte(`{"detail":"Rate limit exceeded"}`),
		&http.Response{Header: make(http.Header)},
		"gpt-5.6-sol",
	)

	if decision.Scope != rateLimitScopeModel || decision.Reason != "rate_limited_model" {
		t.Fatalf("decision = %#v, want non-persistent model rate limit", decision)
	}
	if !decision.ResetAt.IsZero() || decision.Cooldown != 0 {
		t.Fatalf("decision cooldown = %v reset=%v, want no persistent cooldown", decision.Cooldown, decision.ResetAt)
	}
	if account.IsModelRateLimited("gpt-5.6-sol") {
		t.Fatal("relay account should remain schedulable after a transient 429")
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
	// 信用开关现在还要求当下确实有积分可花，快照缺失会按「没有积分」处理。
	account.SetCreditBalance("1000.0000000000", true, false, false)
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
	// 窗口打满但积分顶着：显示仍是限流，调度侧不受影响，
	// 前端据 UsingCredits 在限流徽章后面挂一个积分徽章。
	if got := account.RuntimeStatus(); got != "rate_limited" {
		t.Fatalf("RuntimeStatus() = %q, want rate_limited for credit account", got)
	}
	if !account.IsAvailable() {
		t.Fatal("IsAvailable() = false, want true while credits cover the window")
	}
	if !account.UsingCredits() {
		t.Fatal("UsingCredits() = false, want true while credits cover the window")
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

// issue #382：响应头仅有 7d 时清除陈旧 5h；完全无用量头时保留 5h。
func TestSyncCodexUsageState_Clears5hWhenOnly7dHeaders(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer db.Close()

	id, err := db.InsertAccountWithCredentials(ctx, "header-clear-5h", map[string]interface{}{
		"access_token":          "at",
		"plan_type":             "plus",
		"codex_5h_used_percent": 90,
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}

	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: id, AccessToken: "at", PlanType: "plus"}
	account.SetUsageSnapshot5h(90, time.Now().Add(time.Hour))

	resp := &http.Response{Header: make(http.Header)}
	resp.Header.Set("x-codex-primary-used-percent", "15")
	resp.Header.Set("x-codex-primary-window-minutes", "10080")
	resp.Header.Set("x-codex-primary-reset-after-seconds", "500000")

	result := SyncCodexUsageState(store, account, resp)
	if result.HasUsage5h {
		t.Fatalf("result = %+v, want no 5h", result)
	}
	if !result.Cleared5h {
		t.Fatal("Cleared5h = false, want true")
	}
	if !result.HasUsage7d || result.UsagePct7d != 15 {
		t.Fatalf("result = %+v, want 7d=15", result)
	}
	if _, ok := account.GetUsagePercent5h(); ok {
		t.Fatal("in-memory 5h should be cleared")
	}
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if got := row.GetCredential("codex_5h_used_percent"); got != "" {
		t.Errorf("persisted codex_5h_used_percent = %q, want cleared", got)
	}

	reloadedStore := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	if err := reloadedStore.LoadAccountByID(ctx, id); err != nil {
		t.Fatalf("LoadAccountByID: %v", err)
	}
	reloaded := reloadedStore.FindByID(id)
	if reloaded == nil {
		t.Fatal("reloaded account is nil")
	}
	if _, ok := reloaded.GetUsagePercent5h(); ok {
		t.Fatal("cleared 5h snapshot was hydrated again after reload")
	}
}

func TestSyncCodexUsageState_Preserves5hWhenNoUsageHeaders(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: 200, PlanType: "plus"}
	resetAt := time.Now().Add(2 * time.Hour)
	account.SetUsageSnapshot5h(55, resetAt)

	resp := &http.Response{Header: make(http.Header)}
	resp.Header.Set("x-codex-plan-type", "plus")
	// 无 primary/secondary 用量头：可能是中间件剥头，不能误清 5h

	result := SyncCodexUsageState(store, account, resp)
	if result.Cleared5h {
		t.Fatal("Cleared5h = true, want false when response has no usage windows")
	}
	pct, ok := account.GetUsagePercent5h()
	if !ok || pct != 55 {
		t.Fatalf("usage_percent_5h = (%v, %v), want (55, true)", pct, ok)
	}
}

func TestSyncCodexUsageState_PartialUsedPercentHeaderDoesNotClear5h(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: 201, PlanType: "plus"}
	account.SetUsageSnapshot5h(63, time.Now().Add(2*time.Hour))

	resp := &http.Response{Header: make(http.Header)}
	resp.Header.Set("x-codex-primary-used-percent", "12")
	result := SyncCodexUsageState(store, account, resp)

	if result.Cleared5h || result.HasUsage7d {
		t.Fatalf("result = %+v, want partial headers ignored", result)
	}
	if pct, ok := account.GetUsagePercent5h(); !ok || pct != 63 {
		t.Fatalf("usage_percent_5h = (%v, %v), want (63, true)", pct, ok)
	}
}

func TestParseCodexUsageHeaders_7dOnlyClearsMemoryWithoutStore(t *testing.T) {
	account := &auth.Account{DBID: 202, PlanType: "plus"}
	account.SetUsageSnapshot5h(63, time.Now().Add(2*time.Hour))
	resp := &http.Response{Header: make(http.Header)}
	resp.Header.Set("x-codex-primary-used-percent", "12")
	resp.Header.Set("x-codex-primary-window-minutes", "10080")
	resp.Header.Set("x-codex-primary-reset-after-seconds", "500000")

	pct7d, ok := ParseCodexUsageHeaders(resp, account)
	if !ok || pct7d != 12 {
		t.Fatalf("ParseCodexUsageHeaders() = (%v, %v), want (12, true)", pct7d, ok)
	}
	if _, ok := account.GetUsagePercent5h(); ok {
		t.Fatal("public parse-only path should clear stale in-memory 5h without a store")
	}
}

func TestSyncCodexUsageState_NilStorePreservesPremiumCooldown(t *testing.T) {
	account := &auth.Account{DBID: 203, PlanType: "plus", Status: auth.StatusReady}
	account.SetUsageSnapshot5h(100, time.Now().Add(2*time.Hour))
	account.SetCooldownUntil(time.Now().Add(2*time.Hour), "rate_limited_5h")

	resp := &http.Response{Header: make(http.Header)}
	resp.Header.Set("x-codex-primary-used-percent", "12")
	resp.Header.Set("x-codex-primary-window-minutes", "10080")
	resp.Header.Set("x-codex-primary-reset-after-seconds", "500000")

	result := SyncCodexUsageState(nil, account, resp)
	if !result.Cleared5h {
		t.Fatal("7d-only parse should clear the stale in-memory snapshot")
	}
	if account.Status != auth.StatusCooldown || account.GetCooldownReason() != "rate_limited_5h" {
		t.Fatalf("nil-store parse changed cooldown state: status=%v reason=%q", account.Status, account.GetCooldownReason())
	}
}

func TestSyncCodexUsageState_7dOnlyPreservesNewerUnauthorizedCooldown(t *testing.T) {
	ctx := context.Background()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "codex2api.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer db.Close()
	id, err := db.InsertAccountWithCredentials(ctx, "header-preserve-401", map[string]interface{}{
		"access_token":              "at",
		"plan_type":                 "plus",
		"codex_5h_used_percent":     100,
		"codex_5h_reset_at":         time.Now().Add(2 * time.Hour).Format(time.RFC3339),
		"codex_5h_usage_updated_at": time.Now().Format(time.RFC3339),
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}

	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: id, AccessToken: "at", PlanType: "plus", Status: auth.StatusReady, HealthTier: auth.HealthTierHealthy}
	store.MarkPremium5hRateLimited(account, time.Now().Add(2*time.Hour))
	atomic.StoreInt32(&account.Disabled, 1)
	store.MarkCooldown(account, time.Hour, "unauthorized")

	resp := &http.Response{Header: make(http.Header)}
	resp.Header.Set("x-codex-primary-used-percent", "15")
	resp.Header.Set("x-codex-primary-window-minutes", "10080")
	resp.Header.Set("x-codex-primary-reset-after-seconds", "500000")
	result := SyncCodexUsageState(store, account, resp)

	if !result.Cleared5h {
		t.Fatal("Cleared5h = false, want stale snapshot cleared")
	}
	if account.GetCooldownReason() != "unauthorized" || atomic.LoadInt32(&account.Disabled) != 1 {
		t.Fatalf("runtime state = (%q, disabled=%d), want unauthorized and disabled", account.GetCooldownReason(), atomic.LoadInt32(&account.Disabled))
	}
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if row.CooldownReason != "unauthorized" || !row.CooldownUntil.Valid {
		t.Fatalf("persisted cooldown = (%q, %v), want unauthorized", row.CooldownReason, row.CooldownUntil)
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

func TestAuthMiddlewareAcceptsOpenAIWebSocketSubprotocolAPIKey(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New returned error: %v", err)
	}
	defer db.Close()

	key := "sk-test-ws-auth-1234567890"
	id, err := db.InsertAPIKey(context.Background(), "NewAPI WS", key)
	if err != nil {
		t.Fatalf("InsertAPIKey returned error: %v", err)
	}

	handler := NewHandler(nil, db, nil, nil)
	router := gin.New()
	router.Use(handler.authMiddleware())
	router.GET("/v1/responses", func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"id": c.MustGet(contextAPIKeyID), "raw": c.MustGet("apiKey")})
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/responses?model=gpt-5.6-sol", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Protocol", "realtime, openai-insecure-api-key."+key+", openai-beta.realtime-v1")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if got := gjson.GetBytes(recorder.Body.Bytes(), "id").Int(); got != id {
		t.Fatalf("api key id = %d, want %d", got, id)
	}
	if got := gjson.GetBytes(recorder.Body.Bytes(), "raw").String(); got != key {
		t.Fatalf("raw key = %q, want configured websocket key", got)
	}
}

func TestAuthMiddlewareDoesNotAcceptWebSocketSubprotocolOnOrdinaryHTTP(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New returned error: %v", err)
	}
	defer db.Close()
	key := "sk-test-ws-http-reject-1234567890"
	if _, err := db.InsertAPIKey(context.Background(), "HTTP Reject", key); err != nil {
		t.Fatalf("InsertAPIKey returned error: %v", err)
	}

	handler := NewHandler(nil, db, nil, nil)
	router := gin.New()
	router.Use(handler.authMiddleware())
	router.GET("/ok", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"ok": true}) })

	req := httptest.NewRequest(http.MethodGet, "/ok", nil)
	req.Header.Set("Sec-WebSocket-Protocol", "realtime, openai-insecure-api-key."+key)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusUnauthorized, recorder.Body.String())
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

func TestApplyAffinityGroupRoutingSplitsByRequestFingerprint(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set(contextAPIKeyRow, &database.APIKeyRow{
		AllowedGroupIDs: []int64{10},
		Limits: database.APIKeyLimits{
			NoAffinityGroupIDs: []int64{20},
		},
	})
	primary := &auth.Account{DBID: 1, GroupIDs: []int64{10}}
	split := &auth.Account{DBID: 2, GroupIDs: []int64{20}}

	withoutAffinity := applyAffinityGroupRouting(c, requestSessionIdentity{}, nil)
	if withoutAffinity(primary) || !withoutAffinity(split) {
		t.Fatal("request without affinity header must use only the split groups")
	}

	withFingerprint := applyAffinityGroupRouting(c, requestSessionIdentity{hasRequestFingerprint: true}, nil)
	if !withFingerprint(primary) || withFingerprint(split) {
		t.Fatal("fingerprinted request must keep using the original groups")
	}
}

func TestApplyAffinityGroupRoutingDisabledKeepsExistingFilter(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set(contextAPIKeyRow, &database.APIKeyRow{AllowedGroupIDs: []int64{10}})
	want := auth.AccountFilter(func(account *auth.Account) bool { return account != nil && account.DBID == 1 })
	if got := applyAffinityGroupRouting(c, requestSessionIdentity{}, want); got(&auth.Account{DBID: 1}) != true || got(&auth.Account{DBID: 2}) != false {
		t.Fatal("disabled split routing must preserve the existing account filter")
	}
}

// TestApplyAffinityGroupRoutingExcludesSplitGroupsWhenNoAllowedGroups 覆盖最常见的配置：
// Key 不限分组。此时带指纹的请求必须避开分流组，否则分流组照样接真 Codex 流量，隔离等于没做。
func TestApplyAffinityGroupRoutingExcludesSplitGroupsWhenNoAllowedGroups(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set(contextAPIKeyRow, &database.APIKeyRow{
		Limits: database.APIKeyLimits{NoAffinityGroupIDs: []int64{20}},
	})
	split := &auth.Account{DBID: 1, GroupIDs: []int64{20}}
	other := &auth.Account{DBID: 2, GroupIDs: []int64{30}}
	ungrouped := &auth.Account{DBID: 3}

	withFingerprint := applyAffinityGroupRouting(c, requestSessionIdentity{hasRequestFingerprint: true}, nil)
	if withFingerprint(split) {
		t.Fatal("fingerprinted request must not land on a split group account")
	}
	if !withFingerprint(other) || !withFingerprint(ungrouped) {
		t.Fatal("fingerprinted request must keep every non-split account available")
	}

	withoutFingerprint := applyAffinityGroupRouting(c, requestSessionIdentity{}, nil)
	if !withoutFingerprint(split) || withoutFingerprint(other) || withoutFingerprint(ungrouped) {
		t.Fatal("request without a fingerprint must use only the split groups")
	}
}

// TestApplyAffinityGroupRoutingKeepsInnerFilter 分组门必须叠加在既有 filter 之上，
// 不能把模型/冷却/预算这些闸门盖掉。
func TestApplyAffinityGroupRoutingKeepsInnerFilter(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set(contextAPIKeyRow, &database.APIKeyRow{
		Limits: database.APIKeyLimits{NoAffinityGroupIDs: []int64{20}},
	})
	inner := auth.AccountFilter(func(account *auth.Account) bool { return account != nil && account.DBID == 1 })

	split := applyAffinityGroupRouting(c, requestSessionIdentity{}, inner)
	if !split(&auth.Account{DBID: 1, GroupIDs: []int64{20}}) {
		t.Fatal("split routing dropped an account the inner filter accepts")
	}
	if split(&auth.Account{DBID: 2, GroupIDs: []int64{20}}) {
		t.Fatal("split routing must not bypass the inner filter")
	}

	fingerprinted := applyAffinityGroupRouting(c, requestSessionIdentity{hasRequestFingerprint: true}, inner)
	if !fingerprinted(&auth.Account{DBID: 1, GroupIDs: []int64{30}}) {
		t.Fatal("exclusion routing dropped an account the inner filter accepts")
	}
	if fingerprinted(&auth.Account{DBID: 2, GroupIDs: []int64{30}}) {
		t.Fatal("exclusion routing must not bypass the inner filter")
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

// body-signal compact:中转账号池收到带 compaction_trigger 的流式 /responses
// 请求时，必须保留 /responses 的 SSE 协议；compact 专用模型映射只能改写模型，
// 不能把请求改道到返回一次性 JSON 的 /responses/compact（issue #361）。
func TestResponses_BodySignalCompactStaysStreamingOnRelayOnlyPool(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var seenPath string
	var seenBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenBody, _ = io.ReadAll(r.Body)
		if r.URL.Path == "/v1/responses/compact" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{
				"id":"resp_compaction_test",
				"object":"response.compaction",
				"created_at":1710000000,
				"model":"gpt-4.1-direct",
				"output":[{"type":"compaction_summary","summary":"user likes blue"}],
				"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}
			}`))
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"response.created","response":{"id":"resp_compaction_test"}}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp_compaction_test","status":"completed","output":[{"type":"compaction_summary","summary":"user likes blue"}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`+"\n\n")
	}))
	defer upstream.Close()

	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:      2,
		MaxRetries:          0,
		MaxRateLimitRetries: 0,
	})
	store.SetCodexModelMapping(`{"client-body-signal-alias-openai-compact":"gpt-4.1-direct","gpt-4.1-direct":"gpt-4.1-second"}`)
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
		"model":"client-body-signal-alias",
		"stream":true,
		"client_metadata":{"x-codex-window-id":"w-1","x-codex-installation-id":"i-1"},
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
	if seenPath != "/v1/responses" {
		t.Fatalf("upstream path = %q, want /v1/responses to preserve streaming", seenPath)
	}
	if !gjson.GetBytes(seenBody, "stream").Bool() {
		t.Fatalf("upstream body must preserve stream=true, got %s", seenBody)
	}
	if !requestBodyHasCompactionTrigger(seenBody) {
		t.Fatalf("upstream body lost compaction_trigger: %s", seenBody)
	}
	if model := gjson.GetBytes(seenBody, "model").String(); model != "gpt-4.1-direct" {
		t.Fatalf("streaming compact model = %q, want one-pass mapping to gpt-4.1-direct; body=%s", model, seenBody)
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.Contains(contentType, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream; body=%s", contentType, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"type":"response.completed"`) {
		t.Fatalf("downstream stream missing response.completed; body=%s", recorder.Body.String())
	}
}

// 流式 body-signal 继续走 /responses 时，也必须保留账号级 compact 专用映射；
// 否则 compact-only alias 无法选中中转账号，或会把错误模型发给上游（PR #350）。
func TestResponses_BodySignalCompactStreamingUsesAccountCompactMapping(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var seenPath string
	var seenBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp_account_mapping","status":"completed","output":[{"type":"compaction_summary","summary":"mapped"}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`+"\n\n")
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
		ModelMapping: `{"gpt-5.4-openai-compact":"gpt-4.1-direct"}`,
		PlanType:     "api",
	})
	handler := NewHandler(store, nil, nil, nil)

	body := []byte(`{
		"model":"gpt-5.4",
		"stream":true,
		"input":[{"type":"compaction_trigger"}]
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
	if seenPath != "/v1/responses" {
		t.Fatalf("upstream path = %q, want /v1/responses", seenPath)
	}
	if model := gjson.GetBytes(seenBody, "model").String(); model != "gpt-4.1-direct" {
		t.Fatalf("upstream model = %q, want account compact mapping target gpt-4.1-direct; body=%s", model, seenBody)
	}
}

// 非流式 body-signal 没有 SSE 契约，继续使用 compact 专用端点以兼容只实现
// /responses/compact 的中转账号。
func TestResponses_NonStreamingBodySignalCompactStillUsesCompactEndpoint(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var seenPath string
	var seenBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"resp_non_stream_compaction",
			"object":"response.compaction",
			"output":[{"type":"compaction_summary","summary":"done"}],
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
		"stream":false,
		"input":[{"type":"compaction_trigger"}]
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
		t.Fatalf("upstream path = %q, want /v1/responses/compact", seenPath)
	}
	if gjson.GetBytes(seenBody, "stream").Exists() {
		t.Fatalf("compact upstream body should not carry stream, got %s", seenBody)
	}
}

// A durable compaction item is conversation history, not a request control. It must remain on the
// normal Responses path even when the account pool contains only relay accounts.
func TestResponses_CompactionHistoryNotPromotedOnRelayOnlyPool(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var seenPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"resp_history_test",
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

	body := []byte(`{
		"model":"gpt-4.1-direct",
		"input":[
			{"type":"compaction","encrypted_content":"opaque-history"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}
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
	if seenPath != "/v1/responses" {
		t.Fatalf("upstream path = %q, want /v1/responses for durable compaction history", seenPath)
	}
}

// A metadata-only manual /compact turn is an observability signal, not the
// protocol compaction_trigger control. It must remain on the normal Responses
// route even for a non-streaming relay-only pool.
func TestResponses_MetadataCompactionNotPromotedOnRelayOnlyPool(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var seenPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"resp_metadata_compaction",
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

	body := []byte(`{
		"model":"gpt-4.1-direct",
		"stream":false,
		"input":[{"type":"message","role":"user","content":"summarize"}],
		"client_metadata":{
			"x-codex-turn-metadata":"{\"request_kind\":\"compaction\",\"thread_source\":\"user\"}"
		}
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
	if seenPath != "/v1/responses" {
		t.Fatalf("upstream path = %q, want /v1/responses for metadata-only compaction", seenPath)
	}
	meta, ok := cachedRequestCompactionMeta(ctx)
	if !ok {
		t.Fatal("request compaction metadata was not cached")
	}
	if meta.ProtocolTriggered || !meta.UsageTriggered {
		t.Fatalf("cached meta = %+v, want protocol=false usage=true", meta)
	}
}

// 不带 compaction_trigger 的普通请求不受提升逻辑影响,仍走 /v1/responses。
func TestResponses_PlainRequestNotPromotedOnRelayOnlyPool(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var seenPath string
	var seenBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenBody, _ = io.ReadAll(r.Body)
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
	store.SetCodexModelMapping(`{"client-response-alias":"gpt-4.1-direct"}`)
	store.AddAccount(&auth.Account{
		DBID:         1,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      upstream.URL,
		APIKey:       "sk-direct",
		Models:       []string{"gpt-4.1-direct"},
		PlanType:     "api",
	})
	handler := NewHandler(store, nil, nil, nil)

	body := []byte(`{"model":"client-response-alias","input":"hello"}`)
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
	if model := gjson.GetBytes(seenBody, "model").String(); model != "gpt-4.1-direct" {
		t.Fatalf("upstream model = %q, want mapped gpt-4.1-direct; body=%s", model, seenBody)
	}
}

func TestResponsesRelaySuccessPreservesNewerUsageLimitCooldown(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var store *auth.Store
	account := &auth.Account{
		DBID:         1,
		UpstreamType: auth.UpstreamOpenAIResponses,
		APIKey:       "sk-direct",
		Models:       []string{"gpt-4.1-direct"},
		PlanType:     "api",
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		store.MarkCooldown(account, time.Hour, "rate_limited")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"resp_success_clears_limit",
			"object":"response",
			"status":"completed",
			"model":"gpt-4.1-direct",
			"output":[],
			"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}
		}`))
	}))
	defer upstream.Close()

	account.BaseURL = upstream.URL
	store = auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:         2,
		MaxRetries:             0,
		MaxRateLimitRetries:    0,
		IgnoreUsageLimitStatus: true,
	})
	store.AddAccount(account)
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
	if !account.HasActiveCooldown() || account.IsAvailable() {
		t.Fatal("a stale successful relay request must not clear a newer usage-limit cooldown")
	}
}

// issue #540：纯 OpenAI Responses 中转池收到 Codex 新版 Remote Compact
// （/responses + stream + compaction_trigger）时，必须按普通 /responses 选中该中转，
// 不能在选号阶段直接 503。
func TestResponses_NativeRemoteCompactionV2UsesRelayOnlyPool(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var seenPath string
	var seenBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		seenBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp_issue_540","status":"completed","output":[{"type":"compaction_summary","summary":"compacted"}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`+"\n\n")
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
		Models:       []string{"gpt-5.6-luna", "gpt-5.6-sol", "gpt-5.6-terra"},
		PlanType:     "api",
	})
	handler := NewHandler(store, nil, nil, nil)

	body := []byte(`{
		"model":"gpt-5.6-sol",
		"stream":true,
		"input":[
			{"type":"message","role":"user","content":"hello"},
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
	if seenPath != "/v1/responses" {
		t.Fatalf("upstream path = %q, want /v1/responses", seenPath)
	}
	if !requestBodyHasCompactionTrigger(seenBody) {
		t.Fatalf("upstream body lost compaction_trigger: %s", seenBody)
	}
	if model := gjson.GetBytes(seenBody, "model").String(); model != "gpt-5.6-sol" {
		t.Fatalf("upstream model = %q, want gpt-5.6-sol; body=%s", model, seenBody)
	}
}

// issue #540：池里还有可用官方账号时，不能把流式 Remote Compact 钉死在官方账号上。
// 官方号若因模型白名单选不上，必须回落到能打 /responses 的中转，而不是立刻 503。
func TestResponses_NativeRemoteCompactionV2FallsBackToRelayWhenOfficialCannotServe(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var seenPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenPath = r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"type":"response.completed","response":{"id":"resp_issue_540_fallback","status":"completed","output":[{"type":"compaction_summary","summary":"compacted"}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`+"\n\n")
	}))
	defer upstream.Close()

	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:      2,
		MaxRetries:          0,
		MaxRateLimitRetries: 0,
	})
	store.AddAccount(&auth.Account{
		DBID:        2,
		AccessToken: "at-codex",
		Models:      []string{"gpt-5.4"},
	})
	store.AddAccount(&auth.Account{
		DBID:         1,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      upstream.URL,
		APIKey:       "sk-direct",
		Models:       []string{"gpt-5.6-luna", "gpt-5.6-sol", "gpt-5.6-terra"},
		PlanType:     "api",
	})
	handler := NewHandler(store, nil, nil, nil)

	body := []byte(`{
		"model":"gpt-5.6-sol",
		"stream":true,
		"input":[{"type":"compaction_trigger"}]
	}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = req

	handler.Responses(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 after falling back to relay; body=%s", recorder.Code, recorder.Body.String())
	}
	if seenPath != "/v1/responses" {
		t.Fatalf("upstream path = %q, want /v1/responses", seenPath)
	}
}

// excludeRelayAccountsFilter / storeHasAvailableCodexAccount 仍供其它路径复用；
// 流式 Remote Compact 不再用它们把请求钉死在官方账号上（issue #540）。
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

// 回归（PR #350）：中转账号映射里存在 suffixed→suffixed 的恒等规则时，
// 客户端用基础名触发压缩，上游必须仍收到基础名 gpt-5.6-sol，
// 而不是被合成别名命中规则后原样转发 gpt-5.6-sol-openai-compact。
func TestResponsesCompactStaleSuffixIdentityRuleKeepsBaseModelUpstream(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var seenBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"resp_compact_identity",
			"object":"response",
			"model":"gpt-5.6-sol",
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
		Models:       []string{"gpt-5.6-sol", "gpt-5.6-sol-openai-compact"},
		ModelMapping: `{"gpt-5.6-sol-openai-compact":"gpt-5.6-sol-openai-compact"}`,
		PlanType:     "api",
	})
	handler := NewHandler(store, nil, nil, nil)

	body := []byte(`{"model":"gpt-5.6-sol","input":"hello","stream":true}`)
	requestCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(body)).WithContext(requestCtx)
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = req

	handler.ResponsesCompact(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if model := gjson.GetBytes(seenBody, "model").String(); model != "gpt-5.6-sol" {
		t.Fatalf("upstream model = %q, want base gpt-5.6-sol; body=%s", model, seenBody)
	}
}

// 统计展示：客户端用 -openai-compact 别名请求压缩时，usage 上下文中的模型
// 应折算为基础名，且"仅剥后缀"不算映射（不产生 effective_model 箭头）。
func TestResponsesCompactSuffixOnlyRequestLogsBaseModel(t *testing.T) {
	gin.SetMode(gin.TestMode)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"resp_compact_display",
			"object":"response",
			"model":"gpt-5.6-sol",
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
		Models:       []string{"gpt-5.6-sol"},
		PlanType:     "api",
	})
	handler := NewHandler(store, nil, nil, nil)

	body := []byte(`{"model":"gpt-5.6-sol-openai-compact","input":"hello","stream":true}`)
	requestCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(body)).WithContext(requestCtx)
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = req

	handler.ResponsesCompact(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if got := ctx.GetString("x-model"); got != "gpt-5.6-sol" {
		t.Fatalf("x-model = %q, want base gpt-5.6-sol (suffix stripped for display)", got)
	}
}

// Grok/xAI 的错误体用顶层字符串 error 字段({"code":"...","error":"文本"})。
// 解析器此前只认 error.message 对象形态,导致这类 400 对用户只显示裸的
// "Upstream returned status 400",根因全靠翻容器日志。
func TestUsageLogErrorMessageGrokStringErrorField(t *testing.T) {
	got := usageLogErrorMessage(400, []byte(`{"code":"invalid-argument","error":"The function name tool_search is reserved for the tool_search tool"}`))
	want := "invalid-argument · The function name tool_search is reserved for the tool_search tool"
	if got != want {
		t.Fatalf("grok string error not extracted: got %q want %q", got, want)
	}

	// 对象形态不受影响。
	if got := usageLogErrorMessage(400, []byte(`{"error":{"message":"object form","code":"bad"}}`)); got != "bad · object form" {
		t.Fatalf("object error form regressed: %q", got)
	}

	// error 为对象但无 message 时不得把整个 JSON 打进 message。
	if got := usageLogErrorMessage(400, []byte(`{"error":{"foo":"bar"}}`)); got != "HTTP 400" {
		t.Fatalf("object error without message must fall back: %q", got)
	}
}
