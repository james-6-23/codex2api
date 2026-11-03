package proxy

import (
	"bytes"
	"fmt"
	"strconv"

	"github.com/codex2api/security/outputguard"
	"github.com/tidwall/gjson"
)

func extractJSONStringPayload(res gjson.Result, data []byte) []byte {
	if res.Index > 0 && len(res.Raw) >= 2 && res.Raw[0] == '"' && res.Raw[len(res.Raw)-1] == '"' {
		rawInner := data[res.Index+1 : res.Index+len(res.Raw)-1]
		if bytes.IndexByte(rawInner, '\\') == -1 {
			return rawInner
		}
	}
	return []byte(res.String())
}

type ObservationAction string

const (
	ActionAppend   ObservationAction = "append"
	ActionSnapshot ObservationAction = "snapshot"
	ActionEnd      ObservationAction = "end"
)

type Observation struct {
	ChannelID     string
	AliasID       string
	Kind          outputguard.ChannelKind
	Action        ObservationAction
	Payload       []byte
	TerminalStage string // 严格限制为 3 种合法语义终态阶段: "arguments.done", "item.done", "response.completed"，其余普通帧置空
}

type ProtocolProjector interface {
	Project(eventType string, data []byte) ([]Observation, error)
}

// -------------------------------------------------------------
// 1. ResponsesProjector (HTTP SSE, WebSocket, Realtime Text)
// -------------------------------------------------------------

type ResponsesProjector struct {
	aliasMap map[string]string
}

func NewResponsesProjector() *ResponsesProjector {
	return &ResponsesProjector{
		aliasMap: make(map[string]string),
	}
}

