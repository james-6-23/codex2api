package proxy

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"
)

// Neutralizer for client-side agent scaffolding.
//
// Harnesses such as Claude Code prepend large <system-reminder> blocks, git
// attribution footers, and a "powered by the model" line to every request.
// None of it is useful to the upstream model, all of it is volatile between
// turns (working directory, git status, file lists), and it is the loudest
// structural fingerprint that the caller is not the official client.
//
// The pass is deliberately deterministic and idempotent: identical input always
// yields identical output, so an unchanged conversation keeps an identical
// prefix and the upstream implicit prompt cache still hits. Dropping the
// volatile blocks actively helps caching, because those blocks change between
// turns and would otherwise invalidate the cached prefix. It also stabilises the
// session id, which is seeded from the first user text: without stripping, that
// seed is a volatile reminder block, so every turn looks like a fresh session.
//
// The same function feeds deriveContentSessionSeed, so the local affinity key is
// derived from the content that is actually sent upstream rather than from
// scaffolding that is stripped on the way out. Identity and payload cannot
// disagree about what the conversation is.
//
// Only conversational text is rewritten. Structured payloads — function call
// arguments, function responses, images — are never touched.
const (
	clientScaffoldingStripEnv          = "CLIENT_SCAFFOLDING_STRIP"
	clientScaffoldingStripRemindersEnv = "CLIENT_SCAFFOLDING_STRIP_REMINDERS"
)

// clientScaffoldingMaxNormalizeBytes bounds how much text this pass will scan.
// Scaffolding is a small fragment a harness injects into conversational text,
// so a block past this size is a payload rather than a reminder — an inline
// image, a pasted log. The bound matters because deriveContentSessionSeed hands
// this function content.Raw, which carries base64 image bytes verbatim, and
// four regex passes across eight megabytes cost more than the request they are
// meant to stabilise. Beyond the bound the text is returned unchanged, which is
// what the callers did before the neutralizer existed.
const clientScaffoldingMaxNormalizeBytes = 1 << 20

// clientScaffoldingReminderMarkers identify a <system-reminder> block as
// harness scaffolding rather than text the caller actually pasted. A block is
// only removed when its body carries at least one marker, so a question that
// merely quotes a bare <system-reminder> tag survives untouched. Markers are
// matched case-insensitively.
var clientScaffoldingReminderMarkers = []string{
	"# environment",
	"you have been invoked in the following environment",
	"you are powered by the model",
	"available agent types for the agent tool",
	"following skills are available for use with the skill tool",
	"attribution for git commits and pull requests",
	"# session-specific guidance",
	"here is useful information about the environment",
	"this part of your instructions changes from turn to turn",
	"this part of your instructions changed since this session started",
	"the notes that were in your instructions when this session started have been cleared",
	"the openmausbot context notes from earlier in this conversation have been cleared",
}

var clientScaffoldingReminderBlockPattern = regexp.MustCompile(`(?is)<system-reminder>\s*(.*?)\s*</system-reminder>`)

