package wsrelay

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

const (
	readPumpMaxQueuedPayload = 16 * 1024 * 1024
	readPumpMaxQueuedItems   = 4096
	defaultProbeTimeout      = 2 * time.Second
	readPumpCloseCauseWait   = 100 * time.Millisecond
)

var (
	errReadPumpQueueOverflow = errors.New("websocket read pump queue exceeds its payload or item limit")
	errReadPumpIdleFrame     = errors.New("websocket read pump received a business frame without an active lease")
	errReadPumpUncommitted   = errors.New("websocket read pump received a business frame before request write committed")
	errReadPumpStopped       = errors.New("websocket read pump stopped")
	probeSequence            atomic.Uint64
)

type readLeasePhase uint8

const (
	readLeaseIdle readLeasePhase = iota
	readLeaseReserved
	readLeaseWriting
	readLeaseCommitted
)

type readPumpItem struct {
	messageType int
	payload     []byte
	err         error
	leaseID     string
	captured    capturedReadLease
}

type capturedReadLease struct {
	leaseID string
	write   *readLeaseWriteResult
}

type readLeaseWriteResult struct {
	done      chan struct{}
	resolved  bool
	committed bool
	err       error
}

type wsReadState struct {
	mu sync.Mutex

	queue               []readPumpItem
	queuedPayload       int
	activeLease         string
	leasePhase          readLeasePhase
	leaseWrite          *readLeaseWriteResult
	leaseTerminalQueued bool
	pumpStarted         bool
	readerErr           error
	readerStopped       bool

	notify     chan struct{}
	readerDone chan struct{}
	doneOnce   sync.Once
}

func (wc *WsConnection) ensureReadState() *wsReadState {
	wc.readStateOnce.Do(func() {
		wc.readState = &wsReadState{
			notify:     make(chan struct{}, 1),
			readerDone: make(chan struct{}),
		}
	})
	return wc.readState
}

func (state *wsReadState) notifyReaderLocked() {
	select {
	case state.notify <- struct{}{}:
	default:
	}
}

func (state *wsReadState) resolveLeaseWriteLocked(committed bool, writeErr error) {
	result := state.leaseWrite
	if result == nil || result.resolved {
		return
	}
	result.committed = committed
	result.err = writeErr
	result.resolved = true
	close(result.done)
	state.leaseWrite = nil
}

func (wc *WsConnection) installControlHandlers() {
	wc.controlHandlersOnce.Do(func() {
		if wc.conn == nil {
			return
		}
		wc.conn.SetPingHandler(func(appData string) error {
			return wc.conn.WriteControl(
				websocket.PongMessage,
				[]byte(appData),
				time.Now().Add(WriteTimeout),
			)
		})
		wc.conn.SetPongHandler(func(appData string) error {
			if wc.session != nil {
				wc.session.HandlePong()
			}
			wc.Touch()
			wc.notifyProbePong(appData)
			return nil
		})
	})
}

// StartReadPump starts the connection's sole underlying WebSocket reader.
// It is safe to call repeatedly; WsConnection.ReadMessage consumes this pump's
// queue and never reads the Gorilla connection directly.
func (wc *WsConnection) StartReadPump() {
	if wc == nil {
		return
	}
	wc.ensureReadState()
	wc.installControlHandlers()
	wc.readPumpOnce.Do(func() {
		if wc.conn == nil {
			wc.finishReadPump(fmt.Errorf("websocket connection is nil"), true)
			return
		}
		if !wc.IsConnected() {
			wc.finishReadPump(fmt.Errorf("websocket connection is not connected"), true)
			return
		}
		state := wc.ensureReadState()
		state.mu.Lock()
		state.pumpStarted = true
		state.mu.Unlock()
		wc.conn.SetReadLimit(readPumpMaxQueuedPayload)
		go wc.runReadPump()
	})
}