func (p *ResponsesProjector) Project(eventType string, data []byte) ([]Observation, error) {
	if len(data) == 0 {
		return nil, nil
	}
	t := eventType
	var parsed gjson.Result
	if t == "" {
		parsed = gjson.ParseBytes(data)
		t = parsed.Get("type").String()
	}

	var observations []Observation

	switch t {
	case "response.output_text.delta":
		res := gjson.GetManyBytes(data, "item_id", "delta", "content_index", "output_index")
		itemID := res[0].String()
		cIdx := res[2].Int()
		if itemID == "" {
			itemID = "output_" + strconv.FormatInt(res[3].Int(), 10)
		}
		cid := itemID + ":content:" + strconv.FormatInt(cIdx, 10)
		payload := extractJSONStringPayload(res[1], data)
		observations = append(observations, Observation{
			ChannelID: cid,
			AliasID:   itemID,
			Kind:      outputguard.KindText,
			Action:    ActionAppend,
			Payload:   payload,
		})

	case "response.reasoning.delta", "response.thinking.delta",
		"response.reasoning_text.delta", "response.reasoning_summary_text.delta":
		res := gjson.GetManyBytes(data, "item_id", "delta", "content_index", "output_index", "summary_index")
		itemID := res[0].String()
		cIdx := res[2].Int()
		if itemID == "" {
			itemID = "output_" + strconv.FormatInt(res[3].Int(), 10)
		}
		cid := itemID + ":thinking:" + strconv.FormatInt(cIdx, 10)
		aliasID := itemID + ".thinking"
		// reasoning_summary_text.delta 的 part 分隔符是 summary_index（不携带 content_index），
		// 必须按 summary_index 独立分通道，且 alias 也按 part 隔离：共享 alias 会在
		// RequestGuard.resolveChannelLocked 中把后续 part 合并回首通道，使独立 part 的
		// 字节/重复检查失去意义。
		if t == "response.reasoning_summary_text.delta" {
			sIdx := strconv.FormatInt(res[4].Int(), 10)
			cid = itemID + ":thinking:summary:" + sIdx
			aliasID = itemID + ".thinking.summary." + sIdx
		}
		payload := extractJSONStringPayload(res[1], data)
		observations = append(observations, Observation{
			ChannelID: cid,
			AliasID:   aliasID,
			Kind:      outputguard.KindThinking,
			Action:    ActionAppend,
			Payload:   payload,
		})

	case "response.function_call_arguments.delta":
		res := gjson.GetManyBytes(data, "call_id", "item_id", "delta", "output_index")
		callID := res[0].String()
		itemID := res[1].String()
		targetID := callID
		if targetID == "" {
			targetID = itemID
		}
		if targetID == "" {
			targetID = "output_" + strconv.FormatInt(res[3].Int(), 10)
		}
		payload := extractJSONStringPayload(res[2], data)
		observations = append(observations, Observation{
			ChannelID: targetID,
			AliasID:   itemID,
			Kind:      outputguard.KindToolArguments,
			Action:    ActionAppend,
			Payload:   payload,
		})

	case "response.custom_tool_call_input.delta":
		res := gjson.GetManyBytes(data, "call_id", "item_id", "delta", "output_index")
		callID := res[0].String()
		itemID := res[1].String()
		targetID := callID
		if targetID == "" {
			targetID = itemID
		}
		if targetID == "" {
			targetID = "output_" + strconv.FormatInt(res[3].Int(), 10)
		}
		payload := extractJSONStringPayload(res[2], data)
		observations = append(observations, Observation{
			ChannelID: targetID,
			AliasID:   itemID,
			Kind:      outputguard.KindToolArguments,
			Action:    ActionAppend,
			Payload:   payload,
		})

	case "response.function_call_arguments.done":
		res := gjson.GetManyBytes(data, "call_id", "item_id", "arguments")
		callID := res[0].String()
		itemID := res[1].String()
		targetID := callID
		if targetID == "" {
			targetID = itemID
		}
		payload := extractJSONStringPayload(res[2], data)
		observations = append(observations, Observation{
			ChannelID:     targetID,
			AliasID:       itemID,
			Kind:          outputguard.KindToolArguments,
			Action:        ActionSnapshot,
			Payload:       payload,
			TerminalStage: "arguments.done",
		})

	case "response.custom_tool_call_input.done":
		res := gjson.GetManyBytes(data, "call_id", "item_id", "input")
		callID := res[0].String()
		itemID := res[1].String()
		targetID := callID
		if targetID == "" {
			targetID = itemID
		}
		payload := extractJSONStringPayload(res[2], data)
		observations = append(observations, Observation{
			ChannelID:     targetID,
			AliasID:       itemID,
			Kind:          outputguard.KindToolArguments,
			Action:        ActionSnapshot,
			Payload:       payload,
			TerminalStage: "arguments.done",
		})

	case "response.output_item.done":
		if !parsed.Exists() {
			parsed = gjson.ParseBytes(data)
		}
		item := parsed.Get("item")
		itemType := item.Get("type").String()
		itemID := item.Get("id").String()
		if itemID == "" {
			itemID = parsed.Get("item_id").String()
		}

		if itemType == "function_call" {
			callID := item.Get("call_id").String()
			args := item.Get("arguments").String()
			targetID := callID
			if targetID == "" {
				targetID = itemID
			}
			observations = append(observations, Observation{
				ChannelID:     targetID,
				AliasID:       itemID,
				Kind:          outputguard.KindToolArguments,
				Action:        ActionSnapshot,
				Payload:       []byte(args),
				TerminalStage: "item.done",
			})
		} else if itemType == "custom_tool_call" {
			callID := item.Get("call_id").String()
			inputVal := item.Get("input").String()
			targetID := callID
			if targetID == "" {
				targetID = itemID
			}
			observations = append(observations, Observation{
				ChannelID:     targetID,
				AliasID:       itemID,
				Kind:          outputguard.KindToolArguments,
				Action:        ActionSnapshot,
				Payload:       []byte(inputVal),
				TerminalStage: "item.done",
			})
		} else if itemType == "message" {
			contentArray := item.Get("content").Array()
			for idx, cBlock := range contentArray {
				cType := cBlock.Get("type").String()
				text := cBlock.Get("text").String()
				if cType == "output_text" || (cType == "" && text != "") {
					if text != "" {
						observations = append(observations, Observation{
							ChannelID:     fmt.Sprintf("%s:content:%d", itemID, idx),
							AliasID:       itemID,
							Kind:          outputguard.KindText,
							Action:        ActionSnapshot,
							Payload:       []byte(text),
							TerminalStage: "item.done",
						})
					}
				} else if cType == "reasoning" || cType == "thinking" {
					thinkingText := cBlock.Get("thinking").String()
					if thinkingText == "" {
						thinkingText = cBlock.Get("text").String()
					}
					if thinkingText != "" {
						observations = append(observations, Observation{
							ChannelID:     fmt.Sprintf("%s:thinking:%d", itemID, idx),
							AliasID:       fmt.Sprintf("%s.thinking", itemID),
							Kind:          outputguard.KindThinking,
							Action:        ActionSnapshot,
							Payload:       []byte(thinkingText),
							TerminalStage: "item.done",
						})
					}
				}
			}
		}

	case "response.completed", "response.incomplete":
		if !parsed.Exists() {
			parsed = gjson.ParseBytes(data)
		}
		// SSE 终态事件的 output 通常内嵌于 response；非流式完整 body 则位于顶层。
		outputs := parsed.Get("response.output")
		if !outputs.Exists() {
			outputs = parsed.Get("output")
		}
		for _, item := range outputs.Array() {
			observations = append(observations, p.projectResponsesOutputItem(item, "response.completed")...)
		}

	default:
		// 兼容非流式完整响应 JSON（completed 与 incomplete 均为语义终态）
		if !parsed.Exists() {
			parsed = gjson.ParseBytes(data)
		}
		status := parsed.Get("status").String()
		if (status == "completed" || status == "incomplete") && parsed.Get("output").Exists() {
			for _, item := range parsed.Get("output").Array() {
				observations = append(observations, p.projectResponsesOutputItem(item, "response.completed")...)
			}
		}
	}

	return observations, nil
}

