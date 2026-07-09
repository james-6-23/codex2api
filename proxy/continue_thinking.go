package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// 本文件实现「思考截断自动续想」：上游按特征指纹截断 reasoning 时
// （reasoning_tokens == 518*n - 2，如 516、1034），自动携带本轮 reasoning
// 续发请求，并把多轮上游响应折叠成一次下游 SSE 响应。
//
// 折叠规则：
//   - reasoning 类 output item 的事件实时转发（重写 sequence_number/output_index）；
//   - message / function_call 等其余 item 的事件先缓冲为「暂定输出」；
//   - 收到 terminal 事件后检查指纹：命中则丢弃暂定输出、开续想轮，
//     否则按原始字节冲刷暂定输出并重构 terminal 事件。

const (
	reasoningTruncationStep    = 518
	continueThinkingMarkerText = "Continue thinking..."
)

// isReasoningTruncationTokens 判断 reasoning token 数是否命中截断指纹（518*n - 2）。
func isReasoningTruncationTokens(tokens int) bool {
	return tokens >= reasoningTruncationStep-2 && (tokens+2)%reasoningTruncationStep == 0
}

// 续想折叠的停止原因。
const (
	continueStopClean        = "clean"              // 上游干净结束（未命中指纹）
	continueStopMaxRounds    = "max_rounds"         // 仍命中指纹但已达最大轮数
	continueStopNoEncrypted  = "no_encrypted"       // 命中指纹但 reasoning 缺 encrypted_content
	continueStopClientGone   = "client_gone"        // 客户端已断开，不再续想
	continueStopUpstreamEOF  = "upstream_eof"       // 上游未发 terminal 就关闭
	continueStopRoundError   = "continuation_error" // 续想轮请求失败（4xx/传输错误）
	continueStopForwardAbort = "forward_abort"      // 下游回调要求中止（交还上层语义）
)

// continueRoundStat 记录一轮上游请求的真实消耗，供逐轮 usage 记账。
type continueRoundStat struct {
	Usage      *UsageInfo
	StatusCode int
	DurationMs int
	ErrMessage string
}

// continueFoldResult 是一次折叠的汇总结果。
type continueFoldResult struct {
	// Rounds 仅含「成功读到终态」的真实上游轮次，按顺序排列；最后一条对应 FinalUsage。
	Rounds []continueRoundStat
	// FailedContinuation 记录失败的续想开轮尝试（openRound 出错 / 非 200 / 组包失败），
	// 与 Rounds 分开，避免逐轮记账时与最终成功轮混淆导致重复计费。
	FailedContinuation *continueRoundStat
	FinalUsage         *UsageInfo     // 最终成功轮的真实 usage（终态计费用）
	FinalResponse      *http.Response // 最终轮响应（body 已关，仅供读 header 做 cooldown/quota 同步）
	GotTerminal        bool           // 客户端是否已收到终态事件（据此判定是否算正常结束）
	ReadErr            error
	StopReason         string
	RoundsRun          int
}

// continueFold 是折叠状态机的外部依赖与配置。
type continueFold struct {
	baseBody  []byte // 本 attempt 实际发出的上游请求体（已过 prepare 管线）
	maxRounds int    // 最大轮数（含首轮）

	forward    func(data []byte) bool                    // 现有下游事件回调，返回 false 表示中止
	observe    func(data []byte)                         // 对被缓冲（未转发）的事件做旁路观察（TTFT 等）
	openRound  func(body []byte) (*http.Response, error) // 用同一账号/通道开续想轮
	clientGone func() bool                               // 客户端是否已断开
}

// bufferedOutputItem 缓冲一个非 reasoning output item 的完整事件序列。
type bufferedOutputItem struct {
	upstreamOI int64
	itemType   string
	events     [][]byte        // 原始事件字节（未重写）
	item       json.RawMessage // 最新的 item 快照（added 或 done 携带）
}

