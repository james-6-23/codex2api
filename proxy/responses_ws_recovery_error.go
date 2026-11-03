package proxy

import (
	"log"
	"net/http"

	"github.com/codex2api/api"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

func responsesWSErrorStatus(err *api.APIError) int {
	switch string(err.Code) {
	case string(api.ErrCodeResponseContextUnavailable):
		return http.StatusConflict
	case ErrorCodeAccountPoolConcurrencySaturated, ErrorCodeUpstreamWSConnectionCapacity:
		return http.StatusServiceUnavailable
	}
	if err.Type == api.ErrorTypeRateLimit {
		return http.StatusTooManyRequests
	}
	if err.Type == api.ErrorTypeInvalidRequest && api.HTTPStatusCode(err.Code) == http.StatusInternalServerError {
		return http.StatusBadRequest
	}
	return api.HTTPStatusCode(err.Code)
}

func writeResponsesWSRecoveryError(conn *websocket.Conn, err *api.APIError) error {
	recordResponseCacheKnownUnavailableError()
	log.Printf("Responses WebSocket context recovery unavailable: code=%s reason=%s", err.Code, responsesWSContextReason(err))
	return writeResponsesWSError(conn, err)
}

func writeResponsesWSRecoveryErrorForContext(c *gin.Context, conn *websocket.Conn, err *api.APIError) error {
	recordResponseCacheKnownUnavailableError()
	log.Printf("Responses WebSocket context recovery unavailable: code=%s reason=%s", err.Code, responsesWSContextReason(err))
	return writeResponsesWSErrorForContext(c, conn, err)
}
