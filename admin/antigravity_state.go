package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

const (
	antigravityCapabilityProbeTimeout = 30 * time.Second
	antigravityProbeResponseLimit     = 256 << 10
)

var (
	errAntigravityAccountRequired   = errors.New("only Antigravity accounts support this operation")
	errAntigravityCredentialChanged = errors.New("Antigravity credential changed during the operation; retry")
)

type antigravityCapabilityExecutor func(context.Context, *auth.Account, string, []byte, bool, string) (*http.Response, error)

type antigravityCatalogState struct {
	Models       []string `json:"models"`
	Source       string   `json:"source"`
	Verified     bool     `json:"verified"`
	Synchronized bool     `json:"synchronized"`
	ObservedAt   string   `json:"observed_at,omitempty"`
}

type antigravityIdentityState struct {
	Status        string `json:"status"`
	EmailVerified bool   `json:"email_verified"`
	SubjectKnown  bool   `json:"subject_known"`
	ProjectStatus string `json:"project_status"`
	ProjectID     string `json:"project_id,omitempty"`
}

type antigravityCapabilityObservation struct {
	CredentialGeneration int64     `json:"credential_generation"`
	Protocol             string    `json:"protocol"`
	ModelID              string    `json:"model_id"`
	Status               string    `json:"status"`
	Verified             bool      `json:"verified"`
	HTTPStatus           int       `json:"http_status,omitempty"`
	Source               string    `json:"source"`
	ObservedAt           time.Time `json:"observed_at"`
	ContentType          string    `json:"content_type,omitempty"`
}

type antigravityAccountState struct {
	AccountID            int64                              `json:"account_id"`
	CredentialGeneration int64                              `json:"credential_generation"`
	CredentialKind       string                             `json:"credential_kind"`
	Catalog              antigravityCatalogState            `json:"catalog"`
	Identity             antigravityIdentityState           `json:"identity"`
	Permissions          *auth.AntigravityEntitlements      `json:"permissions,omitempty"`
	Quota                *auth.AntigravityQuotaSnapshot     `json:"quota,omitempty"`
	Capabilities         []antigravityCapabilityObservation `json:"capabilities"`
	LastSyncedAt         string                             `json:"last_synced_at,omitempty"`
	LastSyncAttemptAt    string                             `json:"last_sync_attempt_at,omitempty"`
	LastCapabilityProbe  string                             `json:"last_capability_probe_at,omitempty"`
	Warnings             []string                           `json:"warnings"`
}

type antigravityStateSyncResponse struct {
	Message       string                   `json:"message"`
	State         *antigravityAccountState `json:"state"`
	Remote        bool                     `json:"remote"`
	CatalogSource string                   `json:"catalog_source"`
	Verified      bool                     `json:"verified"`
}

func parseAntigravityAdminAccountID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusBadRequest, "invalid account ID")
		return 0, false
	}
	return id, true
}

func writeAntigravityAdminError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeError(c, http.StatusNotFound, "account not found")
	case errors.Is(err, errAntigravityAccountRequired):
		writeError(c, http.StatusBadRequest, err.Error())
	case errors.Is(err, errAntigravityCredentialChanged):
		writeError(c, http.StatusConflict, err.Error())
	default:
		writeInternalError(c, err)
	}
}

func (h *Handler) antigravityAdminRow(ctx context.Context, id int64) (*database.AccountRow, error) {
	if h == nil || h.db == nil {
		return nil, errors.New("Antigravity admin service is not initialized")
	}
	row, err := h.db.GetAccountByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if !isAntigravityAccountRow(row) {
		return nil, errAntigravityAccountRequired
	}
	return row, nil
}

func antigravityAuthKindFromRow(row *database.AccountRow) string {
	if row != nil && strings.TrimSpace(row.GetCredential("api_key")) != "" {
		return auth.AntigravityAuthKindAPIKey
	}
	return auth.AntigravityAuthKindOAuth
}

