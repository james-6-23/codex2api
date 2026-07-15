package promptfilter

import (
	"encoding/json"
	"fmt"
	"regexp"
	"regexp/syntax"
	"sort"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/tidwall/gjson"
)

const (
	ActionAllow = "allow"
	ActionWarn  = "warn"
	ActionBlock = "block"

	ModeMonitor = "monitor"
	ModeWarn    = "warn"
	ModeBlock   = "block"

	DefaultThreshold       = 50
	DefaultStrictThreshold = 90
	DefaultMaxTextLength   = 80 * 1024
	defaultHeadScanLength  = 64 * 1024
	defaultTailScanLength  = 16 * 1024
	HitStartMarker         = "⟦PF_HIT⟧"
	HitEndMarker           = "⟦/PF_HIT⟧"
)

type Config struct {
	Enabled               bool            `json:"enabled"`
	Mode                  string          `json:"mode"`
	Threshold             int             `json:"threshold"`
	StrictThreshold       int             `json:"strict_threshold"`
	StrictTerminalEnabled bool            `json:"strict_terminal_enabled"`
	LogMatches            bool            `json:"log_matches"`
	MaxTextLength         int             `json:"max_text_length"`
	SensitiveWords        string          `json:"sensitive_words"`
	CustomPatterns        []PatternConfig `json:"custom_patterns"`
	DisabledPatterns      []string        `json:"disabled_patterns"`
	Review                ReviewConfig    `json:"review"`
	Advanced              AdvancedConfig  `json:"advanced"`
}

