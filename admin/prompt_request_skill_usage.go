package admin

import (
	"path"
	"regexp"
	"strings"

	"github.com/tidwall/gjson"
)

type promptTaskStepSkillUsage struct {
	Name            string `json:"name"`
	Path            string `json:"path"`
	Source          string `json:"source"`
	SourceRequestID int64  `json:"source_request_id"`
	Command         string `json:"command"`
	Evidence        string `json:"evidence"`
}

type promptTaskSkillDirectory struct {
	name      string
	requestID int64
}

type promptTaskSkillSession struct {
	usage     *promptTaskStepSkillUsage
	startedAt int64
	pollCall  string
}

const (
	promptTaskSkillCommandLimit = 2048
	promptTaskSkillCommandToken = `(?:'[^'\r\n]*'|"[^"\r\n]*"|[-A-Za-z0-9_./:=,\\]+)`
	promptTaskSkillBinPrefix    = `(?:/usr/(?:local/)?bin/|/bin/)?`
	promptTaskSkillDashDash     = `(?:[ \t]+--)?[ \t]+`
	promptTaskSkillPythonFlags  = `(?:[ \t]+-(?:u|B|E|I|s|S))*`
)

// Allow only options that leave the following argument as a script path.
// Inline code, modules, and shell substitutions do not prove that a helper ran.
var promptTaskSkillCommand = regexp.MustCompile(`^(?:&[ \t]+|` +
	promptTaskSkillBinPrefix + `(?:bash|sh)(?:[ \t]+(?:-[euxv]+|--noprofile|--norc))*` + promptTaskSkillDashDash + `|` +
	promptTaskSkillBinPrefix + `python(?:[23](?:\.[0-9]+)?)?(?:\.exe)?` + promptTaskSkillPythonFlags + promptTaskSkillDashDash + `|` +
	`(?i:py(?:\.exe)?)[ \t]+-3(?:\.[0-9]+)?` + promptTaskSkillPythonFlags + promptTaskSkillDashDash + `|` +
	promptTaskSkillBinPrefix + `node(?:\.exe)?(?:[ \t]+(?:--no-warnings|--enable-source-maps))*` + promptTaskSkillDashDash + `|` +
	`(?i:(?:pwsh|powershell)(?:\.exe)?(?:[ \t]+-(?:noprofile|nologo|noninteractive|executionpolicy[ \t]+(?:bypass|remotesigned|allsigned|restricted|unrestricted|default|undefined)))*[ \t]+-file[ \t]+)` +
	`)?(` + promptTaskSkillCommandToken + `)(?:[ \t]+` + promptTaskSkillCommandToken + `)*$`)

var promptTaskSkillRunningSession = regexp.MustCompile(
	`^Chunk ID:[^\n]*\nWall time:[^\n]*\nProcess running with session ID ([1-9][0-9]{0,9})\n(?:Original token count:[^\n]*\n)?(?:Final )?Output:\n`,
)

var promptTaskSkillSessionNumber = regexp.MustCompile(`^[1-9][0-9]{0,9}$`)

func (p *promptTaskProjection) rememberLoadedSkill(evidence promptRequestContextSection, workDir string) {
	if evidence.Source == "user_message" {
		return
	}
	fileName := promptTaskSkillAbsolutePath(evidence.Path, workDir)
	if fileName == "" || !strings.EqualFold(path.Base(fileName), "SKILL.md") {
		return
	}
	dir := path.Dir(fileName)
	if previous, present := p.loadedDirectories[dir]; present {
		if previous.name != evidence.Name {
			// Two different names from one directory cannot identify one of them.
			p.loadedDirectories[dir] = promptTaskSkillDirectory{}
		}
		return
	}
	p.loadedDirectories[dir] = promptTaskSkillDirectory{name: evidence.Name, requestID: evidence.SourceRequestID}
}

