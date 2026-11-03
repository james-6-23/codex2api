// proxy/acceptance_lifecycle_test.go
package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/codex2api/security/outputguard"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

// Helper: 构建具备指定 upstream 的测试 Handler、Store 与 Account
func setupLifecycleTestHandler(t *testing.T, upstreamURL string, upstreamType string, model string) (*Handler, *auth.Store, *auth.Account) {
	t.Helper()

	// 保证装载合法的最小输出失控防护配置，并于退出时重置状态
	setupOutputGuardHTTPTestEnv(t)

	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "acceptance_lifecycle.db"))
	if err != nil {
		t.Fatalf("database.New(sqlite) error: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	store := auth.NewStore(db, nil, &database.SystemSettings{
		MaxConcurrency:      10,
		MaxRetries:          2,
		MaxRateLimitRetries: 0,
	})
	t.Cleanup(store.Stop)

	account := &auth.Account{
		DBID:                       101,
		UpstreamType:               upstreamType,
		Status:                     auth.StatusReady,
		HealthTier:                 auth.HealthTierHealthy,
		BaseURL:                    upstreamURL,
		APIKey:                     "sk-lifecycle-test-key",
		AccessToken:                "at-lifecycle-test-token",
		PlanType:                   "plus",
		AccountID:                  "acct-lifecycle-101",
		Models:                     []string{model},
		ResponsesUpstreamTransport: auth.OpenAIResponsesTransportHTTP,
	}

	if upstreamType == auth.UpstreamClaude {
		account.ClaudeAuthKind = auth.ClaudeAuthKindAPIKey
		account.ClaudeBaseURL = upstreamURL
		account.AccessToken = "sk-ant-test-key"
	}

	if upstreamType == auth.UpstreamAntigravity {
		account.AntigravityProjectID = "test-project-101"
		account.AccessToken = "test-antigravity-token"
		account.APIKey = ""
		account.Models = []string{"gemini-3.7-flash-tiered", "gemini-3.7-flash-low", model}
	}

	if upstreamType == auth.UpstreamGrok {
		account.BaseURL = upstreamURL + "/v1"
		account.APIKey = "xai-test-key"
		now := time.Now()
		account.SetGrokRoutingState(auth.GrokRoutingState{
			Models: []auth.GrokModelRoute{
				{
					ModelID:    model,
					BaseURL:    upstreamURL + "/v1",
					APIBackend: auth.GrokProtocolChatCompletions,
				},
			},
			Capabilities: []auth.GrokProtocolCapability{
				{
					ModelID:    model,
					Origin:     upstreamURL + "/v1",
					Protocol:   auth.GrokProtocolChatCompletions,
					Status:     auth.GrokCapabilityOK,
					ExpiresAt:  now.Add(time.Hour),
					ObservedAt: now,
				},
			},
		})
	}

	store.AddAccount(account)

	cfg := &config.Config{AllowAnonymousV1: true}
	handler := NewHandler(store, db, cfg, nil)
	handler.SetRuntimeCache(cache.NewMemory(1))
	return handler, store, account
}

