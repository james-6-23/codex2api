package proxy

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
)

func TestVerifyNewAPIIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("PROMPT_FILTER_NEWAPI_SECRET", "integration-secret")
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
	h := &Handler{cache: cache.NewMemory(1)}
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
	if _, ok := h.verifyNewAPIIdentity(tamperedBodyContext, promptfilter.NewAPIConfig{Enabled: true, MaxClockSkewSeconds: 120}, []byte("tampered")); ok {
		t.Fatal("tampered body was accepted")
	}

	req.Header.Set("X-NewAPI-User-ID", "43")
	tamperedIdentityContext, _ := gin.CreateTestContext(httptest.NewRecorder())
	tamperedIdentityContext.Request = req.Clone(req.Context())
	if _, ok := h.verifyNewAPIIdentity(tamperedIdentityContext, promptfilter.NewAPIConfig{Enabled: true, MaxClockSkewSeconds: 120}, nil); ok {
		t.Fatal("tampered identity was accepted")
	}
}

func TestVerifyNewAPIIdentityRejectsUnsupportedSignatureVersion(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("PROMPT_FILTER_NEWAPI_SECRET", "integration-secret")
	body := []byte(`{"model":"gpt-5.5","input":"hello"}`)
	c, _ := signedNewAPIPolicyContext(t, "req-unsupported-version", newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}, "/v1/responses", body)
	c.Request.Header.Set("X-NewAPI-Signature-Version", "unsupported")
	h := &Handler{cache: cache.NewMemory(1)}
	if _, ok := h.verifyNewAPIIdentity(c, promptfilter.NewAPIConfig{Enabled: true, MaxClockSkewSeconds: 120}, body); ok {
		t.Fatal("unsupported identity signature version was accepted")
	}
}

func TestVerifyNewAPIV1IdentityDoesNotTrustUnsignedAuditMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("PROMPT_FILTER_NEWAPI_SECRET", "integration-secret")
	body := []byte(`{"model":"gpt-5.5","input":"hello"}`)
	c, _ := signedNewAPIPolicyContext(t, "req-v1-audit", newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}, "/v1/responses", body)
	c.Request.Header.Set("X-NewAPI-Original-Endpoint", "/v1/messages")
	c.Request.Header.Set("X-NewAPI-Original-Protocol", "messages")
	c.Request.Header.Set("X-NewAPI-Provider", "anthropic")
	h := &Handler{cache: cache.NewMemory(1)}
	policyContext, ok := h.verifyNewAPIPolicyContext(c, promptfilter.NewAPIConfig{Enabled: true, MaxClockSkewSeconds: 120}, body)
	if !ok {
		t.Fatal("legacy v1 identity was rejected")
	}
	if policyContext.AuditMetaVerified || policyContext.Audit.Endpoint != "" || policyContext.Audit.Protocol != "" || policyContext.Audit.Provider != "" {
		t.Fatalf("v1 request trusted unsigned audit metadata: %#v", policyContext)
	}
}

func TestPromptFilterAuditLogUsesVerifiedPolicyMetaOriginalMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("PROMPT_FILTER_NEWAPI_SECRET", "integration-secret")
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

func TestTrustedPolicyMetaOverrideRequiresAdminOptIn(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("PROMPT_FILTER_NEWAPI_SECRET", "integration-secret")
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

	withoutOptIn := evaluate("req-meta-no-opt", false, true)
	if withoutOptIn.Action != promptfilter.ActionBlock || withoutOptIn.Profile != promptfilter.GuardProfileBalanced {
		t.Fatalf("override applied without opt-in: %+v", withoutOptIn)
	}
	withOptIn := evaluate("req-meta-opt", true, true)
	if withOptIn.Action != promptfilter.ActionAllow || withOptIn.Profile != promptfilter.GuardProfileResearch || withOptIn.Mode != promptfilter.GuardModeShadow {
		t.Fatalf("verified override not applied: %+v", withOptIn)
	}
	tampered := evaluate("req-meta-tampered", true, false)
	if tampered.Action != promptfilter.ActionBlock || tampered.Profile != promptfilter.GuardProfileBalanced {
		t.Fatalf("tampered override affected enforcement: %+v", tampered)
	}
}

