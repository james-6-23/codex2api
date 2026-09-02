package proxy

import (
	"context"
	"fmt"
	"time"

	"github.com/codex2api/database"
)

const (
	responseCacheConfigPollInterval = 5 * time.Second
	responseCacheConfigReadTimeout  = 3 * time.Second
)

type ResponseCacheAppliedConfig struct {
	LocalMaxBytes       int64
	LocalMaxEntryBytes  int64
	ReconstructMaxBytes int64
	WritePolicy         string
	Generation          int64
	MaxEntries          int
	TTL                 time.Duration
	MaxItems            int
}

type ResponseCacheConfigSyncStatus struct {
	LastSuccessfulSyncAt time.Time
	LastSyncError        string
}

type ResponseCacheOpsSnapshot struct {
	Stats               ResponseCacheStats
	EffectiveConfig     ResponseCacheAppliedConfig
	AppliedConfig       ResponseCacheAppliedConfig
	LastConfigSyncAt    time.Time
	LastConfigSyncError string
}

type responseCacheSettingsReader interface {
	GetResponseCacheSettings(context.Context) (database.ResponseCacheSettings, error)
}

func ApplyResponseCacheSettings(settings database.ResponseCacheSettings) bool {
	if database.ValidateResponseCacheSettings(settings) != nil {
		return false
	}

	respCache.mu.Lock()
	defer respCache.mu.Unlock()
	if settings.Generation <= respCache.generation {
		return false
	}
	respCache.config.maxBytes = settings.LocalMaxBytes
	respCache.config.maxEntryBytes = settings.LocalMaxEntryBytes
	respCache.config.reconstructMaxBytes = settings.ReconstructMaxBytes
	respCache.config.writePolicy = database.NormalizeResponseCacheWritePolicy(settings.WritePolicy)
	respCache.generation = settings.Generation
	respCache.enforceConfigLocked()
	return true
}

func GetResponseCacheAppliedConfig() ResponseCacheAppliedConfig {
	respCache.mu.RLock()
	config := responseCacheAppliedConfigLocked()
	respCache.mu.RUnlock()
	return config
}

func responseCacheAppliedConfigLocked() ResponseCacheAppliedConfig {
	return ResponseCacheAppliedConfig{
		LocalMaxBytes:       respCache.config.maxBytes,
		LocalMaxEntryBytes:  respCache.config.maxEntryBytes,
		ReconstructMaxBytes: respCache.config.reconstructMaxBytes,
		WritePolicy:         respCache.config.writePolicy,
		Generation:          respCache.generation,
		MaxEntries:          respCache.config.maxEntries,
		TTL:                 respCache.config.ttl,
		MaxItems:            respCache.config.maxItems,
	}
}

func GetResponseCacheConfigSyncStatus() ResponseCacheConfigSyncStatus {
	respCache.mu.RLock()
	status := ResponseCacheConfigSyncStatus{
		LastSuccessfulSyncAt: respCache.lastSyncAt,
		LastSyncError:        respCache.lastSyncErr,
	}
	respCache.mu.RUnlock()
	return status
}

func GetResponseCacheOpsSnapshot() ResponseCacheOpsSnapshot {
	respCache.mu.RLock()
	applied := responseCacheAppliedConfigLocked()
	snapshot := ResponseCacheOpsSnapshot{
		Stats:               respCache.stats,
		EffectiveConfig:     applied,
		AppliedConfig:       applied,
		LastConfigSyncAt:    respCache.lastSyncAt,
		LastConfigSyncError: respCache.lastSyncErr,
	}
	respCache.mu.RUnlock()
	return snapshot
}

func LoadResponseCacheSettings(ctx context.Context, reader responseCacheSettingsReader) error {
	settings, err := readResponseCacheSettings(ctx, reader, responseCacheConfigReadTimeout)
	if err != nil {
		if ctx == nil || ctx.Err() == nil {
			recordResponseCacheConfigSyncError(err)
		}
		return err
	}
	if err := database.ValidateResponseCacheSettings(settings); err != nil {
		recordResponseCacheConfigSyncError(err)
		return err
	}
	ApplyResponseCacheSettings(settings)
	recordResponseCacheConfigSyncSuccess(time.Now())
	return nil
}

func StartResponseCacheSettingsPoller(parent context.Context, db *database.DB) bool {
	return startResponseCacheSettingsPoller(
		parent,
		db,
		responseCacheConfigPollInterval,
		responseCacheConfigReadTimeout,
	)
}

func startResponseCacheSettingsPoller(
	parent context.Context,
	db *database.DB,
	interval time.Duration,
	readTimeout time.Duration,
) bool {
	if db == nil {
		return false
	}
	if parent == nil {
		parent = context.Background()
	}
	return db.RunBackgroundTask(func(lifecycle context.Context) {
		taskCtx, cancel := context.WithCancel(lifecycle)
		stopParent := context.AfterFunc(parent, cancel)
		defer func() {
			stopParent()
			cancel()
		}()
		runResponseCacheSettingsPoller(
			taskCtx,
			db,
			interval,
			readTimeout,
		)
	})
}

func runResponseCacheSettingsPoller(
	ctx context.Context,
	reader responseCacheSettingsReader,
	interval time.Duration,
	readTimeout time.Duration,
) {
	if ctx == nil {
		ctx = context.Background()
	}
	if interval <= 0 {
		interval = responseCacheConfigPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			settings, err := readResponseCacheSettings(ctx, reader, readTimeout)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				recordResponseCacheConfigSyncError(err)
				continue
			}
			if err := database.ValidateResponseCacheSettings(settings); err != nil {
				recordResponseCacheConfigSyncError(err)
				continue
			}
			ApplyResponseCacheSettings(settings)
			recordResponseCacheConfigSyncSuccess(time.Now())
		}
	}
}

func readResponseCacheSettings(
	parent context.Context,
	reader responseCacheSettingsReader,
	timeout time.Duration,
) (database.ResponseCacheSettings, error) {
	if reader == nil {
		return database.ResponseCacheSettings{}, fmt.Errorf("response cache settings reader is nil")
	}
	if parent == nil {
		parent = context.Background()
	}
	if err := parent.Err(); err != nil {
		return database.ResponseCacheSettings{}, err
	}
	if timeout <= 0 || timeout > responseCacheConfigReadTimeout {
		timeout = responseCacheConfigReadTimeout
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	return reader.GetResponseCacheSettings(ctx)
}

func recordResponseCacheConfigSyncSuccess(at time.Time) {
	respCache.mu.Lock()
	respCache.lastSyncAt = at
	respCache.lastSyncErr = ""
	respCache.mu.Unlock()
}

func recordResponseCacheConfigSyncError(err error) {
	if err == nil {
		return
	}
	respCache.mu.Lock()
	respCache.lastSyncErr = err.Error()
	respCache.mu.Unlock()
}
