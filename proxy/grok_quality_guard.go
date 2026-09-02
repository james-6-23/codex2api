package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"

	"github.com/gin-gonic/gin"
)

// Grok 降智检测(issue #587):流式请求拿到上游 200 后先扣住帧做质量判定,
// 判定"有可见输出但全程无思考证据"即丢弃该响应换号重试,并按阶梯惩罚账号
// (首次冷却,冷却后再犯禁用)。默认关闭;开启后健康账号首个思考帧一到立即放行,
// 不改变现有转发路径的字节级行为。
const (
	grokQualityCooldownReason      = "quality_degraded"
	grokQualityEmptyStreamReason   = "grok_empty_stream"
	grokQualityFailureKind         = "quality_degraded"
	grokQualityMinOutputTokens     = 8
	grokQualityHoldMaxBufferBytes  = 4 << 20
	grokQualityPendingLineCapBytes = 1 << 20
	grokQualityEmptyStreamCooldown = 15 * time.Minute
)

var errGrokQualityEmptyStream = errors.New("grok upstream stream produced no output")

type grokQualityVerdict string

const (
	grokQualityWait     grokQualityVerdict = "wait"
	grokQualityDeliver  grokQualityVerdict = "deliver"
	grokQualityWithhold grokQualityVerdict = "withhold"
)

// grokQualityGuardAction 是 handler 循环对一次 peek 结果的处置。
type grokQualityGuardAction int

const (
	// grokQualityGuardProceed:继续原转发路径(resp.Body 已替换为前缀回放)。
	grokQualityGuardProceed grokQualityGuardAction = iota
	// grokQualityGuardRetry:丢弃本次响应换号重试(惩罚与用量日志已在内部完成)。
	grokQualityGuardRetry
	// grokQualityGuardFailClosed:重试额度耗尽且策略为 fail_closed,调用方写错误响应后返回。
	grokQualityGuardFailClosed
)

// ---------------------------------------------------------------------------
// 判定状态机
// ---------------------------------------------------------------------------

// grokQualityStreamSignals 是判定器输入。usage 里的 reasoning_tokens 不作为
// 思考证据:降智账号会在终态虚报几百 reasoning token 但全程不发任何思考 delta。
type grokQualityStreamSignals struct {
	HasThinking      bool
	ReasoningStarted bool
	VisibleTokens    int64
	OutputTokens     int64
	Terminal         bool
	HoldExpired      bool
}

type grokQualityScanState struct {
	protocol       GrokProtocol
	pending        []byte
	hasThinking    bool
	reasoningBegan bool
	visibleRunes   int
	aggregateRunes int
	semanticOutput bool
	usageReported  bool
	usage          UsageInfo
	terminal       bool
}

func (s *grokQualityScanState) signals() grokQualityStreamSignals {
	visibleRunes := max(s.visibleRunes, s.aggregateRunes)
	visible := int64((visibleRunes + 3) / 4)
	output := int64(s.usage.OutputTokens)
	if s.usageReported {
		fromUsage := int64(s.usage.OutputTokens - s.usage.ReasoningTokens)
		if fromUsage > visible {
			visible = fromUsage
		}
	}
	return grokQualityStreamSignals{
		HasThinking:      s.hasThinking,
		ReasoningStarted: s.reasoningBegan || s.hasThinking,
		VisibleTokens:    visible,
		OutputTokens:     output,
		Terminal:         s.terminal,
	}
}

// classifyGrokQualityHold 判定扣住的流能否放行。流内思考证据(思考 delta、
// 带 encrypted_content 的 reasoning 项、signature delta)一律放行;终态且可见
// 输出足量但零思考证据 → withhold;短回复(<minOutput)放行避免误杀 "ok"/"yes";
// 空 reasoning 占位(item added 无密文)不算证据,超时时已有可见输出则放行不罚。
func classifyGrokQualityHold(sig grokQualityStreamSignals, minOutput int64) grokQualityVerdict {
	if minOutput <= 0 {
		minOutput = grokQualityMinOutputTokens
	}
	if sig.HasThinking {
		return grokQualityDeliver
	}
	output := sig.VisibleTokens
	if output <= 0 {
		output = sig.OutputTokens
	}
	enough := output >= minOutput
	if sig.ReasoningStarted && !sig.Terminal {
		if sig.HoldExpired && output > 0 {
			return grokQualityDeliver
		}
		return grokQualityWait
	}
	if sig.Terminal {
		if output <= 0 {
			return grokQualityWait
		}
		if enough {
			return grokQualityWithhold
		}
		return grokQualityDeliver
	}
	if enough {
		return grokQualityWithhold
	}
	if sig.HoldExpired {
		if output <= 0 {
			return grokQualityWait
		}
		return grokQualityDeliver
	}
	return grokQualityWait
}

