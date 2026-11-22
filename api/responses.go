// Package api provides standardized API response formats and utilities
package api

import (
	"net/http"

	"github.com/gin-gonic/gin"
)

// ResponseMeta contains metadata for API responses
type ResponseMeta struct {
	RequestID string `json:"request_id,omitempty"`
	Version   string `json:"version"`
}

// SuccessResponse represents a standardized successful response wrapper
type SuccessResponse struct {
	Data interface{} `json:"data"`
	Meta *ResponseMeta `json:"meta,omitempty"`
}

// ListResponse represents a standardized list response
type ListResponse struct {
	Object  string      `json:"object"`
	Data    interface{} `json:"data"`
	Meta    *ResponseMeta `json:"meta,omitempty"`
	HasMore *bool       `json:"has_more,omitempty"`
}

// PaginatedResponse represents a paginated list response
type PaginatedResponse struct {
	Object     string      `json:"object"`
	Data       interface{} `json:"data"`
	Meta       *ResponseMeta `json:"meta,omitempty"`
	HasMore    bool        `json:"has_more"`
	Total      int         `json:"total,omitempty"`
	Page       int         `json:"page,omitempty"`
	PageSize   int         `json:"page_size,omitempty"`
}

// SendSuccess sends a standardized success response
func SendSuccess(c *gin.Context, data interface{}) {
	c.JSON(http.StatusOK, data)
}

// SendSuccessWithMeta sends a success response with metadata
func SendSuccessWithMeta(c *gin.Context, data interface{}, meta *ResponseMeta) {
	c.JSON(http.StatusOK, SuccessResponse{
		Data: data,
		Meta: meta,
	})
}

// SendCreated sends a created response
func SendCreated(c *gin.Context, data interface{}) {
	c.JSON(http.StatusCreated, data)
}

// SendList sends a list response
func SendList(c *gin.Context, object string, data interface{}, hasMore ...bool) {
	resp := ListResponse{
		Object: object,
		Data:   data,
	}
	if len(hasMore) > 0 {
		resp.HasMore = &hasMore[0]
	}
	c.JSON(http.StatusOK, resp)
}

// SendPaginated sends a paginated list response
func SendPaginated(c *gin.Context, object string, data interface{}, total, page, pageSize int) {
	hasMore := (page * pageSize) < total
	c.JSON(http.StatusOK, PaginatedResponse{
		Object:   object,
		Data:     data,
		HasMore:  hasMore,
		Total:    total,
		Page:     page,
		PageSize: pageSize,
	})
}

// SendNoContent sends a 204 No Content response
func SendNoContent(c *gin.Context) {
	c.Status(http.StatusNoContent)
}

// SendAccepted sends a 202 Accepted response
func SendAccepted(c *gin.Context, data interface{}) {
	c.JSON(http.StatusAccepted, data)
}

// Model represents an OpenAI-style model object
type Model struct {
	ID                       string                `json:"id"`
	Object                   string                `json:"object"`
	Created                  int64                 `json:"created"`
	OwnedBy                  string                `json:"owned_by"`
	Root                     string                `json:"root,omitempty"`
	Parent                   string                `json:"parent,omitempty"`
	SupportedReasoningLevels []ModelReasoningLevel `json:"supported_reasoning_levels,omitempty"`
	DefaultReasoningLevel    string                `json:"default_reasoning_level,omitempty"`
}

type ModelReasoningLevel struct {
	Effort      string `json:"effort"`
	Description string `json:"description"`
}

// ModelList represents a list of models
type ModelList struct {
	Object string  `json:"object"`
	Data   []Model `json:"data"`
}

// ChatCompletionResponse represents a chat completion response
type ChatCompletionResponse struct {
	ID       string                 `json:"id"`
	Object   string                 `json:"object"`
	Created  int64                  `json:"created"`
	Model    string                 `json:"model"`
	Choices  []ChatCompletionChoice `json:"choices"`
	Usage    *UsageInfo             `json:"usage,omitempty"`
	SystemFingerprint string        `json:"system_fingerprint,omitempty"`
}

