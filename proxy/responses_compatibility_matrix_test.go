package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// This fixture exercises the shared TokenCache contract used by Redis. It is
// intentionally in-memory: it proves gateway fallback semantics, not Redis
// server persistence, wire encoding, failover, or deployment readiness.
type compatibilityMatrixSharedCache struct {
	cache.TokenCache
	gets, sets atomic.Int32
}

func (*compatibilityMatrixSharedCache) SharedAcrossInstances() bool { return true }
func (f *compatibilityMatrixSharedCache) SetResponseContext(ctx context.Context, key string, items []json.RawMessage, ttl time.Duration) error {
	f.sets.Add(1)
	return f.TokenCache.SetResponseContext(ctx, key, items, ttl)
}
func (f *compatibilityMatrixSharedCache) GetResponseContext(ctx context.Context, key string) ([]json.RawMessage, error) {
	f.gets.Add(1)
	return f.TokenCache.GetResponseContext(ctx, key)
}

func TestResponsesCompatibilityMatrixNamedToolRoundTrips(t *testing.T) {
	started := make(chan string, 2)
	release := make(chan struct{})
	var turns atomic.Int32
	client := newResponsesWSMultiplexClient(t, func(ctx context.Context, body []byte) (*http.Response, error) {
		lane := gjson.GetBytes(body, "stream_id").String()
		if lane != "planner" && lane != "research" {
			return nil, fmt.Errorf("unexpected lane: %s", body)
		}
		turns.Add(1)
		if previous := gjson.GetBytes(body, "previous_response_id").String(); previous != "" {
			if previous != "resp_"+lane+"_tool" || gjson.GetBytes(body, "input.#").Int() != 2 {
				t.Errorf("tool continuation lost pairing, duplicated history or crossed lanes: %s", body)
			}
			for index := range 2 {
				item := gjson.GetBytes(body, fmt.Sprintf("input.%d", index))
				if item.Get("type").String() != "function_call_output" || item.Get("call_id").String() != fmt.Sprintf("call_%s_%d", lane, index) || item.Get("output").String() != fmt.Sprintf("result_%s_%d", lane, index) {
					t.Errorf("parallel tool call/result pairing crossed lanes: %s", body)
				}
			}
			return responsesWSMultiplexResponse("resp_" + lane + "_answer"), nil
		}
		if !gjson.GetBytes(body, "tools.0.strict").Bool() || !gjson.GetBytes(body, "parallel_tool_calls").Bool() || gjson.GetBytes(body, "tool_choice").String() != "auto" {
			t.Errorf("tool declaration/choice/parallel semantics changed: %s", body)
		}
		started <- lane
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		firstCall := fmt.Sprintf(`{"type":"function_call","call_id":%q,"name":"lookup","arguments":"{}"}`, "call_"+lane+"_0")
		secondCall := fmt.Sprintf(`{"type":"function_call","call_id":%q,"name":"lookup","arguments":"{}"}`, "call_"+lane+"_1")
		return nativeWSResponse("resp_"+lane+"_tool", firstCall, secondCall), nil
	})
	for _, lane := range []string{"planner", "research"} {
		if err := client.WriteJSON(map[string]any{
			"type": "response.create", "model": "gpt-5.5", "store": false, "stream_id": lane, "input": "lookup_" + lane,
			"parallel_tool_calls": true, "tool_choice": "auto",
			"tools": []any{map[string]any{"type": "function", "name": "lookup", "strict": true, "parameters": map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]bool{}
	for len(seen) != 2 {
		select {
		case lane := <-started:
			seen[lane] = true
		case <-time.After(5 * time.Second):
			t.Fatal("independent tool lanes failed to overlap")
		}
	}
	close(release)
	readBoth := func(suffix string) {
		completed := map[string]bool{}
		for len(completed) != 2 {
			frame := responsesWSMultiplexRead(t, client)
			lane := gjson.GetBytes(frame, "stream_id").String()
			if lane != "planner" && lane != "research" {
				t.Fatalf("unscoped/crossed event: %s", frame)
			}
			if gjson.GetBytes(frame, "type").String() == "error" {
				t.Fatalf("tool round-trip rejected: %s", frame)
			}
			if gjson.GetBytes(frame, "type").String() == "response.completed" {
				if gjson.GetBytes(frame, "response.id").String() != "resp_"+lane+suffix {
					t.Fatalf("wrong response ownership: %s", frame)
				}
				completed[lane] = true
			}
		}
	}
	readBoth("_tool")
	for _, lane := range []string{"planner", "research"} {
		if err := client.WriteJSON(map[string]any{"type": "response.create", "model": "gpt-5.5", "store": false, "stream_id": lane, "previous_response_id": "resp_" + lane + "_tool", "input": []any{map[string]any{"type": "function_call_output", "call_id": "call_" + lane + "_0", "output": "result_" + lane + "_0"}, map[string]any{"type": "function_call_output", "call_id": "call_" + lane + "_1", "output": "result_" + lane + "_1"}}}); err != nil {
			t.Fatal(err)
		}
	}
	readBoth("_answer")
	if turns.Load() != 4 {
		t.Fatalf("tool round-trip upstream calls=%d, want 4", turns.Load())
	}
}

// Compare native local replay with relay server-side continuation, both before
// and after L1 state loss. The shared fixture survives the L1 reset; a plain
// memory backend must return an actionable 409 instead of forwarding orphaned
// tool results. All actual upstream requests go to the loopback HTTP server.
func TestResponsesCompatibilityMatrixHTTPContinuationStorage(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previous := CurrentRuntimeSettings()
	settings := DefaultRuntimeSettings()
	settings.CodexForceWebsocket = false
	settings.ContinuousRetryPolicy = database.ContinuousRetryPolicy{}
	ApplyRuntimeSettings(settings)
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })
	for _, shared := range []bool{false, true} {
		for _, native := range []bool{true, false} {
			for _, loseL1 := range []bool{false, true} {
				t.Run(fmt.Sprintf("shared=%v/native=%v/L1lost=%v", shared, native, loseL1), func(t *testing.T) {
					resetResponseCacheStateForTest(testResponseCacheConfig())
					defer resetResponseCacheForTest()
					memory := cache.NewMemory(1)
					defer memory.Close()
					var backend cache.TokenCache = memory
					fixture := &compatibilityMatrixSharedCache{TokenCache: memory}
					if shared {
						backend = fixture
					}
					SetResponseContextCache(backend)
					defer SetResponseContextCache(nil)
					requests := make(chan []byte, 3)
					var calls atomic.Int32
					upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						body := readUpstreamRequestBody(r)
						requests <- body
						w.Header().Set("Content-Type", "text/event-stream")
						if calls.Add(1) == 1 {
							_, _ = io.WriteString(w, wsContextTestSSE("resp_matrix_tool", `{"type":"function_call","call_id":"call_matrix","name":"lookup","arguments":"{}"}`))
						} else {
							_, _ = io.WriteString(w, wsContextTestSSE("resp_matrix_answer"))
						}
					}))
					defer upstream.Close()
					oldResin := resinCfg.Load()
					SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "matrix"})
					defer resinCfg.Store(oldResin)
					store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, MaxRetries: 0, MaxRateLimitRetries: 0})
					defer store.Stop()
					account := &auth.Account{DBID: 1, AccessToken: "synthetic-matrix", PlanType: "pro"}
					if !native {
						account.UpstreamType = auth.UpstreamOpenAIResponses
						account.BaseURL = upstream.URL
						account.APIKey = "synthetic-matrix"
						account.PlanType = "api"
						account.Models = []string{"gpt-5.5"}
					}
					store.AddAccount(account)
					h := NewHandler(store, nil, nil, nil)
					request := func(body []byte, owner int64) *httptest.ResponseRecorder {
						return invokeResponsesHandlerWithContext(t, func(c *gin.Context) {
							c.Set(contextAPIKeyID, owner)
							c.Set(contextAPIKeyRow, &database.APIKeyRow{ID: owner})
						}, h.Responses, body)
					}
					first := request([]byte(`{"type":"response.create","model":"gpt-5.5","stream":true,"stream_id":"http-only-envelope","input":[{"type":"message","role":"user","stream_id":"nested-input","content":"lookup"}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"stream_id":{"type":"string"}}}}]}`), 7)
					if first.Code != 200 || !strings.Contains(first.Body.String(), "response.completed") {
						t.Fatalf("initial tool turn=%d %s", first.Code, first.Body.String())
					}
					firstBody := <-requests
					if gjson.GetBytes(firstBody, "stream_id").Exists() || gjson.GetBytes(firstBody, "type").Exists() || gjson.GetBytes(firstBody, "tools.0.parameters.properties.stream_id.type").String() != "string" || gjson.GetBytes(firstBody, "input.0.stream_id").String() != "nested-input" || gjson.GetBytes(firstBody, "input.0.type").String() != "message" {
						t.Fatalf("HTTP envelope cleanup removed nested data or retained stream_id: %s", firstBody)
					}
					drainResponseCacheBackendWrites()
					if native && shared && fixture.sets.Load() == 0 {
						t.Fatal("native tool state never reached shared backend fixture")
					}
					if loseL1 {
						resetResponseCacheStateForTest(testResponseCacheConfig())
						SetResponseContextCache(backend)
					}
					secondRaw := []byte(`{"model":"gpt-5.5","stream":true,"stream_id":"http-only-envelope","previous_response_id":"resp_matrix_tool","input":[{"type":"function_call_output","call_id":"call_matrix","output":"local-result"}]}`)
					// A native replay belongs to the original API key, including when
					// a shared backend rather than L1 supplies the snapshot.
					if native {
						wrongOwner := request(secondRaw, 8)
						if wrongOwner.Code != 409 || calls.Load() != 1 {
							t.Fatalf("cross-owner replay was not rejected: %d %s calls=%d", wrongOwner.Code, wrongOwner.Body.String(), calls.Load())
						}
					}
					second := request(secondRaw, 7)
					if native && loseL1 && !shared {
						if second.Code != 409 || gjson.GetBytes(second.Body.Bytes(), "error.code").String() != "previous_response_not_found" || calls.Load() != 1 {
							t.Fatalf("lost memory history did not fail closed: %d %s calls=%d", second.Code, second.Body.String(), calls.Load())
						}
					} else {
						if second.Code != 200 || calls.Load() != 2 {
							t.Fatalf("continuation=%d %s calls=%d", second.Code, second.Body.String(), calls.Load())
						}
						sent := <-requests
						if gjson.GetBytes(sent, "stream_id").Exists() {
							t.Fatalf("HTTP leaked stream_id: %s", sent)
						}
						if native {
							if gjson.GetBytes(sent, "previous_response_id").Exists() || gjson.GetBytes(sent, "input.#").Int() != 3 || gjson.GetBytes(sent, "input.1.call_id").String() != "call_matrix" || gjson.GetBytes(sent, "input.2.output").String() != "local-result" {
								t.Fatalf("native tool call/result expansion changed: %s", sent)
							}
						} else if gjson.GetBytes(sent, "previous_response_id").String() != "resp_matrix_tool" || gjson.GetBytes(sent, "input.#").Int() != 1 {
							t.Fatalf("relay lost server continuation or repeated history: %s", sent)
						}
					}
					if account.ActiveRequests.Load() != 0 {
						t.Fatal("continuation leaked account capacity")
					}
				})
			}
		}
	}
}

