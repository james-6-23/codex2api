package proxy

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"
)

// imageProxySecret 进程级随机密钥，用于 HMAC 签名图片 URL
// 进程重启后旧的签名 URL 全部失效（防止长期有效的 URL 泄漏）
var imageProxySecret []byte

func init() {
	imageProxySecret = make([]byte, 32)
	if _, err := rand.Read(imageProxySecret); err != nil {
		// crypto/rand 失败是严重的系统问题，不应降级到不安全的密钥
		// 直接 panic 以避免生成可预测的签名
		panic(fmt.Sprintf("failed to generate secure random key for image proxy: %v", err))
	}
}

// ImageProxyTTL 单条签名 URL 的默认有效期（24小时）
const ImageProxyTTL = 24 * time.Hour

// BuildImageProxyURL 生成代理 URL
// 返回绝对 path（不含 host），前端可以直接使用
func BuildImageProxyURL(assetID int64, ttl time.Duration) string {
	if ttl <= 0 {
		ttl = ImageProxyTTL
	}
	expMs := time.Now().Add(ttl).UnixMilli()
	sig := computeImgSig(assetID, expMs)
	return fmt.Sprintf("/p/img/%d?exp=%d&sig=%s", assetID, expMs, sig)
}

// computeImgSig 计算 HMAC-SHA256 签名
func computeImgSig(assetID int64, expMs int64) string {
	mac := hmac.New(sha256.New, imageProxySecret)
	fmt.Fprintf(mac, "%d|%d", assetID, expMs)
	return hex.EncodeToString(mac.Sum(nil))[:24]
}

// VerifyImgSig 验证签名是否有效
func VerifyImgSig(assetID int64, expMs int64, sig string) bool {
	// 检查是否过期
	if expMs < time.Now().UnixMilli() {
		return false
	}
	// 验证签名
	want := computeImgSig(assetID, expMs)
	return hmac.Equal([]byte(sig), []byte(want))
}
