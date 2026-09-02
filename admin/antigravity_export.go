package admin

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// antigravityExportEntry is a portable, secret-bearing credential document.
// It is intentionally returned only by the admin-only download endpoint.
type antigravityExportEntry struct {
	Type           string   `json:"type"`
	Version        int      `json:"version"`
	AuthKind       string   `json:"auth_kind"`
	Email          string   `json:"email,omitempty"`
	Name           string   `json:"name,omitempty"`
	AccessToken    string   `json:"access_token,omitempty"`
	RefreshToken   string   `json:"refresh_token,omitempty"`
	IDToken        string   `json:"id_token,omitempty"`
	ProjectID      string   `json:"project_id,omitempty"`
	OAuthClientKey string   `json:"oauth_client_key,omitempty"`
	ClientID       string   `json:"client_id,omitempty"`
	ClientSecret   string   `json:"client_secret,omitempty"`
	Scope          string   `json:"scope,omitempty"`
	ExpiresAt      string   `json:"expires_at,omitempty"`
	APIKey         string   `json:"api_key,omitempty"`
	Models         []string `json:"models,omitempty"`
	ModelMapping   string   `json:"model_mapping,omitempty"`
	ProxyURL       string   `json:"proxy_url,omitempty"`
	ProxyLabel     string   `json:"proxy_label,omitempty"`
	ProxyEnabled   *bool    `json:"proxy_enabled,omitempty"`
	Disabled       bool     `json:"disabled"`

	exportFileName string
}

// antigravityAccountRowToExportEntry converts an account row into an export
// entry. Unlike the CPA and Grok exports, proxies defaults to enabled for this
// channel: it has always emitted proxy_url, and dropping it would silently
// break the round-trip of existing backups.
func antigravityAccountRowToExportEntry(row *database.AccountRow, proxies exportProxyResolver) (antigravityExportEntry, bool) {
	if !isAntigravityAccountRow(row) {
		return antigravityExportEntry{}, false
	}
	apiKey := strings.TrimSpace(row.GetCredential("api_key"))
	accessToken := strings.TrimSpace(row.GetCredential("access_token"))
	refreshToken := strings.TrimSpace(row.GetCredential("refresh_token"))
	if apiKey == "" && accessToken == "" && refreshToken == "" {
		return antigravityExportEntry{}, false
	}
	proxyURL, proxyLabel, proxyEnabled := proxies.resolve(row.ProxyURL)
	entry := antigravityExportEntry{
		Type: "antigravity", Version: 1, Email: row.GetCredential("email"), Name: row.Name,
		ProxyURL: proxyURL, ProxyLabel: proxyLabel, ProxyEnabled: proxyEnabled, Disabled: !row.Enabled,
	}
	if apiKey != "" {
		entry.AuthKind = auth.AntigravityAuthKindAPIKey
		entry.APIKey = apiKey
		entry.Models = row.GetCredentialStringSlice("models")
		entry.ModelMapping = row.GetCredential("model_mapping")
	} else {
		entry.AuthKind = auth.AntigravityAuthKindOAuth
		entry.AccessToken = accessToken
		entry.RefreshToken = refreshToken
		entry.IDToken = row.GetCredential("id_token")
		entry.ProjectID = row.GetCredential("project_id")
		entry.OAuthClientKey = row.GetCredential("oauth_client_key")
		entry.ClientID = row.GetCredential("antigravity_client_id")
		entry.ClientSecret = row.GetCredential("antigravity_client_secret")
		entry.Scope = row.GetCredential("oauth_scope")
		entry.ExpiresAt = row.GetCredential("expires_at")
	}
	entry.exportFileName = antigravityExportFileName(entry.Email, row.ID)
	return entry, true
}

func isAntigravityAccountRow(row *database.AccountRow) bool {
	if row == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(row.GetCredential("upstream_type")), auth.UpstreamAntigravity) ||
		strings.EqualFold(strings.TrimSpace(row.Platform), auth.UpstreamAntigravity)
}

var antigravityExportUnsafeFileChars = regexp.MustCompile(`[^A-Za-z0-9@._-]`)

