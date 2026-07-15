package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/internal/imagestore"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	defaultImagesMainModel = "gpt-5.4-mini"
	defaultImagesToolModel = "gpt-image-2"

	imageModel2KAlias = "gpt-image-2-2k"
	imageModel4KAlias = "gpt-image-2-4k"

	defaultImages1KSize = "1024x1024"
	defaultImages2KSize = "2048x2048"
	defaultImages4KSize = "3840x2160"

	defaultImages1KLandscapeSize = "1536x864"
	defaultImages1KPortraitSize  = "864x1536"
	defaultImages2KLandscapeSize = "2560x1440"
	defaultImages2KPortraitSize  = "1440x2560"
	defaultImages4KLandscapeSize = defaultImages4KSize
	defaultImages4KPortraitSize  = "2160x3840"
	defaultImages4KSquareSize    = "2880x2880"

	maxGPTImage2Pixels = 8294400

	// maxImageAttempts caps the total number of upstream attempts for image
	// generation requests, including retries across different accounts.
	maxImageAttempts = 5

	// MaxImageEditInputCount caps the number of input images for edit requests.
	// 与官方 Images API 对 gpt-image 系列的上限一致（16 张）；上游 responses
	// 通道已实测可接受 16 张 input_image（issue #275）。
	MaxImageEditInputCount = 16

	imageStreamConnectedComment = ": connected\n\n"
	imageStreamKeepaliveComment = ": keepalive\n\n"

	// imageCloudURLTTL 控制 response_format=url 时返回的预签名云直链有效期。
	imageCloudURLTTL = time.Hour
)

var imageStreamKeepaliveInterval = 15 * time.Second

type imageCallResult struct {
	Result        string
	RevisedPrompt string
	OutputFormat  string
	Size          string
	ByteSize      int
	Width         int
	Height        int
	Background    string
	Quality       string
	Model         string
}

type imageOutputStats struct {
	ByteSize int
	Width    int
	Height   int
}

type imageUsageLogInfo struct {
	Count  int
	Width  int
	Height int
	Bytes  int
	Format string
	Size   string
}

func decodeImageBase64(raw string) ([]byte, bool) {
	encoded := strings.TrimSpace(raw)
	if encoded == "" {
		return nil, false
	}
	// Guard against extremely large inputs (100 MB limit).
	const maxDecodeInputLen = 100 * 1024 * 1024
	if len(encoded) > maxDecodeInputLen {
		return nil, false
	}
	if strings.HasPrefix(strings.ToLower(encoded), "data:") {
		if comma := strings.Index(encoded, ","); comma >= 0 {
			encoded = encoded[comma+1:]
		}
	}
	if strings.ContainsAny(encoded, " \t\r\n") {
		encoded = strings.NewReplacer(" ", "", "\t", "", "\r", "", "\n", "").Replace(encoded)
	}
	// Prefer URL-safe encodings when the input contains '-' or '_'
	// (characters that only appear in URL-safe base64).
	preferURLSafe := strings.ContainsAny(encoded, "-_")
	encodings := [4]*base64.Encoding{
		base64.StdEncoding,
		base64.RawStdEncoding,
		base64.URLEncoding,
		base64.RawURLEncoding,
	}
	if preferURLSafe {
		encodings = [4]*base64.Encoding{
			base64.URLEncoding,
			base64.RawURLEncoding,
			base64.StdEncoding,
			base64.RawStdEncoding,
		}
	}
	for _, encoding := range encodings {
		data, err := encoding.DecodeString(encoded)
		if err == nil {
			return data, true
		}
	}
	return nil, false
}

func imageStatsFromBase64(raw string) (imageOutputStats, bool) {
	data, ok := decodeImageBase64(raw)
	if !ok {
		return imageOutputStats{}, false
	}
	stats := imageOutputStats{ByteSize: len(data)}
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(data)); err == nil {
		stats.Width = cfg.Width
		stats.Height = cfg.Height
		return stats, true
	}
	if width, height, ok := decodeWebPDimensions(data); ok {
		stats.Width = width
		stats.Height = height
	}
	return stats, true
}

func decodeWebPDimensions(data []byte) (int, int, bool) {
	if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WEBP" {
		return 0, 0, false
	}
	for offset := 12; offset+8 <= len(data); {
		chunkType := string(data[offset : offset+4])
		chunkSize := int(data[offset+4]) | int(data[offset+5])<<8 | int(data[offset+6])<<16 | int(data[offset+7])<<24
		payloadStart := offset + 8
		payloadEnd := payloadStart + chunkSize
		if chunkSize < 0 || payloadEnd > len(data) {
			return 0, 0, false
		}
		payload := data[payloadStart:payloadEnd]
		switch chunkType {
		case "VP8X":
			if len(payload) >= 10 {
				width := 1 + int(payload[4]) + int(payload[5])<<8 + int(payload[6])<<16
				height := 1 + int(payload[7]) + int(payload[8])<<8 + int(payload[9])<<16
				return width, height, true
			}
		case "VP8 ":
			if len(payload) >= 10 && payload[3] == 0x9d && payload[4] == 0x01 && payload[5] == 0x2a {
				width := (int(payload[6]) | int(payload[7])<<8) & 0x3fff
				height := (int(payload[8]) | int(payload[9])<<8) & 0x3fff
				return width, height, true
			}
		case "VP8L":
			if len(payload) >= 5 && payload[0] == 0x2f {
				bits := int(payload[1]) | int(payload[2])<<8 | int(payload[3])<<16 | int(payload[4])<<24
				width := 1 + (bits & 0x3fff)
				height := 1 + ((bits >> 14) & 0x3fff)
				return width, height, true
			}
		}
		offset = payloadEnd
		if chunkSize%2 == 1 {
			offset++
		}
	}
	return 0, 0, false
}

func populateImageStats(image *imageCallResult) bool {
	if image == nil || strings.TrimSpace(image.Result) == "" {
		return false
	}
	stats, ok := imageStatsFromBase64(image.Result)
	if !ok {
		return false
	}
	changed := false
	if stats.ByteSize > 0 && image.ByteSize == 0 {
		image.ByteSize = stats.ByteSize
		changed = true
	}
	if stats.Width > 0 && image.Width == 0 {
		image.Width = stats.Width
		changed = true
	}
	if stats.Height > 0 && image.Height == 0 {
		image.Height = stats.Height
		changed = true
	}
	return changed
}

func addImageStatsToMap(item map[string]any) bool {
	if item == nil {
		return false
	}
	result := firstNonEmptyAnyString(item["result"])
	if result == "" {
		return false
	}
	stats, ok := imageStatsFromBase64(result)
	if !ok {
		return false
	}
	changed := false
	if stats.ByteSize > 0 {
		if _, exists := item["bytes"]; !exists {
			item["bytes"] = stats.ByteSize
			changed = true
		}
	}
	if stats.Width > 0 {
		if _, exists := item["width"]; !exists {
			item["width"] = stats.Width
			changed = true
		}
	}
	if stats.Height > 0 {
		if _, exists := item["height"]; !exists {
			item["height"] = stats.Height
			changed = true
		}
	}
	return changed
}

func imageUsageLogInfoFromImage(image imageCallResult) imageUsageLogInfo {
	populateImageStats(&image)
	info := imageUsageLogInfo{
		Count:  1,
		Width:  image.Width,
		Height: image.Height,
		Bytes:  image.ByteSize,
		Format: strings.TrimSpace(image.OutputFormat),
		Size:   strings.TrimSpace(image.Size),
	}
	return info
}

func mergeImageUsageLogInfo(current imageUsageLogInfo, next imageUsageLogInfo) imageUsageLogInfo {
	if next.Count <= 0 {
		return current
	}
	if current.Count <= 0 {
		return next
	}
	current.Count += next.Count
	if current.Width == 0 {
		current.Width = next.Width
	}
	if current.Height == 0 {
		current.Height = next.Height
	}
	if current.Bytes == 0 {
		current.Bytes = next.Bytes
	}
	if current.Format == "" {
		current.Format = next.Format
	}
	if current.Size == "" {
		current.Size = next.Size
	}
	return current
}

func imageUsageLogInfoFromImages(images []imageCallResult) imageUsageLogInfo {
	var info imageUsageLogInfo
	for _, image := range images {
		info = mergeImageUsageLogInfo(info, imageUsageLogInfoFromImage(image))
	}
	return info
}

func AppendImageStyleToPrompt(prompt string, style string) string {
	prompt = strings.TrimSpace(prompt)
	style = strings.TrimSpace(style)
	if style == "" {
		return prompt
	}
	return prompt + "\n\nStyle guidance: " + style
}

func imageUsageLogInfoFromResponseJSON(responseJSON []byte) imageUsageLogInfo {
	var info imageUsageLogInfo
	output := gjson.GetBytes(responseJSON, "output")
	if !output.IsArray() {
		return info
	}
	for _, item := range output.Array() {
		if item.Get("type").String() != "image_generation_call" || strings.TrimSpace(item.Get("result").String()) == "" {
			continue
		}
		image := imageCallResult{
			Result:       strings.TrimSpace(item.Get("result").String()),
			OutputFormat: strings.TrimSpace(item.Get("output_format").String()),
			Size:         strings.TrimSpace(item.Get("size").String()),
			ByteSize:     int(item.Get("bytes").Int()),
			Width:        int(item.Get("width").Int()),
			Height:       int(item.Get("height").Int()),
		}
		info = mergeImageUsageLogInfo(info, imageUsageLogInfoFromImage(image))
	}
	return info
}

func applyImageUsageLogInfo(input *database.UsageLogInput, info imageUsageLogInfo) {
	if input == nil || info.Count <= 0 {
		return
	}
	input.ImageCount = info.Count
	input.ImageWidth = info.Width
	input.ImageHeight = info.Height
	input.ImageBytes = info.Bytes
	input.ImageFormat = info.Format
	input.ImageSize = info.Size
}

func isImageOnlyModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "gpt-image-")
}

type imageDefaultSizeSet struct {
	defaultSize   string
	squareSize    string
	landscapeSize string
	portraitSize  string
}

func normalizeImageToolModel(model string) (string, string) {
	return normalizeImageToolModelForPrompt(model, "")
}

