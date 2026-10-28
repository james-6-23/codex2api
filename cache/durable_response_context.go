package cache

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

// DurableResponseContextStore persists previous_response_id contexts on local
// disk. A Memory-driver deployment otherwise keeps them only in the bounded L1
// map, so an eviction, an oversize entry or a process restart makes every
// dependent HTTP continuation fail. Item bodies are content-addressed and
// reference-counted: the overlapping prefixes of one conversation chain are
// stored once, and a context row only lists the hashes of its items.
//
// The store is a cache, not a system of record. It never shares the main
// database file, and losing it only reverts to the previous L1-only behavior.
type DurableResponseContextStore struct {
	db       *sql.DB
	path     string
	ttl      time.Duration
	maxBytes int64

	writeMu sync.Mutex

	touchMu     sync.Mutex
	touched     map[string]int64
	activeReads map[string]int

	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

// DurableResponseContextOptions bounds the on-disk store. TTL is a sliding
// minimum retention: a write never expires sooner than its caller's TTL.
type DurableResponseContextOptions struct {
	TTL           time.Duration
	MaxBytes      int64
	SweepInterval time.Duration
}

// DurableResponseContextStats is a point-in-time summary for logs and ops.
type DurableResponseContextStats struct {
	Contexts  int64
	Items     int64
	ItemBytes int64
}

const (
	defaultDurableResponseContextTTL      = 48 * time.Hour
	defaultDurableResponseContextMaxBytes = int64(2 << 30)
	defaultDurableResponseContextSweep    = time.Minute
	durableResponseContextHashSize        = sha256.Size
	durableResponseContextQueryChunk      = 400
	durableResponseContextDeleteBatch     = 64
	// Callers bound backend writes to a couple of seconds for Redis. A local
	// disk transaction of a large first context may need longer; abandoning it
	// would only mark the context as a backend error.
	durableResponseContextWriteTimeout = 30 * time.Second
	durableResponseContextBusyMillis   = 15000
)

var errDurableResponseContextClosed = errors.New("durable response context store is closed")

// OpenDurableResponseContextStore opens or creates the store at path.
func OpenDurableResponseContextStore(path string, opts DurableResponseContextOptions) (*DurableResponseContextStore, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, errors.New("durable response context store path is empty")
	}
	if opts.TTL <= 0 {
		opts.TTL = defaultDurableResponseContextTTL
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = defaultDurableResponseContextMaxBytes
	}
	if opts.SweepInterval <= 0 {
		opts.SweepInterval = defaultDurableResponseContextSweep
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create durable response context directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create durable response context store: %w", err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("restrict durable response context store permissions: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close durable response context store file: %w", err)
	}
	q := url.Values{}
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", durableResponseContextBusyMillis))
	// auto_vacuum only takes effect before the first table is created.
	q.Add("_pragma", "auto_vacuum(INCREMENTAL)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(NORMAL)")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("open durable response context store: %w", err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(0)
	s := &DurableResponseContextStore{
		db:          db,
		path:        path,
		ttl:         opts.TTL,
		maxBytes:    opts.MaxBytes,
		touched:     make(map[string]int64),
		activeReads: make(map[string]int),
		stop:        make(chan struct{}),
		done:        make(chan struct{}),
	}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	go s.sweepLoop(opts.SweepInterval)
	return s, nil
}

func (s *DurableResponseContextStore) migrate() error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS response_context_items (
			id INTEGER PRIMARY KEY,
			hash BLOB NOT NULL UNIQUE,
			body BLOB NOT NULL,
			bytes INTEGER NOT NULL,
			refs INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS response_contexts (
			key TEXT PRIMARY KEY,
			hashes BLOB NOT NULL,
			logical_bytes INTEGER NOT NULL,
			expires_at INTEGER NOT NULL,
			accessed_at INTEGER NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS response_contexts_expires_at ON response_contexts(expires_at)`,
		`CREATE INDEX IF NOT EXISTS response_contexts_accessed_at ON response_contexts(accessed_at)`,
	}
	for _, statement := range statements {
		if _, err := s.db.Exec(statement); err != nil {
			return fmt.Errorf("migrate durable response context store: %w", err)
		}
	}
	return nil
}

// Path reports the backing file.
func (s *DurableResponseContextStore) Path() string { return s.path }

// TTL reports the minimum retention of a written context.
func (s *DurableResponseContextStore) TTL() time.Duration { return s.ttl }

// MaxBytes reports the retained item-body budget.
func (s *DurableResponseContextStore) MaxBytes() int64 { return s.maxBytes }

// Close stops the sweeper, flushes recorded reads and closes the database.
func (s *DurableResponseContextStore) Close() error {
	if s == nil {
		return nil
	}
	var err error
	s.closeOnce.Do(func() {
		close(s.stop)
		<-s.done
		s.flushTouches()
		err = s.db.Close()
	})
	return err
}

func (s *DurableResponseContextStore) closed() bool {
	select {
	case <-s.stop:
		return true
	default:
		return false
	}
}

func durableResponseContextHash(item json.RawMessage) [durableResponseContextHashSize]byte {
	return sha256.Sum256(item)
}

// SetResponseContext replaces the context stored under key. Items keep their
// exact bytes and order; the caller has already normalized them.
func (s *DurableResponseContextStore) SetResponseContext(ctx context.Context, key string, items []json.RawMessage, ttl time.Duration) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil
	}
	if s.closed() {
		return errDurableResponseContextClosed
	}
	var logical int64
	hashes := make([]byte, 0, len(items)*durableResponseContextHashSize)
	for _, item := range items {
		logical += int64(len(item))
		hash := durableResponseContextHash(item)
		hashes = append(hashes, hash[:]...)
	}
	if len(items) == 0 || logical > s.maxBytes {
		// A context larger than the whole store cannot be retained. Remove any
		// older value so a stale parent is never served for this key.
		return s.deleteKey(ctx, key)
	}
	if ttl < s.ttl {
		ttl = s.ttl
	}
	now := time.Now()
	expiresAt := now.Add(ttl).UnixMilli()

	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), durableResponseContextWriteTimeout)
	defer cancel()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var previous []byte
	switch err := tx.QueryRowContext(ctx, `SELECT hashes FROM response_contexts WHERE key = ?`, key).Scan(&previous); {
	case errors.Is(err, sql.ErrNoRows):
		previous = nil
	case err != nil:
		return err
	}
	// Reference the new items before releasing the old ones, so items shared
	// by both versions never reach zero inside the transaction.
	increment, err := tx.PrepareContext(ctx, `UPDATE response_context_items SET refs = refs + 1 WHERE hash = ?`)
	if err != nil {
		return err
	}
	defer increment.Close()
	insert, err := tx.PrepareContext(ctx, `INSERT INTO response_context_items (hash, body, bytes, refs) VALUES (?, ?, ?, 1)`)
	if err != nil {
		return err
	}
	defer insert.Close()
	for index, item := range items {
		hash := hashes[index*durableResponseContextHashSize : (index+1)*durableResponseContextHashSize]
		result, err := increment.ExecContext(ctx, hash)
		if err != nil {
			return err
		}
		if affected, err := result.RowsAffected(); err != nil {
			return err
		} else if affected > 0 {
			continue
		}
		if _, err := insert.ExecContext(ctx, hash, []byte(item), len(item)); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO response_contexts (key, hashes, logical_bytes, expires_at, accessed_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(key) DO UPDATE SET hashes = excluded.hashes, logical_bytes = excluded.logical_bytes,
			expires_at = excluded.expires_at, accessed_at = excluded.accessed_at`,
		key, hashes, logical, expiresAt, now.UnixMilli()); err != nil {
		return err
	}
	if err := releaseDurableResponseContextHashes(ctx, tx, previous); err != nil {
		return err
	}
	return tx.Commit()
}

// SetResponseContextReadOnly implements the optional no-copy writer used by
// the proxy. The store never mutates the bodies it receives.
func (s *DurableResponseContextStore) SetResponseContextReadOnly(ctx context.Context, key string, items []json.RawMessage, ttl time.Duration) error {
	return s.SetResponseContext(ctx, key, items, ttl)
}

// GetResponseContext returns nil for a missing, expired or corrupt context.
func (s *DurableResponseContextStore) GetResponseContext(ctx context.Context, key string) ([]json.RawMessage, error) {
	result, err := s.GetResponseContextBounded(ctx, key, 0)
	if err != nil || result.Status != ResponseContextReadFound {
		return nil, err
	}
	return result.Items, nil
}

// GetResponseContextBounded reads one context from a single snapshot. A
// positive maxWireBytes rejects a context before its bodies are loaded.
func (s *DurableResponseContextStore) GetResponseContextBounded(ctx context.Context, key string, maxWireBytes int64) (ResponseContextReadResult, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return ResponseContextReadResult{Status: ResponseContextReadMiss}, nil
	}
	if s.closed() {
		return ResponseContextReadResult{}, errDurableResponseContextClosed
	}
	s.beginRead(key)
	defer s.endRead(key)
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ResponseContextReadResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	var hashes []byte
	var logical, expiresAt int64
	switch err := tx.QueryRowContext(ctx, `SELECT hashes, logical_bytes, expires_at FROM response_contexts WHERE key = ?`, key).Scan(&hashes, &logical, &expiresAt); {
	case errors.Is(err, sql.ErrNoRows):
		return ResponseContextReadResult{Status: ResponseContextReadMiss}, nil
	case err != nil:
		return ResponseContextReadResult{}, err
	}
	if time.Now().UnixMilli() >= expiresAt {
		return ResponseContextReadResult{Status: ResponseContextReadMiss}, nil
	}
	if maxWireBytes > 0 && logical > maxWireBytes {
		return ResponseContextReadResult{Status: ResponseContextReadTooLarge}, nil
	}
	if len(hashes) == 0 || len(hashes)%durableResponseContextHashSize != 0 {
		return ResponseContextReadResult{Status: ResponseContextReadCorrupt}, nil
	}
	bodies, err := loadDurableResponseContextBodies(ctx, tx, hashes)
	if err != nil {
		return ResponseContextReadResult{}, err
	}
	count := len(hashes) / durableResponseContextHashSize
	items := make([]json.RawMessage, count)
	var total int64
	for index := 0; index < count; index++ {
		var hash [durableResponseContextHashSize]byte
		copy(hash[:], hashes[index*durableResponseContextHashSize:])
		body, ok := bodies[hash]
		if !ok {
			return ResponseContextReadResult{Status: ResponseContextReadCorrupt}, nil
		}
		items[index] = body
		total += int64(len(body))
	}
	if total != logical {
		return ResponseContextReadResult{Status: ResponseContextReadCorrupt}, nil
	}
	s.recordTouch(key)
	return ResponseContextReadResult{Status: ResponseContextReadFound, Items: items}, nil
}