func (wc *WsConnection) runReadPump() {
	for {
		messageType, reader, err := wc.conn.NextReader()
		if err != nil {
			wc.finishReadPump(err, true)
			return
		}
		if messageType != websocket.TextMessage && messageType != websocket.BinaryMessage {
			_, _ = io.Copy(io.Discard, reader)
			continue
		}

		// Capture the lease at the first data frame, before reading the rest of a
		// possibly fragmented message. A later BeginReadLease must never claim a
		// message that started while idle or while the request write was in flight.
		captured, err := wc.captureReadLease()
		if err != nil {
			return
		}
		payload, err := io.ReadAll(reader)
		if err != nil {
			wc.finishReadPumpForLease(err, captured.leaseID)
			return
		}
		// Never wait for the request writer in the sole raw reader. A response can
		// arrive just before WriteMessage returns; queue it with the captured
		// commit result so Ping/Pong/Close frames behind it are still processed.
		wc.Touch()
		if err := wc.enqueueBusinessFrameForCapturedLease(messageType, payload, captured); err != nil {
			wc.finishReadPumpForLease(err, captured.leaseID)
			return
		}
	}
}

func (wc *WsConnection) captureReadLease() (capturedReadLease, error) {
	state := wc.ensureReadState()
	state.mu.Lock()
	leaseID := state.activeLease
	if state.activeLease == "" {
		wc.recordReadPumpFailureLocked(state, errReadPumpIdleFrame, "")
		state.mu.Unlock()
		wc.finalizeReadPumpFailure(state)
		return capturedReadLease{}, errReadPumpIdleFrame
	}
	if state.leaseTerminalQueued {
		err := fmt.Errorf("websocket read pump received a business frame after the terminal frame for request %q", leaseID)
		wc.recordReadPumpFailureLocked(state, err, leaseID)
		state.mu.Unlock()
		wc.finalizeReadPumpFailure(state)
		return capturedReadLease{}, err
	}
	if state.leasePhase == readLeaseWriting && state.leaseWrite != nil {
		captured := capturedReadLease{leaseID: leaseID, write: state.leaseWrite}
		state.mu.Unlock()
		return captured, nil
	}
	if state.leasePhase != readLeaseCommitted {
		err := fmt.Errorf("%w: request %q phase %d", errReadPumpUncommitted, leaseID, state.leasePhase)
		wc.recordReadPumpFailureLocked(state, err, leaseID)
		state.mu.Unlock()
		wc.finalizeReadPumpFailure(state)
		return capturedReadLease{}, err
	}
	state.mu.Unlock()
	return capturedReadLease{leaseID: leaseID}, nil
}

func (wc *WsConnection) awaitCapturedReadLease(captured capturedReadLease) error {
	if captured.write == nil {
		return nil
	}
	<-captured.write.done

	state := wc.ensureReadState()
	state.mu.Lock()
	committed := captured.write.committed
	writeErr := captured.write.err
	resolved := captured.write.resolved
	state.mu.Unlock()
	if resolved && committed {
		return nil
	}
	if writeErr != nil {
		return writeErr
	}
	return fmt.Errorf("%w: request %q did not commit after its write completed", errReadPumpUncommitted, captured.leaseID)
}

func (wc *WsConnection) enqueueBusinessFrameForLease(messageType int, payload []byte, leaseID string) error {
	return wc.enqueueBusinessFrameForCapturedLease(messageType, payload, capturedReadLease{leaseID: leaseID})
}

func (wc *WsConnection) enqueueBusinessFrameForCapturedLease(messageType int, payload []byte, captured capturedReadLease) error {
	state := wc.ensureReadState()
	state.mu.Lock()
	defer state.mu.Unlock()

	leaseID := captured.leaseID
	if leaseID == "" || state.activeLease != leaseID || state.leasePhase != readLeaseCommitted {
		if captured.write == nil || state.activeLease != leaseID {
			return fmt.Errorf("websocket read pump lease changed while reading message for request %q", leaseID)
		}
		if captured.write.resolved {
			if !captured.write.committed {
				if captured.write.err != nil {
					return captured.write.err
				}
				return fmt.Errorf("%w: request %q write did not commit", errReadPumpUncommitted, leaseID)
			}
			if state.leasePhase != readLeaseCommitted {
				return fmt.Errorf("websocket read pump lease changed after request %q committed", leaseID)
			}
		} else if state.leasePhase != readLeaseWriting || state.leaseWrite != captured.write {
			return fmt.Errorf("websocket read pump write changed while reading message for request %q", leaseID)
		}
	}
	if len(state.queue) >= readPumpMaxQueuedItems || len(payload) > readPumpMaxQueuedPayload-state.queuedPayload {
		return errReadPumpQueueOverflow
	}

	state.queue = append(state.queue, readPumpItem{
		messageType: messageType,
		payload:     payload,
		leaseID:     leaseID,
		captured:    captured,
	})
	state.queuedPayload += len(payload)
	if isReadLeaseTerminal(payload) {
		if captured.write != nil && !captured.write.resolved {
			state.leaseTerminalQueued = true
		} else {
			state.activeLease = ""
			state.leasePhase = readLeaseIdle
			state.leaseTerminalQueued = false
		}
	}
	state.notifyReaderLocked()
	return nil
}

