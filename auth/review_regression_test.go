package auth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type reviewOAuthTransport func(*http.Request) (*http.Response, error)

func (f reviewOAuthTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestReviewClaudeRefreshInheritsGrantedScopes(t *testing.T) {
	client := &http.Client{Transport: reviewOAuthTransport(func(r *http.Request) (*http.Response, error) {
		status, body := 200, `{"access_token":"new-access","refresh_token":"new-refresh","expires_in":3600}`
		if r.Method == http.MethodPost {
			var payload map[string]string
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Fatal(err)
			}
			if _, ok := payload["scope"]; ok {
				t.Fatal("refresh attempted to expand the original authorization scope")
			}
			if payload["refresh_token"] != "limited-scope-refresh" {
				t.Fatal("refresh credential was lost")
			}
		} else {
			status, body = 404, `{}`
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})}
	tokens, err := (&ClaudeAuth{primary: client, fallback: client}).RefreshTokens(context.Background(), "limited-scope-refresh")
	if err != nil || tokens.RefreshToken != "new-refresh" {
		t.Fatalf("refresh failed: %v", err)
	}
}

func TestReviewSetupTokenPlanCanStillBeInferred(t *testing.T) {
	store := NewStore(nil, nil, nil)
	t.Cleanup(store.Stop)
	account := &Account{UpstreamType: UpstreamClaude, ClaudeAuthKind: ClaudeAuthKindSetupToken, PlanType: "pro", AccessToken: "test"}
	store.ApplyClaudePlanFromGatedModelSuccess(context.Background(), account, "claude-fable-5")
	if account.GetPlanType() != "max" {
		t.Fatal("setup token without profile lost plan inference")
	}
	store.ApplyClaudePlanFromCreditsRequired(context.Background(), account)
	if account.GetPlanType() != "pro" {
		t.Fatal("setup token inference did not react to billing rejection")
	}
}
