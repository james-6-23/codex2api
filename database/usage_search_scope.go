package database

import (
	"fmt"
	"strings"
)

func ValidUsageSearchScope(scope string) bool {
	switch scope {
	case "", "all", "session", "request", "error", "model", "endpoint", "account", "newapi_user", "ip", "user_agent", "api_key":
		return true
	default:
		return false
	}
}

// Only server-owned expressions can become SQL; the query stays a bound value.
func usageSearchPredicate(scope, placeholder string) string {
	groups := []struct {
		name    string
		columns []string
	}{
		{"session", []string{"u.session_id_prefix"}},
		{"request", []string{"u.request_id", "u.upstream_request_id"}},
		{"error", []string{"u.error_message", "u.upstream_error_kind"}},
		{"model", []string{"u.model", "u.effective_model"}},
		{"endpoint", []string{"u.inbound_endpoint", "u.upstream_endpoint"}},
		{"api_key", []string{"u.api_key_name", "u.api_key_masked"}},
		{"newapi_user", []string{"u.newapi_user_name"}},
		{"ip", []string{"u.client_ip"}},
		{"user_agent", []string{"u.client_user_agent"}},
	}
	if !ValidUsageSearchScope(scope) {
		return "1=0"
	}
	all := scope == "" || scope == "all"
	parts := make([]string, 0, 18)
	for _, group := range groups {
		if !all && scope != group.name {
			continue
		}
		for _, column := range group.columns {
			parts = append(parts, fmt.Sprintf("LOWER(COALESCE(%s, '')) LIKE LOWER(%s)", column, placeholder))
		}
	}
	if all || scope == "account" {
		parts = append(parts, fmt.Sprintf(`u.account_id IN (
			SELECT search_accounts.id FROM accounts search_accounts
			WHERE LOWER(COALESCE(search_accounts.name, '')) LIKE LOWER(%[1]s)
			OR LOWER(COALESCE(CAST(search_accounts.credentials AS TEXT), '')) LIKE LOWER(%[1]s)
		)`, placeholder))
	}
	return "(" + strings.Join(parts, " OR ") + ")"
}