// -----------------------------------------------------------------------------
// 场景 12: §4.1 每入口 stream/non-stream、native/converted 及实际支持的 upstream 组合
// 覆盖: Responses HTTP (SSE/JSON), Responses WS, Realtime WS 文本子集,
// ChatCompletions (SSE/JSON via native Grok & Responses), Messages (SSE/JSON),
// Gemini Native (streamGenerateContent & generateContent via Antigravity OAuth),
// Grok Native Chat, Antigravity 渠道组合
// -----------------------------------------------------------------------------
func TestAcceptanceScenario12_FullProtocolCoverageMatrix(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 构造通用失控重复 payload (周期 20B 循环 500 次 = 10000B >= 9215B，触发 repetition)
	pattern := "runaway_loop_abcdef_"
	runawayText := strings.Repeat(pattern, 500)

	matrix := []struct {
		name         string
		endpoint     string
		routePath    string
		upstreamType string
		model        string
		stream       bool
		reqBody      string
		mockHandler  http.HandlerFunc
		invoke       func(h *Handler, c *gin.Context)
	}{
		{
			name:         "ResponsesHTTP_Stream_SSE",
			endpoint:     "/v1/responses",
			routePath:    "/v1/responses",
			upstreamType: auth.UpstreamOpenAIResponses,
			model:        "gpt-5.5",
			stream:       true,
			reqBody:      `{"model":"gpt-5.5","stream":true,"input":"test"}`,
			mockHandler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				flusher := w.(http.Flusher)
				_, _ = io.WriteString(w, "data: "+`{"type":"response.output_text.delta","delta":"`+runawayText+`"}`+"\n\n")
				flusher.Flush()
			},
			invoke: func(h *Handler, c *gin.Context) { h.Responses(c) },
		},
		{
			name:         "ResponsesHTTP_NonStream_JSON",
			endpoint:     "/v1/responses",
			routePath:    "/v1/responses",
			upstreamType: auth.UpstreamOpenAIResponses,
			model:        "gpt-5.5",
			stream:       false,
			reqBody:      `{"model":"gpt-5.5","stream":false,"input":"test"}`,
			mockHandler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				resp := map[string]any{
					"id":     "resp_json",
					"object": "response",
					"status": "completed",
					"output": []any{
						map[string]any{
							"type": "message",
							"content": []any{
								map[string]any{"type": "output_text", "text": runawayText},
							},
						},
					},
				}
				_ = json.NewEncoder(w).Encode(resp)
			},
			invoke: func(h *Handler, c *gin.Context) { h.Responses(c) },
		},
		{
			name:         "ChatCompletions_Stream_SSE_ResponsesUpstream",
			endpoint:     "/v1/chat/completions",
			routePath:    "/v1/chat/completions",
			upstreamType: auth.UpstreamOpenAIResponses,
			model:        "gpt-5.5",
			stream:       true,
			reqBody:      `{"model":"gpt-5.5","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			mockHandler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				flusher := w.(http.Flusher)
				_, _ = io.WriteString(w, "data: "+`{"type":"response.output_text.delta","delta":"Hello "}`+"\n\n")
				flusher.Flush()
				time.Sleep(10 * time.Millisecond)
				chunk := `{"type":"response.output_text.delta","delta":"` + runawayText + `"}`
				_, _ = io.WriteString(w, "data: "+chunk+"\n\n")
				flusher.Flush()
			},
			invoke: func(h *Handler, c *gin.Context) { h.ChatCompletions(c) },
		},
		{
			name:         "ChatCompletions_NonStream_JSON_ResponsesUpstream",
			endpoint:     "/v1/chat/completions",
			routePath:    "/v1/chat/completions",
			upstreamType: auth.UpstreamOpenAIResponses,
			model:        "gpt-5.5",
			stream:       false,
			reqBody:      `{"model":"gpt-5.5","stream":false,"messages":[{"role":"user","content":"hi"}]}`,
			mockHandler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				flusher := w.(http.Flusher)
				chunk := `{"type":"response.output_text.delta","delta":"` + runawayText + `"}`
				_, _ = io.WriteString(w, "data: "+chunk+"\n\n")
				flusher.Flush()
			},
			invoke: func(h *Handler, c *gin.Context) { h.ChatCompletions(c) },
		},
		{
			name:         "Grok_Native_ChatCompletions_Stream",
			endpoint:     "/v1/chat/completions",
			routePath:    "/v1/chat/completions",
			upstreamType: auth.UpstreamGrok,
			model:        "grok-beta",
			stream:       true,
			reqBody:      `{"model":"grok-beta","stream":true,"messages":[{"role":"user","content":"hi"}]}`,
			mockHandler: func(w http.ResponseWriter, r *http.Request) {
				// C12: 区分 Native Grok chat 返回标准的 chat.completion.chunk
				w.Header().Set("Content-Type", "text/event-stream")
				flusher := w.(http.Flusher)
				chunk := `{"id":"grok-1","choices":[{"index":0,"delta":{"content":"` + runawayText + `"}}],"object":"chat.completion.chunk"}`
				_, _ = io.WriteString(w, "data: "+chunk+"\n\n")
				flusher.Flush()
				select {
				case <-r.Context().Done():
				case <-time.After(2 * time.Second):
				}
			},
			invoke: func(h *Handler, c *gin.Context) { h.ChatCompletions(c) },
		},
		{
			name:         "AnthropicMessages_Stream_SSE",
			endpoint:     "/v1/messages",
			routePath:    "/v1/messages",
			upstreamType: auth.UpstreamClaude,
			model:        "claude-3-7-sonnet",
			stream:       true,
			reqBody:      `{"model":"claude-3-7-sonnet","stream":true,"max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`,
			mockHandler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
				flusher := w.(http.Flusher)
				_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\"}}\n\n")
				_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
				flusher.Flush()
				_, _ = io.WriteString(w, "event: content_block_delta\ndata: "+`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"`+runawayText+`"}}`+"\n\n")
				flusher.Flush()
				select {
				case <-r.Context().Done():
				case <-time.After(2 * time.Second):
				}
			},
			invoke: func(h *Handler, c *gin.Context) { h.Messages(c) },
		},
		{
			name:         "AnthropicMessages_NonStream_JSON",
			endpoint:     "/v1/messages",
			routePath:    "/v1/messages",
			upstreamType: auth.UpstreamClaude,
			model:        "claude-3-7-sonnet",
			stream:       false,
			reqBody:      `{"model":"claude-3-7-sonnet","stream":false,"max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`,
			mockHandler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				resp := map[string]any{
					"id":   "msg_1",
					"type": "message",
					"role": "assistant",
					"content": []any{
						map[string]any{"type": "text", "text": runawayText},
					},
				}
				_ = json.NewEncoder(w).Encode(resp)
			},
			invoke: func(h *Handler, c *gin.Context) { h.Messages(c) },
		},
		{
			name:         "GeminiNative_StreamGenerateContent_AntigravityOAuth",
			endpoint:     "/v1beta/models/gemini-3.7-flash-low:streamGenerateContent?alt=sse",
			routePath:    "/v1beta/models/*action",
			upstreamType: auth.UpstreamAntigravity,
			model:        "gemini-3.7-flash-low",
			stream:       true,
			reqBody:      `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			mockHandler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				flusher := w.(http.Flusher)
				resp := `{"candidates":[{"content":{"role":"model","parts":[{"text":"` + runawayText + `"}]}}]}`
				_, _ = io.WriteString(w, "data: "+resp+"\n\n")
				flusher.Flush()
			},
			invoke: func(h *Handler, c *gin.Context) { h.GeminiModelsAction(c) },
		},
		{
			name:         "GeminiNative_NonStream_GenerateContent_AntigravityOAuth",
			endpoint:     "/v1beta/models/gemini-3.7-flash-low:generateContent",
			routePath:    "/v1beta/models/*action",
			upstreamType: auth.UpstreamAntigravity,
			model:        "gemini-3.7-flash-low",
			stream:       false,
			reqBody:      `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			mockHandler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				resp := map[string]any{
					"candidates": []any{
						map[string]any{
							"content": map[string]any{
								"role":  "model",
								"parts": []any{map[string]any{"text": runawayText}},
							},
						},
					},
				}
				_ = json.NewEncoder(w).Encode(resp)
			},
			invoke: func(h *Handler, c *gin.Context) { h.GeminiModelsAction(c) },
		},
	}

	for _, tc := range matrix {
		t.Run(tc.name, func(t *testing.T) {
			var upstreamCalls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamCalls.Add(1)
				tc.mockHandler(w, r)
			}))
			defer upstream.Close()

			if tc.upstreamType == auth.UpstreamAntigravity {
				previous := antigravityOAuthEndpointBases
				antigravityOAuthEndpointBases = []string{upstream.URL}
				t.Cleanup(func() { antigravityOAuthEndpointBases = previous })
			}

			handler, _, _ := setupLifecycleTestHandler(t, upstream.URL, tc.upstreamType, tc.model)

			router := gin.New()
			router.POST(tc.routePath, func(c *gin.Context) {
				tc.invoke(handler, c)
			})

			rec := &guardDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
			req := httptest.NewRequest(http.MethodPost, tc.endpoint, strings.NewReader(tc.reqBody))
			req.Header.Set("Content-Type", "application/json")
			router.ServeHTTP(rec, req)

			// 断言 1: 上游被真实调用，且由于 guard 阻断不重试，calls 严格为 1 (I1)
			if calls := upstreamCalls.Load(); calls != 1 {
				t.Fatalf("upstream calls = %d, want exactly 1 for %s", calls, tc.name)
			}

			respBody := rec.Body.String()

			// 断言 2 & 4: 逐协议精确定义终态，消除“或”弱化，严格执行“且”
			isCommittedStream := rec.Code == http.StatusOK
			if isCommittedStream {
				// 已提交头部流式：禁止正常完成终态
				if strings.Contains(respBody, "response.completed") ||
					strings.Contains(respBody, "[DONE]") ||
					strings.Contains(respBody, "message_stop") ||
					strings.Contains(respBody, `"finishReason":"STOP"`) ||
					strings.Contains(respBody, `"finish_reason":"stop"`) {
					t.Fatalf("downstream received successful completion for %s: %s", tc.name, respBody)
				}
				// 协议级终态错误验证
				if strings.Contains(tc.name, "Anthropic") {
					if !strings.Contains(respBody, "event: error") || !strings.Contains(respBody, GuardTriggeredPublicMessage) {
						t.Fatalf("Anthropic stream missing error event or guard message for %s: %s", tc.name, respBody)
					}
				} else {
					if !strings.Contains(respBody, GuardTriggeredPublicCode) || !strings.Contains(respBody, GuardTriggeredPublicMessage) {
						t.Fatalf("stream missing guard public code and message for %s: %s", tc.name, respBody)
					}
				}
			} else {
				// 未提交头部（HTTP 终态）：必须严格为 502 Bad Gateway
				if rec.Code != http.StatusBadGateway {
					t.Fatalf("expected 502 Bad Gateway for %s, got %d", tc.name, rec.Code)
				}
				if strings.Contains(tc.name, "Gemini") {
					if !strings.Contains(respBody, GuardTriggeredPublicMessage) || !strings.Contains(respBody, "502") {
						t.Fatalf("Gemini 502 missing guard message or 502 code for %s: %s", tc.name, respBody)
					}
				} else {
					if !strings.Contains(respBody, GuardTriggeredPublicCode) || !strings.Contains(respBody, GuardTriggeredPublicMessage) {
						t.Fatalf("expected 502 with guard code and message for %s: %s", tc.name, respBody)
					}
				}
			}

			// 断言 3: 失控文本下游不可见（整事件被拦截，单份 pattern 泄漏即可检出）
			if strings.Contains(respBody, pattern) {
				t.Fatalf("runaway pattern leaked to downstream client for %s", tc.name)
			}
		})
	}
}

// 场景 12 (WS 专项): 真实 Client 连接路由，真实 WebSocket turn 请求，upstream 调用严格为 1
func TestAcceptanceScenario12_WebSocketMatrix(t *testing.T) {
	gin.SetMode(gin.TestMode)

	pattern := "ws_runaway_loop_12345678_"
	runawayText := strings.Repeat(pattern, 400) // 24 * 400 = 9600B >= 9215B

	var upstreamCalls atomic.Int32
	previousExec := WebsocketExecuteFunc
	t.Cleanup(func() { WebsocketExecuteFunc = previousExec })

	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		upstreamCalls.Add(1)
		sse := `data: {"type":"response.output_text.delta","delta":"` + runawayText + `"}` + "\n\n"
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(sse)),
		}, nil
	}

	handler, store, account := setupLifecycleTestHandler(t, "https://upstream.example", "", "gpt-5.5")
	account.ResponsesUpstreamTransport = auth.OpenAIResponsesTransportHTTP
	store.SetCodexModelMapping(`{"client-ws-alias":"gpt-5.5"}`)

	router := gin.New()
	router.GET("/v1/responses", handler.ResponsesWebSocket)
	router.GET("/v1/realtime", handler.RealtimeWebSocket)

	server := httptest.NewServer(router)
	defer server.Close()

	// 1. 测试 ResponsesWebSocket
	t.Run("ResponsesWebSocket_Turn", func(t *testing.T) {
		upstreamCalls.Store(0)
		wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"
		conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			t.Fatalf("dial websocket failed: %v", err)
		}
		defer conn.Close()
		if resp != nil {
			_ = resp.Body.Close()
		}

		turnReq := `{"model":"client-ws-alias","input":"hello"}`
		if err := conn.WriteMessage(websocket.TextMessage, []byte(turnReq)); err != nil {
			t.Fatalf("write websocket turn: %v", err)
		}

		assertWSGuardTerminalThen1013(t, conn, pattern)
		if calls := upstreamCalls.Load(); calls != 1 {
			t.Fatalf("upstream calls = %d, want exactly 1", calls)
		}
	})

	// 2. 测试 RealtimeWebSocket
	t.Run("RealtimeWebSocket_TextSubprotocol", func(t *testing.T) {
		upstreamCalls.Store(0)
		wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/realtime?model=gpt-5.5"
		conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			t.Fatalf("dial realtime websocket failed: %v", err)
		}
		defer conn.Close()
		if resp != nil {
			_ = resp.Body.Close()
		}

		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, _, _ = conn.ReadMessage() // session.created

		turnReq := `{"type":"conversation.item.create","item":{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}}`
		_ = conn.WriteMessage(websocket.TextMessage, []byte(turnReq))
		_, _, _ = conn.ReadMessage() // item.created

		triggerReq := `{"type":"response.create","response":{"max_output_tokens":64}}`
		if err := conn.WriteMessage(websocket.TextMessage, []byte(triggerReq)); err != nil {
			t.Fatalf("write realtime trigger: %v", err)
		}

		assertWSGuardTerminalThen1013(t, conn, pattern)
		if calls := upstreamCalls.Load(); calls != 1 {
			t.Fatalf("upstream calls = %d, want exactly 1", calls)
		}
	})
}

