package compat

import (
	"log"

	compatImage "github.com/codex2api/compat/image"
)

// InitCompat 初始化兼容层（在 main.go 中调用）
func InitCompat(enableUpscale bool, cacheMB int64, concurrency int) {
	if !enableUpscale {
		log.Println("[compat] 图片放大功能已禁用")
		return
	}

	compatImage.InitGlobalCache(&compatImage.Config{
		EnableUpscale:      enableUpscale,
		UpscaleCacheMB:     cacheMB,
		UpscaleConcurrency: concurrency,
		EnableThumbnail:    true,
	})

	log.Printf("[compat] 图片兼容层已初始化: cache=%dMB concurrency=%d", cacheMB, concurrency)
}
