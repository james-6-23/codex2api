package proxy

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func newImageLimitCtx(t *testing.T, path string, body string, limits database.APIKeyLimits) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, path, bytes.NewReader([]byte(body)))
	c.Set(contextAPIKeyRow, &database.APIKeyRow{ID: 1, Limits: limits})
	if body != "" {
		setRawRequestBody(c, []byte(body))
	}
	return c
}

func TestEnforceAPIKeyLimits_DisableImageGeneration(t *testing.T) {
	h := &Handler{}
	disabled := database.APIKeyLimits{DisableImageGeneration: true}

	cases := []struct {
		name    string
		path    string
		model   string
		body    string
		blocked bool
	}{
		{"image-only model", "/v1/responses", "gpt-image-2", `{"model":"gpt-image-2","input":"draw a cat"}`, true},
		{"images generations endpoint", "/v1/images/generations", "gpt-image-2", `{"prompt":"a cat"}`, true},
		{"images edits endpoint", "/v1/images/edits", "gpt-image-2", `{"prompt":"a cat"}`, true},
		{"image_generation tool on text model", "/v1/responses", "gpt-5.4", `{"model":"gpt-5.4","input":"hi","tools":[{"type":"image_generation"}]}`, true},
		{"tool_choice image_generation", "/v1/responses", "gpt-5.4", `{"model":"gpt-5.4","input":"hi","tool_choice":{"type":"image_generation"}}`, true},
		{"plain text request", "/v1/responses", "gpt-5.4", `{"model":"gpt-5.4","input":"hello world"}`, false},
		{"plain function tool", "/v1/responses", "gpt-5.4", `{"model":"gpt-5.4","input":"hi","tools":[{"type":"function","name":"foo"}]}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newImageLimitCtx(t, tc.path, tc.body, disabled)
			status, msg := h.enforceAPIKeyLimits(c, tc.model)
			if tc.blocked {
				if status != http.StatusForbidden {
					t.Fatalf("expected 403, got %d (%q)", status, msg)
				}
			} else if status != 0 {
				t.Fatalf("expected pass, got %d (%q)", status, msg)
			}
		})
	}
}

func TestEnforceAPIKeyLimits_ImageAllowedWhenFlagOff(t *testing.T) {
	h := &Handler{}
	// 未开启禁用时，生图请求正常放行（limits 为空 → IsZero → 直接通过）。
	c := newImageLimitCtx(t, "/v1/images/generations", `{"prompt":"a cat"}`, database.APIKeyLimits{})
	if status, msg := h.enforceAPIKeyLimits(c, "gpt-image-2"); status != 0 {
		t.Fatalf("image request should pass when flag off, got %d (%q)", status, msg)
	}
}
