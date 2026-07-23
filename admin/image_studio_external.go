package admin

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/internal/imageproc"
	"github.com/codex2api/proxy"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

// RegisterExternalImageRoutes registers API-key authenticated async Image Studio
// task endpoints for external clients.
func (h *Handler) RegisterExternalImageRoutes(r *gin.Engine, imageProxy *proxy.Handler) {
	if h == nil || r == nil || imageProxy == nil {
		return
	}
	h.imageProxy = imageProxy
	v1 := r.Group("/v1")
	v1.Use(imageProxy.APIKeyAuthMiddleware())
	v1.POST("/images/jobs", h.CreateExternalImageJob)
	v1.GET("/images/jobs/:id", h.GetExternalImageJob)
}

func (h *Handler) CreateExternalImageJob(c *gin.Context) {
	var req imageGenerationJobPayload
	if err := c.ShouldBindBodyWithJSON(&req); err != nil {
		writeExternalImageError(c, http.StatusBadRequest, "Invalid request: body must be valid JSON")
		return
	}
	bodyValue, _ := c.Get(gin.BodyBytesKey)
	rawBody, _ := bodyValue.([]byte)
	if len(rawBody) == 0 {
		rawBody, _ = json.Marshal(req)
	}
	editMode, err := normalizeExternalImageJobFields(&req)
	if err != nil {
		writeExternalImageError(c, http.StatusBadRequest, "Invalid request: "+err.Error())
		return
	}
	req.External = true

	apiKey := proxy.APIKeyRowFromContext(c)
	if apiKey == nil {
		writeExternalImageError(c, http.StatusUnauthorized, "Missing or invalid API key")
		return
	}
	req.APIKeyID = apiKey.ID
	keyID, keyName, keyMasked := imageJobAPIKeyMeta(apiKey)
	imageProxy := h.imageProxy
	if imageProxy == nil {
		imageProxy = proxy.NewHandler(h.store, h.db, nil, nil)
	}
	if imageProxy.InspectPromptFilterOpenAI(c, rawBody, "/v1/images/jobs", req.Model, func(c *gin.Context) {
		writeExternalImageError(c, http.StatusBadRequest, "Prompt was blocked by prompt filter")
	}) {
		return
	}

	// Remote image fetches and Base64 expansion are intentionally after prompt
	// inspection. A blocked prompt must not consume network, image-processing,
	// database, concurrency, or upstream resources.
	imageCount := len(req.InputImages)
	if imageCount < 1 {
		imageCount = 1
	}
	normalizeCtx, cancel := context.WithTimeout(c.Request.Context(), externalInputImageFetchTimeout*time.Duration(imageCount))
	err = normalizeExternalImageJobInputs(normalizeCtx, &req)
	cancel()
	if err != nil {
		writeExternalImageError(c, http.StatusBadRequest, "Invalid request: "+err.Error())
		return
	}

	// The in-process image handler intentionally does not run the /v1 auth
	// middleware again. Enforce API-key limits and hold the concurrency slot here
	// from enqueue through background completion so async jobs match /v1 policy
	// without double-counting the same request.
	if status, msg := imageProxy.EnforceAPIKeyLimits(c, req.Model); status != 0 {
		proxy.SendAPIKeyLimitError(c, status, msg)
		return
	}
	releaseAPIKeyConcurrency, ok := imageProxy.AcquireAPIKeyConcurrency(c)
	if !ok {
		return
	}
	jobStarted := false
	defer func() {
		if !jobStarted && releaseAPIKeyConcurrency != nil {
			releaseAPIKeyConcurrency()
		}
	}()

	paramsJSON, _ := json.Marshal(req)
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	jobID, err := h.db.InsertImageGenerationJob(ctx, database.ImageGenerationJobInput{
		Prompt:       req.Prompt,
		ParamsJSON:   string(paramsJSON),
		APIKeyID:     keyID,
		APIKeyName:   keyName,
		APIKeyMasked: keyMasked,
	})
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if req.TemplateID > 0 {
		_ = h.db.IncrementImagePromptTemplateUsage(ctx, req.TemplateID)
	}
	job, err := h.db.GetImageGenerationJob(ctx, jobID)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	log.Printf("[image-studio] external job=%d queued mode=%s model=%s size=%s quality=%s format=%s api_key=%s prompt_chars=%d",
		jobID,
		externalImageJobMode(editMode),
		imageLogValue(req.Model),
		imageLogValue(req.Size),
		imageLogValue(req.Quality),
		imageLogValue(req.OutputFormat),
		imageLogAPIKeyLabel(keyID, keyName, keyMasked),
		len([]rune(req.Prompt)),
	)
	jobStarted = true
	go func() {
		if releaseAPIKeyConcurrency != nil {
			defer releaseAPIKeyConcurrency()
		}
		if editMode {
			h.runImageEditJob(jobID, req, apiKey)
			return
		}
		h.runImageGenerationJob(jobID, req, apiKey)
	}()
	c.JSON(http.StatusAccepted, externalImageJobResponse{Job: job})
}

