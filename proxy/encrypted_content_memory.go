package proxy

import (
	"bytes"
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	encryptedMemoryTTL         = 30 * time.Minute
	encryptedMemoryMaxSessions = 4096
	encryptedMemoryMaxDigests  = 256
	encryptedErrorMaxBytes     = 64 << 10
)

type encryptedDigest = [sha256.Size]byte

// encryptedScopeKey namespaces remembered rejections. Only the raw downstream
// session ID is digested; the other dimensions are plain, non-secret values
// (the API key's stable identity is the same derivation used for
// prompt_cache_key) and are deliberately kept out of the hash so that nothing
// credential-related ever feeds a general-purpose hash.
type encryptedScopeKey struct {
	owner       int64
	keyIdentity string
	account     int64
	generation  int64
	session     encryptedDigest
}

type encryptedMemoryEntry struct {
	key     encryptedScopeKey
	digests map[encryptedDigest]struct{}
	expires time.Time
}

// Only hashes are retained. Entries are scoped to a downstream owner/session
// and the rejecting account's credential generation, and are bounded by LRU/TTL.
type encryptedContentMemory struct {
	mu      sync.Mutex
	entries map[encryptedScopeKey]*list.Element
	lru     list.List
	now     func() time.Time
}

var rejectedEncryptedContent = &encryptedContentMemory{now: time.Now}

func (m *encryptedContentMemory) get(key encryptedScopeKey) map[encryptedDigest]struct{} {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.entries[key]
	if e == nil {
		return nil
	}
	entry := e.Value.(*encryptedMemoryEntry)
	if !m.now().Before(entry.expires) {
		delete(m.entries, key)
		m.lru.Remove(e)
		return nil
	}
	m.lru.MoveToFront(e)
	copy := make(map[encryptedDigest]struct{}, len(entry.digests))
	for digest := range entry.digests {
		copy[digest] = struct{}{}
	}
	return copy
}

func (m *encryptedContentMemory) mark(key encryptedScopeKey, digests []encryptedDigest) {
	if len(digests) == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.entries == nil {
		m.entries = make(map[encryptedScopeKey]*list.Element)
	}
	now := m.now()
	e := m.entries[key]
	if e == nil {
		for len(m.entries) >= encryptedMemoryMaxSessions {
			old := m.lru.Back()
			delete(m.entries, old.Value.(*encryptedMemoryEntry).key)
			m.lru.Remove(old)
		}
		e = m.lru.PushFront(&encryptedMemoryEntry{key: key})
		m.entries[key] = e
	}
	entry := e.Value.(*encryptedMemoryEntry)
	if entry.digests == nil || !now.Before(entry.expires) {
		entry.digests = make(map[encryptedDigest]struct{})
	}
	for _, digest := range digests {
		if len(entry.digests) >= encryptedMemoryMaxDigests {
			break
		}
		entry.digests[digest] = struct{}{}
	}
	entry.expires = now.Add(encryptedMemoryTTL)
	m.lru.MoveToFront(e)
}

func encryptedItemDigest(item gjson.Result) (encryptedDigest, bool) {
	typ := strings.TrimSpace(item.Get("type").String())
	if typ != "reasoning" && !isEncryptedCompactionItemType(typ) {
		return encryptedDigest{}, false
	}
	value := item.Get("encrypted_content")
	if value.Type != gjson.String || value.String() == "" {
		return encryptedDigest{}, false
	}
	return sha256.Sum256([]byte(value.String())), true
}

func encryptedPayloadDigests(body []byte) []encryptedDigest {
	if !bytes.Contains(body, []byte(`"encrypted_content"`)) {
		return nil
	}
	var digests []encryptedDigest
	input := gjson.GetBytes(body, "input")
	visit := func(item gjson.Result) {
		if digest, ok := encryptedItemDigest(item); ok && len(digests) < encryptedMemoryMaxDigests {
			digests = append(digests, digest)
		}
	}
	if input.IsArray() {
		input.ForEach(func(_, item gjson.Result) bool { visit(item); return true })
	} else if input.IsObject() {
		visit(input)
	}
	return digests
}

func stripRememberedEncryptedContent(body []byte, invalid map[encryptedDigest]struct{}) []byte {
	if len(invalid) == 0 || !bytes.Contains(body, []byte(`"encrypted_content"`)) {
		return body
	}
	input := gjson.GetBytes(body, "input")
	if input.IsObject() {
		if digest, ok := encryptedItemDigest(input); ok {
			if _, rejected := invalid[digest]; rejected {
				next, err := sjson.SetBytes(body, "input", []json.RawMessage{})
				if err == nil {
					return next
				}
			}
		}
		return body
	}
	if !input.IsArray() {
		return body
	}
	changed := false
	items := make([]json.RawMessage, 0, len(input.Array()))
	input.ForEach(func(_, item gjson.Result) bool {
		digest, ok := encryptedItemDigest(item)
		if _, rejected := invalid[digest]; ok && rejected {
			// These are the same reasoning/compaction items removed by the
			// existing invalid_encrypted_content recovery, with exact hash scope.
			changed = true
			return true
		}
		items = append(items, json.RawMessage(item.Raw))
		return true
	})
	if !changed {
		return body
	}
	next, err := sjson.SetBytes(body, "input", items)
	if err != nil {
		return body
	}
	return next
}

type encryptedContentAttempt struct {
	memory *encryptedContentMemory
	key    encryptedScopeKey
}

