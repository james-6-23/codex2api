package admin

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

func grokOAuthRow() *database.AccountRow {
	return &database.AccountRow{
		ID:       5,
		Platform: "xai",
		Enabled:  true,
		Credentials: map[string]interface{}{
			"upstream_type":         auth.UpstreamGrok,
			"email":                 "mashir0zhao@gmail.com",
			"account_id":            "a7f5ab5b-8384-4a04-93d8-f55f55403b4e",
			"access_token":          "eyJhdCI.payload.sig",
			"refresh_token":         "rt-abc123",
			"id_token":              "eyJpZCI.payload.sig",
			"grok_client_id":        "b1a00492-073a-47ea-816f-4c329264a828",
			"grok_token_endpoint":   "https://auth.x.ai/oauth2/token",
			"grok_oidc_issuer":      "https://auth.x.ai",
			"grok_principal_type":   "User",
			"grok_principal_id":     "84ac4dd3-35d6-451c-84b6-019ad7419b41",
			"plan_type":             "supergrok_heavy",
			"base_url":              "https://cli-chat-proxy.grok.com/v1",
			"expires_at":            time.Now().Add(6 * time.Hour).UTC().Format(time.RFC3339),
			"grok_billing_detail":   `{"plan":"supergrok_heavy"}`,
			"grok_weekly_usage_pct": "22",
		},
		UpdatedAt: time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC),
	}
}

func TestGrokAccountRowToExportEntryOAuth(t *testing.T) {
	entry, ok := grokAccountRowToExportEntry(grokOAuthRow(), exportProxyResolver{})
	if !ok {
		t.Fatalf("OAuth 账号应可导出")
	}

	if entry.Type != "xai" {
		t.Errorf("type = %q, want xai", entry.Type)
	}
	// auth_kind 是 CLI 文件的字段名，auth_mode 是本项目解析器认的字段名，两个都要有。
	if entry.AuthKind != auth.GrokAuthKindOAuth || entry.AuthMode != auth.GrokAuthKindOAuth {
		t.Errorf("auth_kind=%q auth_mode=%q, 两者都应为 oauth", entry.AuthKind, entry.AuthMode)
	}
	if entry.ClientID != "b1a00492-073a-47ea-816f-4c329264a828" {
		t.Errorf("client_id 未导出: %q", entry.ClientID)
	}
	if entry.Sub != "a7f5ab5b-8384-4a04-93d8-f55f55403b4e" {
		t.Errorf("sub = %q", entry.Sub)
	}
	if entry.PrincipalID != "84ac4dd3-35d6-451c-84b6-019ad7419b41" || entry.PrincipalType != "User" {
		t.Errorf("principal 字段未导出: %+v", entry)
	}
	if entry.TokenType != "Bearer" {
		t.Errorf("token_type = %q, want Bearer", entry.TokenType)
	}
	if entry.RedirectURI != auth.GrokDefaultOAuthRedirectURI {
		t.Errorf("redirect_uri = %q", entry.RedirectURI)
	}
	if entry.Disabled {
		t.Errorf("enabled 账号的 disabled 应为 false")
	}
	// expires_in 是相对当下的剩余秒数，6 小时后到期应落在合理区间。
	if entry.ExpiresIn <= 0 || entry.ExpiresIn > 6*3600 {
		t.Errorf("expires_in = %d, 应为正数且不超过 6h", entry.ExpiresIn)
	}
	if entry.LastRefresh != "2026-07-31T12:00:00Z" {
		t.Errorf("last_refresh = %q", entry.LastRefresh)
	}

	// 用量/计费字段不进导出：导出只为迁移凭据，用量在新实例会重新探测。
	encoded, err := marshalGrokExportEntry(entry)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	for _, forbidden := range []string{"billing", "usage", "weekly", "monthly", "rate_limit"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("导出内容不应含 %q: %s", forbidden, encoded)
		}
	}
}

func TestAccountRowToExportEntrySkipsClaudeOAuth(t *testing.T) {
	row := &database.AccountRow{
		Platform: "anthropic",
		Credentials: map[string]interface{}{
			"upstream_type": auth.UpstreamClaude,
			"access_token":  "claude-access",
			"refresh_token": "claude-refresh",
		},
	}
	if entry, ok := accountRowToExportEntry(row, exportProxyResolver{}); ok || entry != nil {
		t.Fatalf("generic Codex export must skip Claude OAuth, entry=%#v ok=%v", entry, ok)
	}
}

