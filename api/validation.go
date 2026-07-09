// Package api provides request validation utilities
package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// responsesMaxOutputTokensCap is the upper bound enforced by the local
// validator on the /v1/responses max_output_tokens field. The translator
// strips the field before forwarding to the Codex upstream (which does not
// accept it), so this cap only guards against obviously-absurd client values
// — it does not control the actual output length, which is decided upstream.
// Aligned to 128000 to match the highest cap OpenAI currently advertises on
// any Codex-served model, so SDKs that default to 100k+ pass through.
const responsesMaxOutputTokensCap = 128000

// ValidationRule represents a validation rule function
type ValidationRule func(value gjson.Result, path string) *ValidationError

// ValidationError represents a validation error
type ValidationError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
	Code    string `json:"code"`
}

// ValidationResult contains all validation errors
type ValidationResult struct {
	Valid  bool              `json:"valid"`
	Errors []ValidationError `json:"errors"`
}

// Validator provides request validation capabilities
type Validator struct {
	Body   []byte
	Errors []ValidationError
}

// NewValidator creates a new validator for the request body
func NewValidator(body []byte) *Validator {
	return &Validator{
		Body:   body,
		Errors: make([]ValidationError, 0),
	}
}

// ValidateRequest validates the request body against validation rules
func (v *Validator) ValidateRequest(rules map[string][]ValidationRule) *ValidationResult {
	for path, ruleList := range rules {
		value := gjson.GetBytes(v.Body, path)
		for _, rule := range ruleList {
			if err := rule(value, path); err != nil {
				v.Errors = append(v.Errors, *err)
			}
		}
	}

	return &ValidationResult{
		Valid:  len(v.Errors) == 0,
		Errors: v.Errors,
	}
}

// HasErrors returns true if there are validation errors
func (v *Validator) HasErrors() bool {
	return len(v.Errors) > 0
}

// ToAPIError converts validation errors to APIError
func (v *Validator) ToAPIError() *APIError {
	if len(v.Errors) == 0 {
		return nil
	}

	if len(v.Errors) == 1 {
		return NewAPIErrorWithDetails(
			ErrCodeInvalidParameter,
			v.Errors[0].Message,
			ErrorTypeInvalidRequest,
			v.Errors[0],
		)
	}

	var details []ValidationError
	for _, err := range v.Errors {
		details = append(details, err)
	}

	return NewAPIErrorWithDetails(
		ErrCodeInvalidRequest,
		"Multiple validation errors occurred",
		ErrorTypeInvalidRequest,
		details,
	)
}

// ============ Validation Rules ============

// Required validates that a field exists and is not empty
func Required() ValidationRule {
	return func(value gjson.Result, path string) *ValidationError {
		if !value.Exists() {
			return &ValidationError{
				Field:   path,
				Message: fmt.Sprintf("Field '%s' is required", path),
				Code:    "required",
			}
		}
		if value.Type == gjson.Null {
			return &ValidationError{
				Field:   path,
				Message: fmt.Sprintf("Field '%s' cannot be null", path),
				Code:    "null_not_allowed",
			}
		}
		if value.Type == gjson.String && strings.TrimSpace(value.String()) == "" {
			return &ValidationError{
				Field:   path,
				Message: fmt.Sprintf("Field '%s' cannot be empty", path),
				Code:    "empty_not_allowed",
			}
		}
		return nil
	}
}

// TypeString validates that a field is a string
func TypeString() ValidationRule {
	return func(value gjson.Result, path string) *ValidationError {
		if !value.Exists() {
			return nil
		}
		if value.Type != gjson.String {
			return &ValidationError{
				Field:   path,
				Message: fmt.Sprintf("Field '%s' must be a string", path),
				Code:    "type_error",
			}
		}
		return nil
	}
}

// TypeNumber validates that a field is a number
func TypeNumber() ValidationRule {
	return func(value gjson.Result, path string) *ValidationError {
		if !value.Exists() {
			return nil
		}
		if value.Type != gjson.Number {
			return &ValidationError{
				Field:   path,
				Message: fmt.Sprintf("Field '%s' must be a number", path),
				Code:    "type_error",
			}
		}
		return nil
	}
}

