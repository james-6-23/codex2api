package auth

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const antigravityCatalogRefreshInterval = 30 * time.Minute

// RefreshAntigravityCatalog only reads the model/quota control plane. Token
// refresh and identity changes remain owned by the existing OAuth scheduler.
func (s *Store) RefreshAntigravityCatalog(ctx context.Context, account *Account) error {
	if s == nil || s.db == nil || account == nil || account.AntigravityAuthKind() != AntigravityAuthKindOAuth {
		return errors.New("Antigravity catalog requires a database-backed OAuth account")
	}
	row, err := s.db.GetAccountByID(ctx, account.ID())
	if err != nil {
		return err
	}
	if reason, _ := antigravityPersistedHardFence(row); reason != "" {
		return errors.New("Antigravity account needs authentication or administrative recovery")
	}
	proxyURL, usable := s.ResolveUsableProxyForAccount(account)
	if !usable {
		return errors.New("Antigravity catalog has no usable proxy")
	}
	client, err := NewAntigravityClient(proxyURL)
	if err != nil {
		return err
	}
	quota, err := client.fetchQuota(ctx, row.GetCredential("access_token"), row.GetCredential("project_id"))
	if err != nil {
		return err
	}
	if quota.Forbidden {
		return errors.New("Antigravity catalog access denied")
	}
	models := antigravityRefreshModels(AntigravitySyncResult{Quota: quota})
	// An empty/malformed result is not evidence that every model was withdrawn.
	if len(models) == 0 {
		return errors.New("Antigravity catalog is empty; retaining last successful catalog")
	}
	// Preserve quota groups and credits, which this list-only endpoint cannot see.
	var previous AntigravityQuotaSnapshot
	_ = json.Unmarshal([]byte(row.GetCredential("antigravity_quota")), &previous)
	quota.Groups, quota.AICredits = previous.Groups, previous.AICredits
	raw, err := json.Marshal(quota)
	if err != nil {
		return err
	}
	applied, err := s.db.MergeAccountCredentialsForGeneration(ctx, row.ID, row.CredentialGeneration, map[string]any{
		"models": models, "antigravity_quota": string(raw),
		"antigravity_catalog_source": "upstream", "antigravity_catalog_verified": true,
		"antigravity_catalog_updated_at": time.Now().UTC().Format(time.RFC3339),
	})
	if err != nil {
		return err
	}
	if !applied {
		return errors.New("Antigravity credential changed during catalog fetch; discarded stale response")
	}
	current, err := s.db.GetAccountByID(ctx, row.ID)
	if err != nil {
		return err
	}
	// Publish only the catalog, not an older credential/status snapshot.
	account.mu.Lock()
	if account.CredentialGeneration == current.CredentialGeneration {
		account.Models = normalizeModelList(current.GetCredentialStringSlice("models"))
	}
	account.mu.Unlock()
	return nil
}

func (s *Store) triggerAntigravityCatalogRefresh() {
	if s == nil || s.db == nil || !s.antigravityCatalogBatch.CompareAndSwap(false, true) {
		return
	}
	started := s.startDBBackgroundTask(func(parent context.Context) {
		defer s.antigravityCatalogBatch.Store(false)
		ctx, cancel := context.WithTimeout(parent, 5*time.Minute)
		defer cancel()
		jobs := make(chan *Account)
		var workers sync.WaitGroup
		for i := 0; i < 4; i++ {
			workers.Add(1)
			go func() {
				defer workers.Done()
				for account := range jobs {
					callCtx, stop := context.WithTimeout(ctx, 30*time.Second)
					err := s.RefreshAntigravityCatalog(callCtx, account)
					stop()
					if err != nil && ctx.Err() == nil {
						// Do not log upstream error bodies or credentials.
						log.Printf("[Antigravity catalog] account %d refresh failed; retaining cached catalog", account.ID())
					}
				}
			}()
		}
		for _, account := range s.Accounts() {
			if account == nil || account.AntigravityAuthKind() != AntigravityAuthKindOAuth || atomic.LoadInt32(&account.Disabled) != 0 {
				continue
			}
			select {
			case jobs <- account:
			case <-ctx.Done():
				close(jobs)
				workers.Wait()
				return
			}
		}
		close(jobs)
		workers.Wait()
	})
	if !started {
		s.antigravityCatalogBatch.Store(false)
	}
}

// Keep credential-independent model IDs bounded and suitable for API names.
func validAntigravityDiscoveredModelID(id string) bool {
	return len(id) > 0 && len(id) <= 200 && !strings.ContainsAny(id, "\r\n\t ")
}
