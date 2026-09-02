package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/cache"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

var errResponsesWSSessionPreempted = errors.New("responses websocket session preempted by a newer request")

const (
	responsesWSSessionPreemptNamespace     = "responses-ws-preempt"
	responsesWSSessionPreemptOwnerTTL      = 2 * time.Hour
	responsesWSSessionPreemptWatchInterval = 2 * time.Second
	responsesWSSessionPreemptCacheTimeout  = 2 * time.Second
	responsesWSSessionPreemptHandoffWait   = 2 * time.Second
)

type responsesWSSessionPreemptContextKey struct{}

type responsesWSSessionPreemptKey struct {
	apiKeyID    int64
	scopeHash   string
	sessionHash string
}

type responsesWSSessionPreemptEntry struct {
	generation uint64
	cancel     func()
	done       chan struct{}
}

type responsesWSSessionPreemptRegistry struct {
	mu     sync.Mutex
	next   uint64
	active map[responsesWSSessionPreemptKey]responsesWSSessionPreemptEntry
}

func (r *responsesWSSessionPreemptRegistry) Begin(key responsesWSSessionPreemptKey, cancel func()) (cleanup func(), previousDone <-chan struct{}, replaced bool) {
	if r == nil || key.apiKeyID <= 0 || strings.TrimSpace(key.scopeHash) == "" || strings.TrimSpace(key.sessionHash) == "" {
		return func() {}, nil, false
	}
	r.mu.Lock()
	if r.active == nil {
		r.active = make(map[responsesWSSessionPreemptKey]responsesWSSessionPreemptEntry)
	}
	r.next++
	generation := r.next
	done := make(chan struct{})
	previous, replaced := r.active[key]
	r.active[key] = responsesWSSessionPreemptEntry{generation: generation, cancel: cancel, done: done}
	r.mu.Unlock()

	if replaced && previous.cancel != nil {
		previous.cancel()
	}
	var cleanupOnce sync.Once
	cleanup = func() {
		cleanupOnce.Do(func() {
			close(done)
			r.mu.Lock()
			if current, ok := r.active[key]; ok && current.generation == generation {
				delete(r.active, key)
			}
			r.mu.Unlock()
		})
	}
	if replaced {
		previousDone = previous.done
	}
	return cleanup, previousDone, replaced
}

func newResponsesWSSessionPreemptKey(c *gin.Context, rawBody []byte, identity requestSessionIdentity) (responsesWSSessionPreemptKey, bool) {
	apiKeyID := requestAPIKeyID(c)
	if apiKeyID <= 0 {
		return responsesWSSessionPreemptKey{}, false
	}
	scopeHash := responsesWSSessionPreemptScopeHash(c, identity)
	sessionHash := responsesWSSessionPreemptIdentityHash(rawBody, identity, responsesWSTransportLane(c, identity))
	if scopeHash == "" || sessionHash == "" {
		return responsesWSSessionPreemptKey{}, false
	}
	return responsesWSSessionPreemptKey{apiKeyID: apiKeyID, scopeHash: scopeHash, sessionHash: sessionHash}, true
}

func responsesWSSessionPreemptScopeHash(c *gin.Context, identity requestSessionIdentity) string {
	row := apiKeyRowFromContext(c)
	if row == nil || row.ID <= 0 {
		return ""
	}
	allowed := append([]int64(nil), row.AllowedGroupIDs...)
	allowed = normalizedResponsesWSPreemptGroupIDs(allowed)
	split := normalizedResponsesWSPreemptGroupIDs(row.Limits.NoAffinityGroupIDs)
	mode := "all"
	effectiveGroups := allowed
	if len(split) > 0 {
		switch {
		case !identity.hasRequestFingerprint:
			mode = "split-only"
			effectiveGroups = split
		case len(allowed) > 0:
			mode = "allowed-only"
		case identity.hasRequestFingerprint:
			mode = "exclude-split"
			effectiveGroups = split
		}
	} else if len(allowed) > 0 {
		mode = "allowed-only"
	}
	var builder strings.Builder
	builder.WriteString(strings.TrimSpace(row.Limits.ResolveUpstreamChannel()))
	builder.WriteByte('\x00')
	builder.WriteString(mode)
	for _, groupID := range effectiveGroups {
		_, _ = fmt.Fprintf(&builder, "\x00%d", groupID)
	}
	sum := sha256.Sum256([]byte(builder.String()))
	return hex.EncodeToString(sum[:16])
}

