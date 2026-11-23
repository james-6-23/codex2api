package proxy

import (
	"bytes"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	promptResponseCaptureKey     = "prompt_request_response_capture"
	promptResponseFrameLimit     = 1024 * 1024
	promptResponseTextLimit      = 256 * 1024
	promptResponseArgsLimit      = 128 * 1024
	promptResponsePartLimit      = 128
	promptResponseChatPrimaryKey = "chat/0"
)

type promptResponseCapture struct {
	traceUsage           *promptRequestTraceUsage
	mode                 string
	body                 []byte
	line                 []byte
	event                []byte
	skipLine, skipEvent  bool
	bodyOverflow         bool
	writeFailed          bool
	status, message      string
	truncated            bool
	textParts            []*promptResponseText
	textByKey            map[string]*promptResponseText
	messagePhases        map[string]string
	chatReplyClassified  bool
	chatPrimaryStopped   bool
	chatPrimaryHasTool   bool
	chatPrimaryOtherRole bool
	chatStreamDone       bool
	calls                []*promptResponseCall
	callByKey            map[string]*promptResponseCall
	textBytes, argsBytes int
}

type promptResponseText struct {
	key   string
	value strings.Builder
}

type promptResponseCall struct {
	id, name string
	args     strings.Builder
}

type promptResponseWriter struct {
	gin.ResponseWriter
	capture *promptResponseCapture
}

func (w *promptResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *promptResponseWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.observe(p[:n], err)
	return n, err
}

func (w *promptResponseWriter) WriteString(s string) (int, error) {
	n, err := w.ResponseWriter.WriteString(s)
	w.observe([]byte(s[:n]), err)
	return n, err
}

func (w *promptResponseWriter) Flush() {
	w.capture.selectHTTPMode(w.Header().Get("Content-Type"))
	w.ResponseWriter.Flush()
}

func (w *promptResponseWriter) observe(p []byte, err error) {
	w.capture.selectHTTPMode(w.Header().Get("Content-Type"))
	w.capture.writeFailed = w.capture.writeFailed || err != nil
	switch w.capture.mode {
	case "sse":
		w.capture.observeSSE(p)
	case "json":
		if len(p) > promptResponseFrameLimit-len(w.capture.body) {
			w.capture.body = nil
			w.capture.bodyOverflow = true
			w.capture.truncated = true
		} else if !w.capture.bodyOverflow {
			w.capture.body = append(w.capture.body, p...)
		}
	}
}

func (r *promptResponseCapture) selectHTTPMode(contentType string) {
	if r.mode != "" || contentType == "" {
		return
	}
	contentType = strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0])
	switch {
	case strings.EqualFold(contentType, "text/event-stream"):
		r.mode = "sse"
	case strings.EqualFold(contentType, "application/json"), strings.HasSuffix(strings.ToLower(contentType), "+json"):
		r.mode = "json"
	default:
		r.mode = "other"
	}
}

func promptResponseFromContext(c *gin.Context) *promptResponseCapture {
	value, _ := c.Get(promptResponseCaptureKey)
	capture, _ := value.(*promptResponseCapture)
	return capture
}

func observePromptResponseWS(c *gin.Context, payload []byte, err error) {
	if capture := promptResponseFromContext(c); capture != nil {
		if err != nil {
			capture.writeFailed = true
			return
		}
		capture.observeEvent(payload)
	}
}

func (r *promptResponseCapture) observeSSE(p []byte) {
	for len(p) > 0 {
		line, remainder, ended := bytes.Cut(p, []byte{'\n'})
		if len(line) > promptResponseFrameLimit-len(r.line) {
			r.line = nil
			r.skipLine = true
			r.skipEvent = true
			r.truncated = true
		} else if !r.skipLine {
			r.line = append(r.line, line...)
		}
		if ended {
			if !r.skipLine {
				r.observeSSELine(bytes.TrimSuffix(r.line, []byte{'\r'}))
			}
			r.line = r.line[:0]
			r.skipLine = false
		}
		p = remainder
	}
}

func (r *promptResponseCapture) observeSSELine(line []byte) {
	if len(line) == 0 {
		if !r.skipEvent {
			r.observeEvent(r.event)
		}
		r.event = r.event[:0]
		r.skipEvent = false
		return
	}
	if r.skipEvent || !bytes.HasPrefix(line, []byte("data:")) {
		return
	}
	value := bytes.TrimPrefix(line[len("data:"):], []byte{' '})
	if len(value)+1 > promptResponseFrameLimit-len(r.event) {
		r.skipEvent = true
		r.truncated = true
		return
	}
	if len(r.event) > 0 {
		r.event = append(r.event, '\n')
	}
	r.event = append(r.event, value...)
}

