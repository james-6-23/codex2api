package auth

import (
	"testing"
	"time"
)

func TestParseClaudeCredentialsJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		raw       string
		wantCount int
		wantErr   bool
		check     func(t *testing.T, creds []ClaudeCredential)
	}{
		{
			name: "claude code credentials file with millisecond expiry",
			raw: `{"claudeAiOauth":{"accessToken":"sk-ant-oat01-fake","refreshToken":"sk-ant-ort01-fake",
				"expiresAt":1786548694331,"scopes":["user:chat","user:inference","user:profile"],
				"subscriptionType":"default_claude_ai"},"email":"pool@example.com","scope":"full"}`,
			wantCount: 1,
			check: func(t *testing.T, creds []ClaudeCredential) {
				cred := creds[0]
				if cred.AccessToken != "sk-ant-oat01-fake" || cred.RefreshToken != "sk-ant-ort01-fake" {
					t.Fatalf("tokens = %q / %q", cred.AccessToken, cred.RefreshToken)
				}
				if cred.Email != "pool@example.com" {
					t.Fatalf("email = %q, want pool@example.com", cred.Email)
				}
				if cred.SubscriptionType != "default_claude_ai" {
					t.Fatalf("subscription = %q", cred.SubscriptionType)
				}
				if got, want := cred.Scope, "user:chat user:inference user:profile"; got != want {
					t.Fatalf("scope = %q, want %q", got, want)
				}
				if want := time.UnixMilli(1786548694331); !cred.ExpiresAt.Equal(want) {
					t.Fatalf("expiresAt = %s, want %s", cred.ExpiresAt, want)
				}
			},
		},
		{
			name:      "flat snake case shape with RFC3339 expiry",
			raw:       `{"access_token":"at","refresh_token":"rt","expires_at":"2026-08-12T11:49:59Z","scope":"user:inference"}`,
			wantCount: 1,
			check: func(t *testing.T, creds []ClaudeCredential) {
				want, _ := time.Parse(time.RFC3339, "2026-08-12T11:49:59Z")
				if !creds[0].ExpiresAt.Equal(want) {
					t.Fatalf("expiresAt = %s, want %s", creds[0].ExpiresAt, want)
				}
				if creds[0].Scope != "user:inference" {
					t.Fatalf("scope = %q", creds[0].Scope)
				}
			},
		},
		{
			name:      "second precision expiry is not read as milliseconds",
			raw:       `{"refreshToken":"rt","expiresAt":1786548694}`,
			wantCount: 1,
			check: func(t *testing.T, creds []ClaudeCredential) {
				if want := time.Unix(1786548694, 0); !creds[0].ExpiresAt.Equal(want) {
					t.Fatalf("expiresAt = %s, want %s", creds[0].ExpiresAt, want)
				}
			},
		},
		{
			name:      "array batch keeps order",
			raw:       `[{"refreshToken":"rt-1","email":"a@example.com"},{"refreshToken":"rt-2","email":"b@example.com"}]`,
			wantCount: 2,
			check: func(t *testing.T, creds []ClaudeCredential) {
				if creds[0].RefreshToken != "rt-1" || creds[1].RefreshToken != "rt-2" {
					t.Fatalf("order lost: %q, %q", creds[0].RefreshToken, creds[1].RefreshToken)
				}
			},
		},
		{
			name:      "one object per line",
			raw:       "{\"refreshToken\":\"rt-1\"}\n\n{\"refreshToken\":\"rt-2\"}\n",
			wantCount: 2,
		},
		{
			name:      "missing refresh token is rejected",
			raw:       `{"claudeAiOauth":{"accessToken":"at-only","expiresAt":1786548694331}}`,
			wantErr:   true,
			wantCount: 0,
		},
		{
			name:      "unparsable expiry leaves a zero value instead of failing",
			raw:       `{"refreshToken":"rt","expiresAt":"not-a-date"}`,
			wantCount: 1,
			check: func(t *testing.T, creds []ClaudeCredential) {
				if !creds[0].ExpiresAt.IsZero() {
					t.Fatalf("expiresAt = %s, want zero", creds[0].ExpiresAt)
				}
			},
		},
		{
			name:      "empty content is rejected",
			raw:       "   ",
			wantErr:   true,
			wantCount: 0,
		},
		{
			name:      "garbage is rejected",
			raw:       "not json at all",
			wantErr:   true,
			wantCount: 0,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			creds, err := ParseClaudeCredentialsJSON([]byte(tt.raw))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseClaudeCredentialsJSON() error = nil, want error")
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseClaudeCredentialsJSON() error = %v", err)
			}
			if len(creds) != tt.wantCount {
				t.Fatalf("parsed %d credentials, want %d", len(creds), tt.wantCount)
			}
			if tt.check != nil {
				tt.check(t, creds)
			}
		})
	}
}

func TestClaudeSubscriptionScopeSufficient(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		scope string
		want  bool
	}{
		{name: "claude code scope set", scope: "user:profile user:inference user:sessions:claude_code", want: true},
		{name: "inference alone is enough", scope: "user:chat user:inference user:profile", want: true},
		{name: "profile only cannot infer", scope: "user:profile", want: false},
		{name: "unknown scope cannot infer", scope: "org:create_api_key", want: false},
		{name: "blank defers to the real request", scope: "", want: true},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := ClaudeSubscriptionScopeSufficient(tt.scope); got != tt.want {
				t.Fatalf("ClaudeSubscriptionScopeSufficient(%q) = %t, want %t", tt.scope, got, tt.want)
			}
		})
	}
}

func TestClaudePlanFromSubscriptionType(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name             string
		subscriptionType string
		want             string
	}{
		{name: "max 20x", subscriptionType: "claude_max_20x", want: "max20"},
		{name: "max 5x", subscriptionType: "claude_max_5x", want: "max5"},
		{name: "team", subscriptionType: "claude_team", want: "team"},
		// default_claude_ai carries no tier keyword; bootstrap resolves it later.
		{name: "subscription without a tier keyword stays blank", subscriptionType: "default_claude_ai", want: ""},
		{name: "blank stays blank", subscriptionType: "", want: ""},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := ClaudePlanFromSubscriptionType(tt.subscriptionType); got != tt.want {
				t.Fatalf("ClaudePlanFromSubscriptionType(%q) = %q, want %q", tt.subscriptionType, got, tt.want)
			}
		})
	}
}