// assertWSGuardTerminalThen1013 断言 WS guard 终态的行为契约：先收到一帧
// error（error.code == GuardTriggeredPublicCode），随后连接以精确的
// 1013 CloseTryAgainLater 关闭，且失控原文绝不泄漏到下游。
func assertWSGuardTerminalThen1013(t *testing.T, conn *websocket.Conn, runawayPattern string) {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	msgType, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("expected guard terminal error frame before close, got: %v", err)
	}
	if msgType != websocket.TextMessage {
		t.Fatalf("guard terminal message type = %d, want text frame: %s", msgType, string(msg))
	}
	if strings.Contains(string(msg), runawayPattern) {
		t.Fatalf("runaway pattern leaked to downstream client: %s", string(msg))
	}
	if gjson.GetBytes(msg, "type").String() != "error" ||
		gjson.GetBytes(msg, "error.code").String() != GuardTriggeredPublicCode {
		t.Fatalf("WS terminal frame missing %s: %s", GuardTriggeredPublicCode, string(msg))
	}

	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, extra, nextErr := conn.ReadMessage()
	if nextErr == nil {
		t.Fatalf("expected 1013 CloseTryAgainLater after guard terminal, got extra frame: %s", string(extra))
	}
	var closeErr *websocket.CloseError
	if !errors.As(nextErr, &closeErr) || closeErr.Code != websocket.CloseTryAgainLater {
		t.Fatalf("expected websocket close %d (CloseTryAgainLater), got %v", websocket.CloseTryAgainLater, nextErr)
	}
}

// TestResponsesWebSocketGuardTripSkipsBufferedFlush 回归 guard 命中后仍补写
// outputBuffer 的问题：命中前被输出过滤缓冲的消息不得在 guard 终态帧之后再
// flush 给客户端——终态必须是连接关闭前的最后一帧。
func TestResponsesWebSocketGuardTripSkipsBufferedFlush(t *testing.T) {
	gin.SetMode(gin.TestMode)

	pattern := "ws_buffered_runaway_12345678_"
	runawayText := strings.Repeat(pattern, 400)

	previousExec := WebsocketExecuteFunc
	t.Cleanup(func() { WebsocketExecuteFunc = previousExec })
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		sse := `data: {"type":"response.output_text.delta","delta":"buffered-before-guard"}` + "\n\n" +
			`data: {"type":"response.output_text.delta","delta":"` + runawayText + `"}` + "\n\n"
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(sse)),
		}, nil
	}

	handler, store, account := setupLifecycleTestHandler(t, "https://upstream.example", "", "gpt-5.5")
	account.ResponsesUpstreamTransport = auth.OpenAIResponsesTransportHTTP
	store.SetCodexModelMapping(`{"client-ws-alias":"gpt-5.5"}`)
	// 开启输出过滤缓冲，让 guard 命中前的合法 delta 停留在 outputBuffer 中；
	// 命中时缓冲区仍非空，才能覆盖"终态后 flush"这条路径。
	filterConfig := promptfilter.DefaultConfig()
	filterConfig.Enabled = true
	filterConfig.Mode = promptfilter.ModeBlock
	filterConfig.Advanced.Output = promptfilter.OutputConfig{
		Enabled:      true,
		BufferBytes:  1 << 20,
		OverlapBytes: 64,
		StrictOnly:   true,
	}
	store.SetPromptFilterConfig(filterConfig)

	router := gin.New()
	router.GET("/v1/responses", handler.ResponsesWebSocket)
	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket failed: %v", err)
	}
	defer conn.Close()
	if resp != nil {
		_ = resp.Body.Close()
	}

	turnReq := `{"model":"client-ws-alias","input":"hello"}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(turnReq)); err != nil {
		t.Fatalf("write websocket turn: %v", err)
	}

	assertWSGuardTerminalThen1013(t, conn, pattern)
}

// TestResponsesWebSocketGuardTripWithPendingInterruptDropsFailedFrame 回归 guard 命中
// 且存在未确认的 response.interrupt 场景：guard 终态帧必须是连接关闭前的最后一帧，
// 不得在其后补发 response.interrupt.failed 帧。
func TestResponsesWebSocketGuardTripWithPendingInterruptDropsFailedFrame(t *testing.T) {
	gin.SetMode(gin.TestMode)

	pattern := "ws_guard_interrupt_runaway_12345678_"
	runawayText := strings.Repeat(pattern, 400)

	pipeR, pipeW := io.Pipe()
	interruptReceived := make(chan struct{})

	previousExec := WebsocketExecuteFunc
	t.Cleanup(func() { WebsocketExecuteFunc = previousExec })
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		// 注册 interrupt 上游发送函数，模拟上游支持 interrupt
		RegisterResponsesWSInterrupt(ctx, func(payload []byte) error {
			close(interruptReceived)
			return nil
		})
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       pipeR,
		}, nil
	}

	handler, store, account := setupLifecycleTestHandler(t, "https://upstream.example", "", "gpt-5.5")
	account.ResponsesUpstreamTransport = auth.OpenAIResponsesTransportHTTP
	store.SetCodexModelMapping(`{"client-ws-alias":"gpt-5.5"}`)

	router := gin.New()
	router.GET("/v1/responses", handler.ResponsesWebSocket)
	server := httptest.NewServer(router)
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"
	conn, resp, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket failed: %v", err)
	}
	defer conn.Close()
	if resp != nil {
		_ = resp.Body.Close()
	}

	turnReq := `{"model":"client-ws-alias","input":"hello"}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(turnReq)); err != nil {
		t.Fatalf("write websocket turn: %v", err)
	}

	// 上游先下发 response.created 和一个正常 delta，触发首包下发并记录 response.id
	go func() {
		_, _ = pipeW.Write([]byte("data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_guard_int_test\"}}\n\n"))
		_, _ = pipeW.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"hello\"}\n\n"))
	}()

	// 消费 response.created 帧
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	msgType, msg, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read response.created: %v", err)
	}
	if msgType != websocket.TextMessage || gjson.GetBytes(msg, "type").String() != "response.created" {
		t.Fatalf("msgType=%d payload=%s, want response.created", msgType, string(msg))
	}

	// 消费 delta 帧
	_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	msgType, msg, err = conn.ReadMessage()
	if err != nil {
		t.Fatalf("read delta: %v", err)
	}
	if msgType != websocket.TextMessage || gjson.GetBytes(msg, "type").String() != "response.output_text.delta" {
		t.Fatalf("msgType=%d payload=%s, want response.output_text.delta", msgType, string(msg))
	}

	// 此时 response.id 已绑定，客户端发送 interrupt
	interruptReq := `{"type":"response.interrupt","response_id":"resp_guard_int_test","mode":"discard_partial_items"}`
	if err := conn.WriteMessage(websocket.TextMessage, []byte(interruptReq)); err != nil {
		t.Fatalf("write interrupt req: %v", err)
	}

	// 等待网关将 interrupt 分发并调用 state.send（置位 state.requested）
	select {
	case <-interruptReceived:
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for interrupt to be processed by gateway")
	}

	// 上游推送失控文本命中 guard，并且不上报 interrupt ack，模拟未决中断场景
	go func() {
		_, _ = pipeW.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"" + runawayText + "\"}\n\n"))
	}()

	// 断言：终态必须是 guard terminal 错误帧，且随后的 close 之后无任何额外帧（如 response.interrupt.failed）
	assertWSGuardTerminalThen1013(t, conn, pattern)
}

