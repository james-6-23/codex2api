package proxy

import (
	"log"

	"github.com/codex2api/cache"
	"github.com/gin-gonic/gin"
)

// responseContextUnavailableMessage follows OpenAI's previous_response_not_found
// wording and tells the client how to recover: drop the parent reference and
// resend the complete conversation.
const responseContextUnavailableMessage = "Previous response not found; resend the full conversation input without previous_response_id"

func (k responseCacheLookupKind) String() string {
	switch k {
	case responseCacheLookupMiss:
		return "miss"
	case responseCacheLookupHit:
		return "hit"
	case responseCacheLookupExpired:
		return "expired"
	case responseCacheLookupKnownEvicted:
		return "evicted"
	case responseCacheLookupKnownOversize:
		return "oversize"
	case responseCacheLookupReconstructionTooLarge:
		return "reconstruction_too_large"
	case responseCacheLookupBackendCorrupt:
		return "backend_corrupt"
	case responseCacheLookupBackendError:
		return "backend_error"
	case responseCacheLookupBackendPending:
		return "backend_pending"
	default:
		return "unknown"
	}
}

func (s responseCacheLookupSource) String() string {
	switch s {
	case responseCacheSourceLocal:
		return "local"
	case responseCacheSourceBackend:
		return "backend"
	default:
		return "none"
	}
}

// shortResponseID keeps enough of a response ID to correlate log lines
// without writing whole conversation identifiers to the log.
func shortResponseID(id string) string {
	const keep = 16
	if len(id) <= keep {
		return id
	}
	return id[:keep] + "…"
}

// logResponseContextUnavailable records why a continuation was rejected. The
// access log only shows the status, which cannot tell a restart, an eviction
// and an oversize context apart.
func logResponseContextUnavailable(c *gin.Context, status int, reason string, prepared responsesBodyPreparation) {
	path := ""
	if c != nil {
		path = c.FullPath()
	}
	stats := GetResponseCacheStats()
	respCache.mu.RLock()
	backend := "none"
	if respCache.runtimeCache != nil {
		backend = respCache.runtimeCache.Driver()
		if cache.IsDurableResponseContextCache(respCache.runtimeCache) {
			backend += "+durable"
		}
	}
	respCache.mu.RUnlock()
	log.Printf("[response-context] unavailable status=%d reason=%s lookup=%s source=%s previous_response_id=%s api_key_id=%d path=%s l1_entries=%d l1_bytes=%d byte_evictions=%d oversize_rejections=%d backend=%s",
		status, reason, prepared.CacheLookup.Kind, prepared.CacheLookup.Source, shortResponseID(prepared.PreviousResponseID),
		requestAPIKeyID(c), path, stats.Entries, stats.Bytes, stats.ByteEvictions, stats.OversizeRejections, backend)
}
