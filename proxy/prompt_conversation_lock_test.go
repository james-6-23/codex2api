package proxy

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func newPromptConversationLockTestHandler(t *testing.T) (*Handler, *database.DB) {
	t.Helper()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "conversation-lock.db"))
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	cfg := promptGuardTestConfig()
	cfg.Advanced.Enforcement.ConversationLockEnabled = true
	cfg.Advanced.Enforcement.CYBStrikeEnabled = true
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1})
	store.SetPromptFilterConfig(cfg)
	store.ReplacePromptFilterNewAPIBindings([]*database.PromptFilterNewAPIBinding{{
		APIKeyID: 101, PlatformCode: "gateway-a", Secret: "gateway-a-secret", Enabled: true,
		PolicyMode: database.PromptFilterPolicyModeEnforce, PolicyProfile: database.PromptFilterPolicyProfileBalanced,
	}})
	handler := NewHandler(store, db, nil, nil)
	handler.SetRuntimeCache(cache.NewMemory(1))
	return handler, db
}

func signedBoundPromptConversationContextWithRecorder(t *testing.T, requestID string, identity newAPIIdentity, body []byte, fingerprint string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	c, recorder := signedNewAPIPolicyContextWithSecret(t, requestID, identity, "/v1/responses", body, "gateway-a-secret")
	c.Set(contextAPIKeyID, int64(101))
	addSignedNewAPIPolicyMetaWithSecret(t, c, newAPIPolicyMeta{
		PlatformID: "gateway-a", Profile: "balanced", Mode: "enforce",
		Provider: "openai", Protocol: "responses", SessionFingerprint: fingerprint,
	}, true, "gateway-a-secret")
	return c, recorder
}

func TestExplicitUpstreamCYBLocksOnlyTheSignedConversation(t *testing.T) {
	handler, db := newPromptConversationLockTestHandler(t)
	body := []byte(`{"model":"gpt-5.5","input":"ordinary request"}`)
	fingerprint := "0123456789abcdef0123456789abcdef"
	identity := newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}
	first := signedBoundNewAPIPolicyContext(t, "cyb-lock-first", identity, body, 101, "gateway-a", "gateway-a-secret", fingerprint)
	setIngressRequestBodyIfAbsent(first, body)

	handler.logUpstreamCyberPolicy(first, "/v1/responses", "gpt-5.5", []byte(`{"error":{"code":"cyber_policy"}}`))
	metadata := policyDecisionMetadataFromHeaders(first.Writer.Header())
	if metadata.ReasonCode != newAPIUpstreamCyberPolicyReasonCode || !metadata.StrikeEligible {
		t.Fatalf("first CYB metadata = %+v", metadata)
	}
	responseMetadata, delegated := newAPIUpstreamCyberPolicyDecision(first)
	if !delegated || !responseMetadata.ConversationLocked {
		t.Fatalf("first CYB response metadata = %+v delegated=%t", responseMetadata, delegated)
	}
	if message := newAPIPolicyDecisionAPIError(responseMetadata).Message; message != upstreamCyberPolicyLockedUserMessage || !strings.Contains(message, "再次触发可能会停用账号") {
		t.Fatalf("first locked CYB message = %q", message)
	}
	policyContext, verified := handler.verifyNewAPIPolicyContext(first, handler.promptFilterConfigForRequest(first).Advanced.NewAPI, body)
	lockIdentity, ok := verifiedPromptConversationLockIdentity(first, policyContext)
	if !verified || !ok {
		t.Fatal("signed session identity was not available for conversation lock")
	}
	lock, err := db.GetActivePromptConversationLock(t.Context(), lockIdentity.LockKey)
	if err != nil || lock.TriggerCount != 1 || lock.DecisionID != metadata.DecisionID {
		t.Fatalf("active lock = %#v err=%v", lock, err)
	}

	repeat, repeatRecorder := signedBoundPromptConversationContextWithRecorder(t, "cyb-lock-repeat", identity, body, fingerprint)
	setIngressRequestBodyIfAbsent(repeat, body)
	if blocked := handler.inspectPromptFilterOpenAI(repeat, body, "/v1/responses", "gpt-5.5"); !blocked {
		t.Fatal("locked conversation was forwarded")
	}
	repeatMetadata := policyDecisionMetadataFromHeaders(repeat.Writer.Header())
	if repeatMetadata.ReasonCode != promptConversationLockedReasonCode || repeatMetadata.StrikeEligible {
		t.Fatalf("locked retry metadata = %+v", repeatMetadata)
	}
	if message := newAPIPolicyDecisionAPIError(repeatMetadata).Message; !strings.Contains(message, "不会重复累计") || !strings.Contains(message, "再次触发 CYB 可能会停用账号") {
		t.Fatalf("locked retry message = %q", message)
	}
	if got := gjson.GetBytes(repeatRecorder.Body.Bytes(), "error.code").String(); got != promptConversationLockedReasonCode {
		t.Fatalf("locked retry error code=%q body=%s", got, repeatRecorder.Body.String())
	}
	if got := gjson.GetBytes(repeatRecorder.Body.Bytes(), "error.details.restriction_scope").String(); got != database.PromptConversationRestrictionScopeConversation {
		t.Fatalf("locked retry scope=%q body=%s", got, repeatRecorder.Body.String())
	}
	if got := gjson.GetBytes(repeatRecorder.Body.Bytes(), "error.details.retry_after_seconds").Int(); got <= 0 {
		t.Fatalf("locked retry remaining=%d body=%s", got, repeatRecorder.Body.String())
	}
	if got := gjson.GetBytes(repeatRecorder.Body.Bytes(), "error.details.audit_reference").String(); got == "" || got != lock.IncidentID {
		t.Fatalf("locked retry audit reference=%q incident=%q body=%s", got, lock.IncidentID, repeatRecorder.Body.String())
	}
	if retryAfter := repeat.Writer.Header().Get("Retry-After"); retryAfter == "" {
		t.Fatalf("locked retry missing Retry-After header: %v", repeat.Writer.Header())
	}
	if message := gjson.GetBytes(repeatRecorder.Body.Bytes(), "error.message").String(); !strings.Contains(message, "会话详情") || !strings.Contains(message, promptConversationLockedReasonCode) {
		t.Fatalf("locked retry actionable message=%q", message)
	}
	lock, err = db.GetActivePromptConversationLock(t.Context(), lockIdentity.LockKey)
	if err != nil || lock.TriggerCount != 1 {
		t.Fatalf("locked retry changed CYB count: lock=%#v err=%v", lock, err)
	}

	otherFingerprint := "fedcba9876543210fedcba9876543210"
	other := signedBoundNewAPIPolicyContext(t, "cyb-lock-other", newAPIIdentity{UserID: "43", ClientIP: "203.0.113.9"}, body, 101, "gateway-a", "gateway-a-secret", otherFingerprint)
	setIngressRequestBodyIfAbsent(other, body)
	if blocked := handler.inspectPromptFilterOpenAI(other, body, "/v1/responses", "gpt-5.5"); blocked {
		t.Fatal("different user was blocked by another user's CYB lock")
	}
}

