package auth

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"unicode"
)

// Antigravity OAuth client 的双来源配置：环境变量 ANTIGRAVITY_OAUTH_CLIENTS 与系统
// 设置（system_settings.antigravity_oauth_config，管理页可编辑、随保存热更新）。
// 与 Grok 的 oauth_client_id 一致，环境变量压在系统设置之上：它属于部署级配置，
// 数据库里的值被误改时仍能从部署侧兜住。同 key 冲突时环境变量条目生效。
// 两边都未配置时回落到内置官方 Antigravity Desktop client。
const (
	AntigravityOAuthClientMaxCount     = 16
	AntigravityOAuthClientKeyMaxLen    = 64
	AntigravityOAuthClientIDMaxLen     = 256
	AntigravityOAuthClientSecretMaxLen = 256
)

// AntigravityOAuthClientConfig 是系统设置里的一条 OAuth client 配置。
type AntigravityOAuthClientConfig struct {
	Key          string `json:"key"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
}

// AntigravityOAuthSettings 是 antigravity_oauth_config 列的 JSON 形态。
type AntigravityOAuthSettings struct {
	Clients   []AntigravityOAuthClientConfig `json:"clients"`
	ActiveKey string                         `json:"active_key"`
}

// configuredAntigravityOAuth 持有系统设置里的 OAuth client 配置，随设置保存热更新。
var configuredAntigravityOAuth atomic.Value // AntigravityOAuthSettings

// SetConfiguredAntigravityOAuth 热更新系统设置里的 OAuth client 配置。
// 传入值不再二次归一化：调用方（启动加载/设置保存）负责先走
// NormalizeAntigravityOAuthSettings，避免两处规则漂移。
func SetConfiguredAntigravityOAuth(settings AntigravityOAuthSettings) {
	configuredAntigravityOAuth.Store(settings)
}

// ConfiguredAntigravityOAuth 返回系统设置里配的 OAuth client 配置副本。
func ConfiguredAntigravityOAuth() AntigravityOAuthSettings {
	v, _ := configuredAntigravityOAuth.Load().(AntigravityOAuthSettings)
	if len(v.Clients) == 0 {
		return AntigravityOAuthSettings{ActiveKey: v.ActiveKey}
	}
	clients := make([]AntigravityOAuthClientConfig, len(v.Clients))
	copy(clients, v.Clients)
	return AntigravityOAuthSettings{Clients: clients, ActiveKey: v.ActiveKey}
}

// ParseAntigravityOAuthSettings 解析并归一化 antigravity_oauth_config JSON。
// 空串/`{}` 表示未配置。解析失败视为配置损坏，返回错误而不是静默清空，
// 由调用方决定回落行为。
func ParseAntigravityOAuthSettings(raw string) (AntigravityOAuthSettings, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" {
		return AntigravityOAuthSettings{}, nil
	}
	var settings AntigravityOAuthSettings
	if err := json.Unmarshal([]byte(raw), &settings); err != nil {
		return AntigravityOAuthSettings{}, fmt.Errorf("parse antigravity oauth settings: %w", err)
	}
	return NormalizeAntigravityOAuthSettings(settings)
}

// EncodeAntigravityOAuthSettings 把配置编码成落库 JSON。空配置编码成 `{}`。
func EncodeAntigravityOAuthSettings(settings AntigravityOAuthSettings) (string, error) {
	normalized, err := NormalizeAntigravityOAuthSettings(settings)
	if err != nil {
		return "", err
	}
	if len(normalized.Clients) == 0 {
		return "{}", nil
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// NormalizeAntigravityOAuthSettings 校验并归一化配置：key 统一小写、去空白，
// 拒绝重复 key、非法字符与超长字段。active_key 必须指向已配置的条目（或为空）。
func NormalizeAntigravityOAuthSettings(settings AntigravityOAuthSettings) (AntigravityOAuthSettings, error) {
	if len(settings.Clients) > AntigravityOAuthClientMaxCount {
		return AntigravityOAuthSettings{}, fmt.Errorf("antigravity oauth clients exceed the maximum of %d entries", AntigravityOAuthClientMaxCount)
	}
	result := AntigravityOAuthSettings{}
	seen := make(map[string]struct{}, len(settings.Clients))
	for index, client := range settings.Clients {
		key := strings.ToLower(strings.TrimSpace(client.Key))
		clientID := strings.TrimSpace(client.ClientID)
		clientSecret := strings.TrimSpace(client.ClientSecret)
		if key == "" {
			return AntigravityOAuthSettings{}, fmt.Errorf("antigravity oauth client %d has an empty key", index+1)
		}
		if err := validateAntigravityOAuthField(key, AntigravityOAuthClientKeyMaxLen); err != nil {
			return AntigravityOAuthSettings{}, fmt.Errorf("antigravity oauth client %q key: %w", key, err)
		}
		// key 会进 `key|id|secret;...` 的 env 序列化语法与凭据行,分隔符必须拒绝。
		if strings.ContainsAny(key, "|;") {
			return AntigravityOAuthSettings{}, fmt.Errorf("antigravity oauth client %q key: must not contain '|' or ';'", key)
		}
		if clientID == "" {
			return AntigravityOAuthSettings{}, fmt.Errorf("antigravity oauth client %q has an empty client_id", key)
		}
		if err := validateAntigravityOAuthField(clientID, AntigravityOAuthClientIDMaxLen); err != nil {
			return AntigravityOAuthSettings{}, fmt.Errorf("antigravity oauth client %q client_id: %w", key, err)
		}
		if clientSecret == "" {
			return AntigravityOAuthSettings{}, fmt.Errorf("antigravity oauth client %q has an empty client_secret", key)
		}
		if err := validateAntigravityOAuthField(clientSecret, AntigravityOAuthClientSecretMaxLen); err != nil {
			return AntigravityOAuthSettings{}, fmt.Errorf("antigravity oauth client %q client_secret: %w", key, err)
		}
		if _, duplicate := seen[key]; duplicate {
			return AntigravityOAuthSettings{}, fmt.Errorf("antigravity oauth client key %q is duplicated", key)
		}
		seen[key] = struct{}{}
		result.Clients = append(result.Clients, AntigravityOAuthClientConfig{
			Key: key, ClientID: clientID, ClientSecret: clientSecret,
		})
	}
	activeKey := strings.ToLower(strings.TrimSpace(settings.ActiveKey))
	if activeKey != "" {
		if _, ok := seen[activeKey]; !ok {
			return AntigravityOAuthSettings{}, fmt.Errorf("antigravity oauth active key %q does not match any configured client", activeKey)
		}
	}
	result.ActiveKey = activeKey
	return result, nil
}

func validateAntigravityOAuthField(value string, maxLen int) error {
	if len(value) > maxLen {
		return fmt.Errorf("must not exceed %d characters", maxLen)
	}
	for _, r := range value {
		if unicode.IsSpace(r) || unicode.IsControl(r) || r > unicode.MaxASCII {
			return errors.New("must not contain whitespace, control, or non-ASCII characters")
		}
	}
	return nil
}

// AntigravityOAuthEnvClients 返回环境变量 ANTIGRAVITY_OAUTH_CLIENTS 里配置的
// client 列表（不含 secret 的公开视图），供管理端展示「env 覆盖」提示。
func AntigravityOAuthEnvClients() []AntigravityOAuthClientInfo {
	clients := antigravityOAuthClientsFromEnv()
	result := make([]AntigravityOAuthClientInfo, 0, len(clients))
	for _, client := range clients {
		result = append(result, AntigravityOAuthClientInfo{Key: client.Key, ClientID: client.ClientID})
	}
	return result
}

// AntigravityOAuthActiveKeyFromEnv 返回环境变量指定的活跃 key（未设为空）。
func AntigravityOAuthActiveKeyFromEnv() string {
	return antigravityActiveOAuthKeyFromEnv()
}

// EffectiveAntigravityOAuthClients 返回合并后的生效 client 列表与活跃 key。
// 合并顺序：环境变量条目在前并在 key 冲突时生效；活跃 key 优先级为
// 环境变量 > 系统设置 > 第一个条目。
func EffectiveAntigravityOAuthClients() ([]AntigravityOAuthClientInfo, string) {
	clients, activeKey := effectiveAntigravityOAuthClients()
	result := make([]AntigravityOAuthClientInfo, 0, len(clients))
	for _, client := range clients {
		result = append(result, AntigravityOAuthClientInfo{Key: client.Key, ClientID: client.ClientID})
	}
	return result, activeKey
}

func effectiveAntigravityOAuthClients() ([]antigravityOAuthClient, string) {
	clients := antigravityOAuthClientsFromEnv()
	seen := make(map[string]struct{}, len(clients))
	for _, client := range clients {
		seen[client.Key] = struct{}{}
	}
	configured := ConfiguredAntigravityOAuth()
	for _, client := range configured.Clients {
		if _, ok := seen[client.Key]; ok {
			continue
		}
		seen[client.Key] = struct{}{}
		clients = append(clients, antigravityOAuthClient{
			Key: client.Key, ClientID: client.ClientID, ClientSecret: client.ClientSecret,
		})
	}
	active := antigravityActiveOAuthKeyFromEnv()
	if active == "" {
		if _, ok := seen[configured.ActiveKey]; ok && configured.ActiveKey != "" {
			active = configured.ActiveKey
		}
	}
	if len(clients) == 0 {
		builtin := builtinAntigravityOAuthClient()
		clients = append(clients, builtin)
		seen[builtin.Key] = struct{}{}
	}
	if active == "" && len(clients) > 0 {
		active = clients[0].Key
	}
	return clients, active
}

// UsingBuiltinAntigravityOAuth 表示当前没有环境变量或系统设置条目，
// 生效 client 来自内置官方 Desktop 凭据。
func UsingBuiltinAntigravityOAuth() bool {
	return len(antigravityOAuthClientsFromEnv()) == 0 && len(ConfiguredAntigravityOAuth().Clients) == 0
}

// BuiltinAntigravityOAuthClient 返回内置官方 client 的公开视图（不含 secret）。
func BuiltinAntigravityOAuthClient() AntigravityOAuthClientInfo {
	builtin := builtinAntigravityOAuthClient()
	return AntigravityOAuthClientInfo{Key: builtin.Key, ClientID: builtin.ClientID}
}
