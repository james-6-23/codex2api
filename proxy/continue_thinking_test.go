package proxy

import (
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tidwall/gjson"
)

// --- fixture 构造 -----------------------------------------------------------

func sseBody(events ...string) io.ReadCloser {
	var b strings.Builder
	for _, ev := range events {
		b.WriteString("data: ")
		b.WriteString(ev)
		b.WriteString("\n\n")
	}
	return io.NopCloser(strings.NewReader(b.String()))
}

// sseResponse 构造包含给定事件的成功测试 SSE 响应。
func sseResponse(events ...string) *http.Response {
	return &http.Response{StatusCode: http.StatusOK, Body: sseBody(events...)}
}

// delayedSSEResponse 返回在指定延迟后写入事件的测试 SSE 响应。
func delayedSSEResponse(delay time.Duration, events ...string) *http.Response {
	reader, writer := io.Pipe()
	payload := sseBody(events...)
	go func() {
		defer writer.Close()
		defer payload.Close()
		time.Sleep(delay)
		_, _ = io.Copy(writer, payload)
	}()
	return &http.Response{StatusCode: http.StatusOK, Body: reader}
}

func evCreated() string {
	return `{"type":"response.created","sequence_number":0,"response":{"id":"resp_r1","created_at":1700000000,"status":"in_progress"}}`
}

func evReasoningAdded(seq, oi int) string {
	return fmt.Sprintf(`{"type":"response.output_item.added","sequence_number":%d,"output_index":%d,"item":{"id":"rs_%d","type":"reasoning"}}`, seq, oi, oi)
}

func evReasoningDelta(seq, oi int, text string) string {
	return fmt.Sprintf(`{"type":"response.reasoning_summary_text.delta","sequence_number":%d,"output_index":%d,"delta":%q}`, seq, oi, text)
}

func evReasoningDone(seq, oi int, encrypted string) string {
	enc := ""
	if encrypted != "" {
		enc = fmt.Sprintf(`,"encrypted_content":%q`, encrypted)
	}
	return fmt.Sprintf(`{"type":"response.output_item.done","sequence_number":%d,"output_index":%d,"item":{"id":"rs_%d","type":"reasoning","summary":[]%s}}`, seq, oi, oi, enc)
}

func evMessageAdded(seq, oi int) string {
	return fmt.Sprintf(`{"type":"response.output_item.added","sequence_number":%d,"output_index":%d,"item":{"id":"msg_%d","type":"message","role":"assistant"}}`, seq, oi, oi)
}

func evMessageDelta(seq, oi int, text string) string {
	return fmt.Sprintf(`{"type":"response.output_text.delta","sequence_number":%d,"output_index":%d,"delta":%q}`, seq, oi, text)
}

func evMessageDone(seq, oi int, text string) string {
	return fmt.Sprintf(`{"type":"response.output_item.done","sequence_number":%d,"output_index":%d,"item":{"id":"msg_%d","type":"message","role":"assistant","content":[{"type":"output_text","text":%q}]}}`, seq, oi, oi, text)
}

func evCompleted(seq, inputTokens, outputTokens, reasoningTokens int) string {
	return fmt.Sprintf(`{"type":"response.completed","sequence_number":%d,"response":{"id":"resp_r1","status":"completed","usage":{"input_tokens":%d,"output_tokens":%d,"total_tokens":%d,"input_tokens_details":{"cached_tokens":40},"output_tokens_details":{"reasoning_tokens":%d}}}}`, seq, inputTokens, outputTokens, inputTokens+outputTokens, reasoningTokens)
}

func evFailed(seq int) string {
	return fmt.Sprintf(`{"type":"response.failed","sequence_number":%d,"response":{"id":"resp_r1","status":"failed","error":{"code":"server_error","message":"boom"}}}`, seq)
}

// collectFold 用收集器跑一次折叠，返回下游事件与结果。
type foldCollector struct {
	events   [][]byte
	rounds   [][]byte // openRound 收到的请求体
	nextResp []*http.Response
	nextErr  []error
}

