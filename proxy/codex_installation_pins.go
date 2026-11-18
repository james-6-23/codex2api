package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/codex2api/auth"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// These are internal gateway settings, never request headers or body fields.
func codexInstallationPins() (map[string]string, error) {
	raw := strings.TrimSpace(os.Getenv("CODEX_ACCOUNT_INSTALLATION_IDS"))
	if raw == "" {
		return nil, nil
	}
	var pins map[string]string
	if err := json.Unmarshal([]byte(raw), &pins); err != nil {
		return nil, fmt.Errorf("CODEX_ACCOUNT_INSTALLATION_IDS must be an account ID to UUID map")
	}
	for id, value := range pins {
		n, err := strconv.ParseInt(id, 10, 64)
		u, uuidErr := uuid.Parse(value)
		if err != nil || n <= 0 || uuidErr != nil || u.Version() != 4 || u.String() != value {
			return nil, fmt.Errorf("invalid account ID or installation UUID in CODEX_ACCOUNT_INSTALLATION_IDS")
		}
	}
	return pins, nil
}

// ValidateCodexInstallationPins rejects invalid persistent identity configuration
// at startup rather than silently changing an account's upstream identity.
func ValidateCodexInstallationPins() error {
	_, err := codexInstallationPins()
	return err
}

func configuredCodexInstallationID(account *auth.Account) string {
	if account == nil {
		return ""
	}
	pins, _ := codexInstallationPins() // Validated before the server starts.
	return pins[strconv.FormatInt(account.ID(), 10)]
}

// FinalizeCodexIdentityHeaders follows Codex rust-v0.160.0,
// core/src/responses_metadata.rs::compatibility_headers. Installation ID is a
// body metadata value; tool_namespaces_info is excluded from the bounded header
// projection while the complete metadata remains in the request body.
func FinalizeCodexIdentityHeaders(headers http.Header, account *auth.Account) {
	if headers == nil || configuredCodexInstallationID(account) == "" {
		return
	}
	deleteHeaderCaseInsensitive(headers, codexInstallationIDHeader)
	raw := headers.Get(codexTurnMetadataHeader)
	if gjson.Valid(raw) && gjson.Get(raw, "tool_namespaces_info").Exists() {
		if bounded, err := sjson.Delete(raw, "tool_namespaces_info"); err == nil {
			headers.Set(codexTurnMetadataHeader, bounded)
		}
	}
}

// Official WS turn state travels per response.create frame, not in the
// handshake (core/src/client.rs::build_websocket_headers passes None).
// Call after copying the guarded turn state into the frame.
func FinalizeCodexWebsocketHandshakeHeaders(headers http.Header, account *auth.Account) {
	if headers != nil && configuredCodexInstallationID(account) != "" {
		deleteHeaderCaseInsensitive(headers, "x-codex-turn-state")
	}
}