type ReviewConfig struct {
	Enabled        bool   `json:"enabled"`
	APIKey         string `json:"api_key,omitempty"`
	BaseURL        string `json:"base_url"`
	Model          string `json:"model"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	FailClosed     bool   `json:"fail_closed"`
}

type PatternConfig struct {
	Name            string   `json:"name"`
	Pattern         string   `json:"pattern"`
	Weight          int      `json:"weight"`
	Category        string   `json:"category,omitempty"`
	Strict          bool     `json:"strict,omitempty"`
	Enabled         *bool    `json:"enabled,omitempty"`
	AllPatterns     []string `json:"all_patterns,omitempty"`
	AnyPatterns     []string `json:"any_patterns,omitempty"`
	ExcludePatterns []string `json:"exclude_patterns,omitempty"`
	MinMatches      int      `json:"min_matches,omitempty"`
}

type Match struct {
	Name     string `json:"name"`
	Weight   int    `json:"weight"`
	Category string `json:"category,omitempty"`
	Strict   bool   `json:"strict,omitempty"`
}

type Verdict struct {
	Enabled             bool    `json:"enabled"`
	Mode                string  `json:"mode"`
	Action              string  `json:"action"`
	Score               int     `json:"score"`
	RawScore            int     `json:"raw_score"`
	Threshold           int     `json:"threshold"`
	StrictHit           bool    `json:"strict_hit"`
	TerminalStrictHit   bool    `json:"terminal_strict_hit"`
	TerminalCategoryHit bool    `json:"terminal_category_hit"`
	Matched             []Match `json:"matched"`
	Reason              string  `json:"reason,omitempty"`
	TextPreview         string  `json:"text_preview,omitempty"`
	FullText            string  `json:"full_text,omitempty"`
	ExtractedChars      int     `json:"extracted_chars"`
	Reviewed            bool    `json:"reviewed,omitempty"`
	ReviewFlagged       bool    `json:"review_flagged,omitempty"`
	ReviewError         string  `json:"review_error,omitempty"`
	ReviewModel         string  `json:"review_model,omitempty"`
}

type Engine struct {
	cfg            Config
	patterns       []compiledPattern
	sensitiveWords []string
	literalIndex   *literalIndex
}

type compiledPattern struct {
	cfg      PatternConfig
	re       *regexp.Regexp
	requires []string
	all      []*regexp.Regexp
	any      []*regexp.Regexp
	exclude  []*regexp.Regexp
}

type literalIndex struct {
	literals []literalNeedle
}

type literalNeedle struct {
	text string
}

func DefaultConfig() Config {
	return Config{
		Enabled:         false,
		Mode:            ModeMonitor,
		Threshold:       DefaultThreshold,
		StrictThreshold: DefaultStrictThreshold,
		LogMatches:      true,
		MaxTextLength:   DefaultMaxTextLength,
		Review:          DefaultReviewConfig(),
		Advanced:        DefaultAdvancedConfig(),
	}
}

func DefaultReviewConfig() ReviewConfig {
	return ReviewConfig{
		BaseURL:        DefaultReviewBaseURL,
		Model:          DefaultReviewModel,
		TimeoutSeconds: DefaultReviewTimeoutSeconds,
		FailClosed:     true,
	}
}

func ParseCustomPatterns(raw string) ([]PatternConfig, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var out []PatternConfig
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return nil, fmt.Errorf("invalid custom_patterns JSON: %w", err)
	}
	return out, nil
}

func MarshalCustomPatterns(patterns []PatternConfig) string {
	if len(patterns) == 0 {
		return "[]"
	}
	data, err := json.Marshal(patterns)
	if err != nil {
		return "[]"
	}
	return string(data)
}

func ParseDisabledPatterns(raw string) ([]string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var names []string
	if err := json.Unmarshal([]byte(raw), &names); err != nil {
		return nil, fmt.Errorf("invalid disabled_patterns JSON: %w", err)
	}
	return normalizePatternNames(names), nil
}

func MarshalDisabledPatterns(names []string) string {
	names = normalizePatternNames(names)
	if len(names) == 0 {
		return "[]"
	}
	data, err := json.Marshal(names)
	if err != nil {
		return "[]"
	}
	return string(data)
}

func NormalizeConfig(cfg Config) Config {
	defaults := DefaultConfig()
	if strings.TrimSpace(cfg.Mode) == "" {
		cfg.Mode = defaults.Mode
	}
	switch strings.ToLower(strings.TrimSpace(cfg.Mode)) {
	case ModeBlock:
		cfg.Mode = ModeBlock
	case ModeWarn:
		cfg.Mode = ModeWarn
	default:
		cfg.Mode = ModeMonitor
	}
	if cfg.Threshold <= 0 {
		cfg.Threshold = defaults.Threshold
	}
	if cfg.Threshold > 500 {
		cfg.Threshold = 500
	}
	if cfg.StrictThreshold <= 0 {
		cfg.StrictThreshold = defaults.StrictThreshold
	}
	if cfg.StrictThreshold < cfg.Threshold {
		cfg.StrictThreshold = cfg.Threshold
	}
	if cfg.StrictThreshold > 1000 {
		cfg.StrictThreshold = 1000
	}
	if cfg.MaxTextLength <= 0 {
		cfg.MaxTextLength = defaults.MaxTextLength
	}
	if cfg.MaxTextLength > 1024*1024 {
		cfg.MaxTextLength = 1024 * 1024
	}
	cfg.DisabledPatterns = normalizePatternNames(cfg.DisabledPatterns)
	cfg.Review = NormalizeReviewConfig(cfg.Review)
	cfg.Advanced = NormalizeAdvancedConfig(cfg.Advanced)
	return cfg
}

var engineCache sync.Map // map[string]*Engine

func engineForConfig(cfg Config) (*Engine, error) {
	key := engineCacheKey(cfg)
	if cached, ok := engineCache.Load(key); ok {
		return cached.(*Engine), nil
	}
	engine, err := NewEngine(cfg)
	if err != nil {
		return nil, err
	}
	actual, _ := engineCache.LoadOrStore(key, engine)
	return actual.(*Engine), nil
}

func engineCacheKey(cfg Config) string {
	cfg = NormalizeConfig(cfg)
	key := struct {
		Enabled               bool            `json:"enabled"`
		Mode                  string          `json:"mode"`
		Threshold             int             `json:"threshold"`
		StrictThreshold       int             `json:"strict_threshold"`
		StrictTerminalEnabled bool            `json:"strict_terminal_enabled"`
		LogMatches            bool            `json:"log_matches"`
		MaxTextLength         int             `json:"max_text_length"`
		SensitiveWords        string          `json:"sensitive_words"`
		CustomPatterns        []PatternConfig `json:"custom_patterns"`
		DisabledPatterns      []string        `json:"disabled_patterns"`
		Advanced              AdvancedConfig  `json:"advanced"`
	}{
		Enabled:               cfg.Enabled,
		Mode:                  cfg.Mode,
		Threshold:             cfg.Threshold,
		StrictThreshold:       cfg.StrictThreshold,
		StrictTerminalEnabled: cfg.StrictTerminalEnabled,
		LogMatches:            cfg.LogMatches,
		MaxTextLength:         cfg.MaxTextLength,
		SensitiveWords:        cfg.SensitiveWords,
		CustomPatterns:        cfg.CustomPatterns,
		DisabledPatterns:      cfg.DisabledPatterns,
		Advanced:              cfg.Advanced,
	}
	data, err := json.Marshal(key)
	if err != nil {
		return fmt.Sprintf("%t|%s|%d|%d|%t|%t|%d|%s|%s|%s|%s", cfg.Enabled, cfg.Mode, cfg.Threshold, cfg.StrictThreshold, cfg.StrictTerminalEnabled, cfg.LogMatches, cfg.MaxTextLength, cfg.SensitiveWords, MarshalCustomPatterns(cfg.CustomPatterns), MarshalDisabledPatterns(cfg.DisabledPatterns), MarshalAdvancedConfig(cfg.Advanced))
	}
	return string(data)
}

func NewEngine(cfg Config) (*Engine, error) {
	cfg = NormalizeConfig(cfg)
	disabled := disabledPatternSet(cfg.DisabledPatterns)
	merged := append([]PatternConfig{}, defaultPatternConfigs...)
	merged = append(merged, cfg.CustomPatterns...)

	patterns := make([]compiledPattern, 0, len(merged))
	for _, pattern := range merged {
		pattern.Name = strings.TrimSpace(pattern.Name)
		pattern.Pattern = strings.TrimSpace(pattern.Pattern)
		pattern.Category = strings.TrimSpace(pattern.Category)
		if pattern.Name == "" || (pattern.Pattern == "" && len(pattern.AllPatterns) == 0 && len(pattern.AnyPatterns) == 0) || pattern.Weight <= 0 {
			continue
		}
		if disabled[strings.ToLower(pattern.Name)] {
			continue
		}
		if pattern.Enabled != nil && !*pattern.Enabled {
			continue
		}
		var re *regexp.Regexp
		var err error
		if pattern.Pattern != "" {
			re, err = regexp.Compile(pattern.Pattern)
			if err != nil {
				return nil, fmt.Errorf("compile pattern %q: %w", pattern.Name, err)
			}
		}
		compileList := func(items []string) ([]*regexp.Regexp, error) {
			out := make([]*regexp.Regexp, 0, len(items))
			for _, item := range items {
				x, e := regexp.Compile(item)
				if e != nil {
					return nil, e
				}
				out = append(out, x)
			}
			return out, nil
		}
		all, err := compileList(pattern.AllPatterns)
		if err != nil {
			return nil, fmt.Errorf("compile all pattern %q: %w", pattern.Name, err)
		}
		any, err := compileList(pattern.AnyPatterns)
		if err != nil {
			return nil, fmt.Errorf("compile any pattern %q: %w", pattern.Name, err)
		}
		exclude, err := compileList(pattern.ExcludePatterns)
		if err != nil {
			return nil, fmt.Errorf("compile exclude pattern %q: %w", pattern.Name, err)
		}
		patterns = append(patterns, compiledPattern{
			cfg:      pattern,
			re:       re,
			requires: patternRequires(pattern.Pattern),
			all:      all, any: any, exclude: exclude,
		})
	}
	sensitiveWords := parseSensitiveWords(cfg.SensitiveWords)

	return &Engine{
		cfg:            cfg,
		patterns:       patterns,
		sensitiveWords: sensitiveWords,
		literalIndex:   buildLiteralIndex(patterns, sensitiveWords),
	}, nil
}

func BuiltinPatternConfigs() []PatternConfig {
	out := make([]PatternConfig, len(defaultPatternConfigs))
	copy(out, defaultPatternConfigs)
	return out
}

func patternShouldRun(text string, pattern compiledPattern, literalHits map[string]bool) bool {
	for _, required := range pattern.requires {
		if !literalMatched(text, literalHits, required) {
			return false
		}
	}
	return true
}

func literalMatched(text string, literalHits map[string]bool, literal string) bool {
	if literal == "" {
		return true
	}
	if literalHits != nil {
		return literalHits[literal]
	}
	return strings.Contains(text, literal)
}

func (idx *literalIndex) match(text string) map[string]bool {
	if idx == nil || len(idx.literals) == 0 || text == "" {
		return nil
	}
	hits := make(map[string]bool, len(idx.literals))
	for _, literal := range idx.literals {
		if strings.Contains(text, literal.text) {
			hits[literal.text] = true
		}
	}
	return hits
}

func buildLiteralIndex(patterns []compiledPattern, sensitiveWords []string) *literalIndex {
	index := &literalIndex{}
	seen := map[string]int{}
	add := func(text string) int {
		text = strings.TrimSpace(text)
		if text == "" {
			return -1
		}
		if existing, ok := seen[text]; ok {
			return existing
		}
		id := len(index.literals)
		seen[text] = id
		index.literals = append(index.literals, literalNeedle{text: text})
		return id
	}
	for _, pattern := range patterns {
		for _, literal := range pattern.requires {
			add(literal)
		}
	}
	for _, word := range sensitiveWords {
		add(word)
	}
	return index
}

func patternRequires(pattern string) []string {
	parsed, err := syntax.Parse(pattern, syntax.Perl)
	if err != nil {
		return nil
	}
	return regexpRequiredLiterals(parsed.Simplify())
}

func regexpRequiredLiterals(re *syntax.Regexp) []string {
	literals := requiredLiteralSet(re)
	return sortedLiteralSet(literals, 4)
}

func requiredLiteralSet(re *syntax.Regexp) map[string]struct{} {
	if re == nil {
		return nil
	}
	switch re.Op {
	case syntax.OpLiteral:
		return literalSetFromRunes(re.Rune, 4)
	case syntax.OpCapture, syntax.OpPlus:
		return requiredLiteralSet(re.Sub[0])
	case syntax.OpConcat:
		out := map[string]struct{}{}
		for _, sub := range re.Sub {
			for literal := range requiredLiteralSet(sub) {
				out[literal] = struct{}{}
			}
		}
		return out
	case syntax.OpAlternate:
		var common map[string]struct{}
		for _, sub := range re.Sub {
			literals := requiredLiteralSet(sub)
			if common == nil {
				common = literals
				continue
			}
			for literal := range common {
				if _, ok := literals[literal]; !ok {
					delete(common, literal)
				}
			}
		}
		return common
	}
	return nil
}

func literalSetFromRunes(runes []rune, minRunes int) map[string]struct{} {
	literal := normalizeForScan(string(runes))
	if utf8.RuneCountInString(literal) < minRunes {
		return nil
	}
	return map[string]struct{}{literal: {}}
}

func sortedLiteralSet(literals map[string]struct{}, minRunes int) []string {
	if len(literals) == 0 {
		return nil
	}
	out := make([]string, 0, len(literals))
	seen := map[string]struct{}{}
	for literal := range literals {
		literal = strings.TrimSpace(literal)
		if utf8.RuneCountInString(literal) < minRunes {
			continue
		}
		if _, ok := seen[literal]; ok {
			continue
		}
		seen[literal] = struct{}{}
		out = append(out, literal)
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i]) == len(out[j]) {
			return out[i] < out[j]
		}
		return len(out[i]) > len(out[j])
	})
	return out
}

func Inspect(body []byte, endpoint string, cfg Config) Verdict {
	text := ExtractText(body, endpoint, NormalizeConfig(cfg).MaxTextLength)
	return InspectText(text, cfg)
}

func InspectText(text string, cfg Config) Verdict {
	cfg = NormalizeConfig(cfg)
	preview := Preview(text, 500)
	verdict := Verdict{
		Enabled:        cfg.Enabled,
		Mode:           cfg.Mode,
		Action:         ActionAllow,
		Threshold:      cfg.Threshold,
		TextPreview:    preview,
		ExtractedChars: utf8.RuneCountInString(text),
	}
	if !cfg.Enabled || strings.TrimSpace(text) == "" {
		return verdict
	}

	engine, err := engineForConfig(cfg)
	if err != nil {
		verdict.Reason = err.Error()
		return verdict
	}
	return engine.InspectText(text)
}

func (e *Engine) InspectText(text string) Verdict {
	cfg := e.cfg
	preview := Preview(text, 500)
	verdict := Verdict{
		Enabled:        cfg.Enabled,
		Mode:           cfg.Mode,
		Action:         ActionAllow,
		Threshold:      cfg.Threshold,
		TextPreview:    preview,
		FullText:       text,
		ExtractedChars: utf8.RuneCountInString(text),
	}
	if !cfg.Enabled || strings.TrimSpace(text) == "" {
		return verdict
	}

	limitedText := limitScanText(text, cfg.MaxTextLength)
	scanText := strings.Join(scanViews(limitedText, cfg.Advanced.Normalization), "\n")
	if utf8.RuneCountInString(scanText) < 3 {
		return verdict
	}

	matchContexts := make([]string, 0, 3)
	recordContext := func(context string) {
		context = strings.TrimSpace(context)
		if context == "" || len(matchContexts) >= 3 {
			return
		}
		for _, existing := range matchContexts {
			if existing == context {
				return
			}
		}
		matchContexts = append(matchContexts, context)
	}
	matchesByName := map[string]Match{}
	rawScore := 0
	strictScore := 0
	strictMatched := false
	terminalCategoryMatched := false
	terminalCategories := make(map[string]bool, len(cfg.Advanced.Enforcement.TerminalCategories))
	for _, category := range cfg.Advanced.Enforcement.TerminalCategories {
		terminalCategories[strings.ToLower(category)] = true
	}
	literalHits := e.literalIndex.match(scanText)
	for _, word := range e.sensitiveWords {
		if word == "" {
			continue
		}
		if literalMatched(scanText, literalHits, word) {
			match := Match{Name: "sensitive_word", Weight: 100, Category: "sensitive_word", Strict: true}
			_, context := matchContextFromLiteral(scanText, word)
			recordContext(context)
			matchesByName[match.Name+":"+word] = match
		}
	}
	for _, pattern := range e.patterns {
		if !patternShouldRun(scanText, pattern, literalHits) {
			continue
		}
		if loc := compiledPatternMatchIndex(scanText, pattern); loc != nil {
			match := Match{
				Name:     pattern.cfg.Name,
				Weight:   pattern.cfg.Weight,
				Category: pattern.cfg.Category,
				Strict:   pattern.cfg.Strict,
			}
			_, context := regexMatchContext(scanText, loc)
			recordContext(context)
			matchesByName[match.Name] = match
		}
	}

	matches := make([]Match, 0, len(matchesByName))
	for _, match := range matchesByName {
		matches = append(matches, match)
		rawScore += match.Weight
		if match.Strict {
			strictScore += match.Weight
			strictMatched = true
		}
		if terminalCategories[strings.ToLower(strings.TrimSpace(match.Category))] {
			terminalCategoryMatched = true
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].Weight == matches[j].Weight {
			return matches[i].Name < matches[j].Name
		}
		return matches[i].Weight > matches[j].Weight
	})

	score := rawScore
	contextDiscount := 0
	terminalCategoryHit := terminalCategoryMatched
	terminalStrictHit := (cfg.StrictTerminalEnabled && strictMatched) || terminalCategoryHit
	if rawScore > 0 && !terminalStrictHit {
		contextDiscount = defensiveContextDiscount(scanText)
		score -= contextDiscount
		if score < 0 {
			score = 0
		}
	}
	strictHit := terminalStrictHit || strictScore >= cfg.StrictThreshold
	action := ActionAllow
	if terminalStrictHit {
		action = ActionBlock
	} else if score >= cfg.Threshold || strictHit {
		switch cfg.Mode {
		case ModeBlock:
			action = ActionBlock
		case ModeWarn:
			action = ActionWarn
		default:
			action = ActionAllow
		}
	}

	verdict.Action = action
	verdict.Score = score
	verdict.RawScore = rawScore
	verdict.StrictHit = strictHit
	verdict.TerminalStrictHit = terminalStrictHit
	verdict.TerminalCategoryHit = terminalCategoryHit
	verdict.Matched = matches
	if len(matches) > 0 {
		verdict.Reason = reasonForVerdict(action, score, cfg.Threshold, matches)
	}
	if len(matchContexts) > 0 {
		verdict.TextPreview = strings.Join(matchContexts, "\n---\n")
	}
	return verdict
}

func compiledPatternMatchIndex(text string, pattern compiledPattern) []int {
	for _, re := range pattern.exclude {
		if re.MatchString(text) {
			return nil
		}
	}
	first := []int(nil)
	if pattern.re != nil {
		first = pattern.re.FindStringIndex(text)
		if first == nil {
			return nil
		}
	}
	for _, re := range pattern.all {
		loc := re.FindStringIndex(text)
		if loc == nil {
			return nil
		}
		if first == nil || loc[0] < first[0] {
			first = loc
		}
	}
	matchedAny := 0
	for _, re := range pattern.any {
		loc := re.FindStringIndex(text)
		if loc != nil {
			matchedAny++
			if first == nil || loc[0] < first[0] {
				first = loc
			}
		}
	}
	minimum := pattern.cfg.MinMatches
	if minimum <= 0 && len(pattern.any) > 0 {
		minimum = 1
	}
	if matchedAny < minimum {
		return nil
	}
	return first
}

func ExtractText(body []byte, endpoint string, maxLen int) string {
	if len(body) == 0 || !gjson.ValidBytes(body) {
		return ""
	}
	var parts []string
	endpoint = strings.ToLower(strings.TrimSpace(endpoint))

	addResultText := func(result gjson.Result) {
		if result.Exists() {
			collectGJSONText(result, &parts)
		}
	}

	switch endpoint {
	case "chat", "chat_completions", "/v1/chat/completions":
		addResultText(gjson.GetBytes(body, "messages"))
	case "messages", "anthropic", "/v1/messages":
		addResultText(gjson.GetBytes(body, "system"))
		addResultText(gjson.GetBytes(body, "messages"))
	case "image", "images", "images_generations", "images_edits", "/v1/images/generations", "/v1/images/edits":
		addResultText(gjson.GetBytes(body, "prompt"))
		addResultText(gjson.GetBytes(body, "style"))
	default:
		addResultText(gjson.GetBytes(body, "instructions"))
		addResultText(gjson.GetBytes(body, "input"))
		addResultText(gjson.GetBytes(body, "prompt"))
		addResultText(gjson.GetBytes(body, "messages"))
	}
	return limitScanText(strings.Join(parts, "\n"), maxLen)
}

func Preview(text string, maxRunes int) string {
	text = strings.Join(strings.Fields(strings.TrimSpace(text)), " ")
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(text)
	if len(runes) <= maxRunes {
		return text
	}
	return string(runes[:maxRunes]) + "..."
}

func RedactedPreview(text string, maxRunes int) string {
	return Preview(RedactSensitive(text), maxRunes)
}

func RedactSensitive(text string) string {
	if text == "" {
		return ""
	}
	redacted := text
	for _, pattern := range sensitiveRedactionPatterns {
		redacted = pattern.re.ReplaceAllString(redacted, pattern.replacement)
	}
	return redacted
}

func matchContextFromLiteral(text string, literal string) (string, string) {
	start := strings.Index(text, literal)
	if start < 0 {
		return "", ""
	}
	return matchContext(text, start, start+len(literal))
}

func regexMatchContext(text string, loc []int) (string, string) {
	if len(loc) != 2 {
		return "", ""
	}
	return matchContext(text, loc[0], loc[1])
}

func matchContext(text string, start int, end int) (string, string) {
	if start < 0 || end < start || start > len(text) {
		return "", ""
	}
	if end > len(text) {
		end = len(text)
	}
	sample := RedactedPreview(text[start:end], 120)
	contextStart := byteOffsetBefore(text, start, 80)
	contextEnd := byteOffsetAfter(text, end, 80)
	rawContext := strings.TrimSpace(text[contextStart:contextEnd])
	redactedContext := RedactSensitive(rawContext)
	if redactedContext != rawContext {
		if contextStart > 0 {
			redactedContext = "..." + redactedContext
		}
		return sample, redactedContext
	}

	before := strings.TrimSpace(text[contextStart:start])
	hit := text[start:end]
	after := strings.TrimSpace(text[end:contextEnd])
	parts := make([]string, 0, 3)
	if before != "" {
		parts = append(parts, before)
	}
	parts = append(parts, HitStartMarker+hit+HitEndMarker)
	if after != "" {
		parts = append(parts, after)
	}
	context := strings.Join(parts, " ")
	if contextStart > 0 {
		context = "..." + context
	}
	return sample, context
}

func byteOffsetBefore(text string, start int, maxRunes int) int {
	if start <= 0 || maxRunes <= 0 {
		return start
	}
	offsets := make([]int, 0, maxRunes+1)
	for idx := range text[:start] {
		offsets = append(offsets, idx)
	}
	if len(offsets) <= maxRunes {
		return 0
	}
	return offsets[len(offsets)-maxRunes]
}

func byteOffsetAfter(text string, end int, maxRunes int) int {
	if end >= len(text) || maxRunes <= 0 {
		return end
	}
	count := 0
	for idx := range text[end:] {
		if count >= maxRunes {
			return end + idx
		}
		count++
	}
	return len(text)
}

func MatchesJSON(matches []Match) string {
	if len(matches) == 0 {
		return "[]"
	}
	data, err := json.Marshal(matches)
	if err != nil {
		return "[]"
	}
	return string(data)
}

func collectGJSONText(result gjson.Result, parts *[]string) {
	if !result.Exists() || result.Type == gjson.Null {
		return
	}
	switch {
	case result.IsArray():
		for _, item := range result.Array() {
			collectGJSONText(item, parts)
		}
	case result.IsObject():
		if textValue := result.Get("text"); textValue.Type == gjson.String {
			if t := strings.TrimSpace(textValue.String()); t != "" {
				*parts = append(*parts, t)
			}
		}
		if contentValue := result.Get("content"); contentValue.Exists() {
			if contentValue.Type == gjson.String {
				if t := strings.TrimSpace(contentValue.String()); t != "" {
					*parts = append(*parts, t)
				}
			} else {
				collectGJSONText(contentValue, parts)
			}
		}
		result.ForEach(func(key, value gjson.Result) bool {
			switch strings.ToLower(key.String()) {
			case "text", "content", "image_url", "url", "file_id", "result", "data", "b64_json", "source", "file", "type", "role":
				return true
			}
			collectGJSONText(value, parts)
			return true
		})
	case result.Type == gjson.String:
		if t := strings.TrimSpace(result.String()); t != "" {
			*parts = append(*parts, t)
		}
	}
}

func parseSensitiveWords(raw string) []string {
	lines := strings.Split(raw, "\n")
	seen := map[string]struct{}{}
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		line = normalizeForScan(strings.TrimSpace(line))
		if line == "" {
			continue
		}
		if _, ok := seen[line]; ok {
			continue
		}
		seen[line] = struct{}{}
		out = append(out, line)
	}
	return out
}

func normalizePatternNames(names []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		key := strings.ToLower(name)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, name)
	}
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i]) < strings.ToLower(out[j])
	})
	return out
}

func disabledPatternSet(names []string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			out[strings.ToLower(name)] = true
		}
	}
	return out
}

func normalizeForScan(text string) string {
	text = strings.ReplaceAll(text, "```", " ")
	text = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' {
			return ' '
		}
		return unicode.ToLower(r)
	}, text)
	return strings.Join(strings.Fields(text), " ")
}

