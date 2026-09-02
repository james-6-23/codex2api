package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"

	"github.com/codex2api/auth"
)

func fingerprintAccount(t *testing.T, mode string) *auth.Account {
	t.Helper()
	return &auth.Account{DBID: 42, CodexFingerprintMode: mode}
}

// codexClientHeaders 模拟真实 Codex CLI 的下游请求头：带 x-codex- 前缀头（引擎特征）
// 和连字符形式的 session-id。
func codexClientHeaders(turnMetadata, sessionID string) http.Header {
	headers := http.Header{}
	if turnMetadata != "" {
		headers.Set("X-Codex-Turn-Metadata", turnMetadata)
	}
	if sessionID != "" {
		headers.Set("Session-Id", sessionID)
	}
	return headers
}

// codexUUIDUnixMilli 解出 v7 UUID 前 48 bit 的毫秒时间戳，用于断言时间戳位被正确写入。
func codexUUIDUnixMilli(t *testing.T, id string) int64 {
	t.Helper()
	u, err := uuid.Parse(id)
	if err != nil {
		t.Fatalf("uuid.Parse(%q) error = %v", id, err)
	}
	return int64(u[0])<<40 | int64(u[1])<<32 | int64(u[2])<<24 | int64(u[3])<<16 | int64(u[4])<<8 | int64(u[5])
}

// deriveStableCodexUUID 现在只服务 installation_id：真实客户端的 installation 标识是 v4。
func TestDeriveStableCodexUUIDIsDeterministicAndV4(t *testing.T) {
	first := deriveStableCodexUUID("seed-a")
	if second := deriveStableCodexUUID("seed-a"); first != second {
		t.Fatalf("deriveStableCodexUUID(%q) = %q then %q, want identical values", "seed-a", first, second)
	}
	if other := deriveStableCodexUUID("seed-b"); other == first {
		t.Fatalf("different seeds produced the same value %q", first)
	}

	parsed, err := uuid.Parse(first)
	if err != nil {
		t.Fatalf("uuid.Parse(%q) error = %v", first, err)
	}
	if parsed.Version() != 4 {
		t.Fatalf("UUID version = %d, want 4 (installation id must match the client's v4 identifier)", parsed.Version())
	}
	if parsed.Variant() != uuid.RFC4122 {
		t.Fatalf("UUID variant = %v, want RFC4122", parsed.Variant())
	}
}

// TestDeriveStableCodexUUIDv7 锁定 session/thread/window 的派生：真实客户端这三个标识
// 是 UUIDv7，收敛值必须同为 v7 且时间戳位被精确写入（否则按 UUID 排序会暴露，见 #536）。
func TestDeriveStableCodexUUIDv7(t *testing.T) {
	const ts int64 = 1786949015072 // 抓包量级的毫秒时间戳
	first := deriveStableCodexUUIDv7("seed-a", ts)
	if second := deriveStableCodexUUIDv7("seed-a", ts); first != second {
		t.Fatalf("deriveStableCodexUUIDv7(%q) = %q then %q, want identical values", "seed-a", first, second)
	}
	if other := deriveStableCodexUUIDv7("seed-b", ts); other == first {
		t.Fatalf("different seeds produced the same value %q", first)
	}

	parsed, err := uuid.Parse(first)
	if err != nil {
		t.Fatalf("uuid.Parse(%q) error = %v", first, err)
	}
	if parsed.Version() != 7 {
		t.Fatalf("UUID version = %d, want 7 (must match the client's v7 session/thread/window)", parsed.Version())
	}
	if parsed.Variant() != uuid.RFC4122 {
		t.Fatalf("UUID variant = %v, want RFC4122", parsed.Variant())
	}
	if got := codexUUIDUnixMilli(t, first); got != ts {
		t.Fatalf("timestamp bits = %d, want %d written into the leading 48 bits", got, ts)
	}
}

// TestConvergedIdentityUUIDVersionsAndTimestamp 走完整推导，断言 session/thread/window
// 是 v7 且时间戳锚定在账号加入时间的散布窗口内，而 installation 仍是 v4。
func TestConvergedIdentityUUIDVersionsAndTimestamp(t *testing.T) {
	const addedAtMilli int64 = 1780000000000
	account := &auth.Account{DBID: 42, CodexFingerprintMode: auth.CodexFingerprintModeSession}
	account.AddedAt = addedAtMilli * int64(time.Millisecond)

	ids := resolveCodexFingerprintIDs(account, codexClientHeaders("", "client-session"))
	if ids == nil {
		t.Fatal("session mode returned nil")
	}

	for name, id := range map[string]string{
		"session": ids.sessionID,
		"thread":  ids.threadID,
		"window":  strings.TrimSuffix(ids.windowID, ":0"),
	} {
		parsed, err := uuid.Parse(id)
		if err != nil {
			t.Fatalf("%s id %q not a valid uuid: %v", name, id, err)
		}
		if parsed.Version() != 7 {
			t.Fatalf("%s id version = %d, want 7", name, parsed.Version())
		}
		ms := codexUUIDUnixMilli(t, id)
		if ms < addedAtMilli || ms >= addedAtMilli+codexIdentitySpreadMilli {
			t.Fatalf("%s id timestamp = %d, want within [%d, %d) anchored to account added time",
				name, ms, addedAtMilli, addedAtMilli+codexIdentitySpreadMilli)
		}
	}

	// installation 真实是 v4，不受 v7 改动影响。
	installation, err := uuid.Parse(ids.installationID)
	if err != nil {
		t.Fatalf("installation id %q not a valid uuid: %v", ids.installationID, err)
	}
	if installation.Version() != 4 {
		t.Fatalf("installation id version = %d, want 4", installation.Version())
	}

	// session 与 thread 种子不同，时间戳位应随之散开而非全部撞在同一毫秒。
	if codexUUIDUnixMilli(t, ids.sessionID) == codexUUIDUnixMilli(t, ids.threadID) {
		t.Fatal("session and thread share the same v7 timestamp, want the per-seed spread to separate them")
	}
}

