package admin

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

// grokExportEntry 是单个 Grok 账号的导出形态：Grok CLI auth.json 的超集。
//
// 字段布局对齐 Grok CLI 单凭据文件（顶层扁平），同时补上
// client_id / oidc_issuer / principal_* / plan_type —— CLI 文件不带这些，
// 只能靠 access_token 的 JWT claims 兜底掏 client_id，AT 一过期就再也导不回来。
// 补齐后即便 AT 失效也能凭 refresh_token 继续刷新。
//
// auth_kind 与 auth_mode 同时写出：前者是 CLI 文件的字段名，后者是
// ParseGrokAuthJSON 实际识别 API Key 凭据的字段名，缺了后者 API Key 账号导不回来。
type grokExportEntry struct {
	Type          string `json:"type"`
	AuthKind      string `json:"auth_kind"`
	AuthMode      string `json:"auth_mode"`
	Email         string `json:"email,omitempty"`
	Sub           string `json:"sub,omitempty"`
	AccessToken   string `json:"access_token,omitempty"`
	RefreshToken  string `json:"refresh_token,omitempty"`
	IDToken       string `json:"id_token,omitempty"`
	ClientID      string `json:"client_id,omitempty"`
	TokenEndpoint string `json:"token_endpoint,omitempty"`
	OIDCIssuer    string `json:"oidc_issuer,omitempty"`
	PrincipalType string `json:"principal_type,omitempty"`
	PrincipalID   string `json:"principal_id,omitempty"`
	PlanType      string `json:"plan_type,omitempty"`
	BaseURL       string `json:"base_url,omitempty"`
	Expired       string `json:"expired,omitempty"`
	ExpiresIn     int64  `json:"expires_in,omitempty"`
	LastRefresh   string `json:"last_refresh,omitempty"`
	TokenType     string `json:"token_type,omitempty"`
	RedirectURI   string `json:"redirect_uri,omitempty"`
	Disabled      bool   `json:"disabled"`
	// 代理三件套只在 include_proxy=1 时写出，语义与 CPA 导出一致。
	ProxyURL     string `json:"proxy_url,omitempty"`
	ProxyLabel   string `json:"proxy_label,omitempty"`
	ProxyEnabled *bool  `json:"proxy_enabled,omitempty"`

	// exportFileName 是该条目在 ZIP 里的文件名，未导出字段本就不参与序列化。
	exportFileName string
}

// marshalGrokExportEntry 按参考文件的排版输出：2 空格缩进、键按字母序。
// encoding/json 对结构体按字段声明顺序输出，这里先转成 map 再编码以拿到字母序
// （Go 对 map 键排序），与 Grok CLI 写出的文件保持同形。
func marshalGrokExportEntry(entry grokExportEntry) ([]byte, error) {
	raw, err := json.Marshal(entry)
	if err != nil {
		return nil, err
	}
	var ordered map[string]any
	if err := json.Unmarshal(raw, &ordered); err != nil {
		return nil, err
	}
	return json.MarshalIndent(ordered, "", "  ")
}

