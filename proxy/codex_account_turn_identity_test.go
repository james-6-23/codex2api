package proxy

import (
	"bytes"
	"net/http"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const turnIdentitySample = "01a095b5-86a3-7ec2-af42-0bb1111ef330"
const turnIdentityChildSample = "01a095b6-86a3-7ec2-af42-0bb1111ef331"

func setTurnIdentityTestFields(test *testing.T, headers http.Header, body []byte, turn, rootTurn string, asString bool) (http.Header, []byte) {
	test.Helper()
	metadata := diagnosticMetadataObject(gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata")).Raw
	for field, value := range map[string]string{"turn_id": turn, "root_turn_id": rootTurn} {
		var err error
		body, err = sjson.SetBytes(body, "client_metadata."+field, value)
		require.NoError(test, err)
		metadata, err = sjson.Set(metadata, field, value)
		require.NoError(test, err)
	}
	var err error
	if asString {
		body, err = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata", metadata)
	} else {
		body, err = sjson.SetRawBytes(body, "client_metadata.x-codex-turn-metadata", []byte(metadata))
	}
	require.NoError(test, err)
	headers = headers.Clone()
	headers.Set(codexTurnMetadataHeader, metadata)
	return headers, body
}

func requireTurnIdentityOutput(test *testing.T, fingerprint *CodexFingerprint, body []byte) (string, string) {
	test.Helper()
	outbound := fingerprint.ApplyBody(body)
	turn := gjson.GetBytes(outbound, "client_metadata.turn_id").String()
	rootTurn := gjson.GetBytes(outbound, "client_metadata.root_turn_id").String()
	metadata := diagnosticMetadataObject(gjson.GetBytes(outbound, "client_metadata.x-codex-turn-metadata"))
	headers := http.Header{}
	fingerprint.ApplySessionHeaders(headers)
	require.Equal(test, turn, metadata.Get("turn_id").String())
	require.Equal(test, rootTurn, metadata.Get("root_turn_id").String())
	require.Equal(test, turn, gjson.Get(headers.Get(codexTurnMetadataHeader), "turn_id").String())
	require.Equal(test, rootTurn, gjson.Get(headers.Get(codexTurnMetadataHeader), "root_turn_id").String())
	require.Equal(test, outbound, fingerprint.ApplyBody(outbound))
	return turn, rootTurn
}

func TestCodexTurnIdentityRewritesOnlyOutboundMetadata(test *testing.T) {
	test.Setenv("CODEX_OUTBOUND_SESSION_MODE", "account")
	handler := newWindowAuthorizationHandler(test)
	account := &auth.Account{DBID: 1695, AccountID: accountIdentitySampleAccount}
	ctx := WithCodexIdentityStore(test.Context(), handler.db)
	var previousTurn string
	for _, asString := range []bool{false, true} {
		headers, body := accountIdentityFixture(test, false, !asString)
		headers, body = setTurnIdentityTestFields(test, headers, body, turnIdentitySample, turnIdentitySample, asString)
		originalHeaders, originalBody := headers.Clone(), bytes.Clone(body)
		originalRouting := resolveRequestRootSessionIdentity(headers, body)
		fingerprint := NewCodexTransportFingerprint(account, headers, body, "cache")
		require.NoError(test, fingerprint.ClaimSessionIdentity(ctx, account, "test-user-key"))
		turn, rootTurn := requireTurnIdentityOutput(test, fingerprint, body)
		require.NotEqual(test, turnIdentitySample, turn)
		require.NotEqual(test, turnIdentitySample[:13], turn[:13])
		require.Equal(test, turn, rootTurn)
		if previousTurn != "" {
			require.Equal(test, previousTurn, turn)
		}
		previousTurn = turn
		require.Equal(test, originalHeaders, headers)
		require.Equal(test, originalBody, body)
		require.Equal(test, originalRouting, resolveRequestRootSessionIdentity(headers, body))
		require.Equal(test, gjson.GetBytes(body, "input").Raw, gjson.GetBytes(fingerprint.ApplyBody(body), "input").Raw)
		require.Equal(test, "signed-original-do-not-rewrite", headers.Get("X-NewAPI-Meta"))
		var recorded int
		for _, change := range fingerprint.accountIdentityDiagnostic.Changes {
			if change.Original == turnIdentitySample {
				recorded++
				require.Equal(test, turn, change.Outbound)
				require.Equal(test, []string{"turn_id", "root_turn_id"}, change.Fields)
			}
		}
		require.Equal(test, 1, recorded)
	}
}

func TestCodexTurnIdentityRotatesWithAccountEpoch(test *testing.T) {
	handler, owner, target := legacyParentTestSetup(test)
	var turns []string
	current := owner
	for index, account := range []*auth.Account{owner, target, owner} {
		if index > 0 {
			_, _, err := handler.db.SwitchSessionContinuityAccount(test.Context(), database.SessionAccountFailover{RootKey: hashRiskIdentity(accountIdentitySampleRoot), ExpectedAccountID: current.ID(), AccountID: account.ID(), ExpectedGeneration: uint64(index - 1), ResetOutboundWindow: true, WindowThreadID: accountIdentitySampleRoot, WindowNumber: uint64(54 + index)})
			require.NoError(test, err)
		}
		for repeat := 0; repeat < 2; repeat++ {
			request, body := legacyParentTestRequest(test, handler, accountIdentitySampleRoot, "", "turn", uint64(54+index), false)
			headers, body := setTurnIdentityTestFields(test, request.Request.Header, body, turnIdentitySample, turnIdentitySample, repeat == 1)
			fingerprint := NewCodexTransportFingerprint(account, headers, body, "cache")
			require.NoError(test, fingerprint.ClaimSessionIdentity(request.Request.Context(), account, "test-user-key"))
			turn, rootTurn := requireTurnIdentityOutput(test, fingerprint, body)
			require.NotEqual(test, turnIdentitySample, turn)
			require.Equal(test, turn, rootTurn)
			if repeat == 0 {
				turns = append(turns, turn)
			} else {
				require.Equal(test, turns[index], turn)
			}
		}
		current = account
	}
	require.NotEqual(test, turns[0], turns[1])
	require.NotEqual(test, turns[0], turns[2])
	require.NotEqual(test, turns[1], turns[2])
}

func TestCodexRootTurnIdentitySharesPublishedParentEpoch(test *testing.T) {
	handler, owner, target := legacyParentTestSetup(test)
	current := owner
	for index, account := range []*auth.Account{target, owner} {
		_, _, err := handler.db.SwitchSessionContinuityAccount(test.Context(), database.SessionAccountFailover{RootKey: hashRiskIdentity(continuityTestThread), ExpectedAccountID: current.ID(), AccountID: account.ID(), ExpectedGeneration: uint64(index), ResetOutboundWindow: true, WindowThreadID: continuityTestThread, WindowNumber: uint64(55 + index)})
		require.NoError(test, err)
		current = account
	}
	request, body := legacyParentTestRequest(test, handler, continuityTestThread, "", "turn", 56, false)
	headers, body := setTurnIdentityTestFields(test, request.Request.Header, body, turnIdentitySample, turnIdentitySample, false)
	parent := NewCodexTransportFingerprint(owner, headers, body, "cache")
	require.NoError(test, parent.ClaimSessionIdentity(request.Request.Context(), owner, "test-user-key"))
	parentTurn, _ := requireTurnIdentityOutput(test, parent, body)
	for _, kind := range []string{"turn", "compaction", "thread_description", "guardian_review"} {
		childRequest, childBody := legacyParentTestRequest(test, handler, accountIdentitySampleRoot, continuityTestThread, kind, 54, true)
		childHeaders, childBody := setTurnIdentityTestFields(test, childRequest.Request.Header, childBody, turnIdentityChildSample, turnIdentitySample, true)
		child := NewCodexTransportFingerprint(owner, childHeaders, childBody, "cache")
		require.NoError(test, child.ClaimSessionIdentity(childRequest.Request.Context(), owner, "test-user-key"))
		childTurn, childRootTurn := requireTurnIdentityOutput(test, child, childBody)
		require.Equal(test, parentTurn, childRootTurn)
		require.NotEqual(test, turnIdentityChildSample, childTurn)
		require.NotEqual(test, childTurn, childRootTurn)
	}
}
