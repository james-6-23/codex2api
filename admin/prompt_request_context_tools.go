package admin

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
)

const (
	promptContextReadAttempt   = "attempted_read"
	promptContextReadFailed    = "read_failed"
	promptContextReadSuccess   = "successful_read"
	promptContextNoBody        = "body_unavailable"
	promptContextTruncatedBody = "truncated_body_unavailable"
)

type promptContextReads struct {
	sections []promptRequestContextSection
	byCallID map[string]promptContextReadCall
}

type promptContextReadCall struct {
	sections []int
	batch    bool
	filePath string
}

type promptContextUnpairedReceipt struct {
	event promptRequestToolEvent
	state string
}

func (h *Handler) promptContextReadSections(ctx context.Context, request database.PromptRequestBody, changes *promptRequestChanges, result *promptRequestResult) []promptRequestContextSection {
	reads := promptContextReads{byCallID: make(map[string]promptContextReadCall)}
	knownCalls := make(map[string]bool)
	unpaired := make(map[string]promptContextUnpairedReceipt)
	field, items := promptRequestToolHistory(request.Body)
	for index, item := range items {
		state := promptContextHistoryState(changes, field, index)
		visitPromptRequestToolItem(item, "", func(event promptRequestToolEvent) {
			if !event.receipt {
				knownCalls[event.id] = true
				reads.addCall(event.id, event.name, promptRequestToolText(event.value), state)
				return
			}
			if !knownCalls[event.id] && state != promptContextInherited {
				unpaired[event.id] = promptContextUnpairedReceipt{event, state}
			}
			source := request.ID
			if state == promptContextInherited {
				source = 0
			}
			reads.addReceipt(event.id, promptRequestToolText(event.value), state, source, event.failed)
		})
	}
	h.matchDetachedPromptContextReads(ctx, request, unpaired, &reads)
	if result != nil {
		if result.Response != nil {
			for _, call := range result.Response.ToolCalls {
				reads.addCall(call.CallID, call.Name, call.Arguments, promptContextNew)
			}
		}
		for _, receipt := range result.ToolResults {
			// Captured replies supply arguments from the call above. Archived
			// receipts can carry the matching call inferred from the next request.
			if receipt.Arguments != "" {
				reads.addCall(receipt.CallID, receipt.Name, receipt.Arguments, promptContextNew)
			}
			reads.addReceipt(receipt.CallID, receipt.Text, promptContextNew, receipt.FromRequestID, receipt.failed)
		}
	}
	var resultSections []promptRequestContextSection
	for _, section := range reads.sections {
		// An unproven AGENTS.md read must not look like loaded instructions.
		if section.Kind != "" {
			resultSections = append(resultSections, section)
		}
	}
	return resultSections
}

// A stateful continuation may carry only the receipt. Its matching call must
// be an observed reply in the same verified conversation.
func (h *Handler) matchDetachedPromptContextReads(ctx context.Context, request database.PromptRequestBody, unpaired map[string]promptContextUnpairedReceipt, reads *promptContextReads) {
	if len(unpaired) == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	ids, err := h.db.PromptRequestNeighborIDs(ctx, request, false, promptResultNeighbors)
	if err != nil {
		return
	}
	for _, id := range ids {
		response, err := h.db.GetPromptRequestResponse(ctx, id)
		if err != nil {
			return
		}
		if response == nil {
			continue
		}
		for _, call := range response.ToolCalls {
			receipt, ok := unpaired[call.CallID]
			if !ok {
				continue
			}
			reads.addCall(call.CallID, call.Name, call.Arguments, receipt.state)
			reads.addReceipt(call.CallID, promptRequestToolText(receipt.event.value), receipt.state, request.ID, receipt.event.failed)
			delete(unpaired, call.CallID)
		}
		if len(unpaired) == 0 {
			return
		}
	}
}

