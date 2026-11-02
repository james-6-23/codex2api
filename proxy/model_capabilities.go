package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
)

// Store only capability fields. Instructions, credentials and provider-specific
// opaque payloads from the model descriptor are never persisted or synthesized.
var codexCapabilityKinds = map[string]string{
	"supports_search_tool": "bool", "use_responses_lite": "bool", "prefer_websockets": "bool",
	"supports_parallel_tool_calls": "bool", "support_verbosity": "bool",
	"experimental_supported_tools": "string_list",
	"apply_patch_tool_type":        "string", "comp_hash": "string", "tool_mode": "string",
	"default_reasoning_level": "string", "default_verbosity": "string",
	"context_window": "number", "max_context_window": "number", "max_output_tokens": "number",
	"input_modalities": "modalities", "supported_reasoning_levels": "list", "service_tiers": "list",
}

func parseCodexModelCapabilities(body []byte) map[string]map[string]json.RawMessage {
	if len(body) > 8<<20 || !json.Valid(body) {
		return nil
	}
	items := gjson.GetBytes(body, "models")
	if !items.IsArray() || len(items.Array()) > 512 {
		return nil
	}
	result := make(map[string]map[string]json.RawMessage)
	items.ForEach(func(_, item gjson.Result) bool {
		slug := strings.ToLower(strings.TrimSpace(item.Get("slug").String()))
		if !isAllowedUpstreamCodexModel(slug) {
			return true
		}
		if _, duplicate := result[slug]; duplicate {
			return true
		}
		fields := make(map[string]json.RawMessage)
		for name, kind := range codexCapabilityKinds {
			value := item.Get(name)
			if !value.Exists() && value.Raw != "null" {
				continue
			}
			raw := json.RawMessage(value.Raw)
			if validCodexCapability(kind, raw) {
				fields[name] = append(json.RawMessage(nil), raw...)
			}
		}
		result[slug] = fields
		return true
	})
	return result
}

func validCodexCapability(kind string, raw json.RawMessage) bool {
	if len(raw) == 0 || len(raw) > 16<<10 {
		return false
	}
	if bytes.Equal(raw, []byte("null")) {
		return true
	}
	switch kind {
	case "bool":
		return bytes.Equal(raw, []byte("true")) || bytes.Equal(raw, []byte("false"))
	case "string":
		var value string
		return json.Unmarshal(raw, &value) == nil && len(value) <= 512
	case "number":
		var value int64
		return json.Unmarshal(raw, &value) == nil && value > 0 && value <= 100_000_000
	case "list", "string_list", "modalities":
		var values []json.RawMessage
		if json.Unmarshal(raw, &values) != nil || len(values) > 32 {
			return false
		}
		for _, value := range values {
			if kind == "string_list" || kind == "modalities" {
				var text string
				if json.Unmarshal(value, &text) != nil || !validCapabilityListString(text) {
					return false
				}
				if kind == "modalities" && text != "text" && text != "image" && text != "audio" {
					return false
				}
			}
			if capabilityListKey(value) == "" {
				return false
			}
		}
		return true
	}
	return false
}

func validCapabilityListString(value string) bool {
	if strings.TrimSpace(value) == "" || len(value) > 512 {
		return false
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return false
		}
	}
	return true
}