func TestResolveCodexFingerprintIDsPerMode(t *testing.T) {
	headers := codexClientHeaders("", "client-session-1")

	if ids := resolveCodexFingerprintIDs(fingerprintAccount(t, auth.CodexFingerprintModeOff), headers); ids != nil {
		t.Fatalf("off mode returned %#v, want nil", ids)
	}
	if ids := resolveCodexFingerprintIDs(nil, headers); ids != nil {
		t.Fatalf("nil account returned %#v, want nil", ids)
	}

	device := resolveCodexFingerprintIDs(fingerprintAccount(t, auth.CodexFingerprintModeDevice), headers)
	if device == nil || device.installationID == "" {
		t.Fatalf("device mode ids = %#v, want a converged installation id", device)
	}
	if device.sessionID != "" || device.threadID != "" || device.windowID != "" {
		t.Fatalf("device mode converged session/thread/window (%#v), want installation id only", device)
	}

	session := resolveCodexFingerprintIDs(fingerprintAccount(t, auth.CodexFingerprintModeSession), headers)
	if session == nil {
		t.Fatal("session mode returned nil")
	}
	if session.installationID != device.installationID {
		t.Fatalf("installation id = %q, want %q (must not depend on mode)", session.installationID, device.installationID)
	}
	if session.sessionID == "" || session.threadID == "" {
		t.Fatalf("session mode ids = %#v, want session and thread ids", session)
	}
	if session.threadID == session.sessionID {
		t.Fatal("session mode thread id equals session id, want a thread derived from the client session")
	}
	if want := session.threadID + ":0"; session.windowID != want {
		t.Fatalf("window id = %q, want %q", session.windowID, want)
	}

	// 不同客户端会话派生出不同线程，同一客户端会话恒定派生同一线程。
	other := resolveCodexFingerprintIDs(fingerprintAccount(t, auth.CodexFingerprintModeSession), codexClientHeaders("", "client-session-2"))
	if other.threadID == session.threadID {
		t.Fatal("distinct client sessions derived the same thread id")
	}
	if other.sessionID != session.sessionID {
		t.Fatalf("session id = %q, want %q (account level constant)", other.sessionID, session.sessionID)
	}
	repeat := resolveCodexFingerprintIDs(fingerprintAccount(t, auth.CodexFingerprintModeSession), headers)
	if repeat.threadID != session.threadID {
		t.Fatalf("thread id = %q, want %q (same client session must be stable)", repeat.threadID, session.threadID)
	}

	full := resolveCodexFingerprintIDs(fingerprintAccount(t, auth.CodexFingerprintModeFull), headers)
	if full.threadID != full.sessionID {
		t.Fatalf("full mode thread id = %q, want it to equal session id %q", full.threadID, full.sessionID)
	}

	// 客户端没发会话头时，session 档退回单线程而不是每请求漂移。
	noSession := resolveCodexFingerprintIDs(fingerprintAccount(t, auth.CodexFingerprintModeSession), http.Header{})
	if noSession.threadID != noSession.sessionID {
		t.Fatalf("thread id = %q, want fallback to session id %q", noSession.threadID, noSession.sessionID)
	}
}

func TestResolveCodexFingerprintIDsSessionModeKeepsDistinctClientThreads(t *testing.T) {
	account := fingerprintAccount(t, auth.CodexFingerprintModeSession)
	parent := resolveCodexFingerprintIDs(account, codexClientHeaders(
		`{"session_id":"client-session","thread_id":"client-session"}`, "client-session"))
	spawn := resolveCodexFingerprintIDs(account, func() http.Header {
		headers := codexClientHeaders(
			`{"session_id":"client-session","thread_id":"client-thread"}`, "client-session")
		headers.Set("Thread-Id", "client-thread")
		return headers
	}())
	if parent == nil || spawn == nil {
		t.Fatal("session mode returned nil")
	}
	if spawn.sessionID != parent.sessionID {
		t.Fatalf("spawn session id = %q, want parent session %q", spawn.sessionID, parent.sessionID)
	}
	if spawn.threadID == parent.threadID {
		t.Fatal("spawn thread collapsed onto the parent session thread")
	}
	if spawn.threadID == spawn.sessionID {
		t.Fatal("spawn thread id equals session id, want a thread derived from the client thread")
	}

	repeat := resolveCodexFingerprintIDs(account, func() http.Header {
		headers := codexClientHeaders(
			`{"session_id":"client-session","thread_id":"client-thread"}`, "client-session")
		headers.Set("Thread-Id", "client-thread")
		return headers
	}())
	if repeat.threadID != spawn.threadID {
		t.Fatalf("thread id = %q, want %q (same client thread must be stable)", repeat.threadID, spawn.threadID)
	}
}

func TestResolveCodexFingerprintIDsIsolatesAccounts(t *testing.T) {
	headers := codexClientHeaders("", "client-session-1")
	first := resolveCodexFingerprintIDs(&auth.Account{DBID: 1, CodexFingerprintMode: auth.CodexFingerprintModeSession}, headers)
	second := resolveCodexFingerprintIDs(&auth.Account{DBID: 2, CodexFingerprintMode: auth.CodexFingerprintModeSession}, headers)
	if first.installationID == second.installationID {
		t.Fatal("distinct accounts share an installation id")
	}
	if first.sessionID == second.sessionID {
		t.Fatal("distinct accounts share a session id")
	}
	if first.threadID == second.threadID {
		t.Fatal("distinct accounts share a thread id")
	}
}

