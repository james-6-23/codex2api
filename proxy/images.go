package proxy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"mime/multipart"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
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
	MaxImageEditInputCount = 10
)

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
	rawBody, err := io.ReadAll(c.Request.Body)
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
	if imageModel == "" {
		imageModel = defaultImagesToolModel
	}
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
	h.forwardImagesRequest(c, "/v1/images/generations", imageModel, responsesBody, responseFormat, "image_generation", stream)
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
	if imageModel == "" {
		imageModel = defaultImagesToolModel
	}
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
	tool := buildImagesEditToolFromForm(c, imageModel, maskDataURL)
	responsesBody := buildImagesResponsesRequest(promptForRequest, images, tool)
	h.forwardImagesRequest(c, "/v1/images/edits", imageModel, responsesBody, responseFormat, "image_edit", stream)
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
	rawBody, err := io.ReadAll(c.Request.Body)
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
	if imageModel == "" {
		imageModel = defaultImagesToolModel
	}
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
	h.forwardImagesRequest(c, "/v1/images/edits", imageModel, responsesBody, responseFormat, "image_edit", stream)
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

func (h *Handler) forwardImagesRequest(c *gin.Context, inboundEndpoint, requestModel string, responsesBody []byte, responseFormat, streamPrefix string, stream bool) {
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
	streamStarted := false

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

		resp, reqErr := ExecuteRequest(c.Request.Context(), account, responsesBody, "", proxyURL, apiKey, deviceCfg, c.Request.Header.Clone(), h.shouldUseWebsocketForHTTP())
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
			if usagePct, ok := parseCodexUsageHeaders(resp, account); ok {
				h.store.PersistUsageSnapshot(account, usagePct)
			}
			errBody, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			h.store.Release(account)
			excludeAccounts[account.ID()] = true
			logUpstreamError(inboundEndpoint, resp.StatusCode, requestModel, account.ID(), errBody)
			h.logUpstreamCyberPolicy(c, inboundEndpoint, requestModel, errBody)
			decision := h.applyCooldownForModel(account, resp.StatusCode, errBody, resp, requestModel)
			shouldRetry := shouldRetryHTTPStatus(resp.StatusCode, &generalRetries, &rateLimitRetries, maxRetries, maxRateLimitRetries)
			h.logUsageForRequest(c, &database.UsageLogInput{
				AccountID:         account.ID(),
				Endpoint:          inboundEndpoint,
				Model:             requestModel,
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
		c.Set("x-model", requestModel)

		var usage *UsageInfo
		var firstTokenMs int
		var imageCount int
		var imageLogInfo imageUsageLogInfo
		var readErr error
		if stream {
				streamStarted = true
			usage, imageCount, firstTokenMs, imageLogInfo, readErr = h.streamImagesResponse(c, resp.Body, responseFormat, streamPrefix, requestModel, start)
		} else {
			var out []byte
			out, usage, imageCount, imageLogInfo, readErr = collectImagesResponse(resp.Body, responseFormat, requestModel)
			if readErr == nil {
				c.Data(http.StatusOK, "application/json", out)
			} else {
				// Check retryability BEFORE writing error response to avoid
				// double-write when the error is transient.
				resp.Body.Close()
				h.store.Release(account)
				excludeAccounts[account.ID()] = true
				if shouldRetryImageStreamError(readErr, &generalRetries, maxRetries, attempt, maxImageAttempts) {
					lastStatusCode = http.StatusBadGateway
					lastBody = []byte(readErr.Error())
					continue
				}
				c.JSON(http.StatusBadGateway, gin.H{"error": gin.H{"message": readErr.Error(), "type": "upstream_error"}})
				h.logUsageForRequest(c, &database.UsageLogInput{
					AccountID: account.ID(),
					Endpoint:  inboundEndpoint,
					Model:     requestModel,
					StatusCode: http.StatusBadGateway,
				})
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
			if shouldRetryImageStreamError(readErr, &generalRetries, maxRetries, attempt, maxImageAttempts) {
				lastStatusCode = statusCode
				lastBody = []byte(readErr.Error())
				if !streamStarted {
					continue
				}
			}
			// Non-retryable -- deliver error response already written above.
			h.logUsageForRequest(c, &database.UsageLogInput{
				AccountID: account.ID(),
				Endpoint:  inboundEndpoint,
				Model:     requestModel,
				StatusCode: http.StatusBadGateway,
			})
			return
		}
		logInput := &database.UsageLogInput{
			AccountID:        account.ID(),
			Endpoint:         inboundEndpoint,
			Model:            requestModel,
			StatusCode:       statusCode,
			DurationMs:       int(time.Since(start).Milliseconds()),
			FirstTokenMs:     firstTokenMs,
			InboundEndpoint:  inboundEndpoint,
			UpstreamEndpoint: "/v1/responses",
			Stream:           stream,
		}
		if readErr != nil {
			logInput.ErrorMessage = usageLogErrorMessage(statusCode, []byte(readErr.Error()))
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
		if readErr != nil {
			h.store.ReportRequestFailure(account, "transport", time.Duration(logInput.DurationMs)*time.Millisecond)
		} else {
			h.store.ClearModelCooldown(account, requestModel)
			h.store.ReportRequestSuccess(account, time.Duration(logInput.DurationMs)*time.Millisecond)
		}
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

func collectImagesResponse(body io.Reader, responseFormat, fallbackModel string) ([]byte, *UsageInfo, int, imageUsageLogInfo, error) {
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
			out, readErr = buildImagesAPIResponse(results, createdAt, usageRaw, firstMeta, responseFormat)
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
			out, readErr = buildImagesAPIResponse(pendingResults, createdAt, nil, firstMeta, responseFormat)
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
	streamWriter := newStreamFlushWriter(c.Writer, flusher)
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
		if err := streamWriter.WriteString(builder.String()); err != nil && readErr == nil {
			readErr = err
		}
	}

	err := ReadSSEStream(body, func(data []byte) bool {
		if readErr != nil {
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
				readErr = err
				writeEvent("error", buildImagesStreamErrorPayload(err.Error()))
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
				readErr = fmt.Errorf("upstream did not return image output")
				writeEvent("error", buildImagesStreamErrorPayload(readErr.Error()))
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
			readErr = imageGenerationFailureError(data)
			writeEvent("error", buildImagesStreamErrorPayload(readErr.Error()))
			return false
		case "response.failed":
			readErr = imageGenerationFailureError(data)
			writeEvent("error", buildImagesStreamErrorPayload(readErr.Error()))
			return false
		}
		return true
	})
	if err != nil {
		return usage, imageCount, firstTokenMs, imageLogInfo, err
	}
	if readErr == nil {
		readErr = streamWriter.Flush()
	}
	if imageCount == 0 && len(pendingResults) > 0 && readErr == nil {
		eventName := streamPrefix + ".completed"
		for _, image := range pendingResults {
			mergeImageMeta(&image, streamMeta)
			writeEvent(eventName, buildImagesStreamCompletedPayload(eventName, image, responseFormat, createdAt, nil))
			imageLogInfo = mergeImageUsageLogInfo(imageLogInfo, imageUsageLogInfoFromImage(image))
			imageCount++
		}
	}
	if imageCount == 0 && readErr == nil {
		readErr = fmt.Errorf("stream disconnected before image generation completed")
		writeEvent("error", buildImagesStreamErrorPayload(readErr.Error()))
	}
	return usage, imageCount, firstTokenMs, imageLogInfo, readErr
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

func buildImagesAPIResponse(results []imageCallResult, createdAt int64, usageRaw []byte, firstMeta imageCallResult, responseFormat string) ([]byte, error) {
	if createdAt <= 0 {
		createdAt = time.Now().Unix()
	}
	out := []byte(`{"created":0,"data":[]}`)
	out, _ = sjson.SetBytes(out, "created", createdAt)

	format := strings.ToLower(strings.TrimSpace(responseFormat))
	if format == "" {
		format = "b64_json"
	}
	for _, image := range results {
		populateImageStats(&image)
		item := []byte(`{}`)
		if format == "url" {
			item, _ = sjson.SetBytes(item, "url", "data:"+mimeTypeFromOutputFormat(image.OutputFormat)+";base64,"+image.Result)
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
