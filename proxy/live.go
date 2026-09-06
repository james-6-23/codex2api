package proxy

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/proxy/liveattestation"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

const (
	defaultLiveMaxSessionDuration = time.Hour
	liveLeaseRefreshInterval      = 20 * time.Second
	liveUpstreamBodyLimit         = 2 << 20
	liveAttestationHeader         = "X-Oai-Attestation"
	liveCallCacheNamespace        = "live_call"
	liveAccountLeaseNamespace     = "live_acct"
	liveKeyLeaseNamespace         = "live_key"

	liveControllerPending  = "pending"
	liveControllerObserver = "observer"
	liveControllerProxy    = "proxy"
	liveControllerClosed   = "closed"
)

var (
	chatGPTLiveCallsURL        = "https://chatgpt.com/backend-api/codex/realtime/calls?intent=quicksilver&architecture=avas"
	chatGPTLiveSidebandBaseURL = "wss://chatgpt.com/backend-api/codex"
	// liveCallsURLForTest 仅测试替换上游 POST URL。
	liveCallsURLForTest               = ""
	liveMaxSessionDuration            = defaultLiveMaxSessionDuration
	liveAttestationUnavailableMessage = "ChatGPT Live requires an X-Oai-Attestation header on this host. DeviceCheck generation is only available on Apple Silicon with the official ChatGPT app installed."
)

var generateLiveAttestation = defaultGenerateLiveAttestation

func defaultGenerateLiveAttestation(ctx context.Context) (string, error) {
	return liveattestation.NewProvider().Generate(ctx)
}

var liveModelAliases = []string{"gpt-live", "gpt-live-1", "gpt-live-1-mini"}

type liveCallRequest struct {
	SDP     string          `json:"sdp"`
	Session json.RawMessage `json:"session"`
}

type liveCallRecord struct {
	CallID                string    `json:"call_id"`
	CallHash              string    `json:"call_hash"`
	AccountID             int64     `json:"account_id"`
	APIKeyID              int64     `json:"api_key_id"`
	LeaseID               string    `json:"lease_id"`
	Model                 string    `json:"model"`
	CreatedAt             time.Time `json:"created_at"`
	ExpiresAt             time.Time `json:"expires_at"`
	Controller            string    `json:"controller"`
	ControllerOwner       string    `json:"controller_owner,omitempty"`
	AttestationCiphertext string    `json:"attestation_ciphertext,omitempty"`
	UserAgent             string    `json:"user_agent,omitempty"`
	ClientIP              string    `json:"client_ip,omitempty"`
	InboundEndpoint       string    `json:"inbound_endpoint,omitempty"`
	RequestID             string    `json:"request_id,omitempty"`
	UpstreamRequestID     string    `json:"upstream_request_id,omitempty"`
	UpstreamProxyID       int64     `json:"upstream_proxy_id,omitempty"`
	UpstreamProxyName     string    `json:"upstream_proxy_name,omitempty"`
	UsageLogged           bool      `json:"usage_logged,omitempty"`
}

type liveAttestationError struct {
	Reason string
}

func (e *liveAttestationError) Error() string {
	if e == nil || e.Reason == "" {
		return liveAttestationUnavailableMessage
	}
	return e.Reason
}

func liveAccountFilter(account *auth.Account) bool {
	return account != nil && !account.IsRelayStyle()
}

func isLiveModelName(model string) bool {
	switch strings.ToLower(strings.TrimSpace(model)) {
	case "gpt-live", "gpt-live-1", "gpt-live-1-mini":
		return true
	default:
		return false
	}
}

func liveModelExplicitlyDenied(id string, deny []string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "" {
		return false
	}
	for _, item := range deny {
		if strings.ToLower(strings.TrimSpace(item)) == id {
			return true
		}
	}
	return false
}

func hashLiveCallID(callID string) string {
	sum := sha256.Sum256([]byte(callID))
	return fmt.Sprintf("%x", sum[:])
}

func liveSessionModel(session json.RawMessage) string {
	model := strings.TrimSpace(gjson.GetBytes(session, "model").String())
	if model == "" {
		return "gpt-live"
	}
	return model
}

func ValidateLiveCallRequest(request *liveCallRequest) error {
	if request == nil || strings.TrimSpace(request.SDP) == "" {
		return errors.New("sdp is required")
	}
	if len(request.Session) == 0 || !json.Valid(request.Session) {
		return errors.New("session must be valid JSON")
	}
	var sessionObject map[string]json.RawMessage
	if err := json.Unmarshal(request.Session, &sessionObject); err != nil || sessionObject == nil {
		return errors.New("session must be a JSON object")
	}
	return nil
}