func normalizeImageToolModelForPrompt(model string, prompt string) (string, string) {
	model = strings.TrimSpace(model)
	switch strings.ToLower(model) {
	case "", defaultImagesToolModel:
		return defaultImagesToolModel, inferDefaultImageSize(prompt, imageDefaultSizeSet{
			defaultSize:   defaultImages1KSize,
			squareSize:    defaultImages1KSize,
			landscapeSize: defaultImages1KLandscapeSize,
			portraitSize:  defaultImages1KPortraitSize,
		})
	case imageModel2KAlias:
		return defaultImagesToolModel, inferDefaultImageSize(prompt, imageDefaultSizeSet{
			defaultSize:   defaultImages2KSize,
			squareSize:    defaultImages2KSize,
			landscapeSize: defaultImages2KLandscapeSize,
			portraitSize:  defaultImages2KPortraitSize,
		})
	case imageModel4KAlias:
		return defaultImagesToolModel, inferDefaultImageSize(prompt, imageDefaultSizeSet{
			defaultSize:   defaultImages4KSize,
			squareSize:    defaultImages4KSquareSize,
			landscapeSize: defaultImages4KLandscapeSize,
			portraitSize:  defaultImages4KPortraitSize,
		})
	default:
		return model, ""
	}
}

func inferDefaultImageSize(prompt string, sizes imageDefaultSizeSet) string {
	switch inferImageAspectFromPrompt(prompt) {
	case "square":
		if sizes.squareSize != "" {
			return sizes.squareSize
		}
	case "landscape":
		if sizes.landscapeSize != "" {
			return sizes.landscapeSize
		}
	case "portrait":
		if sizes.portraitSize != "" {
			return sizes.portraitSize
		}
	}
	return sizes.defaultSize
}

func inferImageAspectFromPrompt(prompt string) string {
	normalized := strings.ToLower(strings.TrimSpace(prompt))
	if normalized == "" {
		return ""
	}

	containsAny := func(keywords ...string) bool {
		for _, keyword := range keywords {
			if strings.Contains(normalized, strings.ToLower(keyword)) {
				return true
			}
		}
		return false
	}

	if containsAny("方图", "方形", "正方形", "square", "1:1") {
		return "square"
	}
	if containsAny("竖版", "竖屏", "纵向", "竖向", "手机壁纸", "手机屏保", "手机海报", "portrait", "vertical", "phone wallpaper", "mobile wallpaper", "9:16") {
		return "portrait"
	}
	if containsAny("横版", "横屏", "横向", "宽屏", "桌面壁纸", "电脑壁纸", "电脑桌面", "landscape", "horizontal", "wide", "widescreen", "desktop wallpaper", "16:9") {
		return "landscape"
	}

	if containsAny("头像", "图标", "徽标", "贴纸", "表情包", "logo", "icon", "avatar", "sticker") {
		return "square"
	}
	if containsAny("海报", "poster", "封面", "cover") {
		return "portrait"
	}
	if containsAny("壁纸", "wallpaper", "电影感", "cinematic", "banner", "横幅") {
		return "landscape"
	}
	return ""
}

func setDefaultImageToolSize(tool []byte, defaultSize string) []byte {
	defaultSize = strings.TrimSpace(defaultSize)
	if defaultSize == "" || strings.TrimSpace(gjson.GetBytes(tool, "size").String()) != "" {
		return tool
	}
	tool, _ = sjson.SetBytes(tool, "size", defaultSize)
	return tool
}

func shouldValidateGPTImage2Size(model string) bool {
	toolModel, _ := normalizeImageToolModel(model)
	return strings.EqualFold(strings.TrimSpace(toolModel), defaultImagesToolModel)
}

func validateGPTImage2Size(size string) error {
	raw := strings.TrimSpace(size)
	if raw == "" || strings.EqualFold(raw, "auto") {
		return nil
	}

	parts := strings.Split(strings.ToLower(raw), "x")
	if len(parts) != 2 {
		return fmt.Errorf("image size %q must use WIDTHxHEIGHT format or auto", raw)
	}
	width, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || width <= 0 {
		return fmt.Errorf("image size %q has invalid width", raw)
	}
	height, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil || height <= 0 {
		return fmt.Errorf("image size %q has invalid height", raw)
	}
	if width%16 != 0 || height%16 != 0 {
		return fmt.Errorf("image size %q is invalid: width and height must be multiples of 16", raw)
	}
	pixels := int64(width) * int64(height)
	if pixels > maxGPTImage2Pixels {
		return fmt.Errorf("image size %q is invalid: total pixels %d exceeds max %d", raw, pixels, maxGPTImage2Pixels)
	}
	longSide, shortSide := width, height
	if height > width {
		longSide, shortSide = height, width
	}
	if int64(longSide) > int64(shortSide)*3 {
		return fmt.Errorf("image size %q is invalid: aspect ratio must not exceed 3:1", raw)
	}
	return nil
}

func validateResponsesImageGenerationSizes(body []byte) error {
	tools := gjson.GetBytes(body, "tools")
	if !tools.Exists() || !tools.IsArray() {
		return nil
	}
	for i, tool := range tools.Array() {
		if strings.TrimSpace(tool.Get("type").String()) != "image_generation" {
			continue
		}
		if !shouldValidateGPTImage2Size(tool.Get("model").String()) {
			continue
		}
		size := tool.Get("size")
		if !size.Exists() || size.Type == gjson.Null {
			continue
		}
		if size.Type != gjson.String {
			return fmt.Errorf("image_generation tool %d size must be a string like 1024x1024 or auto", i)
		}
		if err := validateGPTImage2Size(size.String()); err != nil {
			return fmt.Errorf("image_generation tool %d: %w", i, err)
		}
	}
	return nil
}

func responsesBodyHasImageGenerationTool(body []byte) bool {
	tools := gjson.GetBytes(body, "tools")
	if tools.Exists() && tools.IsArray() {
		for _, tool := range tools.Array() {
			if strings.TrimSpace(tool.Get("type").String()) == "image_generation" {
				return true
			}
		}
	}
	choice := gjson.GetBytes(body, "tool_choice")
	if !choice.Exists() {
		return false
	}
	if choice.Type == gjson.String {
		return strings.EqualFold(strings.TrimSpace(choice.String()), "image_generation")
	}
	return strings.EqualFold(strings.TrimSpace(choice.Get("type").String()), "image_generation")
}

func responsesBodyRequestsImageGeneration(body []byte) bool {
	if isImageOnlyModel(gjson.GetBytes(body, "model").String()) {
		return true
	}
	choice := gjson.GetBytes(body, "tool_choice")
	if choice.Type == gjson.String && strings.EqualFold(strings.TrimSpace(choice.String()), "image_generation") {
		return true
	}
	if choice.Exists() && strings.EqualFold(strings.TrimSpace(choice.Get("type").String()), "image_generation") {
		return true
	}
	for _, key := range responsesImageGenerationOptionFields {
		value := gjson.GetBytes(body, key)
		if value.Exists() && value.Type != gjson.Null {
			return true
		}
	}
	return false
}

// rawResponsesBodyShouldForceHTTPForImageGeneration 判断请求是否"真的要生图"，
// 从而必须改走 HTTP 上游——WebSocket 上游传输大体积图片数据会卡死（issue #220）。
//
// 只在"真实生图意图"时强制 HTTP：image-only 模型、tool_choice=image_generation、
// 顶层图片选项字段（responsesBodyRequestsImageGeneration），或 prompt 中的自然语言
// 生图意图（issue #288）。它刻意**不**因 tools[] 里单纯存在 image_generation 工具而
// 触发：客户端把生图工具无差别注入到每个请求时（issue #304），否则会把普通请求也全部
// 打到 HTTP、丢掉 WebSocket。这类"注入但未使用"的工具改由 WS 路径上的
// stripResponsesImageGenerationTool 剥离。
//
// /v1/responses 路径须传入下游 raw body（PrepareResponsesBody 注入默认工具之前）；
// chat 路径传入翻译后的 Codex body（TranslateRequest 不会自动注入图片工具）。两者都
// 不应含自动注入的 tool_choice=image_generation，否则普通请求会被误判。
func rawResponsesBodyShouldForceHTTPForImageGeneration(body []byte) bool {
	return responsesBodyRequestsImageGeneration(body) || responsesBodyHasNaturalImageGenerationIntent(body)
}

func responsesBodyHasNaturalImageGenerationIntent(body []byte) bool {
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		return false
	}
	return promptTextRequestsImageGeneration(extractResponsesPromptText(parsed))
}

func promptTextRequestsImageGeneration(text string) bool {
	normalized := normalizeImageIntentText(text)
	if normalized == "" {
		return false
	}
	if containsAnyPhrase(normalized, imageIntentFalsePositivePhrases) {
		return false
	}
	return containsAnyPhrase(normalized, imageIntentPositivePhrases)
}

func normalizeImageIntentText(text string) string {
	text = strings.ToLower(strings.TrimSpace(text))
	if text == "" {
		return ""
	}
	replacer := strings.NewReplacer(
		"\r", " ",
		"\n", " ",
		"\t", " ",
		"，", " ",
		"。", " ",
		"！", " ",
		"？", " ",
		"；", " ",
		"：", " ",
		",", " ",
		".", " ",
		"!", " ",
		"?", " ",
		";", " ",
		":", " ",
		"\"", " ",
		"'", " ",
		"`", " ",
	)
	return strings.Join(strings.Fields(replacer.Replace(text)), " ")
}

func containsAnyPhrase(text string, phrases []string) bool {
	for _, phrase := range phrases {
		if strings.Contains(text, phrase) {
			return true
		}
	}
	return false
}

var imageIntentFalsePositivePhrases = []string{
	"生成图片的代码",
	"生成图片代码",
	"生成图片的脚本",
	"生成图片脚本",
	"生成图片的函数",
	"生成图片函数",
	"图片生成函数",
	"写一个生成图片",
	"写个生成图片",
	"代码",
	"脚本",
	"函数",
	"接口",
	"教程",
	"示例",
	"文档",
	"提示词",
	"生成图片接口",
	"生图接口",
	"生成图片 api",
	"生图 api",
	"生成图片 sdk",
	"生图 sdk",
	"生成图片教程",
	"生图教程",
	"生成图片示例",
	"生图示例",
	"生成图片的提示词",
	"生图提示词",
	"生成一张表格",
	"生成一张清单",
	"生成一张列表",
	"生成一张报告",
	"生成一张计划",
	"如何生成图片",
	"怎么生成图片",
	"如何生图",
	"怎么生图",
	"how to generate an image",
	"how do i generate an image",
	"generate an image with python",
	"generate images with python",
	"image generation code",
	"image generation api",
	"image generation sdk",
	"image generation tutorial",
	"image generation example",
	"image prompt",
	"write code",
	"write a script",
	" code",
	"code ",
	"script",
	"function",
	"tutorial",
	"example",
	"documentation",
	"mermaid",
	"python script",
	"javascript",
	"typescript",
	"html canvas",
	"svg",
}