func (r *promptResponseCapture) observeEvent(payload []byte) {
	if len(payload) > promptResponseFrameLimit {
		r.truncated = true
		r.observeResponsesStatus(gjson.GetBytes(payload, "type").String(), gjson.Result{})
		return
	}
	payload = bytes.TrimSpace(payload)
	if bytes.Equal(payload, []byte("[DONE]")) {
		r.chatStreamDone = true
		if r.status == "" {
			r.status = "completed"
		}
		return
	}
	if len(payload) == 0 || !gjson.ValidBytes(payload) {
		return
	}
	root := gjson.ParseBytes(payload)
	kind := root.Get("type").String()
	r.observeResponsesStatus(kind, root.Get("response"))
	switch kind {
	case "response.output_text.delta", "response.refusal.delta", "response.text.delta":
		r.addText(responseTextKey(root), root.Get("delta").String(), false)
	case "response.output_text.done", "response.refusal.done", "response.text.done":
		r.addText(responseTextKey(root), promptResponseField(root, "text", "refusal"), true)
	case "response.output_item.added", "response.output_item.done":
		r.observeResponsesItem(root.Get("item"), root.Get("output_index").Int())
	case "response.function_call_arguments.delta", "response.custom_tool_call_input.delta", "response.mcp_call_arguments.delta":
		r.addCall(responseCallKey(root, root.Get("output_index").Int()), root.Get("call_id").String(),
			root.Get("name").String(), root.Get("delta").String(), false)
	case "response.function_call_arguments.done", "response.custom_tool_call_input.done", "response.mcp_call_arguments.done":
		r.addCall(responseCallKey(root, root.Get("output_index").Int()), root.Get("call_id").String(),
			root.Get("name").String(), promptResponseField(root, "arguments", "input"), true)
	case "message_stop":
		if r.status == "" {
			r.status = "completed"
		}
	case "content_block_start":
		r.observeAnthropicBlock(root.Get("content_block"), root.Get("index").Int())
	case "content_block_delta":
		part := root.Get("delta")
		key := strconv.FormatInt(root.Get("index").Int(), 10)
		switch part.Get("type").String() {
		case "text_delta":
			r.addText(key, part.Get("text").String(), false)
		case "input_json_delta":
			r.addCall(key, "", "", part.Get("partial_json").String(), false)
		}
	}
	r.observeChat(root, true)
	r.observeError(root.Get("error"))
}

func (r *promptResponseCapture) observeResponsesStatus(kind string, response gjson.Result) {
	switch {
	case isResponsesSuccessTerminalEvent(kind), kind == "response.done":
		r.status = "completed"
		r.observeResponseObjectStatus(response)
		r.observeResponsesOutput(response.Get("output"))
	case kind == "response.failed":
		r.status = "failed"
		r.observeResponsesOutput(response.Get("output"))
		r.observeError(response.Get("error"))
	}
}

func (r *promptResponseCapture) observeResponseObjectStatus(root gjson.Result) {
	switch root.Get("status").String() {
	case "failed":
		r.status = "failed"
		r.observeError(root.Get("error"))
	case "cancelled", "canceled":
		r.status = "interrupted"
	case "incomplete":
		switch root.Get("incomplete_details.reason").String() {
		case "cancelled", "canceled", "interrupted":
			r.status = "interrupted"
		}
	}
}

func responseTextKey(root gjson.Result) string {
	return responseTextPartKey(root.Get("output_index").Int(), root.Get("content_index").Int())
}

func responseTextPartKey(outputIndex, contentIndex int64) string {
	return responseMessageKey(outputIndex) + ":" + strconv.FormatInt(contentIndex, 10)
}

func responseMessageKey(outputIndex int64) string {
	return "responses/" + strconv.FormatInt(outputIndex, 10)
}

func promptResponseField(root gjson.Result, paths ...string) string {
	for _, path := range paths {
		if value := root.Get(path).String(); value != "" {
			return value
		}
	}
	return ""
}

func responseCallKey(root gjson.Result, index int64) string {
	if id := promptResponseField(root, "item_id", "id", "call_id"); id != "" && len(id) <= 512 {
		return id
	}
	return strconv.FormatInt(index, 10)
}

func (r *promptResponseCapture) observeResponsesOutput(output gjson.Result) {
	output.ForEach(func(index, item gjson.Result) bool {
		r.observeResponsesItem(item, index.Int())
		return true
	})
}

