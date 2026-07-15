package proxy

import (
	"context"
	"testing"
	"time"

	"github.com/codex2api/database"
)

func TestFirstTokenTimeoutGuardCancelsUpstream(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	guard := newFirstTokenTimeoutGuard(20*time.Millisecond, cancel)
	defer guard.Stop()

	select {
	case <-ctx.Done():
	case <-time.After(500 * time.Millisecond):
		t.Fatal("first token timeout guard did not cancel upstream context")
	}
	if !guard.TimedOut() {
		t.Fatal("guard TimedOut() = false, want true")
	}
}

func TestFirstTokenTimeoutGuardStopsOnFirstTokenEvent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	guard := newFirstTokenTimeoutGuard(30*time.Millisecond, cancel)
	defer guard.Stop()

	guard.MarkPayload([]byte(`{"type":"response.output_text.delta","delta":"hello"}`))

	select {
	case <-ctx.Done():
		t.Fatal("first token timeout guard canceled after first token event")
	case <-time.After(80 * time.Millisecond):
	}
	if guard.TimedOut() {
		t.Fatal("guard TimedOut() = true, want false")
	}
}

func TestFirstTokenTimeoutGuardMarkProgressIgnoresLifecycle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	guard := newFirstTokenTimeoutGuard(30*time.Millisecond, cancel)
	defer guard.Stop()

	// created / in_progress 不应解除看门狗：上游只发生命周期帧仍视为未开始响应。
	guard.MarkProgress("response.created")
	guard.MarkProgress("response.in_progress")

	select {
	case <-ctx.Done():
	case <-time.After(200 * time.Millisecond):
		t.Fatal("guard did not fire when only lifecycle frames arrived")
	}
	if !guard.TimedOut() {
		t.Fatal("guard TimedOut() = false, want true")
	}
}

func TestFirstTokenTimeoutGuardMarkProgressDisarmsOnStructuralFrame(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	guard := newFirstTokenTimeoutGuard(30*time.Millisecond, cancel)
	defer guard.Stop()

	guard.MarkProgress("response.created")           // 生命周期帧：不解除
	guard.MarkProgress("response.output_item.added") // 首个非生命周期帧：解除看门狗

	select {
	case <-ctx.Done():
		t.Fatal("guard fired after a structural frame proved upstream liveness")
	case <-time.After(120 * time.Millisecond):
	}
	if guard.TimedOut() {
		t.Fatal("guard TimedOut() = true, want false")
	}
}

func TestNormalizeRuntimeSettingsFirstTokenTimeout(t *testing.T) {
	settings := NormalizeRuntimeSettings(RuntimeSettings{FirstTokenTimeoutSec: -1})
	if settings.FirstTokenTimeoutSec != 0 {
		t.Fatalf("negative first token timeout normalized to %d, want 0", settings.FirstTokenTimeoutSec)
	}

	settings = NormalizeRuntimeSettings(RuntimeSettings{FirstTokenTimeoutSec: 601})
	if settings.FirstTokenTimeoutSec != 600 {
		t.Fatalf("oversized first token timeout normalized to %d, want 600", settings.FirstTokenTimeoutSec)
	}
}

func TestNormalizeRuntimeSettingsFirstTokenMode(t *testing.T) {
	settings := NormalizeRuntimeSettings(RuntimeSettings{FirstTokenMode: "loose"})
	if settings.FirstTokenMode != FirstTokenModeLoose {
		t.Fatalf("FirstTokenMode = %q, want loose", settings.FirstTokenMode)
	}

	settings = NormalizeRuntimeSettings(RuntimeSettings{FirstTokenMode: "invalid"})
	if settings.FirstTokenMode != FirstTokenModeStrict {
		t.Fatalf("invalid FirstTokenMode = %q, want strict", settings.FirstTokenMode)
	}
}

func TestNormalizeRuntimeSettingsCodexWSSilentRetries(t *testing.T) {
	settings := NormalizeRuntimeSettings(RuntimeSettings{CodexWSSilentRetries: -1})
	if settings.CodexWSSilentRetries != 0 {
		t.Fatalf("negative CodexWSSilentRetries normalized to %d, want 0", settings.CodexWSSilentRetries)
	}

	settings = NormalizeRuntimeSettings(RuntimeSettings{CodexWSSilentRetries: 99})
	if settings.CodexWSSilentRetries != 10 {
		t.Fatalf("oversized CodexWSSilentRetries normalized to %d, want 10", settings.CodexWSSilentRetries)
	}
}

func TestApplyRuntimeSettingsFromSystemFirstTokenTimeout(t *testing.T) {
	defer ApplyRuntimeSettings(DefaultRuntimeSettings())

	settings := ApplyRuntimeSettingsFromSystem(&database.SystemSettings{
		FirstTokenTimeoutSeconds: 42,
		FirstTokenMode:           FirstTokenModeLoose,
	})

	if settings.FirstTokenMode != FirstTokenModeLoose {
		t.Fatalf("FirstTokenMode = %q, want loose", settings.FirstTokenMode)
	}
	if settings.FirstTokenTimeoutSec != 42 {
		t.Fatalf("FirstTokenTimeoutSec = %d, want 42", settings.FirstTokenTimeoutSec)
	}
	if got := currentFirstTokenTimeout(); got != 42*time.Second {
		t.Fatalf("currentFirstTokenTimeout() = %s, want 42s", got)
	}
}

func TestApplyRuntimeSettingsFromSystemCodexWebSocketRetrySettings(t *testing.T) {
	defer ApplyRuntimeSettings(DefaultRuntimeSettings())

	settings := ApplyRuntimeSettingsFromSystem(&database.SystemSettings{
		CodexWSHideUpstreamErrors: true,
		CodexWSSilentRetryEnabled: true,
		CodexWSSilentMaxRetries:   42,
	})

	if !settings.CodexWSHideErrors {
		t.Fatal("CodexWSHideErrors = false, want true")
	}
	if !settings.CodexWSSilentRetry {
		t.Fatal("CodexWSSilentRetry = false, want true")
	}
	if settings.CodexWSSilentRetries != 10 {
		t.Fatalf("CodexWSSilentRetries = %d, want 10", settings.CodexWSSilentRetries)
	}
}

func TestFirstTokenTimeoutForRequestExemptsCompaction(t *testing.T) {
	base := 30 * time.Second

	// 普通请求保持配置阈值不变。
	if got := firstTokenTimeoutForRequest(base, false); got != base {
		t.Fatalf("non-compaction timeout = %s, want %s", got, base)
	}
	// 压缩轮豁免看门狗（返回 0）。
	if got := firstTokenTimeoutForRequest(base, true); got != 0 {
		t.Fatalf("compaction timeout = %s, want 0", got)
	}
	// 看门狗本身遇到 0 阈值不启动，保证豁免生效。
	if guard := newFirstTokenTimeoutGuard(firstTokenTimeoutForRequest(base, true), func() {}); guard != nil {
		t.Fatal("compaction round should not create a first token timeout guard")
	}
}

func TestNormalizeBillingTierPolicy(t *testing.T) {
	if got := NormalizeBillingTierPolicy(""); got != BillingTierPolicyActual {
		t.Fatalf("empty policy = %q, want actual", got)
	}
	if got := NormalizeBillingTierPolicy("requested"); got != BillingTierPolicyRequested {
		t.Fatalf("requested policy = %q, want requested", got)
	}
	if got := NormalizeBillingTierPolicy("invalid"); got != BillingTierPolicyActual {
		t.Fatalf("invalid policy = %q, want actual", got)
	}
}