func TestContinuousRetryCatchAllRetainsCYBLockAndUserCooldown(t *testing.T) {
	previousRuntime := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previousRuntime) })
	nextRuntime := previousRuntime
	nextRuntime.ContinuousRetryPolicy = database.ContinuousRetryPolicy{Enabled: true, CatchAll: true}
	ApplyRuntimeSettings(nextRuntime)

	handler, db := newPromptConversationLockTestHandler(t)
	body := []byte(`{"model":"gpt-5.5","input":"ordinary request"}`)
	fingerprint := "0123456789abcdef0123456789abcdef"
	c := signedBoundNewAPIPolicyContext(t, "catch-all-cyb-no-lock", newAPIIdentity{
		UserID: "42", ClientIP: "203.0.113.8",
	}, body, 101, "gateway-a", "gateway-a-secret", fingerprint)
	setIngressRequestBodyIfAbsent(c, body)

	_, accepted := handler.logUpstreamCyberPolicy(c, "/v1/responses", "gpt-5.5", []byte(`{"error":{"code":"cyber_policy"}}`))
	metadata, delegated := newAPIUpstreamCyberPolicyDecision(c)
	if !delegated || !metadata.ConversationLocked {
		t.Fatalf("catch-all CYB decision = %+v delegated=%t", metadata, delegated)
	}
	if !accepted {
		t.Fatal("catch-all CYB was not retained in the local audit queue")
	}
	policyContext, verified := handler.verifyNewAPIPolicyContext(c, handler.promptFilterConfigForRequest(c).Advanced.NewAPI, body)
	lockIdentity, ok := verifiedPromptConversationLockIdentity(c, policyContext)
	if !verified || !ok {
		t.Fatal("signed session identity was not available")
	}
	if lock, err := db.GetActivePromptConversationLock(t.Context(), lockIdentity.LockKey); err != nil || lock.NewAPIUserID != "42" {
		t.Fatalf("catch-all CYB conversation lock = %#v err=%v", lock, err)
	}
	cooldown, exact, err := db.GetActivePromptConversationRestriction(t.Context(), "", "gateway-a", "42", 24*time.Hour, 30*time.Minute)
	if err != nil || exact || cooldown.NewAPIUserID != "42" {
		t.Fatalf("catch-all CYB user cooldown = %#v exact=%t err=%v", cooldown, exact, err)
	}
}

