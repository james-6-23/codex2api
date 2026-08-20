package auth

import (
	"testing"
)

func TestOrcaRouterAccountPredicates(t *testing.T) {
	acc := &Account{
		UpstreamType: UpstreamOrcaRouter,
		BaseURL:      "https://api.orcarouter.ai/v1",
		APIKey:       "sk-orca-test",
		Models:       []string{"orcarouter/auto"},
	}
	if !acc.IsOrcaRouterAPI() {
		t.Fatal("IsOrcaRouterAPI must be true for orcarouter account")
	}
	if !acc.IsOpenAIResponsesAPI() {
		t.Fatal("orcarouter must dispatch through OpenAI Responses machinery (IsOpenAIResponsesAPI)")
	}
	if !acc.IsRelayStyle() {
		t.Fatal("orcarouter must be relay-style")
	}
	baseURL, apiKey := acc.OpenAIResponsesCredentials()
	if baseURL != "https://api.orcarouter.ai/v1" || apiKey != "sk-orca-test" {
		t.Fatalf("OpenAIResponsesCredentials = (%q, %q)", baseURL, apiKey)
	}
}

func TestOrcaRouterAccountMissingCredentialNotRoutable(t *testing.T) {
	acc := &Account{
		UpstreamType: UpstreamOrcaRouter,
		BaseURL:      "https://api.orcarouter.ai/v1",
		APIKey:       "",
		Models:       []string{"orcarouter/auto"},
	}
	if acc.IsOrcaRouterAPI() {
		t.Fatal("IsOrcaRouterAPI must require a non-empty API key")
	}
	if acc.IsOpenAIResponsesAPI() {
		t.Fatal("dispatch predicate must require a non-empty API key")
	}
}

func TestOrcaRouterUpstreamEndpointBuilding(t *testing.T) {
	if got := OpenAIResponsesEndpoint("https://api.orcarouter.ai/v1", "/v1/responses"); got != "https://api.orcarouter.ai/v1/responses" {
		t.Fatalf("endpoint = %q, want https://api.orcarouter.ai/v1/responses", got)
	}
	if got := OpenAIResponsesEndpoint("https://api.orcarouter.ai/v1", "/v1/models"); got != "https://api.orcarouter.ai/v1/models" {
		t.Fatalf("endpoint = %q, want https://api.orcarouter.ai/v1/models", got)
	}
	if got := OpenAIResponsesEndpoint("https://api.orcarouter.ai/v1", "/v1/responses/compact"); got != "https://api.orcarouter.ai/v1/responses/compact" {
		t.Fatalf("endpoint = %q, want compact endpoint", got)
	}
}

func TestNormalizeOpenAIResponsesBaseURLPreservesV1(t *testing.T) {
	got, err := NormalizeOpenAIResponsesBaseURL("https://api.orcarouter.ai/v1/")
	if err != nil {
		t.Fatalf("normalize error: %v", err)
	}
	if got != "https://api.orcarouter.ai/v1" {
		t.Fatalf("normalized = %q, want https://api.orcarouter.ai/v1", got)
	}
}
