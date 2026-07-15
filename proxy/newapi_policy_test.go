package proxy

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
)

func TestVerifyNewAPIIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("PROMPT_FILTER_NEWAPI_SECRET", "integration-secret")
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	req := httptest.NewRequest("POST", "/v1/responses", nil)
	req.Header.Set("X-NewAPI-User-ID", "42")
	req.Header.Set("X-NewAPI-Client-IP", "203.0.113.8")
	req.Header.Set("X-NewAPI-Request-ID", "req-test")
	req.Header.Set("X-NewAPI-Timestamp", timestamp)
	canonical := strings.Join([]string{"v1", timestamp, "req-test", "42", "203.0.113.8"}, "\n")
	mac := hmac.New(sha256.New, []byte("integration-secret"))
	_, _ = mac.Write([]byte(canonical))
	req.Header.Set("X-NewAPI-Signature", hex.EncodeToString(mac.Sum(nil)))
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = req
	identity, ok := verifyNewAPIIdentity(c, promptfilter.NewAPIConfig{Enabled: true, MaxClockSkewSeconds: 120})
	if !ok || identity.UserID != "42" || identity.ClientIP != "203.0.113.8" {
		t.Fatalf("verification failed: %#v %v", identity, ok)
	}
	req.Header.Set("X-NewAPI-User-ID", "43")
	if _, ok := verifyNewAPIIdentity(c, promptfilter.NewAPIConfig{Enabled: true, MaxClockSkewSeconds: 120}); ok {
		t.Fatal("tampered identity was accepted")
	}
}