func antigravityExportFileName(email string, id int64) string {
	safe := antigravityExportUnsafeFileChars.ReplaceAllString(strings.TrimSpace(email), "")
	safe = strings.TrimLeft(safe, ".")
	if safe == "" {
		safe = fmt.Sprintf("account-%d", id)
	}
	return safe + ".json"
}

func marshalAntigravityExportEntry(entry antigravityExportEntry) ([]byte, error) {
	return json.MarshalIndent(entry, "", "  ")
}

func buildAntigravityExportZIP(entries []antigravityExportEntry) ([]byte, error) {
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	used := make(map[string]int, len(entries))
	for _, entry := range entries {
		name := entry.exportFileName
		if seen := used[name]; seen > 0 {
			ext := path.Ext(name)
			name = fmt.Sprintf("%s-%d%s", strings.TrimSuffix(name, ext), seen+1, ext)
		}
		used[entry.exportFileName]++
		member, err := writer.Create(name)
		if err != nil {
			return nil, err
		}
		encoded, err := marshalAntigravityExportEntry(entry)
		if err != nil {
			return nil, err
		}
		if _, err := member.Write(encoded); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func antigravityExportDownloadName(count int, extension string) string {
	return fmt.Sprintf("codex2api-antigravity-%s-%d.%s", time.Now().UTC().Format("20060102-150405"), count, extension)
}

// writeSecretResponseHeaders marks a response as secret-bearing so it is never
// cached or content-sniffed. Split from writeSecretDownloadHeaders because the
// CPA export endpoints return an inline JSON array the frontend names itself —
// they need the cache guards without the download disposition.
func writeSecretResponseHeaders(c *gin.Context) {
	c.Header("Cache-Control", "no-store, max-age=0")
	c.Header("Pragma", "no-cache")
	c.Header("X-Content-Type-Options", "nosniff")
}

func writeSecretDownloadHeaders(c *gin.Context, filename string) {
	writeSecretResponseHeaders(c)
	c.Header("Content-Disposition", `attachment; filename="`+filename+`"`)
}

func parseAntigravityExportIDSet(raw string, present bool) (map[int64]bool, error) {
	if !present {
		return nil, nil
	}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, errors.New("ids must contain at least one positive account ID")
	}
	ids := make(map[int64]bool)
	for _, item := range strings.Split(raw, ",") {
		id, err := strconv.ParseInt(strings.TrimSpace(item), 10, 64)
		if err != nil || id <= 0 {
			return nil, errors.New("ids must contain only positive account IDs")
		}
		ids[id] = true
	}
	return ids, nil
}

// ExportAntigravityAccounts downloads one JSON credential document or a ZIP
// containing one JSON member per selected account. The response contains live
// secrets and must never be cached, embedded, or logged.
func (h *Handler) ExportAntigravityAccounts(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	rows, err := h.db.ListActive(ctx)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	idSet, err := parseAntigravityExportIDSet(c.Query("ids"), c.Request.URL.Query().Has("ids"))
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	proxies := h.newExportProxyResolver(ctx, exportIncludeProxy(c, true))
	entries := make([]antigravityExportEntry, 0, len(rows))
	for _, row := range rows {
		if idSet != nil && !idSet[row.ID] {
			continue
		}
		if entry, ok := antigravityAccountRowToExportEntry(row, proxies); ok {
			entries = append(entries, entry)
		}
	}
	if len(entries) == 0 {
		writeError(c, http.StatusNotFound, "no exportable Antigravity accounts")
		return
	}
	if len(entries) == 1 {
		encoded, err := marshalAntigravityExportEntry(entries[0])
		if err != nil {
			writeInternalError(c, err)
			return
		}
		writeSecretDownloadHeaders(c, antigravityExportDownloadName(1, "json"))
		c.Header("X-Export-Count", "1")
		c.Data(http.StatusOK, "application/json; charset=utf-8", encoded)
		return
	}
	archive, err := buildAntigravityExportZIP(entries)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	writeSecretDownloadHeaders(c, antigravityExportDownloadName(len(entries), "zip"))
	c.Header("X-Export-Count", strconv.Itoa(len(entries)))
	c.Data(http.StatusOK, "application/zip", archive)
}