// TestResolveCodexFingerprintIDsHonorsPinnedInstallationHeader 验证运维在账号自定义头里
// 钉死的 installation id 优先于派生值——自定义头本来就会覆盖出站头，取同一值才能让
// 请求头与 metadata 一致。
func TestResolveCodexFingerprintIDsHonorsPinnedInstallationHeader(t *testing.T) {
	account := &auth.Account{
		DBID:                 42,
		CodexFingerprintMode: auth.CodexFingerprintModeDevice,
		CustomHeaders:        map[string]string{"x-codex-installation-id": "pinned-device-id"},
	}
	ids := resolveCodexFingerprintIDs(account, http.Header{})
	if ids.installationID != "pinned-device-id" {
		t.Fatalf("installation id = %q, want the pinned custom header value", ids.installationID)
	}
}

func TestApplyCodexFingerprintHeadersRewritesOnlyPresentTurnMetadataKeys(t *testing.T) {
	// sandbox / thread_source 等非身份字段必须原样保留，缺失的 window_id 不能被补上。
	const raw = `{"thread_id":"client-thread","installation_id":"client-install","sandbox":"danger-full-access","session_id":"client-session","turn_id":"client-turn","turn_started_at_unix_ms":1730000000000}`
	outbound := http.Header{}
	outbound.Set("X-Codex-Turn-Metadata", raw)
	account := fingerprintAccount(t, auth.CodexFingerprintModeSession)
	downstream := codexClientHeaders(raw, "client-session")

	ApplyCodexFingerprintHeaders(outbound, account, downstream)

	got := outbound.Get("X-Codex-Turn-Metadata")
	ids := resolveCodexFingerprintIDs(account, downstream)
	if value := gjson.Get(got, "installation_id").String(); value != ids.installationID {
		t.Fatalf("installation_id = %q, want %q", value, ids.installationID)
	}
	if value := gjson.Get(got, "session_id").String(); value != ids.sessionID {
		t.Fatalf("session_id = %q, want %q", value, ids.sessionID)
	}
	if value := gjson.Get(got, "thread_id").String(); value != ids.threadID {
		t.Fatalf("thread_id = %q, want %q", value, ids.threadID)
	}
	if gjson.Get(got, "window_id").Exists() {
		t.Fatalf("window_id was added to %q, want absent keys left absent", got)
	}
	if value := gjson.Get(got, "sandbox").String(); value != "danger-full-access" {
		t.Fatalf("sandbox = %q, want it preserved", value)
	}
	// turn_id 与其时间戳是逐轮随机值，不标识设备；重写只会引入头体不一致。
	if value := gjson.Get(got, "turn_id").String(); value != "client-turn" {
		t.Fatalf("turn_id = %q, want the client value preserved", value)
	}
	if value := gjson.Get(got, "turn_started_at_unix_ms").Int(); value != 1730000000000 {
		t.Fatalf("turn_started_at_unix_ms = %d, want the client value preserved", value)
	}
	// 键序必须保持原样：重排本身就是可识别特征。
	if wantPrefix := `{"thread_id":"`; !strings.HasPrefix(got, wantPrefix) {
		t.Fatalf("rewritten metadata = %q, want original key order starting with %q", got, wantPrefix)
	}
}

func TestApplyCodexFingerprintHeadersDeviceModeLeavesSessionFieldsAlone(t *testing.T) {
	const raw = `{"installation_id":"client-install","session_id":"client-session","thread_id":"client-thread"}`
	outbound := http.Header{}
	outbound.Set("X-Codex-Turn-Metadata", raw)

	ApplyCodexFingerprintHeaders(outbound, fingerprintAccount(t, auth.CodexFingerprintModeDevice), codexClientHeaders(raw, "client-session"))

	got := outbound.Get("X-Codex-Turn-Metadata")
	if value := gjson.Get(got, "installation_id").String(); value == "client-install" {
		t.Fatal("installation_id was not converged in device mode")
	}
	if value := gjson.Get(got, "session_id").String(); value != "client-session" {
		t.Fatalf("session_id = %q, want the client value untouched in device mode", value)
	}
	if value := gjson.Get(got, "thread_id").String(); value != "client-thread" {
		t.Fatalf("thread_id = %q, want the client value untouched in device mode", value)
	}
	if outbound.Get("X-Codex-Window-Id") != "" {
		t.Fatal("device mode set a window id header")
	}
}

// TestApplyCodexFingerprintHeadersNeverTouchesSessionIDHeader 锁定与既有上游会话
// 隔离逻辑的边界：Session_id 由 resolveUpstreamSessionID 决定，收敛不得介入，
// 否则 prompt cache 隔离语义会被改变。
func TestApplyCodexFingerprintHeadersNeverTouchesSessionIDHeader(t *testing.T) {
	outbound := http.Header{}
	outbound.Set("Session_id", "upstream-isolated-id")
	outbound.Set("X-Codex-Turn-Metadata", `{"session_id":"client-session"}`)

	ApplyCodexFingerprintHeaders(outbound, fingerprintAccount(t, auth.CodexFingerprintModeFull), codexClientHeaders(`{"session_id":"client-session"}`, "client-session"))

	if got := outbound.Get("Session_id"); got != "upstream-isolated-id" {
		t.Fatalf("Session_id = %q, want it left to resolveUpstreamSessionID", got)
	}
	if got := outbound.Get("Thread-Id"); got != "" {
		t.Fatalf("Thread-Id = %q, want unset (not part of this project's outbound header set)", got)
	}
}

// TestApplyCodexFingerprintHeadersSkipsNonCodexClients 验证不给没有 Codex 引擎特征的
// 下游请求伪造 x-codex-* 头——半套特征比不发更可疑。
func TestApplyCodexFingerprintHeadersSkipsNonCodexClients(t *testing.T) {
	outbound := http.Header{}
	plainSDKHeaders := http.Header{"User-Agent": []string{"OpenAI/Python 1.0"}}

	ApplyCodexFingerprintHeaders(outbound, fingerprintAccount(t, auth.CodexFingerprintModeSession), plainSDKHeaders)

	if got := outbound.Get("X-Codex-Installation-Id"); got != "" {
		t.Fatalf("X-Codex-Installation-Id = %q, want unset for a non-Codex downstream", got)
	}
	if got := outbound.Get("X-Codex-Window-Id"); got != "" {
		t.Fatalf("X-Codex-Window-Id = %q, want unset for a non-Codex downstream", got)
	}
}