// 场景 12 (WS 并发回归): 同一连接上两个命名 lane 同时命中 guard。gorilla/websocket
// 禁止同一 conn 并发写；终态帧必须经 per-conn 写锁串行化，否则并发写会 panic、
// 产生帧损坏并让 -race 报数据竞争。两个 lane 各自收到含 GuardTriggeredPublicCode
// 的 error 帧后，连接关闭与 multiplexer worker 释放必须保持正常。
func TestAcceptanceScenario12ConcurrentLanesGuardTerminalSerialized(t *testing.T) {
	gin.SetMode(gin.TestMode)
	// 装载真实 guard 配置（Enabled + 默认阈值），复用 multip 连接测试夹具。
	setupOutputGuardHTTPTestEnv(t)

	pattern := "ws_lane_runaway_loop_12345678_"
	runawayText := strings.Repeat(pattern, 400) // 26 * 400 = 10400B >= 9215B
	started := make(chan string, 2)
	release := make(chan struct{})
	client := newResponsesWSMultiplexClient(t, func(ctx context.Context, body []byte) (*http.Response, error) {
		lane := ""
		for _, candidate := range []string{"lane-a", "lane-b"} {
			if strings.Contains(string(body), candidate) {
				lane = candidate
			}
		}
		if lane == "" {
			return nil, errors.New("unexpected synthetic guard lane: " + string(body))
		}
		started <- lane
		// 两个 upstream 在同一屏障上释放，让两条 lane 的 guard 命中与终态
		// 写尽可能重叠，从而暴露缺失的 per-conn 写串行化。
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		sse := `data: {"type":"response.output_text.delta","delta":"` + runawayText + `"}` + "\n\n"
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(sse)),
		}, nil
	})

	responsesWSMultiplexSend(t, client, "lane-a", "lane-a")
	responsesWSMultiplexWaitStart(t, started, "lane-a")
	responsesWSMultiplexSend(t, client, "lane-b", "lane-b")
	responsesWSMultiplexWaitStart(t, started, "lane-b")
	close(release)

	terminals := map[string]bool{}
	for len(terminals) < 2 {
		frame := responsesWSMultiplexRead(t, client)
		lane := gjson.GetBytes(frame, "stream_id").String()
		if lane != "lane-a" && lane != "lane-b" {
			t.Fatalf("unexpected frame on concurrent guard lanes: %s", frame)
		}
		if strings.Contains(string(frame), runawayText) {
			t.Fatalf("runaway pattern leaked on lane %s: %s", lane, frame)
		}
		if gjson.GetBytes(frame, "type").String() != "error" ||
			gjson.GetBytes(frame, "error.code").String() != GuardTriggeredPublicCode {
			t.Fatalf("lane %s guard terminal frame missing %s: %s", lane, GuardTriggeredPublicCode, frame)
		}
		if terminals[lane] {
			t.Fatalf("lane %s received a duplicate guard terminal frame", lane)
		}
		terminals[lane] = true
	}
	// 两个命名 lane 的 guard 终态是 lane 级收尾，连接保持可用；显式关闭客户端
	// 由 multiplex 夹具断言所有 handler worker 正常释放，无 panic/泄漏。
	if err := client.Close(); err != nil {
		t.Fatalf("close concurrent guard client: %v", err)
	}
}