func liveCallIDFromLocation(location string) (string, error) {
	location = strings.TrimSpace(location)
	if location == "" {
		return "", errors.New("live upstream response has no Location")
	}
	parsed, err := url.Parse(location)
	if err != nil {
		return "", fmt.Errorf("parse live Location: %w", err)
	}
	callID := strings.TrimSpace(path.Base(strings.TrimSuffix(parsed.Path, "/")))
	if callID == "" || callID == "." || callID == "codex" {
		return "", errors.New("live upstream Location has no call id")
	}
	return callID, nil
}

func liveSidebandLocation(fullPath, callID string) string {
	prefix := "/v1/live/"
	if strings.HasPrefix(fullPath, "/backend-api/codex/") {
		prefix = "/backend-api/codex/"
	}
	return prefix + url.PathEscape(callID)
}

func liveRequireAllowLive(c *gin.Context) bool {
	row := apiKeyRowFromContext(c)
	if row != nil && row.Limits.AllowLive {
		return true
	}
	api.SendErrorWithStatus(c, api.NewAPIError(
		api.ErrCodeInsufficientScope,
		"ChatGPT Live is not enabled for this API key",
		api.ErrorTypePermission,
	), http.StatusForbidden)
	return false
}

func parseLiveCallRequest(c *gin.Context) (*liveCallRequest, error) {
	contentType := strings.ToLower(c.GetHeader("Content-Type"))
	if strings.HasPrefix(contentType, "multipart/form-data") {
		request := &liveCallRequest{
			SDP:     c.PostForm("sdp"),
			Session: json.RawMessage(c.PostForm("session")),
		}
		if err := ValidateLiveCallRequest(request); err != nil {
			return nil, err
		}
		return request, nil
	}
	raw, err := io.ReadAll(io.LimitReader(c.Request.Body, liveUpstreamBodyLimit+1))
	if err != nil {
		return nil, errors.New("failed to read request body")
	}
	if len(raw) > liveUpstreamBodyLimit {
		return nil, errors.New("request body is too large")
	}
	var request liveCallRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		return nil, errors.New("request body must be valid JSON")
	}
	if err := ValidateLiveCallRequest(&request); err != nil {
		return nil, err
	}
	return &request, nil
}