func normalizedResponsesWSPreemptGroupIDs(ids []int64) []int64 {
	if len(ids) == 0 {
		return nil
	}
	seen := make(map[int64]struct{}, len(ids))
	result := make([]int64, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}

func responsesWSTransportLane(c *gin.Context, identity requestSessionIdentity) string {
	if c == nil || c.Request == nil {
		return ""
	}
	base := strings.TrimSpace(identity.explicitUpstreamID)
	if base == "" && identity.hasDownstreamAffinity {
		base = strings.TrimSpace(identity.affinityID)
	}
	if base == "" {
		return ""
	}
	lane := ResolveCodexWebsocketTransportSessionKey(base, c.Request.Header)
	if lane == base {
		return ""
	}
	return lane
}

func responsesWSSessionPreemptIdentityHash(rawBody []byte, identity requestSessionIdentity, transportLane string) string {
	turnState := strings.TrimSpace(codexWSTurnContinuationToken(rawBody))
	previousResponseID := strings.TrimSpace(gjson.GetBytes(rawBody, "previous_response_id").String())
	contentSeed := strings.TrimSpace(deriveContentSessionSeed(rawBody))
	source := ""
	switch {
	case identity.hasDownstreamAffinity && strings.TrimSpace(identity.affinityID) != "":
		source = "affinity:" + strings.TrimSpace(identity.affinityID)
	case strings.TrimSpace(identity.explicitUpstreamID) != "":
		source = "explicit:" + strings.TrimSpace(identity.explicitUpstreamID)
	case turnState != "":
		source = "turn:" + turnState
	case previousResponseID != "":
		source = "previous:" + previousResponseID
	case contentSeed != "":
		source = "content:" + contentSeed
	default:
		return ""
	}
	if transportLane = strings.TrimSpace(transportLane); transportLane != "" {
		// A Codex session tree shares session-id while each parent/subagent has its
		// own thread-id. Only newer work in the same logical thread may preempt an
		// active owner; sibling agents must be allowed to run concurrently.
		source += "\x00transport:" + transportLane
	}
	streamID := strings.TrimSpace(gjson.GetBytes(rawBody, "stream_id").String())
	sum := sha256.Sum256([]byte(source + "\x00stream:" + streamID))
	return hex.EncodeToString(sum[:])
}

func responsesWSSessionPreemptRemoteKey(key responsesWSSessionPreemptKey) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s\x00%s", key.apiKeyID, key.scopeHash, key.sessionHash)))
	return hex.EncodeToString(sum[:])
}

func (h *Handler) responsesWSSessionPreemptOwnerStore() cache.RuntimeOwnerStore {
	if h == nil || h.cache == nil || !h.cache.SharedAcrossInstances() {
		return nil
	}
	store, _ := h.cache.(cache.RuntimeOwnerStore)
	return store
}

func (h *Handler) beginResponsesWSSessionPreemption(ctx context.Context, c *gin.Context, rawBody []byte, identity requestSessionIdentity) (context.Context, func(), bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	if h == nil || ctx.Err() != nil {
		return ctx, func() {}, false
	}
	if armed, _ := ctx.Value(responsesWSSessionPreemptContextKey{}).(bool); armed {
		return ctx, func() {}, true
	}
	key, ok := newResponsesWSSessionPreemptKey(c, rawBody, identity)
	if !ok {
		return ctx, func() {}, false
	}

	preemptCtx, cancel := context.WithCancelCause(ctx)
	var preemptOnce sync.Once
	preempt := func() {
		preemptOnce.Do(func() { cancel(errResponsesWSSessionPreempted) })
	}
	owner := []byte(uuid.NewString())
	ownerStore := h.responsesWSSessionPreemptOwnerStore()
	remoteClaimed := false
	if ownerStore != nil {
		claimCtx, claimCancel := context.WithTimeout(ctx, responsesWSSessionPreemptCacheTimeout)
		_, claimErr := ownerStore.ClaimRuntimeOwner(
			claimCtx,
			responsesWSSessionPreemptNamespace,
			responsesWSSessionPreemptRemoteKey(key),
			owner,
			responsesWSSessionPreemptOwnerTTL,
		)
		claimCancel()
		remoteClaimed = claimErr == nil
	}

	cleanupLocal, previousDone, replaced := h.responsesWSSessionPreemptions.Begin(key, preempt)
	if replaced && previousDone != nil {
		timer := time.NewTimer(responsesWSSessionPreemptHandoffWait)
		select {
		case <-previousDone:
		case <-ctx.Done():
		case <-timer.C:
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
	}
	stopWatch := func() {}
	if remoteClaimed {
		stopWatch = watchResponsesWSSessionPreemptOwner(preemptCtx, ownerStore, key, owner, preempt)
	}

	cleanup := func() {
		stopWatch()
		cleanupLocal()
		if remoteClaimed {
			releaseCtx, releaseCancel := context.WithTimeout(context.Background(), responsesWSSessionPreemptCacheTimeout)
			_, _ = ownerStore.CompareAndDeleteRuntimeOwner(
				releaseCtx,
				responsesWSSessionPreemptNamespace,
				responsesWSSessionPreemptRemoteKey(key),
				owner,
			)
			releaseCancel()
		}
		cancel(nil)
	}
	return context.WithValue(preemptCtx, responsesWSSessionPreemptContextKey{}, true), cleanup, true
}

func watchResponsesWSSessionPreemptOwner(ctx context.Context, ownerStore cache.RuntimeOwnerStore, key responsesWSSessionPreemptKey, owner []byte, onLost func()) func() {
	if ownerStore == nil || onLost == nil || len(owner) == 0 {
		return func() {}
	}
	stop := make(chan struct{})
	var stopOnce sync.Once
	go func() {
		ticker := time.NewTicker(responsesWSSessionPreemptWatchInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-ticker.C:
				refreshCtx, refreshCancel := context.WithTimeout(context.Background(), responsesWSSessionPreemptCacheTimeout)
				owned, err := ownerStore.CompareAndRefreshRuntimeOwner(
					refreshCtx,
					responsesWSSessionPreemptNamespace,
					responsesWSSessionPreemptRemoteKey(key),
					owner,
					responsesWSSessionPreemptOwnerTTL,
				)
				refreshCancel()
				if err == nil && !owned {
					onLost()
					return
				}
			}
		}
	}()
	return func() { stopOnce.Do(func() { close(stop) }) }
}

func isResponsesWSSessionPreempted(ctx context.Context) bool {
	return ctx != nil && errors.Is(context.Cause(ctx), errResponsesWSSessionPreempted)
}
