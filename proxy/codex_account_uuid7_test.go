package proxy

import (
	"database/sql"
	"encoding/hex"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestCodexAccountUUIDv7NewForkRetainsLegacyParentAndRootTurn(test *testing.T) {
	test.Setenv("CODEX_OUTBOUND_SESSION_MODE", "account")
	path := filepath.Join(test.TempDir(), "legacy-suffix.db")
	db, err := database.New("sqlite", path)
	require.NoError(test, err)
	test.Cleanup(func() { require.NoError(test, db.Close()) })
	account := &auth.Account{DBID: 1695, AccountID: accountIdentitySampleAccount}
	owner := codexIdentityDigest("codex-owner-v1", "credential:test-user-key")
	var secret string
	for _, original := range []string{accountIdentitySampleRoot, accountIdentitySampleContext} {
		policy, err := db.ResolveCodexIdentityMapping(test.Context(), codexIdentityDigest("codex-account-root-v1", owner, account.AccountID, original), nil, true)
		require.NoError(test, err)
		secret = policy.Secret
	}
	connection, err := sql.Open("sqlite", path)
	require.NoError(test, err)
	_, err = connection.ExecContext(test.Context(), `UPDATE codex_identity_mapping_policies SET mode='account-suffix-v1'`)
	require.NoError(test, err)
	require.NoError(test, connection.Close())
	headers, body := accountIdentityFixture(test, false, true)
	headers, body = setTurnIdentityTestFields(test, headers, body, turnIdentitySample, turnIdentitySample, false)
	ctx := WithCodexIdentityStore(test.Context(), db)
	parent := NewCodexTransportFingerprint(account, headers, body, "cache")
	require.NoError(test, parent.ClaimSessionIdentity(ctx, account, "test-user-key"))
	parentBody := parent.ApplyBody(body)
	parentSession := gjson.GetBytes(parentBody, "client_metadata.session_id").String()
	parentTurn, _ := requireTurnIdentityOutput(test, parent, body)
	decoded, err := hex.DecodeString(secret)
	require.NoError(test, err)
	legacy := codexAccountIdentity{secret: decoded, owner: owner, account: account.AccountID}
	require.Equal(test, accountIdentitySampleRoot[:27]+legacy.digest("identity", accountIdentitySampleRoot)[:9], parentSession)
	require.Equal(test, turnIdentitySample[:27]+legacy.digest("turn", turnIdentitySample)[:9], parentTurn)
	require.NoError(test, db.Close())
	db, err = database.New("sqlite", path)
	require.NoError(test, err)
	ctx = WithCodexIdentityStore(test.Context(), db)
	resumed := NewCodexTransportFingerprint(account, headers, body, "cache")
	require.NoError(test, resumed.ClaimSessionIdentity(ctx, account, "test-user-key"))
	require.Equal(test, parentBody, resumed.ApplyBody(body))
	childBody := []byte(strings.ReplaceAll(string(body), accountIdentitySampleRoot, promptLockForkSession))
	for _, path := range []string{"client_metadata.parent_thread_id", "client_metadata.x-codex-turn-metadata.parent_thread_id", "client_metadata.x-codex-turn-metadata.forked_from_thread_id"} {
		childBody, err = sjson.SetBytes(childBody, path, accountIdentitySampleRoot)
		require.NoError(test, err)
	}
	childHeaders := nativeSessionHeaders(promptLockForkSession, promptLockForkSession, 0)
	childHeaders, childBody = setTurnIdentityTestFields(test, childHeaders, childBody, turnIdentityChildSample, turnIdentitySample, false)
	child := NewCodexTransportFingerprint(account, CodexRequestMetadataHeaders(childHeaders, childBody), childBody, "cache")
	require.NoError(test, child.ClaimSessionIdentity(ctx, account, "test-user-key"))
	childOutbound := child.ApplyBody(childBody)
	childSession := gjson.GetBytes(childOutbound, "client_metadata.session_id").String()
	require.NotEqual(test, promptLockForkSession[:13], childSession[:13])
	require.Equal(test, parentSession, gjson.GetBytes(childOutbound, "client_metadata.parent_thread_id").String())
	childTurn, childRootTurn := requireTurnIdentityOutput(test, child, childBody)
	require.Equal(test, parentTurn, childRootTurn)
	require.NotEqual(test, turnIdentityChildSample[:13], childTurn[:13])
	require.Equal(test, database.CodexIdentityMappingUUIDv7, child.accountIdentityDiagnostic.Version)
	for _, change := range child.accountIdentityDiagnostic.Changes {
		if change.Original == turnIdentitySample || change.Original == accountIdentitySampleRoot || change.Original == accountIdentitySampleContext {
			require.Equal(test, "account-suffix-v1", change.Version)
			require.Nil(test, change.MappedAt)
		} else {
			require.Equal(test, database.CodexIdentityMappingUUIDv7, change.Version)
			require.NotNil(test, change.MappedAt)
			parsed, err := uuid.Parse(change.Outbound)
			require.NoError(test, err)
			seconds, nanoseconds := parsed.Time().UnixTime()
			require.Equal(test, time.Unix(seconds, nanoseconds).UTC(), *change.MappedAt)
		}
	}
	outboundHeaders := http.Header{}
	child.ApplySessionHeaders(outboundHeaders)
	require.Equal(test, childSession, outboundHeaders.Get("Session-Id"))
	require.Equal(test, promptLockForkSession, gjson.GetBytes(childBody, "client_metadata.session_id").String())
}
