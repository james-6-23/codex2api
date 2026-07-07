package proxy

import (
	"net/http"
	"strings"
	"testing"
)

func TestDeriveContentSessionSeedStableAcrossTurns(t *testing.T) {
	turn1 := []byte(`{
		"model":"gpt-5.4",
		"instructions":"You are a coding assistant.",
		"input":[
			{"role":"user","content":"帮我写个爬虫"}
		]
	}`)
	// 第二轮:同一对话,追加了 assistant 回复和新的 user 消息(首条 user 不变)
	turn2 := []byte(`{
		"model":"gpt-5.4",
		"instructions":"You are a coding assistant.",
		"input":[
			{"role":"user","content":"帮我写个爬虫"},
			{"role":"assistant","content":"好的,用 requests 还是 playwright?"},
			{"role":"user","content":"用 requests"}
		]
	}`)

	seed1 := deriveContentSessionSeed(turn1)
	seed2 := deriveContentSessionSeed(turn2)
	if seed1 == "" {
		t.Fatal("seed1 should not be empty")
	}
	if !strings.HasPrefix(seed1, "content-") {
		t.Fatalf("seed prefix missing: %q", seed1)
	}
	if seed1 != seed2 {
		t.Fatalf("same conversation must derive same seed: %q vs %q", seed1, seed2)
	}
}

func TestDeriveContentSessionSeedDistinguishesConversations(t *testing.T) {
	convA := []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":"你好"}]}`)
	convB := []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":"help me debug"}]}`)

	seedA := deriveContentSessionSeed(convA)
	seedB := deriveContentSessionSeed(convB)
	if seedA == "" || seedB == "" {
		t.Fatal("both seeds should derive")
	}
	if seedA == seedB {
		t.Fatal("different conversations must derive different seeds")
	}
}

func TestDeriveContentSessionSeedStringInput(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","input":"hello world"}`)
	if seed := deriveContentSessionSeed(body); seed == "" {
		t.Fatal("string input should derive a seed")
	}
}

func TestDeriveContentSessionSeedChatCompletionsMessages(t *testing.T) {
	body := []byte(`{
		"model":"gpt-5.4",
		"messages":[
			{"role":"system","content":"be brief"},
			{"role":"user","content":"question one"}
		]
	}`)
	if seed := deriveContentSessionSeed(body); seed == "" {
		t.Fatal("chat completions messages should derive a seed")
	}
}

func TestDeriveContentSessionSeedSkipsContinuation(t *testing.T) {
	// 续链请求的 input 逐轮变化,内容种子会漂移导致换号,必须跳过
	body := []byte(`{"model":"gpt-5.4","previous_response_id":"resp_1","input":[{"role":"user","content":"next"}]}`)
	if seed := deriveContentSessionSeed(body); seed != "" {
		t.Fatalf("continuation request must not derive content seed, got %q", seed)
	}
}

func TestDeriveContentSessionSeedRequiresUserAnchor(t *testing.T) {
	// 没有 user 消息可锚定时放弃(回退按 API Key)
	body := []byte(`{"model":"gpt-5.4","input":[{"role":"assistant","content":"orphan"}]}`)
	if seed := deriveContentSessionSeed(body); seed != "" {
		t.Fatalf("no user anchor must not derive seed, got %q", seed)
	}
	if seed := deriveContentSessionSeed(nil); seed != "" {
		t.Fatalf("empty body must not derive seed, got %q", seed)
	}
}

func TestResolveSessionIDUsesContentSeedBeforeAPIKeyFallback(t *testing.T) {
	headers := http.Header{"Authorization": []string{"Bearer shared-channel-key"}}
	convA := []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":"conversation A"}]}`)
	convB := []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":"conversation B"}]}`)

	idA := ResolveSessionID(headers, convA)
	idB := ResolveSessionID(headers, convB)
	if idA == idB {
		t.Fatal("same API key but different conversations must resolve to different session IDs")
	}
	if !strings.HasPrefix(idA, "content-") {
		t.Fatalf("expected content-derived session ID, got %q", idA)
	}

	// 同一对话多轮 → 稳定
	convA2 := []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":"conversation A"},{"role":"assistant","content":"hi"},{"role":"user","content":"more"}]}`)
	if got := ResolveSessionID(headers, convA2); got != idA {
		t.Fatalf("multi-turn same conversation must keep session ID: %q vs %q", got, idA)
	}
}

func TestResolveSessionIDExplicitStillWins(t *testing.T) {
	headers := http.Header{"Session_id": []string{"explicit-1"}}
	body := []byte(`{"model":"gpt-5.4","input":[{"role":"user","content":"anything"}]}`)
	if got := ResolveSessionID(headers, body); got != "explicit-1" {
		t.Fatalf("explicit session header must win, got %q", got)
	}
}

func TestResolveSessionIDContinuationFallsBackToAPIKey(t *testing.T) {
	headers := http.Header{"Authorization": []string{"Bearer shared-channel-key"}}
	cont := []byte(`{"model":"gpt-5.4","previous_response_id":"resp_1","input":[{"role":"user","content":"next"}]}`)
	noBody := []byte(`{}`)

	// 续链请求跳过内容种子,应与"无 body 信号"请求一样落到按 API Key 的确定性 UUID
	if got, want := ResolveSessionID(headers, cont), ResolveSessionID(headers, noBody); got != want {
		t.Fatalf("continuation must fall back to API-key derivation: %q vs %q", got, want)
	}
}
