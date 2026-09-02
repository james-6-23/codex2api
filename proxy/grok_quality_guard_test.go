package proxy

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
)

func feedGrokQualityLines(t *testing.T, protocol GrokProtocol, lines ...string) *grokQualityScanState {
	t.Helper()
	state := &grokQualityScanState{protocol: protocol}
	observeGrokQualityChunk(state, []byte(strings.Join(lines, "\n")+"\n"))
	return state
}

// --- 判定状态机 ---

func TestClassifyGrokQualityHold(t *testing.T) {
	cases := []struct {
		name string
		sig  grokQualityStreamSignals
		want grokQualityVerdict
	}{
		{"思考证据直接放行", grokQualityStreamSignals{HasThinking: true}, grokQualityDeliver},
		{"终态足量输出零思考判降智", grokQualityStreamSignals{Terminal: true, VisibleTokens: 100}, grokQualityWithhold},
		{"终态短回复放行", grokQualityStreamSignals{Terminal: true, VisibleTokens: 3}, grokQualityDeliver},
		{"终态零输出继续等(空流另判)", grokQualityStreamSignals{Terminal: true}, grokQualityWait},
		{"未终态足量输出零思考判降智", grokQualityStreamSignals{VisibleTokens: 100}, grokQualityWithhold},
		{"未终态无输出等待", grokQualityStreamSignals{}, grokQualityWait},
		{"reasoning已开始未终态等待", grokQualityStreamSignals{ReasoningStarted: true, VisibleTokens: 100}, grokQualityWait},
		{"reasoning已开始超时有输出放行", grokQualityStreamSignals{ReasoningStarted: true, VisibleTokens: 5, HoldExpired: true}, grokQualityDeliver},
		{"超时有少量输出放行不罚", grokQualityStreamSignals{VisibleTokens: 5, HoldExpired: true}, grokQualityDeliver},
		{"超时零输出继续等待", grokQualityStreamSignals{HoldExpired: true}, grokQualityWait},
	}
	for _, tc := range cases {
		if got := classifyGrokQualityHold(tc.sig, grokQualityMinOutputTokens); got != tc.want {
			t.Errorf("%s: got %s, want %s (sig=%+v)", tc.name, got, tc.want, tc.sig)
		}
	}
}

func TestGrokQualityUsageReasoningTokensAreNotThinkingEvidence(t *testing.T) {
	// 降智账号典型形态:终态 usage 虚报几百 reasoning token,但全程无思考 delta。
	state := feedGrokQualityLines(t, GrokProtocolResponses,
		`data: {"type":"response.output_text.delta","delta":"这是一段足够长的可见回答内容,超过八个 token 的估算阈值没有问题"}`,
		`data: {"type":"response.completed","response":{"usage":{"input_tokens":100,"output_tokens":350,"total_tokens":450,"output_tokens_details":{"reasoning_tokens":300}}}}`,
	)
	sig := state.signals()
	if sig.HasThinking {
		t.Fatal("usage.reasoning_tokens 不应被当作思考证据")
	}
	if got := classifyGrokQualityHold(sig, grokQualityMinOutputTokens); got != grokQualityWithhold {
		t.Fatalf("虚报 reasoning token 的降智流应判 withhold, got %s", got)
	}
}

// --- Responses 协议扫描 ---

func TestObserveGrokQualityResponses(t *testing.T) {
	t.Run("reasoning delta 是思考证据", func(t *testing.T) {
		state := feedGrokQualityLines(t, GrokProtocolResponses,
			`data: {"type":"response.reasoning_text.delta","delta":"thinking..."}`,
		)
		if !state.hasThinking {
			t.Fatal("expected hasThinking")
		}
	})
	t.Run("encrypted_content 是思考证据", func(t *testing.T) {
		state := feedGrokQualityLines(t, GrokProtocolResponses,
			`data: {"type":"response.output_item.added","item":{"id":"rs_1","type":"reasoning","encrypted_content":"gAAAAxyz"}}`,
		)
		if !state.hasThinking {
			t.Fatal("expected hasThinking")
		}
	})
	t.Run("空reasoning占位只标记开始不算证据", func(t *testing.T) {
		state := feedGrokQualityLines(t, GrokProtocolResponses,
			`data: {"type":"response.output_item.added","item":{"id":"rs_1","type":"reasoning"}}`,
		)
		if state.hasThinking {
			t.Fatal("空 reasoning stub 不应算思考证据")
		}
		if !state.reasoningBegan {
			t.Fatal("expected reasoningBegan")
		}
	})
	t.Run("终态聚合output补计可见文本", func(t *testing.T) {
		state := feedGrokQualityLines(t, GrokProtocolResponses,
			`data: {"type":"response.completed","response":{"output":[{"type":"message","content":[{"type":"output_text","text":"这是一段没有任何思考过程直接吐出的最终回答内容文本,字数足够长以确保按四字符一个词元的保守估算也能跨过八词元的判定阈值线"}]}]}}`,
		)
		sig := state.signals()
		if !sig.Terminal || sig.VisibleTokens < grokQualityMinOutputTokens {
			t.Fatalf("终态聚合应计入可见输出: %+v", sig)
		}
	})
	t.Run("工具调用是有意义输出", func(t *testing.T) {
		state := feedGrokQualityLines(t, GrokProtocolResponses,
			`data: {"type":"response.function_call_arguments.delta","delta":"{\"a\":1}"}`,
		)
		if !state.semanticOutput {
			t.Fatal("expected semanticOutput")
		}
	})
	t.Run("DONE是终态", func(t *testing.T) {
		state := feedGrokQualityLines(t, GrokProtocolResponses, `data: [DONE]`)
		if !state.terminal {
			t.Fatal("expected terminal")
		}
	})
}

