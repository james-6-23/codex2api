package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// realtimeTextSession adapts the text subset of the OpenAI Realtime protocol
// to the Responses WebSocket transport already supported by Codex2API. Audio
// buffer events are rejected explicitly rather than being silently discarded.
type realtimeTextSession struct {
	Model        string
	Instructions string
	Tools        json.RawMessage
	ToolChoice   json.RawMessage
	Items        []json.RawMessage
	History      []json.RawMessage
	historyBytes int
}

const (
	realtimeTextHistoryMaxItems = 64
	realtimeTextHistoryMaxBytes = 256 << 10
)

// RealtimeWebSocket accepts standard Realtime text events from NewAPI and
// translates each response.create turn into a Responses WebSocket turn.
func (h *Handler) RealtimeWebSocket(c *gin.Context) {
	if !isResponsesWebSocketUpgradeRequest(c.Request) {
		api.SendErrorWithStatus(c, api.NewAPIError(
			api.ErrCodeInvalidRequest,
			"WebSocket upgrade required (Upgrade: websocket)",
			api.ErrorTypeInvalidRequest,
		), http.StatusUpgradeRequired)
		return
	}
	if h != nil && h.store != nil {
		cfg := h.promptFilterConfigForRequest(c)
		if h.rejectRequiredNewAPIIdentity(c, cfg.Advanced.NewAPI, nil) {
			return
		}
	}

	conn, err := responsesWSUpgrader.Upgrade(c.Writer, c.Request, newAPIPolicyWebSocketUpgradeHeaders())
	if err != nil {
		return
	}
	conn.SetReadLimit(int64(security.MaxRequestBodySize))
	requestCtx, cancel := context.WithCancel(c.Request.Context())
	c.Request = c.Request.WithContext(requestCtx)
	stopDownstreamKeepalive := startDownstreamWSKeepalive(requestCtx, conn, cancel)
	defer func() {
		cancel()
		_ = conn.Close()
		stopDownstreamKeepalive()
	}()

	state := realtimeTextSession{Model: strings.TrimSpace(c.Query("model"))}
	if err := writeResponsesWSMessage(conn, marshalRealtimeServerEvent(map[string]any{
		"type": "session.created",
		"session": map[string]any{
			"model":             state.Model,
			"output_modalities": []string{"text"},
		},
	})); err != nil {
		return
	}
	for turn := 0; ; turn++ {
		if turn == 0 {
			_ = conn.SetReadDeadline(time.Now().Add(responsesWSFirstMessageTimeout))
		} else {
			_ = conn.SetReadDeadline(time.Time{})
		}
		messageType, payload, err := conn.ReadMessage()
		if err != nil {
			return
		}
		_ = conn.SetReadDeadline(time.Time{})
		if messageType != websocket.TextMessage && messageType != websocket.BinaryMessage {
			_ = writeResponsesWSError(conn, api.NewAPIError(api.ErrCodeInvalidRequest, "unsupported websocket message type", api.ErrorTypeInvalidRequest))
			continue
		}
		payload, forwardedEventID := stripNewAPIPolicyWebSocketEventID(payload)

		ack, forward, apiErr := normalizeRealtimeTextClientEvent(&state, payload)
		if apiErr != nil {
			_ = writeResponsesWSError(conn, apiErr)
			continue
		}
		if len(ack) > 0 {
			if err := writeResponsesWSMessage(conn, ack); err != nil {
				return
			}
		}
		if len(forward) == 0 {
			continue
		}
		if forwardedEventID == "" {
			forwardedEventID = fmt.Sprintf("realtime:%d", turn)
		}
		completed := false
		var completedOutput []json.RawMessage
		options := &responsesWSForwardOptions{
			auditEndpoint:        "/v1/realtime",
			transformClientEvent: realtimeResponsesClientEvent,
			onResponseCompleted: func(data []byte) {
				completed = true
				completedOutput = realtimeResponseHistoryItems(data)
			},
		}
		if err := h.forwardResponsesWebSocketTurn(c, conn, forward, forwardedEventID, options); err != nil {
			if errors.Is(err, errResponsesWSClientGone) {
				return
			}
			var closeErr *responsesWSCloseError
			if errors.As(err, &closeErr) {
				closeResponsesWS(conn, closeErr.code, closeErr.reason)
				return
			}
			closeResponsesWS(conn, websocket.CloseInternalServerErr, "upstream websocket proxy failed")
			return
		}
		if completed {
			state.appendHistory(state.Items...)
			state.appendHistory(completedOutput...)
		}
		state.Items = nil
	}
}

