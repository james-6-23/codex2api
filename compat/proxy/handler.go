package proxy

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

// AssetResolver 按资产 ID 解析图片信息
type AssetResolver interface {
	GetAssetPath(ctx context.Context, assetID int64) (storagePath string, mimeType string, err error)
}

// ProxyHandler 图片代理处理器
type ProxyHandler struct {
	AssetResolver AssetResolver
}

// NewProxyHandler 创建代理处理器
func NewProxyHandler(resolver AssetResolver) *ProxyHandler {
	return &ProxyHandler{
		AssetResolver: resolver,
	}
}

// HandleImageProxy 处理图片代理请求
// GET /p/img/:asset_id?exp=<unix_ms>&sig=<hex>
func (h *ProxyHandler) HandleImageProxy(c *gin.Context) {
	assetIDStr := c.Param("asset_id")
	expStr := c.Query("exp")
	sig := c.Query("sig")

	// 验证参数
	if assetIDStr == "" || expStr == "" || sig == "" {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	assetID, err := strconv.ParseInt(assetIDStr, 10, 64)
	if err != nil || assetID <= 0 {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	expMs, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}

	// 验证签名
	if !VerifyImgSig(assetID, expMs, sig) {
		log.Printf("[proxy] 签名验证失败: asset_id=%d", assetID)
		c.AbortWithStatus(http.StatusForbidden)
		return
	}

	// 解析资产路径
	if h.AssetResolver == nil {
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	storagePath, mimeType, err := h.AssetResolver.GetAssetPath(ctx, assetID)
	if err != nil {
		log.Printf("[proxy] 获取资产路径失败: asset_id=%d err=%v", assetID, err)
		c.AbortWithStatus(http.StatusNotFound)
		return
	}

	// 读取文件
	file, err := http.Dir(".").Open(storagePath)
	if err != nil {
		log.Printf("[proxy] 打开文件失败: path=%s err=%v", storagePath, err)
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	defer file.Close()

	// 读取文件内容
	data, err := io.ReadAll(file)
	if err != nil {
		log.Printf("[proxy] 读取文件失败: path=%s err=%v", storagePath, err)
		c.AbortWithStatus(http.StatusInternalServerError)
		return
	}

	// 设置缓存头
	c.Header("Cache-Control", "private, max-age=86400")

	// 返回图片
	if mimeType == "" {
		mimeType = "image/png"
	}
	c.Data(http.StatusOK, mimeType, data)
}