func TestPromptCyberRestrictionDerivesAuditReferenceForLegacyLocalLock(t *testing.T) {
	requestID := "298ee1bb-ad0f-4e96-8924-d34066def71e"
	restriction := promptCyberRestrictionDecision(&database.PromptConversationLock{
		DecisionID: "local-block:" + requestID,
		ReasonCode: "terminal_policy_match",
		LockedAt:   time.Now().UTC(),
	}, promptfilter.DefaultConfig())
	if restriction.AuditReference != requestID {
		t.Fatalf("audit reference = %q, want %q", restriction.AuditReference, requestID)
	}
	if !strings.Contains(restriction.Message, "审计编号："+requestID) {
		t.Fatalf("restriction message lacks audit reference: %q", restriction.Message)
	}
	if !strings.Contains(restriction.Message, "本地高风险规则") || strings.Contains(restriction.Message, "因上游 CYB 已锁定") {
		t.Fatalf("local restriction origin is misleading: %q", restriction.Message)
	}
	details := promptCyberRestrictionDetails(restriction, nil)
	if details["audit_reference"] != requestID || details["decision_id"] != "local-block:"+requestID || details["trigger_reason_code"] != "terminal_policy_match" {
		t.Fatalf("restriction details = %#v", details)
	}
}

func TestUpstreamCYBCoolsVerifiedUserAcrossSessionChurn(t *testing.T) {
	handler, _ := newPromptConversationLockTestHandler(t)
	cfg := handler.store.GetPromptFilterConfig()
	cfg.Advanced.Enforcement.UserCyberCooldownMinutes = 7
	handler.store.SetPromptFilterConfig(cfg)
	body := []byte(`{"model":"gpt-5.5","input":"ordinary request"}`)
	identity := newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}
	first := signedBoundNewAPIPolicyContext(t, "cyb-cooldown-first", identity, body, 101, "gateway-a", "gateway-a-secret", "0123456789abcdef0123456789abcdef")
	setIngressRequestBodyIfAbsent(first, body)
	handler.logUpstreamCyberPolicy(first, "/v1/responses", "gpt-5.5", []byte(`{"error":{"code":"cyber_policy"}}`))

	churned, churnedRecorder := signedBoundPromptConversationContextWithRecorder(t, "cyb-cooldown-churned", identity, body, "fedcba9876543210fedcba9876543210")
	setIngressRequestBodyIfAbsent(churned, body)
	if blocked := handler.inspectPromptFilterOpenAI(churned, body, "/v1/responses", "gpt-5.5"); !blocked {
		t.Fatal("verified user bypassed CYB cooldown by changing session fingerprint")
	}
	metadata := policyDecisionMetadataFromHeaders(churned.Writer.Header())
	if metadata.ReasonCode != promptUserCyberCooldownReasonCode || metadata.StrikeEligible {
		t.Fatalf("user cooldown metadata = %+v", metadata)
	}
	if message := newAPIPolicyDecisionAPIError(metadata).Message; !strings.Contains(message, "安全冷却期") || strings.Contains(message, "30 分钟") || !strings.Contains(message, "不会重复累计处罚") {
		t.Fatalf("user cooldown message = %q", message)
	}
	if got := gjson.GetBytes(churnedRecorder.Body.Bytes(), "error.code").String(); got != promptUserCyberCooldownReasonCode {
		t.Fatalf("user cooldown error code=%q body=%s", got, churnedRecorder.Body.String())
	}
	if got := gjson.GetBytes(churnedRecorder.Body.Bytes(), "error.details.restriction_scope").String(); got != database.PromptConversationRestrictionScopeUserCooldown {
		t.Fatalf("user cooldown scope=%q body=%s", got, churnedRecorder.Body.String())
	}
	cooldownTTL := promptUserCyberCooldownTTL(handler.store.GetPromptFilterConfig())
	if cooldownTTL != 7*time.Minute {
		t.Fatalf("configured user cooldown TTL = %s, want 7m", cooldownTTL)
	}
	if got := gjson.GetBytes(churnedRecorder.Body.Bytes(), "error.details.retry_after_seconds").Int(); got <= 0 || got > int64(cooldownTTL/time.Second) {
		t.Fatalf("user cooldown remaining=%d body=%s", got, churnedRecorder.Body.String())
	}
	if message := gjson.GetBytes(churnedRecorder.Body.Bytes(), "error.message").String(); !strings.Contains(message, "用户详情") || !strings.Contains(message, promptUserCyberCooldownReasonCode) {
		t.Fatalf("user cooldown actionable message=%q", message)
	}
}

