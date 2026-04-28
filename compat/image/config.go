package image

import "sync"

// Config 图片兼容层配置
type Config struct {
	// EnableUpscale 是否启用 2K/4K 本地放大功能
	EnableUpscale bool `json:"enable_upscale" yaml:"enable_upscale"`

	// UpscaleCacheMB 放大缓存大小(MB),默认 512MB
	UpscaleCacheMB int64 `json:"upscale_cache_mb" yaml:"upscale_cache_mb"`

	// UpscaleConcurrency 并发放大任务数,默认为 CPU 核心数
	UpscaleConcurrency int `json:"upscale_concurrency" yaml:"upscale_concurrency"`

	// EnableThumbnail 是否启用缩略图功能
	EnableThumbnail bool `json:"enable_thumbnail" yaml:"enable_thumbnail"`
}

// DefaultConfig 返回默认配置
func DefaultConfig() *Config {
	return &Config{
		EnableUpscale:      true,
		UpscaleCacheMB:     512,
		UpscaleConcurrency: 0, // 0 表示使用 CPU 核心数
		EnableThumbnail:    true,
	}
}

// globalCache 全局放大缓存实例
var (
	globalCache     *UpscaleCache
	globalCacheOnce sync.Once
)

// InitGlobalCache 初始化全局缓存(应在程序启动时调用一次)
func InitGlobalCache(cfg *Config) {
	if cfg == nil {
		cfg = DefaultConfig()
	}
	globalCacheOnce.Do(func() {
		cacheMB := cfg.UpscaleCacheMB
		if cacheMB <= 0 {
			cacheMB = 512
		}
		globalCache = NewUpscaleCache(cacheMB*1024*1024, cfg.UpscaleConcurrency)
	})
}

// GetGlobalCache 获取全局缓存实例
func GetGlobalCache() *UpscaleCache {
	if globalCache == nil {
		InitGlobalCache(nil)
	}
	return globalCache
}
