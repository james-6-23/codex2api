package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
)

const (
	promptSessionCorrelationNamespace  = "prompt-filter-session-correlation"
	promptAttachmentCacheNamespace     = "prompt-filter-attachment-cache"
	promptExtensionBreakerNamespace    = "prompt-filter-extension-breaker"
	promptAttachmentResponseMaxBytes   = 64 * 1024
	promptGuardSessionReadTimeout      = 50 * time.Millisecond
	promptGuardPolicyEventIDContextKey = "prompt_filter_policy_event_id"
)

type promptSessionCorrelationRecord struct {
	Fragments     []string  `json:"fragments"`
	UpdatedAt     time.Time `json:"updated_at"`
	LastRequestID string    `json:"last_request_id,omitempty"`
}

type promptAttachmentRequest struct {
	References        []string `json:"references"`
	Endpoint          string   `json:"endpoint,omitempty"`
	Model             string   `json:"model,omitempty"`
	MaxFiles          int      `json:"max_files"`
	MaxBytes          int      `json:"max_bytes"`
	MaxExtractedChars int      `json:"max_extracted_chars"`
	AllowRemoteURLs   bool     `json:"allow_remote_urls"`
}

type promptAttachmentItem struct {
	Reference string `json:"reference,omitempty"`
	Text      string `json:"text"`
	MIME      string `json:"mime,omitempty"`
	SHA256    string `json:"sha256,omitempty"`
}

type promptAttachmentResponse struct {
	Items []promptAttachmentItem `json:"items"`
}

type promptExtensionBreakerRecord struct {
	Failures  int       `json:"failures"`
	OpenUntil time.Time `json:"open_until,omitempty"`
}

type promptExtensionLimiter struct {
	tokens chan struct{}
}

var promptExtensionLimiters sync.Map

func acquirePromptExtensionSlot(key string, limit int) (func(), bool) {
	if limit <= 0 {
		return func() {}, true
	}
	limiterKey := fmt.Sprintf("%s:%d", key, limit)
	value, _ := promptExtensionLimiters.LoadOrStore(limiterKey, &promptExtensionLimiter{tokens: make(chan struct{}, limit)})
	limiter := value.(*promptExtensionLimiter)
	select {
	case limiter.tokens <- struct{}{}:
		return func() { <-limiter.tokens }, true
	default:
		return func() {}, false
	}
}

type promptSessionCorrelationPending struct {
	Key         string
	CurrentText string
	RequestID   string
}