// -----------------------------------------------------------------------------
// 场景 13: hit-before-provenance/collector/replay 真实 sink 前置拦截
// 真实观察点：真实 Responses Handler 开启 continuous retry 且上游永不成功，
// 校验真实 provenance cache 物理存储未被写入异常 sentinel
// -----------------------------------------------------------------------------
func TestAcceptanceScenario13_HitBeforeProvenanceAndReplaySink(t *testing.T) {
	gin.SetMode(gin.TestMode)

	badSentinel := "malicious_provenance_sentinel_xyz_"
	runawayPayload := strings.Repeat(badSentinel, 400) // 34 * 400 = 13600B >= 9215B

	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		// 上游输出失控 tool arguments delta
		frame := `{"type":"response.function_call_arguments.delta","call_id":"call_13","delta":"` + runawayPayload + `"}`
		_, _ = io.WriteString(w, "data: "+frame+"\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	handler, store, account := setupLifecycleTestHandler(t, upstream.URL, auth.UpstreamOpenAIResponses, "gpt-5.5")
	// 开启 continuous retry 策略，使重试逻辑活跃
	store.SetContinuousRetryPolicy(database.ContinuousRetryPolicy{
		Enabled:    true,
		Categories: []string{database.ContinuousRetryCategoryHTTP5xx, database.ContinuousRetryCategoryTransport},
	})

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":"test"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	handler.Responses(c)

	// 真实观察点：检查 compaction provenance 物理缓存中绝对不能包含恶意 sentinel 摘要
	// badDigestProbe 探针对真实聚合内容（runawayPayload）取摘要，同时保留 badSentinel 单片探针进行双重核验；
	// 注：若后续 provenance 改为对单片 chunk 记账，单片 sentinel 探针亦可即时捕捉。
	badDigestAggregated := compactionContentDigest(runawayPayload)
	raw, ok, err := handler.cache.GetRuntime(context.Background(), compactionProvenanceCacheNamespace, badDigestAggregated)
	if err != nil {
		t.Fatalf("GetRuntime failed: %v", err)
	}
	if ok || raw != nil {
		t.Fatalf("CRITICAL: provenance cache stored malicious sentinel after guard tripped: %v", raw)
	}

	badDigestSingle := compactionContentDigest(badSentinel)
	rawSingle, okSingle, errSingle := handler.cache.GetRuntime(context.Background(), compactionProvenanceCacheNamespace, badDigestSingle)
	if errSingle != nil {
		t.Fatalf("GetRuntime single sentinel failed: %v", errSingle)
	}
	if okSingle || rawSingle != nil {
		t.Fatalf("CRITICAL: provenance cache stored single malicious sentinel chunk: %v", rawSingle)
	}
	_ = account

	if calls := upstreamCalls.Load(); calls != 1 {
		t.Fatalf("continuous retry was not blocked before replay sink! calls = %d, want 1", calls)
	}
}

// -----------------------------------------------------------------------------
// 场景 14: cancel/bodyclose 无 5s usage drain，newDrainableUpstreamContext 路径立即断开，
// 类型化原因保留不被 context.Canceled (499) 或 EOF (598) 覆盖
// -----------------------------------------------------------------------------
func TestAcceptanceScenario14_ImmediateCancelNo5sDrain(t *testing.T) {
	gin.SetMode(gin.TestMode)

	pattern := "drain_bypass_pattern_"
	runawayText := strings.Repeat(pattern, 500)

	upstreamCanceled := make(chan struct{})
	var closeOnce sync.Once

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: "+`{"type":"response.output_text.delta","delta":"`+runawayText+`"}`+"\n\n")
		flusher.Flush()

		// 监测上游 context 是否在 guard 触发后立即被 cancel，而非等待 5 秒 drain
		select {
		case <-r.Context().Done():
			closeOnce.Do(func() { close(upstreamCanceled) })
		case <-time.After(3 * time.Second):
		}
	}))
	defer upstream.Close()

	handler, _, _ := setupLifecycleTestHandler(t, upstream.URL, auth.UpstreamOpenAIResponses, "gpt-5.5")

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	clientCtx, clientCancel := context.WithCancel(context.Background())
	defer clientCancel()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":"test"}`)).WithContext(clientCtx)
	c.Request.Header.Set("Content-Type", "application/json")

	start := time.Now()
	handlerDone := make(chan struct{})
	go func() {
		handler.Responses(c)
		close(handlerDone)
	}()

	select {
	case <-upstreamCanceled:
		elapsed := time.Since(start)
		if elapsed >= 2*time.Second {
			t.Fatalf("upstream cancel delayed! took %v, must not wait for 5s drain", elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("upstream context was not canceled immediately, still waiting (likely 5s drain bug)")
	}

	<-handlerDone

	// 断言：下游错误状态不被误判为 499 (Client Closed Request) 或 598 (Network Read Timeout)
	if rec.Code == 499 || rec.Code == 598 {
		t.Fatalf("guard outcome misclassified as %d", rec.Code)
	}

	// Coordinator 原语断言：HandleTripped 返回的 outcome 经过 OverlayGuardOutcome 后，guardReason 保持为 repetition
	t.Run("Coordinator_Outcome_Overlay_Typed_Reason", func(t *testing.T) {
		guard := outputguard.NewRequestGuard(outputguard.Config{
			Enabled:               true,
			MaxToolArgumentBytes:  262144,
			MaxToolArgumentDeltas: 4096,
			MaxChannelBytes:       2097152,
			MaxRequestBytes:       4194304,
		})
		_ = guard.ObserveAppend("c_14", "", outputguard.KindText, bytes.Repeat([]byte("rep_14_"), 2000))

		uCtx, uCancel := context.WithCancel(context.Background())
		defer uCancel()
		var bodyClosed atomic.Bool
		closer := &testCloser{onClose: func() { bodyClosed.Store(true) }}

		coordinator := NewGuardTerminationCoordinator(guard, uCancel, closer)
		outcome := coordinator.HandleTripped(guard.TrippedReason())

		if uCtx.Err() == nil {
			t.Fatalf("expected upstream context canceled immediately")
		}
		if !bodyClosed.Load() {
			t.Fatalf("expected upstream body closed immediately")
		}

		overlaid := OverlayGuardOutcome(outcome, guard)
		if !overlaid.guardTripped || overlaid.guardReason != "repetition" {
			t.Fatalf("typed reason was overwritten! got %+v", overlaid)
		}
		if overlaid.logStatusCode == 499 || overlaid.logStatusCode == 598 {
			t.Fatalf("guard outcome misclassified as 499 or 598: %d", overlaid.logStatusCode)
		}
	})
}

// -----------------------------------------------------------------------------
// 场景 15: 保活/自动续想与 guard 并发无旧缓冲泄露、无成功/重复终态、无 data race
// 覆盖 coalesce 与 immediate 策略、真实 downstreamMu 锁约定与 Finalize 零泄露
// -----------------------------------------------------------------------------
func TestAcceptanceScenario15_ConcurrencyKeepaliveNoRace(t *testing.T) {
	policies := []struct {
		name   string
		policy string
	}{
		{name: "CoalescePolicy", policy: StreamFlushPolicyCoalesce},
		{name: "ImmediatePolicy", policy: StreamFlushPolicyImmediate},
	}

	for _, p := range policies {
		t.Run(p.name, func(t *testing.T) {
			guard := outputguard.NewRequestGuard(outputguard.Config{
				Enabled:               true,
				MaxToolArgumentBytes:  262144,
				MaxToolArgumentDeltas: 4096,
				MaxChannelBytes:       2097152,
				MaxRequestBytes:       4194304,
			})

			var outBuf bytes.Buffer
			var downstreamMu sync.Mutex
			writer := &streamFlushWriter{
				writer:   &outBuf,
				policy:   p.policy,
				interval: 10 * time.Millisecond,
			}

			var wg sync.WaitGroup
			var stopKeepalive atomic.Bool

			// 协程 1: 保活注释 Ticker (按 downstreamMu 规范加锁保护)
			wg.Add(1)
			go func() {
				defer wg.Done()
				for !stopKeepalive.Load() {
					downstreamMu.Lock()
					if !guard.IsTripped() {
						_ = writer.WriteSSEComment("keepalive-ping")
					}
					downstreamMu.Unlock()
					time.Sleep(200 * time.Microsecond)
				}
			}()

			// 协程 2: 并发触发 guard 命中与清空缓冲
			wg.Add(1)
			go func() {
				defer wg.Done()
				// 注入失控重复文本到 buffer (模拟 coalesce 阶段积累的未 flush 数据)
				pattern := []byte("race_runaway_pattern_")
				downstreamMu.Lock()
				_ = writer.WriteBytes(pattern)

				_ = guard.ObserveAppend("c_15", "", outputguard.KindText, bytes.Repeat(pattern, 500))

				// 按生产 downstreamMu 约定锁定清空
				writer.DiscardBuffered()
				downstreamMu.Unlock()

				stopKeepalive.Store(true)
			}()

			// 协程 3: 并发读取 guard 状态
			wg.Add(1)
			go func() {
				defer wg.Done()
				for !stopKeepalive.Load() {
					_ = guard.IsTripped()
					_ = guard.TrippedReason()
					time.Sleep(100 * time.Microsecond)
				}
			}()

			wg.Wait()

			// Finalize 调用：断言 DiscardBuffered 之后不再向底层释放任何新内容，且无成功帧
			beforeLen := outBuf.Len()
			downstreamMu.Lock()
			_ = writer.Finalize()
			downstreamMu.Unlock()

			if outBuf.Len() > beforeLen {
				t.Fatalf("buffer leak! Finalize released bytes after discard: before=%d, after=%d", beforeLen, outBuf.Len())
			}

			outStr := outBuf.String()
			if strings.Contains(outStr, "response.completed") || strings.Contains(outStr, "[DONE]") {
				t.Fatalf("illegal terminal frame emitted: %s", outStr)
			}
		})
	}
}

// -----------------------------------------------------------------------------
// 场景 16: 各 ordinary/continuous select/catchall/换号/WS HTTP fallback/
// context repair/compact/continue 入口调用数 == 1 (不 >1 容忍 0)
// -----------------------------------------------------------------------------
func TestAcceptanceScenario16_RecoveryBlockedCallsExactlyOne(t *testing.T) {
	gin.SetMode(gin.TestMode)

	pattern := "retry_block_test_loop_"
	runawayText := strings.Repeat(pattern, 450) // 22 * 450 = 9900B >= 9215B

	paths := []struct {
		name     string
		policy   database.ContinuousRetryPolicy
		maxRetry int
		setup    func(h *Handler, store *auth.Store, upstreamURL string)
		trigger  func(h *Handler, upstreamURL string) *httptest.ResponseRecorder
	}{
		{
			name:     "OrdinaryRetry",
			policy:   database.ContinuousRetryPolicy{Enabled: false},
			maxRetry: 3,
			trigger: func(h *Handler, upstreamURL string) *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":"hi"}`))
				c.Request.Header.Set("Content-Type", "application/json")
				h.Responses(c)
				return rec
			},
		},
		{
			name: "SelectiveContinuousRetry",
			policy: database.ContinuousRetryPolicy{
				Enabled:    true,
				Categories: []string{database.ContinuousRetryCategoryHTTP5xx, database.ContinuousRetryCategoryTransport},
			},
			maxRetry: 3,
			trigger: func(h *Handler, upstreamURL string) *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":"hi"}`))
				c.Request.Header.Set("Content-Type", "application/json")
				h.Responses(c)
				return rec
			},
		},
		{
			name: "CatchAllContinuousRetry",
			policy: database.ContinuousRetryPolicy{
				Enabled:  true,
				CatchAll: true,
			},
			maxRetry: 3,
			trigger: func(h *Handler, upstreamURL string) *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":"hi"}`))
				c.Request.Header.Set("Content-Type", "application/json")
				h.Responses(c)
				return rec
			},
		},
		{
			name: "AccountRotation",
			policy: database.ContinuousRetryPolicy{
				Enabled:  true,
				CatchAll: true,
			},
			maxRetry: 2,
			setup: func(h *Handler, store *auth.Store, upstreamURL string) {
				// 添加第二个候选账号供轮换
				store.AddAccount(&auth.Account{
					DBID:         202,
					UpstreamType: auth.UpstreamOpenAIResponses,
					Status:       auth.StatusReady,
					BaseURL:      upstreamURL,
					APIKey:       "sk-recovery-rotate-2",
					Models:       []string{"gpt-5.5"},
				})
			},
			trigger: func(h *Handler, upstreamURL string) *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":"hi"}`))
				c.Request.Header.Set("Content-Type", "application/json")
				h.Responses(c)
				return rec
			},
		},
		{
			name:     "ContextRepair",
			policy:   database.ContinuousRetryPolicy{Enabled: false},
			maxRetry: 2,
			trigger: func(h *Handler, upstreamURL string) *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				// 携带 previous_response_id 模拟上下文修复链路
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":true,"previous_response_id":"resp_prev_123","input":"continue"}`))
				c.Request.Header.Set("Content-Type", "application/json")
				h.Responses(c)
				return rec
			},
		},
		{
			name:     "AutoCompact",
			policy:   database.ContinuousRetryPolicy{Enabled: false},
			maxRetry: 2,
			trigger: func(h *Handler, upstreamURL string) *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				// 模拟 compact 请求
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":[{"type":"compaction","encrypted_content":"enc_state"}]}`))
				c.Request.Header.Set("Content-Type", "application/json")
				h.Responses(c)
				return rec
			},
		},
		{
			name:     "AutoContinue",
			policy:   database.ContinuousRetryPolicy{Enabled: false},
			maxRetry: 2,
			trigger: func(h *Handler, upstreamURL string) *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":"auto-continue-token"}`))
				c.Request.Header.Set("Content-Type", "application/json")
				h.Responses(c)
				return rec
			},
		},
	}

	for _, tc := range paths {
		t.Run(tc.name, func(t *testing.T) {
			var upstreamCalls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamCalls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				flusher := w.(http.Flusher)
				_, _ = io.WriteString(w, "data: "+`{"type":"response.output_text.delta","delta":"`+runawayText+`"}`+"\n\n")
				flusher.Flush()
			}))
			defer upstream.Close()

			handler, store, _ := setupLifecycleTestHandler(t, upstream.URL, auth.UpstreamOpenAIResponses, "gpt-5.5")
			store.SetContinuousRetryPolicy(tc.policy)
			if tc.setup != nil {
				tc.setup(handler, store, upstream.URL)
			}

			rec := tc.trigger(handler, upstream.URL)

			calls := upstreamCalls.Load()
			if calls != 1 {
				t.Fatalf("path %s executed retry or failed to call upstream! upstreamCalls = %d, want exactly 1 (status=%d, body=%s)",
					tc.name, calls, rec.Code, rec.Body.String())
			}
		})
	}
}

