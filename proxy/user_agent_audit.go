package proxy

import (
	"context"
	"strings"
	"sync"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

const maxUsageLogUserAgentLength = 2048

type userAgentAuditContextKey struct{}

type userAgentAudit struct {
	mu                sync.RWMutex
	upstreamUserAgent string
	upstreamKnown     bool
}

func withUserAgentAudit(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if userAgentAuditFromContext(ctx) != nil {
		return ctx
	}
	return context.WithValue(ctx, userAgentAuditContextKey{}, &userAgentAudit{})
}

func userAgentAuditFromContext(ctx context.Context) *userAgentAudit {
	if ctx == nil {
		return nil
	}
	audit, _ := ctx.Value(userAgentAuditContextKey{}).(*userAgentAudit)
	return audit
}

func resetUpstreamUserAgentAudit(ctx context.Context) {
	if audit := userAgentAuditFromContext(ctx); audit != nil {
		audit.mu.Lock()
		audit.upstreamUserAgent = ""
		audit.upstreamKnown = false
		audit.mu.Unlock()
	}
}

// RecordUpstreamUserAgent records the final User-Agent used for an upstream
// HTTP request or WebSocket handshake. An empty value is meaningful when the
// WebSocket User-Agent has explicitly been disabled.
func RecordUpstreamUserAgent(ctx context.Context, userAgent string) {
	if audit := userAgentAuditFromContext(ctx); audit != nil {
		audit.mu.Lock()
		audit.upstreamUserAgent = normalizeUsageLogUserAgent(userAgent)
		audit.upstreamKnown = true
		audit.mu.Unlock()
	}
}

func upstreamUserAgentAudit(ctx context.Context) (string, bool) {
	audit := userAgentAuditFromContext(ctx)
	if audit == nil {
		return "", false
	}
	audit.mu.RLock()
	defer audit.mu.RUnlock()
	return audit.upstreamUserAgent, audit.upstreamKnown
}

func attachUserAgentAudit(c *gin.Context) {
	if c == nil || c.Request == nil {
		return
	}
	c.Request = c.Request.WithContext(withUserAgentAudit(c.Request.Context()))
}

func populateUserAgentMetaFromRequest(c *gin.Context, input *database.UsageLogInput) {
	if c == nil || c.Request == nil || input == nil {
		return
	}
	clientUserAgent := normalizeUsageLogUserAgent(c.GetHeader("User-Agent"))
	input.ClientUserAgent = clientUserAgent
	if upstreamUserAgent, ok := upstreamUserAgentAudit(c.Request.Context()); ok {
		input.UpstreamUserAgent = upstreamUserAgent
		input.UserAgentOverridden = clientUserAgent != upstreamUserAgent
	}
}

func normalizeUsageLogUserAgent(userAgent string) string {
	userAgent = strings.ToValidUTF8(strings.TrimSpace(userAgent), "")
	if len(userAgent) <= maxUsageLogUserAgentLength {
		return userAgent
	}
	return strings.ToValidUTF8(userAgent[:maxUsageLogUserAgentLength], "")
}
