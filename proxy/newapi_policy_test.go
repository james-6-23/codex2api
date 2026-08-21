package proxy

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestVerifyNewAPIIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := promptGuardTestConfig()
	cfg.Advanced.NewAPI.Enabled = true
	cfg.Advanced.NewAPI.MaxClockSkewSeconds = 120
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	req := httptest.NewRequest("POST", "/v1/responses", nil)
	req.Header.Set("X-NewAPI-User-ID", "42")
	req.Header.Set("X-NewAPI-Client-IP", "203.0.113.8")
	req.Header.Set("X-NewAPI-Request-ID", "req-test")
	req.Header.Set("X-NewAPI-Timestamp", timestamp)
	bodyDigest := sha256.Sum256(nil)
	bodyDigestHex := hex.EncodeToString(bodyDigest[:])
	req.Header.Set("X-NewAPI-Method", "POST")
	req.Header.Set("X-NewAPI-Path", "/v1/responses")
	req.Header.Set("X-NewAPI-Body-SHA256", bodyDigestHex)
	canonical := strings.Join([]string{"v1", timestamp, "req-test", "42", "203.0.113.8", "POST", "/v1/responses", bodyDigestHex}, "\n")
	mac := hmac.New(sha256.New, []byte("integration-secret"))
	_, _ = mac.Write([]byte(canonical))
	req.Header.Set("X-NewAPI-Signature", hex.EncodeToString(mac.Sum(nil)))
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = req
	c.Set(contextAPIKeyID, int64(101))
	h := newPromptGuardTestHandler(cfg)
	identity, ok := h.verifyNewAPIIdentity(c, promptfilter.NewAPIConfig{Enabled: true, MaxClockSkewSeconds: 120}, nil)
	if !ok || identity.UserID != "42" || identity.ClientIP != "203.0.113.8" {
		t.Fatalf("verification failed: %#v %v", identity, ok)
	}
	if cached, ok := h.verifyNewAPIIdentity(c, promptfilter.NewAPIConfig{Enabled: true, MaxClockSkewSeconds: 120}, nil); !ok || cached != identity {
		t.Fatal("verified identity was not reusable inside the same request")
	}
	replayRecorder := httptest.NewRecorder()
	replayContext, _ := gin.CreateTestContext(replayRecorder)
	replayContext.Request = req.Clone(req.Context())
	replayContext.Set(contextAPIKeyID, int64(101))
	if _, ok := h.verifyNewAPIIdentity(replayContext, promptfilter.NewAPIConfig{Enabled: true, MaxClockSkewSeconds: 120}, nil); ok {
		t.Fatal("replayed request ID from a new request was accepted")
	}

	// A fresh request ID with a signature for a different body must be rejected.
	req.Header.Set("X-NewAPI-Request-ID", "req-body-tamper")
	canonical = strings.Join([]string{"v1", timestamp, "req-body-tamper", "42", "203.0.113.8", "POST", "/v1/responses", bodyDigestHex}, "\n")
	mac = hmac.New(sha256.New, []byte("integration-secret"))
	_, _ = mac.Write([]byte(canonical))
	req.Header.Set("X-NewAPI-Signature", hex.EncodeToString(mac.Sum(nil)))
	tamperedBodyContext, _ := gin.CreateTestContext(httptest.NewRecorder())
	tamperedBodyContext.Request = req.Clone(req.Context())
	tamperedBodyContext.Set(contextAPIKeyID, int64(101))
	if _, ok := h.verifyNewAPIIdentity(tamperedBodyContext, promptfilter.NewAPIConfig{Enabled: true, MaxClockSkewSeconds: 120}, []byte("tampered")); ok {
		t.Fatal("tampered body was accepted")
	}

	req.Header.Set("X-NewAPI-User-ID", "43")
	tamperedIdentityContext, _ := gin.CreateTestContext(httptest.NewRecorder())
	tamperedIdentityContext.Request = req.Clone(req.Context())
	tamperedIdentityContext.Set(contextAPIKeyID, int64(101))
	if _, ok := h.verifyNewAPIIdentity(tamperedIdentityContext, promptfilter.NewAPIConfig{Enabled: true, MaxClockSkewSeconds: 120}, nil); ok {
		t.Fatal("tampered identity was accepted")
	}
}

func TestVerifyNewAPIIdentityRejectsUnsupportedSignatureVersion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"gpt-5.5","input":"hello"}`)
	c, _ := signedNewAPIPolicyContext(t, "req-unsupported-version", newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}, "/v1/responses", body)
	c.Request.Header.Set("X-NewAPI-Signature-Version", "unsupported")
	cfg := promptGuardTestConfig()
	cfg.Advanced.NewAPI.Enabled = true
	cfg.Advanced.NewAPI.MaxClockSkewSeconds = 120
	h := newPromptGuardTestHandler(cfg)
	if _, ok := h.verifyNewAPIIdentity(c, promptfilter.NewAPIConfig{Enabled: true, MaxClockSkewSeconds: 120}, body); ok {
		t.Fatal("unsupported identity signature version was accepted")
	}
}

func TestPromptFilterAuditLogUsesVerifiedPolicyMetaOriginalMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hello"}]}`)
	c, _ := signedNewAPIPolicyContext(t, "req-v1-meta-log", newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}, "/v1/chat/completions", body)
	addSignedNewAPIPolicyMeta(t, c, newAPIPolicyMeta{
		Profile: "balanced", Mode: "enforce", Provider: "anthropic", Protocol: "openai",
		OriginalEndpoint: "/v1/messages", OriginalProtocol: "claude",
	}, true)
	setIngressRequestBodyIfAbsent(c, body)
	cfg := promptGuardTestConfig()
	cfg.Advanced.NewAPI.Enabled = true
	cfg.Advanced.NewAPI.MaxClockSkewSeconds = 120
	h := newPromptGuardTestHandler(cfg)
	input := &database.PromptFilterLogInput{Endpoint: "/v1/chat/completions"}
	h.populateVerifiedNewAPIAuditMeta(c, input)
	if input.Endpoint != "/v1/messages" || input.Protocol != "claude" || input.Provider != "anthropic" {
		t.Fatalf("audit log metadata = %+v", input)
	}
}

func TestPromptFilterAuditLogKeepsEnvelopeMetadataWhenSignedMetaIsUnknown(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"gpt-5.5","input":"hello"}`)
	c, _ := signedNewAPIPolicyContext(t, "req-v1-meta-unknown", newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}, "/v1/responses", body)
	addSignedNewAPIPolicyMeta(t, c, newAPIPolicyMeta{
		Profile: promptfilter.GuardProfileBalanced,
		Mode:    promptfilter.GuardModeEnforce,
	}, true)
	setIngressRequestBodyIfAbsent(c, body)
	cfg := promptGuardTestConfig()
	cfg.Advanced.NewAPI.Enabled = true
	cfg.Advanced.NewAPI.MaxClockSkewSeconds = 120
	h := newPromptGuardTestHandler(cfg)
	input := &database.PromptFilterLogInput{
		Endpoint: "/v1/responses",
		Protocol: string(promptfilter.ProtocolResponses),
		Provider: string(promptfilter.ModelFamilyOpenAI),
	}
	h.populateVerifiedNewAPIAuditMeta(c, input)
	if input.Protocol != string(promptfilter.ProtocolResponses) || input.Provider != string(promptfilter.ModelFamilyOpenAI) {
		t.Fatalf("unknown signed metadata replaced envelope metadata: %+v", input)
	}
}

func TestPromptFilterAuditClassifiesNewAPIPassthroughState(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := newPromptFilterBindingTestHandler(t, promptGuardTestConfig(), []database.PromptFilterNewAPIBinding{
		{APIKeyID: 101, PlatformCode: "gateway-a", Secret: "gateway-a-secret", Enabled: true},
		{APIKeyID: 102, PlatformCode: "disabled", Secret: "disabled-secret", Enabled: false},
	})
	makeContext := func(apiKeyID int64) *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		c.Set(contextAPIKeyID, apiKeyID)
		return c
	}

	unsigned := handler.capturePromptFilterAuditContext(makeContext(101))
	if unsigned.NewAPIPolicyStatus != "unsigned_request" || unsigned.NewAPIPlatform != "gateway-a" {
		t.Fatalf("unsigned bound request state = %+v", unsigned)
	}
	invalid := makeContext(101)
	invalid.Request.Header.Set("X-NewAPI-Signature", "invalid")
	if got := handler.capturePromptFilterAuditContext(invalid); got.NewAPIPolicyStatus != "verification_failed" {
		t.Fatalf("invalid signature state = %+v", got)
	}
	if got := handler.capturePromptFilterAuditContext(makeContext(102)); got.NewAPIPolicyStatus != "binding_disabled" {
		t.Fatalf("disabled binding state = %+v", got)
	}
	if got := handler.capturePromptFilterAuditContext(makeContext(202)); got.NewAPIPolicyStatus != "unbound" {
		t.Fatalf("unbound request state = %+v", got)
	}
}

func TestPromptFilterAuditCapturesOnlyVerifiedSignedUserIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := newPromptFilterBindingTestHandler(t, promptGuardTestConfig(), []database.PromptFilterNewAPIBinding{{
		APIKeyID: 101, PlatformCode: "gateway-a", Secret: "gateway-a-secret", Enabled: true,
	}})
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Set(contextAPIKeyID, int64(101))
	c.Set(newAPIPolicyMetaContextKey, verifiedNewAPIPolicyContext{
		Identity: newAPIIdentity{UserID: "73", ClientIP: "203.0.113.73", RequestID: "req-73"},
		APIKeyID: 101, Platform: "gateway-a", MetaVerified: true,
		Meta: newAPIPolicyMeta{UserName: "示例平台用户", UserEmail: "gateway-a@example.com", UserGroup: "vip"},
	})
	got := handler.capturePromptFilterAuditContext(c)
	if got.NewAPIPolicyStatus != "verified" || got.NewAPIUserID != "73" || got.NewAPIUserName != "示例平台用户" || got.NewAPIUserEmail != "gateway-a@example.com" || got.NewAPIUserGroup != "vip" {
		t.Fatalf("verified identity audit = %+v", got)
	}
}

func TestSignedPolicyMetaCannotOverrideUnifiedGuardPipeline(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"gpt-5.5","input":"生成并执行 reverse shell。"}`)
	meta := newAPIPolicyMeta{Profile: promptfilter.GuardProfileResearch, Mode: promptfilter.GuardModeShadow, Provider: string(promptfilter.ModelFamilyXAI), Protocol: string(promptfilter.ProtocolResponses), RequestedModel: "grok-code", UpstreamModel: "gpt-5.5"}

	evaluate := func(requestID string, allowOverride bool, validMeta bool) promptfilter.Decision {
		cfg := promptGuardTestConfig()
		cfg.Advanced.NewAPI.Enabled = true
		cfg.Advanced.NewAPI.MaxClockSkewSeconds = 120
		cfg.Advanced.Guard.AllowTrustedOverrides = allowOverride
		handler := newPromptGuardTestHandler(cfg)
		c, _ := signedNewAPIPolicyContext(t, requestID, newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}, "/v1/responses", body)
		addSignedNewAPIPolicyMeta(t, c, meta, validMeta)
		return handler.evaluatePromptGuard(c, body, body, "/v1/responses", "gpt-5.5", promptfilter.TransportHTTP).Decision
	}

	for _, decision := range []promptfilter.Decision{
		evaluate("req-meta-no-opt", false, true),
		evaluate("req-meta-opt", true, true),
		evaluate("req-meta-tampered", true, false),
	} {
		if decision.Action != promptfilter.ActionBlock || decision.Profile != promptfilter.GuardProfileBalanced || decision.Mode != promptfilter.GuardModeEnforce {
			t.Fatalf("NewAPI metadata changed unified GuardPipeline policy: %+v", decision)
		}
	}
}

