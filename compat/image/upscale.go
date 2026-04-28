package image

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	gimage "image"
	"image/png"
	"runtime"
	"sync"

	_ "image/gif"
	_ "image/jpeg"

	"golang.org/x/image/draw"

	_ "golang.org/x/image/webp" // 上游 CDN 偶发 webp,标准库解不了,这里 blank import 注册
)

// Upscale 档位 —— 对外只暴露三档:空字符串 / "2k" / "4k"。
//
// 采用"长边目标像素"策略(避免不同比例出图的长边被裁),短边按原比例等比缩放:
//   - 2K:长边 2560
//   - 4K:长边 3840
//
// 算法固定为 Catmull-Rom(biquintic),本地 CPU 运算,不需要模型 / 不调用任何外部服务。
// 这是"传统插值放大",不是 AI 超分 —— 只会更平滑,不会补出新的毛发 / 纹理 / 细节。
const (
	UpscaleNone = ""
	Upscale2K   = "2k"
	Upscale4K   = "4k"
)

// ValidateUpscale 规整前端 / 上游传入的档位字符串,非法值一律视为空(原图)。
func ValidateUpscale(s string) string {
	switch s {
	case Upscale2K, Upscale4K:
		return s
	default:
		return UpscaleNone
	}
}

// longSideOf 返回档位对应的"长边目标像素"。0 表示不放大。
func longSideOf(scale string) int {
	switch scale {
	case Upscale2K:
		return 2560
	case Upscale4K:
		return 3840
	default:
		return 0
	}
}

// ErrUpscaleDecode 当原始字节解码失败(既不是主流 PNG/JPEG/GIF 也不是 webp)时返回。
var ErrUpscaleDecode = errors.New("image upscale: decode source failed")

// ComputeCacheKey 计算图片内容的缓存键（SHA256 哈希 + 放大档位）
func ComputeCacheKey(src []byte, scale string) string {
	hash := sha256.Sum256(src)
	return hex.EncodeToString(hash[:16]) + "-" + scale // 使用前 16 字节（32 字符）+ 档位
}

// DoUpscale 对给定字节做 Catmull-Rom 放大并重新编码为 PNG。
//
//   - 输入:src 任意主流位图字节(PNG / JPEG / GIF / WEBP)。
//   - scale:"" 表示直接原样返回(零开销);"2k" / "4k" 会做实际放大。
//   - 若原图长边已经 ≥ 目标长边,直接返回原字节(不放大,不重编码,避免白白损失 JPEG 细节)。
//
// 返回 (输出字节, 输出 content-type, error)。输出 content-type 在做了实际放大时固定为 image/png。
func DoUpscale(src []byte, scale string) ([]byte, string, error) {
	scale = ValidateUpscale(scale)
	target := longSideOf(scale)
	if target == 0 || len(src) == 0 {
		return src, "", nil
	}

	// image.Decode 已经注册了 png/jpeg/gif(上面 blank import),webp 来自 x/image/webp。
	srcImg, _, err := gimage.Decode(bytes.NewReader(src))
	if err != nil {
		return nil, "", fmt.Errorf("%w: %v", ErrUpscaleDecode, err)
	}

	b := srcImg.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw <= 0 || sh <= 0 {
		return nil, "", ErrUpscaleDecode
	}

	// 原图长边已经 ≥ 目标,放弃放大以免重复损失质量
	long := sw
	if sh > long {
		long = sh
	}
	if long >= target {
		return src, "", nil
	}

	// 等比缩,长边对齐 target
	var dw, dh int
	if sw >= sh {
		dw = target
		dh = int(float64(sh) * float64(target) / float64(sw))
		if dh < 1 {
			dh = 1
		}
	} else {
		dh = target
		dw = int(float64(sw) * float64(target) / float64(sh))
		if dw < 1 {
			dw = 1
		}
	}

	dst := gimage.NewRGBA(gimage.Rect(0, 0, dw, dh))
	draw.CatmullRom.Scale(dst, dst.Bounds(), srcImg, b, draw.Src, nil)

	var buf bytes.Buffer
	// BestSpeed 显著快于默认 DefaultCompression(4K 下大约 3~5x),
	// 文件体积多约 15~25%,对 PNG 4K 出图场景来说值得:交互感优先。
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&buf, dst); err != nil {
		return nil, "", fmt.Errorf("image upscale: png encode: %w", err)
	}
	return buf.Bytes(), "image/png", nil
}

// ---------------- 并发闸 + LRU 缓存 ----------------

// UpscaleCache 进程内 LRU 字节缓存,附带一个并发信号量限制同时计算的数量。
//
// 设计动机:
//   - 同一张图第一次按 scale=4k 请求时需要跑 decode + Catmull-Rom + png encode,
//     合计约 0.5~2s;命中缓存后毫秒级返回,交互体验差异巨大。
//   - 并发闸避免 4K 请求风暴把 CPU 打满,影响生图主流程。
type UpscaleCache struct {
	mu       sync.Mutex
	items    map[string]*cacheEntry
	maxBytes int64
	curBytes int64

	sem chan struct{}
}

type cacheEntry struct {
	key         string
	data        []byte
	contentType string
	accessCount int64
}

// NewUpscaleCache 初始化 LRU。maxBytes ≤ 0 时使用默认 512MB;并发上限默认 NumCPU。
//
// 默认 512MB 足够放 ~50 张 4K PNG,对面板"刚生成 + 回头再看"的场景命中率很高。
func NewUpscaleCache(maxBytes int64, concurrency int) *UpscaleCache {
	if maxBytes <= 0 {
		maxBytes = 512 * 1024 * 1024
	}
	if concurrency <= 0 {
		concurrency = runtime.NumCPU()
		if concurrency < 2 {
			concurrency = 2
		}
	}
	return &UpscaleCache{
		items:    make(map[string]*cacheEntry),
		maxBytes: maxBytes,
		sem:      make(chan struct{}, concurrency),
	}
}

// Get 命中时返回 (data, ct, true);未命中返回 false。
func (c *UpscaleCache) Get(key string) ([]byte, string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.items[key]
	if !ok {
		return nil, "", false
	}
	entry.accessCount++
	return entry.data, entry.contentType, true
}

// Put 写入缓存,超过容量时淘汰访问次数最少的条目。
func (c *UpscaleCache) Put(key string, data []byte, contentType string) {
	if len(data) == 0 || int64(len(data)) > c.maxBytes {
		return // 单条就能撑爆缓存的直接放弃
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if entry, ok := c.items[key]; ok {
		c.curBytes -= int64(len(entry.data))
		entry.data = data
		entry.contentType = contentType
		c.curBytes += int64(len(data))
		return
	}

	entry := &cacheEntry{key: key, data: data, contentType: contentType, accessCount: 0}
	c.items[key] = entry
	c.curBytes += int64(len(data))

	// 超过容量时淘汰访问次数最少的
	for c.curBytes > c.maxBytes {
		var minKey string
		var minCount int64 = -1
		for k, e := range c.items {
			if minCount == -1 || e.accessCount < minCount {
				minKey = k
				minCount = e.accessCount
			}
		}
		if minKey == "" {
			break
		}
		old := c.items[minKey]
		delete(c.items, minKey)
		c.curBytes -= int64(len(old.data))
	}
}

// Acquire 占用一格并发配额;请与 Release 成对使用。
func (c *UpscaleCache) Acquire() { c.sem <- struct{}{} }

// Release 释放一格。
func (c *UpscaleCache) Release() { <-c.sem }
