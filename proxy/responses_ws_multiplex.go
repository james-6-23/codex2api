package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/codex2api/api"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const responsesWSStreamContextKey = "responses_ws_stream_id"
const responsesWSMaxNamedStreams = 32
const responsesWSMaxActiveStreams = 16
const responsesWSMaxQueuedTurns = 64

// A connection has one reader and serialized data writes even when independent
// lanes run in parallel. Control frames keep gorilla's concurrent-safe path.
var responsesWSWriters sync.Map // map[*websocket.Conn]*sync.Mutex

func registerResponsesWSWriter(conn *websocket.Conn) func() {
	responsesWSWriters.Store(conn, &sync.Mutex{})
	return func() { responsesWSWriters.Delete(conn) }
}

func writeResponsesWSPayload(conn *websocket.Conn, payload []byte, timeout time.Duration) error {
	if conn == nil {
		return errResponsesWSClientGone
	}
	if lock, ok := responsesWSWriters.Load(conn); ok {
		mu := lock.(*sync.Mutex)
		mu.Lock()
		defer mu.Unlock()
	}
	_ = conn.SetWriteDeadline(time.Now().Add(timeout))
	return conn.WriteMessage(websocket.TextMessage, payload)
}

// sendResponsesWSGuardTerminal serializes the guard terminal frame on the
// connection's shared write lock. gorilla/websocket panics on concurrent
// writers, and runResponsesWSMultiplex may run up to 16 same-connection lanes
// in parallel. Unregistered connections (single direct test sockets outside
// the multiplexer) have no competing lane and are written directly.
func sendResponsesWSGuardTerminal(conn *websocket.Conn, streamID string, reason string) error {
	if lock, ok := responsesWSWriters.Load(conn); ok {
		mu := lock.(*sync.Mutex)
		mu.Lock()
		defer mu.Unlock()
	}
	return SendDownstreamGuardTerminalWS(conn, streamID, reason)
}

func responsesWSStreamID(raw []byte) (string, *api.APIError) {
	value := gjson.GetBytes(raw, "stream_id")
	if !value.Exists() {
		return "", nil
	}
	id := value.String()
	valid := value.Type == gjson.String && len(id) > 0 && len(id) <= 256
	for _, ch := range id {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '_' || ch == '-' || ch == '.') {
			valid = false
		}
	}
	if !valid {
		return "", api.NewAPIErrorWithDetails(api.ErrCodeInvalidStreamID, "stream_id must contain 1-256 letters, numbers, underscores, hyphens or periods", api.ErrorTypeInvalidRequest, api.ErrorDetail{Field: "stream_id"})
	}
	return id, nil
}

func withResponsesWSStreamID(payload []byte, streamID string) []byte {
	if streamID == "" {
		return payload
	}
	result, err := sjson.SetBytes(payload, "stream_id", streamID)
	if err != nil {
		return payload
	}
	return result
}

func writeResponsesWSErrorForContext(c *gin.Context, conn *websocket.Conn, err *api.APIError) error {
	return writeResponsesWSErrorForStream(conn, err, c.GetString(responsesWSStreamContextKey), responsesWSWriteTimeout)
}

// Each lane gets frame-local Gin state, headers and an inert HTTP writer:
// after upgrade, request-scoped headers must never mutate the shared handshake.
type responsesWSFrameWriter struct{ header http.Header }

func (w *responsesWSFrameWriter) Header() http.Header { return w.header }
func (*responsesWSFrameWriter) WriteHeader(int)       {}
func (*responsesWSFrameWriter) Write(p []byte) (int, error) {
	return 0, errors.New("HTTP write after WebSocket upgrade")
}
func (*responsesWSFrameWriter) WriteString(string) (int, error) {
	return 0, errors.New("HTTP write after WebSocket upgrade")
}
func (*responsesWSFrameWriter) Status() int              { return http.StatusSwitchingProtocols }
func (*responsesWSFrameWriter) Size() int                { return 0 }
func (*responsesWSFrameWriter) Written() bool            { return true }
func (*responsesWSFrameWriter) WriteHeaderNow()          {}
func (*responsesWSFrameWriter) Flush()                   {}
func (*responsesWSFrameWriter) Pusher() http.Pusher      { return nil }
func (*responsesWSFrameWriter) CloseNotify() <-chan bool { return nil }
func (*responsesWSFrameWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return nil, nil, errors.New("already upgraded")
}

type responsesWSLaneTurn struct {
	message  responsesWSInboundMessage
	streamID string
}

type responsesWSLaneResult struct {
	streamID  string
	err       error
	interrupt *responsesWSInterruptState
}