// TypeBoolean validates that a field is a boolean
func TypeBoolean() ValidationRule {
	return func(value gjson.Result, path string) *ValidationError {
		if !value.Exists() {
			return nil
		}
		if value.Type != gjson.True && value.Type != gjson.False {
			return &ValidationError{
				Field:   path,
				Message: fmt.Sprintf("Field '%s' must be a boolean", path),
				Code:    "type_error",
			}
		}
		return nil
	}
}

// TypeArray validates that a field is an array
func TypeArray() ValidationRule {
	return func(value gjson.Result, path string) *ValidationError {
		if !value.Exists() {
			return nil
		}
		if value.Type != gjson.JSON || !value.IsArray() {
			return &ValidationError{
				Field:   path,
				Message: fmt.Sprintf("Field '%s' must be an array", path),
				Code:    "type_error",
			}
		}
		return nil
	}
}

// TypeObject validates that a field is an object
func TypeObject() ValidationRule {
	return func(value gjson.Result, path string) *ValidationError {
		if !value.Exists() {
			return nil
		}
		if value.Type != gjson.JSON || value.IsArray() {
			return &ValidationError{
				Field:   path,
				Message: fmt.Sprintf("Field '%s' must be an object", path),
				Code:    "type_error",
			}
		}
		return nil
	}
}

// TypeStringOrObject validates that a field is either a string or an object.
func TypeStringOrObject() ValidationRule {
	return func(value gjson.Result, path string) *ValidationError {
		if !value.Exists() {
			return nil
		}
		if value.Type == gjson.String {
			return nil
		}
		if value.Type == gjson.JSON && !value.IsArray() {
			return nil
		}
		return &ValidationError{
			Field:   path,
			Message: fmt.Sprintf("Field '%s' must be a string or object", path),
			Code:    "type_error",
		}
	}
}

// MinLength validates minimum string length
func MinLength(min int) ValidationRule {
	return func(value gjson.Result, path string) *ValidationError {
		if !value.Exists() || value.Type != gjson.String {
			return nil
		}
		if len(value.String()) < min {
			return &ValidationError{
				Field:   path,
				Message: fmt.Sprintf("Field '%s' must be at least %d characters", path, min),
				Code:    "min_length",
			}
		}
		return nil
	}
}

// MaxLength validates maximum string length
func MaxLength(max int) ValidationRule {
	return func(value gjson.Result, path string) *ValidationError {
		if !value.Exists() || value.Type != gjson.String {
			return nil
		}
		if len(value.String()) > max {
			return &ValidationError{
				Field:   path,
				Message: fmt.Sprintf("Field '%s' must be at most %d characters", path, max),
				Code:    "max_length",
			}
		}
		return nil
	}
}

// MinValue validates minimum numeric value
func MinValue(min float64) ValidationRule {
	return func(value gjson.Result, path string) *ValidationError {
		if !value.Exists() || value.Type != gjson.Number {
			return nil
		}
		if value.Float() < min {
			return &ValidationError{
				Field:   path,
				Message: fmt.Sprintf("Field '%s' must be at least %v", path, min),
				Code:    "min_value",
			}
		}
		return nil
	}
}

// MaxValue validates maximum numeric value
func MaxValue(max float64) ValidationRule {
	return func(value gjson.Result, path string) *ValidationError {
		if !value.Exists() || value.Type != gjson.Number {
			return nil
		}
		if value.Float() > max {
			return &ValidationError{
				Field:   path,
				Message: fmt.Sprintf("Field '%s' must be at most %v", path, max),
				Code:    "max_value",
			}
		}
		return nil
	}
}

// Range validates numeric range
func Range(min, max float64) ValidationRule {
	return func(value gjson.Result, path string) *ValidationError {
		if !value.Exists() || value.Type != gjson.Number {
			return nil
		}
		v := value.Float()
		if v < min || v > max {
			return &ValidationError{
				Field:   path,
				Message: fmt.Sprintf("Field '%s' must be between %v and %v", path, min, max),
				Code:    "out_of_range",
			}
		}
		return nil
	}
}

// Enum validates that a value is in the allowed set
func Enum(values ...string) ValidationRule {
	allowed := make(map[string]bool)
	for _, v := range values {
		allowed[v] = true
	}
	return func(value gjson.Result, path string) *ValidationError {
		if !value.Exists() {
			return nil
		}
		str := value.String()
		if !allowed[str] {
			return &ValidationError{
				Field:   path,
				Message: fmt.Sprintf("Field '%s' must be one of: %s", path, strings.Join(values, ", ")),
				Code:    "invalid_enum_value",
			}
		}
		return nil
	}
}