func (p *ResponsesProjector) projectResponsesOutputItem(item gjson.Result, stage string) []Observation {
	var observations []Observation
	itemType := item.Get("type").String()
	itemID := item.Get("id").String()

	if itemType == "function_call" {
		callID := item.Get("call_id").String()
		args := item.Get("arguments").String()
		targetID := callID
		if targetID == "" {
			targetID = itemID
		}
		observations = append(observations, Observation{
			ChannelID:     targetID,
			AliasID:       itemID,
			Kind:          outputguard.KindToolArguments,
			Action:        ActionSnapshot,
			Payload:       []byte(args),
			TerminalStage: stage,
		})
	} else if itemType == "custom_tool_call" {
		callID := item.Get("call_id").String()
		inputVal := item.Get("input").String()
		targetID := callID
		if targetID == "" {
			targetID = itemID
		}
		observations = append(observations, Observation{
			ChannelID:     targetID,
			AliasID:       itemID,
			Kind:          outputguard.KindToolArguments,
			Action:        ActionSnapshot,
			Payload:       []byte(inputVal),
			TerminalStage: stage,
		})
	} else if itemType == "message" {
		contentArray := item.Get("content").Array()
		for idx, cBlock := range contentArray {
			cType := cBlock.Get("type").String()
			text := cBlock.Get("text").String()
			if cType == "output_text" || (cType == "" && text != "") {
				if text != "" {
					observations = append(observations, Observation{
						ChannelID:     fmt.Sprintf("%s:content:%d", itemID, idx),
						AliasID:       itemID,
						Kind:          outputguard.KindText,
						Action:        ActionSnapshot,
						Payload:       []byte(text),
						TerminalStage: stage,
					})
				}
			} else if cType == "reasoning" || cType == "thinking" {
				thinkingText := cBlock.Get("thinking").String()
				if thinkingText == "" {
					thinkingText = cBlock.Get("text").String()
				}
				if thinkingText != "" {
					observations = append(observations, Observation{
						ChannelID:     fmt.Sprintf("%s:thinking:%d", itemID, idx),
						AliasID:       fmt.Sprintf("%s.thinking", itemID),
						Kind:          outputguard.KindThinking,
						Action:        ActionSnapshot,
						Payload:       []byte(thinkingText),
						TerminalStage: stage,
					})
				}
			}
		}
	}
	return observations
}

// -------------------------------------------------------------
// 2. ChatProjector (OpenAI /v1/chat/completions Stream & JSON)
// -------------------------------------------------------------

type ChatProjector struct {
	aliasMap    map[string]string
	choiceTools map[int]map[string]struct{}
}

func NewChatProjector() *ChatProjector {
	return &ChatProjector{
		aliasMap:    make(map[string]string),
		choiceTools: make(map[int]map[string]struct{}),
	}
}