func isReadLeaseTerminal(payload []byte) bool {
	switch gjson.GetBytes(payload, "type").String() {
	case "response.completed", "response.failed", "response.done", "error":
		return true
	default:
		return false
	}
}

func (wc *WsConnection) finishReadPump(readErr error, enqueueForActiveLease bool) {
	if readErr == nil {
		readErr = errReadPumpStopped
	}
	state := wc.ensureReadState()
	state.mu.Lock()
	leaseID := ""
	if enqueueForActiveLease {
		leaseID = state.activeLease
	}
	wc.recordReadPumpFailureLocked(state, readErr, leaseID)
	state.mu.Unlock()
	wc.finalizeReadPumpFailure(state)
}

func (wc *WsConnection) finishReadPumpForLease(readErr error, leaseID string) {
	if readErr == nil {
		readErr = errReadPumpStopped
	}
	state := wc.ensureReadState()
	state.mu.Lock()
	wc.recordReadPumpFailureLocked(state, readErr, leaseID)
	state.mu.Unlock()
	wc.finalizeReadPumpFailure(state)
}

// recordReadPumpFailureLocked seals the reader and optionally queues the
// failure for the lease captured when the failing message began. The caller
// holds state.mu, so BeginReadLease cannot slip into the failure boundary.
func (wc *WsConnection) recordReadPumpFailureLocked(state *wsReadState, readErr error, leaseID string) {
	readErr = normalizeReadPumpError(readErr)
	if state.readerStopped {
		return
	}
	deferTerminalCommit := state.leaseTerminalQueued &&
		state.leasePhase == readLeaseWriting &&
		state.leaseWrite != nil &&
		isNormalPeerClose(readErr)
	if leaseID != "" && state.activeLease == leaseID && len(state.queue) < readPumpMaxQueuedItems {
		state.queue = append(state.queue, readPumpItem{err: readErr, leaseID: leaseID})
	}
	if !deferTerminalCommit {
		state.resolveLeaseWriteLocked(false, readErr)
		state.activeLease = ""
		state.leasePhase = readLeaseIdle
		state.leaseTerminalQueued = false
	}
	state.readerErr = readErr
	state.readerStopped = true
	state.notifyReaderLocked()
}

func normalizeReadPumpError(readErr error) error {
	if errors.Is(readErr, websocket.ErrReadLimit) {
		return fmt.Errorf("%w: %w", &websocket.CloseError{
			Code: websocket.CloseMessageTooBig,
			Text: "message too big",
		}, readErr)
	}
	return readErr
}

func isNormalPeerClose(readErr error) bool {
	var closeErr *websocket.CloseError
	if !errors.As(readErr, &closeErr) {
		return false
	}
	return closeErr.Code == websocket.CloseNormalClosure || closeErr.Code == websocket.CloseGoingAway
}

func (wc *WsConnection) finalizeReadPumpFailure(state *wsReadState) {
	wc.readFailureOnce.Do(func() {
		// Make the connection non-reusable only after the real read error has
		// been queued for an active consumer.
		wc.state.Store(int32(StateClosing))
		state.doneOnce.Do(func() { close(state.readerDone) })
		if wc.onReadFailure != nil {
			wc.onReadFailure(wc)
		}
		_ = wc.Close()
		wc.state.Store(int32(StateDisconnected))
	})
}