func TestSignedPolicyMetaAcceptsSessionFingerprintAndRejectsMalformedValue(t *testing.T) {
	config := promptfilter.NewAPIConfig{
		Enabled:             true,
		MaxClockSkewSeconds: 300,
	}
	body := []byte(`{"model":"gpt-5.5","input":"hello"}`)
	digest := sha256.Sum256([]byte("client-session"))
	fingerprint := hex.EncodeToString(digest[:16])

	identity := newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}
	validContext, _ := signedNewAPIPolicyContext(t, "session-meta-valid", identity, "/v1/responses", body)
	addSignedNewAPIPolicyMeta(t, validContext, newAPIPolicyMeta{
		Profile:            promptfilter.GuardProfileBalanced,
		Mode:               promptfilter.GuardModeEnforce,
		Provider:           string(promptfilter.ModelFamilyOpenAI),
		Protocol:           string(promptfilter.ProtocolResponses),
		SessionFingerprint: fingerprint,
	}, true)
	handlerCfg := promptGuardTestConfig()
	handlerCfg.Advanced.NewAPI.Enabled = true
	handlerCfg.Advanced.NewAPI.MaxClockSkewSeconds = 300
	handler := newPromptGuardTestHandler(handlerCfg)
	policyContext, verified := handler.verifyNewAPIPolicyContext(validContext, config, body)
	if !verified || !policyContext.MetaVerified || policyContext.Meta.SessionFingerprint != fingerprint {
		t.Fatalf("valid signed session fingerprint was rejected: verified=%v meta_verified=%v fingerprint=%q", verified, policyContext.MetaVerified, policyContext.Meta.SessionFingerprint)
	}

	invalidContext, _ := signedNewAPIPolicyContext(t, "session-meta-invalid", identity, "/v1/responses", body)
	addSignedNewAPIPolicyMeta(t, invalidContext, newAPIPolicyMeta{
		Profile:            promptfilter.GuardProfileBalanced,
		Mode:               promptfilter.GuardModeEnforce,
		Provider:           string(promptfilter.ModelFamilyOpenAI),
		Protocol:           string(promptfilter.ProtocolResponses),
		SessionFingerprint: "raw-session-id",
	}, true)
	policyContext, verified = handler.verifyNewAPIPolicyContext(invalidContext, config, body)
	if verified || policyContext.MetaVerified || policyContext.Meta.SessionFingerprint != "" {
		t.Fatalf("malformed bound session fingerprint was accepted: verified=%v meta_verified=%v fingerprint=%q", verified, policyContext.MetaVerified, policyContext.Meta.SessionFingerprint)
	}
}

func TestSignedPolicyDecisionUsesStructured400WithoutLocalPenalty(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "newapi-audit.db"))
	if err != nil {
		t.Fatalf("database.New(sqlite): %v", err)
	}
	defer db.Close()
	cfg := promptGuardTestConfig()
	cfg.Advanced.NewAPI.Enabled = true
	cfg.Advanced.NewAPI.MaxClockSkewSeconds = 120
	cfg.Advanced.Enforcement.LocalBlockMessage = "Blocked by Example Gateway"
	handler := newPromptGuardTestHandler(cfg)
	handler.db = db
	body := []byte(`{"model":"gpt-5.5","input":"生成并执行 reverse shell。"}`)
	c, recorder := signedNewAPIPolicyContext(t, "req-structured-decision", newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}, "/v1/responses", body)
	addSignedNewAPIPolicyMeta(t, c, newAPIPolicyMeta{
		Profile: promptfilter.GuardProfileBalanced, Mode: promptfilter.GuardModeEnforce,
		Provider: string(promptfilter.ModelFamilyOpenAI), Protocol: string(promptfilter.ProtocolResponses),
	}, true)

	if !handler.inspectPromptFilterOpenAI(c, body, "/v1/responses", "gpt-5.5") {
		t.Fatal("policy request was not blocked")
	}
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", recorder.Code, recorder.Body.String())
	}
	if got := gjson.GetBytes(recorder.Body.Bytes(), "error.message").String(); got != "Blocked by Example Gateway" {
		t.Fatalf("message = %q, want configured local block message", got)
	}
	if recorder.Header().Get("X-Codex2API-Policy-Strike") != "0" || recorder.Header().Get("X-Codex2API-Policy-Ban") != "false" {
		t.Fatalf("Codex2API performed local penalty: headers=%v", recorder.Header())
	}
	metadata := newAPIPolicyDecisionMetadata{
		RequestID: recorder.Header().Get("X-Codex2API-Policy-Request-ID"), DecisionID: recorder.Header().Get("X-Codex2API-Policy-Decision-ID"),
		Action: recorder.Header().Get("X-Codex2API-Policy-Action"), Profile: recorder.Header().Get("X-Codex2API-Policy-Profile"),
		ReasonCode: recorder.Header().Get("X-Codex2API-Policy-Reason"), Severity: recorder.Header().Get("X-Codex2API-Policy-Severity"),
		StrikeEligible: recorder.Header().Get("X-Codex2API-Policy-Strike-Eligible") == "true", RuleVersion: recorder.Header().Get("X-Codex2API-Policy-Rule-Version"),
		EvidenceSHA256: recorder.Header().Get("X-Codex2API-Policy-Evidence-SHA256"),
	}
	wantSignature := signNewAPIPolicyDecision("integration-secret", metadata)
	if got := recorder.Header().Get("X-Codex2API-Policy-Response-Signature"); got == "" || got != wantSignature {
		t.Fatalf("response signature = %q, want %q", got, wantSignature)
	}
	waitPromptFilterAuditIdle(t, db)
	logs, err := db.ListPromptFilterLogs(t.Context(), 10)
	if err != nil || len(logs) != 1 {
		t.Fatalf("ListPromptFilterLogs logs=%d err=%v", len(logs), err)
	}
	if logs[0].NewAPIPolicyStatus != "signed_response" || logs[0].NewAPIPlatform != "test-platform" || logs[0].NewAPIUserID != "42" || logs[0].NewAPIRequestID != "req-structured-decision" || logs[0].NewAPIDecisionID != metadata.DecisionID {
		t.Fatalf("NewAPI audit passthrough metadata = %+v", logs[0])
	}
}

func TestWebSocketPolicyDecisionIDUsesLogicalFrameSequence(t *testing.T) {
	cfg := promptfilter.RecommendedConfig()
	cfg.Enabled = true
	identity := newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8", RequestID: "ws-connection-request"}
	decision := promptfilter.Decision{Action: promptfilter.ActionBlock, Profile: promptfilter.GuardProfileBalanced, ReasonCode: "strict_rule", StrikeEligible: true, Terminal: true}
	verdict := promptfilter.Verdict{FullText: "blocked websocket prompt"}
	body := []byte(`{"type":"response.create","model":"gpt-5.5"}`)

	first := buildNewAPIPolicyDecisionMetadataWithSecret(identity, decision, verdict, cfg, body, "/v1/responses", "gpt-5.5", "responses:1", "integration-secret")
	firstRetry := buildNewAPIPolicyDecisionMetadataWithSecret(identity, decision, verdict, cfg, body, "/v1/responses", "gpt-5.5", "responses:1", "integration-secret")
	second := buildNewAPIPolicyDecisionMetadataWithSecret(identity, decision, verdict, cfg, body, "/v1/responses", "gpt-5.5", "responses:2", "integration-secret")

	if first.DecisionID != firstRetry.DecisionID {
		t.Fatalf("same logical websocket event lost idempotency: %q != %q", first.DecisionID, firstRetry.DecisionID)
	}
	if first.DecisionID == second.DecisionID {
		t.Fatalf("distinct websocket frames reused decision id %q", first.DecisionID)
	}
	if first.EventID != "responses:1" || first.EventSignature == "" {
		t.Fatalf("websocket event metadata was not emitted: %+v", first)
	}
	if first.EventSignature != firstRetry.EventSignature {
		t.Fatalf("same logical websocket event lost event-signature idempotency")
	}
	if want := signNewAPIPolicyEvent("", first); want != "" {
		t.Fatalf("empty secret unexpectedly signed websocket event: %q", want)
	}
	secret := "integration-secret"
	if want := signNewAPIPolicyEvent(secret, first); first.EventSignature != want {
		t.Fatalf("websocket event signature = %q, want %q", first.EventSignature, want)
	}
	tampered := first
	tampered.EventID = "responses:2"
	if first.EventSignature == signNewAPIPolicyEvent(secret, tampered) {
		t.Fatal("event signature did not bind event_id")
	}
	details, ok := newAPIPolicyDecisionAPIError(first).Details.(gin.H)
	if !ok {
		t.Fatalf("policy error details type = %T", newAPIPolicyDecisionAPIError(first).Details)
	}
	if details["event_id"] != "responses:1" || details["event_signature_version"] != "v1" || details["event_signature"] != first.EventSignature {
		t.Fatalf("policy error omitted signed event metadata: %+v", details)
	}
}