var imageIntentPositivePhrases = []string{
	"生图",
	"文生图",
	"图生图",
	"生成一张",
	"生成一幅",
	"生成图片",
	"生成照片",
	"生成海报",
	"生成封面",
	"生成头像",
	"生成壁纸",
	"生成插画",
	"生成漫画",
	"生成表情包",
	"生成一张表情包",
	"生成图标",
	"生成logo",
	"生成 logo",
	"画一张",
	"画一幅",
	"画一个",
	"画个",
	"帮我画",
	"请画",
	"绘制一张",
	"绘制一幅",
	"做一张图",
	"做张图",
	"做一张图片",
	"做个图",
	"出一张图",
	"出图",
	"设计一张海报",
	"设计一个海报",
	"设计一个logo",
	"设计一个 logo",
	"设计图标",
	"修图",
	"改图",
	"编辑图片",
	"编辑照片",
	"修改图片",
	"修改照片",
	"把这张图",
	"将这张图",
	"把图片",
	"把照片",
	"换背景",
	"背景换",
	"去背景",
	"抠图",
	"扩图",
	"重绘",
	"局部重绘",
	"generate an image",
	"generate a picture",
	"generate a photo",
	"create an image",
	"create a picture",
	"create a photo",
	"make an image",
	"make a picture",
	"make a photo",
	"draw me",
	"draw a",
	"draw an",
	"paint a",
	"paint an",
	"illustrate a",
	"illustrate an",
	"design a poster",
	"design a logo",
	"edit this image",
	"edit the image",
	"modify this image",
	"modify the image",
	"retouch this photo",
	"retouch the photo",
	"turn this image",
	"make this image",
}

// stripResponsesImageGenerationTool 移除请求体中的 image_generation 工具及指向它的
// tool_choice。仅在 WebSocket 上游模式下使用：此时 body 中的 image_generation 工具
// 要么是 PrepareResponsesBody 自动注入的，要么是客户端无差别注入的（issue #304）——
// 两者都未表达真实生图意图（真实意图已被 rawResponsesBodyShouldForceHTTPForImageGeneration
// 判定为强制 HTTP，不会走到这里）。移除后可防止模型自主调用图片工具产生大体积数据导致
// WS 流卡死（issue #220）。
func stripResponsesImageGenerationTool(body []byte) []byte {
	tools := gjson.GetBytes(body, "tools")
	if tools.Exists() && tools.IsArray() {
		kept := make([]interface{}, 0, len(tools.Array()))
		removed := false
		for _, tool := range tools.Array() {
			if strings.TrimSpace(tool.Get("type").String()) == "image_generation" {
				removed = true
				continue
			}
			kept = append(kept, tool.Value())
		}
		if removed {
			if len(kept) == 0 {
				body, _ = sjson.DeleteBytes(body, "tools")
			} else {
				body, _ = sjson.SetBytes(body, "tools", kept)
			}
		}
	}
	choice := gjson.GetBytes(body, "tool_choice")
	if choice.Exists() {
		isImageChoice := false
		if choice.Type == gjson.String {
			isImageChoice = strings.EqualFold(strings.TrimSpace(choice.String()), "image_generation")
		} else {
			isImageChoice = strings.EqualFold(strings.TrimSpace(choice.Get("type").String()), "image_generation")
		}
		if isImageChoice {
			body, _ = sjson.DeleteBytes(body, "tool_choice")
		}
	}
	// 移除与图片工具配套注入的桥接 instructions（引导模型调用 image_generation
	// 工具）；保留用户自带的 instructions 内容。
	if instructions := gjson.GetBytes(body, "instructions").String(); strings.Contains(instructions, codexImageGenerationBridgeMarker) {
		cleaned := strings.ReplaceAll(instructions, "\n\n"+codexImageGenerationBridgeText, "")
		cleaned = strings.ReplaceAll(cleaned, codexImageGenerationBridgeText, "")
		cleaned = strings.TrimSpace(cleaned)
		if cleaned == "" {
			body, _ = sjson.DeleteBytes(body, "instructions")
		} else {
			body, _ = sjson.SetBytes(body, "instructions", cleaned)
		}
	}
	return body
}

func validateImagesModel(model string) error {
	if !isImageOnlyModel(model) {
		return fmt.Errorf("images endpoint requires an image model, got %q", strings.TrimSpace(model))
	}
	return nil
}

func sendImageOnlyModelError(c *gin.Context, model string) {
	c.JSON(http.StatusServiceUnavailable, gin.H{
		"error": gin.H{
			"message": fmt.Sprintf("model %s is only supported on /v1/images/generations and /v1/images/edits", strings.TrimSpace(model)),
			"type":    "server_error",
		},
	})
}

func mimeTypeFromOutputFormat(outputFormat string) string {
	outputFormat = strings.TrimSpace(outputFormat)
	if outputFormat == "" {
		return "image/png"
	}
	if strings.Contains(outputFormat, "/") {
		return outputFormat
	}
	switch strings.ToLower(outputFormat) {
	case "png":
		return "image/png"
	case "jpg", "jpeg":
		return "image/jpeg"
	case "webp":
		return "image/webp"
	default:
		return "image/png"
	}
}

func parseIntField(raw string, fallback int64) int64 {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		return fallback
	}
	return value
}

func parseBoolField(raw string, fallback bool) bool {
	raw = strings.TrimSpace(strings.ToLower(raw))
	if raw == "" {
		return fallback
	}
	switch raw {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func multipartFileToDataURL(fileHeader *multipart.FileHeader) (string, error) {
	if fileHeader == nil {
		return "", fmt.Errorf("upload file is nil")
	}
	const maxImageUploadBytes = 20 * 1024 * 1024
	if fileHeader.Size > maxImageUploadBytes {
		return "", fmt.Errorf("upload file too large (%d bytes, max %d)", fileHeader.Size, maxImageUploadBytes)
	}
	if fileHeader.Size == 0 {
		return "", fmt.Errorf("upload file is empty")
	}
	file, err := fileHeader.Open()
	if err != nil {
		return "", fmt.Errorf("open upload file failed: %w", err)
	}
	defer file.Close()

	data, err := io.ReadAll(file)
	if err != nil {
		return "", fmt.Errorf("read upload file failed: %w", err)
	}
	if len(data) == 0 {
		return "", fmt.Errorf("upload %q is empty", strings.TrimSpace(fileHeader.Filename))
	}

	mediaType := strings.TrimSpace(fileHeader.Header.Get("Content-Type"))
	if mediaType == "" {
		mediaType = http.DetectContentType(data)
	}
	return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

func (h *Handler) ImagesGenerations(c *gin.Context) {
	rawBody, err := readRawRequestBody(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid request: " + err.Error(), "type": "invalid_request_error"}})
		return
	}
	if !json.Valid(rawBody) {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid request: body must be valid JSON", "type": "invalid_request_error"}})
		return
	}

	prompt := strings.TrimSpace(gjson.GetBytes(rawBody, "prompt").String())
	if prompt == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid request: prompt is required", "type": "invalid_request_error"}})
		return
	}

	imageModel := strings.TrimSpace(gjson.GetBytes(rawBody, "model").String())
	modelProvided := imageModel != ""
	if imageModel == "" {
		imageModel = defaultImagesToolModel
	}
	requestModel := imageModel
	if modelProvided {
		if mappedModel, ok := h.resolveConfiguredRequestModel(imageModel, h.supportedModelIDs(c.Request.Context())); ok {
			imageModel = mappedModel
		}
	}
	logEffectiveModel := usageEffectiveModelForMapping(requestModel, imageModel, !strings.EqualFold(requestModel, imageModel))
	if err := validateImagesModel(imageModel); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid request: " + err.Error(), "type": "invalid_request_error"}})
		return
	}

	responseFormat := strings.TrimSpace(gjson.GetBytes(rawBody, "response_format").String())
	if responseFormat == "" {
		responseFormat = "b64_json"
	}
	stream := gjson.GetBytes(rawBody, "stream").Bool()

	style := strings.TrimSpace(gjson.GetBytes(rawBody, "style").String())
	promptForRequest := AppendImageStyleToPrompt(prompt, style)
	if h.inspectPromptFilterTextOpenAI(c, promptForRequest, "/v1/images/generations", imageModel) {
		return
	}
	if h.enforceAPIKeyLimitsAndReply(c, imageModel) {
		return
	}
	releaseAPIKeyConcurrency, ok := h.acquireAPIKeyConcurrency(c)
	if !ok {
		return
	}
	if releaseAPIKeyConcurrency != nil {
		defer releaseAPIKeyConcurrency()
	}
	tool := []byte(`{"type":"image_generation","action":"generate","model":""}`)
	toolModel, defaultSize := normalizeImageToolModelForPrompt(imageModel, promptForRequest)
	tool, _ = sjson.SetBytes(tool, "model", toolModel)
	for _, field := range []string{"size", "quality", "background", "output_format", "moderation"} {
		if value := strings.TrimSpace(gjson.GetBytes(rawBody, field).String()); value != "" {
			tool, _ = sjson.SetBytes(tool, field, value)
		}
	}
	for _, field := range []string{"output_compression", "partial_images"} {
		if value := gjson.GetBytes(rawBody, field); value.Exists() && value.Type == gjson.Number {
			tool, _ = sjson.SetBytes(tool, field, value.Int())
		}
	}
	tool = setDefaultImageToolSize(tool, defaultSize)

	responsesBody := buildImagesResponsesRequest(promptForRequest, nil, tool)
	h.forwardImagesRequest(c, "/v1/images/generations", imageModel, requestModel, logEffectiveModel, responsesBody, responseFormat, "image_generation", stream)
}

func (h *Handler) ImagesEdits(c *gin.Context) {
	contentType := strings.ToLower(strings.TrimSpace(c.GetHeader("Content-Type")))
	if strings.HasPrefix(contentType, "application/json") {
		h.imagesEditsFromJSON(c)
		return
	}
	if strings.HasPrefix(contentType, "multipart/form-data") || contentType == "" {
		h.imagesEditsFromMultipart(c)
		return
	}

	c.JSON(http.StatusBadRequest, gin.H{
		"error": gin.H{"message": fmt.Sprintf("Invalid request: unsupported Content-Type %q", contentType), "type": "invalid_request_error"},
	})
}

func (h *Handler) imagesEditsFromMultipart(c *gin.Context) {
	form, err := c.MultipartForm()
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid request: " + err.Error(), "type": "invalid_request_error"}})
		return
	}

	prompt := strings.TrimSpace(c.PostForm("prompt"))
	if prompt == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid request: prompt is required", "type": "invalid_request_error"}})
		return
	}

	var imageFiles []*multipart.FileHeader
	if files := form.File["image[]"]; len(files) > 0 {
		imageFiles = files
	} else if files := form.File["image"]; len(files) > 0 {
		imageFiles = files
	}
	if len(imageFiles) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid request: image is required", "type": "invalid_request_error"}})
		return
	}
	if len(imageFiles) > MaxImageEditInputCount {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": fmt.Sprintf("Invalid request: too many input images (%d, max %d)", len(imageFiles), MaxImageEditInputCount), "type": "invalid_request_error"}})
		return
	}

	images := make([]string, 0, len(imageFiles))
	for _, fileHeader := range imageFiles {
		dataURL, err := multipartFileToDataURL(fileHeader)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid request: " + err.Error(), "type": "invalid_request_error"}})
			return
		}
		images = append(images, dataURL)
	}

	var maskDataURL string
	if maskFiles := form.File["mask"]; len(maskFiles) > 0 && maskFiles[0] != nil {
		dataURL, err := multipartFileToDataURL(maskFiles[0])
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid request: " + err.Error(), "type": "invalid_request_error"}})
			return
		}
		maskDataURL = dataURL
	}

	imageModel := strings.TrimSpace(c.PostForm("model"))
	modelProvided := imageModel != ""
	if imageModel == "" {
		imageModel = defaultImagesToolModel
	}
	requestModel := imageModel
	if modelProvided {
		if mappedModel, ok := h.resolveConfiguredRequestModel(imageModel, h.supportedModelIDs(c.Request.Context())); ok {
			imageModel = mappedModel
		}
	}
	logEffectiveModel := usageEffectiveModelForMapping(requestModel, imageModel, !strings.EqualFold(requestModel, imageModel))
	if err := validateImagesModel(imageModel); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid request: " + err.Error(), "type": "invalid_request_error"}})
		return
	}

	responseFormat := strings.TrimSpace(c.PostForm("response_format"))
	if responseFormat == "" {
		responseFormat = "b64_json"
	}
	stream := parseBoolField(c.PostForm("stream"), false)

	style := strings.TrimSpace(c.PostForm("style"))
	promptForRequest := AppendImageStyleToPrompt(prompt, style)
	if h.inspectPromptFilterTextOpenAI(c, promptForRequest, "/v1/images/edits", imageModel) {
		return
	}
	if h.enforceAPIKeyLimitsAndReply(c, imageModel) {
		return
	}
	releaseAPIKeyConcurrency, ok := h.acquireAPIKeyConcurrency(c)
	if !ok {
		return
	}
	if releaseAPIKeyConcurrency != nil {
		defer releaseAPIKeyConcurrency()
	}
	tool := buildImagesEditToolFromForm(c, imageModel, maskDataURL)
	responsesBody := buildImagesResponsesRequest(promptForRequest, images, tool)
	h.forwardImagesRequest(c, "/v1/images/edits", imageModel, requestModel, logEffectiveModel, responsesBody, responseFormat, "image_edit", stream)
}

