package proxy

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const promptRequestRecordID = "prompt_request_record_id"
const promptRequestAction = "prompt_request_action"

// RecordModelRequest captures the admitted payload and downstream reply on authenticated routes.
func (h *Handler) RecordModelRequest(c *gin.Context) {
	if !h.promptRequestRecordingEnabled() {
		c.Next()
		return
	}
	rawBody, _ := rawRequestBodyFromContext(c)
	finish := h.beginPromptRequest(c, rawBody, c.GetHeader("Content-Type"), "http")
	writer := c.Writer
	if capture := promptResponseFromContext(c); capture != nil {
		c.Writer = &promptResponseWriter{ResponseWriter: writer, capture: capture}
	}
	returned := false
	defer func() {
		status := c.Writer.Status()
		c.Writer = writer
		if !returned {
			status = http.StatusInternalServerError
		} else if reported := c.GetInt(AccessLogStatusContextKey); reported != 0 {
			status = reported
		}
		finish(status)
	}()
	c.Next()
	returned = true
}

func (h *Handler) promptRequestRecordingEnabled() bool {
	return h != nil && h.db != nil && h.cfg != nil && h.cfg.PromptRequestRecording
}

func (h *Handler) beginPromptRequest(c *gin.Context, body []byte, contentType, transport string) func(int) {
	// The caller already owns the size-limited ingress buffer. Write it before
	// forwarding (including for long streams) while its memory is still reserved.
	c.Set(promptRequestRecordID, int64(0))
	c.Set(promptRequestAction, "")
	endpoint := c.Request.URL.Path
	chat := database.IsPromptRequestChat(endpoint, transport)
	keyID := requestAPIKeyID(c)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 5*time.Second)
	id, err := h.db.InsertPromptRequest(ctx, database.PromptRequestBody{
		PromptRequestRecord: database.PromptRequestRecord{
			CreatedAt: time.Now().UTC(), Endpoint: endpoint, Transport: transport,
			Model: gjson.GetBytes(body, "model").String(), APIKey: c.GetString(contextAPIKeyName),
		},
		APIKeyID:          keyID,
		ContentType:       contentType,
		Body:              body,
		ConversationScope: database.PromptRequestConversationScope(body, keyID, endpoint, transport),
		ConversationGroup: database.PromptRequestConversationGroup(body, keyID),
	})
	cancel()
	if err != nil {
		log.Printf("[prompt-requests] could not persist request on %s: %T", c.FullPath(), err)
		return func(int) {}
	}
	if id == 0 {
		return func(int) {}
	}
	c.Set(promptRequestRecordID, id)
	capture := &promptResponseCapture{chatReplyClassified: chat}
	if h.cfg.PromptTraceExport.Enabled() {
		capture.traceUsage = &promptRequestTraceUsage{}
	}
	if transport == "websocket" {
		capture.mode = transport
	}
	c.Set(promptResponseCaptureKey, capture)
	return func(status int) {
		finishedAt := time.Now().UTC()
		action := c.GetString(promptRequestAction)
		if action == "" {
			switch {
			case status >= 400:
				action = "error"
			case status > 0:
				action = "allow"
			default:
				action = "received"
			}
		}
		c.Set(promptRequestRecordID, int64(0))
		c.Set(promptResponseCaptureKey, nil)
		ctx, cancel := context.WithTimeout(context.WithoutCancel(c.Request.Context()), 5*time.Second)
		defer cancel()
		if chat {
			defer func() {
				if err := h.db.IndexPromptRequestChat(ctx, id, body); err != nil {
					log.Printf("[prompt-requests] chat replay indexing failed for request %d: %T", id, err)
				}
			}()
		}
		response := capture.result(status, c.Request.Context().Err() != nil)
		response.FinishedAt = &finishedAt
		if capture.traceUsage != nil {
			err := h.db.CompletePromptRequestTrace(ctx, id, action, status, response, finishedAt,
				h.cfg.PromptTraceExport.Destination(), capture.traceUsage.snapshot())
			if err == nil {
				return
			}
			log.Printf("[prompt-requests] could not queue trace for request %d: %T", id, err)
		}
		if err := h.db.SavePromptRequestResponse(ctx, id, response); err != nil {
			log.Printf("[prompt-requests] could not persist response %d: %T", id, err)
		}
		if err := h.db.FinishPromptRequest(ctx, id, action, status); err != nil {
			log.Printf("[prompt-requests] could not update request %d: %T", id, err)
		}
	}
}

func markPromptRequestAction(c *gin.Context, action string) {
	if c == nil || c.GetInt64(promptRequestRecordID) <= 0 || action == "" {
		return
	}
	previous := c.GetString(promptRequestAction)
	if previous == "block" || (previous == "warn" && action == "allow") {
		return
	}
	c.Set(promptRequestAction, action)
}

func (h *Handler) StartPromptRequestRetention(ctx context.Context) {
	if h == nil || h.db == nil || h.cfg == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			indexCtx, cancelIndex := context.WithTimeout(ctx, time.Minute)
			indexErr := h.db.IndexPromptRequestConversations(indexCtx)
			cancelIndex()
			if indexErr != nil && ctx.Err() == nil {
				log.Printf("[prompt-requests] conversation indexing failed: %T", indexErr)
			}
			taskCtx, cancelTasks := context.WithTimeout(ctx, time.Minute)
			taskErr := h.db.IndexPromptRequestTaskAnchors(taskCtx, 128)
			cancelTasks()
			if taskErr != nil && ctx.Err() == nil {
				log.Printf("[prompt-requests] task replay indexing failed: %T", taskErr)
			}
			chatCtx, cancelChat := context.WithTimeout(ctx, time.Minute)
			chatErr := h.db.IndexArchivedPromptRequestChats(chatCtx)
			cancelChat()
			if chatErr != nil && ctx.Err() == nil {
				log.Printf("[prompt-requests] archived chat replay indexing failed: %T", chatErr)
			}
			purgeCtx, cancel := context.WithTimeout(ctx, time.Minute)
			err := h.db.PurgeExpiredPromptRequests(purgeCtx, time.Now())
			cancel()
			if err != nil && ctx.Err() == nil {
				log.Printf("[prompt-requests] cleanup failed: %T", err)
			}
			select {
			case <-ticker.C:
			case <-ctx.Done():
				return
			}
		}
	}()
}