func (h *Handler) GetExternalImageJob(c *gin.Context) {
	apiKey := proxy.APIKeyRowFromContext(c)
	if apiKey == nil {
		writeExternalImageError(c, http.StatusUnauthorized, "Missing or invalid API key")
		return
	}
	id, err := parsePositiveIDParam(c, "id")
	if err != nil {
		writeExternalImageError(c, http.StatusBadRequest, "Invalid request: invalid job id")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	job, err := h.db.GetImageGenerationJob(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		writeExternalImageError(c, http.StatusNotFound, "Image job not found")
		return
	}
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if job.APIKeyID != apiKey.ID {
		writeExternalImageError(c, http.StatusNotFound, "Image job not found")
		return
	}
	if c.Query("include_cache") == "1" {
		h.attachImageJobAssetCachePayload(job)
	}
	decorateImageJobAssets(job)
	c.JSON(http.StatusOK, externalImageJobResponse{Job: job})
}

func normalizeExternalImageJobFields(req *imageGenerationJobPayload) (bool, error) {
	if req == nil {
		return false, fmt.Errorf("body is required")
	}
	req.Prompt = strings.TrimSpace(req.Prompt)
	if req.Prompt == "" {
		return false, fmt.Errorf("prompt is required")
	}
	if len([]rune(req.Prompt)) > 8000 {
		return false, fmt.Errorf("prompt must be at most 8000 characters")
	}
	req.Model = normalizeImageStudioModel(req.Model)
	if req.Model == "" {
		req.Model = "gpt-image-2"
	}
	req.Size = normalizeOptionalImageParam(req.Size)
	req.Quality = normalizeOptionalImageParam(req.Quality)
	req.OutputFormat = normalizeOptionalImageParam(req.OutputFormat)
	if req.OutputFormat == "" {
		req.OutputFormat = "png"
	}
	req.Background = normalizeOptionalImageParam(req.Background)
	req.Style = normalizeOptionalImageParam(req.Style)
	req.Upscale = imageproc.NormalizeUpscale(req.Upscale)

	images := make([]string, 0, len(req.InputImages))
	for _, imageURL := range req.InputImages {
		imageURL = strings.TrimSpace(imageURL)
		if imageURL == "" {
			continue
		}
		images = append(images, imageURL)
	}
	req.InputImages = images
	editMode := len(req.InputImages) > 0
	if len(req.InputImages) > proxy.MaxImageEditInputCount {
		return false, fmt.Errorf("too many input_images (%d, max %d)", len(req.InputImages), proxy.MaxImageEditInputCount)
	}
	return editMode, nil
}

func normalizeExternalImageJobInputs(ctx context.Context, req *imageGenerationJobPayload) error {
	if req == nil {
		return fmt.Errorf("body is required")
	}
	images := make([]string, 0, len(req.InputImages))
	for _, imageURL := range req.InputImages {
		normalizedImage, err := normalizeExternalInputImage(ctx, imageURL)
		if err != nil {
			return err
		}
		images = append(images, normalizedImage)
	}
	req.InputImages = images
	return nil
}

const (
	externalInputImageFetchTimeout  = 20 * time.Second
	externalInputImageMaxBytes      = 20 << 20
	externalInputImageMaxEncodedLen = (externalInputImageMaxBytes+2)/3*4 + 128
)

func normalizeExternalInputImage(ctx context.Context, raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", fmt.Errorf("input_images contains empty image URL")
	}
	lower := strings.ToLower(value)
	if strings.HasPrefix(lower, "data:image/") {
		if err := validateExternalInputImageDataURL(value); err != nil {
			return "", err
		}
		return value, nil
	}
	return fetchExternalInputImageAsDataURL(ctx, value)
}

func validateExternalInputImageDataURL(value string) error {
	if len(value) > externalInputImageMaxEncodedLen {
		return fmt.Errorf("input_images data URL is too large")
	}
	comma := strings.Index(value, ",")
	if comma < 0 {
		return fmt.Errorf("input_images data URLs must be base64 encoded images")
	}
	header := strings.ToLower(value[:comma])
	if !strings.HasPrefix(header, "data:image/") || !strings.Contains(header, ";base64") {
		return fmt.Errorf("input_images data URLs must be base64 encoded images")
	}
	encoded := value[comma+1:]
	if encoded == "" {
		return fmt.Errorf("input_images data URL image data is empty")
	}
	if _, err := base64.StdEncoding.DecodeString(encoded); err != nil {
		return fmt.Errorf("input_images data URL image data is not valid base64")
	}
	return nil
}