func TestUpstreamCYBCoolsVerifiedUserWithoutSessionFingerprint(t *testing.T) {
	handler, db := newPromptConversationLockTestHandler(t)
	body := []byte(`{"model":"gpt-5.5","input":"ordinary request"}`)
	identity := newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}
	first := signedBoundNewAPIPolicyContext(t, "cyb-cooldown-no-session-first", identity, body, 101, "gateway-a", "gateway-a-secret", "")
	setIngressRequestBodyIfAbsent(first, body)
	handler.logUpstreamCyberPolicy(first, "/v1/responses", "gpt-5.5", []byte(`{"error":{"code":"cyber_policy"}}`))

	metadata, delegated := newAPIUpstreamCyberPolicyDecision(first)
	if !delegated || metadata.ConversationLocked {
		t.Fatalf("CYB without session response metadata = %+v delegated=%t", metadata, delegated)
	}
	item, exact, err := db.GetActivePromptConversationRestriction(t.Context(), "", "gateway-a", "42", 24*time.Hour, 30*time.Minute)
	if err != nil || exact || item.NewAPIUserID != "42" || item.SessionFingerprint != "" || item.SessionHash != "" {
		t.Fatalf("sessionless user cooldown = %#v exact=%t err=%v", item, exact, err)
	}

	repeat := signedBoundNewAPIPolicyContext(t, "cyb-cooldown-no-session-repeat", identity, body, 101, "gateway-a", "gateway-a-secret", "")
	setIngressRequestBodyIfAbsent(repeat, body)
	if blocked := handler.inspectPromptFilterOpenAI(repeat, body, "/v1/responses", "gpt-5.5"); !blocked {
		t.Fatal("verified user without session bypassed CYB cooldown")
	}
	repeatMetadata := policyDecisionMetadataFromHeaders(repeat.Writer.Header())
	if repeatMetadata.ReasonCode != promptUserCyberCooldownReasonCode || repeatMetadata.StrikeEligible {
		t.Fatalf("sessionless user cooldown metadata = %+v", repeatMetadata)
	}

	withSession := signedBoundNewAPIPolicyContext(t, "cyb-cooldown-session-after-sessionless", identity, body, 101, "gateway-a", "gateway-a-secret", "0123456789abcdef0123456789abcdef")
	setIngressRequestBodyIfAbsent(withSession, body)
	if blocked := handler.inspectPromptFilterOpenAI(withSession, body, "/v1/responses", "gpt-5.5"); !blocked {
		t.Fatal("verified user escaped sessionless CYB cooldown by later adding a session fingerprint")
	}
}

func TestConfiguredUserCyberCooldownExpiresAcrossSessionChurn(t *testing.T) {
	handler, db := newPromptConversationLockTestHandler(t)
	cfg := handler.store.GetPromptFilterConfig()
	cfg.Advanced.Enforcement.UserCyberCooldownMinutes = 1
	handler.store.SetPromptFilterConfig(cfg)

	body := []byte(`{"model":"gpt-5.5","input":"ordinary request"}`)
	identity := newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}
	first := signedBoundNewAPIPolicyContext(t, "cyb-short-cooldown-first", identity, body, 101, "gateway-a", "gateway-a-secret", "")
	requestCfg := handler.promptFilterConfigForRequest(first)
	policyContext, verified := handler.verifyNewAPIPolicyContext(first, requestCfg.Advanced.NewAPI, body)
	lockIdentity, ok := verifiedPromptUserCooldownIdentity(first, policyContext)
	if !verified || !ok {
		t.Fatal("signed user identity unavailable")
	}
	if _, _, err := db.LockPromptConversation(t.Context(), database.PromptConversationLockInput{
		LockKey: lockIdentity.LockKey, Platform: lockIdentity.Platform, NewAPIUserID: lockIdentity.NewAPIUserID,
		IncidentID: "incident-short-cooldown", DecisionID: "decision-short-cooldown", ReasonCode: newAPIUpstreamCyberPolicyReasonCode,
		LockedAt: time.Now().UTC().Add(-2 * time.Minute),
	}); err != nil {
		t.Fatalf("LockPromptConversation: %v", err)
	}

	repeat := signedBoundNewAPIPolicyContext(t, "cyb-short-cooldown-repeat", identity, body, 101, "gateway-a", "gateway-a-secret", "fedcba9876543210fedcba9876543210")
	setIngressRequestBodyIfAbsent(repeat, body)
	if blocked := handler.inspectPromptFilterOpenAI(repeat, body, "/v1/responses", "gpt-5.5"); blocked {
		t.Fatal("expired configured user cooldown still blocked a new session")
	}
}

