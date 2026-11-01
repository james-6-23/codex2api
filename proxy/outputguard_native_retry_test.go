package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// newNativeGuardRetryGrokStore 构造一个真实 Grok 原生账号的 store，沿用与
// continuous_retry_local_failure_test.go 相同的路由夹具，使 /v1/messages 真正
// 命中 Grok 原生协议分支（而不是 Codex 翻译链路）。
func newNativeGuardRetryGrokStore(t *testing.T, upstreamURL string, protocol GrokProtocol) *auth.Store {
	t.Helper()
	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency: 1, TestConcurrency: 1, TestModel: "grok-4.5",
		MaxRetries: 0, MaxRateLimitRetries: 0,
	})
	t.Cleanup(store.Stop)
	baseURL := strings.TrimRight(upstreamURL, "/") + "/v1"
	account := &auth.Account{
		DBID: 1, UpstreamType: auth.UpstreamGrok, APIKey: "test-grok-key", BaseURL: baseURL,
		Models: []string{"grok-4.5"}, PlanType: "api",
	}
	account.SetGrokRoutingState(auth.GrokRoutingState{
		Models: []auth.GrokModelRoute{{
			ModelID: "grok-4.5", BaseURL: baseURL, APIBackend: protocol,
		}},
		Capabilities: []auth.GrokProtocolCapability{{
			ModelID: "grok-4.5", Origin: baseURL, Protocol: protocol,
			Status: auth.GrokCapabilityOK, ObservedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
		}},
	})
	store.AddAccount(account)
	return store
}

// TestNativeHTTP_GrokMessagesGuardBudgetInheritedAcrossAttempts 验证 Grok 原生
// Messages 路径在既有重试语义下跨 attempt 继承 Output Guard 预算：第一个
// attempt 已消耗但未触发的字节必须累计到第二个 attempt（请求级语义字节跨 attempt
// 累积超出 MaxRequestBytes），否则每次 attempt 重建状态机会把跨 attempt 的失控输出放行。
// 上游调用次数固定为「首个可重试断流 + 一次被 Guard 终止的 attempt」，Guard 命中后不得再引入透明重试。
func TestNativeHTTP_GrokMessagesGuardBudgetInheritedAcrossAttempts(t *testing.T) {
	setupOutputGuardHTTPTestEnv(t)
	enableCatchAllContinuousRetry(t)

	// 每个 block 70KiB，单独低于 128KiB channel 上限。
	// Attempt 1 发送 block 0 与 block 1 共 140KiB（低于 256KiB request 上限）。
	// Attempt 2 发送 block 0（70KiB）+ block 1（70KiB），累计请求字节达 280KiB，触发 request_bytes。
	perBlock := guardHTTPTestMaxChannelBytes/2 + 6*1024
	chunkA := outputGuardTestRandomText(101, int(perBlock))
	chunkB := outputGuardTestRandomText(202, int(perBlock))

	var upstreamCalls atomic.Int32
	secondAttemptCanceled := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_native_guard_attempt\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[]}}\n\n")

		writeBlock := func(index int, text string) {
			pStart, _ := json.Marshal(gin.H{
				"type":  "content_block_start",
				"index": index,
				"content_block": gin.H{
					"type": "text",
					"text": "",
				},
			})
			_, _ = io.WriteString(w, "event: content_block_start\ndata: "+string(pStart)+"\n\n")
			pDelta, _ := json.Marshal(gin.H{
				"type":  "content_block_delta",
				"index": index,
				"delta": gin.H{"type": "text_delta", "text": text},
			})
			_, _ = io.WriteString(w, "event: content_block_delta\ndata: "+string(pDelta)+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
		}

		writeBlock(0, chunkA)
		writeBlock(1, chunkB)

		if call == 1 {
			// 无 message_stop 的 EOF：既有语义下可重试的上游断流。
			return
		}
		// 共享预算未生效时第二 attempt 会正常完成；共享生效时 Guard 会在读取
		// 上面的大 delta 时终止本次 attempt，下面的成功终态不会到达。
		select {
		case <-r.Context().Done():
			secondAttemptCanceled <- struct{}{}
			return
		case <-time.After(200 * time.Millisecond):
		}
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
			secondAttemptCanceled <- struct{}{}
		case <-time.After(2 * time.Second):
		}
	}))
	t.Cleanup(upstream.Close)

	store := newNativeGuardRetryGrokStore(t, upstream.URL, GrokProtocolMessages)
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	handler.SetRuntimeCache(cache.NewMemory(1))

	requestBody := []byte(`{"model":"grok-4.5","max_tokens":128,"messages":[{"role":"user","content":"hello"}],"stream":true}`)
	recorder := &guardDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(requestBody))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Request.Header.Set("X-Codex2API-Affinity-Key", "native-guard-cross-attempt")
	affinityKey := sessionAffinityKey(resolveRequestSessionIdentity(ctx.Request.Header, requestBody).affinityID, 0)
	codexTurnStateOrigins.Delete(affinityKey)
	t.Cleanup(func() { codexTurnStateOrigins.Delete(affinityKey) })

	handler.Messages(ctx)

	body := recorder.Body.String()
	if got := upstreamCalls.Load(); got != 2 {
		t.Fatalf("expected 2 upstream calls (retryable stream break + guarded attempt), got %d; body=%s", got, outputGuardTestBodySummary(body))
	}
	// 重试等待期间 Messages keepalive 已提交 SSE 200 头，因此 Guard 终态按既有
	// 语义以 anthropic error envelope 下发；关键是它必须是 Guard 终态而不是
	// 伪造的 message_stop / 成功完成态。
	if !strings.Contains(body, "event: error") || !strings.Contains(body, GuardTriggeredPublicMessage) {
		t.Fatalf("expected the sanitized anthropic guard terminal, got: %s", outputGuardTestBodySummary(body))
	}
	for _, leaked := range []string{chunkA[:32], chunkB[:32]} {
		if strings.Contains(body, leaked) {
			t.Fatalf("the tripped model output leaked downstream: %s", outputGuardTestBodySummary(body))
		}
	}
	if strings.Contains(body, "event: message_stop") || strings.Contains(body, "content_block_stop") {
		t.Fatalf("guard terminal must not be followed by a forged completion frame: %s", outputGuardTestBodySummary(body))
	}
	select {
	case <-secondAttemptCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("expected the guarded attempt to be canceled upstream")
	}
}

