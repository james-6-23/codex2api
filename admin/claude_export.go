package admin

// Claude OAuth credential export/import primitives.
//
// Claude credentials are intentionally kept out of the generic Codex export
// endpoint.  This file defines a provider-specific, versioned document that
// can be moved between Codex2API installations without exposing arbitrary
// request headers or instance-local group IDs.

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

const (
	claudeCredentialExportVersion    = 1
	claudeCredentialExportMaxBytes   = 8 << 20
	claudeCredentialImportMaxEntries = 500
)

func claudeImportTimeout(entries int) time.Duration {
	if entries < 1 {
		entries = 1
	}
	// Account creation can perform a bounded upstream model discovery when an
	// old token document has no models. Scale the request budget without making
	// a large bundle unbounded.
	timeout := 20*time.Second + time.Duration(entries)*2*time.Second
	if timeout > 10*time.Minute {
		return 10 * time.Minute
	}
	return timeout
}

// claudeGroupRef is portable across installations.  Numeric group IDs are
// deliberately not exported because IDs are instance-local and could bind an
// imported account to an unrelated production group.
type claudeGroupRef struct {
	Name    string `json:"name"`
	Channel string `json:"channel"`
}

// claudeExportEntry is the stable, secret-bearing Claude credential document.
// Operational counters/cooldowns/locks are intentionally omitted; they are
// local runtime state and must be re-established by the destination instance.
type claudeExportEntry struct {
	Type                  string            `json:"type"`
	Version               int               `json:"version"`
	AuthKind              string            `json:"auth_kind"`
	Email                 string            `json:"email,omitempty"`
	Name                  string            `json:"name,omitempty"`
	AccessToken           string            `json:"access_token"`
	RefreshToken          string            `json:"refresh_token"`
	AccountID             string            `json:"account_id,omitempty"`
	ExpiresAt             string            `json:"expires_at,omitempty"`
	PlanType              string            `json:"plan_type,omitempty"`
	Models                []string          `json:"models,omitempty"`
	ProxyURL              string            `json:"proxy_url,omitempty"`
	Timezone              string            `json:"timezone,omitempty"`
	ClaudeFingerprintMode string            `json:"claude_fingerprint_mode,omitempty"`
	FingerprintHeaders    map[string]string `json:"fingerprint_headers,omitempty"`
	Tags                  []string          `json:"tags,omitempty"`
	GroupRefs             []claudeGroupRef  `json:"group_refs,omitempty"`
	Enabled               bool              `json:"enabled"`

	// exportFileName is only used as a ZIP member name and never serialized.
	exportFileName string `json:"-"`
}

// claudeImportDocument is the validated internal representation accepted by
// the Claude import endpoint.  Enabled is a pointer so legacy documents that
// omit it keep the historical default (enabled=true).
type claudeImportDocument struct {
	Type                  string
	Version               int
	AuthKind              string
	Email                 string
	Name                  string
	AccessToken           string
	RefreshToken          string
	AccountID             string
	ExpiresAt             string
	PlanType              string
	Models                []string
	ProxyURL              string
	UseProxyPool          bool
	Timezone              string
	ClaudeFingerprintMode string
	FingerprintHeaders    map[string]string
	Tags                  []string
	GroupRefs             []claudeGroupRef
	Enabled               *bool
}

// claudeAccountImportOptions carries metadata that is not part of
// auth.ClaudeTokenData.  It is consumed by the common account creation path.
type claudeAccountImportOptions struct {
	Models             []string
	PlanType           string
	FingerprintMode    string
	FingerprintHeaders map[string]string
	Tags               []string
	GroupRefs          []claudeGroupRef
	ResolvedGroupIDs   []int64
	SkipModelFetch     bool
	Enabled            *bool
}

type claudeImportResultItem struct {
	ID       int64    `json:"id,omitempty"`
	Email    string   `json:"email,omitempty"`
	OK       bool     `json:"ok"`
	Error    string   `json:"error,omitempty"`
	Warnings []string `json:"warnings,omitempty"`
	status   int      `json:"-"`
}

type claudeAccountCreateError struct {
	Status  int
	Message string
}

type claudeAccountCreateResult struct {
	ID       int64
	Email    string
	Warnings []string
}