func buildImagesEditToolFromForm(c *gin.Context, imageModel, maskDataURL string) []byte {
	tool := []byte(`{"type":"image_generation","action":"edit","model":""}`)
	toolModel, defaultSize := normalizeImageToolModelForPrompt(imageModel, strings.TrimSpace(c.PostForm("prompt")))
	tool, _ = sjson.SetBytes(tool, "model", toolModel)
	for _, field := range []string{"size", "quality", "background", "output_format", "input_fidelity", "moderation"} {
		if value := strings.TrimSpace(c.PostForm(field)); value != "" {
			tool, _ = sjson.SetBytes(tool, field, value)
		}
	}
	for _, field := range []string{"output_compression", "partial_images"} {
		if value := strings.TrimSpace(c.PostForm(field)); value != "" {
			tool, _ = sjson.SetBytes(tool, field, parseIntField(value, 0))
		}
	}
	if strings.TrimSpace(maskDataURL) != "" {
		tool, _ = sjson.SetBytes(tool, "input_image_mask.image_url", strings.TrimSpace(maskDataURL))
	}
	tool = setDefaultImageToolSize(tool, defaultSize)
	return tool
}

func (h *Handler) imagesEditsFromJSON(c *gin.Context) {
	rawBody, err := readRawRequestBody(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid request: " + err.Error(), "type": "invalid_request_error"}})
		return
	}
	if !json.Valid(rawBody) {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid request: body must be valid JSON", "type": "invalid_request_error"}})
		return
	}

	prompt := strings.TrimSpace(gjson.GetBytes(rawBody, "prompt").String())
	if prompt == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid request: prompt is required", "type": "invalid_request_error"}})
		return
	}

	images := make([]string, 0)
	imagesResult := gjson.GetBytes(rawBody, "images")
	if imagesResult.Exists() && !imagesResult.IsArray() {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid request: invalid images field type", "type": "invalid_request_error"}})
		return
	}
	for _, image := range imagesResult.Array() {
		if imageURL := strings.TrimSpace(image.Get("image_url").String()); imageURL != "" {
			images = append(images, imageURL)
			continue
		}
		if image.Get("file_id").Exists() {
			c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid request: images[].file_id is not supported (use images[].image_url instead)", "type": "invalid_request_error"}})
			return
		}
	}
	if len(images) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid request: images[].image_url is required", "type": "invalid_request_error"}})
		return
	}
	if len(images) > MaxImageEditInputCount {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": fmt.Sprintf("Invalid request: too many input images (%d, max %d)", len(images), MaxImageEditInputCount), "type": "invalid_request_error"}})
		return
	}

	maskDataURL := strings.TrimSpace(gjson.GetBytes(rawBody, "mask.image_url").String())
	if maskDataURL == "" && gjson.GetBytes(rawBody, "mask.file_id").Exists() {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid request: mask.file_id is not supported (use mask.image_url instead)", "type": "invalid_request_error"}})
		return
	}

	imageModel := strings.TrimSpace(gjson.GetBytes(rawBody, "model").String())
	modelProvided := imageModel != ""
	if imageModel == "" {
		imageModel = defaultImagesToolModel
	}
	requestModel := imageModel
	if modelProvided {
		if mappedModel, ok := h.resolveConfiguredRequestModel(imageModel, h.supportedModelIDs(c.Request.Context())); ok {
			imageModel = mappedModel
		}
	}
	logEffectiveModel := usageEffectiveModelForMapping(requestModel, imageModel, !strings.EqualFold(requestModel, imageModel))
	if err := validateImagesModel(imageModel); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid request: " + err.Error(), "type": "invalid_request_error"}})
		return
	}

	responseFormat := strings.TrimSpace(gjson.GetBytes(rawBody, "response_format").String())
	if responseFormat == "" {
		responseFormat = "b64_json"
	}
	stream := gjson.GetBytes(rawBody, "stream").Bool()

	style := strings.TrimSpace(gjson.GetBytes(rawBody, "style").String())
	promptForRequest := AppendImageStyleToPrompt(prompt, style)
	if h.inspectPromptFilterTextOpenAI(c, promptForRequest, "/v1/images/edits", imageModel) {
		return
	}
	if h.enforceAPIKeyLimitsAndReply(c, imageModel) {
		return
	}
	releaseAPIKeyConcurrency, ok := h.acquireAPIKeyConcurrency(c)
	if !ok {
		return
	}
	if releaseAPIKeyConcurrency != nil {
		defer releaseAPIKeyConcurrency()
	}
	tool := []byte(`{"type":"image_generation","action":"edit","model":""}`)
	toolModel, defaultSize := normalizeImageToolModelForPrompt(imageModel, promptForRequest)
	tool, _ = sjson.SetBytes(tool, "model", toolModel)
	for _, field := range []string{"size", "quality", "background", "output_format", "input_fidelity", "moderation"} {
		if value := strings.TrimSpace(gjson.GetBytes(rawBody, field).String()); value != "" {
			tool, _ = sjson.SetBytes(tool, field, value)
		}
	}
	for _, field := range []string{"output_compression", "partial_images"} {
		if value := gjson.GetBytes(rawBody, field); value.Exists() && value.Type == gjson.Number {
			tool, _ = sjson.SetBytes(tool, field, value.Int())
		}
	}
	if maskDataURL != "" {
		tool, _ = sjson.SetBytes(tool, "input_image_mask.image_url", maskDataURL)
	}
	tool = setDefaultImageToolSize(tool, defaultSize)

	responsesBody := buildImagesResponsesRequest(promptForRequest, images, tool)
	h.forwardImagesRequest(c, "/v1/images/edits", imageModel, requestModel, logEffectiveModel, responsesBody, responseFormat, "image_edit", stream)
}

func buildImagesResponsesRequest(prompt string, images []string, toolJSON []byte) []byte {
	req := []byte(`{"instructions":"","stream":true,"reasoning":{"effort":"medium","summary":"auto"},"parallel_tool_calls":true,"include":["reasoning.encrypted_content"],"model":"","store":false,"tool_choice":{"type":"image_generation"}}`)
	req, _ = sjson.SetBytes(req, "model", defaultImagesMainModel)

	input := []byte(`[{"type":"message","role":"user","content":[{"type":"input_text","text":""}]}]`)
	input, _ = sjson.SetBytes(input, "0.content.0.text", prompt)
	contentIndex := 1
	for _, imageURL := range images {
		if strings.TrimSpace(imageURL) == "" {
			continue
		}
		part := []byte(`{"type":"input_image","image_url":""}`)
		part, _ = sjson.SetBytes(part, "image_url", imageURL)
		input, _ = sjson.SetRawBytes(input, fmt.Sprintf("0.content.%d", contentIndex), part)
		contentIndex++
	}
	req, _ = sjson.SetRawBytes(req, "input", input)

	req, _ = sjson.SetRawBytes(req, "tools", []byte(`[]`))
	if len(toolJSON) > 0 && json.Valid(toolJSON) {
		req, _ = sjson.SetRawBytes(req, "tools.-1", toolJSON)
	}
	return req
}

func imagePreferredAccountFilter(account *auth.Account) bool {
	if account == nil {
		return false
	}
	return auth.IsPlusOrHigherPlan(account.GetPlanType())
}

func (h *Handler) nextImageAccount(apiKeyID int64, exclude map[int64]bool, model string) (*auth.Account, string) {
	preferredFilter := h.withModelCooldownFilter(model, imagePreferredAccountFilter)
	account, stickyProxyURL := h.nextAccountForSessionWithFilter("", apiKeyID, exclude, preferredFilter)
	if account != nil {
		return account, stickyProxyURL
	}
	return h.nextAccountForSessionWithFilter("", apiKeyID, exclude, h.withModelCooldownFilter(model, nil))
}

