package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestPromptRiskProfileListTimeoutAllowsProductionAggregation(t *testing.T) {
	if promptRiskProfileListTimeout < 10*time.Second {
		t.Fatalf("prompt risk profile list timeout = %s, want at least 10s for production aggregation", promptRiskProfileListTimeout)
	}
}

func TestPromptRiskSessionWindowsReturnsOnlyCurrentlyActiveUserWindows(t *testing.T) {
	runtimeCache := cache.NewMemory(1)
	defer runtimeCache.Close()
	now := time.Now().UTC().Truncate(time.Second)
	createdAt := now.Add(-10 * time.Minute)
	state := cache.PromptSessionLimitState{
		Version: 2,
		Sessions: map[string]time.Time{
			"active-hash":  now.Add(20 * time.Minute),
			"expired-hash": now.Add(-time.Second),
		},
		Details: map[string]cache.PromptSessionWindowDetail{
			"active-hash":  {CreatedAt: createdAt, AccountID: 73, Model: "gpt-5.6-sol", ReasoningEffort: "high", ClientUserAgent: "codex_cli_rs/0.128.0", PromptPreview: "hello active window"},
			"expired-hash": {CreatedAt: now.Add(-time.Hour), AccountID: 74},
		},
	}
	raw, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	subject := cache.PromptSessionLimitSubject("NEWAPI", "user-42")
	if err := runtimeCache.SetRuntime(t.Context(), cache.PromptSessionLimitRuntimeNamespace, subject, raw, time.Hour); err != nil {
		t.Fatalf("SetRuntime: %v", err)
	}
	h := &Handler{cache: runtimeCache}
	items := h.promptRiskSessionWindows(t.Context(), &database.PromptRiskProfile{
		SubjectType: database.PromptRiskSubjectNewAPIUser, Platform: "newapi", NewAPIUserID: "user-42",
	}, now)
	if len(items) != 1 {
		t.Fatalf("active windows = %#v, want one item", items)
	}
	item := items[0]
	if item.SessionHash != "active-hash" || item.CreatedAt == nil || !item.CreatedAt.Equal(createdAt) || item.AccountID != 73 || item.Model != "gpt-5.6-sol" || item.ReasoningEffort != "high" || item.ClientUserAgent != "codex_cli_rs/0.128.0" || item.PromptPreview != "hello active window" {
		t.Fatalf("active item = %#v", item)
	}
	if item.RemainingSeconds < 1199 || item.RemainingSeconds > 1200 {
		t.Fatalf("remaining seconds = %d, want about 1200", item.RemainingSeconds)
	}
}