// 上游 CYB 始终是权威封号信号;本地高置信度严重违规是否累计封号则由
// LocalSevereStrikeEnabled 掌控。两条路径都要覆盖,任一策略回归都应被发现。
func TestUpstreamCYBStrikeAlwaysAndLocalSevereFollowsSwitch(t *testing.T) {
	cfg := promptGuardTestConfig()
	cfg.Advanced.Enforcement.CYBStrikeEnabled = true
	binding := database.PromptFilterNewAPIBinding{
		APIKeyID: 101, PlatformCode: "gateway-a", Secret: "gateway-a-secret", Enabled: true,
		PolicyMode: database.PromptFilterPolicyModeEnforce, PolicyProfile: database.PromptFilterPolicyProfileBalanced,
	}
	handler := newPromptFilterBindingTestHandler(t, cfg, []database.PromptFilterNewAPIBinding{binding})
	body := []byte(`{"model":"gpt-5.5","input":"ordinary request"}`)
	c := signedBoundNewAPIPolicyContext(t, "upstream-cyb-request", newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}, body, 101, "gateway-a", "gateway-a-secret", "")
	setIngressRequestBodyIfAbsent(c, body)

	_, _ = handler.logUpstreamCyberPolicy(c, "/v1/responses", "gpt-5.5", []byte(`{"error":{"code":"cyber_policy","message":"blocked"}}`))
	metadata := policyDecisionMetadataFromHeaders(c.Writer.Header())
	if metadata.ReasonCode != newAPIUpstreamCyberPolicyReasonCode || !metadata.StrikeEligible || metadata.Action != promptfilter.ActionBlock {
		t.Fatalf("upstream CYB decision metadata = %+v", metadata)
	}
	if got, want := c.Writer.Header().Get("X-Codex2API-Policy-Response-Signature"), signNewAPIPolicyDecision("gateway-a-secret", metadata); got == "" || got != want {
		t.Fatalf("upstream CYB signature = %q, want %q", got, want)
	}

	local := promptfilter.Decision{
		Action: promptfilter.ActionBlock, Profile: promptfilter.GuardProfileStrict,
		ReasonCode: "terminal_policy_match", StrikeEligible: true, Terminal: true,
	}
	buildLocal := func(c promptfilter.Config) newAPIPolicyDecisionMetadata {
		return buildNewAPIPolicyDecisionMetadataWithSecret(
			newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8", RequestID: "local-policy-request"},
			local, promptfilter.Verdict{Action: promptfilter.ActionBlock, FullText: "local match"}, c,
			body, "/v1/responses", "gpt-5.5", "", "gateway-a-secret",
		)
	}

	disabled := cfg
	disabled.Advanced.Enforcement.LocalSevereStrikeEnabled = false
	if buildLocal(disabled).StrikeEligible {
		t.Fatal("关闭 LocalSevereStrikeEnabled 后本地严重违规仍被判为可累计封号")
	}

	enabled := cfg
	enabled.Advanced.Enforcement.LocalSevereStrikeEnabled = true
	if !buildLocal(enabled).StrikeEligible {
		t.Fatal("开启 LocalSevereStrikeEnabled 后本地最高置信度严重违规未累计封号")
	}
}

func TestUpstreamCyberPolicyStrikeRequiresExplicitCodex2APISwitch(t *testing.T) {
	cfg := promptGuardTestConfig()
	cfg.Advanced.Enforcement.CYBStrikeEnabled = false
	binding := database.PromptFilterNewAPIBinding{
		APIKeyID: 101, PlatformCode: "gateway-a", Secret: "gateway-a-secret", Enabled: true,
		PolicyMode: database.PromptFilterPolicyModeEnforce, PolicyProfile: database.PromptFilterPolicyProfileBalanced,
	}
	handler := newPromptFilterBindingTestHandler(t, cfg, []database.PromptFilterNewAPIBinding{binding})
	body := []byte(`{"model":"gpt-5.5","input":"ordinary request"}`)
	c := signedBoundNewAPIPolicyContext(t, "upstream-cyb-audit-only", newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}, body, 101, "gateway-a", "gateway-a-secret", "")
	setIngressRequestBodyIfAbsent(c, body)

	_, _ = handler.logUpstreamCyberPolicy(c, "/v1/responses", "gpt-5.5", []byte(`{"error":{"code":"cyber_policy","message":"blocked"}}`))
	metadata, delegated := newAPIUpstreamCyberPolicyDecision(c)
	if !delegated || metadata.ReasonCode != newAPIUpstreamCyberPolicyReasonCode || metadata.StrikeEligible {
		t.Fatalf("disabled CYB strike switch metadata = %+v delegated=%t", metadata, delegated)
	}
	if got := c.Writer.Header().Get("X-Codex2API-Policy-Strike-Eligible"); got != "false" {
		t.Fatalf("strike eligibility header = %q, want false", got)
	}
	if message := newAPIPolicyDecisionAPIError(metadata).Message; !strings.Contains(message, "再次触发可能会停用账号") {
		t.Fatalf("disabled CYB strike deterrence message = %q", message)
	}
}

func TestResponseFailedCarriesSignedNewAPIPolicyDecisionWithoutDroppingUpstreamDetails(t *testing.T) {
	cfg := promptGuardTestConfig()
	metadata := buildNewAPIPolicyDecisionMetadataWithSecret(
		newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8", RequestID: "stream-cyb-request"},
		promptfilter.Decision{
			Action: promptfilter.ActionBlock, Profile: promptfilter.GuardProfileBalanced,
			ReasonCode: newAPIUpstreamCyberPolicyReasonCode, StrikeEligible: true, Terminal: true,
		},
		promptfilter.Verdict{Action: promptfilter.ActionBlock, FullText: "cyber_policy"},
		cfg, []byte(`{"error":{"code":"cyber_policy"}}`), "/v1/responses", "gpt-5.5", "", "gateway-a-secret",
	)
	original := []byte(`{"type":"response.failed","response":{"error":{"code":"cyber_policy","message":"blocked","details":{"upstream_trace":"trace-1"}}}}`)
	decorated := attachNewAPIPolicyDecisionToResponseFailed(original, metadata)

	if got := gjson.GetBytes(decorated, "response.error.code").String(); got != "cyber_policy" {
		t.Fatalf("upstream error code = %q", got)
	}
	if got := gjson.GetBytes(decorated, "response.error.details.upstream_trace").String(); got != "trace-1" {
		t.Fatalf("upstream details were dropped: %s", decorated)
	}
	policy := gjson.GetBytes(decorated, "response.error.details.codex2api_policy")
	if policy.Get("decision_id").String() != metadata.DecisionID || !policy.Get("strike_eligible").Bool() {
		t.Fatalf("signed stream decision missing: %s", decorated)
	}
	if got := policy.Get("response_signature").String(); got != signNewAPIPolicyDecision("gateway-a-secret", metadata) {
		t.Fatalf("stream decision signature = %q", got)
	}
	translated, done := TranslateStreamChunk(decorated, "gpt-5.5", "chatcmpl-test", time.Now().Unix())
	if !done || gjson.GetBytes(translated, "error.details.codex2api_policy.decision_id").String() != metadata.DecisionID {
		t.Fatalf("chat stream translation dropped signed decision: %s", translated)
	}
}

func TestOrdinaryUpstream400DoesNotEmitPolicyStrike(t *testing.T) {
	cfg := promptGuardTestConfig()
	binding := database.PromptFilterNewAPIBinding{
		APIKeyID: 101, PlatformCode: "gateway-a", Secret: "gateway-a-secret", Enabled: true,
		PolicyMode: database.PromptFilterPolicyModeEnforce, PolicyProfile: database.PromptFilterPolicyProfileBalanced,
	}
	handler := newPromptFilterBindingTestHandler(t, cfg, []database.PromptFilterNewAPIBinding{binding})
	body := []byte(`{"model":"gpt-5.5","input":"ordinary request"}`)
	c := signedBoundNewAPIPolicyContext(t, "ordinary-400-request", newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}, body, 101, "gateway-a", "gateway-a-secret", "")
	setIngressRequestBodyIfAbsent(c, body)

	if incidentID, accepted := handler.logUpstreamCyberPolicy(c, "/v1/responses", "gpt-5.5", []byte(`{"error":{"code":"invalid_request_error"}}`)); incidentID != "" || accepted {
		t.Fatalf("ordinary 400 was treated as CYB: incident=%q accepted=%t", incidentID, accepted)
	}
	assertNoPromptPolicyPenaltyHeaders(t, c.Writer.Header())
}

func TestNewAPIPolicyDecisionAndEventSignatureGoldenVector(t *testing.T) {
	metadata := newAPIPolicyDecisionMetadata{
		RequestID: " req-1 ", DecisionID: " dec-1 ", EventID: " responses:7 ",
		Action: " block ", Profile: " balanced ", ReasonCode: " strict_rule ", Severity: " critical ",
		StrikeEligible: true, RuleVersion: " rules-1 ", EvidenceSHA256: " aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa ",
	}
	if got := signNewAPIPolicyDecision("golden-secret", metadata); got != "0928b076c3eca3e9a02c5207ed50c21a2fbd995e9286a3c7991949f32218963d" {
		t.Fatalf("decision signature golden vector = %s", got)
	}
	if got := signNewAPIPolicyEvent("golden-secret", metadata); got != "7146eeed562577cfbaa3bf57ee5a037b394e96b94664642398dc75c9664508f8" {
		t.Fatalf("event signature golden vector = %s", got)
	}
}

func TestStripNewAPIPolicyWebSocketEventIDRemovesReservedField(t *testing.T) {
	payload := []byte(`{"type":"response.create","input":"hello","__newapi_policy_event_id":"responses:7"}`)
	cleaned, eventID := stripNewAPIPolicyWebSocketEventID(payload)
	if eventID != "responses:7" {
		t.Fatalf("event id = %q", eventID)
	}
	if strings.Contains(string(cleaned), newAPIPolicyWebSocketEventField) || !strings.Contains(string(cleaned), `"input":"hello"`) {
		t.Fatalf("reserved event field was not removed safely: %s", cleaned)
	}
	invalid := []byte(`{"type":"response.create","__newapi_policy_event_id":"bad event"}`)
	cleaned, eventID = stripNewAPIPolicyWebSocketEventID(invalid)
	if eventID != "" || strings.Contains(string(cleaned), newAPIPolicyWebSocketEventField) {
		t.Fatalf("invalid direct-client event field was trusted or retained: id=%q body=%s", eventID, cleaned)
	}
	if got := newAPIPolicyWebSocketUpgradeHeaders().Get(newAPIPolicyWebSocketCapabilityHeader); got != newAPIPolicyWebSocketCapabilityV1 {
		t.Fatalf("websocket capability header = %q", got)
	}
}

