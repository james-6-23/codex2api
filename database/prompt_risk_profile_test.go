package database

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

func findPromptRiskProfile(items []*PromptRiskProfile, subjectType string) *PromptRiskProfile {
	for _, item := range items {
		if item.SubjectType == subjectType {
			return item
		}
	}
	return nil
}

func TestPromptRiskProfilesSeparatePersonTenantNetworkAndAccount(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	logInput := &PromptFilterLogInput{
		Source: "local_filter", Endpoint: "/v1/responses", Model: "gpt-5.6-sol", Action: "block", Score: 95,
		AuditScore: 95, ReasonCode: "terminal_category", StrikeEligible: true, MatchedPatterns: `[{"name":"terminal","weight":95}]`,
		TextPreview: "redacted prompt", APIKeyID: 9, APIKeyName: "tenant-key", APIKeyMasked: "sk-***9",
		NewAPIPolicyStatus: "signed_response", NewAPIPlatform: "newapi-prod", NewAPIUserID: "user-42",
		NewAPIRequestID: "req-42", NewAPIDecisionID: "dec-42", SessionHash: promptRiskHash("session", "session-42"),
		ClientIPHash: promptRiskHash("client-ip", "203.0.113.8"), RequestCorrelationID: "corr-42",
	}
	if err := db.InsertPromptFilterLog(ctx, logInput); err != nil {
		t.Fatalf("InsertPromptFilterLog: %v", err)
	}
	incident, candidate, evidence := promptPolicyTestInputs("risk-incident-42")
	incident.LocalComparison = PromptPolicyComparisonConfirmedMiss
	incident.NewAPIPolicyStatus = "verified"
	incident.NewAPIPlatform = "newapi-prod"
	incident.NewAPIUserID = "user-42"
	incident.NewAPIUserName = "示例平台用户"
	incident.NewAPIUserEmail = "gateway-a-user@example.com"
	incident.NewAPIUserGroup = "gateway-a-paid"
	incident.NewAPIRequestID = "req-cy-42"
	incident.SessionHash = logInput.SessionHash
	incident.ClientIPHash = logInput.ClientIPHash
	if err := db.PersistPromptPolicyIncident(ctx, incident, candidate, evidence); err != nil {
		t.Fatalf("PersistPromptPolicyIncident: %v", err)
	}
	profiles, total, err := db.ListPromptRiskProfiles(ctx, PromptRiskProfileQuery{Page: 1, PageSize: 20})
	if err != nil {
		t.Fatalf("ListPromptRiskProfiles: %v", err)
	}
	if total != 5 {
		t.Fatalf("profile total = %d, want 5: %#v", total, profiles)
	}
	user := findPromptRiskProfile(profiles, PromptRiskSubjectNewAPIUser)
	if user == nil || !user.IsPerson || user.IdentityConfidence != 100 || user.EventCount != 2 || user.ConfirmedMissCount != 1 || user.RiskScore < 60 {
		t.Fatalf("signed user profile = %#v", user)
	}
	if user.SubjectDisplay != "示例平台用户" || user.NewAPIUserID != "user-42" || user.NewAPIUserEmail != "gateway-a-user@example.com" || user.NewAPIUserGroup != "gateway-a-paid" {
		t.Fatalf("signed user identity = %#v", user)
	}
	key := findPromptRiskProfile(profiles, PromptRiskSubjectAPIKey)
	if key == nil || key.IsPerson || key.IdentityConfidence != 60 || key.APIKeyID != 9 {
		t.Fatalf("API key profile = %#v", key)
	}
	account := findPromptRiskProfile(profiles, PromptRiskSubjectUpstreamAccount)
	if account == nil || account.IsPerson || account.IdentityConfidence != 25 || account.AccountID != 7 {
		t.Fatalf("account profile = %#v", account)
	}
	if findPromptRiskProfile(profiles, PromptRiskSubjectClientIP) == nil || findPromptRiskProfile(profiles, PromptRiskSubjectSession) == nil {
		t.Fatalf("network/session profiles missing: %#v", profiles)
	}
	events, eventTotal, err := db.ListPromptRiskEvents(ctx, user.SubjectType, user.SubjectKey, PromptRiskEventQuery{Page: 1, PageSize: 1})
	if err != nil || eventTotal != 2 || len(events) != 1 {
		t.Fatalf("user events total=%d items=%#v err=%v", eventTotal, events, err)
	}
	if events[0].NewAPIUserName != "示例平台用户" || events[0].NewAPIUserID != "user-42" {
		t.Fatalf("event identity = %#v", events[0])
	}
	filtered, filteredTotal, err := db.ListPromptRiskProfiles(ctx, PromptRiskProfileQuery{Page: 1, PageSize: 20, Query: "gateway-a-user@example.com"})
	if err != nil || filteredTotal != 1 || len(filtered) != 1 || filtered[0].SubjectKey != user.SubjectKey {
		t.Fatalf("identity search total=%d items=%#v err=%v", filteredTotal, filtered, err)
	}
}

func TestPromptRiskIdentityDirectoryEnrichesHistoricalProfiles(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	if err := db.InsertPromptFilterLog(ctx, &PromptFilterLogInput{
		Source: "local_filter", Action: "block", StrikeEligible: true, MatchedPatterns: `[{"name":"x"}]`,
		NewAPIPolicyStatus: "verified", NewAPIPlatform: "gateway-a", NewAPIUserID: "73",
	}); err != nil {
		t.Fatalf("InsertPromptFilterLog: %v", err)
	}
	if err := db.UpsertPromptRiskIdentities(ctx, []PromptRiskIdentityInput{{
		Platform: "gateway-a", ExternalUserID: "73", UserName: "known-user", UserEmail: "known@example.com", UserGroup: "vip", Source: "admin_import",
	}}); err != nil {
		t.Fatalf("UpsertPromptRiskIdentities: %v", err)
	}
	profiles, total, err := db.ListPromptRiskProfiles(ctx, PromptRiskProfileQuery{Page: 1, PageSize: 20, SubjectType: PromptRiskSubjectNewAPIUser, Query: "vip"})
	if err != nil || total != 1 || len(profiles) != 1 {
		t.Fatalf("profiles total=%d items=%#v err=%v", total, profiles, err)
	}
	profile := profiles[0]
	if profile.SubjectDisplay != "known-user" || profile.NewAPIUserID != "73" || profile.NewAPIUserEmail != "known@example.com" || profile.NewAPIUserGroup != "vip" {
		t.Fatalf("historical profile identity = %#v", profile)
	}
}

