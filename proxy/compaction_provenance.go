package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const (
	compactionProvenanceCacheNamespace = "compaction-provenance"
	compactionAffinityTTLEnv           = "CODEX_COMPACTION_AFFINITY_TTL"
	nativeCodexCompactionDomain        = "codex:openai"
	defaultCompactionProvenanceTTL     = 7 * 24 * time.Hour
	compactionProvenanceRecordVersion  = 1
	compactionProvenanceCacheTimeout   = 500 * time.Millisecond
)

var errConflictingCompactionProvenance = errors.New("conflicting compaction provenance domains")

type compactionProvenanceRecord struct {
	Version             int       `json:"version"`
	AccountID           int64     `json:"account_id"`
	CompatibilityDomain string    `json:"compatibility_domain"`
	CreatedAt           time.Time `json:"created_at"`
	LastSeenAt          time.Time `json:"last_seen_at"`
}

type compactionAffinityResolution struct {
	Known               bool
	CompatibilityDomain string
	PreferredAccountID  int64
}

func compactionContentDigest(encryptedContent string) string {
	sum := sha256.Sum256([]byte(encryptedContent))
	return hex.EncodeToString(sum[:])
}

func compactionProvenanceTTL() time.Duration {
	raw := strings.TrimSpace(os.Getenv(compactionAffinityTTLEnv))
	if raw == "" {
		return defaultCompactionProvenanceTTL
	}
	ttl, err := time.ParseDuration(raw)
	if err != nil || ttl <= 0 {
		return defaultCompactionProvenanceTTL
	}
	return ttl
}

func canonicalCompactionBaseURL(raw string) string {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return ""
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.RawQuery = ""
	parsed.Fragment = ""
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed.String()
}

func accountCompactionDomain(account *auth.Account) string {
	if account == nil {
		return ""
	}
	if account.IsOpenAIResponsesAPI() {
		baseURL, _ := account.OpenAIResponsesCredentials()
		if normalized, err := auth.NormalizeOpenAIResponsesBaseURL(baseURL); err == nil {
			baseURL = normalized
		}
		if canonical := canonicalCompactionBaseURL(baseURL); canonical != "" {
			return "responses:" + canonical
		}
		return ""
	}
	if account.IsGrokAPI() {
		// Grok resolves the actual upstream route from the model catalog at
		// request time. The configured base URL is therefore not a safe
		// compatibility boundary for opaque compaction state. Keep Grok state
		// account-local until the resolved route is available at record time.
		if account.ID() > 0 {
			return fmt.Sprintf("grok:account:%d", account.ID())
		}
		return ""
	}
	return nativeCodexCompactionDomain
}

func decodeCompactionProvenanceRecord(raw json.RawMessage) (compactionProvenanceRecord, error) {
	var record compactionProvenanceRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return compactionProvenanceRecord{}, err
	}
	if record.Version != compactionProvenanceRecordVersion || record.AccountID <= 0 || strings.TrimSpace(record.CompatibilityDomain) == "" {
		return compactionProvenanceRecord{}, errors.New("invalid compaction provenance record")
	}
	return record, nil
}

