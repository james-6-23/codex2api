package admin

import (
	"context"
	"regexp"
	"sort"
	"strings"

	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
)

const (
	promptContextSystem    = "system_prompt"
	promptContextDeveloper = "developer_prompt"
	promptContextAgents    = "agents_md"
	promptContextCatalog   = "available_skills"
	promptContextSkill     = "loaded_skill"
	promptContextAttempt   = "attempted_skill"
	promptContextNew       = "new"
	promptContextInherited = "inherited"
	promptContextReceived  = "received"
)

type promptRequestContext struct {
	Sections []promptRequestContextSection `json:"sections"`
}

type promptRequestContextSection struct {
	Kind            string                      `json:"kind"`
	State           string                      `json:"state"`
	Text            string                      `json:"text,omitempty"`
	Name            string                      `json:"name,omitempty"`
	Path            string                      `json:"path,omitempty"`
	Source          string                      `json:"source,omitempty"`
	Evidence        string                      `json:"evidence,omitempty"`
	Skills          []promptRequestContextSkill `json:"skills,omitempty"`
	SourceRequestID int64                       `json:"source_request_id,omitempty"`
}

type promptRequestContextSkill struct {
	Name string `json:"name"`
	Path string `json:"path,omitempty"`
}

// analyzePromptRequest uses the same history comparison and receipt attribution
// for admin display and analysis export. The result contains private data.
func (h *Handler) analyzePromptRequest(ctx context.Context, request database.PromptRequestBody) promptRequestPresentation {
	view := presentPromptRequest(request, -1)
	changes, previous := h.requestChanges(ctx, request, view)
	view.Changes = changes
	view.Result = h.requestResult(ctx, request)
	if view.bodyFields == nil {
		request.Body = nil
	}
	view.PromptContext = projectPromptRequestContext(request, previous, changes)
	view.PromptContext.Sections = append(view.PromptContext.Sections, h.promptContextReadSections(ctx, request, changes, view.Result)...)
	return view
}

var promptContextInstructionFields = []string{"instructions", "system", "systemInstruction", "system_instruction"}

func projectPromptRequestContext(request database.PromptRequestBody, previous []byte, changes *promptRequestChanges) promptRequestContext {
	sections := make([]promptRequestContextSection, 0)
	for _, field := range promptContextInstructionFields {
		value := gjson.GetBytes(request.Body, field)
		if text := promptContextText(value, 0); strings.TrimSpace(text) != "" {
			state := promptContextReceived
			if changes != nil {
				state = promptContextNew
				if samePromptRequestJSON(gjson.GetBytes(previous, field), value) {
					state = promptContextInherited
				}
			}
			sections = appendPromptContextSections(sections, text, promptContextSystem, state, field)
		}
	}
	for _, field := range promptRequestHistoryFields {
		value := gjson.GetBytes(request.Body, field)
		for index, item := range value.Array() {
			if kind := item.Get("type").Str; kind != "" && kind != "message" {
				continue
			}
			role := item.Get("role").Str
			var kind, source string
			switch role {
			case "developer":
				kind, source = promptContextDeveloper, "developer_message"
			case "system":
				kind, source = promptContextSystem, "system_message"
			case "user", "":
				source = "user_message"
			default:
				continue
			}
			state := promptContextHistoryState(changes, field, index)
			sections = appendPromptContextSections(sections, promptContextText(item, 0), kind, state, source)
		}
	}
	return promptRequestContext{Sections: sections}
}

func promptContextHistoryState(changes *promptRequestChanges, field string, index int) string {
	if changes == nil || changes.historyField != field {
		return promptContextReceived
	}
	if index < changes.RepeatedItems {
		return promptContextInherited
	}
	return promptContextNew
}

func promptContextText(value gjson.Result, depth int) string {
	if depth >= 24 {
		return ""
	}
	if value.Type == gjson.String {
		return value.Str
	}
	if value.IsObject() {
		for _, key := range []string{"content", "parts", "text"} {
			if child := value.Get(key); child.Exists() {
				return promptContextText(child, depth+1)
			}
		}
		return ""
	}
	if value.IsArray() {
		var parts []string
		value.ForEach(func(_, child gjson.Result) bool {
			if text := promptContextText(child, depth+1); text != "" {
				parts = append(parts, text)
			}
			return true
		})
		return strings.Join(parts, "\n")
	}
	return ""
}

var (
	promptContextAgentsBlock     = regexp.MustCompile(`(?m)^# AGENTS\.md instructions for[ \t]+([^\n]+)\n[ \t\n]*<INSTRUCTIONS>[ \t]*\n([\s\S]*?)^</INSTRUCTIONS>[ \t]*$`)
	promptContextCatalogBlock    = regexp.MustCompile(`(?m)^<(skills_instructions|available_skills)>[ \t]*\n([\s\S]*?)^</(skills_instructions|available_skills)>[ \t]*$`)
	promptContextCatalogHeading  = regexp.MustCompile(`(?im)^#{2,4}[ \t]+Available skills[ \t]*\n`)
	promptContextHeading         = regexp.MustCompile(`(?m)^#{1,4}[ \t]+`)
	promptContextSectionBoundary = regexp.MustCompile(`(?im)^(?:#{1,4}[ \t]+|</?[a-z_]+>[ \t]*$)`)
	promptContextCatalogEntry    = regexp.MustCompile(`(?m)^[ \t]*-[ \t]+([^:\n]+):[^\n]*?\((?:file:[ \t]*)?([^()\n]+[/\\]SKILL\.md)\)[ \t]*$`)
	promptContextXMLCatalogEntry = regexp.MustCompile(`<skill>[\s\S]*?<name>([^<>\n]+)</name>([\s\S]*?)</skill>`)
	promptContextXMLPath         = regexp.MustCompile(`<(?:path|location)>([^<>\n]+)</(?:path|location)>`)
	promptContextSkillBlock      = regexp.MustCompile(`(?m)^<skill>[ \t]*\n[ \t]*<name>([^<>\n]+)</name>[ \t]*\n[ \t]*<path>([^<>\n]+)</path>[ \t]*\n([\s\S]*?)^</skill>[ \t]*$`)
	promptContextNormalNewlines  = strings.NewReplacer("\r\n", "\n", "\r", "\n")
)