func (c *foldCollector) fold(baseBody string, maxRounds int, first *http.Response) continueFoldResult {
	f := &continueFold{
		baseBody:  []byte(baseBody),
		maxRounds: maxRounds,
		forward: func(data []byte) bool {
			c.events = append(c.events, append([]byte(nil), data...))
			return true
		},
		observe: func([]byte) {},
		openRound: func(body []byte) (*http.Response, error) {
			c.rounds = append(c.rounds, append([]byte(nil), body...))
			i := len(c.rounds) - 1
			if i < len(c.nextErr) && c.nextErr[i] != nil {
				return nil, c.nextErr[i]
			}
			if i < len(c.nextResp) {
				return c.nextResp[i], nil
			}
			return nil, fmt.Errorf("unexpected continuation round %d", i+2)
		},
		clientGone: func() bool { return false },
	}
	return runContinueThinkingFold(first, f)
}

func (c *foldCollector) eventTypes() []string {
	types := make([]string, 0, len(c.events))
	for _, ev := range c.events {
		types = append(types, gjson.GetBytes(ev, "type").String())
	}
	return types
}

// assertMonotonicSeq 校验下游事件 sequence_number 严格递增、output_index 单调不减。
func assertMonotonicSeq(t *testing.T, events [][]byte) {
	t.Helper()
	prevSeq := int64(-1)
	for i, ev := range events {
		seq := gjson.GetBytes(ev, "sequence_number")
		if !seq.Exists() {
			t.Fatalf("event %d 缺 sequence_number: %s", i, ev)
		}
		if seq.Int() != prevSeq+1 {
			t.Fatalf("event %d sequence_number 不连续: got %d want %d", i, seq.Int(), prevSeq+1)
		}
		prevSeq = seq.Int()
	}
}

// --- 用例 -------------------------------------------------------------------

func TestIsReasoningTruncationTokens(t *testing.T) {
	cases := []struct {
		tokens int
		want   bool
	}{
		{0, false}, {2, false}, {515, false}, {516, true}, {517, false},
		{518, false}, {1033, false}, {1034, true}, {1035, false},
		{518*7 - 2, true}, {518 * 7, false}, {-2, false},
	}
	for _, c := range cases {
		if got := isReasoningTruncationTokens(c.tokens); got != c.want {
			t.Errorf("isReasoningTruncationTokens(%d) = %v, want %v", c.tokens, got, c.want)
		}
	}
}

const testBaseBody = `{"model":"gpt-5.2","stream":true,"include":["reasoning.encrypted_content"],"input":[{"role":"user","content":"hi"}]}`

func TestFoldSingleCleanRound(t *testing.T) {
	c := &foldCollector{}
	res := c.fold(testBaseBody, 8, sseResponse(
		evCreated(),
		evReasoningAdded(1, 0),
		evReasoningDelta(2, 0, "thinking"),
		evReasoningDone(3, 0, "enc-a"),
		evMessageAdded(4, 1),
		evMessageDelta(5, 1, "hello"),
		evMessageDone(6, 1, "hello"),
		evCompleted(7, 100, 700, 300),
	))

	if res.StopReason != continueStopClean || res.RoundsRun != 1 || !res.GotTerminal {
		t.Fatalf("unexpected result: %+v", res)
	}
	if len(c.rounds) != 0 {
		t.Fatalf("clean round 不应触发续想, got %d rounds", len(c.rounds))
	}
	types := c.eventTypes()
	want := []string{
		"response.created",
		"response.output_item.added", // reasoning 实时
		"response.reasoning_summary_text.delta",
		"response.output_item.done",
		"response.output_item.added", // message 冲刷
		"response.output_text.delta",
		"response.output_item.done",
		"response.completed",
	}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("event types = %v, want %v", types, want)
	}
	assertMonotonicSeq(t, c.events)

	final := c.events[len(c.events)-1]
	if got := gjson.GetBytes(final, "response.id").String(); got != "resp_r1" {
		t.Errorf("terminal response.id = %q", got)
	}
	if n := gjson.GetBytes(final, "response.output.#").Int(); n != 2 {
		t.Errorf("terminal output items = %d, want 2", n)
	}
	if got := gjson.GetBytes(final, "response.usage.output_tokens").Int(); got != 700 {
		t.Errorf("usage.output_tokens = %d, want 700 (300 reasoning + 400 final)", got)
	}
}