// clientScaffoldingProductLinePatterns are OpenMausBot notices, truncation
// banners, provenance labels, memory fragments and volatile-context headers.
// They are safe on tool results as well as message text because each one names
// the harness explicitly rather than matching generic prose.
var clientScaffoldingProductLinePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?im)^[ \t]*\[OpenMausBot:[^\]\r\n]*\][ \t]*\r?\n?`),
	regexp.MustCompile(`(?im)^[ \t]*\[OpenMausBot started this again by itself, once:[^\]\r\n]*\][ \t]*\r?\n?`),
	regexp.MustCompile(`(?im)^[ \t]*\[(?:Payload|Event) truncated by OpenMausBot\][ \t]*\r?\n?`),
	regexp.MustCompile(`(?im)^[ \t]*⚠️?[ \t]*(?:TRUNCATED|SHAPED) by OpenMausBot:[^\r\n]*\r?\n?`),
	regexp.MustCompile(`(?im)^[ \t]*\[(?:Escalation notice from|From) OpenMausBot[^\]\r\n]*\][ \t]*\r?\n?`),
	regexp.MustCompile(`(?im)^[ \t]*\[From [^\]\r\n]*·[^\]\r\n]*·[^\]\r\n]*\][ \t]*\r?\n?`),
	regexp.MustCompile(`(?im)^[ \t]*\[Message from @[^\]\r\n]*OpenMausBot[^\]\r\n]*\][ \t]*\r?\n?`),
	regexp.MustCompile(`(?im)^[ \t]*(?:-[ \t]*)?OpenMausBot (?:already retried this run|did not retry it|cannot trace this work)[^\r\n]*\r?\n?`),
	regexp.MustCompile(`(?im)^[ \t]*Context from OpenMausBot updated since this conversation started; it replaces any earlier copy:[ \t]*\r?\n?`),
	regexp.MustCompile(`(?im)^[ \t]*The OpenMausBot context notes from earlier in this conversation have been cleared; the standing instructions still apply\.[ \t]*\r?\n?`),
	regexp.MustCompile(`(?im)^[ \t]*-[ \t]*\d{4}-\d{2}-\d{2}[ \t]*·[ \t]*learned by [^·\r\n]*[ \t]*·.*\r?\n?`),
	regexp.MustCompile(`(?i)[ \t]*TEST EVENT, started from OpenMausBot:[^\r\n]*`),
}

var clientScaffoldingBillingLinePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?im)^[ \t]*x-anthropic-billing-header:.*\r?\n?`),
	regexp.MustCompile(`(?im)^[ \t]*custom-billing-id:.*\r?\n?`),
	regexp.MustCompile(`(?im)^[ \t]*cc_version=.*\r?\n?`),
	regexp.MustCompile(`(?im)^[ \t]*cc_entrypoint=.*\r?\n?`),
	regexp.MustCompile(`(?im)^[ \t]*cch=.*\r?\n?`),
}

// clientScaffoldingLinePatterns match standalone scaffolding lines that appear
// outside a reminder block. Each is anchored to its own line so prose that
// merely mentions the wording is left alone.
var clientScaffoldingLinePatterns = func() []*regexp.Regexp {
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`(?im)^[ \t]*Co-Authored-By:[ \t]*Claude Code[ \t]*<noreply@anthropic\.com>[ \t]*\r?\n?`),
		regexp.MustCompile(`(?im)^[ \t]*🤖[ \t]*Generated with \[Claude Code\]\(https://claude\.com/claude-code\)[ \t]*\r?\n?`),
		regexp.MustCompile(`(?im)^[ \t]*You are powered by the model[ \t]+\S+\.?[ \t]*\r?\n?`),
	}
	patterns = append(patterns, clientScaffoldingBillingLinePatterns...)
	patterns = append(patterns, clientScaffoldingProductLinePatterns...)
	return patterns
}()

const clientScaffoldingVolatileContextPrefix = "context from openmausbot updated since this conversation started"

// clientScaffoldingMarkerPattern is the cheap pre-check that keeps ordinary
// prompts off the regex and code-scan path.
var clientScaffoldingMarkerPattern = regexp.MustCompile(`(?i)(?:openmausbot|mausbot|maus-bot|chief of staff|learned by |\[from |<system-reminder>|co-authored-by:|generated with \[claude code\]|you are powered by the model|you are claude code|you are codex|x-anthropic-billing-header:|custom-billing-id:|cc_version=|cc_entrypoint=|cch=)`)

// clientScaffoldingProductMarkerPattern is the narrower pre-check for tool
// results, which only need product notices and reminder blocks.
var clientScaffoldingProductMarkerPattern = regexp.MustCompile(`(?i)(?:openmausbot|mausbot|maus-bot|learned by |\[from |<system-reminder>)`)

var clientScaffoldingCodePlaceholder = regexp.MustCompile("\x00code([0-9]+)\x00")

