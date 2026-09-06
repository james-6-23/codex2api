package proxy

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/internal/timezone"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type codexEnvironmentContextKey struct{}

type codexEnvironmentContext struct {
	reference time.Time
	resolve   func(string) *time.Location
	mu        sync.Mutex
	locations map[string]*time.Location
}

func WithCodexEnvironment(ctx context.Context, resolve func(string) *time.Location, reference time.Time) context.Context {
	return context.WithValue(ctx, codexEnvironmentContextKey{}, &codexEnvironmentContext{
		reference: reference,
		resolve:   resolve,
		locations: make(map[string]*time.Location),
	})
}

func (handler *Handler) bindCodexEnvironment(request *gin.Context, body []byte, received time.Time) {
	headers := CodexRequestMetadataHeaders(request.Request.Header, body)
	reference := codexEnvironmentReference(headers, received)
	request.Request = request.Request.WithContext(WithCodexEnvironment(request.Request.Context(), handler.store.ProxyTimezone, reference))
}

func codexEnvironmentReference(headers http.Header, received time.Time) time.Time {
	started := gjson.Get(headers.Get(codexTurnMetadataHeader), "turn_started_at_unix_ms")
	if started.Type == gjson.Number && started.Int() > 0 {
		reference := time.UnixMilli(started.Int())
		if reference.Year() >= 2000 && reference.Year() <= 2100 {
			return reference
		}
	}
	return received
}

func ApplyCodexEnvironment(ctx context.Context, body []byte, proxyURL string) []byte {
	if ctx == nil || IsResinEnabled() {
		return body
	}
	state, _ := ctx.Value(codexEnvironmentContextKey{}).(*codexEnvironmentContext)
	proxyURL = strings.TrimSpace(proxyURL)
	if state == nil || state.resolve == nil || proxyURL == "" {
		return body
	}
	state.mu.Lock()
	location, exists := state.locations[proxyURL]
	if !exists {
		location = state.resolve(proxyURL)
		state.locations[proxyURL] = location
	}
	state.mu.Unlock()
	if location == nil {
		return body
	}
	return rewriteCodexEnvironment(body, location, state.reference)
}

type codexEnvironmentField struct {
	start int
	end   int
	value string
}

func codexEnvironmentFields(text string) (map[string]codexEnvironmentField, bool) {
	trimmed := strings.TrimSpace(text)
	if !strings.HasPrefix(trimmed, "<environment_context>") || !strings.HasSuffix(trimmed, "</environment_context>") {
		return nil, false
	}
	decoder := xml.NewDecoder(strings.NewReader(text))
	fields := make(map[string]codexEnvironmentField)
	depth := 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			return fields, depth == 0
		}
		if err != nil {
			return nil, false
		}
		switch element := token.(type) {
		case xml.StartElement:
			depth++
			if depth == 1 && element.Name.Local != "environment_context" {
				return nil, false
			}
			if depth != 2 || (element.Name.Local != "timezone" && element.Name.Local != "current_date") {
				continue
			}
			if _, duplicate := fields[element.Name.Local]; duplicate || len(element.Attr) != 0 {
				return nil, false
			}
			start := int(decoder.InputOffset())
			content, err := decoder.Token()
			if err != nil {
				return nil, false
			}
			value, ok := content.(xml.CharData)
			if !ok {
				return nil, false
			}
			valueText := strings.TrimSpace(string(value))
			end := int(decoder.InputOffset())
			closing, err := decoder.Token()
			closingElement, ok := closing.(xml.EndElement)
			if err != nil || !ok || closingElement.Name != element.Name {
				return nil, false
			}
			depth--
			fields[element.Name.Local] = codexEnvironmentField{start: start, end: end, value: valueText}
		case xml.EndElement:
			depth--
		}
	}
}

func rewriteCodexEnvironment(body []byte, location *time.Location, reference time.Time) []byte {
	if location == nil || reference.IsZero() {
		return body
	}
	items := gjson.GetBytes(body, "input").Array()
	for itemIndex := len(items) - 1; itemIndex >= 0; itemIndex-- {
		item := items[itemIndex]
		if kind := item.Get("type").String(); kind != "" && kind != "message" {
			continue
		}
		switch item.Get("role").String() {
		case "user", "developer", "system":
		default:
			continue
		}
		content := item.Get("content")
		if !content.IsArray() {
			continue
		}
		parts := content.Array()
		for partIndex := len(parts) - 1; partIndex >= 0; partIndex-- {
			part := parts[partIndex]
			if part.Get("type").String() != "input_text" {
				continue
			}
			text := part.Get("text").String()
			fields, valid := codexEnvironmentFields(text)
			if !valid || len(fields) == 0 {
				continue
			}
			zone, exists := fields["timezone"]
			if !exists {
				return body
			}
			source, err := timezone.Load(zone.value)
			if err != nil {
				return body
			}
			date, hasDate := fields["current_date"]
			if hasDate && date.value != reference.In(source).Format(time.DateOnly) {
				return body
			}
			zone.value = location.String()
			replacements := []codexEnvironmentField{zone}
			if hasDate {
				date.value = reference.In(location).Format(time.DateOnly)
				replacements = append(replacements, date)
				if replacements[0].start < replacements[1].start {
					replacements[0], replacements[1] = replacements[1], replacements[0]
				}
			}
			updated := text
			for _, replacement := range replacements {
				updated = updated[:replacement.start] + replacement.value + updated[replacement.end:]
			}
			if updated == text {
				return body
			}
			updatedBody, err := sjson.SetBytes(body, fmt.Sprintf("input.%d.content.%d.text", itemIndex, partIndex), updated)
			if err != nil {
				return body
			}
			return updatedBody
		}
	}
	return body
}
