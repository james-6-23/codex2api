package proxy

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const windowGrantContextKey = "newapi_window_grant_v1"
const windowGrantDomain = "codex2api-window-grant-v1"
const windowGrantResponseHeader = "X-Codex2API-Window-Grant"

var errWindowAdmissionDenied = errors.New("window admission denied")
var errWindowGrantRefresh = errors.New("窗口预留需要重新确认，尚未调用上游")
var errWindowGrantStorage = errors.New("窗口授权服务暂时不可用，请稍后重试")

type signedWindowGrant struct {
	Version       int                      `json:"version"`
	Platform      string                   `json:"platform"`
	UserID        string                   `json:"user_id"`
	APIKeyID      int64                    `json:"api_key_id"`
	Fingerprint   string                   `json:"root_fingerprint"`
	Grant         database.UserWindowGrant `json:"grant"`
	ReservationID string                   `json:"reservation_id,omitempty"`
}

type windowControlRequest struct {
	Operation      string  `json:"operation"`
	AllowExpansion bool    `json:"allow_expansion"`
	ExtraLimit     int     `json:"extra_limit"`
	Multiplier     float64 `json:"multiplier"`
	GrantID        string  `json:"grant_id,omitempty"`
	ReservationID  string  `json:"reservation_id,omitempty"`
}

type personalWindow struct {
	ID         string    `json:"id"`
	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	Model      string    `json:"model,omitempty"`
	Expanded   bool      `json:"expanded"`
	Multiplier float64   `json:"multiplier"`
}

func windowGrantMAC(secret, payload string) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(windowGrantDomain + "\n" + payload))
	return mac.Sum(nil)
}

