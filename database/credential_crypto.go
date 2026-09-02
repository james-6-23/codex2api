package database

// 账号凭据落库加密(可选,默认关闭)。
//
// 设计目标:把 credentials JSONB 里的敏感字段(access_token / refresh_token /
// session_token / api_key / id_token / agent_private_key / client_secret)在写库时
// 加密、读出时解密,而**不改动任何上层调用**,也不破坏平台既有的两类 SQL 依赖:
//   1. 调度 outbox 触发器按 OLD/NEW 的 access_token 等做**变更检测**;
//   2. 账号列表投影按 `<> ''` 做**存在性检查**。
// 为此采用**确定性 AEAD**(nonce 由 HMAC(key, field||plaintext) 派生):同一明文恒
// 得同一密文 → 变更检测语义不变;密文非空 → 存在性检查不变。
//
// 开关:环境变量 CODEX_CRED_ENCRYPTION_KEY。未设置时所有函数是 no-op,行为与不加密
// 完全一致(存量明文账号照常工作)。设置后:新写入的敏感字段加密,读取端透明解密;
// 存量明文行因无 enc: 前缀被原样返回,继续可用(渐进迁移,改写时自动转密文)。
//
// 注意:密钥一旦丢失,已加密的凭据无法解密,相关账号需重新导入——这是加密的固有代价。

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"os"
	"strings"
	"sync"
)

const credEncPrefix = "enc:v1:"

// sensitiveCredentialKeys 是需要加密的凭据字段。仅这些字段加密;upstream_type /
// email / plan_type / models 等参与 SQL 过滤的字段保持明文。
var sensitiveCredentialKeys = map[string]struct{}{
	"access_token":      {},
	"refresh_token":     {},
	"session_token":     {},
	"api_key":           {},
	"id_token":          {},
	"agent_private_key": {},
	"client_secret":     {},
}

var (
	credKeyOnce sync.Once
	credKey     []byte // 32 字节;nil 表示未启用
)

// credCipherKey 惰性读取并派生密钥(SHA-256(env 值)→ 32 字节)。未设置返回 nil。
func credCipherKey() []byte {
	credKeyOnce.Do(func() {
		if v := strings.TrimSpace(os.Getenv("CODEX_CRED_ENCRYPTION_KEY")); v != "" {
			sum := sha256.Sum256([]byte(v))
			credKey = sum[:]
		}
	})
	return credKey
}

// setCredEncryptionKeyForTest 仅供测试注入/清空密钥。
func setCredEncryptionKeyForTest(raw string) {
	credKeyOnce.Do(func() {}) // 标记 once 已触发,避免后续 env 覆盖
	if strings.TrimSpace(raw) == "" {
		credKey = nil
		return
	}
	sum := sha256.Sum256([]byte(raw))
	credKey = sum[:]
}

// encryptCredentialValue 加密单个字段值。已加密 / 空值 / 未启用时原样返回。
func encryptCredentialValue(field, plaintext string) string {
	key := credCipherKey()
	if key == nil || plaintext == "" || strings.HasPrefix(plaintext, credEncPrefix) {
		return plaintext
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return plaintext
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return plaintext
	}
	// 确定性 nonce:HMAC(key, field || 0x00 || plaintext) 截断到 nonce 长度。
	// 同明文恒得同 nonce/密文(保变更检测);不同明文几乎必得不同 nonce(GCM 安全)。
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(field))
	mac.Write([]byte{0})
	mac.Write([]byte(plaintext))
	nonce := mac.Sum(nil)[:gcm.NonceSize()]
	// AAD=field,把密文绑定到字段,防止跨字段搬运。
	ct := gcm.Seal(nil, nonce, []byte(plaintext), []byte(field))
	buf := make([]byte, 0, len(nonce)+len(ct))
	buf = append(buf, nonce...)
	buf = append(buf, ct...)
	return credEncPrefix + base64.RawURLEncoding.EncodeToString(buf)
}

// decryptCredentialValue 解密单个字段值。无前缀 / 未启用 / 解密失败时原样返回。
func decryptCredentialValue(field, value string) string {
	if !strings.HasPrefix(value, credEncPrefix) {
		return value
	}
	key := credCipherKey()
	if key == nil {
		return value
	}
	raw, err := base64.RawURLEncoding.DecodeString(value[len(credEncPrefix):])
	if err != nil {
		return value
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return value
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return value
	}
	if len(raw) < gcm.NonceSize() {
		return value
	}
	nonce, ct := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	pt, err := gcm.Open(nil, nonce, ct, []byte(field))
	if err != nil {
		return value
	}
	return string(pt)
}

// encryptSensitiveCredentials 返回一份浅拷贝,其中敏感字段被加密。未启用时原样返回入参。
// 在每个写库函数 marshal 之前调用。
func encryptSensitiveCredentials(m map[string]interface{}) map[string]interface{} {
	if credCipherKey() == nil || m == nil {
		return m
	}
	out := make(map[string]interface{}, len(m))
	for k, v := range m {
		if _, ok := sensitiveCredentialKeys[k]; ok {
			if s, isStr := v.(string); isStr {
				out[k] = encryptCredentialValue(k, s)
				continue
			}
		}
		out[k] = v
	}
	return out
}

// decryptSensitiveCredentialsInPlace 就地解密 map 里的敏感字段。在 decodeCredentials
// 里调用,使所有 Go 读取端(GetCredential / 各处 map 直读)统一见明文。
func decryptSensitiveCredentialsInPlace(m map[string]interface{}) {
	if credCipherKey() == nil || m == nil {
		return
	}
	for k, v := range m {
		if _, ok := sensitiveCredentialKeys[k]; !ok {
			continue
		}
		if s, isStr := v.(string); isStr {
			m[k] = decryptCredentialValue(k, s)
		}
	}
}