func TestFoldTwoRounds(t *testing.T) {
	c := &foldCollector{
		nextResp: []*http.Response{sseResponse(
			evCreated(), // 隐藏轮生命周期事件应被吞掉
			evReasoningAdded(1, 0),
			evReasoningDone(2, 0, "enc-b"),
			evMessageAdded(3, 1),
			evMessageDelta(4, 1, "real answer"),
			evMessageDone(5, 1, "real answer"),
			evCompleted(6, 120, 900, 400),
		)},
	}
	res := c.fold(testBaseBody, 8, sseResponse(
		evCreated(),
		evReasoningAdded(1, 0),
		evReasoningDone(2, 0, "enc-a"),
		evMessageAdded(3, 1),
		evMessageDelta(4, 1, "truncated junk"),
		evMessageDone(5, 1, "truncated junk"),
		evCompleted(6, 100, 600, 516), // 516 = 命中指纹
	))

	if res.StopReason != continueStopClean || res.RoundsRun != 2 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if len(res.Rounds) != 2 || res.Rounds[0].Usage.ReasoningTokens != 516 || res.Rounds[1].Usage.ReasoningTokens != 400 {
		t.Fatalf("rounds stat 错误: %+v", res.Rounds)
	}
	if res.FinalUsage == nil || res.FinalUsage.OutputTokens != 900 {
		t.Fatalf("FinalUsage 应为最终轮真实值: %+v", res.FinalUsage)
	}

	// 续想请求体断言
	if len(c.rounds) != 1 {
		t.Fatalf("应有 1 次续想, got %d", len(c.rounds))
	}
	body := c.rounds[0]
	input := gjson.GetBytes(body, "input").Array()
	if len(input) != 3 { // 原始 user 消息 + 剥 id 的 reasoning + marker
		t.Fatalf("continuation input len = %d, want 3: %s", len(input), body)
	}
	if input[1].Get("type").String() != "reasoning" || input[1].Get("id").Exists() {
		t.Errorf("回放 reasoning 应剥掉 id: %s", input[1].Raw)
	}
	if input[1].Get("encrypted_content").String() != "enc-a" {
		t.Errorf("回放 reasoning 应保留 encrypted_content: %s", input[1].Raw)
	}
	marker := input[2]
	if marker.Get("phase").String() != "commentary" || !strings.Contains(marker.Get("content.0.text").String(), "Continue thinking") {
		t.Errorf("marker 形状错误: %s", marker.Raw)
	}
	if gjson.GetBytes(body, "previous_response_id").Exists() {
		t.Errorf("续想体不应有 previous_response_id")
	}

	// 下游事件:第 1 轮被截断的 message 不应出现
	for _, ev := range c.events {
		if strings.Contains(string(ev), "truncated junk") {
			t.Fatalf("截断轮的暂定输出泄漏到下游: %s", ev)
		}
	}
	types := c.eventTypes()
	want := []string{
		"response.created",                                        // 仅第 1 轮
		"response.output_item.added", "response.output_item.done", // r1 reasoning
		"response.output_item.added", "response.output_item.done", // r2 reasoning
		"response.output_item.added", "response.output_text.delta", "response.output_item.done", // r2 message 冲刷
		"response.completed",
	}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("event types = %v, want %v", types, want)
	}
	assertMonotonicSeq(t, c.events)

	// 重构 usage:input=第 1 轮,reasoning=516+400,output=916+500(最终轮非 reasoning)
	final := c.events[len(c.events)-1]
	usage := gjson.GetBytes(final, "response.usage")
	if usage.Get("input_tokens").Int() != 100 {
		t.Errorf("usage.input_tokens = %d, want 100(第 1 轮)", usage.Get("input_tokens").Int())
	}
	if usage.Get("output_tokens_details.reasoning_tokens").Int() != 916 {
		t.Errorf("reasoning_tokens = %d, want 916", usage.Get("output_tokens_details.reasoning_tokens").Int())
	}
	if usage.Get("output_tokens").Int() != 916+500 {
		t.Errorf("output_tokens = %d, want 1416", usage.Get("output_tokens").Int())
	}
	if usage.Get("input_tokens_details.cached_tokens").Int() != 40 {
		t.Errorf("cached_tokens = %d, want 40", usage.Get("input_tokens_details.cached_tokens").Int())
	}
	// output_index 应重编号为 0,1,2(r1 reasoning, r2 reasoning, r2 message)
	var ois []int64
	for _, ev := range c.events {
		if gjson.GetBytes(ev, "type").String() == "response.output_item.added" {
			ois = append(ois, gjson.GetBytes(ev, "output_index").Int())
		}
	}
	if fmt.Sprint(ois) != "[0 1 2]" {
		t.Errorf("output_index 序列 = %v, want [0 1 2]", ois)
	}
}