func TestPromptRiskIdentityDirectoryListsPeopleBeforeFirstRiskEvent(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	if err := db.UpsertPromptRiskIdentities(ctx, []PromptRiskIdentityInput{
		{Platform: "gateway-a", ExternalUserID: "101", UserName: "new-user", UserEmail: "new@example.com", UserGroup: "standard", Source: "admin_import"},
		{Platform: "gateway-a", ExternalUserID: "102", UserName: "active-user", Source: "admin_import"},
	}); err != nil {
		t.Fatalf("UpsertPromptRiskIdentities: %v", err)
	}
	if err := db.InsertPromptFilterLog(ctx, &PromptFilterLogInput{
		Source: "local_filter", Action: "warn", Score: 40, MatchedPatterns: `[{"name":"x"}]`,
		NewAPIPolicyStatus: "verified", NewAPIPlatform: "gateway-a", NewAPIUserID: "102",
	}); err != nil {
		t.Fatalf("InsertPromptFilterLog: %v", err)
	}

	profiles, total, err := db.ListPromptRiskProfiles(ctx, PromptRiskProfileQuery{Page: 1, PageSize: 20, SubjectType: PromptRiskSubjectNewAPIUser})
	if err != nil || total != 2 || len(profiles) != 2 {
		t.Fatalf("profiles total=%d items=%#v err=%v", total, profiles, err)
	}
	var identityOnly, active *PromptRiskProfile
	for _, profile := range profiles {
		switch profile.NewAPIUserID {
		case "101":
			identityOnly = profile
		case "102":
			active = profile
		}
	}
	if identityOnly == nil || identityOnly.HasActivity || identityOnly.EventCount != 0 || identityOnly.RiskScore != 0 || identityOnly.IdentitySource != "admin_import" || identityOnly.IdentityUpdatedAt == nil {
		t.Fatalf("identity-only profile = %#v", identityOnly)
	}
	if active == nil || !active.HasActivity || active.EventCount != 1 {
		t.Fatalf("active profile = %#v", active)
	}
	filtered, filteredTotal, err := db.ListPromptRiskProfiles(ctx, PromptRiskProfileQuery{Page: 1, PageSize: 20, SubjectType: PromptRiskSubjectNewAPIUser, Query: "new@example.com"})
	if err != nil || filteredTotal != 1 || len(filtered) != 1 || filtered[0].NewAPIUserID != "101" {
		t.Fatalf("identity-only search total=%d items=%#v err=%v", filteredTotal, filtered, err)
	}
	minimumRisk, minimumRiskTotal, err := db.ListPromptRiskProfiles(ctx, PromptRiskProfileQuery{Page: 1, PageSize: 20, SubjectType: PromptRiskSubjectNewAPIUser, MinScore: 1})
	if err != nil || minimumRiskTotal != 1 || len(minimumRisk) != 1 || minimumRisk[0].NewAPIUserID != "102" {
		t.Fatalf("min-risk profiles total=%d items=%#v err=%v", minimumRiskTotal, minimumRisk, err)
	}
}

func TestPromptRiskUnsignedIdentityNeverCreatesPersonProfile(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	input := &PromptFilterLogInput{
		Source: "local_filter", Action: "warn", Score: 70, MatchedPatterns: `[{"name":"x"}]`,
		APIKeyID: 10, APIKeyName: "shared", ClientIPHash: promptRiskHash("client-ip", "198.51.100.1"),
		NewAPIPolicyStatus: "verification_failed", NewAPIPlatform: "newapi-prod", NewAPIUserID: "spoofed-user",
	}
	if err := db.InsertPromptFilterLog(context.Background(), input); err != nil {
		t.Fatalf("InsertPromptFilterLog: %v", err)
	}
	profiles, _, err := db.ListPromptRiskProfiles(context.Background(), PromptRiskProfileQuery{Page: 1, PageSize: 20})
	if err != nil {
		t.Fatalf("ListPromptRiskProfiles: %v", err)
	}
	if person := findPromptRiskProfile(profiles, PromptRiskSubjectNewAPIUser); person != nil {
		t.Fatalf("unverified user became person profile: %#v", person)
	}
	if findPromptRiskProfile(profiles, PromptRiskSubjectAPIKey) == nil || findPromptRiskProfile(profiles, PromptRiskSubjectClientIP) == nil {
		t.Fatalf("lower-confidence scopes missing: %#v", profiles)
	}
}

