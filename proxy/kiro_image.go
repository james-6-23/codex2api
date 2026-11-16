package proxy

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/color"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"os"
	"strconv"
	"strings"

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// 入站图片按需缩放 / 重编码，降低 AWS Q 单字段大小限制导致的 400。规则移植自
// kiro.rs src/image_resize.rs；GIF 可能是动图，原样透传。

// kiroImageMaxDecodeBytes 对齐 kiro.rs 所用 image crate 的默认解码上限（Limits::max_alloc = 512 MiB）：
// 解码需要的内存超过它时不解码、原样透传，避免高压缩比的超大图片耗尽内存。
const kiroImageMaxDecodeBytes = 512 << 20

type kiroImageResizeConfig struct {
	enabled     bool
	maxLongSide int
	maxBytes    int
	jpegQuality int
}

func kiroImageResizeConfigFromEnv() kiroImageResizeConfig {
	cfg := kiroImageResizeConfig{enabled: true, maxLongSide: 1568, maxBytes: 400_000, jpegQuality: 85}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("KIRO_IMAGE_RESIZE"))) {
	case "0", "false", "no", "off":
		cfg.enabled = false
	}
	for name, target := range map[string]*int{"KIRO_IMAGE_MAX_LONG_SIDE": &cfg.maxLongSide, "KIRO_IMAGE_MAX_BYTES": &cfg.maxBytes, "KIRO_IMAGE_JPEG_QUALITY": &cfg.jpegQuality} {
		if value, err := strconv.Atoi(strings.TrimSpace(os.Getenv(name))); err == nil && value > 0 {
			*target = value
		}
	}
	return cfg
}

// kiroShrinkImage 返回 (format, base64)；失败时原样透传并按魔数修正声明的格式。
func kiroShrinkImage(format, data string) (string, string) {
	cfg := kiroImageResizeConfigFromEnv()
	format = strings.ToLower(format)
	if !cfg.enabled || format == "gif" {
		return kiroDetectImageFormat(format, data), data
	}
	raw, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return format, data
	}
	if len(data) <= cfg.maxBytes {
		if config, _, err := image.DecodeConfig(bytes.NewReader(raw)); err == nil && max(config.Width, config.Height) <= cfg.maxLongSide {
			return kiroDetectImageFormat(format, data), data
		}
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || !kiroImageDecodeWithinBudget(config.Width, config.Height) {
		return kiroDetectImageFormat(format, data), data
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return kiroDetectImageFormat(format, data), data
	}
	width, height := img.Bounds().Dx(), img.Bounds().Dy()
	longSide := max(min(max(width, height), cfg.maxLongSide), 1)
	const minQuality, minLongSide = 35, 256
	var out []byte
	for {
		resized := img
		if max(width, height) > longSide {
			scale := float64(longSide) / float64(max(width, height))
			target := image.NewRGBA(image.Rect(0, 0, max(int(float64(width)*scale+0.5), 1), max(int(float64(height)*scale+0.5), 1)))
			draw.CatmullRom.Scale(target, target.Bounds(), img, img.Bounds(), draw.Src, nil)
			resized = target
		}
		// JPEG 没有透明通道：先铺白底，避免透明区域变黑。
		flat := image.NewRGBA(resized.Bounds())
		draw.Draw(flat, flat.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
		draw.Draw(flat, flat.Bounds(), resized, resized.Bounds().Min, draw.Over)
		quality := cfg.jpegQuality
		for {
			var buffer bytes.Buffer
			if err := jpeg.Encode(&buffer, flat, &jpeg.Options{Quality: quality}); err != nil {
				return kiroDetectImageFormat(format, data), data
			}
			out = buffer.Bytes()
			if len(out)*4/3 <= cfg.maxBytes || quality <= minQuality {
				break
			}
			quality = max(quality-10, minQuality)
		}
		if len(out)*4/3 <= cfg.maxBytes || longSide <= minLongSide {
			break
		}
		longSide = max(int(float64(longSide)*0.8), minLongSide)
	}
	return "jpeg", base64.StdEncoding.EncodeToString(out)
}

func kiroDetectImageFormat(declared, data string) string {
	head := data
	if len(head) > 24 {
		head = head[:24]
	}
	raw, err := base64.StdEncoding.DecodeString(head)
	if err != nil {
		return declared
	}
	switch {
	case bytes.HasPrefix(raw, []byte("\x89PNG")):
		return "png"
	case bytes.HasPrefix(raw, []byte{0xFF, 0xD8, 0xFF}):
		return "jpeg"
	case bytes.HasPrefix(raw, []byte("GIF8")):
		return "gif"
	case len(raw) >= 12 && bytes.Equal(raw[:4], []byte("RIFF")) && bytes.Equal(raw[8:12], []byte("WEBP")):
		return "webp"
	}
	return declared
}

// Budget padded pixels conservatively for tRNS expansion, progressive JPEG coefficients, scan buffers and resize copies.
func kiroImageDecodeWithinBudget(width, height int) bool {
	const bytesPerPixel = 64
	const overhead = 1 << 20
	const maxPixels = (kiroImageMaxDecodeBytes - overhead) / bytesPerPixel
	if width <= 0 || height <= 0 || width > maxPixels || height > maxPixels {
		return false
	}
	paddedWidth := (width + 15) / 16 * 16
	paddedHeight := (height + 15) / 16 * 16
	return paddedWidth <= maxPixels/paddedHeight
}
