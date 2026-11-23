package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/config"
	"github.com/codex2api/database"
)

func TestPromptTraceDeliveryKeepsRawArchiveLocalAndPreservesSession(t *testing.T) {
	ctx := context.Background()
	db, err := database.New("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	start := time.Now().UTC().Add(-time.Minute)
	insert := func(offset time.Duration, body string) int64 {
		t.Helper()
		id, err := db.InsertPromptRequest(ctx, database.PromptRequestBody{
			PromptRequestRecord: database.PromptRequestRecord{
				CreatedAt: start.Add(offset), Endpoint: "/v1/responses", Transport: "http",
				Model: "gpt-6.1-sol", APIKey: "PRIVATE_KEY_LABEL",
			},
			ContentType: "application/json", Body: []byte(body),
			ConversationGroup: database.PromptRequestConversationGroup([]byte(body), 8),
		})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	first := insert(0, `{"client_metadata":{"thread_id":"PRIVATE_THREAD_MARKER"},"input":[{"role":"developer","content":"PRIVATE_DEV_PROMPT"},{"role":"user","content":"PRIVATE_USER_PROMPT"}]}`)
	second := insert(time.Second, `{"client_metadata":{"thread_id":"PRIVATE_THREAD_MARKER"},"input":[{"role":"developer","content":"PRIVATE_DEV_PROMPT"},{"role":"user","content":"PRIVATE_USER_PROMPT"},{"type":"function_call","call_id":"PRIVATE_CALL_ID","name":"PRIVATE_TOOL_ID","arguments":"PRIVATE_ARGS"},{"type":"function_call_output","call_id":"PRIVATE_CALL_ID","output":"PRIVATE_RESULT"}]}`)

	requestPayloads := make(chan []byte, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, password, basic := r.BasicAuth()
		if !basic || username != "public" || password != "private" ||
			r.URL.Path != "/api/public/otel/v1/traces" || r.Method != http.MethodPost ||
			r.Header.Get("x-langfuse-ingestion-version") != "4" {
			t.Error("invalid OTLP contract")
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error("reading OTLP request")
		}
		requestPayloads <- body
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, "{}")
	}))
	defer server.Close()
	cfg := config.PromptTraceExportConfig{
		BaseURL: server.URL, PublicKey: "public", SecretKey: "private", ProjectID: "sample",
		ViewURL: "https://trace.example", SourceURL: "https://gateway.example",
	}
	exporter := newPromptTraceExporter(cfg)
	h := &Handler{db: db, promptTraceExporter: exporter}
	if err := db.EnqueuePromptRequestTraceBackfill(ctx, exporter.destination, []int64{first}, start); err != nil {
		t.Fatal(err)
	}
	if err := db.CompletePromptRequestTrace(ctx, second, "allow", 200, database.PromptRequestResponse{
		Status: "interrupted", Text: "PRIVATE_REPLY", Error: "PRIVATE_ERROR",
		ToolCalls: []database.PromptRequestResponseCall{{Name: "PRIVATE_TOOL_NAME", Arguments: "PRIVATE_MODEL_ARGS"}},
	}, start.Add(3*time.Second), exporter.destination, database.PromptRequestTraceMetrics{
		TotalAttempts: 1, Attempts: []database.PromptRequestTraceAttempt{{
			FinishedAt: start.Add(2 * time.Second), Model: "gpt-6.1-sol", StatusCode: 499,
			InputTokens: 56, OutputTokens: 17, CachedTokens: 21, ReasoningTokens: 8, TotalTokens: 73,
			FirstTokenMs: 250, DurationMs: 1000, Stream: true,
		}},
	}); err != nil {
		t.Fatal(err)
	}

	var roots []map[string]string
	now := time.Now().UTC()
	for _, id := range []int64{first, second} {
		job, err := db.ClaimPromptRequestTrace(ctx, exporter.destination, now, now.Add(time.Minute))
		if err != nil || job == nil || job.RequestID != id {
			t.Fatalf("claim for %d failed: %#v %v", id, job, err)
		}
		if err := exporter.process(ctx, h, job); err != nil {
			t.Fatal(err)
		}
		raw := <-requestPayloads
		if strings.Contains(string(raw), "PRIVATE_") {
			t.Fatal("OTLP request contained private archive content")
		}
		var payload otlpTracePayload
		if err := json.Unmarshal(raw, &payload); err != nil {
			t.Fatal(err)
		}
		spans := payload.ResourceSpans[0].ScopeSpans[0].Spans
		root := promptTraceSpanStringAttrs(spans[0])
		roots = append(roots, root)
		if root["langfuse.trace.metadata.request_id"] != fmt.Sprint(id) ||
			root["langfuse.trace.name"] != fmt.Sprintf("Codex2API #%d", id) ||
			root["langfuse.trace.metadata.source_url"] != fmt.Sprintf(`"https://gateway.example/admin/prompt-filter/requests?request=%d"`, id) ||
			h.promptRequestTraceURL(ctx, id) != "https://trace.example/project/sample/traces/"+spans[0].TraceID {
			t.Fatal("trace is not correlated with the source request")
		}
		if id == first && root["langfuse.trace.metadata.response_status"] != `"not_recorded"` {
			t.Fatal("historical response was invented")
		}
		if id == second {
			if root["langfuse.trace.metadata.response_status"] != `"interrupted"` ||
				root["langfuse.trace.metadata.historical_items"] != "2" {
				t.Fatal("missing response status or input delta")
			}
			generation := promptTraceSpanStringAttrs(spans[1])
			if generation["langfuse.observation.type"] != "generation" ||
				generation["langfuse.observation.usage_details"] != `{"input":35,"input_cached_tokens":21,"output":9,"output_reasoning_tokens":8,"total":73}` {
				t.Fatal("expected observed token buckets")
			}
		}
	}
	if roots[0]["session.id"] == "" || roots[0]["session.id"] != roots[1]["session.id"] ||
		roots[0]["session.id"] == "PRIVATE_THREAD_MARKER" {
		t.Fatal("trace session is missing or inconsistent")
	}
}

func promptTraceSpanStringAttrs(span otlpSpan) map[string]string {
	attrs := make(map[string]string, len(span.Attributes))
	for _, attr := range span.Attributes {
		if value, ok := attr.Value["stringValue"].(string); ok {
			attrs[attr.Key] = value
		}
	}
	return attrs
}

func TestPromptTraceOTLPPartialSuccessAndRedirects(t *testing.T) {
	hitRedirect := make(chan struct{}, 1)
	var state sync.Mutex
	redirect := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/unexpected" {
			hitRedirect <- struct{}{}
			return
		}
		state.Lock()
		moved := redirect
		state.Unlock()
		if moved {
			http.Redirect(w, r, "/unexpected", http.StatusTemporaryRedirect)
			return
		}
		_, _ = io.WriteString(w, `{"partialSuccess":{"rejectedSpans":"1"}}`)
	}))
	defer server.Close()
	e := newPromptTraceExporter(config.PromptTraceExportConfig{BaseURL: server.URL, PublicKey: "public", SecretKey: "private"})
	if err := e.send(context.Background(), otlpTracePayload{}); err == nil {
		t.Fatal("partial rejection must retry")
	}
	state.Lock()
	redirect = true
	state.Unlock()
	if err := e.send(context.Background(), otlpTracePayload{}); err == nil {
		t.Fatal("redirect should fail without following")
	}
	select {
	case <-hitRedirect:
		t.Fatal("followed a credential-bearing redirect")
	default:
	}
}
