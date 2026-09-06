package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/google/uuid"
)

type liveSession struct {
	record     *liveCallRecord
	account    *auth.Account
	releaseKey func()
	reserved   bool
	cancel     context.CancelFunc
}

type liveCallStore struct {
	mu       sync.Mutex
	sessions map[string]*liveSession
	handler  *Handler
}

func newLiveCallStore(h *Handler) *liveCallStore {
	return &liveCallStore{
		sessions: make(map[string]*liveSession),
		handler:  h,
	}
}

func (h *Handler) liveCalls() *liveCallStore {
	if h == nil {
		return nil
	}
	if h.liveStore != nil {
		return h.liveStore
	}
	h.liveStore = newLiveCallStore(h)
	return h.liveStore
}

func (s *liveCallStore) save(ctx context.Context, record *liveCallRecord, account *auth.Account, releaseKey func()) error {
	if s == nil || record == nil {
		return fmt.Errorf("live store unavailable")
	}
	if err := s.acquireLeases(ctx, record); err != nil {
		return err
	}
	// Promote the controller before the record is published: once the session
	// is in the map, the record is shared and may only be mutated under s.mu.
	record.Controller = liveControllerObserver
	if err := s.persist(ctx, record); err != nil {
		s.releaseLeases(ctx, record)
		return err
	}
	watchCtx, cancel := context.WithCancel(context.Background())
	session := &liveSession{
		record:     record,
		account:    account,
		releaseKey: releaseKey,
		reserved:   account != nil,
		cancel:     cancel,
	}
	s.mu.Lock()
	s.sessions[record.CallHash] = session
	s.mu.Unlock()
	go s.watch(watchCtx, session)
	return nil
}

func (s *liveCallStore) get(ctx context.Context, callID string) (*liveCallRecord, *liveSession, error) {
	if s == nil {
		return nil, nil, fmt.Errorf("live store unavailable")
	}
	callID = strings.TrimSpace(callID)
	hash := hashLiveCallID(callID)
	s.mu.Lock()
	session := s.sessions[hash]
	if session != nil && session.record != nil && session.record.CallID == callID {
		// Hand out a snapshot: the canonical record keeps being mutated under
		// s.mu (claimProxy/finalize), and callers read fields without the lock.
		// Store methods re-resolve the canonical record via CallHash, so a
		// snapshot round-trips through them correctly.
		snapshot := *session.record
		s.mu.Unlock()
		return &snapshot, session, nil
	}
	s.mu.Unlock()
	record, ok, err := s.load(ctx, hash)
	if err != nil {
		return nil, nil, err
	}
	if !ok || record == nil || record.CallID != callID {
		return nil, nil, errLiveCallNotFound
	}
	return record, session, nil
}

func (s *liveCallStore) claimProxy(ctx context.Context, record *liveCallRecord) (string, bool, error) {
	if record == nil {
		return "", false, errLiveCallNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current := record
	if session := s.sessions[record.CallHash]; session != nil && session.record != nil {
		current = session.record
	}
	if current.Controller == liveControllerClosed || current.Controller == liveControllerProxy {
		return "", false, nil
	}
	owner := uuid.NewString()
	current.Controller = liveControllerProxy
	current.ControllerOwner = owner
	if err := s.persistLocked(ctx, current); err != nil {
		return "", false, err
	}
	return owner, true, nil
}

func (s *liveCallStore) finalize(record *liveCallRecord) {
	if s == nil || record == nil {
		return
	}
	s.mu.Lock()
	session := s.sessions[record.CallHash]
	current := record
	if session != nil && session.record != nil {
		current = session.record
	}
	if current.Controller == liveControllerClosed {
		s.mu.Unlock()
		return
	}
	current.Controller = liveControllerClosed
	current.ControllerOwner = ""
	usageLogged := current.UsageLogged
	current.UsageLogged = true
	_ = s.persistLocked(context.Background(), current)
	if session != nil {
		delete(s.sessions, record.CallHash)
	}
	s.mu.Unlock()

	if session != nil && session.cancel != nil {
		session.cancel()
	}
	s.releaseLeases(context.Background(), current)
	if session != nil && session.reserved && session.account != nil && s.handler != nil && s.handler.store != nil {
		s.handler.store.Release(session.account)
		session.reserved = false
	}
	if session != nil && session.releaseKey != nil {
		session.releaseKey()
		session.releaseKey = nil
	}
	if !usageLogged && s.handler != nil {
		s.handler.logLiveUsage(current)
	}
}

func (s *liveCallStore) watch(ctx context.Context, session *liveSession) {
	if session == nil || session.record == nil {
		return
	}
	refresh := time.NewTicker(liveLeaseRefreshInterval)
	defer refresh.Stop()
	timer := time.NewTimer(time.Until(session.record.ExpiresAt))
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			s.finalize(session.record)
			return
		case <-refresh.C:
			s.mu.Lock()
			closed := session.record.Controller == liveControllerClosed
			s.mu.Unlock()
			if closed {
				return
			}
			_ = s.persist(ctx, session.record)
		}
	}
}