func (p *ChatProjector) Project(eventType string, data []byte) ([]Observation, error) {
	if len(data) == 0 {
		return nil, nil
	}
	parsed := gjson.ParseBytes(data)
	var observations []Observation

	choices := parsed.Get("choices").Array()
	for _, choice := range choices {
		choiceIdx := choice.Get("index").Int()
		finishReason := choice.Get("finish_reason").String()

		// 流式 delta
		delta := choice.Get("delta")
		if delta.Exists() {
			if content := delta.Get("content").String(); content != "" {
				observations = append(observations, Observation{
					ChannelID: fmt.Sprintf("choice_%d.text", choiceIdx),
					Kind:      outputguard.KindText,
					Action:    ActionAppend,
					Payload:   []byte(content),
				})
			}
			reasoning := delta.Get("reasoning_content").String()
			if reasoning == "" {
				reasoning = delta.Get("reasoning").String()
			}
			if reasoning != "" {
				observations = append(observations, Observation{
					ChannelID: fmt.Sprintf("choice_%d.thinking", choiceIdx),
					Kind:      outputguard.KindThinking,
					Action:    ActionAppend,
					Payload:   []byte(reasoning),
				})
			}

			// 标准 tool_calls
			toolCalls := delta.Get("tool_calls").Array()
			for _, tc := range toolCalls {
				toolIdx := int(tc.Get("index").Int())
				callID := tc.Get("id").String()
				channelID := fmt.Sprintf("choice_%d.tool_%d", choiceIdx, toolIdx)
				if callID != "" {
					p.aliasMap[callID] = channelID
				}
				if p.choiceTools[int(choiceIdx)] == nil {
					p.choiceTools[int(choiceIdx)] = make(map[string]struct{})
				}
				p.choiceTools[int(choiceIdx)][channelID] = struct{}{}
				args := tc.Get("function.arguments").String()
				if args != "" {
					observations = append(observations, Observation{
						ChannelID: channelID,
						AliasID:   callID,
						Kind:      outputguard.KindToolArguments,
						Action:    ActionAppend,
						Payload:   []byte(args),
					})
				}
			}

			// 兼容 legacy delta.function_call (Spec §4.1)
			if fnCall := delta.Get("function_call"); fnCall.Exists() {
				args := fnCall.Get("arguments").String()
				channelID := fmt.Sprintf("choice_%d.function_call", choiceIdx)
				if args != "" {
					observations = append(observations, Observation{
						ChannelID: channelID,
						Kind:      outputguard.KindToolArguments,
						Action:    ActionAppend,
						Payload:   []byte(args),
					})
				}
			}
		}

		// 非流式 message
		msg := choice.Get("message")
		if msg.Exists() {
			if content := msg.Get("content").String(); content != "" {
				observations = append(observations, Observation{
					ChannelID:     fmt.Sprintf("choice_%d.text", choiceIdx),
					Kind:          outputguard.KindText,
					Action:        ActionSnapshot,
					Payload:       []byte(content),
					TerminalStage: "response.completed",
				})
			}
			reasoning := msg.Get("reasoning_content").String()
			if reasoning == "" {
				reasoning = msg.Get("reasoning").String()
			}
			if reasoning != "" {
				observations = append(observations, Observation{
					ChannelID:     fmt.Sprintf("choice_%d.thinking", choiceIdx),
					Kind:          outputguard.KindThinking,
					Action:        ActionSnapshot,
					Payload:       []byte(reasoning),
					TerminalStage: "response.completed",
				})
			}

			// 标准 tool_calls
			toolCalls := msg.Get("tool_calls").Array()
			for idx, tc := range toolCalls {
				callID := tc.Get("id").String()
				channelID := fmt.Sprintf("choice_%d.tool_%d", choiceIdx, idx)
				args := tc.Get("function.arguments").String()
				observations = append(observations, Observation{
					ChannelID:     channelID,
					AliasID:       callID,
					Kind:          outputguard.KindToolArguments,
					Action:        ActionSnapshot,
					Payload:       []byte(args),
					TerminalStage: "response.completed",
				})
			}

			// 兼容 legacy message.function_call (Spec §4.1)
			if fnCall := msg.Get("function_call"); fnCall.Exists() {
				args := fnCall.Get("arguments").String()
				channelID := fmt.Sprintf("choice_%d.function_call", choiceIdx)
				observations = append(observations, Observation{
					ChannelID:     channelID,
					Kind:          outputguard.KindToolArguments,
					Action:        ActionSnapshot,
					Payload:       []byte(args),
					TerminalStage: "response.completed",
				})
			}
		}

		// finish_reason 尾检触发 ActionEnd
		if finishReason != "" {
			observations = append(observations, Observation{
				ChannelID: fmt.Sprintf("choice_%d.text", choiceIdx),
				Kind:      outputguard.KindText,
				Action:    ActionEnd,
			})
			observations = append(observations, Observation{
				ChannelID: fmt.Sprintf("choice_%d.thinking", choiceIdx),
				Kind:      outputguard.KindThinking,
				Action:    ActionEnd,
			})
			observations = append(observations, Observation{
				ChannelID: fmt.Sprintf("choice_%d.function_call", choiceIdx),
				Kind:      outputguard.KindToolArguments,
				Action:    ActionEnd,
			})
			if tools, exists := p.choiceTools[int(choiceIdx)]; exists {
				for toolKey := range tools {
					observations = append(observations, Observation{
						ChannelID: toolKey,
						Kind:      outputguard.KindToolArguments,
						Action:    ActionEnd,
					})
				}
			}
		}
	}

	return observations, nil
}

