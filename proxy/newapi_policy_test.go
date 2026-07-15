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

	"github.com/codex2api/cache"
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
	bodyDigest := sha256.Sum256(nil)
	bodyDigestHex := hex.EncodeToString(bodyDigest[:])
	req.Header.Set("X-NewAPI-Method", "POST")
	req.Header.Set("X-NewAPI-Path", "/v1/responses")
	req.Header.Set("X-NewAPI-Body-SHA256", bodyDigestHex)
	canonical := strings.Join([]string{"v1", timestamp, "req-test", "42", "203.0.113.8", "POST", "/v1/responses", bodyDigestHex}, "\n")
	mac := hmac.New(sha256.New, []byte("integration-secret"))
	_, _ = mac.Write([]byte(canonical))
	req.Header.Set("X-NewAPI-Signature", hex.EncodeToString(mac.Sum(nil)))
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = req
	h := &Handler{cache: cache.NewMemory(1)}
	identity, ok := h.verifyNewAPIIdentity(c, promptfilter.NewAPIConfig{Enabled: true, MaxClockSkewSeconds: 120}, nil)
	if !ok || identity.UserID != "42" || identity.ClientIP != "203.0.113.8" {
		t.Fatalf("verification failed: %#v %v", identity, ok)
	}
	if _, ok := h.verifyNewAPIIdentity(c, promptfilter.NewAPIConfig{Enabled: true, MaxClockSkewSeconds: 120}, nil); ok {
		t.Fatal("replayed request ID was accepted")
	}

	// A fresh request ID with a signature for a different body must be rejected.
	req.Header.Set("X-NewAPI-Request-ID", "req-body-tamper")
	canonical = strings.Join([]string{"v1", timestamp, "req-body-tamper", "42", "203.0.113.8", "POST", "/v1/responses", bodyDigestHex}, "\n")
	mac = hmac.New(sha256.New, []byte("integration-secret"))
	_, _ = mac.Write([]byte(canonical))
	req.Header.Set("X-NewAPI-Signature", hex.EncodeToString(mac.Sum(nil)))
	if _, ok := h.verifyNewAPIIdentity(c, promptfilter.NewAPIConfig{Enabled: true, MaxClockSkewSeconds: 120}, []byte("tampered")); ok {
		t.Fatal("tampered body was accepted")
	}

	req.Header.Set("X-NewAPI-User-ID", "43")
	if _, ok := h.verifyNewAPIIdentity(c, promptfilter.NewAPIConfig{Enabled: true, MaxClockSkewSeconds: 120}, nil); ok {
		t.Fatal("tampered identity was accepted")
	}
}
