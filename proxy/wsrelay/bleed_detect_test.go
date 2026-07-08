package wsrelay

import "testing"

// 被动串扰检测：一个请求流内出现第二个不同的 response_id 应报警；同一 id 不报。
func TestHandleMessageDetectsSessionBleed(t *testing.T) {
	var got [][2]string
	prev := OnSessionBleedDetected
	OnSessionBleedDetected = func(accountID int64, p, c string) { got = append(got, [2]string{p, c}) }
	defer func() { OnSessionBleedDetected = prev }()

	r := &WsResponse{}
	noop := func(b []byte) bool { return true }

	// 正常流：created(resp_A) → delta → completed(resp_A)，不应报警
	_ = r.handleMessage([]byte(`{"type":"response.created","response":{"id":"resp_A"}}`), noop)
	_ = r.handleMessage([]byte(`{"type":"response.output_text.delta","delta":"hi"}`), noop)
	if len(got) != 0 {
		t.Fatalf("同一 response_id 不应报警，got %v", got)
	}

	// 串扰：出现第二个不同 response_id → 报警一次
	_ = r.handleMessage([]byte(`{"type":"response.created","response":{"id":"resp_B"}}`), noop)
	if len(got) != 1 || got[0][0] != "resp_A" || got[0][1] != "resp_B" {
		t.Fatalf("应检测到 resp_A→resp_B 串扰，got %v", got)
	}
}