// TestFoldTerminalOutputKeepsOnlyLastRoundEncryptedContent 验证 issue #353 修复：
// 折叠后的 response.completed.output 里，只有最后一轮 reasoning 保留
// encrypted_content，更早各轮剥离（防止 N 份账号绑定加密载荷被客户端回传后
// 顶爆上下文窗口 / 跨账号成批被拒）。同轮续想用的 replayTail 不受影响。
func TestFoldTerminalOutputKeepsOnlyLastRoundEncryptedContent(t *testing.T) {
	c := &foldCollector{
		nextResp: []*http.Response{
			// 第 2 轮：仍命中指纹（516）→ 继续第 3 轮
			sseResponse(
				evCreated(),
				evReasoningAdded(1, 0),
				evReasoningDone(2, 0, "enc-r2"),
				evCompleted(3, 120, 600, 516),
			),
			// 第 3 轮：干净结束（400 未命中）→ 最后一轮
			sseResponse(
				evCreated(),
				evReasoningAdded(1, 0),
				evReasoningDone(2, 0, "enc-r3"),
				evMessageAdded(3, 1),
				evMessageDelta(4, 1, "final answer"),
				evMessageDone(5, 1, "final answer"),
				evCompleted(6, 140, 900, 400),
			),
		},
	}
	res := c.fold(testBaseBody, 8, sseResponse(
		evCreated(),
		evReasoningAdded(1, 0),
		evReasoningDone(2, 0, "enc-r1"),
		evCompleted(3, 100, 600, 516), // 命中指纹
	))
	if res.RoundsRun != 3 || res.StopReason != continueStopClean {
		t.Fatalf("unexpected result: RoundsRun=%d stop=%s", res.RoundsRun, res.StopReason)
	}

	final := c.events[len(c.events)-1]
	if gjson.GetBytes(final, "type").String() != "response.completed" {
		t.Fatalf("最终事件应为 response.completed: %s", final)
	}
	output := gjson.GetBytes(final, "response.output").Array()

	var reasoningEnc []string
	for _, item := range output {
		if item.Get("type").String() == "reasoning" {
			reasoningEnc = append(reasoningEnc, item.Get("encrypted_content").String())
		}
	}
	if len(reasoningEnc) != 3 {
		t.Fatalf("output 应含 3 个 reasoning item, got %d: %s", len(reasoningEnc), final)
	}
	// 前两轮剥离，最后一轮保留
	if reasoningEnc[0] != "" || reasoningEnc[1] != "" {
		t.Errorf("更早各轮 reasoning 应剥离 encrypted_content: got %q, %q", reasoningEnc[0], reasoningEnc[1])
	}
	if reasoningEnc[2] != "enc-r3" {
		t.Errorf("最后一轮 reasoning 应保留 encrypted_content: got %q", reasoningEnc[2])
	}

	// replayTail（发上游续想）不受影响：第 1 轮续想体仍带 enc-r1
	if got := gjson.GetBytes(c.rounds[0], "input.1.encrypted_content").String(); got != "enc-r1" {
		t.Errorf("续想体 1 应保留 enc-r1, got %q", got)
	}
}

// TestFoldTerminalOutputKeepAllEncryptedEscapeHatch 验证逃生阀恢复旧行为。
func TestFoldTerminalOutputKeepAllEncryptedEscapeHatch(t *testing.T) {
	t.Setenv("CODEX_CONTINUE_KEEP_ALL_ENCRYPTED", "1")
	c := &foldCollector{
		nextResp: []*http.Response{sseResponse(
			evCreated(),
			evReasoningAdded(1, 0),
			evReasoningDone(2, 0, "enc-b"),
			evMessageAdded(3, 1),
			evMessageDone(5, 1, "answer"),
			evCompleted(6, 120, 900, 400),
		)},
	}
	c.fold(testBaseBody, 8, sseResponse(
		evCreated(),
		evReasoningAdded(1, 0),
		evReasoningDone(2, 0, "enc-a"),
		evCompleted(3, 100, 600, 516),
	))
	final := c.events[len(c.events)-1]
	output := gjson.GetBytes(final, "response.output").Array()
	if output[0].Get("encrypted_content").String() != "enc-a" {
		t.Errorf("逃生阀开启时更早轮应保留 encrypted_content: %s", output[0].Raw)
	}
}

