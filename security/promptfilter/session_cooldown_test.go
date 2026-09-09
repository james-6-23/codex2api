package promptfilter

import (
	"encoding/json"
	"testing"
)

func TestSessionCreationCooldownDefaultsBoundariesAndCustomTiers(test *testing.T) {
	config := DefaultSessionCreationCooldownConfig()
	if config.Validate() != nil || config.FreeCreations != 2 || config.FrequencyWindowSeconds != 1800 || config.Mode != "off" {
		test.Fatalf("unexpected defaults: %+v", config)
	}
	config.Mode = "enforce"
	for _, sample := range []struct {
		average  float64
		interval int
	}{{0, 900}, {299.9, 900}, {300, 600}, {599.9, 600}, {600, 300}, {899.9, 300}, {900, 0}, {1200, 0}} {
		if actual := config.Interval(sample.average, 10); actual != sample.interval {
			test.Errorf("average=%v got %d want %d", sample.average, actual, sample.interval)
		}
	}
	if config.Interval(0, 9) != 0 {
		test.Fatal("insufficient samples must bypass")
	}
	config.Tiers = []SessionCreationCooldownTier{{0, 500}, {100, 50}}
	config.MaxIntervalSeconds = 200
	if config.Interval(50, 20) != 200 || config.Interval(100, 20) != 50 {
		test.Fatal("custom order, inclusive boundary or cap ignored")
	}
}

func TestSessionCreationCooldownDocumentValidation(test *testing.T) {
	for _, raw := range []string{`{}`, `{"risk":{"enabled":true}}`, `{"risk":{"session_creation_cooldown":{"mode":"observe","free_creations":3}}}`} {
		document, err := ParseAdvancedConfigDocument(raw)
		if err != nil {
			test.Fatal(err)
		}
		if _, err := ParseAdvancedConfigDocument(document.Raw); err != nil {
			test.Fatal(err)
		}
	}
	for _, patch := range []map[string]any{
		{"mode": "bad"}, {"frequency_window_seconds": 0}, {"free_creations": 0}, {"history_days": 91},
		{"min_samples": 21}, {"max_samples": 201}, {"max_interval_seconds": -1}, {"tiers": nil},
		{"tiers": []SessionCreationCooldownTier{{1, 1}}}, {"tiers": []SessionCreationCooldownTier{{0, 1}, {0, 2}}},
	} {
		raw, _ := json.Marshal(map[string]any{"risk": map[string]any{"session_creation_cooldown": patch}})
		if _, err := ParseAdvancedConfigDocument(string(raw)); err == nil {
			test.Fatalf("accepted invalid config: %s", raw)
		}
	}
}

func TestSessionCreationCooldownDocumentPreservesOtherSettings(test *testing.T) {
	document, err := MergeAdvancedConfigDocument(`{"risk":{"session_creation_limit":8,"session_creation_cooldown":{"future":true}},"future_root":{"value":1}}`, `{"risk":{"session_creation_cooldown":{"mode":"enforce","frequency_window_seconds":1200,"free_creations":3,"history_days":14,"min_samples":5,"max_samples":15,"max_interval_seconds":1200,"tiers":[{"min_average_seconds":0,"interval_seconds":1200},{"min_average_seconds":1200,"interval_seconds":0}]}}}`)
	if err != nil {
		test.Fatal(err)
	}
	if document.Effective.Risk.SessionCreationLimit != 8 || document.Effective.Risk.SessionCreationCooldown.FreeCreations != 3 || document.Effective.Risk.SessionCreationCooldown.Interval(1199, 5) != 1200 {
		test.Fatalf("round-trip changed config: %+v", document.Effective.Risk)
	}
	var root map[string]any
	if err := json.Unmarshal([]byte(document.Raw), &root); err != nil {
		test.Fatal(err)
	}
	if root["future_root"] == nil || root["risk"].(map[string]any)["session_creation_cooldown"].(map[string]any)["future"] != true {
		test.Fatal("unknown settings dropped")
	}
}
