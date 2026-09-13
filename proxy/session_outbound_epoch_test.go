package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func outboundEpochTestRequest(test *testing.T, handler *Handler, number uint64) (*gin.Context, []byte) {
	test.Helper()
	request, body := continuityTestRequest(number, "turn")
	body, _ = sjson.SetBytes(body, "input", "Full plaintext conversation")
	body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata.window_number", number)
	body, _ = sjson.SetBytes(body, "client_metadata.session_id", continuityTestThread)
	body, _ = sjson.SetBytes(body, "client_metadata.thread_id", continuityTestThread)
	body, _ = sjson.SetBytes(body, "client_metadata.x-codex-window-id", fmt.Sprintf("%s:%d", continuityTestThread, number))
	request.Request.Header.Set("Authorization", "Bearer test-user-key")
	handler.bindCodexIdentityClaims(request)
	request.Set(ingressRequestBodyContextKey, body)
	return request, body
}

func TestSessionAccountFailoverCapacitySwitchIsOptional(test *testing.T) {
	for _, enabled := range []bool{false, true} {
		test.Run(fmt.Sprint(enabled), func(test *testing.T) {
			handler, owner, target, key := failoverTestSetup(test, enabled)
			owner.SessionCapacityEnabled, owner.SessionCapacityMax = true, 1
			require.True(test, handler.store.AdmitAccountSession(owner, "occupied-root", time.Now()))
			request, body := failoverTestRequest(test, handler)
			failure := handler.configureSessionModelAffinity(request, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body)
			if !enabled {
				require.NotNil(test, failure)
				require.Contains(test, string(failure.Code), "capacity")
				return
			}
			require.Nil(test, failure)
			selected, _, handled := handler.takeSessionAccountFailover(request.Request.Context(), key, 0, nil, nil, auth.DispatchPolicyStandard)
			require.True(test, handled)
			require.Same(test, target, selected)
			handler.store.Release(selected)
			require.Equal(test, "account_session_capacity_full", continuityRequest(request).Diagnostic.AccountFailover.Reason)
		})
	}
}