func TestFoldMaxRoundsGuard(t *testing.T) {
	c := &foldCollector{}
	res := c.fold(testBaseBody, 1, sseResponse( // maxRounds=1:即使命中指纹也不续想
		evCreated(),
		evReasoningAdded(1, 0),
		evReasoningDone(2, 0, "enc-a"),
		evMessageAdded(3, 1),
		evMessageDone(4, 1, "answer"),
		evCompleted(5, 100, 600, 516),
	))
	if res.StopReason != continueStopMaxRounds || len(c.rounds) != 0 {
		t.Fatalf("应因 max_rounds 停止且不发续想: %+v rounds=%d", res, len(c.rounds))
	}
	// 当前输出仍应冲刷给客户端
	types := c.eventTypes()
	if types[len(types)-1] != "response.completed" || !strings.Contains(strings.Join(types, ","), "response.output_item.added") {
		t.Fatalf("护栏停止时应冲刷输出并给出终态: %v", types)
	}
}

func TestFoldNoEncryptedContentGuard(t *testing.T) {
	c := &foldCollector{}
	res := c.fold(testBaseBody, 8, sseResponse(
		evCreated(),
		evReasoningAdded(1, 0),
		evReasoningDone(2, 0, ""), // 无 encrypted_content
		evMessageAdded(3, 1),
		evMessageDone(4, 1, "answer"),
		evCompleted(5, 100, 600, 516),
	))
	if res.StopReason != continueStopNoEncrypted || len(c.rounds) != 0 {
		t.Fatalf("缺 encrypted_content 不应续想: %+v", res)
	}
}