// TestApplyCodexFingerprintHeadersNeverAddsAbsentIdentityHeaders 锁定「只覆盖、不新增」
// 对请求头同样成立。实测真实 Codex CLI 只发 x-codex-window-id，从不发
// x-codex-installation-id（该值只存在于 turn metadata JSON 里）——凭空补发等于给请求
// 添一个真实客户端不会有的头。
func TestApplyCodexFingerprintHeadersNeverAddsAbsentIdentityHeaders(t *testing.T) {
	outbound := http.Header{}
	account := fingerprintAccount(t, auth.CodexFingerprintModeSession)
	downstream := codexClientHeaders(`{"installation_id":"client-install"}`, "client-session")
	outbound.Set("X-Codex-Turn-Metadata", downstream.Get("X-Codex-Turn-Metadata"))

	ApplyCodexFingerprintHeaders(outbound, account, downstream)

	if got := outbound.Get("X-Codex-Installation-Id"); got != "" {
		t.Fatalf("X-Codex-Installation-Id = %q, want unset (downstream never sent it)", got)
	}
	if got := outbound.Get("X-Codex-Window-Id"); got != "" {
		t.Fatalf("X-Codex-Window-Id = %q, want unset (downstream never sent it)", got)
	}
	// metadata 内部的 installation_id 仍应收敛——收敛面本来就在 JSON 里。
	if got := gjson.Get(outbound.Get("X-Codex-Turn-Metadata"), "installation_id").String(); got == "client-install" {
		t.Fatal("metadata installation_id was left at the client value")
	}
}

// TestApplyCodexFingerprintHeadersOverridesPresentIdentityHeaders 验证下游确实发过时，
// 出站取值被收敛值覆盖（真实 CLI 会发 window id，这条是它的主路径）。
func TestApplyCodexFingerprintHeadersOverridesPresentIdentityHeaders(t *testing.T) {
	outbound := http.Header{}
	account := fingerprintAccount(t, auth.CodexFingerprintModeSession)
	downstream := codexClientHeaders(`{"installation_id":"client-install"}`, "client-session")
	downstream.Set("X-Codex-Window-Id", "client-window:0")
	downstream.Set("X-Codex-Installation-Id", "client-install")

	ApplyCodexFingerprintHeaders(outbound, account, downstream)

	ids := resolveCodexFingerprintIDs(account, downstream)
	if got := outbound.Get("X-Codex-Installation-Id"); got != ids.installationID {
		t.Fatalf("X-Codex-Installation-Id = %q, want %q", got, ids.installationID)
	}
	if got := outbound.Get("X-Codex-Window-Id"); got != ids.windowID {
		t.Fatalf("X-Codex-Window-Id = %q, want %q", got, ids.windowID)
	}
}

func TestApplyCodexFingerprintOffModeIsNoop(t *testing.T) {
	const raw = `{"installation_id":"client-install","session_id":"client-session"}`
	outbound := http.Header{}
	outbound.Set("X-Codex-Turn-Metadata", raw)
	account := fingerprintAccount(t, auth.CodexFingerprintModeOff)
	downstream := codexClientHeaders(raw, "client-session")

	ApplyCodexFingerprintHeaders(outbound, account, downstream)
	if got := outbound.Get("X-Codex-Turn-Metadata"); got != raw {
		t.Fatalf("turn metadata = %q, want %q unchanged", got, raw)
	}
	if got := outbound.Get("X-Codex-Installation-Id"); got != "" {
		t.Fatalf("X-Codex-Installation-Id = %q, want unset", got)
	}

	body := []byte(`{"client_metadata":{"x-codex-installation-id":"client-install"}}`)
	if got := ApplyCodexFingerprintToBody(body, account, downstream); string(got) != string(body) {
		t.Fatalf("body = %s, want %s unchanged", got, body)
	}
}

func TestApplyCodexFingerprintToBodyRewritesClientMetadata(t *testing.T) {
	account := fingerprintAccount(t, auth.CodexFingerprintModeSession)
	downstream := codexClientHeaders("", "client-session")
	ids := resolveCodexFingerprintIDs(account, downstream)
	body := []byte(`{"model":"gpt-5.6-codex","client_metadata":{"x-codex-installation-id":"client-install","session_id":"client-session","thread_id":"client-thread","x-codex-window-id":"client-window:0","x-codex-turn-metadata":"{\"installation_id\":\"client-install\",\"session_id\":\"client-session\",\"sandbox\":\"read-only\"}"}}`)

	got := ApplyCodexFingerprintToBody(body, account, downstream)

	if value := gjson.GetBytes(got, "client_metadata.x-codex-installation-id").String(); value != ids.installationID {
		t.Fatalf("client_metadata.x-codex-installation-id = %q, want %q", value, ids.installationID)
	}
	if value := gjson.GetBytes(got, "client_metadata.session_id").String(); value != ids.sessionID {
		t.Fatalf("client_metadata.session_id = %q, want %q", value, ids.sessionID)
	}
	if value := gjson.GetBytes(got, "client_metadata.thread_id").String(); value != ids.threadID {
		t.Fatalf("client_metadata.thread_id = %q, want %q", value, ids.threadID)
	}
	if value := gjson.GetBytes(got, "client_metadata.x-codex-window-id").String(); value != ids.windowID {
		t.Fatalf("client_metadata.x-codex-window-id = %q, want %q", value, ids.windowID)
	}
	if value := gjson.GetBytes(got, "model").String(); value != "gpt-5.6-codex" {
		t.Fatalf("model = %q, want it preserved", value)
	}

	embedded := gjson.GetBytes(got, "client_metadata.x-codex-turn-metadata").String()
	if value := gjson.Get(embedded, "installation_id").String(); value != ids.installationID {
		t.Fatalf("embedded installation_id = %q, want %q", value, ids.installationID)
	}
	if value := gjson.Get(embedded, "session_id").String(); value != ids.sessionID {
		t.Fatalf("embedded session_id = %q, want %q", value, ids.sessionID)
	}
	if value := gjson.Get(embedded, "sandbox").String(); value != "read-only" {
		t.Fatalf("embedded sandbox = %q, want it preserved", value)
	}
}

