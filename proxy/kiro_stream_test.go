package proxy

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws/protocol/eventstream"
)

func kiroTestFrames(t *testing.T, frames ...[2]string) *bytes.Buffer {
	t.Helper()
	var buffer bytes.Buffer
	encoder := eventstream.NewEncoder()
	for _, frame := range frames {
		headers := eventstream.Headers{{Name: ":message-type", Value: eventstream.StringValue("event")}, {Name: ":event-type", Value: eventstream.StringValue(frame[0])}}
		if err := encoder.Encode(&buffer, eventstream.Message{Headers: headers, Payload: []byte(frame[1])}); err != nil {
			t.Fatal(err)
		}
	}
	return &buffer
}

func kiroCollect(t *testing.T, stream *kiroStreamContext, source *bytes.Buffer) []kiroSSEEvent {
	t.Helper()
	var events []kiroSSEEvent
	if err := kiroPumpSSE(source, stream, func(event kiroSSEEvent) error { events = append(events, event); return nil }); err != nil {
		t.Fatal(err)
	}
	return events
}

func TestKiroStreamReasoningTextToolAndUsage(t *testing.T) {
	source := kiroTestFrames(t,
		[2]string{"reasoningContentEvent", `{"text":"think "}`},
		[2]string{"reasoningContentEvent", `{"text":"more","signature":"sig-1"}`},
		[2]string{"assistantResponseEvent", `{"content":"Hello"}`},
		[2]string{"toolUseEvent", `{"name":"get_weather","toolUseId":"tu_1","input":"{\"city\":"}`},
		[2]string{"toolUseEvent", `{"name":"get_weather","toolUseId":"tu_1","input":"\"Paris\"}","stop":true}`},
		[2]string{"meteringEvent", `{"unit":"credit","unitPlural":"credits","usage":0.25}`},
		[2]string{"metadataEvent", `{"tokenUsage":{"uncachedInputTokens":11,"outputTokens":7,"cacheReadInputTokens":100,"cacheWriteInputTokens":3}}`},
	)
	stream := newKiroStreamContext("claude-sonnet-4-6", 50, true, &kiroConversion{ToolNameMap: map[string]string{}})
	events := kiroCollect(t, stream, source)
	var kinds []string
	for _, event := range events {
		kinds = append(kinds, event.Event)
	}
	want := "message_start content_block_start content_block_delta content_block_delta content_block_delta content_block_delta content_block_stop content_block_start content_block_delta content_block_stop content_block_start content_block_delta content_block_delta content_block_stop message_delta message_stop"
	if got := strings.Join(kinds, " "); got != want {
		t.Fatalf("event order\n got: %s\nwant: %s", got, want)
	}
	message, failure := kiroAggregateEvents(events)
	if failure != nil {
		t.Fatalf("unexpected failure %v", failure)
	}
	encoded, _ := json.Marshal(message)
	for _, fragment := range []string{`"thinking":"think more"`, `"signature":"sig-1"`, `"text":"Hello"`, `"input":{"city":"Paris"}`, `"stop_reason":"tool_use"`, `"input_tokens":11`, `"cache_read_input_tokens":100`, `"cache_creation_input_tokens":3`, `"output_tokens":7`, `"credit_usage":0.25`} {
		if !strings.Contains(string(encoded), fragment) {
			t.Errorf("aggregated message missing %s: %s", fragment, encoded)
		}
	}
}

func TestKiroStreamLegacyThinkingTagsAndFallbackUsage(t *testing.T) {
	source := kiroTestFrames(t,
		[2]string{"assistantResponseEvent", `{"content":"<thinking>\nplan it"}`},
		[2]string{"assistantResponseEvent", `{"content":"</thinking>\n\nAnswer"}`},
		[2]string{"contextUsageEvent", `{"contextUsagePercentage":1}`},
	)
	stream := newKiroStreamContext("claude-sonnet-4-5", 10, true, nil)
	message, failure := kiroAggregateEvents(kiroCollect(t, stream, source))
	if failure != nil {
		t.Fatal(failure)
	}
	encoded, _ := json.Marshal(message)
	if !strings.Contains(string(encoded), `"thinking":"plan it"`) || !strings.Contains(string(encoded), `"text":"Answer"`) || !strings.Contains(string(encoded), `"stop_reason":"end_turn"`) {
		t.Fatalf("legacy thinking tags not split: %s", encoded)
	}
	// 200K 窗口的 1% = 2000，扣掉估算输出后作为输入。
	usage := message["usage"].(map[string]any)
	if input := usage["input_tokens"].(int); input <= 1900 || input >= 2000 {
		t.Fatalf("context-derived input tokens = %d", input)
	}
}