func TestResponsesCompatibilityMatrixCompactToolContinuation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	old := CurrentRuntimeSettings()
	settings := DefaultRuntimeSettings()
	settings.CompactViaResponses = false
	settings.ContinuousRetryPolicy = database.ContinuousRetryPolicy{}
	ApplyRuntimeSettings(settings)
	t.Cleanup(func() { ApplyRuntimeSettings(old) })
	const compactOutput = `{"id":"resp_matrix_compact","object":"response.compaction","output":[{"type":"compaction","encrypted_content":"synthetic-opaque-context"}],"usage":{"input_tokens":2,"output_tokens":1}}`
	for _, native := range []bool{true, false} {
		t.Run(fmt.Sprintf("native=%v", native), func(t *testing.T) {
			resetResponseCacheStateForTest(testResponseCacheConfig())
			defer resetResponseCacheForTest()
			setResponseCache("key:7", "resp_before_compact", []json.RawMessage{json.RawMessage(`{"role":"user","content":"lookup"}`), json.RawMessage(`{"type":"function_call","call_id":"compact_call","name":"lookup","arguments":"{}"}`)})
			requestBodies := make(chan []byte, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requestBodies <- readUpstreamRequestBody(r)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, compactOutput)
			}))
			defer upstream.Close()
			oldResin := resinCfg.Load()
			SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "compact-matrix"})
			defer resinCfg.Store(oldResin)
			store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, MaxRetries: 0, MaxRateLimitRetries: 0})
			defer store.Stop()
			account := &auth.Account{DBID: 1, AccessToken: "synthetic-compact", PlanType: "pro"}
			if !native {
				account.UpstreamType = auth.UpstreamOpenAIResponses
				account.BaseURL = upstream.URL
				account.APIKey = "synthetic-compact"
				account.PlanType = "api"
				account.Models = []string{"gpt-5.5"}
			}
			store.AddAccount(account)
			h := NewHandler(store, nil, nil, nil)
			raw := []byte(`{"model":"gpt-5.5","type":"response.create","stream_id":"http-envelope","previous_response_id":"resp_before_compact","input":[{"type":"function_call_output","call_id":"compact_call","output":"result"}],"metadata":{"owner":"retained-by-relay"}}`)
			response := invokeResponsesHandlerWithContext(t, func(c *gin.Context) {
				c.Set(contextAPIKeyID, int64(7))
				c.Set(contextAPIKeyRow, &database.APIKeyRow{ID: 7})
			}, h.ResponsesCompact, raw)
			if response.Code != 200 || gjson.GetBytes(response.Body.Bytes(), "output.0.encrypted_content").String() != "synthetic-opaque-context" {
				t.Fatalf("compact output=%d %s", response.Code, response.Body.String())
			}
			sent := <-requestBodies
			if gjson.GetBytes(sent, "type").Exists() || gjson.GetBytes(sent, "stream_id").Exists() {
				t.Fatalf("compact HTTP leaked envelope: %s", sent)
			}
			if native {
				if gjson.GetBytes(sent, "previous_response_id").Exists() || gjson.GetBytes(sent, "input.#").Int() != 3 || gjson.GetBytes(sent, "metadata").Exists() {
					t.Fatalf("native compact replay=%s", sent)
				}
			} else if gjson.GetBytes(sent, "previous_response_id").String() != "resp_before_compact" || gjson.GetBytes(sent, "input.#").Int() != 1 || gjson.GetBytes(sent, "metadata.owner").String() != "retained-by-relay" {
				t.Fatalf("relay compact semantics=%s", sent)
			}
			if account.ActiveRequests.Load() != 0 {
				t.Fatal("compact leaked account capacity")
			}
		})
	}
}
