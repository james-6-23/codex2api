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
// attempt 已消耗但未触发的字节必须累计到第二个 attempt，否则每次 attempt
// 重建状态机会把跨 attempt 的失控输出放行。上游调用次数固定为「首个可重试
// 断流 + 一次被 Guard 终止的 attempt」，Guard 命中后不得再引入透明重试。
func TestNativeHTTP_GrokMessagesGuardBudgetInheritedAcrossAttempts(t *testing.T) {
	setupOutputGuardHTTPTestEnv(t)
	enableCatchAllContinuousRetry(t)

	// 每个 attempt 单独都低于 128KiB channel 上限，两次累计后越界。
	perAttempt := guardHTTPTestMaxChannelBytes/2 + 6*1024
	firstChunk := outputGuardTestRandomText(101, int(perAttempt))
	secondChunk := outputGuardTestRandomText(202, int(perAttempt))

	var upstreamCalls atomic.Int32
	secondAttemptCanceled := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_native_guard_attempt\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[]}}\n\n")
		chunk := firstChunk
		if call > 1 {
			chunk = secondChunk
		}
		payload, _ := json.Marshal(gin.H{
			"type":  "content_block_delta",
			"index": 0,
			"delta": gin.H{"type": "text_delta", "text": chunk},
		})
		_, _ = io.WriteString(w, "event: content_block_delta\ndata: "+string(payload)+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
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
	for _, leaked := range []string{firstChunk[:32], secondChunk[:32]} {
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