// grokAccountRowToExportEntry 把账号行转成导出条目。凭据两者皆空时返回 ok=false
// （没有可迁移的东西）。proxies 决定是否连同账号绑定的代理一起导出。
func grokAccountRowToExportEntry(row *database.AccountRow, proxies exportProxyResolver) (grokExportEntry, bool) {
	if row == nil {
		return grokExportEntry{}, false
	}
	accessToken := row.GetCredential("access_token")
	refreshToken := row.GetCredential("refresh_token")
	apiKey := row.GetCredential("api_key")
	if accessToken == "" && refreshToken == "" && apiKey == "" {
		return grokExportEntry{}, false
	}

	proxyURL, proxyLabel, proxyEnabled := proxies.resolve(row.ProxyURL)
	entry := grokExportEntry{
		Type:         "xai",
		Email:        row.GetCredential("email"),
		Sub:          row.GetCredential("account_id"),
		PlanType:     row.GetCredential("plan_type"),
		BaseURL:      row.GetCredential("base_url"),
		LastRefresh:  row.UpdatedAt.UTC().Format(time.RFC3339),
		Disabled:     !row.Enabled,
		ProxyURL:     proxyURL,
		ProxyLabel:   proxyLabel,
		ProxyEnabled: proxyEnabled,
	}

	// API Key 凭据：ParseGrokAuthJSON 认的是 auth_mode=api_key + access_token 位置放 key，
	// 不带 refresh_token/OIDC 那一套。
	if apiKey != "" {
		entry.AuthKind = auth.GrokAuthKindAPIKey
		entry.AuthMode = auth.GrokAuthKindAPIKey
		entry.AccessToken = apiKey
		if entry.BaseURL == "" {
			entry.BaseURL = auth.GrokDefaultAPIBaseURL
		}
		entry.exportFileName = grokExportFileName(entry.Email, entry.Sub, row.ID)
		return entry, true
	}

	entry.AuthKind = auth.GrokAuthKindOAuth
	entry.AuthMode = auth.GrokAuthKindOAuth
	entry.AccessToken = accessToken
	entry.RefreshToken = refreshToken
	entry.IDToken = row.GetCredential("id_token")
	entry.ClientID = row.GetCredential("grok_client_id")
	entry.TokenEndpoint = row.GetCredential("grok_token_endpoint")
	entry.OIDCIssuer = row.GetCredential("grok_oidc_issuer")
	entry.PrincipalType = row.GetCredential("grok_principal_type")
	entry.PrincipalID = row.GetCredential("grok_principal_id")
	entry.TokenType = "Bearer"
	entry.RedirectURI = auth.GrokDefaultOAuthRedirectURI

	// 缺失的 OIDC 参数补默认值：导入侧对 client_id 是硬要求，token_endpoint /
	// oidc_issuer 缺失会让刷新走不通，而这些值对 Grok CLI 通道是固定的。
	if entry.ClientID == "" {
		entry.ClientID = auth.GrokDefaultOAuthClientID
	}
	if entry.TokenEndpoint == "" {
		entry.TokenEndpoint = auth.GrokDefaultTokenURL
	}
	if entry.OIDCIssuer == "" {
		entry.OIDCIssuer = auth.GrokDefaultOIDCIssuer
	}
	if entry.BaseURL == "" {
		entry.BaseURL = auth.GrokDefaultChatProxyBaseURL
	}

	if expired := strings.TrimSpace(row.GetCredential("expires_at")); expired != "" {
		entry.Expired = expired
		if expiresAt := grokParseExportTime(expired); !expiresAt.IsZero() {
			// expires_in 是相对当下的剩余秒数（与 CLI 文件语义一致）；已过期则省略，
			// 不输出负数误导消费方。
			if remaining := int64(time.Until(expiresAt).Seconds()); remaining > 0 {
				entry.ExpiresIn = remaining
			}
		}
	}

	entry.exportFileName = grokExportFileName(entry.Email, entry.Sub, row.ID)
	return entry, true
}

func grokParseExportTime(raw string) time.Time {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t
		}
	}
	return time.Time{}
}

// grokExportUnsafeFileChars 匹配文件名里不安全的字符。放行 @ 以便邮箱名保持可读
// （@ 在三大平台的文件名与 ZIP 条目名里都合法）；斜杠、点点等路径构造字符一律剔除。
// 凭据里的值来自上游，不能假定干净。
var grokExportUnsafeFileChars = regexp.MustCompile(`[^A-Za-z0-9@._-]`)

// grokExportFileName 生成 ZIP 内的文件名：优先 <邮箱>.json，邮箱缺失时退回
// <sub>.json，两者都拿不到时回落 account-<id>.json。
func grokExportFileName(email, sub string, id int64) string {
	for _, candidate := range []string{email, sub} {
		safe := grokExportUnsafeFileChars.ReplaceAllString(strings.TrimSpace(candidate), "")
		// 去掉纯点名（"." / ".."）与前导点，避免生成隐藏文件或路径穿越。
		safe = strings.TrimLeft(safe, ".")
		if safe != "" {
			return safe + ".json"
		}
	}
	return fmt.Sprintf("account-%d.json", id)
}

