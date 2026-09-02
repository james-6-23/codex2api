package admin

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
)

func adminPolicyFingerprint(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func TestPromptPolicyIncidentListAndDetailAPI(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "admin-policy.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer db.Close()
	zero := 0
	incident := database.PromptPolicyIncidentInput{
		IncidentID: "incident-admin", RequestCorrelationID: "request-admin", Endpoint: "/v1/responses", Model: "gpt-5.4",
		Protocol: "responses", Transport: "sse", StatusCode: 400, AccountID: 73, AccountName: "account@example.com", AccountGroupIDs: []int64{4}, AccountGroupNames: []string{"打铁"},
		APIKeyID: 9, APIKeyName: "test-key", APIKeyAllowedGroupIDs: []int64{1, 4}, APIKeyAllowedGroupNames: []string{"示例平台", "打铁"}, UpstreamErrorCode: "cyber_policy",
		LocalEvaluationState: database.PromptPolicyEvaluationCompleted, LocalOutcome: database.PromptPolicyOutcomeNoHit,
		LocalScore: &zero, LocalRawScore: &zero, LocalAuditScore: &zero, LocalAuditRawScore: &zero,
		LocalMatchedPatterns: `[{"name":"test","weight":0}]`, PromptFingerprint: adminPolicyFingerprint("prompt"), PromptPreview: "prompt", PromptText: "prompt",
		PromptAvailable: true, LocalComparison: database.PromptPolicyComparisonConfirmedMiss,
		ObservedAt: time.Now().UTC(),
	}
	candidate := database.PromptRuleCandidateInput{Fingerprint: incident.PromptFingerprint, Kind: database.PromptRuleCandidateKindEvidence, Source: database.PromptRuleCandidateSourceUpstreamCyberPolicy, SamplePreview: "prompt"}
	evidence := database.PromptRuleCandidateEvidenceInput{SourceKind: database.PromptRuleCandidateSourceUpstreamCyberPolicy, SourceRef: "request-admin", SourceRefHash: adminPolicyFingerprint("incident-admin"), MetadataJSON: `{}`, ObservedAt: incident.ObservedAt}
	if err := db.PersistPromptPolicyIncident(t.Context(), incident, candidate, evidence); err != nil {
		t.Fatalf("PersistPromptPolicyIncident: %v", err)
	}
	second := incident
	second.IncidentID = "incident-admin-second"
	second.RequestCorrelationID = "request-admin-second"
	second.PromptFingerprint = adminPolicyFingerprint("prompt-second")
	second.PromptPreview = "prompt-second"
	second.PromptText = "prompt-second"
	second.ObservedAt = second.ObservedAt.Add(time.Second)
	secondCandidate := candidate
	secondCandidate.Fingerprint = second.PromptFingerprint
	secondEvidence := evidence
	secondEvidence.SourceRef = second.RequestCorrelationID
	secondEvidence.SourceRefHash = adminPolicyFingerprint(second.IncidentID)
	secondEvidence.ObservedAt = second.ObservedAt
	if err := db.PersistPromptPolicyIncident(t.Context(), second, secondCandidate, secondEvidence); err != nil {
		t.Fatalf("PersistPromptPolicyIncident(second): %v", err)
	}

	h := &Handler{db: db}
	router := gin.New()
	router.GET("/api/admin/prompt-policy/incidents", h.ListPromptPolicyIncidents)
	router.GET("/api/admin/prompt-policy/incidents/:incident_id", h.GetPromptPolicyIncident)
	router.DELETE("/api/admin/prompt-policy/incidents/:incident_id", h.DeletePromptPolicyIncident)

	listRecorder := httptest.NewRecorder()
	router.ServeHTTP(listRecorder, httptest.NewRequest(http.MethodGet, "/api/admin/prompt-policy/incidents?local_miss=true&outcome=no_hit&account_id=73&page=2&page_size=1", nil))
	if listRecorder.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", listRecorder.Code, listRecorder.Body.String())
	}
	var list struct {
		Incidents []database.PromptPolicyIncident `json:"incidents"`
		Total     int                             `json:"total"`
	}
	if err := json.Unmarshal(listRecorder.Body.Bytes(), &list); err != nil || list.Total != 2 || len(list.Incidents) != 1 || !list.Incidents[0].LocalMiss || list.Incidents[0].AccountName != "account@example.com" || len(list.Incidents[0].AccountGroupNames) != 1 || list.Incidents[0].RoutingSnapshotState != "event_snapshot" {
		t.Fatalf("list response=%s err=%v", listRecorder.Body.String(), err)
	}

	detailRecorder := httptest.NewRecorder()
	router.ServeHTTP(detailRecorder, httptest.NewRequest(http.MethodGet, "/api/admin/prompt-policy/incidents/incident-admin", nil))
	if detailRecorder.Code != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", detailRecorder.Code, detailRecorder.Body.String())
	}
	var detail struct {
		Incident  database.PromptPolicyIncident `json:"incident"`
		Matches   []map[string]any              `json:"matches"`
		Candidate map[string]any                `json:"candidate"`
		Evidence  map[string]any                `json:"evidence"`
	}
	if err := json.Unmarshal(detailRecorder.Body.Bytes(), &detail); err != nil || detail.Incident.IncidentID != "incident-admin" || len(detail.Matches) != 1 || detail.Candidate == nil || detail.Evidence == nil {
		t.Fatalf("detail response=%s err=%v", detailRecorder.Body.String(), err)
	}
	deleteRecorder := httptest.NewRecorder()
	router.ServeHTTP(deleteRecorder, httptest.NewRequest(http.MethodDelete, "/api/admin/prompt-policy/incidents/incident-admin", nil))
	if deleteRecorder.Code != http.StatusOK {
		t.Fatalf("delete status=%d body=%s", deleteRecorder.Code, deleteRecorder.Body.String())
	}
	missingRecorder := httptest.NewRecorder()
	router.ServeHTTP(missingRecorder, httptest.NewRequest(http.MethodGet, "/api/admin/prompt-policy/incidents/incident-admin", nil))
	if missingRecorder.Code != http.StatusNotFound {
		t.Fatalf("deleted incident status=%d body=%s", missingRecorder.Code, missingRecorder.Body.String())
	}
}

