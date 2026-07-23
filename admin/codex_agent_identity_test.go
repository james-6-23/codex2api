package admin

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"strings"
	"testing"
)

// newTestAgentPrivateKey 生成一把合法的 PKCS8 Ed25519 私钥（base64），供解析测试使用。
func newTestAgentPrivateKey(t *testing.T) string {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("生成 Ed25519 key 失败: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatalf("PKCS8 编码失败: %v", err)
	}
	return base64.StdEncoding.EncodeToString(der)
}

func TestParseAgentIdentityAuthJSON_Forms(t *testing.T) {
	pk := newTestAgentPrivateKey(t)

	cases := []struct {
		name    string
		json    string
		wantErr bool
	}{
		{
			name: "credentials wrapper flat fields (issue #424)",
			json: `{"credentials":{"account_id":"acc-1","agent_private_key":"` + pk + `","agent_runtime_id":"rt-1","auth_mode":"agentIdentity","chatgpt_account_is_fedramp":false,"chatgpt_user_id":"user-1","email":"a@b.com"}}`,
		},
		{
			name: "flat root with auth_mode",
			json: `{"auth_mode":"agentIdentity","agent_runtime_id":"rt-1","agent_private_key":"` + pk + `","account_id":"acc-1","chatgpt_user_id":"user-1"}`,
		},
		{
			name: "agent_identity sub-object",
			json: `{"agent_identity":{"agent_runtime_id":"rt-1","agent_private_key":"` + pk + `","account_id":"acc-1","chatgpt_user_id":"user-1"}}`,
		},
		{
			name: "credentials wrapping agent_identity sub-object",
			json: `{"credentials":{"agent_identity":{"agent_runtime_id":"rt-1","agent_private_key":"` + pk + `","account_id":"acc-1","chatgpt_user_id":"user-1"}}}`,
		},
		{
			name:    "plain oauth credentials are not agent identity",
			json:    `{"credentials":{"refresh_token":"rt","access_token":"at","account_id":"acc-1"}}`,
			wantErr: true,
		},
		{
			name:    "missing required fields",
			json:    `{"credentials":{"auth_mode":"agentIdentity","agent_runtime_id":"rt-1"}}`,
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fields, err := parseAgentIdentityAuthJSON(tc.json)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got fields=%+v", fields)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if fields.RuntimeID != "rt-1" || fields.AccountID != "acc-1" || fields.UserID != "user-1" {
				t.Fatalf("parsed fields mismatch: %+v", fields)
			}
			if fields.PrivateKey != pk {
				t.Fatalf("private key not extracted")
			}
		})
	}
}

func TestParseAgentIdentityAuthJSON_WrapperMissingFieldsReportsFieldError(t *testing.T) {
	// credentials 包装被识别为 Agent Identity 后，缺字段应报"缺少必要字段"，
	// 而不是"不是 Agent Identity 格式"——确认穿透生效。
	_, err := parseAgentIdentityAuthJSON(`{"credentials":{"auth_mode":"agentIdentity","agent_runtime_id":"rt-1"}}`)
	if err == nil {
		t.Fatal("expected missing-field error")
	}
	if strings.Contains(err.Error(), "不是 Agent Identity 格式") {
		t.Fatalf("wrapper should be detected as agent identity, got: %v", err)
	}
}
