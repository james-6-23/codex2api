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
	"apply_patch_tool_type": "string", "comp_hash": "string", "tool_mode": "string",
	"default_reasoning_level": "string", "default_verbosity": "string",
	"context_window": "number", "max_context_window": "number", "max_output_tokens": "number",
	"input_modalities": "list", "supported_reasoning_levels": "list", "service_tiers": "list",
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
	case "list":
		var values []json.RawMessage
		if json.Unmarshal(raw, &values) != nil || len(values) > 32 {
			return false
		}
		for _, value := range values {
			if capabilityListKey(value) == "" {
				return false
			}
		}
		return true
	}
	return false
}

func capabilityListKey(raw json.RawMessage) string {
	var value string
	if json.Unmarshal(raw, &value) == nil {
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
	if h == nil || h.db == nil || account == nil || account.IsRelayStyle() {
		return
	}
	models := parseCodexModelCapabilities(body)
	if len(models) == 0 {
		return
	}
	observed := time.Now()
	snapshot := database.ModelCapabilitySnapshot{AccountID: account.ID(), CredentialGeneration: account.GetCredentialGeneration(), ObservedAt: observed.UnixNano(), Models: models}
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
		if err := h.db.SaveModelCapabilities(ctx, snapshot); err != nil {
			forget()
			log.Printf("save model capabilities: %v", err)
			return
		}
		if stored, err := h.db.ListModelCapabilities(ctx, []int64{snapshot.AccountID}); err == nil {
			if current, ok := stored[snapshot.AccountID]; ok {
				account.ApplyModelCapabilities(current)
			}
		}
	}) {
		forget()
	}
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
		case "list":
			var shared []json.RawMessage
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
	if h == nil || h.db == nil || h.store == nil || row == nil {
		return body
	}
	var accounts []*auth.Account
	var ids []int64
	for _, account := range h.store.Accounts() {
		if h.accountVisibleToAPIKey(account, row.ID, time.Now()) {
			accounts = append(accounts, account)
			ids = append(ids, account.ID())
		}
	}
	snapshots, err := h.db.ListModelCapabilities(ctx, ids)
	if err != nil || len(snapshots) == 0 {
		return body
	}
	for _, account := range accounts {
		if snapshot, ok := snapshots[account.ID()]; ok {
			account.ApplyModelCapabilities(snapshot)
		}
	}
	var root map[string]json.RawMessage
	var models []map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil || json.Unmarshal(root["models"], &models) != nil {
		return body
	}
	for _, model := range models {
		var slug string
		if json.Unmarshal(model["slug"], &slug) != nil {
			continue
		}
		filter := accountFilterForResponsesModel(slug, true)
		var candidates []map[string]json.RawMessage
		for _, account := range accounts {
			if !filter(account) {
				continue
			}
			target := slug
			if mapped, ok := resolveAccountModelMapping(account, slug); ok {
				target = mapped
			}
			snapshot := snapshots[account.ID()]
			var fields map[string]json.RawMessage
			if snapshot.CredentialGeneration == account.GetCredentialGeneration() {
				fields = snapshot.Models[strings.ToLower(target)]
			}
			candidates = append(candidates, fields)
		}
		for name, value := range intersectCodexCapabilities(candidates) {
			if bytes.Equal(value, []byte("null")) {
				delete(model, name)
			} else {
				model[name] = value
			}
		}
	}
	root["models"], err = json.Marshal(models)
	if err != nil {
		return body
	}
	result, err := json.Marshal(root)
	if err != nil {
		return body
	}
	return result
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