func TestApplyCodexFingerprintToBodyLeavesBodiesWithoutClientMetadata(t *testing.T) {
	account := fingerprintAccount(t, auth.CodexFingerprintModeFull)
	downstream := codexClientHeaders("", "client-session")

	body := []byte(`{"model":"gpt-5.6-codex","input":[]}`)
	if got := ApplyCodexFingerprintToBody(body, account, downstream); string(got) != string(body) {
		t.Fatalf("body = %s, want %s unchanged", got, body)
	}

	// client_metadata 存在但不含身份字段时不得新增字段。
	partial := []byte(`{"client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":"true"}}`)
	if got := ApplyCodexFingerprintToBody(partial, account, downstream); string(got) != string(partial) {
		t.Fatalf("body = %s, want %s unchanged", got, partial)
	}
}

// TestCodexFingerprintHeaderAndBodyAgree 是这套改写的核心不变量：请求头与请求体在
// 两个独立改写点各自推导，必须落到完全相同的一组标识。
func TestCodexFingerprintHeaderAndBodyAgree(t *testing.T) {
	account := fingerprintAccount(t, auth.CodexFingerprintModeSession)
	const raw = `{"installation_id":"client-install","session_id":"client-session","thread_id":"client-thread","window_id":"client-window:0"}`
	downstream := codexClientHeaders(raw, "client-session")
	// 标识头只在下游发过时才被覆盖，这里补上以便断言头与 metadata 的一致性。
	downstream.Set("X-Codex-Installation-Id", "client-install")
	downstream.Set("X-Codex-Window-Id", "client-window:0")

	outbound := http.Header{}
	outbound.Set("X-Codex-Turn-Metadata", raw)
	ApplyCodexFingerprintHeaders(outbound, account, downstream)

	body := ApplyCodexFingerprintToBody(
		[]byte(`{"client_metadata":{"x-codex-installation-id":"client-install","session_id":"client-session","thread_id":"client-thread","x-codex-window-id":"client-window:0"}}`),
		account, downstream,
	)

	headerMetadata := outbound.Get("X-Codex-Turn-Metadata")
	pairs := []struct {
		name       string
		headerPath string
		bodyPath   string
	}{
		{"installation id", "installation_id", "client_metadata.x-codex-installation-id"},
		{"session id", "session_id", "client_metadata.session_id"},
		{"thread id", "thread_id", "client_metadata.thread_id"},
		{"window id", "window_id", "client_metadata.x-codex-window-id"},
	}
	for _, pair := range pairs {
		fromHeader := gjson.Get(headerMetadata, pair.headerPath).String()
		fromBody := gjson.GetBytes(body, pair.bodyPath).String()
		if fromHeader == "" {
			t.Fatalf("%s missing from rewritten header metadata %q", pair.name, headerMetadata)
		}
		if fromHeader != fromBody {
			t.Fatalf("%s: header = %q, body = %q, want identical values", pair.name, fromHeader, fromBody)
		}
	}
	if got := outbound.Get("X-Codex-Installation-Id"); got != gjson.Get(headerMetadata, "installation_id").String() {
		t.Fatalf("X-Codex-Installation-Id = %q, want it to match the metadata installation id", got)
	}
	if got := outbound.Get("X-Codex-Window-Id"); got != gjson.Get(headerMetadata, "window_id").String() {
		t.Fatalf("X-Codex-Window-Id = %q, want it to match the metadata window id", got)
	}
}

// TestApplyCodexFingerprintHeadersIgnoresInvalidTurnMetadata 验证客户端发来的非法 JSON
// 原样透传，不因改写失败而破坏请求。
func TestApplyCodexFingerprintHeadersIgnoresInvalidTurnMetadata(t *testing.T) {
	outbound := http.Header{}
	outbound.Set("X-Codex-Turn-Metadata", "not-json")

	ApplyCodexFingerprintHeaders(outbound, fingerprintAccount(t, auth.CodexFingerprintModeSession), codexClientHeaders("not-json", "client-session"))

	if got := outbound.Get("X-Codex-Turn-Metadata"); got != "not-json" {
		t.Fatalf("turn metadata = %q, want it passed through unchanged", got)
	}
}

// TestApplyCodexFingerprintSkipsRelayAccounts 验证中转型账号不走 Codex 官方出站路径，
// 恒为 off，即使凭据里配了收敛档位。
func TestApplyCodexFingerprintSkipsRelayAccounts(t *testing.T) {
	relay := &auth.Account{
		DBID:                 42,
		UpstreamType:         auth.UpstreamOpenAIResponses,
		BaseURL:              "https://relay.example.com",
		APIKey:               "sk-relay",
		CodexFingerprintMode: auth.CodexFingerprintModeFull,
	}
	if ids := resolveCodexFingerprintIDs(relay, codexClientHeaders("", "client-session")); ids != nil {
		t.Fatalf("relay account ids = %#v, want nil", ids)
	}

	grok := &auth.Account{
		DBID:                 42,
		UpstreamType:         auth.UpstreamGrok,
		APIKey:               "xai-key",
		CodexFingerprintMode: auth.CodexFingerprintModeFull,
	}
	if ids := resolveCodexFingerprintIDs(grok, codexClientHeaders("", "client-session")); ids != nil {
		t.Fatalf("grok account ids = %#v, want nil", ids)
	}

	// 即使下游带着完整的 Codex 特征头打到 Grok 账号上，也不得改写任何载体。
	const raw = `{"installation_id":"client-install","session_id":"client-session"}`
	outbound := http.Header{}
	outbound.Set("X-Codex-Turn-Metadata", raw)
	downstream := codexClientHeaders(raw, "client-session")
	ApplyCodexFingerprintHeaders(outbound, grok, downstream)
	if got := outbound.Get("X-Codex-Turn-Metadata"); got != raw {
		t.Fatalf("grok turn metadata = %q, want %q unchanged", got, raw)
	}
	if got := outbound.Get("X-Codex-Installation-Id"); got != "" {
		t.Fatalf("grok X-Codex-Installation-Id = %q, want unset", got)
	}
	body := []byte(`{"client_metadata":{"x-codex-installation-id":"client-install"}}`)
	if got := ApplyCodexFingerprintToBody(body, grok, downstream); string(got) != string(body) {
		t.Fatalf("grok body = %s, want %s unchanged", got, body)
	}
}

