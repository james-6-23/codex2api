package proxy

import (
	"net/http"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

const (
	lineageContextWindowID = "01a00e75-1111-7542-89bf-35812620690f"
	lineageParentThreadID  = "01a00e75-2222-7542-89bf-35812620690f"
	lineageForkedThreadID  = "01a00e75-3333-7542-89bf-35812620690f"
	lineageClientThreadID  = "01a00e75-8856-7542-89bf-35812620690f"
)

func lineageTurnMetadata() string {
	return `{"installation_id":"341596ee-ab98-43f8-82e2-08ecdfb56db4","session_id":"` + lineageClientThreadID +
		`","thread_id":"` + lineageClientThreadID + `","window_id":"` + lineageClientThreadID +
		`:0","context_window_id":"` + lineageContextWindowID +
		`","parent_thread_id":"` + lineageParentThreadID +
		`","forked_from_thread_id":"` + lineageForkedThreadID +
		`","parent_turn_id":"turn-parent-1","root_turn_id":"turn-root-1",` +
		`"turn_id":"turn-9","turn_started_at_unix_ms":1735689600000,"request_kind":"turn"}`
}

func TestConvergeLineageMetadataReplacesDownstreamIdentities(t *testing.T) {
	account := fingerprintAccount(t, auth.CodexFingerprintModeSession)
	ids := resolveCodexFingerprintIDs(account, nil)
	if ids == nil {
		t.Fatal("session 档未推导出收敛身份")
	}

	rewritten, changed := rewriteCodexTurnMetadataJSON(lineageTurnMetadata(), ids)
	if !changed {
		t.Fatal("turn metadata 未被改写")
	}

	// 会话级 / 线程谱系标识必须换掉：它们跨请求稳定，留着就能把同一下游用户的
	// 多次请求串起来，前面几项收敛随之失效。
	for path, downstream := range map[string]string{
		"context_window_id":     lineageContextWindowID,
		"parent_thread_id":      lineageParentThreadID,
		"forked_from_thread_id": lineageForkedThreadID,
	} {
		got := gjson.Get(rewritten, path).String()
		if got == downstream {
			t.Fatalf("%s 仍是下游真实值 %q", path, downstream)
		}
		if got == "" {
			t.Fatalf("%s 被删除了，应替换而非移除（形状要保持）", path)
		}
		assertUUIDv7(t, path, got)
	}

	// 轮次级标识保持原样：与本仓库对 turn_id 的既有判断一致——逐轮变化、不标识
	// 设备或会话，重算反而会与同轮的 turn_id / 时间戳失去一致性。
	for path, want := range map[string]string{
		"parent_turn_id": "turn-parent-1",
		"root_turn_id":   "turn-root-1",
		"turn_id":        "turn-9",
	} {
		if got := gjson.Get(rewritten, path).String(); got != want {
			t.Fatalf("%s = %q, want 原样保留 %q", path, got, want)
		}
	}
	if got := gjson.Get(rewritten, "turn_started_at_unix_ms").Int(); got != 1735689600000 {
		t.Fatalf("turn_started_at_unix_ms = %d, want 原样保留", got)
	}
}

func TestConvergeLineageMetadataIsDeterministicAndAccountScoped(t *testing.T) {
	first := convergeCodexLineageValue(1, "context_window_id", lineageContextWindowID)
	if second := convergeCodexLineageValue(1, "context_window_id", lineageContextWindowID); second != first {
		t.Fatalf("同输入两次派生不一致: %q vs %q", first, second)
	}
	// 换账号必须换值，否则上游能把两个账号关联到同一个下游用户。
	if other := convergeCodexLineageValue(2, "context_window_id", lineageContextWindowID); other == first {
		t.Fatalf("跨账号撞值 %q，同一下游用户在两个账号下可被关联", first)
	}
	// 同一个原始 UUID 出现在不同字段上也不该派生出同值。
	if sameValue := convergeCodexLineageValue(1, "parent_thread_id", lineageContextWindowID); sameValue == first {
		t.Fatalf("跨字段撞值 %q", first)
	}
}

func TestConvergeLineageMetadataSkippedInDeviceMode(t *testing.T) {
	// device 档只收敛 installation_id，会话级字段一律不动。
	account := fingerprintAccount(t, auth.CodexFingerprintModeDevice)
	ids := resolveCodexFingerprintIDs(account, nil)
	if ids == nil {
		t.Fatal("device 档未推导出收敛身份")
	}

	rewritten, _ := rewriteCodexTurnMetadataJSON(lineageTurnMetadata(), ids)
	for path, want := range map[string]string{
		"context_window_id":     lineageContextWindowID,
		"parent_thread_id":      lineageParentThreadID,
		"forked_from_thread_id": lineageForkedThreadID,
	} {
		if got := gjson.Get(rewritten, path).String(); got != want {
			t.Fatalf("device 档改写了 %s: %q, want %q", path, got, want)
		}
	}
}

func TestConvergeLineageMetadataLeavesAbsentKeysAlone(t *testing.T) {
	// 绝不新增字段：客户端没发过的键凭空出现，本身就改变了 metadata 的形状。
	account := fingerprintAccount(t, auth.CodexFingerprintModeSession)
	ids := resolveCodexFingerprintIDs(account, nil)
	minimal := `{"installation_id":"341596ee-ab98-43f8-82e2-08ecdfb56db4","session_id":"` +
		lineageClientThreadID + `","thread_id":"` + lineageClientThreadID + `"}`

	rewritten, _ := rewriteCodexTurnMetadataJSON(minimal, ids)
	for _, path := range codexLineageMetadataPaths {
		if gjson.Get(rewritten, path).Exists() {
			t.Fatalf("凭空新增了 %s: %s", path, rewritten)
		}
	}
}

func TestParentThreadIDHeaderMatchesMetadata(t *testing.T) {
	// 真实客户端里 x-codex-parent-thread-id 与 metadata.parent_thread_id 是同一份
	// 快照的两个投影，两处报不同值就是可直接比对的破绽。
	account := fingerprintAccount(t, auth.CodexFingerprintModeSession)
	downstream := http.Header{}
	downstream.Set("X-Codex-Turn-Metadata", lineageTurnMetadata())
	downstream.Set("Session-Id", lineageClientThreadID)
	downstream.Set("Thread-Id", lineageClientThreadID)
	downstream.Set("X-Codex-Parent-Thread-Id", lineageParentThreadID)
	downstream.Set("Originator", "codex-tui")

	outbound := http.Header{}
	outbound.Set("X-Codex-Turn-Metadata", lineageTurnMetadata())
	ApplyCodexFingerprintHeaders(outbound, account, downstream)

	headerValue := outbound.Get("X-Codex-Parent-Thread-Id")
	if headerValue == "" {
		t.Fatal("下游发了 parent-thread-id 头，出站却丢了")
	}
	if headerValue == lineageParentThreadID {
		t.Fatal("parent-thread-id 头仍是下游真实值")
	}
	metadataValue := gjson.Get(outbound.Get("X-Codex-Turn-Metadata"), "parent_thread_id").String()
	if headerValue != metadataValue {
		t.Fatalf("头与 metadata 各说各话: header=%q metadata=%q", headerValue, metadataValue)
	}
}

func TestParentThreadIDHeaderNotFabricated(t *testing.T) {
	// 下游没发就不该补：给一个本来没有该头的请求伪造出半套客户端特征，
	// 比不发更可疑（与本文件对 installation-id 头的既有判断一致）。
	account := fingerprintAccount(t, auth.CodexFingerprintModeSession)
	downstream := http.Header{}
	downstream.Set("X-Codex-Turn-Metadata", lineageTurnMetadata())
	downstream.Set("Session-Id", lineageClientThreadID)
	downstream.Set("Originator", "codex-tui")

	outbound := http.Header{}
	outbound.Set("X-Codex-Turn-Metadata", lineageTurnMetadata())
	ApplyCodexFingerprintHeaders(outbound, account, downstream)

	if got := outbound.Get("X-Codex-Parent-Thread-Id"); got != "" {
		t.Fatalf("X-Codex-Parent-Thread-Id = %q, want unset", got)
	}
}

func TestConvergeLineageMetadataPreservesKeyOrder(t *testing.T) {
	// 用 sjson 原地替换而非反序列化重排：键序变化本身就是可识别特征。
	account := fingerprintAccount(t, auth.CodexFingerprintModeSession)
	ids := resolveCodexFingerprintIDs(account, nil)
	raw := lineageTurnMetadata()

	rewritten, _ := rewriteCodexTurnMetadataJSON(raw, ids)
	keyOrder := func(doc string) []string {
		var keys []string
		gjson.Parse(doc).ForEach(func(key, _ gjson.Result) bool {
			keys = append(keys, key.String())
			return true
		})
		return keys
	}
	before, after := keyOrder(raw), keyOrder(rewritten)
	if strings.Join(before, ",") != strings.Join(after, ",") {
		t.Fatalf("键序被改动:\nbefore=%v\nafter =%v", before, after)
	}
}
