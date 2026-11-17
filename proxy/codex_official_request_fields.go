package proxy

import (
	"os"
	"strings"

	"github.com/tidwall/gjson"
)

// This opt-in preserves the payload fields and absent defaults verified against
// Codex 0.160.0. It does not bypass identity isolation or other schema repairs.
// Metadata identifies a payload shape, not a trusted or authenticated client.
func codexOfficialRequestFieldsEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CODEX_PRESERVE_OFFICIAL_REQUEST_FIELDS"))) {
	case "1", "true", "on":
		return true
	default:
		return false
	}
}

func hasCodexOfficialRequestMetadata(installationID, turnMetadata string) bool {
	if strings.TrimSpace(installationID) == "" || !gjson.Valid(turnMetadata) {
		return false
	}
	meta := gjson.Parse(turnMetadata)
	return meta.IsObject() && meta.Get("installation_id").String() == installationID &&
		strings.TrimSpace(meta.Get("session_id").String()) != "" &&
		strings.TrimSpace(meta.Get("thread_id").String()) != "" &&
		strings.TrimSpace(meta.Get("request_kind").String()) != ""
}

func preserveCodexOfficialInputFields(body map[string]any) bool {
	if !codexOfficialRequestFieldsEnabled() {
		return false
	}
	metadata, ok := body["client_metadata"].(map[string]any)
	if !ok {
		return false
	}
	installationID, _ := metadata["x-codex-installation-id"].(string)
	turnMetadata, _ := metadata["x-codex-turn-metadata"].(string)
	return hasCodexOfficialRequestMetadata(installationID, turnMetadata)
}

// PreserveCodexOfficialRequestFields also runs at the HTTP/WS output boundary,
// after normal preparation and account identity rewrites. Existing instructions
// are always retained; this only decides whether missing instructions stay absent.
func PreserveCodexOfficialRequestFields(body []byte) bool {
	if !codexOfficialRequestFieldsEnabled() {
		return false
	}
	metadata := gjson.GetBytes(body, "client_metadata")
	return hasCodexOfficialRequestMetadata(metadata.Get("x-codex-installation-id").String(),
		metadata.Get("x-codex-turn-metadata").String())
}