// The scheduler is connection-owned. Same-lane work is FIFO; other lanes may
// proceed while one upstream is slow. Queued bodies retain the read-pump byte
// and process memory reservations until dispatch or disconnect.
func (h *Handler) runResponsesWSMultiplex(c *gin.Context, conn *websocket.Conn, messages <-chan responsesWSInboundMessage, ctx context.Context) {
	ctx, cancel := context.WithCancel(ctx)
	var workers sync.WaitGroup
	var pending []responsesWSLaneTurn
	defer func() {
		cancel()
		for _, turn := range pending {
			turn.message.releaseQueueBudget()
		}
		_ = conn.Close()
		workers.Wait()
	}()
	results := make(chan responsesWSLaneResult, responsesWSMaxActiveStreams)
	running := map[string]bool{}
	names := map[string]bool{}
	lineage := newResponsesWSLineage()
	interrupts := newResponsesWSInterruptRegistry()
	active := 0
	for {
		for i := 0; i < len(pending) && active < responsesWSMaxActiveStreams; {
			turn := pending[i]
			if running[turn.streamID] {
				i++
				continue
			}
			copy(pending[i:], pending[i+1:])
			pending[len(pending)-1] = responsesWSLaneTurn{}
			pending = pending[:len(pending)-1]
			running[turn.streamID] = true
			active++
			frame := c.Copy()
			// Request.Clone copies context values, including mutable audit
			// pointers. Allocate them per turn so parallel lanes cannot reset
			// each other's request IDs, account traces or timing records.
			frameCtx := context.WithValue(ctx, upstreamTraceContextKey{}, &upstreamTraceAudit{requestID: NewUpstreamSessionUUID(), store: h.store})
			frameCtx = context.WithValue(frameCtx, userAgentAuditContextKey{}, &userAgentAudit{})
			frameCtx = context.WithValue(frameCtx, wsAcquireAuditContextKey{}, &wsAcquireAudit{})
			frameCtx, interrupt := interrupts.begin(frameCtx, turn.streamID)
			frame.Request = c.Request.Clone(frameCtx)
			frame.Writer = &responsesWSFrameWriter{header: c.Writer.Header().Clone()}
			frame.Set(responsesWSStreamContextKey, turn.streamID)
			workers.Add(1)
			go func() {
				defer workers.Done()
				turn.message.releaseQueueBudget()
				payload, eventID := stripNewAPIPolicyWebSocketEventID(turn.message.payload)
				if eventID == "" {
					eventID = fmt.Sprintf("responses:%d", turn.message.turn)
				}
				options := &responsesWSForwardOptions{lineage: lineage, transformClientEvent: func(data []byte) []byte { return withResponsesWSStreamID(data, turn.streamID) }}
				err := h.forwardResponsesWebSocketTurn(frame, conn, payload, eventID, options)
				results <- responsesWSLaneResult{streamID: turn.streamID, err: err, interrupt: interrupt}
			}()
		}
		select {
		case <-ctx.Done():
			return
		case result := <-results:
			active--
			delete(running, result.streamID)
			interrupts.finish(result.interrupt)
			if result.err != nil {
				var closeErr *responsesWSCloseError
				if errors.Is(result.err, errResponsesWSClientGone) {
					if result.streamID == "" || ctx.Err() != nil {
						return
					}
					// A named lane may have been preempted independently. The
					// connection's reader owns detection of actual disconnects.
					continue
				}
				if !errors.As(result.err, &closeErr) {
					return
				}
				// Legacy default-lane close behavior is unchanged. Named request
				// failures have already emitted a lane error; peers can continue.
				var apiErr *api.APIError
				// Upstream account 401/403 failures belong to one request. Only
				// revocation of this connection's verified identity affects peers.
				authFailure := errors.As(closeErr.err, &apiErr) && apiErr.Code == api.ErrorCode("newapi_websocket_binding_changed")
				if result.streamID == "" || authFailure {
					closeResponsesWS(conn, closeErr.code, closeErr.reason)
					return
				}
			}
		case message, ok := <-messages:
			if !ok {
				return
			}
			if message.err != nil {
				message.releaseQueueBudget()
				return
			}
			if message.messageType != websocket.TextMessage && message.messageType != websocket.BinaryMessage {
				message.releaseQueueBudget()
				_ = writeResponsesWSError(conn, api.NewAPIError(api.ErrCodeInvalidRequest, "unsupported websocket message type", api.ErrorTypeInvalidRequest))
				return
			}
			eventType, eventErr := responsesWSClientEventType(message.payload)
			if eventErr != nil {
				message.releaseQueueBudget()
				_ = writeResponsesWSError(conn, eventErr)
				continue
			}
			streamID, streamErr := responsesWSStreamID(message.payload)
			if streamErr != nil {
				message.releaseQueueBudget()
				_ = writeResponsesWSError(conn, streamErr)
				continue
			}
			if eventType == "response.interrupt" {
				err := interrupts.handle(conn, message.payload, streamID)
				message.releaseQueueBudget()
				if err != nil {
					return
				}
				continue
			}
			if eventType != "" && eventType != "response.create" {
				_, _, eventErr := normalizeResponsesWebSocketClientPayload(message.payload)
				message.releaseQueueBudget()
				_ = writeResponsesWSErrorForStream(conn, eventErr, streamID, responsesWSWriteTimeout)
				continue
			}
			if streamID != "" && !names[streamID] {
				if len(names) == responsesWSMaxNamedStreams {
					message.releaseQueueBudget()
					_ = writeResponsesWSErrorForStream(conn, api.NewAPIErrorWithDetails(api.ErrCodeWebsocketStreamLimit, "maximum number of named streams reached (32)", api.ErrorTypeInvalidRequest, api.ErrorDetail{Field: "stream_id"}), streamID, responsesWSWriteTimeout)
					continue
				}
				names[streamID] = true
			}
			queuedInLane := 0
			for _, turn := range pending {
				if turn.streamID == streamID {
					queuedInLane++
				}
			}
			if len(pending) >= responsesWSMaxQueuedTurns || queuedInLane >= responsesWSInboundQueueCapacity {
				message.releaseQueueBudget()
				_ = writeResponsesWSErrorForStream(conn, api.NewAPIError(api.ErrCodeServiceUnavailable, "WebSocket request queue capacity exhausted", api.ErrorTypeServer), streamID, responsesWSOverloadWriteTimeout)
				if streamID == "" || len(pending) >= responsesWSMaxQueuedTurns {
					return
				}
				continue
			}
			pending = append(pending, responsesWSLaneTurn{message: message, streamID: streamID})
		}
	}
}