func TestKiroStreamContentFilterAndTruncatedTool(t *testing.T) {
	refused := kiroTestFrames(t, [2]string{"assistantResponseEvent", `{"content":"partial"}`}, [2]string{"metadataEvent", `{"stopReason":"CONTENT_FILTERED"}`})
	message, _ := kiroAggregateEvents(kiroCollect(t, newKiroStreamContext("claude-sonnet-4.6", 1, false, nil), refused))
	if message["stop_reason"] != kiroRefusalStopReason {
		t.Fatalf("content filter stop reason = %v", message["stop_reason"])
	}
	truncated := kiroTestFrames(t, [2]string{"toolUseEvent", `{"name":"Bash","toolUseId":"tu_9","input":"{\"command\":\"ls"}`})
	_, failure := kiroAggregateEvents(kiroCollect(t, newKiroStreamContext("claude-sonnet-4.6", 1, false, nil), truncated))
	if failure == nil || failure["type"] != "upstream_tool_json_error" {
		t.Fatalf("truncated tool JSON must fail the stream, got %v", failure)
	}
}

func TestKiroMapUpstreamStatus(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   int
	}{
		{403, `{"message":"The bearer token included in the request is invalid."}`, 401},
		{402, `{"message":"limit","reason":"MONTHLY_REQUEST_COUNT"}`, 429},
		{429, `{"reason":"USER_REQUEST_RATE_EXCEEDED"}`, 429},
		{403, `{"reason":"TEMPORARILY_SUSPENDED"}`, 403},
		{400, `{"reason":"TOOL_SCHEMA_INVALID"}`, 400},
		{500, `upstream returned status code 524: gateway timeout`, 502},
		{400, `{"message":"Improperly formed request."}`, 400},
	}
	for _, tc := range cases {
		if got := kiroMapUpstreamStatus(tc.status, tc.body); got != tc.want {
			t.Errorf("kiroMapUpstreamStatus(%d, %s) = %d want %d", tc.status, tc.body, got, tc.want)
		}
	}
}