func antigravityStateFromRow(row *database.AccountRow) *antigravityAccountState {
	kind := antigravityAuthKindFromRow(row)
	rawModels := row.GetCredentialStringSlice("models")
	state := &antigravityAccountState{
		AccountID: row.ID, CredentialGeneration: row.CredentialGeneration, CredentialKind: kind,
		Capabilities: []antigravityCapabilityObservation{}, Warnings: []string{},
		LastSyncedAt:        row.GetCredential("antigravity_last_synced_at"),
		LastSyncAttemptAt:   row.GetCredential("antigravity_last_sync_attempt_at"),
		LastCapabilityProbe: row.GetCredential("antigravity_capability_last_probe_at"),
	}
	if kind == auth.AntigravityAuthKindAPIKey {
		state.Identity = antigravityIdentityState{Status: "not_applicable", ProjectStatus: "not_applicable"}
		state.Catalog.Source = strings.TrimSpace(row.GetCredential("antigravity_catalog_source"))
		if state.Catalog.Source == "" {
			state.Catalog.Source = "declared"
		}
		if len(rawModels) == 0 {
			rawModels = auth.AntigravityDefaultModelIDs()
			state.Catalog.Source = "default"
		}
		state.Catalog.Verified = row.GetCredentialBool("antigravity_catalog_verified")
		state.Catalog.Synchronized = strings.TrimSpace(row.GetCredential("antigravity_catalog_source")) != ""
		state.Catalog.ObservedAt = state.LastSyncAttemptAt
		state.Warnings = append(state.Warnings, "API-key catalog is local and unverified; run an explicit capability probe before claiming Interactions compatibility")
	} else {
		state.Identity = antigravityIdentityState{
			Status: "pending", EmailVerified: row.GetCredentialBool("verified_email"),
			SubjectKnown:  strings.TrimSpace(row.GetCredential("account_id")) != "",
			ProjectStatus: "unavailable", ProjectID: row.GetCredential("project_id"),
		}
		if state.Identity.EmailVerified && state.Identity.SubjectKnown {
			state.Identity.Status = "verified"
		}
		if state.Identity.ProjectID != "" {
			state.Identity.ProjectStatus = "available"
		}
		state.Catalog.Source = "google_control_plane"
		state.Catalog.Verified = state.LastSyncedAt != "" && len(rawModels) > 0
		state.Catalog.Synchronized = state.LastSyncedAt != ""
		state.Catalog.ObservedAt = state.LastSyncedAt
	}
	state.Catalog.Models = antigravityPublishedModels(rawModels)
	if raw := strings.TrimSpace(row.GetCredential("antigravity_permissions")); raw == "" {
		raw = strings.TrimSpace(row.GetCredential("antigravity_entitlements"))
		if raw != "" {
			var permissions auth.AntigravityEntitlements
			if json.Unmarshal([]byte(raw), &permissions) == nil {
				state.Permissions = &permissions
			}
		}
	} else {
		var permissions auth.AntigravityEntitlements
		if json.Unmarshal([]byte(raw), &permissions) == nil {
			state.Permissions = &permissions
		}
	}
	if raw := strings.TrimSpace(row.GetCredential("antigravity_quota")); raw != "" {
		var quota auth.AntigravityQuotaSnapshot
		if json.Unmarshal([]byte(raw), &quota) == nil {
			projected := antigravityPublishedQuota(quota)
			state.Quota = &projected
		}
	}
	if raw := strings.TrimSpace(row.GetCredential("antigravity_capabilities")); raw != "" {
		var observations []antigravityCapabilityObservation
		if json.Unmarshal([]byte(raw), &observations) == nil {
			for _, observation := range observations {
				if observation.CredentialGeneration == row.CredentialGeneration {
					publishedModel, ok := antigravityPublishedModelForObservation(observation.ModelID, state.Catalog.Models)
					if !ok {
						continue
					}
					observation.ModelID = publishedModel
					state.Capabilities = append(state.Capabilities, observation)
				}
			}
		}
	}
	for _, warning := range []string{row.GetCredential("antigravity_sync_error"), row.GetCredential("antigravity_sync_warning")} {
		if warning = strings.TrimSpace(warning); warning != "" {
			state.Warnings = append(state.Warnings, warning)
		}
	}
	return state
}