// --- Chat 协议扫描 ---

func TestObserveGrokQualityChat(t *testing.T) {
	t.Run("reasoning_content 是思考证据", func(t *testing.T) {
		state := feedGrokQualityLines(t, GrokProtocolChatCompletions,
			`data: {"choices":[{"delta":{"reasoning_content":"let me think"}}]}`,
		)
		if !state.hasThinking {
			t.Fatal("expected hasThinking")
		}
	})
	t.Run("纯文本流终态判降智", func(t *testing.T) {
		state := feedGrokQualityLines(t, GrokProtocolChatCompletions,
			`data: {"choices":[{"delta":{"content":"这是一段没有任何思考过程直接吐出的最终回答内容文本,字数足够长以确保按四字符一个词元的保守估算也能跨过八词元的判定阈值线"}}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		)
		if got := classifyGrokQualityHold(state.signals(), grokQualityMinOutputTokens); got != grokQualityWithhold {
			t.Fatalf("want withhold, got %s", got)
		}
	})
	t.Run("usage兜底可见token", func(t *testing.T) {
		state := feedGrokQualityLines(t, GrokProtocolChatCompletions,
			`data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":50,"total_tokens":60,"completion_tokens_details":{"reasoning_tokens":0}}}`,
		)
		if sig := state.signals(); sig.VisibleTokens != 50 {
			t.Fatalf("usage 兜底可见 token 应为 50, got %d", sig.VisibleTokens)
		}
	})
}

// --- Messages 协议扫描 ---

func TestObserveGrokQualityMessages(t *testing.T) {
	t.Run("thinking_delta 是思考证据", func(t *testing.T) {
		state := feedGrokQualityLines(t, GrokProtocolMessages,
			`data: {"type":"content_block_delta","delta":{"type":"thinking_delta","thinking":"hmm"}}`,
		)
		if !state.hasThinking {
			t.Fatal("expected hasThinking")
		}
	})
	t.Run("signature_delta 是思考证据", func(t *testing.T) {
		state := feedGrokQualityLines(t, GrokProtocolMessages,
			`data: {"type":"content_block_delta","delta":{"type":"signature_delta","signature":"EqQBCkgIBBAB"}}`,
		)
		if !state.hasThinking {
			t.Fatal("expected hasThinking")
		}
	})
	t.Run("空thinking块开场只标记开始", func(t *testing.T) {
		state := feedGrokQualityLines(t, GrokProtocolMessages,
			`data: {"type":"content_block_start","content_block":{"type":"thinking"}}`,
		)
		if state.hasThinking || !state.reasoningBegan {
			t.Fatalf("空 thinking 块: hasThinking=%v reasoningBegan=%v", state.hasThinking, state.reasoningBegan)
		}
	})
	t.Run("纯文本终态判降智", func(t *testing.T) {
		state := feedGrokQualityLines(t, GrokProtocolMessages,
			`data: {"type":"content_block_delta","delta":{"type":"text_delta","text":"这是一段没有任何思考过程直接吐出的最终回答内容文本,字数足够长以确保按四字符一个词元的保守估算也能跨过八词元的判定阈值线"}}`,
			`data: {"type":"message_stop"}`,
		)
		if got := classifyGrokQualityHold(state.signals(), grokQualityMinOutputTokens); got != grokQualityWithhold {
			t.Fatalf("want withhold, got %s", got)
		}
	})
}

// --- peek 端到端 ---

func grokQualityTestConfig() auth.GrokQualityGuardConfig {
	cfg := auth.DefaultGrokQualityGuardConfig()
	cfg.Enabled = true
	cfg.HoldTimeoutSec = 2
	return cfg
}