// compact 路径此前只收敛请求头、请求体 client_metadata 原样发出，上游会看到
// 「头说设备 A、体说设备 B」这种真实客户端不会有的矛盾（也让收敛形同虚设）。
func TestExecuteCompactRequestConvergesBodyAndHeaders(t *testing.T) {
	account := fingerprintAccount(t, auth.CodexFingerprintModeSession)
	account.AccessToken = "token-compact"
	// 带 turn metadata 才算真 Codex 客户端（见 ApplyCodexFingerprintHeaders 的
	// EvaluateEngineFingerprint 门）；标识头还需下游确实发过才会被覆盖。
	downstream := codexClientHeaders(`{"installation_id":"client-install","session_id":"client-session"}`, "client-session")
	downstream.Set("X-Codex-Installation-Id", "client-install")
	ids := resolveCodexFingerprintIDs(account, downstream)
	if ids == nil {
		t.Fatal("expected convergence ids for session mode")
	}

	var capturedBody []byte
	var capturedHeader http.Header
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody = readUpstreamRequestBody(r)
		capturedHeader = r.Header.Clone()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	// 经 Resin 反代把出站请求引到本地服务器，从而断言真实发出的字节。
	previousResin := resinCfg.Load()
	t.Cleanup(func() { resinCfg.Store(previousResin) })
	SetResinConfig(&ResinConfig{BaseURL: server.URL, PlatformName: "test"})
	clientPool.Delete(fmt.Sprintf("resin|%d", account.ID()))

	body := []byte(`{"model":"gpt-5.6-codex","client_metadata":{"x-codex-installation-id":"client-install","session_id":"client-session","thread_id":"client-thread","x-codex-window-id":"client-window:0"}}`)
	resp, err := ExecuteCompactRequest(context.Background(), account, body, "", "", "api-key-1", nil, downstream)
	if err != nil {
		t.Fatalf("ExecuteCompactRequest: %v", err)
	}
	_ = resp.Body.Close()

	for _, tc := range []struct{ path, want string }{
		{"client_metadata.x-codex-installation-id", ids.installationID},
		{"client_metadata.session_id", ids.sessionID},
		{"client_metadata.thread_id", ids.threadID},
		{"client_metadata.x-codex-window-id", ids.windowID},
	} {
		if got := gjson.GetBytes(capturedBody, tc.path).String(); got != tc.want {
			t.Fatalf("outbound %s = %q, want converged %q", tc.path, got, tc.want)
		}
	}
	// 头与体必须来自同一份推导，否则矛盾本身就是特征。
	if got := capturedHeader.Get(codexInstallationIDHeader); got != ids.installationID {
		t.Fatalf("outbound %s = %q, want %q", codexInstallationIDHeader, got, ids.installationID)
	}
	if got := gjson.GetBytes(capturedBody, "client_metadata.x-codex-installation-id").String(); got != capturedHeader.Get(codexInstallationIDHeader) {
		t.Fatalf("header/body installation id disagree: body=%q header=%q", got, capturedHeader.Get(codexInstallationIDHeader))
	}
}

// TestConvergedClientRequestIDFollowsClientIdentity 覆盖 issue #536 的主诉：
// 实测真实客户端把 X-Client-Request-Id 取成与自身 thread_id 相同的值，收敛必须跟上，
// 否则原始线程标识会绕过 metadata 改写直达上游。
func TestConvergedClientRequestIDFollowsClientIdentity(t *testing.T) {
	account := fingerprintAccount(t, auth.CodexFingerprintModeSession)

	t.Run("matches thread id", func(t *testing.T) {
		const raw = `{"session_id":"client-session","thread_id":"client-thread"}`
		downstream := codexClientHeaders(raw, "client-session")
		downstream.Set("Thread-Id", "client-thread")
		outbound := http.Header{}
		outbound.Set("X-Client-Request-Id", "client-thread") // 白名单原样透传的结果
		outbound.Set("X-Codex-Turn-Metadata", raw)

		ApplyCodexFingerprintHeaders(outbound, account, downstream)

		ids := resolveCodexFingerprintIDs(account, downstream)
		if got := outbound.Get("X-Client-Request-Id"); got != ids.threadID {
			t.Fatalf("X-Client-Request-Id = %q, want converged thread id %q", got, ids.threadID)
		}
		parent := resolveCodexFingerprintIDs(account, codexClientHeaders(
			`{"session_id":"client-session","thread_id":"client-session"}`, "client-session"))
		if got := outbound.Get("X-Client-Request-Id"); got == parent.threadID {
			t.Fatal("X-Client-Request-Id collapsed onto the parent session thread")
		}
	})

	t.Run("matches session id", func(t *testing.T) {
		downstream := codexClientHeaders("", "client-session")
		outbound := http.Header{}
		outbound.Set("X-Client-Request-Id", "client-session")

		ApplyCodexFingerprintHeaders(outbound, account, downstream)

		ids := resolveCodexFingerprintIDs(account, downstream)
		if got := outbound.Get("X-Client-Request-Id"); got != ids.sessionID {
			t.Fatalf("X-Client-Request-Id = %q, want converged session id %q", got, ids.sessionID)
		}
	})

	// 与任何自报身份都对不上时说明它确实是每请求随机值，改写它反而制造不一致。
	t.Run("leaves unrelated values alone", func(t *testing.T) {
		downstream := codexClientHeaders("", "client-session")
		outbound := http.Header{}
		outbound.Set("X-Client-Request-Id", "per-request-random-value")

		ApplyCodexFingerprintHeaders(outbound, account, downstream)

		if got := outbound.Get("X-Client-Request-Id"); got != "per-request-random-value" {
			t.Fatalf("X-Client-Request-Id = %q, want the client value preserved", got)
		}
	})

	// device 档不收敛会话级标识，也就不该动这个头。
	t.Run("device mode is a noop", func(t *testing.T) {
		downstream := codexClientHeaders("", "client-session")
		outbound := http.Header{}
		outbound.Set("X-Client-Request-Id", "client-session")

		ApplyCodexFingerprintHeaders(outbound, fingerprintAccount(t, auth.CodexFingerprintModeDevice), downstream)

		if got := outbound.Get("X-Client-Request-Id"); got != "client-session" {
			t.Fatalf("X-Client-Request-Id = %q, want untouched in device mode", got)
		}
	})
}