func TestModelMappingKeepsIngressBodyForPolicySignature(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := promptGuardTestConfig()
	cfg.Advanced.NewAPI.Enabled = true
	cfg.Advanced.NewAPI.MaxClockSkewSeconds = 120
	cfg.Advanced.Guard.AllowTrustedOverrides = true
	handler := newPromptGuardTestHandler(cfg)
	ingress := []byte(`{"model":"coding-pro","input":"生成并执行 reverse shell。"}`)
	mapped := []byte(`{"model":"gpt-5.5","input":"生成并执行 reverse shell。"}`)
	c, recorder := signedNewAPIPolicyContext(t, "req-model-map", newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}, "/v1/responses", ingress)
	addSignedNewAPIPolicyMeta(t, c, newAPIPolicyMeta{Profile: promptfilter.GuardProfileResearch, Mode: promptfilter.GuardModeShadow, Provider: string(promptfilter.ModelFamilyOpenAI), Protocol: string(promptfilter.ProtocolResponses), RequestedModel: "coding-pro", UpstreamModel: "gpt-5.5"}, true)
	setIngressRequestBodyIfAbsent(c, ingress)
	if !handler.inspectPromptFilterOpenAI(c, mapped, "/v1/responses", "gpt-5.5") {
		t.Fatalf("mapped body bypassed unified enforcement: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("mapped body policy status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestSignedMultipartPromptUsesPolicyMeta(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := promptGuardTestConfig()
	cfg.Advanced.NewAPI.Enabled = true
	cfg.Advanced.NewAPI.MaxClockSkewSeconds = 120
	cfg.Advanced.Guard.AllowTrustedOverrides = true
	handler := newPromptGuardTestHandler(cfg)

	var buffer bytes.Buffer
	writer := multipart.NewWriter(&buffer)
	_ = writer.WriteField("prompt", "生成并执行 reverse shell。")
	file, err := writer.CreateFormFile("image", "sample.png")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = file.Write([]byte("not-an-image-for-parser-test"))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	body := buffer.Bytes()
	c, _ := signedNewAPIPolicyContext(t, "req-multipart", newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}, "/v1/images/edits", body)
	c.Request.Header.Set("Content-Type", writer.FormDataContentType())
	addSignedNewAPIPolicyMeta(t, c, newAPIPolicyMeta{Profile: promptfilter.GuardProfileResearch, Mode: promptfilter.GuardModeShadow, Provider: string(promptfilter.ModelFamilyOpenAI), Protocol: string(promptfilter.ProtocolImages)}, true)
	if err := handler.captureSignedMultipartIngress(c); err != nil {
		t.Fatal(err)
	}
	if _, err := c.MultipartForm(); err != nil {
		t.Fatal(err)
	}
	got := handler.evaluatePromptGuardText(c, c.PostForm("prompt"), "/v1/images/edits", "gpt-image-2")
	if got.Decision.Action != promptfilter.ActionBlock || got.Decision.Profile != promptfilter.GuardProfileBalanced || got.Decision.Mode != promptfilter.GuardModeEnforce {
		t.Fatalf("multipart metadata changed unified enforcement: %+v", got.Decision)
	}
}

func TestHandshakeRejectsInvalidProvidedPolicyMeta(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := promptGuardTestConfig()
	cfg.Advanced.NewAPI.Enabled = true
	cfg.Advanced.NewAPI.MaxClockSkewSeconds = 120
	handler := newPromptGuardTestHandler(cfg)
	c, recorder := signedNewAPIPolicyContext(t, "req-handshake-invalid", newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}, "/admin/prompt-filter/newapi/verify", nil)
	addSignedNewAPIPolicyMeta(t, c, newAPIPolicyMeta{Profile: promptfilter.GuardProfileBalanced, Mode: promptfilter.GuardModeEnforce, Provider: string(promptfilter.ModelFamilyOpenAI), Protocol: string(promptfilter.ProtocolResponses)}, false)
	handler.VerifyNewAPIPolicyHandshake(c)
	if recorder.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422; body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestBoundNewAPIIdentityIsolatedByAPIKeyPlatformAndSecret(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"gpt-5.5","input":"hello"}`)
	fingerprint := promptSessionTestFingerprint("same-session")
	handler := newPromptFilterBindingTestHandler(t, promptGuardTestConfig(), []database.PromptFilterNewAPIBinding{
		{APIKeyID: 101, PlatformCode: "gateway-a", Secret: "gateway-a-secret", Enabled: true, PolicyMode: database.PromptFilterPolicyModeInherit, PolicyProfile: database.PromptFilterPolicyProfileInherit},
		{APIKeyID: 202, PlatformCode: "gateway-b", Secret: "gateway-b-secret", Enabled: true, PolicyMode: database.PromptFilterPolicyModeInherit, PolicyProfile: database.PromptFilterPolicyProfileInherit},
	})
	identity := newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}

	gatewayAContext := signedBoundNewAPIPolicyContext(t, "same-request", identity, body, 101, "gateway-a", "gateway-a-secret", fingerprint)
	gatewayAConfig := handler.promptFilterConfigForRequest(gatewayAContext)
	gatewayAVerified, ok := handler.verifyNewAPIPolicyContext(gatewayAContext, gatewayAConfig.Advanced.NewAPI, body)
	if !ok || gatewayAVerified.APIKeyID != 101 || gatewayAVerified.Platform != "gateway-a" || gatewayAVerified.VerificationSecret != "gateway-a-secret" {
		t.Fatalf("gateway-a verification = %+v ok=%v", gatewayAVerified, ok)
	}

	// The exact same request/user/session tuple is valid on another platform;
	// replay state is scoped by Codex API key and bound platform.
	buy := signedBoundNewAPIPolicyContext(t, "same-request", identity, body, 202, "gateway-b", "gateway-b-secret", fingerprint)
	buyCfg := handler.promptFilterConfigForRequest(buy)
	buyVerified, ok := handler.verifyNewAPIPolicyContext(buy, buyCfg.Advanced.NewAPI, body)
	if !ok || buyVerified.APIKeyID != 202 || buyVerified.Platform != "gateway-b" || buyVerified.VerificationSecret != "gateway-b-secret" {
		t.Fatalf("gateway-b verification = %+v ok=%v", buyVerified, ok)
	}

	for _, tc := range []struct {
		name       string
		requestID  string
		secret     string
		platformID string
	}{
		{name: "other binding secret", requestID: "cross-secret", secret: "gateway-b-secret", platformID: "gateway-a"},
		{name: "legacy global fallback", requestID: "global-fallback", secret: "legacy-global-secret", platformID: "gateway-a"},
		{name: "signed platform mismatch", requestID: "platform-mismatch", secret: "gateway-a-secret", platformID: "gateway-b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := signedBoundNewAPIPolicyContext(t, tc.requestID, identity, body, 101, tc.platformID, tc.secret, fingerprint)
			cfg := handler.promptFilterConfigForRequest(c)
			if _, verified := handler.verifyNewAPIPolicyContext(c, cfg.Advanced.NewAPI, body); verified {
				t.Fatalf("cross-platform request was accepted: secret=%q platform=%q", tc.secret, tc.platformID)
			}
		})
	}
}

func TestNewAPIIdentitySecretsDoNotFallbackForUnboundKeyInBindingMode(t *testing.T) {
	t.Setenv("PROMPT_FILTER_NEWAPI_SECRET", "legacy-global-secret")
	handler := newPromptFilterBindingTestHandler(t, promptGuardTestConfig(), []database.PromptFilterNewAPIBinding{{
		APIKeyID: 101, PlatformCode: "gateway-a", Secret: "gateway-a-secret", Enabled: true,
		PolicyMode: database.PromptFilterPolicyModeInherit, PolicyProfile: database.PromptFilterPolicyProfileInherit,
	}})
	unbound, _ := gin.CreateTestContext(httptest.NewRecorder())
	unbound.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	unbound.Set(contextAPIKeyID, int64(999))
	_, platform, enabled, candidates := handler.newAPIIdentitySecrets(unbound, time.Now())
	if enabled || len(candidates) != 0 || platform != "bound" {
		t.Fatalf("unbound key borrowed legacy secret: platform=%q enabled=%v candidates=%+v", platform, enabled, candidates)
	}

	zeroBindingHandler := newPromptFilterBindingTestHandler(t, promptGuardTestConfig(), nil)
	zeroBinding, _ := gin.CreateTestContext(httptest.NewRecorder())
	zeroBinding.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	zeroBinding.Set(contextAPIKeyID, int64(999))
	_, platform, enabled, candidates = zeroBindingHandler.newAPIIdentitySecrets(zeroBinding, time.Now())
	if enabled || len(candidates) != 0 || platform != "bound" {
		t.Fatalf("zero-binding unbound key trusted retired global secret: platform=%q enabled=%v candidates=%+v", platform, enabled, candidates)
	}
}

func TestPromptFilterBindingScopeControlsOnlyItsAPIKey(t *testing.T) {
	cfg := promptGuardTestConfig()
	cfg.Enabled = true
	cfg.Review.Enabled = true
	handler := newPromptFilterBindingTestHandler(t, cfg, []database.PromptFilterNewAPIBinding{
		{APIKeyID: 101, PlatformCode: "full-review", Secret: "full-review-secret", Enabled: true, PromptFilterScope: database.PromptFilterScopeInherit},
		{APIKeyID: 202, PlatformCode: "local-only", Secret: "local-only-secret", Enabled: true, PromptFilterScope: database.PromptFilterScopeLocalOnly},
		{APIKeyID: 303, PlatformCode: "prompt-off", Secret: "prompt-off-secret", Enabled: true, PromptFilterScope: database.PromptFilterScopeOff},
	})
	requestConfig := func(apiKeyID int64) promptfilter.Config {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		c.Set(contextAPIKeyID, apiKeyID)
		return handler.promptFilterConfigForRequest(c)
	}

	inherit := requestConfig(101)
	if !inherit.Enabled || !inherit.Review.Enabled {
		t.Fatalf("inherit scope changed global review: enabled=%v review=%v", inherit.Enabled, inherit.Review.Enabled)
	}
	localOnly := requestConfig(202)
	if !localOnly.Enabled || localOnly.Review.Enabled || !localOnly.Advanced.NewAPI.Enabled {
		t.Fatalf("local-only scope=%+v", localOnly)
	}
	off := requestConfig(303)
	if off.Enabled || !off.Advanced.NewAPI.Enabled {
		t.Fatalf("off scope disabled identity or retained prompt checks: enabled=%v newapi=%v", off.Enabled, off.Advanced.NewAPI.Enabled)
	}
	unbound := requestConfig(404)
	if !unbound.Enabled || !unbound.Review.Enabled || unbound.Advanced.NewAPI.Enabled {
		t.Fatalf("unbound key inherited wrong config: enabled=%v review=%v newapi=%v", unbound.Enabled, unbound.Review.Enabled, unbound.Advanced.NewAPI.Enabled)
	}
}

func TestUnboundWebSocketIsNotRevokedByAnotherKeysBinding(t *testing.T) {
	handler := newPromptFilterBindingTestHandler(t, promptGuardTestConfig(), []database.PromptFilterNewAPIBinding{{
		APIKeyID: 101, PlatformCode: "gateway-a", Secret: "gateway-a-secret", Enabled: true,
		PolicyMode: database.PromptFilterPolicyModeInherit, PolicyProfile: database.PromptFilterPolicyProfileInherit,
	}})
	connection, _ := gin.CreateTestContext(httptest.NewRecorder())
	connection.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	connection.Set(contextAPIKeyID, int64(999))
	if _, bound := handler.resolvePromptFilterNewAPIBinding(connection); bound {
		t.Fatal("test connection unexpectedly started with a binding")
	}
	if apiErr := handler.refreshNewAPIWebSocketBinding(connection, time.Now()); apiErr != nil {
		t.Fatalf("another key's binding revoked an unbound websocket: %v", apiErr)
	}

	newBinding := database.PromptFilterNewAPIBinding{
		APIKeyID: 999, PlatformCode: "later-bound", Secret: "later-bound-secret", Enabled: true,
		PolicyMode: database.PromptFilterPolicyModeInherit, PolicyProfile: database.PromptFilterPolicyProfileInherit,
	}
	handler.store.ReplacePromptFilterNewAPIBindings([]*database.PromptFilterNewAPIBinding{&newBinding})
	if apiErr := handler.refreshNewAPIWebSocketBinding(connection, time.Now()); apiErr == nil || apiErr.Code != api.ErrorCode("newapi_websocket_binding_changed") {
		t.Fatalf("current key becoming bound did not require reconnect: %+v", apiErr)
	}
}

func TestBindingSnapshotRefreshesPolicyAndRevokesObsoleteWebSocketIdentity(t *testing.T) {
	oldBinding := database.PromptFilterNewAPIBinding{
		APIKeyID: 101, PlatformCode: "gateway-a", Secret: "old-secret", Enabled: true, RequireSignedIdentity: true,
		PromptFilterScope: database.PromptFilterScopeInherit,
		PolicyMode:        database.PromptFilterPolicyModeWarn, PolicyProfile: database.PromptFilterPolicyProfileResearch,
	}
	handler := newPromptFilterBindingTestHandler(t, promptGuardTestConfig(), []database.PromptFilterNewAPIBinding{oldBinding})
	newConnection := func() *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
		c.Set(contextAPIKeyID, int64(101))
		binding, bound := handler.resolvePromptFilterNewAPIBinding(c)
		if !bound || binding.PlatformCode != "gateway-a" {
			t.Fatalf("initial binding = %+v bound=%v", binding, bound)
		}
		c.Set(newAPIIdentityContextKey, verifiedNewAPIIdentityContext{
			APIKeyID: 101, Platform: "gateway-a", VerificationSecret: "old-secret",
		})
		return c
	}

	c := newConnection()
	expiresAt := time.Now().Add(time.Minute)
	rotated := database.PromptFilterNewAPIBinding{
		APIKeyID: 101, PlatformCode: "gateway-a", Secret: "new-secret", PreviousSecret: "old-secret", PreviousSecretExpiresAt: &expiresAt,
		Enabled: true, RequireSignedIdentity: true,
		PromptFilterScope: database.PromptFilterScopeLocalOnly,
		PolicyMode:        database.PromptFilterPolicyModeEnforce, PolicyProfile: database.PromptFilterPolicyProfileStrict,
	}
	handler.store.ReplacePromptFilterNewAPIBindings([]*database.PromptFilterNewAPIBinding{&rotated})
	if apiErr := handler.refreshNewAPIWebSocketBinding(c, time.Now()); apiErr != nil {
		t.Fatalf("valid grace-period websocket identity was revoked: %v", apiErr)
	}
	resetPromptRequestSecurityFrame(c)
	currentBinding, bound := handler.resolvePromptFilterNewAPIBinding(c)
	if !bound || currentBinding.Secret != "new-secret" {
		t.Fatalf("connection did not refresh the current binding: %+v bound=%v", currentBinding, bound)
	}
	currentCfg := handler.promptFilterConfigForRequest(c)
	if currentCfg.Advanced.Guard.Mode != promptfilter.GuardModeInherit || currentCfg.Advanced.Guard.DefaultProfile != promptfilter.GuardProfileBalanced {
		t.Fatalf("binding changed unified policy: mode=%q profile=%q", currentCfg.Advanced.Guard.Mode, currentCfg.Advanced.Guard.DefaultProfile)
	}
	if currentCfg.Review.Enabled {
		t.Fatal("websocket frame did not hot-refresh local-only review scope")
	}

	for _, tc := range []struct {
		name     string
		bindings []database.PromptFilterNewAPIBinding
	}{
		{name: "secret grace expired", bindings: []database.PromptFilterNewAPIBinding{{APIKeyID: 101, PlatformCode: "gateway-a", Secret: "new-secret", Enabled: true, RequireSignedIdentity: true, PolicyMode: "inherit", PolicyProfile: "inherit"}}},
		{name: "binding disabled", bindings: []database.PromptFilterNewAPIBinding{{APIKeyID: 101, PlatformCode: "gateway-a", Secret: "old-secret", Enabled: false, RequireSignedIdentity: true, PolicyMode: "inherit", PolicyProfile: "inherit"}}},
		{name: "platform rebound", bindings: []database.PromptFilterNewAPIBinding{{APIKeyID: 101, PlatformCode: "gateway-b", Secret: "old-secret", Enabled: true, RequireSignedIdentity: true, PolicyMode: "inherit", PolicyProfile: "inherit"}}},
		{name: "binding deleted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			connection := newConnection()
			bindings := make([]*database.PromptFilterNewAPIBinding, 0, len(tc.bindings))
			for index := range tc.bindings {
				binding := tc.bindings[index]
				bindings = append(bindings, &binding)
			}
			handler.store.ReplacePromptFilterNewAPIBindings(bindings)
			if apiErr := handler.refreshNewAPIWebSocketBinding(connection, time.Now()); apiErr == nil || apiErr.Code != api.ErrorCode("newapi_websocket_binding_changed") {
				t.Fatalf("obsolete websocket binding was not revoked: %+v", apiErr)
			}
			handler.store.ReplacePromptFilterNewAPIBindings([]*database.PromptFilterNewAPIBinding{&oldBinding})
		})
	}
}

func TestResponsesWebSocketClosesBeforeUpstreamAfterBindingSecretRevocation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "ws-binding-revocation.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	apiKey := "sk-ws-binding-revocation-1234567890"
	apiKeyID, err := db.InsertAPIKey(context.Background(), "ws binding", apiKey)
	if err != nil {
		t.Fatal(err)
	}
	tokenCache := cache.NewMemory(8)
	defer tokenCache.Close()
	store := auth.NewStore(db, tokenCache, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1})
	defer store.Stop()
	store.SetPromptFilterConfig(promptGuardTestConfig())
	store.ReplacePromptFilterNewAPIBindings([]*database.PromptFilterNewAPIBinding{{
		APIKeyID: apiKeyID, PlatformCode: "gateway-a", Secret: "gateway-a-old-secret", Enabled: true, RequireSignedIdentity: true,
		PolicyMode: database.PromptFilterPolicyModeEnforce, PolicyProfile: database.PromptFilterPolicyProfileBalanced,
	}})
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "at", PlanType: "plus", AccountID: "acct-ws-binding"})
	handler := NewHandler(store, db, nil, nil)
	handler.SetRuntimeCache(tokenCache)

	previousExecute := WebsocketExecuteFunc
	defer func() { WebsocketExecuteFunc = previousExecute }()
	var upstreamCalls atomic.Int32
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, requestBody []byte, sessionID string, proxyOverride string, apiKey string, deviceCfg *DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
		upstreamCalls.Add(1)
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("data: [DONE]\n\n"))}, nil
	}

	router := gin.New()
	handler.RegisterRoutes(router)
	server := httptest.NewServer(router)
	defer server.Close()
	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"
	signedRequest := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
	signedRequest.Header.Set("Authorization", "Bearer "+apiKey)
	setSignedNewAPIRequestHeaders(t, signedRequest, nil, "ws-binding-handshake", newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}, "gateway-a", "gateway-a-old-secret", "")
	conn, response, err := websocket.DefaultDialer.Dial(wsURL, signedRequest.Header)
	if err != nil {
		if response != nil {
			t.Fatalf("dial websocket: %v status=%d", err, response.StatusCode)
		}
		t.Fatalf("dial websocket: %v", err)
	}
	defer conn.Close()

	store.UpsertPromptFilterNewAPIBinding(database.PromptFilterNewAPIBinding{
		APIKeyID: apiKeyID, PlatformCode: "gateway-a", Secret: "gateway-a-new-secret", Enabled: true, RequireSignedIdentity: true,
		PolicyMode: database.PromptFilterPolicyModeEnforce, PolicyProfile: database.PromptFilterPolicyProfileBalanced,
	})
	if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-5.5","input":"hello"}`)); err != nil {
		t.Fatal(err)
	}
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, event, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read revocation event: %v", err)
	}
	if gjson.GetBytes(event, "type").String() != "error" || gjson.GetBytes(event, "error.code").String() != "newapi_websocket_binding_changed" {
		t.Fatalf("unexpected revocation event: %s", event)
	}
	if upstreamCalls.Load() != 0 {
		t.Fatalf("revoked websocket reached upstream %d times", upstreamCalls.Load())
	}
}