func TestPromptRiskProfileListAndDetailAPI(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "admin-risk-profile.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer db.Close()
	zero := 0
	incident := database.PromptPolicyIncidentInput{
		IncidentID: "risk-profile-incident", RequestCorrelationID: "risk-profile-request",
		Endpoint: "/v1/responses", Protocol: "responses", Transport: "sse", Model: "gpt-5.6-sol",
		StatusCode: 500, AccountID: 73, AccountName: "account@example.com", APIKeyID: 9, APIKeyName: "tenant-key",
		UpstreamErrorCode: "cyber_policy", LocalEvaluationState: database.PromptPolicyEvaluationCompleted,
		LocalOutcome: database.PromptPolicyOutcomeNoHit, LocalComparison: database.PromptPolicyComparisonConfirmedMiss,
		LocalScore: &zero, LocalRawScore: &zero, LocalAuditScore: &zero, LocalAuditRawScore: &zero,
		PromptFingerprint: adminPolicyFingerprint("risk-profile-prompt"), PromptPreview: "risk-profile-prompt", PromptText: "risk-profile-prompt", PromptAvailable: true,
		NewAPIPolicyStatus: "verified", NewAPIPlatform: "newapi", NewAPIUserID: "user-42", SessionHash: "session-hash", ClientIPHash: "ip-hash",
		ObservedAt: time.Now().UTC(),
	}
	candidate := database.PromptRuleCandidateInput{Fingerprint: incident.PromptFingerprint, Kind: database.PromptRuleCandidateKindEvidence, Source: database.PromptRuleCandidateSourceUpstreamCyberPolicy, SamplePreview: incident.PromptPreview}
	evidence := database.PromptRuleCandidateEvidenceInput{SourceKind: database.PromptRuleCandidateSourceUpstreamCyberPolicy, SourceRef: incident.RequestCorrelationID, SourceRefHash: adminPolicyFingerprint(incident.IncidentID), MetadataJSON: `{}`, ObservedAt: incident.ObservedAt}
	if err := db.PersistPromptPolicyIncident(t.Context(), incident, candidate, evidence); err != nil {
		t.Fatalf("PersistPromptPolicyIncident: %v", err)
	}

	h := &Handler{db: db}
	router := gin.New()
	router.GET("/api/admin/prompt-policy/risk-profiles", h.ListPromptRiskProfiles)
	router.GET("/api/admin/prompt-policy/risk-profiles/:subject_type/:subject_key", h.GetPromptRiskProfile)

	listRecorder := httptest.NewRecorder()
	router.ServeHTTP(listRecorder, httptest.NewRequest(http.MethodGet, "/api/admin/prompt-policy/risk-profiles?subject_type=newapi_user&platform=newapi&page=1&page_size=1", nil))
	if listRecorder.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", listRecorder.Code, listRecorder.Body.String())
	}
	var list struct {
		Profiles       []database.PromptRiskProfile `json:"profiles"`
		Total          int                          `json:"total"`
		Page           int                          `json:"page"`
		PageSize       int                          `json:"page_size"`
		ScoringVersion string                       `json:"scoring_version"`
		Guardrail      string                       `json:"guardrail"`
	}
	if err := json.Unmarshal(listRecorder.Body.Bytes(), &list); err != nil || list.Total != 1 || len(list.Profiles) != 1 || !list.Profiles[0].IsPerson || list.Profiles[0].SubjectDisplay != "user-42" || list.ScoringVersion != database.PromptRiskScoringVersion || list.Guardrail == "" {
		t.Fatalf("list response=%s err=%v", listRecorder.Body.String(), err)
	}

	detailRecorder := httptest.NewRecorder()
	path := "/api/admin/prompt-policy/risk-profiles/newapi_user/" + list.Profiles[0].SubjectKey + "?event_page=1&event_page_size=1"
	router.ServeHTTP(detailRecorder, httptest.NewRequest(http.MethodGet, path, nil))
	if detailRecorder.Code != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", detailRecorder.Code, detailRecorder.Body.String())
	}
	var detail struct {
		Profile       database.PromptRiskProfile `json:"profile"`
		Events        []database.PromptRiskEvent `json:"events"`
		EventTotal    int                        `json:"event_total"`
		EventPage     int                        `json:"event_page"`
		EventPageSize int                        `json:"event_page_size"`
		Guardrail     string                     `json:"guardrail"`
	}
	if err := json.Unmarshal(detailRecorder.Body.Bytes(), &detail); err != nil || detail.EventTotal != 1 || len(detail.Events) != 1 || detail.Events[0].IncidentID != incident.IncidentID || detail.Profile.RiskScore <= 0 || detail.Guardrail == "" {
		t.Fatalf("detail response=%s err=%v", detailRecorder.Body.String(), err)
	}
}

func TestPromptRiskProfileDetailRejectsUnknownSubjectType(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handler{}
	router := gin.New()
	router.GET("/api/admin/prompt-policy/risk-profiles/:subject_type/:subject_key", h.GetPromptRiskProfile)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/prompt-policy/risk-profiles/person/raw-user-id", nil))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestPromptRiskProfileListAndDetailExposeActiveLockWithoutAuditHistory(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "admin-lock-only-profile.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer db.Close()
	sessionHash := strings.Repeat("a", 64)
	if _, _, err := db.LockPromptConversation(t.Context(), database.PromptConversationLockInput{
		LockKey: strings.Repeat("b", 64), IdentityKind: database.PromptConversationLockIdentityCodexSession,
		Platform: "codex-local", NewAPIUserID: "apikey:9",
		SessionFingerprint: strings.Repeat("c", 32), SessionHash: sessionHash,
		DecisionID: "lock-only-decision", ReasonCode: "terminal_policy_match", LockedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("LockPromptConversation: %v", err)
	}

	h := &Handler{db: db}
	router := gin.New()
	router.GET("/api/admin/prompt-policy/risk-profiles", h.ListPromptRiskProfiles)
	router.GET("/api/admin/prompt-policy/risk-profiles/:subject_type/:subject_key", h.GetPromptRiskProfile)

	listRecorder := httptest.NewRecorder()
	router.ServeHTTP(listRecorder, httptest.NewRequest(http.MethodGet, "/api/admin/prompt-policy/risk-profiles?subject_type=session&locked_only=true", nil))
	if listRecorder.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", listRecorder.Code, listRecorder.Body.String())
	}
	var list struct {
		Profiles []database.PromptRiskProfile `json:"profiles"`
		Total    int                          `json:"total"`
	}
	if err := json.Unmarshal(listRecorder.Body.Bytes(), &list); err != nil || list.Total != 1 || len(list.Profiles) != 1 {
		t.Fatalf("list response=%s err=%v", listRecorder.Body.String(), err)
	}
	if list.Profiles[0].HasActivity || list.Profiles[0].ConversationLock == nil || list.Profiles[0].ConversationLock.RestrictionScope != database.PromptConversationRestrictionScopeConversation {
		t.Fatalf("lock-only list profile=%#v", list.Profiles[0])
	}

	detailRecorder := httptest.NewRecorder()
	router.ServeHTTP(detailRecorder, httptest.NewRequest(http.MethodGet, "/api/admin/prompt-policy/risk-profiles/session/"+sessionHash, nil))
	if detailRecorder.Code != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", detailRecorder.Code, detailRecorder.Body.String())
	}
	var detail struct {
		Profile    database.PromptRiskProfile `json:"profile"`
		Events     []database.PromptRiskEvent `json:"events"`
		EventTotal int                        `json:"event_total"`
	}
	if err := json.Unmarshal(detailRecorder.Body.Bytes(), &detail); err != nil || detail.EventTotal != 0 || len(detail.Events) != 0 || detail.Profile.ConversationLock == nil {
		t.Fatalf("detail response=%s err=%v", detailRecorder.Body.String(), err)
	}
}