type promptContextMatch struct {
	start, end int
	section    promptRequestContextSection
}

func appendPromptContextSections(sections []promptRequestContextSection, raw, defaultKind, state, source string) []promptRequestContextSection {
	if strings.TrimSpace(raw) == "" {
		return sections
	}
	text := promptContextNormalNewlines.Replace(raw)
	trimmed := strings.TrimSpace(text)
	matches := matchPromptContextBlocks(text)
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].start == matches[j].start {
			return matches[i].end > matches[j].end
		}
		return matches[i].start < matches[j].start
	})
	addPlain := func(segment string) {
		if defaultKind != "" && strings.TrimSpace(segment) != "" {
			sections = append(sections, promptRequestContextSection{Kind: defaultKind, State: state, Source: source, Text: segment})
		}
	}
	offset := 0
	for _, match := range matches {
		if match.start < offset || (source == "user_message" && match.section.Kind == promptContextSkill &&
			strings.TrimSpace(text[match.start:match.end]) != trimmed) {
			continue
		}
		addPlain(text[offset:match.start])
		section := match.section
		section.State, section.Source, section.Text = state, source, text[match.start:match.end]
		sections = append(sections, section)
		offset = match.end
	}
	addPlain(text[offset:])
	return sections
}

func matchPromptContextBlocks(text string) []promptContextMatch {
	var matches []promptContextMatch
	for _, span := range promptContextAgentsBlock.FindAllStringSubmatchIndex(text, -1) {
		if strings.TrimSpace(text[span[4]:span[5]]) == "" {
			continue
		}
		path := strings.TrimSpace(text[span[2]:span[3]])
		if promptContextFileName(path) != "AGENTS.md" {
			path = strings.TrimRight(path, "/\\") + "/AGENTS.md"
		}
		matches = append(matches, promptContextMatch{span[0], span[1], promptRequestContextSection{
			Kind: promptContextAgents, Path: path, Evidence: "injected_body",
		}})
	}
	addCatalog := func(start, end int, body string) {
		if skills := parsePromptContextCatalog(body); len(skills) > 0 {
			matches = append(matches, promptContextMatch{start, end, promptRequestContextSection{Kind: promptContextCatalog, Skills: skills}})
		}
	}
	for _, span := range promptContextCatalogBlock.FindAllStringSubmatchIndex(text, -1) {
		if text[span[2]:span[3]] == text[span[6]:span[7]] {
			addCatalog(span[0], span[1], text[span[4]:span[5]])
		}
	}
	for _, span := range promptContextCatalogHeading.FindAllStringIndex(text, -1) {
		end := len(text)
		if next := promptContextSectionBoundary.FindStringIndex(text[span[1]:]); next != nil {
			end = span[1] + next[0]
		}
		addCatalog(span[0], end, text[span[0]:end])
	}
	for _, span := range promptContextSkillBlock.FindAllStringSubmatchIndex(text, -1) {
		name := strings.TrimSpace(text[span[2]:span[3]])
		path := strings.TrimSpace(text[span[4]:span[5]])
		body := strings.TrimSpace(strings.NewReplacer("<content>", "", "</content>", "").Replace(text[span[6]:span[7]]))
		if name == "" || promptContextFileName(path) != "SKILL.md" || body == "" ||
			strings.HasPrefix(body, "<description>") || strings.HasPrefix(body, "<location>") {
			continue
		}
		matches = append(matches, promptContextMatch{span[0], span[1], promptRequestContextSection{
			Kind: promptContextSkill, Name: name, Path: path, Evidence: "injected_body",
		}})
	}
	return matches
}

func parsePromptContextCatalog(text string) []promptRequestContextSkill {
	var skills []promptRequestContextSkill
	if heading := promptContextCatalogHeading.FindStringIndex(text); heading != nil {
		entries := text[heading[1]:]
		if end := promptContextHeading.FindStringIndex(entries); end != nil {
			entries = entries[:end[0]]
		}
		for _, match := range promptContextCatalogEntry.FindAllStringSubmatch(entries, -1) {
			skills = append(skills, promptRequestContextSkill{Name: strings.TrimSpace(match[1]), Path: strings.TrimSpace(match[2])})
		}
		return skills
	}
	for _, match := range promptContextXMLCatalogEntry.FindAllStringSubmatch(text, -1) {
		skill := promptRequestContextSkill{Name: strings.TrimSpace(match[1])}
		if path := promptContextXMLPath.FindStringSubmatch(match[2]); path != nil {
			skill.Path = strings.TrimSpace(path[1])
		}
		skills = append(skills, skill)
	}
	return skills
}

func promptContextFileName(path string) string {
	return path[strings.LastIndexAny(path, "/\\")+1:]
}
