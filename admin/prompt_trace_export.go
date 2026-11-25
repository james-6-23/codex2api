package admin

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/codex2api/config"
	"github.com/codex2api/database"
)

const promptTraceMaxAnalysisBytes = 16 << 20
const promptTracePollInterval = 3 * time.Second
const promptTraceLease = 45 * time.Second

type promptTraceExporter struct {
	cfg         config.PromptTraceExportConfig
	destination string
	endpoint    string
	client      *http.Client
}

func newPromptTraceExporter(cfg config.PromptTraceExportConfig) *promptTraceExporter {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	transport.MaxIdleConnsPerHost = 2
	endpoint, _ := url.JoinPath(cfg.BaseURL, "api/public/otel/v1/traces")
	return &promptTraceExporter{
		cfg:         cfg,
		destination: cfg.Destination(),
		endpoint:    endpoint,
		client: &http.Client{
			Transport: transport,
			Timeout:   8 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

func (h *Handler) StartPromptTraceExport(ctx context.Context, cfg config.PromptTraceExportConfig) {
	if h.db == nil || !cfg.Enabled() {
		return
	}
	exporter := newPromptTraceExporter(cfg)
	h.promptTraceExporter = exporter
	go exporter.run(ctx, h)
}

func (h *Handler) promptRequestTraceURL(ctx context.Context, id int64) string {
	exporter := h.promptTraceExporter
	if exporter == nil || exporter.cfg.ViewURL == "" || exporter.cfg.ProjectID == "" {
		return ""
	}
	traceID, err := h.db.GetPromptRequestTraceID(ctx, id, exporter.destination)
	if err != nil || len(traceID) != 32 {
		return ""
	}
	if _, err := hex.DecodeString(traceID); err != nil {
		return ""
	}
	address, _ := url.JoinPath(exporter.cfg.ViewURL, "project", exporter.cfg.ProjectID, "traces", traceID)
	return address
}

func (e *promptTraceExporter) run(ctx context.Context, h *Handler) {
	defer e.client.CloseIdleConnections()
	backfill := e.cfg.BackfillIDs
	delay := time.Duration(0)
	for waitForPromptTrace(ctx, delay) {
		delay = promptTracePollInterval
		if len(backfill) > 0 {
			enqueueCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			err := h.db.EnqueuePromptRequestTraceBackfill(enqueueCtx, e.destination, backfill, time.Now().UTC())
			cancel()
			if err != nil {
				log.Printf("[prompt-trace] historical enrollment failed: %T", err)
				delay = 15 * time.Second
				continue
			}
			backfill = nil
		}
		for n := 0; n < 8 && ctx.Err() == nil; n++ {
			now := time.Now().UTC()
			claimCtx, cancel := context.WithTimeout(ctx, 4*time.Second)
			job, err := h.db.ClaimPromptRequestTrace(claimCtx, e.destination, now, now.Add(promptTraceLease))
			cancel()
			if err != nil {
				log.Printf("[prompt-trace] claim failed: %T", err)
				delay = 15 * time.Second
				break
			}
			if job == nil {
				break
			}
			if e.process(ctx, h, job) != nil {
				// Avoid claiming every queued row while the destination is down.
				delay = promptTraceBackoff(job.DeliveryTry)
				break
			}
		}
	}
}

func waitForPromptTrace(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func promptTraceBackoff(try int) time.Duration {
	return time.Duration(1<<min(max(try, 1), 7)) * 3 * time.Second
}

func (e *promptTraceExporter) process(ctx context.Context, h *Handler, job *database.PromptRequestTraceJob) error {
	jobCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	readCtx, cancelRead := context.WithTimeout(jobCtx, 10*time.Second)
	request, err := h.db.GetStoredPromptRequest(readCtx, job.RequestID)
	if errors.Is(err, sql.ErrNoRows) {
		cancelRead()
		return nil
	}
	var traceID string
	if err == nil {
		skipped := len(request.Body) > promptTraceMaxAnalysisBytes
		var view promptRequestPresentation
		if skipped {
			response, lookupErr := h.db.GetPromptRequestResponse(readCtx, request.ID)
			if lookupErr == nil && response != nil {
				view.Result = &promptRequestResult{Response: response}
			}
		} else {
			view = h.analyzePromptRequest(readCtx, request)
		}
		err = readCtx.Err()
		cancelRead()
		if err == nil {
			var trace otlpTracePayload
			traceID, trace = buildPromptTrace(e.destination, e.cfg.SourceURL, request, view, job, skipped)
			err = e.send(jobCtx, trace)
		}
	} else {
		cancelRead()
	}

	now := time.Now().UTC()
	if err != nil {
		var status *promptTraceHTTPError
		if errors.As(err, &status) {
			log.Printf("[prompt-trace] request %d delivery rejected with HTTP %d", job.RequestID, status.code)
		} else if ctx.Err() == nil {
			log.Printf("[prompt-trace] request %d delivery failed: %T", job.RequestID, err)
		}
		traceID = ""
		now = now.Add(promptTraceBackoff(job.DeliveryTry))
	}
	finishCtx, cancelFinish := context.WithTimeout(ctx, 4*time.Second)
	defer cancelFinish()
	if persistErr := h.db.FinishPromptRequestTrace(finishCtx, e.destination, job, traceID, now); persistErr != nil {
		if ctx.Err() == nil {
			log.Printf("[prompt-trace] could not finalize delivery for request %d: %T", job.RequestID, persistErr)
		}
		return persistErr
	}
	return err
}

type promptTraceHTTPError struct{ code int }

func (err *promptTraceHTTPError) Error() string {
	return fmt.Sprintf("Langfuse returned HTTP %d", err.code)
}

func (e *promptTraceExporter) send(ctx context.Context, detail otlpTracePayload) error {
	encoded, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, e.endpoint, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	request.SetBasicAuth(e.cfg.PublicKey, e.cfg.SecretKey)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("x-langfuse-ingestion-version", "4")
	response, err := e.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return &promptTraceHTTPError{code: response.StatusCode}
	}
	var result struct {
		PartialSuccess struct {
			Rejected json.RawMessage `json:"rejectedSpans"`
		} `json:"partialSuccess"`
	}
	err = json.NewDecoder(io.LimitReader(response.Body, 8192)).Decode(&result)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if len(result.PartialSuccess.Rejected) > 0 {
		raw := bytes.Trim(result.PartialSuccess.Rejected, "\"")
		rejected, err := strconv.ParseInt(string(raw), 10, 64)
		if err != nil || rejected != 0 {
			return errors.New("Langfuse rejected spans in the batch")
		}
	}
	return nil
}