func TestBindingAndSignedMetaCannotControlGuardModeOrProfile(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","input":"hello"}`)
	identity := newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}
	evaluate := func(requestID, bindingMode, bindingProfile string) (string, string, bool) {
		cfg := promptGuardTestConfig()
		cfg.Advanced.Guard.AllowTrustedOverrides = true
		handler := newPromptFilterBindingTestHandler(t, cfg, []database.PromptFilterNewAPIBinding{{
			APIKeyID: 101, PlatformCode: "gateway-a", Secret: "gateway-a-secret", Enabled: true,
			PolicyMode: bindingMode, PolicyProfile: bindingProfile,
		}})
		c, _ := signedNewAPIPolicyContextWithSecret(t, requestID, identity, "/v1/responses", body, "gateway-a-secret")
		c.Set(contextAPIKeyID, int64(101))
		addSignedNewAPIPolicyMetaWithSecret(t, c, newAPIPolicyMeta{
			PlatformID: "gateway-a", Profile: promptfilter.GuardProfileResearch, Mode: promptfilter.GuardModeOff,
			Provider: string(promptfilter.ModelFamilyOpenAI), Protocol: string(promptfilter.ProtocolResponses),
		}, true, "gateway-a-secret")
		requestCfg := handler.promptFilterConfigForRequest(c)
		_, _, trusted, profile, mode, _, _ := handler.resolvePromptGuardOverrides(c, requestCfg, body, "gpt-5.5")
		return profile, mode, trusted
	}

	for _, values := range [][2]string{
		{database.PromptFilterPolicyModeEnforce, database.PromptFilterPolicyProfileStrict},
		{database.PromptFilterPolicyModeInherit, database.PromptFilterPolicyProfileInherit},
	} {
		profile, mode, trusted := evaluate("binding-policy-ignored-"+values[0], values[0], values[1])
		if !trusted || profile != "" || mode != "" {
			t.Fatalf("identity metadata exposed policy override: trusted=%v profile=%q mode=%q", trusted, profile, mode)
		}
	}
}