func (reads *promptContextReads) addCall(id, name, args, state string) {
	if _, exists := reads.byCallID[id]; exists && id != "" {
		return
	}
	paths, batch := promptContextReadPaths(name, args)
	call := promptContextReadCall{batch: batch}
	if !batch && len(paths) == 1 && promptContextFileTool(name) {
		call.filePath = paths[0]
	}
	for _, path := range paths {
		file := promptContextFileName(path)
		isSkill := strings.EqualFold(file, "SKILL.md")
		// A batch cannot link unlabelled output to AGENTS.md.
		if !isSkill && (batch || !strings.EqualFold(file, "AGENTS.md")) {
			continue
		}
		section := promptRequestContextSection{Path: path, State: state, Source: "tool_call", Evidence: promptContextReadAttempt}
		if isSkill {
			section.Kind = promptContextAttempt
			parent := strings.TrimRight(path[:len(path)-len(file)], "/\\")
			section.Name = promptContextFileName(parent)
		}
		call.sections = append(call.sections, len(reads.sections))
		reads.sections = append(reads.sections, section)
	}
	if id != "" && len(call.sections) != 0 {
		reads.byCallID[id] = call
	}
}

func (reads *promptContextReads) addReceipt(id, output, state string, source int64, failed bool) {
	call, ok := reads.byCallID[id]
	if !ok || reads.sections[call.sections[0]].Source == "tool_result" {
		return
	}
	var body, evidence string
	if failed {
		evidence = promptContextReadFailed
	} else if call.filePath != "" {
		body, evidence = promptContextFileReceipt(output, call.filePath)
	} else {
		body, evidence = promptContextReceiptBody(output, 0)
	}
	var headers map[string]string
	if call.batch && evidence == promptContextReadSuccess {
		headers = promptContextBatchSkillHeaders(body)
		names := make(map[string]bool)
		for _, index := range call.sections {
			name := strings.ToLower(reads.sections[index].Name)
			if names[name] {
				delete(headers, name)
			}
			names[name] = true
		}
	}
	for _, index := range call.sections {
		section := &reads.sections[index]
		header, proof := body, evidence
		if call.batch && proof == promptContextReadSuccess {
			header = headers[strings.ToLower(section.Name)]
			if header == "" {
				proof = promptContextNoBody
				if promptContextTruncationHeader.MatchString(body) && promptContextTruncationMarker.MatchString(body) {
					proof = promptContextTruncatedBody
				}
			}
		} else if proof == promptContextReadSuccess && section.Kind == promptContextAttempt {
			header, proof = promptContextSingleSkillReceipt(body)
		}
		applyPromptContextReadReceipt(section, header, state, proof, source)
	}
}

func promptContextSingleSkillReceipt(body string) (string, string) {
	text := strings.TrimSpace(strings.TrimPrefix(body, "\ufeff"))
	truncated := promptContextTruncationHeader.FindStringIndex(text)
	if truncated != nil {
		text = strings.TrimSpace(text[truncated[1]:])
	} else if strings.HasPrefix(text, "Warning: truncated output") {
		return "", promptContextNoBody
	}
	proof := promptContextNoBody
	if truncated != nil && promptContextTruncationMarker.MatchString(text) {
		proof = promptContextTruncatedBody
	}
	if promptContextReadDiagnostic.MatchString(text) {
		return "", proof
	}
	instructions := text
	if promptContextFrontMatterStart.MatchString(text) {
		header := promptContextFrontMatter.FindStringSubmatchIndex(text)
		if header == nil || len(promptContextSkillName.FindAllStringSubmatch(text[header[2]:header[3]], 2)) != 1 {
			return "", proof
		}
		instructions = text[header[1]:]
	}
	for _, line := range strings.Split(promptContextTruncationMarker.ReplaceAllString(instructions, ""), "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "#") &&
			strings.Trim(line, "-") != "" && !promptContextSkillMetadata.MatchString(line) {
			return text, promptContextReadSuccess
		}
	}
	return "", proof
}

func applyPromptContextReadReceipt(section *promptRequestContextSection, body, state, evidence string, source int64) {
	section.State, section.Source, section.SourceRequestID, section.Evidence = state, "tool_result", source, evidence
	if evidence != promptContextReadSuccess {
		return
	}
	section.Kind = promptContextAgents
	section.Text = body
	if strings.EqualFold(promptContextFileName(section.Path), "SKILL.md") {
		section.Kind = promptContextSkill
		if header := promptContextFrontMatter.FindStringSubmatch(body); header != nil {
			if variable := promptContextSkillName.FindStringSubmatch(header[1]); variable != nil {
				section.Name = strings.TrimSpace(variable[1])
			}
		}
	}
}

