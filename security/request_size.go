package security

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

func requestBodySizeLimit(path string, maxSize int64) int64 {
	if MaxImportBodySize > 0 && IsLargeImportUploadPath(path) {
		return MaxImportBodySize
	}
	return maxSize
}

// rejectRequestSize runs before access/usage logging and must not log bodies,
// authorization, query strings, or client-supplied URLs.
func rejectRequestSize(c *gin.Context, limit, observed int64, stage string, lowerBound bool) {
	var observedBytes, declaredLength *int64
	if observed >= 0 {
		observedBytes = &observed
	} else {
		lowerBound = false
	}
	if c.Request.ContentLength >= 0 {
		declaredLength = &c.Request.ContentLength
	}
	requestID := boundedSizeMetadata(c.Writer.Header().Get("X-Request-ID"))
	encoding := strings.Join(parseContentEncodings(c.GetHeader("Content-Encoding")), ",")
	if encoding == "" {
		encoding = "identity"
	} else {
		for _, token := range parseContentEncodings(encoding) {
			if !isSupportedContentEncoding(token) {
				encoding = "unsupported"
				break
			}
		}
	}
	entry := struct {
		RequestID  string `json:"request_id"`
		CFRay      string `json:"cf_ray,omitempty"`
		Route      string `json:"route"`
		Stage      string `json:"size_stage"`
		Encoding   string `json:"content_encoding"`
		Limit      int64  `json:"max_body_bytes"`
		Observed   *int64 `json:"observed_body_bytes"`
		Declared   *int64 `json:"declared_content_length,omitempty"`
		LowerBound bool   `json:"observed_is_lower_bound"`
	}{requestID, boundedSizeMetadata(c.GetHeader("CF-Ray")), c.FullPath(), stage, boundedSizeMetadata(encoding), limit, observedBytes, declaredLength, lowerBound}
	encoded, _ := json.Marshal(entry)
	log.Printf("[request-size] %s", encoded)
	c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, gin.H{"error": gin.H{
		"message": fmt.Sprintf("请求体过大（上限 %d 字节；请缩短会话或减少附件后重试）", limit),
		"type":    "invalid_request_error", "code": "request_too_large",
		"request_id": requestID, "max_body_bytes": limit,
		"observed_body_bytes": observedBytes, "size_stage": stage,
		"declared_content_length": declaredLength,
		"observed_is_lower_bound": lowerBound,
	}})
}

func boundedSizeMetadata(value string) string {
	const maxLength = 128
	if len(value) > maxLength {
		return value[:maxLength]
	}
	return value
}