func TestSessionlessUserCooldownBlocksConcurrentRetriesWithoutNewCYIncidents(t *testing.T) {
	handler, db := newPromptConversationLockTestHandler(t)
	body := []byte(`{"model":"gpt-5.5","input":"ordinary request"}`)
	identity := newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}
	first := signedBoundNewAPIPolicyContext(t, "cyb-sessionless-concurrent-first", identity, body, 101, "gateway-a", "gateway-a-secret", "")
	setIngressRequestBodyIfAbsent(first, body)
	handler.logUpstreamCyberPolicy(first, "/v1/responses", "gpt-5.5", []byte(`{"error":{"code":"cyber_policy"}}`))
	waitPromptFilterAuditIdle(t, db)

	_, incidentTotalBefore, err := db.ListPromptPolicyIncidentsPage(t.Context(), database.PromptPolicyIncidentQuery{Page: 1, PageSize: 10})
	if err != nil || incidentTotalBefore != 1 {
		t.Fatalf("initial CY incidents total=%d err=%v", incidentTotalBefore, err)
	}

	const retries = 64
	requests := make([]*gin.Context, 0, retries)
	for index := 0; index < retries; index++ {
		request := signedBoundNewAPIPolicyContext(t, "cyb-sessionless-concurrent-"+strconv.Itoa(index), identity, body, 101, "gateway-a", "gateway-a-secret", "")
		setIngressRequestBodyIfAbsent(request, body)
		requests = append(requests, request)
	}
	var blocked atomic.Int64
	var wg sync.WaitGroup
	for _, request := range requests {
		wg.Add(1)
		go func(c *gin.Context) {
			defer wg.Done()
			if handler.inspectPromptFilterOpenAI(c, body, "/v1/responses", "gpt-5.5") {
				blocked.Add(1)
			}
		}(request)
	}
	wg.Wait()
	if got := blocked.Load(); got != retries {
		t.Fatalf("blocked concurrent retries=%d, want %d", got, retries)
	}
	waitPromptFilterAuditIdle(t, db)
	_, incidentTotalAfter, err := db.ListPromptPolicyIncidentsPage(t.Context(), database.PromptPolicyIncidentQuery{Page: 1, PageSize: 10})
	if err != nil || incidentTotalAfter != incidentTotalBefore {
		t.Fatalf("concurrent retries created new CY incidents: before=%d after=%d err=%v", incidentTotalBefore, incidentTotalAfter, err)
	}
}

func TestConversationLockRequiresStableSignedFingerprintAndCanBeDisabled(t *testing.T) {
	handler, db := newPromptConversationLockTestHandler(t)
	body := []byte(`{"model":"gpt-5.5","input":"ordinary request"}`)
	identity := newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}
	withoutSession := signedBoundNewAPIPolicyContext(t, "cyb-no-session", identity, body, 101, "gateway-a", "gateway-a-secret", "")
	setIngressRequestBodyIfAbsent(withoutSession, body)
	handler.logUpstreamCyberPolicy(withoutSession, "/v1/responses", "gpt-5.5", []byte(`{"error":{"code":"cyber_policy"}}`))
	if _, err := db.GetActivePromptConversationLockBySessionHash(t.Context(), "anything"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("CYB without signed session created a lock: %v", err)
	}
	withoutSessionMetadata, delegated := newAPIUpstreamCyberPolicyDecision(withoutSession)
	if !delegated || withoutSessionMetadata.ConversationLocked {
		t.Fatalf("CYB without session response metadata = %+v delegated=%t", withoutSessionMetadata, delegated)
	}
	if message := newAPIPolicyDecisionAPIError(withoutSessionMetadata).Message; message != upstreamCyberPolicyUserMessage || strings.Contains(message, "已锁定当前对话") || !strings.Contains(message, "再次触发可能会停用账号") {
		t.Fatalf("CYB without session message = %q", message)
	}

	cfg := handler.store.GetPromptFilterConfig()
	cfg.Advanced.Enforcement.ConversationLockEnabled = false
	handler.store.SetPromptFilterConfig(cfg)
	fingerprint := "0123456789abcdef0123456789abcdef"
	disabled := signedBoundNewAPIPolicyContext(t, "cyb-lock-disabled", identity, body, 101, "gateway-a", "gateway-a-secret", fingerprint)
	setIngressRequestBodyIfAbsent(disabled, body)
	handler.logUpstreamCyberPolicy(disabled, "/v1/responses", "gpt-5.5", []byte(`{"error":{"code":"cyber_policy"}}`))
	if _, err := db.GetActivePromptConversationLockBySessionHash(t.Context(), hashRiskIdentity(fingerprint)); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("disabled conversation-lock feature created a lock: %v", err)
	}
}

