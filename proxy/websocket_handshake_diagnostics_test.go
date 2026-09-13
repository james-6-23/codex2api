package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptrace"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestWebsocketHandshakeDiagnosticRedactionAndPersistence(t *testing.T) {
	const session = "31811466-ec40-4690-b07b-b4828d3095ff"
	headers := http.Header{}
	for name, value := range map[string]string{
		"Authorization": "Bearer fixture-private-token", "Cookie": "fixture-private-cookie",
		"Proxy-Authorization": "fixture-private-proxy", "X-Oai-Attestation": "fixture-private-proof",
		"Sec-WebSocket-Key": "fixture-private-nonce", "Sec-WebSocket-Protocol": "fixture-private-protocol",
		"Chatgpt-Account-Id": "41811466-ec40-4690-b07b-b4828d3095ff", "X-Resin-Account": "fixture-resin-account",
		"Session-Id": session, "Host": "chatgpt.com", "Upgrade": "websocket",
		"X-Codex-Routing-Hint": "model=gpt-5;tier=priority", "X-Responsesapi-Include-Timing-Metrics": "true",
	} {
		headers.Set(name, value)
	}
	headers.Add("Sec-WebSocket-Protocol", "second-private-protocol")
	diagnostic := CaptureOutboundWebsocketHeaders(headers)
	diagnostic.CaptureStage = "written"
	for _, name := range []string{"Authorization", "Sec-WebSocket-Key", "X-Oai-Attestation"} {
		require.Equal(t, "[present]", diagnostic.Headers[name])
	}
	for _, name := range []string{"Session-Id", "Chatgpt-Account-Id", "X-Resin-Account", "Sec-WebSocket-Protocol"} {
		require.Equal(t, "hash:"+hashRiskIdentity(headers.Get(name)), diagnostic.Headers[name])
	}
	request := transportTestContext()
	beginUpstreamTrace(request.Request.Context(), &auth.Account{DBID: 17}, "", true)
	observer := UpstreamTransportObserver(request.Request.Context())
	observer.OutboundWebsocketHandshake(diagnostic)
	observer.ResponsesInput([]byte(`{"client_metadata":{"session_id":"`+session+`"},"input":[]}`), nil, "")
	identity := snapshotUpstreamTrace(request.Request.Context()).Transport.OutboundIdentity
	require.Equal(t, "matched", identity.SessionConsistency)
	observer.ResponsesInput([]byte(`{"client_metadata":{"session_id":"different-session"},"input":[]}`), nil, "")
	require.Equal(t, "mismatched", snapshotUpstreamTrace(request.Request.Context()).Transport.OutboundIdentity.SessionConsistency)
	usage := &database.UsageLogInput{AccountID: 17, StatusCode: 200}
	populateUpstreamTrace(request, usage)
	populateUsageRequestDiagnostics(request, usage)
	require.Equal(t, "written", gjson.Get(usage.RequestDiagnostics, "upstream.outbound_identity.ws_handshake.capture_stage").String())
	encoded, err := json.Marshal(diagnostic)
	require.NoError(t, err)
	for _, forbidden := range []string{"fixture-private", "second-private", "fixture-resin-account", session, headers.Get("Chatgpt-Account-Id"), "Cookie", "Proxy-Authorization"} {
		require.NotContains(t, string(encoded), forbidden)
	}
	var restored OutboundHeaderDiagnostic
	require.NoError(t, json.Unmarshal(encoded, &restored))
	require.Equal(t, diagnostic.Headers, restored.Headers)
	require.Equal(t, "written", restored.CaptureStage)
	diagnostic.CaptureStage = "changed"
	require.Equal(t, "written", identity.WSHandshake.CaptureStage)
	headers.Set("Session-Id", "")
	observer.OutboundWebsocketHandshake(CaptureOutboundWebsocketHeaders(headers))
	require.Equal(t, "missing_header", snapshotUpstreamTrace(request.Request.Context()).Transport.OutboundIdentity.SessionConsistency)
}

func TestWebsocketHandshakeTraceStagesAndFreeze(t *testing.T) {
	for _, stage := range []string{"prepared", "headers_partial", "headers_serialized", "write_failed", "written"} {
		t.Run(stage, func(t *testing.T) {
			prepared := CaptureOutboundWebsocketHeaders(http.Header{"User-Agent": {"prepared/1"}})
			prepared.CaptureStage = "prepared"
			calls := 0
			ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{WroteHeaderField: func(string, []string) { calls++ }})
			ctx, finish := TraceOutboundWebsocketHandshake(ctx, prepared)
			trace := httptrace.ContextClientTrace(ctx)
			// Ignore proxy CONNECT observations, including on a failed connection.
			trace.WroteHeaderField("Host", []string{"proxy.example"})
			trace.WroteRequest(httptrace.WroteRequestInfo{})
			if stage != "prepared" {
				trace.WroteHeaderField("Host", []string{"target.example"})
				trace.WroteHeaderField("Upgrade", []string{"websocket"})
				if stage != "headers_partial" {
					trace.WroteHeaders()
				}
				if stage == "written" {
					trace.WroteRequest(httptrace.WroteRequestInfo{})
				} else if stage == "write_failed" {
					trace.WroteRequest(httptrace.WroteRequestInfo{Err: errors.New("fixture failure")})
				}
			}
			captured := finish()
			require.Equal(t, stage, captured.CaptureStage)
			require.Positive(t, calls)
			before, err := json.Marshal(captured)
			require.NoError(t, err)
			trace.WroteHeaderField("Host", []string{"late.example"})
			trace.WroteRequest(httptrace.WroteRequestInfo{})
			after, err := json.Marshal(captured)
			require.NoError(t, err)
			require.Equal(t, string(before), string(after))
		})
	}
}