// -------------------------------------------------------------
// 3. AnthropicProjector (Claude /v1/messages Stream & JSON)
// -------------------------------------------------------------

type AnthropicProjector struct {
	scope      string
	blockTypes map[int]string
	blockIDs   map[int]string
}

func NewAnthropicProjector() *AnthropicProjector {
	return NewAnthropicProjectorWithScope("")
}

func NewAnthropicProjectorWithScope(scope string) *AnthropicProjector {
	return &AnthropicProjector{
		scope:      scope,
		blockTypes: make(map[int]string),
		blockIDs:   make(map[int]string),
	}
}

func (p *AnthropicProjector) scopedChannelID(raw string) string {
	if p.scope == "" {
		return raw
	}
	return p.scope + "/" + raw
}

func (p *AnthropicProjector) Project(eventType string, data []byte) ([]Observation, error) {
	if len(data) == 0 {
		return nil, nil
	}
	parsed := gjson.ParseBytes(data)
	t := eventType
	if t == "" {
		t = parsed.Get("type").String()
	}

	var observations []Observation

	switch t {
	case "content_block_start":
		idx := int(parsed.Get("index").Int())
		block := parsed.Get("content_block")
		bType := block.Get("type").String()
		p.blockTypes[idx] = bType
		if bType == "tool_use" {
			p.blockIDs[idx] = block.Get("id").String()
		}
		// Grok Messages 原生路由可能在 start 帧内直接携带正文/工具入参；
		// 这些字节同样必须进入字节预算与重复扫描，不能只记录 block 元信息。
		if bType == "text" {
			if text := block.Get("text").String(); text != "" {
				observations = append(observations, Observation{
					ChannelID: p.scopedChannelID(fmt.Sprintf("block_%d", idx)),
					Kind:      outputguard.KindText,
					Action:    ActionAppend,
					Payload:   []byte(text),
				})
			}
		} else if bType == "tool_use" {
			input := block.Get("input")
			var payload []byte
			if input.IsObject() || input.IsArray() {
				payload = bytes.TrimSpace([]byte(input.Raw))
				if len(payload) <= 2 { // "{}" / "[]" 空占位不产生观察
					payload = nil
				}
			} else if input.Exists() {
				payload = []byte(input.String())
				if len(payload) == 0 {
					payload = nil
				}
			}
			if len(payload) > 0 {
				observations = append(observations, Observation{
					ChannelID: p.scopedChannelID(fmt.Sprintf("block_%d", idx)),
					AliasID:   p.blockIDs[idx],
					Kind:      outputguard.KindToolArguments,
					Action:    ActionAppend,
					Payload:   payload,
				})
			}
		}

	case "content_block_delta":
		idx := int(parsed.Get("index").Int())
		delta := parsed.Get("delta")
		dType := delta.Get("type").String()

		switch dType {
		case "text_delta":
			text := delta.Get("text").String()
			observations = append(observations, Observation{
				ChannelID: p.scopedChannelID(fmt.Sprintf("block_%d", idx)),
				Kind:      outputguard.KindText,
				Action:    ActionAppend,
				Payload:   []byte(text),
			})
		case "thinking_delta":
			thinking := delta.Get("thinking").String()
			observations = append(observations, Observation{
				ChannelID: p.scopedChannelID(fmt.Sprintf("block_%d", idx)),
				Kind:      outputguard.KindThinking,
				Action:    ActionAppend,
				Payload:   []byte(thinking),
			})
		case "input_json_delta":
			partJSON := delta.Get("partial_json").String()
			callID := p.blockIDs[idx]
			observations = append(observations, Observation{
				ChannelID: p.scopedChannelID(fmt.Sprintf("block_%d", idx)),
				AliasID:   callID,
				Kind:      outputguard.KindToolArguments,
				Action:    ActionAppend,
				Payload:   []byte(partJSON),
			})
		}

	case "content_block_stop":
		idx := int(parsed.Get("index").Int())
		bType := p.blockTypes[idx]
		if bType == "tool_use" {
			observations = append(observations, Observation{
				ChannelID: p.scopedChannelID(fmt.Sprintf("block_%d", idx)),
				AliasID:   p.blockIDs[idx],
				Kind:      outputguard.KindToolArguments,
				Action:    ActionEnd,
			})
		} else if bType == "thinking" {
			observations = append(observations, Observation{
				ChannelID: p.scopedChannelID(fmt.Sprintf("block_%d", idx)),
				Kind:      outputguard.KindThinking,
				Action:    ActionEnd,
			})
		} else {
			observations = append(observations, Observation{
				ChannelID: p.scopedChannelID(fmt.Sprintf("block_%d", idx)),
				Kind:      outputguard.KindText,
				Action:    ActionEnd,
			})
		}

	case "message_stop":
		// stop key 与 start key 严格一致 (block_%d)，不造 message_root 伪 channel
		for blockIdx, blockType := range p.blockTypes {
			kind := outputguard.KindText
			if blockType == "tool_use" {
				kind = outputguard.KindToolArguments
			} else if blockType == "thinking" {
				kind = outputguard.KindThinking
			}
			observations = append(observations, Observation{
				ChannelID: p.scopedChannelID(fmt.Sprintf("block_%d", blockIdx)),
				AliasID:   p.blockIDs[blockIdx],
				Kind:      kind,
				Action:    ActionEnd,
			})
		}

	default:
		// 非流式 JSON (type == "message")
		if parsed.Get("type").String() == "message" && parsed.Get("content").IsArray() {
			contentBlocks := parsed.Get("content").Array()
			for idx, b := range contentBlocks {
				bType := b.Get("type").String()
				switch bType {
				case "text":
					text := b.Get("text").String()
					observations = append(observations, Observation{
						ChannelID:     p.scopedChannelID(fmt.Sprintf("block_%d", idx)),
						Kind:          outputguard.KindText,
						Action:        ActionSnapshot,
						Payload:       []byte(text),
						TerminalStage: "response.completed",
					})
				case "thinking":
					thinking := b.Get("thinking").String()
					observations = append(observations, Observation{
						ChannelID:     p.scopedChannelID(fmt.Sprintf("block_%d", idx)),
						Kind:          outputguard.KindThinking,
						Action:        ActionSnapshot,
						Payload:       []byte(thinking),
						TerminalStage: "response.completed",
					})
				case "tool_use":
					callID := b.Get("id").String()
					inputObj := b.Get("input")
					var inputBytes []byte
					if inputObj.IsObject() {
						inputBytes = []byte(inputObj.Raw)
					} else {
						inputBytes = []byte(inputObj.String())
					}
					observations = append(observations, Observation{
						ChannelID:     p.scopedChannelID(fmt.Sprintf("block_%d", idx)),
						AliasID:       callID,
						Kind:          outputguard.KindToolArguments,
						Action:        ActionSnapshot,
						Payload:       inputBytes,
						TerminalStage: "response.completed",
					})
				}
			}
		}
	}

	return observations, nil
}

