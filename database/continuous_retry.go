package database

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// ContinuousRetryPolicy controls the opt-in, request-scoped retry loop for
// upstream failures. The database package owns the wire representation so the
// auth, proxy, admin, and runtime packages can share one normalized policy
// without introducing an import cycle.
type ContinuousRetryPolicy struct {
	Enabled            bool     `json:"enabled"`
	CatchAll           bool     `json:"catch_all"`
	Categories         []string `json:"categories"`
	StatusCodes        []int    `json:"status_codes"`
	ErrorCodes         []string `json:"error_codes"`
	MaxDurationSeconds int      `json:"max_duration_seconds"`
}

// ContinuousRetryPolicyUpdate identifies the policy fields edited by one
// admin request. Nil fields retain the latest value already in the database.
type ContinuousRetryPolicyUpdate struct {
	Enabled            *bool
	CatchAll           *bool
	Categories         *[]string
	StatusCodes        *[]int
	ErrorCodes         *[]string
	MaxDurationSeconds *int
}

const (
	DefaultContinuousRetryMaxDurationSeconds = 600
	MinContinuousRetryMaxDurationSeconds     = 1
	MaxContinuousRetryMaxDurationSeconds     = 900

	ContinuousRetryCategoryTransport      = "transport"
	ContinuousRetryCategoryHTTP429        = "http_429"
	ContinuousRetryCategoryHTTP4xx        = "http_4xx"
	ContinuousRetryCategoryHTTP5xx        = "http_5xx"
	ContinuousRetryCategoryStreamError    = "stream_error"
	ContinuousRetryCategoryResponseFailed = "response_failed"
	ContinuousRetryCategoryContextError   = "context_error"
)

var continuousRetryCategories = map[string]struct{}{
	ContinuousRetryCategoryTransport:      {},
	ContinuousRetryCategoryHTTP429:        {},
	ContinuousRetryCategoryHTTP4xx:        {},
	ContinuousRetryCategoryHTTP5xx:        {},
	ContinuousRetryCategoryStreamError:    {},
	ContinuousRetryCategoryResponseFailed: {},
	ContinuousRetryCategoryContextError:   {},
}

// DefaultContinuousRetryPolicy is deliberately disabled. When an operator
// enables it, the preselected categories cover the usual transient failures;
// deterministic 4xx/context errors require an explicit category or status/code
// selection in the admin UI.
func DefaultContinuousRetryPolicy() ContinuousRetryPolicy {
	return ContinuousRetryPolicy{
		Enabled:  false,
		CatchAll: false,
		Categories: []string{
			ContinuousRetryCategoryTransport,
			ContinuousRetryCategoryHTTP429,
			ContinuousRetryCategoryHTTP5xx,
			ContinuousRetryCategoryStreamError,
		},
		StatusCodes:        []int{},
		ErrorCodes:         []string{},
		MaxDurationSeconds: DefaultContinuousRetryMaxDurationSeconds,
	}
}

// NormalizeContinuousRetryPolicy removes unknown/duplicate values and clamps
// exact status selectors to valid HTTP status codes. Empty values are retained
// as empty slices so JSON responses stay stable and editable in the UI.
func NormalizeContinuousRetryPolicy(policy ContinuousRetryPolicy) ContinuousRetryPolicy {
	if policy.MaxDurationSeconds == 0 {
		policy.MaxDurationSeconds = DefaultContinuousRetryMaxDurationSeconds
	} else if policy.MaxDurationSeconds < MinContinuousRetryMaxDurationSeconds {
		policy.MaxDurationSeconds = MinContinuousRetryMaxDurationSeconds
	} else if policy.MaxDurationSeconds > MaxContinuousRetryMaxDurationSeconds {
		policy.MaxDurationSeconds = MaxContinuousRetryMaxDurationSeconds
	}
	if !policy.Enabled {
		policy.CatchAll = false
	}
	categorySet := make(map[string]struct{}, len(policy.Categories))
	for _, raw := range policy.Categories {
		category := strings.ToLower(strings.TrimSpace(raw))
		switch category {
		case "rate_limit", "rate_limited", "429":
			category = ContinuousRetryCategoryHTTP429
		case "4xx":
			category = ContinuousRetryCategoryHTTP4xx
		case "5xx", "server", "server_error":
			category = ContinuousRetryCategoryHTTP5xx
		case "stream", "stream_failure":
			category = ContinuousRetryCategoryStreamError
		case "failed", "response.failed":
			category = ContinuousRetryCategoryResponseFailed
		case "context", "context_length":
			category = ContinuousRetryCategoryContextError
		}
		if _, ok := continuousRetryCategories[category]; ok {
			categorySet[category] = struct{}{}
		}
	}
	policy.Categories = make([]string, 0, len(categorySet))
	for category := range categorySet {
		policy.Categories = append(policy.Categories, category)
	}
	sort.Strings(policy.Categories)

	statusSet := make(map[int]struct{}, len(policy.StatusCodes))
	for _, status := range policy.StatusCodes {
		if status >= 100 && status <= 599 {
			statusSet[status] = struct{}{}
		}
	}
	policy.StatusCodes = make([]int, 0, len(statusSet))
	for status := range statusSet {
		policy.StatusCodes = append(policy.StatusCodes, status)
	}
	sort.Ints(policy.StatusCodes)

	errorSet := make(map[string]struct{}, len(policy.ErrorCodes))
	for _, raw := range policy.ErrorCodes {
		code := strings.ToLower(strings.TrimSpace(raw))
		if code == "" || len(code) > 128 {
			continue
		}
		valid := true
		for _, r := range code {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' && r != '-' && r != '.' {
				valid = false
				break
			}
		}
		if valid {
			errorSet[code] = struct{}{}
		}
	}
	policy.ErrorCodes = make([]string, 0, len(errorSet))
	for code := range errorSet {
		policy.ErrorCodes = append(policy.ErrorCodes, code)
	}
	sort.Strings(policy.ErrorCodes)
	return policy
}

