package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
)

func TestSignedCYCreatesSeparatedPersonKeyNetworkSessionAndAccountProfiles(t *testing.T) {
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "signed-cy-risk.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer db.Close()
	cfg := promptGuardTestConfig()
	cfg.Advanced.NewAPI.Enabled = true
	cfg.Advanced.NewAPI.MaxClockSkewSeconds = 300
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1})
	store.SetPromptFilterConfig(cfg)
	store.ReplacePromptFilterNewAPIBindings([]*database.PromptFilterNewAPIBinding{{
		APIKeyID: 101, PlatformCode: "gateway-a", PlatformName: "示例平台平台", Secret: "gateway-a-secret", Enabled: true, RequireSignedIdentity: true,
	}})
	handler := NewHandler(store, db, nil, nil)
	handler.SetRuntimeCache(cache.NewMemory(1))
	body := []byte(`{"model":"gpt-5.6-sol","input":"ordinary request"}`)
	ctx := signedBoundNewAPIPolicyContext(t, "risk-cy-request", newAPIIdentity{UserID: "operator-42", ClientIP: "203.0.113.9"}, body, 101, "gateway-a", "gateway-a-secret", "0123456789abcdef0123456789abcdef")
	ctx.Set(contextAPIKeyName, "tenant-key")
	ctx.Set(contextAPIKeyMasked, "sk-***risk")
	setIngressRequestBodyIfAbsent(ctx, body)
	evaluation := handler.evaluatePromptGuard(ctx, body, body, "/v1/responses", "gpt-5.6-sol", promptfilter.TransportHTTP)
	if evaluation.Verdict.Action != promptfilter.ActionAllow {
		t.Fatalf("test precondition expected allow, got %+v", evaluation.Verdict)
	}
	incidentID, accepted := handler.logUpstreamCyberPolicy(ctx, "/v1/responses", "gpt-5.6-sol", []byte(`{"error":{"code":"cyber_policy"}}`), upstreamCyberPolicyAttempt{
		Transport: "sse", StatusCode: 500, AccountID: 73, AttemptIndex: 1,
	})
	if !accepted || incidentID == "" {
		t.Fatalf("incident enqueue accepted=%t id=%q", accepted, incidentID)
	}
	waitPromptFilterAuditIdle(t, db)

	incident, err := db.GetPromptPolicyIncident(context.Background(), incidentID)
	if err != nil {
		t.Fatalf("GetPromptPolicyIncident: %v", err)
	}
	if incident.NewAPIPolicyStatus != "verified" || incident.NewAPIPlatform != "gateway-a" || incident.NewAPIUserID != "operator-42" || incident.APIKeyID != 101 || incident.AccountID != 73 || incident.SessionHash == "" || incident.ClientIPHash == "" {
		t.Fatalf("incident identity/routing snapshot = %#v", incident)
	}
	profiles, total, err := db.ListPromptRiskProfiles(context.Background(), database.PromptRiskProfileQuery{Page: 1, PageSize: 20})
	if err != nil || total != 5 || len(profiles) != 5 {
		t.Fatalf("profiles total=%d items=%#v err=%v", total, profiles, err)
	}
	seen := map[string]*database.PromptRiskProfile{}
	for _, profile := range profiles {
		seen[profile.SubjectType] = profile
		for _, action := range profile.RecommendedActions {
			if action == "block" || action == "ban" {
				t.Fatalf("historical profile emitted automatic enforcement action: %#v", profile)
			}
		}
	}
	if seen[database.PromptRiskSubjectNewAPIUser] == nil || !seen[database.PromptRiskSubjectNewAPIUser].IsPerson || seen[database.PromptRiskSubjectNewAPIUser].IdentityConfidence != 100 {
		t.Fatalf("trusted person profile missing: %#v", seen)
	}
	for _, subjectType := range []string{database.PromptRiskSubjectSession, database.PromptRiskSubjectAPIKey, database.PromptRiskSubjectClientIP, database.PromptRiskSubjectUpstreamAccount} {
		if seen[subjectType] == nil || seen[subjectType].IsPerson {
			t.Fatalf("non-person profile boundary failed for %s: %#v", subjectType, seen[subjectType])
		}
	}
}

func TestPromptPolicyRoutingSnapshotNormalizesDefaultCodexAccount(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{})
	t.Cleanup(store.Stop)
	account := &auth.Account{DBID: 73, Email: "codex-account@example.com", AccessToken: "test-token"}
	store.AddAccount(account)
	store.SetGroupName(4, "打铁")
	store.ApplyAccountGroups(account.DBID, []int64{4})
	store.SetAPIKeyAllowedGroups(9, []int64{4})
	handler := NewHandler(store, nil, nil, nil)

	snapshot := handler.capturePromptPolicyRoutingSnapshot(account.DBID, 9)
	if snapshot.AccountName != account.Email || snapshot.AccountPlatform != database.UpstreamChannelCodex || len(snapshot.AccountGroupNames) != 1 || len(snapshot.APIKeyAllowedGroupNames) != 1 {
		t.Fatalf("routing snapshot = %#v", snapshot)
	}
}

func TestCapturePromptFilterAuditContextIncludesSignedChannelID(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{})
	defer store.Stop()
	store.ReplacePromptFilterNewAPIBindings([]*database.PromptFilterNewAPIBinding{{
		APIKeyID: 101, PlatformCode: "gateway-a", Secret: "gateway-a-secret", Enabled: true,
	}})
	handler := NewHandler(store, nil, nil, nil)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	c.Set(contextAPIKeyID, int64(101))
	c.Set(newAPIPolicyMetaContextKey, verifiedNewAPIPolicyContext{
		APIKeyID: 101, Platform: "gateway-a", Identity: newAPIIdentity{UserID: "user-42", ClientIP: "203.0.113.8", RequestID: "req-42"},
		MetaVerified: true, Meta: newAPIPolicyMeta{ChannelID: 2002, SessionFingerprint: "0123456789abcdef0123456789abcdef"},
	})
	audit := handler.capturePromptFilterAuditContext(c)
	if audit.NewAPIChannelID != 2002 {
		t.Fatalf("signed NewAPI channel id = %d, want 2002", audit.NewAPIChannelID)
	}
}
