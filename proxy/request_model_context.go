package proxy

import (
	"strings"

	"github.com/gin-gonic/gin"
)

const trustedRequestedModelContextKey = "trusted_requested_model_v1"

// cacheTrustedRequestedModel preserves the client-facing model before local
// model mapping rewrites the request body. The value is set only by the proxy's
// parsed request path and is never read from a downstream header.
func cacheTrustedRequestedModel(c *gin.Context, model string) {
	if c == nil {
		return
	}
	c.Set(trustedRequestedModelContextKey, strings.TrimSpace(model))
}

func trustedRequestedModel(c *gin.Context, fallback string) string {
	if c != nil {
		if value, exists := c.Get(trustedRequestedModelContextKey); exists {
			if model, ok := value.(string); ok && strings.TrimSpace(model) != "" {
				return strings.TrimSpace(model)
			}
		}
	}
	return strings.TrimSpace(fallback)
}