func (h *Handler) forwardImagesRequest(c *gin.Context, inboundEndpoint, requestModel, logModel, logEffectiveModel string, responsesBody []byte, responseFormat, streamPrefix string, stream bool) {
	if strings.TrimSpace(logModel) == "" {
		logModel = requestModel
	}
	if err := validateResponsesImageGenerationSizes(responsesBody); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid request: " + err.Error(), "type": "invalid_request_error"}})
		return
	}

	apiKeyID := requestAPIKeyID(c)
	maxRetries := h.getMaxRetries()
	maxRateLimitRetries := h.getMaxRateLimitRetries()
	generalRetries := 0
	rateLimitRetries := 0
	var lastStatusCode int
	var lastBody []byte
	excludeAccounts := make(map[int64]bool)

	// 仅在 response_format=url 且配置了云存储时启用：上传图片到对象存储、
	// 登记进图库并返回预签名直链。否则 urlFor 为 nil，沿用 base64/data URL。
	persister := h.newImageGalleryPersister(c, responseFormat, requestModel, responsesBody)
	var urlFor imageURLBuilder
	if persister != nil {
		urlFor = persister.buildURL
	}

	for attempt := 0; attempt < maxImageAttempts; attempt++ {
		if err := c.Request.Context().Err(); err != nil {
			return
		}
		account, stickyProxyURL := h.nextImageAccount(apiKeyID, excludeAccounts, requestModel)
		if account == nil {
			account, stickyProxyURL = h.store.WaitForSessionAvailableWithFilter(c.Request.Context(), "", 30*time.Second, apiKeyID, excludeAccounts, h.withModelCooldownFilter(requestModel, nil))
			if account == nil {
				if lastStatusCode == http.StatusTooManyRequests && len(lastBody) > 0 {
					h.sendFinalUpstreamError(c, lastStatusCode, lastBody)
					return
				}
				c.JSON(http.StatusServiceUnavailable, noAvailableAccountError(""))
				return
			}
		}

		start := time.Now()
		proxyURL := h.resolveProxyForAttempt(account, stickyProxyURL)
		apiKey := strings.TrimSpace(strings.TrimPrefix(c.GetHeader("Authorization"), "Bearer "))
		deviceCfg := h.deviceCfg
		if deviceCfg == nil {
			deviceCfg = &DeviceProfileConfig{StabilizeDeviceProfile: false}
		}

		resp, reqErr := ExecuteRequest(c.Request.Context(), account, responsesBody, "", proxyURL, apiKey, deviceCfg, c.Request.Header.Clone(), false)
		durationMs := int(time.Since(start).Milliseconds())
		if reqErr != nil {
			if kind := classifyTransportFailure(reqErr); kind != "" {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			h.store.Release(account)
			excludeAccounts[account.ID()] = true
			if !IsRetryableError(reqErr) && classifyTransportFailure(reqErr) == "" {
				ErrorToGinResponse(c, reqErr)
				return
			}
			if shouldRetryRequestError(reqErr, &generalRetries, maxRetries) {
				continue
			}
			ErrorToGinResponse(c, reqErr)
			return
		}

		if resp.StatusCode != http.StatusOK {
			if kind := classifyHTTPFailure(resp.StatusCode); kind != "" {
				h.store.ReportRequestFailure(account, kind, time.Duration(durationMs)*time.Millisecond)
			}
			SyncCodexUsageState(h.store, account, resp)
			errBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			h.store.Release(account)
			excludeAccounts[account.ID()] = true
			logUpstreamError(inboundEndpoint, resp.StatusCode, logModel, account.ID(), errBody)
			h.logUpstreamCyberPolicy(c, inboundEndpoint, logModel, errBody)
			decision := h.applyCooldownForModel(account, resp.StatusCode, errBody, resp, requestModel)
			shouldRetry := shouldRetryHTTPStatus(resp.StatusCode, &generalRetries, &rateLimitRetries, maxRetries, maxRateLimitRetries)
			h.logUsageForRequest(c, &database.UsageLogInput{
				AccountID:         account.ID(),
				Endpoint:          inboundEndpoint,
				Model:             logModel,
				EffectiveModel:    logEffectiveModel,
				StatusCode:        resp.StatusCode,
				DurationMs:        durationMs,
				InboundEndpoint:   inboundEndpoint,
				UpstreamEndpoint:  "/v1/responses",
				Stream:            stream,
				IsRetryAttempt:    shouldRetry,
				AttemptIndex:      attempt + 1,
				UpstreamErrorKind: upstreamErrorKind(resp.StatusCode, errBody, decision),
				ErrorMessage:      usageLogErrorMessage(resp.StatusCode, errBody),
			})
			if shouldRetry {
				lastStatusCode = resp.StatusCode
				lastBody = errBody
				continue
			}
			h.sendFinalUpstreamError(c, resp.StatusCode, errBody)
			return
		}

		account.Mu().RLock()
		c.Set("x-account-email", account.Email)
		account.Mu().RUnlock()
		c.Set("x-account-proxy", proxyURL)
		c.Set("x-model", logModel)

		var usage *UsageInfo
		var firstTokenMs int
		var imageCount int
		var imageLogInfo imageUsageLogInfo
		var readErr error
		if stream {
			usage, imageCount, firstTokenMs, imageLogInfo, readErr = h.streamImagesResponse(c, resp.Body, responseFormat, streamPrefix, requestModel, start)
		} else {
			var out []byte
			out, usage, imageCount, imageLogInfo, readErr = collectImagesResponse(c.Request.Context(), resp.Body, responseFormat, requestModel, urlFor)
			if readErr == nil {
				persister.finalize(c.Request.Context())
				c.Data(http.StatusOK, "application/json", out)
			} else {
				// Check retryability BEFORE writing error response to avoid
				// double-write when the error is transient.
				resp.Body.Close()
				h.store.Release(account)
				excludeAccounts[account.ID()] = true
				willRetry := shouldRetryImageStreamError(readErr, &generalRetries, maxRetries, attempt, maxImageAttempts)
				// Always record the failed attempt so it appears in usage stats,
				// matching the chat completions error path.
				h.logUsageForRequest(c, buildImageErrorUsageLog(account, inboundEndpoint, logModel, logEffectiveModel, stream, int(time.Since(start).Milliseconds()), attempt, willRetry, readErr, usage, imageLogInfo))
				if willRetry {
					lastStatusCode = http.StatusBadGateway
					lastBody = []byte(readErr.Error())
					continue
				}
				c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": readErr.Error(), "type": "upstream_error"}})
				return
			}
		}

		statusCode := http.StatusOK
		if readErr != nil {
			statusCode = http.StatusBadGateway
			// Retry stream read errors on next account when there are attempts left.
			// Stream disconnects and upstream image generation failures can be
			// transient (e.g. upstream model overload, network hiccup).
			resp.Body.Close()
			h.store.Release(account)
			excludeAccounts[account.ID()] = true
			// Only retry when nothing has been written to the client yet.
			willRetry := shouldRetryImageStreamError(readErr, &generalRetries, maxRetries, attempt, maxImageAttempts) && !c.Writer.Written()
			// Always record the failed attempt so it appears in usage stats.
			h.logUsageForRequest(c, buildImageErrorUsageLog(account, inboundEndpoint, logModel, logEffectiveModel, stream, int(time.Since(start).Milliseconds()), attempt, willRetry, readErr, usage, imageLogInfo))
			if willRetry {
				lastStatusCode = statusCode
				lastBody = []byte(readErr.Error())
				continue
			}
			// Non-retryable -- deliver error response if nothing written yet.
			if !c.Writer.Written() {
				c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": readErr.Error(), "type": "upstream_error"}})
			}
			return
		}
		logInput := &database.UsageLogInput{
			AccountID:        account.ID(),
			Endpoint:         inboundEndpoint,
			Model:            logModel,
			EffectiveModel:   logEffectiveModel,
			StatusCode:       statusCode,
			DurationMs:       int(time.Since(start).Milliseconds()),
			FirstTokenMs:     firstTokenMs,
			InboundEndpoint:  inboundEndpoint,
			UpstreamEndpoint: "/v1/responses",
			Stream:           stream,
		}
		if usage != nil {
			logInput.PromptTokens = usage.PromptTokens
			logInput.CompletionTokens = usage.CompletionTokens
			logInput.TotalTokens = usage.TotalTokens
			logInput.InputTokens = usage.InputTokens
			logInput.OutputTokens = usage.OutputTokens
			logInput.ReasoningTokens = usage.ReasoningTokens
			logInput.CachedTokens = usage.CachedTokens
		}
		if imageCount > 0 && logInput.CompletionTokens == 0 {
			logInput.CompletionTokens = imageCount
			logInput.OutputTokens = imageCount
			logInput.TotalTokens = logInput.PromptTokens + imageCount
		}
		applyImageUsageLogInfo(logInput, imageLogInfo)
		h.logUsageForRequest(c, logInput)

		resp.Body.Close()
		SyncCodexUsageState(h.store, account, resp)
		h.store.ClearModelCooldown(account, requestModel)
		h.store.ReportRequestSuccess(account, time.Duration(logInput.DurationMs)*time.Millisecond)
		h.store.Release(account)
		return
	}
	// Exhausted all attempts.
	if lastStatusCode > 0 && len(lastBody) > 0 {
		h.sendFinalUpstreamError(c, lastStatusCode, lastBody)
		return
	}
	c.JSON(http.StatusServiceUnavailable, noAvailableAccountError(""))
}

// buildImageErrorUsageLog builds a usage log entry for a failed image request
// so that failures -- including retried attempts -- still appear in usage stats.
// Previously the read-error retry paths called continue without logging, so
// failed image requests were silently missing from the statistics.
func buildImageErrorUsageLog(account *auth.Account, inboundEndpoint, logModel, logEffectiveModel string, stream bool, durationMs, attempt int, willRetry bool, readErr error, usage *UsageInfo, imageLogInfo imageUsageLogInfo) *database.UsageLogInput {
	logInput := &database.UsageLogInput{
		AccountID:        account.ID(),
		Endpoint:         inboundEndpoint,
		Model:            logModel,
		EffectiveModel:   logEffectiveModel,
		StatusCode:       http.StatusBadGateway,
		DurationMs:       durationMs,
		InboundEndpoint:  inboundEndpoint,
		UpstreamEndpoint: "/v1/responses",
		Stream:           stream,
		IsRetryAttempt:   willRetry,
		AttemptIndex:     attempt + 1,
		ErrorMessage:     usageLogErrorMessage(http.StatusBadGateway, []byte(readErr.Error())),
	}
	if usage != nil {
		logInput.PromptTokens = usage.PromptTokens
		logInput.CompletionTokens = usage.CompletionTokens
		logInput.TotalTokens = usage.TotalTokens
		logInput.InputTokens = usage.InputTokens
		logInput.OutputTokens = usage.OutputTokens
		logInput.ReasoningTokens = usage.ReasoningTokens
		logInput.CachedTokens = usage.CachedTokens
	}
	applyImageUsageLogInfo(logInput, imageLogInfo)
	return logInput
}