// foldState 是跨轮的折叠内部状态。
type foldState struct {
	seq  int64 // 下游 sequence_number 单调计数
	dsOI int64 // 下游 output_index 单调计数

	baseResponse json.RawMessage   // 第 1 轮 response.created 的 response 对象
	finalOutput  []json.RawMessage // 已定稿的下游 output items（各轮 reasoning + 最终冲刷项）
	replayTail   []json.RawMessage // 续想时追加到 input 的 items（剥 id 的 reasoning + marker）
	firstUsage   *UsageInfo        // 第 1 轮 usage（重构下游 usage 的 input/cached 来源）
	sumReasoning int               // 各轮 reasoning tokens 之和
	flushedAny   bool              // 是否已向客户端冲刷过暂定输出
}

func (st *foldState) nextSeq() int64 {
	s := st.seq
	st.seq++
	return s
}

// rewriteEvent 重写事件的 sequence_number（及可选的 output_index）后返回新字节。
func (st *foldState) rewriteEvent(data []byte, outputIndex int64, setOI bool) []byte {
	out, err := sjson.SetBytes(data, "sequence_number", st.nextSeq())
	if err != nil {
		out = data
	}
	if setOI && gjson.GetBytes(out, "output_index").Exists() {
		if rewritten, err := sjson.SetBytes(out, "output_index", outputIndex); err == nil {
			out = rewritten
		}
	}
	return out
}

// continueMarkerItem 返回续想标记消息：一条 phase:"commentary" 的 assistant
// message，促使模型基于回放的 reasoning 继续思考，而非把上一轮当作终稿。
func continueMarkerItem() json.RawMessage {
	item := map[string]any{
		"type": "message",
		"role": "assistant",
		"content": []map[string]any{
			{"type": "output_text", "text": continueThinkingMarkerText},
		},
		"phase": "commentary",
	}
	raw, _ := json.Marshal(item)
	return raw
}

// buildContinuationBody 在原始请求体基础上替换 input 为 originalInput + replayTail。
// baseBody 已过 prepare 管线（stream=true、include 带 encrypted_content、无
// previous_response_id），这里只需替换 input。
func buildContinuationBody(baseBody []byte, replayTail []json.RawMessage) ([]byte, error) {
	origInput := gjson.GetBytes(baseBody, "input")
	items := make([]json.RawMessage, 0, 8+len(replayTail))
	if origInput.IsArray() {
		origInput.ForEach(func(_, v gjson.Result) bool {
			items = append(items, json.RawMessage(v.Raw))
			return true
		})
	}
	items = append(items, replayTail...)
	merged, err := json.Marshal(items)
	if err != nil {
		return nil, err
	}
	return sjson.SetRawBytes(baseBody, "input", merged)
}

// rebuildTerminalEvent 重构折叠后的下游 terminal 事件：保留第 1 轮的响应
// 身份（id/created_at），采用最终轮的 status/error/incomplete_details，output
// 换成折叠后的完整列表，usage 换成单响应视角的重构值。
func (st *foldState) rebuildTerminalEvent(terminal []byte, finalRoundUsage *UsageInfo) []byte {
	base := st.baseResponse
	if len(base) == 0 {
		base = json.RawMessage(`{}`)
	}
	resp := []byte(base)
	tresp := gjson.GetBytes(terminal, "response")
	for _, key := range []string{"status", "status_code", "error", "incomplete_details", "service_tier"} {
		if v := tresp.Get(key); v.Exists() {
			resp, _ = sjson.SetRawBytes(resp, key, []byte(v.Raw))
		}
	}
	outputRaw, err := json.Marshal(st.finalOutput)
	if err != nil {
		outputRaw = []byte(`[]`)
	}
	resp, _ = sjson.SetRawBytes(resp, "output", outputRaw)
	if usageRaw := st.rebuildUsage(finalRoundUsage); usageRaw != nil {
		resp, _ = sjson.SetRawBytes(resp, "usage", usageRaw)
	}

	eventType := gjson.GetBytes(terminal, "type").String()
	if eventType == "" {
		eventType = "response.completed"
	}
	out := []byte(`{}`)
	out, _ = sjson.SetBytes(out, "type", eventType)
	out, _ = sjson.SetRawBytes(out, "response", resp)
	out, _ = sjson.SetBytes(out, "sequence_number", st.nextSeq())
	return out
}

