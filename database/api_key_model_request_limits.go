package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// APIKeyModelRequestLimit is a shared request budget for all matching effective models.
// ResetWeekday is ISO weekday (1=Monday, 7=Sunday). Each rule has a stable identity.
type APIKeyModelRequestLimit struct {
	ID           string `json:"id"`
	Model        string `json:"model"`
	Window       string `json:"window"`
	MaxRequests  int64  `json:"max_requests"`
	Timezone     string `json:"timezone"`
	ResetWeekday int    `json:"reset_weekday"`
	ResetTime    string `json:"reset_time"`
}

type APIKeyModelRequestUsage struct {
	RuleID      string    `json:"rule_id"`
	Model       string    `json:"model"`
	Window      string    `json:"window"`
	Limit       int64     `json:"limit"`
	Used        int64     `json:"used"`
	Remaining   int64     `json:"remaining"`
	WindowStart time.Time `json:"window_start"`
	ResetAt     time.Time `json:"reset_at"`
	Timezone    string    `json:"timezone"`
}

type APIKeyModelRequestExhaustion struct {
	APIKeyModelRequestUsage
}

func (e *APIKeyModelRequestExhaustion) Error() string {
	return fmt.Sprintf("model request budget exhausted for %s (%d/%d)", e.Model, e.Used, e.Limit)
}

var modelRequestRuleIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,80}$`)
var modelRequestPattern = regexp.MustCompile(`^[A-Za-z0-9_./:+*\-]{1,200}$`)

// NormalizeAPIKeyModelRequestLimits generates IDs only for new rules. Persist the
// returned rules and retain their IDs on subsequent edits; counters are separate.
func NormalizeAPIKeyModelRequestLimits(input []APIKeyModelRequestLimit) ([]APIKeyModelRequestLimit, error) {
	if len(input) == 0 {
		return nil, nil
	}
	if len(input) > 64 {
		return nil, errors.New("model_request_limits supports at most 64 rules")
	}
	out := make([]APIKeyModelRequestLimit, len(input))
	seen := make(map[string]bool, len(input))
	for i, rule := range input {
		rule.ID = strings.TrimSpace(rule.ID)
		if rule.ID == "" {
			rule.ID = "mr_" + uuid.NewString()
		}
		if !modelRequestRuleIDPattern.MatchString(rule.ID) || seen[rule.ID] {
			return nil, fmt.Errorf("model_request_limits[%d]: invalid or duplicate rule id", i)
		}
		seen[rule.ID] = true
		rule.Model = strings.TrimSpace(rule.Model)
		if !modelRequestPattern.MatchString(rule.Model) {
			return nil, fmt.Errorf("model_request_limits[%d]: model must be a model name or pattern using only * as wildcard", i)
		}
		if rule.MaxRequests <= 0 {
			return nil, fmt.Errorf("model_request_limits[%d]: max_requests must be positive", i)
		}
		rule.Window = strings.TrimSpace(rule.Window)
		if rule.Window == "" {
			rule.Window = "week"
		}
		if rule.Window != "week" {
			return nil, fmt.Errorf("model_request_limits[%d]: window must be week", i)
		}
		rule.Timezone = strings.TrimSpace(rule.Timezone)
		if rule.Timezone == "" {
			rule.Timezone = "Asia/Shanghai"
		}
		if rule.Timezone == "Local" {
			return nil, fmt.Errorf("model_request_limits[%d]: timezone must identify a fixed IANA location", i)
		}
		if _, err := time.LoadLocation(rule.Timezone); err != nil {
			return nil, fmt.Errorf("model_request_limits[%d]: invalid timezone", i)
		}
		if rule.ResetWeekday == 0 {
			rule.ResetWeekday = 1
		}
		if rule.ResetWeekday < 1 || rule.ResetWeekday > 7 {
			return nil, fmt.Errorf("model_request_limits[%d]: reset_weekday must be 1 through 7", i)
		}
		rule.ResetTime = strings.TrimSpace(rule.ResetTime)
		if rule.ResetTime == "" {
			rule.ResetTime = "00:00"
		}
		if parsed, err := time.Parse("15:04", rule.ResetTime); err != nil || parsed.Format("15:04") != rule.ResetTime {
			return nil, fmt.Errorf("model_request_limits[%d]: reset_time must be HH:MM", i)
		}
		out[i] = rule
	}
	return out, nil
}

// MatchAPIKeyModelRequestLimit matches the whole model name. Only * has special
// meaning, including across separators; all other characters match literally.
func MatchAPIKeyModelRequestLimit(pattern, model string) bool {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	model = strings.ToLower(strings.TrimSpace(model))
	p, m, star, retry := 0, 0, -1, 0
	for m < len(model) {
		if p < len(pattern) && pattern[p] == '*' {
			star, retry = p, m
			p++
		} else if p < len(pattern) && pattern[p] == model[m] {
			p++
			m++
		} else if star >= 0 {
			retry++
			m, p = retry, star+1
		} else {
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}

// APIKeyModelRequestWindow returns the containing calendar week, in UTC. It uses
// calendar dates rather than 168 hours so a DST week can be shorter or longer.
func APIKeyModelRequestWindow(rule APIKeyModelRequestLimit, now time.Time) (time.Time, time.Time, error) {
	normalized, err := NormalizeAPIKeyModelRequestLimits([]APIKeyModelRequestLimit{rule})
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	rule = normalized[0]
	loc, err := time.LoadLocation(rule.Timezone)
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	clock, _ := time.Parse("15:04", rule.ResetTime)
	local := now.In(loc)
	weekday := int(local.Weekday())
	if weekday == 0 {
		weekday = 7
	}
	date := time.Date(local.Year(), local.Month(), local.Day()-(weekday-rule.ResetWeekday+7)%7, 0, 0, 0, 0, time.UTC)
	boundary := func(date time.Time) time.Time {
		return modelRequestLocalBoundary(date, clock.Hour(), clock.Minute(), loc)
	}
	start := boundary(date)
	if now.Before(start) {
		date = date.AddDate(0, 0, -7)
		start = boundary(date)
	}
	return start.UTC(), boundary(date.AddDate(0, 0, 7)).UTC(), nil
}

// On repeated wall times use the first occurrence. If DST skips the configured
// time, move it forward by the gap (e.g. 02:30 becomes 03:30 for a one-hour gap).
func modelRequestLocalBoundary(date time.Time, hour, minute int, loc *time.Location) time.Time {
	wall := time.Date(date.Year(), date.Month(), date.Day(), hour, minute, 0, 0, time.UTC)
	var earliest time.Time
	for _, offsetDate := range []time.Time{wall.Add(-36 * time.Hour), wall, wall.Add(36 * time.Hour)} {
		_, offset := offsetDate.In(loc).Zone()
		candidate := wall.Add(-time.Duration(offset) * time.Second)
		local := candidate.In(loc)
		if local.Year() == date.Year() && local.Month() == date.Month() && local.Day() == date.Day() && local.Hour() == hour && local.Minute() == minute {
			if earliest.IsZero() || candidate.Before(earliest) {
				earliest = candidate
			}
		}
	}
	if !earliest.IsZero() {
		return earliest
	}
	candidate := time.Date(date.Year(), date.Month(), date.Day(), hour, minute, 0, 0, loc)
	local := candidate.In(loc)
	actualWall := time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), local.Minute(), 0, 0, time.UTC)
	if actualWall.Before(wall) {
		candidate = candidate.Add(wall.Sub(actualWall))
	}
	return candidate
}

func modelRequestUsage(rule APIKeyModelRequestLimit, used int64, start, end time.Time) APIKeyModelRequestUsage {
	remaining := rule.MaxRequests - used
	if remaining < 0 {
		remaining = 0
	}
	return APIKeyModelRequestUsage{RuleID: rule.ID, Model: rule.Model, Window: rule.Window, Limit: rule.MaxRequests, Used: used, Remaining: remaining, WindowStart: start, ResetAt: end, Timezone: rule.Timezone}
}

// ConsumeAPIKeyModelRequest atomically checks and charges every matching rule.
// A logical request ID is counted at most once per rule, including after a retry
// crosses a week boundary. Call only at dispatch; failed/uncertain dispatches count.
// Persisted limits are authoritative, including when an old authentication
// cache entry still describes a formerly unrestricted key.
func (db *DB) ConsumeAPIKeyModelRequest(ctx context.Context, keyID int64, requestID, effectiveModel string, rules []APIKeyModelRequestLimit, now time.Time) (*APIKeyModelRequestExhaustion, error) {
	if keyID <= 0 || requestID == "" || len(requestID) > 200 {
		return nil, errors.New("invalid model request quota key or request identity")
	}
	if len(rules) == 0 {
		// One unlocked read keeps truly unrestricted traffic out of the write
		// transaction, while a newly enabled rule cannot hide behind old caches.
		var raw interface{}
		if err := db.conn.QueryRowContext(ctx, `SELECT COALESCE(limits, '{}') FROM api_keys WHERE id=$1`, keyID).Scan(&raw); err != nil {
			return nil, err
		}
		if len(decodeAPIKeyLimits(raw).ModelRequestLimits) == 0 {
			return nil, nil
		}
	}
	var exhausted *APIKeyModelRequestExhaustion
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		current, err := db.lockAPIKeyLimits(ctx, tx, keyID)
		if err != nil {
			return err
		}
		rules, err = NormalizeAPIKeyModelRequestLimits(current.ModelRequestLimits)
		if err != nil {
			return err
		}
		sort.Slice(rules, func(i, j int) bool { return rules[i].ID < rules[j].ID })
		type charge struct {
			rule       APIKeyModelRequestLimit
			start, end time.Time
		}
		charges := make([]charge, 0, len(rules))
		for _, rule := range rules {
			if !MatchAPIKeyModelRequestLimit(rule.Model, effectiveModel) {
				continue
			}
			var exists int
			err := tx.QueryRowContext(ctx, `SELECT 1 FROM api_key_model_request_ledger WHERE api_key_id=$1 AND rule_id=$2 AND request_id=$3`, keyID, rule.ID, requestID).Scan(&exists)
			if err == nil {
				continue
			}
			if !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			start, end, err := APIKeyModelRequestWindow(rule, now)
			if err != nil {
				return err
			}
			var used int64
			err = tx.QueryRowContext(ctx, `SELECT used_requests FROM api_key_model_request_counters WHERE api_key_id=$1 AND rule_id=$2 AND window_start=$3`, keyID, rule.ID, start.Unix()).Scan(&used)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return err
			}
			if used >= rule.MaxRequests {
				exhausted = &APIKeyModelRequestExhaustion{modelRequestUsage(rule, used, start, end)}
				return exhausted
			}
			charges = append(charges, charge{rule: rule, start: start, end: end})
		}
		for _, item := range charges {
			if _, err := tx.ExecContext(ctx, `INSERT INTO api_key_model_request_counters (api_key_id,rule_id,window_start,reset_at,used_requests) VALUES ($1,$2,$3,$4,1) ON CONFLICT (api_key_id,rule_id,window_start) DO UPDATE SET used_requests=api_key_model_request_counters.used_requests+1`, keyID, item.rule.ID, item.start.Unix(), item.end.Unix()); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO api_key_model_request_ledger (api_key_id,rule_id,request_id,window_start,created_at) VALUES ($1,$2,$3,$4,$5)`, keyID, item.rule.ID, requestID, item.start.Unix(), now.Unix()); err != nil {
				return err
			}
		}
		return nil
	})
	if exhausted != nil && errors.Is(err, exhausted) {
		return exhausted, nil
	}
	return nil, err
}