func (p *promptTaskProjection) matchSkillUsage(command, workDir string, requestID int64) *promptTaskStepSkillUsage {
	if len(p.loadedDirectories) == 0 || len(command) > promptTaskSkillCommandLimit ||
		strings.ContainsAny(command, ";|<>()\r\n\x00`$*?[]{}#") {
		return nil
	}
	command = strings.TrimSpace(command)
	match := promptTaskSkillCommand.FindStringSubmatchIndex(command)
	if match == nil {
		return nil
	}
	helper := command[match[2]:match[3]]
	if helper[0] == '\'' || helper[0] == '"' {
		if match[2] == 0 {
			// Without an invocation operator, PowerShell can print a quoted path.
			return nil
		}
		helper = helper[1 : len(helper)-1]
	}
	// A bare executable may have come from PATH instead of the working directory.
	if !strings.ContainsAny(helper, `/\`) || strings.HasPrefix(helper, "-") || strings.ContainsAny(helper, `'"`) {
		return nil
	}
	fileName := promptTaskSkillAbsolutePath(helper, workDir)
	switch strings.ToLower(path.Ext(fileName)) {
	case ".sh", ".bash", ".py", ".ps1", ".bat", ".cmd", ".exe", ".js", ".mjs", ".cjs":
	default:
		return nil
	}
	for dir := path.Dir(fileName); dir != "." && dir != "/"; dir = path.Dir(dir) {
		if skill, present := p.loadedDirectories[dir]; present {
			if skill.name == "" || skill.requestID > requestID {
				return nil
			}
			return &promptTaskStepSkillUsage{
				Name: skill.name, Path: fileName, Source: "skill_helper_execution",
				Command: command[:match[3]],
			}
		}
	}
	return nil
}