// -------------------------------------------------------------
// 4. GeminiProjector (Native streamGenerateContent & generateContent)
// -------------------------------------------------------------

type GeminiProjectorMode int

const (
	GeminiProjectorModeAuto GeminiProjectorMode = iota
	GeminiProjectorModeStream
	GeminiProjectorModeNonStream
)

type GeminiProjector struct {
	scope                 string
	candidateToolCounters map[int]int
	candidateToolKeys     map[int][]string
	candidatePartCounts   map[int]int
	mode                  GeminiProjectorMode
	isStream              bool
}

func NewGeminiProjector() *GeminiProjector {
	return NewGeminiProjectorWithScopeAndMode("", GeminiProjectorModeAuto)
}

func NewGeminiProjectorWithMode(mode GeminiProjectorMode) *GeminiProjector {
	return NewGeminiProjectorWithScopeAndMode("", mode)
}

func NewGeminiProjectorWithScopeAndMode(scope string, mode GeminiProjectorMode) *GeminiProjector {
	p := &GeminiProjector{
		scope:                 scope,
		candidateToolCounters: make(map[int]int),
		candidateToolKeys:     make(map[int][]string),
		candidatePartCounts:   make(map[int]int),
		mode:                  mode,
		isStream:              mode == GeminiProjectorModeStream,
	}
	return p
}

