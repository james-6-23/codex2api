package proxy

import (
	"net/http"
	"strings"

	"github.com/codex2api/auth"
)

// CodexCLIVersionPlaceholder opts a pinned CLI identity into the gateway's
// synchronized CLI version. Everything outside this explicit token stays fixed.
const CodexCLIVersionPlaceholder = "{{codex_cli_version}}"

// CodexAccountCustomHeaders returns a copy resolved from one version snapshot,
// so User-Agent and Version cannot observe different versions during a sync.
// Literal overrides and non-Codex upstream headers keep their original values.
func CodexAccountCustomHeaders(account *auth.Account) map[string]string {
	headers := account.GetCustomHeaders()
	if account == nil || account.IsRelayStyle() || account.IsOpenAIResponsesAPI() || account.IsAntigravityAPI() || account.IsClaudeOAuth() {
		return headers
	}
	version := effectiveLatestCodexCLIVersion()
	for name, value := range headers {
		switch http.CanonicalHeaderKey(strings.TrimSpace(name)) {
		case "User-Agent", "Version":
			headers[name] = strings.ReplaceAll(value, CodexCLIVersionPlaceholder, version)
		}
	}
	return headers
}