func TestSignedPolicyMetaRejectsInvalidPlatformCodeWithoutTruncation(t *testing.T) {
	valid32 := "A" + strings.Repeat("b", 31)
	meta := newAPIPolicyMeta{
		PlatformID: valid32, Profile: promptfilter.GuardProfileBalanced, Mode: promptfilter.GuardModeEnforce,
		Provider: string(promptfilter.ModelFamilyOpenAI), Protocol: string(promptfilter.ProtocolResponses),
	}
	if !normalizeVerifiedNewAPIPolicyMeta(&meta) || meta.PlatformID != strings.ToLower(valid32) {
		t.Fatalf("valid platform metadata was rejected or not normalized: %+v", meta)
	}
	for _, value := range []string{strings.Repeat("a", 33), "gateway-a.prod", "_gateway-a"} {
		invalid := newAPIPolicyMeta{
			PlatformID: value, Profile: promptfilter.GuardProfileBalanced, Mode: promptfilter.GuardModeEnforce,
			Provider: string(promptfilter.ModelFamilyOpenAI), Protocol: string(promptfilter.ProtocolResponses),
		}
		if normalizeVerifiedNewAPIPolicyMeta(&invalid) {
			t.Fatalf("invalid platform metadata %q was accepted as %q", value, invalid.PlatformID)
		}
	}
}

func TestSignedPolicyMetaNormalizesUserIdentityText(t *testing.T) {
	meta := newAPIPolicyMeta{
		PlatformID: "gateway-a", Profile: promptfilter.GuardProfileBalanced, Mode: promptfilter.GuardModeEnforce,
		Provider: string(promptfilter.ModelFamilyOpenAI), Protocol: string(promptfilter.ProtocolResponses),
		UserName: "  示例平台用户  ", UserEmail: " user@example.com ", UserGroup: " vip ",
	}
	if !normalizeVerifiedNewAPIPolicyMeta(&meta) || meta.UserName != "示例平台用户" || meta.UserEmail != "user@example.com" || meta.UserGroup != "vip" {
		t.Fatalf("identity metadata normalization = %+v", meta)
	}
	meta.UserName = "bad\nname"
	if normalizeVerifiedNewAPIPolicyMeta(&meta) {
		t.Fatal("control character in signed identity metadata was accepted")
	}
}

func TestBoundPreviousSecretGraceSignsDecisionWithVerifiedSecret(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"gpt-5.5","input":"hello"}`)
	expires := time.Now().Add(time.Minute)
	binding := database.PromptFilterNewAPIBinding{
		APIKeyID: 101, PlatformCode: "gateway-a", Secret: "current-secret", Enabled: true,
		PolicyMode: database.PromptFilterPolicyModeEnforce, PolicyProfile: database.PromptFilterPolicyProfileBalanced,
		PreviousSecret: "previous-secret", PreviousSecretExpiresAt: &expires,
	}
	handler := newPromptFilterBindingTestHandler(t, promptGuardTestConfig(), []database.PromptFilterNewAPIBinding{binding})
	c := signedBoundNewAPIPolicyContext(t, "previous-grace", newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}, body, 101, "gateway-a", "previous-secret", "")
	cfg := handler.promptFilterConfigForRequest(c)
	decision := promptfilter.Decision{Action: promptfilter.ActionBlock, Profile: promptfilter.GuardProfileBalanced, ReasonCode: "prompt_policy_match", StrikeEligible: true}
	verdict := promptfilter.Verdict{Action: promptfilter.ActionBlock, FullText: "blocked evidence"}
	if !handler.sendNewAPIPolicyDecision(c, cfg, decision, verdict, body, "/v1/responses", "gpt-5.5", body) {
		t.Fatal("previous-secret request was not delegated")
	}
	metadata := policyDecisionMetadataFromHeaders(c.Writer.Header())
	if got, want := c.Writer.Header().Get("X-Codex2API-Policy-Response-Signature"), signNewAPIPolicyDecision("previous-secret", metadata); got != want {
		t.Fatalf("response signature = %q, want previous-secret signature %q", got, want)
	}
	if got := signNewAPIPolicyDecision("current-secret", metadata); got == c.Writer.Header().Get("X-Codex2API-Policy-Response-Signature") {
		t.Fatal("response was signed with current secret instead of the secret that verified the request")
	}
	policyContext, verified := handler.verifyNewAPIPolicyContext(c, cfg.Advanced.NewAPI, body)
	if !verified {
		t.Fatal("verified previous-secret context was not cached")
	}
	wsMetadata := buildNewAPIPolicyDecisionMetadataWithSecret(policyContext.Identity, decision, verdict, cfg, body, "/v1/responses", "gpt-5.5", "responses:1", policyContext.VerificationSecret)
	if wsMetadata.EventSignature != signNewAPIPolicyEvent("previous-secret", wsMetadata) || wsMetadata.EventSignature == signNewAPIPolicyEvent("current-secret", wsMetadata) {
		t.Fatal("WebSocket decision event did not use the request verification secret")
	}

	expired := time.Now().Add(-time.Second)
	binding.PreviousSecretExpiresAt = &expired
	handler.store.ReplacePromptFilterNewAPIBindings([]*database.PromptFilterNewAPIBinding{&binding})
	expiredContext := signedBoundNewAPIPolicyContext(t, "previous-expired", newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}, body, 101, "gateway-a", "previous-secret", "")
	expiredCfg := handler.promptFilterConfigForRequest(expiredContext)
	if _, verified := handler.verifyNewAPIPolicyContext(expiredContext, expiredCfg.Advanced.NewAPI, body); verified {
		t.Fatal("expired previous secret was accepted")
	}
}

func TestSignedNewAPIPolicyBlockUsesAnthropicErrorEnvelopeForMessages(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hello"}]}`)
	baseCfg := promptGuardTestConfig()
	baseCfg.Advanced.Enforcement.LocalBlockMessage = "Blocked by Example Gateway"
	handler := newPromptFilterBindingTestHandler(t, baseCfg, []database.PromptFilterNewAPIBinding{{
		APIKeyID: 101, PlatformCode: "gateway-a", Secret: "gateway-a-secret", Enabled: true, RequireSignedIdentity: true,
		PolicyMode: database.PromptFilterPolicyModeEnforce, PolicyProfile: database.PromptFilterPolicyProfileBalanced,
	}})
	c, recorder := signedNewAPIPolicyContextWithSecret(t, "messages-policy-block", newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}, "/v1/messages", body, "gateway-a-secret")
	c.Set(contextAPIKeyID, int64(101))
	addSignedNewAPIPolicyMetaWithSecret(t, c, newAPIPolicyMeta{
		PlatformID: "gateway-a", Profile: promptfilter.GuardProfileBalanced, Mode: promptfilter.GuardModeEnforce,
		Provider: string(promptfilter.ModelFamilyAnthropic), Protocol: string(promptfilter.ProtocolMessages),
	}, true, "gateway-a-secret")
	cfg := handler.promptFilterConfigForRequest(c)
	decision := promptfilter.Decision{Action: promptfilter.ActionBlock, Profile: promptfilter.GuardProfileBalanced, ReasonCode: "prompt_policy_match"}
	verdict := promptfilter.Verdict{Action: promptfilter.ActionBlock, FullText: "blocked tool output"}
	if !handler.sendNewAPIPolicyDecision(c, cfg, decision, verdict, body, "/v1/messages", "claude-sonnet-4", body) {
		t.Fatal("signed NewAPI Messages policy decision was not returned")
	}
	if recorder.Code != http.StatusBadRequest || gjson.GetBytes(recorder.Body.Bytes(), "type").String() != "error" || gjson.GetBytes(recorder.Body.Bytes(), "error.type").String() != "invalid_request_error" {
		t.Fatalf("Messages policy response = %d %s", recorder.Code, recorder.Body.String())
	}
	if got := gjson.GetBytes(recorder.Body.Bytes(), "error.message").String(); got != "Blocked by Example Gateway" {
		t.Fatalf("message = %q, want configured local block message", got)
	}
	if recorder.Header().Get("X-Codex2API-Policy-Decision-ID") == "" || recorder.Header().Get("X-Codex2API-Policy-Response-Signature") == "" {
		t.Fatalf("Messages policy response lost signed decision headers: %v", recorder.Header())
	}
}

func TestConfiguredLocalBlockMessageDoesNotReplaceRestrictionMessages(t *testing.T) {
	cfg := promptfilter.DefaultConfig()
	cfg.Advanced.Enforcement.LocalBlockMessage = "Blocked by Example Gateway"
	locked := newAPILocalPromptPolicyDecisionAPIError(newAPIPolicyDecisionMetadata{ReasonCode: promptConversationLockedReasonCode}, cfg)
	if locked.Message != promptConversationLockedMessage {
		t.Fatalf("conversation lock message = %q", locked.Message)
	}
	cooldown := newAPILocalPromptPolicyDecisionAPIError(newAPIPolicyDecisionMetadata{ReasonCode: promptUserCyberCooldownReasonCode}, cfg)
	if cooldown.Message != promptUserCyberCooldownMessage {
		t.Fatalf("cooldown message = %q", cooldown.Message)
	}
	upstream := newAPILocalPromptPolicyDecisionAPIError(newAPIPolicyDecisionMetadata{ReasonCode: newAPIUpstreamCyberPolicyReasonCode}, cfg)
	if upstream.Message != upstreamCyberPolicyUserMessage {
		t.Fatalf("upstream policy message = %q", upstream.Message)
	}
}

func TestClearNewAPIUpstreamCyberPolicyDecisionBeforeTransparentRetry(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	first := newAPIPolicyDecisionMetadata{
		RequestID: "request-1", DecisionID: "decision-1", Action: "block",
		ReasonCode: newAPIUpstreamCyberPolicyReasonCode, Signature: "signature-1",
	}
	c.Set(newAPIUpstreamCyberDecisionContextKey, first)
	writeNewAPIPolicyDecisionHeaders(c, first)

	clearNewAPIUpstreamCyberPolicyDecision(c)

	if _, ok := newAPIUpstreamCyberPolicyDecision(c); ok {
		t.Fatal("transparent retry retained the previous attempt's policy decision")
	}
	for name := range recorder.Header() {
		if strings.HasPrefix(http.CanonicalHeaderKey(name), "X-Codex2api-Policy-") {
			t.Fatalf("transparent retry retained policy response header %q", name)
		}
	}

	second := first
	second.DecisionID = "decision-2"
	second.Signature = "signature-2"
	c.Set(newAPIUpstreamCyberDecisionContextKey, second)
	writeNewAPIPolicyDecisionHeaders(c, second)
	got, ok := newAPIUpstreamCyberPolicyDecision(c)
	if !ok || got.DecisionID != second.DecisionID || recorder.Header().Get("X-Codex2API-Policy-Response-Signature") != second.Signature {
		t.Fatalf("final policy decision was not preserved: metadata=%+v headers=%v", got, recorder.Header())
	}
}

