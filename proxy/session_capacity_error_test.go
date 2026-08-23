package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codex2api/api"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestSendAccountSessionCapacityErrorIsNonRetryableAndChinese(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)

	SendAccountSessionCapacityError(c)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	if got := gjson.GetBytes(recorder.Body.Bytes(), "error.code").String(); got != string(api.ErrCodeAccountSessionCapacity) {
		t.Fatalf("error.code = %q, want %q", got, api.ErrCodeAccountSessionCapacity)
	}
	if got := gjson.GetBytes(recorder.Body.Bytes(), "error.message").String(); got != accountSessionCapacityExceededMessage {
		t.Fatalf("error.message = %q, want %q", got, accountSessionCapacityExceededMessage)
	}
}