// BeginReadLease reserves the pump's business-frame boundary for one request.
func (wc *WsConnection) BeginReadLease(requestID string) error {
	if wc == nil {
		return fmt.Errorf("begin websocket read lease: nil connection")
	}
	if requestID == "" {
		return fmt.Errorf("begin websocket read lease: empty request ID")
	}
	if !wc.IsConnected() {
		return fmt.Errorf("begin websocket read lease: connection is not connected")
	}

	state := wc.ensureReadState()
	state.mu.Lock()
	defer state.mu.Unlock()
	if !wc.IsConnected() {
		return fmt.Errorf("begin websocket read lease: connection is not connected")
	}
	if state.readerStopped {
		return fmt.Errorf("begin websocket read lease: reader stopped: %w", state.readerErr)
	}
	if state.activeLease != "" {
		return fmt.Errorf("begin websocket read lease: request %q is already active", state.activeLease)
	}
	if len(state.queue) != 0 {
		return fmt.Errorf("begin websocket read lease: %d unread item(s) remain", len(state.queue))
	}
	state.activeLease = requestID
	state.leasePhase = readLeaseReserved
	state.leaseTerminalQueued = false
	return nil
}

func (wc *WsConnection) beginReadLeaseWrite(messageType int) (string, bool, error) {
	if messageType != websocket.TextMessage && messageType != websocket.BinaryMessage {
		return "", false, nil
	}
	state := wc.ensureReadState()
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.readerStopped {
		return "", false, fmt.Errorf("write websocket request: reader stopped: %w", state.readerErr)
	}
	if state.activeLease == "" {
		return "", false, fmt.Errorf("write websocket request: no active read lease")
	}
	if state.leasePhase != readLeaseReserved {
		return "", false, fmt.Errorf("write websocket request: lease %q phase %d is not reserved", state.activeLease, state.leasePhase)
	}
	if state.leaseWrite != nil {
		return "", false, fmt.Errorf("write websocket request: lease %q already has an active write", state.activeLease)
	}
	state.leaseWrite = &readLeaseWriteResult{done: make(chan struct{})}
	state.leasePhase = readLeaseWriting
	return state.activeLease, true, nil
}

func (wc *WsConnection) ensureReadLeaseForSend(requestID string) error {
	state := wc.ensureReadState()
	state.mu.Lock()
	if state.activeLease == requestID && state.leasePhase == readLeaseReserved {
		state.mu.Unlock()
		return nil
	}
	if state.activeLease != "" {
		activeLease := state.activeLease
		phase := state.leasePhase
		state.mu.Unlock()
		return fmt.Errorf("send websocket request: lease %q is active in phase %d", activeLease, phase)
	}
	if state.readerStopped {
		err := state.readerErr
		state.mu.Unlock()
		return fmt.Errorf("send websocket request: reader stopped: %w", err)
	}
	if len(state.queue) != 0 {
		queued := len(state.queue)
		state.mu.Unlock()
		return fmt.Errorf("send websocket request: %d unread item(s) remain", queued)
	}
	state.mu.Unlock()
	return wc.BeginReadLease(requestID)
}

func (wc *WsConnection) completeReadLeaseWrite(leaseID string, writeErr error) error {
	if writeErr != nil {
		state := wc.ensureReadState()
		state.mu.Lock()
		wc.state.Store(int32(StateClosing))
		state.mu.Unlock()

		resolvedErr := wc.preservePeerCloseCause(writeErr)
		state.mu.Lock()
		if state.readerStopped {
			state.resolveLeaseWriteLocked(false, resolvedErr)
			state.activeLease = ""
			state.leasePhase = readLeaseIdle
			state.leaseTerminalQueued = false
		} else {
			wc.recordReadPumpFailureLocked(state, resolvedErr, leaseID)
		}
		state.mu.Unlock()
		wc.finalizeReadPumpFailure(state)
		return resolvedErr
	}
	state := wc.ensureReadState()
	state.mu.Lock()
	if state.readerStopped || !wc.IsConnected() {
		if state.readerStopped &&
			state.activeLease == leaseID &&
			state.leasePhase == readLeaseWriting &&
			state.leaseWrite != nil &&
			state.leaseTerminalQueued &&
			isNormalPeerClose(state.readerErr) {
			state.activeLease = ""
			state.leasePhase = readLeaseIdle
			state.leaseTerminalQueued = false
			state.resolveLeaseWriteLocked(true, nil)
			state.mu.Unlock()
			return nil
		}
		var err error
		if state.readerErr != nil {
			err = fmt.Errorf("write websocket request: connection failed before lease %q committed: %w", leaseID, state.readerErr)
		} else {
			err = fmt.Errorf("write websocket request: connection failed before lease %q committed", leaseID)
		}
		wc.recordReadPumpFailureLocked(state, err, leaseID)
		state.mu.Unlock()
		wc.finalizeReadPumpFailure(state)
		return err
	}
	if state.activeLease != leaseID || state.leasePhase != readLeaseWriting {
		err := fmt.Errorf("write websocket request: lease %q changed before commit", leaseID)
		wc.state.Store(int32(StateClosing))
		wc.recordReadPumpFailureLocked(state, err, leaseID)
		state.mu.Unlock()
		wc.finalizeReadPumpFailure(state)
		return err
	}
	state.leasePhase = readLeaseCommitted
	if state.leaseTerminalQueued {
		state.activeLease = ""
		state.leasePhase = readLeaseIdle
		state.leaseTerminalQueued = false
	}
	state.resolveLeaseWriteLocked(true, nil)
	state.mu.Unlock()
	return nil
}

