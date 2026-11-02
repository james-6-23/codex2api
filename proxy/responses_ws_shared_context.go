package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/google/uuid"
)

const (
	nativeWSContextNamespace = "native-ws-context"
	nativeWSPendingNamespace = "native-ws-context-pending"
	nativeWSContextVersion   = 1
	nativeWSMetadataBytes    = int64(4096)
	nativeWSDomainMaxBytes   = 1024
	nativeWSPendingMaxBytes  = int64(64)
	nativeWSCommitPoll       = 100 * time.Millisecond
)

type nativeWSSharedContext struct {
	Version     int               `json:"version"`
	Items       []json.RawMessage `json:"items"`
	AccountID   int64             `json:"account_id,omitempty"`
	Generation  int64             `json:"generation,omitempty"`
	Domain      string            `json:"domain,omitempty"`
	ExpiresAt   time.Time         `json:"expires_at"`
	Unavailable string            `json:"unavailable,omitempty"`
}

type nativeWSSharedWrite struct {
	key      string
	response string
	serial   uint64
	record   nativeWSSharedContext
}

func nativeWSReplayAccountFilter(filter auth.AccountFilter, source *responsesWSReplaySource) auth.AccountFilter {
	if source == nil || source.previous == nil || source.previous.nativeProvenance == nil {
		return filter
	}
	previous := source.previous
	hasOpaque := false
	for _, item := range previous.Items {
		if strings.Contains(string(item), `"encrypted_content"`) {
			hasOpaque = true
			break
		}
	}
	if !hasOpaque || previous.nativeProvenance.accountID <= 0 {
		return filter
	}
	return func(account *auth.Account) bool {
		return account.ID() == previous.nativeProvenance.accountID && (filter == nil || filter(account))
	}
}

// Native WebSocket provenance needs cross-instance runtime state. A durable
// local response-context store keeps every other runtime value in process
// memory, so it must not turn on the shared native path.
func nativeWSSharedBackend() cache.TokenCache {
	respCache.mu.RLock()
	defer respCache.mu.RUnlock()
	if respCache.runtimeCache == nil || !respCache.runtimeCache.SharedAcrossInstances() {
		return nil
	}
	return respCache.runtimeCache
}

func nativeWSContextLookup(owner, responseID string) responseCacheLookupResult {
	local := nativeWSLocalLookup(owner, responseID)
	if local.Kind == responseCacheLookupHit || responseID == "" {
		return local
	}
	backend := nativeWSSharedBackend()
	if backend == nil {
		return local
	}
	ctx, cancel := context.WithTimeout(context.Background(), responseCacheBackendSyncTimeout)
	defer cancel()
	respCache.mu.RLock()
	config := respCache.config
	respCache.mu.RUnlock()
	key := responseCacheStoreKey(owner, responseID)
	raw, found, err := readNativeWSRuntime(ctx, backend, nativeWSRuntimeRead{namespace: nativeWSContextNamespace, key: key,
		maxBytes: responseContextWireLimit(config) + nativeWSMetadataBytes})
	if errors.Is(err, cache.ErrRuntimeSnapshotTooLarge) {
		return responseCacheLookupResult{Kind: responseCacheLookupReconstructionTooLarge}
	}
	if err != nil {
		return responseCacheLookupResult{Kind: responseCacheLookupBackendError, Err: err}
	}
	if !found {
		if local.Kind == responseCacheLookupBackendError {
			return local
		}
		return responseCacheLookupResult{Kind: responseCacheLookupMiss, remoteMiss: true}
	}
	return hydrateNativeWSContext(key, raw, config)
}

type nativeWSRuntimeRead struct {
	namespace string
	key       string
	maxBytes  int64
}

func readNativeWSRuntime(ctx context.Context, backend cache.TokenCache, read nativeWSRuntimeRead) ([]byte, bool, error) {
	if bounded, ok := backend.(cache.BoundedRuntimeReader); ok {
		return bounded.GetRuntimeBounded(ctx, read.namespace, read.key, read.maxBytes)
	}
	raw, found, err := backend.GetRuntime(ctx, read.namespace, read.key)
	if int64(len(raw)) > read.maxBytes {
		return nil, false, cache.ErrRuntimeSnapshotTooLarge
	}
	return raw, found, err
}

func hydrateNativeWSContext(key string, raw []byte, config responseCacheConfig) responseCacheLookupResult {
	var record nativeWSSharedContext
	if json.Unmarshal(raw, &record) != nil || record.Version != nativeWSContextVersion || record.ExpiresAt.IsZero() || len(record.Domain) > nativeWSDomainMaxBytes {
		return responseCacheLookupResult{Kind: responseCacheLookupBackendCorrupt}
	}
	if !time.Now().Before(record.ExpiresAt) {
		return responseCacheLookupResult{Kind: responseCacheLookupExpired}
	}
	if record.Unavailable != "" {
		kind := responseCacheLookupBackendError
		if record.Unavailable == "oversize" {
			kind = responseCacheLookupReconstructionTooLarge
		}
		return responseCacheLookupResult{Kind: kind}
	}
	items, err := cache.NormalizeResponseContextItems(record.Items)
	if err != nil {
		return responseCacheLookupResult{Kind: responseCacheLookupBackendCorrupt}
	}
	if len(items) > config.maxItems || responseContextLogicalBytes(items) > config.reconstructMaxBytes {
		return responseCacheLookupResult{Kind: responseCacheLookupReconstructionTooLarge}
	}
	provenance := &nativeWSReplayProvenance{accountID: record.AccountID, generation: record.Generation, domain: record.Domain}
	items, admitted, oversize, serial := admitResponseCacheWithTicket(key, items)
	setNativeWSLocalMetadata(key, nativeWSLocalMetadata{serial: serial, provenance: provenance, expiresAt: record.ExpiresAt})
	return responseCacheLookupResult{Kind: responseCacheLookupHit, Source: responseCacheSourceBackend,
		Items: items, nativeProvenance: provenance, Promoted: admitted, oversizeBypass: !admitted && oversize}
}