// -----------------------------------------------------------------------------
// 场景 17: HTTP pre 502 / post 协议 envelope (Chat 无 DONE/Anthropic 无 stop/Gemini 裸 JSON/WS 租约废弃)
// -----------------------------------------------------------------------------
func TestAcceptanceScenario17_ProtocolTerminalEnvelopes(t *testing.T) {
	// 1. 头未提交时：HTTP 502 且携带标准 APIError 结构
	rec502 := httptest.NewRecorder()
	c502, _ := gin.CreateTestContext(rec502)
	c502.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	var mu502 sync.Mutex
	clientGone502 := false

	err := SendDownstreamGuardTerminalHTTP(c502, &mu502, &clientGone502, "chat", "repetition")
	if err != nil {
		t.Fatalf("SendDownstreamGuardTerminalHTTP error: %v", err)
	}
	if rec502.Code != http.StatusBadGateway {
		t.Fatalf("expected 502 Bad Gateway for uncommitted error, got %d", rec502.Code)
	}
	if !strings.Contains(rec502.Body.String(), GuardTriggeredPublicCode) {
		t.Fatalf("body missing %s: %s", GuardTriggeredPublicCode, rec502.Body.String())
	}

	// 2. Chat SSE 真实流式已提交场景：严禁带有 [DONE] 或 finish_reason: "stop"
	t.Run("Chat_Committed_Stream_No_DONE", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			runaway := strings.Repeat("chat_runaway_repeat_", 500)
			_, _ = io.WriteString(w, "data: "+`{"type":"response.output_text.delta","delta":"`+runaway+`"}`+"\n\n")
			flusher.Flush()
		}))
		defer upstream.Close()

		handler, _, _ := setupLifecycleTestHandler(t, upstream.URL, auth.UpstreamOpenAIResponses, "gpt-5.5")
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-5.5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		c.Request.Header.Set("Content-Type", "application/json")

		handler.ChatCompletions(c)
		chatStr := rec.Body.String()
		if strings.Contains(chatStr, "[DONE]") {
			t.Fatalf("Chat stream terminal contains illegal [DONE]: %s", chatStr)
		}
		if strings.Contains(chatStr, `"stop"`) {
			t.Fatalf("Chat stream terminal contains illegal stop finish_reason: %s", chatStr)
		}
	})

	// 3. Anthropic Messages 真实流式已提交场景：严禁带有 message_stop
	t.Run("Anthropic_Committed_Stream_No_MessageStop", func(t *testing.T) {
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)
			runaway := strings.Repeat("claude_runaway_repeat_", 500)
			_, _ = io.WriteString(w, "event: content_block_delta\ndata: "+`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"`+runaway+`"}}`+"\n\n")
			flusher.Flush()
		}))
		defer upstream.Close()

		handler, _, _ := setupLifecycleTestHandler(t, upstream.URL, auth.UpstreamClaude, "claude-3-7-sonnet")
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-3-7-sonnet","stream":true,"max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`))
		c.Request.Header.Set("Content-Type", "application/json")

		handler.Messages(c)
		anthropicStr := rec.Body.String()
		if strings.Contains(anthropicStr, "message_stop") {
			t.Fatalf("Anthropic stream terminal contains illegal message_stop: %s", anthropicStr)
		}
	})

	// 4. WebSocket SendDownstreamGuardTerminalWS 实际连接与 1013 Close 控制帧
	// 注：并发环境下的 WS Lease 隔离与废弃已由场景 20 (Concurrent_WS_Lease_Isolation) 完整覆盖
	t.Run("WS_Terminal_Frame", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			upgrader := websocket.Upgrader{}
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()

			_ = SendDownstreamGuardTerminalWS(conn, "stream_test_17", "repetition")
			// 发送 guard 终态帧后，以 1013 CloseTryAgainLater 控制帧关闭连接
			_ = conn.WriteControl(
				websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseTryAgainLater, GuardTriggeredPublicMessage),
				time.Now().Add(time.Second),
			)
		}))
		defer server.Close()

		wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
		conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
		if err != nil {
			t.Fatalf("dial WS error: %v", err)
		}
		defer conn.Close()

		// 断言 1: 必须读取到 error 帧且包含公开的 GuardTriggeredPublicCode
		msgType, msg, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("expected error frame before close, got: %v", err)
		}
		if msgType != websocket.TextMessage || !strings.Contains(string(msg), GuardTriggeredPublicCode) {
			t.Fatalf("WS terminal frame missing %s: %s", GuardTriggeredPublicCode, string(msg))
		}

		// 断言 2: 随后必须接收到 1013 (CloseTryAgainLater) 关闭控制帧（且关系，严禁弱化）
		_, _, nextErr := conn.ReadMessage()
		if nextErr == nil {
			t.Fatalf("expected connection closed with 1013 CloseTryAgainLater, got nil error")
		}
		var closeErr *websocket.CloseError
		if !errors.As(nextErr, &closeErr) || closeErr.Code != websocket.CloseTryAgainLater {
			t.Fatalf("expected websocket 1013 CloseTryAgainLater, got %v", nextErr)
		}
	})

	// 5. Envelope 基础原语格式验证
	t.Run("Format_Envelope_Primitives", func(t *testing.T) {
		chatEnv, _, err := FormatGuardTerminalEnvelope("chat", "")
		if err != nil || strings.Contains(string(chatEnv), "[DONE]") || !strings.Contains(string(chatEnv), GuardTriggeredPublicCode) {
			t.Fatalf("invalid chat envelope: %s err=%v", string(chatEnv), err)
		}
		anthropicEnv, _, err := FormatGuardTerminalEnvelope("anthropic", "")
		if err != nil || strings.Contains(string(anthropicEnv), "message_stop") || !strings.Contains(string(anthropicEnv), GuardTriggeredPublicMessage) {
			t.Fatalf("invalid anthropic envelope: %s err=%v", string(anthropicEnv), err)
		}
		geminiEnv, _, err := FormatGuardTerminalEnvelope("gemini", "")
		if err != nil || !gjson.Valid(string(geminiEnv)) || !strings.Contains(string(geminiEnv), GuardTriggeredPublicMessage) {
			t.Fatalf("invalid gemini envelope: %s err=%v", string(geminiEnv), err)
		}
	})
}

// -----------------------------------------------------------------------------
// 场景 18: TryLock 已 commit 锁占无追加 bytes/clientGone、SetWriteDeadline(5s) 正断言、
// unsupportedWriter 无 Write 并退出
// -----------------------------------------------------------------------------
func TestAcceptanceScenario18_TryLockAndWriteDeadlines(t *testing.T) {
	// 1. 锁争用（TryLock 失败）：立即安全放弃，标记 clientGone，不产生孤儿 goroutine 阻塞
	var downstreamMu sync.Mutex
	downstreamMu.Lock() // 人为占用锁
	clientGone := false

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Writer.WriteHeaderNow() // 模拟已 commit
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

	done := make(chan error, 1)
	go func() {
		done <- SendDownstreamGuardTerminalHTTP(c, &downstreamMu, &clientGone, "responses", "repetition")
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("expected error on lock contention, got nil")
		}
		if rec.Body.Len() > 0 {
			t.Fatalf("wrote bytes while lock was held by another goroutine!")
		}
		if !clientGone {
			t.Fatalf("expected clientGone = true on lock contention")
		}
	case <-time.After(150 * time.Millisecond):
		t.Fatalf("SendDownstreamGuardTerminalHTTP hung on contended lock! Orphan goroutine detected!")
	}
	downstreamMu.Unlock()

	// 2. SetWriteDeadline(5s) 正断言与写入成功测试
	t.Run("SetWriteDeadline_5s_Assertion", func(t *testing.T) {
		rawRecorder := httptest.NewRecorder()
		spy := &spyResponseControllerWriter{ResponseWriter: rawRecorder}
		cSpy, _ := gin.CreateTestContext(rawRecorder)
		cSpy.Writer = &customGinResponseWriter{ResponseWriter: spy}
		cSpy.Writer.WriteHeaderNow() // 模拟已 commit
		cSpy.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

		var mu sync.Mutex
		clientGoneSpy := false
		err := SendDownstreamGuardTerminalHTTP(cSpy, &mu, &clientGoneSpy, "responses", "repetition")
		if err != nil {
			t.Fatalf("SendDownstreamGuardTerminalHTTP with spy error: %v", err)
		}
		if !spy.deadlineSet.Load() {
			t.Fatalf("expected SetWriteDeadline(5s) was called, but was not")
		}
		lastDeadline := time.Unix(0, spy.lastDeadline.Load())
		expectedDeadline := time.Now().Add(5 * time.Second)
		diff := lastDeadline.Sub(expectedDeadline)
		if diff < -2*time.Second || diff > 2*time.Second {
			t.Fatalf("expected SetWriteDeadline ~5s in the future (within ±2s), got %v (diff %v)", lastDeadline, diff)
		}
		if !spy.writeCalled.Load() {
			t.Fatalf("expected terminal bytes written, but Write was not called")
		}
	})

	// 3. unsupportedWriter: 安全退出，严禁无界写入，严禁 nil err 通过
	t.Run("Unsupported_Writer_No_Write", func(t *testing.T) {
		unsupportedWriter := &customNoDeadlineWriter{header: make(http.Header)}
		recUnsupported := httptest.NewRecorder()
		cUnsup, _ := gin.CreateTestContext(recUnsupported)
		cUnsup.Writer = &customGinResponseWriter{ResponseWriter: unsupportedWriter}
		cUnsup.Writer.WriteHeaderNow() // 模拟已 commit
		cUnsup.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)

		var muUnsup sync.Mutex
		clientGoneUnsup := false
		errUnsup := SendDownstreamGuardTerminalHTTP(cUnsup, &muUnsup, &clientGoneUnsup, "responses", "repetition")
		// I7: unsupported nil err 不能 pass，必须返回明确的错误或 http.ErrNotSupported
		if errUnsup == nil {
			t.Fatalf("unsupported writer must return non-nil error, got nil")
		}
		if !errors.Is(errUnsup, http.ErrNotSupported) && !strings.Contains(errUnsup.Error(), "not supported") {
			t.Fatalf("unexpected error on unsupported writer: %v", errUnsup)
		}
		if unsupportedWriter.writeCount > 0 {
			t.Fatalf("unsupported writer performed unsafe unbounded write! writeCount = %d", unsupportedWriter.writeCount)
		}
	})
}

// -----------------------------------------------------------------------------
// 场景 19: 真实 accountReportFailure / store health / cache / usage 检查
// 端到端真实 Responses 请求验证：guard 命中不产生惩罚（零惩罚），无虚假成功缓存或虚构 token；
// 对照组构造非 guard 上游失败，证明 FailureStreak/LastFailureKind/LastFailureAt 观察点有效。
// -----------------------------------------------------------------------------
func TestAcceptanceScenario19_ZeroPenaltyAndNoCacheUsageFabrication(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// 周期 30 字节重复循环 400 次 = 12000B >= 9215B，触发 repetition guard
	pattern := "runaway_cycle_30_bytes_123456_"
	runawayText := strings.Repeat(pattern, 400)

	var upstreamFailWith5xx atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if upstreamFailWith5xx.Load() {
			// 对照组：模拟上游 500 内部错误
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":{"message":"Internal upstream crash"}}`)
			return
		}
		// 实验组：上游输出失控流
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: "+`{"type":"response.output_text.delta","delta":"`+runawayText+`"}`+"\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	handler, store, account := setupLifecycleTestHandler(t, upstream.URL, auth.UpstreamOpenAIResponses, "gpt-5.5")

	// 1. 实验组：发起真实 Responses 流式请求，使 guard 真实命中
	recGuard := httptest.NewRecorder()
	cGuard, _ := gin.CreateTestContext(recGuard)
	cGuard.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":"guard_test"}`))
	cGuard.Request.Header.Set("Content-Type", "application/json")

	handler.Responses(cGuard)

	// 断言 guard 确已触发：必须是 502 终态且携带公开码（且关系，严禁弱化）
	if recGuard.Code != http.StatusBadGateway || !strings.Contains(recGuard.Body.String(), GuardTriggeredPublicCode) {
		t.Fatalf("expected guard triggered in experiment group, got code=%d body=%s", recGuard.Code, recGuard.Body.String())
	}

	// 真实观察点断言：该账号绝对未被施加任何失败惩罚
	account.Mu().RLock()
	streak := account.FailureStreak
	lastKind := account.LastFailureKind
	lastAt := account.LastFailureAt
	healthTier := account.HealthTier
	account.Mu().RUnlock()

	if streak != 0 {
		t.Fatalf("expected FailureStreak == 0 on guard hit, got %d", streak)
	}
	if lastKind != "" {
		t.Fatalf("expected LastFailureKind == \"\" on guard hit, got %q", lastKind)
	}
	if !lastAt.IsZero() {
		t.Fatalf("expected LastFailureAt to be zero on guard hit, got %v", lastAt)
	}
	if healthTier != auth.HealthTierHealthy {
		t.Fatalf("expected HealthTier to remain Healthy on guard hit, got %v", healthTier)
	}

	// 2. 真实 DB Usage 与 Cache 检查：在真实命中后执行
	if handler.db != nil {
		handler.db.FlushUsageLogs()
		logs, err := handler.db.ListUsageLogsByFilter(context.Background(), database.UsageLogFilter{
			Start: time.Now().Add(-time.Minute), End: time.Now().Add(time.Minute), IncludeCanceled: true,
		})
		if err == nil {
			for _, logEntry := range logs {
				if logEntry.StatusCode == http.StatusOK {
					t.Fatalf("found successful usage log for tripped guard stream: %+v", logEntry)
				}
				if logEntry.InputTokens > 0 && logEntry.OutputTokens > 100000 {
					t.Fatalf("fabricated usage tokens detected: %+v", logEntry)
				}
			}
		}
	}

	// 3. 对照组：构造一次非 guard 的真实流/上游失败（上游 5xx），断言观察点字段确会被写入，防伪断言
	upstreamFailWith5xx.Store(true)

	recControl := httptest.NewRecorder()
	cControl, _ := gin.CreateTestContext(recControl)
	cControl.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":"control_test"}`))
	cControl.Request.Header.Set("Content-Type", "application/json")

	handler.Responses(cControl)

	account.Mu().RLock()
	controlStreak := account.FailureStreak
	controlKind := account.LastFailureKind
	controlAt := account.LastFailureAt
	account.Mu().RUnlock()

	if controlStreak == 0 {
		t.Fatalf("control group expected non-zero FailureStreak for upstream failure, got 0 (fake assertion check failed)")
	}
	if controlKind == "" {
		t.Fatalf("control group expected non-empty LastFailureKind for upstream failure, got empty (fake assertion check failed)")
	}
	if controlAt.IsZero() {
		t.Fatalf("control group expected non-zero LastFailureAt for upstream failure, got zero time (fake assertion check failed)")
	}

	_ = store
}

