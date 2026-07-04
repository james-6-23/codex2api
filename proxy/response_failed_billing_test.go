package proxy

import "testing"

// 流式请求在首 token 之前收到 response.failed 时,应中止 SSE 转发、由循环外按真实
// HTTP 错误码返回,而不是把失败包成 200 + [DONE](后者会被上游中转/计费方误判为
// 成功流并按预估 input token 计费)。见 shouldReturnHTTPErrorForResponseFailed。
func TestShouldReturnHTTPErrorForResponseFailed(t *testing.T) {
	cases := []struct {
		name         string
		eventType    string
		ttftRecorded bool
		wroteAnyBody bool
		clientGone   bool
		want         bool
	}{
		{"首 token 前的 response.failed 应返回错误码", "response.failed", false, false, false, true},
		{"response.completed 不拦截", "response.completed", false, false, false, false},
		{"已产出首 token 不拦截(维持流式收尾)", "response.failed", true, false, false, false},
		{"已向下游写过 body 不拦截(200 已发出)", "response.failed", false, true, false, false},
		{"客户端已断开不拦截(继续读上游取 usage)", "response.failed", false, false, true, false},
		{"普通内容事件不拦截", "response.output_text.delta", false, false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := shouldReturnHTTPErrorForResponseFailed(tc.eventType, tc.ttftRecorded, tc.wroteAnyBody, tc.clientGone)
			if got != tc.want {
				t.Errorf("shouldReturnHTTPErrorForResponseFailed(%q, ttft=%v, wrote=%v, gone=%v) = %v, want %v",
					tc.eventType, tc.ttftRecorded, tc.wroteAnyBody, tc.clientGone, got, tc.want)
			}
		})
	}
}