// rebuildUsage 把多轮 usage 重构成单响应视角：input/cached 取第 1 轮（客户端
// 实际发送的内容），reasoning 各轮求和（都已转发给客户端），output = reasoning
// 总和 + 最终冲刷轮的非 reasoning 部分。隐藏轮的 input 不求和，避免客户端误判
// 上下文已爆窗。
func (st *foldState) rebuildUsage(finalRoundUsage *UsageInfo) []byte {
	if st.firstUsage == nil {
		return nil
	}
	inputTokens := st.firstUsage.InputTokens
	cachedTokens := st.firstUsage.CachedTokens
	finalNonReasoning := 0
	if st.flushedAny && finalRoundUsage != nil {
		finalNonReasoning = finalRoundUsage.OutputTokens - finalRoundUsage.ReasoningTokens
		if finalNonReasoning < 0 {
			finalNonReasoning = 0
		}
	}
	outputTokens := st.sumReasoning + finalNonReasoning

	usage := map[string]any{
		"input_tokens":  inputTokens,
		"output_tokens": outputTokens,
		"total_tokens":  inputTokens + outputTokens,
		"output_tokens_details": map[string]any{
			"reasoning_tokens": st.sumReasoning,
		},
	}
	if cachedTokens > 0 {
		usage["input_tokens_details"] = map[string]any{"cached_tokens": cachedTokens}
	}
	raw, err := json.Marshal(usage)
	if err != nil {
		return nil
	}
	return raw
}

// syntheticIncompleteEvent 合成 response.incomplete terminal 事件（续想轮
// 失败/上游 EOF 时兜底，避免客户端挂死在无终态的流上）。
func (st *foldState) syntheticIncompleteEvent(reason string, finalRoundUsage *UsageInfo) []byte {
	base := st.baseResponse
	if len(base) == 0 {
		base = json.RawMessage(`{}`)
	}
	resp := []byte(base)
	outputRaw, err := json.Marshal(st.finalOutput)
	if err != nil {
		outputRaw = []byte(`[]`)
	}
	resp, _ = sjson.SetRawBytes(resp, "output", outputRaw)
	resp, _ = sjson.SetBytes(resp, "status", "incomplete")
	resp, _ = sjson.SetBytes(resp, "incomplete_details.reason", reason)
	if usageRaw := st.rebuildUsage(finalRoundUsage); usageRaw != nil {
		resp, _ = sjson.SetRawBytes(resp, "usage", usageRaw)
	}
	out := []byte(`{}`)
	out, _ = sjson.SetBytes(out, "type", "response.incomplete")
	out, _ = sjson.SetRawBytes(out, "response", resp)
	out, _ = sjson.SetBytes(out, "sequence_number", st.nextSeq())
	return out
}

// roundOutcome 是单轮读取的结果。
type roundOutcome struct {
	terminal       []byte            // terminal 事件原始字节（nil = EOF 无终态）
	usage          *UsageInfo        // 本轮 usage
	roundReasoning []json.RawMessage // 本轮 reasoning items（output_item.done 的快照）
	buffered       []*bufferedOutputItem
	aborted        bool  // forward 返回 false
	readErr        error // SSE 读取错误
}

