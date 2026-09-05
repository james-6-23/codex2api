package proxy

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// 上游容量降载的真实序列是「event: error → event: response.failed」。可重试类
// error 帧不能算客户端输出：一旦写出置位 wroteAnyBody，随后的 response.failed
// 就进不了首包前静默换号分支。不可重试类维持原样立即转发。
func TestIsRetryableUpstreamErrorFrame(t *testing.T) {
	cases := []struct {
		name      string
		eventType string
		payload   string
		want      bool
	}{
		{"降载 server_is_overloaded", "error", `{"type":"error","error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}}`, true},
		{"降载 slow_down", "error", `{"type":"error","error":{"code":"slow_down","message":"slow down"}}`, true},
		{"限流 rate_limit_exceeded", "error", `{"type":"error","error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"limited"}}`, true},
		{"不可重试 content_policy 原样转发", "error", `{"type":"error","error":{"type":"invalid_request_error","code":"content_policy_violation","message":"blocked"}}`, false},
		{"不可重试 invalid_request 原样转发", "error", `{"type":"error","error":{"type":"invalid_request_error","code":"invalid_request","message":"bad"}}`, false},
		{"response.failed 不归本判定管", "response.failed", `{"type":"response.failed","response":{"error":{"code":"server_is_overloaded"}}}`, false},
		{"内容事件不归本判定管", "response.output_text.delta", `{"type":"response.output_text.delta","delta":"hi"}`, false},
		{"空 payload", "error", ``, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isRetryableUpstreamErrorFrame(tc.eventType, []byte(tc.payload))
			if got != tc.want {
				t.Errorf("isRetryableUpstreamErrorFrame(%q, %s) = %v, want %v", tc.eventType, tc.payload, got, tc.want)
			}
		})
	}
}

// isCapacityShedPayload 识别 response.failed / error 帧里的容量降载码，两种嵌套形态都认，
// server_error / rate_limit 等不算降载。
func TestIsCapacityShedPayload(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    bool
	}{
		{"裸 error.code overloaded", `{"error":{"code":"server_is_overloaded"}}`, true},
		{"裸 error.code slow_down", `{"error":{"code":"SLOW_DOWN"}}`, true},
		{"嵌套 response.error.code", `{"response":{"error":{"code":"server_is_overloaded"}}}`, true},
		{"status_details.error.code", `{"response":{"status_details":{"error":{"code":"slow_down"}}}}`, true},
		{"server_error 不算降载", `{"error":{"code":"server_error"}}`, false},
		{"rate_limit 不算降载", `{"error":{"code":"rate_limit_exceeded"}}`, false},
		{"空 payload", ``, false},
		{"非 JSON", `not json`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isCapacityShedPayload([]byte(tc.payload)); got != tc.want {
				t.Errorf("isCapacityShedPayload(%s) = %v, want %v", tc.payload, got, tc.want)
			}
		})
	}
}

// 降载事件在 classify 里既保持 penalize=true（复用首包前帧缓冲/透明重试机制），
// 又置 capacityShed=true（供上报侧跳过账号连击）。环境变量可整体退回旧行为。
func TestClassifyResponseFailedOutcomeCapacityShed(t *testing.T) {
	shed := []byte(`{"type":"response.failed","response":{"status":"failed","error":{"code":"server_is_overloaded","message":"overloaded"}}}`)

	got := classifyResponseFailedOutcome(shed)
	if !got.capacityShed {
		t.Fatalf("capacityShed = false, want true for shed payload")
	}
	if !got.penalize {
		t.Fatalf("penalize = false, want true (帧缓冲/透明重试依赖它)")
	}

	// 普通 500 不是降载。
	server := []byte(`{"type":"response.failed","response":{"status":"failed","error":{"code":"internal_error","status_code":500}}}`)
	if classifyResponseFailedOutcome(server).capacityShed {
		t.Fatalf("capacityShed = true for generic 500, want false")
	}

	// 逃生阀:整体退回旧行为。
	t.Setenv("CODEX_DISABLE_CAPACITY_SHED_HANDLING", "1")
	if classifyResponseFailedOutcome(shed).capacityShed {
		t.Fatalf("capacityShed = true when disabled by env, want false")
	}
	if !classifyResponseFailedOutcome(shed).penalize {
		t.Fatalf("penalize = false when disabled, want true (旧行为仍换号重试)")
	}
}