func ParseContinuousRetryPolicy(raw string) ContinuousRetryPolicy {
	var policy ContinuousRetryPolicy
	if strings.TrimSpace(raw) != "" {
		_ = json.Unmarshal([]byte(raw), &policy)
	}
	return NormalizeContinuousRetryPolicy(policy)
}

func EncodeContinuousRetryPolicy(policy ContinuousRetryPolicy) string {
	normalized := NormalizeContinuousRetryPolicy(policy)
	raw, err := json.Marshal(normalized)
	if err != nil {
		return `{"enabled":false,"catch_all":false,"categories":[],"status_codes":[],"error_codes":[],"max_duration_seconds":600}`
	}
	return string(raw)
}

// CatchesAllUpstreamFailures reports whether the explicit catch-all override
// is active. Enabled remains the master gate.
func (p ContinuousRetryPolicy) CatchesAllUpstreamFailures() bool {
	return p.Enabled && p.CatchAll
}

func (p ContinuousRetryPolicy) HasCategory(category string) bool {
	if !p.Enabled {
		return false
	}
	category = strings.ToLower(strings.TrimSpace(category))
	for _, selected := range p.Categories {
		if strings.EqualFold(selected, category) {
			return true
		}
	}
	return false
}

func (p ContinuousRetryPolicy) HasStatusCode(status int) bool {
	if !p.Enabled {
		return false
	}
	for _, selected := range p.StatusCodes {
		if selected == status {
			return true
		}
	}
	return false
}

// MatchesHTTP reports whether a status/body pair is selected by the policy.
// Error-code selectors use token-boundary matching: upstream relays place
// codes at different JSON paths and sometimes expose only a plain-text error
// body, while partial identifiers must not match accidentally.
func (p ContinuousRetryPolicy) MatchesHTTP(status int, body []byte) bool {
	if !p.Enabled {
		return false
	}
	// The proxied inference protocols in this service have one successful
	// upstream status: 200. Catch-all therefore keeps retrying every other
	// status, including unusual 2xx responses that the handlers cannot decode
	// as a completed model result.
	if p.CatchAll && status > 0 && status != 200 {
		return true
	}
	if p.HasStatusCode(status) || p.MatchesErrorCodes(body) {
		return true
	}
	switch {
	case status == 429:
		return p.HasCategory(ContinuousRetryCategoryHTTP429) || p.HasCategory(ContinuousRetryCategoryHTTP4xx)
	case status >= 400 && status <= 499:
		return p.HasCategory(ContinuousRetryCategoryHTTP4xx)
	case status >= 500 && status <= 599:
		return p.HasCategory(ContinuousRetryCategoryHTTP5xx)
	default:
		return false
	}
}

func (p ContinuousRetryPolicy) MatchesErrorCodes(body []byte) bool {
	if !p.Enabled || len(body) == 0 || len(p.ErrorCodes) == 0 {
		return false
	}
	value := strings.ToLower(string(body))
	for _, code := range p.ErrorCodes {
		if containsContinuousRetryCode(value, strings.ToLower(code)) {
			return true
		}
	}
	return false
}

// containsContinuousRetryCode matches a complete machine-readable token. A
// plain substring would make selecting `rate_limited` also match
// `not_rate_limited` or `rate_limited_model`; callers can select those codes
// explicitly when they need the broader marker.
func containsContinuousRetryCode(value, code string) bool {
	code = strings.TrimSpace(strings.ToLower(code))
	if value == "" || code == "" {
		return false
	}
	for offset := 0; offset <= len(value)-len(code); {
		relative := strings.Index(value[offset:], code)
		if relative < 0 {
			return false
		}
		start := offset + relative
		end := start + len(code)
		leftOK := start == 0 || !isContinuousRetryCodeChar(value[start-1])
		rightOK := end == len(value) || !isContinuousRetryCodeChar(value[end])
		if leftOK && rightOK {
			return true
		}
		offset = start + 1
	}
	return false
}

func isContinuousRetryCodeChar(value byte) bool {
	return (value >= 'a' && value <= 'z') ||
		(value >= '0' && value <= '9') ||
		value == '_' || value == '-' || value == '.'
}

func (p ContinuousRetryPolicy) MatchesTransport(errText string) bool {
	if !p.Enabled {
		return false
	}
	if p.CatchAll {
		return true
	}
	if p.HasCategory(ContinuousRetryCategoryTransport) {
		return true
	}
	return p.MatchesErrorCodes([]byte(strings.ToLower(errText)))
}

// ContinuousRetryStatusCodeList formats the configured exact status codes for
// logs and backwards-compatible text settings without exposing any secrets.
func (p ContinuousRetryPolicy) ContinuousRetryStatusCodeList() string {
	parts := make([]string, 0, len(p.StatusCodes))
	for _, status := range p.StatusCodes {
		parts = append(parts, strconv.Itoa(status))
	}
	return strings.Join(parts, ",")
}