// TestNativeHTTP_GrokMessagesGuardScopeIsolatesChannelsAcrossAttempts 验证跨 attempt
// 的同名 block ID 经过 attempt 作用域隔离后，不会发生通道类型复用或绕过工具参数限额：
// Attempt 1 以 text 通道（上限 128KiB）消费 50KiB 后断流，Attempt 2 将同名 block 0
// 作为 tool_use 并下发 20KiB（超过 16KiB 工具参数上限但低于 128KiB text 上限）。
// 修复后 Attempt 2 必须在 20KiB 处被安全拦截，而不是复用 Attempt 1 的 text 通道放行。
func TestNativeHTTP_GrokMessagesGuardScopeIsolatesChannelsAcrossAttempts(t *testing.T) {
	setupOutputGuardHTTPTestEnv(t)
	enableCatchAllContinuousRetry(t)

	textChunk := outputGuardTestRandomText(101, 50*1024)
	toolChunk := outputGuardTestRandomText(202, 20*1024)

	var upstreamCalls atomic.Int32
	secondAttemptCanceled := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_scope_guard_attempt\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[]}}\n\n")

		if call == 1 {
			// Attempt 1: block 0 作为 text 通道下发 50KiB，之后模拟断流触发重试
			payload, _ := json.Marshal(gin.H{
				"type":  "content_block_start",
				"index": 0,
				"content_block": gin.H{
					"type": "text",
					"text": "",
				},
			})
			_, _ = io.WriteString(w, "event: content_block_start\ndata: "+string(payload)+"\n\n")
			payload, _ = json.Marshal(gin.H{
				"type":  "content_block_delta",
				"index": 0,
				"delta": gin.H{"type": "text_delta", "text": textChunk},
			})
			_, _ = io.WriteString(w, "event: content_block_delta\ndata: "+string(payload)+"\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			return
		}

		// Attempt 2: 同样的 block 0 作为 tool_use 下发 20KiB (> 16KiB tool limit)
		payload, _ := json.Marshal(gin.H{
			"type":  "content_block_start",
			"index": 0,
			"content_block": gin.H{
				"type": "tool_use",
				"id":   "toolu_scope_01",
				"name": "exec",
			},
		})
		_, _ = io.WriteString(w, "event: content_block_start\ndata: "+string(payload)+"\n\n")
		payload, _ = json.Marshal(gin.H{
			"type":  "content_block_delta",
			"index": 0,
			"delta": gin.H{"type": "input_json_delta", "partial_json": toolChunk},
		})
		_, _ = io.WriteString(w, "event: content_block_delta\ndata: "+string(payload)+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}

		select {
		case <-r.Context().Done():
			secondAttemptCanceled <- struct{}{}
			return
		case <-time.After(200 * time.Millisecond):
		}
		_, _ = io.WriteString(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
			secondAttemptCanceled <- struct{}{}
		case <-time.After(2 * time.Second):
		}
	}))
	t.Cleanup(upstream.Close)

	store := newNativeGuardRetryGrokStore(t, upstream.URL, GrokProtocolMessages)
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	handler.SetRuntimeCache(cache.NewMemory(1))

	requestBody := []byte(`{"model":"grok-4.5","max_tokens":128,"messages":[{"role":"user","content":"hello"}],"stream":true}`)
	recorder := &guardDeadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(requestBody))
	ctx.Request.Header.Set("Content-Type", "application/json")
	ctx.Request.Header.Set("X-Codex2API-Affinity-Key", "native-guard-scope-attempt")
	affinityKey := sessionAffinityKey(resolveRequestSessionIdentity(ctx.Request.Header, requestBody).affinityID, 0)
	codexTurnStateOrigins.Delete(affinityKey)
	t.Cleanup(func() { codexTurnStateOrigins.Delete(affinityKey) })

	handler.Messages(ctx)

	body := recorder.Body.String()
	if got := upstreamCalls.Load(); got != 2 {
		t.Fatalf("expected 2 upstream calls (retryable stream break + guarded attempt), got %d; body=%s", got, outputGuardTestBodySummary(body))
	}
	if !strings.Contains(body, "event: error") || !strings.Contains(body, GuardTriggeredPublicMessage) {
		t.Fatalf("expected the sanitized anthropic guard terminal, got: %s", outputGuardTestBodySummary(body))
	}
	if strings.Contains(body, toolChunk[:32]) {
		t.Fatalf("the tripped tool output leaked downstream: %s", outputGuardTestBodySummary(body))
	}
	if strings.Contains(body, "event: message_stop") || strings.Contains(body, "content_block_stop") {
		t.Fatalf("guard terminal must not be followed by a completion frame: %s", outputGuardTestBodySummary(body))
	}
	select {
	case <-secondAttemptCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("expected the guarded attempt to be canceled upstream")
	}
}
