package auth

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
)

// Antigravity 渠道级设置（system_settings.antigravity_config 列）。目前只有模型
// 重定向：下游请求不带思考强度后缀的逻辑模型（如 gemini-3.8-flash）时，自动按配置
// 落到某个固定档位（如 gemini-3.8-flash-high）。

// AntigravityModelRedirectMaxEntries 限制重定向条目数，避免无界 JSON。
const AntigravityModelRedirectMaxEntries = 64

// AntigravitySettings 是 antigravity_config 列的 JSON 形态。
type AntigravitySettings struct {
	// ModelRedirects 以逻辑模型（无后缀）为键，固定档位公开 ID 为值。
	ModelRedirects map[string]string `json:"model_redirects,omitempty"`
	// RedirectOverridesEffort 为 true 时，即使请求自带 reasoning.effort 也按重定向
	// 走；默认只在请求没有指定思考强度时生效。
	RedirectOverridesEffort bool `json:"redirect_overrides_effort,omitempty"`
}

var configuredAntigravitySettings atomic.Value // AntigravitySettings

// SetConfiguredAntigravitySettings 热更新 Antigravity 渠道设置。调用方负责先归一化。
func SetConfiguredAntigravitySettings(settings AntigravitySettings) {
	configuredAntigravitySettings.Store(settings)
}

// ConfiguredAntigravitySettings 返回当前生效设置的副本。
func ConfiguredAntigravitySettings() AntigravitySettings {
	v, _ := configuredAntigravitySettings.Load().(AntigravitySettings)
	out := AntigravitySettings{RedirectOverridesEffort: v.RedirectOverridesEffort}
	if len(v.ModelRedirects) > 0 {
		out.ModelRedirects = make(map[string]string, len(v.ModelRedirects))
		for key, value := range v.ModelRedirects {
			out.ModelRedirects[key] = value
		}
	}
	return out
}

// AntigravityModelRedirect 返回逻辑模型配置的重定向目标；未配置返回空串。
func AntigravityModelRedirect(model string) string {
	v, _ := configuredAntigravitySettings.Load().(AntigravitySettings)
	if len(v.ModelRedirects) == 0 {
		return ""
	}
	return v.ModelRedirects[strings.ToLower(strings.TrimSpace(model))]
}

// AntigravityRedirectOverridesEffort 报告重定向是否压过请求自带的思考强度。
func AntigravityRedirectOverridesEffort() bool {
	v, _ := configuredAntigravitySettings.Load().(AntigravitySettings)
	return v.RedirectOverridesEffort
}

// ParseAntigravitySettings 解析 antigravity_config JSON。空串/`{}` 表示未配置；
// 解析失败返回错误而不是静默清空，由调用方决定回落。
func ParseAntigravitySettings(raw string) (AntigravitySettings, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" {
		return AntigravitySettings{}, nil
	}
	var settings AntigravitySettings
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		return AntigravitySettings{}, fmt.Errorf("parse antigravity settings: %w", err)
	}
	return NormalizeAntigravitySettings(settings)
}

// EncodeAntigravitySettings 把设置编码成落库 JSON。空配置编码成 `{}`。
func EncodeAntigravitySettings(settings AntigravitySettings) (string, error) {
	normalized, err := NormalizeAntigravitySettings(settings)
	if err != nil {
		return "", err
	}
	if len(normalized.ModelRedirects) == 0 && !normalized.RedirectOverridesEffort {
		return "{}", nil
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// NormalizeAntigravitySettings 去空白、统一小写、丢弃空值条目并限制条目数。
// 目标模型是否真是该逻辑模型的档位由 admin 层结合模型目录校验（auth 不依赖 proxy）。
func NormalizeAntigravitySettings(settings AntigravitySettings) (AntigravitySettings, error) {
	out := AntigravitySettings{RedirectOverridesEffort: settings.RedirectOverridesEffort}
	if len(settings.ModelRedirects) == 0 {
		return out, nil
	}
	keys := make([]string, 0, len(settings.ModelRedirects))
	for key := range settings.ModelRedirects {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, rawKey := range keys {
		key := strings.ToLower(strings.TrimSpace(rawKey))
		value := strings.ToLower(strings.TrimSpace(settings.ModelRedirects[rawKey]))
		if key == "" || value == "" || key == value {
			continue
		}
		if out.ModelRedirects == nil {
			out.ModelRedirects = make(map[string]string, len(keys))
		}
		out.ModelRedirects[key] = value
		if len(out.ModelRedirects) > AntigravityModelRedirectMaxEntries {
			return AntigravitySettings{}, fmt.Errorf("antigravity model redirects exceed the maximum of %d entries", AntigravityModelRedirectMaxEntries)
		}
	}
	return out, nil
}
