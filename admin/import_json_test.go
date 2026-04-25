package admin

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestParseImportJSONTokensSupportsFlatObjectWithBOM(t *testing.T) {
	data := append([]byte{0xef, 0xbb, 0xbf}, []byte(`{"refresh_token":"rt-flat","email":"flat@example.com"}`)...)

	tokens, err := parseImportJSONTokens(data)
	if err != nil {
		t.Fatalf("parseImportJSONTokens returned error: %v", err)
	}

	if len(tokens) != 1 {
		t.Fatalf("tokens len = %d, want 1", len(tokens))
	}
	if tokens[0].refreshToken != "rt-flat" {
		t.Fatalf("refreshToken = %q, want %q", tokens[0].refreshToken, "rt-flat")
	}
	if tokens[0].name != "flat@example.com" {
		t.Fatalf("name = %q, want %q", tokens[0].name, "flat@example.com")
	}
	if tokens[0].accessToken != "" {
		t.Fatalf("accessToken = %q, want empty", tokens[0].accessToken)
	}
}

func TestParseImportJSONTokensSupportsFlatArray(t *testing.T) {
	data := []byte(`[
		{"refresh_token":"rt-1","email":"one@example.com"},
		{"access_token":"at-2","email":"two@example.com"},
		{"refresh_token":"","access_token":"","email":"ignored@example.com"}
	]`)

	tokens, err := parseImportJSONTokens(data)
	if err != nil {
		t.Fatalf("parseImportJSONTokens returned error: %v", err)
	}

	if len(tokens) != 2 {
		t.Fatalf("tokens len = %d, want 2", len(tokens))
	}
	if tokens[0].refreshToken != "rt-1" || tokens[0].name != "one@example.com" {
		t.Fatalf("first token = %+v, want rt-1 / one@example.com", tokens[0])
	}
	if tokens[1].accessToken != "at-2" || tokens[1].name != "two@example.com" {
		t.Fatalf("second token = %+v, want at-2 / two@example.com", tokens[1])
	}
}

func TestParseImportJSONTokensSupportsSub2API(t *testing.T) {
	data := []byte(`{
		"exported_at": "2026-04-03T14:49:53Z",
		"proxies": [
			{"proxy_key":"http|10.0.1.4|80|user|pass","name":"ignored proxy"}
		],
		"accounts": [
			{
				"name": "Primary Account",
				"proxy_key": "http|10.0.1.4|80|user|pass",
				"credentials": {
					"refresh_token": "rt-primary",
					"access_token": "at-primary",
					"email": "primary@example.com"
				},
				"extra": {"ignored": true}
			},
			{
				"credentials": {
					"access_token": "at-email-fallback",
					"email": "fallback@example.com"
				}
			},
			{
				"credentials": {
					"access_token": "at-default-name"
				}
			},
			{
				"name": "Ignored Account",
				"credentials": {}
			}
		]
	}`)

	tokens, err := parseImportJSONTokens(data)
	if err != nil {
		t.Fatalf("parseImportJSONTokens returned error: %v", err)
	}

	if len(tokens) != 3 {
		t.Fatalf("tokens len = %d, want 3", len(tokens))
	}

	if tokens[0].refreshToken != "rt-primary" {
		t.Fatalf("first refreshToken = %q, want %q", tokens[0].refreshToken, "rt-primary")
	}
	if tokens[0].accessToken != "at-primary" {
		t.Fatalf("first accessToken = %q, want %q", tokens[0].accessToken, "at-primary")
	}
	if tokens[0].name != "Primary Account" {
		t.Fatalf("first name = %q, want %q", tokens[0].name, "Primary Account")
	}

	if tokens[1].accessToken != "at-email-fallback" || tokens[1].name != "fallback@example.com" {
		t.Fatalf("second token = %+v, want access token with email fallback", tokens[1])
	}

	if tokens[2].accessToken != "at-default-name" || tokens[2].name != "" {
		t.Fatalf("third token = %+v, want access token with empty name for default naming", tokens[2])
	}
}

func TestParseImportJSONTokensPreservesCPAFields(t *testing.T) {
	data := []byte(`{
		"type": "codex",
		"email": "cpa@example.com",
		"expired": "2026-04-25T12:00:00Z",
		"id_token": "id-cpa",
		"account_id": "acc-cpa",
		"access_token": "at-cpa",
		"refresh_token": "rt-cpa"
	}`)

	tokens, err := parseImportJSONTokens(data)
	if err != nil {
		t.Fatalf("parseImportJSONTokens returned error: %v", err)
	}
	if len(tokens) != 1 {
		t.Fatalf("tokens len = %d, want 1", len(tokens))
	}

	token := tokens[0]
	if token.refreshToken != "rt-cpa" || token.accessToken != "at-cpa" {
		t.Fatalf("token = %+v, want RT and AT preserved", token)
	}
	if token.email != "cpa@example.com" || token.name != "cpa@example.com" {
		t.Fatalf("identity = name:%q email:%q, want cpa@example.com", token.name, token.email)
	}
	if token.idToken != "id-cpa" || token.accountID != "acc-cpa" || token.expiresAt != "2026-04-25T12:00:00Z" {
		t.Fatalf("metadata = %+v, want CPA token metadata preserved", token)
	}
}

func TestParseImportJSONTokensReturnsNoTokensForValidUnsupportedJSON(t *testing.T) {
	data := []byte(`{"accounts":[{"credentials":{}}],"proxies":[{"proxy_key":"ignored"}]}`)

	tokens, err := parseImportJSONTokens(data)
	if err != nil {
		t.Fatalf("parseImportJSONTokens returned error: %v", err)
	}
	if len(tokens) != 0 {
		t.Fatalf("tokens len = %d, want 0", len(tokens))
	}
}

func TestParseImportJSONTokensRejectsInvalidJSON(t *testing.T) {
	if _, err := parseImportJSONTokens([]byte(`{"accounts":[}`)); err == nil {
		t.Fatal("expected invalid JSON error, got nil")
	}
}

func TestImportAccountsJSONReturnsExistingNoTokenMessageForUnsupportedJSON(t *testing.T) {
	gin.SetMode(gin.TestMode)

	req := newMultipartJSONRequest(t, "accounts.json", `{"accounts":[{"credentials":{}}]}`)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = req

	handler := &Handler{}
	handler.importAccountsJSON(ctx, "")

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}

	var payload map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := payload["error"]; got != "JSON 文件中未找到有效的 refresh_token 或 access_token" {
		t.Fatalf("error = %q, want %q", got, "JSON 文件中未找到有效的 refresh_token 或 access_token")
	}
}

func TestImportAccountsJSONRejectsInvalidJSONFile(t *testing.T) {
	gin.SetMode(gin.TestMode)

	req := newMultipartJSONRequest(t, "broken.json", `{"accounts":[}`)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = req

	handler := &Handler{}
	handler.importAccountsJSON(ctx, "")

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}

	var payload map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := payload["error"]; got != "文件 broken.json 不是有效的 JSON 格式" {
		t.Fatalf("error = %q, want %q", got, "文件 broken.json 不是有效的 JSON 格式")
	}
}

func newMultipartJSONRequest(t *testing.T, filename string, content string) *http.Request {
	t.Helper()

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	if _, err := part.Write([]byte(content)); err != nil {
		t.Fatalf("part.Write: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("writer.Close: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/admin/accounts/import", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	return req
}
