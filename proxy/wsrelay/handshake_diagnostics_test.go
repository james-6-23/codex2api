package wsrelay

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/proxy"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func TestWebsocketHandshakeDiagnosticMatchesTransport(t *testing.T) {
	for _, takeover := range []bool{false, true} {
		for _, reject := range []bool{false, true} {
			for _, suppressUA := range []bool{false, true} {
				t.Run(fmt.Sprintf("takeover=%t/reject=%t/suppressUA=%t", takeover, reject, suppressUA), func(t *testing.T) {
					received := make(chan *http.Request, 1)
					server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						received <- r.Clone(context.Background())
						if reject {
							http.Error(w, "fixture rejected", http.StatusForbidden)
							return
						}
						connection, err := (&websocket.Upgrader{EnableCompression: true}).Upgrade(w, r, nil)
						if err == nil {
							defer connection.Close()
							_, _, _ = connection.ReadMessage()
						}
					}))
					defer server.Close()
					headers := http.Header{}
					for name, value := range map[string]string{
						"Host": "override.example", "Authorization": "Bearer private-fixture-token",
						"User-Agent": "codex_cli_rs/1", "Version": "1", "Originator": "codex_cli_rs",
						"OpenAI-Beta": "responses_websockets=2026-02-06", "X-Codex-Beta-Features": "remote_compaction_v2",
						"Chatgpt-Account-Id": "private-fixture-account", "Session-Id": "private-fixture-session",
						"X-Codex-Routing-Hint": "model=gpt-5;tier=priority", "X-Oai-Attestation": "private-fixture-proof",
						"X-Responsesapi-Include-Timing-Metrics": "true", "X-Resin-Account": "private-fixture-resin",
					} {
						headers.Set(name, value)
					}
					if suppressUA {
						headers.Set("User-Agent", "")
					}
					prepared := proxy.CaptureOutboundWebsocketHeaders(headers)
					prepared.CaptureStage = "prepared"
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					defer cancel()
					ctx, finish := proxy.TraceOutboundWebsocketHandshake(ctx, prepared)
					dialer := &websocket.Dialer{HandshakeTimeout: time.Second, EnableCompression: true, Subprotocols: []string{"fixture-protocol"}}
					var connection upstreamWebsocket
					var response *http.Response
					var err error
					wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
					if takeover {
						connection, response, err = dialContextTakeover(ctx, dialer, wsURL, headers)
					} else {
						connection, response, err = dialer.DialContext(ctx, wsURL, headers)
					}
					captured := finish()
					if reject {
						require.Error(t, err)
						require.NotNil(t, response)
						require.Equal(t, http.StatusForbidden, response.StatusCode)
						defer response.Body.Close()
					} else {
						require.NoError(t, err)
						defer connection.Close()
					}
					var actual *http.Request
					select {
					case actual = <-received:
					case <-ctx.Done():
						t.Fatal("handshake was not received")
					}
					expected := actual.Header.Clone()
					expected.Set("Host", actual.Host)
					require.Equal(t, proxy.CaptureOutboundWebsocketHeaders(expected).Headers, captured.Headers)
					require.Equal(t, "written", captured.CaptureStage)
					if suppressUA {
						require.NotContains(t, captured.Headers, "User-Agent")
					}
					extension := "permessage-deflate; server_no_context_takeover; client_no_context_takeover"
					if takeover {
						extension = "permessage-deflate; client_max_window_bits"
					}
					require.Equal(t, extension, captured.Headers["Sec-WebSocket-Extensions"])
					encoded, err := json.Marshal(captured)
					require.NoError(t, err)
					require.NotContains(t, string(encoded), "private-fixture")
					require.NotContains(t, string(encoded), actual.Header.Get("Sec-WebSocket-Key"))
				})
			}
		}
	}
}
