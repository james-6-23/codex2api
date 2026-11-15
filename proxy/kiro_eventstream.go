package proxy

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
)

// Kiro 的 generateAssistantResponse 返回 AWS event-stream 二进制帧。

type kiroFrame struct {
	MessageType   string
	EventType     string
	ErrorCode     string
	ExceptionType string
	Payload       []byte
}

type kiroFrameReader struct {
	source  io.Reader
	decoder *eventstream.Decoder
	buffer  []byte
}

func newKiroFrameReader(source io.Reader) *kiroFrameReader {
	return &kiroFrameReader{source: source, decoder: eventstream.NewDecoder()}
}

// Next 返回下一帧；流正常结束时返回 io.EOF。
func (r *kiroFrameReader) Next() (kiroFrame, error) {
	message, err := r.decoder.Decode(r.source, r.buffer[:0])
	if err != nil {
		return kiroFrame{}, err
	}
	r.buffer = message.Payload[:0]
	header := func(name string) string {
		if value := message.Headers.Get(name); value != nil {
			return fmt.Sprint(value.Get())
		}
		return ""
	}
	frame := kiroFrame{
		MessageType:   header(":message-type"),
		EventType:     header(":event-type"),
		ErrorCode:     header(":error-code"),
		ExceptionType: header(":exception-type"),
		Payload:       append([]byte(nil), message.Payload...),
	}
	if frame.MessageType == "" {
		frame.MessageType = "event"
	}
	return frame, nil
}

type kiroAssistantEvent struct {
	Content string `json:"content"`
}

type kiroToolUseEvent struct {
	Name      string `json:"name"`
	ToolUseID string `json:"toolUseId"`
	Input     string `json:"input"`
	Stop      bool   `json:"stop"`
}

type kiroReasoningEvent struct {
	Text            string `json:"text"`
	Signature       string `json:"signature"`
	RedactedContent string `json:"redactedContent"`
}

type kiroMeteringEvent struct {
	Unit       string  `json:"unit"`
	UnitPlural string  `json:"unitPlural"`
	Usage      float64 `json:"usage"`
}

type kiroContextUsageEvent struct {
	ContextUsagePercentage float64 `json:"contextUsagePercentage"`
}

// kiroTokenUsage 是单次模型调用的最终用量快照（不是增量）。
type kiroTokenUsage struct {
	UncachedInputTokens   int `json:"uncachedInputTokens"`
	OutputTokens          int `json:"outputTokens"`
	CacheReadInputTokens  int `json:"cacheReadInputTokens"`
	CacheWriteInputTokens int `json:"cacheWriteInputTokens"`
}

func (u kiroTokenUsage) sanitized() kiroTokenUsage {
	return kiroTokenUsage{max(u.UncachedInputTokens, 0), max(u.OutputTokens, 0), max(u.CacheReadInputTokens, 0), max(u.CacheWriteInputTokens, 0)}
}

type kiroMetadataEvent struct {
	TokenUsage  *kiroTokenUsage `json:"tokenUsage"`
	StopReason  string          `json:"stopReason"`
	StopDetails *struct {
		Refusal *struct {
			Category    string `json:"category"`
			Explanation string `json:"explanation"`
		} `json:"refusal"`
	} `json:"stopDetails"`
}

const kiroContentFilteredStopReason = "CONTENT_FILTERED"

func (m kiroMetadataEvent) contentFiltered() bool {
	return m.StopReason == kiroContentFilteredStopReason
}

func (m kiroMetadataEvent) truncationStopReason() string {
	switch lower := toLowerASCII(m.StopReason); lower {
	case "max_tokens", "stop_sequence", "model_context_window_exceeded":
		return lower
	}
	return ""
}

func toLowerASCII(value string) string {
	out := []byte(value)
	for i, b := range out {
		if b >= 'A' && b <= 'Z' {
			out[i] = b + 32
		}
	}
	return string(out)
}

func decodeKiroPayload[T any](frame kiroFrame) (T, error) {
	var payload T
	if len(frame.Payload) == 0 {
		return payload, nil
	}
	err := json.Unmarshal(frame.Payload, &payload)
	return payload, err
}