func TestBindingCannotChangeRequestPolicySnapshot(t *testing.T) {
	base := promptGuardTestConfig()
	base.Mode = promptfilter.ModeBlock
	base.Advanced.Guard.Mode = promptfilter.GuardModeEnforce
	base.Advanced.Guard.DefaultProfile = promptfilter.GuardProfileBalanced
	handler := newPromptFilterBindingTestHandler(t, base, []database.PromptFilterNewAPIBinding{{
		APIKeyID: 101, PlatformCode: "gateway-a", Secret: "gateway-a-secret", Enabled: true,
		PolicyMode: database.PromptFilterPolicyModeWarn, PolicyProfile: database.PromptFilterPolicyProfileResearch,
	}})
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	c.Set(contextAPIKeyID, int64(101))
	cfg := handler.promptFilterConfigForRequest(c)
	if cfg.Mode != promptfilter.ModeBlock || cfg.Advanced.Guard.Mode != promptfilter.GuardModeEnforce || cfg.Advanced.Guard.DefaultProfile != promptfilter.GuardProfileBalanced {
		t.Fatalf("binding changed unified request policy: mode=%q guard=%q profile=%q", cfg.Mode, cfg.Advanced.Guard.Mode, cfg.Advanced.Guard.DefaultProfile)
	}
}

func TestRequiredBoundIdentityFailsWithoutPolicyPenalty(t *testing.T) {
	gin.SetMode(gin.TestMode)
	body := []byte(`{"model":"gpt-5.5","input":"hello"}`)
	handler := newPromptFilterBindingTestHandler(t, promptGuardTestConfig(), []database.PromptFilterNewAPIBinding{{
		APIKeyID: 101, PlatformCode: "gateway-a", Secret: "gateway-a-secret", Enabled: true, RequireSignedIdentity: true,
		PolicyMode: database.PromptFilterPolicyModeEnforce, PolicyProfile: database.PromptFilterPolicyProfileBalanced,
	}})

	missingRecorder := httptest.NewRecorder()
	missing, _ := gin.CreateTestContext(missingRecorder)
	missing.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	missing.Set(contextAPIKeyID, int64(101))
	if !handler.rejectRequiredNewAPIIdentity(missing, handler.promptFilterConfigForRequest(missing).Advanced.NewAPI, body) {
		t.Fatal("missing signed identity was not rejected")
	}
	if missingRecorder.Code != http.StatusUnauthorized || !strings.Contains(missingRecorder.Body.String(), "newapi_signed_identity_required") {
		t.Fatalf("missing identity response = %d %s", missingRecorder.Code, missingRecorder.Body.String())
	}
	assertNoPromptPolicyPenaltyHeaders(t, missingRecorder.Header())

	missingMeta, missingMetaRecorder := signedNewAPIPolicyContextWithSecret(t, "missing-meta", newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}, "/v1/responses", body, "gateway-a-secret")
	missingMeta.Set(contextAPIKeyID, int64(101))
	if !handler.rejectRequiredNewAPIIdentity(missingMeta, handler.promptFilterConfigForRequest(missingMeta).Advanced.NewAPI, body) {
		t.Fatal("missing signed platform metadata was not rejected")
	}
	if missingMetaRecorder.Code != http.StatusUnauthorized || !strings.Contains(missingMetaRecorder.Body.String(), "newapi_platform_identity_required") {
		t.Fatalf("missing platform meta response = %d %s", missingMetaRecorder.Code, missingMetaRecorder.Body.String())
	}
	assertNoPromptPolicyPenaltyHeaders(t, missingMetaRecorder.Header())

	wrongPlatform := signedBoundNewAPIPolicyContext(t, "wrong-platform-required", newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}, body, 101, "gateway-b", "gateway-a-secret", "")
	if !handler.rejectRequiredNewAPIIdentity(wrongPlatform, handler.promptFilterConfigForRequest(wrongPlatform).Advanced.NewAPI, body) {
		t.Fatal("mismatched signed platform identity was not rejected")
	}
	if status := wrongPlatform.Writer.Status(); status != http.StatusUnauthorized {
		t.Fatalf("wrong platform status = %d, want 401", status)
	}
	assertNoPromptPolicyPenaltyHeaders(t, wrongPlatform.Writer.Header())
}

func TestAuthMiddlewareEnforcesBoundIdentityAndRestoresV1Body(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "codex2api.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	apiKey := "sk-bound-platform-test-1234567890"
	apiKeyID, err := db.InsertAPIKey(context.Background(), "bound platform", apiKey)
	if err != nil {
		t.Fatal(err)
	}
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1})
	store.SetPromptFilterConfig(promptGuardTestConfig())
	store.ReplacePromptFilterNewAPIBindings([]*database.PromptFilterNewAPIBinding{{
		APIKeyID: apiKeyID, PlatformCode: "gateway-a", Secret: "gateway-a-secret", Enabled: true, RequireSignedIdentity: true,
		PolicyMode: database.PromptFilterPolicyModeEnforce, PolicyProfile: database.PromptFilterPolicyProfileBalanced,
	}})
	handler := NewHandler(store, db, nil, nil)
	handler.SetRuntimeCache(cache.NewMemory(1))
	router := gin.New()
	v1 := router.Group("/v1")
	v1.Use(handler.authMiddleware())
	v1.GET("/models", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	v1.POST("/responses", func(c *gin.Context) {
		body, readErr := io.ReadAll(c.Request.Body)
		if readErr != nil {
			c.String(http.StatusInternalServerError, readErr.Error())
			return
		}
		c.Data(http.StatusOK, "application/json", body)
	})
	v1.POST("/messages", func(c *gin.Context) { c.Status(http.StatusNoContent) })
	v1.POST("/messages/count_tokens", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	missing := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	missing.Header.Set("Authorization", "Bearer "+apiKey)
	missingRecorder := httptest.NewRecorder()
	router.ServeHTTP(missingRecorder, missing)
	if missingRecorder.Code != http.StatusUnauthorized || !strings.Contains(missingRecorder.Body.String(), "newapi_signed_identity_required") {
		t.Fatalf("unsigned V1 GET = %d %s", missingRecorder.Code, missingRecorder.Body.String())
	}
	assertNoPromptPolicyPenaltyHeaders(t, missingRecorder.Header())

	for _, path := range []string{"/v1/messages", "/v1/messages/count_tokens"} {
		anthropicBody := []byte(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hello"}]}`)
		unsigned := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(anthropicBody))
		unsigned.Header.Set("Authorization", "Bearer "+apiKey)
		unsignedRecorder := httptest.NewRecorder()
		router.ServeHTTP(unsignedRecorder, unsigned)
		if unsignedRecorder.Code != http.StatusUnauthorized || gjson.GetBytes(unsignedRecorder.Body.Bytes(), "type").String() != "error" || gjson.GetBytes(unsignedRecorder.Body.Bytes(), "error.type").String() != "authentication_error" || !strings.Contains(gjson.GetBytes(unsignedRecorder.Body.Bytes(), "error.message").String(), "NewAPI") {
			t.Fatalf("unsigned Anthropic endpoint %s = %d %s", path, unsignedRecorder.Code, unsignedRecorder.Body.String())
		}
		assertNoPromptPolicyPenaltyHeaders(t, unsignedRecorder.Header())
	}

	wrongPlatformBody := []byte(`{"model":"claude-sonnet-4","messages":[{"role":"user","content":"hello"}]}`)
	wrongPlatform := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(wrongPlatformBody))
	wrongPlatform.Header.Set("Authorization", "Bearer "+apiKey)
	setSignedNewAPIRequestHeaders(t, wrongPlatform, wrongPlatformBody, "messages-wrong-platform", newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}, "gateway-b", "gateway-a-secret", "")
	wrongPlatformRecorder := httptest.NewRecorder()
	router.ServeHTTP(wrongPlatformRecorder, wrongPlatform)
	if wrongPlatformRecorder.Code != http.StatusUnauthorized || gjson.GetBytes(wrongPlatformRecorder.Body.Bytes(), "type").String() != "error" || gjson.GetBytes(wrongPlatformRecorder.Body.Bytes(), "error.type").String() != "authentication_error" {
		t.Fatalf("wrong-platform Anthropic response = %d %s", wrongPlatformRecorder.Code, wrongPlatformRecorder.Body.String())
	}
	assertNoPromptPolicyPenaltyHeaders(t, wrongPlatformRecorder.Header())

	body := []byte(`{"model":"gpt-5.5","input":"hello"}`)
	valid := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	valid.Header.Set("Authorization", "Bearer "+apiKey)
	setSignedNewAPIRequestHeaders(t, valid, body, "v1-valid", newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}, "gateway-a", "gateway-a-secret", "")
	validRecorder := httptest.NewRecorder()
	router.ServeHTTP(validRecorder, valid)
	if validRecorder.Code != http.StatusOK || !bytes.Equal(validRecorder.Body.Bytes(), body) {
		t.Fatalf("signed V1 POST did not retain body: status=%d body=%s", validRecorder.Code, validRecorder.Body.String())
	}

	verifyRouter := gin.New()
	handler.RegisterRoutes(verifyRouter)
	verifyRequest := httptest.NewRequest(http.MethodPost, "/v1/prompt-filter/newapi/verify", nil)
	verifyRequest.Header.Set("Authorization", "Bearer "+apiKey)
	setSignedNewAPIRequestHeaders(t, verifyRequest, nil, "v1-binding-handshake", newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}, "gateway-a", "gateway-a-secret", "")
	verifyRecorder := httptest.NewRecorder()
	verifyRouter.ServeHTTP(verifyRecorder, verifyRequest)
	if verifyRecorder.Code != http.StatusOK || gjson.GetBytes(verifyRecorder.Body.Bytes(), "platform").String() != "gateway-a" {
		t.Fatalf("authenticated V1 binding handshake = %d %s", verifyRecorder.Code, verifyRecorder.Body.String())
	}
}