// TestScrubCodexWorkspacesRemovesWorkspaceIdentity 验证工作区身份被抹掉：本地绝对
// 路径含系统用户名、associated_remote_urls 含仓库归属，多人共享账号时这些会把每个
// 下游用户的身份原样送到上游。
func TestScrubCodexWorkspacesRemovesWorkspaceIdentity(t *testing.T) {
	// commit hash 用真实长度的值：它是全局唯一的仓库标记，公开仓库可反查到归属，
	// 与 remote URL 泄漏的是同一份身份。
	const originalCommit = "3cd12a685fe3ea23b84a9097fd4563927857ea21"
	const raw = `{"request_kind":"memory","workspaces":{"/Users/alice/code/secret-project":{"associated_remote_urls":{"origin":"https://github.com/alice-corp/secret-project.git"},"latest_git_commit_hash":"` + originalCommit + `","has_changes":false}}}`

	got, changed := scrubCodexWorkspaces(raw, 42)
	if !changed {
		t.Fatal("scrubCodexWorkspaces reported no change, want the workspace scrubbed")
	}
	for _, leaked := range []string{"alice", "secret-project", "github.com", originalCommit} {
		if strings.Contains(got, leaked) {
			t.Fatalf("scrubbed metadata %q still contains %q", got, leaked)
		}
	}

	workspaces := gjson.Get(got, "workspaces")
	if n := len(workspaces.Map()); n != 1 {
		t.Fatalf("workspace entry count = %d, want 1 (shape must be preserved)", n)
	}
	for path, entry := range workspaces.Map() {
		if !strings.HasPrefix(path, "/workspace/") {
			t.Fatalf("workspace key = %q, want a derived placeholder path", path)
		}
		if entry.Get("associated_remote_urls").Exists() {
			t.Fatalf("associated_remote_urls survived in %q", entry.Raw)
		}
		// 形状保持：字段仍在，且仍是等长的十六进制串。
		if replaced := entry.Get("latest_git_commit_hash").String(); len(replaced) != len(originalCommit) {
			t.Fatalf("latest_git_commit_hash = %q, want a same-length derived placeholder", replaced)
		}
		if entry.Get("has_changes").Bool() {
			t.Fatal("has_changes = true, want the client value preserved")
		}
	}

	// 同一路径恒定映射到同一占位符，不同路径互不相同。
	repeat, _ := scrubCodexWorkspaces(raw, 42)
	if repeat != got {
		t.Fatalf("scrub is not deterministic: %q vs %q", repeat, got)
	}
	if placeholderWorkspacePath(42, "/Users/alice/a") == placeholderWorkspacePath(42, "/Users/alice/b") {
		t.Fatal("distinct workspace paths collapsed to the same placeholder")
	}

	// 账号维度隔离：同一路径在不同账号下必须派生出不同占位符，否则上游可以把
	// 同一个下游用户跨账号关联起来。
	if placeholderWorkspacePath(42, "/Users/alice/a") == placeholderWorkspacePath(43, "/Users/alice/a") {
		t.Fatal("same path produced the same placeholder across accounts")
	}
	otherAccount, _ := scrubCodexWorkspaces(raw, 43)
	if otherAccount == got {
		t.Fatal("scrub result is identical across accounts, want account-scoped derivation")
	}

	// 抹除必须幂等：重试链路可能把改写过的载荷再送进来一次。
	if again, changedAgain := scrubCodexWorkspaces(got, 42); changedAgain || again != got {
		t.Fatalf("second scrub changed the payload again: %q -> %q", got, again)
	}

	// 没有 workspaces 的 metadata 不受影响。
	if _, changed := scrubCodexWorkspaces(`{"session_id":"s"}`, 42); changed {
		t.Fatal("metadata without workspaces was modified")
	}
}

