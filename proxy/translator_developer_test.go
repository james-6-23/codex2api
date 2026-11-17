package proxy

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/tidwall/gjson"
)

func TestTranslateRequestPreservesDeveloperMessages(t *testing.T) {
	for _, segmented := range []bool{false, true} {
		name := "plain_text"
		if segmented {
			name = "text_parts"
		}
		t.Run(name, func(t *testing.T) {
			roles := []string{"system", "developer", "user", "assistant", "developer", "user"}
			wantRoles := []string{"developer", "developer", "user", "assistant", "developer", "user"}
			messages := make([]map[string]any, 0, len(roles))
			wantTexts := make([][]string, len(roles))
			for i, role := range roles {
				text := fmt.Sprintf(" %s 指令 %d\n", role, i)
				var content any = text
				wantTexts[i] = []string{text}
				if segmented {
					second := fmt.Sprintf("第二段 %d", i)
					content = []map[string]string{{"type": "text", "text": text}, {"type": "text", "text": second}}
					wantTexts[i] = append(wantTexts[i], second)
				}
				messages = append(messages, map[string]any{"role": role, "content": content})
			}
			raw, err := json.Marshal(map[string]any{"model": "gpt-5.4", "messages": messages})
			if err != nil {
				t.Fatal(err)
			}
			got, err := TranslateRequest(raw)
			if err != nil {
				t.Fatal(err)
			}
			input := gjson.GetBytes(got, "input").Array()
			if len(input) != len(messages) {
				t.Fatalf("input length = %d, want %d; body=%s", len(input), len(messages), got)
			}
			for i, item := range input {
				if item.Get("type").String() != "message" || item.Get("role").String() != wantRoles[i] {
					t.Errorf("input[%d] = %s, want message with role %q", i, item.Raw, wantRoles[i])
				}
				parts := item.Get("content").Array()
				if len(parts) != len(wantTexts[i]) {
					t.Fatalf("input[%d] content length = %d, want %d", i, len(parts), len(wantTexts[i]))
				}
				wantType := "input_text"
				if roles[i] == "assistant" {
					wantType = "output_text"
				}
				for j, part := range parts {
					if part.Get("type").String() != wantType || part.Get("text").String() != wantTexts[i][j] {
						t.Errorf("input[%d].content[%d] = %s, want %s with text %q", i, j, part.Raw, wantType, wantTexts[i][j])
					}
				}
			}
		})
	}
}
