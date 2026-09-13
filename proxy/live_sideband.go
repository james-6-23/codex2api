package proxy

import (
	"context"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

// liveSidebandDialForTest 仅测试替换上游 sideband 拨号。
var liveSidebandDialForTest func(ctx context.Context, record *liveCallRecord, headers http.Header) (*websocket.Conn, error)

// LiveSideband 代理 GET /v1/live/:call_id 与 Codex call_id 控制面 WebSocket。
func (h *Handler) LiveSideband(c *gin.Context) {
	if !liveRequireAllowLive(c) {
		return
	}
	callID, err := url.PathUnescape(strings.TrimSpace(c.Param("call_id")))
	if err != nil || callID == "" {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidRequest, "call_id is required", api.ErrorTypeInvalidRequest))
		return
	}
	record, _, err := h.liveCalls().get(c.Request.Context(), callID)
	if err != nil || record == nil {
		api.SendErrorWithStatus(c, api.NewAPIError(api.ErrCodeResourceNotFound, "Live call not found", api.ErrorTypeNotFound), http.StatusNotFound)
		return
	}
	if record.Controller == liveControllerClosed {
		api.SendErrorWithStatus(c, api.NewAPIError(api.ErrCodeResourceNotFound, "Live call not found", api.ErrorTypeNotFound), http.StatusNotFound)
		return
	}
	if record.APIKeyID != requestAPIKeyID(c) {
		api.SendErrorWithStatus(c, api.NewAPIError(api.ErrCodeInsufficientScope, "Live call belongs to another API key", api.ErrorTypePermission), http.StatusForbidden)
		return
	}

	_, claimed, err := h.liveCalls().claimProxy(c.Request.Context(), record)
	if err != nil || !claimed {
		api.SendErrorWithStatus(c, api.NewAPIError(api.ErrCodeResourceConflict, "Live sideband is already in use", api.ErrorTypeInvalidRequest), http.StatusConflict)
		return
	}

	downstream, err := responsesWSUpgrader.Upgrade(c.Writer, c.Request, newAPIPolicyWebSocketUpgradeHeaders())
	if err != nil {
		go h.liveCalls().finalize(record)
		return
	}
	proxyCtx, cancel := context.WithCancel(c.Request.Context())
	var upstream *websocket.Conn
	stopDownstreamKeepalive := startDownstreamWSKeepalive(proxyCtx, downstream, cancel)
	defer func() {
		cancel()
		_ = downstream.Close()
		if upstream != nil {
			_ = upstream.Close()
		}
		stopDownstreamKeepalive()
	}()

	upstream, err = h.dialLiveSideband(proxyCtx, record)
	if err != nil {
		_ = downstream.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseInternalServerErr, "live sideband unavailable"))
		h.liveCalls().finalize(record)
		return
	}
	errCh := make(chan error, 2)
	go func() { errCh <- copyLiveSideband(proxyCtx, upstream, downstream) }()
	go func() { errCh <- copyLiveSideband(proxyCtx, downstream, upstream) }()
	<-errCh
	cancel()
	h.liveCalls().finalize(record)
}

// copyLiveSideband 在两个 WebSocket 连接之间双向复制一条数据流。
func copyLiveSideband(ctx context.Context, dst, src *websocket.Conn) error {
	if dst == nil || src == nil {
		return errLiveCallNotFound
	}
	go func() {
		<-ctx.Done()
		// Gorilla 允许 Close 与读写并发，直接关闭可同时解除两端阻塞，
		// 也避免与 SetReadDeadline/SetWriteDeadline 竞争。
		_ = src.Close()
		_ = dst.Close()
	}()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		messageType, reader, err := src.NextReader()
		if err != nil {
			return err
		}
		writer, err := dst.NextWriter(messageType)
		if err != nil {
			return err
		}
		if _, err := io.Copy(writer, reader); err != nil {
			_ = writer.Close()
			return err
		}
		if err := writer.Close(); err != nil {
			return err
		}
	}
}

func (h *Handler) dialLiveSideband(ctx context.Context, record *liveCallRecord) (*websocket.Conn, error) {
	headers, err := h.liveSidebandHeaders(ctx, record)
	if err != nil {
		return nil, err
	}
	if liveSidebandDialForTest != nil {
		return liveSidebandDialForTest(ctx, record, headers)
	}
	account := h.liveAccountForRecord(record)
	target := strings.TrimRight(chatGPTLiveSidebandBaseURL, "/") + "/" + url.PathEscape(record.CallID)
	dialer := websocket.Dialer{HandshakeTimeout: 15 * time.Second}
	if account != nil && h.store != nil {
		if proxyURL := strings.TrimSpace(h.store.ResolveProxyForAccount(account)); proxyURL != "" {
			if parsed, parseErr := url.Parse(proxyURL); parseErr == nil {
				dialer.Proxy = http.ProxyURL(parsed)
			}
		}
	}
	conn, resp, err := dialer.DialContext(ctx, target, headers)
	if resp != nil && resp.Body != nil {
		_ = resp.Body.Close()
	}
	if err != nil {
		return nil, err
	}
	return conn, nil
}

func (h *Handler) liveSidebandHeaders(ctx context.Context, record *liveCallRecord) (http.Header, error) {
	account := h.liveAccountForRecord(record)
	if account == nil {
		return nil, errLiveCallNotFound
	}
	attestation, err := h.decryptLiveAttestation(record)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://chatgpt.com/backend-api/codex/"+url.PathEscape(record.CallID), nil)
	if err != nil {
		return nil, err
	}
	h.applyLiveUpstreamHeaders(req, account, attestation, nil, "")
	req.Header.Del("Content-Type")
	req.Header.Del("Accept")
	return req.Header, nil
}

func (h *Handler) liveAccountForRecord(record *liveCallRecord) *auth.Account {
	if h == nil || h.store == nil || record == nil {
		return nil
	}
	if session := h.liveCalls().localSession(record.CallHash); session != nil && session.account != nil {
		return session.account
	}
	return h.store.FindByID(record.AccountID)
}

func (s *liveCallStore) localSession(callHash string) *liveSession {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[callHash]
}