// maskCodeBlocksAndApply runs apply over text with fenced blocks and inline
// code spans replaced by opaque placeholders, then restores them byte for byte.
// Fences are matched by length so a shorter fence inside a longer one does not
// end protection early.
func maskCodeBlocksAndApply(text string, apply func(string) string) string {
	if text == "" || apply == nil {
		return text
	}
	blocks := make([]string, 0, 2)
	var masked strings.Builder
	masked.Grow(len(text))
	paragraphEnd := 0
	for index := 0; index < len(text); {
		if end, ok := clientScaffoldingFencedBlockEnd(text, index); ok {
			block := text[index:end]
			suffix := ""
			if strings.HasSuffix(block, "\r\n") {
				block, suffix = block[:len(block)-2], "\r\n"
			} else if strings.HasSuffix(block, "\n") {
				block, suffix = block[:len(block)-1], "\n"
			}
			blocks = append(blocks, block)
			masked.WriteString(fmt.Sprintf("\x00code%d\x00", len(blocks)-1))
			masked.WriteString(suffix)
			index = end
			continue
		}
		if paragraphEnd <= index {
			paragraphEnd = clientScaffoldingParagraphEnd(text, index)
		}
		if end, ok := clientScaffoldingInlineCodeEnd(text, index, paragraphEnd); ok {
			blocks = append(blocks, text[index:end])
			masked.WriteString(fmt.Sprintf("\x00code%d\x00", len(blocks)-1))
			index = end
			continue
		}
		if text[index] == '`' {
			// No matching span. Copy the whole run so the next search starts
			// after it instead of rescanning the same suffix.
			for index < len(text) && text[index] == '`' {
				masked.WriteByte(text[index])
				index++
			}
			continue
		}
		masked.WriteByte(text[index])
		index++
	}
	if len(blocks) == 0 {
		return apply(text)
	}
	applied := apply(masked.String())
	return clientScaffoldingCodePlaceholder.ReplaceAllStringFunc(applied, func(token string) string {
		index, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(token, "\x00code"), "\x00"))
		if err != nil || index < 0 || index >= len(blocks) {
			return token
		}
		return blocks[index]
	})
}

// clientScaffoldingFencedBlockEnd returns the end of a fenced code block that
// starts at index, or false when index is not a valid opening fence or no
// closing fence follows. A closing fence must use the same character and be at
// least as long as the opening fence.
func clientScaffoldingFencedBlockEnd(text string, index int) (int, bool) {
	if index != 0 && text[index-1] != '\n' {
		return 0, false
	}
	cursor := index
	for cursor < len(text) && cursor-index < 3 && (text[cursor] == ' ' || text[cursor] == '\t') {
		cursor++
	}
	if cursor >= len(text) || (text[cursor] != '`' && text[cursor] != '~') {
		return 0, false
	}
	fence := text[cursor]
	fenceStart := cursor
	for cursor < len(text) && text[cursor] == fence {
		cursor++
	}
	fenceLength := cursor - fenceStart
	if fenceLength < 3 {
		return 0, false
	}
	openingLineEnd := strings.IndexByte(text[cursor:], '\n')
	if openingLineEnd < 0 {
		return 0, false
	}
	cursor += openingLineEnd + 1
	for cursor < len(text) {
		lineEnd := strings.IndexByte(text[cursor:], '\n')
		if lineEnd < 0 {
			lineEnd = len(text)
		} else {
			lineEnd += cursor
		}
		closing := cursor
		for closing < lineEnd && closing-cursor < 3 && (text[closing] == ' ' || text[closing] == '\t') {
			closing++
		}
		closeStart := closing
		for closing < lineEnd && text[closing] == fence {
			closing++
		}
		if closing-closeStart >= fenceLength && strings.TrimSpace(text[closing:lineEnd]) == "" {
			if lineEnd < len(text) {
				lineEnd++
			}
			return lineEnd, true
		}
		if lineEnd >= len(text) {
			break
		}
		cursor = lineEnd + 1
	}
	return 0, false
}