// enrichPromptGuardSession links only an explicitly continued prompt from a
// verified user/session pair. Explicit continuations inherit the preceding
// accepted user intent as linked history and therefore use the current-user
// guard policy. Heuristic short-fragment linking remains administrator opt-in
// and shadow-only session context.
func (h *Handler) enrichPromptGuardSession(c *gin.Context, cfg promptfilter.Config, signedBody []byte, envelope *promptfilter.RequestEnvelope) (*promptSessionCorrelationPending, error) {
	if h == nil || h.cache == nil || c == nil || envelope == nil || !cfg.Enabled || !cfg.Advanced.Session.Enabled {
		return nil, nil
	}
	sessionCfg := cfg.Advanced.Session
	policyContext, verified := h.verifyNewAPIPolicyContext(c, cfg.Advanced.NewAPI, signedBody)
	identityKey := ""
	sessionFingerprint := ""
	requestID := ""
	runtimeScope := ""
	trust := promptfilter.SegmentTrustClientSupplied
	if verified && policyContext.MetaVerified {
		identityKey = policyContext.Identity.UserID
		sessionFingerprint = policyContext.Meta.SessionFingerprint
		requestID = policyContext.Identity.RequestID
		runtimeScope = newAPIRuntimeScopeForPolicyContext(policyContext)
		trust = promptfilter.SegmentTrustGatewaySigned
	}
	if identityKey == "" && !sessionCfg.RequireSignedIdentity {
		identityKey = fmt.Sprintf("api-key:%d", requestAPIKeyID(c))
		sessionFingerprint = hashRiskIdentity(promptSessionID(c))
	}
	if strings.TrimSpace(identityKey) == "" || strings.TrimSpace(sessionFingerprint) == "" {
		return nil, nil
	}
	currentText := strings.TrimSpace(envelopeDirectCurrentUserText(*envelope))
	if currentText == "" {
		return nil, nil
	}
	currentText = truncatePromptRunes(currentText, sessionCfg.MaxTextLength)
	key := hashRiskIdentity(runtimeScope + "\x00" + identityKey + "\x00" + sessionFingerprint)
	if eventID := promptGuardPolicyEventID(c); requestID != "" && eventID != "" {
		requestID += "\x00" + eventID
	}
	pending := &promptSessionCorrelationPending{Key: key, CurrentText: currentText, RequestID: requestID}
	explicitContinuation := promptfilter.IsContinuationOnly(currentText)
	linkPrevious := explicitContinuation
	readForShortFragment := sessionCfg.CombineShortFragments && utf8.RuneCountInString(currentText) <= sessionCfg.ShortFragmentMaxChars
	if !linkPrevious && !readForShortFragment {
		// Ordinary requests only schedule the bounded post-decision write. They do
		// not pay a cache read RTT on the first-token path.
		return pending, nil
	}
	ctx, cancel := context.WithTimeout(promptGuardRequestContext(c), promptGuardSessionReadTimeout)
	defer cancel()
	var record promptSessionCorrelationRecord
	if raw, ok, err := h.cache.GetRuntime(ctx, promptSessionCorrelationNamespace, key); err == nil && ok {
		_ = json.Unmarshal(raw, &record)
	} else if err != nil {
		return pending, err
	}
	if requestID != "" && record.LastRequestID == requestID {
		return nil, nil
	}
	if !linkPrevious && readForShortFragment {
		linkPrevious = len(record.Fragments) > 0
	}
	if linkPrevious && len(record.Fragments) > 0 {
		linkedText := truncatePromptRunes(strings.Join(record.Fragments, "\n"), sessionCfg.MaxTextLength)
		if linkedText != "" {
			origin := promptfilter.OriginSessionContext
			role := "session"
			linked := false
			if explicitContinuation {
				origin = promptfilter.OriginHistory
				role = "user"
				linked = true
			}
			minimumSequence := 0
			for _, segment := range envelope.Segments {
				if segment.Sequence < minimumSequence {
					minimumSequence = segment.Sequence
				}
			}
			envelope.Segments = append(envelope.Segments, promptfilter.Segment{
				Origin:   origin,
				Role:     role,
				Text:     linkedText,
				Sequence: minimumSequence - 1,
				Linked:   linked,
				Trust:    trust,
			})
		}
	}
	return pending, nil
}

func promptGuardPolicyEventID(c *gin.Context) string {
	if c == nil {
		return ""
	}
	value, _ := c.Get(promptGuardPolicyEventIDContextKey)
	eventID, _ := value.(string)
	return normalizeNewAPIPolicyWebSocketEventID(eventID)
}

func envelopeDirectCurrentUserText(envelope promptfilter.RequestEnvelope) string {
	parts := make([]string, 0, 2)
	for _, segment := range envelope.Segments {
		if segment.Origin != promptfilter.OriginCurrentUser {
			continue
		}
		if text := strings.TrimSpace(segment.Text); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, "\n")
}

// commitPromptGuardSession stores only requests that were not blocked. This
// prevents rejected payloads from poisoning later continuation checks.
func (h *Handler) commitPromptGuardSession(c *gin.Context, cfg promptfilter.Config, pending *promptSessionCorrelationPending, decision promptfilter.Decision) error {
	if h == nil || h.cache == nil || pending == nil || pending.Key == "" || decision.Action == promptfilter.ActionBlock {
		return nil
	}
	ctx := promptGuardRequestContext(c)
	unlock, acquired := acquirePromptRuntimeLease(ctx, h.cache, promptSessionCorrelationNamespace, pending.Key)
	if !acquired {
		return nil
	}
	defer unlock()
	var record promptSessionCorrelationRecord
	if raw, ok, err := h.cache.GetRuntime(ctx, promptSessionCorrelationNamespace, pending.Key); err == nil && ok {
		_ = json.Unmarshal(raw, &record)
	} else if err != nil {
		return err
	}
	if pending.RequestID != "" && record.LastRequestID == pending.RequestID {
		return nil
	}
	record.Fragments = append(record.Fragments, pending.CurrentText)
	if len(record.Fragments) > cfg.Advanced.Session.MaxFragments {
		record.Fragments = append([]string(nil), record.Fragments[len(record.Fragments)-cfg.Advanced.Session.MaxFragments:]...)
	}
	record.UpdatedAt = time.Now()
	record.LastRequestID = pending.RequestID
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return h.cache.SetRuntime(ctx, promptSessionCorrelationNamespace, pending.Key, raw, time.Duration(cfg.Advanced.Session.WindowSeconds)*time.Second)
}