// Pattern validates string against regex pattern
func Pattern(pattern string, description string) ValidationRule {
	re, err := regexp.Compile(pattern)
	if err != nil {
		panic(fmt.Sprintf("Invalid regex pattern: %s", pattern))
	}
	return func(value gjson.Result, path string) *ValidationError {
		if !value.Exists() || value.Type != gjson.String {
			return nil
		}
		if !re.MatchString(value.String()) {
			return &ValidationError{
				Field:   path,
				Message: fmt.Sprintf("Field '%s' %s", path, description),
				Code:    "pattern_mismatch",
			}
		}
		return nil
	}
}

// MinItems validates minimum array length
func MinItems(min int) ValidationRule {
	return func(value gjson.Result, path string) *ValidationError {
		if !value.Exists() || !value.IsArray() {
			return nil
		}
		if int(value.Get("#").Int()) < min {
			return &ValidationError{
				Field:   path,
				Message: fmt.Sprintf("Field '%s' must have at least %d items", path, min),
				Code:    "min_items",
			}
		}
		return nil
	}
}

// MaxItems validates maximum array length
func MaxItems(max int) ValidationRule {
	return func(value gjson.Result, path string) *ValidationError {
		if !value.Exists() || !value.IsArray() {
			return nil
		}
		if int(value.Get("#").Int()) > max {
			return &ValidationError{
				Field:   path,
				Message: fmt.Sprintf("Field '%s' must have at most %d items", path, max),
				Code:    "max_items",
			}
		}
		return nil
	}
}

// ============ Chat Completions Validation Rules ============

// ChatCompletionValidationRules returns validation rules for chat completions request
// Note: Fields like 'stop' and 'tool_choice' are not validated strictly here
// because they are ignored/deleted during translation to upstream format.
// Validation is kept permissive to maintain backward compatibility.
func ChatCompletionValidationRules() map[string][]ValidationRule {
	return map[string][]ValidationRule{
		"model":       {Required(), TypeString(), MaxLength(64)},
		"messages":    {Required(), TypeArray(), MinItems(1), MaxItems(4096), ValidateMessages()},
		"max_tokens":  {TypeNumber(), MinValue(1), MaxValue(65536)},
		"temperature": {TypeNumber(), Range(0, 2)},
		"top_p":       {TypeNumber(), Range(0, 1)},
		"n":           {TypeNumber(), MinValue(1), MaxValue(1)},
		"stream":      {TypeBoolean()},
		// stop and tool_choice are intentionally not strictly validated
		// as they are ignored during request translation
		"presence_penalty":  {TypeNumber(), Range(-2, 2)},
		"frequency_penalty": {TypeNumber(), Range(-2, 2)},
		"user":              {TypeString(), MaxLength(256)},
		"reasoning_effort":  {TypeString(), MaxLength(64)},
		"service_tier":      {TypeString(), MaxLength(64)},
		"tools":             {TypeArray()},
		// tool_choice removed from strict validation to maintain backward compatibility
	}
}

// ResponsesMaxOutputTokensForModel returns the local validation cap for
// max_output_tokens. The cap is intentionally model-agnostic: the translator
// drops the field before forwarding to Codex, so the real upstream ceiling is
// enforced server-side. The model argument is kept for API compatibility.
func ResponsesMaxOutputTokensForModel(_ string) int {
	return responsesMaxOutputTokensCap
}

// ResponsesAPIValidationRules returns validation rules for responses API request
// Note: input can be either a string or an array of items (validated separately)
func ResponsesAPIValidationRules() map[string][]ValidationRule {
	return ResponsesAPIValidationRulesForModel("")
}

