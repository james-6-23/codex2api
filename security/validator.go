// Package security provides security-related utilities for input validation,
// sanitization, and sensitive data masking.
package security

import (
	"regexp"
	"strings"
	"unicode/utf8"
)

// Validation constants
const (
	MaxModelLength        = 100
	MaxEmailLength        = 255
	MaxProxyURLLength     = 500
	MaxTokenLength        = 8192
	MaxRequestBodySize    = 10 * 1024 * 1024 // 10MB
	MaxHeaderSize         = 16 * 1024        // 16KB
	AllowedModelPattern   = `^[a-zA-Z0-9._-]+$`
	AllowedEndpointPattern = `^[a-zA-Z0-9/_-]+$`
)

// Dangerous patterns for XSS prevention
var (
	xssPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)<script[^>]*>[\s\S]*?</script>`),
		regexp.MustCompile(`(?i)javascript:\s*`),
		regexp.MustCompile(`(?i)on\w+\s*=\s*["'][^"']*["']`),
		regexp.MustCompile(`(?i)<iframe[^>]*>[\s\S]*?</iframe>`),
		regexp.MustCompile(`(?i)<object[^>]*>[\s\S]*?</object>`),
		regexp.MustCompile(`(?i)<embed[^>]*>`),
		regexp.MustCompile(`(?i)data:\s*text/html`),
	}

	// SQL injection patterns
	sqlInjectionPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)(SELECT|INSERT|UPDATE|DELETE|DROP|TRUNCATE|ALTER|CREATE|UNION)\s+`),
		regexp.MustCompile(`(?i)(--|;|/\*|\*/|\|)`),
		regexp.MustCompile(`(?i)(OR|AND)\s+['"\d]\s*=\s*['"\d]`),
	}

	// Model name validation
	modelNameRegex = regexp.MustCompile(AllowedModelPattern)

	// Endpoint validation
	endpointRegex = regexp.MustCompile(AllowedEndpointPattern)

	// Token patterns to mask
	sensitivePatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)(refresh[_-]?token["']?\s*[:=]\s*["']?)([^"'\s&]+)`),
		regexp.MustCompile(`(?i)(access[_-]?token["']?\s*[:=]\s*["']?)([^"'\s&]+)`),
		regexp.MustCompile(`(?i)(id[_-]?token["']?\s*[:=]\s*["']?)([^"'\s&]+)`),
		regexp.MustCompile(`(?i)(bearer\s+)([^\s&]+)`),
		regexp.MustCompile(`(?i)(["']?api[_-]?key["']?\s*[:=]\s*["']?)([^"'\s&]+)`),
		regexp.MustCompile(`(?i)(["']?secret["']?\s*[:=]\s*["']?)([^"'\s&]+)`),
		regexp.MustCompile(`(?i)(["']?password["']?\s*[:=]\s*["']?)([^"'\s&]+)`),
		regexp.MustCompile(`(sk-[a-zA-Z0-9]{24,})`),
	}
)

// ValidateModelName validates a model name parameter
func ValidateModelName(model string) error {
	if model == "" {
		return nil // Empty is allowed (will use default)
	}
	if utf8.RuneCountInString(model) > MaxModelLength {
		return &ValidationError{Field: "model", Message: "model name too long"}
	}
	if !modelNameRegex.MatchString(model) {
		return &ValidationError{Field: "model", Message: "invalid model name format"}
	}
	return nil
}

// ValidateEndpoint validates an endpoint string
func ValidateEndpoint(endpoint string) error {
	if endpoint == "" {
		return nil
	}
	if utf8.RuneCountInString(endpoint) > 200 {
		return &ValidationError{Field: "endpoint", Message: "endpoint too long"}
	}
	if !endpointRegex.MatchString(endpoint) {
		return &ValidationError{Field: "endpoint", Message: "invalid endpoint format"}
	}
	return nil
}

// ValidateEmail validates an email address (basic validation)
func ValidateEmail(email string) error {
	if email == "" {
		return nil
	}
	if utf8.RuneCountInString(email) > MaxEmailLength {
		return &ValidationError{Field: "email", Message: "email too long"}
	}
	if strings.Contains(email, "@") {
		parts := strings.Split(email, "@")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return &ValidationError{Field: "email", Message: "invalid email format"}
		}
	}
	return nil
}

// ValidateProxyURL validates a proxy URL
func ValidateProxyURL(url string) error {
	if url == "" {
		return nil
	}
	if utf8.RuneCountInString(url) > MaxProxyURLLength {
		return &ValidationError{Field: "proxy_url", Message: "proxy URL too long"}
	}
	return nil
}

// SanitizeInput removes potentially dangerous content from input
func SanitizeInput(input string) string {
	if input == "" {
		return input
	}

	// Remove null bytes
	input = strings.ReplaceAll(input, "\x00", "")

	// Remove control characters except common whitespace
	var result strings.Builder
	for _, r := range input {
		if r == '\n' || r == '\r' || r == '\t' || (r >= 32 && r < 127) || r > 127 {
			result.WriteRune(r)
		}
	}

	return result.String()
}

// ContainsXSS checks if input contains potential XSS patterns
func ContainsXSS(input string) bool {
	for _, pattern := range xssPatterns {
		if pattern.MatchString(input) {
			return true
		}
	}
	return false
}

// ContainsSQLInjection checks if input contains potential SQL injection patterns
func ContainsSQLInjection(input string) bool {
	for _, pattern := range sqlInjectionPatterns {
		if pattern.MatchString(input) {
			return true
		}
	}
	return false
}

// MaskSensitiveData masks sensitive data in a string
func MaskSensitiveData(input string) string {
	if input == "" {
		return input
	}

	result := input
	for _, pattern := range sensitivePatterns {
		result = pattern.ReplaceAllString(result, "${1}****MASKED****")
	}

	// Mask UUID-like patterns that might be tokens
	uuidPattern := regexp.MustCompile(`\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	result = uuidPattern.ReplaceAllString(result, "****UUID-MASKED****")

	return result
}

// MaskAPIKey masks an API key for display (show only first 4 and last 4 chars)
func MaskAPIKey(key string) string {
	if len(key) <= 8 {
		return "****"
	}
	if len(key) > 64 {
		return key[:4] + "****...****" + key[len(key)-4:]
	}
	return key[:4] + "****...****" + key[len(key)-4:]
}

// MaskToken masks a token completely for logs
func MaskToken(token string) string {
	if token == "" {
		return ""
	}
	return "[TOKEN-MASKED]"
}

// MaskEmail masks an email address (show only first 2 chars and domain)
func MaskEmail(email string) string {
	if email == "" {
		return ""
	}
	at := strings.Index(email, "@")
	if at == -1 || at < 2 {
		return "****"
	}
	return email[:2] + "****@" + email[at+1:]
}

// ValidationError represents a validation error
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return e.Field + ": " + e.Message
}

// IsValidationError checks if an error is a validation error
func IsValidationError(err error) bool {
	_, ok := err.(*ValidationError)
	return ok
}

// SafeTruncate safely truncates a string to max length
func SafeTruncate(s string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	if utf8.RuneCountInString(s) <= maxLen {
		return s
	}
	// Truncate to maxLen runes
	runes := []rune(s)
	if len(runes) > maxLen {
		return string(runes[:maxLen])
	}
	return s
}