// ExportGrokAccounts 导出 Grok 账号凭据（GET /api/admin/accounts/grok/export）。
// 单个账号直接返回裸 JSON；多个账号打包成 ZIP，内部每账号一个 <邮箱>.json，
// 解开即可逐个导入。
func (h *Handler) ExportGrokAccounts(c *gin.Context) {
	filter := c.DefaultQuery("filter", "all")
	idsParam := c.Query("ids")

	// 与 /accounts/export 同一道门禁：导出文件含明文 refresh_token。
	if c.Query("remote") == "true" {
		if !h.hasConfiguredAdminSecret(c.Request.Context()) {
			writeError(c, http.StatusForbidden, "请先设置管理密钥，再启用远程迁移")
			return
		}
		if !h.store.GetAllowRemoteMigration() {
			writeError(c, http.StatusForbidden, "远程迁移未启用，请在系统设置中开启")
			return
		}
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()

	rows, err := h.db.ListActive(ctx)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "查询账号失败: "+err.Error())
		return
	}

	proxies := h.newExportProxyResolver(ctx, exportIncludeProxy(c, false))
	idSet := parseExportIDSet(idsParam)
	runtimeMap := make(map[int64]*auth.Account)
	if filter == "healthy" && h.store != nil {
		for _, account := range h.store.Accounts() {
			runtimeMap[account.DBID] = account
		}
	}

	entries := make([]grokExportEntry, 0, len(rows))
	for _, row := range rows {
		if !isGrokAccountRow(row) {
			continue
		}
		if idSet != nil && !idSet[row.ID] {
			continue
		}
		if filter == "healthy" {
			account, ok := runtimeMap[row.ID]
			if !ok || account.RuntimeStatus() != "active" {
				continue
			}
		}
		entry, ok := grokAccountRowToExportEntry(row, proxies)
		if !ok {
			continue
		}
		entries = append(entries, entry)
	}

	if len(entries) == 0 {
		writeError(c, http.StatusNotFound, "没有可导出的 Grok 账号")
		return
	}

	// 单账号：直接给裸 JSON，内容形态与 Grok CLI 的单凭据文件一致。
	if len(entries) == 1 {
		encoded, err := marshalGrokExportEntry(entries[0])
		if err != nil {
			writeError(c, http.StatusInternalServerError, "序列化导出文件失败: "+err.Error())
			return
		}
		writeSecretDownloadHeaders(c, grokExportDownloadName(1, "json"))
		c.Data(http.StatusOK, "application/json; charset=utf-8", encoded)
		return
	}

	archive, err := buildGrokExportZIP(entries)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "打包导出文件失败: "+err.Error())
		return
	}
	writeSecretDownloadHeaders(c, grokExportDownloadName(len(entries), "zip"))
	c.Data(http.StatusOK, "application/zip", archive)
}

// grokExportDownloadName 生成下载文件名，沿用仓库既有的导出命名约定
// codex2api-<平台>-<时间戳>-<数量>.<ext>（对齐 codex2api-recycle-… 等）。
// 注意这只是下载物的名字：ZIP 内部成员按 <邮箱>.json 命名，
// 保留每个账号的身份、也让解开后能逐个导入。
func grokExportDownloadName(count int, ext string) string {
	return fmt.Sprintf("codex2api-grok-%s-%d.%s", time.Now().UTC().Format("20060102-150405"), count, ext)
}

// accountRowToExportEntry 按平台分派导出形态：Grok/xAI 账号走 Grok CLI 超集形态，
// 传统 Codex 账号走 CPA 形态。Claude OAuth 不进入这个通用导出端点：其 token
// 不是 Codex auth.json，误导出为 type:"codex" 会导致回灌协议错误并扩大凭据暴露面。
//
// 通用导出端点原先对所有账号硬编码 type:"codex"，Grok 账号既被标错类型、又丢掉
// client_id / token_endpoint / oidc_issuer / principal_* —— 导出的文件回灌必然失败
// （导入侧对 client_id 是硬要求）。这里按平台分派修掉该问题。
func accountRowToExportEntry(row *database.AccountRow, proxies exportProxyResolver) (any, bool) {
	if row != nil && (strings.EqualFold(strings.TrimSpace(row.Platform), "anthropic") ||
		strings.EqualFold(strings.TrimSpace(row.GetCredential("upstream_type")), auth.UpstreamClaude)) {
		return nil, false
	}
	if isGrokAccountRow(row) {
		entry, ok := grokAccountRowToExportEntry(row, proxies)
		if !ok {
			return nil, false
		}
		return entry, true
	}
	entry, ok := accountRowToCPAExportEntry(row, proxies)
	if !ok {
		return nil, false
	}
	return entry, true
}

// isGrokAccountRow 判断账号行是否为 Grok/xAI 账号。平台字段与凭据里的
// upstream_type 都要认：历史数据可能只有其中一个。
func isGrokAccountRow(row *database.AccountRow) bool {
	if row == nil {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(row.Platform), "xai") {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(row.GetCredential("upstream_type")), auth.UpstreamGrok)
}

// buildGrokExportZIP 把多个条目打成 ZIP，每个条目一个 <sub>.json。
// 同名（同一 sub 出现多次）时追加序号，避免条目互相覆盖。
func buildGrokExportZIP(entries []grokExportEntry) ([]byte, error) {
	var buf bytes.Buffer
	writer := zip.NewWriter(&buf)
	used := make(map[string]int, len(entries))

	for _, entry := range entries {
		name := entry.exportFileName
		if seen := used[name]; seen > 0 {
			ext := path.Ext(name)
			name = fmt.Sprintf("%s-%d%s", strings.TrimSuffix(name, ext), seen+1, ext)
		}
		used[entry.exportFileName]++

		file, err := writer.Create(name)
		if err != nil {
			return nil, err
		}
		encoded, err := marshalGrokExportEntry(entry)
		if err != nil {
			return nil, err
		}
		if _, err := file.Write(encoded); err != nil {
			return nil, err
		}
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