// shouldRetryImageStreamError determines whether an image generation stream
// read error warrants retrying on a different account. Transient failures
// (stream disconnects, upstream model errors) are retryable; permanent
// failures (content policy, invalid request, quota exhausted) are not.
func shouldRetryImageStreamError(err error, generalRetries *int, maxGeneralRetries int, attempt int, maxAttempts int) bool {
	if err == nil || generalRetries == nil || *generalRetries >= maxGeneralRetries {
		return false
	}
	if attempt >= maxAttempts-1 {
		return false
	}
	msg := strings.ToLower(err.Error())
	// Never retry content policy or safety violations.
	for _, keyword := range []string{
		"content_policy", "safety", "cyber_policy",
		"unsupported_country", "invalid_request",
	} {
		if strings.Contains(msg, keyword) {
			return false
		}
	}
	// Retry transient upstream issues.
	*generalRetries++
	return true
}

func collectImagesResponse(ctx context.Context, body io.Reader, responseFormat, fallbackModel string, urlFor imageURLBuilder) ([]byte, *UsageInfo, int, imageUsageLogInfo, error) {
	var (
		out            []byte
		usage          *UsageInfo
		pendingResults []imageCallResult
		createdAt      int64
		firstMeta      = imageCallResult{Model: fallbackModel}
		imageLogInfo   imageUsageLogInfo
		readErr        error
	)
	err := ReadSSEStream(body, func(data []byte) bool {
		if meta, eventCreatedAt, ok := extractImageMetaFromLifecycleEvent(data); ok {
			mergeImageMeta(&firstMeta, meta)
			if eventCreatedAt > 0 {
				createdAt = eventCreatedAt
			}
		}
		switch gjson.GetBytes(data, "type").String() {
		case "response.output_item.done":
			if image, ok := extractImageFromOutputItemDone(data, fallbackModel); ok {
				mergeImageMeta(&image, firstMeta)
				pendingResults = append(pendingResults, image)
			}
		case "response.completed":
			results, completedAt, usageRaw, completedMeta, completedUsage, err := extractImagesFromResponsesCompleted(data, fallbackModel)
			if err != nil {
				readErr = err
				return false
			}
			if completedAt > 0 {
				createdAt = completedAt
			}
			mergeImageMeta(&firstMeta, completedMeta)
			if completedUsage != nil {
				usage = completedUsage
			}
			if len(results) == 0 {
				results = pendingResults
				if len(results) > 0 {
					firstMeta = results[0]
				}
			}
			if len(results) == 0 {
				readErr = fmt.Errorf("upstream did not return image output")
				return false
			}
			out, readErr = buildImagesAPIResponse(ctx, results, createdAt, usageRaw, firstMeta, responseFormat, urlFor)
			imageLogInfo = imageUsageLogInfoFromImages(results)
			return false
		case "error":
			readErr = imageGenerationFailureError(data)
			return false
		case "response.failed":
			readErr = imageGenerationFailureError(data)
			return false
		}
		return true
	})
	if err != nil {
		return nil, usage, 0, imageLogInfo, err
	}
	if readErr != nil {
		return nil, usage, 0, imageLogInfo, readErr
	}
	if len(out) == 0 {
		if len(pendingResults) > 0 {
			for i := range pendingResults {
				mergeImageMeta(&pendingResults[i], firstMeta)
			}
			out, readErr = buildImagesAPIResponse(ctx, pendingResults, createdAt, nil, firstMeta, responseFormat, urlFor)
			if readErr != nil {
				return nil, usage, 0, imageLogInfo, readErr
			}
			imageLogInfo = imageUsageLogInfoFromImages(pendingResults)
			return out, usage, len(gjson.GetBytes(out, "data").Array()), imageLogInfo, nil
		}
		return nil, usage, 0, imageLogInfo, fmt.Errorf("stream disconnected before image generation completed")
	}
	return out, usage, len(gjson.GetBytes(out, "data").Array()), imageLogInfo, nil
}

func (h *Handler) streamImagesResponse(c *gin.Context, body io.Reader, responseFormat, streamPrefix, fallbackModel string, start time.Time) (*UsageInfo, int, int, imageUsageLogInfo, error) {
	c.Header("Content-Type", "text/event-stream")
	c.Header("Cache-Control", "no-cache")
	c.Header("Connection", "keep-alive")
	c.Header("X-Accel-Buffering", "no")

	flusher, ok := c.Writer.(http.Flusher)
	if !ok {
		return nil, 0, 0, imageUsageLogInfo{}, fmt.Errorf("streaming not supported")
	}

	var (
		usage          *UsageInfo
		firstTokenMs   int
		createdAt      int64
		streamMeta     = imageCallResult{Model: fallbackModel}
		pendingResults []imageCallResult
		imageCount     int
		imageLogInfo   imageUsageLogInfo
		readErr        error
	)
	streamWriter := h.newStreamFlushWriter(c.Writer, flusher)
	var (
		writeMu   sync.Mutex
		closeOnce sync.Once
	)
	closeUpstream := func() {
		if closer, ok := body.(io.Closer); ok {
			closeOnce.Do(func() {
				_ = closer.Close()
			})
		}
	}
	getReadErr := func() error {
		writeMu.Lock()
		defer writeMu.Unlock()
		return readErr
	}
	setReadErr := func(err error) {
		if err == nil {
			return
		}
		writeMu.Lock()
		if readErr == nil {
			readErr = err
		}
		writeMu.Unlock()
	}
	writeRaw := func(data string, forceFlush bool) error {
		var err error
		writeMu.Lock()
		if readErr == nil {
			if err = streamWriter.WriteString(data); err == nil && forceFlush {
				err = streamWriter.Flush()
			}
			if err != nil && readErr == nil {
				readErr = err
			}
		} else {
			err = readErr
		}
		writeMu.Unlock()
		if err != nil {
			closeUpstream()
		}
		return err
	}
	writeEvent := func(eventName string, payload []byte) {
		var builder strings.Builder
		if strings.TrimSpace(eventName) != "" {
			builder.WriteString("event: ")
			builder.WriteString(eventName)
			builder.WriteString("\n")
		}
		builder.WriteString("data: ")
		builder.Write(payload)
		builder.WriteString("\n\n")
		_ = writeRaw(builder.String(), true)
	}
	if err := writeRaw(imageStreamConnectedComment, true); err != nil {
		return nil, 0, 0, imageUsageLogInfo{}, err
	}
	stopKeepalive := startImageStreamKeepalive(c.Request.Context(), imageStreamKeepaliveInterval, func() bool {
		return writeRaw(imageStreamKeepaliveComment, true) == nil
	})
	defer stopKeepalive()

	err := ReadSSEStream(body, func(data []byte) bool {
		if getReadErr() != nil {
			return false
		}
		if firstTokenMs == 0 {
			firstTokenMs = int(time.Since(start).Milliseconds())
		}
		if meta, eventCreatedAt, ok := extractImageMetaFromLifecycleEvent(data); ok {
			mergeImageMeta(&streamMeta, meta)
			if eventCreatedAt > 0 {
				createdAt = eventCreatedAt
			}
		}
		switch gjson.GetBytes(data, "type").String() {
		case "response.image_generation_call.partial_image":
			b64 := strings.TrimSpace(gjson.GetBytes(data, "partial_image_b64").String())
			if b64 == "" {
				return true
			}
			partialMeta := streamMeta
			mergeImageMeta(&partialMeta, imageCallResult{
				OutputFormat: strings.TrimSpace(gjson.GetBytes(data, "output_format").String()),
				Background:   strings.TrimSpace(gjson.GetBytes(data, "background").String()),
			})
			eventName := streamPrefix + ".partial_image"
			writeEvent(eventName, buildImagesStreamPartialPayload(eventName, b64, gjson.GetBytes(data, "partial_image_index").Int(), responseFormat, createdAt, partialMeta))
		case "response.output_item.done":
			if image, ok := extractImageFromOutputItemDone(data, fallbackModel); ok {
				mergeImageMeta(&image, streamMeta)
				pendingResults = append(pendingResults, image)
			}
		case "response.completed":
			results, completedAt, usageRaw, firstMeta, completedUsage, err := extractImagesFromResponsesCompleted(data, fallbackModel)
			if err != nil {
				writeEvent("error", buildImagesStreamErrorPayload(err.Error()))
				setReadErr(err)
				return false
			}
			if completedUsage != nil {
				usage = completedUsage
			}
			if completedAt > 0 {
				createdAt = completedAt
			}
			mergeImageMeta(&streamMeta, firstMeta)
			if len(results) == 0 {
				results = pendingResults
			}
			if len(results) == 0 {
				err := fmt.Errorf("upstream did not return image output")
				writeEvent("error", buildImagesStreamErrorPayload(err.Error()))
				setReadErr(err)
				return false
			}
			eventName := streamPrefix + ".completed"
			for _, image := range results {
				mergeImageMeta(&image, streamMeta)
				writeEvent(eventName, buildImagesStreamCompletedPayload(eventName, image, responseFormat, createdAt, usageRaw))
				imageLogInfo = mergeImageUsageLogInfo(imageLogInfo, imageUsageLogInfoFromImage(image))
				imageCount++
			}
			return false
		case "error":
			err := imageGenerationFailureError(data)
			writeEvent("error", buildImagesStreamErrorPayload(err.Error()))
			setReadErr(err)
			return false
		case "response.failed":
			err := imageGenerationFailureError(data)
			writeEvent("error", buildImagesStreamErrorPayload(err.Error()))
			setReadErr(err)
			return false
		}
		return true
	})
	stopKeepalive()
	writeMu.Lock()
	if finalizeErr := streamWriter.Finalize(); finalizeErr != nil && readErr == nil {
		readErr = finalizeErr
	}
	writeMu.Unlock()
	if err != nil {
		if streamErr := getReadErr(); streamErr != nil {
			return usage, imageCount, firstTokenMs, imageLogInfo, streamErr
		}
		return usage, imageCount, firstTokenMs, imageLogInfo, err
	}
	if getReadErr() == nil {
		_ = writeRaw("", true)
	}
	if imageCount == 0 && len(pendingResults) > 0 && getReadErr() == nil {
		eventName := streamPrefix + ".completed"
		for _, image := range pendingResults {
			mergeImageMeta(&image, streamMeta)
			writeEvent(eventName, buildImagesStreamCompletedPayload(eventName, image, responseFormat, createdAt, nil))
			imageLogInfo = mergeImageUsageLogInfo(imageLogInfo, imageUsageLogInfoFromImage(image))
			imageCount++
		}
	}
	if imageCount == 0 && getReadErr() == nil {
		err := fmt.Errorf("stream disconnected before image generation completed")
		writeEvent("error", buildImagesStreamErrorPayload(err.Error()))
		setReadErr(err)
	}
	writeMu.Lock()
	if finalizeErr := streamWriter.Finalize(); finalizeErr != nil && readErr == nil {
		readErr = finalizeErr
	}
	writeMu.Unlock()
	return usage, imageCount, firstTokenMs, imageLogInfo, getReadErr()
}

