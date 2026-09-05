package auth

import (
	"testing"
	"time"
)

func TestParseClaudeConfig_FirstTokenTimeoutDefaultsWhenMissing(t *testing.T) {
	cfg := ParseClaudeConfig(`{"fingerprint_mode":"preserve"}`)
	if cfg.FirstTokenTimeoutSecondsValue() != DefaultClaudeFirstTokenTimeoutSeconds {
		t.Fatalf("missing field must default to %d, got %d", DefaultClaudeFirstTokenTimeoutSeconds, cfg.FirstTokenTimeoutSecondsValue())
	}
	if !cfg.StreamKeepaliveEnabledValue() {
		t.Fatal("missing stream_keepalive_enabled must default to true")
	}
}

func TestParseClaudeConfig_FirstTokenTimeoutExplicitZeroFollowsGlobal(t *testing.T) {
	cfg := ParseClaudeConfig(`{"first_token_timeout_seconds":0,"stream_keepalive_enabled":false}`)
	if cfg.FirstTokenTimeoutSecondsValue() != 0 {
		t.Fatalf("explicit 0 must stay 0 (follow global), got %d", cfg.FirstTokenTimeoutSecondsValue())
	}
	if cfg.StreamKeepaliveEnabledValue() {
		t.Fatal("explicit false must stay false")
	}
}

func TestNormalizeClaudeFirstTokenTimeoutSeconds(t *testing.T) {
	cases := map[string]struct {
		in   *int
		want int
	}{
		"nil":      {nil, DefaultClaudeFirstTokenTimeoutSeconds},
		"negative": {intPtrForTest(-5), 0},
		"zero":     {intPtrForTest(0), 0},
		"normal":   {intPtrForTest(90), 90},
		"too big":  {intPtrForTest(99999), MaxClaudeFirstTokenTimeoutSeconds},
	}
	for name, tc := range cases {
		if got := NormalizeClaudeFirstTokenTimeoutSeconds(tc.in); got != tc.want {
			t.Fatalf("%s: got %d, want %d", name, got, tc.want)
		}
	}
}

func TestStoreClaudeFirstTokenTimeoutRoundTrip(t *testing.T) {
	store := NewStore(nil, nil, nil)
	defer store.Stop()
	if got := store.ClaudeFirstTokenTimeout(); got != time.Duration(DefaultClaudeFirstTokenTimeoutSeconds)*time.Second {
		t.Fatalf("fresh store must use the default, got %s", got)
	}
	store.SetClaudeFirstTokenTimeoutSeconds(45)
	if got := store.ClaudeFirstTokenTimeout(); got != 45*time.Second {
		t.Fatalf("got %s, want 45s", got)
	}
	store.SetClaudeFirstTokenTimeoutSeconds(0)
	if got := store.ClaudeFirstTokenTimeout(); got != 0 {
		t.Fatalf("0 must disable the Claude-specific timeout, got %s", got)
	}
	if !store.ClaudeStreamKeepaliveEnabled() {
		t.Fatal("fresh store must enable pre-first-token keepalive")
	}
	store.SetClaudeStreamKeepaliveEnabled(false)
	if store.ClaudeStreamKeepaliveEnabled() {
		t.Fatal("keepalive switch must persist false")
	}
}

func TestApplyClaudeConfigToStore_FirstTokenTimeout(t *testing.T) {
	store := NewStore(nil, nil, nil)
	defer store.Stop()
	applyClaudeConfigToStore(store, `{"first_token_timeout_seconds":75,"stream_keepalive_enabled":false}`)
	if got := store.ClaudeFirstTokenTimeout(); got != 75*time.Second {
		t.Fatalf("got %s, want 75s", got)
	}
	if store.ClaudeStreamKeepaliveEnabled() {
		t.Fatal("stream keepalive must be applied from config")
	}
}

func intPtrForTest(v int) *int { return &v }
