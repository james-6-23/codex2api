# Codex2API 兼容层

本目录包含从 gpt2api 移植的核心功能，作为独立兼容层集成到 codex2api。

## 功能模块

### 1. 图片处理 (`image/`)

#### 2K/4K 本地放大
- **文件**: `upscale.go`
- **功能**: 使用 Catmull-Rom 插值算法将图片放大到 2K(2560px) 或 4K(3840px)
- **特性**:
  - 进程内 LRU 缓存(默认 512MB)
  - 并发控制(避免 CPU 打满)
  - 首次放大 0.5-2s，缓存命中毫秒级返回
  - 输出 PNG 格式(BestSpeed 压缩)

#### 缩略图生成
- **文件**: `thumb.go`
- **功能**: 将任意格式图片压缩为 JPEG 缩略图
- **特性**:
  - 多档降级策略(768px@78% → 192px@45%)
  - 目标体积控制(≤ 64KB)
  - 使用 ApproxBiLinear 算法(速度优先)

#### 配置管理
- **文件**: `config.go`
- **功能**: 统一配置管理和全局缓存初始化

## 使用方法

### 基础用法

```go
import "github.com/codex2api/compat/image"

// 初始化全局缓存(程序启动时调用一次)
func init() {
    cfg := &image.Config{
        EnableUpscale:      true,
        UpscaleCacheMB:     512,
        UpscaleConcurrency: 4,
    }
    image.InitGlobalCache(cfg)
}

// 放大图片
func upscaleImage(imageBytes []byte, scale string) ([]byte, error) {
    cache := image.GetGlobalCache()
    
    // 检查缓存
    cacheKey := fmt.Sprintf("task_%s_%s", taskID, scale)
    if data, ct, ok := cache.Get(cacheKey); ok {
        return data, nil
    }
    
    // 执行放大
    cache.Acquire()
    defer cache.Release()
    
    upscaled, ct, err := image.DoUpscale(imageBytes, scale)
    if err != nil {
        return nil, err
    }
    
    // 写入缓存
    cache.Put(cacheKey, upscaled, ct)
    return upscaled, nil
}

// 生成缩略图
func makeThumbnail(imageBytes []byte, budgetKB int) ([]byte, error) {
    thumb, ct, ok := image.MakeThumbnail(imageBytes, budgetKB)
    if !ok {
        return imageBytes, nil // 回落原图
    }
    return thumb, nil
}
```

### 集成到现有代码

#### 1. 在 `admin/image_studio.go` 中集成放大功能

```go
import compatImage "github.com/codex2api/compat/image"

// 生成图片后调用
func (h *Handler) saveImageJobAssets(...) {
    // ... 原有代码 ...
    
    // 如果请求了放大
    if upscale := req.Upscale; upscale != "" && compatImage.ValidateUpscale(upscale) != "" {
        cache := compatImage.GetGlobalCache()
        cacheKey := fmt.Sprintf("%d-%d-%s", jobID, idx, upscale)
        
        if cached, _, ok := cache.Get(cacheKey); ok {
            imageBytes = cached
        } else {
            cache.Acquire()
            upscaled, _, err := compatImage.DoUpscale(imageBytes, upscale)
            cache.Release()
            
            if err == nil && len(upscaled) > 0 {
                imageBytes = upscaled
                cache.Put(cacheKey, upscaled, "image/png")
            }
        }
    }
    
    // ... 保存文件 ...
}
```

#### 2. 添加配置支持

在 `config/config.go` 中添加:

```go
type Config struct {
    // ... 现有字段 ...
    
    // 图片兼容层配置
    ImageCompat struct {
        EnableUpscale      bool  `yaml:"enable_upscale"`
        UpscaleCacheMB     int64 `yaml:"upscale_cache_mb"`
        UpscaleConcurrency int   `yaml:"upscale_concurrency"`
    } `yaml:"image_compat"`
}
```

在 `config.yaml` 中添加:

```yaml
image_compat:
  enable_upscale: true
  upscale_cache_mb: 512
  upscale_concurrency: 4
```

## 配置说明

| 配置项 | 类型 | 默认值 | 说明 |
|--------|------|--------|------|
| `enable_upscale` | bool | true | 是否启用 2K/4K 放大功能 |
| `upscale_cache_mb` | int64 | 512 | LRU 缓存大小(MB)，512MB 约可缓存 50 张 4K PNG |
| `upscale_concurrency` | int | CPU 核心数 | 并发放大任务数，避免 CPU 打满 |
| `enable_thumbnail` | bool | true | 是否启用缩略图功能 |

## 性能指标

### 放大性能
- **首次放大**: 0.5-2s (取决于原图大小和目标分辨率)
- **缓存命中**: < 10ms
- **内存占用**: 512MB 缓存 + 临时解码内存
- **CPU 占用**: 受 `upscale_concurrency` 限制

### 缩略图性能
- **生成时间**: 50-200ms (取决于原图大小)
- **输出体积**: ≤ 64KB (JPEG)
- **算法**: ApproxBiLinear (速度优先)

## 依赖

```go
require (
    golang.org/x/image v0.39.0
)
```

## 测试

```bash
# 运行测试
cd compat/image
go test -v

# 性能测试
go test -bench=. -benchmem
```

## 注意事项

1. **内存管理**: 放大 4K 图片会临时占用约 60-100MB 内存，请确保服务器有足够内存
2. **并发控制**: 默认并发数为 CPU 核心数，生产环境建议设置为核心数的 50-75%
3. **缓存策略**: LRU 缓存按访问次数淘汰，适合"刚生成+回头再看"的场景
4. **格式支持**: 输入支持 PNG/JPEG/GIF/WEBP，输出固定为 PNG(放大)或 JPEG(缩略图)

## 后续计划

- [ ] 阶段 2: HMAC 防盗链代理
- [ ] 阶段 3: SSE 直出优化(可选)
- [ ] 阶段 4: RT/ST 双路径刷新(可选)

## 许可证

本兼容层代码移植自 [gpt2api](https://github.com/432539/gpt2api)，遵循原项目许可证。