func (s *liveCallStore) persist(ctx context.Context, record *liveCallRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.persistLocked(ctx, record)
}

func (s *liveCallStore) persistLocked(ctx context.Context, record *liveCallRecord) error {
	if s == nil || s.handler == nil || s.handler.cache == nil || record == nil {
		return nil
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return err
	}
	ttl := time.Until(record.ExpiresAt) + 5*time.Minute
	if ttl < time.Minute {
		ttl = time.Minute
	}
	return s.handler.cache.SetRuntime(ctx, liveCallCacheNamespace, record.CallHash, payload, ttl)
}

func (s *liveCallStore) load(ctx context.Context, callHash string) (*liveCallRecord, bool, error) {
	if s == nil || s.handler == nil || s.handler.cache == nil {
		return nil, false, nil
	}
	raw, ok, err := s.handler.cache.GetRuntime(ctx, liveCallCacheNamespace, callHash)
	if err != nil || !ok {
		return nil, ok, err
	}
	var record liveCallRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return nil, false, err
	}
	return &record, true, nil
}

func (s *liveCallStore) acquireLeases(ctx context.Context, record *liveCallRecord) error {
	if s == nil || s.handler == nil || s.handler.cache == nil || record == nil {
		return nil
	}
	ttl := time.Until(record.ExpiresAt) + 5*time.Minute
	if ttl < time.Minute {
		ttl = time.Minute
	}
	if record.AccountID > 0 {
		ok, err := s.handler.cache.AcquireLease(ctx, liveAccountLeaseNamespace, liveLeaseKey(record.AccountID, record.LeaseID), record.LeaseID, ttl)
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("live account lease unavailable")
		}
	}
	if record.APIKeyID > 0 {
		ok, err := s.handler.cache.AcquireLease(ctx, liveKeyLeaseNamespace, liveLeaseKey(record.APIKeyID, record.LeaseID), record.LeaseID, ttl)
		if err != nil {
			if record.AccountID > 0 {
				_ = s.handler.cache.ReleaseLease(ctx, liveAccountLeaseNamespace, liveLeaseKey(record.AccountID, record.LeaseID), record.LeaseID)
			}
			return err
		}
		if !ok {
			if record.AccountID > 0 {
				_ = s.handler.cache.ReleaseLease(ctx, liveAccountLeaseNamespace, liveLeaseKey(record.AccountID, record.LeaseID), record.LeaseID)
			}
			return fmt.Errorf("live API key lease unavailable")
		}
	}
	return nil
}

func (s *liveCallStore) releaseLeases(ctx context.Context, record *liveCallRecord) {
	if s == nil || s.handler == nil || s.handler.cache == nil || record == nil {
		return
	}
	if record.AccountID > 0 {
		_ = s.handler.cache.ReleaseLease(ctx, liveAccountLeaseNamespace, liveLeaseKey(record.AccountID, record.LeaseID), record.LeaseID)
	}
	if record.APIKeyID > 0 {
		_ = s.handler.cache.ReleaseLease(ctx, liveKeyLeaseNamespace, liveLeaseKey(record.APIKeyID, record.LeaseID), record.LeaseID)
	}
}

func liveLeaseKey(id int64, leaseID string) string {
	return strconv.FormatInt(id, 10) + ":" + leaseID
}

func (h *Handler) logLiveUsage(record *liveCallRecord) {
	if h == nil || record == nil {
		return
	}
	duration := int(time.Since(record.CreatedAt) / time.Millisecond)
	if duration < 0 {
		duration = 0
	}
	model := stringsOrDefault(record.Model, "gpt-live")
	input := &database.UsageLogInput{
		RequestID:         record.RequestID,
		UpstreamRequestID: record.UpstreamRequestID,
		UpstreamProxyID:   record.UpstreamProxyID,
		UpstreamProxyName: record.UpstreamProxyName,
		AccountID:         record.AccountID,
		Channel:           database.UpstreamChannelCodex,
		ClientIP:          record.ClientIP,
		ClientUserAgent:   record.UserAgent,
		Endpoint:          "/v1/live",
		InboundEndpoint:   record.InboundEndpoint,
		UpstreamEndpoint:  "/backend-api/codex/realtime/calls",
		Model:             model,
		EffectiveModel:    model,
		StatusCode:        200,
		DurationMs:        duration,
		APIKeyID:          record.APIKeyID,
	}
	if liveUsageLogForTest != nil {
		liveUsageLogForTest(input)
	}
	h.logUsage(input)
}

func stringsOrDefault(value, fallback string) string {
	if value = strings.TrimSpace(value); value == "" {
		return fallback
	}
	return value
}

var (
	errLiveCallNotFound = fmt.Errorf("live call not found")
	liveUsageLogForTest func(*database.UsageLogInput)
)
