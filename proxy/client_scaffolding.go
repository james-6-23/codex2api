package proxy

import (
	"os"
	"regexp"
	"strings"
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
}

var clientScaffoldingReminderBlockPattern = regexp.MustCompile(`(?is)<system-reminder>\s*(.*?)\s*</system-reminder>`)

// clientScaffoldingLinePatterns match standalone scaffolding lines that appear
// outside a reminder block. Each is anchored to its own line so prose that
// merely mentions the wording is left alone.
var clientScaffoldingLinePatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?im)^[ \t]*Co-Authored-By:[ \t]*Claude Code[ \t]*<noreply@anthropic\.com>[ \t]*\r?\n?`),
	regexp.MustCompile(`(?im)^[ \t]*🤖[ \t]*Generated with \[Claude Code\]\(https://claude\.com/claude-code\)[ \t]*\r?\n?`),
	regexp.MustCompile(`(?im)^[ \t]*You are powered by the model[ \t]+\S+\.?[ \t]*\r?\n?`),
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
	out := text
	if clientScaffoldingReminderStripEnabled() {
		out = stripSystemReminderBlocks(out)
	}
	for _, pattern := range clientScaffoldingLinePatterns {
		out = pattern.ReplaceAllString(out, "")
	}
	return strings.TrimSpace(out)
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