// A peer Close can race a data write and surface as ErrCloseSent, broken pipe,
// or connection reset on the writer. Give the sole reader a short,
// error-path-only window to publish the protocol-level close code/reason so
// callers can still distinguish close 1009 and fall back to HTTP.
func (wc *WsConnection) preservePeerCloseCause(writeErr error) error {
	state := wc.ensureReadState()
	readCause := func() (error, bool) {
		state.mu.Lock()
		defer state.mu.Unlock()
		return state.readerErr, state.readerStopped
	}
	if err, stopped := readCause(); stopped {
		if err != nil {
			return fmt.Errorf("websocket write interrupted by peer close: %w", err)
		}
		return writeErr
	}

	state.mu.Lock()
	pumpStarted := state.pumpStarted
	readerDone := state.readerDone
	state.mu.Unlock()
	if !pumpStarted || readerDone == nil {
		return writeErr
	}

	timer := time.NewTimer(readPumpCloseCauseWait)
	defer timer.Stop()
	select {
	case <-readerDone:
		if err, _ := readCause(); err != nil {
			return fmt.Errorf("websocket write interrupted by peer close: %w", err)
		}
	case <-timer.C:
	}
	return writeErr
}

// ensureReadLeaseForResponse preserves compatibility with tests and callers
// that manually construct WsConnection/WsResponse rather than acquiring them
// through Manager. Manager-owned production paths begin the lease before send.
func (wc *WsConnection) ensureReadLeaseForResponse(requestID string) error {
	state := wc.ensureReadState()
	state.mu.Lock()
	if state.activeLease == requestID {
		// Compatibility for tests/callers that manually construct a WsResponse
		// after writing directly to the underlying socket. Manager/Executor paths
		// have already committed the lease in WsConnection.WriteMessage.
		if state.leasePhase == readLeaseReserved && !state.pumpStarted {
			state.leasePhase = readLeaseCommitted
		}
		if state.leasePhase != readLeaseCommitted {
			phase := state.leasePhase
			state.mu.Unlock()
			return fmt.Errorf("websocket read lease %q is not committed (phase %d)", requestID, phase)
		}
		state.mu.Unlock()
		return nil
	}
	if state.activeLease != "" {
		active := state.activeLease
		state.mu.Unlock()
		return fmt.Errorf("websocket read lease belongs to request %q", active)
	}
	if len(state.queue) != 0 {
		leaseID := state.queue[0].leaseID
		state.mu.Unlock()
		if leaseID == requestID {
			return nil
		}
		return fmt.Errorf("websocket read queue belongs to request %q", leaseID)
	}
	legacyManualConnection := !state.pumpStarted
	state.mu.Unlock()
	if !legacyManualConnection {
		return fmt.Errorf("websocket read lease %q was not reserved before the reader started", requestID)
	}
	if err := wc.BeginReadLease(requestID); err != nil {
		return err
	}
	state.mu.Lock()
	if state.activeLease == requestID && state.leasePhase == readLeaseReserved {
		state.leasePhase = readLeaseCommitted
	}
	state.mu.Unlock()
	return nil
}

func (wc *WsConnection) readPumpReusable() bool {
	if wc == nil {
		return false
	}
	state := wc.ensureReadState()
	state.mu.Lock()
	defer state.mu.Unlock()
	return !state.readerStopped && state.activeLease == "" && state.leasePhase == readLeaseIdle && state.leaseWrite == nil && !state.leaseTerminalQueued && len(state.queue) == 0
}