func TestPeekGrokQualityStreamHealthyReplayLossless(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"type":"response.reasoning_text.delta","delta":"thinking hard"}`,
		``,
		`data: {"type":"response.output_text.delta","delta":"最终回答"}`,
		``,
		`data: {"type":"response.completed","response":{}}`,
		``,
	}, "\n")
	state := &grokQualityScanState{protocol: GrokProtocolResponses}
	replay, verdict, _, err := peekGrokQualityStream(context.Background(), io.NopCloser(strings.NewReader(raw)), state, grokQualityTestConfig())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if verdict != grokQualityDeliver {
		t.Fatalf("healthy stream want deliver, got %s", verdict)
	}
	if !state.hasThinking {
		t.Fatal("expected state.hasThinking after healthy peek")
	}
	got, readErr := io.ReadAll(replay)
	if readErr != nil {
		t.Fatalf("replay read err: %v", readErr)
	}
	if string(got) != raw {
		t.Fatalf("replay 必须与原始流字节级一致:\n got=%q\nwant=%q", got, raw)
	}
}

func TestPeekGrokQualityStreamDegradedWithhold(t *testing.T) {
	raw := strings.Join([]string{
		`data: {"type":"response.output_text.delta","delta":"这是一段没有任何思考过程直接吐出的最终回答内容文本,字数足够长以确保按四字符一个词元的保守估算也能跨过八词元的判定阈值线"}`,
		`data: {"type":"response.completed","response":{"usage":{"input_tokens":10,"output_tokens":240,"total_tokens":250,"output_tokens_details":{"reasoning_tokens":200}}}}`,
		``,
	}, "\n")
	state := &grokQualityScanState{protocol: GrokProtocolResponses}
	_, verdict, usage, err := peekGrokQualityStream(context.Background(), io.NopCloser(strings.NewReader(raw)), state, grokQualityTestConfig())
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if verdict != grokQualityWithhold {
		t.Fatalf("degraded stream want withhold, got %s", verdict)
	}
	if usage == nil || usage.OutputTokens != 240 || usage.ReasoningTokens != 200 {
		t.Fatalf("peek 应带回上游 usage: %+v", usage)
	}
}

func TestPeekGrokQualityStreamEmptyStream(t *testing.T) {
	state := &grokQualityScanState{protocol: GrokProtocolResponses}
	_, _, _, err := peekGrokQualityStream(context.Background(), io.NopCloser(strings.NewReader("")), state, grokQualityTestConfig())
	if !errors.Is(err, errGrokQualityEmptyStream) {
		t.Fatalf("空流应返回 errGrokQualityEmptyStream, got %v", err)
	}
}

type erroringReader struct {
	data []byte
	err  error
	read bool
}

func (r *erroringReader) Read(dst []byte) (int, error) {
	if !r.read {
		r.read = true
		n := copy(dst, r.data)
		return n, nil
	}
	return 0, r.err
}

func (r *erroringReader) Close() error { return nil }

func TestPeekGrokQualityStreamBrokenStreamReplaysError(t *testing.T) {
	prefix := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"部分\"}\n"
	wantErr := errors.New("connection reset")
	state := &grokQualityScanState{protocol: GrokProtocolResponses}
	replay, _, _, err := peekGrokQualityStream(context.Background(), &erroringReader{data: []byte(prefix), err: wantErr}, state, grokQualityTestConfig())
	if !errors.Is(err, wantErr) {
		t.Fatalf("断流应把原错误交还调用方, got %v", err)
	}
	got, readErr := io.ReadAll(replay)
	if !bytes.Equal(got, []byte(prefix)) {
		t.Fatalf("断流回放应保留前缀: %q", got)
	}
	if !errors.Is(readErr, wantErr) {
		t.Fatalf("回放读到断点应复现原错误, got %v", readErr)
	}
}

func TestPeekGrokQualityStreamHoldTimeoutDeliversPartialOutput(t *testing.T) {
	// 有可见输出但上游挂住不发终态:超时后放行不罚。
	pr, pw := io.Pipe()
	go func() {
		_, _ = pw.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"少量\"}\n"))
		// 不关闭、不再写:模拟上游挂起。判定超时放行后 peek 返回,测试结束时关闭。
		time.Sleep(3 * time.Second)
		pw.Close()
	}()
	cfg := grokQualityTestConfig()
	cfg.HoldTimeoutSec = 1
	state := &grokQualityScanState{protocol: GrokProtocolResponses}
	start := time.Now()
	_, verdict, _, err := peekGrokQualityStream(context.Background(), pr, state, cfg)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if verdict != grokQualityDeliver {
		t.Fatalf("超时且有输出应放行, got %s", verdict)
	}
	if elapsed := time.Since(start); elapsed > 2500*time.Millisecond {
		t.Fatalf("应在 hold 超时后立即返回, took %s", elapsed)
	}
}

// --- 准入闸门 ---

func TestGrokQualityModelShouldReason(t *testing.T) {
	cases := map[string]bool{
		"grok-4.5":             true,
		"grok-4":               true,
		"grok-4-fast":          true,
		"grok-5":               true,
		"grok-3":               false,
		"grok-code-fast-1":     false,
		"grok-4-non-reasoning": false,
		"grok-2-image-1212":    false,
		"grok-imagine-image":   false,
		"gpt-5.3-codex":        false,
		"":                     false,
	}
	for model, want := range cases {
		if got := grokQualityModelShouldReason(model); got != want {
			t.Errorf("model %q: got %v, want %v", model, got, want)
		}
	}
}

func TestGrokQualityRequestDisablesReasoning(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"effort none 顶层", `{"reasoning_effort":"none"}`, true},
		{"reasoning.effort none", `{"reasoning":{"effort":"none"}}`, true},
		{"output_config.effort none", `{"output_config":{"effort":"none"}}`, true},
		{"thinking disabled", `{"thinking":{"type":"disabled"}}`, true},
		{"budget_tokens 0", `{"thinking":{"type":"enabled","budget_tokens":0}}`, true},
		{"正常请求", `{"reasoning":{"effort":"high"}}`, false},
		{"thinking enabled", `{"thinking":{"type":"enabled","budget_tokens":2048}}`, false},
		{"空 body", ``, false},
	}
	for _, tc := range cases {
		if got := grokQualityRequestDisablesReasoning([]byte(tc.body)); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestGrokQualityUpstreamObservableReasoning(t *testing.T) {
	cases := []struct {
		name     string
		protocol GrokProtocol
		body     string
		want     bool
	}{
		{"responses 带 include", GrokProtocolResponses, `{"include":["reasoning.encrypted_content"]}`, true},
		{"responses 带 grok cli include", GrokProtocolResponses, `{"include":["reasoning.encrypted_content","no_inline_citations"]}`, true},
		{"responses 带 summary auto", GrokProtocolResponses, `{"reasoning":{"effort":"high","summary":"auto"}}`, true},
		{"responses 带 summary detailed", GrokProtocolResponses, `{"reasoning":{"summary":"detailed"}}`, true},
		{"responses summary none", GrokProtocolResponses, `{"reasoning":{"effort":"high","summary":"none"}}`, false},
		{"responses 裸透传不可观测", GrokProtocolResponses, `{"model":"grok-4.6","stream":true,"input":[]}`, false},
		{"responses include 不含加密思考", GrokProtocolResponses, `{"include":["message.output_text.logprobs"]}`, false},
		{"messages thinking enabled", GrokProtocolMessages, `{"thinking":{"type":"enabled","budget_tokens":2048}}`, true},
		{"messages 无 thinking", GrokProtocolMessages, `{"model":"grok-4.6"}`, false},
		{"chat 保守不启用", GrokProtocolChatCompletions, `{"reasoning_effort":"high"}`, false},
		{"空 body", GrokProtocolResponses, ``, false},
	}
	for _, tc := range cases {
		if got := grokQualityUpstreamObservableReasoning(tc.protocol, []byte(tc.body)); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestGrokQualityRequestHasReplayUnsafeHostedTools(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"web_search_options", `{"web_search_options":{}}`, true},
		{"mcp_servers", `{"mcp_servers":[{"url":"https://x"}]}`, true},
		{"web_search tool", `{"tools":[{"type":"web_search"}]}`, true},
		{"托管 shell", `{"tools":[{"type":"shell","environment":{"type":"remote"}}]}`, true},
		{"本地 shell 安全", `{"tools":[{"type":"shell","environment":{"type":"local"}}]}`, false},
		{"function 工具安全", `{"tools":[{"type":"function","function":{"name":"f"}}]}`, false},
		{"anthropic 自定义工具安全", `{"tools":[{"name":"get_weather","input_schema":{}}]}`, false},
		{"anthropic server 工具", `{"tools":[{"type":"web_search_20250305","name":"web_search"}]}`, true},
		{"namespace 嵌套托管", `{"tools":[{"type":"namespace","tools":[{"type":"code_interpreter"}]}]}`, true},
		{"input 内 additional_tools", `{"input":[{"type":"additional_tools","tools":[{"type":"web_search"}]}]}`, true},
		{"无工具", `{"model":"grok-4.5"}`, false},
	}
	for _, tc := range cases {
		if got := grokQualityRequestHasReplayUnsafeHostedTools([]byte(tc.body)); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
}
