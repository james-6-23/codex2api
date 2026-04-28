// thumb.go —— 服务端 JPEG 缩略图压缩。
//
// 目标:让前端列表(图片任务历史 / 后台审计列表 / 在线体验回顾)在加载
// 时不必把动辄 2~5MB 的原图拉满,而是请求一份 ≤ N KB 的 JPEG 预览。
//
// 设计要点:
//   - 优先压缩到调用方指定的 budget(单位 KB,实际是 budget*1024 字节);
//     budget ≤ 0 不做任何处理。
//   - "多档降级":先固定较高质量按宽度梯度缩;若依旧超 budget,再保持
//     宽度按 JPEG 质量梯度降。最终仍超 budget 就返回最后一档结果(总比
//     原图小)。
//   - 解码失败回落原字节,不影响主流程。
//   - 上限保护:budget 超过 64KB 时自动夹到 64,避免被滥用成"原图压缩"。
package image

import (
	"bytes"
	gimage "image"
	"image/jpeg"

	"golang.org/x/image/draw"
)

// MaxThumbKB 单次缩略图请求允许的最大体积(KB)。再大就和原图差不多了,
// 走 thumb 路径反而比直接拉原图慢。这里夹到 64 KB,刚好能放下一张
// 800x600、质量 70 的人像 JPEG,够前端"放大前小卡片"用。
const MaxThumbKB = 64

// thumbStage 描述一档"宽度 + 质量"参数组合。
// MaxWidth 是输出图的最大长边像素;Quality 是 JPEG 质量 [1,100]。
type thumbStage struct {
	MaxWidth int
	Quality  int
}

// 多档降级表:从清晰到模糊 / 从大到小,逐档尝试,第一档命中 budget 即出。
// 注意第一档保持较大宽度,第二档之后才显著降清晰度,避免"列表里的小预览
// 全是糊的",影响选图。
var thumbStages = []thumbStage{
	{MaxWidth: 768, Quality: 78},
	{MaxWidth: 640, Quality: 70},
	{MaxWidth: 512, Quality: 62},
	{MaxWidth: 384, Quality: 55},
	{MaxWidth: 256, Quality: 50},
	{MaxWidth: 192, Quality: 45},
}

// ClampThumbKB 把外部传入的 KB 数夹到 [0, MaxThumbKB]。
// 0 表示不启用缩略图。其它都按合法值返回。
func ClampThumbKB(kb int) int {
	if kb <= 0 {
		return 0
	}
	if kb > MaxThumbKB {
		return MaxThumbKB
	}
	return kb
}

// MakeThumbnail 将任意主流格式(PNG/JPEG/GIF/WEBP)压缩为 JPEG 缩略图。
//
//   - src      原始图片字节
//   - budgetKB 目标体积,> 0 才有效;> MaxThumbKB 自动夹到 MaxThumbKB
//
// 返回 (data, content-type, ok)。ok=false 表示走不通(解码失败 / 输入为空 /
// budget 无效),调用方应当回落到原字节。
//
// 该函数是纯计算函数,不接入 LRU 缓存 —— 缩略图本身就便宜,且使用频度高
// 但生命周期短,缓存命中率低,不如让浏览器侧做 30 分钟 max-age 二级缓存。
func MakeThumbnail(src []byte, budgetKB int) ([]byte, string, bool) {
	budgetKB = ClampThumbKB(budgetKB)
	if budgetKB <= 0 || len(src) == 0 {
		return nil, "", false
	}
	budget := budgetKB * 1024

	srcImg, _, err := gimage.Decode(bytes.NewReader(src))
	if err != nil {
		return nil, "", false
	}
	b := srcImg.Bounds()
	sw, sh := b.Dx(), b.Dy()
	if sw <= 0 || sh <= 0 {
		return nil, "", false
	}

	// 已经比 budget 还小的情况非常罕见(原图通常都是 PNG ≥ 几百 KB),
	// 但保险起见判一下,避免徒劳的 decode + encode。
	if len(src) <= budget {
		// 原图已经小于预算,但仍要保证 content-type 是 jpeg 才算"缩略图命中"。
		// 这里直接转 JPEG 一次,大概率体积更小;如果反而更大,回落原字节。
		if out, ok := encodeStage(srcImg, sw, sh, thumbStages[0]); ok && len(out) <= len(src) {
			return out, "image/jpeg", true
		}
		// 否则保留原字节、原 ct 由调用方决定 —— 返回 ok=false 让调用方走原图。
		return nil, "", false
	}

	var last []byte
	for _, st := range thumbStages {
		out, ok := encodeStage(srcImg, sw, sh, st)
		if !ok {
			continue
		}
		last = out
		if len(out) <= budget {
			return out, "image/jpeg", true
		}
	}
	// 所有档位都没命中 budget:返回最后一档(最小那档)兜底,
	// 总比直出 5MB 原图友好。
	if len(last) > 0 {
		return last, "image/jpeg", true
	}
	return nil, "", false
}

// encodeStage 按指定档位等比缩 + JPEG 编码。失败时返回 ok=false。
// 若原图长边已经 ≤ MaxWidth,跳过缩放直接编码,避免"先放大再压缩"。
func encodeStage(srcImg gimage.Image, sw, sh int, st thumbStage) ([]byte, bool) {
	q := st.Quality
	if q < 1 {
		q = 1
	}
	if q > 100 {
		q = 100
	}

	// 选定目标长边
	target := st.MaxWidth
	long := sw
	if sh > long {
		long = sh
	}

	var dst gimage.Image
	if long <= target {
		dst = srcImg
	} else {
		var dw, dh int
		if sw >= sh {
			dw = target
			dh = int(float64(sh) * float64(target) / float64(sw))
		} else {
			dh = target
			dw = int(float64(sw) * float64(target) / float64(sh))
		}
		if dw < 1 {
			dw = 1
		}
		if dh < 1 {
			dh = 1
		}
		canvas := gimage.NewRGBA(gimage.Rect(0, 0, dw, dh))
		// 缩略图对清晰度要求不高,使用 ApproxBiLinear:CPU 开销显著
		// 低于 CatmullRom,目标体积 < 32KB 时人眼也分不出区别。
		draw.ApproxBiLinear.Scale(canvas, canvas.Bounds(), srcImg, srcImg.Bounds(), draw.Src, nil)
		dst = canvas
	}

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, dst, &jpeg.Options{Quality: q}); err != nil {
		return nil, false
	}
	return buf.Bytes(), true
}