func loadDurableResponseContextBodies(ctx context.Context, tx *sql.Tx, hashes []byte) (map[[durableResponseContextHashSize]byte]json.RawMessage, error) {
	unique := make(map[[durableResponseContextHashSize]byte]struct{}, len(hashes)/durableResponseContextHashSize)
	ordered := make([][durableResponseContextHashSize]byte, 0, len(hashes)/durableResponseContextHashSize)
	for offset := 0; offset < len(hashes); offset += durableResponseContextHashSize {
		var hash [durableResponseContextHashSize]byte
		copy(hash[:], hashes[offset:])
		if _, seen := unique[hash]; !seen {
			unique[hash] = struct{}{}
			ordered = append(ordered, hash)
		}
	}
	bodies := make(map[[durableResponseContextHashSize]byte]json.RawMessage, len(ordered))
	for start := 0; start < len(ordered); start += durableResponseContextQueryChunk {
		end := start + durableResponseContextQueryChunk
		if end > len(ordered) {
			end = len(ordered)
		}
		args := make([]any, 0, end-start)
		for _, hash := range ordered[start:end] {
			args = append(args, hash[:])
		}
		rows, err := tx.QueryContext(ctx, `SELECT hash, body FROM response_context_items WHERE hash IN (?`+strings.Repeat(",?", len(args)-1)+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var hash, body []byte
			if err := rows.Scan(&hash, &body); err != nil {
				_ = rows.Close()
				return nil, err
			}
			if len(hash) != durableResponseContextHashSize {
				continue
			}
			var key [durableResponseContextHashSize]byte
			copy(key[:], hash)
			bodies[key] = json.RawMessage(body)
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return bodies, nil
}

func releaseDurableResponseContextHashes(ctx context.Context, tx *sql.Tx, hashes []byte) error {
	if len(hashes) == 0 {
		return nil
	}
	decrement, err := tx.PrepareContext(ctx, `UPDATE response_context_items SET refs = refs - 1 WHERE hash = ?`)
	if err != nil {
		return err
	}
	defer decrement.Close()
	remove, err := tx.PrepareContext(ctx, `DELETE FROM response_context_items WHERE hash = ? AND refs <= 0`)
	if err != nil {
		return err
	}
	defer remove.Close()
	for offset := 0; offset+durableResponseContextHashSize <= len(hashes); offset += durableResponseContextHashSize {
		if _, err := decrement.ExecContext(ctx, hashes[offset:offset+durableResponseContextHashSize]); err != nil {
			return err
		}
	}
	for offset := 0; offset+durableResponseContextHashSize <= len(hashes); offset += durableResponseContextHashSize {
		if _, err := remove.ExecContext(ctx, hashes[offset:offset+durableResponseContextHashSize]); err != nil {
			return err
		}
	}
	return nil
}

func (s *DurableResponseContextStore) deleteKey(ctx context.Context, key string) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), durableResponseContextWriteTimeout)
	defer cancel()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.deleteKeysLocked(ctx, []string{key})
}