// GetAntigravityAccountState returns sanitized persisted observations only. It
// never performs an upstream request and never serializes credential secrets.
func (h *Handler) GetAntigravityAccountState(c *gin.Context) {
	id, ok := parseAntigravityAdminAccountID(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	row, err := h.antigravityAdminRow(ctx, id)
	if err != nil {
		writeAntigravityAdminError(c, err)
		return
	}
	c.JSON(http.StatusOK, antigravityStateFromRow(row))
}

func (h *Handler) runAntigravityRefresh(ctx context.Context, id int64) antigravityRefreshItem {
	if h.antigravitySyncAccount != nil {
		return h.antigravitySyncAccount(ctx, id)
	}
	return h.refreshAntigravityAccount(ctx, id)
}

// SyncAntigravityAccountState refreshes OAuth control-plane facts. API-key
// accounts are deliberately local-only: no remote catalog claim is fabricated.
func (h *Handler) SyncAntigravityAccountState(c *gin.Context) {
	id, ok := parseAntigravityAdminAccountID(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 100*time.Second)
	defer cancel()
	row, err := h.antigravityAdminRow(ctx, id)
	if err != nil {
		writeAntigravityAdminError(c, err)
		return
	}
	if antigravityAuthKindFromRow(row) == auth.AntigravityAuthKindAPIKey {
		models := row.GetCredentialStringSlice("models")
		source := "declared"
		if len(models) == 0 {
			models = auth.AntigravityDefaultModelIDs()
			source = "default"
		}
		now := time.Now().UTC().Format(time.RFC3339)
		applied, mergeErr := h.db.MergeAccountCredentialsForGeneration(ctx, id, row.CredentialGeneration, map[string]any{
			"models": models, "antigravity_catalog_source": source,
			"antigravity_catalog_verified": false, "antigravity_last_sync_attempt_at": now,
			"antigravity_sync_warning": "API-key catalog is local and was not remotely verified",
		})
		if mergeErr != nil {
			writeAntigravityAdminError(c, mergeErr)
			return
		}
		if !applied {
			writeAntigravityAdminError(c, errAntigravityCredentialChanged)
			return
		}
		row, err = h.antigravityAdminRow(ctx, id)
		if err != nil {
			writeAntigravityAdminError(c, err)
			return
		}
		c.JSON(http.StatusOK, antigravityStateSyncResponse{Message: "local API-key catalog synchronized", State: antigravityStateFromRow(row), CatalogSource: source})
		return
	}
	item := h.runAntigravityRefresh(ctx, id)
	if !item.OK {
		writeError(c, http.StatusBadGateway, item.Error)
		return
	}
	row, err = h.antigravityAdminRow(ctx, id)
	if err != nil {
		writeAntigravityAdminError(c, err)
		return
	}
	state := antigravityStateFromRow(row)
	c.JSON(http.StatusOK, antigravityStateSyncResponse{Message: "Google control plane synchronized", State: state, Remote: true, CatalogSource: "google_control_plane", Verified: state.Catalog.Verified})
}

func antigravityProbeStatus(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusTooManyRequests:
		return "rate_limited"
	}
	if status >= 200 && status < 300 {
		return "ok"
	}
	if status >= 500 || status == 0 {
		return "unavailable"
	}
	return "error"
}

func antigravityProbeEnvelopeVerified(envelope map[string]any) bool {
	if len(envelope) == 0 || envelope["error"] != nil {
		return false
	}
	if status, ok := envelope["status"].(string); ok {
		switch strings.ToLower(strings.TrimSpace(status)) {
		case "failed", "error", "cancelled", "canceled", "incomplete":
			return false
		}
	}
	return true
}

func (h *Handler) antigravityProbeExecutor() antigravityCapabilityExecutor {
	if h.antigravityCapabilityProbe != nil {
		return h.antigravityCapabilityProbe
	}
	return proxy.ExecuteAntigravityResponsesRequest
}

func (h *Handler) antigravityProbeRuntimeAccount(ctx context.Context, row *database.AccountRow) (*auth.Account, error) {
	if h.store == nil {
		return nil, errors.New("Antigravity runtime store is unavailable")
	}
	account, err := h.store.BuildTransientAccountByID(ctx, row.ID)
	if err != nil {
		return nil, err
	}
	if account == nil || !account.IsAntigravityAPI() {
		return nil, errors.New("Antigravity account has no usable credential")
	}
	return account, nil
}

