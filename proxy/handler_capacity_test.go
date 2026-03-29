package proxy

import (
	"testing"
)

func TestIsCodexModelCapacityError(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		expected bool
	}{
		{
			name:     "selected model is at capacity",
			body:     `{"error": {"message": "Selected model is at capacity"}}`,
			expected: true,
		},
		{
			name:     "model is at capacity please try a different model",
			body:     `{"error": {"message": "Model is at capacity. Please try a different model"}}`,
			expected: true,
		},
		{
			name:     "model is currently at capacity",
			body:     `{"message": "Model is currently at capacity"}`,
			expected: true,
		},
		{
			name:     "case insensitive match",
			body:     `{"error": {"message": "SELECTED MODEL IS AT CAPACITY"}}`,
			expected: true,
		},
		{
			name:     "unrelated error",
			body:     `{"error": {"message": "Invalid request"}}`,
			expected: false,
		},
		{
			name:     "empty body",
			body:     ``,
			expected: false,
		},
		{
			name:     "rate limit error not capacity",
			body:     `{"error": {"type": "usage_limit_reached", "message": "Rate limit exceeded"}}`,
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := isCodexModelCapacityError([]byte(tt.body))
			if result != tt.expected {
				t.Errorf("isCodexModelCapacityError(%s) = %v, want %v", tt.body, result, tt.expected)
			}
		})
	}
}