func ResponsesAPIValidationRulesForModel(_ string) map[string][]ValidationRule {
	return map[string][]ValidationRule{
		"model": {Required(), TypeString(), MaxLength(64)},
		// input validation is handled separately to support both string and array formats
		"max_output_tokens": {TypeNumber(), MinValue(1), MaxValue(float64(responsesMaxOutputTokensCap))},
		"temperature":       {TypeNumber(), Range(0, 2)},
		"top_p":             {TypeNumber(), Range(0, 1)},
		"stream":            {TypeBoolean()},
		"stop":              {TypeString(), MaxLength(256)},
		"user":              {TypeString(), MaxLength(256)},
		"reasoning.effort":  {TypeString(), MaxLength(64)},
		"service_tier":      {TypeString(), MaxLength(64)},
		"store":             {TypeBoolean()},
		"truncation":        {TypeString(), Enum("auto", "disabled")},
		"tools":             {TypeArray()},
		"tool_choice":       {TypeStringOrObject(), MaxLength(64)},
		"input":             {ValidateInput()},
	}
}

// ValidateChatCompletionsRequest validates a chat completions request with model validation
func ValidateChatCompletionsRequest(body []byte, supportedModels []string) *ValidationResult {
	rules := ChatCompletionValidationRules()
	rules["model"] = append(rules["model"], ModelValidator(supportedModels))
	validator := NewValidator(body)
	return validator.ValidateRequest(rules)
}

// ValidateResponsesAPIRequest validates a responses API request with model validation
func ValidateResponsesAPIRequest(body []byte, supportedModels []string) *ValidationResult {
	rules := ResponsesAPIValidationRulesForModel(gjson.GetBytes(body, "model").String())
	rules["model"] = append(rules["model"], ModelValidator(supportedModels))
	validator := NewValidator(body)
	return validator.ValidateRequest(rules)
}

// ============ Gin Middleware ============

// ValidationMiddleware creates a middleware that validates request body
func ValidationMiddleware(rules map[string][]ValidationRule) gin.HandlerFunc {
	return func(c *gin.Context) {
		// Skip if not a POST/PUT/PATCH request
		if c.Request.Method != "POST" && c.Request.Method != "PUT" && c.Request.Method != "PATCH" {
			c.Next()
			return
		}

		// Read body
		body, err := c.GetRawData()
		if err != nil {
			SendError(c, NewAPIError(ErrCodeInvalidRequest, "Failed to read request body", ErrorTypeInvalidRequest))
			c.Abort()
			return
		}

		// Store body for later use and restore c.Request.Body
		c.Set("raw_body", body)
		c.Request.Body = io.NopCloser(bytes.NewReader(body))

		// Validate
		validator := NewValidator(body)
		result := validator.ValidateRequest(rules)

		if !result.Valid {
			apiErr := validator.ToAPIError()
			SendError(c, apiErr)
			c.Abort()
			return
		}

		c.Next()
	}
}

// ModelValidator validates model names
func ModelValidator(supportedModels []string) ValidationRule {
	validModels := make(map[string]bool)
	for _, m := range supportedModels {
		validModels[m] = true
	}
	return func(value gjson.Result, path string) *ValidationError {
		if !value.Exists() || value.Type != gjson.String {
			return nil
		}
		model := value.String()
		if !validModels[model] {
			return &ValidationError{
				Field:   path,
				Message: fmt.Sprintf("Model '%s' is not supported", model),
				Code:    "unsupported_model",
			}
		}
		return nil
	}
}

// ValidateModel validates model name against supported models
func ValidateModel(body []byte, supportedModels []string, path string) *ValidationError {
	value := gjson.GetBytes(body, path)
	if !value.Exists() || value.Type != gjson.String {
		return nil
	}

	validModels := make(map[string]bool)
	for _, m := range supportedModels {
		validModels[m] = true
	}

	model := value.String()
	if !validModels[model] {
		return &ValidationError{
			Field:   path,
			Message: fmt.Sprintf("Model '%s' is not supported", model),
			Code:    "unsupported_model",
		}
	}
	return nil
}

// ParseFloat safely parses a float from string
func ParseFloat(s string) (float64, error) {
	return strconv.ParseFloat(s, 64)
}

// ParseInt safely parses an int from string
func ParseInt(s string) (int64, error) {
	return strconv.ParseInt(s, 10, 64)
}

// IsValidJSON validates if a string is valid JSON
func IsValidJSON(s string) bool {
	var js interface{}
	return json.Unmarshal([]byte(s), &js) == nil
}