const promptContextPathToken = `(?:'([^'\r\n]+)'|"([^"$\x60\r\n]+)"|([A-Za-z0-9_./:\\-]+))[ \t]*`
const promptContextSedLines = `[1-9][0-9]*(?:,[1-9][0-9]*)?p`
const promptContextPowerShellOption = `-(?:raw|totalcount[ \t]+[1-9][0-9]*|encoding[ \t]+(?:utf8(?:(?:no)?bom)?|'utf8(?:(?:no)?bom)?'|"utf8(?:(?:no)?bom)?")|erroraction[ \t]+stop)`

var (
	promptContextCatPath  = regexp.MustCompile(`^cat[ \t]+(?:--[ \t]+)?` + promptContextPathToken + `$`)
	promptContextHeadPath = regexp.MustCompile(`^head[ \t]+(?:-n[ \t]*|--lines=)[1-9][0-9]*[ \t]+(?:--[ \t]+)?` + promptContextPathToken + `$`)
	promptContextSedPath  = regexp.MustCompile(`^sed[ \t]+-n[ \t]+(?:'` + promptContextSedLines + `'|"` + promptContextSedLines + `"|` +
		promptContextSedLines + `)[ \t]+(?:--[ \t]+)?` + promptContextPathToken + `$`)
	promptContextGetContentOptions = regexp.MustCompile(`(?i)^get-content[ \t]+(?:(?:-path|-literalpath|` + promptContextPowerShellOption + `)[ \t]+){0,5}` +
		promptContextPathToken + `(?:[ \t]+` + promptContextPowerShellOption + `){0,4}[ \t]*$`)
	promptContextWrappedExec      = regexp.MustCompile(`(?s)^\s*text\(\s*await\s+tools\.exec_command\(\s*\{(.*)\}\s*\)\s*\)\s*;?\s*$`)
	promptContextWrappedField     = regexp.MustCompile(`^\s*([A-Za-z_$][\w$]*|"(?:[^"\\]|\\.)*")\s*:\s*("(?:[^"\\]|\\.)*"|-?\d+(?:\.\d+)?|true|false|null)\s*`)
	promptContextExited           = regexp.MustCompile(`(?i)^Chunk ID:[^\n]*\nWall time:[^\n]*\nProcess exited with code (-?[0-9]+)\n(?:Original token count:[^\n]*\n)?(?:Final )?Output:\n`)
	promptContextFrontMatter      = regexp.MustCompile(`^---[ \t]*\n([\s\S]*?)\n---[ \t]*(?:\n|$)`)
	promptContextFrontMatterStart = regexp.MustCompile(`^---[ \t]*(?:\n|$)`)
	promptContextReadDiagnostic   = regexp.MustCompile(`(?i)^(?:get-content|cat|head|sed)[ \t]*:`)
	promptContextSkillName        = regexp.MustCompile(`(?m)^name:[ \t]*["']?([^"'\n]+)["']?[ \t]*$`)
	promptContextSkillMetadata    = regexp.MustCompile(`(?i)^(?:name|description|version|license|compatibility|metadata|allowed-tools):`)
	promptContextHeaderLine       = regexp.MustCompile(`(?m)^---$`)
	promptContextTruncationHeader = regexp.MustCompile(`^Warning: truncated output \(original token count: [0-9]+\)\nTotal output lines: [0-9]+\n`)
	promptContextTruncationMarker = regexp.MustCompile(`…[0-9]+ chars truncated…`)
)

func promptContextReadPath(tool, raw string) string {
	if promptContextFileTool(tool) && gjson.Valid(raw) {
		return promptContextFilePath(gjson.Parse(raw))
	}
	command := promptContextReadCommand(tool, raw)
	for _, parser := range []*regexp.Regexp{promptContextCatPath, promptContextHeadPath, promptContextSedPath, promptContextGetContentOptions} {
		if path := promptContextPathMatch(parser, command); path != "" {
			return path
		}
	}
	return ""
}

func promptContextFileTool(tool string) bool {
	switch tool {
	case "Read", "read_file", "mcp__filesystem__read_file", "mcp__filesystem__read_text_file":
		return true
	default:
		return false
	}
}