func (h *Handler) recordCompactionProvenance(ctx context.Context, account *auth.Account, encryptedContent string) error {
	if h == nil || h.cache == nil || account == nil {
		return nil
	}
	encryptedContent = strings.TrimSpace(encryptedContent)
	domain := accountCompactionDomain(account)
	if encryptedContent == "" || domain == "" || account.ID() <= 0 {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	cacheCtx, cancel := context.WithTimeout(ctx, compactionProvenanceCacheTimeout)
	defer cancel()
	now := time.Now().UTC()
	record := compactionProvenanceRecord{
		Version:             compactionProvenanceRecordVersion,
		AccountID:           account.ID(),
		CompatibilityDomain: domain,
		CreatedAt:           now,
		LastSeenAt:          now,
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return h.cache.SetRuntime(cacheCtx, compactionProvenanceCacheNamespace, compactionContentDigest(encryptedContent), raw, compactionProvenanceTTL())
}

// compactionPayloadMayContainEncryptedState is a cheap prefilter that keeps
// full JSON walks off bodies and stream frames that cannot contain encrypted
// compaction items. Every recognized item type literal contains "compaction",
// so frames carrying only reasoning encrypted_content are skipped too.
func compactionPayloadMayContainEncryptedState(payload []byte) bool {
	return len(payload) > 0 &&
		bytes.Contains(payload, []byte(`"encrypted_content"`)) &&
		bytes.Contains(payload, []byte("compaction"))
}

func requestCompactionEncryptedContents(body []byte) []string {
	if !compactionPayloadMayContainEncryptedState(body) {
		return nil
	}
	input := gjson.GetBytes(body, "input")
	if !input.Exists() {
		return nil
	}
	contents := make([]string, 0, 1)
	inspect := func(item gjson.Result) {
		if !gjsonResultHasEncryptedCompaction(item) {
			return
		}
		if encrypted := strings.TrimSpace(item.Get("encrypted_content").String()); encrypted != "" {
			contents = append(contents, encrypted)
		}
	}
	if input.IsArray() {
		input.ForEach(func(_, item gjson.Result) bool {
			inspect(item)
			return true
		})
	} else {
		inspect(input)
	}
	return contents
}

func gjsonResultHasEncryptedCompaction(result gjson.Result) bool {
	if !result.IsObject() {
		return false
	}
	itemType := strings.TrimSpace(result.Get("type").String())
	return strings.EqualFold(itemType, "compaction") ||
		strings.EqualFold(itemType, "context_compaction") ||
		strings.EqualFold(itemType, "compaction_summary")
}

func compactionEncryptedContentsFromPayload(payload []byte) []string {
	if !compactionPayloadMayContainEncryptedState(payload) || !gjson.ValidBytes(payload) {
		return nil
	}
	root := gjson.ParseBytes(payload)
	contents := make([]string, 0, 1)
	seen := make(map[string]struct{})
	inspect := func(item gjson.Result) {
		if !gjsonResultHasEncryptedCompaction(item) {
			return
		}
		encrypted := strings.TrimSpace(item.Get("encrypted_content").String())
		if encrypted == "" {
			return
		}
		if _, ok := seen[encrypted]; ok {
			return
		}
		seen[encrypted] = struct{}{}
		contents = append(contents, encrypted)
	}
	inspect(root)
	inspect(root.Get("item"))
	for _, path := range []string{"output", "response.output"} {
		items := root.Get(path)
		if items.IsArray() {
			items.ForEach(func(_, item gjson.Result) bool {
				inspect(item)
				return true
			})
		}
	}
	return contents
}

func (h *Handler) recordCompactionProvenanceFromPayload(ctx context.Context, account *auth.Account, payload []byte) {
	for _, encryptedContent := range compactionEncryptedContentsFromPayload(payload) {
		if err := h.recordCompactionProvenance(ctx, account, encryptedContent); err != nil {
			log.Printf("record compaction provenance failed: account=%d err=%v", account.ID(), err)
		}
	}
}

func (h *Handler) resolveCompactionAffinity(ctx context.Context, body []byte) (compactionAffinityResolution, error) {
	if h == nil || h.cache == nil {
		return compactionAffinityResolution{}, nil
	}
	contents := requestCompactionEncryptedContents(body)
	if len(contents) == 0 {
		return compactionAffinityResolution{}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	cacheCtx, cancel := context.WithTimeout(ctx, compactionProvenanceCacheTimeout)
	defer cancel()

	var resolution compactionAffinityResolution
	for _, encryptedContent := range contents {
		digest := compactionContentDigest(encryptedContent)
		raw, ok, err := h.cache.GetRuntime(cacheCtx, compactionProvenanceCacheNamespace, digest)
		if err != nil {
			// Provenance is a routing optimization; a cache outage must not
			// take compaction conversations down. Fall back to the same
			// legacy scheduling used for unknown pre-deployment state.
			log.Printf("compaction provenance read failed, using normal scheduling: %v", err)
			return compactionAffinityResolution{}, nil
		}
		if !ok {
			continue
		}
		record, err := decodeCompactionProvenanceRecord(raw)
		if err != nil {
			_ = h.cache.DeleteRuntime(cacheCtx, compactionProvenanceCacheNamespace, digest)
			continue
		}
		if resolution.Known && resolution.CompatibilityDomain != record.CompatibilityDomain {
			return compactionAffinityResolution{}, errConflictingCompactionProvenance
		}
		if !resolution.Known {
			resolution = compactionAffinityResolution{
				Known:               true,
				CompatibilityDomain: record.CompatibilityDomain,
				PreferredAccountID:  record.AccountID,
			}
		}

		record.LastSeenAt = time.Now().UTC()
		refreshed, marshalErr := json.Marshal(record)
		if marshalErr == nil {
			_ = h.cache.SetRuntime(cacheCtx, compactionProvenanceCacheNamespace, digest, refreshed, compactionProvenanceTTL())
		}
	}
	return resolution, nil
}

func compactionProvenanceConflictAPIError() *api.APIError {
	return api.NewAPIError(api.ErrorCode("compaction_provenance_conflict"), "Compaction state contains conflicting upstream provenance", api.ErrorTypeInvalidRequest)
}

func compactionUpstreamUnavailableAPIError() *api.APIError {
	return api.NewAPIError(api.ErrorCode("compaction_upstream_unavailable"), "No account is available for the upstream that created this compaction state", api.ErrorTypeServer)
}

func sendCompactionProvenanceConflict(c *gin.Context) {
	api.SendErrorWithStatus(c, compactionProvenanceConflictAPIError(), http.StatusBadRequest)
}

func sendCompactionUpstreamUnavailable(c *gin.Context) {
	api.SendErrorWithStatus(c, compactionUpstreamUnavailableAPIError(), http.StatusServiceUnavailable)
}

func compactionDomainFilter(domain string, next auth.AccountFilter, traces ...*auth.SelectionTrace) auth.AccountFilter {
	domain = strings.TrimSpace(domain)
	return func(account *auth.Account) bool {
		if account == nil || accountCompactionDomain(account) != domain {
			if len(traces) > 0 {
				traces[0].Reject("compaction_domain_mismatch")
			}
			return false
		}
		return next == nil || next(account)
	}
}
