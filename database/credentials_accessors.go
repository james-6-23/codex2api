package database

import (
	"fmt"
	"strconv"
	"strings"
)

func (a *AccountRow) GetCredentialFloat64(key string) (float64, bool) {
	if a == nil || a.Credentials == nil {
		return 0, false
	}
	v, ok := a.Credentials[key]
	if !ok || v == nil {
		return 0, false
	}
	switch val := v.(type) {
	case float64:
		return val, true
	case float32:
		return float64(val), true
	case int:
		return float64(val), true
	case int64:
		return float64(val), true
	case string:
		parsed, err := strconv.ParseFloat(val, 64)
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

func (a *AccountRow) GetCredentialBool(key string) bool {
	if a == nil || a.Credentials == nil {
		return false
	}
	v, ok := a.Credentials[key]
	if !ok || v == nil {
		return false
	}
	switch val := v.(type) {
	case bool:
		return val
	case string:
		parsed, err := strconv.ParseBool(val)
		return err == nil && parsed
	default:
		return false
	}
}

func (a *AccountRow) GetCredentialStringMap(key string) map[string]string {
	if a == nil || a.Credentials == nil {
		return nil
	}
	v, ok := a.Credentials[key]
	if !ok || v == nil {
		return nil
	}
	switch val := v.(type) {
	case map[string]string:
		return cloneTrimmedStringMap(val)
	case map[string]interface{}:
		out := make(map[string]string, len(val))
		for key, raw := range val {
			name := strings.TrimSpace(key)
			if name == "" || raw == nil {
				continue
			}
			out[name] = strings.TrimSpace(fmt.Sprint(raw))
		}
		if len(out) == 0 {
			return nil
		}
		return out
	default:
		return nil
	}
}

func cloneTrimmedStringMap(values map[string]string) map[string]string {
	if len(values) == 0 {
		return nil
	}
	out := make(map[string]string, len(values))
	for key, value := range values {
		name := strings.TrimSpace(key)
		if name == "" {
			continue
		}
		out[name] = strings.TrimSpace(value)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