// clientScaffoldingInlineCodeEnd returns the end of an inline code span that
// starts at index, or false when the backtick run has no matching run before
// paragraphEnd. CommonMark permits a span to cross a single line ending, so the
// caller passes a paragraph boundary rather than the next newline.
func clientScaffoldingInlineCodeEnd(text string, index, paragraphEnd int) (int, bool) {
	if index >= len(text) || text[index] != '`' {
		return 0, false
	}
	runEnd := index
	for runEnd < len(text) && text[runEnd] == '`' {
		runEnd++
	}
	runLength := runEnd - index
	for cursor := runEnd; cursor < paragraphEnd; {
		if text[cursor] != '`' {
			cursor++
			continue
		}
		closeEnd := cursor
		for closeEnd < paragraphEnd && text[closeEnd] == '`' {
			closeEnd++
		}
		if closeEnd-cursor == runLength {
			return closeEnd, true
		}
		cursor = closeEnd
	}
	return 0, false
}

// clientScaffoldingParagraphEnd returns the first blank line or fenced-block
// opener after start. Whitespace-only lines count as blank, and fences interrupt
// paragraphs without a blank line, so both are boundaries for a code span.
func clientScaffoldingParagraphEnd(text string, start int) int {
	cursor := start
	for cursor < len(text) {
		lineEnd := strings.IndexByte(text[cursor:], '\n')
		if lineEnd < 0 {
			return len(text)
		}
		lineEnd += cursor
		line := text[cursor:lineEnd]
		if strings.TrimSpace(line) == "" && cursor > start {
			return cursor
		}
		if clientScaffoldingLineStartsFence(line) {
			return cursor
		}
		cursor = lineEnd + 1
	}
	return len(text)
}

func clientScaffoldingLineStartsFence(line string) bool {
	cursor := 0
	for cursor < len(line) && cursor < 3 && (line[cursor] == ' ' || line[cursor] == '\t') {
		cursor++
	}
	if cursor >= len(line) || (line[cursor] != '`' && line[cursor] != '~') {
		return false
	}
	fence := line[cursor]
	length := 0
	for cursor < len(line) && line[cursor] == fence {
		length++
		cursor++
	}
	return length >= 3
}

// clientScaffoldingStripEnabled reports whether the neutralizer is active. It
// defaults to on and is switched off by setting the env var to 0/false/no/off.
func clientScaffoldingStripEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(clientScaffoldingStripEnv))) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// clientScaffoldingReminderStripEnabled reports whether whole <system-reminder>
// blocks may be removed. Attribution lines are always stripped once the
// neutralizer is on; block removal is separable because it also drops harness
// hints (agent types, skill lists) that some clients rely on, so the safe half
// can be kept on its own.
func clientScaffoldingReminderStripEnabled() bool {
	if !clientScaffoldingStripEnabled() {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv(clientScaffoldingStripRemindersEnv))) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

// normalizeClientScaffolding removes client scaffolding from one text block.
// Determinism and idempotence are the contract: they are what keeps the upstream
// prompt cache warm across turns and the affinity key stable. Text past
// clientScaffoldingMaxNormalizeBytes is returned unchanged.
func normalizeClientScaffolding(text string) string {
	if text == "" || len(text) > clientScaffoldingMaxNormalizeBytes || !clientScaffoldingStripEnabled() {
		return text
	}
	if !clientScaffoldingMarkerPattern.MatchString(text) {
		return strings.TrimSpace(text)
	}
	return strings.TrimSpace(maskCodeBlocksAndApply(text, normalizeClientScaffoldingUnmasked))
}

