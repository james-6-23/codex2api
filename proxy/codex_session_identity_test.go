package proxy

import (
	"testing"

	"github.com/google/uuid"
)

// assertUUIDv7 断言取值是 UUIDv7：真实 Codex 客户端的 session_id / thread_id /
// prompt_cache_key 都是 v7，版本 nibble 与 variant 位对任何解析 UUID 的一侧都直接可见。
func assertUUIDv7(t *testing.T, label, value string) {
	t.Helper()
	parsed, err := uuid.Parse(value)
	if err != nil {
		t.Fatalf("%s = %q 不是合法 UUID: %v", label, value, err)
	}
	if got := parsed.Version(); got != 7 {
		t.Fatalf("%s = %q 版本 = %d, want 7", label, value, got)
	}
	if got := parsed.Variant(); got != uuid.RFC4122 {
		t.Fatalf("%s = %q variant = %v, want RFC4122", label, value, got)
	}
}

func TestDeriveStableSessionUUIDv7IsDeterministicAndV7(t *testing.T) {
	const seed = "codex2api:prompt-cache:some-key"
	first := DeriveStableSessionUUIDv7(seed)
	assertUUIDv7(t, "DeriveStableSessionUUIDv7", first)

	if second := DeriveStableSessionUUIDv7(seed); second != first {
		t.Fatalf("同种子两次派生不一致: %q vs %q（跨进程重启也必须恒定，否则每次重启都换一批上游身份）", first, second)
	}
	if other := DeriveStableSessionUUIDv7(seed + ":other"); other == first {
		t.Fatalf("不同种子派生撞值: %q", other)
	}
}

func TestIsolateCodexSessionIDProducesUUIDv7(t *testing.T) {
	got := IsolateCodexSessionID(7, "downstream-seed")
	assertUUIDv7(t, "IsolateCodexSessionID", got)

	if same := IsolateCodexSessionID(7, "downstream-seed"); same != got {
		t.Fatalf("确定性丢失: %q vs %q", got, same)
	}
	if other := IsolateCodexSessionID(8, "downstream-seed"); other == got {
		t.Fatalf("不同 API Key 未隔离，都得到 %q", got)
	}
}

func TestIsolateCodexSessionIDLeavesRawWithoutAPIKeyScope(t *testing.T) {
	// apiKeyID<=0 时保持原样返回：这是既有契约，隔离无从谈起时不该凭空造一个身份。
	const raw = "explicit-session"
	if got := IsolateCodexSessionID(0, raw); got != raw {
		t.Fatalf("IsolateCodexSessionID(0, %q) = %q, want 原样返回", raw, got)
	}
}

func TestDeterministicPromptCacheKeyProducesUUIDv7(t *testing.T) {
	got := deterministicPromptCacheKey("shared-key", nil)
	assertUUIDv7(t, "deterministicPromptCacheKey", got)

	// 与 resolveRequestSessionIdentity 的 API Key 兜底共享种子，必须同值——否则
	// 同一个 API Key 在 HTTP 与 WS 两条路径上会拿到两个上游身份。
	if want := DeriveStableSessionUUIDv7("codex2api:prompt-cache:shared-key"); got != want {
		t.Fatalf("deterministicPromptCacheKey = %q, want %q", got, want)
	}
}

func TestNewUpstreamSessionUUIDIsV7AndUnique(t *testing.T) {
	// 默认隔离是本网关最常走的路径，出站 prompt_cache_key / session-id 绝大多数由
	// NewUpstreamSessionUUID 产出。这条曾经用 uuid.NewString() 发 v4，实测时从上游
	// 回显的 prompt_cache_key 里看出版本位不对才发现（另两处确定性派生已单独覆盖）。
	seen := make(map[string]struct{}, 64)
	for i := 0; i < 64; i++ {
		got := NewUpstreamSessionUUID()
		assertUUIDv7(t, "NewUpstreamSessionUUID", got)
		if _, dup := seen[got]; dup {
			t.Fatalf("每请求隔离的会话键重复: %q", got)
		}
		seen[got] = struct{}{}
	}
}

func TestResolveUpstreamSessionIDIsolatedModeProducesV7(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	isolated := previous
	isolated.RequestIsolationMode = RequestIsolationModeIsolated
	ApplyRuntimeSettings(isolated)

	// 无显式会话 + 默认隔离 → 每请求唯一，且必须是 v7。
	first := resolveUpstreamSessionID(7, "seed", "", false)
	assertUUIDv7(t, "resolveUpstreamSessionID(isolated)", first)
	if second := resolveUpstreamSessionID(7, "seed", "", false); second == first {
		t.Fatalf("隔离模式下两次请求拿到同一个会话键 %q", first)
	}
}

func TestDeterministicPromptCacheKeyEmptyWithoutIdentity(t *testing.T) {
	if got := deterministicPromptCacheKey("", nil); got != "" {
		t.Fatalf("无 API Key 无账号时 = %q, want 空串", got)
	}
}