// TestCodexFingerprintLeavesNoOriginalIdentifierOutbound 是 issue #536 要求的一致性
// 回归：用真实抓包的请求形态跑完整条改写链，断言出站头里不再残留任何一个原始标识。
// 将来协议新增身份字段导致遗漏时，这条会先失败。
func TestCodexFingerprintLeavesNoOriginalIdentifierOutbound(t *testing.T) {
	const (
		clientSessionUUID = "01a00e75-8856-7542-89bf-35812620690f"
		clientInstallUUID = "341596ee-ab98-43f8-82e2-08ecdfb56db4"
		workspacePath     = "/Users/kyx/code_project/codex2api"
		remoteURL         = "https://github.com/james-6-23/codex2api.git"
		commitHash        = "3cd12a685fe3ea23b84a9097fd4563927857ea21"
	)
	rawMetadata := `{"installation_id":"` + clientInstallUUID + `","session_id":"` + clientSessionUUID +
		`","thread_id":"` + clientSessionUUID + `","turn_id":"01a00e76-15fb-7940-91a0-e201c45f502a","window_id":"` +
		clientSessionUUID + `:0","request_kind":"turn","sandbox":"none","workspaces":{"` + workspacePath +
		`":{"associated_remote_urls":{"origin":"` + remoteURL + `"},"latest_git_commit_hash":"` + commitHash + `"}},"turn_started_at_unix_ms":1786949015072}`

	// 复刻真实 CLI 的下游头集合。
	downstream := http.Header{}
	downstream.Set("X-Codex-Turn-Metadata", rawMetadata)
	downstream.Set("Session-Id", clientSessionUUID)
	downstream.Set("Thread-Id", clientSessionUUID)
	downstream.Set("X-Client-Request-Id", clientSessionUUID)
	downstream.Set("X-Codex-Window-Id", clientSessionUUID+":0")

	// 复刻网关的白名单透传结果（见 codexAllowedForwardHeaders）。
	outbound := http.Header{}
	outbound.Set("X-Codex-Turn-Metadata", rawMetadata)
	outbound.Set("X-Client-Request-Id", clientSessionUUID)

	account := fingerprintAccount(t, auth.CodexFingerprintModeSession)
	ApplyCodexFingerprintHeaders(outbound, account, downstream)
	body := ApplyCodexFingerprintToBody(
		[]byte(`{"client_metadata":{"x-codex-installation-id":"`+clientInstallUUID+`","session_id":"`+clientSessionUUID+
			`","thread_id":"`+clientSessionUUID+`","x-codex-window-id":"`+clientSessionUUID+`:0"}}`),
		account, downstream,
	)

	var outboundDump strings.Builder
	for name, values := range outbound {
		for _, value := range values {
			outboundDump.WriteString(name + ": " + value + "\n")
		}
	}
	outboundDump.Write(body)

	// turn_id 不在此列：它是逐轮随机值，不标识设备或会话，重写只会引入不一致。
	for _, leaked := range []string{clientSessionUUID, clientInstallUUID, workspacePath, remoteURL, "james-6-23", commitHash} {
		if strings.Contains(outboundDump.String(), leaked) {
			t.Fatalf("original identifier %q survived into the outbound request:\n%s", leaked, outboundDump.String())
		}
	}
}

// TestApplyCodexRequestHeadersConvergesForwardedClientRequestID 走真实的出站头组装
// 顺序（白名单透传 → 指纹收敛 → 账号自定义头），而不是单独调收敛函数。
// issue #536 的 bug 正出在这个顺序上：X-Client-Request-Id 先被白名单原样透传，
// 收敛若不接手，原始线程标识就直达上游。
func TestApplyCodexRequestHeadersConvergesForwardedClientRequestID(t *testing.T) {
	const (
		clientUUID    = "01a00e75-8856-7542-89bf-35812620690f"
		installUUID   = "341596ee-ab98-43f8-82e2-08ecdfb56db4"
		workspacePath = "/Users/kyx/code_project/codex2api"
		remoteURL     = "https://github.com/james-6-23/codex2api.git"
	)
	rawMetadata := `{"installation_id":"` + installUUID + `","session_id":"` + clientUUID +
		`","thread_id":"` + clientUUID + `","window_id":"` + clientUUID +
		`:0","request_kind":"turn","workspaces":{"` + workspacePath +
		`":{"associated_remote_urls":{"origin":"` + remoteURL + `"},"has_changes":false}}}`

	downstream := http.Header{}
	downstream.Set("X-Codex-Turn-Metadata", rawMetadata)
	downstream.Set("Session-Id", clientUUID)
	downstream.Set("Thread-Id", clientUUID)
	downstream.Set("X-Client-Request-Id", clientUUID)
	downstream.Set("X-Codex-Window-Id", clientUUID+":0")
	downstream.Set("Originator", "codex-tui")

	account := fingerprintAccount(t, auth.CodexFingerprintModeSession)
	req, err := http.NewRequest(http.MethodPost, "https://chatgpt.com/backend-api/codex/responses", nil)
	if err != nil {
		t.Fatalf("http.NewRequest: %v", err)
	}
	applyCodexRequestHeaders(req, account, "access-token", "upstream-cache-key", "api-key-1", nil, downstream)

	ids := resolveCodexFingerprintIDs(account, downstream)
	if got := req.Header.Get("X-Client-Request-Id"); got != ids.threadID {
		t.Fatalf("X-Client-Request-Id = %q, want converged thread id %q", got, ids.threadID)
	}
	// 出站会话键仍归 resolveUpstreamSessionID 管，收敛默认不得介入
	// （对齐需显式开 CODEX_SESSION_HEADER_ALIGN_CONVERGED）。头名改成真实形态，
	// 但取值语义不变。
	if got := req.Header.Get("Session-Id"); got != "upstream-cache-key" {
		t.Fatalf("Session-Id = %q, want the cache key untouched", got)
	}
	// thread-id 与 x-client-request-id 必须同值：后者已被收敛改写，前者回落到
	// 未收敛的值就自相矛盾。
	if got := req.Header.Get("Thread-Id"); got != ids.threadID {
		t.Fatalf("Thread-Id = %q, want converged thread id %q", got, ids.threadID)
	}
	// 下游没发 installation 头，出站也不该有。
	if got := req.Header.Get("X-Codex-Installation-Id"); got != "" {
		t.Fatalf("X-Codex-Installation-Id = %q, want unset", got)
	}

	var dump strings.Builder
	for name, values := range req.Header {
		for _, value := range values {
			dump.WriteString(name + ": " + value + "\n")
		}
	}
	for _, leaked := range []string{clientUUID, installUUID, workspacePath, remoteURL, "james-6-23"} {
		if strings.Contains(dump.String(), leaked) {
			t.Fatalf("original identifier %q survived the real header pipeline:\n%s", leaked, dump.String())
		}
	}
}