// TestGrokExportRoundTripsThroughImporter 是补字段这个决策的验证点：
// 导出的文件必须能被 ParseGrokAuthJSON 解回来，且 client_id 不依赖 access_token
// 的 JWT claims —— AT 过期或缺失时也要能凭 refresh_token 继续刷新。
func TestGrokExportRoundTripsThroughImporter(t *testing.T) {
	row := grokOAuthRow()
	entry, ok := grokAccountRowToExportEntry(row, exportProxyResolver{})
	if !ok {
		t.Fatalf("导出失败")
	}
	encoded, err := marshalGrokExportEntry(entry)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}

	creds, err := auth.ParseGrokAuthJSON(encoded)
	if err != nil {
		t.Fatalf("导出文件无法被导入器解析: %v\n%s", err, encoded)
	}
	if len(creds) != 1 {
		t.Fatalf("应解出 1 条凭据，得到 %d", len(creds))
	}
	cred := creds[0]
	if cred.AuthKind() != auth.GrokAuthKindOAuth {
		t.Errorf("凭据类型 = %q, want oauth", cred.AuthKind())
	}
	if cred.RefreshToken != "rt-abc123" {
		t.Errorf("refresh_token = %q", cred.RefreshToken)
	}
	// 关键断言：access_token 是伪造的、JWT 掏不出 claims，client_id 只能来自显式字段。
	if cred.ClientID != "b1a00492-073a-47ea-816f-4c329264a828" {
		t.Errorf("client_id 未还原（说明仍依赖 JWT claims 兜底）: %q", cred.ClientID)
	}
	if cred.TokenEndpoint != "https://auth.x.ai/oauth2/token" {
		t.Errorf("token_endpoint = %q", cred.TokenEndpoint)
	}
	if cred.OIDCIssuer != "https://auth.x.ai" {
		t.Errorf("oidc_issuer = %q", cred.OIDCIssuer)
	}
	if cred.PrincipalID != "84ac4dd3-35d6-451c-84b6-019ad7419b41" {
		t.Errorf("principal_id = %q", cred.PrincipalID)
	}
	if cred.Subject != "a7f5ab5b-8384-4a04-93d8-f55f55403b4e" {
		t.Errorf("sub = %q", cred.Subject)
	}
	if cred.Email != "mashir0zhao@gmail.com" {
		t.Errorf("email = %q", cred.Email)
	}
}

// TestGrokExportAPIKeyRoundTrip API Key 账号必须写 auth_mode（解析器只认这个键），
// 否则会被当 OAuth 处理然后报缺 refresh_token。
func TestGrokExportAPIKeyRoundTrip(t *testing.T) {
	row := &database.AccountRow{
		ID:       9,
		Platform: "xai",
		Enabled:  true,
		Credentials: map[string]interface{}{
			"upstream_type": auth.UpstreamGrok,
			"api_key":       "xai-secret-key",
			"plan_type":     "api",
			"email":         "xai-api-key",
		},
		UpdatedAt: time.Now(),
	}
	entry, ok := grokAccountRowToExportEntry(row, exportProxyResolver{})
	if !ok {
		t.Fatalf("API Key 账号应可导出")
	}
	if entry.AuthMode != auth.GrokAuthKindAPIKey {
		t.Fatalf("auth_mode = %q, want api_key", entry.AuthMode)
	}
	if entry.RefreshToken != "" {
		t.Errorf("API Key 条目不应带 refresh_token")
	}
	if entry.BaseURL != auth.GrokDefaultAPIBaseURL {
		t.Errorf("base_url = %q, want %q", entry.BaseURL, auth.GrokDefaultAPIBaseURL)
	}

	encoded, err := marshalGrokExportEntry(entry)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	creds, err := auth.ParseGrokAuthJSON(encoded)
	if err != nil {
		t.Fatalf("API Key 导出文件无法解析: %v\n%s", err, encoded)
	}
	if creds[0].AuthKind() != auth.GrokAuthKindAPIKey {
		t.Fatalf("应识别为 API Key，实际 %q", creds[0].AuthKind())
	}
	if creds[0].APIKey != "xai-secret-key" {
		t.Errorf("api_key = %q", creds[0].APIKey)
	}
}

