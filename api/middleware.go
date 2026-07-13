// Package api provides middleware for API versioning and request processing
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// Version represents an API version
type Version struct {
	Major int
	Minor int
	Patch int
}

// String returns the version string representation
func (v Version) String() string {
	return fmt.Sprintf("v%d", v.Major)
}

// FullVersion returns the full version string with minor and patch
func (v Version) FullVersion() string {
	return fmt.Sprintf("v%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// Compare compares two versions
// Returns -1 if v < other, 0 if v == other, 1 if v > other
func (v Version) Compare(other Version) int {
	if v.Major != other.Major {
		if v.Major < other.Major {
			return -1
		}
		return 1
	}
	if v.Minor != other.Minor {
		if v.Minor < other.Minor {
			return -1
		}
		return 1
	}
	if v.Patch != other.Patch {
		if v.Patch < other.Patch {
			return -1
		}
		return 1
	}
	return 0
}

// CurrentVersion is the current API version
var CurrentVersion = Version{Major: 1, Minor: 0, Patch: 0}

// SupportedVersions lists all supported API versions
var SupportedVersions = []Version{
	{Major: 1, Minor: 0, Patch: 0},
}

// VersionMiddleware adds version information to the context
func VersionMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Extract version from URL path
		path := c.Request.URL.Path
		version := extractVersionFromPath(path)

		c.Set("api_version", version)
		c.Set("api_version_string", version.String())

		// Add version header to response
		c.Header("X-API-Version", version.String())
		c.Header("X-API-Supported-Versions", getSupportedVersionsHeader())

		c.Next()
	}
}

// extractVersionFromPath extracts API version from URL path
func extractVersionFromPath(path string) Version {
	parts := strings.Split(path, "/")
	for _, part := range parts {
		if strings.HasPrefix(part, "v") {
			// Parse v1, v2, etc.
			versionStr := strings.TrimPrefix(part, "v")
			major, err := strconv.Atoi(versionStr)
			if err == nil && major > 0 {
				return Version{Major: major}
			}
		}
	}
	// Default to current version
	return CurrentVersion
}

// getSupportedVersionsHeader returns comma-separated list of supported versions
func getSupportedVersionsHeader() string {
	var versions []string
	for _, v := range SupportedVersions {
		versions = append(versions, v.String())
	}
	return strings.Join(versions, ", ")
}

// IsVersionSupported checks if a version is supported
func IsVersionSupported(version Version) bool {
	for _, v := range SupportedVersions {
		if v.Major == version.Major {
			return true
		}
	}
	return false
}

// RequestContext holds request-scoped information
type RequestContext struct {
	RequestID string
	Version   Version
	StartTime time.Time
	APIKey    string
	Model     string
	Stream    bool
}

// GetRequestContext retrieves the request context from gin context
func GetRequestContext(c *gin.Context) *RequestContext {
	if ctx, exists := c.Get("request_context"); exists {
		if rc, ok := ctx.(*RequestContext); ok {
			return rc
		}
	}
	return nil
}

// RequestContextMiddleware initializes request context
func RequestContextMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		requestID := c.GetHeader("X-Request-ID")
		if requestID == "" {
			requestID = generateRequestID()
		}

		version := extractVersionFromPath(c.Request.URL.Path)

		ctx := &RequestContext{
			RequestID: requestID,
			Version:   version,
			StartTime: time.Now(),
		}

		// Extract API key from Authorization header
		authHeader := c.GetHeader("Authorization")
		if authHeader != "" {
			ctx.APIKey = strings.TrimPrefix(authHeader, "Bearer ")
		}

		c.Set("request_context", ctx)
		c.Header("X-Request-ID", requestID)

		c.Next()
	}
}

// generateRequestID generates a unique request ID
func generateRequestID() string {
	return fmt.Sprintf("req_%d_%d", time.Now().UnixNano(), randInt())
}

// randInt generates a random int for request ID
func randInt() int {
	return int(time.Now().UnixNano() % 1000000)
}

// BodyCacheMiddleware caches the request body for multiple reads
func BodyCacheMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if body := GetRawBody(c); body != nil {
			c.Request.Body = io.NopCloser(bytes.NewReader(body))
			c.Next()
			return
		}

		if c.Request.Body != nil && c.Request.Body != http.NoBody {
			body, err := io.ReadAll(c.Request.Body)
			if err != nil {
				log.Printf("Failed to read request body: %v", err)
				c.Next()
				return
			}
			c.Set("raw_body", body)
			// Restore body for later use using bytes.Reader to avoid unnecessary conversion
			c.Request.Body = io.NopCloser(bytes.NewReader(body))
		}
		c.Next()
	}
}