// ProbeAntigravityAccountCapabilities performs exactly one bounded, non-stream
// generation request against one model. It is never called by state reads or
// sync and is the only path that can mark API-key Interactions as verified.
func (h *Handler) ProbeAntigravityAccountCapabilities(c *gin.Context) {
	id, ok := parseAntigravityAdminAccountID(c)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), antigravityCapabilityProbeTimeout)
	defer cancel()
	row, err := h.antigravityAdminRow(ctx, id)
	if err != nil {
		writeAntigravityAdminError(c, err)
		return
	}
	account, err := h.antigravityProbeRuntimeAccount(ctx, row)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	rawModels := row.GetCredentialStringSlice("models")
	if len(rawModels) == 0 {
		rawModels = auth.AntigravityDefaultModelIDs()
	}
	models := antigravityPublishedModels(rawModels)
	if len(models) == 0 {
		writeError(c, http.StatusBadRequest, "Antigravity account has no probe model")
		return
	}
	protocol := "cloud_code_v1internal"
	if account.AntigravityAuthKind() == auth.AntigravityAuthKindAPIKey {
		protocol = "interactions"
	}
	observed := antigravityCapabilityObservation{
		CredentialGeneration: row.CredentialGeneration, Protocol: protocol, ModelID: models[0], Status: "unavailable", Source: "explicit_probe", ObservedAt: time.Now().UTC(),
	}
	response, probeErr := h.antigravityProbeExecutor()(ctx, account, models[0], []byte(`{"input":"Reply with OK.","max_output_tokens":1}`), false, row.ProxyURL)
	if probeErr == nil && response != nil {
		observed.HTTPStatus = response.StatusCode
		observed.ContentType = strings.TrimSpace(strings.Split(response.Header.Get("Content-Type"), ";")[0])
		body, readErr := io.ReadAll(io.LimitReader(response.Body, antigravityProbeResponseLimit+1))
		_ = response.Body.Close()
		observed.Status = antigravityProbeStatus(response.StatusCode)
		if readErr != nil || len(body) > antigravityProbeResponseLimit {
			observed.Status = "invalid_response"
		} else if response.StatusCode >= 200 && response.StatusCode < 300 {
			var envelope map[string]any
			if !strings.Contains(strings.ToLower(observed.ContentType), "json") || json.Unmarshal(body, &envelope) != nil || !antigravityProbeEnvelopeVerified(envelope) {
				observed.Status = "invalid_response"
			} else {
				observed.Verified = true
			}
		}
	} else if probeErr == nil {
		probeErr = errors.New("empty upstream response")
	}
	encoded, marshalErr := json.Marshal([]antigravityCapabilityObservation{observed})
	if marshalErr != nil {
		writeInternalError(c, marshalErr)
		return
	}
	applied, persistErr := h.db.MergeAccountCredentialsForGeneration(ctx, id, row.CredentialGeneration, map[string]any{
		"antigravity_capabilities": string(encoded), "antigravity_capability_last_probe_at": observed.ObservedAt.Format(time.RFC3339),
	})
	if persistErr != nil {
		writeAntigravityAdminError(c, persistErr)
		return
	}
	if !applied {
		writeAntigravityAdminError(c, errAntigravityCredentialChanged)
		return
	}
	row, err = h.antigravityAdminRow(ctx, id)
	if err != nil {
		writeAntigravityAdminError(c, err)
		return
	}
	result := gin.H{"message": "Antigravity capability probe completed", "state": antigravityStateFromRow(row), "result": observed}
	if probeErr != nil {
		result["warning"] = fmt.Sprintf("probe request failed: %s", boundedAntigravityProbeError(probeErr))
	}
	c.JSON(http.StatusOK, result)
}

func boundedAntigravityProbeError(err error) string {
	if err == nil {
		return ""
	}
	message := strings.Join(strings.Fields(err.Error()), " ")
	if len(message) > 256 {
		message = message[:256]
	}
	return message
}