func TestUnsignedUpstreamCYUsesScopedFingerprintReplayLock(t *testing.T) {
	handler, db := newPromptConversationLockTestHandler(t)
	body := promptRequestBody(t, "repeat this exact request")
	fingerprint := promptfilter.PromptEvidenceFingerprint("repeat this exact request")

	newUnsigned := func(keyID int64, remoteAddr string, requestBody []byte) *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		c.Request.RemoteAddr = remoteAddr
		c.Set(contextAPIKeyID, keyID)
		setRawRequestBody(c, requestBody)
		setIngressRequestBodyIfAbsent(c, requestBody)
		return c
	}

	first := newUnsigned(101, "198.51.100.9:1234", body)
	if _, accepted := handler.logUpstreamCyberPolicy(first, "/v1/responses", "gpt-5.5", []byte(`{"error":{"code":"cyber_policy"}}`)); !accepted {
		t.Fatal("unsigned upstream CY evidence was not accepted")
	}
	lock, err := db.GetActivePromptConversationLockBySessionHash(t.Context(), fingerprint)
	if err != nil {
		t.Fatalf("fingerprint replay lock was not persisted: %v", err)
	}
	if lock.IdentityKind != "fingerprint_replay" || lock.Platform != "codex-fingerprint" {
		t.Fatalf("unexpected fingerprint lock identity: %+v", lock)
	}
	if !strings.Contains(lock.NewAPIUserID, "apikey:101") {
		t.Fatalf("fingerprint lock subject leaked beyond the API key scope: %q", lock.NewAPIUserID)
	}

	repeat := newUnsigned(101, "198.51.100.9:1234", body)
	if _, locked := handler.activePromptConversationLock(repeat, handler.promptFilterConfigForRequest(repeat), body, "/v1/responses", "gpt-5.5"); !locked {
		t.Fatal("same API key, IP, and fingerprint did not activate replay cooldown")
	}

	differentIP := newUnsigned(101, "198.51.100.10:1234", body)
	if _, locked := handler.activePromptConversationLock(differentIP, handler.promptFilterConfigForRequest(differentIP), body, "/v1/responses", "gpt-5.5"); locked {
		t.Fatal("fingerprint replay cooldown leaked across client IPs")
	}
	differentKey := newUnsigned(102, "198.51.100.9:1234", body)
	if _, locked := handler.activePromptConversationLock(differentKey, handler.promptFilterConfigForRequest(differentKey), body, "/v1/responses", "gpt-5.5"); locked {
		t.Fatal("fingerprint replay cooldown leaked across API keys")
	}
	differentPrompt := newUnsigned(101, "198.51.100.9:1234", promptRequestBody(t, "a different request"))
	if _, locked := handler.activePromptConversationLock(differentPrompt, handler.promptFilterConfigForRequest(differentPrompt), ingressRequestBody(differentPrompt, nil), "/v1/responses", "gpt-5.5"); locked {
		t.Fatal("fingerprint replay cooldown leaked across prompt fingerprints")
	}
}

// 指纹提取按端点协议分派(images 读 prompt,responses 读 messages)。检查路径
// 必须携带真实端点:曾经硬编码 /v1/responses,导致 images 端点触发的冷却锁
// 建得出来却永远命中不了(静默失效)。
func TestFingerprintReplayCooldownCoversImagesEndpoint(t *testing.T) {
	handler, db := newPromptConversationLockTestHandler(t)
	body := []byte(`{"model":"gpt-image-1","prompt":"repeat this exact image request"}`)

	newUnsigned := func(remoteAddr string, requestBody []byte) *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
		c.Request.RemoteAddr = remoteAddr
		c.Set(contextAPIKeyID, 101)
		setRawRequestBody(c, requestBody)
		setIngressRequestBodyIfAbsent(c, requestBody)
		return c
	}

	first := newUnsigned("198.51.100.9:1234", body)
	if _, accepted := handler.logUpstreamCyberPolicy(first, "/v1/images/generations", "gpt-image-1", []byte(`{"error":{"code":"cyber_policy"}}`)); !accepted {
		t.Fatal("unsigned upstream CY evidence on images endpoint was not accepted")
	}
	fingerprint := promptfilter.PromptEvidenceFingerprint("repeat this exact image request")
	if lock, err := db.GetActivePromptConversationLockBySessionHash(t.Context(), fingerprint); err != nil || lock.IdentityKind != "fingerprint_replay" {
		t.Fatalf("fingerprint replay lock was not persisted for images endpoint: %+v err=%v", lock, err)
	}

	repeat := newUnsigned("198.51.100.9:1234", body)
	if _, locked := handler.activePromptConversationLock(repeat, handler.promptFilterConfigForRequest(repeat), body, "/v1/images/generations", "gpt-image-1"); !locked {
		t.Fatal("images-endpoint replay was not caught by the cooldown (endpoint mismatch regression)")
	}
}

