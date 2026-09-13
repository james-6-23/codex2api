package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestCodexAccountIdentityPassiveHTTPAndCompact(test *testing.T) {
	test.Setenv("CODEX_OUTBOUND_SESSION_MODE", "account")
	previousResin := GetResinConfig()
	test.Cleanup(func() { SetResinConfig(previousResin) })
	type capture struct {
		headers http.Header
		body    []byte
	}
	received := make(chan capture, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		received <- capture{request.Header.Clone(), readUpstreamRequestBody(request)}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"id":"response-test"}`))
	}))
	test.Cleanup(server.Close)
	SetResinConfig(&ResinConfig{BaseURL: server.URL, PlatformName: "passive-identity"})
	db, err := database.New("sqlite", filepath.Join(test.TempDir(), "passive.db"))
	require.NoError(test, err)
	test.Cleanup(func() { require.NoError(test, db.Close()) })
	ctx := WithCodexIdentityStore(context.Background(), db)
	accounts := []*auth.Account{
		{DBID: 1695, AccountID: accountIdentitySampleAccount, AccessToken: "first-token", CodexFingerprintMode: auth.CodexFingerprintModeDevice},
		{DBID: 1696, AccountID: "761373c1-f1a9-4ca9-8682-a0594b30c36c", AccessToken: "second-token", CodexFingerprintMode: auth.CodexFingerprintModeDevice},
	}
	for _, source := range []string{"thread_description", "guardian_review", "memory_consolidation", "agent_created_thread", "compaction"} {
		test.Run(source, func(test *testing.T) {
			var previousSession string
			for _, account := range accounts {
				child := source == "guardian_review"
				headers, body := accountIdentityFixture(test, child, true)
				body, err = sjson.SetBytes(body, "input", "diagnostic context")
				require.NoError(test, err)
				body, err = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata.thread_source", source)
				require.NoError(test, err)
				if source == "compaction" {
					body, err = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata.request_kind", "compaction")
					require.NoError(test, err)
				}
				headers.Set(codexTurnMetadataHeader, gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata").Raw)
				var sameAccountSession string
				for attempt := 0; attempt < 2; attempt++ {
					var response *http.Response
					if source == "compaction" {
						response, err = ExecuteCompactRequest(ctx, account, body, "cache", "", "test-user-key", nil, headers)
					} else {
						response, err = ExecuteRequest(ctx, account, body, "cache", "", "test-user-key", nil, headers, false)
					}
					require.NoError(test, err)
					_, err = io.Copy(io.Discard, response.Body)
					require.NoError(test, err)
					require.NoError(test, response.Body.Close())
					sent := <-received
					session := sent.headers.Get("Session-Id")
					thread := sent.headers.Get("Thread-Id")
					require.NotEqual(test, accountIdentitySampleRoot, session)
					require.NotEqual(test, accountIdentitySampleRoot[:13], session[:13])
					require.Equal(test, account.AccountID, sent.headers.Get("Chatgpt-Account-Id"))
					require.Equal(test, session, gjson.GetBytes(sent.body, "client_metadata.session_id").String())
					require.Equal(test, thread, gjson.GetBytes(sent.body, "client_metadata.thread_id").String())
					require.Equal(test, thread, sent.headers.Get("X-Client-Request-Id"))
					metadata := diagnosticMetadataObject(gjson.GetBytes(sent.body, "client_metadata.x-codex-turn-metadata"))
					require.Equal(test, source, metadata.Get("thread_source").String())
					if child {
						require.NotEqual(test, session, thread)
						require.Equal(test, session, metadata.Get("parent_thread_id").String())
					} else {
						require.False(test, metadata.Get("parent_thread_id").Exists())
					}
					require.Equal(test, "matched", outboundSessionConsistency(&outboundIdentityDiagnostic{HTTP: CaptureOutboundIdentityHeaders(sent.headers), Body: captureOutboundIdentityBody(sent.body)}))
					if attempt == 0 {
						sameAccountSession = session
						require.NotEqual(test, previousSession, session)
					} else {
						require.Equal(test, sameAccountSession, session)
					}
				}
				previousSession = sameAccountSession
			}
		})
	}
}
