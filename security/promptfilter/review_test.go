package promptfilter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestReviewTextAllowsWhenNotFlagged(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Fatalf("authorization = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model": "omni-moderation-latest",
			"results": []map[string]any{
				{"flagged": false},
			},
		})
	}))
	defer server.Close()

	client := ReviewClient{HTTPClient: server.Client()}
	flagged, model, err := client.ReviewText(context.Background(), "hello", ReviewConfig{
		Enabled:        true,
		APIKey:         "test-key",
		BaseURL:        server.URL,
		Model:          "omni-moderation-latest",
		TimeoutSeconds: 2,
	})
	if err != nil {
		t.Fatalf("ReviewText returned error: %v", err)
	}
	if flagged {
		t.Fatal("flagged = true, want false")
	}
	if model != "omni-moderation-latest" {
		t.Fatalf("model = %q, want omni-moderation-latest", model)
	}
}

func TestReviewTextReturnsErrorWhenResultsMissing(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"omni-moderation-latest","results":[]}`))
	}))
	defer server.Close()

	client := ReviewClient{HTTPClient: server.Client()}
	_, _, err := client.ReviewText(context.Background(), "hello", ReviewConfig{
		Enabled:        true,
		APIKey:         "test-key",
		BaseURL:        server.URL,
		Model:          "omni-moderation-latest",
		TimeoutSeconds: 2,
	})
	if err == nil {
		t.Fatal("ReviewText returned nil error, want missing results error")
	}
}

func TestApplyReviewResultClearsLocalBlockWhenCleared(t *testing.T) {
	verdict := Verdict{Action: ActionBlock, Reason: "local block"}
	got := ApplyReviewResult(verdict, false, "omni-moderation-latest", nil, ReviewConfig{FailClosed: true, Model: "omni-moderation-latest"})
	if got.Action != ActionAllow {
		t.Fatalf("action = %s, want allow", got.Action)
	}
	if !got.Reviewed || got.ReviewFlagged {
		t.Fatalf("review metadata = %+v, want reviewed and not flagged", got)
	}
}

func TestApplyReviewResultBlocksWhenReviewFailsClosed(t *testing.T) {
	verdict := Verdict{Action: ActionAllow}
	got := ApplyReviewResult(verdict, false, "omni-moderation-latest", context.DeadlineExceeded, ReviewConfig{FailClosed: true, Model: "omni-moderation-latest"})
	if got.Action != ActionBlock {
		t.Fatalf("action = %s, want block", got.Action)
	}
	if got.ReviewError == "" {
		t.Fatal("expected review_error to be recorded")
	}
}

func TestApplyReviewResultAllowsWhenReviewFailsOpen(t *testing.T) {
	verdict := Verdict{Action: ActionBlock}
	got := ApplyReviewResult(verdict, false, "omni-moderation-latest", context.DeadlineExceeded, ReviewConfig{FailClosed: false, Model: "omni-moderation-latest"})
	if got.Action != ActionAllow {
		t.Fatalf("action = %s, want allow", got.Action)
	}
	if got.ReviewError == "" {
		t.Fatal("expected review_error to be recorded")
	}
}

func TestApplyReviewResultBlocksHighRiskWhenReviewFailsOpen(t *testing.T) {
	verdict := Verdict{Action: ActionBlock, Score: HighRiskReviewFailureScore}
	got := ApplyReviewResult(verdict, false, "omni-moderation-latest", context.DeadlineExceeded, ReviewConfig{FailClosed: false, Model: "omni-moderation-latest"})
	if got.Action != ActionBlock {
		t.Fatalf("action = %s, want block", got.Action)
	}
	if got.ReviewError == "" {
		t.Fatal("expected review_error to be recorded")
	}
	if !strings.Contains(got.Reason, "high-risk") {
		t.Fatalf("reason = %q, want high-risk marker", got.Reason)
	}
}

func TestApplyReviewResultBlocksStrictMatchWhenReviewFailsOpen(t *testing.T) {
	verdict := Verdict{Action: ActionBlock, Matched: []Match{{Name: "credential_theft", Strict: true}}}
	got := ApplyReviewResult(verdict, false, "omni-moderation-latest", context.DeadlineExceeded, ReviewConfig{FailClosed: false, Model: "omni-moderation-latest"})
	if got.Action != ActionBlock {
		t.Fatalf("action = %s, want block", got.Action)
	}
}

func TestParseReviewAPIKeys(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{"single", "sk-1", []string{"sk-1"}},
		{"newline separated", "sk-1\nsk-2\nsk-3", []string{"sk-1", "sk-2", "sk-3"}},
		{"comma and spaces", "sk-1, sk-2 ,sk-3", []string{"sk-1", "sk-2", "sk-3"}},
		{"dedupe and blanks", "sk-1\n\nsk-1\nsk-2\n  ", []string{"sk-1", "sk-2"}},
		{"empty", "   ", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseReviewAPIKeys(tc.raw)
			if len(got) != len(tc.want) {
				t.Fatalf("parseReviewAPIKeys(%q) = %v, want %v", tc.raw, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("parseReviewAPIKeys(%q)[%d] = %q, want %q", tc.raw, i, got[i], tc.want[i])
				}
			}
		})
	}
}

func TestReviewTextFailsOverToNextKeyOn429(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		seen[key]++
		mu.Unlock()
		if key != "good" {
			// 模拟低等级账号 TPM 限流。
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   "omni-moderation-latest",
			"results": []map[string]any{{"flagged": false}},
		})
	}))
	defer server.Close()

	client := ReviewClient{HTTPClient: server.Client()}
	flagged, _, err := client.ReviewText(context.Background(), "hello", ReviewConfig{
		Enabled:        true,
		APIKey:         "bad1\nbad2\ngood",
		BaseURL:        server.URL,
		Model:          "omni-moderation-latest",
		TimeoutSeconds: 2,
	})
	if err != nil {
		t.Fatalf("ReviewText returned error: %v", err)
	}
	if flagged {
		t.Fatal("flagged = true, want false")
	}
	mu.Lock()
	defer mu.Unlock()
	if seen["good"] == 0 {
		t.Fatalf("expected failover to reach the good key, seen=%v", seen)
	}
}

func TestReviewTextReturnsErrorWhenAllKeysRateLimited(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		seen[key]++
		mu.Unlock()
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()

	client := ReviewClient{HTTPClient: server.Client()}
	_, _, err := client.ReviewText(context.Background(), "hello", ReviewConfig{
		Enabled:        true,
		APIKey:         "k1\nk2",
		BaseURL:        server.URL,
		Model:          "omni-moderation-latest",
		TimeoutSeconds: 2,
	})
	if err == nil {
		t.Fatal("ReviewText returned nil error, want error after all keys rate limited")
	}
	mu.Lock()
	defer mu.Unlock()
	if seen["k1"] == 0 || seen["k2"] == 0 {
		t.Fatalf("expected both keys to be tried, seen=%v", seen)
	}
}

func TestReviewTextRoundRobinsAcrossKeys(t *testing.T) {
	var mu sync.Mutex
	seen := map[string]int{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		mu.Lock()
		seen[key]++
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   "omni-moderation-latest",
			"results": []map[string]any{{"flagged": false}},
		})
	}))
	defer server.Close()

	client := ReviewClient{HTTPClient: server.Client()}
	cfg := ReviewConfig{
		Enabled:        true,
		APIKey:         "ka\nkb\nkc",
		BaseURL:        server.URL,
		Model:          "omni-moderation-latest",
		TimeoutSeconds: 2,
	}
	// 连续多次请求应把成功请求分摊到全部 key 上。
	for i := 0; i < 9; i++ {
		if _, _, err := client.ReviewText(context.Background(), "hello", cfg); err != nil {
			t.Fatalf("ReviewText #%d error: %v", i, err)
		}
	}
	mu.Lock()
	defer mu.Unlock()
	for _, key := range []string{"ka", "kb", "kc"} {
		if seen[key] == 0 {
			t.Fatalf("key %q never used, seen=%v", key, seen)
		}
	}
}