func normalizeRealtimeTextClientEvent(state *realtimeTextSession, raw []byte) (ack []byte, forward []byte, apiErr *api.APIError) {
	if state == nil {
		return nil, nil, api.NewAPIError(api.ErrCodeServerError, "realtime session state is unavailable", api.ErrorTypeServer)
	}
	trimmed := []byte(strings.TrimSpace(string(raw)))
	if len(trimmed) == 0 || len(trimmed) > security.MaxRequestBodySize || !gjson.ValidBytes(trimmed) {
		return nil, nil, api.NewAPIError(api.ErrCodeInvalidRequest, "invalid realtime websocket request payload", api.ErrorTypeInvalidRequest)
	}
	eventType := strings.TrimSpace(gjson.GetBytes(trimmed, "type").String())
	switch eventType {
	case "session.update":
		session := gjson.GetBytes(trimmed, "session")
		if !session.Exists() || !session.IsObject() {
			return nil, nil, api.NewAPIError(api.ErrCodeMissingField, "session is required in session.update", api.ErrorTypeInvalidRequest)
		}
		if model := strings.TrimSpace(session.Get("model").String()); model != "" {
			state.Model = model
		}
		if realtimeModalitiesContainAudio(session.Get("output_modalities")) || realtimeModalitiesContainAudio(session.Get("modalities")) {
			return nil, nil, api.NewAPIError(api.ErrCodeInvalidRequest, "Codex2API /v1/realtime currently supports text modalities only", api.ErrorTypeInvalidRequest)
		}
		if instructions := session.Get("instructions"); instructions.Exists() {
			state.Instructions = instructions.String()
		}
		if tools := session.Get("tools"); tools.Exists() {
			state.Tools = append(state.Tools[:0], tools.Raw...)
		}
		if toolChoice := session.Get("tool_choice"); toolChoice.Exists() {
			state.ToolChoice = append(state.ToolChoice[:0], toolChoice.Raw...)
		}
		return marshalRealtimeServerEvent(map[string]any{
			"type":    "session.updated",
			"session": json.RawMessage(session.Raw),
		}), nil, nil

	case "conversation.item.create":
		item := gjson.GetBytes(trimmed, "item")
		if !item.Exists() || !item.IsObject() {
			return nil, nil, api.NewAPIError(api.ErrCodeMissingField, "item is required in conversation.item.create", api.ErrorTypeInvalidRequest)
		}
		for _, content := range item.Get("content").Array() {
			if strings.Contains(strings.ToLower(content.Get("type").String()), "audio") {
				return nil, nil, api.NewAPIError(api.ErrCodeInvalidRequest, "Codex2API /v1/realtime currently supports text conversation items only", api.ErrorTypeInvalidRequest)
			}
		}
		state.Items = append(state.Items, append(json.RawMessage(nil), item.Raw...))
		return marshalRealtimeServerEvent(map[string]any{
			"type":             "conversation.item.created",
			"previous_item_id": gjson.GetBytes(trimmed, "previous_item_id").String(),
			"item":             json.RawMessage(item.Raw),
		}), nil, nil

	case "response.create":
		body, err := buildResponsesTurnFromRealtime(state, trimmed)
		if err != nil {
			// A failed logical turn must not leak pending user items into the
			// next response.create. The client can resend the intended input as a
			// fresh conversation item after correcting the request.
			state.Items = nil
			return nil, nil, api.NewAPIError(api.ErrCodeInvalidRequest, err.Error(), api.ErrorTypeInvalidRequest)
		}
		return nil, body, nil

	case "ping":
		return marshalRealtimeServerEvent(map[string]any{"type": "pong"}), nil, nil

	case "input_audio_buffer.append", "input_audio_buffer.commit", "input_audio_buffer.clear":
		return nil, nil, api.NewAPIError(api.ErrCodeInvalidRequest, "Codex2API /v1/realtime currently supports text events only", api.ErrorTypeInvalidRequest)

	case "response.cancel":
		// Cancellation is a logical turn boundary even though the Responses
		// compatibility transport does not forward this event upstream.
		state.Items = nil
		return nil, nil, api.NewAPIError(api.ErrCodeInvalidRequest, fmt.Sprintf("realtime event %s is not supported by the Responses compatibility transport", eventType), api.ErrorTypeInvalidRequest)

	case "conversation.item.delete", "conversation.item.truncate":
		return nil, nil, api.NewAPIError(api.ErrCodeInvalidRequest, fmt.Sprintf("realtime event %s is not supported by the Responses compatibility transport", eventType), api.ErrorTypeInvalidRequest)

	default:
		return nil, nil, api.NewAPIError(api.ErrCodeInvalidRequest, fmt.Sprintf("unsupported realtime websocket request type: %s", eventType), api.ErrorTypeInvalidRequest)
	}
}

