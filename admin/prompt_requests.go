package admin

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func (h *Handler) ConfigurePromptRequestRecords(enabled bool) {
	h.promptRequestRecording = enabled
}

func (h *Handler) ListPromptRequests(c *gin.Context) {
	const pageSize = 25
	before, _ := strconv.ParseInt(c.Query("before"), 10, 64)
	records, err := h.db.ListPromptRequests(c.Request.Context(), before, pageSize+1)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	days, err := h.db.GetPromptRequestRetentionDays(c.Request.Context())
	if err != nil {
		writeInternalError(c, err)
		return
	}
	var next int64
	if len(records) > pageSize {
		records = records[:pageSize]
		next = records[pageSize-1].ID
	}
	c.Header("Cache-Control", "private, no-store")
	c.JSON(http.StatusOK, gin.H{
		"records": records, "next_before": next, "enabled": h.promptRequestRecording,
		"retention_days": days,
	})
}

func (h *Handler) UpdatePromptRequestRetention(c *gin.Context) {
	var input struct {
		Days int `json:"retention_days"`
	}
	if err := c.ShouldBindJSON(&input); err != nil {
		writeError(c, http.StatusBadRequest, database.ErrInvalidPromptRequestRetention.Error())
		return
	}
	if err := h.db.SetPromptRequestRetentionDays(c.Request.Context(), input.Days); err != nil {
		if errors.Is(err, database.ErrInvalidPromptRequestRetention) {
			writeError(c, http.StatusBadRequest, err.Error())
			return
		}
		writeInternalError(c, err)
		return
	}
	c.Header("Cache-Control", "private, no-store")
	c.JSON(http.StatusOK, gin.H{"retention_days": input.Days})
}

func (h *Handler) loadPromptRequest(c *gin.Context) (database.PromptRequestBody, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		writeError(c, http.StatusBadRequest, "Invalid request record ID")
		return database.PromptRequestBody{}, false
	}
	request, err := h.db.GetPromptRequest(c.Request.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(c, http.StatusNotFound, "Request record not found")
		return database.PromptRequestBody{}, false
	}
	if err != nil {
		writeInternalError(c, err)
		return database.PromptRequestBody{}, false
	}
	return request, true
}

type promptRequestImage struct {
	Index     int    `json:"index"`
	Label     string `json:"label"`
	Source    string `json:"source"`
	Reference string `json:"reference,omitempty"`
	base64    string
	content   []byte
}

type promptRequestPresentation struct {
	Text             string                `json:"text"`
	Images           []promptRequestImage  `json:"images"`
	Changes          *promptRequestChanges `json:"changes,omitempty"`
	Result           *promptRequestResult  `json:"result,omitempty"`
	PromptContext    promptRequestContext  `json:"prompt_context"`
	LangfuseTraceURL string                `json:"langfuse_trace_url,omitempty"`
	bodyFields       map[string]any
}

func (h *Handler) GetPromptRequest(c *gin.Context) {
	request, ok := h.loadPromptRequest(c)
	if !ok {
		return
	}
	presentation := h.analyzePromptRequest(c.Request.Context(), request)
	presentation.LangfuseTraceURL = h.promptRequestTraceURL(c.Request.Context(), request.ID)
	c.Header("Cache-Control", "private, no-store")
	c.JSON(http.StatusOK, struct {
		database.PromptRequestRecord
		promptRequestPresentation
	}{request.PromptRequestRecord, presentation})
}

func (h *Handler) GetPromptRequestImage(c *gin.Context) {
	index, err := strconv.Atoi(c.Param("image"))
	if err != nil || index < 0 {
		writeError(c, http.StatusBadRequest, "Invalid request image index")
		return
	}
	request, ok := h.loadPromptRequest(c)
	if !ok {
		return
	}
	images := presentPromptRequest(request, index).Images
	if index >= len(images) || images[index].Source == "reference" {
		writeError(c, http.StatusNotFound, "Request image bytes are unavailable")
		return
	}
	image := images[index]
	data := image.content
	if image.Source == "url" {
		data, _, err = fetchExternalInputImageBytes(c.Request.Context(), image.Reference)
		if err != nil {
			writeError(c, http.StatusBadGateway, "Linked request image could not be fetched")
			return
		}
	} else if image.base64 != "" {
		data, err = base64.StdEncoding.DecodeString(image.base64)
		if err != nil {
			data, err = base64.RawStdEncoding.DecodeString(image.base64)
		}
		if err != nil {
			writeError(c, http.StatusUnprocessableEntity, "Invalid request image data")
			return
		}
	}
	// Serve only sniffed raster formats. Untrusted SVG/HTML must never execute
	// under the admin origin, even if the client labelled it as an image.
	contentType := http.DetectContentType(data)
	switch contentType {
	case "image/png", "image/jpeg", "image/gif", "image/webp", "image/bmp", "image/x-icon":
	default:
		writeError(c, http.StatusUnsupportedMediaType, "Unsupported request image format")
		return
	}
	c.Header("Cache-Control", "private, no-store")
	c.Header("X-Content-Type-Options", "nosniff")
	c.Header("Content-Security-Policy", "default-src 'none'; sandbox")
	c.Data(http.StatusOK, contentType, data)
}

