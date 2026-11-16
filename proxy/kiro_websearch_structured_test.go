package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

func TestKiroSearchOnlyStructuredOutput(t *testing.T) {
	t.Setenv("CODEX_TRANSPORT_MODE", "standard")
	for _, responses := range []bool{false, true} {
		for _, streaming := range []bool{false, true} {
			t.Run(fmt.Sprintf("responses=%v/stream=%v", responses, streaming), func(t *testing.T) {
				account := &auth.Account{DBID: 910003, UpstreamType: auth.UpstreamKiro, AccessToken: "test", Kiro: auth.KiroCredential{AuthKind: auth.KiroAuthKindSocial}}
				generations, searches := 0, 0
				installClaudeBoundaryTransport(t, account, func(request *http.Request) (*http.Response, error) {
					payload, err := io.ReadAll(request.Body)
					if err != nil {
						return nil, err
					}
					if request.URL.Path == "/mcp" {
						searches++
						if query := gjson.GetBytes(payload, "params.arguments.query").String(); query != "test-query" {
							t.Errorf("search query changed: %q", query)
						}
						return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{"result":{"content":[{"type":"text","text":"{\"results\":[{\"title\":\"answer\",\"url\":\"https://example.com\",\"snippet\":\"42\"}]}"}]}}`))}, nil
					}
					generations++
					if !strings.Contains(string(payload), kiroStructuredMarker) {
						t.Error("structured instruction missing from generation")
					}
					frames := kiroTestFrames(t, [2]string{"assistantResponseEvent", `{"content":"Here is the answer: {\"answer\":42}"}`})
					if generations == 1 {
						frames = kiroTestFrames(t, [2]string{"toolUseEvent", `{"name":"web_search","toolUseId":"search-1","input":"{\"query\":\"test-query\"}","stop":true}`})
					}
					return &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(frames)}, nil
				})
				body := fmt.Sprintf(`{"model":"claude-haiku-4.5","stream":%v,"max_tokens":200,"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":"test-query"}],"output_config":{"format":{"type":"json_schema","schema":{"type":"object","properties":{"answer":{"type":"integer"}},"required":["answer"]}}}}`, streaming)
				var response *http.Response
				var err error
				if responses {
					body = fmt.Sprintf(`{"model":"claude-haiku-4.5","stream":%v,"tools":[{"type":"web_search"}],"input":"test-query","text":{"format":{"type":"json_schema","name":"answer","schema":{"type":"object","properties":{"answer":{"type":"integer"}},"required":["answer"]}}}`, streaming)
					response, err = ExecuteKiroResponsesRequest(context.Background(), account, []byte(body), "", nil)
				} else {
					response, err = ExecuteKiroMessagesRequest(context.Background(), account, []byte(body), "")
				}
				if err != nil {
					t.Fatal(err)
				}
				defer response.Body.Close()
				output, err := io.ReadAll(response.Body)
				if err != nil {
					t.Fatal(err)
				}
				if generations != 2 || searches != 1 || response.StatusCode != 200 || strings.Contains(string(output), "Here is the answer") || !strings.Contains(string(output), `\"answer\":42`) {
					t.Fatalf("generations=%d searches=%d status=%d body=%s", generations, searches, response.StatusCode, output)
				}
				if !strings.Contains(string(output), "web_search") {
					t.Fatal("search provenance lost")
				}
			})
		}
	}
}
