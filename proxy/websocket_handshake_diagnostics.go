package proxy

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
)

// CaptureOutboundWebsocketHeaders retains only allowlisted, sanitized values.
// Credentials are never retained, even temporarily in the diagnostic snapshot.
func CaptureOutboundWebsocketHeaders(headers http.Header) *OutboundHeaderDiagnostic {
	diagnostic := CaptureOutboundIdentityHeaders(headers)
	for _, name := range []string{
		"Host", "Authorization", "Connection", "Upgrade", "Sec-WebSocket-Version",
		"Sec-WebSocket-Key", "Sec-WebSocket-Extensions", "Sec-WebSocket-Protocol",
		"X-Codex-Routing-Hint", "X-Oai-Attestation", "X-Responsesapi-Include-Timing-Metrics",
		"X-Resin-Account", "Chatgpt-Account-Id", "Session-Id", "Session_id", "Conversation-Id", "Conversation_id",
	} {
		values := headers.Values(name)
		if len(values) == 0 {
			continue
		}
		value := strings.TrimSpace(values[0])
		switch name {
		case "Authorization", "Sec-WebSocket-Key", "X-Oai-Attestation":
			if value == "" {
				value = "[empty]"
			} else {
				value = "[present]"
			}
		case "Chatgpt-Account-Id", "X-Resin-Account", "Session-Id", "Session_id", "Conversation-Id", "Conversation_id", "Sec-WebSocket-Protocol":
			if value != "" {
				value = "hash:" + hashRiskIdentity(value)
			}
		case "Host":
			parsed, err := url.Parse("//" + value)
			if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
				value = "[invalid]"
			} else {
				value = diagnosticClientText(parsed.Host)
			}
		default:
			value = diagnosticClientText(value)
		}
		diagnostic.Headers[name] = value
		if len(values) > 1 {
			diagnostic.Headers[name+"_multiple"] = "true"
		}
	}
	return diagnostic
}

func outboundComparableIdentity(value string) string {
	if strings.HasPrefix(value, "hash:") {
		return value
	}
	return "hash:" + hashRiskIdentity(value)
}

// TraceOutboundWebsocketHandshake observes library-generated headers on both
// transports, without wrapping sockets or changing the request. finish freezes
// the snapshot after Dial, including when a transport writer exits later.
func TraceOutboundWebsocketHandshake(ctx context.Context, prepared *OutboundHeaderDiagnostic) (context.Context, func() *OutboundHeaderDiagnostic) {
	var mu sync.Mutex
	var actual *OutboundHeaderDiagnostic
	var upgraded, finished bool
	trace := &httptrace.ClientTrace{
		WroteHeaderField: func(name string, values []string) {
			mu.Lock()
			defer mu.Unlock()
			if finished {
				return
			}
			// A proxy CONNECT may also invoke tracing. Each HTTP request starts
			// with Host; only retain the request that contains Upgrade: websocket.
			if actual == nil || strings.EqualFold(name, "Host") {
				actual = &OutboundHeaderDiagnostic{Headers: make(map[string]string), TurnMetadata: make(map[string]string), CaptureStage: "headers_partial"}
				upgraded = false
			}
			header := make(http.Header, 1)
			header[http.CanonicalHeaderKey(name)] = values
			if strings.EqualFold(name, "Upgrade") && strings.EqualFold(header.Get(name), "websocket") {
				upgraded = true
			}
			sanitized := CaptureOutboundWebsocketHeaders(header)
			for key, value := range sanitized.Headers {
				if _, exists := actual.Headers[key]; exists && !strings.HasSuffix(key, "_multiple") {
					actual.Headers[key+"_multiple"] = "true"
				} else {
					actual.Headers[key] = value
				}
			}
			maps.Copy(actual.TurnMetadata, sanitized.TurnMetadata)
		},
		WroteHeaders: func() {
			mu.Lock()
			defer mu.Unlock()
			if !finished && actual != nil {
				actual.CaptureStage = "headers_serialized"
			}
		},
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			mu.Lock()
			defer mu.Unlock()
			if !finished && actual != nil {
				actual.CaptureStage = "write_failed"
				if info.Err == nil {
					actual.CaptureStage = "written"
				}
			}
		},
	}
	return httptrace.WithClientTrace(ctx, trace), func() *OutboundHeaderDiagnostic {
		mu.Lock()
		defer mu.Unlock()
		finished = true
		if upgraded {
			return actual
		}
		return prepared
	}
}
