package wsrelay

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/codex2api/proxy"
	"github.com/tidwall/gjson"
)

func TestOfficialFieldsWebSocketInstructionsAndInput(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		t.Run(fmt.Sprint(enabled), func(t *testing.T) {
			t.Setenv("CODEX_PRESERVE_OFFICIAL_REQUEST_FIELDS", fmt.Sprint(enabled))
			embedded := `{"installation_id":"client","session_id":"thread","thread_id":"thread","request_kind":"turn"}`
			body, _ := json.Marshal(map[string]any{"model": "gpt-6-luna",
				"client_metadata": map[string]string{"x-codex-installation-id": "client", "x-codex-turn-metadata": embedded},
				"input": []any{map[string]any{"type": "message", "role": "user", "id": "msg_original", "content": "OK",
					"internal_chat_message_metadata_passthrough": map[string]any{"content_item_kinds": []string{"user.text"}}}}})
			prepared, _ := proxy.PrepareResponsesWebSocketBody(body)
			result := (&Executor{}).prepareWebsocketBody(prepared, "thread")
			if gjson.GetBytes(result, "instructions").Exists() == enabled {
				t.Fatal("WS instructions compatibility scope incorrect")
			}
			if gjson.GetBytes(result, "input.0.id").Exists() != enabled || gjson.GetBytes(result, "input.0.internal_chat_message_metadata_passthrough").Exists() != enabled {
				t.Fatal("WS input preservation scope incorrect")
			}
			if gjson.GetBytes(result, "type").String() != "response.create" {
				t.Fatal("WS envelope was lost")
			}
		})
	}
}