func capabilityListKey(raw json.RawMessage) string {
	var value string
	if json.Unmarshal(raw, &value) == nil {
		if !validCapabilityListString(value) {
			return ""
		}
		return value
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil || len(object) > 3 {
		return ""
	}
	for name, value := range object {
		if name != "id" && name != "effort" && name != "name" && name != "description" {
			return ""
		}
		var text string
		if json.Unmarshal(value, &text) != nil || len(text) > 512 {
			return ""
		}
	}
	for _, key := range []string{"effort", "id"} {
		if json.Unmarshal(object[key], &value) == nil && value != "" {
			return value
		}
	}
	return ""
}

type capabilityLearnEntry struct {
	hash    [32]byte
	expires time.Time
}

var capabilityLearnCache = struct {
	sync.Mutex
	entries map[string]capabilityLearnEntry
}{entries: make(map[string]capabilityLearnEntry)}

func (h *Handler) learnModelCapabilitiesAsync(account *auth.Account, body []byte) {
	if account == nil {
		return
	}
	h.learnModelCapabilitiesAsyncAtGeneration(account, account.GetCredentialGeneration(), body)
}

func (h *Handler) learnModelCapabilitiesAsyncAtGeneration(account *auth.Account, generation int64, body []byte) {
	if h == nil || h.db == nil || account == nil || account.IsRelayStyle() {
		return
	}
	models := parseCodexModelCapabilities(body)
	if len(models) == 0 {
		return
	}
	observed := time.Now()
	snapshot := database.ModelCapabilitySnapshot{AccountID: account.ID(), CredentialGeneration: generation, ObservedAt: observed.UnixNano(), Models: models}
	key := fmt.Sprintf("%p:%d:%d", h.db, snapshot.AccountID, snapshot.CredentialGeneration)
	hash := sha256.Sum256(body)
	capabilityLearnCache.Lock()
	previous := capabilityLearnCache.entries[key]
	if previous.hash == hash && observed.Before(previous.expires) {
		capabilityLearnCache.Unlock()
		return
	}
	if len(capabilityLearnCache.entries) >= 1024 {
		clear(capabilityLearnCache.entries)
	}
	capabilityLearnCache.entries[key] = capabilityLearnEntry{hash: hash, expires: observed.Add(time.Minute)}
	capabilityLearnCache.Unlock()
	forget := func() {
		capabilityLearnCache.Lock()
		if capabilityLearnCache.entries[key].hash == hash {
			delete(capabilityLearnCache.entries, key)
		}
		capabilityLearnCache.Unlock()
	}
	if !h.db.RunBackgroundTask(func(parent context.Context) {
		ctx, cancel := context.WithTimeout(parent, 5*time.Second)
		defer cancel()
		if err := saveModelCapabilitySnapshot(ctx, h.db, account, snapshot); err != nil {
			forget()
			log.Printf("save model capabilities: %v", err)
		}
	}) {
		forget()
	}
}

// SaveCapabilities persists this manifest for the account and credential
// generation observed when the upstream request started. Explicit account
// synchronization uses this synchronous path so success includes persistence.
func (manifest *CodexModelsManifest) SaveCapabilities(ctx context.Context, db *database.DB) error {
	if manifest == nil || manifest.capabilityAccount == nil || db == nil {
		return fmt.Errorf("missing model capability observation")
	}
	snapshot := manifest.capabilitySnapshot
	snapshot.Models = parseCodexModelCapabilities(manifest.Body)
	if len(snapshot.Models) == 0 {
		return fmt.Errorf("manifest contains no Codex model capabilities")
	}
	return saveModelCapabilitySnapshot(ctx, db, manifest.capabilityAccount, snapshot)
}

func saveModelCapabilitySnapshot(ctx context.Context, db *database.DB, account *auth.Account, snapshot database.ModelCapabilitySnapshot) error {
	if snapshot.CredentialGeneration != account.GetCredentialGeneration() {
		return fmt.Errorf("account credential generation changed")
	}
	if err := db.SaveModelCapabilities(ctx, snapshot); err != nil {
		return err
	}
	stored, err := db.ListModelCapabilities(ctx, []int64{snapshot.AccountID})
	if err != nil {
		return err
	}
	if current, ok := stored[snapshot.AccountID]; ok {
		if current.CredentialGeneration != snapshot.CredentialGeneration {
			return fmt.Errorf("model capability snapshot generation changed")
		}
		account.ApplyModelCapabilities(current)
	} else {
		return fmt.Errorf("model capability snapshot was not saved for current credentials")
	}
	return nil
}

// Missing declarations count as unknown. Booleans require every serving
// account to agree; limits take the minimum and supported lists intersect.
func intersectCodexCapabilities(candidates []map[string]json.RawMessage) map[string]json.RawMessage {
	result := make(map[string]json.RawMessage)
	if len(candidates) == 0 {
		return result
	}
	for name, kind := range codexCapabilityKinds {
		values := make([]json.RawMessage, len(candidates))
		declared := false
		complete := true
		for i, candidate := range candidates {
			values[i] = candidate[name]
			declared = declared || len(values[i]) > 0
			if len(values[i]) == 0 || bytes.Equal(values[i], []byte("null")) {
				complete = false
			}
		}
		if !declared {
			continue
		}
		if !complete {
			if kind == "bool" {
				result[name] = json.RawMessage("false")
			} else {
				result[name] = json.RawMessage("null")
			}
			continue
		}
		switch kind {
		case "bool":
			all := true
			for _, value := range values {
				all = all && bytes.Equal(value, []byte("true"))
			}
			if all {
				result[name] = json.RawMessage("true")
			} else {
				result[name] = json.RawMessage("false")
			}
		case "number":
			var minimum int64
			for _, raw := range values {
				var value int64
				_ = json.Unmarshal(raw, &value)
				if minimum == 0 || value < minimum {
					minimum = value
				}
			}
			result[name], _ = json.Marshal(minimum)
		case "list", "string_list", "modalities":
			shared := make([]json.RawMessage, 0)
			_ = json.Unmarshal(values[0], &shared)
			for _, raw := range values[1:] {
				var other []json.RawMessage
				_ = json.Unmarshal(raw, &other)
				keys := make(map[string]bool)
				for _, item := range other {
					keys[capabilityListKey(item)] = true
				}
				next := make([]json.RawMessage, 0, len(shared))
				for _, item := range shared {
					if keys[capabilityListKey(item)] {
						next = append(next, item)
					}
				}
				shared = next
			}
			result[name], _ = json.Marshal(shared)
		case "string":
			same := true
			for _, value := range values[1:] {
				same = same && bytes.Equal(values[0], value)
			}
			if same {
				result[name] = values[0]
			} else {
				result[name] = json.RawMessage("null")
			}
		}
	}
	return result
}

func (h *Handler) applyStoredModelCapabilities(ctx context.Context, row *database.APIKeyRow, body []byte) []byte {
	return h.applyModelCapabilities(ctx, row, body, false)
}

// A live upstream descriptor is also an upper bound: an older persisted
// observation must never add tools or limits that this descriptor removed.
func (h *Handler) applyUpstreamModelCapabilities(ctx context.Context, row *database.APIKeyRow, body []byte) []byte {
	return h.applyModelCapabilities(ctx, row, body, true)
}

func (h *Handler) applyModelCapabilities(ctx context.Context, row *database.APIKeyRow, body []byte, upstream bool) []byte {
	var root map[string]json.RawMessage
	var models []map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil || json.Unmarshal(root["models"], &models) != nil {
		return body
	}
	var accounts []*auth.Account
	var ids []int64
	if h != nil && h.store != nil && row != nil {
		for _, account := range h.store.Accounts() {
			if h.accountVisibleToAPIKey(account, row.ID, time.Now()) {
				accounts = append(accounts, account)
				ids = append(ids, account.ID())
			}
		}
	}
	snapshots := make(map[int64]database.ModelCapabilitySnapshot)
	// Without a readable store an observed narrower account cannot be told
	// apart from an unobserved one, so every account stays a blocker.
	snapshotsLoaded := false
	if h != nil && h.db != nil {
		stored, err := h.db.ListModelCapabilities(ctx, ids)
		if err == nil {
			snapshots = stored
			snapshotsLoaded = true
		}
	}
	for _, account := range accounts {
		if snapshot, ok := snapshots[account.ID()]; ok {
			account.ApplyModelCapabilities(snapshot)
		}
	}
	for _, model := range models {
		var slug string
		if json.Unmarshal(model["slug"], &slug) != nil || !isAllowedUpstreamCodexModel(slug) {
			// Non-Codex adapters build their own conservative descriptors; their
			// provider-specific reasoning controls cannot be learned here.
			continue
		}
		filter := codexManifestCapabilityAccountFilter(row, slug)
		var live map[string]json.RawMessage
		if upstream {
			live = make(map[string]json.RawMessage)
			for name, kind := range codexCapabilityKinds {
				if raw := model[name]; validCodexCapability(kind, raw) {
					live[name] = raw
				}
			}
		}
		var candidates []map[string]json.RawMessage
		eligible := 0
		websocketEligible := accountFilterForResponsesWebSocket(slug)
		allWebsocketEligible := true
		for _, account := range accounts {
			if !filter(account) {
				continue
			}
			eligible++
			if !websocketEligible(account) {
				allWebsocketEligible = false
			}
			target := slug
			if mapped, ok := resolveAccountModelMapping(account, slug); ok {
				target = mapped
			}
			snapshot, observed := snapshots[account.ID()]
			observed = observed && snapshot.CredentialGeneration == account.GetCredentialGeneration()
			var fields map[string]json.RawMessage
			if observed {
				fields, observed = snapshot.Models[strings.ToLower(strings.TrimSpace(target))]
			}
			// Against a live descriptor, an account without a current observation
			// of this model (relay-style upstreams never learn; new or rotated
			// credentials have not been seen yet) is unknown rather than a
			// downgrade. Request-side gates still handle such accounts per call.
			if upstream && snapshotsLoaded && !observed {
				continue
			}
			validated := make(map[string]json.RawMessage)
			for name, raw := range fields {
				if kind, ok := codexCapabilityKinds[name]; ok && validCodexCapability(kind, raw) {
					validated[name] = raw
				}
			}
			if upstream && snapshotsLoaded {
				// Fields an older observation did not declare cannot narrow, and
				// the live descriptor stays the upper bound for those it did.
				for name, raw := range live {
					if _, declared := validated[name]; !declared {
						validated[name] = raw
					}
				}
			}
			candidates = append(candidates, validated)
		}
		if upstream {
			// No eligible credential means there is no usable capability proof.
			if eligible == 0 {
				candidates = append(candidates, nil)
			}
			candidates = append(candidates, live)
		}
		effective := intersectCodexCapabilities(candidates)
		// Unknown observations cannot widen the transports actually available
		// to this key. HTTP-only relays never learn a capability snapshot.
		if !allWebsocketEligible {
			effective["prefer_websockets"] = json.RawMessage("false")
		}
		for name, kind := range codexCapabilityKinds {
			value := effective[name]
			if len(value) == 0 || bytes.Equal(value, []byte("null")) {
				if name == "input_modalities" {
					model[name] = json.RawMessage(`["text"]`)
				} else if kind == "bool" {
					model[name] = json.RawMessage("false")
				} else if kind == "list" || kind == "string_list" || kind == "modalities" {
					model[name] = json.RawMessage("[]")
				} else {
					delete(model, name)
				}
			} else {
				model[name] = value
			}
		}
	}
	encodedModels, err := json.Marshal(models)
	if err != nil {
		return body
	}
	root["models"] = encodedModels
	result, err := json.Marshal(root)
	if err != nil {
		return body
	}
	return result
}

func codexManifestCapabilityAccountFilter(row *database.APIKeyRow, model string) auth.AccountFilter {
	filter := excludeClaudeAccountsFilter(accountFilterForResponsesModel(model, true))
	if row == nil {
		return filter
	}
	return func(account *auth.Account) bool {
		if !filter(account) {
			return false
		}
		switch row.Limits.ResolveUpstreamChannel() {
		case database.UpstreamChannelCodex:
			return !account.IsGrokAPI() && !account.IsAntigravityAPI() && !account.IsClaudeOAuth()
		case database.UpstreamChannelGrok:
			return grokChannelAccountFilter(model)(account)
		case database.UpstreamChannelAntigravity:
			return antigravityChannelAccountFilter(model)(account)
		case database.UpstreamChannelClaude:
			return false // This provider serves native Messages, not Responses.
		default:
			return true
		}
	}
}

func restoreModelCapabilities(store *auth.Store, db *database.DB) {
	if store == nil || db == nil {
		return
	}
	accounts := store.Accounts()
	ids := make([]int64, 0, len(accounts))
	for _, account := range accounts {
		if account != nil && !account.IsRelayStyle() {
			ids = append(ids, account.ID())
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	snapshots, err := db.ListModelCapabilities(ctx, ids)
	if err != nil {
		log.Printf("restore model capabilities: %v", err)
		return
	}
	for _, account := range accounts {
		if account != nil {
			if snapshot, ok := snapshots[account.ID()]; ok {
				account.ApplyModelCapabilities(snapshot)
			}
		}
	}
}

func gateResponsesLiteForAccount(requested bool, body []byte, account *auth.Account) bool {
	if !requested {
		return false
	}
	if supported, known := account.ModelSupportsResponsesLite(gjson.GetBytes(body, "model").String()); known {
		return supported
	}
	return gateResponsesLiteForModel(requested, body)
}