func TestBoundRiskAndSessionStateArePlatformScoped(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := promptSessionTestConfig()
	cfg.Advanced.Risk.Enabled = true
	cfg.Advanced.Risk.WindowSeconds = 300
	cfg.Advanced.Risk.UserWeightPercent = 0
	cfg.Advanced.Risk.IPWeightPercent = 0
	cfg.Advanced.Risk.SessionWeightPercent = 100
	handler := newPromptFilterBindingTestHandler(t, promptfilter.NormalizeConfig(cfg), []database.PromptFilterNewAPIBinding{
		{APIKeyID: 101, PlatformCode: "gateway-a", Secret: "gateway-a-secret", Enabled: true, PolicyMode: database.PromptFilterPolicyModeInherit, PolicyProfile: database.PromptFilterPolicyProfileInherit},
		{APIKeyID: 202, PlatformCode: "gateway-b", Secret: "gateway-b-secret", Enabled: true, PolicyMode: database.PromptFilterPolicyModeInherit, PolicyProfile: database.PromptFilterPolicyProfileInherit},
	})
	fingerprint := promptSessionTestFingerprint("same-session")
	identity := newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}
	verdict := promptfilter.Verdict{Enabled: true, Action: promptfilter.ActionBlock, Score: 50, SensitiveIntent: true, TerminalStrictHit: true}

	applyRisk := func(requestID string, apiKeyID int64, platform, secret string) promptfilter.Verdict {
		body := []byte(`{"input":"risk"}`)
		c := signedBoundNewAPIPolicyContext(t, requestID, identity, body, apiKeyID, platform, secret, fingerprint)
		setIngressRequestBodyIfAbsent(c, body)
		return handler.applyPromptRisk(c, verdict, handler.promptFilterConfigForRequest(c))
	}
	if got := applyRisk("risk-gateway-a-1", 101, "gateway-a", "gateway-a-secret"); got.RiskScore > verdict.Score {
		t.Fatalf("first gateway-a risk unexpectedly accumulated: %+v", got)
	}
	if got := applyRisk("risk-gateway-b-1", 202, "gateway-b", "gateway-b-secret"); got.RiskScore > verdict.Score {
		t.Fatalf("gateway-b inherited gateway-a risk: %+v", got)
	}
	if got := applyRisk("risk-gateway-a-2", 101, "gateway-a", "gateway-a-secret"); got.RiskScore <= verdict.Score {
		t.Fatalf("same-platform session risk did not accumulate: %+v", got)
	}

	seed := evaluateBoundPromptSession(t, handler, "session-gateway-a-seed", 101, "gateway-a", "gateway-a-secret", fingerprint, []byte(`{"input":"请记住这段平台隔离上下文。"}`))
	if seed.Decision.Action == promptfilter.ActionBlock {
		t.Fatalf("benign session seed blocked: %+v", seed.Decision)
	}
	buyContinuation := evaluateBoundPromptSession(t, handler, "session-gateway-b-cont", 202, "gateway-b", "gateway-b-secret", fingerprint, []byte(`{"input":"继续"}`))
	if promptEnvelopeHasOrigin(buyContinuation.Envelope, promptfilter.OriginSessionContext) {
		t.Fatalf("session context crossed platform boundary: %+v", buyContinuation.Envelope.Segments)
	}
	gatewayAContinuation := evaluateBoundPromptSession(t, handler, "session-gateway-a-cont", 101, "gateway-a", "gateway-a-secret", fingerprint, []byte(`{"input":"继续"}`))
	if !promptEnvelopeHasLinkedText(gatewayAContinuation.Envelope, promptfilter.OriginHistory, "请记住这段平台隔离上下文") {
		t.Fatalf("same-platform session context was not linked: %+v", gatewayAContinuation.Envelope.Segments)
	}
}

func newPromptFilterBindingTestHandler(t *testing.T, cfg promptfilter.Config, bindings []database.PromptFilterNewAPIBinding) *Handler {
	t.Helper()
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1})
	store.SetPromptFilterConfig(cfg)
	items := make([]*database.PromptFilterNewAPIBinding, 0, len(bindings))
	for index := range bindings {
		binding := bindings[index]
		items = append(items, &binding)
	}
	store.ReplacePromptFilterNewAPIBindings(items)
	handler := NewHandler(store, nil, nil, nil)
	handler.SetRuntimeCache(cache.NewMemory(1))
	return handler
}

func signedBoundNewAPIPolicyContext(t *testing.T, requestID string, identity newAPIIdentity, body []byte, apiKeyID int64, platform, secret, fingerprint string) *gin.Context {
	t.Helper()
	c, _ := signedNewAPIPolicyContextWithSecret(t, requestID, identity, "/v1/responses", body, secret)
	c.Set(contextAPIKeyID, apiKeyID)
	addSignedNewAPIPolicyMetaWithSecret(t, c, newAPIPolicyMeta{
		PlatformID: platform, Profile: promptfilter.GuardProfileBalanced, Mode: promptfilter.GuardModeEnforce,
		Provider: string(promptfilter.ModelFamilyOpenAI), Protocol: string(promptfilter.ProtocolResponses), SessionFingerprint: fingerprint,
	}, true, secret)
	return c
}

func evaluateBoundPromptSession(t *testing.T, handler *Handler, requestID string, apiKeyID int64, platform, secret, fingerprint string, body []byte) promptGuardEvaluation {
	t.Helper()
	c := signedBoundNewAPIPolicyContext(t, requestID, newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}, body, apiKeyID, platform, secret, fingerprint)
	setIngressRequestBodyIfAbsent(c, body)
	return handler.evaluatePromptGuard(c, body, body, "/v1/responses", "gpt-5.5", promptfilter.TransportHTTP)
}

func policyDecisionMetadataFromHeaders(header http.Header) newAPIPolicyDecisionMetadata {
	return newAPIPolicyDecisionMetadata{
		RequestID: header.Get("X-Codex2API-Policy-Request-ID"), DecisionID: header.Get("X-Codex2API-Policy-Decision-ID"),
		Action: header.Get("X-Codex2API-Policy-Action"), Profile: header.Get("X-Codex2API-Policy-Profile"),
		ReasonCode: header.Get("X-Codex2API-Policy-Reason"), Severity: header.Get("X-Codex2API-Policy-Severity"),
		StrikeEligible: header.Get("X-Codex2API-Policy-Strike-Eligible") == "true", RuleVersion: header.Get("X-Codex2API-Policy-Rule-Version"),
		EvidenceSHA256: header.Get("X-Codex2API-Policy-Evidence-SHA256"),
	}
}

func assertNoPromptPolicyPenaltyHeaders(t *testing.T, header http.Header) {
	t.Helper()
	for _, name := range []string{"X-Codex2API-Policy-Violation", "X-Codex2API-Policy-Strike", "X-Codex2API-Policy-Ban", "X-Codex2API-Policy-Response-Signature"} {
		if value := header.Get(name); value != "" {
			t.Fatalf("authentication failure emitted policy header %s=%q", name, value)
		}
	}
}

func setSignedNewAPIRequestHeaders(t *testing.T, req *http.Request, body []byte, requestID string, identity newAPIIdentity, platform, secret, fingerprint string) {
	t.Helper()
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	digest := sha256.Sum256(body)
	digestHex := hex.EncodeToString(digest[:])
	path := req.URL.EscapedPath()
	if path == "" {
		path = req.URL.Path
	}
	req.Header.Set("X-NewAPI-User-ID", identity.UserID)
	req.Header.Set("X-NewAPI-Client-IP", identity.ClientIP)
	req.Header.Set("X-NewAPI-Request-ID", requestID)
	req.Header.Set("X-NewAPI-Timestamp", timestamp)
	req.Header.Set("X-NewAPI-Method", req.Method)
	req.Header.Set("X-NewAPI-Path", path)
	req.Header.Set("X-NewAPI-Body-SHA256", digestHex)
	canonical := strings.Join([]string{"v1", timestamp, requestID, identity.UserID, identity.ClientIP, req.Method, path, digestHex}, "\n")
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(canonical))
	req.Header.Set("X-NewAPI-Signature", hex.EncodeToString(mac.Sum(nil)))
	metaPayload, err := json.Marshal(newAPIPolicyMeta{
		PlatformID: platform, Profile: promptfilter.GuardProfileBalanced, Mode: promptfilter.GuardModeEnforce,
		Provider: string(promptfilter.ModelFamilyOpenAI), Protocol: string(promptfilter.ProtocolResponses), SessionFingerprint: fingerprint,
	})
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(metaPayload)
	metaCanonical := strings.Join([]string{"policy-meta-v1", requestID, digestHex, encoded}, "\n")
	metaMAC := hmac.New(sha256.New, []byte(secret))
	_, _ = metaMAC.Write([]byte(metaCanonical))
	req.Header.Set("X-NewAPI-Policy-Meta", encoded)
	req.Header.Set("X-NewAPI-Policy-Meta-Signature", hex.EncodeToString(metaMAC.Sum(nil)))
}

func signedNewAPIPolicyContext(t *testing.T, requestID string, identity newAPIIdentity, path string, body []byte) (*gin.Context, *httptest.ResponseRecorder) {
	return signedNewAPIPolicyContextWithSecret(t, requestID, identity, path, body, "integration-secret")
}

func signedNewAPIPolicyContextWithSecret(t *testing.T, requestID string, identity newAPIIdentity, path string, body []byte, secret string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
	req.Header.Set("X-NewAPI-User-ID", identity.UserID)
	req.Header.Set("X-NewAPI-Client-IP", identity.ClientIP)
	req.Header.Set("X-NewAPI-Request-ID", requestID)
	req.Header.Set("X-NewAPI-Timestamp", timestamp)
	bodyDigest := sha256.Sum256(body)
	bodyDigestHex := hex.EncodeToString(bodyDigest[:])
	req.Header.Set("X-NewAPI-Method", http.MethodPost)
	req.Header.Set("X-NewAPI-Path", path)
	req.Header.Set("X-NewAPI-Body-SHA256", bodyDigestHex)
	canonical := strings.Join([]string{"v1", timestamp, requestID, identity.UserID, identity.ClientIP, http.MethodPost, path, bodyDigestHex}, "\n")
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(canonical))
	req.Header.Set("X-NewAPI-Signature", hex.EncodeToString(mac.Sum(nil)))
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = req
	c.Set(contextAPIKeyID, int64(101))
	return c, recorder
}

func addSignedNewAPIPolicyMeta(t *testing.T, c *gin.Context, meta newAPIPolicyMeta, valid bool) {
	if strings.TrimSpace(meta.PlatformID) == "" {
		meta.PlatformID = "test-platform"
	}
	addSignedNewAPIPolicyMetaWithSecret(t, c, meta, valid, "integration-secret")
}

func addSignedNewAPIPolicyMetaWithSecret(t *testing.T, c *gin.Context, meta newAPIPolicyMeta, valid bool, secret string) {
	t.Helper()
	payload, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	canonical := strings.Join([]string{"policy-meta-v1", c.GetHeader("X-NewAPI-Request-ID"), c.GetHeader("X-NewAPI-Body-SHA256"), encoded}, "\n")
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(canonical))
	signature := hex.EncodeToString(mac.Sum(nil))
	if !valid {
		signature = strings.Repeat("0", len(signature))
	}
	c.Request.Header.Set("X-NewAPI-Policy-Meta", encoded)
	c.Request.Header.Set("X-NewAPI-Policy-Meta-Signature", signature)
}
