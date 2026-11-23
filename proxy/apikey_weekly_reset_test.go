package proxy

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/codex2api/cache"
	"github.com/codex2api/database"
)

func TestAPIKeyThirtyDayLimitsUseExemptedCostAndOriginalTokens(t *testing.T) {
	for _, batch := range []bool{false, true} {
		name := "single window"
		if batch {
			name = "batch windows"
		}
		t.Run(name, func(t *testing.T) {
			for _, tt := range []struct {
				name       string
				exempted   float64
				costLimit  float64
				tokenLimit int64
				status     int
				message    string
			}{
				{"credit releases cost budget", 120, 480, 2000, 0, ""},
				{"net cost still reaches limit", 120, 360, 2000, http.StatusTooManyRequests, "$360.00 / $360.00"},
				{"original token budget still enforced", 120, 480, 1000, http.StatusTooManyRequests, "1000 / 1000 in last 30d"},
				{"ordinary key retains original limit", 0, 480, 2000, http.StatusTooManyRequests, "$480.00 / $480.00"},
			} {
				t.Run(tt.name, func(t *testing.T) {
					tc := &batchCountingTokenCache{TokenCache: cache.NewMemory(1)}
					t.Cleanup(func() { _ = tc.Close() })
					h := &Handler{cache: tc}
					row := &database.APIKeyRow{ID: 42, Limits: database.APIKeyLimits{
						CostLimit30d: tt.costLimit, TokenLimit30d: tt.tokenLimit,
					}}
					if batch {
						row.Limits.CostLimit7d = 120
						h.writeAPIKeyLimitCache(context.Background(), apiKeyLimitsCacheKey(row.ID, "usage", "7d"), &database.APIKeyWindowUsage{})
					}
					h.writeAPIKeyLimitCache(context.Background(), apiKeyLimitsCacheKey(row.ID, "usage", "30d"), &database.APIKeyWindowUsage{
						UserBilled: 480, CostExempted: tt.exempted, Tokens: 1000,
					})
					status, message := h.enforceAPIKeyLimits(limitTestContext(row), "gpt-5.4")
					if status != tt.status || !strings.Contains(message, tt.message) {
						t.Fatalf("limit result = %d %q, want %d containing %q", status, message, tt.status, tt.message)
					}
					if batch && tc.batches.Load() != 1 {
						t.Fatalf("expected batch cache path, got %d batch reads", tc.batches.Load())
					}
				})
			}
		})
	}
}