func startImageStreamKeepalive(ctx context.Context, interval time.Duration, writeKeepalive func() bool) func() {
	if interval <= 0 || writeKeepalive == nil {
		return func() {}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	done := make(chan struct{})
	var stopOnce sync.Once
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if !writeKeepalive() {
					return
				}
			case <-ctx.Done():
				return
			case <-done:
				return
			}
		}
	}()
	return func() {
		stopOnce.Do(func() {
			close(done)
		})
	}
}

func imageGenerationFailureError(payload []byte) error {
	message := firstNonEmptyImageErrorField(
		gjson.GetBytes(payload, "error.message").String(),
		gjson.GetBytes(payload, "response.error.message").String(),
		gjson.GetBytes(payload, "error").String(),
	)
	code := firstNonEmptyImageErrorField(
		gjson.GetBytes(payload, "error.code").String(),
		gjson.GetBytes(payload, "response.error.code").String(),
		gjson.GetBytes(payload, "error.type").String(),
		gjson.GetBytes(payload, "response.error.type").String(),
	)
	if message == "" {
		message = "upstream image generation failed"
	}
	if code != "" && !strings.Contains(strings.ToLower(message), strings.ToLower(code)) {
		return fmt.Errorf("upstream image generation failed (%s): %s", code, message)
	}
	return fmt.Errorf("upstream image generation failed: %s", message)
}

