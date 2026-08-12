package admin

import (
	"net/http/httptest"
	"testing"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// Claude 视图靠 ?channel=claude 拿到自己的账号集合；这里没有归一化的话，
// 未知渠道会退回「不限」，Claude 页会把 Codex/Grok 账号一起拉进列表。
func TestParseUsageChannelRecognizesClaude(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		query string
		want  string
	}{
		{name: "claude", query: "claude", want: database.UpstreamChannelClaude},
		{name: "claude is case insensitive", query: "Claude", want: database.UpstreamChannelClaude},
		{name: "grok still resolves", query: "grok", want: database.UpstreamChannelGrok},
		{name: "codex still resolves", query: "codex", want: database.UpstreamChannelCodex},
		{name: "unknown means unfiltered", query: "anthropic", want: ""},
		{name: "empty means unfiltered", query: "", want: ""},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ginContext, _ := gin.CreateTestContext(httptest.NewRecorder())
			ginContext.Request = httptest.NewRequest("GET", "/api/admin/accounts?channel="+tt.query, nil)
			if got := parseUsageChannel(ginContext); got != tt.want {
				t.Fatalf("parseUsageChannel(%q) = %q, want %q", tt.query, got, tt.want)
			}
		})
	}
}