// enrichPromptGuardAttachments delegates binary/PDF/OCR extraction to a
// bounded parser service. Codex2API never fetches user-provided URLs directly,
// which keeps SSRF and decompression-bomb handling outside the request process.
func (h *Handler) enrichPromptGuardAttachments(ctx context.Context, cfg promptfilter.Config, envelope *promptfilter.RequestEnvelope) error {
	if h == nil || envelope == nil || !cfg.Advanced.Attachment.Enabled {
		return nil
	}
	attachmentCfg := cfg.Advanced.Attachment
	baseURL := strings.TrimRight(strings.TrimSpace(attachmentCfg.BaseURL), "/")
	if baseURL == "" {
		return fmt.Errorf("attachment parser base URL is empty")
	}
	references := promptAttachmentReferences(*envelope, attachmentCfg.MaxFiles, attachmentCfg.MaxBytes, attachmentCfg.AllowRemoteURLs)
	if len(references) == 0 {
		return nil
	}
	payload := promptAttachmentRequest{
		References:        references,
		Endpoint:          envelope.Endpoint,
		Model:             envelope.EffectiveModel,
		MaxFiles:          attachmentCfg.MaxFiles,
		MaxBytes:          attachmentCfg.MaxBytes,
		MaxExtractedChars: attachmentCfg.MaxExtractedChars,
		AllowRemoteURLs:   attachmentCfg.AllowRemoteURLs,
	}
	if payload.Model == "" {
		payload.Model = envelope.RequestedModel
	}
	encoded, _ := json.Marshal(payload)
	cacheKeyBytes := sha256.Sum256(append([]byte(baseURL+"\x00"), encoded...))
	cacheKey := hex.EncodeToString(cacheKeyBytes[:])
	var result promptAttachmentResponse
	if h.cache != nil && attachmentCfg.CacheTTLSeconds > 0 {
		if raw, ok, err := h.cache.GetRuntime(ctx, promptAttachmentCacheNamespace, cacheKey); err == nil && ok && json.Unmarshal(raw, &result) == nil {
			appendPromptAttachmentText(envelope, result, attachmentCfg.MaxExtractedChars, attachmentCfg.MaxFiles)
			return nil
		}
	}
	breakerKey := hashRiskIdentity("attachment\x00" + baseURL)
	if h.promptExtensionBreakerOpen(ctx, breakerKey) {
		return fmt.Errorf("attachment parser circuit breaker is open")
	}
	release, acquired := acquirePromptExtensionSlot("attachment:"+baseURL, attachmentCfg.MaxConcurrent)
	if !acquired {
		return fmt.Errorf("attachment parser capacity is exhausted")
	}
	defer release()
	requestCtx, cancel := context.WithTimeout(ctx, time.Duration(attachmentCfg.TimeoutSeconds)*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, baseURL+"/v1/attachments/extract", bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if key := strings.TrimSpace(os.Getenv("PROMPT_FILTER_ATTACHMENT_API_KEY")); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.recordPromptExtensionFailure(ctx, breakerKey, attachmentCfg.CircuitBreakerFailures, attachmentCfg.CircuitBreakerSeconds)
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		h.recordPromptExtensionFailure(ctx, breakerKey, attachmentCfg.CircuitBreakerFailures, attachmentCfg.CircuitBreakerSeconds)
		return fmt.Errorf("attachment parser status %d", resp.StatusCode)
	}
	limit := int64(promptAttachmentResponseMaxBytes)
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil || int64(len(body)) > limit {
		h.recordPromptExtensionFailure(ctx, breakerKey, attachmentCfg.CircuitBreakerFailures, attachmentCfg.CircuitBreakerSeconds)
		if err != nil {
			return err
		}
		return fmt.Errorf("attachment parser response exceeded %d bytes", limit)
	}
	if err := json.Unmarshal(body, &result); err != nil {
		h.recordPromptExtensionFailure(ctx, breakerKey, attachmentCfg.CircuitBreakerFailures, attachmentCfg.CircuitBreakerSeconds)
		return err
	}
	h.clearPromptExtensionFailure(ctx, breakerKey)
	appendPromptAttachmentText(envelope, result, attachmentCfg.MaxExtractedChars, attachmentCfg.MaxFiles)
	if h.cache != nil && attachmentCfg.CacheTTLSeconds > 0 {
		_ = h.cache.SetRuntime(ctx, promptAttachmentCacheNamespace, cacheKey, body, time.Duration(attachmentCfg.CacheTTLSeconds)*time.Second)
	}
	return nil
}