func TestFoldContinuationRoundRejected(t *testing.T) {
	c := &foldCollector{
		nextResp: []*http.Response{{
			StatusCode: http.StatusBadRequest,
			Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"bad request"}}`)),
		}},
	}
	res := c.fold(testBaseBody, 8, sseResponse(
		evCreated(),
		evReasoningAdded(1, 0),
		evReasoningDone(2, 0, "enc-a"),
		evCompleted(3, 100, 516, 516),
	))
	if res.StopReason != continueStopRoundError {
		t.Fatalf("StopReason = %q, want continuation_error", res.StopReason)
	}
	// 失败的续想开轮记入 FailedContinuation（与真实成功轮 Rounds 分开，避免重复计费）。
	if len(res.Rounds) != 1 {
		t.Fatalf("Rounds 应只含 1 个成功轮, got %+v", res.Rounds)
	}
	if res.FailedContinuation == nil || res.FailedContinuation.StatusCode != http.StatusBadRequest {
		t.Fatalf("失败续想应记入 FailedContinuation: %+v", res.FailedContinuation)
	}
	if res.FinalUsage == nil || res.FinalUsage.ReasoningTokens != 516 {
		t.Fatalf("FinalUsage 应为最后成功轮真实用量: %+v", res.FinalUsage)
	}
	if !res.GotTerminal {
		t.Fatalf("续想失败已合成终态, GotTerminal 应为 true")
	}
	final := c.events[len(c.events)-1]
	if gjson.GetBytes(final, "type").String() != "response.incomplete" {
		t.Fatalf("续想失败应合成 response.incomplete: %s", final)
	}
	if gjson.GetBytes(final, "response.incomplete_details.reason").String() != "upstream_error" {
		t.Fatalf("incomplete reason 错误: %s", final)
	}
	assertMonotonicSeq(t, c.events)
}

func TestFoldFirstRoundContinuationFailsBillsRound1(t *testing.T) {
	// 第 1 轮命中指纹但首次续想开轮即失败：FinalUsage 必须保留第 1 轮真实用量
	// （否则整轮消耗漏记），且已合成终态（否则会被误判断流惩罚账号）。
	c := &foldCollector{
		nextErr: []error{fmt.Errorf("dial timeout")},
	}
	res := c.fold(testBaseBody, 8, sseResponse(
		evCreated(),
		evReasoningAdded(1, 0),
		evReasoningDone(2, 0, "enc-a"),
		evCompleted(3, 4000, 516, 516),
	))
	if res.RoundsRun != 1 || res.StopReason != continueStopRoundError {
		t.Fatalf("unexpected: RoundsRun=%d stop=%s", res.RoundsRun, res.StopReason)
	}
	if res.FinalUsage == nil || res.FinalUsage.InputTokens != 4000 || res.FinalUsage.ReasoningTokens != 516 {
		t.Fatalf("第 1 轮真实用量应保留在 FinalUsage: %+v", res.FinalUsage)
	}
	if res.FailedContinuation == nil {
		t.Fatalf("首次续想失败应记入 FailedContinuation")
	}
	if !res.GotTerminal {
		t.Fatalf("已合成终态, GotTerminal 应为 true(避免误判断流)")
	}
}

func TestFoldTwoRoundsThenOpenFailsNoDoubleCount(t *testing.T) {
	// r1 命中 → r2 命中 → 开 r3 失败。Rounds 应含 r1、r2 两个成功轮，
	// FailedContinuation 单独记 r3 失败；FinalUsage=r2，收尾计费 r2，
	// logContinueThinkingRounds 计费 r1 + 失败占位，绝不重复计 r2。
	c := &foldCollector{
		nextResp: []*http.Response{sseResponse(
			evCreated(),
			evReasoningAdded(1, 0),
			evReasoningDone(2, 0, "enc-b"),
			evCompleted(3, 120, 900, 516), // r2 也命中指纹
		)},
		nextErr: []error{nil, fmt.Errorf("r3 dial fail")}, // 第 2 次 openRound(开 r3)失败
	}
	res := c.fold(testBaseBody, 8, sseResponse(
		evCreated(),
		evReasoningAdded(1, 0),
		evReasoningDone(2, 0, "enc-a"),
		evCompleted(3, 100, 600, 516),
	))
	if res.StopReason != continueStopRoundError {
		t.Fatalf("StopReason = %q, want continuation_error", res.StopReason)
	}
	if len(res.Rounds) != 2 {
		t.Fatalf("Rounds 应含 2 个成功轮, got %d: %+v", len(res.Rounds), res.Rounds)
	}
	if res.FailedContinuation == nil {
		t.Fatalf("r3 失败应记入 FailedContinuation")
	}
	if res.FinalUsage == nil || res.FinalUsage.OutputTokens != 900 {
		t.Fatalf("FinalUsage 应为 r2: %+v", res.FinalUsage)
	}
	// 记账口径核对：收尾计 FinalUsage(r2)，logContinueThinkingRounds 计 Rounds[:len-1]=[r1] + 失败占位。
	// 断言 Rounds[:len-1] 恰为 [r1]，不含 r2，避免与收尾重复计费。
	if res.Rounds[0].Usage == nil || res.Rounds[0].Usage.InputTokens != 100 {
		t.Fatalf("Rounds[0] 应为 r1: %+v", res.Rounds[0].Usage)
	}
}

func TestFoldFirstRoundEOFSilent(t *testing.T) {
	c := &foldCollector{}
	res := c.fold(testBaseBody, 8, sseResponse(
		evCreated(), // 只有生命周期事件,无 terminal
	))
	if res.GotTerminal || res.StopReason != continueStopUpstreamEOF {
		t.Fatalf("第 1 轮 EOF 应静默(交给上层透明重试): %+v", res)
	}
	for _, ev := range c.events {
		if gjson.GetBytes(ev, "type").String() == "response.incomplete" {
			t.Fatalf("第 1 轮无输出 EOF 不应合成 incomplete: %s", ev)
		}
	}
}

func TestFoldFirstRoundEOFWithBufferedContentFlushes(t *testing.T) {
	// 第 1 轮缓冲了 message 内容后 EOF（无 terminal）：不应静默丢弃，
	// 应冲刷缓冲内容并合成 incomplete，避免客户端收到空的 200 流。
	c := &foldCollector{}
	res := c.fold(testBaseBody, 8, sseResponse(
		evCreated(),
		evMessageAdded(1, 0),
		evMessageDelta(2, 0, "partial answer"),
		// 无 terminal
	))
	if !res.GotTerminal || res.StopReason != continueStopUpstreamEOF {
		t.Fatalf("有缓冲内容的 EOF 应合成终态: %+v", res)
	}
	joined := ""
	for _, ev := range c.events {
		joined += string(ev)
	}
	if !strings.Contains(joined, "partial answer") {
		t.Fatalf("缓冲内容应被冲刷给客户端, events=%v", c.eventTypes())
	}
	if c.eventTypes()[len(c.eventTypes())-1] != "response.incomplete" {
		t.Fatalf("末事件应为 incomplete: %v", c.eventTypes())
	}
}

func TestFoldContinuationRoundEOF(t *testing.T) {
	c := &foldCollector{
		nextResp: []*http.Response{sseResponse(
			evReasoningAdded(1, 0), // 续想轮断流,无 terminal
		)},
	}
	res := c.fold(testBaseBody, 8, sseResponse(
		evCreated(),
		evReasoningAdded(1, 0),
		evReasoningDone(2, 0, "enc-a"),
		evCompleted(3, 100, 516, 516),
	))
	if !res.GotTerminal || res.StopReason != continueStopUpstreamEOF {
		t.Fatalf("续想轮 EOF 应合成终态: %+v", res)
	}
	final := c.events[len(c.events)-1]
	if gjson.GetBytes(final, "type").String() != "response.incomplete" ||
		gjson.GetBytes(final, "response.incomplete_details.reason").String() != "upstream_eof" {
		t.Fatalf("应合成 incomplete(upstream_eof): %s", final)
	}
}

func TestFoldFirstRoundFailedPassthrough(t *testing.T) {
	failed := evFailed(1)
	c := &foldCollector{}
	res := c.fold(testBaseBody, 8, sseResponse(
		evCreated(),
		failed,
	))
	if res.StopReason != continueStopClean {
		t.Fatalf("unexpected result: %+v", res)
	}
	final := c.events[len(c.events)-1]
	if string(final) != failed {
		t.Fatalf("第 1 轮无输出的 failed 应字节级透传:\ngot  %s\nwant %s", final, failed)
	}
}

func TestFoldForwardAbort(t *testing.T) {
	aborted := false
	f := &continueFold{
		baseBody:  []byte(testBaseBody),
		maxRounds: 8,
		forward: func(data []byte) bool {
			aborted = true
			return false // 模拟首包前 failed 抑制等上层中止
		},
		observe:    func([]byte) {},
		openRound:  func([]byte) (*http.Response, error) { return nil, fmt.Errorf("unreachable") },
		clientGone: func() bool { return false },
	}
	res := runContinueThinkingFold(sseResponse(
		evCreated(),
		evReasoningAdded(1, 0),
		evCompleted(2, 100, 516, 516),
	), f)
	if !aborted || res.StopReason != continueStopForwardAbort {
		t.Fatalf("forward=false 应立即中止: %+v", res)
	}
}

func TestFoldClientGoneStopsContinuation(t *testing.T) {
	c := &foldCollector{}
	f := &continueFold{
		baseBody:  []byte(testBaseBody),
		maxRounds: 8,
		forward: func(data []byte) bool {
			c.events = append(c.events, append([]byte(nil), data...))
			return true
		},
		observe:    func([]byte) {},
		openRound:  func([]byte) (*http.Response, error) { t.Fatal("客户端断开后不应开续想轮"); return nil, nil },
		clientGone: func() bool { return true },
	}
	res := runContinueThinkingFold(sseResponse(
		evCreated(),
		evReasoningAdded(1, 0),
		evReasoningDone(2, 0, "enc-a"),
		evCompleted(3, 100, 516, 516),
	), f)
	if res.StopReason != continueStopClientGone {
		t.Fatalf("StopReason = %q, want client_gone", res.StopReason)
	}
}

// --- 隐藏轮下游保活 (issue #458) --------------------------------------------

// keepaliveFold 构造带保活回调的折叠:openRound 延迟 openDelay 模拟慢续想轮。
func keepaliveFold(t *testing.T, counter *atomic.Int32, keepaliveRet func() bool, openDelay time.Duration, round2 *http.Response) *continueFold {
	t.Helper()
	return &continueFold{
		baseBody:  []byte(testBaseBody),
		maxRounds: 8,
		forward:   func([]byte) bool { return true },
		observe:   func([]byte) {},
		openRound: func([]byte) (*http.Response, error) {
			time.Sleep(openDelay)
			return round2, nil
		},
		clientGone:        func() bool { return false },
		keepalive:         func() bool { counter.Add(1); return keepaliveRet() },
		keepaliveInterval: 10 * time.Millisecond,
	}
}

func fingerprintFirstRound() *http.Response {
	return sseResponse(
		evCreated(),
		evReasoningAdded(1, 0),
		evReasoningDone(2, 0, "enc-a"),
		evCompleted(3, 100, 516, 516), // 516 命中指纹 → 进入隐藏续想
	)
}

func cleanSecondRound() *http.Response {
	return sseResponse(
		evReasoningAdded(1, 0),
		evReasoningDone(2, 0, "enc-b"),
		evCompleted(3, 120, 900, 400),
	)
}

// TestFoldKeepaliveFiresDuringHiddenRoundAndStopsAfterFold 验证隐藏续想轮等待期间会保活，
// 且整个折叠结束后停止回调。
func TestFoldKeepaliveFiresDuringHiddenRoundAndStopsAfterFold(t *testing.T) {
	var counter atomic.Int32
	f := keepaliveFold(t, &counter, func() bool { return true }, 80*time.Millisecond, cleanSecondRound())

	res := runContinueThinkingFold(fingerprintFirstRound(), f)
	if res.StopReason != continueStopClean || res.RoundsRun != 2 {
		t.Fatalf("unexpected result: %+v", res)
	}
	after := counter.Load()
	if after < 1 {
		t.Fatalf("openRound 延迟 80ms、间隔 10ms,保活至少应触发 1 次, got %d", after)
	}

	// fold 返回时 defer 已同步 join 保活 goroutine:之后不允许再有回调。
	time.Sleep(50 * time.Millisecond)
	if got := counter.Load(); got != after {
		t.Fatalf("fold 返回后保活仍在触发: %d -> %d", after, got)
	}
}

// TestFoldKeepaliveFiresDuringCleanFirstRoundAndStops 验证首轮静默期间会发送保活，
// 且折叠结束后保活协程会停止。
func TestFoldKeepaliveFiresDuringCleanFirstRoundAndStops(t *testing.T) {
	var counter atomic.Int32
	f := keepaliveFold(t, &counter, func() bool { return true }, 0, nil)
	f.openRound = func([]byte) (*http.Response, error) {
		t.Fatal("未命中指纹不应开续想轮")
		return nil, nil
	}
	resp := delayedSSEResponse(30*time.Millisecond,
		evCreated(),
		evReasoningAdded(1, 0),
		evReasoningDone(2, 0, "enc-a"),
		evCompleted(3, 100, 600, 400), // 400 不命中指纹
	)

	res := runContinueThinkingFold(resp, f)
	if res.StopReason != continueStopClean || res.RoundsRun != 1 {
		t.Fatalf("unexpected result: %+v", res)
	}
	after := counter.Load()
	if after < 1 {
		t.Fatalf("首轮延迟 30ms、间隔 10ms，保活至少应触发 1 次, got %d", after)
	}
	time.Sleep(20 * time.Millisecond)
	if got := counter.Load(); got != after {
		t.Fatalf("fold 返回后保活仍在触发: %d -> %d", after, got)
	}
}

// TestFoldKeepaliveDisabledAtZeroInterval 验证保活间隔为零时不会启动心跳。
func TestFoldKeepaliveDisabledAtZeroInterval(t *testing.T) {
	var counter atomic.Int32
	f := keepaliveFold(t, &counter, func() bool { return true }, 0, nil)
	f.keepaliveInterval = 0
	res := runContinueThinkingFold(delayedSSEResponse(30*time.Millisecond,
		evCreated(),
		evCompleted(1, 100, 600, 400),
	), f)
	if res.StopReason != continueStopClean || res.RoundsRun != 1 {
		t.Fatalf("unexpected result: %+v", res)
	}
	if got := counter.Load(); got != 0 {
		t.Fatalf("间隔为 0 时不应触发保活, got %d", got)
	}
}

func TestFoldKeepaliveStopsWhenCallbackReportsDeadClient(t *testing.T) {
	var counter atomic.Int32
	f := keepaliveFold(t, &counter, func() bool { return false }, 100*time.Millisecond, cleanSecondRound())

	res := runContinueThinkingFold(fingerprintFirstRound(), f)
	if res.StopReason != continueStopClean {
		t.Fatalf("unexpected result: %+v", res)
	}
	// 回调首次返回 false 即停表:openRound 100ms 里 10ms 间隔本可触发 ~10 次。
	if got := counter.Load(); got != 1 {
		t.Fatalf("回调返回 false 后保活应立即停止, got %d 次", got)
	}
}