func promptContextFilePath(node gjson.Result) string {
	filePath, alternate := node.Get("path"), node.Get("file_path")
	if filePath.Exists() && alternate.Exists() {
		return ""
	}
	if !filePath.Exists() {
		filePath = alternate
	}
	return directPromptContextFile(filePath.Str)
}

func promptContextFileReceipt(raw, expectedPath string) (string, string) {
	if !gjson.Valid(raw) {
		return promptContextReceiptBody(raw, 0)
	}
	node := gjson.Parse(raw)
	if node.Get("success").Type == gjson.False || node.Get("isError").Type == gjson.True || node.Get("is_error").Type == gjson.True {
		return "", promptContextReadFailed
	}
	exit := node.Get("exit_code")
	if exit.Exists() && (exit.Type != gjson.Number || exit.Num != 0) {
		return "", promptContextReadFailed
	}
	document := node.Get("structuredContent")
	if !document.Exists() {
		if !node.Get("path").Exists() && !node.Get("file_path").Exists() {
			return promptContextReceiptBody(raw, 0)
		}
		document = node
	}
	path := promptContextFilePath(document)
	if path == "" || strings.ReplaceAll(path, `\`, "/") != strings.ReplaceAll(expectedPath, `\`, "/") {
		return "", promptContextNoBody
	}
	if exit.Type != gjson.Number && node.Get("success").Type != gjson.True && node.Get("isError").Type != gjson.False && node.Get("is_error").Type != gjson.False {
		return "", promptContextNoBody
	}
	for _, field := range []string{"text", "content", "output"} {
		if text := document.Get(field); text.Type == gjson.String {
			return nonemptyPromptContextBody(text.Str)
		}
	}
	return "", promptContextNoBody
}

func promptContextReadCommand(tool, raw string) string {
	command, _ := promptContextToolCommand(tool, raw)
	return command
}

func promptContextToolCommand(tool, raw string) (command, workDir string) {
	switch tool {
	case "exec":
		if match := promptContextWrappedExec.FindStringSubmatch(raw); match != nil {
			return promptContextWrappedCommand(match[1])
		}
	case "exec_command", "shell", "local_shell", "shell_command", "bash", "Bash":
		if gjson.Valid(raw) {
			cmd, alternate := gjson.Get(raw, "cmd"), gjson.Get(raw, "command")
			if cmd.Exists() && alternate.Exists() {
				return "", ""
			}
			if !cmd.Exists() {
				cmd = alternate
			}
			command = cmd.Str
			workDir = gjson.Get(raw, "workdir").Str
		}
	}
	return command, workDir
}

func promptContextPathMatch(parser *regexp.Regexp, command string) string {
	if match := parser.FindStringSubmatch(command); match != nil {
		for _, part := range match[1:] {
			if part != "" {
				return directPromptContextFile(part)
			}
		}
	}
	return ""
}

func promptContextReadPaths(tool, raw string) ([]string, bool) {
	if path := promptContextReadPath(tool, raw); path != "" {
		return []string{path}, false
	}
	segments := splitPromptContextPowerShell(promptContextReadCommand(tool, raw))
	if len(segments) < 2 {
		return nil, false
	}
	var paths []string
	seen := make(map[string]bool)
	for _, segment := range segments {
		path := promptContextPathMatch(promptContextGetContentOptions, strings.TrimSpace(segment))
		if path != "" && !seen[path] {
			paths = append(paths, path)
			seen[path] = true
		}
	}
	return paths, true
}

func splitPromptContextPowerShell(command string) []string {
	var quote byte
	var scopes []byte
	var segments []string
	start := 0
	for i := 0; i < len(command); i++ {
		char := command[i]
		if char == '`' && quote != '\'' {
			i++
			continue
		}
		if quote != 0 {
			if char == quote {
				quote = 0
			}
			continue
		}
		switch char {
		case '\'', '"':
			quote = char
		case '#':
			if end := strings.IndexByte(command[i:], '\n'); end >= 0 {
				i += end
			} else {
				i = len(command)
			}
		case '(', '{', '[':
			scopes = append(scopes, char)
		case ')', '}', ']':
			match := byte('(')
			if char == '}' {
				match = '{'
			} else if char == ']' {
				match = '['
			}
			if len(scopes) == 0 || scopes[len(scopes)-1] != match {
				return nil
			}
			scopes = scopes[:len(scopes)-1]
		case ';':
			if len(scopes) == 0 {
				segments = append(segments, command[start:i])
				start = i + 1
			}
		}
	}
	if quote != 0 || len(scopes) != 0 {
		return nil
	}
	return append(segments, command[start:])
}