func fetchExternalInputImageAsDataURL(ctx context.Context, raw string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed == nil || parsed.Host == "" {
		return "", fmt.Errorf("input_images contains invalid URL")
	}
	if parsed.User != nil {
		return "", fmt.Errorf("input_images URL userinfo is not allowed")
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("input_images URL scheme must be http, https, or data:image")
	}
	if parsed.Hostname() == "" || strings.EqualFold(parsed.Hostname(), "localhost") {
		return "", fmt.Errorf("input_images URL host is not allowed")
	}

	fetchCtx, cancel := context.WithTimeout(ctx, externalInputImageFetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return "", fmt.Errorf("input_images contains invalid URL")
	}
	req.Header.Set("Accept", "image/*")
	client := &http.Client{
		Timeout:       externalInputImageFetchTimeout,
		Transport:     newExternalInputImageTransport(),
		CheckRedirect: rejectExternalInputImageRedirect,
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("input_images URL cannot be fetched safely")
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return "", fmt.Errorf("input_images URL redirects are not allowed")
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("input_images URL returned HTTP %d", resp.StatusCode)
	}
	if resp.ContentLength > externalInputImageMaxBytes {
		return "", fmt.Errorf("input_images URL image is too large")
	}

	contentType := resp.Header.Get("Content-Type")
	mediaType, _, _ := mime.ParseMediaType(contentType)
	mediaType = strings.ToLower(strings.TrimSpace(mediaType))
	if mediaType != "" && !strings.HasPrefix(mediaType, "image/") {
		return "", fmt.Errorf("input_images URL content type must be an image")
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, externalInputImageMaxBytes+1))
	if err != nil {
		return "", fmt.Errorf("input_images URL image could not be read")
	}
	if len(data) == 0 {
		return "", fmt.Errorf("input_images URL image data is empty")
	}
	if len(data) > externalInputImageMaxBytes {
		return "", fmt.Errorf("input_images URL image is too large")
	}
	if mediaType == "" {
		mediaType = strings.ToLower(http.DetectContentType(data))
	}
	if !strings.HasPrefix(mediaType, "image/") {
		return "", fmt.Errorf("input_images URL content type must be an image")
	}
	return "data:" + mediaType + ";base64," + base64.StdEncoding.EncodeToString(data), nil
}

func rejectExternalInputImageRedirect(req *http.Request, via []*http.Request) error {
	return http.ErrUseLastResponse
}

func newExternalInputImageTransport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.DialContext = dialPublicExternalInputImageAddress
	return transport
}

var dialPublicExternalInputImageAddress = func(ctx context.Context, network, address string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	if host == "" || strings.EqualFold(host, "localhost") {
		return nil, fmt.Errorf("blocked private or reserved address")
	}
	ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
	if err != nil || len(ips) == 0 {
		return nil, fmt.Errorf("input_images URL host cannot be resolved")
	}
	for _, ip := range ips {
		if !isPublicImageURLIP(ip) {
			return nil, fmt.Errorf("blocked private or reserved address")
		}
	}
	network = "tcp"
	dialer := &net.Dialer{Timeout: externalInputImageFetchTimeout}
	var lastErr error
	for _, ip := range ips {
		conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func isPublicImageURLIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return false
	}
	if ip4 := ip.To4(); ip4 != nil {
		return !(ip4[0] == 0 || ip4[0] == 10 || ip4[0] == 127 ||
			(ip4[0] == 169 && ip4[1] == 254) ||
			(ip4[0] == 172 && ip4[1] >= 16 && ip4[1] <= 31) ||
			(ip4[0] == 192 && ip4[1] == 168) ||
			(ip4[0] == 100 && ip4[1] >= 64 && ip4[1] <= 127) ||
			(ip4[0] == 192 && ip4[1] == 0 && ip4[2] == 0) ||
			(ip4[0] == 192 && ip4[1] == 0 && ip4[2] == 2) ||
			(ip4[0] == 198 && ip4[1] == 18) ||
			(ip4[0] == 198 && ip4[1] == 19) ||
			(ip4[0] == 198 && ip4[1] == 51 && ip4[2] == 100) ||
			(ip4[0] == 203 && ip4[1] == 0 && ip4[2] == 113) ||
			ip4[0] >= 224)
	}
	return !(ip.IsInterfaceLocalMulticast() || ip.IsLinkLocalMulticast())
}

func externalImageJobMode(editMode bool) string {
	if editMode {
		return "edit"
	}
	return "generation"
}

func writeExternalImageError(c *gin.Context, status int, message string) {
	message = strings.TrimSpace(message)
	if message == "" {
		message = http.StatusText(status)
	}
	c.JSON(status, gin.H{
		"error": gin.H{
			"message": security.SanitizeInput(message),
			"type":    "invalid_request_error",
		},
	})
}