// GetAPIKeyModelRequestUsage reads authoritative counters in configuration order.
func (db *DB) GetAPIKeyModelRequestUsage(ctx context.Context, keyID int64, rules []APIKeyModelRequestLimit, now time.Time) ([]APIKeyModelRequestUsage, error) {
	rules, err := NormalizeAPIKeyModelRequestLimits(rules)
	if err != nil {
		return nil, err
	}
	out := make([]APIKeyModelRequestUsage, 0, len(rules))
	for _, rule := range rules {
		start, end, err := APIKeyModelRequestWindow(rule, now)
		if err != nil {
			return nil, err
		}
		var used int64
		err = db.conn.QueryRowContext(ctx, `SELECT used_requests FROM api_key_model_request_counters WHERE api_key_id=$1 AND rule_id=$2 AND window_start=$3`, keyID, rule.ID, start.Unix()).Scan(&used)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, err
		}
		out = append(out, modelRequestUsage(rule, used, start, end))
	}
	return out, nil
}

func (db *DB) lockAPIKeyLimits(ctx context.Context, tx *sql.Tx, keyID int64) (APIKeyLimits, error) {
	query := `SELECT COALESCE(limits, '{}') FROM api_keys WHERE id=$1`
	if !db.isSQLite() {
		query += ` FOR UPDATE`
	}
	var raw interface{}
	if err := tx.QueryRowContext(ctx, query, keyID).Scan(&raw); err != nil {
		return APIKeyLimits{}, err
	}
	return decodeAPIKeyLimits(raw), nil
}

func (db *DB) validateAPIKeyModelRequestRuleUpdate(ctx context.Context, tx *sql.Tx, keyID int64, rules []APIKeyModelRequestLimit) error {
	previous, err := db.lockAPIKeyLimits(ctx, tx, keyID)
	if err != nil {
		return err
	}
	oldRules, err := NormalizeAPIKeyModelRequestLimits(previous.ModelRequestLimits)
	if err != nil {
		return err
	}
	oldByID := make(map[string]APIKeyModelRequestLimit, len(oldRules))
	for _, rule := range oldRules {
		oldByID[rule.ID] = rule
	}
	for _, rule := range rules {
		if old, ok := oldByID[rule.ID]; ok {
			if old.Model != rule.Model || old.Window != rule.Window || old.Timezone != rule.Timezone || old.ResetWeekday != rule.ResetWeekday || old.ResetTime != rule.ResetTime {
				return fmt.Errorf("model request rule %s: model and reset schedule are immutable; create a new rule", rule.ID)
			}
		}
	}
	return nil
}
