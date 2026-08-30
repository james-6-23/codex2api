package proxy

import (
	"net/http"
	"testing"

	"github.com/codex2api/auth"
)

func TestShouldRecoverCodexOAuthWithSessionToken(t *testing.T) {
	account := &auth.Account{DBID: 42, SessionToken: "independent-web-session"}
	retried := map[int64]bool{}

	if !shouldRecoverCodexOAuthWithSessionToken(account, http.StatusUnauthorized, retried) {
		t.Fatal("expected an official OAuth account with a session token to be recoverable")
	}
	retried[account.ID()] = true
	if shouldRecoverCodexOAuthWithSessionToken(account, http.StatusUnauthorized, retried) {
		t.Fatal("session-token recovery must be attempted at most once per request")
	}
}

func TestShouldNotRecoverRelayOrNonUnauthorizedAccounts(t *testing.T) {
	retried := map[int64]bool{}
	cases := []struct {
		name    string
		account *auth.Account
		status  int
	}{
		{name: "no session token", account: &auth.Account{DBID: 1}, status: http.StatusUnauthorized},
		{name: "non unauthorized", account: &auth.Account{DBID: 2, SessionToken: "st"}, status: http.StatusForbidden},
		{name: "relay", account: &auth.Account{DBID: 3, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: "https://relay.example", APIKey: "relay-key", SessionToken: "st"}, status: http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if shouldRecoverCodexOAuthWithSessionToken(tc.account, tc.status, retried) {
				t.Fatal("account must not enter Codex Web Session recovery")
			}
		})
	}
}
