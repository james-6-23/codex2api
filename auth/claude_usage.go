package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ClaudeOAuthUsageURL is the zero-spend usage endpoint used by Claude Code.
const ClaudeOAuthUsageURL = "https://api.anthropic.com/api/oauth/usage"

// ClaudeUsageWindow is one account-level or model-family usage bucket. Percent
// values use the OAuth endpoint's 0..100 scale (not the response-header 0..1
// scale used by the Messages API).
type ClaudeUsageWindow struct {
	Name        string    `json:"name"`
	Label       string    `json:"label,omitempty"`
	Utilization float64   `json:"utilization"`
	ResetAt     time.Time `json:"reset_at,omitempty"`
	ModelScoped bool      `json:"model_scoped,omitempty"`
	ModelFamily string    `json:"model_family,omitempty"`
}

// MarshalJSON omits reset_at when the upstream bucket carried no reset time;
// omitempty does not apply to time.Time so the zero value would otherwise be
// serialized as 0001-01-01 and rendered as a real date downstream.
func (w ClaudeUsageWindow) MarshalJSON() ([]byte, error) {
	type alias ClaudeUsageWindow
	payload := struct {
		alias
		ResetAt *time.Time `json:"reset_at,omitempty"`
	}{alias: alias(w)}
	if !w.ResetAt.IsZero() {
		reset := w.ResetAt
		payload.ResetAt = &reset
	}
	return json.Marshal(payload)
}

type claudeUsageResponse struct {
	FiveHour *claudeUsageBucket `json:"five_hour"`
	SevenDay *claudeUsageBucket `json:"seven_day"`
	Limits   []claudeUsageLimit `json:"limits"`
}

type claudeUsageBucket struct {
	Utilization float64         `json:"utilization"`
	ResetsAt    json.RawMessage `json:"resets_at"`
}

type claudeUsageLimit struct {
	Group    string           `json:"group"`
	Percent  float64          `json:"percent"`
	ResetsAt json.RawMessage  `json:"resets_at"`
	Scope    claudeUsageScope `json:"scope"`
}

type claudeUsageScope struct {
	Model *struct {
		DisplayName string `json:"display_name"`
		ID          string `json:"id"`
	} `json:"model"`
}

// FetchUsage fetches Claude's OAuth usage page without spending inference
// tokens. It deliberately uses the same primary/fallback clients as profile
// and model discovery so configured proxy/fingerprint behavior is preserved.
func (o *ClaudeAuth) FetchUsage(ctx context.Context, accessToken string) ([]ClaudeUsageWindow, error) {
	if strings.TrimSpace(accessToken) == "" {
		return nil, fmt.Errorf("缺少 access token")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	resp, err := o.doWithFallback(ctx, http.MethodGet, ClaudeOAuthUsageURL, nil, func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer "+accessToken)
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Set("anthropic-beta", ClaudeOAuthBeta)
	})
	if err != nil {
		return nil, fmt.Errorf("usage 请求失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := readClaudeOAuthResponseBody(resp)
	if err != nil {
		return nil, fmt.Errorf("读取 usage 响应失败: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("获取 usage 失败 (status %d): %s", resp.StatusCode, string(body))
	}
	return ParseClaudeOAuthUsage(body)
}

// ParseClaudeOAuthUsage normalizes the OAuth response to stable UI/API window
// names. Anthropic exposes Fable as a shared weekly limit in limits[], so both
// Fable 5 and 5.1 intentionally map to the single 7d_fable bucket.
func ParseClaudeOAuthUsage(body []byte) ([]ClaudeUsageWindow, error) {
	var parsed claudeUsageResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("解析 Claude usage 响应失败: %w", err)
	}
	windows := make([]ClaudeUsageWindow, 0, 2+len(parsed.Limits))
	if parsed.FiveHour != nil {
		windows = append(windows, ClaudeUsageWindow{
			Name: "5h", Label: "5h", Utilization: clampClaudeUsagePercent(parsed.FiveHour.Utilization), ResetAt: parseClaudeUsageTime(parsed.FiveHour.ResetsAt),
		})
	}
	if parsed.SevenDay != nil {
		windows = append(windows, ClaudeUsageWindow{
			Name: "7d", Label: "7d", Utilization: clampClaudeUsagePercent(parsed.SevenDay.Utilization), ResetAt: parseClaudeUsageTime(parsed.SevenDay.ResetsAt),
		})
	}
	seen := make(map[string]struct{})
	for _, limit := range parsed.Limits {
		if !strings.EqualFold(strings.TrimSpace(limit.Group), "weekly") || limit.Scope.Model == nil {
			continue
		}
		family := claudeUsageModelFamily(limit.Scope.Model.DisplayName + " " + limit.Scope.Model.ID)
		if family == "" {
			continue
		}
		name := "7d_" + family
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		windows = append(windows, ClaudeUsageWindow{
			Name: name, Label: claudeUsageFamilyLabel(family) + " 5.x", Utilization: clampClaudeUsagePercent(limit.Percent),
			ResetAt: parseClaudeUsageTime(limit.ResetsAt), ModelScoped: true, ModelFamily: family,
		})
	}
	return windows, nil
}

func claudeUsageFamilyLabel(family string) string {
	words := strings.Fields(strings.ReplaceAll(family, "_", " "))
	for i, word := range words {
		words[i] = strings.ToUpper(word[:1]) + word[1:]
	}
	return strings.Join(words, " ")
}

func claudeUsageModelFamily(value string) string {
	lower := strings.ToLower(value)
	if strings.Contains(lower, "fable") {
		return "fable"
	}
	if strings.Contains(lower, "mythos") {
		return "mythos"
	}
	return ""
}

func clampClaudeUsagePercent(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

func parseClaudeUsageTime(raw json.RawMessage) time.Time {
	if len(raw) == 0 || string(raw) == "null" {
		return time.Time{}
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		if t, err := time.Parse(time.RFC3339, strings.TrimSpace(text)); err == nil {
			return t
		}
		if seconds, err := strconv.ParseInt(strings.TrimSpace(text), 10, 64); err == nil {
			return time.Unix(seconds, 0).UTC()
		}
	}
	var number float64
	if json.Unmarshal(raw, &number) == nil && number > 0 {
		return time.Unix(int64(number), 0).UTC()
	}
	return time.Time{}
}