func TestConversationLockStorageFailureDoesNotClaimConversationWasLocked(t *testing.T) {
	handler, _ := newPromptConversationLockTestHandler(t)
	body := []byte(`{"model":"gpt-5.5","input":"ordinary request"}`)
	c := signedBoundNewAPIPolicyContext(t, "cyb-lock-db-failure", newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}, body, 101, "gateway-a", "gateway-a-secret", "0123456789abcdef0123456789abcdef")
	setIngressRequestBodyIfAbsent(c, body)
	metadata, delegated := handler.emitNewAPIUpstreamCyberPolicyDecision(c, "/v1/responses", "gpt-5.5", []byte(`{"error":{"code":"cyber_policy"}}`))
	if !delegated {
		t.Fatal("signed CYB decision was not emitted")
	}
	canceledContext, cancel := context.WithCancel(c.Request.Context())
	cancel()
	c.Request = c.Request.WithContext(canceledContext)
	metadata.ConversationLocked = handler.lockPromptConversationAfterUpstreamCYB(c, "/v1/responses", "gpt-5.5", "incident-db-failure", metadata)
	if metadata.ConversationLocked {
		t.Fatal("database failure was reported as a successful conversation lock")
	}
	if message := newAPIPolicyDecisionAPIError(metadata).Message; message != upstreamCyberPolicyUserMessage || strings.Contains(message, "已锁定当前对话") {
		t.Fatalf("database failure CYB message = %q", message)
	}
}

func TestExpiredConversationLockDoesNotBlockSignedConversation(t *testing.T) {
	handler, db := newPromptConversationLockTestHandler(t)
	cfg := handler.store.GetPromptFilterConfig()
	cfg.Advanced.Enforcement.ConversationLockTTLHours = 1
	handler.store.SetPromptFilterConfig(cfg)
	body := []byte(`{"model":"gpt-5.5","input":"ordinary request"}`)
	fingerprint := "0123456789abcdef0123456789abcdef"
	c := signedBoundNewAPIPolicyContext(t, "expired-lock", newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}, body, 101, "gateway-a", "gateway-a-secret", fingerprint)
	setIngressRequestBodyIfAbsent(c, body)
	requestConfig := handler.promptFilterConfigForRequest(c)
	policyContext, verified := handler.verifyNewAPIPolicyContext(c, requestConfig.Advanced.NewAPI, body)
	identity, ok := verifiedPromptConversationLockIdentity(c, policyContext)
	if !verified || !ok {
		t.Fatal("signed lock identity unavailable")
	}
	if _, _, err := db.LockPromptConversation(t.Context(), database.PromptConversationLockInput{
		LockKey: identity.LockKey, Platform: identity.Platform, NewAPIUserID: identity.NewAPIUserID,
		SessionFingerprint: identity.SessionFingerprint, SessionHash: identity.SessionHash,
		IncidentID: "incident-expired", DecisionID: "decision-expired", ReasonCode: newAPIUpstreamCyberPolicyReasonCode,
		LockedAt: time.Now().UTC().Add(-2 * time.Hour),
	}); err != nil {
		t.Fatalf("LockPromptConversation: %v", err)
	}
	if blocked := handler.inspectPromptFilterOpenAI(c, body, "/v1/responses", "gpt-5.5"); blocked {
		t.Fatal("expired conversation lock blocked request")
	}
	if _, err := db.GetActivePromptConversationLockWithTTL(t.Context(), identity.LockKey, time.Hour); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("expired conversation lock remained active: %v", err)
	}
}