func (p *GeminiProjector) scopedChannelID(raw string) string {
	if p.scope == "" {
		return raw
	}
	return p.scope + "/" + raw
}

func (p *GeminiProjector) Project(eventType string, data []byte) ([]Observation, error) {
	if len(data) == 0 {
		return nil, nil
	}
	parsed := gjson.ParseBytes(data)
	var observations []Observation

	candidates := parsed.Get("candidates").Array()
	// 若处于 Auto 模式：当前事件未带 finishReason，说明处于多 chunk 流式传输中；若已进入流式，则后续 chunk 均为流式
	if p.mode == GeminiProjectorModeAuto && !p.isStream && len(candidates) > 0 {
		hasFinishReason := false
		for _, cand := range candidates {
			if cand.Get("finishReason").String() != "" {
				hasFinishReason = true
				break
			}
		}
		if !hasFinishReason {
			p.isStream = true
		}
	}

	for candIdx, cand := range candidates {
		finishReason := cand.Get("finishReason").String()
		parts := cand.Get("content.parts").Array()
		// 非流式整包响应的快照判定不得依赖 finishReason：上游可以省略该字段，
		// 若退化为 Append 就会跳过全文扫描与尾检，使 8KiB~9214B 的重复尾逃逸。
		nonStreamSnapshot := p.mode == GeminiProjectorModeNonStream ||
			(p.mode == GeminiProjectorModeAuto && !p.isStream && finishReason != "")
		// 按 candidate 跟踪已见 part 索引数：收尾 chunk 可能不带 parts。
		if len(parts) > p.candidatePartCounts[candIdx] {
			p.candidatePartCounts[candIdx] = len(parts)
		}

		for partIdx, part := range parts {
			// 1. Text / Thought
			if part.Get("text").Exists() {
				text := part.Get("text").String()
				isThought := part.Get("thought").Bool()
				kind := outputguard.KindText
				suffix := "text"
				if isThought {
					kind = outputguard.KindThinking
					suffix = "thinking"
				}
				action := ActionAppend
				var stage string
				if nonStreamSnapshot {
					action = ActionSnapshot
					stage = "response.completed"
				}
				observations = append(observations, Observation{
					ChannelID:     p.scopedChannelID(fmt.Sprintf("candidate_%d.part_%d.%s", candIdx, partIdx, suffix)),
					Kind:          kind,
					Action:        action,
					Payload:       []byte(text),
					TerminalStage: stage,
				})
			}

			// 2. Function Call (Native object bounded serialization)
			if fnCall := part.Get("functionCall"); fnCall.Exists() {
				callID := fnCall.Get("id").String()
				args := fnCall.Get("args")
				var argBytes []byte
				if args.IsObject() {
					argBytes = []byte(args.Raw)
				} else {
					argBytes = []byte(args.String())
				}

				action := ActionAppend
				var stage string
				if nonStreamSnapshot {
					action = ActionSnapshot
					stage = "response.completed"
				}

				// 稳定映射规则：有 callID 用 callID；无 callID 流式形态使用 candidateToolCounters 跨 chunk 单调计数。
				// 关联限制说明：对于无 ID 工具调用，Gemini 协议缺乏连续性标识，无法跨 chunk 判别同逻辑续传；
				// 原生完整 object 与后续新 call 由请求级 channel 数量及字节预算兜底，不作跨 chunk 合并的绝对宣称。
				var channelID string
				if callID != "" {
					channelID = fmt.Sprintf("candidate_%d.tool_%s", candIdx, callID)
					// 记录有 ID 工具 key 以便 finishReason 触发 ActionEnd 尾检
					found := false
					for _, k := range p.candidateToolKeys[candIdx] {
						if k == channelID {
							found = true
							break
						}
					}
					if !found {
						p.candidateToolKeys[candIdx] = append(p.candidateToolKeys[candIdx], channelID)
					}
				} else {
					currentToolIdx := p.candidateToolCounters[candIdx]
					p.candidateToolCounters[candIdx]++
					channelID = fmt.Sprintf("candidate_%d.tool_idx_%d", candIdx, currentToolIdx)
				}

				observations = append(observations, Observation{
					ChannelID:     p.scopedChannelID(channelID),
					AliasID:       callID,
					Kind:          outputguard.KindToolArguments,
					Action:        action,
					Payload:       argBytes,
					TerminalStage: stage,
				})
			}
		}

		// finishReason 尾检触发 ActionEnd：结束该 candidate 已见的所有 part 索引，
		// 确保无 parts 的收尾 chunk 同样能关闭此前 chunk 打开的 text/thinking channel。
		if finishReason != "" {
			for i := 0; i < p.candidatePartCounts[candIdx]; i++ {
				observations = append(observations, Observation{
					ChannelID: p.scopedChannelID(fmt.Sprintf("candidate_%d.part_%d.text", candIdx, i)),
					Kind:      outputguard.KindText,
					Action:    ActionEnd,
				})
				observations = append(observations, Observation{
					ChannelID: p.scopedChannelID(fmt.Sprintf("candidate_%d.part_%d.thinking", candIdx, i)),
					Kind:      outputguard.KindThinking,
					Action:    ActionEnd,
				})
			}
			if totalTools, exists := p.candidateToolCounters[candIdx]; exists {
				for tIdx := 0; tIdx < totalTools; tIdx++ {
					observations = append(observations, Observation{
						ChannelID: p.scopedChannelID(fmt.Sprintf("candidate_%d.tool_idx_%d", candIdx, tIdx)),
						Kind:      outputguard.KindToolArguments,
						Action:    ActionEnd,
					})
				}
			}
			if toolKeys, exists := p.candidateToolKeys[candIdx]; exists {
				for _, k := range toolKeys {
					observations = append(observations, Observation{
						ChannelID: p.scopedChannelID(k),
						Kind:      outputguard.KindToolArguments,
						Action:    ActionEnd,
					})
				}
			}
		}
	}

	return observations, nil
}