// observeGrokQualityChunk 按行喂入一段 SSE 字节流,增量更新判定状态。
func observeGrokQualityChunk(state *grokQualityScanState, chunk []byte) {
	if state == nil || len(chunk) == 0 {
		return
	}
	state.pending = append(state.pending, chunk...)
	for {
		index := bytes.IndexByte(state.pending, '\n')
		if index < 0 {
			if len(state.pending) > grokQualityPendingLineCapBytes {
				state.pending = nil
			}
			return
		}
		line := bytes.TrimSpace(state.pending[:index])
		state.pending = state.pending[index+1:]
		if len(line) == 0 || !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if bytes.Equal(payload, []byte("[DONE]")) {
			state.terminal = true
			continue
		}
		switch state.protocol {
		case GrokProtocolChatCompletions:
			observeGrokQualityChat(state, payload)
		case GrokProtocolMessages:
			observeGrokQualityMessages(state, payload)
		default:
			observeGrokQualityResponses(state, payload)
		}
	}
}

func grokQualityNoteVisible(state *grokQualityScanState, text string) {
	if text != "" {
		state.visibleRunes += utf8.RuneCountInString(text)
	}
}

func observeGrokQualityChat(state *grokQualityScanState, payload []byte) {
	var event struct {
		Choices []struct {
			Delta struct {
				Content          string `json:"content"`
				Reasoning        string `json:"reasoning"`
				ReasoningContent string `json:"reasoning_content"`
				ThinkingContent  string `json:"thinking_content"`
				ToolCalls        []any  `json:"tool_calls"`
				FunctionCall     any    `json:"function_call"`
			} `json:"delta"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens            int `json:"prompt_tokens"`
			CompletionTokens        int `json:"completion_tokens"`
			TotalTokens             int `json:"total_tokens"`
			CompletionTokensDetails struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal(payload, &event) != nil {
		return
	}
	if event.Usage != nil {
		state.usageReported = true
		state.usage.PromptTokens = event.Usage.PromptTokens
		state.usage.CompletionTokens = event.Usage.CompletionTokens
		state.usage.TotalTokens = event.Usage.TotalTokens
		state.usage.InputTokens = event.Usage.PromptTokens
		state.usage.OutputTokens = event.Usage.CompletionTokens
		state.usage.ReasoningTokens = event.Usage.CompletionTokensDetails.ReasoningTokens
	}
	for _, choice := range event.Choices {
		delta := choice.Delta
		if strings.TrimSpace(delta.Reasoning) != "" || strings.TrimSpace(delta.ReasoningContent) != "" || strings.TrimSpace(delta.ThinkingContent) != "" {
			state.hasThinking = true
		}
		grokQualityNoteVisible(state, delta.Content)
		if len(delta.ToolCalls) > 0 || delta.FunctionCall != nil {
			state.semanticOutput = true
		}
		if choice.FinishReason != "" {
			state.terminal = true
		}
	}
}

type grokQualityResponsesItem struct {
	ID               string `json:"id"`
	Type             string `json:"type"`
	EncryptedContent string `json:"encrypted_content"`
	Content          []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	} `json:"content"`
}

func grokQualityNoteResponsesItem(state *grokQualityScanState, item grokQualityResponsesItem) int {
	visibleRunes := 0
	switch strings.ToLower(strings.TrimSpace(item.Type)) {
	case "":
	case "reasoning":
		if strings.TrimSpace(item.ID) != "" {
			state.reasoningBegan = true
		}
		if strings.TrimSpace(item.EncryptedContent) != "" {
			state.hasThinking = true
		}
	case "message":
		for _, content := range item.Content {
			text := content.Text
			if text == "" {
				text = content.Refusal
			}
			if text != "" {
				visibleRunes += utf8.RuneCountInString(text)
				state.semanticOutput = true
				continue
			}
			if content.Type != "" && content.Type != "output_text" && content.Type != "refusal" {
				state.semanticOutput = true
			}
		}
	default:
		// function/shell/MCP 等调用项即使没有 usage 与参数 delta 也算有意义输出。
		state.semanticOutput = true
	}
	return visibleRunes
}

func observeGrokQualityResponses(state *grokQualityScanState, payload []byte) {
	var event struct {
		Type     string                   `json:"type"`
		Delta    string                   `json:"delta"`
		Item     grokQualityResponsesItem `json:"item"`
		Response *struct {
			Output []grokQualityResponsesItem `json:"output"`
			Usage  *struct {
				InputTokens         int `json:"input_tokens"`
				OutputTokens        int `json:"output_tokens"`
				TotalTokens         int `json:"total_tokens"`
				OutputTokensDetails struct {
					ReasoningTokens int `json:"reasoning_tokens"`
				} `json:"output_tokens_details"`
			} `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(payload, &event) != nil {
		return
	}
	switch event.Type {
	case "response.completed", "response.incomplete", "response.failed":
		state.terminal = true
	case "response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		if strings.TrimSpace(event.Delta) != "" {
			state.hasThinking = true
		}
	case "response.reasoning.encrypted_content.delta":
		// Grok Messages→Responses 适配器把 signature 投影成该事件,是加密思考实证。
		if strings.TrimSpace(event.Delta) != "" {
			state.reasoningBegan = true
			state.hasThinking = true
		}
	case "response.output_item.added", "response.output_item.done":
		state.aggregateRunes = max(state.aggregateRunes, grokQualityNoteResponsesItem(state, event.Item))
	case "response.output_text.delta":
		grokQualityNoteVisible(state, event.Delta)
	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta", "response.mcp_call_arguments.delta":
		if event.Delta != "" {
			state.semanticOutput = true
		}
	}
	if event.Response != nil {
		if event.Response.Usage != nil {
			state.usageReported = true
			state.usage.InputTokens = event.Response.Usage.InputTokens
			state.usage.OutputTokens = event.Response.Usage.OutputTokens
			state.usage.TotalTokens = event.Response.Usage.TotalTokens
			state.usage.PromptTokens = event.Response.Usage.InputTokens
			state.usage.CompletionTokens = event.Response.Usage.OutputTokens
			state.usage.ReasoningTokens = event.Response.Usage.OutputTokensDetails.ReasoningTokens
		}
		aggregateRunes := 0
		for _, item := range event.Response.Output {
			aggregateRunes += grokQualityNoteResponsesItem(state, item)
		}
		state.aggregateRunes = max(state.aggregateRunes, aggregateRunes)
	}
}

func observeGrokQualityMessages(state *grokQualityScanState, payload []byte) {
	var event struct {
		Type         string `json:"type"`
		ContentBlock struct {
			Type string `json:"type"`
			Text string `json:"text"`
			Data string `json:"data"`
		} `json:"content_block"`
		Delta struct {
			Type        string `json:"type"`
			Text        string `json:"text"`
			Thinking    string `json:"thinking"`
			PartialJSON string `json:"partial_json"`
			Signature   string `json:"signature"`
		} `json:"delta"`
		Usage *struct {
			InputTokens         int `json:"input_tokens"`
			OutputTokens        int `json:"output_tokens"`
			OutputTokensDetails struct {
				ThinkingTokens int `json:"thinking_tokens"`
			} `json:"output_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal(payload, &event) != nil {
		return
	}
	switch event.Type {
	case "message_stop":
		state.terminal = true
	case "content_block_start":
		switch event.ContentBlock.Type {
		case "thinking":
			state.reasoningBegan = true
		case "redacted_thinking":
			state.reasoningBegan = true
			if strings.TrimSpace(event.ContentBlock.Data) != "" {
				state.hasThinking = true
			}
		case "text":
			if event.ContentBlock.Text != "" {
				grokQualityNoteVisible(state, event.ContentBlock.Text)
				state.semanticOutput = true
			}
		case "":
		default:
			state.semanticOutput = true
		}
	case "content_block_delta":
		switch event.Delta.Type {
		case "thinking_delta":
			if strings.TrimSpace(event.Delta.Thinking) != "" {
				state.hasThinking = true
			}
		case "signature_delta":
			// 非空 signature 是加密思考实证。
			if strings.TrimSpace(event.Delta.Signature) != "" {
				state.reasoningBegan = true
				state.hasThinking = true
			}
		case "text_delta":
			grokQualityNoteVisible(state, event.Delta.Text)
		case "input_json_delta":
			if event.Delta.PartialJSON != "" {
				state.semanticOutput = true
			}
		}
	}
	if event.Usage != nil {
		state.usageReported = true
		state.usage.InputTokens = event.Usage.InputTokens
		state.usage.OutputTokens = event.Usage.OutputTokens
		state.usage.PromptTokens = event.Usage.InputTokens
		state.usage.CompletionTokens = event.Usage.OutputTokens
		state.usage.ReasoningTokens = event.Usage.OutputTokensDetails.ThinkingTokens
	}
}

// ---------------------------------------------------------------------------
// 读泵与前缀回放
// ---------------------------------------------------------------------------

type grokQualityReadResult struct {
	data []byte
	err  error
}

// grokQualityReadPump 是上游 body 的唯一读者:让 hold 计时器能在上游 Read
// 阻塞时胜出,判定结束后继续充当剩余流的续读者。
type grokQualityReadPump struct {
	source    io.ReadCloser
	results   chan grokQualityReadResult
	done      chan struct{}
	closeOnce sync.Once
	pending   []byte
	finalErr  error
}

func newGrokQualityReadPump(source io.ReadCloser) *grokQualityReadPump {
	pump := &grokQualityReadPump{
		source:  source,
		results: make(chan grokQualityReadResult),
		done:    make(chan struct{}),
	}
	go pump.run()
	return pump
}

func (p *grokQualityReadPump) run() {
	defer close(p.results)
	buf := make([]byte, 4096)
	for {
		n, err := p.source.Read(buf)
		if n == 0 && err == nil {
			continue
		}
		result := grokQualityReadResult{err: err}
		if n > 0 {
			result.data = append([]byte(nil), buf[:n]...)
		}
		select {
		case p.results <- result:
		case <-p.done:
			return
		}
		if err != nil {
			return
		}
	}
}

func (p *grokQualityReadPump) Read(dst []byte) (int, error) {
	for len(p.pending) == 0 {
		if p.finalErr != nil {
			return 0, p.finalErr
		}
		result, ok := <-p.results
		if !ok {
			p.finalErr = io.EOF
			return 0, io.EOF
		}
		p.pending = result.data
		p.finalErr = result.err
		if len(p.pending) == 0 && p.finalErr != nil {
			return 0, p.finalErr
		}
	}
	n := copy(dst, p.pending)
	p.pending = p.pending[n:]
	return n, nil
}

func (p *grokQualityReadPump) Close() error {
	var err error
	p.closeOnce.Do(func() {
		close(p.done)
		err = p.source.Close()
	})
	return err
}

type grokQualityReplayCloser struct {
	io.Reader
	source io.Closer
}

func (r *grokQualityReplayCloser) Close() error { return r.source.Close() }

func newGrokQualityPrefixReplay(held *bytes.Buffer, rest io.ReadCloser) io.ReadCloser {
	if rest == nil {
		rest = io.NopCloser(bytes.NewReader(nil))
	}
	if held == nil || held.Len() == 0 {
		return rest
	}
	return &grokQualityReplayCloser{Reader: io.MultiReader(bytes.NewReader(held.Bytes()), rest), source: rest}
}

// ---------------------------------------------------------------------------
// peek 主循环
// ---------------------------------------------------------------------------

// peekGrokQualityStream 扣住上游流做质量判定。state 由调用方分配(仅需填 protocol),
// 判定结束后可回读 hasThinking 等证据。返回的 io.ReadCloser 始终可用:
// deliver/wait 时是"已扣帧前缀 + 剩余流"的无损回放;err 非空时回放读到断点处
// 会得到同一错误,由现有断流机制接管。usage 仅在上游已上报时非空。
func peekGrokQualityStream(ctx context.Context, body io.ReadCloser, state *grokQualityScanState, cfg auth.GrokQualityGuardConfig) (io.ReadCloser, grokQualityVerdict, *UsageInfo, error) {
	cfg = auth.NormalizeGrokQualityGuardConfig(cfg)
	if body == nil {
		return io.NopCloser(bytes.NewReader(nil)), grokQualityWait, nil, errGrokQualityEmptyStream
	}
	pump := newGrokQualityReadPump(body)
	var held bytes.Buffer
	holdTimer := time.NewTimer(time.Duration(cfg.HoldTimeoutSec) * time.Second)
	defer holdTimer.Stop()
	for {
		sig := state.signals()
		if verdict := classifyGrokQualityHold(sig, grokQualityMinOutputTokens); verdict != grokQualityWait {
			return newGrokQualityPrefixReplay(&held, pump), verdict, state.reportedUsage(), nil
		}
		// 终态空流必须立刻轮转:等 idle 超时会把 HTTP 200 + 0 token 兜给下游。
		if sig.Terminal {
			return finishGrokQualityPeek(&held, pump, state)
		}
		select {
		case <-ctx.Done():
			return newGrokQualityPrefixReplay(&held, pump), grokQualityWait, state.reportedUsage(), ctx.Err()
		case <-holdTimer.C:
			sig.HoldExpired = true
			if verdict := classifyGrokQualityHold(sig, grokQualityMinOutputTokens); verdict != grokQualityWait {
				return newGrokQualityPrefixReplay(&held, pump), verdict, state.reportedUsage(), nil
			}
			// 超时仍无可见输出:继续等待更多字节或流中断,不把空挂起兜成 200。
		case result, ok := <-pump.results:
			if !ok {
				return finishGrokQualityPeek(&held, pump, state)
			}
			overflow := false
			if len(result.data) > 0 {
				overflow = held.Len()+len(result.data) > grokQualityHoldMaxBufferBytes
				_, _ = held.Write(result.data)
				if !overflow {
					observeGrokQualityChunk(state, result.data)
				}
			}
			if result.err == io.EOF {
				return finishGrokQualityPeek(&held, pump, state)
			}
			if result.err != nil {
				// 断流:错误已被本循环从 pump 消费掉,必须回填给回放器,
				// 否则下游读到断点会得到干净 EOF 而不是原错误,断流被伪装成正常结束。
				pump.finalErr = result.err
				return newGrokQualityPrefixReplay(&held, pump), grokQualityWait, state.reportedUsage(), result.err
			}
			if overflow {
				// 缓冲超上限仍无判定:放行,别为一个开关扣住无界内存。
				return newGrokQualityPrefixReplay(&held, pump), grokQualityDeliver, state.reportedUsage(), nil
			}
		}
	}
}

func (s *grokQualityScanState) reportedUsage() *UsageInfo {
	if !s.usageReported {
		return nil
	}
	usage := s.usage
	return &usage
}

func finishGrokQualityPeek(held *bytes.Buffer, pump *grokQualityReadPump, state *grokQualityScanState) (io.ReadCloser, grokQualityVerdict, *UsageInfo, error) {
	if len(state.pending) > 0 {
		// 上游末行缺换行时补处理最后一条 data 行。
		observeGrokQualityChunk(state, []byte{'\n'})
	}
	state.terminal = true
	sig := state.signals()
	if !sig.HasThinking && sig.OutputTokens <= 0 && sig.VisibleTokens <= 0 && state.usage.ReasoningTokens <= 0 {
		if state.semanticOutput {
			return newGrokQualityPrefixReplay(held, pump), grokQualityDeliver, state.reportedUsage(), nil
		}
		return newGrokQualityPrefixReplay(held, pump), grokQualityWait, state.reportedUsage(), errGrokQualityEmptyStream
	}
	return newGrokQualityPrefixReplay(held, pump), classifyGrokQualityHold(sig, grokQualityMinOutputTokens), state.reportedUsage(), nil
}

// ---------------------------------------------------------------------------
// 准入闸门
// ---------------------------------------------------------------------------

// grokQualityModelShouldReason 判断模型是否默认产生思考:grok-4 起的主线模型。
// 无法解析版本号(如 grok-code-fast-1)、显式 non-reasoning、生图/生视频一律不启用。
func grokQualityModelShouldReason(model string) bool {
	m := strings.ToLower(strings.TrimSpace(model))
	if !strings.HasPrefix(m, "grok-") {
		return false
	}
	if strings.Contains(m, "non-reasoning") {
		return false
	}
	if isGrokImageModel(m) || isGrokVideoModel(m) {
		return false
	}
	version := strings.TrimPrefix(m, "grok-")
	if dash := strings.IndexByte(version, '-'); dash >= 0 {
		version = version[:dash]
	}
	major, err := strconv.Atoi(strings.SplitN(version, ".", 2)[0])
	if err != nil {
		return false
	}
	return major >= 4
}

// grokQualityRequestDisablesReasoning 识别显式关闭思考的请求
// (effort=none / thinking disabled / budget_tokens=0),这类请求无思考是预期。
func grokQualityRequestDisablesReasoning(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(body, &payload) != nil {
		return false
	}
	if grokQualityJSONStringEquals(payload["reasoning_effort"], "none") {
		return true
	}
	for _, key := range []string{"reasoning", "output_config", "thinking"} {
		var nested map[string]json.RawMessage
		if json.Unmarshal(payload[key], &nested) != nil {
			continue
		}
		if grokQualityJSONStringEquals(nested["effort"], "none") || grokQualityJSONStringEquals(nested["type"], "disabled") {
			return true
		}
		var budget int64
		if raw, ok := nested["budget_tokens"]; ok && json.Unmarshal(raw, &budget) == nil && budget == 0 {
			return true
		}
	}
	return grokQualityJSONStringEquals(payload["thinking"], "disabled")
}

func grokQualityJSONStringEquals(raw json.RawMessage, want string) bool {
	var value string
	return json.Unmarshal(raw, &value) == nil && strings.EqualFold(strings.TrimSpace(value), want)
}

// grokQualityRequestHasReplayUnsafeHostedTools 识别声明了上游侧执行工具的请求:
// 重放它们可能重复执行搜索/沙箱/远程 MCP 等有副作用的动作,不启用检测。
// 客户端执行的工具(function/custom 等)安全:扣住期间调用尚未到达客户端。
func grokQualityRequestHasReplayUnsafeHostedTools(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	var payload map[string]any
	if json.Unmarshal(body, &payload) != nil || payload == nil {
		return false
	}
	if raw, exists := payload["web_search_options"]; exists && raw != nil {
		return true
	}
	if raw, exists := payload["mcp_servers"]; exists && raw != nil {
		servers, ok := raw.([]any)
		if !ok || len(servers) > 0 {
			return true
		}
	}
	if grokQualityToolListHasHostedTool(payload["tools"]) {
		return true
	}
	// Responses Tool Search 可在 input 内追加声明;只看 additional_tools 项,
	// 用户/schema 对象里恰好叫 "tools" 的字段不影响判定。
	items, _ := payload["input"].([]any)
	for _, rawItem := range items {
		item, ok := rawItem.(map[string]any)
		if !ok || grokQualityJSONNodeString(item["type"]) != "additional_tools" {
			continue
		}
		if grokQualityToolListHasHostedTool(item["tools"]) {
			return true
		}
	}
	return false
}

func grokQualityToolListHasHostedTool(value any) bool {
	tools, ok := value.([]any)
	if !ok {
		return false
	}
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		switch grokQualityJSONNodeString(tool["type"]) {
		case "", "function", "custom", "local_shell", "apply_patch", "tool_search":
			continue
		case "shell":
			environment, _ := tool["environment"].(map[string]any)
			if grokQualityJSONNodeString(environment["type"]) != "local" {
				return true
			}
		case "namespace":
			if grokQualityToolListHasHostedTool(tool["tools"]) {
				return true
			}
		default:
			// 未知类型默认按上游侧执行处理,包括未来新增的 server/native 工具。
			return true
		}
	}
	return false
}

func grokQualityJSONNodeString(value any) string {
	text, _ := value.(string)
	return strings.ToLower(strings.TrimSpace(text))
}

// grokQualityUpstreamObservableReasoning 判断实际发往上游的请求是否声明了流内
// 可观测的思考产物。没声明的请求(裸 /v1/responses 透传不带 include/summary)
// 上游思考"按设计不可见",健康流与降智流在网关侧长得一模一样,一律不启用检测
// ——本地实测放开这道闸会把健康号连环误判成降智。protocol 是判定器的扫描协议。
func grokQualityUpstreamObservableReasoning(protocol GrokProtocol, upstreamBody []byte) bool {
	if len(upstreamBody) == 0 {
		return false
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(upstreamBody, &payload) != nil {
		return false
	}
	switch protocol {
	case GrokProtocolMessages:
		// 显式开启 thinking 块才有 thinking_delta/signature_delta 可观测。
		var thinking struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(payload["thinking"], &thinking) != nil {
			return false
		}
		return strings.EqualFold(strings.TrimSpace(thinking.Type), "enabled")
	case GrokProtocolChatCompletions:
		// 原生 chat 上游的思考流可观测性未实证,保守不启用(当前号池 catalog
		// 全是 responses 后端,该分支实际打不到)。
		return false
	default:
		var include []string
		if json.Unmarshal(payload["include"], &include) == nil {
			for _, entry := range include {
				if strings.EqualFold(strings.TrimSpace(entry), "reasoning.encrypted_content") {
					return true
				}
			}
		}
		var reasoning struct {
			Summary string `json:"summary"`
		}
		if json.Unmarshal(payload["reasoning"], &reasoning) == nil {
			switch strings.ToLower(strings.TrimSpace(reasoning.Summary)) {
			case "auto", "concise", "detailed":
				return true
			}
		}
		return false
	}
}

func grokQualityGuardAdmits(c *gin.Context, cfg auth.GrokQualityGuardConfig, account *auth.Account, isStream bool, model string, rawBody, responsesBody []byte) bool {
	if !cfg.Enabled || !isStream || account == nil || !account.IsGrokAPI() {
		return false
	}
	if !grokQualityModelShouldReason(model) {
		return false
	}
	// compaction 摘要轮(协议触发或用量触发)绝不能被扣流误判。
	// 没有独立 responses 转换体的 handler(Responses/Anthropic)回落到 rawBody。
	compactionBody := responsesBody
	if len(compactionBody) == 0 {
		compactionBody = rawBody
	}
	meta := requestCompactionMetaForHTTP(c, compactionBody)
	if meta.ProtocolTriggered || meta.UsageTriggered {
		return false
	}
	if grokQualityRequestDisablesReasoning(rawBody) || grokQualityRequestDisablesReasoning(responsesBody) {
		return false
	}
	if grokQualityRequestHasReplayUnsafeHostedTools(rawBody) || grokQualityRequestHasReplayUnsafeHostedTools(responsesBody) {
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// 惩罚阶梯与 handler 接入
// ---------------------------------------------------------------------------

// applyGrokQualityPenalty 首次缺思考冷却 cfg 时长;冷却期满后再犯直接标错误禁用;
// 冷却仍在进行(并发请求已罚)则不动。
func (h *Handler) applyGrokQualityPenalty(account *auth.Account, cfg auth.GrokQualityGuardConfig) {
	if account == nil {
		return
	}
	reason, until := account.GetCooldownSnapshot()
	if reason == grokQualityCooldownReason {
		if time.Now().Before(until) {
			return
		}
		h.store.MarkError(account, "降智检测:冷却期满后再次缺失思考证据,已停用 (quality guard)")
		log.Printf("[quality-guard] 账号 %d 冷却后再犯缺思考,已禁用", account.ID())
		return
	}
	cooldown := time.Duration(cfg.AccountCooldownHours) * time.Hour
	h.store.MarkCooldownWithErrorExactDuration(account, cooldown, grokQualityCooldownReason, "降智检测:流式响应缺失思考证据 (quality guard)")
	log.Printf("[quality-guard] 账号 %d 判定降智,冷却 %s", account.ID(), cooldown)
}

// clearGrokQualityPenaltyLadder 在实证到健康思考后清除本守卫留下的惩罚记录,
// 让"冷却期满后再犯即禁用"的阶梯只作用于连续降智,不跨越健康期。只认本守卫
// 自己写入的 reason,不碰限流/鉴权等其他冷却状态。
func (h *Handler) clearGrokQualityPenaltyLadder(account *auth.Account) {
	if account == nil {
		return
	}
	reason, until := account.GetCooldownSnapshot()
	if reason != grokQualityCooldownReason && reason != grokQualityEmptyStreamReason {
		return
	}
	if time.Now().Before(until) {
		// 冷却仍在进行却收到健康流:并发在途请求的正常放行,不当作恢复证据。
		return
	}
	h.store.ClearCooldown(account)
	log.Printf("[quality-guard] 账号 %d 冷却期满后实证健康思考,惩罚阶梯已重置", account.ID())
}

// grokQualityGuardArgs 汇集 handler 循环现场,供守卫做判定、惩罚与用量日志。
type grokQualityGuardArgs struct {
	Ctx             context.Context
	Account         *auth.Account
	Resp            *http.Response
	Inbound         GrokProtocol
	IsStream        bool
	Endpoint        string
	UpstreamPath    string
	LogModel        string
	EffectiveModel  string // 用量日志的 effective model
	GateModel       string // 准入判定用的实际上游模型
	ReasoningEffort string
	RawBody         []byte
	ResponsesBody   []byte
	UpstreamBody    []byte // 实际发往上游的 responses 规范体(可观测性闸门用)
	Start           time.Time
	Attempt         int
	Attempts        *int // 本请求累计降智换号次数(跨 attempt 持有)
}

func grokQualityDegradedOutcome() streamOutcome {
	return streamOutcome{
		logStatusCode:  http.StatusServiceUnavailable,
		failureKind:    grokQualityFailureKind,
		failureMessage: "上游响应缺少思考过程(降智检测),已尝试的账号均未通过质量判定",
	}
}

// applyGrokQualityGuard 是三个 handler 在拿到 Grok 上游 200 后、进入转发前的
// 统一入口。返回 Proceed 时 resp.Body 已替换为无损回放;返回 Retry/FailClosed
// 时账号惩罚与失败用量日志已完成,body 已关闭,调用方只需做换号簿记或写错误响应。
func (h *Handler) applyGrokQualityGuard(c *gin.Context, args grokQualityGuardArgs) grokQualityGuardAction {
	cfg := h.store.GrokQualityGuardConfig()
	if !grokQualityGuardAdmits(c, cfg, args.Account, args.IsStream, args.GateModel, args.RawBody, args.ResponsesBody) {
		return grokQualityGuardProceed
	}
	protocol := GrokProtocolResponses
	if isGrokNativeRouteResponse(args.Resp) {
		protocol = args.Inbound
	}
	// 上游思考"按设计不可见"的请求(未声明 include: reasoning.encrypted_content
	// 也没开 summary/thinking)健康流与降智流在网关侧一模一样,不可判定——本地实测
	// 放开这道闸会把健康号连环误判。chat/anthropic 入站的 responses 转换
	// (buildChatResponsesRequest / TranslateAnthropicToResponsesForGrok)固定注入
	// include,恒可观测;responses 入站透传取决于客户端自己的声明。
	observable := false
	switch {
	case protocol == GrokProtocolMessages:
		observable = grokQualityUpstreamObservableReasoning(protocol, args.RawBody)
	case protocol == GrokProtocolChatCompletions:
		// 原生 chat 上游的思考流可观测性未实证,保守不启用。
		observable = false
	case args.Inbound == GrokProtocolMessages || args.Inbound == GrokProtocolChatCompletions:
		observable = true
	default:
		observable = grokQualityUpstreamObservableReasoning(protocol, args.UpstreamBody)
	}
	if !observable {
		return grokQualityGuardProceed
	}
	state := &grokQualityScanState{protocol: protocol}
	holdStart := time.Now()
	replay, verdict, peekUsage, peekErr := peekGrokQualityStream(args.Ctx, args.Resp.Body, state, cfg)
	args.Resp.Body = replay
	// 每个被准入的流记一行判定结果:静默的守卫与被闸门全量拒绝的守卫从外部无法区分。
	log.Printf("[quality-guard] 判定完成 account=%d protocol=%s verdict=%s thinking=%v hold_ms=%d err=%v",
		args.Account.ID(), protocol, verdict, state.hasThinking, time.Since(holdStart).Milliseconds(), peekErr)
	emptyStream := errors.Is(peekErr, errGrokQualityEmptyStream)
	if peekErr != nil && !emptyStream {
		// 断流/取消:回放前缀后在断点处复现原错误,交给现有断流重试机制。
		return grokQualityGuardProceed
	}
	if verdict != grokQualityWithhold && !emptyStream {
		if state.hasThinking {
			// 实证健康思考即重置惩罚阶梯:冷却期满后恢复正常的账号不该
			// 因几个月前的一次判罚在下次偶发缺思考时被直接禁用。
			h.clearGrokQualityPenaltyLadder(args.Account)
		}
		return grokQualityGuardProceed
	}

	*args.Attempts++
	canRetry := *args.Attempts < cfg.MaxAttempts
	if emptyStream {
		// 真空流:与降智区分,短冷却不进禁用阶梯。
		h.store.MarkCooldownWithErrorExactDuration(args.Account, grokQualityEmptyStreamCooldown, grokQualityEmptyStreamReason, "降智检测:上游流式响应为空 (quality guard)")
		log.Printf("[quality-guard] 账号 %d 上游空流,冷却 %s 后换号 (attempt %d)", args.Account.ID(), grokQualityEmptyStreamCooldown, *args.Attempts)
	} else {
		h.applyGrokQualityPenalty(args.Account, cfg)
	}
	if !canRetry && cfg.OnExhausted == auth.GrokQualityGuardOnExhaustedFailOpen {
		// fail_open 耗尽:把最后一次响应原样兜给下游(账号已罚)。
		log.Printf("[quality-guard] 换号额度耗尽(%d 次),fail_open 兜底转发账号 %d 的响应", *args.Attempts, args.Account.ID())
		return grokQualityGuardProceed
	}

	_ = args.Resp.Body.Close()
	failureMessage := "上游响应缺少思考过程(降智检测),丢弃响应换号重试"
	if emptyStream {
		failureMessage = "上游流式响应为空(降智检测),换号重试"
	}
	logInput := &database.UsageLogInput{
		AccountID:         args.Account.ID(),
		Endpoint:          args.Endpoint,
		Model:             args.LogModel,
		EffectiveModel:    args.EffectiveModel,
		StatusCode:        http.StatusServiceUnavailable,
		DurationMs:        int(time.Since(args.Start).Milliseconds()),
		ReasoningEffort:   args.ReasoningEffort,
		InboundEndpoint:   args.Endpoint,
		UpstreamEndpoint:  args.UpstreamPath,
		Stream:            true,
		IsRetryAttempt:    canRetry,
		AttemptIndex:      args.Attempt + 1,
		UpstreamErrorKind: grokQualityFailureKind,
		ErrorMessage:      failureMessage,
	}
	if peekUsage != nil {
		logInput.PromptTokens, logInput.CompletionTokens, logInput.TotalTokens = peekUsage.PromptTokens, peekUsage.CompletionTokens, peekUsage.TotalTokens
		logInput.InputTokens, logInput.OutputTokens = peekUsage.InputTokens, peekUsage.OutputTokens
		logInput.ReasoningTokens = peekUsage.ReasoningTokens
	}
	h.logUsageForRequest(c, logInput)
	if canRetry {
		outputTokens := 0
		if peekUsage != nil {
			outputTokens = peekUsage.OutputTokens
		}
		log.Printf("[quality-guard] 丢弃账号 %d 的降智响应换号重试 (quality attempt %d/%d, output_tokens=%d)", args.Account.ID(), *args.Attempts, cfg.MaxAttempts, outputTokens)
		return grokQualityGuardRetry
	}
	log.Printf("[quality-guard] 换号额度耗尽(%d 次),fail_closed 拒绝请求", *args.Attempts)
	return grokQualityGuardFailClosed
}
