package proxy

import (
	"bytes"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
)

func TestRetryAccountExclusionsSoftResetPreservesHard(t *testing.T) {
	exclusions := newRetryAccountExclusions()
	exclusions.MarkSoftFirstTokenTimeout(1)
	exclusions.MarkHard(2)

	selection := exclusions.ForSelection()
	if !selection[1] || !selection[2] {
		t.Fatalf("selection excludes = %#v, want soft and hard accounts", selection)
	}

	if !exclusions.ResetSoft() {
		t.Fatal("ResetSoft() = false, want true")
	}
	selection = exclusions.ForSelection()
	if selection[1] {
		t.Fatalf("soft account still excluded after reset: %#v", selection)
	}
	if !selection[2] {
		t.Fatalf("hard account was cleared by soft reset: %#v", selection)
	}
}

func TestRetryAccountExclusionsHardOverridesSoft(t *testing.T) {
	exclusions := newRetryAccountExclusions()
	exclusions.MarkSoftFirstTokenTimeout(1)
	exclusions.MarkHard(1)

	if exclusions.ResetSoft() {
		t.Fatal("ResetSoft() cleared a hard-only account")
	}
	selection := exclusions.ForSelection()
	if !selection[1] {
		t.Fatalf("hard account missing from selection excludes: %#v", selection)
	}
}

func TestIsFirstTokenTimeoutOutcome(t *testing.T) {
	if !isFirstTokenTimeoutOutcome(firstTokenTimeoutOutcome(10)) {
		t.Fatal("first-token timeout outcome should be classified as timeout")
	}
	if isFirstTokenTimeoutOutcome(streamOutcome{failureKind: "transport"}) {
		t.Fatal("transport outcome should not be classified as first-token timeout")
	}
}

func TestWebsocketHTTPFallbackStateRetainsLeaseOnce(t *testing.T) {
	account := &auth.Account{DBID: 7}
	var state websocketHTTPFallbackState
	state.Retain(account, "http://proxy.example", 1500*time.Millisecond, "local_read_limit")

	if !state.ForceHTTP() {
		t.Fatal("ForceHTTP() = false after retaining a WebSocket 1009 fallback")
	}
	if state.ID() == "" {
		t.Fatal("fallback correlation ID is empty")
	}
	if state.Source() != "local_read_limit" {
		t.Fatalf("source = %q, want local_read_limit", state.Source())
	}
	gotAccount, gotProxy, ok := state.Take()
	if !ok || gotAccount != account || gotProxy != "http://proxy.example" {
		t.Fatalf("Take() = (%p, %q, %v), want retained account/proxy", gotAccount, gotProxy, ok)
	}
	if _, _, ok := state.Take(); ok {
		t.Fatal("second Take() reused the retained account lease")
	}
	if !state.ForceHTTP() {
		t.Fatal("ForceHTTP() reset after consuming the retained lease")
	}
}

func TestWebsocketHTTPFallbackStateLogsAttemptsWithoutInventingFirstEvent(t *testing.T) {
	previousWriter := log.Writer()
	previousFlags := log.Flags()
	var logs bytes.Buffer
	log.SetOutput(&logs)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(previousWriter)
		log.SetFlags(previousFlags)
	})

	account := &auth.Account{DBID: 7}
	var state websocketHTTPFallbackState
	state.Retain(account, "", 1500*time.Millisecond, "peer_close")
	fallbackID := state.ID()
	state.LogHTTPAttemptCompletion("/v1/responses", account.ID(), 2, 500, 0, logStatusUpstreamStreamBreak)

	firstAttempt := logs.String()
	for _, want := range []string{
		"fallback_id=" + fallbackID,
		"attempt=2",
		"http_first_event_ms=0",
		"total_first_event_ms=0",
	} {
		if !strings.Contains(firstAttempt, want) {
			t.Fatalf("first attempt log missing %q: %s", want, firstAttempt)
		}
	}

	logs.Reset()
	state.LogHTTPAttemptCompletion("/v1/responses", account.ID(), 3, 500, 100, 200)
	secondAttempt := logs.String()
	if !strings.Contains(secondAttempt, "fallback_id="+fallbackID) || !strings.Contains(secondAttempt, "attempt=3") {
		t.Fatalf("subsequent attempt lost fallback correlation: %s", secondAttempt)
	}
	if strings.Contains(secondAttempt, "total_first_event_ms=0") {
		t.Fatalf("observed first event was not included in cumulative timing: %s", secondAttempt)
	}
}