// -----------------------------------------------------------------------------
// 场景 20: 并发请求与 WS Lease 强隔离
// -----------------------------------------------------------------------------
func TestAcceptanceScenario20_ConcurrentRequestsAndLeaseIsolation(t *testing.T) {
	gin.SetMode(gin.TestMode)

	// C9: 周期 18B 循环 600 次 = 10800B >= 9215B，确保触发 repetition 且不先触发其它 budget
	pattern := "abnormal_loop_xyz_"
	runawayText := strings.Repeat(pattern, 600)

	// Mock Upstream 针对不同请求参数返回正常与异常数据
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		if strings.Contains(string(body), "runaway") {
			// 异常请求返回失控流
			_, _ = io.WriteString(w, "data: "+`{"type":"response.output_text.delta","delta":"`+runawayText+`"}`+"\n\n")
		} else {
			// 正常请求返回成功流
			_, _ = io.WriteString(w, "data: "+`{"type":"response.output_text.delta","delta":"Valid output"}`+"\n\n")
			_, _ = io.WriteString(w, "data: "+`{"type":"response.completed","response":{"id":"resp_ok","status":"completed"}}`+"\n\n")
		}
		flusher.Flush()
	}))
	defer upstream.Close()

	handler, store, _ := setupLifecycleTestHandler(t, upstream.URL, auth.UpstreamOpenAIResponses, "gpt-5.5")
	store.SetCodexModelMapping(`{"client-ws-alias":"gpt-5.5"}`)

	router := gin.New()
	router.POST("/v1/responses", handler.Responses)
	router.GET("/v1/responses", handler.ResponsesWebSocket)
	server := httptest.NewServer(router)
	defer server.Close()

	// 1. 并发 HTTP 请求强隔离测试
	t.Run("Concurrent_HTTP_Isolation", func(t *testing.T) {
		var wg sync.WaitGroup
		recNormal := httptest.NewRecorder()
		recAbnormal := httptest.NewRecorder()

		// 并发执行正常请求
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, _ := gin.CreateTestContext(recNormal)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":"normal"}`))
			c.Request.Header.Set("Content-Type", "application/json")
			handler.Responses(c)
		}()

		// 并发执行异常请求
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, _ := gin.CreateTestContext(recAbnormal)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":"runaway"}`))
			c.Request.Header.Set("Content-Type", "application/json")
			handler.Responses(c)
		}()

		wg.Wait()

		// 断言 1: 异常请求被准确拦截
		abnormalBody := recAbnormal.Body.String()
		if !strings.Contains(abnormalBody, GuardTriggeredPublicCode) && recAbnormal.Code != http.StatusBadGateway {
			t.Fatalf("abnormal request was not tripped, got code=%d body=%s", recAbnormal.Code, abnormalBody)
		}

		// 断言 2: 正常请求完全不受影响，成功输出并完成
		normalBody := recNormal.Body.String()
		if strings.Contains(normalBody, GuardTriggeredPublicCode) {
			t.Fatalf("isolation failure! normal request was falsely tripped by concurrent abnormal request")
		}
		if !strings.Contains(normalBody, "Valid output") {
			t.Fatalf("normal request missing expected output: %s", normalBody)
		}
	})

	// 2. 并发 WebSocket 租约与连接强隔离测试：正常 WS 租约返还，异常 WS 租约废弃
	t.Run("Concurrent_WS_Lease_Isolation", func(t *testing.T) {
		wsAccount := &auth.Account{
			DBID:         102,
			UpstreamType: "",
			Status:       auth.StatusReady,
			HealthTier:   auth.HealthTierHealthy,
			AccessToken:  "at-lifecycle-ws-102",
			PlanType:     "plus",
			AccountID:    "acct-lifecycle-102",
			Models:       []string{"gpt-5.5"},
		}
		store.AddAccount(wsAccount)
		t.Cleanup(func() { store.RemoveAccount(wsAccount.DBID) })

		previousExec := WebsocketExecuteFunc
		t.Cleanup(func() { WebsocketExecuteFunc = previousExec })

		WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
			if strings.Contains(string(requestBody), "runaway") {
				sse := `data: {"type":"response.output_text.delta","delta":"` + runawayText + `"}` + "\n\n"
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(sse)),
				}, nil
			}
			sse := `data: {"type":"response.output_text.delta","delta":"Normal WS output"}` + "\n\n" +
				`data: {"type":"response.completed","response":{"id":"resp_ws_ok","status":"completed"}}` + "\n\n"
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(sse)),
			}, nil
		}

		wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"

		var wg sync.WaitGroup
		var normalMsg string
		var abnormalClosed bool

		// 并发 WS 正常会话
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"model":"client-ws-alias","input":"normal"}`))
			_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			_, msg, _ := conn.ReadMessage()
			normalMsg = string(msg)
		}()

		// 并发 WS 异常会话
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"model":"client-ws-alias","input":"runaway"}`))
			_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			_, msg, err := conn.ReadMessage()
			if err != nil && (websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.ClosePolicyViolation, websocket.CloseInternalServerErr) || errors.Is(err, io.EOF)) {
				abnormalClosed = true
			} else if strings.Contains(string(msg), GuardTriggeredPublicCode) {
				// 读取到 error 帧后，等待收到 close 控制帧或连接断开
				_, _, nextErr := conn.ReadMessage()
				if nextErr != nil {
					abnormalClosed = true
				}
			}
		}()

		wg.Wait()

		if !strings.Contains(normalMsg, "Normal WS output") {
			t.Fatalf("normal WS session failed to deliver output: %s", normalMsg)
		}
		if !abnormalClosed {
			t.Fatalf("expected abnormal WS lease to be discarded and connection closed, but was not")
		}
	})
}

