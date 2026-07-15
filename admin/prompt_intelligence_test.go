package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSearchGitHubPromptIntelligence(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") != "jailbreak prompt" {
			t.Fatalf("query = %q", r.URL.Query().Get("q"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[{"full_name":"owner/repo","html_url":"https://github.com/owner/repo","description":"prompt injection research","updated_at":"2026-07-15T00:00:00Z"}]}`))
	}))
	defer server.Close()
	old := githubPromptSearchBaseURL
	githubPromptSearchBaseURL = server.URL
	defer func() { githubPromptSearchBaseURL = old }()
	items, err := searchGitHubPromptIntelligence(context.Background(), "jailbreak prompt", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Title != "owner/repo" {
		t.Fatalf("items = %#v", items)
	}
}

func TestValidateIntelligenceCandidate(t *testing.T) {
	valid := promptIntelligenceCandidate{Name: "new_jailbreak_phrase", Pattern: `(?i)ignore\s+safety`, Weight: 80, Category: "prompt_injection", Strict: true}
	if err := validateIntelligenceCandidate(valid); err != nil {
		t.Fatal(err)
	}
	valid.Pattern = "("
	if err := validateIntelligenceCandidate(valid); err == nil {
		t.Fatal("invalid regexp accepted")
	}
}

func TestMergeIntelligenceQueriesIncludesChineseBuiltins(t *testing.T) {
	queries := mergeIntelligenceQueries(defaultIntelligenceQueries, []string{"custom query", "GPT 破甲 提示词"})
	want := map[string]bool{"大模型 破限 提示词": false, "GPT 破甲 提示词": false, "AI 越狱 提示词": false, "custom query": false}
	for _, query := range queries {
		if _, ok := want[query]; ok {
			want[query] = true
		}
	}
	for query, found := range want {
		if !found {
			t.Fatalf("missing query %q in %#v", query, queries)
		}
	}
}