func TestPromptRiskProfileDetailReturnsAdaptiveBasisAndPagedTrustAudit(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "admin-risk-profile-audit.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer db.Close()
	if err := db.InsertPromptFilterLog(t.Context(), &database.PromptFilterLogInput{
		Source: "local_filter", Action: "allow", ReviewModel: "review-model", ReviewFlagged: false,
		NewAPIPolicyStatus: "verified", NewAPIPlatform: "gateway-a", NewAPIUserID: "audit-user",
	}); err != nil {
		t.Fatalf("InsertPromptFilterLog: %v", err)
	}
	profiles, total, err := db.ListPromptRiskProfiles(t.Context(), database.PromptRiskProfileQuery{
		Page: 1, PageSize: 10, SubjectType: database.PromptRiskSubjectNewAPIUser,
	})
	if err != nil || total != 1 || len(profiles) != 1 {
		t.Fatalf("profiles=%#v total=%d err=%v", profiles, total, err)
	}
	profile := profiles[0]
	policy, err := db.UpsertPromptRiskTrustPolicy(t.Context(), database.PromptRiskTrustPolicyInput{
		SubjectType: profile.SubjectType, SubjectKey: profile.SubjectKey, Reason: "audit pagination",
		RiskThreshold: 35, ValidUntil: time.Now().UTC().Add(24 * time.Hour),
	})
	if err != nil {
		t.Fatalf("UpsertPromptRiskTrustPolicy: %v", err)
	}
	if err := db.RecordPromptRiskTrustBypass(t.Context(), policy.ID, policy.SubjectType, policy.SubjectKey, "request-audit-hash"); err != nil {
		t.Fatalf("RecordPromptRiskTrustBypass: %v", err)
	}

	h := &Handler{db: db}
	router := gin.New()
	router.GET("/api/admin/prompt-policy/risk-profiles/:subject_type/:subject_key", h.GetPromptRiskProfile)
	recorder := httptest.NewRecorder()
	path := "/api/admin/prompt-policy/risk-profiles/newapi_user/" + profile.SubjectKey + "?event_page=1&event_page_size=1&trust_event_page=1&trust_event_page_size=1"
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		AdaptiveReviewBasis promptRiskAdaptiveReviewBasisResponse `json:"adaptive_review_basis"`
		TrustEvents         []database.PromptRiskTrustEvent       `json:"trust_events"`
		TrustEventTotal     int                                   `json:"trust_event_total"`
		TrustEventPage      int                                   `json:"trust_event_page"`
		TrustEventPageSize  int                                   `json:"trust_event_page_size"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode detail response: %v", err)
	}
	if response.TrustEventTotal != 2 || response.TrustEventPage != 1 || response.TrustEventPageSize != 1 || len(response.TrustEvents) != 1 {
		t.Fatalf("unexpected trust audit pagination: %#v body=%s", response, recorder.Body.String())
	}
	if response.TrustEvents[0].RequestIDHash != "request-audit-hash" {
		t.Fatalf("request audit hash missing: %#v", response.TrustEvents[0])
	}
	if response.AdaptiveReviewBasis.Decision == "" || response.AdaptiveReviewBasis.MinCleanReviews <= 0 || response.AdaptiveReviewBasis.TrustDurationHours <= 0 {
		t.Fatalf("adaptive review basis missing defaults: %#v", response.AdaptiveReviewBasis)
	}
}