// TestGrokExportFillsMissingOIDCDefaults 实测里 free 账号缺 base_url / oidc_issuer，
// 缺失时要补 Grok CLI 通道的固定值，不能留空导致回灌后刷不了 token。
func TestGrokExportFillsMissingOIDCDefaults(t *testing.T) {
	row := &database.AccountRow{
		ID:       31,
		Platform: "xai",
		Enabled:  true,
		Credentials: map[string]interface{}{
			"upstream_type": auth.UpstreamGrok,
			"email":         "hzf5hmet@hx65zll.xyz",
			"account_id":    "sub-31",
			"access_token":  "at",
			"refresh_token": "rt-31",
			"plan_type":     "free",
		},
		UpdatedAt: time.Now(),
	}
	entry, ok := grokAccountRowToExportEntry(row, exportProxyResolver{})
	if !ok {
		t.Fatalf("导出失败")
	}
	if entry.ClientID != auth.GrokDefaultOAuthClientID {
		t.Errorf("client_id 应补默认值, got %q", entry.ClientID)
	}
	if entry.TokenEndpoint != auth.GrokDefaultTokenURL {
		t.Errorf("token_endpoint 应补默认值, got %q", entry.TokenEndpoint)
	}
	if entry.OIDCIssuer != auth.GrokDefaultOIDCIssuer {
		t.Errorf("oidc_issuer 应补默认值, got %q", entry.OIDCIssuer)
	}
	if entry.BaseURL != auth.GrokDefaultChatProxyBaseURL {
		t.Errorf("base_url 应补 chat-proxy 默认值, got %q", entry.BaseURL)
	}
	// 无 expires_at 时不应输出 expired / expires_in。
	if entry.Expired != "" || entry.ExpiresIn != 0 {
		t.Errorf("缺 expires_at 时不应输出到期字段: %+v", entry)
	}
}

func TestGrokAccountRowToExportEntrySkipsCredentialless(t *testing.T) {
	row := &database.AccountRow{
		ID:          7,
		Platform:    "xai",
		Credentials: map[string]interface{}{"upstream_type": auth.UpstreamGrok, "email": "x@y.z"},
	}
	if _, ok := grokAccountRowToExportEntry(row, exportProxyResolver{}); ok {
		t.Fatalf("无任何凭据的账号不应被导出")
	}
}

func TestGrokExportEntryDisabledReflectsEnabled(t *testing.T) {
	row := grokOAuthRow()
	row.Enabled = false
	entry, _ := grokAccountRowToExportEntry(row, exportProxyResolver{})
	if !entry.Disabled {
		t.Fatalf("enabled=false 应导出 disabled=true")
	}
}

func TestGrokExportFileName(t *testing.T) {
	cases := []struct {
		name  string
		email string
		sub   string
		id    int64
		want  string
	}{
		{"邮箱优先", "jfqaz5mx@hx65zll.xyz", "0de45e38-b9aa", 1, "jfqaz5mx@hx65zll.xyz.json"},
		{"@ 与点保留", "a.b+c@example.co.uk", "", 1, "a.bc@example.co.uk.json"},
		{"缺邮箱退回 sub", "", "0de45e38-b9aa-4bb8-9ff5-0f81bd977a1c", 1, "0de45e38-b9aa-4bb8-9ff5-0f81bd977a1c.json"},
		{"邮箱仅空白退回 sub", "   ", "sub-1", 1, "sub-1.json"},
		{"两者皆空回落 id", "", "", 42, "account-42.json"},
		{"两者仅空白回落 id", "  ", "  ", 42, "account-42.json"},
		{"邮箱路径穿越被净化", "../../etc/passwd", "", 3, "etcpasswd.json"},
		{"斜杠被净化", "a/b\\c@d.e", "", 4, "abc@d.e.json"},
		{"纯点退回 sub", "..", "sub-5", 5, "sub-5.json"},
		{"前导点被剥离", ".hidden@x.y", "", 6, "hidden@x.y.json"},
		{"邮箱净化后为空则退回 sub", "///", "sub-7", 7, "sub-7.json"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := grokExportFileName(c.email, c.sub, c.id); got != c.want {
				t.Fatalf("= %q, want %q", got, c.want)
			}
		})
	}
}

