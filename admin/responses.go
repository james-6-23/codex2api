package admin

import (
	"net/http"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

type errorResponse struct {
	Error string `json:"error"`
}

type messageResponse struct {
	Message string `json:"message"`
}

type statsResponse struct {
	Total         int   `json:"total"`
	Available     int   `json:"available"`
	Error         int   `json:"error"`
	TodayRequests int64 `json:"today_requests"`
}

type accountsResponse struct {
	Accounts []accountResponse `json:"accounts"`
}

type createAccountResponse struct {
	ID      int64  `json:"id"`
	Message string `json:"message"`
}

type healthResponse struct {
	Status    string `json:"status"`
	Available int    `json:"available"`
	Total     int    `json:"total"`
}

type usageLogsResponse struct {
	Logs []*database.UsageLog `json:"logs"`
}

type apiKeysResponse struct {
	Keys []*database.APIKeyRow `json:"keys"`
}

type createAPIKeyResponse struct {
	ID   int64  `json:"id"`
	Key  string `json:"key"`
	Name string `json:"name"`
}

func writeError(c *gin.Context, statusCode int, message string) {
	c.JSON(statusCode, errorResponse{Error: message})
}

func writeMessage(c *gin.Context, statusCode int, message string) {
	c.JSON(statusCode, messageResponse{Message: message})
}

func writeInternalError(c *gin.Context, err error) {
	writeError(c, http.StatusInternalServerError, err.Error())
}