// ValidateJSONField validates that a field contains valid JSON
func ValidateJSONField() ValidationRule {
	return func(value gjson.Result, path string) *ValidationError {
		if !value.Exists() || value.Type != gjson.String {
			return nil
		}
		if !IsValidJSON(value.String()) {
			return &ValidationError{
				Field:   path,
				Message: fmt.Sprintf("Field '%s' must be valid JSON", path),
				Code:    "invalid_json",
			}
		}
		return nil
	}
}

// ValidateMessages validates the messages array structure
func ValidateMessages() ValidationRule {
	return func(value gjson.Result, path string) *ValidationError {
		if !value.Exists() || !value.IsArray() {
			return nil
		}

		validRoles := map[string]bool{
			"system":    true,
			"developer": true,
			"user":      true,
			"assistant": true,
			"tool":      true,
		}

		for i := 0; i < int(value.Get("#").Int()); i++ {
			msgPath := fmt.Sprintf("%s.%d", path, i)
			role := value.Get(fmt.Sprintf("%d.role", i)).String()
			if role == "" {
				return &ValidationError{
					Field:   msgPath + ".role",
					Message: fmt.Sprintf("Message at index %d is missing 'role' field", i),
					Code:    "missing_message_role",
				}
			}
			if !validRoles[role] {
				return &ValidationError{
					Field:   msgPath + ".role",
					Message: fmt.Sprintf("Invalid role '%s' at message index %d", role, i),
					Code:    "invalid_message_role",
				}
			}
			content := value.Get(fmt.Sprintf("%d.content", i))
			toolCalls := value.Get(fmt.Sprintf("%d.tool_calls", i))
			if !content.Exists() && role != "tool" {
				// Allow assistant messages that have tool_calls to omit content
				if !(role == "assistant" && toolCalls.Exists()) {
					return &ValidationError{
						Field:   msgPath + ".content",
						Message: fmt.Sprintf("Message at index %d is missing 'content' field", i),
						Code:    "missing_message_content",
					}
				}
			}
		}
		return nil
	}
}

// ValidateInput validates the input array for Responses API
func ValidateInput() ValidationRule {
	return func(value gjson.Result, path string) *ValidationError {
		if !value.Exists() || !value.IsArray() {
			return nil
		}

		if int(value.Get("#").Int()) == 0 {
			return &ValidationError{
				Field:   path,
				Message: "Input array cannot be empty",
				Code:    "empty_input",
			}
		}

		validTypes := map[string]bool{
			"message":                 true,
			"reasoning":               true,
			"function_call":           true,
			"function_call_output":    true,
			"tool_call":               true,
			"local_shell_call":        true,
			"local_shell_call_output": true,
			"shell_call":              true,
			"shell_call_output":       true,
			"apply_patch_call":        true,
			"apply_patch_call_output": true,
			"tool_search_call":        true,
			"tool_search_output":      true,
			"custom_tool_call":        true,
			"custom_tool_call_output": true,
			"mcp_tool_call":           true,
			"mcp_tool_call_output":    true,
			"mcp_call":                true,
			"mcp_list_tools":          true,
			"mcp_approval_request":    true,
			"mcp_approval_response":   true,
			"additional_tools":        true,
			"code_interpreter_call":   true,
			"computer_call":           true,
			"computer_call_output":    true,
			"file_search_call":        true,
			"item_reference":          true,
			"image_generation_call":   true,
			"web_search_call":         true,
			"compaction":              true,
			"compaction_trigger":      true,
			"context_compaction":      true,
			"input_text":              true,
			"input_image":             true,
			"output_text":             true,
			"refusal":                 true,
			"input_file":              true,
			"computer_screenshot":     true,
			"summary_text":            true,
			"file":                    true,
			"image":                   true,
		}

		for i := 0; i < int(value.Get("#").Int()); i++ {
			itemType := value.Get(fmt.Sprintf("%d.type", i)).String()
			// If no explicit type is provided, accept the item. This allows
			// message-style inputs (e.g., {role, content}) and other variants
			// that are handled elsewhere in the codebase without requiring
			// clients to set type explicitly.
			if itemType == "" {
				continue
			}
			if !validTypes[itemType] {
				return &ValidationError{
					Field:   fmt.Sprintf("%s.%d.type", path, i),
					Message: fmt.Sprintf("Invalid input type '%s' at index %d", itemType, i),
					Code:    "invalid_input_type",
				}
			}
		}
		return nil
	}
}
