package proxy

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

const dispatchDiagnosticHeader = "X-Codex2API-Dispatch-Diagnostic"
const dispatchDiagnosticDomain = "codex2api:dispatch-diagnostic:v1"
const dispatchDiagnosticPlaintextSize = 2048
const dispatchPublicMessage = "请求暂时无法处理，请稍后重试"
const dispatchFailureContextKey = "codex2api_dispatch_failure"

type dispatchDiagnosticEnvelope struct {
	RequestID string `json:"request_id"`
	ChannelID int    `json:"channel_id"`
	Status    int    `json:"status"`
	IssuedAt  int64  `json:"issued_at"`
	auth.SelectionDiagnostic
}

type dispatchFailure struct {
	RequestID string
	Envelope  string
}

func selectionTraceForRequest(ctx *gin.Context) *auth.SelectionTrace {
	if ctx == nil || ctx.Request == nil {
		return nil
	}
	return auth.SelectionTraceFromContext(ctx.Request.Context())
}

func beginDispatchSelection(ctx *gin.Context) {
	trace := &auth.SelectionTrace{}
	ctx.Request = ctx.Request.WithContext(auth.WithSelectionTrace(ctx.Request.Context(), trace))
}

func sealDispatchDiagnostic(secret, requestID, userID, platform string, diagnostic dispatchDiagnosticEnvelope, random io.Reader) (string, error) {
	payload, err := json.Marshal(diagnostic)
	if err != nil || len(payload) > dispatchDiagnosticPlaintextSize-2 {
		return "", fmt.Errorf("invalid dispatch diagnostic size")
	}
	key := hmac.New(sha256.New, []byte(secret))
	key.Write([]byte(dispatchDiagnosticDomain))
	block, err := aes.NewCipher(key.Sum(nil))
	if err != nil {
		return "", err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := io.ReadFull(random, nonce); err != nil {
		return "", err
	}
	plaintext := make([]byte, dispatchDiagnosticPlaintextSize)
	binary.BigEndian.PutUint16(plaintext, uint16(len(payload)))
	copy(plaintext[2:], payload)
	aad := strings.Join([]string{dispatchDiagnosticDomain, requestID, userID, platform}, "\n")
	sealed := aead.Seal(nonce, nonce, plaintext, []byte(aad))
	return "v1." + base64.RawURLEncoding.EncodeToString(sealed), nil
}

func (handler *Handler) dispatchFailureForRequest(ctx *gin.Context) dispatchFailure {
	if cached, ok := ctx.Get(dispatchFailureContextKey); ok {
		if failure, valid := cached.(dispatchFailure); valid {
			return failure
		}
	}
	trace := selectionTraceForRequest(ctx)
	trace.Freeze()
	status, verified := handler.cachedNewAPIPolicyAuditState(ctx)
	failure := dispatchFailure{}
	if status == "verified" && verified.MetaVerified && verified.VerificationSecret != "" {
		failure.RequestID = verified.Identity.RequestID
		diagnostic := dispatchDiagnosticEnvelope{
			RequestID: failure.RequestID, ChannelID: verified.Meta.ChannelID,
			Status: http.StatusServiceUnavailable, IssuedAt: time.Now().Unix(),
			SelectionDiagnostic: trace.Snapshot(),
		}
		failure.Envelope, _ = sealDispatchDiagnostic(verified.VerificationSecret, failure.RequestID, verified.Identity.UserID, verified.Platform, diagnostic, rand.Reader)
	}
	if failure.RequestID == "" {
		var identifier [16]byte
		if _, err := rand.Read(identifier[:]); err == nil {
			failure.RequestID = hex.EncodeToString(identifier[:])
		}
	}
	ctx.Set(dispatchFailureContextKey, failure)
	log.Printf("dispatch_failure request_id=%q status=503 protected_diagnostic=%t", failure.RequestID, failure.Envelope != "")
	return failure
}

func (handler *Handler) sendDispatchUnavailable(ctx *gin.Context, stream bool, chat bool) {
	if !ctx.Writer.Written() {
		protocol := continuousRetryProtocolResponses
		if chat {
			protocol = continuousRetryProtocolChat
		}
		if !claimContinuousRetryTerminal(ctx, protocol) {
			return
		}
	}
	failure := handler.dispatchFailureForRequest(ctx)
	if !ctx.Writer.Written() {
		ctx.Header("X-Request-ID", failure.RequestID)
		if failure.Envelope != "" {
			ctx.Header(dispatchDiagnosticHeader, failure.Envelope)
		}
	}
	if stream && ctx.Writer.Written() {
		if chat && writeCommittedChatRetryError(ctx, dispatchPublicMessage) {
			return
		}
		if !chat && writeCommittedResponsesRetryError(ctx, dispatchPublicMessage) {
			return
		}
	}
	ctx.JSON(http.StatusServiceUnavailable, gin.H{"error": gin.H{
		"message": dispatchPublicMessage, "type": ErrorTypeServerError,
		"code": "service_unavailable", "request_id": failure.RequestID,
	}})
}

func dispatchStreamError(ctx *gin.Context, message, code string) gin.H {
	err := gin.H{"message": message, "type": "upstream_error", "code": code}
	if cached, ok := ctx.Get(dispatchFailureContextKey); ok && message == dispatchPublicMessage && (code == "upstream_error" || code == ErrorCodeUpstreamStreamBreak) {
		if failure, valid := cached.(dispatchFailure); valid {
			err["code"] = "service_unavailable"
			err["request_id"] = failure.RequestID
			if failure.Envelope != "" {
				_, _ = ctx.Writer.WriteString(": codex2api_dispatch " + failure.Envelope + "\n\n")
			}
		}
	}
	return err
}

func (handler *Handler) dispatchUnavailableAPIError(ctx *gin.Context) *api.APIError {
	failure := handler.dispatchFailureForRequest(ctx)
	err := api.NewAPIError(api.ErrCodeServiceUnavailable, dispatchPublicMessage, api.ErrorTypeServer)
	details := gin.H{"request_id": failure.RequestID}
	if failure.Envelope != "" {
		details["codex2api_dispatch"] = failure.Envelope
	}
	err.Details = details
	return err
}