func TestPromptRiskBackfillLearnsExistingCYWithoutInventingPerson(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	createdAt := time.Now().UTC().Add(-time.Hour)
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO prompt_policy_incidents (
		incident_id, created_at, api_key_id, api_key_name, account_id, account_name, upstream_error_code,
		local_evaluation_state, local_outcome, local_comparison, prompt_fingerprint, prompt_preview, prompt_available
	) VALUES ($1,$2,$3,$4,$5,$6,'cyber_policy','completed','no_hit','confirmed_miss',$7,'redacted',true)`,
		"legacy-risk-cy", createdAt, 77, "legacy-key", 88, "legacy-account", promptPolicyTestFingerprint("legacy-risk")); err != nil {
		t.Fatalf("insert legacy incident: %v", err)
	}
	if err := db.backfillPromptRiskEvents(ctx); err != nil {
		t.Fatalf("backfillPromptRiskEvents: %v", err)
	}
	profiles, total, err := db.ListPromptRiskProfiles(ctx, PromptRiskProfileQuery{Page: 1, PageSize: 20})
	if err != nil || total != 2 {
		t.Fatalf("profiles total=%d items=%#v err=%v", total, profiles, err)
	}
	if findPromptRiskProfile(profiles, PromptRiskSubjectNewAPIUser) != nil {
		t.Fatalf("legacy CY invented a person: %#v", profiles)
	}
	if findPromptRiskProfile(profiles, PromptRiskSubjectAPIKey) == nil || findPromptRiskProfile(profiles, PromptRiskSubjectUpstreamAccount) == nil {
		t.Fatalf("legacy CY did not learn key/account scopes: %#v", profiles)
	}
	if err := db.backfillPromptRiskEvents(ctx); err != nil {
		t.Fatalf("idempotent backfill: %v", err)
	}
	_, totalAfter, _ := db.ListPromptRiskProfiles(ctx, PromptRiskProfileQuery{Page: 1, PageSize: 20})
	if totalAfter != total {
		t.Fatalf("backfill duplicated profiles: before=%d after=%d", total, totalAfter)
	}
}

func TestPromptRiskProfilesPaginateFilterAndSurviveLogClear(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	for _, userID := range []string{"user-a", "user-b", "user-c"} {
		if err := db.InsertPromptFilterLog(ctx, &PromptFilterLogInput{
			Source: "local_filter", Action: "block", StrikeEligible: true, MatchedPatterns: `[{"name":"x"}]`,
			NewAPIPolicyStatus: "verified", NewAPIPlatform: "prod", NewAPIUserID: userID,
		}); err != nil {
			t.Fatalf("InsertPromptFilterLog(%s): %v", userID, err)
		}
	}
	page, total, err := db.ListPromptRiskProfiles(ctx, PromptRiskProfileQuery{Page: 2, PageSize: 1, SubjectType: PromptRiskSubjectNewAPIUser})
	if err != nil || total != 3 || len(page) != 1 {
		t.Fatalf("pagination total=%d page=%#v err=%v", total, page, err)
	}
	filtered, filteredTotal, err := db.ListPromptRiskProfiles(ctx, PromptRiskProfileQuery{Page: 1, PageSize: 20, SubjectType: PromptRiskSubjectNewAPIUser, Query: "user-b"})
	if err != nil || filteredTotal != 1 || len(filtered) != 1 || filtered[0].SubjectDisplay != "user-b" {
		t.Fatalf("filter total=%d items=%#v err=%v", filteredTotal, filtered, err)
	}
	if err := db.ClearPromptFilterLogs(ctx); err != nil {
		t.Fatalf("ClearPromptFilterLogs: %v", err)
	}
	_, total, err = db.ListPromptRiskProfiles(ctx, PromptRiskProfileQuery{Page: 1, PageSize: 20})
	if err != nil || total != 3 {
		t.Fatalf("profiles changed after log clear total=%d err=%v", total, err)
	}
}

func TestPromptRiskProfilesSurviveIncidentClear(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	incident, candidate, evidence := promptPolicyTestInputs("risk-clear-incident")
	incident.LocalComparison = PromptPolicyComparisonConfirmedMiss
	incident.NewAPIPolicyStatus = "verified"
	incident.NewAPIPlatform = "prod"
	incident.NewAPIUserID = "incident-user"
	if err := db.PersistPromptPolicyIncident(ctx, incident, candidate, evidence); err != nil {
		t.Fatalf("PersistPromptPolicyIncident: %v", err)
	}
	_, total, err := db.ListPromptRiskProfiles(ctx, PromptRiskProfileQuery{Page: 1, PageSize: 20})
	if err != nil || total == 0 {
		t.Fatalf("profiles before incident clear total=%d err=%v", total, err)
	}
	if err := db.ClearPromptPolicyIncidents(ctx); err != nil {
		t.Fatalf("ClearPromptPolicyIncidents: %v", err)
	}
	_, totalAfter, err := db.ListPromptRiskProfiles(ctx, PromptRiskProfileQuery{Page: 1, PageSize: 20})
	if err != nil || totalAfter != total {
		t.Fatalf("profiles changed after incident clear before=%d after=%d err=%v", total, totalAfter, err)
	}
}

func TestPromptRiskProfilesUseStableTieBreakerAcrossPages(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	// Keep every row on the same timestamp to exercise the stable tie-breaker,
	// but place the fixture inside ListPromptRiskProfiles' rolling 30-day window.
	createdAt := time.Now().UTC().Add(-time.Hour).Truncate(time.Second)
	for index := 0; index < 6; index++ {
		userID := fmt.Sprintf("stable-user-%d", index)
		subjectKey := PromptRiskNewAPIUserSubjectKey("gateway-a", userID)
		if _, err := db.conn.ExecContext(ctx, `INSERT INTO prompt_risk_events (
			created_at, source_type, source_id, subject_type, subject_key, subject_display, platform,
			is_person, identity_confidence, event_kind, request_risk_score, evidence_confidence, action
		) VALUES ($1,'prompt_filter',$2,'newapi_user',$3,$4,'gateway-a',true,100,'local_warn',45,100,'warn')`,
			createdAt, fmt.Sprintf("stable-source-%d", index), subjectKey, userID); err != nil {
			t.Fatalf("insert stable profile: %v", err)
		}
	}
	got := make([]string, 0, 6)
	for page := 1; page <= 3; page++ {
		items, total, err := db.ListPromptRiskProfiles(ctx, PromptRiskProfileQuery{Page: page, PageSize: 2, SubjectType: PromptRiskSubjectNewAPIUser})
		if err != nil || total != 6 || len(items) != 2 {
			t.Fatalf("page=%d total=%d items=%#v err=%v", page, total, items, err)
		}
		for _, item := range items {
			got = append(got, item.SubjectKey)
		}
	}
	want := append([]string(nil), got...)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unstable page order got=%#v want=%#v", got, want)
	}
}

func TestPromptRiskProfilesFilterUpstreamCYAndActivityState(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	if err := db.UpsertPromptRiskIdentities(ctx, []PromptRiskIdentityInput{{
		Platform: "gateway-a", ExternalUserID: "identity-only", UserName: "identity-only-user", Source: "test",
	}}); err != nil {
		t.Fatalf("UpsertPromptRiskIdentities: %v", err)
	}
	now := time.Now().UTC()
	for _, item := range []struct {
		sourceID, subjectKey, kind string
		score                      int
	}{
		{"cy-profile", "cy-user", "upstream_cy_policy", 80},
		{"local-profile", "local-user", "local_warn", 20},
	} {
		if _, err := db.conn.ExecContext(ctx, `INSERT INTO prompt_risk_events (
			created_at, source_type, source_id, subject_type, subject_key, subject_display,
			identity_confidence, event_kind, request_risk_score, evidence_confidence, action
		) VALUES ($1,'prompt_policy_incident',$2,'newapi_user',$3,$3,100,$4,100,100,'warn')`,
			now, item.sourceID, item.subjectKey, item.kind, item.score); err != nil {
			t.Fatalf("insert profile %s: %v", item.sourceID, err)
		}
	}
	cyOnly, total, err := db.ListPromptRiskProfiles(ctx, PromptRiskProfileQuery{Page: 1, PageSize: 20, UpstreamCYOnly: true})
	if err != nil || total != 1 || len(cyOnly) != 1 || cyOnly[0].SubjectKey != "cy-user" {
		t.Fatalf("CY-only profiles total=%d items=%#v err=%v", total, cyOnly, err)
	}
	active, total, err := db.ListPromptRiskProfiles(ctx, PromptRiskProfileQuery{Page: 1, PageSize: 20, ActivityState: "active"})
	if err != nil || total != 2 || len(active) != 2 {
		t.Fatalf("active profiles total=%d items=%#v err=%v", total, active, err)
	}
	identityOnly, total, err := db.ListPromptRiskProfiles(ctx, PromptRiskProfileQuery{Page: 1, PageSize: 20, ActivityState: "identity_only"})
	if err != nil || total != 1 || len(identityOnly) != 1 || identityOnly[0].NewAPIUserID != "identity-only" {
		t.Fatalf("identity-only profiles total=%d items=%#v err=%v", total, identityOnly, err)
	}
}

func TestPromptRiskProfilesPrioritizeAndFilterActiveConversationLocksBeforePagination(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	lockedSession := strings.Repeat("a", 64)
	unlockedSession := strings.Repeat("b", 64)
	for _, item := range []struct {
		sourceID   string
		sessionKey string
		score      int
	}{
		{sourceID: "locked-low-risk", sessionKey: lockedSession, score: 10},
		{sourceID: "unlocked-high-risk", sessionKey: unlockedSession, score: 100},
	} {
		if _, err := db.conn.ExecContext(ctx, `INSERT INTO prompt_risk_events (
			created_at, source_type, source_id, subject_type, subject_key, subject_display,
			identity_confidence, event_kind, request_risk_score, evidence_confidence, action
		) VALUES ($1,'prompt_filter_log',$2,'session',$3,$4,100,'local_warn',$5,100,'warn')`,
			now, item.sourceID, item.sessionKey, "session-"+item.sessionKey[:8], item.score); err != nil {
			t.Fatalf("insert risk profile %s: %v", item.sourceID, err)
		}
	}
	if _, _, err := db.LockPromptConversation(ctx, PromptConversationLockInput{
		LockKey: strings.Repeat("c", 64), IdentityKind: PromptConversationLockIdentityCodexSession,
		Platform: "codex-local", NewAPIUserID: "apikey:9",
		SessionFingerprint: strings.Repeat("d", 32), SessionHash: lockedSession,
		DecisionID: "local-block:locked-low-risk", ReasonCode: "terminal_policy_match", LockedAt: now,
	}); err != nil {
		t.Fatalf("LockPromptConversation: %v", err)
	}

	items, total, err := db.ListPromptRiskProfiles(ctx, PromptRiskProfileQuery{
		Page: 1, PageSize: 1, SubjectType: PromptRiskSubjectSession,
		PrioritizeActiveLocks: true, ConversationLockTTL: 168 * time.Hour, UserCyberCooldownTTL: 30 * time.Minute,
	})
	if err != nil || total != 2 || len(items) != 1 || items[0].SubjectKey != lockedSession {
		t.Fatalf("prioritized profiles total=%d items=%#v err=%v", total, items, err)
	}

	lockedOnly, lockedTotal, err := db.ListPromptRiskProfiles(ctx, PromptRiskProfileQuery{
		Page: 1, PageSize: 20, ActiveLocksOnly: true,
		ConversationLockTTL: 168 * time.Hour, UserCyberCooldownTTL: 30 * time.Minute,
	})
	if err != nil || lockedTotal != 1 || len(lockedOnly) != 1 || lockedOnly[0].SubjectKey != lockedSession {
		t.Fatalf("locked-only profiles total=%d items=%#v err=%v", lockedTotal, lockedOnly, err)
	}

	if _, err := db.conn.ExecContext(ctx, `DELETE FROM prompt_risk_events WHERE subject_type=$1 AND subject_key=$2`, PromptRiskSubjectSession, lockedSession); err != nil {
		t.Fatalf("clear locked session risk history: %v", err)
	}
	lockOnlyProfile, lockOnlyTotal, err := db.ListPromptRiskProfiles(ctx, PromptRiskProfileQuery{
		Page: 1, PageSize: 20, SubjectType: PromptRiskSubjectSession, ActiveLocksOnly: true,
		ConversationLockTTL: 168 * time.Hour, UserCyberCooldownTTL: 30 * time.Minute,
	})
	if err != nil || lockOnlyTotal != 1 || len(lockOnlyProfile) != 1 {
		t.Fatalf("lock-only profile total=%d items=%#v err=%v", lockOnlyTotal, lockOnlyProfile, err)
	}
	if lockOnlyProfile[0].SubjectKey != lockedSession || lockOnlyProfile[0].HasActivity || lockOnlyProfile[0].APIKeyID != 9 || lockOnlyProfile[0].RiskLevel != PromptRiskLevelLow {
		t.Fatalf("lock-only profile metadata=%#v", lockOnlyProfile[0])
	}
}

func TestPromptRiskScoringBandsAndHistoryGuardrail(t *testing.T) {
	for _, test := range []struct {
		score int
		want  string
	}{{0, PromptRiskLevelLow}, {14, PromptRiskLevelLow}, {15, PromptRiskLevelObserved}, {35, PromptRiskLevelElevated}, {60, PromptRiskLevelHigh}, {80, PromptRiskLevelCritical}} {
		if got := promptRiskLevel(test.score); got != test.want {
			t.Fatalf("promptRiskLevel(%d)=%q want %q", test.score, got, test.want)
		}
	}
	profile := PromptRiskProfile{RiskLevel: PromptRiskLevelCritical, SubjectType: PromptRiskSubjectNewAPIUser}
	actions := promptRiskRecommendations(profile)
	for _, action := range actions {
		if action == "block" || action == "ban" {
			t.Fatalf("history recommendation contains automatic enforcement: %#v", actions)
		}
	}
}

func TestPromptRiskClearedReviewDoesNotIncreaseRiskOrRecurrence(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	for index := 0; index < 3; index++ {
		if err := db.InsertPromptFilterLog(ctx, &PromptFilterLogInput{
			Source: "local_filter", Action: "allow", ReviewModel: "review-model", ReviewFlagged: false,
			NewAPIPolicyStatus: "verified", NewAPIPlatform: "prod", NewAPIUserID: "cleared-user",
		}); err != nil {
			t.Fatalf("InsertPromptFilterLog(%d): %v", index, err)
		}
	}
	profiles, total, err := db.ListPromptRiskProfiles(ctx, PromptRiskProfileQuery{Page: 1, PageSize: 20, SubjectType: PromptRiskSubjectNewAPIUser})
	if err != nil || total != 1 || len(profiles) != 1 {
		t.Fatalf("profiles total=%d items=%#v err=%v", total, profiles, err)
	}
	if profiles[0].RiskScore != 0 || profiles[0].ScoreBreakdown.Recurrence != 0 || profiles[0].RiskLevel != PromptRiskLevelLow {
		t.Fatalf("cleared reviews increased risk: %#v", profiles[0])
	}
}

func TestPromptRiskUnverifiedBlocksAreDeduplicatedAndCapped(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	for index := 0; index < 20; index++ {
		if err := db.InsertPromptFilterLog(ctx, &PromptFilterLogInput{
			Source: "local_filter", Action: "block", ReviewModel: "review-model", ReviewError: "context deadline exceeded",
			TextPreview: "benign repeated development request", RequestCorrelationID: fmt.Sprintf("unverified-%d", index),
			NewAPIPolicyStatus: "verified", NewAPIPlatform: "prod", NewAPIUserID: "unverified-user",
		}); err != nil {
			t.Fatalf("InsertPromptFilterLog(%d): %v", index, err)
		}
	}
	for index := 0; index < 20; index++ {
		if err := db.InsertPromptFilterLog(ctx, &PromptFilterLogInput{
			Source: "local_filter", Action: "block", ReviewModel: "review-model", ReviewError: "context deadline exceeded",
			TextPreview: fmt.Sprintf("distinct unverified development request %d", index), RequestCorrelationID: fmt.Sprintf("distinct-unverified-%d", index),
			NewAPIPolicyStatus: "verified", NewAPIPlatform: "prod", NewAPIUserID: "unverified-user",
		}); err != nil {
			t.Fatalf("InsertPromptFilterLog(distinct %d): %v", index, err)
		}
	}
	profiles, total, err := db.ListPromptRiskProfiles(ctx, PromptRiskProfileQuery{Page: 1, PageSize: 20, SubjectType: PromptRiskSubjectNewAPIUser})
	if err != nil || total != 1 || len(profiles) != 1 {
		t.Fatalf("profiles total=%d items=%#v err=%v", total, profiles, err)
	}
	profile := profiles[0]
	if profile.RiskScore != 14 || profile.RiskLevel != PromptRiskLevelLow || profile.LocalBlockCount != 40 {
		t.Fatalf("unverified repeats inflated profile: %#v", profile)
	}
	basis, err := db.GetPromptRiskAdaptiveReviewBasis(ctx, profile.SubjectType, profile.SubjectKey, time.Now().UTC().Add(-time.Hour))
	if err != nil || basis.PositiveEvidenceCount != 0 {
		t.Fatalf("unverified blocks became confirmed trust evidence: basis=%#v err=%v", basis, err)
	}
	events, eventTotal, err := db.ListPromptRiskEvents(ctx, profile.SubjectType, profile.SubjectKey, PromptRiskEventQuery{Page: 1, PageSize: 1})
	if err != nil || eventTotal != 40 || len(events) != 1 || events[0].EventKind != promptRiskEventLocalBlockUnverified || events[0].PromptFingerprint == "" {
		t.Fatalf("unverified event total=%d items=%#v err=%v", eventTotal, events, err)
	}
}

func TestPromptRiskUnverifiedBlockReconcilesWhenClearArrivesFirst(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	common := PromptFilterLogInput{
		Source: "local_filter", TextPreview: "normal application maintenance", RequestCorrelationID: "review-correction-reversed",
		NewAPIPolicyStatus: "verified", NewAPIPlatform: "prod", NewAPIUserID: "reverse-corrected-user",
	}
	cleared := common
	cleared.Action = "allow"
	cleared.ReviewModel = "review-model"
	cleared.Reviewed = true
	if err := db.InsertPromptFilterLog(ctx, &cleared); err != nil {
		t.Fatalf("InsertPromptFilterLog(clear): %v", err)
	}
	blocked := common
	blocked.Action = "block"
	blocked.ReviewModel = "review-model"
	blocked.ReviewError = "context deadline exceeded"
	if err := db.InsertPromptFilterLog(ctx, &blocked); err != nil {
		t.Fatalf("InsertPromptFilterLog(block): %v", err)
	}
	profiles, _, err := db.ListPromptRiskProfiles(ctx, PromptRiskProfileQuery{Page: 1, PageSize: 20, SubjectType: PromptRiskSubjectNewAPIUser})
	if err != nil || len(profiles) != 1 || profiles[0].RiskScore != 0 {
		t.Fatalf("reverse-order correction profile=%#v err=%v", profiles, err)
	}
	events, _, err := db.ListPromptRiskEvents(ctx, profiles[0].SubjectType, profiles[0].SubjectKey, PromptRiskEventQuery{Page: 1, PageSize: 10})
	if err != nil || len(events) != 2 || events[0].EventKind != promptRiskEventLocalBlockCleared || events[1].EventKind != promptRiskEventReviewCleared {
		t.Fatalf("reverse-order correction events=%#v err=%v", events, err)
	}
}

func TestPromptRiskClearedReviewReconcilesPriorUnverifiedBlock(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	common := PromptFilterLogInput{
		Source: "local_filter", TextPreview: "normal application maintenance", RequestCorrelationID: "review-correction-request",
		NewAPIPolicyStatus: "verified", NewAPIPlatform: "prod", NewAPIUserID: "corrected-user",
	}
	blocked := common
	blocked.Action = "block"
	blocked.ReviewModel = "review-model"
	blocked.ReviewError = "context deadline exceeded"
	if err := db.InsertPromptFilterLog(ctx, &blocked); err != nil {
		t.Fatalf("InsertPromptFilterLog(block): %v", err)
	}
	cleared := common
	cleared.Action = "allow"
	cleared.ReviewModel = "review-model"
	cleared.Reviewed = true
	if err := db.InsertPromptFilterLog(ctx, &cleared); err != nil {
		t.Fatalf("InsertPromptFilterLog(clear): %v", err)
	}
	profiles, total, err := db.ListPromptRiskProfiles(ctx, PromptRiskProfileQuery{Page: 1, PageSize: 20, SubjectType: PromptRiskSubjectNewAPIUser})
	if err != nil || total != 1 || len(profiles) != 1 || profiles[0].RiskScore != 0 {
		t.Fatalf("corrected profile total=%d items=%#v err=%v", total, profiles, err)
	}
	events, eventTotal, err := db.ListPromptRiskEvents(ctx, profiles[0].SubjectType, profiles[0].SubjectKey, PromptRiskEventQuery{Page: 1, PageSize: 10})
	if err != nil || eventTotal != 2 || len(events) != 2 {
		t.Fatalf("corrected events total=%d items=%#v err=%v", eventTotal, events, err)
	}
	if events[0].EventKind != promptRiskEventReviewCleared || events[1].EventKind != promptRiskEventLocalBlockCleared || events[1].RequestRiskScore != 0 {
		t.Fatalf("review correction was not durable: %#v", events)
	}
}

func TestPromptRiskLegacyLocalBlocksUseConservativeDeduplicatedScoring(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	subjectKey := PromptRiskNewAPIUserSubjectKey("prod", "legacy-unverified-user")
	for index := 0; index < 20; index++ {
		if _, err := db.conn.ExecContext(ctx, `INSERT INTO prompt_risk_events (
			created_at, source_type, source_id, subject_type, subject_key, subject_display, platform,
			is_person, identity_confidence, event_kind, request_risk_score, evidence_confidence, action, prompt_preview
		) VALUES (CURRENT_TIMESTAMP,'prompt_filter_log',$1,'newapi_user',$2,'legacy-user','prod',true,100,'local_block',38,85,'block','same benign preview')`,
			fmt.Sprintf("legacy-unverified-%d", index), subjectKey); err != nil {
			t.Fatalf("insert legacy event: %v", err)
		}
	}
	profiles, total, err := db.ListPromptRiskProfiles(ctx, PromptRiskProfileQuery{Page: 1, PageSize: 20, SubjectType: PromptRiskSubjectNewAPIUser})
	if err != nil || total != 1 || len(profiles) != 1 {
		t.Fatalf("legacy profiles total=%d items=%#v err=%v", total, profiles, err)
	}
	if profiles[0].RiskScore > 14 || profiles[0].RiskLevel != PromptRiskLevelLow || profiles[0].LocalBlockCount != 20 {
		t.Fatalf("legacy local blocks were not conservatively reinterpreted: %#v", profiles[0])
	}
}

func TestPromptRiskProfileAggregationStaysBoundedWithManyClearedEvents(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	tx, err := db.conn.Begin()
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.Prepare(`INSERT INTO prompt_risk_events (
		created_at, source_type, source_id, subject_type, subject_key, subject_display, platform,
		is_person, identity_confidence, event_kind, request_risk_score, evidence_confidence,
		action, prompt_fingerprint, prompt_preview
	) VALUES (CURRENT_TIMESTAMP,'prompt_filter_log',$1,'newapi_user',$2,$3,'prod',true,100,$4,$5,100,$6,$7,$8)`)
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	for index := 0; index < 30_000; index++ {
		subject := fmt.Sprintf("bulk-user-%03d", index%100)
		eventKind, score, action := promptRiskEventReviewCleared, 0, "allow"
		fingerprint := promptRiskHash("cleared", fmt.Sprintf("%d", index))
		preview := "cleared review"
		if index < 1_000 {
			eventKind, score, action = promptRiskEventLocalBlock, 38, "block"
			fingerprint = promptRiskHash("blocked", fmt.Sprintf("%d", index%100))
			preview = "repeated unverified block"
		}
		if _, err := stmt.Exec(fmt.Sprintf("bulk-%d", index), PromptRiskNewAPIUserSubjectKey("prod", subject), subject,
			eventKind, score, action, fingerprint, preview); err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			t.Fatalf("insert event %d: %v", index, err)
		}
	}
	if err := stmt.Close(); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	// 性能界限门:30k 事件的聚合必须在 2s 内完成。-race 构建的开销是
	// 5-20 倍(CI 的 2 核 runner 上实测超过 40s 也跑不完 2s 门),竞争
	// 检测跑的是正确性不是性能,按倍率放宽。
	deadline := 2 * time.Second
	if raceDetectorEnabled {
		deadline = 120 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), deadline)
	defer cancel()
	profiles, total, err := db.ListPromptRiskProfiles(ctx, PromptRiskProfileQuery{
		Page: 1, PageSize: 20, SubjectType: PromptRiskSubjectNewAPIUser,
	})
	if err != nil {
		t.Fatalf("bounded aggregation failed: %v", err)
	}
	if total != 100 || len(profiles) != 20 {
		t.Fatalf("profiles total=%d len=%d, want 100/20", total, len(profiles))
	}
}

func TestPromptRiskDefensiveSecurityContextIsObservedAndRepeated(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	confidence := 0.04
	for index := 0; index < 5; index++ {
		if err := db.InsertPromptFilterLog(ctx, &PromptFilterLogInput{
			Source: "local_filter", Action: "allow", AuditScore: 20,
			MatchedPatterns: `[{"name":"malware_family","weight":20,"category":"malware","signal_only":true}]`,
			TextPreview:     "Write a YARA rule to detect ransomware encryptors.", Reviewed: true,
			ReviewModel: "review-model", ReviewConfidence: &confidence,
			NewAPIPolicyStatus: "verified", NewAPIPlatform: "prod", NewAPIUserID: "defensive-user",
		}); err != nil {
			t.Fatalf("InsertPromptFilterLog(%d): %v", index, err)
		}
	}
	profiles, total, err := db.ListPromptRiskProfiles(ctx, PromptRiskProfileQuery{Page: 1, PageSize: 20, SubjectType: PromptRiskSubjectNewAPIUser})
	if err != nil || total != 1 || len(profiles) != 1 {
		t.Fatalf("profiles total=%d items=%#v err=%v", total, profiles, err)
	}
	profile := profiles[0]
	if profile.EventCount != 5 || profile.RepeatedFingerprints != 4 || profile.RiskScore <= 0 || profile.RiskScore >= 35 {
		t.Fatalf("defensive observation profile=%#v", profile)
	}
	events, eventTotal, err := db.ListPromptRiskEvents(ctx, profile.SubjectType, profile.SubjectKey, PromptRiskEventQuery{Page: 1, PageSize: 10})
	if err != nil || eventTotal != 5 || len(events) != 5 || events[0].EventKind != "local_security_context_observed" || events[0].PromptFingerprint == "" {
		t.Fatalf("events total=%d items=%#v err=%v", eventTotal, events, err)
	}
}

func TestPromptRiskTerminalBlockCannotBeErasedByClearedReview(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	if err := db.InsertPromptFilterLog(context.Background(), &PromptFilterLogInput{
		Source: "local_filter", Action: "block", StrikeEligible: true, ReviewModel: "review-model",
		Reviewed: true, ReviewFlagged: false, MatchedPatterns: `[{"name":"terminal","category":"malware"}]`,
		NewAPIPolicyStatus: "verified", NewAPIPlatform: "prod", NewAPIUserID: "terminal-user",
	}); err != nil {
		t.Fatal(err)
	}
	profiles, _, err := db.ListPromptRiskProfiles(context.Background(), PromptRiskProfileQuery{Page: 1, PageSize: 20, SubjectType: PromptRiskSubjectNewAPIUser})
	if err != nil || len(profiles) != 1 || profiles[0].LocalBlockCount != 1 || profiles[0].RiskScore == 0 {
		t.Fatalf("terminal block profile=%#v err=%v", profiles, err)
	}
}

func TestPromptRiskHistoricalAuditOnlyEventsNoLongerInflateProfiles(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	subjectKey := PromptRiskNewAPIUserSubjectKey("gateway-a", "audit-only-user")
	for index := 0; index < 5; index++ {
		if _, err := db.conn.ExecContext(ctx, `INSERT INTO prompt_risk_events (
			created_at, source_type, source_id, subject_type, subject_key, subject_display, platform,
			is_person, identity_confidence, event_kind, request_risk_score, evidence_confidence,
			action, prompt_fingerprint
		) VALUES (CURRENT_TIMESTAMP,'prompt_filter',$1,'newapi_user',$2,'audit-only-user','gateway-a',true,100,'local_audit_hit',10,100,'allow',$3)`,
			fmt.Sprintf("legacy-audit-%d", index), subjectKey, promptRiskHash("audit", fmt.Sprintf("%d", index))); err != nil {
			t.Fatalf("insert legacy audit event: %v", err)
		}
	}
	profiles, total, err := db.ListPromptRiskProfiles(ctx, PromptRiskProfileQuery{Page: 1, PageSize: 20, SubjectType: PromptRiskSubjectNewAPIUser})
	if err != nil || total != 1 || len(profiles) != 1 {
		t.Fatalf("profiles total=%d items=%#v err=%v", total, profiles, err)
	}
	if profiles[0].RiskScore != 0 || profiles[0].ScoreBreakdown.Recurrence != 0 || profiles[0].RiskLevel != PromptRiskLevelLow {
		t.Fatalf("legacy audit-only events still inflate risk: %#v", profiles[0])
	}
}

func TestPromptRiskSQLiteSchemaAndIndexes(t *testing.T) {
	db := newPromptPolicySQLiteTestDB(t)
	ctx := context.Background()
	for table, expected := range map[string][]string{
		"prompt_filter_logs":        {"session_hash"},
		"prompt_policy_incidents":   {"newapi_policy_status", "newapi_platform", "newapi_user_id", "newapi_request_id", "session_hash", "client_ip_hash"},
		"prompt_risk_events":        {"source_type", "source_id", "subject_type", "subject_key", "is_person", "identity_confidence", "request_risk_score", "evidence_confidence", "incident_id", "api_key_id", "account_id"},
		"prompt_risk_event_sources": {"source_type", "source_id", "processed_at"},
		"prompt_risk_identities":    {"subject_type", "subject_key", "platform", "external_user_id", "user_name", "user_email", "user_group", "source", "updated_at"},
	} {
		columns, err := db.sqliteTableColumns(ctx, table)
		if err != nil {
			t.Fatalf("sqliteTableColumns(%s): %v", table, err)
		}
		for _, name := range expected {
			if _, ok := columns[name]; !ok {
				t.Fatalf("%s missing column %q", table, name)
			}
		}
	}
	rows, err := db.conn.QueryContext(ctx, `SELECT name FROM sqlite_master WHERE type='index' AND tbl_name='prompt_risk_events'`)
	if err != nil {
		t.Fatalf("list risk indexes: %v", err)
	}
	defer rows.Close()
	indexes := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatalf("scan risk index: %v", err)
		}
		indexes[name] = true
	}
	for _, name := range []string{"idx_prompt_risk_events_subject", "idx_prompt_risk_events_created", "idx_prompt_risk_events_kind", "idx_prompt_risk_events_incident", "idx_prompt_risk_events_api_key", "idx_prompt_risk_events_account", "idx_prompt_risk_events_request_match", "idx_prompt_risk_events_fingerprint_match"} {
		if !indexes[name] {
			t.Fatalf("prompt_risk_events missing index %q", name)
		}
	}
}

func TestPromptRiskPostgresMigrationDDL(t *testing.T) {
	promptPolicyDDLDriverOnce.Do(func() { sql.Register("prompt-policy-ddl-capture", promptPolicyDDLDriver{}) })
	promptPolicyDDLQueryMu.Lock()
	promptPolicyDDLQueries = nil
	promptPolicyDDLQueryMu.Unlock()
	conn, err := sql.Open("prompt-policy-ddl-capture", "")
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer conn.Close()
	db := &DB{conn: conn, driver: "postgres"}
	if err := db.ensurePromptPolicyIncidentsTable(context.Background()); err != nil {
		t.Fatalf("ensurePromptPolicyIncidentsTable: %v", err)
	}
	promptPolicyDDLQueryMu.Lock()
	joined := strings.Join(promptPolicyDDLQueries, "\n")
	promptPolicyDDLQueryMu.Unlock()
	for _, fragment := range []string{
		"CREATE TABLE IF NOT EXISTS prompt_risk_events",
		"CREATE TABLE IF NOT EXISTS prompt_risk_event_sources",
		"UNIQUE(source_type, source_id, subject_type, subject_key)",
		"ALTER TABLE prompt_policy_incidents ADD COLUMN IF NOT EXISTS newapi_policy_status",
		"ALTER TABLE prompt_policy_incidents ADD COLUMN IF NOT EXISTS session_hash",
		"ALTER TABLE prompt_policy_incidents ADD COLUMN IF NOT EXISTS client_ip_hash",
		"ALTER TABLE prompt_filter_logs ADD COLUMN IF NOT EXISTS session_hash",
		"idx_prompt_risk_events_subject",
		"idx_prompt_risk_events_incident",
		"idx_prompt_risk_events_request_match",
		"idx_prompt_risk_events_fingerprint_match",
	} {
		if !strings.Contains(joined, fragment) {
			t.Fatalf("postgres risk migration missing %q: %s", fragment, joined)
		}
	}
}

func TestPromptRiskReviewReconciliationUsesTargetedIndexes(t *testing.T) {
	db, err := New("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("New sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	assertPlanUses := func(query string, args []interface{}, indexName string) {
		t.Helper()
		rows, err := db.conn.QueryContext(ctx, "EXPLAIN QUERY PLAN "+query, args...)
		if err != nil {
			t.Fatalf("explain query plan: %v", err)
		}
		defer rows.Close()
		var plan strings.Builder
		for rows.Next() {
			var id, parent, unused int
			var detail string
			if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
				t.Fatalf("scan query plan: %v", err)
			}
			plan.WriteString(detail)
			plan.WriteByte('\n')
		}
		if !strings.Contains(plan.String(), indexName) {
			t.Fatalf("query plan does not use %s:\n%s", indexName, plan.String())
		}
	}

	assertPlanUses(`SELECT id FROM prompt_risk_events
		WHERE request_correlation_id<>'' AND request_correlation_id=$1
		AND subject_type=$2 AND subject_key=$3 AND event_kind=$4`,
		[]interface{}{"request-1", PromptRiskSubjectAPIKey, "key-1", promptRiskEventReviewCleared},
		"idx_prompt_risk_events_request_match")
	assertPlanUses(`SELECT id FROM prompt_risk_events
		WHERE request_correlation_id='' AND prompt_fingerprint<>'' AND prompt_fingerprint=$1
		AND subject_type=$2 AND subject_key=$3 AND created_at >= $4 AND created_at <= $5 AND event_kind=$6`,
		[]interface{}{"fingerprint-1", PromptRiskSubjectAPIKey, "key-1", time.Now().Add(-time.Minute), time.Now(), promptRiskEventReviewCleared},
		"idx_prompt_risk_events_fingerprint_match")
}