// TestGrokExportDownloadName 下载文件名沿用仓库既有约定
// codex2api-<平台>-<时间戳>-<数量>.<ext>。
func TestGrokExportDownloadName(t *testing.T) {
	single := grokExportDownloadName(1, "json")
	if !strings.HasPrefix(single, "codex2api-grok-") || !strings.HasSuffix(single, "-1.json") {
		t.Errorf("单账号下载名不符约定: %q", single)
	}
	archive := grokExportDownloadName(14, "zip")
	if !strings.HasPrefix(archive, "codex2api-grok-") || !strings.HasSuffix(archive, "-14.zip") {
		t.Errorf("多账号下载名不符约定: %q", archive)
	}
	// 时间戳部分必须是 20060102-150405 形态（8 位日期 + 6 位时间）。
	stamp := strings.TrimSuffix(strings.TrimPrefix(archive, "codex2api-grok-"), "-14.zip")
	if _, err := time.Parse("20060102-150405", stamp); err != nil {
		t.Errorf("时间戳部分 %q 不可解析: %v", stamp, err)
	}
}

// TestBuildGrokExportZIPMembersUseEmailNames ZIP 内部成员按 <邮箱>.json 命名，
// 下载物改名不影响成员命名——成员名承载账号身份，也是解开后逐个导入的依据。
func TestBuildGrokExportZIPMembersUseEmailNames(t *testing.T) {
	entry, ok := grokAccountRowToExportEntry(grokOAuthRow(), exportProxyResolver{})
	if !ok {
		t.Fatalf("导出失败")
	}
	archive, err := buildGrokExportZIP([]grokExportEntry{entry})
	if err != nil {
		t.Fatalf("打包失败: %v", err)
	}
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatalf("ZIP 不可读: %v", err)
	}
	want := "mashir0zhao@gmail.com.json"
	if reader.File[0].Name != want {
		t.Fatalf("ZIP 成员名 = %q, want %q", reader.File[0].Name, want)
	}
}

// TestBuildGrokExportZIPDedupesSharedEmail 多个 API Key 账号的 email 都是
// 占位串 xai-api-key，同名成员必须被追加序号而不是互相覆盖。
func TestBuildGrokExportZIPDedupesSharedEmail(t *testing.T) {
	rows := []*database.AccountRow{
		{ID: 1, Platform: "xai", Enabled: true, UpdatedAt: time.Now(),
			Credentials: map[string]interface{}{"api_key": "k1", "email": "xai-api-key"}},
		{ID: 2, Platform: "xai", Enabled: true, UpdatedAt: time.Now(),
			Credentials: map[string]interface{}{"api_key": "k2", "email": "xai-api-key"}},
		{ID: 3, Platform: "xai", Enabled: true, UpdatedAt: time.Now(),
			Credentials: map[string]interface{}{"api_key": "k3", "email": "xai-api-key"}},
	}
	entries := make([]grokExportEntry, 0, len(rows))
	for _, row := range rows {
		entry, ok := grokAccountRowToExportEntry(row, exportProxyResolver{})
		if !ok {
			t.Fatalf("账号 %d 导出失败", row.ID)
		}
		entries = append(entries, entry)
	}
	archive, err := buildGrokExportZIP(entries)
	if err != nil {
		t.Fatalf("打包失败: %v", err)
	}
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatalf("ZIP 不可读: %v", err)
	}
	if len(reader.File) != 3 {
		t.Fatalf("应有 3 个成员，实际 %d（同名被覆盖）", len(reader.File))
	}
	seen := map[string]bool{}
	for _, file := range reader.File {
		if seen[file.Name] {
			t.Fatalf("成员名重复: %s", file.Name)
		}
		seen[file.Name] = true
	}
	for _, want := range []string{"xai-api-key.json", "xai-api-key-2.json", "xai-api-key-3.json"} {
		if !seen[want] {
			t.Errorf("缺少成员 %s（实际: %v）", want, seen)
		}
	}
}

func TestBuildGrokExportZIP(t *testing.T) {
	entries := []grokExportEntry{
		{Type: "xai", Email: "a@x.y", exportFileName: "a@x.y.json"},
		{Type: "xai", Email: "b@x.y", exportFileName: "b@x.y.json"},
		// 同名条目：第二个应被追加序号，不能互相覆盖。
		{Type: "xai", Email: "a@x.y", exportFileName: "a@x.y.json"},
	}
	archive, err := buildGrokExportZIP(entries)
	if err != nil {
		t.Fatalf("打包失败: %v", err)
	}
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatalf("ZIP 不可读: %v", err)
	}
	if len(reader.File) != 3 {
		t.Fatalf("ZIP 内应有 3 个文件，实际 %d", len(reader.File))
	}
	names := make([]string, 0, 3)
	for _, file := range reader.File {
		names = append(names, file.Name)
		// 每个成员都必须是合法 JSON。
		rc, err := file.Open()
		if err != nil {
			t.Fatalf("打开 %s 失败: %v", file.Name, err)
		}
		data, _ := io.ReadAll(rc)
		_ = rc.Close()
		var decoded map[string]any
		if err := json.Unmarshal(data, &decoded); err != nil {
			t.Fatalf("%s 不是合法 JSON: %v", file.Name, err)
		}
		if decoded["type"] != "xai" {
			t.Errorf("%s 的 type = %v", file.Name, decoded["type"])
		}
	}
	joined := strings.Join(names, ",")
	for _, want := range []string{"a@x.y.json", "b@x.y.json", "a@x.y-2.json"} {
		if !strings.Contains(joined, want) {
			t.Errorf("ZIP 缺少 %s（实际: %s）", want, joined)
		}
	}
}

