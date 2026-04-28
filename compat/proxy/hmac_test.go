package proxy

import (
	"testing"
	"time"
)

func TestComputeImgSig(t *testing.T) {
	assetID := int64(12345)
	expMs := time.Now().Add(24 * time.Hour).UnixMilli()

	sig1 := computeImgSig(assetID, expMs)
	sig2 := computeImgSig(assetID, expMs)

	if sig1 != sig2 {
		t.Errorf("相同参数应生成相同签名: sig1=%s sig2=%s", sig1, sig2)
	}

	if len(sig1) != 24 {
		t.Errorf("签名长度应为 24: got %d", len(sig1))
	}
}

func TestVerifyImgSig(t *testing.T) {
	assetID := int64(12345)
	expMs := time.Now().Add(24 * time.Hour).UnixMilli()
	sig := computeImgSig(assetID, expMs)

	// 测试有效签名
	if !VerifyImgSig(assetID, expMs, sig) {
		t.Error("有效签名应通过验证")
	}

	// 测试错误的 assetID
	if VerifyImgSig(assetID+1, expMs, sig) {
		t.Error("错误的 assetID 不应通过验证")
	}

	// 测试错误的 expMs
	if VerifyImgSig(assetID, expMs+1000, sig) {
		t.Error("错误的 expMs 不应通过验证")
	}

	// 测试错误的签名
	if VerifyImgSig(assetID, expMs, "invalid_signature") {
		t.Error("错误的签名不应通过验证")
	}

	// 测试过期签名
	expiredMs := time.Now().Add(-1 * time.Hour).UnixMilli()
	expiredSig := computeImgSig(assetID, expiredMs)
	if VerifyImgSig(assetID, expiredMs, expiredSig) {
		t.Error("过期签名不应通过验证")
	}
}

func TestBuildImageProxyURL(t *testing.T) {
	assetID := int64(12345)
	url := BuildImageProxyURL(assetID, 24*time.Hour)

	if url == "" {
		t.Error("生成的 URL 不应为空")
	}

	// 验证 URL 格式
	expectedPrefix := "/p/img/12345?exp="
	if len(url) < len(expectedPrefix) {
		t.Errorf("URL 格式错误: %s", url)
	}

	if url[:len(expectedPrefix)] != expectedPrefix {
		t.Errorf("URL 前缀错误: expected %s, got %s", expectedPrefix, url[:len(expectedPrefix)])
	}
}
