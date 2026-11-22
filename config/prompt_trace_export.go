package config

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// PromptTraceExportConfig enables asynchronous, metadata-only request tracing.
type PromptTraceExportConfig struct {
	BaseURL     string
	PublicKey   string
	SecretKey   string
	ProjectID   string
	ViewURL     string
	SourceURL   string
	BackfillIDs []int64
}

func (c PromptTraceExportConfig) Enabled() bool {
	return c.BaseURL != "" && c.PublicKey != "" && c.SecretKey != ""
}

// Destination distinguishes queues for different Langfuse projects without
// storing the target's URL or credentials in the request tables.
func (c PromptTraceExportConfig) Destination() string {
	identity := c.ProjectID
	if identity == "" {
		identity = c.PublicKey
	}
	digest := sha256.Sum256([]byte(c.BaseURL + "\x00" + identity))
	return hex.EncodeToString(digest[:])
}

var langfuseProjectIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var langfuseKeyPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,256}$`)

func loadPromptTraceExportConfig() (PromptTraceExportConfig, error) {
	c := PromptTraceExportConfig{
		BaseURL:   strings.TrimSpace(os.Getenv("CODEX_LANGFUSE_BASE_URL")),
		PublicKey: strings.TrimSpace(os.Getenv("CODEX_LANGFUSE_PUBLIC_KEY")),
		SecretKey: strings.TrimSpace(os.Getenv("CODEX_LANGFUSE_SECRET_KEY")),
		ProjectID: strings.TrimSpace(os.Getenv("CODEX_LANGFUSE_PROJECT_ID")),
		ViewURL:   strings.TrimSpace(os.Getenv("CODEX_LANGFUSE_VIEW_URL")),
		SourceURL: strings.TrimSpace(os.Getenv("CODEX_LANGFUSE_SOURCE_URL")),
	}
	backfill := strings.TrimSpace(os.Getenv("CODEX_LANGFUSE_BACKFILL_IDS"))
	if c.BaseURL == "" && c.PublicKey == "" && c.SecretKey == "" && c.ProjectID == "" && c.ViewURL == "" && c.SourceURL == "" && backfill == "" {
		return c, nil
	}
	if !c.Enabled() {
		return c, fmt.Errorf("CODEX_LANGFUSE_BASE_URL, CODEX_LANGFUSE_PUBLIC_KEY and CODEX_LANGFUSE_SECRET_KEY must be set together")
	}
	if !langfuseKeyPattern.MatchString(c.PublicKey) || !langfuseKeyPattern.MatchString(c.SecretKey) {
		return c, fmt.Errorf("CODEX_LANGFUSE_PUBLIC_KEY and CODEX_LANGFUSE_SECRET_KEY must be valid Langfuse keys")
	}
	if c.ProjectID != "" && !langfuseProjectIDPattern.MatchString(c.ProjectID) {
		return c, fmt.Errorf("CODEX_LANGFUSE_PROJECT_ID must be a valid Langfuse project ID")
	}
	if c.ViewURL != "" && c.ProjectID == "" {
		return c, fmt.Errorf("CODEX_LANGFUSE_VIEW_URL requires CODEX_LANGFUSE_PROJECT_ID")
	}
	for _, setting := range []struct {
		name string
		addr *string
	}{
		{"CODEX_LANGFUSE_BASE_URL", &c.BaseURL},
		{"CODEX_LANGFUSE_VIEW_URL", &c.ViewURL},
		{"CODEX_LANGFUSE_SOURCE_URL", &c.SourceURL},
	} {
		if *setting.addr == "" {
			continue
		}
		u, err := url.Parse(*setting.addr)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" ||
			u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
			return c, fmt.Errorf("%s must be an absolute HTTP(S) URL without credentials, query or fragment", setting.name)
		}
		*setting.addr = strings.TrimRight(u.String(), "/")
	}
	if backfill != "" {
		seen := make(map[int64]bool)
		fields := strings.Split(backfill, ",")
		if len(fields) > 200 {
			return c, fmt.Errorf("CODEX_LANGFUSE_BACKFILL_IDS supports at most 200 request IDs")
		}
		for _, field := range fields {
			id, err := strconv.ParseInt(strings.TrimSpace(field), 10, 64)
			if err != nil || id <= 0 {
				return c, fmt.Errorf("CODEX_LANGFUSE_BACKFILL_IDS must contain comma-separated positive request IDs")
			}
			if !seen[id] {
				c.BackfillIDs = append(c.BackfillIDs, id)
				seen[id] = true
			}
		}
	}
	return c, nil
}