func (r *promptResponseCapture) observeResponsesItem(item gjson.Result, index int64) {
	var arguments string
	kind := item.Get("type").String()
	switch kind {
	case "message":
		phase := item.Get("phase").Str
		if item.Get("role").Str == "assistant" && (phase == "commentary" || phase == "final_answer") &&
			index >= 0 && index < promptResponsePartLimit {
			if r.messagePhases == nil {
				r.messagePhases = make(map[string]string)
			}
			r.messagePhases[responseMessageKey(index)] = phase
		}
		content := item.Get("content")
		if content.Type == gjson.String {
			r.addText(responseTextPartKey(index, 0), content.Str, true)
			return
		}
		content.ForEach(func(partIndex, part gjson.Result) bool {
			switch part.Get("type").String() {
			case "output_text", "text", "refusal":
				key := responseTextPartKey(index, partIndex.Int())
				r.addText(key, promptResponseField(part, "text", "refusal"), true)
			}
			return true
		})
		return
	case "function_call", "mcp_call":
		arguments = item.Get("arguments").String()
	case "custom_tool_call", "code_interpreter_call":
		arguments = promptResponseField(item, "input", "code")
	case "computer_call", "web_search_call", "local_shell_call", "shell_call", "apply_patch_call":
		arguments = promptResponseField(item, "action", "operation")
	default:
		return
	}
	name := item.Get("name").String()
	if name == "" {
		name = kind
	}
	callID := promptResponseField(item, "call_id", "id")
	key := responseCallKey(item, index)
	// Some adapters omit item_id from argument deltas. Merge per-index deltas
	// already collected for the same output item with its eventual identifier.
	indexKey := strconv.FormatInt(index, 10)
	if previous := r.callByKey[indexKey]; previous != nil && r.callByKey[key] == nil {
		r.callByKey[strings.Clone(key)] = previous
	}
	r.addCall(key, callID, name, arguments, true)
	if current := r.callByKey[key]; current != nil && r.callByKey[indexKey] == nil {
		r.callByKey[indexKey] = current
	}
}

func (r *promptResponseCapture) observeChat(root gjson.Result, stream bool) {
	choices := root.Get("choices")
	if !choices.IsArray() {
		return
	}
	r.chatReplyClassified = true
	choices.ForEach(func(i, choice gjson.Result) bool {
		index := i.Int()
		if actual := choice.Get("index"); actual.Exists() {
			index = actual.Int()
		}
		key := "chat/" + strconv.FormatInt(index, 10)
		field := "message"
		if stream {
			field = "delta"
		}
		message := choice.Get(field)
		tools := message.Get("tool_calls")
		function := message.Get("function_call")
		if index == 0 {
			if finish := choice.Get("finish_reason").Str; finish != "" {
				r.chatPrimaryStopped = finish == "stop"
			}
			r.chatPrimaryHasTool = r.chatPrimaryHasTool || tools.Get("0").Exists() || function.IsObject()
			if role := message.Get("role").Str; role != "" && role != "assistant" {
				r.chatPrimaryOtherRole = true
			}
		}
		content := message.Get("content")
		if content.Type == gjson.String {
			r.addText(key, content.String(), !stream)
		} else if content.IsArray() {
			content.ForEach(func(partIndex, part gjson.Result) bool {
				if part.Get("type").String() == "text" {
					r.addText(key+":"+strconv.FormatInt(partIndex.Int(), 10), part.Get("text").String(), !stream)
				}
				return true
			})
		}
		r.addText(key+":refusal", message.Get("refusal").Str, !stream)
		tools.ForEach(func(toolIndex, tool gjson.Result) bool {
			i := toolIndex.Int()
			if actual := tool.Get("index"); actual.Exists() {
				i = actual.Int()
			}
			r.addCall(key+":"+strconv.FormatInt(i, 10), tool.Get("id").String(),
				promptResponseField(tool, "function.name", "custom.name"), promptResponseField(tool, "function.arguments", "custom.input"), !stream)
			return true
		})
		if function.IsObject() {
			r.addCall(key+":function", "", function.Get("name").String(), function.Get("arguments").String(), !stream)
		}
		return true
	})
}

func (r *promptResponseCapture) observeAnthropicBlock(block gjson.Result, index int64) {
	key := strconv.FormatInt(index, 10)
	switch block.Get("type").String() {
	case "text":
		r.addText(key, block.Get("text").String(), true)
	case "tool_use", "server_tool_use":
		input := block.Get("input").String()
		if input == "{}" {
			input = ""
		}
		r.addCall(key, block.Get("id").String(), block.Get("name").String(), input, true)
	}
}

func (r *promptResponseCapture) observeJSON() {
	if r.bodyOverflow || !gjson.ValidBytes(r.body) {
		return
	}
	root := gjson.ParseBytes(r.body)
	r.observeResponseObjectStatus(root)
	r.observeError(root.Get("error"))
	r.observeResponsesOutput(root.Get("output"))
	r.observeChat(root, false)
	root.Get("content").ForEach(func(i, block gjson.Result) bool {
		r.observeAnthropicBlock(block, i.Int())
		return true
	})
}