func TestPromptPolicyAuditHealthAPI(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "admin-policy-health.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer db.Close()
	store := auth.NewStore(db, nil, &database.SystemSettings{})
	t.Cleanup(store.Stop)
	cfg := promptfilter.DefaultConfig()
	cfg.Enabled = true
	cfg.Review.Enabled = true
	cfg.Review.APIKey = "health-test-key"
	cfg.Advanced.Enforcement.ConversationLockEnabled = true
	store.SetPromptFilterConfig(cfg)
	h := &Handler{db: db, store: store}
	router := gin.New()
	router.GET("/api/admin/prompt-policy/incidents/health", h.GetPromptPolicyAuditHealth)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/prompt-policy/incidents/health", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("health status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		StorageReady  bool   `json:"storage_ready"`
		Status        string `json:"status"`
		IncidentCount int    `json:"incident_count"`
		ReviewPool    struct {
			Configured int `json:"configured"`
			Available  int `json:"available"`
		} `json:"review_pool"`
		Queue struct {
			DroppedHigh uint64 `json:"dropped_high"`
			Failed      uint64 `json:"failed"`
		} `json:"queue"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || !response.StorageReady || response.Status != "healthy" || response.IncidentCount != 0 || response.ReviewPool.Configured != 1 || response.ReviewPool.Available != 1 || response.Queue.DroppedHigh != 0 || response.Queue.Failed != 0 {
		t.Fatalf("health response=%s err=%v", recorder.Body.String(), err)
	}

	// 管理员主动关闭审查/会话锁属于配置选择,不应报 degraded。
	cfg.Review.Enabled = false
	cfg.Advanced.Enforcement.ConversationLockEnabled = false
	store.SetPromptFilterConfig(cfg)
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/prompt-policy/incidents/health", nil))
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || response.Status != "healthy" {
		t.Fatalf("disabled-feature health response=%s err=%v", recorder.Body.String(), err)
	}

	// 审查启用但 key 池为空才是真实故障。
	cfg.Review.Enabled = true
	cfg.Review.APIKey = ""
	store.SetPromptFilterConfig(cfg)
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/prompt-policy/incidents/health", nil))
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || response.Status != "degraded" {
		t.Fatalf("empty-pool health response=%s err=%v", recorder.Body.String(), err)
	}
}

func TestPromptPolicyIncidentHistoricalRoutingUsesCurrentDirectory(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "admin-policy-routing.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer db.Close()
	store := auth.NewStore(nil, nil, &database.SystemSettings{})
	t.Cleanup(store.Stop)
	account := &auth.Account{DBID: 73, Email: "current-account@example.com", AccessToken: "test-token"}
	store.AddAccount(account)
	store.SetGroupName(4, "打铁")
	store.SetGroupName(5, "示例平台")
	store.ApplyAccountGroups(account.DBID, []int64{4})
	store.SetAPIKeyAllowedGroups(9, []int64{4, 5})

	incident := database.PromptPolicyIncidentInput{
		IncidentID: "incident-routing-legacy", RequestCorrelationID: "request-routing-legacy", Endpoint: "/v1/responses", Model: "gpt-5.4",
		Protocol: "responses", Transport: "sse", StatusCode: 400, AccountID: account.DBID,
		APIKeyID: 9, APIKeyName: "legacy-key", UpstreamErrorCode: "cyber_policy",
		LocalEvaluationState: database.PromptPolicyEvaluationCompleted, LocalOutcome: database.PromptPolicyOutcomeNoHit,
		LocalMatchedPatterns: `[]`, PromptFingerprint: adminPolicyFingerprint("legacy-prompt"), PromptPreview: "legacy-prompt", PromptText: "legacy-prompt",
		PromptAvailable: true, LocalComparison: database.PromptPolicyComparisonConfirmedMiss, ObservedAt: time.Now().UTC(),
	}
	candidate := database.PromptRuleCandidateInput{Fingerprint: incident.PromptFingerprint, Kind: database.PromptRuleCandidateKindEvidence, Source: database.PromptRuleCandidateSourceUpstreamCyberPolicy, SamplePreview: incident.PromptPreview}
	evidence := database.PromptRuleCandidateEvidenceInput{SourceKind: database.PromptRuleCandidateSourceUpstreamCyberPolicy, SourceRef: incident.RequestCorrelationID, SourceRefHash: adminPolicyFingerprint(incident.IncidentID), MetadataJSON: `{}`, ObservedAt: incident.ObservedAt}
	if err := db.PersistPromptPolicyIncident(t.Context(), incident, candidate, evidence); err != nil {
		t.Fatalf("PersistPromptPolicyIncident: %v", err)
	}

	h := &Handler{db: db, store: store}
	router := gin.New()
	router.GET("/api/admin/prompt-policy/incidents", h.ListPromptPolicyIncidents)
	router.GET("/api/admin/prompt-policy/incidents/:incident_id", h.GetPromptPolicyIncident)

	listRecorder := httptest.NewRecorder()
	router.ServeHTTP(listRecorder, httptest.NewRequest(http.MethodGet, "/api/admin/prompt-policy/incidents?page=1&page_size=20", nil))
	if listRecorder.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%s", listRecorder.Code, listRecorder.Body.String())
	}
	var list struct {
		Incidents []database.PromptPolicyIncident `json:"incidents"`
	}
	if err := json.Unmarshal(listRecorder.Body.Bytes(), &list); err != nil || len(list.Incidents) != 1 {
		t.Fatalf("list response=%s err=%v", listRecorder.Body.String(), err)
	}
	got := list.Incidents[0]
	if got.RoutingSnapshotState != "current_inferred" || got.AccountName != account.Email || got.AccountPlatform != database.UpstreamChannelCodex || len(got.AccountGroupNames) != 1 || len(got.APIKeyAllowedGroupNames) != 2 {
		t.Fatalf("historical routing was not enriched: %+v", got)
	}

	detailRecorder := httptest.NewRecorder()
	router.ServeHTTP(detailRecorder, httptest.NewRequest(http.MethodGet, "/api/admin/prompt-policy/incidents/incident-routing-legacy", nil))
	if detailRecorder.Code != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", detailRecorder.Code, detailRecorder.Body.String())
	}
	var detail struct {
		Incident database.PromptPolicyIncident `json:"incident"`
	}
	if err := json.Unmarshal(detailRecorder.Body.Bytes(), &detail); err != nil || detail.Incident.RoutingSnapshotState != "current_inferred" || detail.Incident.AccountName != account.Email {
		t.Fatalf("detail response=%s err=%v", detailRecorder.Body.String(), err)
	}
}
