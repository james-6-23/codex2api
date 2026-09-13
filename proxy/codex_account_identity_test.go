package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const accountIdentitySampleRoot = "01a09302-49f4-7b53-b545-91ef29610317"
const accountIdentitySampleContext = "01a09302-49f4-7b53-b545-91fb553a57b4"
const accountIdentitySampleAccount = "661373c1-f1a9-4ca9-8682-a0594b30c36c"

func accountIdentityFixture(test *testing.T, child bool, object bool) (http.Header, []byte) {
	test.Helper()
	thread := accountIdentitySampleRoot
	metadata := map[string]any{
		"session_id": accountIdentitySampleRoot, "thread_id": thread,
		"context_window_id": accountIdentitySampleContext, "window_id": thread + ":0", "window_number": 0,
		"turn_id": "01a0939f-d89c-77f1-94fa-080df9ebda48", "root_turn_id": "01a0939f-d89c-77f1-94fa-080df9ebda48",
		"turn_started_at_unix_ms": int64(1789183121593), "thread_source": "user", "request_kind": "turn",
		"installation_id": "9dcfc09b-4e8b-4e25-9052-fefb87224807",
	}
	if child {
		thread = "01a09303-49f4-7b53-b545-920f29610317"
		metadata["thread_id"], metadata["window_id"] = thread, thread+":2"
		metadata["window_number"] = 2
		metadata["parent_thread_id"], metadata["forked_from_thread_id"] = accountIdentitySampleRoot, accountIdentitySampleRoot
		metadata["thread_source"], metadata["subagent_kind"] = "guardian_review", "guardian"
	}
	raw, err := json.Marshal(metadata)
	require.NoError(test, err)
	var embedded any = string(raw)
	if object {
		embedded = metadata
	}
	client := map[string]any{"session_id": accountIdentitySampleRoot, "thread_id": thread, "x-client-request-id": thread, "x-codex-window-id": metadata["window_id"], "x-codex-installation-id": metadata["installation_id"], "x-codex-turn-metadata": embedded}
	if child {
		client["parent_thread_id"], client["x-codex-forked-from-thread-id"] = accountIdentitySampleRoot, accountIdentitySampleRoot
	}
	body, err := json.Marshal(map[string]any{"model": "gpt-6-astra", "client_metadata": client, "prompt_cache_key": "original-cache", "input": []any{map[string]any{"type": "compaction", "id": "opaque-id", "encrypted_content": "private-encrypted"}}})
	require.NoError(test, err)
	headers := http.Header{}
	headers.Set("Session-Id", accountIdentitySampleRoot)
	headers.Set("Thread-Id", thread)
	headers.Set("X-Client-Request-Id", thread)
	headers.Set(codexTurnMetadataHeader, string(raw))
	headers.Set("X-NewAPI-Meta", "signed-original-do-not-rewrite")
	return CodexRequestMetadataHeaders(headers, body), body
}