// LiveCreate 处理 POST /v1/live 与 Codex realtime/calls。
func (h *Handler) LiveCreate(c *gin.Context) {
	if !liveRequireAllowLive(c) {
		return
	}
	request, err := parseLiveCallRequest(c)
	if err != nil {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidRequest, err.Error(), api.ErrorTypeInvalidRequest))
		return
	}
	if h.enforceAPIKeyLimitsAndReply(c, "") {
		return
	}
	releaseKey, ok := h.acquireAPIKeyConcurrency(c)
	if !ok {
		return
	}
	holdKey := false
	defer func() {
		if !holdKey && releaseKey != nil {
			releaseKey()
		}
	}()

	attestation, ciphertext, err := h.prepareLiveAttestation(c.Request.Context(), c.GetHeader(liveAttestationHeader))
	if err != nil {
		var attErr *liveAttestationError
		if errors.As(err, &attErr) {
			api.SendErrorWithStatus(c, api.NewAPIError(api.ErrCodeServiceUnavailable, attErr.Error(), api.ErrorTypeServer), http.StatusServiceUnavailable)
			return
		}
		api.SendErrorWithStatus(c, api.NewAPIError(api.ErrCodeServiceUnavailable, err.Error(), api.ErrorTypeServer), http.StatusServiceUnavailable)
		return
	}

	apiKeyID := requestAPIKeyID(c)
	apiKey := strings.TrimSpace(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "))
	filter := applyAffinityGroupRouting(c, resolveRequestSessionIdentity(c.Request.Header, request.Session), liveAccountFilter)
	excluded := map[int64]bool{}
	var lastStatus int
	var lastBody []byte
	var lastContentType string
	for attempt := 0; attempt <= 3; attempt++ {
		account := h.store.NextExcludingWithFilter(apiKeyID, excluded, filter)
		if account == nil {
			if lastStatus != 0 {
				if lastContentType == "" {
					lastContentType = "application/json"
				}
				c.Data(lastStatus, lastContentType, lastBody)
				return
			}
			api.SendError(c, api.ErrServiceUnavailable)
			return
		}

		created, status, contentType, body, createErr := h.createUpstreamLiveCall(c.Request.Context(), account, request, attestation, c.Request.Header, apiKey)
		if createErr != nil || created == nil {
			excluded[account.ID()] = true
			h.store.Release(account)
			if status >= 400 && status < 500 && status != http.StatusTooManyRequests {
				if contentType == "" {
					contentType = "application/json"
				}
				c.Data(status, contentType, body)
				return
			}
			if status != 0 {
				lastStatus, lastBody, lastContentType = status, body, contentType
			}
			continue
		}

		now := time.Now()
		trace := snapshotUpstreamTrace(c.Request.Context())
		record := &liveCallRecord{
			RequestID:             trace.RequestID,
			UpstreamRequestID:     trace.UpstreamRequestID,
			UpstreamProxyID:       trace.Proxy.ID,
			UpstreamProxyName:     trace.Proxy.Name,
			CallID:                created.callID,
			CallHash:              hashLiveCallID(created.callID),
			AccountID:             account.ID(),
			APIKeyID:              apiKeyID,
			LeaseID:               uuid.NewString(),
			Model:                 liveSessionModel(request.Session),
			CreatedAt:             now,
			ExpiresAt:             now.Add(liveMaxSessionDuration),
			Controller:            liveControllerPending,
			AttestationCiphertext: ciphertext,
			UserAgent:             c.GetHeader("User-Agent"),
			ClientIP:              c.ClientIP(),
			InboundEndpoint:       liveInboundEndpoint(c),
		}
		if err := h.liveCalls().save(c.Request.Context(), record, account, releaseKey); err != nil {
			h.store.Release(account)
			log.Printf("live: save call mapping: %v", err)
			api.SendErrorWithStatus(c, api.NewAPIError(api.ErrCodeServiceUnavailable, "Live call mapping unavailable", api.ErrorTypeServer), http.StatusServiceUnavailable)
			return
		}
		holdKey = true
		c.Header("Location", liveSidebandLocation(c.FullPath(), created.callID))
		c.Data(http.StatusOK, "application/sdp", created.sdp)
		return
	}
	if lastStatus != 0 {
		if lastContentType == "" {
			lastContentType = "application/json"
		}
		c.Data(lastStatus, lastContentType, lastBody)
		return
	}
	api.SendError(c, api.ErrServiceUnavailable)
}

type liveCallCreated struct {
	sdp    []byte
	callID string
}

func (h *Handler) createUpstreamLiveCall(
	ctx context.Context,
	account *auth.Account,
	request *liveCallRequest,
	attestation string,
	downstream http.Header,
	apiKey string,
) (*liveCallCreated, int, string, []byte, error) {
	body, err := json.Marshal(struct {
		SDP     string          `json:"sdp"`
		Session json.RawMessage `json:"session"`
	}{SDP: request.SDP, Session: request.Session})
	if err != nil {
		return nil, 0, "", nil, err
	}
	endpoint := chatGPTLiveCallsURL
	if liveCallsURLForTest != "" {
		endpoint = liveCallsURLForTest
	}
	reqCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, 0, "", nil, err
	}
	h.applyLiveUpstreamHeaders(req, account, attestation, downstream, apiKey)

	proxyURL := ""
	if h.store != nil {
		proxyURL = h.store.ResolveProxyForAccount(account)
	}
	resp, err := getCodexMaintenanceClient(account, proxyURL).Do(req)
	if err != nil {
		return nil, 0, "", nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, liveUpstreamBodyLimit+1))
	if err != nil {
		return nil, resp.StatusCode, resp.Header.Get("Content-Type"), nil, err
	}
	if len(responseBody) > liveUpstreamBodyLimit {
		return nil, resp.StatusCode, resp.Header.Get("Content-Type"), nil, errors.New("live upstream response is too large")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("live: upstream create %d: %s", resp.StatusCode, truncateForLog(responseBody, 512))
		return nil, resp.StatusCode, resp.Header.Get("Content-Type"), responseBody, fmt.Errorf("live upstream status %d", resp.StatusCode)
	}
	callID, err := liveCallIDFromLocation(resp.Header.Get("Location"))
	if err != nil {
		return nil, resp.StatusCode, resp.Header.Get("Content-Type"), responseBody, err
	}
	return &liveCallCreated{sdp: responseBody, callID: callID}, resp.StatusCode, resp.Header.Get("Content-Type"), responseBody, nil
}