func limitScanText(text string, maxLen int) string {
	if maxLen <= 0 {
		maxLen = DefaultMaxTextLength
	}
	if len(text) <= maxLen {
		return text
	}
	head := defaultHeadScanLength
	tail := defaultTailScanLength
	if maxLen < head+tail {
		head = maxLen * 4 / 5
		tail = maxLen - head
	}
	if head > len(text) {
		head = len(text)
	}
	if tail > len(text)-head {
		tail = len(text) - head
	}
	return safeUTF8Prefix(text, head) + "\n" + safeUTF8Suffix(text, tail)
}

func safeUTF8Prefix(text string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if maxBytes >= len(text) {
		return text
	}
	for maxBytes > 0 && !utf8.ValidString(text[:maxBytes]) {
		maxBytes--
	}
	return text[:maxBytes]
}

func safeUTF8Suffix(text string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if maxBytes >= len(text) {
		return text
	}
	start := len(text) - maxBytes
	for start < len(text) && !utf8.RuneStart(text[start]) {
		start++
	}
	return text[start:]
}

func defensiveContextDiscount(text string) int {
	discount := 0
	for _, pattern := range defensiveContextPatterns {
		if pattern.MatchString(text) {
			discount += 30
		}
	}
	if discount > 90 {
		return 90
	}
	return discount
}

func reasonForVerdict(action string, score int, threshold int, matches []Match) string {
	if len(matches) == 0 {
		return ""
	}
	names := make([]string, 0, len(matches))
	for i, match := range matches {
		if i >= 3 {
			break
		}
		names = append(names, match.Name)
	}
	if action == ActionBlock {
		return fmt.Sprintf("prompt blocked: score %d >= %d (%s)", score, threshold, strings.Join(names, ", "))
	}
	if action == ActionWarn {
		return fmt.Sprintf("prompt warning: score %d >= %d (%s)", score, threshold, strings.Join(names, ", "))
	}
	return fmt.Sprintf("prompt matched: score %d (%s)", score, strings.Join(names, ", "))
}