// readRound 读完一轮上游 SSE：reasoning 实时转发，其余 item 缓冲，terminal 截停。
func (st *foldState) readRound(body io.Reader, roundNo int, f *continueFold) roundOutcome {
	var out roundOutcome
	// upstream output_index → 下游 output_index（reasoning 类）或缓冲槽
	reasoningOI := map[int64]int64{}
	bufferByOI := map[int64]*bufferedOutputItem{}

	out.readErr = ReadSSEStream(body, func(data []byte) bool {
		parsed := gjson.ParseBytes(data)
		eventType := parsed.Get("type").String()

		switch eventType {
		case "response.created", "response.in_progress":
			if roundNo > 1 {
				// 隐藏轮的生命周期事件不下发，但仍供 TTFT 观察。
				f.observe(data)
				return true
			}
			if eventType == "response.created" {
				if resp := parsed.Get("response"); resp.Exists() {
					st.baseResponse = json.RawMessage(resp.Raw)
				}
			}
			if !f.forward(st.rewriteEvent(data, 0, false)) {
				out.aborted = true
				return false
			}
			return true
		case "response.completed", "response.failed", "response.incomplete":
			out.terminal = append([]byte(nil), data...)
			out.usage = extractUsageFromResult(parsed.Get("response.usage"))
			return false
		}

		oi := parsed.Get("output_index")

		if eventType == "response.output_item.added" {
			item := parsed.Get("item")
			if item.Get("type").String() == "reasoning" {
				ds := st.dsOI
				st.dsOI++
				reasoningOI[oi.Int()] = ds
				if !f.forward(st.rewriteEvent(data, ds, true)) {
					out.aborted = true
					return false
				}
				return true
			}
			entry := &bufferedOutputItem{
				upstreamOI: oi.Int(),
				itemType:   item.Get("type").String(),
				events:     [][]byte{append([]byte(nil), data...)},
				item:       json.RawMessage(item.Raw),
			}
			bufferByOI[oi.Int()] = entry
			out.buffered = append(out.buffered, entry)
			f.observe(data)
			return true
		}

		if oi.Exists() {
			if ds, ok := reasoningOI[oi.Int()]; ok {
				if eventType == "response.output_item.done" {
					if item := parsed.Get("item"); item.Exists() {
						ritem := json.RawMessage(item.Raw)
						out.roundReasoning = append(out.roundReasoning, ritem)
						st.finalOutput = append(st.finalOutput, ritem)
					}
				}
				if !f.forward(st.rewriteEvent(data, ds, true)) {
					out.aborted = true
					return false
				}
				return true
			}
			if entry, ok := bufferByOI[oi.Int()]; ok {
				entry.events = append(entry.events, append([]byte(nil), data...))
				if eventType == "response.output_item.done" {
					if item := parsed.Get("item"); item.Exists() {
						entry.item = json.RawMessage(item.Raw)
					}
				}
				f.observe(data)
				return true
			}
		}

		// 未跟踪到所属 item 的事件（如 response.output_text.annotation 等），
		// 尽力转发，保持与透传路径一致的行为。
		if !f.forward(st.rewriteEvent(data, 0, false)) {
			out.aborted = true
			return false
		}
		return true
	})
	return out
}

// flushBuffered 把一轮缓冲的暂定输出按原始字节冲刷给下游（重写 seq/oi）。
func (st *foldState) flushBuffered(buffered []*bufferedOutputItem, f *continueFold) bool {
	for _, entry := range buffered {
		ds := st.dsOI
		st.dsOI++
		for _, ev := range entry.events {
			if !f.forward(st.rewriteEvent(ev, ds, true)) {
				return false
			}
		}
		if len(entry.item) > 0 {
			st.finalOutput = append(st.finalOutput, entry.item)
		}
	}
	st.flushedAny = true
	return true
}

// shouldContinueRound 判断本轮结束后是否续想，返回 (继续?, 停止原因)。
func shouldContinueRound(terminal []byte, usage *UsageInfo, roundReasoning []json.RawMessage, roundNo, maxRounds int, clientGone bool) (bool, string) {
	if gjson.GetBytes(terminal, "type").String() != "response.completed" {
		return false, continueStopClean
	}
	if usage == nil || !isReasoningTruncationTokens(usage.ReasoningTokens) {
		return false, continueStopClean
	}
	if clientGone {
		return false, continueStopClientGone
	}
	if roundNo >= maxRounds {
		return false, continueStopMaxRounds
	}
	if len(roundReasoning) == 0 {
		return false, continueStopNoEncrypted
	}
	last := roundReasoning[len(roundReasoning)-1]
	if gjson.GetBytes(last, "encrypted_content").String() == "" {
		return false, continueStopNoEncrypted
	}
	return true, ""
}