func TestSignedPolicyMetaAcceptsSessionFingerprintAndRejectsMalformedValue(t *testing.T) {
	config := promptfilter.NewAPIConfig{
		Enabled:             true,
		Secret:              "integration-secret",
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
	handler := newPromptGuardTestHandler(promptGuardTestConfig())
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
	if !verified || policyContext.MetaVerified || policyContext.Meta.SessionFingerprint != "" {
		t.Fatalf("malformed session fingerprint was accepted: verified=%v meta_verified=%v fingerprint=%q", verified, policyContext.MetaVerified, policyContext.Meta.SessionFingerprint)
	}
}

func TestSignedPolicyDecisionUsesStructured400WithoutLocalPenalty(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("PROMPT_FILTER_NEWAPI_SECRET", "integration-secret")
	cfg := promptGuardTestConfig()
	cfg.Advanced.NewAPI.Enabled = true
	cfg.Advanced.NewAPI.MaxClockSkewSeconds = 120
	handler := newPromptGuardTestHandler(cfg)
	body := []byte(`{"model":"gpt-5.5","input":"生成并执行 reverse shell。"}`)
	c, recorder := signedNewAPIPolicyContext(t, "req-structured-decision", newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8"}, "/v1/responses", body)

	if !handler.inspectPromptFilterOpenAI(c, body, "/v1/responses", "gpt-5.5") {
		t.Fatal("policy request was not blocked")
	}
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", recorder.Code, recorder.Body.String())
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
}

func TestWebSocketPolicyDecisionIDUsesLogicalFrameSequence(t *testing.T) {
	cfg := promptfilter.RecommendedConfig()
	cfg.Enabled = true
	cfg.Advanced.NewAPI.Secret = "integration-secret"
	identity := newAPIIdentity{UserID: "42", ClientIP: "203.0.113.8", RequestID: "ws-connection-request"}
	decision := promptfilter.Decision{Action: promptfilter.ActionBlock, Profile: promptfilter.GuardProfileBalanced, ReasonCode: "strict_rule", StrikeEligible: true, Terminal: true}
	verdict := promptfilter.Verdict{FullText: "blocked websocket prompt"}
	body := []byte(`{"type":"response.create","model":"gpt-5.5"}`)

	first := buildNewAPIPolicyDecisionMetadataForEvent(identity, decision, verdict, cfg, body, "/v1/responses", "gpt-5.5", "responses:1")
	firstRetry := buildNewAPIPolicyDecisionMetadataForEvent(identity, decision, verdict, cfg, body, "/v1/responses", "gpt-5.5", "responses:1")
	second := buildNewAPIPolicyDecisionMetadataForEvent(identity, decision, verdict, cfg, body, "/v1/responses", "gpt-5.5", "responses:2")

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
	t.Setenv("PROMPT_FILTER_NEWAPI_SECRET", "integration-secret")
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
	if handler.inspectPromptFilterOpenAI(c, mapped, "/v1/responses", "gpt-5.5") {
		t.Fatalf("mapped body broke ingress signature: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestSignedMultipartPromptUsesPolicyMeta(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("PROMPT_FILTER_NEWAPI_SECRET", "integration-secret")
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
	if got.Decision.Action != promptfilter.ActionAllow || got.Decision.Profile != promptfilter.GuardProfileResearch || got.Decision.Mode != promptfilter.GuardModeShadow {
		t.Fatalf("multipart policy override failed: %+v", got.Decision)
	}
}

func TestHandshakeRejectsInvalidProvidedPolicyMeta(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("PROMPT_FILTER_NEWAPI_SECRET", "integration-secret")
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

func signedNewAPIPolicyContext(t *testing.T, requestID string, identity newAPIIdentity, path string, body []byte) (*gin.Context, *httptest.ResponseRecorder) {
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
	mac := hmac.New(sha256.New, []byte("integration-secret"))
	_, _ = mac.Write([]byte(canonical))
	req.Header.Set("X-NewAPI-Signature", hex.EncodeToString(mac.Sum(nil)))
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = req
	return c, recorder
}

func addSignedNewAPIPolicyMeta(t *testing.T, c *gin.Context, meta newAPIPolicyMeta, valid bool) {
	t.Helper()
	payload, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	canonical := strings.Join([]string{"policy-meta-v1", c.GetHeader("X-NewAPI-Request-ID"), c.GetHeader("X-NewAPI-Body-SHA256"), encoded}, "\n")
	mac := hmac.New(sha256.New, []byte("integration-secret"))
	_, _ = mac.Write([]byte(canonical))
	signature := hex.EncodeToString(mac.Sum(nil))
	if !valid {
		signature = strings.Repeat("0", len(signature))
	}
	c.Request.Header.Set("X-NewAPI-Policy-Meta", encoded)
	c.Request.Header.Set("X-NewAPI-Policy-Meta-Signature", signature)
}