func prepareEncryptedContentAttempt(ctx context.Context, account *auth.Account, body []byte, session string, headers http.Header) ([]byte, *encryptedContentAttempt) {
	if account == nil || !bytes.Contains(body, []byte(`"encrypted_content"`)) {
		return body, nil
	}
	if local := resolveDownstreamAffinityID(headers); local != "" {
		session = local
	}
	if session == "" {
		session = ResolveExplicitSessionID(headers, body)
	}
	if session == "" {
		return body, nil
	}
	var owner int64
	if identity := PayloadRuleIdentityFromContext(ctx); identity != nil {
		owner = identity.APIKeyID
	}
	// Static API keys have no database ID. Namespace them by the key's stable
	// non-secret identity too; neither credentials nor raw conversation IDs
	// are kept.
	key := encryptedScopeKey{
		owner:       owner,
		keyIdentity: deterministicPromptCacheKey(strings.TrimPrefix(strings.TrimSpace(headers.Get("Authorization")), "Bearer "), nil),
		account:     account.ID(),
		generation:  account.GetCredentialGeneration(),
		session:     sha256.Sum256([]byte(session)),
	}
	a := &encryptedContentAttempt{memory: rejectedEncryptedContent, key: key}
	return stripRememberedEncryptedContent(body, a.memory.get(key)), a
}

func (a *encryptedContentAttempt) observeResponse(resp *http.Response, sentBody []byte) {
	if a == nil || resp == nil || resp.Body == nil {
		return
	}
	digests := encryptedPayloadDigests(sentBody)
	if len(digests) == 0 {
		return
	}
	stream := resp.StatusCode < 400 && (strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") || gjson.GetBytes(sentBody, "stream").Bool())
	if !stream && resp.StatusCode != http.StatusBadRequest {
		return
	}
	resp.Body = &encryptedErrorObserver{ReadCloser: resp.Body, stream: stream, record: func(payload []byte) {
		body := responseFailedErrorBody(payload)
		// Missing encrypted_content is a different failure; it cannot prove
		// that any supplied ciphertext was rejected.
		if isRejectedEncryptedContentFailure(body) {
			a.memory.mark(a.key, digests)
		}
	}}
}

func isRejectedEncryptedContentFailure(body []byte) bool {
	for _, path := range []string{"error.code", "detail.code", "code"} {
		if strings.EqualFold(strings.TrimSpace(gjson.GetBytes(body, path).String()), "invalid_encrypted_content") {
			return true
		}
	}
	for _, path := range []string{"error.message", "message"} {
		message := strings.ToLower(gjson.GetBytes(body, path).String())
		if strings.Contains(message, "encrypted content") && (strings.Contains(message, "could not be verified") || strings.Contains(message, "could not be decrypted")) {
			return true
		}
	}
	return false
}

// Observe bounded error envelopes as the caller reads. Never pre-read a stream,
// change bytes, or retain generated content beyond one bounded SSE event.
type encryptedErrorObserver struct {
	io.ReadCloser
	mu                          sync.Mutex // Close may race a read when an upstream deadline expires.
	stream                      bool
	pending, event              []byte
	eventName                   string
	droppingLine, droppingEvent bool
	record                      func([]byte)
}

func (r *encryptedErrorObserver) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.stream {
		if len(r.pending)+n <= encryptedErrorMaxBytes && !r.droppingEvent {
			r.pending = append(r.pending, p[:n]...)
		} else {
			r.pending = nil
			r.droppingEvent = true
		}
		if err == io.EOF {
			r.flushJSON()
		}
		return n, err
	}
	data := p[:n]
	for len(data) > 0 {
		i := bytes.IndexByte(data, '\n')
		part := data
		if i >= 0 {
			part = data[:i]
		}
		if !r.droppingLine && len(r.pending)+len(part) <= encryptedErrorMaxBytes {
			r.pending = append(r.pending, part...)
		} else {
			r.pending = nil
			r.droppingLine = true
			r.droppingEvent = true
		}
		if i < 0 {
			break
		}
		if !r.droppingLine {
			r.line(bytes.TrimSuffix(r.pending, []byte{'\r'}))
		}
		r.pending = r.pending[:0]
		r.droppingLine = false
		data = data[i+1:]
	}
	if err == io.EOF {
		if len(r.pending) > 0 && !r.droppingLine {
			r.line(r.pending)
		}
		r.line(nil)
	}
	return n, err
}

func (r *encryptedErrorObserver) line(line []byte) {
	if len(line) == 0 {
		if !r.droppingEvent && json.Valid(r.event) {
			typ := gjson.GetBytes(r.event, "type").String()
			if typ == "error" || typ == "response.failed" || r.eventName == "error" || r.eventName == "response.failed" {
				r.record(r.event)
			}
		}
		r.event, r.eventName, r.droppingEvent = r.event[:0], "", false
		return
	}
	if bytes.HasPrefix(line, []byte("event:")) {
		r.eventName = strings.TrimSpace(string(line[6:]))
		return
	}
	if bytes.HasPrefix(line, []byte("data:")) && !r.droppingEvent {
		part := bytes.TrimPrefix(line[5:], []byte(" "))
		if len(r.event)+len(part)+1 > encryptedErrorMaxBytes {
			r.event = nil
			r.droppingEvent = true
			return
		}
		if len(r.event) > 0 {
			r.event = append(r.event, '\n')
		}
		r.event = append(r.event, part...)
	}
}

func (r *encryptedErrorObserver) flushJSON() {
	if !r.droppingEvent && json.Valid(r.pending) {
		r.record(r.pending)
	}
	r.pending = nil
}

func (r *encryptedErrorObserver) Close() error {
	r.mu.Lock()
	if !r.stream {
		r.flushJSON()
	}
	r.mu.Unlock()
	return r.ReadCloser.Close()
}