// ChatCompletionChoice represents a choice in chat completion
type ChatCompletionChoice struct {
	Index        int         `json:"index"`
	Message      *Message    `json:"message,omitempty"`
	Delta        *Message    `json:"delta,omitempty"`
	FinishReason string      `json:"finish_reason,omitempty"`
	Logprobs     interface{} `json:"logprobs,omitempty"`
}

// Message represents a chat message
type Message struct {
	Role             string     `json:"role"`
	Content          string     `json:"content,omitempty"`
	ReasoningContent string     `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
}

// ToolCall represents a tool call in a message
type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// FunctionCall represents a function call
type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// UsageInfo represents token usage information
type UsageInfo struct {
	PromptTokens     int            `json:"prompt_tokens"`
	CompletionTokens int            `json:"completion_tokens"`
	TotalTokens      int            `json:"total_tokens"`
	PromptTokensDetails *TokenDetails `json:"prompt_tokens_details,omitempty"`
	CompletionTokensDetails *TokenDetails `json:"completion_tokens_details,omitempty"`
}

// TokenDetails provides detailed token usage
type TokenDetails struct {
	CachedTokens int `json:"cached_tokens,omitempty"`
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}

// StreamChunk represents a streaming response chunk
type StreamChunk struct {
	ID       string                 `json:"id"`
	Object   string                 `json:"object"`
	Created  int64                  `json:"created"`
	Model    string                 `json:"model"`
	Choices  []ChatCompletionChoice `json:"choices"`
	Usage    *UsageInfo             `json:"usage,omitempty"`
	SystemFingerprint string        `json:"system_fingerprint,omitempty"`
}

// ResponsesAPIResponse represents a responses API response
type ResponsesAPIResponse struct {
	ID           string      `json:"id"`
	Object       string      `json:"object"`
	CreatedAt    int64       `json:"created_at"`
	Status       string      `json:"status"`
	Error        *APIError   `json:"error,omitempty"`
	IncompleteDetails interface{} `json:"incomplete_details,omitempty"`
	Instructions string      `json:"instructions,omitempty"`
	MaxOutputTokens int      `json:"max_output_tokens,omitempty"`
	Model        string      `json:"model"`
	Output       []OutputItem `json:"output"`
	ParallelToolCalls bool   `json:"parallel_tool_calls,omitempty"`
	PreviousResponseID string `json:"previous_response_id,omitempty"`
	Reasoning    *ReasoningConfig `json:"reasoning,omitempty"`
	Store        bool             `json:"store,omitempty"`
	Temperature  float64          `json:"temperature,omitempty"`
	ToolChoice   interface{}      `json:"tool_choice,omitempty"`
	Tools        interface{}      `json:"tools,omitempty"`
	TopP         float64          `json:"top_p,omitempty"`
	Truncation   string           `json:"truncation,omitempty"`
	Usage        *UsageInfo       `json:"usage,omitempty"`
	User         string           `json:"user,omitempty"`
	Metadata     map[string]string `json:"metadata,omitempty"`
}

// OutputItem represents an output item in responses API
type OutputItem struct {
	Type        string          `json:"type"`
	ID          string          `json:"id,omitempty"`
	Status      string          `json:"status,omitempty"`
	Role        string          `json:"role,omitempty"`
	Content     []ContentPart   `json:"content,omitempty"`
	Name        string          `json:"name,omitempty"`
	Arguments   string          `json:"arguments,omitempty"`
	CallID      string          `json:"call_id,omitempty"`
}

// ContentPart represents a content part
type ContentPart struct {
	Type       string      `json:"type"`
	Text       string      `json:"text,omitempty"`
	ImageURL   string      `json:"image_url,omitempty"`
}

// ReasoningConfig represents reasoning configuration
type ReasoningConfig struct {
	Effort string `json:"effort,omitempty"`
}