func TestKiroToolLeakFilters(t *testing.T) {
	conversion := &kiroConversion{KnownToolNames: map[string]struct{}{"Bash": {}, "Fetch": {}}, Request: kiroRequest{ConversationState: kiroConversationState{CurrentMessage: kiroCurrentMessage{UserInputMessage: kiroUserMessage{UserInputMessageContext: &kiroUserContext{
		Tools: []kiroTool{
			{ToolSpecification: kiroToolSpecification{Name: "Bash", InputSchema: kiroInputSchema{JSON: map[string]any{"type": "object", "properties": map[string]any{"command": map[string]any{"type": "string"}, "timeout": map[string]any{"type": "integer"}}, "required": []any{"command"}}}}},
			{ToolSpecification: kiroToolSpecification{Name: "Fetch", InputSchema: kiroInputSchema{JSON: map[string]any{"type": "object", "properties": map[string]any{"ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}}}, "required": []any{"ids"}}}}},
		},
	}}}}}}
	run := func(chunks ...string) []kiroSniffOutput {
		filters := newKiroToolLeakFilters(conversion)
		var out []kiroSniffOutput
		for _, chunk := range chunks {
			out = append(out, filters.pushText(filters.filterXML(chunk))...)
		}
		out = append(out, filters.pushText(filters.finishXML())...)
		return append(out, filters.finishText()...)
	}
	trailing := run("Running it.\n<invo", "ke name=\"Bash\"><parameter name=\"command\">ls -la</parameter><parameter name=\"timeout\">30</parameter></invoke>")
	if len(trailing) != 2 || trailing[0].text != "Running it.\n" || trailing[1].toolName != "Bash" || trailing[1].toolInput["command"] != "ls -la" || trailing[1].toolInput["timeout"] != int64(30) {
		t.Fatalf("trailing invoke not recovered: %+v", trailing)
	}
	fenced := run("```xml\n<invoke name=\"Bash\"><parameter name=\"command\">ls</parameter></invoke>\n```\n")
	if len(fenced) != 1 || fenced[0].toolName != "" {
		t.Fatalf("invoke inside a code fence must stay text: %+v", fenced)
	}
	unknown := run("<invoke name=\"rm\"><parameter name=\"x\">1</parameter></invoke>")
	if len(unknown) != 1 || unknown[0].toolName != "" {
		t.Fatalf("undeclared tool must stay text: %+v", unknown)
	}
	wrongItems := run("<invoke name=\"Fetch\"><parameter name=\"ids\">[\"wrong\"]</parameter></invoke>")
	if len(wrongItems) != 1 || wrongItems[0].toolName != "" {
		t.Fatalf("array items that violate the schema must stay text: %+v", wrongItems)
	}
	goodItems := run("<invoke name=\"Fetch\"><parameter name=\"ids\">[1, 2]</parameter></invoke>")
	if len(goodItems) != 1 || goodItems[0].toolName != "Fetch" {
		t.Fatalf("valid array items must still become a tool call: %+v", goodItems)
	}
	leaked := run("before <tool_use name=\"x\">{\"a\":1}</tool_use> after `<tool_use x>` kept")
	if len(leaked) != 1 || leaked[0].text != "before  after `<tool_use x>` kept" {
		t.Fatalf("tool_use XML leak handling: %+v", leaked)
	}
}

func TestKiroErrorFrameAfterPartialOutputInterrupts(t *testing.T) {
	buffer := kiroTestFrames(t, [2]string{"assistantResponseEvent", `{"content":"Hello"}`})
	encoder := eventstream.NewEncoder()
	exception := eventstream.Message{Headers: eventstream.Headers{
		{Name: ":message-type", Value: eventstream.StringValue("exception")},
		{Name: ":exception-type", Value: eventstream.StringValue("com.amazon.aws.codewhisperer#ThrottlingException")},
	}, Payload: []byte(`{"message":"Rate exceeded for request id secret-123"}`)}
	if err := encoder.Encode(buffer, exception); err != nil {
		t.Fatal(err)
	}
	events := kiroCollect(t, newKiroStreamContext("m", 10, false, nil), buffer)
	last := events[len(events)-1]
	if last.Event != "error" {
		t.Fatalf("an exception after partial output must end the stream with an error, got %s", last.Event)
	}
	failure, _ := last.Data["error"].(map[string]any)
	if failure["type"] != "rate_limit_error" || strings.Contains(failure["message"].(string), "secret-123") || !strings.Contains(failure["message"].(string), "ThrottlingException") {
		t.Fatalf("failure = %v", failure)
	}
	for _, event := range events {
		if event.Event == "message_delta" {
			t.Fatal("an interrupted stream must not report a normal stop")
		}
	}
	response, _ := kiroAnthropicJSONResponse(events, newKiroStreamContext("m", 10, false, nil))
	if response.StatusCode != 429 {
		t.Fatalf("non-streaming status = %d, want 429", response.StatusCode)
	}
}

func TestKiroClassifyUpstreamFailure(t *testing.T) {
	cases := map[string][2]string{
		"ServiceUnavailableException|high load":               {"overloaded_error", "503"},
		"ValidationException|Input is too long for requested": {"invalid_request_error", "prompt is too long"},
		"AccessDeniedException|nope":                          {"permission_error", "denied"},
		"InternalServerException|secret-request-id":           {"api_error", "InternalServerException"},
	}
	for input, want := range cases {
		code, message, _ := strings.Cut(input, "|")
		errorType, clientMessage := kiroClassifyUpstreamFailure(code, message)
		if errorType != want[0] || strings.Contains(clientMessage, "secret") || (want[1] != "503" && !strings.Contains(clientMessage, want[1])) {
			t.Errorf("%s -> %s %q", input, errorType, clientMessage)
		}
	}
	if kiroFailureStatus("overloaded_error") != 503 || kiroFailureStatus("api_error") != 502 {
		t.Fatal("status mapping drifted from kiro.rs")
	}
}