func encodeWindowGrant(secret string, grant signedWindowGrant) (string, error) {
	payload, err := json.Marshal(grant)
	if err != nil {
		return "", err
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	return encoded + "." + hex.EncodeToString(windowGrantMAC(secret, encoded)), nil
}

func decodeWindowGrant(secret, token string) (signedWindowGrant, error) {
	var result signedWindowGrant
	if len(token) > 4096 {
		return result, errors.New("window grant is too large")
	}
	payload, signature, found := strings.Cut(token, ".")
	decodedSignature, err := hex.DecodeString(signature)
	if !found || err != nil || !hmac.Equal(decodedSignature, windowGrantMAC(secret, payload)) {
		return result, errors.New("invalid window grant signature")
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil || json.Unmarshal(raw, &result) != nil || result.Version != 1 {
		return result, errors.New("invalid window grant")
	}
	if result.Grant.ID == "" || len(result.ReservationID) > 64 || (result.Grant.NoWindow && result.Grant.Expanded) || result.Grant.Multiplier < 1 || result.Grant.Multiplier > 10 || math.IsNaN(result.Grant.Multiplier) || math.IsInf(result.Grant.Multiplier, 0) || (!result.Grant.Expanded && result.Grant.Multiplier != 1) || (result.Grant.Expanded && result.Grant.ExtraLimit < 1) {
		return signedWindowGrant{}, errors.New("invalid window grant tariff")
	}
	return result, nil
}

func (handler *Handler) userWindowControlLimits(request *gin.Context, identity verifiedNewAPIPolicyContext) (int, int) {
	risk := handler.promptFilterConfigForRequest(request).Advanced.Risk
	limit, seconds := risk.SessionCreationLimit, risk.SessionCreationLimitWindowSeconds
	if !risk.SessionCreationLimitEnabled {
		limit = 0
	}
	if override, found := handler.store.GetPromptSessionLimitOverride(identity.Platform, identity.Identity.UserID); found {
		if override.Mode == database.PromptSessionLimitModeOff {
			return 0, 0
		}
		if override.Mode == database.PromptSessionLimitModeCustom {
			limit, seconds = override.Limit, override.WindowSeconds
		}
	}
	return limit, seconds
}

func (handler *Handler) userWindowControlSnapshot(subject string, now time.Time) map[string]personalWindow {
	handler.ensurePromptSessionLimitsLoaded(subject, now)
	handler.promptSessionLimitMu.Lock()
	defer handler.promptSessionLimitMu.Unlock()
	result := make(map[string]personalWindow)
	for key, expiry := range handler.promptSessionLimits[subject] {
		if !expiry.After(now) {
			continue
		}
		detail := handler.promptSessionWindowDetails[subject][key]
		multiplier := detail.Multiplier
		if multiplier < 1 {
			multiplier = 1
		}
		result[key] = personalWindow{ID: key, CreatedAt: detail.CreatedAt, ExpiresAt: expiry, Model: detail.Model, Expanded: detail.Expanded, Multiplier: multiplier}
	}
	return result
}

func (handler *Handler) ControlNewAPIUserWindows(request *gin.Context) {
	if handler == nil || handler.db == nil || handler.store == nil {
		request.JSON(http.StatusServiceUnavailable, gin.H{"message": "window service unavailable"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(request.Request.Body, 8193))
	if err != nil || len(body) > 8192 {
		request.JSON(http.StatusBadRequest, gin.H{"message": "invalid window request"})
		return
	}
	config := handler.promptFilterConfigForRequest(request)
	identity, verified := handler.verifyNewAPIPolicyContext(request, config.Advanced.NewAPI, body)
	if !verified || !identity.MetaVerified || identity.Identity.UserID == "" {
		request.JSON(http.StatusUnauthorized, gin.H{"message": "verified NewAPI identity required"})
		return
	}
	var input windowControlRequest
	if json.Unmarshal(body, &input) != nil || len(input.ReservationID) > 64 || input.ExtraLimit < 0 || input.ExtraLimit > 100 || input.Multiplier < 1 || input.Multiplier > 10 || math.IsNaN(input.Multiplier) || math.IsInf(input.Multiplier, 0) {
		request.JSON(http.StatusBadRequest, gin.H{"message": "invalid window policy"})
		return
	}
	now := time.Now().UTC()
	subject := cache.PromptSessionLimitSubject(identity.Platform, identity.Identity.UserID)
	limit, seconds := handler.userWindowControlLimits(request, identity)
	windows := handler.userWindowControlSnapshot(subject, now)
	if input.Operation == "list" {
		items := make([]personalWindow, 0, len(windows))
		for _, window := range windows {
			items = append(items, window)
		}
		sort.Slice(items, func(left, right int) bool { return items[left].CreatedAt.After(items[right].CreatedAt) })
		if len(items) > 1000 {
			items = items[:1000]
		}
		var recovery *time.Time
		unavailable := false
		cooldown := config.Advanced.Risk.SessionCreationCooldown
		if cooldown.Mode == "enforce" && cooldown.Validate() == nil && limit > 0 {
			ctx, cancel := context.WithTimeout(request.Request.Context(), time.Second)
			state, readErr := handler.db.ReadSessionCooldown(ctx, subject)
			cancel()
			unavailable = readErr != nil || state.EvaluatedAt.IsZero()
			if !unavailable {
				availableAt, _ := sessionCooldownRecovery(&state, cooldown, cooldown.Interval(state.AverageSeconds, state.Samples), now)
				if availableAt.After(now) {
					recovery = &availableAt
				}
			}
		}
		request.JSON(http.StatusOK, gin.H{"version": 1, "server_now": now, "limit": limit, "window_seconds": seconds, "used": len(windows), "windows": items, "truncated": len(windows) > len(items), "creation_available_at": recovery, "cooldown_unavailable": unavailable})
		return
	}
	if input.Operation != "quote" && input.Operation != "release" {
		request.JSON(http.StatusBadRequest, gin.H{"message": "unsupported window operation"})
		return
	}
	if identity.Meta.RootSessionState != newAPIPolicyRootSessionResolved || identity.Meta.RootSessionFingerprint == "" {
		request.JSON(http.StatusOK, gin.H{"version": 1, "multiplier": 1, "ticket": "", "reason": "root_unavailable"})
		return
	}
	root := hashRiskIdentity(identity.Meta.RootSessionFingerprint)
	ctx, cancel := context.WithTimeout(request.Request.Context(), 2*time.Second)
	defer cancel()
	var granted *database.UserWindowGrant
	var deniedRecovery time.Time
	quoteReason := ""
	background := identity.Meta.RootSessionRelation == newAPIPolicyRootSessionRelationRelated || identity.Meta.SessionAccounting == newAPISessionAccountingBypass || identity.Meta.RequestKind == "compaction" || (identity.Meta.ThreadSource != "" && identity.Meta.ThreadSource != "user")
	err = handler.db.UpdateUserWindowAdmissions(ctx, subject, func(state *database.UserWindowAdmissionState) error {
		if state.Reservations == nil {
			state.Reservations = make(map[string]map[string]time.Time)
		}
		for key, grant := range state.Windows {
			if grant == nil || !grant.ExpiresAt.After(now) || (!grant.Confirmed && !grant.PendingUntil.After(now)) {
				delete(state.Windows, key)
				delete(state.Reservations, key)
			}
		}
		if input.Operation == "release" {
			if grant := state.Windows[root]; grant != nil && grant.ID == input.GrantID && !grant.Confirmed && input.ReservationID != "" {
				leases := state.Reservations[root]
				if _, owned := leases[input.ReservationID]; owned {
					delete(leases, input.ReservationID)
					for owner, expiry := range leases {
						if !expiry.After(now) {
							delete(leases, owner)
						}
					}
					if len(leases) == 0 {
						delete(state.Windows, root)
						delete(state.Reservations, root)
					}
				}
			}
			return nil
		}
		defer func() {
			if granted == nil || granted.Confirmed || background || input.ReservationID == "" {
				return
			}
			leases := state.Reservations[root]
			if leases == nil {
				leases = make(map[string]time.Time)
				state.Reservations[root] = leases
			}
			for owner, expiry := range leases {
				if !expiry.After(now) {
					delete(leases, owner)
				}
			}
			if len(leases) < 128 {
				leases[input.ReservationID] = granted.PendingUntil
			}
		}()
		if grant := state.Windows[root]; grant != nil {
			copy := *grant
			granted = &copy
			return nil
		}
		if window, found := windows[root]; found {
			granted = &database.UserWindowGrant{ID: uuid.NewString(), Root: root, CreatedAt: window.CreatedAt, ExpiresAt: window.ExpiresAt, Confirmed: true, Expanded: window.Expanded, Multiplier: window.Multiplier, ExtraLimit: input.ExtraLimit}
			state.Windows[root] = granted
			return nil
		}
		if background {
			return nil
		}
		if limit <= 0 || seconds <= 0 || limit > 1000 {
			quoteReason = "ordinary_only"
			return nil
		}
		if len(state.Windows) >= 1100 {
			return errWindowAdmissionDenied
		}
		ordinary, expanded := 0, 0
		counted := make(map[string]bool, len(windows)+len(state.Windows))
		for key, window := range windows {
			counted[key] = true
			if window.Expanded {
				expanded++
			} else {
				ordinary++
			}
		}
		for key, window := range state.Windows {
			if counted[key] || window.NoWindow {
				continue
			}
			if window.Expanded {
				expanded++
			} else {
				ordinary++
			}
		}
		useExpansion := ordinary >= limit
		if useExpansion && (!input.AllowExpansion || input.ExtraLimit <= expanded || input.Multiplier <= 1) {
			for _, window := range windows {
				if deniedRecovery.IsZero() || window.ExpiresAt.Before(deniedRecovery) {
					deniedRecovery = window.ExpiresAt
				}
			}
			for _, window := range state.Windows {
				recovery := window.ExpiresAt
				if !window.Confirmed {
					recovery = window.PendingUntil
				}
				if deniedRecovery.IsZero() || recovery.Before(deniedRecovery) {
					deniedRecovery = recovery
				}
			}
			return errWindowAdmissionDenied
		}
		multiplier := 1.0
		if useExpansion {
			multiplier = input.Multiplier
		}
		granted = &database.UserWindowGrant{ID: uuid.NewString(), Root: root, CreatedAt: now, ExpiresAt: now.Add(time.Duration(seconds) * time.Second), PendingUntil: now.Add(30 * time.Second), Expanded: useExpansion, Multiplier: multiplier, ExtraLimit: input.ExtraLimit}
		state.Windows[root] = granted
		return nil
	})
	if err != nil {
		statusCode := http.StatusServiceUnavailable
		message := "窗口服务暂时不可用，请稍后重试"
		if errors.Is(err, errWindowAdmissionDenied) {
			statusCode = http.StatusBadRequest
			message = promptSessionCreationLimitMessage(promptSessionCreationLimitStatus{NextRecoveryAt: deniedRecovery, RetryAfter: max(1, int(time.Until(deniedRecovery).Seconds()))})
		}
		request.JSON(statusCode, gin.H{"message": message, "code": "window_admission_failed"})
		return
	}
	if granted == nil {
		request.JSON(http.StatusOK, gin.H{"version": 1, "ticket": "", "multiplier": 1, "reason": quoteReason})
		return
	}
	signed := signedWindowGrant{Version: 1, Platform: identity.Platform, UserID: identity.Identity.UserID, APIKeyID: identity.APIKeyID, Fingerprint: identity.Meta.RootSessionFingerprint, Grant: *granted, ReservationID: input.ReservationID}
	ticket, err := encodeWindowGrant(identity.VerificationSecret, signed)
	if err != nil {
		request.JSON(http.StatusInternalServerError, gin.H{"message": "could not issue window grant"})
		return
	}
	request.JSON(http.StatusOK, gin.H{"version": 1, "ticket": ticket, "grant": granted, "multiplier": granted.Multiplier})
}

func windowGrantForRequest(request *gin.Context) *signedWindowGrant {
	if request == nil {
		return nil
	}
	value, _ := request.Get(windowGrantContextKey)
	grant, _ := value.(*signedWindowGrant)
	return grant
}

func (handler *Handler) validateRequestWindowGrant(request *gin.Context) error {
	request.Set(windowGrantContextKey, nil)
	handler.verifyNewAPIPolicyContext(request, handler.promptFilterConfigForRequest(request).Advanced.NewAPI, ingressRequestBody(request, nil))
	status, identity := handler.cachedNewAPIPolicyAuditState(request)
	if (status != "verified" && status != "signed_response") || !identity.MetaVerified {
		return nil
	}
	subject := cache.PromptSessionLimitSubject(identity.Platform, identity.Identity.UserID)
	root := hashRiskIdentity(identity.Meta.RootSessionFingerprint)
	now := time.Now()
	handler.ensurePromptSessionLimitsLoaded(subject, now)
	handler.promptSessionLimitMu.Lock()
	detail := handler.promptSessionWindowDetails[subject][root]
	existing := handler.promptSessionLimits[subject][root].After(now)
	handler.promptSessionLimitMu.Unlock()
	if identity.Meta.WindowGrant == "" {
		if existing && detail.Expanded {
			return errWindowGrantRefresh
		}
		return nil
	}
	grant, err := decodeWindowGrant(identity.VerificationSecret, identity.Meta.WindowGrant)
	if err != nil {
		return err
	}
	wasConfirmed := grant.Grant.Confirmed
	if grant.Platform != identity.Platform || grant.UserID != identity.Identity.UserID || grant.APIKeyID != identity.APIKeyID || grant.Fingerprint != identity.Meta.RootSessionFingerprint || grant.Grant.Root != root {
		return errors.New("window grant scope or expiry mismatch")
	}
	if !grant.Grant.ExpiresAt.After(now) {
		return errWindowGrantRefresh
	}
	if !grant.Grant.Confirmed && !grant.Grant.PendingUntil.After(now) && (!existing || detail.GrantID != grant.Grant.ID) {
		ctx, cancel := context.WithTimeout(request.Request.Context(), time.Second)
		defer cancel()
		state, readErr := handler.db.ReadUserWindowAdmissions(ctx, subject)
		if readErr != nil {
			return errWindowGrantStorage
		}
		if state.Windows[root] == nil || state.Windows[root].ID != grant.Grant.ID || !state.Windows[root].Confirmed || !state.Windows[root].ExpiresAt.After(now) {
			return errWindowGrantRefresh
		}
		grant.Grant = *state.Windows[root]
	}
	request.Set(windowGrantContextKey, &grant)
	usageRequestDiagnosticState(request).WindowGrant = "pending"
	if grant.Grant.Confirmed {
		usageRequestDiagnosticState(request).WindowGrant = "confirmed"
		if !wasConfirmed {
			handler.publishRequestWindowGrant(request, &grant)
		}
	}
	return nil
}

func (handler *Handler) requestWindowGrantError(request *gin.Context) *api.APIError {
	if err := handler.validateRequestWindowGrant(request); err != nil {
		return requestWindowGrantAPIError(err)
	}
	if trace := selectionTraceForRequest(request); trace != nil {
		grant := windowGrantForRequest(request)
		trace.SetExpandedWindow(grant != nil && grant.Grant.Expanded)
	}
	return nil
}

func (handler *Handler) confirmRequestWindowGrant(request *gin.Context) error {
	grant := windowGrantForRequest(request)
	if grant == nil {
		return nil
	}
	if grant.Grant.Confirmed {
		return nil
	}
	subject := cache.PromptSessionLimitSubject(grant.Platform, grant.UserID)
	ctx, cancel := context.WithTimeout(request.Request.Context(), time.Second)
	defer cancel()
	var confirmed database.UserWindowGrant
	var windows map[string]personalWindow
	limit := 0
	if request.GetBool("window_grant_convert_no_window") {
		_, identity := handler.cachedNewAPIPolicyAuditState(request)
		limit, _ = handler.userWindowControlLimits(request, identity)
		windows = handler.userWindowControlSnapshot(subject, time.Now())
	}
	refreshRequired := false
	err := handler.db.UpdateUserWindowAdmissions(ctx, subject, func(state *database.UserWindowAdmissionState) error {
		stored := state.Windows[grant.Grant.Root]
		if stored == nil || stored.ID != grant.Grant.ID || !stored.ExpiresAt.After(time.Now()) || (!stored.Confirmed && !stored.PendingUntil.After(time.Now())) {
			return errWindowGrantRefresh
		}
		if stored.NoWindow && !grant.Grant.NoWindow && limit > 0 {
			ordinary := 0
			counted := make(map[string]bool, len(windows))
			for root, window := range windows {
				counted[root] = true
				if !window.Expanded {
					ordinary++
				}
			}
			for root, window := range state.Windows {
				if window != nil && !counted[root] && !window.NoWindow && !window.Expanded && window.ExpiresAt.After(time.Now()) && (window.Confirmed || window.PendingUntil.After(time.Now())) {
					ordinary++
				}
			}
			if ordinary >= limit {
				delete(state.Windows, grant.Grant.Root)
				refreshRequired = true
				return nil
			}
		}
		stored.Confirmed = true
		stored.NoWindow = grant.Grant.NoWindow
		confirmed = *stored
		delete(state.Reservations, grant.Grant.Root)
		return nil
	})
	if err != nil {
		if errors.Is(err, errWindowGrantRefresh) {
			return err
		}
		return errWindowGrantStorage
	}
	if refreshRequired {
		return errWindowGrantRefresh
	}
	grant.Grant = confirmed
	handler.publishRequestWindowGrant(request, grant)
	return nil
}

func requestWindowGrantAPIError(err error) *api.APIError {
	if errors.Is(err, errWindowGrantRefresh) {
		return api.NewAPIError(api.ErrorCode("window_billing_refresh_required"), err.Error(), api.ErrorTypeInvalidRequest)
	}
	if errors.Is(err, errWindowGrantStorage) {
		return api.NewAPIError(api.ErrCodeServiceUnavailable, err.Error(), api.ErrorTypeServer)
	}
	return api.NewAPIError(api.ErrorCode("window_expansion_invalid"), err.Error(), api.ErrorTypeInvalidRequest)
}

func (handler *Handler) publishRequestWindowGrant(request *gin.Context, grant *signedWindowGrant) {
	_, identity := handler.cachedNewAPIPolicyAuditState(request)
	ticket, err := encodeWindowGrant(identity.VerificationSecret, *grant)
	if err == nil {
		request.Header(windowGrantResponseHeader, ticket)
		usageRequestDiagnosticState(request).WindowGrant = "confirmed"
	}
}

func (handler *Handler) waitForBackgroundWindowGrant(ctx context.Context, request *gin.Context) *api.APIError {
	if apiErr := handler.requestWindowGrantError(request); apiErr != nil {
		return apiErr
	}
	grant := windowGrantForRequest(request)
	if grant == nil || grant.Grant.Confirmed {
		return nil
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		lookup, cancel := context.WithTimeout(ctx, time.Second)
		state, err := handler.db.ReadUserWindowAdmissions(lookup, cache.PromptSessionLimitSubject(grant.Platform, grant.UserID))
		cancel()
		if err != nil {
			return requestWindowGrantAPIError(errWindowGrantStorage)
		}
		stored := state.Windows[grant.Grant.Root]
		if stored == nil || stored.ID != grant.Grant.ID || !stored.ExpiresAt.After(time.Now()) {
			return requestWindowGrantAPIError(errWindowGrantRefresh)
		}
		if stored.Confirmed {
			grant.Grant = *stored
			handler.publishRequestWindowGrant(request, grant)
			return nil
		}
		select {
		case <-ctx.Done():
			return api.NewAPIError(api.ErrCodeRootAccountWaitTimeout, "主窗口授权未在 60 秒等待预算内确认，后台请求已停止。", api.ErrorTypeInvalidRequest)
		case <-ticker.C:
		}
	}
}