func TestSignedConversationLockSeparatesNewAPIChannels(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	base := verifiedNewAPIPolicyContext{
		Platform:     "gateway-a",
		Identity:     newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"},
		MetaVerified: true,
		Meta:         newAPIPolicyMeta{SessionFingerprint: "0123456789abcdef0123456789abcdef"},
	}
	first := base
	first.Meta.ChannelID = 1001
	second := base
	second.Meta.ChannelID = 1002
	firstIdentity, ok := verifiedPromptConversationLockIdentity(c, first)
	if !ok {
		t.Fatal("first channel lock identity unavailable")
	}
	secondIdentity, ok := verifiedPromptConversationLockIdentity(c, second)
	if !ok {
		t.Fatal("second channel lock identity unavailable")
	}
	if firstIdentity.LockKey == secondIdentity.LockKey {
		t.Fatalf("signed channels share conversation lock key: %q", firstIdentity.LockKey)
	}
}

// 纯外部审核命中(如 moderation 分类阈值越线)与 fail-closed 审核失败没有本地
// 检测证据,只应拒绝当次请求;本地规则命中才升级为会话锁(issue #527)。
func TestReviewOnlyBlockDoesNotLockConversation(t *testing.T) {
	handler, db := newPromptConversationLockTestHandler(t)
	body := []byte(`{"model":"gpt-5.5","input":"ordinary request"}`)
	fingerprint := "fedcba9876543210fedcba9876543210"
	identity := newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}

	reviewOnlyVerdict := promptfilter.Verdict{
		Enabled: true, Action: promptfilter.ActionBlock,
		Reviewed: true, ReviewFlagged: true, ReviewModel: "omni-moderation-latest",
	}
	reviewOnlyDecision := finalizePromptGuardDecision(promptfilter.Decision{
		Enabled: true, Mode: promptfilter.GuardModeEnforce, Action: promptfilter.ActionBlock,
	}, reviewOnlyVerdict)
	if reviewOnlyDecision.PrimaryDetector != promptGuardDetectorExternalReview {
		t.Fatalf("review-only decision detector = %q", reviewOnlyDecision.PrimaryDetector)
	}
	reviewOnly := signedBoundNewAPIPolicyContext(t, "review-only-block", identity, body, 101, "gateway-a", "gateway-a-secret", fingerprint)
	setIngressRequestBodyIfAbsent(reviewOnly, body)
	cfg := handler.promptFilterConfigForRequest(reviewOnly)
	if handler.lockPromptConversationOnLocalBlock(reviewOnly, cfg, body, "/v1/responses", "gpt-5.5", reviewOnlyDecision, reviewOnlyVerdict) {
		t.Fatal("review-only block claimed to lock the conversation")
	}
	if _, err := db.GetActivePromptConversationLockBySessionHash(t.Context(), hashRiskIdentity(fingerprint)); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("review-only block created a conversation lock: %v", err)
	}

	failClosedVerdict := promptfilter.Verdict{
		Enabled: true, Action: promptfilter.ActionBlock,
		Reviewed: true, ReviewError: "review request failed: status 500",
	}
	failClosed := signedBoundNewAPIPolicyContext(t, "review-fail-closed", identity, body, 101, "gateway-a", "gateway-a-secret", fingerprint)
	setIngressRequestBodyIfAbsent(failClosed, body)
	if handler.lockPromptConversationOnLocalBlock(failClosed, cfg, body, "/v1/responses", "gpt-5.5", promptfilter.Decision{Enabled: true}, failClosedVerdict) {
		t.Fatal("fail-closed review error claimed to lock the conversation")
	}
	if _, err := db.GetActivePromptConversationLockBySessionHash(t.Context(), hashRiskIdentity(fingerprint)); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("fail-closed review error created a conversation lock: %v", err)
	}

	localVerdict := promptfilter.Verdict{
		Enabled: true, Action: promptfilter.ActionBlock,
		Matched: []promptfilter.Match{{Name: "terminal-rule", Weight: 100, Category: "intrusion"}},
	}
	localDecision := finalizePromptGuardDecision(promptfilter.Decision{
		Enabled: true, Mode: promptfilter.GuardModeEnforce, Action: promptfilter.ActionBlock,
		ReasonCode: "prompt_policy_match",
	}, localVerdict)
	local := signedBoundNewAPIPolicyContext(t, "local-rule-block", identity, body, 101, "gateway-a", "gateway-a-secret", fingerprint)
	setIngressRequestBodyIfAbsent(local, body)
	if !handler.lockPromptConversationOnLocalBlock(local, cfg, body, "/v1/responses", "gpt-5.5", localDecision, localVerdict) {
		t.Fatal("local rule block failed to lock the conversation")
	}
	item, err := db.GetActivePromptConversationLockBySessionHash(t.Context(), hashRiskIdentity(fingerprint))
	if err != nil {
		t.Fatalf("local rule block did not persist a conversation lock: %v", err)
	}
	if item.ReasonCode != "prompt_policy_match" {
		t.Fatalf("lock reason = %q", item.ReasonCode)
	}
}