type nativeWSLocalMetadata struct {
	serial     uint64
	provenance *nativeWSReplayProvenance
	expiresAt  time.Time
}

func setNativeWSLocalMetadata(key string, metadata nativeWSLocalMetadata) {
	respCache.mu.Lock()
	defer respCache.mu.Unlock()
	if entry := respCache.store[key]; entry != nil && entry.serial == metadata.serial {
		entry.nativeProvenance = metadata.provenance
		entry.expiresAt = metadata.expiresAt
	}
}

func persistNativeWSContext(write nativeWSSharedWrite) {
	backend := nativeWSSharedBackend()
	if backend == nil {
		return
	}
	if err := writeNativeWSContext(backend, write); err != nil {
		write.record.Items = nil
		write.record.Unavailable = "backend_error"
		ctx, cancel := context.WithTimeout(context.Background(), responseCacheBackendSyncTimeout)
		defer cancel()
		_ = storeNativeWSRecord(ctx, backend, write)
	}
}

func writeNativeWSContext(backend cache.TokenCache, write nativeWSSharedWrite) (err error) {
	writer := responseCacheBackendWriter
	writer.start()
	defer writer.finish()
	defer func() { recordResponseCacheBackendWriteResult(write.key, write.response, write.serial, err) }()
	bytes := responseContextLogicalBytes(write.record.Items)
	waitCtx, cancel := context.WithTimeout(context.Background(), responseCacheBackendWaitTimeout)
	_, err = writer.acquire(waitCtx, bytes)
	cancel()
	if err != nil {
		return err
	}
	defer writer.release(bytes)
	ctx, cancel := context.WithTimeout(context.Background(), responseCacheBackendWriteTimeout)
	defer cancel()
	return storeNativeWSRecord(ctx, backend, write)
}

func storeNativeWSRecord(ctx context.Context, backend cache.TokenCache, write nativeWSSharedWrite) error {
	raw, err := json.Marshal(write.record)
	if err != nil {
		return err
	}
	ttl := time.Until(write.record.ExpiresAt)
	if ttl <= 0 {
		return nil
	}
	return backend.SetRuntime(ctx, nativeWSContextNamespace, write.key, raw, ttl)
}

func beginNativeWSSharedCommit(key string) func() {
	backend := nativeWSSharedBackend()
	owners, ok := backend.(cache.RuntimeOwnerStore)
	if !ok {
		return func() {}
	}
	owner, _ := json.Marshal(uuid.NewString())
	ctx, cancel := context.WithTimeout(context.Background(), responseCacheBackendSyncTimeout)
	_, err := owners.ClaimRuntimeOwner(ctx, nativeWSPendingNamespace, key, owner, nativeWSCommitWait+responseCacheBackendWaitTimeout+responseCacheBackendWriteTimeout)
	cancel()
	if err != nil {
		log.Printf("登记 Redis 原生 WS 提交屏障失败: %v", err)
		return func() {}
	}
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), responseCacheBackendSyncTimeout)
		defer cancel()
		_, _ = owners.CompareAndDeleteRuntimeOwner(ctx, nativeWSPendingNamespace, key, owner)
	}
}

func waitNativeWSSharedCommit(ctx context.Context, owner, responseID string) error {
	backend := nativeWSSharedBackend()
	if backend == nil || responseID == "" || !strings.HasPrefix(owner, nativeWSCachePrefix) {
		return nil
	}
	if nativeWSLocalLookup(owner, responseID).Kind == responseCacheLookupHit {
		return nil
	}
	waitCtx, cancel := context.WithTimeout(ctx, nativeWSCommitWait)
	defer cancel()
	ticker := time.NewTicker(nativeWSCommitPoll)
	defer ticker.Stop()
	for {
		readCtx, readCancel := context.WithTimeout(waitCtx, responseCacheBackendSyncTimeout)
		_, pending, err := readNativeWSRuntime(readCtx, backend,
			nativeWSRuntimeRead{namespace: nativeWSPendingNamespace, key: responseCacheStoreKey(owner, responseID), maxBytes: nativeWSPendingMaxBytes})
		readCancel()
		if err != nil || !pending {
			return err
		}
		select {
		case <-waitCtx.Done():
			return waitCtx.Err()
		case <-ticker.C:
		}
	}
}