// 容量降载先在同账号退避重试 maxCapacityShedSameAccountRetries 次（保留亲和），耗尽后换号；
// 非降载故障一律不保留亲和。预算按账号独立计（map 语义）。
func TestCapacityShedRetainsAffinity(t *testing.T) {
	shed := streamOutcome{capacityShed: true, penalize: true}
	for i := 0; i < maxCapacityShedSameAccountRetries; i++ {
		if !capacityShedRetainsAffinity(shed, i) {
			t.Fatalf("retriesSoFar=%d: want retain affinity", i)
		}
	}
	if capacityShedRetainsAffinity(shed, maxCapacityShedSameAccountRetries) {
		t.Fatalf("retriesSoFar=%d: want switch (budget exhausted)", maxCapacityShedSameAccountRetries)
	}
	nonShed := streamOutcome{capacityShed: false, penalize: true}
	if capacityShedRetainsAffinity(nonShed, 0) {
		t.Fatalf("non-shed outcome should never retain affinity")
	}

	// 按账号独立预算：账号 A 用满后换到账号 B，B 从 0 起仍有自己的退避预算。
	retries := map[int64]int{}
	const accountA, accountB = int64(11), int64(22)
	for capacityShedRetainsAffinity(shed, retries[accountA]) {
		retries[accountA]++
	}
	if retries[accountA] != maxCapacityShedSameAccountRetries {
		t.Fatalf("accountA budget = %d, want %d", retries[accountA], maxCapacityShedSameAccountRetries)
	}
	if !capacityShedRetainsAffinity(shed, retries[accountB]) {
		t.Fatalf("accountB should have its own fresh budget, not inherit A's exhaustion")
	}
}

// 预算耗尽后必须软排除该账号：降载不惩罚健康度，仅解绑亲和不足以换号，软排除才能
// 让调度选到兄弟账号，池试完后 ResetSoft 清空不会永久搁置。
func TestCapacityShedExhaustionSoftExcludes(t *testing.T) {
	ex := newRetryAccountExclusions()
	ex.MarkSoft(11)
	if sel := ex.ForSelection(); !sel[11] {
		t.Fatalf("MarkSoft(11) should exclude account 11 from selection")
	}
	if !ex.ResetSoft() {
		t.Fatalf("ResetSoft should report it cleared a soft entry")
	}
	if sel := ex.ForSelection(); sel[11] {
		t.Fatalf("ResetSoft should clear soft exclusion of 11")
	}
	// 软排除不得覆盖硬排除。
	ex.MarkHard(33)
	ex.MarkSoft(33)
	ex.ResetSoft()
	if sel := ex.ForSelection(); !sel[33] {
		t.Fatalf("hard exclusion of 33 must survive ResetSoft")
	}
}

// capacityShedEpilogue 复刻 Responses 流式路径对 error/response.failed 帧的
// 抑制→拦截→defer/写出时序（与 handler.go 的三条 SSE 转发循环同构），返回是否
// 命中首包前静默换号分支。
func capacityShedEpilogue(t *testing.T, c *gin.Context, events [][]byte, attempt, maxRetries int) bool {
	t.Helper()
	c.Header("Content-Type", "text/event-stream")
	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		t.Error("c.Writer 未实现 http.Flusher")
		return false
	}
	streamWriter := newStreamFlushWriter(c.Writer, flusher)
	var pending bytes.Buffer
	ttftRecorded := false
	wroteAnyBody := false
	gotTerminal := false
	abortedForHTTPError := false
	var terminalFailurePayload []byte

	for _, data := range events {
		eventType := gjson.GetBytes(data, "type").String()
		if !ttftRecorded && isFirstTokenPayload(data) {
			ttftRecorded = true
		}
		if eventType == "response.completed" || eventType == "response.failed" {
			gotTerminal = true
		}
		if eventType == "response.failed" {
			terminalFailurePayload = append([]byte(nil), data...)
		}
		if shouldSuppressRetryableResponseFailedBeforeFirstToken(eventType, terminalFailurePayload, ttftRecorded, wroteAnyBody, attempt, maxRetries, nil, nil) {
			pending.Reset()
			return true
		}
		if shouldReturnHTTPErrorForResponseFailed(eventType, ttftRecorded, wroteAnyBody, false) {
			pending.Reset()
			abortedForHTTPError = true
			break
		}
		shouldDefer := !ttftRecorded && !gotTerminal &&
			(isPreContentLifecycleEvent(eventType) || isRetryableUpstreamErrorFrame(eventType, data))
		wrote, err := writeDeferredSSEData(streamWriter, &pending, data, shouldDefer)
		if err != nil {
			t.Errorf("writeDeferredSSEData: %v", err)
			return false
		}
		if wrote {
			wroteAnyBody = true
		}
	}
	if wroteAnyBody {
		_ = streamWriter.Flush()
	}
	if abortedForHTTPError && !wroteAnyBody {
		outcome := classifyResponseFailedOutcome(terminalFailurePayload)
		c.Header("Content-Type", "application/json; charset=utf-8")
		writeResponseFailedHTTPError(c, outcome.logStatusCode, terminalFailurePayload, outcome.failureMessage)
	}
	return false
}

