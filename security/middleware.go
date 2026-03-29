package security

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// Security headers middleware
func SecurityHeadersMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Prevent XSS
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Header("X-XSS-Protection", "1; mode=block")
		c.Header("Referrer-Policy", "strict-origin-when-cross-origin")

		// Content Security Policy
		c.Header("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; connect-src 'self';")

		// Strict Transport Security (HTTPS only)
		// c.Header("Strict-Transport-Security", "max-age=31536000; includeSubDomains; preload")

		c.Next()
	}
}

// RateLimitMiddleware creates a rate limiter middleware
func RateLimitMiddleware(requests int, window time.Duration) gin.HandlerFunc {
	limiter := NewIPRateLimiter(requests, window)
	return func(c *gin.Context) {
		ip := c.ClientIP()
		if !limiter.Allow(ip) {
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error": gin.H{
					"message": "请求过于频繁，请稍后重试",
					"type":    "rate_limit_error",
					"code":    "too_many_requests",
				},
			})
			c.Abort()
			return
		}
		c.Next()
	}
}

// RequestSizeLimiter limits request body size
func RequestSizeLimiter(maxSize int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxSize)
		c.Next()
	}
}

// GenerateSecureToken generates a cryptographically secure random token
func GenerateSecureToken(length int) (string, error) {
	if length <= 0 {
		length = 32
	}
	bytes := make([]byte, length)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

// SecureCompare compares two strings in constant time to prevent timing attacks
func SecureCompare(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var result byte
	for i := 0; i < len(a); i++ {
		result |= a[i] ^ b[i]
	}
	return result == 0
}

// SanitizeLog sanitizes data for logging
func SanitizeLog(data string) string {
	if data == "" {
		return data
	}
	// Mask sensitive patterns
	data = MaskSensitiveData(data)
	// Truncate very long strings
	return SafeTruncate(data, 1000)
}

// ValidateContentType validates Content-Type header
func ValidateContentType(allowedTypes ...string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Method == "GET" || c.Request.Method == "HEAD" || c.Request.Method == "OPTIONS" {
			c.Next()
			return
		}

		contentType := c.ContentType()
		if contentType == "" {
			// Allow empty Content-Type for some requests
			c.Next()
			return
		}

		for _, allowed := range allowedTypes {
			if strings.HasPrefix(contentType, allowed) {
				c.Next()
				return
			}
		}

		c.JSON(http.StatusUnsupportedMediaType, gin.H{
			"error": gin.H{
				"message": fmt.Sprintf("Content-Type must be one of: %v", allowedTypes),
				"type":    "invalid_request_error",
				"code":    "unsupported_media_type",
			},
		})
		c.Abort()
	}
}

// IPRateLimiter implements a simple rate limiter per IP
type IPRateLimiter struct {
	visitors map[string]*visitor
	requests int
	window   time.Duration
}

type visitor struct {
	tokens    int
	lastSeen  time.Time
}

// NewIPRateLimiter creates a new rate limiter
func NewIPRateLimiter(requests int, window time.Duration) *IPRateLimiter {
	l := &IPRateLimiter{
		visitors: make(map[string]*visitor),
		requests: requests,
		window:   window,
	}
	go l.cleanup()
	return l
}

// Allow checks if a request from this IP is allowed
func (l *IPRateLimiter) Allow(ip string) bool {
	v, exists := l.visitors[ip]
	now := time.Now()

	if !exists {
		l.visitors[ip] = &visitor{tokens: l.requests - 1, lastSeen: now}
		return true
	}

	// Reset tokens if window has passed
	if now.Sub(v.lastSeen) > l.window {
		v.tokens = l.requests - 1
		v.lastSeen = now
		return true
	}

	v.lastSeen = now
	if v.tokens > 0 {
		v.tokens--
		return true
	}

	return false
}

// cleanup removes old entries periodically
func (l *IPRateLimiter) cleanup() {
	ticker := time.NewTicker(l.window * 2)
	for range ticker.C {
		now := time.Now()
		for ip, v := range l.visitors {
			if now.Sub(v.lastSeen) > l.window*2 {
				delete(l.visitors, ip)
			}
		}
	}
}

// SecurityAuditLog writes security-related events to log
func SecurityAuditLog(action, details string) {
	// Sanitize details before logging
	safeDetails := SanitizeLog(details)
	// Use standard log format with security prefix
	fmt.Printf("[SECURITY] %s | %s | %s\n", time.Now().Format(time.RFC3339), action, safeDetails)
}

// IsSensitiveEndpoint checks if an endpoint contains sensitive data
func IsSensitiveEndpoint(path string) bool {
	sensitivePaths := []string{
		"/api/admin/accounts",
		"/api/admin/keys",
		"/api/admin/settings",
		"/api/admin/proxies",
	}
	for _, p := range sensitivePaths {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}
