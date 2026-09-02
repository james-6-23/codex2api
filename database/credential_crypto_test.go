package database

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

func TestCredentialCrypto_RoundTrip(t *testing.T) {
	setCredEncryptionKeyForTest("test-master-key-123")
	defer setCredEncryptionKeyForTest("")

	m := map[string]interface{}{
		"upstream_type": "claude",
		"access_token":  "sk-at-secret",
		"refresh_token": "rt-secret",
		"email":         "user@example.com",
		"plan_type":     "claude",
	}
	enc := encryptSensitiveCredentials(m)
	// 敏感字段应被加密(带前缀),非敏感字段原样。
	if !strings.HasPrefix(enc["access_token"].(string), credEncPrefix) {
		t.Fatalf("access_token 未加密: %v", enc["access_token"])
	}
	if !strings.HasPrefix(enc["refresh_token"].(string), credEncPrefix) {
		t.Fatalf("refresh_token 未加密")
	}
	if enc["email"] != "user@example.com" || enc["upstream_type"] != "claude" {
		t.Fatal("非敏感字段不应改动")
	}
	// 原 map 不应被 mutate(返回副本)。
	if strings.HasPrefix(m["access_token"].(string), credEncPrefix) {
		t.Fatal("encryptSensitiveCredentials 不应 mutate 入参")
	}

	// 模拟落库→读出:marshal(enc) 再 decodeCredentials 应还原明文。
	raw, _ := json.Marshal(enc)
	decoded := decodeCredentials(raw)
	if decoded["access_token"] != "sk-at-secret" || decoded["refresh_token"] != "rt-secret" {
		t.Fatalf("解密还原失败: at=%v rt=%v", decoded["access_token"], decoded["refresh_token"])
	}
}

func TestCredentialCrypto_Deterministic(t *testing.T) {
	setCredEncryptionKeyForTest("k")
	defer setCredEncryptionKeyForTest("")
	// 同明文两次加密应得同密文(保 outbox 变更检测语义)。
	a := encryptCredentialValue("access_token", "same-token")
	b := encryptCredentialValue("access_token", "same-token")
	if a != b {
		t.Fatalf("确定性加密应产生相同密文: %s vs %s", a, b)
	}
	// 不同明文应得不同密文。
	c := encryptCredentialValue("access_token", "other-token")
	if a == c {
		t.Fatal("不同明文不应同密文")
	}
	// 不同字段(AAD)同明文应得不同密文。
	d := encryptCredentialValue("refresh_token", "same-token")
	if a == d {
		t.Fatal("不同字段应绑定不同密文")
	}
}

func TestCredentialCrypto_Disabled_NoOp(t *testing.T) {
	setCredEncryptionKeyForTest("") // 未启用
	defer setCredEncryptionKeyForTest("")
	m := map[string]interface{}{"access_token": "plain", "refresh_token": "plain2"}
	enc := encryptSensitiveCredentials(m)
	if enc["access_token"] != "plain" {
		t.Fatal("未启用时应原样返回(no-op)")
	}
	raw, _ := json.Marshal(enc)
	decoded := decodeCredentials(raw)
	if decoded["access_token"] != "plain" {
		t.Fatal("未启用时解密应原样")
	}
}

func TestCredentialCrypto_BackwardCompat_PlaintextRows(t *testing.T) {
	// 存量明文行:即使启用密钥,无 enc: 前缀的值应原样读出(渐进迁移)。
	setCredEncryptionKeyForTest("k")
	defer setCredEncryptionKeyForTest("")
	raw := []byte(`{"access_token":"legacy-plain","refresh_token":"legacy-rt","upstream_type":"codex"}`)
	decoded := decodeCredentials(raw)
	if decoded["access_token"] != "legacy-plain" || decoded["refresh_token"] != "legacy-rt" {
		t.Fatalf("存量明文应原样读出: %v", decoded)
	}
}

func TestCredentialCrypto_WrongKey_FailsClosed(t *testing.T) {
	setCredEncryptionKeyForTest("key-A")
	enc := encryptCredentialValue("access_token", "secret")
	// 换密钥后解密失败,返回原密文(而非明文),账号需重导——不误当明文用。
	setCredEncryptionKeyForTest("key-B")
	defer setCredEncryptionKeyForTest("")
	got := decryptCredentialValue("access_token", enc)
	if got == "secret" {
		t.Fatal("错误密钥不应解出明文")
	}
	if !strings.HasPrefix(got, credEncPrefix) {
		t.Fatal("解密失败应返回原密文")
	}
}

func TestCredentialCrypto_DBRoundTrip_AtRestEncrypted(t *testing.T) {
	setCredEncryptionKeyForTest("db-master-key")
	defer setCredEncryptionKeyForTest("")

	db, err := New("sqlite", filepath.Join(t.TempDir(), "cred-crypto.db"))
	if err != nil {
		t.Fatalf("New sqlite: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	id, err := db.InsertAccountWithUpstream(ctx, "claude", "anthropic", "oauth", map[string]interface{}{
		"upstream_type": "claude",
		"access_token":  "at-plain-secret",
		"refresh_token": "rt-plain-secret",
		"email":         "u@example.com",
	}, "")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	// 读回:GetCredential 应见明文。
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if row.GetCredential("access_token") != "at-plain-secret" || row.GetCredential("refresh_token") != "rt-plain-secret" {
		t.Fatalf("读回应为明文: at=%q rt=%q", row.GetCredential("access_token"), row.GetCredential("refresh_token"))
	}

	// 底层存储应为密文(enc: 前缀)。
	var rawCred string
	if err := db.conn.QueryRowContext(ctx, "SELECT credentials FROM accounts WHERE id = ?", id).Scan(&rawCred); err != nil {
		t.Fatalf("raw select: %v", err)
	}
	if strings.Contains(rawCred, "at-plain-secret") || strings.Contains(rawCred, "rt-plain-secret") {
		t.Fatalf("底层不应含明文 token: %s", rawCred)
	}
	if !strings.Contains(rawCred, credEncPrefix) {
		t.Fatalf("底层应为密文(含 %s 前缀): %s", credEncPrefix, rawCred)
	}
	// email(非敏感)应仍是明文,供 SQL 过滤。
	if !strings.Contains(rawCred, "u@example.com") {
		t.Fatalf("非敏感字段应保持明文: %s", rawCred)
	}

	// UpdateCredentials 往返:刷新 token 后读回仍明文。
	if err := db.UpdateCredentials(ctx, id, map[string]interface{}{"access_token": "at-refreshed"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	row2, _ := db.GetAccountByID(ctx, id)
	if row2.GetCredential("access_token") != "at-refreshed" {
		t.Fatalf("更新后读回应为新明文, got %q", row2.GetCredential("access_token"))
	}
}
