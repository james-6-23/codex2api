package main

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestConfigureTrustedProxiesRejectsForwardedForSpoofing(t *testing.T) {
	gin.SetMode(gin.TestMode)

	r := gin.New()
	if err := configureTrustedProxies(r); err != nil {
		t.Fatalf("configureTrustedProxies() error = %v", err)
	}
	r.GET("/client-ip", func(c *gin.Context) {
		c.String(http.StatusOK, c.ClientIP())
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/client-ip", nil)
	req.RemoteAddr = "203.0.113.10:12345"
	req.Header.Set("X-Forwarded-For", "127.0.0.1")
	req.Header.Set("X-Real-IP", "127.0.0.1")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}
	if got := strings.TrimSpace(w.Body.String()); got != "203.0.113.10" {
		t.Fatalf("ClientIP() = %q, want remote addr and not spoofed loopback", got)
	}
}

func TestLoggerMiddlewareRedactsSensitiveContext(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var logs bytes.Buffer
	previousOutput := log.Writer()
	previousFlags := log.Flags()
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(previousOutput)
		log.SetFlags(previousFlags)
	})

	r := gin.New()
	r.Use(loggerMiddleware())
	r.GET("/probe", func(c *gin.Context) {
		c.Set("x-account-email", "alice@example.com")
		c.Set("x-account-proxy", "http://user:secret@proxy.example:8080")
		c.Set("x-model", "gpt-5.5")
		c.Set("x-reasoning-effort", "medium")
		c.Set("x-service-tier", "fast")
		c.Status(http.StatusAccepted)
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/probe", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusAccepted)
	}

	got := logs.String()
	for _, forbidden := range []string{"alice@example.com", "secret"} {
		if strings.Contains(got, forbidden) {
			t.Fatalf("log output leaked %q: %s", forbidden, got)
		}
	}
	for _, expected := range []string{"GET /probe 202", "gpt-5.5", "effort=medium", "fast"} {
		if !strings.Contains(got, expected) {
			t.Fatalf("log output missing %q: %s", expected, got)
		}
	}
}