func (e *claudeAccountCreateError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

func marshalClaudeExportEntry(entry claudeExportEntry) ([]byte, error) {
	return json.MarshalIndent(entry, "", "  ")
}

var claudeExportUnsafeFileChars = regexp.MustCompile(`[^A-Za-z0-9@._-]`)

func claudeExportFileName(email, name string, id int64) string {
	for _, candidate := range []string{email, name} {
		safe := claudeExportUnsafeFileChars.ReplaceAllString(strings.TrimSpace(candidate), "")
		safe = strings.TrimLeft(safe, ".")
		if safe != "" {
			return safe + ".json"
		}
	}
	return fmt.Sprintf("account-%d.json", id)
}

func buildClaudeExportZIP(entries []claudeExportEntry) ([]byte, error) {
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	used := make(map[string]int, len(entries))
	for index, entry := range entries {
		baseName := entry.exportFileName
		if baseName == "" {
			baseName = claudeExportFileName(entry.Email, entry.Name, int64(index+1))
		}
		name := baseName
		if seen := used[baseName]; seen > 0 {
			ext := path.Ext(name)
			name = fmt.Sprintf("%s-%d%s", strings.TrimSuffix(name, ext), seen+1, ext)
		}
		used[baseName]++
		member, err := writer.Create(name)
		if err != nil {
			_ = writer.Close()
			return nil, err
		}
		encoded, err := marshalClaudeExportEntry(entry)
		if err != nil {
			_ = writer.Close()
			return nil, err
		}
		if _, err := member.Write(encoded); err != nil {
			_ = writer.Close()
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// normalizeClaudeFingerprintHeaders accepts Claude Code identity headers and
// the account's device-ID metadata stored alongside them. The device ID is
// deliberately not a ClaudeIdentityHeaderNames entry and is never sent as an
// HTTP header. Authorization, Cookie, API keys, and arbitrary custom headers
// must never cross an export boundary.
func normalizeClaudeFingerprintHeaders(headers map[string]string) (map[string]string, error) {
	if len(headers) == 0 {
		return nil, nil
	}
	allowed := make(map[string]struct{}, len(auth.ClaudeIdentityHeaderNames))
	for _, name := range auth.ClaudeIdentityHeaderNames {
		allowed[strings.ToLower(strings.TrimSpace(name))] = struct{}{}
	}
	allowed[auth.ClaudeDeviceIDCredentialKey] = struct{}{}
	out := make(map[string]string, len(headers))
	for rawName, rawValue := range headers {
		name := strings.TrimSpace(rawName)
		lower := strings.ToLower(name)
		if _, ok := allowed[lower]; !ok {
			return nil, fmt.Errorf("fingerprint_headers contains unsupported header %q", name)
		}
		value := strings.TrimSpace(rawValue)
		if value == "" {
			continue
		}
		if strings.ContainsAny(value, "\r\n") {
			return nil, fmt.Errorf("fingerprint_headers.%s cannot contain newlines", name)
		}
		if len(value) > 8192 {
			return nil, fmt.Errorf("fingerprint_headers.%s exceeds 8192 bytes", name)
		}
		canonical := http.CanonicalHeaderKey(name)
		if previous, exists := out[canonical]; exists && previous != value {
			return nil, fmt.Errorf("fingerprint_headers contains conflicting duplicate header %q", canonical)
		}
		out[canonical] = value
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// prepareClaudeTimezoneCredentialUpdate keeps only approved tracing headers
// while replacing the generated Claude Code identity headers. Timezone is an
// account-level fingerprint boundary, so the credential and runtime header
// snapshot must be updated together instead of only changing a display field.
func prepareClaudeTimezoneCredentialUpdate(row *database.AccountRow, timezone string, updates map[string]interface{}) error {
	_, err := prepareClaudeTimezoneCredentialUpdateWithHeaders(row, timezone, updates, nil)
	return err
}

func prepareClaudeTimezoneCredentialUpdateWithHeaders(row *database.AccountRow, timezone string, updates map[string]interface{}, requestedHeaders map[string]string) (bool, error) {
	if row == nil || updates == nil {
		return false, nil
	}
	if !strings.EqualFold(strings.TrimSpace(row.Platform), "anthropic") &&
		!strings.EqualFold(strings.TrimSpace(row.GetCredential("upstream_type")), auth.UpstreamClaude) {
		return false, nil
	}
	timezone = strings.TrimSpace(timezone)
	if err := validateAccountTimezone(timezone); err != nil {
		return false, err
	}
	identity := make(map[string]struct{}, len(auth.ClaudeIdentityHeaderNames))
	for _, name := range auth.ClaudeIdentityHeaderNames {
		identity[strings.ToLower(strings.TrimSpace(name))] = struct{}{}
	}
	baseHeaders := row.GetCredentialStringMap("custom_headers")
	if requestedHeaders != nil {
		baseHeaders = requestedHeaders
	}
	// Keys are canonicalized up front: the generated fingerprint says
	// "X-Stainless-OS" while previously persisted headers come back as
	// "X-Stainless-Os", and normalizeCustomHeaders treats a case-only clash
	// with different values as an error. Without this a same-timezone save
	// failed whenever the freshly rolled OS differed from the stored one.
	// 先统一成规范大小写：指纹生成的是 X-Stainless-OS，落库后读回是 X-Stainless-Os，
	// 否则同时区保存时随机到不同 OS 就会触发"大小写重复且值冲突"。
	merged := make(map[string]string)
	keepIdentity := requestedHeaders == nil && strings.EqualFold(strings.TrimSpace(row.GetCredential("timezone")), timezone)
	for name, value := range auth.GenerateClaudeFingerprint(timezone).Headers() {
		merged[http.CanonicalHeaderKey(strings.TrimSpace(name))] = value
	}
	// An explicit header patch may omit account metadata. Keep the stored
	// device ID unless the operator also supplies a replacement for it.
	for name, value := range row.GetCredentialStringMap("custom_headers") {
		if strings.EqualFold(strings.TrimSpace(name), auth.ClaudeDeviceIDCredentialKey) {
			merged[http.CanonicalHeaderKey(auth.ClaudeDeviceIDCredentialKey)] = value
		}
	}
	for name, value := range baseHeaders {
		lowerName := strings.ToLower(strings.TrimSpace(name))
		canonicalName := http.CanonicalHeaderKey(strings.TrimSpace(name))
		if _, isIdentity := identity[lowerName]; isIdentity {
			// Keep a complete existing fingerprint stable when the operator
			// saves the same timezone again; a timezone change (or explicit
			// header patch) intentionally rotates the identity snapshot.
			if keepIdentity {
				merged[canonicalName] = value
			}
			continue
		}
		// Rotating the platform fingerprint must not rotate or erase the
		// account's persistent device identity.
		if lowerName == auth.ClaudeDeviceIDCredentialKey || isClaudeSafeOperationalHeader(name) {
			merged[canonicalName] = value
		}
	}
	normalized, err := normalizeCustomHeaders(merged)
	if err != nil {
		return false, err
	}
	updates["custom_headers"] = normalized
	updates["timezone"] = timezone
	return true, nil
}

// Only a small, explicit set of tracing headers may survive a Claude
// fingerprint rebuild.  This prevents historical Authorization/Cookie/API-key
// values (or arbitrary operator headers) from being copied into a credential
// update merely because they happened to be present in custom_headers.
func isClaudeSafeOperationalHeader(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "traceparent", "tracestate", "x-request-id", "x-client-request-id", "x-correlation-id", "x-trace-id":
		return true
	default:
		return false
	}
}

func claudeExportFingerprintHeaders(headers map[string]string) map[string]string {
	allowed := make(map[string]struct{}, len(auth.ClaudeIdentityHeaderNames))
	for _, name := range auth.ClaudeIdentityHeaderNames {
		allowed[strings.ToLower(strings.TrimSpace(name))] = struct{}{}
	}
	allowed[auth.ClaudeDeviceIDCredentialKey] = struct{}{}
	out := make(map[string]string)
	for name, value := range headers {
		if _, ok := allowed[strings.ToLower(strings.TrimSpace(name))]; !ok {
			continue
		}
		if strings.TrimSpace(value) == "" {
			continue
		}
		out[http.CanonicalHeaderKey(strings.TrimSpace(name))] = strings.TrimSpace(value)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func claudeAccountRowToExportEntry(row *database.AccountRow, groupRefs []claudeGroupRef) (claudeExportEntry, bool) {
	if row == nil {
		return claudeExportEntry{}, false
	}
	if !strings.EqualFold(strings.TrimSpace(row.Platform), "anthropic") &&
		!strings.EqualFold(strings.TrimSpace(row.GetCredential("upstream_type")), auth.UpstreamClaude) {
		return claudeExportEntry{}, false
	}
	accessToken := strings.TrimSpace(row.GetCredential("access_token"))
	refreshToken := strings.TrimSpace(row.GetCredential("refresh_token"))
	if accessToken == "" || refreshToken == "" {
		return claudeExportEntry{}, false
	}
	entry := claudeExportEntry{
		Type:                  "claude",
		Version:               claudeCredentialExportVersion,
		AuthKind:              "oauth",
		Email:                 strings.TrimSpace(row.GetCredential("email")),
		Name:                  row.Name,
		AccessToken:           accessToken,
		RefreshToken:          refreshToken,
		AccountID:             strings.TrimSpace(row.GetCredential("account_id")),
		ExpiresAt:             strings.TrimSpace(row.GetCredential("expires_at")),
		PlanType:              strings.TrimSpace(row.GetCredential("plan_type")),
		Models:                row.GetCredentialStringSlice("models"),
		ProxyURL:              strings.TrimSpace(row.ProxyURL),
		Timezone:              strings.TrimSpace(row.GetCredential("timezone")),
		ClaudeFingerprintMode: auth.NormalizeClaudeFingerprintMode(row.GetCredential(auth.ClaudeFingerprintModeCredentialKey)),
		FingerprintHeaders:    claudeExportFingerprintHeaders(row.GetCredentialStringMap("custom_headers")),
		Tags:                  append([]string(nil), row.Tags...),
		GroupRefs:             append([]claudeGroupRef(nil), groupRefs...),
		Enabled:               row.Enabled,
	}
	entry.exportFileName = claudeExportFileName(entry.Email, entry.Name, row.ID)
	return entry, true
}

func parseClaudeExportIDSet(raw string, present bool) (map[int64]bool, error) {
	if !present {
		return nil, nil
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("ids must contain at least one positive account ID")
	}
	ids := make(map[int64]bool)
	for _, value := range strings.Split(raw, ",") {
		id, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil || id <= 0 {
			return nil, errors.New("ids must contain only positive account IDs")
		}
		ids[id] = true
	}
	return ids, nil
}

// resolveClaudeGroupRefs maps portable references to this instance's IDs. A
// non-Claude or missing reference is reported in missing rather than guessed.
func (h *Handler) resolveClaudeGroupRefs(ctx context.Context, refs []claudeGroupRef) ([]int64, []string, error) {
	if len(refs) == 0 {
		return nil, nil, nil
	}
	groups, err := h.db.ListAccountGroups(ctx)
	if err != nil {
		return nil, nil, err
	}
	index := make(map[string]int64, len(groups))
	for _, group := range groups {
		channel := database.NormalizeAccountGroupChannel(group.Channel)
		key := channel + "\x00" + strings.ToLower(strings.TrimSpace(group.Name))
		if strings.TrimSpace(group.Name) != "" {
			index[key] = group.ID
		}
	}
	ids := make([]int64, 0, len(refs))
	missing := make([]string, 0)
	seen := make(map[int64]struct{}, len(refs))
	for _, ref := range refs {
		name := strings.TrimSpace(ref.Name)
		channel := strings.TrimSpace(ref.Channel)
		if channel == "" {
			channel = database.AccountGroupChannelClaude
		} else {
			channel = database.NormalizeAccountGroupChannel(channel)
		}
		if name == "" || channel != database.AccountGroupChannelClaude {
			if name != "" {
				missing = append(missing, name)
			}
			continue
		}
		id, ok := index[channel+"\x00"+strings.ToLower(name)]
		if !ok {
			missing = append(missing, name)
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids, missing, nil
}

// claudeImportWire is intentionally permissive about unknown future fields so
// a newer exporter can still be consumed by an older gateway. Validation below
// rejects unsupported provider/auth shapes and unsafe values.
type claudeImportWire struct {
	Type                  string                      `json:"type"`
	UpstreamType          string                      `json:"upstream_type"`
	Version               int                         `json:"version"`
	AuthKind              string                      `json:"auth_kind"`
	Email                 string                      `json:"email"`
	Name                  string                      `json:"name"`
	AccessToken           string                      `json:"access_token"`
	RefreshToken          string                      `json:"refresh_token"`
	AccountID             string                      `json:"account_id"`
	ExpiresAt             string                      `json:"expires_at"`
	PlanType              string                      `json:"plan_type"`
	Models                []string                    `json:"models"`
	ProxyURL              string                      `json:"proxy_url"`
	UseProxyPool          bool                        `json:"use_proxy_pool"`
	Timezone              string                      `json:"timezone"`
	ClaudeFingerprintMode string                      `json:"claude_fingerprint_mode"`
	FingerprintHeaders    map[string]string           `json:"fingerprint_headers"`
	CustomHeaders         map[string]string           `json:"custom_headers"`
	Tags                  []string                    `json:"tags"`
	GroupRefs             []claudeGroupRef            `json:"group_refs"`
	Groups                []claudeGroupRef            `json:"groups"`
	Enabled               *bool                       `json:"enabled"`
	Credentials           *claudeImportCredentialWire `json:"credentials"`
}

type claudeImportCredentialWire struct {
	Type                  string            `json:"type"`
	UpstreamType          string            `json:"upstream_type"`
	Version               int               `json:"version"`
	AuthKind              string            `json:"auth_kind"`
	Email                 string            `json:"email"`
	Name                  string            `json:"name"`
	AccessToken           string            `json:"access_token"`
	RefreshToken          string            `json:"refresh_token"`
	AccountID             string            `json:"account_id"`
	ExpiresAt             string            `json:"expires_at"`
	PlanType              string            `json:"plan_type"`
	Models                []string          `json:"models"`
	ProxyURL              string            `json:"proxy_url"`
	UseProxyPool          bool              `json:"use_proxy_pool"`
	Timezone              string            `json:"timezone"`
	ClaudeFingerprintMode string            `json:"claude_fingerprint_mode"`
	FingerprintHeaders    map[string]string `json:"fingerprint_headers"`
	CustomHeaders         map[string]string `json:"custom_headers"`
	Tags                  []string          `json:"tags"`
	GroupRefs             []claudeGroupRef  `json:"group_refs"`
	Groups                []claudeGroupRef  `json:"groups"`
	Enabled               *bool             `json:"enabled"`
}

func mergeClaudeImportWire(root claudeImportWire, nested *claudeImportCredentialWire) claudeImportWire {
	if nested == nil {
		return root
	}
	if root.Type == "" {
		root.Type = nested.Type
	}
	if root.UpstreamType == "" {
		root.UpstreamType = nested.UpstreamType
	}
	if root.Version == 0 {
		root.Version = nested.Version
	}
	if root.AuthKind == "" {
		root.AuthKind = nested.AuthKind
	}
	if root.Email == "" {
		root.Email = nested.Email
	}
	if root.Name == "" {
		root.Name = nested.Name
	}
	if root.AccessToken == "" {
		root.AccessToken = nested.AccessToken
	}
	if root.RefreshToken == "" {
		root.RefreshToken = nested.RefreshToken
	}
	if root.AccountID == "" {
		root.AccountID = nested.AccountID
	}
	if root.ExpiresAt == "" {
		root.ExpiresAt = nested.ExpiresAt
	}
	if root.PlanType == "" {
		root.PlanType = nested.PlanType
	}
	if len(root.Models) == 0 {
		root.Models = nested.Models
	}
	if root.ProxyURL == "" {
		root.ProxyURL = nested.ProxyURL
	}
	if !root.UseProxyPool {
		root.UseProxyPool = nested.UseProxyPool
	}
	if root.Timezone == "" {
		root.Timezone = nested.Timezone
	}
	if root.ClaudeFingerprintMode == "" {
		root.ClaudeFingerprintMode = nested.ClaudeFingerprintMode
	}
	if len(root.FingerprintHeaders) == 0 {
		root.FingerprintHeaders = nested.FingerprintHeaders
	}
	if len(root.CustomHeaders) == 0 {
		root.CustomHeaders = nested.CustomHeaders
	}
	if len(root.Tags) == 0 {
		root.Tags = nested.Tags
	}
	if len(root.GroupRefs) == 0 {
		root.GroupRefs = nested.GroupRefs
	}
	if len(root.Groups) == 0 {
		root.Groups = nested.Groups
	}
	if root.Enabled == nil {
		root.Enabled = nested.Enabled
	}
	return root
}

func normalizeClaudeImportTags(tags []string) ([]string, error) {
	if len(tags) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(tags))
	out := make([]string, 0, len(tags))
	for _, raw := range tags {
		value := strings.TrimSpace(raw)
		if value == "" {
			continue
		}
		if err := validateClaudeImportMetadata(value, "tags", 40); err != nil {
			return nil, err
		}
		key := strings.ToLower(value)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	if len(out) > 32 {
		return nil, errors.New("tags contains more than 32 items")
	}
	return out, nil
}

func validateClaudeImportMetadata(value, field string, maxRunes int) error {
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s must be valid UTF-8", field)
	}
	if maxRunes > 0 && utf8.RuneCountInString(value) > maxRunes {
		return fmt.Errorf("%s exceeds %d characters", field, maxRunes)
	}
	for _, r := range value {
		if unicode.IsControl(r) || r == 0x7f {
			return fmt.Errorf("%s contains a control character", field)
		}
	}
	return nil
}

func normalizeClaudeImportModels(models []string) ([]string, error) {
	if len(models) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(models))
	out := make([]string, 0, len(models))
	for _, raw := range models {
		model := strings.TrimSpace(raw)
		if model == "" {
			continue
		}
		if !strings.HasPrefix(strings.ToLower(model), "claude-") {
			return nil, fmt.Errorf("models contains non-Claude model %q", model)
		}
		if err := security.ValidateModelName(model); err != nil {
			return nil, fmt.Errorf("invalid Claude model %q: %w", model, err)
		}
		key := strings.ToLower(model)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, model)
	}
	return out, nil
}

func normalizeClaudeGroupRefs(refs []claudeGroupRef) ([]claudeGroupRef, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	if len(refs) > 32 {
		return nil, errors.New("group_refs contains more than 32 items")
	}
	seen := make(map[string]struct{}, len(refs))
	out := make([]claudeGroupRef, 0, len(refs))
	for _, ref := range refs {
		name := strings.TrimSpace(ref.Name)
		if name == "" {
			continue
		}
		if err := validateClaudeImportMetadata(name, "group_refs.name", 80); err != nil {
			return nil, err
		}
		channel := strings.TrimSpace(ref.Channel)
		if channel == "" {
			channel = database.AccountGroupChannelClaude
		} else {
			channel = database.NormalizeAccountGroupChannel(channel)
		}
		if channel != database.AccountGroupChannelClaude {
			return nil, fmt.Errorf("group_refs channel must be claude, got %q", ref.Channel)
		}
		key := channel + "\x00" + strings.ToLower(name)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, claudeGroupRef{Name: name, Channel: channel})
	}
	return out, nil
}

func claudeImportDocumentFromWire(raw claudeImportWire) (claudeImportDocument, error) {
	raw = mergeClaudeImportWire(raw, raw.Credentials)
	if raw.Type != "" && !strings.EqualFold(strings.TrimSpace(raw.Type), "claude") && !strings.EqualFold(strings.TrimSpace(raw.Type), "anthropic") {
		return claudeImportDocument{}, fmt.Errorf("unsupported credential type %q", raw.Type)
	}
	if raw.UpstreamType != "" && !strings.EqualFold(strings.TrimSpace(raw.UpstreamType), auth.UpstreamClaude) {
		return claudeImportDocument{}, fmt.Errorf("unsupported upstream_type %q", raw.UpstreamType)
	}
	if raw.Version < 0 || raw.Version > claudeCredentialExportVersion {
		return claudeImportDocument{}, fmt.Errorf("unsupported Claude credential version %d", raw.Version)
	}
	authKind := strings.ToLower(strings.TrimSpace(raw.AuthKind))
	if authKind != "" && authKind != "oauth" {
		return claudeImportDocument{}, errors.New("Claude credential auth_kind must be oauth")
	}
	accessToken := strings.TrimSpace(raw.AccessToken)
	refreshToken := strings.TrimSpace(raw.RefreshToken)
	if accessToken == "" || refreshToken == "" {
		return claudeImportDocument{}, errors.New("Claude credential requires access_token and refresh_token")
	}
	for _, metadata := range []struct {
		field    string
		value    string
		maxRunes int
	}{
		{field: "email", value: strings.TrimSpace(raw.Email), maxRunes: 320},
		{field: "name", value: strings.TrimSpace(raw.Name), maxRunes: 120},
		{field: "account_id", value: strings.TrimSpace(raw.AccountID), maxRunes: 128},
		{field: "plan_type", value: strings.TrimSpace(raw.PlanType), maxRunes: 80},
	} {
		if err := validateClaudeImportMetadata(metadata.value, metadata.field, metadata.maxRunes); err != nil {
			return claudeImportDocument{}, err
		}
	}
	timezone := strings.TrimSpace(raw.Timezone)
	if err := validateAccountTimezone(timezone); err != nil {
		return claudeImportDocument{}, err
	}
	fingerprintMode := auth.NormalizeClaudeFingerprintMode(raw.ClaudeFingerprintMode)
	if !auth.IsValidClaudeFingerprintMode(raw.ClaudeFingerprintMode) {
		return claudeImportDocument{}, errors.New("claude_fingerprint_mode must be preserve, force, or empty")
	}
	headers := raw.FingerprintHeaders
	if len(headers) == 0 {
		headers = raw.CustomHeaders
	}
	normalizedHeaders, err := normalizeClaudeFingerprintHeaders(headers)
	if err != nil {
		return claudeImportDocument{}, err
	}
	models, err := normalizeClaudeImportModels(raw.Models)
	if err != nil {
		return claudeImportDocument{}, err
	}
	tags, err := normalizeClaudeImportTags(raw.Tags)
	if err != nil {
		return claudeImportDocument{}, err
	}
	refs := raw.GroupRefs
	if len(refs) == 0 {
		refs = raw.Groups
	}
	refs, err = normalizeClaudeGroupRefs(refs)
	if err != nil {
		return claudeImportDocument{}, err
	}
	proxyURL := strings.TrimSpace(raw.ProxyURL)
	if proxyURL != "" {
		if err := security.ValidateProxyURL(proxyURL); err != nil {
			return claudeImportDocument{}, errors.New("proxy_url is invalid")
		}
	}
	if expires := strings.TrimSpace(raw.ExpiresAt); expires != "" {
		if _, err := time.Parse(time.RFC3339, expires); err != nil {
			return claudeImportDocument{}, errors.New("expires_at must be an RFC3339 timestamp")
		}
	}
	return claudeImportDocument{
		Type: strings.TrimSpace(raw.Type), Version: raw.Version, AuthKind: authKind,
		Email: strings.TrimSpace(raw.Email), Name: strings.TrimSpace(raw.Name),
		AccessToken: accessToken, RefreshToken: refreshToken, AccountID: strings.TrimSpace(raw.AccountID),
		ExpiresAt: strings.TrimSpace(raw.ExpiresAt), PlanType: strings.TrimSpace(raw.PlanType),
		Models: models, ProxyURL: proxyURL, UseProxyPool: raw.UseProxyPool,
		Timezone: timezone, ClaudeFingerprintMode: fingerprintMode, FingerprintHeaders: normalizedHeaders,
		Tags: tags, GroupRefs: refs, Enabled: raw.Enabled,
	}, nil
}

func parseClaudeImportDocuments(raw []byte) ([]claudeImportDocument, error) {
	if len(raw) == 0 {
		return nil, errors.New("credential content is empty")
	}
	if len(raw) > claudeCredentialExportMaxBytes {
		return nil, fmt.Errorf("credential content exceeds %d bytes", claudeCredentialExportMaxBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("parse credential JSON: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("credential content must contain exactly one JSON document")
	}
	documents := make([]claudeImportDocument, 0, 1)
	var collect func(any) error
	collect = func(item any) error {
		if len(documents) >= claudeCredentialImportMaxEntries {
			return fmt.Errorf("credential bundle contains more than %d entries", claudeCredentialImportMaxEntries)
		}
		switch typed := item.(type) {
		case []any:
			for _, child := range typed {
				if err := collect(child); err != nil {
					return err
				}
			}
			return nil
		case map[string]any:
			if accounts, ok := typed["accounts"]; ok {
				if list, ok := accounts.([]any); ok {
					for _, child := range list {
						if err := collect(child); err != nil {
							return err
						}
					}
					return nil
				}
				return errors.New("accounts must be an array")
			}
			encoded, err := json.Marshal(typed)
			if err != nil {
				return err
			}
			var wire claudeImportWire
			if err := json.Unmarshal(encoded, &wire); err != nil {
				return fmt.Errorf("invalid Claude credential object: %w", err)
			}
			document, err := claudeImportDocumentFromWire(wire)
			if err != nil {
				return err
			}
			documents = append(documents, document)
			return nil
		default:
			return fmt.Errorf("unsupported credential JSON type %T", item)
		}
	}
	if err := collect(value); err != nil {
		return nil, err
	}
	if len(documents) == 0 {
		return nil, errors.New("credential content contains no Claude credentials")
	}
	return documents, nil
}

// ExportClaudeAccounts downloads one JSON credential document or a ZIP with
// one document per account. The endpoint is admin-authenticated by the route
// group and deliberately uses secret download headers.
func (h *Handler) ExportClaudeAccounts(c *gin.Context) {
	filter := strings.ToLower(strings.TrimSpace(c.DefaultQuery("filter", "all")))
	if filter != "all" && filter != "healthy" {
		writeError(c, http.StatusBadRequest, "filter must be all or healthy")
		return
	}
	format := strings.ToLower(strings.TrimSpace(c.DefaultQuery("format", "auto")))
	if format != "auto" && format != "json" && format != "zip" {
		writeError(c, http.StatusBadRequest, "format must be auto, json, or zip")
		return
	}
	idSet, err := parseClaudeExportIDSet(c.Query("ids"), c.Request.URL.Query().Has("ids"))
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 15*time.Second)
	defer cancel()
	rows, err := h.db.ListActiveByChannel(ctx, database.UpstreamChannelClaude)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if filter == "healthy" && h.store == nil {
		writeError(c, http.StatusNotFound, "no exportable Claude accounts")
		return
	}
	runtimeByID := make(map[int64]*auth.Account)
	if filter == "healthy" {
		for _, account := range h.store.Accounts() {
			runtimeByID[account.DBID] = account
		}
	}
	accountIDs := make([]int64, 0, len(rows))
	for _, row := range rows {
		if idSet != nil && !idSet[row.ID] {
			continue
		}
		if filter == "healthy" {
			account, ok := runtimeByID[row.ID]
			if !ok || !account.IsAvailable() {
				continue
			}
		}
		accountIDs = append(accountIDs, row.ID)
	}
	memberships, err := h.db.ListAccountGroupMembershipsByAccountIDs(ctx, accountIDs)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	groups, err := h.db.ListAccountGroups(ctx)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	groupByID := make(map[int64]database.AccountGroup, len(groups))
	for _, group := range groups {
		groupByID[group.ID] = group
	}
	entries := make([]claudeExportEntry, 0, len(accountIDs))
	for _, row := range rows {
		if idSet != nil && !idSet[row.ID] {
			continue
		}
		if filter == "healthy" {
			account, ok := runtimeByID[row.ID]
			if !ok || !account.IsAvailable() {
				continue
			}
		}
		refs := make([]claudeGroupRef, 0, len(memberships[row.ID]))
		for _, groupID := range memberships[row.ID] {
			if group, ok := groupByID[groupID]; ok && database.NormalizeAccountGroupChannel(group.Channel) == database.AccountGroupChannelClaude {
				refs = append(refs, claudeGroupRef{Name: strings.TrimSpace(group.Name), Channel: database.AccountGroupChannelClaude})
			}
		}
		if entry, ok := claudeAccountRowToExportEntry(row, refs); ok {
			entries = append(entries, entry)
		}
	}
	if len(entries) == 0 {
		writeError(c, http.StatusNotFound, "no exportable Claude accounts")
		return
	}
	// Record only aggregate, non-secret audit metadata. Never include an email,
	// account ID, token, proxy URL, or serialized response body.
	security.SecurityAuditLog("CLAUDE_ACCOUNT_EXPORTED", fmt.Sprintf("count=%d filter=%s format=%s ip=%s", len(entries), filter, format, c.ClientIP()))
	useJSON := format == "json" || (format == "auto" && len(entries) == 1)
	if useJSON {
		var encoded []byte
		if len(entries) == 1 {
			encoded, err = marshalClaudeExportEntry(entries[0])
		} else {
			encoded, err = json.MarshalIndent(entries, "", "  ")
		}
		if err != nil {
			writeInternalError(c, err)
			return
		}
		writeSecretDownloadHeaders(c, fmt.Sprintf("codex2api-claude-%s-%d.json", time.Now().UTC().Format("20060102-150405"), len(entries)))
		c.Header("X-Export-Count", strconv.Itoa(len(entries)))
		c.Data(http.StatusOK, "application/json; charset=utf-8", encoded)
		return
	}
	archive, err := buildClaudeExportZIP(entries)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	writeSecretDownloadHeaders(c, fmt.Sprintf("codex2api-claude-%s-%d.zip", time.Now().UTC().Format("20060102-150405"), len(entries)))
	c.Header("X-Export-Count", strconv.Itoa(len(entries)))
	c.Data(http.StatusOK, "application/zip", archive)
}
