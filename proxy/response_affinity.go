package proxy

import (
	"context"
	"encoding/json"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/tidwall/gjson"
)

// responseAccountAffinityTTL bounds how long a response id can pin a
// continuation to the account that created it. A response id is a one-hop
// lookup aid, not a long-lived session key.
const responseAccountAffinityTTL = time.Hour

const responseAccountAffinityNamespace = "codex-response-account-v1"

type responseAccountAffinity struct {
	AccountID    int64     `json:"account_id"`
	Owner        string    `json:"owner"`
	AffinityKey  string    `json:"affinity_key,omitempty"`
	Model        string    `json:"model,omitempty"`
	UpstreamType string    `json:"upstream_type,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// responseIDFromPayload accepts both SSE event envelopes (response.id) and
// ordinary Responses JSON objects (id), as compact transports may return
// either shape.
func responseIDFromPayload(payload []byte) string {
	if id := strings.TrimSpace(gjson.GetBytes(payload, "response.id").String()); id != "" {
		return id
	}
	return strings.TrimSpace(gjson.GetBytes(payload, "id").String())
}

func responseAccountUpstreamType(account *auth.Account) string {
	if account == nil {
		return ""
	}
	account.Mu().RLock()
	defer account.Mu().RUnlock()
	return account.UpstreamType
}

var responseAffinityLocal = struct {
	sync.RWMutex
	entries map[string]responseAccountAffinity
}{entries: make(map[string]responseAccountAffinity)}

func cleanupResponseAccountAffinityExpired(now time.Time) {
	responseAffinityLocal.Lock()
	for key, record := range responseAffinityLocal.entries {
		if !record.ExpiresAt.After(now) {
			delete(responseAffinityLocal.entries, key)
		}
	}
	responseAffinityLocal.Unlock()
}

func responseAffinityKey(owner, responseID string) string {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		owner = "anon"
	}
	return owner + "|" + strings.TrimSpace(responseID)
}

// recordResponseAccountAffinity records a successful response's account
// ownership. The owner (API key namespace) is part of the key and payload to
// prevent cross-user previous_response_id injection.
func (h *Handler) recordResponseAccountAffinity(owner, responseID string, accountID int64, affinityKey, model, upstreamType string) {
	responseID = strings.TrimSpace(responseID)
	if responseID == "" || len(responseID) > 256 || accountID == 0 {
		return
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		owner = "anon"
	}
	now := time.Now()
	record := responseAccountAffinity{AccountID: accountID, Owner: owner, AffinityKey: strings.TrimSpace(affinityKey), Model: strings.TrimSpace(model), UpstreamType: strings.TrimSpace(upstreamType), CreatedAt: now, ExpiresAt: now.Add(responseAccountAffinityTTL)}
	key := responseAffinityKey(owner, responseID)
	responseAffinityLocal.Lock()
	if len(responseAffinityLocal.entries) >= 4096 {
		for k, existing := range responseAffinityLocal.entries {
			if !existing.ExpiresAt.After(now) {
				delete(responseAffinityLocal.entries, k)
			}
		}
	}
	responseAffinityLocal.entries[key] = record
	responseAffinityLocal.Unlock()
	if h == nil || h.cache == nil {
		return
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_ = h.cache.SetRuntime(ctx, responseAccountAffinityNamespace, key, payload, responseAccountAffinityTTL)
}

func lookupResponseAccountAffinity(ctx context.Context, tc cache.TokenCache, owner, responseID string) (responseAccountAffinity, bool) {
	responseID = strings.TrimSpace(responseID)
	if responseID == "" || len(responseID) > 256 {
		return responseAccountAffinity{}, false
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		owner = "anon"
	}
	key := responseAffinityKey(owner, responseID)
	now := time.Now()
	responseAffinityLocal.RLock()
	record, ok := responseAffinityLocal.entries[key]
	responseAffinityLocal.RUnlock()
	if ok {
		if record.ExpiresAt.After(now) && record.Owner == owner {
			return record, true
		}
		responseAffinityLocal.Lock()
		delete(responseAffinityLocal.entries, key)
		responseAffinityLocal.Unlock()
	}
	if tc == nil {
		return responseAccountAffinity{}, false
	}
	if ctx == nil {
		ctx = context.Background()
	}
	payload, found, err := tc.GetRuntime(ctx, responseAccountAffinityNamespace, key)
	if err != nil {
		log.Printf("读取 response account affinity 失败: owner=%s response_id=%s err=%v", owner, responseID, err)
		return responseAccountAffinity{}, false
	}
	if !found {
		return responseAccountAffinity{}, false
	}
	if err := json.Unmarshal(payload, &record); err != nil || record.AccountID == 0 || record.Owner != owner || !record.ExpiresAt.After(now) {
		return responseAccountAffinity{}, false
	}
	responseAffinityLocal.Lock()
	responseAffinityLocal.entries[key] = record
	responseAffinityLocal.Unlock()
	return record, true
}