// normalizeClientScaffoldingUnmasked runs the line patterns on text whose code
// blocks are already masked.
func normalizeClientScaffoldingUnmasked(text string) string {
	volatileBody := strings.HasPrefix(strings.ToLower(strings.TrimSpace(text)), clientScaffoldingVolatileContextPrefix)
	out := text
	if clientScaffoldingReminderStripEnabled() {
		out = stripSystemReminderBlocks(out)
	}
	for _, pattern := range clientScaffoldingLinePatterns {
		out = pattern.ReplaceAllString(out, "")
	}
	if volatileBody {
		out = obfuscateClientBrand(out)
	}
	return out
}

// normalizeProductScaffolding is the narrower pass used on tool results. It
// removes harness notices and memory fragments but leaves billing headers and
// other prose alone, because a tool result can be a raw file rather than a
// reminder block.
func normalizeProductScaffolding(text string) string {
	if text == "" || len(text) > clientScaffoldingMaxNormalizeBytes || !clientScaffoldingStripEnabled() {
		return text
	}
	if !clientScaffoldingProductMarkerPattern.MatchString(text) {
		return text
	}
	out := maskCodeBlocksAndApply(text, func(masked string) string {
		if clientScaffoldingReminderStripEnabled() {
			masked = stripSystemReminderBlocks(masked)
		}
		for _, pattern := range clientScaffoldingProductLinePatterns {
			masked = pattern.ReplaceAllString(masked, "")
		}
		return masked
	})
	if strings.TrimSpace(out) == "" {
		return ""
	}
	return out
}

var clientScaffoldingBrandPhrases = []string{
	"learned by MausBot",
	"OpenMausBot",
	"OpenMaus",
	"MausBot Cloud",
	"Maus-Bot",
	"MausBot",
	"My Cloud",
	"Chief of Staff",
}

// obfuscateClientBrand breaks exact product-name matches with a zero-width
// space. It is used for the bare volatile-context body, which is generated by
// OpenMausBot rather than typed by the user and has no block delimiter.
func obfuscateClientBrand(text string) string {
	return obfuscateWithPhrases(text, clientScaffoldingBrandPhrases)
}

// obfuscateWithPhrases breaks every listed phrase with a zero-width space. It
// repeats until no phrase matches, so overlapping names such as OpenMausBot and
// MausBot are both broken in one call. The result is deterministic and
// idempotent.
var obfuscationMatcherCache sync.Map

func obfuscationMatcher(phrases []string) *regexp.Regexp {
	key := strings.Join(phrases, "\x00")
	if cached, ok := obfuscationMatcherCache.Load(key); ok {
		return cached.(*regexp.Regexp)
	}
	escaped := make([]string, 0, len(phrases))
	for _, phrase := range phrases {
		phrase = strings.TrimSpace(phrase)
		if utf8.RuneCountInString(phrase) >= 2 && !strings.Contains(phrase, "\u200B") {
			escaped = append(escaped, regexp.QuoteMeta(phrase))
		}
	}
	if len(escaped) == 0 {
		return nil
	}
	matcher, err := regexp.Compile("(?i)" + strings.Join(escaped, "|"))
	if err != nil {
		return nil
	}
	actual, _ := obfuscationMatcherCache.LoadOrStore(key, matcher)
	return actual.(*regexp.Regexp)
}

func obfuscateWithPhrases(text string, phrases []string) string {
	if text == "" || len(phrases) == 0 {
		return text
	}
	matcher := obfuscationMatcher(phrases)
	if matcher == nil {
		return text
	}
	// Our phrase set has at most two levels of overlap. Three passes cover it
	// and keep the cost bounded when text is large.
	for pass := 0; pass < 3; pass++ {
		next := matcher.ReplaceAllStringFunc(text, func(match string) string {
			if strings.Contains(match, "\u200B") {
				return match
			}
			_, size := utf8.DecodeRuneInString(match)
			if size <= 0 || size >= len(match) {
				return match
			}
			return match[:size] + "\u200B" + match[size:]
		})
		if next == text {
			break
		}
		text = next
	}
	return text
}