// -------------------------------------------------------------
// 5. ObserveEvent Dispatcher (单上游事件严格计 1 次，流水线批调度)
// -------------------------------------------------------------

func ObserveEvent(guard *outputguard.RequestGuard, projector ProtocolProjector, eventType string, data []byte) error {
	// 心跳/注释帧直接放行，不计入业务事件预算
	trimmed := bytes.TrimSpace(data)
	if bytes.HasPrefix(trimmed, []byte(":")) || eventType == "ping" {
		return nil
	}

	// 步骤 1: 严格每上游业务事件计 1 次 requestEvents（即使是 control/unknown 帧）
	if err := guard.RecordEvent(); err != nil {
		return err
	}

	obs, err := projector.Project(eventType, data)
	if err != nil {
		return err
	}
	if len(obs) == 0 {
		return nil
	}

	// 依次将 observations 推入 guard，执行判定流水线
	// 单 event 所有 obs 检查通过再允许 consumer，一 obs 命中整 event 不可见
	for _, o := range obs {
		switch o.Action {
		case ActionAppend:
			if err := guard.ObserveAppendNoEvent(o.ChannelID, o.AliasID, o.Kind, o.Payload); err != nil {
				return err
			}
		case ActionSnapshot:
			if err := guard.ObserveSnapshotNoEvent(o.ChannelID, o.AliasID, o.Kind, o.Payload, o.TerminalStage); err != nil {
				return err
			}
		case ActionEnd:
			// ActionEnd 目标解析：当 Observation 携带 AliasID（如 tool call_id）时优先使用；
			// 依赖 RequestGuard.aliasMap 内部双向索引反查至对应 primary channel，保持解耦契约。
			target := o.ChannelID
			if o.AliasID != "" {
				target = o.AliasID
			}
			if err := guard.EndChannel(target); err != nil {
				return err
			}
		}
	}

	return nil
}
