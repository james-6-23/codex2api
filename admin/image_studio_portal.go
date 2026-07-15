package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/internal/imageproc"
	"github.com/codex2api/internal/imagestore"
	"github.com/codex2api/proxy"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

// imageStudioPortalAuthMiddleware gates the public Image Studio portal:
// page must be enabled, and the request must present a valid API key.
// When imageProxy is available, reuse /v1 auth so limits/concurrency context is set.
func (h *Handler) imageStudioPortalAuthMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if h == nil || h.db == nil {
			writeError(c, http.StatusServiceUnavailable, "服务未就绪")
			c.Abort()
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
		ok, err := h.PublicImageStudioPageEnabled(ctx)
		cancel()
		if err != nil {
			writeInternalError(c, err)
			c.Abort()
			return
		}
		if !ok {
			writeError(c, http.StatusNotFound, "生图门户未启用")
			c.Abort()
			return
		}
		if h.imageProxy != nil {
			h.imageProxy.APIKeyAuthMiddleware()(c)
			return
		}
		h.portalAPIKeyAuthFallback()(c)
	}
}

// portalAPIKeyAuthFallback validates the API key when imageProxy is not wired.
func (h *Handler) portalAPIKeyAuthFallback() gin.HandlerFunc {
	return func(c *gin.Context) {
		key := extractPublicAPIKey(c)
		if key == "" {
			writeError(c, http.StatusUnauthorized, "缺少 Authorization Bearer API Key")
			c.Abort()
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
		defer cancel()
		row, err := h.db.GetAPIKeyByValue(ctx, key)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				security.SecurityAuditLog("IMAGE_STUDIO_AUTH_FAILED", "ip="+security.SanitizeLog(c.ClientIP())+" key="+security.MaskAPIKey(key))
				writeError(c, http.StatusUnauthorized, "API Key 无效或不存在")
				c.Abort()
				return
			}
			writeInternalError(c, err)
			c.Abort()
			return
		}
		if row.IsExpired(time.Now()) {
			writeError(c, http.StatusUnauthorized, "API Key 已过期")
			c.Abort()
			return
		}
		if row.IsQuotaExhausted() {
			writeError(c, http.StatusForbidden, "API Key 配额已用尽")
			c.Abort()
			return
		}
		c.Set("portalAPIKeyRow", row)
		c.Next()
	}
}

func portalAPIKeyFromContext(c *gin.Context) *database.APIKeyRow {
	if row := proxy.APIKeyRowFromContext(c); row != nil {
		return row
	}
	if c == nil {
		return nil
	}
	v, ok := c.Get("portalAPIKeyRow")
	if !ok || v == nil {
		return nil
	}
	row, _ := v.(*database.APIKeyRow)
	return row
}

// PublicImageStudioPageEnabled reports whether the public Image Studio portal is enabled.
func (h *Handler) PublicImageStudioPageEnabled(ctx context.Context) (bool, error) {
	if h == nil || h.db == nil {
		return false, nil
	}
	settings, err := h.db.GetSystemSettings(ctx)
	if err != nil {
		return false, err
	}
	if settings == nil {
		return true, nil
	}
	return settings.PublicImageStudioPageEnabled, nil
}

func (h *Handler) CreatePortalImageJob(c *gin.Context) {
	apiKey := portalAPIKeyFromContext(c)
	if apiKey == nil {
		writeError(c, http.StatusUnauthorized, "缺少或无效的 API Key")
		return
	}
	var req imageGenerationJobPayload
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求体无效")
		return
	}
	if err := normalizePortalImageJobPayload(&req, false); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	req.APIKeyID = apiKey.ID
	req.External = true
	h.enqueuePortalImageJob(c, apiKey, req, false)
}

func (h *Handler) CreatePortalImageEditJob(c *gin.Context) {
	apiKey := portalAPIKeyFromContext(c)
	if apiKey == nil {
		writeError(c, http.StatusUnauthorized, "缺少或无效的 API Key")
		return
	}
	var req imageGenerationJobPayload
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求体无效")
		return
	}
	if err := normalizePortalImageJobPayload(&req, true); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	req.APIKeyID = apiKey.ID
	req.External = true
	h.enqueuePortalImageJob(c, apiKey, req, true)
}