func (s *DurableResponseContextStore) deleteKeysLocked(ctx context.Context, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, key := range keys {
		var hashes []byte
		switch err := tx.QueryRowContext(ctx, `SELECT hashes FROM response_contexts WHERE key = ?`, key).Scan(&hashes); {
		case errors.Is(err, sql.ErrNoRows):
			continue
		case err != nil:
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM response_contexts WHERE key = ?`, key); err != nil {
			return err
		}
		if err := releaseDurableResponseContextHashes(ctx, tx, hashes); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// recordTouch defers the access-time write of a read to the sweeper. A read
// path must not wait on the single writer lock.
func (s *DurableResponseContextStore) recordTouch(key string) {
	s.touchMu.Lock()
	s.touched[key] = time.Now().UnixMilli()
	s.touchMu.Unlock()
}

func (s *DurableResponseContextStore) beginRead(key string) {
	s.touchMu.Lock()
	s.activeReads[key]++
	s.touchMu.Unlock()
}

func (s *DurableResponseContextStore) endRead(key string) {
	s.touchMu.Lock()
	s.activeReads[key]--
	if s.activeReads[key] == 0 {
		delete(s.activeReads, key)
	}
	s.touchMu.Unlock()
}

// Called under writeMu, so drained touches cannot be hidden from another
// maintenance transaction. The read path only holds touchMu for map updates.
func (s *DurableResponseContextStore) takeTouches(keys []string) (map[string]int64, map[string]bool) {
	s.touchMu.Lock()
	defer s.touchMu.Unlock()
	touched := s.touched
	s.touched = make(map[string]int64)
	reading := make(map[string]bool)
	for _, key := range keys {
		if s.activeReads[key] > 0 {
			reading[key] = true
		}
	}
	return touched, reading
}

func (s *DurableResponseContextStore) restoreTouches(touched map[string]int64) {
	s.touchMu.Lock()
	defer s.touchMu.Unlock()
	for key, at := range touched {
		if at > s.touched[key] {
			s.touched[key] = at
		}
	}
}

func (s *DurableResponseContextStore) applyTouches(ctx context.Context, tx *sql.Tx, touched map[string]int64) error {
	ttl := s.ttl.Milliseconds()
	for key, at := range touched {
		// A read keeps an active conversation alive for another full TTL.
		if _, err := tx.ExecContext(ctx, `UPDATE response_contexts SET accessed_at = MAX(accessed_at, ?),
			expires_at = MAX(expires_at, ?) WHERE key = ?`, at, at+ttl, key); err != nil {
			return err
		}
	}
	return nil
}

func (s *DurableResponseContextStore) flushTouches() {
	ctx, cancel := context.WithTimeout(context.Background(), durableResponseContextWriteTimeout)
	defer cancel()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_ = s.flushTouchesLocked(ctx)
}

func (s *DurableResponseContextStore) flushTouchesLocked(ctx context.Context) (err error) {
	touched, _ := s.takeTouches(nil)
	if len(touched) == 0 {
		return nil
	}
	defer func() {
		if err != nil {
			s.restoreTouches(touched)
		}
	}()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.applyTouches(ctx, tx, touched); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *DurableResponseContextStore) sweepLoop(interval time.Duration) {
	defer close(s.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			s.flushTouches()
			if expired, evicted, err := s.Sweep(time.Now()); err != nil {
				log.Printf("持久化 response context 清理失败: %v", err)
			} else if expired > 0 || evicted > 0 {
				log.Printf("持久化 response context 清理: expired=%d evicted=%d", expired, evicted)
			}
		}
	}
}

// Sweep removes expired contexts, then the least recently used ones until
// retained item bodies fit in 90% of MaxBytes, and returns freed pages.
func (s *DurableResponseContextStore) Sweep(now time.Time) (expired, evicted int, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*durableResponseContextWriteTimeout)
	defer cancel()
	for {
		keys, err := s.selectKeys(ctx, `SELECT key FROM response_contexts WHERE expires_at <= ? LIMIT ?`, now.UnixMilli(), durableResponseContextDeleteBatch)
		if err != nil {
			return expired, evicted, err
		}
		if len(keys) == 0 {
			break
		}
		deleted, err := s.deleteExpiredBatch(ctx, keys, now)
		if err != nil {
			return expired, evicted, err
		}
		expired += deleted
		if deleted == 0 {
			// In-flight reads may still be renewing this batch. Try it on the
			// next sweep rather than spinning while those readers finish.
			break
		}
	}
	for {
		deleted, err := s.evictLRUBatch(ctx, evicted > 0)
		if err != nil {
			return expired, evicted, err
		}
		if deleted == 0 {
			break
		}
		evicted += deleted
	}
	if expired > 0 || evicted > 0 {
		s.writeMu.Lock()
		_, err = s.db.ExecContext(ctx, `PRAGMA incremental_vacuum`)
		s.writeMu.Unlock()
	}
	return expired, evicted, err
}

func (s *DurableResponseContextStore) selectKeys(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

// selectEvictionKeys picks the least recently used contexts whose combined
// logical size covers excess. Shared items make that an upper bound on what
// is freed, so Sweep re-measures and repeats instead of over-evicting.
func (s *DurableResponseContextStore) selectEvictionKeys(ctx context.Context, excess int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, logical_bytes FROM response_contexts ORDER BY accessed_at ASC LIMIT ?`, durableResponseContextDeleteBatch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var keys []string
	var covered int64
	for rows.Next() {
		var key string
		var logical int64
		if err := rows.Scan(&key, &logical); err != nil {
			return nil, err
		}
		keys = append(keys, key)
		covered += logical
		if covered >= excess {
			break
		}
	}
	return keys, rows.Err()
}

// Keep writes and flushed read touches from refreshing a selected LRU key
// between measuring the budget, selecting candidates and deleting the batch.
func (s *DurableResponseContextStore) evictLRUBatch(ctx context.Context, alreadyEvicted bool) (int, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.flushTouchesLocked(ctx); err != nil {
		return 0, err
	}
	stats, err := s.Stats(ctx)
	if err != nil {
		return 0, err
	}
	target := s.maxBytes / 10 * 9
	if stats.ItemBytes <= s.maxBytes && (!alreadyEvicted || stats.ItemBytes <= target) {
		return 0, nil
	}
	keys, err := s.selectEvictionKeys(ctx, stats.ItemBytes-target)
	if err != nil {
		return 0, err
	}
	if err := s.deleteKeysLocked(ctx, keys); err != nil {
		return 0, err
	}
	return len(keys), nil
}

// Expiration candidates can be renewed after selection. Recheck expiration in
// the delete transaction and release only the hashes of rows actually removed.
func (s *DurableResponseContextStore) deleteExpiredBatch(ctx context.Context, keys []string, now time.Time) (deleted int, err error) {
	if len(keys) == 0 {
		return 0, nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	touched, reading := s.takeTouches(keys)
	defer func() {
		if err != nil {
			s.restoreTouches(touched)
		}
	}()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := s.applyTouches(ctx, tx, touched); err != nil {
		return 0, err
	}
	for _, key := range keys {
		// Readers registered before this snapshot may have observed a live
		// context and not recorded their touch yet. Later reads of an expired
		// row miss, so they cannot introduce another successful renewal.
		if reading[key] {
			continue
		}
		var hashes []byte
		switch err := tx.QueryRowContext(ctx, `DELETE FROM response_contexts
			WHERE key = ? AND expires_at <= ? RETURNING hashes`, key, now.UnixMilli()).Scan(&hashes); {
		case errors.Is(err, sql.ErrNoRows):
			continue
		case err != nil:
			return 0, err
		}
		if err := releaseDurableResponseContextHashes(ctx, tx, hashes); err != nil {
			return 0, err
		}
		deleted++
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return deleted, nil
}

// Stats counts retained contexts and unique item bodies.
func (s *DurableResponseContextStore) Stats(ctx context.Context) (DurableResponseContextStats, error) {
	var stats DurableResponseContextStats
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM response_contexts`).Scan(&stats.Contexts); err != nil {
		return stats, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(bytes), 0) FROM response_context_items`).Scan(&stats.Items, &stats.ItemBytes); err != nil {
		return stats, err
	}
	return stats, nil
}

// DurableResponseContextCache marks a TokenCache whose response contexts
// survive L1 eviction and process restarts even though its other runtime
// state stays process-local.
type DurableResponseContextCache interface {
	DurableResponseContexts() bool
}

// IsDurableResponseContextCache reports whether tc persists response contexts.
func IsDurableResponseContextCache(tc TokenCache) bool {
	durable, ok := tc.(DurableResponseContextCache)
	return ok && durable.DurableResponseContexts()
}

type durableResponseContextTokenCache struct {
	TokenCache
	store *DurableResponseContextStore
}

// WithDurableResponseContexts serves response contexts from store and every
// other TokenCache operation from base. Closing the returned cache does not
// close base or store; their owners close them.
func WithDurableResponseContexts(base TokenCache, store *DurableResponseContextStore) TokenCache {
	if base == nil || store == nil {
		return base
	}
	return &durableResponseContextTokenCache{TokenCache: base, store: store}
}

func (c *durableResponseContextTokenCache) DurableResponseContexts() bool { return true }

func (c *durableResponseContextTokenCache) Close() error { return nil }

func (c *durableResponseContextTokenCache) SetResponseContext(ctx context.Context, responseID string, items []json.RawMessage, ttl time.Duration) error {
	return c.store.SetResponseContext(ctx, responseID, items, ttl)
}

func (c *durableResponseContextTokenCache) SetResponseContextReadOnly(ctx context.Context, responseID string, items []json.RawMessage, ttl time.Duration) error {
	return c.store.SetResponseContextReadOnly(ctx, responseID, items, ttl)
}

func (c *durableResponseContextTokenCache) GetResponseContext(ctx context.Context, responseID string) ([]json.RawMessage, error) {
	return c.store.GetResponseContext(ctx, responseID)
}

func (c *durableResponseContextTokenCache) GetResponseContextBounded(ctx context.Context, responseID string, maxWireBytes int64) (ResponseContextReadResult, error) {
	return c.store.GetResponseContextBounded(ctx, responseID, maxWireBytes)
}
