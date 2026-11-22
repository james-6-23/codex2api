package proxy

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

const (
	promptTaskMaxFinalBytes  = 128 << 10
	promptTaskClientMaxSteps = 128
	promptTaskMaxClockSkew   = 3 * time.Second
)

type promptTaskClientReport struct {
	database.PromptTaskClientSnapshot
	SessionID  string `json:"session_id"`
	UserDigest string `json:"user_sha256"`
}

var (
	promptTaskClientIDPattern    = regexp.MustCompile(`^[A-Za-z0-9_.:-]{1,160}$`)
	promptTaskCredentialPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)(bearer[ \t]+)[^ \t\r\n"'<>]+`),
		regexp.MustCompile(`(?i)((?:api[_-]?key|client[_-]?secret|(?:session|refresh|access|id)[_-]?token|password)["']?[ \t]*[:=][ \t]*["']?)[^ \t\r\n"'<>]+`),
		regexp.MustCompile(`\b(?:sk-[A-Za-z0-9_-]{12,}|gh[pousr]_[A-Za-z0-9]{16,}|github_pat_[A-Za-z0-9_]{16,}|xox[baprs]-[A-Za-z0-9-]{12,})\b`),
		regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`),
		regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z0-9 ]*PRIVATE KEY-----`),
	}
)

// recordPromptTaskClient accepts opt-in completed or interrupted turn reports
// using the same API key as the requests being replayed.
func (h *Handler) recordPromptTaskClient(c *gin.Context) {
	keyID := requestAPIKeyID(c)
	if keyID <= 0 {
		api.SendError(c, api.ErrMissingAPIKey)
		return
	}
	if !h.promptRequestRecordingEnabled() {
		api.SendError(c, api.NewAPIError(api.ErrCodeResourceNotFound, "Prompt task recording is disabled", api.ErrorTypeInvalidRequest))
		return
	}

	var report promptTaskClientReport
	decoder := json.NewDecoder(http.MaxBytesReader(c.Writer, c.Request.Body, security.MaxPromptTaskClientBodySize))
	decoder.DisallowUnknownFields()
	err := decoder.Decode(&report)
	if err == nil {
		var trailing json.RawMessage
		if extra := decoder.Decode(&trailing); !errors.Is(extra, io.EOF) {
			err = extra
			if err == nil {
				err = errors.New("multiple task reports")
			}
		}
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		c.Status(http.StatusRequestEntityTooLarge)
		return
	}
	now := time.Now().UTC()
	if err != nil || !validPromptTaskClientReport(report, now) {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidRequest, "Invalid Codex task replay report", api.ErrorTypeInvalidRequest))
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	days, err := h.db.GetPromptRequestRetentionDays(ctx)
	if err != nil {
		api.SendError(c, api.ErrServerError)
		return
	}
	if report.StartedAt.Before(now.AddDate(0, 0, -days)) {
		api.SendError(c, api.NewAPIError(api.ErrCodeInvalidRequest, "Task report is outside the request record retention period", api.ErrorTypeInvalidRequest))
		return
	}
	credential := c.GetString("apiKey")
	masked := maskPromptTaskClientText(report.FinalText, credential)
	report.FinalRedacted = report.FinalRedacted || masked != report.FinalText
	if len(masked) > promptTaskMaxFinalBytes {
		end := promptTaskMaxFinalBytes
		for end > 0 && !utf8.RuneStart(masked[end]) {
			end--
		}
		masked = masked[:end]
		report.FinalTruncated = true
	}
	report.FinalText = masked
	for index := range report.Steps {
		name := report.Steps[index].Name
		if maskPromptTaskClientText(name, credential) != name {
			report.Steps[index].Name = "redacted"
		}
	}
	groups := database.PromptTaskClientGroups(report.SessionID, keyID)
	matched, err := h.db.StorePromptTaskClientSnapshot(ctx, groups, strings.ToLower(report.UserDigest), report.PromptTaskClientSnapshot)
	if err != nil {
		log.Printf("[prompt-task-replay] could not store client report: %T", err)
		api.SendError(c, api.ErrServerError)
		return
	}
	if !matched {
		api.SendError(c, api.NewAPIError(api.ErrCodeResourceNotFound, "No corresponding recorded gateway conversation", api.ErrorTypeInvalidRequest))
		return
	}
	c.Status(http.StatusNoContent)
}

func validPromptTaskClientReport(report promptTaskClientReport, now time.Time) bool {
	if !promptTaskClientIDPattern.MatchString(report.SessionID) || !promptTaskClientIDPattern.MatchString(report.TurnID) {
		return false
	}
	if len(report.UserDigest) != 64 {
		return false
	}
	if _, err := hex.DecodeString(report.UserDigest); err != nil {
		return false
	}
	if report.StartedAt.IsZero() || report.FinishedAt == nil || report.FinishedAt.Before(report.StartedAt) ||
		report.FinishedAt.After(now.Add(promptTaskMaxClockSkew)) || len(report.FinalText) > promptTaskMaxFinalBytes ||
		len(report.Steps) > promptTaskClientMaxSteps {
		return false
	}
	if report.Status != "completed" && (report.Status != "interrupted" || report.FinalText != "") {
		return false
	}
	for _, step := range report.Steps {
		if !promptTaskClientIDPattern.MatchString(step.Name) ||
			(step.Kind != "tool" || step.Status != "observed") && (step.Kind != "skill" || step.Status != "loaded") {
			return false
		}
		if step.At != nil && (step.At.Before(report.StartedAt) || step.At.After(*report.FinishedAt)) {
			return false
		}
	}
	return true
}

func maskPromptTaskClientText(text, credential string) string {
	if credential != "" {
		text = strings.ReplaceAll(text, credential, "[redacted]")
	}
	text = security.MaskURLCredentials(text)
	for _, pattern := range promptTaskCredentialPatterns {
		text = pattern.ReplaceAllString(text, "$1[redacted]")
	}
	return text
}
