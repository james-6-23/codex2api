package admin

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"strings"
	"sync"
	"testing"

	"github.com/codex2api/auth"
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
			json: `{"credentials":{"account_id":"acc-1","agent_private_key":"` + pk + `","agent_runtime_id":"agent-rt-1","auth_mode":"agentIdentity","chatgpt_account_is_fedramp":false,"chatgpt_user_id":"user-1","email":"a@b.com"}}`,
		},
		{
			name: "flat root with auth_mode",
			json: `{"auth_mode":"agentIdentity","agent_runtime_id":"agent-rt-1","agent_private_key":"` + pk + `","account_id":"acc-1","chatgpt_user_id":"user-1"}`,
		},
		{
			name: "agent_identity sub-object",
			json: `{"agent_identity":{"agent_runtime_id":"agent-rt-1","agent_private_key":"` + pk + `","account_id":"acc-1","chatgpt_user_id":"user-1"}}`,
		},
		{
			name: "credentials wrapping agent_identity sub-object",
			json: `{"credentials":{"agent_identity":{"agent_runtime_id":"agent-rt-1","agent_private_key":"` + pk + `","account_id":"acc-1","chatgpt_user_id":"user-1"}}}`,
		},
		{
			name:    "plain oauth credentials are not agent identity",
			json:    `{"credentials":{"refresh_token":"rt","access_token":"at","account_id":"acc-1"}}`,
			wantErr: true,
		},
		{
			name:    "missing required fields",
			json:    `{"credentials":{"auth_mode":"agentIdentity","agent_runtime_id":"agent-rt-1"}}`,
			wantErr: true,
		},
		{
			name:    "runtime id without agent prefix",
			json:    `{"agent_identity":{"agent_runtime_id":"rt-1","agent_private_key":"` + pk + `","account_id":"acc-1","chatgpt_user_id":"user-1"}}`,
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
			if fields.RuntimeID != "agent-rt-1" || fields.AccountID != "acc-1" || fields.UserID != "user-1" {
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
	_, err := parseAgentIdentityAuthJSON(`{"credentials":{"auth_mode":"agentIdentity","agent_runtime_id":"agent-rt-1"}}`)
	if err == nil {
		t.Fatal("expected missing-field error")
	}
	if strings.Contains(err.Error(), "不是 Agent Identity 格式") {
		t.Fatalf("wrapper should be detected as agent identity, got: %v", err)
	}
}

func TestImportAgentIdentityTokensRejectsMissingRequiredFields(t *testing.T) {
	handler := &Handler{}
	success, duplicate, failed := handler.importAgentIdentityTokens(context.Background(), []importToken{{
		agentRuntimeID:  "agent-runtime-1",
		agentPrivateKey: newTestAgentPrivateKey(t),
	}}, "", false)
	if success != 0 || duplicate != 0 || failed != 1 {
		t.Fatalf("counts = success:%d duplicate:%d failed:%d, want 0/0/1", success, duplicate, failed)
	}
}

func TestCreateAgentIdentityAccountIfAbsentChecksDatabase(t *testing.T) {
	db := newTestAdminDB(t)
	privateKey := newTestAgentPrivateKey(t)
	_, err := db.InsertAccountWithCredentials(context.Background(), "existing-agent", map[string]interface{}{
		"auth_mode":         auth.CodexAuthModeAgentIdentity,
		"agent_runtime_id":  "agent-existing",
		"agent_private_key": privateKey,
		"account_id":        "acc-existing",
		"chatgpt_user_id":   "user-existing",
	}, "")
	if err != nil {
		t.Fatalf("insert existing account: %v", err)
	}

	store := auth.NewStore(db, nil, nil)
	t.Cleanup(store.Stop)
	handler := &Handler{db: db, store: store}
	_, duplicate, err := handler.createAgentIdentityAccountIfAbsent(context.Background(), &agentIdentityFields{
		RuntimeID:  "agent-existing",
		PrivateKey: privateKey,
		AccountID:  "acc-new",
		UserID:     "user-new",
	}, "new-agent", "", "test")
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	if !duplicate {
		t.Fatal("database-only existing runtime should be detected as duplicate")
	}
	rows, err := db.ListActive(context.Background())
	if err != nil {
		t.Fatalf("list active accounts: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("active accounts = %d, want 1", len(rows))
	}
}

func TestCreateAgentIdentityAccountIfAbsentSerializesConcurrentImports(t *testing.T) {
	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, nil)
	t.Cleanup(store.Stop)
	handler := &Handler{
		db:    db,
		store: store,
		probeUsage: func(context.Context, *auth.Account) error {
			return nil
		},
	}
	fields := &agentIdentityFields{
		RuntimeID:  "agent-concurrent",
		PrivateKey: newTestAgentPrivateKey(t),
		AccountID:  "acc-concurrent",
		UserID:     "user-concurrent",
	}

	const workers = 8
	type result struct {
		duplicate bool
		err       error
	}
	results := make(chan result, workers)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, duplicate, err := handler.createAgentIdentityAccountIfAbsent(context.Background(), fields, "concurrent-agent", "", "test")
			results <- result{duplicate: duplicate, err: err}
		}()
	}
	wg.Wait()
	close(results)

	created := 0
	duplicates := 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent create: %v", result.err)
		}
		if result.duplicate {
			duplicates++
		} else {
			created++
		}
	}
	if created != 1 || duplicates != workers-1 {
		t.Fatalf("created=%d duplicates=%d, want 1/%d", created, duplicates, workers-1)
	}
	rows, err := db.ListActive(context.Background())
	if err != nil {
		t.Fatalf("list active accounts: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("active accounts = %d, want 1", len(rows))
	}
}