func buildResponsesTurnFromRealtime(state *realtimeTextSession, raw []byte) ([]byte, error) {
	response := gjson.GetBytes(raw, "response")
	body := make(map[string]any)
	if response.Exists() {
		if !response.IsObject() {
			return nil, fmt.Errorf("response must be an object in response.create")
		}
		if err := json.Unmarshal([]byte(response.Raw), &body); err != nil {
			return nil, fmt.Errorf("invalid response.create response object")
		}
	} else if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("invalid response.create payload")
	}
	delete(body, "event_id")
	delete(body, "response")
	if modalities, ok := body["output_modalities"]; ok {
		rawModalities, _ := json.Marshal(modalities)
		if realtimeModalitiesContainAudio(gjson.ParseBytes(rawModalities)) {
			return nil, fmt.Errorf("Codex2API /v1/realtime currently supports text output only")
		}
		delete(body, "output_modalities")
	}
	for _, unsupported := range []string{"modalities", "voice", "output_audio_format", "input_audio_format", "input_audio_transcription", "turn_detection"} {
		delete(body, unsupported)
	}
	body["type"] = "response.create"
	model, _ := body["model"].(string)
	if strings.TrimSpace(model) == "" {
		body["model"] = state.Model
		model = state.Model
	}
	if strings.TrimSpace(model) == "" {
		return nil, fmt.Errorf("model is required for /v1/realtime (query, session.update, or response.create)")
	}
	if _, ok := body["instructions"]; !ok && state.Instructions != "" {
		body["instructions"] = state.Instructions
	}
	if _, ok := body["tools"]; !ok && len(state.Tools) > 0 {
		body["tools"] = json.RawMessage(state.Tools)
	}
	if _, ok := body["tool_choice"]; !ok && len(state.ToolChoice) > 0 {
		body["tool_choice"] = json.RawMessage(state.ToolChoice)
	}
	if value, ok := body["max_response_output_tokens"]; ok {
		if _, exists := body["max_output_tokens"]; !exists {
			body["max_output_tokens"] = value
		}
		delete(body, "max_response_output_tokens")
	}
	if _, ok := body["input"]; !ok && (len(state.History) > 0 || len(state.Items) > 0) {
		items := make([]json.RawMessage, 0, len(state.History)+len(state.Items))
		items = append(items, state.History...)
		items = append(items, state.Items...)
		body["input"] = items
	}
	if _, ok := body["input"]; !ok {
		return nil, fmt.Errorf("response.create requires prior conversation.item.create input or response.input")
	}
	return json.Marshal(body)
}

func (state *realtimeTextSession) appendHistory(items ...json.RawMessage) {
	if state == nil {
		return
	}
	for _, raw := range items {
		item, ok := sanitizeRealtimeHistoryItem(raw)
		if !ok {
			continue
		}
		state.History = append(state.History, item)
		state.historyBytes += len(item)
	}
	for len(state.History) > 0 && (len(state.History) > realtimeTextHistoryMaxItems || state.historyBytes > realtimeTextHistoryMaxBytes) {
		state.historyBytes -= len(state.History[0])
		state.History = state.History[1:]
	}
}

func sanitizeRealtimeHistoryItem(raw json.RawMessage) (json.RawMessage, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	var item map[string]any
	if err := json.Unmarshal(raw, &item); err != nil || item == nil {
		return nil, false
	}
	itemType, _ := item["type"].(string)
	if itemType == "reasoning" {
		return nil, false
	}
	if itemType != "message" && !isCodexToolCallContextType(itemType) && !strings.HasSuffix(itemType, "_call_output") {
		return nil, false
	}
	delete(item, "id")
	delete(item, "status")
	cleaned, err := json.Marshal(item)
	if err != nil || len(cleaned) == 0 || len(cleaned) > realtimeTextHistoryMaxBytes {
		return nil, false
	}
	return cleaned, true
}

func realtimeResponseHistoryItems(data []byte) []json.RawMessage {
	output := gjson.GetBytes(data, "response.output")
	if !output.IsArray() {
		return nil
	}
	items := make([]json.RawMessage, 0, len(output.Array()))
	output.ForEach(func(_, value gjson.Result) bool {
		if item, ok := sanitizeRealtimeHistoryItem(json.RawMessage(value.Raw)); ok {
			items = append(items, item)
		}
		return true
	})
	return items
}

func realtimeResponsesClientEvent(data []byte) []byte {
	if len(data) == 0 || gjson.GetBytes(data, "type").String() != "response.completed" {
		return data
	}
	transformed, err := sjson.SetBytes(data, "type", "response.done")
	if err != nil {
		return data
	}
	return transformed
}

func realtimeModalitiesContainAudio(value gjson.Result) bool {
	if !value.Exists() {
		return false
	}
	if value.Type == gjson.String {
		return strings.Contains(strings.ToLower(value.String()), "audio")
	}
	for _, item := range value.Array() {
		if strings.Contains(strings.ToLower(item.String()), "audio") {
			return true
		}
	}
	return false
}

func marshalRealtimeServerEvent(payload map[string]any) []byte {
	raw, _ := json.Marshal(payload)
	return raw
}