func (wc *WsConnection) waitForEarlyReadFailure(ctx context.Context, grace time.Duration) error {
	state := wc.ensureReadState()
	state.mu.Lock()
	if state.readerStopped {
		err := state.readerErr
		state.mu.Unlock()
		return fmt.Errorf("new websocket reader stopped: %w", err)
	}
	state.mu.Unlock()

	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-state.readerDone:
		state.mu.Lock()
		err := state.readerErr
		state.mu.Unlock()
		return fmt.Errorf("new websocket reader stopped: %w", err)
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// ReadMessage consumes the permanent reader's bounded ordered queue. The
// timeout is applied to this consumer wait, never to Gorilla's permanent read.
func (wc *WsConnection) ReadMessage() (int, []byte, error) {
	if wc == nil {
		return 0, nil, fmt.Errorf("websocket connection is nil")
	}
	state := wc.ensureReadState()
	wc.StartReadPump()

	timer := time.NewTimer(ReadTimeout)
	defer timer.Stop()
	for {
		state.mu.Lock()
		if len(state.queue) != 0 {
			item := state.queue[0]
			state.queue[0] = readPumpItem{}
			state.queue = state.queue[1:]
			state.queuedPayload -= len(item.payload)
			if len(state.queue) == 0 {
				state.queue = nil
			}
			state.mu.Unlock()
			if item.err != nil {
				return item.messageType, item.payload, item.err
			}
			if err := wc.awaitCapturedReadLease(item.captured); err != nil {
				return item.messageType, item.payload, err
			}
			wc.Touch()
			return item.messageType, item.payload, nil
		}
		readerStopped := state.readerStopped
		readerErr := state.readerErr
		state.mu.Unlock()

		if readerStopped {
			if readerErr == nil {
				readerErr = errReadPumpStopped
			}
			return 0, nil, readerErr
		}

		select {
		case <-state.notify:
		case <-state.readerDone:
		case <-timer.C:
			return 0, nil, fmt.Errorf("websocket read timeout after %s", ReadTimeout)
		}
	}
}

func (wc *WsConnection) notifyProbePong(payload string) {
	wc.probeStateMu.Lock()
	if wc.probePayload == payload && wc.probeResult != nil {
		select {
		case wc.probeResult <- struct{}{}:
		default:
		}
	}
	wc.probeStateMu.Unlock()
}

func (wc *WsConnection) acquireProbeGate(state *wsReadState, deadline time.Time) bool {
	wc.probeGateOnce.Do(func() {
		wc.probeGate = make(chan struct{}, 1)
	})
	remaining := time.Until(deadline)
	if remaining <= 0 {
		return false
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case wc.probeGate <- struct{}{}:
		return true
	case <-state.readerDone:
		return false
	case <-timer.C:
		return false
	}
}

func probeConnectionWithTimeout(wc *WsConnection, timeout time.Duration) bool {
	if wc == nil || timeout <= 0 || !wc.IsConnected() || wc.conn == nil {
		return false
	}
	deadline := time.Now().Add(timeout)
	wc.StartReadPump()
	state := wc.ensureReadState()

	if !wc.acquireProbeGate(state, deadline) {
		return false
	}
	defer func() { <-wc.probeGate }()
	if !wc.IsConnected() {
		return false
	}

	payload := fmt.Sprintf("probe-%d-%d", time.Now().UnixNano(), probeSequence.Add(1))
	result := make(chan struct{}, 1)
	wc.probeStateMu.Lock()
	wc.probePayload = payload
	wc.probeResult = result
	wc.probeStateMu.Unlock()
	defer func() {
		wc.probeStateMu.Lock()
		if wc.probeResult == result {
			wc.probePayload = ""
			wc.probeResult = nil
		}
		wc.probeStateMu.Unlock()
	}()

	err := wc.conn.WriteControl(websocket.PingMessage, []byte(payload), deadline)
	if err != nil {
		return false
	}

	remaining := time.Until(deadline)
	if remaining <= 0 {
		return false
	}
	timer := time.NewTimer(remaining)
	defer timer.Stop()
	select {
	case <-result:
		return wc.IsConnected()
	case <-state.readerDone:
		return false
	case <-timer.C:
		return false
	}
}