// -----------------------------------------------------------------------------
// 辅助类型定义 (包含完整的 gin.ResponseWriter 实现)
// -----------------------------------------------------------------------------

type testCloser struct {
	onClose func()
}

func (c *testCloser) Close() error {
	if c.onClose != nil {
		c.onClose()
	}
	return nil
}

type customNoDeadlineWriter struct {
	header     http.Header
	writeCount int
}

func (w *customNoDeadlineWriter) Header() http.Header         { return w.header }
func (w *customNoDeadlineWriter) Write(b []byte) (int, error) { w.writeCount++; return len(b), nil }
func (w *customNoDeadlineWriter) WriteHeader(statusCode int)  {}

type customGinResponseWriter struct {
	http.ResponseWriter
}

func (w *customGinResponseWriter) CloseNotify() <-chan bool { return nil }
func (w *customGinResponseWriter) Flush()                   {}
func (w *customGinResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	var conn net.Conn
	var rw *bufio.ReadWriter
	return conn, rw, errors.New("hijack unsupported")
}
func (w *customGinResponseWriter) Size() int                         { return 0 }
func (w *customGinResponseWriter) Status() int                       { return 200 }
func (w *customGinResponseWriter) WriteHeaderNow()                   {}
func (w *customGinResponseWriter) Written() bool                     { return true }
func (w *customGinResponseWriter) WriteString(s string) (int, error) { return w.Write([]byte(s)) }
func (w *customGinResponseWriter) Pusher() http.Pusher               { return nil }
func (w *customGinResponseWriter) Unwrap() http.ResponseWriter       { return w.ResponseWriter }

var _ gin.ResponseWriter = (*customGinResponseWriter)(nil)

type spyResponseControllerWriter struct {
	http.ResponseWriter
	deadlineSet    atomic.Bool
	lastDeadline   atomic.Int64
	writtenNanos   atomic.Int64
	writeCalled    atomic.Bool
	lockAttemptAt  atomic.Int64
	cancelNoticeAt atomic.Int64
}

func (s *spyResponseControllerWriter) SetWriteDeadline(deadline time.Time) error {
	if !deadline.IsZero() {
		s.deadlineSet.Store(true)
		s.lastDeadline.Store(deadline.UnixNano())
	}
	return nil
}

func (s *spyResponseControllerWriter) Write(b []byte) (int, error) {
	s.writeCalled.Store(true)
	s.writtenNanos.Store(time.Now().UnixNano())
	return s.ResponseWriter.Write(b)
}

func (s *spyResponseControllerWriter) WriteHeader(code int) {
	s.ResponseWriter.WriteHeader(code)
}

func (s *spyResponseControllerWriter) Header() http.Header {
	return s.ResponseWriter.Header()
}

func (s *spyResponseControllerWriter) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}