// RecoveryMiddleware provides enhanced panic recovery with standardized error response
func RecoveryMiddleware() gin.HandlerFunc {
	return gin.CustomRecovery(func(c *gin.Context, recovered interface{}) {
		log.Printf("Panic recovered: %v", recovered)

		// Return a generic message to the client to avoid leaking internal details.
		message := "Internal server error"

		SendError(c, NewAPIError(ErrCodeServerError, message, ErrorTypeServer))
	})
}

// LoggingMiddleware provides structured API request logging
func LoggingMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		path := c.Request.URL.Path
		raw := c.Request.URL.RawQuery

		if raw != "" {
			path = path + "?" + raw
		}

		c.Next()

		latency := time.Since(start)
		status := c.Writer.Status()

		// Get request context
		reqCtx := GetRequestContext(c)
		requestID := ""
		if reqCtx != nil {
			requestID = reqCtx.RequestID
		}

		// Build log entry
		logEntry := map[string]interface{}{
			"timestamp":   start.Format(time.RFC3339),
			"method":      c.Request.Method,
			"path":        path,
			"status":      status,
			"latency_ms":  float64(latency.Nanoseconds()) / 1e6,
			"client_ip":   c.ClientIP(),
			"request_id":  requestID,
			"api_version": extractVersionFromPath(c.Request.URL.Path).String(),
		}

		// Add error if present
		if len(c.Errors) > 0 {
			logEntry["error"] = c.Errors.String()
		}

		// Log based on status
		logJSON, _ := json.Marshal(logEntry)
		if status >= 500 {
			log.Printf("[ERROR] %s", logJSON)
		} else if status >= 400 {
			log.Printf("[WARN] %s", logJSON)
		} else {
			log.Printf("[INFO] %s", logJSON)
		}
	}
}

// CORSMiddleware provides CORS headers
func CORSMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS, PATCH")
		c.Header("Access-Control-Allow-Headers", "Origin, Content-Type, Accept, Authorization, X-Request-ID, X-API-Version, X-Codex2API-Affinity-Key")
		c.Header("Access-Control-Expose-Headers", "X-Request-ID, X-API-Version, X-API-Supported-Versions")
		c.Header("Access-Control-Max-Age", "86400")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}

		c.Next()
	}
}

// RateLimitHeadersMiddleware adds rate limit headers to responses
func RateLimitHeadersMiddleware(rl RateLimiterInfo) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()

		// Add rate limit headers after handler execution
		if rl != nil {
			limit, remaining, reset := rl.GetRateLimitInfo(c)
			if limit > 0 {
				c.Header("X-RateLimit-Limit", strconv.Itoa(limit))
				c.Header("X-RateLimit-Remaining", strconv.Itoa(remaining))
				c.Header("X-RateLimit-Reset", strconv.Itoa(reset))
			}
		}
	}
}

// RateLimiterInfo interface for getting rate limit information
type RateLimiterInfo interface {
	GetRateLimitInfo(c *gin.Context) (limit int, remaining int, reset int)
}

// SecurityHeadersMiddleware adds security headers
func SecurityHeadersMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "DENY")
		c.Header("X-XSS-Protection", "1; mode=block")
		c.Header("Referrer-Policy", "strict-origin-when-cross-origin")
		c.Header("Content-Security-Policy", "default-src 'none'")

		c.Next()
	}
}

// ContentTypeMiddleware ensures proper content-type handling
func ContentTypeMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		contentType := c.ContentType()

		// For POST/PUT/PATCH, validate content-type
		if c.Request.Method == "POST" || c.Request.Method == "PUT" || c.Request.Method == "PATCH" {
			if c.Request.ContentLength > 0 && contentType == "" {
				SendError(c, NewAPIError(ErrCodeInvalidRequest, "Content-Type header is required", ErrorTypeInvalidRequest))
				c.Abort()
				return
			}
			if contentType != "" && !strings.Contains(contentType, "application/json") {
				SendError(c, NewAPIError(ErrCodeInvalidRequest, "Content-Type must be application/json", ErrorTypeInvalidRequest))
				c.Abort()
				return
			}
		}

		c.Next()
	}
}

// TimeoutMiddleware adds a timeout to requests by setting a context deadline.
// Downstream handlers should respect the context deadline via c.Request.Context().Done().
// For a more robust solution, use http.TimeoutHandler at the server level.
func TimeoutMiddleware(timeout time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

// GetRawBody retrieves the cached request body
func GetRawBody(c *gin.Context) []byte {
	if body, exists := c.Get("raw_body"); exists {
		if b, ok := body.([]byte); ok {
			return b
		}
	}
	return nil
}