func promptAttachmentReferences(envelope promptfilter.RequestEnvelope, maxFiles int, maxBytes int, allowRemoteURLs bool) []string {
	if maxFiles <= 0 || maxBytes <= 0 {
		return nil
	}
	seen := map[string]bool{}
	references := make([]string, 0, maxFiles)
	for _, segment := range envelope.Segments {
		if segment.Origin != promptfilter.OriginAttachmentRefs {
			continue
		}
		value := strings.TrimSpace(segment.Text)
		if value == "" || seen[value] {
			continue
		}
		if len(value) > maxBytes {
			continue
		}
		lower := strings.ToLower(value)
		if !allowRemoteURLs && (strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")) {
			continue
		}
		seen[value] = true
		references = append(references, value)
		if len(references) >= maxFiles {
			break
		}
	}
	sort.Strings(references)
	return references
}

func appendPromptAttachmentText(envelope *promptfilter.RequestEnvelope, result promptAttachmentResponse, maxChars int, maxItems int) {
	if envelope == nil || maxChars <= 0 || maxItems <= 0 {
		return
	}
	nextSequence := 0
	for _, segment := range envelope.Segments {
		if segment.Sequence >= nextSequence {
			nextSequence = segment.Sequence + 1
		}
	}
	remaining := maxChars
	for index, item := range result.Items {
		if index >= maxItems {
			break
		}
		text := strings.TrimSpace(item.Text)
		if text == "" || remaining <= 0 {
			continue
		}
		text = truncatePromptRunes(text, remaining)
		remaining -= utf8.RuneCountInString(text)
		envelope.Segments = append(envelope.Segments, promptfilter.Segment{
			Origin:   promptfilter.OriginAttachmentContent,
			Role:     "attachment",
			Text:     text,
			Sequence: nextSequence,
			Trust:    promptfilter.SegmentTrustClientSupplied,
		})
		nextSequence++
	}
}

func truncatePromptRunes(value string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	if utf8.RuneCountInString(value) <= maxRunes {
		return value
	}
	runes := []rune(value)
	return string(runes[len(runes)-maxRunes:])
}

func (h *Handler) promptExtensionBreakerOpen(ctx context.Context, key string) bool {
	if h == nil || h.cache == nil || key == "" {
		return false
	}
	raw, ok, err := h.cache.GetRuntime(ctx, promptExtensionBreakerNamespace, key)
	if err != nil || !ok {
		return false
	}
	var record promptExtensionBreakerRecord
	return json.Unmarshal(raw, &record) == nil && record.OpenUntil.After(time.Now())
}

func (h *Handler) recordPromptExtensionFailure(ctx context.Context, key string, threshold int, openSeconds int) {
	if h == nil || h.cache == nil || key == "" || threshold <= 0 || openSeconds <= 0 {
		return
	}
	unlock, acquired := acquirePromptRuntimeLease(ctx, h.cache, promptExtensionBreakerNamespace, key)
	if !acquired {
		return
	}
	defer unlock()
	var record promptExtensionBreakerRecord
	if raw, ok, _ := h.cache.GetRuntime(ctx, promptExtensionBreakerNamespace, key); ok {
		_ = json.Unmarshal(raw, &record)
	}
	record.Failures++
	if record.Failures >= threshold {
		record.OpenUntil = time.Now().Add(time.Duration(openSeconds) * time.Second)
	}
	if raw, err := json.Marshal(record); err == nil {
		_ = h.cache.SetRuntime(ctx, promptExtensionBreakerNamespace, key, raw, time.Duration(openSeconds*2)*time.Second)
	}
}

func (h *Handler) clearPromptExtensionFailure(ctx context.Context, key string) {
	if h != nil && h.cache != nil && key != "" {
		_ = h.cache.DeleteRuntime(ctx, promptExtensionBreakerNamespace, key)
	}
}