func promptTaskSkillAbsolutePath(fileName, workDir string) string {
	backslashes := strings.ContainsRune(fileName, '\\')
	workDirBackslashes := strings.ContainsRune(workDir, '\\')
	fileName = strings.ReplaceAll(fileName, `\`, "/")
	workDir = strings.ReplaceAll(workDir, `\`, "/")
	windowsFile, windowsWorkDir := promptTaskSkillDrivePath(fileName), promptTaskSkillDrivePath(workDir)
	unixFile, unixWorkDir := strings.HasPrefix(fileName, "/"), strings.HasPrefix(workDir, "/")
	if fileName == "" || strings.ContainsAny(fileName, "\r\n\x00*$?[]") ||
		strings.Contains("/"+fileName+"/", "/../") || strings.HasPrefix(fileName, "//") ||
		(workDirBackslashes && !windowsWorkDir) || (unixFile && (backslashes || windowsWorkDir)) ||
		(windowsFile && unixWorkDir) || (backslashes && !windowsFile && !windowsWorkDir) {
		return ""
	}
	if !unixFile && !windowsFile {
		if (!unixWorkDir && !windowsWorkDir) || strings.Contains("/"+workDir+"/", "/../") ||
			strings.ContainsAny(workDir, "\r\n\x00*$?[]") || strings.ContainsRune(fileName, ':') ||
			strings.HasPrefix(workDir, "//") {
			return ""
		}
		fileName = workDir + "/" + fileName
	}
	return path.Clean(fileName)
}

func promptTaskSkillDrivePath(fileName string) bool {
	return len(fileName) >= 3 && fileName[1:3] == ":/" &&
		((fileName[0] >= 'a' && fileName[0] <= 'z') || (fileName[0] >= 'A' && fileName[0] <= 'Z'))
}

func promptTaskSkillUsageReceipt(raw string) string {
	_, status := promptContextReceiptBody(raw, 0)
	if status == promptContextReadSuccess {
		return "Process exited with code 0"
	}
	if status == promptContextReadFailed {
		return ""
	}
	// An explicitly successful single execution can be silent.
	if gjson.Valid(raw) {
		receipt := gjson.Parse(raw)
		if exitCode, output := receipt.Get("exit_code"), receipt.Get("output"); exitCode.Type == gjson.Number &&
			exitCode.Num == 0 && output.Type == gjson.String && strings.TrimSpace(output.Str) == "" {
			return "Process exited with code 0"
		}
	}
	text := promptContextNormalNewlines.Replace(raw)
	if match := promptContextExited.FindStringSubmatchIndex(text); match != nil &&
		text[match[2]:match[3]] == "0" && strings.TrimSpace(text[match[1]:]) == "" {
		return "Process exited with code 0"
	}
	return ""
}

func promptTaskSkillSessionID(raw string) string {
	if match := promptTaskSkillRunningSession.FindStringSubmatch(promptContextNormalNewlines.Replace(raw)); match != nil {
		return match[1]
	}
	return ""
}

func promptTaskSkillPollInput(raw string) (id string, empty bool) {
	if len(raw) > promptTaskSkillCommandLimit || !gjson.Valid(raw) {
		return "", false
	}
	input := gjson.Parse(raw)
	if !input.IsObject() {
		return "", false
	}
	empty = true
	seen := make(map[string]bool)
	input.ForEach(func(key, value gjson.Result) bool {
		if seen[key.Str] {
			empty = false
			if key.Str == "session_id" {
				id = ""
				return false
			}
		}
		seen[key.Str] = true
		switch key.Str {
		case "session_id":
			if value.Type == gjson.Number && promptTaskSkillSessionNumber.MatchString(value.Raw) {
				id = value.Raw
			} else {
				empty = false
			}
		case "chars":
			empty = empty && value.Type == gjson.String && value.Str == ""
		case "max_output_tokens", "yield_time_ms":
			empty = empty && (value.Type == gjson.Number || value.Type == gjson.Null)
		default:
			empty = false
		}
		return true
	})
	return id, empty
}

func (p *promptTaskProjection) beginSkillPoll(tool *promptTaskTool, arguments string, requestID int64) {
	if p.skillSessions == nil || tool.Name != "write_stdin" {
		return
	}
	id, empty := promptTaskSkillPollInput(arguments)
	if id == "" {
		for _, session := range p.skillSessions {
			if session.usage != nil && session.usage.SourceRequestID == 0 {
				session.usage = nil
			}
		}
		return
	}
	session := p.skillSessions[id]
	if session == nil || session.usage == nil || session.usage.SourceRequestID != 0 {
		return
	}
	if !empty || session.pollCall != "" || tool.CallID == "" || requestID < session.startedAt {
		session.usage = nil
		return
	}
	session.pollCall, tool.skillPollID = tool.CallID, id
}

func (p *promptTaskProjection) observeSkillSession(tool *promptTaskTool, result promptRequestToolResult, requestID int64) {
	if p.skillSessions == nil || tool.CallID == "" || result.FromRequestID < requestID {
		return
	}
	if tool.Name == "exec_command" && !result.failed {
		id := promptTaskSkillSessionID(tool.Result)
		if id == "" {
			return
		}
		if previous := p.skillSessions[id]; previous != nil {
			// A reused session identifier cannot identify one tool reliably.
			previous.usage = nil
			return
		}
		p.skillSessions[id] = &promptTaskSkillSession{usage: tool.skillUsage, startedAt: result.FromRequestID}
		tool.skillSessionID = id
		return
	}
	session := p.skillSessions[tool.skillPollID]
	if session == nil || session.usage == nil || session.pollCall != tool.CallID {
		return
	}
	session.pollCall = ""
	if !result.failed {
		if promptTaskSkillSessionID(tool.Result) == tool.skillPollID {
			session.startedAt = result.FromRequestID
			return
		}
		if evidence := promptTaskSkillUsageReceipt(tool.Result); evidence != "" {
			session.usage.SourceRequestID, session.usage.Evidence = result.FromRequestID, evidence
			return
		}
	}
	session.usage = nil
}

func (p *promptTaskProjection) appendPolledSkillUsages() {
	for i := range p.detail.Steps {
		step := &p.detail.Steps[i]
		for _, tool := range step.Tools {
			session := p.skillSessions[tool.skillSessionID]
			if session != nil && session.usage != nil && session.usage.SourceRequestID > 0 {
				step.SkillUsage = append(step.SkillUsage, *session.usage)
			}
		}
	}
}