// TestAccountRowToExportEntryDispatchesByPlatform 通用导出端点原先把 Grok 账号
// 也标成 type:"codex" 且丢掉 Grok 字段，导出的文件回灌必然失败。
func TestAccountRowToExportEntryDispatchesByPlatform(t *testing.T) {
	grokEntry, ok := accountRowToExportEntry(grokOAuthRow(), exportProxyResolver{})
	if !ok {
		t.Fatalf("Grok 行应可导出")
	}
	typed, isGrok := grokEntry.(grokExportEntry)
	if !isGrok {
		t.Fatalf("Grok 行应产出 grokExportEntry，实际 %T", grokEntry)
	}
	if typed.Type != "xai" || typed.ClientID == "" {
		t.Errorf("Grok 条目字段不完整: %+v", typed)
	}

	codexRow := &database.AccountRow{
		ID:       1,
		Platform: "openai",
		Enabled:  true,
		Credentials: map[string]interface{}{
			"email":         "a@b.c",
			"access_token":  "at",
			"refresh_token": "rt",
		},
		UpdatedAt: time.Now(),
	}
	codexEntry, ok := accountRowToExportEntry(codexRow, exportProxyResolver{})
	if !ok {
		t.Fatalf("Codex 行应可导出")
	}
	if cpa, isCPA := codexEntry.(cpaExportEntry); !isCPA {
		t.Fatalf("Codex 行应产出 cpaExportEntry，实际 %T", codexEntry)
	} else if cpa.Type != "codex" {
		t.Errorf("Codex 条目 type = %q", cpa.Type)
	}
}

// TestIsGrokAccountRow 平台字段与凭据 upstream_type 任一命中即算 Grok 账号。
func TestIsGrokAccountRow(t *testing.T) {
	if !isGrokAccountRow(&database.AccountRow{Platform: "xai"}) {
		t.Errorf("platform=xai 应判为 Grok")
	}
	if !isGrokAccountRow(&database.AccountRow{Platform: "XAI"}) {
		t.Errorf("平台判定应忽略大小写")
	}
	if !isGrokAccountRow(&database.AccountRow{
		Credentials: map[string]interface{}{"upstream_type": auth.UpstreamGrok},
	}) {
		t.Errorf("仅凭 upstream_type 也应判为 Grok")
	}
	if isGrokAccountRow(&database.AccountRow{Platform: "openai"}) {
		t.Errorf("openai 不应判为 Grok")
	}
	if isGrokAccountRow(nil) {
		t.Errorf("nil 不应判为 Grok")
	}
}

// TestMarshalGrokExportEntryFormatting 排版对齐参考文件：2 空格缩进、键按字母序。
func TestMarshalGrokExportEntryFormatting(t *testing.T) {
	encoded, err := marshalGrokExportEntry(grokExportEntry{
		Type: "xai", AuthKind: "oauth", AuthMode: "oauth",
		Email: "a@b.c", AccessToken: "at", RefreshToken: "rt",
	})
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	lines := strings.Split(string(encoded), "\n")
	if len(lines) < 3 {
		t.Fatalf("应为多行缩进输出: %s", encoded)
	}
	if !strings.HasPrefix(lines[1], "  \"") {
		t.Errorf("应为 2 空格缩进，实际首字段行: %q", lines[1])
	}
	// 字母序：access_token 必须排在 auth_kind 之前。
	atIdx := strings.Index(string(encoded), `"access_token"`)
	akIdx := strings.Index(string(encoded), `"auth_kind"`)
	if atIdx < 0 || akIdx < 0 || atIdx > akIdx {
		t.Errorf("键未按字母序排列: %s", encoded)
	}
}
