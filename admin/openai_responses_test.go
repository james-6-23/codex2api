package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

func TestFetchOpenAIResponsesModelIDsSupportsV1BaseURL(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Fatalf("path = %q, want /v1/models", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-test" {
			t.Fatalf("Authorization = %q, want Bearer sk-test", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4.1"},{"id":"gpt-4.1"},{"id":"gpt-4.1-mini"}]}`))
	}))
	defer server.Close()

	models, err := fetchOpenAIResponsesModelIDs(context.Background(), server.URL+"/v1", "sk-test", "")
	if err != nil {
		t.Fatalf("fetchOpenAIResponsesModelIDs returned error: %v", err)
	}
	want := []string{"gpt-4.1", "gpt-4.1-mini"}
	if !reflect.DeepEqual(models, want) {
		t.Fatalf("models = %#v, want %#v", models, want)
	}
}

func TestConnectionTestModelForOpenAIResponsesAccountUsesFirstSupportedFallback(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{TestModel: "gpt-5.4"})
	handler := &Handler{store: store}
	account := &auth.Account{
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      "https://api.openai.com",
		APIKey:       "sk-test",
		Models:       []string{"gpt-4.1-mini", "gpt-4.1"},
	}

	model, err := handler.connectionTestModelForAccount(context.Background(), account, "")
	if err != nil {
		t.Fatalf("connectionTestModelForAccount returned error: %v", err)
	}
	if model != "gpt-4.1-mini" {
		t.Fatalf("model = %q, want first account model", model)
	}

	model, err = handler.connectionTestModelForAccount(context.Background(), account, "gpt-4.1")
	if err != nil {
		t.Fatalf("requested model returned error: %v", err)
	}
	if model != "gpt-4.1" {
		t.Fatalf("requested model = %q, want gpt-4.1", model)
	}
}

func TestConnectionTestModelForOpenAIResponsesAccountAcceptsMappingAlias(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{TestModel: "gpt-5.4"})
	handler := &Handler{store: store}
	account := &auth.Account{
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      "https://api.openai.com",
		APIKey:       "sk-test",
		Models:       []string{"gpt-4.1"},
		ModelMapping: `{"client-alias":"gpt-4.1"}`,
	}

	model, err := handler.connectionTestModelForAccount(context.Background(), account, "client-alias")
	if err != nil {
		t.Fatalf("requested alias returned error: %v", err)
	}
	if model != "gpt-4.1" {
		t.Fatalf("mapped model = %q, want gpt-4.1", model)
	}
}

// TestNormalizeAccountModelMappingValidatesModelNames 验证映射键值必须通过
// security.ValidateModelName——源别名会进入 /v1/models 响应和使用日志,
// 不能放任意字符串注入(PR #325 评审遗留)。
func TestNormalizeAccountModelMappingValidatesModelNames(t *testing.T) {
	valid := `{"gpt-5.4":"gpt-5.4-upstream","claude-x":"gpt-5.4"}`
	if _, err := normalizeAccountModelMapping(valid); err != nil {
		t.Fatalf("valid mapping rejected: %v", err)
	}

	for name, raw := range map[string]string{
		"invalid_key_chars":   `{"bad model\nname":"gpt-5.4"}`,
		"invalid_value_chars": `{"gpt-5.4":"bad<script>"}`,
		"key_too_long":        `{"` + strings.Repeat("a", 300) + `":"gpt-5.4"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := normalizeAccountModelMapping(raw); err == nil {
				t.Fatalf("mapping %q must be rejected", raw)
			}
		})
	}
}
