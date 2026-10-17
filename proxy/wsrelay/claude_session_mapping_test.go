package wsrelay

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
)

func TestMessagesCodexMappingSessionCacheIsolation(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	previousRuntime := proxy.CurrentRuntimeSettings()
	previousResin := proxy.GetResinConfig()
	previousWS := proxy.WebsocketExecuteFunc
	t.Cleanup(func() {
		proxy.ApplyRuntimeSettings(previousRuntime)
		proxy.SetResinConfig(previousResin)
		proxy.WebsocketExecuteFunc = previousWS
	})

	for _, mode := range []string{proxy.RequestIsolationModeIsolated, proxy.RequestIsolationModePerAPIKey} {
		for _, tc := range []struct {
			name            string
			model           string
			upstream        string
			header          bool
			instructionRole string
		}{
			{"header", "claude-sonnet-4-6", "gpt-5.5", true, "system"},
			{"metadata", "claude-sonnet-4-6", "gpt-5.5", false, "developer"},
			{"suffixed alias header", "claude-sonnet-4-6-custom", "gpt-5.6-sol", true, "developer"},
			{"suffixed alias metadata", "claude-sonnet-4-6-custom", "gpt-5.6-sol", false, "system"},
		} {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				frames := make(chan []byte, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
					if err != nil {
						t.Error(err)
						return
					}
					defer conn.Close()
					for {
						_, frame, err := conn.ReadMessage()
						if err != nil {
							return
						}
						frames <- frame
						if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`)); err != nil {
							return
						}
					}
				}))
				t.Cleanup(server.Close)
				proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: server.URL, PlatformName: "test"})
				manager := NewManager()
				t.Cleanup(manager.Stop)
				executor := NewExecutorWithManager(manager)
				proxy.WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, body []byte, sessionID, proxyURL, apiKey string, deviceCfg *proxy.DeviceProfileConfig, headers http.Header, poolRouteKey string) (*http.Response, error) {
					response, err := executor.ExecuteRequestViaWebsocket(ctx, account, body, sessionID, proxyURL, apiKey, deviceCfg, headers, poolRouteKey)
					if err != nil {
						return nil, err
					}
					return websocketResponseToHTTP(ctx, response, http.StatusOK, nil), nil
				}
				store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
				t.Cleanup(store.Stop)
				store.AddAccount(&auth.Account{DBID: 913421, AccessToken: "test-token", Status: auth.StatusReady})
				store.SetModelMapping(`{"claude-sonnet-4-6-custom":"gpt-5.6-sol"}`)
				handler := proxy.NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
				runtime := previousRuntime
				runtime.CodexForceWebsocket = true
				runtime.RequestIsolationMode = mode
				proxy.ApplyRuntimeSettings(runtime)

				var stableInput string
				turn := 0
				invoke := func(apiKeyID int64, session string) string {
					t.Helper()
					turn++
					bodySession := session
					if tc.header {
						bodySession = "ignored-metadata-session"
					}
					body := fmt.Sprintf(`{
						"model":%q,"max_tokens":32,
						"system":[{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.261.test; cch=turn-%d;"},{"type":"text","text":"Follow CLAUDE.md. Working directory: D:/project"}],
						"messages":[{"role":"user","content":[
							{"type":"text","text":"<system-reminder>\nYou are powered by the model turn-%d.\n</system-reminder>"},
							{"type":"text","text":"<system-reminder>\n# Environment\nGit status: modified\nAvailable skills: project-build\n</system-reminder>"},
							{"type":"document","title":"Specification","source":{"type":"text","media_type":"text/plain","data":"Keep the required feature."}},
							{"type":"text","text":"hello"}
						]},{"role":%q,"content":"Keep the resumed project instructions."}],"metadata":{"user_id":%q}
					}`, tc.model, turn, turn, tc.instructionRole, fmt.Sprintf(`{"session_id":%q}`, bodySession))
					recorder := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(recorder)
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)).WithContext(ctx)
					c.Set("apiKeyID", apiKeyID)
					c.Request.Header.Set("Authorization", fmt.Sprintf("Bearer test-key-%d", apiKeyID))
					if tc.header {
						c.Request.Header.Set("X-Claude-Code-Session-Id", session)
					}
					handler.Messages(c)
					if recorder.Code != http.StatusOK {
						t.Fatalf("Messages status=%d body=%s", recorder.Code, recorder.Body.String())
					}
					select {
					case frame := <-frames:
						input := gjson.GetBytes(frame, "input").Raw
						if strings.Contains(input, "x-anthropic-billing-header:") || strings.Contains(input, "You are powered by the model") {
							t.Fatalf("transport metadata reached the upstream frame: %s", input)
						}
						for _, required := range []string{"CLAUDE.md", "D:/project", "Git status: modified", "project-build", "Specification", "Keep the required feature."} {
							if !strings.Contains(input, required) {
								t.Fatalf("necessary context %q was lost from the upstream frame: %s", required, input)
							}
						}
						instruction := gjson.GetBytes(frame, "input").Array()
						last := instruction[len(instruction)-1]
						if last.Get("role").String() != "developer" || last.Get("content.0.text").String() != "Keep the resumed project instructions." {
							t.Fatalf("instruction message lost its role or content: %s", input)
						}
						if stableInput == "" {
							stableInput = input
						} else if input != stableInput {
							t.Fatalf("transport-only changes changed the upstream prefix:\n%s\n%s", stableInput, input)
						}
						if got := gjson.GetBytes(frame, "model").String(); got != tc.upstream {
							t.Fatalf("mapped model = %q, want %q", got, tc.upstream)
						}
						cacheKey := gjson.GetBytes(frame, "prompt_cache_key").String()
						if want := proxy.IsolateCodexSessionID(apiKeyID, session); cacheKey != want {
							t.Fatalf("prompt_cache_key = %q, want API-key-isolated session %q", cacheKey, want)
						}
						return cacheKey
					case <-ctx.Done():
						t.Fatal("no upstream WebSocket frame")
						return ""
					}
				}

				first := invoke(41, "session-a")
				if repeated := invoke(41, "session-a"); repeated != first {
					t.Fatalf("same-session cache key changed: %q -> %q", first, repeated)
				}
				if other := invoke(41, "session-b"); other == first {
					t.Fatal("different sessions shared a cache key")
				}
				if other := invoke(42, "session-a"); other == first {
					t.Fatal("different API keys shared a cache key")
				}
				if resumed := invoke(41, "session-a"); resumed != first {
					t.Fatalf("resumed session cache key changed: %q -> %q", first, resumed)
				}
			})
		}
	}
}