// Surrounding text can come from other operations in the same command.
// Preserve the matching document header itself as the batch read evidence.
func promptContextBatchSkillHeaders(body string) map[string]string {
	headers := make(map[string]string)
	boundaries := promptContextHeaderLine.FindAllStringIndex(body, -1)
	for i := 1; i < len(boundaries); i++ {
		start, end := boundaries[i-1], boundaries[i]
		names := promptContextSkillName.FindAllStringSubmatch(body[start[1]:end[0]], 2)
		tail := strings.TrimLeft(body[end[1]:], " \t\n")
		if len(names) != 1 || tail == "" || strings.HasPrefix(tail, "---\n") || tail == "---" {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(names[0][1]))
		if _, exists := headers[name]; exists {
			headers[name] = ""
		} else {
			headers[name] = body[start[0]:end[1]]
		}
	}
	return headers
}

func directPromptContextFile(path string) string {
	if strings.TrimSpace(path) == "" || strings.ContainsAny(path, "\r\n\x00*?[]") {
		return ""
	}
	return path
}

func promptContextWrappedCommand(fields string) (command, workDir string) {
	seen := make(map[string]bool)
	for fields = strings.TrimSpace(fields); fields != ""; fields = strings.TrimSpace(fields) {
		match := promptContextWrappedField.FindStringSubmatch(fields)
		if match == nil {
			return "", ""
		}
		name := match[1]
		if strings.HasPrefix(name, `"`) && json.Unmarshal([]byte(name), &name) != nil {
			return "", ""
		}
		if seen[name] {
			return "", ""
		}
		seen[name] = true
		if name == "cmd" && json.Unmarshal([]byte(match[2]), &command) != nil {
			return "", ""
		}
		if name == "workdir" && strings.HasPrefix(match[2], `"`) {
			_ = json.Unmarshal([]byte(match[2]), &workDir)
		}
		fields = fields[len(match[0]):]
		if fields != "" {
			if !strings.HasPrefix(fields, ",") {
				return "", ""
			}
			fields = fields[1:]
		}
	}
	return command, workDir
}

func promptContextReceiptBody(raw string, depth int) (string, string) {
	if depth >= 8 {
		return "", promptContextNoBody
	}
	if gjson.Valid(raw) {
		node := gjson.Parse(raw)
		if node.IsArray() {
			return promptContextArrayReceipt(node, depth)
		}
		if node.IsObject() {
			if node.Get("isError").Bool() || node.Get("is_error").Bool() || node.Get("success").Type == gjson.False {
				return "", promptContextReadFailed
			}
			if status := node.Get("exit_code"); status.Type == gjson.Number {
				if status.Num != 0 {
					return "", promptContextReadFailed
				}
				if output := node.Get("output"); output.Type == gjson.String {
					return nonemptyPromptContextBody(output.Str)
				}
			}
		}
	}
	text := promptContextNormalNewlines.Replace(raw)
	if match := promptContextExited.FindStringSubmatchIndex(text); match != nil {
		if text[match[2]:match[3]] != "0" {
			return "", promptContextReadFailed
		}
		return nonemptyPromptContextBody(text[match[1]:])
	}
	return "", promptContextNoBody
}

func promptContextArrayReceipt(node gjson.Result, depth int) (string, string) {
	var body string
	for _, child := range node.Array() {
		text := child.Get("text")
		if text.Type != gjson.String {
			continue
		}
		part, evidence := promptContextReceiptBody(text.Str, depth+1)
		if evidence == promptContextReadFailed {
			return "", evidence
		}
		if evidence == promptContextReadSuccess {
			if body != "" {
				return "", promptContextNoBody
			}
			body = part
		}
	}
	return nonemptyPromptContextBody(body)
}

func nonemptyPromptContextBody(output string) (string, string) {
	body := promptContextNormalNewlines.Replace(output)
	if strings.TrimSpace(body) == "" {
		return "", promptContextNoBody
	}
	return body, promptContextReadSuccess
}