// runContinueThinkingFold 执行多轮折叠主循环。firstResp 是已确认 2xx 的第 1 轮
// 上游响应（由调用方负责其生命周期外的资源），后续轮通过 f.openRound 打开。
func runContinueThinkingFold(firstResp *http.Response, f *continueFold) continueFoldResult {
	st := &foldState{}
	var result continueFoldResult

	resp := firstResp
	roundNo := 0
	for {
		roundNo++
		result.RoundsRun = roundNo
		result.FinalResponse = resp
		roundStart := time.Now()
		outcome := st.readRound(resp.Body, roundNo, f)
		resp.Body.Close()

		statusCode := http.StatusOK
		if outcome.terminal != nil {
			if t := gjson.GetBytes(outcome.terminal, "type").String(); t == "response.failed" || t == "response.incomplete" {
				// 非 completed 终态一定是最终轮（fold 只在 completed+命中指纹时续想），
				// 这里记录真实状态而非一律 200，便于统计区分。
				statusCode = 0
			}
		}
		stat := continueRoundStat{
			Usage:      outcome.usage,
			StatusCode: statusCode,
			DurationMs: int(time.Since(roundStart).Milliseconds()),
		}
		result.Rounds = append(result.Rounds, stat)
		if roundNo == 1 {
			st.firstUsage = outcome.usage
		}
		if outcome.usage != nil {
			st.sumReasoning += outcome.usage.ReasoningTokens
		}
		result.FinalUsage = outcome.usage
		result.ReadErr = outcome.readErr

		if outcome.aborted {
			result.StopReason = continueStopForwardAbort
			return result
		}

		if outcome.terminal == nil {
			// 上游 EOF 无终态。第 1 轮且从未产出任何输出（含缓冲）时保持静默，让上层的
			// 透明断流重试接管；否则冲刷已缓冲内容 + 合成 incomplete 给客户端一个终态，
			// 避免缓冲内容被丢弃或客户端收到空的 200 流。
			result.StopReason = continueStopUpstreamEOF
			if roundNo == 1 && len(st.finalOutput) == 0 && !st.flushedAny && len(outcome.buffered) == 0 {
				return result
			}
			if !st.flushBuffered(outcome.buffered, f) {
				result.StopReason = continueStopForwardAbort
				return result
			}
			f.forward(st.syntheticIncompleteEvent("upstream_eof", outcome.usage))
			result.GotTerminal = true
			return result
		}
		result.GotTerminal = true

		doContinue, stopReason := shouldContinueRound(
			outcome.terminal, outcome.usage, outcome.roundReasoning,
			roundNo, f.maxRounds, f.clientGone(),
		)

		if !doContinue {
			// 第 1 轮直接 failed 且从未向客户端写过折叠输出：原样透传 terminal，
			// 保证现有的失败抑制 / 换号 / cooldown 语义字节级不变。
			if roundNo == 1 && len(st.finalOutput) == 0 && !st.flushedAny && len(outcome.buffered) == 0 &&
				gjson.GetBytes(outcome.terminal, "type").String() != "response.completed" {
				f.forward(outcome.terminal)
				result.StopReason = stopReason
				return result
			}
			if !st.flushBuffered(outcome.buffered, f) {
				result.StopReason = continueStopForwardAbort
				return result
			}
			f.forward(st.rebuildTerminalEvent(outcome.terminal, outcome.usage))
			result.StopReason = stopReason
			return result
		}

		// 续想：本轮 reasoning 剥 id 后连同 marker 追加到回放尾巴。
		for _, ritem := range outcome.roundReasoning {
			if stripped, ok := stripResponseItemID(ritem); ok {
				st.replayTail = append(st.replayTail, stripped)
			}
		}
		st.replayTail = append(st.replayTail, continueMarkerItem())

		nextBody, err := buildContinuationBody(f.baseBody, st.replayTail)
		if err != nil {
			result.FailedContinuation = &continueRoundStat{ErrMessage: err.Error()}
			f.forward(st.syntheticIncompleteEvent("upstream_error", outcome.usage))
			result.StopReason = continueStopRoundError
			return result
		}
		nextResp, err := f.openRound(nextBody)
		if err != nil {
			result.FailedContinuation = &continueRoundStat{ErrMessage: err.Error()}
			f.forward(st.syntheticIncompleteEvent("upstream_error", outcome.usage))
			result.StopReason = continueStopRoundError
			return result
		}
		if nextResp.StatusCode != http.StatusOK {
			errBody, _ := io.ReadAll(io.LimitReader(nextResp.Body, 2048))
			nextResp.Body.Close()
			result.FailedContinuation = &continueRoundStat{
				StatusCode: nextResp.StatusCode,
				ErrMessage: fmt.Sprintf("continuation round rejected: %s", upstreamErrorConsoleBody(errBody)),
			}
			f.forward(st.syntheticIncompleteEvent("upstream_error", outcome.usage))
			result.StopReason = continueStopRoundError
			return result
		}
		resp = nextResp
	}
}
