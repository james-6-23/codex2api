package proxy

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
)

// TestAntigravityQuotaCooldownIsolation covers both directions of independent
// Argon/Gemini exhaustion, including shared-cache admission on auto/scoped keys.
func TestAntigravityQuotaCooldownIsolation(t *testing.T) {
	for _, tc := range []struct {
		name      string
		requested string
		wire      string
		sibling   string
		otherPool string
	}{
		{"argon exhausted", "gemini-4-argon-medium", "gemini-4-argon-high", "gemini-4-argon-low", "gemini-3.8-flash-medium"},
		{"ordinary Gemini exhausted", "gemini-3.8-flash-high", "gemini-3.8-flash-tiered", "gemini-3.7-flash-medium", "gemini-4-argon-low"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tokenCache := cache.NewMemory(8)
			defer tokenCache.Close()
			writer := auth.NewStore(nil, tokenCache, &database.SystemSettings{MaxConcurrency: 4})
			defer writer.Stop()
			account := newAntigravityCooldownTestAccount(9751)
			account.Models = []string{"gemini-4-argon-high", "gemini-3.8-flash-tiered", "gemini-3.7-flash-tiered"}
			writer.AddAccount(account)
			body := strings.ReplaceAll(antigravityQuotaExhaustedBody, "gemini-3.7-flash-tiered", tc.wire)
			decision := applyAntigravityCooldown(writer, account, http.StatusTooManyRequests, []byte(body), nil, tc.requested)
			familyKey := antigravityGeminiQuotaCooldownKey(tc.requested, tc.wire)
			if remaining := account.ModelCooldownRemaining(familyKey); remaining < 4*time.Minute {
				t.Fatalf("quota-family cooldown = %s, want the upstream retry window", remaining)
			}
			if decision.ResetAt.IsZero() {
				t.Fatal("real exhaustion must still record a cooldown")
			}
			// An old instance may have left the ambiguous global key in cache.
			// Do not clear it globally or let it revive cross-pool blocking.
			writer.MarkModelCooldownUntil(account, "antigravity:gemini", "quota_exhausted", time.Now().Add(time.Hour))
			otherWire, _ := AntigravityWireModelID(tc.otherPool)
			if antigravityAccountModelRateLimited(account, tc.otherPool, otherWire) {
				t.Fatal("one Gemini quota pool must not rate-limit the other")
			}
			if antigravityAccountModelRateLimited(account, "claude-sonnet-4-6", "claude-sonnet-4-6") {
				t.Fatal("Gemini quota must not rate-limit Claude")
			}
			for _, channel := range []string{"", database.UpstreamChannelAntigravity} {
				reader := auth.NewStore(nil, tokenCache, &database.SystemSettings{MaxConcurrency: 4})
				defer reader.Stop()
				remote := newAntigravityCooldownTestAccount(account.DBID)
				handler := NewHandler(reader, nil, nil, nil)
				ctx := newChannelTestGin(channel)
				for _, check := range []struct {
					model   string
					allowed bool
				}{
					{tc.sibling, false},
					{tc.otherPool, true},
					{"claude-sonnet-4-6", true},
				} {
					filter := handler.withRequestModelCooldownFilter(ctx, context.Background(), check.model, nil)
					if got := filter(remote); got != check.allowed {
						t.Fatalf("channel=%q model=%s allowed=%v, want %v", channel, check.model, got, check.allowed)
					}
				}
			}
		})
	}

	t.Run("argon family key alone", func(t *testing.T) {
		for _, channel := range []struct {
			name  string
			value string
		}{{"auto", ""}, {"antigravity", database.UpstreamChannelAntigravity}} {
			t.Run(channel.name, func(t *testing.T) {
				tokenCache := cache.NewMemory(8)
				defer tokenCache.Close()
				writer := auth.NewStore(nil, tokenCache, &database.SystemSettings{MaxConcurrency: 4})
				defer writer.Stop()
				account := newAntigravityCooldownTestAccount(9752)
				writer.AddAccount(account)
				const model = "gemini-4-argon-low"
				const wire = "gemini-4-argon-high"
				const familyKey = "antigravity:quota-family:argon"
				writer.MarkModelCooldownUntil(account, familyKey, "quota_exhausted", time.Now().Add(time.Minute))
				if account.IsModelRateLimited(model) || account.IsModelRateLimited(wire) {
					t.Fatal("family-only fixture must leave public and wire cooldowns clear")
				}
				if !antigravityAccountModelRateLimited(account, model, wire) {
					t.Fatal("local admission must reject Argon using only its family key")
				}
				reader := auth.NewStore(nil, tokenCache, &database.SystemSettings{MaxConcurrency: 4})
				defer reader.Stop()
				remote := newAntigravityCooldownTestAccount(account.DBID)
				handler := NewHandler(reader, nil, nil, nil)
				filter := handler.withRequestModelCooldownFilter(newChannelTestGin(channel.value), context.Background(), model, nil)
				if filter(remote) {
					t.Fatal("cross-instance admission must reject Argon using only its family key")
				}
				if !remote.IsModelRateLimited(familyKey) {
					t.Fatal("the Argon family cooldown must be hydrated from shared cache")
				}
				if remote.IsModelRateLimited(model) || remote.IsModelRateLimited(wire) {
					t.Fatal("admission must not depend on a public or wire cooldown")
				}
			})
		}
	})
}

// TestAntigravityQuotaCooldownMetadataFallback ensures upstream model metadata
// fills a missing Gemini quota family without overriding a known request family.
func TestAntigravityQuotaCooldownMetadataFallback(t *testing.T) {
	const argonFamily = "antigravity:quota-family:argon"
	const geminiFamily = "antigravity:quota-family:gemini"
	for _, tc := range []struct {
		name       string
		request    string
		upstream   string
		wantFamily string
	}{
		{"Argon metadata fallback", "claude-sonnet-4-6", "gemini-4-argon-high", argonFamily},
		{"Gemini metadata fallback", "claude-sonnet-4-6", "gemini-3.8-flash-tiered", geminiFamily},
		{"known Argon request takes precedence", "gemini-4-argon-low", "gemini-3.8-flash-tiered", argonFamily},
		{"known Gemini request takes precedence", "gemini-3.8-flash-low", "gemini-4-argon-high", geminiFamily},
		{"non-Gemini metadata", "claude-sonnet-4-6", "claude-sonnet-4-6", ""},
		{"missing metadata model", "claude-sonnet-4-6", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newProxyPremiumTestStore()
			defer store.Stop()
			account := newAntigravityCooldownTestAccount(9753)
			account.Models = []string{"gemini-4-argon-high", "gemini-3.8-flash-tiered", "claude-sonnet-4-6"}
			store.AddAccount(account)
			body := strings.ReplaceAll(antigravityQuotaExhaustedBody, "gemini-3.7-flash-tiered", tc.upstream)
			decision := applyAntigravityCooldown(store, account, http.StatusTooManyRequests, []byte(body), nil, tc.request)
			if decision.Model != tc.request || !account.IsModelRateLimited(tc.request) {
				t.Fatal("metadata fallback must preserve the public-model cooldown")
			}
			for _, key := range []string{argonFamily, geminiFamily} {
				remaining := account.ModelCooldownRemaining(key)
				if key == tc.wantFamily {
					if remaining < 4*time.Minute {
						t.Fatalf("family %s cooldown = %s, want the upstream retry window", key, remaining)
					}
				} else if remaining > 0 {
					t.Fatalf("unrelated family %s must not be cooled down", key)
				}
			}
		})
	}
}