func presentPromptRequest(request database.PromptRequestBody, imageIndex int) promptRequestPresentation {
	view := promptRequestPresentation{Images: []promptRequestImage{}}
	media, params, _ := mime.ParseMediaType(request.ContentType)
	if strings.HasPrefix(media, "multipart/") && params["boundary"] != "" {
		view.presentMultipart(multipart.NewReader(bytes.NewReader(request.Body), params["boundary"]), imageIndex)
		return view
	}
	if !json.Valid(request.Body) {
		if imageIndex < 0 {
			view.Text = string(request.Body)
		}
		return view
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(request.Body))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return view
	}
	view.walkJSON(&value, "$", false, imageIndex)
	if imageIndex < 0 {
		view.bodyFields, _ = value.(map[string]any)
		text, _ := json.MarshalIndent(value, "", "  ")
		view.Text = string(text)
	}
	return view
}

func (view *promptRequestPresentation) addImage(image promptRequestImage) string {
	image.Index = len(view.Images)
	view.Images = append(view.Images, image)
	return fmt.Sprintf("[Image %d]", image.Index+1)
}

func (view *promptRequestPresentation) walkJSON(value *any, path string, imageContext bool, want int) {
	switch node := (*value).(type) {
	case map[string]any:
		blockType, _ := node["type"].(string)
		mediaType, _ := node["media_type"].(string)
		if mediaType == "" {
			mediaType, _ = node["mimeType"].(string)
		}
		if mediaType == "" {
			mediaType, _ = node["mime_type"].(string)
		}
		imageContext = imageContext || strings.HasPrefix(mediaType, "image/") ||
			blockType == "image" || blockType == "input_image" || blockType == "image_url" || blockType == "computer_screenshot"
		keys := make([]string, 0, len(node))
		for key := range node {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			child := node[key]
			label := path + "." + key
			if data, ok := child.(string); ok && imageContext && key == "data" && data != "" {
				image := promptRequestImage{Label: label, Source: "embedded"}
				if len(view.Images) == want {
					image.base64 = data
				}
				node[key] = view.addImage(image)
				continue
			}
			if reference, ok := child.(string); ok && reference != "" && imageContext &&
				(key == "file_id" || key == "fileUri" || key == "file_uri") {
				view.addImage(promptRequestImage{Label: label, Source: "reference", Reference: reference})
				continue
			}
			switch key {
			case "image", "images", "image_url", "input_image", "input_images", "mask":
				view.walkJSON(&child, label, true, want)
			case "url", "source":
				view.walkJSON(&child, label, imageContext, want)
			default:
				view.walkJSON(&child, label, false, want)
			}
			node[key] = child
		}
	case []any:
		for index := range node {
			view.walkJSON(&node[index], fmt.Sprintf("%s[%d]", path, index), imageContext, want)
		}
	case string:
		if metadata, data, ok := strings.Cut(node, ","); imageContext && ok &&
			strings.HasPrefix(strings.ToLower(metadata), "data:image/") &&
			strings.HasSuffix(strings.ToLower(metadata), ";base64") {
			image := promptRequestImage{Label: path, Source: "embedded"}
			if len(view.Images) == want {
				image.base64 = data
			}
			*value = view.addImage(image)
		} else if imageContext {
			if parsed, err := url.Parse(node); err == nil && parsed.Host != "" && parsed.User == nil &&
				(parsed.Scheme == "https" || parsed.Scheme == "http") {
				view.addImage(promptRequestImage{Label: path, Source: "url", Reference: node})
			}
		}
	}
}

func (view *promptRequestPresentation) presentMultipart(reader *multipart.Reader, want int) {
	type formField struct {
		Name     string `json:"name"`
		Filename string `json:"filename,omitempty"`
		Value    string `json:"value"`
	}
	fields := []formField{}
	for {
		part, err := reader.NextPart()
		if err != nil {
			if err != io.EOF && want < 0 {
				fields = append(fields, formField{Value: "Incomplete multipart request"})
			}
			break
		}
		field := formField{Name: part.FormName(), Filename: part.FileName()}
		media, _, _ := mime.ParseMediaType(part.Header.Get("Content-Type"))
		switch {
		case strings.HasPrefix(media, "image/") || field.Name == "image" || field.Name == "image[]" || field.Name == "mask":
			image := promptRequestImage{Label: field.Name, Source: "embedded"}
			if field.Filename != "" {
				image.Label += " (" + field.Filename + ")"
			}
			if len(view.Images) == want {
				image.content, _ = io.ReadAll(part)
			}
			field.Value = view.addImage(image)
		case field.Filename != "":
			field.Value = "[File]"
		case want < 0:
			data, _ := io.ReadAll(part)
			field.Value = string(data)
		}
		_ = part.Close()
		if want < 0 {
			fields = append(fields, field)
		}
	}
	if want < 0 {
		text, _ := json.MarshalIndent(fields, "", "  ")
		view.Text = string(text)
	}
}