// 回归用例（真实上游降载序列）：created → in_progress → error 帧 → response.failed。
func TestStreamCapacityShedWireBehavior(t *testing.T) {
	gin.SetMode(gin.TestMode)
	setCodexCapacityRetryForTest(t, true)

	shedError := []byte(`{"type":"error","error":{"type":"service_unavailable_error","code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."},"sequence_number":2}`)
	shedFailed := []byte(`{"type":"response.failed","response":{"id":"resp_1","status":"failed","error":{"code":"server_is_overloaded","message":"Our servers are currently overloaded. Please try again later."}},"sequence_number":3}`)
	preamble := [][]byte{
		[]byte(`{"type":"response.created","response":{"id":"resp_1"},"sequence_number":0}`),
		[]byte(`{"type":"response.in_progress","response":{"id":"resp_1"},"sequence_number":1}`),
	}

	t.Run("首包前降载且有重试额度 → 静默换号、零字节下发", func(t *testing.T) {
		var suppressed bool
		router := gin.New()
		router.GET("/stream", func(c *gin.Context) {
			suppressed = capacityShedEpilogue(t, c, append(append([][]byte{}, preamble...), shedError, shedFailed), 0, 3)
		})
		srv := httptest.NewServer(router)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/stream")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)

		if !suppressed {
			t.Error("期望命中首包前静默换号分支")
		}
		if len(body) != 0 {
			t.Errorf("期望零字节下发, got %q", body)
		}
	})

	t.Run("首包前降载且重试耗尽 → 400 JSON 保留原始容量错误", func(t *testing.T) {
		router := gin.New()
		router.GET("/stream", func(c *gin.Context) {
			capacityShedEpilogue(t, c, append(append([][]byte{}, preamble...), shedError, shedFailed), 3, 3)
		})
		srv := httptest.NewServer(router)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/stream")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)

		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("status = %d, want 400 (body=%q)", resp.StatusCode, body)
		}
		if gjson.GetBytes(body, "error.code").String() != "server_is_overloaded" {
			t.Errorf("capacity code was lost: %s", body)
		}
		if !strings.Contains(resp.Header.Get("Content-Type"), "application/json") {
			t.Errorf("Content-Type = %q, want application/json", resp.Header.Get("Content-Type"))
		}
	})

	t.Run("流中途降载 → 保留原始降载码", func(t *testing.T) {
		events := append(append([][]byte{}, preamble...),
			[]byte(`{"type":"response.output_text.delta","delta":"partial"}`),
			shedError, shedFailed)
		router := gin.New()
		router.GET("/stream", func(c *gin.Context) {
			capacityShedEpilogue(t, c, events, 0, 3)
		})
		srv := httptest.NewServer(router)
		defer srv.Close()

		resp, err := http.Get(srv.URL + "/stream")
		if err != nil {
			t.Fatalf("GET: %v", err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		body := string(raw)

		if resp.StatusCode != http.StatusOK {
			t.Errorf("status = %d, want 200", resp.StatusCode)
		}
		if !strings.Contains(body, "partial") {
			t.Errorf("body 应包含已下发的正文, got %q", body)
		}
		if !strings.Contains(body, "response.failed") {
			t.Errorf("body 应包含终止事件, got %q", body)
		}
		if strings.Contains(body, `"code":"server_error"`) {
			t.Errorf("容量错误不应改写为 server_error, got %q", body)
		}
		if !strings.Contains(body, "server_is_overloaded") {
			t.Errorf("应保留 server_is_overloaded, got %q", body)
		}
		if !strings.Contains(body, "Our servers are currently overloaded") {
			t.Errorf("错误消息应原样保留, got %q", body)
		}
	})
}