func TestCodexAccountIdentityStableAcrossTransportsAccountsAndRestart(test *testing.T) {
	test.Setenv("CODEX_OUTBOUND_SESSION_MODE", "account")
	path := filepath.Join(test.TempDir(), "identities.db")
	db, err := database.New("sqlite", path)
	require.NoError(test, err)
	account := &auth.Account{DBID: 1695, AccountID: accountIdentitySampleAccount, CodexFingerprintMode: auth.CodexFingerprintModeDevice}
	headers, body := accountIdentityFixture(test, true, true)
	originalBody, originalHeaders := bytes.Clone(body), headers.Clone()
	ctx := WithCodexIdentityStore(context.Background(), db)
	first := NewCodexTransportFingerprint(account, headers, body, "cache")
	require.NoError(test, first.ClaimSessionIdentity(ctx, account, "test-user-key"))
	mapped := first.ApplyBody(body)
	root := gjson.GetBytes(mapped, "client_metadata.session_id").String()
	thread := gjson.GetBytes(mapped, "client_metadata.thread_id").String()
	require.NotEqual(test, accountIdentitySampleRoot[:13], root[:13])
	require.NotEqual(test, accountIdentitySampleRoot, root)
	require.NotEqual(test, root, thread)
	require.Equal(test, root, gjson.GetBytes(mapped, "client_metadata.parent_thread_id").String())
	require.Equal(test, root, gjson.GetBytes(mapped, "client_metadata.x-codex-forked-from-thread-id").String())
	require.Equal(test, thread+":2", gjson.GetBytes(mapped, "client_metadata.x-codex-window-id").String())
	require.Equal(test, thread, gjson.GetBytes(mapped, "client_metadata.x-client-request-id").String())
	metadata := gjson.GetBytes(mapped, "client_metadata.x-codex-turn-metadata")
	require.Equal(test, root, metadata.Get("session_id").String())
	require.Equal(test, root, metadata.Get("parent_thread_id").String())
	require.Equal(test, root, metadata.Get("forked_from_thread_id").String())
	require.NotEqual(test, accountIdentitySampleContext, metadata.Get("context_window_id").String())
	for _, field := range []string{"turn_id", "root_turn_id"} {
		original := gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata."+field).String()
		require.NotEqual(test, original, metadata.Get(field).String())
		require.NotEqual(test, original[:13], metadata.Get(field).String()[:13])
	}
	require.Equal(test, metadata.Get("turn_id").String(), metadata.Get("root_turn_id").String())
	for _, field := range []string{"turn_started_at_unix_ms", "request_kind", "window_number"} {
		require.Equal(test, gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata."+field).Raw, metadata.Get(field).Raw)
	}
	require.Equal(test, gjson.GetBytes(body, "input").Raw, gjson.GetBytes(mapped, "input").Raw)
	request := httptest.NewRequest(http.MethodPost, "/responses", nil)
	applyCodexRequestHeaders(request, account, "test-token", "cache", "test-user-key", nil, headers, first)
	require.Equal(test, root, request.Header.Get("Session-Id"))
	require.Equal(test, thread, request.Header.Get("Thread-Id"))
	require.Equal(test, "matched", outboundSessionConsistency(&outboundIdentityDiagnostic{HTTP: CaptureOutboundIdentityHeaders(request.Header), Body: captureOutboundIdentityBody(mapped)}))
	require.Equal(test, originalBody, body)
	require.Equal(test, originalHeaders, headers)
	require.Equal(test, mapped, first.ApplyBody(mapped))
	cacheKey := first.ScopeCacheKey(ctx, "cache")
	require.NotEqual(test, "cache", cacheKey)
	require.NoError(test, db.Close())
	db, err = database.New("sqlite", path)
	require.NoError(test, err)
	test.Cleanup(func() { require.NoError(test, db.Close()) })
	ctx = WithCodexIdentityStore(context.Background(), db)
	resumed := NewCodexTransportFingerprint(account, headers, body, "cache")
	require.NoError(test, resumed.ClaimSessionIdentity(ctx, account, "test-user-key"))
	require.Equal(test, mapped, resumed.ApplyBody(body))
	require.Equal(test, cacheKey, resumed.ScopeCacheKey(ctx, "cache"))
	duplicate := &auth.Account{DBID: 2695, AccountID: accountIdentitySampleAccount, CodexFingerprintMode: auth.CodexFingerprintModeDevice}
	duplicateFingerprint := NewCodexTransportFingerprint(duplicate, headers, body, "cache")
	require.NoError(test, duplicateFingerprint.ClaimSessionIdentity(ctx, duplicate, "test-user-key"))
	require.Equal(test, root, gjson.GetBytes(duplicateFingerprint.ApplyBody(body), "client_metadata.session_id").String())
	other := &auth.Account{DBID: 2696, AccountID: "761373c1-f1a9-4ca9-8682-a0594b30c36c"}
	otherFingerprint := NewCodexTransportFingerprint(other, headers, body, "cache")
	require.NoError(test, otherFingerprint.ClaimSessionIdentity(ctx, other, "test-user-key"))
	require.NotEqual(test, root, gjson.GetBytes(otherFingerprint.ApplyBody(body), "client_metadata.session_id").String())
	require.NotEqual(test, cacheKey, otherFingerprint.ScopeCacheKey(ctx, "cache"))
	require.Error(test, first.ClaimSessionIdentity(ctx, other, "test-user-key"))
	test.Setenv("CODEX_OUTBOUND_SESSION_MODE", "preserve")
	retained := NewCodexTransportFingerprint(account, headers, body, "cache")
	require.NoError(test, retained.ClaimSessionIdentity(ctx, account, "test-user-key"))
	require.Equal(test, root, gjson.GetBytes(retained.ApplyBody(body), "client_metadata.session_id").String())
}

func TestCodexAccountIdentityPreservesExistingAndFailsClosed(test *testing.T) {
	db, err := database.New("sqlite", filepath.Join(test.TempDir(), "legacy.db"))
	require.NoError(test, err)
	test.Cleanup(func() { require.NoError(test, db.Close()) })
	account := &auth.Account{DBID: 1695, AccountID: accountIdentitySampleAccount}
	headers, body := accountIdentityFixture(test, false, false)
	owner := codexIdentityDigest("codex-owner-v1", "credential:test-key")
	legacy := codexIdentityDigest("codex-session-v1", "account:1695", accountIdentitySampleRoot)
	require.NoError(test, db.ClaimCodexIdentities(context.Background(), []string{legacy}, owner))
	test.Setenv("CODEX_OUTBOUND_SESSION_MODE", "account")
	request := transportTestContext()
	request.Request = request.Request.WithContext(WithCodexIdentityStore(request.Request.Context(), db))
	beginUpstreamTrace(request.Request.Context(), account, "", false)
	fingerprint := NewCodexTransportFingerprint(account, headers, body, "cache")
	require.NoError(test, fingerprint.ClaimSessionIdentity(request.Request.Context(), account, "test-key"))
	require.Equal(test, accountIdentitySampleRoot, gjson.GetBytes(fingerprint.ApplyBody(body), "client_metadata.session_id").String())
	require.Equal(test, "preserved_existing", snapshotUpstreamTrace(request.Request.Context()).Transport.OutboundIdentity.AccountMapping.Status)
	fresh := NewCodexTransportFingerprint(account, headers, body, "cache")
	require.Error(test, fresh.ClaimSessionIdentity(context.Background(), account, "test-key"))
	otherHeaders, otherBody := accountIdentityFixture(test, false, true)
	otherHeaders.Set("Session-Id", "invalid-root")
	otherBody, _ = sjson.SetBytes(otherBody, "client_metadata.x-codex-turn-metadata.session_id", "invalid-root")
	invalid := NewCodexTransportFingerprint(account, otherHeaders, otherBody, "cache")
	require.Error(test, invalid.ClaimSessionIdentity(request.Request.Context(), account, "test-key"))
}

func TestCodexAccountIdentityHTTPAndCompactFinalBytes(test *testing.T) {
	test.Setenv("CODEX_OUTBOUND_SESSION_MODE", "account")
	previous := GetResinConfig()
	test.Cleanup(func() { SetResinConfig(previous) })
	type capture struct {
		headers http.Header
		body    []byte
	}
	received := make(chan capture, 2)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		received <- capture{request.Header.Clone(), readUpstreamRequestBody(request)}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"id":"response-test"}`))
	}))
	test.Cleanup(server.Close)
	SetResinConfig(&ResinConfig{BaseURL: server.URL, PlatformName: "identity-test"})
	db, err := database.New("sqlite", filepath.Join(test.TempDir(), "http.db"))
	require.NoError(test, err)
	test.Cleanup(func() { require.NoError(test, db.Close()) })
	account := &auth.Account{DBID: 1695, AccountID: accountIdentitySampleAccount, AccessToken: "test-access-token", CodexFingerprintMode: auth.CodexFingerprintModeDevice, CustomHeaders: map[string]string{"Session-Id": "bad-custom-session", "Thread-Id": "bad-custom-thread", "X-Codex-Turn-Metadata": `{"session_id":"bad-custom"}`}}
	var firstSession string
	for _, compact := range []bool{false, true} {
		headers, body := accountIdentityFixture(test, false, !compact)
		request := transportTestContext()
		request.Request = request.Request.WithContext(WithCodexIdentityStore(request.Request.Context(), db))
		var response *http.Response
		if compact {
			response, err = ExecuteCompactRequest(request.Request.Context(), account, body, "cache", "", "test-key", nil, headers)
		} else {
			response, err = ExecuteRequest(request.Request.Context(), account, body, "cache", "", "test-key", nil, headers, false)
		}
		require.NoError(test, err)
		_, err = io.Copy(io.Discard, response.Body)
		require.NoError(test, err)
		require.NoError(test, response.Body.Close())
		sent := <-received
		session := sent.headers.Get("Session-Id")
		if firstSession == "" {
			firstSession = session
		}
		require.Equal(test, firstSession, session)
		require.NotEqual(test, accountIdentitySampleRoot, session)
		require.Equal(test, session, sent.headers.Get("Thread-Id"))
		require.Equal(test, session, sent.headers.Get("X-Client-Request-Id"))
		require.Equal(test, session, gjson.GetBytes(sent.body, "client_metadata.session_id").String())
		require.Equal(test, "matched", outboundSessionConsistency(&outboundIdentityDiagnostic{HTTP: CaptureOutboundIdentityHeaders(sent.headers), Body: captureOutboundIdentityBody(sent.body)}))
		require.NotEqual(test, "cache", gjson.GetBytes(sent.body, "prompt_cache_key").String())
		trace := snapshotUpstreamTrace(request.Request.Context())
		require.NotNil(test, trace.Transport)
		diagnostic := trace.Transport.OutboundIdentity
		require.NotNil(test, diagnostic)
		require.NotNil(test, diagnostic.AccountMapping)
		require.Equal(test, "mapped", diagnostic.AccountMapping.Status)
		require.True(test, diagnostic.AccountMapping.CachePartitioned)
		encoded, err := json.Marshal(diagnostic)
		require.NoError(test, err)
		require.Contains(test, string(encoded), accountIdentitySampleRoot)
		require.Contains(test, string(encoded), session)
		require.NotContains(test, string(encoded), "test-access-token")
		require.NotContains(test, string(encoded), "test-key")
	}
}

func TestCodexAccountIdentityExampleVector(test *testing.T) {
	mapping := &codexAccountIdentity{secret: bytes.Repeat([]byte{0x42}, 32), owner: codexIdentityDigest("codex-owner-v1", "preview-user"), account: accountIdentitySampleAccount, aliases: make(map[string]string)}
	for original, expected := range map[string]string{
		accountIdentitySampleRoot:    "01a09302-49f4-7b53-b545-91e6c3cf3214",
		accountIdentitySampleContext: "01a09302-49f4-7b53-b545-91f488aedd9a",
	} {
		alias := original[:27] + mapping.digest("identity", original)[:9]
		require.Equal(test, expected, alias)
		require.Len(test, alias, 36)
		require.True(test, strings.HasPrefix(alias, original[:27]))
		test.Logf("preview %s -> %s", original, alias)
	}
}

func TestCodexAccountIdentityVerifiedOwnerAndPreservedParent(test *testing.T) {
	test.Setenv("CODEX_OUTBOUND_SESSION_MODE", "account")
	db, err := database.New("sqlite", filepath.Join(test.TempDir(), "owners.db"))
	require.NoError(test, err)
	test.Cleanup(func() { require.NoError(test, db.Close()) })
	handler := &Handler{db: db}
	first := codexOwnerTestContext(test, handler, "first", true)
	same := codexOwnerTestContext(test, handler, "first", true)
	other := codexOwnerTestContext(test, handler, "other", true)
	account := &auth.Account{DBID: 1695, AccountID: accountIdentitySampleAccount}
	headers, body := accountIdentityFixture(test, false, true)
	fingerprint := NewCodexTransportFingerprint(account, headers, body, "cache")
	require.NoError(test, fingerprint.ClaimSessionIdentity(first.Request.Context(), account, "token-first"))
	rotated := NewCodexTransportFingerprint(account, headers, body, "cache")
	require.NoError(test, rotated.ClaimSessionIdentity(same.Request.Context(), account, "token-rotated"))
	require.Equal(test, fingerprint.ApplyBody(body), rotated.ApplyBody(body))
	conflict := NewCodexTransportFingerprint(account, headers, body, "cache")
	require.Error(test, conflict.ClaimSessionIdentity(other.Request.Context(), account, "token-other"))

	parent := "01a08303-49f4-7b53-b545-920f29610317"
	owner := codexIdentityDigest("codex-owner-v1", verifiedTransportUser(first.Request.Context()))
	require.NoError(test, db.ClaimCodexIdentities(context.Background(), []string{codexIdentityDigest("codex-session-v1", "account:1695", parent)}, owner))
	childBody, err := sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata.parent_thread_id", parent)
	require.NoError(test, err)
	child := NewCodexTransportFingerprint(account, headers, childBody, "cache")
	require.Error(test, child.ClaimSessionIdentity(first.Request.Context(), account, "token-first"))
}

func TestCodexAccountIdentityAnonymousContextStaysStable(test *testing.T) {
	test.Setenv("CODEX_OUTBOUND_SESSION_MODE", "account")
	db, err := database.New("sqlite", filepath.Join(test.TempDir(), "anonymous.db"))
	require.NoError(test, err)
	test.Cleanup(func() { require.NoError(test, db.Close()) })
	ctx := WithCodexIdentityStore(context.Background(), db)
	headers, body := accountIdentityFixture(test, false, true)
	account := &auth.Account{DBID: 1695, AccountID: accountIdentitySampleAccount}
	first := NewCodexTransportFingerprint(account, headers, body, "cache")
	require.NoError(test, first.ClaimSessionIdentity(ctx, account, ""))
	require.NoError(test, first.ClaimSessionIdentity(ctx, account, ""))
	second := NewCodexTransportFingerprint(account, headers, body, "cache")
	require.NoError(test, second.ClaimSessionIdentity(WithCodexIdentityStore(ctx, db), account, ""))
	require.Equal(test, first.ApplyBody(body), second.ApplyBody(body))
}
