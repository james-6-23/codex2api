package security

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// contentSecurityPolicy is kept in a constant so a unit test can verify that
// every inline script in frontend/index.html is covered by its hash.
const contentSecurityPolicy = "default-src 'self'; script-src 'self' 'sha256-8jI3h1oN0D7cJ3eKd/Mjk+hKunZV0rZrmg9g4u/5+Nk='; style-src 'self' 'unsafe-inline'; img-src 'self' data: blob: https: http:; media-src 'self' data: blob: https: http:; font-src 'self'; connect-src 'self' https://api.github.com;"

// Security headers middleware
func SecurityHeadersMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Prevent XSS
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Header("X-XSS-Protection", "1; mode=block")
		c.Header("Referrer-Policy", "strict-origin-when-cross-origin")

		// Content Security Policy
		// script-src 内联哈希对应 frontend/index.html 中的主题恢复脚本（在 React 挂载前同步 localStorage 主题到 <html>，避免刷新闪烁）。
		// 如修改该脚本，请同步更新 contentSecurityPolicy（security/csp_test.go 会检查）。
		c.Header("Content-Security-Policy", contentSecurityPolicy)

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

// RequestSizeLimiter limits request body size. 管理端账号导入端点(multipart
// 文件上传)单独放宽到 MaxImportBodySize,其余端点用传入的 maxSize。
func RequestSizeLimiter(maxSize int64) gin.HandlerFunc {
	return requestSizeLimiterWithMemory(maxSize, processRequestMemory)
}

func requestSizeLimiterWithMemory(maxSize int64, budget *requestMemoryBudget) gin.HandlerFunc {
	return func(c *gin.Context) {
		limit := requestBodySizeLimit(c.Request.URL.Path, maxSize)
		largeImport := MaxImportBodySize > 0 && IsLargeImportUploadPath(c.Request.URL.Path)
		c.Header("X-Codex2API-Max-Request-Body-Bytes", strconv.FormatInt(limit, 10))
		// 导入端点(multipart 文件上传)走流式限制:用 MaxBytesReader 包裹 body 交给
		// handler,不整体 ReadAll、不缓存 raw_body。避免 200MB 文件在中间件里常驻
		// 一整份内存(handler 用 ParseMultipartForm 流式解析,>32MB 落临时文件)。
		// 超限时 handler 的 body 读取会返回错误,由 handler 侧回 400。
		if largeImport {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, limit)
			c.Next()
			return
		}
		if c.Request.ContentLength > limit {
			rejectRequestSize(c, limit, -1, "content_length", false)
			return
		}
		// Charge the budget as bytes actually arrive. Reserving the declared
		// Content-Length up front lets a client declare 47MB, send nothing, and
		// pin the global budget; an empty body must reserve zero.
		prepaid := int64(0)
		reservation, admitted := budget.acquire(prepaid)
		if !admitted {
			rejectRequestMemory(c)
			return
		}
		defer reservation.Release()
		c.Set(requestMemoryContextKey, reservation)
		// Read the request body once, enforce the configured size limit, and cache it
		// for downstream handlers that need raw JSON without re-reading c.Request.Body.
		body, err := readRequestMemoryBounded(c.Request.Body, limit, reservation, prepaid)
		if err != nil {
			if errors.Is(err, ErrRequestMemoryBudget) {
				rejectRequestMemory(c)
				return
			}
			c.JSON(http.StatusBadRequest, gin.H{
				"error": gin.H{
					"message": "读取请求体失败",
					"type":    "invalid_request_error",
				},
			})
			c.Abort()
			return
		}
		defer c.Request.Body.Close()

		// Check if body exceeds max size
		if int64(len(body)) > limit {
			rejectRequestSize(c, limit, int64(len(body)), "wire_body", true)
			return
		}

		c.Set("raw_body", body)
		// Replace the body so code paths using Gin binding/GetRawData can still read it.
		c.Request.Body = io.NopCloser(bytes.NewReader(body))
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
	// Hash both values first so the comparison takes the same path regardless
	// of length; a direct byte loop leaks the expected secret's length.
	aHash := sha256.Sum256([]byte(a))
	bHash := sha256.Sum256([]byte(b))
	return subtle.ConstantTimeCompare(aHash[:], bHash[:]) == 1
}

// SanitizeLog sanitizes data for logging
func SanitizeLog(data string) string {
	if data == "" {
		return data
	}
	// Keep one log event per line: URL paths and query values can contain
	// escaped newlines that would otherwise forge additional audit lines.
	data = strings.NewReplacer("\r", " ", "\n", " ").Replace(data)
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
	mu       sync.Mutex
	ticker   *time.Ticker
	stopChan chan struct{}
}

type visitor struct {
	tokens   int
	lastSeen time.Time
}

// NewIPRateLimiter creates a new rate limiter
func NewIPRateLimiter(requests int, window time.Duration) *IPRateLimiter {
	l := &IPRateLimiter{
		visitors: make(map[string]*visitor),
		requests: requests,
		window:   window,
		stopChan: make(chan struct{}),
	}
	go l.cleanup()
	return l
}

// Stop stops the cleanup goroutine
func (l *IPRateLimiter) Stop() {
	l.mu.Lock()
	if l.ticker != nil {
		l.ticker.Stop()
	}
	l.mu.Unlock()
	close(l.stopChan)
}

// Allow checks if a request from this IP is allowed
func (l *IPRateLimiter) Allow(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

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
	l.mu.Lock()
	l.ticker = time.NewTicker(l.window * 2)
	l.mu.Unlock()
	defer l.ticker.Stop()

	for {
		select {
		case <-l.ticker.C:
			l.mu.Lock()
			now := time.Now()
			for ip, v := range l.visitors {
				if now.Sub(v.lastSeen) > l.window*2 {
					delete(l.visitors, ip)
				}
			}
			l.mu.Unlock()
		case <-l.stopChan:
			return
		}
	}
}

// IsSensitiveEndpoint checks if an endpoint contains sensitive data
func IsSensitiveEndpoint(path string) bool {
	sensitivePaths := []string{
		"/api/admin/accounts",
		"/api/admin/keys",
		"/api/admin/settings",
		"/api/admin/proxies",
		"/api/admin/prompt-filter/review/keys",
	}
	for _, p := range sensitivePaths {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}