func firstNonEmptyImageErrorField(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

func extractImagesFromResponsesCompleted(payload []byte, fallbackModel string) ([]imageCallResult, int64, []byte, imageCallResult, *UsageInfo, error) {
	if gjson.GetBytes(payload, "type").String() != "response.completed" {
		return nil, 0, nil, imageCallResult{}, nil, fmt.Errorf("unexpected event type")
	}

	createdAt := gjson.GetBytes(payload, "response.created_at").Int()
	if createdAt <= 0 {
		createdAt = time.Now().Unix()
	}

	results := make([]imageCallResult, 0)
	firstMeta := imageCallResult{Model: fallbackModel}
	if meta, _, ok := extractImageMetaFromLifecycleEvent(payload); ok {
		mergeImageMeta(&firstMeta, meta)
	}
	if output := gjson.GetBytes(payload, "response.output"); output.IsArray() {
		for _, item := range output.Array() {
			if item.Get("type").String() != "image_generation_call" {
				continue
			}
			result := strings.TrimSpace(item.Get("result").String())
			if result == "" {
				continue
			}
			image := imageCallResult{
				Result:        result,
				RevisedPrompt: strings.TrimSpace(item.Get("revised_prompt").String()),
				OutputFormat:  strings.TrimSpace(item.Get("output_format").String()),
				Size:          strings.TrimSpace(item.Get("size").String()),
				ByteSize:      int(item.Get("bytes").Int()),
				Width:         int(item.Get("width").Int()),
				Height:        int(item.Get("height").Int()),
				Background:    strings.TrimSpace(item.Get("background").String()),
				Quality:       strings.TrimSpace(item.Get("quality").String()),
				Model:         fallbackModel,
			}
			populateImageStats(&image)
			mergeImageMeta(&image, firstMeta)
			if len(results) == 0 {
				firstMeta = image
			}
			results = append(results, image)
		}
	}

	var usageRaw []byte
	if usage := gjson.GetBytes(payload, "response.tool_usage.image_gen"); usage.Exists() && usage.IsObject() {
		usageRaw = []byte(usage.Raw)
	}
	usage := extractUsageFromResult(gjson.GetBytes(payload, "response.usage"))
	if len(usageRaw) > 0 {
		if imageUsage := extractUsageFromResult(gjson.ParseBytes(usageRaw)); hasTokenUsage(imageUsage) {
			usage = imageUsage
		}
	}
	return results, createdAt, usageRaw, firstMeta, usage, nil
}

func hasTokenUsage(usage *UsageInfo) bool {
	return usage != nil && (usage.InputTokens > 0 || usage.OutputTokens > 0 || usage.TotalTokens > 0)
}

func extractImageFromOutputItemDone(payload []byte, fallbackModel string) (imageCallResult, bool) {
	if gjson.GetBytes(payload, "type").String() != "response.output_item.done" {
		return imageCallResult{}, false
	}
	item := gjson.GetBytes(payload, "item")
	if !item.Exists() || item.Get("type").String() != "image_generation_call" {
		return imageCallResult{}, false
	}
	result := strings.TrimSpace(item.Get("result").String())
	if result == "" {
		return imageCallResult{}, false
	}
	image := imageCallResult{
		Result:        result,
		RevisedPrompt: strings.TrimSpace(item.Get("revised_prompt").String()),
		OutputFormat:  strings.TrimSpace(item.Get("output_format").String()),
		Size:          strings.TrimSpace(item.Get("size").String()),
		ByteSize:      int(item.Get("bytes").Int()),
		Width:         int(item.Get("width").Int()),
		Height:        int(item.Get("height").Int()),
		Background:    strings.TrimSpace(item.Get("background").String()),
		Quality:       strings.TrimSpace(item.Get("quality").String()),
		Model:         fallbackModel,
	}
	populateImageStats(&image)
	return image, true
}

func extractImageMetaFromLifecycleEvent(payload []byte) (imageCallResult, int64, bool) {
	response := gjson.GetBytes(payload, "response")
	if !response.Exists() {
		return imageCallResult{}, 0, false
	}
	meta := imageCallResult{
		OutputFormat: strings.TrimSpace(response.Get("tools.0.output_format").String()),
		Size:         strings.TrimSpace(response.Get("tools.0.size").String()),
		Background:   strings.TrimSpace(response.Get("tools.0.background").String()),
		Quality:      strings.TrimSpace(response.Get("tools.0.quality").String()),
		Model:        strings.TrimSpace(response.Get("tools.0.model").String()),
	}
	return meta, response.Get("created_at").Int(), true
}

func mergeImageMeta(target *imageCallResult, source imageCallResult) {
	if target == nil {
		return
	}
	if target.OutputFormat == "" {
		target.OutputFormat = source.OutputFormat
	}
	if target.Size == "" {
		target.Size = source.Size
	}
	if target.ByteSize == 0 {
		target.ByteSize = source.ByteSize
	}
	if target.Width == 0 {
		target.Width = source.Width
	}
	if target.Height == 0 {
		target.Height = source.Height
	}
	if target.Background == "" {
		target.Background = source.Background
	}
	if target.Quality == "" {
		target.Quality = source.Quality
	}
	if target.Model == "" {
		target.Model = source.Model
	}
}

// imageURLBuilder 接收一张生成图，返回其托管直链。返回 ok=false 表示
// 应回退到 base64 data URL。为 nil 时表示未启用云存储直链。
type imageURLBuilder func(ctx context.Context, image imageCallResult, idx int) (string, bool)

func buildImagesAPIResponse(ctx context.Context, results []imageCallResult, createdAt int64, usageRaw []byte, firstMeta imageCallResult, responseFormat string, urlFor imageURLBuilder) ([]byte, error) {
	if createdAt <= 0 {
		createdAt = time.Now().Unix()
	}
	out := []byte(`{"created":0,"data":[]}`)
	out, _ = sjson.SetBytes(out, "created", createdAt)

	format := strings.ToLower(strings.TrimSpace(responseFormat))
	if format == "" {
		format = "b64_json"
	}
	for idx, image := range results {
		populateImageStats(&image)
		item := []byte(`{}`)
		if format == "url" {
			// 已配置云存储直链时上传并返回托管 URL；失败或未配置则回退到 data URL。
			if url, ok := buildImageURL(ctx, urlFor, image, idx); ok {
				item, _ = sjson.SetBytes(item, "url", url)
			} else {
				item, _ = sjson.SetBytes(item, "url", "data:"+mimeTypeFromOutputFormat(image.OutputFormat)+";base64,"+image.Result)
			}
		} else {
			item, _ = sjson.SetBytes(item, "b64_json", image.Result)
		}
		if image.ByteSize > 0 {
			item, _ = sjson.SetBytes(item, "bytes", image.ByteSize)
		}
		if image.Width > 0 {
			item, _ = sjson.SetBytes(item, "width", image.Width)
		}
		if image.Height > 0 {
			item, _ = sjson.SetBytes(item, "height", image.Height)
		}
		if image.RevisedPrompt != "" {
			item, _ = sjson.SetBytes(item, "revised_prompt", image.RevisedPrompt)
		}
		out, _ = sjson.SetRawBytes(out, "data.-1", item)
	}
	if firstMeta.Background != "" {
		out, _ = sjson.SetBytes(out, "background", firstMeta.Background)
	}
	if firstMeta.OutputFormat != "" {
		out, _ = sjson.SetBytes(out, "output_format", firstMeta.OutputFormat)
	}
	if firstMeta.Quality != "" {
		out, _ = sjson.SetBytes(out, "quality", firstMeta.Quality)
	}
	if firstMeta.Size != "" {
		out, _ = sjson.SetBytes(out, "size", firstMeta.Size)
	}
	if firstMeta.Model != "" {
		out, _ = sjson.SetBytes(out, "model", firstMeta.Model)
	}
	if len(usageRaw) > 0 && json.Valid(usageRaw) {
		out, _ = sjson.SetRawBytes(out, "usage", usageRaw)
	}
	return out, nil
}

// imageStorageIsCloud 报告当前图片存储后端是否为云端（S3 兼容）对象存储。
func imageStorageIsCloud() bool {
	return imagestore.CurrentConfig().Backend == imagestore.BackendS3
}

// cloudUploadImage 把单张生成图上传到已配置的云存储，返回存储 ref 与限时预签名直链。
//
// 任一步失败都返回 ok=false，由调用方回退到 base64/data URL，
// 确保 API 不会因为对象存储配置或网络问题而整体失败。
func cloudUploadImage(ctx context.Context, image imageCallResult, idx int) (ref, url string, ok bool) {
	data, decoded := decodeImageBase64(image.Result)
	if !decoded || len(data) == 0 {
		return "", "", false
	}
	backend, err := imagestore.Primary()
	if err != nil {
		log.Printf("[images] 云存储未初始化，回退 base64: %v", err)
		return "", "", false
	}
	mimeType := mimeTypeFromOutputFormat(image.OutputFormat)
	key := fmt.Sprintf("api/%d-%02d-%s.%s", time.Now().UnixNano(), idx+1, uuid.NewString()[:8], imageExtFromOutputFormat(image.OutputFormat))
	ref, err = backend.Save(ctx, key, data, mimeType)
	if err != nil {
		log.Printf("[images] 上传云存储失败，回退 base64: %v", err)
		return "", "", false
	}
	url, err = imagestore.PresignURL(ctx, ref, imageCloudURLTTL)
	if err != nil {
		log.Printf("[images] 生成预签名直链失败，回退 base64: %v", err)
		return "", "", false
	}
	return ref, url, true
}

// cloudImageURLOnly 仅上传 + 预签名，不写图库。供未接入 DB 的场景（如单测）使用。
func cloudImageURLOnly(ctx context.Context, image imageCallResult, idx int) (string, bool) {
	_, url, ok := cloudUploadImage(ctx, image, idx)
	return url, ok
}

// buildImageURL 执行注入的 url 构造回调；回调为 nil 时返回 ok=false（回退 data URL）。
func buildImageURL(ctx context.Context, urlFor imageURLBuilder, image imageCallResult, idx int) (string, bool) {
	if urlFor == nil {
		return "", false
	}
	return urlFor(ctx, image, idx)
}

// imageExtFromOutputFormat 把 output_format 归一为对象 key 用的文件扩展名。
func imageExtFromOutputFormat(outputFormat string) string {
	switch strings.ToLower(strings.TrimSpace(outputFormat)) {
	case "jpg", "jpeg", "image/jpeg":
		return "jpg"
	case "webp", "image/webp":
		return "webp"
	default:
		return "png"
	}
}

// imageGalleryPersister 在 response_format=url 且配置了云存储时，把每张生成图
// 上传到对象存储、登记进图库（懒创建 synthetic job + asset 记录），并返回预签名直链。
//
// 这样 API 生成的图与后台 Image Studio 生成的图共用同一套图库展示与删除逻辑：
// 管理员可在图库中看到它们，删除时会级联删掉数据库记录与云端对象，不再无主堆积。
type imageGalleryPersister struct {
	h            *Handler
	prompt       string
	paramsJSON   string
	apiKeyID     int64
	apiKeyName   string
	apiKeyMasked string
	model        string
	start        time.Time

	jobID        int64 // 懒创建：第一张图上传成功时才建 job；0 表示尚未/无法创建
	jobAttempted bool
	saved        int
}

// newImageGalleryPersister 在 response_format=url 且已配置云存储时构造一个 persister，
// 否则返回 nil（调用方据此回退到 data URL / base64）。
func (h *Handler) newImageGalleryPersister(c *gin.Context, responseFormat, model string, responsesBody []byte) *imageGalleryPersister {
	if !strings.EqualFold(strings.TrimSpace(responseFormat), "url") || !imageStorageIsCloud() {
		return nil
	}
	prompt := gjson.GetBytes(responsesBody, "input.0.content.0.text").String()
	paramsJSON := "{}"
	if tool := gjson.GetBytes(responsesBody, "tools.0"); tool.Exists() {
		paramsJSON = tool.Raw
	}
	p := &imageGalleryPersister{
		h:          h,
		prompt:     prompt,
		paramsJSON: paramsJSON,
		apiKeyID:   requestAPIKeyID(c),
		model:      model,
		start:      time.Now(),
	}
	if v, ok := c.Get(contextAPIKeyName); ok {
		if name, ok := v.(string); ok {
			p.apiKeyName = name
		}
	}
	if v, ok := c.Get(contextAPIKeyMasked); ok {
		if masked, ok := v.(string); ok {
			p.apiKeyMasked = masked
		}
	}
	return p
}

// buildURL 实现注入 buildImagesAPIResponse 的回调：上传 + 登记图库，返回直链。
func (p *imageGalleryPersister) buildURL(ctx context.Context, image imageCallResult, idx int) (string, bool) {
	ref, url, ok := cloudUploadImage(ctx, image, idx)
	if !ok {
		return "", false
	}
	p.recordAsset(ctx, image, ref)
	return url, true
}

// recordAsset 尽力把已上传对象登记进图库（job + asset）。失败时删除已上传对象，
// 保持「每个云端对象都有数据库记录」的不变式，避免产生无主文件。
func (p *imageGalleryPersister) recordAsset(ctx context.Context, image imageCallResult, ref string) {
	if p == nil || p.h == nil || p.h.db == nil {
		return
	}
	jobID := p.ensureJob(ctx)
	populateImageStats(&image)
	input := database.ImageAssetInput{
		JobID:         jobID,
		Filename:      ref,
		StoragePath:   ref,
		MimeType:      mimeTypeFromOutputFormat(image.OutputFormat),
		Bytes:         image.ByteSize,
		Width:         image.Width,
		Height:        image.Height,
		Model:         firstNonEmptyImageStr(image.Model, p.model),
		RequestedSize: image.Size,
		ActualSize:    imageActualSize(image.Width, image.Height),
		Quality:       image.Quality,
		OutputFormat:  image.OutputFormat,
		RevisedPrompt: image.RevisedPrompt,
	}
	if _, err := p.h.db.InsertImageAsset(ctx, input); err != nil {
		log.Printf("[images] 登记图库 asset 失败，删除已上传对象避免无主: %v", err)
		if backend, rerr := imagestore.Resolve(ref); rerr == nil {
			_ = backend.Delete(ctx, ref)
		}
		return
	}
	p.saved++
}

// ensureJob 懒创建一条 synthetic job，返回 job_id；创建失败时返回 0（asset 仍可见于图库平铺视图）。
func (p *imageGalleryPersister) ensureJob(ctx context.Context) int64 {
	if p.jobAttempted {
		return p.jobID
	}
	p.jobAttempted = true
	id, err := p.h.db.InsertImageGenerationJob(ctx, database.ImageGenerationJobInput{
		Prompt:       p.prompt,
		ParamsJSON:   p.paramsJSON,
		APIKeyID:     p.apiKeyID,
		APIKeyName:   p.apiKeyName,
		APIKeyMasked: p.apiKeyMasked,
	})
	if err != nil {
		log.Printf("[images] 创建图库 job 失败，asset 将以 job_id=0 登记: %v", err)
		return 0
	}
	p.jobID = id
	return id
}

// finalize 把 synthetic job 标记为成功（含耗时）。无 job 或一张都没存成时跳过。
func (p *imageGalleryPersister) finalize(ctx context.Context) {
	if p == nil || p.h == nil || p.h.db == nil || p.jobID == 0 || p.saved == 0 {
		return
	}
	durationMs := int(time.Since(p.start).Milliseconds())
	if err := p.h.db.MarkImageJobSucceeded(ctx, p.jobID, durationMs); err != nil {
		log.Printf("[images] 标记图库 job 成功失败: %v", err)
	}
}

func firstNonEmptyImageStr(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func imageActualSize(width, height int) string {
	if width > 0 && height > 0 {
		return fmt.Sprintf("%dx%d", width, height)
	}
	return ""
}

func buildImagesStreamPartialPayload(eventType, b64 string, partialImageIndex int64, responseFormat string, createdAt int64, meta imageCallResult) []byte {
	if createdAt <= 0 {
		createdAt = time.Now().Unix()
	}
	payload := []byte(`{"type":"","created_at":0,"partial_image_index":0,"b64_json":""}`)
	payload, _ = sjson.SetBytes(payload, "type", eventType)
	payload, _ = sjson.SetBytes(payload, "created_at", createdAt)
	payload, _ = sjson.SetBytes(payload, "partial_image_index", partialImageIndex)
	payload, _ = sjson.SetBytes(payload, "b64_json", b64)
	if stats, ok := imageStatsFromBase64(b64); ok {
		if stats.ByteSize > 0 {
			payload, _ = sjson.SetBytes(payload, "bytes", stats.ByteSize)
		}
		if stats.Width > 0 {
			payload, _ = sjson.SetBytes(payload, "width", stats.Width)
		}
		if stats.Height > 0 {
			payload, _ = sjson.SetBytes(payload, "height", stats.Height)
		}
	}
	if strings.EqualFold(strings.TrimSpace(responseFormat), "url") {
		payload, _ = sjson.SetBytes(payload, "url", "data:"+mimeTypeFromOutputFormat(meta.OutputFormat)+";base64,"+b64)
	}
	return addImageMetaToPayload(payload, meta)
}

func buildImagesStreamCompletedPayload(eventType string, image imageCallResult, responseFormat string, createdAt int64, usageRaw []byte) []byte {
	if createdAt <= 0 {
		createdAt = time.Now().Unix()
	}
	payload := []byte(`{"type":"","created_at":0,"b64_json":""}`)
	payload, _ = sjson.SetBytes(payload, "type", eventType)
	payload, _ = sjson.SetBytes(payload, "created_at", createdAt)
	payload, _ = sjson.SetBytes(payload, "b64_json", image.Result)
	populateImageStats(&image)
	if strings.EqualFold(strings.TrimSpace(responseFormat), "url") {
		payload, _ = sjson.SetBytes(payload, "url", "data:"+mimeTypeFromOutputFormat(image.OutputFormat)+";base64,"+image.Result)
	}
	payload = addImageMetaToPayload(payload, image)
	if len(usageRaw) > 0 && json.Valid(usageRaw) {
		payload, _ = sjson.SetRawBytes(payload, "usage", usageRaw)
	}
	return payload
}

func addImageMetaToPayload(payload []byte, meta imageCallResult) []byte {
	if meta.Background != "" {
		payload, _ = sjson.SetBytes(payload, "background", meta.Background)
	}
	if meta.OutputFormat != "" {
		payload, _ = sjson.SetBytes(payload, "output_format", meta.OutputFormat)
	}
	if meta.Quality != "" {
		payload, _ = sjson.SetBytes(payload, "quality", meta.Quality)
	}
	if meta.Size != "" {
		payload, _ = sjson.SetBytes(payload, "size", meta.Size)
	}
	if meta.ByteSize > 0 {
		payload, _ = sjson.SetBytes(payload, "bytes", meta.ByteSize)
	}
	if meta.Width > 0 {
		payload, _ = sjson.SetBytes(payload, "width", meta.Width)
	}
	if meta.Height > 0 {
		payload, _ = sjson.SetBytes(payload, "height", meta.Height)
	}
	if meta.Model != "" {
		payload, _ = sjson.SetBytes(payload, "model", meta.Model)
	}
	return payload
}

func buildImagesStreamErrorPayload(message string) []byte {
	payload := []byte(`{"error":{"message":"","type":"upstream_error"}}`)
	payload, _ = sjson.SetBytes(payload, "error.message", message)
	return payload
}