// officialAntigravityIdentity is the identity the upstream Antigravity channel
// expects. Third-party harness preambles are rewritten to it so the request is
// byte-compatible with the official client.
const officialAntigravityIdentity = "You are Antigravity, a large language model built by Google."

// clientScaffoldingThirdPartyIdentityPatterns match the known identity
// preamble shapes only. They stop at the first sentence boundary so ordinary
// instructions that merely mention a client name are left intact.
var clientScaffoldingThirdPartyIdentityPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)^You are [^.\n]*?, a personal bot in (?:OpenMausBot|MausBot|Maus-Bot)\.\s*`),
	regexp.MustCompile(`(?i)^You are [^.\n]*?, a bot in the room "[^"\n]*" in (?:OpenMausBot|MausBot|Maus-Bot)\.\s*`),
	regexp.MustCompile(`(?i)^You are [^.\n]*?, an AI agent that [^.\n]*?(?:OpenMausBot|MausBot|Maus-Bot)[^.\n]*?\.\s*`),
	regexp.MustCompile(`(?i)^You are [^.\n]*?'s assistant and Chief of Staff: [^.\n]*?\.\s*`),
	regexp.MustCompile(`(?i)^You are the person's assistant and the Chief of Staff for the [^.\n]*? department: [^.\n]*?\.\s*`),
	regexp.MustCompile(`(?i)^You hold a Chief of Staff's tools[^.\n]*?\.\s*`),
	regexp.MustCompile(`(?i)^You are (?:Claude Code|Codex)\b[^.\n]*?\.\s*`),
}

func clientScaffoldingIdentityPreamble(text string) string {
	for _, pattern := range clientScaffoldingThirdPartyIdentityPatterns {
		if matched := pattern.FindString(text); matched != "" {
			return matched
		}
	}
	return ""
}

// normalizeAntigravitySystemInstruction strips harness scaffolding from a system
// instruction and rewrites a known third-party identity preamble to the official
// one. Code blocks are masked first: an example persona inside backticks is
// content, not an instruction.
func normalizeAntigravitySystemInstruction(system string) string {
	if system == "" || len(system) > clientScaffoldingMaxNormalizeBytes || !clientScaffoldingStripEnabled() {
		return system
	}
	if !clientScaffoldingMarkerPattern.MatchString(system) {
		return strings.TrimSpace(system)
	}
	return maskCodeBlocksAndApply(system, func(masked string) string {
		masked = normalizeClientScaffoldingUnmasked(masked)
		if matched := clientScaffoldingIdentityPreamble(masked); matched != "" {
			masked = officialAntigravityIdentity + " " + masked[len(matched):]
		}
		return masked
	})
}

func stripSystemReminderBlocks(text string) string {
	return clientScaffoldingReminderBlockPattern.ReplaceAllStringFunc(text, func(block string) string {
		match := clientScaffoldingReminderBlockPattern.FindStringSubmatch(block)
		if len(match) < 2 {
			return block
		}
		body := strings.ToLower(match[1])
		for _, marker := range clientScaffoldingReminderMarkers {
			if strings.Contains(body, marker) {
				return ""
			}
		}
		return block
	})
}

// normalizeClientScaffoldingParts normalizes message part text and drops text
// parts that end up empty, mirroring the empty-part semantics the callers
// already apply. Non-text parts pass through untouched.
func normalizeClientScaffoldingParts(parts []any) []any {
	if len(parts) == 0 || !clientScaffoldingStripEnabled() {
		return parts
	}
	out := make([]any, 0, len(parts))
	for _, part := range parts {
		partMap, ok := part.(map[string]any)
		if !ok {
			out = append(out, part)
			continue
		}
		text, ok := partMap["text"].(string)
		if !ok {
			out = append(out, part)
			continue
		}
		normalized := normalizeClientScaffolding(text)
		if normalized == "" {
			continue
		}
		partMap["text"] = normalized
		out = append(out, partMap)
	}
	return out
}
