package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestResolveAccountModelMappingUsesGrokVisibleTargets(t *testing.T) {
	t.Run("conservative defaults before catalog sync", func(t *testing.T) {
		account := &auth.Account{
			UpstreamType: auth.UpstreamGrok,
			APIKey:       "test-key",
			ModelMapping: `{"gpt-5.5":"grok-4.5"}`,
		}
		if got, ok := resolveAccountModelMapping(account, "gpt-5.5"); !ok || got != "grok-4.5" {
			t.Fatalf("mapping = %q, %t; want grok-4.5, true", got, ok)
		}
	})

	t.Run("explicit whitelist must include target", func(t *testing.T) {
		account := &auth.Account{
			UpstreamType: auth.UpstreamGrok,
			APIKey:       "test-key",
			Models:       []string{"grok-4.6"},
			ModelMapping: `{"gpt-5.5":"grok-4.5"}`,
		}
		if got, ok := resolveAccountModelMapping(account, "gpt-5.5"); ok || got != "gpt-5.5" {
			t.Fatalf("mapping = %q, %t; target outside whitelist must be rejected", got, ok)
		}
	})

	t.Run("authoritative catalog hides absent and hidden targets", func(t *testing.T) {
		account := &auth.Account{
			UpstreamType: auth.UpstreamGrok,
			APIKey:       "test-key",
			ModelMapping: `{"gpt-visible":"grok-visible","gpt-hidden":"grok-hidden","gpt-absent":"grok-absent"}`,
		}
		account.SetGrokRoutingState(auth.GrokRoutingState{Models: []auth.GrokModelRoute{
			{ModelID: "grok-visible", APIBackend: auth.GrokProtocolResponses},
			{ModelID: "grok-hidden", APIBackend: auth.GrokProtocolResponses, Hidden: true},
		}})
		if got, ok := resolveAccountModelMapping(account, "gpt-visible"); !ok || got != "grok-visible" {
			t.Fatalf("visible mapping = %q, %t; want grok-visible, true", got, ok)
		}
		for _, alias := range []string{"gpt-hidden", "gpt-absent"} {
			if got, ok := resolveAccountModelMapping(account, alias); ok || got != alias {
				t.Fatalf("mapping %q = %q, %t; hidden/absent target must be rejected", alias, got, ok)
			}
		}
		aliases := accountModelMappingAliases(account)
		if len(aliases) != 1 || aliases[0] != "gpt-visible" {
			t.Fatalf("visible aliases = %v, want [gpt-visible]", aliases)
		}
	})
}

func TestScopedModelsIncludesGPTAliasBackedByDefaultGrokModel(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1})
	store.AddAccount(&auth.Account{
		DBID:         1,
		UpstreamType: auth.UpstreamGrok,
		APIKey:       "test-key",
		ModelMapping: `{"gpt-5.5":"grok-4.5"}`,
	})
	handler := NewHandler(store, nil, nil, nil)
	models := listScopedModelsForTest(t, handler, &database.APIKeyRow{ID: 1})
	owner, _, ok := scopedModelByID(models, "gpt-5.5")
	if !ok || owner != "codex2api" {
		t.Fatalf("gpt-5.5 alias owner = %q, present=%t; models=%+v", owner, ok, models)
	}
	if !modelIDInList("gpt-5.5", handler.supportedModelIDs(context.Background())) {
		t.Fatal("legacy unscoped model list omitted the Grok-backed GPT alias")
	}
}

func TestGrokGPTAliasHonorsChannelAndTransportBoundaries(t *testing.T) {
	gin.SetMode(gin.TestMode)
	account := &auth.Account{
		UpstreamType: auth.UpstreamGrok,
		APIKey:       "test-key",
		ModelMapping: `{"gpt-5.5":"grok-4.5"}`,
	}
	baseFilter := accountFilterForResponsesModelWithOriginal("gpt-5.5", "gpt-5.5", false)
	if !baseFilter(account) {
		t.Fatal("ordinary HTTP Responses filter rejected the mapped Grok account")
	}

	channelFilter := func(channel string) auth.AccountFilter {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Set(contextAPIKeyRow, &database.APIKeyRow{
			ID:     1,
			Limits: database.APIKeyLimits{UpstreamChannel: channel},
		})
		return (&Handler{}).applyUpstreamChannelFilter(c, "gpt-5.5", baseFilter)
	}
	if !channelFilter(database.UpstreamChannelGrok)(account) {
		t.Fatal("grok-channel key rejected the mapped Grok account")
	}
	if channelFilter(database.UpstreamChannelCodex)(account) {
		t.Fatal("codex-channel key accepted a Grok account through its GPT alias")
	}
	if accountFilterForModel("gpt-5.5")(account) {
		t.Fatal("Responses WebSocket/Codex-only filter unexpectedly accepted Grok")
	}
	if accountFilterForCompactResponsesModelWithOriginal("gpt-5.5", "gpt-5.5", false)(account) {
		t.Fatal("/v1/responses/compact unexpectedly accepted Grok")
	}
}

func TestMappedGrokAliasExecutesAcrossHTTPProtocols(t *testing.T) {
	var capturedModels []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := readUpstreamRequestBody(r)
		if r.URL.Path != "/v1/responses" {
			t.Errorf("upstream path = %q, want /v1/responses", r.URL.Path)
		}
		capturedModels = append(capturedModels, gjson.GetBytes(body, "model").String())
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"status\":\"completed\",\"model\":\"grok-4.5\",\"output\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n")
	}))
	defer server.Close()

	account := &auth.Account{
		UpstreamType: auth.UpstreamGrok,
		APIKey:       "test-key",
		BaseURL:      server.URL + "/v1",
		ModelMapping: `{"gpt-5.5":"grok-4.5"}`,
	}
	account.SetGrokRoutingState(auth.GrokRoutingState{Models: []auth.GrokModelRoute{{
		ModelID: "grok-4.5", BaseURL: server.URL + "/v1", APIBackend: auth.GrokProtocolResponses,
	}}})
	handler := NewHandler(nil, nil, nil, nil)
	tests := []struct {
		name     string
		protocol GrokProtocol
		inbound  []byte
	}{
		{name: "responses", protocol: GrokProtocolResponses, inbound: []byte(`{"model":"gpt-5.5","input":"hello","stream":true}`)},
		{name: "chat completions", protocol: GrokProtocolChatCompletions, inbound: []byte(`{"model":"gpt-5.5","messages":[{"role":"user","content":"hello"}],"stream":true}`)},
		{name: "messages", protocol: GrokProtocolMessages, inbound: []byte(`{"model":"gpt-5.5","max_tokens":16,"messages":[{"role":"user","content":"hello"}],"stream":true}`)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			canonical := []byte(`{"model":"gpt-5.5","input":[{"role":"user","content":"hello"}],"stream":true}`)
			mappedBody, mappedModel, mapped := handler.applyAccountModelMappingToBody(canonical, account)
			if !mapped || mappedModel != "grok-4.5" {
				t.Fatalf("mapped model = %q, applied=%t", mappedModel, mapped)
			}
			resp, err := ExecuteRelayStyleProtocolRequest(context.Background(), account, test.protocol, test.inbound, mappedBody, "", nil)
			if err != nil {
				t.Fatalf("ExecuteRelayStyleProtocolRequest: %v", err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		})
	}
	if len(capturedModels) != len(tests) {
		t.Fatalf("captured %d requests, want %d", len(capturedModels), len(tests))
	}
	for index, model := range capturedModels {
		if model != "grok-4.5" {
			t.Fatalf("request %d upstream model = %q, want grok-4.5", index, model)
		}
	}
}