func (r *promptResponseCapture) observeError(value gjson.Result) {
	if !value.Exists() || value.Type == gjson.Null {
		return
	}
	status := "failed"
	switch value.Get("code").String() {
	case string(ErrorCodeUpstreamStreamBreak), "stream_interrupted", "cancelled", "canceled":
		status = "interrupted"
	}
	if r.status != "interrupted" {
		r.status = status
	}
	if value.Type == gjson.String {
		r.message = strings.Clone(r.limit(value.String(), 4096))
	} else if message := value.Get("message").String(); message != "" {
		r.message = strings.Clone(r.limit(message, 4096))
	}
}

func (r *promptResponseCapture) addText(key, value string, replace bool) {
	if value == "" {
		return
	}
	part := r.textByKey[key]
	if part == nil {
		if len(r.textParts) >= promptResponsePartLimit {
			r.truncated = true
			return
		}
		if r.textByKey == nil {
			r.textByKey = make(map[string]*promptResponseText)
		}
		part = &promptResponseText{key: key}
		r.textByKey[key] = part
		r.textParts = append(r.textParts, part)
	}
	if replace {
		r.textBytes -= part.value.Len()
		part.value.Reset()
	}
	value = r.limit(value, promptResponseTextLimit-r.textBytes)
	part.value.WriteString(value)
	r.textBytes += len(value)
}

func (r *promptResponseCapture) addCall(key, id, name, arguments string, replace bool) {
	call := r.callByKey[key]
	if call == nil {
		if len(r.calls) >= promptResponsePartLimit {
			r.truncated = true
			return
		}
		if r.callByKey == nil {
			r.callByKey = make(map[string]*promptResponseCall)
		}
		call = &promptResponseCall{}
		r.calls = append(r.calls, call)
		r.callByKey[strings.Clone(key)] = call
	}
	if id != "" {
		call.id = strings.Clone(r.limit(id, 512))
	}
	if name != "" {
		call.name = strings.Clone(r.limit(name, 512))
	}
	if arguments != "" {
		if replace {
			r.argsBytes -= call.args.Len()
			call.args.Reset()
		}
		arguments = r.limit(arguments, promptResponseArgsLimit-r.argsBytes)
		call.args.WriteString(arguments)
		r.argsBytes += len(arguments)
	}
}

func (r *promptResponseCapture) limit(value string, capacity int) string {
	if len(value) <= capacity {
		return value
	}
	r.truncated = true
	for capacity > 0 && !utf8.RuneStart(value[capacity]) {
		capacity--
	}
	return value[:capacity]
}

func (r *promptResponseCapture) result(status int, cancelled bool) database.PromptRequestResponse {
	if r.mode == "json" {
		r.observeJSON()
	}
	if r.status == "" {
		switch {
		case cancelled, r.writeFailed, status == 499:
			r.status = "interrupted"
		case status >= 400:
			r.status = "failed"
		case r.mode == "sse", r.mode == "websocket":
			r.status = "interrupted"
		default:
			r.status = "completed"
		}
	}
	// Chat choices do not carry Responses phases. A regular terminal stop
	// proves the primary assistant choice ended, while a stream also needs
	// the wire sentinel to rule out a disconnected downstream.
	chatFinal := r.chatPrimaryStopped && !r.chatPrimaryHasTool && !r.chatPrimaryOtherRole &&
		!cancelled && !r.writeFailed && status >= 200 && status < 300 && r.status == "completed" &&
		(r.mode == "json" || (r.mode == "sse" && r.chatStreamDone))
	var answer, final, commentary strings.Builder
	for _, part := range r.textParts {
		if part.value.Len() == 0 {
			continue
		}
		appendPromptResponseText(&answer, part.value.String())
		message, _, _ := strings.Cut(part.key, ":")
		switch {
		case r.messagePhases[message] == "final_answer", chatFinal && message == promptResponseChatPrimaryKey:
			appendPromptResponseText(&final, part.value.String())
		case r.messagePhases[message] == "commentary":
			appendPromptResponseText(&commentary, part.value.String())
		}
	}
	response := database.PromptRequestResponse{
		Status: r.status, Text: answer.String(), Error: r.message, Truncated: r.truncated,
		FinalText: final.String(), FinalObserved: final.Len() != 0, CommentaryText: commentary.String(),
		ChatReplyClassified: r.chatReplyClassified,
		ToolCalls:           make([]database.PromptRequestResponseCall, 0, len(r.calls)),
	}
	for _, call := range r.calls {
		if call.name != "" || call.id != "" || call.args.Len() > 0 {
			response.ToolCalls = append(response.ToolCalls, database.PromptRequestResponseCall{CallID: call.id, Name: call.name, Arguments: call.args.String()})
		}
	}
	return response
}

func appendPromptResponseText(target *strings.Builder, text string) {
	if target.Len() > 0 {
		target.WriteString("\n\n")
	}
	target.WriteString(text)
}