func TestSessionAccountFailoverOutboundWindowsAndReturnToAccount(test *testing.T) {
	handler, first, second, key := failoverTestSetup(test, true)
	_, err := handler.db.CommitSessionContinuity(context.Background(), hashRiskIdentity(key), database.SessionContinuityRecord{AccountID: first.ID(), ThreadID: continuityTestThread, NumberKnown: true, Number: 47})
	require.NoError(test, err)
	previousResin := GetResinConfig()
	test.Cleanup(func() { SetResinConfig(previousResin) })
	type capture struct {
		headers http.Header
		body    []byte
	}
	seen := make(chan capture, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		seen <- capture{request.Header.Clone(), readUpstreamRequestBody(request)}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"id":"response-test"}`))
	}))
	test.Cleanup(server.Close)
	SetResinConfig(&ResinConfig{BaseURL: server.URL, PlatformName: "epoch-test"})
	var sessions []string
	for index, step := range []struct {
		inbound, outbound uint64
		account           *auth.Account
		migrate, compact  bool
	}{
		{47, 47, first, false, false}, {47, 0, second, true, false}, {48, 1, second, false, true}, {49, 0, first, true, false}, {50, 1, first, false, false},
	} {
		if step.migrate {
			atomic.StoreInt32(&first.Disabled, 1)
			atomic.StoreInt32(&second.Disabled, 1)
			atomic.StoreInt32(&step.account.Disabled, 0)
		}
		request, body := outboundEpochTestRequest(test, handler, step.inbound)
		original := bytes.Clone(body)
		identity := requestSessionIdentity{stableIdentity: true}
		require.Nil(test, handler.configureSessionModelAffinity(request, identity, key, "gpt-5.6-sol", "gpt-5.6-sol", step.compact, body))
		if step.migrate {
			selected, _, handled := handler.takeSessionAccountFailover(request.Request.Context(), key, 0, nil, nil, auth.DispatchPolicyStandard)
			require.True(test, handled)
			require.Same(test, step.account, selected)
			handler.store.Release(selected)
		}
		require.Nil(test, handler.commitSessionContinuity(request, step.account))
		var response *http.Response
		if step.compact {
			response, err = ExecuteCompactRequest(request.Request.Context(), step.account, body, "cache", "", "test-user-key", nil, request.Request.Header)
		} else {
			response, err = ExecuteRequest(request.Request.Context(), step.account, body, "cache", "", "test-user-key", nil, request.Request.Header, false)
		}
		require.NoError(test, err)
		_, err = io.Copy(io.Discard, response.Body)
		require.NoError(test, err)
		require.NoError(test, response.Body.Close())
		sent := <-seen
		session := sent.headers.Get("Session-Id")
		sessions = append(sessions, session)
		meta := diagnosticMetadataObject(gjson.GetBytes(sent.body, "client_metadata.x-codex-turn-metadata"))
		require.Equal(test, step.outbound, meta.Get("window_number").Uint())
		require.Equal(test, fmt.Sprintf("%s:%d", session, step.outbound), meta.Get("window_id").String())
		require.Equal(test, meta.Get("window_id").String(), sent.headers.Get("X-Codex-Window-Id"))
		require.Equal(test, meta.Get("window_id").String(), gjson.GetBytes(sent.body, "client_metadata.x-codex-window-id").String())
		require.Equal(test, session, meta.Get("session_id").String())
		require.Equal(test, original, body)
		require.Equal(test, step.inbound, continuityRequest(request).Number)
		if index == 2 {
			settings := CurrentRuntimeSettings()
			settings.CodexSessionFailoverEnabled = false
			ApplyRuntimeSettings(settings)
			resumed, raw := outboundEpochTestRequest(test, handler, 48)
			require.Nil(test, handler.configureSessionModelAffinity(resumed, identity, key, "gpt-5.6-sol", "gpt-5.6-sol", false, raw))
			fingerprint := NewCodexTransportFingerprint(second, nil, raw, "cache")
			require.NoError(test, fingerprint.ClaimSessionIdentity(resumed.Request.Context(), second, "test-user-key"))
			require.Equal(test, session, fingerprint.DownstreamHeaders().Get("Session-Id"))
			require.Equal(test, session+":1", fingerprint.DownstreamHeaders().Get("X-Codex-Window-Id"))
			settings.CodexSessionFailoverEnabled = true
			ApplyRuntimeSettings(settings)
		}
	}
	require.Equal(test, sessions[1], sessions[2])
	require.Equal(test, sessions[3], sessions[4])
	require.NotEqual(test, sessions[0], sessions[3])
	require.NotEqual(test, sessions[1], sessions[3])
	record, found, err := handler.db.ReadSessionContinuity(context.Background(), hashRiskIdentity(key))
	require.NoError(test, err)
	require.True(test, found)
	require.EqualValues(test, 2, record.FailoverCount)
	require.EqualValues(test, 49, record.OutboundWindowBases[continuityTestThread])
	oldRequest, oldBody := outboundEpochTestRequest(test, handler, 48)
	handler.attachSessionOutboundEpoch(oldRequest, hashRiskIdentity(key), record)
	fingerprint := NewCodexTransportFingerprint(first, nil, oldBody, "cache")
	require.NoError(test, fingerprint.ClaimSessionIdentity(oldRequest.Request.Context(), first, "test-user-key"))
	require.Equal(test, sessions[3]+":2", fingerprint.DownstreamHeaders().Get("X-Codex-Window-Id"))
}

func TestSessionAccountFailoverRelatedWindowAndContinuationEpoch(test *testing.T) {
	handler, owner, target, key := failoverTestSetup(test, true)
	record, _, err := handler.db.SwitchSessionContinuityAccount(context.Background(), database.SessionAccountFailover{RootKey: hashRiskIdentity(key), ExpectedAccountID: owner.ID(), AccountID: target.ID(), ResetOutboundWindow: true, WindowThreadID: continuityTestThread, WindowNumber: 47})
	require.NoError(test, err)
	handler.store.UnbindSessionAffinity(key, owner.ID())
	handler.store.BindSessionAffinity(key, target, "")
	const child = "01a03bb0-9da5-7772-a16a-f38258dd30c5"
	var firstChild string
	for number := uint64(3); number <= 4; number++ {
		request, body := outboundEpochTestRequest(test, handler, number)
		body, _ = sjson.SetBytes(body, "client_metadata.thread_id", child)
		body, _ = sjson.SetBytes(body, "client_metadata.x-codex-window-id", fmt.Sprintf("%s:%d", child, number))
		body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata.thread_id", child)
		body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata.window_id", fmt.Sprintf("%s:%d", child, number))
		body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata.parent_thread_id", continuityTestThread)
		body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata.thread_source", "guardian_review")
		require.Nil(test, handler.prepareBackgroundAccountMatch(request, key, body))
		fingerprint := NewCodexTransportFingerprint(target, nil, body, "cache")
		require.NoError(test, fingerprint.ClaimSessionIdentity(request.Request.Context(), target, "test-user-key"))
		mapped := fingerprint.ApplyBody(body)
		meta := diagnosticMetadataObject(gjson.GetBytes(mapped, "client_metadata.x-codex-turn-metadata"))
		require.Equal(test, number-3, meta.Get("window_number").Uint())
		require.Equal(test, meta.Get("session_id").String(), meta.Get("parent_thread_id").String())
		require.NotEqual(test, meta.Get("thread_id").String(), meta.Get("session_id").String())
		require.Equal(test, mapped, fingerprint.ApplyBody(mapped))
		if number == 3 {
			firstChild = meta.Get("thread_id").String()
		} else {
			require.Equal(test, firstChild, meta.Get("thread_id").String())
		}
	}
	request, body := outboundEpochTestRequest(test, handler, 47)
	handler.attachSessionOutboundEpoch(request, hashRiskIdentity(key), record)
	cacheOwner := responseCacheOwnerForRequest(request, requestAPIKeyID(request))
	handler.recordResponseAccountAffinity(cacheOwner, "old-response", target.ID(), key, "gpt-5.6-sol", "codex")
	handler.recordResponseAccountAffinity(cacheOwner, "new-response", target.ID(), key, "gpt-5.6-sol", "codex", request.Request.Context())
	body, _ = sjson.SetBytes(body, "previous_response_id", "old-response")
	require.NotNil(test, handler.validateMigratedSessionContext(request, body, record, key))
	body, _ = sjson.SetBytes(body, "previous_response_id", "new-response")
	require.Nil(test, handler.validateMigratedSessionContext(request, body, record, key))
	record.FailoverCount += 2
	require.NotNil(test, handler.validateMigratedSessionContext(request, body, record, key))
}

func TestSessionAccountFailoverFlatWindowNumberUsesEmbeddedThread(test *testing.T) {
	handler, owner, target, key := failoverTestSetup(test, true)
	record, _, err := handler.db.SwitchSessionContinuityAccount(context.Background(), database.SessionAccountFailover{RootKey: hashRiskIdentity(key), ExpectedAccountID: owner.ID(), AccountID: target.ID(), ResetOutboundWindow: true, WindowThreadID: continuityTestThread, WindowNumber: 47})
	require.NoError(test, err)
	request, body := continuityTestRequest(148, "turn")
	body, _ = sjson.SetBytes(body, "client_metadata.window_number", 148)
	handler.bindCodexIdentityClaims(request)
	handler.attachSessionOutboundEpoch(request, hashRiskIdentity(key), record)
	fingerprint := NewCodexTransportFingerprint(target, nil, body, "cache")
	require.NoError(test, fingerprint.ClaimSessionIdentity(request.Request.Context(), target, "test-user-key"))
	mapped := fingerprint.ApplyBody(body)
	require.EqualValues(test, 1, gjson.GetBytes(mapped, "client_metadata.window_number").Uint())
	require.Equal(test, mapped, fingerprint.ApplyBody(mapped))
	body, _ = sjson.SetBytes(body, "client_metadata.window_number", 149)
	fingerprint = NewCodexTransportFingerprint(target, nil, body, "cache")
	require.Error(test, fingerprint.ClaimSessionIdentity(request.Request.Context(), target, "test-user-key"))
}

func TestSessionAccountFailoverRollbackContextIdentity(test *testing.T) {
	handler, owner, target, key := failoverTestSetup(test, true)
	config := handler.store.GetPromptFilterConfig()
	config.Advanced.Risk.SessionContinuityMode = "enforce"
	handler.store.SetPromptFilterConfig(config)
	_, err := handler.db.CommitSessionContinuity(context.Background(), hashRiskIdentity(key), database.SessionContinuityRecord{AccountID: owner.ID(), ThreadID: continuityTestThread, NumberKnown: true, Number: 16})
	require.NoError(test, err)
	atomic.StoreInt32(&owner.Disabled, 1)
	var mappedSession string
	for index, step := range []struct {
		original, expected uint64
		contextID          string
	}{
		{16, 0, "01a09935-e304-7631-8fa3-ef6da401d261"},
		{15, 1, "01a098f6-a753-7472-878a-b93c55180188"},
		{15, 1, "01a098f6-a753-7472-878a-b93c55180188"},
		{16, 2, "01a0995d-3268-7fb0-9109-b70a663a3302"},
		{16, 0, "01a09935-e304-7631-8fa3-ef6da401d261"},
	} {
		request, body := outboundEpochTestRequest(test, handler, step.original)
		body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata.context_window_id", step.contextID)
		original := bytes.Clone(body)
		require.Nil(test, handler.configureSessionModelAffinity(request, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
		if index == 0 {
			selected, _, handled := handler.takeSessionAccountFailover(request.Request.Context(), key, 0, nil, nil, auth.DispatchPolicyStandard)
			require.True(test, handled)
			require.Same(test, target, selected)
			handler.store.Release(selected)
		}
		require.Nil(test, handler.commitSessionContinuity(request, target))
		fingerprint := NewCodexTransportFingerprint(target, request.Request.Header, body, "cache")
		require.NoError(test, fingerprint.ClaimSessionIdentity(request.Request.Context(), target, "test-user-key"))
		mapped := fingerprint.ApplyBody(body)
		metadata := diagnosticMetadataObject(gjson.GetBytes(mapped, "client_metadata.x-codex-turn-metadata"))
		require.Equal(test, step.expected, metadata.Get("window_number").Uint())
		require.Equal(test, metadata.Get("window_id").String(), fingerprint.DownstreamHeaders().Get("X-Codex-Window-Id"))
		if mappedSession != "" {
			require.Equal(test, mappedSession, metadata.Get("thread_id").String())
		}
		mappedSession = metadata.Get("thread_id").String()
		require.Equal(test, original, body)
		require.False(test, continuityRequest(request).Diagnostic.WouldBlock)
		handler.continuityRecords = nil
	}
}