func (h *Handler) applyLiveUpstreamHeaders(req *http.Request, account *auth.Account, attestation string, downstream http.Header, apiKey string) {
	if req == nil || account == nil {
		return
	}
	accessToken := account.GetAccessToken()
	userAgent, version := ResolveCodexOutboundClientHeaders(account, apiKey, h.deviceCfg, downstream)
	if account.IsCodexAgentIdentity() {
		if assertion, err := account.BuildCodexAgentAssertion(time.Now()); err == nil {
			req.Header.Set("Authorization", assertion)
		} else {
			req.Header.Set("Authorization", "Bearer "+accessToken)
		}
	} else {
		req.Header.Set("Authorization", "Bearer "+accessToken)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/sdp")
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Originator", Originator)
	if version != "" {
		req.Header.Set("Version", version)
	}
	if accountID := account.EffectiveAccountID(); accountID != "" {
		req.Header.Set("chatgpt-account-id", accountID)
	}
	applyCodexAllowedForwardHeaders(req, downstream)
	ApplyCodexFingerprintHeaders(req.Header, account, downstream)
	applyAccountCustomHeaders(req, account)
	req.Header.Set("OpenAI-Alpha", "quicksilver=v2")
	req.Header.Del("OpenAI-Beta")
	if strings.TrimSpace(req.Header.Get("session-id")) == "" {
		req.Header.Set("session-id", uuid.NewString())
	}
	if strings.TrimSpace(req.Header.Get("thread-id")) == "" {
		req.Header.Set("thread-id", uuid.NewString())
	}
	if strings.TrimSpace(attestation) != "" {
		req.Header.Set(liveAttestationHeader, attestation)
	}
}

func (h *Handler) prepareLiveAttestation(ctx context.Context, clientHeader string) (string, string, error) {
	header := strings.TrimSpace(clientHeader)
	if header == "" {
		generated, err := generateLiveAttestation(ctx)
		if err != nil || strings.TrimSpace(generated) == "" {
			reason := liveAttestationUnavailableMessage
			if err != nil && !errors.Is(err, liveattestation.ErrUnsupportedPlatform) && !errors.Is(err, liveattestation.ErrChatGPTAppMissing) {
				reason = "ChatGPT Live attestation is unavailable: " + err.Error()
			}
			return "", "", &liveAttestationError{Reason: reason}
		}
		header = strings.TrimSpace(generated)
	}
	ciphertext, err := encryptLiveAttestation(header, h.liveAttestationSecret())
	if err != nil {
		return "", "", &liveAttestationError{Reason: "failed to protect the Live attestation"}
	}
	return header, ciphertext, nil
}

func (h *Handler) decryptLiveAttestation(record *liveCallRecord) (string, error) {
	if record == nil || strings.TrimSpace(record.AttestationCiphertext) == "" {
		return "", &liveAttestationError{Reason: "the Live call has no reusable attestation"}
	}
	plain, err := decryptLiveAttestation(record.AttestationCiphertext, h.liveAttestationSecret())
	if err != nil {
		return "", &liveAttestationError{Reason: "the Live call attestation cannot be decrypted on this instance"}
	}
	return plain, nil
}

func (h *Handler) liveAttestationSecret() string {
	if h != nil && h.cfg != nil {
		return strings.TrimSpace(h.cfg.AdminSecret)
	}
	return ""
}

func liveInboundEndpoint(c *gin.Context) string {
	if c == nil {
		return "/v1/live"
	}
	if path := strings.TrimSpace(c.FullPath()); path != "" {
		return path
	}
	if c.Request != nil {
		return c.Request.URL.Path
	}
	return "/v1/live"
}

func liveAttestationKey(secret string) [32]byte {
	return sha256.Sum256([]byte("codex2api/live-attestation/v1\x00" + secret))
}

func encryptLiveAttestation(plaintext, secret string) (string, error) {
	key := liveAttestationKey(secret)
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	encrypted := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.RawStdEncoding.EncodeToString(encrypted), nil
}

func decryptLiveAttestation(ciphertext, secret string) (string, error) {
	encrypted, err := base64.RawStdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", err
	}
	key := liveAttestationKey(secret)
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(encrypted) < gcm.NonceSize() {
		return "", errors.New("encrypted Live attestation is too short")
	}
	plain, err := gcm.Open(nil, encrypted[:gcm.NonceSize()], encrypted[gcm.NonceSize():], nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}