func normalizePortalImageJobPayload(req *imageGenerationJobPayload, editMode bool) error {
	if req == nil {
		return errors.New("请求体无效")
	}
	req.Prompt = strings.TrimSpace(req.Prompt)
	if req.Prompt == "" {
		return errors.New("提示词不能为空")
	}
	if len([]rune(req.Prompt)) > 8000 {
		return errors.New("提示词不能超过 8000 个字符")
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
	req.TemplateID = 0 // portal never selects admin templates by id write

	if editMode {
		if len(req.InputImages) == 0 {
			return errors.New("图生图需要上传参考图片")
		}
		if len(req.InputImages) > proxy.MaxImageEditInputCount {
			return errors.New("参考图片数量超过限制")
		}
	} else {
		req.InputImages = nil
	}
	return nil
}

func (h *Handler) enqueuePortalImageJob(c *gin.Context, apiKey *database.APIKeyRow, req imageGenerationJobPayload, editMode bool) {
	keyID, keyName, keyMasked := imageJobAPIKeyMeta(apiKey)
	endpoint := "/api/image-studio/jobs"
	if editMode {
		endpoint = "/api/image-studio/edit-jobs"
	}
	if h.inspectImagePromptFilter(c, proxy.AppendImageStyleToPrompt(req.Prompt, req.Style), req.Model, keyID, keyName, keyMasked, endpoint, nil, true) {
		return
	}

	imageProxy := h.imageProxy
	if imageProxy == nil {
		imageProxy = proxy.NewHandler(h.store, h.db, nil, nil)
	}
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
	job, err := h.db.GetImageGenerationJob(ctx, jobID)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	log.Printf("[image-studio] portal job=%d queued mode=%s model=%s size=%s quality=%s format=%s api_key=%s prompt_chars=%d",
		jobID,
		map[bool]string{true: "edit", false: "generate"}[editMode],
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
	c.JSON(http.StatusAccepted, imageJobResponse{Job: job})
}

func (h *Handler) ListPortalImageJobs(c *gin.Context) {
	apiKey := portalAPIKeyFromContext(c)
	if apiKey == nil {
		writeError(c, http.StatusUnauthorized, "缺少或无效的 API Key")
		return
	}
	page, pageSize := paginationParams(c, 20)
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	result, err := h.db.ListImageGenerationJobs(ctx, page, pageSize, apiKey.ID)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	decorateImageJobPage(result)
	c.JSON(http.StatusOK, result)
}

func (h *Handler) GetPortalImageJob(c *gin.Context) {
	apiKey := portalAPIKeyFromContext(c)
	if apiKey == nil {
		writeError(c, http.StatusUnauthorized, "缺少或无效的 API Key")
		return
	}
	id, err := parsePositiveIDParam(c, "id")
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效 ID")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	job, err := h.db.GetImageGenerationJob(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(c, http.StatusNotFound, "任务不存在")
		return
	}
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if job.APIKeyID != apiKey.ID {
		writeError(c, http.StatusNotFound, "任务不存在")
		return
	}
	if c.Query("include_cache") == "1" {
		h.attachImageJobAssetCachePayload(job)
	}
	decorateImageJobAssets(job)
	c.JSON(http.StatusOK, imageJobResponse{Job: job})
}

func (h *Handler) DeletePortalImageJob(c *gin.Context) {
	apiKey := portalAPIKeyFromContext(c)
	if apiKey == nil {
		writeError(c, http.StatusUnauthorized, "缺少或无效的 API Key")
		return
	}
	id, err := parsePositiveIDParam(c, "id")
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效 ID")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	job, err := h.db.GetImageGenerationJob(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(c, http.StatusNotFound, "任务不存在")
		return
	}
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if job.APIKeyID != apiKey.ID {
		writeError(c, http.StatusNotFound, "任务不存在")
		return
	}
	if job.Status == database.ImageJobQueued || job.Status == database.ImageJobRunning {
		writeError(c, http.StatusConflict, "任务仍在处理中，完成或失败后才能删除")
		return
	}
	if err := h.db.DeleteImageGenerationJob(ctx, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(c, http.StatusNotFound, "任务不存在")
			return
		}
		writeInternalError(c, err)
		return
	}
	for _, asset := range job.Assets {
		if asset.StoragePath != "" {
			if backend, err := imagestore.Resolve(asset.StoragePath); err == nil {
				_ = backend.Delete(ctx, asset.StoragePath)
			}
		}
		thumbCache.Invalidate(asset.ID)
	}
	writeMessage(c, http.StatusOK, "已删除")
}

func (h *Handler) ListPortalImageAssets(c *gin.Context) {
	apiKey := portalAPIKeyFromContext(c)
	if apiKey == nil {
		writeError(c, http.StatusUnauthorized, "缺少或无效的 API Key")
		return
	}
	page, pageSize := paginationParams(c, 24)
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	result, err := h.db.ListImageAssets(ctx, page, pageSize, apiKey.ID)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if result != nil {
		decorateImageAssets(result.Assets)
	}
	c.JSON(http.StatusOK, result)
}

func (h *Handler) GetPortalImageAssetFile(c *gin.Context) {
	apiKey := portalAPIKeyFromContext(c)
	if apiKey == nil {
		writeError(c, http.StatusUnauthorized, "缺少或无效的 API Key")
		return
	}
	id, err := parsePositiveIDParam(c, "id")
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效 ID")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	ownerKeyID, err := h.db.GetImageAssetJobAPIKeyID(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(c, http.StatusNotFound, "图片不存在")
		return
	}
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if ownerKeyID != apiKey.ID {
		writeError(c, http.StatusNotFound, "图片不存在")
		return
	}
	asset, err := h.db.GetImageAsset(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(c, http.StatusNotFound, "图片不存在")
		return
	}
	if err != nil {
		writeInternalError(c, err)
		return
	}
	h.serveImageAssetFile(c, asset, imageAssetFileOptions{
		download: c.Query("download") == "1",
		private:  true,
		thumbKB:  imageproc.ClampThumbKB(queryInt(c, "thumb_kb")),
	})
}

func (h *Handler) DeletePortalImageAsset(c *gin.Context) {
	apiKey := portalAPIKeyFromContext(c)
	if apiKey == nil {
		writeError(c, http.StatusUnauthorized, "缺少或无效的 API Key")
		return
	}
	id, err := parsePositiveIDParam(c, "id")
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效 ID")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	ownerKeyID, err := h.db.GetImageAssetJobAPIKeyID(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(c, http.StatusNotFound, "图片不存在")
		return
	}
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if ownerKeyID != apiKey.ID {
		writeError(c, http.StatusNotFound, "图片不存在")
		return
	}
	asset, err := h.db.GetImageAsset(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(c, http.StatusNotFound, "图片不存在")
		return
	}
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if err := h.db.DeleteImageAsset(ctx, id); err != nil {
		writeInternalError(c, err)
		return
	}
	if asset.StoragePath != "" {
		if backend, err := imagestore.Resolve(asset.StoragePath); err == nil {
			_ = backend.Delete(ctx, asset.StoragePath)
		}
		thumbCache.Invalidate(asset.ID)
	}
	writeMessage(c, http.StatusOK, "已删除")
}
