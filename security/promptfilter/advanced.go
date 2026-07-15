package promptfilter

import (
	"encoding/base64"
	"encoding/json"
	"html"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

type AdvancedConfig struct {
	Normalization NormalizationConfig `json:"normalization"`
	Enforcement   EnforcementConfig   `json:"enforcement"`
	Risk          RiskConfig          `json:"risk"`
	Sidecar       SidecarConfig       `json:"sidecar"`
	Output        OutputConfig        `json:"output"`
	Intelligence  IntelligenceConfig  `json:"intelligence"`
	NewAPI        NewAPIConfig        `json:"newapi"`
}

// NewAPIConfig controls signed identity propagation and repeat-offender directives.
type NewAPIConfig struct {
	Enabled              bool   `json:"enabled"`
	MaxClockSkewSeconds  int    `json:"max_clock_skew_seconds"`
	OffenseWindowSeconds int    `json:"offense_window_seconds"`
	BanAfter             int    `json:"ban_after"`
	Secret               string `json:"-"`
}

type EnforcementConfig struct {
	TerminalCategories []string `json:"terminal_categories"`
}

type NormalizationConfig struct {
	Enabled       bool `json:"enabled"`
	DecodeURL     bool `json:"decode_url"`
	DecodeHTML    bool `json:"decode_html"`
	DecodeBase64  bool `json:"decode_base64"`
	MaxDecodeRuns int  `json:"max_decode_runs"`
}

type RiskConfig struct {
	Enabled              bool `json:"enabled"`
	WindowSeconds        int  `json:"window_seconds"`
	BlockThreshold       int  `json:"block_threshold"`
	ReviewThreshold      int  `json:"review_threshold"`
	UserWeightPercent    int  `json:"user_weight_percent"`
	IPWeightPercent      int  `json:"ip_weight_percent"`
	SessionWeightPercent int  `json:"session_weight_percent"`
}

type SidecarConfig struct {
	Enabled        bool   `json:"enabled"`
	BaseURL        string `json:"base_url"`
	TimeoutSeconds int    `json:"timeout_seconds"`
	FailClosed     bool   `json:"fail_closed"`
	MinScore       int    `json:"min_score"`
}

type OutputConfig struct {
	Enabled      bool `json:"enabled"`
	BufferBytes  int  `json:"buffer_bytes"`
	OverlapBytes int  `json:"overlap_bytes"`
	StrictOnly   bool `json:"strict_only"`
}

// IntelligenceConfig controls the optional public-source rule intelligence job.
// It is disabled by default and never auto-adds rules unless AutoAdd is explicitly enabled.
type IntelligenceConfig struct {
	Enabled          bool     `json:"enabled"`
	IntervalHours    int      `json:"interval_hours"`
	Queries          []string `json:"queries"`
	MaxSearchResults int      `json:"max_search_results"`
	ModelEnabled     bool     `json:"model_enabled"`
	Model            string   `json:"model"`
	MaxModelCalls    int      `json:"max_model_calls"`
	AutoAdd          bool     `json:"auto_add"`
}

func DefaultAdvancedConfig() AdvancedConfig {
	return AdvancedConfig{
		Normalization: NormalizationConfig{MaxDecodeRuns: 1},
		Risk:          RiskConfig{WindowSeconds: 600, BlockThreshold: 100, ReviewThreshold: 60, UserWeightPercent: 50, IPWeightPercent: 30, SessionWeightPercent: 20},
		Sidecar:       SidecarConfig{TimeoutSeconds: 3, FailClosed: true, MinScore: 30},
		Output:        OutputConfig{BufferBytes: 4096, OverlapBytes: 512, StrictOnly: true},
		Intelligence:  IntelligenceConfig{IntervalHours: 24, MaxSearchResults: 20, Model: "gpt-5.4", MaxModelCalls: 1},
		NewAPI:        NewAPIConfig{MaxClockSkewSeconds: 120, OffenseWindowSeconds: 86400, BanAfter: 2},
	}
}

func ParseAdvancedConfig(raw string) (AdvancedConfig, error) {
	cfg := DefaultAdvancedConfig()
	if strings.TrimSpace(raw) == "" {
		return cfg, nil
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return AdvancedConfig{}, err
	}
	return NormalizeAdvancedConfig(cfg), nil
}

func MarshalAdvancedConfig(cfg AdvancedConfig) string {
	b, err := json.Marshal(NormalizeAdvancedConfig(cfg))
	if err != nil {
		return "{}"
	}
	return string(b)
}

func NormalizeAdvancedConfig(cfg AdvancedConfig) AdvancedConfig {
	d := DefaultAdvancedConfig()
	seenCategories := map[string]bool{}
	categories := make([]string, 0, len(cfg.Enforcement.TerminalCategories))
	for _, category := range cfg.Enforcement.TerminalCategories {
		category = strings.ToLower(strings.TrimSpace(category))
		if category != "" && !seenCategories[category] {
			seenCategories[category] = true
			categories = append(categories, category)
		}
	}
	cfg.Enforcement.TerminalCategories = categories
	if cfg.Normalization.MaxDecodeRuns <= 0 {
		cfg.Normalization.MaxDecodeRuns = d.Normalization.MaxDecodeRuns
	}
	if cfg.Normalization.MaxDecodeRuns > 2 {
		cfg.Normalization.MaxDecodeRuns = 2
	}
	if cfg.Risk.WindowSeconds <= 0 {
		cfg.Risk.WindowSeconds = d.Risk.WindowSeconds
	}
	if cfg.Risk.WindowSeconds > 86400 {
		cfg.Risk.WindowSeconds = 86400
	}
	if cfg.Risk.BlockThreshold <= 0 {
		cfg.Risk.BlockThreshold = d.Risk.BlockThreshold
	}
	if cfg.Risk.ReviewThreshold <= 0 {
		cfg.Risk.ReviewThreshold = d.Risk.ReviewThreshold
	}
	if cfg.Sidecar.TimeoutSeconds <= 0 {
		cfg.Sidecar.TimeoutSeconds = d.Sidecar.TimeoutSeconds
	}
	if cfg.Sidecar.TimeoutSeconds > 30 {
		cfg.Sidecar.TimeoutSeconds = 30
	}
	if cfg.Output.BufferBytes < 512 {
		cfg.Output.BufferBytes = d.Output.BufferBytes
	}
	if cfg.Output.BufferBytes > 65536 {
		cfg.Output.BufferBytes = 65536
	}
	if cfg.Output.OverlapBytes < 64 {
		cfg.Output.OverlapBytes = d.Output.OverlapBytes
	}
	if cfg.Output.OverlapBytes >= cfg.Output.BufferBytes {
		cfg.Output.OverlapBytes = cfg.Output.BufferBytes / 4
	}
	if cfg.Intelligence.IntervalHours < 1 {
		cfg.Intelligence.IntervalHours = d.Intelligence.IntervalHours
	}
	if cfg.Intelligence.IntervalHours > 720 {
		cfg.Intelligence.IntervalHours = 720
	}
	if cfg.Intelligence.MaxSearchResults < 1 {
		cfg.Intelligence.MaxSearchResults = d.Intelligence.MaxSearchResults
	}
	if cfg.Intelligence.MaxSearchResults > 100 {
		cfg.Intelligence.MaxSearchResults = 100
	}
	if strings.TrimSpace(cfg.Intelligence.Model) == "" {
		cfg.Intelligence.Model = d.Intelligence.Model
	}
	if cfg.Intelligence.MaxModelCalls < 0 {
		cfg.Intelligence.MaxModelCalls = 0
	}
	if cfg.Intelligence.MaxModelCalls > 3 {
		cfg.Intelligence.MaxModelCalls = 3
	}
	if cfg.NewAPI.MaxClockSkewSeconds < 30 {
		cfg.NewAPI.MaxClockSkewSeconds = d.NewAPI.MaxClockSkewSeconds
	}
	if cfg.NewAPI.MaxClockSkewSeconds > 600 {
		cfg.NewAPI.MaxClockSkewSeconds = 600
	}
	if cfg.NewAPI.OffenseWindowSeconds < 60 {
		cfg.NewAPI.OffenseWindowSeconds = d.NewAPI.OffenseWindowSeconds
	}
	if cfg.NewAPI.OffenseWindowSeconds > 2592000 {
		cfg.NewAPI.OffenseWindowSeconds = 2592000
	}
	if cfg.NewAPI.BanAfter < 2 {
		cfg.NewAPI.BanAfter = d.NewAPI.BanAfter
	}
	if cfg.NewAPI.BanAfter > 10 {
		cfg.NewAPI.BanAfter = 10
	}
	queries := make([]string, 0, len(cfg.Intelligence.Queries))
	for _, query := range cfg.Intelligence.Queries {
		query = strings.TrimSpace(query)
		if query != "" && len(queries) < 10 {
			queries = append(queries, query)
		}
	}
	cfg.Intelligence.Queries = queries
	return cfg
}

func scanViews(text string, cfg NormalizationConfig) []string {
	base := normalizeForScan(text)
	if !cfg.Enabled {
		return []string{base}
	}
	views := []string{base}
	add := func(value string) {
		value = normalizeForScan(value)
		if value == "" || len(value) > DefaultMaxTextLength*4 {
			return
		}
		for _, existing := range views {
			if existing == value {
				return
			}
		}
		views = append(views, value)
	}
	normalized := norm.NFKC.String(stripInvisible(text))
	add(normalized)
	add(compactForScan(normalized))
	for i := 0; i < cfg.MaxDecodeRuns; i++ {
		if cfg.DecodeURL {
			if v, err := url.QueryUnescape(normalized); err == nil {
				add(v)
				normalized = v
			}
		}
		if cfg.DecodeHTML {
			v := html.UnescapeString(normalized)
			add(v)
			normalized = v
		}
	}
	if cfg.DecodeBase64 {
		for _, field := range strings.Fields(text) {
			field = strings.Trim(field, "\"'`()[]{}<>,.;:")
			if len(field) < 16 || len(field) > 8192 {
				continue
			}
			for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
				decoded, err := enc.DecodeString(field)
				if err == nil && utf8.Valid(decoded) && mostlyPrintable(string(decoded)) {
					add(string(decoded))
					break
				}
			}
		}
	}
	return views
}

func stripInvisible(text string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\u200b', '\u200c', '\u200d', '\u2060', '\ufeff':
			return -1
		}
		if unicode.Is(unicode.Bidi_Control, r) {
			return -1
		}
		if mapped, ok := commonHomoglyphs[r]; ok {
			return mapped
		}
		return r
	}, text)
}

var commonHomoglyphs = map[rune]rune{
	'а': 'a', 'е': 'e', 'о': 'o', 'р': 'p', 'с': 'c', 'х': 'x', 'у': 'y', 'і': 'i', 'ј': 'j',
	'Α': 'a', 'Β': 'b', 'Ε': 'e', 'Ζ': 'z', 'Η': 'h', 'Ι': 'i', 'Κ': 'k', 'Μ': 'm', 'Ν': 'n', 'Ο': 'o', 'Ρ': 'p', 'Τ': 't', 'Χ': 'x',
}

func compactForScan(text string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, text)
}

func mostlyPrintable(text string) bool {
	if text == "" {
		return false
	}
	printable, total := 0, 0
	for _, r := range text {
		total++
		if unicode.IsPrint(r) || unicode.IsSpace(r) {
			printable++
		}
	}
	return total > 0 && printable*100/total >= 85
}
